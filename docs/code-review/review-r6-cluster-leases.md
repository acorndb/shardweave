---
title: R6 Cluster 与多 Slice lease review
files:
  - pkg/backend/cluster.go
  - pkg/backend/slice.go
  - pkg/route/source/routers.go
  - pkg/execute/pipeline.go
  - pkg/server/server.go
---

## Must Fix

无。

## Should Fix

无。

## Nit

### `NewCluster` 不校验 Slice 是否有 master
- status: wontfix
- file: pkg/backend/cluster.go

`Slice.Acquire` 会对 nil master 返回明确错误；`server` 装配时一定填 master。
若在 `NewCluster` 再校验，测试里构造"只有名字的 Slice"会更啰嗦；保留到 acquire
时统一报错。

### `Release` 释放全部 lease，而不是只释放当前目标
- status: wontfix
- file: pkg/execute/pipeline.go

这是有意选择：非 sticky 语句结束/会话关闭时不能留下任何 lease；多分片事务
（P3/P4）需要"commit 后释放全部"。如果未来需要单目标释放，可加 `ReleaseSlice`，
不改变现在的安全不变量。

### `Cluster.Close` 按 map 顺序关闭
- status: wontfix
- file: pkg/backend/cluster.go

关闭顺序没有语义；错误只保留第一个。若未来需要确定性日志，可在 Close 里按
`Names()` 排序。

## 验证

- `go build ./...`：通过
- `go vet ./...`：通过
- `go test -race -short -count=1 ./pkg/backend/ ./pkg/route/... ./pkg/execute/ ./pkg/proxy/ ./pkg/server/ ./pkg/plan/`：全绿
- `go test -short -run '^$' -count=1 ./test/...`：编译通过（差分未跑，Docker 不可用）
- 覆盖率：`pkg/backend` 73.1%、`pkg/execute` 89.0%、`pkg/route/source` 83.5%、`pkg/server` 64.1%
- 多 slice 用例：`TestPipeline_MultiSliceLeases`（两个 lease、Release 全部）
