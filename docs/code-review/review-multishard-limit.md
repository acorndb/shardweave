---
title: 多分片 LIMIT concat 截断 review
files:
  - pkg/merge/merge.go
  - pkg/sql/limit.go
  - pkg/plan/plan.go
  - pkg/route/shard/router.go
  - pkg/execute/dispatch.go
  - test/sharding/sharding_test.go
---

本次增量把 concat 归并抽到 `pkg/merge`（`Concat`/`Limit`），并放开
“无 ORDER BY、无 OFFSET”的多分片 `LIMIT n`：各分片原 SQL 已带同一 LIMIT，
concat 后由 `Limit` 截断到最终 n 行。验证边界：变更包聚焦单测、完整
`make test-unit`、2 Slice 真拓扑 e2e 均通过；golangci-lint 0 issues。未跑
全量差分/randgen，按 AGENTS 需用户显式批准。

## Must Fix

（无）

## Should Fix

### LIMIT 提前结束会留下未读 packet
- status: fixed
- file: pkg/execute/dispatch.go

`merge.Limit` 达到 n 行后不再读上游；此时分片 stream 里可能还有行和结束符。
若按普通 `Release`（COM_RESET_CONNECTION）还池，残留 packet 会被下一次请求
读到，造成协议错序。修复：只要计划带 LIMIT，成功路径也 `Discard` 所有 lease；
测试断言连接被关闭，e2e 在 LIMIT 之后继续执行 `SELECT 1` 验证会话可用。

### OFFSET 不能沿用同一 SQL 直接截断
- status: fixed
- file: pkg/plan/plan.go, pkg/route/shard/router.go

`LIMIT m, n` 在每个分片先丢掉各自的前 m 行；concat 后再全局跳过 m 行会少读，
结果行数不足。修复（D5.1b）：plan 把每个 target 的 SQL 改写为 `LIMIT m+n`
（AST restore，不保留原 OFFSET），归并后 `merge.Limit` 全局跳过 m 行；单测
解析改写后的 SQL，e2e 覆盖 `LIMIT 1 OFFSET 1` 与“offset 超出总行数”。

### concat 逻辑内联在 execute，无法复用
- status: fixed
- file: pkg/merge/merge.go

把逐片拼接与截断抽成不依赖 backend/execute 的 `result.Stream` 算子，
`pkg/merge` 只 import `result`；后续 ordered/aggregate 算子可在同一层实现。
单测覆盖顺序、空流、nil 流、错误传播、Close 幂等、Limit 不越读。

## Nit

### 带 LIMIT 时即使未截断也 Discard
- status: wontfix
- file: pkg/execute/dispatch.go

当前只要计划带 LIMIT 就丢弃 lease，即使所有分片行已读完（如 `LIMIT 100`
但总共 2 行）。要区分“limit 截断”与“上游恰好结束”需要额外探测/算子状态，
而丢弃只影响连接复用率，不影响正确性。D7 并发 dispatch/流式管线上线时再
引入 `Exhausted/Truncated` 信号优化。

### 无 ORDER BY 的 LIMIT 行选择
- status: wontfix
- file: pkg/merge/merge.go

`LIMIT n` 无 ORDER BY 时 MySQL 本身不保证返回哪些行；concat 按 slice 顺序取
前 n 行属于合法结果。需要稳定顺序的客户端必须使用 ORDER BY（D5.1 再做
ordered merge）。

### LIMIT 值上限
- status: wontfix
- file: pkg/sql/limit.go

超过 int64 的 LIMIT 字面量（如 `18446744073709551615`）由 `ParseLimit` 返回
ok=false，多分片路径 1235；单分片仍由后端处理。分片场景下这种值等价于
“无限制”，但精确支持需要 uint64 计数，当前收益低。
