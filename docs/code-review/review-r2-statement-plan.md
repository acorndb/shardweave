---
title: R2 Statement 与 Plan 骨架 review
files:
  - pkg/sql/statement.go
  - pkg/route/route.go
  - pkg/route/source/routers.go
  - pkg/route/source/pick.go
  - pkg/plan/plan.go
  - pkg/executor/executor.go
  - pkg/proxy/session.go
---

## Must Fix

无。

## Should Fix

无。

## Nit

### `plan.Builder` 的多目标 Reject 仍是占位实现
- status: wontfix
- file: pkg/plan/plan.go

`Build` 对 `len(targets) > 1` 返回硬编码 `1105` 的 `Reject`。R2 的路由器只产出一个目标，
该路径不可达；P3 会定义稳定的错误码，执行层会改为写 ERR 而不是返回错误。保留占位是为了
把三态契约先放进 Plan。

### `Statement.Kind` 目前只标 COM_QUERY / COM_INIT_DB
- status: wontfix
- file: pkg/proxy/session.go

其他命令的 `Kind` 仍是 `KindUnknown`。R4 会把 USE/SET/autocommit 语义移入 `pkg/sql`，
届时统一分类；R2 不引入未使用的分类逻辑。

### `executor` 仍保留 `Mode()` 与 sharding 桩
- status: wontfix
- file: pkg/executor/executor.go

R2 的目标是让 single/rwsplit 先走 Plan，暂不改执行器命名与生命周期；R3 会迁到
`pkg/execute` 并删除 sharding 桩。

### `pkg/route` 没有独立测试文件
- status: wontfix
- file: pkg/route/route.go

`Target`/`Router` 是纯契约；`source` 的实现测试与 `plan` 的集成测试覆盖了它，
并在 `routers.go` 增加了 `var _ route.Router = ...` 编译期断言。

## 验证

- `go build ./...`：通过
- `go test -race -short -count=1 ./pkg/sql/... ./pkg/route/... ./pkg/plan/ ./pkg/executor/ ./pkg/proxy/ ./pkg/server/ ./pkg/backend/`：全绿
- `go test -short -run '^$' -count=1 ./test/...`：编译通过（差分未跑，需要 Docker）
- `go vet ./...`：通过
- `make lint`：`go vet` 通过；`golangci-lint` 因本机工具链为 go1.23、目标为 go1.27.3
  无法加载配置（环境问题，非代码问题）
