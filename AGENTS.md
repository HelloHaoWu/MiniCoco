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

- `backend/`：Go 单轮请求服务。`main.go` 接收 HTTP 请求，`agentloop.go` 编排单轮，`llm.go` 调用模型；`errors.go` 分类失败，`mock.go` 提供确定性故障预算，`transport.go` 收集有界 JSON 事件。`prompts/` 和 `tools/` 编译嵌入提示词及六个工具 Schema。
- `backend/store.go`：MetaStore / TranscriptStore / Coordinator 三接口及 Session、Message、Usage、Summary 类型。`store_files.go` 负责 ID 与原子落盘；`metastore_json.go` 保存元数据、用量、点赞及检查点；`transcript_jsonl.go` 保存消息、round 去重、pending/final 和摘要；`coord_inproc.go` 提供 TTL 锁、固定窗口限流、开关与单机广播空实现。
- `client/src/`：TypeScript CLI 和本地 Agent 循环。`conversation.ts` 持有内存历史并执行工具；`executor.ts` 负责请求及完整事件校验，`roundError.ts` 定义类型化错误，`retry.ts` 负责可取消退避和压缩接口接缝，`sse.ts` 解析有界 SSE；`tools/` 实现文件、搜索及命令工具。
- `check.sh`：统一构建、静态检查与测试；`run_backend.sh` / `run_client.sh`：启动入口；`README.md`：使用方式与能力边界；`.env.example`：可提交配置模板。

客户端每轮回传全量 `messages`，后端请求管线尚未接入会话持久化；Phase 2 Part 1 的存储层独立存在，HTTP 入口不创建或使用它。`/chat/stream` 缺省或 `stream:true` 返回 SSE，`stream:false` 返回 `{events:[...]}` JSON；两种出口共用模型管线，上游仍使用流式调用。只有瞬时故障重试耗尽后才触发一次 JSON 兜底，fatal、取消和压缩失败不兜底。压缩只有接口接缝，默认报 cannot_compress，没有压缩算法。

文件存储每个数据目录只允许一个写入实例，实例内并发安全，无跨进程锁或跨文件事务。JSONL 采用整文件原子替换提交逻辑追加批次，同 round 重发不重复写入；Latest 排除 pending、去掉存储元数据、按首次位置保留顺序。摘要仅保存，尚不自动合并；预占 round 绑定与请求级去重留给 Part 2。进程内协调器不支持多实例；TTL 是租约，过期不取消工作，旧 owner 不得释放新 owner 的锁。数据目录 data/、backend/data/ 被 Git 忽略。

文件工具检查路径和符号链接；命令工作目录不构成 OS 沙箱。后端默认本机监听、无用户鉴权，真实多机器、多实例及跨平台部署尚未完整验证。

## 阶段与验证状态

Phase 1 Part 1（分类与 flaky mock，`89ad751`）、Part 2（类型化错误、可取消退避、压缩接缝，`a853254`）、Part 3（一次性 JSON 兜底，`845d067`）均已验收并推送。Phase 2 Part 1 的三接口及 JSON/JSONL/进程内默认实现也已完成并通过验收，发布记录见上层 ChangeLog 008。

下一项为 Phase 2 Part 2：迁移会话历史到后端、改用 sessionId/roundId 增量协议并实现 --resume/--list；实施时须同步修改本架构摘要、README 和协议说明。Part 2、可选 Part 3 真实数据库，以及 Phase 3—4 的工具版本与格式、模型路由和缓存前缀尚未实现；详细设计在上层 `AgentDocs/`。

最近一次完整验收发生于 2026-10-25 Phase 2 Part 1：Go 竞态测试、静态检查和构建、TypeScript 类型检查、10 项工具自测及 39 项客户端测试通过，后端覆盖率 88.3%。新增 12 项存储/协调器专项测试验证持久化重开、并发 round 去重、预占回写、用量累加、失败写盘、取消、路径与损坏数据校验，以及 TTL/限流；默认模块及依赖列表仅 minicoco 和 Go 标准库。验收使用本机模拟模型及假密钥，未验证真实模型 API、真实数据库或多进程共享写入。

从仓库根目录执行，要求 Go ≥ 1.22、Node ≥ 24：

```bash
npm --prefix client ci
bash check.sh
```

离线运行：一个终端执行 `LLM_PROVIDER=mock bash run_backend.sh`，另一个执行 `./run_client.sh "列出工作区文件" --workspace /path/to/workspace`。
