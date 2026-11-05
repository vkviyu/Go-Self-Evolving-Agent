# 自进化本地智能体架构设计

## Go 1.27.0、Eino ADK 与 WASM 能力插件

> 文档状态：Architecture Baseline v2（设计基线；已按 2026-10-26 代码核对，偏离处以“实际实现：…”标注）  
> 更新日期：2026-10-26（原基线 2026-08-27）  
> 目标形态：本地优先、单用户、可离线运行的 CLI/TUI Agent  
> 核心技术：Go 1.27.0、Eino ADK、wazero、WASI Preview 1

实现状态补充（2026-10-26）：受控工具生成、隔离编译/行为验证/审批发布、版本化策略 shadow/CAS 晋升/失败隔离、完成消费者归档和离线插件 GC 已落地（2026-09-26）；此后又落地了 Multi-Agent 控制面（durable Supervisor、Task DAG、mailbox 消费账本、快照隔离写入与 merge queue）、Goal Mode、Flow、LSP 诊断和 benchmark 评测协议。当前可执行边界和命令以 [EVOLUTION.md](./EVOLUTION.md)、[ARCHITECTURE.md](./ARCHITECTURE.md) 与 [MULTI_AGENT_ARCHITECTURE.md](./MULTI_AGENT_ARCHITECTURE.md) 为准；本文路线图中的任意 WASM Driver/宿主自重写、插件 Host Capability、MCP、Skills/AGENTS.md 不属于已实现能力。完整自进化闭环仍受宿主权限、人工发布与 Eino Loop 边界约束。

---

## 1. 结论与关键决策

本项目实现的是一个**本地可信 Agent Runtime**，不是从零发明 Agent Loop，也不是让模型随意重写宿主程序。

系统采用以下固定边界：

1. **Eino ADK 负责 Agent Loop**
   - 使用 Eino 的 `ChatModelAgent`、Runner、流式事件、Interrupt/Resume、Checkpoint 与 Middleware。
   - 稳定 Base Loop 编译进 Go 宿主，不由模型生成，不通过 WASM 执行。
   - Eino 是本地 Go 库，CLI 可以直接调用，不要求启动远程 Agent 服务。
   - 实际实现：仅使用 `ChatModelAgent`、`Runner`（流式 + `CheckPointStore`）、`WithCheckPointID`/`Resume` 与 `WithCancel`；未使用 Eino Middleware，审批也不经 Eino Interrupt（见 5.3、13.3）。

2. **Go 宿主负责可信控制面**
   - 管理会话、模型、流式输出、权限、审批、凭证、预算、审计和插件生命周期。
   - 所有有副作用的操作必须经过统一 Policy Engine。
   - 宿主状态通过 append-only 事件日志持久化，可以恢复、重放和分叉。

3. **wazero 负责动态 Tool 插件，不承载第一版完整 Loop**
   - Agent 可以生成 Go Tool 源码，使用固定的 Go 1.27.0 工具链编译为 WASM。
   - WASM 插件默认无文件系统、网络、环境变量和密钥访问权。
   - 插件只能通过显式声明并获批的 Host Capability 访问外部世界。
   - 实际实现：Host Capability 未实现；生成插件只能是 `RiskPure` 且 `Capabilities` 为空的纯 JSON 计算工具（`pluginbuild.Request.Validate`）。

4. **“自进化”采用候选版本晋升制**
   - 模型可以提出源码、测试和 `LoopSpec` 候选版本。
   - 候选版本必须经过静态检查、编译、契约测试、沙箱测试和权限评估。
   - 模型不能直接覆盖当前稳定版本，也不能自动修改 Trusted Runtime。

5. **热更新采用不可变 Revision + 原子注册表切换**
   - 不覆盖 `dynamic_latest.wasm`。
   - 每次构建生成内容寻址的不可变版本。
   - 新调用使用新版本，已经开始的调用继续使用旧版本；旧版本排空后再关闭。
   - 实际实现：已有 Snapshot 继续持有旧可执行体；尚无租约/排空跟踪（`plugin.Manager` 注释“A future lease tracker can drain them”），旧版本删除只走显式离线 GC。

6. **容灾目标是故障隔离与可恢复，不承诺“绝对可靠”**
   - WASM Trap、超时或内存超限不能击穿宿主进程。
   - 有副作用操作不能通过简单重跑实现回滚，必须依赖事件日志、幂等键和结果核对。
   - 模型不可用、磁盘损坏、操作系统故障等问题不在 WASM 回退可以解决的范围内。

### 1.1 当前实现状态（2026-10-26 核对）

基础纵切（2026-08-27）已经落地并由自动化测试验证：

- SQLite append-only Session Event Store、事件校验和与乐观序列；
- 默认拒绝 Policy Engine、不可变 Tool Registry Snapshot 和统一执行管线；
- Eino `ChatModelAgent` / `Runner` 的流式 ReAct 闭环，包含模型请求、Tool 调用和耐久事件；
- Eino Tool Adapter 保留模型 tool-call ID，并固定整个 Turn 的 Registry Snapshot；
- Go 1.27.0 WASI reactor SDK（`sdk/pluginabi`）、Host-owned Blob Handle ABI v1 和 wazero fresh-instance Runtime；
- Go 插件正常往返、跨调用 Handle 隔离、Host-owned Handle 防释放与无限循环超时测试；
- Plugin Manager 的 Stage、原子 Activate、旧 Snapshot 续用与显式 Rollback；
- OpenAI Chat Completions / Responses、Ollama ModelGateway，以及可恢复已完成 Turn 的 `local-agent chat`；
- OpenAI Responses SSE 的 reasoning summary、Token 用量和增量消息转换；
- Bubble Tea TUI、headless 流式 Markdown、Chroma 多语言高亮和审批交互；
- Coding Agent workspace 工具、macOS Seatbelt/Linux bubblewrap shell 边界，以及非 TTY 默认拒绝高风险调用；
- Eino Checkpoint Store、Turn Resume、上下文压缩和完整 ReAct 历史投影；
- 可运行的 `local-agent doctor` Composition Root 自检。

此后落地（截至 2026-10-26）：

- 沙箱化 Compiler Worker（`internal/pluginbuild`）、SQLite 持久化插件目录（`plugin_blobs`/`plugin_revisions`/`plugin_builds`/`plugin_test_reports`）、行为测试与确认发布、离线精确 GC；
- 版本化 LoopSpec（`internal/strategy`）、影子比较、CAS 发布与失败隔离回退；
- Multi-Agent：durable Supervisor（`internal/agentmanager`）、Task DAG、mailbox、快照隔离写入与 merge queue（`internal/workspacecoord`、`internal/workspacedomain`）；
- Goal Mode（`internal/agentgoal`）、宿主命名 Flow（`internal/agentflow`）、LSP 诊断（`internal/lsp`）、Team 预算与用量预留（`internal/tokenusage`）；
- benchmark 任务/结果协议与 `local-agent eval`（`internal/evaluation`，见 [evals/README.md](./evals/README.md)）。

仍未落地：WASM 插件的 FS/HTTP/Shell/KV/Secret Host Capability、MCP、Skills/AGENTS.md 注入、Plan Task、模型重试/Failover、Revision 排空租约、Full/Lite 工具链分发与 WASM Driver。当前实现明确把这些能力留在宿主控制面，不会让插件或模型绕过现有 Policy、Approval 和 Event Store。

---

## 2. 技术基线

### 2.1 固定版本

| 组件 | 基线版本 | 使用原则 |
| --- | --- | --- |
| Go | **go1.27.0** | 宿主和 Tool 编译器均使用该版本 |
| Eino | **v0.9.22** | 使用当前稳定版本，不采用 v0.10 alpha（基线原定 v0.9.14，`go.mod` 现为 v0.9.22） |
| wazero | **v1.12.0** | 使用稳定 API，启用编译执行模式 |
| WASM Target | `GOOS=wasip1 GOARCH=wasm` | 使用 WASI Preview 1 reactor |

`go.mod` 固定语言/最低工具链版本；开发机与发布流水线另外固定实际 Go 工具链为 go1.27.0：

```go
module github.com/vkviyu/go-self-evolving-agent

go 1.27.0
```

当 `toolchain` 与 `go` 行完全相同，Go 1.27 的 `go mod tidy` 会移除冗余的 `toolchain` 行，因此不依赖该冗余声明做版本锁定。GVM、CI 镜像和发行构建器必须显式选择 go1.27.0。

可复现构建中禁止使用 `@latest`。依赖升级必须通过显式变更、回归测试和基准测试。

### 2.2 Go 1.27.0 约束

- Go 1.27.0 于 2026-08-19 发布，是本设计更新时的最新正式版本。
- macOS 宿主最低支持版本为 macOS 13 Ventura。
- Tool 插件与宿主统一使用 Go 1.27.0，避免 ABI、标准库行为和构建结果漂移。
- 第一版不启用实验性的 `GOEXPERIMENT=simd`，保证生成插件的可移植性。
- Tool ABI 使用严格 JSON；可以在内部评估 `encoding/json/v2`，但协议行为必须通过固定测试定义，不能依赖错误文本等实现细节。

### 2.3 发布形式

提供两种发行包，但运行语义完全一致：

- **Full**：宿主二进制附带压缩后的官方 Go 1.27.0 工具链。
- **Lite**：首次需要生成插件时，从官方来源下载 Go 1.27.0，并校验平台对应的 SHA-256。

工具链安装后，Agent 的日常运行和已有 WASM Tool 执行均可离线完成。

实际实现：Full/Lite 发行包未实现（计划中）。编译器直接使用宿主进程自身的 `runtime.GOROOT()/bin/go`，要求 `go version go1.27.0`，并在构建证据中记录该可执行文件的 SHA-256；不附带、不下载工具链，也没有配置键可改。

---

## 3. 目标与非目标

### 3.1 第一阶段目标

- 本地 CLI/TUI 多轮对话和流式输出。
- 支持远程 OpenAI-compatible（Chat Completions / Responses）模型与本地 Ollama 模型。
- Eino Base Agent 支持工具调用、上下文压缩、计划任务、Skills 和人工审批。（实际实现：工具调用、自研上下文压缩和人工审批已落地；Plan Task、Skills 未实现，任务拆解由 durable `task_*` 工具与 Goal Mode 承担。）
- 所有会话可以在进程崩溃或用户中断后恢复。
- 原生 Tool 和 WASM Tool 通过相同 Tool Registry 暴露给 Eino。
- 手工编写和 Agent 生成的 Go Tool 均可编译、测试、热激活和回滚。
- Agent 对本地文件、Shell、网络和密钥的使用可审计、可限制、可审批。

### 3.2 暂不追求

- 不在第一版实现任意 WASM Agent Loop。
- 不允许 Agent 自动修改或重编译 Go 宿主。
- 不提供通用容器平台或完整 Linux 虚拟化。
- 不承诺任意副作用都能事务回滚。
- 不用 WASM 代替 Shell 沙箱；二者是不同的安全边界。
- 不把 Coze Studio、Coze Loop 或 DeepSeek Harness 作为运行时依赖。

---

## 4. 总体架构

```text
┌─────────────────────────────────────────────────────────────────────┐
│                          CLI / TUI                                  │
│  输入队列 / 流式渲染 / Ctrl+C / 审批交互 / Session 切换             │
└──────────────────────────────┬──────────────────────────────────────┘
                               │ Internal Events
┌──────────────────────────────▼──────────────────────────────────────┐
│                    Go Trusted Runtime                              │
│                                                                     │
│  ┌──────────────────────┐     ┌─────────────────────────────────┐  │
│  │ Eino Agent Runtime   │     │ Session & Checkpoint Store      │  │
│  │ Base Loop / Runner   │◄───►│ Append-only Events / Snapshots  │  │
│  └──────────┬───────────┘     └─────────────────────────────────┘  │
│             │                                                       │
│  ┌──────────▼───────────┐     ┌─────────────────────────────────┐  │
│  │ Tool Registry       │────►│ Policy & Approval Engine         │  │
│  │ Native / WASM / MCP │     │ Capabilities / Budget / Audit    │  │
│  └──────────┬───────────┘     └───────────────┬─────────────────┘  │
│             │                                  │                    │
│  ┌──────────▼───────────┐     ┌───────────────▼─────────────────┐  │
│  │ Plugin Manager      │     │ Host Capability Adapters         │  │
│  │ Revision / Activate │     │ FS / Shell / HTTP / KV / Secret  │  │
│  └──────────┬───────────┘     └─────────────────────────────────┘  │
└─────────────┼───────────────────────────────────────────────────────┘
              │
┌─────────────▼───────────────────────────────────────────────────────┐
│                     wazero WASM Runtime                             │
│  Fresh Instance / Memory Limit / Timeout / No Preopened FS          │
└─────────────▲───────────────────────────────────────────────────────┘
              │ verified candidate.wasm
┌─────────────┴───────────────────────────────────────────────────────┐
│                  Out-of-process Compiler Worker                     │
│  Go 1.27.0 / AST Policy / Offline Build / Resource Limits / Tests   │
└─────────────────────────────────────────────────────────────────────┘
```

实际实现与上图的对应与偏离：

- CLI/TUI = `cmd/local-agent` → `internal/cli`（`internal/tui`、`internal/render`）；Composition Root = `internal/app`（唯一允许构造 `sqlitestore.Open`、`workspacedomain.Open`、`wasm.NewRuntime`、`tool.NewPipeline` 的包，`strategy_shadow.go` 另可构造影子 Pipeline）。
- Eino Agent Runtime = `internal/agentruntime`（唯一导入 `eino/adk` 的包）+ `internal/conversation`（Session 串行化、Goal 循环）；Tool Registry/管线 = `internal/tool`，Eino 绑定 = `internal/einoadapter`。
- Session & Checkpoint Store = `internal/session` 协议 + `internal/session/sqlitestore`（唯一 SQLite owner，另有 `sessionregistry`、`workspacedomain` 两个独立库）；没有派生 Snapshot 缓存（见 6.2）。
- Policy & Approval = `internal/policy` + `internal/approval`；预算 = `internal/tokenusage`。
- Plugin Manager = `internal/plugin`，wazero = `internal/plugin/wasm`（唯一导入 wazero 的包），Compiler Worker = `internal/pluginbuild` 经 `internal/sandbox` 运行的 `go build` 子进程。
- “MCP”与“Host Capability Adapters（FS/Shell/HTTP/KV/Secret）”未实现；FS/Shell 只以原生 workspace 工具（`internal/codingtools`）形式提供给模型，不提供给 WASM。
- 图外新增：`internal/agentmanager`/`internal/orchestration`（Multi-Agent）、`internal/agentgoal`、`internal/agentflow`、`internal/strategy`、`internal/evolutiontools`、`internal/lsp`、`internal/evaluation`、`internal/toolbridge`、`internal/modelgateway`、`internal/modelcontext`。
- 依赖方向以 `internal/architecture/architecture_test.go` 为准：例如 `eino-ext` 仅限 `internal/modelgateway`，`database/sql` 仅限 `sqlitestore`/`sessionregistry`/`workspacedomain`，`internal/evolutiontools` 不得依赖 Eino，`sdk/pluginabi` 不依赖任何内部包。

### 4.1 可信计算基（TCB）

以下代码属于 Trusted Computing Base，不能由 Agent 自动替换：

- Eino Runner 适配层和 Base Agent 配置。
- Session Event Store 与 Checkpoint Store。
- Policy Engine、审批系统和凭证管理。
- Tool Registry 与 Plugin Manager。
- wazero 初始化、Host Capability 实现和 ABI 解码器。
- 编译流水线、源码检查器和晋升规则。
- Shell/进程沙箱与工作区边界实现。

TCB 可以通过正常软件版本升级更新，但不能作为运行时自进化目标。

### 4.2 数据面与控制面

- **数据面**：模型请求、Token 流、Tool 调用、Tool 结果和用户消息。
- **控制面**：权限、预算、插件版本、LoopSpec、会话状态、审批和审计。

WASM Tool 只能参与受控数据面，不能直接修改控制面。

---

## 5. Eino Agent Runtime

### 5.1 为什么使用 Eino

Eino 已经提供 Agent 开发所需的通用机制：

- ChatModel 与 Tool 抽象。
- ReAct 风格 `ChatModelAgent`。
- Runner 生命周期与流式事件。
- Interrupt/Resume 与 Checkpoint。
- 文件系统、Tool Search、Tool Reduction、Summarization、Plan Task、Skill、Agents.md 等 Middleware。
- Graph、Workflow、Supervisor、Plan-and-Execute 和 Deep Agent 等扩展方式。

本项目不复制这些机制，而是在 Eino 外围增加本地可信执行、持久化和 WASM Tool Runtime。（实际实现：当前只用到 ChatModel/Tool 抽象、`ChatModelAgent`、Runner、流式事件和 Checkpoint；上列 Middleware、Graph/Workflow/Supervisor 均未使用，Multi-Agent Supervisor 与 Flow 是项目自研。）

### 5.2 Base Agent

第一版使用 `ChatModelAgent` 作为内置 Base Agent。实际装配在 `internal/agentruntime/eino_turn.go`（每个 Turn 构造一次）：

```go
agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
    Name:          f.config.Name,
    Description:   f.config.Description,
    Instruction:   f.runtimeInstruction(), // 配置指令 + Goal 协议
    Model:         recordedModel,          // 记录 model.requested / 用量的包装
    MaxIterations: f.config.MaxIterations,
    ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{
        Tools:               einoTools,    // 来自本 Turn 固定的 Registry Snapshot
        ExecuteSequentially: true,
    }},
})
if err != nil {
    return nil, err
}

runner := adk.NewRunner(ctx, adk.RunnerConfig{
    Agent:           agent,
    EnableStreaming: true,
    CheckPointStore: f.checkpoints,
})
```

以上代码基于固定的 Eino v0.9.22 API，并通过编译测试。

### 5.3 Middleware 顺序

推荐的逻辑顺序如下：

1. 修复或拒绝不完整 Tool Call 历史。
2. 注入项目级 `AGENTS.md` 与用户规则。
3. 上下文预算计算与 Summarization。
4. 大 Tool Result 截断或外置。
5. Tool Search 与动态 Tool 可见性。
6. Skill 渐进式披露。
7. Plan Task。
8. Policy、审计与指标 Middleware。

不得直接把 Eino 的本地文件系统或 Shell Backend 暴露给模型。它们必须由本项目的 Policy-aware Backend 包装，以确保所有路径和命令经过同一权限系统。

实际实现：未使用任何 Eino Middleware，上述顺序中只有第 1、3、4、8 项以自研方式落地，其余（AGENTS.md/用户规则注入、Tool Search、Skill、Plan Task）未实现：

- 不完整 Tool Call 历史：由历史投影与压缩时“工具调用/结果成组保留”处理（`compaction_transcript.go`）。
- 上下文预算与压缩：`context_policy.go` 的 `planContext` 在 token ≥ 窗口 90% 时为 critical，≥ 触发线（或 Team 预算 ≥80% 的预算压力）时为 elevated；`/compress` 强制、token ≥ 触发线、或预算压力且 token > 窗口/2 时压缩。较旧前缀由模型摘要为 `Conversation summary:` 系统消息并保留尾部（`retainTokens = min(window/4, trigger/3)`，critical 时 `min(window/6, trigger/4)`），critical 时或摘要格式经一次修复仍无效时退回抽取式摘要，并写入 `context.compacted` 事件。`request_context.go` 在 Eino 同一轮工具循环的每次模型派发前复查。
- 窗口与触发线：`internal/modelcontext.Resolve` 合并 provider 元数据、目录、配置和 32768 fallback；输出预留默认 `min(8192, window/4)`，输入上限扣除安全余量 `min(4096, window/20)`，默认触发线约为工作集 80%。配置键为 `[agent] context_window_tokens`、`max_output_tokens`、`compaction_trigger_tokens`（0 = 自动）。
- 大 Tool Result 外置：超过 24 KiB 的工具结果写入内容寻址 artifact store，模型只看到 2 KiB 预览和引用，可用 `artifact_read` 读回。
- Policy/审计不是 Middleware，而是 `tool.Pipeline` 内的固定阶段（见 7.1）。

### 5.4 模型接入

`ModelGateway` 统一承载：

- OpenAI Chat Completions / Responses API。
- Ollama 或其他本地模型服务。
- 流式响应规范化。
- 超时、退避重试和 Provider Failover。
- Token 与费用统计。
- 模型能力声明，例如 Tool Calling、Vision 和 Context Window。
- 敏感凭证注入。

Eino 只看到标准 ChatModel 接口，不直接管理真实 API Key。

实际实现（`internal/modelgateway`）：provider 只有 `openai`（`wire_api` 为 `chat-completions`，经 eino-ext openai；或 `responses`，自研 Responses SSE 客户端）和 `ollama`（默认 `127.0.0.1:11434`）。有请求超时（`[model] timeout`，默认 2m），**没有**退避重试和 Provider Failover。Token 统计与费用记账在 `internal/tokenusage`（只对显式配置的价格计费，支持 Team 预算与预留）。能力声明只覆盖上下文窗口/最大输出：OpenAI 读 `/models`，Ollama 读已加载模型的 `/api/ps`。凭证只以环境变量名配置（`[model] api_key_environment`，默认 `OPENAI_API_KEY`），在 Gateway 内解析，含凭证的 base URL 会被拒绝。

### 5.5 Runner 隔离

- 每个 Session 拥有独立的 Runner State。
- 每个 Turn 固定一个 `RuntimeSnapshot`，其中包含模型、Tool Revision、LoopSpec 和权限版本。
- Turn 执行期间配置发生变化时，不修改正在运行的 Snapshot。
- 新配置从下一个 Turn 生效。

这条规则避免热更新导致同一 Turn 前后使用不同 Tool Schema 或行为。

实际实现：`RuntimeSnapshot` ID 是 Tool Registry Snapshot 的内容地址 `registry-sha256:…`（覆盖宿主可执行文件摘要与每个工具的 Descriptor/实现身份），持久化在 `runtime_snapshots`，并以 `{Kind:"turn", Scope:session, ID:turnID}` 引用保留，写入每条事件的 `runtime_snapshot_id`。模型配置与 LoopSpec 不在其中：模型/提示/额度由单独的 execution config ID 固定，LoopSpec 在 `turn.started` 中记录 `loop_spec_id` 与 `base_execution_config_id`。权限规则编译进宿主，没有独立版本号。

---

## 6. 会话、事件溯源与恢复

### 6.1 Append-only Session Log

所有能够影响恢复结果的事实都写入 append-only 日志。每条事件至少包含：

```text
session_id
sequence
event_id
event_type
timestamp
causation_id
correlation_id
runtime_snapshot_id
payload_or_blob_ref
schema_version
checksum
```

关键事件类型（实际常量见 `internal/session/event.go`）：

```text
session.created
turn.started
user.message
background.input        # 实际新增：Agent 消息作为不可信 user-role 输入
model.requested
model.failed            # 实际新增
assistant.message
tool.proposed
policy.decided
approval.requested
approval.resolved
tool.started
tool.succeeded
tool.failed
tool.outcome_unknown    # 常量已定义，但当前没有生产代码写入该事件（见 6.3）
checkpoint.created
context.compacted       # 设计名 context.summarized；实际名为 context.compacted
context.managed         # 实际新增：上下文管理动作（级别、外置 artifact、压缩前后 token）审计
turn.completed
turn.failed
turn.abandoned          # 实际新增：显式放弃中断 Turn
goal.abandoned          # 实际新增
turn.archived           # 实际新增：maintenance archive 关闭恢复引用
```

流式 Token Chunk 默认只进入短期 Trace；最终消息和影响恢复的边界事件必须持久化。

插件激活、LoopSpec 激活、全局策略和工具链变更属于全局 `control_events`，不写入某一个 Session。Session 通过 `runtime_snapshot_id` 引用当时已经生效的控制面版本。

实际实现：没有 `control_events` 表。插件激活/退役的事实是 `plugin_revisions` 的状态行（离线 GC 另写 `plugin_revision_tombstones` 与 `maintenance_receipts`）；策略发布/回退写入 `strategy_events`（带序号和校验和，事件如 `strategy.promoted`、`strategy.fallback`）；Multi-Agent 控制面写入 `orchestration_events`（全局序号 + stream 序号）。Session 仍通过 `runtime_snapshot_id` 引用 `runtime_snapshots`。

### 6.2 Snapshot 与重放

- Snapshot 是事件日志的派生缓存，不是事实来源。
- 每 N 个事件或每个 Turn 结束时生成 Snapshot。
- Snapshot 损坏时可以从事件日志重建。
- 大型 Tool Result 和文件产物存入内容寻址 Blob Store，事件只保存 Hash 引用。
- Context Summarization 生成新事件，但不删除原始消息。

实际实现：没有 Session 状态派生 Snapshot（无 `session_snapshots` 表），历史每次由事件日志投影（`internal/conversation/history.go`、`agentruntime/history.go`）；恢复点只有 Eino 不透明 checkpoint（`agent_checkpoints`）。内容寻址 Blob Store 为 `internal/artifact`（`<data_dir>/artifacts/sha256/<hex>`，单对象 ≤32 MiB），用于大 Tool Result、补丁和转录；插件 WASM 字节另存于 SQLite `plugin_blobs`。`context.compacted` 只追加摘要，不删除原始事件。

### 6.3 副作用状态机

有副作用的工具必须遵循以下状态机：

```text
Proposed
   │
   ▼
PolicyDecided ──denied──► Failed
   │ allowed
   ▼
Started ──► Succeeded
   │
   ├─────► Failed
   └─────► OutcomeUnknown
```

`OutcomeUnknown` 表示操作可能已经发生，但宿主在记录结果前崩溃。此状态禁止自动重试，必须由工具执行器执行 reconciliation，或者向用户确认。

每次副作用调用包含稳定的幂等键：

```text
idempotency_key = hash(session_id, tool_call_id, tool_revision, normalized_args)
```

实际实现：状态机由 `tool.Pipeline` 写入的事件体现（`tool.proposed → policy.decided → [approval.requested → approval.resolved] → tool.started → tool.succeeded | tool.failed`）；前置条件（TurnGuard）失败、策略拒绝、无审批器和审批拒绝都写 `tool.failed`。`OutcomeUnknown` 不写独立事件、也不由执行器自动 reconcile：成功事件无法持久化，或重放时发现某调用已 `tool.started` 但无成功记录且风险不是 pure/read，均返回致命的 `tool.OutcomeUnknownError`（“inspect actual state before retrying; automatic replay is refused”），交由用户核对。没有通用的哈希幂等键公式：同一 Turn 内以模型 tool-call ID（缺省时随机 16 字节）为键，`replaySuccess` 在工具/revision/参数一致时返回已记录结果，否则报 `ErrCallConflict`；个别领域另有显式键（如 workspace lease 的 `lease:<callID>`、子任务派发 `task-dispatch:…`）。

### 6.4 回退语义

动态策略或 Tool 失败时：

1. 持久化失败事件。
2. 标记当前调用结果，不删除历史。
3. 从最后一个安全 Checkpoint 恢复 Base Agent。
4. 将已成功 Tool Result 重新提供给 Base Agent。
5. 不重新执行已提交的副作用。

因此，“回退”是恢复控制流，而不是假装物理世界已经回滚。

实际实现：Tool 执行错误、审批拒绝和前置条件错误作为结构化 Tool 结果返回模型，由同一 Eino Loop 自行修正；致命错误使 Turn 失败（`turn.failed`），中断的 Turn 可经 checkpoint `Resume`（`chat --resume-turn`）恢复，或经 `chat --abandon-turn/--abandon-goal … --confirm-abandon` 显式关闭。策略候选的 Turn 失败（非用户取消）只把后续 Turn 退回宿主基线并隔离该候选，不重放请求、不恢复文件。

### 6.5 存储实现

第一版使用嵌入式 SQLite 数据库和内容寻址文件 Blob Store，不依赖外部数据库服务。为保持跨平台与无 CGO 构建，选择经过验证的纯 Go SQLite Driver，并在 `go.mod` 中固定版本。

核心表包括：

```text
session_events
control_events
session_snapshots
runtime_snapshots
tool_registry
plugin_revisions
plugin_builds
approvals
loop_specs
```

实际实现：`internal/session/sqlitestore` 的 forward-only 迁移 v1–v36（`schema.go`；账本 `schema_migrations` 逐版本事务应用并校验 checksum）。与上表对应：`session_events`、`runtime_snapshots`、`plugin_revisions`、`plugin_builds` 存在；`control_events` 不存在（Multi-Agent 用 `orchestration_events`，策略用 `strategy_events`）；`session_snapshots` 不存在（Eino checkpoint 在 `agent_checkpoints`）；`tool_registry` 不存在（Registry 在内存，身份经 `runtime_snapshots` 持久化）；`approvals` 不存在（只有 `approval.*` 事件）；`loop_specs` 的对应物是 `strategy_specs/suites/reports/active/events/quarantine`。另有 `runtime_snapshot_refs`、`runtime_consumer_closures`、`recovery_abandonments`、`plugin_blobs`、`plugin_test_reports`、`plugin_revision_tombstones`、`maintenance_receipts`，以及 Multi-Agent/Goal/Flow/用量/LSP 表（`teams`、`agents`、`tasks`、`mailbox_messages`、`workspace_leases`、`merge_queue`、`goal_runs`、`flow_runs`、`usage_records`、`team_budgets`、`lsp_diagnostic_projections` 等）。另有两个独立 SQLite 库：`<GSEA_HOME>/control.db`（`internal/sessionregistry`，`agent_sessions` 表）与 `<GSEA_HOME>/workspaces/<identity>/coord.db`（`internal/workspacedomain`，`meta/instances/leases`）。

写入规则：

- 由单写入器串行分配 Session Sequence，避免同一 Session 事件乱序。
- Session Event 与对应 Session 派生状态在同一事务中提交；Active Revision 映射与对应 Control Event 在另一类原子事务中提交。
- 大 Blob 先写临时文件、同步并原子重命名，再允许事件引用其 Hash。
- 数据库启动时执行迁移、完整性检查和未完成副作用扫描。
- `tool_registry` 等表是快速查询视图；重要状态变化仍必须留下不可变审计事实。
- Blob 垃圾回收使用可恢复的 mark-and-sweep，不能根据文件年龄直接删除。

实际实现：单写入器为 `SetMaxOpenConns(1)` + WAL + `busy_timeout`，`Append` 以期望序号乐观检查（冲突返回 `session.ErrSequenceConflict`），进程独占由 `<db>.owner.lock` 文件锁保证；artifact 按临时文件 + Sync + Rename 写入 `<data>/artifacts/sha256/<hex>`。启动时执行迁移与迁移 checksum 核对、读事件时校验事件 checksum、扫描可恢复 Turn；`PRAGMA integrity_check` 只在 `maintenance compact` 中执行。没有 artifact mark-and-sweep；只有 `plugins gc-plan/collect` 对单个明确插件 revision 做离线确认式回收（写 tombstone 与回执），`maintenance archive` 只关闭恢复引用、不删历史。

---

## 7. Tool Registry 与统一执行管线

### 7.1 Tool 类型

Tool Registry 对 Eino 暴露统一 Tool 抽象，底层可以是：

- `NativeTool`：可信 Go 内置工具。
- `WasmTool`：由 wazero 执行的动态插件。
- `MCPTool`：后续可选的外部 MCP 工具。
- `AgentTool`：由 Eino Agent 包装的子 Agent。

实际实现：执行器均实现 `tool.Executor`，包括原生 Go 工具（`tool.InvokeFunc`，如 `internal/codingtools`、`evolutiontools`、`agentmanager/tools.go`）、`plugin/wasm` 执行的 WASM 插件、评测用 `internal/toolbridge`（Unix socket 转发外部 benchmark 工具），以及 `internal/agentflow`（Flow 调用）。`MCPTool` 未实现。子 Agent 不是包装成单个 AgentTool，而是由 `agent_*`/`task_*` 等控制工具交给 `agentmanager` Supervisor 调度（见 MULTI_AGENT_ARCHITECTURE.md）。

所有类型都必须经过相同执行管线：

```text
Schema Validate
→ Normalize Arguments
→ Resolve Tool Revision
→ Policy Decision
→ Optional Human Approval
→ Persist tool.started
→ Execute
→ Validate Result
→ Persist terminal event
→ Return to Eino
```

实际实现：`tool.Pipeline.Execute` 的顺序为参数/调用校验 → 解析 Snapshot 中的 Binding → guard 检查 → 按 CallID 加锁并尝试 `replaySuccess` → `tool.proposed` → TurnGuard 前置条件 → `policy.decided` → 可选 `approval.requested/resolved` → `tool.started` → Invoke → `tool.succeeded`。没有独立的“参数规范化”与“结果 Schema 校验”步骤：参数与结果只校验为合法 JSON（`NormalizedArg` 为原参数副本），`InputSchema` 仅经 `einoadapter` 转为 Eino ToolInfo 向模型声明，具体字段由各工具自行解码校验。

### 7.2 Registry Snapshot

```go
type ToolBinding struct {
    Name         string
    RevisionHash string
    SchemaHash   string
    ExecutorKind string
    RiskClass    string
}

type ToolRegistrySnapshot struct {
    ID       string
    Bindings map[string]ToolBinding
}
```

模型看到的 Tool Schema 与最终执行的 Revision 必须来自同一个 Snapshot。

实际实现（`internal/tool/registry.go`、`snapshot_manifest.go`）：

```go
type Descriptor struct {
    Name, Description string
    InputSchema       json.RawMessage
    Revision          string
    RiskClass         RiskClass // pure | read | write | execute | secret
    Capabilities      []string
}
type Binding struct {
    Descriptor       Descriptor
    Executor         Executor // 不序列化
    ImplementationID string
}
type SnapshotManifest struct { Version int; Host string; Tools []SnapshotEntry } // Entry = Descriptor + Implementation
```

`Snapshot` 不可变，持有进程内 `Generation()` 与按名 Binding；持久身份 `ID()` 为 `registry-sha256:<hex>`，对含宿主可执行文件哈希与全部规范化 Descriptor/ImplementationID 的清单求哈希，并写入 `runtime_snapshots`。没有单独的 SchemaHash/ExecutorKind 字段；恢复时按清单逐项 `Matches` 当前 Binding，不匹配即拒绝。

### 7.3 Tool Result Envelope

成功结果：

```json
{
  "ok": true,
  "output": {},
  "artifacts": [
    {"sha256": "...", "mediaType": "text/plain", "name": "result.txt"}
  ],
  "metrics": {"durationMs": 12}
}
```

失败结果：

```json
{
  "ok": false,
  "error": {
    "code": "permission_denied",
    "message": "workspace write permission was not granted",
    "retryable": false
  }
}
```

错误码是协议的一部分；Agent 不应通过匹配人类可读错误文本控制逻辑。

实际实现：未采用 `ok/output/artifacts/metrics` 信封（`internal/einoadapter/tool.go`）。成功时把执行器返回的 JSON 原样交给模型；可恢复失败统一为 `{"error": ..., "message": ...}`：审批拒绝为 `{"error":"tool call approval denied",...}`，TurnGuard 前置条件失败的 `error` 为稳定错误码（`PreconditionError.Code`），执行失败（`ExecutionError`）的 `error` 为 `invoke <tool>@<revision>: <原因>` 文本，没有 `retryable` 字段。策略拒绝、无审批器、`OutcomeUnknownError` 等其余错误不返回模型，而是使 Turn 失败。大结果的外置（>24 KiB 存 artifact、仅留预览）发生在上下文管理层而非 Tool 信封。

---

## 8. WASM Tool 插件模型

### 8.1 插件边界

第一版 WASM 插件必须满足：

- 面向单次 Tool 调用。
- 默认无状态。
- 不直接调用 LLM。
- 不持有 API Key。
- 不拥有裸文件系统、Shell 或网络能力。
- 不创建跨调用后台任务。
- 不依赖调用结束后仍存活的 Go goroutine。

需要 LLM 的能力应建模为 Agent 或宿主工作流，而不是普通 Tool。

### 8.2 Manifest

每个 Revision 都包含经过宿主规范化的 `manifest.json`：

```json
{
  "schemaVersion": 1,
  "id": "local.weather",
  "version": "0.1.0",
  "abi": "local-agent.tool.v1",
  "goToolchain": "go1.27.0",
  "entry": "tool.wasm",
  "stateful": false,
  "tool": {
    "name": "weather",
    "description": "Read weather data for one location.",
    "inputSchema": {
      "type": "object",
      "properties": {
        "location": {"type": "string", "maxLength": 200}
      },
      "required": ["location"],
      "additionalProperties": false
    }
  },
  "capabilities": [
    {
      "name": "net.http",
      "constraints": {
        "methods": ["GET"],
        "hosts": ["api.example.com"],
        "maxResponseBytes": 1048576
      }
    }
  ],
  "limits": {
    "timeoutMs": 10000,
    "memoryMiB": 64,
    "requestBytes": 1048576,
    "responseBytes": 4194304
  },
  "provenance": {
    "sourceSha256": "...",
    "wasmSha256": "...",
    "builder": "local-agent/go1.27.0"
  }
}
```

Manifest 中的权限是申请，不是授权。最终授权由 Policy Engine 根据插件 Hash、用户配置和本次调用共同决定。

实际实现：没有 `manifest.json` 文件，上面的 JSON 是设计形态。代码中的 Manifest 为 `plugin.Manifest{PluginID, Tool tool.Descriptor, Timeout}`（`internal/plugin/manager.go`），持久记录为 `plugin.RevisionRecord{Version, Manifest, ContentSHA256, RuntimeIdentity}`，存于 SQLite `plugin_revisions`，WASM 字节存于 `plugin_blobs`。`RuntimeIdentity` 记录 ABI 与资源上限（形如 `wasm-abi1:memory:…:blob:…:call:…:timeout:…`）。没有 `capabilities`、`limits.memoryMiB/requestBytes/responseBytes`、`stateful`、`goToolchain` 字段；构建溯源（toolchain/SDK/source/artifact SHA-256）记录在 `plugin_builds`（见 11.4）。

### 8.3 Capability 原则

Host Capability 使用最小权限设计：

| Capability | 示例约束 |
| --- | --- |
| `fs.read` | 工作区根目录、Glob、最大字节数 |
| `fs.write` | 指定路径范围、最大文件大小、是否要求审批 |
| `net.http` | Method、Host、端口、响应大小、重定向策略 |
| `process.exec` | 命令白名单、工作目录、超时、环境变量白名单 |
| `kv.read/write` | 插件命名空间和配额 |
| `clock.now` | 是否允许真实时间 |
| `random.secure` | 最大读取字节数 |

以下数据永不直接暴露给 WASM：

- API Key 原文。
- 宿主完整环境变量。
- 用户 Home 目录句柄。
- 未经授权的网络 Socket。
- Policy Engine 内部状态。

实际实现：Host Capability 未实现（计划中）。宿主只提供 Blob 读写函数，没有 `host_cap_call`；模型构建路径（`internal/pluginbuild`）要求 `RiskClass` 为 `pure` 且 `Capabilities` 为空，因此插件只能做纯计算。上表各能力均未提供。

### 8.4 WASI 配置

- 实例化 WASI Preview 1 以支持 Go reactor 初始化。
- 默认不配置任何 preopened directory。
- 默认不继承宿主参数和环境变量。
- 默认不提供网络 Socket。
- 时间和随机能力按运行需求提供，但不得被误认为业务授权。

WASM 是软件沙箱与能力边界，不称为“硬件级隔离”。

实际实现：`wasi_snapshot_preview1` 以默认 `ModuleConfig` 提供，无 preopen、无参数和环境变量；未显式配置墙钟或随机源（使用 wazero 默认）。导入模块只允许 `wasi_snapshot_preview1` 与 `local_agent_host_v1`，禁止导入内存。

---

## 9. Tool ABI v1

### 9.1 构建模式

Go 1.27.0 Tool 使用 WASI reactor：

```bash
GOOS=wasip1 \
GOARCH=wasm \
CGO_ENABLED=0 \
go build \
  -buildmode=c-shared \
  -trimpath \
  -buildvcs=false \
  -o candidate.wasm \
  ./cmd/plugin
```

宿主实例化模块后，必须先调用 `_initialize`，再调用业务导出函数。

实际实现（`internal/pluginbuild/compiler.go`）：在临时目录 `gsea-candidate-*` 中执行 `go build -mod=readonly -trimpath -buildvcs=false -buildmode=c-shared -o candidate.wasm .`（构建目标是模块根 `.`，不是 `./cmd/plugin`），环境变量另含 `GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0`。宿主以 `WithStartFunctions()` 实例化，再调用 `_initialize`（若导出）、`plugin_abi_version`、`plugin_invoke`。

### 9.2 不跨调用返回 Guest Pointer

禁止使用以下模式：

```go
ptr := uintptr(unsafe.Pointer(&[]byte(result)[0]))
return uint64(ptr)<<32 | uint64(len(result))
```

原因包括：

- 返回值指向临时对象，函数返回后不再具备可靠生命周期。
- Go WASM Guest Pointer 与 Host Linear Memory Offset 存在宽度和布局差异。
- `memory.grow` 后宿主先前读取的内存视图可能失效。
- 错误路径、空结果和并发调用难以定义。

### 9.3 Host-owned Blob Handle

ABI v1 使用调用作用域内的 Host-owned Blob Handle。所有跨边界长期存活的数据都先复制到宿主。

Guest 导出：

```text
plugin_abi_version() -> u32
plugin_invoke(call_id: u64, request_blob: u64) -> response_blob: u64
```

Guest 导入：

```text
host_blob_len(handle: u64) -> packed(status: u32, len: u32)
host_blob_read(handle: u64, offset: u32, dst_ptr: u32, dst_len: u32) -> u32
host_blob_put(src_ptr: u32, src_len: u32) -> u64
host_blob_drop(handle: u64)
host_cap_call(capability_name_blob: u64, request_blob: u64) -> response_blob: u64
host_emit(call_id: u64, event_blob: u64) -> u32
```

调用流程：

```text
Host JSON Request
→ Host 创建 request_blob
→ plugin_invoke(call_id, request_blob)
→ Guest 在调用期间复制并解析 Request
→ Guest 可通过 host_cap_call 请求获批能力
→ Guest 通过 host_blob_put 复制 Result 到 Host
→ Guest 返回 response_blob
→ Host 校验并读取 Result
→ Host 释放本次调用的全部 Blob Handle
```

Blob Handle 必须：

- 带 generation，防止旧 Handle 被复用。
- 绑定 `call_id`，不能跨调用访问。
- 受单 Blob 和单调用总配额限制。
- 在 Trap、Timeout 和取消时统一回收。

Handle `0` 永远无效。所有 Pointer 只在当前 Guest Call 内用于立即复制，Host 不保留 Guest Memory View。

所有权规则：

- `request_blob` 由 Host 所有，Guest 只借用，不得释放。
- `host_cap_call` 返回的 Blob 由 Guest 临时持有，读取后应释放。
- `host_blob_put` 返回的 Result Handle 在 `plugin_invoke` 返回时转移给 Host。
- `host_emit` 在调用期间立即复制 Event，Guest 不向 Host 转移原始内存。
- 重复释放、伪造 Handle 和越权访问返回 ABI Error，不能触发宿主 Panic。

复杂的 `unsafe`、`go:wasmimport` 与 `go:wasmexport` 代码只存在于宿主提供的 Plugin SDK Wrapper 中，不由模型生成。

实际实现（`internal/plugin/wasm`、`sdk/pluginabi`，ABI `Version = 1`，导入模块 `local_agent_host_v1`）：

```text
plugin_abi_version() -> i32
plugin_invoke(request_blob: i64) -> response_blob: i64   // 无 call_id 参数
host_blob_len(handle: u64) -> i64                         // 失败返回 -1
host_blob_read(handle: u64, dst_ptr: u32, capacity: u32) -> i32   // 0 成功，1 失败；一次读全，无 offset
host_blob_put(src_ptr: u32, len: u32) -> u64              // 失败返回 0
host_blob_drop(handle: u64) -> i32                        // 只能释放 Guest 自有 Blob
```

`host_cap_call` 与 `host_emit` 未实现。响应 Blob 内容为 SDK 生成的 `{"ok":bool,"result":<JSON>,"error":string}` 信封，宿主解包后只把 `result` 交给 Pipeline。Handle 作用域是每次调用的 module 名 `local-agent-call-N`（进程内单调计数），调用结束 `end(callKey)` 统一回收；没有 generation 计数。Blob 区分 Host/Guest 所有者：Guest 不能释放 request Blob，响应 Handle 必须是 Guest 所有。默认配额为单 Blob 16 MiB、单调用 32 MiB。SDK 不调用 `host_blob_drop`。

### 9.4 模型只生成业务实现

LLM 仅生成受限包中的业务代码：

```go
package implementation

import "local-agent/plugin-sdk/tool"

type Request struct {
    Location string `json:"location"`
}

type Response struct {
    Summary string `json:"summary"`
}

func Invoke(host tool.Host, req Request) (Response, error) {
    // 业务逻辑；ABI wrapper、导出函数和 unsafe 均由宿主模板提供。
    return Response{}, nil
}
```

这样可以避免模型生成编译器指令、内存 ABI 或生命周期代码。

实际实现：模型提交的是单个 `package main` 源文件，导出 `func Invoke(request json.RawMessage) (json.RawMessage, error)`，没有 `tool.Host` 参数和类型化 Request/Response。编译器另行生成 `go.mod`（`module local-agent-candidate`，`go 1.27.0`）、`main.go`（`init` 中 `pluginabi.Register(Invoke)`）并复制嵌入的 `sdk/pluginabi` 源码（`abi.go`、`bridge_wasip1.go`）。源码策略见 11.2。

---

## 10. wazero 执行策略

### 10.1 Runtime 配置

第一版使用统一硬上限：

```go
runtimeConfig := wazero.NewRuntimeConfigCompiler().
    WithMemoryLimitPages(1024).       // 64 MiB
    WithCloseOnContextDone(true)
```

具体 API 以固定的 wazero v1.12.0 编译结果为准。

必须同时满足：

- 所有 Guest Call 使用带 Deadline 的 Context。
- 所有 Host Capability 也必须尊重同一 Context。
- Timeout 后模块已经关闭，禁止放回实例池。
- Guest Trap 转换为结构化 Tool Error，不能导致宿主 Panic。
- Host Function 自身设置 Panic Recovery 边界，但不能依赖 Recovery 掩盖逻辑错误。

### 10.2 实例生命周期

安全优先的默认策略：

1. `CompileModule` 结果按 WASM Hash 缓存。
2. 每次 Tool 调用创建新 Module Instance。
3. 调用完成后关闭 Instance。
4. 编译缓存可以复用，线性内存和 Guest 全局状态不复用。

只有经过基准测试并定义可靠 Reset Contract 后，才允许对特定只读插件启用暖实例池。

实际实现：编译结果缓存在 `plugin.Manager.artifacts`，键为 `tool@revision` 而非 WASM Hash；每次调用新建 Instance（`WithName("local-agent-call-N")`）并在返回时关闭；默认超时 30s（模型构建路径的插件上限 5s），1024 页内存上限，`CloseOnContextDone`。退役 revision 保持已编译，直到 `plugins collect` 回收。没有暖实例池。

### 10.3 并发

- 单个 Go WASM Module Instance 不执行并发调用。
- 并发 Tool Call 使用独立 Instance。
- Host Blob Store 按 `call_id` 分区。
- 相同资源上的写操作由宿主资源锁和 Policy Engine 串行化，而不是依赖插件自律。

实际实现：Blob Store 按每次调用的 call key（`local-agent-call-N`，不是 Tool CallID）分区；Eino 工具配置为 `ExecuteSequentially`，同一 Agent 内 Tool 串行执行。

---

## 11. 编译器 Worker 与供应链安全

### 11.1 为什么编译器必须进程隔离

生成源码尚未运行，但编译器仍会处理攻击者可控输入。编译任务不得与 Agent 主进程共享无限资源和开放环境。

Compiler Worker 由宿主启动为独立进程，并设置：

- 独立临时目录。
- 只读 Go 1.27.0 Toolchain。
- 独立 `GOCACHE` 和 `GOMODCACHE`。
- 空白或严格白名单环境变量。
- 构建超时、内存、CPU、进程数和输出文件大小限制。
- 整个进程组取消；Windows 使用 Job Object 或等价机制。
- 无网络构建。

实际实现：没有常驻 Compiler Worker；`pluginbuild.Compiler` 每次构建通过 `sandbox.New` 获取 OS 沙箱（macOS Seatbelt / Linux bubblewrap，无可用后端时拒绝而非降级），在其中经 shell 执行 `ulimit -t 90 && ulimit -f 131072 && exec go build …`，整体 2 分钟超时，取消时杀进程组。使用宿主 `runtime.GOROOT()` 下的 `go`（必须为 go1.27.0，并记录二进制 SHA-256），工具链只读可见；沙箱把 `HOME/TMPDIR/GOCACHE/XDG_CACHE_HOME` 指向每次调用私有临时目录，并剥离名称含 KEY/TOKEN/SECRET 等的环境变量；未单独设置 `GOMODCACHE`（无外部依赖，`GOPROXY=off`）。内存只有 `GOMEMLIMIT=256MiB` 软限制与 `GOMAXPROCS=2`，没有进程数限制；Windows Job Object 未实现。同一时刻只运行一个构建。

### 11.2 源码策略

Agent 生成源码必须通过 AST 检查：

- 仅允许标准库白名单和随构建模板 vendored 的 Plugin SDK。
- 业务源码禁止导入 `unsafe`、`syscall`、`os/exec`、`plugin` 和内部 ABI 包。
- 禁止 Agent 源码包含 `//go:`、`//line`、cgo 和汇编文件。
- 禁止符号链接逃出 Staging Root。
- 限制文件数量、单文件大小、AST 深度和总源码大小。
- JSON Schema 与 Go Request 类型必须通过契约测试保持一致。

ABI Wrapper、`go.mod`、vendored SDK 和构建脚本均由宿主生成，模型只能填充指定业务目录。

实际实现（`internal/pluginbuild/source.go`、`types.go`）：只接受单个源文件字符串（1 字节–64 KiB），不接收目录，因此不存在符号链接、多文件、cgo 或汇编文件问题。AST 检查为：包名必须是 `main`；导入只允许 `encoding/json strings bytes strconv math sort unicode unicode/utf8 errors fmt regexp slices cmp` 且禁止别名；任何含 `go:` 的注释都被拒绝（`//line` 指令本身不在检查范围）；禁止声明 `init`、`main`、`pluginInvoke`、`pluginABIVersion`；必须声明顶层 `Invoke`（签名由编译时 `Register(Invoke)` 类型检查保证）。没有 AST 深度限制和 Schema/类型契约测试。请求另要求工具名以 `extension_` 开头、`PluginID`/revision ≤128 字节、`RiskClass=pure`、无 Capabilities、超时 ≤5s（缺省 5s）、附带 1–32 个测试用例、整个构建请求 JSON ≤256 KiB。SDK 不是 vendored 目录，而是嵌入宿主二进制的 `sdk/pluginabi` 源码在构建时复制进临时模块。

### 11.3 固定构建环境

逻辑构建环境：

```text
GOOS=wasip1
GOARCH=wasm
CGO_ENABLED=0
GOWORK=off
GOPROXY=off
GOSUMDB=off
GOFLAGS=-mod=vendor -trimpath -buildvcs=false
```

不得让构建自动访问公网下载依赖。新增 SDK 依赖必须随宿主版本审核和发布。

实际实现：没有 `GOFLAGS` 与 vendor 模式；构建参数直接写在命令行（`-mod=readonly -trimpath -buildvcs=false -buildmode=c-shared`）。完整环境为 `PATH=<GOROOT>/bin:/usr/bin:/bin GOROOT=<host> GOTOOLCHAIN=local GOENV=off GOWORK=off GOPROXY=off GOSUMDB=off CGO_ENABLED=0 GOOS=wasip1 GOARCH=wasm GOMAXPROCS=2 GOMEMLIMIT=256MiB`，沙箱再追加私有 `HOME/TMPDIR/GOCACHE/XDG_CACHE_HOME`。

### 11.4 构建记录

每次构建生成 `build.json`：

```json
{
  "goVersion": "go1.27.0",
  "sdkVersion": "1.0.0",
  "sourceSha256": "...",
  "wasmSha256": "...",
  "manifestSha256": "...",
  "startedAt": "...",
  "durationMs": 0,
  "tests": [],
  "diagnostics": []
}
```

构建记录和测试结果不可由候选插件自身修改。

实际实现：没有 `build.json` 文件。每次构建在 SQLite `plugin_builds` 中记录请求与结果：`BuildEvidence{toolchain, toolchain_sha256, sdk_sha256, source_sha256, artifact_sha256, sandbox, log(≤64 KiB), success}`，`Result.State` 为 `failed` / `quarantined`（测试未通过）/ `awaiting_approval`（测试通过、待人工发布），错误文本截断至 4096 字节；测试报告存 `plugin_test_reports`（1–32 个用例，总时限 30s）。没有 `sdkVersion`/`manifestSha256`/`durationMs` 字段。

---

## 12. 插件生命周期与热切换

### 12.1 状态机

```text
Draft
  │ static checks
  ▼
Validated
  │ compile
  ▼
Built
  │ ABI + contract + smoke tests
  ▼
Quarantined
  │ policy / optional user approval / shadow evaluation
  ▼
Active ──► Retired
  │ failure threshold
  ▼
Disabled
```

任一阶段失败都保留诊断信息，但不会影响当前 Active Revision。

实际实现：状态分两层。构建服务（`pluginbuild.Service`）的结果状态为 `failed`（校验/编译/暂存失败）、`quarantined`（测试未通过）、`awaiting_approval`（测试通过）；`plugin.Manager` 与 `plugin_revisions` 的 revision 状态只有 `staged`、`active`、`retired`（每个工具至多一个 active）。没有 Draft/Validated/Built 独立状态，没有 `Disabled` 状态和失败阈值自动停用。

### 12.2 不可变 Revision

插件目录按内容 Hash 存储：

```text
${AGENT_HOME}/plugins/local.weather/
├── revisions/
│   ├── sha256-aaa/
│   │   ├── manifest.json
│   │   ├── source/
│   │   ├── build.json
│   │   ├── tests.json
│   │   └── tool.wasm
│   └── sha256-bbb/
│       └── ...
└── registry.snapshot.json
```

`registry.snapshot.json` 只是便于检查的可重建快照；Active Revision 的持久化事实位于数据库事务和审计事件中。正式激活只修改 Registry 中的 Hash 映射，不覆盖旧文件。

实际实现：没有 `plugins/` 目录和 `registry.snapshot.json`。revision 记录在 SQLite `plugin_revisions`，WASM 字节在 `plugin_blobs`（按 SHA-256 去重），源码与构建证据在 `plugin_builds`，测试在 `plugin_test_reports`；Registry 快照身份在 `runtime_snapshots`。revision 字符串由提交方给出（≤128 字节），同名同 revision 不可覆盖。

### 12.3 原子切换

```text
Build Candidate
→ Validate Imports/Exports
→ Instantiate in Staging Runtime
→ Contract Tests
→ Smoke Tests
→ Optional Shadow Calls
→ Durable Transaction:
     Update Active Mapping + Append plugin.activated
→ Publish In-memory Atomic Registry Snapshot
→ Drain Old Revision
→ Close Old Compiled Module When Unreferenced
```

已经开始的调用持有旧 Snapshot，直到结束；新调用获得新 Snapshot。这是 read-copy-update 语义，而不是关闭正在执行的旧模块。

实际实现：`Manager.Activate` 在 `Registry.ActivateManyWithCommit` 回调内先持久化 active 映射（catalog CAS），成功后才发布新内存 Snapshot；旧 active 变为 `retired` 但保持编译，不做 drain/lease 跟踪，模块仅在 `Manager.Close` 或 `plugins collect` 时关闭。没有 `plugin.activated` 事件，状态变化体现在 `plugin_revisions` 行。`Rollback` 即对保留的 revision 再次 `Activate`。Shadow Calls 未实现；测试为构建时附带用例的确定性比对。

### 12.4 自动晋升规则

建议默认规则：

| 风险等级 | 示例 | 是否允许自动晋升 |
| --- | --- | --- |
| Pure | 格式化、解析、纯计算 | 测试通过后可配置自动晋升 |
| Read | 工作区只读、受限 HTTP GET | 首次人工审批，后续按策略 |
| Write | 文件写入、数据库变更 | 必须人工审批 |
| Execute | Shell、进程、Git push | 必须逐次或严格规则审批 |
| Secret | 代表用户访问第三方账户 | 默认禁止动态插件直接获得 |

授权绑定插件 Revision Hash。插件更新后不得继承旧版本的敏感授权。

实际实现：没有自动晋升。动态插件只能是 Pure；晋升（`PublicationService.Promote`）要求显式 `confirmed` 与一份通过的测试报告，且报告的工具、revision、内容 SHA-256 与 RuntimeIdentity 必须与候选一致。经 Agent 发布时 `plugin_publish` 受策略 `require_approval` 约束，CLI 为 `plugins activate|rollback --report-id … --confirm`；调用已发布的 `extension_*` 工具也逐次 `require_approval`。没有持久授权，审批不跨调用复用。

---

## 13. Policy 与 Human-in-the-loop

### 13.1 决策输入

```text
Subject:
  session_id
  agent_id
  tool_name
  plugin_revision_hash

Action:
  capability
  normalized_arguments

Resource:
  workspace_path / host / command / credential_ref

Context:
  user_policy
  runtime_snapshot
  risk_class
  prior_approval
  budget
```

实际实现（`internal/policy/policy.go`）：`policy.Request{Actor, CallID, SessionID, ToolName, Revision, Capability, Resource, RiskClass, NormalizedArg}`，Pipeline 固定填 `Capability="tool.invoke"`、`Resource="tool:<name>"`，`NormalizedArg` 为原参数副本。`StaticEngine` 只按 `ToolPattern`/`CapabilityPattern`（`path.Match` glob）首条命中，缺省 `DefaultDeny`；规则硬编码在 `internal/app/builtins.go`，没有用户策略文件，也不读取 Resource、参数、预算或历史审批。路径、命令等资源约束由各工具自身（工作区根校验、OS 沙箱）实施。

### 13.2 决策结果

```text
Allow
Deny
RequireApproval
AllowWithConstraints
```

审批必须绑定：

- Tool Revision。
- 规范化参数或参数约束。
- 资源范围。
- 有效期和最大使用次数。
- Session 或全局作用域。

实际实现：`policy.Effect` 只有 `allow`、`deny`、`require_approval`，没有 `AllowWithConstraints`。审批是一次性的：每次 `require_approval` 调用都单独询问，结果只作用于该次调用，不保存、不复用，因此没有有效期、使用次数、作用域或参数哈希绑定（审批请求展示 Tool、revision 与参数快照）。

### 13.3 中断与恢复

审批不在 WASM 内通过阻塞式 `host_ask_user()` 实现。

正确流程：

1. Tool Adapter 生成审批请求。
2. 通过 Eino Interrupt 暂停 Runner。
3. Checkpoint Store 持久化恢复点。
4. CLI/TUI 获取用户选择。
5. 宿主写入 `approval.resolved`。
6. 使用 Eino Resume 继续执行。

这样即使进程退出，审批中的会话也能恢复。

实际实现：未使用 Eino Interrupt/Resume 做审批。Pipeline 写 `approval.requested` 后同步调用 `policy.Approver`：交互模式为 `approval.Broker`（请求 ID `approval-<base36>`，队列 16，UI 调 `Resolve` 唤醒等待的执行 goroutine，`Close` 时释放全部等待者并使其失败），非交互模式为 `DenyApprover`（直接拒绝），`--evaluation-mode` 为工具白名单审批器，无审批器时返回 `policy.ErrApprovalUnavailable`。审批结果写 `approval.resolved`。审批状态只在内存中，不跨进程保留；进程在等待审批时退出，该 Turn 成为中断 Turn，需经 `--resume-turn`（从 checkpoint 恢复）或 `--abandon-turn` 显式处理。

### 13.4 Tool Result 不可信

文件、网页和外部 Tool Result 都可能包含 Prompt Injection。宿主必须：

- 标记数据来源和信任级别。
- 不把 Tool Result 当成系统指令。
- 不允许 Tool Result 修改 Policy。
- 对外部内容触发的敏感操作重新审批。

实际实现：没有通用的 Tool Result 来源/信任级别标记；Tool Result 以 Tool 消息进入上下文，策略为静态规则不受其影响，敏感工具每次调用都需审批。子 Agent 消息、邮箱与父上下文摘录以 “Untrusted …” 标题注入。

---

## 14. Loop 的可进化方式

### 14.1 第一版使用 LoopSpec，不生成 Loop 代码

可进化的编排策略表示为数据：

```yaml
schemaVersion: 1
id: coder-default
baseAgent: chat-model-agent
maxSteps: 40
modelRoute: coding-primary
toolSets:
  - workspace-read
  - workspace-write
middlewares:
  - agentsmd
  - summarization
  - tool-reduction
  - skill
  - plan-task
contextPolicy:
  summarizeAtPercent: 80
  preserveRecentUserTokens: 30000
failurePolicy:
  modelRetries: 2
  modelFailover: coding-fallback
  toolFailureBudget: 5
approvalPolicy: workspace-standard
```

`LoopSpec` 由宿主 Schema 校验并映射到受支持的 Eino Agent 与 Middleware，不能引用任意 Go 符号。

实际实现：上面的 YAML 是设计形态，未实现。实际“LoopSpec”是 `internal/strategy.Spec`（JSON），只有 `version`（=1）、`revision`（≤128 字节）、`owner_session`、`instruction_suffix`（≤8192 字节）、`max_iterations`（≤100）、`max_output_tokens`（≤32768）。它只能在宿主基线指令后追加文本、调低迭代与输出上限，不能更换工具集、模型路由、上下文策略、审批策略或失败策略（也没有 Middleware 可选）。Spec ID 为规范化 JSON 的 SHA-256；`turn.started` 只记录 `loop_spec_id` 与 `base_execution_config_id`。

### 14.2 LoopSpec 激活

- 候选 LoopSpec 先在复制的 Session 或评测集上运行。
- 记录完成率、Token、延迟、Tool 错误和审批次数。
- 达到门槛后激活。
- 激活只影响下一个 Turn。
- 失败时回退到编译进宿主的 `base-default`。

实际实现：候选在用户登记的 shadow suite（1–8 个 fixture，至少一个 `safety` fixture，期望输出精确字符串匹配）上与基线成对运行，空 Registry、`DefaultDeny`，单例 30s、总计 2 分钟，消耗当前 Team 预算；报告记录通过、模型调用数、输出字节与耗时。验收门槛：全部 safety fixture 通过、候选通过数 ≥ 基线、候选模型调用数 ≤ 基线。没有自动激活：`strategy publish --report-id … --confirm` 或 `strategy_publish`（策略 `require_approval`）以 CAS 写 `strategy_active` 并记 `strategy.promoted`，只影响后续 Turn。任一非用户取消的 Turn 失败即回退到宿主基线配置，候选进入 `strategy_quarantine` 并记 `strategy.fallback`，不重放当前 Turn；也可用 `strategy rollback --confirm` 手动回退。

### 14.3 可选的未来 WASM Driver

只有在 LoopSpec 无法表达需求后，才引入实验性 WASM Driver，且必须采用单步 reducer：

```text
Step(state, event) -> Action
```

允许的 `Action`：

```text
RequestModel
CallTool
AskUser
CreateCheckpoint
Yield
Finish
```

WASM Driver 不直接调用 LLM 或 Tool；宿主执行 Action、持久化 Event，再调用下一次 `Step`。这保证控制权在每个状态转换后回到宿主。

实际实现：WASM Driver 未实现（计划中）。

---

## 15. 自进化分层

自进化按风险从低到高分四层：

### L0：记忆与 Skills

- 新增说明文档、操作流程和项目知识。
- 不执行新代码。
- 经过来源标记和大小限制即可使用。

实际实现：Skills、AGENTS.md 与持久记忆未实现（计划中）。

### L1：LoopSpec 与 Prompt 策略

- 调整工具集合、Middleware、上下文预算和停止条件。
- 使用评测集和 Shadow Session 验证。
- 只在 Turn 边界生效。

实际实现：只能追加指令与调低迭代/输出上限（见 14.1），经 shadow suite 验证，Turn 边界生效。

### L2：WASM Tool

- 生成受限 Go 业务实现。
- 经过编译、测试、权限评估和版本晋升。
- 无权修改 Trusted Runtime。

实际实现：已实现，限 Pure 插件（见第 9、11、12 节）。

### L3：宿主升级

- 通过正常软件发布、签名包和用户确认完成。
- 永不由运行中的 Agent 自动完成。

“Self-Evolving”主要指 L0-L2；L3 仍属于传统软件升级。

---

## 16. 本地文件系统布局

```text
${AGENT_HOME}/
├── bin/
│   └── local-agent
├── config/
│   ├── config.toml
│   ├── policies.toml
│   └── credentials.refs.toml
├── state/
│   ├── agent.db
│   └── blobs/
├── toolchains/
│   └── go1.27.0/
├── plugins/
│   └── <plugin-id>/
│       ├── revisions/
│       └── registry.snapshot.json
├── loop-specs/
│   ├── revisions/
│   └── registry.snapshot.json
├── skills/
├── cache/
│   ├── go-build/
│   └── wazero/
├── workspaces/
├── logs/
│   ├── runtime.log
│   └── audit.log
└── tmp/
```

真实 API Key 默认保存在操作系统 Keychain/Credential Manager 中，配置文件只保存引用。无法使用系统凭证库时，才使用用户口令保护的本地加密存储。

实际实现：上图是设计形态。实际 `GSEA_HOME`（缺省 `~/.gsea`，`internal/config`）布局为：

```text
${GSEA_HOME}/
├── config.toml                # 用户配置；另可有 <profile>.config.toml（-p/--profile）
├── control.db                 # 托管 Session 目录（sessionregistry）
├── sessions.db                # 缺省 data_directory=GSEA_HOME 时的会话库
├── artifacts/sha256/<hex>     # 大 Tool 结果等内容寻址 artifact
├── views/                     # Multi-Agent 工作区视图
├── sessions/<id>/{data,tmp,tool-state}   # `session` 命令创建的托管 Session
└── workspaces/<identity>/coord.db        # 跨 Session 工作区协调
```

`sessions.db`、`artifacts/`、`views/` 位于 `data_directory`（缺省即 `GSEA_HOME`，托管 Session 为 `sessions/<id>/data`）。项目级配置为最近祖先目录的 `.gsea/config.toml`；跨 `GSEA_HOME` 的协调目录来自 `GSEA_COORDINATION_DIR` 或用户状态目录下的 `gsea/coordination`。没有 `bin/`、`policies.toml`、`credentials.refs.toml`、`toolchains/`、`plugins/`、`loop-specs/`、`skills/`、`cache/`、`logs/`：插件与策略存于 SQLite，Go 构建缓存在每次构建的临时目录，wazero 不做磁盘编译缓存。凭证未接入 Keychain：配置只保存环境变量名（`[model] api_key_environment`，缺省 `OPENAI_API_KEY`），运行时从该环境变量读取；含凭证的 base URL 被拒绝。

---

## 17. 可观测性

每次 Turn 生成统一 Trace，至少关联：

- Session、Turn、Step。
- Model Provider、Model 和请求序号。
- Runtime Snapshot。
- Tool Revision。
- Policy Decision 与 Approval。
- Plugin Instance。
- Token、费用、延迟、错误类别。

日志分级：

- Runtime Log：调试和运行状态。
- Audit Log：权限、审批、插件激活和副作用。
- Session Event Log：可恢复的业务事实。

三者用途不同，不能混用。日志中的 Prompt、Tool Result 和路径可能包含敏感信息，导出前必须脱敏。

实际实现：没有统一 Trace，也没有 Runtime Log / Audit Log 文件；可观测性来自 Session Event Log（含 `runtime_snapshot_id`、`policy.decided`、`approval.*`）、`orchestration_events`、`strategy_events`、插件构建/测试记录，以及 `usage_records`（Token 与按配置价格计算的费用）。查询入口为 `session`、`control`、`doctor`、`plugins list|inspect-build`、`strategy report` 等 CLI 子命令。没有导出脱敏流程。

---

## 18. 故障处理矩阵

| 故障 | 处理方式 | 是否自动重试 |
| --- | --- | --- |
| 模型 429/临时网络失败 | 退避、同 Provider 重试、可选 Failover | 是，受预算限制 |
| 模型认证失败 | 停止并提示更新凭证 | 否 |
| WASM Trap | 关闭 Instance，记录失败，返回结构化 Tool Error | 默认否 |
| WASM Timeout | Context 取消并关闭 Instance | 仅纯函数可按策略重试 |
| WASM 内存超限 | Trap，禁用或降级插件 | 否 |
| Host Capability 失败 | 返回明确错误码 | 取决于错误分类 |
| 副作用结果未知 | Reconcile 或询问用户 | 否 |
| LoopSpec 构造失败 | 使用内置 Base LoopSpec | 是，继续当前会话 |
| Session Snapshot 损坏 | 从事件日志重建 | 是 |
| 事件日志损坏 | 停止写入并进入只读恢复模式 | 否 |
| 候选插件构建失败 | 保留诊断，不影响 Active Revision | 可由 Agent 修复后新建候选 |

自动重试必须基于错误语义和幂等性，不能对所有失败统一重跑。

实际实现：没有任何自动重试。模型请求失败（含 429、认证失败）写 `model.failed` 并使 Turn 失败，无退避、无 Failover。WASM Trap、Timeout 与内存超限都作为 `ExecutionError` 返回模型，插件不会被自动禁用或降级。Host Capability 未实现。副作用结果未知时返回致命 `OutcomeUnknownError` 交由用户核对，没有 Reconcile。策略候选导致 Turn 失败时，后续 Turn 回退到宿主基线，当前 Turn 不自动重跑。没有 Session Snapshot，事件 checksum 不符时读取报错，没有只读恢复模式。候选构建失败行为与上表一致。

---

## 19. 测试与验收

### 19.1 单元与契约测试

- Eino Tool Adapter 的 Schema 和错误映射。
- Policy Decision 的路径、Host、命令和 Revision 约束。
- Session Event 的序列、校验和与重放。
- Blob Handle 的作用域、配额和回收。
- Plugin Manifest 与 ABI 版本检查。
- LoopSpec Schema 与 Eino 配置映射。

### 19.2 必须通过的故障测试

1. 无限循环 WASM 在超时后终止，宿主仍可继续新会话。
2. 内存持续增长的 WASM 被限制在固定上限内。
3. 越界 Pointer、伪造 Blob Handle 和重复释放不会导致宿主 Panic。
4. 插件调用中热激活新 Revision，旧调用正常完成，新调用使用新版本。
5. 插件构建失败不影响 Active Revision。
6. Tool 已产生副作用但宿主随后崩溃，恢复后进入 `OutcomeUnknown` 而不是自动重跑。
7. 审批等待期间退出进程，重启后可以继续 Resume。
8. 恶意网页内容不能绕过 Policy 获得文件写入或 Shell 权限。
9. Go 编译过程无法访问网络、宿主敏感环境变量或 Staging Root 之外的源码。
10. 工具链安装完成后，离线环境能够编译标准模板并执行插件。

实际实现：第 7 条的前提不成立（审批不跨进程保留，见 13.3），中断 Turn 需经 `--resume-turn` 恢复；第 10 条没有工具链安装步骤，构建直接使用宿主 Go 1.27.0。

### 19.3 Fuzz 与安全测试

- Fuzz Manifest、JSON Envelope 和 Blob ABI。
- Fuzz Tool Schema 与 Go Request 映射。
- 随机取消 Host Call 和 Guest Call。
- 模拟磁盘满、数据库写失败和部分 Blob 损坏。
- 对路径规范化、符号链接和重定向策略做专门测试。

实际实现：仓库中目前没有 `Fuzz*` 测试（计划中）。

### 19.4 性能基准

正式文档只记录实测数据：

- 宿主冷启动。
- Eino 首 Token 延迟。
- WASM CompileModule 延迟。
- Fresh Instance 创建延迟。
- Go 1.27.0 Tool 编译延迟。
- 单次 Tool ABI 序列化开销。
- 事件日志写入和恢复时间。
- 不同 Tool Result 大小下的内存峰值。

不预设 `<10ms`、`毫秒级完整自进化` 等未经验证的指标。

实际实现：仓库中目前没有 `Benchmark*` 测试，本文也尚未记录上述实测数据。

---

## 20. 实施路线图

### Phase 0：技术验证

- 固定 Go 1.27.0、Eino v0.9.14 和 wazero v1.12.0。（实际 go.mod 已升级至 Eino v0.9.22。）
- 验证 Eino ChatModelAgent + 本地 CLI 流式输出。
- 验证 Go 1.27.0 WASI reactor、`_initialize` 和 Tool ABI v1。
- 完成无限循环、内存超限和 Host Blob Handle 测试。

验收标准：一个手工编写的纯计算 WASM Tool 可以被 Eino 调用，Trap 和 Timeout 不影响宿主后续运行。

### Phase 1：可用的本地 Agent

- ModelGateway：OpenAI Chat Completions / Responses + Ollama。
- Eino Base Agent、Runner 和核心 Middleware。
- CLI/TUI、Session 管理和 Ctrl+C。
- 原生只读文件工具与 Policy-aware Backend。

验收标准：能完成多轮本地编码辅助任务，但还不允许 Agent 生成代码插件。

### Phase 2：持久化与安全控制面

- Append-only Session Event Store。
- Checkpoint、Interrupt/Resume。
- Policy Engine、审批与 Audit Log。
- 副作用状态机、幂等键和 OutcomeUnknown 恢复。

验收标准：任意安全边界退出后，会话可以从最后 Checkpoint 恢复且不重复已提交副作用。

### Phase 3：WASM Plugin Runtime

- Manifest、不可变 Revision 和 Tool Registry Snapshot。
- Compiler Worker、固定 SDK 模板和离线构建。
- Staging、契约测试、激活、排空与回滚。
- 受限的 `fs.read`、`net.http` 和 KV Capability。

验收标准：人工提交的新 Tool 可以在不中断宿主的情况下热激活和回滚。

### Phase 4：Agent 生成 Tool

- 能力缺口识别。
- 受限源码生成 Prompt。
- 自动生成测试和修复循环。
- Quarantine、风险分级与晋升审批。

验收标准：Agent 能生成一个只读 Tool，通过完整流水线后继续原 Session，并留下可审计的源码、构建和激活记录。

### Phase 5：策略自进化

- LoopSpec Schema、版本库和评测集。
- Shadow Session 与指标比较。
- Turn 边界原子切换。
- Base LoopSpec 自动回退。

验收标准：策略升级失败不会影响会话事实和 Tool 副作用，且可以明确比较升级前后的收益。

### Phase 6：实验性 WASM Driver

- 仅在 LoopSpec 表达力不足时启动。
- 使用 `Step(state, event) -> Action` reducer 契约。
- 禁止 Guest 内部直接驱动 LLM、审批和 Tool。

此阶段不是 MVP 的前置条件。

实际进度（2026-10-26 核对）：

- Phase 0：已完成。
- Phase 1：已完成（Bubble Tea TUI 与 headless 模式），但未使用 Eino Middleware（上下文管理为自研，见 5.3）。
- Phase 2：基本完成。事件存储、checkpoint Resume、策略与审批、OutcomeUnknown 检测均已实现；Eino Interrupt 审批、Audit Log 与 Reconcile 未实现。
- Phase 3：基本完成。Revision、Registry Snapshot、离线沙箱构建、测试、激活、回滚与精确 GC 已实现；排空（drain）和 Host Capability（`fs.read`/`net.http`/KV）未实现。
- Phase 4：部分完成。已有 `plugin_propose/inspect/publish`、构建失败诊断回传模型修复、quarantine 与人工晋升，但只支持 Pure 插件（不能生成只读 I/O Tool），也没有自动能力缺口识别。
- Phase 5：以受限形式完成（见 14.1、14.2）。
- Phase 6：未实现。
- 路线图之外另已交付 Multi-Agent、Goal Mode、Flow、LSP 诊断与评测（`eval`），见 MULTI_AGENT_ARCHITECTURE.md、EVOLUTION.md 与 `evals/README.md`。

---

## 21. 推荐的首个纵向切片

不要一开始实现完整自进化系统。首个可运行闭环只包含：

1. CLI 接收用户输入。
2. Eino ChatModelAgent 调用一个原生 `read_file` Tool。
3. Tool 调用写入 Session Event Log。
4. 用户通过命令安装一个手工编写的 `text_stats.wasm`。
5. Plugin Manager 完成验证和原子激活。
6. 同一 Session 的下一个 Turn 可以发现并调用该 Tool。
7. 修改 Tool 源码并构建新 Revision，验证调用不中断热切换。
8. 注入无限循环版本，验证超时、禁用和旧稳定版本恢复。

这个切片同时验证 Eino、事件日志、Policy、WASM ABI 和插件生命周期，是后续 Agent 自动生成 Tool 的可靠基础。

实际实现：该切片已被后续实现覆盖。原生读取工具名为 `workspace_read`（另有 `workspace_list/glob/grep/write/edit/diagnostics`、`shell_exec`）；手工插件经 `plugins stage --manifest … --wasm …`、`plugins test`、`plugins activate --report-id … --confirm` 安装，激活后下一个 Turn 的 Snapshot 可见。第 8 步中的“禁用”未实现：超时的插件调用返回执行错误，恢复旧版本需显式 `plugins rollback`。

---

## 22. 设计原则总结

- **Loop 复用 Eino，不自行实现通用 Agent Loop。**
- **宿主掌握状态和副作用，插件只提供受限计算能力。**
- **工具可以生成，权限不能由工具自己声明后自动获得。**
- **模型可以提出升级，不能自己成为升级审批者。**
- **热更新是版本切换，不是覆盖文件。**
- **恢复基于事件和 Checkpoint，不基于重新执行用户 Prompt。**
- **WASM 提供故障隔离和能力约束，不提供物理世界事务回滚。**
- **性能和可靠性结论必须由测试证明。**

该架构保留了“自进化本地智能体”的核心价值，同时将最难且最关键的 Agent Loop、会话一致性、权限与副作用管理放在稳定、可审计的 Go Trusted Runtime 中。

---

## 23. 主要参考

- Go 1.27 Release Notes：<https://go.dev/doc/go1.27>
- Go 1.27 Release Announcement：<https://go.dev/blog/go1.27>
- Go WASM Reactor 与 `go:wasmexport`：<https://go.dev/blog/wasmexport>
- Eino：<https://github.com/cloudwego/eino>
- Eino Documentation：<https://www.cloudwego.io/docs/eino/>
- wazero：<https://github.com/tetratelabs/wazero>
- wazero API：<https://pkg.go.dev/github.com/tetratelabs/wazero>
- DeepSeek Harness Architecture：<https://github.com/deepseek-ai/deepseek-harness/blob/master/docs/architecture.md>
- DeepSeek Harness HMR：<https://github.com/deepseek-ai/deepseek-harness/blob/master/docs/cordis-tutorial/06-composition-and-hmr.md>
