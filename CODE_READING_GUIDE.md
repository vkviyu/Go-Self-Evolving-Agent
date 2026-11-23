# 代码阅读指南

阅读这个项目的目标，是能沿一次请求说明：谁决定下一步、从哪里读取已发生的事实、什么提交成功后才能继续、恢复时哪些动作不能重做。先读 [架构导读](ARCHITECTURE.md) 的工作流与概念表，再按下列路线进入源码。

本指南定位当前生产路径，不保留逐文件 SQL 审计清单、迁移编号或旧问题时间线。测试文件是行为契约的阅读材料；本文没有声称重新运行或通过这些测试。包名以当前源码为准，待改名项列在文末。

## 第一遍：沿一个请求走通

| 顺序          | 先读这些入口                                                                                                                                                       | 读完应能回答                                                       |
| ------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------ |
| 1. 用户入口   | [main.go](cmd/gsea/main.go)、[chat.go](internal/cli/chat.go)、[conversation_builder.go](internal/cli/conversation_builder.go)                                      | 输入怎样进入 Conversation？交互与 headless 的审批和输出如何接线？  |
| 2. 资源装配   | [open.go](internal/app/open.go)、[authority.go](internal/app/authority.go)、[startup_team.go](internal/app/startup_team.go)                                        | 哪些仓库指向 ATE？哪些对象是本地资源？root Session 如何关联 Team？ |
| 3. 请求与执行 | [run.go](internal/conversation/run.go)、[execution.go](internal/conversation/execution.go)、[execution_contract.go](internal/execution/contract.go)                | 普通 Turn 如何开始；需要协作、预算或验收时，执行契约如何固定？     |
| 4. 回合准入   | [factory_turn.go](internal/agentruntime/factory_turn.go)、[eino_turn.go](internal/agentruntime/eino_turn.go)、[turn.go](internal/agentruntime/turn.go)             | 工具快照何时固定？输入何时耐久？Eino 什么时候才开始调用模型？      |
| 5. 模型与工具 | [model.go](internal/agentruntime/model.go)、[gateway.go](internal/modelgateway/gateway.go)、[pipeline.go](internal/tool/pipeline.go)                               | 请求、权限、外部副作用与结果分别在哪个边界发生？                   |
| 6. 结果与后续 | [turn_stream.go](internal/agentruntime/turn_stream.go)、[conversation.go](internal/conversation/conversation.go)、[recovery.go](internal/conversation/recovery.go) | Turn 执行完成、Task 验收和用户收到最终结果有什么区别？             |

第一遍可以先跳过模型供应商协议、存储 codec 和全部工具实现。到第 5 步时选择一个真实工具，例如 [write.go](internal/codingtools/write.go) 或 [shell.go](internal/codingtools/shell.go)，观察 Pipeline 如何把领域调用连接到实际效果。

## 用类型理解概念，不从同名词猜关系

| 要理解的概念                | 类型或入口                                                                                                                                                                               | 重点                                                                                        |
| --------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------- |
| Session / Turn              | [event.go](internal/session/event.go)、[turn.go](internal/agentruntime/turn.go)                                                                                                          | `EventStore` 与 `CheckpointStore` 分离；一个 Turn 可以有多次模型/工具调用                   |
| ExecutionContract / Binding | [contract.go](internal/execution/contract.go)、[binding.go](internal/execution/binding.go)、[task.go](internal/orchestration/task.go)                                                    | canonical hash、不可变绑定、预算、截止时间、attempt 与 Turn/Task 的关联                     |
| 编排 Task                   | [task.go](internal/orchestration/task.go)、[state.go](internal/orchestration/state.go)                                                                                                   | 依赖、claim、验收、`awaiting_merge` 和终态；不是 ATE Task 的状态机                          |
| Agent / Team / Thread / Run | [agent.go](internal/orchestration/agent.go)、[team.go](internal/orchestration/team.go)、[root.go](internal/orchestration/root.go)、[id.go](internal/orchestration/id.go)                 | `AgentSnapshot` 连接这些身份；Thread 是编排身份，Session 承载有序历史；Run 与 Turn 分别保存 |
| Flow Run / Step             | [types.go](internal/agentflow/types.go)、[runtime.go](internal/agentflow/runtime.go)                                                                                                     | 定义 revision/fingerprint、幂等输入、持久步骤和工具快照                                     |
| Agent profile               | [profile.go](internal/agentprofile/profile.go)、[profile.go](internal/agentruntime/profile.go)                                                                                           | 配置快照必须约束实际模型、工具和工作区访问，不能只改提示词                                  |
| 工具快照                    | [snapshot_identity.go](internal/tool/snapshot_identity.go)、[snapshot_manifest.go](internal/tool/snapshot_manifest.go)、[snapshot_persistence.go](internal/tool/snapshot_persistence.go) | 内容地址、实现身份、产物解析与 consumer 引用；generation 不是恢复身份                       |

ATE 的 `agentprofile` 是另一仓库中的持久化状态机协议。要看本项目怎样使用它，读 [execution.go](internal/atebridge/execution.go)：`EnsureExecution` 为 Turn 建立记录用途的 Task/Execution/Step，供经验与证据作用域使用。不要把它解释为宿主 Task DAG 的调度实现，也不要把 Flow Step 当作 ATE Step。

## 第二遍：确认事实到底写在哪里

先对照 [open.go](internal/app/open.go) 的实际赋值，再读适配器。这样可以避免把同目录残留的旧实现误认为当前生产入口。

| 阅读路线         | 关键代码                                                                                                                                                                                                                | 要核对的边界                                                                                                        |
| ---------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------- |
| Session 原生历史 | [store.go](internal/session/atestore/store.go) → [append.go](internal/session/atestore/append.go) → [payload.go](internal/session/atestore/payload.go) → [read.go](internal/session/atestore/read.go)                   | Session sequence CAS、整批 append、payload 引用、读容量；本地 pending 不保存已提交历史镜像                          |
| 业务 state 事务  | [store.go](internal/ateauthority/store.go) → [tx.go](internal/ateauthority/tx.go)                                                                                                                                       | 固定 view、根 CAS、有界 write set；已明确拒绝的冲突才允许重算 callback，callback 不得有外部副作用                   |
| 编排仓库         | [core.go](internal/orchestration/atestore/core.go) → [root.go](internal/orchestration/atestore/root.go) → [tasks.go](internal/orchestration/atestore/tasks.go) → [events.go](internal/orchestration/atestore/events.go) | 一个 batch 中包含哪些状态、索引和事件；“存在 event 类型”本身不能证明事务边界                                        |
| 准确请求与核对   | [executor.go](internal/aterequest/executor.go) → [authority.go](internal/app/authority.go)                                                                                                                              | 发送前保存原 envelope、BeginAttempt、回执验证、已提交但 pending 清理失败，以及 attempted 后查无记录为何仍是 unknown |
| 运行资源         | [runtime_database.go](internal/runtimestore/runtime_database.go) → [runtime_snapshots.go](internal/runtimestore/runtime_snapshots.go)                                                                                   | 当前 DDL、authority 绑定、checkpoint/插件/快照等资源；旧业务格式被拒绝，不自动迁移                                  |
| 自进化业务记录   | [store.go](internal/evolutionstore/store.go)、[strategies.go](internal/evolutionstore/strategies.go)、[plugin_builds.go](internal/evolutionstore/plugin_builds.go)                                                      | 业务证据写 ATE；WASM blob、运行版本和快照资源另由本地服务保存                                                       |

读日志时先确定是哪一种：SessionEvent 按 Session sequence 组织；orchestration event 按业务 stream 与 global sequence 组织；ATE 物理 journal 按存储提交组织。`turn_journal.go` 的模型回放与 workspace apply journal 又有各自用途，它们不能替代请求回执。

## 按问题深入

### “执行完成了，为什么还没有最终完成？”

按下面顺序区分结果与收尾：

1. [turn_stream.go](internal/agentruntime/turn_stream.go) 和 [completed_turn.go](internal/agentruntime/completed_turn.go)：Turn 的候选输出和耐久终态。
2. [candidate_events.go](internal/conversation/candidate_events.go)、[goal_loop.go](internal/conversation/goal_loop.go)、[goal_transition.go](internal/conversation/goal_transition.go)：候选缓冲、验收、继续和停止。
3. [supervisor_run.go](internal/agentmanager/supervisor_run.go)、[agent_completion.go](internal/orchestration/agent_completion.go)、[agents.go](internal/orchestration/atestore/agents.go)：先保存工作结果，再完成协调结算。
4. [finalization.go](internal/workspacecoord/finalization.go)：文件应用后的外部验证与 Task/view 结算。

尤其检查这两条**分开提交**的路径：

- `internal/orchestration/atestore`：Task、Execution、Step 和 recovery 的 native state batch 不包含 Session 的 `turn.started` append。结合 [execution_contract.go](internal/orchestration/execution_contract.go) 和 [recovery.go](internal/conversation/recovery.go) 看执行绑定和中断窗口；不要把跨存储步骤误读成单一事务。
- [turn_durability.go](internal/agentruntime/turn_durability.go) 与 [strategy.go](internal/agentruntime/strategy.go)：`turn.failed` 追加之后另行尝试 strategy fallback。它们不是一个事务，fallback 也不触发自动重放。

### “重启后应该继续哪一步？”

先读 [recovery.go](internal/conversation/recovery.go) 的只读发现，再读 [resume.go](internal/conversation/resume.go) 与 [goal_recovery.go](internal/conversation/goal_recovery.go)。区分执行、验收、已有 verdict 和 unavailable，避免把“恢复”统一理解成重跑 Turn。

随后核对 [startup_recovery.go](internal/app/startup_recovery.go)、[authority_scope.go](internal/app/authority_scope.go) 和 [recovery.go](internal/session/atestore/recovery.go)：只有拥有对应 Session 的 runtime 才能推进该 Session 的请求计划；普通启动核对 ACK，不自动提交所有旧业务请求。

执行层继续读 [factory_turn.go](internal/agentruntime/factory_turn.go)、[turn_journal.go](internal/agentruntime/turn_journal.go)、[outcome.go](internal/tool/outcome.go) 与 [supervisor_recovery.go](internal/agentmanager/supervisor_recovery.go)。重点看原 Run/Turn/Call ID、快照与模型兼容性、已完成结果回放，以及没有确定结果的副作用如何停止。

### “子 Agent 怎样领任务、等待和交付？”

从 [supervisor.go](internal/agentmanager/supervisor.go)、[supervisor_tasks.go](internal/agentmanager/supervisor_tasks.go) 和 [supervisor_scheduler.go](internal/agentmanager/supervisor_scheduler.go) 开始，再追到 [agents_spawn.go](internal/orchestration/atestore/agents_spawn.go)、[task_dependencies.go](internal/orchestration/atestore/task_dependencies.go)。

调度要看 Team 是否 active、父 Agent 状态、运行槽、模型 bucket 和 shared writer 限制。Task 重指派读 [supervisor_reassign.go](internal/agentmanager/supervisor_reassign.go)、[task_reassign.go](internal/orchestration/atestore/task_reassign.go)：可信的 typed failure 与原执行证据比失败文字更重要；结果未知不能直接当作安全重试。

等待与消息读 [supervisor_wait.go](internal/agentmanager/supervisor_wait.go)、[mailbox.go](internal/orchestration/atestore/mailbox.go)、[wait.go](internal/orchestration/atestore/wait.go) 和 [background.go](internal/agentruntime/background.go)。恢复原 checkpoint 时，新到的 mailbox 消息不能被误认为已经交给模型并确认消费。

### “并行修改怎样进入主工作区？”

读 [supervisor_workspace.go](internal/agentmanager/supervisor_workspace.go) 确认工作区模式，再沿 [snapshot.go](internal/workspacecoord/snapshot.go) → [candidate.go](internal/workspacecoord/candidate.go) → [merge_snapshot.go](internal/workspacecoord/merge_snapshot.go) → [applyplan.go](internal/workspacecoord/applyplan.go) → [applycommit.go](internal/workspacecoord/applycommit.go) → [finalization.go](internal/workspacecoord/finalization.go)。

这些步骤分别涉及副本、冻结候选、三方合并、文件应用和验证结算。不要把文件应用成功等同于 Task 完成。实际资源互斥另读 [domain.go](internal/workspacedomain/domain.go) 和 [root_io.go](internal/codingtools/root_io.go)：前者协调实例/租约，后者保护具体文件访问。共享可写子 Agent 当前串行；隔离或只读工作才使用其他并行槽。

### “profile、Flow 和插件怎样共享工具治理？”

读 [profile.go](internal/agentruntime/profile.go)、[context.go](internal/tool/context.go)、[pipeline.go](internal/tool/pipeline.go)，确认可信 ActorScope 与 profile guard 如何到达每次调用。再读 [operators.go](internal/agentflow/operators.go) 和 [runtime.go](internal/agentflow/runtime.go)：控制算子由宿主执行，工具/编排效果通过 Pipeline。

插件服务由 [open.go](internal/app/open.go) 接线。进一步读 [plugin](internal/plugin)、[pluginbuild](internal/pluginbuild) 与 [wasm](internal/plugin/wasm)，重点理解构建、验证、发布和调用的边界。已开始的 Turn/Flow 固定快照，新发布不会把旧快照中的实现替换成当前版本。

### “上下文、经验和自进化分别改了什么？”

| 方向           | 入口                                                                                                                                                                                             | 阅读重点                                                       |
| -------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | -------------------------------------------------------------- |
| 当前模型上下文 | [context_policy.go](internal/agentruntime/context_policy.go)、[context_artifacts.go](internal/agentruntime/context_artifacts.go)、[compaction.go](internal/agentruntime/compaction.go)           | 压缩决策、外置大结果和保留消息；区分上下文窗口与累计预算       |
| 请求与用量     | [model_accounting.go](internal/agentruntime/model_accounting.go)、[usage.go](internal/orchestration/atestore/usage.go)、[usage_recovery.go](internal/orchestration/atestore/usage_recovery.go)   | 预留、确定响应和恢复估算；不要把估算写成供应商账单或严格上限   |
| 经验复用       | [experience.go](internal/agentruntime/experience.go)、[service.go](internal/experience/service.go)、[prepare.go](internal/experience/prepare.go)、[complete.go](internal/experience/complete.go) | Prepare / Check / Complete、历史截点、来源证据和独立维护状态   |
| 策略与工具进化 | [evolution_scope.go](internal/app/evolution_scope.go)、[strategy](internal/strategy)、[evolutionstore](internal/evolutionstore)                                                                  | 候选、评估与激活分别由谁负责；模型建议怎样经过宿主校验和持久化 |

### “只读查询、归档和关闭为什么有独立入口？”

[control.go](internal/app/control.go) 的 `OpenControl` 只连接已有 authority，不打开 runtime 数据库、不取得 Session 执行权、不恢复请求或启动 Runner。后续写控制操作才按其明确范围取得所有权；不要把只读入口的性质扩展为所有 control 命令都只读。

保留期关闭读 [retention.go](internal/orchestration/atestore/retention.go) 和 [runtime_retention.go](internal/runtimestore/runtime_retention.go)。其顺序是：

```text
ATE 业务关闭计划 / 准入墓碑
    → Session 审计事件
    → 完成的业务回执
    → 本地快照引用释放
```

不同维护操作清理的资源集合不同。ATE 请求 ACK、完成的业务 receipt、本地 closure/release 是三个层次；本地引用只提供资源候选，不能覆盖 ATE 的生命周期事实。清理不撤销已发生的外部效果。

进程关闭另读 [close.go](internal/app/close.go)：停止工作、等待退出、释放资源的顺序很重要。worker 超时时，当前实现保留所有权直到进程退出，不承诺任意 Go 代码都能被强制取消。

## 将测试作为行为示例阅读

以下是有针对性的阅读入口，不是本次执行结果或全量覆盖声明。

| 关注点                              | 测试入口                                                                                                                                                                                                  |
| ----------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 请求丢响应、unknown 与 pending 清理 | [executor_test.go](internal/aterequest/executor_test.go)、[recovery_test.go](internal/ateauthority/recovery_test.go)                                                                                      |
| Session 原生批次与 payload          | [native_test.go](internal/session/atestore/native_test.go)、[payload_test.go](internal/session/atestore/payload_test.go)                                                                                  |
| 执行契约与身份边界                  | [contract_test.go](internal/execution/contract_test.go)、[pipeline_test.go](internal/tool/pipeline_test.go)、[task_retry_authority_test.go](internal/orchestration/atestore/task_retry_authority_test.go) |
| 快照准入和工具结果未知              | [snapshot_admission_test.go](internal/agentruntime/snapshot_admission_test.go)、[outcome_test.go](internal/tool/outcome_test.go)                                                                          |
| Task 重试依据和原生历史 fence       | [task_retry_authority_test.go](internal/orchestration/atestore/task_retry_authority_test.go)                                                                                                              |
| 工作完成后的协调结算                | [supervisor_completion_test.go](internal/agentmanager/supervisor_completion_test.go)、[supervisor_delivery_test.go](internal/agentmanager/supervisor_delivery_test.go)                                    |
| Flow 的 native 恢复                 | [native_recovery_test.go](internal/agentflow/native_recovery_test.go)、[snapshot_recovery_test.go](internal/agentflow/snapshot_recovery_test.go)                                                          |
| 执行所有权与只读边界                | [authority_scope_test.go](internal/app/authority_scope_test.go)、[control_scope_test.go](internal/app/control_scope_test.go)                                                                              |
| ATE 业务关闭与本地资源释放          | [retention_native_test.go](internal/orchestration/atestore/retention_native_test.go)、[runtime_retention_test.go](internal/runtimestore/runtime_retention_test.go)                                        |
| 包依赖与资源关闭                    | [architecture_test.go](internal/architecture/architecture_test.go)、[close_test.go](internal/app/close_test.go)                                                                                           |

修改某个行为时，应选择覆盖该边界及中断窗口的测试，并记录实际执行结果。文档中的测试链接只说明去哪读，不能替代测试运行。

## 当前名称与阅读时的检查点

`internal/runtimestore` 是运行资源入口：checkpoint、插件/LSP/快照引用与未完成 ATE 请求。它不含业务历史；`OpenRuntime` 拒绝旧业务库或混合格式，不隐式迁移。

`internal/ateports` 定义宿主对 ATE 的历史读取与决策状态端口，没有投递队列或镜像语义。`internal/atebridge` 通过它承接经验与执行生命周期；`agentruntime/turn_journal.go` 是现用的模型回放逻辑，不应按名称归为已删除的 delivery journal。

读完一条调用链，应能明确指出：业务事实属于哪个仓库、哪个命令是一次原子提交、哪里可能结果未知、哪一个回执允许下一步，以及当前本地资源是否足以恢复。如果这些答案仍依赖“所有操作都在一个事务里”或“重跑即可”，应回到对应的提交与恢复分支继续核对。
