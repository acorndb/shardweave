# 单机能力与读写分离兼容性评估

> 状态：评估报告（2026-10-31）
> 范围：单机代理（single / rwsplit），不含分片。
> 上位：[industrial-readiness-gap.md](industrial-readiness-gap.md)、[module-map.md](module-map.md)
> 提交提示：`docs/design/` 被 `.gitignore` 忽略；入仓需 `git add -f`。

## 0. 结论

单机核心路径已经可用，池、packet 上限、I/O 超时、max_connections 已在最近几轮补齐。
但距离生产仍缺：**Auth/TLS、协议能力协商、优雅 drain、查询总超时、可观测/管理面、
会话变量、prepared 生命周期、故障重试**。

**rwsplit 不只是“读从库”的性能问题，存在真实兼容性风险**：

1. 连接级函数/状态（`LAST_INSERT_ID()`、`FOUND_ROWS()`、`ROW_COUNT()`、
   `SHOW WARNINGS`、`@@warning_count`）可能被路由到另一个后端。
2. `SET sql_mode` / `time_zone` / `character_set_*` / `collation_*` /
   `transaction_isolation` 等会话变量没有跟踪/对齐，读从库时可能不生效。
3. SQL 级 `PREPARE` / `EXECUTE` / `HANDLER` 是连接级状态，但没有 pin。
4. `SHOW MASTER STATUS` / `SHOW BINARY LOGS` / `SHOW SLAVE STATUS` /
   `SHOW PROCESSLIST` 当前按 `SHOW` 走从库，语义可能错误。
5. `SELECT ... INTO OUTFILE` 是写操作，但按 `SELECT` 走从库。
6. 读后写一致性（read-after-write）、从库延迟、查询失败换库都没有策略。
7. COM_STMT_* 一律 master，读 prep 不拆分；`COM_STMT_SEND_LONG_DATA` 等命令不支持。

## 1. 单机剩余能力

| 能力 | 现状 | 优先级 |
|---|---|---|
| Auth/TLS（caching_sha2、前端/后端 TLS） | 未实现 | **P0** |
| 协议能力协商（deprecateEOF、session_track、命令覆盖） | 硬编码/部分命令 1047 | **P0** |
| 优雅退出 / drain（`termination_drain`） | 未接线 | **P1** |
| 查询总超时（`MAX_EXECUTION_TIME`） | 未实现；read/write timeout 只是单次 I/O | **P1** |
| 每用户/IP 限额 | 只有单实例 `max_connections` | **P1** |
| metrics / healthz / admin / access log | 未实现 | **P0/P1** |
| 会话变量跟踪与对齐 | 只有 db/charset/autocommit/reserved | **P1** |
| Prepared statement 生命周期 | 只透传；无 stmt 表；LONG_DATA 不支持 | **P1** |
| 后端失败重试 / 从库健康 / 延迟 / 权重 | 只有 acquire 失败回落 master | **P1** |
| fuzz / 故障注入 / 负载 / MTR/RQG | 仅有基础差分与单测 | **P1** |
| 压缩协议 | 未实现 | P2 |

## 2. 当前 rwsplit 模型

```text
proxy: DetectReservedSQL(raw) → state.reserved
       ↓
plan/source.Pick:
  1. state.NeedsSticky()? → master
  2. command: COM_STMT_* → master; COM_INIT_DB/COM_FIELD_LIST → slave
  3. COM_QUERY: ClassifyWantSlave(sql) → master/slave
       ↓
execute: Align(db, charset) → 执行
```

`ClassifyWantSlave` 是字符串启发式（`pkg/route/source/classify.go`）：
`SELECT`/`SHOW`/`EXPLAIN` 默认 slave；写语句/SET/BEGIN 等默认 master；
`FOR UPDATE`、`LOCK IN SHARE MODE`、`/*master*/`、`@@read_only` 等强制 master。

## 3. 兼容性问题清单

### 3.1 连接级函数与状态（P0）

这些值属于“当前后端连接”，被路由到另一个后端就会错：

| SQL | 当前行为 | 影响 |
|---|---|---|
| `SELECT LAST_INSERT_ID()` | `SELECT` → slave | 常见 ORM 模式：INSERT 后取 id 得到 0/旧值 |
| `SELECT LAST_INSERT_ID(expr)` | `SELECT` → slave | 在从库上修改了错误的会话值 |
| `SELECT FOUND_ROWS()` | `SELECT` → slave | `SQL_CALC_FOUND_ROWS` 后结果错误 |
| `SELECT ROW_COUNT()` | `SELECT` → slave | 上一语句影响行数错误 |
| `SELECT CONNECTION_ID()` | `SELECT` → slave | 同一前端会话看到不同连接 ID |
| `SELECT @@warning_count` / `@@error_count` | `SELECT` → slave（非 read_only） | 警告/错误计数错误 |
| `SHOW WARNINGS` / `SHOW ERRORS` | `SHOW` → slave | 看不到上一语句（可能在 master）的警告 |
| `SHOW COUNT(*) WARNINGS` | `SHOW` → slave | 同上 |
| `SELECT IS_FREE_LOCK()` / `IS_USED_LOCK()` | `SELECT` → slave（若当前未 reserved） | 锁状态查询错库 |

`DetectReservedSQL` 只识别 `GET_LOCK(`/`RELEASE_LOCK(`、临时表、`LOCK TABLES`、
用户变量；不覆盖上表。

### 3.2 会话变量（P0/P1）

`session.State` 只有 database/inTx/autocommit/reserved/charset；
`backend.Align` 只发 `COM_INIT_DB` + `SET NAMES charset`。因此：

- `SET sql_mode = ...`、`SET time_zone = ...`、`SET optimizer_switch = ...`、
  `SET foreign_key_checks = ...`、`SET character_set_results = ...`、
  `SET collation_connection = ...`、`SET SESSION TRANSACTION ISOLATION LEVEL ...`
  只在执行该 SET 的后端生效；下一次读去从库时不带这些设置。
- `SET NAMES utf8mb4 COLLATE utf8mb4_bin` 只记录 charset，不记录 collation；
  `Align` 发的是 `SET NAMES utf8mb4`，collation 丢失。
- `SET autocommit=TRUE/FALSE` 不被 `ParseSessionEffect` 识别（只识别 1/0/ON/OFF），
  状态可能过期，进而影响选主/选从和 sticky。
- `SET TRANSACTION READ ONLY` / `SET TRANSACTION ISOLATION LEVEL` 后紧接
  `START TRANSACTION`，SET 可能留在 master 连接，事务却在另一条连接开始。

### 3.3 SQL 级会话状态（P0）

- `PREPARE stmt FROM ...` / `EXECUTE stmt` / `DEALLOCATE PREPARE stmt`：
  默认走 master，但没有 reserved/sticky；前后语句可能落在不同 master 连接，
  `EXECUTE` 找不到 prepared statement。
- `HANDLER tbl OPEN` / `HANDLER tbl READ` / `HANDLER tbl CLOSE`：
  默认走 master，但没有 sticky；`HANDLER READ` 可能换连接，handler 不存在。
- `GET_LOCK` 已被 reserved 覆盖；但 `RELEASE_ALL_LOCKS()` 只在已 reserved 时安全。

### 3.4 SELECT 里隐藏的写/副作用（P1）

- `SELECT ... INTO OUTFILE` / `INTO DUMPFILE`：按 SELECT 走 slave，文件会写从库。
- `SELECT LAST_INSERT_ID(expr)`：修改连接级状态（见 3.1）。
- `SELECT GET_LOCK(...)`：已被 reserved 覆盖。
- `SELECT ... FOR UPDATE` / `LOCK IN SHARE MODE`：已强制 master。

### 3.5 SHOW / 管理 / 复制（P1）

当前 `ClassifyWantSlave` 对 `SHOW` 默认返回 slave（除非 `read_only`）：

- `SHOW MASTER STATUS`、`SHOW BINARY LOGS`、`SHOW BINARY LOG STATUS`：
  从库上可能为空/不同。
- `SHOW SLAVE STATUS` / `SHOW REPLICA STATUS`：会随机落到某个从库，
  未必是用户想看的那个；主库上语义也不同。
- `SHOW PROCESSLIST`：只看到该从库的会话，不是全局。
- `SHOW ENGINE INNODB STATUS`、`SHOW STATUS`、`SHOW VARIABLES`：
  服务器级状态，从库与主库可能不同。

### 3.6 USE / COM_INIT_DB（P1）

`USE db` 和 `COM_INIT_DB` 在非事务时走 slave（`ClassifyWantSlave` 返回 true /
`PickRole` 返回 slave）。如果逻辑库在主从上的存在性/名称不同：

- 在从库执行 `USE` 成功、主库不存在 → 后续写失败；
- 或从库不存在 → 读直接失败；
- `Align` 到另一后端时会重发 `COM_INIT_DB`，错误会延迟暴露。

### 3.7 Prepared statement（P1）

- `COM_STMT_*` 在 `PickRole` 中一律 master，**读 prep 不会走从库**；
  安全性 OK，但读扩展性丢失。
- `COM_STMT_PREPARE` 会 `MarkReserved`，但 `COM_STMT_CLOSE` 不会释放；
  长连接会永久占住 master，池利用率下降。
- `COM_STMT_SEND_LONG_DATA` 未处理（proxy 默认 1047）。
- 无 stmt 表/ID 映射；后端失效后客户端 stmt id 无明确错误语义。

### 3.8 一致性与故障（P1）

- **没有 read-after-write**：INSERT/UPDATE/COMMIT 后的 SELECT 可能读到延迟从库。
- 没有从库延迟检测（`Seconds_Behind_Master` / performance_schema）。
- 没有从库健康分/权重/最小连接数/本地优先。
- 从库 query 失败不会换库重试；只有 `Slice.Acquire` 失败时回落 master。
- 没有 `/*slave*/` / 读一致性 hint。

### 3.9 命令覆盖（P1）

- `COM_CHANGE_USER`、`COM_FIELD_LIST`、`COM_SET_OPTION`、
  `COM_STMT_SEND_LONG_DATA`、`COM_STATISTICS`、`COM_PROCESS_KILL` 等未处理；
  部分客户端/ORM 会用。
- `COM_CHANGE_USER` 还涉及会话 reset、用户/库切换，rwsplit 下需要重新选源。

### 3.10 实测确认（2026-10-31）

环境：`test/rwsplit` 真实拓扑（1 master + 2 replica，GTID 复制，从库
`read_only`）。下表是**修复前**同一会话分别直连 master 与经代理的观测值：

| 语句序列 | 经代理（修复前） | 直连 master |
|---|---|---|
| INSERT 后 `SELECT LAST_INSERT_ID()` | 0 | 2 |
| `SET SESSION sql_mode='ANSI_QUOTES'` 后 `SELECT @@SESSION.sql_mode` | 默认 sql_mode | `ANSI_QUOTES` |
| UPDATE（1 行）后 `SELECT ROW_COUNT()` | 0 | 1 |
| `INSERT IGNORE`（1 warning）后 `SELECT @@warning_count` | 0 | 1 |
| 同上 `SHOW WARNINGS` | 空 | 1 行（1366） |
| `SQL_CALC_FOUND_ROWS ... LIMIT 1` 后 `SELECT FOUND_ROWS()` | 1 | 4 |
| `SHOW MASTER STATUS` | 从库位置 3001203 | 主库位置 2646 |

这些与 §3.1/§3.2/§3.5 的预测一致，确认为真实缺陷，不是理论风险。

后续落地（同日）：

- **已支持** `LAST_INSERT_ID()` / `ROW_COUNT()`：解析 OK 包的
  `last_insert_id`/`affected_rows` 写入 `session.State`；精确的
  `SELECT LAST_INSERT_ID()` / `SELECT ROW_COUNT()`（含别名）改写成
  `CAST(...)` 字面量，由现有转发链路返回。prepared 形式暂不支持（prepare
  时返回 1235）。
- **显式拒绝** `FOUND_ROWS()` / `SQL_CALC_FOUND_ROWS`（1235）：响应包没有
  忽略 LIMIT 的总行数，需要额外 COUNT 查询，暂不实现。
- `LAST_INSERT_ID(expr)`、`SELECT LAST_INSERT_ID() + 1` 等非精确形式返回
  1235，避免静默返回错误值。
- `ROW_COUNT()` 语义：默认 -1；DML 取 `affected_rows`；DDL/SET/BEGIN/COMMIT
  等为 0；错误为 -1。

`SHOW MASTER STATUS` 这类 Admin 语句的产品策略是**显式不支持**：`plan.Build`
直接返回 Reject（1235 / 42000），`proxy` 写 ERR 后保持会话，不再按 `SHOW` 落到
从库。已落地的拒绝清单：

- `SHOW WARNINGS` / `SHOW ERRORS` / `SHOW COUNT(*) WARNINGS|ERRORS`
- `SHOW MASTER STATUS` / `SHOW BINARY LOGS` / `SHOW SLAVE|REPLICA STATUS`
- `SET sql_mode`（含 `SESSION` / `GLOBAL` / `@@` 形式）

客户端应改用 OK 包里的 warning_count/affected_rows 等元数据；其他会话变量
（`time_zone`、`transaction_isolation` 等）尚未决策，仍属于已知缺口。

同轮验证中，以下 SQL 类型在直连 vs 代理对比里一致：DDL/DML 生命周期、
JOIN/GROUP BY/HAVING/UNION/CTE/递归 CTE/窗口函数/JSON/字符串日期函数、SHOW
TABLES/COLUMNS/INDEX/CREATE TABLE、prepared 读写、事务/保存点/autocommit/
FOR UPDATE/FOR SHARE、多表 UPDATE/DELETE、INSERT ... SET、CREATE TABLE AS
SELECT、RENAME/VIEW、CALL 多结果集、多语句 COM_QUERY、临时表/用户变量/命名锁/
LOCK TABLES、5 类错误码、utf8mb4 中文、二进制 blob、以及 16MiB+1 的大包往返。

## 4. 修复建议

### 4.1 连接级状态优先 master/pin

把“连接级语句/函数”单独归类，默认 master + sticky（或至少 pin 到当前后端）：

- 已支持：`LAST_INSERT_ID()`、`ROW_COUNT()`（OK 元数据 + 本地改写；仅精确
  `SELECT <func>() [AS alias]`，prepared 形式拒绝）。
- 仍拒绝：`FOUND_ROWS()`、`SQL_CALC_FOUND_ROWS`（1235）。
- 仍需处理：`CONNECTION_ID()`、`@@warning_count`/`@@error_count`、
  `LAST_INSERT_ID(expr)`、SQL `PREPARE`/`EXECUTE`/`HANDLER`。
- 语句：`SHOW WARNINGS`、`SHOW ERRORS`、`SHOW COUNT(*) WARNINGS` 已显式拒绝；
  `SHOW MASTER STATUS`/`BINARY LOGS`/`SLAVE STATUS` 已显式拒绝。

实现位置建议：`pkg/route/source` 升级成明确的 `StatementClass`（token/AST 均可），
输出 `ReadOnly` / `Write` / `ConnectionState` / `Admin` / `ReplicaState`，
`PickRole` 只消费分类结果。不要在 `proxy` 里继续堆字符串判断。

### 4.2 会话变量跟踪与对齐

- 跟踪 `sql_mode`、`time_zone`、`character_set_*`、`collation_*`、
  `transaction_isolation`、`optimizer_switch`、`foreign_key_checks` 等白名单。
- `Align` 时重放这些变量；或拒绝/强制 master，直到实现。
- `SET NAMES ... COLLATE` 同时记录 charset + collation。
- 利用后端 `CLIENT_SESSION_TRACK` 的 OK/EOF 信息更新，而不是纯解析。
- 扩展 `ParseSessionEffect` 的 `autocommit` 语法（TRUE/FALSE/`:=`）。

### 4.3 read-after-write

- 写语句成功后，在可配置窗口（如 1–5s）内把会话 pin 到 master；或直到显式
  `COMMIT`/`ROLLBACK`/`/*slave*/`。
- 窗口结束再允许读从库。
- 事务内始终 master（已是现状）。

### 4.4 SHOW / ADMIN 策略

- `SHOW MASTER STATUS`、`SHOW BINARY LOG[S]`、`SHOW SLAVE/REPLICA STATUS`：
  走 master 或拒绝（取决于拓扑）。
- `SHOW PROCESSLIST`、`SHOW STATUS`、`SHOW VARIABLES`：可配置“master/从库/拒绝”，
  默认 master 更安全。
- `SELECT ... INTO OUTFILE/DUMPFILE`：明确拒绝或 master。

### 4.5 Prepared statement

- 短期：`COM_STMT_PREPARE`/`EXECUTE`/`CLOSE` 在 master 上 sticky；`CLOSE` 在
  stmt 计数归零时释放 reserved。
- 中期：stmt 表 + 允许无副作用 SELECT 的 prep 走从库（复杂，后置）。
- 补 `COM_STMT_SEND_LONG_DATA`。

### 4.6 从库健康与故障

- 后台探活/延迟采集；选源排除不健康/高延迟从库。
- 幂等读在从库失败时换下一个从库/master 重试；写不重试。
- 权重/本地优先/最小连接数。

## 5. 测试矩阵（rwsplit）

| 用例 | 期望 |
|---|---|
| `INSERT` 后 `SELECT LAST_INSERT_ID()` | 返回正确 id（不得路由到从库） |
| `SELECT SQL_CALC_FOUND_ROWS` 后 `FOUND_ROWS()` | 正确 |
| `UPDATE` 后 `SELECT ROW_COUNT()` | 正确 |
| 写后 `SHOW WARNINGS` | 正确 |
| `SET sql_mode` 后 `SELECT @@sql_mode` | 正确（对齐或 master） |
| `SET NAMES ... COLLATE` 后读 | collation 正确 |
| `SET autocommit=FALSE` 后 SELECT | 不读从库 |
| `PREPARE`/`EXECUTE` | 同一连接 |
| `HANDLER OPEN/READ` | 同一连接 |
| `SHOW MASTER STATUS` | 不落到从库 |
| `SELECT ... INTO OUTFILE` | 不在从库写 |
| INSERT 后立即 SELECT | read-after-write 策略生效 |
| 从库延迟/挂掉 | 排除/回落/重试 |

建议先做 rwsplit 双 MySQL 容器集成测试，再逐步把用例转成差分/变形语料。

## 6. 优先级

**P0（会返回错误结果）**

1. `LAST_INSERT_ID` / `FOUND_ROWS` / `ROW_COUNT` / `SHOW WARNINGS` / `@@warning_count`。
2. `SET sql_mode` / `time_zone` / charset/collation / transaction isolation。
3. SQL 级 `PREPARE`/`EXECUTE`/`HANDLER`。
4. read-after-write。

**P1（语义/运维风险）**

5. `SHOW MASTER/SLAVE/BINARY/PROCESSLIST` 策略。
6. `SELECT ... INTO OUTFILE`。
7. Prepared statement 生命周期与 LONG_DATA。
8. 从库健康/延迟/权重/失败重试。
9. 其余命令覆盖。

**P2**

10. 读一致性 hint、权重策略、query rules、多租户。
