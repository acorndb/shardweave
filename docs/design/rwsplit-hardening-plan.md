# 读写分离兼容性加固计划

> 状态：计划草案（2026-10-31）
> 范围：single / rwsplit 单机代理；不含分片。
> 关联：[rwsplit-compatibility-gap.md](rwsplit-compatibility-gap.md)、
> [industrial-readiness-gap.md](industrial-readiness-gap.md)、
> [test-infrastructure.md](test-infrastructure.md)
> 提交提示：`docs/design/` 被 `.gitignore` 忽略；入仓需 `git add -f`。

## 0. 一句话

先把 rwsplit 从“字符串启发式选主从”升级为**语句类别 + 会话状态驱动**：
连接级状态/管理语句默认钉 master，读一致性可配置，最后才做 `LAST_INSERT_ID`
之类的本地改写优化；每一阶段都用双 MySQL 的 rwsplit 测试矩阵验收。

## 1. 目标与非目标

### 1.1 目标

- **语义安全**：任何依赖连接级状态、写副作用、复制管理面的语句，要么落到正确的
  后端（master/已 pin 后端），要么明确拒绝；不允许静默返回错误结果。
- **可测试**：路由分类是纯函数；rwsplit 有双后端集成矩阵；`-short` 不依赖 Docker。
- **可配置**：read-after-write、从库健康/延迟策略可配置，默认不改变现有行为。
- **可演进**：为后续 `LAST_INSERT_ID` 本地改写、session tracking、分片路由留接口。

### 1.2 非目标

- 分片路由/改写/归并。
- 完整 MySQL 语义模拟（存储过程、触发器内部状态）。
- 跨主从强一致（只提供 read-after-write 窗口/显式策略）。
- 查询防火墙、审计、多租户。

## 2. 现状与根因

- `pkg/route/source/classify.go`：`SELECT`/`SHOW`/`EXPLAIN` 默认 slave；
  写语句/SET 默认 master；`FOR UPDATE`、`@@read_only`、`/*master*/` 强制 master。
- `pkg/route/source/pick.go`：`NeedsSticky()` 优先 master；`COM_STMT_*` master；
  `COM_INIT_DB`/`COM_FIELD_LIST` slave。
- `pkg/session/state.go`：只有 database/inTx/autocommit/reserved/charset。
- `pkg/backend/conn.go` `Align`：只发 `COM_INIT_DB` + `SET NAMES charset`。
- `pkg/mysql/forward.go`：解析 status，但不解析 OK 的 `last_insert_id`/`affected_rows`/`warnings`。
- `pkg/sql/parser` 已引入 TiDB parser，但数据面未使用；`Statement.AST` 仍为空。
- 测试：没有 rwsplit 双后端矩阵；现有差分是 single/透传。

根因：**“SQL 文本关键字”不足以判断语义**。`SELECT LAST_INSERT_ID()`、
`SHOW WARNINGS`、`SET sql_mode` 的读/写/连接级属性都不是首关键字能表达的。

## 3. 工作流

### A. 语句分类（StatementClass）

新增纯函数分类，输入 `sql.Statement`（Cmd/Raw/AST），输出：

```go
type Class uint8

const (
    ClassReadOnly        Class = iota // 无副作用、无连接级状态
    ClassWrite                        // 写/DDL/有副作用
    ClassConnectionState              // 依赖当前连接状态
    ClassAdmin                        // 复制/进程/全局管理
    ClassUnsupported                  // 明确拒绝
)
```

建议规则（AST 优先，token fallback）：

| 类别 | 典型语句 |
|---|---|
| Write | INSERT/UPDATE/DELETE/REPLACE/DDL、`LOAD DATA`、`SELECT ... INTO OUTFILE/DUMPFILE`、`CALL`、`DO` |
| ConnectionState | `LAST_INSERT_ID()`、`FOUND_ROWS()`、`ROW_COUNT()`、`@@warning_count`、`@@error_count`、`SHOW WARNINGS/ERRORS`、SQL `PREPARE/EXECUTE/DEALLOCATE`、`HANDLER`、用户变量、临时表、`LOCK TABLES`、`GET_LOCK`/`RELEASE_ALL_LOCKS`/`IS_*_LOCK` |
| Admin | `SHOW MASTER STATUS`、`SHOW BINARY LOG[S]`、`SHOW SLAVE/REPLICA STATUS`、`SHOW PROCESSLIST`、`SHOW ENGINE`、`KILL`、`FLUSH`、`RESET`、`CHANGE MASTER/REPLICA` |
| ReadOnly | 普通 SELECT/SHOW/EXPLAIN/DESCRIBE，且不含上述内容 |

`PickRole` 改为消费分类：

```text
ClassWrite / ClassConnectionState / ClassAdmin → master
ClassReadOnly → slave（事务/sticky/一致性窗口优先）
ClassUnsupported → 明确错误
```

分类结果要能解释：`RouteDecision{Class, Role, Reason}`，供日志/EXPLAIN/测试断言。

### B. 会话状态与对齐

`session.State` 扩展（按需）：

```go
type State struct {
    // 现有
    database, charset string
    inTx, autocommit, reserved bool

    lastInsertID uint64
    rowCount     int64
    warningCount uint16
    masterUntil  time.Time          // read-after-write
    vars         map[string]string  // 白名单系统变量
    stmtCount    int                // 打开的 prepared stmt 数
}
```

- `mysql.ForwardResponseInfo` 返回 `AffectedRows`/`LastInsertID`/`Warnings`/
  `ResultSet`；仅成功 OK 更新；ERR 不更新。`ForwardResponseResult` 保留兼容。
- 已实现：`LAST_INSERT_ID()`/`ROW_COUNT()` 从 OK 元数据写入 session，精确的
  `SELECT <func>() [AS alias]` 改写成 `CAST(...)` 字面量；非精确形式、
  prepared 形式、`FOUND_ROWS()`/`SQL_CALC_FOUND_ROWS` 返回 1235。
- `AlignSettings` 增加 `Vars`；`MySQLConn.Align` 重放白名单变量（或先强制 master）。
- `ParseSessionEffect` 扩展：
  - `SET autocommit=TRUE/FALSE`、`:=`、`@@SESSION`、`LOCAL`；
  - `SET NAMES ... COLLATE` 同时记录 charset + collation；
  - `SET sql_mode/time_zone/character_set_*/collation_*/transaction_isolation/...`
    进入 `vars` 白名单。
- `COM_RESET_CONNECTION` 清理 vars/lastInsertID/rowCount/stmtCount，
  保留 database（现有语义）。
- 白名单未覆盖的 SET：保守 master + sticky，直到实现。

### C. 路由与一致性策略

- **Sticky/pin**：`ClassConnectionState`/`ClassAdmin` 语句默认 master；
  若 session 已 pin，沿用 pin。
- **USE / COM_INIT_DB**：默认 master（schema of record），Align 到当前库；
  不再默认 slave，避免从库 schema 差异。
- **SHOW 策略**：Admin 类走 master；普通 `SHOW TABLES/COLUMNS` 可 slave。
- **read-after-write**：写成功后设置 `masterUntil = now + window`；
  窗口内读走 master。配置 `rwsplit.read_after_write`，默认 `0s`（关闭，保持现状）。
- **从库选择**：先保持 round_robin/random + acquire 失败回落；
  后续加 health/lag/weights，延迟超阈值排除。
- **失败重试**：幂等读在从库失败时换下一个从库/master；写不重试。
- **Prepared**：`COM_STMT_*` 继续 master + sticky；`COM_STMT_CLOSE` 计数归零时
  释放 reserved。
- **显式不支持（已落地）**：下列语句不尝试兼容，`plan.Build` 直接 Reject
  （1235 / 42000），`proxy` 写 ERR 后保持会话：
  - `SHOW WARNINGS` / `SHOW ERRORS` / `SHOW COUNT(*) WARNINGS|ERRORS`
  - `SHOW MASTER STATUS` / `SHOW BINARY LOGS` / `SHOW SLAVE|REPLICA STATUS`
  - `SET sql_mode`（含 `SESSION` / `GLOBAL` / `@@` 形式）

  检测在 `pkg/sql/unsupported.go`，按 COM_QUERY 文本 + 注释剥离实现；后续
  `StatementClass` 落地时可替换为 AST 分类。其他会话变量仍按 R1 的 pin 策略处理。

### D. 命令与协议兼容

- `COM_STMT_SEND_LONG_DATA`：转发到当前 prepared 所在后端。
- `COM_FIELD_LIST`：明确支持或返回稳定错误（不要 1047）。
- `COM_SET_OPTION`：跟踪/转发 multi-statements 开关。
- `COM_CHANGE_USER`：重置会话并重新选源；复杂，可先明确拒绝。
- `LOCAL INFILE`：支持转发或明确拒绝，不静默断连。
- `COM_STATISTICS`/`COM_PROCESS_KILL`：管理类，稳定错误或转发。

### E. 本地改写优化（后置）

只在**安全可证明**时做：

- 捕获 `lastInsertID` 后，把简单文本协议
  `SELECT LAST_INSERT_ID()` → `SELECT <id> AS \`LAST_INSERT_ID()\``；
  带别名/常量表达式时按 AST 替换。
- `LAST_INSERT_ID(expr)`、`CALL`、prepared、复杂表达式 → 不 rewrite，pin master。
- `FOUND_ROWS()`/`ROW_COUNT()`/warnings 先 pin，后续再评估捕获/改写。
- 需要 `pkg/sql/rewrite` + 列名保持 + 客户端 capability/deprecateEOF 处理。

### F. 观测

- 路由决策日志：`class`、`role`、`reason`、`pinned`、`read_after_write`。
- metrics：master/slave 命中、connection-state fallback、slave error/retry、
  read-after-write pin 次数。
- 慢查询/错误日志（后续）。

## 4. 阶段计划

| 阶段 | 内容 | 退出条件 |
|---|---|---|
| **R0 分类骨架** | `StatementClass` + `RouteDecision`；TiDB AST 解析用于 COM_QUERY；token fallback；单元矩阵 | 分类用例全绿；无行为变化（仅日志） |
| **R1 正确性 pin** | ConnectionState/Admin/写 → master；`INTO OUTFILE`；SQL `PREPARE`/`HANDLER`；USE/INIT_DB master；`session.vars` 跟踪/对齐 | P0 矩阵集成全绿；无静默错结果 |
| **R2 测试基建** | `test/rwsplit` 双 MySQL（容器）；写主读从、事务、prepared、admin 用例；差分 vs direct master | 矩阵可一键跑；`-short` Skip |
| **R3 一致性** | `read_after_write` 窗口；从库失败换库；延迟/健康策略 | 写后读、延迟、杀从库用例通过 |
| **R4 会话函数** | 已实现 `LAST_INSERT_ID()`/`ROW_COUNT()`：OK 元数据 + 精确 SELECT 改写；`FOUND_ROWS()`/`SQL_CALC_FOUND_ROWS` 与非精确形式显式 1235 | 真实拓扑用例通过 |
| **R5 命令/观测** | SEND_LONG_DATA/FIELD_LIST/SET_OPTION/CHANGE_USER/LOCAL INFILE；路由 metrics/log | 驱动兼容冒烟；指标可见 |

R0/R1 先做，R2 与 R1 并行（测试先行）；R3 之后才考虑 R4 改写，避免在语义未锁定时过早优化。

## 5. 测试矩阵

### 5.1 单元（无 Docker）

| 用例 | 断言 |
|---|---|
| `Classify` 表驱动：读写/连接级/管理/不支持 | Class 正确 |
| `PickRole`：事务、sticky、读/写/连接级 | Role 正确 |
| `ParseSessionEffect`：TRUE/FALSE、`:=`、COLLATE、多变量 | Effect 正确 |
| `SessionState.Reset` | lastInsertID/vars/stmt 清零，DB 保留 |
| `read_after_write` 窗口（fake clock） | 窗口内 master，窗口外可 slave |
| `ForwardResponseResult` OK 解析 | last_insert_id/affected_rows/warnings |
| `StatementClass` fuzz | 不 panic |

### 5.2 rwsplit 集成（Docker）

| 用例 | 期望 |
|---|---|
| INSERT 后 `SELECT LAST_INSERT_ID()` | 正确 id |
| `SQL_CALC_FOUND_ROWS` + `FOUND_ROWS()` | 正确 |
| UPDATE/DELETE + `ROW_COUNT()` | 正确 |
| 写后 `SHOW WARNINGS` / `@@warning_count` | 正确 |
| `SET sql_mode/time_zone/charset/collation` 后读 | 与 master 一致 |
| `SET autocommit=TRUE/FALSE` | 选源正确 |
| `SET TRANSACTION` + `START TRANSACTION` | 同一连接/主库 |
| SQL `PREPARE`/`EXECUTE`/`DEALLOCATE` | 同一连接 |
| `HANDLER OPEN/READ/CLOSE` | 同一连接 |
| `USE`/`COM_INIT_DB` | 主库成功，后续读对齐 |
| `SHOW MASTER STATUS`/`BINARY LOGS`/`SLAVE STATUS`/`PROCESSLIST` | 不落错实例 |
| `SELECT ... INTO OUTFILE` | 不在从库写/明确拒绝 |
| 事务中读 | master，sticky |
| 写后立即读（read_after_write 开启/关闭） | 分别命中 master/slave |
| 杀从库 / 延迟从库 | 排除/回落/重试策略 |
| `COM_STMT_SEND_LONG_DATA` | 正确执行 |
| 并发混合读写 | 无错结果、无泄漏 |

### 5.3 差分/变形

- rwsplit 代理 vs 直连 master：写/连接级状态结果一致。
- 只读场景：rwsplit vs 单库结果一致（允许配置的延迟窗口例外）。
- 与 `test/differential` 的 CalibrationScript 合并，每个 Bug 落语料。

## 6. 配置草案

```yaml
backend:
  mode: rwsplit
  slices:
    - name: default
      master: { ... }
      slaves: [ ... ]
  load_balance: round_robin
  rwsplit:
    read_after_write: 0s   # 0 = 关闭；建议 1s
    max_replica_lag: 0s    # 0 = 不按延迟过滤
```

- 默认全部保持现有行为（read_after_write=0，lag 不过滤）。
- 后续可加 weights/local_preference。

## 6.5 实现时对分片友好（轻量）

不提前造分片抽象，只避免把“单后端”固化进类型/接口：

- `Plan.Targets` 保持切片、`Runtime.leases` 保持 map（现状已如此），rwsplit 直接用；
- 新增会话状态（vars、lastInsertID、warnings）放 `session.State`，不放后端连接或租约；
- 路由/分类输出目标集合，不要输出单个 conn；
- 不在 rwsplit 阶段预建 `StmtTable`、`LeaseSet`、`TxPolicy`、merger 接口，等分片设计定稿再加。

判断标准：新增代码里尽量避免 `Targets[0]` 这类“只能一个后端”的写法；其余不做过早抽象。

## 7. 风险

- **AST 开销**：COM_QUERY 才解析；简单语句可 token 快路径；注意不要二次 parse。
- **会话变量爆炸**：只做白名单；未覆盖的 SET 保守 master。
- **一致性默认**：read_after_write 默认关闭，避免改变现有性能特征。
- **测试环境**：真复制测试重；优先用双独立 MySQL + fake lag/clock，真复制作为 nightly。
- **Prepared**：读 prep 仍不拆分；先保证正确性。
- **分类误判**：不确定时 master；每条规则都有表驱动用例和开源 ref。

## 8. 优先级

**P0**：R0 分类、R1 pin、R2 基础矩阵、read-after-write。
**P1**：会话变量完整白名单、从库健康/延迟/重试、prepared 生命周期、命令覆盖。
**P2**：`LAST_INSERT_ID`/`FOUND_ROWS` 本地改写、权重/本地优先、查询规则。
