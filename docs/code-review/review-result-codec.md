---
title: 结果集编解码与 decoded stream review
files:
  - pkg/mysql/result.go
  - pkg/mysql/decode.go
  - pkg/backend/decode.go
  - pkg/result/result.go
---

## Must Fix

（无）

## Should Fix

### `rawStream.Decode` 假定后端使用 DEPRECATE_EOF
- status: wontfix
- file: pkg/backend/decode.go

后端 client 握手固定协商 `CLIENT_DEPRECATE_EOF`，所以 `Decode` 传 true。若以后
后端握手能力变化，需要把该值从连接握手中带出来，而不是继续硬编码。

### 只支持文本协议结果集
- status: wontfix
- file: pkg/backend/decode.go

`decodedStream` 按文本行解析。多分片 prepared（binary 结果集）在 P0 直接 Reject，
不进入 decode；后续支持时再补 binary 行解码。

### 不支持多结果集
- status: wontfix
- file: pkg/backend/decode.go

decodedStream 解析一个结果集后到 EOF 即结束；多结果集（CALL/多语句）不在 P1
归并范围。多分片 `CALL`/多语句先 Reject。

## Nit

### `Fields()` 返回内部切片
- status: wontfix
- file: pkg/backend/decode.go

调用方只读；与现有 `result.Stream` 契约一致。若后续需要防篡改再返回副本。
