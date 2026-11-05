# openJiuwen 论文分析与项目落地

## 来源

- 论文：*openJiuwen: Beyond Static Harnesses for Long-Horizon Coding Agents*
- arXiv：2608.27969v1，2026-08-28
- 本地原文：`research/papers/arxiv-2608.27969v1.html`
- 源码链接（论文给出）：`https://github.com/openJiuwen-ai/jiuwenswarm`

本文档基于完整正文、公式、实验表格、限制和结论整理，不以摘要或关键词代替论文内容。

## 论文的核心判断

论文把长程 Coding Agent harness 的问题分成两个维度：

1. **Structural Composability**：单 Agent、委派子 Agent 和多 Agent 流程应复用同一执行语义，能力通过有序生命周期钩子组合，不为每种拓扑再造执行引擎。
2. **Runtime Adaptivity**：模型策略保持不变，框架根据执行中出现的新证据调整上下文、诊断反馈、完成判定和跨任务经验。

其执行结构由以下机制组成：

- Inner Loop：有界的 model -> tool -> observation ReAct 循环。
- Outer Loop：一次 Inner Loop 结束后，按语义完成、阻塞、资源上限或恢复条件决定是否继续。
- Rail：挂在 invoke/call/task 生命周期边界上的有优先级能力，可按 Agent 角色做可见性隔离。
- Swarm Flow：用 `budget`、`parallel`、`compact`、`pipeline`、`agent_session`、`human` 和 `return` 等算子组合多 Agent 流程。
- Context Management：渐进压缩、结构化裁剪、死循环折叠、artifact 外置和按需检索。
- Goal Mode：将 `continue`、`complete`、`blocked` 与时间/token/尝试次数等硬上限分开。
- LSP-Driven Passive Feedback：代码变化后自动产生语义诊断，经过排序、去重和限量后进入下一步执行。
- Self-Reflection：任务结束后提炼经验，验证、去重、合并后持久化，并在未来任务中检索使用。

## 实验证据及其边界

论文报告：

- SWE-bench Verified：500 个任务，openJiuwen + Claude Opus 4.5 high 的 Pass@1 为 82.6%。
- Terminal-Bench 2.1：89 个任务，openJiuwen + GPT-5.6 Sol 为 87.19%，openJiuwen + Fable 5 为 84.04%。
- 在 15 分钟到 1 小时的 SWE-bench 分桶中为 81.23%；在 1 到 4 小时分桶中为 52.38%。
- Terminal-Bench 的 file-operations 和 system-administration 分类表现较强，作者认为统一且丰富的工具接口可能是原因之一。

这些数字只能证明整套 harness 的系统级表现，不能证明任一单项机制单独带来多少收益。论文没有给出 Rail、Goal Mode、LSP、Context Management 或 Self-Reflection 的独立消融；不同榜单系统之间还存在模型、prompt、工具和实现差异。论文也在 Limitations 中明确提出需要更细的消融和更多模型/基准。因此本项目只优先吸收具备清晰工程闭环、可独立测试且不破坏现有边界的机制。

## 与当前项目的对应关系

| 论文机制 | 当前项目基础 | 决策 |
| --- | --- | --- |
| Inner Loop | Eino `ChatModelAgent` / `Runner` | 已有，继续复用 |
| Outer Loop | `Conversation`、Eino Turn、Supervisor/Wait 恢复 | 已实现：一个 Eino Inner Loop 对应一个 round，`Conversation` 依据 evaluator verdict 继续或停止 |
| Rail | Tool Registry、Pipeline、Policy、EventSink、Agent Profile | 不照搬通用 Rail 总线；将思想落实为 `ChangeSetDiagnoser`、`ContextHints`、`ContextArtifactStore` 等类型化 capability，由 Composition Root 挂载 |
| Swarm Flow | Agent Spawn、调度器、Task DAG、Mailbox、WaitSet、Reviewer、预算账本 | 已实现完整 operator substrate 并编译到既有控制面；显式 revision 与 topology fingerprint 约束恢复 |
| Context Management | token 阈值压缩、durable compaction、artifact store | 已实现 token/budget 分级、Goal 锚点、结构化缩减、循环折叠、artifact 自动外置与 `artifact_read` 按需召回 |
| Goal Mode | Task 状态机、依赖阻塞、reviewer verification gate、资源上限 | 已实现 durable Goal/round/report、self/independent/hybrid evaluator 和独立 hard-cap 终态 |
| LSP Passive Feedback | 原先没有 | 本轮实现 |
| Self-Reflection | durable session 和 artifact 基础 | 暂不自动写入“经验”；缺少验证、去重、修订和检索质量门槛 |

## 本轮落地：LSP 闭环语义反馈

新增 `internal/lsp`，以进程级服务实现标准 LSP stdio/JSON-RPC 生命周期：

```text
workspace_write / workspace_edit / shell_exec
        |
        v
原子写入或命令完成（仍在 workspace lease 内）
        |
        v
bounded ChangeSet：created / modified / deleted
        |
        v
LSP Manager：按 workspace/worktree 选择 client
        |
        v
initialize -> initialized -> didOpen/didChange
        |
        v
publishDiagnostics
        |
        v
changed-file 优先 -> severity 排序 -> 去重
        -> 单文件上限 -> 总量上限 -> SQLite revision projection
        |
        v
合并到原工具 JSON result，供同一 ReAct turn 的下一模型步骤使用
```

关键语义：

- 默认使用 `gopls serve`，仅在首次分析受支持文件时懒启动。
- shared workspace 复用一个 client；Git worktree 使用独立 client，避免不同代码状态共享 document state。
- 支持 `.go`、`go.mod` 和 `go.work`。
- diagnostics 使用 workspace-relative path，并把 LSP 的零基位置转换为便于模型读取的一基行列。
- 结果按 changed-file、error/warning/information/hint、路径和位置稳定排序。
- 完全相同的 path/range/severity/code/source/message 会被去重。
- 默认每文件最多 5 条，总共最多 20 条，超出时返回 `truncated: true`。
- `workspace_diagnostics` 提供主动查询，和其他工具一样经过 Registry、Policy、Event Store 与 Tool Pipeline。
- `shell_exec` 在命令前后扫描最多 50,000 个 workspace 文件，返回最多 2,000 个稳定排序的变化，并将最多 128 个受支持文件作为一个诊断批次；达到边界会显式返回 `truncated`，不会伪装成完整覆盖。
- diagnostics 按 canonical workspace root、相对路径和内容 SHA-256 revision 保存到 SQLite；语言服务器暂时无法启动时，同一 revision 可以恢复为带 `cached: true` 的 partial 报告。
- 语言服务器失败、超时或退出会返回 `partial`/`unavailable`，不会把已经成功的文件写入变成错误，避免 Agent 对副作用工具进行危险重试。
- App 关闭时先终止所有语言服务器，再释放底层数据库资源。

默认配置：

```toml
[lsp]
enabled = true
command = "gopls"
args = ["serve"]
diagnostic_timeout = "3s"
max_diagnostics_per_file = 5
max_diagnostics_total = 20
```

## 当前限制

- shell ChangeSet 为有界快照比较：Go 源文件额外使用内容 SHA-256，可识别大小和 mtime 不变的改写；非 LSP 文件主要比较 size/mtime/mode。超过文件或诊断批次上限会明确标记不完整。
- 当前只配置 Go/gopls。Manager 的协议实现可复用，但不同语言仍需要明确的扩展名、language ID、命令和 workspace 生命周期配置，不能假设所有 server 行为相同。
- LSP 只证明 language server 能观察到的类型、语法、符号和静态分析问题；它不证明业务逻辑、架构质量、可维护性或端到端任务完成。
- 首次启动语言服务器可能增加一次写工具延迟；后续变更复用同一进程。

## 本轮落地：动态 Context Management

Context Policy 保持 append-only 原始历史不变，只调整下一次模型输入，并把决定写入 `context.managed`：

- 先按 token 压力和 durable Team token/cost budget 选择 normal/elevated/critical 级别；预算使用达到限制的 80% 时可提前压缩。
- 超过 24 KiB 的工具结果自动写入 content-addressed Artifact Store，模型上下文保留 SHA-256 reference、原始字节数与首尾预览。
- `artifact_read` 经过 Tool Pipeline，以 offset 和最多 1 MiB 的窗口分段召回；不会把 artifact 全量重新塞回上下文。
- Outer Loop 通过可信 `ContextHints` 提供 active Goal ID、原文与 round；压缩后的 system summary 始终带不可丢失的 Goal 锚点。
- 单条超过 32 KiB 的旧消息保留首部和 8 KiB 尾部，中段替换为明确的省略字节数标记。
- 原始完整内容仍在 append-only session history 中，不重写历史。
- 连续完全相同的消息会折叠；重复的 assistant tool-call + tool result 两消息模式也会折叠。
- 工具循环的相等判断忽略重试产生的新 tool-call ID，但保留工具名、参数和结果内容，避免把不同操作误判为同一循环。
- critical 压力下如果语义摘要模型失败，使用有明确说明的近期 durable entry 抽取式 fallback，避免上下文维护故障直接终止长任务。

这对应论文的 structure-aware reduction 与 dead-loop collapse，同时修复了一个实际风险：上下文已经接近窗口上限时，再把超大历史原样提交给摘要模型，摘要请求本身可能继续超限。

## 本轮落地：Inner/Outer Loop 与 Goal Mode

- Eino `ChatModelAgent` 的 model -> tool -> observation 循环保持为有界 Inner Loop；没有再造第二套工具执行引擎。
- `Conversation` 将一个用户目标持久化为 `goal_runs`，每次 Inner Loop 预先登记 `goal_rounds`，等待恢复后仍回到同一 Goal/round。
- acting Agent 必须通过可信 Tool Pipeline 中的 `goal_report` 提交 self assessment；goal/round/turn 身份由 Pipeline context 注入，模型参数不能伪造。
- assessment 支持 `self`、独立模型 evaluator 和要求双方对 terminal verdict 一致的 `hybrid`。后续工程修订增加默认 `adaptive`：机械验证覆盖完整契约时可直接接受，否则使用语义评估。独立 evaluator 格式错误重试一次，仍失败则报告评估失败，不再将基础设施错误转换成重新执行任务的 `continue`。
- `complete`/`blocked` 是语义 verdict；`max_rounds`、`max_wall_time`、`budget_exhausted` 是独立 hard-cap stop reason，最终状态为 `exhausted`。被拒绝 candidate 不进入 live 正文输出。
- Goal 的 blocked/partial 结果会投影到 durable Agent result；关联 Task 进入 `blocked` 而不是被误标为 completed。

### 2026-09-09：任务准入与验收边界修订

论文 §3.1.1 将工具交互放在 Inner Loop，§3.2.3 将 Goal Mode 定义为任务级验收与停止控制；§3.2.4 的 LSP 反馈不等于端到端任务完成。论文没有要求每条用户消息都开启 Goal Mode，也没有规定关键词路由、强制 hybrid 或隐藏评估进度。

本项目因此采用显式 `goal_start` 契约准入，普通问答可以使用读取工具而不登记 Goal；只读但要求完整覆盖的任务也可以登记。准入、机械验证、语义评估、Outer Loop 继续控制分开实现，Eino 仍是唯一 model/tool 循环。精确文件内容是首个机械验证类型，测试/业务逻辑/覆盖性等仍需要合适证据与语义评估，不能用文件匹配替代。上述默认策略、UI 阶段通知和实际截止时间是本项目的产品工程选择，不应声称是论文直接要求或已由其分数证明的独立收益。

## 本轮落地：Composable Flow

`internal/agentflow` 提供宿主组合、Agent 调用的 Flow substrate：

```text
host-registered named Flow
        |
        v
flow_run (Tool Pipeline)
        |
        +--> budget ------> runtime_budget (Tool Pipeline)
        +--> parallel ----> result-ready streaming
        +--> compact -----> drop failed/null/empty branches
        +--> agent_session -> agent_spawn / agent_continue / agent_wait
        +--> human -------> human_confirm approval
        +--> tool --------> any registered tool
        +--> return ------> cancel remaining downstream work
```

关键语义：

- Agent 只能 `flow_list`/`flow_run` 选择宿主 Catalog 中的命名 Flow，不能上传任意 operator graph 或 callback。
- 每个定义必须声明 revision；Runtime 对 operator kind、stage/branch identity、顺序和并发度生成 topology fingerprint。revision/fingerprint 均进入 idempotency hash、SQLite run 和事件，恢复时不允许静默漂移。
- Flow run、step 和 stateful Agent session-stage 均有 SQLite checkpoint；同一 stage 恢复会重放原 spawn/continue，后续 stage 才会继续同一 child thread。
- `parallel` 不做 join barrier；分支结果完成后立即进入下游 stage。`compact` 的含义是过滤失败与空分支，不是 context compaction。
- 每个外部动作都使用确定性 call ID 回到 Tool Pipeline，继承发起者的 Actor Scope、session、registry generation、policy、approval 和事件记录。交互式允许与 headless 拒绝不会因嵌套 Flow 改变。
- Pipeline 会按 session+turn+call 回放 durable success result；Flow 在“工具成功、step 尚未完成”后恢复时不重复副作用，跨 turn 复用 provider call ID 则仍是新调用。

Catalog 只保存可信进程内定义，运行状态和 checkpoint 保存在 SQLite。进程重启后宿主必须重新注册同名且同 revision/fingerprint 的定义；callback 语义变化必须主动升级 revision，不会把 Go callback 或任意模型生成代码反序列化为可执行流程。

## 后续吸收顺序

1. 增加 definition、references、hover 和 call hierarchy 等主动只读语义工具。
2. 为 Context Policy 增加摘要忠实度、artifact 召回命中、token 节省和任务成功率指标；以数据校准阈值，而不是继续堆叠启发式规则。
3. 为 Goal/Flow 增加 benchmark telemetry 和按机制消融，确认额外 evaluator 调用、并行宽度和恢复策略对真实任务成功率/成本的影响。
4. Self-Reflection 只有在经验具备来源、适用范围、验证状态、冲突处理和淘汰机制后再进入持久化与检索，避免错误经验污染未来任务。
