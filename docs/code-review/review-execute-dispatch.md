---
title: execute 多目标 dispatch seam review
files:
  - pkg/execute/pipeline.go
---

## Must Fix

（无）

## Should Fix

### 多目标 dispatch 仍是占位
- status: wontfix
- file: pkg/execute/pipeline.go

`executePlan` 已按 `[]Target` 分派，但 `dispatch` 对 N>1 返回
`multi-target not implemented`；`plan.Build` 也仍然拒绝多目标。A4/B2 会用真实的
acquire/解码/归并替换。当前不会出现"只执行了部分目标却返回成功"的静默错误。

### `Target.SQL` 只对 COM_QUERY 生效
- status: wontfix
- file: pkg/execute/pipeline.go

prepared 命令没有可直接替换的文本 SQL，改写需要参数/二进制协议处理。P0 单分片
prepared 继续使用原 `Payload`；多分片 prepared 计划在 B1 Reject。

## Nit

### serverErr 时 `runOne` 返回 `(false, nil)`
- status: wontfix
- file: pkg/execute/pipeline.go

ERR 已经转发给客户端，`Handle` 的 defer 统一把 `row_count` 置 -1。语义正确，
但"成功标志"依赖 defer 才完整；A4 多目标汇总时可以显式返回状态枚举。
