---
title: 统一 errs 包 review
files:
  - pkg/errs/errs.go
  - pkg/mysql/result.go
  - pkg/mysql/decode.go
  - pkg/mysql/packet/packet.go
  - pkg/mysql/server/conn.go
  - pkg/plan/plan.go
  - pkg/route/shard/router.go
  - pkg/execute/pipeline.go
  - pkg/execute/dispatch.go
  - pkg/proxy/session.go
  - pkg/backend/pool.go
  - pkg/backend/decode.go
---

本次重构把跨层错误收敛到 `pkg/errs`：一个 MySQL ERR 值类型、常用 errno/SQLSTATE
常量与构造器、三个跨包哨兵错误，以及统一的 `mysql.WriteERR`。验证边界：受影响
包聚焦单测全绿；`go vet`/golangci-lint v2.15.0 干净；2 Slice 真拓扑 e2e
（包含 1235 拒绝、握手、认证路径）通过。未跑全量差分/randgen，按 AGENTS 需
用户显式批准。

## Must Fix

（无）

## Should Fix

### 合成错误与后端解码错误是两套相同结构
- status: fixed
- file: pkg/plan/plan.go, pkg/mysql/decode.go

`plan.RejectError{Code,State,Message}` 与 `mysql.ServerError{Number,SQLState,
Message}` 表达同一件事，但类型不同：`proxy.handleExecError` 只认前者，后端
解码错误需要单独分支；任何新层都可能再发明一套。修复：`errs.MySQL` 作为唯一
值类型，`Plan.Reject`、`DecodeServerError`、proxy、router、execute 与测试全部
直接使用 `*errs.MySQL`，不再保留中间类型/别名；字段统一为
`Number/SQLState/Message`。`errors.As(err, *errs.MySQL)` 现在对合成错误和
解码错误都成立。

### proxy/server 各写一份 writeERR
- status: fixed
- file: pkg/mysql/result.go, pkg/proxy/session.go, pkg/mysql/server/conn.go

两处都把 `EncodeERR` + 4 字节 header 再复制一遍，容易在 packet 上限/sequence
处理上漂移。修复：`mysql.WriteERR(front, *errs.MySQL)` 统一负责 framing；
proxy/server 删除本地 helper。新增 `TestWriteERR`/`TestWriteERR_NilArgs`
覆盖正常、nil front、nil error 分支。

### 哨兵错误定义分散
- status: fixed
- file: pkg/backend/pool.go, pkg/backend/decode.go, pkg/mysql/packet/packet.go

`ErrPoolExhausted`、`ErrNoResultSet`、`ErrPacketTooLarge` 原先各包自行
`errors.New`。定义收口到 `errs`；原位置的 `backend.ErrPoolExhausted`、
`backend.ErrNoResultSet`、`packet.ErrPacketTooLarge`、`execute.ErrPoolExhausted`
别名已全部删除，所有调用点直接使用 `errs.*`，避免同一个错误值存在两个名字。

### 常用 MySQL 错误码/文案散落
- status: fixed
- file: pkg/errs/errs.go, pkg/proxy/session.go, pkg/mysql/server/conn.go

1153/1047/1040/1043/1045/1235/1105 及 SQLSTATE 现在由 `errs` 常量与构造器
提供（`Unsupported`、`UnknownCommand`、`TooManyConnections`、`PacketTooLarge`、
`BadHandshake`、`AccessDenied`、`Unknown`）。proxy 不再硬编码 errno/SQLSTATE；
plan/router/execute 的 1235 统一走 `errs.Unsupported`。

## Nit

### 包内诊断错误仍留在原包
- status: wontfix
- file: pkg/backend, pkg/mysql/packet, pkg/mysql/auth

握手截断、length-encoded 非法、池关闭等错误只在包内产生和判断，跨包没有
`errors.Is` 需求。把它们搬进 `errs` 会形成 god package，也失去就近的上下文。
`errs` 只收口“需要跨层识别或写给客户端”的错误，边界已写入包注释与
`module-map.md`。

### 兼容别名已全部移除
- status: fixed
- file: pkg/backend, pkg/mysql/packet, pkg/plan, pkg/mysql/decode.go

初版为了减少调用点改动，保留了 `backend.ErrPoolExhausted` 等变量 alias 与
`plan.RejectError`/`mysql.ServerError` 类型 alias。alias 本身就是重复命名，
容易让新代码继续使用旧入口。现已全部删除：哨兵直接引用 `errs.Err*`，
`Plan.Reject` 与 `DecodeServerError` 直接产出 `*errs.MySQL`。
