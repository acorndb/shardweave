---
title: GROUP BY HAVING 下推与全局过滤 review
files:
  - pkg/plan/plan.go
  - pkg/sql/rewrite/rewrite.go
  - pkg/merge/filter.go
  - pkg/merge/merge.go
  - test/metamorphic/aggregate_test.go
---

本次实现 GROUP BY 的数值 HAVING：plan 解析已选分组列/聚合别名/匹配聚合
表达式与字面量的 AND 比较，per-shard SQL 移除 HAVING，归并 Group 后再过滤。
验证边界：plan/rewrite/merge 单测、完整 `make test-unit`、4 Slice e2e、
single-DB oracle 全绿；sharding e2e 约 76s，oracle 约 49s；
golangci-lint 0 issues。

## Must Fix

### per-shard HAVING 会提前过滤部分聚合
- status: fixed
- file: pkg/plan/plan.go, pkg/sql/rewrite/rewrite.go

`HAVING COUNT(*) > 1` 在每个分片只看到局部计数；某 group 跨片合计 >1、但每片
都 ≤1 时会被错误丢弃（测试数据 amount=50 正是这种情况）。修复：
`rewrite.Spec.RemoveHaving` 从 per-shard SQL 移除 HAVING，归并 Group 后用
`merge.Filter` 做全局过滤。

### 聚合表达式/别名的 HAVING 列解析
- status: fixed
- file: pkg/plan/plan.go

HAVING 可以写别名（`HAVING c > 1`）或原聚合表达式（`HAVING COUNT(*) > 1`）。
plan 用 `selectFieldIndex` 解析别名/分组列，用聚合函数名 + 参数文本匹配已选
聚合；未选聚合/无法匹配显式 1235。

### HAVING 字面量类型
- status: fixed
- file: pkg/plan/plan.go, pkg/merge/filter.go

只接受整数/无符号/浮点数值字面量；字符串/Decimal/Binary 等显式 1235，避免
collation/精度语义不清。Filter 按输出列 kind 解析字面量，NULL 行不满足任何
比较（MySQL 语义）。

### HAVING OR
- status: fixed
- file: pkg/plan/plan.go

当前只支持 AND 组合；OR 显式 1235，避免引入未验证的布尔求值语义。

### 移除 HAVING 时列名保持
- status: fixed
- file: pkg/sql/rewrite/rewrite.go

AST restore 会把 `COUNT(*)` 变成 `COUNT(1)`，改变派生列名；oracle 因此失败。
修复：任何 rewrite 都对无别名表达式设置 `SelectField.Text()` 为显式别名，
保持客户端列名。

## Should Fix

### HAVING 范围有限
- status: wontfix
- file: pkg/plan/plan.go

不支持 OR、字符串/日期字面量、跨列比较、函数/表达式（如 `HAVING c*2 > 4`）。
这些需要完整表达式求值与 collation；当前显式 1235。

### HAVING 过滤无额外内存
- status: wontfix
- file: pkg/merge/filter.go

Filter 是流式逐行判断，不额外缓冲；Group 本身仍无内存上限（已有专项 review）。

### 聚合 DISTINCT
- status: wontfix
- file: pkg/plan/plan.go

`COUNT(DISTINCT ...)` 仍 1235；需要跨片去重集合/改写，未纳入本次。

## Nit

### 列名匹配依赖 AST 文本
- status: wontfix
- file: pkg/plan/plan.go

聚合表达式匹配用 `format.Restore` 的参数文本；语义相同但文本不同的写法
（如 `COUNT(1)` vs `COUNT(*)`）不会互相匹配，需使用相同表达式或别名。

### HAVING 过滤在 Sort 之前
- status: wontfix
- file: pkg/merge/merge.go

Group → Filter → Sort → Limit，符合 SQL 语义；过滤后排序成本更低。
