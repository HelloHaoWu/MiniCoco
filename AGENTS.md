# MiniCoco Agent Harness 仓库约定

最后更新：2026-10-25（Asia/Shanghai）。目标是逐步完善 MiniCoco Agent Harness，保证模型请求、工具执行和会话状态具有明确的成功、失败及取消语义。

## 研发与维护规则

- 保持 Go 后端 + TypeScript/Node 客户端；默认后端仅依赖 Go 标准库，客户端运行时仅依赖 Node 内置模块。新增依赖须有明确用途。
- 开始改动前检查 Git 状态，保留已有修改；本仓库的 Git 操作在本目录执行。真实密钥和 `.env` 不得进入提交或日志。
- **每次完成代码、配置或文档改动后，检查本 `AGENTS.md` 并根据实际变化动态修改。** 核对目标、目录、架构与协议、阶段进度、运行与自测命令、验证结果及能力边界；事实未变无需机械改写。若工作区上层 `AGENTS.md` 存在，同步其中涉及的同一事实，并在上层 `ChangeLogs/` 记录检查结果和必要修订。
- 协议变更同步 Go/TypeScript 类型、消费器、mock、测试和产品说明。模型请求重试不得重复执行有副作用的工具，失败轮不得提交不完整 assistant/tool 批次；取消须贯穿请求、退避和工具，已执行的副作用不得声称回滚。
- 每个 Part 达到对应行为验收要求后才标记完成；执行统一检查，区分历史验证与本次新运行结果。
- 在本工作区，每完成一个 Part 并通过验收，检查更新两层 AGENTS、完成上层 ChangeLog 及索引后，自动提交该 Part 的相关代码和仓库内文档并推送 GitHub 一次，用户已授权，无需逐次询问。提交前检查 diff/暂存范围，只提交相关改动，核实目标分支，不强推、不自动合并 PR。如实记录提交号及推送结果。
- 上层 `AgentDocs/`、`ChangeLogs/`、`AGENTS.md` 位于本仓库外，推送本仓库不会上传它们，不擅自改变仓库边界。独立克隆缺少阶段资料时，不根据摘要补造设计或验收结果。

## 目录与当前架构

- `backend/`：Go 持久化单轮请求服务。`main.go` 接收 HTTP 请求，`agentloop.go` 编排单轮，`llm.go` 调用模型；`errors.go` 分类失败，`mock.go` 提供确定性故障预算，`transport.go` 收集有界 JSON 事件。`prompts/` 和 `tools/` 编译嵌入提示词及六个工具 Schema。
- `backend/sessions.go` 编排稳定首轮会话、已提交历史、输入/预占、模型调用、final 和元数据回写、成功事件回放；`session_http.go` 提供会话列表和恢复元数据，`transcript_rounds.go` 保存轮次检查点和成功事件。
- `backend/store.go`：MetaStore / TranscriptStore / Coordinator 三接口及 Session、Message、Usage、Summary 类型。`store_files.go` 负责 ID 与原子落盘；`metastore_json.go` 保存元数据、用量、点赞及检查点；`transcript_jsonl.go` 保存消息、round 去重、pending/final 和摘要；`coord_inproc.go` 提供 TTL 锁、固定窗口限流、开关与单机广播空实现。
- `client/src/`：TypeScript CLI 和本地 Agent 循环。`conversation.ts` 只持有 sessionId/未确认增量并执行工具，`sessions.ts` 获取列表和恢复信息；`executor.ts` 负责请求及完整事件校验，`roundError.ts` 定义类型化错误，`retry.ts` 负责固定 roundId 的增量重试和可取消退避，`sse.ts` 解析有界 SSE；`tools/` 实现文件、搜索及命令工具。
- `check.sh`：统一构建、静态检查与测试；`run_backend.sh` / `run_client.sh`：启动入口；`README.md`：使用方式与能力边界；`.env.example`：可提交配置模板。

客户端使用 sessionId + 固定 roundId + query 或 toolUseResults 增量，完整历史与预算在后端。旧 messages 请求被拒绝，客户端/后端须同步升级。首轮无 sessionId 时由固定 roundId 派生会话，响应头丢失也不重复创建；成功轮持久化事件后可跨重启回放，不再次调用模型。输入和 pending 原子预占；失败输入和占位不进入 Latest，工具结果身份/数量须匹配完整批次，同会话并发返回 425。

单次 HTTP 请求至多调用模型一次，成功轮重试直接回放；工具输出按 Go JSON 转义限制为 64 KiB（含 HTML/Unicode 分隔符）；预留结果预算、提交 final 和元数据后才发送可执行工具批次/成功 done。使用 CommitRoundUsage 按轮去重聚合，不能再对同轮额外 BumpUsage；transcript final 可修复中断的元数据回写。供应商没有 usage 时记 0，mock 每轮 1/1/2 仅为合成测试计数，成功轮用量不等于供应商全部计费。

默认启动创建 data/（相对进程 cwd；run_backend.sh 为 backend/data/），MINICOCO_DATA_DIR 可设绝对路径；每目录一个写入实例，无跨进程锁或跨文件事务，JSONL 全文件原子替换，协调器仅进程内 TTL 租约与固定窗口限流。数据目录已被 Git 忽略。部署必须持久化该目录，不能按无状态服务多实例共享写入。

--list 使用 GET /sessions；--resume 读取 GET /sessions/<sid> 的元数据，并由后端恢复已提交历史，workspace 绑定会话。客户端同进程保留的工具结果可在重试相同 query 时续传；进程退出后如存在未完成工具批次，拒绝自动重放。无跨进程工具执行日志或副作用回滚，不承诺外部动作 exactly-once。

SSE/JSON 出口共用模型管线，上游仍流式；瞬时故障最多 8 次流式 + 1 次 JSON，fatal/取消不兜底。历史迁到后端后，旧客户端 CompressionHook 已撤下；后端 compressSeam 为 Phase 5 空接缝，need_compression 当前明确以 cannot_compress 失败，摘要只保存、不合并。

文件工具检查路径和符号链接，命令 cwd 不是 OS 沙箱；后端本机默认监听、无鉴权，目前仅本地匿名会话，真实多机器/多实例/跨平台未完整验证。

## 阶段与验证状态

Phase 1 三个 Part 与 Phase 2 Part 1 已完成并推送（最近基线 0d4e2de）。Phase 2 Part 2 的会话增量协议、持久化、重试去重和 --resume/--list 也已实现并验收；本次发布记录见上层 ChangeLog 009。

2026-10-25 本次统一检查通过：Go 竞态测试、go vet、构建、TypeScript 类型检查、10 项工具自测与 44 项客户端测试，后端覆盖率 83.9%。新增 7 项 Go 会话专项和 5 项客户端会话测试覆盖回放、usage 恢复、并发、预算、首轮响应丢失、真实后端/CLI 重启恢复和工具不重放；既有存储/可靠性测试已按新协议继续验收。默认模块与非标准库依赖列表均仅 minicoco；均为临时目录、本地模拟模型和假密钥，未验证真实模型 API 或真实数据库。

后续为可选 Phase 2 Part 3 真实数据库档，或按默认零依赖路线继续 Phase 3 Part 1；Phase 3—4、真实压缩、多实例协调及跨进程工具日志均未实现，本次未进入下一 Part。详细路线仍以工作区上层 AgentDocs 为准。

从仓库根目录执行，要求 Go ≥ 1.22、Node ≥ 24：

```bash
npm --prefix client ci
bash check.sh
```

离线运行：一个终端执行 `LLM_PROVIDER=mock bash run_backend.sh`，另一个执行 `./run_client.sh "列出工作区文件" --workspace /path/to/workspace`。
