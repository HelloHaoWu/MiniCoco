# Proactive compaction eval

从 MiniCoco 根目录执行：

```bash
bash evals/proactive_compaction/run.sh
# 机器可读的 Go 测试事件流；PROACTIVE_EVAL 行包含度量 JSON
GO_TEST_JSON=1 bash evals/proactive_compaction/run.sh > /tmp/minicoco-eval.jsonl
```

只使用本机 HTTP 模拟上游和临时存储，不加载用户 .env、不访问真实模型。统一 check.sh 也运行本目录对应的 Go eval；check_realdb.sh 对真实 MongoDB/SQLite/Redis 运行同一 15 项矩阵和快照接口契约。

cases.json 是 3 组固定语料（编码任务、中文约束、长期权限），每组比较 baseline、hit、delta、miss、worker-failure。fixture 模型从请求中提取显式 facts；断言保留 decisions/paths/unresolved/constraints/conclusions。负控逐项删除事实或添加冲突约束必须失败。这是已知事实回归，不是开放语义评分，也不证明真实模型不会遗忘。

验收包括：精确命中 0 次关键路径摘要调用、delta 1 次折叠且只传旧摘要+增量、实际读取量至少减半、输入字节明显减少、失败 worker 同步兜底；压缩/trim 后固定约束仍原文存在，预算不足明确失败；新摘要导致前缀变化，后续追加轮稳定，重启后仍可比较；改动前置 memory/约束会失效；回放不虚报模型请求或压缩耗时，同轮更换约束被拒绝。

observed-file.json 为 2026-10-28 本机文件档实测快照，三组分别记录基线/命中/delta/失配/后台失败。coding 组基线 52 次对象读取、命中 12、delta 16；摘要输入 7008/0/223 字节。模拟上游人为增加 40ms 摘要延迟，观测压缩耗时约 52/13/60ms：命中消除了模型等待，但文件验证和持久化仍有成本；delta 虽输入小仍需一次调用，不能断言每次更快。墙钟耗时记录供分析，不使用易受机器负载影响的“必须接近 0ms”断言。

object_reads 计数实际成功读取的 transcript 消息对象：文件档包括 pending/final 的物理 JSONL 记录；Mongo 档按实际返回 envelope 中的输入、reply/占位消息计数。重复回源累计，缓存重放不计；不含摘要文件、元数据、Redis、索引扫描或后台任务。因此跨档绝对数不可直接比较，不是供应商 token 用量。优化来自同一持锁请求内的读取快照（16 MiB 上限、写入失效），并非 delta 自动免除了历史读取。冷请求仍读完整原始历史；文件档仍整文件重写并重复解析缓存字节，没有实现 O(delta) 的持久化读取。

prompt_cache_prefix_stable 比较当前请求与上一成功轮的“固定消息+tools+摘要锚点”诊断 hash，包含位于摘要前的 memory、environment、固定约束，以及模型/端点身份。新会话、旧记录无 hash、成功回放显示 unknown；新摘要采用/替换为 false，锚点不变的追加轮才为 true。没有摘要时只比较固定前缀。该值不是供应商缓存命中率或费用证明；没有测量真实 cache invalidation 频率下降。

默认 LOOP_IMPL=v2、MINICOCO_PRECOMPACT_RATIO=0.60，启用已通过本地 eval 的调度、复用及读取快照；显式 ratio=1 可关闭这三项，显式 v1 可回退基线。未增加快模型，未调用真实模型质量评估；真实部署扩大启用前仍需用目标模型验证语义和成本。约束配置见产品 README。
