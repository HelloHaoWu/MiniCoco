# MiniCoco - 最小 Codig Agent Harness 实现(双端：Go后端 + TypeScript前端)

一个**可运行**的最小 coding agent，使用双端架构，以方便后端做独立服务扩展（"号池灵活路由"等功能）；

- **backend/**（Go）= 每轮无状态的「Prompt 组装 + LLM 流式代理」。它**不碰文件、不执行命令**，只负责：收到信息 -> 拼系统提示词 + 工具清单 -> 调模型 -> 把文本/`tool_call`通过SSE流式吐回。
- **client/**（TypeScript）= 拥有 **Agent 主循环**，并在**本地执行工具**（读写文件、跑命令、检索）。它发请求 -> 消费SSE -> 执行`tool_call` -> 带着结果再发下一轮，直到模型不再需要工具。

这正是该产品的核心设计的最小化落地：**循环在前端，后端是无状态的每轮代理**。前后端只靠 HTTP -> SSE 通信，各自零第三方依赖（后端仅 Go 标准库，前端仅 Node 内置模块）。

```
用户提问 -(client/TS)-> POST /chat/stream -(backend/Go)-> 拼 prompt+tools -> 调LLM
        ———————————— SSE: text / tool_call ————————————
(client) 本地执行 tool_call -> 收集结果 -> 带结果再 POST (下一轮)
        -> 循环, 直到某轮没有 tool_call (finish=stop) 结束
```

## 环境要求
- **Go** ≧ 1.22 (跑后端)
- **Node** ≧ 22.6 (跑前端: 用 Node 原生 TypeScript 支持, **无需编译**, 直接 `node src/cli.ts`)

## 运行

开两个终端

默认模型为 **DeepSeek Flash**（`deepseek-flash`）。后端启动时自动读取项目根目录 `.env`，
也支持在 `backend/` 下放 `.env`（优先读取）；已有环境变量优先于文件配置。
本机已配置密钥，直接运行 `bash run_backend.sh` 即可使用默认模型。
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

**DeepSeek 接入（已实测跑通）**：

- `LLM_BASE_URL=https://api.deepseek.com/v1`
- 可用 `LLM_MODEL`：`deepseek-flash`、`deepseek-v4-pro`（若传了不存在的名字，后端会把 API 的 400 原样透传为 `[backend error] ...`，并提示支持的名字）。
- 模型也可在客户端按次覆盖：`./run_client.sh "..." --model deepseek-v4-pro`（请求体里的 `model` 优先于后端的 `LLM_MODEL` 默认值）。
- key 不要写进脚本/仓库：用 `export LLM_API_KEY=sk-xxx` 或放进被 `.gitignore` 忽略的 `.env`。
- 默认关闭 DeepSeek 思考模式，以适配当前仅回传文本和工具结果的客户端；请求超时为 120 秒。


### 3）工具自测（只测前端工具，不依赖后端/模型）

```bash
node client/src/selftest.ts
```

## 目录结构

```
Mininum_Coco/
├── backend/                      # ← 真实后端映射（后端：Go，无状态每轮代理）
│   ├── main.go                   #   HTTP + SSE 入口 /chat/stream
│   ├── agentloop.go              #   processRequest：单轮编排（无 for 循环）
│   ├── prompt.go                 #   genPrompt：系统提示词 + 工具清单（//go:embed 资源）
│   ├── llm.go                    #   provider 选择 + 流式 + tool_call 解析（openai / mock）
│   ├── protocol.go               #   SSE 事件类型 / 请求体
│   ├── go.mod
│   ├── prompts/system.txt        #   主系统提示词（编译进二进制）
│   └── tools/*.json              #   工具 schema（OpenAI function 格式，编译进二进制）
├── client/                       # ← 真实前端映射（前端：TS，Agent 循环 + 本地工具）
│   ├── package.json / tsconfig.json
│   └── src/
│       ├── cli.ts                #   入口（相当于 webview / IDE 端）
│       ├── conversation.ts       #   Agent 主循环（真正的循环在这里）
│       ├── executor.ts           #   单轮：发请求 + 消费 SSE
│       ├── sse.ts                #   SSE 解析
│       ├── protocol.ts           #   与后端共用的事件类型
│       ├── selftest.ts           #   本地工具离线自测
│       └── tools/
│           ├── index.ts          #     createHandler 分发（switch）
│           ├── base.ts           #     Handler 基类 + 工作区沙箱
│           └── readFile / writeFile / editFile / listDir / grep / runCommand
├── run_backend.sh / run_client.sh
```

## 关键取舍

- **历史放客户端**：这里让 client 持有完整 `messages` 并每轮回传，后端因此**完全无状态**。复杂前后端设计把历史存在 MongoDB，client 只回传新 query + 工具结果。两者「后端按轮组 prompt」的本质一致。
- **原生 function calling**：直接用模型的 `tools` / `tool_calls`。真实前后端设计 还支持 XML 风格工具协议、prompt cache、ABTest 分流等。
- **砍掉的工程增强**：上下文压缩、重试退避、非流式兜底、Langfuse tracing、权限审批、RSS 保护、memory、多模型适配、subagent / MCP 等。这些正是真实代码膨胀到两万多行、而内核只有两三千行的原因。
- **工具集**：只保留 6 个最核心的（read/write/edit/list/grep/run_command）。加工具 = 在 `backend/tools/` 放一个 JSON + 在 `client/src/tools/` 写一个 Handler 并在 `createHandler` 注册。
- **语言**：真实后端是 Java；这里用 Go 保持「编译型、单二进制、无框架」的最小形态，前端沿用 TypeScript。

## 安全说明

- 后端只监听 `127.0.0.1`，**无鉴权**，是本地开发用的 harness，**切勿暴露到网络**。
- client 的工具会在你的机器上**读写文件、执行任意 shell 命令**（coding agent 的固有能力）。工具被限制在 `--workspace` 指定的目录内（`base.ts` 做了路径逃逸校验），`run_command` 有 120s 超时，但它仍可在该工作区内执行任意命令——请只在可信工作区、对可信模型使用。
