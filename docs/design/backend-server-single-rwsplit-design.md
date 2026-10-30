# 后端拓扑：`backend_server`（single / rwsplit，sharding 预留）

> 状态：**已定稿（待实现）** — 含全文件通读附录 A；实现计划见 [backend-server-single-rwsplit-plan.md](backend-server-single-rwsplit-plan.md)  
> 日期：2026-10-30  
> 范围：配置与选源；**先落地 `single` + `rwsplit`**；`sharding` 仅占位并拒绝  
> 上位： [m1-passthrough-pipeline-design.md](m1-passthrough-pipeline-design.md)、[m1-process-and-pipeline-design.md](m1-process-and-pipeline-design.md)（**E10**）  
> 调研：浅克隆 `/tmp/prism-oss-survey/{dbpack,kingshard,Gaea}` **直接读源码**（非记忆）；规则以 §5–§5.12 为规范

---

## 0. 一句话

配置顶层用 **`backend_server`**，用 **`mode: single | rwsplit | sharding`** 表达三种后端形态（产品语义学 dbpack 三 mode，命名用更直白的词）。  
数据面仍是一条 **`Plan → Dispatch → Merge`**；mode 只装配 Router / 多池，**不**复制三套 Executor。  
`rwsplit` 选源 **对照 kingshard/Gaea/dbpack 源码逐条移植**（§5 规则表）：先会话/事务（`IN_TRANS || !autocommit`），再语句分类；变量靠 **State + Align**，不默认广播 `SET`。禁止脱离开源依据自创规则。

---

## 1. 目标与非目标

### 1.1 目标

1. YAML 能表达 `single` 与 `rwsplit`；`sharding` 未实现时 **Validate 拒绝**。
2. `pkg/proxy` 协议循环 **不出现** `switch mode`；差异在 Router / 拓扑装配。
3. `single` 行为与现 M1 透传等价（差分不回归）。
4. `rwsplit`：写与需强一致路径走 master；只读可走 slave；事务 / Prepare / reserved **钉同一后端（主）**。
5. 一地址一池（叶子）；master / 每个 slave 各一个 `pool.Pool`。

### 1.2 非目标（本切片）

- `sharding` 实现与分片规则  
- 多 master、读写权重 DSL（`r0w10`）、健康检查摘流（可后置接口）  
- dbpack Filter / 分布式事务  
- 完整 SQL parser / AST 作为路由唯一依据（可用轻量 tokenize；规则语义仍对齐开源）  
- 只读用户权限体系、监控/统计专用池（Gaea userType）——后置

---

## 2. 开源对照（选源与会话）

| 维度 | dbpack RWS | kingshard | Gaea | **prism 选定** |
|---|---|---|---|---|
| 产品 mode | `sdb` / `rws` / `shd` | node master/slave | slice master/slaves | **`single` / `rwsplit` / `sharding`** |
| 配置形状 | 列表 + `weight` + `master_name` | 显式 master/slave | 显式 Master/Slaves | **显式 master + slaves**（顶层段名 `backend_server`） |
| SQL 分类 | AST（Insert/Select…） | parser + fromSlave 标志 | stmtType + token 启发（FOR UPDATE、hint） | **启发规则**（前缀/关键字），性能优先 |
| 会话压过路由 | `localTransactionMap` 钉 tx | `isInTransaction()` → 强制 master | `isInTransaction` / keepSession → master | **已有 `session.State`：InTx / Reserved / NeedsSticky** |
| `SET` / 变量 | RWS **`QueryAll` 广播** | 部分 SET 在代理侧处理 + 会话状态 | **会话变量表 + 借连 init** | **State + Align（跟 Gaea/已有池）**；不默认 QueryAll |
| 上层差异 | 三种大 Executor | ClientConn 内分支多 | SessionExecutor 统一入口 | **统一管线；mode 只换 Router** |
| 池 | 每 DS 一池 | 每地址 DB | Slice 内多池 | **保持一地址一 `pool.Pool`** |

结论：学 dbpack 的 **三种 mode 叙事**；学 kingshard/Gaea 的 **启发 + 会话状态选源**；学本仓库已有的 **Align / sticky**，不学 dbpack 默认 SET 广播。

---

## 3. 配置契约

### 3.1 顶层

```yaml
listen: "0.0.0.0:3307"
users:
  - name: root
    password: "root"

backend_server:
  mode: single          # single | rwsplit | sharding
  # --- mode: single ---
  address: "127.0.0.1:3306"
  user: root
  password: "root"
  database: ""

# --- mode: rwsplit（示例）---
# backend_server:
#   mode: rwsplit
#   master:
#     address: "127.0.0.1:3306"
#     user: root
#     password: "root"
#     database: ""
#   slaves:
#     - address: "127.0.0.1:3307"
#       user: root
#       password: "root"
#       database: ""
#   # 可选，默认 round_robin
#   # load_balance: round_robin   # round_robin | random

pool:
  capacity: 4
  max_capacity: 16
  get_timeout: 2s

log:
  level: info

termination_drain: 5s
```

### 3.2 校验规则

| mode | 要求 |
|---|---|
| `single` | 需要 `address` + `user`；禁止出现 `master`/`slaves`（或出现则报错，避免双源） |
| `rwsplit` | 需要 `master.address` + `master.user`；`slaves` 可空（空则读写都走 master，仍合法）；禁止再用顶层 `address` |
| `sharding` | **本切片 Validate 直接失败**（明确错误：未实现） |
| 缺省 | `mode` 缺省时视为 `single`（兼容迁移），或要求显式——**推荐显式，缺省 = single 并在文档写明** |

热更（沿用 N1b）：`backend_server` 地址/账号/mode 变更 → reload **失败须重启**；`users` / `log.level` 可热更。

兼容：删除旧顶层键 `backend:`；示例与测试改用 `backend_server`（KnownFields 拒绝旧键，避免静默双轨）。

### 3.3 命名

- YAML / 文档：`backend_server`、`mode`、`rwsplit`  
- Go 类型建议：`config.BackendServer`（配置根段）、叶子可用 `config.BackendEndpoint`（address/user/password/database）  
- 运行时拓扑：`router` 或 `topology` 包内的 `Group`（master pool + slave pools），**不**把 `pool.Pool` 改名为 backend_server

---

## 4. 分层：如何把 mode 差屏蔽在下层

```text
cmd/prism + pkg/server     装配：按 mode 建 Pool(s) + Router
        ↓
pkg/proxy.Serve            COM 循环：Acquire 目标 → 转发 → 释放/sticky
        ↓  （无 mode 分支）
pkg/executor（N2）         Plan → Dispatch → Merge
        ↓
Router.Route(ctx, sql, session) → []Target   ← mode 差异在此
        ↓
Target = 某个 pool.Pool（一地址一池）
```

| 层 | 是否知道 mode |
|---|---|
| proxy / mysql 协议 | **否** |
| executor 骨架 | **否**（永远 1..N 目标） |
| Router 实现 | **是**（`SingleRouter` / `RWSplitRouter`） |
| `pool.Pool` | **否** |

`single`：Router 恒返回 master（唯一池）。  
`rwsplit`：Router 按 §5 返回 master 或某个 slave 池。  
`sharding`：以后返回 N；Merge 变复杂——本切片不做。

---

## 5. `rwsplit` 选源细则（对照源码移植，禁止自创）

**实现方针**：规则从 kingshard / Gaea / dbpack **逐条对照搬迁**，落成 `RouteTarget(session, sql, cmd) → master|slave` 的纯函数 + 表驱动测试。  
**禁止**凭感觉加减规则；有差异必须在本节写明「跟谁、为何」。

主参考文件（均相对 `/tmp/prism-oss-survey`）：

| 仓库 | 文件 | 学什么 |
|---|---|---|
| kingshard | `proxy/server/conn_tx.go` | `isInTransaction = IN_TRANS \|\| !autocommit` |
| kingshard | `proxy/server/conn_query.go` | 事务中强制 `GetMasterConn`；非事务才看 `fromSlave`；借连后 `UseDB`+`SetCharset` |
| kingshard | `proxy/server/conn_select.go` / `conn_query.go` `handleExec` | Select 默认 `fromSlave=true`；Insert/Update/Delete/Replace/Truncate **`fromSlave=false`**；`/*master*/` |
| Gaea | `proxy/server/executor.go` `isInTransaction` / `checkExecuteFromSlave` | 同上事务定义；Select/Show 才可能从；FOR UPDATE / hint / `@@read_only` |
| Gaea | `proxy/server/executor_handle.go` | 先算 fromSlave，再执行；`handleSet` / `handleSetAutoCommit` |
| Gaea | `backend/slice.go` `GetConn` | 从库失败可回落 master（配置项） |
| dbpack | `pkg/executor/read_write_splitting.go` | AST 分支：写→Master，Select→Slave，tx map 优先；`SET autocommit=0` 开事务 |
| dbpack | `pkg/group/group.go` `pick` | Slave 空 → 回落 master |
| prism | `pkg/session/state.go` / `pkg/proxy/session.go` | 已有 sticky、`SetInTx(status&IN_TRANS)`、`Align`；**须补齐 autocommit** |

---

### 5.1 两层判定（与开源同构）

开源都是两层，**不要合成一层「猜 SQL」**：

```text
Layer A — 会话强制（与语句类型无关）
  if inTransaction || reserved/sticky/prepare-pin:
      → 必须 master，且复用已钉连接（不得换池）

Layer B — 语句分类（仅当 Layer A 未命中）
  wantSlave = classify(sql, cmd)   // 见 §5.3
  if wantSlave:
      → 试 slave，失败按 §5.5 回落 master
  else:
      → master
```

对应关系：

- kingshard：`getBackendConn` 先 `if isInTransaction()` 走 `txConns`/master，否则才看 `fromSlave`
- Gaea：`getBackendNoKsConn` 先 `isInTransaction()` → `getTransactionConn`，否则 `slice.GetConn(fromSlave)`
- dbpack：每个 case 先查 `localTransactionMap`，命中则 `tx.Query`，不再 pick slave

---

### 5.2 Layer A：事务与会话状态（最易自研踩坑，必须对齐）

#### 5.2.1 `inTransaction` 定义（跟 kingshard + Gaea，字面一致）

```text
inTransaction := (status & SERVER_STATUS_IN_TRANS) != 0
              || !autocommit
```

源码：

- kingshard `conn_tx.go`：`status&SERVER_STATUS_IN_TRANS > 0 || !isAutoCommit()`
- Gaea `executor.go`：`status&ServerStatusInTrans > 0 || !isAutoCommit()`

**prism 缺口**：当前仅 `ForwardResponse` 后 `SetInTx(status&IN_TRANS)`，**未跟踪 autocommit**。  
本切片必须补：

| 状态字段 | 更新时机 | 说明 |
|---|---|---|
| `InTx` | 每次后端 OK/EOF 的 status 位；以及本地 `BEGIN` 成功 | 已有 |
| `Autocommit` | 握手后默认 `true`；`SET autocommit=0/1` / `ON/OFF` 更新；OK 包若带 Autocommit 位可校验 | **新增**（跟 Gaea `handleSetAutoCommit`） |
| `NeedsSticky` | `InTx \|\| !Autocommit \|\| Reserved` | **扩展**（今日仅 InTx\|\|Reserved） |

`SET autocommit=0`：dbpack `shouldStartTransaction` 会直接 `Begin` 并记入 tx map；Gaea/kingshard 清 Autocommit 位并在下次借主库时 `SetAutoCommit(0)`。  
**prism 选定（跟 Gaea/kingshard）**：更新会话 `Autocommit=false`，选源走 master 并 sticky；在借到的 master 上执行该 `SET`（或等价 `SetAutoCommit(0)`），**不必**另发 `BEGIN`（与 Gaea 注释「SetAutoCommit(0) equivalent to starting a transaction」一致）。

`BEGIN` / `START TRANSACTION`：走 master；执行后依赖 OK 的 `IN_TRANS` 置位（已有转发路径）。

`COMMIT` / `ROLLBACK`（无 savepoint）：走 master；成功后清 `IN_TRANS`；若仍 `!autocommit`，按开源定义 **仍视为 inTransaction**，继续钉 master（直到 `autocommit=1`）。

`ROLLBACK TO SAVEPOINT`：走 master；**不**清事务（跟 dbpack：仅无 SavepointName 时不 delete tx map）。

#### 5.2.2 Reserved / Prepare / sticky（prism 已有 + 开源一致）

| 条件 | 行为 | 依据 |
|---|---|---|
| `DetectReservedSQL`（用户变量、临时表、`GET_LOCK`、`LOCK TABLES`） | `MarkReserved`；之后全程 master + 钉连接 | prism 已有；语义同 Gaea keep 类需求 |
| `COM_STMT_PREPARE` 及后续 `EXECUTE`/`RESET`/`CLOSE` 绑定 | Prepare 时 MarkReserved；钉 **master**（二进制协议连接局部） | prism 已 MarkReserved；dbpack 事务中 stmt 走 tx；非事务 Select prepare 可走从——**prism 第一版 Prepare 一律 master**（简单、避坑；与「连接局部 ID」一致） |
| 已持有 sticky `Backend` | 下一条命令复用，**禁止**换 slave 池 | prism `withBackend`；kingshard `txConns` |

#### 5.2.3 Layer A 单测必覆盖

1. `BEGIN` → `SELECT` → 两次都是同一 master  
2. `SET autocommit=0` → `SELECT` → master（**不是** slave）  
3. `COMMIT` 后 `autocommit=1` → `SELECT` 可 slave  
4. `COMMIT` 后仍 `autocommit=0` → `SELECT` 仍 master  
5. `SET @a=1`（reserved）→ 之后 `SELECT` master  
6. Prepare → Execute → master  

---

### 5.3 Layer B：语句分类规则表（搬迁清单）

实现形态：对 SQL 做 `trim` + 去前导注释（prism 已有 `stripLeadingComments`），再 **前缀 / 关键字 / 尾部 token** 匹配；与 Gaea token + dbpack 语句种类对齐。  
输出：`wantSlave bool`（仅 Layer A 未命中时有效）。

#### 5.3.1 强制 master（`wantSlave=false`）

| # | 规则 | 依据 |
|---|---|---|
| M1 | 前缀（忽略大小写）：`INSERT` `UPDATE` `DELETE` `REPLACE` `TRUNCATE` | kingshard `handleExec(..., fromSlave=false)`；dbpack Insert/Update/Delete→Master；Gaea 非 Select/Show → 不从 |
| M2 | 前缀：`LOAD` `CREATE` `DROP` `ALTER` `RENAME` `GRANT` `REVOKE` `FLUSH` `ANALYZE` `OPTIMIZE` `REPAIR` `CHECK` `INSTALL` `UNINSTALL` `IMPORT` | Gaea：非 Select/Show 默认不从；保守跟「非读」 |
| M3 | 前缀：`BEGIN` `START TRANSACTION` `COMMIT` `ROLLBACK` `SAVEPOINT` `RELEASE SAVEPOINT` `XA` | 三家均主路径；事务控制绝不能上从 |
| M4 | 前缀：`SET`（任意 `SET`，含 `NAMES`/`autocommit`/系统变量） | kingshard/Gaea 在代理侧处理或打后端会话；dbpack 普通 SET→`QueryAll`——**prism 跟 Gaea：打 master + 记 State，不广播** |
| M5 | `USE` / `COM_INIT_DB` | 更新 `State.Database` 并**必须打到一台后端**（透传）。选源：**无事务时允许 slave**（kingshard `getBackendConn(..., true)`）；事务/sticky 中钉当前连接。Gaea 纯改 `se.db` 不打后端——不适合透传。dbpack RWS UseDB unimplemented——不学。详见附录 A.5 |
| M6 | `SELECT`/`SHOW` 但命中锁读：尾部或 token 含 `FOR UPDATE` / `FOR SHARE` / `LOCK IN SHARE MODE`（及 Gaea 所覆盖的 `nowait`/`skip locked` 组合） | Gaea `checkExecuteFromSlave` + `CheckSelectLock` |
| M7 | master hint：注释或 token 含 `/*master*/` / `*master*`（Gaea 还认 `/*+ master */`） | kingshard `MasterComment`；Gaea `masterHint`/`standardMasterHint` |
| M8 | `SHOW …` 且 SQL 含 `read_only`（大小写不敏感）；或 `SELECT` 含 `@@read_only` / `@@global.read_only` | Gaea `readonlyVariable` 特例 → master |
| M9 | 无法识别语句种类 | **master**（保守；开源未知类型也倾向不从或报错） |

#### 5.3.2 允许 slave（`wantSlave=true`）

| # | 规则 | 依据 |
|---|---|---|
| S1 | 前缀 `SELECT`（且未命中 M6/M7/M8） | kingshard Select 默认 fromSlave；dbpack Select→Slave；Gaea StmtSelect |
| S2 | 前缀 `SHOW`（且未命中 M8） | Gaea StmtShow 可从 |
| S3 | 前缀 `EXPLAIN` / `DESCRIBE` / `DESC`（非 `EXPLAIN` 改写类管理语句） | 读意图；Gaea 非强制但从保守角度：若实现困难可先 **master**（记开放项）；**建议第一版 EXPLAIN/DESC → slave**，与「只读」一致 |
| S4 | kingshard `SimpleSelect`（无 FROM 的 select） | 走 Select 路径；prism 用同一 S1 |

**明确不做（本切片）**：Gaea 只读用户拒写、Statistic 专用从库、dbpack 表 hint 指定 DS 名。

#### 5.3.3 COM 命令级（非 SQL 文本）

| COM | 选源 |
|---|---|
| `COM_QUERY` | 按 §5.3.1–5.3.2 |
| `COM_INIT_DB` | 无事务 → slave 可（失败回落主）；事务/sticky → 钉当前（M5 / 附录 A.5） |
| `COM_PING` / `COM_QUIT` | 可不借后端或借任意；现有逻辑可保持 |
| `COM_STMT_PREPARE`/`EXECUTE`/`RESET`/`CLOSE`/`SEND_LONG_DATA` | master + reserved（§5.2.2） |
| `COM_FIELD_LIST` | Gaea/dbpack 偏 slave；**prism：slave，失败回落 master** |

---

### 5.4 会话变量与库名（跟 Gaea，禁止学 dbpack QueryAll）

| 事件 | 行为 |
|---|---|
| `USE` / `COM_INIT_DB` | `State.SetDatabase`；打一台后端（无事务可从，见 M5）；其它池靠 Align |
| `SET NAMES` / charset | `State.SetCharset`；在 **master** 执行 |
| 其它 `SET` | 能解析进 State 的（autocommit）更新 State；一律在 **master** 执行；不解析的也打 master，避免从库会话漂移 |
| 下次借 **另一台**（如 slave） | `Align(database, charset)`（prism 已有）；对齐 Gaea `initBackendConn` 的 USE + charset（系统变量白名单可后续扩） |
| 用户变量 `@x` | reserved → sticky master（已有），**不要**以为 Align 能同步用户变量 |

**禁止**：dbpack RWS 对普通 `SET` 的 `QueryAll`（主从全广播）作为默认——成本高且与现 Align 模型重复。

---

### 5.5 从库选择与回落

| 项 | 选定 | 依据 |
|---|---|---|
| 多 slave 负载 | 默认 `round_robin`；可选 `random` | kingshard 权重/轮询；dbpack Random/RoundRobin/RandomWeight；第一版不实现 weight DSL |
| slave Acquire 失败 | **回落 master** | kingshard `GetSlaveConn` 失败则 `GetMasterConn`；Gaea `SlaveFallbackMaster`；dbpack `getAvailableSlaves` 空则 `_randomMaster` |
| 无 slaves 配置 | 全部走 master（合法） | 退化成 single |
| 健康检查摘流 | 本切片不做 | 后置 |

---

### 5.6 推荐实现结构（便于对照测试）

```text
pkg/router/rwsplit.go   // 或 session 旁纯函数，避免 proxy 膨胀
  IsInTransaction(st) bool           // IN_TRANS || !Autocommit
  ClassifyWantSlave(sql, cmd) bool   // 仅 Layer B；表驱动
  Pick(st, sql, cmd, group) Target   // A then B；含 sticky

测试：router/rwsplit_route_test.go 纯表驱动，用例注释标明对照仓库+函数名
```

`proxy` 只调用 `Pick`，**不**内嵌规则字符串。

---

### 5.7 规则争议时的默认原则

1. **会话一致性 > 读扩散**（事务 / autocommit=0 / reserved 绝不走从）  
2. **跟两家以上开源的交集**；仅一家有的特例（如 Gaea `@@read_only`）值得搬则写入表并标来源  
3. **拿不准 → master**  
4. 新增规则必须先加失败单测再改实现（TDD）

---

### 5.8 三家分歧对照 → prism 显式选定（避免混抄）

实现时最容易「三家各抄一点」互相打架。下表把分歧钉死：

| 议题 | kingshard | Gaea | dbpack RWS | **prism** |
|---|---|---|---|---|
| `inTransaction` | `IN_TRANS \|\| !AC` | 同左 | 靠 `localTransactionMap` 有无 tx 句柄 | **跟 K/G：`IN_TRANS \|\| !AC`**；另保留 sticky/reserved |
| `SET autocommit` | **代理侧改 status**，必要时对已借 tx 连接 `SetAutoCommit`；`=0` 时**不必立刻借连接** | 同左；keep-session 时对已持连接执行 | `autocommit=0` → **立刻 Begin** 并放入 tx map | **跟 K/G**：改会话位；若已有 sticky master 则在其上执行 `SET`/`SetAutoCommit`；若无连接则只改会话，下一条按 inTx 借 master 再对齐 |
| `SET NAMES` / charset | **代理侧改** `charset/collation`，`writeOK`，不强制打后端 | 写入 session 字段 + `sessionVariables`，借连 `initBackendConn` | 普通 SET → **QueryAll** | **跟 Gaea 心智**：更新 `State.Charset`；**同时在 master 执行**（透传保真 + Align 源）；**不** QueryAll |
| 其它系统 `SET` | 多数「不支持」却 `writeOK`（吞掉） | 白名单进 sessionVariables；未知 **忽略并打日志** | QueryAll 或走从/主 | **第一版：整句转发 master**（保真优先）；能识别的 `autocommit`/`NAMES` **额外**更新 State。未知变量不吞掉静默 OK（与 K 不同） |
| 用户变量 `SET @x` | 有限 | 进 user session var + keep 语义 | tx 或默认从？default 分支 slave | **DetectReserved → sticky master**（已有） |
| Select 默认 | `fromSlave=true` | `checkExecuteFromSlave` | `WithSlave` | **wantSlave=true**（S1） |
| 写语句 | `handleExec(..., false)` | 非 Select/Show → 不从 | `WithMaster` | **M1** |
| FOR UPDATE | （依赖注释/主路径） | token 规则强制主 | AST 未单独拆 FOR UPDATE※ | **跟 Gaea：M6** |
| master hint | `/*master*/` | `/*master*/` / `*master*` / `/*+ master */` | 表 hint 换 DS | **跟 Gaea：M7** |
| 从库失败 | 回落 master | 可配置 fallback | slaves 空 → master | **回落 master（开）** |
| Prepare | 绑连接 | 有 prepare 路径 | 非 tx 时 Select prepare 可从 | **一律 master+reserved（B11）** |
| 归还连接 | 事务中不还（钉在 `txConns`） | `recycleBackendConn`：inTx 则 return 不 Recycle | tx 句柄持有至 commit | **跟现有 sticky：NeedsSticky 不 Release 回池** |
| `USE` / InitDB | **优先从** `getBackendConn(..., true)` | **只改 `se.db`，不打后端** | **unimplemented** | **打后端 + 更新 State**；非事务可从（跟 K）；事务钉当前。不学 Gaea 空跑、不学 dbpack 未实现 |

※ dbpack 对 `SelectStmt` 一律 Slave，**不**单独识别 `FOR UPDATE`——这是已知简化；**prism 不跟**，跟 Gaea，避免锁读打到从库。

---

### 5.9 会话状态机（规范）

握手成功后：

```text
Autocommit = true
InTx       = false
Reserved   = false
Database   = 握手/默认库
Charset    = "" 或协商值
sticky     = nil
```

#### 5.9.1 `IsInTransaction(st)`（唯一实现）

```text
return st.InTx || !st.Autocommit
```

与 kingshard `conn_tx.go` / Gaea `executor.go` 同构。  
`NeedsSticky(st) := IsInTransaction(st) || st.Reserved`。

#### 5.9.2 事件 → 状态迁移

| 事件 | Autocommit | InTx | Reserved | sticky | 选源 |
|---|---|---|---|---|---|
| 握手 | true | false | false | nil | — |
| `SET autocommit=0/OFF` | **false** | 不变（常仍 false） | — | 若已有则保留 | master（因 !AC） |
| `SET autocommit=1/ON` | **true** | **清 false**（跟 K/G：顺带清 IN_TRANS） | — | 若不再 NeedsSticky 则归还 | 按语句 |
| `BEGIN`/`START TRANSACTION` 成功 | — | **true** | — | 钉 master | master |
| 后端 OK/EOF：`status & IN_TRANS` | — | **同步为位值**（权威来自后端，与现 `SetInTx` 一致） | — | 随 NeedsSticky | — |
| 后端 OK：`status & AUTOCOMMIT` | **可选同步**（建议：若跟踪 AC 位则与会话一致；冲突以**会话显式 SET 为准**，后端位作校验日志） | — | — | — | — |
| `COMMIT`/`ROLLBACK`（无 savepoint）成功 | — | **false**（清 IN_TRANS；K/G commit/rollback 清位） | — | 若 `Autocommit` 则松开 sticky | 下条再分类 |
| `ROLLBACK TO savepoint` | — | **保持 true** | — | 保持 | master |
| `DetectReservedSQL` / Prepare | — | — | **true** | 钉 master | master |
| 前端关闭 / 错误丢弃连接 | reset | reset | reset | Discard | — |

**关键语义（务必单测）**：

- `Autocommit=false` 且 `InTx=false` 时，`IsInTransaction==true` → **SELECT 仍走 master**（K/G 同款，最易漏测）。  
- `COMMIT` 后若仍 `Autocommit=false`，`IsInTransaction` 仍为 true → 继续 master。  
- `SET autocommit=1` 在 K/G 会清 IN_TRANS 并 **回收 txConns**；prism 对应：清 InTx、若 `!Reserved` 则 Release sticky。

#### 5.9.3 sticky 借还（对齐 Gaea `recycleBackendConn`）

```text
after each command:
  if NeedsSticky(st):
      keep sticky backend (do not pool.Release)
  else:
      pool.Release (Reset) sticky; sticky = nil
```

事务中途 **禁止** 因「本条是 SELECT」而换到 slave 池。

首次因事务需要后端时（尚无 sticky）：

```text
pc = Acquire(master)
if !st.Autocommit:
    // Gaea getTransactionConn: SetAutoCommit(0) ≡ 开事务
    ensure backend autocommit=0  (via SET or helper)
else if st.InTx:  // BEGIN 路径已发或即将发
    // BEGIN 由客户端 SQL 转发，或本地已置位后后端 Begin
Align(database, charset)
sticky = pc
```

---

### 5.10 规范算法 `Pick`（实现必须等价）

输入：`st *session.State`，`cmd`（COM 与 payload），`group`（master + slaves 池）。  
输出：`target`（池 + 是否复用 sticky）。

```text
function Pick(st, cmd, sql, group, sticky):
    // ----- Layer A -----
    if sticky != nil && NeedsSticky(st):
        return Reuse(sticky)                    // 不得换池

    if NeedsSticky(st) || cmd in STMT_FAMILY:  // PREPARE/EXECUTE/RESET/CLOSE/SEND_LONG_DATA
        return AcquireMaster(group, st)        // 并设 sticky

    // COM 级短路
    if cmd == COM_INIT_DB:
        return AcquireSlaveOrFallback(group, st) // M5：无事务可从；Layer A 已处理 sticky

    if cmd == COM_FIELD_LIST:
        return AcquireSlaveOrFallback(group, st)  // 跟 Gaea/dbpack

    if cmd != COM_QUERY:
        return AcquireMaster(group, st)        // 未知 COM 保守

    // ----- Layer B：仅文本 -----
    sql = stripLeadingComments(trim(sql))
    if sql == "":
        return AcquireMaster(group, st)

    if hasMasterHint(sql):                     // M7
        return AcquireMaster(group, st)

    kind = classifyPrefix(sql)                 // 见 §5.10.1

    if kind == USE:
        return AcquireSlaveOrFallback(group, st) // M5 / kingshard

    if kind in WRITE_DDL_TX_SET:               // M1–M4
        return AcquireMaster(group, st)

    if kind in {SELECT, SHOW} && isLockingRead(sql):  // M6
        return AcquireMaster(group, st)

    if kind in {SELECT, SHOW} && isReadOnlyVarProbe(sql):  // M8
        return AcquireMaster(group, st)

    if kind in {SELECT, SHOW, EXPLAIN, DESC, DESCRIBE}:    // S1–S3
        return AcquireSlaveOrFallback(group, st)

    return AcquireMaster(group, st)            // M9
```

#### 5.10.1 `classifyPrefix`（扎实匹配，避免假前缀）

1. `stripLeadingComments`（已有：`--` `#` `/* */`；**注意**：`/*master*/` 若在语句**中间/尾部**，前导剥离不会删掉——hint 检测须在整串或 token 上做，见 M7）。  
2. 跳过空白；取第一个 SQL 关键字（字母开头连续 `[A-Za-z0-9_]`）。  
3. 若关键字为 `START`，lookahead 下一词为 `TRANSACTION` → 归为 BEGIN 类。  
4. 若关键字为 `SET`，整类 M4（不再细分是否变量，选源已定 master；状态更新见 §5.9）。  
5. **多语句**（`;` 连接）：第一版 **整条走 master**（K/G 亦非完整多语句路由）；或拒绝。选定：**整条 master**。

#### 5.10.2 `isLockingRead`（跟 Gaea token 思路，启发实现）

在 upper 后的 SQL 上检测（允许简单子串 + 边界，表驱动校准）：

- `FOR UPDATE`  
- `FOR SHARE`  
- `LOCK IN SHARE MODE`  
- 以及 Gaea 覆盖的尾部组合：`... FOR UPDATE NOWAIT` / `SKIP LOCKED` 等（实现时把 Gaea `checkExecuteFromSlave` 的 token 条件译成测试用例，而不是只抄注释）

#### 5.10.3 `hasMasterHint`

- 子串（大小写不敏感注释）：`/*master*/`、`/*+ master */`  
- 或 tokenize 后存在 `*master*`（Gaea analyzer 行为）  

命中 → master，**即使**是普通 SELECT。

#### 5.10.4 `AcquireSlaveOrFallback`

```text
for candidate in roundRobin(group.slaves):
    pc, err = Acquire(candidate)
    if err == nil:
        Align(pc, st)
        return pc
return AcquireMaster(group, st)
```

---

### 5.11 验收语料（实现前先落成测试表）

下列用例 **必须** 在 `Classify`/`Pick` 单测中出现；注释写 `ref: kingshard|Gaea|dbpack` + 函数名。  
约定：`AC=autocommit`，期望为 `M`/`S`（slave 可回落 M，断言「意图为 S」时用 mock：slave 可用）。

#### A. 事务 / AC（Layer A）

| # | 前置状态 | SQL | 期望 |
|---|---|---|---|
| A1 | 默认 | `SELECT 1` | S |
| A2 | 默认 | `BEGIN` | M |
| A3 | InTx | `SELECT 1` | M（同 sticky） |
| A4 | InTx | `COMMIT` | M；之后 InTx=0 |
| A5 | AC=0, InTx=0 | `SELECT 1` | **M**（K/G） |
| A6 | AC=0 | `COMMIT` 后仍 AC=0 | 下一条 `SELECT` → **M** |
| A7 | AC=0 | `SET autocommit=1` | M；之后 `SELECT` → S |
| A8 | InTx | `ROLLBACK TO sp1` | M；仍 InTx |
| A9 | reserved | `SELECT 1` | M |
| A10 | Prepare 后 | `EXECUTE` | M |

#### B. 语句分类（Layer B，非事务）

| # | SQL | 期望 | 规则 |
|---|---|---|---|
| B1 | `INSERT INTO t VALUES (1)` | M | M1 |
| B2 | `UPDATE t SET a=1` | M | M1 |
| B3 | `DELETE FROM t` | M | M1 |
| B4 | `REPLACE INTO t VALUES (1)` | M | M1 |
| B5 | `TRUNCATE t` | M | M1 |
| B6 | `CREATE TABLE t(i INT)` | M | M2 |
| B7 | `SET NAMES utf8mb4` | M | M4 |
| B8 | `SET autocommit=0` | M | M4+A |
| B9 | `USE db1`（非事务） | S（可回落 M） | M5 / kingshard |
| B10 | `SELECT * FROM t` | S | S1 |
| B11 | `SELECT * FROM t FOR UPDATE` | M | M6 |
| B12 | `SELECT * FROM t LOCK IN SHARE MODE` | M | M6 |
| B13 | `SELECT /*master*/ * FROM t` | M | M7 |
| B14 | `/*master*/ SELECT * FROM t` | M | M7 |
| B15 | `SHOW TABLES` | S | S2 |
| B16 | `SHOW VARIABLES LIKE '%read_only%'` | M | M8 |
| B17 | `SELECT @@read_only` | M | M8 |
| B18 | `EXPLAIN SELECT * FROM t` | S（开放项若改 M 则改表） | S3 |
| B19 | `SELECT * FROM t; DELETE FROM t`（多语句） | M | §5.10.1 |
| B20 | `-- c\nSELECT 1` | S | 注释剥离 |
| B21 | `select * from t`（小写） | S | 大小写 |
| B22 | 空 / 仅注释 | M | M9 |

#### C. 回落与 sticky

| # | 场景 | 期望 |
|---|---|---|
| C1 | 无 slaves | 一切 M |
| C2 | slave Acquire 失败 | 该次 M，且不污染 sticky（非事务） |
| C3 | 事务中 sticky 在 master | 中途 SELECT 不得换到其它连接 |

---

### 5.12 SQL 规范化与假阳性防护

| 风险 | 处理 |
|---|---|
| 字符串里出现 `FOR UPDATE` 字样 | 第一版启发可能假阳性 → **偏 master（可接受）**；进阶再用 tokenizer；禁止为去假阳性而漏掉真锁读 |
| `INSERT` 在注释里 | 前导注释已剥；中部注释导致误判写 → 偏 master |
| `SELECT` 前有 CTE `WITH ... SELECT` | **第一版：前缀 `WITH` → master（M9 保守）**；或识别 `WITH` 后仍当读——**选定：WITH → 再扫描是否含主路径关键字，默认 master** 直到有单测覆盖 CTE 只读 |
| `/*master*/` 在字符串字面量 | 可能假阳性偏 master（可接受） |
| 前置 `/*!40101 ... */` MySQL 条件注释 | 剥离策略与 go-mysql/开源一致；单测一条 |

前缀关键字集合（实现常量，与 M1/M2/M3 同步，**改集合必须改 §5.3 与 §5.11**）：

```text
WRITE: INSERT UPDATE DELETE REPLACE TRUNCATE LOAD
DDL:   CREATE DROP ALTER RENAME GRANT REVOKE FLUSH ANALYZE OPTIMIZE REPAIR CHECK
TX:    BEGIN COMMIT ROLLBACK SAVEPOINT XA  (+ START TRANSACTION)
SET:   SET
USE:   USE
READ:  SELECT SHOW EXPLAIN DESCRIBE DESC
```

---

## 6. 与现有代码的落点

| 现有 | 变化 |
|---|---|
| `config.Backend` | 改为 `BackendServer` 段 + `mode`；叶子 `BackendEndpoint` |
| `etc/prism.yaml` | `backend:` → `backend_server.mode: single` |
| `cmd/prism` / `pkg/server` | 按 mode 建 1 个或多个 Pool，注入 Router |
| `pkg/proxy.SessionConfig` | `Pool *pool.Pool` → 抽象为「按 Route 取池」（或持有 `BackendGroup`） |
| `pkg/session` | 补 `Autocommit`；`NeedsSticky` 含 `!Autocommit`；保留 reserved 检测 |
| `pkg/router`（新建或暂放 session） | `Pick` / `ClassifyWantSlave`；§5 表驱动单测，注释标开源出处 |
| `pkg/backend_server`（原 `pkg/pool`） | 仍为叶子连接池；不感知主从 |
| `pkg/executor`（N2） | Router 接口在此装配；可与本切片一起或紧后 |

差分：`single` 跑现有 scenarios；`rwsplit` 另加集成（Docker 一主一从或 mock Router 单测为主，集成可后置）。

---

## 7. 实现分期（建议）

| 步 | 内容 | 退出 |
|---|---|---|
| **T0** | 本文档评审通过 | 无 open 争议 |
| **T1** | `pkg/config`：`backend_server` + Validate（single/rwsplit；sharding 拒绝）；改示例 YAML；迁移测例 | 聚焦单测绿 |
| **T2** | `Autocommit`+状态机 §5.9；`Pick`≡§5.10；**先写入 §5.11 语料为失败测试再实现**；`BackendGroup` | §5.11 A/B/C 全绿；注释含开源 ref |
| **T3** | `proxy`/`server`/`cmd` 接到 Group；`single` 路径差分不回归 | 本地/现有 integration |
| **T4** | `rwsplit` 冒烟（可选 Docker 主从）；文档与 E10 命名对齐 | 人工可连从库读 |

禁止未实现字段「写了当没有」；`sharding` 与未知 mode 必须报错。

---

## 8. 决策表（本设计锁定）

| ID | 议题 | 选定 |
|---|---|---|
| **B1** | 配置顶层名 | `backend_server` |
| **B2** | mode 名 | `single` / `rwsplit` / `sharding` |
| **B3** | 配置形状 | 显式 `master` + `slaves[]`（rwsplit）；single 用段内 address 字段 |
| **B4** | 执行结构 | 一条管线；mode 装配 Router；非三套大 Executor |
| **B5** | 读路由 | **§5 规则表逐条移植**；禁止自创；拿不准 → master |
| **B6** | SET | State + 打 master + Align；**禁止** QueryAll / 禁止吞 OK |
| **B12** | USE | State + 打后端；非事务可从（K）；事务钉当前 |
| **B7** | 本切片范围 | 只实现 single + rwsplit；sharding 拒绝 |
| **B8** | 从库 LB | 默认 round_robin；失败回落 master |
| **B9** | 事务定义 | **`IN_TRANS \|\| !autocommit`**（跟 kingshard/Gaea）；补齐 Autocommit 与 NeedsSticky |
| **B10** | hint / FOR UPDATE / read_only | **第一版纳入** §5.3 M6–M8 |
| **B11** | Prepare | 第一版一律 master + reserved |

---

## 9. 开放项 → 默认锁定（P0）

| 项 | 锁定 |
|---|---|
| `mode` 缺省 | **`single`** |
| `EXPLAIN`/`DESC` | **slave（S3）** |
| 集成测进 CI | **有主从夹具后再加**；现阶段以 §5.11 单测为准 |
| Align 系统变量白名单 | **本阶段仅 database + charset**；扩白名单后置 |  

---

## 10. 参考路径（相对 `/tmp/prism-oss-survey`）

| 仓库 | 文件 |
|---|---|
| dbpack | `pkg/executor/read_write_splitting.go`，`misc.go`（`shouldStartTransaction`），`pkg/group/group.go`，`docker/conf/config_{sdb,rws}.yaml` |
| kingshard | `proxy/server/conn_query.go`（`getBackendConn`/`handleExec`），`conn_select.go`（`/*master*/`），`conn_tx.go`，`conn_set.go`，`backend/node.go` |
| Gaea | `proxy/server/executor.go`（`isInTransaction`、`checkExecuteFromSlave`、`handleBegin`），`executor_handle.go`（`handleSet*`），`backend/slice.go`（`GetConn` 回落） |
| prism | `pkg/session/state.go`，`pkg/pool/backend.go`（`Align`），`pkg/proxy/session.go`（`SetInTx`） |

规则正文以 **§5–§5.12** 与 **附录 A（全文件通读纪要）** 为准。实现 PR 必须：

1. `Pick` 与 §5.10 伪代码行为一致；  
2. §5.11 语料全覆盖；  
3. 测试注释标注 **A#/B#/C#** 或 **M#/S#** + 开源 `文件/函数`；  
4. 与 §5.8 / 附录 A.4 分歧表冲突的「顺手改行为」视为 bug。

实现切片步骤见同目录：[backend-server-single-rwsplit-plan.md](backend-server-single-rwsplit-plan.md)。

---

## 附录 A. 开源全文件通读纪要（2026-10-30）

> 方法：对下列文件 **整文件阅读**（非 grep 片段），再提炼。路径相对 `/tmp/prism-oss-survey`。

### A.1 已通读文件清单

| 仓库 | 文件 | 行数级 | 通读范围 |
|---|---|---:|---|
| kingshard | `proxy/server/conn_query.go` | 410 | **全文** |
| kingshard | `proxy/server/conn_tx.go` | 83 | **全文** |
| kingshard | `proxy/server/conn_set.go` | 144 | **全文** |
| kingshard | `proxy/server/conn_use.go` | 52 | **全文** |
| kingshard | `proxy/server/conn_select.go` | 780 | **全文关键路径**（handleSelect/FieldList/SimpleSelect；merge 算法非 RWS） |
| kingshard | `proxy/server/conn_stmt.go` | 519 | **Prepare 选源段全文**（`getBackendConn(..., false)`） |
| kingshard | `backend/node.go` | 363 | **GetMaster/Slave/检查与权重段** |
| Gaea | `proxy/server/executor_handle.go` | 673 | **全文**（handleQuery→doQuery→withoutPlan→Set/Use/Prepare/FieldList） |
| Gaea | `proxy/server/executor.go` | 1563 | **COM 分发、借还、事务、checkExecuteFromSlave、Begin/Commit、initBackendConn 全文段** |
| Gaea | `backend/slice.go` | 1296 | **GetConn / DirectMaster / SlaveFallback 段**（熔断细节后置） |
| dbpack | `pkg/executor/read_write_splitting.go` | 372 | **全文** |
| dbpack | `pkg/executor/misc.go` | 64 | **全文** |
| dbpack | `pkg/group/group.go` | 309 | **全文** |

### A.2 kingshard — 控制流（整文件结论）

```text
handleQuery
  → preHandleShard? 
  → Parse
  → switch stmt:
       Select      → handleSelect: fromSlave=true；Comments[0]=="/*master*/"则 false
       Insert/Update/Delete/Replace/Truncate → handleExec: getShardConns(false)  // 强制主
       Set         → handleSet（代理侧，多数不打后端）
       Begin/Commit/Rollback → handleBegin/Commit/Rollback（代理侧改 status + txConns）
       UseDB       → handleUseDB: getBackendConn(n, true)  // 优先从！
       SimpleSelect→ 本地 last_insert_id，不借后端
       default     → error 不支持

getBackendConn(n, fromSlave):
  if !isInTransaction():
      fromSlave ? GetSlaveConn()（失败则 GetMasterConn）: GetMasterConn
  else:
      复用 txConns[n]；若无则 GetMasterConn，
      !autocommit → SetAutoCommit(0)  else Begin()；记入 txConns
  然后 UseDB(c.db) + SetCharset（每次借连对齐）

closeConn / closeShardConns:
  if isInTransaction(): return  // 不归还
  else Close（回池）

isInTransaction = IN_TRANS || !AUTOCOMMIT   // conn_tx.go 全文仅此语义

SET（conn_set.go 全文）:
  仅处理 AUTOCOMMIT / NAMES / character_set_* 
  其它 SET → 打错误日志但仍 writeOK（吞掉，不转发后端）——prism 不学
  autocommit=1 → 置 AC、清 IN_TRANS、对 txConns SetAutoCommit(1) 后 Close 清空
  autocommit=0 → 仅清 AC 位，不立刻借连接

USE（conn_use.go 全文）:
  优先 fromSlave=true；成功则 c.db=dbName
  （与「会话变量只打主」不同——库名切换可走从）

Prepare（conn_stmt.go）:
  getBackendConn(n, false)  // 明确走主；Prepare 完 ClosePrepare，不长期钉后端语句 ID
  （kingshard 前端 stmt id 自管，后端 prepare 用完即关——与 prism「前后端均二进制、钉连接」不同）

FieldList（conn_select.go）:
  getBackendConn(n, false)  // 走主

Node（backend/node.go）:
  Master 单 DB；Slave[] + 权重轮询；Ping 摘流 DownAfterNoAlive
```

**对 prism 的硬启示**：Layer A 事务钉主 + 借连 Align 与 K 同构；语句层 Select 默认可从、写强制主；**USE 开源是优先从**——见 A.4 选定。

### A.3 Gaea — 控制流（整文件结论）

```text
ExecuteCommand(cmd):
  Query → handleQuery → (multi?) doQuery
  InitDB → handleUseDB（只改 se.db，不打后端）
  FieldList → SetFromSlave(rwsplit|readonly) 再 getBackendConn
  StmtPrepare → 本地登记 Stmt（算参数），不一定立刻打后端 prepare
  Quit → handleRollback

doQuery:
  checkSQLAllowed / Preview stmtType
  if canHandleWithoutPlan (Show/Set/Begin/Commit/Rollback/Savepoint/Use/Kill/LockTables):
      handleQueryWithoutPlan  // 不走 fromSlave 分类
  else:
      getPlan（先 tokenize）
      checkExecuteFromSlave → SetFromSlave true/false
      plan.ExecuteIn → 内部再 getBackendConn

checkExecuteFromSlave（全文逻辑）:
  仅 StmtSelect 或 StmtShow 才可能 true；其它一律 false（走主）
  只读用户 → true
  CheckSelectLock 时：尾部 token 匹配 FOR UPDATE / SHARE MODE / NOWAIT / SKIP LOCKED → false
  SHOW 含 read_only → false
  SELECT 含 @@read_only / @@global.read_only → false
  token[1] 或 token[last] == *master* → false
  否则 return IsRWSplit(user)

getBackendNoKsConn:
  !isInTransaction → slice.GetConn(fromSlave 标志)
  isInTransaction → getTransactionConn → 必 Master；
      SyncSessionVariables；!AC 则 SetAutoCommit(0) 否则 Begin；重放 savepoints

recycleBackendConn:
  isInTransaction 或 KeepSession → 不 Recycle
  否则 Recycle

handleBegin（全文）:
  对已有 txConns/ksConns 调 Begin；置 IN_TRANS；清 savepoints
  注释写明：客户端 BEGIN 时「后端可能尚未真正开事务」，真正开在首次 getTransactionConn

handleUseDB（全文）:
  只校验 allowed_dbs 后 se.db=name；return nil —— 纯代理状态

handleSet / handleSetAutoCommit（全文）:
  会话变量进 sessionVariables / charset；AC 位处理同 K
  未知系统变量：忽略 + Warn 日志（不报错）
  用户变量：setUserSessionVariable

handleFieldList:
  SetFromSlave(rwsplit|readonly) —— 可读从

slice.GetConn:
  DirectMaster | DirectSlave | SlaveFallbackMaster（可配置从失败回主）
```

**对 prism**：`checkExecuteFromSlave` 是 Layer B 的权威模板；Begin/AC 状态机跟 Gaea；**USE/SET 大量代理侧**——prism 透传代理需折中（见 A.4）。

### A.4 dbpack RWS — 控制流（全文结论）

```text
ExecutorComQuery switch AST:
  每个分支先查 localTransactionMap —— 命中则 tx.Query，不再 pick
  Set + shouldStartTransaction(autocommit=0/off) → dbGroup.Begin + 存 map
  Set 其它 → 无 tx 时 QueryAll（主+从并发；错误只 log；立刻返回空 Result）
  Begin → Begin+map
  Commit → Commit + Delete map
  Rollback → Rollback；无 SavepointName 才 Delete map
  Insert/Delete/Update → Master
  Select → Slave（可 HasUseDBHint 指定 DS）；无 FOR UPDATE 特殊分支
  default → Slave   // 注意：未知语句偏从！
  XAStart/Prepare/Commit/Rollback 另案

ExecuteUseDB → 直接 error unimplemented
ExecuteFieldList → WithSlave
StmtExecute：无 tx 时 Insert*→Master，Select→Slave，其它 unsupported
ConnectionClose → Rollback 残留 tx

DBGroup.pick:
  IsSlave(ctx) → 按算法选从；无从则 master
  否则选 master
Begin/XAStart → getAvailableMasters()[0]
QueryAll → 对所有主从 go Query，返回空 OK Result（不等待错误汇总）
```

**对 prism**：**不**学 default→Slave、**不**学 QueryAll、**不**学 UseDB unimplemented；学「tx map 优先于语句类型」与「无从回落主」。

### A.5 通读后对设计正文的修正（相对早期草稿）

| 点 | 旧草稿 | 通读后 **prism 锁定** |
|---|---|---|
| `USE` / `COM_INIT_DB` | 一律 master | **更新 `State.Database`；选源：无事务时允许 slave（跟 kingshard `fromSlave=true`），有事务/sticky 仍钉当前连接；Align 保证其它池稍后一致**。Gaea 纯代理侧不打后端——透传场景我们仍要打一台后端执行 `USE`/`COM_INIT_DB`。 |
| `SET` 未知变量 | 转发 master | **保持转发 master**（拒绝 kingshard 吞 OK、拒绝 dbpack QueryAll） |
| `SET NAMES` / `autocommit` | 打 master + State | **State 必更新**；有 sticky 则在 sticky 上执行；无 sticky 时 `autocommit`/`NAMES`：**执行一次 master**（保真），不只改本地位 |
| `BEGIN` | 转发 master | **跟透传：转发 SQL 到 master**（不学 Gaea「先只改 status」除非单测证明需要）；置位以后端 OK 的 `IN_TRANS` 为准，并可本地乐观置位 |
| `COM_FIELD_LIST` | slave | **rwsplit 下从（跟 Gaea/dbpack）；失败回落主。kingshard 走主——我们选 Gaea** |
| 未知语句 default | master | **维持 master（拒绝 dbpack default→Slave）** |
| Prepare | master+reserved | **维持**；注意 kingshard 用完即关后端 prepare——我们 M1 设计是钉连接，与 K 不同但与保真目标一致 |
| SimpleSelect / last_insert_id | — | 可选本地优化后置；第一版当普通 SELECT |

正文 §5.3 M5、§5.8 USE 行、§5.10 `COM_INIT_DB` **以本表为准**（覆盖前文「USE 一律 master」）。

### A.6 与 prism 现状缺口（通读 × 代码）

| 缺口 | 位置 | 闭环 |
|---|---|---|
| 无 `Autocommit` 字段 | `pkg/session` | §5.9；`NeedsSticky` 含 `!AC` |
| `SetInTx` 仅跟 OK 的 IN_TRANS | `proxy/session.go` | 保留；补 AC 显式 SET |
| 单 `Pool` | `proxy.SessionConfig` | `BackendGroup` + Pick |
| 配置键 `backend` | `pkg/config` | `backend_server.mode` |
| 无 Classify/Pick | — | `pkg/router` |
