# Multi-Agent 子系统架构设计

> 文档状态：Architecture Proposal v1（含实施进度标注；“实际实现：…”标注代码与设计的偏差，“未实现/计划中”标注尚未落地的设计）  
> 更新日期：2026-10-29
> 目标版本：在保持现有可信工具边界的前提下，提供可恢复、可审计、可编排的多 Agent Runtime  
> 设计原则：先冻结完整系统契约，再按纵向切片分阶段实现  
> 阶段状态：Phase 0–2 已落地；Phase 3 基础闭环已落地（关键路径优先未实现；细化与失败改派已实现）；Phase 4 基础闭环已落地（git worktree + 快照隔离 + 三方合并队列 + 可配置的合并后检查）；Phase 5 未实现。详见 §25。执行层并行化计划与逐阶段完成记录见 `MULTI_AGENT_PARALLEL_PLAN.md`（v2.1，P0–P3 已完成，P4 计划中）

---

## 1. 结论与关键决策

Multi-Agent 不是把现有 Eino 工具执行从串行改成并行，也不是增加一个 `spawn` 函数就完成。它是覆盖 Agent 生命周期、任务图、消息传递、资源调度、权限、审批、工作区冲突、持久化恢复和交互界面的完整控制面。

本设计固定以下决策：

1. **完整架构先行，纵向切片交付**
   - 先固定 ID、状态机、事件、权限、取消、恢复和工作区语义。
   - 后续阶段只增加能力，不改写前一阶段已经公开的核心语义。
   - 不为了尽快看到并行效果而引入只能在单进程、单会话下工作的临时协议。

2. **Supervisor 是唯一 Agent 生命周期控制面**
   - `agentmanager.Supervisor` 接受 spawn、message、wait、interrupt 和 task 命令。
   - Agent 不直接创建不受监管的 goroutine，也不直接修改其他 Agent 的状态。
   - 所有控制命令先耐久记录，再产生进程内副作用。
   - 实际实现：没有独立命令队列/command loop；各方法直接在 SQLite 事务中以 CAS（`Expected` 版本）写入状态与事件，提交成功后才启动 goroutine 或取消 context；spawn 由进程内 admission mutex 串行化配额检查。

3. **每个 Agent 拥有独立 Thread 和上下文窗口**
   - 一个 Agent 同时只执行一个 turn。
   - 不同 Agent 可以并行执行，各自拥有独立 Session、Eino Runner、Checkpoint、token 预算和历史压缩边界。
   - 父子 Agent 不共享可变消息切片或模型上下文。

4. **普通工具继续保持 Agent 内串行执行**
   - 保留 `internal/agentruntime/eino_turn.go` 中 `ExecuteSequentially: true`。
   - 并发发生在 Agent Thread 之间，而不是在同一个 ReAct turn 内无约束地并发执行副作用工具。
   - 将其直接改成 `false` 无法解决生命周期、恢复、权限和文件冲突问题，因此不作为 Multi-Agent 实现路径。

5. **Eino 是执行引擎，不是多 Agent 控制面**
   - 继续使用 Eino `ChatModelAgent`、Runner、Interrupt/Resume 和 Checkpoint。
   - Eino `NewAgentTool` 可以作为适配手段，但不能替代 durable Supervisor、任务图、Mailbox、审批和 UI 协议。
   - Eino `NewParallelAgent` 更适合固定工作流和共享上下文图，不作为动态 `spawn/send/wait/interrupt` 的主实现。

6. **结构化并发，默认禁止孤儿 Agent**
   - 每个非 root Agent 必须有父 Agent、所属 Team 和取消作用域。
   - 父级取消默认向下传播；父级完成前必须收割、转交或取消全部子 Agent。
   - 第一版不支持 detached Agent。
   - 实际实现：中断向下递归传播。`Interrupt` 取消目标 Agent 后逐个中断其非 terminal 子 Agent（queued 与 running 均是），并由此覆盖全部后代；Team 级取消见 §9.1；`Supervisor.Close` 取消全部非 root Agent。父级终态提交后会递归中断仍存活的后代，恢复扫描也会修复终态父级留下的 queued 后代。

7. **权限只能收窄，不能因委派而升级**
   - 子 Agent 的有效权限是全局策略、父级可委派权限、Agent Profile 上限和任务授权范围的交集。
   - 子 Agent 不能批准自己的请求，也不能把自己的权限再委派成更高权限。
   - 所有工具仍经过现有 `tool.Pipeline -> policy -> approval -> executor` 边界。

8. **事件日志是事实来源，投影表用于查询**
   - Session 消息和 Eino checkpoint 继续使用现有边界。
   - 新增 orchestration event stream，记录 Team、Agent、Task、Mailbox、Lease、Budget 和恢复事实。
   - UI 不能成为状态来源；进程退出后可从数据库恢复 Team。

9. **共享工作区必须有协调协议**
   - 精确 workspace 工具使用路径 lease、基线 fingerprint 和冲突检测。
   - 无法静态确定写集合的 shell 默认获取工作区级写 lease，或运行在隔离 Git worktree。
   - 高冲突任务可以使用独立 worktree 和 merge queue，不能只依赖模型口头约定“不要修改同一文件”。

10. **交互体验以“可观察、可控制、不喧宾夺主”为目标**
    - 主对话仍以 root Agent 为中心。
    - Agent 活动、任务、审批和资源占用通过独立面板查看。
    - 用户可以进入任意 Agent Thread、发送指令、等待、取消并查看完整工具轨迹。
    - 实际实现：TUI `/agents`（Ctrl+G）面板可查看子 Agent 与合并候选列表、打开子 Agent transcript（只读）、二次确认后取消子 Agent（`x x`）、重新审阅待决合并（`r`）、二次确认后丢弃合并候选（`d d`）；进入子 Thread 对话、向子 Agent 发送指令（steer）未实现。

11. **行为参考公开产品文档，代码采用独立 Go 实现**
    - 参考 Codex subagents 的独立 thread、spawn、steer、wait、collect 和并发限制。
    - 参考 Claude Code subagents/Agent Teams 的独立上下文、任务依赖和 mailbox。
    - 不复制其源代码、内部协议或视觉资产；本项目按自己的领域模型和现有可信边界实现。

---

## 2. 背景与当前差距

当前系统已经具备适合作为 Multi-Agent 基础的能力：

- `conversation.Conversation` 为一个 Session 串行化 turn；
- `agentruntime.Factory` 为每个 turn 创建固定 Tool Registry Snapshot 的 Eino Runner；
- `session.EventStore` 保存带 checksum 的 append-only 事件；
- `session.CheckpointStore` 独立保存 Eino checkpoint；
- `tool.Pipeline` 统一处理 proposed、policy、approval、execute 和 result；
- `approval.Broker` 可以排队多个审批请求；
- tokenizer、上下文压缩、provider usage 和 cache token 已有统一基础；
- TUI 已经能展示流式消息、工具调用、审批和上下文用量；
- workspace 工具、Policy、WASM 和 OS sandbox 已经形成可信执行边界。

当前限制是：

| 领域 | 当前行为 | Multi-Agent 所需行为 |
| --- | --- | --- |
| 会话 | 一个 `Conversation` 使用一个 mutex 串行 turn | 每个 Thread 内串行，不同 Thread 可并行 |
| Agent 配置 | 一个主模型和一个 Agent Profile | 多模型、多 Profile、每个 Agent 固定配置快照 |
| 生命周期 | turn 开始、完成、失败 | Team/Agent 创建、排队、运行、等待、取消、恢复、终态 |
| 任务 | 没有 durable task graph | DAG、依赖、claim、重分配、验证 gate |
| 通信 | 用户与单 Conversation 通信 | durable mailbox、steer、question、result、ack |
| 调度 | 没有全局并发与预算调度 | 有界并发、provider 限流、token/cost/time 预算 |
| 审批 | 请求缺少 Agent/Task 来源 | 中央队列，明确来源、作用域和取消语义 |
| 工作区 | 多调用共享同一目录 | lease、fingerprint、worktree、merge queue |
| 恢复 | 单 turn checkpoint 恢复 | Team/Agent/Task/Mailbox/Lease 全面恢复 |
| UI | 单 transcript | root transcript + Agent/Task 活动面板 + thread 切换 |

注：上表“当前行为”为 2026-09 立项时的单 Agent 基线；截至 2026-10-26 的落地情况见 §25。

因此，Multi-Agent 应作为现有 `conversation` 和 `agentruntime` 之上的新控制面，而不是侵入或替换已经稳定的单 Agent 执行边界。

---

## 3. 目标、非目标与成功标准

### 3.1 目标

1. 支持模型或用户动态创建、查看、等待、指导和取消子 Agent。
2. 支持多个 Agent 在资源上限内真实并行执行。
3. 支持独立上下文、不同模型和不同工具/权限 Profile。
4. 支持 durable Task DAG、依赖、认领、重试、重分配和验证 gate。
5. 支持 Agent 间可靠消息传递和结构化结果回传。
6. 支持共享目录下的主动冲突预防与可选 worktree 隔离。
7. 支持进程崩溃或重启后的 Team 恢复，不丢失任务、消息和审批事实。
8. 支持按 Agent、Task、Team、Provider 汇总 token、cache token、cost 和耗时。
9. 同时服务 TUI 和 headless 客户端，不把 Bubble Tea 或 ANSI 语义写入 Runtime。
10. 在简单任务上避免无收益委派，在复杂任务上支持自适应拆分、并行、验证和收敛。

### 3.2 非目标

第一版明确不做：

- 跨机器分布式 Agent 集群；
- 多用户协作、云端租户隔离和远程 RBAC；
- 任意长时间后台运行的 detached Agent；
- 多 Agent 共享同一个可变模型上下文；
- 让模型绕过 Supervisor 直接启动进程或 goroutine；
- 自动合并任何有冲突的代码并声称成功；
- 保存或展示 provider 未明确返回的隐藏思维链；
- 依靠 tokenizer 估算值伪装成 provider 账单真值；
- 用 Multi-Agent 替代确定性更强的普通并行工具或固定工作流。

### 3.3 成功标准

达到生产可用需要同时满足：

- 8 个 Agent 并发运行时无数据竞争、无跨 Thread 消息串线；
- 任意时刻 kill 进程后重启，Team 可进入一致且可解释的恢复状态；
- 父级取消后，不再出现继续调用副作用工具的子 Agent；
- 权限委派属性测试证明子权限永远不大于父级可委派权限；
- 两个 Agent 修改同一路径时，要么被协调，要么明确产生 conflict，不能静默覆盖；
- 所有控制操作均有 event、correlation ID 和可重放结果；
- TUI 能观察所有 Agent 和审批，headless 模式能通过稳定 JSON 事件完成同等控制；
- token/cost 汇总与各 provider 返回的原始 usage 可逐项核对。

---

## 4. 核心术语

| 术语 | 定义 |
| --- | --- |
| Team | 一次多 Agent 协作的 durable 根聚合，包含 root Agent、Task Graph 和预算 |
| Agent | 受 Supervisor 管理的逻辑执行者，拥有 Profile、Thread、父子关系和生命周期 |
| Thread | 一个 Agent 独占的 durable 对话上下文；内部映射到现有 Session |
| Turn | Thread 中一次用户/控制输入到最终 assistant 结果的执行 |
| Agent Profile | 模型、instruction、工具、权限、上下文、预算和 workspace 模式的不可变配置模板 |
| Task | Task Graph 中可认领、可依赖、可验证的工作单元 |
| Mailbox | Agent 间 durable、有序、可确认的消息队列 |
| Lease | 对文件路径、路径前缀、worktree 或任务 claim 的有时限占有权 |
| Workspace View | Agent 看到的工作区视图，可以是共享根目录或独立 worktree |
| Supervisor | 接受命令、验证不变量、持久化意图、调度 Agent 并恢复状态的控制面 |
| Scheduler | 根据并发、provider、预算、公平性和依赖决定哪个 Agent 可以运行 |
| Projection | 从 append-only orchestration events 构建的可查询状态表 |
| Activity | Agent 当前的瞬时等待原因，例如 model、tool、approval、children、rate limit |

### 4.1 标识符

所有 ID 均为宿主生成的不可猜测字符串，采用带类型前缀的 UUIDv7 或等价单调 ID：

```text
team_...
agent_...
thread_...
task_...
turn_...
msg_...
approval_...
lease_...
run_...
event_...
```

约束：

- `AgentID` 标识逻辑 Agent，进程重启后保持不变；
- `RunID` 标识一次调度尝试，重试时变化；
- `ThreadID` 标识上下文血缘；第一版一个 Agent 对应一个 Thread；
- `ThreadID` 映射到 `session.ID`，但 API 和事件中保留字段名，避免把“Agent 身份”和“消息分区”混为一谈；
- `TaskID` 与 Agent 生命周期独立，一个任务可被重新认领；
- `CorrelationID` 贯穿用户命令、Agent control tool call、Task 和子 Agent；
- 所有重试命令必须携带 `IdempotencyKey`。

实际实现：ID 格式为 `<prefix>_<32 位小写 hex>`，hex 部分为项目自行生成的 UUIDv7（`orchestration/id.go`）；实际前缀为 `team`、`agent`、`thread`、`task`、`run`、`msg`、`approval`、`lease`、`event`，另有 `view`（Workspace View）、`wait`（WaitSet）、`merge`（合并请求）；没有 `turn_` 前缀（turn 沿用现有 Session/conversation 的标识）。`ThreadID` 是独立 ID，通过 `agent_threads` 表与 `session_id` 一对一映射，并非直接复用 `session.ID`。Agent control tool 的 `idempotency_key` 缺省取 ToolCallID；headless 控制协议没有 `idempotency_key` 字段（见 §21.3）。

---

## 5. 不可破坏的不变量

1. 一个 Thread 最多有一个 active turn。
2. 一个 Agent 最多有一个 active Run。
3. 一个 Task 同时最多有一个有效 claim。
4. terminal Agent 和 terminal Task 不允许回到非 terminal 状态；Agent 只有在进入 terminal 前才能创建新 Run，terminal 后的任务重试必须创建新的 Agent attempt。实际实现：`canceled` Agent 与 terminal Task 不可回退；`completed`/`failed` Agent 允许经 `agent_continue` 或任务指派回到 `queued` 并以新 RunID 继续同一 Thread（见 §9.2）。
5. 非 root Agent 必须拥有一个存在于同一 Team 的父 Agent。
6. 父子关系在 Agent 创建后不可修改；任务可重新分配，但 Agent 血缘不可重写。
7. 父级退出前必须处理全部非 terminal 子级；第一版不允许 detached child。实际实现：未强制收割，父级可在子级未结束时完成；子级在父级 canceling/terminal 后不再被调度（见 §1 决策 6）。
8. 子 Agent 的有效能力必须是权限集合的交集，不能出现权限并集。
9. 任何副作用工具都必须经过 `tool.Pipeline`，Agent control tool 也不例外。
10. 一个 turn 从开始到结束固定 Tool Registry、Agent Profile、Model Profile 和 Workspace View snapshot。
11. orchestration 状态变更必须与对应 event 在同一 SQLite transaction 内提交。
12. 未提交的 event 不得发送给 UI；已提交的 event 最终必须可被重放。
13. Mailbox 消息至少投递一次，消费者必须按 `MessageID` 幂等处理。
14. tool outcome unknown 时禁止盲目重试副作用工具。
15. 未获取有效 write lease 的 Agent 不得在共享 Workspace View 中执行已知写操作。
16. 用户审批只能授权该请求描述的规范化操作，不能成为子 Agent 的通用提权令牌。
17. provider usage 原始值不可被本地估算覆盖；归一化值必须保留质量状态。
18. UI、ANSI、按键和折叠状态不进入领域事件。

---

## 6. 总体架构

```text
cmd/local-agent
        |
        +--> internal/cli ------------------------------+
        |      |                                        |
        |      +--> internal/tui                        |
        |      +--> internal/render/headless            |
        |                                               v
        +------------------------------------> agentmanager.Supervisor
                                                        |
                     +----------------------------------+--------------------+
                     |                                  |                    |
                     v                                  v                    v
             taskgraph.Graph                    agentmailbox.Mailbox   scheduler.Scheduler
                     |                                  |                    |
                     +-------------------+--------------+                    |
                                         v                                   |
                                 agentthread.Registry <----------------------+
                                         |
                              one Conversation per Thread
                                         |
                                 agentruntime.Factory
                                         |
                               Eino ChatModelAgent/Runner
                                         |
                                einoadapter -> tool.Pipeline
                                         |
                         policy -> approval -> workspacecoord -> executor

agentmanager.Supervisor
        +--> agentprofile.Registry
        +--> workspacecoord.Coordinator
        +--> tokenusage.Ledger / budget.Manager
        +--> orchestration.EventStore + Projections
        +--> session.EventStore / CheckpointStore
```

实际实现：图中 `taskgraph`、`agentmailbox`、`agentthread`、`budget` 包未单独建立，`internal/render/headless` 也不存在。Task DAG/claim、Mailbox、Thread 投影、预算账户的领域类型与接口位于 `internal/orchestration`，SQLite 实现集中在 `internal/session/sqlitestore`（`OrchestrationStore` 等）；Task 调度、Mailbox 注入和 Thread 打开由 `internal/agentmanager` 与 `internal/app` 装配；预算 reservation/settlement 位于 `internal/tokenusage` 与 `sqlitestore`；headless 输出位于 `internal/cli`（`chat_headless.go`、`control.go`）。另有设计外的 `internal/workspacedomain`（跨进程工作区协调库与 lease，见 §17）和 `internal/agentflow`（durable Flow 引擎）。

### 6.1 控制面与执行面

**控制面**负责：

- Team、Agent、Task 和 Mailbox 命令；
- 状态机验证；
- 资源和预算准入；
- 权限派生；
- Workspace View 与 lease；
- durable event、projection 和恢复；
- 对 TUI/headless 发布统一事件。

**执行面**负责：

- 一个 Thread 内的上下文准备与压缩；
- Eino Agent Loop；
- 模型流式调用；
- 工具调用与 checkpoint；
- 将执行事件回传 Supervisor。

控制面不得解析 provider wire format；执行面不得直接修改 Team/Task 状态。

### 6.2 Supervisor 并发模型

Supervisor 使用“单命令提交器 + 外部 worker”模式：

1. 一个 command loop 串行验证控制命令和写入 orchestration transaction。
2. 模型、工具和长时间等待在受控 worker goroutine 中运行，绝不阻塞 command loop。
3. worker 只通过带 `RunID` 和期望状态版本的结果命令回写。
4. scheduler 根据已提交 projection 做 admission，不直接修改状态。
5. committed events 经 broadcaster 发布给 UI；慢订阅者按 cursor 从数据库补读。

这使生命周期操作保持确定顺序，同时允许真正的 Agent 执行并行。

实际实现：没有单一 command loop 和 broadcaster。Supervisor 是具体 struct，各方法并发调用：状态变更通过 SQLite 单连接事务 + `Expected` 版本 CAS 串行化，spawn 配额由进程内 admission mutex 保护，进程内 `running` 表由 `mu` 保护；worker 结束经 `finish` 以 CAS 回写；状态变化通过 wake channel（`signal`）唤醒 `Wait`/调度。TUI 与 headless 不订阅推送，而是按 cursor 轮询数据库（`LoadGlobal`/team snapshot）。

---

## 7. 包边界与依赖方向

### 7.1 新包

| 包 | 单一职责 | 禁止依赖 |
| --- | --- | --- |
| `internal/agentmanager` | Supervisor 命令、生命周期、恢复和对外查询 | TUI、ANSI、具体 provider |
| `internal/agentthread` | Thread 与 Conversation 的注册、打开、关闭和单 turn 约束 | Task UI、具体审批 UI |
| `internal/agentprofile` | Profile 校验、快照、模型/工具/权限派生 | Eino Runner、SQLite |
| `internal/taskgraph` | Task DAG、依赖、claim、状态机和验证 gate | 模型、TUI、workspace executor |
| `internal/agentmailbox` | durable 消息、顺序、ack、等待和去重 | Eino、TUI |
| `internal/workspacecoord` | Workspace View、lease、fingerprint、worktree 和 merge queue | 模型、TUI |
| `internal/scheduler` | admission、并发、公平性、provider 限流和 backoff | SQLite schema、Bubble Tea |
| `internal/budget` | 分层预算账户、reservation、settlement 和 hard limit | provider HTTP、TUI |
| `internal/artifact` | transcript、diff、报告等大对象的内容寻址存取和保留策略 | 模型、TUI 布局 |
| `internal/orchestration` | event envelope、store、projection、subscription cursor | Eino、工具实现 |
| `internal/tokenusage` | 扩展现有 usage snapshot，保存 provider 原始值并提供 token/cache/cost 账本与预算聚合 | TUI |

### 7.2 现有包调整

| 现有包 | 调整 |
| --- | --- |
| `internal/app` | 装配 Supervisor、Profile Registry、Scheduler、Coordinator 和 stores；保持 Composition Root |
| `internal/conversation` | 保持一个 Thread 内的 turn 串行化；增加稳定 Thread metadata 输入，不承担跨 Agent 调度 |
| `internal/agentruntime` | Factory 改为按 Profile/Thread snapshot 构建；执行语义保持单 Agent |
| `internal/einoadapter` | 为每个工具调用注入 Team/Agent/Thread/Task/Run 元数据 |
| `internal/tool` | `Call` 增加 actor scope；Pipeline 插入 Workspace Coordinator hook，但不理解 TUI |
| `internal/policy` | Request 增加 actor/delegation scope；实现权限交集校验 |
| `internal/approval` | Request 增加 Agent/Task 来源、过期和取消；Broker 作为 UI bridge，不作为 durable source |
| `internal/session/sqlitestore` | 演进为共享 Database owner 下的 Session、Checkpoint、Orchestration store |
| `internal/tui` | 消费统一 control/runtime events；增加活动面板、thread 导航和中央审批队列 |
| `internal/config` | 支持多 Model Profile、Agent Profile、Team limits 和 workspace strategy |

实际实现（7.1/7.2）：已建立 `agentmanager`、`agentprofile`、`workspacecoord`、`scheduler`、`artifact`、`orchestration`，并扩展 `tokenusage`；`agentthread`、`taskgraph`、`agentmailbox`、`budget` 未单独建包（职责去向见 §6 注）。`internal/config` 只新增 `[agents]` 段（并发/深度/子级上限、`model_limits`、`default_workspace`、`auto_merge`、`auto_continue`、`snapshot_copy_limit`）；多 Model Profile、可配置 Agent Profile 未实现，内置 Profile 共用主模型 `model.name`。Team limits 中的预算与价格表不可配置（见 §13.5）。

### 7.3 依赖规则

```text
cli/tui/render
      -> agentmanager public API
      -> read-only projection API

agentmanager
      -> taskgraph / agentmailbox / scheduler / workspacecoord / usage
      -> agentthread
      -> orchestration store

agentthread
      -> conversation -> agentruntime

agentruntime
      -> einoadapter -> tool -> policy
      -> session stores
```

禁止：

- `taskgraph` 调用模型；
- `scheduler` 直接写 SQLite；
- `tui` 直接访问 `Conversation` mutex 或 Eino Runner；
- `agentruntime` 反向调用 Supervisor 具体实现；
- `workspacecoord` 绕过 `codingtools` 路径规范化；
- 包之间通过全局变量共享 Agent 状态。

---

## 8. 领域对象

### 8.1 Team

```go
type Team struct {
    ID          TeamID
    RootAgentID AgentID
    State       TeamState
    Limits      TeamLimits
    Usage       UsageSummary
    Version     uint64
    CreatedAt   time.Time
    UpdatedAt   time.Time
}
```

Team 是预算、并发、取消和恢复的根。一次普通单 Agent 对话也可以创建只有 root Agent 的 Team，从而避免后续从“单会话模式”迁移到“多 Agent 模式”时出现两套协议。

实际实现：没有 `Team` Go struct，只有 `teams` 表（`team_id`、`root_agent_id`、`state`、`version`、`created_at`、`updated_at`）；Limits 与 Usage 不在 Team 上，预算位于 `team_budgets`，用量汇总由 `usage_records` 聚合。单 Agent 对话经 `EnsureRootThread` 建立只含 root Agent 的 Team/Agent/Thread 投影，与设计一致。

### 8.2 Agent

```go
type Agent struct {
    ID               AgentID
    TeamID           TeamID
    ParentAgentID    AgentID
    ThreadID         ThreadID
    ProfileRevision  ProfileRevision
    AssignedTaskID   TaskID
    State            AgentState
    Activity         AgentActivity
    Depth            int
    WorkspaceViewID  WorkspaceViewID
    EffectivePolicy  PolicySnapshotID
    CurrentRunID     RunID
    Version          uint64
}
```

`State` 是 durable 生命周期；`Activity` 是可恢复或可重建的当前等待原因。两者不能混成一个不断膨胀的枚举。

实际实现：类型名为 `orchestration.AgentSnapshot`，字段为 `ID`、`TeamID`、`ParentAgentID`、`ThreadID`、`SessionID`、`AssignedTaskID`、`InitialPrompt`、`ContextPolicy`、`ProfileRevision`、`ProfileSnapshot`（spawn 时冻结的 Profile JSON）、`Workspace`（请求的访问）、`WorkspaceMode`（授予的模式）、`CurrentRunID`、`State`、`Activity`（普通字符串）、`Depth`、`Version`、`CreatedAt`、`UpdatedAt`、`Result`。没有 `WorkspaceViewID` 与 `EffectivePolicy` 字段：View 通过 `workspace_views` 按 Agent 查询，有效权限在运行时由 Profile 快照派生（见 §16.1）。

### 8.3 Thread

```go
type Thread struct {
    ID               ThreadID
    SessionID        session.ID
    AgentID          AgentID
    ParentThreadID   ThreadID
    ContextSeed      ContextSeed
    ContextPolicy    ContextPolicy
    ModelProfile     ModelProfileRevision
    ToolSnapshot     ToolSnapshotID
    LastSequence     uint64
    ActiveTurnID     string
    Version          uint64
}
```

Thread 不保存共享消息指针。Context seed 只引用 spawn 时已经完成并持久化的父级消息范围或 summary artifact。

实际实现：没有 `Thread` struct 与 `ContextSeed` 记录，只有 `agent_threads` 表（`thread_id`、`session_id` UNIQUE、`team_id`、`agent_id` UNIQUE、`parent_thread_id`、`context_policy`、`profile_revision`、`last_sequence`、`active_turn_id`、`version`、时间戳）；`ModelProfile`/`ToolSnapshot` 不单独持久化，由 Agent 的 `ProfileSnapshot` 决定。Context seed 在 spawn 时从父级已提交消息生成文本，前置到子 Agent 的 `InitialPrompt`，不引用 summary artifact（见 §11）。

### 8.4 Task

```go
type Task struct {
    ID               TaskID
    TeamID           TeamID
    ParentTaskID     TaskID
    Title            string
    Description      string
    State            TaskState
    Priority         int
    Dependencies     []TaskID
    AssigneeAgentID  AgentID
    Claim            *TaskClaim
    RequiredProfile  string
    WorkspaceScope   []PathScope
    Acceptance       []AcceptanceCriterion
    Verification     VerificationPolicy
    Attempt          int
    Version          uint64
}
```

Task 是工作事实，不等同于 Agent。Agent 失败后 Task 可以重新进入 ready 并交给另一个 Agent。

实际实现：类型名为 `orchestration.TaskSnapshot`，字段为 `ID`、`TeamID`、`ParentTaskID`、`Title`、`Description`、`State`、`Priority`、`AssigneeAgentID`、`RequiredProfile`、`VerificationProfile`（字符串，代替 `VerificationPolicy`）、`ClaimExpiresAt`、`Attempt`、`Version`、时间戳。依赖存于 `task_dependencies` 表；claim 是 `tasks` 上的列而非 `TaskClaim` 结构；`WorkspaceScope`、`Acceptance` 未实现。Agent 失败时其 Task 进入 `failed`；Agent 取消时释放 claim、Task 回到 `ready`；结果为 `partial`/`blocked` 时 Task 进入 `blocked`。可信的 iteration-limit 失败在安全证明通过后可由 `task_reassign` 或恢复扫描创建下一次 attempt，默认总执行次数上限为 3；网络/额度/预算暂停恢复原 Run，不消耗 attempt。

### 8.5 Agent Result

子 Agent 必须返回结构化结果，而不是只返回一段自然语言：

```go
type AgentResult struct {
    Status       ResultStatus       `json:"status"`
    Summary      string             `json:"summary"`
    Changes      []ChangeSummary    `json:"changes,omitempty"`
    Verification []VerificationRun  `json:"verification,omitempty"`
    Artifacts     []ArtifactRef      `json:"artifacts,omitempty"`
    Decisions     []DecisionRecord   `json:"decisions,omitempty"`
    Risks         []string           `json:"risks,omitempty"`
    Blockers      []string           `json:"blockers,omitempty"`
    FollowUps     []SuggestedTask    `json:"follow_ups,omitempty"`
    Usage         UsageSummary       `json:"usage"`
}
```

`status` 只能是 `succeeded`、`partial`、`blocked`、`failed` 或 `canceled`。父 Agent 收到的是该结构的摘要和 artifact 引用，完整 child transcript 通过 Thread inspection 查看，不自动复制进父上下文。

实际实现：`orchestration.AgentResult` 字段为 `Status`、`Summary`、`Payload`（任意合法 JSON）、`Blockers`、`Risks`、`Error`、`Delivery`（仅 Supervisor 填写：`ViewID`、`CandidateDigest`、`MergeID`、`ChangedPaths`，表示快照候选及其合并请求）；`Changes`、`Verification`、`Artifacts`、`Decisions`、`FollowUps`、`Usage` 未实现。status 取值与设计一致。结果以 `result` 类型 Mailbox 消息（幂等键 `result:<runID>`）投递给父 Agent；完整 child transcript 只能通过 TUI `/agents` 面板查看，没有 Thread inspection 工具。

---

## 9. 状态机

### 9.1 Team 状态

```text
active -> draining -> completed
   |         |
   |         +---------> failed
   +-------------------> canceling -> canceled
```

| 状态 | 含义 |
| --- | --- |
| `active` | 可创建任务和 Agent |
| `draining` | 不再接受新 spawn，等待已有工作收敛 |
| `canceling` | 正在向全部非 terminal Agent 传播取消 |
| `completed` | root 已完成，全部 child 已收割，任务图满足完成条件 |
| `failed` | root 或关键任务失败，且恢复策略耗尽 |
| `canceled` | 用户或上级明确取消，所有 child 已终止 |

实际实现：Team 状态由 `teams.state/version` 持久化，迁移经 `orchestration.ValidateTeamTransition` 校验（`active→draining|canceling`，`draining→completed|failed|canceling`，`canceling→canceled`，`completed|failed|canceled→active`），`sqlitestore.TransitionTeam` 做乐观版本检查（`ErrTeamStateConflict`）并按目标状态追加 `team.draining_started`/`team.cancel_requested`/`team.completed`/`team.failed`/`team.canceled`/`team.reopened`。
- Team 取消：`Supervisor.CancelTeam`（control 命令 `team.cancel`）在 admission 锁内把 Team 置为 `canceling`，中断全部非 terminal 子 Agent（中断会级联到其后代），等待它们终止后置为 `canceled`；重复调用对已取消的 Team 无副作用。等待超时或进程中断会让 Team 停在 `canceling`，再次执行 `team.cancel` 即可完成。root Agent 不被中断，会话保留。
- spawn 门禁：`CreateChild` 在建 Agent 的同一事务中要求 Team 为 `active`，否则返回 `ErrTeamNotActive`；与取消竞争的 spawn 要么先落地并被中断，要么被拒绝。就绪任务派发遇到该错误时停止派发，任务保持 `ready`。
- 重开：Team 的生命周期跨越同一会话的多次运行，`EnsureRootThread` 恢复会话时把 `completed/failed/canceled` 的 Team 迁回 `active` 并记录 `team.reopened`。
- 限制：会话 Team 不会产生 `draining`/`completed`/`failed`（状态机已支持，没有生产调用方）；Team 状态出现在 control `snapshot` 的 `state` 字段中。

### 9.2 Agent 状态

```text
created -> queued -> running -> completed
              ^       |  |
              |       |  +-> waiting -> queued
              |       +----> failed
              |
              +---- retryable Run failure -> queued with a new RunID

created/queued/running/waiting -> canceling -> canceled
```

允许的 durable 状态：

| 状态 | 可进入条件 | 可离开方向 |
| --- | --- | --- |
| `created` | spawn 已提交，Thread/Profile/权限已冻结 | `queued`、`canceled` |
| `queued` | 依赖满足，等待 scheduler admission | `running`、`canceling`、`failed` |
| `running` | 已分配 Run slot | `waiting`、`completed`、`failed`、`canceling` |
| `waiting` | 不消耗 model slot，等待 approval/message/child/rate-limit/retry | `queued`、`canceling`、`failed` |
| `canceling` | cancel 已提交，正在终止模型/工具/child | `canceled`、`failed` |
| `completed` | 结构化结果已提交，child 已收割 | 无 |
| `failed` | 不可恢复错误或重试耗尽 | 无 |
| `canceled` | 取消已完成 | 无 |

`failed` 是 Agent 终态。单次 Run 的可重试失败先记录 `agent.run_failed`，Agent 回到 `queued` 并在下次 admission 获得新 `RunID`；只有重试耗尽才进入 `failed`。

实际实现（`orchestration/state.go` `ValidateAgentTransition`）：
- `created → queued/canceled`；`queued → running/canceling/failed`；`running → queued/waiting/completed/failed/canceling`；`waiting → queued/canceling/failed`；`canceling → canceled/failed`；
- `completed → queued`、`failed → queued`：由 `agent_continue` 或任务指派重新排队，同一 Thread 以新 RunID 继续；只有 `canceled` 是不可离开的终态；
- `running → queued` 只用于进程重启恢复（Activity `recovered`，沿用原 RunID 从 checkpoint 恢复）；
- 普通 Run 失败进入 `failed`；只有宿主可信分类的 iteration-limit 失败才允许 Task 层安全改派，且不会把任意 provider 错误或模型输出的 boolean 当作重试授权；`agent.run_failed` 与结果记录仍按原 Run 持久化；
- `waiting` 只用于子 Agent 的 durable `agent_wait`（WaitSet，见 §13.2），不用于 approval/message/rate-limit/retry。

实际实现的 Activity 为普通字符串：Supervisor 写入 `runner`、`recovered`、`canceling`、`canceled` 及结果状态等；运行中由 runner 上报的实时 activity 只保存在内存（`liveActivity`）并在查询时覆盖，不持久化。

`Activity` 示例：`model`、`tool`、`approval`、`mailbox`、`children`、`capacity`、`rate_limit`、`retry_backoff`、`checkpoint_recovery`。Activity 变化可高频展示，但只在语义边界持久化，token delta 不逐条写 orchestration event。

### 9.3 Task 状态

```text
draft -> blocked -> ready -> claimed -> in_progress -> verification -> completed
          ^          ^         |           |              |
          |          +---------+-----------+--------------+
          |                    retry/reassign
          +------------------------------------------------

任意非终态 -> canceled
claimed/in_progress/verification -> failed
```

规则：

- 有未完成 dependency 的任务只能是 `blocked`；
- claim 使用 TTL 和版本号，claim 丢失后任务回到 `ready`；
- `in_progress -> completed` 只有在无需验证时允许；
- 需要验证时必须经过 `verification`；
- verifier/reviewer 默认不能与 worker 是同一个 Agent；
- dependency 失败时由策略决定 `blocked`、`canceled` 或创建替代任务，不能隐式忽略。

实际实现（`ValidateTaskTransition`）：新增 `awaiting_merge` 状态（快照隔离的任务完成后等待候选合入主工作区，合入后才 `completed`，依赖者不会提前启动）。迁移表：`draft → blocked/ready/canceled`；`blocked → ready/failed/canceled`；`ready → blocked/claimed/canceled`；`claimed → blocked/ready/in_progress/failed/canceled`；`in_progress → blocked/ready/verification/awaiting_merge/completed/failed/canceled`；`verification → blocked/ready/awaiting_merge/completed/failed/canceled`；`awaiting_merge → blocked/completed/failed/canceled`；`completed`/`failed`/`canceled` 为终态。claim TTL 固定 30 分钟（`claim_expires_at` 列），只有 `claimed` 且持有 Agent 已不存活的过期 claim 才会被回收到 `ready`（`attempt + 1`，事件 `task.claim_expired`）；`in_progress` 不按 TTL 回收。dependency 失败不传播：依赖者保持 `blocked`，由模型/用户处理，没有自动策略。

### 9.4 Approval 状态

```text
requested -> approved
          -> denied
          -> expired
          -> canceled
```

审批一旦进入终态不可修改。Agent/Task 取消会将仍 pending 的审批变为 `canceled`，等待中的 tool call 收到 typed cancellation，而不是被包装成通用 `NodeRunError`。

实际实现：`ApprovalState` 常量已定义但未使用，没有 durable 审批记录或状态机；审批仍由进程内 `approval.Broker` 处理，审批事实只以 `approval.requested`/`approval.resolved` 写入 session events。expired 状态未实现（计划中）。

### 9.5 Lease 状态

```text
requested -> active -> released
                   -> expired
                   -> conflicted
requested --------> rejected
```

Lease 过期不等于写入安全。恢复时必须比较 owner、workspace view 和 fingerprint，再决定释放、续租或标记冲突。

实际实现：lease 没有 TTL 与续租，只写入 `active`、`released` 和新增的 `abandoned`；`requested`、`expired`、`conflicted`、`rejected` 已定义但不写入（冲突直接返回 `ErrLeaseConflict` 或等待）。持有者存活通过 `workspacedomain` 的实例 flock 判定：持有实例及其启动的全部子进程退出后，lease 由 `ReapAbandonedLeases` 标记为 `abandoned`。`workspace.lease_*` 事件类型均未产生。

---

## 10. Agent Profile 与配置快照

### 10.1 Profile 内容

Profile 至少包含：

- 名称、描述、instruction revision；
- Model Profile；
- tool allowlist/denylist；
- 最大迭代次数；
- context window、压缩策略和继承上限；
- 可委派能力和最大 spawn 深度；
- workspace 模式；
- token、cost、wall-clock 和 tool-call 预算；
- retry policy；
- 是否允许创建 Task、发送消息、spawn child；
- 结构化结果 schema revision。

建议内置 Profile：

| Profile | 主要用途 | 默认权限 |
| --- | --- | --- |
| `explorer` | 阅读代码、搜索、形成方案 | 只读 workspace，无 shell 写入 |
| `worker` | 实现一个有边界的任务 | 任务路径范围内写入，受审批约束 |
| `tester` | 运行测试、复现问题 | 测试命令，默认不修改产品代码 |
| `reviewer` | 检查 diff、风险和遗漏 | 只读代码与 diff，不接受 worker 自证 |
| `general` | root/lead 协调 | 可创建任务和 Agent，权限仍受全局策略限制 |

实际实现：`agentprofile.Profile` 字段为 `Name`、`Description`、`Revision`、`ModelProfile`、`Instruction`、`Tools`、`DelegableTools`、`ContextInheritance`（none/summary/recent/full）、`WorkspaceMode`（shared-readonly/shared-lease/git-worktree）、`Limits{MaxIterations, MaxChildren, MaxInheritedTokens, MaxInputTokens, MaxOutputTokens, MaxToolCalls, MaxWallTime}`、`CanSpawn`、`CanManageTasks`、`CanSendMessages`；retry policy、cost 预算、结构化结果 schema revision 未实现。上表五个 Profile 未实现，内置 Profile 只有三个（`internal/app/profiles.go`，均使用主模型 `model.name`、`MaxIterations` 默认 20、`MaxChildren` 4、`MaxToolCalls` 为迭代数 ×8、`MaxWallTime` 30 分钟）：

| Profile | 继承 | Workspace | 能力 |
| --- | --- | --- | --- |
| `default` | none | shared-lease | 可 spawn、管理任务、发消息 |
| `worker` | summary | shared-lease | 可 spawn、管理任务、发消息 |
| `reviewer` | summary | shared-readonly | 不可 spawn；可管理任务、发消息 |

Profile 中的 shared-lease 写入者实际按 `[agents].default_workspace` 授予模式（默认 `auto`，即快照隔离，见 §17.1）。

### 10.2 配置示例

```toml
[multi_agent]
enabled = true
max_live_agents = 16
max_running_agents = 4
max_depth = 3
max_tasks = 128
default_context_inheritance = "summary"
workspace_strategy = "lease"

[multi_agent.team_budget]
max_input_tokens = 1000000
max_output_tokens = 250000
max_wall_time = "2h"

[models.profiles.primary]
provider = "openai"
wire_api = "responses"
name = "gpt-5.6-sol"
api_key_env = "XAPI_API_KEY"

[agents.profiles.explorer]
model = "primary"
instruction_file = "~/.gsea/agents/explorer.md"
tools = ["workspace_list", "workspace_read", "workspace_glob", "workspace_grep"]
max_iterations = 12
max_children = 0
workspace_mode = "shared-readonly"

[agents.profiles.worker]
model = "primary"
instruction_file = "~/.gsea/agents/worker.md"
tools = ["workspace_list", "workspace_read", "workspace_glob", "workspace_grep", "workspace_write", "workspace_edit", "shell_exec"]
max_iterations = 24
max_children = 2
workspace_mode = "lease"
```

现有单一 `[model]` 和 `[agent]` 配置在迁移期映射为 `models.profiles.default` 和 `agents.profiles.default`，避免破坏已有 CLI。

实际实现：上述 `[multi_agent]`、`[multi_agent.team_budget]`、`[models.profiles.*]`、`[agents.profiles.*]` 均未实现（计划中），Profile 不能通过配置定义，`[model]`/`[agent]` 保持原样。实际只有 `[agents]` 段：

```toml
[agents]
max_running = 4            # 同时运行的子 Agent 上限（root 不计）
max_live = 16              # 非终态子 Agent 上限
max_depth = 3
max_children = 4           # 每个父级的子 Agent 上限
model_limits = { }         # 按模型名限制并发；未列出的模型共享 max_running
default_workspace = "auto" # auto | snapshot | readonly | shared；auto 解析为 snapshot
auto_merge = false         # 无冲突快照合并是否免审批直接应用
auto_continue = true       # 子 Agent 结果到达时唤醒空闲 root
snapshot_copy_limit = "1GiB" # 无法 copy-on-write 克隆时单个快照的明文复制上限
```

`max_tasks`、上下文继承默认值、Team 预算均无配置项。

### 10.3 不可变快照

spawn 成功时固化：

```text
AgentProfileRevision
ModelProfileRevision
ToolRegistryGeneration
PolicySnapshotID
WorkspaceViewID
ContextSeed source range
Budget allocation
```

配置热更新只影响后续 spawn，不改变在途 Agent。

实际实现：spawn 时持久化 `ProfileRevision`、完整 `ProfileSnapshot` JSON（运行时以快照而非当前注册表派生模型、工具和限额）、请求的 `Workspace` 与授予的 `WorkspaceMode`、`ContextPolicy`；Workspace View 在 Run 启动时创建。`ModelProfileRevision`、`ToolRegistryGeneration`、`PolicySnapshotID`、ContextSeed source range、Budget allocation 未单独固化。

---

## 11. 上下文继承与结果回收

### 11.1 继承模式

`agent_spawn` 支持四种模式：

| 模式 | 输入 | 使用场景 | 风险 |
| --- | --- | --- | --- |
| `none` | 仅 profile instruction + task | 完全独立调查 | 可能缺少必要背景 |
| `summary` | durable 父上下文摘要 + task | 默认模式 | 摘要可能遗漏细节 |
| `recent` | 最近 N token 的已完成消息 + task | 需要近期原文 | token 成本较高 |
| `full` | 父 Thread 已完成历史快照 + task | 高耦合任务 | 成本高、扩大 prompt injection 面 |

约束：

- 默认 `summary`；
- 只复制 spawn 命令提交前已经完成的父消息，不复制在途 delta；
- `full` 仍受 Profile 的 `max_inherited_tokens` 限制；
- tool result 可按 artifact 引用替代大段原文；
- secret、审批凭据和仅宿主可见元数据永不进入继承上下文；
- ContextSeed 记录父 Thread、消息 sequence 范围、summary artifact checksum 和 tokenizer 计数。

实际实现（`internal/cli/conversation_builder.go` 的 child context seeder）：不生成模型摘要，各模式都是截取父 Thread 已持久化历史中最后若干条 user/assistant 文本消息（不含 tool 消息），按字符而非 token 限制：`summary` 最近 4 条/8000 字符，`recent` 最近 12 条/24000 字符，`full` 全部消息/48000 字符；结果标注为“不可信父上下文摘录”并前置到子 Agent 的 `InitialPrompt`。请求的继承模式不得高于子 Profile 的 `ContextInheritance`，未指定时为 `summary`。Profile 设置 `MaxInheritedTokens` 时再按 tokenizer 裁剪，但内置 Profile 均未设置。没有 ContextSeed 记录，只写 `thread.context_seeded` 事件；tool result 的 artifact 引用替代未实现。

### 11.2 独立压缩

每个 Thread 独立执行自动压缩和 `/compress`。父 Thread 压缩不会重写已经创建的 child ContextSeed；child 压缩也不会影响父级。

### 11.3 结果回收

child 结束时：

1. 写入 `agent.result_recorded`；
2. 将结构化 `AgentResult` 保存为小 payload 或 artifact；
3. 向父 Mailbox 投递 `result` 消息；
4. 唤醒正在 `agent_wait` 的父 Agent；
5. 父 Agent 的 tool result 只包含结构化摘要、状态和 artifact/thread 引用；
6. 父级需要细节时使用 `agent_inspect` 或切换 Thread，而不是自动灌入完整 transcript。

这与“共享一个大上下文”相比，可控制 token 成本并保留可审计性。

实际实现：终态迁移事务内写入 `agent.result_recorded`（canceled 以外另写 `agent.run_completed`/`agent.run_failed`），`AgentResult` 作为小 payload 存于 `agents` 投影（不写 artifact）；随后向父 Agent 投递 `result` 类型 Mailbox 消息（幂等键 `result:<runID>`）并 reconcile WaitSet 唤醒等待中的父级。`agent_inspect` 与切换 Thread 未实现；父级只能通过 `agent_list`/`agent_wait` 的结构化结果获取信息，用户可在 TUI `/agents` 面板查看子 transcript，`artifact_read` 仅读取已存在的 artifact 引用。

---

## 12. Supervisor API 与 Agent Control Tools

### 12.1 Go API

```go
type Supervisor interface {
    CreateTeam(ctx context.Context, command CreateTeamCommand) (Team, error)
    Spawn(ctx context.Context, command SpawnCommand) (Agent, error)
    ListAgents(ctx context.Context, query AgentQuery) ([]AgentSnapshot, error)
    Wait(ctx context.Context, command WaitCommand) (WaitResult, error)
    Send(ctx context.Context, command SendMessageCommand) (Message, error)
    Interrupt(ctx context.Context, command InterruptCommand) error

    CreateTask(ctx context.Context, command CreateTaskCommand) (Task, error)
    UpdateTask(ctx context.Context, command UpdateTaskCommand) (Task, error)
    ListTasks(ctx context.Context, query TaskQuery) ([]TaskSnapshot, error)

    Snapshot(ctx context.Context, teamID TeamID) (TeamSnapshot, error)
    Subscribe(ctx context.Context, cursor Cursor) (<-chan Event, error)
}
```

所有 command 包含：

```go
type CommandMeta struct {
    ActorAgentID  AgentID
    CorrelationID string
    CausationID   string
    IdempotencyKey string
    ExpectedVersion uint64
}
```

实际实现：`agentmanager.Supervisor` 是具体 struct，没有接口、`CreateTeam`（Team 由 `EnsureRootThread` 创建）、`UpdateTask` 和 `Subscribe`（客户端按 cursor 轮询事件）。主要导出方法：`Spawn`、`Continue`、`ListAgents`、`Wait`、`Send`、`AgentMailbox`、`Interrupt`（只有取消语义）、`CreateTask`、`TransitionTask`、`ClaimTask`、`ReleaseTask`、`AddTaskDependency`、`RemoveTaskDependency`、`GetTask`、`ListTasks`、`LoadTeamSnapshot`（对应 `Snapshot`）、`ListWorkspaceViews`、`ListWorkspaceLeases`、`ListMerges`、`RunPendingMerges`、`RequeueResolvedMerge`、`ReviewMerges`、`DiscardMerge`、`Suspend`、`ResolveWaitSet`、`ReconcileWaitSets`、`Recover`、`Close`。`CommandMeta` 未实现：Actor、CorrelationID、IdempotencyKey、Expected 版本分散在各 spec/transition 结构中。

### 12.2 模型可调用工具

第一版公开：

```text
agent_spawn
agent_list
agent_wait
agent_message
agent_interrupt
task_create
task_update
task_list
```

后续可增加：

```text
agent_inspect
task_claim
task_release
artifact_read
```

实际实现：已公开 `agent_spawn`、`agent_list`、`agent_wait`、`agent_message`、`agent_interrupt`、`agent_continue`（让已完成/失败的子 Agent 在同一 Thread 继续新指令）、`task_create`、`task_list`、`task_update`（版本化计划字段编辑）、`task_refine`（增量细化）、`task_reassign`（安全失败重指派）、`task_transition`（状态迁移）、`task_verify`（reviewer 提交验证结论）、`task_claim`、`task_release`、`task_add_dependency`、`task_remove_dependency`、`merge_list`、`merge_run`、`merge_requeue`、`artifact_read`（`internal/app/artifacts.go`，单次最多 1 MiB）。`agent_inspect` 未实现。同一工具面另有 Flow 与运行时工具 `flow_list`、`flow_run`、`runtime_budget`、`human_confirm`、`runtime_diagnostics`，不属于本文 Agent control tools。

### 12.3 工具语义

**`agent_spawn`**

- 输入：task、profile、context mode、task ID、workspace scope、budget hint；
- 输出：AgentID、ThreadID、初始状态；
- 成功表示 spawn 已耐久接受，不表示 child 已完成；
- 使用模型 tool-call ID 作为默认 idempotency key，避免恢复后重复创建 child。

**`agent_wait`**

- 支持 `all`、`any`、指定 Agent 集合和 deadline；
- 等待时父 Agent 状态为 `waiting(children)`，不占 model concurrency slot；
- 条件未满足时创建 durable wait set，并通过 Eino interrupt/checkpoint 挂起当前 turn，而不是让 worker goroutine 长时间阻塞；
- 条件满足后由 Scheduler 将父 Agent 重新排队，并以同一个 tool call ID ResumeTurn；
- deadline 到期返回当前快照，不自动取消 child；
- child 终态结果按稳定顺序返回。

**`agent_message`**

- durable enqueue 后即返回 MessageID；
- 支持 directive、question、answer、context 和 status 类型；
- 消息不会插入正在生成的 model token stream，只在安全边界注入下一 turn，除非同时显式 interrupt。

**`agent_interrupt`**

- `steer`：取消当前 model generation，保留 Agent，追加指令后重新排队；
- `cancel`：进入结构化取消并向下传播；
- 不能假装已经撤销一个已提交的外部副作用。

**Task 工具**

- 只提交 Task Graph 命令，不直接启动 goroutine；
- dependency 变更必须通过环检测和版本检查；
- Agent 只能修改授权范围内的任务字段。

实际实现：
- `agent_spawn` 参数为 `prompt`、`profile`、`task_id`、`context_policy`、`workspace`（访问请求，只有 `auto` 与 `readonly`；授予的模式由 Profile 与 `[agents].default_workspace` 决定）、`idempotency_key`；workspace scope 与 budget hint 未实现；`idempotency_key` 缺省取 tool-call ID，与设计一致。
- `agent_wait` 参数为 `agent_ids`（只能是当前 Agent 的直接子级）、`any`、`timeout_ms`。子 Agent（有 RunID）调用时创建 durable WaitSet 并挂起 turn，满足后重新排队并以 resume token 恢复；root Agent 没有 RunID，走阻塞式 `Supervisor.Wait`，不进入 `waiting` 状态。
- `agent_message` 参数为 `recipient_agent_id`、`type`、`body`（必须是 JSON）、`idempotency_key`；只允许父子之间通信；类型另含 `result` 与 `control`。注入时机见 §15.3。
- `agent_interrupt` 只有 `agent_id` 参数，语义为 `cancel`，并递归中断全部非 terminal 后代（见 §1 决策 6）；`steer` 未实现，可用 `agent_message` 加 `agent_continue` 近似。
- Task 工具：dependency 变更经递归 CTE 环检测与版本检查；没有字段级授权，`task_transition` 需要满足状态机及持有者检查。

### 12.4 Tool Pipeline 集成

Agent control tools 也注册为 `tool.Binding`：

```text
model tool call
  -> einoadapter actor metadata
  -> tool.Pipeline
  -> policy decision
  -> optional approval
  -> agentmanager ToolAdapter
  -> durable Supervisor command
  -> structured tool result
```

不能因为它们是“内部工具”就绕过 Policy、审计和幂等。

---

## 13. 结构化并发、调度与预算

### 13.1 取消树

```text
process context
  -> Team context
      -> Agent context
          -> Run context
              -> model/tool context
          -> child Agent contexts
```

规则：

- 进程信号触发 checkpoint/recovery 流程，不直接把 durable Agent 标成 canceled；
- 用户取消 Team 时向下传播 cancel；
- 父 Agent `cancel` 默认递归取消 child；
- 父 Agent `steer` 不自动取消 child，但新指令可以显式选择 child；
- tool timeout 只取消该调用，是否使 Agent 失败由 retry policy 决定；
- cancel 后的晚到 worker 结果用 RunID fencing 拒绝，不能覆盖新 Run。

实际实现：没有 Team context 层；每个子 Run 的 context 派生自 Supervisor context，并按 Profile `MaxWallTime` 以 Agent 创建时间为起点设置 deadline。`Interrupt` 递归中断全部非 terminal 后代；Team 取消由 `CancelTeam` 中断全部子 Agent 并等待其终止（§9.1）；`steer` 未实现；`Supervisor.Close` 取消全部非 root Agent。进程重启时 running 子 Agent 回到 `queued` 并从原 RunID checkpoint 恢复，canceling 子 Agent 直接进入 `canceled`。tool timeout 后的 retry policy 未实现。晚到结果通过 `Expected` 版本 CAS 与 RunID 拒绝。

### 13.2 Durable Suspension

`waiting(children)`、`waiting(approval)`、`waiting(mailbox)` 和较长的 `waiting(rate_limit)` 必须是 durable suspension：

1. 先记录 wait condition、resume token 和 Agent 状态；
2. 请求 Eino 在安全节点创建 checkpoint/interrupt；
3. worker 返回并释放 run/model slot；
4. 外部事件满足条件后，Supervisor 写入 wait resolved event；
5. Scheduler 创建新 `RunID`，使用原 Thread/turn snapshot 调用 ResumeTurn；
6. 原 tool call ID 保持不变，恢复结果按幂等协议交给 Runner。

进程内 channel 只用于降低唤醒延迟，不能作为 wait condition 的事实来源。正在执行且无法安全中断的副作用工具不进入 suspension；它只能完成、超时、被 sandbox 终止或进入 `tool.outcome_unknown` 核对流程。

实际实现：只有子 Agent 的 `agent_wait` 使用 durable suspension（`wait_sets` 表，状态 pending/resolved/expired/canceled/consumed，`any` 标志，事件 `wait_set.*` 与 `agent.wait_started`/`agent.wait_resolved`）。恢复时不创建新 RunID，而是沿用原 RunID，并以 WaitSet 的 resume token 恢复原 turn；WaitSet 消费后才继续。`waiting(approval)`、`waiting(mailbox)`、`waiting(rate_limit)` 未实现：审批在 turn 内同步阻塞，root 的 `agent_wait` 也是阻塞等待。

### 13.3 并发限制

限制分层：

| 层级 | 示例限制 |
| --- | --- |
| 进程 | 最大 live/running Agent、最大 goroutine worker |
| Team | 最大 Agent 数、深度、任务数、总 token/cost/time |
| 父 Agent | 最大直接 child、最大未收割 child |
| Provider | 最大并发请求、RPM、TPM、backoff |
| Model Profile | 最大并发、context window、输出上限 |
| Workspace | 最大 writer、路径 lease 冲突 |
| Agent | turn、iteration、tool-call、token、wall time |

建议默认值：

```text
max_live_agents      = 16
max_running_agents   = 4
max_depth            = 3
max_children         = 4
max_tasks            = 128
max_pending_approvals= 32
```

默认值必须可配置，并由 hard safety ceiling 限制。

实际实现：`[agents]` 默认 `max_running = 4`、`max_live = 16`、`max_depth = 3`、`max_children = 4`（只计子 Agent，root 不计），另有 `model_limits` 按模型名限制并发；未配置时 `agentmanager.NewSupervisor` 的回退值为 running 1、live 16、depth 3、children 4（chat 路径从 `[agents]` 传入实际值）。`max_tasks`、`max_pending_approvals`、Provider RPM/TPM、最大 writer 数配置与 hard safety ceiling 未实现；写共享工作区（shared-lease）的子 Agent 同时只运行一个。

### 13.4 Scheduler

Scheduler 使用带优先级的 weighted fair queue：

1. 过滤 dependency、budget、approval、workspace lease 未满足的 Agent；
2. 按用户交互、关键路径、普通 worker、后台验证确定优先级；
3. 在 Team 间公平轮转，避免一个 Team 占满进程；
4. 检查 provider/model bucket；
5. 分配 RunID 和 slot；
6. 提交 `agent.run_admitted` 后启动 worker；
7. rate limit 时释放 model slot，进入带 jitter 的 durable backoff。

等待 child、approval 或 mailbox 的 Agent 不占 model slot。执行本地工具是否占 agent slot 由工具类型决定，但始终计入 Team wall time 和 tool-call budget。

实际实现（`agentmanager/supervisor_scheduler.go` + `internal/scheduler`）：
1. 候选为各 Team 中 `queued` 且父级未进入 canceling/terminal 的子 Agent；Team 预算耗尽（`CanAdmit`）时整 Team 跳过；
2. `scheduler.RoundRobin` 按 Team 轮转（旧 WeightedRoundRobin 为兼容别名）：Team 按名称排序后以游标轮转，Team 内按 `Priority` 降序、ID 升序；Agent 的 Priority 实际恒为 0（Task 的 `priority` 不传给调度器），关键路径/用户交互优先级未实现；
3. 运行槽位（`max_running`）、shared-lease 写入者互斥（同时一个）、模型并发桶（`BucketSet`，键为 Profile 的 `ModelProfile`，限额来自 `model_limits`）三重检查；
4. CAS 迁移到 `running` 时同事务写入 `agent.run_admitted` 与 `agent.run_started`，随后启动 worker；
5. Provider RPM/TPM、rate limit durable backoff 未实现。
等待中的子 Agent 释放运行槽位；tool-call 上限按 Profile `MaxToolCalls` 计，Team 级 wall time/tool-call 预算未实现。

### 13.5 Budget

预算是层级化的 reservation + settlement：

```text
process ceiling
  -> Team allocation
      -> Agent allocation
          -> model request reservation
```

- 调用前按最大输出和估算输入预留；
- provider 返回 usage 后按真实值结算；
- cache read/write token 单独记录，不与普通 input token 混淆；
- provider 未返回的字段标记 `unavailable`，不能填零冒充；
- 预算达到 warning 阈值发事件；达到 hard limit 时停止新 admission，并允许在有限范围内生成收尾结果。

实际实现：只有 Team 一层账户（`team_budgets`，限额为 input/output/total token 与 cost micros），没有 process ceiling 与 Agent allocation。每次模型请求前按 tokenizer 估算输入加请求的输出上限写入 `usage_reservations`（无 TTL，启动时 `RecoverReservations` 回收），返回后按 provider usage 结算到 `usage_records`；预算耗尽时 Team 不再被调度（`CanAdmit`），在途请求不受影响。`tokenusage.Snapshot` 只有 `Input`、`Cached`、`Output`、`Reasoning`、`Total`：cache read/write 未分开，未返回字段不标 `unavailable`，改为在记录上用 `Quality` 字符串区分 `provider`/`estimated_reservation`，配置了价格表但查不到价格时追加 `_unpriced`。warning 阈值、`budget.*` 与 `usage.recorded` 事件未实现。配置入口是 `[usage]`：`[usage.prices."provider/model"]` 的 `input/cached_input/output/reasoning_micros_per_million` 装配为 `agentruntime.Config.Pricing`（未配置价格时 cost 为 0）；`[usage.budget]` 的 `max_input_tokens`/`max_output_tokens`/`max_total_tokens`/`max_cost_micros` 在每次 `EnsureRootThread` 时经 `ConfigureBudget` 写入该会话的 Team，全部为 0 时清除预算（配置是 Team 预算的唯一来源）。

### 13.6 委派收益判断

默认不对每个任务自动 spawn。自动编排器在以下条件满足时才建议委派：

- 至少有两个相互独立或低冲突的工作单元；
- 子任务的上下文隔离能减少主线程噪声；
- 存在独立测试/审查价值；
- 预计并行节省大于 spawn 和结果整合成本；
- Team 预算和 provider capacity 足够。

负面信号：任务小、强顺序依赖、同一小文件高冲突、需要连续共享推理、剩余预算不足。模型可以提出拆分，但 Supervisor 保留准入权。

实际实现：自动编排器与委派收益判断未实现（计划中）；是否 spawn 完全由模型决定，Supervisor 只做配额、深度、Profile 与预算准入。

---

## 14. Task Graph 与复杂任务编排

### 14.1 DAG 规则

- Task Graph 必须是有向无环图；
- 每次增加 dependency 使用增量环检测；
- Task 创建和 dependency 更新使用 optimistic version；
- Task 的 acceptance criteria 在 claim 前冻结，变更会产生新 revision；
- 子任务可以由模型建议，Supervisor 验证后提交；
- 删除使用 tombstone event，已经被引用的 Task 不做物理删除。

实际实现：dependency 增加时用 SQLite 递归 CTE 做环检测，Task 迁移与 dependency 更新使用 `Expected` 版本；依赖全部 `completed` 后被依赖任务自动从 `blocked` 解除。acceptance criteria 与 revision、Task 删除/tombstone 未实现（只有 `canceled` 状态，dependency 可移除）。

### 14.2 Claim 与重分配

Task claim 包含 `AgentID`、`RunID`、`LeaseUntil` 和 `Version`：

- Agent 进入 `running` 前必须成功 claim 任务；
- heartbeat 只续租 active Run 的 claim；
- 进程崩溃后 claim 不立即归零，恢复流程先检查 checkpoint；
- Agent terminal 后 claim 释放；
- 可重试失败使 Task attempt +1 后回到 `ready`；
- 超过 max attempts 后进入 `failed`；
- 重分配保留每次尝试的结果和失败原因。

实际实现：claim 不是独立结构，而是 `tasks` 上的 `assignee_agent_id` 与 `claim_expires_at` 列（固定 30 分钟），没有 RunID 与 heartbeat 续租；`agent_spawn` 携带 `task_id` 时在同一事务内创建子 Agent 并 claim 任务，模型也可显式 `task_claim`/`task_release`。过期 claim 只在持有 Agent 已不存活时回到 `ready` 并 `attempt + 1`；Agent 取消时 claim 释放、任务回到 `ready`；Agent 失败时任务直接 `failed`（终态）。最多 3 次总执行；可信的 iteration-limit 失败可在无副作用未知、无存活后代、无未清理资源和无 merge 记录时安全重分配。尝试历史保留在 `orchestration_events` 和 Agent completion receipt 中。另有 `dispatchReadyTasks`：子 Agent 完成后，Supervisor 先回收过期 claim，再把未指派的 `ready` 任务自动派给新的子 Agent（Profile 取 `required_profile`，幂等键 `task-dispatch:<taskID>:<attempt>`）。

### 14.3 验证 Gate

复杂实现任务推荐：

```text
worker result
  -> tester task
      -> reviewer task
          -> accepted/completed
          -> revision task -> worker
```

Gate 是 Task Graph 的显式节点，不是 prompt 中一句“请检查”。验证结果使用结构化字段记录命令、exit code、测试范围、发现和 artifact。

实际实现：Gate 不是独立 Task 节点，而是 Task 的 `verification` 状态加 `verification_profile` 字段。worker 完成设置了 `verification_profile` 的任务后，任务进入 `verification`，Supervisor 自动派生一个使用该 Profile 的 reviewer 子 Agent；reviewer 调用 `task_verify`（`passed`、`reason`），且它必须是当前 Task attempt 指派的 reviewer，Profile 与 `verification_profile` 一致，提交时核验 Task version：通过则进入 `completed`（快照候选未合入时进入 `awaiting_merge`），不通过则回到 `ready` 并递增 attempt，旧 reviewer 不能为新 attempt 投票。验证结果只记录 `reason` 文本，没有命令、exit code、测试范围等结构化字段；tester 节点与 revision Task 未实现。`internal/app/builtins.go` 的 DefaultDeny 策略以 `allow-task-verify` 规则放行 `task_verify`，是否接受结论由 Supervisor 按 reviewer 身份与 Task 状态判断。

### 14.4 自适应细化

运行中允许：

- 将过大的 Task 拆成子 DAG；
- 根据探索结果增加依赖；
- 因模型/工具失败切换 Profile；
- 将高冲突任务从 shared lease 转为 worktree；
- 为失败验收条件创建 revision Task；
- 根据关键路径提高优先级。

所有细化都通过命令和事件发生，不能直接修改内存图。

实际实现：模型可通过 `task_update` 编辑未执行计划，通过 `task_refine` 原子增量添加细化成员和依赖；宿主执行版本、权限、幂等和循环校验。普通 `ParentTaskID` 仍是分组，v40 `task_refinement_members` 才形成父完成等待边。Profile 切换、shared lease 到 worktree 的迁移、revision Task 与关键路径优先级均未实现。快照合并冲突时自动创建的 Resolution Task（见 §17.5）是唯一由 Supervisor 自动生成的任务。

---

## 15. Mailbox 与 Agent 通信

### 15.1 消息模型

```go
type Message struct {
    ID              MessageID
    TeamID          TeamID
    SenderAgentID   AgentID
    RecipientAgentID AgentID
    Type            MessageType
    Body            json.RawMessage
    CorrelationID   string
    CreatedAt       time.Time
    Sequence        uint64
    DeliveryState   DeliveryState
}
```

MessageType：

- `directive`：改变后续执行方向；
- `question`：请求信息；
- `answer`：回应指定问题；
- `context`：提供 artifact/路径/事实；
- `status`：非终态进度；
- `result`：child 结构化终态结果；
- `control`：Supervisor 生成的恢复或取消通知。

实际实现：`orchestration.Message` 另有 `IdempotencyKey`、`DeliveredAt`、`AcknowledgedAt`，以及消费账本字段 `ConsumerTurnID`、`ConsumerRequest`、`BoundAt`（迁移 v36）。七种 MessageType 均已定义；`result` 由 Supervisor 在子 Agent 终态时投递，`control` 由 `agent_continue` 用于携带续作指令（body 为 `{"prompt": ...}`）；这两类消息可发给或来自 terminal Agent，其余类型对 terminal Agent 返回错误。Body 必须是 JSON，上限 1 MiB。

### 15.2 投递语义

- 对单个 recipient 按 mailbox sequence 有序；
- 至少一次投递；
- 通过 MessageID 去重；
- `enqueued` 表示已耐久保存；
- `delivered` 表示已进入 Agent 的安全输入边界；
- `acknowledged` 表示 Agent runtime 已接收，不表示模型已遵循；
- terminal Agent 不接受普通 directive，返回 typed error；
- 父 Agent 的 result mailbox 不因 UI 未打开而丢失。

### 15.3 安全输入边界

Mailbox 消息只在以下时机注入：

1. 新 turn 开始前；
2. Agent 显式 `agent_wait` 返回时；
3. 当前 generation 被 steer/cancel 后恢复时。

不在模型 token stream 中间拼接文本，避免上下文损坏和 provider 行为不确定。

实际实现：
- 子 Agent：只在 Run 开始时（新 Run，非 checkpoint 恢复/WaitSet 恢复）读取 `enqueued`/`delivered` 消息并标为 `delivered`，Run 成功结束后 `acknowledged`；运行中到达的消息留待下一次 Run（`agent_continue` 或任务指派），恢复 Run 不注入新消息。
- root Agent：使用消费账本（`AgentMailbox`，`ClaimMessages`/`AcknowledgeTurn`/`ReleaseTurn`）。每次主模型请求前把可用消息绑定到当前 turn 与请求序号，作为“不可信背景数据”块插入模型输入（不在 token stream 中间拼接）；turn 提交后确认，未提交则释放。root 空闲且 `[agents].auto_continue` 为 true 时，TUI 以子结果为输入发起 wake turn（headless/control 不发起）。重启后先 `SettleBackground` 结算崩溃前已绑定的消息。
- steer 恢复时注入未实现。

---

## 16. 权限、审批与安全边界

### 16.1 权限派生

```text
EffectivePermissions(child)
  = GlobalPolicy
  ∩ TeamPolicy
  ∩ DelegablePermissions(parent)
  ∩ AgentProfileMaximum
  ∩ TaskScope
  ∩ WorkspaceViewCapabilities
```

任一层拒绝即拒绝。不存在“父 Agent 是 root，所以 child 自动全权限”。

实际实现：只有 `GlobalPolicy`（`app/builtins.go` 默认 DefaultDeny + 显式 allow/require-approval 规则，capability 为 `tool.invoke`）∩ `agentprofile.ConstrainDelegation(parent, child)`（工具 allowlist 与父级 `DelegableTools` 求交，`CanSpawn`/`CanManageTasks`/`CanSendMessages` 逐项与父级相与，只读父级不能委派可写 workspace，Limits 与父级逐项取交集，支持显式 snapshot，结果持久化）∩ Profile 自身的 `AllowsTool`。`TeamPolicy`、`TaskScope`、`WorkspaceViewCapabilities` 未实现（workspace 只读由工具层按 view 模式拒绝写入）。`merge_run`/`merge_requeue` 规则为 require-approval；`task_verify` 由 `allow-task-verify` 规则放行。

### 16.2 Actor Scope

`tool.Call`、`policy.Request` 和 approval event 增加：

```go
type ActorScope struct {
    TeamID       TeamID
    AgentID      AgentID
    ThreadID     ThreadID
    TaskID       TaskID
    RunID        RunID
    ParentAgentID AgentID
    ProfileRevision string
}
```

这使审批 UI 能明确显示“哪个 Agent、为哪个 Task、在什么 workspace view、请求执行什么”。

实际实现：`ActorScope` 定义在 `internal/orchestration/id.go`，字段与上面一致；通过 `orchestration.WithActorScope` 放入 context，`policy.Request.Actor` 携带它。TUI 审批框的来源标签为“profile 短 ID · task 标题”，不显示 workspace view。

### 16.3 中央审批

审批由 durable approval projection + 进程内 Broker 组成：

- durable projection 是事实来源；
- Broker 只负责唤醒等待 goroutine；
- UI 重启后从 projection 恢复 pending request；
- 请求按用户关注度、风险和时间排序；
- 同意/拒绝在审批区域内上下选择，结果明确显示；
- 拒绝是正常业务结果，tool result 返回 `approval_denied`，不把整个 Team 变成不可解释的框架异常；
- Agent cancel 会取消相关 pending approval；
- headless 模式默认 fail closed，除非显式提供 approval channel。

现有 `policy.Approver.Approve(...) (bool, error)` 是同步阻塞接口。Multi-Agent 最终契约应拆为：

```go
type ApprovalService interface {
    Request(ctx context.Context, request Request) (ApprovalDisposition, error)
    Resolve(ctx context.Context, command ResolveApprovalCommand) error
}

type ApprovalDisposition struct {
    State      ApprovalState
    ApprovalID ApprovalID
    ResumeToken string
}
```

`approved`/`denied` 可以立即返回；`pending` 返回 durable resume token，并触发 13.2 节的 checkpoint suspension。`approval.Broker` 保留为 UI notification/wakeup bridge，但不再让 production worker 通过 channel 阻塞等待。迁移期可以保留同步 adapter 供现有单 Agent headless 测试使用。

实际实现（durable 审批未实现）：
- 仍是同步 `policy.Approver.Approve(...) (bool, error)`；TUI 使用进程内 `approval.Broker`（缓冲 16），所有 Agent 的请求进入同一审批区域，选项只有 Allow/Reject；没有 durable approval projection、排序、过期、重启恢复和 `ApprovalService`。审批只在 session events 中记录 `approval.requested`/`approval.resolved`（`tool/pipeline.go`）。
- 拒绝返回 `ErrApprovalDenied`，模型侧收到 `{"error":"tool call approval denied",...}` 的 tool result。
- 非交互（headless/control）使用 `DenyApprover` fail closed；evaluation 模式使用工具 allowlist（write/edit/shell/`merge_apply` 或调用方指定的工具）。
- snapshot 合并的 `merge_apply` 审批由 merge 流程直接调用 Approver，不经过工具 pipeline。
- Agent cancel 通过 context 取消使阻塞中的审批返回，不存在单独的 approval cancel 事件。

### 16.4 Approval Scope

批准结果只适用于：

```text
normalized tool name
normalized arguments digest
AgentID/TaskID scope
WorkspaceViewID
policy rule revision
expiration
```

如果支持“本会话持续允许”，也必须生成一条有明确路径/命令/时间范围的临时 policy rule，而不是缓存一个布尔值。

未实现：每次审批只对当次调用生效，没有 approval scope 记录，也没有“本会话持续允许”。

### 16.5 威胁模型

| 威胁 | 控制 |
| --- | --- |
| prompt injection 要求无限 spawn | depth、fan-out、budget、admission 和权限上限 |
| child 充当 confused deputy | 非升级权限交集、actor scope、独立审批 |
| Mailbox 伪造父级指令 | 宿主生成 sender identity，模型不能自填 |
| symlink/path traversal | 复用 canonical workspace 路径校验，lease 基于规范路径 |
| 多 Agent 静默覆盖文件 | lease、fingerprint、worktree、merge conflict |
| tool 重放造成重复副作用 | idempotency key、tool outcome unknown、reconciliation |
| 子 Agent 泄漏 secret | secret 不进入上下文，ModelGateway 独占凭据 |
| 资源耗尽 | 分层预算、有界队列、hard ceiling |
| 恶意/失控 child 阻止父级完成 | deadline、cancel propagation、forced terminalization |

---

## 17. Workspace 协调与冲突处理

### 17.1 Workspace View

支持三种模式：

| 模式 | 行为 | 用途 |
| --- | --- | --- |
| `shared-readonly` | 共享根目录，只读 | explorer/reviewer |
| `shared-lease` | 共享根目录，写入前获取 lease | 小范围、低冲突修改 |
| `git-worktree` | 独立 worktree，结果进入 merge queue | 大修改或高并行度 |

Profile 给出最大能力，Task 可以进一步收窄。

实际实现：
- 共有四种模式，新增 `snapshot`：从主 workspace 克隆（macOS clonefile / Linux FICLONE，不支持时复制，受 `snapshot_copy_limit` 限制）出的私有副本，不依赖 Git，结果作为 snapshot 候选进入 merge queue。snapshot view 位于 `<DataDirectory>/views`，git worktree 位于 `<workspace>/.gsea/worktrees/<viewID>`。
- 授予规则（`grantWorkspaceMode`）：`agent_spawn` 的 `workspace=readonly` 总是得到 `shared-readonly`；Profile 为 `shared-readonly` 或 `git-worktree` 时保持该模式；其余写入者得到 `[agents].default_workspace` 解析出的 writer 模式（`auto` 在有 snapshot store 时为 `snapshot`，否则 `shared-lease`；另有 `snapshot`/`readonly`/`shared`）。内置 Profile 都不声明 `git-worktree`，因此 git-worktree 路径只在自定义 Profile 中可达。
- Task 不能收窄 workspace 模式；只读 view 中写工具直接返回 “workspace view is read-only”。reviewer 审查 snapshot 候选时得到该 snapshot 的只读 view。

### 17.2 路径 Lease

精确工具在执行前提交 write intent：

```go
type WriteIntent struct {
    AgentID        AgentID
    TaskID         TaskID
    WorkspaceViewID WorkspaceViewID
    Paths          []CanonicalPath
    Prefixes       []CanonicalPath
    Baseline       map[CanonicalPath]Fingerprint
    TTL            time.Duration
}
```

冲突规则：

- 同一路径或父子前缀的 writer lease 冲突；
- read 不阻塞 read；
- 默认 read 不阻塞 writer，但读取结果记录 fingerprint；
- 写入前对依赖读取文件执行 optimistic fingerprint 校验；
- 多路径 lease 按规范路径排序后原子获取，防止死锁；
- Agent/Task 终态时释放 lease；
- lease 等待有 deadline，超时返回可解释 conflict。

实际实现（`internal/workspacedomain/leases.go`）：`WriteIntent` 未实现。lease 是单路径、单次操作级的：write/edit 工具在 `shared-lease` view 中对目标文件取 write lease，执行完立即释放；git-worktree/snapshot view 不取 lease。模式只有 `read`/`write`，但生产代码只取 write lease（read 不阻塞 read 的规则只在 store 中实现）。冲突为同一路径、父子前缀或整 workspace（`__workspace__`）重叠，路径比较大小写不敏感；冲突时以 10–250 ms 退避轮询，工具默认最多等 10 秒，超时错误说明持有者。lease 存于 GSEA_HOME 下 `workspaces/<id>/coord.db`（同一 GSEA_HOME 的多个实例共享），没有 TTL；持有者存活由 instance flock 判断，退出实例的 lease 在获取时或 `ReapAbandonedLeases` 中回收。没有 fingerprint 校验、多路径原子获取或 Agent/Task 终态释放（操作结束即释放）。

### 17.3 Shell

Shell 无法总是可靠推断写集合，因此：

- 明确只读命令可在 shared view 运行；
- 声明 `write_paths` 的命令获取对应 lease；
- 未声明且可能写入的命令获取 workspace-wide writer lease；
- 高并发场景优先在独立 worktree 运行；
- sandbox 继续限制路径和进程能力，lease 不能替代 sandbox。

实际实现：没有只读命令识别和 `write_paths` 声明；`shared-lease` view 中每条 shell 命令都在整 workspace write lease 下执行，只读 view 中 shell 被拒绝。此外 Supervisor 同时只运行一个 `shared-lease` 写入者（13.3）。

### 17.4 Fingerprint

Fingerprint 至少包含：

```text
canonical path
existence
file type
size
mtime (仅作快速判断)
content hash (最终判断)
optional git blob ID
```

mtime 不能作为唯一冲突依据。

实际实现：fingerprint 只用于 snapshot 合并，为 `workspacecoord.ManifestEntry`（path、type、归一化 mode 0644/0755、size、sha256、symlink target），不使用 mtime 和 git blob ID；整树 manifest 的 digest 作为 candidate/plan 身份。shared-lease 写入不做 fingerprint 校验。

### 17.5 Git Worktree 与 Merge Queue

流程：

1. Coordinator 从基线 commit 创建私有 worktree；
2. child 只获得该 Workspace View；
3. Agent 结果记录 base commit、diff artifact 和验证结果；
4. merge queue 在主 workspace 上串行处理；
5. 基线未变化时应用 patch；
6. 基线变化时执行三方合并或标记 conflict；
7. 合并后运行配置的验证 gate；
8. 成功后更新 Task，失败则创建 resolution Task；
9. worktree 在结果和 artifact 持久化后回收。

第一版不要求 Agent 自动 commit；Coordinator 可以生成和应用 patch。任何自动提交或分支策略必须由项目配置明确开启。

实际实现（`internal/workspacecoord`）：merge queue 有 `git-patch` 与 `snapshot` 两类，状态 `pending/running/applied/conflicted/rejected/superseded`（`pending→running|rejected|superseded`，`running→applied|conflicted|rejected`，`conflicted→pending`）；不自动 commit。
- git-patch：git-worktree child 成功结束时 `deliverPatch`（`supervisor_delivery.go`）把 worktree 相对基线的 diff 存为 artifact 并入队 git-patch merge（幂等键 `merge:<viewID>:<runID>:<artifact>`，同一 worktree 的新 patch 取代其未处理的旧 merge），结果写入 `AgentResult.Delivery`，Task 与 snapshot 一样进入 `verification` 或 `awaiting_merge`。后台合并在 Task 进入 `awaiting_merge` 后执行：要求主 workspace HEAD 等于 merge 基线，除非 `auto_merge=true` 否则先审批 patch，在整 workspace 屏障下运行 `git apply --3way`（已应用的 patch 视为成功）；Task 已离开执行/审查/等待合并状态的 git-patch merge 保留给 `merge_run`（`Supervisor.RunPendingMerges`，仍由 MergeExecutor 的 Approver/AutoMerge 门禁授权，调用本身不构成审批）。冲突时转为 `conflicted` 并创建 Resolution Task（draft，由父级规划）；该 Task 完成后 merge 原地回到 `pending`。
- snapshot：child 完成后 `deliverSnapshot` 生成 candidate（manifest + 变更 artifact，结果写入 `AgentResult.Delivery`），Task 进入 `verification`（有 verification_profile 时）或 `awaiting_merge`。新 candidate 取代同一 Task 未处理的旧 candidate（`superseded`）。Supervisor 在投递后自动运行 snapshot 合并：Task 仍在执行/审查时等待，进入 `awaiting_merge` 后基于 candidate 基线、candidate 与当前主 workspace 做三方合并（`merge3.go`，文本行级合并使用 go-udiff v0.3.1），得到 `ApplyPlan`；有冲突则 `conflicted`。无冲突时除非 `auto_merge=true` 或 plan 为空，否则通过 `merge_apply` 同步审批（审批绑定 plan digest，审批期间主 workspace 变化则重建 plan 重新审批，最多 3 轮，超出后保持 running 等下一轮）；在整 workspace write lease 屏障下，经 apply journal（`merge_apply_journal`/`merge_apply_progress`，migration v35）提交，崩溃后由 `resumeSnapshot` 继续。提交时发现主 workspace 偏离 plan（`ErrApplyDiverged`）直接视为冲突；拒绝审批则 merge `rejected` 并使 Task 失败。没有 Approver（headless 且 `auto_merge=false`）时 snapshot merge 保持排队。
- 冲突时创建 Resolution Task（幂等键 `merge-resolution:<mergeID>`，`RequiredProfile=worker`，parent 为原 Task）；snapshot 冲突的 Resolution Task 直接置为 `ready`，在由 candidate 预置的 snapshot 中解决，产出取代原 merge 的新 candidate。
- 合并后检查（第 7 步）：配置 `[agents].post_merge_check`（shell 命令，环境变量 `GSEA_AGENTS_POST_MERGE_CHECK`；超时 `post_merge_check_timeout`，默认 10m）时，merge 进入 `applied` 后在主 workspace 的沙箱中运行一次该命令（`MergeVerifier`），通过则关联 Task（含被取代链上的 Task）置为 `completed`，失败则置为 `failed`（下游依赖保持阻塞），变更保留在主 workspace，不自动回滚；未配置或没有等待中的 Task 时直接 `completed`。snapshot merge 随后丢弃 snapshot view，git-patch merge 保留 worktree。worktree 池、所有权 lease、epoch/revoking 与 `shared_writes`（并行计划 P4）均未实现。

2026-10-26 审计补全：检查现为只读，在整个 workspace 屏障中执行，持久 finalization 回执同时绑定 merge 候选/patch/plan、主工作区内容和检查配置摘要（v37/v38）。派发前持久化 dispatched，恢复不自动重跑结果未知的命令；unknown、内容偏离或检查失败使 Task failed 并保持依赖阻塞。回执结算可重试，避免重复 apply 或验证。候选审查期间禁止对应 git view 的 Continue/reuse；授权要求 Task 当前 attempt 指派的 reviewer。完整边界见 [AUDIT_REMEDIATION.md](AUDIT_REMEDIATION.md)。

---

## 18. 事件协议

### 18.1 Event Envelope

```go
type Event struct {
    EventID          EventID
    GlobalSequence  uint64
    StreamType      string
    StreamID        string
    StreamSequence  uint64
    TeamID           TeamID
    Type             EventType
    OccurredAt       time.Time
    ActorAgentID     AgentID
    CausationID      string
    CorrelationID    string
    IdempotencyKey   string
    Payload          json.RawMessage
    Checksum         string
    SchemaVersion    uint32
}
```

`GlobalSequence` 用于 UI cursor 和跨 aggregate 投影；`StreamSequence` 用于 aggregate optimistic concurrency。

### 18.2 事件目录

**Team**

```text
team.created
team.draining_started
team.cancel_requested
team.completed
team.failed
team.canceled
team.reopened
```

**Agent/Run**

```text
agent.spawn_requested
agent.created
agent.queued
agent.run_admitted
agent.run_started
agent.run_failed
agent.run_completed
agent.activity_changed
agent.wait_started
agent.wait_resolved
agent.steer_requested
agent.cancel_requested
agent.result_recorded
agent.completed
agent.failed
agent.canceled
agent.recovery_scheduled
agent.recovery_failed
```

**Thread/Context**

```text
thread.created
thread.context_seeded
thread.turn_attached
thread.compacted
thread.closed
```

Thread 内具体 user/model/tool 事件继续进入现有 `session_events`，通过 Team/Agent/Thread metadata 关联。

**Task**

```text
task.created
task.updated
task.dependency_added
task.dependency_removed
task.ready
task.claimed
task.claim_renewed
task.claim_released
task.started
task.verification_started
task.completed
task.failed
task.canceled
task.result_attached
```

**Mailbox**

```text
mailbox.message_enqueued
mailbox.message_delivered
mailbox.message_acknowledged
mailbox.message_dead_lettered
```

**Workspace**

```text
workspace.view_created
workspace.lease_requested
workspace.lease_acquired
workspace.lease_released
workspace.lease_expired
workspace.conflict_detected
workspace.merge_queued
workspace.merge_started
workspace.merge_requeued
workspace.merge_resolution_linked
workspace.merge_applied
workspace.merge_conflicted
workspace.view_released
```

**Approval/Usage/Scheduler**

```text
approval.requested
approval.resolved
approval.expired
approval.canceled
usage.recorded
budget.warning_reached
budget.exhausted
scheduler.admission_deferred
scheduler.rate_limited
```

实际实现（2026-10-26，`internal/orchestration` 事件常量）：
- 生产代码会写入：`team.created`；`agent.created/queued/run_admitted/run_started/run_completed/run_failed/result_recorded/wait_started/wait_resolved/cancel_requested/completed/failed/canceled`；`wait_set.created/resolved/consumed/expired/canceled`（新增）；`thread.created/context_seeded`；`task.created/updated/dependency_added/dependency_removed/claim_expired`（`claim_expired` 为新增；ready/claim/start/verification/完成等状态变化统一记为 `task.updated`）；`mailbox.message_enqueued/delivered/acknowledged`，以及新增的 `message_bound/message_released`（root 消费账本）；`workspace.view_created/view_released/view_preserved`，`workspace.merge_queued/started/requeued/resolution_linked/applied/conflicted`，以及新增的 `merge_rejected/merge_superseded`；`flow.*`（agentflow，新增）。
- 已定义但从不写入：`team.draining_started/completed/failed`（取消与重开事件已有生产写入），orchestration 的 `approval.*`（审批只写 session event `approval.requested/resolved`），`workspace.lease_*`（常量为 requested/active/released/expired/conflicted），`usage.recorded`，`budget.warning_reached/exhausted`。
- 未定义：`agent.spawn_requested/activity_changed/steer_requested/recovery_scheduled/recovery_failed`，`thread.turn_attached/compacted/closed`，`task.ready/claimed/claim_renewed/claim_released/started/verification_started/completed/failed/canceled/result_attached`，`mailbox.message_dead_lettered`，`workspace.conflict_detected`，`scheduler.*`。

### 18.3 Payload 演进

- 每个 event type 有独立 versioned payload struct；
- 新增 optional 字段保持向后兼容；
- 删除/改义字段必须新增 event version 或新 event type；
- projector 必须能重放所有仍受支持版本；
- 未知 event type 在读模型中保留但不解释，不能破坏 event checksum；
- migration 不原地重写历史 payload，必要时追加 correction event。

### 18.4 Transactional Outbox

每个命令在一个 SQLite transaction 内：

1. 校验 idempotency key 和 expected version；
2. 追加 orchestration event；
3. 更新 projection；
4. 写入 outbox/global sequence；
5. commit；
6. broadcaster 发布 committed event。

进程在 5 和 6 之间崩溃时，重启后 broadcaster 从 global cursor 补发。因此 UI 可能收到重复 event，必须按 EventID 去重。

实际实现：没有 outbox 表和 broadcaster。每个状态变化在一个 SQLite transaction 内校验 Expected version/幂等键、追加 `orchestration_events`（global sequence 即自增序号）并更新 projection 表；commit 后 Supervisor 只通过内存 `signal` 唤醒调度。TUI 和 headless 都按 cursor 轮询事件或重新加载快照，不存在推送与补发。projection 由命令直接维护，不从事件重放重建。

### 18.5 Session 审计与 Orchestration 事实的一致性

`session_events` 继续是模型历史和单 Thread 工具轨迹的事实来源；`orchestration_events` 是 Agent 生命周期、pending approval、Task、Mailbox、Budget 和 Workspace 协调的事实来源。两者职责不同，但一次工具状态转换可能同时影响两者，不能由调用方执行两个互不相关的 `Append`。

Phase 0 引入共享 SQLite `RuntimeUnitOfWork` 和窄接口 `tool.ExecutionRecorder`：

```go
type ExecutionRecorder interface {
    RecordProposal(ctx context.Context, fact ToolProposal) error
    RecordApprovalRequested(ctx context.Context, fact ApprovalRequest) error
    RecordApprovalResolved(ctx context.Context, fact ApprovalResolution) error
    RecordStarted(ctx context.Context, fact ToolStarted) error
    RecordSucceeded(ctx context.Context, fact ToolSucceeded) error
    RecordFailed(ctx context.Context, fact ToolFailed) error
}
```

实现规则：

- 需要跨域的记录在同一个 SQLite transaction 内追加 Session event、Orchestration event 并更新 projection；
- 两个 event 使用相同 `CorrelationID`/tool call ID，并互相保存 counterpart EventID；
- transaction commit 前不得通知 Broker、UI 或启动 executor；
- `tool.started` 必须先 commit 再执行副作用；结果在下一 transaction 中记录；
- 内存测试 store 实现同一语义，生产 Pipeline 不依赖 SQLite 具体类型；
- 迁移完成后，`tool.Pipeline` 不再对同一状态自行进行非原子双写。

这样既保留现有历史投影，又保证重启时 pending approval 和工具执行状态不会分裂。

未实现：没有 `RuntimeUnitOfWork` 和 `tool.ExecutionRecorder`。`tool.Pipeline` 仍只向 `session_events` 记录 proposed/policy_decided/approval/started/succeeded/failed；审批不是 durable 状态（16.3），因此也不存在需要跨两类事件对齐的 pending approval。

---

## 19. 数据库与迁移

### 19.1 Database Owner

当前 `sqlitestore.Store` 同时实现 EventStore 和 CheckpointStore，并拥有 `Close`。Multi-Agent 阶段应重构为：

```text
sqlitestore.Database            唯一连接池和迁移 owner
  ├─ SessionEventStore
  ├─ CheckpointStore
  ├─ OrchestrationEventStore
  ├─ ProjectionStore
  └─ ArtifactMetadataStore
```

子 store 不单独关闭数据库。`internal/app` 仍是唯一生命周期 owner。

实际实现：已重构为 `sqlitestore.Database`（唯一连接与迁移 owner），子 store 为 `SessionEvents()`、`Checkpoints()`、`Orchestration()`（orchestration 事件、projection、Task、Mailbox、WaitSet、merge、usage/budget 都在 `OrchestrationStore`）、`LSPDiagnostics()`、`Plugins()`、`RuntimeSnapshots()`、`Strategies()`。没有独立的 ProjectionStore/ArtifactMetadataStore；artifact 存在文件系统内容寻址存储（`sha256:<hex>`，单个 32 MiB 上限，无保留/回收策略）。

### 19.2 表结构

保留：

```text
session_events
agent_checkpoints
```

新增事实表：

```sql
CREATE TABLE orchestration_events (
    global_sequence INTEGER PRIMARY KEY,
    event_id TEXT NOT NULL UNIQUE,
    stream_type TEXT NOT NULL,
    stream_id TEXT NOT NULL,
    stream_sequence INTEGER NOT NULL,
    team_id TEXT NOT NULL,
    event_type TEXT NOT NULL,
    occurred_at TEXT NOT NULL,
    actor_agent_id TEXT NOT NULL DEFAULT '',
    causation_id TEXT NOT NULL DEFAULT '',
    correlation_id TEXT NOT NULL DEFAULT '',
    idempotency_key TEXT NOT NULL DEFAULT '',
    payload BLOB NOT NULL,
    checksum TEXT NOT NULL,
    schema_version INTEGER NOT NULL,
    UNIQUE(stream_type, stream_id, stream_sequence)
);
```

核心 projection：

```text
teams
agents
agent_threads
tasks
task_dependencies
task_claims
mailbox_messages
workspace_views
workspace_leases
approval_requests
usage_ledger
budget_accounts
idempotency_records
projection_cursors
```

Projection 可以重建，但正常运行直接查询 projection，不能每次从头 replay。

实际实现：`orchestration_events` 与上面 DDL 一致。projection/状态表实际为 `teams`、`agents`、`agent_threads`、`tasks`、`task_dependencies`、`mailbox_messages`、`mailbox_recipient_sequences`、`wait_sets`、`workspace_views`、`merge_queue`、`merge_apply_journal`、`merge_apply_progress`、`usage_records`、`usage_reservations`、`team_budgets`、`idempotency_records`、`projection_cursors`。没有 `task_claims`（claim 是 `tasks` 的列）、`approval_requests`、`usage_ledger`、`budget_accounts`。主库中的 `workspace_leases` 表保留但不再使用，lease 在 workspace 协调库 `coord.db`（17.2）。没有从事件重建 projection 的实现。

### 19.3 Usage Ledger

每次模型响应写一条不可变记录：

```go
type UsageRecord struct {
    TeamID              TeamID
    AgentID             AgentID
    TaskID              TaskID
    ThreadID            ThreadID
    TurnID              string
    Provider            string
    Model               string
    InputTokens         OptionalInt
    OutputTokens        OptionalInt
    ReasoningTokens     OptionalInt
    CacheReadTokens     OptionalInt
    CacheWriteTokens    OptionalInt
    EstimatedInput      int
    CountingStatus      tokenizer.Status
    ProviderRequestID   string
    Cost                OptionalDecimal
    Currency            string
}
```

`OptionalInt` 区分 `0` 和 provider 未提供。Provider adapter 保存原始字段，usage 层再归一化。Team 总计从 ledger 聚合，不从 TUI 屏幕数字反推。

实际实现：`usage_records` 列为 record_id、team_id、agent_id、run_id、turn_id、request_id（Team 内唯一）、model_profile、provider、model、input/cached/output/reasoning/total_tokens（整数，默认 0，不区分“未提供”）、cost_micros、quality、recorded_at。没有 TaskID/ThreadID、cache write、EstimatedInput、CountingStatus、Currency；估算与否由 `quality` 字符串表达（见 13.5）。Team 总计按 ledger 聚合。

### 19.4 Migration 策略

1. 引入独立 `schema_migrations(version, applied_at, checksum)`；
2. 每个版本使用 forward-only transaction；
3. 启动时拒绝打开比程序更新的 schema；
4. 大迁移前创建数据库备份或要求显式确认；
5. projection schema 可重建，事实表 migration 必须保守；
6. v1 到 Multi-Agent schema 的首次迁移只新增表和索引，不重写已有 `session_events`；
7. 旧 Session 首次打开时按需创建单 Agent Team/Agent/Thread projection；
8. 所有 migration 有空库、旧库、重复执行和中断恢复测试。

实际实现：`schema_migrations(version, applied_at, checksum)` 已实现，当前到 v41（v34 agent workspace 列，v35 merge apply journal，v36 mailbox 消费账本，v37 finalization 回执，v38 verification digest，v39 Agent completion receipt，v40 细化成员关系，v41 Goal durable admission token），每版一个 forward-only transaction，已应用版本校验 checksum，比程序新的 schema 拒绝打开；固定 sha256 测试 fixture 覆盖 v1–v41 并检查版本数量。第 4 条（迁移前备份/确认）未实现。旧 Session 首次打开时创建 Team/root Agent/Thread（`team.created` payload `source=legacy_session`）。

### 19.5 SQLite 并发

- 保持 WAL；
- 一个短事务 writer 负责 sequence 和 projection；
- 多个只读连接服务 TUI/query；
- 模型、工具、hash 和 Git 操作不在数据库 transaction 内运行；
- busy timeout 之外仍使用 bounded retry；
- command queue 满时进行 backpressure，不能无限积压内存。

实际实现：WAL、`busy_timeout(5000)`、`SetMaxOpenConns(1)`（读写共用一个连接，没有独立只读连接）；没有 busy 之外的 bounded retry，也没有 command queue。模型、工具与 Git 操作不在数据库 transaction 内。

---

## 20. 崩溃恢复与重启

### 20.1 启动恢复流程

1. 获取单实例 Supervisor lock；
2. 执行 schema migration；
3. 校验 orchestration event checksum 和 projection cursor；
4. replay 缺失 event；
5. 加载非 terminal Team、Agent、Task、approval、mailbox 和 lease；
6. 对每个 active Run 检查 Eino checkpoint 和最后一个 tool outcome；
7. 重新建立 scheduler queue、deadline 和 provider backoff；
8. 恢复 pending approval 到 Broker/UI；
9. 校验 workspace view/worktree 和 lease fingerprint；
10. 发出 `agent.recovery_scheduled` 或明确的 `agent.recovery_failed`；
11. TUI 连接后通过 cursor 获取完整恢复快照。

实际实现（`agentmanager/supervisor_recovery.go` 的 `Recover`）：没有单实例 Supervisor lock（同一 workspace 的多实例靠 workspacedomain flock 协调 lease），也没有 event checksum 校验和 replay（第 3、4 步）。顺序为：
1. 打开数据库时执行 migration；
2. `RecoverReservations` 结算崩溃前未结算的预算预留；
3. `ReapAbandonedLeases` 回收已退出实例的 lease；
4. 逐 Team 处理非 root Agent：`running` → `queued`（同一 RunID，Activity `recovered`）；`waiting` 但没有 pending WaitSet → `queued`；`canceling` → `canceled`，并取消其 WaitSet、释放 Task、清理资源；
5. 逐 Team `dispatchReadyTasks`、`ReconcileWaitSets`，并调度 merge（继续已写入 journal 的 snapshot 提交）；
6. `startAvailable` 并唤醒调度。

没有 scheduler queue/backoff 重建（调度直接查询 projection）、pending approval 恢复、view/lease fingerprint 校验和 `agent.recovery_*` 事件。root 的消费账本由 `SettleBackground` 单独结算（15.3）。

### 20.2 Run 恢复矩阵

| 崩溃前状态 | checkpoint | tool 状态 | 恢复行为 |
| --- | --- | --- | --- |
| queued | 无 | 无 | 重新进入 scheduler |
| running model | 有 | 无 | 使用同一 Thread/turn snapshot ResumeTurn |
| running model | 无 | 无副作用 | 根据 retry policy 创建新 Run |
| waiting approval | 有/无 | approval pending | 恢复审批，等待用户 |
| tool started | 任意 | outcome known | 投影结果并恢复 Runner |
| tool started | 任意 | outcome unknown | 标记 reconciliation，不盲目重跑 |
| waiting children | 无要求 | child active | 恢复 wait set |
| canceling | 任意 | 任意 | 继续取消并 fencing 晚到结果 |

实际实现：恢复的 `queued` Run 以 `Recover` 标志调用子会话 `RecoverRun(currentRunID)`，沿用单 Agent 的 checkpoint 与工具恢复语义，不重新注入 mailbox；WaitSet 恢复的 Run 用 resume token `ResumeTurn`；Task 层已提供受限 retry policy（仅可信 iteration-limit 且通过副作用证明，最多 3 次）；没有 approval pending 状态（审批不持久化，进程退出时 pending 审批丢失）；`canceling` 直接终结为 `canceled`。20.3 的 reconciliation 只有单 Agent 运行时已有的部分，Multi-Agent 层没有新增。

### 20.3 副作用核对

`tool.outcome_unknown` 的处理：

- 只读工具可按策略安全重试；
- workspace write 可通过目标 fingerprint/预期内容核对；
- shell、网络或外部系统调用需要 executor 提供 idempotency/reconcile 接口；
- 无法核对时请求用户决定，不能默认重跑或默认成功。

### 20.4 恢复保证

本设计目标是：

- 不丢失已 commit 的命令、消息和结果；
- 不重复创建同一幂等 spawn/task/message；
- 不让旧 Run 的晚到结果覆盖新 Run；
- 不把“进程退出”解释为“用户取消”；
- 无法安全恢复时进入可解释的 blocked/failed 状态，而不是静默重置。

---

## 21. TUI 与 Headless 协议

### 21.1 TUI 信息架构

默认视图仍以当前 Thread transcript 为主体：

```text
Header: model / workspace / team / current agent / thread
---------------------------------------------------------
Current Thread transcript

---------------------------------------------------------
Composer
Status: context / team usage / running agents / approvals
```

Agent 活动面板作为可切换区域：

```text
Agents (3 running, 1 waiting)
  root        running  model       task: coordinate release
  explorer-1  completed            task: inspect API
  worker-1    waiting  approval    task: update store
  tester-1    running  tool        task: run integration tests

Tasks
  [done] inspect API
  [work] update store
  [wait] integration tests <- update store
```

交互要求：

- 进入任意 Agent 查看其完整 Thread；
- root/child 标识、层级和 Task 清晰可见；
- 当前运行、等待原因、耗时、context、token/cost 可扫描；
- 可以发送 steer/message、wait、cancel；
- pending approval 集中展示，并标出 Agent、Task、风险和 workspace view；
- 默认折叠工具参数和结果，但始终显示每次工具调用本身；
- child 的高频 token delta 只在当前 Thread 展示，活动面板只显示节流后的摘要；
- 保持原生 terminal selection，不用全屏 overlay 破坏复制和 scrollback。

具体快捷键在 TUI keymap 中配置，不写进领域协议。鼠标点击、键盘导航和命令面板触发相同 Supervisor command。

实际实现（`internal/tui`）：
- 默认布局不变，没有 team/current agent/thread header。状态栏显示 `agents N running M waiting K queued`（只列非零项）与 `merges N pending`（未决 merge 候选数）。
- `/agents`（别名 `/activity`，快捷键 Ctrl+G / Alt+G）打开 Agent 面板：列出 Agent（短 ID、状态、live activity、Task 标题、耗时、已记录 token）、workspace view、lease 与 merge 候选，可只读打开子 Agent transcript，`x x` 取消子 Agent，`r` 重新审查 merge，`d d` 丢弃 merge 候选。`/merge`（`/merges`）在 transcript 中输出 merge 摘要。
- 没有 Task 列表视图、context/cost 列、切换到子 Thread 继续对话、steer/message 输入和面板内 wait；子 Agent 的 token delta 不在主界面展示，面板只显示 activity 字符串。
- `[agents].auto_continue` 只在 TUI 中生效（15.3）。

### 21.2 审批体验

审批区域显示：

```text
workspace_write · worker-1 · task_update_store
approval reason: writing files changes the workspace

> Approve once
  Deny
  Inspect arguments
```

选择结果在同一区域更新为 `approved`、`denied`、`expired` 或 `canceled`，与模型正文、工具结果视觉区分。Arguments/result 默认折叠，工具名永远可见。

实际实现：所有 Agent 的审批进入同一审批区域，来源标签为“profile 短 ID · task 标题”，选项只有 Allow/Reject（没有 Inspect arguments、expired/canceled 结果）。snapshot 合并的 `merge_apply` 审批附带 plan diff。

### 21.3 Headless

Headless 使用 versioned JSON Lines：

```json
{"protocol":"gsea.control.v1","type":"agent.created","cursor":42,"team_id":"team_...","agent_id":"agent_..."}
{"protocol":"gsea.control.v1","type":"agent.delta","agent_id":"agent_...","channel":"assistant","delta":"..."}
{"protocol":"gsea.control.v1","type":"approval.requested","approval_id":"approval_...","agent_id":"agent_..."}
```

控制输入同样为结构化命令：

```json
{"protocol":"gsea.command.v1","command":"agent.cancel","agent_id":"agent_...","idempotency_key":"..."}
```

要求：

- schema version 明确；
- event 带 cursor，可断线续传；
- stdout 只输出协议，诊断写 stderr；
- 非交互审批默认拒绝；
- exit code 反映 root/Team 结果，child 非关键失败通过结构化事件报告；
- 慢客户端不会阻塞 Agent 执行，超出 buffer 后要求从 cursor 补读。

实际实现：
- 管道模式的 `chat`（`cli/chat_headless.go`）仍是单 Agent 的文本/JSON 输出，可以运行子 Agent，审批使用 `DenyApprover`（evaluation 模式例外，见 16.3）；没有 snapshot merge Approver，`auto_merge=false` 时 snapshot merge 保持排队。
- 独立子命令 `control`（`cli/control.go`）是请求/响应式 JSON Lines：输入每行一条 `controlCommand`（`protocol` 为空或 `gsea.command.v1`，单行 ≤1 MiB），输出带 `"protocol":"gsea.control.v1"` 的 `snapshot`、`result`、`error` 或原样事件。命令为 `snapshot`、`agent.list`、`task.list`、`workspace.list`、`merge.list`、`events`（`after` cursor + `limit`，可按 `team_id` 过滤；事件行之后输出一条 `result`，其 `count` 为本页事件数、`next_cursor` 为下一页的 `after`，空页时等于请求的 `after`）、`agent.cancel`（递归中断后代）、`team.cancel`（需 `team_id`，可选 `reason` 与 `timeout_ms`，返回 Team 快照，见 §9.1）、`merge.run`、`merge.requeue`、`task.transition`（需 `actor_agent_id` 与 `expected_version`）、`wait`。
- `control` 创建的 Supervisor 没有 Runner，只做查询与管理，不执行 Agent；没有 `idempotency_key`、approval、spawn、message 命令，也没有 `agent.delta` 流。`events` 的 Team 过滤在 SQL 中先于 LIMIT 进行，按 Team 分页不会出现空页跳过。

---

## 22. 可观测性

### 22.1 指标

至少记录：

- live/running/waiting Agent；
- spawn accepted/rejected、queue latency、run duration；
- 每 provider/model 并发、请求、rate limit、重试和 latency；
- input/output/reasoning/cache read/cache write token；
- Team/Agent/Task cost；
- tool call、approval wait、denial 和 outcome unknown；
- mailbox lag、未 ack 消息；
- lease wait、conflict、merge queue latency；
- recovery count、recovery failure；
- context compression 次数和压缩前后 token。

### 22.2 Trace 关联

统一关联链：

```text
TeamID
  -> CorrelationID (用户请求)
      -> TaskID
          -> AgentID
              -> RunID
                  -> TurnID
                      -> model request ID / tool call ID / approval ID
```

日志默认不包含 secret、完整 prompt 或未折叠工具内容。Debug transcript 必须显式启用并遵守文件权限。

### 22.3 审计查询

需要支持：

- 谁创建了某 Agent；
- 某 Task 为什么重新分配；
- 某文件由哪个 Agent、哪个 tool call 修改；
- 某审批授权了什么规范化参数；
- 某 Team 的 token/cost 如何按 Agent 汇总；
- 崩溃恢复后哪些 Run 被恢复、重试或阻塞。

实际实现（未实现指标系统）：没有 metrics 导出；可观测数据只有 `orchestration_events`、`session_events`、`usage_records`（按 Agent 汇总 token）与 TUI/`control` 快照。关联字段为 TeamID、AgentID、RunID、TurnID、CorrelationID（多为 tool call ID）与 request ID；没有 approval ID。上面的审计查询没有专门实现，需要直接查询这些表。

---

## 23. 失败处理矩阵

| 失败 | 局部行为 | 上层行为 |
| --- | --- | --- |
| 模型临时错误 | durable backoff，释放 provider slot | Agent waiting，达到上限后 failed |
| provider rate limit | 更新 provider bucket | 调度其他可运行 Agent |
| context 超限 | 精确计数后压缩/降低继承 | 仍超限则 typed failure |
| tool policy denied | 作为 tool result 返回模型 | Agent 可调整方案，不自动 Team failed |
| approval denied | `approval_denied` 结果 | 允许模型收尾或 Task blocked |
| approval UI 消失 | 请求保持 pending 或按 deadline expired | Agent waiting，不丢请求 |
| child failed | 父收到结构化 result | retry、reassign、降级或关键路径失败 |
| parent failed | 按 Team policy cancel/transfer child | 默认结构化取消 |
| task dependency failed | downstream blocked | lead 决定替代或取消 |
| workspace lease conflict | 等待、改为 worktree 或返回 conflict | 不执行写入 |
| merge conflict | 保存 diff/artifact | 创建 resolution Task |
| DB busy | bounded retry/backpressure | 不启动未持久化副作用 |
| event checksum 错误 | 停止受影响 stream | doctor/repair 流程，不静默跳过 |
| process crash | 启动恢复矩阵 | Team 继续或进入明确 blocked |
| tool outcome unknown | reconcile | 禁止盲重试 |
| budget exhausted | 停止新 admission | 允许有限收尾并报告 partial |
| cancel 超时 | fencing + sandbox/process kill | 标记 forced canceled/failed |

实际实现与上表的主要差异：当前恢复暂停没有通用的持久 backoff 调度器，`agent_resume` 是显式恢复入口；child failed 仍没有自动 retry/reassign；非 root Agent 终态会递归取消其后代，root projection 仍由 Conversation 层管理；依赖失败不传播；审批不持久化；DB busy 只依赖 busy_timeout；没有 event checksum 修复流程（`doctor` 子命令做配置/环境诊断，不修复事件）；价格与 Team 预算经配置装配（13.5）；cancel 通过 context 取消，没有 forced terminalization。merge conflict → Resolution Task 与 lease conflict → 超时返回错误已实现。

### 23.0 动态计划的持久契约

Root Driver 仍由模型即时决定工作分发，Task 是可增量修改的执行清单。`task_update` 必须携带当前版本，只能编辑 draft/blocked/ready 且无 live Agent、无待合并结果的任务。`task_refine` 必须携带父版本，每条命令新增 1–64 个成员、最多 256 条依赖，所有 child/edge/receipt/event 在同一个事务提交。当前 owner、合法管理祖先及 root 可以管理子树；同 Team 身份不等于任意修改权限。

普通 `ParentTaskID` 保持分组语义；v40 `task_refinement_members` 才产生父等待成员完成的约束。提交细化与后续添加依赖使用相同边集检测环，禁止子任务依赖等待它的父任务。模型负责提出细化内容，宿主不启动独立的自动规划模型，也不宣称能自动生成最优分解。已执行任务契约不被字段编辑覆盖。

自动派发的 prompt 绑定读取时的 Task version，创建 Agent 与认领任务在同一事务核对版本。新 attempt 的失败改派仅允许宿主可信的 iteration-limit 分类，且原 Run 的工具调用必须证明为纯读/读、逐条成功回执匹配；无写入、无未知结果、无存活后代及未清理 lease/view/merge。最多 3 次总执行。API、额度、预算和网络暂停只恢复同一 Run，不消耗新 attempt。

worker 结果冻结后如父细化成员尚未完成，completion 保持 pending，释放进程运行槽位并由周期扫描或重启扫描补做结算。终态后通知、reviewer 派发、后代中断、资源释放也可重复补齐；这些协调错误不能改写已冻结 worker 结果。

### 23.1 Team 可靠恢复验收标准

恢复正确性以 GSEA 的持久事实和故障注入测试为准：

1. Team、父子关系、Thread、Profile、Workspace、Task claim 和 RunID 在子 Agent 执行前已持久化。
2. 可恢复的模型、限流、额度或预算故障进入 `recovery:<kind>` 等待状态；重启不会自动消耗新请求，`agent_resume` 保留原 RunID 和 Task attempt。
3. 已确认的模型响应、工具成功/拒绝/已知错误结果、Agent 终态和合并不会重复执行；副作用工具只有 `started` 没有成功回执时进入 `OutcomeUnknown`，等待核对。
4. Agent 终态与父级结果通知同事务提交；恢复扫描能补齐被冻结但尚未结算的 Task、交付和通知。
5. 等待挂起先保存 Eino checkpoint，再创建 WaitSet；恢复时同一 WaitSet 的 resolved/expired 状态可以重放。
6. 硬杀、网络错误、额度拒绝、checkpoint 缺失、重复消息和合并中断都必须在 SQLite 重开测试中验证；无证据时进入 `unavailable` 或人工恢复。
7. v41 durable admission token 已覆盖 Goal 创建到 `turn.started`/首轮绑定的崩溃窗口：token 与 session、actor、输入摘要、契约摘要、配置摘要、Turn 和 round 绑定，状态迁移使用 CAS；未完成绑定进入 `unavailable`，已绑定的恢复沿用原 token。

---

## 24. 性能与容量边界

第一版目标不是无限扩展，而是可预测：

- 单进程最多 64 live Agent，默认 16；
- 同时 running Agent 默认 4，hard ceiling 16；
- Team 深度默认 3，hard ceiling 8；
- 每 Team Task 默认上限 128；
- mailbox body 的目标默认上限为 64 KiB，大内容必须转 artifact；当前 Phase 2 SQLite 实现先以 1 MiB 作为硬上限；
- event payload 默认上限 256 KiB；
- TUI delta 合并刷新不高于 30 FPS；
- activity event 节流到语义变化或至少 250 ms；
- projection query 的目标 p95 小于 50 ms；
- Supervisor command commit 的目标 p95 小于 20 ms，不包含模型/工具执行；
- 文件 hash 使用 bounded worker pool；
- transcript、diff、日志等大对象进入 artifact store，不塞入 SQLite event payload。

达到上限必须返回 typed capacity error 或排队，不允许无界 goroutine/channel/map 增长。

实际实现：`[agents]` 默认 max_live 16、max_running 4、max_depth 3、max_children 4，均无 hard ceiling；没有每 Team Task 上限；mailbox body 与 Task description 上限 1 MiB；event payload 没有单独上限；artifact 单个上限 32 MiB（`internal/artifact`），`artifact_read` 单次最多 1 MiB；TUI 刷新频率、activity 节流和 p95 目标没有专门实现或测量。超出 live/depth/children 上限时 spawn 返回错误，超出 running 时保持 queued。

---

## 25. 实现阶段

阶段交付遵循本文件的最终 ID、事件、状态机和权限契约。早期阶段可以不开放全部能力，但不能使用未来无法兼容的临时含义。

### 当前实现进度（2026-10-29）

- Phase 0/1 已落地：durable Team/Agent/Thread projection、状态机、Actor Scope、Profile 快照（内置 default/worker/reviewer）、Supervisor 生命周期（CAS + admission mutex，无 command loop）、重启恢复和 Agent/Task/merge control tools（含 `agent_continue`）。
- Phase 2 已落地：有界并行运行槽位、per-model-profile bucket、跨 Team round-robin（`RoundRobin` 无权重，Priority 恒为 0）、同时只运行一个 shared-lease 写入者、parent-child durable mailbox、recipient 顺序、at-least-once delivery/ack、root 消费账本（turn 内每次模型请求前注入，空闲时 TUI wake turn）、spawn/mailbox 幂等冲突校验。steer、Agent 级预算未实现。
- Phase 3 已落地基础闭环：durable Task CRUD、父子任务、依赖边、递归环检测、乐观版本推进、依赖阻塞/自动解锁、claim/release、claim TTL 与过期回收、Task-Agent 生命周期联动、ready task 自动派发、reviewer verification gate（自动派生 reviewer，`task_verify`）和新增的 `awaiting_merge` 状态。已支持 `task_update` 执行前编辑、`task_refine` 原子增量细化与安全受限的 `task_reassign`；tester Profile 和关键路径优先未实现。
- Durable waiting 已落地：WaitSet projection、pending/resolved/consumed/expired/canceled、Agent `waiting -> queued`、deadline reconcile、重启恢复、`ResumeTurn` 和 `agent_wait` 挂起错误契约；挂起会释放运行槽位。只支持等待子 Agent。
- Team 级可靠恢复已落地基础闭环：终态 Agent 结果与父级 `MessageResult` 在同一 SQLite 事务中提交；`agent_completions` 冻结运行结果并在 Supervisor 恢复时补做 Task/Agent/交付结算；可恢复的传输、限流、供应商不可用、凭据、额度和预算错误进入 `recovery:<kind>`，保留原 RunID 与 Task claim，由 `agent_resume` 显式重新入队。普通错误、明确取消和副作用未知不会盲目重试。
- Turn journal 已落地：`turn.started` 先保存原始输入和运行身份；模型成功响应保存 request digest/输入快照，checkpoint 缺失时可按已确认模型响应和工具回执重放原 Turn，不重复注入 Instruction、消费新 mailbox 或重做已完成摘要；旧版本缺损 receipt 明确拒绝；缺失 checkpoint 与 journal 时 fail closed。`agent_wait` 先让 Eino 保存 StatefulInterrupt checkpoint，再创建 WaitSet。
- Usage/budget：provider usage、cache token、价格表与 unknown pricing 标记、durable usage ledger、Team budget、请求级 reservation/settlement/release 和启动期回收均已实现；价格表与 Team 预算由 `[usage]` 配置（§13），未配置时没有预算上限、成本为 0。
- Workspace Coordinator 已落地：四种 view（shared-readonly/shared-lease/git-worktree/新增 snapshot，`default_workspace=auto` 时写入者用 snapshot）、单次操作级 path lease（shell 取整 workspace lease，flock 判活、无 TTL）、snapshot 候选 → 三方合并（go-udiff）→ ApplyPlan → `merge_apply` 审批 → apply journal → 提交、冲突时 Resolution Task 与取代链、git-worktree 结果自动入队 git-patch merge 并经 `git apply --3way` 合入、可选的合并后检查（`[agents].post_merge_check`）。worktree 池、所有权 lease/epoch/`shared_writes` 未实现。
- Artifact store 已落地：bounded content-addressed filesystem store（单个 32 MiB，`sha256:<hex>` 引用、原子写入、去重、符号链接逃逸防护），无保留策略；App 默认装配。
- TUI/headless：`/agents`（`/activity`，Ctrl+G/Alt+G）面板可列出 Agent/View/Lease/merge 候选、只读查看子 transcript、取消子 Agent、重审/丢弃 merge 候选；状态栏显示 Agent 与 merge 计数；`control` 子命令提供快照、事件 cursor 分页、Agent/Team 取消、任务推进与 merge 控制；有 checkpoint 可恢复性故障注入测试。线程切换、steer、durable 审批未实现。
- 当前 SQLite schema 使用 forward migrations 至 v41；Mailbox body 上限 1 MiB，超大 transcript、diff 和日志应转 artifact。
- 各 Phase 状态：Phase 0–2 已完成（steer、Agent 预算等少量条目除外）；Phase 3 基础闭环完成；Phase 4 基础闭环完成（含可配置的合并后检查）；Phase 5 未实现。

### Phase 0：协议与存储基线

- 定义 typed IDs、CommandMeta、状态机和 event payload v1；
- 引入 Database owner、migration runner 和 orchestration event store；
- 建立 Team/Agent/Thread/Task 基础 projection；
- 为现有单 Conversation 自动创建单 Agent Team projection；
- 扩展 tool/policy metadata，但行为保持单 Agent；
- 完成 replay、checksum、idempotency 和 migration 测试。

**验收：** 现有 chat 行为不变；每个 turn 可关联 Team/Agent/Thread；重启后 projection 一致。

状态：已完成。实际差异：没有 CommandMeta；event checksum 在读取时校验（不匹配即报错），但没有 replay/repair 流程。

### Phase 1：Durable Supervisor 与单 child

- 实现 Supervisor command loop、Agent/Profile/Thread registry；
- 实现 `agent_spawn/list/wait/interrupt`；
- 只允许一个 child running，验证完整生命周期；
- 实现 context inheritance 和结构化 AgentResult；
- 实现父子取消和 checkpoint 恢复。

**验收：** child 可跨重启恢复；重复 spawn tool call 不创建重复 Agent；取消无孤儿。

状态：已完成。实际差异：没有 command loop（CAS + admission mutex）；中断递归传播到全部后代，父级终态会级联中断仍存活子 Agent，恢复扫描修复崩溃留下的 queued 后代。

### Phase 2：有界并行、Mailbox 与预算

- 实现 scheduler、公平队列和 provider bucket；
- 开放多个 Agent 并行；
- 实现 durable mailbox、steer 和安全注入；
- 实现 usage ledger、Team/Agent budget；
- 增加 TUI activity panel 和 thread inspection。

**验收：** 并行任务真实重叠；race test 通过；断线/重启不丢 mailbox/result；预算能阻止新 admission。

状态：已完成，steer 与 Agent 级预算未实现；Team 预算由 `[usage.budget]` 配置，未配置时没有上限；thread inspection 只有 TUI 只读 transcript。

### Phase 3：Task DAG 与验证 Gate

- 实现 Task CRUD、dependency、claim、重分配；
- 提供 task tools；
- 实现 worker/tester/reviewer Profile 和 gate；
- 实现关键路径优先和自适应任务细化。

**验收：** cycle 被拒绝；Agent 失败后任务可重分配；验证未通过不能 completed。

状态：基础闭环完成。cycle 拒绝、验证 gate、版本化计划编辑、原子细化和安全失败重分配已实现；tester Profile 和关键路径优先未实现。

### Phase 4：Workspace Coordinator

- 为 workspace 工具接入 path lease 和 fingerprint；
- 为 shell 接入写范围或 workspace-wide lease；
- 实现 Git worktree Workspace View 和 merge queue；
- 在 TUI 展示 conflict 和 resolution task。

**验收：** 同路径并发写不能静默覆盖；worktree 结果可验证合并；崩溃后 lease 可核对恢复。

状态：基础闭环完成，并新增 snapshot view 与三方合并（17.5）。shared-lease 写入没有 fingerprint 校验；git-worktree 结果自动入队并合并；合并后验证由 `[agents].post_merge_check` 配置，失败时 Task `failed` 且不回滚；TUI 面板展示 merge 候选及其状态。崩溃后 lease 按持有实例存活回收，不做 fingerprint 核对。

### Phase 5：高级编排与体验优化

- Delegation benefit heuristic；
- 跨 provider/model 路由；
- 基于任务类型和历史表现选择 Profile；
- 自动创建修订/验证任务；
- Team 模板、可视化 DAG、完整 headless control protocol；
- benchmark、故障注入和容量调优。

**验收：** 在基准复杂任务上减少 wall time 或提高验证覆盖，同时受预算和冲突约束，不因盲目并行增加失败率。

状态：未实现。仅有与之相关的局部能力：`control` 子命令（非完整 control protocol）与 checkpoint 故障注入测试；merge 冲突会自动创建 Resolution Task。

---

## 26. 测试与验收计划

### 26.1 单元测试

- 所有状态机的合法/非法转换表；
- typed ID 解析和错误；
- DAG 环检测、dependency 解锁；
- 权限交集和非升级属性；
- mailbox 顺序、去重、ack；
- scheduler 公平性、capacity、backoff；
- lease 路径重叠和多路径死锁避免；
- usage Optional 字段和 provider 映射；
- event payload version 和 checksum；
- idempotency 命令重复提交。

### 26.2 并发与 Race 测试

```bash
GOCACHE=/tmp/go-self-evolving-agent-cache go test -race ./internal/agentmanager/...
GOCACHE=/tmp/go-self-evolving-agent-cache go test -race ./internal/orchestration/... ./internal/session/sqlitestore/...
GOCACHE=/tmp/go-self-evolving-agent-cache go test -race ./internal/scheduler/...
GOCACHE=/tmp/go-self-evolving-agent-cache go test -race ./internal/workspacecoord/... ./internal/workspacedomain/...
GOCACHE=/tmp/go-self-evolving-agent-cache go test -race ./...
```

实际实现：设计中的 `internal/taskgraph`、`internal/agentmailbox` 包不存在，Task 与 Mailbox 的实现与测试位于 `internal/orchestration`（类型与校验）和 `internal/session/sqlitestore`（持久化），上面的命令已按实际包改写。“多个 UI subscriber”没有对应实现（客户端轮询，18.4）。

覆盖：并发 spawn、wait+cancel、approval+cancel、claim expiry+result late arrival、mailbox+recovery、lease acquire/release、多个 UI subscriber。

### 26.3 集成场景

1. root spawn explorer 和 worker，wait all，结果稳定回传。
2. 4 个 Agent 并行调用不同模型 Profile，usage 正确聚合。
3. 父级等待 child 时收到用户 steer，安全中断并恢复。
4. child 请求审批，UI 切换 Thread 后仍能统一处理。
5. 审批拒绝后 Agent 生成替代方案，Team 不出现框架级失败。
6. 两个 worker 写同一文件，第二个等待或 conflict，不静默覆盖。
7. 两个 worktree 产生冲突，merge queue 创建 resolution Task。
8. tester 失败后 Task 回到 revision 流程，不能提前 completed。
9. provider rate limit 时其他 provider Agent 继续运行。
10. budget exhausted 时不再 spawn，新结果报告 partial。

### 26.4 故障注入

在以下边界 kill 进程并重启：

- spawn event commit 前/后；
- Agent admitted 后、worker 启动前；
- model stream 中；
- approval requested 后；
- tool started 后、result event 前；
- mailbox enqueue 后、delivery 前；
- lease acquired 后、write 前；
- write 后、lease release 前；
- merge applied 后、verification 前；
- projection commit 后、UI publish 前。

每个测试验证：无重复 Agent、无丢消息、无静默副作用重放、状态可解释。

### 26.5 端到端验收任务

- 独立探索多个模块并由 root 汇总架构；
- worker 修改、tester 验证、reviewer 审查的完整闭环；
- 多文件低冲突并行实现；
- 同文件高冲突自动降级为顺序或 worktree；
- 跨 provider 路由和统一 token/cache/cost 报表；
- 运行中关闭 TUI、重启进程并恢复 Team；
- headless client 从旧 cursor 重连并继续控制。

---

## 27. 明确拒绝的替代方案

### 27.1 直接设置 `ExecuteSequentially: false`

拒绝原因：只会并行同一 turn 的工具调用，缺少独立上下文、Agent 生命周期、预算、恢复、取消和工作区协调，副作用风险反而上升。

### 27.2 仅使用 Eino `NewParallelAgent`

拒绝作为主方案的原因：适合静态图，不覆盖动态 spawn、durable task、mailbox、审批和跨重启 Team。可以在某个固定 Task 内作为执行优化使用。

### 27.3 把 Agent 包装成普通函数并同步等待

拒绝原因：无法 steer/inspect/cancel，父级占用调用栈和资源，进程重启后丢失全部生命周期。

### 27.4 所有 Agent 共享一个 Conversation

拒绝原因：mutex 会重新串行化执行；并发写历史顺序不确定；上下文迅速膨胀；权限和结果来源难以区分。

### 27.5 只在内存保存 Team

拒绝原因：复杂任务最需要恢复能力；崩溃会丢失 child 结果、审批、任务 claim 和文件所有权。

### 27.6 仅靠 prompt 避免文件冲突

拒绝原因：不可验证、不可恢复、对 shell 无约束。模型提示只能辅助，不能替代 lease/fingerprint/worktree。

### 27.7 第一版直接做分布式集群

拒绝原因：当前目标是本地单用户 CLI/TUI；先把 durable 单进程语义做正确，未来可以把 scheduler worker 边界远程化。

---

## 28. 相对公开方案的增强目标

本项目不以复制 UI 为目标，而是在共同基础体验上增加以下能力：

1. **Team 跨进程完整恢复**：Agent、Task、Mailbox、Approval 和 Lease 均 durable。
2. **主动工作区冲突控制**：不仅提示可能冲突，而是通过 lease/fingerprint/worktree 阻止静默覆盖。
3. **结构化验证流水线**：worker、tester、reviewer 由 Task DAG 和 gate 强制约束。
4. **自适应任务图**：可根据探索和失败结果拆分、重分配、切换 Profile。
5. **跨 Provider 路由与统一账本**：保留 provider 原始 usage，同时汇总 token/cache/cost。
6. **委派收益控制**：避免为了“看起来并行”而浪费上下文和模型调用。
7. **统一 TUI/headless 控制协议**：UI 只是 projection consumer，断线不影响执行。

这些增强目标不能以牺牲权限、可恢复性或结果可解释性为代价。

---

## 29. 需要冻结的 ADR

实现 Phase 0 前应将以下决策形成 ADR，并在变更时显式评审：

1. ID 格式和 ThreadID/SessionID 映射；
2. Agent、Task、Team、Approval 和 Lease 状态机；
3. event envelope、payload version 和 migration 规则；
4. Supervisor command loop 与 transactional outbox；
5. structured concurrency 和 detached policy；
6. context inheritance 默认值与 token 上限；
7. 权限交集和 delegation 表达方式；
8. shared lease、shell coarse lease 和 worktree 策略；
9. mailbox at-least-once 与安全注入边界；
10. usage 原始值、估算值和 cost 的数据类型；
11. headless protocol version/cursor 语义；
12. tool outcome unknown 的 reconcile 接口。

这些属于高迁移成本契约。TUI 配色、面板布局、默认快捷键和启发式权重属于可迭代细节，不需要在 Phase 0 永久冻结。

---

## 30. 开放问题

以下问题不阻塞总体架构，但应在对应 Phase 开始前决定：

- artifact store 第一版使用文件系统内容寻址目录还是 SQLite blob；
- Git worktree 是否默认要求 clean baseline，还是允许从带用户未提交修改的 snapshot 生成 patch；
- Team budget 的 cost 是否只做展示，还是支持用户配置价格表并硬限制；
- provider 不返回 cache write token 时是否只保留 unavailable，或允许协议特定估算；
- 自动编排器是 root Agent prompt 能力，还是独立 planner Profile；
- reviewer gate 的失败是直接创建 revision Task，还是先交给 root 决策；
- 多个 workspace root 的支持时机；
- 长任务恢复时模型 provider 是否支持 request continuation，或统一依赖 Eino checkpoint + 新请求；
- shell read-only 分类器采用静态规则还是显式工具 schema 声明；
- Team snapshot 的默认保留和 artifact 清理策略。

截至 2026-10-26 已有结论：artifact store 采用文件系统内容寻址目录（`internal/artifact`）；带未提交修改的工作区通过 snapshot view（不依赖 Git）隔离并以三方合并回写，git worktree 仍从 HEAD 基线创建；价格表与 Team 硬限额已由 usage.prices/usage.budget 装配到生产路径；cache write token 不记录；reviewer 失败使 Task 回到 `ready`，不自动创建 revision Task；shell 没有只读分类器（一律取整 workspace lease）。其余问题仍开放，artifact 仍没有保留/清理策略。

默认处理原则：未知能力保持保守、可观察、可恢复，不用隐式“智能”掩盖不确定性。

---

## 31. 外部行为参考

本设计参考以下公开产品文档描述的用户可见行为：

- Codex Multi-Agent / Subagents：`https://developers.openai.com/codex/multi-agent`
- Claude Code Subagents：`https://code.claude.com/docs/en/sub-agents`
- Claude Code Agent Teams：`https://code.claude.com/docs/en/agent-teams`

参考范围是独立上下文、Agent 生命周期、任务依赖、消息通信和交互控制等公开行为。实现基于本项目现有 Go/Eino/SQLite/Policy 架构独立完成。

---

## 32. 最终架构判断

本项目应采用：

```text
完整领域设计
  + durable Supervisor
  + independent Agent Threads
  + bounded Scheduler
  + Task DAG and Mailbox
  + non-escalating Policy/Approval
  + Workspace Coordinator
  + event-sourced recovery
  + TUI/headless projections
```

然后按纵向切片实现，而不是先做一个仅能并发 goroutine 的临时版本。

这种顺序可以同时解决两个看似冲突的目标：

- **架构上完整**：前期固定最难迁移的状态、事件和安全语义；
- **交付上渐进**：每个 Phase 都产生可运行、可测试、可恢复的用户能力。

Phase 0 的重点不是堆功能，而是让后续所有 spawn、task、message、approval、lease 和 recovery 都建立在同一套最终契约上。
