# GSEA / ATE 完整会话交接

> 面向没有任何前文的新会话。先读本文，再按阅读路线打开引用文档。
>
> 时间范围：2026-11-22 至本机日志标记的 2026-11-23。会话开始时环境日期为 11 月 22 日，后续测试、构建和诊断日志已进入 11 月 23 日。
>
> 本文是工作状态快照，不是全部完成声明：**ATE 权威存储与 GSEA 长期 stdio 连接已实现；模型流读取超时仍未确定最终根因；长连接改造引入 control/resume/catalog 访问回归，最近整库测试仍失败。**

## 1. 新会话首先掌握的结论

1. 用户在开发本地编程 Agent 宿主 **Go-Self-Evolving-Agent（GSEA）**，同时独立设计 **Agent Temporal Engine（ATE）**，提供时间轨迹、历史证据、恢复和跨任务记忆基础设施。
2. **GSEA 执行和决策，ATE 保存权威事实。** ATE 不运行 Agent Loop，不替代模型、工具、Supervisor、工作区合并或验收决策。
3. GSEA 的业务事实已经从 SQLite 切到 ATE。`runtime-v3.db` 只保存本地执行资源与未完成请求；不要恢复双写、业务历史镜像或启动/退出补送历史。
4. 生产接入是 **native stdio**，不是 MCP，不需要 prompt 激活。用户要求 GSEA 启动后 ATE 持续在线。
5. 当前 App 持有 persistent ATE client lease，普通仓储操作复用同一进程和连接。但其他入口争用独占 owner 的问题尚未解决。
6. Memory 的查看、采用和维护由主 Agent 自主决定；禁止通过自然语言关键词路由把“记忆数量”硬编码成特殊路径。
7. unknown outcome 不等于没执行。保留原 RequestKey、原请求字节、digest、Store ID 和执行身份；不能换 ID 盲重试或重复外部副作用。
8. 最新故障是会话 `chat-20261122T173750Z-318da7f81593d473` 的 `model stream read timed out after 2m0s`，发生于模型 HTTP 响应体读取阶段。
9. 最近 GSEA 全库测试有 5 个 CLI / Session Registry 测试失败。**这是功能回归，不能解释为“旧测试无效”而忽略或删掉。**
10. 用户曾授权一次清空测试数据，已经执行。随后用户重新运行并产生新数据；**不能沿用旧授权再次清库、停止进程或重放请求。** 最新请求是写交接文档。

## 2. 项目目标与核心概念

### 2.1 用户真正想做的事情

GSEA 不只是一次性回答工具。目标是让 Agent 完成长程工作，按需协作、验收、交付、从中断恢复，并从真实历史中形成可复用经验。

例如用户要求“修改模块并验证”：主 Agent 理解目标，必要时派生子 Agent；子 Agent 在隔离工作区修改和测试；宿主冻结结果、验收并合并。中途重启时，系统应知道哪些模型调用已完成、哪些工具执行过、哪些结果未知，决定继续执行还是仅补结算。另一个项目随后可以查看该经历形成的 Memory，并判断是否采用。

- GSEA 决定怎么做、谁来做、允许哪些工具、怎样验收与交付。
- ATE 保存发生了什么、决策看到了什么、知识来自哪里、提交在哪个位置、结果是否可核验。
- 时间旅行和语义 replay 不意味着自动重跑工具或撤销文件、Shell、网络副作用。

### 2.2 概念边界

| 概念                        | 当前含义                                        | 不应混淆为                        |
| --------------------------- | ----------------------------------------------- | --------------------------------- |
| Session                     | 原始交互和执行事实的有序历史                    | stdio 连接、当前模型上下文        |
| Turn                        | 固定工具快照的一次模型—工具循环，可以含多轮请求 | 一次 HTTP 请求                    |
| GSEA 编排 Task              | Team 中有依赖、claim、验收和交付的工作单元      | ATE Task 的同一状态机             |
| ExecutionContract           | 不可变的目标、范围、约束、验收、预算和时限      | 旧 Goal Mode、每轮必经 Query Plan |
| Agent / Agent Run           | 持久参与者及一次被接纳的运行身份                | goroutine、Turn ID、Flow Run      |
| ATE Task / Execution / Step | 宿主无关的执行语义协议                          | ATE 自动调度 GSEA                 |
| Memory                      | 有来源、验证和生命周期的可复用声明              | 全部聊天消息或任意候选            |
| ExperiencePath              | 有来源和步骤关系的经验路径                      | 自动运行的工作流                  |
| Space                       | 状态/授权命名空间及语义组织目录                 | 固定知识分类枚举                  |
| RequestKey / Receipt        | 确切请求的稳定身份和提交证据                    | 外部效果恰好执行一次的证明        |

`turn.completed`、Task 验收通过、工作区交付完成是三个不同边界。GSEA 的 Agent profile 是执行配置，ATE `agent/v1` profile 是持久化协议，两者也不能混用。

### 2.3 已接受的架构原则

- 默认入口是主 Agent Loop；Eino 承载 Turn 内循环，宿主拥有权限、调度和业务状态机。
- Session、任务、消息、用量事实、业务回执、执行证据、知识与记忆由 ATE 权威保存。
- checkpoint、工具快照、插件产物、LSP projection、artifact、工作区文件/租约、未完成 ATE 请求属于本地执行资源。
- Workspace 的共享协调数据库管理租约与进程存活，不是第二份 Task 仓库。
- 原生 Session append 与 authority state batch 是独立提交，不是跨域大事务。
- 文件、模型供应商、Shell 和 ATE 不共享端到端 exactly-once 保证。
- 已尝试请求查无回执仍可能 unknown；ATE 当前没有覆盖所有拒绝的耐久回执协议。
- TUI `Ready` 应意味着基础设施可用；首次 query 不应反复初始化进程或做无关功能扫描。
- 用户不要求为假想旧用户保留永久兼容架构；但格式简化不是删除真实数据的许可。

## 3. 工作区与协作约束

| 路径                                              | 角色                                                   |
| ------------------------------------------------- | ------------------------------------------------------ |
| `/Users/zhaozhiqiang/work/Go-Self-Evolving-Agent` | GSEA 主项目、运行与诊断入口、本文所在仓库              |
| `/Users/zhaozhiqiang/work/agent-temporal-engine`  | ATE 独立引擎、协议、client、历史与 Memory 能力         |
| `/Users/zhaozhiqiang/work/deepseek-harness`       | 同时挂载的第三仓库；本轮未在此实现，不纳入当前改动范围 |

本文编写时只读核实：GSEA 分支 `main`，HEAD `b01b19f initial commit`；ATE 分支 `main`，HEAD `b158c61 refactor(ate): complete temporal engine restructuring and validation`。两者都有大量修改、删除和未跟踪文件。旧记忆中“GSEA 无 git”已失效。

**主要成果在工作树，不能只阅读 HEAD。** `git diff` 不显示未跟踪文件正文，新的 `internal/ateprocess`、多个 adapter 和文档需要直接读取。仅携带 commit ID 无法迁移当前成果。

用户协作偏好与限制：

- 中文交流、设计文档；英文代码注释。解释先讲场景和职责，再讲内部机制。
- 已明确要求实现就落实并验证，不反复问“是否继续”；诊断请求不等于授权改架构、清库或停进程。
- 优先统一概念和所有权，完成替代后删除过时 adapter，不长期保留两套架构。
- 大幅整理类型时，遵循将类型集中在 const 后 `type (...)` 块的项目偏好。
- 不创建 Git commit，不用 reset/checkout 覆盖工作树，保留无关修改。
- 并行代理不要编辑同一文件。本轮发生过实现代理与主线程同时改文件导致上下文漂移，后续应明确单一编辑者。

本轮核查未发现两主仓库内的 `AGENTS.md` / `CLAUDE.md`；新会话仍应检查是否新增。

## 4. 文档阅读路线：实现、设计与历史要分开

### 4.1 当前架构的首选入口

| 文档                                                              | 用途与注意事项                                                 |
| ----------------------------------------------------------------- | -------------------------------------------------------------- |
| [GSEA 架构导读](../ARCHITECTURE.md)                               | 当前执行主线、领域边界、原子性与恢复；不是测试报告             |
| [代码阅读指南](../CODE_READING_GUIDE.md)                          | 按工作流定位实现                                               |
| [ATE 原生接入](ATE_INTEGRATION.md)                                | stdio、Memory、启动、恢复；历史性能数字不是当前端到端指标      |
| [ATE 权威迁移](ATE_AUTHORITY_CUTOVER.md)                          | 事实迁移与 exact request 恢复；11 月 20 日全绿不覆盖之后的改动 |
| [ATE 架构与概念边界](../../agent-temporal-engine/ARCHITECTURE.md) | Session、执行协议、Knowledge、时间与统一发布                   |

本文在 `GSEA/docs/`，到 sibling ATE 的相对路径是 `../../agent-temporal-engine/`。新机器保留 sibling 布局或调整路径。

### 4.2 用户提到的执行设计文档

[Query Plan、多 Agent 与 ATE 时序执行设计](QUERY_PLAN_MULTI_AGENT_TEMPORAL_DESIGN.md) 是理解 Query、长程异步、多任务并行、Memory、分支、恢复与交付方向的重要材料。

**它第 3 行明确标注“设计提案，不是完整实现清单”。** 后文“所有 Query 先编译为 GoalContract/QueryPlan”“直接工具完成”等不能当成当前行为或默认下一步任务。

当前以 `ARCHITECTURE.md` 和 `internal/execution` 为准：

- 已移除旧 GSEA Goal 控制模式和独立 Goal Loop。
- 统一 ExecutionContract、绑定、验证、交付与恢复领域仍然存在。
- ATE Task 的 `Goal` / goal epoch 是独立协议概念，并未一并删除。
- 普通输入先交给主 Agent；不强制每轮规划编译、经验检索或路由模型。

### 4.3 按需深入及过期材料

| 文档                                                                                      | 阅读用途                                                 |
| ----------------------------------------------------------------------------------------- | -------------------------------------------------------- |
| [GSEA README](../README.md)                                                               | 构建、配置、CLI；活动 chat 时并发 control 查询当前有回归 |
| [多 Agent 架构](../MULTI_AGENT_ARCHITECTURE.md)                                           | Supervisor、Task DAG、Workspace、路线图                  |
| [剩余工作](../IMPLEMENTATION_REMAINING.md)                                                | 插件/策略 R1–R8 与资源缺口；状态日期早于本文             |
| [审计修复](../AUDIT_REMEDIATION.md)                                                       | 权限、持久化与恢复边界                                   |
| [自进化能力](../EVOLUTION.md)                                                             | 构建、验证、发布与资源限制                               |
| [ATE 中文 README](../../agent-temporal-engine/README.zh-CN.md)                            | 引擎概览                                                 |
| [ATE API](../../agent-temporal-engine/docs/ATE_API_REFERENCE.md)                          | 公共契约                                                 |
| [ATE 传输](../../agent-temporal-engine/docs/ATE_TRANSPORTS.md)                            | stdio/Unix 与生命周期                                    |
| [ATE 原始设计](../../agent-temporal-engine/docs/AGENT_TEMPORAL_ENGINE_DESIGN.md)          | 时间轨迹目标                                             |
| [ATE 执行 Profile](../../agent-temporal-engine/docs/ATE_EXECUTION_PROFILE_WALKTHROUGH.md) | 执行协议场景                                             |
| [ATE 实现状态](../../agent-temporal-engine/docs/ATE_IMPLEMENTATION_STATUS.zh-CN.md)       | 能力与证据限制                                           |
| [ATE 主计划](../../agent-temporal-engine/docs/ATE_REFACTOR_MASTER_PLAN.md)                | R 系列进展、延期及性能工作                               |
| [ATE 验证入口](../../agent-temporal-engine/docs/validation/VERIFY.md)                     | 验证方法                                                 |

[2026-11-13 旧交接](SESSION_HANDOFF_2026-11-13.md) 仅作历史背景，其 SQLite 事实仓库描述已过时。旧并行计划、早期长篇设计中的 `sqlitestore`、Goal 模式、旧 migration 同样不能覆盖当前源码。

证据冲突时：用户最新目标决定意图，当前源码决定行为，最近实际测试决定验证状态。旧“全绿”不能代表后来工作树；新文档也不能凭日期代替测试。本文编写仅做源码、文档和既有证据核查。

## 5. 当前实现与代码地图

### 5.1 主调用链

```text
CLI / TUI
  → App：配置、运行资源、ATE authority、Session 所有权
  → Conversation：串行输入、历史、执行绑定、恢复
  → Factory / Turn：冻结工具与策略，构造 Eino 循环
  → modelgateway：模型协议与流读取
  → tool.Pipeline：校验、权限、审批、记录与执行
  → Supervisor / Flow / Workspace：委派、协调、验收、交付

Session 历史 → session/atestore → ATE 原生 Session
编排/进化事实 → orchestration/atestore、evolutionstore → ateauthority → ATE state
本地执行资源 → runtimestore、artifact、workspace domain
```

| GSEA 路径                                                             | 职责                                                  |
| --------------------------------------------------------------------- | ----------------------------------------------------- |
| `internal/app/open.go`、`authority.go`、`close.go`                    | 装配、Store ID 绑定、资源生命周期                     |
| `internal/app/control.go`、`catalog.go`、`offline_authority.go`       | control、独立 catalog 与离线维护                      |
| `internal/ateprocess/manager.go`、`process.go`                        | persistent lease、串行 callback、stdio child、owner   |
| `internal/aterequest/lease.go`、`executor.go`                         | callback 生命周期、pending envelope、ACK/unknown 核验 |
| `internal/session/atestore/append.go`、`recovery.go`                  | Session 追加与原上传/追加计划恢复                     |
| `internal/ateauthority`、`internal/orchestration/atestore`            | 条件事务与编排业务状态机                              |
| `internal/execution`                                                  | Contract、Binding、生命周期、验证、交付、恢复领域     |
| `internal/conversation`、`internal/agentruntime`                      | Turn、模型工具循环、journal/checkpoint 恢复           |
| `internal/modelgateway/request_timing.go`                             | 请求头等待和流读取时限                                |
| `internal/modelgateway/responses_transport.go`、`responses_stream.go` | Responses HTTP 与 SSE                                 |
| `internal/app/memory_tools.go`、`internal/experience`                 | 主 Agent 的 Memory 能力                               |
| `internal/cli/interactive_startup.go`、`internal/tui/startup.go`      | 启动阶段、Ready、排队输入                             |
| `internal/agentmanager`、`internal/agentflow`                         | Supervisor、Task、Mailbox、WaitSet、Flow              |
| `internal/workspacecoord`、`internal/workspacedomain`                 | view、租约、snapshot/merge、实际资源                  |

ATE 对应入口为 `client`、`protocol`、`service`、`transport`、`engine`、`storage`、`agentprofile`、`memory`、`knowledge`、`spacecatalog`。GSEA 通过 client/API 接入，不把 engine 嵌入生产业务执行层。

### 5.2 持久 stdio 的实际状态

旧实现每个普通 `WithClient` callback 都做：

```text
获取 owner → 检查/初始化 store → 启动 ate → 打开/恢复 store
→ hello → callback → 关闭 client/进程 → 释放 owner
```

当前 App：

```text
App Open
  → Manager.OpenClient / OpenLease
  → owner、初始化、启动、握手和能力校验
  → 保留 ClientLease
  → 多次有界、串行的仓储 callback
  → App Close：等待使用者，关闭 client，等待 child，释放 owner
```

当前 API 包括 `Manager.OpenClient/OpenLease/CurrentLease/WithFreshClient`、`ClientLease.WithClient/WithClientContext/Invalidate/Close`；ATE client 新增 `Closed()`。

`Manager.WithClient` 在已有 persistent lease 时复用，没有时仍保留短生命周期路径。不能说所有 Manager 使用场景都已变成长连接。

`authorityProcess` 动态读取当前 lease，避免显式重连后仍拿旧指针。普通操作仍检查 Snapshot/Store ID。**消除反复启动不等于所有冗余读取、恢复扫描、后台 polling 和锁排队都已优化。**

`WithStorageBatch` 复用 App 的连接，不是跨仓库事务；不可跨模型/网络/用户等待或长期 worker 持有 batch context。Supervisor 同步恢复使用 batch context，长期运行使用独立 RunContext，monitor/worker admission 在启动恢复完成后放行。

### 5.3 未知结果和恢复的不变量

1. 保存确切命令及尝试状态后提交；unknown 时保留原字节、身份和 Store ID。
2. 普通业务 callback 不因断连而整体自动重跑。
3. fresh recovery 等旧 callback 退出，关闭旧 lease/child、释放 owner，再建立新连接并核对身份。
4. 只核验原请求；原计划里从未尝试且依赖已确认的后续步骤才可能继续。
5. 已尝试请求查无 ACK 不证明没到达；不自动换身份或重发。
6. batch 内不能关闭外层 lease 再等待自身 callback，会自等待死锁。

第 6 点使用 `aterequest.DeferReconciliation/ReconciliationDeferred`：batch 内正常启动恢复复用有效连接，unknown append 的 fresh reconciliation 延后到 batch 退出。显式 fresh API 在 batch scope 内被拒绝。

本轮已修过旧 lease 指针、batch 内 fresh recovery 死锁、正常 Supervisor 恢复被误拒绝、unknown 退出后的连接失效等问题。后续仍应验证并发 fresh reconnect、关闭与重连竞争、callback 捕获外层 context 时的超时传播；这些是验证方向，不能直接写成已复现故障。

### 5.4 Memory 与自进化的默认行为

- 主模型按共享指令决定何时调用 `memory_query` / `memory_manage`。
- `memory_query` 有 `count/search/read/directory`，查询不自动采用。
- `memory_manage` 有候选写入、撤回、纠正、显式采用，写操作有宿主身份和审批边界。
- `/memory`、`@memory:` 是显式浏览/引用入口，历史材料不成为可信系统指令。
- 没有自然语言 phrase router，没有每轮必调的辅助 Experience 模型。
- 普通无显式记忆引用的 query 不会仅因文字提到记忆就被宿主前置检索。
- CLI 普通成功 Turn 默认不自动执行 `Experience.Complete`；嵌入宿主显式启用的自动经验服务是另一条配置路径。
- 工具结果通常回到主模型再生成答复，不能把工具成功等同于最终回答完成。

插件与策略已有受控构建、验证、发布、回退基础。当前重点是稳定主链与恢复，不是无限制自治修改宿主或发布未经验证的工具。

## 6. 已完成工作与进度边界

| 工作                         | 当前状态                                        | 不能扩大为                   |
| ---------------------------- | ----------------------------------------------- | ---------------------------- |
| ATE Session/authority 迁移   | 已实现，早期有全库验收                          | 当前所有修改已全绿           |
| 统一执行领域、移除 Goal Mode | Contract/Binding/验证/交付/恢复已落地           | Query Plan 提案全文实现      |
| Memory 主 Agent 决策         | 句式路由已移除                                  | 真实供应商请求均能完成       |
| TUI 启动阶段与 Ready         | 已接线，输入可排队                              | 窗口出现就已经 Ready         |
| Startup storage batch        | 已减少启动重复打开                              | 当时所有普通 Turn 已有长连接 |
| ATE 恢复期对象字节复用       | 已实现并测量隔离副本                            | 跳过历史或权限校验           |
| App persistent stdio         | 已实现，核心/定向竞态通过                       | control/catalog 并发访问正确 |
| 原请求恢复                   | 字节和身份保留、显式重连、batch deferral 已实现 | 所有 unknown 自动消除        |
| 一次测试数据清理             | 已执行                                          | 现在仍空库或以后可继续删     |
| 模型流读取超时               | 触发代码与语义已定位                            | 根因已证实或修复             |
| 多入口访问回归               | 全库稳定复现 5 个失败                           | 旧测试可以忽略               |

历史启动对比在隔离存储、不调用模型的条件下测得：

- 使用相同优化后 ATE，unbatched startup 约 **28.17 秒 / 19 次启动**；batch 后约 **1.40 秒 / 1 次启动**。
- 包括 `app.Open` 后分别约 **30.97 秒** 和 **4.15 秒**。
- 约 1,638 笔事务、20 MB 数据的 ATE 单次打开/恢复/关闭中位耗时约 **2.04 → 1.42 秒**。

这些是特定批次的启动测量，不是 persistent 改造后的端到端输入到模型派发指标。用户真正关心的是 Ready 后前置等待和请求发出后无回答，不能用启动优化掩盖后者。

## 7. 当前故障 A：模型 Responses 流读取超时

### 7.1 最新现场与已知配置

```text
workspace: ~/work/Go-Self-Evolving-Agent
session: chat-20261122T173750Z-318da7f81593d473
model: openai/gpt-5.6-sol
input: 查看一下你当前的记忆数量
error:
execution paused (transport):
read Responses stream: model stream read timed out after 2m0s
```

截图中的 `ctx input 6.0k / 1037k estimated` 是估算展示，不能证明 token 超限。

最近只读检查的配置为：Responses、中转 `https://2xa.cc.cd/v1`、`gpt-5.6-sol`、`reasoning_effort = "xhigh"`，凭据通过环境变量名 `XAPI_API_KEY` 获取，未读取或保存 key。默认模型时限 2 分钟，环境变量/CLI 可覆盖；错误本身确认该请求的读等待限制为 2 分钟。

### 7.2 已证实与尚未证实

已证实：

- 错误来自 `request_timing.go` 的 `modelDeadline`。
- 流请求先限制等待响应头，之后每次阻塞 `Body.Read` 单独计时。
- `stream read` 是模型 HTTP 响应体阶段，不是 ATE stdio，也不是整轮总时长。
- 连续有数据的流可超过 2 分钟，SSE 心跳字节也算活动。
- `responses_stream.go` 包装 scanner 错误，runtime 将其分类为 `transport` 并暂停。
- 本地 gateway 超时/流读取测试通过，真实供应商请求没有因此得到修复证明。
- 最近只读诊断时 GSEA PID 17581 与 ATE child PID 17589 均在运行，启动时间相差约一秒。PID 只作历史快照。

尚未证实：

- 最新失败发生于首轮模型调用，还是工具执行后的第二轮。
- 最新 `memory_query` 是否执行过。
- 首字节/最后收包时间、最后一个 SSE event、请求 ID。
- 是上游推理/排队、中转缓冲/断转发、网络停顿还是本地边界问题。

当前 ATE 被活动 GSEA 独占，另开 `OpenControl` 会争锁。前一轮未停止用户进程，因此未读取这次完整事件时间线。不能把旧会话证据套用到最新现场。

此前不同真实模型批次结果不一致：有成功完成问候和记忆计数的批次，也有 `memory_query` 本身约 0.1 秒成功、之后模型流等待 2 分钟超时的批次。这只说明工具与最终答复是两段链路，不证明最新故障阶段。

### 7.3 后续诊断建议，尚未实施

为每个请求关联原 Session/Turn/模型轮次，并记录有界、不含正文或凭据的诊断：

- 开始、响应头、首字节、最后字节、完成/超时的时间。
- 已读字节数、最后 SSE 类型、是否见到语义完成事件。
- 白名单中转/供应商请求 ID、status、Content-Type。

另有两个待验证方向：计时器 `Stop()` 与回调并发的边界；成功 HTTP 响应缺少 SSE Content-Type 校验。这些未作为当前根因证实，也未在本轮修复。

提高 timeout 只延长等待；降低 reasoning effort 可作对照，不是根因修复。没有相应授权时，不改用户配置或重发可能计费的请求。

## 8. 当前故障 B：persistent owner 引起访问回归

最近失败测试：

```text
internal/cli:
  TestControlScopeProductionJSONL
  TestResumeDiscoversAndAdoptsNativeHistory
  TestSessionReconcileCLIOwnedAndSafeErrors

internal/sessionregistry:
  TestNativeCatalogListShowReopenAndRemove
  TestNativeCLICatalogAndDiscovery
```

control snapshot、`resume --list`、独占作用域诊断、catalog 辅助访问会等待约 30 秒超时。

根本冲突是：persistent lease 长期持有 ATE directory owner，其他入口仍通过另一 Manager/App 打开相同目录。ATE 自身也持有底层独占 owner，移除 GSEA 外层锁不能正确解决问题。

**既有跨进程需求，也有同进程多个 App/catalog/helper 争用。** `openCatalogAuthority` 也调用 `openAuthority`，因此独立 registry 现在同样可能长期持锁。不能把范围缩成“两个聊天进程同时写”。

之前报告曾错误称这些为“旧短连接测试假设”，已向用户纠正为真实回归。禁止通过删测试、削弱活动实例查询要求、降低所有权校验或频繁关主连接来伪装修复。

后续需明确唯一 store/child owner、控制/目录访问通道、只读与执行权分离、崩溃与重连清理。可以评估宿主控制通道等本地共享访问机制，但尚未选定方案；不要自行将生产接入换成库模式或 MCP。

## 9. 验证结果与证据

### 9.1 最近执行结果

| 检查                             | 结果           | 范围                                             |
| -------------------------------- | -------------- | ------------------------------------------------ |
| `internal/ateprocess`            | 通过           | persistent reuse、fresh reconnect 等             |
| `internal/app` 整包              | 通过           | 单独约 115 秒；最后整库中约 183 秒               |
| `internal/session/atestore` 整包 | 通过           | 单独约 32 秒；最后整库中约 48 秒                 |
| focused race                     | 通过           | lease/batch/部分恢复；不是全库 race              |
| `go vet ./...`                   | 通过           | 当时工作树                                       |
| `git diff --check`               | 通过           | 已跟踪 diff，不代表全部未跟踪文件已审查          |
| `make build`                     | 通过           | 配套 `bin/ate`、`bin/gsea` 已生成                |
| 最后 GSEA `go test ./...`        | **失败**       | 上节 5 个 CLI / sessionregistry 测试；其余包通过 |
| 最新 gateway 流读取测试          | 通过           | 本地受控测试，不是供应商验收                     |
| 当前真实供应商端到端             | **未通过验收** | 用户仍报告流读取超时                             |

ATE 较早阶段有全库/vet/race/构建通过记录，但本文没有重新执行 ATE 全库，也不把旧绿灯挪用到当前工作树。本文编写只核查文档、源码和既有结果。

### 9.2 本机证据文件

共同目录：`/Users/zhaozhiqiang/.comate-engine/store/terminals/`。这些文件不随仓库迁移，本节摘录应足够让新会话了解实际状态；若需要完整输出，应另外携带。

| 文件            | 内容                                                          |
| --------------- | ------------------------------------------------------------- |
| `eurqqy.output` | 最后全库，2026-11-23 00:52:38 完成，exit 1，5 个访问回归      |
| `6w1upo.output` | App 整包 exit 0，115.371 秒                                   |
| `dwkm67.output` | Session store 整包 exit 0，32.056 秒                          |
| `xc3evw.output` | lease/batch focused 与 race exit 0                            |
| `ze3mso.output` | 最后静态检查 exit 0                                           |
| `3nymdg.output` | 最后配套构建 exit 0                                           |
| `mxre2m.output` | 2026-11-23 11:03:49，本地 gateway timeout/stream tests exit 0 |

`kbs7cb`、`dw6ye5`、`6fkq4w`、`ptylmw` 等更早输出属于实现中的失败、死锁排查，随后已修补。不要把延迟通知当新回归，也不要用中间通过结果覆盖最后全库失败。

### 9.3 新会话复验入口

以下命令在 GSEA 根目录运行，是复验建议，不是本文新执行结果：

```sh
go test ./internal/ateprocess ./internal/aterequest -count=1
go test ./internal/app ./internal/session/atestore -count=1
go test ./internal/cli -run 'TestControlScopeProductionJSONL|TestResumeDiscoversAndAdoptsNativeHistory|TestSessionReconcileCLIOwnedAndSafeErrors' -count=1
go test ./internal/sessionregistry -run 'TestNativeCatalogListShowReopenAndRemove|TestNativeCLICatalogAndDiscovery' -count=1
go test ./internal/modelgateway -run 'Test.*Timing|Test.*Timeout|Test.*Stream' -count=1 -timeout=45s
go test -race ./internal/ateprocess ./internal/app -run 'TestPersistentLease|TestFreshClient|TestStorageBatch' -count=1
go test ./...
go vet ./...
git diff --check
make build
```

`Makefile` 通过本地 sibling ATE module 构建配套二进制。默认 `"ate"` 优先解析为 GSEA 可执行文件旁的 `ate`，再查 PATH。`go run` 使用临时 executable 目录，应明确配套 ATE 路径，防止验证旧 binary。

opt-in 现有存储测试不是纯内存检查：先读 `internal/app/storage_timing_test.go`、`internal/cli/startup_timing_test.go` 的环境开关、目标和 ownership 行为。不要对活动 store 反复开第二个进程争锁，不要复制正在写入的日志后便宣称得到一致副本。

## 10. 数据、配置与现场的时间顺序

### 10.1 一次清理已经执行

用户原话：“那你倒是改啊，改完记得清空一下现在的 gsea 和 ate 中的数据，我从零测试。”

此前正常停止当时 GSEA PID 72437、确认退出后，清理了：

- `/Users/zhaozhiqiang/.gsea/runtime-v3.db`，对应 `-shm`、`-wal`、`.owner.lock`
- `/Users/zhaozhiqiang/.gsea/temporal`
- `/Users/zhaozhiqiang/.gsea/temporal.owner.lock`
- `/Users/zhaozhiqiang/.gsea/.gsea-runtime-owners`

当时验证这些路径不存在，配置保留。该操作直接删除，本轮没有建立可恢复备份。源码、工作区、旧版数据库、历史附件及其他未列出的目录保留。

**没有清空整个 `~/.gsea`。** 当时还保留 `authority-runtime-v3.db`、旧 `authority-runtime*` / `runtime*` / `sessions.db` / `control.db`、`sessions/`、`artifacts/`、`views/`、`workspaces/` 等。

特别是 `authority-runtime-v3.db` 仍由当前独立 catalog 用作 pending 运行库，不能统称为弃用旧库。ATE 重建 Store ID 后，其原绑定是否影响 catalog 需要核对；这是额外待核实点，不是已证明的本次模型超时原因。不能直接删库或改写绑定绕过 fence。

### 10.2 清理之后已有新运行

用户随后启动新 GSEA 并提供最新超时会话。最近诊断中 GSEA/ATE 都在运行，因此“数据仍为空”“当前无运行进程”已不是有效现状。

新会话必须重新检查 PID、父子关系、二进制和有效目录。不要对历史 PID 直接发信号。只读诊断也避免输出整个进程环境，防止暴露 key。

### 10.3 当前授权边界

- 最新用户请求是交接文档，不包含再次清库、停止当前进程或调用真实模型。
- 一次清理授权已经消费，不延续到后来生成的新测试数据。
- 最新超时诊断只做了只读检查与本地测试，未修改模型配置。
- 不为拿到事件时间线擅自杀掉 GSEA。
- 后续可以先只读核实；实际修复按新会话中的用户要求推进。

## 11. 下一步建议与完成标准

### 11.1 优先级一：把超时变成可定位的现场

明确模型请求轮次、是否执行工具、首字节/后续字节停顿、最后事件和中转请求关联。先补有界且不含敏感内容的诊断，在授权下开展真实对照。

验收应有至少一份可关联的完整阶段时间线，并说明仍缺哪些上游证据。只增加 timeout、只看测试绿、只说“网络原因”都不算解决。

### 11.2 优先级二：修复多入口 authority 访问

保留 persistent stdio 主链，恢复 control/resume/catalog 的合理使用能力，同时保持 Session 独占执行、权限、Store ID 与 unknown 语义。

验收包括 5 个失败测试、真实不同进程访问、owner 崩溃/重连、无重复 child/孤儿/锁泄漏。仅有进程内共享对象不代表跨进程问题解决。

### 11.3 优先级三：首轮前置工作与生命周期补强

测量用户提交到真正 `inner.Stream` 派发的耗时，区分必要 durable recording、冗余读取、恢复扫描、后台 polling 与锁排队。检查 `RecoverSession` 等正常路径是否不必要地请求 fresh connection。

继续验证操作超时、callback unwind、Manager.Close、并发重连、child 异常退出、重新连接后的身份和能力校验。取消、unknown、已确认 ACK 后清理失败必须保留不同语义。

### 11.4 中长期范围

已有多 Agent、Task DAG、Mailbox、WaitSet、隔离工作区、snapshot 合并和验证基础；整体路线图未完成。剩余包括 durable approval、steer、Agent 独立预算、关键路径调度、worktree pool、Git 生命周期及统一 apply 协议、未知外部验证的显式恢复、资源硬限额、增量引用扫描与效果评测。

ATE 仍有全状态增量化及规模性能工作；部分条目只有测量基线。设备掉电、Linux 实机隔离、真实模型效果不能由本机单元测试推定。优先处理当前用户实际遇到的链路，不扩成整份未来设计稿的实施。

## 12. 历史任务编号与真实状态

| 历史任务                                     | 交接状态                                         |
| -------------------------------------------- | ------------------------------------------------ |
| #38–#43 执行架构、身份、验证/交付/恢复、清理 | 相关实现落地，不代表 Query Plan 提案全实现       |
| #44 Memory 主 Agent 决策                     | 默认路径已落实                                   |
| #45–#47 前置与启动优化                       | 已做阶段化、batch、恢复修补，不代表全部延迟解决  |
| #48 模型流读取超时                           | 未完成；最新会话是否为工具后轮次也待核实         |
| #49 前置工作核查                             | 已识别重复 ATE 生命周期等成本                    |
| #50 首轮批次                                 | 被长期连接方向取代，不重新执行过时独立方案       |
| #51 persistent ATE                           | 任务系统曾标完成；核心实现完成，集成回归仍须解决 |
| #52 验证与清理                               | 清理已执行，整库未通过                           |
| #53 本交接文档                               | 文档完成不改变代码故障状态                       |

编号可能不会迁移；根据实际未完成工作重建任务即可，不要求用户恢复旧任务系统。

## 13. 可直接粘贴给新会话的开场指令

```text
请先完整阅读：
/Users/zhaozhiqiang/work/Go-Self-Evolving-Agent/docs/SESSION_HANDOFF_2026-11-23.md

这是 GSEA / Agent Temporal Engine 项目的交接入口。按文中的路线阅读
当前 ARCHITECTURE、ATE_INTEGRATION 和 Query Plan 执行设计稿的状态说明。
不要把设计提案当成已实现行为，不沿用旧会话“全部完成”的说法。

先核实当前工作树和运行状态，然后用中文说明：
1. 两个项目的目标与职责边界；
2. 已实现主线与两个未解决问题；
3. 最近测试通过/失败的真实范围；
4. 下一步怎样取证和验证修复。

重点问题：
- chat-20261122T173750Z-318da7f81593d473 的 Responses 流读取超时，
  尚未确定具体根因和工具执行轮次；
- persistent stdio 导致 control/resume/catalog 争用 ATE owner，
  最近整库有 5 个相关失败，是真实回归。

保留 native stdio、ATE 权威事实、Memory 主 Agent 自主决策，
以及 unknown 请求按原身份核验等约束。
不要擅自删库、停止运行进程、重发模型/工具或创建 commit，
不要恢复 SQLite 业务双写、MCP 接入或自然语言句式路由。
先理解上下文，再按我在新会话中的具体要求开展工作。
```

新模型仅拿到本文也能理解目标、状态和未完成工作；若要继续实现，还需能够访问两仓库的当前工作树与引用文档。仅迁移已提交代码会丢失当前大量成果。
