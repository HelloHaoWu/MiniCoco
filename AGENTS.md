# MiniCoco Agent Harness 仓库约定

最后更新：2026-10-26（Asia/Shanghai）。目标是逐步完善 MiniCoco Agent Harness，保证模型请求、工具执行和会话状态具有明确的成功、失败及取消语义。

## 研发与维护规则

- 保持 Go 后端 + TypeScript/Node 客户端；默认后端仅依赖 Go 标准库，客户端运行时仅依赖 Node 内置模块。新增依赖须有明确用途。可选真实库实现的第三方 import 必须位于带 realdb 构建标签的文件，依赖只加入独立 go.realdb.mod/go.realdb.sum，不能污染默认 go.mod；不要在默认模块执行会扫描可选标签的 go mod tidy。
- 开始改动前检查 Git 状态，保留已有修改；本仓库的 Git 操作在本目录执行。真实密钥和 .env 不得进入提交或日志。
- **每次完成代码、配置或文档改动后，检查本 AGENTS.md 并根据实际变化动态修改。** 核对目标、目录、架构与协议、阶段进度、运行与自测命令、验证结果及能力边界；事实未变无需机械改写。若工作区上层 AGENTS.md 存在，同步其中涉及的同一事实，并在上层 ChangeLogs/ 记录检查结果和必要修订。
- 协议变更同步 Go/TypeScript 类型、消费器、mock、测试和产品说明。模型请求重试不得重复执行有副作用的工具，失败轮不得提交不完整 assistant/tool 批次；取消须贯穿请求、退避和工具，已执行的副作用不得声称回滚。
- 工具增加/升级需使用 name__vN.json 与 availableVersion，更新后端注册顺序、模式策略及客户端 Handler/策略。模式必须贯穿请求、重试和执行；完整批次检查在任何本地副作用之前完成。同轮不得变更模式，默认 full 保留旧轮次指纹。模式不是鉴权或 OS 沙箱。
- 工具结果先校验完整身份再按请求顺序格式化并落库。Native 保留旧字段/错误前缀；XML 必须转义文本并限制标签名称，按转义后的 Go JSON 最坏体积预留预算。已持久化结果的会话不得混用格式；默认 Native 指纹保持兼容。完整 prompt 调试默认关闭，仅在显式开关下输出本地 stderr，日志不得提交。
- 模型路由集中在 models.json / models.go，提示词、formatter 与传输共用单轮快照。修改路由须同步模型能力/提示词测试和配置迁移说明；maxTokens 用于 v2 上下文估算预算，不得误用作输出 max_tokens。未知模型在创建会话前拒绝，成功轮回放不调用供应商。模型规格须查官方资料，项目配置与实际供应商能力分开描述。
- 新单轮实现须通过 registerLoop 注册，保留 context 取消、固定 roundId 幂等、完整批次持久化后再暴露及统一失败结束边界；SSE/JSON 共用分流。空/未知 LOOP_IMPL 回退 v1，未实现的压缩能力不得宣称可用。相同 family/mode/agentType/slash/Schema（含 MCP 目录） 的 system/tools 须与工作区和历史无关，记忆索引不变时追加消息不改已有前缀；诊断 hash/模拟缓存不能当作真实命中或收益证据。
- 压缩只能改变模型视图，RawLatest 保留原始幂等、工具身份和计数；保存摘要必须校验完整批次边界及源指纹，独立去重摘要 usage 并支持回写恢复，不能用 Latest 的压缩长度更新原始账本。
- 每个 Part 达到对应行为验收要求后才标记完成；执行统一检查，区分历史验证与本次新运行结果。真实库验收使用 check_realdb.sh 并显式指定测试服务；缺少服务的 skip 不算验收通过。
- 在本工作区，每完成一个 Part 并通过验收，检查更新两层 AGENTS、完成上层 ChangeLog 及索引后，自动提交该 Part 的相关代码和仓库内文档并推送 GitHub 一次，用户已授权，无需逐次询问。提交前检查 diff/暂存范围，只提交相关改动，核实目标分支，不强推、不自动合并 PR。如实记录提交号及推送结果。
- 上层 AgentDocs/、ChangeLogs/、AGENTS.md 位于本仓库外，推送本仓库不会上传它们，不擅自改变仓库边界。独立克隆缺少阶段资料时，不根据摘要补造设计或验收结果。

## 目录与当前架构

- backend/：Go 持久化单轮请求服务。main.go 接收 HTTP 请求，agentloop.go 编排单轮，llm.go 调用模型；errors.go 分类失败，mock.go 提供确定性故障预算，transport.go 收集有界 JSON 事件。prompts/ 和 tools/ 编译嵌入提示词及八个内建工具 Schema。
- backend/tools.go：ToolName 注册、固定 read/list/grep/write/edit/run_command/delegate_subagent/memory_write 顺序、兼容最高 Schema 版本与模式过滤；tools/*__vN.json 含最低 Harness 版本 availableVersion，当前内部版本 1，元数据不发给供应商。prompt.go 使用请求模式构建清单，启动时检查全部资源。
- backend/loop_registry.go：LoopImpl、registerLoop、并发安全选择、LOOP_IMPL 与未知回退；v1 委托现有持久化服务。agentloop.go 的共享入口贯通 SSE/JSON；main.go 启动日志和 health.loop_impl 显示实际选择。loop_registry_test.go 验证分流/取消/回放/配置，prompt_cache_test.go 验证静态 hash、上游字节、历史追加和模拟缓存用量。
- backend/context_budget.go / compression.go / context_history*.go：v2 估算预算、micro/full 摘要、主动/反应式压缩、进度限制、锁/限流与摘要视图；RawLatest 保留原始幂等/恢复依据，Latest 合并校验后的摘要；源指纹与独立用量账本保证重开和回写恢复。
- backend/models.go / models.json：内嵌路由表、完整外部替代表、严格配置校验与单轮快照；llm.go 使用路由 provider/baseURL/temperature/thinking。prompts/system_openai.txt 与 system_claude.txt 按 family 选择；工作区仅注入首条 user 环境消息，不写 transcript。
- backend/toolresult.go：Formatter、NativeFormatter、XmlFormatter、完整批次身份校验、请求顺序、已存格式识别及结果预算；prompt.go 在 MINICOCO_DEBUG_PROMPT=1 时输出完整 messages。toolresult_test.go、client/src/toolresult.test.ts 和真实档专项覆盖两轨落库/恢复/回放。
- backend/sessions.go：稳定首轮会话、已提交历史、输入/预占、模型调用、final 和元数据回写、成功事件回放；session_http.go 提供列表和恢复元数据。
- backend/store.go：MetaStore / TranscriptStore / Coordinator 三接口。默认 metastore_json.go、transcript_jsonl.go、transcript_rounds.go、coord_inproc.go 实现 JSON 元数据、原子 JSONL 轮次、进程内协调；store_files.go 负责 ID 与原子文件写入。
- backend/wire_stores.go / wire_file.go / wire_realdb.go：按 STORE_BACKEND=file|real 装配，默认 file；未编入 realdb 的程序选择 real 时明确报错。metastore_sqlite_realdb.go、transcript_mongo_realdb.go、coord_redis_realdb.go 为可选实现；emitter.go 串行化本地/Redis 对 SSE 的投递。
- backend/store_contract_test.go：两档共用接口用例；stores_realdb_test.go：真实服务、并发唯一键、用量、跨进程协调及 HTTP/CLI 验收。默认 go.mod 为 Go 1.22、零第三方模块；go.realdb.mod / .sum 锁定可选驱动，要求 Go ≥ 1.26。
- client/src/：TypeScript CLI 和本地 Agent 循环。conversation.ts 只持有 sessionId/未确认增量并执行工具，sessions.ts 获取列表和恢复信息；executor.ts 校验事件，roundError.ts 定义错误，retry.ts 负责固定 roundId 重试和可取消退避，sse.ts 解析有界 SSE；tools/ 实现文件、搜索及命令工具。
- client/src/toolMode.ts 与 toolMode.test.ts：模式策略及两端协议消费/执行/CLI 回归；client/scripts/test.ts 为各测试文件启动独立进程，进程内直接运行 Node 原生测试，避开当前 Node 24.19.0 的内部 V8/stdout 转发缺陷；不移除文件级进程隔离。npm test 使用此入口，类型检查包括 scripts/。
- backend/trace.go、trace_http.go、jsonl_sink.go、audit.go：trace/usage 时间线、查询、异步 JSONL、摘要审计接收；client/src/trace.ts、approval.ts、audit.ts 对应 CLI 读取、审批及上报。
- backend/subagents.go、background.go、fork.go：只读子会话、后台完成增量、完整历史分叉；client/src/agents.ts、background.ts、sessions.ts 对应本地编排。
- backend/scenes.go、mcp.go、memory.go：场景、动态目录和记忆提示词；client/src/scenes.ts、mcp.ts、memory.ts 负责场景解析、stdio 服务、工作区记忆。
- check.sh：默认统一验收；check_default_deps.sh：空模块缓存、禁用下载检查；check_realdb.sh：显式真实服务验收；run_backend.sh / run_backend_realdb.sh / run_client.sh：启动入口。README.md 和 .env.example 提供使用及配置说明。

客户端提交 sessionId + 固定 roundId + query 或 toolUseResults 增量，完整历史与预算在后端；旧 messages 请求拒绝。首轮由固定 roundId 派生会话，响应头丢失也不重复创建；输入和 pending 原子预占，失败输入不进入 Latest，成功轮保存事件后直接回放。同会话并发返回 425。工作区绑定会话，恢复时使用保存的规范路径。

mode 可选，省略/空串等价 full（八个内建工具及已注册 MCP）；readonly 内建仅 read_file/list_dir/grep，safe 内建仅禁用 run_command、仍可写文件；safe/readonly 只放行显式 readOnly 的 MCP。CLI --mode 在本次调用每轮及重试/JSON 兜底保持固定，--resume 不继承上次模式，需显式重新指定。模式不作为会话权限保存。后端整批检查后才提交/暴露工具，客户端消费者和本地执行再次检查，未知工具拒绝；同 roundId 不能换模式。只有 full 在指纹中省略 Mode 字段，以兼容 Phase 2 已存储轮次。

工具输出按 Go JSON 转义限制为 64 KiB（含 HTML/Unicode 分隔符）；预留结果预算、提交 final 和元数据后才发送完整可执行工具批次/成功 done。CommitRoundUsage 按轮去重聚合，不能再对同轮额外 BumpUsage；transcript final 修复中断的元数据回写。缺失供应商 usage 时记 0，mock 每轮 1/1/2 是合成计数；成功用量不等于供应商全部计费。

模型路由 isFunctionCall=true 使用 Native（N 条 role:tool），false 使用 XML（一条 role:user，含 status/id/工具名）。默认真实模型、mock、mock-claude 为 Native，mock-xml 为 XML。结果顺序与请求一致，Native ERROR 前缀及旧指纹保持；XML 使用 xml-v1 标识，已有结果的会话不能切换格式。旧 MINICOCO_MODEL_IS_FUNCTION_CALL 非空明确报错，需迁移到路由字段。MINICOCO_MODELS_FILE 可指定完整替代表（1 MiB 上限，非合并），自定义模型须注册；表按新请求读取，修改请原子替换，pending 重试保持配置稳定。删除旧模型条目会阻断相应会话，须保留恢复所需路由。

默认 deepseek-flash / deepseek-v4-pro 路由 temperature=0.7、thinking=disabled；0.7 是阶段配置，maxTokens=131072 是 v2 启发预算配置，非供应商窗口/输出上限，v2 按 UTF-16 字符/2 向上估算，非真实 tokenizer；当前仍是 1 MiB / 512 条历史预算。OpenAI 兼容传输与 mock 两种 provider，family openai/claude 仅提示词差异，mock-claude 不代表 Anthropic API 已接入。LLM_PROVIDER=mock 离线覆盖；空/openai 遵从路由 provider，mock 路由保持本地；LLM_BASE_URL 非空覆盖所有 openai 路由端点，按模型使用不同端点时显式设为空。默认有效 provider 为 mock 时初始化 flaky 预算。thinking enabled 需先实现 reasoning_content 持久化/回传，当前配置拒绝。

此处仍只实现结果呈现，未实现 XML 工具调用生成/解析。MINICOCO_DEBUG_PROMPT=1 显式输出完整 messages，可能含私密代码/用户文本，默认关闭。system/tools 在相同 family/mode 下与 workspace 无关，环境消息在 system 之后、历史之前；不表示已验证供应商缓存命中。会话元数据保存首轮模型，显式 --model 只覆盖本次调用，恢复覆盖需再次传入。

LOOP_IMPL 默认/空为 v1，未注册键回退 v1；环境和 .env 支持，环境优先。生产已注册 v1/v2，v2 已实现预算、摘要和压缩重试；测试假实现为 test-v2；无用户分桶或 HTTP 注册接口。注册拒绝空/重复/空实现，读写锁保护；分流键不改变协议或旧轮次指纹，pending 重试保持实现/路由配置稳定，不表示自动数据迁移。缓存诊断 hash=SHA256(system_text+tools_json)，非完整 HTTP 请求或供应商 cache key。相同 family/mode/agentType/slash/Schema（含 MCP 目录） 下，工具 JSON 字节、工作区隔离和历史前缀追加已验收；改 family/mode/Schema 可使 hash 改变。cacheHandle、命中指标持久化和真实命中率/延迟/费用尚未实现或验证。

默认文件档 data/ 相对后端 cwd（启动脚本为 backend/data/），MINICOCO_DATA_DIR 可设绝对路径，每目录仅一个写入实例。JSONL 整文件原子替换，协调器只在进程内生效。部署须持久化数据目录。

真实档 SQLite 保存 meta.sqlite，WAL 和事务保障轮次账本/用量原子更新；Mongo 每轮一个原子文档，唯一 (sessionId,roundId)，保存输入、pending/final、事件、usage 和顺序；正文 JSON 字节保留字段与整数精度。auto_llm_message / auto_llm_summary / auto_llm_sequence 分别保存轮次、摘要和顺序。两库没有跨库事务，仍以 transcript final 恢复元数据。两档无自动数据迁移。

Redis 提供 TTL 锁、Lua CAS 解锁、固定窗口限流和 feature_toggle 开关；inbox 订阅匹配本节点 X-Emitter-Id 并投递 SSE。Pub/Sub 是实时至多一次投递，不能替代存储或任务队列；只有可信后端可发布已授权且按需持久化的事件，模型仍在接收节点运行。共享业务数据的实例须使用同一 SQLite 文件、Mongo 数据库和 Redis 前缀；SQLite 多进程共享只支持本机本地磁盘方案，未实现跨机器控制面。

--list / --resume 使用后端元数据和历史。同进程保留的工具结果可续传；退出后若存在未完成工具批次，拒绝自动重放。无跨进程工具执行日志、外部副作用 exactly-once 或回滚。SSE/JSON 共用流式上游；瞬时故障最多 8 次流式 + 1 次 JSON，fatal/取消不兜底。v2 已实现主动/反应式压缩、摘要合并和最多 3 次进度保证；v1 不自动压缩。

后端默认本机监听、无鉴权，仅本地匿名会话；文件工具检查路径与符号链接，命令 cwd 不是 OS 沙箱。已验证本机真实服务和跨进程协调，未验证生产多机器部署、跨平台或真实模型 API。


- Phase 6—9 当前能力：trace/usage 与有界 JSONL、GET /trace、CLI trace；写工具审批/摘要 audit；run_command 进程组 RSS/时间/输出保护；explore 只读子会话、最多 4 个后台任务与 task_complete 回填、完整历史分叉；composer/qa/review 场景；显式 stdio MCP；工作区文件记忆。所有本地工具和 MCP 进程仍在客户端执行。
- memoryIndex 与 MCP 目录由客户端传送，主 system/tools 的静态前缀由 family/mode/agentType/slash/Schema 决定；动态记忆 reminder 是第二条 system，索引改变时完整消息前缀会改变。记忆正文不全量传送，同一 roundId 固定索引/目录快照，v2 固定预算包括两者；新轮可刷新。
- 当前八个内建工具，full 全部开放，safe 排除 run_command，readonly 仅 read/list/grep；MCP 在 safe/readonly 只开放用户声明 readOnly 的服务。审批是工具执行门槛，mode/readOnly/提示词都不构成 OS 沙箱或远程鉴权。

## 阶段与验证状态

当前研发阶段：Phase 1—9 的全部现有 Part 已验收；当前路线实现结束。各 Part 验证和边界见末尾记录，发布见上层 ChangeLogs；最新维护为 CLI 正文重复显示修复（016）。

从仓库根目录执行，默认要求 Go ≥ 1.22、Node ≥ 24：

```bash
npm --prefix client ci
bash check.sh
LLM_PROVIDER=mock bash run_backend.sh
# 另一个终端
./run_client.sh "列出工作区文件" --workspace /path/to/workspace
# 只读检查；safe 允许文件修改但不执行命令
./run_client.sh "检查项目" --workspace /path/to/workspace --mode readonly
```

真实档要求 Go ≥ 1.26，先准备 MongoDB / Redis 并设置连接配置：

```bash
STORE_BACKEND=real LLM_PROVIDER=mock bash run_backend_realdb.sh
MINICOCO_TEST_MONGO_URI=mongodb://127.0.0.1:27017 MINICOCO_TEST_REDIS_URL=redis://127.0.0.1:6379/0 bash check_realdb.sh
# 手动构建在 backend/：go build -modfile=go.realdb.mod -tags realdb .
```

## Phase 5 实现与历史验收（2026-10-26）

- context_budget.go 以 UTF-16 字符/2 向上估算文本、工具调用/ID；图片 base64 估算仅是估算器能力，HTTP/存储仍为文本协议。maxTokens 扣除主 system/tools/记忆 reminder/环境后为历史额度，>0.65 micro、>0.80 full；这是启发预算，不是供应商 tokenizer 或输出 max_tokens。
- LOOP_IMPL=v2 启用 compression.go，v1 保留不自动压缩的基线；两者均可读取已存在的摘要视图。主动预算与上游 need_compression 共用 compactContext，每个后端请求最多 3 次压缩/trim 尝试；新 query 保最后 user，工具轮保最后完整 Native/XML 批次。范围至少替换 3 条为两条摘要前缀，要求消息数和估算 token 都严格下降；缩不动或受保护工具块放不下明确 cannot_compress。
- micro 压最旧的小段，full 压尾段前历史。summarize 对真实兼容端点发无工具摘要请求，要求保留决策/路径/未决事项/结论；mock 用确定性占位，摘要不超过 8 KiB。摘要请求自身超限、摘要锁不可用或全局 rate:summary（1/10s）拒绝时只生成安全 trim 视图；取消贯穿请求，不提交不完整模型轮次。
- context_history.go / context_history_realdb.go：TranscriptStore 增加 RawLatest/Summaries，Latest 返回 user 占位+assistant 摘要+未压段。Summary 通过 PrefixCount 和 PrefixHash 校验不可变原始前缀；originStartId/EndId 是跨存储通用的逻辑消息序号。旧无 checkpoint 的摘要保持只保存、不自动合并。RawLatest 决定 pending stale、工具身份、恢复安全及 MessageCount，不能拿压缩后的长度当账本计数；原始正文与成功事件不删除，压缩不减少磁盘存储。
- 摘要保存成功后其真实 usage 以独立内部账本 ID 去重提交；reconcile 可恢复摘要已写但 meta 未回写的情况。NUL 前缀为内部摘要用量域，用户 roundId 拒绝 NUL，防止账本碰撞。失败供应商请求可能仍产生未统计费用。mock 摘要 usage=0，普通 mock 轮仍为 1/1/2。
- v2 对每次模型尝试暂存全部事件，成功持久化后统一发出，SSE 不再逐 token 实时显示；避免失败文本混入压缩重试结果。v1 保持原流式输出，客户端只提交原增量，不重跑工具；v1 收到 need_compression 时仍明确终止，客户端没有历史可自行压缩。

Part 1 已验收并推送 d3c0f5d，见 ChangeLog 004。Part 2 本次两档完整验收通过：默认零依赖、空模块缓存/禁用下载/无 cgo、Go -race/vet/构建、TS 类型检查、10 项工具自测及 52 项客户端测试（6 文件）通过，Go 覆盖率 84.6%；真实档 -race/vet/无 cgo/驱动包含检查通过，覆盖率 83.1%。新增 6 项默认 Go 顶层压缩专项（含子用例）、真实档共享压缩持久化/重开/摘要用量恢复专项、1 项长会话真实 CLI 重启恢复测试。临时独立端口 MongoDB/Redis 已停止；未调用真实模型 API。Part 2 发布见 ChangeLog 2026-10-26-005。

当前资料 Phase 1—9 共 30 个 Markdown 文件；各 Part 的当前状态以阶段表和最新验收记录为准。未实现 OS 沙箱、跨进程工具日志/副作用 exactly-once、XML 工具调用解析、原生 Anthropic 协议或真实缓存收益验证。摘要是有损视图，语义质量尚无真实模型实测；默认文件档仍整文件重写，长期原始日志与摘要文件会增长，当前无物理日志清理。1 MiB/512 条限制应用于模型视图，已有工具结果/请求大小限制不变。

## Phase 6 Part 1 历史验收（2026-10-26）

Phase 6 Part 1：trace.go 定义无正文/参数的 TraceRecord；X-Trace-Id=sessionId-r<已提交轮序>，RoundID 区分失败尝试，成功回放复用轮序并标记 replay。阶段贯通 SSE/JSON、错误 code/retryable、路由 model/provider/family；工具结果和已提交工具调用分别记录，客户端错误保留 traceId。本 Part 仅埋点，生产持久化在 Part 2。

本次默认 check.sh 与真实 check_realdb.sh 均通过：Go -race/vet/构建、默认零依赖、TS 类型检查、10 项工具自测和 53 项客户端测试；真实档覆盖率 83.5%。临时服务继续供下一 Part 使用。阶段实现及发布记录见 ChangeLog 006，下一步 Phase 6 Part 2；Phase 7—9 尚未实施。

## Phase 6 历史验收（2026-10-26）

Phase 6 Part 2：jsonl_sink.go 提供容量 1024 的共享异步 JSONL 队列，满时丢最旧、写失败隔离，SIGINT/SIGTERM 关闭 HTTP 后有界 flush。trace 写入 backend/data/logs/trace-<sid>.jsonl（随 MINICOCO_DATA_DIR）；trace_http.go 提供 GET /trace/<sid>?round=N 和 /health.recentSessions 最近 10 会话轮数/账本用量/日志错误数；client/src/trace.ts 提供 trace <sid> [--round N]。普通 usage 仍只由 CommitRoundUsage 去重；摘要以 summary_request/summary_committed 记录独立账本用量。回放只记 replay，不重复计量。

本次 check.sh 与 check_realdb.sh 全通过：默认零依赖、Go -race/vet/构建、TS 类型检查、10 工具自测、54 客户端测试；默认覆盖率 83.5%，真实档 82.3%。专项覆盖三轮/回放/用量/health/过滤读取、真实写盘失败不影响 SSE、阻塞 writer 下丢最旧、CLI 时间线。未调用真实模型 API。

日志为 best effort，队列丢弃/磁盘错误/崩溃可能缺记录；错误数来自日志且异步可延迟，不是交易账本。服务端时间线文件限 8 MiB，客户端响应限 4 MiB，超限明确报错，尚无轮转。日志不含正文/参数，禁止用 trace 再 BumpUsage。Part 1 已推送 f7a71ea，Part 2 发布见 ChangeLog 007；下一步 Phase 7 Part 1，Phase 7—9 尚未实现。

## Phase 7 Part 1 历史验收（2026-10-26）

Phase 7 Part 1：write_file/edit_file/run_command 标记 requiresApproval；Conversation 执行前审批，拒绝作为 ERROR: denied by user 回灌并继续循环。CLI --yes 显式放行，TTY 交互 y/N，非 TTY 默认拒绝；readonly 工具免审批，模式禁用仍优先。取消贯穿审批。audit.ts 通过有界异步队列发送摘要记录至 POST /audit，后端复用 Phase 6 JSONL sink 写 data/audit.jsonl，保存 session/trace/tool/argsDigest/approved/status/ms，拒绝正文参数字段。CLI 退出有界 flush。

check.sh 和 check_realdb.sh 均通过：默认零依赖、Go -race/vet/构建、TS 类型检查、10 工具自测、57 客户端测试（7 文件）；默认覆盖率 83.4%，真实档 82.2%。新增拒绝后继续/只读免审批/--yes/管道默认拒绝/取消/审计摘要测试，后端共享 sink 和拒绝正文参数测试。真实 PTY 手动验收：出现 y/N，输入 n 后模型继续、退出 0、未生成目标文件。

审批不是 OS 沙箱；直接调用底层 Handler 的程序需自行提供审批入口。审计是本地客户端报告、best effort，不能作为安全证明或工具去重日志。--yes 表示本次调用显式批准危险工具。下一步 Part 2 进程资源保护和权限提示词；Phase 8—9 未实施。Phase 6 Part 2 已推送 f3feace，本 Part 发布见 ChangeLog 008。

## Phase 7 历史验收（2026-10-26）

Phase 7 Part 2：run_command 在 macOS/Linux 下监测整个普通子进程组的 RSS（100ms 采样），默认 1.5 GiB、120s 墙钟超时和 1 MiB 合并输出限额。超限/取消先 SIGTERM，再 SIGKILL 清理；输出超限保存 workspace/data/terminals/<uuid>.out 的有界已捕获前缀，返回尾部 2 KiB 和明确截断说明。RUN_OUTPUT_CAP / RSS_CAP / RUN_TIMEOUT_MS 从客户端环境读取并严格校验。permissions.txt 按阶段原文追加到模型族 system，静态前缀和预算均包含此段。

本次 check.sh 与 check_realdb.sh 均通过：默认零依赖、Go -race/vet/构建、TS 类型检查、10 工具自测和 60 客户端测试（8 文件）；默认覆盖率 83.3%，真实档 82.2%。实测 5 MiB 输出限制为 1 MiB 日志、RSS 杀死分配内存的后代且 PID 消失、墙钟超时/取消无后续标记文件、配置非法不执行命令；权限原文和固定预算纳入测试。

RSS 是采样保护，不是内核硬限制；自行脱离进程组的恶意命令、OS 沙箱和 Windows 命令保护未实现，Windows 明确拒绝 guarded command。日志只保存已捕获限额内输出，后续输出未保存；工具回传仍受 64 KiB JSON 预算。权限提示词为软约束，强制工具审批仍执行。真实模型是否主动声明意图尚未实测。Part 1 已推送 29cf85a，Part 2 发布见 ChangeLog 009；下一步 Phase 8 Part 1，Phase 8—9 尚未实施。

## Phase 8 Part 1 历史验收（2026-10-26）

Phase 8 Part 1：delegate_subagent__v1 工具及 explore 注册表，后端注入 enum/说明并使用阶段原文子 Agent system；客户端递归 Conversation，新 session、readonly、独立 10 轮预算，默认快模型 deepseek-flash（显式 mock 模型保持 mock），只回最终文本，未完成作为工具错误。Go 强制子会话只读，拒绝第三层派生和不同工作区；ParentSessionID/AgentType 在 JSON 和 SQLite 持久化。SQLite 新增独立 session_link 表，兼容旧 session 表，不迁移正文。CLI 恢复子会话同样锁定 readonly。

本次 check.sh 与 check_realdb.sh 通过：默认零依赖、Go -race/vet/构建、TS 类型检查、10 工具自测、61 客户端测试（8 文件）；默认覆盖率 83.5%，真实档 82.3%。最后的恢复权限收紧又通过类型检查与 sessions/toolresult 9 项回归。真实 Go+CLI+本机模拟上游验证父子各两轮、子工具仅 read/list/grep、父仅收到一条报告、重开父子关系保持，以及子预算耗尽不执行最后批次；file/real 共用关系持久化和父历史隔离测试。

full 七个工具，safe 排除 run_command、允许只读委派；readonly 仍只有 read/list/grep，子 Agent 不能继续委派。多个委派当前顺序执行，未实现并发子 Agent 调度。子中间步骤仅写子 transcript，不进入父历史；子失败/取消无副作用回滚。新增 run_command__v2 仅修正 Phase 7 后资源说明，参数兼容；后台任务尚未实现。无真实模型 API。Phase 7 Part 2 已推送 fd9cf37，本 Part 发布见 ChangeLog 010，下一步 Phase 8 Part 2；Phase 9 未实施。

## Phase 8 Part 2 历史验收（2026-10-26）

Phase 8 Part 2：run_command__v3 增加 run_in_background；客户端 BackgroundTasks 在同一 Conversation 中启动任务，立即返回随机 UUID taskId，最多 4 个未消费任务，复用审批、RSS/超时/输出保护。完成结果 backgroundResults 携带到下一轮增量；后端校验并构建完整 task_complete/ack 配对（Native/XML），与原输入原子预占、成功提交、重试回放。客户端快照深拷贝后台增量，成功重试不重跑命令。--wait-background 等待并消费结果，取消、SIGINT/SIGTERM 和 close 清理进程组。

check.sh 与 check_realdb.sh 均通过：默认零依赖、Go -race/vet/构建、TS 类型检查、10 工具自测、65 客户端测试（9 文件）。新增队列限额/立即返回/消费一次、同 session 后台结果不变重试与命令单次执行、退出后无延迟写文件；file/real 的 Native/XML 完整配对、重开回放和用量去重；真实 Go+CLI+模拟上游验证 --wait-background 后模型收到 background 和 task_complete。

后台进程仅在当前客户端存活；默认单次 CLI 退出会取消尚未完成任务，需要等待消费时用 --wait-background。REPL 可跨输入接收完成结果。无持久化任务队列、崩溃后任务恢复或 SIGKILL 清理保证；task_complete 是后端内部配对，不下发为可执行模型工具。后台工具审计 ok 表示启动已接受，最终完成/失败保存在后台增量中。父子 Agent 与后台任务路径独立；子 Agent 不支持后台命令。Part 1 已推送 9a3bfd7，本 Part 发布见 ChangeLog 011，下一步 Part 3 分叉；Phase 9 未实施。

## Phase 8 历史验收（2026-10-26）

Phase 8 Part 3：POST /fork 与 CLI --fork --resume <sourceSid> [--message assistant:N|message:N]。按 RawLatest 的逻辑序号分叉；assistant 工具调用前向包含完整 Native/XML 结果，未配对则最多回退 50 个 assistant，无法收敛报 FORK_BOUNDARY_UNRESOLVABLE；源会话锁忙报 FORK_SESSION_BUSY。先写独立 transcript/可覆盖前缀的摘要，再发布元数据；新 session 的 parentSessionId=null、forkedFrom=源、计费用量从 0 开始，SQLite 用新增 session_fork 表兼容旧库。

check.sh 和 check_realdb.sh 均通过：默认零依赖、Go -race/vet/构建、TS 类型检查、10 工具自测、66 客户端测试（9 文件）；默认覆盖率 83.2%。两档共享完整前缀、摘要复制、重开、分支继续与源历史/元数据不变测试；边界覆盖 Native/XML 前向配对和后向收敛、busy/无解；失败复制不发布半成品。真实 CLI 验证源 transcript 字节不变、fork 元数据和 --resume 独立续跑。

分叉点使用原始已提交消息序号，不是物理数据库 ID；assistant:N 包含工具调用消息，message:N 为全部原始消息序号。只复制有效且完全落在前缀内的摘要检查点，旧无范围摘要不拷贝；历史供应商费用归源会话，新分支不会重记。复制上限 16 MiB/10000 条，取消/失败可能留下未发布的孤立 transcript，不修改源；没有跨库事务或 fork 自动重试。fork 本身不请求模型，与 query 互斥，创建后单独 --resume 新 ID。后台 Part 已推送 334dfef，本 Part 发布见 ChangeLog 012，下一步 Phase 9 Part 1。

## Phase 9 Part 1 历史验收（2026-10-26）

Phase 9 Part 1：slash 场景注册表 composer/qa/review；省略或未知回退 composer。qa/review 后端与客户端均收窄为 readonly，composer 不扩大用户 --mode；系统提示词按 family 追加阶段给定场景原文，缺后缀文件回退族提示词。场景与 LOOP_IMPL 正交，加入非默认轮次指纹和 trace；同轮不能换场景，默认 composer 保持旧指纹。CLI 支持 /qa、/review、/composer 前缀及 --slash，REPL 每次输入可选场景。

check.sh 与 check_realdb.sh 通过：默认零依赖、Go -race/vet/构建、TS 类型检查、10 工具自测、67 客户端测试（10 文件）；默认覆盖率 83.3%，真实档 82.2%。两种 loop 下五种场景输入的实际上游提示词/工具清单、两族原文、缺后缀回退、同轮换场景拒绝；客户端即使 --yes 也拦截 qa/review 写操作，新增 CLI slash 传输专项 5 项回归通过。

场景按本次请求选择，不保存为会话权限；--resume 可选新场景，但未确认轮必须保持场景和增量。子 Agent 始终 readonly；场景只改变提示词/工具，不新增循环实现或鉴权。Phase 8 Part 3 已推送 919837b，本 Part 发布见 ChangeLog 013；下一步 Part 2 MCP，跨会话记忆未实施。

## Phase 9 Part 2 历史验收（2026-10-26）

Phase 9 Part 2：客户端 mcp.ts 实现 MCP 2024-11-05 stdio JSON-RPC 握手、分页 tools/list、命名空间目录和 tools/call；--mcp-config 显式启动可信配置，不自动执行工作区配置。目录作为增量 mcpTools 传到 Go，纳入工具清单、预算、整批模式检查和轮次指纹，重试深拷贝；SSE/JSON 共用客户端目录校验，Native/XML 回填兼容。写工具复用审批和摘要审计，safe/readonly/qa/review 只允许显式 readOnly:true 服务。

check.sh 与 check_realdb.sh 均通过：默认零依赖、Go -race/vet/构建、TS 类型检查、10 工具自测、70 客户端测试（11 文件）；默认覆盖率 83.3%，真实档 82.2%。本地 mock stdio 验证握手顺序、并发 ID、取消/超时/退出、缺失服务隔离、审批和回灌；Go 验证实际目录/模式/成功回放/同轮目录改变拒绝；真实 Go+CLI+模拟上游验证 v2 下发现→调用→结果→完成。

最多 8 服务/64 工具、64 KiB 配置及目录、单行 1 MiB、30 秒 RPC；输出限额和进程退出清理有界。MCP 调用不自动重试，取消通知不保证外部副作用停止。readOnly 是用户对服务的声明，服务启动本身可执行代码，无 OS 沙箱；explore 不继承 MCP。仅 stdio，不实现 HTTP、目录动态刷新或服务端主动采样。未调用真实模型 API。Part 1 已推送 d2ff728，本 Part 发布见 ChangeLog 014；下一步 Part 3 工作区记忆。

## 最新进度：Phase 9 完成（2026-10-26）

Phase 9 Part 3：memory_write 与 --remember，在客户端 workspace/data/memory 保存 frontmatter 条目和 MEMORY.md 索引，同名更新、operation=delete 删除。索引每行 150 Unicode 字符/总 16 KiB，正文 64 KiB，文件名来自 name 的稳定哈希；文件路径检查、原子替换和 0600 文件权限。主 system 追加阶段 memory.txt 原文，独立第二条 system-reminder 逐字包含索引（空为 empty），正文仅按需 read_file。新轮刷新 memoryIndex，重试固定快照，Go 纳入指纹与 v2 固定预算；无效索引明确提示并省略，避免已执行工具结果丢失。

memory_write 在 full/safe 开放、readonly/qa/review 禁止，走审批和摘要审计；--remember 不请求模型/创建会话，默认类型 preference，以 workspaceDigest 记录无 session 审计，取消返回 130。先写条目再索引，删除反向执行；没有两文件事务或多客户端写锁，每个工作区只支持一个记忆写入客户端。失败可能留下孤立条目，审计仍为 best effort。分叉同工作区共享当前记忆，不复制记忆文件。

check.sh 与 check_realdb.sh 均通过：默认零依赖、Go -race/vet/构建、TS 类型检查、10 工具自测、73 客户端测试（12 文件）；默认覆盖率 83.3%，真实档 82.2%。CLI 取消码调整后追加类型检查及审批/记忆/真实 CLI 13 项回归通过。验证索引/条目隔离、更新/删除、符号链接和大小限制、拒绝不写、单轮重试快照、主静态前缀不随记忆变化、预算计入、同轮换索引拒绝；真实 Go+CLI+模拟上游完成 --remember→新会话仅索引→读条目→读当前代码→采用当前代码，并核查审计无正文。首次全量检查发现旧恢复测试仍预期一个 system，已按新双 system 协议修正后全量通过；早期新增 Go 测试切片类型错误已修复。

记忆是上下文提示，不自动验证事实。冲突处理是本机模拟流程验收，未调用真实模型验证语义判断、摘要质量或供应商缓存收益。未实现 OS 沙箱、鉴权、跨机器控制面、跨进程工具副作用 exactly-once、MCP HTTP、XML 工具调用解析或原生 Anthropic 协议。已检查同步两层 AGENTS、目录职责、当前工具数/状态和 README，清理 Phase 6 尚未收到等过期描述；历史记录保留当时验收。Phase 9 Part 2 已推送 ef9c014，本 Part 发布见 ChangeLog 015。

## CLI 显示修复（2026-10-26，ChangeLog 016）

修复 CLI 完成时重复显示正文：executor 已渲染 SSE/JSON 文本事件，cli.ts 在 completed 后仅输出换行，不再输出 === answer === 和 result.text。Conversation 返回的完整 text、子 Agent quiet 返回、工具循环、模型请求和持久化不变；thinking 与 text 的事件区分未改。客户端源码直接由 Node 加载，退出旧客户端并重新启动即可生效，后端无需重启。

本次 bash check.sh 通过：默认零依赖检查、Go -race/vet/构建（Go 测试缓存命中，覆盖率 83.3%）、TS 类型检查、10 项工具自测及 73 项客户端测试（12 文件）。扩展已有回归验证分片正文、单次/连续输入只输出一次、请求次数不增加，以及默认/v1/v2/未知回退下真实 Go+CLI 输出一次且会话消息数/用量不变。本次未运行可选真实数据库档，未改后端、协议或存储；测试使用临时端口和 mock/模拟上游，未调用真实模型或停止用户后端。
