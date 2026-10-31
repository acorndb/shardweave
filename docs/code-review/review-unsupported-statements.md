---
title: 显式拒绝不支持语句 review
files:
  - pkg/sql/unsupported.go
  - pkg/plan/plan.go
  - pkg/proxy/session.go
---

## Must Fix

### 拒绝语句后会话被关闭
- status: fixed
- file: pkg/proxy/session.go

`handleExecError` 对 Reject 写 ERR 后返回 nil，但四个调用点仍然 `return
handleExecError(...)`，于是 `Serve` 退出、连接关闭，客户端无法继续发语句。
改为调用点检查 `herr != nil`：Reject 时 `continue`，其他错误保持原行为退出。
新增 `TestServe_RejectErrorWrites1235AndContinues` 验证 ERR 后 `COM_PING` 仍可用。

## Should Fix

### 只拒绝了 SET sql_mode，其他会话变量仍静默
- status: wontfix
- file: pkg/sql/unsupported.go

`SET time_zone` / `transaction_isolation` 等仍未跟踪也未拒绝，会被路由到后端后
丢失。产品尚未决策，已在 `rwsplit-compatibility-gap.md` §3.10 记录；R1 统一按
ConnectionState pin 策略处理，届时扩展 `unsupported.go` 或替换为 StatementClass。

## Nit

### 文本启发式而非 AST
- status: wontfix
- file: pkg/sql/unsupported.go

检测基于 COM_QUERY 文本（剥离前导注释），覆盖 SHOW 前缀与 SET 赋值列表，
不解析 AST。对当前明确的拒绝清单足够；`StatementClass` 落地后替换为 AST 分类。

### 错误文案固定英文
- status: wontfix
- file: pkg/sql/unsupported.go

客户端错误文案保持英文与 MySQL 风格一致，便于驱动/用户识别；不做本地化。
