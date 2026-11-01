# 归并引擎设计（merge-engine-design）

> 状态：设计稿 + P0.5–P0.7 实现状态（2026-10-31）
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
- 完整 MySQL collation 实现（2026-11-01 更新：白名单制子集提前实施，见 §13；
  白名单外仍 Reject，「完整实现」仍是非目标）。
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
- `COUNT/SUM/AVG(DISTINCT expr)`：不能把各片局部结果相加；已落地为 per-shard
  按 DISTINCT 参数 GROUP BY 并带回原始值，归并层按规范化 key 去重后再算；
  字符串/二进制参数、多参数 `COUNT(DISTINCT a, b)` 仍 Reject；
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

（2026-11-01 更新：该硬边界按 §13 白名单制部分放开——引入第一方化 collation 库，
utf8mb4 系三 collation 进入支持范围，其余仍 Reject。三态契约不变。）

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
| P0.5 多分片 concat + limit | `Concat` + `Limit` | 已落地于 `pkg/merge`：逐片拼接后按最终 LIMIT 截断；OFFSET>0 时 plan 把 per-shard SQL 改写成 `LIMIT m+n`。聚合/去重仍 Reject。并发 dispatch 列入 D7 |
| P0.6 多分片 ordered | `Ordered`（k 路堆） | 已落地：每片执行同一 ORDER BY，堆内每片一行；整数/浮点/DECIMAL/日期文本比较，NULL 按 MySQL 语义；未投影列/表达式由 plan 追加隐藏列、`Output` 投影去掉；字符串 collation Reject。LIMIT/OFFSET 已由 plan 改写 |
| P0.7 无 GROUP BY 聚合 | `Aggregate` | 已落地 COUNT/SUM/MIN/MAX/AVG：每片一行折叠；AVG 由 plan 注入隐藏 SUM/COUNT 列，按原 AVG 列 scale 输出；SUM 用 big.Rat/float，MIN/MAX 复用排序比较 |
| P0.8 GROUP BY | `Group` | 已落地数值/日期分组列 + 聚合；map 按 group key 累积，插入顺序输出；字符串分组/collation 未引入，仍 Reject；GROUP BY + ORDER/LIMIT 后置 |
| P0.9 DISTINCT | `Distinct` | 已落地数值/日期行去重；规范化行 key，首次出现顺序输出；字符串/collation 仍 Reject |
| P0.10 分组后排序 | `Sort` | 已落地：GROUP BY/DISTINCT 后在归并层缓冲排序，再应用全局 LIMIT/OFFSET；per-shard LIMIT 移除；字符串排序列仍 Reject |
| P0.11 HAVING | `Filter` | 已落地 AND 数值比较（分组列/聚合别名/匹配聚合表达式）；per-shard HAVING 移除；OR/字符串/未选聚合 Reject |
| P0.12 聚合 DISTINCT | `Aggregate`/`Group` + 去重集合 | 已落地 `COUNT/SUM/AVG(DISTINCT expr)`：per-shard 投影原始参数并加入 GROUP BY，归并层去重后计算；可与普通聚合混用；字符串/多参数仍 Reject |
| P1 多分片只读 v1 | concat、ordered、limit、COUNT/SUM/MIN/MAX/AVG | ORDER BY 仅数值/时间/二进制字符串 |
| P1.5 | GROUP BY、DISTINCT、UNION 去重 | 加内存上限 |
| P2 | bufferSort、spill、GROUP_CONCAT、窗口/跨片 JOIN；字符串 collation 已提前实施（§13） | 单独设计 |

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
   （已决策：按 §13 白名单放开 utf8mb4 系；`_bin`/数值/时间维持现状。）
2. `GROUP BY` 超限：直接报错还是引入 spill？P1 报错。
3. 部分结果 + ERR：客户端兼容性需要真实驱动验证（go-sql-driver/MySQL CLI）。
4. `UNION` 去重与 `DISTINCT` 的 P1/P1.5 边界。
5. DECIMAL 比较用 `math/big.Rat` 还是引入 decimal 库。
6. 是否在 P1 做 `GROUP BY` 分片键优化（组不重叠时用有序归并）。
7. `ORDER BY` 相等键的 tie-break 是否需要确定性（影响 LIMIT 结果）。

## 13. 字符串 Collation（P2 提前实施；方案 B：第一方化 vitess collations）

> 2026-11-01 拍板：采用拷贝第一方化（方案 B），不引入 `vitess.io/vitess` module 依赖。
> 本节部分取代 §6 的「字符串 collation 硬边界」；三态契约不变——白名单外仍 Reject。

### 13.1 决策与理由

| 选项 | 结论 |
|---|---|
| A. module 依赖 `vitess.io/vitess` | 否——运行时直接依赖 +1，闭包含 `sqltypes`（历史上经 querypb 拉 protobuf 运行库）；违背「依赖面极小」原则 |
| **B. 拷贝第一方化到 `pkg/mysql/collation`** | **是**——与协议层「第一方代码」哲学一致；上游测试资产（golden/fuzz）跟包走；依赖面不变 |
| C. 自实现最小子集 | 否——`0900_ai_ci` 需 UCA 权重表，自实现的正确性风险正是本次要买掉的东西 |

选 B 的另一个理由：`Environment` 按后端 server version 区分 collation 行为
（同一 collation 在 5.7/8.0 权重有差异、`0900` 系仅 8.0 存在），自实现很难覆盖。

### 13.2 包形态与依赖规则

- 路径 `pkg/mysql/collation`，镜像上游 `go/mysql/collations`，便于将来 diff 同步。
- **验收条件：仅依赖 stdlib**。上游对 `go/sqltypes`、`go/hack` 的依赖必须在拷贝时消除。
- `pkg/merge` 需要导入它；architecture.md §5 的 merge 依赖行修订为
  `merge → result、plan（仅 MergeSpec）、mysql/collation（stdlib-only 叶子）`。

### 13.3 拷贝范围与适配清单

| 处置 | 内容 |
|---|---|
| 拷贝 | `collation.go`、`env.go`、`local.go`、`coercion.go`、`supported.go`、`cached_size.go`、`mysqlversion.go`、`charset/`、`colldata/`、`internal/` |
| 裁剪 | `remote/`、`vindex/`、`integration/`、`testutil/`、`tools/` |
| 消除 sqltypes | `local.go` 的 `CollationForType(sqltypes.Type)` 删除（本代理从 `result.Field.Charset` 直接拿 collation ID，不经类型推导） |
| 消除 hack | 零拷贝 string↔[]byte 内联为普通转换（只影响 weight key 构造的分配，正确性无关） |
| 测试资产 | 上游 golden/fuzz 测试一并拷贝——正确性门禁跟包走 |
| provenance | 锁定 vitess **release tag**（D1 执行时定；不使用 survey checkout 的随机 commit）；包内 `doc.go` 记录来源 tag 与本地改动清单 |
| license | 保留 Apache 2.0 文件头；NOTICE 记录来源、版本、改动 |

### 13.4 集成设计

1. **比较器**：`valueKind` 扩 `kindCollated`（携带 collator）。`comparatorFor(field)` 读
   `field.Charset` → 查白名单 → 取 collator；字符串比较从 `bytes.Compare`
   换 `collator.Collate()`。
   **⚠ 2026-11-01 实测修正：`field.Charset` 不是列的 collation。** MySQL 的
   ColumnDefinition41 报的是连接 charset（`character_set_results`）。用原始协议探针对同一张
   含 `utf8mb4_bin` 与 `utf8mb4_general_ci` 列的表验证：握手 45/46/255 时，两列分别都报
   45/46/255。由于后端握手固定 45，所有字符串列都会被误判为 `utf8mb4_general_ci`，
   对 `utf8mb4_0900_ai_ci`/`utf8mb4_bin` 列产生**静默错误**的 ORDER BY/GROUP BY/DISTINCT。
   真实列 collation 必须来自 catalog（`information_schema.columns`，按
   schema/table/column 缓存），或由每分片 SQL 追加 `COLLATION(col)` 隐藏列带回。
   在 catalog/探针落地前，collated 归并不能安全启用（见 §13.8）。
2. **白名单（首版）**：`utf8mb4_bin`（字节序等价，零成本）、`utf8mb4_general_ci`、
   `utf8mb4_0900_ai_ci`。`IsSupported && !白名单` → Reject。每个放行的 collation 是
   一份保真承诺，须有 oracle 覆盖后才扩。
3. **连接 charset 固定 utf8mb4（2026-11-01 决策，取代原 charset 门）**：前端
   `SET NAMES` 仅允许 `utf8mb4`（其余 1235 拒绝），后端握手固定
   `utf8mb4_general_ci`(45) 不再回显后端默认 collation。两端都固定后，结果字节恒为
   utf8mb4，collator 假定成立；跨 charset 逆转换留作 Phase 2。
4. **Environment**：进程级单例，按后端 server version 构建。P2 假设全部 slice 同版本；
   混合版本策略见 §13.7。
5. **连接 charset plumbing**：merge 目前只拿 streams/fields/spec；连接 charset（握手固定
   255 + `SET NAMES` 跟踪值）由 `execute.dispatch` 注入。
6. **GROUP BY / DISTINCT 组键（本设计最深的点）**：`group.go` 现为 `map[string]` 裸字节键；
   `_ci` 下分片 0 的 `'A'` 组与分片 1 的 `'a'` 组 collation 相等，必须归同一组，裸字节键会
   裂组。组键改为 collation 等价类键（collator 的 weight string）；`_bin`/数值列保留
   字节键快速路径。DISTINCT accumulator 同理。
7. **MIN/MAX**：`aggregate.go` 的比较替换复用同一 comparator（`kindCollated` 分支）。
8. **PAD SPACE（2026-11-01 修正）**：MySQL 除 UCA 9.0.0（`*_0900_*`，NO PAD）外的
   collation 都是 PAD SPACE——比较时忽略尾随空格。归并层用 `paddedCollator` /
   `byteCollator{pad}` 在比较与 weight key 前裁剪尾随空格；`binary` charset 除外
   （字节敏感，不裁剪）。已用 MySQL 8.0 实测锚定：
   `'a'='a '` 在 general_ci/bin 为 1、0900_ai_ci 为 0；GROUP BY 分别为 1 组/2 组。
9. **范围（2026-11-01 决策）**：不考虑用户切换 collation。归并只支持 utf8mb4 系白名单
   （`utf8mb4_general_ci`、`utf8mb4_0900_ai_ci`、`utf8mb4_bin`）与 `binary` 字节比较；
   非 utf8mb4 会话 charset 在 `SET NAMES` 处拒绝；跨 charset 逆转换不做。非 utf8mb4
   **列**（latin1/gbk 等）即使结果已被转码为 utf8mb4 字节，也仍 Reject——原 collator
   不能作用于转码后的字节。

### 13.5 分阶段

| 阶段 | 内容 | 红测入口 |
|---|---|---|
| D1 | 拷贝 + 适配 + stdlib-only 验收 + Environment 单例 | 包内 golden/fuzz 全绿；`go list -deps` 确认无外部依赖 |
| D2 | `comparatorFor` collation 化 + ORDER BY（ordered/sort） | `ORDER BY varchar general_ci` 从 Reject → 正确排序；`'a'`/`'A'` ci 相等；`'ß'`/`'ss'` 在 general_ci（ß=s）与 0900_ai_ci（ß=ss）行为不同 |
| D3 | GROUP BY/DISTINCT weight key 化 | 跨片 `'A'`/`'a'` 归一组；DISTINCT 跨片去重 |
| D4 | MIN/MAX 字符串 | 同 comparator 向量 |
| D5 | metamorphic oracle 扩展 | DDL 加 VARCHAR（bin/general_ci/0900_ai_ci）× ORDER BY/GROUP BY/DISTINCT/MIN/MAX 矩阵 |

### 13.5.1 实施记录（2026-11-01）

- **D1 已落地**：`pkg/mysql/collation`（54 个 go 文件，7.0 MB，含上游 golden 测试与
  charset testdata）。`go list -deps` 确认**仅依赖 stdlib**；本地改动与来源 commit 记录在
  `doc.go` 与 `NOTICE`。上游 `Hash(*vthash.Hasher)`、`CachedSize`、`CollationForType`
  已移除。
- **D2 已落地**：`pkg/merge/collation.go` 新增 `collatorFor` / `orderComparatorFor`；
  `valueKind` 增加 `kindCollated`；`Ordered`/`Sort` 支持白名单 collation。
  白名单：`utf8mb4_general_ci`、`utf8mb4_0900_ai_ci`、`utf8mb4_bin`、`binary`。
- **D3 已落地**：`encodeValueKey` 对 `kindCollated` 使用 `WeightString`，GROUP BY/DISTINCT
  跨分片按等价类合并；`Group`/`Distinct`/`aggregate DISTINCT` 全部接入。
- **D4 已落地**：MIN/MAX 复用 collator 比较。
- **PAD SPACE 已修正**：`utf8mb4_general_ci`/`utf8mb4_bin` 的 GROUP BY/DISTINCT 现在
  与 MySQL 一致地忽略尾随空格（`paddedCollator`/`byteCollator`）；`*_0900_*` 保持
  NO PAD。该差异已用 MySQL 8.0 实测确认。
- **charset 策略已落地**：`SET NAMES` 仅接受 utf8mb4，后端握手固定 45（见 §13.4 item 3）。
- **未完成**：D5（oracle）与 provenance 的 release tag；见
  `docs/code-review/review-statement-classification-and-collation.md`。

每步：变更包聚焦测试 + `go vet`；行为变更进差分语料；功能提交落 code-review 报告。

### 13.6 测试策略增补

- **oracle 规范化**：MySQL 8.0 GROUP BY 无隐式排序，组键列返回 `'A'` 还是 `'a'` 非确定——
  对比前把字符串组键按 collation 等价类规范化行集合，否则 flaky；
- ORDER BY 相等键 tie-break：沿用 §11 约定（唯一键消除 tie）；
- charset 不一致（latin1 列 + utf8mb4 连接）→ Reject 用例；
- 白名单外 collation（如 utf8mb4_unicode_ci）→ Reject 用例。

### 13.7 开放问题

1. Environment 的 server version：首版固定 8.0（测试矩阵即 8.0）；后端版本探测与
   混合版本策略后置。
2. 跨 charset 转换（Phase 2）：用 Environment 的转换能力还是继续 Reject。
3. weight key 分配开销：`hack` 消除后 string/[]byte 转换是否需要 fast path（基准后定）。
4. **列 collation 来源（阻塞）**：见 §13.8。

### 13.8 D5 oracle 结果与阻塞项（2026-11-01）

已实现并**触发** `test/metamorphic/collation_test.go`（4 分片代理 vs 单库 oracle，
`general_ci`/`0900_ai_ci`/`bin` × ORDER BY/GROUP BY/DISTINCT/COUNT(DISTINCT)/MIN/MAX）。
结果：`0900_ai_ci` 通过；`general_ci` 的 GROUP BY 组数错误（4 vs 3）；`utf8mb4_bin` 的
ORDER BY 顺序错误。

**根因（2026-11-01 修正，探针实测）**：不是「同一结果集内 collation 混用」，而是
**连接池的 `COM_RESET_CONNECTION` 把会话字符集重置回服务端默认**。实测同一条连接：

```
握手 collation=45，首条查询 → 元数据 45
COM_RESET_CONNECTION 之后   → 元数据 255（服务端默认）
```

`pool.Release` 无条件发 `COM_RESET_CONNECTION`，所以每个池化连接只有**第一条查询**是
握手时的 45，之后都是服务端默认（本项目测试拓扑为 255）。这解释了 oracle 中
「ORDER BY 报 45、GROUP BY/聚合报 255」——是连接新旧不同，不是查询形态不同。
在任一连接状态下，MySQL 报的都是**统一的**结果字符集 collation，而非列 collation。

**附带发现的潜在问题**：握手固定 45 只对每条连接的第一条查询有效。若后端
`character_set_server` 不是 utf8mb4，Reset 后结果会被转码成服务端默认字符集——
「只支持 utf8mb4」的假设在首查之后不再成立。修复 collation 或推进 Track B 时需一并处理
（Reset 后重新对齐会话字符集，或改为条件 Reset）。

（2026-11-01 后续：无条件 `COM_RESET_CONNECTION` 已移除，改为「脏/坏/未读完即 Discard」，
见 `docs/code-review/review-pool-lifecycle.md`。干净连接不再被重置，握手字符集 45 在复用
连接上得以保留；但「元数据是结果字符集而非列 collation」的结论不变。）

**结论（2026-11-01 用户决策）：暂不修复，保持 collated 归并启用**，接受下表风险；
oracle 测试保留为失败复现（不在 `make test-integration` 列表中，CI 门禁不受影响）。
风险矩阵：0900 表仅 ORDER BY 可能错（尾随空格/部分权重），其 GROUP BY/DISTINCT 正确；
`general_ci`/`bin` 表则 ORDER BY 或分组侧会错，详见 review 报告。

后续修复时两个候选方案：

| 方案 | 做法 | 成本 | 风险 |
|---|---|---|---|
| A. Catalog（不推荐） | `information_schema.columns` 按 (schema, table, column) 加载并缓存（TTL + 未命中重载）；dispatch 在 merge 前用 `Field.OrgTable/OrgName` 回填真实 collation | ~1 天 | **DDL 可绕过代理**，缓存必然有陈旧窗口，需轮询；表达式列无法解析 |
| B. 隐藏 `COLLATION()` 列 | 每分片 SQL 追加 `COLLATION(col)` 隐藏列，取首行解析；空结果无需比较 | ~1–2 天，改 plan/rewrite/merge | 每行多一列；表达式列需 `COLLATION(expr)` |
| C. `character_set_results=NULL` | 后端拨号后 `SET character_set_results=NULL`；元数据直接返回真实列 collation，**无需改 SQL** | 最小（一个会话变量） | 非 utf8mb4 列不再转码，单分片透传返回列原生字节；需配套拒绝非 utf8mb4 列 |

**实测依据（原始协议探针，表含 bin/general_ci/0900 三列）**：

| 连接设置 | `SELECT b,g,n` | `... ORDER BY b` | `... GROUP BY b` | `MIN(b)` |
|---|---|---|---|---|
| 握手 collation=45（当前） | 45,45,45 | 45,45,45 | 45,63 | 45 |
| 握手 collation=0（不设置） | 255,255,255 | 255,255,255 | 255,63 | 255 |
| 45 + `SET character_set_results=NULL` | **46,45,255** | **46,45,255** | **46,63** | **46** |

结论：不设置 collation 拿到的是服务端默认值（255），同样不准；只有
`character_set_results=NULL` 时元数据才是真实列 collation，且对四种查询形态都成立。

在 A/B 落地前，`orderComparatorFor` 对字符串列应回到 Reject（三态契约优先于功能），
oracle 测试保留为失败复现（Red）。
