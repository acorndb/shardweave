---
title: 多分片并发 dispatch review
files:
  - pkg/execute/pipeline.go
  - pkg/execute/dispatch.go
  - pkg/execute/dispatch_test.go
---

本次把多分片 dispatch 的 align/exec/列头解码从串行改为按 target 并发：
lease 仍顺序 acquire，避免 session map 并发写；结果按 target 下标收集，
写回/归并顺序不变。验证边界：execute 聚焦单测、完整 `make test-unit`、
2 Slice e2e、single-DB oracle 全绿；sharding e2e 约 69s，oracle 约 26s；
golangci-lint 0 issues。

## Must Fix

### 多分片执行串行，延迟随分片数线性增长
- status: fixed
- file: pkg/execute/dispatch.go

原实现逐个 acquire/align/exec/decode；N 片时首包延迟叠加。修复：先顺序
acquire lease，再为每个 target 启动 goroutine 执行
align + payloadFor + Exec + Decode；`sync.WaitGroup` 后按 target 顺序校验
列元数据并交给 merge。

### lease map 不可并发写
- status: fixed
- file: pkg/execute/pipeline.go

`Runtime.leases` 是普通 map，原设计只在单线程命令循环中使用。并发执行时
若同时 acquire 会 race。修复：acquire 阶段仍串行完成 map 写入；goroutine
只使用已分配的 lease，不再改 map。`go test -race` 覆盖。

### 并发错误路径的流/lease 清理
- status: fixed
- file: pkg/execute/dispatch.go

多个 goroutine 同时返回时，需要关闭已成功的 stream 并丢弃所有 lease，且只
向前端写一次 ERR。修复：等待全部 goroutine，按 target 顺序取第一个错误；
关闭所有成功 stream 后 `cleanup(true)`；`*errs.MySQL` 原样写 ERR，其他错误
返回导致会话关闭。

### 结果顺序保持 target 顺序
- status: fixed
- file: pkg/execute/dispatch.go

并发完成顺序不确定；结果按 `pl.Targets` 下标收集，concat/Ordered/Sort 等
算子看到的 stream 顺序与配置一致，测试仍可期望稳定的 tie 行为。

## Should Fix

### lease acquire 仍串行
- status: wontfix
- file: pkg/execute/dispatch.go

首次查询需要为 N 个 slice 建立连接时，握手仍串行。要并发 acquire 需要给
`Runtime.leases` 加锁或改为 per-target lease 结构；当前先解决执行阶段并发，
后续池/健康探测落地时再评估。

### context 取消不能中断进行中的后端 I/O
- status: wontfix
- file: pkg/backend/conn.go

`Conn.Align/Exec` 目前忽略 ctx，只有 packet 的读/写 deadline；goroutine 会
等到 I/O 返回。查询总超时/取消是独立的 P1 工作。

### goroutine 数等于 target 数
- status: wontfix
- file: pkg/execute/dispatch.go

按片一 goroutine；分片数通常为个位数到几十，暂不需要 worker pool。若未来
支持大批量片，再引入并发上限。

## Nit

### 并发测试用阻塞 hook
- status: wontfix
- file: pkg/execute/dispatch_test.go

测试让两个 fakeConn 的 Exec 互相等待并记录最大并发数；断言 ≥2。比测时间更
稳定，但 hook 仅用于测试，不进入产品路径。

### 元数据校验仍在并发之后
- status: wontfix
- file: pkg/execute/dispatch.go

列头解码并发完成后再统一比较；任一 mismatch 会关闭全部 stream 并 discard。
这保留了“写列头前失败”的语义，代价是多读了一些列头。
