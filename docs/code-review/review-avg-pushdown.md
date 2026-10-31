---
title: 多分片 AVG 隐藏列改写 review
files:
  - pkg/plan/plan.go
  - pkg/sql/rewrite/rewrite.go
  - pkg/merge/aggregate.go
  - pkg/merge/aggregate_test.go
  - test/metamorphic/aggregate_test.go
---

本次实现无 GROUP BY 的 `AVG`：plan 将每个 AVG 改写为隐藏 `SUM/COUNT` 列，
merge 用两列计算最终平均值；同时修复 AST restore 导致的表达式列名变化。
验证边界：`pkg/plan`/`rewrite`/`merge`/`execute` 单测、完整
`make test-unit`、2 Slice 复制拓扑 e2e、single-DB aggregate oracle 全绿；
2 Slice 复制 e2e 约 70s，oracle 约 26s；golangci-lint 0 issues。

## Must Fix

### 不能直接合并每片 AVG
- status: fixed
- file: pkg/plan/plan.go, pkg/merge/aggregate.go

AVG 不是可结合的部分聚合；对各片 AVG 再取平均会忽略每片行数。修复：
`aggregateSpecs` 为每个 AVG 追加隐藏 `SUM(arg)`/`COUNT(arg)` 列，
`AggregateSpec` 增加 `CountColumn`；merge 以
`sum(hidden SUM) / sum(hidden COUNT)` 计算，按原 AVG 输出列的 DECIMAL scale
格式化。SUM/COUNT 为 NULL/0 时输出 NULL，单测与 oracle 覆盖。

### AST restore 改变表达式列名
- status: fixed
- file: pkg/sql/rewrite/rewrite.go

`SelectStmt.Restore` 会反引号标识符，`SELECT AVG(amount)` 的派生列名变成
`AVG(\`amount\`)`，与直连 MySQL 的 `AVG(amount)` 不一致，客户端按列名取值会
出问题。修复：改写时对无别名、非普通列引用的投影设置显式别名，值取 parser
保留的 `SelectField.Text()`，恢复原列名；oracle 断言列名后通过。

### 改写后必须恢复调用方 AST
- status: fixed
- file: pkg/sql/rewrite/rewrite.go

AVG 改写会临时替换 `sel.Fields`，若不恢复，后续 EXPLAIN/重试会看到隐藏列。
`Select` 保存并 defer 恢复 `Fields` 与 `Limit`；测试断言原始 AVG 查询字段数
仍为 1。

## Should Fix

### AVG 的类型范围
- status: wontfix
- file: pkg/merge/aggregate.go

AVG 复用 SUM 的类型域：整数/无符号/浮点/DECIMAL；字符串、日期等类型显式
1235。MySQL 对部分类型有隐式转换，但跨片结果精度/语义不安全，暂不开放。

### AVG + GROUP BY / DISTINCT
- status: wontfix
- file: pkg/plan/plan.go

GROUP BY 需要分组状态；`AVG(DISTINCT)` 需要跨片去重。当前均 1235，属于
D5.2d 的范围。

### 浮点 AVG 文本格式
- status: wontfix
- file: pkg/merge/aggregate.go

FLOAT/DOUBLE AVG 用 float64 除法和 `'g'` 格式化；与 MySQL 文本有效位可能
不同，客户端数值解析一致。需要逐字节对齐时再按 MySQL double 规则实现。

## Nit

### 隐藏列别名
- status: wontfix
- file: pkg/plan/plan.go

隐藏列使用 `__sw_avg_sum_N` / `__sw_avg_count_N`，只用于列定位，不暴露给
客户端；若用户 SELECT 恰好包含同名列，MySQL 会允许重复，但最终输出会按
原字段截断，不影响结果。

### big.Rat 分配
- status: wontfix
- file: pkg/merge/aggregate.go

AVG 的 SUM 用 big.Rat 累加、除法时再分配；无 GROUP BY 每片只有一行，开销
可接受。GROUP BY 落地时再考虑按 scale 的整数累加。
