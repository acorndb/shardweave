---
title: GROUP BY/DISTINCT 排序与 LIMIT review
files:
  - pkg/plan/plan.go
  - pkg/sql/rewrite/rewrite.go
  - pkg/merge/sort.go
  - pkg/merge/merge.go
  - pkg/execute/dispatch_test.go
  - test/metamorphic/aggregate_test.go
---

本次在 GROUP BY/DISTINCT 后支持按已选列/别名/位置 ORDER BY，并在全局归并后
应用 LIMIT/OFFSET；per-shard LIMIT 被移除。验证边界：plan/rewrite/merge/
execute 单测、完整 `make test-unit`、2 Slice e2e、single-DB oracle 全绿；
sharding e2e 约 71s，oracle 约 26s；golangci-lint 0 issues。

## Must Fix

### per-shard LIMIT 会提前截断部分分组
- status: fixed
- file: pkg/plan/plan.go, pkg/sql/rewrite/rewrite.go

GROUP BY/DISTINCT 的同一个 group/distinct 值可能跨多个分片；若每片保留原
LIMIT，会丢掉本应参与全局结果的组。修复：`rewrite.Spec.RemoveLimit` 从
per-shard SQL 移除 LIMIT，归并后由 `merge.Limit` 应用全局 OFFSET/COUNT。

### 分组/去重结果不能直接 k 路归并
- status: fixed
- file: pkg/merge/sort.go

同一个 group 在不同分片的部分聚合需要先合并，输出行也不是按分片有序；
k 路堆无法直接使用。修复：`merge.Sort` 在 Group/Distinct 之后缓冲输出行，
用输出列 comparator 排序，再应用 Limit。排序 key 支持位置/别名/已选列。

### ORDER BY 必须能解析到输出列
- status: fixed
- file: pkg/plan/plan.go

`ORDER BY COUNT(*)` 没有可解析的输出列名/别名；当前 1235，要求使用别名
（`AS c`）或位置。`DISTINCT` 的 ORDER BY 列必须在 select list 中（MySQL 也
有这一限制）。字符串/二进制排序列仍 1235（collation 未引入）。

### LIMIT 的 OFFSET 语义
- status: fixed
- file: pkg/plan/plan.go, pkg/merge/merge.go

全局 Limit 在排序后执行；OFFSET/COUNT 直接作用于去重/分组后的行，不再做
per-shard `m+n` 改写，避免部分分组被截断。

## Should Fix

### 排序/去重缓冲无内存上限
- status: wontfix
- file: pkg/merge/sort.go, pkg/merge/distinct.go

Sort 缓冲全部分组/去重行，Distinct/Group 的 map 也无上限；当前 P0 数据量可
接受。正式开放大结果集前应加内存预算与稳定错误码。

### 聚合表达式排序
- status: wontfix
- file: pkg/plan/plan.go

`ORDER BY SUM(amount)` 需要把聚合表达式映射到输出列；当前要求别名/位置。
后续可做 AST 表达式等价匹配。

### HAVING
- status: wontfix
- file: pkg/plan/plan.go

HAVING 需要在分组后过滤；当前 router 显式 1235，属于后续工作。

## Nit

### per-shard ORDER BY 保留
- status: wontfix
- file: pkg/plan/plan.go

per-shard SQL 仍保留 ORDER BY（只移除 LIMIT）；最终顺序由全局 Sort 决定，
分片排序会多一次排序成本。后续可在 rewrite 中去掉 order 以省后端开销，但
需要确认隐藏列/别名兼容。

### 排序稳定性
- status: wontfix
- file: pkg/merge/sort.go

使用 `sort.SliceStable`，相等 key 保持 Group/Distinct 首次出现顺序；MySQL
未保证 tie 顺序，结果属于合法集合。
