# 分片能力评估与 P0 设计（sharding-p0-design）

> 状态：当前分片设计与实现状态（2026-10-31）
> 范围：`backend.mode=sharding` 的能力盘点、模块拆分、分阶段落地与已实现边界。
> 关联：[architecture.md](../architecture.md)、[module-map.md](module-map.md)、
> [merge-engine-design.md](merge-engine-design.md)、[test-infrastructure.md](test-infrastructure.md)
> 提交提示：`docs/design/` 被 `.gitignore` 忽略；入仓需 `git add -f`。

## 0. 结论

1. **不需要三 Executor。** 现有单管线（`Plan → Targets → execute`）可以承载分片；
   分片差异放在 Router / Plan / Rewrite / Merge，执行层只增加"多目标 dispatch"。
2. **当前最大缺口不是路由，而是结果层。**
   - `execute.Handle` 仍只接受 1 个 Target（`pipeline.go` 的
     `len(pl.Targets) != 1`）；
   - `result.Stream` 只有 raw 转发，`backend.rawStream.Next()` 返回 `io.EOF`；
   - `pkg/mysql` 只有 OK/ERR 编码，没有结果集列定义/行/EOF 编码。
   没有这三块，多分片 SELECT 无法归并、也无法写回前端。
3. **P0 只做"可证明单分片"**：单分片 SELECT/DML/DDL、单分片事务、
   `EXPLAIN ROUTE`；无分片键/跨片/跨片事务一律 Reject。多分片只读归并放 P1。
4. **先做 S0 执行/结果骨架（行为不变），再打开 `mode=sharding`。**
   直接在 `Handle` 里堆分片分支会重演 kingshard 的会话 god object。
5. 需要先拍板的四件事：分片粒度与命名、ID 生成、P0 是否含多分片读、
   prepared 与 DDL 的策略。

## 1. 当前架构复盘（rwsplit 加固之后）

### 1.1 已经具备的分片基础

| 能力 | 现状 | 对分片的价值 |
|---|---|---|
| 目标集合 | `route.Target{Slice, Role, DB, SQL}` | 已经是"多目标"形状，`SQL` 预留改写结果 |
| 纯路由 | `route.Router` 不持有池 | 可分片规则表驱动单测、可 Explain |
| 拓扑 | `backend.Slice`/`Cluster` 支持多 Slice | 每片 master+slave 现成 |
| 租约 | `execute.Runtime.leases map[slice]*lease` | 已按 slice 管理，Release 会遍历全部 |
| 计划 | `plan.Plan{Kind, Targets, Merge, Reject}` | 三态契约已定；`MergeSpec` 是空壳 |
| OK 元数据 | `mysql.ForwardResponseInfo` 解析 affected/lastInsertID/warnings/resultSet | 多片写可汇总 affected；会话函数可维护 |
| 会话 | `session.State` 有 lastInsertID/rowCount/unsupported 拒绝 | 逻辑会话状态可扩展到分片 |
| 测试 | 真实主从 harness、SQL 矩阵、随机差分 | 可扩展成多 Slice + 单库 oracle |

### 1.2 结构性缺口（按影响排序）

| # | 缺口 | 代码位置 | 影响 |
|---|---|---|---|
| 1 | 单目标假设 | `pkg/execute/pipeline.go`：`len(pl.Targets) != 1`、`Targets[0]` | 无法执行 N 个目标 |
| 2 | raw-only 结果 | `pkg/backend/stream.go`：`rawStream.Next()` 返回 `io.EOF` | 无法把后端结果解码成 `result.Row` |
| 3 | 无结果集编码 | `pkg/mysql/result.go` 只有 `EncodeOK/EncodeERR` | 归并后的列/行无法写回前端 |
| 4 | 无分片元数据/规则 | `config.ShardingConfig` 只有 table/key/algorithm/slices；`Validate` 拒绝 sharding | 无法把 SQL 映射到 slice |
| 5 | AST 未上数据面 | `sql.Statement.AST` 为空；`pkg/sql/parser` 仅单测 | 无法可靠提取分片键、改写、归并 |
| 6 | 无 rewrite | `Target.SQL` 从未与原文不同 | 分片键注入、LIMIT/聚合改写无从下手 |
| 7 | 无 StmtTable | `proxy` 对 `COM_STMT_PREPARE` 仅 `MarkReserved` 后转发 | 多分片 prepared 无法映射后端 stmt id |
| 8 | 无事务策略 | `session.State` 只有 `inTx/autocommit/reserved` | 跨片事务语义未定义 |
| 9 | 无全局 ID / DDL / 全局表 | 配置与计划均无 | 多片 INSERT、DDL 广播、维度表无策略 |
| 10 | 单库 oracle / 数据生成 | 分片 e2e 已有 2 Slice + 复制拓扑；聚合 metamorphic 已接单库 oracle | 随机数据、ORDER/LIMIT metamorphic 仍缺 |

### 1.3 不能破坏的现有约束

- **N=1 零解码**：single/rwsplit/单分片继续走 raw packet；只有 `MultiShard` 解码。
- **lease 归 execute**：route/plan 不持有池；session 只存意图。
- **mode 只在装配层**：execute/proxy/plan/route/merge 不出现 `ModeSharding`。
- **会话函数**：`LAST_INSERT_ID()`/`ROW_COUNT()` 已按 session state + 改写实现；
  分片下要保持语义（见 §7.8）。
- **显式不支持**：`SHOW WARNINGS`、复制类 SHOW、`SET sql_mode` 等继续 1235，
  不因为分片而改变。

## 2. 分片需要的能力清单

按"必须 / 可分阶段"标注；P0 指打开 `mode=sharding` 的第一版。

### 2.1 元数据与规则

| 能力 | P0 | 说明 |
|---|---|---|
| 逻辑表 → 分片键、算法、Slice 列表 | 是 | 静态 YAML；规则变更需重启 |
| 分片键列名/值域 | 部分 | Key 必须配置；值域由算法定义（`hash_mod` 整数 / `hash_mod_str` 字符串），不做 schema 反射 |
| 全局/广播表 | 否 | P1；DDL/DML 广播、读任选一片 |
| 默认片 / 未配置表策略 | 否 | P0 直接 Reject，避免猜错 |
| 物理库/表命名 | 是 | P0 推荐同库同名，`Target.DB` 预留物理库 |
| 规则与 Slice 引用一致性校验 | 是 | 未知 Slice、重复规则直接拒绝 |

### 2.2 SQL 理解与路由

| 能力 | P0 | 说明 |
|---|---|---|
| AST 解析（COM_QUERY） | 是 | TiDB parser 已在仓库；`Statement.AST` 落地 |
| 分片键等值路由（`=`/`<=>`） | 是 | 单分片 |
| 单分片 `IN`（值同片） | 是 | 多值同片才算单分片 |
| 范围/`BETWEEN` | 否 | 默认 Reject；P1 再做分片裁剪 |
| 无分片键 / 非等值 | 是（Reject） | 明确错误，不猜默认片 |
| 多表/JOIN | 是（Reject） | P0 不做跨片 JOIN；同片 JOIN 可后续放开 |
| 读写角色 | 是 | 每个 Slice 内仍由 `source.Pick` 选 master/slave |
| 广播 DDL | 是 | 所有 Slice 执行；失败语义见 §7.6 |
| `EXPLAIN ROUTE` | 是 | 打印 Kind/Targets/Reject 原因 |

### 2.3 改写

| 能力 | P0 | 说明 |
|---|---|---|
| 表名/库名改写 | 否 | 同库同名；`Target.DB` 只影响 `USE` |
| `INSERT` 分片键缺失检查 | 是 | 缺键或一行多片 → Reject |
| `LIMIT/OFFSET` 下推 | 是 | OFFSET=0 原样；OFFSET>0 时 per-shard 改写为 `offset+limit` |
| 聚合改写（`AVG`→`SUM/COUNT` 隐藏列） | 是 | 保留原输出列名/scale |
| `COUNT/SUM/MIN/MAX/AVG` 归并 | 是 | 无 GROUP BY |
| `ORDER BY`/`GROUP BY` 列补齐 | 否 | P1 |
| 分片键更新保护 | 是 | `UPDATE` 改分片键 → Reject |

### 2.4 执行与结果

| 能力 | P0 | 说明 |
|---|---|---|
| N 目标 dispatch | 是 | 顺序或并发；要保留 per-slice lease |
| 单分片 raw 快路径 | 是 | 不解码 |
| 多分片并发 + 超时 | 否 | P1 |
| 结果集解码（列/行/EOF） | 否 | P1；P0 单分片不需要 |
| 结果集编码（列/行/EOF/OK） | 否 | P1 |
| identity merge | 是 | `len(streams)==1` 直接返回 |
| aggregate/order/limit merge | 否 | P1 |
| 部分失败/超时处理 | 是（Reject/错误） | P0 单分片不存在部分成功；DDL 广播单独定义 |

### 2.5 会话、事务、Prepared

| 能力 | P0 | 说明 |
|---|---|---|
| 单分片事务（`BEGIN..COMMIT`） | 是 | 事务内所有语句必须同片；否则 Reject |
| 跨片事务 | 是（Reject） | P0 不做 2PC/Saga |
| `SAVEPOINT` | 是 | 单分片内透传 |
| 会话变量/临时表/锁 | 是（单分片 pin） | 与 rwsplit 的 reserved 逻辑一致 |
| prepared 单分片 | 是 | 需要 `StmtTable`；prepare 发到目标片 |
| prepared 多分片 | 是（Reject） | P1 再评估广播 prepare |
| `LAST_INSERT_ID` 单分片 | 是 | 沿用现有 OK 元数据 |
| `LAST_INSERT_ID` 多分片写 | 是（Reject） | 需要全局序列，P2 |
| `ROW_COUNT` 多分片写 | 是（Reject） | 多片写 P0 直接拒绝；P1 汇总 affected |
| `FOUND_ROWS` | 是（Reject） | 与单机决策一致 |

### 2.6 全局 ID / DDL / 运维

| 能力 | P0 | 说明 |
|---|---|---|
| 本地 auto_increment | 是 | 单分片 INSERT 可用；跨片不保证全局唯一 |
| 全局序列 / Snowflake | 否 | P2；多分片 INSERT 前必须解决 |
| DDL 广播 | 否 | 代理不广播；DDL 由外部迁移工具按片执行，proxy 显式 Reject |
| `SHOW TABLES`/`SHOW CREATE` 合并 | 否 | P1；P0 可拒绝或只查一片 |
| 在线扩缩容 | 否 | 非目标 |
| 分片健康/熔断/重试 | 否 | P1 |
| 路由决策日志/metrics | 否 | P1；`EXPLAIN ROUTE` P0 |

## 3. 开源对照：分片是怎么做的

| 项目 | 规则/路由 | 改写 | 归并 | 事务/ID | 结论 |
|---|---|---|---|---|---|
| **kingshard** | `router.Rule`：hash/range/date，`Plan` 记录 table index 与 `RewrittenSqls` | 按 table index 重写 `IN`/值 | 全缓冲 + 全排序；单分片也归并 | 按节点事务；无全局 ID | 规则模型值得借鉴，归并是反面教材 |
| **Gaea** | `SelectPlan` 提取 group/order/limit/aggregate；`MergeSelectResult` | 补列、limit 改写 | 全缓冲；有单节点快路径 | 按 slice 事务 | Plan 对象 + 单节点快路径值得借鉴 |
| **dbpack** | `QueryOnSingleDBPlan` vs 多片 Plan；`cond.ShardingAlgorithm` | LIMIT 下推、全局表 | k 路归并 | 复杂事务/全局表有探索 | 单分片独立 Plan；但三 Executor 复制 |
| **Vitess** | VSchema/vindex + planbuilder | 完整 AST 改写 | Primitive 树（Route/Aggregate/Order/Limit/Join） | 两阶段/预留连接 | 能力上限最高，但引擎太重 |
| **ShardingSphere** | 规则插件 + `SQLRouteEngine` | Rewrite 阶段 | Merge 阶段 | 本地/2PC/Saga | 五阶段流水线最适合作为架构参照 |

### 3.1 可复用的结论

1. **静态规则 + AST 足够做 P0/P1**（kingshard/Gaea/dbpack 都是这样）。
2. **单分片必须有独立快路径**，不能走全量归并（Gaea `isExecOnSingleNode`、
   dbpack `QueryOnSingleDBPlan`）。
3. **归并先全缓冲**是行业常态；流式/k 路堆是优化，不是 P0。
4. **跨片事务、全局 ID、DDL 广播是产品问题**，不是 Router 细节；必须在设计里
   明确支持/拒绝，不能留给执行层猜。
5. **Primitive 树（Vitess）只在跨片 JOIN/复杂聚合时才有收益**；当前扁平
   `Targets + MergeSpec` 足够，保留演进空间。

## 4. 可行形态对比

| 方案 | 形态 | 优点 | 代价 | 结论 |
|---|---|---|---|---|
| **A. 单管线 + 五阶段** | Parse → Route → Rewrite → Execute → Merge，共用 `execute.Pipeline` | 与现有代码/文档一致；单分片 raw 快路径；可按阶段单测 | 需要补结果编解码/归并 | **推荐** |
| B. 三 Executor | Single/RWSplit/Sharding 各自执行 | 模式边界直观 | 管线复制、会话/租约逻辑重复（dbpack 已验证） | 拒绝 |
| C. Vitess Primitive 树 | Plan 是算子树 | 跨片 JOIN/复杂 SQL 上限高 | 引擎重、学习/维护成本高 | 后置演进 |
| D. kingshard 式全缓冲 | 所有 SELECT 都归并 | 实现快 | 单分片性能差、内存不可控 | 只作为 P1 归并实现 |
| E. 代理层规则引擎 | 只在 proxy 里按规则转发 | 改动小 | 无法做聚合/事务/改写，易成 god object | 拒绝 |

**选择 A**，并把阶段边界固定为：

```
sql.Parse → route.Route → sql/rewrite.Rewrite → execute.Dispatch → merge.Merge → result.Write
```

- `route`/`rewrite`/`plan` 纯函数；
- `execute` 唯一持有 lease；
- `merge` 只消费 `result.Stream`；
- `result` 不依赖 mysql；编码由 `execute`/`mysql` 适配。

## 5. 模块拆分与接口草案

### 5.1 新增/调整包

| 包 | 职责 | 关键接口 |
|---|---|---|
| `pkg/sql` | `Statement.AST` 落地；分类；分片键提取辅助 | `Parse`（复用 parser）、`ShardKey(stmt)` |
| `pkg/sql/rewrite` | 按 Target 改写 SQL | `Spec`、`Select(sel, spec) (string, error)` |
| `pkg/route/shard`（新） | 静态规则、算法、路由 | `Rule`、`Algorithm`、`Router.Route → []Target` |
| `pkg/plan` | `SingleShard/MultiShard`、`MergeSpec`、`TxPolicy` | `Builder` 组合 Router+Rewriter |
| `pkg/result` | 逻辑 Row/Stream；解码/编码接口 | `Decoder`、`Encoder`/`Writer` |
| `pkg/merge` | concat/limit/ordered/aggregate | `Merge(streams, fields, MergeSpec)`、`Validate` |
| `pkg/execute` | N 目标 dispatch、LeaseSet、写回 | `dispatchTargets`、`writeRaw`/`writeMerged` |
| `pkg/session` | `StmtTable`、per-slice pin、vars | `StmtTable`、`Pin(slice, until)` |
| `pkg/config` | sharding 规则/全局表/序列/默认片 | `Validate`、`SameTopology` |
| `pkg/server` | 唯一装配点 | 构建 Cluster + `shard.Router` + Builder |
| `test/harness` | 多 Slice + 单库 oracle | `StartShardedTopology`、`StartOracle` |

### 5.2 建议的接口形态

```go
// pkg/route/shard
type Rule struct {
    Table     string
    Key       string
    Algorithm Algorithm
    Slices    []string
}

type Algorithm interface {
    // Route returns slice names for the shard-key values; 0 means "all slices"
    // (scatter), which P0 rejects for writes.
    Route(keyValues []sql.Value) ([]string, error)
}

type Router struct {
    Rules       map[string]Rule // logical table -> rule
    SliceNames  []string
    DefaultRule Rule // optional; P0 rejects unconfigured tables
}
```

```go
// pkg/plan
type Kind uint8

const (
    Passthrough Kind = iota
    SingleShard
    MultiShard
    Reject
)

type TxPolicy uint8

const (
    TxNone TxPolicy = iota
    TxSingleShard // all statements must stay on one slice
    TxReject      // cross-shard transaction / write
)

type Plan struct {
    Kind    Kind
    Targets []route.Target // Target.SQL is the rewritten SQL
    Merge   MergeSpec
    Tx      TxPolicy
    Reject  *errs.MySQL // 固定 MySQL ERR（errno/SQLSTATE/message）
}
```

```go
// pkg/result
type Stream interface {
    Fields() []Field
    Next() (Row, error)
    Close() error
}

type Writer interface {
    WriteFields([]Field) error
    WriteRow(Row) error
    Close() error
}
```

```go
// pkg/merge
type Merger interface {
    Merge(ctx context.Context, streams []result.Stream, spec plan.MergeSpec) (result.Stream, error)
}
```

```go
// pkg/execute
func (p *Pipeline) dispatch(ctx context.Context, rt *Runtime, pl *plan.Plan) ([]result.Stream, error)
```

### 5.3 结果编解码放哪

- **解码**：`pkg/backend` 新增 `decodedStream`，用 `mysql` 读 packet、产出
  `result.Row`。原因：`backend` 已经同时依赖 `mysql` 与 `result`，不破坏
  `result` 零依赖。
- **编码**：`pkg/mysql` 增加列定义/行/EOF 的基础编码函数；`execute` 负责把
  `result.Field/Row` 转成 `mysql` 的协议结构并写前端。`result` 不 import mysql。
- **单分片**：继续 `rawStream.ForwardTo`，不触碰解码/编码。

### 5.4 S0 先行实现调整（行为保持）

这些调整不打开 `mode=sharding`，但必须在 `route/shard` 之前完成：

| # | 调整 | 文件 | 为什么 |
|---|---|---|---|
| 1 | `execute` 从 `Targets[0]` 改为 dispatch over `[]Target`；N=1 仍走 raw | `pkg/execute/pipeline.go` | 分片 N 目标的前提 |
| 2 | 执行时使用 `Target.SQL`（空则回落 `stmt.Payload`） | `pkg/execute/pipeline.go` | 当前 `Target.SQL` **完全没有消费方**；不改则 rewrite 不生效 |
| 3 | `result.Field` 补齐 `OrgName`/`OrgTable`/`ColumnLength`/`Decimals` | `pkg/result/result.go` | 合并后要原样编码 ColumnDefinition41 |
| 4 | `backend` 增加 decodedStream（列/行/EOF） | `pkg/backend/stream.go` | 归并必须拿到 `result.Row` |
| 5 | `mysql` 增加结果集编码（列/行/EOF/OK） | `pkg/mysql/result.go` | 归并结果写回前端 |
| 6 | 前端 `deprecateEOF` 能力进入 Session/Runtime，不要硬编码 | `pkg/backend/stream.go`、`pkg/proxy` | 合成结果集必须按客户端协商值编码 |
| 7 | `plan` 增加 `SingleShard/MultiShard`、`TxPolicy`、`MergeSpec` 字段 | `pkg/plan/plan.go` | 分片计划契约 |
| 8 | `session.StmtTable` 落地；prepare 不再无脑 `MarkReserved` | `pkg/session`、`pkg/proxy` | 多片 prepared 与 stmt id 映射 |
| 9 | `sql.Statement.AST` 落地；COM_QUERY 只 parse 一次，补 `Kind` | `pkg/proxy`、`pkg/sql` | 分片键提取与改写的前提 |
| 10 | `config/server` 支持 N Slice 装配（`mode=sharding` 仍拒绝） | `pkg/config`、`pkg/server` | S1 打开前先有装配骨架 |
| 11 | harness 增加多 Slice + 单库 oracle | `test/harness` | 分片行为的验收基础 |

两个容易被忽略的点：

- **`Target.SQL` 目前没有使用方。** `execute.Handle` 永远发送 `stmt.Payload`，
  所以即使计划层产出了改写 SQL，也不会生效。S0 必须先加"优先用 `Target.SQL`"
  的分支，并保留 `stmt.Payload` 回落。
- **`deprecateEOF` 被硬编码为 true。** `rawStream.ForwardTo` 和
  `ForwardPrepareResponse` 都写死 true；多分片合成结果集必须使用前端握手协商值，
  否则老客户端/不同驱动会解析错。单分片 raw 路径暂时不受影响，但 S0 应把能力
  传进 `Runtime`/`Stream`。
- **`reserved` 是会话全局 bool。** P0 单分片可以继续用全局 pin；S2 多分片读
  没有事务/临时表，问题不大；但一旦分片内临时表/锁跨片，就需要 per-slice pin
  集合，不能把全局 `reserved` 当作"所有片都 pin"。
- **binary 结果集只在单分片 prepared 路径出现。** P0 不需要多分片 binary 归并；
  多分片 prepared 直接 Reject，避免同时做文本/二进制两套归并。

## 6. 数据通路

### 6.1 单分片 SELECT/DML（P0）

```
proxy COM_QUERY
  → sql.Parse（一次，AST）
  → shard.Route（等值键 → 1 个 Target）
  → plan.SingleShard（Target.SQL 可能改写）
  → execute: acquire/align → Conn.Exec → raw ForwardTo
```

- 零解码、零归并；
- 事务：同一 slice 的 lease 保持到 COMMIT/ROLLBACK；
- 无分片键 / 多 slice → `Reject`。

### 6.2 多分片 SELECT（P1）

```
route → []Target
  → 并发 acquire/exec（每片一个 stream）
  → backend.decodedStream × N
  → merge.Merge（identity/aggregate/order/limit）
  → execute 编码列/行到前端
```

- 需要 `MergeSpec` 由 `plan` 根据 AST 填充；
- **流式归并优先**：concat / 已下推 ORDER BY 的 k 路堆 / LIMIT / 无 GROUP BY 聚合
  都只持有 O(k) 或 O(1) 状态；`GROUP BY`/`DISTINCT` 是半流式，需内存上限；
  未下推排序/跨片 JOIN 等非流式形态在 P1 直接 Reject。详见
  [merge-engine-design.md](merge-engine-design.md)；
- `LIMIT/OFFSET` 下推：每片 `LIMIT offset+count`，归并后重新截断。

**当前已落地（P0.5，2026-10-31）**：`pkg/merge` 的 `Concat` + `Limit`：

- 触发条件：分片表 SELECT 没有可证明的单片键约束（无 WHERE、范围、
  非键列 OR、跨片 `IN` 等），或键值映射到多个 slice；
- 支持 `ORDER BY` 列名/位置/表达式（多列、ASC/DESC），由 k 路堆归并；
  未投影的列/表达式通过隐藏列下推，`Output` 投影去掉隐藏列；字符串/二进制/
  JSON 类型 1235；`SELECT *` 搭配表达式排序 1235；
- 支持 `COUNT/SUM/MIN/MAX/AVG`（无 GROUP BY）、数值/日期列 GROUP BY、
  数值/日期列 DISTINCT，以及其按已选列的 ORDER BY/LIMIT；
- 支持 GROUP BY 的 AND 数值 HAVING（分组列/聚合别名/匹配聚合表达式）；
- 不支持窗口函数、聚合 DISTINCT、HAVING OR/字符串、聚合表达式排序、
  INTO、锁定读、SQL_CALC_FOUND_ROWS；字符串列分组/去重/排序仍 1235；
- `LIMIT n` / `LIMIT n OFFSET m`：OFFSET>0 时 per-shard 改写成 `LIMIT m+n`，
  归并后由 `Limit` 截断/跳过；
- lease 顺序 acquire（session map 不并发写），随后各 target 的
  align/exec/列头解码并发执行；列元数据校验后按 target 顺序交给 merge；
  LIMIT 提前结束时未读完的后端连接直接 Discard；
- 事务 / `autocommit=0` / reserved / prepared pin 的会话中的多分片语句
  1235（多片连接无法共享事务语义）；
- 结束符的 `warnings` 固定 0（各分片 warning 不汇总）；任何分片在写列头前
  返回 ERR 时，原样回传 ERR、不产生部分结果集；写行后某片失败会关闭会话
  （与后端断连一致），后续 merge/error 设计再细化；
- 结果行顺序为 target 顺序，不保证与单库 `ORDER BY` 或无排序时一致。

### 6.3 多分片写（P1/P2）

- P0：多分片 `INSERT/UPDATE/DELETE` 一律 Reject；
- P1：仅在显式允许且无事务时，逐片执行并汇总 affected rows；
  部分失败必须报错，不能返回 OK；
- P2：跨片事务（2PC/Saga）另立设计。

### 6.4 DDL 策略（2026-10-31 修订）

**代理不广播 DDL。** 所有 Slice 同时执行 DDL 会放大风险：部分失败无法回滚、
锁表/重建在不同分片异步发生、流量高峰影响稳定性。

- sharding 模式下 `CREATE/ALTER/DROP/TRUNCATE/RENAME` 一律 1235；
- DDL 由外部变更流程执行（gh-ost/pt-osc 等在线工具），按 Slice 分批/串行，
  先小流量验证；
- 代理后续只读校验 schema 版本/表结构一致性，发现不一致时拒绝路由或告警；
- 全局/广播表 DDL 同样走外部流程；
- 不在代理内做部分失败自动回滚；
- 若未来支持代理内 DDL，必须单独立项：在线变更、限流、审计、回滚/补偿。

## 7. 分阶段计划

| 阶段 | 内容 | 用户可见行为 | 退出条件 |
|---|---|---|---|
| **S0 执行/结果骨架** | `execute` 支持 `[]Target`；`LeaseSet`；`result.Decoder/Writer`；`StmtTable` 骨架；AST 上数据面；N=1 行为不变 | 无变化 | 现有单测/差分全绿；N=2 fake target 用例 |
| **S1 P0 单分片** | `route/shard`（hash_mod + 等值/IN）；`SingleShard/Reject`；`EXPLAIN ROUTE`；单分片 DQL prepared；DDL 外部迁移（proxy 拒绝）；配置打开 `mode=sharding` | 单分片 SQL 可用；跨片/无键明确 Reject | 路由表驱动单测；多 Slice e2e；无静默错结果 |
| **S2 多分片只读** | `MultiShard` dispatch；identity/aggregate/order/limit merge；结果编解码；`LIMIT` 下推 | 跨片 SELECT 可用 | 分片 vs 单库 metamorphic 全绿；内存上限 |
| **S3 高级** | GROUP BY/HAVING/DISTINCT/UNION；全局表；全局序列；多分片 prepared；跨片 JOIN；2PC/Saga；在线 DDL | 逐步开放 | 每项单独设计+测试，不一次性开 |

映射到现有文档：S1 ≈ `module-map` 的 P3，S2/S3 ≈ P4 的拆分。

### 7.1 实施顺序（配置/池优先）

先拓扑、后执行、再路由/归并。每个 PR 行为可回滚，前 6 步不打开
`mode=sharding`。

| 顺序 | 模块/内容 | 行为变化 | 退出条件 |
|---|---|---|---|
| **A1 配置模型** | `ShardingConfig` 扩展：规则、算法参数、Slice 引用；严格校验；`SameTopology` 覆盖规则 | 无（`mode=sharding` 仍拒绝） | 配置表驱动用例；非法规则全部拒绝 |
| **A2 多 Slice 装配** | `server` 抽出 `buildCluster`，按 `slices` 建 N 个 `Slice`/Pool；`Cluster` 已支持多 Slice | 无（single/rwsplit 仍 1 个 Slice） | N=3 Slice 装配单测；池 Close/Lease 正常 |
| **A3 执行骨架** | `execute` dispatch over `[]Target`；`Target.SQL` 生效；per-target `ResponseInfo`；LeaseSet | 无（N=1 raw 路径不变） | N=2 fake target：payload/lease/错误/Close |
| **A4 结果骨架** | `backend.decodedStream`；`mysql` 结果集编码；`result.Field` 元数据；`deprecateEOF` 能力传递 | 无（不接数据面） | packet fixture + 脚本化 Stream |
| **A5 计划/会话** | `SingleShard/MultiShard`、`TxPolicy`、`MergeSpec`；`StmtTable`；AST parse 一次 | 无（新类型未接线） | plan/session/sql 单测 |
| **A6 测试拓扑** | `test/harness` 多 Slice + 单库 oracle | 无 | 能启动 N Slice + oracle；`-short` Skip |
| **B1 单分片 sharding** | `route/shard`（等值/同片 IN）；`SingleShard/Reject`；`EXPLAIN ROUTE`；单分片 DQL prepared；打开 `mode=sharding`；DDL 外部迁移 | 新功能 | 单分片 e2e；无键/跨片 Reject |
| **B2 多分片只读** | rewrite 下推、merge operator、并发 dispatch、结果编码 | 新功能 | 分片 vs 单库 metamorphic；内存上限 |
| **B3 高级** | GROUP BY/DISTINCT/UNION、全局表/序列、多分片 prepared、跨片 JOIN、2PC | 逐步 | 每项单独设计 |

为什么配置/池先做：

- `slices`/规则是运行时拓扑的输入；先让配置与装配可测，后面才有一个真实的
  N Slice Cluster 可以跑执行/归并。
- **连接池不需要重写**：`backend.Pool` 是 endpoint 级的，`Slice` 已是
  "master + slaves + LB"，`Cluster` 已是 map。真正要做的是**装配**和
  （可选的）per-slice pool 覆盖，不是改池实现。
- `mode=sharding` 在 A1–A5 保持拒绝，避免"配置能写、运行时不能用"的半成品；
  B1.0 完成路由与装配后已打开。

主流程顺序不能反：

1. 先有 N 目标执行（A3），才谈得上多分片；
2. 先有结果解码/编码（A4），才谈得上归并；
3. 先有 AST/Plan（A5），才谈得上分片键与改写；
4. 最后才是 `route/shard`（B1）和 merge（B2）。

### 7.2 B1 当前实现状态（2026-10-31）

已落地（B1.0）：

- `route/shard` + `hash_mod`（整数）/ `hash_mod_str`（字符串）：单分片
  `SELECT/INSERT/UPDATE/DELETE` 的等值 / `<=>` / 同片 `IN`；多行 INSERT 必须同片；
- 无键、跨片、JOIN、多表、schema 限定、DDL、SHOW、事务、prepared 均显式
  1235；
- `plan.SingleShard + TxSingleShard`；server 装配多 Slice + shard Router；
- 无 FROM 的 SELECT（常量、`LAST_INSERT_ID()`/`ROW_COUNT()` 改写）路由默认片；
- 2 Slice 真拓扑 e2e：CRUD、数据落片、跨片拒绝、`LAST_INSERT_ID`/`ROW_COUNT`。

已落地（B1.1，DQL 兼容）：

- `EXPLAIN ROUTE`：输出 Kind/Slice/Role/DB/SQL/Reject 原因；
- 单分片 prepared SELECT + `StmtTable`：**分片键必须是字面量**，
  参数化分片键明确 1235；prepared 会话 pin 后端连接；
- `SHOW TABLES/COLUMNS/CREATE/INDEX/VARIABLES`、`USE`、`COM_INIT_DB`、
  `SET NAMES` 路由默认片，连接初始化不再被打断。

已落地（B1.2，多分片 concat，2026-10-31）：

- 分片表 SELECT 在无法证明单片时 scatter 到规则的全部 slice；跨片 `IN`
  只访问键值命中的 slice（按规则顺序去重）；
- `MultiShard + TxNone`：`execute.dispatch` 逐片解码列头、校验列元数据一致、
  按 target 顺序流式拼接行；成功/失败都释放 lease，失败路径丢弃未读完的
  后端连接；
- 只支持“拼接后语义不变”的 SELECT；`ORDER BY / LIMIT / GROUP BY / HAVING /
  DISTINCT / 窗口 / 聚合 / INTO / 锁定读 / SQL_CALC_FOUND_ROWS` 以及
  事务、`autocommit=0`、reserved、prepared pin 会话中的多分片语句均 1235；
- prepared statement 的 pin 从 `reserved` 拆成 `stmtPinned`：最后一个
  `COM_STMT_CLOSE` 会释放 pin 与 lease，会话可继续执行多分片查询；
- 2 Slice 真拓扑 e2e：无键/跨片 `IN` 查询、列名、行集合、`EXPLAIN ROUTE`
  多 target，以及 merge 类语句/事务的显式拒绝。

已落地（B1.3，键类型严格化，2026-10-31）：

- `hash_mod` 只接受整数键：`int/int64/uint64`，以及可精确解析为十进制整数的
  字符串（`'1'`、`'01'`、`'+1'`、`'-1'`）；十进制、hex/bit、非数字字符串 1235；
- `hash_mod_str` 只接受字符串/字节串键，按 FNV-1a 取模；数值字面量 1235，
  避免 VARCHAR 与数字比较的非单射语义（`'01'`/`'1.0'` 不能靠单值哈希收窄）；
- 支持一元正负号（`key = -1`）；`= / <=> / IN / AND / OR / 括号` 的键提取
  对两种算法共用；
- 修复同一逻辑键因加引号而路由到不同分片的问题：整数规则下 `1` 与 `'1'`
  现在同片；此前 UPDATE/DELETE 会静默打到错分片并影响 0 行；
- 表驱动单测覆盖两种算法的接受/拒绝域；2 Slice e2e 增加字符串键表
  （`profiles.name` + `hash_mod_str`）与引号数字键回归。

已落地（B1.4，多分片 LIMIT，2026-10-31）：

- 无 `ORDER BY` 的 `LIMIT n`：各分片原 SQL 已带同一 LIMIT，
  `pkg/merge.Concat` 后由 `Limit` 截断到最终 n 行；
- `LIMIT n OFFSET m` / `LIMIT m, n`：per-shard SQL 改写成 `LIMIT m+n`
  （AST restore），归并后由 `Limit` 全局跳过 m 行；
- 新增 `pkg/merge`（`Concat`、`Limit` 流式 operator，纯 `result.Stream`）；
- LIMIT 提前结束时后端仍有未读 packet，dispatch 直接 Discard，不把连接放回池；
- e2e：`LIMIT 0/1/100`、跨片 `IN + LIMIT`、OFFSET 改写与会话恢复。

已落地（B1.5，多分片 ORDER BY，2026-10-31）：

- 每片执行原 ORDER BY，`pkg/merge.Ordered` 用 k 路堆归并已排序流，只持有
  每片一行；支持多列、ASC/DESC、位置（`ORDER BY 1`）；
- 比较类型：整数/无符号整数、FLOAT/DOUBLE、DECIMAL、DATE/DATETIME/TIMESTAMP
  （日期按规范文本）；NULL 按 MySQL 语义 ASC 在前、DESC 在后；
- 字符串/二进制/JSON 等 collation 相关类型显式 1235，避免字节序与 MySQL
  排序不一致；
- ORDER BY 未投影的列/表达式会自动补隐藏列并投影掉；字符串/二进制/JSON
  类型 1235；`SELECT *` 搭配表达式排序需显式 select list；类型/列校验在
  写列头前完成；
- `ORDER BY ... LIMIT n OFFSET m` 可用：per-shard 改写成 `LIMIT m+n`，
  有序归并后全局跳过 m 行；
- e2e：ASC/DESC/位置/表达式/LIMIT、未投影列排序、字符串排序的显式拒绝。

已落地（B1.6，无 GROUP BY 聚合，2026-10-31）：

- `COUNT(*)` / `COUNT(expr)` / `SUM` / `MIN` / `MAX` / `AVG` 的多分片归并：
  每片返回部分聚合行，`pkg/merge.Aggregate` 折叠为最终行；AVG 通过改写注入
  隐藏 `SUM/COUNT` 列，按输出列的 DECIMAL scale 计算；
- 支持聚合别名与多聚合投影；COUNT 求和，SUM 用 big.Rat/float 累加，
  MIN/MAX 用与 ORDER BY 相同的类型比较；全 NULL/空输入按 MySQL 语义返回
  NULL（COUNT 为 0）；
- 新增 `pkg/sql/rewrite` 承接 per-shard SQL 改写（LIMIT OFFSET 已迁入）；
  `pkg/merge.Merge` 统一装配 Concat/Ordered/Aggregate/Group/Limit，execute 不再选算子；
- 仍 1235：`COUNT(DISTINCT ...)`、聚合与其他投影混合、聚合 + ORDER BY、
  HAVING/DISTINCT。

已落地（B1.7，GROUP BY，2026-10-31）：

- 投影分组列 + COUNT/SUM/MIN/MAX/AVG；`pkg/merge.Group` 按 group key 合并
  各片部分分组，输出为首次出现顺序（MySQL 未保证顺序）；
- 分组列必须在 select list；分组表达式/未选分组列 1235；
- 字符串/二进制分组列 1235（collation 权重表未引入）；
- GROUP BY + ORDER BY/LIMIT、数值 HAVING 已支持；
- single-DB oracle 覆盖用户分组、金额跨片分组、过滤后分组。

已落地（B1.8，DISTINCT，2026-10-31）：

- `SELECT DISTINCT 列/表达式` 跨片去重；各片先 DISTINCT，归并层按规范化行 key
  去重，输出首次出现顺序；
- 数值/日期键可用；字符串/二进制列 1235（collation 权重表未引入）；
- `DISTINCT + ORDER BY/LIMIT` 已支持；聚合 DISTINCT 仍 1235；
- oracle 覆盖单列/多列 DISTINCT 与单库一致。

已落地（B1.9，GROUP BY/DISTINCT 排序与 LIMIT，2026-10-31）：

- `GROUP BY`/`DISTINCT` 支持按已选列/别名/位置 `ORDER BY`；
- 分组/去重后在归并层排序（`merge.Sort`），再应用全局 `LIMIT/OFFSET`；
- per-shard SQL 移除 LIMIT（`rewrite.Spec.RemoveLimit`），避免各片提前截断
  部分分组；ORDER BY 仍保留在各片但最终以全局排序为准；
- 聚合表达式排序（`ORDER BY COUNT(*)`）仍 1235，需先起别名或位置；
- 字符串/二进制排序仍 1235（collation 权重表未引入）。

已落地（B1.10，HAVING，2026-10-31）：

- `GROUP BY ... HAVING` 支持已选分组列/聚合别名/匹配聚合表达式与数值字面量
  的 `= != < <= > >=` 比较，多个条件按 AND 组合；
- per-shard SQL 移除 HAVING（`rewrite.Spec.RemoveHaving`），HAVING 在全局
  分组后执行；
- OR、字符串字面量、未选聚合、无 GROUP BY 的 HAVING 仍 1235；
- oracle/e2e 覆盖别名、聚合表达式、AND、跨片 group 过滤。

未落地（B1.2）：

- 结构化路由决策日志/metrics；
- 从库健康/延迟探测：连接池定期探测，选源时屏蔽不可用从库并回落主库
  （不做 per-request GTID，也不做会话级 read-after-write）；
- 单分片写事务与 prepared 写。

DML/DDL 后置：

- DDL 不在代理内广播，走外部迁移流程（§6.4）。

### 7.3 DQL 优先 backlog

| 顺序 | 任务 | 退出条件 |
|---|---|---|
| **D1** | `EXPLAIN ROUTE` + 路由决策日志 | 路由/计划单测；真实拓扑可读 |
| **D2** | 单分片 prepared SELECT + `StmtTable` | 驱动 prepare/execute/close 往返 |
| **D3** | `SHOW TABLES/COLUMNS/CREATE`、`USE`、`SET NAMES` | mysql cli/驱动连接初始化不被打断 |
| **D4.0** | 多分片 SELECT scatter + concat（`MultiShard`、`execute.dispatch`、列元数据校验、stmt pin 释放） | 2 片 e2e；无键/跨片 IN 行集合正确；merge 类语句 1235 |
| **D4.2** | 分片键值域严格化：`hash_mod` 整数 / `hash_mod_str` 字符串；引号数字、负数、混合 IN | 表驱动单测；字符串键 e2e；类型不匹配 1235 |
| **D4.1** | 从库健康/延迟探测：池内定期探测，选源屏蔽不可用从库，回落主库（**延后**，单机与分片共用） | `STOP REPLICA`/杀从库后读请求不落坏从库；恢复后重新纳入 |
| **D5** | 多分片 SELECT rewrite 下推（聚合/隐藏列）；ORDER BY、LIMIT/OFFSET 与 `pkg/merge.Concat/Limit/Ordered` 已落地 | 分片 vs 单库一致 |
| **D6** | 流式 merge operator（concat/order/limit/aggregate/group/distinct） | 单测 + metamorphic |
| **D7** | execute 并发 dispatch + 结果写回；已落地：lease 顺序 acquire + align/exec/header 并发 | N 片 e2e；失败清理 |
| **D8** | DQL corpus/randgen/metamorphic | CI/nightly |

DML 事务、prepared 写、全局序列、跨片 JOIN 后置；DDL 不进代理。

## 8. P0 明确范围

### 8.1 支持

B1.0：

- 单分片 `SELECT/UPDATE/DELETE`：分片键 `=`、`<=>`、同片 `IN`；
- 单分片 `INSERT`：分片键存在且所有行落同一片；
- 无 FROM 的常量 SELECT / 会话函数改写。

B1.1（已落地，DQL 兼容）：

- `EXPLAIN ROUTE`：输出 Kind、Targets、Reject 原因；
- 单分片 prepared SELECT（`StmtTable` 接线；分片键必须字面量）；
- `SHOW TABLES/COLUMNS/CREATE/INDEX/VARIABLES`、`USE`、`SET NAMES`；
- 从库健康/延迟探测（连接池内，选源时屏蔽坏从库）仍未实现；
- 不做会话级 read-after-write / per-request GTID；异步复制下接受最终一致。

B1.2（已落地，多分片 concat）：

- 简单 SELECT 的 scatter（无键/范围/非键列 OR）与跨片 `IN`；
- `MultiShard` 逐片列头校验 + 按 target 顺序拼接行；
- `ORDER BY/LIMIT/GROUP BY/HAVING/DISTINCT/窗口/聚合/INTO/锁定读/
  SQL_CALC_FOUND_ROWS` 仍 1235；
- 事务、`autocommit=0`、reserved、prepared pin 中的多分片语句 1235；
- 多分片结束符 warnings 不汇总，固定 0。

B1.3（已落地，键类型严格化）：

- `hash_mod`（整数键）：整数与严格十进制整数字符串；非整数 1235；
- `hash_mod_str`（字符串键）：字符串/字节串；数值字面量 1235；
- 支持一元正负号；引号数字键与裸数字键同片。

B1.4（已落地，多分片 LIMIT）：

- 无 `ORDER BY` 的 `LIMIT n` 按片执行后在 concat 流上截断；
- `LIMIT ... OFFSET` per-shard 改写成 `LIMIT m+n`，归并后全局跳过 m 行。

B1.5（已落地，多分片 ORDER BY）：

- 列名/位置的 ASC/DESC、多列排序；整数/无符号/浮点/DECIMAL/日期文本；
- 未投影的列/表达式自动补隐藏列；字符串/二进制/JSON 1235；
- `ORDER BY ... LIMIT n OFFSET m` 可用（per-shard `LIMIT m+n` + 全局跳过）。

B1.6（已落地，无 GROUP BY 聚合）：

- `COUNT/SUM/MIN/MAX/AVG` 多分片归并；`pkg/merge.Aggregate`；
- AVG 注入隐藏 SUM/COUNT 列，保留原输出列名/scale；
- 聚合 DISTINCT、混合投影、聚合 + ORDER BY 仍 1235。

B1.7（已落地，GROUP BY）：

- 投影分组列 + 聚合函数；`pkg/merge.Group` 合并跨片部分分组；
- 分组列必须被 select；字符串/二进制分组列 1235（collation 未引入）；
- GROUP BY + ORDER BY/LIMIT、HAVING、分组表达式仍 1235。

B1.8（已落地，DISTINCT）：

- 数值/日期列的跨片 DISTINCT；`pkg/merge.Distinct` 规范化行 key 去重；
- 字符串/二进制列、聚合 DISTINCT 仍 1235。

B1.9（已落地，GROUP BY/DISTINCT 排序与 LIMIT）：

- 按已选列/别名/位置 ORDER BY，归并层排序后应用全局 LIMIT/OFFSET；
- per-shard LIMIT 移除，避免部分分组被提前截断；
- 聚合表达式排序、字符串排序仍 1235。

B1.10（已落地，HAVING）：

- `GROUP BY ... HAVING` 的 AND 数值比较（分组列/聚合别名/匹配聚合表达式）；
- per-shard HAVING 移除，全局分组后过滤；
- OR、字符串字面量、未选聚合、无 GROUP BY 的 HAVING 仍 1235。

DDL 不通过代理执行：由外部迁移工具按片变更（§6.4），代理始终显式 Reject。

### 8.2 Reject（当前）

- 多分片写（INSERT/UPDATE/DELETE）；跨片 `IN` 的写语句；
- 分片键字面量类型与算法值域不匹配（如 `hash_mod` 下的 `'abc'`、
  `hash_mod_str` 下的 `1`）；
- 分片表上需要归并的 SELECT（HAVING/窗口/INTO/锁定读/SQL_CALC_FOUND_ROWS）；
  子查询；聚合 DISTINCT、聚合与非聚合投影混合、聚合 + ORDER BY、聚合表达式
  排序、HAVING OR/字符串/未选聚合；字符串/二进制 GROUP BY/DISTINCT/排序 1235；
- 事务 / `autocommit=0` / reserved / prepared pin 会话中的多分片语句；
- 跨片/多表 JOIN、schema 限定表；
- prepared 参数化分片键、prepared DML、prepared 多分片；
- SHOW STATUS/MASTER 等非安全 SHOW；`SET sql_mode` 等非 `SET NAMES`；
- DDL 永久 Reject（外部迁移工具按片变更，见 §6.4）；SHOW/事务控制/prepared 写当前 1235；
- 多分片 `LAST_INSERT_ID` / `ROW_COUNT`；
- 未配置规则的普通表（P0 无默认片）；
- `FOUND_ROWS()` / `SQL_CALC_FOUND_ROWS`（沿用单机决策）；
- 表名改写/物理表后缀（P0 同库同名）。

### 8.3 非目标（整个 P0/P1）

- 在线扩缩容、数据迁移、双写；
- 跨片 JOIN 优化器；
- 全局一致性读；
- 多租户 namespace；
- 管理面/审计/查询防火墙。

## 9. 测试策略

| 层 | 内容 | Docker |
|---|---|---|
| 规则单测 | `hash_mod`/`hash_mod_str` 值域、Slice 引用、重复规则 | 否 |
| 路由单测 | AST 键提取：`=`/`IN`/`BETWEEN`/无键/多表；整数/字符串键类型 | 否 |
| 计划单测 | `SingleShard/MultiShard/Reject`、`EXPLAIN ROUTE` | 否 |
| 执行单测 | fake Conn + 2 个 target：lease 按片、N=1 raw、N=2 dispatch | 否 |
| 归并单测 | 脚本化 Stream：count/sum/min/max/avg/order/limit | 否 |
| 多 Slice e2e | 2 Slice + 每片 master+replica + 真代理；`TestShardedReplicas` 验证读从/写主 | 是 |
| 变形测试 | 聚合已接单库 oracle（NULL/负数/跨片 IN）；ORDER BY/LIMIT 与随机数据待补 | 是 |
| 故障 | 杀一个 Slice、DDL 部分失败、事务中断 | 是 |

`test/harness` 现状：

- `StartShardedTopology(n)`：n 个 master-only Slice（快速路径）；
- `StartShardedTopologyWithReplicas(n, replicas)`：每 Slice 独立 master +
  GTID 复制从库，从库追平后 `read_only`；`ShardedTopology.Sync` 等待所有分片
  复制追平；
- 聚合 metamorphic 用单实例 harness 作 oracle；随机数据生成与 ORDER/LIMIT 变形待补。

## 10. 需要先决策的问题

| # | 决策 | 推荐 | 备选 |
|---|---|---|---|
| 1 | 分片粒度/命名 | 同库同名，`Target.DB` 预留 | 物理表后缀（P2） |
| 2 | P0 算法 | `hash_mod`（整数键）+ `hash_mod_str`（字符串键）+ 等值/同片 IN | range/date（P1） |
| 3 | ID 生成 | P0 单分片用本地 auto_increment；多分片 INSERT 要求显式 ID 或 Reject | Snowflake/segment（P2） |
| 4 | P0 是否含多分片读 | 是（仅 concat：无 ORDER/LIMIT/聚合/去重） | 全缓冲归并（不推荐） |
| 5 | prepared | P0 单分片；多分片 Reject | 广播 prepare（S2） |
| 6 | DDL 策略 | 代理不广播，sharding 模式 Reject；外部迁移工具按片变更 | 代理内在线 DDL（单独立项） |
| 7 | 未配置表 | Reject | `default_slice`（P2） |
| 8 | 归并内存 | 流式优先；GROUP BY/DISTINCT 半流式 + 上限；未下推排序 Reject | 全缓冲（不推荐） |
| 9 | 解析器 | TiDB parser AST | token 启发式（不推荐） |
| 10 | `MergeSpec` 归属 | `plan` | `result` |

## 11. 风险

- **AST 性能**：每查询解析；P1 可加 normalized SQL → AST 缓存。
- **结果编解码**：二进制协议（prepared）与 DEPRECATE_EOF 的边界要单独测；
  单分片继续 raw，降低风险。
- **事务语义**：跨片 Reject 必须稳定错误码；不能出现"部分成功但返回 OK"。
- **DDL**：代理不广播；外部变更流程需保证各片 schema 一致，代理后续只读校验。
- **ID**：本地 auto_increment 跨片不唯一；多分片 INSERT 在 P0 必须拒绝，
  不能静默生成冲突 ID。
- **全局表/默认片**：P0 不做，避免"看似支持、实际错结果"。

## 12. 参考

- 本地 checkout：`/tmp/prism-oss-survey/{kingshard,Gaea,dbpack,vitess}`
- kingshard：`proxy/router/router.go`、`shard.go`、`planbuilder.go`；
  `proxy/server/conn_select.go`、`conn_preshard.go`
- Gaea：`proxy/plan/plan_select.go`、`merge_result.go`、`plan_insert/update/delete.go`；
  `proxy/server/executor_select.go`
- dbpack：`pkg/plan/query.go`、`insert.go`、`update.go`、`delete.go`、`result.go`；
  `pkg/cond/sharding_algorithm.go`；`pkg/executor/sharding.go`
- Vitess：`go/vt/vtgate/planbuilder/select.go`、`engine/route.go`、
  `engine/aggregate.go`、`engine/orderby.go`、`engine/limit.go`、
  `engine/scatter_conn.go`
- ShardingSphere：`infra/route`、`infra/rewrite`、`infra/executor`、`infra/merge`
  五阶段；`LastInsertIdReviser`
- 现有设计：[module-map.md](module-map.md)、[architecture.md](../architecture.md)
