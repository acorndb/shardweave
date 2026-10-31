---
title: 分片键类型严格化（hash_mod / hash_mod_str）review
files:
  - pkg/route/shard/router.go
  - pkg/constant/constant.go
  - pkg/config/config.go
  - test/harness/sharded.go
  - test/sharding/sharding_test.go
---

本次变更把“算法”与“键值域”绑定：`hash_mod` 只接受整数键（含严格十进制
整数字符串），新增 `hash_mod_str` 只接受字符串/字节键。验证边界：变更包
聚焦单测全绿；`go vet`/golangci-lint v2.15.0 干净；2 Slice 真拓扑 e2e
（含新增字符串键表）通过。未跑全量差分/randgen，按 AGENTS 需用户显式批准。

## Must Fix

### 同一逻辑键因字面量加引号而路由到不同分片
- status: fixed
- file: pkg/route/shard/router.go

旧 `shardIndex` 按 Go 字面量类型分派：`k = 1` 走 `uint64 % N`，`k = '1'`
走 FNV-1a。INT 列下 MySQL 认为两者相等，但代理会把它们送到不同 slice。
SELECT 漏行；UPDATE/DELETE 静默影响 0 行；INSERT/查询混用引号时读写分片
不一致。这是“静默错误结果”，不是不支持。

修复：算法决定键值域。`hash_mod` 只接受整数；字符串字面量必须是严格
十进制整数（`'1'`、`'01'`、`'+1'`、`'-1'`，含 uint64 范围），解析后走
整数取模。`hash_mod_str` 只接受字符串/字节，数值字面量 1235。单测覆盖
`1` / `'1'` / `'01'` / `'+1'` / `'-1'` / 混合 `IN`；e2e 增加引号数字键回归。

### 一元负号键值无法路由
- status: fixed
- file: pkg/route/shard/router.go

`literalValue` 只识别 `ast.ValueExpr`，`-1` 解析为 `UnaryOperationExpr`：
SELECT 退化为 scatter，DML 直接 1235。修复：折叠 `+/-` 一元运算到整数字面量；
`-uint64`、decimal 等仍显式拒绝。单测覆盖 `-1` 与 `'-1'` 同片。

### 混合类型 IN 可能跨哈希域路由
- status: fixed
- file: pkg/route/shard/router.go

`IN (1, '2')` 旧实现会同时产生整数取模值与 FNV 值，可能命中错误 slice 或
错误地跨片。现在每个值都经同一算法的规范化：整数规则下两个值都转为整数；
字符串规则下 `1` 直接 1235。`slicesForValues` 与 `pickSlice` 共用
`shardIndex`，SELECT/DML/INSERT 行为一致。

## Should Fix

### 配置与 shard.New 只认识 hash_mod
- status: fixed
- file: pkg/config/config.go, pkg/constant/constant.go

`validateSharding` 与 `shard.New` 增加 `hash_mod_str`；配置单测覆盖合法配置，
非法算法仍拒绝。`shard.New` 同时保留空/重复 slice 名校验。

### 数字字符串的 MySQL 隐式转换未完全覆盖
- status: wontfix
- file: pkg/route/shard/router.go

`hash_mod` 只接受严格十进制整数；`' 1'`、`'1.0'`、`'1e0'` 会 1235，而
MySQL 会把它们当 1 比较。完整模拟需要列类型与 MySQL 转换规则，且这些形式
在分片键上极少见。当前选择显式拒绝，避免非单射转换导致错片；文档已注明。

### hash_mod_str 拒绝数值字面量
- status: wontfix
- file: pkg/route/shard/router.go

VARCHAR 列与数字比较时，MySQL 会把列值转成数字：`WHERE name = 1` 可匹配
`'1'`、`'01'`、`'1.0'` 等，无法靠单个字符串哈希安全收窄。接受 `1` 并转成
`'1'` 会静默漏掉其他等价字符串，因此选择 1235。若后续引入列类型元数据，
可再做严格等价类分析。

## Nit

### 不做 schema 反射，键类型由算法声明
- status: wontfix
- file: pkg/route/shard/router.go

代理不读取 `information_schema` 校验规则 `key` 的真实列类型；误配（字符串列
用 `hash_mod`）会在查询时以 1235 暴露，而不是启动期失败。列类型元数据属于
后续 schema 校验能力；当前 P0 以算法名作为唯一来源，避免启动期连库依赖。

### 字符串键按字节哈希，忽略 collation
- status: wontfix
- file: pkg/route/shard/router.go

`hash_mod_str` 使用 UTF-8 字节的 FNV-1a；大小写不敏感 collation 下
`'Alice'` 与 `'alice'` 在 MySQL 中相等，但代理视为不同键。P0 要求应用使用
二进制/区分大小写的键语义（或统一大小写写入）；collation-aware 分片需要
字符集/排序规则元数据，列入后续。
