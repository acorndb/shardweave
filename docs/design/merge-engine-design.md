# 归并引擎设计（merge-engine-design）

> 状态：设计稿（2026-10-31）
> 范围：分片多目标结果的流式归并；单分片仍走 raw 快路径。
> 关联：[sharding-p0-design.md](sharding-p0-design.md)、[module-map.md](module-map.md)
> 提交提示：`docs/design/` 被 `.gitignore` 忽略；入仓需 `git add -f`。

## 0. 结论

1. **归并做成 pull-based 的 Stream operator 树**，不是"一次性把 N 份结果拼成二维数组再处理"。
   ```text
   Project(Output)
     └─ Limit(offset,count)
         └─ OrderedMerge(keys)
             ├─ shard0 stream
             └─ shard1 stream
   ```
2. **能流式的**：
   - `UNION ALL` / 无排序无 limit 的拼接；
   - 每个分片已按同一 `ORDER BY` 排序时的 k 路堆归并；
   - `ORDER BY ... LIMIT`（堆 + 计数器）；
   - 无 `GROUP BY` 的 `COUNT/SUM/MIN/MAX/AVG`（流式折叠）。
3. **半流式（状态随基数增长）**：`GROUP BY`、`DISTINCT`、`UNION` 去重。
   必须有内存上限；超限报错，不能静默 OOM。
4. **不能流式**：跨片 `JOIN`、窗口函数、未下推 `ORDER BY` 的排序。
   P1 直接 Reject；需要时再引入 buffered sort / spill。
5. **流式归并的前提是改写**：`ORDER BY`、`GROUP BY`、聚合、`LIMIT` 必须先下推到各分片，
   并把排序/分组所需的隐藏列带回来；否则归并层只能全缓冲。
6. **比较语义是最大风险**：字符串排序依赖 MySQL collation；P1 先限制为
   数值/时间/二进制字符串，普通字符串 `ORDER BY` 暂 Reject，避免"看起来支持、结果不对"。

## 1. 目标与非目标

### 1.1 目标

- 多分片 SELECT 的结果按行流式产出，内存与分片数、limit 相关，而不是总行数相关。
- 归并 operator 可组合、可单测，不依赖真实 MySQL。
- 单分片/无排序场景零额外内存；`LIMIT` 能提前关闭上游。
- 归并语义可证明：NULL 排序、空结果、重复行、类型比较都有表驱动用例。
- 与现有 `result.Stream` 接口兼容，`pkg/merge` 不 import `backend`/`mysql`。

### 1.2 非目标（P1）

- 跨片 JOIN / 窗口函数 / 子查询重写。
- 完整 MySQL collation（utf8mb4_general_ci/unicode_ci 等）实现。
- 磁盘 spill / 外部排序。
- `GROUP_CONCAT` 跨片有序合并。
- 多分片 binary prepared 结果集归并（多分片 prepared 先 Reject）。

## 2. 流模型

### 2.1 接口

```go
// pkg/result
type Stream interface {
    Fields() []Field
    Next() (Row, error) // io.EOF 结束
    Close() error
}
```

- **pull-based**：`Next` 每次只向上游要一行；归并层不预读整表。
- **单行缓冲**：k 路堆只保存每个 stream 的当前行 + 排序键。
- **Close 级联**：任一 operator 关闭时关闭全部上游；`LIMIT` 达到上限立即关闭上游。

### 2.2 Operator 树

`merge.Merge` 根据 `plan.MergeSpec` 构造：

| Spec | Operator 链 | 内存 |
|---|---|---|
| 单 stream | `identity` | O(1) |
| `UNION ALL`，无 order/limit | `concat` | O(1) |
| 有 `ORDER BY`，已下推 | `ordered`（k 路堆） | O(k) 行 |
| `ORDER BY + LIMIT` | `limit(ordered(...))` | O(k) 行 |
| 无 GROUP BY 聚合 | `aggregate`（折叠） | O(1) |
| `GROUP BY` | `group`（hash） | O(groups) |
| `DISTINCT` | `distinct`（hash） | O(distinct) |
| `UNION`（去重） | `distinct(concat(...))` | O(distinct) |
| 无下推排序 | `bufferSort`（P1 可选/Reject） | O(rows) |

所有 operator 都实现 `result.Stream`；`Fields()` 在构造时确定，不依赖消费完数据。

### 2.3 单分片快路径

- `len(targets)==1` 且 `Plan.Kind==SingleShard`：**不进入 merge**，继续 raw packet 直通。
- `len(streams)==1` 但走了多分片计划：`identity` operator，仍可流式。
- 只有 `MultiShard` 才需要 decoded stream。

## 3. MergeSpec

`plan` 只放"纯描述"，不 import `result`：

```go
type MergeKind uint8

const (
    MergeIdentity MergeKind = iota
    MergeConcat
    MergeOrdered
    MergeAggregate
    MergeGroup
    MergeDistinct
)

type OrderKey struct {
    Column int  // 结果列下标（含隐藏列）
    Desc   bool
}

type Limit struct {
    Offset int64
    Count  int64 // -1 = no limit
}

type AggregateFunc uint8

const (
    AggCount AggregateFunc = iota
    AggSum
    AggMin
    AggMax
    AggAvg
)

type AggregateSpec struct {
    Func    AggregateFunc
    Column  int // 输入列下标
    Output  int // 输出列下标
    Distinct bool
}

type MergeSpec struct {
    Kind       MergeKind
    OrderBy    []OrderKey
    Limit      *Limit
    Aggregates []AggregateSpec
    GroupBy    []int
    Distinct   bool
    Output     []int // 最终投影（去掉隐藏列）；nil = 全列
}
```

- 列下标基于"改写后各分片返回的列顺序"，不是原始 SQL 的列序。
- `Output` 用于去掉为排序/分组补的隐藏列。
- `MergeSpec` 由 `plan.Builder`/`rewrite` 生成，`merge` 只消费。

## 4. 各形态的流式策略

### 4.1 拼接（UNION ALL / 无排序查询）

```text
concat: 依次/轮转读取各 stream，边读边产出
```

- 不需要排序，不需要缓存全部行；
- 轮转可以降低首行延迟；顺序对无 `ORDER BY` 的 SQL 本身无定义；
- 有 `LIMIT` 时读满即停，关闭上游。

### 4.2 有序归并（ORDER BY 已下推）

前提：每个分片返回的结果已按同一组 `OrderKey` 排序，且排序列在结果中（隐藏列）。

```text
初始化：每个 stream 读 1 行，push (row, streamIdx)
循环：pop 最小行 → 产出 → 从该 stream 读下一行 → push
```

- 内存 O(k)；
- 比较器：
  - NULL：ASC 在前，DESC 在后；
  - 数值：按列类型比较（int64/uint64/float/decimal）；
  - 时间：`time.Time`；
  - 字符串：P1 仅支持二进制/大小写敏感排序；其他 collation 先 Reject；
- 相等键：用 stream index 做稳定 tie-break（MySQL 对相等键顺序本身无定义）。
- 有 `LIMIT`：跳过 offset 行、产出 count 行后立即关闭全部上游。

### 4.3 无 GROUP BY 聚合

```text
aggregate: 逐行更新累加器；EOF 时产出一行
```

- `COUNT(*)` / `COUNT(col)`：计数；
- `SUM`：按类型累加；
- `MIN/MAX`：比较替换；
- `AVG`：必须改写为 `SUM + COUNT`，归并后相除；直接平均各分片 AVG 是错的；
- `COUNT(DISTINCT)`：不能把各片 distinct count 相加；P1 Reject，或改写为 group-by-distinct 再统计；
- 空输入：`COUNT` 返回 0，`SUM/MIN/MAX/AVG` 返回 NULL（按 MySQL 语义）。

### 4.4 GROUP BY

```text
group: map[groupKey]accumulator；读到 EOF 后输出所有组
```

- 每个分片先做 `GROUP BY`，减少传输；
- 归并层按组 key 合并累加器；组 key 必须包含在结果列中（隐藏列）；
- 内存 O(组数)，需要上限；
- 若 `GROUP BY` 列就是分片键，组在各片天然不重叠，可用有序归并而不是 hash（优化项）；
- `HAVING` 在组输出时过滤；
- `ORDER BY` 组结果：
  - 若各分片已按 `ORDER BY` 输出组，k 路堆可流式；
  - 否则需要缓存所有组再排序（半流式）。

### 4.5 DISTINCT / UNION 去重

```text
distinct: hash set；首次出现的行立即产出
```

- 行 key 用规范化后的值（NULL 视为相等）；
- 内存 O(distinct)；
- 有 `LIMIT` 时读满即停。

### 4.6 不能流式的

- 跨片 JOIN：需要 build side；P1 Reject；
- 窗口函数：需要分区/排序状态；P1 Reject；
- 无下推 `ORDER BY`：必须缓存全部行再排序；P1 默认 Reject，后续可加 `bufferSort` + 内存上限；
- `GROUP_CONCAT` 跨片：分片内顺序无法在归并层恢复；P1 Reject。

## 5. 改写前提（没有 rewrite 就没有流式归并）

| 改写 | 目的 |
|---|---|
| `ORDER BY` 下推到每个分片 | 使 k 路堆成立 |
| 排序/分组列补进 select list（隐藏列） | 归并层有比较键 |
| `Output` 投影去隐藏列 | 客户端看到的列不变 |
| `LIMIT offset,count` → 每片 `LIMIT offset+count` | 减少传输；归并后再截断 |
| `AVG(x)` → `SUM(x), COUNT(x)` | 可合并 |
| `COUNT(*)` → 每片 `COUNT(*)` | 归并求和 |
| `GROUP BY` 下推 | 减少行数 |
| `DISTINCT` 下推 | 减少行数（跨片仍需去重） |
| `ORDER BY` 表达式 → 列别名/隐藏列 | 比较器只处理列 |

隐藏列的名字要唯一（如 `__sw_ord_0`），并在 `Project` 中移除；列元数据以原始 select list 为准。

## 6. 比较与类型

`result.Value` 由 decoder 产出，必须带类型：

| 类型 | 比较方式 |
|---|---|
| 整数 | int64/uint64，注意无符号 |
| 浮点 | float64；NaN 按 MySQL 语义处理 |
| DECIMAL | `math/big.Rat` 或 decimal 库，避免 float 误差 |
| 字符串 | 二进制比较（P1）；普通 collation 暂 Reject |
| 日期/时间 | `time.Time` |
| NULL | ASC 在前，DESC 在后 |
| 枚举/集合 | 按底层字符串/整数 |

**字符串 collation 是 P1 的硬边界**：

- MySQL 的 `utf8mb4_general_ci` / `unicode_ci` 排序不是字节序；
- 代理没有 collation 库就无法保证与单库一致；
- P1 只允许 `ORDER BY` 数值/时间/`BINARY`/`_bin` 字符串；
- 普通字符串排序要么 Reject（推荐），要么明确文档化"按二进制排序"（不推荐）。

## 7. 内存、背压与提前终止

- **有序归并**：O(k) 行；每 stream 只预读 1 行。
- **聚合**：O(1)；**GROUP BY/DISTINCT**：O(基数)。
- `MergeSpec`/`merge.Options` 带预算：
  ```go
  type Options struct {
      MaxGroups int
      MaxRows   int
      MaxBytes  int64
  }
  ```
- 超限返回稳定错误（如 `ErrMergeMemoryExceeded`），由 `execute` 映射为 MySQL 错误；
- 不做静默 spill（P1）；后续可加外部排序/临时文件。
- `LIMIT` 达到后立即 `Close` 全部上游，取消未完成的分片请求（context cancel）。
- 上游错误：`Next` 返回错误 → 归并层取消其它 stream 并返回错误。

## 8. 写回前端

- `result.Writer` 接口：
  ```go
  type Writer interface {
      WriteFields([]Field) error
      WriteRow(Row) error
      Close() error
  }
  ```
- `pkg/mysql` 实现协议编码：ColumnDefinition41、文本行、EOF/OK；
- **必须使用前端协商的 `deprecateEOF`**，不能硬编码 true；
- 先写字段定义，再逐行写；不缓存全部行；
- 列元数据来自 `Project.Fields()`（已去掉隐藏列）；
- 若归并中途某个分片报错，已发出的行无法撤回；按 MySQL 协议发送 ERR 终止结果集，
  客户端应能看到错误。P1 测试要覆盖"部分行 + ERR"。

## 9. 与执行层的关系

```text
execute.dispatch(targets) → []result.Stream
  → merge.Merge(streams, spec) → result.Stream
  → result.Writer 逐行写前端
```

- `execute` 负责 acquire/align/Exec/Close、并发与取消；
- `merge` 只消费 stream，不知道 slice/连接；
- 单分片 raw 路径不经过 `merge`/`Writer`；
- 多分片失败时 `execute` 关闭所有 stream 和 lease；
- 会话状态（affected rows 等）由 `execute` 汇总，`merge` 不碰 session。

## 10. 分阶段

| 阶段 | 归并能力 | 说明 |
|---|---|---|
| P0 单分片 | identity（不进入 merge） | raw 直通 |
| P0.5 多分片 concat + limit | `Concat` + `Limit` | 已落地于 `pkg/merge`：逐片拼接后按最终 LIMIT 截断（OFFSET=0）；ORDER BY/聚合/去重仍 Reject。并发 dispatch 列入 D7 |
| P1 多分片只读 v1 | concat、ordered、limit、COUNT/SUM/MIN/MAX/AVG | ORDER BY 仅数值/时间/二进制字符串 |
| P1.5 | GROUP BY、DISTINCT、UNION 去重 | 加内存上限 |
| P2 | 字符串 collation、bufferSort、spill、GROUP_CONCAT、窗口/跨片 JOIN | 单独设计 |

## 11. 测试策略

- **operator 单测**：脚本化 `sliceStream`，逐 operator 表驱动；
- **流式证明**：自定义 stream 记录 `Next` 调用次数：
  - ordered merge 首次产出前，每个 stream 最多读 1 行；
  - limit 达到后不再读上游；
- **边界**：空 stream、NULL、重复键、相等键、类型边界、unsigned、decimal；
- **错误**：某 stream 中途错误、context cancel、Close 幂等；
- **内存**：GROUP BY/DISTINCT 超预算返回稳定错误；
- **metamorphic**：多分片代理 vs 单库全量数据，结果一致（ORDER BY 用唯一键避免 tie 不确定）；
- **差分**：每个分片结果与直连一致；归并结果与单库一致。

## 12. 开放问题

1. 字符串 `ORDER BY`：P1 是否只允许 `_bin`/数值/时间？推荐是。
2. `GROUP BY` 超限：直接报错还是引入 spill？P1 报错。
3. 部分结果 + ERR：客户端兼容性需要真实驱动验证（go-sql-driver/MySQL CLI）。
4. `UNION` 去重与 `DISTINCT` 的 P1/P1.5 边界。
5. DECIMAL 比较用 `math/big.Rat` 还是引入 decimal 库。
6. 是否在 P1 做 `GROUP BY` 分片键优化（组不重叠时用有序归并）。
7. `ORDER BY` 相等键的 tie-break 是否需要确定性（影响 LIMIT 结果）。
