---
title: R3 execution seam（result/execute/Conn.Exec）review
files:
  - pkg/backend/conn.go
  - pkg/backend/stream.go
  - pkg/result/result.go
  - pkg/execute/pipeline.go
  - pkg/execute/forward.go
  - pkg/proxy/session.go
  - pkg/server/server.go
---

## Must Fix

无。

## Should Fix

无。

## Nit

### `MySQLConn.Packet()` 仍导出
- status: wontfix
- file: pkg/backend/conn.go

`execute` 已不再使用 `Packet()`；R6 引入 `Cluster`/多 lease 时可把它降为包内实现。
R3 保留是为了让 `Align`/`Exec` 内部复用，行为不变。

### `rawStream` 的 `Fields/Next` 是占位实现
- status: wontfix
- file: pkg/backend/stream.go

R3 只有 N=1 raw 路径，`execute` 只调用 `ForwardTo`；逻辑行解码与 merge 在 P4。
`Next` 返回 `io.EOF` 是明确的“无逻辑行”语义，不会静默产出错误数据。

### `Pipeline` 未防御 `Acquire` 返回 `(nil, nil)`
- status: wontfix
- file: pkg/execute/pipeline.go

`backend.Pool.Acquire` 的 `OpenFunc` 契约是成功时返回非 nil `Conn`；现有实现与测试
都满足。若后续引入会返回 nil 的 OpenFunc，应在 `Pool` 层统一拒绝，而不是在
`execute` 里加分支。

### `result.Value` 使用 `any`
- status: wontfix
- file: pkg/result/result.go

P4 的 merge 需要类型化列值（int/string/bytes/time）；当前只有 raw 路径，`any`
足以表达契约。等 `merge` 落地时再收敛为带类型的 `Value`。

## 验证

- `go build ./...`：通过
- `go vet ./...`：通过
- `go test -race -short -count=1 ./pkg/execute/ ./pkg/backend/ ./pkg/result/ ./pkg/proxy/ ./pkg/server/ ./pkg/plan/ ./pkg/route/... ./pkg/sql/...`：全绿
- `go test -short -run '^$' -count=1 ./test/...`：编译通过（差分未跑，Docker 不可用）
- 覆盖率：`pkg/execute` 86.2%、`pkg/proxy` 81.5%、`pkg/backend` 66.8%
- `grep shardingExecutor|ErrShardingNotImplemented|MySQLBackend|\.Packet()`：
  `pkg/execute`/`pkg/proxy`/`pkg/server` 无结果
