# MiniCoco - 最小 Coding Agent Harness 实现（Go 后端 + TypeScript 客户端）

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
- **Node** ≧ 24（客户端使用原生 TypeScript 类型擦除，无需编译）
- 首次进行开发检查：在 `client/` 执行 `npm ci`。TypeScript 和 Node 类型声明仅用于开发，运行时仍只使用内置模块。

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

#### flaky mock：确定性故障注入（Phase 1 Part 1）

```bash
# 终端 A：该进程前两次模型请求失败，之后恢复正常 mock
LLM_PROVIDER=mock LLM_MOCK_MODE=flaky LLM_MOCK_FLAKY_FAILS=2 bash run_backend.sh

# 终端 B：独立执行三次，前两次失败，第三次完成 list_dir → 回答闭环
./run_client.sh "列出当前文件" --workspace .
./run_client.sh "列出当前文件" --workspace .
./run_client.sh "列出当前文件" --workspace .
```

`LLM_MOCK_MODE` 默认 `normal`；`flaky` 下 `LLM_MOCK_FLAKY_FAILS` 默认 2，接受非负整数。
失败预算在后端启动时初始化、每进程一份，并发时原子消耗；`/health` 不消耗预算，恢复后不会在下一轮重新注入失败。
非法 mock 配置会在启动时失败，真实模型 provider 忽略这些 mock 设置。这两个参数也可通过本地 `.env` 配置，导出的环境变量优先。

本阶段只增加后端分类与故障源，**客户端仍把错误当作失败返回，不会自动重试**。
可取消退避和自动重试属于 Phase 1 Part 2，非流式兜底属于 Part 3，当前均未实现。

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
压缩仅提供反应式错误信号，本阶段没有实现压缩算法；旧客户端可忽略新增字段，其他事件格式不变。

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

`check.sh` 执行 Go 竞态测试、静态检查、构建，以及客户端类型检查、工具自测和回归测试。
回归测试使用临时工作区、假密钥和本机模拟模型，包含真实 Go 后端与 CLI 的
“读文件 → 编辑 → 执行测试 → 最终回答”四轮流程，不调用付费模型。

### 5）运行结果、取消与资源限制

- `Conversation.run()` 返回 `{status, text, rounds, message?}`，状态包括 `completed`、`failed`、`exhausted`、`cancelled`。CLI 仅为正常完成打印答案，失败或耗尽返回退出码 1，取消返回 130。
- `--max-rounds` 为 1—100 的整数，默认 25；最后一轮若仍要求工具则停止，不再执行该批工具。
- 运行中按 Ctrl+C 取消当前请求或命令；客户端断连也会取消后端模型请求。macOS/Linux 会终止命令进程组，Windows 目前只终止直接子进程。
- 会话最多 1 MiB JSON 或 512 条消息，达到预算后需新建会话；工具执行前预留结果预算，不自动删除历史中的工具调用。
- 单个文本文件读写最多 1 MiB；工具结果最多 64 KiB（同时限制 JSON 编码大小），截断时明确标记；命令输出超限则终止该命令并返回工具错误。
- `read_file` 每次最多返回 2000 行；`list_dir` 最多列出 2000 项。`grep` 最多遍历 2000 项、32 层、8 MiB 文件内容、返回 200 个匹配；跳过符号链接、过大文件、二进制内容和超过 4096 字符的行，并报告限制。
- 搜索在独立 Worker 中执行，5 秒超时并支持取消；命令超时 120 秒；模型超时 120 秒，客户端请求超时 125 秒。
- 后端请求体最多 2 MiB，响应流最多 4 MiB，单个 SSE 帧最多 1 MiB，每轮最多 16 个工具调用。无效参数、异常流、缺失结束标记和不一致的结束原因都会失败，工具调用在整批验证通过后才发送给客户端。

## 目录结构

```
MiniCoco/
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
│           ├── index.ts          #     createHandler 注册表
│           ├── base.ts           #     Handler 基类 + 工作区沙箱
│           └── readFile / writeFile / editFile / listDir / grep / runCommand
├── run_backend.sh / run_client.sh
```

## 关键取舍

- **历史放客户端**：这里让 client 持有完整 `messages` 并每轮回传，后端因此**完全无状态**。复杂前后端设计把历史存在 MongoDB，client 只回传新 query + 工具结果。两者「后端按轮组 prompt」的本质一致。
- **原生 function calling**：直接用模型的 `tools` / `tool_calls`。真实前后端设计 还支持 XML 风格工具协议、prompt cache、ABTest 分流等。
- **保持最小实现**：暂不引入上下文压缩、自动重试、非流式兜底、数据库、tracing、权限审批、memory、subagent / MCP。使用明确的预算和失败状态控制基本边界。
- **工具集**：只保留 6 个最核心的（read/write/edit/list/grep/run_command）。加工具 = 在 `backend/tools/` 放一个 JSON + 在 `client/src/tools/` 写一个 Handler 并在 `createHandler` 注册。
- **语言**：真实后端是 Java；这里用 Go 保持「编译型、单二进制、无框架」的最小形态，前端沿用 TypeScript。

## 安全说明

- 后端默认监听 `127.0.0.1`，**无鉴权**，用于本地开发。`MINICOCO_HOST` 可以覆盖监听地址，当前实现不适合直接暴露到网络。
- 文件工具拒绝路径越界和路径中的符号链接，写入通过同目录临时文件替换完成。这是应用层路径检查，不能抵御其他进程并发恶意替换父目录。
- `run_command` 提供可信本地 shell 执行能力，`--workspace` 只设置它的工作目录，**不限制它访问工作区以外的文件或网络**。当前没有容器或操作系统级沙箱。
