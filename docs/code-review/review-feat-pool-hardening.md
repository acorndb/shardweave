---
title: 连接池加固（健康检查 / idle 回收 / 上限）review
files:
  - pkg/backend/pool.go
  - pkg/backend/conn.go
  - pkg/config/config.go
  - pkg/server/server.go
---

## Must Fix

无。

## Should Fix

无。

## Nit

### `created` map 以 `Conn` 接口为 key
- status: wontfix
- file: pkg/backend/pool.go

当前所有 `Conn` 实现都是指针（`*MySQLConn` / 测试 fake），可比较。若未来出现
非可比较的值类型实现，需要在 pool 层拒绝或改用内部 wrapper。

### warmup / reaper 失败没有日志或指标
- status: wontfix
- file: pkg/backend/pool.go

预热失败会静默停止，reaper 关闭连接也不计数。当前 `pkg/backend` 没有 logger/metrics
依赖；等可观测面（metrics/slog 注入）落地后补，不在池本身强行引入。

### Acquire 仍用 5ms 轮询等待
- status: wontfix
- file: pkg/backend/pool.go

高并发达到 `MaxCapacity` 时仍是短轮询；正确性没问题，但会浪费定时器。
后续可用 `sync.Cond`/等待队列替换，属于性能优化。

### 满 idle / 超 lifetime 时仍先执行 Reset
- status: wontfix
- file: pkg/backend/pool.go

`Release` 先 `COM_RESET_CONNECTION`，再判断 `MaxIdle`/`MaxLifetime` 并可能直接关闭。
多一次 round trip，但实现简单、语义安全；后续可前置判断。

### 仍无查询失败重试 / 从库换库
- status: wontfix
- file: pkg/backend/pool.go

本轮只加固池自身（健康检查、回收、上限、预热、Stats）。查询失败重试与
rwsplit 换库属于路由/执行层策略，单独工作流。

## 验证

- `go build ./...`：通过
- `go vet ./...`：通过
- `gofumpt -extra -l` 变更包：无输出
- `go test -race -short -count=1 ./pkg/backend/ ./pkg/config/ ./pkg/server/ ./pkg/execute/`：全绿
- `go test -short -run '^$' -count=1 ./test/...`：编译通过（差分未跑，Docker 不可用）
- 覆盖率：`pkg/backend` 74.2%
- 新增用例：`TestPool_Warmup`、`TestPool_HealthCheckDiscardsDeadIdle`、
  `TestPool_HealthCheckReusesLiveIdle`、`TestPool_IdleReap`、`TestPool_MaxIdle`、
  `TestPool_MaxLifetime`、`TestPool_Stats`、`TestMySQLConn_Ping`
