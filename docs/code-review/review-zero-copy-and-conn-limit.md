---
title: 零拷贝命令文本与连接槽 CAS 限次 review
files:
  - pkg/proxy/serve.go
  - pkg/route/shard/router.go
  - pkg/server/server.go
  - pkg/session/state.go
  - pkg/constant/constant.go
  - pkg/util/zerocopy/zerocopy.go
---

## Must Fix

无。本次变更的三条核心不变量均有测试兜底：

- `ReadPacket` 每次返回新缓冲、命令循环不改写 payload（`TestServe_StatementTextSurvivesNextRead`）；
- 会话持久字段不别名调用方缓冲（`TestState_ClonesRetainedStrings`）；
- 槽位不超发、不泄漏（`TestServer_AcquireConnConcurrentChurn`）。

## Should Fix

### 注释仍引用已删除的 packetText
- status: fixed
- file: pkg/session/state.go

零拷贝 helper 已迁移到 `pkg/util/zerocopy.HackString`，`SetDatabase` 与对应测试的注释仍写着
`proxy.packetText`，会误导后续维护者。已改为引用新包路径。

### zerocopy 包注释声称双向转换
- status: fixed
- file: pkg/util/zerocopy/zerocopy.go

包注释写的是 “converts between strings and byte slices”，但当前只提供
bytes→string 一个方向。已改为 “provides zero-copy views over byte slices”，
避免读者误以为存在 string→bytes API。

### acquireConn 缺少单自增者与兜底语义说明
- status: fixed
- file: pkg/server/server.go

限次循环移回函数内后，原提取版本的说明丢失：读者无法从代码看出
“生产只有 accept 循环自增”、以及 CAS 预算耗尽后的 `false` 只表示达到上限。
已补函数注释；锁边界与回滚逻辑本身未改。

## Nit

### 兜底预留路径在多自增者假设下会短暂超上限
- status: wontfix
- file: pkg/server/server.go

`Add(1)` 后校验的兜底若被多个 goroutine 同时调用，计数会瞬时超过 `maxConns`
（随即回滚）；成功准入数仍不会超过上限。生产路径只有 accept 循环调用
`acquireConn`，该前提已写入注释。若未来出现第二个自增者，需要改回带锁或
纯 CAS 方案，届时再评估。

### 扫描器 Token.Upper 的每标识符分配未处理
- status: wontfix
- file: pkg/sql/scanner.go

`strings.ToUpper(text)` 对全小写 SQL 的每个标识符分配一次，是比本次移除的
SQL 文本拷贝更大的分配来源。它不属于 `[]byte→string` 范畴，且改动会波及
classify 的全部关键字比较，故本次不处理，留待单独的性能项。

### FNV 手写循环的常量可读性
- status: wontfix
- file: pkg/route/shard/router.go

`offset64/prime64` 以局部常量写在函数内，便于与 `hash/fnv` 对照；
若后续出现第二个哈希使用点，再提升为包级常量。
