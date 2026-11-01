# 语句层（pkg/sql）

把一条命令文本变成唯一的语义快照。所有语句级判定（读写分类、粘性、拒绝、效果）
都消费这份快照，不再各自扫描原文——单真相源（A14）。

## 包清单

| 包 | 职责 |
|---|---|
| `sql` | `Statement` 形态、token scanner、`Class` 语义快照、`Effect`、会话函数改写 |
| `sql/parser` | TiDB parser 的薄包装（AST，仅 sharding 模式使用） |
| `sql/rewrite` | 按目标改写 SELECT（LIMIT 下推、隐藏列、移除 HAVING） |

## 关键类型

- `Statement`：`Cmd / Payload / Raw / StmtID / AST / Class / Effect / Err`。
- `Statement.EnsureClass()`：惰性计算且只算一次的语义快照；`ResetClass()` 供改写后重算。
- `Class`：Kind / Multi / MasterHint / LockingRead / UserVar(Write) / SessionVar(Write) /
  TempTable / Calls / SetNames / UseDB / Autocommit / SessionVars / SessionFunc / ScanFailed。
- `Effect`：语句成功后才应用的会话效果（UseDB / Charset / Autocommit / Vars）。

## 行为不变量

1. **只判定一次**：`Class` 在构造点计算，下游只读；AST 是 token 层的超集而非替代
   （TiDB AST 不保留注释，`/*master*/` hint 只能来自 token 层）。
2. **Scan 失败保守降级**：词法错误 → `ScanFailed` → 永不从库 + 粘主库，不向客户端报错。
3. **改写后必须 `ResetClass`**：`SELECT LAST_INSERT_ID()` 改写为 CAST 字面量后，
   判定必须看到改写后的语句；`COM_STMT_PREPARE` 没有改写路径，会话函数一律拒绝。
4. **白名单边界**：`SET NAMES` 仅 utf8mb4；会话变量仅
   `autocommit`（走 Effect+sticky）/ `time_zone` / `transaction_isolation`（走重放）；
   其余 `SET SESSION` 在 `Class.UnsupportedReason` 处 1235。
5. **grep 门禁**：route/plan/execute 不得出现 `strings.Contains/HasPrefix/ToUpper`
   形式的语义判定（见总览依赖规则）。

## 测试入口

`scanner_test.go`（表驱动 + FuzzScan + 位置单调）、`classify_test.go`
（五个旧判定器的测试表整体平移 + 会话变量/白名单）、`rewrite_test.go`。
