---
title: LAST_INSERT_ID / ROW_COUNT 会话函数支持 review
files:
  - pkg/mysql/forward.go
  - pkg/backend/stream.go
  - pkg/execute/forward.go
  - pkg/execute/pipeline.go
  - pkg/sql/session_func.go
  - pkg/sql/statement.go
  - pkg/sql/unsupported.go
  - pkg/session/state.go
---

## Must Fix

（无）

## Should Fix

### 多语句的 ROW_COUNT 依赖首关键字
- status: wontfix
- file: pkg/sql/statement.go

`sql.IsDML` 只看整段 SQL 的第一个关键字。`SELECT 1; INSERT ...` 这类多语句
最终 OK 的 affected_rows 正确，但首关键字是 SELECT，`rowCount` 会被写成 0
而不是 affected_rows。多语句本身在代理里是保守走 master，且客户端很少依赖
多语句后的 ROW_COUNT；需要逐语句解析/逐结果记录才能完全正确，暂不实现。

### 非精确函数形式改为拒绝
- status: wontfix
- file: pkg/sql/unsupported.go

`INSERT ... VALUES (LAST_INSERT_ID())`、`SET @x = LAST_INSERT_ID()`、
`SELECT ROW_COUNT() + 1` 等非精确形式现在返回 1235。这是有意的安全策略：
不做部分改写，避免静默返回错误值；后续如需支持，应基于 AST 改写。

### prepared 形式的 LAST_INSERT_ID/ROW_COUNT 不支持
- status: wontfix
- file: pkg/execute/pipeline.go

`COM_STMT_PREPARE SELECT LAST_INSERT_ID()` 在 prepare 阶段返回 1235。文本协议
可以改写成字面量，二进制协议需要本地合成二进制结果集，工作量和风险都更高；
常见 ORM 从 OK 包取 generated key，不依赖 prepared 形式。

### 改写后的 SELECT 仍需要一次后端往返
- status: wontfix
- file: pkg/execute/pipeline.go

`SELECT LAST_INSERT_ID()` / `ROW_COUNT()` 被改写成 `CAST(...)` 字面量后仍走
现有转发链路，需要一个健康后端连接。Gaea/kingshard 是本地构造结果集，不依赖
后端；当前实现换来的是不新增结果集编码路径，且值本身与后端数据无关。后端全挂
时这条查询也会失败，属于已知取舍。

## Nit

### `AS` 与引号别名之间要求空白
- status: wontfix
- file: pkg/sql/session_func.go

`SELECT LAST_INSERT_ID() AS\`id\`` 这种 `AS` 后直接跟反引号的写法不被识别；
常见客户端都会带空格或省略 `AS`，影响很小。

### affected_rows 转 int64
- status: wontfix
- file: pkg/execute/pipeline.go

`int64(info.AffectedRows)` 在超过 MaxInt64 时会溢出；MySQL 单语句 affected
rows 不可能达到这个量级，保持简单转换。
