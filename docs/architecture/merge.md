# 归并与结果层（pkg/merge、pkg/result）

把多分片的逻辑行流合并成与单库一致的结果。pull-based 的 operator 树，不 import
`backend` / `execute`——只消费 `result.Stream` 和 `plan.MergeSpec`，调用方控制
租约与背压。

## 包清单

| 包 | 职责 |
|---|---|
| `result` | `Field / Row / Value / Stream` 接口（raw packet 与逻辑行两种形态） |
| `merge` | operator 树：`Concat / Ordered / Sort / Aggregate / Group / Distinct / Filter / Limit / project` |

## 关键类型

- `result.Stream`：`Fields() / Next() (Row, error) / Close()`——`io.EOF` 结束。
- `merge.Merge(streams, fields, spec)`：按 `MergeSpec` 组装 operator 树的单一入口。
- `ordered`：k 路堆（每片一行缓冲）；`Sort`：Group/DISTINCT 后的全量缓冲排序。

## 行为不变量

1. **只有 MultiShard 解码进 merge**（A7）：N=1 永远 raw 直通，归并是分片专属成本。
2. **collation 白名单**（A11）：`utf8mb4_general_ci` / `utf8mb4_0900_ai_ci` /
   `utf8mb4_bin` / `binary`；白名单外 1235。每个放行的 collation 是一份保真承诺，
   须有 oracle 覆盖后才扩。
3. **PAD SPACE**：legacy collation（general_ci / bin）比较前裁剪尾随空格，
   `*_0900_*` 保持 NO PAD——与 MySQL 实测行为对齐。
4. **组键用 collation 权重**：`_ci` 下跨分片的 `'A'` 与 `'a'` 必须归同一组 /
   去重为同一行——裸字节键会裂组。数值/`_bin` 列保留字节键快速路径。
5. **代数正确性**：AVG 由 plan 注入的隐藏 SUM+COUNT 合并；DISTINCT 聚合由隐藏原始列
   + 每片 GROUP BY 带回，归并层按权重去重后再算；`LIMIT m,n` 每片取 `m+n`、
   归并后跳 m 取 n；LIMIT 提前终止后未读尽的连接由 execute 淘汰。
6. **内存有界性**：ordered 是 O(k)；Group/DISTINCT 是 O(基数)——预算上限在设计中
   （`merge.Options`）尚未接线。
7. `pkg/merge` 不 import `backend` / `execute` / `mysql`（仅 `mysql/collation`）。

## 测试入口

`ordered_test / group_test / aggregate_test / distinct_test / collation_test`
（脚本化 stream，表驱动）、`test/metamorphic/`（4 分片 vs 单库 oracle：
聚合矩阵 + collation 矩阵——`general_ci` / `0900_ai_ci` / `bin` ×
ORDER BY / GROUP BY / DISTINCT / COUNT(DISTINCT) / MIN / MAX）。
