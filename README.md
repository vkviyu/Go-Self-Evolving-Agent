# Go Self-Evolving Agent

假设昨天排查一次模型流超时，工具记录显示响应头已到、响应体一直没有新字节。今天另一个项目也报告超时，主 Agent 可以在共享的 Agent Temporal Engine（ATE）中查找已验证且仍可复用的排查经验，核对来源，再用今天的请求记录判断是否适用。两次故障的原因要分别取证。

这就是 GSEA 名称里 **Evolving** 的含义。任务留下的事实可以形成带来源的记忆，供其他任务查询、核验和显式采用。发现反例或来源失效时，可以纠正或撤回其复用资格。GSEA 在本地用 Eino ADK 运行模型与工具循环，是否查记忆由主 Agent 按需决定。[跨任务记忆指南](./EVOLUTION.md)

会话、任务、消息和执行事实直接保存在 ATE；GSEA 的 `runtime-v3.db` 只保存 checkpoint、宿主运行资源及未完成请求。旧版 `runtime.db` 和 `runtime-v2.db` 不会被当前版本读取或迁移。权威存储切换已完成并通过自动化集成验收，验证范围与恢复边界见 [ATE 权威存储迁移](./docs/ATE_AUTHORITY_CUTOVER.md)。

完整方案见[设计文档](./自进化本地智能体（Self-Evolving%20Local%20Agent）.md)；按目录阅读请先看[架构导读](./ARCHITECTURE.md)；多 Agent 的完整控制面、状态机与交付方案见 [Multi-Agent 子系统架构](./MULTI_AGENT_ARCHITECTURE.md)，面向使用的并行改造（P0–P3 已完成，P4 同目录并行写入未实现）见 [Multi-Agent 并行执行改造计划](./MULTI_AGENT_PARALLEL_PLAN.md)。

工具分组、三级披露与按需加载的待实施方案见[渐进式工具发现设计](./TOOL_DISCOVERY_DESIGN.md)；该文档不表示功能已经实现。

功能边界补齐后的 F1–F5 架构重构已完成，不改变现有权限与持久化协议；各批次交付及全量/静态/竞态验收记录见 [重构计划](./REFACTORING.md)。

排查模型流超时、工具等待或并行子 Agent 运行时，可以按会话和请求查看本地诊断轨迹。见[执行观察与排障](./docs/EXECUTION_OBSERVATION.md)。

## 已实现

- Eino `ChatModelAgent` / `Runner` 的真实 ReAct 循环；
- turn 级不可变 Tool Registry Snapshot，热更新不影响在途调用；
- Eino Tool Adapter，模型 tool-call ID 贯穿策略与审计事件；
- 默认拒绝的 Policy Engine，以及统一的提议、审批、执行、结果管线；
- ATE 原生只追加 Session Event Store，带乐观序列、事件校验和及可追溯的历史来源；
- `memory_query` 只读查询 ATE，`memory_manage` 经审批写入候选、记录采用、撤回或纠正来源。候选需经过独立验证和晋升才有资格供后续任务检索。
- 宿主可单独评估版本化 LoopSpec。影子对比、审批和回退约束后续 Turn 的策略选择。
- `maintenance` 可预览并归档已结束的恢复消费者，按回执释放本地工具快照引用。
- `gsea doctor` 启动自检。
- OpenAI-compatible / Ollama ModelGateway 与可恢复的 `gsea chat` 多轮会话；
- OpenAI Responses SSE 与 Eino 流式消息统一转换，CLI 实时输出 assistant delta；
- Responses reasoning effort、官方 reasoning summary 增量和 reasoning token 用量展示；
- 独立 Eino Checkpoint Store，支持 turn 中断后的 `ResumeTurn` 恢复；
- 完整 ReAct 历史投影（assistant tool calls、tool results）与按 token 触发的上下文压缩。
- 原生 ATE 基础设施接入：应用通过 stdio 调用配套的 `ate`，会话、模型与工具事实、编排对象直接提交和读取 ATE。主 Agent 按需选择记忆查询和维护工具；普通 turn 不再强制运行经验检索、辅助路由或结束后的自动提炼。记忆查询不等于采用，候选写入不等于验证或发布；修改仍经过统一 Policy/Approval 和持久化协议。配置、生命周期、恢复和验证边界见 [ATE 接入说明](./docs/ATE_INTEGRATION.md)。
- 基于 `gopls` 的 LSP 闭环语义反馈：结构化写入和 `shell_exec` 的 bounded ChangeSet 都会自动回注排序、去重、限量的 diagnostics；报告按 workspace/path/content revision 持久化，并提供主动只读查询工具。
- 动态 Context Policy：按上下文窗口、压缩阈值和当前 token 占用分级，支持手动压缩；Team 累计消费不参与分级、触发或保留量计算。大工具结果自动外置为 content-addressed artifact，模型可通过 `artifact_read` 分段召回；压缩保留 active execution contract 锚点，并在临界上下文压力下提供明确标记的抽取式 fallback。append-only 原始历史保持不变。
- 统一执行模型：普通问答由一个 Eino Turn 完成；需要验收、预算、截止时间或多 Agent 协作的工作使用 Query Plan、Task、Execution 和 Step 表达。ExecutionContract 承载范围、约束、验收策略和资源预算，Task verification、Workspace merge、Tool Pipeline 和 durable recovery 分别负责验收、交付、安全边界和恢复。
- composable Flow Runtime：提供 `budget`、有界并行 `parallel`（以及可调的 `ParallelWithConcurrency`）、`compact`、`each`、streaming `pipeline`、`agent_session`、`human`、`return`、纯 `map` 和通用 `tool` 算子；Flow 及步骤事实持久化到 ATE，运行时 checkpoint 和工具快照留在本地。显式 revision 与自动 topology fingerprint 共同约束恢复，重复 payload 通过 item lineage 保持独立 step 和 child Agent session，Agent 只能通过 `flow_list`/`flow_run` 调用宿主注册的命名 Flow。
- Flow 的 spawn/continue/wait、预算读取、人工介入和普通工具动作全部回编译到当前调用者的 Tool Pipeline；固定 registry snapshot、policy、approval 和事件语义不会因嵌套 Flow 改变。确定性 call ID 会回放已持久成功的工具结果，Runtime 还会串行化同进程同 key 的重复执行，避免 Flow checkpoint 恢复重复副作用。
- Multi-Agent Phase 0/1/2 控制面、Phase 3 Task DAG 基础闭环与 Phase 4 Workspace Coordinator 基础闭环：类型化 Team/Agent/Thread/Task/Run ID、状态机、Actor Scope、Profile 快照注册表、ATE 原子事实仓库、durable Supervisor、恢复和 Agent control tools；已支持有界并行、durable parent-child mailbox、顺序投递、at-least-once ack、root turn 安全注入、消息幂等、spawn request hash 冲突检测，以及 Task CRUD、依赖环检测、依赖阻塞/自动解锁、状态推进、claim/release、claim TTL 回收、Task-Agent 生命周期联动、就绪任务自动派发和 reviewer verification gate。可写子 Agent 默认（`[agents] default_workspace = "auto"`）在私有 snapshot view 中工作，完成后经三方合并生成 ApplyPlan，由 `merge_apply` 审批后通过 apply journal 写回，冲突生成 Resolution Task。已支持执行前版本化编辑 `task_update`、原子增量细化 `task_refine` 和保守的失败任务重新指派。任务依赖图随执行增量维护，不要求预先生成完整 DAG；关键路径优先级和 Phase 5 尚未实现。
- 动态任务计划：`task_update` 修改尚未执行的 draft/blocked/ready 任务，`task_refine` 在同一事务中添加 1–64 个子任务和最多 256 条依赖；两者要求 `task_list` 返回的 `expected_version`。只有细化成员约束父任务完成，普通 `parent_task_id` 保持分组语义；模型选择何时细化，宿主验证权限、版本、幂等和循环。自动派发绑定 Task 版本，避免旧说明执行新计划。
- 失败改派：API/网络/预算中断保留原 Run，不创建新 attempt。新 attempt 仅接受宿主可信的 iteration-limit 失败，并逐项核对原执行的纯读/成功回执、后代、lease、view 和 merge 状态；任何写入、结果未知或缺失回执均拒绝自动重跑。每个 Task 最多 3 次执行，包含首次；`task_reassign` 使用相同约束。
- Team 级中断恢复：终态结果与父级 mailbox 通知原子提交；模型传输、限流、供应商不可用、凭据、额度和预算故障保留原 RunID 进入 `recovery:<kind>`，可通过 `agent_resume` 显式恢复；Turn journal 可在 Eino checkpoint 缺失时复用已确认响应、有效请求上下文和工具回执继续执行；拒绝/已知工具错误也重放原结果。执行结果冻结后，协调层失败只重试 Task/交付/通知/清理结算，不再调用 worker。恢复只重放确定性事实，副作用未知时停止核对。旧执行准入记录若缺少可验证绑定会标记为 unavailable，不会按普通聊天执行。
- TUI 在子 Agent 活跃时显示可点击的 `main` 和子 Agent 快捷列表，空间不足时收为一行概览。点击 Agent 查看其执行记录，点击 `main` 或按 `Esc` 返回主会话。点击概览、按 `Ctrl+G` 或输入 `/agents`（别名 `/activity`）打开完整面板，支持鼠标滚轮浏览，可查看已结束的 Agent、Task、Workspace View、Lease 和 Merge 状态，也可中断子 Agent、重新请求合并审批或丢弃候选。`[agents] auto_continue`（默认开启）让子 Agent 消息在 root 空闲时自动唤醒 root 处理（TUI 中生效）。
- `gsea control` 提供 `gsea.command.v1` 输入与 `gsea.control.v1` JSONL 输出，可查询快照/事件以及执行受边界约束的 Agent、Task、Merge 控制命令，适合断线重连和自动化客户端。
- `gsea eval` 提供语言无关的 `gsea.eval.task.v1` JSONL runner：任务可从固定 Git commit 创建隔离 worktree，也可从非 Git fixture 创建一次性 `directory_copy` 工作区；以 evaluation allowlist 执行 GSEA，捕获包含新文件的 binary-safe patch，在 Agent 不可见的 sandbox/Docker verifier 中检查目标、回归和安全条件，并输出 `gsea.eval.result.v1`；结果可直接导出为 SWE-bench prediction JSONL。`eval score` 读取 Harbor Terminal-Bench 2.1 或官方 SWE-bench `results.json`，用官方分母、失败规则、标准误和 pass@k 生成独立可比性报告，避免把开发期 smoke rate 当作排行榜分数。

## 代码边界

```text
cmd/gsea             可执行入口；只处理信号和进程退出码
internal/cli                 命令解析、依赖编排、chat/headless 协议
internal/app                Composition Root；内置策略和工具原子装配
internal/config             TOML/profile/项目配置/环境变量的分层加载
internal/runtimeowner       运行库与 ATE 会话执行身份的进程级所有权锁
internal/agentruntime       Eino Agent/Runner 与耐久 turn 协议
internal/ateprocess         原生 ATE stdio 子进程、paired binary 解析和生命周期回收
internal/aterequest         原生请求身份、未完成请求核对与确认后正文清理
internal/ateauthority       ATE 截面读取、原子实体事务、条件提交和原生分页
internal/atebridge          ATE 原生来源证据、经验与执行生命周期
internal/experience         ATE 记忆查询、来源核验、候选、显式采用与撤回；可选经验整理
internal/experienceport     runtime 使用的稳定经验端口类型，隔离具体实现
internal/ateports           宿主对 ATE 的端口：精确历史读取与语义决策状态；不是投递日志或 outbox
internal/agentflow          命名 Flow Catalog、流式组合算子和 durable Flow Runtime
internal/orchestration      Multi-Agent 控制面 ID、Agent/Task 状态机、事件 envelope 和 root projection 契约
internal/agentprofile       不可变 Agent Profile 配置快照与注册表
internal/agentmanager       durable Supervisor、child lifecycle、并行调度、mailbox、workspace 分配/merge 投递和 Agent/Task/merge control tools
internal/scheduler          模型并发桶与按 Team 轮转的候选排序（RoundRobin）
internal/workspacecoord     workspace view、snapshot 候选、三方合并、ApplyPlan/apply journal、Resolution Task 与 Git 补丁保留
internal/workspacedomain    跨数据目录/GSEA_HOME 的同一工作区身份、实例登记、写 lease 与 rendezvous（独立 coord.db）
internal/sessionregistry    ATE 中的 Session 目录与本地资源位置关联
internal/approval           TUI 审批 Broker 与 evaluation 精确工具 allowlist
internal/sandbox            shell_exec 的 OS 沙箱与容器边界调用
internal/artifact           content-addressed Artifact Store
internal/modelcontext       上下文窗口、最大输出与输入预算解析
internal/tokenizer          本地 BPE tokenizer 选择与估算标记
internal/tokenusage         模型请求预留/派发/回执/核销账本
internal/evolutiontools     策略工具描述、严格 JSON 解码与可信调用者适配
internal/evolutionstore     ATE 中的 LoopSpec、影子对比报告和策略状态
internal/einoadapter        Eino Tool 到统一执行管线的适配
internal/modelgateway       OpenAI Chat Completions / Responses、Ollama provider 构造
internal/render             headless 流式 Markdown、Chroma 代码高亮和 ANSI
internal/tui                Bubble Tea 交互式终端 UI（核心状态、命令、按键和 turn 状态分文件）
internal/evaluation         语言无关任务协议、隔离 worktree、隐藏 verifier、结果与 benchmark 导出
internal/conversation       session 对话编排与 turn 串行化
internal/codingtools        Coding Agent workspace 工具集（workspace/paths/read/search/write/shell）
internal/lsp                LSP stdio/JSON-RPC 客户端、workspace client 生命周期和 diagnostics 筛选
internal/tool               不可变注册表和统一工具执行管线
internal/toolbridge         评测专用 Unix-socket 动态工具传输；仍经过 Eino/Policy/Tool Pipeline
internal/policy             默认拒绝的授权决策
internal/session            append-only 事件协议、EventStore 与 CheckpointStore 接口
internal/session/memorystore 测试与临时运行的内存 Store
internal/session/atestore   ATE 原生会话事件、载荷、历史读取及来源引用
internal/orchestration/atestore ATE 上的任务、Agent、Goal、Flow、消息与编排事实
internal/runtimestore      本地运行资源、checkpoint、LSP 缓存、工具快照引用与未完成请求
internal/strategy           不可变 LoopSpec、可信影子样例与准入规则
internal/retention          显式关闭恢复的计划与回执
internal/architecture       仅测试：依赖方向与关键构造点白名单
```

App 装配领域服务。Eino 发出的工具调用统一经过宿主的 Policy 与 Event Store。

### 当前运行时结构

```text
cmd/gsea
  └─ 进程入口：信号处理与退出码
          │
          ▼
internal/cli                 命令解析与应用编排
  ├─ chat：TTY 进入 internal/tui，管道进入 headless runner
  ├─ doctor：调用本地诊断，不创建模型连接
  └─ session / control / eval / maintenance / strategy
          │
          ▼
internal/app                  Composition Root
  ├─ config → ATE 会话与编排仓库（原生 stdio）
  ├─ runtime-v3.db → checkpoint、宿主运行资源、未完成请求
  ├─ experience → ATE 语义空间、记忆与采用记录
  ├─ LSP Manager（按 workspace/worktree 懒启动）
  ├─ Policy Engine + Tool Registry
  ├─ Tool Pipeline
  └─ named Flow Catalog + durable Flow Runtime
          │
          ▼
internal/conversation          普通 turn + 显式任务准入 + durable Outer Loop
  └─ acceptance: adaptive / self / independent / hybrid + hard caps
          │
          ▼
internal/agentruntime          每个 round 对应一个 Eino Inner Loop Turn
  ├─ Factory：固定 Registry Snapshot，创建 Eino Runner
  ├─ History：从已完成事件投影模型上下文
  ├─ Turn：记录开始/完成/失败，消费流式 Eino 事件并管理 checkpoint
  └─ Model：记录模型请求和 assistant 消息
          │
          ▼
Eino ChatModelAgent              ReAct: model → tool → model → final
          │
          ▼
internal/einoadapter → internal/tool.Pipeline
  proposed → policy → approval → started → executor → succeeded/failed
```

`cmd/gsea` 管理进程生命周期，`internal/cli` 解析命令并编排依赖，`internal/app` 负责应用装配和关闭。`internal/agentruntime` 管理 Eino Agent 与 Turn 协议，`internal/tui` 和 `internal/render` 负责展示。Agent 调用工具时统一经过 `internal/tool.Pipeline`。

当前 `chat` 启动时注册 `runtime_diagnostics`、`artifact_read`、记忆工具和 Coding Agent workspace 工具集。启用 LSP 时还注册只读 `workspace_diagnostics`。`workspace_write`、`workspace_edit` 和 `shell_exec` 成功后会把 bounded unified diff 与被动诊断合并进同一个工具结果。小 Diff 直接内联，大 Diff 写入 content-addressed artifact 并返回 `artifact_ref`，可通过 `artifact_read` 分段读取。TUI 在工具详情中展示 Diff，`/diff` 可直接查看当前记录中的代码变更。

普通工具也有快照。每个 Turn/Flow 在执行前保存当时可用的工具名称、Schema、实现身份及消费者引用。恢复时宿主按原内容 ID 核对工具和环境，身份不兼容就停止。`App.RuntimeSnapshotReferences` 可查未释放引用，`RuntimeSnapshotReferenceHistory` 可查释放原因。

离线 `maintenance plan` 和 `maintenance archive` 可为符合条件的 Turn、子 Agent 和 Flow 关闭恢复，待 ATE 确认后释放本地快照引用。原有会话历史和记忆来源仍可按权限核查。[恢复与归档边界](./EVOLUTION.md)

## 安装与运行

当前 `go.mod` 通过本地 replacement 使用 sibling ATE 工作区；先准备匹配版本的 ATE 源码，再在仓库根目录用 Go 1.27.0 构建成对的 `gsea` 与 `ate`。这不是脱离工作区的独立安装流程：

```bash
mkdir -p "$HOME/.local/bin"
go build -o "$HOME/.local/bin/ate" github.com/vkviyu/agent-temporal-engine/cmd/ate
go build -o "$HOME/.local/bin/gsea" ./cmd/gsea
"$HOME/.local/bin/gsea" doctor
```

在仓库中可以用 `make build` 同时生成 `bin/ate` 与 `bin/gsea`，再运行 `./bin/gsea doctor`。使用 `go run` 时，临时可执行目录不包含配套 `ate`，需要把它加入 `PATH`，或显式设置 `GSEA_ATE_BINARY="$PWD/bin/ate"`。

## 开发与验证

项目使用 Go 1.27.0。确认 `go version` 后运行验证命令：

```bash
go version
go test ./...
go vet ./...
go test -race ./...
go run ./cmd/gsea doctor --data-dir /tmp/gsea-doctor
```

依赖已缓存时可为测试命令设置 `GOPROXY=off`，避免下载。记忆工具的权限、路由及 ATE 读写契约可从 `internal/app` 和 `internal/experience` 的测试查起。[阶段验收记录](./REFACTORING.md)

## 配置与本地对话

配置根目录由 `GSEA_HOME` 控制；未设置时使用用户家目录下的 `~/.gsea`。配置环境变量统一使用 `GSEA_*` 命名。配置文件只保存非敏感设置，API Key 永远通过环境变量解析。

配置按以下顺序叠加，后面的值覆盖前面的值：内置默认值、`$GSEA_HOME/config.toml`、`$GSEA_HOME/<profile>.config.toml`、从当前目录向上搜索的项目 `.gsea/config.toml`、`GSEA_*` 环境变量、CLI 参数。profile 可通过 `GSEA_PROFILE` 或 `--profile/-p` 指定；指定但不存在的 profile 会直接报错。

项目配置可能携带可执行命令、API 地址与凭据环境变量名，默认拒绝加载。检查文件后，可用 `GSEA_TRUST_PROJECT_CONFIG=true go run ./cmd/gsea chat ...` 明确信任本次启动找到的项目配置；宿主嵌入可设置 `config.LoadOptions.TrustProjectConfig`。仓库配置不能给自己授信。用户配置与 profile 仍按上面的优先级加载。A–E 审计的修复状态及剩余限制见 [AUDIT_REMEDIATION.md](AUDIT_REMEDIATION.md)。

同一 `data_directory` 的数据库同时只能由一个应用实例持有，锁在打开 SQLite 和恢复 Supervisor 之前取得。第二个实例会明确报错；应先关闭原实例，或为独立运行使用不同的数据目录。不要删除 `.owner.lock` 来“解锁”：进程退出会由操作系统自动释放锁，锁文件保留是正常的。此锁只管每个数据库的所有权；不同数据目录（甚至不同 `GSEA_HOME`）的实例打开同一工作区时，由 `internal/workspacedomain` 按目录的 dev+inode 身份登记实例并协调写 lease（数据位于 `$GSEA_HOME/workspaces/<workspace-id>/coord.db`，跨 `GSEA_HOME` 通过 `GSEA_COORDINATION_DIR` 或平台默认 rendezvous 目录汇合）。

`chat` 自动在 ATE 登记会话及其工作区和数据目录，历史也直接写入同一个 ATE 权威存储。通过 `session create` 创建的托管会话默认使用独立的 `$GSEA_HOME/sessions/<session-id>/data/runtime-v3.db` 保存运行资源。工作区可以是任意普通目录。同一 ATE 会话另有独占执行锁，更换本地数据目录不能同时运行该会话。

在终端运行 `gsea resume` 会打开当前工作区的会话选择器，可以按标题、工作区或会话 ID 搜索。`--list` 直接打印列表，`--last` 打开最近一次会话，`--all` 也显示其他工作区及工作区未记录的会话。列表和历史直接从 ATE 读取。打开具体会话执行时，GSEA 再校验该会话的运行库与 ATE 身份、checkpoint 和原工具资源。旧 `sessions.db` 原地保留，不在启动时自动导入或作为恢复后备源。

```bash
go run ./cmd/gsea session create \
  --name runtime-analysis \
  --id runtime-analysis \
  --workspace /Users/me/work/runtime-analysis
go run ./cmd/gsea session list
go run ./cmd/gsea session show runtime-analysis
go run ./cmd/gsea chat --session runtime-analysis
go run ./cmd/gsea resume --list
go run ./cmd/gsea resume --last
go run ./cmd/gsea resume <session-id>
```

带有中心登记的 `chat --session` 会自动使用 Session 绑定的 workspace 和 data directory；显式传入 `--workspace` 或 `--data-dir` 时，命令行参数优先。同一 Session 内的可写子 Agent 默认在私有 snapshot view（`<data_directory>/views`）中并行工作，结果经合并审批后写回，不需要 Git branch 或 worktree；多个 Session 指向同一源目录时由 workspacedomain 协调写 lease（写操作互斥而非并行）；直接在主工作区按 `owned_paths` 并行写入（P4）尚未实现。Session 的 artifacts 也保存在其 runtime data directory，不写入用户工作区。

关闭超时时，仍在运行的策略比较或 Supervisor worker 可能继续使用资源。App 会返回错误并保留所有权，重复关闭仍返回原错误，需要退出该进程后再重开。离线维护和评测后的 durable 用量读取同样遵守所有权检查。

`workspace_directory`（或 `GSEA_WORKSPACE_DIRECTORY`、`chat --workspace`）定义 Coding Agent 的文件和命令边界；未指定时使用启动目录。内置读取和搜索工具自动允许，文件写入、精确编辑和 shell 命令必须在 TUI 中审批；普通非 TTY 管道运行仍会对这些高风险工具拒绝执行。隔离 benchmark runner 可以显式传入 `--evaluation-mode` 和 `--workspace`，此时只自动批准 `workspace_write`、`workspace_edit`、`shell_exec` 与子 Agent snapshot 合并的 `merge_apply` 审批请求，其他需要审批的工具继续拒绝。

内置 Coding Agent 工具包括：`workspace_read`、`workspace_list`、`workspace_glob`、`workspace_grep`、可选的 `workspace_diagnostics`、`workspace_write`、`workspace_edit`、`shell_exec` 和只读分段召回工具 `artifact_read`。`workspace_read` 在完整读取的文件不超过 4 MiB 时返回整个文件的 `sha256` 指纹及 `fingerprint_complete=true`，可用于后续 `expected_sha256` 校验；超过 4 MiB 的文件仍可分页读取，`fingerprint_complete=false`。`max_bytes` 约束所选行范围的内容，默认 16 KiB。读取大文档时可用返回的 `next_start_line` 或 `next_offset` 接着读取。`shell_exec` 可用 `timeout_ms` 为单条命令设置执行时限。省略时沿用调用方的上下文和取消信号。结构化 write/edit 的输入和输出同样最多 4 MiB，扩大替换在分配前检查。结构化写入结果的 `diff` 对象和 `shell_exec` 结果的 `diffs` 数组均包含路径、变化类型、状态和 unified diff 内容；`binary`、`unavailable`、`partial` 状态会明确说明限制。多 Agent 控制工具包括 `agent_spawn`、`agent_continue`、`agent_list`、`agent_wait`、`agent_interrupt`、`agent_message`，以及 `task_create`、`task_list`、`task_transition`、`task_verify`、`task_claim`、`task_release`、`task_add_dependency`、`task_remove_dependency`；配置了 workspace coordinator 时还有 `merge_list`、`merge_requeue` 和仅 root 可用的 `merge_run`。Flow 控制工具包括 `flow_list`、`flow_run`、`runtime_budget` 和 `human_confirm`。带 `verification_profile` 的任务在 worker 完成后进入 `verification`，必须由本 Task 当前 attempt 指派、匹配 Profile 且不同于 worker 的 reviewer Agent 调用 `task_verify`；通过后才进入 `completed`，拒绝则回到 `ready` 并递增 attempt。Task claim 默认有 30 分钟 TTL，过期 claim 只会在没有 live Agent 的情况下回收到 `ready`，并递增 attempt、记录 `task.claim_expired` 事件。工具调用统一经过注册表快照、完整 JSON Schema 参数校验、策略判定、审批和事件持久化；同一 session/turn/call 的已成功结果可安全回放，跨 turn 重用 provider call ID 不会错误去重；Pipeline 依赖只能通过构造器注入，且会拒绝来自其他 Registry 的快照；TUI 会在执行需要审批的修改或命令前暂停并等待用户决定。审批卡会展示待确认的 unified diff。

在本仓库工作区可以直接问「请阅读 `@ARCHITECTURE.md`，解释 Turn 中断后如何恢复」。也支持 `@"ARCHITECTURE.md"` 这种引号写法，路径含空格时用引号包住完整路径。可明确识别的普通路径和工作区内的绝对路径也会解析为工作区相对路径。首个模型请求可附带文件字节数、完整扫描后得到的 UTF-8 字符数和行数，以及 Markdown 标题的行号。元信息沿用当前 Agent 的工作区和 `workspace_read` 权限，只扫描有限字节，并按当前请求的输入空间裁减。模型据此自行选择 `workspace_read` 的行范围或字节量，文件正文由工具按需读取。

LSP 默认通过 stdio 懒启动 `gopls serve`。结构化写工具直接产生单文件 ChangeSet；`shell_exec` 在 workspace lease 内比较命令前后快照，返回 bounded 创建/修改/删除列表，并把最多 128 个变化的 Go 文件作为一个批次同步给 LSP。诊断按 workspace root、相对路径和内容 SHA-256 revision 保存到 SQLite；语言服务器重启失败时，同一 revision 可以返回带 `cached: true` 的 durable 报告。诊断失败不会回滚或否定已经完成的文件写入。安装 `gopls` 后可使用默认配置，也可以通过 `[lsp]` 或 `GSEA_LSP_*` 环境变量调整。

`shell_exec` 默认只允许通过操作系统 sandbox helper 执行：macOS 使用 Seatbelt，Linux 使用 bubblewrap；未检测到后端时明确拒绝执行，不会退回到普通宿主子进程。OS 模式按调用者的实际 workspace view 收紧权限，而不仅改变命令工作目录：可写 view 与每次调用独立的临时目录可写，只读请求不给 workspace 写权限；系统运行库、显式工具链、PATH 目录和模块缓存按白名单只读，其他宿主文件内容默认不可读。`HOME`、`TMPDIR`、`GOCACHE` 等指向私有临时目录，命令结束后清理；不能依赖宿主 home 配置或跨命令缓存。网络默认关闭，环境变量只保留明确的 locale/终端/工具链白名单；bubblewrap 还隔离 PID/IPC namespace，避免通过宿主 `/proc` 重新访问文件系统。LSP 同样通过只读 OS 沙箱启动，写管道与协议输入有界。自定义工具链可能需要补充受信任的窄读取路径，不能用恢复全盘只读来解决权限错误。

子 worktree 的 `.git` 若指向父仓库的共享元数据，Shell 中依赖该元数据的 Git 命令也会受限；不能为此开放父目录。补丁捕获和受控合并由宿主的 `workspacecoord` 执行，后续可为 Agent 增加专门的 Git capability。

只有显式 `--evaluation-mode --shell-sandbox container` 且进程已位于容器内时，才把文件系统、进程与网络边界委托给外层一次性评测容器。该选项只供评测调用。容器模式仍过滤凭证，并拒绝无法由外层容器强制隔离的、更窄的子 Agent worktree。Shell 变更和 Diff 快照最多读取 8 MiB，单文件最多 4 MiB，单个内联 Diff 最多 96 KiB。超出后保留变更摘要并返回截断状态或 artifact 引用。

### 恢复与产物保留

- 新 Turn/Flow 使用内容寻址的 Registry ID，包含工具描述、版本、动态实现身份和宿主可执行文件摘要，不再用进程内 generation 判断跨进程身份。同一内容的注册顺序可以改变；工具或程序变化、以及旧 `registry-N` 中断 checkpoint 会明确拒绝恢复，不会猜测兼容。已完成的会话历史仍可读取；先检查之前的副作用，再决定是否新开回合。
- 同一 session/turn/call 已持久成功时，恢复只返回原结果。有 `tool.started`、但没有成功结果的写入/执行/敏感调用会返回 `OutcomeUnknownError` 并停止自动重放，需要先核对真实外部状态；只读/纯计算调用可以重试。这不等于所有工具具备 exactly-once 语义，也不对新的 call ID 去重。
- `Conversation.ResumeTurn` 按 durable Goal 阶段恢复：执行已完成但未验收时只做验收；验收结论已保存时复用结论，只有明确的 `continue` 才进入下一轮。继续反馈和部分结果跨重启保留，资源上限不会重新计时。根会话加载后提示有中断任务，可用 `/resume` 恢复前台 Turn，或通过 `/resume <turn-id>` 明确选择；Supervisor 对已委派子任务沿用原 Run ID 自动恢复，不以新 Turn 替代旧任务。
- 新压缩事件同时保存摘要与保留的完整消息后缀，不把尚未提交的新用户输入重复写进历史；工具调用及其结果不会被压缩边界拆开。旧压缩事件继续兼容原有投影语义。
- Git worktree 的 Agent 结束或合并完成后，保存包含新增、删除和二进制文件的补丁及 `workspace.view_preserved` 事件，原目录保留用于检查。补丁捕获使用私有 Git index，不改动 worker 的暂存区；忽略文件不进补丁，但仍留在目录中。保存失败也不会删除目录。只有没有改动、没有忽略/未跟踪文件、HEAD 未偏离基线的 worktree 才能走普通释放；自动合并授权及产物 GC 不由任务终态隐式触发。默认的 snapshot view 则在 Agent 结束时：无改动直接丢弃；merge 处于 pending/running/conflicted 时保留；合并已应用后丢弃；其余有改动的情况保留目录并记录 `workspace.view_preserved`（含 work digest）。

### 子 Agent 的 Profile 约束

新子 Agent 持久保存完整 Profile 和指纹，恢复时使用出生时的配置。模型、指令、工具白名单、只读模式、迭代数、继承上下文、工具/Token 预算和运行截止时间在实际执行边界生效；嵌套 Flow 继承相同的工具管线。工具权限逐级取交集，不能通过孙 Agent 重新扩大授权。`Tools=nil` 表示交给宿主策略，显式空列表表示不允许工具；`MaxChildren=0` 表示不能创建子 Agent。

工具调用和输入/输出 Token 预算按子 Agent 的整个 session 累计；成功调用回放不重复扣工具次数。Token 准入依赖本地估算，收到 provider usage 后核销，并非供应商账单的精确硬上限。内置 Profile 的 `MaxWallTime` 默认为 `0`。正数设置的截止时间从子 Agent 创建时计算，继续任务沿用原截止时间。模型迭代上限按每个 Eino turn 生效。配置了继承 Token 上限时，用接收方模型的 tokenizer 截断背景内容。

上下文压缩和 Task verification 也经过模型请求记账；未返回 usage 或已发出但失败的请求保留输入估算和输出额度，标记 `estimated_reservation`，不能当作实际费用。当前内置 Profile 使用 chat 配置的模型；额外模型别名需宿主显式提供 `Config.ResolveModel`，未配置会报错，不会静默使用父模型。子 Agent 的 Profile 保存在 ATE；缺少快照时拒绝执行恢复，不从当前配置猜测原配置。

Task verification、模型请求派发和回执直接保存在 ATE。Supervisor 取得根会话执行锁后，只对自己拥有的 Team 对账：确认未派发才退还预留；已有回执按回执结算；已派发但结果未知、或无法证明未派发的记录，保留预留消耗并标记 `estimated_recovery`。不按请求年龄直接退款，也不能在活跃请求期间运行这项恢复。配置了价格表时预留费用一起准入；未知价格仍不能推导账单。已退还的请求 ID 也不复用。

Responses 流以有效的 `response.completed` 为成功边界，收到后不等待 HTTP 断开；提前 EOF、孤立 `[DONE]`、错误或不完整响应均不会当成正常完成。模型输出额度会传到 `max_output_tokens`。相关恢复路径和限制见架构导读。

示例 `$GSEA_HOME/config.toml`：

```toml
data_directory = "/Users/me/.gsea/data" # 省略时默认为 $GSEA_HOME 本身
workspace_directory = "/Users/me/src/my-project"

[model]
provider = "openai"
wire_api = "responses"
name = "gpt-5.6-sol"
base_url = "https://2xa.cc.cd/v1"
api_key_environment = "XAPI_API_KEY"
reasoning_effort = "medium"
timeout = "2m"
# 可选的流式超时覆盖，按阶段计时
# response_header_timeout = "2m" # 等响应头，默认沿用 timeout
# stream_idle_timeout = "5m" # 等响应体新字节
# stream_event_idle_timeout = "5m" # Responses 持续收到字节但没有有效事件
# 可选：私有 OpenAI-compatible 模型的 tokenizer 编码覆盖，例如 cl100k_base
# tokenizer_encoding = "cl100k_base"

[agent]
max_iterations = 20
max_output_tokens = 0 # 0 或省略：发现容量后自动预留（最多 8192）
context_window_tokens = 0 # 0 或省略：自动识别模型窗口
compaction_trigger_tokens = 0 # 0 或省略：自动计算压缩阈值

[agents]
max_running = 4 # 同时运行的子 Agent 上限；chat --max-running-agents 可覆盖
max_live = 16
max_depth = 3
max_children = 4
default_workspace = "auto" # auto（有 snapshot 时用 snapshot）/ snapshot / readonly / shared
snapshot_copy_limit = "1GiB"
auto_merge = false # true 时无冲突的 snapshot 合并计划与 git-patch 直接应用，不再请求审批
auto_continue = true # TUI 中子 Agent 消息自动唤醒空闲 root
post_merge_check = "" # 每次合并应用后在主工作区只读沙箱中运行的检查命令，如 "go test ./..."；失败时关联 Task 失败，变更不回滚
post_merge_check_timeout = "10m"

[usage.prices."openai/gpt-5"] # 键为 provider/model；未配置价格时成本记为 0
input_micros_per_million = 0
cached_input_micros_per_million = 0
output_micros_per_million = 0
reasoning_micros_per_million = 0

[usage.budget] # 会话 Team 的预算，每次启动或恢复会话时重写；全部为 0 表示不限
max_input_tokens = 0
max_output_tokens = 0
max_total_tokens = 0
max_cost_micros = 0

[lsp]
enabled = true
command = "gopls"
args = ["serve"]
diagnostic_timeout = "3s"
max_diagnostics_per_file = 5
max_diagnostics_total = 20
```

聊天的流式计时按单次模型请求分段，长任务可跨越这些时长。可用 `--response-header-timeout`、`--stream-idle-timeout` 和 `--stream-event-idle-timeout` 分别覆盖，也可用同名大写 `GSEA_*` 环境变量。显式传入 `--timeout` 会影响非流式请求、流式响应头和字节空闲，专用 CLI 参数优先。模型请求超时后，会话会从原 turn 自动重试一次并复用已确认的工具结果；再遇超时则暂停。见[执行观察与排障](./docs/EXECUTION_OBSERVATION.md)。

上下文窗口默认自动选择：兼容网关 `/models/{id}` 声明的容量优先，其次是内置官方模型目录；无法识别时使用明确标记为 `fallback` 的 32768-token 保护预算，不代表已知模型容量。Ollama 仅从 `/api/ps` 读取已加载模型的实际 `context_length`，不把理论架构窗口当作运行时部署容量，也不套用同名云模型数据。发现请求有 2 秒上限，不发生成探测。标准 OpenAI Models API 不保证提供容量信息，模型别名也不能证明第三方部署的实际容量，因此不能把成功请求的 input tokens 当作最大窗口。

用户仍可用配置文件、配置 Profile、环境变量 `GSEA_CONTEXT_WINDOW_TOKENS` / `GSEA_COMPACTION_TRIGGER_TOKENS` 或同名 CLI 参数设定工作预算；允许覆盖可能过时的目录或 fallback 默认值，但不能超过服务端已明确声明的硬窗口。旧配置中的 `32768` 仍是显式预算；改为 `0` 启用自动模式。窗口和最大输出分别发现、分别标明来源，`max_tokens` 不会被误认为窗口。输出预留自动取 `min(8192, 窗口/4)`，并受已知最大输出约束；显式正数也不能超过已知最大输出。总工作窗口始终保留完整含义，输入预算只扣除一次输出预留与安全余量。

模型容量与工作上下文阈值分别管理：自动阈值取窗口的 80%、200000 tokens，以及扣除输出额度与安全余量后的输入预算三者最小值。这是本项目的默认工作策略，不表示 Codex/Claude Code 使用相同常数。输入预算（input budget）限制单次请求的上下文容量；Team 预算（team budget）限制累计 token/费用消费。Context Policy 只使用窗口、阈值、当前 token 占用和手动强制标记，不读取 Team 累计用量或费用，也不会因 Team 消费达到 80% 而在半窗口时提前压缩；手动压缩的保留量同样不受 Team 余额影响。每次模型请求前都会检查，包括同一轮内连续调用工具：完整输入（系统指令、历史、本次输入、工具 Schema/结果）加输出预留和安全余量不能超过总工作窗口；先外置大工具结果，再压缩较早历史，保留当前请求、系统指令、执行契约和完整工具调用/结果组。超大的单条输入或工具定义无法压缩时会在派发前报错。回复结束后重新本地计数，并随 turn 完成原子记录 `compaction_pending` / `over_budget`；不在空闲时额外调用模型，下一次请求前才真正处理。

新摘要使用版本化结构：任务、约束、完成事项、修改、验证及结果、未完成事项、下一步和引用。模型输出经过格式校验，最多一次有界格式修复；仍无效时明确标记抽取式 fallback，不虚构已完成或验证成功。摘要若反而增大上下文则不提交；取消、额度耗尽和记账/存储失败不能被降级吞掉。执行契约由宿主单独固定，不依赖模型摘要。辅助摘要和修复请求自身也受完整输入、输出预留和安全余量的容量约束，不带工具、不会递归压缩，继续通过同一账本预留并按 usage 或失败预留核销。Team/Agent 硬配额、Task contract 和 verification policy 继续生效；预算不足时明确停止，不能用抽取式 fallback 绕过。仅对 `purpose=compaction` 的摘要及修复，Profile 累计输出余额不足计划输出额度时返回 `ProfileConstraintError`，恰好足额时正常执行，不会静默缩短摘要输出。摘要输出最多 2048 tokens、工具结果超过 24 KiB 外置的默认值保持不变。结束后的计数与持久化历史投影一致，临时 reasoning 不作为下一 turn 的输入重复计数，但 reasoning 的生成消耗继续记账。旧摘要、事件和 v0/v1/v2 历史 checkpoint 保持兼容，原始证据不改写。

状态栏 `ctx input 已用 / 输入预算` 的分母不是总窗口，也不是压缩阈值；越过阈值会显示 pending。`/context` 分别显示输入预算、工作窗口、发现的模型容量及来源、最大输出、输出预留、压缩线和状态；`/compress` 可以手动提前压缩。缓存输入仍占上下文空间，session 累计消耗不等于当前窗口占用。压缩只改变模型上下文视图，原始事件和完整工具产物保留。

默认使用 Ollama：

```bash
export GSEA_MODEL_PROVIDER=ollama
export GSEA_MODEL=qwen3:8b
export OLLAMA_HOST=http://127.0.0.1:11434
go run ./cmd/gsea chat
```

OpenAI Responses API（包括支持 `/v1/responses` 的中转服务）：

```bash
export GSEA_MODEL_PROVIDER=openai
export GSEA_MODEL_WIRE_API=responses
export GSEA_MODEL=gpt-5.6-sol
export GSEA_API_KEY_ENV=XAPI_API_KEY
export GSEA_MODEL_BASE_URL=https://2xa.cc.cd/v1
export GSEA_REASONING_EFFORT=medium
go run ./cmd/gsea chat
```

也可以使用 `--wire-api`、`--base-url`、`--api-key-env`、`--reasoning-effort`、`--tokenizer-encoding`、`--profile`、`--max-running-agents` 和 Agent 参数覆盖配置。Responses 模式由 `internal/modelgateway` 将 SSE 文本、reasoning summary 与函数调用增量转换为 Eino 的 `schema.Message` 流，并由同一套工具循环执行。

当前每条输入都从一个普通 Turn 开始。简单问答、只读查询和解释在当前 Turn 内完成；需要范围、验收条件、预算、截止时间或多 Agent 协作的工作，由宿主创建或绑定 Query Plan、Task、Execution 和 Step。ExecutionContract 由宿主固定并保存，模型只能提出行动，不能改变授权范围、工具权限、预算或验收策略。

Task 的 `Contract` 承载 objective、scope、criteria、constraints、verification、deadline、attempt 和 token/cost budget。Task verification 使用独立的验证路径，Workspace snapshot/merge 负责交付边界，Tool Pipeline 继续负责参数校验、Policy、Approval、写入/执行/secret 控制。模型请求、工具结果、checkpoint、unknown outcome 和恢复事实仍由 ATE 持久化；缺少原身份或证据的旧执行记录会 fail closed。

`--timeout` 默认 `2m`。非流式请求使用单次总时限。流式请求等待 HTTP 响应头或读取时连续没有数据达到该时限会暂停，持续有数据的流可以运行更久。Agent 还会在连续四次重复相同工具调用和结果的周期时停止当前 Turn，最多比较长度为三轮的循环。

TTY 环境中的 `chat` 直接启动基于 Bubble Tea 的完整终端 UI，并使用独立的备用屏幕缓冲区隔离 shell 输出：输入框处于 raw terminal 模式，支持多行编辑、历史召回、命令补全、鼠标和 PageUp/PageDown 滚动、transcript 内鼠标拖动复制、实时 reasoning/assistant delta、工具调用状态、Markdown 与多语言代码高亮、窗口缩放和上下文 token 状态栏。状态栏显示 `ctx used / window status`，其中 `exact` 表示本地模型 tokenizer 可用，`provider` 表示已用服务端 `input_tokens` 校准，`estimated`/`unavailable` 表示当前只能估算；当 OpenAI Responses、OpenAI Chat Completions 或 DeepSeek-compatible 响应返回缓存用量时，还会显示缓存 token 数和命中率。非 TTY 输出使用 `cached=` 与 `cache_hit=` 展示同一份 provider usage。上下文达到配置阈值会自动压缩，也可以使用 `/compress` 手动触发同一套持久化压缩策略。使用 `Ctrl+G` 或 `/agents`（`/activity` 为别名）可展开/收起 Team 活动面板，查看 Agent、Task、Workspace View、Lease 和 Merge 状态；面板内 `↑`/`↓` 选择、`Enter`/`o` 打开子 Agent transcript、连按 `x x` 中断、`r` 重新请求合并审批、连按 `d d` 丢弃候选、`Esc` 返回；`/merge` 可将当前已加载的冲突及 resolution task 摘要复制到 transcript；`/diff` 打开当前记录的 Diff 视图。滚动区域是 Agent 自己维护的 transcript；程序接管终端之前由 Bash 输出的历史属于 shell，应用无法重新读取，退出 TUI 后主屏和 shell 历史会恢复。`Enter` 提交，`Ctrl+J` 插入换行；运行中 `Ctrl+C` 取消当前 turn，空闲时先清空输入、再次按下退出；空输入时 `Ctrl+D` 退出。内置命令为 `/help`、`/session`、`/recoveries`、`/resume [turn-id]`、`/clear`、`/compress`、`/context`、`/reasoning`、`/info`、`/question`、`/agents`（别名 `/activity`）、`/merge`（别名 `/merges`）、`/diff`（别名 `/diffs`）和 `/exit`（别名 `/quit`）。

聊天记录把相邻的已结束工具调用合并为一行，显示调用次数、工具名和失败数。点击该行可查看这组调用，再点某次调用可查看参数、结果和 Diff。单次调用直接打开详情。`Ctrl+O`（或 `Alt+O`）可查看当前 Agent 的工具记录，再按一次回到聊天。详情内用滚轮或方向键浏览，点击上方的 `‹ Back` 或按 `Esc` 逐级返回。需要检查历史中断时，可输入 `/recoveries` 查看详情。

顶部默认显示一行会话标题，点击标题栏、按 `Alt+I` 或输入 `/info` 可展开/收起 model、workspace、session 信息。已命名的 managed Session 沿用名称，普通会话从最早真实提问提取短标题，不额外调用模型；恢复时读取原始事件中的首问，避免标题随上下文压缩改变。浏览长回答时，原问题滚出视口后，顶部导航栏显示它的简短预览；点击该栏、按 `Alt+P` 或输入 `/question` 可跳回提问。会话标题固定，问题导航随阅读位置变化；跳转不改动输入草稿。

选中的子 Agent 正在运行时，TUI 随 Team 活动刷新读取其已保存的输出和工具事件。切换只影响当前查看的执行记录，原有执行和输入草稿会保留，新提问仍发给主 Agent。待审批请求会将视图切回主会话，方便查看审批卡片。

终端内容在进入 viewport 前按显示列宽进行 ANSI 安全换行，中文长段落和超长词不会被横向静默裁掉；流结束时用完整最终消息校准已有流式正文，避免保留不完整的 delta。`assistant.message` 事件另保存供应商提供的 `finish_reason`，回放也保留该值；没有供应商元信息时保持未知，不凭标点猜测模型是否截断。

本地 BPE 编码通过 `tiktoken-go` 读取缓存，不会在模型请求期间隐式联网；需要精确本地计数时，可预先将对应编码文件放入 `TIKTOKEN_CACHE_DIR` 指定的缓存目录。未公开 tokenizer 的中转模型不会被错误映射到其他编码，首次请求使用显式估算，收到 provider 的真实 `input_tokens` 后自动校准并持久化到 session。

stdin 或 stdout 不是 TTY 时不会启动 TUI，而是使用稳定的逐行 headless 对话协议，适合管道和自动化；`--prompt-file` 配合 `--headless-format json` 提供单请求的 `gsea.chat.result.v1` 契约。该模式默认 fail closed；只有同时显式指定 `--evaluation-mode` 和非空 `--workspace` 时，才会使用精确工具 allowlist 自动批准工作区写入、编辑、sandbox shell 和子 Agent snapshot 合并（`merge_apply`）。评测 harness 还可成对指定 `--tool-bridge-manifest`/`--tool-bridge-socket` 注入本次调用的工具，这些工具固定为 execute-risk、逐名加入 Policy 与 allowlist，且调用仍完整记录在 Tool Pipeline；bridge 激活时使用独占 mutation allowlist，workspace write/edit/shell 不再获批，避免 harness 内容通过 shell 读取模型进程环境。两个参数不能在普通模式使用。`--evaluation-mode` 会拒绝在 TTY 中运行，也不会批准 `human_confirm` 或其他未列出的高风险工具；bridge 模式下连 `merge_apply` 也不批准。文件系统根、用户主目录及其上级目录不能作为 evaluation workspace，应只指向可丢弃的 benchmark worktree/container。需要观察或控制 durable Team 时使用 `gsea control` 的 `gsea.command.v1` JSONL 输入；响应均为 `gsea.control.v1` JSONL，事件带全局 `cursor`，客户端可以从旧 cursor 补读。原始隐藏思维链不会被请求、持久化或展示；界面只处理服务端明确返回的 reasoning summary。

隔离评测调用示例：

```bash
printf '%s\n' 'Fix the issue described by the benchmark task.' '/exit' \
  | go run ./cmd/gsea chat \
      --evaluation-mode \
      --workspace /tmp/gsea-eval/task-worktree \
      --data-dir /tmp/gsea-eval/task-data
```

统一评测入口接受 JSONL task manifest；完整字段、不同语言和 benchmark 适配方式见 [`evals/README.md`](./evals/README.md)：

```bash
go run ./cmd/gsea eval doctor

go run ./cmd/gsea eval validate --tasks evals/examples/tasks.jsonl

go run ./cmd/gsea eval run \
  --tasks /path/to/tasks.jsonl \
  --root /tmp/gsea-eval/run-1 \
  --output /tmp/gsea-eval/run-1/results.jsonl \
  --run-id run-1 \
  --model-label gpt-5.6-sol \
  --parallel 4 \
  -- \
  --provider openai \
  --wire-api responses \
  --model gpt-5.6-sol \

go run ./cmd/gsea eval export-swebench \
  --results /tmp/gsea-eval/run-1/results.jsonl \
  --output /tmp/gsea-eval/run-1/swebench-predictions.jsonl

go run ./cmd/gsea eval summarize \
  --results /tmp/gsea-eval/run-1/results.jsonl \
  --results /tmp/gsea-eval/run-2/results.jsonl

go run ./cmd/gsea eval score \
  --benchmark terminal-bench-2.1 \
  --input "$PWD/.local-agent/evals/harbor-jobs/gsea-tb21-official" \
  --reasoning-effort high
```

`eval run` 拥有并覆盖 `--evaluation-mode`、`--workspace`、`--data-dir` 和 `--session`，这些参数不能出现在 `--` 后的 chat 参数中。每个任务使用独立 worktree/fixture copy、数据目录和 session；默认完成后删除工作区，`--keep-worktrees` 仅用于调试。任务的 `checks` 不进入 Agent request，且至少要有一个非 optional check。Verifier 默认走与 `shell_exec` 相同的 OS sandbox，也可在预缓存镜像中使用 `backend: "docker"`；Docker 网络固定为 `none`。只有 Agent 正常完成、patch 捕获成功、verifier 正常返回且所有 required checks 通过时才记为 resolved；超时或失败后留下的 patch 仍保留用于分析。结果内含 patch 与 SHA-256，详细 transcript、stderr、check output 和本地运行资源保留在 run directory；Agent 执行事件和用量账本保存在所选配置的 ATE 中。

控制示例（查询命令直接读取 ATE，可以在 chat 运行时使用；修改命令取得目标会话和本地运行库的所有权锁，活动实例持锁时会拒绝修改。未配置 `data_directory` 时默认数据目录为 `$GSEA_HOME`，即 `~/.gsea`）：

```bash
printf '%s\n' '{"command":"snapshot"}' \
  | go run ./cmd/gsea control --data-dir ~/.gsea --workspace .
printf '%s\n' '{"command":"events","after":0,"limit":100}' \
  | go run ./cmd/gsea control --data-dir ~/.gsea --workspace .
printf '%s\n' '{"command":"team.cancel","team_id":"<team-id>","reason":"stop","timeout_ms":30000}' \
  | go run ./cmd/gsea control --data-dir ~/.gsea --workspace .
```

可用控制命令包括 `snapshot`、`agent.list`、`task.list`、`workspace.list`、`merge.list`、`events`、`agent.cancel`、`team.cancel`、`task.transition`、`merge.run`、`merge.requeue` 和 `wait`。修改命令必须提供对应的 Team/Agent/Task/merge ID；控制端点不会替代工具管线中的模型审批。`events` 可带 `team_id` 过滤，末尾的 `result` 行给出 `count` 与 `next_cursor`，下一页把 `next_cursor` 作为 `after`。`agent.cancel` 会递归中断后代；`team.cancel` 中断 Team 的全部子 Agent，等待其结束后把 Team 置为 `canceled`，此后拒绝新 spawn，直到该会话再次打开（root 会话本身保留）。超时后 Team 停在 `canceling`，重复执行即可完成。

`merge.run` 的调用本身不构成合并授权；它和后台合并共用 Approver/AutoMerge 门禁。合并检查以持久回执绑定候选/计划、主工作区内容及检查配置摘要，恢复不会重复运行结果未知的外部检查，unknown 使 Task 失败并阻塞依赖。检查只读、在整个 workspace 屏障内运行，失败不回滚。`team.cancel` 当前只管理子 Agent 及新的准入，尚不取消已在等待审批的后台合并；完整限制见 [审计记录](AUDIT_REMEDIATION.md)。

API Key 的值不会作为 CLI 参数传递或写入 Session Event。`--api-key-env` 和 `api_key_environment` 只用于指定保存密钥的环境变量名称。

启动时会打印 Session ID。加载同一段对话历史（不会自动执行根会话中断任务）：

```bash
go run ./cmd/gsea chat --session chat-20260827T120000Z-0123456789abcdef
```

运行 `gsea resume`，选中会话后即可查看历史。直接输入新问题会在该会话创建一个 Turn。模型会收到已完成的对话，以及当前未完成工作的原始请求、最后记录的执行阶段和有界的工具结果摘录。直接输入「继续」或补充条件即可让模型结合这些记录接着做。界面也会显示中断时留下的部分回复，模型使用的是已确认的记录。

输入 `/resume` 可从当前中断的前台 Turn 续跑，沿用原请求和已确认的工具结果。没有明确的前台 Turn 时，可用 `/recoveries` 查看原始请求和 Turn ID，再用 `/resume <turn-id>` 选择。普通聊天输入会按原文发送。宿主程序已经变化的 Turn 需要原来的 GSEA 可执行文件才能原地恢复。核对先前的执行结果后，也可以直接在当前会话描述下一步。

```bash
go run ./cmd/gsea chat --session <session-id> --list-recoveries
go run ./cmd/gsea chat --session <session-id> --resume-turn <turn-id>
```

`--list-recoveries` 输出 JSON 列表，不创建模型客户端、不要求 API Key，也不启动 Supervisor；它使用 ATE 事实和持有所有权锁的本地运行库检查恢复条件，不自动导入旧会话数据库。执行恢复时需使用原来的模型、执行配置和兼容的宿主程序/工具版本。TTY 下恢复进入正常审批界面；非 TTY 恢复沿用默认拒绝审批的策略，并可加 `--headless-format json` 输出结果。恢复参数不能与新 prompt 或 evaluation mode 混用。

打开会话时，TUI 只提示中断项数量，`/recoveries` 展示详细原因。列表中的 `execution` 表示有 checkpoint 或 Turn journal 待恢复；这些状态只标识阶段，执行前仍会验证兼容性。`unavailable` 会附上阻止恢复的原因，例如 checkpoint 和 journal 都缺失，或旧执行缺少持久化身份绑定。旧子任务没有 `turn.started.run_id` 的记录不能自动绑定原 Run，保守拒绝。恢复沿用原 Turn 已记录的输入。较早的 Turn 在恢复后完成时，宿主把结果追加到当前会话上下文。恢复不会接收/确认新到的 mailbox 消息，这些消息留待后续显式输入；也不会自动合并、删除工作区或扩大权限。

确定不再恢复某个根任务时，先退出使用同一数据目录的运行实例，人工检查之前的文件修改、命令和外部副作用，再执行（将示例 ID 替换为真实值，使用原 `--data-dir` / profile）：

```bash
go run ./cmd/gsea chat --session SESSION_ID \
  --abandon-turn TURN_ID \
  --abandon-reason "已核对副作用，决定停止恢复" \
  --confirm-abandon
```

此操作只放弃指定 Turn 的恢复，不撤销已经发生的工具副作用，也不会替代 Task、Agent、Flow 或 merge 的生命周期管理。必须提供原因并显式确认，不能与恢复、新 prompt 或 evaluation mode 混用。命令输出 JSON 回执；相同目标和原因重复执行返回原回执，不重复释放。已完成的 Turn 不通过此入口清理。

ATE 保存终止恢复的决定。宿主先提交关闭准备记录和原生审计事实，确认权威回执完成后，再释放本地快照引用并删除目标 Turn 的 checkpoint。本地清理失败可按同一请求继续。事件、Task 结果、用量和工作区保留。存在非终态子 Agent、团队 Task 或本会话 Flow 时拒绝操作，子会话需走 Supervisor 生命周期。确认外部状态后，可以显式发起新的工作。

开发时也可运行短测试。

```bash
go test -short ./...
```

依赖版本固定在 `go.mod`，包括 OpenAI provider v0.1.13 与 Ollama provider v0.1.9；可复现构建中不使用浮动的 `@latest`。

### Go 1.27 与 Sonic

`go.mod` 当前使用 Eino v0.9.22，间接依赖 Sonic v1.15.4。该版本的原生编码/解码快路径构建约束覆盖 amd64/arm64 上的 Go 1.27（`!go1.28`），不再回退到标准 `encoding/json`；升级到 Go 1.28 或其他架构时需重新核对 Sonic 的构建约束。
