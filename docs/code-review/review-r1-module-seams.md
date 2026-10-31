---
title: R1 模块归属重构（backend / route/source）review
files:
  - pkg/backend/conn.go
  - pkg/backend/pool.go
  - pkg/backend/slice.go
  - pkg/constant/constant.go
  - pkg/executor/executor.go
  - pkg/executor/forward.go
  - pkg/server/server.go
  - pkg/config/config.go
---

## Must Fix

### conn_test.go 的 `require.Equal` 缺少 `testing.T`
- status: fixed
- file: pkg/backend/conn_test.go

迁移 `align_test.go` 时保留了旧的 `require.Equal([]byte{...}, data)`，缺少 `t` 参数，
该测试包无法编译。已补上 `t`，`pkg/backend` 测试恢复。

## Should Fix

无。

## Nit

### `execute` 仍对 `*backend.MySQLConn` 做类型断言
- status: wontfix
- file: pkg/executor/forward.go

`writeBackend` / `execCommand` 仍通过 `*backend.MySQLConn` 取 `Packet()`。
R1 只做包归属，保持行为不变；R6 会把 `Exec/Align` 收进 `backend.Conn` 并删除
这两个断言，refactor plan 已有 grep 门禁。

### `Slice.Acquire` 仍返回 `(Conn, *Pool, error)`
- status: wontfix
- file: pkg/backend/slice.go

目标形态是 `Cluster` + lease 抽象；R1 保持现有签名，避免把行为重构混进纯移动。
R6 引入 `Runtime.leases` 时再收敛。

### round-robin 测试断言含冗余条件
- status: wontfix
- file: pkg/backend/slice_test.go

`TestSlice_AcquireSlaveRoundRobin` 的断言里 `p1 != p2` 让前两个条件冗余。
这是迁移前的既有测试，R1 只移动不改断言；后续可单独收紧。

## 验证

- `go build ./...`：通过
- `go test -race -short -count=1 ./pkg/backend/ ./pkg/route/... ./pkg/executor/ ./pkg/server/ ./pkg/proxy/ ./pkg/config/ ./pkg/session/ ./pkg/constant/`：全绿
- `go test -short -run '^$' -count=1 ./test/...`：编译通过（差分未跑，需要 Docker）
- `go vet ./...`：通过
- `make lint`：`go vet` 通过；`golangci-lint` 因本机工具链为 go1.23、目标为 go1.27.3
  无法加载配置（环境问题，非代码问题）
- 旧包引用检查：`grep pkg/backend_server|pkg/router` 无结果
