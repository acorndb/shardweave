---
title: 池 acquire 策略（fail-fast / 可选等待）review
files:
  - pkg/backend/pool.go
  - pkg/config/config.go
  - pkg/server/server.go
  - pkg/proxy/session.go
---

## Must Fix

无。

## Should Fix

无。

## Nit

### `acquire_timeout` 只管池满等待，不含 dial
- status: wontfix
- file: pkg/backend/pool.go

当前语义：池满时，`0` 立即返回 `ErrPoolExhausted`，`>0` 最多等待该时长；
需要新建连接时走 `dial_timeout`。这与 DBCP 的 `maxWait`（只在池满时生效）
一致，而不是 Hikari 的 `connectionTimeout`（覆盖包括新建连接在内的整个 borrow）。
如果后续需要“整个 acquire 总预算”，再单独定义 `acquire_budget`，避免一个字段
承担两种语义。

### 等待路径仍是 5ms 轮询
- status: wontfix
- file: pkg/backend/pool.go

`AcquireTimeout > 0` 时仍用短轮询等待归还；正确性没问题，但可换成
`sync.Cond`/等待队列。默认 fail-fast 下不进入这段路径。

### 默认 fail-fast 会在突发流量下直接报错
- status: wontfix
- file: pkg/backend/pool.go

这是明确选择：池满说明 `max_capacity` 已打满，快速失败避免请求无限排队；
需要吸收短突发时显式设置 `acquire_timeout > 0`。

### 池满时前端连接会收到 1040 后关闭
- status: fixed
- file: pkg/proxy/session.go

`execute.ErrPoolExhausted` 映射为 MySQL 1040（Too many connections），写回前端后
关闭连接；不会让客户端只看到 EOF。连接级准入控制（`server.max_connections`）
是下一项独立改动。

## 横向对照（调研结论）

| 实现 | 获取连接等待策略 |
|---|---|
| Go `database/sql` | 等 `connRequests` 队列，由调用方 `ctx` 取消；无独立 acquire timeout |
| Vitess / dbpack `ResourcePool` | `Get(ctx)` 等资源或 ctx；注释明确 0 = indefinite |
| Gaea | `Get(ctx)` 外套固定 `GetConnTimeout = 2s`；connect/handshake/read/write 另有超时 |
| kingshard | channel pop，无显式 acquire timeout；空闲连接按 ping 周期校验 |
| ProxySQL | 无 acquire timeout；`mysql-wait_timeout` 是前端空闲超时，`connect_timeout_server` 是 dial |
| HikariCP | `connectionTimeout` 默认 30s，覆盖包括新建连接在内的整个 borrow |
| DBCP2 | `maxWaitMillis` 默认 -1（无限等待），只在池满时生效 |
| Tomcat JDBC | `maxWait`，可配置等待 |
| SQLAlchemy | `pool_timeout` 默认 30s，仅池满时 |
| Druid | `maxWait` 默认 60s |

结论：acquire timeout 是标准概念，但 Go 生态更常用调用方 ctx，Java 池多为
可配置等待；“池满直接报错”同样是合法策略。prism 选择默认 fail-fast，
保留 `>0` 的显式等待，不再隐式等待 2s。

## 验证

- `go build ./...`：通过
- `go vet ./...`：通过
- `go test -race -short -count=1 ./pkg/backend/ ./pkg/config/ ./pkg/server/ ./pkg/execute/`：全绿
- `go test -short -run '^$' -count=1 ./test/...`：编译通过（差分未跑，Docker 不可用）
- 新增用例：`TestPool_MaxCapacity`（fail-fast）、`TestPool_AcquireTimeoutWaits`、
  `TestPool_AcquireTimeoutExpires`
- 覆盖率：`pkg/backend` 72.1%、`pkg/config` 94.0%
