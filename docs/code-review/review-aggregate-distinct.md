---
title: 聚合 DISTINCT 跨片归并 review
files:
  - pkg/plan/plan.go
  - pkg/sql/rewrite/rewrite.go
  - pkg/merge/aggregate.go
  - pkg/merge/group.go
  - pkg/merge/aggregate_distinct.go
  - test/metamorphic/aggregate_test.go
  - test/sharding/sharding_test.go
  - test/sharding/replica_test.go
---

本次实现跨片 `COUNT/SUM/AVG(DISTINCT expr)`：plan 把 DISTINCT 参数投影为隐藏列并
加入 per-shard GROUP BY；merge 按 group 维护规范化去重集合，再计算最终聚合。
覆盖无 GROUP BY、GROUP BY、与普通聚合混用、HAVING/ORDER BY/LIMIT、NULL 与
DECIMAL/UNSIGNED/DOUBLE。验证：plan/rewrite/merge/execute 聚焦 `-race -short`
全绿；4 Slice + single-DB oracle 约 47s；分片/复制 e2e 约 71s；
golangci-lint v2.15.0（Go 1.27.3）0 issues。

追加加固（2026-10-31）：随机查询按 8 种形态轮转，DISTINCT 形态使用 scatter-only
WHERE 确保进入归并路径；oracle 增加表达式/常量/日期/极值/多 DISTINCT 参数/
AVG+聚合 DISTINCT/空集与 NULL 集；e2e 增加 EXPLAIN ROUTE、位置排序、OFFSET、
LIMIT 0、ROLLUP 与聚合别名拒绝。最终 oracle 约 47.3s、sharding e2e 约 69.2s。

## Must Fix

### 跨片重复值不能靠局部聚合求和
- status: fixed
- file: pkg/plan/plan.go, pkg/merge/aggregate_distinct.go

`COUNT(DISTINCT x)` 在各片各算一次后直接相加会把跨片重复值算多次，
`SUM/AVG(DISTINCT x)` 同样会把重复值重复累加。修复：每片把 DISTINCT 参数作为
隐藏原始列返回（并加入 per-shard GROUP BY），归并层用规范化 key 去重后再
COUNT/SUM/AVG；NULL 不进入集合（COUNT 返回 0，SUM/AVG 返回 NULL）。

### per-shard LIMIT 会截断 DISTINCT 值
- status: fixed
- file: pkg/plan/plan.go, pkg/sql/rewrite/rewrite.go

无 GROUP BY 的聚合 DISTINCT 也会注入 per-shard GROUP BY；若保留原 LIMIT，
分片会在去重值集合完整返回前截断。修复：注入 GROUP BY 时统一
`RemoveLimit`，LIMIT/OFFSET 在全局归并后应用；单测覆盖 `LIMIT 1` 的分片 SQL
不含 LIMIT。

### AVG 隐藏列与 DISTINCT 隐藏列位置冲突
- status: fixed
- file: pkg/plan/plan.go, pkg/merge/aggregate.go, pkg/merge/group.go

AVG 会追加隐藏 SUM/COUNT，DISTINCT 又追加原始参数列；两类隐藏列顺序必须与
rewrite 的“先 ExtraAggregates、后 ExtraFields”一致。修复：先收集 DISTINCT
参数，扫描完所有 AVG 隐藏列后再以 `base+len(extras)` 分配 DISTINCT 列位置；
新增 AVG + COUNT(DISTINCT) 位置单测。

### GROUP BY WITH ROLLUP / 分组键引用聚合输出的静默错误
- status: fixed
- file: pkg/plan/plan.go

`WITH ROLLUP` 的汇总行会被当成普通 group 合并；`GROUP BY c` 若 `c` 是聚合别名，
归并层会把部分聚合值当分组键。两者都会产生静默错误结果。修复：多分片计划
显式 1235；新增 rollup 与聚合别名拒绝用例与 e2e 覆盖。

### rewrite 空 GROUP BY 别名错误路径未恢复 AST
- status: fixed
- file: pkg/sql/rewrite/rewrite.go

`ExtraGroupBy` 含空别名时，原实现已在修改 `sel.Limit/Having/Fields` 后才返回
错误，调用方 AST 会残留改写状态，影响后续 target 的 rewrite。修复：在修改
AST 前完成别名校验；新增错误路径的 AST 恢复测试。

## Should Fix

### SUM(DISTINCT) 输出 scale 取错元数据
- status: fixed
- file: pkg/merge/aggregate.go

非 DISTINCT SUM 原先用 `fields[spec.Column].Decimals` 格式化；DISTINCT 时
`spec.Column` 是隐藏原始参数列，scale 可能与 SUM 输出列不同。修复：统一使用
`fields[spec.Output].Decimals`，与 Group 及 AVG 输出保持一致。

### 聚合 DISTINCT + ORDER BY（无 GROUP BY）
- status: wontfix
- file: pkg/plan/plan.go

`SELECT COUNT(DISTINCT x) AS c ... ORDER BY c` 仍 1235。无 GROUP BY 的聚合只有
一行，排序无实际意义；若后续需要按聚合表达式排序，应先做别名/位置到输出列的
统一解析，再开放。

### DISTINCT 集合内存无上限
- status: wontfix
- file: pkg/merge/aggregate_distinct.go

`seen` map 随 DISTINCT 值数量增长，尚未设置内存预算/超限错误码；与现有
GROUP BY/DISTINCT/Sort 的限制一致。正式开放大结果集前应统一加配置上限。

### 字符串/二进制与多参数 DISTINCT
- status: wontfix
- file: pkg/merge/aggregate_distinct.go, pkg/plan/plan.go

字符串/二进制参数需要 collation 权重表，按既定方向暂不引入；`COUNT(DISTINCT
a, b)` 需要元组集合键，当前显式 1235。两者都不会返回静默错误结果。

## Nit

### MIN/MAX(DISTINCT) 按普通聚合处理
- status: wontfix
- file: pkg/plan/plan.go

`MIN/MAX(DISTINCT x)` 与 `MIN/MAX(x)` 语义等价，保留原表达式并按部分聚合
归并即可，无需去重集合；注释已说明。

### 隐藏列别名冲突
- status: wontfix
- file: pkg/plan/plan.go

`__sw_distinct_N`/`__sw_avg_*` 与用户显式别名同名时可能产生歧义；与既有 AVG
隐藏列风险一致，前缀已足够低概率。若后续需要彻底避免，可对用户别名做保留
前缀校验。

### aggregateFunc 默认值
- status: wontfix
- file: pkg/plan/plan.go

`aggregateFunc` 对未知名称返回 `AggCount`，依赖调用前 switch 校验；当前所有
调用点都先拒绝未知函数。注释已明确约束，避免静默错误。
