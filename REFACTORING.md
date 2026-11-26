# 架构稳定性与可读性重构（历史记录）

本文保留 2026 年 9–10 月 F1–F5 的实施与验收记录，不是当前功能清单或安装指南。旧 SQL 业务库与迁移不再是现行事实存储。文中的包名、迁移版本和测试通过结论只适用于各自记录日期。当前实现和验证状态以 [架构导读](ARCHITECTURE.md)、[功能范围与剩余工作](IMPLEMENTATION_REMAINING.md) 及源码为准。

现行 Evolving 指 ATE 跨任务记忆，普通工具是 `memory_query` 和 `memory_manage`；后者仅有 `create_candidate`、`adopt`、`withdraw_source`、`correct_source`，不提供 `validate/promote`。有界 LoopSpec 策略比较独立保留。当前职责与入口见[架构导读](ARCHITECTURE.md)和[跨任务记忆指南](EVOLUTION.md)。

当时 R1–R8 的受控功能边界已交付，F1–F5 只重构实现，不借机改变权限、用户协议或产品范围。

## 不变量

- Eino 继续负责 Agent Loop；不增加第二套循环。
- 保留现有 CLI flags、工具名/schema/risk/审批、JSON 输出及错误的 fatal/可修复分类。
- 不改变 SQLite migration/checksum、持久身份、回放语义、CAS 发布及旧消费者关闭规则。
- 保持先预留再派发、取消后保存证据、进程组关闭和 App 持有运行时所有权的顺序。
- 结构调整会改变宿主二进制摘要；不伪造旧宿主兼容性，不改写已有快照。
- 测试只使用临时数据和模型替身，不调用付费模型、不下载 benchmark。

上述不变量约束的是本轮重构本身。重构完成后，多 Agent 并行工作（[MULTI_AGENT_PARALLEL_PLAN.md](MULTI_AGENT_PARALLEL_PLAN.md) P0–P3）做了以下追加变更：

- 追加了会话库迁移 v34–v36。
- `agent_spawn` 增加了可选的 `workspace` 参数（`auto`/`readonly`）。
- 合并审批增加了 `merge_apply`。

这些都是追加变更，没有改写已发布的迁移、删除工具名或改变原有 schema 字段。

## 实施顺序

- [x] F1：收敛 Composition Root。提取策略影子测试服务；使用窄依赖接口支持失败注入；按装配、生命周期、Agent 装配、控制面和只读访问拆分 `app.go`。
- [x] F2：收敛输入适配层。拆开工具描述/参数校验/可信调用者归属与业务服务，拆开维护命令解析和执行；保留现有外部协议。
- [x] F3：梳理运行时协议。以 Turn、Task verification、恢复和上下文管理为边界减少 Factory/Conversation 的交叉职责，保持副作用与恢复状态机不变。
- [x] F4：梳理持久层。按领域组织 store/迁移/事务辅助逻辑，收敛跨领域查询与校验边界；不重写已发布迁移或合并不相关事务。
- [x] F5：全局回归与架构约束。固化依赖方向、关闭顺序和契约测试，复核文档与重复抽象，完成最终全量/静态/竞态检查。

每一批必须有独立验收结果；不因一个文件变短就认为整轮架构重构完成。只有具体调用者确有替身/适配需要时才增加接口，不建立通用框架或 Service Locator。

## 当时完成状态

F1–F5 当时已全部交付并通过全量、静态与竞态验收。这不构成当前清理后工作区的验收结果；本次文档核对未重跑这些测试。

### F1 交付与验收（2026-09-26）

- `app.go` 保留组件清单和调用选项；装配、关闭、Factory、控制面、Profile 和资源访问分别落到 `open.go`、`close.go`、`agent_factory.go`、`orchestration.go`、`profiles.go`、`resources.go`。
- `strategy.ShadowService` 管理影子比较。串行工作准入归服务，进程取消/join/资源生命周期仍归 App。
- SQLite 持久端口和 Eino 比较器通过窄接口注入；运行时 `strategy.Store` 与宿主管理 `strategy.Catalog` 分开。App 在绑定闭包前分配稳定实例，不再整体覆盖含锁结构体。
- 新增无数据库依赖的服务测试，覆盖阶段顺序、参数传递、已完成回执重放、无效请求、故障与取消后证据、fatal 错误分类和准入等待取消。
- Go 1.27.0（darwin/arm64）、`GOPROXY=off`：当日全量测试、静态检查及定向竞态检查通过；没有付费模型请求或 benchmark 下载。

### F2 交付与验收（2026-09-26）

- `internal/evolutiontools` 接管策略工具的描述、解码、归属校验和结果编码，依赖窄的宿主服务端口；不依赖 App、SQLite 或 Eino。`app/evolution_scope.go` 保留可信 durable root/Factory 解析，影子工作仍经过 App 生命周期边界。
- `maintenance`、`strategy` 命令拆成选项解析和执行。保留配置优先级、原有校验顺序、确认条件、错误/JSON/退出码和模型创建时机。
- 增加工具的固定契约 fixture、真实 Pipeline 下的审批/归属/候选与报告选择/错误分类测试、宿主 root scope 测试，以及所有维护操作的解析/帮助/文件输入与无运行时副作用检查。
- Go 1.27.0（darwin/arm64）、`GOPROXY=off`：当日全量测试、静态检查及定向竞态检查通过；新增适配/选项/输入/root scope 测试在竞态模式重复 5 次通过。没有付费模型请求、benchmark 下载或迁移变更。

### F3 交付与验收（2026-09-28）

- Factory 的配置、不可变视图、Turn 准入/恢复、Eino 装配与上下文管理分开；保留现有公开方法与唯一 Eino Loop，不增加新的运行时容器。
- 上下文压力策略提取为无副作用的 `planContext`，大结果外置、摘要请求、transcript 处理、压缩 checkpoint 和管理事件按职责组织，保留阈值与持久格式。
- Conversation 保留 session 锁与状态所有权；分开输入路由、Inner Turn 执行、恢复、上下文操作与历史视图。统一历史刷新边界，失败不发布部分投影，不越过回合准入顺序。
- 实时 Task verification 与已提交结果的恢复共享统一执行结果路径，继续检查、终态提交和候选释放规则不再各自实现。原始执行 deadline 与终态持久化上下文保持区分。
- 新增上下文阈值、历史读失败、round 预留失败、完成后重开、实时/恢复 verdict 对照、过期终态和预算故障测试；所有模型调用使用本地替身。
- Go 1.27.0（darwin/arm64）、`GOPROXY=off`：这是旧版本的验证记录；当前清理后的状态必须重新运行 `go test ./...`、`go vet ./...` 和构建检查，不能沿用旧的 Goal/SQLite 结果。

### F4 交付与验收（2026-10-10）

- `store.go` 保留连接生命周期和兼容入口，迁移执行移到 `migrations.go`，历史 DDL 移到 `schema.go`。迁移账本有意保留原始全局顺序；跨领域历史版本不能按当前领域重新拆分。每版 DDL 与校验和登记仍原子提交。
- Orchestration 按 root、spawn、状态转换、查询及事件存储组织；Task 按创建/转换、领取/过期、依赖传播及查询组织。保留原有 Store API，子 Agent 与 task claim、Task 完成与 dependent unlock 不拆成多个事务。
- 查询辅助函数复用包内 `rowQuery` / `readQuery` / `rowScanner` 窄接口，事务内继续传递原 `tx`；Agent/Task 的列表与点查询共用列投影，避免 scanner 顺序漂移。没有增加通用事务框架或新的公共仓储接口。
- 归档计划计算从写事务入口中分离，跨领域生命周期、引用、依赖和回执辅助逻辑各有明确归属。归档和放弃恢复保留不同的校验规则、确认要求及错误语义，预览不回填身份。
- 当时固定重构前 1–33 迁移的 SHA-256 fixture，覆盖空库及每个已发布版本升级、历史事件/checkpoint/已应用账本保留、DDL 中段或登记失败回滚，以及校验和/缺号/未来版本拒绝。截至 2026-10-26，fixture 已追加到 v34；随后最高版本为 v36，v35、v36 尚未登记。旧迁移与 fixture 现已随业务存储切换退出生产路径，不再是当前升级指引。新增六类业务事务故障注入，比较全部持久行并验证重试/幂等；维护查询验证事务内可见性与预览无写入。
- 首次跨模块竞态回归发现既有 Flow 取消测试依赖固定 20ms 延迟，可能在准入前取消，却断言准入后的终态。改为在测试阶段实际输出后触发取消，另测准入前取消；不改 Flow 运行时语义。两个取消测试在竞态模式重复 20 次通过。
- Go 1.27.0（darwin/arm64）、`GOPROXY=off`：修正测试时序后，当日全量测试、静态检查及定向竞态检查通过。新增迁移/事务/只读预览测试在竞态模式重复 5 次通过。没有付费模型请求、benchmark 下载、用户数据库维护或已发布迁移改写。

### F5 交付与验收（2026-10-12）

- 增加 `internal/architecture` 生产源码依赖测试：领域端口、SQLite/SQL 驱动、Eino 与 Provider adapter 有明确归属，核心构造点按文件约束。扫描包含平台源码，识别别名和构造器函数值；测试替身/fixture 不受生产依赖规则限制。这是结构回归检查，不是安全隔离机制。
- CLI 评测用量读取不再自行打开 SQLite；由包级函数 `app.ReadSessionUsage(ctx, cfg, id)`（`internal/app/usage.go`）经统一离线所有权入口汇总指定 session 的整个 Team。放弃恢复复用同一入口，保持原有验证顺序、回执与关闭错误传播。测试覆盖活跃 owner 拒绝、root/child 汇总、跨 Team 隔离、读取失败释放锁及 CLI 字段映射。
- App 在自进化或 Supervisor worker 关闭超时后保留全部后续资源与所有权，不提前关闭仍可能使用的数据库。Supervisor 并发及重复 Close 返回同一结果。初始化失败也检查关闭超时，保留失败 Supervisor 并拒绝新工作，不能丢失仍存活的 worker owner。
- 新增关闭测试复现了取消收尾竞态：runner 可先于关闭流程登记 `canceling` 返回，直接提交 `canceled` 会被原状态机拒绝；并发登记还可能产生版本冲突。收尾现在先确保原有中间状态，冲突只接受同一 Run 的 `canceling` 版本，不重放 Task 效果、不覆盖其他 Run/终态。确定性冲突测试和并发关闭测试覆盖两种时序。
- 复核并更新 `ARCHITECTURE.md`、`README.md`，写清依赖检查、离线入口、正常关闭顺序、超时保留及既有影子测试 Pipeline 例外；没有增加通用生命周期框架、仓储容器或自动清理权限。
- Go 1.27.0（darwin/arm64）、`GOPROXY=off`：当日最终全量测试、静态检查及全仓竞态检查全部通过；全仓竞态检查未使用 `-short`。新增所有权、关闭、失败装配、取消冲突与依赖约束测试在竞态模式重复 5 次通过；没有付费模型请求、benchmark 下载、用户数据库维护或已发布迁移改写。

### F5 之后的结构变化（2026-10-26 复核）

以下是 2026-10-26 的结构快照与当日全量测试通过记录；当前依赖规则以 `internal/architecture` 为准，不能沿用下列旧 SQL owner 与构造点。

- 新增包：
  - `internal/sessionregistry`：独立的 Session 目录库，无领域依赖。
  - `internal/modelcontext`：无领域依赖，由 `agentruntime` 和 `modelgateway` 使用。
  - `internal/workspacedomain`：工作区协调库，只依赖 `internal/orchestration`；`workspacedomain.Open` 只能在 `internal/app/open.go` 中调用。
- SQL 驱动所有者：`database/sql` 和 `modernc.org/sqlite` 的所有者现在是 `sqlitestore`、`sessionregistry`、`workspacedomain` 三个包。每个库都设置 `SetMaxOpenConns(1)`。具体的 `sqlitestore` 包仍只能由 `internal/app` 导入。
- `sqlitestore.Open` 的调用点限定为 `internal/app/{open,maintenance}.go`。
- `internal/workspacecoord` 没有自己的角色条目，只作为 `agentmanager` 和 `sqlitestore` 允许的依赖登记，它自身的导入不受约束。
