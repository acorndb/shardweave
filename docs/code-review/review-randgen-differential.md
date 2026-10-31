---
title: 随机 SQL 差分（randgen）review
files:
  - test/randgen/gen.go
  - test/rwsplit/random_test.go
  - test/rwsplit/rwsplit_test.go
---

## Must Fix

### comparison 使用全局 rand，种子不可复现
- status: fixed
- file: test/randgen/gen.go

初版 `comparison()` 用包级 `rand.Intn`，同一 seed 的两个 `Generator` 会生成不同
比较符，随机差分不可复现。改为 `(*Generator).comparison()` 使用实例的
`g.rnd`，并用 `TestSelectDeterministic` 固定 50 条序列。

### 全表达式投影时 LIMIT 可能选到不确定行集
- status: fixed
- file: test/randgen/gen.go

当投影全部是算术表达式时 `orderable` 为空，原实现不生成 ORDER BY，但仍有 1/3
概率生成 LIMIT；主库与从库可能返回不同子集，造成差分抖动。改为始终在
ORDER BY 末尾追加唯一的 `t.id`，保证 LIMIT 行集确定。

## Should Fix

### 错误比较依赖具体错误包装
- status: fixed
- file: test/rwsplit/random_test.go

初版直接 `reflect.DeepEqual(queryObs)`，会连 `error` 接口的具体实现一起比较；
不同包装类型即使 errno/message 相同也可能判定不等。新增 `queryObs.equal`：
错误只比 errno + message，成功只比列与行。

## Nit

### 行结果排序后比较，不验证 ORDER BY 顺序
- status: wontfix
- file: test/rwsplit/random_test.go

本轮目标是读路径转发一致性。ORDER BY 的排序结果属于查询计划行为，
更适合后续的 plan/differential 层；且排序比较可避免同值并列导致的抖动。
注释已说明。

### 生成器只生成 SELECT
- status: wontfix
- file: test/randgen/gen.go

随机 DML 需要事务回滚/重建数据与复制追平，先以只读差分建立基线；
写路径的随机化在 rwsplit 一致性窗口（read_after_write）落地后再加。
