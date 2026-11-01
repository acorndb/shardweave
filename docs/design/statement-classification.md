# 语句级判定统一（Single Truth）设计

> 状态：决策稿（2026-11-01 复核收缩：D4 库绑定移出范围，见 §6）。
> 前置：[architecture.md](../architecture.md) §1（设计原则）、§5（依赖规则）。
> 本文只解决一个问题：语句级语义判定的双真相源。实现偏离本文时先改文档。
> 提交提示：`docs/design/` 被 `.gitignore` 忽略，入仓需 `git add -f`。

## 0. 问题

同一句 SQL 的语义判定存在两套并行引擎，盲区不同、结论可能不一致：

| 引擎 | 位置 | 职责 |
|---|---|---|
| 字符串启发式 | `pkg/sql/unsupported.go`、`reserved.go`、`session_cmd.go`、`session_func.go`、`pkg/route/source/classify.go`、`pkg/route/shard/router.go`（`isSetNames`/`isDescribe`） | 拒绝理由、读写分类、粘性检测、USE/SET effect、会话函数识别 |
| AST（TiDB parser） | `pkg/route/shard/router.go`、`pkg/plan/plan.go` | 分片路由、归并规格、改写 |

- sharding 模式下两者同时运行：AST 已在手，`PickRole`/`isSetNames`/`isDescribe` 仍查原始文本。
- single/rwsplit 模式只有字符串引擎（`ParseAST=false`）。
- 字符串引擎内部还有重复且**行为不一致**的辅助函数（两个 `stripLeadingComments`、`hasWordPrefix`×2 等），以及已知的误判（字符串字面量含 `TEMPORARY TABLE`/`*master*` 误判）与漏判（`SET SESSION <var>` 不可见）。

TiDB AST 不保留注释，master hint 等信息只能来自 token 层。因此统一入口必须是
tokenizer；**AST 是 token 层的超集，不是替代**。

## 1. 目标形态

```
                COM_QUERY 到达 (pkg/proxy)
                       │
              ┌────────▼────────┐
              │  Scan() 一次     │  pkg/sql/scanner.go —— 第一方 tokenizer
              │  Classify() 一次 │  pkg/sql/classify.go —— 唯一语句级语义入口
              └────────┬────────┘
                       │  stmt.Class（挂在 sql.Statement 上，只算一次）
     ┌─────────┬───────┴──────┬──────────┬───────────┐
     ▼         ▼              ▼          ▼           ▼
 WantSlave   Reserved      Effect   Unsupported  SessionFunc
 (读写分离)  (粘性标记)     (SET 族)  (拒绝理由)  (LAST_INSERT_ID 等)
     │
     │  sharding 模式额外：AST 已在 stmt.AST
     ▼
  路由/改写只读 AST；hint 等注释信息只读 stmt.Class
```

两条铁律：

1. **AST 是超集**。sharding 模式下路由语义以 AST 为准，`Class` 兜底；两者重叠字段
   （首关键字等）以一致性 property 测试约束（B3）。
2. **只判定一次**。`Class` 在 `proxy/serve.go` 构造 `Statement` 时填充（架构原则 #6），
   下游禁止再对原始文本做语义判定。

## 2. API 骨架

```go
// pkg/sql/scanner.go —— 第一方 tokenizer（预估 ~300 行）
// 只覆盖判定所需词法：空白、--/#//* */注释（hint 保留为 token）、
// 裸/反引号/限定名标识符、单双引号字符串（反斜杠转义，见 §5 假设）、
// 数字、@var/@@var、运算符、分号、括号。
type TokenKind uint8
type Token struct {
    Kind  TokenKind
    Text  string // 原文
    Upper string // 大写形式（关键字/标识符比较用）
    Pos   int
}
func Scan(sql string) ([]Token, error)

// pkg/sql/classify.go —— 单趟消费 token，产出唯一语义快照
// raw 参数用于还原表达式原文（SessionFunc 列标签）与 SHOW 子类型。
type Class struct {
    Kind             StmtKind // SELECT/INSERT/UPDATE/DELETE/REPLACE/DDL/SHOW(带子类型)/SET/USE/事务语句/CALL/WITH/LOAD/OTHER
    Multi            bool     // 顶层分号
    MasterHint       bool     // 仅 /*master*/（D3）
    LockingRead      bool     // FOR UPDATE / FOR SHARE / LOCK IN SHARE MODE
    UserVarWrite     bool     // @x := 或 SET @x =
    SessionVarWrite  []string // SET SESSION/@@session. 赋值的变量名
    TempTable        bool
    LockTables       bool
    Calls            []string // 出现的函数名（GET_LOCK/RELEASE_LOCK/FOUND_ROWS/...）
    SetNames         string
    UseDB            string
    Autocommit       *bool
}
func Classify(tokens []Token) Class
```

（实现修订 2026-11-01：签名为 `Classify(raw string, tokens []Token) Class`——列标签与
SHOW 子类型需要原文切片。`Token` 另带 `End` 字段。`Class` 另带 `ScanFailed bool`，
由 `Statement.EnsureClass` 在 Scan 失败时置位。）

`sql.Statement` 增加 `Class Class` 字段与 `EnsureClass() *Class`：快照只算一次
（`classReady`），Scan 失败时置 `ScanFailed`（D2 退化：非从库 + 粘主）。旧导出函数
（`UnsupportedReason`/`DetectReservedSQL`/`ParseSessionEffect`/`ParseSessionFunc`/`IsDML`/
`ClassifyWantSlave`）已删除，测试表平移为 `Class` 的行为规格。

## 3. 迁移清单（全部在判定链路上）

| # | 现有函数 | 调用点 | 迁移方式 |
|---|---|---|---|
| 1 | `IsDML` | `execute/pipeline.go:438` | 改读 `Class.Kind` |
| 2 | `ParseSessionFunc` | `pipeline.go:402` | 识别层换 token；`Rewrite()` 原样保留 |
| 3 | `ParseSessionEffect` | `proxy/serve.go:92` | `Effect` 结构不变，填充换 token |
| 4 | `ClassifyWantSlave` | `source/pick.go:20`、`router.go` 5 处 | 新 `WantSlave(cls)`；`PickRole` 收 `*sql.Statement` |
| 5 | `UnsupportedReason` | `plan.go:183` | 语义最重（函数调用任意位置），测试表整体平移 |
| 6 | `isSetNames`/`isDescribe` | `router.go:125,135` | 直接用 AST（SetStmt 赋值 / ExplainStmt 判型） |
| 7 | `DetectReservedSQL` | `serve.go:127` | 平价部分换 token；`SessionVarWrite` 的消费进 Track B |
| — | 重复扫描辅助（`hasWordPrefix`×2、`stripLeadingComments`×2、`stripSQLStrings`、`splitTopLevel`、`containsSQLFunc`、`containsSQLIdent`） | — | 全部删除 |

USE 的读写路由（M5：USE → 从库）在平价迁移中**按现状保留**，不在本次改动。

## 4. 两条轨道

```
本文档落盘
    │
    ├─ Track A：平价迁移（核心，零行为变更，除 §5 账本中的误判修复）
    │           A1 scanner → A2 classify → A3 逐消费者切换 → A4 单点判定+删旧+门禁
    │
    └─ Track B：判定精度的收益兑现（依赖 A，可独立延后另立）
                B1 条件 Reset + B2 会话变量白名单 —— 必须同一批落地（原子性见下）
                B3 Class/AST 一致性 property 测试
```

**B 为什么改池却属于本整改**：字符串引擎看不见 `SET SESSION x=1`（`reserved.go` 只抓
`@用户变量`/临时表/锁），池只能用无条件 `COM_RESET_CONNECTION` 兜底防跨前端泄漏，
代价是每查询固定多 3 个 RTT（Reset + 下次 Acquire 的 USE + SET NAMES）。A 产出
`Class.SessionVarWrite` 后这个兜底才可能退役。B 改的是 `pkg/backend`，修的是判定盲区
的经济后果——不是判定修复本身，故标注为第二阶段、可拆。

**B1+B2 落地状态（2026-11-01 更新）**：B1 已改为更简单的规则——**不再无条件 Reset，
「不该放回的连接不放回」**：

| 归还判定 | 动作 |
|---|---|
| 响应未读完（`Drained()==false`） | Discard |
| 执行/对齐/转发出错 | Discard |
| 脏语句（用户变量写、非 autocommit 的 SET SESSION、临时表、锁、多语句、Exec 注释、扫描失败） | Discard |
| 会话仍 sticky（事务/autocommit=0/保留/pin） | Discard |
| 以上都不成立 | 直接归还，**不发 Reset** |

对应实现：`pkg/backend/pool.go`（Release 不再 Reset）、`stream.go`/`decode.go`
（Drained 追踪）、`pkg/execute/pipeline.go`（lease.reusable + dirtyForReuse）。

B2 尚未落地：白名单外的 `SET SESSION` 目前**执行后丢弃连接**而不是拒绝；`time_zone`
尚未进 `Align` 重放。这两项是独立的后续小步。

## 5. 决策记录与行为变更账本

### 5.1 决策记录（2026-11-01 已拍板）

| ID | 问题 | 结论 | 关键影响 |
|---|---|---|---|
| D1 | 未跟踪会话变量（`SET SESSION x=1`）策略 | **白名单对齐 + 其余拒绝**：白名单变量进 `AlignSettings` 跨连接重放；白名单外报 1235（消息点名变量，与 `sql_mode` 现状同风格） | JDBC/ORM 连接初始化兼容性保住；每个白名单变量是一份保真承诺，须进差分语料。首版：database / SET NAMES（仅 utf8mb4）/ autocommit + `time_zone` |
| D2 | `Scan()` 失败降级 | **保守降级**：路由 master + mark reserved，`slog.Warn` 带位置 | scanner bug 表现为路由变保守而非客户端报错；未闭合 `/*` 的两处旧实现不一致随之消解 |
| D3 | master hint 语法 | **只留 `/*master*/`**（注释内容 trim 空白、忽略大小写后等于 `master`；`/* master */` 亦命中）。`/*+ master */` 与 `*master*` 移除 | 升级后依赖这两种形式的客户端**静默失去主库路由**——release note 必须标注 breaking；`*master*` 的字符串字面量误判自然消失 |
| D4 | USE 路由 / 库绑定 | **移出本文范围，另行立项**（原答复「代理绑定固定库、不允许 USE 切换」记录于 §6） | 本次整改中 USE 按现状平价迁移（M5 保留） |
| D5 | 非 utf8mb4 会话 charset（2026-11-01 追加） | **直接拒绝**：`SET NAMES <非 utf8mb4>` 报 1235；后端握手固定 `utf8mb4_general_ci`(45)，不再回显后端默认 collation | 结果字节恒为 utf8mb4，归并层 collator 假定成立；显式 `SET NAMES latin1` 的老客户端会在该语句失败（连接其余部分可用） |

### 5.2 行为变更账本

平价迁移 ≠ 逐字节等价。以下变更全部需要差分/单测**先锚定后切换**（TDD Red→Green）：

| # | 场景 | 现行为 | 新行为 | 轨道 |
|---|---|---|---|---|
| 1 | `SELECT '...TEMPORARY TABLE...'`（字面量含关键字） | 子串命中 → 误判 reserved | 不再 reserved | A |
| 2 | `/*+ master */`、`*master*`（含字面量误判） | 触发主库路由 | 不再识别（D3） | A |
| 3 | 未闭合 `/*` | 两处实现不一致（保留原文 vs 返回空） | Scan 报错 → D2 保守降级，统一 | A |
| 4 | `SET SESSION <白名单外>` | 静默透传，靠无条件 Reset 兜底 | 1235 拒绝（D1） | B |
| 5 | `SET SESSION time_zone=...` | 静默透传、跨连接丢失 | Align 重放（D1 白名单） | B |
| 6 | 词法假设：反斜杠转义恒开启 | 同（散落各处） | 显式声明：`sql_mode` 已被全局拒绝，`NO_BACKSLASH_ESCAPES` 无法经代理开启；假设写进 scanner.go 注释 | A |
| 7 | 独立检查器的 `SELECT ROW_COUNT() AS n` | 拒绝（原文含函数调用） | 不拒绝：`Class.SessionFunc` 已标记为可改写形式，端到端行为不变（旧检查器只是没感知改写） | A |
| 8 | `SET NAMES latin1` / `SET NAMES utf8` / `SET NAMES gbk` | 透传并跟踪 | 1235 拒绝（D5） | A |
| 9 | 后端连接 charset | 回显后端握手 collation（可能是 latin1） | 固定 utf8mb4_general_ci(45)（D5） | A |

### 5.3 实施记录（2026-11-01，Track A）

- A1/A2 已落地：`pkg/sql/scanner.go`、`classify.go`；五个旧测试表整体平移。
- A3/A4 已落地：消费点全部改读 `EnsureClass()`；删除 `unsupported.go`、`reserved.go`、
  `session_cmd.go`、`session_func.go` 的解析部分、`route/source/classify.go` 及对应测试；
  `pkg/route` 的 `isSetNames`/`isDescribe` 改读 `Class`（不再扫原文）。
- **性能保险丝通过**：`BenchmarkScanClassify` 805ns/op vs `BenchmarkLegacyStringScans`
  600ns/op = **1.34x**（在 2x 阈值内）。旧基准忠实复现了「四次独立扫描 + stripSQLStrings
  全量拷贝」；实际代理中新路径只算一次、跨层复用，旧路径每层各扫一次。
- grep 门禁（`pkg/route`/`pkg/plan`/`pkg/execute` 无 `strings.Contains/HasPrefix/ToUpper`）
  与 `go vet` 均干净。
- Track B 未落地（条件 Reset + 会话变量白名单），按 §4 原子性要求整批实施。
  （2026-11-01 更新：B1 已按「脏/坏/未读完即 Discard、干净连接不 Reset」落地；
  B2 白名单拒绝与 time_zone 重放仍待做。）

## 6. 范围外：库绑定（D4，另行立项）

评审中拍板过一个策略：**「代理启动时就只服务于固定的数据库，不允许用户 USE 切换」**。
它与判定双真相源无直接关系，涉及 `pkg/config`、`pkg/mysql/server`（握手期校验）、
`pkg/proxy`/`pkg/execute`（USE 拒绝路径），属产品策略改动，应独立设计与评审
（建议 `docs/design/database-binding.md`，配置形态、握手矩阵、差分锚点届时再定）。
在本次整改中：USE 检测进 `Class.UseDB`（token 层自然产出），消费策略维持现状。

## 7. 测试与门禁

| 层 | 内容 |
|---|---|
| scanner 单测 | 表驱动：每 token 类型正常/边界/截断错误；`scanner_test.go` |
| scanner fuzz | 不 panic、`Pos` 单调、token 拼接无损原文 |
| scanner property | randgen 语料：TiDB parser 接受 ⇒ `Scan` 必须无错（防方言误杀） |
| classify 单测 | 五个旧测试表（classify/unsupported/reserved/session_cmd/session_func）**整体平移**为 `Class` 断言；平移中发现的用例矛盾逐条裁决并记录 |
| 消费者切换 | 每步先跑旧测试再删旧实现，红绿之间不允许改测试期望 |
| 一致性 property | sharding 模式随机语料断言 `Class.Kind` 与 AST 节点类型一致（B3） |
| grep 门禁 | `grep -rn "strings.Contains\|strings.HasPrefix\|strings.ToUpper" pkg/route pkg/plan pkg/execute --include='*.go' \| grep -v _test` 预期为空 |
| 差分 | 账本每行 ≥1 条 CalibrationScript 锚定 |

每步验证：变更包聚焦测试（`go test -race -short -count=1 ./pkg/...` 相关包）+
`go vet`；差分由用户显式触发（AGENTS.md 约定）。功能提交按 AGENTS.md 落
`docs/code-review/review-<主题>.md`。

## 8. 顺序、风险与工作量

```
Track A（6-8 提交）→ Track B（2-3 提交，原子）
```

| 风险 | 缓解 |
|---|---|
| scanner 词法边界 bug（转义、hex、charset 引入符） | fuzz + property + 差分兜底；D2 保证 bug 形态是保守路由 |
| 平移测试表掩盖回归 | 先旧测试后删实现；账本行先锚定 |
| D3 breaking（hint 失效静默改走从库） | release note + README 标注；差分语料正向锚定 `/*master*/` |
| 白名单变量重放不保真 | 每变量一条差分锚定；首版只加 time_zone |

粗估：A ≈ 6-8 提交（scanner、classify 各一，A3 三批，A4 一）；B ≈ 2-3。均为可独立
review 的 PR 粒度。
