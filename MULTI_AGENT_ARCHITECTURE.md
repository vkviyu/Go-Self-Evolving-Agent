# Multi-Agent 当前架构与路线图

更新：2026-11-20。本文按当前 App 装配和源码描述实现；源码清理仍在进行，功能存在不代表本轮全量验收通过。旧 SQL 业务适配器、业务迁移和遗留日志不属于当前架构，也不提供旧库兼容承诺。

## 1. 架构与持久化边界

GSEA 用 Supervisor 管理独立 Agent Thread、Task DAG、Mailbox、调度和工作区交付；每个 Agent 的执行循环仍由 Eino 承担。控制工具与普通工具共用 Policy、审批、审计和幂等边界。

```text
CLI / TUI / control
  → App：资源装配、所有权锁、关闭顺序
  → Conversation / Supervisor：Goal、Agent、Task、Mailbox、恢复
  → Eino Turn → ModelGateway / Tool Pipeline → 受控执行器

业务事实 → ATE native Session + authority records
执行资源 → runtime SQLite
工作区互斥 → workspacedomain
大内容 → 内容寻址 artifact store
```

| 责任                                                                          | 当前存储与入口                                                             |
| ----------------------------------------------------------------------------- | -------------------------------------------------------------------------- |
| 消息、工具审计、Turn 输入/模型回执和恢复事实                                  | ATE native Session/SessionEvent；`internal/session/atestore`               |
| Team、Agent、Thread、Task、Mailbox、WaitSet、Goal、Flow、用量、合并与关闭回执 | ATE 逐实体 records；`internal/orchestration/atestore`                      |
| 带来源的跨任务记忆及采用记录                                                  | ATE Memory、Knowledge，经 `internal/experience` 访问                       |
| LoopSpec、策略候选与影子报告                                                  | ATE records，经 `internal/evolutionstore` 访问                             |
| Eino opaque checkpoint、工具快照清单与本地引用、LSP 缓存                      | runtime SQLite，由 `internal/runtimestore` 管理                            |
| 未决 ATE 请求                                                                 | runtime SQLite 的 exact request 信封；`internal/aterequest` 负责提交与对账 |
| 跨实例工作区 lease、实例/子进程存活                                           | `internal/workspacedomain` 的协调库与 flock                                |
| diff、文件内容等大对象                                                        | `internal/artifact` 的文件系统内容寻址存储                                 |

`internal/app/open.go` 使用 `OpenRuntime` 装配本地运行资源，通过受管进程与 native client 连接 ATE。Session、编排事实、策略与记忆各有仓库或服务入口。runtime SQLite 保存执行资源与未决请求，`OpenRuntime` 会拒绝旧业务库、混合格式和未知格式。

单次编排仓储变更在同一 authority batch 中提交实体、领域事件、计数器与 global/stream/team 序号索引。实体直接维护，不提供从事件重建所有状态的 replay/repair 服务。查询走 native 索引/分页；Supervisor 的内存 signal 只负责唤醒，TUI/control 用 cursor 查询事件或重载快照。

**native Session append 与 authority batch 是两个提交边界。** 不能因为它们都使用 ATE 就声称跨域原子事务；跨边界工作依靠稳定身份、阶段回执及恢复核对。没有共享的 Session/业务事务或本地业务广播队列。

## 2. 核心对象与包职责

| 对象/模块                                         | 职责                                                                                                                |
| ------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------- |
| Team                                              | 根会话对应的执行与预算范围，生命周期跨越多次 Turn                                                                   |
| Agent / Thread / Run                              | Agent 保存父子关系、Profile 和 Workspace；Thread 对应独立会话历史；Run 标识一次执行，恢复沿用原 Run，新指令另建 Run |
| Task                                              | 带版本、依赖、claim、attempt 和验证门禁的任务                                                                       |
| Mailbox / WaitSet                                 | 父子通信、结果投递与子 Agent 持久等待                                                                               |
| ExecutionContract / Flow                          | Contract 管理验收、预算与截止时间；Flow 管理受控工具流程；均复用原有执行边界                                        |
| `internal/agentmanager`                           | Supervisor、控制工具、调度准入、Task/结果结算                                                                       |
| `internal/agentprofile`                           | Profile 快照、权限和额度派生                                                                                        |
| `internal/agentruntime` / `internal/conversation` | Eino Turn、历史、模型回执、Execution binding 与恢复                                                                 |
| `internal/orchestration`                          | 领域契约、状态机和仓储接口；Task/Mailbox 不另设虚构的独立包                                                         |
| `internal/ateauthority` / `internal/aterequest`   | native batch/CAS 与 exact request 对账                                                                              |
| `internal/workspacecoord`                         | snapshot/worktree、候选、合并、检查与收尾                                                                           |

Supervisor 是具体结构体，以仓储 CAS、admission 锁及结算锁协调并发。模型/工具执行不占有全局控制锁。领域仓储不创建模型；调度器不依赖存储 schema；TUI 不直接修改持久状态。

## 3. 实际状态机

以下迁移以 `internal/orchestration/state.go` 为准；调用方还必须满足版本、所有权、依赖、当前 Run/attempt 等条件。

### Team

```text
active → draining → completed / failed
active / draining → canceling → canceled
completed / failed / canceled → active
```

`CancelTeam` 在 admission 门禁内停止新准入，中断并等待子 Agent，保留 root 会话；中断时可停在 `canceling`，再次取消可收尾。`EnsureRootThread` 重开终态 Team。`draining/completed/failed` 有状态定义，但当前会话级生产收敛调用方仍缺失，不能把定义当作自动完成能力。

### Agent

| 当前状态              | 允许目标                                                |
| --------------------- | ------------------------------------------------------- |
| `created`             | `queued`、`canceled`                                    |
| `queued`              | `running`、`canceling`、`failed`                        |
| `running`             | `queued`、`waiting`、`completed`、`failed`、`canceling` |
| `waiting`             | `queued`、`canceling`、`failed`                         |
| `canceling`           | `canceled`、`failed`                                    |
| `completed`、`failed` | `queued`                                                |
| `canceled`            | 无                                                      |

`agent_continue` 让已完成/失败的子 Agent 在同一 Thread 执行新指令，使用新 RunID；它不同于恢复。重启恢复的 `running → queued` 沿用原 RunID。可信分类的传输、限流、供应商不可用、凭据、额度或预算故障进入 `waiting`，Activity 为 `recovery:<kind>`；`agent_resume` 显式恢复原 Run 和 Task attempt，重启不自动发出新模型请求。

普通失败进入 `failed`，不自动循环重试。Task 安全改派只接受宿主可信失败分类和额外安全检查，不接受模型自报的 `retryable`。Activity 的实时展示部分保存在内存，不是完整持久执行轨迹。

### Task

| 当前状态                          | 允许目标                                                                                |
| --------------------------------- | --------------------------------------------------------------------------------------- |
| `draft`                           | `blocked`、`ready`、`canceled`                                                          |
| `blocked`                         | `ready`、`failed`、`canceled`                                                           |
| `ready`                           | `blocked`、`claimed`、`canceled`                                                        |
| `claimed`                         | `blocked`、`ready`、`in_progress`、`failed`、`canceled`                                 |
| `in_progress`                     | `blocked`、`ready`、`verification`、`awaiting_merge`、`completed`、`failed`、`canceled` |
| `verification`                    | `blocked`、`ready`、`awaiting_merge`、`completed`、`failed`、`canceled`                 |
| `awaiting_merge`                  | `blocked`、`completed`、`failed`、`canceled`                                            |
| `completed`、`failed`、`canceled` | 无                                                                                      |

依赖变更检查环，提交由 authority CAS 保护；未完成依赖阻止就绪，依赖失败不自动传播取消或替换。claim TTL 为 30 分钟，仅回收持有 Agent 不再存活的过期 `claimed` 任务；`in_progress` 不按 TTL 自动改派。

`task_update` 支持执行前的版本化编辑；`task_refine` 原子增加子任务/依赖；`task_reassign` 只在安全范围内重指派。reviewer 必须是当前 Task attempt 指派的 Agent，校验状态和 expected version，不能只凭相同 Profile 越权验证。负面结论可回到 `ready` 并递增 attempt。需要合并的候选必须经过 `awaiting_merge`，合入和检查成功后才解锁依赖。

### WaitSet、审批和 Lease

- WaitSet 持久状态为 `pending/resolved/consumed/expired/canceled`。子 Agent 的 `agent_wait` 先保存 Eino StatefulInterrupt checkpoint，再登记等待，释放运行槽位；满足条件后携原 resume token 继续。只支持等待直接子 Agent，deadline 到期不自动取消子级。root 没有 RunID，使用阻塞式等待。
- 审批由进程内 `approval.Broker` 处理，`approval.requested/resolved` 写 Session 审计。尚无可跨重启恢复的审批队列或 expired 工作流。
- 工作区 Lease 实际写入 `active/released/abandoned`，没有 TTL 或续租。实例及其后代进程都退出后才能回收；冲突返回错误或等待，不依靠时间到期推定写入安全。

## 4. 调度、上下文与通信

`scheduler.RoundRobin` 按 Team 轮转，Team 内按 Priority 降序、ID 升序。当前 Agent 候选 Priority 为 0，Task priority 未接入调度，不具备权重或关键路径优先级。

默认 `max_live=16`、`max_running=4`、`max_depth=3`、`max_children=4`，配置约束和 per-model-profile bucket 控制准入；超 running 槽位保持 queued，spawn 超 live/depth/children 上限返回错误。当前没有原提案中的固定 hard ceiling 和每 Team Task 数上限。同一 Supervisor 只同时运行一个 shared-lease 写入者。

模型请求使用 Team budget 与 reservation/settlement/release，保留供应商用量及价格未知标记。未配置预算时不施加 Team 上限；未配置价格不能据成本显示为 0 推断免费。Agent 独立预算、基于历史表现自动路由与委派收益判断仍待实现。

Profile 以不可变快照绑定 Agent，恢复不替换成当前默认 Profile。委派权限和 Limits 与父级取交集；ActorScope 由宿主注入，模型不能伪造发送者、Task attempt 或权限。内置 default/worker/reviewer；tester 专用 Profile 尚未实现。

上下文继承支持 `none/summary/recent/full`，默认 summary。当前 summary 是父 Thread 已持久化 user/assistant 文本摘录，不额外调用模型生成摘要；summary/recent/full 分别按最近 4/12/全部消息及 8000/24000/48000 字符限制，设置 MaxInheritedTokens 后再裁剪。摘录标记为不可信数据，不复制在途 delta 或 tool 消息；各 Thread 独立压缩。

Mailbox 仅允许父子通信，按 recipient 排序，采用至少一次投递与显式 ack；同幂等键异内容拒绝。子 Agent 的终态、结果和父级 `MessageResult` 在同一 authority batch 提交，结果键绑定 Run。父级从结构化结果读取摘要，完整 transcript 由用户在 TUI 检视。root 有消费账本，在模型请求前安全注入结果，空闲时可由后台 Turn 结算；恢复原 Turn 不顺带消费新到消息。

公开控制工具包括：

```text
agent_spawn / agent_list / agent_wait / agent_message
agent_interrupt / agent_continue / agent_resume
task_create / task_list / task_update / task_refine / task_reassign
task_transition / task_verify / task_claim / task_release
task_add_dependency / task_remove_dependency
merge_list / merge_run / merge_requeue
```

`agent_interrupt` 向后代传播取消。steer、`agent_inspect` 和交互式 Thread 切换仍待实施。`artifact_read`、Flow、`memory_query` 和 `memory_manage` 使用相同的工具管线。记忆写操作需要审批，采用记录绑定宿主提供的 Actor 与调用身份。

## 5. 恢复与跨提交协议

### exact request 对账

`aterequest.Executor` 在网络调用前保存原始信封，固定 request key、digest 和 ATE store identity，并在提交前持久化 attempted 标记。

1. 查询得到匹配 durable ack，校验 fence/身份后清理 pending。
2. 只有尚未 attempted 且查询确认不存在的信封，才可进入首次提交。
3. 已 attempted 的请求查询不到，仍为 unknown：无法区分未送达和拒绝响应丢失，**不能自动重发，也不能换 key/digest 绕过**。
4. 已提交但本地清理失败是已知成功，只补清理；明确拒绝的清理失败仍是拒绝，不重新执行业务。

### 所有权与恢复范围

App 持有 runtime 数据库所有权锁，并按 ATE store identity + Session ID 获取共享会话执行锁。Supervisor 只枚举当前持有会话的 Team，reservation、Task claim 和 completion 恢复都受范围约束；共享 ATE 历史不授予全局执行权。

离线归档取得 runtime 和会话锁后才对账，无法证明作用域的未派发请求继续保留。工作区 lease 另由 `workspacedomain` 管理。

### Turn、工具与 Agent 完成

`turn.started` 保存输入、执行配置与快照身份；模型成功回执绑定请求摘要和输入快照。checkpoint 缺失时可按已确认模型响应及工具回执恢复原 Turn；缺失必要证据则拒绝恢复，不猜测输入、不重新注入 Instruction 或新 mailbox。

工具先持久化 `tool.started` 再执行副作用；只有 started、没有确定结果时为 `OutcomeUnknown`，不得自动重做。已确认的成功、拒绝和已知错误按原调用身份复用。

`turn.failed` 与对应的 `checkpoint.created` 事实在同一次 native Session append 中提交；opaque checkpoint 字节仍是独立本地资源。策略 Fallback 是之后的独立 authority 提交，不能声称它与 Turn 失败原子联动；两侧错误都会向上传递。

Supervisor 先冻结 completion receipt，再结算 Task、候选交付、Agent 终态与通知，最后标记 receipt settled。中间失败保留原结果，所属 Team 恢复时补做协调，不重新运行成功的 worker。Agent 终态与父级通知原子提交不等于整个完成链条处在同一事务。

### Execution admission saga

Execution admission 绑定 Session/actor、输入/契约/配置摘要、Turn、Execution 与 Step，按版本推进：

```text
reserved → turn_started → execution_started → consumed
未完成且无法核实的绑定 → unavailable
```

`Prepare`、Session `turn.started`、`BindTurn`、Execution/Step、`Consume` 是跨提交协调步骤，不是一个大事务。Task/Execution/Step 的 authority batch 不包含 Session append。

具体顺序由宿主的执行协议决定：先固定 Query Plan/Task 的 ExecutionContract，再绑定 Turn、Execution 和 Step；每个阶段都通过 ATE 事实与 CAS 校验推进。恢复必须读取绑定的实际状态并沿用原身份；缺少完整绑定时进入 unavailable，不能按普通聊天重放。

归档与显式放弃按阶段取得回执。先提交权威关闭事实，再清理本地恢复引用。失败时仅补做有回执依据的后续步骤。

## 6. 工作区、合并与安全

| View              | 当前行为                                                       |
| ----------------- | -------------------------------------------------------------- |
| `shared-readonly` | 共享只读目录；写工具拒绝                                       |
| `shared-lease`    | 共享写入；结构化写操作获取路径 lease，shell 获取整工作区 lease |
| `snapshot`        | 私有副本，不依赖 Git；三方合并后回写主工作区                   |
| `git-worktree`    | 自定义 Profile 可选择；从 HEAD 建立 worktree，交付 git-patch   |

`default_workspace=auto` 在装配 snapshot store 时给写入者 snapshot；显式 readonly 请求进一步收窄权限。snapshot 存在私有数据目录，artifact 也归 runtime 所有。

路径 lease 按规范路径及父子前缀判断冲突；当前是单操作获取/释放，没有多路径原子 WriteIntent，也没有 shared-lease 写前 fingerprint 检查。shell 没有只读分类器或 `write_paths` 声明。lease 负责互斥，沙箱负责执行能力，两者均不能撤销已发生的副作用。

合并候选状态为 `pending/running/applied/conflicted/rejected/superseded`：

- snapshot：候选 manifest、当前主目录与基线进行三方比较，生成 ApplyPlan；审批绑定 plan digest，主目录变化需重建/重新审批。在工作区屏障下保存 native apply 进度并提交；恢复核对内容，不盲目覆盖。
- git-patch：采集 patch artifact，审批后在工作区屏障内 `git apply --3way`；当前未统一到 snapshot 的逐路径 apply 协议，可能留下冲突标记。
- 冲突生成 Resolution Task；snapshot resolution 在预置候选的隔离副本中产出替代候选。supersede 限制在同 Task/明确 resolution 链或同 Agent/view。
- reviewer 使用候选的只读 view；待处理/运行/冲突候选冻结后禁止对应 Agent Continue/reuse，避免审查期间改写。

`post_merge_check` 是宿主配置的只读沙箱命令，运行期间保持整工作区屏障。持久 finalization 绑定候选、patch/plan、主目录内容和检查配置摘要，检查阶段为 prepared→dispatched→completed/unknown。派发后结果未知不自动重跑；失败、unknown 或内容偏离使 Task failed，依赖继续阻塞，已合入文件保留。无此配置时没有额外命令检查，也没有第二轮审查 Agent。

安全入口保留项目配置显式信任、Schema 参数校验、权限交集、审批、`os.Root` 路径隔离、受限环境与 OS 沙箱。当前通用宿主 Git 生命周期仍有未解决边界：worktree checkout 可能触发仓库配置的 smudge/process filter；不能由补丁采集已加固推断所有 Git 操作都安全。

## 7. UI、容量与当前限制

TUI `/agents`（`/activity`）可查看 Agent/View/Lease/merge、只读子 transcript、取消子 Agent和处理合并候选。headless `control` 提供快照、事件 cursor 分页及 Agent/Team/Task/merge 操作；尚不是完整订阅协议。

Mailbox body 和 Task description 的领域上限为 1 MiB；native 单记录/批次限制还可能更小，原子操作不能为绕过限制拆批。artifact 单个最多 32 MiB，`artifact_read` 单次最多 1 MiB；超大 transcript、diff、日志应使用 artifact 或已支持的 Session payload 引用机制。

当前没有独立的 event payload 通用上限、已验证 p95 指标或 TUI 固定帧率承诺。历史引用扫描仍可能 O(n)，artifact 没有自动保留/清理策略。

其他明确限制：

- Team cancel 尚未提供等待审批的后台 merge 的独立 per-Team 取消/等待生命周期。
- 未实现 durable 审批、steer、Agent 独立预算、关键路径调度、worktree 池及所有权 epoch/revoking。
- 未实现分布式 worker、跨机器副作用 exactly-once、任意历史 checkpoint 回滚、自动撤销文件/网络副作用。
- Linux 实机隔离、Shell 子进程资源限制和宿主总 RSS 上限不能由 macOS 测试推定；退役扩展编译器不再属于待完善能力。

## 8. 分阶段路线图

阶段表示产品能力范围，验收需针对清理后的当前源码重新执行。

| 阶段                | 已有能力                                                                        | 保留计划                                                                                                                        |
| ------------------- | ------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------- |
| Phase 0：领域与存储 | typed IDs、状态机、ActorScope、native Session/authority、事件索引、Profile 快照 | 完整状态重建/repair 未实现                                                                                                      |
| Phase 1：Supervisor | 动态 spawn、独立 Thread、父子取消、恢复、Continue/Resume                        | 更完整的检查与交互控制                                                                                                          |
| Phase 2：有界并行   | 槽位/bucket、RoundRobin、Mailbox、WaitSet、Team budget、结果回收                | steer、Agent budget                                                                                                             |
| Phase 3：Task DAG   | 依赖/claim、验证 gate、版本化编辑、细化、安全改派                               | tester Profile、关键路径优先                                                                                                    |
| Phase 4：工作区交付 | 四种 view、lease、snapshot/git-patch 合并、Resolution Task、可配置检查          | Git 生命周期安全、统一 apply 协议、worktree 池、unknown 检查人工恢复                                                            |
| Phase 5：高级编排   | 已有局部 control 与冲突自动建任务                                               | 委派收益评估、跨 provider 路由、历史表现选 Profile、自动修订/验证任务、Team 模板、可视化 DAG、完整 headless 协议、容量/效果评测 |

Phase 5 整体尚未实现。评测目标是受预算、权限和冲突约束的 wall time 或验证覆盖改善，不能以更多并行请求代替效果证明。执行并行的专项计划仍由 `MULTI_AGENT_PARALLEL_PLAN.md` 管理。

## 9. 后续验收与待核实

2026-11-24 已运行全量测试：除 CLI 的 `TestControlScopeProductionJSONL` 因 ATE 运行库所有权冲突失败外，其余包通过。build、vet 和 tool/runtimestore/app 定向 race 通过，具体范围见[本轮验证记录](IMPLEMENTATION_REMAINING.md#验证状态与待核实项)。后续变更应继续覆盖以下边界：

1. native batch/CAS、事件分页、同 key 异内容冲突、attempted unknown 不重发、store identity 更换拒绝。
2. 不同 runtime 共享 ATE 时的 Session 锁、所属 Team claim/reservation/completion 恢复及离线维护作用域。
3. Execution binding 各阶段崩溃；Session 与 authority 分别失败；Turn 失败与策略回退分别失败。
4. checkpoint 缺失、模型回执恢复、工具 OutcomeUnknown、WaitSet 取消/超时、Mailbox 重复投递。
5. reviewer/attempt 校验、细化/改派安全、候选冻结、snapshot apply 中断、检查 dispatched 后未知。
6. 关闭/归档与本地快照引用释放之间的失败、关闭后的恢复准入、取消时资源所有权保留。

对应测试集中于 `internal/orchestration/atestore`、`internal/session/atestore`、`internal/aterequest`、`internal/app`、`internal/agentmanager`、`internal/agentruntime`、`internal/conversation` 和 `internal/workspacecoord`，本地执行资源由 `internal/runtimestore` 管理。旧 SQL 升级测试与过去的全库通过记录不能作为当前验收证据。
