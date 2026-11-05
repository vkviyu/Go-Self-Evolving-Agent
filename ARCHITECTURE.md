# 架构导读

这个项目是一个本地优先的 Agent Runtime：Eino 负责 Agent Loop，项目负责模型协议、工具治理、会话耐久化和插件沙箱。阅读代码时按依赖方向从外向内走，不要从 `cmd` 直接跳进 Eino 实现细节。

Multi-Agent 的完整目标和分阶段交付基线见 [Multi-Agent 子系统架构](./MULTI_AGENT_ARCHITECTURE.md)；当前已落地 Phase 0 基线、Phase 1 的 durable Supervisor、恢复和 Agent control tools，以及 Phase 2 的有界并行、durable mailbox、root 消息接入和幂等请求校验。Phase 3 的 Task DAG 基础闭环已实现：依赖传播、递归 CTE 环检测、claim/release、`ReapExpiredTaskClaims` 的 TTL 回收、Task-Agent 生命周期联动、`dispatchReadyTasks` 自动派发和 reviewer 验收 gate（`task_verify`）；关键路径优先级尚未实现；任务细化和失败任务重新指派已落地。Phase 4 Workspace Coordinator 的基础闭环已实现（snapshot/worktree view、三方合并、ApplyPlan、apply journal、`merge_apply` 审批、Resolution Task）；Phase 5 未实现。面向使用的并行改造按 [Multi-Agent 并行执行改造计划](./MULTI_AGENT_PARALLEL_PLAN.md) 交付：P0–P3 已完成（P3 为 `/agents` 交互面板、后台唤醒和 mailbox 消费账本），P4（同目录并行写入）未实现。

团队恢复的当前底座包括：终态 Agent 结果与父级 mailbox 通知原子提交、completion receipt 恢复结算、可恢复错误的 `recovery:<kind>` 暂停与 `agent_resume`、以及在 Eino checkpoint 缺失时使用 Turn journal 重放已确认模型/工具事实。恢复测试的验收标准见 [Multi-Agent 子系统架构 §23.1](./MULTI_AGENT_ARCHITECTURE.md#231-team-可靠恢复验收标准)。 Goal admission 使用 v41 token 绑定 session、actor、输入、契约、配置、Turn 和首轮；中间崩溃只会进入可审计的 unavailable 或按原 token 补绑定，不会普通执行恢复。

## 分层与依赖

```text
cmd/local-agent                     进程入口与信号生命周期
        |
        +--> internal/cli            命令解析与应用编排
        |      +--> internal/tui    Bubble Tea 终端状态机
        |      +--> internal/render headless Markdown/ANSI 渲染
        |      +--> internal/conversation --> internal/agentruntime
        |      |          +--> internal/agentgoal
        |      +--> internal/sessionregistry  $GSEA_HOME/control.db Session 目录
        |      +--> internal/app
        |             +--> internal/evolutiontools (JSON / scoped caller adapters)
        |             +--> internal/orchestration / internal/agentprofile
        |             +--> internal/agentflow
        |             +--> internal/pluginbuild --> internal/plugin
        |             +--> internal/strategy (Catalog / ShadowService)
        |             +--> internal/agentmanager (Supervisor)
        |             +--> internal/workspacecoord / internal/workspacedomain
        |
        |          +--> internal/plugin ----------> internal/plugin/wasm
        |          +--> internal/tool ------------> internal/policy
        |          +--> internal/codingtools -----> internal/lsp
        |          +--> internal/session.EventStore / CheckpointStore
        |
        +--> internal/modelgateway   Provider/HTTP 协议边界
        +--> internal/config         layered configuration loader

internal/agentruntime
        +--> internal/einoadapter --> internal/tool
        +--> internal/session.EventStore / CheckpointStore

internal/orchestration
        +--> internal/session (root Thread 的 Session 映射)

internal/session/sqlitestore
        +--> internal/orchestration (orchestration event/projection store)
```

依赖只能向下指向稳定边界：CLI 不直接创建 SQLite、WASM 或 Eino Tool；Eino 不直接执行工具实现；工具必须经过 `tool.Pipeline`；模型凭据只能在 `modelgateway` 内从环境变量解析。

## 目录职责

| 目录 | 负责什么 | 从哪里开始读 |
| --- | --- | --- |
| `cmd/local-agent` | 进程入口、信号生命周期和退出码转交 | `main.go` |
| `internal/cli` | 命令解析、配置覆盖、应用编排和非 TTY headless 协议；维护命令先解析选项再执行 | `runner.go`、`chat_options.go`、`conversation_builder.go`、`chat_headless.go`、`maintenance_options.go`、`plugins_options.go`、`strategy_options.go` |
| `internal/app` | Composition Root，组装依赖与输入适配、拥有资源生命周期和关闭顺序、提供 doctor 诊断 | `app.go`（依赖清单）、`open.go`、`close.go`、`agent_factory.go`、`orchestration.go`、`builtins.go` |
| `internal/codingtools` | 内置 Coding Agent workspace 工具集；工作区生命周期、路径边界、读写搜索、Shell 和通用限制分层 | `workspace.go`、`paths.go`、`read.go`、`search.go`、`write.go`、`shell.go` |
| `internal/lsp` | 可选 LSP stdio/JSON-RPC 运行时；按 workspace view 管理 server、同步文档并筛选 diagnostics | `manager.go`、`client.go`、`protocol.go` |
| `internal/config` | 按层叠顺序加载 TOML、profile、项目配置和环境变量；解析本地目录且不保存密钥 | `config.go`、`load.go` |
| `internal/conversation` | 串行化一个 session 的 turn；上下文、Inner Turn、Goal 准入/验收/恢复和历史视图各有边界 | `conversation.go`、`run.go`、`execution.go`、`resume.go`、`goal_loop.go`、`goal_transition.go`、`history.go` |
| `internal/agentgoal` | Goal Mode 输入分流、durable 状态、round/report 协议、语义判定与硬上限 | `activation.go`、`types.go`、`evaluator.go`、`tool.go` |
| `internal/agentflow` | 宿主命名 Flow Catalog、流式组合算子、Tool Pipeline 编译和 checkpoint 恢复 | `catalog.go`、`operators.go`、`runtime.go` |
| `internal/render` | headless 流式 Markdown、基于 Chroma 的代码高亮和 ANSI 样式 | `markdown_stream.go` |
| `internal/evaluation` | 语言无关 benchmark task/result 协议、隔离 Git worktree/fixture copy、环境 capability 预检、隐藏 sandbox/Docker verifier 和 SWE-bench prediction 导出 | `types.go`、`runner.go`、`workspace.go`、`capabilities.go`、`verifier.go`、`export.go` |
| `internal/tui` | Bubble Tea raw terminal 事件循环、输入编辑、滚动、流式状态、Markdown 和布局 | `run.go`、`model.go`、`keys.go`、`commands.go`、`turn_state.go`、`view.go`、`transcript.go` |
| `internal/agentruntime` | 创建固定工具快照的 Eino Runner，执行/恢复单个 Turn；上下文策略、压缩提交和历史投影分离 | `factory.go`、`config.go`、`factory_turn.go`、`eino_turn.go`、`turn.go`、`context_policy.go`、`compaction.go`、`history.go` |
| `internal/orchestration` | Multi-Agent 控制面稳定契约：类型化 ID、Actor Scope、Agent/Task 状态机、事件 envelope 和 root Thread 模型 | `id.go`、`state.go`、`event.go`、`root.go`、`task.go` |
| `internal/agentprofile` | Agent Profile 校验、不可变快照和 generation 注册表 | `profile.go` |
| `internal/agentmanager` | Supervisor 生命周期、容量准入、并行运行槽位、mailbox、恢复、等待/取消、workspace 分配与 merge 投递、统一 Team 查询快照，以及 Agent/Task/merge control tools | `supervisor.go`、`supervisor_scheduler.go`、`supervisor_run.go`、`supervisor_tasks.go`、`supervisor_workspace.go`、`supervisor_merge.go`、`activity.go`、`tools_bindings.go` |
| `internal/scheduler` | 模型并发桶（`BucketSet`）与按 Team 轮转的候选排序 `RoundRobin`（旧名称保留兼容别名），Team 内按 Priority 排序 | `scheduler.go` |
| `internal/workspacecoord` | Workspace view（shared/readonly/git-worktree/snapshot）、快照候选与审阅、三方合并、ApplyPlan 与 apply journal、Resolution Task、Git 补丁保留 | `coordinator.go`、`snapshot.go`、`merge3.go`、`applyplan.go`、`applycommit.go`、`resolution.go`、`preservation.go` |
| `internal/workspacedomain` | 跨数据目录/跨 `GSEA_HOME` 的同一工作区协调：dev+inode 身份、实例登记、flock、写 lease 和 rendezvous；自有 `coord.db` | `domain.go`、`identity.go`、`instance.go`、`leases.go`、`rendezvous.go`、`store.go` |
| `internal/modelgateway` | OpenAI Chat Completions、Responses、Ollama 的构造；Responses SSE 转成 Eino 流 | `gateway.go`、`responses.go`、`responses_transport.go` |
| `internal/einoadapter` | 把统一工具绑定成 Eino Tool，并注入 session/turn/snapshot 元数据 | `tool.go` |
| `internal/evolutiontools` | 自进化工具描述、严格 JSON 解码、候选/报告归属校验、业务服务调用与结果编码；不依赖 App、SQLite 或 Eino | `ports.go`、`plugin_bindings.go`、`strategy_bindings.go`、`plugin.go`、`strategy.go`、`strategy_scope.go` |
| `internal/tool` | 不可变 registry snapshot 和统一 proposed-policy-approval-execute-result 管线 | `registry.go`、`pipeline.go`（`NewPipeline`、分阶段执行） |
| `internal/toolbridge` | 评测 harness 的 invocation-scoped Unix-socket 工具适配；只负责调用传输，不拥有 Agent Loop | `bridge.go` |
| `internal/policy` | 默认拒绝的规则匹配与授权决策 | `policy.go` |
| `internal/approval` | TUI 审批 Broker 与 evaluation mode 的精确工具名 allowlist | `broker.go`、`allowlist.go` |
| `internal/sandbox` | Shell 的 OS 沙箱（macOS `sandbox-exec` / Linux `bwrap`）、已有容器边界检测（`--shell-sandbox container`）与进程组生命周期 | `sandbox.go`、`invocation.go`、`process_unix.go` |
| `internal/artifact` | content-addressed Artifact Store（大工具结果、Diff、补丁） | `store.go` |
| `internal/modelcontext` | 合并 provider/目录/fallback 的上下文窗口、最大输出与输入预算 | `limits.go` |
| `internal/tokenizer` | 按 provider/model 选择本地 BPE tokenizer，标记 estimated/unavailable | `tokenizer.go` |
| `internal/tokenusage` | 模型请求的 durable 预留、派发、回执与核销账本 | `usage.go` |
| `internal/session` | append-only 事件协议和独立 checkpoint 接口 | `event.go`、`checkpoint.go` |
| `internal/sessionregistry` | `$GSEA_HOME/control.db` Session 目录（`local-agent session`、`chat --session`），独立于对话历史数据库 | `registry.go` |
| `internal/runtimeowner` | 数据库旁 `.owner.lock` 的 OS 文件锁所有权 | `lock.go`、`lock_unix.go` |
| `internal/retention` | 离线归档与插件回收的不可变计划/回执契约 | `retention.go`、`collection.go` |
| `internal/session/sqlitestore` | 主 SQLite Database owner（`sessionregistry` 与 `workspacedomain` 各有独立数据库）；按领域组织存储、查询和原子事务，离线维护查询与应用分离；保留全局 forward-only migration 账本（当前 v1–v41） | `store.go`、`migrations.go`、`schema.go`、`query.go`、`orchestration.go`、`agents_spawn.go`、`tasks.go`、`mailbox_ledger.go`、`workspacecoord.go`、`merge_apply.go`、`retention_plan.go`、`plugin_collection_plan.go` |
| `internal/session/memorystore` | 测试和临时运行使用的内存实现 | `store.go` |
| `internal/plugin` | 插件 Stage/Activate/Retire/Rollback、精确测试报告与确认发布、持久目录边界 | `manager.go`、`publication.go`、`verification.go`、`catalog.go`、`persistence.go` |
| `internal/pluginbuild` | 候选请求校验、隔离编译、先预留后构建/测试、耐久失败证据；没有发布权限 | `service.go`、`types.go`、`compiler.go` |
| `internal/strategy` | 有界 LoopSpec、运行时 Store 与宿主管理 Catalog 契约、串行影子比较及证据保存；比较器由 Eino Factory 实现 | `types.go`、`catalog.go`、`shadow.go` |
| `internal/plugin/wasm` | wazero WASI reactor、Blob Handle 和资源限制 | `runtime.go`、`blobstore.go` |
| `sdk/pluginabi` | 插件侧使用的 WASI ABI 和 Go 辅助函数 | `abi.go` |
| `internal/architecture` | 仅含测试：解析生产源码并检查依赖方向与关键构造点白名单 | `architecture_test.go` |

## 自进化服务与装配边界（重构 F1）

`App` 仍是进程资源的唯一 owner，但不再直接实现编译/测试/比较流程。`open.go` 按依赖顺序装配所有服务，再返回可用实例；绑定工具闭包时使用同一个已分配的 App，不整体覆盖包含锁的结构体。初始化失败逆向释放已打开的资源。`close.go` 保留关闭语义：先拒绝新工作、取消并等待 evolution worker，再关闭 Supervisor、插件、诊断、审批、WASM、SQLite 与 owner；worker 未退出时不提前释放它可能仍使用的资源。

| 组件 | 拥有的职责 | 明确不拥有的职责 |
| --- | --- | --- |
| `pluginbuild.Service` | 单 worker 准入、请求预留/重放、编译、Stage、验证、失败回执 | 不能审批或激活工具，不关闭宿主资源 |
| `plugin.PublicationService` | 测试报告保存、显式确认、报告与 artifact/runtime 精确匹配 | 不代替 Policy 审批，不改变 Manager 的注册表发布与 CAS |
| `strategy.ShadowService` | 单 worker 比较、读取可信候选/测试集、保存比较报告 | 不自建 Agent Loop、不注册可信答案、不发布策略 |
| `App` | 装配、模型 Factory 接线、可信 root 调用者解析、自进化取消/join 与资源所有权 | 不重复实现业务流程或模型 JSON 适配 |

构建服务依赖窄的 `BuildStore`、`SourceCompiler`、`Stager`、`Verifier` 端口；影子服务依赖 `ShadowStore` 和 `Comparator`。SQLite 实现持久端口，现有 Eino `Factory.CompareStrategy` 实现比较器。运行时继续只依赖 `strategy.Store`，宿主维护入口使用 `strategy.Catalog`，不把具体 SQLite adapter 暴露给业务流程。

取消后的构建回执/完整影子报告仍使用保留 context values、脱离取消且最多 5 秒的保存上下文；保存失败保持 fatal 分类，不能被当成普通可修复错误再次自动消费资源。纯单元测试注入各阶段失败并验证调用顺序；原有真实 Go→WASM、Eino 提案修复发布、下一 Turn 可见、关闭重开测试验证装配后的行为。分批交付记录见 [REFACTORING.md](./REFACTORING.md)。

## 输入适配与可信调用者（重构 F2）

七个自进化工具的描述/schema/risk/revision 集中在 `evolutiontools/*_bindings.go`，执行函数和参数/归属检查分开。工具参数是模型数据，不是授权：插件归属仍由 Pipeline 的 `Invocation.SessionID` 覆盖；策略调用由 `app/evolution_scope.go` 比对 durable root 的完整 Actor Scope、排除 child 并选择已配置 Factory，再交给适配层一个绑定好的 `StrategyCaller`。该对象不能从 JSON 构造；影子比较闭包始终经过 `App.ShadowStrategy` 的取消/join 边界，不直接调用 Factory 绕过资源所有者。

适配层只持有 `PluginHost`、窄的 `StrategyStore` 和可信调用者解析器。`StrategyStore` 不提供 `SaveSuite` 或 `Fallback`；模型不能通过适配层注册期望答案或主动绕开发布检查。报告仍先关联到属于当前 session 的候选，发布再复核 baseline，业务服务继续负责持久证据与 CAS。工具必须通过 Pipeline 执行，拆分没有创建新的审批入口。

维护 CLI 分为 `*_options.go` 的配置/flags 解析与同名命令文件的执行入口。解析只读取配置、不读取 stdin、不打开 SQLite/WASM、不创建模型；确认与冲突选项在这一阶段拒绝。插件文件读取集中在 `plugins_input.go` / `plugin_json.go`，保持原来的大小限制、严格解码及先校验文件后打开 App 的顺序。`strategy shadow` 仍独占模型创建路径，先确认、验证持久 candidate/suite 身份，再创建模型。其余命令不请求模型。

`evolutiontools/testdata/bindings.json` 固化拆分前的七个描述契约；测试在真实 Pipeline 上验证审批拒绝、可信归属覆盖、跨 session 隔离、非法选择与 fatal/可修复错误分类。CLI 测试覆盖每个操作、错误文本/退出码、help、严格文件输入和解析无运行时副作用。契约 fixture 不应在普通结构重构中随代码自动更新。

## 回合、验收与上下文职责（重构 F3）

`Factory` 保留稳定的公开入口和共享依赖，不增加第二套 Agent Loop 或通用运行时容器。`config.go` 定义配置契约，`factory.go` 负责构造与默认值，`factory_views.go` 负责不可变的 actor/Goal 视图；`factory_turn.go` 负责新 Turn 的策略/快照准入及恢复身份校验，`eino_turn.go` 是新建和恢复共同使用的 Eino 装配点。原有 Turn 单次执行、冻结工具快照、旧策略恢复、模型请求序号和 checkpoint 协议不变。

上下文处理分为策略、效果与数据变换：`context_policy.go` 用纯函数 `planContext(window, trigger, tokens, force)` 决定上下文压力级别、是否压缩和保留 token 数，不读取 Team 累计 token/费用；手动压缩也不因 Team 余额改变保留量。`context_artifacts.go` 外置大工具结果；`context_summary.go` 调用原有记账模型或生成临界上下文压力 fallback；`compaction_transcript.go` 处理去重、UTF-8 边界和工具调用/结果成组保留；`compaction.go` 提交摘要与 retained messages；`context_events.go` 写入审计事件。默认工作阈值仍取窗口的 80%、200000 tokens 和输入预算的最小值；摘要最多 2048 tokens、工具结果超过 24 KiB 外置的策略也保持不变。移除的是 Team 消费达到 80% 后在半窗口提前压缩的路径。旧事件和 checkpoint 继续按原语义回放，无需改写历史 schema；尚未持久化的新输入仍不能进入历史 checkpoint。

2026-10-16 的上下文修复将部署容量、工作窗口、输入预算与触发线分开：`internal/modelcontext.Resolve` 集中合并容量目录、每字段来源与用户预算；Gateway 读取有界元数据，Runtime 通过可选 `Informer` 消费，不依赖 HTTP/provider 实现。自动使用 provider → catalog → fallback；显式工作预算可覆盖目录/fallback，但不能突破 provider 声明硬窗口。窗口与最大输出独立发现，Ollama 只读已加载模型的 `/api/ps` 实际 context_length。输出预留可自动选择，并受已知最大输出约束；输入预算只扣一次输出和安全余量。`context_count.go` 统一计入系统消息和工具 Schema；provider usage 只校准输入估算，缓存仍占窗口，不推断容量。

`request_context.go` 在每次模型派发前执行检查，覆盖 Eino 同一轮内的工具循环：完整输入加输出预留和安全余量不能超过工作窗口。输入预算（input budget）是单请求容量，Team 预算（team budget）是累计消费额度，二者分别执行约束。Team/Agent 硬配额、Goal guard 和模型请求记账继续生效；摘要和格式修复也必须预留并核销用量，预算不足时明确停止，不能由抽取式 fallback 绕过。`profileOptions` 对 `purpose=compaction`（含修复）保留计划输出额度：Profile 累计输出余额不足时报 `ProfileConstraintError`，恰好足额则允许，不通过缩短摘要省配额；普通主模型请求及 Goal assessment 的配额行为保持不变。Eino 继续拥有执行循环和恢复 checkpoint；适配层以原始前缀摘要验证并复用压缩视图，后续请求只追加新消息，内容变化时重建。上下文压缩保留当前用户请求、宿主指令、Goal 合同以及完整工具消息组；辅助摘要请求也受相同的容量检查。v2 `context.compacted` 关联当前 turn，仅在该 turn 完成时发布为后续历史；中断/失败不能把未完成工具结果暴露给新 turn。v0/v1 的回合间 checkpoint 保持原语义，原始审计日志不修改。

`context_assessment.go` 保留最后实际准备的请求私有副本及工具 Schema；turn 回复完成后本地重计数，将状态与 `turn.completed` 一次提交，并通过 `Result.ContextUsage` 反馈 UI。结束后的超线只标记待处理，不把成功回复改判失败、不自动发空闲模型请求；下一次模型派发前重新检查，恢复会话也按当前预算重新评估。`context_summary_format.go` 定义版本化事实摘要并严格校验；格式无效最多一次有界修复，再失败使用明确标记的抽取式摘要；旧摘要加载不依赖新格式解析。宿主 Goal/合同锚点在模型输出之外添加，不给摘要授予权限。

`Conversation` 始终拥有 session mutex、历史投影和完成数。`run.go` 选择 direct/auto/always，`execution.go` 串接上下文准备、历史刷新、Turn 创建及执行，`context.go` 提供上下文查询/手动压缩。`history.go` 的统一刷新仅在完整读取成功后替换视图：准备后的刷新失败不能继续创建 Turn；执行已完成后的刷新失败不会抹掉 durable outcome，重开会话仍能恢复结果。锁的范围和原有错误信息保持不变。

Goal 的实时验收与已提交 verdict 恢复共用 `goal_transition.go`：它只处理 complete/blocked/continue 及继续前的资源上限，不执行 worker 或 evaluator。`goal_loop.go` 管理实时 round 和评估，`goal_recovery.go` 选择恢复阶段和 deadline，`goal_outcome.go` 负责终态持久化、候选释放、阶段事件及失败/取消收尾；`goal_protocol.go` 保留原有 Goal 锚点和继续提示。

| 恢复时的持久状态 | 允许的工作 | 禁止重复的工作 |
| --- | --- | --- |
| 执行未完成 | 恢复原 Eino checkpoint | 不替换为新 Turn 或新用户输入 |
| 执行完成、验收未提交 | 读取原候选，只做验收 | 不重跑 worker/工具 |
| verdict 已提交 | 复用结论；complete/blocked 仅收尾，continue 经原 deadline/资源检查后进入下一轮 | 不再次调用 evaluator，不重放旧副作用 |

已提交终态结论的收尾沿用脱离取消、最多 5 秒的持久上下文；继续执行仍受原 Goal 起始时间约束。候选答案只能在 Goal 终态提交成功后释放。新增测试覆盖上下文精确阈值、历史刷新故障、round 登记先于执行、实时/恢复结论的一致性、过期终态收尾及继续前的预算失败；原有恢复、准入、策略快照和压缩 checkpoint 集成测试继续负责跨层回归。

## 持久层与事务所有权（重构 F4）

`Database` 仍是 SQLite 连接池的唯一 owner，保留现有 Store 入口和兼容方法，不另建仓储容器或通用事务框架。连接池仍只有一个连接；业务操作一旦开始事务，内部查询与写入必须使用同一个 `tx`，不能再回到连接池，否则既可能看不到未提交状态，也可能等待自己占有的连接。

| 文件边界 | 职责与约束 |
| --- | --- |
| `store.go` / `migrations.go` / `schema.go` | 分别负责连接生命周期、迁移执行、已发布 DDL 账本；每个版本的 DDL 与校验和登记同事务提交 |
| `orchestration.go` / `agents_spawn.go` / `agents_state.go` / `orchestration_events.go` | root Team/Thread、子 Agent 创建、状态转换与事件持久化；事件辅助函数使用调用方事务，不独立提交 |
| `agents_query.go` / `tasks_query.go` / `query.go` | 列表与点查询共用列投影及 scanner；包内窄查询接口省略 Exec 和事务生命周期方法，表达依赖边界，不替代 SQL 权限控制 |
| `tasks.go` / `task_claims.go` / `task_dependencies.go` | Task 创建/转换、领取/过期处理、依赖传播；子 Agent 与任务领取、任务完成与依赖解锁仍分别原子提交 |
| `retention_plan.go` / `plugin_collection_plan.go` / `maintenance_*.go` | 只读计划、生命周期证据、引用、依赖检查与回执；预览不修复身份，应用在原事务内重新计算计划并保存效果及回执 |

迁移不能随领域文件重新分组或重新编号：历史版本 6、26、27 等包含跨领域变更，它们的原子性和全局顺序已经是持久协议。`schema.go` 有意保留一个按时间顺序追加的账本，已发布 SQL 的空白也属于校验和。`testdata/migrations.sha256` 固定 v1–v41 全部已发布校验和并校验版本数量；v37/v38 追加合并验证的持久回执及检查身份，v39 追加 Agent completion receipt，v40 追加细化成员关系，v41 追加 Goal durable admission token。普通重构不应重新生成历史 fixture，也不应为了适配旧数据库而改写已有 migration 记录。

离线维护只共享事实查询和事件解码，不合并不同的安全判断。归档检查完整生命周期和依赖，放弃恢复检查指定 root/Goal/Turn 并保留候选证据，插件清理检查全部冻结工具集与未知旧消费者。三者保留原有阻塞条件、错误和确认要求；`maintenance_receipts.go` 的回执写入参加调用方事务，不能先于实际变更单独提交。

新增测试从空库及每个已发布版本升级，检查最终表/索引/约束、历史事件、checkpoint 和已应用记录；注入 DDL 中段失败及迁移登记失败，验证整版回滚与重试。业务故障测试比较数据库全部持久行，覆盖 root 创建、带任务领取的 spawn、Agent 转换、依赖变更、任务完成解锁及 claim 回收，避免只检查主表而漏掉事件或幂等记录。维护预览测试验证查询可见调用方未提交状态、不产生写入，事务回滚后原计划保持一致。

## 架构约束与关闭契约（重构 F5）

`internal/architecture` 的测试解析 `cmd`、`internal` 和 `sdk` 下的生产 Go 文件（包含其他平台的源码，不包含测试 fixture），在普通 `go test ./...` 中检查依赖方向。它约束具体 SQLite adapter 只能由 App 引入、SQL 驱动只能由持久层引入、Eino ADK/compose 的装配层、Provider adapter 和 wazero 的归属，以及领域服务的内部依赖。它是结构回归检查，不是安全沙箱，也不替代运行时测试。

关键构造点有明确白名单：会话 SQLite（`sqlitestore.Open`）只在 `app/open.go` 与离线 `app/maintenance.go` 打开，`workspacedomain.Open` 只在 `app/open.go` 打开，SQL 驱动只允许 `sqlitestore`、`sessionregistry` 和 `workspacedomain` 引入；WASM 只由 App 创建，Eino Runner/ChatModelAgent 只在 `agentruntime/eino_turn.go` 装配。正常 Tool Pipeline 由 App 创建；唯一额外入口是策略影子测试的空工具、内存会话 Pipeline，不具有业务执行/发布能力。检查识别导入别名和构造器函数值，拒绝模糊归属的 dot import；新增依赖必须复核角色，不能仅为通过测试扩大白名单。

放弃恢复、维护和评测后用量读取共用 `withOfflineDatabase` 的 owner → SQLite → 查询/操作 → 关闭 SQLite → 释放 owner 顺序，不启动模型、插件注册表或恢复中的 Supervisor。`ReadSessionUsage` 从 session 找到 durable Team，汇总 root 与 child 的用量且排除其他 Team。正在运行或关闭超时后仍持锁的实例会拒绝离线打开；CLI 保留原有 transcript 用量回退与错误报告。

正常 `App.Close` 先拒绝新工作，取消并等待自进化 worker，然后关闭 Supervisor、插件、诊断、审批、WASM、SQLite，最后释放 owner。自进化等待超过 10 秒，或 Supervisor 的 worker join 超过 5 秒时，不再关闭后续资源，所有权锁保留到进程退出；这两个等待上限不是整个关闭过程的总 deadline。App 与 Supervisor 的并发/重复 Close 共享同一结果，不能把第二次调用当成 worker 已退出的证明。Supervisor 初始化失败也必须检查关闭结果：正常失败可以重试；有未退出 worker 时 App 拒绝新工作、移除失败装配的控制绑定并保留资源。

取消后的结果落库遵守已有 `running → canceling → canceled` 状态机。runner 可能在关闭流程写入 `canceling` 前返回，或因自身 deadline 取消，因此收尾负责补齐中间状态；若 Close/Interrupt 抢先提交，只接受同一 Run 的 `canceling` 版本继续提交结果，不重试 Task 效果，不覆盖新 Run 或已提交终态。测试覆盖并发关闭、超时持锁和资源保留、初始化失败清理、取消后证据保存，以及精确 CAS 冲突。

## 一次对话的生命周期

1. `config.Load` 根据 `GSEA_HOME`（未设置时为 `~/.gsea`）读取用户配置、profile，再发现项目 `.gsea/config.toml`；项目配置必须经宿主 `TrustProjectConfig` 或本次环境变量 `GSEA_TRUST_PROJECT_CONFIG=true` 显式信任，默认拒绝。之后应用环境变量；CLI flags 在 `internal/cli/chat_options.go` 中作为最高优先级覆盖。
2. `internal/cli/chat_options.go` 把 flags 解析为不可变的 `chatOptions`，校验 session ID，并生成 provider-neutral 的 `modelgateway.Config`。
3. `app.Open` 按顺序创建 SQLite Database、Session/Checkpoint/Orchestration stores、WASM Runtime、可选 LSP Manager、Policy、Registry、Tool Pipeline 和 Plugin Manager；内置规则与工具目录由 `app/builtins.go` 原子装配。LSP 进程在首次诊断时按 workspace/worktree 懒启动，不阻塞无代码修改的启动路径。
4. `internal/cli` 在 `conversation.Open` 前通过 Orchestration store 幂等确保 root Team/Agent/Thread projection，创建 Supervisor 并恢复 durable child；root Conversation 在每轮开始接收 child mailbox，在 turn 成功后 ack，然后加载已完成历史并根据 TTY 状态进入 `tui.Run` 或独立的 headless runner。普通 headless 使用 `DenyApprover`；显式、非 TTY 且带明确 workspace 的 evaluation mode 使用精确工具 allowlist。无 bridge 时只批准 workspace write/edit/shell 和 snapshot 合并的 `merge_apply`；有 bridge 时只批准本次调用通过成对 manifest/socket 参数声明的 harness 工具，禁止 workspace mutation 和 shell，避免外部内容读取模型进程环境。外部工具作为 execute-risk `tool.Binding` 与 built-ins 同批原子装配，名称冲突直接使启动失败。
5. TUI 将按键、窗口、鼠标和 runtime delta 统一为 Bubble Tea message；提交输入后只调用 `Conversation.RunTurn`。
6. `Conversation` 区分任务准入与继续执行。默认 `auto` 在普通 Eino turn 暴露 `goal_start`，不使用关键词路由或额外分类请求。执行模型只为用户授权且有验收责任的任务登记不可变 Contract（目标、范围、验收条件）；宿主从可信 invocation 绑定 turn/session/actor 和原用户输入，`AdmitGoal` 在单个 SQLite 事务中创建 Goal 并将同一个 turn 登记为第一轮。`recordingModel` 按持久化准入状态切换模型可见的 start/report 工具，不改变冻结的 Eino handlers。`agentgoal.AdmissionGuard` 通过通用 `tool.TurnGuard` 端口检查调用与 turn 完成；Pipeline 在重放之后、策略与审批之前记录可恢复的 `PreconditionError`，Eino 继续负责修正参数和工具循环。登记失败的任务保留只读检查，但不能继续显式写入、execute/secret 调用或以普通聊天完成；结构化事件恢复待登记状态，父子 turn 的检查隔离。普通读取与未登记前的 shell 查询不自动创建 Goal，风险元数据不替代副作用沙箱。工具写权限仍由原策略管线负责，Contract 不构成新增授权。`always` 先创建 durable admission token 再创建 Goal；`enabled=false` 不暴露登记与报告工具。工厂使用不可变视图配置协议，每个 turn 捕获 Registry generation 并绑定 checkpoint ID。
7. `Turn.Run` 先记录 `turn.started` 和 `user.message`，然后消费 Eino iterator。
8. `turn_stream.go` 将 Eino 的完整消息或流式消息转换为 provider-neutral `EventSink` 事件，TUI 不解析 provider wire format。
9. 工具调用经 `einoadapter` 进入 `tool.Pipeline`，携带 Team/Agent/Thread/Task/Run Actor Scope，依次记录 proposed、policy、approval、started 和结果事件。
   `workspace_write`/`workspace_edit` 直接产生单文件 ChangeSet 和 bounded unified diff；`shell_exec` 在 workspace lease 内比较命令前后快照，产生有界的 created/modified/deleted ChangeSet 与多文件 Diff。Diff 输入受单文件 4 MiB、快照 8 MiB 限制，内联结果受 96 KiB 限制，超出后写入 content-addressed Artifact Store 并返回引用；binary、unavailable、partial 状态显式保留。受支持文件批量同步给 LSP，diagnostics 经 changed-file 优先、severity 排序、去重和双层限量后合并进同一 durable tool result，并按 workspace/path/content revision 保存为 SQLite projection。诊断或 Diff artifact 基础设施失败只改变反馈状态，不改变写入成功语义。
10. 完成时记录 `turn.completed` 并删除 checkpoint；失败时记录 `turn.failed`，如果 checkpoint 存在则标记为可恢复。
11. 直接对话即时流式输出；登记后由 `candidate_events.go` 缓冲候选最终正文，`goal_loop.go` 管理 round、验收、继续和终态。`agentgoal.Verifier` 与 `Evaluator` 分离：默认 adaptive 对支持的文件内容条件通过原 Tool Pipeline 进行新鲜读取；只验证了部分条件不能认定整体完成，真实检查失败不能被语义评估覆盖。其他条件使用独立评估器及有界真实工具记录；显式 self/independent/hybrid 继续兼容。评估器超时或格式重试失败属于基础设施失败，不是额外执行轮次。独立 `goal.phase` 事件让界面显示执行、验收、继续及停止，正文尚未接受不等于界面静默。总时限使用从持久化起点计算的 deadline，验收阶段另有默认 30s deadline；完成、阻塞、资源耗尽、失败和取消分开记录。
   `turn.started.goal_mode` 记录协议能力是否启用，新增 `goal_activation` 记录 auto/always 快照，并保存 v41 admission token；是否真正承担 Goal 由 durable goal/round 映射确定，不能用“暴露了工具”代替任务准入。checkpoint 按这两个字段恢复，旧 goal-enabled checkpoint 兼容 always 协议，已登记任务沿用保存的 assessment、契约及起始时间。会话完成数合并无 Goal 的已完成 turn 与终态 Goal，失败任务不算成功，拒绝的 round 不重复计数。reasoning summary 仍只是瞬时展示信息。
12. `internal/tokenizer` 按 provider/model 选择本地 BPE tokenizer；未知或未缓存编码明确标记为 `estimated`/`unavailable`，收到 provider 的真实 `input_tokens` 后以偏移校准，不把近似值伪装成精确值。
13. `Conversation.ContextUsage` 提供当前上下文窗口占用；`Factory.PrepareMessagesWithHints` 按窗口、阈值和当前 token 占用计算 Context Policy，active Goal/round 作为独立锚点保留，Team 累计消费不参与压缩决策。超过 24 KiB 的工具结果先写入 content-addressed Artifact Store，再以带首尾预览的引用进入模型上下文；`artifact_read` 提供最多 1 MiB 的分段召回。达到压缩级别后保留 Goal 锚点、近期消息并折叠重复循环；临界上下文压力下允许的摘要模型失败会使用明确标记的抽取式 fallback，预算耗尽及记账/存储失败仍直接返回错误。所有策略决策写入 `context.managed`，旧事件继续可读，原始 append-only 历史不被改写。
14. `agentmanager.Supervisor` 负责 child 的 spawn/list/wait/interrupt、并行运行槽位、容量和重启恢复；子 Agent 通过 `Factory.WithActorScope` 与独立 `conversation.Conversation` 执行，不绕过既有工具管线。
15. `agentmanager` 与 SQLite mailbox 共同提供 parent-child durable 消息：消息按 recipient sequence 有序，Receive 将 enqueued 标记为 delivered，消费 turn 成功后才 acknowledged；失败会保留 delivered 状态以便下一轮重试。
16. 带 idempotency key 的 spawn、task 和 mailbox 请求在 SQLite 事务中校验 request hash；相同输入安全重试，不同输入返回 `ErrIdempotencyConflict`。Mailbox recipient sequence 由 durable counter 原子预留，支持并发 enqueue。
17. Task repository 在 SQLite 事务内执行状态版本校验、同 Team 约束、依赖环检测和 dependency completion gate；未完成依赖的任务自动进入 `blocked`，依赖完成后自动解锁为 `ready`；`task_claim` 只允许认领依赖全部完成的 ready task，默认 30 分钟 TTL 的 claim 在无 live Agent 时由 `ReapExpiredTaskClaims` 原子回收到 `ready`。带 `verification_profile` 的 Task 在 worker 成功后进入 `verification`，必须由不同且匹配 reviewer Profile 的 Agent 完成 gate。Supervisor 将 child Agent 的启动、成功、失败和取消同步到其 assigned Task，并在有 Runner 时自动派发 ready task/reviewer。
18. `agentflow.Runtime` 将 `parallel` 的完成项立即推入下一 stage，`compact` 过滤失败/空分支，`agent_session` 用 durable child thread 跨 stage 继续。`parallel` 对同时执行的分支数设置有界预算，避免大输入流创建无界 goroutine；`ParallelWithConcurrency` 可由宿主显式调整。每个 stream item 还携带不进入模型或持久化的 lineage identity，durable step ID 和 `agent_session` key 都基于 lineage，不再把重复 payload 错误折叠成一次执行或共享错误的 child Agent。Runtime 在 terminal output 出口再次校验自定义 stage 产出的 Value，拒绝无效 durable 结果；同时按 Team/session/idempotency key 串行化同进程重复 `flow_run`，避免两个 caller 同时执行同一 running run；durable checkpoint 仍负责崩溃后的恢复。spawn/continue/wait、budget、human 和通用 tool operator 都调用 Tool Pipeline；Flow 恢复校验原 registry 内容身份，并按 session-stage checkpoint 区分同一 stage 重放与下一 stage continuation。
19. 每个 Flow 必须声明 revision，Runtime 还会对 operator topology 计算 SHA-256 fingerprint；两者进入 run request hash、SQLite 和启动事件。重启恢复时任一身份漂移都会拒绝旧 run，Catalog 也会拒绝注册后被修改的自定义 stage。
20. Agent 不能提交任意 Flow 代码，只能用 `flow_list` 查看并用 `flow_run` 调用 `App.FlowCatalog()` 中由宿主注册的命名定义。嵌套执行继承当前调用者的 Pipeline view，因此 TUI 审批、普通 headless fail-closed 和 evaluation tool allowlist 行为保持一致。
21. `local-agent eval` 从严格 JSONL manifest 创建 detached Git worktree，或把非 Git fixture 复制到一次性目录并建立私有 Git 基线；传给 Agent 的 `evaluation.AgentRequest` 不包含 checks、base commit 或 evaluator metadata。Agent 完成后先捕获 binary-safe patch，再由 evaluator 在 OS sandbox 或无网络 Docker 容器中运行 checks；runtime-owned `.gsea/artifacts` 不进入 model patch。只有 Agent 正常结束且至少一个 required check 全部通过才产生 resolved attempt；失败或超时后的 patch/check evidence 仍持久化。每个 task 的 transcript、stderr、check log、SQLite 数据和 result 都位于独立 run directory。
22. 对 AgentDojo 这类由 Python harness 持有环境的评测，`internal/toolbridge` 从严格 manifest 建立临时 tool bindings。Eino 发出的 tool call 仍经 Registry Snapshot、Policy、approval、事件持久化后，才通过私有 Unix socket 执行 harness 官方函数；返回值沿原路径回到 Eino。bridge 不发送模型请求、不解释任务，也不实现 ReAct 循环。manifest/socket 只能在 evaluation mode 中使用，socket 必须是绝对路径，协议和消息大小都有版本与上限。

## 只读查询与装配边界

- `agentmanager.TeamSnapshot` 是控制面唯一的组合查询读模型，集中读取 Agent、Task、Workspace View、Lease 和 Merge 投影。TUI 与 headless control 的 snapshot 路径只依赖 `TeamSnapshotReader`，不再各自拼装多组 repository 查询或复制响应字段。
- `internal/tui` 只依赖 `TeamSnapshotReader`；生产 Supervisor 实现 `LoadTeamSnapshot`，读取失败时返回部分快照和聚合错误，界面可以继续显示可用数据。
- `internal/cli/conversation_builder.go` 是 provider-neutral Conversation 的组合根适配器，唯一负责把 Factory 变成 child Runner、注入 mailbox 上下文和转换 durable wait。Supervisor 只依赖 `Runner`/`ContextSeeder` 接口，不知道 Eino 或 SQLite。
- 新的只读读模型不持有 Supervisor 的可变锁或进程内 worker map；所有字段来自持久化 projection，适合 TUI 刷新、JSONL 控制和崩溃后恢复。

Supervisor 内部按变化原因拆分文件：核心构造与关闭、运行槽调度、单 Agent 执行、Task 调度、Task-Agent 联动、wait set、mailbox、workspace、恢复、merge 和只读查询彼此独立。拆分保持同一 package，避免引入无收益的跨 package 抽象，同时使每个文件只处理一个生命周期维度。

Supervisor 的终态收敛遵守同一组不变量：`interrupt`、进程关闭和崩溃恢复都会取消 pending wait set、释放 assigned Task、workspace lease 与共享 view；Git worktree 与有改动的 snapshot view 则进入产物保留/合并流程，不能把任务终态解释为删除或合并权限。wait 监控和调度扫描都绑定 Supervisor context。正常关闭会等待 worker 退出后再释放 lease；若 runner 忽略取消并触发关闭超时，则保留 durable lease 及运行时所有权锁，直到进程退出后才允许另一个实例恢复，避免后台写入失去互斥保护。

## 第一阶段：恢复与隔离不变量

1. `app.OpenWithOptions` 先通过 `runtimeowner.Acquire` 取得数据库旁的 OS 文件锁，再打开 SQLite、装配服务和恢复任务。macOS/Linux 使用非阻塞 `flock`；初始化失败或正常关闭释放锁，进程死亡由内核释放。锁文件不能 unlink，也不能作为 PID 文件人工判定“过期”。这是每个数据库的所有权，不是跨主机协调；同一工作区跨数据目录/`GSEA_HOME` 的写互斥由 `workspacedomain` 的实例登记、flock 与写 lease 负责。
2. Tool Pipeline 对 `tool.started` 后没有 durable success 的副作用调用返回 `OutcomeUnknownError`，不会根据缺少成功记录推断“尚未执行”。成功记录使用脱离用户取消、但有五秒上限的 context；执行失败且失败日志也无法写入时，错误不能包装成 Eino 可继续处理的普通 `ExecutionError`。幂等键仍限定 session/turn/call，新的调用需要独立授权与判断。
3. `context_checkpoint_version=1` 将摘要和 retained messages 放进同一个 `context.compacted` 事件，`LoadHistory` 先恢复两者，再追加后续完整 turn。自动压缩排除未提交的新 user input；保留边界向前扩展至完整的 assistant/tool 消息组。原始事件仍保持 append-only，旧 version 0 按旧语义读取，不会凭空恢复以前遗漏的后缀。
4. `workspacecoord.FinalizeView` 与 `ReleaseView` 分开：前者保存补丁和 durable 引用并保留 Git view；后者只允许移除 pristine worktree，不使用 `--force`。Supervisor 和 MergeExecutor 的终态都调用前者；snapshot view 由 `finalizeSnapshotView` 处理：未改动或合并已应用则丢弃，有 pending/running/conflicted merge 时保留，其余有改动的情况保留目录并记录 `workspace.view_preserved`。补丁通过隔离的 `GIT_INDEX_FILE` 捕获，输出有界，包含未跟踪文件且不污染原 index。持久化引用用 workspace stream CAS 去重；存储失败、创建碰撞、忽略文件及已提交的偏离基线内容均不能触发自动清理。保留状态目前体现为 active view + `workspace.view_preserved` 事件，后续 GC/展示可再增加专门投影。
5. `codingtools` 将可信 workspace view 传入 `sandbox.Request.WorkspaceRoot`，沙箱只允许缩小配置根。Shell 的 cwd、符号链接、读白名单、写范围与私有 HOME/temp/cache 在每次调用时解析；父工作区和其他 view 不能通过 PATH/GOROOT 白名单重新开放。Linux 不再只读挂载整个宿主 `/`，并隔离 PID/IPC/network namespace；macOS 保留动态链接器所需的根目录 literal read 和全局 metadata 查询，但不因此授权子文件内容读取。容器模式无法实现更窄的 view 时明确拒绝。

上述改动不改变 Eino Inner Loop、Goal Outer Loop 与 Supervisor 的职责划分。Registry/Profile、请求恢复、插件持久发布与精确 revision GC 的后续实现见下文。

## 第二阶段 A：可验证快照与 Profile 执行边界

1. `tool.Snapshot.ID` 对排序后的工具描述、规范化 JSON schema、`Binding.ImplementationID` 和宿主可执行文件 SHA-256 计算内容地址。generation 只用于进程内诊断，`SnapshotByID` 查询已接纳的视图，跨重启恢复使用第三阶段 B 的 `ResolveSnapshot`。WASM binding 额外包含产物 SHA、显式 timeout 和规范化沙箱资源身份（ABI、内存、Blob/调用字节限制及默认 timeout）；原生/动态 executor 的其他配置发生改变时，宿主仍必须更新 revision 或 ImplementationID，不能把闭包配置当作自动被哈希覆盖。重编译宿主会保守地使旧中断 checkpoint 不兼容。
2. SQLite migration 26 保存 `flow_runs.runtime_snapshot_id` 和 `agents.profile_snapshot`。Flow 请求哈希在有内容 ID 时排除 generation；不同注册顺序不改变幂等请求。同一内容 ID 不代表 Flow 回调可任意修改，既有 definition revision/fingerprint 校验仍保留。旧 generation-only checkpoint 不能证明跨进程身份，明确拒绝恢复；不尝试恢复不存在的旧 WASM 产物。
3. `agentprofile.Encode/Decode` 定义版本化 Profile 快照并校验指纹和 revision。注册表拒绝 revision 重绑定，保留旧 revision。Supervisor 只从宿主注册表建立子 Profile，不接受模型参数伪造快照；实际授权快照同时写入 Agent projection 和 `agent.created` 事件。Actor 授权、调度模型桶和 workspace 模式均读取 durable Profile。旧 metadata 可用于查询，但生产 child Runner 拒绝执行缺少快照的旧 Agent。
4. 委派授权是父实际工具集合、父可委派集合与子工具集合的交集；子可委派集合再与其实际授权取交集，控制面能力不能逐代扩大。只读父 Agent 不能委派可写 workspace。`nil` allowlist 与显式空 allowlist 保持不同语义。`MaxChildren=0` 不是无限；context inheritance 不允许超过子 Profile 的级别，有 Token 上限时 seeder 必须实现带完整 Profile 的有界接口。
5. `Factory.WithProfile` 是子执行装配入口：应用指令、模型、迭代上限和 Profile；模型别名不匹配时通过显式 `ResolveModel` 得到模型/tokenizer/provider/context window，否则报错。CLI 内置 Profile 仍只配置当前 chat 模型，并未增加用户侧多模型目录。继承上下文按接收方 tokenizer 截断。Supervisor/child Runner 用 `CreatedAt + MaxWallTime` 限制运行，等待、恢复或 continuation 不重置截止时间。
6. 模型可见工具过滤与真实执行授权分开。`Pipeline.WithExecutionGuard` 在 replay 前校验完整 ActorScope、session 和工具授权，在 `tool.started` 持久化前串行准入，并在执行前释放准入锁，避免嵌套 Flow 死锁。Flow 使用调用方管线，不能退回无 Profile 的全局管线；Profile 失败沿嵌套调用保持 fatal，不能被 Eino 当作普通工具错误后继续。工具次数从 durable events 重建，按子 session 累计，成功 replay 不重复扣次数。
7. `execution_config_id` 绑定 turn 的 Profile 指纹、指令、provider/model、输出/上下文/迭代设置，配置不一致不能继续旧 turn。`recordingModel` 在模型调用前根据 durable requests/assistant usage 核对累计 Profile Token 预算，并将剩余输出额度落实到模型选项；无 usage 或未完成请求保留估算额度。当前按页扫描事件以优先保证正确性；若长 session 成为瓶颈，可新增可重建的增量投影，不应把进程内计数器当作权威。
8. 语义压缩与独立 Goal 评估通过相同的 `recordingModel` 入口，带 `purpose` 和唯一 request ID。重复评估从 durable event 恢复请求序号；helper 消息不进入已完成对话历史。正常响应按 provider usage 核销，无 usage/已发出的失败请求按 `estimated_reservation` 核销；仅未派发请求退还预留。估算输入不保证精确，输出额度也不等于实际费用；记录缺少价格时不能推算账单。已知 provider usage 的结算失败会留下 pending reservation，不降级为估算或退还。

上述 Token 准入不是精确计费证明。Profile 快照可跨进程读取不等于运行时自动持久发布或重新装载旧插件。回归测试覆盖跨进程身份、SQLite 重开、快照篡改、权限逐级收窄、嵌套 Flow、并发工具预算、配置漂移、模型选择及辅助请求记账。

## 第二阶段 B：流式终态、验收恢复与用量对账

1. Responses SSE 解析器只在有效 `response.completed` 且 `response.status=completed` 时正常结束，不等待 HTTP EOF；提前 EOF、孤立 `[DONE]`、`error`、failed/incomplete/cancelled、非法 JSON 或事件名冲突都返回错误。模型参数中的输出上限映射到请求的 `max_output_tokens`。完成事件的含义按 [OpenAI 流式 Responses 文档](https://developers.openai.com/api/docs/guides/streaming-responses) 实现；解析器负责协议边界，Eino 继续负责工具循环。集成测试用本地截断流验证：即使已经收到完整工具参数，也不会因此执行写操作或提交 `turn.completed`。
2. `Factory.CompletedTurn` 从 `turn.completed` 读取候选结果，验证原模型、执行配置和 Registry 内容身份，不靠已删除的 Eino checkpoint 重新执行。`Conversation.ResumeTurn` 对 Goal 区分三种阶段：执行未完成按 checkpoint 恢复；执行已完成只进行验收；验收已提交则复用结论。SQLite migration 27 将 `Verdict.Feedback` 与 candidate/decision/reason 一起写入 round；继续只进入下一轮，完成或阻塞只提交终态。过期 round 拒绝恢复，达到轮数/时间上限保留部分结果；不重复发布历史工具事件。已提交的终态判定可在原截止时间后完成持久化，但不授权新的执行。
3. `tokenusage.DurableReservationLedger` 以窄接口分离请求生命周期：`Reserve` 保存额度及模型身份，`MarkDispatched` 在 provider 调用前一次性提交派发边界，`StageSettlement` 持久保存回执，`SettleReservation` 核销到用量账本。发送边界不确定时可能保守多计，但不能退款或复用同一请求 ID。回执身份和内容不能被后续估算覆盖；账本暂时不可写时，回执仍可在重启后核销。
4. `Supervisor.Recover` 在独占启动阶段调用 `RecoverReservations`，替代按年龄退款的旧 reaper。新记录确认未派发时释放；已有回执或相同请求账本记录时核销而不重复计入；派发后未知结果及 migration 27 以前的遗留 pending 记录按预留额度记为 `estimated_recovery`。整个恢复事务遇到损坏回执、身份冲突或未知版本会回滚，不静默退款。调用方必须保证此时没有活跃模型请求，不能将它用作周期清理器。
5. 恢复模型请求序号取事件与所有预留记录的最大值，包括 released 记录，覆盖“Reserve 已提交但 model.requested 尚未追加”的崩溃窗口。已配置价格表时同时预留费用。`estimated_reservation`/`estimated_recovery` 是保守记账，不是供应商实际账单；输入 tokenizer 估算不保证精确上界，未知价格也不提供有效费用上限。收到响应但尚未来得及持久化回执即崩溃，仍只能按估算恢复；Profile 的事件预算在缺少 assistant usage 时也可能保守保留额度。

阶段 B 提供按 Turn ID 的恢复核心；CLI/TUI 入口和 Supervisor 调度接线见阶段 C。这仍不是跨进程 exactly-once 保证。插件持久发布、旧版本自动装载和产物 GC 未包含在这批修改中。

验证覆盖 Responses 错误/缺终止/连接不关闭、输出额度线格式、真实网关至 Eino 的截断流、Goal 在执行/验收两阶段重开、继续反馈及耗尽后的部分结果，以及模型派发和回执核销之间的故障注入。均使用本地 fake model、HTTP fixture 和临时 SQLite，不调用真实模型或下载评测数据。

## 第二阶段 C：恢复发现与调度接线

1. `agentruntime.LoadTurnRecords` 只投影事件和 checkpoint 是否存在；`conversation.DiscoverRecoveries` 结合 Goal 当前 round 给出 execution/assessment/verdict/unavailable 阶段。`PendingRoundReader` 补充尚无 turn.started 的 Goal，包含下一轮已登记但尚未启动的窗口。查询只依赖读接口，不创建 Runner 或调用模型；“存在 checkpoint”不等于已验证兼容，选择后仍由原 ResumeTurn 边界校验。
2. CLI/TUI 复用 Conversation 恢复服务：`/recoveries` 发现、`/resume <turn-id>` 显式执行；CLI 另支持 `--session … --list-recoveries` 和 `--resume-turn …`。列表模式从 App 直接查询，不要求模型/API Key、不创建 root thread、不启动 Supervisor；数据库打开仍使用所有权锁及迁移流程。TTY 恢复复用流式事件、取消、审批和完成渲染，headless 复用原 JSON/文本结果编码。恢复命令不是 user.message，不添加第二个用户请求。
3. 新 turn.started 保存 `run_id`，不改变 Eino checkpoint 格式，也不额外增加 SQLite schema。Supervisor 对重启恢复或 resolved wait 延续原 CurrentRunID；只有明确的新执行/continuation 才创建新 Run ID。RunRequest.Recover 由宿主状态产生；child adapter 调用 `Conversation.RecoverRun`，选取该 Run 的最后一个已绑定 Turn。执行已完成时复用 durable candidate，Goal 已终态时仅补 Supervisor 收尾；否则进入原 Turn/Goal 的恢复路径。缺少绑定、checkpoint、版本兼容或当前 Goal round 时拒绝，不退回 RunTurn。旧记录不猜测绑定。
4. 恢复沿用原输入，不读取或确认新 mailbox 消息；正常新回合也只在成功后确认消息。旧输入中已包含、但因中断未确认的消息可能在后续显式回合再次作为背景出现，当前尚无按输入快照精确核销消息的协议。工具副作用仍以原 session/turn/call 去重，未知结果仍停止；恢复不绕过 Profile 预算、期限、审批、workspace 隔离或宿主快照身份检查。
5. 回归测试包括：写操作完成但 Supervisor 尚未收尾时关闭/重开 SQLite；原 Run/Turn 身份与写次数保持不变；Goal 只验收及终态结果重放；缺失/旧绑定拒绝；根列表不派发子任务；CLI/TUI 分发不产生新提示；mailbox 不被恢复错误确认。全量测试、静态检查及相关包竞态检查作为交付门槛。

后续边界：缺失 checkpoint 的任意时点崩溃不会被本系统“补造”成可恢复执行；未显式放弃的任务仍保留 unavailable 项，根任务离线放弃见第三阶段 C。长会话当前按页扫描事件，必要时可以增加可重建的索引投影。插件持久发布与产物生命周期按以下阶段独立实施。

## 第三阶段 A：插件持久发布与保守保留

1. `plugin.Catalog` 将不可变 revision 数据与可变激活状态分开。SQLite migration 28 增加 `plugin_blobs` 和 `plugin_revisions`：WASM 按 SHA-256 去重，manifest 带版本、校验和及资源身份；blob 与 manifest 同事务暂存，不依赖文件落盘和数据库提交的双写补偿。`tool_name + revision` 永不重绑定，同名工具归属同一个 PluginID。单产物上限 32 MiB、manifest 上限 64 KiB，目录按 revision 引用累计源字节上限 128 MiB（同一 blob 的重复引用也计入；不是编译后内存上限）。
2. `Stage` 复制并验证元数据、将零 timeout 解析为显式默认值、编译并校验模块导入/导出约束，然后保存 staged revision；不发布工具。编译检查不等于行为验收，ABI 版本函数的实际返回值仍在调用时检查。manifest 是宿主可信元数据，内容哈希不构成来源认证；不接受模型自行降低风险、取得能力或晋升插件。
3. `Activate` / `Retire` / `Rollback` 通过 `Registry.*ManyWithCommit` 在发布锁内核对所有权，并先提交目录状态，再发布新的不可变快照。目录事务用 expected active revision 做 CAS，并以唯一索引确保每个工具至多一个 active revision。失败时不改变进程内已发布快照；若存储返回错误，提交结果可能不确定，Manager 拒绝后续变更，必须重开运行时以目录为准。不自动补偿或盲目重试；崩溃窗口也不提供跨进程 exactly-once 保证。
4. `App.Open` 在宿主工具装配后，通过 `OpenManager` 读取全部 retained revisions、验证身份/校验和/资源配置并重新编译，最后一次性发布 active 集合。任何 revision 损坏、资源身份改变或工具名冲突都使启动失败，不发布半套插件。恢复不执行插件函数、不自动提升 staged revision。`NewManager` 仍供明确的非持久场景使用；生产 App 始终装配 SQLite Catalog，并持有原有数据库独占所有权锁。
5. 插件不得覆盖当前宿主 executor 或其他 PluginID 的同名工具。`agent_` / `task_` / `merge_` / `goal_` / `flow_` / `plugin_` / `strategy_` 为 App 保留前缀，防止 Supervisor 延迟装配时冲突；Supervisor 发布本身也拒绝覆盖已有工具。检查既在 Stage 发生，也在发布锁内复核。实际插件调用仍经过原 Tool Pipeline、Policy、审批与事件记录，不因恢复或回滚扩大权限。
6. `Artifact(name, revision)` 与 `Inventory()` 提供 retained revision 的只读信息；`Retire` 只撤销后续快照中的精确 active revision，不关闭旧模块。进程内旧快照继续可用，staged/retired 代码均保留供显式激活或回滚。此 API 不触发自动删除或卸载；离线精确回收见 R1–R4。

阶段 A 的持久目录仅恢复当前激活组合；历史组合的清单、引用和重建由以下阶段 B 补充。仅保留旧插件代码不构成可恢复证明。插件 CLI、隔离编译、行为验证、审批发布与精确回收由后续 R1–R8 补齐。

验证覆盖真实本地 Go WASM 在 App 关闭/重开后的激活、停用与回滚；旧快照继续调用；并发 Stage；发布前失败、提交后丢失回执；blob 去重与事务回滚；目录损坏、资源漂移、同名接管及批量恢复的失败原子性。测试不调用真实模型，不下载评测数据。

## 第三阶段 B：历史工具快照重建与引用登记

1. `tool.SnapshotManifest` 提取原有 v1 内容地址格式：宿主二进制身份、排序/规范化后的完整工具描述和 ImplementationID。清单不保存 executor，也不授予执行权限。单清单最多 1 MiB、1024 个工具；未知版本/字段、重复名称及摘要不匹配均拒绝。SQLite migration 29 增加 `runtime_snapshots` 和 `runtime_snapshot_refs`，同事务保存不可变清单与消费方引用；同一个引用不能重新绑定另一快照。
2. `Factory.NewTurn` 在登记 Turn、构造 Runner 或调用模型之前保存快照引用；`ResumeTurn` 复核并保留原引用。Flow 在 `CreateRun` 之前保存引用。Turn 引用键为 `(turn, session ID, turn ID)`，Flow 为 `(flow, team ID, idempotency key)`，与既有 Flow 去重范围一致。保存失败则停止准入；提交后、任务登记前崩溃可能留下多余引用，但不会以清单尚未提交的状态开始执行。清单/引用事务不与 Session、Goal 或 Flow 记录组成跨领域大事务。
3. `Registry.ResolveSnapshot(ctx, id)` 先查进程内视图，缺失时从持久清单校验并重建。原生 executor 只复用当前宿主中元数据和实现身份完全相同的绑定；WASM 由 `Manager.ResolveBinding` 从 retained revisions 提供精确模块，Registry 再次核对完整绑定和最终内容 ID。缺失 revision、资源/宿主/元数据漂移时明确失败，不回退最新版，也不从清单执行任意源码。存储和 resolver 调用均不持有 Registry 发布锁，避免与插件发布锁形成反向锁序。
4. 重建的视图由当前 Registry 接纳，但不替换 `Registry.Snapshot()`，也不修改插件 active/retired 状态。导入和后续发布共享单调递增的进程内 generation 分配器，不能覆盖旧 generation；并发重建相同内容 ID 只接纳一个实例。`SnapshotByID` 仍是纯内存查询；Turn 恢复、已完成候选读取、Goal 文件验收和 Flow 执行均使用带 context 的显式重建路径。
5. `App` 为宿主绑定追加运行环境摘要，包含 canonical workspace、数据库路径、shell sandbox 模式、LSP 配置和模型连接配置，Supervisor 延迟装配时使用相同摘要。只保存摘要，不读取 API Key 环境变量值。更换工作目录、数据目录或连接/沙箱配置不能悄悄重定向旧任务。此摘要不是任意闭包状态的自动序列化：额外 executor 的配置语义变化仍必须由宿主更新 revision/ImplementationID。Profile、预算、时限、审批和 Tool Pipeline 的真实执行校验保持不变。
6. `App.RuntimeSnapshotReferences` 提供宿主侧未释放引用读模型。引用是保留依据，不是“任务仍在运行”的证明：已完成任务、尚需 Goal 验收/子任务收尾的任务，以及准入中断留下的引用都默认保留。显式放弃时的窄释放边界见第三阶段 C；当前没有自动回填全部历史事件、产物删除或模块卸载。`Retire` 只影响后续新任务，不是对历史任务的强制安全撤销；阻止历史执行仍须通过执行策略等独立撤销边界。

恢复边界：新记录了清单的任务可在兼容宿主环境下按原 ID 重建旧组合；没有清单的旧任务只有在原 ID 已存在于本进程时才可能继续，迁移不会猜造历史清单。缺少 checkpoint 的执行仍不会被补造。宿主直接恢复 Flow 时须提供原 `RuntimeSnapshotID`；嵌套 Flow 从原 Turn 的调用上下文携带该 ID，不能只拿相同 key 用最新快照重跑。本批新增宿主环境摘要会改变 App 快照身份，不会改写旧事件或绕过兼容性校验。

验证覆盖清单与引用的 SQLite 重开、不可重绑定、损坏和事务回滚；并发重建及 generation 不碰撞；真实 Go/WASM 旧版恢复且全局仍使用新版；已完成候选读取不再次调用模型；恢复后权限仍拒绝未授权插件、工作目录漂移拒绝；Flow 在重开后复用原 Run、回放已成功写入并用原版本执行剩余调用。人工放弃只解决引用生命周期的一部分；遗留任务保留策略和安全 GC 必须独立设计，不能直接把零引用数当作可删除证明。

## 第三阶段 C：离线放弃恢复与引用释放

1. `session.AbandonRequest` 明确选定 session 与一个 Turn/Goal、必填原因及“已检查副作用”的人工确认。CLI `chat --abandon-turn` / `--abandon-goal` 调用独立 `app.AbandonRecovery`：先取得原数据库 OS 所有权锁，仅打开 SQLite 并迁移，不装配 App 运行时、模型、WASM 或 Supervisor。不能在活跃实例旁修改生命周期，也不是模型可调用工具或聊天内命令。
2. 只处理根会话：直接未完成 Turn、带保留引用但尚无 turn.started 的准入中断、运行中 Goal（包括零轮次）均可显式放弃。指定 Goal 的当前 Turn 等价于放弃整个 Goal；之前轮次不能作为首次操作目标。子会话、非终态团队子 Agent/Task、本会话非终态 Flow 均保守拒绝，需先经各自所有者生命周期处理；不自动取消子工作、释放 workspace lease、处理合并、确认 mailbox 或退款。已完成直接 Turn/终态 Goal 不走此入口释放。
3. SQLite migration 30 为 `runtime_snapshot_refs` 增加释放时间和原因，并增加带校验和的 `recovery_abandonments` 回执表。一个事务复核归属、Goal 轮次、事件校验和和快照引用身份，写入每轮 `turn.abandoned`，将 Goal 置为 `canceled / abandoned` 并保存 `goal.abandoned`，标记对应引用释放、保存幂等回执；不删除行。失败全量回滚；相同目标/原因重试返回原回执，提交后丢失回执不会重复变更。改原因或选旧轮次不能冒充相同请求。此窄管理事务故意将终止消费方和释放引用绑定，普通任务准入仍沿用阶段 B 的保守先保留策略。
4. `session.CheckRecovery` 是执行端禁用检查，使用 correlation 读取或分页回退。`Factory.ResumeTurn`、已完成候选读取、Goal 文件验收、Conversation 的 Turn/Run 恢复和预构建 `Turn.Run/Resume` 均在继续执行前拒绝 abandoned Turn；恢复列表不再提供它。快照保存也拒绝重新登记已释放或已放弃的消费者，包括放弃时没有引用的旧 Turn。保留 checkpoint/manifest 不会重新授权执行，`Registry.ResolveSnapshot` 本身仍只负责内容重建而不是任务授权。
5. 所有原事件、opaque checkpoint、候选结果、轮次报告、WASM 和工作区保留。Goal 部分结果保存，不标为完成或声称回滚成功。回执明确 `effects_reverted: false`；用户确认仅是声明已检查，并非程序验证了外部副作用。丢失 checkpoint 的 legacy Turn 可以人工放弃，但不会补造清单、checkpoint 或成功证据。
6. `RuntimeSnapshotReferences` 仅返回未释放引用，`RuntimeSnapshotReferenceHistory` 包含释放原因和时间。共享同一 snapshot 的其他 Turn/Flow 不受影响。本阶段不自动释放、回填身份或卸载模块；后续的完成消费者归档和插件 GC 仍必须复核 legacy 消费者及生命周期依赖，不能依赖零引用数删除。

验证包括：持久回执跨重开幂等、共享快照隔离、旧引用不能复活、Goal 全轮次关闭且报告/候选保留、零轮次/无起点恢复、活跃 owner/子任务/Flow 拒绝、审计与回执写入失败的原子回滚、损坏事件拒绝、验收/已保存 verdict 不再恢复，以及 CLI 不读取新输入、不构造模型、不派发 Supervisor。所有放弃操作测试仅使用临时数据库。

## 收尾 R1–R4：保留期关闭、精确 GC 与隔离构建

`internal/retention` 定义不可变计划与回执，SQLite migration 31 持久化消费者关闭记录、维护回执与插件 tombstone。`app/maintenance.go` 只装配 owner + SQLite，不装配可执行注册表；预览与确认应用都用同一事务投影复核，计划改变或存在活动/未知依赖即拒绝。

完成消费者包括直接 Turn、终态 Goal 的各轮、已持久返回合法结果的子 Agent、关联的终态 Flow。关闭操作保存 `turn.archived` 并禁用执行/验收恢复，保留 checkpoint/结果/清单；Snapshot Save 拒绝复活已关闭身份。未完成根任务仍走原 abandonment 边界，不通过归档伪造完成。

插件回收扫描并校验快照清单、保留引用和未关闭历史消费者；未知 legacy 身份是全局保留根。事务内将原 revision 身份写入 tombstone、删除 exact revision，最后一个共享引用消失才删除 blob；同 revision 永不重绑定。没有进程内卸载、全目录清理、worktree/补丁/历史证据 GC 或自动回填。`compact` 仅归还 SQLite 已释放页面。

`internal/pluginbuild` 接收受限单文件源码，使用固定 Go 1.27.0、嵌入 SDK、标准库白名单和 OS 沙箱离线编译。无自定义 build flags、go.mod、宿主路径或能力授权。源码、Request、日志、WASM、构建时间和证据存储有界；Go 软内存目标不是操作系统硬 RAM 配额。进程组取消避免后代进程在清理后继续写入。

SQLite migration 32 保存先预留后派发的构建请求与结果、行为测试报告。构建重复请求只重放已完成结果；中断构建不自动重启。测试报告绑定 exact revision/source/runtime，测试通过不自动发布。App 管理构建/影子 worker 的取消与 join；持久证据失败作为 fatal tool error 停止 Turn，不被包装成可继续修复的普通执行失败。

## 收尾 R6–R8：工具和策略的受控自进化

模型通过统一 Tool Pipeline 调用 `plugin_propose/inspect/publish`。propose 经过审批后执行构建和行为测试，返回失败/隔离/待审批状态；修复通过原 Eino Loop 使用新 revision，不新写一套 Agent Loop。publish 单独审批，随后 `extension_*` 调用仍审批。工具自身只做纯 JSON 计算，不自行获得文件、网络或凭证能力。在途注册表始终冻结。

`internal/strategy` 的 LoopSpec 是追加策略提示和降低额度的数据协议，不能变更模型、授权、Goal Gate 或执行代码。`strategy.Store` 是 runtime 使用的窄读取/回退端口；装配层负责候选、shadow 和发布，SQLite migration 33 保存 immutable spec/suite/report、CAS active 指针、撤回隔离表及审计事件。根 Agent 的工具按可信 actor/session 归属校验，子 Agent 不能调用根影子模型。

可信 shadow suite 只能由宿主显式注册。`Factory.CompareStrategy` 为每个 fixture 构造两个工具为空的独立 Eino/内存会话，关闭 Goal、业务工具与发布能力；模型网关与 Team 记账仍共享。整体/单例时限为 2 分钟/30 秒，最多 8 题；保存完整失败报告，取消也不伪装通过。准入复核题目身份与安全标记、安全题全通过、精确匹配数不下降、模型请求数不增加。输出字节和耗时只作诊断，这不是通用能力、安全或计费证明。

报告绑定基础执行配置、宿主工作区/环境和实际 CLI 模型设置。Promote 以被测试的 PreviousSpecID 做 CAS；任何基线漂移需重新验证。Factory 在 NewTurn 选择策略，Turn 开始记录 `loop_spec_id` 与 `base_execution_config_id`；Resume/CompletedTurn 恢复旧策略而非读取当前 active。额度取宿主和候选较小值，不改变 Profile、policy 或历史 Goal 契约。

策略 Turn 执行失败时仅 CAS 撤回仍 active 的同一候选，恢复基础策略并隔离该 revision；旧报告不能再晋升，旧 Turn 失败不能撤掉较新候选。用户取消不自动判定策略失败。回退不回滚文件、不重放副作用、不退款，也不自动发现“运行成功但质量下降”。完整使用说明、资源上限和测试范围见 [EVOLUTION.md](./EVOLUTION.md)。

## 关键边界

- 配置加载是单独的边界：`GSEA_HOME` 决定用户配置和 profile 的根目录，项目 `.gsea/config.toml` 只在显式信任后加载并影响其目录树；环境变量和 CLI 覆盖文件值。API Key 只保存环境变量名，不进入 TOML 或 session。修复后的完整安全与持久性边界见 [AUDIT_REMEDIATION.md](AUDIT_REMEDIATION.md)。
- `internal/tui` 独占 raw terminal、ANSI、布局和交互状态；`internal/conversation` 独占 session 内 turn 串行化。TUI 不创建模型，不直接访问 SQLite，也不解释 Eino 事件。
- `session.EventStore` 只处理事件时间线；`session.CheckpointStore` 只处理 Eino opaque checkpoint。SQLite 可以同时实现两者，但业务代码不依赖一个过宽接口。
- `EventSink` 是 live-only 协议，不写入 durable store。它只传递 assistant/reasoning delta、完成消息和工具结果。
- `tool.NewPipeline` 是工具执行边界的生产构造入口；交互式、普通 headless 和 evaluation 模式通过 `Pipeline.WithApprover` 派生独立视图，不直接修改共享管线。Evaluation mode 的 allowlist 只解析精确工具名，未列出的审批请求默认拒绝。
- Tool Pipeline 使用 session+turn+call 作为 durable replay 作用域；成功事件已存在时返回原 JSON result，不再次执行副作用。Flow 的确定性 call ID 依赖这一语义完成 checkpoint 恢复。
- `Pipeline` 的 Registry、Policy、Approver 和 Event Store 只通过构造器注入并保持私有；调用携带的 turn Snapshot 必须由同一 Registry 发布或校验重建并接纳，跨 Registry 快照会被拒绝。
- `Conversation.History` 返回消息及其内部 slices/maps 的深拷贝，展示层不能意外修改下一轮模型上下文。
- Responses 网关分为三段：请求/响应转换、HTTP 生命周期、SSE 事件解析。不得把原始隐藏思维链请求、持久化或展示；只处理服务端明确返回的 reasoning summary。
- Registry 的每次 Activate/Retire 都产生新 generation。一个 turn 从创建到结束只使用自己的 snapshot，热更新不会改变在途调用；可恢复 Turn 还会校验 `model_profile`、provider 和 model，防止 checkpoint 在重启后静默切换模型。模型请求序号从 durable `model.requested` 事件和 reservation 记录恢复，usage 幂等键不会因进程重启复用。
- 所有动态工具都走相同的 Policy 和 Event Store 边界，原生工具与 WASM 工具不允许绕过管线。
- `toolbridge` 是 invocation-scoped 评测边界，不是第二套插件系统：manifest 不能降低 risk class，不能覆盖同名 built-in，Unix socket transport failure 作为 tool execution failure 持久化；生产扩展仍使用 WASM Plugin Manager。
- LSP 是 App 管理的可选运行时服务，不直接调用模型或修改文件。主动 `workspace_diagnostics` 仍经过 Tool Pipeline；被动反馈属于已审批写工具的结构化结果。shared workspace 复用 client，Git worktree 隔离 client，避免不同代码状态共享 document state。
- 论文 Rail 的跨生命周期思想通过窄的类型化 capability 融合：workspace mutation 使用 `ChangeSetDiagnoser` 和 `DiffArtifactStore`，Outer Loop 向 Context Policy 传递 `ContextHints`，大结果外置依赖 `ContextArtifactStore`。这些 capability 由 Composition Root 装配，有固定输入和失败语义；不存在可绕过 Tool Pipeline 的通用可执行 hook 总线。

## 扩展规则

- 增加模型协议：在 `internal/modelgateway` 内实现 `model.ToolCallingChatModel`，不要让 CLI 或 runtime 读取 provider-specific 字段。
- 增加工具：注册 `tool.Binding`；执行逻辑实现 `tool.Executor`，授权通过 `policy.Rule` 配置。
- 增加 Agent 可调用流程：用 `agentflow.Pipeline(name, revision, ...)` 组合 operator，再通过 `App.FlowCatalog().Register` 发布命名 Flow；callback 语义改变必须升级 revision，拓扑改变由 fingerprint 自动识别。外部动作使用 `agentflow.Tool`/`AgentSession`/`Human`，不要在纯 `Map` callback 中隐藏副作用。
- 增加语言服务器：扩展 `internal/lsp` 的文件类型/language ID 和可信进程配置；保持诊断有界、workspace-relative，并保证服务失败不会把已经发生的文件副作用包装成可重试错误。
- 增加持久化投影：消费 `session.EventStore.Load`，不要修改事件含义或把投影状态塞入 `Turn`。
- 增加终端交互：扩展 `internal/tui` 的 Bubble Tea model/message/view；headless 渲染扩展 `internal/render`，使用 Chroma lexer/formatter，不要在 runtime 事件中加入 ANSI、按键或 UI 文本。
- 增加插件能力：通过 `App.Plugins()` 先 `Stage`，完成宿主验收后显式 `Activate`；`Retire` 停用精确 active revision，`Rollback` 使用 retained revision，不修改在途 snapshot。不能绕过 Catalog 直接覆盖插件 binding，也不能把 retired 当作可安全删除的证明。

## 验证顺序

```bash
GOPROXY=off go test ./... -count=1
GOPROXY=off go vet ./...
GOPROXY=off go test -race ./... -count=1
```

以上离线命令要求依赖已在本地缓存。阅读或修改一个模块后，先运行该模块测试，再运行完整测试；Responses、session、WASM 和 runtime 的集成测试共同覆盖跨层契约。架构依赖、七个自进化工具描述和已发布迁移校验和的契约测试均包含在全量命令中，不能通过重生成 fixture 来掩盖普通重构中的协议漂移。

## 动态任务与恢复结算

Eino 拥有单个 Agent 的工具循环；Supervisor 拥有 Agent 树、并发准入和持久 Task 调度。模型可以即时派生子 Agent、修改未执行的计划、通过 `task_refine` 追加细化成员及依赖。普通 `ParentTaskID` 仅分组；`task_refinement_members`（migration 40）才表示父完成必须等待的成员，循环检查使用普通依赖与细化等待边的并集。

`TaskPlanRepository`、`TaskRefinementRepository` 和 `TaskReassignmentRepository` 是可选的存储扩展，避免把新功能强加给简单适配器。计划编辑使用当前版本 CAS，自动 spawn 在同一事务中核对所读 Task 版本并认领，阻止过时说明与新任务契约混用。失败改派只接受宿主认可的失败类型，并证明没有写入、未知回执、存活后代或未清理资源；默认总执行次数上限为 3。

`agent_completions` 冻结原始 worker 结果，Task/交付/Agent 终态和终态后操作按其确定事实补齐。结算错误保留 receipt，由周期扫描与启动恢复继续；不把协调错误覆盖为 worker 失败，也不重新运行已返回的 worker。旧 Run 回调与 receipt 都要通过 RunID fence。Turn journal 重放在新的摘要、mailbox 和模型请求之前匹配原始输入；缺字段或摘要无法证明兼容的旧回执明确拒绝恢复。
