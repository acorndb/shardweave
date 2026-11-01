# Code Review 归档（2026-11-01 合并）

> 本文件由 30 份历史报告按主题合并而成（全部条目为 fixed / wontfix，无 open）。
> 报告规范与新报告的落盘方式见 [README.md](README.md) 与 `AGENTS.md`；
> `files:` 并集与本次 diff 有交集时，同样要求本归档无 `status: open`（当前成立）。

## 目录

- **归并引擎（merge）**
  - D5 rewrite 包、merge 工厂与无 GROUP BY 聚合 review（原 `review-d5-rewrite-merge-aggregate.md`）
  - 多分片并发 dispatch review（原 `review-d7-concurrent-dispatch.md`）
  - 多分片 SELECT scatter + concat review（原 `review-multi-shard-concat.md`）
  - 多分片 LIMIT concat 截断 review（原 `review-multishard-limit.md`）
  - 多分片 LIMIT OFFSET 改写 review（原 `review-multishard-limit-offset.md`）
  - 多分片 ORDER BY 有序归并 review（原 `review-multishard-orderby.md`）
  - ORDER BY 表达式隐藏列 review（原 `review-orderby-expression.md`）
  - 未投影列 ORDER BY 隐藏列与投影 review（原 `review-orderby-hidden.md`）
  - 多分片 GROUP BY 归并 review（原 `review-groupby.md`）
  - GROUP BY HAVING 下推与全局过滤 review（原 `review-having.md`）
  - 多分片 DISTINCT 归并 review（原 `review-distinct.md`）
  - GROUP BY/DISTINCT 排序与 LIMIT review（原 `review-group-distinct-sort-limit.md`）
  - 聚合 DISTINCT 跨片归并 review（原 `review-aggregate-distinct.md`）
  - 多分片 AVG 隐藏列改写 review（原 `review-avg-pushdown.md`）
- **语句判定与会话（sql / session）**
  - LAST_INSERT_ID / ROW_COUNT 会话函数支持 review（原 `review-session-functions.md`）
  - 显式拒绝不支持语句 review（原 `review-unsupported-statements.md`）
  - DQL 兼容（EXPLAIN ROUTE / prepared SELECT / SHOW-USE-SET NAMES）review（原 `review-dql-compat.md`）
  - 语句判定统一（A 轨）与字符串 collation 归并（D1–D4）review（原 `review-statement-classification-and-collation.md`）
  - collation 来源（方案 C）与会话变量重放（B2）review（原 `review-collation-source-and-session-vars.md`）
- **路由与装配（route / plan / server）**
  - B1.0 单分片路由与 sharding 装配 review（原 `review-shard-router-b1.md`）
  - 分片键类型严格化（hash_mod / hash_mod_str）review（原 `review-shard-key-types.md`）
  - EXPLAIN ROUTE 与 DQL 路由可观测 review（原 `review-explain-route.md`）
  - 分片配置模型与多 Slice 装配 review（原 `review-sharding-config-assembly.md`）
- **协议与池（mysql / backend）**
  - 连接池归还生命周期（不再无条件 Reset）review（原 `review-pool-lifecycle.md`）
  - 统一 errs 包 review（原 `review-errors-package.md`）
- **测试与重构基建（test / refactor）**
  - 分片聚合 single-DB oracle review（原 `review-aggregate-metamorphic.md`）
  - 随机 SQL 差分（randgen）review（原 `review-randgen-differential.md`）
  - rwsplit 真实拓扑测试基建 review（原 `review-rwsplit-topology-harness.md`）
  - 分片复制拓扑 harness 与真实能力验证 review（原 `review-sharded-replica-harness.md`）
  - 存量架构清理 review（原 `review-architecture-cleanup.md`）

## 归并引擎（merge）

### D5 rewrite 包、merge 工厂与无 GROUP BY 聚合 review（原 `review-d5-rewrite-merge-aggregate.md`）

**files:**

- pkg/sql/rewrite/rewrite.go
- pkg/plan/plan.go
- pkg/merge/merge.go
- pkg/merge/aggregate.go
- pkg/execute/dispatch.go
- pkg/route/shard/router.go
- test/sharding/sharding_test.go

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

### 多分片并发 dispatch review（原 `review-d7-concurrent-dispatch.md`）

**files:**

- pkg/execute/pipeline.go
- pkg/execute/dispatch.go
- pkg/execute/dispatch_test.go

本次把多分片 dispatch 的 align/exec/列头解码从串行改为按 target 并发：
lease 仍顺序 acquire，避免 session map 并发写；结果按 target 下标收集，
写回/归并顺序不变。验证边界：execute 聚焦单测、完整 `make test-unit`、
2 Slice e2e、single-DB oracle 全绿；sharding e2e 约 69s，oracle 约 26s；
golangci-lint 0 issues。

## Must Fix

### 多分片执行串行，延迟随分片数线性增长
- status: fixed
- file: pkg/execute/dispatch.go

原实现逐个 acquire/align/exec/decode；N 片时首包延迟叠加。修复：先顺序
acquire lease，再为每个 target 启动 goroutine 执行
align + payloadFor + Exec + Decode；`sync.WaitGroup` 后按 target 顺序校验
列元数据并交给 merge。

### lease map 不可并发写
- status: fixed
- file: pkg/execute/pipeline.go

`Runtime.leases` 是普通 map，原设计只在单线程命令循环中使用。并发执行时
若同时 acquire 会 race。修复：acquire 阶段仍串行完成 map 写入；goroutine
只使用已分配的 lease，不再改 map。`go test -race` 覆盖。

### 并发错误路径的流/lease 清理
- status: fixed
- file: pkg/execute/dispatch.go

多个 goroutine 同时返回时，需要关闭已成功的 stream 并丢弃所有 lease，且只
向前端写一次 ERR。修复：等待全部 goroutine，按 target 顺序取第一个错误；
关闭所有成功 stream 后 `cleanup(true)`；`*errs.MySQL` 原样写 ERR，其他错误
返回导致会话关闭。

### 结果顺序保持 target 顺序
- status: fixed
- file: pkg/execute/dispatch.go

并发完成顺序不确定；结果按 `pl.Targets` 下标收集，concat/Ordered/Sort 等
算子看到的 stream 顺序与配置一致，测试仍可期望稳定的 tie 行为。

## Should Fix

### lease acquire 仍串行
- status: wontfix
- file: pkg/execute/dispatch.go

首次查询需要为 N 个 slice 建立连接时，握手仍串行。要并发 acquire 需要给
`Runtime.leases` 加锁或改为 per-target lease 结构；当前先解决执行阶段并发，
后续池/健康探测落地时再评估。

### context 取消不能中断进行中的后端 I/O
- status: wontfix
- file: pkg/backend/conn.go

`Conn.Align/Exec` 目前忽略 ctx，只有 packet 的读/写 deadline；goroutine 会
等到 I/O 返回。查询总超时/取消是独立的 P1 工作。

### goroutine 数等于 target 数
- status: wontfix
- file: pkg/execute/dispatch.go

按片一 goroutine；分片数通常为个位数到几十，暂不需要 worker pool。若未来
支持大批量片，再引入并发上限。

## Nit

### 并发测试用阻塞 hook
- status: wontfix
- file: pkg/execute/dispatch_test.go

测试让两个 fakeConn 的 Exec 互相等待并记录最大并发数；断言 ≥2。比测时间更
稳定，但 hook 仅用于测试，不进入产品路径。

### 元数据校验仍在并发之后
- status: wontfix
- file: pkg/execute/dispatch.go

列头解码并发完成后再统一比较；任一 mismatch 会关闭全部 stream 并 discard。
这保留了“写列头前失败”的语义，代价是多读了一些列头。

### 多分片 SELECT scatter + concat review（原 `review-multi-shard-concat.md`）

**files:**

- pkg/route/shard/router.go
- pkg/plan/plan.go
- pkg/execute/pipeline.go
- pkg/execute/dispatch.go
- pkg/session/state.go
- test/sharding/sharding_test.go

本次变更（D4.0）在分片查询链路上新增：无键/跨片 `IN` 的 scatter 路由、
`MultiShard + TxNone` 计划、`execute.dispatch` 的逐片列头校验与 concat 写回，
以及 prepared statement pin 的独立释放。验证边界：变更包聚焦单测
（`route/shard`、`plan`、`execute`、`session`）全绿；2 Slice 真拓扑 e2e
（Docker）通过；`go vet` 与 golangci-lint v2.15.0 干净。未跑全量差分/randgen，
按 AGENTS 需用户显式批准。

## Must Fix

### 跨片 IN 的组合语句绕过 scatter 形状检查
- status: fixed
- file: pkg/route/shard/router.go

初版只在“键约束不完整”时调用 `checkScatterSelect`。`WHERE user_id IN (1,2)
ORDER BY id` 的键值完整但映射到两个 slice，仍会生成 `MultiShard`，concat 后
顺序与 `ORDER BY` 语义不符（静默错误结果）。修复：只要最终 `len(slices) > 1`
就执行形状检查；单分片键（含同片 IN）仍允许排序/聚合，由该分片 MySQL 处理。
单测覆盖 `cross_shard_in_order_by`、`cross_shard_in_aggregate` 与
`keyed_single_order_by`。

### prepared pin 永不解释放，阻塞后续多分片查询
- status: fixed
- file: pkg/execute/pipeline.go, pkg/session/state.go

D2 用 `MarkReserved()` 让 prepared 会话持有后端连接，但 `COM_STMT_CLOSE`
只删除 StmtTable 条目，`reserved` 一直为 true。结果是同一连接只要 prepare 过
一次，后续所有 scatter 查询都被 1235 拒绝。修复：新增独立的 `stmtPinned`
状态（不污染 reserved 的 SQL 分类语义），最后一个 `COM_STMT_CLOSE` 清 pin 并
在 `runOne` 的 deferred sticky 检查中释放 lease；`Reset` 同步清理。
单测覆盖 pin/释放/Reset；e2e 覆盖“prepared 未关闭时 scatter 1235、关闭后恢复”。

### 失败路径把未读完的后端连接放回池
- status: fixed
- file: pkg/execute/dispatch.go

某片列元数据不一致或读行中途失败时，stream 里仍有未消费的 packet。若按普通
`Release`（COM_RESET_CONNECTION）还池，残留 packet 会被下一次请求读到，造成
协议错序。修复：失败 cleanup 统一 `Discard`（关闭连接）；成功路径才
`Release`。所有列头在写前端之前完成解码与一致性校验，因此元数据错误不会
产生部分结果集。单测 `MetadataMismatch`/`RowError`/`NoColumns` 断言连接被关闭。

### shard.New 接受重复/空 slice 名，scatter 会重复执行
- status: fixed
- file: pkg/route/shard/router.go

`Rule.Slices` 重复时（例如 `["s0","s0"]`），scatter 会生成两个相同 target，
对同一连接连续执行两次并破坏结果流；空 slice 名则会在运行时 `cluster.Get`
失败。配置层已校验，但 `shard.New` 是导出入口且被单测直接使用。修复：在
`New` 中拒绝空名与重复名，并补表驱动用例。

## Should Fix

### 多分片结束符 warnings 固定为 0
- status: wontfix
- file: pkg/execute/dispatch.go

concat 结果的 OK/EOF 结束符没有汇总各分片 `warning_count`。要正确汇总需要
`decodedStream` 暴露每个分片的终止状态，且 `SHOW WARNINGS` 在产品上已明确
不支持（1235），客户端无法消费对应 warning 详情。当前在设计中记录为已知
限制，D6 引入 merge 层时再统一处理终止符元数据。不修的理由：单独汇总一个
无法查询详情的计数没有可操作价值，反而增加协议状态面。

### dispatch 防御分支缺少用例
- status: fixed
- file: pkg/execute/dispatch.go

补了非 COM_QUERY、非空 MergeSpec、零 target、无列、行宽不符、不支持值类型
的表驱动用例，避免后续接线时静默走错路径。

## Nit

### scatter 对子查询中的聚合保守拒绝
- status: wontfix
- file: pkg/route/shard/router.go

`selectNeedsMerge` 会遍历整个 SELECT（含标量子查询），因此
`SELECT (SELECT COUNT(*) FROM t2) FROM orders` 这类本可在单片内完成的查询在
scatter 时被 1235。P0 选择保守拒绝而不是做子查询归属分析；D5 做 AST 改写
时可收窄到“当前查询块”的投影/HAVING。

### 逐片串行执行
- status: wontfix
- file: pkg/execute/dispatch.go

P0.5 顺序 acquire/exec，延迟随分片数线性增长。并发 dispatch、取消与失败
传播属于 D7；当前实现优先保证 lease/stream 生命周期简单可证明。

### 结果行顺序等于 target 顺序
- status: wontfix
- file: pkg/execute/dispatch.go

无 `ORDER BY` 的 SQL 本身不保证行序，concat 按规则配置的 slice 顺序输出。
需要稳定顺序的客户端应使用单分片键或等待 D5 的 ORDER BY 下推；文档已注明。

### 多分片 LIMIT concat 截断 review（原 `review-multishard-limit.md`）

**files:**

- pkg/merge/merge.go
- pkg/sql/limit.go
- pkg/plan/plan.go
- pkg/route/shard/router.go
- pkg/execute/dispatch.go
- test/sharding/sharding_test.go

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

### 多分片 LIMIT OFFSET 改写 review（原 `review-multishard-limit-offset.md`）

**files:**

- pkg/plan/plan.go
- test/sharding/sharding_test.go

本次增量支持 `LIMIT m, n` / `LIMIT n OFFSET m`：plan 用 AST restore 把每个
target 的 SQL 改写为 `LIMIT m+n`，`merge.Limit` 在归并后全局跳过 m 行。
验证边界：plan/execute/merge 聚焦单测、完整 `make test-unit`、2 Slice 真拓扑
e2e 全绿；golangci-lint 0 issues。未跑全量差分/randgen，按 AGENTS 需用户
显式批准。

## Must Fix

（无）

## Should Fix

### 改写不能永久修改调用方 AST
- status: fixed
- file: pkg/plan/plan.go

`SelectStmt` 没有 Clone；若直接覆盖 `sel.Limit` 后 restore，语句 AST 会残留
per-shard 的 `LIMIT`，影响同请求后续使用（例如 EXPLAIN/日志/重试）。
`restoreSelectLimit` 保存旧 `Limit` 指针并在 defer 中恢复；单测在 Build 后
再次 `sql.ParseLimit(st.AST.(*ast.SelectStmt).Limit)`，断言仍是原始
`OFFSET 2 / COUNT 3`。

### OFFSET+COUNT 溢出
- status: fixed
- file: pkg/plan/plan.go

`offset+count` 可能超过 int64；若不检查，改写的 LIMIT 会变成负数/错误值。
修复：`count > math.MaxInt64-offset` 时 1235；单测覆盖
`OFFSET 9223372036854775807`。

### 每片先丢 OFFSET 会少读行
- status: fixed
- file: pkg/plan/plan.go

若每个分片仍执行原 `LIMIT m, n`，concat/merge 再全局跳过 m 行会少读；改写为
`LIMIT m+n` 后，归并层能凑够 m+n 行再截断。e2e 用
`ORDER BY user_id LIMIT 1 OFFSET 1` 验证返回第二行，以及
`LIMIT 5 OFFSET 10` 返回 0 行。

## Nit

### AST restore 会丢注释/优化器 hint
- status: wontfix
- file: pkg/plan/plan.go

`SelectStmt.Restore` 只保留语法结构：`/*master*/`、`/*+ MAX_EXECUTION_TIME */`
等注释/提示不会进入改写后的 SQL。路由角色在 rewrite 前已由 raw SQL 决定，
`MAX_EXECUTION_TIME` 属于尚未实现的查询超时能力；P0 接受该限制。需要保留
hint 时应改为基于 AST 的 hint 保留/文本区间替换。

### 改写后 SQL 的格式与字面量形式
- status: wontfix
- file: pkg/plan/plan.go

Restore 使用 `DefaultRestoreFlags`（反引号标识符、单引号字符串、大写关键字），
语义等价但文本与用户输入不同；`EXPLAIN ROUTE` 会展示改写后的 SQL。若需要
“原样显示”，应在 `Target` 上区分 `SQL` 与 `DisplaySQL`，当前不做。

### 多分片 ORDER BY 有序归并 review（原 `review-multishard-orderby.md`）

**files:**

- pkg/merge/ordered.go
- pkg/plan/plan.go
- pkg/route/shard/router.go
- pkg/execute/dispatch.go
- test/sharding/sharding_test.go

本次增量实现多分片 `ORDER BY`：各分片执行原排序，`pkg/merge.Ordered` 用
k 路堆归并已排序的 `result.Stream`；同时支持 `ORDER BY ... LIMIT n`
（OFFSET=0）。验证边界：`pkg/merge`/`plan`/`route`/`execute` 聚焦单测、
完整 `make test-unit`、2 Slice 真拓扑 e2e 全绿；golangci-lint 0 issues。
未跑全量差分/randgen，按 AGENTS 需用户显式批准。

## Must Fix

### ORDER BY 类型/列校验必须在写列头之前
- status: fixed
- file: pkg/execute/dispatch.go, pkg/merge/ordered.go

如果先写列数/列定义再发现“排序列缺失”或“字符串列不支持”，客户端会收到
半截结果集后断连。修复：`merge.Ordered` 在任何 packet 写出前解析列名/位置、
校验字段类型并构建比较器；失败返回 `*errs.MySQL`（1235），dispatch 丢弃
后端连接后由 proxy 正常写 ERR。单测断言此时前端 packet 为空。

### 字符串/JSON 等 collation 相关排序不能按字节比较
- status: fixed
- file: pkg/merge/ordered.go

MySQL 字符串排序依赖 collation（大小写、重音、语言规则），按 UTF-8 字节序
会静默返回不同顺序。修复：只支持整数/无符号整数、FLOAT/DOUBLE、DECIMAL、
DATE/DATETIME/TIMESTAMP（规范文本）比较；其他类型显式 1235。后续需要
charset/collation 元数据再开放字符串排序。

### NULL 排序语义
- status: fixed
- file: pkg/merge/ordered.go

MySQL 规定 NULL 在 ASC 时最前、DESC 时最后。比较器先按 ASC 语义比较
（NULL 小于非 NULL），DESC 时统一取反，保证两种方向都正确；表驱动单测覆盖
两片各含 NULL 的合并。

### DESC 归并要求每个分片按 DESC 返回
- status: fixed
- file: pkg/plan/plan.go, pkg/merge/ordered.go

`Ordered` 假设每条上游流已按目标键有序；`ORDER BY x DESC` 时若分片仍按 ASC
返回，k 路堆会产生错误交错（单测最初用 ASC 流模拟 DESC 已复现）。当前实现
把原 SQL（含 DESC）原样发给每个分片，因此上游顺序与归并方向一致；测试改为
分别提供 ASC/DESC 有序流。

## Should Fix

### ORDER BY 隐藏列（未投影列）暂不支持
- status: wontfix
- file: pkg/merge/ordered.go

`SELECT amount FROM orders ORDER BY user_id` 需要把 user_id 作为隐藏列带回，
否则归并层拿不到排序键。当前按输出名/OrgName 解析，找不到即 1235；e2e 覆盖。
隐藏列注入属于 D5.1 的 rewrite 工作（同时可减少对 `SELECT *` 的字段推断）。

### LIMIT OFFSET 仍需 per-shard 改写
- status: fixed
- file: pkg/plan/plan.go

`LIMIT m, n` 需要每片改写成 `LIMIT m+n` 再全局跳过 m 行。D5.1b 已实现：
plan 用 AST restore 生成 per-target SQL，`merge.Limit` 负责全局跳过；单测与
e2e 覆盖。

### 表达式排序（`ORDER BY amount+1`）
- status: wontfix
- file: pkg/plan/plan.go

表达式同样需要隐藏列/表达式下推；当前只接受列名或位置（`ORDER BY 1`），
其他 1235。计划在 D5.1 的 rewrite 中扩展。

## Nit

### DECIMAL 每行分配 big.Rat
- status: wontfix
- file: pkg/merge/ordered.go

DECIMAL 比较用 `big.Rat` 保证精度，行读取时每个排序键分配一次；P0 可接受。
后续可缓存解析结果或按 scale 用 int64/128 比较。

### 相同排序键的 tie-break
- status: wontfix
- file: pkg/merge/ordered.go

键相等时按上游流下标决定顺序，保证确定性；MySQL 对相同排序键不保证顺序，
因此不构成语义差异。

### ORDER BY 表达式隐藏列 review（原 `review-orderby-expression.md`）

**files:**

- pkg/plan/plan.go
- pkg/sql/rewrite/rewrite.go
- pkg/merge/merge.go
- test/metamorphic/aggregate_test.go

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

### 未投影列 ORDER BY 隐藏列与投影 review（原 `review-orderby-hidden.md`）

**files:**

- pkg/plan/plan.go
- pkg/sql/rewrite/rewrite.go
- pkg/merge/merge.go
- pkg/execute/dispatch_test.go
- test/metamorphic/aggregate_test.go

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

### 多分片 GROUP BY 归并 review（原 `review-groupby.md`）

**files:**

- pkg/plan/plan.go
- pkg/merge/group.go
- pkg/merge/merge.go
- pkg/execute/dispatch_test.go
- test/metamorphic/aggregate_test.go

本次实现多分片 `GROUP BY`：plan 校验分组列/聚合投影并复用 AVG 隐藏列改写，
merge 用 map 累积每个 group 的部分聚合。验证边界：plan/merge/execute 单测、
完整 `make test-unit`、2 Slice e2e、single-DB oracle 全绿；sharding e2e 约
69s，oracle 约 28s；golangci-lint 0 issues。

## Must Fix

### 分组列必须在结果集中
- status: fixed
- file: pkg/plan/plan.go

`SELECT COUNT(*) FROM orders GROUP BY user_id` 没有把分组键带回，归并层无法
区分 group。修复：显式 1235，要求 GROUP BY 列出现在 select list；位置/别名
按 `selectFieldIndex` 解析，未选列/表达式拒绝。

### 跨片部分分组不能直接拼接
- status: fixed
- file: pkg/merge/group.go

同一个 group 可能出现在多个分片（例如 `GROUP BY amount`）。修复：`Group`
用规范化 group key 累积 `COUNT/SUM/MIN/MAX/AVG`，同 key 合并后输出一次；
AVG 复用隐藏 SUM/COUNT 列。

### AVG 隐藏列与输出投影
- status: fixed
- file: pkg/plan/plan.go, pkg/merge/group.go

GROUP BY + AVG 时隐藏列追加在原 select 之后，`Group` 只读取隐藏 SUM/COUNT，
输出仍按 `Output` 投影到原始列；列名/scale 由原 AVG 字段元数据决定。

### 输出顺序不确定
- status: fixed
- file: pkg/merge/group.go

MySQL 未保证 GROUP BY 顺序；`Group` 按 group 首次出现顺序输出，测试/oracle
对分组结果做排序后比较，不依赖代理顺序。

## Should Fix

### 字符串/二进制分组键
- status: wontfix
- file: pkg/merge/group.go

需要 collation 权重表/规范化 key；当前字符串分组 1235。按已定方向先不做
权重表，客户端使用数值/日期分组或显式拼接键。

### 无分组内存上限
- status: wontfix
- file: pkg/merge/group.go

`Group` 的 map 随分组数增长，尚未设置内存上限/超限错误；当前 P0 数据量与
测试场景可接受。GROUP BY/DISTINCT 正式开放前应加配置上限（如 64 MiB 或
最大组数）与稳定错误码。

### GROUP BY + ORDER BY/LIMIT
- status: wontfix
- file: pkg/plan/plan.go

需要在分组后再排序/截断；当前显式 1235，属于 D5.2d 后续。

### HAVING / 聚合 DISTINCT / DISTINCT
- status: wontfix
- file: pkg/plan/plan.go

HAVING 需要聚合后过滤；DISTINCT 需要去重算子；当前均 1235。

## Nit

### group key 编码
- status: wontfix
- file: pkg/merge/group.go

数值/无符号/浮点/DECIMAL/日期文本分别编码并长度前缀；float 归一化 -0。
字符串若未来接入 collation，应改用 WeightString 而不是原始字节。

### 分组输出值代表
- status: wontfix
- file: pkg/merge/group.go

同一 group 的分组列原始值取首次出现的行；MySQL 对等价值（如未来 PAD SPACE
字符串）返回哪个代表值本身不保证，当前 bin/数值场景无歧义。

### GROUP BY HAVING 下推与全局过滤 review（原 `review-having.md`）

**files:**

- pkg/plan/plan.go
- pkg/sql/rewrite/rewrite.go
- pkg/merge/filter.go
- pkg/merge/merge.go
- test/metamorphic/aggregate_test.go

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

### 多分片 DISTINCT 归并 review（原 `review-distinct.md`）

**files:**

- pkg/plan/plan.go
- pkg/merge/distinct.go
- pkg/merge/merge.go
- pkg/execute/dispatch_test.go
- test/metamorphic/aggregate_test.go

本次实现跨片 `SELECT DISTINCT`：各分片先执行 DISTINCT，归并层按规范化行 key
去重。验证边界：plan/merge/execute 单测、完整 `make test-unit`、2 Slice e2e、
single-DB oracle 全绿；sharding e2e 约 69s，oracle 约 27s；golangci-lint
0 issues。

## Must Fix

### 各片 DISTINCT 后仍有跨片重复
- status: fixed
- file: pkg/merge/distinct.go

`SELECT DISTINCT amount` 中同一个 amount 可能在不同 slice 各出现一次，单纯
concat 会重复。修复：`Distinct` 用规范化行 key 去重，跨流只输出首次出现的行；
单测覆盖跨流重复、NULL、空输入。

### 行 key 的数值/日期规范化
- status: fixed
- file: pkg/merge/distinct.go

文本结果不能直接按字节去重：数值/DECIMAL/日期需要按类型规范化，NULL 单独
表示。修复：复用 `encodeRowKey`（int/uint/float/DECIMAL/日期文本），float
归一化 -0；字符串类型在构造阶段 1235。

### DISTINCT 与其他归并语义的组合
- status: fixed
- file: pkg/plan/plan.go, pkg/merge/merge.go

`DISTINCT + ORDER BY/LIMIT`、`DISTINCT + 聚合`、`DISTINCT + GROUP BY` 需要
先定义执行顺序；当前显式 1235，避免结果不确定。

### 输出顺序
- status: fixed
- file: pkg/merge/distinct.go

DISTINCT 未保证顺序；实现按行首次出现顺序输出，oracle 对行排序后比较，不
依赖代理顺序。

## Should Fix

### 字符串/二进制 DISTINCT
- status: wontfix
- file: pkg/merge/distinct.go

需要 collation 权重/规范化 key；当前字符串列 1235。按已定方向暂不引入权重表。

### 去重集合无内存上限
- status: wontfix
- file: pkg/merge/distinct.go

`seen` map 随 distinct 行数增长；尚未设置内存预算/超限错误。正式开放前应与
GROUP BY 一起加配置上限。

### DISTINCT + ORDER BY/LIMIT
- status: wontfix
- file: pkg/plan/plan.go

需要在去重后排序/截断；当前 1235，属于后续 D5.2d/merge operator 组合。

### 聚合 DISTINCT
- status: wontfix
- file: pkg/plan/plan.go

`COUNT(DISTINCT ...)` 需要跨片去重集合而非简单求和；当前 1235。

## Nit

### 行 key 编码
- status: wontfix
- file: pkg/merge/distinct.go

key 使用类型前缀 + 长度前缀，避免数值/字符串拼接冲突；字符串未来接入
collation 时应改为 WeightString。

### 首次出现顺序
- status: wontfix
- file: pkg/merge/distinct.go

输出顺序取决于 target 顺序与分片返回顺序；MySQL 未保证 DISTINCT 顺序，
客户端需要稳定顺序时应显式 ORDER BY（当前该组合仍 1235）。

### GROUP BY/DISTINCT 排序与 LIMIT review（原 `review-group-distinct-sort-limit.md`）

**files:**

- pkg/plan/plan.go
- pkg/sql/rewrite/rewrite.go
- pkg/merge/sort.go
- pkg/merge/merge.go
- pkg/execute/dispatch_test.go
- test/metamorphic/aggregate_test.go

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

### 聚合 DISTINCT 跨片归并 review（原 `review-aggregate-distinct.md`）

**files:**

- pkg/plan/plan.go
- pkg/sql/rewrite/rewrite.go
- pkg/merge/aggregate.go
- pkg/merge/group.go
- pkg/merge/aggregate_distinct.go
- test/metamorphic/aggregate_test.go
- test/sharding/sharding_test.go
- test/sharding/replica_test.go

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

### 多分片 AVG 隐藏列改写 review（原 `review-avg-pushdown.md`）

**files:**

- pkg/plan/plan.go
- pkg/sql/rewrite/rewrite.go
- pkg/merge/aggregate.go
- pkg/merge/aggregate_test.go
- test/metamorphic/aggregate_test.go

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

## 语句判定与会话（sql / session）

### LAST_INSERT_ID / ROW_COUNT 会话函数支持 review（原 `review-session-functions.md`）

**files:**

- pkg/mysql/forward.go
- pkg/backend/stream.go
- pkg/execute/forward.go
- pkg/execute/pipeline.go
- pkg/sql/session_func.go
- pkg/sql/statement.go
- pkg/sql/unsupported.go
- pkg/session/state.go

## Must Fix

（无）

## Should Fix

### 多语句的 ROW_COUNT 依赖首关键字
- status: wontfix
- file: pkg/sql/statement.go

`sql.IsDML` 只看整段 SQL 的第一个关键字。`SELECT 1; INSERT ...` 这类多语句
最终 OK 的 affected_rows 正确，但首关键字是 SELECT，`rowCount` 会被写成 0
而不是 affected_rows。多语句本身在代理里是保守走 master，且客户端很少依赖
多语句后的 ROW_COUNT；需要逐语句解析/逐结果记录才能完全正确，暂不实现。

### 非精确函数形式改为拒绝
- status: wontfix
- file: pkg/sql/unsupported.go

`INSERT ... VALUES (LAST_INSERT_ID())`、`SET @x = LAST_INSERT_ID()`、
`SELECT ROW_COUNT() + 1` 等非精确形式现在返回 1235。这是有意的安全策略：
不做部分改写，避免静默返回错误值；后续如需支持，应基于 AST 改写。

### prepared 形式的 LAST_INSERT_ID/ROW_COUNT 不支持
- status: wontfix
- file: pkg/execute/pipeline.go

`COM_STMT_PREPARE SELECT LAST_INSERT_ID()` 在 prepare 阶段返回 1235。文本协议
可以改写成字面量，二进制协议需要本地合成二进制结果集，工作量和风险都更高；
常见 ORM 从 OK 包取 generated key，不依赖 prepared 形式。

### 改写后的 SELECT 仍需要一次后端往返
- status: wontfix
- file: pkg/execute/pipeline.go

`SELECT LAST_INSERT_ID()` / `ROW_COUNT()` 被改写成 `CAST(...)` 字面量后仍走
现有转发链路，需要一个健康后端连接。Gaea/kingshard 是本地构造结果集，不依赖
后端；当前实现换来的是不新增结果集编码路径，且值本身与后端数据无关。后端全挂
时这条查询也会失败，属于已知取舍。

## Nit

### `AS` 与引号别名之间要求空白
- status: wontfix
- file: pkg/sql/session_func.go

`SELECT LAST_INSERT_ID() AS\`id\`` 这种 `AS` 后直接跟反引号的写法不被识别；
常见客户端都会带空格或省略 `AS`，影响很小。

### affected_rows 转 int64
- status: wontfix
- file: pkg/execute/pipeline.go

`int64(info.AffectedRows)` 在超过 MaxInt64 时会溢出；MySQL 单语句 affected
rows 不可能达到这个量级，保持简单转换。

### 显式拒绝不支持语句 review（原 `review-unsupported-statements.md`）

**files:**

- pkg/sql/unsupported.go
- pkg/plan/plan.go
- pkg/proxy/session.go

## Must Fix

### 拒绝语句后会话被关闭
- status: fixed
- file: pkg/proxy/session.go

`handleExecError` 对 Reject 写 ERR 后返回 nil，但四个调用点仍然 `return
handleExecError(...)`，于是 `Serve` 退出、连接关闭，客户端无法继续发语句。
改为调用点检查 `herr != nil`：Reject 时 `continue`，其他错误保持原行为退出。
新增 `TestServe_RejectErrorWrites1235AndContinues` 验证 ERR 后 `COM_PING` 仍可用。

## Should Fix

### 只拒绝了 SET sql_mode，其他会话变量仍静默
- status: wontfix
- file: pkg/sql/unsupported.go

`SET time_zone` / `transaction_isolation` 等仍未跟踪也未拒绝，会被路由到后端后
丢失。产品尚未决策，已在单机差距评估中记录；R1 统一按
ConnectionState pin 策略处理，届时扩展 `unsupported.go` 或替换为 StatementClass。

## Nit

### 文本启发式而非 AST
- status: wontfix
- file: pkg/sql/unsupported.go

检测基于 COM_QUERY 文本（剥离前导注释），覆盖 SHOW 前缀与 SET 赋值列表，
不解析 AST。对当前明确的拒绝清单足够；`StatementClass` 落地后替换为 AST 分类。

### 错误文案固定英文
- status: wontfix
- file: pkg/sql/unsupported.go

客户端错误文案保持英文与 MySQL 风格一致，便于驱动/用户识别；不做本地化。

### DQL 兼容（EXPLAIN ROUTE / prepared SELECT / SHOW-USE-SET NAMES）review（原 `review-dql-compat.md`）

**files:**

- pkg/route/shard/router.go
- pkg/mysql/forward.go
- pkg/backend/stream.go
- pkg/execute/forward.go
- pkg/execute/pipeline.go
- pkg/proxy/session.go
- pkg/session/state.go
- pkg/session/stmt.go
- pkg/sql/statement.go

## Must Fix

（无）

## Should Fix

### prepared 分片键必须是字面量
- status: wontfix
- file: pkg/route/shard/router.go

`COM_STMT_PREPARE` 时才能确定目标片；`WHERE shard_key = ?` 的参数值在
execute 才可见。D2 v1 要求分片键在 prepare 时是字面量，参数化分片键返回
1235。完整支持需要 execute-time 参数解码 + 广播/惰性 prepare，单独立项。

### prepared 会话会长期 pin 后端连接
- status: wontfix
- file: pkg/execute/pipeline.go

prepare 成功后 `MarkReserved()` 保持 lease，避免后端 stmt id 随连接归还失效；
D2 v1 不在最后一个 stmt close 时解除（reserved 还承载临时表/用户变量语义）。
`COM_RESET_CONNECTION` 会清理。后续引入 prepared-only pin 计数再优化。

### SHOW 走默认片，不做跨片合并
- status: wontfix
- file: pkg/route/shard/router.go

`SHOW TABLES/COLUMNS/CREATE/INDEX/VARIABLES` 路由默认片，依赖外部迁移保证各片
schema 一致；不做结果合并/差异检测。`SHOW STATUS/MASTER` 等仍 1235。

### USE / SET NAMES 只在默认片执行
- status: wontfix
- file: pkg/route/shard/router.go

语句本身在默认片执行，但 `Effect` 写入 `session.State`，后续 lease 通过
`Align` 重放 database/charset，所以其他片也能看到会话效果。

## Nit

### proxy 为 prepared 分配 client stmt id
- status: wontfix
- file: pkg/mysql/forward.go

proxy 改写 prepare OK 的 statement_id，并维护 client→backend 映射；这样不同
slice 的后端 id 冲突不会泄漏给客户端。已有 mysql 层 fixture 单测。

### 语句判定统一（A 轨）与字符串 collation 归并（D1–D4）review（原 `review-statement-classification-and-collation.md`）

**files:**

- pkg/sql/scanner.go
- pkg/sql/classify.go
- pkg/sql/statement.go
- pkg/sql/session_func.go
- pkg/proxy/serve.go
- pkg/execute/pipeline.go
- pkg/plan/plan.go
- pkg/route/source/pick.go
- pkg/route/source/routers.go
- pkg/route/shard/router.go
- pkg/merge/collation.go
- pkg/merge/ordered.go
- pkg/merge/sort.go
- pkg/merge/group.go
- pkg/merge/distinct.go
- pkg/merge/aggregate.go
- pkg/merge/aggregate_distinct.go
- pkg/merge/filter.go
- pkg/result/type.go
- pkg/mysql/collation
- pkg/mysql/client/conn.go

## Must Fix

### 列 collation 取自结果集元数据是错的，collated 归并在部分场景静默出错
- status: wontfix（2026-11-01 用户决策：暂不修复，保持功能启用）
- file: pkg/merge/collation.go、test/metamorphic/collation_test.go

D5 oracle（2026-11-01 触发）发现：MySQL ColumnDefinition41 的 charset 字段不携带列的
真实 collation，而是**结果字符集的 collation**。探针进一步定位到触发条件：**代理连接池
每次 Release 都发 `COM_RESET_CONNECTION`，它会把会话字符集重置回服务端默认**。实测同一条
连接：握手 45 时首条查询元数据为 45；`COM_RESET_CONNECTION` 之后变为 255（服务端默认）。
因此每个池化连接只有第一条查询是 45，之后都是 255——oracle 中「ORDER BY 报 45、
GROUP BY/聚合报 255」源于连接新旧，不是查询形态。

真实 collation 本身是**可获得的**（已实测）：`information_schema.columns.COLLATION_NAME`
与 `SELECT COLLATION(col)` 都返回正确值，与连接 charset 无关。缺的是把它接进 merge 的
管道，不是数据不可得。另注：握手固定 45 只对首条查询有效；若后端默认字符集不是
utf8mb4，Reset 后结果会被转码成服务端默认，「只支持 utf8mb4」的假设在首查后失效。

**影响矩阵（实测）**：

| 表列 collation | ORDER BY | GROUP BY / DISTINCT / MIN/MAX |
|---|---|---|
| `utf8mb4_0900_ai_ci`（MySQL 8 默认） | 用 45（PAD）；尾随空格及部分权重与 0900 不同 → 可能错 | 用 255 → 正确 |
| `utf8mb4_general_ci` | 用 45 → 正确 | 用 255（NO PAD）→ 尾随空格分组错误 |
| `utf8mb4_bin` | 用 45（CI）→ 大小写排序错误 | 用 255 → 大小写/尾随空格错误 |

**不修理由**：用户决定先保持启用，接受上述已知场景的风险；oracle 测试
`test/metamorphic/collation_test.go` 保留为失败复现（它不在 `make test-integration`
的用例列表中，仓库 CI 门禁不受影响）。后续如需修复，方案见
merge-engine-design §13.8（A: catalog；B: 隐藏 `COLLATION()` 列），届时把本项改回 open。

### PAD SPACE 未实现，GROUP BY/DISTINCT 对尾随空格错误
- status: fixed
- file: pkg/merge/collation.go

原问题：`utf8mb4_general_ci` 与 `utf8mb4_bin` 是 PAD SPACE collation，MySQL 比较时忽略
尾随空格（实测：`'a'='a '` 返回 1，GROUP BY 合并为 1 组）。vendored 包的
`Collate`/`WeightString` 不自动裁剪，导致跨分片 GROUP BY/DISTINCT 把 `'a'` 与 `'a '`
分成两组（`*_0900_*` 为 NO PAD，行为本就正确）。

修复：`paddedCollator`（裁剪尾随空格后委托上游 collator）与 `byteCollator{pad:true}`
（`utf8mb4_bin`）；`binary` charset 保持字节敏感、不裁剪。测试覆盖
general_ci/bin 合并为一组、0900 保持两组，并用 MySQL 8.0 实测值锚定。

### 字符串比较缺少连接 charset 一致性保证
- status: fixed
- file: pkg/merge/collation.go、pkg/mysql/client/conn.go、pkg/sql/classify.go

原问题：`collatorFor` 按列的 collation ID 取 collator，但结果字节的实际 charset 取决于
会话 charset。MySQL 会把结果值转码到 `character_set_results`；若客户端
`SET NAMES latin1`，utf8mb4 列返回的是 latin1 字节，再用 utf8mb4 collator 比较会得到
错误顺序/分组（ASCII 不受影响，非 ASCII 会错）。

修复（2026-11-01 决策，D5）：两端都固定 utf8mb4——
- 前端：`Class.UnsupportedReason` 对 `SET NAMES <非 utf8mb4>` 返回 1235；
- 后端：`client.Dial` 的 HandshakeResponse41 固定 `Collation=45`
  (`utf8mb4_general_ci`)，不再回显后端默认 collation。

因此归并层可以安全假定结果字节为 utf8mb4。单测：`set_names_latin1/utf8/gbk` 拒绝、
`set_names_utf8mb4[_collate|quoted]` 放行；`TestDial_PinsUTF8mb4Collation` 用 net.Pipe
假服务端验证握手不再回显 33。

## Should Fix

### 上游 collation 代码的 provenance 未锁定 release tag
- status: wontfix
- file: pkg/mysql/collation/doc.go

迁移时本地 checkout 无 tag（commit `6c6c003`，2026-10-30），因此以 commit 作为可复现
锁定，并在 doc.go/NOTICE 中记录。design §13.3 期望 release tag，但当前仓库检查点不具备
该信息；commit 锁定同样可复现，待拿到 tag 后仅需更新文档记录（代码不变），故不在本次
提交范围内。

### D5（metamorphic oracle）未落地，collation 保真只有单测覆盖
- status: fixed
- file: test/metamorphic/collation_test.go

已实现 `TestShardedCollationOracle`（4 分片代理 vs 单库 oracle，`general_ci` /
`0900_ai_ci` / `bin` × ORDER BY / GROUP BY / DISTINCT / COUNT(DISTINCT) / MIN/MAX），
并已按用户要求触发一次。它同时暴露了「列 collation 来源」缺陷（见 Must Fix 的
wontfix 项），保留为失败复现，不在 `make test-integration` 列表中，CI 门禁不受影响。

### 设计文档 API 与实现不一致
- status: fixed
- file: docs/design/statement-classification.md

设计稿写 `Classify(tokens)`，实现为 `Classify(raw, tokens)`（列标签与 SHOW 子类型需要
原文切片），`Token` 增加 `End`，`Class` 增加 `ScanFailed`。文档 §2 已同步。

### 独立检查器对可改写会话函数的语义不一致
- status: fixed
- file: pkg/sql/classify.go

旧 `UnsupportedReason` 会拒绝 `SELECT ROW_COUNT() AS n`，但端到端流程先由
`rewriteSessionFunc` 改写成字面量，因此该拒绝从未生效。新 `Class.UnsupportedReason`
以 `SessionFunc != nil` 为判据，与端到端行为一致；已在设计 §5.2 账本第 7 行记录。

## Nit

### groupState MIN/MAX 每行重复解析 comparator
- status: wontfix
- file: pkg/merge/group.go

`groupState.add` 每行调用一次 `orderComparatorFor`（含 collation 查表）。可把解析结果
缓存到 groupState。当前每行成本为一次 map 查找 + 字符串比较，相对于行解码可忽略；
留待性能剖析后处理。

### collEnv 进程级单例假设所有 slice 同一 MySQL 版本
- status: wontfix
- file: pkg/merge/collation.go

design §13.4 item 4 明确 Phase 1 接受该假设；混合版本策略在 §13.7 开放问题中跟踪。

### EXPLAIN ROUTE 仍做原文扫描
- status: wontfix
- file: pkg/sql/explain_route.go

`ParseExplainRoute` 需要从 `EXPLAIN ROUTE <sql>` 中提取内层 SQL 原文，属语法提取而非
语义判定；内层语句随后仍走 `Classify`。不在 grep 门禁范围内。

### 删除的重复辅助函数
- status: fixed
- file: pkg/sql

`unsupported.go`/`reserved.go`/`session_cmd.go` 的解析部分、`route/source/classify.go`
及重复的 `hasWordPrefix`×2、`stripLeadingComments`×2、`stripSQLStrings`、`splitTopLevel`、
`containsSQLFunc`、`containsSQLIdent`、`skipSQLSpaces`、`parseSQLAlias` 已删除；行为规格
平移到 `classify_test.go`。

### collation 来源（方案 C）与会话变量重放（B2）review（原 `review-collation-source-and-session-vars.md`）

**files:**

- pkg/backend/conn.go
- pkg/mysql/client/conn.go
- pkg/sql/classify.go
- pkg/session/state.go
- pkg/execute/pipeline.go

## Must Fix

### 列 collation 来源错误（静默错结果）
- status: fixed
- file: pkg/backend/conn.go

`Field.Charset` 报的是连接的 `character_set_results` 而非列 collation，跨 collation 的
ORDER BY/GROUP BY/DISTINCT/MIN/MAX 会静默错序/错分组（oracle 实证：移除 Reset 后
general_ci 过、0900/bin 红；此前 0900 过、general_ci/bin 红——正确集合随连接状态翻转）。

修复（方案 C，merge-engine-design §13.8）：
- `finishDial` 在握手后固定 `SET character_set_results = NULL`，元数据改为返回列真实
  collation；utf8mb4 数据字节不变（转码本为 no-op）；
- `Align` 的字符集对齐从 `SET NAMES` 改为 `SET character_set_client/connection`，
  避免重置 results。

验收：`TestShardedCollationOracle` 三子用例（general_ci/0900_ai_ci/bin）全部转绿；
单测 `TestDialMySQLWithOptions_SetsResultsNull`、`TestMySQLConn_Align_USEAndSetNames`。

### 白名单外 SET SESSION 静默丢失
- status: fixed
- file: pkg/sql/classify.go、pkg/execute/pipeline.go

白名单外会话变量此前透传执行后丢弃连接：客户端收到 OK 但下一条查询换新连接、变量
静默消失。修复（B2，Gaea 模式）：
- `Class.UnsupportedReason` 对白名单（time_zone/transaction_isolation/autocommit）外
  的 `SET SESSION` 返回 1235；
- 白名单变量经 `Class.SessionVars` → `Effect.Vars` → `session.State.vars` 快照，
  `Align` 按 `MySQLConn.vars` diff 重放为一条 `SET`，unused 变量重置为 `DEFAULT`；
- `dirtyForReuse` 删除 SessionVarWrite 检查（拒绝前置后执行层只见白名单）。

## Should Fix

### 重放与透传执行之间的冗余 SET
- status: wontfix
- file: pkg/backend/conn.go

会话执行 `SET time_zone=X` 时该语句透传到当前连接，但 `MySQLConn.vars` 未记录；
下一条查询的 Align 会再发一次相同 SET（幂等、每会话至多一次）。在透传路径回写
连接变量跟踪需要把执行语句与 Align 状态耦合，收益一行冗余 SQL，不值得。

### 多语句中嵌套的非白名单 SET 仍是「执行后丢弃」
- status: wontfix
- file: pkg/execute/pipeline.go

`SELECT 1; SET SESSION x=1` 的 Class.Kind 是 Select，parseSet 不运行，白名单拒绝不
触发；现有 `Multi` 脏判定会在执行后丢弃连接，不泄漏但客户端看到 OK。多语句整体
Reject 是更彻底的修法，属于路由策略变更（WantSlave 已保守路由 master），另行立项。

### SET GLOBAL 仍不拒绝不判脏
- status: wontfix
- file: pkg/sql/classify.go

全局变量无连接级隔离问题；策略限制（如拒绝写全局）应在产品层决定。

## Nit

### 白名单首版只有两个变量
- status: wontfix
- file: pkg/sql/classify.go

Gaea 白名单 13 个（sql_select_limit/max_execution_time/sql_safe_updates 等）。扩白
名单的每一项都是保真承诺，按需增量（每项补差分锚定），首版从最常用的
time_zone/transaction_isolation 起步。

## 路由与装配（route / plan / server）

### B1.0 单分片路由与 sharding 装配 review（原 `review-shard-router-b1.md`）

**files:**

- pkg/route/shard/router.go
- pkg/plan/plan.go
- pkg/config/config.go
- pkg/server/server.go
- pkg/proxy/session.go

## Must Fix

（无）

## Should Fix

### DDL / 事务 / prepared / EXPLAIN ROUTE 尚未实现
- status: wontfix
- file: pkg/route/shard/router.go

B1.0 只做文本协议的单分片 CRUD；DDL 永久 Reject（由外部迁移工具按片变更），
SHOW/事务控制/prepared 写当前 1235。DQL 优先补齐 `EXPLAIN ROUTE`、单分片
prepared SELECT、SHOW/USE/SET NAMES 连接兼容与分片读一致性；在此之前 sharding
模式只适合点查/点写验证。

### 无 FROM 的 SELECT 固定走默认片
- status: wontfix
- file: pkg/route/shard/router.go

常量 SELECT、`LAST_INSERT_ID()`/`ROW_COUNT()` 改写后的 SELECT 没有表，Router
用第一条规则的第一个 Slice 作为默认片。值本身是字面量，因此结果正确；但
`SELECT @@server_id`/`SELECT DATABASE()` 会暴露该片的值，与单库 oracle 可能不同。
P0 文档已标注只保证常量/会话函数。

### TxSingleShard 只是计划标记
- status: wontfix
- file: pkg/plan/plan.go

B1.0 的 Router 直接拒绝事务，`execute` 不会收到跨片事务；`TxSingleShard` 作为
B1.1 的契约先落位。不要在 B1.1 之前把它当成已强制执行的策略。

### hash_mod 的字符串哈希是 FNV-1a
- status: wontfix
- file: pkg/route/shard/router.go

数值键用取模，字符串/字节键用 FNV-1a 64 后取模。算法必须在数据分布与代理之间
保持一致；P0 文档已写明，换算法属于 breaking 变更。其他代理（kingshard 用
crc32）不要求兼容。

### schema 限定表被拒绝
- status: wontfix
- file: pkg/route/shard/router.go

`app.orders` 无法确定逻辑库/物理库映射，B1.0 直接 1235；使用当前会话库的
非限定表名。

## Nit

### 配置与 shard.New 双重校验
- status: wontfix
- file: pkg/route/shard/router.go

`config.Validate` 与 `shard.New` 都校验规则。Router 可能在非 YAML 装配路径
（测试/未来动态规则）被使用，保留防御式校验。

### 分片键类型严格化（hash_mod / hash_mod_str）review（原 `review-shard-key-types.md`）

**files:**

- pkg/route/shard/router.go
- pkg/constant/constant.go
- pkg/config/config.go
- test/harness/sharded.go
- test/sharding/sharding_test.go

本次变更把“算法”与“键值域”绑定：`hash_mod` 只接受整数键（含严格十进制
整数字符串），新增 `hash_mod_str` 只接受字符串/字节键。验证边界：变更包
聚焦单测全绿；`go vet`/golangci-lint v2.15.0 干净；2 Slice 真拓扑 e2e
（含新增字符串键表）通过。未跑全量差分/randgen，按 AGENTS 需用户显式批准。

## Must Fix

### 同一逻辑键因字面量加引号而路由到不同分片
- status: fixed
- file: pkg/route/shard/router.go

旧 `shardIndex` 按 Go 字面量类型分派：`k = 1` 走 `uint64 % N`，`k = '1'`
走 FNV-1a。INT 列下 MySQL 认为两者相等，但代理会把它们送到不同 slice。
SELECT 漏行；UPDATE/DELETE 静默影响 0 行；INSERT/查询混用引号时读写分片
不一致。这是“静默错误结果”，不是不支持。

修复：算法决定键值域。`hash_mod` 只接受整数；字符串字面量必须是严格
十进制整数（`'1'`、`'01'`、`'+1'`、`'-1'`，含 uint64 范围），解析后走
整数取模。`hash_mod_str` 只接受字符串/字节，数值字面量 1235。单测覆盖
`1` / `'1'` / `'01'` / `'+1'` / `'-1'` / 混合 `IN`；e2e 增加引号数字键回归。

### 一元负号键值无法路由
- status: fixed
- file: pkg/route/shard/router.go

`literalValue` 只识别 `ast.ValueExpr`，`-1` 解析为 `UnaryOperationExpr`：
SELECT 退化为 scatter，DML 直接 1235。修复：折叠 `+/-` 一元运算到整数字面量；
`-uint64`、decimal 等仍显式拒绝。单测覆盖 `-1` 与 `'-1'` 同片。

### 混合类型 IN 可能跨哈希域路由
- status: fixed
- file: pkg/route/shard/router.go

`IN (1, '2')` 旧实现会同时产生整数取模值与 FNV 值，可能命中错误 slice 或
错误地跨片。现在每个值都经同一算法的规范化：整数规则下两个值都转为整数；
字符串规则下 `1` 直接 1235。`slicesForValues` 与 `pickSlice` 共用
`shardIndex`，SELECT/DML/INSERT 行为一致。

## Should Fix

### 配置与 shard.New 只认识 hash_mod
- status: fixed
- file: pkg/config/config.go, pkg/constant/constant.go

`validateSharding` 与 `shard.New` 增加 `hash_mod_str`；配置单测覆盖合法配置，
非法算法仍拒绝。`shard.New` 同时保留空/重复 slice 名校验。

### 数字字符串的 MySQL 隐式转换未完全覆盖
- status: wontfix
- file: pkg/route/shard/router.go

`hash_mod` 只接受严格十进制整数；`' 1'`、`'1.0'`、`'1e0'` 会 1235，而
MySQL 会把它们当 1 比较。完整模拟需要列类型与 MySQL 转换规则，且这些形式
在分片键上极少见。当前选择显式拒绝，避免非单射转换导致错片；文档已注明。

### hash_mod_str 拒绝数值字面量
- status: wontfix
- file: pkg/route/shard/router.go

VARCHAR 列与数字比较时，MySQL 会把列值转成数字：`WHERE name = 1` 可匹配
`'1'`、`'01'`、`'1.0'` 等，无法靠单个字符串哈希安全收窄。接受 `1` 并转成
`'1'` 会静默漏掉其他等价字符串，因此选择 1235。若后续引入列类型元数据，
可再做严格等价类分析。

## Nit

### 不做 schema 反射，键类型由算法声明
- status: wontfix
- file: pkg/route/shard/router.go

代理不读取 `information_schema` 校验规则 `key` 的真实列类型；误配（字符串列
用 `hash_mod`）会在查询时以 1235 暴露，而不是启动期失败。列类型元数据属于
后续 schema 校验能力；当前 P0 以算法名作为唯一来源，避免启动期连库依赖。

### 字符串键按字节哈希，忽略 collation
- status: wontfix
- file: pkg/route/shard/router.go

`hash_mod_str` 使用 UTF-8 字节的 FNV-1a；大小写不敏感 collation 下
`'Alice'` 与 `'alice'` 在 MySQL 中相等，但代理视为不同键。P0 要求应用使用
二进制/区分大小写的键语义（或统一大小写写入）；collation-aware 分片需要
字符集/排序规则元数据，列入后续。

### EXPLAIN ROUTE 与 DQL 路由可观测 review（原 `review-explain-route.md`）

**files:**

- pkg/sql/explain_route.go
- pkg/execute/pipeline.go
- pkg/proxy/session.go

## Must Fix

（无）

## Should Fix

### 路由决策日志尚未落地
- status: wontfix
- file: pkg/execute/pipeline.go

D1 先做 `EXPLAIN ROUTE` 结果集；结构化路由日志/metrics 属于后续观测项，避免在
B1 阶段引入日志字段与采样策略。

### 合成结果集仍硬编码 DEPRECATE_EOF
- status: wontfix
- file: pkg/execute/pipeline.go

`writeTextResult` 按前端握手当前能力写 OK-as-EOF；与 raw forward 一致。后续做
per-session capability 时统一替换。

### EXPLAIN ROUTE 只显示首个/全部 target，不做代价估算
- status: wontfix
- file: pkg/execute/pipeline.go

输出 Kind/Slice/Role/DB/SQL/Reject reason，不做行数/代价估算；P0 目标是
可解释路由，不是优化器。

## Nit

### Error 行与 Reject 行共用 reason 列
- status: wontfix
- file: pkg/execute/pipeline.go

客户端可按 kind 区分；保持 6 列固定形状便于驱动解析。

### 分片配置模型与多 Slice 装配 review（原 `review-sharding-config-assembly.md`）

**files:**

- pkg/config/config.go
- pkg/config/runtime.go
- pkg/constant/constant.go
- pkg/server/server.go

## Must Fix

（无）

## Should Fix

### reload 时 mode 变化被 sharding "not implemented" 掩盖
- status: fixed
- file: pkg/config/runtime.go

`Apply` 原来先 `Validate(next)`；`mode=sharding` 的合法配置会先返回
"not implemented"，导致 rwsplit→sharding 看不到明确的 mode 变更错误。
现在对**已知模式**先比较 `backend.mode`，再校验其余配置；同时保留
`SameTopology` 作为第二道防线。补充 single→rwsplit、rwsplit→sharding、
sharding→single 三组测试。

### 合法的 sharding 配置仍不可用
- status: wontfix
- file: pkg/config/config.go

A1 只落模型与校验，`mode=sharding` 在校验通过后仍返回 "not implemented"。
这是有意的阶段门禁：执行层还没有 N 目标 dispatch，提前打开会得到半成品。
B1 完成后再移除该返回。

### 每个 Slice 不能覆盖 pool / load_balance
- status: wontfix
- file: pkg/server/server.go

P0 沿用全局 `pool` 配置与顶层 `load_balance`，`buildCluster` 对每个 Slice
应用同一套参数。per-slice 覆盖会扩大配置面；等真实容量差异出现后再加。

## Nit

### single 模式也设置了 LoadBalance
- status: wontfix
- file: pkg/server/server.go

`buildCluster` 对单 Slice 也写 `round_robin`，但 single 没有 slaves，字段不生效。
统一赋值让装配少一个分支，代价可忽略。

## 协议与池（mysql / backend）

### 连接池归还生命周期（不再无条件 Reset）review（原 `review-pool-lifecycle.md`）

**files:**

- pkg/backend/pool.go
- pkg/backend/stream.go
- pkg/backend/decode.go
- pkg/execute/pipeline.go
- pkg/execute/dispatch.go
- pkg/sql/scanner.go

## Must Fix

### 每次归还都发 COM_RESET_CONNECTION
- status: fixed
- file: pkg/backend/pool.go

原实现 `Release` 无条件 `b.Reset(ctx)`：每查询固定多 1 RTT，且 `COM_RESET_CONNECTION`
会把会话字符集重置回服务端默认（D5 oracle 已实测：连接从 45 变为 255），使握手阶段的
字符集固定失效。

修复：`Release` 只负责把「可复用」连接放回 idle；不可复用的连接由调用方走 `Discard`。
判定规则：

| 条件 | 动作 |
|---|---|
| `Drained()==false`（结果集未读完） | Discard |
| Exec/Align/Forward 出错 | Discard |
| 脏语句（用户变量写、非 autocommit 的 SET SESSION、临时表、锁、多语句、Exec 注释、扫描失败） | Discard |
| 归还时会话仍 sticky（事务 / autocommit=0 / reserved / stmt pin） | Discard |
| 以上都不成立 | 归还，不发 Reset |

对照：`database/sql` + `go-sql-driver` 的 `ResetSession` 只做存活检查；kingshard 不做
任何清理；Gaea 做定向清理（rollback + autocommit）并在取用时重放会话变量；
pgxpool 用 `AfterRelease` 钩子返回 bool 决定复用或销毁；HikariCP 用 dirty bits 只恢复
自己跟踪的状态。没有实现每次全量 reset 的。

### 预编译 SQL 的 `?` 占位符无法扫描
- status: fixed
- file: pkg/sql/scanner.go

`Scan` 把 `?` 视为非法字节，导致预处理语句的 SQL 文本触发 `ScanFailed`。后果：
`dirtyForReuse` 将其判为脏 → 连接被丢弃（本次测试暴露），并且若其他路径读取
`Class.WantSlave/Reserved` 也会得到保守但错误的结果。

修复：`?` 归入 `TokPunct`，新增扫描用例。

### dispatch 并发调用 EnsureClass 的数据竞争
- status: fixed
- file: pkg/execute/dispatch.go

`execOnLease` 在每个分片 goroutine 中调用 `stmt.EnsureClass()`，首次调用会写
`Statement.Class/classReady`，`-race` 报 DATA RACE。

修复：`dispatch` 在启动分片 goroutine 之前先调用一次 `stmt.EnsureClass()`（生产路径
`plan.Build` 已计算过，这里是防御性保证）；此后各 goroutine 只读。

## Should Fix

### 会话变量白名单（B2）未落地
- status: wontfix
- file: pkg/execute/pipeline.go

白名单外的 `SET SESSION` 目前是「执行后丢弃连接」，而不是返回 1235；`time_zone` 也尚未
进入 `Align` 重放。本改动已保证**不会泄漏**（脏连接一律丢弃），拒绝与重放属于独立的
后续小步，需要扩展 `AlignSettings` 与差分语料。

### `Conn.Reset` 现在未被连接池使用
- status: wontfix
- file: pkg/backend/conn.go

保留该方法：`conn_test.go` 覆盖其协议行为，后续若需要显式重置（如运维命令）仍可复用；
删除会波及 `Conn` 接口与全部测试替身，收益仅为减少一个未调用的导出方法。

### `SET GLOBAL` 被判定为可复用
- status: wontfix
- file: pkg/execute/pipeline.go

全局变量修改不影响连接级隔离（不会跨会话泄漏），因此不触发 Discard。若后续要对全局
写入做策略限制，应在 `Class.UnsupportedReason` 层处理，而不是池生命周期。

## Nit

### `Drained()` 仅在转发后有意义
- status: wontfix
- file: pkg/backend/stream.go、pkg/backend/decode.go

`rawStream.Drained()` 在 `ForwardTo` 之前恒为 false，语义正确（未消费）；已在方法注释
说明。`noReplyStream` 恒为 true。

### 非生产流默认视为已排空
- status: wontfix
- file: pkg/execute/pipeline.go

`streamDrained` 对未实现 `Drained()` 的流返回 true，避免测试替身与合成流被迫实现该
接口；生产流的 `rawStream`/`decodedStream` 均实现了追踪。

### `Pool.Release` 仍返回 error 但恒为 nil
- status: wontfix
- file: pkg/backend/pool.go

保留签名以兼容现有调用点与测试；后续如引入「释放失败」语义（例如强制 reset 模式）可
直接使用该返回值。

### 统一 errs 包 review（原 `review-errors-package.md`）

**files:**

- pkg/errs/errs.go
- pkg/mysql/result.go
- pkg/mysql/decode.go
- pkg/mysql/packet/packet.go
- pkg/mysql/server/conn.go
- pkg/plan/plan.go
- pkg/route/shard/router.go
- pkg/execute/pipeline.go
- pkg/execute/dispatch.go
- pkg/proxy/session.go
- pkg/backend/pool.go
- pkg/backend/decode.go

本次重构把跨层错误收敛到 `pkg/errs`：一个 MySQL ERR 值类型、常用 errno/SQLSTATE
常量与构造器、三个跨包哨兵错误，以及统一的 `mysql.WriteERR`。验证边界：受影响
包聚焦单测全绿；`go vet`/golangci-lint v2.15.0 干净；2 Slice 真拓扑 e2e
（包含 1235 拒绝、握手、认证路径）通过。未跑全量差分/randgen，按 AGENTS 需
用户显式批准。

## Must Fix

（无）

## Should Fix

### 合成错误与后端解码错误是两套相同结构
- status: fixed
- file: pkg/plan/plan.go, pkg/mysql/decode.go

`plan.RejectError{Code,State,Message}` 与 `mysql.ServerError{Number,SQLState,
Message}` 表达同一件事，但类型不同：`proxy.handleExecError` 只认前者，后端
解码错误需要单独分支；任何新层都可能再发明一套。修复：`errs.MySQL` 作为唯一
值类型，`Plan.Reject`、`DecodeServerError`、proxy、router、execute 与测试全部
直接使用 `*errs.MySQL`，不再保留中间类型/别名；字段统一为
`Number/SQLState/Message`。`errors.As(err, *errs.MySQL)` 现在对合成错误和
解码错误都成立。

### proxy/server 各写一份 writeERR
- status: fixed
- file: pkg/mysql/result.go, pkg/proxy/session.go, pkg/mysql/server/conn.go

两处都把 `EncodeERR` + 4 字节 header 再复制一遍，容易在 packet 上限/sequence
处理上漂移。修复：`mysql.WriteERR(front, *errs.MySQL)` 统一负责 framing；
proxy/server 删除本地 helper。新增 `TestWriteERR`/`TestWriteERR_NilArgs`
覆盖正常、nil front、nil error 分支。

### 哨兵错误定义分散
- status: fixed
- file: pkg/backend/pool.go, pkg/backend/decode.go, pkg/mysql/packet/packet.go

`ErrPoolExhausted`、`ErrNoResultSet`、`ErrPacketTooLarge` 原先各包自行
`errors.New`。定义收口到 `errs`；原位置的 `backend.ErrPoolExhausted`、
`backend.ErrNoResultSet`、`packet.ErrPacketTooLarge`、`execute.ErrPoolExhausted`
别名已全部删除，所有调用点直接使用 `errs.*`，避免同一个错误值存在两个名字。

### 常用 MySQL 错误码/文案散落
- status: fixed
- file: pkg/errs/errs.go, pkg/proxy/session.go, pkg/mysql/server/conn.go

1153/1047/1040/1043/1045/1235/1105 及 SQLSTATE 现在由 `errs` 常量与构造器
提供（`Unsupported`、`UnknownCommand`、`TooManyConnections`、`PacketTooLarge`、
`BadHandshake`、`AccessDenied`、`Unknown`）。proxy 不再硬编码 errno/SQLSTATE；
plan/router/execute 的 1235 统一走 `errs.Unsupported`。

## Nit

### 包内诊断错误仍留在原包
- status: wontfix
- file: pkg/backend, pkg/mysql/packet, pkg/mysql/auth

握手截断、length-encoded 非法、池关闭等错误只在包内产生和判断，跨包没有
`errors.Is` 需求。把它们搬进 `errs` 会形成 god package，也失去就近的上下文。
`errs` 只收口“需要跨层识别或写给客户端”的错误，边界已写入包注释与
`module-map.md`。

### 兼容别名已全部移除
- status: fixed
- file: pkg/backend, pkg/mysql/packet, pkg/plan, pkg/mysql/decode.go

初版为了减少调用点改动，保留了 `backend.ErrPoolExhausted` 等变量 alias 与
`plan.RejectError`/`mysql.ServerError` 类型 alias。alias 本身就是重复命名，
容易让新代码继续使用旧入口。现已全部删除：哨兵直接引用 `errs.Err*`，
`Plan.Reject` 与 `DecodeServerError` 直接产出 `*errs.MySQL`。

## 测试与重构基建（test / refactor）

### 分片聚合 single-DB oracle review（原 `review-aggregate-metamorphic.md`）

**files:**

- test/metamorphic/aggregate_test.go
- test/harness/sharded.go
- test/harness/topology.go
- test/sharding/replica_test.go

本次新增“分片聚合 vs 单库”独立 oracle，并保留/整理真实复制拓扑验证。
验证边界：`test/metamorphic`（Docker，约 26s）、`test/sharding` 两套 e2e
（约 69s）通过；完整 `make test-unit` 全绿；golangci-lint 0 issues。

## Must Fix

### 聚合正确性只靠手写期望值
- status: fixed
- file: test/metamorphic/aggregate_test.go

原有 e2e 用少量已知数据断言 `COUNT/SUM/MIN/MAX` 的固定值，没有独立 oracle；
如果手算期望或 schema 理解错了，测试会和实现一起错。修复：2 个 master-only
slice + 1 个独立 MySQL oracle，同一份 24 行数据（含 NULL、负数）按
`user_id % 2` 分布到分片，oracle 保存全量；逐条比较聚合查询的列名与值。
比较时 NULL 单独判断，数值用 big.Rat 归一化（COUNT/SUM 的文本 scale 差异）。

### 真实复制拓扑没有读从/写主验证
- status: fixed
- file: test/harness/sharded.go, test/sharding/replica_test.go

`StartShardedTopology` 只有 master；新增
`StartShardedTopologyWithReplicas(ctx, slices, replicas)`，每片独立 GTID
复制、从库 read_only。`TestShardedReplicas` 验证 `SELECT @@server_id` 命中
s0 replica、DDL/数据复制、写主读从、写后 `Sync` 再读、聚合/ORDER/LIMIT/
prepared 和错误拒绝。

## Should Fix

### metamorphic 只覆盖聚合
- status: wontfix
- file: test/metamorphic/aggregate_test.go

ORDER BY/LIMIT OFFSET 仍由固定 fixture e2e 验证；下一步应加同一 oracle 下的
排序/分页查询，以及随机数据生成（含重复键、边界值、不同基数）。

### 数据量为固定 24 行
- status: wontfix
- file: test/metamorphic/aggregate_test.go

固定数据便于复现，但覆盖不了大基数/倾斜分布。后续接 `test/randgen` 或仓内
生成器，种子化扩大数据量；聚合结果仍与单库 oracle 对比。

### 复制拓扑测试成本
- status: wontfix
- file: test/sharding/replica_test.go

4 个容器约 54s；master-only 快速 e2e 保留。CI 分层：PR 跑 master-only，
merge/nightly 跑复制拓扑。

## Nit

### oracle 用单实例分片 harness
- status: wontfix
- file: test/metamorphic/aggregate_test.go

`StartShardedTopology(ctx, 1)` 起一个单实例作为 oracle，复用了容器启动/清理
逻辑；它同时携带未使用的 sharding 规则，但不影响查询。若后续 oracle 需求
增多，再抽 `StartOracle`。

### 数值比较归一化
- status: wontfix
- file: test/metamorphic/aggregate_test.go

`sameCell` 对可解析数值用 big.Rat 比较，允许 `250` 与 `250.0000` 等价；
字符串/列名仍要求完全一致。这是刻意避免 DECIMAL 文本 scale 差异造成假失败。

### 随机 SQL 差分（randgen）review（原 `review-randgen-differential.md`）

**files:**

- test/randgen/gen.go
- test/rwsplit/random_test.go
- test/rwsplit/rwsplit_test.go

## Must Fix

### comparison 使用全局 rand，种子不可复现
- status: fixed
- file: test/randgen/gen.go

初版 `comparison()` 用包级 `rand.Intn`，同一 seed 的两个 `Generator` 会生成不同
比较符，随机差分不可复现。改为 `(*Generator).comparison()` 使用实例的
`g.rnd`，并用 `TestSelectDeterministic` 固定 50 条序列。

### 全表达式投影时 LIMIT 可能选到不确定行集
- status: fixed
- file: test/randgen/gen.go

当投影全部是算术表达式时 `orderable` 为空，原实现不生成 ORDER BY，但仍有 1/3
概率生成 LIMIT；主库与从库可能返回不同子集，造成差分抖动。改为始终在
ORDER BY 末尾追加唯一的 `t.id`，保证 LIMIT 行集确定。

## Should Fix

### 错误比较依赖具体错误包装
- status: fixed
- file: test/rwsplit/random_test.go

初版直接 `reflect.DeepEqual(queryObs)`，会连 `error` 接口的具体实现一起比较；
不同包装类型即使 errno/message 相同也可能判定不等。新增 `queryObs.equal`：
错误只比 errno + message，成功只比列与行。

## Nit

### 行结果排序后比较，不验证 ORDER BY 顺序
- status: wontfix
- file: test/rwsplit/random_test.go

本轮目标是读路径转发一致性。ORDER BY 的排序结果属于查询计划行为，
更适合后续的 plan/differential 层；且排序比较可避免同值并列导致的抖动。
注释已说明。

### 生成器只生成 SELECT
- status: wontfix
- file: test/randgen/gen.go

随机 DML 需要事务回滚/重建数据与复制追平，先以只读差分建立基线；
写路径的随机化在 rwsplit 一致性窗口（read_after_write）落地后再加。

### rwsplit 真实拓扑测试基建 review（原 `review-rwsplit-topology-harness.md`）

**files:**

- pkg/session/state.go
- test/harness/topology.go
- test/harness/proxy.go
- test/rwsplit/rwsplit_test.go

## Must Fix

### DetectReservedSQL 把 @@system_var 误判为用户变量
- status: fixed
- file: pkg/session/state.go

`hasUserVariableAssign` 逐字符扫描 `@`，`@@server_id` 的第二个 `@` 后跟字母，
被当作 `@user_var` 命中，导致 `SELECT @@server_id` 在进入路由前就被标记
reserved、pin 到主库。真实拓扑用例 `plain_select_routes_to_replica` 首次复现：
期望从库 server_id ∈ {2,3}，实际返回主库 1。

修复：遇到 `@@` 成对跳过，只把单个 `@` + 标识符视为用户变量。补表驱动用例：
`SELECT @@server_id`、`SELECT @@GLOBAL.max_connections`、
`SET @@SESSION.sql_mode` 均为 false；`SELECT @probe` 仍为 true。

## Should Fix

### adminExec 错误信息包含复制密码
- status: fixed
- file: test/harness/topology.go

`CREATE USER ... IDENTIFIED BY 'repl-pass-123'` 失败时原样拼进 error，CI 失败日志
会泄漏测试密码。改为只报告语句序号，不输出 SQL 文本。

### 复制状态查询瞬时失败被当作终态
- status: fixed
- file: test/harness/topology.go

`START REPLICA` 后立即查 `SHOW REPLICA STATUS` 可能短暂无行；原来直接返回
`replication not configured`，属于启动竞态。改为在 60s 期限内重试，超时错误里
带最后一次 status 错误。

### 启动失败时清理没有超时
- status: fixed
- file: test/harness/topology.go

`StartTopology` 失败路径用 `context.Background()` 清理，Docker 异常时可能挂死。
改为 2 分钟超时上下文。

## Nit

### StartProxy 忽略 Run 返回值
- status: wontfix
- file: test/harness/proxy.go

`Run` 只在 listener 关闭后返回 nil；测试进程内没有需要上报的后台错误。
若后续需要捕获 accept 异常，再引入 error channel。

### 拓扑固定 mysql_native_password
- status: wontfix
- file: test/harness/topology.go

代理当前只实现 native password 认证，拓扑 cnf 固定
`default-authentication-plugin=mysql_native_password` 与现有 passthrough 测试一致。
caching_sha2 全流程支持后再扩展认证矩阵。

### 固定 10s 连接/40s 读写超时
- status: wontfix
- file: test/harness/topology.go

测试拓扑在本机/CI 容器内，固定超时足够；真故障注入（延迟/半包）应由独立的
TCP fault proxy 控制，而不是靠放大超时。

### 分片复制拓扑 harness 与真实能力验证 review（原 `review-sharded-replica-harness.md`）

**files:**

- test/harness/sharded.go
- test/harness/topology.go
- test/harness/sharded_test.go
- test/sharding/replica_test.go

本次把分片 e2e 从 master-only 扩展到“每片 master + GTID 从库”的真实拓扑，
并新增 `TestShardedReplicas` 验证已实现能力。验证边界：完整 `make test-unit`
全绿；`./test/sharding/`（master-only + 复制拓扑两套，约 69s）通过；
golangci-lint 0 issues。

## Must Fix

### 分片拓扑只有 master，读从/复制行为没有被真实覆盖
- status: fixed
- file: test/harness/sharded.go

`StartShardedTopology` 只起 N 个独立 master，sharding 下的 `RoleSlave` 实际
总是回落到 master，无法验证读从、复制延迟和误写从库。修复：
`StartShardedTopologyWithReplicas(ctx, slices, replicas)` 在同一个 Docker
network 内为每个 slice 起 master + GTID 从库，从库追平后 `read_only=ON`；
`ProxyConfig` 为每个 slice 填 `slaves`；`ShardedTopology.Sync` 等待所有分片
复制追平。

### 新增测试里沿用了已过期的 LIMIT OFFSET 拒绝断言
- status: fixed
- file: test/sharding/replica_test.go

D5.1b 已支持 `LIMIT 1,1`，初版复制拓扑测试仍断言 1235。已改为断言返回 1 行，
避免测试把已实现能力误判为不支持。

## Should Fix

### 复制拓扑测试耗时
- status: wontfix
- file: test/sharding/replica_test.go

2 slice × (1 master + 1 replica) 共 4 个容器，单测约 54s；master-only e2e
仍保留给快速回归。后续可把两套测试共享拓扑或按 CI 分层（PR 跑 master-only，
merge/nightly 跑复制拓扑）。

### 复制同步需要测试显式调用
- status: wontfix
- file: test/harness/sharded.go

生产是异步复制，代理不做 read-after-write；测试在写后调用 `tp.Sync` 才读从库，
这是刻意的确定性处理。后续做 D4.1 从库健康/延迟时，再补“延迟从库/停止复制”
的故障用例。

### 没有从库故障/延迟场景
- status: wontfix
- file: test/sharding/replica_test.go

当前只验证正常复制链路；`STOP REPLICA`、杀从库、延迟从库属于 D4.1/R3 的
健康探测测试，等池内探测实现后再加。

## Nit

### server-id 分配规则
- status: wontfix
- file: test/harness/sharded.go

每片 server-id 按 `slice*(replicas+1)+1` 递增；够用且可预测，但测试容器
拓扑变化时需要保持唯一。已由 harness 内部集中计算，调用方无需关心。

### 容器别名与实例名
- status: wontfix
- file: test/harness/sharded.go

网络别名 `mysql-s0-master`/`mysql-s0-replica-1` 与实例名 `s0`/
`s0-replica-1` 并存，分别服务于复制源解析和测试可读性；不合并。

### 存量架构清理 review（原 `review-architecture-cleanup.md`）

**files:**

- pkg/execute/pipeline.go
- pkg/execute/dispatch.go
- pkg/mysql/result.go
- pkg/mysql/server/conn.go
- pkg/proxy/serve.go
- pkg/result/type.go
- pkg/merge/ordered.go
- pkg/merge/aggregate.go
- pkg/plan/plan.go
- pkg/sql/reserved.go
- pkg/session/state.go

本次是行为不变的结构清理：收掉 execute 的重复打开逻辑、统一 MySQL 写包
helper、集中列类型常量、把 reserved 检测移到 sql、显式化 ORDER BY key 类型，
并把 `proxy/session.go` 改名为 `serve.go`。验证边界：完整 `make test-unit`
全绿；2 Slice 真拓扑 e2e 通过（17.4s）；golangci-lint 0 issues。

## Must Fix

### runOne 与 openDecoded 各自实现 acquire/align/exec
- status: fixed
- file: pkg/execute/pipeline.go, pkg/execute/dispatch.go

两条执行路径重复 `cluster.Get → Slice.Acquire → Align → payloadFor → Exec`，
以后加查询超时、健康检查、重试时很容易只改一处。修复：抽出
`acquireLease` / `execTarget`，runOne 与 dispatch 共用；Align 失败时仅释放
本次新 acquire 的 lease，保持原语义。现有 runOne/dispatch 测试覆盖。

### 写 OK/列头/EOF 的 framing 重复
- status: fixed
- file: pkg/mysql/result.go, pkg/proxy/serve.go, pkg/mysql/server/conn.go, pkg/execute

proxy 与 server 各有一份 `writeOK`；execute 的 explain 和 dispatch 各拼一次
column count/definitions/EOF。修复：`mysql.WriteOK`、`WriteResultHeader`、
`WriteEOF` 统一 framing，proxy/server/execute 复用；`WriteERR` 也改用同一
`writePayload`。新增三个 helper 的单测（正常 + nil front）。

### MySQL 列类型码在 merge 内重复硬编码
- status: fixed
- file: pkg/result/type.go, pkg/merge/ordered.go, pkg/merge/aggregate.go

`0x01/0x03/0xf6/0x20...` 同时存在于 merge 与测试，且 `pkg/mysql` 没有对应
常量。修复：`pkg/result` 增加 `Type*` 与 `FlagUnsigned`（Field.Type 的 wire
值），merge 与测试统一引用，避免协议知识继续分叉。

## Should Fix

### reserved 检测放在 session 包
- status: fixed
- file: pkg/sql/reserved.go, pkg/session/state.go, pkg/proxy/serve.go

`DetectReservedSQL` 只被 proxy 使用，本质是 SQL 文本分类而非会话状态。
已迁到 `pkg/sql`，测试同步移动；session 包只保留 State/StmtTable，
后续 R0 `StatementClass` 可直接替换该启发式。

### ORDER BY key 依赖 `Column=-1` 约定
- status: fixed
- file: pkg/plan/plan.go, pkg/merge/ordered.go

原先用负数列下标表示“按名字解析”，隐式且易错。新增 `OrderKey.Kind`
（`OrderByPosition`/`OrderByName`），merge 按 kind 解析；位置键的零值语义
保持不变。

### proxy 文件名与 session 包混淆
- status: fixed
- file: pkg/proxy/serve.go

`pkg/proxy/session.go` 实际是 COM `Serve` 循环，和 `pkg/session` 容易混淆；
已重命名为 `serve.go`，设计文档中的路径同步更新。

### e2e 单体测试过大
- status: wontfix
- file: test/sharding/sharding_test.go

`TestSingleShardRouting` 已有 ~400 行，覆盖路由/order/limit/aggregate/错误。
拆分需要共享同一拓扑的 subtest 结构，属于测试可读性优化，不影响行为；
当前先保留，避免与本次结构清理混在一起。

### plan.go 仍混合 Plan/MergeSpec/聚合解析
- status: wontfix
- file: pkg/plan/plan.go

329 行仍在可读范围；拆分文件会触发测试文件命名迁移（`merge_spec_test.go`），
收益低于本次 execute/mysql 的重复消除。后续加 AVG/GROUP BY 时再拆。

## Nit

### explain 与 dispatch 的列转换各有一小段
- status: wontfix
- file: pkg/execute/pipeline.go, pkg/execute/dispatch.go

explain 的列是字符串（VAR_STRING），dispatch 是 `result.Field`，列元数据来源
不同，强行共用会引入不必要的中间类型；保留两处小转换。

### `DetectReservedSQL` 仍是子串启发式
- status: wontfix
- file: pkg/sql/reserved.go

用户变量/临时表/命名锁的漏判仍是已知问题；R0/R1 的 StatementClass 落地时
替换，当前只做了位置收敛。

### 依赖图未画全
- status: wontfix
- file: docs/design/module-map.md

4.1 的图只保留主路径，`route/source`、`route/shard`、`session` 的边未全部
展开；模块表和禁止依赖表才是准绳。
