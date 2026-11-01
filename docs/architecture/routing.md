# 路由与计划层（pkg/route、pkg/plan）

纯函数层：语句 → 目标集合（slice、角色、物理库、改写后 SQL）→ 计划。不持有连接池
（A5），mode 不出现在这一层。

## 包清单

| 包 | 职责 |
|---|---|
| `route/source` | single/rwsplit 的选源：`AllMaster`、`Pick`（Layer A 会话粘性覆盖 Layer B 语句分类） |
| `route/shard` | 分片规则路由：`hash_mod` / `hash_mod_str`，分片键谓词提取 |
| `plan` | `Builder.Build`：Targets + `MergeSpec` → 三态 `Plan` |

## 关键类型

- `route.Router` 接口：`Route(ctx, state, stmt) ([]Target, error)`。
- `route.Target`：`Slice / Role / DB / SQL`（物理库在 Target.DB，A8）。
- `plan.Kind`：`Passthrough / SingleShard / MultiShard / Reject`——三态契约的载体。
- `plan.MergeSpec`：纯数据的归并描述（OrderBy / Having / Limit / Aggregates /
  GroupBy / Distinct / Output），由 plan 生成、merge 只消费。

## 行为不变量

1. **可证明单分片，否则 Reject**（A6）：分片键等值 / 同片 `IN` 可证；范围、JOIN、
   子查询、窗口函数、schema 限定名、UPDATE 分片键、非字面量分片键全部明确拒绝，
   不 scatter、不猜。
2. **改写发生在 plan 层**：LIMIT `m,n` → 每片 `LIMIT m+n`；AVG → 隐藏 SUM+COUNT 列；
   DISTINCT 参数 → 隐藏原始列 + 每片 GROUP BY；GROUP BY 的 HAVING 移除、归并后过滤；
   排序/分组缺列 → 隐藏列补齐。
3. **AST 仅在 sharding 模式解析**（A14）：single/rwsplit 保持零 parse 透传。
4. `multiShardRejectReason`：非 autocommit、事务中、pin 中的会话拒绝多分片语句——
   跨片无事务语义。
5. `EXPLAIN ROUTE` 走同一 Build 路径，只合成结果不执行。

## 测试入口

`route/shard/router_test.go` 与 `route/source/*_test.go`（表驱动，纯函数无 Docker）、
`plan_test.go`（三态 + MergeSpec 提取 + 改写断言）、`test/sharding/`（e2e）。
