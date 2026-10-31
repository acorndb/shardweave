---
title: 未投影列 ORDER BY 隐藏列与投影 review
files:
  - pkg/plan/plan.go
  - pkg/sql/rewrite/rewrite.go
  - pkg/merge/merge.go
  - pkg/execute/dispatch_test.go
  - test/metamorphic/aggregate_test.go
---

本次让 `ORDER BY` 支持未投影的简单列：plan 追加隐藏排序列、`MergeSpec.Output`
投影回原始列；归并后再按 LIMIT 截断。验证边界：plan/rewrite/merge/execute
单测、完整 `make test-unit`、2 Slice e2e、single-DB oracle 全绿；sharding e2e
约 68s，oracle 约 31s；golangci-lint 0 issues。

## Must Fix

### 未投影排序列此前直接 1235
- status: fixed
- file: pkg/plan/plan.go, pkg/sql/rewrite/rewrite.go

`SELECT amount FROM orders ORDER BY user_id` 的排序键不在结果集中，归并层没有
比较值。修复：显式 select list 中找不到排序列时，把它作为隐藏列追加到
per-shard SQL，`OrderKey` 指向隐藏列下标，`MergeSpec.Output` 记录原始输出列；
`merge.project` 在 Limit 之前去掉隐藏列。

### `SELECT *` 不能推导输出投影
- status: fixed
- file: pkg/plan/plan.go

`SELECT *` 在 plan 阶段不知道列数，无法生成 `Output` 下标。处理：含 wildcard
时 ORDER BY 列仍按名字解析（不追加隐藏列），运行时由 `merge.Ordered` 在字段
里找；找不到则写列头前 1235。这样 `SELECT * ORDER BY 已存在列` 保持可用。

### 改写必须恢复 AST 字段
- status: fixed
- file: pkg/sql/rewrite/rewrite.go

追加隐藏列会临时替换 `sel.Fields`；已在 `Select` 中保存并 defer 恢复，
plan 测试断言原始 AST 字段数不变。

## Should Fix

### ORDER BY 表达式仍不支持
- status: wontfix
- file: pkg/plan/plan.go

`ORDER BY amount + 1` 需要把表达式本身作为隐藏列并做类型比较；当前只补简单
列，表达式显式 1235。属于 D5.1c 的剩余部分。

### 字符串/二进制排序
- status: wontfix
- file: pkg/merge/ordered.go

collation 相关类型仍 1235；需要字符集/排序规则元数据后再开放。

### 多列隐藏键的别名
- status: wontfix
- file: pkg/plan/plan.go

隐藏列使用 `__sw_order_N`，按 ORDER BY 出现顺序唯一；重复键会重复追加，
结果仍正确（比较相同值），只是多一列传输。

## Nit

### 输出投影的列元数据
- status: wontfix
- file: pkg/merge/merge.go

`Project` 直接复用上游 `result.Field`，保留原始列名/类型；隐藏列的元数据
不会写到前端。这是刻意的，客户端看到的列与原始 select list 一致。

### 投影与 Limit 顺序
- status: wontfix
- file: pkg/merge/merge.go

投影在 Limit 之前执行；行数不变，LIMIT 语义不受影响，同时避免把隐藏列
写到前端。
