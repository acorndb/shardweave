---
title: B1.0 单分片路由与 sharding 装配 review
files:
  - pkg/route/shard/router.go
  - pkg/plan/plan.go
  - pkg/config/config.go
  - pkg/server/server.go
  - pkg/proxy/session.go
---

## Must Fix

（无）

## Should Fix

### DDL / 事务 / prepared / EXPLAIN ROUTE 尚未实现
- status: wontfix
- file: pkg/route/shard/router.go

B1.0 只做文本协议的单分片 CRUD；DDL 广播、事务控制、prepared、`EXPLAIN ROUTE`
统一返回 1235，不会静默执行部分目标。B1.1 按文档顺序补齐；在此之前 sharding
模式只适合点查/点写验证。

### 无 FROM 的 SELECT 固定走默认片
- status: wontfix
- file: pkg/route/shard/router.go

常量 SELECT、`LAST_INSERT_ID()`/`ROW_COUNT()` 改写后的 SELECT 没有表，Router
用第一条规则的第一个 Slice 作为默认片。值本身是字面量，因此结果正确；但
`SELECT @@server_id`/`SELECT DATABASE()` 会暴露该片的值，与单库 oracle 可能不同。
P0 文档已标注只保证常量/会话函数。

### TxSingleShard 只是计划标记
- status: wontfix
- file: pkg/plan/plan.go

B1.0 的 Router 直接拒绝事务，`execute` 不会收到跨片事务；`TxSingleShard` 作为
B1.1 的契约先落位。不要在 B1.1 之前把它当成已强制执行的策略。

### hash_mod 的字符串哈希是 FNV-1a
- status: wontfix
- file: pkg/route/shard/router.go

数值键用取模，字符串/字节键用 FNV-1a 64 后取模。算法必须在数据分布与代理之间
保持一致；P0 文档已写明，换算法属于 breaking 变更。其他代理（kingshard 用
crc32）不要求兼容。

### schema 限定表被拒绝
- status: wontfix
- file: pkg/route/shard/router.go

`app.orders` 无法确定逻辑库/物理库映射，B1.0 直接 1235；使用当前会话库的
非限定表名。

## Nit

### 配置与 shard.New 双重校验
- status: wontfix
- file: pkg/route/shard/router.go

`config.Validate` 与 `shard.New` 都校验规则。Router 可能在非 YAML 装配路径
（测试/未来动态规则）被使用，保留防御式校验。
