# M1 透传执行管线设计

> 状态：**已定稿**（决策见 §12）  
> 日期：2026-10-30  
> 范围：单机透传落地 + 分片/改写/聚合接口预留；跨分片实现与规则选型后置调研  
> 上位文档：[docs/architecture.md](../architecture.md)  
> 已有基建：`test/differential/`（NullProxy 校准）

## 0. 一句话

先做 **单后端透传代理**，数据面固定为 `Plan → Dispatch → Merge`；M1 里 Plan 恒直通、Dispatch N=1、Merge 为 identity。分片规则 / SQL 改写 / 跨片聚合只定接口与演进方向（倾向 Vitess「先算目标分片再执行」，不跟 Gaea 装饰器全局物化），用差分 harness 替换 NullProxy 作为协议保真验收。

---

## 1. 目标与非目标

### 1.1 目标

1. **协议保真**：同一 CalibrationScript，经本代理 vs 直连 MySQL 的观测一致（错误码、affected_rows、last_insert_id、结果集、warnings）。
2. **可演进管线**：后续接分片/改写/归并时不拆主路径，只换 Router / Rewriter / Merger 实现。
3. **依赖可控**：运行时直接依赖保持极少；协议层第一方，可在无 router 情况下独立测试。
4. **契约可解释**：三态 Plan（直通 / 改写 / 明确报错）；`explain route` 能打印当前 Plan（M1 恒为单目标直通）。

### 1.2 非目标（本阶段明确不做）

- 真实分片规则、跨分片归并、分布式事务
- 在线扩缩容 / 自动 reshard
- SQL 防火墙、加解密、影子库
- gnet/evio；多租户 etcd 热加载（配置先 YAML 文件）
- 自研 SQL parser
- 管理面除 `explain route` 外的录制回放（接口位可留，实现后置）

---

## 2. 开源调研纪要

### 2.1 对照总表

| 维度 | Vitess (VTGate) | Gaea | kingshard / mixer | go-mysql |
|---|---|---|---|---|
| 定位 | 分布式 MySQL 集群代理 | 多租户分库分表代理 | 早期 Go 分片代理 | 协议/复制库，非完整代理 |
| 协议 | 第一方 `go/mysql` | 自研（文本参考 kingshard） | 自研 `mysql/` | `packet` / `server` / `client` |
| Parser | 自研 sqlparser | **TiDB parser** | 自研/简化 | 无 |
| 计划模型 | `Plan` + **Primitive 树** | AST Visitor + **装饰器改写** | 规则算节点后执行 | Handler 回调 |
| 路由本质 | Vindex → keyspace ID → opcode（Equal/Scatter…） | 表下标规则；易「多片打散再算」 | table+key hash/range | — |
| 下发 | ScatterConn 并行 | execute + 池 | backend 节点 | client.Conn |
| 归并 | 引擎内**流式** merge-sort | 聚合/order/group（偏缓冲） | 有，偏全量 | — |
| 连接池 | tablet 侧重；gate 轻会话 | 封装 Vitess ResourcePool | 自研 | 无完整池 |
| Session | SafeSession；settings 池化；temp/lock 才 reserved | 借连批量 SET；有限变量 | 会话偏绑连接 | server.Conn 状态 |
| 配置 | topo + VSchema | namespace + etcd 热加载 | YAML | — |
| 维护/复杂度 | 重、完整 | 中、国内常用 | 基本停更 | 活跃库 |

### 2.2 分层结论（对我们有约束力）

| 层 | 主参考 | 明确不跟 | 理由 |
|---|---|---|---|
| 线协议包结构 | go-mysql（`packet`/`server`/`client`） | 整库黑盒依赖 | 结构清晰；我们要第一方可控 + 差分 |
| auth / status / 测试纪律 | Vitess `go/mysql` | — | caching_sha2、session track、COM 覆盖更全 |
| 执行管线 | Vitess Plan → Primitive → Scatter | Gaea 装饰器主路径 | 可单 leaf 退化；后续不拆管线 |
| Parser | TiDB parser（与 Gaea 同选） | kingshard/Vitess 自研 parser 作主依赖 | 兼容性 + `Restore()`；architecture 已定 |
| 连接池语义 | Gaea←Vitess ResourcePool | database/sql | capacity/max/idle/获取超时；归还前 reset |
| Session 变量 | Vitess「settings 对齐 + 少 reserved」；Gaea 借连 SET 作 M1 简化 | 猜变量不读 tracking | M1 用借连同步；tracking/粘性矩阵后置 |
| 分片规则（后置） | Vitess Vindex/opcode 方向 | Gaea 全局物化再计算；kingshard 表下标强绑 | 用户明确否决 Gaea 该路径；需另开调研 |
| 配置模型 | Gaea namespace **概念**可借鉴 | 首期不上 etcd/cc | 单机 YAML 足够 |

### 2.3 反面教材（写入实现约束）

1. **Gaea/kingshard 风格「先打到多片再内存算」**作为默认跨片路径 → 正确性与内存都差；归并必须流式、目标分片集合先算清。
2. **依赖全家桶冻结**（Gaea 历史问题）→ 直接依赖白名单，禁止 gin/grpc/zap 等。
3. **Prepare 前端二进制、后端改文本**（Gaea 做法）→ **已否决**（D2-A）；前后端均走二进制。
4. **Session 与后端 conn 强绑定** → 池化失效；前端 session 与后端 conn 解耦。

---

## 3. 总体架构

### 3.1 数据通路（统一管线，无旁路双实现）

```
client
  │
  ▼
mysql/server  +  session/state          # 握手、COM 分发、会话态
  │
  ▼
executor ─────────────────────────────────────────────
  │  1) build Plan（三态）
  │       Router.Route(...)     # M1: NoopRouter → 单 Backend Passthrough
  │       Rewriter（仅 Rewrite 态） # M1: 不调用
  │  2) Reject → 直接 ERR 回客户端
  │  3) Dispatch(targets)       # M1: N=1
  │  4) Merger.Merge(streams)   # M1: IdentityMerger
  └──────────────────────────────────────────────────
  │
  ▼
pool → mysql/client → backend MySQL
  │
  ▼
mysql/server 编码响应 → client
```

说明：architecture.md 中的「fast path / slow path」在本设计中收敛为 **同一管线不同 Plan 态**，避免两套代码路径。Passthrough 时可不解析 SQL（零 AST），只选默认 backend 转发。

### 3.2 包与依赖

```
cmd/shardweave/
mysql/{packet,auth,server,client,charset}
session/{state,stickiness}          # stickiness M1 可仅接口/空实现
pool/
config/
sql/{parser,rewrite,fingerprint}    # parser 可 M1 末引入；rewrite 仅接口
router/{rule,route}                 # M1: Noop；rule 包可空
executor/{plan,dispatch,merge}
admin/                              # explain route
obs/
test/differential/                  # 已存在；接入真代理
```

依赖方向（强制）：

```
mysql ← session/pool ← sql/router ← executor
config 只读；obs/admin 横切不进热路径强耦合
mysql 不得 import router / executor
```

### 3.3 核心接口（定义在消费方 `executor`）

```go
type PlanKind int
const (
    PlanPassthrough PlanKind = iota // 原文发往 Targets
    PlanRewrite                     // 每目标可能不同 SQL
    PlanReject                      // 稳定错误码 + 文档锚点
)

type Plan interface {
    Kind() PlanKind
    Targets() []BackendID
    // QueryFor(id) 在 Passthrough 返回原文；Rewrite 返回改写后；Reject 不用
    QueryFor(id BackendID) string
    RejectError() error // 仅 Reject
}

type Router interface {
    Route(ctx context.Context, req RouteRequest) (Plan, error)
}

type RouteRequest struct {
    Session *session.State
    Query   string      // 原文
    Stmt    any         // 可选 AST；M1 可为 nil
}

type Rewriter interface {
    Rewrite(ctx context.Context, stmt any, targets []BackendID) (map[BackendID]string, error)
}

type ResultStream interface {
    Fields() []*Field
    Next() (Row, error) // io.EOF 结束
    Close() error
}

type Merger interface {
    Merge(ctx context.Context, streams []ResultStream, hint MergeHint) (ResultStream, error)
}

type BackendPool interface {
    Acquire(ctx context.Context, id BackendID, want session.Settings) (Conn, error)
    Release(conn Conn) error
}
```

**M1 默认装配**

| 接口 | 实现 | 行为 |
|---|---|---|
| Router | `NoopRouter` | 唯一 default backend，`PlanPassthrough` |
| Rewriter | 未注册 | 不调用 |
| Merger | `IdentityMerger` | `len(streams)==1` 原样返回；否则断言失败 |
| PlanReject | 预留 | 未识别管理命令等可走 Reject |

---

## 4. 协议层（mysql/）

### 4.1 包职责

| 子包 | 职责 | 主要对标 |
|---|---|---|
| `packet` | 长度编码、header、sequence、压缩（可后置） | go-mysql `packet` |
| `auth` | HandshakeV10、native / caching_sha2、AuthSwitch | Vitess auth + go-mysql |
| `server` | Listener、能力协商、COM 循环、OK/ERR/结果集写回 | go-mysql `server` + Vitess `Conn` |
| `client` | 后端 raw conn：握手、COM_QUERY/STMT、读结果 | go-mysql `client` |
| `charset` | collation id ↔ name | Vitess collations（精简子集） |

### 4.2 M1 必须支持的前端 COM

- 连接阶段：Handshake、Auth — **`mysql_native_password` + `caching_sha2_password`**（D1-A），本地用户表校验
- `COM_QUIT` / `COM_PING` / `COM_INIT_DB` / `COM_QUERY`
- `COM_STMT_PREPARE` / `COM_STMT_EXECUTE` / `COM_STMT_CLOSE` / `COM_STMT_RESET`（与现有 CalibrationScript 对齐）
- `COM_RESET_CONNECTION`（前端）：清会话态（**保留当前 database**，对齐 MySQL/ProxySQL）；解除 reserved / inTx；若有 sticky 后端则对其发 `COM_RESET_CONNECTION` 后归还或丢弃，再回前端 OK。后端归还共享池前同样优先 `COM_RESET_CONNECTION`（MySQL 5.7+）。

暂缓：压缩协议、全量 `COM_FIELD_LIST`、binlog dump、多结果集边界情况的极致对齐（差分暴露后再补）。

### 4.3 能力协商

代理作为 server 声明（可配置裁剪）：

- `CLIENT_PROTOCOL_41`、`CLIENT_SECURE_CONNECTION`、`CLIENT_PLUGIN_AUTH`
- `CLIENT_TRANSACTIONS`、`CLIENT_MULTI_STATEMENTS`、`CLIENT_MULTI_RESULTS`
- `CLIENT_DEPRECATE_EOF`（与 MySQL 8 客户端对齐）
- `CLIENT_SESSION_TRACK`（M1 可协商但不完整消费；为后续粘性铺路）

后端 client 能力与目标 MySQL 8.0 对齐；差分容器固定 `mysql:8.0`。

### 4.4 结果转发策略（M1，D2-A）

命令态理解 + 结果包尽量透传字节（sequence 正确前提下），避免自建全量编解码引入 diff。  
**Prepare：前后端均走二进制**（`mysql/client` 完整实现 STMT），不做 Gaea 式「前端二进制、后端降级文本」。

---

## 5. 会话与连接池

### 5.1 session/state（M1 最小集）

跟踪并在借后端连接时同步（可复用连接时）：

- current database（`COM_INIT_DB` / `USE`）
- charset / collation（握手与 `SET NAMES` 的有限子集；首批对齐以 `USE` + `SET NAMES`/charset 为准，`sql_mode` / `autocommit` 为紧随子项）
- **事务中标志：以转发响应中的 `SERVER_STATUS_IN_TRANS` 为准**（ProxySQL 同款；覆盖隐式事务与 `autocommit=0`），不单靠解析 `BEGIN`/`COMMIT`/`ROLLBACK`。事务期间钉住同一后端，status 清位后再可归还。

#### 粘性 / reserved（D3-A，对标开源）

| 组件 | 用户变量 `@x` | 临时表 | `GET_LOCK` / LOCK TABLES | 系统变量 SET |
|---|---|---|---|---|
| **ProxySQL** | 禁用 multiplexing；`COM_RESET_CONNECTION` 可清标志后再池化 | 同上 | 同上 | 跟踪后在换后端时回放；可配 session_track |
| **Vitess** | 尽量在 VTGate 层改写/托管，避免脏后端 | reserved 直到断开 | reserved | **settings pool** 按 settings 指纹匹配连接，少 reserved |
| **Gaea** | 有限 session 变量列表 + 借连批量 SET；另有 **keep-session** 一对一模式 | 能力有限 | 忽略 `LOCK TABLES` 以免脏池 | 借连对齐 charset/sql_mode 等 |
| **kingshard** | 弱；归还前 `COM_RESET_CONNECTION` 清脏 | 弱 | 弱 | SET NAMES 等少量 |

**本项目 M1 选定（ProxySQL reserved 规则 + Gaea 借连对齐；不引入 Vitess settings pool）**：

1. 检测到用户变量（`SET @` / digest 含 `@` 用户变量）、`CREATE TEMPORARY TABLE`、`GET_LOCK`/`RELEASE_LOCK`、`LOCK TABLES` → 会话进入 **reserved**：钉住同一条后端连接；期间不再 multiplexing。前端 `COM_RESET_CONNECTION` 或前端断开后解除。
2. 常见系统变量（首批：`USE` / `SET NAMES`·charset；后续：`autocommit` / `sql_mode`）→ **记录在 session，Acquire 后按需发对齐命令**（Gaea 模式），不因此 reserved。
3. 归还共享池前发 `COM_RESET_CONNECTION`；失败则丢弃连接（kingshard/驱动通病修复同款）。Acquire 后再做 settings 对齐，形成双保险。
4. **后置（不阻塞 M1）**：Vitess settings pool、用户变量 proxy 层托管、`SET_VAR` hint、完整 session tracking 消费。

### 5.2 pool（单后端）

参数（配置项）：`capacity`、`max_capacity`、`idle_timeout`、`get_timeout`。

行为：

1. `Acquire`：取空闲或新建连接；返回后由调用方按 session 做 **Gaea 式** `USE` / `SET NAMES` 对齐（仅当 want 与连接已知态不同）。**不做** Vitess settings-pool 指纹分桶（后置）。
2. 事务中（`SERVER_STATUS_IN_TRANS`）/ reserved：不归还共享池，钉在 session。
3. `Release`：发 `COM_RESET_CONNECTION`（失败则丢弃连接），再入池；Reset 后清除该连接上缓存的 database/charset 已知态。
4. 不定期 ping 全池（Gaea 经验：写失败重建）；idle 定时回收。
5. **不用** `database/sql`。

多后端：接口 `BackendID` 已预留；M1 配置仅一个 backend。

---

## 6. SQL / Router / Merge 预留

### 6.1 sql/parser（D5-B：尽早引入）

- 引入 `github.com/pingcap/tidb/parser`（锁版本），P0/P1 即可进模块，不必等透传完成。
- 包装：`Parse` recover panic → error；统一错误出口。
- **M1 数据面**：Passthrough 仍可不改写；parser 用于 `EXPLAIN ROUTE`、reserved 语句识别（临时表/锁/用户变量）、以及后续分片预埋。
- 单机虽不改写，但分片阶段必用，故不推迟到 P4。

### 6.2 sql/rewrite（仅接口）

预留场景（实现后置）：分表名改写、跨片 `LIMIT/OFFSET` 拆分、聚合拆分。  
机制倾向 Vitess 式 AST 变换，**不用** Gaea 装饰器整树物化套路作为默认。

### 6.3 router（Noop + 未来）

```text
未来方向（调研结论，不锁实现）:
  输入: AST + 规则元数据
  输出: 目标 Backend 集合 + opcode 类语义（Equal / In / Scatter / Reject）
  禁止默认: 「无条件 scatter 全分片再在 proxy 内过滤」
```

`rule/` 包本阶段放类型占位即可。配置里若出现 `sharding:`：**先忽略不解析**（D6），日志 warn 一次；分片规则落地时再接线（fail-open 于「未实现」，避免阻塞单机开工）。

### 6.4 executor/merge

`IdentityMerger` 先落地。后续：排序堆、聚合、GROUP BY 二次归并 —— 对标 Vitess engine，禁止全结果集无界缓冲为默认。

### 6.5 explain route（D4-C）

- **SQL 入口（必须）**：`EXPLAIN ROUTE <sql>`（代理拦截，不转发后端）。
- **HTTP 入口（一并做）**：`GET/POST /v1/explain/route`（body/query: SQL），便于自动化与 CI。
- 输出：PlanKind、Targets、是否解析、Reject 原因、reserved 提示。M1 恒显示单 backend + Passthrough。

---

## 7. 配置（M1）

```yaml
listen: "0.0.0.0:3307"
auth:
  users:
    - name: app
      password: "***"          # plaintext 或 mysql_native / caching_sha2 哈希
backends:
  - id: default
    addr: "127.0.0.1:3306"
    user: proxy
    password: "***"
    database: ""               # 可空，跟前端 USE
pool:
  capacity: 10
  max_capacity: 50
  idle_timeout: 5m
  get_timeout: 2s
admin:
  listen: "127.0.0.1:8080"
# sharding:   # D6：M1 出现则忽略并 warn，不解析、不生效；后续分片 spec 再接入
#   tables: []
```

校验：`validate.go` dry-run；除明确忽略的 `sharding` 外，未知字段拒绝（严格）。

---

## 8. 可观测（最小）

- `log/slog` 结构化日志（连接建立、Plan 拒绝、池耗尽）
- Prometheus：前端连接数、池 in-use/idle、Query 计数、差分无关的延迟直方图
- 不加 Zap；OTel 可选后置

---

## 9. 测试与验收

### 9.1 门槛（必须全绿才算 M1 Done）

| ID | 验收项 |
|---|---|
| A1 | 现有 `TestNullProxyCalibration` 仍绿（harness 未回归） |
| A2 | **新** `TestPassthroughDifferential`：NullProxy 换成真代理进程/in-process server，同一 CalibrationScript 差分为空 |
| A3 | `make test-unit` / `make test-integration` / CI 同现网 workflow |
| A4 | `explain route` 对样例 SQL 返回 Passthrough + `default` backend |
| A5 | 接口编译期约束：存在 `Router`/`Rewriter`/`Merger` 的 noop/identity 实现与装配点 |

### 9.2 分层测试

- L0：packet 编解码往返；pool acquire/release；NoopRouter
- L1：auth/packet fuzz（可 M1 末）
- L3：差分（主门禁）

语料：每个真实协议 bug → `CalibrationScript` 或 corpus 新条目。

---

## 10. 落地分期（实现顺序建议）

| 阶段 | 交付 | 退出条件 |
|---|---|---|
| P0 | `mysql/packet` + 最小 server/client 握手；**引入 tidb/parser 包装** | 单测往返；Parse 不 panic |
| P1 | 单后端池 + session 最小态 + COM_QUERY 透传 + reserved 检测骨架 | 手工 mysql 客户端可查 |
| P2 | Prepare 双边二进制 + 接入差分 A2 | CalibrationScript 全绿 |
| P3 | 管线显式化（NoopRouter/IdentityMerger）+ `EXPLAIN ROUTE` + HTTP explain | A4/A5 |
| P4 | 配置严格校验、obs 最小集、文档与 CI 对齐 | `make test` / CI 全绿 |
| — | 分片规则 / 改写 / 聚合 | **另开调研 + 新 spec** |

---

## 11. 与 architecture.md 的关系

本 spec 是 M1 开工合同；下列点建议在 architecture 定稿时同步修订：

1. 路由「参考 gaea」→ 改为「规则后置调研；方向 Vitess Vindex/opcode，不采用 Gaea 全局物化默认路径」。
2. 数据通路 fast/slow 双路径 → 统一为三态 Plan 单管线。
3. M2 定义可拆为：「接口与 explain」已在本 M1 预留；「真实单分片规则」待调研后单独里程碑；配置 `sharding` 段 M1 忽略（D6）。

---

## 12. 已定决策

| ID | 议题 | 选定 | 说明 |
|---|---|---|---|
| **D1** | 前端认证 | **A** | `mysql_native_password` + `caching_sha2_password`，本地用户表 |
| **D2** | 结果与 Prepare | **A** | 结果包透传；Prepare 前后端均为二进制 |
| **D3** | 用户变量/临时态 | **A** | 用户变量 / 临时表 / 锁 → reserved；系统变量白名单借连对齐（对标 ProxySQL + Gaea 借连 SET） |
| **D4** | explain route | **C** | **SQL 必须**（`EXPLAIN ROUTE`）+ HTTP admin |
| **D5** | Parser 时机 | **B** | P0 即引入 tidb/parser（单机暂不改写，为分片/识别预埋） |
| **D6** | 配置 sharding 段 | **忽略** | 出现则 warn、不解析、不生效；分片规则后续再接 |

拍板记录：D4/D5/D6 由产品确认；D1/D2/D3 及剩余项按推荐落地。

---

## 13. 参考链接

- Vitess VTGate / engine / reserved conn：https://vitess.io/docs/  
- Vitess `go/mysql`：https://github.com/vitessio/vitess/tree/main/go/mysql  
- go-mysql：https://github.com/go-mysql-org/go-mysql  
- Gaea：https://github.com/XiaoMi/Gaea（architecture / connection-pool 文档）  
- kingshard：https://github.com/flike/kingshard  
- TiDB parser：https://github.com/pingcap/tidb/tree/master/pkg/parser  
- MySQL `COM_RESET_CONNECTION`：https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_reset_connection.html  
