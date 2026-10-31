# 测试基建设计（test-infrastructure）

> 状态：当前测试基建说明（2026-10-31）
> 日期：2026-10-31
> 上位：[architecture.md](../architecture.md)、[module-map.md](module-map.md)
> 范围：测试分层、MTR / RQG 评估、目录与 CI、落地顺序。
> 提交提示：`docs/design/` 当前被 `.gitignore` 忽略；本文件需
> `git add -f docs/design/test-infrastructure.md`。

## 0. 现状与缺口

### 0.1 已有

- `pkg/*` 单元测试：协议编解码、握手、池、路由规则、计划骨架、配置。
- `test/harness` + `test/rwsplit`：真实 MySQL 主从（GTID 复制）+ 进程内代理 +
  `go-sql-driver` 客户端的行为矩阵。
- `test/differential`：`CalibrationScript` 经真代理 vs 直连 MySQL 的透传差分。
- `test/handshake`、`test/passthrough`：握手与真代理冒烟。
- 本次 R1/R2 后的聚焦覆盖率：

| 包 | 覆盖率 |
|---|---|
| `pkg/sql` | 93.4%（R4 session effect 后） |
| `pkg/plan` | 95.0% |
| `pkg/execute` | 89.0%（R6 后） |
| `pkg/route/source` | 83.5% |
| `pkg/proxy` | 73.3%（R4 后；解析测试迁到 `pkg/sql`） |
| `pkg/config` | 94.1%（R5 后） |
| `pkg/mysql` | 69.0% |
| `pkg/mysql/packet` | 75.5% |
| `pkg/backend` | 72.1% |
| `pkg/server` | 77.3% |

### 0.2 缺口

1. **执行路径没有 fake 缝**：`execCommand` 直接断言 `*backend.MySQLConn`，
   单测只能测选源，测不到真正的转发/结果集/错误路径。
2. **代理命令循环覆盖低**：`proxy.Serve` 的 USE/SET/RESET/未知命令分支缺少
   无 MySQL 的驱动测试。
3. **差分只有一组固定脚本**，没有 case 格式、录制/回放、失败最小化。
4. **没有随机 SQL / 语法覆盖**：手写语料增长慢，容易漏掉语法组合。
5. **变形测试仅覆盖无 GROUP BY 聚合**：`test/metamorphic` 已有分片 vs 单库 oracle；ORDER BY/LIMIT、随机数据与 GROUP BY 尚未覆盖。
6. **R1/R2 的验证边界**：做完了静态 diff 复核、`go build`、`go vet`、变更包
   聚焦测试和覆盖率；**差分没有跑**（Docker 当前不可用，且按 AGENTS 需要用户
   明确批准）。所以“行为不变”目前是“按重构方式保证 + 聚焦测试验证”，
   不是“差分已验证”。

## 1. 目标与非目标

### 1.1 目标

- 给“代理是否透明”一个**独立 oracle**：直连 MySQL 的结果/错误/元数据。
- PR 门禁要快（无 Docker 的单元与纯函数测试），重测试放 merge/nightly。
- 失败可复现：记录 SQL、seed、容器日志、包级 trace；失败能最小化成新语料。
- 分片阶段有**变形 oracle**：分片 vs 单库结果一致。
- 语料随 bug 增长：每个真实 bug 都能落成 case。

### 1.2 非目标

- 不追求 100% 覆盖率；不把 MTR 全量套件当 PR 门禁。
- 不做数据库引擎级逻辑 bug 挖掘（SQLancer 的 PQS/NoREC/TLP 不是当前重点）。
- `-short` 单测不依赖 Docker / 真实 MySQL / 外网。
- 不做性能基准（sysbench 单独体系）。

## 2. 测试分层

| 层 | 内容 | 工具 / 形式 | Docker | 门禁 |
|---|---|---|---|---|
| **T0 单元** | 纯函数、编解码、池状态机 | `go test -short`，表驱动 | 否 | PR 必跑 |
| **T1 语料回归** | 路由/计划 case；协议转发 case | YAML/JSON + `test/corpus` | 否 | PR 必跑 |
| **T2 差分** | 直连 MySQL vs 代理 | `test/differential` + 容器 | 是 | merge / 手动 |
| **T3 随机** | 语法/查询组合 | go-randgen / go-sqlsmith / RQG | 是 | nightly |
| **T4 变形** | 分片 vs 单库 | `test/metamorphic` | 是 | P4 前必跑 |
| **T5 Fuzz** | packet/auth/parser 不 panic | Go native fuzz | 否 | nightly / 短跑 |
| **T6 E2E** | 握手、prepare、事务、rwsplit、故障 | testcontainers / 真进程 | 是 | merge / nightly |
| **T7 性能** | 吞吐/延迟/回归 | sysbench / mysqlslap | 是 | nightly（独立） |

原则：

- **T0/T1 是 PR 门禁**，必须快（目标 < 60s）。
- **T2/T6 是行为门禁**，允许慢；失败要能本地一键复现。
- **T3/T5 是发现型**：定时跑，产出语料，不阻塞 PR。
- **T4 是分片准入门槛**：没有它不进 P4 功能。

## 3. MTR 评估

### 3.1 它是什么

- `mysql-test-run.pl`（`mtr`）负责启动/重启 MySQL、选择用例、对比结果；
  `mysqltest` 是执行 `.test` 文件的测试引擎。
- 用例：`mysql-test/t/<name>.test`；期望结果：`mysql-test/r/<name>.result`；
  失败时实际输出写到 `var/log/<name>.reject`。
- `--record` 重新生成 `.result`；`--extern` 可对**已运行实例**跑指定用例
  （必须显式命名用例，不能跑整套）。官方 intro 页没有展开 `--extern`，
  以对应版本的 `mysql-test-run.pl` 参考页/README 为准。

### 3.2 对 shardweave 的适配度

适合：

- **SQL 语义与协议透明度的现成语料**：`mysql-test/t` 有数百个 `.test` 文件、
  语句级语料上万条；跑通一个白名单就是很大的覆盖。
- `--extern` 可以把 `--host/--port` 指到 shardweave，后端仍是真 MySQL。
- `--record` 模式适合把“代理与直连一致”的结果固化成回归语料。

不适合：

- MTR 会重启 server、检查 error log、使用 `--source include/...`、内部表、
  replication/debug sync 等；代理不支持这些生命周期。
- `.result` 与 MySQL 版本/错误信息强绑定；代理一旦改动错误文案就会大面积失败。
- Perl 工具链、测试语言学习成本；CI 镜像要带 MySQL test suite。

### 3.3 建议

**做 MTR-lite，不做 MTR 全量门禁。**

1. 先选 5–10 个不依赖 restart/error log/内部表的 `.test`（基础 SELECT/DML/
   join/type/alias 类），用 `--extern` 指向 shardweave。
2. 固定 MySQL 版本与 `.result`；失败进入“不支持/差异”清单，不阻塞主 CI。
3. 更推荐：把 `.test` 只当 **SQL 语句来源**，导入 `test/corpus/sql`，
   用我们自己的差分 runner 对比直连，而不是依赖静态 `.result`。
4. 如果 MTR harness 太重，直接用 `mysqltest --record` 作为客户端跑 `.test`。

## 4. RQG 评估

### 4.1 它是什么

- MySQL Random Query Generator：按 grammar（`.yy`）随机生成 SQL，按 gendata
  配置（`.zz`）生成表和数据。
- `runall.pl` 启动并管理 server；`gentest.pl` 对已存在 server 跑（可给两个
  `--dsn`）；`gensql.pl` 只生成 SQL。
- 校验器：`ResultsetComparator`（两个 DSN 结果对比）、`SelectStability`、
  `RepeatableRead`、`DatabaseConsistency` 等。

### 4.2 对 shardweave 的适配度

适合：

- **天然差分**：`gentest.pl` 两个 DSN（直连 + 代理）+ `ResultsetComparator`
  就是“同一条随机 SQL 两边结果一致”。
- 语法组合覆盖远超手写语料；适合 passthrough / rwsplit 的读路径。

不适合：

- Perl 工具链；RQG 文档仓库最后更新在 2011 年，工具主要随 MySQL/MariaDB
  源码树分发，版本/维护状态不稳定。
- 随机 DML 的数据准备复杂：两个 DSN 若指向同一后端，写会互相污染；需要两个
  独立 MySQL 实例 + 相同初始数据。
- 结果顺序、并发、非确定性函数需要固定 seed/隔离级别/`ORDER BY`。
- 对协议层（握手、prepare、错误包）覆盖有限。

### 4.3 建议

**RQG 作为 nightly 的“深水区”，不做 PR 门禁。**

- 先做只读 SELECT 语法 + 单线程 + 固定 seed + 独立两个 MySQL 实例；
- `ResultsetComparator` 对比直连 vs 代理；
- 失败保存：SQL、seed、两边结果、容器日志；
- 如果维护成本太高，切换到 Go 生态（见 §5）。

## 5. 其他工具对照

| 工具 | 语言 | 形态 | 对 shardweave 的价值 | 建议 |
|---|---|---|---|---|
| **go-randgen**（PingCAP） | Go | BNF grammar 随机生成 SQL | 易集成 CI；可自带 grammar | **首选**做 T3 |
| **go-sqlsmith**（PingCAP） | Go | sqlsmith 移植，AST 随机查询 | 随机 SELECT/表达式覆盖 | 作为补充 fuzz |
| **SQLancer** | Java | PQS/NoREC/TLP/DQE 等 oracle | 变形测试思路；引擎级 bug | 借鉴 oracle，不直接集成 |
| **RQG** | Perl | grammar + gendata + validators | 成熟差分/随机；维护老 | nightly 可选 |
| **Vitess endtoend** | Go | 真拓扑、真进程、真复制 | E2E harness 设计参考 | 参考目录/进程编排 |
| **ShardingSphere test/it** | Java | parser/route/rewrite/merge/e2e 分层 | 阶段级语料与 case 组织 | 参考分层 |
| **mysql-test corpus** | SQL | `.test`/`.result` | 海量 SQL 语句来源 | 导入语句，不导入 `.result` |
| **sqllogictest** | 多语言 | `.slt` 简单结果对比 | 轻量回归格式 | 可选，不优先 |
| **sysbench** | C | 压测 | 性能回归 | T7 独立 |

结论：

- **T3 首选 Go 工具**（`go-randgen` 或仓内小生成器），CI 友好。
- **RQG 只在 nightly/手动跑**，用来补语法覆盖。
- **MTR 只做白名单试点**，不要作为主 oracle。
- **SQLancer 的 oracle 思想**可在 T4 变形测试里借鉴（例如同一查询的不同写法
  必须同结果），但不引入 Java 工具链。

补充评估（2026-10-31）：`github.com/chaos-mesh/go-sqlsmith` 可种子化、
schema-aware，但会让主模块引入 **368 个模块**（TiDB 依赖树），且 depth≥2
生成的 SQL 出现未定义别名（如 `s1.c5`），因此不进入主模块。随机层采用
`test/randgen` 仓内小生成器：零依赖、SQL 保证可在 MySQL 8.0 执行。

## 6. 推荐目标架构

### 6.1 目录

```
test/
├── corpus/
│   ├── route/              # 纯 route/plan case（无 Docker）
│   ├── sql/                # 代理 vs 直连 case（YAML）
│   └── mysql-test/         # 可选：MTR 白名单 .test
├── harness/                # 真实 MySQL 主从 + 进程内代理
├── rwsplit/                # 真实客户端 rwsplit 行为矩阵
├── differential/           # 现有 runner：对比、录制、失败最小化
├── randgen/                # 种子化随机 SQL 生成 + shrink
├── metamorphic/            # 分片 vs 单库（P4）
├── e2e/                    # testcontainers 集成
└── fuzz/                   # Go fuzz targets
```

### 6.2 case 格式（草案）

```yaml
- id: select_basic_001
  tags: [select, protocol]
  setup: |
    CREATE TABLE t1 (id INT PRIMARY KEY, v VARCHAR(32));
    INSERT INTO t1 VALUES (1, 'a'), (2, 'b');
  sql: |
    SELECT id, v FROM t1 ORDER BY id;
  compare: [result, columns, warnings]
  mode: single   # single | rwsplit | sharding(P3+)
```

runner 输出失败时保存：

- `case_id`、SQL、seed、代理配置、MySQL 版本；
- 两边结果/错误/列元数据 diff；
- 可选 packet trace（代理侧）。

### 6.3 Makefile 目标

| 目标 | 内容 | Docker |
|---|---|---|
| `make test-unit` | `go test -race -short -count=1 ./...` | 否 |
| `make test-corpus` | `test/corpus` 纯函数/协议 case | 否 |
| `make test-differential` | 直连 vs 代理 | 是 |
| `make test-e2e` | 握手/prepare/事务/rwsplit | 是 |
| `make test-randgen` | 种子化随机 SQL | 是 |
| `make test-mtr` | MTR 白名单（可选） | 是 |
| `make test-metamorphic` | 分片 vs 单库 | 是 |
| `make test-fuzz` | 短跑 fuzz | 否 |

CI 分层：

- **PR**：`test-unit` + `test-corpus`（快）。
- **merge**：`test-differential` 子集 + `test-e2e` 冒烟。
- **nightly**：全量差分、randgen、MTR 白名单、fuzz 长跑、MySQL 5.7/8.0 矩阵。

### 6.4 真实拓扑 harness（已落地）

`test/harness` 提供真实环境，不再依赖 fake 角色：

- `StartTopology(ctx, n)`：起 1 master + N replica（`mysql:8.0`），
  开 `log-bin` + `gtid-mode=ON`，用
  `CHANGE REPLICATION SOURCE ... SOURCE_AUTO_POSITION=1` 建真复制；
  追平后对从库 `SET GLOBAL read_only=ON`，误写会以 1290 显式失败。
- `Topology.Sync`：用 `WAIT_FOR_EXECUTED_GTID_SET(master_gtid, 30)` 等所有从库追平，
  不靠 sleep。
- `StartProxy`：进程内启动真实 `server.Server`（rwsplit 装配），监听 loopback。
- `test/rwsplit`：`go-sql-driver` 真实连接，直连 master 作为 oracle，验证：
  从库 read_only、普通 SELECT 落从库、代理写入到主库并复制、事务 pin 主库、
  prepared round-trip、用户变量会话粘滞、MySQL errno 透传。

运行：

```bash
go test -race -count=1 -timeout 10m -v ./test/rwsplit/
```

需要 Docker；`-short` 自动 Skip；CI 的 `make test-integration` 已包含。
后续可对从库 `STOP REPLICA` / `START REPLICA` 制造确定性延迟，验证
从库健康探测、坏从库屏蔽与故障回落，不需要 fake clock。不做 read-after-write 窗口。

分片聚合 metamorphic（2026-10-31）：

- `test/metamorphic/aggregate_test.go`：2 个 master-only slice + 1 个独立
  MySQL oracle；同一份 24 行数据（含 NULL、负数）按 `user_id % 2` 分布，
  oracle 保存全量；
- 对 `COUNT(*) / COUNT(amount) / SUM / MIN / MAX / AVG`、跨片 IN、
  非键过滤、范围过滤、未投影列/表达式 ORDER BY、GROUP BY、DISTINCT 及其
  排序/LIMIT 逐条比较列名与值；数值按 big.Rat 归一化，NULL 语义单独比较；
- 这是聚合归并的独立 oracle，不再只依赖手写期望值。

分片复制拓扑（2026-10-31）：

- `StartShardedTopologyWithReplicas(ctx, slices, replicas)`：每个 slice 独立
  master + GTID 从库，从库 `read_only`；`ShardedTopology.Sync` 等待所有分片
  复制追平；
- 多 target dispatch 并发执行；单测用阻塞 hook 断言至少两个 shard 同时 Exec；
- `test/sharding/replica_test.go`（`TestShardedReplicas`）：2 slice ×
  (1 master + 1 replica)，验证写主读从、scatter/ORDER BY/LIMIT OFFSET/聚合、
  prepared、错误拒绝与会话恢复；
- master-only 的 `StartShardedTopology` 仍保留给快速 e2e。

随机差分（`test/randgen` + `test/rwsplit/random_test.go`）：

- 种子化生成 SELECT：投影、算术、聚合、JOIN、WHERE、ORDER BY、LIMIT；
- 同一 seed 完全可复现；同一 SQL 分别直连 master 与经代理执行，
  比较列、行、errno/message；
- 默认 120 条/轮，`SHARDWEAVE_RANDOM_QUERIES` 调数量，
  `SHARDWEAVE_RANDOM_SEED` 调种子；
- 行结果排序后比较，本轮只验证读路径转发一致性，不验证 ORDER BY 排序本身。

## 7. 落地顺序

| 阶段 | 内容 | 退出条件 |
|---|---|---|
| **H0 测试硬化** | proxy 命令循环用 fake Executor；forwarding 用 `net.Pipe` 或 fake `backend.Conn`；补错误/EOF/多结果集 | executor/proxy 覆盖率 ≥ 70%，无 Docker |
| **H1 差分 case 化** | YAML case 格式、录制/回放、失败保存；导入 50–100 条 MySQL 语句 | 差分可增量跑，失败可复现 |
| **H2 MTR/RQG 试点** | MTR 白名单 5–10 个；RQG 只读 SELECT + ResultsetComparator | 产出一份“差异/不支持”清单 |
| **H3 Go 随机** | `test/randgen`（go-randgen 或仓内 BNF）+ shrink | nightly 跑通，失败进入 corpus |
| **H4 变形** | 分片 vs 单库 harness + 数据生成 | P4 功能准入 |
| **H5 Fuzz/性能** | packet/auth/parser fuzz；sysbench 基线 | nightly 报告 |

## 8. 与重构计划的关系

- **R3/R6 是测试可测性的关键**：`backend.Conn.Exec` + `result.Stream` 已在 R3 落地，
  `execute` 可以用 fake `Conn`/脚本化 Stream 单测，不再依赖真 MySQL（覆盖率 86.2%）。
- R3 之前先做 **H0**：proxy 命令循环用 fake Executor、forwarding 用 `net.Pipe`。
  H0-a 已完成；H0-b 的包级转发行为由 `pkg/mysql/forward_test.go` 与 R3 的 fake Conn 覆盖。
- 每个重构 PR：
  - 聚焦测试必跑；
  - 差分由用户批准后跑（Docker 不可用时明确记录“未跑”）；
  - review 报告里写清验证边界。

## 9. 开放问题

1. MTR 白名单试点是否值得维护？（建议先试 5–10 个，再决定）
2. T3 用 `go-randgen` 还是仓内小生成器？（建议先 `go-randgen`，语法稳定后再自研）
3. case 格式用 YAML 还是 `.test` 风格？（建议 YAML，便于 compare/metadata）
4. 差分是否要求错误消息完全一致？（建议先比 errno/SQLSTATE，消息做 normalize）
5. Docker 矩阵：MySQL 5.7 + 8.0 是否都要进 merge 门禁？（初期 8.0，nightly 5.7）
6. 随机测试的确定性：seed、单线程、固定隔离级别、ORDER BY 规范化。

## 10. 参考

- MySQL Test Framework：
  [Introduction](https://dev.mysql.com/doc/dev/mysql-server/latest/PAGE_INTRODUCTION.html)、
  [mysql-test README](https://github.com/mysql/mysql-server/tree/trunk/mysql-test)
- RQG：
  [RandomQueryGenerator wiki](https://github.com/RQG/RQG-Documentation/wiki/RandomQueryGenerator)、
  [Validators](https://github.com/RQG/RQG-Documentation/wiki/RandomQueryGeneratorValidators)
- go-randgen：<https://github.com/pingcap/go-randgen>
- go-sqlsmith：<https://github.com/PingCAP-QE/go-sqlsmith>
- SQLancer：<https://github.com/sqlancer/sqlancer>
- Vitess endtoend：
  [go/test/endtoend](https://github.com/vitessio/vitess/tree/main/go/test/endtoend)
- ShardingSphere：<https://github.com/apache/shardingsphere>（`test/it`、`test/e2e`）
- 本地调研 checkout：`/tmp/prism-oss-survey/{kingshard,Gaea,dbpack,dbatman,vitess}`
