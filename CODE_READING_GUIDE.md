# Go-Self-Evolving-Agent 代码阅读与审计指南

本文给出读懂、审计本项目的阅读路径与原始工作单。文件数、行数和附录清单保留首次编写时的规模快照，后续修复增加的文件以当前源码为准。

**2026-10-26 修复更新：** A–E 已逐项复核并实施修复，当前结论、证据、兼容性变化和未完成边界见 [AUDIT_REMEDIATION.md](AUDIT_REMEDIATION.md)。本文各包的原始“发现”和下方 A–E 保留为历史审计线索，不能继续当作当前未修问题列表；尤其应先读新的 `tool/schema.go`、`codingtools/root_io.go`、`workspacecoord/finalization.go`、`sqlitestore/merge_apply_finalization.go`、`pluginbuild/toolchain.go` 和相应回归测试。

## 0. 如何使用本文

- 每个包的小节格式基本一致：职责与项目内依赖 → 推荐阅读顺序（文件名后括号内是 wc -l 行数）→ 审计点 → 测试 → 跨包调用。
- "必读"列出读完该文件必须能讲清的类型和函数；"审计"列出需要逐行核对的边界、锁、错误语义、事务和幂等点。
- ★ 表示值得和源码并排读的测试。测试行为大多按测试名和部分测试体整理，读的时候以测试代码为准。
- 原始标注"未确认"的内容是静态阅读时未能闭环的推断；后续已执行测试与复核，按 [修复记录](AUDIT_REMEDIATION.md) 判断其当前状态，不能把原推断当作已复现缺陷。
- 读法建议：每个文件先读类型和导出函数签名，再读主流程，最后读错误分支；读完一个包，用第 4 节的自测问题检查，再跑一遍该包测试（`go test ./internal/<pkg>/...`）。
- 端到端调用链（第 5 节索引）是把分散的包串起来的主线。建议每读完一个阶段，就沿相关调用链在源码里走一遍。

## 1. 项目规模（首次编写快照）

Go 1.27，模块 `github.com/vkviyu/go-self-evolving-agent`，基于 cloudwego/eino ADK 的本地优先 Agent。源文件 326 个、49,314 行；测试 175 个、27,309 行。按目录统计：

| 包（目录） | 源文件数 | 源码行 | 测试文件数 | 测试行 |
|---|---:|---:|---:|---:|
| `cmd/local-agent` | 1 | 15 | 0 | 0 |
| `internal/agentflow` | 5 | 1511 | 2 | 708 |
| `internal/agentgoal` | 7 | 746 | 5 | 289 |
| `internal/agentmanager` | 19 | 3731 | 6 | 2488 |
| `internal/agentprofile` | 2 | 316 | 2 | 163 |
| `internal/agentruntime` | 36 | 4391 | 22 | 3176 |
| `internal/app` | 22 | 1625 | 16 | 1647 |
| `internal/approval` | 2 | 243 | 2 | 188 |
| `internal/architecture` | 0 | 0 | 1 | 242 |
| `internal/artifact` | 1 | 240 | 1 | 46 |
| `internal/cli` | 22 | 3259 | 17 | 2274 |
| `internal/codingtools` | 12 | 1800 | 3 | 770 |
| `internal/config` | 2 | 972 | 2 | 518 |
| `internal/conversation` | 15 | 1053 | 10 | 1556 |
| `internal/einoadapter` | 1 | 147 | 1 | 183 |
| `internal/evaluation` | 9 | 2184 | 3 | 670 |
| `internal/evolutiontools` | 7 | 374 | 2 | 420 |
| `internal/lsp` | 4 | 1279 | 1 | 262 |
| `internal/modelcontext` | 1 | 151 | 1 | 122 |
| `internal/modelgateway` | 7 | 1410 | 5 | 811 |
| `internal/orchestration` | 9 | 1244 | 4 | 135 |
| `internal/plugin` | 5 | 807 | 5 | 863 |
| `internal/plugin/wasm` | 2 | 492 | 2 | 128 |
| `internal/plugin/wasm/testdata/echo` | 1 | 23 | 0 | 0 |
| `internal/pluginbuild` | 4 | 412 | 2 | 321 |
| `internal/policy` | 1 | 137 | 1 | 77 |
| `internal/render` | 2 | 318 | 1 | 119 |
| `internal/retention` | 2 | 79 | 0 | 0 |
| `internal/runtimeowner` | 3 | 111 | 1 | 149 |
| `internal/sandbox` | 4 | 490 | 3 | 293 |
| `internal/scheduler` | 1 | 112 | 1 | 31 |
| `internal/session` | 5 | 262 | 2 | 93 |
| `internal/session/memorystore` | 1 | 171 | 0 | 0 |
| `internal/session/sqlitestore` | 43 | 7814 | 22 | 3923 |
| `internal/sessionregistry` | 1 | 231 | 1 | 66 |
| `internal/strategy` | 3 | 243 | 1 | 183 |
| `internal/tokenizer` | 1 | 312 | 1 | 61 |
| `internal/tokenusage` | 1 | 230 | 1 | 39 |
| `internal/tool` | 9 | 1401 | 7 | 968 |
| `internal/toolbridge` | 1 | 169 | 1 | 75 |
| `internal/tui` | 21 | 3938 | 9 | 1822 |
| `internal/workspacecoord` | 18 | 3352 | 6 | 992 |
| `internal/workspacedomain` | 10 | 1395 | 2 | 438 |
| `sdk/pluginabi` | 3 | 124 | 0 | 0 |
| **合计** | 326 | 49314 | 175 | 27309 |

行数最多的五个包是 sqlitestore（7,814）、agentruntime（4,391）、tui（3,938）、agentmanager（3,731）、workspacecoord（3,352），合计约占源码的 47%，也是审计投入最大的地方。

## 2. 读代码前先读的文档

按顺序读，先建立概念再进代码：
1. `README.md`（422）：功能、命令、配置项总览。
2. `ARCHITECTURE.md`（354）：主 SQLite owner（另有 control.db 与 coord.db）、事件溯源、tool 流水线、Goal Mode、上下文压缩的设计说明。
3. `EVOLUTION.md`（92）与 `TOOL_DISCOVERY_DESIGN.md`（741）：插件自进化的运维流程；工具发现仍是待实施设计。
4. `MULTI_AGENT_ARCHITECTURE.md`（2214）：多 Agent、mailbox、WaitSet、Task DAG、快照合并。篇幅大，可以在第 5 阶段开始前再读。
5. 选读：`MULTI_AGENT_PARALLEL_PLAN.md`（442）、`REFACTORING.md`（89）、`IMPLEMENTATION_REMAINING.md`（57）、`自进化本地智能体（Self-Evolving Local Agent）.md`（1467，基线设计）、`evals/README.md`（492）。
6. `internal/architecture/architecture_test.go`（242）：分层规则的可执行定义（见第 6 阶段的 internal/architecture 小节）。在读任何包之前先读它，后面判断"某个依赖是否合法"都以它为准。

以上文档已于 2026-10-26 按代码逐节校准：已实现的描述改为实际行为，设计与代码偏离处标注"实际实现：…"，未落地内容标注"未实现/计划中"，进度段更新到 2026-10-26。设计正文仍保留原始意图，读到没有标注的设计段落时，仍以代码为准，差异记为审计发现。

## 3. 分层与依赖

下面是大致分层，箭头方向是"被依赖"。准确的依赖白名单见 architecture_test.go 的 domainDependencies。

```
第 6 层 组装与界面   cmd/local-agent → cli → app ；tui ；config
                       │（只有 app 能打开数据库、WASM、workspacedomain）
第 5 层 多 Agent      agentmanager  agentflow  workspacecoord  workspacedomain
第 4 层 运行时        conversation → agentgoal / agentruntime → modelgateway
第 3 层 工具与插件    codingtools sandbox lsp plugin plugin/wasm pluginbuild evolutiontools toolbridge
第 2 层 工具契约      tool  einoadapter  approval  policy  agentprofile  retention  tokenusage
第 1 层 领域契约      session  session/memorystore  orchestration
第 0 层 无项目依赖    strategy tokenizer modelcontext scheduler artifact runtimeowner sessionregistry render sdk/pluginabi
持久化（横切）       session/sqlitestore：实现上面各层定义的仓储端口，只被 app 导入
评估（旁路）         evaluation：由 cli eval 子命令驱动，复用整个 Agent
```

几条贯穿全项目的约束，读每个包时都要对照：
- 单 SQLite owner：只有 sqlitestore 打开数据库，SetMaxOpenConns(1)，WAL，迁移 v1–v36 只增不改。跨进程互斥靠 runtimeowner 的 flock。
- 事件溯源：会话时间线是追加式事件（session 包），turn、工具、goal、上下文压缩都以事件落盘，恢复靠重放事件和 checkpoint。
- tool 流水线：proposed → policy → approval → started → succeeded/failed。有副作用的工具已 started 但没有成功记录时，重放返回 OutcomeUnknown，不重复执行。
- 工具集按 turn 冻结：新 turn 取 registry 快照，恢复的 turn 用原快照，所以新插件从下一个 turn 才可见。
- 沙箱：macOS 用 Seatbelt，Linux 用 bwrap。

## 4. 阅读阶段总览

自底向上读：先读契约，再读用契约的实现，最后读组装。每个阶段结束时能回答"自测问题"，再进入下一阶段。

| 阶段 | 内容 | 源码行 | 对应正文 |
|---|---|---:|---|
| 0 | 文档 + architecture_test.go | — | 第 2 节、第 6 阶段 internal/architecture |
| 1 | 基础包：session、orchestration、policy、tool、approval、einoadapter、agentprofile 等 18 个包 | ≈5,950 | 第 1 阶段 |
| 2 | 模型与运行时：modelgateway → agentruntime → agentgoal → conversation | ≈7,600 | 第 2 阶段 |
| 3 | 持久化：session/sqlitestore | 7,814 | 第 3 阶段 |
| 4 | 工具、沙箱、插件与评估：codingtools、sandbox、lsp、plugin、wasm、pluginabi、pluginbuild、evolutiontools、toolbridge、evaluation | ≈8,150 | 第 4 阶段 |
| 5 | 多 Agent：agentmanager、agentflow、workspacecoord、workspacedomain | ≈9,990 | 第 5 阶段 |
| 6 | 配置、组装与界面：config → app → cmd/cli → tui | ≈9,810 | 第 6 阶段 |

sqlitestore 实现了几乎所有阶段定义的仓储端口。建议第 3 阶段先读它的第 1 节（连接与事务模型）、A（打开与迁移）、B（session 事件）、H（usage 账本）、I（goals）和第 3 节迁移表；L（插件、strategy、LSP）放到第 4 阶段对照读；C–G、J、K（编排、agents、tasks、mailbox、wait、flows、merge）放到第 5 阶段对照读。

自测问题：
- 阶段 1：一个事件怎样带乐观序号追加，冲突时返回什么？工具调用从 proposed 到 succeeded 写了哪些事件，进程在 started 之后崩溃，重放时会发生什么？策略拒绝和审批拒绝分别怎样回到模型？
- 阶段 2：一次普通 turn 写了哪些事件、以什么顺序？checkpoint 何时写、何时删？上下文压缩的四个触发点分别写哪个版本的事件、何时生效？Goal Mode 的准入、轮次、评估、终止如何落盘，崩溃后每个阶段如何恢复且不重复执行？
- 阶段 3：为什么单连接下 helper 必须拿 tx？迁移 checksum 能保护哪些版本？哪些写操作不在一个事务里？
- 阶段 4：shell_exec 和 workspace_write 怎样约束路径、取得租约、进入沙箱？一个插件从 plugin_propose 到被调用要经过几次审批，为什么新插件下一个 turn 才可见？
- 阶段 5：子 Agent 的结果怎样回到父 Agent？快照视图的修改怎样经过三方合并、审批和 apply journal 回到主工作区，崩溃后怎样续提交？
- 阶段 6：OpenWithOptions 的 28 步各自依赖前面哪一步？Close 的顺序为什么是这样？TUI 中一个回合的事件怎样从 RunTurn 流到屏幕？

## 5. 端到端调用链索引

| 链 | 位置 |
|---|---|
| 进程启动（含 OpenWithOptions 28 步） | 第 6 阶段 · 入口、App 与 TUI 端到端调用链 (a) |
| TUI 中按 Enter 到流式渲染结束 | 同上 (b) |
| 关闭顺序 | 同上 (c) |
| mailbox 有消息时的后台 wake | 同上 (d) |
| 普通用户 turn（模型请求、工具、完成） | 第 2 阶段 · 运行时端到端调用链 (a) |
| 进程重启后恢复中断 turn | 同上 (b) |
| Goal Mode 一轮到验收判定 | 同上 (c) |
| 上下文压缩的四个触发点 | 同上，链 (c) 之后 |
| 模型调用内置工具（shell_exec / workspace_write） | 第 4 阶段 · 工具与插件端到端调用链 (a) |
| 插件自进化：候选 → 编译 → 测试 → 发布 → 下一 turn 调用 | 同上 (b) |
| evaluation 跑一个 benchmark task | 同上 (c) |
| spawn 子 Agent 到结果回到父 Agent | 第 5 阶段 · 多 Agent 端到端调用链 (a) |
| 快照隔离写入到合并回主工作区 | 同上 (b) |
| Task DAG 调度 | 同上 (c) |
| 多 Agent 进程重启 | 同上 (d) |

各包小节中的"跨包调用"给出了链上每一跳的具体文件和函数，沿链读时可以随时跳过去。

## 第 1 阶段：基础包

按本阶段末尾"基础包整体阅读顺序和理由"的顺序读：session → memorystore → orchestration → policy → tool → approval → einoadapter → agentprofile → modelcontext/tokenizer/tokenusage → scheduler/strategy → artifact/retention/runtimeowner/sessionregistry → render。下面按包排列，先浏览那一节再开始。

### internal/session（合计 355 行，非测试 262 行）
1. 职责：定义会话时间线的追加式事件契约（事件类型、乐观序号、存储接口），以及恢复墓碑检查。项目内依赖：无。
2. 阅读顺序
- `event.go（111）`：事件模型和 EventStore 端口。必读：ID、EventType 常量、AnySequence、ErrSequenceConflict、ErrInvalidEvent、PendingEvent.Validate、Event（Sequence/EventID/Checksum/SchemaVersion/RuntimeSnapshotID）、EventStore（Append/Load/CurrentSequence）、CorrelatedEventStore.LoadByCorrelation。审计：Append 的 expected 语义（AnySequence=^uint64(0) 跳过序号检查）；PendingEvent.Validate 只要求 Type 非空、Payload 非空且 json.Valid，不校验 CorrelationID；注释声明 LoadByCorrelation 只是优化，正确性不能依赖它，需核对每个调用方都有 VisitEvents 回退。
- `payload.go（24）`：JSONPayload、MustJSONPayload。审计：MustJSONPayload 会 panic，确认只用于不会序列化失败的结构。
- `checkpoint.go（12）`：CheckpointStore Get/Set/Delete，存 Eino 不透明字节。审计：注释要求检查点绝不能投影进模型历史。
- `visit.go（37）`：VisitEvents 分页遍历（visitPageSize=1000），ErrStopVisit 提前停止并返回 nil，其它 visitor 错误原样返回。审计：用 `after = event.Sequence` 推进，依赖 Load 按序号升序且连续；空页即结束。
- `recovery.go（78）`：AbandonRequest.Validate（turn/goal 恰好一个、EffectsReviewed 必须为真、Reason 非空且不超过 2048 字节、ID 不超过 4096）、Abandonment（EffectsReverted）、CheckRecovery、ErrRecoveryAbandoned/ErrRecoveryArchived/ErrRecoveryDependency。审计：CheckRecovery 先查 turn.archived 再查 turn.abandoned，按 CorrelationID==turnID 匹配；有索引走 LoadByCorrelation，否则走 VisitEvents；ErrRecoveryDependency 在本包内未使用（使用方未确认）。
3. 测试
- recovery_test.go（39）：索引路径和分页路径（pagedEvents 隐藏索引，1001+1 个事件）都能找到墓碑；无关 turn 不受阻；ctx 取消返回 context.Canceled。值得对照 recovery.go 读。
- visit_test.go（54）：2500 个事件跨页序号连续、在 1200 处提前停止、visitor 错误透传、空会话不回调。值得对照 visit.go 读。
4. 跨包调用：实现方 session/sqlitestore/events.go（SessionEventStore，LoadByCorrelation 在 :134）、sqlitestore/recovery_control.go:20 Database.AbandonRecovery；入口 app/recovery.go:20 AbandonRecovery、cli/chat_options.go:170 构造 AbandonRequest；agentruntime/turn.go 与 recovery.go 调 CheckRecovery；agentruntime 的 recovery.go、completed_turn.go、strategy.go、factory_snapshot.go 调 VisitEvents；tool/pipeline.go:correlatedEvents 优先走索引。被 agentruntime、sqlitestore、cli、app、tool、orchestration、conversation、agentgoal、agentflow、tui、retention、einoadapter 引用。

### internal/session/memorystore（171 行，全部非测试）
1. 职责：session.EventStore、CorrelatedEventStore、CheckpointStore 的内存实现，用于测试和临时影子执行。依赖：session。
2. `store.go（171）`：必读 Store、New、Append、Load、LoadByCorrelation、CurrentSequence、Get/Set/Delete。审计：一个 RWMutex 同时保护 events 和 checkpoints；Append 先检查 ctx 和 sessionID，在锁外校验全部事件，锁内做 expected 序号检查；Sequence=current+index+1，EventID 为 16 字节随机 hex，Checksum 固定为 "memory-store"，SchemaVersion=1，payload 复制；randomID 中途失败时，因为循环结束后才提交，结果全有或全无。Load 中 limit≤0 取 1000，start=min(int(after),len) 依赖 sequence==index+1；LoadByCorrelation 线性扫描；Set 要求 id 和 data 非空，读写都复制字节；文件末尾有接口断言。
3. 测试：没有独立测试文件，由 session 和 tool 的测试间接覆盖。
4. 跨包调用：agentruntime/strategy_shadow.go:53（影子比较的临时事件存储），以及 session、tool 测试。

### internal/orchestration（合计 1379 行，非测试 1244 行）
1. 职责：多 Agent 编排的领域契约，包括类型化 ID、ActorScope、状态迁移表、多流事件存储端口，以及 agent/task/mailbox/lease/wait/root thread 的仓储接口；本身几乎没有存储逻辑。依赖：session（agent.go）。
2. 阅读顺序
- `id.go（194）`：12 种类型化 ID；newTypedID 生成 `prefix_32hex` 形式的 UUIDv7（48 位毫秒时间戳、version 7、variant 2）；validateTypedID 校验前缀、长度、hex 和版本/变体位。必读 ActorScope（IsZero、Validate）、WithActorScope/ActorScopeFromContext、WithToolCallID/ToolCallIDFromContext、WithTurnID/TurnIDFromContext。审计：ActorScope.Validate 要求 Team/Agent/Thread，Task/Run/Parent 可选但出现时校验；ProfileRevision 不校验；ctx 辅助函数都兼容 nil ctx。
- `state.go（122）`：Team/Agent/Task/Approval/Lease 状态常量，ValidateTeamTransition、ValidateAgentTransition、ValidateTaskTransition、validateTransition。审计：Team 的 completed/failed/canceled 可回到 active（会话重开），canceled 只能经 canceling 到达；Agent 的 Completed/Failed→Queued 允许（复用 Agent），Canceled 是终态；Task 的 Completed/Failed/Canceled 是终态，AwaitingMerge 只能到 Blocked/Completed/Failed/Canceled；validateTransition 拒绝空状态和 from==to。Approval/Lease 没有迁移表（是否在别处校验未确认）。
- `team.go（41）`：TeamSnapshot（json team_id/root_agent_id/state/version/updated_at）、TeamTransition（Expected 版本）、TeamRepository（GetTeam/TransitionTeam）、ErrTeamStateConflict、ErrTeamNotActive。
- `event.go（198）`：StreamType、EventType 大表（agent.*、wait_set.*、mailbox.*、workspace.lease/merge/view、usage/budget、flow.*）、Stream.Validate、PendingEvent.Validate、Event（GlobalSequence 和 StreamSequence）、EventStore（Append/LoadStream/LoadGlobal(Cursor)/CurrentSequence）、ErrSequenceConflict/ErrNotFound/ErrIdempotencyConflict/ErrLeaseConflict。审计：Stream.Validate 要求 type、id 和合法 TeamID；PendingEvent 只在设置 ActorAgentID 时校验它；JSONPayload/MustJSONPayload 与 session 包重复。
- `agent.go（197）`：AgentSnapshot（InitialPrompt 标 json:"-"、ProfileSnapshot 原始 JSON、Version）、ActorScope()、AgentResult.Validate、WorkspaceDelivery（只能由 Supervisor 设置）、SpawnSpec（IdempotencyKey，Workspace 属于持久身份）、WorkspaceRequest.Normalize、WorkspaceMode.Validate、AgentTransition（Expected 版本）、AgentRepository、SpawnRequestFinder、ErrAgentLimit、ErrAgentStateConflict。审计：Normalize 对未知值报错，绝不回落到可写模式；WorkspaceMode.Validate 接受 ""（旧数据）；SpawnRequestFinder 只是优化，CreateChild 必须自行重查幂等。
- `task.go（112）`：TaskSnapshot、TaskSpec.Validate（title 非空且不超过 512、description 不超过 1MiB）、TaskTransition、TaskRepository（ClaimTask/ReleaseTask 带版本、Add/RemoveDependency）、TaskClaimReaper 和一组 Task 错误。审计：乐观版本检查和依赖环检测（ErrTaskDependencyCycle）都在实现方，需到 sqlitestore 核对。
- `mailbox.go（151）`：MessageSpec.Validate（发件人不等于收件人、类型枚举、body JSON 不超过 1MiB）、Message.Available、MailboxRepository、MessageClaim.Validate、MailboxLedger（ClaimMessages/AcknowledgeTurn/ReleaseTurn）、MessageRequestFinder。审计：Available 指 enqueued，或 delivered 且 ConsumerTurnID 为空；绑定 turn 的消息在提交时 ack、失败时 release，提交后崩溃也不会重新可用；MessageClaim.Validate 只要求 TurnID，不校验 Request/Limit。
- `lease.go（112）`：WorkspaceRootLeasePath="__workspace__"、LeaseMode、LeaseSpec.Validate、WorkspaceLease、LeaseRepository（含 ReapAbandonedLeases）、validLeasePath。审计：租约没有时间过期，生命周期跟随持有实例及其后代进程；validLeasePath 拒绝空串、NUL、绝对路径、任意 ".." 分量和清理后为 "." 的路径，`\` 归一为 `/`；Validate 校验的是 TrimSpace 后的路径，存储层是否使用未 trim 的 spec.Path 未确认。
- `wait.go（128）`：WaitSpec.Validate（至少 1 个目标、不能等待自己、目标不重复、设置时 Deadline 非零）、WaitSet、WaitSuspendedError（Error 方法 nil 安全）、WaitRepository（Create/Get/List/Resolve/Consume/Expire/CancelWaitSetsForAgent）、WaitSetTargetsJSON。
- `root.go（44）`：RootThread.ActorScope、RootThreadSeed（IdempotencyKey）、RootThreadRepository.EnsureRootThread（返回 created）和 RootThreadBySession。
3. 测试
- event_test.go（26）：Stream/PendingEvent 校验，坏 payload 返回 ErrInvalidEvent。
- id_test.go（47）：各 ID 前缀和 Validate，agent ID 不能当作 team ID。
- lease_test.go（26）：validLeasePath 表驱动测试，值得对照 lease.go 读。
- state_test.go（36）：每张迁移表一个合法、一个非法用例，加 claimed/in_progress/verification→blocked。值得对照 state.go 读。
4. 跨包调用：sqlitestore（21 个文件）实现各仓储，入口有 agents_spawn.go:CreateChild、agents_state.go:36 调 ValidateAgentTransition、tasks.go:185（TransitionTask）调 ValidateTaskTransition、mailbox_ledger.go:ClaimMessages、waitsets.go:CreateWaitSet、orchestration.go:EnsureRootThread、orchestration_events.go:133 LoadGlobal；workspacedomain/leases.go:AcquireLease；agentmanager/supervisor.go:30 和 tools.go:122 使用 WaitSuspendedError；cli/conversation_builder.go:136 处理 WaitSuspendedError；workspacecoord、agentruntime、tui、app、agentflow，以及 tool/policy/tokenusage/einoadapter/approval 都使用 ActorScope。

### internal/tool（合计 2369 行，非测试 1401 行）
1. 职责：工具注册表（写时复制快照、代际编号）、内容寻址的快照身份与持久化恢复，以及工具调用流水线（提议→策略→审批→开始→成功/失败，带重放和幂等）。这是项目的核心安全边界。依赖：orchestration、policy、session。
2. 阅读顺序
- `registry.go（316）`：必读 toolNamePattern（^[a-z][a-z0-9_]{0,63}$）、RiskClass（pure/read/write/execute/secret）、Descriptor.Validate、cloneDescriptor、Executor、Binding、Snapshot（Get 返回克隆，Descriptors 已排序）、Registry、NewRegistry、SnapshotAt、OwnsSnapshot、ActivateManyWithCommit、RetireManyWithCommit。审计：Descriptor.Validate 要求 schema 是 JSON 对象且根 type 为 "object"；ActivateManyWithCommit 在锁外校验和去重，锁内拒绝同 revision 重新绑定（ErrRevisionExists），复制 map 后在锁内调用 commit(old,next)，因此 commit 钩子不能重入注册表，并且必须有自己的 I/O 超时；commit 失败时状态不变。RetireManyWithCommit 要求每项 revision 精确匹配，否则整体不变。snapshots map 永久保存所有代际，从不回收。Binding.Executor 不深拷贝。OwnsSnapshot 按指针身份判断。
- `native.go（18）`：InvokeFunc 适配器，nil 函数返回错误。
- `context.go（53）`：Invocation（SessionID/TurnID/RuntimeSnapshotID/CallID/RegistryGeneration）、InvocationFromContext、withPipeline/PipelineFromContext。审计：Invocation 只由 Pipeline 注入，执行器不能伪造；flow 工具通过 PipelineFromContext 继承审批器。
- `outcome.go（28）`：OutcomeUnknownError（ToolExecutionFatal 返回 true，Unwrap 返回 Cause）、mayHaveSideEffects（风险等级不是 pure/read 即视为有副作用）。
- `turn_guard.go（54）`：PreconditionError{Code,Message}、TurnGuard（BeforeCall/BeforeComplete）、WithTurnGuard（链表，最内层先执行）、checkCallPreconditions、CheckTurnCompletion。
- `pipeline.go（570）`：重点文件。必读 ErrPolicyDenied、ErrApprovalDenied、ErrCallConflict、ExecutionError（已记录、可恢复的工具域失败）、Call、Result、Pipeline、NewPipeline、WithApprover、WithExecutionGuard、ExecutionGuard（CheckCall/BeforeStart）、Execute、validateCall、resolve、replaySuccess、recordProposal、evaluate、authorize、admitStart、invoke、recordSuccess、recordFailure、callCoordinator。
  执行顺序：validateCall（session/tool 必填、参数为 JSON、非零 actor 要校验、callID 为空时生成）→ resolve（nil 快照用当前代，外来快照返回 ErrForeignSnapshot）→ guards.CheckCall → 按 (session,turn,callID) 加进程内锁 → replaySuccess → recordProposal → checkCallPreconditions（失败时 recordFailure 并带 error_code）→ evaluate（记录 policy.decided）→ authorize（deny 先 recordFailure；require_approval 但审批器为 nil 时返回 ErrApprovalUnavailable；否则记录 approval.requested 和 approval.resolved，拒绝时返回 ErrApprovalDenied）→ admitStart（BeforeStart 返回的 release 在执行器调用前就运行，记录 tool.started）→ invoke（向 ctx 注入 ActorScope/ToolCallID/TurnID/pipeline/Invocation）→ recordSuccess。
  审计：
  - replaySuccess 按 turn_id 过滤，同一 callID 出现在其他 turn 视为新调用。已成功的事件要求提议的 tool、revision、参数（equalJSON）一致，否则返回 ErrCallConflict。已 started 但未成功的有副作用工具返回 OutcomeUnknownError，即使后面有 tool.failed 也一样（测试已确认），read/pure 工具则重新执行。
  - recordSuccess 失败（使用 WithoutCancel 加 5s 超时）时返回 OutcomeUnknownError。执行器出错时若 recordFailure 本身失败，返回普通错误而不是 ExecutionError。被标记为 fatal 的错误原样返回。结果不是合法 JSON 时 recordFailure。
  - 策略拒绝通过 errors.Join 合并返回，不是 ExecutionError。
  - 所有 Append 都用 AnySequence，没有乐观序号检查；evaluate/authorize 阶段的 Append 没有用 WithoutCancel，ctx 取消会让审计记录缺失。
  - callCoordinator 只在进程内生效，跨进程去重依赖 runtimeowner 的独占锁。
  - WithApprover/WithExecutionGuard 是浅拷贝，callCoordinator 在副本间共享。
- `snapshot_identity.go（76）`：ErrSnapshotUnavailable；hostIdentity（sync.OnceValues，对 os.Executable 求 sha256）；Snapshot.ID（identityOnce，对清单求哈希：host、排序后的 descriptor、ImplementationID）；SnapshotByID。审计：SnapshotByID 拒绝非 "registry-sha256:" 前缀的 ID，并在注册表锁内遍历所有快照计算 ID，哈希开销发生在锁内。
- `snapshot_manifest.go（169）`：MaxSnapshotManifestBytes=1MiB、SnapshotManifest、canonicalEntry（schema 用 UseNumber 重新编码，capabilities 排序）、Matches、Encode（version 1、host 为 32 字节小写 hex、最多 1024 个工具、排序且无重复、不超过 1MiB）、ID、DecodeSnapshotManifest（DisallowUnknownFields、只允许单个 JSON 值、重新 Encode 校验）、SnapshotReference（Kind 为 turn 或 flow，ID 不超过 4096）、SnapshotStore、BindingResolver、ErrSnapshotReferenceConflict/ErrSnapshotReferenceReleased。
- `snapshot_persistence.go（117）`：ConfigurePersistence（只能调用一次）、RetainSnapshot、ResolveSnapshot。审计：RetainSnapshot 校验引用和快照归属，store 为 nil 时是 no-op（临时模式），Save 在锁外执行。ResolveSnapshot 的流程是：先走 SnapshotByID 快速路径 → store.Load → 校验 manifest.ID==identity → 校验 host → 每个条目优先用当前绑定（Matches），否则用 resolver → 再次 validate 和 Matches → 校验 candidate.ID==identity → 锁内复查并发重复 → 分配新 generation，但不激活为 current。漂移时直接拒绝，不回退。

3. 测试（值得对照源码读的标注为「对照」）
- pipeline_test.go（309，对照）：allow 路径的事件顺序；actor 传递到策略和审计记录；deny 时不调用执行器；审批通过；NewPipeline 依赖校验；WithApprover 副本相互独立；外来快照被拒；持久化成功后重放不再调用执行器（共 4 个事件），参数变化时返回冲突，同一 callID 在另一个 turn 中会重新执行。
- outcome_test.go（121，对照）：遍历所有风险等级。成功记录写入失败时返回 OutcomeUnknown，重启后只有 read/pure 会重新执行；ctx 取消后成功记录仍会持久化；失败记录写入失败时返回的不是 ExecutionError，重放得到 OutcomeUnknown。
- registry_test.go（97）：旧快照保留原 revision；descriptor 切片不可变；RetireMany 原子性。
- registry_publication_test.go（76）：commit 原子性；发布非法时不调用 commit。
- snapshot_identity_test.go（89）：身份与排序和 schema 空白无关；revision 或 implementation 漂移会改变 ID；旧格式 ID 被拒；跨进程稳定（通过子进程验证）。
- snapshot_persistence_test.go（193，对照）：内存 SnapshotStore；恢复出的快照归本注册表所有、不可变、不激活；并发恢复不产生重复；漂移直接拒绝，不回退；解码拒绝非法或重复的清单。
- turn_guard_test.go（83，对照）：前置条件检查在重放之后、策略/审批/执行之前运行，失败时恰好产生 proposed 和 failed 两个事件，并带 error_code；重放路径跳过 guard。
4. 跨包调用
- 组装：app/open.go:177 调 NewPipeline，app/open.go:201 调 ConfigurePersistence(database.RuntimeSnapshots(), pluginManager.ResolveBinding)；agentruntime/strategy_shadow.go:59 为影子比较构建独立流水线。
- Execute：einoadapter/tool.go:110、agentflow/runtime.go:250、agentruntime/goal_verifier.go:59、app/doctor.go:117。
- 发布：ActivateManyWithCommit 由 app/orchestration.go:83、plugin/manager.go:202、plugin/persistence.go:77 调用；RetireManyWithCommit 由 plugin/persistence.go:191 调用。
- 快照持久化：RetainSnapshot/ResolveSnapshot 由 agentruntime/factory_turn.go:42/96/103、agentflow/runtime.go:92/107/133、agentruntime/completed_turn.go:57 调用。
- Guard：agentruntime/profile.go:109 调 WithExecutionGuard（profileToolGuard）；conversation/admission.go:54 调 WithTurnGuard；agentruntime/turn_stream.go:44 调 CheckTurnCompletion。

### internal/policy（合计 214 行，非测试 137 行）
1. 职责：静态规则策略引擎（allow/deny/require_approval），以及 Approver 端口。依赖：orchestration（ActorScope）。
2. `policy.go（137）`：必读 Effect、ErrApprovalUnavailable、Request（Actor/CallID/SessionID/ToolName/Revision/Capability/Resource/RiskClass/NormalizedArg）、Decision、Engine、Approver、Rule、NewStaticEngine、DefaultDeny、Evaluate。审计：NewStaticEngine 复制规则，要求每条规则有 ID，用 path.Match 预校验模式和 effect，空模式视为 "*"；Evaluate 按顺序首个匹配生效，要求 ToolName 和 Capability 非空，都不匹配时用默认规则（RuleID "default"）。ResourcePrefix 是纯字符串 HasPrefix，没有路径边界，"/workspace" 也会匹配 "/workspace2"，调用方必须先规范化。流水线固定传 Capability="tool.invoke"、Resource="tool:"+name，所以实际只能按工具名粒度控制。
3. 测试：policy_test.go（77）：首个匹配生效加默认拒绝（路径逃逸落到默认规则）；畸形模式被拒。可以对照读。
4. 跨包调用：app/builtins.go:66 调 DefaultDeny(rules...)；agentruntime/strategy_shadow.go:55；tool/pipeline.go:evaluate 消费 Engine；approval 实现 Approver。

### internal/tokenusage（合计 269 行，非测试 230 行）
1. 职责：token 用量快照、预算与预留账本接口、价格表计费。依赖：orchestration，外部依赖 eino schema。
2. `usage.go（230）`：必读 Snapshot、FromMessage（nil 安全）、CacheHitPercent（上限钳到 100）、ErrBudgetExceeded、Record、Reservation*、UsageQuery/Aggregate/Report、BudgetLimits/BudgetSnapshot、Ledger（RecordUsage/CanAdmit，ledger 为 nil 时不计账）、ReservationLedger（Reserve/Settle/Release）、DurableReservationLedger（MarkDispatched/StageSettlement/RecoverReservations/ModelRequestSequence）、Reader、Price/Pricer/PriceTable、NewPriceTable、CostMicros。审计：RecoverReservations 的契约要求不能与活跃请求并发，需核对调用时机；CostMicros 先查 "provider/model" 再查 "model"，拒绝负数，未命中缓存的 token 数=max(Input-Cached,0)，reasoning 单独计费（如果供应商的 output 已包含 reasoning 会重复计费，需核对），除以 1e6 时截断，int64 溢出只在理论上可能。
3. 测试：usage_test.go（39）：FromMessage 字段映射、75% 命中率、空消息为 0、上限钳到 100。
4. 跨包调用：agentruntime/model_accounting.go:43/61（断言 DurableReservationLedger）；agentmanager/supervisor_recovery.go:22；tui/model.go:152、tui/turn_state.go；cli/chat_headless.go:174/224；sqlitestore 的 2 个文件实现账本。在本包之外未找到 NewPriceTable 调用（使用方未确认）。

### internal/einoadapter（合计 330 行，非测试 147 行）
1. 职责：把 tool.Pipeline 包装成绑定在单个 turn 上的 Eino InvokableTool，并把流水线错误映射为模型可见的结果或中止 turn 的错误。依赖：orchestration、session、tool，外部依赖 eino。
2. `tool.go（147）`：必读 Tool（descriptor、pipeline、固定的注册表快照、session、turn、runtimeSnapshotID、actor）、NewTools/NewToolsWithActor、Info、InvokableRun、deniedToolResult。审计：构造时校验 pipeline、snapshot、session、turn、actor；Info 把 schema 解码为 jsonschema 并附加 revision/risk_class。InvokableRun 的 CallID 取自 compose.GetToolCallID(ctx)，为空时随机生成，这会削弱重放幂等。错误映射如下：
  - ErrApprovalDenied 变成 JSON 字符串结果，error 为 nil；
  - PreconditionError 变成 {error:code, message}；
  - ExecutionError 变成固定消息的 JSON；
  - 其它错误（OutcomeUnknown、持久化失败、ErrPolicyDenied）作为 error 返回，会中止 turn。策略拒绝会中止 turn，而审批拒绝不会，两者不对称，值得在文档里标出。
3. 测试：tool_test.go（183，值得对照）：turn 级快照在新版本激活后仍保持 rev-1；已记录的执行失败以 JSON 返回给模型；审批拒绝返回结果而不是错误。
4. 跨包调用：agentruntime/eino_turn.go:20 调 NewToolsWithActor。

### internal/approval（合计 431 行，非测试 243 行）
1. 职责：把策略的 require_approval 桥接到交互前端的审批代理，另有非交互的拒绝审批器和白名单审批器。依赖：orchestration、policy。
2. 阅读顺序
- `broker.go（198）`：必读 Request、Source、SourceDescriber、Broker（requests 通道，默认缓冲 16；done；pending map[id]chan bool，每个通道缓冲 1；closed；nextID 原子计数）、NewBroker、WithSourceDescriber、Approve、Resolve、Close、DenyApprover、clonePolicyRequest。审计：
  - 请求 ID 由 "approval-" 加 base36 计数生成，不用 call ID，避免并行 Agent 复用 call ID 时唤醒错误的等待者。
  - Approve 先在锁内登记 pending 再发布请求，然后两段 select（发送、等待结果，均监听 done 和 ctx），defer 删除 pending。
  - Resolve 是非阻塞发送，第一个决定生效；未知 ID 返回 ErrUnknownRequest。
  - Close 幂等，在锁内向所有 pending 发 false 并关闭 done。等待者可能返回 (false,nil) 或 ErrBrokerClosed，具体哪个不确定，但两者都等于拒绝。
  - 等待者取消后，已缓冲在通道里的请求成为陈旧请求，UI 再 Resolve 会得到 ErrUnknownRequest。
- `allowlist.go（45）`：ToolAllowlistApprover 按精确名称匹配（名称会 trim，忽略空项），检查 ctx，内部审批器为 nil 时拒绝。
3. 测试
- broker_test.go（140，值得对照）：并行请求共享 call ID 时得到不同的请求 ID 和来源标签；往返流程；取消；未知 ID；Close 让所有 pending 审批失败。
- allowlist_test.go（48）：精确匹配、取消、nil 时拒绝。
4. 跨包调用：app/open.go:56 调 NewBroker(16, WithSourceDescriber(approvalSource(...)))；cli/chat.go:407 用 DenyApprover，cli/chat.go:424 调 NewToolAllowlistApprover；tui/messages.go:118 消费 Requests()，tui/turn_state.go:115 调 Resolve。

### internal/agentprofile（合计 479 行，非测试 316 行）
1. 职责：Agent 配置档（模型、工具授权、能力开关、限额、工作区模式）的校验、注册、指纹化持久快照，以及委派时的权限收窄。项目内依赖：无。
2. 阅读顺序
- `profile.go（205）`：必读 ContextInheritance、WorkspaceMode、Limits、Profile、Validate、clone、Registry、Activate、Snapshot、Get/GetRevision/Names。审计：
  - WorkspaceMode 只有 shared-readonly/shared-lease/git-worktree，比 orchestration.WorkspaceMode 少了 "snapshot"，两者不一致。
  - Validate 要求 name/revision、模型档案、合法枚举、MaxIterations/MaxToolCalls>0、MaxWallTime>0，其它限额≥0。
  - Registry 用 RWMutex，按 revision 永久保留历史。Activate 先校验全部条目，再在锁内用指纹检测批次内和与已有条目之间的 revision 重绑，全有或全无，generation 递增。同名换新 revision 会覆盖当前版本。Activate 忽略 Fingerprint 的错误，因为已提前校验，所以安全。
  - Snapshot 深拷贝；Names 排序。
- `snapshot.go（111）`：必读 ErrSnapshot、durableSnapshot（Version 1/Fingerprint/Profile）、Fingerprint、Encode、Decode、AllowsTool、ConstrainDelegation、intersectGrants。审计：
  - Fingerprint 对工具列表排序后取 JSON 的 sha256，nil 与空切片不同（null 与 []）。
  - Decode 校验 version、指纹和 revision 是否一致，任一不符返回 ErrSnapshot。
  - AllowsTool：Tools 为 nil 表示交给宿主策略，显式列表是白名单（空列表表示全部禁止）；CanSpawn 控制 agent_spawn/agent_continue，CanManageTasks 控制除 task_list 外的 task_*，CanSendMessages 控制 agent_message。
  - ConstrainDelegation：子 Tools 与（父 Tools 交父 DelegableTools）取交集，子 DelegableTools 再与子 Tools 取交集，三个布尔开关取与，只读父 Agent 不能委派可写工作区；intersectGrants 中 nil 表示不限制。
  - 未限制的情况：父为 shared-lease 时子可以是 git-worktree；Limits 不与父取交集（是否在别处约束未确认）。
3. 测试
- profile_test.go（43）：快照不可变；非法配置档不改动注册表。
- snapshot_test.go（120，值得对照 snapshot.go 读）：空授权被保留；篡改、revision 不符、缺失均被拒；指纹与顺序无关但 nil 不等于空；委派到孙代不能重新获得权限；全禁止被保留；只读父 Agent；保留历史 revision，批量重绑原子拒绝。
4. 跨包调用：app/profiles.go:19 调 NewRegistry；app/orchestration.go:148 调 Encode；app/open.go:253 调 Decode；agentmanager/supervisor.go:328 调 ConstrainDelegation，:334 调 Encode；agentmanager/profile.go:13 调 Decode；agentruntime/profile.go:71/75 做编解码往返，:187 调 AllowsTool；cli/conversation_builder.go:108。

### internal/scheduler（合计 143 行，非测试 112 行）
1. 职责：模型并发桶（计数信号量）和按团队轮转的调度排序。项目内依赖：无。
2. `scheduler.go（112）`：必读 BucketSet、NewBucketSet、TryAcquire、Release、WeightedRoundRobin、Order。审计：BucketSet 由 mutex 保护，limit≤0 表示不限，TryAcquire 不阻塞；Release 下限为 0，重复释放会被静默吞掉，可能掩盖计数 bug。Order 按团队分组，团队按名称排序，团队内按优先级降序再按 ItemKey 排序，每次调用游标轮转一次后交错输出。名字带 Weighted，但实现并没有权重。
3. 测试：scheduler_test.go（31）：桶上限、复用、默认上限；游标前进和团队内优先级。
4. 跨包调用：agentmanager/supervisor.go:175-176 和 :251（NewBucketSet(config.ModelBucketDefaultLimit, ModelBucketLimits)）；agentmanager/supervisor_scheduler.go:81-85。

### internal/strategy（合计 426 行，非测试 243 行）
1. 职责：有界 LoopSpec 策略的数据契约、评测套件、影子对比与验收判定。晋升只能由宿主执行。项目内依赖：无。
2. 阅读顺序
- `types.go（161）`：必读 Spec.Validate（version 1、revision 和 owner 不超过 128、suffix 不超过 8192、MaxIterations 在 0..100、MaxOutputTokens 在 0..32768、不能为空规格）、ErrEvidence/evidenceError（ToolExecutionFatal）、Digest、Spec.ID、Store（Active/Load/Fallback）、Fixture/Suite.Validate（1-8 个 fixture、名称唯一、至少一个 Safety）、Outcome、Report.Acceptance、ValidateSuite。审计：Digest 忽略 json.Marshal 的错误。Acceptance 要求候选与基线长度相等、fixture 身份一致、ModelCalls≥1、无错误、safety fixture 全部通过、candidatePassed≥basePassed、candidateCalls≤baseCalls。ValidateSuite 把报告绑定到套件 digest 和 fixture 名称/safety 标志。
- `shadow.go（66）`：必读 ShadowStore（Load/Suite/SaveReport，没有发布方法）、Comparator、ShadowService、Compare。审计：gate 是容量为 1 的通道，所以比较串行执行，排队时被取消的等待者不会派发。comparator 为 nil 时报错。comparator 返回错误时直接返回、不保存报告；注释说"取消/失败的比较也是证据"，但这只适用于 comparator 返回报告且 err 为 nil 的情况，注释与行为有偏差。SaveReport 用 WithoutCancel 加 5s 超时，失败时返回 errors.Join(ErrEvidence, err)。Compare 本身不调用 Validate/Acceptance，由调用方 agentruntime/strategy_shadow.go:45 调用。
- `catalog.go（16）`：Catalog 在 Store 上扩展 Save/SaveSuite/Suite/SaveReport/Report/Promote，仅供宿主使用。
3. 测试：shadow_test.go（183，值得对照 shadow.go 读）：在每个失败点上检查流程步骤顺序；证据写入失败是 fatal，其它失败不会被误判；取消后的结果仍以保留值、不带取消、不超过 5s 的 ctx 保存；被取消的等待者不派发；依赖缺失。types.go 没有专门的测试文件（是否由其它包测试覆盖未确认）。
4. 跨包调用：app/open.go:219 调 NewShadowService；agentruntime/strategy_shadow.go:45 调 Acceptance；sqlitestore/strategies.go:65/112/121/154/166/203 实现 Catalog；evolutiontools（3 处）；cli（2 处）。

### internal/modelcontext（合计 273 行，非测试 151 行）
1. 职责：解析模型上下文窗口、输出上限、输入预算和压缩触发阈值。项目内依赖：无。
2. `limits.go（151）`：必读 Source、FallbackWindow（32768）、Metadata、Informer、Overrides、Limits、Lookup、Resolve、InputLimit、SafetyMargin、DefaultTrigger、Validate。审计：
  - 内置目录只收录 OpenAI，精确匹配 ID，另外会去掉日期快照后缀（注释写明 2026-10-16 核对），其它模型回落到默认值。
  - Resolve 拒绝负数覆盖值；窗口≤1 时回落到默认值；供应商声明的窗口是覆盖值的硬上限；默认输出上限=max(1,min(8192,window/4))，再受 MaxOutputTokens 限制（显式覆盖值同样受限）；触发阈值取 min(trigger,budget)。
  - SafetyMargin=max(1,min(4096,window/20))；InputLimit=window−margin−output，下限为 1。
  - DefaultTrigger 取窗口的 80%，计算防溢出，上限为 200000，并且不超过 InputLimit；这是上下文工作阈值，与 Team 累计消费额度无关，本轮保持不变。
  - Validate 要求 output<window−margin 且 0<trigger<window。
3. 测试：limits_test.go（122，值得对照）：目录不猜别名或供应商；各窗口下的触发值（如 32768/8192→22938）；供应商上限与覆盖值；只有输出元数据的情况；非法值；maxInt 处的溢出。
4. 跨包调用：agentruntime/factory.go:68 调 Resolve，:112 调 Lookup；agentruntime/profile.go:90/97；modelgateway/context_metadata.go:39。

### internal/tokenizer（合计 373 行，非测试 312 行）
1. 职责：消息 token 计数器，支持本地 tiktoken 精确计数、按供应商实际输入校准，以及按字符数估算。项目内依赖：无，外部依赖 eino schema 和 tiktoken。
2. `tokenizer.go（312）`：必读 Status（exact/provider/estimated/unavailable）、Counter、NewForModel、NewForModelWithEncoding、NewApproximate、modelCounter、localTokenizer、cachedBPE、CountMessages、CountLocalMessages、Fork、ObserveProviderInput、countWithEncoding、estimateMessages、encodingForModel。审计：
  - localTokenizer 用 RWMutex 双检锁懒加载，只有 BPE 文件已在本地缓存时才加载，不访问网络。
  - CountMessages=本地计数+providerOffset，下限 0；CountLocalMessages 不含 offset，用于后缀选择。
  - ObserveProviderInput 执行 offset += actual−estimated 并把状态设为 Provider。调用方必须传入经过校准的 CountMessages 估计值才能收敛（测试是这样传的）。
  - 状态顺序问题：如果先调用 ObserveProviderInput 设为 Provider，之后第一次 localTokenizer 加载会把状态改写为 Exact/Unavailable，结果取决于调用顺序。
  - countWithEncoding 使用 OpenAI chat 帧开销（每条消息 3+3），并计入字段、tool call、Extra JSON 和多段内容；estimateMessages 按 rune 数/4 估算，忽略多段内容。
  - encodingForModel 只处理 openai：gpt-4* 和 gpt-3.5-turbo* 用 cl100k；text-davinci-、code-davinci- 以及任何包含 "codex" 的名称用 p50k_base。因此 "gpt-5-codex" 会被映射到 p50k_base，与注释中"GPT-5 系列保持供应商校准"的说法矛盾；如果本地有缓存，还会错误地报告 Exact。这是一个需要确认的缺陷。
3. 测试：tokenizer_test.go（61）：未知模型走估算；供应商校准收敛到 42；显式指定 encoding 时如实报告；Fork 之间相互独立。没有覆盖 codex 映射。
4. 跨包调用：modelgateway/gateway.go:61 调 NewForModelWithEncoding；agentruntime/factory.go:76 用 NewApproximate 兜底；agentruntime/factory_views.go:26 调 Fork；agentruntime/compaction_transcript.go:136 调 CountLocalMessages。

### internal/artifact（合计 286 行，非测试 240 行）
1. 职责：有大小上限、按内容寻址的文件存储，存放补丁、转录等大对象。项目内依赖：无。
2. `store.go（240）`：必读 Store（Root，MaxBytes 默认 32MiB）、NewStore、Put、ReadArtifact、resolve、normalizeReference、contextError。审计：
  - NewStore 把 Root 转成绝对路径，以 0700 MkdirAll，并确认是目录。
  - Put 返回 "sha256:<hex>"，文件放在 sha256/<hex>。文件已存在时要求是普通文件、大小合法且字节完全相等，否则报 "collision"（即去重）。否则在同目录 CreateTemp，chmod 0600，Write、Sync、Close 后 Rename；Rename 失败时如果目标已是相同内容就视为成功。父目录没有 fsync，崩溃后持久性需评估。
  - ReadArtifact：normalizeReference 除 "sha256:"+64 位 hex 外，也接受任意本地相对路径，即允许读取非内容寻址的文件；同时检查 Size 并用 LimitReader 双重限长，读完再检查一次 ctx；读取时不重新计算哈希，无法发现被篡改的内容。
  - resolve：对 root 做 EvalSymlinks，用 Rel+IsLocal 检查，再对目标（不存在时对最近的已存在祖先）做 EvalSymlinks，防止符号链接逃逸。但 resolve 与随后的 open/rename 之间存在 TOCTOU 窗口，检查之后符号链接仍可能被替换。
3. 测试：store_test.go（46）：put/read/去重；拒绝符号链接逃逸。没有覆盖 collision、超限和相对路径读取。
4. 跨包调用：app/open.go:113 调 NewStore(DataDirectory/artifacts)；app/artifacts.go:48；workspacecoord 的 applycommit.go:150、snapshot_review.go:95、merge.go:225、applyplan.go:161/169/205 调 ReadArtifact。

### internal/retention（79 行，全部非测试）
1. 职责：会话归档保留计划与插件回收计划的纯数据契约（digest 防止计划在确认后被改动），逻辑在 sqlitestore。依赖：session、tool。
2. 阅读顺序
- `retention.go（48）`：ErrPlanChanged、ErrBlocked、Consumer（tool.SnapshotReference 加 SnapshotID）、Plan（ID/SessionID/Sequence/Consumers/Blockers）、Plan.Digest（把 ID 置空后序列化，忽略 json.Marshal 错误）、Digest、Receipt（ClosedAt/ReleasedReferences/EvidenceDeleted）。审计：digest 是"预览后关闭"的乐观并发令牌，实现方必须在事务内重算并比对。
- `collection.go（31）`：CollectionPlan（精确的工具 revision、ContentSHA256、State、字节数、Blockers）及其 Digest；CollectionReceipt（BlobDeleted）。
3. 测试：无。
4. 跨包调用：sqlitestore/retention.go:15 RetentionPlan，:26 CloseRetention（:42 返回 ErrPlanChanged）；sqlitestore/retention_plan.go:16 planRetention；app/maintenance.go:34 PreviewRetention，:43 ArchiveSession，:52 PreviewPluginCollection。

### internal/runtimeowner（合计 260 行，非测试 111 行）
1. 职责：基于 flock 的会话数据库单进程所有权锁，保证同一会话库同时只有一个运行时（tool 流水线的进程内去重依赖它）。项目内依赖：无。
2. 阅读顺序
- `lock.go（82）`：必读 ErrOwned、Lock（file、once、err）、Acquire、Close、RetainUntilExit。审计：Acquire 拒绝 "" 和 ":memory:"；取绝对路径，以 0700 MkdirAll 父目录；用 EvalSymlinks 规范化，让不同别名映射到同一把锁，悬空的数据库符号链接直接拒绝，否则只规范化父目录；以 0600 打开 `<path>.owner.lock` 后 tryLock，失败时关闭文件并包装错误。Close 只执行一次，且从不删除锁文件，避免 inode 被替换导致两个进程各锁一个文件。RetainUntilExit 把锁放进全局切片，进程退出前永不释放，供超时的 worker 使用（app/close.go:94）。
- `lock_unix.go（17，darwin||linux）`：flock LOCK_EX|LOCK_NB，EWOULDBLOCK/EAGAIN 映射为 ErrOwned。
- `lock_other.go（12）`：其它平台直接返回错误，失败即关闭（fail closed）。
3. 测试：lock_test.go（149，darwin||linux，值得对照读）：通过子进程（TestOwnershipChild 辅助测试）验证跨进程互斥及关闭后释放；目录符号链接别名返回 ErrOwned；悬空别名被拒；持有进程被 kill 后内核释放锁（TestOwnershipHolder 辅助测试）。
4. 跨包调用：app/open.go:41 和 app/maintenance.go:21 调 Acquire(resolved.SessionDatabasePath())；app/app.go:64 持有 owner；app/close.go:94 调 RetainUntilExit（:95 对 domain 也调用了同名方法，属于另一类型）。

### internal/sessionregistry（合计 297 行，非测试 231 行）
1. 职责：用 SQLite 保存会话目录（名称、工作区、运行时、状态、pid、资源目录），供 CLI 的 session 管理使用。项目内依赖：无，外部依赖 modernc sqlite。
2. `registry.go（231）`：必读 State（created/running/stopped/failed）、ErrNotFound、Session、Open、Create、Get、List、UpdateState、Remove、scanSession。审计：
  - Open 取绝对路径，以 0700 MkdirAll，MaxOpenConns=1，在一次 Exec 里设置 busy_timeout/foreign_keys/WAL 并建 agent_sessions 表。
  - Create 校验 ID/名称/工作区/运行时，补默认状态和时间，在 INSERT 之前对资源目录做 MkdirAll 和 Chmod 0700；INSERT 失败（如主键重复）时这些目录会残留。
  - Get 把 ErrNoRows 映射为 ErrNotFound；UpdateState/Remove 检查 RowsAffected，但返回普通错误而不是包装后的 ErrNotFound，与 Get 不一致。
  - 没有状态迁移校验；只有 Create 有 nil registry 保护，Get/List/UpdateState/Remove 没有。
  - List 按 created_at、id 排序；scanSession 解析 RFC3339Nano。
3. 测试：registry_test.go（66）：创建、目录存在、获取、更新为 running 并带 pid、列表、停止、删除、缺失返回 ErrNotFound。
4. 跨包调用：cli/session.go:94/99/118/139/160/172；cli/chat.go:100。

### internal/render（合计 437 行，非测试 318 行）
1. 职责：非交互输出共用的流式 Markdown 渲染器，对代码块做 chroma 语法高亮；交互界面的渲染由 tui 负责。项目内依赖：无，外部依赖 chroma quick。
2. 阅读顺序
- `doc.go（3）`：包文档。
- `markdown_stream.go（315）`：必读 writeStyled/WriteStyled、MetadataColor、markdownStreamRenderer（pending/inFence/codeLineStart/fence/fenceLen/language）、Write、Finish、processLine、parseInlineFence、writeCodePending、isFenceClosing、parseFenceOpening、potentialFencePrefix、highlightSource、NormalizeCodeLanguage。审计：
  - 不着色时直接透传；着色时缓冲输入。不在代码块内时逐行处理，不完整的行会立即刷出，除非它可能是 fence 前缀。
  - parseInlineFence 把 "text```go" 拆成前缀、换行和开头 fence。
  - 关闭 fence 只在行首识别，且长度不能小于开头 fence；开头 fence 至少 3 个字符，info 串不能含反引号或波浪号，首词转小写。
  - 代码块内完整行逐行高亮，不完整的片段也会立即高亮，可能切断 token 或 ANSI 状态（影响小）。
  - 如果流里一直没有换行且看起来像 fence 前缀，pending 会无限增长。
  - highlightSource 使用 terminal256 和 monokai，出错时回落到原文。
3. 测试：markdown_stream_test.go（119）：纯文本原样输出；纯文本和代码的增量立即写出；多语言高亮；关闭 fence；行内开头 fence 的规范化；fence 别名解析。
4. 跨包调用：cli/chat_output.go:17/25 调 NewMarkdownStreamRenderer；cli/chat_headless.go:230 调 WriteStyled/MetadataColor；tui/markdown.go:168 调 NormalizeCodeLanguage。

### 基础包整体阅读顺序和理由
项目内依赖关系：session 无依赖；memorystore 依赖 session；orchestration 依赖 session；policy 依赖 orchestration；tool 依赖 orchestration、policy、session；einoadapter 依赖 orchestration、session、tool；approval 依赖 orchestration、policy；tokenusage 依赖 orchestration；retention 依赖 session、tool；其余各包在项目内没有依赖。

1. session，然后 memorystore：追加式事件、乐观序号和分页遍历是所有持久化语义的基础；memorystore 是最小参考实现，也是测试替身。
2. orchestration：类型化 ID、ActorScope、状态迁移表和各仓储端口，是 sqlitestore、agentmanager、workspacecoord 的共同契约，只依赖 session。
3. policy：规模小，是 tool 流水线授权的前置知识。
4. tool：按 registry、context/native/outcome/turn_guard、pipeline、snapshot_manifest/identity/persistence 的顺序读。这是核心安全边界，重放、幂等和 OutcomeUnknown 的语义决定了上层的崩溃恢复。
5. approval，然后 einoadapter：二者都消费 policy 和 tool。读 einoadapter 时重点看错误映射，它决定哪些失败会中止 turn。
6. agentprofile：工具授权和委派收窄，agentruntime 的 profileToolGuard 与 agentmanager 的 Supervisor 都依赖它；读完 tool 再读，能看清授权落在哪一层。
7. modelcontext、tokenizer、tokenusage：分别负责窗口预算、计数校准和账本计费，按"预算→计数→记账"的数据流顺序读。
8. scheduler，然后 strategy：前者是 Supervisor 的调度原语；后者是自进化策略的证据与验收契约，依赖前面建立的"证据必须持久化"的概念。
9. artifact、retention、runtimeowner、sessionregistry：存储与生命周期基础设施。runtimeowner 最好紧接 tool 之后至少浏览一遍，因为 callCoordinator 只在进程内去重，跨进程安全完全依赖这把锁。
10. render：纯展示层，风险最低，放在最后。

### 基础包审计发现汇总（标注"未确认"的需要再核实）

#### 可能的缺陷或与文档不一致
1. tokenizer/tokenizer.go 的 encodingForModel 只要模型名包含 "codex" 就用 p50k_base。因此 "gpt-5-codex" 会用错编码，和注释"GPT-5 系列靠供应商校准"矛盾；本地有 BPE 缓存时还会错误地报告 Exact。
2. tokenizer：先调用 ObserveProviderInput（状态设为 Provider），之后第一次加载本地 tokenizer 会把状态改成 Exact 或 Unavailable，最终状态取决于调用顺序。
3. strategy/shadow.go 的 Compare：注释说"取消或失败的比较也是证据"，但 comparator 一返回错误就直接返回，不保存报告。
4. einoadapter/tool.go 的 InvokableRun：审批拒绝会作为结果返回给模型，策略拒绝（ErrPolicyDenied，由 errors.Join 合并）却作为 error 返回并中止整个 turn，两者处理不一致。CallID 为空时随机生成，会削弱重放幂等。
5. agentprofile 的 WorkspaceMode 没有 orchestration 里的 "snapshot" 模式。ConstrainDelegation 不限制"父 shared-lease、子 git-worktree"这种组合，也不和父 Agent 的 Limits 取交集（别处是否有约束未确认）。
6. policy：ResourcePrefix 用纯字符串 HasPrefix，没有路径边界，"/workspace" 也会匹配 "/workspace2"。目前流水线固定传 Resource="tool:"+name，所以暂时没有被利用。
7. sessionregistry：UpdateState/Remove 找不到记录时返回普通错误，没有包装 ErrNotFound，和 Get 不一致；INSERT 失败时已建的资源目录会残留；Get/List/UpdateState/Remove 没有 nil registry 保护；没有状态迁移校验。
8. artifact：resolve 与随后的 open/rename 之间有 TOCTOU 窗口，检查后符号链接仍可能被替换；读取时不重新校验哈希；ReadArtifact 接受任意本地相对路径，不只限于 sha256 引用；父目录没有 fsync。
9. tokenusage 的 CostMicros 单独对 reasoning 计费，如果供应商的 output 已包含 reasoning 会重复计费（未确认）。
10. scheduler：WeightedRoundRobin 实际没有权重；Release 重复释放会被静默吞掉。

#### 设计约束（审计时要守住的不变量）
- tool 流水线的 callCoordinator 只在进程内生效，跨进程去重完全依赖 runtimeowner 的 flock。锁文件从不删除，RetainUntilExit 在 app/close.go:94 调用。
- 有副作用的工具已 started 但没有成功记录时，无论之后是否有 tool.failed，重放一律返回 OutcomeUnknownError；只有 read/pure 工具会重新执行。
- 流水线的 Append 都用 AnySequence，没有乐观序号检查。evaluate/authorize 阶段写审计记录时没有用 WithoutCancel，ctx 取消会让 policy.decided 或 approval.* 记录缺失；只有 recordSuccess/recordFailure 用了 WithoutCancel 加 5s 超时。
- tool.Registry 的 snapshots 和 agentprofile.Registry 的历史 revision 都永久保存、从不回收。tool.SnapshotByID 在注册表锁内对所有快照计算哈希。
- ActivateManyWithCommit 在注册表锁内调用 commit 钩子，钩子不能重入注册表，并且要自带 I/O 超时。
- approval.Broker 在 Close 时，等待者可能返回 (false,nil) 或 ErrBrokerClosed，两者都等于拒绝；等待者取消后，通道里缓冲的旧请求再 Resolve 会得到 ErrUnknownRequest。
- orchestration.LeaseSpec.Validate 校验的是 TrimSpace 后的路径，存储层是否使用 trim 后的值未确认。
- session.ErrRecoveryDependency 在本包内没有使用（使用方未确认）。

#### 其它未确认项
- Team/Approval/Lease 状态没有迁移表，是否在 sqlitestore 里校验未确认。
- 在 tokenusage 包外没有找到 NewPriceTable 的调用方。
- strategy/types.go 没有专门的测试文件。

## 第 2 阶段：模型与运行时

顺序：modelgateway（模型线路）→ agentruntime（Turn 生命周期、记账、上下文、恢复）→ agentgoal（Goal Mode 领域模型）→ conversation（会话编排与 Goal 外循环）。读完沿本阶段末尾的运行时调用链 (a)(b)(c) 走一遍。

### internal/modelgateway

职责：按 Provider/WireAPI 构造 Eino ChatModel（OpenAI Chat Completions、自实现的 OpenAI Responses、Ollama），并在构造时探测上下文窗口元数据。项目内依赖：`tokenizer`、`modelcontext`。

阅读顺序：

1. `gateway.go`（176）：Config 归一化与模型构造入口。
   - 必读：`Provider`、`WireAPI`、`Config`、`New`、`newWithLookup`、`NewTokenizer`、`validateBaseURL`。
   - 审计：Config 只保存 `APIKeyEnvironment`（环境变量名），密钥不进入配置；OpenAI 缺 key 直接报错；Timeout 默认 2 分钟；`validateBaseURL` 拒绝非 http(s)、无 host、带 userinfo/query/fragment 的地址；Responses 线路不经过 cacheUsage transport。
2. `context_metadata.go`（150）：`contextAwareModel` 暴露 `ModelContext()`。
   - 必读：`discoverContext`、`discoverOllamaContext`、`readContextMetadata`、`ollamaModelName`、`WithTools`。
   - 审计：探测只在构造时跑一次；OpenAI 兼容探测只在设置了 BaseURL 且 host 不是 api.openai.com 时执行（`GET /models/{model}`，ID 不匹配就丢弃）；窗口取 context_window/context_length/max_model_len 的最小值，刻意排除 max_tokens；Ollama 走 `/api/ps`，取匹配的已加载模型的最小 context_length；2 秒超时、64KiB 上限、要求 HTTP 200。注意：探测用 `os.Getenv` 而不是注入的 lookup，可测试性和 `New` 不一致。
3. `cache_usage_transport.go`（136）：把 DeepSeek 的 `prompt_cache_hit_tokens` 归一到 `prompt_tokens_details.cached_tokens`。
   - 必读：`cacheUsageHTTPClient`、`cacheUsageTransport.RoundTrip`、`normalizeCacheUsageJSON`。
   - 审计：克隆调用方 client，不改原对象；SSE 用 `io.Pipe` + goroutine 逐行改写（scanner 上限 8MiB）；非 SSE 整体读入后改写并修正 Content-Length；任何解析失败都原样返回。
4. `responses_types.go`（154）：Responses 的请求、流事件、item、usage 结构。
   - 必读：`toSchemaTokenUsage`。
   - 审计：cached tokens 的取值优先级是 input_tokens_details > prompt_tokens_details > prompt_cache_hit。
5. `responses_transport.go`（70）：HTTP 收发。
   - 必读：`doResponsesRequest`、`encodeResponsesRequest`、`decodeResponsesPayload`。
   - 审计：成功时把 cancel 交给调用方负责；非 2xx 限长读取 body，返回 `HTTP %d: msg`。
6. `responses.go`（425）：Responses 模型的 Generate/Stream 与请求构建。
   - 必读：`newResponsesModel`、`normalizeReasoningEffort`、`buildResponsesRequest`、`responseToolChoice`、`responseInputItems`、`decodeResponsesMessage`、`decodeResponsesArguments`、`responseErrorText`。
   - 审计：MaxTokens 必须大于 0 并映射为 `max_output_tokens`；reasoning effort 默认 medium，非 none 时 summary=auto；ToolChoice 映射（Forbidden→none，强制单工具→命名函数，多工具→allowed_tools required）；tool 消息必须带 ToolCallID；输出既无内容也无工具调用视为错误；Stream 在 goroutine 中解析，结束后 cancel。
7. `responses_stream.go`（299）：SSE 状态机。
   - 必读：`ErrResponsesStreamTruncated`、`streamResponsesBody`、`emitResponsesStreamEvent`、`streamOutputIndex`。
   - 审计：`[DONE]`、事件名与 payload type 不一致、帧超限都是错误；没收到 `response.completed` 就结束返回 Truncated；`output_item.done` 只在没见过 arguments delta 时补发完整参数（防重复）；completed 要求 status=="completed"，在语义终点停止解析，不等 HTTP 关闭；`writer.Send` 返回 true（消费者已关闭）时停止；error/failed/incomplete/cancelled 转为错误。

测试：

- `gateway_test.go`（466）：缺凭据报错且不泄露值、cache transport 不改调用方 client、OpenAI 兼容线路、DeepSeek 缓存用量（非流/流）归一、Responses 工具往返、推理摘要、构造不发网络请求、拒绝含凭据的 BaseURL、并发 Agent 共享客户端。**对照读**凭据与缓存归一两组。
- `responses_terminal_test.go`（120）：必须有语义终止事件、completed 后不等 HTTP 关闭、拒绝 error/无效 completed/坏帧。**对照读** `streamResponsesBody`。
- `responses_arguments_test.go`（96）：delta 与完整 item 交错时参数不重复。**对照读** `emitResponsesStreamEvent`。
- `context_metadata_test.go`（99）、`responses_types_test.go`（30）：元数据探测回退、cache 字段变体。

跨包：`agentruntime/factory.go` 的 `modelCapacity` 通过 `modelcontext.Informer` 读取 `ModelContext()`；`NewTokenizer` 调用 `tokenizer.NewForModelWithEncoding`；模型实例由 cli 层按配置创建后交给 app。

### internal/agentruntime（第 1 段：骨架与 Turn 生命周期）

职责：把 Eino ChatModelAgent/Runner 绑定到不可变工具快照、append-only 会话事件、checkpoint、token 账本与上下文管理，对外提供 `Factory` 和 `Turn`。项目内依赖：`agentgoal`、`agentprofile`、`modelcontext`、`orchestration`、`session`、`session/memorystore`（只在 strategy_shadow 用）、`strategy`、`tokenizer`、`tokenusage`、`tool`、`einoadapter`、`policy`。

1. `runtime.go`（5）：只有包文档。
2. `config.go`（49）：`Config` 全部字段与 `ContextArtifactStore.Put`。
   - 审计：GoalMode 需要 GoalStore；ContextWindowTokens/CompactionTriggerTokens 只是覆盖值，最终由 factory 解析。
3. `ids.go`（21）：`checkpointID = "session/"+sid+"/turn/"+tid`；`randomID` 是 16 字节 hex。
   - 审计：checkpoint 与 turn 一一对应，是恢复的唯一键。
4. `events.go`（53）：实时（不持久化）事件：assistant.text.delta、reasoning.delta、message.completed、tool.result、runtime.message、goal.phase。
   - 必读：`Event`、`EventSink`、`emitEvent`。
   - 审计：sink 返回错误会中断 turn（见 turn_stream.go）。
5. `factory.go`（134）：
   - 必读：`Factory`、`NewFactory`、`modelCapacity`、`ErrTurnModelChanged`。
   - 审计：必需 model/registry/pipeline/events/checkpoints；MaxIterations 默认 20；窗口由 `modelCapacity` + `modelcontext.Resolve` + `applyContextLimits` 决定；tokenizer 缺省为近似计数；开启 Goal Mode 时组装 goalEval。
6. `factory_views.go`（84）：`WithActorScope`、`GoalRuntime`、`WithoutGoalMode`、`WithActiveGoal`、`GoalEvaluator`。
   - 审计：`WithActorScope` 丢弃 mailbox、fork tokenizer、重置 calibration（子 Agent 不污染父 Agent 的估算）；`WithActiveGoal` = Enabled+Always（隐藏 goal_start）；Adaptive 模式用 `AdaptiveEvaluator{fileGoalVerifier, independent}`，其它走 `agentgoal.Compose`。
7. `factory_snapshot.go`（113）：`snapshotIDForTurn`、`modelRequestSequence`、`validateTurnModel`。
   - 审计：请求序号 = max(model.requested 事件的 request_sequence 或 request_id 后缀, 账本 `ModelRequestSequence`)，包含已退款的预留，保证恢复后 request ID 不复用；`validateTurnModel` 比对首个 turn.started 的 run_id/execution_config_id/model_profile/provider/model，空字段（旧数据）放行。三个函数都通过 `session.VisitEvents` 分页扫描。
8. `factory_turn.go`（121）：`NewTurn` 与 `ResumeTurn`。
   - NewTurn 顺序：withActiveStrategy → profile 会话检查 → settleBackground(releaseOpen=true) → registry.Snapshot → RetainSnapshot → 若 CurrentSequence==0 以期望序号 0 写 session.created → buildTurn(seq 0)。
   - ResumeTurn 顺序：withTurnStrategy → profile 检查 → CheckTurnRecovery（墓碑）→ checkpoint 必须存在 → snapshotIDForTurn → ResolveSnapshot → validateTurnModel → RetainSnapshot → modelRequestSequence → 恢复 goalMode/activation → buildTurn。
   - 审计：恢复必须用原工具快照和原模型，漂移即拒绝；session.created 依赖期望序号 0 防止并发重复写。
9. `strategy.go`（94）：`BaselineConfigID`、`withActiveStrategy`、`withStrategy`、`withTurnStrategy`、`strategyFailure`。
   - 审计：策略后缀追加到 instruction，iterations/output 取最小值；恢复时从 turn.started 的 loop_spec_id 还原，且 base ID 必须一致；失败只触发 `Strategies.Fallback`，不自动重放。
10. `eino_turn.go`（101）：`buildTurn`、`runtimeInstruction`。
    - 审计：`einoadapter.NewToolsWithActor` 之后过滤工具：Goal Mode 关闭时去掉 goal_report；关闭或 Always 时去掉 goal_start；再按 `permitsProfileTool` 过滤。ToolsNode `ExecuteSequentially=true`（工具串行执行）；Runner `EnableStreaming=true`、`CheckPointStore=f.checkpoints`；`recordingModel.requestSequence` 用传入值初始化。**这里的过滤逻辑必须和 `context_count.go:contextToolSchemas` 一致**，否则上下文计数与真实请求不符。
11. `turn.go`（177）：`Turn`、`Result`、`Run`、`RunBackground`、`begin`、`cancelOnDone`、`start`、`Resume`。
    - 审计：`begin` 先 `session.CheckRecovery` 再 CAS（单次使用）；`Run` 要求最后一条消息是 user；`start` 在一次 Append 中写 turn.started + user.message（或 background.input Opening=true），保证原子开局；`cancelOnDone` 用 `context.AfterFunc` 触发 CancelImmediate；`Resume` 不重写 turn.started/user.message，`runner.Resume` 出错走 `t.fail`。
12. `turn_stream.go`（162）：`consumeIterator`、`consumeCompleteMessage`、`consumeStreamingMessage`。
    - 审计：event.Err、流错误、sink 错误都走 `t.fail`；最终 assistant 消息必须非空且无 ToolCalls；写 turn.completed 前调用 `tool.CheckTurnCompletion`（TurnGuard.BeforeComplete）和 completedContext。流式分支用 `schema.ConcatMessages` 合并，`cloneMessage` 后再发事件。
    - **待确认的发现**：turn.completed 的 Append 失败时直接返回，不调用 `t.fail`（不写 turn.failed，checkpoint 也没删），之后这个 turn 会被当作开放 turn 恢复。之后删 checkpoint 和 `mailbox.Acknowledge` 的错误都被忽略（注释说明下次结算会补确认）。

### internal/agentruntime（第 2 段：模型记账、上下文与恢复）

13. `turn_durability.go`（49）：`fail`、`durabilityContext`。
    - 审计：在 `WithoutCancel` + 5 秒的上下文中记录，请求已取消也能写入；先查 checkpoint 是否存在，写 turn.failed{recoverable, checkpoint_id}，可恢复时再追加 checkpoint.created（两次独立 Append，**不是原子的**）；`context.Canceled` 不触发 strategyFailure；返回 `errors.Join(cause, recordErr)`。
14. `completed_turn.go`（61）：`CompletedTurn`。读取已提交 turn.completed 内容作为候选（空则报错），校验模型与快照 ID 一致，绝不重新执行。供 Goal 恢复的"仅评估"阶段使用。
15. `recovery.go`（81）：`TurnRecord`、`CheckTurnRecovery`、`TurnRecords`、`LoadTurnRecords`。
    - 审计：重复 turn.started 报错；跟踪 Completed/Abandoned/Archived；只有开放 turn 设置 HasCheckpoint。
16. `turn_modes.go`（177）：`activationForTurn`、`AutoAdmissionForTurn`、`AdmissionRequestedForTurn`、`TurnInput`、`goalModeForTurn`、`CountCompletedUserTurns`。
    - 审计：恢复时模式以 turn.started 记录为准，旧 turn 回退到 Always 或 factory 设置；`AdmissionRequestedForTurn` 只看结构化事件（tool.proposed/failed 的工具名、error_code、risk_class=write），不解析文本；`TurnInput` 返回原始输入与 turn.started 时间（截止时间不重置）；计数 = 非后台、未映射到 goal 的已完成 turn + CountGoals。
17. `model.go`（314）：`recordingModel` 包装真实模型，是**所有模型请求的记账与事件点**。
    - 必读：`Generate`、`Stream`、`recordModelRequest`、`recordAssistantMessage`、`reserve`、`releaseUnsent`、`recordModelFailure`。
    - 调用顺序：prepareModelInput → countRequestInput → requestID=`turnID:seq+1` → reserve → 写 model.requested（失败则 releaseUnsent）→ markDispatched（失败则 releaseUnsent）→ 调用内层模型（出错 failRequest）→ settle + 写 assistant.message。
    - 审计：requestSequence 为 atomic，reservations/estimates 为 `sync.Map`；reserve 拒绝复用已 Dispatched 或非 pending 的预留；只有派发前才退款。Stream：上游错误转 failedMessageStream；goroutine 中每个 chunk 克隆两份（Eino 会改 ID）；消费者提前关闭则 failRequest("stream consumer closed")。
    - 注意：Stream 先 recordAssistantMessage 后 observeProviderUsage，Generate 相反（Stream 路径的 calibration 晚一步，影响未确认）。
18. `model_accounting.go`（99）：`modelInvocationError`、`failRequest`、`settleUsage`、`markDispatched`。
    - 审计：失败按预留估算计费（`settleUsage(nil)`）并写 model.failed；记账或写事件失败时返回 join 错误而不是 `modelInvocationError`（不能被当作可回退的模型失败）；`DurableReservationLedger` 下先 StageSettlement 再 SettleReservation（崩溃可恢复的收据）；无定价时 quality 加 `_unpriced`。
19. `request_context.go`（262）：Eino 循环内**每次派发前**的上下文处理。
    - 必读：`prepareModelInput`、`prepareRequestContext`、`compactRequest`、`recordRequestContext`、`checkContextLimit`、`requestOutputLimit`、`requestPrefixDigest`、`ErrContextCapacity`。
    - 审计：每次派发都检查完整输入+输出预留+安全余量≤工作窗口；辅助请求（purpose 非空）只做容量检查，不递归压缩，后续仍经过配额和记账。输入预算是单请求容量，Team 预算是累计消费额度；`requestContextState` 加锁，按 `input[:covered]` 的 sha256 前缀摘要复用已准备视图（忽略 ResponseMeta 与 `_eino_msg_id`）；trigger=min(CompactionTriggerTokens, InputLimit)；`compactRequest` 固定非摘要 system 消息、保留 trigger/3 的尾部、最新 user 原样保留；防抖：只剩旧摘要时不再压缩，压缩后不变小就拒绝；写 v2 context.compacted，只有该 turn 完成才进入历史。
20. `context_count.go`（65）：`countContextInput`、`contextToolSchemas`。工具 schema 作为一条 system 消息与输入一起计数一次，calibration 只乘一次。
21. `context_usage.go`（104）：`ContextState`（ready/compaction_pending/over_budget）、`ContextUsage`、`contextCalibration`、`observeProviderInput`。RWMutex 保护；按 requestID 去重观测，防止重刷历史导致校准漂移；Used>InputLimit 即 over_budget。
22. `context_assessment.go`（57）：`captureRequest`、`completedContext`。只有主请求捕获最后一次派发的输入/工具/输出上限的私有副本；turn 结束评估只做本地计数，不调模型。
23. `context_artifacts.go`（55）：`externalizeToolResults`。>24KiB 的 tool 消息替换为 artifact 引用（2KiB 头尾预览 + artifact_read 提示）；复制消息，不改原切片。
24. `context_events.go`（47）：`recordContextManagement` 写 context.managed（随机 management_id）。

### internal/agentruntime（第 3 段：轮前上下文策略、压缩、历史、后台输入、Profile、Goal 评估）

25. `context_policy.go`（152）：轮前（pre-turn）上下文策略。
    - 必读：`PrepareMessages`、`PrepareMessagesWithHints`、`Compact`、`applyContextPolicy`、`planContext(window, trigger, tokens, force)`（纯函数）、`ContextHints`。
    - 审计：级别 critical（tokens ≥ window − window/10）、elevated（tokens ≥ trigger）、normal；保留量 normal=min(window/4, trigger/3)，critical=min(window/6, trigger/4)，force+normal=min(window/3, trigger/2)；压缩条件 = force || tokens≥trigger。策略不读取 Team 累计 token/费用，不因消费达到 80% 而在半窗口提前压缩；手动压缩保留量也不随 Team 余额变化。末尾是 user 且非 force 时 pending=1；Team/Agent 硬配额和 Goal guard 仍在各自准入边界生效。
26. `compaction.go`（89）：`compactWithResult`、`canFallbackSummary`。
    - 审计：最新单条消息超窗口直接报错；摘要失败只在 critical 且 `canFallbackSummary` 时退到抽取式；取消、超时、ErrBudgetExceeded、ProfileConstraintError 不允许降级（不掩盖存储/账本失败），ErrContextCapacity 与 modelInvocationError 允许；结果不变小就拒绝；写 v1 context.compacted，retained_messages 排除 pending 输入，防止重放时用户消息重复。
27. `compaction_transcript.go`（152）：`compactionTranscript`、`reduceCompactionEntry`、`latestMessageCountWithCounter`、UTF-8 边界函数。单条上限 32KiB（头 + 8KiB 尾）；重复的 1/2 消息块按不含 call ID 的签名折叠；保留尾部向前扩展越过 Tool 消息，保证工具结果不与其 assistant 调用分离。
28. `context_summary.go`（157）：`summarize`、`extractiveCompactionSummary`、`boundSummaryInput`、`anchorGoal`。
    - 审计：摘要请求经 recordingModel（turnID `compact-`+随机、snapshot `context-summary-v2`、purpose `compaction`、无工具、MaxTokens≤2048），摘要与修复均受完整容量检查、配额预留和 usage/失败预留核销；预算不足明确返回错误，不能用 fallback 绕过。仅 `purpose=compaction`（含修复）要求 Profile 累计输出余额覆盖计划输出额度：不足返回 `ProfileConstraintError`，恰好足额正常，不静默缩短摘要；普通主请求与 Goal assessment 的配额行为不变。格式失败只修复一次，再失败退到抽取式且不返回错误；`anchorGoal` 在摘要前置 "Active Goal (authoritative...)"。
29. `context_summary_format.go`（116）：`contextSummary`、`parseContextSummary`、`encodeContextSummary`。64KiB 上限，允许 ```json 围栏；DisallowUnknownFields、只能一个对象；version=1、source=semantic、task 非空；各段必须是非 nil 数组且无空项；semantic 摘要禁止 Excerpts。
30. `history.go`（272）：`LoadHistory`、`LoadTranscript`、`projectHistory`、`durableContextMessages`。
    - 审计：v1 压缩点把历史重置为摘要+保留消息；v2 挂到所属 turn 的偏移处，只有该 turn 写入 turn.completed 后生效；version>2 报错；只投影已完成 turn（transcript 模式除外）；tool 结果按 call_id 归属 turn；非 transcript 模式在 assistant 消息上做 provider 校准；去掉 ReasoningContent。
31. `background.go`（271）：后台（Agent 间消息）输入。
    - 必读：`BackgroundMailbox`、`WithMailbox`、`NewBackgroundInput`、`ClaimBackgroundInput`、`SettleBackground`、`settleBackground`、`prepareBackgroundInput`、`spliceBackgroundInput`。
    - 审计：mutex 下懒加载已记录块；Claim(turnID, seq+1)；新消息先 `appendBackgroundRecord` 持久化再送入模型；恢复时按记录位置拼回；已完成 turn 的消息被确认，未完成 turn 在 releaseOpen 时释放。
32. `profile.go`（307）：Profile 绑定。
    - 必读：`WithProfile`、`resolveProfileModel`、`TrimInheritedContext(ForProfile)`、`executionConfigID`、`permitsProfileTool`、`profileToolGuard`、`profileOptions`、`profileTokenUsage`、`ProfileConstraintError`。
    - 审计：revision 必须匹配；不同 ModelProfile 禁止回退到父模型并重置限制与校准；`executionConfigID` 是配置与 profile 指纹的 sha256，恢复时用于漂移检测；shared_readonly 只允许 read/pure 风险及 agent.control 的 task_/agent_ 工具；`BeforeStart` 持锁统计会话内 tool.started 对比 MaxToolCalls，release 为 Unlock（串行化）；主请求与 Goal assessment 的输出预算仍从剩余量计算 MaxTokens，compaction 的计划输出额度不足则拒绝。
33. `model_goal_tools.go`（36）：`goalToolOptions`。只在 auto 激活时按 GoalForTurn 隐藏 goal_start/goal_report——这只是可见性，强制执行在 AdmissionGuard。
34. `goal_verifier.go`（87）：`fileGoalVerifier.Verify`。只有 file_content 判据可机械验证，其它种类 Complete=false；要求 workspace_read 为 RiskRead 且 revision builtin-v2；每次用新的 `verify-`+随机 CallID 调 `pipeline.Execute`，避免命中重放；max_bytes 64KiB+1 检测截断；文件不存在视为未满足；内容必须完全相等。
35. `goal_evaluator.go`（160）：`modelGoalEvaluator.Evaluate`、`goalObservations`、`decodeGoalVerdict`。turnID=goalID+`-eval-`+round 并恢复请求序号；purpose goal_assessment、无工具、MaxTokens≤1024；最多 2 次，格式错误追加纠正消息；观测只取本 goal 所属 turn 的工具事件（单条 8KiB、总尾部 24KiB，带截断标记）。
36. `strategy_shadow.go`（107）：`CompareStrategy`、`shadowFixture`。每个 fixture 在独立 memorystore、空注册表、`policy.DefaultDeny` 中运行（无工具副作用）；总 2 分钟、单例 30 秒；输出与 Expected 完全相等才通过；共享同一模型对象。

### internal/agentruntime（第 4 段：测试与跨包调用）

测试（★ 表示值得与源码对照读）：

- ★`runtime_test.go`（327）：Eino 循环经策略管线写持久事件；Resume 恢复持久请求序号；Resume 拒绝模型快照漂移；审批被拒返回工具结果并继续。对照 factory_turn.go、factory_snapshot.go。
- ★`request_context_test.go`（230）：工具循环中压缩，重载只含已完成上下文；请求内 checkpoint 不发布中断 turn；超大输入+工具 schema 派发前拒绝；刷新历史不导致校准漂移；外置工具输出不回到重载上下文。对照 request_context.go、history.go。
- ★`compaction_test.go`（315）：持久压缩边界、外置大工具结果、跨压缩锚定 Goal、critical 抽取回退、手动强制压缩、重复工具循环折叠。
- ★`compaction_checkpoint_test.go`（120）：保留消息恢复、不重复 pending 输入、尾部保留完整工具组。
- ★`compaction_safety_test.go`（98）：critical 不把取消/存储失败降级、不提交变大的摘要、不掩盖请求日志或结算失败。对照 `canFallbackSummary`。
- ★`context_summary_format_test.go`（170）：显式事实段、最多修复一次且计费、调用失败不变成成功回退、抽取式摘要有界。
- `context_assessment_test.go`（205）、`context_policy_test.go`（50）：重放上下文不计临时 reasoning；turn 末只标记 pending；输入与工具只计一次；上下文阈值、分级与强制保留量（对照 `planContext`）。
- ★`model_accounting_test.go`（272）：按报告或预留计费（含失败）；未派发退款且不调 provider；预算跨 factory 重建；压缩与重复评估共用账本且请求唯一。
- ★`durable_accounting_test.go`（172）：结算前崩溃收据可存活；派发持久化失败不调 provider；序号包含无事件的预留。
- ★`responses_failure_test.go`（56）：截断的 Responses 流不能执行已流出的工具调用。
- `model_test.go`（50）、`background_test.go`（130）、`history_transcript_test.go`（54）、`snapshot_admission_test.go`（51）、`goal_evaluator_test.go`（73）、`profile_flow_test.go`（84）、`strategy_test.go`（164）。
- ★`profile_test.go`（284）：profile 作用于工具/指令/模型/会话；工具守卫并发累计预算与重放；恢复拒绝指令与限制漂移；继承上下文按 tokenizer 裁剪且保持 UTF-8。
- ★`turn_durability_test.go`（77）、★`turn_modes_test.go`（123）、★`recovery_abandonment_test.go`（71）：checkpoint 存在才可恢复；准入策略从结构化事件还原而非重新分类；已放弃 turn 即使预构造运行时也不能执行（对照 turn.go:begin）。

跨包关键调用：

- session：`EventStore.Append`、`VisitEvents`、`CheckRecovery`、`CheckpointStore.Get/Delete`、`CorrelatedEventStore`。
- tool：`registry.Snapshot/RetainSnapshot/ResolveSnapshot`、`Pipeline.Execute`（goal_verifier）、`WithExecutionGuard`（profile）、`CheckTurnCompletion`（turn_stream）。
- `einoadapter.NewToolsWithActor`：Eino 工具 → InvokableRun → `tool.Pipeline.Execute`，CallID 取 `compose.GetToolCallID(ctx)`。
- tokenusage：`ReservationLedger` / `DurableReservationLedger`（Reserve、MarkDispatched、StageSettlement、SettleReservation、ModelRequestSequence）。
- agentgoal：`Store.GoalForTurn`、`Compose`、`AdaptiveEvaluator`。

### internal/agentgoal

职责：Goal Mode 的领域模型与协议：契约/准入、goal_start 与 goal_report 工具、round 状态、硬上限守卫与验收判定器。不依赖 Eino。项目内依赖：`orchestration`、`session`、`tokenusage`、`tool`。

1. `types.go`（295）：
   - 必读：`Decision`（continue/complete/blocked）、`AssessmentStrategy`、`State`（running/completed/blocked/exhausted/failed/canceled）、`StopReason`、`Config.WithDefaults/Validate`、`Goal`、`CreateRequest`、`Round`、`Report`、`Store`、`BudgetReader`、`PendingRoundReader`、`CapGuard.Reached`、`NewID`。
   - 审计：默认 auto、adaptive、MaxRounds 4、MaxWallTime 30 分钟、EvaluationTimeout 30 秒；Report blocked 必须带 blockers；`Store.AdmitGoal` 要求原子且幂等，契约不可替换；`CapGuard` 依次判断 nextRound>MaxRounds、墙钟超时、预算耗尽。
2. `activation.go`（17）：Activation auto/always；`RequiresInitialGoal = Enabled && Always`。
3. `admission.go`（119）：
   - 必读：`StartToolName`、`AdmissionInstruction`、`Contract.Validate`、`validCriterionPath`、`AdmissionFunc`、`WithAdmission`、`AdmissionEnabled`、`StartBinding`。
   - 审计：契约需要 objective、scope、1–20 条判据、总量 ≤128KiB；file_content 需要 path 与 ≤64KiB content，semantic 的 path/content 可选且不成为精确断言；StartBinding 为 RiskPure，要求 ctx 中有准入函数与可信 `InvocationFromContext`，拒绝尾随数据。
4. `admission_guard.go`（78）：
   - 必读：`AdmissionGuard`、`NewAdmissionGuard`、`BindTurn`、`BeforeCall`、`BeforeComplete`、`requireActive`、`AdmissionRequiredCode`。
   - 审计：requested 为 `atomic.Bool`；`BindTurn` 写 turnID 无锁（在 turn.Run 之前由 beforeRun 调用，实际竞态可能性低，但没有同步保证）；非本 turn 调用忽略；goal_start 置 requested 并放行；goal_report 与写风险工具需要活动 goal；requested 后 execute/secret 工具也需要；`BeforeComplete` 在 requested 时要求活动 goal，否则返回 PreconditionError（经 `tool.CheckTurnCompletion` 生效）。
5. `tool.go`（55）：`ReportToolName`、`ProtocolInstruction`、`Binding`。RiskPure；需要可信调用上下文；DisallowUnknownFields；调用 `store.RecordReport`。
6. `evaluator.go`（117）：`Evaluation`、`Verdict.Validate`、`Evaluator`、`EvaluatorFunc`、`SelfEvaluator`、`HybridEvaluator`、`Compose`。Self 无报告时返回 continue；Hybrid 只有两者一致才 complete/blocked，否则 continue。
7. `verification.go`（65）：`Verification{Complete,Satisfied,Evidence}`、`Verifier`、`AdaptiveEvaluator`、`joinEvidence`。
   - 审计：verifier 基础设施错误直接上抛，不被当作重做请求；未满足且 Report 非 blocked → continue "acceptance checks failed"；全覆盖且全通过且 Report=complete → 不调模型直接 complete；其余交给 Fallback；Fallback 判 complete 但机械检查失败 → 降为 continue；Fallback 为 nil 报错。只有部分判据可机械验证时（Complete=false），即使通过也必须走语义评估。

测试：★`admission_guard_test.go`（93，守卫依据任务阶段而非"是否用了工具"）、★`admission_test.go`（56）、★`verification_test.go`（78，语义评估不能覆盖失败的检查，对照 verification.go 全文）、`activation_test.go`（22）、`evaluator_test.go`（40）。

跨包：被 agentruntime 调用（GoalForTurn、Compose、AdaptiveEvaluator、ProtocolInstruction/AdmissionInstruction）；被 conversation 调用（NewAdmissionGuard、WithAdmission、CapGuard.Reached、Store 全部 round 方法）。

### internal/conversation

职责：会话级编排：串行化用户 turn，维护历史投影，驱动 Goal Mode 外循环（准入、轮次、评估、终止）与崩溃恢复。项目内依赖：agentruntime、agentgoal、orchestration、session、tool。

阅读顺序：
1. `conversation.go`（48）：`Conversation{factory, sessionID, mutex, history, completedTurns}`、`Open`。审计：Open 时 LoadHistory + CountCompletedUserTurns。
2. `history.go`（61）：`refreshHistory`、`History`、`cloneMessages`、`CompletedTurns`。审计：只有读取成功才替换投影；JSON 深拷贝。
3. `run.go`（45）：`RunTurn`。审计：空输入报错；mutex 覆盖整个 turn；Always → NewID + CreateGoal + runGoal；auto → runAutoTurn；否则 runInnerTurn，成功后 completedTurns++。
4. `execution.go`（66）：`runInnerTurn`、`resumeInnerTurn`。审计：有 GoalID 用 WithActiveGoal，ctx 未启用准入则 WithoutGoalMode；顺序是 PrepareMessagesWithHints（轮前压缩）→ refreshHistory → NewTurn → beforeRun(turnID) → turn.Run → refreshHistory；事后投影失败不影响已持久化结果。
5. `candidate_events.go`（43）：`candidateEventBuffer`。缓冲文本 delta 与无工具 assistant 消息；带工具调用的 assistant 清空 content 后透传；Flush 只在候选被接受时发出，被拒候选不泄露到 UI。
6. `admission.go`（135）：`runAutoTurn`、`errGoalDeadline`、`completedGoalRound`。
   - 恢复时用 TurnInput 还原原输入与原开始时间，用 AdmissionRequestedForTurn 还原 requested。
   - 截止 = started + MaxWallTime，cause = errGoalDeadline。
   - 准入闭包：持锁、校验身份、goal ID = "goal-"+turnID（幂等），先 `CapGuard.Reached(probe,1)` 探测再 `AdmitGoal(request, turnID)`，同一 turn 即第 1 轮。
   - autoSink 在 goal 出现前直通，之后发 executing 阶段并缓冲候选；无 goal → completedTurns++；WaitSuspendedError 透传，其它错误 → stopGoalError。
7. `goal_protocol.go`（26）：`goalContextText`、`continuationPrompt`。
8. `goal_loop.go`（127）：`runGoal`、`goalStore`。
   - evaluator 为 nil → failGoal；截止基于 goal.StartedAt。
   - 首轮或恢复轮直接用 round.Number，否则先 guard.Reached（到达 → exhaustGoal）。
   - beforeRun 在 turn.Run 前调用 StartRound（轮次先落盘）。
   - ReportForTurn → verifying → 在 EvaluationTimeout 内 Evaluate，评估超时作为错误。
   - verdict.Validate → FinishRound(verdict, candidate) → GetGoal → applyGoalVerdict；continue 时输入换成 continuationPrompt 并发 retrying。
9. `goal_transition.go`（51）：`goalTransition`、`applyGoalVerdict`。complete/blocked → finishGoal；continue → guard.Reached(Round+1) 决定 exhaust 或下一轮；其它决策报错。实时路径与恢复路径共用这套语义。
10. `goal_outcome.go`（79）：`finishGoal`、`exhaustGoal`、`failGoal`、`goalDurableContext`、`goalPhase`、`stopGoalError`。终态写入失败 → stopGoalError；Canceled → StateCanceled，其它 → Failed；都在 WithoutCancel+5s 上下文中写；errGoalDeadline → exhaustGoal(StopMaxWallTime)，用户取消不算资源耗尽。
11. `goal_recovery.go`（61）：`resumeGoal`。轮次必须是当前轮；已评估（CompletedAt）→ 复用已提交 verdict 与候选，不再执行或评估；continue 用原截止，终态用持久 ctx；已有 CompletedTurn → 只跑评估；否则按 resumeTurnID 续跑。
12. `recovery.go`（141）：`Recovery`、`Recoveries`、`DiscoverRecoveries`、`recoveriesForTurns`、`RecoverRun`。离线发现不执行；跳过 abandoned/archived；goal 非 running 或轮次过期跳过；phase 分 verdict/assessment/execution，执行阶段无 checkpoint → unavailable；PendingRounds 没有 turn.started → unavailable（不隐藏孤儿 goal）；RecoverRun 要求绑定 RunID，取该 run 最新 turn。
13. `resume.go`（58）：`ResumeTurn`、`resumeTurn`。持锁；CheckTurnRecovery；goal running → resumeGoal；goal 已停止 → 报错；AutoAdmissionForTurn → runAutoTurn("", turnID)；否则 resumeInnerTurn 并计数。
14. `background.go`（65）：`ErrBackgroundBudgetExhausted`、`PendingBackground`、`RunBackgroundTurn`。强制 WithoutGoalMode；先 NewTurn 再 ClaimBackgroundInput 与 PrepareMessagesWithHints（与用户路径相反，因为 Claim 需要 turnID）；RunBackground 传 prepared[:len-1] 作为历史。
15. `context.go`（47）：`ContextUsage`、`ContextUsageFor`、`Compress`。Compress = factory.Compact(force)，有变化才刷新历史。

### internal/conversation（测试）

测试（★ 表示值得与源码对照读；行为按测试名推断）：
- ★`admission_test.go`（289）：截止时间中断内循环并持久化"耗尽"；用户取消不算耗尽；恢复已过期 goal 不重置截止也不调模型；准入失败不丢原任务也不削弱判据；auto 模式下普通问答与只读工具不建 goal；文件任务同 turn 机械验收不需模型；验收失败则继续直到通过；只读覆盖任务走语义评估；评估超时停止且不重复任务。
- ★`admission_failure_test.go`（143）：被拒准入不能改报告或按普通聊天完成；同一 Eino turn 内准入恢复只写一次；.gitignore 语义判据走评估器而非精确比较。
- ★`goal_test.go`（294）：继续后完成且不泄露被拒候选；硬上限与语义决策分开报告；预算耗尽返回可见的类型化结果；问候绕过混合评估记为直接 turn；混合 turn 重开后计数正确；Always 保持显式 goal。
- ★`goal_recovery_test.go`（207）：续跑耗尽仍保留候选；执行后恢复只做评估；已提交 verdict 不再执行或评估；只在已提交拒绝后继续。
- ★`goal_transition_test.go`（161）：实时与恢复终态语义一致；终态 verdict 在执行截止过期后依然生效；continue 不派发且保留守卫失败。
- `recovery_test.go`（123）：发现只报告持久阶段不执行；不隐藏孤儿 goal 或缺失的 checkpoint；RecoverRun 找到评估阶段并复用终态结果。
- `recovery_abandonment_test.go`（60）：已放弃 goal 不能恢复评估或已提交 verdict。
- `execution_test.go`（142）：投影失败在建 turn 前停止；准入回调先于执行；事后投影失败保留持久结果。
- `conversation_test.go`（76）：空输入、历史深拷贝、候选缓冲。`direct_stream_test.go`（61）：问候文本在模型流结束前就到达 UI。

### 运行时端到端调用链

(a) 普通用户 turn
1. conversation/run.go `RunTurn`（全程持锁）→ execution.go `runInnerTurn`（WithoutGoalMode）。
2. agentruntime/context_policy.go `PrepareMessagesWithHints` → `applyContextPolicy` → `planContext` →（需要时）compaction.go `compactWithResult`，写 v1 context.compacted 与 context.managed【轮前压缩】。
3. conversation/history.go `refreshHistory` → agentruntime/history.go `LoadHistory`。
4. factory_turn.go `NewTurn`：strategy.go `withActiveStrategy` → background.go `settleBackground` → Snapshot/RetainSnapshot →（新会话）session.created → eino_turn.go `buildTurn`（einoadapter.NewToolsWithActor、adk.NewChatModelAgent、adk.NewRunner）。
5. turn.go `Run` → `begin` → `start`：一次 Append 写 turn.started + user.message → runner.Run(WithCheckPointID)。
6. Eino 调 model.go `recordingModel.Stream`：request_context.go `prepareModelInput`（goalToolOptions → prepareBackgroundInput → prepareRequestContext/compactRequest【请求内压缩，v2】→ profileOptions → captureRequest）→ reserve → model.requested → model_accounting.go `markDispatched` → inner.Stream（如 modelgateway/responses.go `Stream` → responses_stream.go `streamResponsesBody`）→ `recordAssistantMessage`（settleUsage → assistant.message）。
7. 工具：Eino ToolsNode 串行执行 → einoadapter/tool.go `InvokableRun` → tool/pipeline.go `Execute`（见工具链 (a)）。
8. 循环直到模型给出无工具调用的最终消息 → turn_stream.go `consumeIterator` → `tool.CheckTurnCompletion` → `completedContext` → turn.completed → 删 checkpoint → mailbox.Acknowledge。
9. `refreshHistory`。

(b) 进程重启后恢复中断 turn
1. 发现：conversation/recovery.go `Recoveries`/`DiscoverRecoveries` → agentruntime/recovery.go `LoadTurnRecords`（HasCheckpoint）；按 run 恢复走 `RecoverRun`。
2. resume.go `ResumeTurn` → `resumeTurn`：CheckTurnRecovery → GoalForTurn。goal running → goal_recovery.go `resumeGoal`；auto → admission.go `runAutoTurn(resumeTurnID)`；其它 → execution.go `resumeInnerTurn`。
3. factory_turn.go `ResumeTurn`：withTurnStrategy → CheckTurnRecovery → checkpoints.Get → snapshotIDForTurn → ResolveSnapshot → validateTurnModel → RetainSnapshot → modelRequestSequence → goalModeForTurn/activationForTurn → buildTurn。
4. turn.go `Resume` → `begin` → runner.Resume(checkpointID)，不重写 turn.started。后台块按记录位置拼回，请求序号接着往后走；已成功的工具调用由 pipeline `replaySuccess` 重放，不重复执行。
5. consumeIterator(resumed=true) → turn.completed。失败时 turn_durability.go `fail` → turn.failed{recoverable} → checkpoint.created。

(c) Goal Mode 一轮到验收判定
1. 准入（auto）：admission.go `runAutoTurn` → NewAdmissionGuard + tool.WithTurnGuard → 截止时间 → WithAdmission 闭包 → runInnerTurn（beforeRun → BindTurn）。模型调 goal_start → agentgoal/admission.go `StartBinding` → 闭包（CapGuard.Reached 探测）→ Store.AdmitGoal，当前 turn 即第 1 轮。goal_report → agentgoal/tool.go `Binding` → RecordReport。之后进入 goal_loop.go `runGoal(first)`。Always 模式：RunTurn → CreateGoal → runGoal。
2. runGoal：guard.Reached → executing → runInnerTurn(ContextHints, WithActiveGoal；beforeRun → StartRound) → 同 (a) 的内循环。
3. ReportForTurn → verifying → evaluator.Evaluate（EvaluationTimeout）。adaptive：agentgoal/verification.go `AdaptiveEvaluator` → agentruntime/goal_verifier.go `fileGoalVerifier.Verify`（pipeline.Execute workspace_read）→ 需要时 goal_evaluator.go `modelGoalEvaluator.Evaluate`（purpose goal_assessment）。
4. verdict.Validate → FinishRound → GetGoal → goal_transition.go `applyGoalVerdict`。complete/blocked → goal_outcome.go `finishGoal`（FinishGoal → buffer.Flush → stopped）；continue → guard.Reached → exhaustGoal 或 continuationPrompt 进入下一轮。

上下文压缩的四个触发点：
1. 轮前：execution.go `runInnerTurn`（以及 background.go `RunBackgroundTurn`）→ PrepareMessagesWithHints → compactWithResult，写 v1，立即生效。
2. 请求内：每次模型派发前 recordingModel.Generate/Stream → prepareRequestContext → compactRequest（before≥trigger），写 v2，只有该 turn 完成后才进入历史。
3. 手动：conversation/context.go `Compress` → factory.Compact(force)（TUI /compress）。
4. 轮后：completedContext 只把状态标为 compaction_pending，不调模型，下次派发时再压缩。

## 第 3 阶段：持久化（internal/session/sqlitestore）

按第 4 节的拆分读：本阶段读第 1 节、A、B、H、I 与第 3 节；L 留到第 4 阶段，C–G、J、K 留到第 5 阶段对照读。

### internal/session/sqlitestore

文件前缀：internal/session/sqlitestore/。非测试文件 43 个，共 7814 行；测试文件 22 个，共 3923 行（wc -l）。所有非测试文件均已逐个打开阅读，未修改任何文件。

#### 1. 包职责、依赖、连接/事务模型

- 职责：项目唯一 SQLite owner。负责 session 事件与 checkpoint、orchestration 事件流及投影（team/agent/thread/task/mailbox/wait/goal/flow/workspace view/merge）、token 用量账本、runtime snapshot 引用、插件目录/构建/测试证据、strategy 目录、LSP 诊断投影，以及离线 retention、插件回收、放弃恢复。
- 项目内依赖：orchestration、session、retention、tool、agentgoal、plugin、workspacecoord、agentflow、tokenusage、lsp、pluginbuild、strategy。artifact 只在 merge_snapshot_test.go 中引用。
- 驱动：modernc.org/sqlite（纯 Go）+ database/sql。
- 连接：Open 设置 SetMaxOpenConns(1)、SetMaxIdleConns(1)，单连接，所有读写串行。
- 文件库 DSN：foreign_keys(ON)、busy_timeout(5000)、journal_mode(WAL)、_defensive=1；父目录 MkdirAll 0700。`:memory:` 用 `file:local-agent-session-<随机>?mode=memory&cache=shared`，带 foreign_keys/busy_timeout/_defensive，不设 WAL。
- busy 处理：只有 busy_timeout=5000ms，无应用层重试。DSN 未设 _txlock，事务按 SQLite 默认 DEFERRED 开始（驱动默认值未确认）。跨进程互斥依赖调用方持有的 runtimeowner 锁（维护函数注释要求），本包不实现锁。
- 事务写法：BeginTx(ctx, &sql.TxOptions{} 或 nil) + defer Rollback。helper 统一接受 rowQuery/readQuery 端口。query.go 注释：写操作里 helper 必须拿 tx 而不是池，单连接下用池会死锁或看不到未提交状态；transaction_boundaries_test 锁定了这一点。
- 完整性：session/orchestration 事件存规范 JSON 的 SHA-256，读时校验；checkpoint、插件、snapshot、构建、报告、回执都存摘要并在读时校验，失败大多返回非哨兵 errors.New。

#### 2. 按领域的推荐阅读顺序

##### A. 打开、迁移、基础设施（最先读）

store.go（97）
- 职责：Open/Close 与各子 store 访问器。
- 必读：Database、Open（migrate 失败即关库）、dataSourceName、SessionEvents/Checkpoints/Orchestration 访问器（共享同一 *sql.DB）、Close（sync.Once）。
- 审计：SetMaxOpenConns(1) 是全包并发正确性的前提，新增 store 不得另开 *sql.DB。

migrations.go（93）
- 职责：forward-only 迁移执行器。
- 必读：migrate、applyMigration、migrationChecksum、validateLegacySessionSchema。
- 审计：每个版本一个 tx（DDL 与账本 insert 同提交）；已应用版本校验 checksum；MAX(version) 超出代码支持返回 "newer than supported"；没有 down 迁移；最后要求 schema_meta 中 session_events=1。

schema.go（607）
- 职责：migrations 切片 v1–v36。注释要求 append-only、逐字节 checksum；v6/26/27 跨领域不可拆；修正只能加新版本。
- 审计：testdata/migrations.sha256 只钉住 v1–v34（34 行）。TestPublishedMigrationContract 只比较 index < len(checksums)，所以改 v35/v36 的 DDL 不会被测试发现。workspace_leases（v11/v12）在全仓库 .go 中只出现在 schema.go，没有读写代码，是遗留表。

codec.go（67）：randomID（128 bit hex）、checksum(session.Event)、sha256Hex、cloneBytes。审计：checksum 的字段集合就是事件不可变契约，新增事件字段必须同步。
query.go（32）：rowQuery/readQuery/rowScanner 端口，以及对 sql.DB/Tx/Row/Rows 的编译期断言。
profile.go（10）：nonNilJSON（nil 转空 RawMessage）。
compact.go（29）：Database.Compact。integrity_check 不是 "ok" 就拒绝；VACUUM；wal_checkpoint(TRUNCATE) 的 busy≠0 报错。注释要求离线 owner、不删证据；调用方是 internal/app/maintenance.go 的 withOfflineDatabase。

##### B. Session 事件与 checkpoint

events.go（203）
- 必读：SessionEventStore.Append→appendSessionEvents、Load、CurrentSequence、LoadByCorrelation、scanSessionEvent、currentSequence、sessionEventColumns。
- 审计：expected≠AnySequence 且≠当前序号返回 session.ErrSequenceConflict；PK(session_id, sequence) 兜底；OccurredAt=now+索引纳秒；空 sessionID 返回 session.ErrInvalidEvent；Load 用 boundedLimit（≤0 取 1000，上限 10000）；LoadByCorrelation 无 limit；checksum 不符返回非哨兵错误。appendSessionEvents 被 retention.go 和 recovery_control.go 在各自 tx 内复用。

checkpoints.go（66）
- 必读：Get、Set、Delete。
- 审计：Set 是 upsert（直接覆盖，无幂等冲突）；Get 校验 sha，缺失返回 (nil,false,nil)；Delete 是硬删除；错误都是 errors.New，无哨兵。

##### C. Orchestration 事件流与 root thread

orchestration_events.go（271）
- 必读：Append、appendOrchestrationEvents、LoadStream、LoadGlobal、scanOrchestrationEvent、currentOrchestrationSequence、orchestrationChecksum、boundedLimit。
- 审计：appendOrchestrationEvents 先读 MAX(stream_sequence) 和 MAX(global_sequence) 再插入，expected 不符返回 orchestration.ErrSequenceConflict；UNIQUE(stream_type,stream_id,stream_sequence) 兜底；全局序号的正确性依赖单连接串行。所有投影写都把事件放在同一 tx，这是“投影与审计原子”的核心。scanOrchestrationEvent 校验 EventID、TeamID 与 checksum。

orchestration.go（168）
- 必读：OrchestrationStore、ListTeams、EnsureRootThread、RootThreadBySession、rootThreadBySession。
- 审计：EnsureRootThread 单 tx，按 session 查到即返回（幂等）；新建时插入 teams/agents/agent_threads 并追加 TeamCreated、AgentCreated、AgentQueued、ThreadCreated；缺失返回 ErrNotFound；校验失败返回 ErrInvalidEvent。

##### D. Agents / spawn

agents_query.go（125）：agentSelect、GetAgent（ErrNotFound）、ListAgents（states 用占位符 IN）、agentByID、scanAgent（解码 result JSON）。先读它，下面两个文件依赖它。

agents_spawn.go（223）
- 必读：hashSpawnRequest、CreateChild、FindSpawn、FindSpawnRequest。
- 审计：幂等记录在 idempotency_records，scope 为 "spawn:"+team；hash 不符返回 ErrIdempotencyConflict，旧行空 hash 视为匹配；hash 排除 CorrelationID，WorkspaceAuto 映射为 ""（兼容旧 hash）；父 Agent 必须同 team 且非终态、非 canceling；同一事务内 requireActiveTeam 拒绝非 active 的 Team（ErrTeamNotActive）；depth=父+1；带 TaskID 时同 tx 调 claimTaskForAgentTx；FindSpawn/FindSpawnRequest 读池，FindSpawn 不比 hash，CreateChild 内部会重新校验。

teams.go（144）
- 必读：GetTeam、TransitionTeam→transitionTeam、teamTransitionEvent、reopenTeam、requireActiveTeam。
- 审计：Expected≠version 返回 ErrTeamStateConflict，再调 ValidateTeamTransition；事件 payload 为 {from,to,reason}，目标 active 记 team.reopened；reopenTeam 由 EnsureRootThread 在恢复会话时调用，只处理 completed/failed/canceled，canceling 保持不变。

agents_state.go（119）
- 必读：TransitionAgent、agentTransitionEvent。
- 审计：Expected≠version 返回 ErrAgentStateConflict；调用 ValidateAgentTransition 和 Result.Validate；UPDATE 带 version=? 但不检查 RowsAffected；进入 Running 时前置 RunAdmitted 事件；终态带 result 时追加 ResultRecorded 和 RunCompleted/RunFailed，Canceled 没有 run 事件。

##### E. Tasks

tasks_query.go（118）：taskSelect、GetTask（ErrNotFound）、ListTasks（校验 states，按 priority DESC）、taskByID、scanTask。

tasks.go（310）
- 必读：hashTaskRequest、CreateTask、TransitionTask、openSnapshotMerges、agentBelongsToTeam。
- 审计：
  - 幂等 scope 为 "task:"+team，空 hash 视为匹配。
  - Expected 不符返回 ErrTaskStateConflict。
  - 有 VerificationProfile 时 InProgress→Completed/AwaitingMerge 返回 ErrTaskVerificationRequired，自审返回 ErrTaskReviewerRequired。
  - AwaitingMerge 要求 open snapshot merge>0，Completed 要求=0（open = pending/running/conflicted）。
  - Blocked→Claimed/InProgress 返回 ErrTaskNotReady；Claimed 指派人不符返回 ErrTaskClaimConflict。
  - Ready 清空 assignee 并检查依赖；从 claimed/in_progress 释放时 attempt+1；Claimed 写 claim_expires_at=now+30min；Completed 调 unblockDependentTasksTx。
  - UPDATE 不检查 RowsAffected。agentBelongsToTeam 返回 ErrNotFound 或 ErrInvalidEvent。

task_claims.go（147）
- 必读：ClaimTask、ReleaseTask（都委托 TransitionTask）、claimTaskForAgentTx、ReapExpiredTaskClaims。
- 审计：claimTaskForAgentTx 要求 Ready 且无 assignee（否则 ErrTaskClaimConflict），依赖完成（否则 ErrTaskNotReady）；Reap 单 tx，只回收没有活跃 Agent（created/queued/running/waiting/canceling）持有的 claim，UPDATE 带 state+version+到期守卫，且只在 RowsAffected>0 时发 TaskClaimExpired。

task_dependencies.go（243）
- 必读：AddDependency、RemoveDependency、ListDependencies、changeDependency、unblockDependentTasksTx、dependenciesComplete。
- 审计：自依赖或递归 CTE 发现环返回 ErrTaskDependencyCycle；两端同 team；拒绝终态任务；ON CONFLICT DO NOTHING 的空操作直接提交且不发事件；加依赖使 draft/ready 变 Blocked，删依赖使 Blocked 回 Ready，两者都清 assignee；删除是硬 DELETE，轨迹只留在事件里；unblockDependentTasksTx 只有 state 守卫，没有 version 守卫。

##### F. Mailbox

mailbox.go（290）
- 必读：Enqueue、sameMessageRequest、FindMessage、ListMessages、transitionMessage、scanMailboxMessage。
- 审计：幂等用 (team, idempotency_key) 部分唯一索引加逐字段比较（team/sender/recipient/type/body/correlation），不符返回 ErrIdempotencyConflict；只允许父子间投递；终态 Agent 只能收发 Result/Control 消息；收件序号来自 mailbox_recipient_sequences 的 upsert（sequence=next-1）；transitionMessage 拼接的列名来自两个常量，不是外部输入；状态 enqueued→delivered→acknowledged，重复同一目标幂等，其他返回 ErrInvalidEvent；ListMessages 的 limit ≤0 或 >1000 时取 100。

mailbox_ledger.go（153）
- 必读：mailboxColumns、ClaimMessages、AcknowledgeTurn、ReleaseTurn、settleTurn。
- 审计：ClaimMessages 单 tx，把 enqueued 或 delivered 且 consumer_turn_id∈('',turnID) 的消息绑定到 turn，发 MailboxBound；已绑定本 turn 的消息再次返回，所以崩溃后可续；空 turnID 被拒绝（测试锁定）。

##### G. Wait sets

waitsets.go（421）
- 必读：hashWaitRequest、CreateWaitSet、transitionWaitSet、ConsumeWaitSet、CancelWaitSetsForAgent、scanWaitSet。
- 审计：
  - (team,key)+hash 幂等，空 hash 视为匹配。
  - Agent 缺失返回 ErrNotFound，跨 team 或非 Running 返回 ErrInvalidEvent，RunID 不一致返回 ErrAgentStateConflict。
  - Agent→Waiting 的 UPDATE 带 state+version 守卫并检查 RowsAffected，是全包少数这样做的地方。
  - transitionWaitSet 只迁移 pending，其他状态原样返回（幂等）；Agent Waiting→Queued 同样检查 RowsAffected。
  - ConsumeWaitSet 把 resolved 改为 consumed，与 resolve 分开，保证崩溃后可以重新恢复。
  - CancelWaitSetsForAgent 不在一个 tx 内，逐个 transition 并忽略 ErrInvalidEvent，可能部分完成。

##### H. Token usage 账本

usage.go（426）
- 必读：RecordUsage→recordUsageTx、usageRecordMatches、Reserve、SettleReservation、ReleaseReservation、UsageReport、ConfigureBudget、Budget、CanAdmit、budgetExceeded。
- 审计：
  - RecordUsage 相同记录 no-op，不同返回 ErrIdempotencyConflict；RecordID 默认 team_request。
  - Reserve 以 (team, request_id)+hash 幂等，旧行空 hash 退回比较数量；准入按 SUM(records)+SUM(pending reservations)，超出返回 tokenusage.ErrBudgetExceeded。Reserve 用严格 `>`，budgetExceeded/CanAdmit 用 `>=`，边界不一致。
  - SettleReservation：缺失返回 ErrNotFound，owner 不符或已 released 返回 ErrInvalidEvent，已 settled 为 no-op。
  - ReleaseReservation 是事务外的单条带守卫 UPDATE（pending、lifecycle_version=1、未 dispatched、无 outcome），0 行时回读状态决定返回 nil、ErrNotFound 或 ErrInvalidEvent。
  - UsageReport 动态拼接条件，值全部参数化。ConfigureBudget 为 upsert。

usage_recovery.go（225）
- 必读：ModelRequestSequence、MarkDispatched、StageSettlement、RecoverReservations、normalizeReceipt、usageIdentityMatches。
- 审计：MarkDispatched 一次性，RowsAffected≠1 返回 ErrInvalidEvent；StageSettlement 在预留缺失时直接返回 sql.ErrNoRows（未映射为 ErrNotFound），outcome 不同返回 ErrIdempotencyConflict，未 dispatch 返回 ErrInvalidEvent；RecoverReservations 单 tx、要求独占启动，对每个 pending 预留：已有记录按记录结算，有回执按回执结算，v1 未发送则释放，其余按预留量结算并标 quality "estimated_recovery"（不退还未知消耗）；lifecycle_version 只接受 0/1，损坏时整体回滚（测试锁定）。

##### I. Goals

goals.go（396）
- 必读：CreateGoal→createGoal、AdmitGoal、StartRound→startGoalRound、FinishRound、RecordReport、FinishGoal、CountGoals、goalByID、roundByNumber、reportForTurn。
- 审计：goal ID 本身是幂等键，逐字段比较且 Contract 用 reflect.DeepEqual，不符返回 ErrIdempotencyConflict；AdmitGoal 把 goal 与 round 1 放在同一 tx，不会留下孤儿 goal；新 round 必须等于 round+1 且 ≤MaxRounds；startGoalRound/FinishGoal 的 version 守卫 UPDATE 不检查 RowsAffected；很多拒绝路径用 errors.New/fmt（"not running"、"stale round"），不是哨兵；StartRound/FinishRound 在 goal 或 round 缺失时漏出原始 sql.ErrNoRows；RecordReport 每 turn 唯一，缺少活动 round 返回 ErrNotFound。

goal_recovery.go（40）：PendingRounds 列出 running goal 的当前 round；round=0 时只填 GoalID，用来覆盖 goal 创建与 turn.started 之间的崩溃窗口。

##### J. Flows

flows.go（423）
- 必读：hashFlowRequest、CreateRun、GetRun、StartStep、FinishStep、FailStep、FinishRun、SessionAgent、BeginSessionStage、BindSessionAgent、flowRunByID、flowStepByID。
- 审计：
  - hashFlowRequest 在 RuntimeSnapshotID 非空时把 RegistryGeneration 置 0（generation 只是进程内调试信息）。
  - CreateRun 用 (team,key) 严格比较 hash，没有空 hash 放行，不符返回 ErrIdempotencyConflict；发 FlowStarted。
  - StartStep：input_hash 不符返回 agentflow.ErrStepConflict；failed 步骤重试时 attempt+1、清空 outputs，但这次重试不发事件。
  - FinishStep：已 completed 时按字节比较 outputs，不同返回 ErrIdempotencyConflict。
  - FinishRun：终态只允许 completed/failed/canceled；重复调用要求 state/error/outputs 全等；UPDATE 有 version 守卫但不检查 RowsAffected。
  - BeginSessionStage 对每个 (flow, session, step) 只记录一次 spawn/continue 决策。
  - BindSessionAgent 不在 tx 内：ON CONFLICT DO NOTHING 后回读，不符返回 ErrIdempotencyConflict。
  - StartStep/FinishStep/FailStep/FinishRun 在 run 或 step 缺失时返回原始 sql.ErrNoRows；"is not running" 是非哨兵错误。

##### K. Workspace view 与 merge

workspacecoord.go（627）
- 必读：hashViewSpec、hashMergeSpec、CreateView、GetView、ListViews、ReleaseView、EnqueueMerge、supersedeMergeTx、TransitionMerge、validateMergeTransition、LinkMergeResolutionTask、viewByID、mergeColumns、scanMerge。
- 审计：
  - view 与 merge 都是 (team,key)+hash，空 hash 视为匹配；snapshot 字段只在 kind=snapshot 时进入 merge hash。
  - 事件写在 workspace 流（stream ID=teamID）。
  - EnqueueMerge 要求 view 属主一致、view active、base_revision 一致，否则 ErrInvalidEvent；view 行缺失时漏出 sql.ErrNoRows；默认 kind 为 git-patch；带 Supersedes 时调 supersedeMergeTx 并发 MergeSuperseded。
  - TransitionMerge：Expected==0 表示跳过版本检查（不是 AnySequence 语义），不符返回 ErrSequenceConflict 而不是状态冲突错误；conflicted→pending 需要关联且已完成的解决任务；允许的迁移为 pending→{running,rejected,superseded}、running→{applied,conflicted,rejected}、conflicted→{pending,rejected,superseded}；写 plan_digest。
  - LinkMergeResolutionTask 对同一任务幂等，不同任务返回 ErrIdempotencyConflict，任务须同 team。
  - ListMerges 的过滤列名来自固定列表；ListViews 不校验 states（但已参数化）；ReleaseView 对非 active 的 view 幂等返回。
  - supersedeMergeTx 与 TransitionMerge 的 UPDATE 不检查 RowsAffected。

merge_apply.go（139）
- 必读：BeginApply、MarkPathApplied、FinishApply、LoadApply、applyRecordTx。实现 workspacecoord.ApplyJournal（断言写在 merge_apply_test.go:172，而不是源文件里）。
- 审计：journal 以 merge_id 为主键，digest 不同返回 ErrInvalidEvent，merge 必须处于 Running；MarkPathApplied 用 INSERT…SELECT，只在 journal 为 started 时写入，ON CONFLICT DO NOTHING，0 行时由 applyJournalOpen 判断原因；FinishApply 的 UPDATE 没有 state 守卫，0 行返回 ErrNotFound；LoadApply 使用 ReadOnly tx。

##### L. Runtime snapshot、插件、strategy、LSP

runtime_snapshots.go（165）
- 必读：RuntimeSnapshots.Save、Load、References、ReferenceHistory。
- 审计：Save 单 tx。consumer 已在 runtime_consumer_closures，或 turn 已有 turn.abandoned 事件，返回 tool.ErrSnapshotReferenceReleased；manifest 以内容 ID 为键，同 ID 内容不同返回 ErrSnapshotUnavailable；同一 consumer 已绑定其它 snapshot 返回 ErrSnapshotReferenceConflict（不允许重绑），已 released 返回 ErrSnapshotReferenceReleased。Load 校验 ID 与规范编码，缺失和损坏统一返回 ErrSnapshotUnavailable。released 行不删除，ReferenceHistory 保留证据。

plugins.go（157）
- 必读：PluginCatalog.Save、Load、SetActive。
- 审计：Save 单 tx。(tool,revision) 在 plugin_revisions ∪ plugin_revision_tombstones 中已存在返回 plugin.ErrArtifactExists；tool 已被其他 plugin_id 占用返回 ErrToolOwned（tombstone 也计入，防止回收后被复用）；目录保留总量上限 128 MiB（errors.New）；blob 内容寻址，同 sha 不同内容返回 ErrCatalogInvalid。SetActive 以当前 active revision 作 CAS，不符返回 ErrCatalogConflict，next 先完整校验再切换；两条 UPDATE 不检查 RowsAffected。Load 逐行校验 record_sha 与身份，失败返回 ErrCatalogInvalid（fail closed）。

plugin_builds.go（114）
- 必读：BeginPluginBuild、FinishPluginBuild、PluginBuild。在 *Database 上实现 pluginbuild.BuildStore。
- 审计：build_id=retention.Digest(请求 JSON)；已存在但没有结果时拒绝重跑（中断不重放）；证据总量上限 64 MiB，单结果 256 KiB；FinishPluginBuild 守卫 result IS NULL 且要求 RowsAffected==1（结果不可改写），这条 UPDATE 不在 tx 内；PluginBuild 在缺失时返回原始 sql.ErrNoRows。

plugin_verification.go（39）
- 必读：SavePluginTest、PluginTest。实现 plugin.TestReportStore。
- 审计：report_id 是内容摘要，但 INSERT 没有 ON CONFLICT，同一报告重复保存会得到主键约束错误而不是幂等返回（调用方是否会重复保存，未确认）；读时校验摘要。

strategies.go（246）
- 必读：Save/Load、SaveSuite/Suite、SaveReport/Report、Active、Promote、Fallback、appendStrategyEvent。实现 strategy.Catalog 与 strategy.ShadowStore。
- 审计：
  - spec/suite/report 都内容寻址，ON CONFLICT(id) DO NOTHING 后回读校验摘要。strategy_specs 另有 UNIQUE(owner_session,revision)，同 owner/revision 的不同 spec 会返回原始约束错误。
  - SaveSuite 和 Promote 需要 confirmed。
  - Promote 的前置读取（Report、Load）在 tx 外；tx 内检查 quarantine，并用 strategy_active 当前 spec 与 report.PreviousSpecID 做 CAS，不符返回 orchestration.ErrIdempotencyConflict；同一 report 重复 promote 返回 nil。
  - Fallback 在 active 匹配时清空 active、写 quarantine、记事件。
  - commit 失败包装为 strategy.ErrEvidence。
  - strategy_events 用 AUTOINCREMENT，checksum 是 payload 值的摘要。
  - Active 每次重新校验发布证据与隔离状态。

lsp.go（83）
- 必读：LSPProjectionStore.SaveProjection、LoadProjection。实现 lsp.ProjectionStore。
- 审计：SaveProjection 是 upsert（覆盖），没有版本或幂等检查，属于可重建的读模型；LoadProjection 缺失返回 found=false。

##### M. Retention、维护与离线恢复（调用方须持离线 owner 锁）

建议顺序：maintenance_references → maintenance_receipts → maintenance_turns → maintenance_dependencies → retention_plan → retention → plugin_collection_plan → plugin_collection → recovery_control。调用入口是 internal/app/maintenance.go 与 internal/app/recovery.go，它们直接调用 *sqlitestore.Database 的方法，不经过接口。

maintenance_references.go（39）：closeRetentionRows（errors.Join(rows.Err, rows.Close)）、loadRetentionReference。审计：命中 closures 表即视为已关闭；ref 的 snapshot 与 consumer 已知 snapshot 不一致时返回非哨兵错误。

maintenance_receipts.go（36）：loadMaintenanceReceipt、saveMaintenanceReceipt。plan_id 为主键；operation 或摘要不符返回错误；save 只接受 *sql.Tx，必须与变更同提交。CloseRetention 和 CollectPlugin 的幂等就靠这张表。

maintenance_turns.go（128）：retentionTurns 按 turn 折叠 started/completed/abandoned/archived，顺序不一致即报错；inspectOfflineTurn 取 turn.completed 的 payload.content 作 candidate；offlineGoalTurns 要求 round 连续、不超过 1000 且与 goal.Round 一致。注释明确两套策略只共享事件解码，不共享规则。

maintenance_dependencies.go（96）：retentionDependencies 返回 blocker 文本列表；guardOfflineDependencies 遇到第一个命中即返回 session.ErrRecoveryDependency；offlineRoot 无行时视为旧版 root（返回空 team），子 session 直接拒绝。审计：状态字面量硬编码在 SQL 里，与 orchestration/agentgoal 的常量需要人工同步。

retention_plan.go（126）：planRetention 是只读投影，包括 session 当前序号、依赖 blocker、turn（含只有 ref 或只有 goal round 的 turn）和 flow；超过 10000 个 turn 拒绝；结果排序后 plan.ID=plan.Digest()。审计：preview 与 apply 共用此函数，参数是 readQuery，拿不到写方法。

retention.go（91）
- 必读：RetentionPlan（在 tx 内计算后回滚，不提交）、CloseRetention。
- 审计：需要 confirmed 与 planID；回执已存在直接返回（会话不符返回 retention.ErrPlanChanged）；同一 tx 内重算 plan，ID 不符返回 ErrPlanChanged，有 blocker 返回 retention.ErrBlocked，无 consumer 报错；写 runtime_consumer_closures、置 refs.released_at、为 turn 追加 turn.archived 事件（以 plan.Sequence 为 expected，期间出现新事件会得到 session.ErrSequenceConflict）、保存回执。不删除任何历史。

plugin_collection_plan.go（191）：planPluginCollection 校验 revision 完整性，active 状态记为 blocker；解码全部 runtime_snapshots manifest，找出引用该 revision 的快照，有未释放 ref 即 blocker；untrackedPluginConsumers 把未关闭且 snapshot 未知或匹配的旧 turn/flow，以及没有持久 turn 的 running goal 都记为 blocker（保守）。审计：全表扫描 runtime_snapshots、session_events、flow_runs，数据量大时成本高。

plugin_collection.go（77）
- 必读：PluginCollectionPlan、CollectPlugin。
- 审计：需要 confirmed 与 planID；按回执幂等；tx 内重算 plan；先写 tombstone，再 DELETE plugin_revisions，blob 只在没有其他 revision 引用时 DELETE，回执同 tx 提交。这是本包唯一删除插件数据的路径，属于硬删除。

recovery_control.go（163）
- 必读：AbandonRecovery。
- 审计：
  - 单 tx，只支持 root session。
  - recovery_abandonments 以 (session, kind, target) 幂等，原因、goal 或 turn 不符返回 orchestration.ErrIdempotencyConflict。
  - guardOfflineDependencies 返回 session.ErrRecoveryDependency；turn 已 abandoned 返回 session.ErrRecoveryAbandoned；已完成的 turn 或已终态的 goal 被拒绝（非哨兵错误）。
  - 释放 ref，追加 turn.abandoned/goal.abandoned 事件（AnySequence）。
  - goal 的 UPDATE 只有 state='running' 守卫，没有 version 守卫，也不检查 RowsAffected。
  - 保留 candidate、report、checkpoint 作为证据，effects_reverted=false。

#### 3. Migration 简表（schema.go）

| v | 建立的表 / 列 |
|---|---|
| 1 | schema_meta、session_events（+type 索引）、agent_checkpoints；写入 session_events='1' |
| 2 | orchestration_events、teams、agents、agent_threads、tasks、task_dependencies、idempotency_records、projection_cursors |
| 3 | agents.depth + idx_agents_team_parent |
| 4 | agent_threads.initial_prompt |
| 5 | mailbox_messages + 部分唯一幂等索引 |
| 6 | idempotency_records.request_hash；mailbox_recipient_sequences（按 MAX(sequence)+1 回填） |
| 7 | tasks.verification_profile |
| 8 | tasks.claim_expires_at |
| 9 | wait_sets |
| 10 | usage_records、team_budgets |
| 11 | workspace_leases（无代码使用） |
| 12 | workspace_leases.request_hash |
| 13 | workspace_views、merge_queue |
| 14 | wait_sets.request_hash |
| 15 | usage_reservations |
| 16 | usage_reservations.request_hash + pending 时间索引 |
| 17 | merge_queue.resolution_task_id |
| 18 | goal_runs、goal_rounds、goal_reports |
| 19 | agents.result |
| 20 | flow_runs、flow_steps、flow_agent_sessions |
| 21 | idx_session_events_correlation |
| 22 | flow_agent_stages |
| 23 | flow_runs.definition_revision、definition_fingerprint |
| 24 | lsp_diagnostic_projections |
| 25 | goal_runs.contract（默认 'null'） |
| 26 | flow_runs.runtime_snapshot_id、agents.profile_snapshot |
| 27 | goal_rounds.feedback；usage_reservations 的 lifecycle_version、model_profile、provider、model、dispatched_at、outcome |
| 28 | plugin_blobs（≤32 MiB）、plugin_revisions（state CHECK，每个 tool 一个 active 的部分唯一索引） |
| 29 | runtime_snapshots、runtime_snapshot_refs（kind turn/flow） |
| 30 | refs.released_at、release_reason；recovery_abandonments |
| 31 | runtime_consumer_closures、maintenance_receipts、plugin_revision_tombstones |
| 32 | plugin_test_reports、plugin_builds（request ≤256 KiB） |
| 33 | strategy_specs、strategy_suites、strategy_reports、strategy_active、strategy_events（AUTOINCREMENT）、strategy_quarantine |
| 34 | agents.workspace_request（默认 'auto'）、workspace_mode |
| 35 | merge_queue 的 kind、candidate_digest、plan_digest、supersedes、superseded_by + 两个索引；merge_apply_journal、merge_apply_progress |
| 36 | mailbox_messages 的 consumer_turn_id、consumer_request、bound_at + idx_mailbox_consumer |

#### 4. 测试文件（★ = 建议与源码对照读）

- ★ migrations_test.go（238）：已发布 DDL 校验和（testdata/migrations.sha256，覆盖 v1–v41）；从每个已发布版本升级后 schema 与新库一致，且历史、checkpoint、账本时间都保留；账本写失败和 DDL 中途失败都整体回滚并可重试；checksum 篡改、版本缺口、未来版本被拒。对照 migrations.go/schema.go。
- ★ transaction_boundaries_test.go（249）：用触发器在审计事件插入时注入失败，验证 root、spawn-with-claim、agent-transition、dependency、completion-unblocks、claim-reaper 六种操作全表无残留，重试成功，重放不重复；维护审计在 tx 视图里只读且能看到未提交状态。对照 query.go 和各写路径。
- store_test.go（123）：Append/Load、ErrSequenceConflict、非法或空 payload 返回 ErrInvalidEvent、checkpoint 生命周期。
- orchestration_test.go（219）：事件 Append/Load/cursor 与序号冲突；EnsureRootThread 幂等；子 Agent 创建与迁移、幂等冲突；迁移幂等；auto workspace 的 hash 兼容；workspace 身份往返与非法 mode。
- ★ tasks_test.go（282）：生命周期、依赖、幂等、环检测、Blocked/NotReady；CreateChild 原子认领；验证要求独立 reviewer；过期 claim 只在 Agent 停止后回收。
- mailbox_test.go（120）：投递、幂等冲突、非法迁移、收件序号唯一。
- ★ mailbox_ledger_test.go（116）：绑定、重领、释放、确认语义，事件计数，空 turn 拒绝。
- waitsets_test.go（93）：挂起与恢复 Agent 原子完成、幂等冲突、consume 幂等。
- usage_test.go（163）：记录幂等与预算；预留防超卖、结算幂等；不同 payload 冲突、未发送预留可回收。
- ★ usage_recovery_test.go（249）：重启恢复不退还未知消耗、回执不丢、请求序号不复用；dispatch 与回执不可重用；损坏数据整体回滚。
- goals_test.go（143）：AdmitGoal 原子、幂等且保留原始时钟；v24→v25 contract 迁移保留旧 goal；goal/round/report 幂等冲突。
- flows_identity_test.go（56）：重开库后 snapshot 身份持久，generation 变化不影响幂等，身份变化返回 ErrIdempotencyConflict。对照 hashFlowRequest。
- workspacecoord_test.go（75）：view/merge 持久、版本化、幂等冲突、非法迁移。
- ★ merge_apply_test.go（172）：supersede 与 apply journal（digest 不同、非 Running、journal 外写入都返回 ErrInvalidEvent）；AwaitingMerge 与 open snapshot merge 计数联动。
- merge_snapshot_test.go（447）：端到端快照合并集成测试（真实工作区与 artifact store）：并行 worker 都合入主线、等待所属任务、任务已变化则拒绝、批准绑定 plan、等待能决策的执行者、冲突由 supersede 解决、journal 崩溃后续提交、无变化 view 丢弃、丢弃时任务失败但保留候选。
- runtime_snapshots_test.go（110）：引用不重绑、失败不留孤儿 manifest、重开后恢复、损坏 manifest 不被静默修复；准入回滚与取消。
- plugins_test.go（162）：目录生命周期跨重开、损坏 fail closed、保存失败不留 blob。
- plugin_builds_test.go（51）：中断不重放、结果不可改写、篡改的请求或结果被拒。
- ★ retention_test.go（296）：归档保留历史且不可复活；preview 重算与回滚（plan 变化返回 ErrPlanChanged）；覆盖终态 goal、子 Agent、flow；插件回收要求消费者已关闭并保留身份（tombstone 阻止复用）；旧消费者保守处理、共享 blob、失败回滚。
- ★ recovery_control_test.go（335）：只释放目标并跨重开保留证据；放弃当前 turn 关闭整个 goal 且不丢 round；旧版和未开始的恢复；所有写入回滚；拒绝有依赖或已委派的工作；目标须存在、未完成且需确认。
- strategies_test.go（188）：不可变、Promote CAS、fallback；报告门禁拒绝缺安全项、篡改与审计失败；suite/candidate 摘要与确认；Active 重新校验发布证据。
- lsp_test.go（36）：投影往返与 UpdatedAt。

#### 5. 本包实现的外部接口

##### 源码中有显式编译期断言（`var _ X = ...`）

| 接口 | 实现类型 | 文件 |
|---|---|---|
| session.EventStore、session.CorrelatedEventStore | SessionEventStore | events.go |
| session.CheckpointStore | CheckpointStore | checkpoints.go |
| orchestration.EventStore | OrchestrationStore | orchestration_events.go |
| orchestration.TaskRepository | OrchestrationStore | tasks.go |
| tokenusage.DurableReservationLedger（嵌入 ReservationLedger、Ledger） | OrchestrationStore | usage_recovery.go（方法分布在 usage.go） |
| agentgoal.Store | OrchestrationStore | goals.go |
| agentflow.Store | OrchestrationStore | flows.go |
| workspacecoord.Repository、workspacecoord.ResolutionLinker | OrchestrationStore | workspacecoord.go |
| workspacecoord.ApplyJournal | OrchestrationStore | merge_apply.go 实现，断言写在 merge_apply_test.go |
| tool.SnapshotStore | RuntimeSnapshots | runtime_snapshots.go |
| plugin.Catalog | PluginCatalog | plugins.go |
| plugin.TestReportStore | *Database | plugin_verification.go |
| pluginbuild.BuildStore | *Database | plugin_builds.go |
| strategy.Catalog、strategy.ShadowStore | Strategies | strategies.go |
| lsp.ProjectionStore | LSPProjectionStore | lsp.go |

##### 无显式断言，按方法集推断（未确认调用方是否经由这些接口使用）

| 接口 | 文件 |
|---|---|
| orchestration.AgentRepository | agents_spawn.go、agents_state.go、agents_query.go、orchestration.go |
| orchestration.SpawnRequestFinder | agents_spawn.go |
| orchestration.RootThreadRepository | orchestration.go |
| orchestration.MailboxRepository、orchestration.MessageRequestFinder | mailbox.go |
| orchestration.MailboxLedger | mailbox_ledger.go |
| orchestration.WaitRepository | waitsets.go |
| orchestration.TaskClaimReaper | task_claims.go |
| tokenusage.Reader | usage.go（UsageReport） |
| agentflow.BudgetReader、agentgoal.BudgetReader | usage.go（Budget） |
| agentgoal.PendingRoundReader | goal_recovery.go |

##### 不经过接口的方法

维护类方法 RetentionPlan、CloseRetention、PluginCollectionPlan、CollectPlugin、AbandonRecovery、Compact 由 internal/app/maintenance.go 和 internal/app/recovery.go 直接在 *sqlitestore.Database 上调用，没有接口。

#### 跨文件审计汇总

**1. version 守卫的 UPDATE 不检查 RowsAffected。** 涉及：TransitionAgent、TransitionTask、claimTaskForAgentTx、changeDependency、unblockDependentTasksTx、supersedeMergeTx、TransitionMerge、startGoalRound、FinishGoal、FinishRun、SetActive 的激活 UPDATE、AbandonRecovery 的 goal UPDATE。
- 单连接加事务内先读后写，目前能保证正确性。
- 一旦改成多连接，这些地方会静默丢失冲突。
- 检查了 RowsAffected 的地方：waitsets、ReapExpiredTaskClaims、MarkDispatched、FinishPluginBuild、Fallback、ReleaseReservation。

**2. 原始 sql.ErrNoRows 漏到包外（没有映射成 ErrNotFound）。** 涉及：
- StageSettlement
- flows：StartStep、FinishStep、FailStep、FinishRun
- EnqueueMerge 查找 view
- goals：StartRound、FinishRound
- PluginBuild、PluginTest
- Strategies：Load、Suite、Report
- SetActive 的 next 不存在时
- planPluginCollection 的 revision 不存在时

**3. 幂等 hash 的旧数据放行规则不一致。** spawn、task、wait、view、merge、reservation 把旧行的空 request_hash 当作匹配。flow_runs 严格比较。mailbox 逐字段比较。goal 用 ID 逐字段比较。

**4. 不在单个事务内或分多步执行的操作。** 包括：
- CancelWaitSetsForAgent（可能只完成一部分）
- BindSessionAgent
- ReleaseReservation（单条带守卫的语句）
- FinishPluginBuild
- SavePluginTest
- Strategies 的 Save、SaveSuite、SaveReport（先插入再回读）
- Promote 的前置读取
- FindSpawn

**5. 幂等缺口。** SavePluginTest 没有 ON CONFLICT，重复保存会收到主键约束错误。strategy_specs 的 UNIQUE(owner_session, revision) 冲突会以原始约束错误返回。

**6. 边界不一致。** Reserve 用 `>` 判断超预算，budgetExceeded 和 CanAdmit 用 `>=`。

**7. TransitionMerge 的 Expected 语义。** Expected==0 表示跳过版本检查，不符时返回 ErrSequenceConflict。其他 Transition 返回各自的 StateConflict。

**8. 大量非哨兵错误。** 例如 "not running"、"stale round"、各类 checksum 失败、插件字节上限、recovery 的若干拒绝路径。调用方无法用 errors.Is 区分这些情况。

**9. 删除语义。** 硬删除只有三处：checkpoint Delete、RemoveDependency、CollectPlugin。CollectPlugin 删除 revision，以及没有其他 revision 共享的 blob，同时写入 tombstone 和回执。retention 和 abandon 只做标记：写 closures 表、置 released_at、追加事件。Compact 只执行 VACUUM 和 WAL checkpoint。

**10. 迁移契约。** 迁移是 forward-only，每个版本一个事务，已应用版本校验 checksum。testdata 只钉住 v1–v34，v35 和 v36 的 DDL 改动不会被测试发现，建议补上 checksum。workspace_leases 是遗留表，没有读写代码。

**11. SQL 参数化。** 动态 SQL 只拼接固定列名和占位符：ListAgents、ListTasks、ListWaitSets、UsageReport、ListMerges，以及 transitionMessage 的列名常量。没有发现把外部输入拼进 SQL 的情况。

**12. 维护路径的扫描成本。** plugin_collection_plan 和 retention_plan 会全表扫描 runtime_snapshots、session_events 和 flow_runs，数据量大时离线维护会变慢（未实测）。

## 第 4 阶段：工具、沙箱、插件与评估

顺序：codingtools → sandbox → lsp → plugin → plugin/wasm → sdk/pluginabi → pluginbuild → evolutiontools → toolbridge → evaluation，然后读本阶段末尾的三条调用链。

### internal/codingtools（非测试 1809 行）
职责：模型可见的 workspace 读/列/glob/grep/诊断/写/编辑/shell 工具，负责路径约束、租约、视图路由、diff 与 LSP 诊断回填。项目内依赖：tool、sandbox、lsp、orchestration、workspacecoord。

推荐阅读顺序：
1. catalog.go（72）：Bindings() 组装 Descriptor；风险等级 read 工具 RiskRead、write/edit RiskWrite、shell RiskExecute；常量 WriteToolName/EditToolName/ShellToolName。审计点：schema 中的上限（mode 0–511、timeout_ms≤300000、max_output_bytes≤2MiB、max_bytes≤4MiB）只是声明。tool.Pipeline 的 validateCall 只做 json.Valid，不按 schema 校验，所以每个上限都必须在执行器里再查一次。
2. limits.go（17）：全部默认值与上限常量（read 256KiB/4MiB、list 200/2000、search 512KiB/4MiB、shell 输出 256KiB/2MiB、shell 默认超时 120s）。
3. helpers.go（57）：decode 使用 DisallowUnknownFields，但不检查尾随数据（evolutiontools 的 decodeEvolutionArgs 会查）；bounded、normalizePattern。
4. paths.go（77）【核心】：normalizeRelative（拒 NUL、绝对路径、..）；resolveExistingAt（Join+EvalSymlinks+ensureInsideRoot）；resolveParentAt（只对最近的已存在祖先做 EvalSymlinks，返回未解析的叶子路径）；ensureInsideRoot（Rel+IsLocal）。审计点：若叶子本身是指向 root 外的 symlink，write() 里的 os.Stat、readDiffFile、verifyExpectedSHA256 会跟随它，root 外内容可能经 diff 回显给模型（未确认，需测试）；atomicWrite 的 Rename 替换的是链接本身。全程是 path 级校验，没有 openat/O_NOFOLLOW，存在校验与使用之间的 TOCTOU 窗口（与并发 shell 组合时）。
5. workspace.go（306）：Workspace 结构与 Attach*；operationRoot（按 ActorScope 的 TeamID/AgentID 取最后一个 ActiveView，Abs+EvalSymlinks 后要求 snapshot/只读评审视图在 snapshotViews 内，其余在 root 内）；ensureWritable（拒 ViewSharedReadonly）；withLease（git worktree、snapshot 视图或未接 lease repo 时跳过；要求 actor scope；Wait 10s；IdempotencyKey="lease:"+callID，无 callID 时为空；释放用 WithoutCancel）；runnerFor（每个 snapshot 根懒建并缓存 sandbox runner，mutex 保护，目录消失则驱逐）。New/NewWithSandboxMode：sandbox 创建失败只存进 shellSandboxErr，不致命。
6. read.go（119）：readLimited（LimitReader limit+1）、selectLines。审计点：fileSHA256 对整个文件哈希，读取量不受 max_bytes 限制。
7. search.go（289）：list（ReadDir 后截断）；glob（WalkDir，跳过 .git 和 symlink，自写的 ** 匹配 matchesGlob 是递归回溯，多个 ** 时可能指数级）；grep（RE2 加 (?i)，usedBytes 预算，跳过含 NUL 的文件和 symlink，每项检查 ctx）。
8. write.go（214）：write 顺序为 ensureWritable→resolveParentAt→withWriteLease→verifyExpectedSHA256→MkdirAll 0755→mode 沿用旧文件或用 args.Mode（代码里没有范围检查，只有 schema 声明）→atomicWrite（同目录 CreateTemp ".gsea-write-*"、Chmod、Sync、Rename）→appendDiff→appendLanguageDiagnostics。edit：resolveExistingAt，os.ReadFile 没有大小上限；多处匹配时必须 replace_all。verifyExpectedBytes 校验 64 位 hex。
9. shell.go（150）：shell→withWorkspaceLease（锁 orchestration.WorkspaceRootLeasePath）→shellUnlocked：operationRoot→runnerFor→working_directory 经 resolveExistingAt+IsDir→输出上限→timeout≤300000ms→context.WithTimeout→snapshotWorkspace（用父 ctx，不计入命令超时）→可选 DescendantLock 令牌作为 ExtraFiles fd 3→runner.Run(sandbox.Request{Command, WorkingDirectory, WorkspaceRoot: operationRoot, InheritedFiles})。父 ctx 取消时仍执行 finalize 捕获变更再返回 ctx.Err()；超时时 timed_out=true、exit_code=-1；limitedBuffer 截断输出。
10. changes.go（252）：snapshotWorkspace（WalkDir，跳过 .git 与 .gsea/artifacts，只记常规文件，上限 50000 文件、8MiB 内容）；captureWorkspaceChanges（前后对比→buildDiff→lsp.ChangeSet）。审计点：对 change.Path 的 Stat/ReadFile 会跟随 symlink；shell 可以写 .git，而快照不跟踪 .git 内的变更（与 evaluation 的宿主 git 调用相关，见第 7 部分）。
11. diff.go（158）：go-udiff；单输入 4MiB、单个内联 96KiB、总内联 1MiB、最多 128 个；大 diff 经 DiffArtifactStore.Put 外置；isBinary（NUL 或非法 UTF-8）。
12. diagnostics.go（89）：languageServerDiagnostics（resolveExistingAt，≤4MiB）；snapshot 视图走 passiveDiagnostics 跳过；ChangeSet 诊断用 WithoutCancel。

测试文件：
- workspace_test.go（609）【对照读 paths.go/write.go/shell.go】：行范围读取、递归 glob、TestWorkspaceRejectsEscapeAndSymlink、唯一匹配 edit、unified diff 与 binary diff、过期指纹拒写、被动 LSP、副作用前取消、agent 视图路由与只读拒绝、descendant liveness 传给命令、无 callID 时重新取 lease、shell 退出码与超时、父 ctx 取消后仍捕获变更、created/modified/deleted 加诊断。
- snapshot_view_test.go（109）【对照读 operationRoot】：TestWorkspaceRoutesSnapshotViewOutsideTheRoot。
- diff_test.go（52）：大 diff 外置、binary 状态、空文件创建/删除。
测试主体未逐行核对；"叶子是外指 symlink 的 write" 是否被覆盖：未确认。

跨包关键调用：app/builtins.go buildBuiltinBindings→Bindings；shell→sandbox.NewWithMode/Runner.Run；withLease→orchestration 的 LeaseRepository.AcquireLease；operationRoot→workspacecoord 视图；diagnostics.go/changes.go→lsp.Manager.DiagnoseChanges。

### internal/sandbox（非测试 490 行）
职责：把一条 /bin/sh 命令限制在 workspace 根内执行，后端为 macOS Seatbelt、Linux bubblewrap，或显式选择的外层容器模式。无项目内依赖。

推荐阅读顺序：
1. sandbox.go（288）：Request（WorkspaceRoot 只能收窄、不能放宽）、Runner、Mode（ModeOS/ModeContainer）。NewWithMode 按 runtime.GOOS 选 sandbox-exec 或 bwrap/bubblewrap，其余返回 ErrUnavailable；容器模式要求 runningInContainer（/.dockerenv、/run/.containerenv、/proc/1/cgroup 标记）。seatbeltRunner.Run：prepareInvocation→`sandbox-exec -p <profile> /bin/sh -c`。bubblewrapArguments：--die-with-parent --new-session --unshare-net/pid/ipc、--tmpfs /、--proc、--dev、--clearenv、ro-bind readPaths、bind workspace 与私有 temp；没有 unshare user/uts/cgroup。containerRunner：不调用 prepareInvocation（没有私有 HOME/TMP），拒绝与自身 root 不同的 WorkspaceRoot。sanitizedEnvironment：输入为空时取 os.Environ()，按子串过滤 KEY/TOKEN/SECRET/PASSWORD/CREDENTIAL/PRIVATE，追加 GSEA_SANDBOX=1。审计点：子串过滤漏掉 DATABASE_URL 这类内嵌凭据的变量；escapeSBPL 只转义 \ 和 "（路径来自规范化后的宿主路径）。
2. invocation.go（162）【核心】：prepareInvocation（规范化 root，校验在配置根内、工作目录在 root 内，MkdirTemp "gsea-shell-"，cleanup 删除）；重建环境，丢弃 HOME/TMP*/GOCACHE/XDG_CACHE_HOME/GSEA_SANDBOX，把它们指向私有临时目录，PATH 项做规范化；shellReadPaths（系统目录、GOROOT、GOMODCACHE、GOPATH/pkg/mod、~/go/pkg/mod、PATH 项；broadReadPath 排除 /、home、/Users、/home、/private、/var、/tmp 等；包含 root 或位于 root 内的路径被排除）；invocationSeatbeltProfile：deny default，全局 allow file-read-metadata（任意路径的元数据可见），allow process-exec/fork、signal self、sysctl-read，读 readPaths+root+tmp，写仅 root+tmp 与 /dev/null，deny network*。
3. process_unix.go（33，//go:build darwin || linux）：runProcessGroup：Setpgid；Cancel 向 -pgid 发 SIGKILL；WaitDelay 1s；Wait 之后再杀整组，清理成功退出后残留的后台进程。审计点：调用 setsid 的子孙会离开进程组，Seatbelt 下没有 pid namespace 兜底，codingtools 用 DescendantLock（fd 3）补救。
4. process_other.go（7，//go:build !darwin && !linux）：runProcessGroup 直接返回 ErrUnavailable，因此其他系统上容器模式也无法运行。

测试文件：
- isolation_test.go（176）【对照读 invocation.go】：收窄与 symlink 逃逸被拒、环境隔离、宽泛 symlink 读路径被拒、真实沙箱下子进程读写受限且有私有 scratch、bwrap 不暴露宿主根、Go 构建使用私有缓存、外层容器拒绝更窄视图。
- sandbox_test.go（90）：秘密环境变量被丢弃、容器 runner 使用工作目录、--unshare-net 存在、Seatbelt 写范围。
- process_unix_test.go（27，darwin||linux）：返回前子孙进程已被杀。

跨包关键调用：codingtools/shell.go（NewWithMode）、pluginbuild/compiler.go、evaluation/verifier.go、evaluation/capabilities.go（均为 sandbox.New，即 OS 模式）→Runner.Run。

### internal/lsp（非测试 1279 行）
职责：通过 stdio 驱动语言服务器（默认 gopls），在文件变更后同步文档，收集、去重并持久化诊断。无项目内依赖。

推荐阅读顺序：
1. types.go（141）：Config.Validate、Position/Diagnostic/Report、ChangeKind/Change/ChangeSet、Projection/ProjectionStore、ChangeDiagnoser/ChangeSetDiagnoser。
2. protocol.go（118）：readProtocolMessage 限制 Content-Length≤8MiB（maxProtocolMessageBytes），但头部行用 ReadString 读取，单行长度不受限；writeProtocolMessage。
3. client.go（461）：startClient 用 exec.CommandContext（background ctx），不经沙箱、不设 Dir、继承完整宿主环境（含模型凭据），stderr 丢弃；启动≤10s，initialize/initialized。request 用 id→pending map；send 持 writeMu 且没有写超时，服务端停止读取时会阻塞调用方；readLoop 处理 publishDiagnostics 和服务端请求（workspace/configuration、workspaceFolders、registerCapability，applyEdit 被拒）；syncDocument（didOpen/didChange、version waiter、100ms settle）；deleteDocument；Close：shutdown 1s→exit→等 2s→forceClose（Kill）。
4. manager.go（559）：每个规范化 root 一个 client，启动失败后 30s 内不重试；DiagnoseChanges：canonicalChangedPath（EvalSymlinks，已删除文件回溯到存在的父目录，校验在 root 内）→clientFor→syncDocument（DiagnosticTimeout ctx）→rankDedupLimit（只保留 root 内 URI，按文件和总数封顶）→persistProjection，失败时 recoverProjection。SupportsPath 只认 .go、go.mod、go.work。
审计点：gopls 以宿主权限运行在模型可写的 workspace 上，会读取模型写的 go.mod/go.work；它调用 go 命令时是否可能触发 toolchain 下载或网络访问：未确认。

测试文件：manager_test.go（262）【对照读 manager.go/protocol.go】：诊断同步与上限、不支持的文件不启动服务器、同一 revision 的持久化恢复、TestProtocolRejectsOversizedFrame、TestLSPHelperProcess（假服务器）、TestGoplsIntegration。

跨包关键调用：codingtools/diagnostics.go、changes.go→Manager.DiagnoseChanges；app/open.go 构造 lsp.NewManager 与 projection store。

### internal/plugin（非测试 807 行）
职责：插件修订的暂存、持久化目录、激活/回滚/退役，以及发布前测试验证与发布门禁。项目内依赖：plugin/wasm、tool。

推荐阅读顺序：
1. catalog.go（108）：常量 CatalogVersion=1、MaxArtifactBytes 32MiB、MaxManifestBytes 64KiB、MaxCatalogBytes 128MiB；错误 ErrCatalogConflict/ErrCatalogInvalid/ErrPersistenceUncertain/ErrToolOwned；RevisionRecord.Encode 校验（PluginID≤128、Revision≤128、Timeout>0、Tool.Validate、64 位 sha hex）；DecodeRecord（严格 JSON+单值）；ValidateSource（大小+sha 一致）；Catalog 接口 Save/Load/SetActive。
2. manager.go（260）【核心】：状态 staged/active/retired；Manifest{PluginID, Tool, Timeout}；Stage：校验→Timeout 0 取 runtime 默认值→复制 source→checkName（编译前后各一次，防止锁外编译期间名字被占）→锁外 runtime.Compile→catalog.Save，持久化失败经 persistenceFailure 让 manager 进入毒化状态。Activate：registry.ActivateManyWithCommit，commit hook 内执行 checkName 与 commitActive（catalog.SetActive CAS，5s 超时），上一修订变 retired。Rollback；Close 关闭已编译模块，但不跟踪进行中的调用（与并发 Invoke 交错时的行为：未确认）。
3. persistence.go（241）【核心】：OpenManager 启动恢复：Load 目录→逐条 ValidateSource→总量≤128MiB→RuntimeIdentity 不一致则拒绝（不做迁移）→状态合法→checkName→重新编译→active 绑定经 ActivateManyWithCommit 发布（名字已被占用返回 ErrToolOwned）。binding()：Executor 直接是 artifact.module（*wasm.Module），ImplementationID="wasm:<sha>:timeout:<n>:<runtimeIdentity>"。ResolveBinding 供快照恢复做精确匹配（entry.Matches）。checkName：保留前缀由 app 传入（agent_、task_、merge_、goal_、flow_、plugin_、strategy_）；不同 PluginID 不能占同一名字；registry 中已有的同名条目必须是本 manager 的同一模块。Retire（RetireManyWithCommit）；Artifact；Inventory（无 GC，产物只增不删）。
4. verification.go（132）：ValidateCases（1–32 个，名字唯一且≤128，input/expected≤16KiB 且为合法 JSON，expect_error 与 expected 互斥）；Manager.Verify：总超时 30s，逐例 Module.Invoke，guest 错误截断到 1024 字符，equalJSON（UseNumber 规范化后比较），计算 CasesSHA256。
5. publication.go（66）：TestReportStore；PublicationService.Verify→manager.Verify→SavePluginTest；Promote：要求 confirmed 与 reportID→加载报告→核对 Passed、Tool、Revision、ContentSHA256、RuntimeIdentity→已 active 直接返回快照（幂等重放）→否则 manager.Activate。

测试文件：
- publication_test.go（142）【对照读 publication.go】：必须有确认与证据、证据不匹配被拒、已激活产物可重放、缺证据/缺产物、未完成的验证不保存、依赖缺一不可。
- persistence_integration_test.go（432）【对照读 persistence.go】：TestDurablePluginLifecycle（重启恢复全生命周期）。
- manager_integration_test.go（121）：TestStageActivateHotSwapAndRollback。
- snapshot_recovery_integration_test.go（116）：TestSnapshotRecoveryRestoresRetiredWASMWithoutGlobalRollback。
- catalog_test.go（52）：TestRevisionRecordRejectsInvalidMetadataAndContent。
集成测试需要用 go1.27 构建 wasm；测试主体未逐行核对。

跨包关键调用：pluginbuild/service.go→Manager.Stage（Stager 接口）与 PublicationService.Verify（Verifier 接口）；app/plugin_verification.go PromotePlugin→PublicationService.Promote；app/open.go 调 OpenManager，并 registry.ConfigurePersistence(RuntimeSnapshots, pluginManager.ResolveBinding)、NewPublicationService(…, wasmRuntime.ResourceIdentity())；Catalog 实现在 internal/session/sqlitestore/plugins.go。

### internal/plugin/wasm（非测试 492 行，testdata 23 行）
职责：基于 wazero 编译并校验插件模块，每次调用新建实例，通过 blob handle ABI 交换 JSON。无项目内依赖。

推荐阅读顺序：
1. blobstore.go（161）【先读】：按调用分 map；handle 从 1 递增；owner 区分 host/guest；putLocked 同时检查单 blob 上限（默认 16MiB）和单调用总量（默认 32MiB，请求本身也计入）；dropGuest 只能删 guest 所有的 blob；end 删除整个调用的 blob。
2. runtime.go（331）【核心】：host 模块 "local_agent_host_v1"，白名单导入 host_blob_len/read/put/drop；默认 1024 页（64MiB）、超时 30s。NewRuntime：compiler 引擎、WithMemoryLimitPages、WithCloseOnContextDone(true)、实例化 WASI；runtimeConstructionMutex 规避 wazero 构造竞态；resourceIdentity 把内存/blob/call/timeout 上限编码进身份串。Compile→validateModule：导入只允许 WASI 与 host 白名单，禁止导入 memory，校验导出签名 plugin_abi_version ()→i32、plugin_invoke (i64)→i64。Module.Invoke：请求必须是合法 JSON→超时取修订值或默认→callKey "local-agent-call-N"→blobs.begin（请求为 host 所有）→InstantiateModule（WithName(callKey)，WithStartFunctions() 不运行 _start）→若导出 _initialize 则调用→ABI 版本检查→plugin_invoke(requestHandle)→响应 handle≠0 且必须为 guest 所有（getGuest）→解析 {ok,result,error}。host 函数用 module.Name() 定位调用；hostBlobLen 出错或>4GiB 返回 -1；hostBlobRead 检查 capacity 并用带边界检查的 Memory().Write；hostBlobPut 用 Memory().Read 后复制；hostBlobDrop。
审计点：64MiB 只是单实例上限，并发调用没有总量或并发数限制；Invoke 返回的 guest 错误字符串不截断（只受 blob 上限约束，Verify 才截到 1024）；WASI 无 preopen 目录，时钟与随机源用 wazero 默认配置（具体行为未确认）；超时依赖 WithCloseOnContextDone 中断死循环。
3. testdata/echo/main.go（23）：由集成测试以 GOOS=wasip1 -buildmode=c-shared 构建；用 pluginabi.Register 注册回显 handler，{"loop":true} 进入死循环，用于超时测试。

测试文件：
- blobstore_test.go（44）【对照读 blobstore.go】：handle 按调用隔离；host 所有的 blob 不能被 drop 也不能当响应；单 blob 与单调用上限。
- runtime_integration_test.go（84）：TestGo127PluginRoundTrip，构建 echo 后往返调用，并验证 25ms 超时下死循环在 2s 内返回（-short 跳过，需要 go1.27）。

跨包关键调用：plugin/manager.go、persistence.go→Runtime.Compile、Module.Invoke、Close；plugin/verification.go→Module.Invoke；app/open.go→wasm.NewRuntime、ResourceIdentity()。

### sdk/pluginabi（124 行）
职责：guest 侧 SDK，候选插件通过 Register 注册 Invoke；同一份源码被 embed 进宿主二进制，由 pluginbuild 写入构建目录。无项目内依赖。

推荐阅读顺序：
1. abi.go（28，无 build tag，两侧共用）：Version=1；Handler 类型；Register 遇到 nil 或重复注册时 panic。
2. bridge_wasip1.go（85，//go:build wasip1）：//go:wasmimport local_agent_host_v1 的 host_blob_len、host_blob_read、host_blob_put（没有导入 host_blob_drop，guest 从不释放 blob，依赖调用结束时 blobstore.end 清理）；//go:wasmexport plugin_abi_version、plugin_invoke；getBlob 先取长度（<0 或>4GiB 报错），再 make 缓冲区调用 hostBlobRead；putBlob；handler 结果必须非空且 json.Valid；errorBlob 编码失败信封。
3. source.go（11，//go:build !wasip1）：//go:embed abi.go bridge_wasip1.go 导出 SourceFiles。审计点：pluginbuild 写入构建目录的 SDK 与宿主构建时的源码逐字节一致，哈希记入 BuildEvidence.SDKSHA256，构建不从网络获取 SDK。

测试文件：无。

跨包关键调用：pluginbuild/compiler.go 读取 SourceFiles；plugin/wasm/testdata/echo 与生成的 main.go 调用 Register。

### internal/pluginbuild（非测试 412 行）
职责：把模型提交的单文件 Go 源码在 OS 沙箱内编译为 wasip1 c-shared WASM，随后暂存、跑测试，并记录不可变的构建证据。项目内依赖：plugin、sandbox、tool、sdk/pluginabi。

推荐阅读顺序：
1. types.go（80）：MaxRequestBytes 256KiB；ErrEvidence（evidenceError，ToolExecutionFatal() 为 true，让 Pipeline 把证据失败当致命错误）；Request{OwnerSession, Manifest, Source, Cases}.Validate：Tool.Validate、工具名必须以 extension_ 开头、PluginID/Revision≤128、RiskClass 必须是 RiskPure、Capabilities 必须为空、Timeout 0–5s、ValidateSource、ValidateCases、编码后≤256KiB；BuildEvidence；Result{BuildID, Build, State, Error, TestReportID, TestReport}。
2. source.go（59）【核心，不可信输入边界】：ValidateSource：1B–64KiB；必须 package main；任何包含 "go:" 的注释都拒绝（挡住 go:linkname、go:embed、go:wasmimport 等指令）；import 白名单为 encoding/json、strings、bytes、strconv、math、sort、unicode、unicode/utf8、errors、fmt、regexp、slices、cmp，且不允许别名（unsafe、C、os、syscall 因此被排除）；禁止名为 init、main、pluginInvoke、pluginABIVersion 的函数；必须有顶层 func Invoke。审计点：Invoke 的签名不在这里检查，由生成的 main.go 中 Register(Invoke) 在编译期强制；包级 var 初始化表达式仍会在 init 阶段执行（只能用白名单包）；方法名、类型名不受限制（是否可借此与 SDK 符号冲突：未确认，同包冲突会导致编译失败而非逃逸）。
3. compiler.go（147）【核心】：Compile：ValidateSource→2 分钟总超时→goPath=runtime.GOROOT()/bin/go→固定环境（GOTOOLCHAIN=local、GOENV=off、GOWORK=off、GOPROXY=off、GOSUMDB=off、CGO_ENABLED=0、GOOS=wasip1、GOARCH=wasm、GOMAXPROCS=2、GOMEMLIMIT=256MiB，不继承宿主环境）→在沙箱外执行 `go version`，要求前缀 "go version go1.27.0 "→对 go 二进制做 sha256→MkdirTemp "gsea-candidate-"，写入 embed 的 SDK 并计算 SDKSHA256，写入 go.mod、candidate.go、main.go（init 中 Register(Invoke)）→sandbox.New(directory).Run，命令为 `ulimit -t 90 && ulimit -f 131072 && exec go build -mod=readonly -trimpath -buildvcs=false -buildmode=c-shared -o candidate.wasm .`→读取产物≤32MiB；boundedBuildLog 上限 64KiB。
审计点：runtime.GOROOT() 会受宿主 GOROOT 环境变量影响，`go version` 不在沙箱内运行（属于宿主自身配置，风险低）；只哈希 bin/go，没有哈希 compile/link 工具与标准库源码；ulimit -f 的块大小取决于 /bin/sh 实现（512 或 1024 字节，未确认）；没有内存硬上限（GOMEMLIMIT 只是 GC 软目标）；OS 沙箱把 HOME/GOCACHE 指向私有临时目录，因此每次都是冷构建。
4. service.go（126）：接口 BuildStore（Begin/Finish/PluginBuild）、SourceCompiler、Stager、Verifier；Service 单 worker 闸门（容量 1 的 chan，等待可被 ctx 取消且不预留）；Build：Timeout 0→5s→Validate→取闸门→BeginPluginBuild（已完成的构建原样重放）→defer FinishPluginBuild（WithoutCancel 5s，失败时 errors.Join 上 ErrEvidence）→Compile→Stage→Verify；测试未通过时 state=quarantined，否则 awaiting_approval；错误文本截断到 4096；Inspect。

测试文件：
- compiler_test.go（54）【对照读 source.go】：TestCandidateSourcePolicy（源码策略正反例）、TestIsolatedCompilerProducesInvokableWASM（需要真实沙箱与 go1.27）。
- service_test.go（267）【对照读 service.go】：完整工作流、回执重放不再派发、非法请求在预留前拒绝、取消后证据仍保存、证据写入失败为致命错误、被取消的等待者不预留、依赖缺一不可。

跨包关键调用：evolutiontools/plugin.go→app/plugin_build.go BuildPlugin→Service.Build；Stager=plugin.Manager.Stage；Verifier=plugin.PublicationService.Verify；BuildStore 实现在 internal/session/sqlitestore/plugin_builds.go；app/open.go 用 pluginbuild.NewService(Store: database, Compiler{}, Stager: pluginManager, Verifier: publication) 组装。

### internal/evolutiontools（非测试 374 行）
职责：把插件与策略的自进化服务适配为模型工具，并把操作范围绑定到可信的调用会话。项目内依赖：pluginbuild、strategy、tool。

推荐阅读顺序：
1. ports.go（46）：PluginHost（BuildPlugin、PluginBuild、PromotePlugin）、StrategyStore、StrategyCaller{OwnerSession, BaselineConfigID, Compare}、ResolveStrategyCaller。
2. arguments.go（25）：decodeEvolutionArgs：≤256KiB、DisallowUnknownFields、要求单个 JSON 值。
3. plugin.go（70）【核心】：propose：从 tool.InvocationFromContext 取 SessionID（缺失即失败），用可信会话覆盖请求里的 OwnerSession，再调 BuildPlugin；只有没有 BuildID 或错误是 ErrEvidence 时才返回 error，其余失败作为结果 JSON 返回给模型。inspect/publish 走 scopedBuild（构建的 owner 必须等于当前会话）。publish 要求 State=awaiting_approval 且有 TestReportID，调用 PromotePlugin(..., confirmed=true)，返回 available_next_turn:true。审计点：confirmed=true 在这里是硬编码，真正的人工确认来自 Pipeline 对 plugin_publish 的 RequireApproval 规则（app/builtins.go）。
4. plugin_bindings.go（46）：plugin_propose（RiskExecute）、plugin_inspect（RiskRead）、plugin_publish（RiskWrite），revision 均为 builtin-v1。
5. strategy.go（69）：propose、inspect、shadow（caller.Compare）、publish（报告的 baseline 必须等于调用方 baseline，再 store.Promote）。
6. strategy_bindings.go（56）：strategy_propose/inspect/shadow/publish 的描述符。
7. strategy_scope.go（62）：scopedStrategy 经 report 解析候选，检查互斥选择规则与 owner session。

测试文件：
- bindings_test.go（250）【对照读 plugin.go】：协议与重构前一致、参数严格解析、propose 使用 invocation 会话并保持错误分类、范围检查与发布门禁、参数和审批都不能扩大范围。
- strategy_test.go（170）：适配器保持输出与可信 owner、范围/选择失败发生在副作用之前、schema/审批/调用方失败不会触达服务、致命证据错误被保留。

跨包关键调用：app/plugin_tools.go→PluginBindings(application)；app 实现 PluginHost（plugin_build.go 的 BuildPlugin 经 evolution_lifecycle.go beginEvolution 绑定应用关闭；plugin_verification.go 的 PromotePlugin→publication.Promote）。

### internal/toolbridge（非测试 169 行）
职责：仅在评测模式使用，把评测 harness 通过 Unix socket 提供的工具注册为普通 tool.Binding，调用仍经过 tool.Pipeline。项目内依赖：orchestration、tool。

bridge.go（169）：
- Load：manifest 与 socket 路径必填，socket 必须是绝对路径；os.ReadFile 之后才检查 4MiB 上限（先整读再判断大小）；严格 JSON（DisallowUnknownFields+单值）；schema 必须是 gsea.tool-bridge.manifest.v1；至少一个工具；revision="bridge-v1-"+manifest sha256 前 12 位 hex；每个工具 RiskExecute、Capabilities ["tool.invoke"]，并调用 Descriptor.Validate（名字正则、schema 根必须是 object）。manifest 内重名或与内置工具同名不在这里检查，由 tool/registry.go 激活时的 "duplicate tool binding" 检查拒绝（app/open.go 把桥接绑定与内置绑定放在同一批 ActivateMany 中）。
- executor.Invoke：每次调用新建连接（拨号 5s），连接 deadline 取 ctx deadline，没有则 5 分钟；发送一行 JSON {schema, call_id, tool, arguments}；读取一行≤32MiB 的响应，用非严格的 json.Unmarshal；校验 schema 与 call_id 一致；harness 返回 error 时包装成成功结果 {"error","result"}，不走 Pipeline 的失败路径。call_id 来自 orchestration.ToolCallIDFromContext，缺失时为空串，空串响应也能匹配。

测试文件：bridge_test.go（75）：TestLoadAndInvokeBridge（真实 Unix socket 往返）、TestLoadRejectsInvalidBoundary（相对 socket 路径、错误 schema）。

跨包关键调用：cli/chat_options.go 要求桥接参数成对出现且必须 --evaluation-mode；cli/chat.go:243 toolbridge.Load→app.OpenWithOptions(ToolBindings)；app/builtins.go buildPolicy 为每个桥接工具追加 RequireApproval 规则 "approve-invocation-tool-<name>"；cli/chat.go selectChatApprover 在有桥接工具时只自动批准桥接工具，不再批准 workspace_write/edit/shell_exec（注释说明是为了防止 prompt 用 shell 读取含模型凭据的进程环境）。

### internal/evaluation（非测试 2184 行）
职责：定义基准任务/结果协议，提供单任务 runner（准备工作区→运行 agent→抽取 patch→跑隐藏检查），以及官方结果导入与计分。项目内依赖：sandbox。

推荐阅读顺序：
1. types.go（237）：Task/Check/Usage/AgentRequest/AgentOutcome/CheckResult/Result。Task.Validate：id 正则、git_worktree 必须有 base_commit 且不得以 "-" 开头（防 git 参数注入）、directory_copy 不得设 base_commit、agent 超时 0–7200s、至少一个非 optional check、check 名唯一；Check.validate：backend 只能 sandbox/docker，docker 必须有 image，超时 0–7200s，phase 枚举。AgentRequest 刻意不包含 checks 与仓库信息。
2. manifest.go（106）：LoadTasks（重复 id 拒绝、空清单拒绝）、LoadResults、decodeJSONL（单行≤64MiB，严格解析并拒绝尾随值）、WriteResult。
3. runner.go（202）【核心】：NewRunner（root 不得是文件系统根，MkdirAll 0700）；RunTask：Validate→runDirectory=root/runs/<safe runID>/<safe taskID>→validateTaskArtifactPath（产物目录不得位于 fixture 内）→PrepareWorkspace→agent（默认 30 分钟，task 可覆盖）→Worktree.Patch（WithoutCancel 30s）→写 model.patch→verifier（为 nil 时用 RunChecks）→Resolved 要求 agent completed、无 agent/patch/check 错误且 ChecksResolved；defer 用 WithoutCancel 30s 清理 worktree（KeepWorktrees 时保留）。
4. workspace.go（355）【核心】：prepareGitWorktree（Abs+EvalSymlinks，workspaces 根不得位于源仓库内，rev-parse --verify <base>^{commit}，git worktree add --detach）；prepareDirectoryCopy（复制 fixture，跳过 .git，symlink 原样复制，然后 git init/add --force --all/commit 作为基线）；canonicalProspectivePath；Patch（git add --intent-to-add，再 git diff --binary --no-ext-diff 排除 .gsea/artifacts、.gsea/worktrees，≤32MiB）；Remove（directory_copy 只删托管根内的路径，git_worktree 用 git worktree remove --force）；safePathComponent（字符清洗+sha256 前 8 位 hex）；runGitLimited 与 cappedBuffer。
审计点：(1) runGitLimited 把 stdout 和 stderr 写进同一个缓冲区，git 的 warning 会混进 patch 文本。(2) Patch 在宿主上直接运行 git，不经沙箱，工作目录是 agent 已经可写的 worktree，并继承宿主完整环境。agent 的 shell 能改写 .git/config（directory_copy 模式），或把 git_worktree 的 .git 文件替换掉，再借 core.fsmonitor 一类配置让宿主 git 执行命令；--no-ext-diff 不覆盖这类配置。此路径未实测，标为未确认，建议重点审计。
5. verifier.go（140）：RunChecks 顺序执行，ctx 取消即返回；runCheck：默认超时 10 分钟，输出≤2MiB，写 check-NNN-<name>.log；sandbox 后端 sandbox.New(workspace).Run（未传 Environment，所以用过滤后的 os.Environ）；docker 后端 dockerCheckArguments：镜像名正则、--pull never、--network none、--cap-drop ALL、no-new-privileges、--pids-limit 512、工作区读写挂载到 /workspace、--entrypoint /bin/sh -lc。审计点：docker 没有内存/CPU 限制，未指定 --user；ChecksResolved 只忽略 optional 失败。
6. capabilities.go（132）：InspectCapabilities 探测命令（LookPath）、sandbox（临时目录跑 true）、docker daemon、python 模块，每项 5s 超时。
7. export.go（139）：WriteSWEPredictions（instance/model 覆盖规则）；Summarize（尝试数、观测 pass@k、用量与按 suite/language 汇总）。
8. score.go（270）：ProtocolForBenchmark（Terminal-Bench 2.1：89 任务×至少 5 次，dataset digest 固定；SWE-bench Verified：500×1）；ScoreTerminalBench（accuracy、每任务标准误、无偏 pass@k，缺 reward 或 errored 计为失败）；覆盖率违规。单独调用 ScoreTerminalBench 时不查重复 TrialID（LoadHarborJobs 会查）。
9. official_results.go（603）：LoadHarborJob（WalkDir 收集 result.json，单文件≤32MiB；inspectHarborProtocol 校验 config 的固定 dataset、lock 的 digest 一致性、禁止 timeout/资源覆盖和关闭 verifier、成功 trial 必须有 ATIF trajectory）；LoadHarborJobs（跨 job 重复 trial ID 记为违规）；ApplyHarborProtocolMetadata；ApplyTerminalBenchAudit（未知 trial ID 报错）；LoadSWEBenchRunReport/ScoreSWEBenchVerified（没有检查 resolved≤total）。

测试文件：
- runner_test.go（232）【对照读 runner.go/workspace.go】：生成 patch、运行隐藏检查并删除 worktree；patch 包含已跟踪变更；评测根位于源仓库内被拒；directory_copy 捕获变更且不修改 fixture；agent 失败时即使检查通过也不算 resolved。
- manifest_test.go（125）：JSONL 与重复 id、必须有非 optional check、workspace 模式校验、一行多值被拒、SWE 预测的 instance/model 覆盖、ChecksResolved 只忽略 optional、docker 参数离线且无特权、Summarize 汇总。
- score_test.go（313）【对照读 official_results.go】：TB 公式与覆盖率、Harbor job 加载、协议证据（固定 config/lock、拒绝 timeout 覆盖、digest 不匹配）、可续跑批次、重复 trial ID、审计取消资格、其他 run 的 trial ID、SWE-bench Verified 需要完整提交。

跨包关键调用：cli/eval.go 调 InspectCapabilities、LoadTasks、NewRunner、Summarize、WriteSWEPredictions、LoadHarborJobs、ScoreTerminalBench；verifier.go/capabilities.go→sandbox.New→Run。

### 工具与插件端到端调用链

#### (a) 模型调用内置工具（shell_exec / workspace_write）
1. internal/agentruntime/factory_turn.go：新 turn 取 `f.registry.Snapshot()`（:33），恢复的 turn 用 `ResolveSnapshot`（:96）。
2. internal/agentruntime/eino_turn.go：buildTurn→einoadapter.NewToolsWithActor(pipeline, snapshot, …)，再按 profile 过滤。
3. internal/einoadapter/tool.go：Eino 发出 tool call→InvokableRun→WithActorScope→pipeline.Execute(Call{CallID: compose.GetToolCallID(ctx), Registry: 本 turn 快照, …})。
4. internal/tool/pipeline.go：Execute→validate→validateCall（只做 json.Valid）→resolve（快照必须属于本 registry）→guards.CheckCall→每个 call 的锁→replaySuccess→recordProposal（EventToolProposed）→checkCallPreconditions→evaluate（policy.Evaluate + EventPolicyDecided；app/builtins.go 中 approve-shell-exec、approve-workspace-write 为 RequireApproval）→authorize（EventApprovalRequested→approver.Approve→EventApprovalResolved；交互模式为 approval.Broker，无头模式 DenyApprover，评测模式 ToolAllowlistApprover，见 cli/chat.go selectChatApprover）→admitStart（guards.BeforeStart + EventToolStarted）→invoke（ctx 注入 ActorScope、ToolCallID、TurnID、Invocation；binding.Executor.Invoke）→recordSuccess（WithoutCancel 5s，写入失败为 OutcomeUnknownError）或 recordFailure 后包装 ExecutionError。
5a. shell：internal/codingtools/shell.go shell→workspace.go withWorkspaceLease/withLease（orchestration AcquireLease）→shell.go shellUnlocked→workspace.go operationRoot→runnerFor→paths.go resolveExistingAt→changes.go snapshotWorkspace→internal/sandbox/sandbox.go Runner.Run（darwin：seatbeltRunner.Run；linux：bwrap runner + bubblewrapArguments）→invocation.go prepareInvocation、invocationSeatbeltProfile→process_unix.go runProcessGroup→回到 shell.go finalize：changes.go captureWorkspaceChanges→diff.go→diagnostics.go appendChangeSetDiagnostics→internal/lsp/manager.go DiagnoseChanges→client.go syncDocument。
5b. write：internal/codingtools/write.go write→workspace.go ensureWritable→paths.go resolveParentAt→withWriteLease→verifyExpectedSHA256→atomicWrite→appendDiff（diff.go）→appendLanguageDiagnostics（diagnostics.go→lsp.Manager）。
6. 结果返回 einoadapter：ErrApprovalDenied→denied JSON，PreconditionError→JSON，ExecutionError→可恢复 JSON，其余作为 error→Eino 把结果交回模型。

#### (b) 自进化：候选→编译→测试→发布→激活→下一 turn 调用
1. turn N 中模型调 plugin_propose：einoadapter InvokableRun→tool/pipeline.go Execute（approve-plugin-propose，需审批）→internal/evolutiontools/plugin.go propose（decodeEvolutionArgs；InvocationFromContext 覆盖 OwnerSession）→internal/app/plugin_build.go BuildPlugin（evolution_lifecycle.go beginEvolution）→internal/pluginbuild/service.go Build（Validate→闸门→sqlitestore/plugin_builds.go BeginPluginBuild）。
2. 编译：pluginbuild/compiler.go Compile→source.go ValidateSource→sdk/pluginabi/source.go SourceFiles→sandbox.New(directory).Run（go build wasip1 c-shared）。
3. 暂存：internal/plugin/manager.go Stage→persistence.go checkName→internal/plugin/wasm/runtime.go Compile→validateModule→Catalog.Save（sqlitestore/plugins.go）。
4. 测试：plugin/publication.go PublicationService.Verify→verification.go Manager.Verify→wasm Module.Invoke（逐 case）→SavePluginTest→回到 service.go 设 state（quarantined/awaiting_approval）→defer FinishPluginBuild。可选 plugin_inspect→plugin.go scopedBuild→PluginBuild。
5. 发布确认：模型调 plugin_publish→Pipeline（approve-plugin-publish，人工审批即"确认"）→evolutiontools/plugin.go publish（scopedBuild、state 与 TestReportID 检查）→app/plugin_verification.go PromotePlugin→plugin/publication.go Promote（核对报告）→plugin/manager.go Activate→tool registry ActivateManyWithCommit（commit hook：checkName + commitActive→Catalog.SetActive CAS）→registry 产生新 generation；工具返回 available_next_turn:true。
6. 为什么是下一 turn：turn N 的工具集在第 1 步之前已由 factory_turn.go 的 Snapshot() 冻结，新 generation 对它不可见。turn N+1：factory_turn.go Snapshot() 取到含 extension_* 的新快照→eino_turn.go buildTurn→einoadapter.NewToolsWithActor。
7. 调用：einoadapter InvokableRun→Pipeline（approve-extension-invoke，需审批）→binding.Executor（plugin/persistence.go binding() 中的 artifact.module）→plugin/wasm/runtime.go Module.Invoke→blobstore.go begin→InstantiateModule→_initialize（guest 包 init：生成的 main.go init→pluginabi.Register(Invoke)）→plugin_abi_version→plugin_invoke→guest sdk/pluginabi/bridge_wasip1.go pluginInvoke→getBlob（host_blob_len/host_blob_read→runtime.go hostBlobLen/hostBlobRead）→registered（候选源码的 Invoke）→putBlob（host_blob_put→hostBlobPut→blobstore putLocked）→宿主 getGuest 解析信封→blobs.end。
8. 进程重启：plugin/persistence.go OpenManager 重新编译并发布 active 修订；恢复旧 turn 时 registry.ResolveSnapshot 经 ResolveBinding 取回精确的（可能已退役的）模块。

#### (c) evaluation 跑一个 benchmark task
1. internal/cli/eval.go 运行子命令：evaluation.LoadTasks（manifest.go decodeJSONL→types.go Task.Validate）→NewRunner(RunnerConfig{Agent: evalAgentFunc(deps, chatArguments, runID), Verifier: deps.evalVerifier})→executeEvaluationTasks(ctx, tasks, parallel, runner.RunTask, …)，每个结果用 WriteResult 追加到 JSONL。
2. internal/evaluation/runner.go RunTask→Task.Validate→validateTaskArtifactPath→workspace.go PrepareWorkspace→prepareGitWorktree 或 prepareDirectoryCopy→runGit。
3. agent：cli/eval.go evalAgentFunc→runChatWithPrompt，参数为 --evaluation-mode --workspace <worktree> --data-dir <run>/data --session <id>。它在进程内运行，parallel>1 时多个任务在同一进程内并发→cli/chat.go（有桥接参数时调 toolbridge.Load）→app.OpenWithOptions→selectChatApprover（ToolAllowlistApprover）→完整 agent 循环（与链 (a) 相同）→readEvaluationUsage（app.ReadSessionUsage）。
4. runner.go：Worktree.Patch（workspace.go→runGitLimited）→写 model.patch。
5. verifier.go RunChecks→runCheck→sandbox.New(workspace).Run，或 runDockerCheck→ChecksResolved。
6. runner.go 计算 Resolved→defer Worktree.Remove→cli/eval.go WriteResult→全部任务结束后 export.go Summarize 输出汇总。
7. 离线计分：cli/eval.go 调 LoadHarborJobs→ScoreTerminalBench，并调 WriteSWEPredictions。CLI 是否调用 ApplyHarborProtocolMetadata、ApplyTerminalBenchAudit、LoadSWEBenchRunReport/ScoreSWEBenchVerified：未确认，没有核对调用点。生产环境 deps.evalVerifier 是否为 nil（为 nil 时回落到 RunChecks）：未确认。

### 工具/插件/评估审计发现（大多未实测）
1. tool.Pipeline 不按 JSON schema 校验参数，schema 里的上限只靠执行器自己检查。codingtools write 的 mode 只在 schema 里限定为 0–511，代码里没有检查。
2. codingtools/paths.go resolveParentAt 不解析叶子。如果叶子是指向 root 外的 symlink，write.go 的 Stat、readDiffFile 和 verifyExpectedSHA256 会读到 root 外的内容，并可能经 diff 回显给模型（未确认）。
3. evaluation/workspace.go Patch 在宿主上直接对 agent 可写的 worktree 运行 git，不经沙箱，且继承完整环境。改写 .git/config 或 .git 文件后，可能借 core.fsmonitor 等配置在宿主执行命令（未确认）。runGitLimited 把 stderr 混进 patch。
4. lsp/client.go 启动 gopls 时没有沙箱，并继承包括凭据在内的完整宿主环境；send 没有写超时。
5. sandbox：环境变量按子串过滤（DATABASE_URL 这类内嵌凭据的变量会漏掉）；Seatbelt profile 全局允许 file-read-metadata；bwrap 没有 unshare user/uts/cgroup；Seatbelt 下 setsid 的子孙能离开进程组，由 DescendantLock 补救。
6. wasm：64MiB 只是单实例上限，并发调用没有总量限制；Module.Invoke 返回的 guest 错误不截断；guest SDK 从不调用 host_blob_drop。
7. 无界读取：codingtools read 的 fileSHA256 和 edit 的 ReadFile 不受大小限制；toolbridge Load 先整读 manifest 再判断是否超过 4MiB；lsp 头部行用 ReadString 读取，长度不受限。
8. pluginbuild/compiler.go：只哈希 bin/go；`go version` 在沙箱外运行，且 go 路径受 GOROOT 环境变量影响；ulimit -f 的块大小未确认；没有内存硬上限。

#### 工具链关键文件
- `internal/tool/pipeline.go`
- `internal/einoadapter/tool.go`
- `internal/agentruntime/factory_turn.go`
- `internal/app/open.go`
- `internal/app/builtins.go`
- `internal/cli/chat.go`
- `internal/cli/eval.go`
- `internal/codingtools/paths.go`
- `internal/sandbox/invocation.go`
- `internal/plugin/wasm/runtime.go`
- `internal/pluginbuild/source.go`
- `internal/evaluation/workspace.go`

## 第 5 阶段：多 Agent

先读 MULTI_AGENT_ARCHITECTURE.md。顺序：agentmanager（Supervisor 主体、工具）→ agentflow → workspacecoord → workspacedomain，同时对照 sqlitestore 的 C–G、J、K；最后读多 Agent 调用链。

### internal/agentmanager（第 1 段：Supervisor 主体）

职责：多 Agent 运行时的 Supervisor。负责 spawn/continue/interrupt、调度、mailbox 投递、WaitSet 挂起与唤醒、工作区视图分配、Task 指派与验收、快照合并排队、重启恢复，并把这些能力暴露为模型工具。项目内依赖：agentprofile、orchestration、scheduler、tokenusage、tool、workspacecoord。

推荐阅读顺序：

1. `supervisor.go`（693）：先读这个文件的全部类型与入口。
   - 必读：`Runner`、`SuspendingRunner`、`ContextSeeder`、`RunRequest`、`Config`、`NewSupervisor`、`Spawn`、`Continue`、`Interrupt`、`Close`、`cancelDurableAgents`、`signal`。
   - 审计：锁顺序为 admission → mu。`FindSpawnRequest` 幂等检查在 seeder 之后执行，所以重放 spawn 会重新跑一次 seeder（可能产生模型费用）。`Continue` 只允许 Completed/Failed → Queued。`Close` 等 5 秒，超时返回 `ErrSupervisorCloseTimeout` 并保留租约。
2. `profile.go`（49）：子 Agent profile 解码与委派收窄（调用 agentprofile.ConstrainDelegation）。
3. `supervisor_scheduler.go`（153）：`start`、`startAvailable`、`removeRunner`、`reportActivity`。配合 scheduler 包的 BucketSet 与 Order 读。
4. `supervisor_run.go`（272）：`run`、`executeRun`、`finish`、`afterAgentTransition`。
   - 审计：recovering 用 Activity "recovered" 标记；外部故障暂停使用 `recovery:<kind>`，`agent_resume` 保留原 RunID 与 Task claim。恢复或 resume 时跳过 `loadMessages` 和 ack。`result.Delivery` 会被清空，只有 Supervisor 能设置。`finishAssignedTask` 在终态迁移之前运行；终态结果先写入 `agent_completions`，SQLite `TransitionAgent` 事务内写入父级 `MessageResult`，重启时补做未结算 completion。
5. `supervisor_mailbox.go`（68）：至少一次投递，`MarkDelivered`/`Acknowledged`。
6. `agent_mailbox.go`（97）：`Claim`、`Ack`、`Release`，列表上限 1000。root 的 mailbox 经 agentruntime 的 BackgroundMailbox 使用。
7. `supervisor_wait.go`（211）：`monitorWaitSets`、`Suspend`、`ResolveWaitSet`、`ReconcileWaitSets`，以及进程内 Wait。
8. `supervisor_workspace.go`（329）：视图复用规则；key "view:"+agent+":"+run；reviewer 得到 SharedReadonly 视图，RootPath 指向 worker 快照工作目录；冲突解决视图经 `PrepareResolution` 创建。
9. `supervisor_assignment.go`（234）：`finishAssignedTask` 状态机。Partial/Blocked → Blocked；`deliverSnapshot` → Verification / AwaitingMerge / Completed；reviewer 作为兄弟 Agent spawn，key 为 task-review:ID:Attempt。
10. `supervisor_delivery.go`（184）：`deliverSnapshot`、`queuedCandidate`、`enqueueCandidate`（key merge:view:run:digest）、`supersededMerge`、`scheduleMerges`（经 workers.Go 异步）。
11. `supervisor_tasks.go`（278）：`authorize*`、`TransitionTask`（reviewer 不能是 assignee）、`dispatchReadyTasks`（key task-dispatch:ID:Attempt）。
12. `supervisor_recovery.go`：`Recover` 顺序：
    1. `RecoverReservations`
    2. `ReapAbandonedLeases`
    3. 先处理 `agent_completions` 冻结结果；逐个 team 修正状态：Running → Queued（"recovered"）；`recovery:<kind>` Waiting 保持暂停；其他 Waiting 但没有 WaitSet → Queued；Canceling → Canceled
    4. dispatch、Reconcile、scheduleMerges
    5. `startAvailable`
    - root Agent 不参与，root 由 conversation 层恢复。
13. `supervisor_merge.go`（84）、`supervisor_query.go`（89）、`activity.go`（106）：合并执行入口、只读查询、TUI /agents 面板用的活动快照（含 Team 状态）。
14. `team_cancel.go`（75）：CancelTeam → beginTeamCancel（admission 锁内置为 canceling）→ 中断全部活跃子 Agent → Wait → 置为 canceled。审计：已 canceled 时直接返回；Wait 失败时 Team 停在 canceling，重复调用可完成；root 不被中断。

### internal/agentmanager（第 2 段：工具与测试）

14. `tools_bindings.go`：Supervisor 暴露 `agent_resume`、`agent_wait` 等工具；`agent_wait` 先由 Eino StatefulInterrupt 保存 checkpoint，再由 Supervisor 创建 WaitSet。没有 merge_apply 工具，合并写回只能经 Supervisor 内部的 mergeApprover 审批。
15. `tools.go`（265）：`actorFromContext`；参数严格解码；所有工具只能操作直接子 Agent；`waitTool` 调 `Suspend` 后返回 `WaitSuspendedError`，由 conversation 和 cli 识别为"挂起"而不是失败。
16. `tools_task.go`（332）：task_* 工具。审计：`expectedTaskVersion` 缺省为 AnySequence，会跳过 CAS 检查。
17. `tools_merge.go`（97）：merge_run、merge_requeue 等工具（默认策略要求审批）。

测试（★ 表示值得与源码对照读）：

- `activity_test.go`（154）：活动快照内容。
- ★`profile_test.go`（244）：委派收窄、孙代不能重新获得权限。
- ★`supervisor_cancellation_test.go`（76）：Interrupt 与 Close 的取消语义。
- ★`supervisor_delivery_test.go`（463）：必读。快照交付、验收、reviewer、合并排队与 supersede 全流程。
- ★`supervisor_lifecycle_test.go`（306）：spawn → run → finish → 结果回投父 Agent，重启恢复。
- `supervisor_test.go`（1245）：覆盖面最广，可按用例名挑读。

### internal/agentflow

职责：声明式多步流程（flow）运行时：步骤算子、Agent 会话步骤、人工确认，以及基于持久记录的重放。项目内依赖：orchestration、session、tokenusage、tool。

1. `types.go`（238）：Flow 定义、Step、Run、Store 端口与错误（如 ErrStepConflict）。
2. `runtime.go`（345）：按 runKey 加锁；`CreateRun` 幂等；遇到 `WaitSuspended` 时 run 保持 Running；`FinishRun` 用 WithoutCancel。工具调用经 `tool.Pipeline.Execute`，快照经 RetainSnapshot/ResolveSnapshot 固定。
3. `operators.go`（635）：`eachStage`/`parallelStage` 起 goroutine；AgentSession 的 spawn key 为 run:step:spawn；agent_wait 默认 30 分钟；`itemIdentity` 用 "#n" 标识元素。审计：并发阶段的错误汇总与取消传播。
4. `catalog.go`（199）：flow 定义目录、校验与指纹。
5. `tools.go`（94）：flow 工具；`human_confirm` 执行器总是批准，真正的门在 policy 的 RequireApproval 规则。

测试：★`runtime_test.go`（572）、★`snapshot_recovery_test.go`（136，重启后按原快照重放）。

跨包：sqlitestore/flows.go 实现 Store；app/open.go 调 `agentflow.NewRuntime`；supervisor 提供 AgentSession 能力。

### internal/workspacecoord

职责：工作区视图与合并协调：快照物化、候选 diff、三方合并、apply 计划与提交、冲突解决、快照评审视图，以及旧的 git-patch 合并路径。项目内依赖：orchestration。

推荐阅读顺序：

1. `types.go`（304）：View、Merge、各状态、端口接口；路径校验用 `filepath.IsLocal`。
2. `coordinator.go`（118）：Coordinator 组装与入口方法。
3. `preservation.go`（148）：保留记录的 CAS 更新，最多重试 5 次，内部是 O(n) 扫描。
4. `manifest.go`（211）：快照清单（文件、模式、哈希）的构建与比较。
5. `clone_darwin.go` / `clone_linux.go` / `clone_other.go`：按平台的文件克隆（clonefile/reflink/普通复制）。
6. `snapshot.go`（413）：`Materialize`。Barrier（工作区根写租约）只在 main → base 复制期间持有。审计：`copyFile` 存在检查与使用之间的 TOCTOU 窗口。
7. `candidate.go`（171）：从快照工作目录生成候选变更与 digest。
8. `merge3.go`（278）：三方合并与冲突标记（`writeConflict`）。
9. `applyplan.go`（261）：`BuildApplyPlan`，计算每个路径的前像、目标内容与冲突。
10. `applycommit.go`（184）：`CommitPlan`、`VerifyPreimages`。临时文件名 `.gsea-apply-`+hash16，fsync 文件、rename、fsync 目录。配合 sqlitestore/merge_apply.go 的 journal 读，崩溃后按 journal 续提交。
11. `resolution.go`（106）：冲突解决视图与任务。
12. `snapshot_review.go`（127）：给 reviewer 的只读评审视图，diff 上限 64KiB。
13. `merge.go`（337）：合并执行器。审计：审批期间一直持有 mutex，因此 Discard 会被阻塞；Conflicted 与 CreateTask 之间存在崩溃窗口。
14. `merge_snapshot.go`（356）：快照合并主流程。`planRounds = 3`；`commitIfStable` 重建计划并比较 digest，不一致就重来；`resumeSnapshot` 续跑；Applied 与 completeTask 之间存在崩溃窗口。
15. `git.go`（188）、`git_patch.go`（95）：git-patch 路径。剥离 `GIT_*` 环境变量，拒绝以 "-" 开头的 revision（防参数注入）。

测试：`merge_test.go`（230）、★`merge3_internal_test.go`（111，必读）、`preservation_test.go`（269）、★`snapshot_test.go`（295，必读）、`memory_journal_test.go`（77，ApplyJournal 的内存参考实现）、`export_test.go`（10）。

### internal/workspacedomain

职责：跨进程的工作区域：同一物理工作区由多个 gsea 进程共享时的身份识别、实例登记、文件锁、写租约与汇合点（rendezvous）。项目内依赖：orchestration。

1. `identity.go`（44）、`identity_unix.go`（19）、`identity_other.go`（11）：用 dev+ino 识别工作区，不受路径别名影响。
2. `lock_unix.go`（29）、`lock_other.go`（20）：平台文件锁；不支持的平台直接失败（fail closed），`errUnsupported` 在 lock_other.go。
3. `instance.go`（134）：进程实例登记与存活判断。
4. `store.go`（193）：独立 SQLite，WAL、`_txlock=immediate`、busy 5000、MaxOpenConns 1，`withTx` 包装。
5. `rendezvous.go`（158）：多进程汇合与协调目录。
6. `domain.go`（300）：锁顺序；Degraded 模式；`RetainUntilExit`。
7. `leases.go`（487）：必读。冲突检查、回收失效租约、授予新租约在同一个 IMMEDIATE 事务里完成；路径前缀重叠即冲突；pathKey 转小写；同 key 同 hash 会创建新租约；存活探测失败时按冲突处理（fail closed）。

测试：★`domain_test.go`（228）、★`leases_test.go`（210），都建议必读。

跨包：codingtools 的 `withLease` 调 `AcquireLease`；app/open.go 调 `workspacedomain.Open`，app/snapshot_merge.go 的 barrier 使用根租约。

### 多 Agent 端到端调用链

(a) spawn 子 Agent 到结果回到父 Agent
1. 父 Agent 调 agent_spawn：agentmanager/tools.go spawnTool → supervisor.go `Spawn` → sqlitestore/agents_spawn.go `CreateChild`（一个事务内：幂等记录、子 Agent 行、带 TaskID 时 `claimTaskForAgentTx`、事件）。
2. supervisor_scheduler.go `startAvailable` → `start` → supervisor_run.go `run`。
3. supervisor_workspace.go `startWorkspaceView` 分配视图 → `loadMessages` 读 mailbox。
4. Runner 执行：cli/conversation_builder.go `newChildRunner` → conversation.Open → RunTurn（与普通 turn 相同的内循环）。
5. ack mailbox → `finish` → `afterAgentTransition`：向父 Agent Enqueue 结果消息、Reconcile WaitSet、scheduleMerges。
6. 父 Agent 侧：waitTool → `Suspend` → 创建 WaitSet，Agent 进入 Waiting → 子 Agent 结束时 `ResolveWaitSet` → 父 Agent 回到 Queued，被重新调度后从 mailbox 读到结果。

(b) 快照隔离写入到合并回主工作区
1. supervisor_workspace.go `startSnapshotView` → workspacecoord/snapshot.go `Materialize`（持有 Barrier 根租约，复制 main → base）。
2. 子 Agent 在快照工作目录中 write/shell。
3. 结束时 supervisor_delivery.go `deliverSnapshot` → Task 进入 AwaitingMerge；有 VerificationProfile 时先进入 Verification → spawn reviewer → task_verify。
4. `scheduleMerges` → `RunPendingAutomatic` → `runOne`（`mergeReady` 检查 Task 是否 awaiting_merge）→ merge_snapshot.go `runSnapshot`：`BuildApplyPlan`，有冲突则 `markConflict`。git-worktree 子 Agent 走 `deliverPatch` 入队 git-patch merge，同一入口进入 merge.go `runPatch`（`git apply --3way`）。
5. 审批：app/snapshot_merge.go `mergeApprover`（merge_apply 请求，附计划 diff）。
6. `commitIfStable`：持有 barrier，重建计划并比较 digest，`VerifyPreimages`，`CommitPlan`（journal 记录每个路径）→ `markApplied` → `verifyApplied`（配置了 post_merge_check 时运行检查）→ `finishAwaitingTask` 把 Task 置为 completed 或 failed。
7. 冲突路径：创建解决任务 → `PrepareResolution` → 新候选 supersede 旧 merge。

(c) Task DAG 调度
TransitionTask / 依赖完成等触发 → supervisor_tasks.go `dispatchReadyTasks` → `Spawn`（带 TaskID）→ `claimTaskForAgentTx` → `startAssignedTask` → 子 Agent 运行 → supervisor_assignment.go `finishAssignedTask` → sqlitestore/tasks.go `TransitionTask` → Completed 时 `unblockDependentTasksTx`。

(d) 进程重启
workspacedomain.Open → app/orchestration.go NewSupervisor → supervisor_recovery.go `Recover`（顺序见 agentmanager 第 12 条）→ 子 Agent 以 recovering=true 重新运行（cli 的 RecoverRun/ResumeTurn）→ Running 状态的 merge 经 apply journal 续提交 → agentflow 按持久记录重放。

未确认的问题（审计时需要核实）：
- 终态 Agent 结果与父级 `MessageResult` 由 `TransitionAgent` 事务内写入；`agent_completions` 保存运行结果冻结点，Supervisor 重启时补做未结算的 Task/交付/终态效果。
- 两个崩溃窗口：Applied 与 completeTask 之间、Conflicted 与 CreateTask 之间。
- reviewer 视图的只读由谁强制（codingtools 的 ensureWritable 拒绝 ViewSharedReadonly，需确认路由一定命中）。
- human_confirm 背后真正的门（policy 规则）。
- Blocked→Ready 与 supersede 在 sqlitestore 中的实现细节。
- `LeaseSpec.Validate` 校验 trim 后的路径，存储层是否也用 trim 后的值。

## 第 6 阶段：配置、组装、CLI 与 TUI

顺序：config → app（组合根）→ cmd/local-agent 与 cli → tui → architecture，最后读入口/App/TUI 调用链。

### internal/config

职责：配置模型、默认值、多来源合并与路径解析。配置中不保存密钥，只保存环境变量名。项目内依赖：`agentgoal`、`lsp`、`plugin/wasm`。

1. `config.go`（168）：
   - 必读：`Config`、`ModelConfig`（只有 `APIKeyEnvironment`，没有明文 key）、`AgentConfig`、`AgentsConfig`、`Resolve`、`SessionDatabasePath`、`SessionRegistryPath`。
   - 环境变量：`GSEA_HOME`、`GSEA_PROFILE`、`GSEA_COORDINATION_DIR`。
   - 审计：`Resolve` 中 DataDirectory 为空时回落到 Home，转绝对路径后 MkdirAll 并 Chmod 0700；workspace 为空时取 Getwd，且必须是目录。`SessionDatabasePath = Data/sessions.db`，`SessionRegistryPath = Home/control.db`。
2. `load.go`（804）：`Load` 的合并顺序，按顺序读：
   1. `builtInConfig`：内置默认值。
   2. coordination 目录：来自环境变量或用户状态目录，配置文件不能覆盖。
   3. strict 模式：选项或 `GSEA_STRICT_CONFIG`。
   4. `home/config.toml`。
   5. `home/<profile>.config.toml`：必须存在，经 `validateProfileName` 校验。
   6. 从 cwd 向上找到的最近一个 `.gsea/config.toml`：只取一个，不逐级合并。
   7. `applyEnvironment`。
   8. `validate`。
   - 审计：用指针字段区分"显式设置"与"零值"；ModelLimits 用 `maps.Clone` 合并；`OPENAI_*` 与 `OLLAMA_*` 只作回退。
   - 默认值：provider ollama、chat-completions 协议、`OPENAI_API_KEY`、超时 2 分钟、MaxIterations 20；Agents 4/16/3/4 且开启 AutoContinue；Goal 4 轮/30 分钟；LSP 为 `gopls serve`；WASM 1024 页。
   - **发现**：不可信仓库里的项目配置可以设置 `lsp.command/args`（会被执行）、`model.base_url` 加 `api_key_environment`（可把凭据发到任意地址）、data/workspace 目录和 instruction。没有找到信任门。另外项目配置按 cwd 查找，而不是按 `--workspace`。

测试：★`config_test.go`（447，`TestLoadPrecedence`、`TestResolveSecuresDataDirectory`，对照 load.go 的合并顺序读）、`context_test.go`（71）。

跨包：cli 的 `chat_options.go`、`doctor.go`、`control.go` 调 `config.Load`；`session.go` 用 `config.Default()`；app 的 `OpenWithOptions` 接收已解析的 Config。

### internal/app（组合根，第 1 段）

职责：把数据库、工作区域、工具注册表、策略、审批、WASM 插件、Supervisor 与 AgentFactory 组装成一个 App，并负责有序关闭。依赖几乎所有领域包。按架构测试，只有 internal/cli 能导入它，而且只有它能调用 `sqlitestore.Open`、`workspacedomain.Open`、`wasm.NewRuntime`。

1. `app.go`（74）：`App` 的全部字段；`OpenOptions{ShellSandbox, ToolBindings}`。先读字段，后面每个文件都在填这些字段。
2. `open.go`（264）：`OpenWithOptions` 装配全过程，是本包最重要的文件。
   - 顺序：`runtimeowner.Acquire` 先于 `sqlitestore.Open`，保证同一数据目录只有一个进程持有。
   - 失败回滚顺序：evolutionCancel → pluginManager.Close → closeOpenResources（diagnostics → broker → wasm → database）→ domain.Close → owner.Close。
   - 审计：`closeOnError` defer 要等 broker 创建后才注册，之前的步骤各自手工关闭资源。逐个检查每个提前 return 的分支有没有漏关。
   - 同文件还有 `leaseOwnerValidator` 与 `approvalSource`。
3. `close.go`（96）：`closeOnce`。
   1. 置 closed，调用 evolutionCancel。
   2. `joinEvolution` 最多等 10 秒；超时则 `retainUntilExit` 并返回。
   3. `supervisor.Close`；遇到 `ErrSupervisorCloseTimeout` 也 `retainUntilExit`。
   4. 依次关闭 plugins → diagnostics → approvalBroker → wasm → database → domain → owner，错误用 join 汇总。
   - 审计：超时分支故意不关数据库和租约，让进程退出时由内核释放，避免仍在运行的 goroutine 写已关闭的库。
4. `builtins.go`（77）：`buildPolicy` 默认拒绝。需要审批的有 write、edit、shell、merge_run、merge_requeue、plugin_*、strategy_*、extension_*、human_confirm 和 bridge 工具。`buildBuiltinBindings` = diagnostics + artifact_read + `workspace.Bindings()`。
5. `agent_factory.go`（83）：`NewAgentFactory` 接受 0 或 1 个 approver，用 `pipeline.WithApprover` 注入；设置 GoalStore、Budget、ContextArtifacts、Strategies；root 追加 `rootWorkspaceInstruction`；`StrategyScope = Digest(manifest.Host, nativeScope, scope)`，并记录 shadowFactories。
6. `orchestration.go`（156）：`NewSupervisor` 填默认值，其中 MergeExecutor 不带 approver。
   - `ActivateManyWithCommit` 注册工具，重名拒绝。
   - 之后 `Recover(context.Background())`；失败时撤销已注册的 bindings。
   - `closeFailedSupervisor`；`EnsureRootThread` 使用 "default" profile。
7. `resources.go`（129）：各种访问器；`newMergeExecutor` 设置 `AutoMerge = config` 与 `Approver = mergeApprover`。注意与第 6 条的默认 executor 对比（cli control 走的是默认值）。
8. `snapshot_merge.go`（110）：
   - `snapshotBarrier` 申请工作区根写租约，最多等 6 分钟，用 WithoutCancel 释放。
   - `mergeApprover` 构造 `policy.Request{ToolName: merge_apply, Capability: workspace.write, RuleID: merge.apply}`，附带计划 diff。这是合并写回主工作区前唯一的人工审批点。
9. `snapshot_scope.go`（70）：`nativeSnapshotScope` 摘要，用于策略作用域。
10. `profiles.go`（57）：内置 default、worker、reviewer 三个 profile；MaxChildren 4，工具调用上限 = 迭代数×8，30 分钟。

### internal/app（第 2 段：其余文件与测试）

11. `evolution_lifecycle.go`（34）：自进化后台任务的启动与 join，配合 close.go 的 10 秒等待读。
12. `evolution_scope.go`（38）：`strategyCaller`。
13. `doctor.go`（147）：`runtime_diagnostics`，汇总运行时自检结果，`cli doctor` 的数据来源。
14. `artifacts.go`（73）：`artifact_read` 工具，默认读 64KiB，最大 1MiB。审计：先把整个 artifact 读进内存再截取。
15. `maintenance.go`（75）：`withOfflineDatabase`，离线打开数据库执行 retention 计划、归档、压缩。
16. 小文件，一次读完：`recovery.go`（30，AbandonRecovery 入口）、`usage.go`（26，ReadSessionUsage）、`plugin_build.go`（22）、`plugin_tools.go`（10）、`plugin_verification.go`（18）、`strategy.go`（26）、`strategy_tools.go`（10）。

测试（★ 表示值得与源码对照读）：

- ★`close_test.go`（283）：关闭顺序、超时保留资源、重复关闭。对照 close.go。
- ★`ownership_test.go`（51）：第二个进程打开同一数据目录被拒。对照 open.go 中的 runtimeowner。
- ★`app_test.go`（210）：完整装配与基本工具调用。
- ★`builtins_test.go`（36）：默认策略中哪些工具需要审批。
- ★`snapshot_merge_test.go`（86）：merge_apply 审批请求的内容与 barrier。
- `workspace_domain_test.go`（86）、`plugin_evolution_test.go`（104）、`plugin_loop_test.go`（179）、`strategy_test.go`（220）、`evolution_scope_test.go`（55）、`usage_test.go`（82）、`recovery_abandonment_test.go`（56）、`agent_factory_test.go`（23）、`artifacts_test.go`（38）、`snapshot_scope_test.go`（30）、`responses_workspace_test.go`（108）。

跨包：被 cli 的 chat、doctor、control、maintenance、plugins、strategy、eval 调用；向下调用 sqlitestore.Open、workspacedomain.Open、wasm.NewRuntime、tool.NewPipeline、agentmanager.NewSupervisor、agentruntime 的 factory 构造。

### cmd/local-agent

职责：进程入口。依赖：`cli`。

- `main.go`（15）：`signal.NotifyContext(ctx, os.Interrupt)` 后调用 `os.Exit(cli.Run(...))`。
  - **发现**：只监听 SIGINT。收到 SIGTERM 时 ctx 不会取消，chat 路径上的 defer（`app.Close`、把 Session 标记为 stopped）都不会运行。
  - `os.Exit` 跳过了 `stop()` 的 defer，没有实际影响。
- 没有测试。目录里有一个与代码无关的 `approval-test.txt`（20 字节），像是审批写入测试的残留。

### internal/cli（第 1 段：主路径）

职责：解析并分发子命令，把 config、app、tui 和 headless 输出组装成命令。依赖面最宽：agentgoal、agentmanager、agentprofile、agentruntime、app、approval、codingtools、config、conversation、evaluation、modelgateway、orchestration、plugin、pluginbuild、policy、render、sandbox、session、sessionregistry、strategy、tokenusage、tool、toolbridge、tui、workspacecoord。

1. `runner.go`（142）：`Run` → `runWithDependencies`，按第一个参数分发子命令。
   - 退出码：成功或 `flag.ErrHelp` 为 0；出错为 1；`context.Canceled` 为 130（只限 chat、control、eval）；未知命令或无参数为 2。
   - `dependencies{newModel, evalVerifier}`，生产环境用 `modelgateway.New`。
   - 审计：还有一个未导出的重复入口 `run()`。
2. `chat_options.go`（271）：`parseChatOptions` 以已加载配置为默认值定义全部 flag，再用 `flags.Visit` 记录哪些 `*Explicit` 是显式给出的。
   - 校验：evaluation 必须显式给 `--workspace`；shell-sandbox 只能是 os 或 container，container 需要 evaluation；tool-bridge 两个路径必须同时给出且需要 evaluation；timeout、max-iterations 必须大于 0；compaction 必须小于 context window；headless-format 只能是 stream 或 json；recovery 需要 `--session`。
   - session ID 必须匹配 `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`，缺省时生成。
   - **发现**：`profileArgument` 只预扫 `--profile`、`-p`、`--profile=`、`-p=` 四种写法，而 flag 包还接受 `-profile x` 和 `--p`。用这两种写法时 `config.Load` 不会加载该 profile，与后续 flag 解析结果不一致。
3. `chat.go`（469）：`runChatWithPrompt` 是主路径。
   - Session 目录 `HOME/control.db` 只在显式给出 `--session` 且文件存在时打开；目录中的注册值只填补没有显式给出的 data-dir、workspace、model、provider。
   - defer 按 LIFO 执行：`app.Close` → `UpdateState(stopped,0)`（用 `context.Background`）→ `catalog.Close`。出错返回时也记为 stopped 而不是 failed，`UpdateState` 的错误被忽略。
   - prompt-file 上限 4MiB；stdin 最多读 4MiB+1；普通文件先 `os.ReadFile` 整个读入再检查大小。
   - stdin 和 stdout 都是字符设备时才进入交互模式；交互模式拒绝 json 输出格式和 evaluation。
   - `selectChatApprover`：TTY 下用 broker；普通 headless 用 `DenyApprover`（需要审批的操作一律拒绝）；evaluation 用白名单（write、edit、shell、merge apply，或只允许 bridge 工具）。
   - `validateEvaluationWorkspace` 拒绝文件系统根目录和包含 HOME 的目录；取不到 HOME 时直接放行。
4. `chat_headless.go`（257）：`runHeadlessChat`、`runHeadlessPrompt`、`runHeadlessResume`。
   - stream 模式：`turnOutput` 实时输出，回合结束后在 stdout 写一行 `usage: input=… output=… reasoning=… total=…`，evaluation 用正则解析这一行。
   - json 模式：事件 sink 为空操作，只输出一个 `{"schema":"gsea.chat.result.v1",content,usage,goal?}` 对象，`SetEscapeHTML(false)`。
   - header 和恢复列表写 stderr；`scanInput` 用 goroutine + channel 读输入，缓冲 1MiB；内建命令 `/recoveries /resume /exit /quit /session /help`。
5. `chat_output.go`（102）：`turnOutput` 实现 Sink。GoalPhase 输出 `[task X · round n/m]`；推理增量原样写出；正文增量经 `render.MarkdownStreamRenderer`；一个增量都没收到时用 final.Content 兜底。**stdout 上协议输出与人类可读输出的边界由这个文件决定。**
6. `root_conversation.go`（28）：`openRootConversation` 依次 `supervisor.AgentMailbox` → `factory.WithMailbox` → `factory.SettleBackground(sessionID)` → `conversation.Open`。
7. `runtime_config.go`（22）：`rootRuntimeConfig` 设置 `StrategyScope=strategy.Digest(options.model)`，带上 Tokenizer 和 UsageLedger。
8. `conversation_builder.go`（227）：子 Agent 的上下文继承和运行器。
   - `inheritanceLimits`：default 4 条/8000 字符；recent 12 条/24000 字符；full 全部/48000 字符。
   - 继承的父上下文和 mailbox 内容分别加 "Untrusted parent context" 和 "Untrusted Agent messages" 头。
   - `newChildRunner`：`agentprofile.Decode` → `WithProfile` → 截止时间 CreatedAt+MaxWallTime → Open → `RecoverRun`、`ResumeTurn` 或 `RunTurn`；`WaitSuspendedError` 转成 `RunnerOutcome{Wait}`；`projectChildResult` 把 Goal 状态映射为结果状态。

### internal/cli（第 2 段：其余子命令与测试）

9. `control.go`（229）：control 子命令，JSONL 协议。
   - 输入 protocol 为 `gsea.command.v1` 或留空，输出 `gsea.control.v1`。
   - 命令：snapshot、agent.list、task.list、workspace.list、merge.list、events（返回 next_cursor）、agent.cancel、team.cancel（需要 team_id，可选 timeout_ms）、merge.run、merge.requeue、task.transition（需要 actor_agent_id）、wait。
   - 审计：`config.Load` 不带 profile；`defer application.Close()` 丢弃关闭错误；`NewSupervisor(agentmanager.Config{})` 没有 Runner，MergeExecutor 由 app 默认值（不带 approver）填入。**merge.run 是否绕过人工审批取决于 auto_merge 和 executor 对 nil approver 的处理，建议重点核对**（按设计：无 Approver 且未开 AutoMerge 时快照 merge 保持排队）。
10. `session.go`（191）：用 `config.Default()`，不支持 `--profile`。create、list、show、remove 操作 `HOME/control.db`。
    - create 把 workspace 转为绝对路径并要求是目录，运行目录为 `HOME/sessions/<id>/{data,tmp,tool-state}`。
    - remove 拒绝 running 状态的 Session。进程被 SIGKILL 或 SIGTERM 后状态会停在 running（见 main.go），是否有 PID 存活检查未确认。
11. `doctor.go`（61）：profileArgument → config.Load → 覆盖 data-dir 和 workspace → app.Open → Doctor，输出缩进 JSON，Close 错误 join 进返回值。
12. `maintenance_options.go`（67）、`maintenance.go`（40）：plan、archive、compact。plan 不允许 confirm 和 plan-id；archive 需要 confirm、plan-id、session；compact 需要 confirm 且不能给 session。三者都走离线的 `app.PreviewRetention`、`ArchiveSession`、`CompactDatabase`。
13. `plugins_options.go`（126）→ `plugin_json.go`（52）→ `plugins_input.go`（44）→ `plugins.go`（100）。
    - 每个动作有自己的 flag 白名单。activate 和 rollback 需要 `--report-id` 和 `--confirm`；retire 和 collect 需要 `--confirm`；collect 还需要 `--plan-id`。
    - `decodePluginJSON` 用 DisallowUnknownFields 且只接受一个 JSON 值；`readBoundedPluginFile` 要求普通文件，读 limit+1 字节判断超限。
    - plugins.go：gc-plan 和 collect 离线执行；其他动作先 `loadPluginInput`（在打开 App 之前），再 `app.Open`。build 失败但已有 BuildID 时先输出结果再返回错误；状态不是 awaiting_approval 时输出并返回 "quarantined or failed"；test 未通过时输出报告并返回错误。
    - 审计：activate 和 rollback 共用 PromotePlugin；Retire 调用时没有传 ctx。
14. `strategy_options.go`（127）、`strategy.go`（94）：propose、suite、inspect、report、shadow、publish、rollback。suite、shadow、publish、rollback 需要 `--confirm`。shadow 解析 `--` 之后的 chat flag 并拒绝其中的 `--profile`，依次 newModel → EnsureRootThread → NewAgentFactory（不传 approver）→ ShadowStrategy。
15. `eval.go`（594）：doctor、run、validate、export-swebench、summarize、score。
    - `runEvalTasks` 拒绝受保护的 chat flag（evaluation-mode、workspace、data-dir、session）；结果根目录默认 `MkdirTemp`（0700），结果文件 O_TRUNC、0600。
    - `executeEvaluationTasks` 是 worker 池，consumer 出错时 cancel。
    - `evalAgentFunc` 在进程内调用 `runChatWithPrompt`，写 transcript.txt 和 agent.stderr.txt（0600），再用 5 秒 WithoutCancel 上下文调用 `app.ReadSessionUsage`；`evaluationSessionID` 格式 `eval-<safe>-<sha256 前 4 字节>`。
    - **发现**：`hasCLIFlag` 只检查 `--name` 写法，单横线 `-workspace` 能绕过拒绝。评测器自己的 flag 追加在最后、按最后一个值生效，实际影响未确认。
16. `recovery_abandonment.go`（12）：writeAbandonment；`doc.go`（4）：包注释。

测试（★ 表示值得与源码对照读）：

- ★`chat_approval_test.go`（109）：headless 默认拒绝；evaluation 白名单；bridge 工具独占白名单；TTY 拒绝 evaluation；TTY 用 broker；评测工作区拒绝过宽根目录。
- ★`chat_options_test.go`（278）：输出额度延后到自动解析；Goal 激活覆盖；provider 配置成组；model/provider 显式标记独立；非法 session ID；evaluation 需要显式 workspace；profileArgument 长短写法。
- ★`root_conversation_test.go`（255）：子结果在回合后才 ack；回合失败时 mailbox 保持已投递；wake 回合消费子结果；崩溃前已提交的回合会被 settle。
- ★`recovery_test.go`（129）、★`child_recovery_test.go`（158）：resume 是操作不是新 prompt；列出恢复项时不启动 Supervisor 和 provider；root resume 不消费新 mailbox 消息；子恢复使用原始 run，不重复写入。
- `recovery_abandonment_test.go`（98）：放弃恢复需要明确目标、原因和同意，离线执行。
- ★`runner_test.go`（344）：doctor、help、多回合、推理与 usage、机器可读结果、多行 stdin、evaluation 下写入经 pipeline 获批。
- `eval_test.go`（252）、`eval_usage_test.go`（45）：validate/export、run 自行掌管隔离参数、有界并发、consumer 失败取消、usage 解析、session ID 不碰撞、端到端补丁捕获。
- `evolution_test.go`（123）、`evolution_options_test.go`（157）、`evolution_examples_test.go`（32）：maintenance/plugins 离线；strategy 需要显式同意；参数校验先于执行；help 不打开运行时。
- `conversation_builder_test.go`（78）、`goal_output_test.go`（51）、`control_test.go`（54）、`plugins_input_test.go`（72）：继承过滤与字符上限；mailbox 标为不可信；Goal 不破坏 JSON 输出；control 输出版本化 JSONL；插件输入在打开运行时之前被拒绝。
- `main_test.go`（39）：TestMain 把 GSEA_HOME 和协调目录指向临时目录，避免读到开发者配置。

CLI 子命令 → 入口：

| 命令 | 入口 |
| --- | --- |
| chat | `chat.go:runChat` → `runChatWithPrompt` |
| doctor | `doctor.go:runDoctor` |
| control | `control.go:runControl` → `executeControlCommand` |
| session create/list/show/remove | `session.go:runSession` → `createSession`/`listSessions`/`showSession`/`removeSession` |
| maintenance plan/archive/compact | `maintenance.go:runMaintenance` → `executeMaintenance` |
| plugins list/stage/build/inspect-build/test/activate/retire/rollback/gc-plan/collect | `plugins.go:runPlugins` → `executePlugins` |
| strategy propose/suite/inspect/report/shadow/publish/rollback | `strategy.go:runStrategy` → `executeStrategy` |
| eval doctor/run/validate/export-swebench/summarize/score | `eval.go:runEval` → `runEvalDoctor`/`runEvalTasks`/`runEvalValidate`/`runEvalExportSWE`/`runEvalSummarize`/`runEvalScore` |
| help、-h、--help | `runner.go:printUsage` |

### internal/tui（Bubble Tea v1.3.10）

职责：交互式终端界面：回合流式渲染、审批、/agents 面板、后台 wake 与恢复入口。只通过接口调用 Conversation，不直接读事件库。依赖：agentmanager、agentruntime、approval、conversation、modelcontext、orchestration、render、session、tokenusage、workspacecoord。

推荐阅读顺序：
1. `run.go`（108）：Conversation（RunTurn）、ContextConversation、pendingContextConversation、BackgroundConversation（PendingBackground、RunBackgroundTurn）、AgentControl、TranscriptReader 接口与 Config。Run → NewModel → tea.NewProgram(WithContext、WithInput、WithOutput(wrapCursorWriter)、AltScreen、MouseCellMotion) → program.Run，之后返回 ctx.Err()。审计：ctx 取消时 program.Run 的错误被包装为 "run terminal UI"，是否仍满足 errors.Is(context.Canceled) 从而得到退出码 130，未确认。
2. `model.go`（305）：Model 全部状态字段。NewModel 用 History 重建输入历史（跳过后台输入）并恢复 lastUsage；只有 AutoContinue 为 true 且 conversation 实现 BackgroundConversation 时才设置 background。Init 用 tea.Batch 启动 Focus、标题、恢复（startResume 或 discoverRecoveries）、唯一的 waitForApproval、startActivityRefresh、scheduleBackgroundCheck。Update 是状态机分发表，未处理的消息交给 textarea。
3. `messages.go`（136）：runTurnCommand 与 runOperationCommand 在 Cmd goroutine 中运行回合。sink 把 runtimeEventMsg 阻塞写入 turnEvents，或在 eventContext 结束时返回其错误。turnDoneMsg 优先阻塞发送；turnContext 已取消时只做一次非阻塞发送。waitForTurnEvent 每次取一条；waitForApproval 读 broker.Requests() 或 Done()。审计：取消回合时若 256 容量的缓冲恰好已满，turnDoneMsg 会被丢弃，界面停在 busy（理论边界，未复现）。
4. `keys.go`（183）：handleKey 优先级：审批（Enter 确认；Esc/Ctrl+C 拒绝；方向键选择）> viewer > 面板 > 全局键。运行中 Ctrl+C 取消 turnContext 并置 wakeSuppressed；空闲时清空输入或退出；运行中 Enter 被忽略。submit：先 handleCommand；否则追加用户块、resetWakeBudget、更新历史、ContextUsageFor，再 startOperation("Thinking", runTurnCommand)。startOperation 设置 busy，派生 turnContext，创建 turnEvents（容量 256），Batch(回合 Cmd, waitForTurnEvent, spinner.Tick)。
5. `turn_state.go`（286）：applyRuntimeEvent（GoalPhase 改 stage；推理与正文增量追加到 pending 块；AssistantMessage 按 call ID 建工具块；ToolResult 回填）；applyApprovalRequest（已有待决请求时排队；只有 root 的请求挂到现有工具卡片，子 Agent 请求单独建卡片并标来源）；resolveApproval（broker.Resolve 后出队，不新开 waiter）；finishTurn（清理状态；跳过的 wake 不留痕迹；只有非 wake 回合增加 completedTurns；没有流式正文时用 Final 兜底；更新 usage）。
6. `background.go`（152）：wake 策略，见下面调用链 (d)。
7. `recovery.go`（78）：RecoveryConversation 是可选接口。discoverRecoveries 启动时静默运行，只在有中断工作时显示且注明"不会自动执行"；startResume 在 busy 时拒绝。
8. `commands.go`（159）：/resume /context /recoveries /agents /activity /merge /diff /exit /quit /clear /compress /session /reasoning /help。/compress 设置 busy 和 turnCancel，但不用 turnEvents，结果经 compressDoneMsg 返回。
9. `activity.go`（336）：Team 快照刷新链。LoadTeamSnapshot 超时 2s；面板打开时每 2s 刷新，隐藏时每 5s；activitySequence 让旧 tick 失效，保证只有一条刷新链。root 行状态由 TUI 的 busy 推断。
10. `agents_panel.go`（270）：o/Enter 打开转录，x 中断，r 重新审查 merge，d 丢弃 merge；x 和 d 需按两次确认；每个动作超时 5s；输入框非空时字母键归输入框。
11. `transcript.go`（583）：blocksFromHistory、refreshTranscript、transcriptParts（连续已完成工具块折叠成 "N tool calls"）、renderBlock（按字段缓存）、hasDiffPayload、formatToolResult、审批卡片渲染。审计：模型输出和工具结果只经过 ansi.Hardwrap 或 glamour，没有剥离控制序列，不可信的 ESC/OSC 序列可能到达终端（上游是否清洗未确认）。
12. `view.go`（275）：头部、分隔线、面板、viewport、状态栏、命令提示、输入框的布局；resize/resizeInput；状态栏显示 ctx 用量、token、cache 命中率、Agent 摘要。
13. `markdown.go`（222）：glamour（按宽度缓存）加自写的代码栅栏切分，chroma 高亮。
14. `tool_status.go`（37）：只识别 JSON 中的 "error" 字段与 shell_exec 的 timed_out、exit_code。
15. `tool_disclosure.go`（100）：Ctrl+O 展开/折叠全部；点击行切换单个块。
16. `path_links.go`（88）：对存在的文件路径加 OSC 8 链接。审计：渲染时对模型输出中任意绝对路径执行 os.Stat（不读内容）。
17. `selection.go`（150）、`input_selection.go`（200）、`cursor.go`（122）、`context.go`（71）、`styles.go`（77）：鼠标选择与复制、输入框换行探测、IME 光标定位（cursorWriter 把自定义 OSC 标记替换为光标移动序列）、/context 文本、样式。审计：wrapInputLine 每个逻辑行新建 textarea 逐列探测，粘贴大段文本约 O(n²)。

测试（★ 表示值得与源码对照读）：
- ★`background_test.go`（149）：有待处理的 Agent 消息时启动 wake；用户输入后重置 wake 预算；取消的 wake 抑制后续 wake；跳过的 wake 不留痕迹；AutoContinue 关闭时从不 wake；后台输入渲染为 Agent 消息。
- ★`model_test.go`（1047）：
  - 审批：审批挂到现有工具块、merge 审批显示计划 diff、子 Agent 审批排队并显示来源、活动审批状态在视图中只出现一次、方向键加 Enter 选择。
  - 回合与事件：消费推理、工具和正文事件；取消活动回合；finishTurn 不清除流式 usage；从历史恢复 lastUsage；新回合固定在底部。
  - 其余用例覆盖布局、输入换行和选择、cursorWriter、工具块分区和折叠、diff、markdown 代码面板、命令补全和历史。
- ★`agents_panel_test.go`（160）：显示实时活动、token 和摘要；确认后中断子 Agent；审查和丢弃 merge；打开子 Agent 转录；输入的文字留给输入框。
- ★`recovery_test.go`（61）：恢复命令只发现不执行；resume 不需要新的输入。
- `context_test.go`（129）：容量和工作预算说明；未知容量与延后压缩；状态栏使用输入预算；finishTurn 优先使用回合完成时的 context usage。
- `tool_status_test.go`（115）：已批准但失败的工具在折叠时仍可见；折叠成一行摘要；历史失败可见；toolFailureSummary。
- `activity_test.go`（90）：加载并渲染控制面快照；行截断不溢出；/merge 摘要。
- `goal_phase_test.go`（33）：Goal 阶段替换 Thinking，并显示耗尽状态。
- `path_links_test.go`（38）：链接目标保持完整；未知路径不改动。

### internal/architecture

只有一个测试文件 `architecture_test.go`（242），没有非测试文件。它用 go/parser 解析 cmd、internal、sdk 下所有非测试 .go 文件（跳过 testdata 和点开头目录），不编译也不执行。建议在读完分层依赖图之后、读具体包之前先读它，它就是本项目分层的"可执行定义"。

- `TestProductionArchitecture` 检查三类规则：
  1. `ownedImports` 规定包的唯一导入方：internal/app 只能被 internal/cli 导入，internal/cli 只能被 cmd/local-agent 导入，sqlitestore 只能被 internal/app 导入。database/sql 和 modernc.org/sqlite 只能被 sqlitestore、sessionregistry、workspacedomain 导入。eino/adk 只能被 agentruntime 导入，eino/compose 只能被 agentruntime 和 einoadapter 导入，eino-ext 只能被 modelgateway 导入，wazero 只能被 plugin/wasm 导入。
  2. `domainDependencies` 是领域包的项目内依赖白名单，子目录继承父目录角色，按最长前缀匹配。例如 session、strategy、tokenizer、modelcontext 不依赖任何项目包；sqlitestore 只能依赖列出的持久化端口；evolutiontools 不得依赖 Eino。
  3. `constructors` 规定构造函数的调用位置：adk.NewRunner 和 NewChatModelAgent 只能在 agentruntime/eino_turn.go；sqlitestore.Open 只能在 app/open.go 和 app/maintenance.go；workspacedomain.Open 和 wasm.NewRuntime 只能在 app/open.go；tool.NewPipeline 只能在 app/open.go 和 agentruntime/strategy_shadow.go。检查的是 selector 解析后的限定名，所以别名导入和把函数当值使用都会被发现；dot import 直接判为违规。
- `TestArchitectureRulesDetectBoundaryViolations`：用正反例表验证规则本身，包括别名导入和 dot import 绕过构造约束的用例。
- 审计意义：这些规则把"只有 app 打开数据库、WASM 和工作区域，只有 cli 能组装 app"固定为静态约束。ownedImports 只限制谁能导入 app 和 cli，不限制它们自己导入什么；这两个包不在 domainDependencies 中，可以依赖任意项目包。

### 入口、App 与 TUI 端到端调用链

(a) 进程启动
1. cmd/local-agent/main.go `main`：signal.NotifyContext(SIGINT) → cli/runner.go `Run` → `runWithDependencies(productionDependencies)` → chat.go `runChat` → `runChatWithPrompt`。
2. chat_options.go `profileArgument` → config/load.go `Load`（内置默认 → 用户配置 → profile → 项目 .gsea → 环境变量 → validate）→ `parseChatOptions`。
3. 显式 --session 时：sessionregistry.Open + Get，UpdateState(running)，defer 写 stopped。
4. 读取 prompt-file，判定 interactive，`validateEvaluationWorkspace`；放弃恢复请求则调 app.AbandonRecovery 后返回。
5. toolbridge.Load。
6. app/open.go `OpenWithOptions` 的组件创建顺序：
   1. config.Resolve
   2. runtimeowner.Acquire
   3. sqlitestore.Open
   4. evolution context
   5. approval.NewBroker
   6. events 与 checkpoints 存储
   7. wasm.NewRuntime
   8. codingtools.NewWithSandboxMode
   9. workspacedomain.Open 与 workspace 的 Attach* 调用
   10. LSP manager（启用时）
   11. WorktreeManager
   12. Coordinator
   13. artifact.Store
   14. snapshot views 与 SnapshotStore（带 barrier）
   15. buildPolicy
   16. tool.NewRegistry
   17. nativeSnapshotScope
   18. agent profiles
   19. &App{}
   20. 绑定 builtin、plugin/strategy evolution、ToolBindings、agentflow 控制、goal，然后 ActivateMany
   21. tool.NewPipeline
   22. agentflow.NewRuntime
   23. Catalog
   24. plugin.OpenManager
   25. ConfigurePersistence
   26. PublicationService
   27. pluginbuild.NewService
   28. strategy.NewShadowService
7. defer app.Close，按需 application.Recoveries。
8. deps.newModel → EnsureRootThread → selectChatApprover → NewAgentFactory(rootRuntimeConfig) → NewMergeExecutor → NewSupervisor（agentmanager.NewSupervisor → ActivateManyWithCommit → Recover）→ root_conversation.go `openRootConversation`。
9. 交互模式进入 tui/run.go `Run`；否则进入 chat_headless.go 的 runHeadlessResume / runHeadlessPrompt / runHeadlessChat。

(b) TUI 中按 Enter 到流式渲染结束
1. model.go `Update(KeyMsg)` → keys.go `handleKey` → `submit`；commands.go `handleCommand` 判定不是斜杠命令后，追加用户块、resetWakeBudget、ContextUsageFor。
2. `startOperation` 用 tea.Batch 同时启动：messages.go `runTurnCommand` → `runOperationCommand`（在 Cmd goroutine 中调用 Conversation.RunTurn(turnContext, input, sink)，实现见 conversation 调用链 (a)）、`waitForTurnEvent`、spinner.Tick。
3. 事件经 sink 写入 turnEvents，由 waitForTurnEvent 取出成为 runtimeEventMsg → turn_state.go `applyRuntimeEvent` → transcript.go `refreshTranscript` → `renderBlock` → markdown.go `render` 与 path_links.go `linkFilePaths` → viewport.SetContent/GotoBottom；Update 返回下一个 waitForTurnEvent。
4. view.go `View` 渲染，cursor.go `cursorWriter` 写出并定位光标。
5. 审批：Init 启动的唯一 waitForApproval 收到 approvalRequestMsg → applyApprovalRequest；用户按 Enter → resolveApproval → broker.Resolve。
6. RunTurn 返回后 turnDoneMsg 经同一 channel 到达 → `finishTurn`（清理、Final 兜底、usage）→ refreshTranscript(true) → input.Focus，不再启动 waitForTurnEvent。

(c) 关闭顺序
1. tui.Run 或 headless 返回。
2. app/close.go `Close`（closeOnce）：持锁设置 closed 并调 evolutionCancel → joinEvolution（最多 10s，超时保留资源并返回）→ supervisor.Close（超时同样返回）→ 依次关闭 plugins → LSP diagnostics → approvalBroker → wasm → database → workspacedomain → runtimeowner，错误用 errors.Join 汇总。
3. chat.go 剩余 defer：UpdateState(stopped, 0)，catalog.Close。
4. runner 把结果映射为退出码，main 调 os.Exit。
- Registry、pipeline、coordinator 没有显式 Close，是否需要未确认。

(d) mailbox 有消息时的后台 wake
1. Init 启动 background.go `scheduleBackgroundCheck`，2s 后发 backgroundTickMsg。
2. `checkBackground` 先检查 `wakeAllowed`：background 非 nil、不 busy、没有待决审批、没有 wakeSuppressed、连续 wake 少于 3 次、输入框为空、距上次 wake 超过 5s。不满足则重新排程。
3. 满足则调 PendingBackground（超时 2s）→ backgroundPendingMsg → `handleBackgroundPending` 再检查 wakeAllowed → `startWake(count)`：设置 lastWake、consecutiveWakes+1；追加 blockBackground 块 "N Agent messages arrived"；startOperation("Reading Agent results") 运行 RunBackgroundTurn，backgroundRunning=true，触发 startActivityRefresh。任何时刻只有一条检查链。
4. 事件流同 (b)。finishTurn → `finishWake`：ErrNoBackgroundInput 或 ErrBackgroundBudgetExhausted 时删掉提示块；wake 回合不增加 completedTurns。
5. 用户输入经 resetWakeBudget 恢复预算；Ctrl+C 取消后置 wakeSuppressed，直到用户再次输入。mailbox 绑定与回合后 ack 见 cli/root_conversation.go 及其测试。

入口/App/TUI 审计要点（按优先级）：
1. 项目 .gsea/config.toml 可能来自不可信仓库，却能设置 lsp.command/args、model.base_url 加 api_key_environment、agent.instruction 和数据目录；config 包里没有信任门槛。
2. control 子命令的 merge.run 用不带 approver 的默认 MergeExecutor，是否绕过人工审批未确认；control 还丢弃 Close 的错误。
3. main 只处理 SIGINT。SIGTERM 时不执行 app.Close，Session 停在 running。
4. profileArgument 不识别 -profile 和 --p；eval 的 hasCLIFlag 不识别单横线 flag。
5. TUI 不剥离模型和工具输出中的控制序列；渲染时对模型给出的路径执行 os.Stat。
6. 取消回合时事件缓冲已满会丢 turnDoneMsg，界面停在 busy。
7. artifact_read 整个读入内存再切片；大段粘贴时输入换行计算约 O(n²)。
8. cmd/local-agent/approval-test.txt 是与代码无关的残留文件。

## 审计发现汇总（历史工作单）

以下保留原 A–E 编号和发现，便于追踪修复来源。**这些是修复前的静态阅读记录。** 当前状态请逐项对照 [A–E 审计复核与修复记录](AUDIT_REMEDIATION.md)，该记录包含安全与崩溃一致性修复、原判断不成立的项目，以及尚未完成的 C.3/C.4、编译硬内存配额、原生 Git 合并 journal 和 Team 取消生命周期等边界。

### A. 安全边界（优先核实）

1. **项目配置没有信任门槛**（internal/config/load.go）。工作区里的 `.gsea/config.toml` 可能来自不可信仓库，却能设置 `lsp.command/args`（会被执行）、`model.base_url` 加 `api_key_environment`（可把凭据发到任意地址）、`agent.instruction` 和数据目录。
2. **control 子命令的 merge.run 不带 approver**（internal/cli/control.go）。`NewSupervisor(agentmanager.Config{})` 由 app 默认值填入不带 approver 的 MergeExecutor，是否绕过 merge_apply 人工审批未确认。
3. **tool.Pipeline 不按 JSON schema 校验参数**（internal/tool/pipeline.go `validateCall` 只做 json.Valid）。schema 中的上限只靠执行器自查，例如 codingtools write 的 mode 只在 schema 里限定 0–511。
4. **resolveParentAt 不解析叶子**（internal/codingtools/paths.go）。叶子是指向 root 外的 symlink 时，write.go 的 Stat、readDiffFile、verifyExpectedSHA256 可能读到 root 外内容并经 diff 回显给模型（未确认）。
5. **evaluation 在宿主上对 agent 可写的 worktree 运行 git**（internal/evaluation/workspace.go `Patch`）。不经沙箱且继承完整环境，改写 .git/config 后可能借 core.fsmonitor 等配置在宿主执行命令（未确认）。
6. **gopls 不经沙箱且继承完整宿主环境**（internal/lsp/client.go），send 没有写超时。
7. **沙箱环境变量按子串过滤**（internal/sandbox/invocation.go）。DATABASE_URL 这类内嵌凭据的变量会漏过；Seatbelt profile 全局允许 file-read-metadata；bwrap 没有 unshare user/uts/cgroup；setsid 的子孙能离开进程组，由 DescendantLock 补救。
8. **pluginbuild 只哈希 bin/go**（internal/pluginbuild/compiler.go）。`go version` 在沙箱外运行，go 路径受 GOROOT 环境变量影响；ulimit -f 的块大小未确认；没有内存硬上限。
9. **TUI 不剥离控制序列**（internal/tui/transcript.go）。模型和工具输出中的 ESC/OSC 序列可能直达终端（上游是否清洗未确认）；path_links.go 在渲染时对模型给出的任意绝对路径执行 os.Stat。
10. **策略拒绝与审批拒绝处理不一致**（internal/einoadapter/tool.go `InvokableRun`）。审批拒绝作为结果交回模型，策略拒绝（ErrPolicyDenied）作为 error 中止整个 turn；CallID 为空时随机生成，削弱重放幂等。
11. **policy 的 ResourcePrefix 没有路径边界**（internal/policy）。"/workspace" 也会匹配 "/workspace2"；目前流水线固定传 Resource="tool:"+name，暂时没有被利用。
12. **artifact 的 TOCTOU**（internal/artifact）。resolve 与随后 open/rename 之间 symlink 仍可能被替换；读取时不重新校验哈希；ReadArtifact 接受任意本地相对路径；父目录没有 fsync。
13. **wasm 资源上限**（internal/plugin/wasm/runtime.go）。64MiB 只是单实例上限，并发调用没有总量限制；guest 错误不截断；guest SDK 从不调用 host_blob_drop。
14. **ConstrainDelegation 的收窄不完整**（internal/agentprofile）。不限制"父 shared-lease、子 git-worktree"组合，也不和父 Agent 的 Limits 取交集（别处是否有约束未确认）；WorkspaceMode 没有 orchestration 里的 "snapshot"。

### B. 崩溃一致性与正确性

1. **turn.completed 的 Append 失败时不调 t.fail**（internal/agentruntime/turn_stream.go）。不写 turn.failed，checkpoint 也不删，之后该 turn 会被当作开放 turn 恢复；删 checkpoint 和 mailbox.Acknowledge 的错误被忽略。
2. **turn_durability 的 fail 是两次独立 Append**（internal/agentruntime/turn_durability.go）。turn.failed 与 checkpoint.created 之间崩溃会留下不一致记录。
3. **main 只处理 SIGINT**（cmd/local-agent/main.go）。SIGTERM 或 SIGKILL 后不执行 app.Close，sessionregistry 中的 Session 停在 running；cli session remove 会拒绝 running 状态，是否有 PID 存活检查未确认。
4. **合并路径的崩溃窗口**（internal/workspacecoord/merge_snapshot.go、merge.go）。Applied 与 completeTask 之间、Conflicted 与 CreateTask 之间崩溃时如何收敛未确认。merge.go 在审批期间持有 mutex，Discard 会被阻塞。
5. **sqlitestore 的版本守卫 UPDATE 不检查 RowsAffected**（TransitionAgent、TransitionTask、claimTaskForAgentTx、TransitionMerge、FinishGoal 等）。单连接加事务内先读后写目前能保证正确，改成多连接后会静默丢失冲突。
6. **sqlitestore 有多步、非事务操作**：CancelWaitSetsForAgent（可能只完成一部分）、BindSessionAgent、FinishPluginBuild、SavePluginTest、Strategies 的 Save/SaveSuite/SaveReport（先插入再回读）、FindSpawn。
7. **迁移 checksum 只钉住 v1–v34**（sqlitestore/testdata/migrations.sha256）。改 v35/v36 的 DDL 不会被 TestPublishedMigrationContract 发现。
8. **TUI 取消回合时可能丢 turnDoneMsg**（internal/tui/messages.go）。256 容量的事件缓冲恰好已满时，界面停在 busy（理论边界）。
9. **退出码 130 未确认**（internal/tui/run.go）。ctx 取消时 program.Run 的错误被包装为 "run terminal UI"，是否仍满足 errors.Is(context.Canceled) 未确认。
10. **tokenizer 对 codex 模型用错编码**（internal/tokenizer/tokenizer.go `encodingForModel`）。模型名含 "codex" 就用 p50k_base，"gpt-5-codex" 会算错并在有本地 BPE 缓存时报告 Exact；ObserveProviderInput 与首次加载本地 tokenizer 的先后顺序决定最终 Status。
11. **strategy Compare 与注释不符**（internal/strategy/shadow.go）。注释说取消或失败的比较也是证据，代码在 comparator 出错时直接返回，不保存报告。
12. **Stream 与 Generate 的记账顺序不同**（internal/agentruntime/model.go）。Stream 先 recordAssistantMessage 后 observeProviderUsage，Generate 相反，影响未确认。
13. **tokenusage 可能重复计费 reasoning**（CostMicros 单独计 reasoning，供应商 output 已含 reasoning 时会重复，未确认）；Reserve 用 `>`，budgetExceeded 和 CanAdmit 用 `>=`。
14. **agentgoal BindTurn 无锁写 turnID**（实际竞态可能性低，但没有同步保证）。
15. **[已修复 2026-10-26] 生产策略拒绝 task_verify**（internal/app/builtins.go）。原问题：`DefaultDeny` 规则表缺 task_verify 的 allow，reviewer 调用会被拒绝，验证 Gate 在生产装配下走不通。修复：新增 `allow-task-verify` 规则，结论是否被接受仍由 Supervisor 按 reviewer 身份与 Task 状态判断；app 测试覆盖真实策略。
16. **[已修复 2026-10-26] git-worktree 的合并没有生产入队路径**（internal/agentmanager/supervisor_delivery.go）。原问题：唯一的 EnqueueMerge 调用者只写 `MergeSnapshot`，git-patch 路径无人触发。修复：`deliverPatch` 在 worktree 子 Agent 成功结束时存 patch artifact 并入队 git-patch merge（取代同一 view 未处理的旧 merge），Task 进入 verification/awaiting_merge；后台合并对 git-patch 使用与 snapshot 相同的门禁（等 Task、审批或 auto_merge）。
17. **[已修复 2026-10-26] 其他由文档审计带出的实现缺口**：
    - 中断级联：`Interrupt` 经 `interruptChildren` 递归中断全部非 terminal 后代。
    - 预算与价格：`[usage.prices."provider/model"]` 装配 Pricing；`[usage.budget]` 在每次 `EnsureRootThread` 时经 ConfigureBudget 写入，全 0 清除预算。
    - Team 状态：`ValidateTeamTransition` + `sqlitestore/teams.go` 的 `TransitionTeam`；`Supervisor.CancelTeam`（control `team.cancel`）把 Team 置为 canceling → 中断子 Agent → canceled；`CreateChild` 拒绝非 active Team（`ErrTeamNotActive`）；会话恢复时 Team 重开（`team.reopened`）。draining/completed/failed 仍无生产调用方。
    - control events：Team 过滤在 SQL LIMIT 之前，`result` 行返回 `count` 与 `next_cursor`。
    - 合并后验证：`[agents].post_merge_check`（`MergeVerifier`，默认超时 10m）在 merge applied 后运行，失败使 Task `failed`，变更不回滚；未配置时行为不变。

### C. 资源与性能

1. 无界读取：codingtools read 的 fileSHA256 与 edit 的 ReadFile；artifact_read 整个读入再切片；toolbridge Load 先整读 manifest 再判断 4MiB；lsp 头部行 ReadString 不限长；render 在无换行的 fence 前缀流上 pending 无限增长。
2. tui/input_selection.go `wrapInputLine` 每个逻辑行新建 textarea 逐列探测，粘贴大段文本约 O(n²)。
3. tool.Registry 的 snapshots 与 agentprofile.Registry 的历史 revision 永久保存；tool.SnapshotByID 在注册表锁内对所有快照算哈希。
4. sqlitestore 的 plugin_collection_plan 与 retention_plan 全表扫描 runtime_snapshots、session_events、flow_runs；workspacecoord/preservation.go 的 CAS 内部是 O(n) 扫描。

### D. 错误语义与一致性

1. sqlitestore 把原始 sql.ErrNoRows 漏到包外（StageSettlement、flows、goals 的 StartRound/FinishRound、PluginBuild、Strategies 等），并有大量非哨兵 errors.New，调用方无法用 errors.Is 区分。
2. 幂等 hash 的旧数据放行规则不一致：spawn/task/wait/view/merge/reservation 把空 request_hash 当匹配，flow_runs 严格比较，mailbox 逐字段比较；SavePluginTest 没有 ON CONFLICT。
3. sessionregistry 的 UpdateState/Remove 找不到记录时不包装 ErrNotFound；INSERT 失败时资源目录残留；没有 nil 保护与状态迁移校验。
4. scheduler 的 WeightedRoundRobin 实际没有权重；Release 重复释放被静默吞掉。
5. 流水线 evaluate/authorize 阶段写审计记录时不用 WithoutCancel，ctx 取消会让 policy.decided 或 approval.* 记录缺失。
6. CLI 参数解析缺口：profileArgument 不识别 -profile 与 --p；eval 的 hasCLIFlag 只认 `--name`，单横线 flag 能绕过拒绝。control 丢弃 Close 错误。
7. cmd/local-agent/approval-test.txt 是与代码无关的残留文件；workspace_leases（v11/v12）是没有读写代码的遗留表。

### E. 需要专门核实的开放问题

- 父 Agent 通过 durable mailbox 读取 `MessageResult`；终态转换与首次结果入队同事务，恢复时只需补投递/结算未完成的 completion receipt。
- reviewer 视图的只读由谁强制（codingtools 的 ensureWritable 拒绝 ViewSharedReadonly，需确认路由一定命中）。
- human_confirm 背后真正的门（policy 规则）。
- Blocked→Ready 与 supersede 在 sqlitestore 中的实现细节。
- `LeaseSpec.Validate` 校验 trim 后的路径，存储层是否也用 trim 后的值。
- Team/Approval/Lease 状态没有迁移表，sqlitestore 是否校验。
- Registry、pipeline、coordinator 没有显式 Close，是否需要。
- 生产环境 deps.evalVerifier 是否为 nil；CLI 是否调用 ApplyHarborProtocolMetadata、ApplyTerminalBenchAudit 等计分函数。

基础包需要守住的不变量（callCoordinator 只在进程内去重、OutcomeUnknown 的重放规则、ActivateManyWithCommit 的钩子约束、approval.Broker 关闭语义等）见第 1 阶段末尾的"基础包审计发现汇总"。

## 附录 A：非 Go 目录

这些目录不在 Go 源码统计内，读到相关阶段时再看：
- `evals/`：外部基准的 Python 适配层，配合第 4 阶段的 internal/evaluation 与 cli/eval.go 读。
  - `README.md`（391）：`gsea.eval.task.v1` 任务协议与结果协议，先读。
  - `adapters/gsea_client.py`（194）：调用 local-agent 的公共客户端；`harbor_gsea.py`（522）、`terminal_bench_gsea.py`（170）、`agentdojo_gsea.py`（304）、`inspect_gsea.py`（69）、`prepare_swebench.py`（152）分别对接 Harbor/Terminal-Bench、AgentDojo、Inspect 与 SWE-bench。
  - `run_agentdojo.py`（104）、`inspect_local_smoke.py`（17）是运行入口；`tests/test_harbor_gsea.py`（151）锁定 Harbor 适配行为；`examples/tasks.jsonl` 是任务样例；`requirements/` 是各 harness 的依赖。
- `examples/evolution/`：`echo.json`（插件样例）、`strategy.json` 与 `shadow-suite.json`（策略与影子评测套件样例），配合第 4 阶段插件自进化链和第 1 阶段 strategy 读。
- `research/`（notes、papers）：调研资料，不影响代码理解。
- `logs/run_evaluation/`：评测运行产物，不需要读。
- `internal/plugin/wasm/testdata/echo/main.go`（23）与 `internal/session/sqlitestore/testdata/`：测试夹具，已在对应包小节说明。

## 附录 B：全部文件清单（行数来自 wc -l）

用法：读完一个文件就勾掉一项。`[T]` 表示测试文件。目录按字母序排列，阅读顺序以正文为准。

#### `cmd/local-agent`
- [ ] `main.go`（15）

#### `internal/agentflow`
- [ ] `catalog.go`（199）
- [ ] `operators.go`（635）
- [ ] `runtime.go`（345）
- [ ] `tools.go`（94）
- [ ] `types.go`（238）
- [ ] [T] `runtime_test.go`（572）
- [ ] [T] `snapshot_recovery_test.go`（136）

#### `internal/agentgoal`
- [ ] `activation.go`（17）
- [ ] `admission.go`（119）
- [ ] `admission_guard.go`（78）
- [ ] `evaluator.go`（117）
- [ ] `tool.go`（55）
- [ ] `types.go`（295）
- [ ] `verification.go`（65）
- [ ] [T] `activation_test.go`（22）
- [ ] [T] `admission_guard_test.go`（93）
- [ ] [T] `admission_test.go`（56）
- [ ] [T] `evaluator_test.go`（40）
- [ ] [T] `verification_test.go`（78）

#### `internal/agentmanager`
- [ ] `activity.go`（106）
- [ ] `agent_mailbox.go`（97）
- [ ] `profile.go`（49）
- [ ] `supervisor.go`（693）
- [ ] `supervisor_assignment.go`（234）
- [ ] `supervisor_delivery.go`（184）
- [ ] `supervisor_mailbox.go`（68）
- [ ] `supervisor_merge.go`（84）
- [ ] `supervisor_query.go`（89）
- [ ] `supervisor_recovery.go`（129）
- [ ] `supervisor_run.go`（272）
- [ ] `supervisor_scheduler.go`（153）
- [ ] `supervisor_tasks.go`（278）
- [ ] `supervisor_wait.go`（211）
- [ ] `supervisor_workspace.go`（329）
- [ ] `team_cancel.go`（75）
- [ ] `tools.go`（265）
- [ ] `tools_bindings.go`（70）
- [ ] `tools_merge.go`（97）
- [ ] `tools_task.go`（332）
- [ ] [T] `activity_test.go`（154）
- [ ] [T] `profile_test.go`（244）
- [ ] [T] `supervisor_cancellation_test.go`（76）
- [ ] [T] `supervisor_delivery_test.go`（463）
- [ ] [T] `supervisor_lifecycle_test.go`（306）
- [ ] [T] `supervisor_test.go`（1245）

#### `internal/agentprofile`
- [ ] `profile.go`（205）
- [ ] `snapshot.go`（111）
- [ ] [T] `profile_test.go`（43）
- [ ] [T] `snapshot_test.go`（120）

#### `internal/agentruntime`
- [ ] `background.go`（271）
- [ ] `compaction.go`（89）
- [ ] `compaction_transcript.go`（152）
- [ ] `completed_turn.go`（61）
- [ ] `config.go`（49）
- [ ] `context_artifacts.go`（55）
- [ ] `context_assessment.go`（57）
- [ ] `context_count.go`（65）
- [ ] `context_events.go`（47）
- [ ] `context_policy.go`（152）
- [ ] `context_summary.go`（157）
- [ ] `context_summary_format.go`（116）
- [ ] `context_usage.go`（104）
- [ ] `eino_turn.go`（101）
- [ ] `events.go`（53）
- [ ] `factory.go`（134）
- [ ] `factory_snapshot.go`（113）
- [ ] `factory_turn.go`（121）
- [ ] `factory_views.go`（84）
- [ ] `goal_evaluator.go`（160）
- [ ] `goal_verifier.go`（87）
- [ ] `history.go`（272）
- [ ] `ids.go`（21）
- [ ] `model.go`（314）
- [ ] `model_accounting.go`（99）
- [ ] `model_goal_tools.go`（36）
- [ ] `profile.go`（307）
- [ ] `recovery.go`（81）
- [ ] `request_context.go`（262）
- [ ] `runtime.go`（5）
- [ ] `strategy.go`（94）
- [ ] `strategy_shadow.go`（107）
- [ ] `turn.go`（177）
- [ ] `turn_durability.go`（49）
- [ ] `turn_modes.go`（177）
- [ ] `turn_stream.go`（162）
- [ ] [T] `background_test.go`（130）
- [ ] [T] `compaction_checkpoint_test.go`（120）
- [ ] [T] `compaction_safety_test.go`（98）
- [ ] [T] `compaction_test.go`（315）
- [ ] [T] `context_assessment_test.go`（205）
- [ ] [T] `context_policy_test.go`（50）
- [ ] [T] `context_summary_format_test.go`（170）
- [ ] [T] `durable_accounting_test.go`（172）
- [ ] [T] `goal_evaluator_test.go`（73）
- [ ] [T] `history_transcript_test.go`（54）
- [ ] [T] `model_accounting_test.go`（272）
- [ ] [T] `model_test.go`（50）
- [ ] [T] `profile_flow_test.go`（84）
- [ ] [T] `profile_test.go`（284）
- [ ] [T] `recovery_abandonment_test.go`（71）
- [ ] [T] `request_context_test.go`（230）
- [ ] [T] `responses_failure_test.go`（56）
- [ ] [T] `runtime_test.go`（327）
- [ ] [T] `snapshot_admission_test.go`（51）
- [ ] [T] `strategy_test.go`（164）
- [ ] [T] `turn_durability_test.go`（77）
- [ ] [T] `turn_modes_test.go`（123）

#### `internal/app`
- [ ] `agent_factory.go`（83）
- [ ] `app.go`（74）
- [ ] `artifacts.go`（73）
- [ ] `builtins.go`（77）
- [ ] `close.go`（96）
- [ ] `doctor.go`（147）
- [ ] `evolution_lifecycle.go`（34）
- [ ] `evolution_scope.go`（38）
- [ ] `maintenance.go`（75）
- [ ] `open.go`（264）
- [ ] `orchestration.go`（156）
- [ ] `plugin_build.go`（22）
- [ ] `plugin_tools.go`（10）
- [ ] `plugin_verification.go`（18）
- [ ] `profiles.go`（57）
- [ ] `recovery.go`（30）
- [ ] `resources.go`（129）
- [ ] `snapshot_merge.go`（110）
- [ ] `snapshot_scope.go`（70）
- [ ] `strategy.go`（26）
- [ ] `strategy_tools.go`（10）
- [ ] `usage.go`（26）
- [ ] [T] `agent_factory_test.go`（23）
- [ ] [T] `app_test.go`（210）
- [ ] [T] `artifacts_test.go`（38）
- [ ] [T] `builtins_test.go`（36）
- [ ] [T] `close_test.go`（283）
- [ ] [T] `evolution_scope_test.go`（55）
- [ ] [T] `ownership_test.go`（51）
- [ ] [T] `plugin_evolution_test.go`（104）
- [ ] [T] `plugin_loop_test.go`（179）
- [ ] [T] `recovery_abandonment_test.go`（56）
- [ ] [T] `responses_workspace_test.go`（108）
- [ ] [T] `snapshot_merge_test.go`（86）
- [ ] [T] `snapshot_scope_test.go`（30）
- [ ] [T] `strategy_test.go`（220）
- [ ] [T] `usage_test.go`（82）
- [ ] [T] `workspace_domain_test.go`（86）

#### `internal/approval`
- [ ] `allowlist.go`（45）
- [ ] `broker.go`（198）
- [ ] [T] `allowlist_test.go`（48）
- [ ] [T] `broker_test.go`（140）

#### `internal/architecture`
- [ ] [T] `architecture_test.go`（242）

#### `internal/artifact`
- [ ] `store.go`（240）
- [ ] [T] `store_test.go`（46）

#### `internal/cli`
- [ ] `chat.go`（469）
- [ ] `chat_headless.go`（257）
- [ ] `chat_options.go`（271）
- [ ] `chat_output.go`（102）
- [ ] `control.go`（229）
- [ ] `conversation_builder.go`（227）
- [ ] `doc.go`（4）
- [ ] `doctor.go`（61）
- [ ] `eval.go`（594）
- [ ] `maintenance.go`（40）
- [ ] `maintenance_options.go`（67）
- [ ] `plugin_json.go`（52）
- [ ] `plugins.go`（100）
- [ ] `plugins_input.go`（44）
- [ ] `plugins_options.go`（126）
- [ ] `recovery_abandonment.go`（12）
- [ ] `root_conversation.go`（28）
- [ ] `runner.go`（142）
- [ ] `runtime_config.go`（22）
- [ ] `session.go`（191）
- [ ] `strategy.go`（94）
- [ ] `strategy_options.go`（127）
- [ ] [T] `chat_approval_test.go`（109）
- [ ] [T] `chat_options_test.go`（278）
- [ ] [T] `child_recovery_test.go`（158）
- [ ] [T] `control_test.go`（54）
- [ ] [T] `conversation_builder_test.go`（78）
- [ ] [T] `eval_test.go`（252）
- [ ] [T] `eval_usage_test.go`（45）
- [ ] [T] `evolution_examples_test.go`（32）
- [ ] [T] `evolution_options_test.go`（157）
- [ ] [T] `evolution_test.go`（123）
- [ ] [T] `goal_output_test.go`（51）
- [ ] [T] `main_test.go`（39）
- [ ] [T] `plugins_input_test.go`（72）
- [ ] [T] `recovery_abandonment_test.go`（98）
- [ ] [T] `recovery_test.go`（129）
- [ ] [T] `root_conversation_test.go`（255）
- [ ] [T] `runner_test.go`（344）

#### `internal/codingtools`
- [ ] `catalog.go`（72）
- [ ] `changes.go`（252）
- [ ] `diagnostics.go`（89）
- [ ] `diff.go`（158）
- [ ] `helpers.go`（57）
- [ ] `limits.go`（17）
- [ ] `paths.go`（77）
- [ ] `read.go`（119）
- [ ] `search.go`（289）
- [ ] `shell.go`（150）
- [ ] `workspace.go`（306）
- [ ] `write.go`（214）
- [ ] [T] `diff_test.go`（52）
- [ ] [T] `snapshot_view_test.go`（109）
- [ ] [T] `workspace_test.go`（609）

#### `internal/config`
- [ ] `config.go`（168）
- [ ] `load.go`（804）
- [ ] [T] `config_test.go`（447）
- [ ] [T] `context_test.go`（71）

#### `internal/conversation`
- [ ] `admission.go`（135）
- [ ] `background.go`（65）
- [ ] `candidate_events.go`（43）
- [ ] `context.go`（47）
- [ ] `conversation.go`（48）
- [ ] `execution.go`（66）
- [ ] `goal_loop.go`（127）
- [ ] `goal_outcome.go`（79）
- [ ] `goal_protocol.go`（26）
- [ ] `goal_recovery.go`（61）
- [ ] `goal_transition.go`（51）
- [ ] `history.go`（61）
- [ ] `recovery.go`（141）
- [ ] `resume.go`（58）
- [ ] `run.go`（45）
- [ ] [T] `admission_failure_test.go`（143）
- [ ] [T] `admission_test.go`（289）
- [ ] [T] `conversation_test.go`（76）
- [ ] [T] `direct_stream_test.go`（61）
- [ ] [T] `execution_test.go`（142）
- [ ] [T] `goal_recovery_test.go`（207）
- [ ] [T] `goal_test.go`（294）
- [ ] [T] `goal_transition_test.go`（161）
- [ ] [T] `recovery_abandonment_test.go`（60）
- [ ] [T] `recovery_test.go`（123）

#### `internal/einoadapter`
- [ ] `tool.go`（147）
- [ ] [T] `tool_test.go`（183）

#### `internal/evaluation`
- [ ] `capabilities.go`（132）
- [ ] `export.go`（139）
- [ ] `manifest.go`（106）
- [ ] `official_results.go`（603）
- [ ] `runner.go`（202）
- [ ] `score.go`（270）
- [ ] `types.go`（237）
- [ ] `verifier.go`（140）
- [ ] `workspace.go`（355）
- [ ] [T] `manifest_test.go`（125）
- [ ] [T] `runner_test.go`（232）
- [ ] [T] `score_test.go`（313）

#### `internal/evolutiontools`
- [ ] `arguments.go`（25）
- [ ] `plugin.go`（70）
- [ ] `plugin_bindings.go`（46）
- [ ] `ports.go`（46）
- [ ] `strategy.go`（69）
- [ ] `strategy_bindings.go`（56）
- [ ] `strategy_scope.go`（62）
- [ ] [T] `bindings_test.go`（250）
- [ ] [T] `strategy_test.go`（170）

#### `internal/lsp`
- [ ] `client.go`（461）
- [ ] `manager.go`（559）
- [ ] `protocol.go`（118）
- [ ] `types.go`（141）
- [ ] [T] `manager_test.go`（262）

#### `internal/modelcontext`
- [ ] `limits.go`（151）
- [ ] [T] `limits_test.go`（122）

#### `internal/modelgateway`
- [ ] `cache_usage_transport.go`（136）
- [ ] `context_metadata.go`（150）
- [ ] `gateway.go`（176）
- [ ] `responses.go`（425）
- [ ] `responses_stream.go`（299）
- [ ] `responses_transport.go`（70）
- [ ] `responses_types.go`（154）
- [ ] [T] `context_metadata_test.go`（99）
- [ ] [T] `gateway_test.go`（466）
- [ ] [T] `responses_arguments_test.go`（96）
- [ ] [T] `responses_terminal_test.go`（120）
- [ ] [T] `responses_types_test.go`（30）

#### `internal/orchestration`
- [ ] `agent.go`（197）
- [ ] `event.go`（198）
- [ ] `id.go`（194）
- [ ] `lease.go`（112）
- [ ] `mailbox.go`（151）
- [ ] `root.go`（44）
- [ ] `state.go`（122）
- [ ] `task.go`（112）
- [ ] `team.go`（41）
- [ ] `wait.go`（128）
- [ ] [T] `event_test.go`（26）
- [ ] [T] `id_test.go`（47）
- [ ] [T] `lease_test.go`（26）
- [ ] [T] `state_test.go`（36）

#### `internal/plugin`
- [ ] `catalog.go`（108）
- [ ] `manager.go`（260）
- [ ] `persistence.go`（241）
- [ ] `publication.go`（66）
- [ ] `verification.go`（132）
- [ ] [T] `catalog_test.go`（52）
- [ ] [T] `manager_integration_test.go`（121）
- [ ] [T] `persistence_integration_test.go`（432）
- [ ] [T] `publication_test.go`（142）
- [ ] [T] `snapshot_recovery_integration_test.go`（116）

#### `internal/plugin/wasm`
- [ ] `blobstore.go`（161）
- [ ] `runtime.go`（331）
- [ ] [T] `blobstore_test.go`（44）
- [ ] [T] `runtime_integration_test.go`（84）

#### `internal/plugin/wasm/testdata/echo`
- [ ] `main.go`（23）

#### `internal/pluginbuild`
- [ ] `compiler.go`（147）
- [ ] `service.go`（126）
- [ ] `source.go`（59）
- [ ] `types.go`（80）
- [ ] [T] `compiler_test.go`（54）
- [ ] [T] `service_test.go`（267）

#### `internal/policy`
- [ ] `policy.go`（137）
- [ ] [T] `policy_test.go`（77）

#### `internal/render`
- [ ] `doc.go`（3）
- [ ] `markdown_stream.go`（315）
- [ ] [T] `markdown_stream_test.go`（119）

#### `internal/retention`
- [ ] `collection.go`（31）
- [ ] `retention.go`（48）

#### `internal/runtimeowner`
- [ ] `lock.go`（82）
- [ ] `lock_other.go`（12）
- [ ] `lock_unix.go`（17）
- [ ] [T] `lock_test.go`（149）

#### `internal/sandbox`
- [ ] `invocation.go`（162）
- [ ] `process_other.go`（7）
- [ ] `process_unix.go`（33）
- [ ] `sandbox.go`（288）
- [ ] [T] `isolation_test.go`（176）
- [ ] [T] `process_unix_test.go`（27）
- [ ] [T] `sandbox_test.go`（90）

#### `internal/scheduler`
- [ ] `scheduler.go`（112）
- [ ] [T] `scheduler_test.go`（31）

#### `internal/session`
- [ ] `checkpoint.go`（12）
- [ ] `event.go`（111）
- [ ] `payload.go`（24）
- [ ] `recovery.go`（78）
- [ ] `visit.go`（37）
- [ ] [T] `recovery_test.go`（39）
- [ ] [T] `visit_test.go`（54）

#### `internal/session/memorystore`
- [ ] `store.go`（171）

#### `internal/session/sqlitestore`
- [ ] `agents_query.go`（125）
- [ ] `agents_spawn.go`（223）
- [ ] `agents_state.go`（119）
- [ ] `checkpoints.go`（66）
- [ ] `codec.go`（67）
- [ ] `compact.go`（29）
- [ ] `events.go`（203）
- [ ] `flows.go`（423）
- [ ] `goal_recovery.go`（40）
- [ ] `goals.go`（396）
- [ ] `lsp.go`（83）
- [ ] `mailbox.go`（290）
- [ ] `mailbox_ledger.go`（153）
- [ ] `maintenance_dependencies.go`（96）
- [ ] `maintenance_receipts.go`（36）
- [ ] `maintenance_references.go`（39）
- [ ] `maintenance_turns.go`（128）
- [ ] `merge_apply.go`（139）
- [ ] `migrations.go`（93）
- [ ] `orchestration.go`（168）
- [ ] `orchestration_events.go`（271）
- [ ] `plugin_builds.go`（114）
- [ ] `plugin_collection.go`（77）
- [ ] `plugin_collection_plan.go`（191）
- [ ] `plugin_verification.go`（39）
- [ ] `plugins.go`（157）
- [ ] `profile.go`（10）
- [ ] `query.go`（32）
- [ ] `recovery_control.go`（163）
- [ ] `retention.go`（91）
- [ ] `retention_plan.go`（126）
- [ ] `runtime_snapshots.go`（165）
- [ ] `schema.go`（607）
- [ ] `store.go`（97）
- [ ] `strategies.go`（246）
- [ ] `task_claims.go`（147）
- [ ] `task_dependencies.go`（243）
- [ ] `tasks.go`（310）
- [ ] `tasks_query.go`（118）
- [ ] `teams.go`（144）
- [ ] `usage.go`（426）
- [ ] `usage_recovery.go`（225）
- [ ] `waitsets.go`（421）
- [ ] `workspacecoord.go`（627）
- [ ] [T] `flows_identity_test.go`（56）
- [ ] [T] `goals_test.go`（143）
- [ ] [T] `lsp_test.go`（36）
- [ ] [T] `mailbox_ledger_test.go`（116）
- [ ] [T] `mailbox_test.go`（120）
- [ ] [T] `merge_apply_test.go`（172）
- [ ] [T] `merge_snapshot_test.go`（447）
- [ ] [T] `migrations_test.go`（238）
- [ ] [T] `orchestration_test.go`（219）
- [ ] [T] `plugin_builds_test.go`（51）
- [ ] [T] `plugins_test.go`（162）
- [ ] [T] `recovery_control_test.go`（335）
- [ ] [T] `retention_test.go`（296）
- [ ] [T] `runtime_snapshots_test.go`（110）
- [ ] [T] `store_test.go`（123）
- [ ] [T] `strategies_test.go`（188）
- [ ] [T] `tasks_test.go`（282）
- [ ] [T] `transaction_boundaries_test.go`（249）
- [ ] [T] `usage_recovery_test.go`（249）
- [ ] [T] `usage_test.go`（163）
- [ ] [T] `waitsets_test.go`（93）
- [ ] [T] `workspacecoord_test.go`（75）

#### `internal/sessionregistry`
- [ ] `registry.go`（231）
- [ ] [T] `registry_test.go`（66）

#### `internal/strategy`
- [ ] `catalog.go`（16）
- [ ] `shadow.go`（66）
- [ ] `types.go`（161）
- [ ] [T] `shadow_test.go`（183）

#### `internal/tokenizer`
- [ ] `tokenizer.go`（312）
- [ ] [T] `tokenizer_test.go`（61）

#### `internal/tokenusage`
- [ ] `usage.go`（230）
- [ ] [T] `usage_test.go`（39）

#### `internal/tool`
- [ ] `context.go`（53）
- [ ] `native.go`（18）
- [ ] `outcome.go`（28）
- [ ] `pipeline.go`（570）
- [ ] `registry.go`（316）
- [ ] `snapshot_identity.go`（76）
- [ ] `snapshot_manifest.go`（169）
- [ ] `snapshot_persistence.go`（117）
- [ ] `turn_guard.go`（54）
- [ ] [T] `outcome_test.go`（121）
- [ ] [T] `pipeline_test.go`（309）
- [ ] [T] `registry_publication_test.go`（76）
- [ ] [T] `registry_test.go`（97）
- [ ] [T] `snapshot_identity_test.go`（89）
- [ ] [T] `snapshot_persistence_test.go`（193）
- [ ] [T] `turn_guard_test.go`（83）

#### `internal/toolbridge`
- [ ] `bridge.go`（169）
- [ ] [T] `bridge_test.go`（75）

#### `internal/tui`
- [ ] `activity.go`（336）
- [ ] `agents_panel.go`（270）
- [ ] `background.go`（152）
- [ ] `commands.go`（159）
- [ ] `context.go`（71）
- [ ] `cursor.go`（122）
- [ ] `input_selection.go`（200）
- [ ] `keys.go`（183）
- [ ] `markdown.go`（222）
- [ ] `messages.go`（136）
- [ ] `model.go`（305）
- [ ] `path_links.go`（88）
- [ ] `recovery.go`（78）
- [ ] `run.go`（108）
- [ ] `selection.go`（150）
- [ ] `styles.go`（77）
- [ ] `tool_disclosure.go`（100）
- [ ] `tool_status.go`（37）
- [ ] `transcript.go`（583）
- [ ] `turn_state.go`（286）
- [ ] `view.go`（275）
- [ ] [T] `activity_test.go`（90）
- [ ] [T] `agents_panel_test.go`（160）
- [ ] [T] `background_test.go`（149）
- [ ] [T] `context_test.go`（129）
- [ ] [T] `goal_phase_test.go`（33）
- [ ] [T] `model_test.go`（1047）
- [ ] [T] `path_links_test.go`（38）
- [ ] [T] `recovery_test.go`（61）
- [ ] [T] `tool_status_test.go`（115）

#### `internal/workspacecoord`
- [ ] `applycommit.go`（184）
- [ ] `applyplan.go`（261）
- [ ] `candidate.go`（171）
- [ ] `clone_darwin.go`（11）
- [ ] `clone_linux.go`（35）
- [ ] `clone_other.go`（9）
- [ ] `coordinator.go`（118）
- [ ] `git.go`（188）
- [ ] `git_patch.go`（95）
- [ ] `manifest.go`（211）
- [ ] `merge.go`（337）
- [ ] `merge3.go`（278）
- [ ] `merge_snapshot.go`（356）
- [ ] `preservation.go`（148）
- [ ] `resolution.go`（106）
- [ ] `snapshot.go`（413）
- [ ] `snapshot_review.go`（127）
- [ ] `types.go`（304）
- [ ] [T] `export_test.go`（10）
- [ ] [T] `memory_journal_test.go`（77）
- [ ] [T] `merge3_internal_test.go`（111）
- [ ] [T] `merge_test.go`（230）
- [ ] [T] `preservation_test.go`（269）
- [ ] [T] `snapshot_test.go`（295）

#### `internal/workspacedomain`
- [ ] `domain.go`（300）
- [ ] `identity.go`（44）
- [ ] `identity_other.go`（11）
- [ ] `identity_unix.go`（19）
- [ ] `instance.go`（134）
- [ ] `leases.go`（487）
- [ ] `lock_other.go`（20）
- [ ] `lock_unix.go`（29）
- [ ] `rendezvous.go`（158）
- [ ] `store.go`（193）
- [ ] [T] `domain_test.go`（228）
- [ ] [T] `leases_test.go`（210）

#### `sdk/pluginabi`
- [ ] `abi.go`（28）
- [ ] `bridge_wasip1.go`（85）
- [ ] `source.go`（11）

## 2026-10-28 增量阅读：动态计划与恢复

历史统计和行号以上次审计时快照为准。当前新增入口：

- `orchestration/task.go` 的可选计划编辑/细化接口；`task_retry.go` 的可信失败分类及 3 次执行上限。
- `sqlitestore/tasks.go` 的 `UpdateTask`、`task_refinement.go` 的单事务细化、`task_plan_authorization.go` 的当前 owner/祖先授权、`task_reassign.go` 的失败执行安全证明。v40 `task_refinement_members` 明确区分分组与完成等待；`task_dependencies.go` 使用相同边集查环。
- `agentmanager/tools_task.go` 和 `tools_bindings.go` 的 `task_update`、`task_refine`、`task_reassign`；`supervisor_tasks.go` 的就绪派发绑定 Task version；`supervisor_reassign.go` 在真实 workspace domain 检查资源后改派。
- `agentruntime/turn_journal.go`、`model.go`、`request_context.go` 的已确认请求重放；`tool/failure_receipt.go` 的宿主工具结果回执；真实 Eino 测试覆盖循环压缩与 checkpoint/wait 恢复。
- `agentmanager/supervisor_run.go` 和 `supervisor_recovery.go` 冻结 worker 结果再结算，协调失败留 pending，恢复不再运行该 worker，终态后操作可补齐。`sqlitestore/agent_completions.go` 同事务校验 RunID，读取回执核验摘要和结果契约。

回归从 `task_plan_test.go`、`task_refinement_test.go`、`task_reassign_test.go`、`agent_completion_test.go`、`supervisor_completion_test.go`、`turn_journal_test.go`、`wait_eino_recovery_test.go`、`failure_receipt_test.go` 读起。不能把纯单元测试通过理解为外部未知副作用的 exactly-once 保证；缺失确认的写调用仍需要核对。


## 2026-10-29 Goal durable admission token

- `agentgoal/admission.go`：AdmissionState、AdmissionRequest、AdmissionRepository；状态为 reserved、turn_started、round_started、consumed、unavailable。
- `agentgoal/admission_digest.go`：宿主生成 opaque token 及输入、Contract、Goal 配置摘要。
- `sqlitestore/goal_admissions.go`：v41 表的幂等 Prepare、Turn/round 绑定、消费、不可用标记和 CAS。
- `agentruntime/turn.go`、`recovery.go`：`turn.started` 持久 admission token，恢复记录检查 token 状态。
- `conversation/run.go`、`admission.go`、`goal_loop.go`、`goal_outcome.go`：Always/auto Goal 预留 token、绑定首轮、完成消费；恢复扫描发现孤立预留并 fail-closed。
