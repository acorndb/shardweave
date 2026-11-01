# 设计文档索引

架构基线（总览 + 按模块分类的介绍）已移至 [../architecture/](../architecture/README.md)；
本目录保留设计过程档案：提案、专项设计与评估快照。**实现偏离设计时先改文档**的
落点优先是 `../architecture/`（基线）与本目录的对应专项设计。

- [module-map.md](module-map.md)：R1–R6 重构期的模块映射与接口草案（历史档案；
  当前模块边界以 `../architecture/` 为准）
- [sharding-p0-design.md](sharding-p0-design.md)：分片设计、已实现能力与拒绝边界
- [merge-engine-design.md](merge-engine-design.md)：归并算子与实现状态
  （含 collation 专项 §13）
- [statement-classification.md](statement-classification.md)：语句判定统一
  （token 单真相源）设计与落地状态
- [ops-plane-design.md](ops-plane-design.md)：M4a 运维面设计（admin HTTP / 池与
  后端健康 / 优雅升级）——**设计稿，开发待人工 review 完成后启动**
- [test-infrastructure.md](test-infrastructure.md)：测试分层、真实拓扑与 oracle
- [industrial-readiness-gap.md](industrial-readiness-gap.md)：单机剩余差距
  （2026-10-31 快照，池/会话/归并部分已演进）

历史文档（M1/三 Executor/早期 rwsplit 设计与计划）已于 2026-10-31 清理，
需要追溯时看 git history。新增文档请直接列到本索引。
