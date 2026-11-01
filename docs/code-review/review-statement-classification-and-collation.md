---
title: 语句判定统一（A 轨）与字符串 collation 归并（D1–D4）review
files:
  - pkg/sql/scanner.go
  - pkg/sql/classify.go
  - pkg/sql/statement.go
  - pkg/sql/session_func.go
  - pkg/proxy/serve.go
  - pkg/execute/pipeline.go
  - pkg/plan/plan.go
  - pkg/route/source/pick.go
  - pkg/route/source/routers.go
  - pkg/route/shard/router.go
  - pkg/merge/collation.go
  - pkg/merge/ordered.go
  - pkg/merge/sort.go
  - pkg/merge/group.go
  - pkg/merge/distinct.go
  - pkg/merge/aggregate.go
  - pkg/merge/aggregate_distinct.go
  - pkg/merge/filter.go
  - pkg/result/type.go
  - pkg/mysql/collation
  - pkg/mysql/client/conn.go
---

## Must Fix

### 列 collation 取自结果集元数据是错的，collated 归并在部分场景静默出错
- status: wontfix（2026-11-01 用户决策：暂不修复，保持功能启用）
- file: pkg/merge/collation.go、test/metamorphic/collation_test.go

D5 oracle（2026-11-01 触发）发现：MySQL ColumnDefinition41 的 charset 字段不携带列的
真实 collation，而是**结果字符集的 collation**。探针进一步定位到触发条件：**代理连接池
每次 Release 都发 `COM_RESET_CONNECTION`，它会把会话字符集重置回服务端默认**。实测同一条
连接：握手 45 时首条查询元数据为 45；`COM_RESET_CONNECTION` 之后变为 255（服务端默认）。
因此每个池化连接只有第一条查询是 45，之后都是 255——oracle 中「ORDER BY 报 45、
GROUP BY/聚合报 255」源于连接新旧，不是查询形态。

真实 collation 本身是**可获得的**（已实测）：`information_schema.columns.COLLATION_NAME`
与 `SELECT COLLATION(col)` 都返回正确值，与连接 charset 无关。缺的是把它接进 merge 的
管道，不是数据不可得。另注：握手固定 45 只对首条查询有效；若后端默认字符集不是
utf8mb4，Reset 后结果会被转码成服务端默认，「只支持 utf8mb4」的假设在首查后失效。

**影响矩阵（实测）**：

| 表列 collation | ORDER BY | GROUP BY / DISTINCT / MIN/MAX |
|---|---|---|
| `utf8mb4_0900_ai_ci`（MySQL 8 默认） | 用 45（PAD）；尾随空格及部分权重与 0900 不同 → 可能错 | 用 255 → 正确 |
| `utf8mb4_general_ci` | 用 45 → 正确 | 用 255（NO PAD）→ 尾随空格分组错误 |
| `utf8mb4_bin` | 用 45（CI）→ 大小写排序错误 | 用 255 → 大小写/尾随空格错误 |

**不修理由**：用户决定先保持启用，接受上述已知场景的风险；oracle 测试
`test/metamorphic/collation_test.go` 保留为失败复现（它不在 `make test-integration`
的用例列表中，仓库 CI 门禁不受影响）。后续如需修复，方案见
merge-engine-design §13.8（A: catalog；B: 隐藏 `COLLATION()` 列），届时把本项改回 open。

### PAD SPACE 未实现，GROUP BY/DISTINCT 对尾随空格错误
- status: fixed
- file: pkg/merge/collation.go

原问题：`utf8mb4_general_ci` 与 `utf8mb4_bin` 是 PAD SPACE collation，MySQL 比较时忽略
尾随空格（实测：`'a'='a '` 返回 1，GROUP BY 合并为 1 组）。vendored 包的
`Collate`/`WeightString` 不自动裁剪，导致跨分片 GROUP BY/DISTINCT 把 `'a'` 与 `'a '`
分成两组（`*_0900_*` 为 NO PAD，行为本就正确）。

修复：`paddedCollator`（裁剪尾随空格后委托上游 collator）与 `byteCollator{pad:true}`
（`utf8mb4_bin`）；`binary` charset 保持字节敏感、不裁剪。测试覆盖
general_ci/bin 合并为一组、0900 保持两组，并用 MySQL 8.0 实测值锚定。

### 字符串比较缺少连接 charset 一致性保证
- status: fixed
- file: pkg/merge/collation.go、pkg/mysql/client/conn.go、pkg/sql/classify.go

原问题：`collatorFor` 按列的 collation ID 取 collator，但结果字节的实际 charset 取决于
会话 charset。MySQL 会把结果值转码到 `character_set_results`；若客户端
`SET NAMES latin1`，utf8mb4 列返回的是 latin1 字节，再用 utf8mb4 collator 比较会得到
错误顺序/分组（ASCII 不受影响，非 ASCII 会错）。

修复（2026-11-01 决策，D5）：两端都固定 utf8mb4——
- 前端：`Class.UnsupportedReason` 对 `SET NAMES <非 utf8mb4>` 返回 1235；
- 后端：`client.Dial` 的 HandshakeResponse41 固定 `Collation=45`
  (`utf8mb4_general_ci`)，不再回显后端默认 collation。

因此归并层可以安全假定结果字节为 utf8mb4。单测：`set_names_latin1/utf8/gbk` 拒绝、
`set_names_utf8mb4[_collate|quoted]` 放行；`TestDial_PinsUTF8mb4Collation` 用 net.Pipe
假服务端验证握手不再回显 33。

## Should Fix

### 上游 collation 代码的 provenance 未锁定 release tag
- status: wontfix
- file: pkg/mysql/collation/doc.go

迁移时本地 checkout 无 tag（commit `6c6c003`，2026-10-30），因此以 commit 作为可复现
锁定，并在 doc.go/NOTICE 中记录。design §13.3 期望 release tag，但当前仓库检查点不具备
该信息；commit 锁定同样可复现，待拿到 tag 后仅需更新文档记录（代码不变），故不在本次
提交范围内。

### D5（metamorphic oracle）未落地，collation 保真只有单测覆盖
- status: fixed
- file: test/metamorphic/collation_test.go

已实现 `TestShardedCollationOracle`（4 分片代理 vs 单库 oracle，`general_ci` /
`0900_ai_ci` / `bin` × ORDER BY / GROUP BY / DISTINCT / COUNT(DISTINCT) / MIN/MAX），
并已按用户要求触发一次。它同时暴露了「列 collation 来源」缺陷（见 Must Fix 的
wontfix 项），保留为失败复现，不在 `make test-integration` 列表中，CI 门禁不受影响。

### 设计文档 API 与实现不一致
- status: fixed
- file: docs/design/statement-classification.md

设计稿写 `Classify(tokens)`，实现为 `Classify(raw, tokens)`（列标签与 SHOW 子类型需要
原文切片），`Token` 增加 `End`，`Class` 增加 `ScanFailed`。文档 §2 已同步。

### 独立检查器对可改写会话函数的语义不一致
- status: fixed
- file: pkg/sql/classify.go

旧 `UnsupportedReason` 会拒绝 `SELECT ROW_COUNT() AS n`，但端到端流程先由
`rewriteSessionFunc` 改写成字面量，因此该拒绝从未生效。新 `Class.UnsupportedReason`
以 `SessionFunc != nil` 为判据，与端到端行为一致；已在设计 §5.2 账本第 7 行记录。

## Nit

### groupState MIN/MAX 每行重复解析 comparator
- status: wontfix
- file: pkg/merge/group.go

`groupState.add` 每行调用一次 `orderComparatorFor`（含 collation 查表）。可把解析结果
缓存到 groupState。当前每行成本为一次 map 查找 + 字符串比较，相对于行解码可忽略；
留待性能剖析后处理。

### collEnv 进程级单例假设所有 slice 同一 MySQL 版本
- status: wontfix
- file: pkg/merge/collation.go

design §13.4 item 4 明确 Phase 1 接受该假设；混合版本策略在 §13.7 开放问题中跟踪。

### EXPLAIN ROUTE 仍做原文扫描
- status: wontfix
- file: pkg/sql/explain_route.go

`ParseExplainRoute` 需要从 `EXPLAIN ROUTE <sql>` 中提取内层 SQL 原文，属语法提取而非
语义判定；内层语句随后仍走 `Classify`。不在 grep 门禁范围内。

### 删除的重复辅助函数
- status: fixed
- file: pkg/sql

`unsupported.go`/`reserved.go`/`session_cmd.go` 的解析部分、`route/source/classify.go`
及重复的 `hasWordPrefix`×2、`stripLeadingComments`×2、`stripSQLStrings`、`splitTopLevel`、
`containsSQLFunc`、`containsSQLIdent`、`skipSQLSpaces`、`parseSQLAlias` 已删除；行为规格
平移到 `classify_test.go`。
