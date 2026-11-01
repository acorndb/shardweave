# shardweave

[![CI](https://github.com/acorndb/shardweave/actions/workflows/ci.yml/badge.svg)](https://github.com/acorndb/shardweave/actions/workflows/ci.yml) [![codecov](https://codecov.io/gh/acorndb/shardweave/branch/main/graph/badge.svg?token=CODECOV_TOKEN)](https://codecov.io/gh/acorndb/shardweave) [![Go](https://img.shields.io/badge/go-1.27.3-blue.svg)](go.mod) [![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
<!-- 私有仓库：把 ?token= 后的 CODECOV_TOKEN 换成 Codecov 后台生成的 badge token -->

MySQL 分库分表代理。单条数据通路承载三种拓扑（`single` / `rwsplit` / `sharding`），
核心纪律是**三态契约**：每一条 SQL 要么跨分片语义正确地支持、要么可证明落在单分片
（raw 透传，行为与裸 MySQL 一致）、要么带理由明确拒绝——**永不静默返回错误结果**。

## 特性

**协议与连接**

- 第一方 MySQL 线协议实现（HandshakeV10、native password、`COM_QUERY` / `COM_STMT_*`），
  不做黑盒依赖
- 后端连接池：预热、健康检查、idle 回收、MaxLifetime；结果未读完 / 出错 / 携带不可
  复现会话状态的连接直接淘汰，干净连接复用**零额外往返**
- `max_allowed_packet` 逻辑包上限、前后端读写 I/O 超时

**读写分离（rwsplit）**

- token 层语句分类选源：读走从库，写与事务走主库；从库不可用自动回落
- 事务 / prepared / 用户变量自动粘主库
- 会话变量白名单重放（`time_zone`、`transaction_isolation`，含 DEFAULT 重置）；
  白名单外明确返回 1235

**分片路由（sharding）**

- `hash_mod` / `hash_mod_str` 分片算法
- 可证明单分片：分片键等值 / 同片 `IN` 直接路由；证明不了就明确报错，不 scatter、不猜
- `EXPLAIN ROUTE <sql>` 查看路由决策（目标 slice、角色、改写后 SQL）
- 单分片 prepared statement（代理侧语句表，按 slice 映射后端 stmt id）

**跨分片归并（sharding）**

- `ORDER BY` + `LIMIT/OFFSET`（每片 `LIMIT m+n` 下推 + k 路堆归并）
- `GROUP BY` / `HAVING` / `DISTINCT` / `COUNT` / `SUM` / `MIN` / `MAX` / `AVG`
  （AVG 改写为隐藏 SUM+COUNT 列后合并）
- MySQL collation 保真：`utf8mb4_general_ci` / `utf8mb4_0900_ai_ci` / `utf8mb4_bin`
  / `binary`，含 PAD SPACE 语义（尾随空格比较规则与 MySQL 一致）
- 列元数据保真：后端固定 `character_set_results=NULL`，结果集列定义携带**列真实
  collation** 而非连接字符集

**会话语义**

- `USE` / `SET NAMES utf8mb4` / `SET autocommit` 由代理跟踪并在连接上重放
- `SELECT LAST_INSERT_ID()` / `SELECT ROW_COUNT()` 由会话状态应答（改写为 CAST 字面量）
- 会话字符集固定 utf8mb4（非 utf8mb4 的 `SET NAMES` 明确拒绝）

**可测试性**

- 差分校准：CalibrationScript 经真代理 vs 直连 MySQL 全量对比
- 变形 oracle：4 分片代理 vs 单库全量数据，聚合与 collation 矩阵结果一致
- 协议 fuzz（scanner）、单元测试与 CI 全绿门禁

## 架构

```
                ┌────────────────────────────────────────────┐
  MySQL client  │                 shardweave                 │
 ──────────────►│  proxy ─► sql（token 扫描，一次语义快照）    │
                │    │                                       │
                │    ▼                                       │
                │  plan（router → Targets + MergeSpec）        │
                │    │                                       │
                │    ▼                                       │
                │  execute ─► backend pool                   │
                │    │        （Align：库/字符集/会话变量；     │
                │    │          未读尽连接淘汰）                │
                │    ▼                                       │
                │  merge（order / aggregate / group /          │
                │         distinct + collation 权重）          │
                └─────┬─────────────┬─────────────┬──────────┘
                      ▼             ▼             ▼
                   slice s0      slice s1   …   slice sN
                （master / slaves，MySQL 5.7+ / 8.0）
```

模块层级与依赖规则的完整说明见 [docs/architecture/](docs/architecture/README.md)。

## 快速开始

要求：Go 1.27+、Docker（集成测试用，不可用时自动跳过）。

```sh
# 1. 后端 MySQL（native password + utf8mb4）
docker run -d --name mysql-dev \
  -e MYSQL_ROOT_PASSWORD=root -p 3306:3306 \
  mysql:8.0 --default-authentication-plugin=mysql_native_password

# 2. 构建并启动（single 模式示例配置，监听 127.0.0.1:3307）
git clone https://github.com/acorndb/shardweave.git
cd shardweave
go run ./cmd/shardweave -config etc/shardweave.yaml

# 3. 连接（需要支持 mysql_native_password 的客户端）
docker exec -it mysql-dev \
  mysql -hhost.docker.internal -P3307 -uroot -proot --ssl-mode=DISABLED
```

> 注意：MySQL 9.x 客户端移除了 `mysql_native_password` 插件，请使用 8.x 客户端
> （如上用容器内的客户端）。caching_sha2_password 支持在路线图中。

分片模式配置示例（完整字段见 [etc/shardweave.yaml](etc/shardweave.yaml)）：

```yaml
listen: "0.0.0.0:3307"
users:
  - name: root
    password: "root"
backend:
  mode: sharding
  slices:
    - name: s0
      master: { address: "10.0.0.1:3306", user: proxy, password: "***" }
    - name: s1
      master: { address: "10.0.0.2:3306", user: proxy, password: "***" }
  sharding:
    rules:
      - table: orders
        key: user_id
        algorithm: hash_mod
        slices: [s0, s1]
```

## SQL 支持矩阵（sharding 模式）

| 状态 | 含义 | 示例 |
|---|---|---|
| 跨分片归并 | 语义与单库一致 | `ORDER BY`、`LIMIT`、`GROUP BY`+`HAVING`、`DISTINCT`、聚合族 |
| 单分片直通 | 可证明单分片，raw 透传 | `WHERE user_id = ?`、同片 `IN`、单分片 prepared |
| 明确拒绝（1235） | 带理由报错，不猜 | 跨分片写、JOIN、子查询、窗口函数、非 utf8mb4 `SET NAMES` |

写路径一律要求可证明单分片；跨分片事务不在 v1 范围。

## 开发

```sh
make build            # 编译
make test-unit        # 单元测试（-race -short，无 Docker）
make test-integration # 差分 + 握手 + 透传 + 读写分离 + 分片（需要 Docker）
make lint             # go vet + golangci-lint
make fmt              # gofumpt
```

测试体系分三层：单元测试（fake/stub，无外部依赖）；差分校准
（[test/differential/](test/differential/)，经真代理 vs 直连对比，每个真实 bug 沉淀为
CalibrationScript 条目）；变形 oracle（[test/metamorphic/](test/metamorphic/)，
分片代理 vs 单库全量数据结果一致）。

## 路线图

- [x] M1 透传（协议、池、差分 harness）
- [x] M1.5 模块化 seam
- [x] M2 会话与单分片路由（`Reject`、`EXPLAIN ROUTE`）
- [x] M3 跨分片归并（order / aggregate / group / distinct / limit + collation）
- [ ] M4 生产化：metrics、健康检查、限流、故障回落
- [ ] 查询超时与 `KILL QUERY`、每连接能力协商
- [ ] 认证扩展（caching_sha2_password、TLS）
- [ ] plan cache、同片 JOIN、广播表、全局 ID

## 已知局限

- 仅 `mysql_native_password`，无 TLS（路线图中）
- 会话字符集固定 utf8mb4；非 utf8mb4 列的跨分片排序 / 分组明确拒绝
- 跨分片写与事务明确拒绝（写必须可证明单分片）
- 无全局 ID 生成：分片表 `AUTO_INCREMENT` 主键跨片不保证唯一，建议应用侧生成
- 多语句 / 用户变量语句执行后淘汰连接（保守策略）

## License

Apache License 2.0。其中 `pkg/mysql/collation` 源自
[vitess](https://github.com/vitessio/vitess)（Apache 2.0），按 commit 锁定拷贝，
见包内 `NOTICE`。
