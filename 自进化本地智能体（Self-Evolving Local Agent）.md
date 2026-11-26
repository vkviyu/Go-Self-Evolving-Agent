# 自进化本地智能体架构设计

## 从一次排障到下一次任务

某次排查流超时，日志显示宿主收到了响应头，随后一直在等待响应体字节。另一个项目遇到类似现象时，主 Agent 可以向 ATE 查询上次留下的经验，核验出处与适用条件，再决定是否采用。第二个项目的超时原因要从自己的执行记录中确认。

GSEA 以这种有来源、可修正的跨任务记忆定义 Evolving。本文保留 2026 年 8 月设计基线与 10 月复核时的 Eino、审批、工作区及恢复设计背景。现行宿主通过 stdio 接入 ATE。[跨任务记忆指南](EVOLUTION.md) · [架构导读](ARCHITECTURE.md) · [Multi-Agent 架构](MULTI_AGENT_ARCHITECTURE.md)

---

## 1. 结论与关键决策

本项目实现的是一个**本地可信 Agent Runtime**，不是从零发明 Agent Loop，也不是让模型随意重写宿主程序。

系统采用以下固定边界：

1. **Eino ADK 负责 Agent Loop**
   - 使用 Eino 的 `ChatModelAgent`、Runner、流式事件和 Checkpoint/Resume。
   - Go 宿主装配稳定的 Base Loop。
   - Eino 是本地 Go 库，CLI 可以直接调用，不要求启动远程 Agent 服务。
   - 实际实现：仅使用 `ChatModelAgent`、`Runner`（流式 + `CheckPointStore`）、`WithCheckPointID`/`Resume` 与 `WithCancel`；未使用 Eino Middleware，审批也不经 Eino Interrupt（见 5.3、13.3）。

2. **Go 宿主负责可信控制面**
   - 管理会话、模型、流式输出、权限、审批、凭证、预算和审计。
   - 所有有副作用的操作必须经过统一 Policy Engine。
   - 会话与业务状态通过 ATE 持久化；恢复按原身份与回执继续，不承诺任意会话分叉或副作用重放。

3. **ATE 管理跨任务经验。** 候选记录主张和原始事件引用。独立证据、当前来源状态和复用策略决定哪些经验可以搜索和采用。主 Agent 在新任务中按需读取，采用时另留回执。

4. **Turn 固定工具身份。** Registry Snapshot 保存本次执行可见的工具定义与实现身份。恢复时按原清单检查，模型、权限和执行配置也要满足原有约束。

5. **按原身份恢复。** 已有持久结果的调用复用原结果。有副作用的操作发生中断时，宿主先查原请求、事件及回执，再决定可以续做哪一步。

### 1.1 早期交付记录（2026-10-26 核对）

基础纵切（2026-08-27）已经落地并由自动化测试验证：

- SQLite append-only Session Event Store、事件校验和与乐观序列；
- 默认拒绝 Policy Engine、不可变 Tool Registry Snapshot 和统一执行管线；
- Eino `ChatModelAgent` / `Runner` 的流式 ReAct 闭环，包含模型请求、Tool 调用和耐久事件；
- Eino Tool Adapter 保留模型 tool-call ID，并固定整个 Turn 的 Registry Snapshot；
- OpenAI Chat Completions / Responses、Ollama ModelGateway，以及可恢复已完成 Turn 的 `local-agent chat`；
- OpenAI Responses SSE 的 reasoning summary、Token 用量和增量消息转换；
- Bubble Tea TUI、headless 流式 Markdown、Chroma 多语言高亮和审批交互；
- Coding Agent workspace 工具、macOS Seatbelt/Linux bubblewrap shell 边界，以及非 TTY 默认拒绝高风险调用；
- Eino Checkpoint Store、Turn Resume、上下文压缩和完整 ReAct 历史投影；
- 可运行的 `local-agent doctor` Composition Root 自检。

此后落地（截至 2026-10-26）：

- 版本化 LoopSpec（`internal/strategy`）、影子比较、CAS 发布与失败隔离回退；
- Multi-Agent：durable Supervisor（`internal/agentmanager`）、Task DAG、mailbox、快照隔离写入与 merge queue（`internal/workspacecoord`、`internal/workspacedomain`）；
- ExecutionContract/Task verification、宿主命名 Flow（`internal/agentflow`）、Team 预算与用量预留（`internal/tokenusage`）；
- benchmark 任务/结果协议与 `local-agent eval`（`internal/evaluation`，见 [evals/README.md](./evals/README.md)）。

当时仍在讨论 MCP、Skills/AGENTS.md、Plan Task 和 provider failover。后续存储切换与适用范围见 [ATE 权威迁移记录](docs/ATE_AUTHORITY_CUTOVER.md)。

---

## 2. 技术基线

### 2.1 固定版本

| 组件 | 基线版本     | 使用原则                                                                        |
| ---- | ------------ | ------------------------------------------------------------------------------- |
| Go   | **go1.27.0** | 构建宿主程序                                                                    |
| Eino | **v0.9.22**  | 使用当前稳定版本，不采用 v0.10 alpha（基线原定 v0.9.14，`go.mod` 现为 v0.9.22） |

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

### 2.3 安装宿主

在 sibling ATE 工作区就绪后运行 `make build`，由配套的 `gsea`、`ate` 提供本地运行与权威存储。[安装与运行](README.md#安装与运行)

---

## 3. 目标与非目标

### 3.1 第一阶段目标

- 本地 CLI/TUI 多轮对话和流式输出。
- 支持远程 OpenAI-compatible（Chat Completions / Responses）模型与本地 Ollama 模型。
- Eino Base Agent 支持工具调用、上下文压缩、计划任务、Skills 和人工审批。（实际实现：工具调用、自研上下文压缩和人工审批已落地；Plan Task、Skills 未实现，任务拆解由 durable `task_*` 工具与 ExecutionContract 承担。）
- 中断会话可发现恢复点；实际续跑须满足原工具快照、checkpoint、执行身份和环境约束。
- 宿主通过 Tool Registry 向 Eino 提供工作区、记忆和编排工具。
- Agent 对本地文件、Shell、网络和密钥的使用可审计、可限制、可审批。

### 3.2 暂不追求

- 不允许 Agent 自动修改或重编译 Go 宿主。
- 不提供通用容器平台或完整 Linux 虚拟化。
- 不承诺任意副作用都能事务回滚。
- 不把 Coze Studio、Coze Loop 或 DeepSeek Harness 作为运行时依赖。

---

## 4. 总体架构

```text
CLI / TUI → Conversation → Eino Agent Runtime → Model Gateway
                               │
                         Tool Pipeline
                      Policy / Approval
                               │
                ┌──────────────┼──────────────────┐
                ▼              ▼                  ▼
          Workspace/Shell   memory tools    Supervisor / Flow
                               │
                          experience
                               │
ATE client / stdio → ATE：会话、编排、用量、记忆、来源与采用记录

App：装配与生命周期
本地 runtime SQLite：checkpoint、工具快照及引用、未决请求
工作区协调库与本地资源：文件、租约、锁、Shell 进程
```

当前实现与上图的对应：

- CLI/TUI = `cmd/gsea` → `internal/cli`（`internal/tui`、`internal/render`）；Composition Root = `internal/app`。
- Eino Agent Runtime = `internal/agentruntime` + `internal/conversation`（输入串行化、执行与恢复）；Tool Registry/管线 = `internal/tool`，Eino 绑定 = `internal/einoadapter`。
- Session = `internal/session` 协议 + `internal/session/atestore`；checkpoint 和工具快照 = `internal/runtimestore`。`internal/sessionregistry` 读取 ATE 会话目录，工作区租约由 `internal/workspacedomain` 管理。
- Policy & Approval = `internal/policy` + `internal/approval`；预算 = `internal/tokenusage`。
- 记忆工具由 `internal/app/memory_tools.go` 注册，`internal/experience` 经 ATE 端口查询、创建候选、采用和维护来源。普通工具不提供验证或晋升操作。
- MCP 工具未实现；FS/Shell 以原生 workspace 工具（`internal/codingtools`）形式提供给模型。
- 图外新增：`internal/agentmanager`/`internal/orchestration`（Multi-Agent）、`internal/agentflow`、`internal/strategy`、`internal/evolutiontools`、`internal/evaluation`、`internal/toolbridge`、`internal/modelgateway`、`internal/modelcontext`。
- 具体依赖与构造点约束见 [architecture_test.go](internal/architecture/architecture_test.go)；旧包名和旧 SQL owner 不能作为现行接入方式。

### 4.1 可信计算基（TCB）

以下代码属于 Trusted Computing Base，不能由 Agent 自动替换：

- Eino Runner 适配层和 Base Agent 配置。
- Session Event Store 与 Checkpoint Store。
- Policy Engine、审批系统和凭证管理。
- Tool Registry、工具快照与执行管线。
- ATE 客户端、身份绑定与未决请求核验。
- 记忆来源核验、采用协议和宿主策略发布边界。
- Shell/进程沙箱与工作区边界实现。

TCB 可以通过正常软件版本升级更新，但不能作为运行时自进化目标。

### 4.2 数据面与控制面

- **数据面**：模型请求、Token 流、Tool 调用、Tool 结果和用户消息。
- **控制面**：权限、预算、工具身份、LoopSpec、会话状态、审批和审计。

历史记忆是有来源的数据，不能直接修改控制面或授予工具权限。

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

本项目不复制这些机制，而是在 Eino 外围增加本地可信执行、ATE 持久化与记忆接入。（实际实现：当前只用到 ChatModel/Tool 抽象、`ChatModelAgent`、Runner、流式事件和 Checkpoint；上列 Middleware、Graph/Workflow/Supervisor 均未使用，Multi-Agent Supervisor 与 Flow 是项目自研。）

### 5.2 Base Agent

第一版使用 `ChatModelAgent` 作为内置 Base Agent。实际装配在 `internal/agentruntime/eino_turn.go`（每个 Turn 构造一次）：

```go
agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
    Name:          f.config.Name,
    Description:   f.config.Description,
    Instruction:   f.runtimeInstruction(), // 配置指令 + 宿主指令
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

以上为装配代码摘录；现行实现见 [eino_turn.go](internal/agentruntime/eino_turn.go)。本次文档核对未执行编译测试。

### 5.3 Middleware 顺序

早期设计提出的逻辑顺序如下；未实现的项目不构成当前能力：

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

当前 `ModelGateway` 与用量服务分别负责：

- OpenAI Chat Completions / Responses API。
- Ollama 或其他本地模型服务。
- 流式响应规范化。
- 超时与流式传输诊断；没有通用退避重试或 Provider Failover。
- Token 与费用统计。
- 上下文窗口与最大输出配置，不承诺完整的 Vision/Tool Calling 能力发现。
- 敏感凭证注入。

Eino 只看到标准 ChatModel 接口，不直接管理真实 API Key。

实际实现（`internal/modelgateway`）：provider 只有 `openai`（`wire_api` 为 `chat-completions`，经 eino-ext openai；或 `responses`，自研 Responses SSE 客户端）和 `ollama`（默认 `127.0.0.1:11434`）。有请求超时（`[model] timeout`，默认 2m），**没有**退避重试和 Provider Failover。Token 统计与费用记账在 `internal/tokenusage`（只对显式配置的价格计费，支持 Team 预算与预留）。能力声明只覆盖上下文窗口/最大输出：OpenAI 读 `/models`，Ollama 读已加载模型的 `/api/ps`。凭证只以环境变量名配置（`[model] api_key_environment`，默认 `OPENAI_API_KEY`），在 Gateway 内解析，含凭证的 base URL 会被拒绝。

### 5.5 Runner 隔离

- 每个 Session 拥有独立的 Runner State。
- 每个 Turn 固定工具 `RuntimeSnapshot`；模型配置与 LoopSpec 使用各自的持久身份。
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

早期事件类型摘录（非当前完整清单；现行常量见 `internal/session/event.go`，旧 Goal 事件不再表示现行执行协议）：

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

当前 Session 事件由 ATE 原生 Session 保存；策略发布/回退及 Multi-Agent 控制面通过各自仓库写入 ATE state records。它们不是一个本地 `control_events` 表，也不与任意 Session append 共享事务。Turn 通过 `runtime_snapshot_id` 引用本地工具清单，通过 `loop_spec_id` 固定策略内容。

### 6.2 Snapshot 与重放

- Snapshot 是事件日志的派生缓存，不是事实来源。
- 每 N 个事件或每个 Turn 结束时生成 Snapshot。
- Snapshot 损坏时可以从事件日志重建。
- 大型 Tool Result 和文件产物存入内容寻址 Blob Store，事件只保存 Hash 引用。
- Context Summarization 生成新事件，但不删除原始消息。

上面的周期性 Session Snapshot 是早期设想，不是当前实现。历史由 ATE Session 事件投影，本地 `agent_checkpoints` 保存 Eino 不透明 checkpoint，工具快照另存清单与引用。内容寻址 Blob Store 为 `internal/artifact`（`<data_dir>/artifacts/sha256/<hex>`，单对象 ≤32 MiB），用于大 Tool Result、补丁和转录。`context.compacted` 只追加摘要，不删除原始事件。

### 6.3 副作用状态机

以下是工具副作用的逻辑状态图；`OutcomeUnknown` 在当前实现中主要通过错误与恢复边界表达，不是每次都追加一个同名事件：

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

调用身份由 Session、Turn 和 Call ID 共同约束；模型工具调用必须有耐久的 tool-call ID，Eino Adapter 不为缺失 ID 静默生成新调用。`tool.Pipeline` 按工具、revision、参数和原结果核对回放，冲突返回 `ErrCallConflict`。已记录的成功或模型可见失败可以重放，不能把“存在 started、缺少确定结果”当作未执行。

ATE 存储请求有自己的准确 RequestKey/digest 与核验流程；其提交回执只证明存储命令成功，不证明文件、Shell 或模型请求从未重复。副作用结果未知时由 `OutcomeUnknownError` 等边界阻止盲目重做，领域回执与显式恢复负责后续处理。

### 6.4 回退语义

动态策略或 Tool 失败时：

1. 持久化失败事件。
2. 标记当前调用结果，不删除历史。
3. 从最后一个安全 Checkpoint 恢复 Base Agent。
4. 将已成功 Tool Result 重新提供给 Base Agent。
5. 不重新执行已提交的副作用。

因此，“回退”是恢复控制流，而不是假装物理世界已经回滚。

实际实现：Tool 执行错误、审批拒绝和前置条件错误作为结构化 Tool 结果返回模型，由同一 Eino Loop 自行修正；致命错误使 Turn 失败（`turn.failed`），中断的 Turn 可经 checkpoint `Resume`（`chat --resume-turn`）恢复，或经 `chat --abandon-turn … --confirm-abandon` 显式关闭。策略候选的 Turn 失败（非用户取消）只把后续 Turn 退回宿主基线并隔离该候选，不重放请求、不恢复文件。

### 6.5 存储实现

当前业务事实由 ATE 保存：会话使用原生 Session 接口，编排、用量与策略由宿主仓库写 state records，Memory/Knowledge 保存记忆与采用记录。本地 `runtime-v3.db` 只保存 checkpoint、工具快照及引用、authority 绑定和未决请求；工作区租约在独立协调库中。

旧业务数据库不自动导入或迁移。已有受支持的 ATE-linked runtime v1 库可原地打开并保留旧 schema 与历史资源行。未知或混合格式拒绝打开。具体检查见 [runtime_database.go](internal/runtimestore/runtime_database.go)。

- `gsea resume` 的会话目录、历史读取和选中会话校验共享一个 existing-only `SessionCatalog` ATE lease；`App.Open` 成功后接管同一 lease，统一负责最终关闭。`--list` 不创建聊天 App，lease 内 reconnect 也不代表第二个持久 owner。
- `App.WithStorageBatch` 只用于有界的同步存储 callback，最多执行一次，不提供跨仓库事务或自动重放。callback 不能等待模型、网络、用户输入或长期 worker，也不能保留批次 context；结果未知时先让 lease 失效，再在 owner 释放后独立核验。
- Session append 使用期望序号与原请求身份；业务 state batch 使用 authority CAS。两个提交边界不能混称一个事务。
- artifact 先写临时文件、同步并原子重命名，再允许事件引用其 Hash。
- ATE 写入结果未知时保留准确 envelope，按原身份对账；查询不存在不授权重发。
- `maintenance archive` 先取得 ATE 关闭回执，再释放本地工具快照引用，不删除会话历史或记忆来源。
- `maintenance compact` 检查本地 SQLite 并归还空闲页，不执行 artifact mark-and-sweep，也不删除 ATE 记忆。停止复用经验须显式维护来源。

---

## 7. Tool Registry 与统一执行管线

### 7.1 Tool 类型

Tool Registry 对 Eino 暴露统一 Tool 抽象。当前执行器实现 `tool.Executor`，包括原生 Go 工具（`tool.InvokeFunc`，如 `internal/codingtools`、记忆工具、`internal/evolutiontools`、`internal/agentmanager`）、评测用 `internal/toolbridge`（Unix socket 转发外部 benchmark 工具），以及 `internal/agentflow`（Flow 调用）。MCP 工具未实现。子 Agent 不是包装成单个 AgentTool，而是由 `agent_*`/`task_*` 等控制工具交给 Supervisor 调度（见 [Multi-Agent 架构](MULTI_AGENT_ARCHITECTURE.md)）。

普通新调用经过统一执行管线；已提交结果与等待续行有专门的回放路径：

```text
Validate Call / Resolve Snapshot Binding / Guards
→ Replay Durable Outcome if Present
→ Persist tool.proposed
→ Input Schema / Preconditions
→ Policy Decision
→ Optional Human Approval
→ Persist tool.started
→ Execute
→ Check JSON Result
→ Persist terminal event
→ Return to Eino
```

当前 `tool.Pipeline.Execute` 会实际校验输入 Schema；各工具还检查操作相关字段与业务条件。`NormalizedArg` 为原参数副本，不表示做过通用语义规范化；结果需为合法 JSON，没有独立输出 Schema 契约。失败必须成功持久化才能作为普通工具错误返回，持久化失败与未知结果不能被错误信封掩盖。详细顺序见 [pipeline.go](internal/tool/pipeline.go)。

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

早期设想的成功信封（未采用）：

```json
{
  "ok": true,
  "output": {},
  "artifacts": [
    { "sha256": "...", "mediaType": "text/plain", "name": "result.txt" }
  ],
  "metrics": { "durationMs": 12 }
}
```

早期设想的失败信封（未采用）：

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

当前未采用 `ok/output/artifacts/metrics` 信封。成功时把执行器返回的 JSON 原样交给模型；已持久的审批拒绝、策略拒绝、前置条件错误和执行错误可转换为 `{"error": ..., "message": ...}`，没有通用 `retryable` 字段。拒绝若伴随持久化错误，仍须传播失败，不能只返回拒绝文本。无审批器、`OutcomeUnknownError` 等错误交由运行时处理；WaitSuspended 走 Eino 的持久中断路径，不等于持久审批。大结果外置发生在上下文管理层。详见 [tool.go](internal/einoadapter/tool.go)。

---

## 13. Policy 与 Human-in-the-loop

### 13.1 决策输入

```text
Subject:
  session_id
  agent_id
  tool_name
  tool_revision

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

当前 `policy.Request` 带 Actor、Execution、Session、Call、工具 revision、Capability、Resource、RiskClass 与参数副本。`StaticEngine` 按 `ToolPattern`/`CapabilityPattern` 和 `ResourcePrefix` 首条匹配，缺省拒绝；规则由宿主装配，没有通用用户策略文件，也不把历史审批当作新的授权。具体路径、命令和工作区约束还由各工具与 OS 沙箱实施。见 [policy.go](internal/policy/policy.go)。

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

上面的持久授权条件是早期设想。当前 `policy.Effect` 只有 `allow`、`deny`、`require_approval`，没有 `AllowWithConstraints`。审批是一次性的：请求展示该次工具、revision 与参数，决定写入审计事件，但不作为跨调用复用的授权，不提供持久授权的有效期或次数管理。

### 13.3 中断与恢复

审批由宿主 Tool Pipeline 与 CLI/TUI 处理。早期的 Eino Interrupt 持久审批设想未落地，不能承诺等待中的审批跨进程自动续接。

实际实现：未使用 Eino Interrupt/Resume 做审批。Pipeline 写 `approval.requested` 后同步调用 `policy.Approver`：交互模式为 `approval.Broker`（请求 ID `approval-<base36>`，队列 16，UI 调 `Resolve` 唤醒等待的执行 goroutine，`Close` 时释放全部等待者并使其失败），非交互模式为 `DenyApprover`（直接拒绝），`--evaluation-mode` 为工具白名单审批器，无审批器时返回 `policy.ErrApprovalUnavailable`。审批结果写 `approval.resolved`。审批状态只在内存中，不跨进程保留；进程在等待审批时退出，该 Turn 成为中断 Turn，需经 `--resume-turn`（从 checkpoint 恢复）或 `--abandon-turn` 显式处理。

### 13.4 Tool Result 不可信

文件、网页和外部 Tool Result 都可能包含 Prompt Injection。宿主必须：

- 标记数据来源和信任级别。
- 不把 Tool Result 当成系统指令。
- 不允许 Tool Result 修改 Policy。
- 对外部内容触发的敏感操作重新审批。

实际实现：没有通用的 Tool Result 来源/信任级别标记；Tool Result 以 Tool 消息进入上下文，策略为静态规则不受其影响，敏感工具每次调用都需审批。子 Agent 消息、邮箱与父上下文摘录以 “Untrusted …” 标题注入。

---

## 14. 独立的宿主策略能力

### 14.1 第一版使用 LoopSpec，不生成 Loop 代码

“LoopSpec”是 `internal/strategy.Spec`（JSON），字段为 `version`（=1）、`revision`（≤128 字节）、`owner_session`、`instruction_suffix`（≤8192 字节）、`max_iterations`（≤100）、`max_output_tokens`（≤32768）。它只能在宿主基线指令后追加文本、调低迭代与输出上限，不能更换工具集、模型路由、上下文策略、审批策略或失败策略（也没有 Middleware 可选）。Spec ID 为规范化 JSON 的 SHA-256；`turn.started` 记录 `loop_spec_id` 与 `base_execution_config_id`，恢复从不可变存储载入原规格。

保留的可用示例为 [strategy.json](examples/evolution/strategy.json) 和 [shadow-suite.json](examples/evolution/shadow-suite.json)。它们用于有界策略比较，不是 ATE 记忆候选的验证/晋升工具。

### 14.2 LoopSpec 激活

候选在用户登记的 shadow suite（1–8 个 fixture，至少一个 `safety` fixture，期望输出精确字符串匹配）上与基线成对运行，空 Registry、`DefaultDeny`，单例 30s、总计 2 分钟，消耗当前 Team 预算；报告记录通过、模型调用数、输出字节与耗时。发布要求完整比较、无执行失败、全部 safety fixture 通过、候选通过数 ≥ 基线、候选模型调用数 ≤ 基线。没有自动激活：CLI 显式确认或 `strategy_publish` 经审批，通过 ATE authority CAS 发布，只影响后续 Turn。

候选 Turn 非用户取消失败时，宿主尝试将仍为同一 active 候选的策略退回基线并隔离该 revision；旧 Turn 不能撤销较新的候选。失败事件与回退分别提交，都可能失败，不重放当前 Turn 或回滚文件。完整命令及限制见[策略比较](EVOLUTION.md#独立的宿主能力loopspec-策略比较)。

### 14.3 不生成新的执行循环

Eino 负责单个 Turn 的模型—工具循环；Supervisor、Flow 与执行契约负责宿主编排。

---

## 15. 自进化分层

现行 Evolving 指有来源、可纠正的 ATE 跨任务记忆。

### 15.1 记忆查询与维护

- `memory_query`：`directory`、`count`、`search`、`read`。搜索与计数只包含当前可复用记忆；精确引用可读取允许披露的候选。
- `memory_manage`：`create_candidate`、`adopt`、`withdraw_source`、`correct_source`，写操作需审批。
- 候选创建只记录主张和来源，不验证、不晋升、不自动采用；普通工具不支持 `validate` 或 `promote`。
- 来源纠正/撤回禁止相应来源继续复用，保留历史；新结论需另建候选。

普通 CLI 不在每轮自动检索或成功后自动提炼。可选的 `Prepare` / `Check` / `Complete` 由嵌入宿主显式接入；其审查不能冒充工具测试成功。Skills 与 AGENTS.md 加载不是本记忆工具协议的能力。

### 15.2 策略与宿主升级

LoopSpec 比较是独立的宿主配置能力（见第 14 节），不代表记忆已验证。宿主代码及其信任边界通过正常软件开发、审核与发布变更，不由运行中的 Agent 自动替换。

---

## 16. 本地文件系统布局

`GSEA_HOME` 缺省为 `~/.gsea`。运行资源与业务存储分离：

```text
${GSEA_HOME}/
├── config.toml                # 用户配置；另可有 <profile>.config.toml（-p/--profile）
├── runtime-v3.db              # 缺省 data_directory=GSEA_HOME 时的运行资源库
├── artifacts/sha256/<hex>     # 大 Tool 结果等内容寻址 artifact
├── views/                     # Multi-Agent 工作区视图
├── sessions/<id>/{data,tmp,tool-state}   # `session` 命令创建的托管 Session
└── workspaces/<identity>/coord.db        # 跨 Session 工作区协调
```

`runtime-v3.db`、`artifacts/`、`views/` 位于 `data_directory`（缺省即 `GSEA_HOME`，托管 Session 为 `sessions/<id>/data`）。ATE 目录由 `[ate] directory` / `GSEA_ATE_DIRECTORY` 配置，可供多个项目共享；Session 目录、历史、策略和记忆均由 ATE 管理，旧本地会话库不作为后备源。

项目配置为最近祖先目录的 `.gsea/config.toml`，需要显式授信。跨 `GSEA_HOME` 的协调目录来自 `GSEA_COORDINATION_DIR` 或用户状态目录下的 `gsea/coordination`。凭证未接入 Keychain：配置只保存环境变量名（`[model] api_key_environment`，缺省 `OPENAI_API_KEY`），运行时从该环境变量读取；含凭证的 base URL 被拒绝。

---

## 17. 可观测性

运行记录与实时观察按 Session/Turn/请求身份关联，重点区分：

- Session、Turn、Step。
- Model Provider、Model 和请求序号。
- Runtime Snapshot。
- Tool Revision。
- Policy Decision 与 Approval。
- ATE 操作阶段与原请求回执。
- Token、费用、延迟、错误类别。

日志分级：

- Runtime Log：调试和运行状态。
- Audit Log：权限、审批、策略发布和副作用。
- Session Event Log：可恢复的业务事实。

三者用途不同，不能混用。日志中的 Prompt、Tool Result 和路径可能包含敏感信息，导出前必须脱敏。

上述日志分级是概念区分，不表示存在三份对应日志文件。当前事实来自 ATE Session、编排与策略事件、用量记录；TUI 阶段通知是实时观察，不改写事实。入口包括 `session`、`control`、`doctor`、`strategy report`，详见[执行观察](docs/EXECUTION_OBSERVATION.md)与 [ATE 接入](docs/ATE_INTEGRATION.md)。不能把阶段标签或历史测试耗时当作本次请求成功的证据。

---

## 18. 故障处理矩阵

| 故障                                | 当前边界                                                                                               |
| ----------------------------------- | ------------------------------------------------------------------------------------------------------ |
| 模型网络失败、认证失败或断流        | 保存失败与诊断，按故障类型失败或暂停；没有通用 provider failover。是否能续跑要检查原 Turn 的恢复条件。 |
| 工具领域错误或审批拒绝              | 已持久的可恢复错误交回同一 Eino Loop；持久化失败不能伪装成普通工具失败。                               |
| 工具副作用结果未知                  | 暂停/终止当前路径，保留原身份核对；不能自动重做副作用。                                                |
| ATE 请求结果未知                    | 保存原 envelope 并查询回执；查无回执仍未知，不换 ID 重发。                                             |
| LoopSpec 候选 Turn 失败             | 独立尝试策略回退，只影响后续 Turn；不回滚文件或重跑当前 Turn。                                         |
| checkpoint、快照或日志不完整/不兼容 | 拒绝不安全恢复，不用当前配置冒充旧版本，也不承诺自动重建。                                             |
| 记忆来源失效                        | 不再作为可复用经验；维护需审批并保留撤回/纠正记录。                                                    |

特定的请求核验、格式修复与日志回放不等于对所有错误统一自动重试。恢复细节以[执行观察](docs/EXECUTION_OBSERVATION.md)、[架构导读](ARCHITECTURE.md)和对应测试为准。

---

## 19. 测试与验收

### 19.1 单元与契约测试

- Eino Tool Adapter 的 Schema 和错误映射。
- Policy Decision 的路径、Host、命令和 Revision 约束。
- Session Event 的序列、校验和与重放。
- 工具快照清单、实现身份与恢复引用。
- 记忆查询范围、候选来源、采用回执与来源撤回。
- LoopSpec Schema 与 Eino 配置映射。

### 19.2 必须通过的故障测试

1. Tool 已产生副作用但宿主随后崩溃，恢复后进入 `OutcomeUnknown` 而不是自动重跑。
2. 审批等待期间退出进程，恢复按原 Turn 身份重新检查，不继承未完成审批。
3. 不可信历史或工具内容不能绕过 Policy 获得文件写入或 Shell 权限。
4. 原始 ATE 请求丢失响应后，按准确身份核验；未知结果不被当作未执行。
5. 新建候选不进入可复用搜索/计数；普通工具不能验证或晋升候选。
6. 来源撤回后禁止复用；采用记录可追溯且不被当作任务验收成功。
7. 归档回执已提交而本地引用释放失败，只补做资源收尾，不重做业务关闭。

实际实现：审批不跨进程保留（见 13.3），中断 Turn 需经 `--resume-turn` 恢复。

### 19.3 Fuzz 与安全测试

- Fuzz 工具参数、记忆引用和请求 envelope。
- Fuzz Tool Schema 与 Go Request 映射。
- 随机取消模型、工具与 ATE 调用。
- 模拟磁盘满、数据库写失败和部分 Blob 损坏。
- 对路径规范化、符号链接和重定向策略做专门测试。

本小节是测试规划，不表示已经执行或覆盖全部场景。

### 19.4 性能基准

正式文档只记录实测数据：

- 宿主冷启动。
- Eino 首 Token 延迟。
- ATE 进程启动、握手、读取与提交延迟。
- 记忆查询、来源核验与采用记录开销。
- 工具参数和结果的序列化开销。
- 事件日志写入和恢复时间。
- 不同 Tool Result 大小下的内存峰值。

不预设 `<10ms`、`毫秒级完整自进化` 等未经验证的指标。

本文不提供新的性能验收结果；已有特定机器与数据规模的阶段测量见 [ATE 接入](docs/ATE_INTEGRATION.md)，不能外推为通用性能保证。

---

## 20. 早期路线图与现行范围

下列 Phase 编号仅用于对照早期设计，不是重新启动已退役功能的实施计划。当前待办以[功能范围与剩余工作](IMPLEMENTATION_REMAINING.md)为准。

### Phase 0：技术验证

保留 Eino ChatModelAgent、CLI 流式输出与原生工具管线的验证；原扩展运行时实验已退役。

### Phase 1：可用的本地 Agent

- ModelGateway：OpenAI Chat Completions / Responses + Ollama。
- Eino Base Agent 与 Runner；上下文管理由宿主实现，未采用 Eino Middleware。
- CLI/TUI、Session 管理和 Ctrl+C。
- 原生只读文件工具与 Policy-aware Backend。

现有能力：多轮本地编码辅助、原生工作区工具和审批边界。

### Phase 2：持久化与安全控制面

- Append-only Session Event Store。
- Checkpoint、Interrupt/Resume。
- Policy Engine、审批与 Audit Log。
- 副作用状态机、幂等键和 OutcomeUnknown 恢复。

现行存储已切换到 ATE。恢复要求 checkpoint、原身份与工具环境兼容；结果未知时停止自动重做，不能承诺任意边界都可恢复。持久审批与通用外部副作用 reconciliation 尚未落地。

### Phase 5：有界策略比较

- LoopSpec Schema、版本库和评测集。
- Shadow Session 与指标比较。
- Turn 边界原子切换。
- Base LoopSpec 自动回退。

当前以受限形式实现，见 14.1、14.2。smoke suite 的比较不是通用能力或安全认证；策略回退不撤销外部副作用。

路线图之外已有 Multi-Agent、执行契约与验收、Flow、评测和 ATE 记忆工具。普通 CLI 的记忆入口是主 Agent 按需调用，不是每轮自动提炼/验证/晋升闭环。

---

## 21. 跨任务记忆的验证切片

用两次独立任务验证事实、来源和采用边界：

1. 第一次任务用原生工具排查问题，ATE 保存模型、工具与用户输入事实。
2. 经审批用 `memory_manage/create_candidate` 提出主张，记录 `kind`、语义空间与原始事件引用。
3. 用 `memory_query/read` 检查候选，确认状态仍为候选，不计入可复用搜索与计数。
4. 只有经独立验证、晋升且当前仍符合复用策略的记忆，才作为第二次任务的搜索对象；普通两个工具不提供这一步的验证/晋升操作。
5. 第二次任务核验现场条件与来源，经审批显式 `adopt`，留下本次采用的 Knowledge observation。
6. 撤回或纠正失效来源，确认禁止继续复用但保留历史与采用记录。

这不是默认 CLI 能自动完成的全闭环，也不是新的测试通过声明。具体工具契约、嵌入宿主可选服务与验证入口见[跨任务记忆指南](EVOLUTION.md)。

---

## 22. 设计原则总结

- **Loop 复用 Eino，不自行实现通用 Agent Loop。**
- **ATE 保存权威事实，宿主掌握执行决策与副作用。**
- **记忆可以提供经验，不能给工具授权。**
- **模型可以提出候选，候选创建不等于验证、晋升或采用。**
- **策略选择固定于 Turn，恢复不套用今天的 active 配置。**
- **恢复基于事件和 Checkpoint，不基于重新执行用户 Prompt。**
- **来源撤回、会话归档与本地引用释放各有自己的回执，不提供物理世界事务回滚。**
- **性能和可靠性结论必须由测试证明。**

该架构保留了“自进化本地智能体”的核心价值，同时将最难且最关键的 Agent Loop、会话一致性、权限与副作用管理放在稳定、可审计的 Go Trusted Runtime 中。

---

## 23. 主要参考

- Go 1.27 Release Notes：<https://go.dev/doc/go1.27>
- Go 1.27 Release Announcement：<https://go.dev/blog/go1.27>
- Eino：<https://github.com/cloudwego/eino>
- Eino Documentation：<https://www.cloudwego.io/docs/eino/>
- DeepSeek Harness Architecture：<https://github.com/deepseek-ai/deepseek-harness/blob/master/docs/architecture.md>
- DeepSeek Harness HMR：<https://github.com/deepseek-ai/deepseek-harness/blob/master/docs/cordis-tutorial/06-composition-and-hmr.md>
