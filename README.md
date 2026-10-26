# MiniCoco - 最小 Coding Agent Harness 实现（Go 后端 + TypeScript 客户端）

一个**可运行**的最小 coding agent，使用双端架构，以方便后端做独立服务扩展（"号池灵活路由"等功能）；

- **backend/**（Go）= 会话与历史的持有者。接收每轮增量，保存元数据和 transcript，预占模型回复、组装提示词并调用模型；成功落盘后才返回完整工具批次和成功结束事件。默认 SSE，也支持一次性 JSON 返回同一事件序列。后端写自己的会话数据目录，不执行工作区工具。
- **client/**（TypeScript）= 驱动 **Agent 工具循环**，在本地读写工作区、执行命令和搜索。仅保存 sessionId 与尚未确认的一轮增量，不持有全量历史；发送 query 或 toolUseResults，消费 SSE/JSON，执行完整工具批次，直到模型完成。

当前架构是**工具循环在客户端，会话主权与持久化在后端**。前后端通过 HTTP 请求和 SSE/JSON 返回通信。默认档后端仅依赖 Go 标准库，客户端运行时仅使用 Node 内置模块；可选 `realdb` 档引入 SQLite、MongoDB 和 Redis 驱动。

```
用户提问 -(client/TS)-> sessionId + roundId + query -> 后端存储/预占 -> 拼 prompt+tools -> 调LLM
        ———————————— SSE: text / tool_call ————————————
(client) 本地执行 tool_call -> 收集 toolUseResults -> 固定下一轮 roundId 再 POST
        -> 循环, 直到某轮没有 tool_call (finish=stop) 结束
```

## 环境要求
- **Go** ≧ 1.22（默认后端）；可选真实库档需要 Go ≧ 1.26
- **Node** ≧ 24（客户端使用原生 TypeScript 类型擦除，无需编译）
- 首次进行开发检查：在 `client/` 执行 `npm ci`。TypeScript 和 Node 类型声明仅用于开发，运行时仍只使用内置模块。

## 运行

开两个终端

默认模型为 **DeepSeek Flash**（`deepseek-flash`）。后端启动时自动读取项目根目录 `.env`，
也支持在 `backend/` 下放 `.env`（优先读取）；已有环境变量优先于文件配置。
配置完成后，运行 `bash run_backend.sh` 使用默认模型；实际可用性由供应商和账号决定。
新环境可复制 `.env.example` 为 `.env`，再填写自己的 `LLM_API_KEY`。
`.env` 已加入 `.gitignore`，不要将真实密钥写进源码或提交到仓库。

### 1) 离线演示 (无需 API Key, 内置 mock 模型)

```bash
# 终端 A: 显式选择离线 mock（默认已改为真实 DeepSeek）
LLM_PROVIDER=mock bash run_backend.sh

# 终端 B: 起 TS 客户端, 指定一个工作区
./run_client.sh "这个项目里有哪些文件?" --workspace /path/to/any/project
```

mock 模型走确定性的两轮循环 (list_dir -> 给结论), 证明闭环跑通, 不需要网络

#### 工具模式与 Schema 版本（Phase 3 Part 1）

```bash
./run_client.sh "只检查代码并提出建议" --workspace /path/to/project --mode readonly
./run_client.sh "修改代码，不执行命令" --workspace /path/to/project --mode safe
./run_client.sh "修复并运行测试" --workspace /path/to/project --mode full
```

| mode | 开放的工具 |
| --- | --- |
| full（默认） | read_file、list_dir、grep、write_file、edit_file、run_command |
| readonly | read_file、list_dir、grep |
| safe | read_file、list_dir、grep、write_file、edit_file |

请求体可选字段 `mode` 使用同名值，省略或空串等价 full，其他值拒绝。CLI 在本次调用的每轮请求、重试和 JSON 兜底中保持同一模式；`--resume` 不自动继承上次模式，需要再次指定 `--mode readonly` 等选项。模式不是持久化的会话权限，也不是操作系统沙箱；safe 仍允许修改文件。

后端按模式过滤发给模型的工具清单，并在成功提交前检查模型返回的完整工具批次；含禁用或未知工具则整批拒绝，不提交失败轮历史，也不发出可执行工具事件。客户端 SSE/JSON 校验和本地执行入口再次检查，整批验证后才执行，避免前面合法的工具先产生副作用。同一个 roundId 改变模式会失败；省略和显式 full 保留已有成功轮的回放兼容性。

工具资源现为 `backend/tools/<name>__vN.json`，`function.name` 保持原名；增加的 `availableVersion` 表示最低 Harness 协议版本，当前内部版本为 1，尚未开放客户端选择。加载器为每个工具选择**兼容版本中数值最大的 N**，再按 read/list/grep/write/edit/run_command 固定顺序输出。availableVersion 仅用于本地选择，不发给模型接口；默认 full 的六个函数定义与改造前一致。

#### 工具结果回填与调试（Phase 3 Part 2）

客户端继续提交 `toolUseResults: [{id,name,output,isError}]`。后端先校验结果完整匹配上一批调用，再按**请求顺序**格式化、持久化并加入 prompt；不按 ID 字典序排序。旧实现按调用顺序回填，现按文档统一为请求顺序，CLI 正常发送顺序保持不变。

- 路由 `isFunctionCall:true`（默认真实模型及 mock）：NativeFormatter，每个结果一条 `role:tool` 消息，保留 tool_call_id、原始输出及既有 ERROR 前缀语义。
- 路由 `isFunctionCall:false`（mock-xml 演示）：XmlFormatter，整批结果合成一条 `role:user` 消息，每项为 `<tool_result><status>succeeded|failed</status><id>调用ID</id><工具名>输出</工具名></tool_result>`。状态由 isError 决定；输出与 ID 转义，非法 XML 字符替换为 U+FFFD，工具名仅允许已注册名称。
- Phase 4 Part 1 已用模型路由字段替代全局能力开关。**此处实现结果呈现，未增加 XML 工具调用生成/解析**，上游仍使用现有 function-call 请求与响应管线。

```bash
# 用 mock 验证 XML 回填；建议使用独立数据目录
LLM_MODEL=mock-xml \
MINICOCO_DATA_DIR=/absolute/path/to/xml-demo bash run_backend.sh

# 显式开启完整 messages 调试输出（stderr，含工作区和会话内容）
MINICOCO_DEBUG_PROMPT=1 LLM_PROVIDER=mock bash run_backend.sh
```

路由配置和调试开关支持本地 .env，已导出环境变量优先。调试默认关闭，不输出 HTTP 认证头或配置中的 API Key，但 messages 本身可能含私密代码与用户文本，调试日志应留在本地。

格式化后的输入是持久化历史的一部分。XML 格式加入轮次指纹，默认 Native 保留旧指纹；同轮不能切换格式。会话已有工具结果后，切换到不同结果格式的模型会拒绝继续，需恢复原配置或新建会话，不自动转换旧历史。XML 会增加体积，工具批次暴露前按最坏转义后的 Go JSON 大小预留预算，超限提前失败；每项原始结果仍受 64 KiB 限制。

#### 模型路由与模型族提示词（Phase 4 Part 1）

后端将 `backend/models.json` 编译嵌入二进制，按请求 `model` 查表；空值首轮使用 `LLM_MODEL`（默认 deepseek-flash），恢复轮使用会话记录的模型。未知模型返回 HTTP 400、`code:bad_request`、`retryable:false` 和按名称排序的支持列表，不创建会话或调用上游；客户端按 HTTP 400 立即失败，不退避或 JSON 兜底。

| 模型路由 | provider / family | 工具结果 | temperature / thinking |
| --- | --- | --- | --- |
| deepseek-flash、deepseek-v4-pro | openai / openai | Native | 0.7 / disabled |
| mock | mock / openai | Native | 0 / 省略 |
| mock-claude | mock / claude | Native | 0 / 省略 |
| mock-xml | mock / openai | XML | 0 / 省略 |

`family` 选择 system_openai.txt 或 system_claude.txt；mock-claude 只演示提示词选择，未接入 Anthropic API。一个模型轮使用同一路由快照决定提示词、formatter 和上游参数。`LLM_PROVIDER=mock` 保留离线覆盖能力；空值/openai 使用路由 provider，mock 路由始终留在本地。`LLM_BASE_URL` 非空时覆盖所有 openai 路由的端点；按模型使用不同端点时应将其设为空。已提交成功轮直接回放，不因供应商环境配置失效而再次请求模型。

工作区路径现在作为 system 之后的首条 user 环境消息注入（仅非空路径），不会写入 transcript。相同 family/mode 的 system 和 tools 与工作区无关；未实施供应商缓存命中/费用测量。既有 1 MiB / 512 条历史预算继续生效。

路由 `maxTokens` 是给后续预算层预留的**项目配置上限**：DeepSeek 路由沿阶段设计取 131072，mock 取 32768；本 Part 不做真实 token 计数，也不把它发成输出 `max_tokens`。它不是供应商当前窗口规格：2026-10-26 官方页面列两模型上下文 1M；0.7 为本项目路线配置，非官方编码任务推荐值。两条 DeepSeek 路由均显式禁用默认 thinking，避免尚未实现 reasoning_content 持久化/回传时破坏工具多轮。[模型规格](https://api-docs.deepseek.com/quick_start/pricing/)、[thinking 说明](https://api-docs.deepseek.com/guides/thinking_mode/)、[温度建议](https://api-docs.deepseek.com/quick_start/parameter_settings/)。未调用真实模型验证账号可用性。

自定义 OpenAI 兼容模型通过 `MINICOCO_MODELS_FILE=/absolute/path/models.json` 提供**完整替代表**（1 MiB 上限），不隐式接收任意模型名：

```json
{
  "my-model": {
    "provider": "openai", "baseURL": "https://example.com/v1",
    "family": "openai", "maxTokens": 32768,
    "isFunctionCall": true, "temperature": 0.2
  }
}
```

相对配置路径从后端 cwd 解析；启动检查并在每个新请求读取，运行中更新文件请使用原子替换；单轮不会中途重读表。表内各项必须具备上例六个字段，provider 仅 openai/mock，family 仅 openai/claude；端点禁止内嵌认证、查询和 fragment，temperature 0..2，maxTokens 正整数；可选 thinking 仅支持 disabled 或省略，enabled 需待 reasoning 回传实现。其他供应商原生协议未接入。成功轮仍回放保存的结果；重试 pending 时需保持路由配置稳定，改路由建议使用新会话；删除旧模型路由会使对应会话无法继续/回放，应在替代表中保留所需条目。

**迁移：删除旧环境或 .env 中的 MINICOCO_MODEL_IS_FUNCTION_CALL**，改用每个路由的 isFunctionCall。旧键非空会明确报配置错误，避免全局值悄悄覆盖模型能力。自定义模型也需先加入路由表。会话元数据记录首轮模型；显式 --model 可覆盖某次调用的各轮请求，不改写会话默认模型，恢复时如需覆盖应再次指定。

```bash
# 无需真实供应商，验证另一模型族
LLM_MODEL=mock-claude bash run_backend.sh
# 使用自定义完整路由表，并由表决定不同模型的端点
MINICOCO_MODELS_FILE=/absolute/path/models.json LLM_MODEL=my-model LLM_BASE_URL= bash run_backend.sh
```

#### 实现注册、分流与缓存前缀（Phase 4 Part 2）

`backend/loop_registry.go` 定义统一的 `LoopImpl(context.Context, ChatRequest, emit) error`，保留 HTTP 请求的取消和超时。注册 `v1` 与预算版 `v2`，默认仍为 `v1`，直接进入现有持久化单轮服务；SSE 和 JSON 都在共享入口 `processRequestContext` 中选择一次实现，错误仍走同一分类和结束事件。`LOOP_IMPL` 可通过环境或本地 .env 设置，已导出环境优先；空值/省略选择 v1，未注册名称也按阶段设计回退到 v1。启动日志及 `GET /health` 的 `loop_impl` 显示实际选择，便于发现回退。

`registerLoop` 为后端代码扩展点，拒绝空名、空实现和重复注册，读写锁保护注册与选择。生产 v2 已实现 Phase 5 的预算、摘要和压缩重试，由环境切换；无用户哈希分桶，测试假实现使用 test-v2。新增实现必须遵守同一单轮持久化/幂等、取消和完整工具批次提交边界；分流键不进入客户端协议或轮次指纹，既有 v1 成功轮保持回放兼容。实现切换不应改变已经提交的轮次；pending 重试期间保持实现和路由配置稳定，不能将这个钩子理解为自动跨版本数据迁移。

```bash
LOOP_IMPL=v1 LLM_PROVIDER=mock bash run_backend.sh
# v2 启用估算预算与压缩触发，health 显示 loop_impl=v2
LOOP_IMPL=v2 LLM_PROVIDER=mock bash run_backend.sh
```

本次缓存验收证明：相同 family、mode 和工具 Schema 下，`hash(system_text + tools_json)` 在不同工作区、不同 query/轮次、SSE/JSON 返回方式下相同；工具 JSON 使用实际上游请求中的序列化字节。工作区差异只进入首条 user 环境消息，追加轮次保留已有 messages 前缀。切换 family/mode 或改工具 Schema 会使静态段改变并产生不同诊断 hash，这是预期行为。

这个 hash 只用于本地测试诊断，不是供应商 cache key，也不表示整个 HTTP JSON 请求字节相同。单测和本机模拟上游均已验证前缀；模拟命中/未命中 usage 不影响总 token 聚合，成功轮回放不再请求上游或重复记账。**未调用真实模型 API，未测真实缓存命中率、延迟或费用**，也未实现显式 cacheHandle 透传或缓存命中指标落库。已有持久化轮次回放与供应商 prompt cache 是两种独立行为。

可单独复核本 Part 的 Go 专项（不需要真实模型密钥）：

```bash
cd backend
LOOP_IMPL=v1 MINICOCO_MODEL_IS_FUNCTION_CALL= MINICOCO_MODELS_FILE= MINICOCO_DEBUG_PROMPT=0 \
  go test -race -run 'TestLoop|TestPromptCache' -v .
```

#### flaky mock：确定性故障注入（Phase 1 Part 1）

```bash
# 终端 A：该进程前两次模型请求失败，之后恢复正常 mock
LLM_PROVIDER=mock LLM_MOCK_MODE=flaky LLM_MOCK_FLAKY_FAILS=2 bash run_backend.sh

# 终端 B：一次运行，自动从前两次瞬时故障恢复，完成 list_dir → 回答闭环
./run_client.sh "列出当前文件" --workspace .
```

`LLM_MOCK_MODE` 默认 `normal`；`flaky` 下 `LLM_MOCK_FLAKY_FAILS` 默认 2，接受非负整数。
失败预算在后端启动时初始化、每进程一份，并发时原子消耗；`/health` 不消耗预算，恢复后不会在下一轮重新注入失败。
默认模型的有效 provider 为 mock 时才初始化故障预算（可由 LLM_MODEL=mock 或 LLM_PROVIDER=mock 选择）；否则忽略这些 mock 设置。非法有效 mock 配置会在启动时失败。这两个参数也可通过本地 `.env` 配置，导出的环境变量优先。

客户端已支持类型化错误和可取消的自动重试：只有瞬时故障进入退避，fatal、用户取消和已成功轮不重试。
对瞬时故障，默认首次请求之外最多重试 7 次（8 次流式尝试），基础退避依次为 3/6/12/24/30/30/30 秒，并增加 0—25% 正向抖动。每次请求单独计算 125 秒超时；它不是整轮重试的总超时。
每次重试都会标记丢弃失败尝试；已显示的部分文本可能仍留在终端，但失败尝试保留 pending 供同 roundId 重试，但不进入已提交历史，工具执行不会重试。
仅瞬时故障重试耗尽后，再发**恰好一次非流式兜底**，失败即结束；fatal、用户取消和压缩失败不触发兜底。默认是 8 次流式尝试 + 最多 1 次 JSON 请求，尝试次数包含兜底。

同一 `POST /chat/stream` 的 `stream` 字段省略或为 true 时返回 SSE，为 false 时返回 `application/json` 的 `{events:[...]}`。
两种返回共用单轮编排、错误分类、完整工具批次验证和客户端 `consumeEvents`，取消贯穿请求与读取。
**本兜底改变客户端到后端的返回方式，上游模型仍复用原有流式管线**，用于下游传输恢复，不承诺修复所有上游流式故障。
JSON 返回同样限制为 4 MiB，单事件不超过 1 MiB，完整验证通过后才执行工具。

历史预算和预占/提交边界已迁到后端；客户端不再压缩或上传历史。LOOP_IMPL=v2 在后端主动/反应式压缩并内部重试，最多 3 次压缩/trim；不能缩短时 cannot_compress，不走 JSON 兜底。v1 不自动压缩，客户端没有历史可自行压缩。

错误 SSE 事件增加 `code`、`retryable`；`retryable:false` 会显式输出，随后仍发 `done(error)`：

```json
{"type":"error","message":"LLM HTTP 429: insufficient quota","code":"quota_exhausted","retryable":false}
{"type":"done","finish_reason":"error"}
```

| 故障 | code | retryable |
| --- | --- | --- |
| 上游 429 限流 | `rate_limited` | true |
| 上游 429 配额/余额耗尽 | `quota_exhausted` | false |
| 上游 400 上下文超长 | `need_compression` | true，需先压缩，不能直接重发 |
| 上游 5xx、408、425，连接错误或缺少流结束标记 | `upstream_5xx` | true |
| 请求或流读取超时 | `upstream_timeout` | true |
| 其他请求级 4xx | `bad_request` | false |
| 缺少密钥、非法端点/provider 等配置错误 | `config` | false |
| 协议/资源限制错误、未知内部错误、用户取消 | `internal` | false |

分类基于 HTTP 状态、已知配额/上下文错误信号和错误来源；单独出现 `quota` 一词不会把可恢复的请求速率限制判成余额耗尽。
Phase 5 已为 v2 实现摘要压缩；客户端提交协议/事件格式不变，后端处理内部上下文重试。

### 2) 接真实模型 (任意 OpenAI 兼容端点)

```bash
# 终端 A
# 使用本地 .env 的默认 DeepSeek Flash：bash run_backend.sh
# 或通过环境变量覆盖：
LLM_PROVIDER=openai \
LLM_BASE_URL=https://api.deepseek.com/v1 \
LLM_API_KEY=sk-xxxxxx \
LLM_MODEL=deepseek-flash \
./run_backend.sh

# 终端 B (交互式 REPL, 不带 query 即进入)
./run_client.sh --workspace /path/to/project
you> 把 calc.py 里的 bug 修掉, 并跑一下测试
```

接真实模型后, agent 会真的调用 `read_file` / `edit_file` / `run_command` 等工具改你的代码、跑你的命令。

**默认模型配置**：

- `LLM_BASE_URL=https://api.deepseek.com/v1`
- 默认 `LLM_MODEL=deepseek-flash`；实际可用模型由供应商和账号决定，上游错误会作为失败返回。
- 模型也可在客户端按次覆盖：`./run_client.sh "..." --model your-model`（请求体里的 `model` 优先于后端的 `LLM_MODEL` 默认值）。
- key 不要写进脚本/仓库：用 `export LLM_API_KEY=sk-xxx` 或放进被 `.gitignore` 忽略的 `.env`。
- 默认关闭 DeepSeek 思考模式，以适配当前仅回传文本和工具结果的客户端；请求超时为 120 秒。


### 3）工具自测（只测前端工具，不依赖后端/模型）

```bash
node client/src/selftest.ts
```

### 4）开发检查与回归测试

```bash
npm --prefix client ci
bash check.sh
```

`check.sh` 执行空模块缓存/禁用下载的默认依赖检查、Go 竞态测试、静态检查、构建，以及客户端类型检查、工具自测和回归测试。
回归测试使用临时工作区、假密钥和本机模拟模型，包含真实 Go 后端与 CLI 的
“读文件 → 编辑 → 执行测试 → 最终回答”四轮流程，不调用付费模型。

客户端 npm test 由 scripts/test.ts 为每个测试文件启动独立进程，再直接运行 Node 原生测试；保留文件级隔离，同时避开当前 Node 24.19.0 测试运行器的 V8/stdout 混合转发问题。测试失败仍会使统一入口返回非零。相关上游修复见 [Node #64706](https://github.com/nodejs/node/pull/64706)。

#### Phase 2：增量会话、持久化与恢复

后端缺省使用 `STORE_BACKEND=file`，默认目录相对进程 cwd 为 data/；run_backend.sh 在 backend/ 启动，所以默认是 backend/data/。可在环境或本地 .env 设置 MINICOCO_DATA_DIR 为绝对路径。文件档包含 meta.json 和 transcripts/<sid>.jsonl，两种默认 data/ 路径均已被 Git 忽略。**文件档一个目录只允许一个后端写入实例**，其协调器只在进程内生效。部署时须持久化存储；可选真实库档见下节。

每轮 POST /chat/stream 只接收以下增量，query 与 toolUseResults 必须二选一；旧 messages 请求被拒绝，客户端与后端需要一起升级：

```json
{"sessionId":"","roundId":"new-r-<random-uuid>","query":"检查工作区","workspace":"/absolute/workspace","model":"","stream":true}
```

- 首轮 sessionId 为空，后端从固定 roundId 派生会话 ID；每次已解析会话的响应都带 X-Session-Id。即使首轮响应头完全丢失，重复同 roundId 也只创建同一会话。CLI 在 stderr 输出 [session] <sid> 供保存。
- 同轮 roundId 和输入固定；新用户输入或工具结果轮使用新 UUID，避免 REPL、恢复进程的轮号碰撞。切换 SSE/JSON 或从响应头补全 sessionId 不改变轮次身份。同 roundId 更换输入会失败。
- 工具轮发送 toolUseResults: [{id,name,output,isError}]，必须完整匹配上一批工具的 ID 和名称，输出按后端 Go JSON 编码（包括 HTML/Unicode 分隔符转义）每项不超过 64 KiB，客户端工具也按同一口径截断。工作区绑定会话，不允许静默换目录。
- 一次原子替换写入输入与 pending，成功后追加相同消息 ID 的 final，并记录成功事件序列和 usage。Latest 不暴露失败轮输入/pending；重试复用预占，成功轮重复请求直接回放事件，不调用模型、不增加正文或用量。同会话并发请求返回 425，客户端按瞬时故障退避。
- token 来自供应商返回的 usage；未返回时为 0，不做估算。mock 每个成功轮使用合成的 1 prompt / 1 completion / 2 total 计数，仅供测试。元数据按 roundId 去重，并在请求/列表读取时从已提交 transcript 修复中断的回写；这些是成功轮统计，不代表供应商全部计费（失败请求也可能收费）。
- JSONL 逻辑追加通过整文件原子替换提交，适合当前小规模默认档；无跨文件事务，使用 transcript final 与元数据 round 账本进行恢复。协调器提供进程内 TTL 租约、固定窗口限流、显式 flags 优先于同名环境变量和单机广播空实现。摘要写入独立 summary.jsonl；带校验检查点的摘要由 Latest 合并，旧无检查点摘要仍仅保存。

自测与续会话：

```bash
# 终端 A：数据保留在同一绝对路径，可停止后再次启动
MINICOCO_DATA_DIR=/absolute/path/to/minicoco-data LLM_PROVIDER=mock bash run_backend.sh

# 终端 B：完成两轮 mock；保存 stderr 中的 [session] s_...
./run_client.sh "列出工作区文件" --workspace /absolute/workspace
./run_client.sh --list
./run_client.sh --resume s_REPLACE_WITH_REAL_ID "再列一次"
```

--list 使用 GET /sessions，列出本地匿名用户的会话、消息数和成功轮 token。--resume 通过 GET /sessions/<sid> 获取元数据，默认使用保存的 workspace/model，新的 query 由后端加载完整已提交历史；显式 --workspace 不匹配则拒绝。此处没有用户鉴权或多租户隔离。

恢复边界：同一进程会保留未确认的 roundId 和已执行工具结果，失败后重复相同 query 可继续提交，工具不会再执行。进程退出会丢失这份临时结果；如果服务端仍有未完成工具批次，--resume 会拒绝自动重放，因为无法判断副作用是否已发生。max-rounds 耗尽、工具取消后也可能留下该状态。当前不提供跨进程工具执行日志或副作用回滚；可新建会话并人工核对工作区。

存储/会话专项测试（在 backend/）：

```bash
go test -race -run 'Test(JSON|Inproc|Session)' -v ./...
go list -m all  # 默认仅 minicoco，无第三方模块
```

#### Phase 2 Part 3：可选真实数据库档

默认 `backend/go.mod` 保持 Go 1.22、零第三方模块。真实档的驱动锁定在独立的 `go.realdb.mod` / `go.realdb.sum`，要求 Go ≥ 1.26，所有第三方 import 都在 `//go:build realdb` 文件内。**必须同时指定 `-modfile` 和 `-tags`**，单独 `-tags realdb` 不提供依赖清单；不要用默认清单执行 `go mod tidy` 来安装可选驱动。

先准备可访问的 MongoDB 和 Redis 服务，再启动：

```bash
# 仓库根目录；数据库地址也可放在被忽略的 .env 中
STORE_BACKEND=real \
MINICOCO_DATA_DIR=/absolute/path/to/minicoco-real-data \
MONGO_URI=mongodb://127.0.0.1:27017 \
MONGO_DATABASE=minicoco \
REDIS_URL=redis://127.0.0.1:6379/0 \
REDIS_PREFIX=minicoco: \
LLM_PROVIDER=mock bash run_backend_realdb.sh

# 手动构建（在 backend/）；纯 Go，不需要 C 编译器
CGO_ENABLED=0 go build -modfile=go.realdb.mod -tags realdb -o /tmp/minicoco-realdb .
```

`run_backend_realdb.sh` 只启用驱动，存储选择仍由环境或 `.env` 的 `STORE_BACKEND` 控制，未设置时仍为 file；带驱动的二进制也可显式使用 file。默认二进制设置 real 会报出启用方式，不会静默降级。切换两档**不会自动迁移已有数据**。

| 接口 | 真实档实现 | 持久化与并发边界 |
| --- | --- | --- |
| MetaStore | SQLite，`MINICOCO_DATA_DIR/meta.sqlite` | WAL、参数化 SQL、用量账本和计数同事务提交；消息索引支持点赞 |
| TranscriptStore | MongoDB `auto_llm_message`、`auto_llm_summary` | 唯一 `(sessionId,roundId)`；每轮输入、pending/final、事件和 usage 存在同一原子文档；`auto_llm_sequence` 保证读取顺序 |
| Coordinator | Redis | SET NX + TTL、Lua CAS 解锁、固定窗口限流、GET 开关及 Pub/Sub inbox |

Mongo 以**一轮一个文档**实现多消息原子提交，避免原设计伪代码中多条消息竞争同一个唯一键。消息正文保存为 JSON 字节以保留字段形状和整数精度，轮次、状态、顺序和用量保留 BSON 字段；无需 Mongo 副本集事务。SQLite 与 Mongo 没有跨库事务，仍由 transcript final 和 SQLite 轮次账本恢复中断的元数据回写。

Redis 开关键为 `<REDIS_PREFIX>feature_toggle:<key>`，默认前缀 `minicoco:`；真实档从 Redis 读取，不使用文件档的环境变量回退。共享业务数据的实例必须使用相同 Mongo 数据库、Redis 前缀和同一 SQLite 数据文件；多进程 SQLite 应位于同一主机的本地磁盘。**这不是跨机器共享 SQLite 文件的部署方案**。

SSE 响应提供 `X-Emitter-Id`，本节点登记的 emitter 接收 `<REDIS_PREFIX>inbox` 事件。`Coordinator.Publish` 编码 `{emitterId,chunk}`，其中 chunk 是一个 Event JSON 的字节数组（外层编码为 base64）；所有订阅节点只向本地匹配 emitter 投递，并串行化本地与远端写入。发布方必须是可信后端，保证工具/完成事件已授权并持久化。Pub/Sub 是实时、至多一次投递，断连会丢事件，不能替代 transcript、任务队列或成功提交确认；目前模型请求仍在接收请求的节点运行。

真实服务验收需要显式测试地址，脚本不会把未连接服务的跳过结果当作验收通过：

```bash
MINICOCO_TEST_MONGO_URI=mongodb://127.0.0.1:27017 \
MINICOCO_TEST_REDIS_URL=redis://127.0.0.1:6379/0 bash check_realdb.sh
```

测试新建随机命名的数据库和 Redis 前缀并清理自身数据，SQLite 使用临时目录；建议使用专用测试服务。脚本运行两档共同接口用例、并发唯一键/用量测试、两个独立进程的锁竞争与 inbox 投递、真实 HTTP SSE 和 CLI 恢复/回放，随后执行 vet、无 cgo 构建并核验驱动确实进入产物。默认 `bash check.sh` 无需数据库；默认依赖也可单独运行 `bash check_default_deps.sh` 检查。

### 5）运行结果、取消与资源限制

- `Conversation.run()` 返回 `{status, text, rounds, message?}`，状态包括 `completed`、`failed`、`exhausted`、`cancelled`。CLI 仅为正常完成打印答案，失败或耗尽返回退出码 1，取消返回 130。
- `--max-rounds` 为 1—100 的整数，默认 25；最后一轮若仍要求工具则停止，不再执行该批工具。
- 运行中按 Ctrl+C 取消当前请求或命令；客户端断连也会取消后端模型请求。macOS/Linux 会终止命令进程组，Windows 目前只终止直接子进程。
- 后端已提交历史加当前轮最多 1 MiB JSON 或 512 条消息，达到预算后需新建会话；返回可执行工具批次前由后端预留结果预算，不自动删除历史中的工具调用。
- 单个文本文件读写最多 1 MiB；工具结果最多 64 KiB（同时限制 JSON 编码大小），截断时明确标记；命令输出超限则终止该命令并返回工具错误。
- `read_file` 每次最多返回 2000 行；`list_dir` 最多列出 2000 项。`grep` 最多遍历 2000 项、32 层、8 MiB 文件内容、返回 200 个匹配；跳过符号链接、过大文件、二进制内容和超过 4096 字符的行，并报告限制。
- 搜索在独立 Worker 中执行，5 秒超时并支持取消；命令超时 120 秒；模型超时 120 秒，客户端请求超时 125 秒。
- 后端请求体最多 2 MiB，SSE/JSON 响应最多 4 MiB，单个 SSE 帧/事件最多 1 MiB，每轮最多 16 个工具调用。无效参数、异常流、缺失结束标记和不一致的结束原因都会失败，工具调用在整批验证通过后才发送给客户端。

## 目录结构

```
MiniCoco/
├── backend/                      # Go：持久化会话与单轮模型调用
│   ├── main.go                   #   HTTP + SSE 入口 /chat/stream
│   ├── agentloop.go              #   单轮入口与错误事件
│   ├── sessions.go               #   历史、预占、模型调用、回写及成功回放
│   ├── session_http.go           #   会话列表和恢复元数据
│   ├── prompt.go                 #   genPrompt：系统提示词 + 工具清单（//go:embed 资源）
│   ├── tools.go                  #   工具注册、Schema 版本选择、稳定顺序和模式过滤
│   ├── toolresult.go             #   Native/XML 结果、身份匹配、顺序与格式预算
│   ├── llm.go                    #   provider 选择 + 流式 + tool_call 解析（openai / mock）
│   ├── protocol.go               #   SSE 事件类型 / 请求体
│   ├── store.go / store_files.go #   三存储接口、数据类型和原子文件写入
│   ├── metastore_json.go         #   JSON 会话元数据
│   ├── transcript_jsonl.go       #   JSONL 消息、预占/回写和摘要
│   ├── transcript_rounds.go      #   原子轮次预占、final 事件和用量检查点
│   ├── coord_inproc.go           #   进程内 TTL 锁、固定窗口限流与开关
│   ├── wire_stores.go / wire_*.go #   file/real 装配与构建档隔离
│   ├── *_realdb.go               #   SQLite / MongoDB / Redis 可选实现和测试
│   ├── emitter.go                #   本地 SSE registry 与 inbox 事件投递
│   ├── store_contract_test.go    #   两档复用的接口验收
│   ├── go.mod                    #   默认 Go 1.22、零第三方模块
│   ├── go.realdb.mod / .sum      #   真实档 Go 1.26、锁定驱动依赖
│   ├── loop_registry.go          #   同形状单轮实现注册、LOOP_IMPL 分流与 v1 回退
│   ├── loop_registry_test.go     #   双传输分流、取消、回放与配置验收
│   ├── prompt_cache_test.go      #   静态段 hash、上游字节、追加历史与模拟缓存验收
│   ├── models.go / models.json   #   模型路由、能力与配置校验（内嵌默认表）
│   ├── prompts/system_*.txt      #   openai / claude 模型族提示词（编译嵌入）
│   └── tools/*__vN.json          #   带 availableVersion 的工具资源，输出标准 function Schema
├── client/                       # ← 真实前端映射（前端：TS，Agent 循环 + 本地工具）
│   ├── package.json / tsconfig.json
│   └── src/
│       ├── cli.ts                #   入口（相当于 webview / IDE 端）
│       ├── conversation.ts       #   工具循环、sessionId 与未确认增量
│       ├── sessions.ts           #   列表/恢复元数据请求
│       ├── executor.ts           #   单轮：发请求 + 消费 SSE
│       ├── sse.ts                #   SSE 解析
│       ├── protocol.ts           #   与后端共用的事件类型
│       ├── toolMode.ts           #   客户端模式与执行范围校验
│       ├── selftest.ts           #   本地工具离线自测
│       └── tools/
│           ├── index.ts          #     createHandler 注册表
│           ├── base.ts           #     Handler 基类 + 工作区沙箱
│           └── readFile / writeFile / editFile / listDir / grep / runCommand
├── run_backend.sh / run_backend_realdb.sh / run_client.sh
└── check.sh / check_default_deps.sh / check_realdb.sh
```

## 关键取舍

- **历史放后端**：服务端从持久化 transcript 读取历史，客户端只提交增量；三个接口分离元数据、正文与协调。默认 JSON/JSONL/进程内实现便于离线运行，可选 SQLite/MongoDB/Redis 档保留同样的轮次和去重语义。
- **原生 function calling**：直接用模型的 `tools` / `tool_calls`。真实前后端设计 还支持 XML 风格工具协议、prompt cache、ABTest 分流等。
- **逐步完善实现**：已实现模型请求重试、一次性 JSON 兜底、会话持久化/恢复、可选真实库档、工具版本/模式、Native/XML 结果回填、按模型名路由/模型族提示词、单轮实现注册、静态前缀验收和可选 v2 上下文工程；尚未实现 XML 工具调用解析、按任务自动选模、tracing、权限审批、memory、subagent / MCP。使用明确的预算和失败状态控制基本边界。
- **工具集**：保留 6 个核心工具。新增工具时提供带版本/元数据的 JSON，更新后端 ToolName、固定顺序与模式策略，并在客户端增加 Handler、注册表和模式策略，补齐两端回归；仅放一个 JSON 不会自动开放执行。
- **语言**：真实后端是 Java；这里用 Go 保持「编译型、单二进制、无框架」的最小形态，前端沿用 TypeScript。

## 安全说明

- 后端默认监听 `127.0.0.1`，**无鉴权**，用于本地开发。`MINICOCO_HOST` 可以覆盖监听地址，当前实现不适合直接暴露到网络。
- 文件工具拒绝路径越界和路径中的符号链接，写入通过同目录临时文件替换完成。这是应用层路径检查，不能抵御其他进程并发恶意替换父目录。
- `run_command` 提供可信本地 shell 执行能力，`--workspace` 只设置它的工作目录，**不限制它访问工作区以外的文件或网络**。当前没有容器或操作系统级沙箱。


## Phase 5：上下文预算、压缩与摘要

- context_budget.go 以 UTF-16 字符/2 向上估算文本、工具调用/ID；图片 base64 估算仅是估算器能力，HTTP/存储仍为文本协议。maxTokens 扣除 system/tools/环境后为历史额度，>0.65 micro、>0.80 full；这是启发预算，不是供应商 tokenizer 或输出 max_tokens。
- LOOP_IMPL=v2 启用 compression.go，v1 保留不自动压缩的基线；两者均可读取已存在的摘要视图。主动预算与上游 need_compression 共用 compactContext，每个后端请求最多 3 次压缩/trim 尝试；新 query 保最后 user，工具轮保最后完整 Native/XML 批次。范围至少替换 3 条为两条摘要前缀，要求消息数和估算 token 都严格下降；缩不动或受保护工具块放不下明确 cannot_compress。
- micro 压最旧的小段，full 压尾段前历史。summarize 对真实兼容端点发无工具摘要请求，要求保留决策/路径/未决事项/结论；mock 用确定性占位，摘要不超过 8 KiB。摘要请求自身超限、摘要锁不可用或全局 rate:summary（1/10s）拒绝时只生成安全 trim 视图；取消贯穿请求，不提交不完整模型轮次。
- context_history.go / context_history_realdb.go：TranscriptStore 增加 RawLatest/Summaries，Latest 返回 user 占位+assistant 摘要+未压段。Summary 通过 PrefixCount 和 PrefixHash 校验不可变原始前缀；originStartId/EndId 是跨存储通用的逻辑消息序号。旧无 checkpoint 的摘要保持只保存、不自动合并。RawLatest 决定 pending stale、工具身份、恢复安全及 MessageCount，不能拿压缩后的长度当账本计数；原始正文与成功事件不删除，压缩不减少磁盘存储。
- 摘要保存成功后其真实 usage 以独立内部账本 ID 去重提交；reconcile 可恢复摘要已写但 meta 未回写的情况。NUL 前缀为内部摘要用量域，用户 roundId 拒绝 NUL，防止账本碰撞。失败供应商请求可能仍产生未统计费用。mock 摘要 usage=0，普通 mock 轮仍为 1/1/2。
- v2 对每次模型尝试暂存全部事件，成功持久化后统一发出，SSE 不再逐 token 实时显示；避免失败文本混入压缩重试结果。v1 保持原流式输出，客户端只提交原增量，不重跑工具；v1 收到 need_compression 时仍明确终止，客户端没有历史可自行压缩。

Part 1 已验收并推送 d3c0f5d，见 ChangeLog 004。Part 2 两档完整验收通过：默认零依赖、空模块缓存/禁用下载/无 cgo、Go -race/vet/构建、TS 类型检查、10 项工具自测及 52 项客户端测试（6 文件）通过，Go 覆盖率 84.6%；真实档 -race/vet/无 cgo/驱动包含检查通过，覆盖率 83.1%。新增 6 项默认 Go 顶层压缩专项（含子用例）、真实档共享压缩持久化/重开/摘要用量恢复专项、1 项长会话真实 CLI 重启恢复测试。临时独立端口 MongoDB/Redis 已停止；未调用真实模型 API。Part 2 发布见 ChangeLog 2026-10-26-005。

当前资料 Phase 1—5 共 17 个文件；Phase 1—5 现有 Part 已实施，Phase 6 未收到。未实现 OS 沙箱、跨进程工具日志/副作用 exactly-once、XML 工具调用解析、原生 Anthropic 协议或真实缓存收益验证。摘要是有损视图，语义质量尚无真实模型实测；默认文件档仍整文件重写，长期原始日志与摘要文件会增长，当前无物理日志清理。1 MiB/512 条限制应用于模型视图，已有工具结果/请求大小限制不变。

启用方式（从仓库根目录）：

```bash
LOOP_IMPL=v2 LLM_PROVIDER=mock bash run_backend.sh
# 另一个终端
./run_client.sh "检查工作区" --workspace /path/to/workspace
# 实际模型使用本地 .env 配置；依然设置 LOOP_IMPL=v2
```

无压缩时既有消息前缀只追加；v2 压缩会替换历史旧段，system/tools 静态段保持不变。摘要仅改变模型视图，不会删除原始 JSONL/Mongo 正文。

### 本地追踪（Phase 6）

响应头 `X-Trace-Id` 可定位 `data/logs/trace-<sid>.jsonl`；CLI 失败信息带 trace ID。日志只保存阶段、路由、工具名、错误分类和用量，不保存提示词、参数或输出。

```bash
./run_client.sh trace <sessionId>
./run_client.sh trace <sessionId> --round 3
```

`GET /health` 的 `recentSessions` 包含最近 10 会话的成功轮数、去重账本 token 和日志中的失败尝试数。日志异步且允许丢失，错误数可延迟；SIGINT/SIGTERM 尝试有界 flush，强杀不保证落盘。成功回放不会重复累计 usage。服务端读取单个 trace 文件限制 8 MiB，CLI 响应限制 4 MiB；当前不自动轮转。

### 工具审批与审计

交互终端执行 write_file / edit_file / run_command 前会询问 `[y/N]`。非交互管道默认拒绝这些工具；需要自动执行时显式传 `--yes`。只读工具免审批，`--mode readonly/safe` 仍限制工具范围。拒绝作为工具错误回给模型，循环继续。

审计由本地客户端异步发送到后端 `data/audit.jsonl`，只记录参数摘要及 approved/status，不记录参数正文。审计可能因队列满、网络或磁盘失败丢失，不是权限证明或重放日志。
