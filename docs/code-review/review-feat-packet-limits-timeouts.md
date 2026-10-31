---
title: packet 上限与前后端 I/O 超时 review
files:
  - pkg/mysql/packet/packet.go
  - pkg/mysql/server/conn.go
  - pkg/backend/conn.go
  - pkg/backend/pool.go
  - pkg/config/config.go
  - pkg/config/runtime.go
  - pkg/proxy/session.go
  - pkg/server/server.go
---

## Must Fix

无。

## Should Fix

无。

## Nit

### `max_allowed_packet` 是实例级配置，后端 MySQL 自身限制不会自动探测
- status: wontfix
- file: pkg/config/config.go

前端与 proxy→后端使用同一个 `server.max_allowed_packet`。如果后端 MySQL 的
`max_allowed_packet` 更小，客户端仍可能收到后端的 1153/断连。生产配置应保证
proxy 的值 <= 最严格后端的值；自动探测/按 endpoint 配置留到后续。

### 后端响应超限时没有前端 1153
- status: wontfix
- file: pkg/backend/stream.go

前端请求超限由 `proxy.Serve` 回 1153；后端响应超限发生在结果集转发中途，
`ForwardTo` 直接错误并关闭连接，避免半截结果集继续写。

### read/write timeout 是单次 I/O，不是查询总超时
- status: wontfix
- file: pkg/mysql/packet/packet.go

每次 frame 读/写都会重置 deadline。长时间不返回任何 packet 的慢查询会触发
read timeout；持续返回行的慢查询不会。查询总超时（`MAX_EXECUTION_TIME`）仍是
独立工作流。

### handshake 阶段超限不返回 1153
- status: wontfix
- file: pkg/mysql/server/conn.go

握手响应超过上限时直接关闭；此时还没有完成认证，返回业务错误意义不大。

### `DialMySQLWithOptions` 没有独立单测
- status: wontfix
- file: pkg/backend/conn.go

它需要真实 TCP + MySQL 握手；参数映射由 `TestToPoolConfig`、packet 层测试和
差分/冒烟覆盖。若要单测，可把 dial 与 handshake 再拆一层。

## 验证

- `go build ./...`：通过
- `go vet ./...`：通过
- `gofumpt -extra -l` 变更包：无输出
- `go test -race -short -count=1 ./pkg/mysql/... ./pkg/backend/ ./pkg/config/ ./pkg/proxy/ ./pkg/server/ ./pkg/execute/`：全绿
- `go test -short -run '^$' -count=1 ./test/...`：编译通过（差分未跑，Docker 不可用）
- 覆盖率：`pkg/mysql/packet` 75.5%、`pkg/backend` 71.9%、`pkg/config` 94.0%、`pkg/server` 64.6%
- 新增用例：packet 上限（读/写/边界/默认）、read/write timeout、Ping、config/server 映射
