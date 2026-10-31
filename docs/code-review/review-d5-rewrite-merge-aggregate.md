---
title: D5 rewrite 包、merge 工厂与无 GROUP BY 聚合 review
files:
  - pkg/sql/rewrite/rewrite.go
  - pkg/plan/plan.go
  - pkg/merge/merge.go
  - pkg/merge/aggregate.go
  - pkg/execute/dispatch.go
  - pkg/route/shard/router.go
  - test/sharding/sharding_test.go
---

本次增量先收敛两处架构漂移，再实现无 GROUP BY 的 `COUNT/SUM/MIN/MAX`
多分片归并。验证边界：`pkg/sql/rewrite`、`plan`、`merge`、`execute`、
`route/shard` 聚焦单测、完整 `make test-unit`、2 Slice 真拓扑 e2e 全绿；
golangci-lint 0 issues。未跑全量差分/randgen，按 AGENTS 需用户显式批准。

## Must Fix

### SQL 改写逻辑留在 plan 层
- status: fixed
- file: pkg/sql/rewrite/rewrite.go, pkg/plan/plan.go

LIMIT OFFSET 的 AST restore/SQL 文本生成原先直接写在 `plan`，与设计中的
`pkg/sql/rewrite` 职责冲突；后续 AVG/隐藏列继续堆会把 plan 变成改写器。
修复：新增 `pkg/sql/rewrite`，`rewrite.Select(sel, Spec)` 负责 per-shard SQL
并恢复调用方 AST；plan 只产出 RewriteSpec/MergeSpec。

### execute 自己选择 Merge 算子
- status: fixed
- file: pkg/merge/merge.go, pkg/execute/dispatch.go

`dispatch` 原先按 `OrderBy` 手工拼 `Ordered`/`Concat`/`Limit`，加聚合会继续
分支。修复：`merge.Validate` 做无流校验，`merge.Merge(streams, fields, spec)`
统一装配 Concat/Ordered/Aggregate/Limit；execute 只传 streams/fields/spec。
`Validate` 在 acquire 前调用，避免先拿连接再发现 shape 不支持。

### 聚合输出列与 per-shard 元数据宽度不一致
- status: fixed
- file: pkg/execute/dispatch.go, pkg/merge/aggregate.go

聚合（以及后续 AVG 隐藏列）可能让 per-shard 结果宽度大于最终输出宽度。
原先 dispatch 用 preflight 的 `fields` 写列头/校验行宽。修复：写列头与
`encodeCells` 使用 `merged.Fields()`；per-shard 元数据仍用于跨片一致性校验。
单测覆盖 COUNT+SUM 的输出列数/行值与 LIMIT 0。

### 不支持 shape 在获取连接后才报错
- status: fixed
- file: pkg/merge/merge.go, pkg/execute/dispatch.go

`merge.Merge` 校验需要 streams/fields，重构后放在 dispatch 末尾会让
unsupported merge 先通过 front/target 检查甚至 acquire。新增
`merge.Validate`，dispatch 在打开后端前拒绝 GROUP BY/DISTINCT/Output 等。

### 聚合表达式绕过聚合识别
- status: fixed
- file: pkg/plan/plan.go

`SELECT COUNT(*) + 1 FROM orders` 的投影是 BinaryOperationExpr，原实现只把
顶层 `*ast.AggregateFuncExpr` 当聚合，导致 specs 为空，plan 走普通 scatter，
每个分片返回一行 `COUNT+1`，客户端拿到多行而不是一行。修复：对每个投影做
AST walk，只要包含聚合就要求它是顶层聚合字段；`COUNT(*)+1`、标量子查询里的
聚合、`SUM(...)*2` 等统一 1235。新增表驱动用例。

### scatter 子查询可能读到不同分片的数据
- status: fixed
- file: pkg/route/shard/router.go

`SELECT * FROM orders WHERE user_id IN (SELECT ... FROM other_table)` 原先被
当作普通 scatter，子查询在每个分片本地执行；如果 `other_table` 不是全局复制
表，结果会依赖分片数据，属于静默错误。修复：多分片 SELECT 中出现任何
`SubqueryExpr` 时显式 1235，单分片键查询仍由 MySQL 处理；单测/e2e 覆盖。
后续做全局表/子查询下推时再放开。

## Should Fix

### AVG 暂不支持
- status: wontfix
- file: pkg/plan/plan.go, pkg/merge/aggregate.go

AVG = SUM/COUNT，需要改写每片 SELECT 注入隐藏列，并按 MySQL 的
DECIMAL scale 输出。当前显式 1235；下一步在 rewrite.Spec 增加隐藏聚合字段，
AggregateSpec 增加 CountColumn 后实现，避免先返回精度不一致的值。

### 聚合 DISTINCT、混合投影、聚合 + ORDER BY
- status: wontfix
- file: pkg/plan/plan.go

`COUNT(DISTINCT ...)` 需要跨片去重；`SELECT COUNT(*), amount` 需要 GROUP BY
语义；聚合 + ORDER BY 需要聚合结果排序/隐藏键。当前全部 1235，e2e 覆盖。

### SUM 类型范围
- status: wontfix
- file: pkg/merge/aggregate.go

COUNT 用 uint64、SUM 用 big.Rat/float64；极端超过 uint64 的 COUNT 或 MySQL
特殊数值类型（BIT/JSON）未覆盖。分片键/业务量级下可接受，后续按需扩展。

## Nit

### 浮点 SUM 的文本格式
- status: wontfix
- file: pkg/merge/aggregate.go

FLOAT/DOUBLE 用 `strconv.FormatFloat(...,'g',-1,64)`，与 MySQL 文本协议的
有效位/指数写法可能不同；客户端按数值解析结果一致。需要逐字节对齐时再按
MySQL 的 double 格式化规则实现。

### rewrite 丢注释/hint
- status: wontfix
- file: pkg/sql/rewrite/rewrite.go

`SelectStmt.Restore` 不保留 SQL 注释/优化器 hint；路由角色在改写前已决定，
查询超时 hint 尚未实现。后续如需保留，改为文本区间替换或自建 hint 渲染。
