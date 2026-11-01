# 固定 Docker 环境

给手工验证、演示和后续 SQL 语料用的固定拓扑。自动测试仍走 `test/harness`
（testcontainers + 进程内代理，动态端口）；这里是把同一套拓扑固化成
`docker compose`，端口、账号、初始化脚本都固定下来。

## 前置

- Docker + Compose v2（本机验证：Docker 29.8 / Compose v5.5）
- Go 1.27（`make e2e-up` 先交叉编译 `bin/shardweave`；代理镜像基于 `scratch`，
  不需要拉 golang 基础镜像）
- `mysql:8.0` 镜像可用（代理只支持 `mysql_native_password`，不要用 9.x）

## 命令

```sh
make e2e-up MODE=single      # 起单库环境
make e2e-up MODE=rwsplit     # 主从（真实 GTID 复制）
make e2e-up MODE=sharding    # 4 片

make e2e-logs MODE=single    # 跟随代理日志
make e2e-down MODE=single    # 停止并删除（-v）
```

代理固定监听宿主机 `127.0.0.1:3307`。

## 拓扑与端口

| MODE | 服务 | 宿主端口 | 账号 |
|---|---|---|---|
| single | `mysql` | 3308 | root/root，app/app-pass-123 |
| rwsplit | `mysql-master` | 3308 | 同上 |
| rwsplit | `mysql-replica` | 3309 | root/root（app 由复制同步） |
| sharding | `mysql-s0` | 3308 | root/root，app/app-pass-123 |
| sharding | `mysql-s1` | 3309 | 同上 |
| sharding | `mysql-s2` | 3312 | 同上 |
| sharding | `mysql-s3` | 3313 | 同上 |
| 全部 | `shardweave` | 3307 | app/app-pass-123，库 `bench` |

初始化数据：

- `init/schema.sql`：所有拓扑共用的表，含 `sql_type_test`（覆盖整数/无符号/浮点/
  DECIMAL/BIT/CHAR/VARCHAR/TEXT/BLOB/BINARY/DATE/TIME/DATETIME/TIMESTAMP/YEAR/
  ENUM/SET/JSON/生成列，以及三种 collation）与 `orders` / `profiles` / `corpus_demo`；
- `init/single.sql`：规范数据集——`orders` 全量 80 行（4 片种子的并集）+
  `sql_type_test` 4 行（每个 shard_key 一行）+ `corpus_demo`；
- `init/shard0.sql` … `shard3.sql`：每片 20 行 `orders`（`user_id % 4 == 片号`）
  与 1 行 `sql_type_test`；
- `init/master.sql`：`repl` 复制账号（schema/数据复用 `single.sql`，经复制同步）；
- rwsplit 的 `bench` 库和 app 用户不落地到 replica 的 init，而是通过真实
  GTID 复制同步（与 `test/harness` 一致）；`scripts/setup-replication.sh`
  追平所有 GTID 后把 replica 设为 `read_only=ON`。

## 手工连代理

宿主机的 MySQL 9.x 客户端删掉了 native password，连不上；用 `mysql:8.0`
容器当客户端，网络名是 `shardweave-<MODE>_default`：

```sh
# single
docker run --rm --network shardweave-single_default mysql:8.0 \
  mysql -hshardweave -P3307 -uapp -papp-pass-123 bench -e "SELECT * FROM corpus_demo"

# sharding（4 片；user_id % 4 选片：0→s0 … 3→s3）
docker run --rm --network shardweave-sharding_default mysql:8.0 \
  mysql -hshardweave -P3307 -uapp -papp-pass-123 bench \
  -e "SELECT * FROM orders WHERE user_id = 2"
```

## SQL 语料（MTR golden）

主测试流程是 MTR 风格的 golden 对比，**正常跑只连代理**；直连只在录制 golden 时
执行一次：

```sh
make e2e-up MODE=single
make e2e-mtr-record MODE=single   # 从单机录制两个 suite（t/ 与 t_sharding/）
make e2e-test MODE=single         # 单机代理 vs r/*.result
make e2e-down MODE=single

make e2e-up MODE=sharding
make e2e-test MODE=sharding       # 4 分片代理 vs r_sharding/*.result
make e2e-down MODE=sharding
```

- `test/mtr/t/*.sql` → `r/*.result`：单机/读写分离路径，21 个 case，覆盖
  类型矩阵（整数/浮点/DECIMAL/位/字符/文本/二进制/时间/枚举/集合/JSON）、表达式与
  函数、JOIN/子查询/CTE/窗口/UNION、DML/DDL/SHOW、事务/SAVEPOINT/autocommit、
  会话变量、错误语义、注释/hint、锁读；
- `test/mtr/t_sharding/*.sql` → `r_sharding/*.result`：4 分片归并路径，9 个 case，
  覆盖 keyed SELECT/DML、同片 IN、scatter 聚合/GROUP BY/HAVING/ORDER BY/LIMIT/
  DISTINCT、类型聚合。golden 由**单机规范数据集**录制（`docker/init/single.sql`
  是 4 片种子的并集），之后只跑分片代理对比，不需要每次重跑单机；
- 指令子集：`--echo`、`--error`、`--sorted_result`、`--replace_column`、
  `--disable_query_log` / `--enable_query_log`、`--source`；
- 分片拒绝类语法（JOIN/事务/DDL 等 1235）仍由 `test/sharding/` 覆盖；MTR 分片
  suite 当前只放正例（keyed / scatter / merge）。

可选的直连 vs 代理差分（用于新增语料时探索，不在主门禁）：

```sh
make e2e-corpus MODE=single
```

YAML 语料在 `test/corpus/testdata/sql/**/*.yaml`，格式是 `id` + `setup` +
`steps`（同一组语句分别在直连和代理执行并对比）+ `teardown`。

rwsplit 的 MTR 尚未接入（需要录制后等待复制追平），后续按需扩展。

DSN 对应关系：

| MODE | DIRECT_DSN / SHARD*_DSN | PROXY_DSN |
|---|---|---|
| single | `app:app-pass-123@tcp(127.0.0.1:3308)/bench` | `...@tcp(127.0.0.1:3307)/bench` |
| rwsplit | master 3308 | 3307 |
| sharding | `SHARD0_DSN` 3308、`SHARD1_DSN` 3309、`SHARD2_DSN` 3312、`SHARD3_DSN` 3313 | 3307 |

DSN 需带 `parseTime=true&timeout=10s&readTimeout=40s&writeTimeout=40s`
（与 `test/harness` 的 `appDSN` 一致）。

## 注意

- 本机 Docker Hub 直连不可用；代理镜像用宿主编译兜底，MySQL 用已缓存的
  `mysql:8.0`。换机器时需保证该镜像可拉取。
- `make e2e-down` 会 `down -v`；MySQL 数据在容器内，重启即重置。
- 固定端口 3307–3309、3312–3313；同时只起一套 MODE，避免端口冲突。
