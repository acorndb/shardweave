# SQL 语法支持矩阵

> 可执行规约：[`test/sqlmatrix/matrix.yaml`](../../test/sqlmatrix/matrix.yaml) +
> [`matrix_test.go`](../../test/sqlmatrix/matrix_test.go)；`go test ./test/sqlmatrix/`。
> 本表是**计划层**判定：`single/rwsplit` 为 raw 透传，`sharding` 为 router/plan 的
> 路由结果。执行期归并层可能再收紧，见 §4。

状态含义：

| 状态 | 含义 |
|---|---|
| 透传 | 单机/读写分离：raw 转发给 MySQL，代理不解析 |
| 单分片 | sharding：可证明落在单一 slice，raw 转发 |
| 多分片归并 | sharding：scatter + merge（concat/order/aggregate/group/distinct） |
| 拒绝 | MySQL ERR 1235，带明确理由；不猜、不静默 |

## 1. single / rwsplit

绝大多数语法是**透传**。代理层显式限制：

| SQL | 结果 | 说明 |
|---|---|---|
| `SELECT SQL_CALC_FOUND_ROWS ...` | 拒绝 | 两种模式都拒绝 |
| `SHOW MASTER STATUS` | 拒绝 | 两种模式都拒绝 |
| `SET NAMES latin1` | 拒绝 | 仅 utf8mb4 |
| `SET SESSION <非白名单变量> = ...` | 拒绝 | 白名单：`time_zone`、`transaction_isolation` |
| `LAST_INSERT_ID()` / `ROW_COUNT()` / `CONNECTION_ID()` 不可改写形态 | 拒绝 | 可改写形态在管线内替换 |
| 多语句、用户变量、临时表、`LOCK TABLES` | 转发但淘汰连接 | 会话状态不可复现 |
| `KILL ...` | 拒绝 | 连接 ID 属于前端会话，透传会误杀池化后端 |
| `SET TRANSACTION ...` / `SET CHARACTER SET ...` / `SET ... = DEFAULT` | **漏网** | 已知缺陷，待修，见 §5 |

prepared statement：single/rwsplit 透传并钉住后端连接；sharding 仅支持可证明单分片的
SELECT（字面量分片键）。

## 2. sharding

以下按 4 片（`s0`…`s3`，`hash_mod`，`user_id % 4` 选片）枚举；`user_id=1` 在 s1，
`user_id=2` 在 s2，`(1,5)` 同片，`(1,3)` 跨片。

### DDL

| SQL | 结果 |
|---|---|
| `CREATE TABLE` / `CREATE INDEX` / `ALTER TABLE` / `DROP TABLE` | 拒绝 |
| `TRUNCATE` / `RENAME` / `CREATE VIEW` | 拒绝 |

DDL 全部拒绝；分片表需预先在各片建好。是否做广播 DDL 见 §5。

### DML

| SQL | 结果 |
|---|---|
| `INSERT ... VALUES`（键字面量，如 `(1,10),(5,20)`） | 单分片 |
| `INSERT ... VALUES`（跨片，如 `(1,10),(3,20)`） | 拒绝 `statement spans multiple shards` |
| `INSERT ... SELECT` | 拒绝 |
| `REPLACE ... VALUES`（可证明单分片） | 单分片 |
| `UPDATE ... WHERE 分片键 = / IN`（同片） | 单分片 |
| `UPDATE` 无分片键 / `UPDATE ... LIMIT` | 拒绝 `WHERE must constrain shard key` |
| 多表 UPDATE | 拒绝 `joins/multiple tables are not supported` |
| `DELETE ... WHERE 分片键 = / IN`（同片） | 单分片 |
| `DELETE` 无分片键 / `DELETE ... LIMIT` | 拒绝 `WHERE must constrain shard key` |
| 多表 DELETE | 拒绝 `multi-table DELETE is not supported` |
| `LOAD DATA` | 拒绝 |

### SELECT / JOIN / 子查询

| SQL | 结果 |
|---|---|
| 分片键等值 / 同片 `IN`（如 `(1,5)`） | 单分片 |
| 无分片键 / 范围 / 跨片 `IN`（如 `(1,3)`） | 多分片归并 |
| `SELECT 1`、常量表达式 | 单分片（默认 slice） |
| `INNER/LEFT/RIGHT/CROSS JOIN`、自连接 | 拒绝 `joins/multiple tables are not supported` |
| 常量标量子查询（`SELECT (SELECT 1) ...`） | 单分片 |
| `EXISTS (SELECT ...)` / `IN (SELECT ...)` | 拒绝 `multi-shard subqueries are not supported` |
| 派生表 `FROM (SELECT ...) x` | 拒绝 `derived tables are not supported` |
| CTE `WITH ...` | 拒绝（CTE 表名未注册为分片表） |
| `UNION` / `UNION ALL` | 拒绝（`SetOprStmt`） |
| 窗口函数 | 拒绝 `multi-shard window function is not supported` |
| 分片键等值 `FOR UPDATE` | 单分片 |
| scatter `LOCK IN SHARE MODE` / `FOR UPDATE` | 拒绝 `multi-shard locking reads are not supported` |
| `INTO OUTFILE` | 拒绝 `multi-shard SELECT INTO is not supported` |
| `SQL_CALC_FOUND_ROWS` | 拒绝（两种模式） |

### 归并算子

| SQL | 结果 |
|---|---|
| `ORDER BY` 列 / `LIMIT` / `LIMIT m,n` | 多分片归并 |
| `COUNT/SUM/AVG/MIN/MAX` | 多分片归并 |
| `COUNT(DISTINCT x)`（单列） | 多分片归并 |
| `GROUP BY` + `HAVING` | 多分片归并 |
| `DISTINCT` | 多分片归并 |
| `GROUP BY ... WITH ROLLUP` | 拒绝 |

### 事务 / 会话 / 其他

| SQL | 结果 |
|---|---|
| `BEGIN` / `START TRANSACTION` / `COMMIT` / `ROLLBACK` / `SAVEPOINT` | 拒绝 |
| `SET TRANSACTION ...` / `SET autocommit` / `SET time_zone` / `SET @var` | 拒绝（`SET is not supported`） |
| `SET NAMES utf8mb4` | 单分片（本地应答） |
| `SET NAMES <非 utf8mb4>` / `SET sql_mode` | 拒绝（两种模式） |
| `USE` / `SHOW TABLES` / `SHOW COLUMNS` / `SHOW CREATE` / `SHOW INDEX` / `DESC` | 单分片 |
| `SHOW STATUS` / `SHOW MASTER STATUS` | 拒绝 |
| `EXPLAIN SELECT ...` | 拒绝；用 `EXPLAIN ROUTE` |
| `CALL` / `DO` / `KILL` / `LOCK TABLES` / `FLUSH` / `PREPARE` / `HANDLER` | 拒绝 |
| 版本注释 `/*!40101 SET NAMES ... */` | 拒绝（按 SET 处理） |
| 普通注释 / optimizer hint + 常量 SELECT | 单分片 |

## 3. 分片模式的组合规则

- 写路径（INSERT/UPDATE/DELETE/REPLACE）只允许**可证明单分片**；跨片写一律 1235。
- 事务内 / `autocommit=0` / pinned 会话拒绝多分片语句（`multiShardRejectReason`）。
- 非分片表在 sharding 模式下不可查询（`table "x" is not sharded`）；需配置规则。
- 复杂语句（JOIN/子查询/CTE/UNION/窗口/DDL）目前不做跨片语义，明确拒绝。

## 4. 执行期额外约束（计划层放行后仍可能被拒）

这些 shape 在计划层是 `MultiShard`，但归并/执行层会拒绝，集成测试覆盖于
`test/sharding/`：

- `ORDER BY` 表达式（如 `ORDER BY amount + 1`）、`ORDER BY` 位置（`ORDER BY 1`）；
- `GROUP BY` 键未出现在输出列、`GROUP BY` 别名、`GROUP BY ... WITH ROLLUP`；
- `HAVING` 混合 OR / 与非分组列比较 / 聚合不在输出列；
- 多列 `COUNT(DISTINCT a, b)`；
- `DISTINCT` 后 `ORDER BY` 非输出列；
- scatter locking read、`INTO OUTFILE`、`SQL_CALC_FOUND_ROWS`。

矩阵测试只锁计划层；执行期约束以 `test/sharding/` 的真代理用例为准。

## 5. 缺口与后续优先级

| 优先级 | 缺口 | 说明 |
|---|---|---|
| P0 | 单分片事务 | `BEGIN/COMMIT/ROLLBACK` + `autocommit=0` + 白名单 SET 在 sharding 下工作，事务内锁同一 slice |
| P0 | DDL 策略 | 当前全部拒绝；明确“预建表”流程，或实现广播 DDL |
| P1 | 同片 JOIN | 所有表落在同一 slice、join key 可证明时放行 |
| P1 | prepared DML（字面量键） | 当前 prepared 只允许 SELECT |
| P1 | SET 白名单在 sharding 下重放 | 复用 rwsplit 的 Align/Effect |
| P2 | UNION ALL、派生表/CTE、窗口、跨片 JOIN | 需要跨片语义设计，长期项 |
| 先修 | `SET TRANSACTION` / `SET CHARACTER SET` / `SET ... = DEFAULT` 漏网 | 连接状态泄漏/跟踪不完整，先于扩大 SET 支持 |

## 6. 如何更新

1. 改 `test/sqlmatrix/matrix.yaml`（新增/调整 case 与预期 status）；
2. `go test ./test/sqlmatrix/` 通过后，同步本文对应行；
3. 若涉及执行期行为，补 `test/sharding/` 或 `test/mtr/` 的真代理用例。
