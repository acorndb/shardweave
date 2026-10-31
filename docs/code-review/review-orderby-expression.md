---
title: ORDER BY 表达式隐藏列 review
files:
  - pkg/plan/plan.go
  - pkg/sql/rewrite/rewrite.go
  - pkg/merge/merge.go
  - test/metamorphic/aggregate_test.go
---

本次让多分片 `ORDER BY` 支持表达式（`amount + 1`、`-amount`、`amount % 100`
等）：plan 把表达式作为隐藏列追加到 per-shard SQL，按隐藏列类型比较，最后
用 `Output` 投影回原始列。验证边界：plan/merge/execute 单测、完整
`make test-unit`、2 Slice e2e、single-DB oracle 全绿；sharding e2e 约 71s，
oracle 约 28s；golangci-lint 0 issues。

## Must Fix

### 表达式排序此前直接 1235
- status: fixed
- file: pkg/plan/plan.go

`ORDER BY amount + 1` 没有可比较的结果列。修复：表达式追加为隐藏 select
字段（`__sw_order_N`），`OrderKey` 指向隐藏列，`Output` 记录原始输出列；
`merge.project` 在 Limit 前移除隐藏列。

### `SELECT *` 无法推导输出宽度
- status: fixed
- file: pkg/plan/plan.go

`SELECT *` 搭配表达式排序时无法知道原始列数，不能安全投影掉隐藏列。当前
显式 1235，要求使用明确 select list；普通列排序仍可按名字运行时解析。

### 隐藏表达式类型仍需校验
- status: fixed
- file: pkg/merge/ordered.go

比较器在写列头前根据隐藏列元数据选择 int/uint/float/decimal/日期文本；
字符串/二进制/JSON 类型仍 1235，避免字节序与 MySQL collation 不一致。

## Should Fix

### 字符串/二进制表达式排序
- status: wontfix
- file: pkg/merge/ordered.go

仍需 collation 元数据；暂 Reject，避免静默错误排序。

### 分组/聚合表达式
- status: wontfix
- file: pkg/plan/plan.go

聚合 + ORDER BY、GROUP BY 仍在 D5.2d 范围；本次仅处理非聚合表达式排序。

## Nit

### 隐藏表达式的别名
- status: wontfix
- file: pkg/plan/plan.go

`__sw_order_N` 按 ORDER BY 出现顺序编号；表达式文本不进入客户端列名，
`Output` 投影保证列元数据与原始 select list 一致。

### 常量表达式排序
- status: wontfix
- file: pkg/plan/plan.go

`ORDER BY 1+1` 会作为隐藏表达式执行，所有行比较值相等；MySQL 允许，结果为
任意稳定顺序（按 target/流顺序），不额外特殊处理。
