---
title: R5 配置 backend + slices review
files:
  - pkg/config/config.go
  - pkg/config/runtime.go
  - pkg/server/server.go
  - pkg/constant/constant.go
---

## Must Fix

无。

## Should Fix

无。

## Nit

### `SameTopology` 依赖调用方先 `Validate` 做 LB 默认值归一化
- status: wontfix
- file: pkg/config/config.go

`Validate` 会把 `rwsplit` 的空 `load_balance` 归一成 `round_robin`；`Apply` 先
`Validate` 再比较，所以实际路径正确。直接在未归一化的结构上调用 `SameTopology`
可能把 `""` 与 `round_robin` 判为不同，但这不是公开使用路径。

### `sharding.rules` 结构已解析但模式仍拒绝
- status: wontfix
- file: pkg/config/config.go

这是 P3 的预埋结构，不是"写了不生效"的假字段：`mode=sharding` 由 `Validate`
明确报错，不会出现"配置接受但行为静默忽略"。P3 打开时会一起做规则引用校验。

### 用户重名未校验
- status: wontfix
- file: pkg/config/config.go

本次只做 backend 拓扑重构；用户重名是既有行为，后续鉴权/多租户切片再处理。

## 验证

- `go build ./...`：通过
- `go vet ./...`：通过
- `go test -race -short -count=1 ./pkg/config/ ./pkg/server/ ./pkg/execute/ ./pkg/proxy/ ./pkg/mysql/... ./pkg/sql/... ./cmd/shardweave/`：全绿
- `go test -short -run '^$' -count=1 ./test/...`：编译通过（差分未跑，Docker 不可用）
- 覆盖率：`pkg/config` 93.8%、`pkg/server` 64.0%
- `grep BackendServer|SameIdentity|\.Primary()`：Go 代码无结果
