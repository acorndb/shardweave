# 模块地图与依赖（module-map）

> 状态：讨论稿 v0.1
> 日期：2026-10-31
> 上位：[docs/architecture.md](../architecture.md)
> 范围：目标模块形态、边界、依赖、核心接口、配置与落地顺序。**本文不含代码实现。**
>
> 提交提示：`docs/design/` 当前被 `.gitignore` 忽略；本文件需
> `git add -f docs/design/module-map.md`。建议后续把 ignore 规则收窄到过程产物
> （如 `p0-implementation-plan.md`），让设计文档默认入仓。

## 0. 一句话

**一个 Executor + 一个 PlanBuilder + 三个 Router 实现 + `backend.Slice` 拓扑。**
`single` / `rwsplit` / `sharding` 是三种拓扑预设与路由策略，不是三个执行器；
执行、转发、粘性、归并只有一份代码。

## 1. 调研结论（为什么是这个形态）

完整对照见 [architecture.md §2](../architecture.md#2-开源对照结论)。关键证据：

| 项目 | 事实 | 对 prism 的含义 |
|---|---|---|
| Vitess | 一个 `Executor`；`engine.Plan{Type, Instructions Primitive}`；`PlanType` 含 `PlanPassthrough/PlanMultiShard/PlanScatter/PlanJoinOp` | 模式差异在 Plan 类型与 Primitive 树，不在执行器 |
| ShardingSphere | `SQLRouteEngine → RouteContext → Rewrite → ExecutorEngine → MergeEngine`；分片是 `ShardingSQLRouter` 规则插件 | 五阶段 + 规则插件；模式不是执行分支 |
| Gaea | 一个 `SessionExecutor`；`Plan.ExecuteIn(Executor)`；`backend.Slice` 封装主从与池 | 单入口 + Plan 接口；拓扑在资源层 |
| kingshard | 一个 `ClientConn`；`Plan` 值对象 + `RewrittenSqls`；`backend.Node` | 单入口，但会话是 god object；全缓冲归并是反面教材 |
| dbpack | 三个 Executor（sdb/rws/shd），各自复制 pre/post filter 与 stmt 分支 | 唯一的三 Executor；复制是结构性代价 |
| dbatman | 一个 `Session.Executor(isread)`；无 Plan/分片 | 分层干净，但没有分片，不能作为分片形态参考 |
| MyCat2 | Calcite（`hbt`）+ `router` + NIO handler | 重型优化器不是 P0 必需 |
| DBLE | `MultiNodeQueryHandler` / `MergeNodeHandlerBuilder` 等 handler/builder | 按 SQL 形态堆 handler 会膨胀；阶段接口更稳 |

结论：

1. **Plan 是 SQL 与执行之间的唯一契约。** 没有 Plan 的项目要么没有分片，要么把路由散在会话里。
2. **拓扑与池在路由之外。** 路由只产出 `SliceID`/角色/物理库/SQL。
3. **单分片是独立快路径。** 不能因为引入 Plan/Stream 就让单分片也解码归并。
4. **模式是拓扑预设。** 只有装配层知道 mode；执行层永远只看到 Plan。
5. **归并接口流式，实现可后置。** P0–P3 只需要 identity；MultiShard 在 P4。

## 2. 目标与非目标

### 2.1 目标

- 为分片库（后续分表）准备稳定的模块边界，后续加功能是"加 Router/Plan 分支"，不是改执行器。
- 三种 mode 共用同一条执行路径，行为差异可被纯函数单测覆盖。
- 路由/计划/归并可以在没有 Docker、没有真实 MySQL 的情况下单测。
- N=1 透传保持零解析、零解码（协议保真与性能）。
- 配置严格：不支持的字段/组合直接拒绝，不留"写了不生效"的字段。

### 2.2 非目标（本阶段）

- 跨分片 `MultiShard` 执行与归并（P4）。
- 跨片事务 / 2PC / 全局事务。
- 在线扩缩容、数据迁移。
- 多租户 namespace（Gaea 式 manager）。
- 管理面 / 可观测性（后置模块）。
- 表名改写（P3 先做库级路由；表级改写 P4）。

## 3. 模块清单

| 模块 | 职责 | 拥有 | 绝不能做 | 允许 import |
|---|---|---|---|---|
| `pkg/mysql` | 帧读写、握手/auth、OK/ERR/结果集编码、命令常量 | `packet.Conn`、`auth`、`server.Conn`、`client.Conn`、`EncodeOK/ERR` | SQL 语义、路由、会话、池 | stdlib |
| `pkg/config` | YAML schema、严格校验、默认值、快照/热更门禁 | `Config`、`BackendConfig`、`Runtime` | 建池、import 运行时模块 | `constant`、yaml |
| `pkg/backend` | 后端资源层：连接、池、Slice、Cluster | `Conn`、`Pool`、`Slice`、`Cluster`、`PoolConfig` | 解析 SQL、认识分片规则、持有 session | `mysql`、`result`、`constant` |
| `pkg/session` | 前端会话意图与 prepared statement 表 | `State`、`StmtTable`、`Stmt` | 持有连接/lease、路由、执行 | `constant` |
| `pkg/sql` | 语句理解：parser 包装、`Statement`、分类、rewrite | `Statement`、`Kind`、`Parse`、`rewrite` | 认识拓扑/规则/连接 | tidb parser |
| `pkg/route` | 纯路由：目标集合 | `Target`、`Router`、`source`、`shard` | acquire、执行、持有池、改写结果 | `sql`、`session`、`constant` |
| `pkg/plan` | 计划：三态 + 每目标 SQL + 归并提示 | `Plan`、`Kind`、`Builder`、`MergeSpec`、`Explain` | 执行、持有池 | `sql`、`sql/rewrite`、`route`、`session` |
| `pkg/result` | 逻辑结果：字段/行/流 | `Field`、`Row`、`Stream` | 认识协议、路由、连接 | stdlib |
| `pkg/merge` | 结果归并 | `Merger`、identity/order/aggregate | acquire、执行 | `result`、`plan`（MergeSpec） |
| `pkg/execute` | 执行引擎：dispatch、lease、写回 | `Runtime`、`Executor`、`lease` | 解析 SQL、决定路由、按 mode 分支 | `plan`、`backend`、`session`、`merge`、`result`、`mysql` |
| `pkg/proxy` | 前端 COM 循环与会话字段 | `Serve`、`SessionConfig` | USE/SET 语义、路由、acquire | `mysql/server`、`mysql`、`sql`、`session`、`execute` |
| `pkg/server` | 监听、Accept、唯一装配点 | `Server`、`New`、mode→Cluster/Router/Executor | 参与单条 SQL 执行 | 全部（装配例外） |
| `cmd/prism` | 入口、flag、信号、优雅退出 | `main` | 业务逻辑 | `server`、`config`、`log` |

命名提醒：

- `pkg/server` 是**进程/监听/装配层**；`pkg/mysql/server` 才是协议前端。
- `pkg/backend` 里不要再用 `Backend` 接口名，避免 `backend.Backend`；接口叫 `Conn`。
- `pkg/config` 里不要用 `BackendServer`；段名 `backend`，类型 `BackendConfig`。

### 3.1 现状与目标

| 模块 | 现状 | 目标 |
|---|---|---|
| `pkg/mysql` | 已实现，协议保真 | 不动 |
| `pkg/backend` | 已落地 `Conn`/`Pool`/`Slice`（R1） | 增加 `Cluster` 与多 Slice 装配（R6） |
| `pkg/session` | 只有 database/inTx/autocommit/reserved/charset | 增加变量；`StmtTable` 在 P3 落地 |
| `pkg/sql` | `Statement` 骨架（R2）+ tidb parser 包装 | 分类 + AST 上数据面 |
| `pkg/route` | `source` 已拆出（R1），产出单目标 | 新增 `shard`；Target 带物理库/改写 SQL |
| `pkg/plan` | N=1 `Passthrough` Plan 骨架（R2） | `EXPLAIN ROUTE` + `SingleShard`/`Reject`（P3） |
| `pkg/result` | 不存在 | Field/Row/Stream；raw 与 decoded 两形态 |
| `pkg/execute` | 现为 `pkg/executor`：passthrough 已走 N=1 Plan；sharding 桩 | 单执行器；R3 改名 execute；R6 lease 集合 |
| `pkg/merge` | 不存在 | identity；P4 order/aggregate |
| `pkg/proxy` | COM 循环 + USE/SET/autocommit 字符串解析 | 只做 COM 循环与 `Statement` 构造 |
| `pkg/config` | `backend_server` 字段拍平 | `backend` + `slices` + `sharding.rules` |
| `pkg/server` | 按 mode 手工建 Group + executor | 唯一 mode 分支；装配 Cluster/Router/Builder/Executor |

## 4. 依赖规则

### 4.1 依赖图

```
                         cmd/prism
                             │
                       pkg/server            ← 唯一装配点
        ┌──────────────┬───┴────────────┬──────────────┐
        ▼              ▼                ▼              ▼
   pkg/proxy     pkg/execute        pkg/config     pkg/backend
        │              │
        │              ├──► pkg/plan ──► pkg/route ──► pkg/sql
        │              │        │            │
        │              │        └────────────┴──► pkg/session
        │              ├──► pkg/merge ──► pkg/result
        │              └──► pkg/result
        └──► pkg/mysql/server
```

### 4.2 强制规则

- **D1 模式只在装配层。** 只有 `pkg/server` 读取/分支 `mode`。`execute`、`proxy`、
  `session`、`backend`、`route`、`plan`、`merge` 的代码里不出现 `ModeSingle` 等常量。
- **D2 路由与计划是纯函数。** `route` / `plan` 不 import `backend`；只产出 `SliceID`
  字符串、角色、物理库、SQL。不得持有 `*Pool` / `*Slice` / `Conn`。
- **D3 执行层是唯一持有 lease 的层。** `execute` 负责 acquire / align / release / discard；
  `session` 只表达 `NeedsSticky()` 的意图。这样 route/plan 可以只依赖纯状态。
- **D4 协议层不做 SQL 语义。** `mysql` 只编解码；`proxy` 只把 COM 包变成 `Statement`；
  USE/SET/autocommit 的识别在 `sql`，状态变更在 `session`/`execute`。
- **D5 只解析一次。** `Statement` 在 `proxy` 构造；route/rewrite/plan 复用 `AST`。
  禁止每层再 `strings.ToUpper` 或重新 parse。
- **D6 N=1 零解码。** `Passthrough` / `SingleShard` 走 raw packet 直通；只有
  `MultiShard` 才把结果解成 `Row` 交给 `merge`。
- **D7 物理库在 `Target.DB`。** `session` 存逻辑库；`route` 产出每个目标的物理库；
  `execute` 用 `Target.DB` 对齐后端。
- **D8 下层不 import 上层。** `mysql`、`result`、`sql`、`session` 不依赖运行时上层；
  `backend` 不依赖 route/plan/execute。
- **D9 config 只产 spec。** `config` 不建池、不 import `backend`/`route`/`execute`；
  `server` 负责把 spec 转成运行时对象。
- **D10 单目标不经过 merge。** `execute` 对 `len(Targets)==1` 直接写回；只有
  `MultiShard` 调用 `merge.Merger`。

### 4.3 禁止依赖表

| 包 | 禁止 import |
|---|---|
| `pkg/mysql` | 所有 prism 上层 |
| `pkg/result` | 所有 prism 模块 |
| `pkg/sql` | 所有 prism 运行时（parser 依赖除外） |
| `pkg/session` | `backend`、`route`、`plan`、`execute`、`proxy` |
| `pkg/backend` | `session`、`route`、`plan`、`execute`、`proxy`、`config` |
| `pkg/route` | `backend`、`plan`、`execute`、`proxy`、`config` |
| `pkg/plan` | `backend`、`execute`、`proxy`、`config` |
| `pkg/merge` | `backend`、`execute`、`proxy`、`config` |
| `pkg/execute` | `proxy`、`config`、`route`（目标类型经 `plan` 传递） |
| `pkg/proxy` | `backend`、`plan`、`route`、`merge`、`config` |
| `pkg/config` | 所有运行时模块 |
| `pkg/server` | 无（装配例外，但不得被任何包 import，除 `cmd`） |

建议在 CI 里加两条 grep：

```sh
grep -rn "pkg/backend" pkg/route pkg/plan pkg/session && exit 1
grep -rn "ModeSingle\|ModeRWSplit\|ModeSharding" \
  pkg/execute pkg/proxy pkg/session pkg/backend pkg/route pkg/plan && exit 1
```

## 5. 核心接口

以下为**目标接口草案**，以 P1/P2 落地时的 TDD 为准。`locked` 表示形态已定，
`proposed` 表示字段/命名可讨论。

### 5.1 Statement（pkg/sql，proposed）

```go
type Kind uint8

const (
    KindUnknown Kind = iota
    KindQuery          // SELECT / SHOW / EXPLAIN
    KindDML            // INSERT / UPDATE / DELETE / REPLACE
    KindDDL            // CREATE / ALTER / DROP / TRUNCATE
    KindSet            // SET ...
    KindUse            // USE ...
    KindExplainRoute   // EXPLAIN ROUTE ...
)

type Statement struct {
    Cmd  byte          // COM_QUERY / COM_STMT_EXECUTE ...
    Raw  string        // 原文（COM_QUERY 时）
    AST  ast.StmtNode  // 可空：命令类或解析失败
    Kind Kind
    Err  error         // 解析失败时保留，供 PlanReject 使用
}
```

规则：`proxy` 只构造一次；`route`/`rewrite`/`plan` 复用 `AST`；不得二次 parse。

### 5.2 Target / Router（pkg/route，locked）

```go
type Target struct {
    Slice string         // 逻辑分片名（Slice.Name），不是 *Pool
    Role  constant.Role  // master | slave
    DB    string         // 物理库（session 里是逻辑库）
    SQL   string         // 原文或改写后
}

type Router interface {
    Route(ctx context.Context, st *session.State, stmt *sql.Statement) ([]Target, error)
}
```

三个实现：

- `source.AllMaster`：single；恒一个 Slice、RoleMaster。
- `source.Pick`：rwsplit；一个 Slice、按会话/语句选 master/slave。
- `shard.Route`：sharding；按规则产出 1..N 个 Target；P3 只允许 1 个，
  否则返回 `Reject`。

### 5.3 Plan（pkg/plan，locked）

```go
type Kind uint8

const (
    Passthrough Kind = iota // 原文、单目标、零改写
    SingleShard             // 可证明单分片，可能改写库/表
    MultiShard              // 多目标，需要归并
    Reject                  // 明确拒绝
)

type Plan struct {
    Kind    Kind
    Targets []route.Target
    Merge   MergeSpec
    Reject  *RejectError
}

type Builder interface {
    Build(ctx context.Context, st *session.State, stmt *sql.Statement) (*Plan, error)
}

// MergeSpec 是归并提示；P3 只使用空值。OrderBy/Limit/Aggregate
// 的具体字段在 P4 设计文档展开。
type MergeSpec struct {
    OrderBy    []OrderBy
    Limit      *Limit
    Aggregates []Aggregate
    GroupBy    []int
    Distinct   bool
}
```

`Plan` 构建后不可变；`EXPLAIN ROUTE` 直接打印它，不另写一套路由逻辑。

### 5.4 Result / Stream（pkg/result，proposed）

```go
type Field struct {
    Name, Table, DB string
    Type            byte
    Flags           uint16
    Charset         uint16
}

type Row []Value

type Stream interface {
    Fields() []Field
    Next() (Row, error) // io.EOF 结束
    Close() error
}
```

两种实现：

- **RawStream**：由 `pkg/backend` 实现，持有 packet 连接；N=1 时直接转发，
  不构造 `Row`。`ForwardTo(front *packet.Conn) error` 是**可选接口**，由
  `execute` 做类型断言，不塞进 `result.Stream`（`result` 不依赖 `mysql`）。
- **RowStream**：`MultiShard` 时解码出行；`merge` 消费它。

RawStream 可以延迟实现 `Fields/Next`；`execute` 在 N=1 时只调用 `ForwardTo`，
不触碰行接口。

`pkg/result` 不 import `pkg/mysql`；字段类型/标志用逻辑值，编码由 `execute` 完成。

### 5.5 Backend（pkg/backend，proposed）

```go
type AlignSettings struct {
    Database string
    Charset  string
}

type ExecRequest struct {
    Payload []byte // 完整命令包（含 cmd 字节）
    Prepare bool   // COM_STMT_PREPARE 响应
    NoReply bool   // COM_STMT_CLOSE 等无响应命令
}

type Conn interface { // 原 backend_server.Backend
    Exec(ctx context.Context, req ExecRequest) (result.Stream, error)
    Align(ctx context.Context, s AlignSettings) error
    Reset(ctx context.Context) error
    Close() error
}

type Slice struct {
    Name   string
    // master/slaves/pool/LB 内部持有
}

// R1 暂时保留 (Conn, *Pool, error)；R6 引入 Cluster/lease 时收敛为 (Conn, error)。
func (s *Slice) Acquire(ctx context.Context, role constant.Role) (Conn, *Pool, error)

type Cluster struct { /* map[string]*Slice */ }

func (c *Cluster) Get(name string) *Slice
func (c *Cluster) Names() []string
```

`AlignSettings` 包含物理库、charset、需要对齐的会话变量；由 `execute` 从
`session.State + route.Target` 组装。`backend` 不认识 `session` 类型。
`Exec` 只负责发完整命令包并返回响应流；OK/ERR/结果集/prepare/no-reply 的包处理
仍由 `pkg/mysql` 的 forward 函数完成。

### 5.6 Session（pkg/session，proposed）

```go
type State struct {
    // database、charset、autocommit、inTx、reserved、vars
}

type Stmt struct {
    ID       uint32
    SQL      string
    AST      ast.StmtNode
    Params   uint16
    Targets  map[string]uint32 // slice -> 后端 stmt id（P3）
}

type StmtTable struct { /* map[uint32]*Stmt */ }
```

`session` 不持有连接/lease；`NeedsSticky()` 只表达意图，由 `execute` 决定 lease 生命周期。
`StmtTable` 在 P3 与 sharding prepare 一起落地；R4 不引入未使用类型。

### 5.7 Executor / Runtime（pkg/execute，locked）

```go
type Runtime struct {
    Front *packet.Conn
    State *session.State
    lease *lease // single until R6; R6 → leases map[string]*lease
}

type Executor interface {
    Handle(ctx context.Context, rt *Runtime, stmt *sql.Statement) error
    Release(rt *Runtime, discard bool) error
    Close() error
}
```

`Handle` 内部：`plan.Build → acquire/align → Conn.Exec → rawForwarder.ForwardTo`。
这是唯一 import `backend` 的数据面模块。R3 用 fake `Conn`/`Stream` 做单测，
不依赖真实 MySQL；R6 把单条 lease 扩成按 Slice 的 map。

### 5.8 Merger（pkg/merge，proposed）

```go
type Merger interface {
    Merge(ctx context.Context, streams []result.Stream, spec plan.MergeSpec) (result.Stream, error)
}
```

P0/P3：`Identity`（仅 `len(streams)==1`，否则拒绝）。
P4：`Order`（k 路堆）、`Aggregate`（count/sum/min/max/avg）、`Limit`、`Distinct`。

## 6. 数据通路与所有权

### 6.1 一条 SELECT

```
proxy:  COM_QUERY → sql.Statement（解析一次）
  │
execute.Handle(rt, stmt)
  ├─ plan.Build(rt.State, stmt)
  │    ├─ route.Route → []Target{Slice, Role, DB, SQL}
  │    └─ sql/rewrite → 每目标 SQL（P3 可能为空）
  ├─ for t in Plan.Targets:
  │     conn := cluster.Get(t.Slice).Acquire(t.Role)
  │     conn.Align(AlignSettings{DB: t.DB, ...})
  │     stream := conn.Exec(t.DB, t.SQL)
  ├─ if len(streams) > 1: merge.Merge(streams, Plan.Merge)
  └─ write:
       Passthrough/SingleShard → RawStream 直通
       MultiShard             → 编码 Row
```

### 6.2 状态与资源所有权

| 状态/资源 | 所有者 | 生命周期 |
|---|---|---|
| 前端连接、握手 | `mysql/server` + `proxy` | 连接 |
| 会话意图（db/charset/autocommit/vars/inTx） | `session.State` | 连接 |
| prepared statement 表 | `session.StmtTable` | 连接 |
| lease（后端连接） | `execute.Runtime` | 连接 / 事务 / 语句 |
| Plan | `execute`（每条语句） | 语句 |
| result streams | `execute` → `merge` → writer | 语句 |
| Cluster / Pool | `backend` | 进程 |
| 配置快照 | `config.Runtime` | 进程 |
| mode | `server` | 进程 |

### 6.3 两个关键边界

- **lease 不能进 session。** 否则 `route`/`plan` 会被迫依赖 `backend`，
  "先规划后执行"和纯函数单测都做不了。
- **`Target.DB` 必须是物理库。** 分片下同一逻辑表可能落在不同 schema；
  P3 可以先用同名库，但字段必须从第一天就留。

## 7. mode 与配置

### 7.1 配置形状

```yaml
listen: "0.0.0.0:3307"

users:
  - name: app
    password: "***"

backend:
  mode: sharding

  slices:
    - name: s0
      master: { address: "10.0.0.1:3306", user: proxy, password: "***", database: "" }
      slaves:
        - { address: "10.0.0.2:3306", user: proxy, password: "***", database: "" }
    - name: s1
      master: { address: "10.0.0.3:3306", user: proxy, password: "***", database: "" }

  load_balance: round_robin   # 作用于每个 Slice 的从库选择

  sharding:
    rules:
      - table: orders
        key: user_id
        algorithm: hash_mod   # hash_mod | range
        slices: [s0, s1]

pool:
  capacity: 4
  max_capacity: 16
  get_timeout: 2s

log:
  level: info

termination_drain: 5s
```

### 7.2 结构体

```go
// pkg/config
type Config struct {
    Listen           string        `yaml:"listen"`
    Users            []User        `yaml:"users"`
    Backend          BackendConfig `yaml:"backend"`
    Pool             Pool          `yaml:"pool"` // 暂留顶层；是否收进 backend 见开放问题
    Log              Log           `yaml:"log"`
    TerminationDrain time.Duration `yaml:"termination_drain"`
}

type BackendConfig struct {
    Mode        string          `yaml:"mode"`
    Slices      []SliceConfig   `yaml:"slices"`
    LoadBalance string          `yaml:"load_balance,omitempty"`
    Sharding    *ShardingConfig `yaml:"sharding,omitempty"`
}

type SliceConfig struct {
    Name   string            `yaml:"name"`
    Master BackendEndpoint   `yaml:"master"`
    Slaves []BackendEndpoint `yaml:"slaves,omitempty"`
}

type ShardingConfig struct {
    Rules []ShardRule `yaml:"rules"`
}

type ShardRule struct {
    Table     string   `yaml:"table"`
    Key       string   `yaml:"key"`
    Algorithm string   `yaml:"algorithm"`
    Slices    []string `yaml:"slices"`
}
```

### 7.3 校验规则

| mode | slices | slaves | sharding | load_balance |
|---|---|---|---|---|
| `single` | 恰好 1 | 必须为空 | 必须为空 | 必须为空 |
| `rwsplit` | 恰好 1 | ≥1 | 必须为空 | 可空，默认 `round_robin` |
| `sharding` | ≥1 | 每个 Slice 0..N | ≥1 条规则；引用的 Slice 必须存在 | 可空，默认 `round_robin` |

- `mode=sharding` 在 P3 之前继续由 `Validate` 拒绝；P3 打开后只支持单分片可证明的语句。
- `sharding` 允许只有 1 个 Slice（迁移期），规则仍按正常路径计算。
- `load_balance` 当前作用于每个 Slice 的从库集合；暂不做 per-slice 覆盖。
- 规则变更视为拓扑变更：`SameTopology` 返回 false，热更拒绝，需重启。
- 用户/日志级别可热更，与拓扑无关。

### 7.4 归一化与兼容

目标运行时只有一种拓扑模型：`[]Slice`。`single`/`rwsplit` 是"1 个 Slice"的预设。

- **推荐（pre-1.0）**：直接切到 `backend.slices`，旧 `backend_server`、
  `address`、`master/slaves` 字段全部拒绝；`KnownFields(true)` 会让旧配置报错。
- **可选**：保留旧字段作 deprecated sugar，解码后归一化成 `slices`；
  新旧混用报错。代价是多一层兼容测试。
- **不做**：三套形状长期并存（single 拍平、rwsplit master/slaves、sharding slices）。

## 8. 落地顺序

| 阶段 | 内容 | YAML | 退出条件 |
|---|---|---|---|
| **P0 文档** | 本文 + architecture v0.2；决策记录；P3 范围 | 不变 | 评审通过 |
| **P1 seam 重构** | `backend_server → backend`；`router.Group → backend.Slice`；`Pick/Classify → route/source`；新增 `plan`/`result`/`execute` 骨架；删 `shardingExecutor`；single/rwsplit 走 Plan（N=1） | 不变 | 差分校准全绿；行为不变 |
| **P2 会话与配置** | `Session` 状态；USE/SET 从 proxy 移出；`backend` + `slices` 配置；`Conn` 接口补 `Exec/Align`，去掉向下转型 | breaking：`backend_server → backend`，`slices` | rwsplit 冒烟 + 单测；单分片模式未开 |
| **P3 单分片 sharding** | `route/shard`（等值/同片 IN/单区间）；`Plan.SingleShard/Reject`；`EXPLAIN ROUTE`；`StmtTable`/prepare 广播；事务钉单分片；跨片写拒绝 | 增加 `sharding.rules` | 路由表驱动单测 + 单分片 e2e；无静默错结果 |
| **P4 跨分片** | `Plan.MultiShard`；dispatch；identity/order/aggregate/limit；变形 harness；表名改写 | 可能增加规则字段 | 分片 vs 单库结果一致；故障注入 |

P1 是纯重构，必须保持现有差分全绿；P2 才开始 breaking 配置；P3 才打开 `mode=sharding`。

PR 级拆分、文件动作、测试迁移与回滚方式见
[refactor-seam-plan.md](refactor-seam-plan.md)。

## 9. 测试策略

| 模块 | 主要测试 | 是否需要 Docker |
|---|---|---|
| `pkg/route` | 规则→Target 表驱动；确定性；Reject 原因 | 否 |
| `pkg/plan` | Kind/Reject/Explain；单分片证明 | 否 |
| `pkg/merge` | identity；order/aggregate 用脚本化 Stream | 否 |
| `pkg/execute` | fake `Plan` + fake `backend.Conn` + 脚本化 Stream | 否 |
| `pkg/backend` | 池/租约状态机；Align/Reset；fake conn | 否 |
| `pkg/config` | 校验/归一化/热更门禁 | 否 |
| `pkg/proxy` | net.Pipe COM 循环；本地 PING/QUIT/RESET | 否 |
| `test/differential` | 裸 MySQL vs proxy 逐包/结果对比 | 是 |
| `test/metamorphic` | 分片 vs 单库结果一致（P4） | 是 |

原则：

- 正确性主门禁是 **route/plan 的纯函数单测**，不是集成测试。
- `execute` 的测试不依赖真实 MySQL；fake backend 返回脚本化 Stream。
- 每个阶段结束都跑变更包聚焦测试；P1/P2 必须保持现有差分全绿。
- P4 前先把变形 harness 建好，再写跨分片功能。
- 测试分层、MTR/RQG 评估与落地顺序见
  [test-infrastructure.md](test-infrastructure.md)。

## 10. 决策记录

| ID | 决策 | 状态 | 理由 |
|---|---|---|---|
| A1 | 单管线 + 单 Executor + 三 Router | locked | 五家开源单入口；dbpack 三 Executor 复制明显 |
| A2 | 配置段名 `backend` | locked | 语义准确、与 `pkg/backend` 对齐 |
| A3 | Go 包名 `pkg/backend`，接口 `Conn` | proposed | 符合 Go 命名；避免 `backend.Backend` stutter |
| A4 | 统一 `slices`，`single`/`rwsplit` 是预设 | proposed | 避免三套配置形状与三路装配 |
| A5 | `rwsplit` 要求 ≥1 个 slave | proposed | 否则与 single 无法区分 |
| A6 | lease 归 `execute`，session 只存意图 | locked | 保持 route/plan 纯函数 |
| A7 | route/plan 不持有池 | locked | 可单测、可 explain、可缓存 |
| A8 | P3 只做单分片可证明 + Reject | locked | 正确性优先；避免假分片 |
| A9 | N=1 走 raw packet，不 Decode | locked | 协议保真与性能 |
| A10 | 物理库放 `Target.DB` | locked | 分片下 schema 可能不同 |
| A11 | sharding prepare 先广播到所有 Slice | proposed | 执行时才知道目标片；广播最稳 |
| A12 | P3 先做库级路由，表名改写 P4 | proposed | 与"分片库"方向一致；降低 P3 复杂度 |
| A13 | 未配置规则的普通表在 sharding 模式下 Reject | proposed | 无默认片时不能猜；避免错结果 |
| A14 | 多租户 namespace 非目标 | locked | P0–P4 不做 |

## 11. 开放问题

1. **未配置规则的普通表**：P3 直接 Reject，还是引入 `default_slice` / 全局表？
   推荐先 Reject；P4 再评估 `default_slice`。
2. **P3 是否支持表名改写**：推荐只做库级路由（物理表名不变），表级改写放 P4。
3. **`rwsplit` 是否允许 0 个 slave**：推荐要求 ≥1；若运维需要"临时摘光从库"，
   可允许 0 但打 warning。
4. **sharding prepare**：推荐广播 prepare 到所有 Slice，execute 只打目标片，
   close 广播；代价是 stmt id 表和 N 倍 prepare 开销。
5. **`pool` 是否收进 `backend`**：当前保持顶层；P2 配置重构时再定。
6. **`MergeSpec` 的归属**：现放 `plan`；若 `merge` 逆向依赖变重，可下沉到 `result`。
7. **Plan 是否需要 Primitive 树**：P3/P4 先用扁平 `Targets`；出现跨片 JOIN/
   复杂聚合再参考 Vitess 的 Primitive 树演进。
8. **跨片事务错误语义**：P3 返回稳定错误码并保持事务；是否自动回滚需在 P3 文档定。

## 12. 当前文件 → 目标文件

| 当前 | 目标 |
|---|---|
| `pkg/backend_server/backend.go` | `pkg/backend/conn.go` |
| `pkg/backend_server/pool.go` | `pkg/backend/pool.go` |
| `pkg/router/group.go` | `pkg/backend/slice.go` |
| `pkg/router/pick.go` | `pkg/route/source/pick.go` |
| `pkg/router/classify.go` | `pkg/route/source/classify.go` |
| `pkg/executor/executor.go`（passthrough） | `pkg/execute/executor.go` + `pkg/route/source` |
| `pkg/executor/executor.go`（sharding 桩） | 删除 |
| `pkg/executor/forward.go` | `pkg/execute/forward.go` + `pkg/backend/conn.go` |
| `pkg/session/state.go` | `pkg/session/state.go` + `stmt.go` |
| `pkg/sql/parser/parser.go` | 保留；新增 `pkg/sql/statement.go` |
| `pkg/proxy/session.go` | 保留 COM 循环；USE/SET 解析移到 `pkg/sql` |
| `pkg/config/config.go` | `pkg/config/config.go`（`BackendConfig`） |
| `pkg/server/server.go` | 保留；装配逻辑改为 Cluster/Router/Builder/Executor |
| `test/differential` | 保留 |
| 新增 | `test/metamorphic`（P4） |

## 13. 参考

- Vitess：`go/vt/vtgate/engine/plan.go`、`engine/route.go`、`executor.go`、
  `scatter_conn.go`
- ShardingSphere：`infra/route/core/.../SQLRouteEngine.java`、`RouteContext.java`、
  `infra/executor/.../ExecutorEngine.java`、`infra/merge/.../MergeEngine.java`、
  `features/sharding/.../ShardingSQLRouter.java`
- Gaea：`proxy/plan/plan.go`、`proxy/server/executor.go`、`backend/slice.go`
- kingshard：`proxy/router/planbuilder.go`、`proxy/router/router.go`、
  `proxy/server/conn_select.go`
- dbpack：`pkg/proto/interface.go`、`pkg/optimize/optimizer.go`、`pkg/plan/result.go`
- dbatman：`proxy/conn.go`、`database/cluster/driver.go`
- 本地 checkout：`/tmp/prism-oss-survey/{kingshard,Gaea,dbpack,dbatman,vitess}`
