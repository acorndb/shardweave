# shardweave 架构基线（v0.3）

> 状态：实现基线（M3 完成）。本文是项目的 north star；实现偏离此处记录的设计时
> **先改文档**。按模块分类的介绍见本目录各文档。
> 日期：2026-11-01
> 模块路径：`github.com/acorndb/shardweave`

## 0. 模块地图

数据通路自上而下；每份模块文档给出职责、关键类型、行为不变量与测试入口。

| 文档 | 层 | 包 |
|---|---|---|
| [protocol.md](protocol.md) | 协议层 | `pkg/mysql/{packet,auth,server,client,collation}` |
| [statement.md](statement.md) | 语句层 | `pkg/sql`、`pkg/sql/parser`、`pkg/sql/rewrite` |
| [routing.md](routing.md) | 路由与计划层 | `pkg/route/{source,shard}`、`pkg/plan` |
| [execution.md](execution.md) | 执行与资源层 | `pkg/execute`、`pkg/backend`、`pkg/session` |
| [merge.md](merge.md) | 归并与结果层 | `pkg/merge`、`pkg/result` |
| [assembly.md](assembly.md) | 装配与进程层 | `pkg/proxy`、`pkg/server`、`pkg/config`、`cmd/shardweave` |

```
                ┌────────────────────────────────────────────┐
  MySQL client  │                 shardweave                 │
 ──────────────►│  proxy ─► sql（token 扫描，一次语义快照）    │
                │    │                                       │
                │    ▼                                       │
                │  plan（router → Targets + MergeSpec）        │
                │    │                                       │
                │    ▼                                       │
                │  execute ─► backend pool                   │
                │    │        （Align：库/字符集/会话变量；     │
                │    │          未读尽连接淘汰）                │
                │    ▼                                       │
                │  merge（order / aggregate / group /          │
                │         distinct + collation 权重）          │
                └─────┬─────────────┬─────────────┬──────────┘
                      ▼             ▼             ▼
                   slice s0      slice s1   …   slice sN
                （master / slaves，MySQL 5.7+ / 8.0）
```

通路要点：

- **两处本地应答，不触后端**：`EXPLAIN ROUTE`（合成路由结果集）与
  `SET NAMES utf8mb4`（合成 OK）。后者的本地化是列元数据保真的前提——透传执行会把
  后端 `character_set_results` 从 NULL 重置，破坏列定义携带真实 collation（A10）。
- **N=1 Passthrough 不解析、不解码**：`Plan.Kind=Passthrough` 时直接把后端包转发给前端。
- **SingleShard 可以走 raw**：表名/库名改写不改变结果集形状，仍然零解码。
- **只有 MultiShard 解码**：把结果集变成 `Row` 后交给 `merge`；字符串比较按列
  collation（白名单 utf8mb4 族 + PAD SPACE，A11）。
- **Align 是会话状态到连接的重放点**：库、字符集、白名单会话变量
  （`time_zone`/`transaction_isolation`，含 unused→DEFAULT，A13）；结果未读完的
  连接在归还时淘汰（A12）。
- **Plan 不可变**：构建完成后只读，可被 `EXPLAIN ROUTE` 和后续 plan cache 复用。

## 1. 设计原则

1. **正确性优先于功能**：每类 SQL 构造只有三种合法状态——完全支持（跨分片语义正确）/
   可证明单分片（直通，行为与裸 MySQL 一致）/ 明确报错（带文档锚点）。
   永不静默返回错误结果。
2. **协议层是第一方代码**：MySQL 线协议的保真度是本项目差异化所在，可以参考
   go-mysql / vitess 的实现，但不做黑盒依赖。
3. **依赖单向、逐层可独立测试**：下层不知道上层存在。协议层可以在没有路由/分片的情况下
   单独跑差分测试（透传模式）。
4. **依赖面极小**：运行时直接依赖目标 < 10 个。
5. **一条数据通路**：`single` / `rwsplit` / `sharding` 只是三种拓扑与路由策略；
   执行、转发、粘性、归并只有一份代码。模式不是执行分支。
6. **先规划后执行**：SQL 先变成 `Plan`（目标集合 + 每目标 SQL + 归并提示），
   再由执行层消费；三态契约由 `Plan.Kind` 承载。
7. **拓扑在路由之外**：路由只产出 `SliceID`、角色、物理库与 SQL，不持有连接池。
   连接池属于资源层 `pkg/backend`。

## 2. 开源对照结论

调研了 kingshard、Gaea、dbpack、dbatman、Vitess、ShardingSphere 的模块划分与数据通路
（详细对照见 [design/module-map.md](../design/module-map.md) §1）。

| 项目 | 模式分流 | 计划抽象 | 拓扑归属 | 归并 | 结论 |
|---|---|---|---|---|---|
| kingshard | 无显式 mode，按表规则 | `Plan` 值对象 | `backend.Node` | 全缓冲 + 全排序 | 单入口，但会话是 god object |
| Gaea | 组合式（slices + rules） | `Plan.ExecuteIn(Executor)` | `backend.Slice` | 全缓冲 | 单入口 + Plan 接口 |
| dbpack | 三个 Executor（sdb/rws/shd） | `Optimizer → Plan` | `resource/group` | 全缓冲 + k 路 | 唯一的三份执行入口，复制明显 |
| dbatman | 单库主从 | 无 | `cluster` | 无 | 分层干净，但无分片 |
| Vitess | 单 Executor + VSchema/vindex | `Plan{Type, Primitive}` | keyspace/shard | 单路由快路径 + 归并 | 单管线 + 算子树 |
| ShardingSphere | 单 `SQLRouteEngine` + 规则插件 | `RouteContext` | `dataSources` + rules | `MergeEngine` | 五阶段 + 规则插件 |

五条结论：Plan 是分片的分水岭；拓扑与连接池必须在路由之外；单分片必须是独立快路径；
模式是规则/拓扑不是执行分支；归并普遍全缓冲。

**落地形态：一个 Executor + 一个 PlanBuilder + 三个 Router 实现 + `backend.Slice` 拓扑。**

## 3. 依赖规则（强制）

```
mysql、result、sql、session   ← 叶子/准叶子
backend                        → mysql、result
route                          → sql、session
plan                           → sql、route、session、sql/rewrite
merge                          → result、plan（仅 MergeSpec）、mysql/collation（stdlib-only 叶子）
execute                        → plan、backend、session、merge、result、mysql
proxy                          → mysql/server、sql、session、execute
config                         → constant、yaml
server                         → 全部（唯一装配点）
cmd                            → server、config、log
```

强制规则：

1. **只有 `pkg/server` 按 `mode` 分支**。`execute` / `proxy` / `session` / `backend` /
   `route` 里不出现 mode。
2. **`route` / `plan` 不 import `backend`**：只产出 SliceID 字符串和角色，不持有池。
3. **`execute` 是唯一 import `backend` 的数据面模块**；`proxy` 不 import `backend`。
4. **lease 归 `execute`，不归 `session`**：session 只表达"需要粘性"的意图。
5. **协议层不做 SQL 语义**：`mysql` 只负责帧与编码；`proxy` 只构造 `Statement`。
6. **只解析一次**：`Statement` 在 `proxy` 构造，AST 被 route/rewrite/plan 复用。
7. **N=1 不解码**：只有 MultiShard 进入行级归并。
8. **物理库在 `Target.DB`**：session 存逻辑库，route 产出每目标物理库。
9. **下层不 import 上层**：`mysql` / `result` / `sql` / `session` 不依赖运行时上层。
10. **config 只产 spec**：不建池、不 import 运行时模块。

禁止依赖的 CI 检查（建议）：

```sh
# route/plan 不得依赖 backend；session 不得依赖 backend
grep -rn "pkg/backend" pkg/route pkg/plan pkg/session
# 除 server/config 外不得出现 mode
grep -rn "ModeSingle\|ModeRWSplit\|ModeSharding" pkg/execute pkg/proxy pkg/session pkg/backend
# 语义判定不得退回字符串启发式
grep -rn "strings.Contains\|strings.HasPrefix\|strings.ToUpper" pkg/route pkg/plan pkg/execute --include='*.go' | grep -v _test
```

## 4. mode 与配置

`backend` 是后端拓扑段。三个 mode 值保留，语义为"拓扑预设 + Router 选择"。

| mode | 拓扑约束 | Router | 执行器 |
|---|---|---|---|
| `single` | 1 个 Slice，0 个 slave，无 sharding 段 | `source.AllMaster` | 同一个 |
| `rwsplit` | 1 个 Slice，≥1 个 slave，无 sharding 段 | `source.Pick` | 同一个 |
| `sharding` | ≥1 个 Slice，≥1 条规则；Slice 可带 slave | `shard.Route` + 角色策略 | 同一个 |

## 5. 三态契约与分片边界

`Plan.Kind` 承载三态：

| Kind | 含义 | 结果路径 |
|---|---|---|
| `Passthrough` | 原文、单目标、无需改写 | raw packet 直通 |
| `SingleShard` | 可证明单分片，可能改写库/表名 | raw packet 直通 |
| `MultiShard` | 多目标，需要归并 | decode + merge |
| `Reject` | 无法证明安全，明确报错 | 稳定错误码 + 文档锚点 |

路由只接受能证明单分片的谓词（分片键等值、同片 `IN`、单区间），证明不了就 `Reject`，
**不 scatter、不猜**。

## 6. 测试映射

| 模块 | L0 单元 | L1 fuzz/property | L2 集成 | L3 差分 | L4 变形 |
|---|---|---|---|---|---|
| `pkg/mysql/*` | codec 往返 | packet/auth fuzz、scanner fuzz | auth、charset、prepare | **透传即差分** | — |
| `pkg/backend` | 池/租约状态机 | — | 故障注入 | — | — |
| `pkg/sql` | 分类/解析 | parser 不 panic | — | 改写后语义 | — |
| `pkg/route` | 规则→目标（纯函数） | 路由确定性 | — | — | 分片 vs 单库 |
| `pkg/plan` | Kind/Reject/Explain | — | — | — | — |
| `pkg/execute` | fake backend/stream | — | 多分片下发 | — | — |
| `pkg/merge` | identity/order/agg/collation | — | — | — | **分片 vs 单库（聚合 + collation oracle）** |
| `pkg/config` | 校验/归一化 | — | 热加载不中断 | — | — |

测试原则：route/plan 不依赖 Docker/MySQL（表驱动单测为主要正确性门禁）；execute 用
fake backend + 脚本化 Stream；差分保持全绿；变形 harness 是跨分片功能的准入门槛。

## 7. v1 明确不做

在线扩缩容（手动迁移 + 校验工具替代）、SQL 防火墙、数据加解密、影子库、自研 parser、
gnet/evio 事件循环（goroutine-per-connection 足够）、跨分片 2PC 与多分片写
（写必须单分片，见 [design/sharding-p0-design.md](../design/sharding-p0-design.md) §6.3）、
多租户 namespace。

## 8. 里程碑

| 里程碑 | 内容 | 退出条件 |
|---|---|---|
| **M1 透传** ✅ | 协议、池、passthrough、差分 harness | 已达成 |
| **M1.5 模块化 seam** ✅ | `backend.Slice`、`route/source`、`plan`/`result`/`execute` 骨架；删 sharding 桩 | 差分全绿，行为不变 |
| **M2 会话与单分片** ✅ | `Session` 状态、`backend` 配置、单分片路由、`Reject`、`EXPLAIN ROUTE` | 路由表驱动单测 + 单分片 e2e |
| **M3 跨分片** ✅ | MultiShard、dispatch、identity/order/aggregate/limit、变形 harness；超出设计的增量：collation 归并、池生命周期（脏即弃）、会话变量重放、语句判定单真相源 | 分片 vs 单库一致（聚合 + collation 矩阵 oracle 全绿） |
| **M4 生产化**（未开始） | 健康检查、故障回落、限流/指标、容量与性能门禁；第一切片 M4a（运维面）设计已就绪：[design/ops-plane-design.md](../design/ops-plane-design.md) | 故障注入 + 性能门禁 |

## 9. 决策记录

| ID | 决策 | 状态 |
|---|---|---|
| A1 | 单管线 + 单 Executor + 三 Router，不做三 Executor | locked |
| A2 | 配置段名 `backend`，包名 `pkg/backend` | locked（YAML）/ proposed（包名） |
| A3 | 统一 `slices` 拓扑，`mode` 只做校验预设 | proposed |
| A4 | lease 归 `execute`，session 只存意图 | locked |
| A5 | route/plan 纯函数，不持有池 | locked |
| A6 | P3 只做"单分片可证明 + 拒绝跨片" | locked |
| A7 | N=1 走 raw packet，不 Decode | locked |
| A8 | 物理库映射放 `Target.DB` | locked |
| A9 | collation 引擎第一方化拷贝 vitess（`pkg/mysql/collation`，stdlib-only，commit 锁定），不引 module 依赖 | locked |
| A10 | 列元数据保真：后端拨号后固定 `character_set_results=NULL`；`SET NAMES utf8mb4` 本地应答不透传；Align 用 `character_set_client/connection` | locked |
| A11 | 归并 collation 白名单：utf8mb4 族（general_ci/0900_ai_ci/bin）+ binary，PAD SPACE 语义；用户不可切换 collation（utf8mb4-only） | locked |
| A12 | 池生命周期：不再无条件 `COM_RESET_CONNECTION`；未读尽/出错/不可复现会话状态的连接淘汰，干净连接直接归还 | locked |
| A13 | 会话变量白名单重放：`time_zone`/`transaction_isolation`（diff + unused→DEFAULT，Gaea 模式）；白名单外 1235 | locked |
| A14 | 语句级语义单真相源：token 扫描一次产出 `Class`（AST 为超集）；`ParseAST` 并入 `Sharding` 会话开关 | locked |

## 10. 参考

- [Vitess](https://github.com/vitessio/vitess)：`go/vt/vtgate/engine/plan.go`、`engine/route.go`、`executor.go`、`scatter_conn.go`
- [ShardingSphere](https://github.com/apache/shardingsphere)：`infra/route`、`infra/rewrite`、`infra/executor`、`infra/merge`、`features/sharding`
- [Gaea](https://github.com/XiaoMi/Gaea)：`proxy/plan`、`proxy/router`、`backend/slice.go`
- [kingshard](https://github.com/flike/kingshard)：`proxy/router/planbuilder.go`、`proxy/server/conn_select.go`
- [dbpack](https://github.com/CECTC/dbpack)：`pkg/optimize`、`pkg/plan`、`pkg/executor`
- [dbatman](https://github.com/bytedance/dbatman)：`proxy`、`database/cluster`、`parser`
