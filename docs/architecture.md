# prism 架构设计（v0.2：模块化形态）

> 状态：讨论稿。模块形态已定；模块边界、接口与配置细节以
> [design/module-map.md](design/module-map.md) 为准（该文件当前位于被 gitignore 的
> `docs/design/`，提交时需 `git add -f`）。
> 本文是项目的 north star；实现偏离此处记录的设计时先改文档。
> 日期：2026-10-31
> 模块路径：`github.com/acorndb/prism`

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

调研了 kingshard、Gaea、dbpack、dbatman、Vitess、ShardingSphere 的模块划分与数据通路。
详细对照见 [design/module-map.md](design/module-map.md) §1。

| 项目 | 模式分流 | 计划抽象 | 拓扑归属 | 归并 | 结论 |
|---|---|---|---|---|---|
| kingshard | 无显式 mode，按表规则 | `Plan` 值对象 | `backend.Node` | 全缓冲 + 全排序 | 单入口，但会话是 god object |
| Gaea | 组合式（slices + rules） | `Plan.ExecuteIn(Executor)` | `backend.Slice` | 全缓冲 | 单入口 + Plan 接口 |
| dbpack | 三个 Executor（sdb/rws/shd） | `Optimizer → Plan` | `resource/group` | 全缓冲 + k 路 | **唯一的三份执行入口，复制明显** |
| dbatman | 单库主从 | 无 | `cluster` | 无 | 分层干净，但无分片 |
| Vitess | 单 Executor + VSchema/vindex | `Plan{Type, Primitive}` | keyspace/shard | 单路由快路径 + 归并 | 单管线 + 算子树 |
| ShardingSphere | 单 `SQLRouteEngine` + 规则插件 | `RouteContext` | `dataSources` + rules | `MergeEngine` | 五阶段 + 规则插件 |

五条结论：

1. **Plan 是分片的分水岭**：所有支持分片的项目都有独立的计划/路由产物；
   没有 Plan 的 dbatman 也正好没有分片。
2. **拓扑与连接池必须在路由之外**：Gaea 的 `backend.Slice`、kingshard 的 `backend.Node`、
   dbpack 的 resource/group 都是这个结构；prism 当前的 `router.Group` 是反例。
3. **单分片必须是独立快路径**：Gaea 的 `continueConn`、dbpack 的 `QueryOnSingleDBPlan`；
   kingshard 不分单/多分片，单分片也走全量归并，是性能坑。
4. **模式是规则/拓扑，不是执行分支**：Vitess、ShardingSphere、Gaea、kingshard、dbatman
   都是单入口 + 策略；只有 dbpack 用三 Executor，代价是管线复制。
5. **归并普遍全缓冲**：dbpack 的 k 路 + LIMIT 下推做得最好，但聚合仍不完整。
   prism 的接口按流式设计，P0 只实现 identity，跨分片归并后置。

结论：**一个 Executor + 一个 PlanBuilder + 三个 Router 实现 + `backend.Slice` 拓扑。**
不是三个 Plan，也不是三个 Executor。

## 3. 模块划分（目标态）

```
prism/
├── cmd/prism/                 # 入口：装配、信号处理、优雅退出（单二进制）
├── pkg/
│   ├── mysql/                 # 【协议层】第一方代码
│   │   ├── packet/            #   帧读写、长度编码
│   │   ├── auth/              #   握手、native/caching_sha2
│   │   ├── server/            #   前端握手与连接包装
│   │   └── client/            #   后端 raw conn 握手
│   ├── config/                # 【配置层】YAML schema + 严格校验 + 快照
│   ├── backend/               # 【资源层】Conn / Pool / Slice / Cluster
│   ├── session/               # 【会话层】纯状态 + StmtTable（不持有连接）
│   ├── sql/                   # 【语句层】parser 包装 + Statement 分类 + rewrite
│   │   └── rewrite/           #   按目标改写 SQL（P2 后）
│   ├── route/                 # 【路由层】纯函数：Target + source + shard
│   │   ├── source/            #   主从选源（single / rwsplit）
│   │   └── shard/             #   分片规则（hash/range）
│   ├── plan/                  # 【计划层】Plan + Builder + Explain
│   ├── result/                # 【结果层】Field / Row / Stream（raw 或 decoded）
│   ├── execute/               # 【执行层】dispatch / lease / response writer
│   ├── merge/                 # 【归并层】identity / order / aggregate
│   ├── proxy/                 # 【会话编排】COM 循环，只做协议与会话字段
│   └── server/                # 【进程层】监听、Accept、唯一 mode 装配点
├── etc/                       # 示例配置
├── docs/
└── test/
    ├── differential/          #   协议差分：裸 MySQL vs proxy
    ├── metamorphic/           #   分片 vs 单库结果一致（P4）
    └── e2e/                   #   testcontainers 集成用例
```

后置模块（P4 之后再建，不建空包）：`pkg/observability`、`pkg/admin`。

重构映射（R1 已完成前三行；`pkg/backend_server` / `pkg/router` 已删除）：

| 重构前 | 目标 | 阶段 |
|---|---|---|
| `pkg/backend_server`（Backend/Pool） | `pkg/backend`（Conn/Pool/Slice/Exec/Cluster） | **R1/R3/R6 完成** |
| `pkg/router.Group` | `pkg/backend.Slice` | **R1 完成** |
| `pkg/router.Pick` / `ClassifyWantSlave` | `pkg/route/source` | **R1 完成** |
| `pkg/executor.passthrough` | `pkg/execute` Pipeline | **R3 完成** |
| `pkg/executor.shardingExecutor` 桩 | 删除；sharding 变成 Router | **R3 完成** |
| `pkg/executor/forward.go` | `pkg/execute` + `pkg/backend.Conn` | **R3 完成** |
| `pkg/session.State` | `pkg/session.State` + `StmtTable` | P3（StmtTable 随 prepare 落地） |
| `pkg/proxy` 内的 USE/SET 解析 | `pkg/sql` 统一成 `Statement` | **R4 完成** |
| `pkg/config.BackendServer` | `pkg/config.BackendConfig`（YAML `backend`） | **R5 完成** |
| `pkg/sql/parser` | 增加 `pkg/sql.Statement` 与分类 | **R2 完成 Statement 骨架** |
| 无 Plan / 无 EXPLAIN ROUTE | `pkg/plan` + `EXPLAIN ROUTE` | **R2 完成 Plan 骨架**；EXPLAIN P3 |

## 4. 数据通路

```
client
  │
  ▼
pkg/mysql/server + pkg/proxy          # 握手、COM 循环、构造 sql.Statement
  │
  ▼
pkg/execute.Handle
  │
  ├─ pkg/plan.Build
  │     ├─ pkg/route.Route → []Target{Slice, Role, DB, SQL}
  │     └─ pkg/sql/rewrite（按目标改写，P2 后）
  │
  ├─ for each Target:
  │     pkg/backend.Cluster.Get(slice).Acquire(role)
  │       → Conn.Align(DB, charset, vars)
  │       → Conn.Exec(DB, SQL) → pkg/result.Stream
  │
  ├─ pkg/merge.Merge（仅 MultiShard）
  │
  └─ pkg/mysql 写回 client（N=1 走 raw packet 零拷贝）
```

说明：

- **N=1 Passthrough 不解析、不解码**：`Plan.Kind=Passthrough` 时直接把后端包转发给前端。
- **SingleShard 可以走 raw**：表名/库名改写不改变结果集形状，仍然零解码。
- **只有 MultiShard 解码**：把结果集变成 `Row` 后交给 `merge`。
- **Plan 不可变**：构建完成后只读，可被 `EXPLAIN ROUTE` 和后续 plan cache 复用。

## 5. 依赖规则（强制）

```
mysql、result、sql、session   ← 叶子/准叶子
backend                        → mysql、result
route                          → sql、session
plan                           → sql、route、session、sql/rewrite
merge                          → result、plan（仅 MergeSpec）
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
```

## 6. mode 与配置

`backend` 是后端拓扑段（原 `backend_server`）。三个 mode 值保留，语义调整为
"拓扑预设 + Router 选择"，不再选择执行器。

```yaml
backend:
  mode: sharding

  slices:
    - name: s0
      master: { address: "10.0.0.1:3306", user: proxy, password: "***" }
      slaves:
        - { address: "10.0.0.2:3306", user: proxy, password: "***" }
    - name: s1
      master: { address: "10.0.0.3:3306", user: proxy, password: "***" }

  load_balance: round_robin

  sharding:
    rules:
      - table: orders
        key: user_id
        algorithm: hash_mod
        slices: [s0, s1]
```

| mode | 拓扑约束 | Router | 执行器 |
|---|---|---|---|
| `single` | 1 个 Slice，0 个 slave，无 sharding 段 | `source.AllMaster` | 同一个 |
| `rwsplit` | 1 个 Slice，≥1 个 slave，无 sharding 段 | `source.Pick` | 同一个 |
| `sharding` | ≥1 个 Slice，≥1 条规则；Slice 可带 slave | `shard.Route` + 角色策略 | 同一个 |

配置细节（结构体、校验、热更门禁）见
[design/module-map.md](design/module-map.md) §6。

## 7. 三态契约与分片边界

`Plan.Kind` 承载三态：

| Kind | 含义 | 结果路径 |
|---|---|---|
| `Passthrough` | 原文、单目标、无需改写 | raw packet 直通 |
| `SingleShard` | 可证明单分片，可能改写库/表名 | raw packet 直通 |
| `MultiShard` | 多目标，需要归并 | decode + merge（P4） |
| `Reject` | 无法证明安全，明确报错 | 稳定错误码 + 文档锚点 |

P0/P3 只实现 `Passthrough` / `SingleShard` / `Reject`；`MultiShard` 在 P4 打开。
P3 的路由只接受能证明单分片的谓词（分片键等值、同片 `IN`、单区间），
证明不了就 `Reject`，**不 scatter、不猜**。

## 8. 模块与测试映射

| 模块 | L0 单元 | L1 fuzz/property | L2 集成 | L3 差分 | L4 变形 |
|---|---|---|---|---|---|
| `pkg/mysql/*` | codec 往返 | packet/auth fuzz | auth、charset、prepare | **透传即差分** | — |
| `pkg/backend` | 池/租约状态机 | — | 故障注入 | — | — |
| `pkg/sql` | 分类/解析 | parser 不 panic | — | 改写后语义 | — |
| `pkg/route` | 规则→目标（纯函数） | 路由确定性 | — | — | 分片 vs 单库 |
| `pkg/plan` | Kind/Reject/Explain | — | — | — | — |
| `pkg/execute` | fake backend/stream | — | 多分片下发 | — | — |
| `pkg/merge` | identity/order/agg | — | — | — | 分片 vs 单库 |
| `pkg/config` | 校验/归一化 | — | 热加载不中断 | — | — |

测试原则：

- **route/plan 不依赖 Docker/MySQL**：表驱动单测是主要正确性门禁。
- **execute 用 fake backend + 脚本化 Stream**：不依赖真实 MySQL。
- **差分保持全绿**：P1/P2 重构每一步都必须跑现有 `test/differential`。
- **变形 harness 在 P4 前建好**：分片 vs 单库结果一致，是跨分片功能的准入门槛。

## 9. v1 明确不做

在线扩缩容（做手动迁移 + 校验工具替代）、SQL 防火墙、数据加解密、影子库、
自研 parser、gnet/evio 事件循环（goroutine-per-connection 足够）、
跨分片 2PC（P0–P3 明确拒绝跨片写）、多租户 namespace。

## 10. 里程碑

| 里程碑 | 内容 | 退出条件 |
|---|---|---|
| **M1 透传**（当前） | 协议、池、passthrough、差分 harness | 已有 P0–P2 能力 |
| **M1.5 模块化 seam** | `backend.Slice`、`route/source`、`plan`/`result`/`execute` 骨架；删 sharding 桩 | 差分全绿，行为不变 |
| **M2 会话与单分片** | `Session` 状态、`backend` 配置、单分片路由、`Reject`、`EXPLAIN ROUTE` | 路由表驱动单测 + 单分片 e2e |
| **M3 跨分片** | MultiShard、dispatch、identity/order/aggregate/limit、变形 harness | 分片 vs 单库一致 |
| **M4 生产化** | 健康检查、故障回落、限流/指标、容量与性能门禁 | 故障注入 + 性能门禁 |

M1.5/M2 的 PR 级拆分、文件动作与测试迁移见
[design/refactor-seam-plan.md](design/refactor-seam-plan.md)。

## 11. 决策记录

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

## 12. 参考

- [Vitess](https://github.com/vitessio/vitess)：`go/vt/vtgate/engine/plan.go`、
  `engine/route.go`、`executor.go`、`scatter_conn.go`
- [ShardingSphere](https://github.com/apache/shardingsphere)：`infra/route`、
  `infra/rewrite`、`infra/executor`、`infra/merge`、`features/sharding`
- [Gaea](https://github.com/XiaoMi/Gaea)：`proxy/plan`、`proxy/router`、`backend/slice.go`
- [kingshard](https://github.com/flike/kingshard)：`proxy/router/planbuilder.go`、
  `proxy/server/conn_select.go`
- [dbpack](https://github.com/CECTC/dbpack)：`pkg/optimize`、`pkg/plan`、`pkg/executor`
- [dbatman](https://github.com/bytedance/dbatman)：`proxy`、`database/cluster`、`parser`
- 本地调研 checkout：`/tmp/prism-oss-survey/{kingshard,Gaea,dbpack,dbatman,vitess}`
