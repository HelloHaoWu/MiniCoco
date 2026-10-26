# MiniCoco Agent Harness 仓库约定

最后更新：2026-10-26（Asia/Shanghai）。目标是逐步完善 MiniCoco Agent Harness，保证模型请求、工具执行和会话状态具有明确的成功、失败及取消语义。

## 研发与维护规则

- 保持 Go 后端 + TypeScript/Node 客户端；默认后端仅依赖 Go 标准库，客户端运行时仅依赖 Node 内置模块。新增依赖须有明确用途。可选真实库实现的第三方 import 必须位于带 realdb 构建标签的文件，依赖只加入独立 go.realdb.mod/go.realdb.sum，不能污染默认 go.mod；不要在默认模块执行会扫描可选标签的 go mod tidy。
- 开始改动前检查 Git 状态，保留已有修改；本仓库的 Git 操作在本目录执行。真实密钥和 .env 不得进入提交或日志。
- **每次完成代码、配置或文档改动后，检查本 AGENTS.md 并根据实际变化动态修改。** 核对目标、目录、架构与协议、阶段进度、运行与自测命令、验证结果及能力边界；事实未变无需机械改写。若工作区上层 AGENTS.md 存在，同步其中涉及的同一事实，并在上层 ChangeLogs/ 记录检查结果和必要修订。
- 协议变更同步 Go/TypeScript 类型、消费器、mock、测试和产品说明。模型请求重试不得重复执行有副作用的工具，失败轮不得提交不完整 assistant/tool 批次；取消须贯穿请求、退避和工具，已执行的副作用不得声称回滚。
- 工具增加/升级需使用 name__vN.json 与 availableVersion，更新后端注册顺序、模式策略及客户端 Handler/策略。模式必须贯穿请求、重试和执行；完整批次检查在任何本地副作用之前完成。同轮不得变更模式，默认 full 保留旧轮次指纹。模式不是鉴权或 OS 沙箱。
- 工具结果先校验完整身份再按请求顺序格式化并落库。Native 保留旧字段/错误前缀；XML 必须转义文本并限制标签名称，按转义后的 Go JSON 最坏体积预留预算。已持久化结果的会话不得混用格式；默认 Native 指纹保持兼容。完整 prompt 调试默认关闭，仅在显式开关下输出本地 stderr，日志不得提交。
- 模型路由集中在 models.json / models.go，提示词、formatter 与传输共用单轮快照。修改路由须同步模型能力/提示词测试和配置迁移说明；maxTokens 只预留上下文预算，不得误用作输出 max_tokens。未知模型在创建会话前拒绝，成功轮回放不调用供应商。模型规格须查官方资料，项目配置与实际供应商能力分开描述。
- 每个 Part 达到对应行为验收要求后才标记完成；执行统一检查，区分历史验证与本次新运行结果。真实库验收使用 check_realdb.sh 并显式指定测试服务；缺少服务的 skip 不算验收通过。
- 在本工作区，每完成一个 Part 并通过验收，检查更新两层 AGENTS、完成上层 ChangeLog 及索引后，自动提交该 Part 的相关代码和仓库内文档并推送 GitHub 一次，用户已授权，无需逐次询问。提交前检查 diff/暂存范围，只提交相关改动，核实目标分支，不强推、不自动合并 PR。如实记录提交号及推送结果。
- 上层 AgentDocs/、ChangeLogs/、AGENTS.md 位于本仓库外，推送本仓库不会上传它们，不擅自改变仓库边界。独立克隆缺少阶段资料时，不根据摘要补造设计或验收结果。

## 目录与当前架构

- backend/：Go 持久化单轮请求服务。main.go 接收 HTTP 请求，agentloop.go 编排单轮，llm.go 调用模型；errors.go 分类失败，mock.go 提供确定性故障预算，transport.go 收集有界 JSON 事件。prompts/ 和 tools/ 编译嵌入提示词及六个工具 Schema。
- backend/tools.go：ToolName 注册、固定 read/list/grep/write/edit/run_command 顺序、兼容最高 Schema 版本与模式过滤；tools/*__vN.json 含最低 Harness 版本 availableVersion，当前内部版本 1，元数据不发给供应商。prompt.go 使用请求模式构建清单，启动时检查全部资源。
- backend/models.go / models.json：内嵌路由表、完整外部替代表、严格配置校验与单轮快照；llm.go 使用路由 provider/baseURL/temperature/thinking。prompts/system_openai.txt 与 system_claude.txt 按 family 选择；工作区仅注入首条 user 环境消息，不写 transcript。
- backend/toolresult.go：Formatter、NativeFormatter、XmlFormatter、完整批次身份校验、请求顺序、已存格式识别及结果预算；prompt.go 在 MINICOCO_DEBUG_PROMPT=1 时输出完整 messages。toolresult_test.go、client/src/toolresult.test.ts 和真实档专项覆盖两轨落库/恢复/回放。
- backend/sessions.go：稳定首轮会话、已提交历史、输入/预占、模型调用、final 和元数据回写、成功事件回放；session_http.go 提供列表和恢复元数据。
- backend/store.go：MetaStore / TranscriptStore / Coordinator 三接口。默认 metastore_json.go、transcript_jsonl.go、transcript_rounds.go、coord_inproc.go 实现 JSON 元数据、原子 JSONL 轮次、进程内协调；store_files.go 负责 ID 与原子文件写入。
- backend/wire_stores.go / wire_file.go / wire_realdb.go：按 STORE_BACKEND=file|real 装配，默认 file；未编入 realdb 的程序选择 real 时明确报错。metastore_sqlite_realdb.go、transcript_mongo_realdb.go、coord_redis_realdb.go 为可选实现；emitter.go 串行化本地/Redis 对 SSE 的投递。
- backend/store_contract_test.go：两档共用接口用例；stores_realdb_test.go：真实服务、并发唯一键、用量、跨进程协调及 HTTP/CLI 验收。默认 go.mod 为 Go 1.22、零第三方模块；go.realdb.mod / .sum 锁定可选驱动，要求 Go ≥ 1.26。
- client/src/：TypeScript CLI 和本地 Agent 循环。conversation.ts 只持有 sessionId/未确认增量并执行工具，sessions.ts 获取列表和恢复信息；executor.ts 校验事件，roundError.ts 定义错误，retry.ts 负责固定 roundId 重试和可取消退避，sse.ts 解析有界 SSE；tools/ 实现文件、搜索及命令工具。
- client/src/toolMode.ts 与 toolMode.test.ts：模式策略及两端协议消费/执行/CLI 回归；client/scripts/test.ts 为各测试文件启动独立进程，进程内直接运行 Node 原生测试，避开当前 Node 24.19.0 的内部 V8/stdout 转发缺陷；不移除文件级进程隔离。npm test 使用此入口，类型检查包括 scripts/。
- check.sh：默认统一验收；check_default_deps.sh：空模块缓存、禁用下载检查；check_realdb.sh：显式真实服务验收；run_backend.sh / run_backend_realdb.sh / run_client.sh：启动入口。README.md 和 .env.example 提供使用及配置说明。

客户端提交 sessionId + 固定 roundId + query 或 toolUseResults 增量，完整历史与预算在后端；旧 messages 请求拒绝。首轮由固定 roundId 派生会话，响应头丢失也不重复创建；输入和 pending 原子预占，失败输入不进入 Latest，成功轮保存事件后直接回放。同会话并发返回 425。工作区绑定会话，恢复时使用保存的规范路径。

mode 可选，省略/空串等价 full（六个工具）；readonly 仅 read_file/list_dir/grep，safe 仅禁用 run_command、仍可写文件。CLI --mode 在本次调用每轮及重试/JSON 兜底保持固定，--resume 不继承上次模式，需显式重新指定。模式不作为会话权限保存。后端整批检查后才提交/暴露工具，客户端消费者和本地执行再次检查，未知工具拒绝；同 roundId 不能换模式。只有 full 在指纹中省略 Mode 字段，以兼容 Phase 2 已存储轮次。

工具输出按 Go JSON 转义限制为 64 KiB（含 HTML/Unicode 分隔符）；预留结果预算、提交 final 和元数据后才发送完整可执行工具批次/成功 done。CommitRoundUsage 按轮去重聚合，不能再对同轮额外 BumpUsage；transcript final 修复中断的元数据回写。缺失供应商 usage 时记 0，mock 每轮 1/1/2 是合成计数；成功用量不等于供应商全部计费。

模型路由 isFunctionCall=true 使用 Native（N 条 role:tool），false 使用 XML（一条 role:user，含 status/id/工具名）。默认真实模型、mock、mock-claude 为 Native，mock-xml 为 XML。结果顺序与请求一致，Native ERROR 前缀及旧指纹保持；XML 使用 xml-v1 标识，已有结果的会话不能切换格式。旧 MINICOCO_MODEL_IS_FUNCTION_CALL 非空明确报错，需迁移到路由字段。MINICOCO_MODELS_FILE 可指定完整替代表（1 MiB 上限，非合并），自定义模型须注册；表按新请求读取，修改请原子替换，pending 重试保持配置稳定。删除旧模型条目会阻断相应会话，须保留恢复所需路由。

默认 deepseek-flash / deepseek-v4-pro 路由 temperature=0.7、thinking=disabled；0.7 是阶段配置，maxTokens=131072 是未来预算接缝，非供应商窗口/输出上限，本 Part 不计 token；当前仍是 1 MiB / 512 条历史预算。OpenAI 兼容传输与 mock 两种 provider，family openai/claude 仅提示词差异，mock-claude 不代表 Anthropic API 已接入。LLM_PROVIDER=mock 离线覆盖；空/openai 遵从路由 provider，mock 路由保持本地；LLM_BASE_URL 非空覆盖所有 openai 路由端点，按模型使用不同端点时显式设为空。默认有效 provider 为 mock 时初始化 flaky 预算。thinking enabled 需先实现 reasoning_content 持久化/回传，当前配置拒绝。

此处仍只实现结果呈现，未实现 XML 工具调用生成/解析。MINICOCO_DEBUG_PROMPT=1 显式输出完整 messages，可能含私密代码/用户文本，默认关闭。system/tools 在相同 family/mode 下与 workspace 无关，环境消息在 system 之后、历史之前；不表示已验证供应商缓存命中。会话元数据保存首轮模型，显式 --model 只覆盖本次调用，恢复覆盖需再次传入。

默认文件档 data/ 相对后端 cwd（启动脚本为 backend/data/），MINICOCO_DATA_DIR 可设绝对路径，每目录仅一个写入实例。JSONL 整文件原子替换，协调器只在进程内生效。部署须持久化数据目录。

真实档 SQLite 保存 meta.sqlite，WAL 和事务保障轮次账本/用量原子更新；Mongo 每轮一个原子文档，唯一 (sessionId,roundId)，保存输入、pending/final、事件、usage 和顺序；正文 JSON 字节保留字段与整数精度。auto_llm_message / auto_llm_summary / auto_llm_sequence 分别保存轮次、摘要和顺序。两库没有跨库事务，仍以 transcript final 恢复元数据。两档无自动数据迁移。

Redis 提供 TTL 锁、Lua CAS 解锁、固定窗口限流和 feature_toggle 开关；inbox 订阅匹配本节点 X-Emitter-Id 并投递 SSE。Pub/Sub 是实时至多一次投递，不能替代存储或任务队列；只有可信后端可发布已授权且按需持久化的事件，模型仍在接收节点运行。共享业务数据的实例须使用同一 SQLite 文件、Mongo 数据库和 Redis 前缀；SQLite 多进程共享只支持本机本地磁盘方案，未实现跨机器控制面。

--list / --resume 使用后端元数据和历史。同进程保留的工具结果可续传；退出后若存在未完成工具批次，拒绝自动重放。无跨进程工具执行日志、外部副作用 exactly-once 或回滚。SSE/JSON 共用流式上游；瞬时故障最多 8 次流式 + 1 次 JSON，fatal/取消不兜底。compressSeam 是空接缝，need_compression 明确失败，摘要不自动合并。

后端默认本机监听、无鉴权，仅本地匿名会话；文件工具检查路径与符号链接，命令 cwd 不是 OS 沙箱。已验证本机真实服务和跨进程协调，未验证生产多机器部署、跨平台或真实模型 API。

## 阶段与验证状态

Phase 1、Phase 2、Phase 3 各 Part 及 Phase 4 Part 1 已实现并验收。本次前置已推送基线 7afd2b5；发布提交与推送结果见上层 ChangeLog 2026-10-26-002。Phase 4 Part 2、真实压缩、XML 工具调用解析、原生 Anthropic 协议、跨机器控制面及跨进程工具日志尚未实现。

2026-10-26 本次完整验收：

- 默认 check.sh 通过：空模块缓存/禁用下载/无 cgo 构建、模块与非标准库列表仅 minicoco；Go -race、vet、构建；TS 类型检查、10 项工具自测、50 项客户端测试（6 个独立文件全部成功）。默认后端覆盖率 84.3%。
- 新增 5 项 Go 顶层专项（含子用例）：路由字段/默认模型/离线覆盖/路由 mock 故障预算；未知模型 SSE/JSON 前置 400、不建会话；外部表和字段校验/限长/错误脱敏；双 family 与 workspace 注入；自定义端点、temperature/thinking、XML 回填、持久化与成功回放、单轮快照。Phase 3 格式专项迁移到路由字段，错误、重试、模式与工具不重放回归继续通过。
- 客户端真实 Go/CLI 专项覆盖 mock、mock-xml、mock-claude 三路由的重启恢复/列表、保存模型元数据，以及未知模型只请求一次、不退避、不建会话。既有恢复测试同步环境 user 消息位置。
- check_realdb.sh 全量 -race、vet、无 cgo 构建及驱动包含检查通过，覆盖率 82.8%；真实 Mongo XML 入库、重开、成功回放及共同接口/并发/跨进程协调继续通过。未改存储接口或驱动依赖。
- 仅使用临时目录、独立端口 MongoDB 8.0.16/Redis 8.10.2、随机数据库/前缀、本机模拟模型与假密钥；临时数据库进程已停止。未调用真实模型 API，未验证缓存命中收益。

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
