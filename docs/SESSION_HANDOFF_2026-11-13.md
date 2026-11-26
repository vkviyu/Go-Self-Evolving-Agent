# GSEA 与 ATE 会话交接

整理日期为 2026-11-13。来源会话为「设计本地 Agent 架构」，Codex chat ID 为 `01a03fb6-8e99-78c1-88dd-28ad52e00a04`。已从最新消息回溯到会话起点，并以当前工作区核对关键事实。本文汇总产品方向、已经确认的设计、代码现状和可复跑的验收入口。

本文记录交接当日的三个工作区。随后 GSEA 将会话与执行事实切换到 ATE。现行工具与跨任务经验的读写方式见 [ATE 接入说明](ATE_INTEGRATION.md) 和[跨任务记忆指南](../EVOLUTION.md)。

## 1. 新会话先读

用户正在开发 **GSEA**，一个本地、自进化、面向复杂长程任务的 TUI Coding Agent。GSEA 借助 Eino 运行模型工具循环，并自己负责工具权限、恢复、并行调度、工作区和 UI。

用户又从这个项目独立设计和开发了 **ATE**，一个可供其他 Agent 接入的确定性时间与执行记录引擎。ATE 面向时间旅行、可审计恢复、跨会话关键经验、上下文证据和多分支探索。交接时 ATE 已有独立的 Go 模块和持久层，GSEA 当时仍用自己的 SQLite 业务存储和上下文管线。DSH 已接入原始事件镜像和引用，原生 TaskTrace、跨执行 Knowledge 与局部分支当时仍待接入。

上一个实现任务已经在 ATE 中完成一轮代码和设计对齐，补了任务关键轨迹、原生分支隔离和发布、增量上下文及显式跨会话记忆采用。用户最新请求是整理并过渡到新会话。下一项工程工作由新会话中的用户请求决定。第 8 节列出从现状可接着推进的方向。

| 工作区                                                                    | 当前用途                                 | 交接时的 Git 基线                                 |
| ------------------------------------------------------------------------- | ---------------------------------------- | ------------------------------------------------- |
| [Go-Self-Evolving-Agent](/Users/zhaozhiqiang/work/Go-Self-Evolving-Agent) | GSEA 主产品，Eino、TUI、SQLite 和自进化  | `main`，`b01b19f` 加大量工作区变更                |
| [agent-temporal-engine](/Users/zhaozhiqiang/work/agent-temporal-engine)   | 独立 ATE 模块、Go 库、Unix 服务和验收    | `main`，`0417ac1` 加大量工作区变更                |
| [deepseek-harness](/Users/zhaozhiqiang/work/deepseek-harness)             | DSH 官方源码的本地改版，ATE 宿主验证用例 | `master`，`fcc0ad4621` 加本地桥接、Web 和文档变更 |

工作区实际状态包括已跟踪、未跟踪和删除的文件。先读对应仓库的 `git status --short --branch`，再定位代码。Git 提交哈希不能代表当前实现。GSEA 根目录中四个旧 ATE 文档显示删除，现行文档位于 ATE 仓库的 `docs/`。GSEA 曾被给过一个不存在的 `docs/design.md`，请使用实际设计和入口列表。

可以把下列文字粘到新会话中，并在其后补上要做的事。

> 请先完整阅读 `/Users/zhaozhiqiang/work/Go-Self-Evolving-Agent/docs/SESSION_HANDOFF_2026-11-13.md`，然后根据我的下一条任务继续。GSEA、ATE 和 DSH 是三个独立工作区，现有未提交文件包含最新成果。优先用实际代码和验收验证状态。若设计文档与实现不一致，以能正确达成目标的设计与实现为准，并同步证据与文档。

## 2. 产品决策与工作原则

- **GSEA 在本地跑 Eino。** 主循环使用 Eino `ChatModelAgent` 与 `Runner`。模型服务可以在远端，本地运行宿主。Task verification、恢复、调度与工具管线属于宿主层。重构时保留这条依赖方向。
- **复杂任务由根 Agent 动态推进。** 根模型可选择是否委派、创建几个子 Agent、如何细化和调整任务，宿主校验并发、深度、权限和状态。任务依赖按需建立，子 Agent 可以临时创建并形成分层树。用户更看重长任务恢复、可观察性和可核验的执行证据。
- **预算和上下文各有职责。** 压缩策略优先保证当前请求的效果与容量。Team 或 Agent 资源硬上限可让执行暂停，恢复沿原状态继续。网络或供应商请求仍有自身时限。全任务默认开放运行时长，并有重复工具周期检测。
- **ATE 是开源基础设施。** 独立 Go 模块，包可被其他项目引用，用户明确要求无 `internal/`，独立于 GSEA/Eino、DSH、模型供应商。Go 进程内库与本地二进制服务并重。首版通过同一个持久发布与回执模型支撑两种形态。双向通知、订阅、广播先留兼容方向，当前可使用轮询。
- **工程取舍围绕四个关联目标。** ATE 支撑跨任务与会话的关键因果链及可引用详细证据、精确上下文管理、多 Agent 分叉与汇聚，以及可独立接入的基础设施。保留对象职责与可验证的关系。文档冲突时以正确性和上述目标为准。可访问的历史事实、当前可以复用的经验、模型已经看到的输入和外部副作用各有证据。
- **尊重历史事实与当前授权。** 用户不希望引入无意义的预发布兼容包袱。对已形成的可审计记录，明确版本读取和拒绝策略。原始事件、回执、未确认的副作用、替代与撤回各有语义。清理和修剪须保持可追溯，避免把撤回、分支选择或历史归档误作对外操作已回滚。
- **验收依据。** 先完成约定代码边界，再交叉核验设计与代码，最后按需要重构。主流 Agent 和论文启发产品，但 GSEA/ATE 要用自身不变量和失败场景验证。问题报告要追到请求、事件、文件和实际执行证据。

## 3. GSEA 当前实现与续做入口

### 3.1 运行、仓库和工具边界

GSEA 模块为 `github.com/vkviyu/go-self-evolving-agent`，交接环境实测 Go 1.27.0、Darwin arm64，GVM 安装是此前用户指定的环境。仓库 `.go-version` 和 `go.mod` 都固定 1.27.0，Eino 为 v0.9.22。通常在仓库根目录运行下列命令。在 `cmd/gsea` 内可改用 `go run . chat`。历史命令 `go run cmd/gsea main.go chat` 会把 `cmd/gsea` 当包路径。

```sh
cd /Users/zhaozhiqiang/work/Go-Self-Evolving-Agent
go run ./cmd/gsea doctor
go run ./cmd/gsea chat
go run ./cmd/gsea resume --list
go run ./cmd/gsea resume --last
go run ./cmd/gsea resume <session-id>
```

远端 OpenAI-compatible 网关可指定 `--provider openai --wire-api responses --model <model> --base-url <url> --api-key-env <变量名>`。凭据继续通过环境变量提供。历史实例使用过 OpenAI Responses 和 `gpt-5.6-sol`。曾看到 TCP `i/o timeout` 或 `context deadline exceeded`，足以证明当次传输失败，代理、路由与服务端根因需要当时的连接证据。Go 1.27 下若出现 Sonic 回退提示，先区分 JSON fallback 与实际失败。历史 `internal/tokenusage` 的 IDE BrokenImport 所指目录当前存在，排查时从自带 `go.mod` 的根目录核对环境。

主链为 `cmd/gsea` → `internal/cli` → `internal/app` → `internal/conversation` → `internal/agentruntime/eino_turn.go` → Eino Adapter → `internal/tool`。工具管线固定 Turn 的 Registry Snapshot，校验 JSON Schema 和权限，处理审批并记录执行结果。按 session/turn/call 身份可以复用已确认结果。Flow 和子 Agent 共用这套边界。本地 shell 的隔离由 macOS Seatbelt 或 Linux bubblewrap 提供。[README](../README.md) · [架构](../ARCHITECTURE.md) · [代码阅读指南](../CODE_READING_GUIDE.md) · [中文设计](../自进化本地智能体（Self-Evolving%20Local%20Agent）.md)

`internal/agentflow` 提供宿主注册的命名 Flow、流式组合、有限并行、`agent_session` 和人工关卡，使用步骤血缘、拓扑指纹和 checkpoint 恢复。Flow 产生的工具和子 Agent 动作仍受调用方 Pipeline 约束。`local-agent control` 提供 JSONL 的 Team/Task/Agent/Merge 管理与事件分页。

### 3.2 Execution contract、上下文和长时运行

- **Execution contract。** 每条输入从普通 Eino Turn 开始；需要验收、预算、截止时间或协作的工作由 Query Plan、Task、Execution 和 Step 承载。宿主固定 ExecutionContract 和工具边界，缺少可靠身份绑定的旧执行标为不可继续。写权限、工具审批、Task verification 和 Workspace delivery 独立。
- **上下文容量与压缩。** 显式部署覆盖、可信 provider 元数据、内置模型目录和标记为 fallback 的 32768-token 安全预算共同解决容量识别。Ollama 的已加载容量取实际部署。状态栏 `ctx input` 分母是本请求的输入预算，`/context` 可区分工作窗口、输出预留、阈值、来源、估算和压缩状态。缓存输入仍占容量。触发线默认取窗口 80%、200k 和输入预算三者的最小值。每次 provider 请求前含同一 Turn 工具续步重新评估，输入、输出预留和安全余量同时计入。Turn 完结可持久化 `compaction_pending`，真正压缩留给下一次需要模型输入时。原始历史保留，工具调用与结果保持成组，大结果外置为 Artifact，执行契约单独固化。Team 累计支出不改压缩保留量和触发线。[容量解析](../internal/modelcontext) · [压缩决策](../internal/agentruntime/context_policy.go) · [请求与上下文策略](../README.md)
- **超时与死循环。** 流式默认 `--timeout 2m` 约束等待响应头或连续无数据，持续流可运行更久。ExecutionContract 或内置子 Agent 的截止时间配置为正值才固定。单条 shell 命令可传 `timeout_ms`。重复工具调用与结果的周期检测可停止当前 Turn。网络、凭据、限流、额度、预算故障按阶段保存恢复位置。[运行配置](../README.md)
- **大文档读取。** 曾出现高频 bash/sed 小段读取，无法凭现象确认当时模型为何绕过 `workspace_read` 或为何由主 Agent 阅读。现有接入会在第一条请求附上受到工作区及读策略约束的 `@路径` 信息，包括字节数，以及有界扫描后可得的 UTF-8 字符数、行数和 Markdown 标题行。模型自由选择行范围和读取量。读取默认 16 KiB，分段结果给 `next_start_line` 或 `next_offset`，完整 SHA 仅在满足完整读取条件时提供。并行拆文档应按章节或任务边界分工。实际读文件的次数还需用真实模型测量。[文件元信息](../internal/agentruntime/file_references.go) · [读取工具契约](../README.md)

### 3.3 多 Agent、恢复与 TUI

根模型可通过 `agent_spawn` 动态选择创建数量。生产缺省上限为并发子 Agent 4、活跃子 Agent 16、深度 3、每父 Agent 子节点 4，并受 Profile、模型并发桶及会话配置限制。Task DAG 支持 CRUD、有界依赖、claim/release/TTL、自动派发和 reviewer gate。`task_update` 在运行前按版本编辑，`task_refine` 原子新增成员及依赖。保守的失败改派仅覆盖宿主确认的 iteration-limit 及可证明安全的先前调用，每 Task 最多 3 次含首次。传输与预算类中断保留原 Run。父子 mailbox、有界执行、回执、任务完成及通知各有持久边界。默认可写子 Agent 使用私有 snapshot，版本化三方合并和 ApplyPlan 经过审批再写回，冲突可形成 Resolution Task。另有 git-worktree 补丁入队模式。

交接时，恢复会话和续跑未完成 Turn 已有独立入口。当时的 `control.db` 管理会话目录，`sessions.db` 保存事件与 checkpoint，曾发生目录和历史脱节导致的 `Session is not registered`。权威存储切换后，目录与历史从 ATE 读取，本地运行库保留 checkpoint 和工具快照。恢复只复用已确认的结果和原执行身份，未知副作用先核对。[恢复入口](../README.md)

TUI 可鼠标点 `main` 和子 Agent，`Ctrl+G` 或 `/agents` 展开面板，支持切换 transcript、检查 Task/Merge、手动中断。顶部标题由会话名或首个真实提问生成，`Alt+I` 展开 model/workspace/session，滚动时 `Alt+P` 或点击问题导航可回到问题。该会话曾报告长回答尾部疑似缺字、恢复时 ANSI/链接下划线、重复显示审批提示。终结结果与显示修正、ANSI 安全处理和单审批标签当时有相关覆盖。历史某次答案在哪一层截断仍需原 provider 或会话数据确认。[并行架构](../MULTI_AGENT_ARCHITECTURE.md) · [并行规划](../MULTI_AGENT_PARALLEL_PLAN.md) · [恢复与 TUI](../README.md)

Claude Code 早先修复 B.15–B.17，之后 A–E 继续核对和加固。有效基线是 `AUDIT_REMEDIATION.md` 逐项状态。尚有合并与团队生命周期细节、历史引用增量扫描、artifact 保留策略和宿主资源限制等事项。共享主目录按 owned paths 并行写入（P4）、关键路径优先和 Phase 5 仍有缺口。Team cancel 保留 root，会有取消收尾；合并后检查可运行宿主配置命令，失败保留合并结果并阻塞下游，已有持久身份绑定，完整 reviewer 复审及未知结果的用户处置仍受限。 [A–E 修复与限制](/Users/zhaozhiqiang/work/Go-Self-Evolving-Agent/AUDIT_REMEDIATION.md)

### 3.4 工具发现与跨任务经验

仍在使用的 Turn/Flow 工具快照记录工具清单与实现身份，以便恢复时核对。当时的 R1–R8 和 F1–F5 分别记录功能探索与架构重构。[阶段范围](../IMPLEMENTATION_REMAINING.md) · [重构记录](../REFACTORING.md)

后续任务遇到相似故障时，主 Agent 可用 `memory_query` 查阅 ATE 中当前可复用的排查经验，再按准确引用核对来源。`memory_manage` 经审批写候选、登记采用或纠正来源。普通 CLI 将是否查阅的选择留给主 Agent。[跨任务记忆指南](../EVOLUTION.md)

当时的渐进发现提案旨在控制工具 Schema 的上下文开销。三级披露依次给出组用途和工具名、组内简介、选中工具的完整定义。宿主固定 ID、版本和成员关系。分组只负责导航，执行仍经授权。提案还要求恢复时能还原当时的快照和披露状态。联网搜索、反爬与 Computer Use 当时暂缓。[渐进式工具发现设计](../TOOL_DISCOVERY_DESIGN.md)

### 3.5 评测历史

当时对照 openJiuwen 论文讨论了官方评分口径。GSEA 可导出 SWE-bench prediction，并从官方 SWE-bench 或 Harbor Terminal-Bench 2.1 报告计算分母、失败规则和分数。留存的单题、小样本、集成及烟雾结果包括一条官方 SWE 用例和个别 Terminal-Bench 用例。用户后来停测并要求删除大型下载，继续下载或付费跑分应以新的任务为依据。[评测索引](../evals/README.md) · [本地产物](../evals/results)

当时比对的论文报告了 SWE-bench Verified 500 题的 82.6%（Claude Opus 4.5 high），以及 Terminal-Bench 2.1 89 题的 87.19%（GPT-5.6 Sol）与 84.04%（Fable 5）。这些数字属于论文的配置和评测分母。

## 4. ATE 架构与对象分工

ATE 模块为 `github.com/vkviyu/agent-temporal-engine`。底层自实现确定性事件编码、内容寻址、不可变压缩 radix、分段日志、发布和历史读取。追加事务先由唯一协调器验证、排序和暂存，发布时保持操作、回执、控制与状态根一致。RootSet 分开持有领域状态、分支、活控制和执行知识。固定历史视图及语义重放共享可验证的发布边界。Go SDK 与 Unix Socket 同用同一内核；服务连接的认证主体、Space 范围和一次性读取许可约束输出。

| 对象                                          | 当前语义与关键关系                                                                                                                                                                                               |
| --------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Session、Query、QueryAnswer                   | Session 是宿主分组；Query 是用户请求，在 Space 与主体范围内定位。最终 QueryAnswer 绑定成功完整且已接受的模型输出、相关任务成果及实际最终输入证据。                                                               |
| Task、Execution、Step、TaskOutcome            | Task 有目标与验收要求；Execution 是一次执行，可有子 Execution 和独立 Task；Step 是本执行内有前置完成门槛的工作单元；Outcome 绑定产物和验证。根与子任务在运行时产生。                                             |
| Invocation、Activity、Attempt、ActivityResult | 工具逻辑槽、工作与输入、执行尝试、捕获结果分别记录。provider/工具操作可区分发送、捕获、接受、结算与对账。结果保留 full/truncated/partial/unknown 完整性；接受结果是显式转移。                                    |
| Work、Continuation、PendingAction             | Work 原子组合保护性转移；Continuation 固定可恢复位置；PendingAction 与 ActionIntent 保存下一次受约束的确切提交。断线优先用原 RequestKey 查询并复用回执。提交中断与外部工具副作用未知分开处理。                   |
| Event、Revision、Commit、Branch               | Event 是已发布事实，因果边来自已记录依赖。Revision 固定状态版本，Branch 是可 CAS 移动的修订引用，Commit 原子持久发布。时间顺序、因果和状态前驱可以给出不同的关系。                                               |
| SnapshotAnchor、Cursor、NodeRef、ViewRef      | Anchor 固定 Store、Space、发布 Fence 和可选修订/知识位置；Cursor 选具体修订；NodeRef 选实体、事件、状态版本或对象。ViewRef 是临时的授权读取句柄，重开用 Anchor。历史位置仍受当前访问和撤回约束。                 |
| Observation、Knowledge Revision               | 逐项记录某个 Actor/Execution 显式采用的材料，Revision 是采用前沿。拥有数据、看到事件、另一个 Agent 已读取内容各自有边界。跨执行采用可通过 Import 保留来源。                                                      |
| Summary、ContextPacket                        | Summary 是有来源和省略项的压缩产物；Packet 保存本次请求的材料、发布位置和确切模型请求字节。Activity 绑定 Packet。Packet 写入不推进知识前沿。                                                                     |
| Memory、ExperiencePath                        | Memory 承担可复用主张及来源、有效性和适用条件。ExperiencePath 串联支持该经验的 Memory、所锚定的步骤与省略项，以及正反证据。它可以被验证、推广、替代或撤回。                                                      |
| TaskTrace、Investigation                      | TaskTrace 是按 Query/Task 读取的里程碑与因果投影，支持宿主提交有来源的解释。Investigation 是带锚点的调查进展，用于记录已经查过的证据、推断与后续方向。模型或宿主提供语义判断，ATE 按事实、引用和授权保存或复核。 |

查询过去有两种典型用法。审计视图从固定时间点回顾授权范围内的证据及后续关系；执行知识视图按当时采用位置重建模型能看的部分。原始请求可逐字节重读，模型重新调用或外部世界恢复由宿主负责。分支可以固定并切换领域状态，也可以修剪导航引用，原审计证据继续保留。跨会话同主体可读取历史；跨 Space 则用授权 Transfer/Archive，目标域显式采用资料。 [总体设计](/Users/zhaozhiqiang/work/agent-temporal-engine/docs/AGENT_TEMPORAL_ENGINE_DESIGN.md)

单个工具调用可由 Invocation、Activity、Attempt 等实体和发布的事件解释。长期归档保留所提交的细节，模型入口可先看关键 TaskTrace，再选择节点与区间。进程内或 Unix 客户端用 `OpenView`、`ListNodes`、`GetNode`、`GetNodeContext`、`ReadContent` 分别处理固定位置、浏览、明细、前后文和有界原始字节。可选的调查 API 用 `UpdateInvestigation` 传入结构化发现、引用和位置，断线后 `GetInvestigation` 恢复，再用锚点继续查证。应用可以据此每次只交付当前需要的证据，模型侧仍需要宿主管理请求预算与进展摘要。 [调查 API 与示例](/Users/zhaozhiqiang/work/agent-temporal-engine/docs/ATE_API_REFERENCE.md)

## 5. ATE 最新实现、验收和未完成边界

### 5.1 最近闭合的三条主路径

1. **任务关键轨迹。** `ApplyTaskTrace` 以版本检查写入宿主精选节点和 `supports/refutes/explains`，证据为真实 NodeRef 与 Anchor。`GetTaskTrace` 按 Query 或 Task 子树返回重点事件和事实因果边，Query 可核对 Session。`GetTaskTraceEvidence` 展开因果区间或某条解释的证据。跨任务的直接因果来源可标 `Boundary`，包括共享状态基线。当前隐私撤回适用于固定历史、所附解释及已保存的证据依赖。Event Registry 2 增强 Query、Task、Step、活动、答复、消息、分支等可核验关系。旧注册表仍按原语义重放，旧日志没有记录的关系不会从时间先后推导。 [轨迹 API](/Users/zhaozhiqiang/work/agent-temporal-engine/docs/ATE_API_REFERENCE.md) · [代码](/Users/zhaozhiqiang/work/agent-temporal-engine/engine/task_trace_read.go)
2. **原生分支。** `ForkLocal` 固定基线与主体/可选执行，`WriteBranch` 仅推进目标分支，`JoinLocal` 对固定头做三方合并和冲突处理，`PublishBranch` 检查领域根及变更键，按差异发布并保留无关状态。`Prune` 记录修剪和对应分支位置。旧 `Join` 是整 Space 领域状态合并，使用时区分这两套契约。执行结果汇聚靠 `JoinExecutions`，知识采用靠 `Import`/Observation，状态合并靠 Graph。Git/worktree、文件系统与外部命令归宿主处理，具体分支发表若要成为最终答案来源，需有明确证据。 [分支 API](/Users/zhaozhiqiang/work/agent-temporal-engine/docs/ATE_API_REFERENCE.md) · [代码](/Users/zhaozhiqiang/work/agent-temporal-engine/engine/graph_branch.go)
3. **可延续的知识与模型上下文。** Observation、Packet 使用增量不可变记录和索引，旧读位置与回执可重放。派发前复核当前 Memory、ExperiencePath、Summary 及来源是否允许采用，防止 Claim 后源版本已被撤回或替代却继续派发。ExperiencePath 的选中与省略节点必须在历史锚点可解析，引用与所声明的证据匹配。新会话中的执行显式采纳既有记录，读取、导入与实际发送可独立审计。 [知识 API](/Users/zhaozhiqiang/work/agent-temporal-engine/docs/ATE_API_REFERENCE.md) · [上下文索引](/Users/zhaozhiqiang/work/agent-temporal-engine/engine/knowledge_index.go) · [路径证据](/Users/zhaozhiqiang/work/agent-temporal-engine/engine/memory_path.go)

### 5.2 文档地图与验证记录

四份设计是总体语义、实施架构、协议和执行场景。独立 API 手册提供公共具体调用，状态页和审计文件说明已实现范围和证据边界。

| 阅读目的       | 入口                                                                                                                                                                                                                                                                                                                    |
| -------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 总体语义与架构 | [设计规范](/Users/zhaozhiqiang/work/agent-temporal-engine/docs/AGENT_TEMPORAL_ENGINE_DESIGN.md) · [实施架构](/Users/zhaozhiqiang/work/agent-temporal-engine/docs/ATE_V1_IMPLEMENTATION_ARCHITECTURE.md)                                                                                                                 |
| 协议及场景     | [API 协议设计](/Users/zhaozhiqiang/work/agent-temporal-engine/docs/ATE_API_PROTOCOL_DESIGN.md) · [执行 Profile 场景](/Users/zhaozhiqiang/work/agent-temporal-engine/docs/ATE_EXECUTION_PROFILE_WALKTHROUGH.md)                                                                                                          |
| 直接接入       | [API Reference](/Users/zhaozhiqiang/work/agent-temporal-engine/docs/ATE_API_REFERENCE.md) · [格式注册表](/Users/zhaozhiqiang/work/agent-temporal-engine/formats/registry.json)                                                                                                                                          |
| 范围和风险     | [实现状态](/Users/zhaozhiqiang/work/agent-temporal-engine/docs/ATE_IMPLEMENTATION_STATUS.zh-CN.md) · [2026-11-06 四文档交叉审计](/Users/zhaozhiqiang/work/agent-temporal-engine/docs/validation/V1_CROSS_AUDIT_2026-11-06.md) · [原生平台范围](/Users/zhaozhiqiang/work/agent-temporal-engine/docs/PLATFORM_SUPPORT.md) |

上一实现轮实际通过 `GOPROXY=off go test -count=1 ./...`、`GOPROXY=off go vet ./...`、重点 `-race` 以及 `git diff --check`。嵌入式和 Unix Go SDK 跑通了同一个长任务，含子 Agent、崩溃后恢复执行授权、非幂等外部效果对账、验证结果、分支与保留反例、固定轨迹、AuditFull 物理压缩和重新打开。另覆核固定历史查读的撤回、混合事件注册表重放、跨会话记忆显式采用、超过旧 owner 知识快照容量及已派发输入重读。建议入口是 [长程宿主验收](/Users/zhaozhiqiang/work/agent-temporal-engine/tests/host/long_task_test.go)、[撤回回归](/Users/zhaozhiqiang/work/agent-temporal-engine/engine/task_trace_test.go)、[跨会话回归](/Users/zhaozhiqiang/work/agent-temporal-engine/engine/typed_import_recovery_test.go) 和 [知识规模回归](/Users/zhaozhiqiang/work/agent-temporal-engine/engine/knowledge_index_integration_test.go)。

**严格 V1 发布验收仍有独立证据门槛。** 当前代码主路径和针对性工程修正已经交叉核验；文档中保留物理设备断电、更广泛真实模型效果验证等证据缺口。此前有 Linux/arm64、真实 ENOSPC、guest 重启及受控模型试验，适用的是报告所记录的基线。一次新协议真实模型烟雾有 4 次请求完成，第二轮摘要被事实核对拒绝，随后没有最终配对得分。上一轮长程联合用例使用确定性模型桩。总体工程重构作为独立任务推进；短锁、读交付和 writer ownership 等已有定向重构。 [模型效果记录](/Users/zhaozhiqiang/work/agent-temporal-engine/docs/validation/V1_REAL_MODEL_EFFECTS_2026-11-03.md) · [交叉审计](/Users/zhaozhiqiang/work/agent-temporal-engine/docs/validation/V1_CROSS_AUDIT_2026-11-06.md)

**当前规模与格式。** TaskTrace 返回分页每页最多 128 节点，后台仍解码固定作用域，包含 65,536 对象 / 64 MiB 遍历约束，闭包另有预算。超大范围需要按需读取和增量索引。当前 Control 6、Packet 5、Summary 2 按各自协议拒绝不支持的预发布格式。事件 Registry 1 的历史重放和旧 Knowledge 记录的保留有各自的兼容约束及回归测试。判断某个文件能否恢复，以具体格式注册表和对应测试为准。

## 6. DSH 适配验证与边界

选择 DeepSeek Harness 官方源码作为真实 Agent 宿主，是因为 GSEA 当时正在处理自身运行时问题。用户明确要求**由 DSH 适配 ATE 公共能力**。通用引擎后续可以为多种宿主增加合理的通用语义，DSH 的记录格式、恢复策略和 UI 逻辑都由 DSH 持有。

本地 DSH 可选插件为 `@deepseek-ai/dsh-ate`，Go 桥接器用 sibling ATE 模块。插件监听持久 Session 事件并镜像到 ATE，含子会话、序号、父会话、事件类型、原始字节、摘要和提交回执。DSH 的源日志仍负责它自己的恢复。Web 执行轨迹面板按当前会话显示事件并刷新，支持点击节点和查看有界邻居。输入 `@事件` 或 `@event` 可检索并插入明确的跨会话引用。模型工具 `ate_event_search` 和 `ate_event_read` 支持以自然语言发问，模型将线索转成文字词项和时间条件，工具执行有范围的字面匹配。工作区归属、子 Agent 谱系、显式所有者附加证据及发送预算分别约束读取与注入。DSH 自带的会话引用与 ATE 事件引用是两条使用路径。

DSH 桥接当前把事件保存为通用状态，通过精确序号与有限邻居展开，ATE 原生 Agent/Event Profile 在这条镜像链关闭。原始事件可以保留 Query、Answer 和工具细节，但不自动变成 ATE 的 QueryAnswer、TaskOutcome、ContextPacket 或原生因果 DAG。由适配策略给出稳定身份、typed 投影和来源映射后，才能将新原生接口用于现有 DSH 功能。完整团队图、独立 SDK 子进程自动镜像和真实模型效果对照仍需验证。

历史讨论以 DSH Super Agent 作产品方向参考，目标是在同一个对话入口继续不同任务并取回合适的历史证据。当前先保留可确定定位的引用和有界导航。向量召回、提炼后的 Query/Answer 语义索引、设备能力检查与低配降级留待后续设计，现有关键字及时间检索无需嵌入模型。语义索引若实施，应把索引与原始证据身份、现行授权和模型输入预算分开。

界面有没有“执行轨迹”按钮取决于正在运行的 DSH 构建是否加载可选插件和其 Web 资源。启用方法见插件文档，常用输入是 `DSH_ATE_BRIDGE`、`DSH_ATE_STORE` 和可选配置 `integrations/ate/ate.cordis.yml`。当前交接复跑了桥接器 Go 测试并通过，未启动 Web 或重验用户正在运行的应用。 [DSH 插件中文说明](/Users/zhaozhiqiang/work/deepseek-harness/packages/experimental/ate/README.zh.md) · [桥接协议](/Users/zhaozhiqiang/work/deepseek-harness/integrations/ate/bridge/README.zh.md) · [Web 用例](/Users/zhaozhiqiang/work/deepseek-harness/apps/web/tests/ate-trace.e2e.ts)

## 7. 历史推进顺序

1. GSEA 从 Eino 本地 Agent、Responses、工具与权限、自进化、基准可比性起步。调试期间经历过 GVM、CLI 参数、代理传输、Goal 准入和审批 UI 问题。早期 Claude Code 与 Codex CLI（`codex-pro`）直接在同一迭代工作区写代码，用户提供过 CLI resume 会话标题，后续应把当前文件当事实来源。
2. GSEA 完成受控自进化与一轮大重构，随后继续处理并行 Agent 审计、TUI、上下文预算与长程恢复。动态计划和失败改派建立在稳定持久恢复之上，保守处理外部副作用。多 Agent 的真实执行可视化与大文档读取成为重点。
3. ATE 作为新基础设施单独建模块，围绕四份文档持续完成第一版主路径、代码交叉审计和阶段性工程修改。随后将 DSH 作为可视化与真实宿主试验，补上跨会话检索、`@事件` 和节点证据查询。
4. 用户再次以四项目标明确 ATE 的内部职责，最后一轮按对象语义、关键时间线、分支隔离和上下文恢复补强。当前交接是在这一轮代码验收通过后进行。

## 8. 下次继续时可从哪里着手

| 用户选择的方向     | 从当前基线开始的具体工作                                                                                                                                                                                                                                                                                                     |
| ------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| ATE 工程质量或规模 | 核对 TaskTrace 每页全范围读、事件元数据索引和因果遍历，先用真实规模夹具测量。在同一授权与固定 Anchor 下让查询按需加载，保持因果、撤回、分页、压缩重放一致。                                                                                                                                                                  |
| DSH 真实宿主验证   | 先确认当前运行配置与现有镜像行为，再在 DSH 设计可重放、可授权的 typed 投影。用一条经历压缩、并行和分支的任务验证从关键轨迹到详细引用的链路。条件允许再做同模型、同任务、预设评审条件的配对试验。                                                                                                                             |
| GSEA 使用 ATE      | 从独立适配层、稳定 ID 和证据来源开始，先做双写或只读对照。验证 Turn/Run 恢复、模型输入精确重建、未知副作用和授权后，再评估是否迁移业务读写。                                                                                                                                                                                 |
| 继续 GSEA 核心能力 | 按 [A–E 清单](/Users/zhaozhiqiang/work/Go-Self-Evolving-Agent/AUDIT_REMEDIATION.md) 及 [并行计划](/Users/zhaozhiqiang/work/Go-Self-Evolving-Agent/MULTI_AGENT_PARALLEL_PLAN.md) 定位剩余边界。另可在已确认的 [三级工具披露](/Users/zhaozhiqiang/work/Go-Self-Evolving-Agent/TOOL_DISCOVERY_DESIGN.md) 上继续独立实现和验证。 |

新会话可以先复跑相关最小验证，再按明确任务改动。交接资料区分已跑测试、代码/文档静态核对、仍缺运行证据三种状态，尤其注意 GSEA 与 DSH 的真实模型效果、系统级故障和 ATE 严格发布边界。
