---
title: collation 来源（方案 C）与会话变量重放（B2）review
files:
  - pkg/backend/conn.go
  - pkg/mysql/client/conn.go
  - pkg/sql/classify.go
  - pkg/session/state.go
  - pkg/execute/pipeline.go
---

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
