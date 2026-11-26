# 多 Agent 并行执行实现方案

> 文档状态：Implementation Plan v2.1 历史计划（设计已确定，P0、P1 于 2026-10-23 完成，P2、P3 于 2026-10-25 完成；P4 可选，未开始）
> 更新日期：2026-10-29（这是当时的实现计划快照，不是当前功能清单或验收报告；旧包名、迁移版本、路径和测试结论只适用于对应记录日期）
> 关联文档：[MULTI_AGENT_ARCHITECTURE.md](MULTI_AGENT_ARCHITECTURE.md)  
> 范围：子 Agent 并行调度、工作区协调、不依赖 Git 的隔离、合并提交、交付门禁、审批与交互

---

## 0. v2 修订说明

v1 经过设计审计，发现多个协议没有闭合：按 v1 实现，会出现静默覆盖、依赖任务过早解锁、恢复后重复消费结果等问题。审计引用的代码已逐项核对，结论全部成立。下表列出各项的 v2 处理。

| 审计项                            | 核对结果                                                                                                                                                                           | v2 处理                                                                   |
| --------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------- |
| 数据库单实例不等于工作区单写入域  | owner 锁锁的是数据库路径（`internal/runtimeowner/lock.go:23`）；租约冲突只在同一 Team 内查询（`internal/session/sqlitestore/leases.go:78`）                                        | 新增工作区协调域（第 5 节）                                               |
| 租约到期不能直接转交路径          | 冲突查询忽略已过期的租约，而旧 Shell 仍持有操作系统写权限                                                                                                                          | 租约存活绑定持有进程，撤销必须确认旧执行已退出（第 5.3 节）               |
| 同 Agent 可重入不等于免除操作互斥 | Flow 并行分支以同一 Actor 并发调用 Pipeline（`internal/agentflow/operators.go:334`、`internal/agentflow/runtime.go:252`）                                                          | 所有权与操作互斥分开，操作租约始终保留（第 5.4 节）                       |
| 首次写入才复制会静默覆盖          | `workspace_write` 不强制提供 `expected_sha256`（`internal/codingtools/write.go:159`）                                                                                              | 快照在子 Agent 启动时、第一次读取之前，在写入屏障下采集（第 7.2 节）      |
| 合并先判断后加锁                  | 判断与加锁之间主工作区可能变化；“等于目标即跳过”不足以保证多文件重放安全                                                                                                           | 不可变 ApplyPlan，审批绑定计划摘要，锁内重新校验，耐久应用日志（第 8 节） |
| 子 Agent 完成不代表 Task 交付     | 完成即推进 Task，并派发审查和依赖任务（`internal/agentmanager/supervisor_assignment.go:114`、`internal/agentmanager/supervisor_run.go:252`）                                       | 新增 Task 交付门禁（第 9 节）                                             |
| 只按文件状态决定是否计算哈希      | 同长度修改并保留 mtime 时会漏判，进而误删视图                                                                                                                                      | 最终判断一律基于内容哈希（第 7.4 节）                                     |
| 冲突解决闭环不通                  | 普通复制缺少基线；`agent_continue` 拒绝已分配 Task 的 Agent（`internal/agentmanager/supervisor.go:405`）；`MergeTransition` 不能更新补丁（`internal/workspacecoord/types.go:212`） | 所有模式保留基线；冲突解决产生新候选和新合并请求（第 8.4 节）             |
| Git 路径违反“主工作区无冲突标记”  | `ApplyPatch` 直接作用于主工作区并保留冲突标记（`internal/workspacecoord/git.go:134`）                                                                                              | 所有视图模式共用一个合并引擎，Git worktree 只是快照的创建方式（第 8 节）  |
| 邮箱缺少耐久消费协议              | `prepareModelInput` 同时服务于 `compaction`、`goal_assessment` 等辅助请求                                                                                                          | 消费账本，注入限定主 Turn 请求（第 11.3 节）                              |

表中引用的是 v2 审计时（实施前）的代码位置。P0–P3 实施后，其中一部分已移动或失效，例如 `internal/session/sqlitestore/leases.go` 已删除，租约迁到了 `internal/workspacedomain/leases.go`。当前生产路径以源码和 [Multi-Agent 当前架构](MULTI_AGENT_ARCHITECTURE.md) 为准；本计划中的旧 SQL 路径、迁移版本和历史测试结果不能作为当前入口或验收证据。

仍然成立的事实有：

- owner 锁按数据库路径加锁；
- Flow 并行分支共用同一 Actor；
- `expected_sha256` 仍是可选的；
- Git worktree 的合并仍走 `git apply --3way`（见第 8 节的实现偏离）。

补充契约全部采纳：后台唤醒的一等输入类型（第 11.2 节）、workspace 参数进入耐久身份（第 10 节）、沙箱按能力准入（第 6 节）、路径隔离边界与 `go-udiff` 冲突策略（第 13、8.4 节）、基线由宿主保护（第 7.1 节）、token 校准归属（第 12 节）。

实施顺序按审计建议调整：先做协调与身份，再开放只读并行，然后做快照隔离写入；共享目录并行写降为最后的可选优化（第 15 节）。

v2.1 确定了 v2 中留待验证的设计：汇合锁的位置与加锁协议（第 5.2 节）、实例与后代进程的存活判断（第 5.3 节）、默认值（第 18 节）。

## 1. 目标与原则

目标：子 Agent 默认并行执行；在任何目录下（不依赖 Git）保证并行写入安全；交互体验对齐 Codex 和 Claude Code。

原则：

1. 并行和隔离分开处理：只读 Agent 直接并行，可写 Agent 默认使用快照隔离。
2. 协调以工作区为单位，跨 Session、Team 和进程生效；无法加入协调的写入者被拒绝。
3. 子 Agent 的修改只通过 ApplyPlan 进入主工作区，主工作区永远不出现冲突标记。
4. 交付以合并提交为准：Task 完成、依赖解锁和审查结论都绑定到确切产物。
5. 所有状态持久化；恢复时使用持久化的生效参数，每一步要么安全重放，要么明确停止。

与 `MULTI_AGENT_ARCHITECTURE.md` 的关系：

- 保留：Supervisor 是唯一的生命周期控制面；每个 Agent 拥有独立 Thread；顶层工具批次串行执行（`internal/agentruntime/eino_turn.go` 的 `ExecuteSequentially`）；权限只能收窄。
- 修订第 1 节决策 9：可写子 Agent 默认使用快照视图，不依赖 Git worktree；Shell 的工作区级写 lease 只在第 13 节的条件全部满足后才去掉。

## 2. 外部参考（2026 年 10 月）

| 能力         | Codex                                                                                            | Claude Code                                                                                                                      |
| ------------ | ------------------------------------------------------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------- |
| 并行         | 子 Agent 在同一目录并发执行，继承父线程的沙箱与审批设置                                          | 子 Agent 默认在主会话目录中工作，可以在后台并发执行                                                                              |
| 写入隔离     | 桌面端 Worktree 模式：在 `$CODEX_HOME/worktrees` 下创建 detached HEAD 的 worktree，要求 Git 仓库 | `isolation: worktree`；后台会话首次写入时迁移到 `.claude/worktrees/`；非 Git 版本控制可用 `WorktreeCreate`/`WorktreeRemove` hook |
| 同目录冲突   | 靠任务拆分和基于原文匹配的编辑                                                                   | Agent Teams 文档提醒：同文件并行编辑会互相覆盖，建议每个成员负责不同的文件                                                       |
| 合并回主目录 | Handoff 或 Apply                                                                                 | 保留 worktree 和分支，由用户合并；无改动的 worktree 自动清理                                                                     |
| 审批         | 审批浮层显示来源线程，按 `o` 打开该线程；无法询问时失败，错误交还父 Agent                        | 后台子 Agent 的权限请求出现在主会话                                                                                              |
| 并发上限     | `agents.max_concurrent_threads_per_session`                                                      | `CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS`                                                                                           |
| 观察与控制   | `/subagents`、后台 Agent 面板                                                                    | `/tasks`、Agent View                                                                                                             |

这些产品提供了方向，但不能替代 GSEA 自己的协调、提交和恢复协议。

参考资料：

- https://developers.openai.com/codex/subagents
- https://developers.openai.com/codex/app/worktrees
- https://code.claude.com/docs/en/sub-agents
- https://code.claude.com/docs/en/worktrees
- https://code.claude.com/docs/en/agent-teams

## 3. 现状

下表是实施前的现状，行号是当时的位置，保留用于说明设计动机。截至 2026-10-26 的变化：

- `MaxRunningAgents` 已通过 `[agents].max_running`（默认 4）、`GSEA_AGENTS_MAX_RUNNING` 和 `--max-running-agents` 暴露。
- 租约已迁入协调库。
- Shell 的工作区级 lease 现在在 `withWorkspaceLease`（`internal/codingtools/workspace.go`）中获取。
- 快照视图位于工作区之外，由 `AttachSnapshotViewsDirectory` 登记。
- 审批卡片已显示来源。
- 后台输入改走独立的 `Turn.RunBackground`。

仍未改变的是 Git 隔离与 Git 合并两行。

| 方面     | 现状                                                                                                                  | 问题                                                                                                                                          |
| -------- | --------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------- |
| 调度     | Supervisor 已支持 goroutine 并发、按模型分桶限流、团队公平排队和持久化恢复                                            | `MaxRunningAgents` 默认为 1（`internal/agentmanager/supervisor.go:194`），且没有暴露为配置                                                    |
| 协调范围 | owner 锁按数据库路径加锁；租约存放在会话库中，冲突只在同一 Team 内查询                                                | 不同 DataDirectory 或不同 Team 可以同时写同一工作区                                                                                           |
| 共享写入 | 内置的 `default`、`worker` 为 `shared-lease`，`reviewer` 为只读（`internal/app/profiles.go:35`）                      | 租约冲突立即失败（`internal/session/sqlitestore/leases.go:92`）；`shell_exec` 获取工作区级写 lease（`internal/codingtools/workspace.go:160`） |
| Git 隔离 | `git-worktree` 依赖 `git rev-parse` 和 `git worktree add`                                                             | worktree 位于 `<workspace>/.gsea/worktrees/`（`internal/workspacecoord/git.go:186`），运行资源进入工作区，且与项目配置目录同名                |
| 合并     | `git apply --3way`（`internal/workspacecoord/git.go:158`）                                                            | 依赖 Git；冲突标记留在主工作区                                                                                                                |
| 任务交付 | Agent 完成即推进 Task，并派发后续任务                                                                                 | 产物尚未进入主工作区时，依赖任务和审查已经开始                                                                                                |
| 视图位置 | 视图根必须在工作区内（`internal/codingtools/workspace.go:97`）；沙箱根只能收窄（`internal/sandbox/invocation.go:23`） | 视图移出工作区时，沙箱要同步修改                                                                                                              |
| 审批来源 | 全 App 共用一个审批 Broker（`internal/app/open.go:53`），请求带有 `Actor`（`internal/policy/policy.go:29`）           | TUI 只显示工具名（`internal/tui/turn_state.go:69`）                                                                                           |
| 后台输入 | `Turn.Run` 要求最后一条消息是用户消息（`internal/agentruntime/turn.go:62`）                                           | 无法表达“不是用户发起的”后台唤醒                                                                                                              |

## 4. 核心不变量

| 编号 | 不变量                                                                                    |
| ---- | ----------------------------------------------------------------------------------------- |
| I1   | 同一工作区在任意时刻只属于一个协调域；无法加入该协调域的实例不能写入                      |
| I2   | 任意路径同一时刻最多一个有效写入者；撤销写权限以确认旧执行已经退出为前提，不依赖 TTL 到期 |
| I3   | 子 Agent 的所有可观察读取都来自同一个固定快照                                             |
| I4   | 子 Agent 的修改只通过 ApplyPlan 进入主工作区；主工作区永不包含冲突标记                    |
| I5   | Task 完成、依赖解锁和审查结论都绑定到确切产物（候选摘要或合并后的版本）                   |
| I6   | “有无修改”和变更集一律以内容哈希为准                                                      |
| I7   | 每条邮箱消息最多被一个 Turn 消费；恢复时先按消费账本核对，再决定是否唤醒                  |
| I8   | 恢复时使用持久化的生效参数，不按当前默认值重新推导                                        |

当前实现中，I4 只对 `snapshot` 模式成立。`git-worktree` 模式的合并仍用 `git apply --3way`，可能在主工作区留下冲突标记（第 8 节）。I5 中审查结论对候选摘要的绑定目前只是间接的（第 9 节）。

## 5. 工作区协调域

### 5.1 工作区身份与协调库

- 身份：工作区根目录的规范路径加文件系统标识（设备号 + inode），符号链接别名不会产生第二个身份。实现：`internal/workspacedomain/identity*.go`；darwin/linux 以外的平台退化为只用规范路径。
- 协调库：`$GSEA_HOME/workspaces/<workspace-id>/coord.db`。同一 `GSEA_HOME` 下的所有实例（不论 DataDirectory、Session 和 Team）共用它。租约迁入协调库，冲突查询以工作区为范围，不再限定 Team。
- 目录生命周期：创建、注册实例和打开协调库都在 `$GSEA_HOME/workspaces/domains.mutex` 内完成；最后一个实例关闭时删除该工作区的协调目录，评测这类临时工作区不会留下目录。崩溃实例只要自身或后代仍存活，目录就保留。

### 5.2 跨协调域互斥

- 不同 `GSEA_HOME` 的实例无法共享协调库，需要一个与 `GSEA_HOME` 无关的汇合目录：优先使用环境变量 `GSEA_COORDINATION_DIR`；Linux 上为 `$XDG_STATE_HOME/gsea/coordination`，未设置时为 `~/.local/state/gsea/coordination`；macOS 上为 `~/Library/Application Support/gsea/coordination`。该位置不能在 `config.toml` 中配置，否则不同 `GSEA_HOME` 会指向不同的汇合目录。
- 汇合目录中有一个全局短互斥锁 `rendezvous.mutex`，以及每个工作区一个 `<workspace-id>.lock`，文件内容记录当前协调域（规范化后的 `GSEA_HOME`）。
- 加入：持有互斥锁后，对工作区锁尝试 `LOCK_EX|LOCK_NB`。成功说明没有其他成员，写入并 fsync 协调域记录后降为 `LOCK_SH`；失败说明已有成员，读取记录：同一协调域则加 `LOCK_SH`，不同协调域则报错，错误信息给出对方的 `GSEA_HOME`。
- 离开：持有互斥锁后解锁，再尝试 `LOCK_EX|LOCK_NB`；成功说明自己是最后一个成员，删除锁文件。
- 加入和离开都在互斥锁内完成。flock 的锁转换不是原子的，删除锁文件也会换掉 inode，互斥锁保证这两个窗口内没有其他实例插入。进程崩溃时内核释放锁，下一个加入者拿到排他锁后覆盖旧记录。
- 汇合目录无法创建或打开时（只读 HOME、受限沙箱），跳过汇合锁，并记录“跨协调域互斥不可用”；同一 `GSEA_HOME` 内的协调不受影响。以编程方式构造、没有设置汇合目录的配置（例如测试）同样跳过。

### 5.3 租约存活、隔离令牌与撤销

- 实例存活：每个实例在协调目录下创建 `instances/<instance-id>.lock`，进程存活期间一直持有 `LOCK_EX`，并在协调库的 `instances` 表登记 PID、主机名和 DataDirectory。租约记录持有它的实例。不复用 runtime owner 锁：它按 DataDirectory 划分，而且探测它会让同一 DataDirectory 新启动的进程误判为已被占用。
- 后代存活：每次执行 Shell 前，实例为 `instances/<instance-id>.children.lock` 打开一个新的文件描述并加 `LOCK_SH`，作为额外的文件描述符传给子进程。实例被杀死后，只要还有继承了它的后代进程（例如 macOS 上被遗弃的进程组），实例仍视为存活，租约不会被回收。每次执行使用独立的文件描述，子进程解锁只影响自己那一份。
- 存活判断：依次对实例锁和后代锁尝试 `LOCK_EX|LOCK_NB`，两者都成功才视为已退出。flock 锁属于打开的文件描述，同一进程内另开描述探测自身也会得到“存活”。锁文件是唯一依据，登记的主机名只用于诊断：macOS 的主机名会随网络变化，按主机名判断会让崩溃实例的租约永远无法回收。
- 回收：申请租约遇到冲突时探测冲突持有者，已退出实例的租约在同一事务内标记为 `abandoned`，然后继续判断；Supervisor 恢复时回收所有已退出实例的租约。租约不再有 TTL，恢复时也不再释放本库所有 Team 的租约，否则会释放其他存活实例的租约。
- epoch：协调库的 `meta` 表维护单调递增的计数，每次授予都递增并写入租约。P0 的操作租约在整个工具调用期间持有，epoch 只做记录；引入长期的所有权租约后，工具执行和合并提交都要携带 epoch，过期的请求被拒绝。未实现：P2 用快照隔离代替了所有权租约，`leases` 表的 `mode` 只有 `read`/`write`，`state` 只有 `active`/`released`/`abandoned`，epoch 至今只写入、不校验。所有权租约与 epoch 校验随 P4 引入。
- 撤销流程：`撤销中 → 停止接受新的工具调用 → 取消进程组并确认退出 → 释放 → 授予新持有者`。epoch 只能拒绝晚到的结果，无法收回运行中 Shell 的写权限，所以“确认退出”这一步不能省。当前只有进程内撤销：Run 被取消后，Shell 工具杀死进程组并等待它退出，之后才释放租约；跨进程转交只发生在持有实例及其后代全部退出之后。显式的 `revoking` 状态没有随 P2 引入，改为随 P4 的所有权租约引入。

### 5.4 操作互斥

- 所有权租约回答“其他 Agent 能不能写”，操作租约回答“这个文件此刻能不能被修改”，两者相互独立。即使是所有者，每次写入仍要申请操作租约，因为 Flow 的并行分支会以同一 Actor 并发调用写工具。目前只实现了操作租约，所有权租约属于 P4。
- 操作租约冲突时以退避轮询有界等待 10 秒，超时返回明确的错误，错误中说明持有者（Agent、实例、PID）。
- 路径比较不区分大小写：在大小写敏感的文件系统上只会多一些不必要的等待，在 macOS 默认的大小写不敏感文件系统上则是必需的。
- 幂等键只用于标识请求：同一个键必须描述同一个请求，否则报冲突；租约不在多次尝试之间共享，崩溃后重放的工具调用会重新申请。

## 6. 工作区模式

| 模式       | 工作位置            | 可写范围                   | 开放阶段               |
| ---------- | ------------------- | -------------------------- | ---------------------- |
| `readonly` | 主工作区            | 不可写                     | P1                     |
| `snapshot` | 视图的 `work/` 目录 | `work/` 内任意位置         | P2                     |
| `shared`   | 主工作区            | 只能写声明的 `owned_paths` | P4，可选优化，默认关闭 |

- `auto` 规则：只读 Profile 使用 `readonly`，其余使用 `snapshot`。`shared` 只在显式请求、开启 `shared_writes` 且沙箱能力支持时可用。
- 实现偏离（2026-10-26 核对）：
  - 持久化的生效模式沿用 `orchestration` 的名称：`shared-readonly`、`shared-lease`、`git-worktree`、`snapshot`（`internal/orchestration/agent.go`）。
  - `auto` 的解析在 `grantWorkspaceMode`/`writerMode`（`internal/agentmanager/supervisor_workspace.go`）中完成：
    - 请求 `readonly` 或只读 Profile，得到 `shared-readonly`；
    - Profile 声明 `git-worktree` 的，保持 `git-worktree`；
    - 其余可写者在有快照存储时得到 `snapshot`。App 总会接线快照存储；以编程方式构造、没有快照存储的 Supervisor 会退回 `shared-lease`（串行的共享写）。
  - 没有持久化模式的旧 Agent 按 Profile 推导，写入者为 `shared-lease`。
- 未实现：`agent_spawn` 的 `workspace` 只接受 `auto`/`readonly`，`shared` 会被拒绝。配置 `default_workspace = "shared"` 映射到旧的 `shared-lease`，不是 P4 的 `owned_paths` 模式。`shared_writes` 和按沙箱能力的准入（下一条）都属于 P4，代码中尚不存在。
- 沙箱按能力准入：`ModeContainer`（`internal/sandbox/sandbox.go`）依赖外层容器，无法在 Agent 之间隔离写入范围，在该模式下拒绝 `shared`。
- `snapshot` 不可用（例如普通复制超过上限）时，创建失败并说明原因，不退化为无保护的并行写入。已实现：超过上限返回 `ErrSnapshotCopyLimit`。显式配置 `default_workspace = "snapshot"` 但没有快照存储时，Supervisor 构造失败。

## 7. 快照视图

### 7.1 存储布局

```text
<DataDirectory>/views/snap-<hash>/
├── work/              # Child Agent root: the only writable path.
└── host/              # Host-owned evidence, outside the child's writable root.
    ├── base/          # Baseline content, kept in every mode.
    └── manifest.json  # Baseline entries: path, type, mode and sha256.
```

子 Agent 的工具根目录和沙箱根目录都是 `work/`，基线和清单不在其可写范围内。目录名不是视图 ID，而是 `snap-` 加 sha256(Team/Agent/Run) 的前 24 个十六进制字符（`SnapshotName`，`internal/workspacecoord/snapshot.go`）。视图记录在快照实体化之后创建。

### 7.2 创建时机与一致性

- 在子 Agent 启动时、第一次可观察读取之前创建，不做懒创建。
- 采集期间持有协调域的写入屏障，阻止主工作区写入，保证采集到的是同一时刻的文件组合。
- 优先写时复制：macOS 用 `clonefile`，Linux 用 `FICLONE`（现有依赖 `golang.org/x/sys`），屏障通常很短。跨卷或不支持时退回普通复制，并受 `snapshot_copy_limit` 限制，超过上限时创建失败。
- 先复制到临时目录，再整体 rename 到位。`.git` 在写时复制模式下照常克隆，普通复制模式下排除。符号链接只复制链接本身。
- 实现细节（2026-10-26 核对，`internal/workspacecoord/snapshot.go`）：
  - 屏障是整个工作区的写租约，最长等待 6 分钟（`snapshotBarrierWait`，`internal/app/snapshot_merge.go`）。它只在把主工作区复制到 `host/base` 期间持有，`work/` 在释放屏障之后从基线复制。
  - 复制上限同时计入基线和 `work/` 的普通复制字节数，默认 1GiB。
  - `.git` 始终不进入基线和清单。只有整个基线都克隆成功时，`.git` 才被克隆到 `work/.git`，克隆失败则丢弃。
  - `.gsea/` 和 `.gsea-apply-*` 被排除。其他特殊文件被跳过。

### 7.3 基线

所有模式都保留基线内容：写时复制模式下是一份克隆，普通复制模式下是一份副本，成本计入复制上限。没有基线，就无法做三方合并和变基。实现偏离：只有 `snapshot` 模式保留 `host/base`。`git-worktree` 模式仍只记录 Git 的 `BaseRevision`，由 `git worktree add` 创建，不保留内容基线（见第 8 节）。

### 7.4 变更判断

- 运行期间可以用 inode、大小和 ctime 做增量扫描提示（ctime 由内核维护，普通进程无法设置），但不作为最终依据。未实现：目前没有增量扫描提示，每次都全量计算内容哈希。
- 生成候选，以及判断能否删除视图时，对 `work/` 中的全部文件计算内容哈希，与基线清单比较。基线不可变，清单哈希可以在视图创建后于后台计算。实现：基线清单（`BuildManifest`，sha256）在快照发布前同步计算，没有放到后台。
- 只有确认全部内容与基线一致，才自动删除视图。此外，合并提交（`applied`）后视图也会被删除（`internal/workspacecoord/preservation.go`）。

### 7.5 工具与沙箱

- `operationRoot` 接受由可信代码登记的视图 `work/` 目录；为每个视图创建以 `work/` 为根的沙箱 runner，并按视图缓存。实现：`operationRoot` 只检查路径位于登记的 views 目录下，`work/` 这一级由 `ViewSpec.Validate` 保证。
- `ViewSpec.Validate` 增加 `snapshot` 分支：视图位于工作区之外，`BaseRevision` 填清单摘要。实现：要求 base 与 work 相同，且 `BaseRevision` 以 `sha256:` 开头。

## 8. 合并与 ApplyPlan

所有视图模式共用一个基于内容的合并引擎。Git worktree 只是快照的一种创建方式，不再用 `git apply --3way` 直接改动主工作区。

实现偏离（2026-10-26 核对）：P2 只把 `snapshot` 类型的合并（`merge_queue.kind = 'snapshot'`）接入了下面的候选/ApplyPlan/应用日志流程（`internal/workspacecoord/merge_snapshot.go`、`applyplan.go`、`applycommit.go`）。`git-worktree` 视图仍在 `<workspace>/.gsea/worktrees/<id>` 下创建。它的合并（`kind = 'git-patch'`）由 `deliverPatch` 在子 Agent 成功结束时自动入队，仍由 `MergeExecutor.runPatch` 读取补丁 artifact，用 `git apply --3way` 直接作用于主工作区（`internal/workspacecoord/git.go`）。后台合并（`RunPendingAutomatic`）对两类合并使用同一套门禁：等待 Task 进入等待合并、除非 `auto_merge` 否则请求审批；Task 已离开该状态的 Git 补丁留给 `merge_run`。统一引擎仍是待办项。

### 8.1 候选

子 Agent 完成后，宿主冻结 `work/` 的内容，生成不可变候选：候选摘要，加上每个路径的类型、权限、基线哈希和新内容哈希，内容存入 artifact。此后对 `work/` 的修改不影响该候选。已实现：`internal/workspacecoord/candidate.go` 实现候选，摘要写入 `merge_queue.candidate_digest`。

### 8.2 ApplyPlan

- 基于候选和主工作区当前状态，生成不可变的 ApplyPlan。每个路径包含 preimage 哈希（主工作区当前内容）、postimage 内容、文件类型和权限，计划本身带摘要。
- 审批绑定计划摘要，计划一旦变化就必须重新审批。实现：绑定只在一次执行内有效（`merge_snapshot.go`）。审批通过后，在屏障内重建计划；摘要不同就重新审批，最多 3 轮（`planRounds`），仍在变化时合并保持 `running`，留给下一轮。`plan_digest` 只在应用日志和 `applied` 状态转换中持久化。
- 每个路径的 postimage：
  - 主工作区等于基线：取候选内容（快进）；
  - 主工作区和候选都改过：取三方合并结果；无法合并时，候选进入冲突处理（第 8.4 节），不生成计划。

### 8.3 提交与恢复

1. 获取协调域写锁，以及所有涉及路径的操作租约（原子批量申请）。
2. 在锁内重新校验每个路径的 preimage；任何一个不一致，计划作废，基于新的主工作区状态重新生成计划并重新审批。
3. 先写入耐久应用日志，再逐个路径用“临时文件 + rename”写入，每完成一个路径记录一次进度。
4. 全部完成后校验 postimage，标记提交完成，然后释放锁。
5. 崩溃恢复：逐个路径比较当前内容。等于 postimage 视为已完成，等于 preimage 视为未开始；两者都不是，说明有外部写入，停止并报告，不做任何覆盖。全部核对通过后，继续剩余路径。

实现（2026-10-26 核对）：

- 第 3–5 步已按设计实现：
  - 应用日志是会话库的 `merge_apply_journal`（`started`/`finished`）和 `merge_apply_progress`，由迁移 v35 引入，代码在 `internal/session/sqlitestore/merge_apply.go`。
  - 写入使用确定名称的临时文件 `.gsea-apply-<hash>`，rename 后 fsync 目录（`internal/workspacecoord/applycommit.go`）。
  - 重启后由 `resumeSnapshot` 续提交。
- 第 1、2 步有偏离：
  - 第 1 步只持有整个工作区的屏障写租约，没有逐路径批量申请操作租约。
  - 第 2 步中，计划摘要变化会走重新审批（见第 8.2 节）。摘要相同、但 preimage 校验失败（`ErrApplyDiverged`）时，合并直接标记为冲突、转入第 8.4 节的冲突处理，不会重新生成计划。

### 8.4 冲突处理

- 三方合并用 `go-udiff` 计算编辑，并叠加项目自己的冲突策略：编辑范围重叠、或同一位置插入不同内容，都判定为冲突。`go-udiff` 的 `Merge` 会把同一位置的不同插入直接拼接，不能直接采用它的结果。
- 冲突在新的解决尝试中处理：以主工作区当前状态为新基线创建视图，把候选作为“对方修改”三方合并进去，冲突标记只出现在这个视图里。
- 解决尝试由显式的 Resolution Task 执行，产出绑定新基线的新候选和新合并请求，重新验证、重新审批。原合并请求标记为 superseded，不复活原 Agent，也不重排旧请求。
- 已实现（快照合并）：
  - 三方合并用 `go-udiff` v0.3.1，只取它算出的编辑，不用 `udiff.Merge`（`internal/workspacecoord/merge3.go`）。冲突标记为 `<<<<<<< gsea:main` / `=======` / `>>>>>>> gsea:candidate`。
  - 两侧都修改了二进制内容，或两侧都修改了删除、符号链接等非普通文件的路径，判定为冲突。
  - `markConflict` 创建 `Resolve merge conflict …` 任务，要求 `worker` Profile。`PrepareResolution` 在当前主工作区上重放候选，生成带冲突标记的视图。
  - 新合并通过 `supersedes`/`superseded_by` 链接旧合并，被替代的合并不会重新排队。
- 实现偏离：`git-patch` 类型的冲突合并仍在 Resolution Task 完成后，通过 `TransitionMerge` 从 `conflicted` 原地改回 `pending`。

## 9. Task 交付门禁

实施前的逻辑在 Agent 完成时就把 Task 推进到 `completed` 或 `verification`，并派发审查和依赖任务（当时的 `internal/agentmanager/supervisor_assignment.go:114`、`internal/agentmanager/supervisor_run.go:252`）。快照模式下产物还在视图里，必须改为：

`候选产出 → 候选验证（可选，绑定候选摘要） → 等待合并 → 合并提交 → 对合并后的版本验证 → Task 完成、依赖解锁`

- 审查 Agent 以只读方式读取候选视图，或在合并后读取主工作区；审查结论记录所绑定的候选摘要或合并版本。
- 依赖任务只在合并提交完成后才能被认领。
- Task 状态机增加候选和合并相关的状态（追加式迁移）。实现：只新增 `awaiting_merge` 一个状态（`internal/orchestration/state.go`）。`tasks.state` 没有 CHECK 约束，所以不需要迁移。
- P2 已实现：快照子 Agent 完成后先冻结视图、生成候选并排队合并，Task 进入等待合并；指定了审查 Profile 时先进入验证，审查 Agent 获得以候选工作树为根的只读视图，提示中带候选 diff。合并提交后 Task 才完成并解锁依赖任务；冲突时自动创建并派发 Resolution Task，它的视图以候选为种子、带冲突标记，产出的新合并替代原合并。
- 未实现：
  - 审查结论尚不独立保存候选摘要，但已绑定当前 Task attempt 指派的 reviewer 并检查 expected version。`task_verify` 使用 `Reason` 推进状态（`internal/agentmanager/tools_task.go`），绑定只是间接的：候选不可变，合并请求带 `candidate_digest`。
  - 合并后验证只是一条可选的 shell 检查：配置 `[agents].post_merge_check` 后，合并进入 `applied` 时在主工作区沙箱中运行一次（`MergeVerifier`，超时 `post_merge_check_timeout`，默认 10m），通过则 Task `completed`，失败则 Task `failed`、依赖保持阻塞，变更留在主工作区不回滚。没有配置时提交后直接 `completed`；检查回执现已绑定 merge 候选/计划、主工作区内容和检查配置摘要；结果未知时不自动重跑，仍没有审查 Agent 参与。

## 10. 调度与配置

```toml
[agents]
max_running = 4               # Concurrent child Agents; the root Agent is excluded.
max_live = 16
max_depth = 3
max_children = 4
default_workspace = "auto"    # auto | snapshot | readonly | shared
auto_merge = false            # Apply conflict-free plans without approval.
auto_continue = true          # Wake an idle root Agent when children finish.
snapshot_copy_limit = "1GiB"  # Limit for non-copy-on-write views and baselines.
shared_writes = false         # Enable the owned-path optimization (P4, not implemented yet).

[agents.model_limits]         # Per-model concurrency buckets.
"qwen3:8b" = 2
```

- 优先级与现有配置一致，同时提供环境变量（例如 `GSEA_AGENTS_MAX_RUNNING`）和命令行参数 `--max-running-agents`（定义在 `internal/cli/chat_options.go`）。`[agents]` 负责多 Agent 编排，与现有的 `[agent]`（根 Agent 运行参数）分开。
- P1 已实现 `max_running`、`max_live`、`max_depth`、`max_children` 和 `[agents.model_limits]`，以及对应的 `GSEA_AGENTS_*` 环境变量。`model_limits` 的键是 Profile 的模型名（`ModelProfile`，没有时为 `default`）；内置 Profile 的 `ModelProfile` 就是当前模型名。含点号或冒号的模型名需要加引号（例如 `"qwen3:8b" = 2`）；后加载的配置按模型逐项覆盖，不会整表替换。`model_limits` 没有对应的环境变量。其余键随各自的阶段引入。
- `agent_spawn` 新增 `workspace` 和 `owned_paths`。请求值、生效模式、规范化后的路径、父级授权范围和策略版本，全部进入 Spawn 幂等哈希、事件和恢复检查。恢复时使用持久化的生效模式，不按当前的 `auto` 重新选择。
- P0 已实现 `workspace` 的 `auto` 与 `readonly`：请求值进入幂等哈希（`auto` 不改变升级前记录的哈希），生效模式写入 Agent 记录（`workspace_request`、`workspace_mode`，迁移 v34）和 Spawn 事件；`readonly` 同时收窄持久化的 Profile 快照，并在委派检查之前生效。未实现的取值直接拒绝，不会退化为可写。生效模式由已哈希的输入推导，不单独进入哈希（`internal/session/sqlitestore/agents_spawn.go`）；`owned_paths`、规范化路径和策略版本随 P4 引入。
- 准入：`readonly` 和 `snapshot` 受并发上限和模型分桶限制；`shared` 还需要拿到全部所有权租约。
- P1 的准入规则：只读和 Git worktree 子 Agent 在 `max_running` 内并行；直接写共享工作区（`shared-lease`）的子 Agent 同一时刻只运行一个，因为操作租约只能串行化单次写入，不能防止两个任务交错修改。等待中的子 Agent 不占用写入名额，否则父子都可写时会互相等待而死锁。
- 子 Agent 的结果附带候选摘要和合并请求 ID。
- P2 已实现 `default_workspace`、`auto_merge`、`snapshot_copy_limit` 以及 `GSEA_AGENTS_DEFAULT_WORKSPACE`、`GSEA_AGENTS_AUTO_MERGE`、`GSEA_AGENTS_SNAPSHOT_COPY_LIMIT`。`snapshot_copy_limit` 接受纯字节数或 `KB/MB/GB/TB`（十进制）与 `KiB/MiB/GiB/TiB`（二进制）后缀，必须为正。快照子 Agent 不占用共享写入名额，在 `max_running` 内并行；子 Agent 结果的 `delivery` 字段附带候选摘要和合并请求 ID。
- P3 已实现 `auto_continue`（默认 `true`）与 `GSEA_AGENTS_AUTO_CONTINUE`。关闭后，子 Agent 的结果只在用户下一次输入的 Turn 中送达，根 Agent 不会自行唤醒。

## 11. 审批、观察与结果回传

### 11.1 审批与面板

- 审批来源：把请求中的 `Actor` 解析成“Profile 名 + 短 ID + 任务标题”显示，按 `o` 打开该 Agent 的对话记录。非交互模式维持默认拒绝，错误交还父 Agent。
- P1 已实现来源显示：审批卡片显示 `requested by <profile> <短 ID> · <任务标题>`。并行的子 Agent 可能同时请求审批，也可能复用根 Agent 的工具调用 ID，所以审批请求改用独立生成的 ID，TUI 逐个排队展示，子 Agent 的请求也不会挂到根 Agent 的工具卡片上；审批监听在启动时建立，根 Agent 空闲时也能收到子 Agent 的请求。按 `o` 查看对话记录随 P3 的 `/agents` 面板实现。
- 合并审批卡片展示 ApplyPlan 的摘要和 diff，批准的对象是该计划摘要。P2 已实现：审批以 `merge_apply` 工具名发出（它只是审批名称，规则为 `merge.apply`、能力为 `workspace.write`，不是模型可调用的工具；模型侧的合并工具是 `merge_list`、`merge_requeue`、`merge_run`），参数包含合并 ID、候选与计划摘要、路径列表和 diff，卡片直接展开 diff；计划变化后重新审批。
- `/activity` 升级为 `/agents`：显示状态、当前工具、耗时、token 和待合并的候选；支持查看对话记录、中断、审查合并、丢弃候选。状态栏增加类似 `agents 2 running 1 queued` 的摘要。
- P3 已实现：`/agents`（`/activity` 保留为别名）和 `Ctrl+G` 打开面板并获得焦点。`↑↓` 在 Agent 行和合并行之间选择，`Enter`/`o` 打开子 Agent 的对话记录（替换主对话区，包括仍在运行的 Turn，`Esc` 返回），`x` 连按两次中断子 Agent（根 Agent 仍用 `Ctrl+C`），`r` 对待决定的合并重新发起审批，`d` 连按两次丢弃候选（合并被拒绝、对应 Task 失败，视图保留在磁盘上），`Esc` 或输入其他字符把焦点交还输入框。当前工具来自运行中子 Agent 的实时上报，Run 结束后清除；token 来自用量账本。面板显示时每 2 秒刷新，隐藏时每 5 秒刷新以更新状态栏摘要。状态栏摘要的格式为 `agents N running M waiting K queued`（为 0 的项省略），有待决合并时追加 `merges N pending`。面板代码在 `internal/tui/{agents_panel,activity,commands,keys}.go`；`Ctrl+G` 之外还绑定了 `Alt+G`。

### 11.2 后台唤醒

- 新增一等输入类型：`Turn.Run` 需要从“最后一条必须是用户消息”泛化为“用户输入”和“后台输入”两种，后台输入使用独立的事件类型，不写成用户消息。
- 规则：后台唤醒 Turn 不参与 Goal 准入；计入团队预算，并限制唤醒频率；用户主动取消后，在用户下一次输入之前不自动唤醒。
- P3 已实现：后台输入写成 `background.input` 事件，以 `Untrusted Agent messages…` 开头的用户角色消息进入模型输入，TUI 显示为 `◆ Agent messages` 块，不进入输入历史。TUI 空闲时每 2 秒检查一次待处理消息；只有在没有进行中的 Turn、没有待处理审批、输入框为空、距上次唤醒至少 5 秒、连续唤醒少于 3 次时才唤醒，用户输入后重置计数。团队预算耗尽或消息已被其他 Turn 取走时，唤醒静默跳过，不留下记录。唤醒 Turn 不计入用户 Turn 数。实现方式与设计的差异：`Turn.Run` 仍要求最后一条是用户消息，后台输入另走 `Turn.RunBackground`（`turn.started` 的 `input` 为 `background`）。`RunBackgroundTurn` 使用普通 Turn，后台输入不创建执行契约（`internal/conversation/background.go`）。唤醒策略在 `internal/tui/background.go`。

### 11.3 邮箱消费协议

- 注入时持久化“消息 ID → 消费它的 Turn 和模型请求”。同一个 Turn 内每条消息只注入一次，后续请求通过 Turn 历史看到它。
- Turn 提交后确认消息。恢复时先核对：绑定到已完成 Turn 的消息补确认；绑定到未完成 Turn 的消息按该 Turn 的恢复规则处理；只有未绑定的消息才可能触发唤醒。
- 只对主 Turn 的模型请求注入。`prepareModelInput`（`internal/agentruntime/request_context.go`）也服务于 `compaction`、`goal_assessment` 等辅助请求，这些 purpose 一律排除。
- P3 已实现：根 Agent 的邮箱改用会话库中的消费账本（认领绑定 Turn、提交后确认、失败后释放），启动时先结算崩溃前已提交 Turn 的消息；Turn 内注入只发生在 purpose 为空的主请求，位置随事件持久化，重放时插回原位。子 Agent 仍沿用按 Run 投递的邮箱，`Factory` 为子 Agent 去掉账本。账本没有新建表，迁移 v36 为 `mailbox_messages` 增加 `consumer_turn_id`、`consumer_request`、`bound_at`（`internal/session/sqlitestore/mailbox_ledger.go`）。“失败后释放”是延迟的：失败 Turn 不立即释放，下一个 Turn 创建时由 `NewTurn` 结算并释放（`internal/agentruntime/background.go`）。

## 12. 运行时状态隔离

- token 校准：实施前，同一模型 Profile 的子 Agent 共享父 Agent 的校准状态（`internal/agentruntime/factory_views.go`），切换模型时已经独立。这是校准归属问题，不是已证实的数据竞争。改为每个 Agent 独立校准，tokenizer 增加 `Fork` 方法。P1 已实现：子 Agent 的 tokenizer 从父 Agent 当前的校准值开始，之后只根据自己的请求调整，共享的编码表不复制。`Factory` 自身的上下文校准（`contextCalibration`）对子 Agent 从空开始（`internal/agentruntime/factory_views.go`）。
- 模型客户端：用 `-race` 和 `httptest` 验证并发请求是否安全；不安全时改为每个 Agent 单独构造客户端。P1 已验证：Chat Completions、Responses 和 Ollama 三种客户端在 8 个并发调用方下的 Generate 与 Stream 没有数据竞争，也没有串答，继续共享同一个客户端。
- SQLite：会话库当前只有一个连接。先实测 4 个子 Agent 并发时的竞争，再决定是否为面板轮询增加只读连接池。现状：会话库、`sessionregistry` 和工作区协调库是三个独立的 SQLite 库，都设置 `SetMaxOpenConns(1)`；尚未压测，也没有只读连接池。

## 13. 共享目录并行写（P4，可选优化）

`shared` 要同时处理动态权限范围、租约撤销、根 Agent 的写入和 Flow 内部并发，复杂度高于快照隔离，因此放在最后，默认关闭。开放前必须满足：

- 路径规范化：在大小写不敏感的文件系统上（macOS 默认如此）按文件系统语义比较路径，避免 `src/A.go` 和 `src/a.go` 被当成两个路径。
- 新建文件：所有权只授予目录，或由宿主代为创建文件；不能为了允许新建而放开父目录的写权限。
- 原子写入：临时文件必须落在所拥有的目录内部。
- rename 与链接：禁止跨所有权边界的 rename。硬链接可能绕过基于路径的沙箱：Linux 上跨 bind mount 创建会失败，macOS Seatbelt 的行为需要验证，开放前必须有对应用例。
- 写入范围：`shared` 子 Agent 只有 `owned_paths` 和临时目录可写；根 Agent 的可写范围扣除其他 Agent 当前拥有的路径。Linux 用 bubblewrap 的 `--ro-bind`/`--bind` 叠加，macOS 的 Seatbelt 规则优先级需要先验证。
- Shell 的变更报告只保留本次调用可写范围内的变化。
- 上述条件全部满足后，才去掉 `shell_exec` 的工作区级写 lease（`internal/codingtools/workspace.go` 的 `withWorkspaceLease`；Git worktree 与快照视图中的 Shell 已经不取这个 lease）。

## 14. 数据与兼容性

- 新增工作区协调库（第 5.1 节），租约迁入协调库，增加持有实例、epoch 和释放原因；会话库中的 `workspace_leases` 表（v11、v12）保留，但不再读写。协调库的 schema（`internal/workspacedomain/store.go`）包含 `meta`、`instances` 和 `leases` 三张表。它的版本由 `user_version` 管理，只有一版。
- 会话库追加迁移：Task 增加候选与合并状态；邮箱增加消费绑定；Agent 增加 workspace 耐久身份字段。迁移账本带校验和，只能追加新版本。实际（当前最高 v38）：
  - v34：`agents.workspace_request`、`agents.workspace_mode`。
  - v35：`merge_queue` 增加 `kind`、`candidate_digest`、`plan_digest`、`supersedes`、`superseded_by`，并新建 `merge_apply_journal`、`merge_apply_progress`。
  - v36：`mailbox_messages` 增加消费绑定列。
  - v37：`merge_finalizations` 记录检查派发、结果与收尾进度。
  - v38：finalization 增加 `verification_digest`，防止重启换命令后沿用旧验证绑定。
  - Task 的 `awaiting_merge` 不需要迁移。
- `workspace_views.mode` 没有 CHECK 约束，新增 `snapshot` 模式不需要迁移。
- 合并请求改为不可变：新增 `superseded_by`，冲突解决产生新请求，不再通过 `MergeTransition` 改写旧请求。实现：快照合并已按此执行；`git-patch` 合并仍原地改回 `pending`（第 8.4 节）。状态推进仍统一经过 `TransitionMerge`。
- 现有 Profile 中的 `shared-lease` 视为 `auto`；已存在的 Git worktree 视图继续使用旧路径，直到完成或被丢弃。实现：内置 `default`、`worker` Profile 仍声明 `shared-lease`，但 `grantWorkspaceMode` 对非只读、非 Git worktree/显式 snapshot 的 Profile 使用配置默认的写入模式，效果等同 `auto`。例外是没有持久化模式的旧 Agent，仍推导为 `shared-lease`。

## 15. 分阶段实施

### P0 协调与身份基础

- 内容：工作区身份与协调库；跨协调域汇合锁；租约迁入协调库并按工作区范围判断冲突；实例与后代进程的存活判断、epoch、有界等待；Spawn 的 workspace 参数进入耐久身份；恢复不变量测试。
- 主要文件：新增 `internal/workspacedomain/`；`internal/orchestration/{lease,state,agent}.go`；`internal/sandbox/sandbox.go`；`internal/codingtools/{workspace,shell}.go`；`internal/agentmanager/{supervisor,supervisor_recovery}.go`；`internal/app/{open,close,orchestration}.go`；`internal/config/{config,load}.go`；`internal/session/sqlitestore/schema.go`（Spawn 身份迁移），删除 `internal/session/sqlitestore/leases.go`。
- 验收：两个使用不同 DataDirectory 的实例在同一协调库下正确互斥；使用不同 `GSEA_HOME` 的第二个实例被拒绝；持有进程被杀死后租约可以回收，但它遗留的后代进程退出之前不会回收；Run 被取消后，租约在 Shell 进程组退出之后才释放。
- 状态（2026-10-23）：已完成。`internal/workspacedomain` 的测试覆盖别名身份、跨协调域拒绝与崩溃后重新加入、并发首次打开、跨实例冲突（含大小写）、有界等待、已退出实例的回收、后代进程保活与目录清理；`internal/app` 的测试覆盖两个 DataDirectory 共享租约和不同 `GSEA_HOME` 被拒绝。在 macOS 上用真实 Seatbelt 沙箱手动验证过：脱离进程组的后代进程使崩溃实例保持存活，直到它退出。架构测试把 `internal/workspacedomain` 登记为 SQLite 的第三个所有者，依赖限定为 `internal/orchestration`。

### P1 只读并行

- 内容：`[agents]` 配置接入 Supervisor；只读子 Agent 并行，可写子 Agent 保持串行；审批显示来源；token 校准按 Agent 独立；验证模型客户端的并发安全。
- 主要文件：`internal/config/*`、`internal/cli/{chat,chat_options}.go`、`internal/app/orchestration.go`、`internal/agentmanager/supervisor*.go`（准入在 `supervisor_scheduler.go`）、`internal/agentruntime/factory_views.go`、`internal/tokenizer/tokenizer.go`、`internal/tui/turn_state.go`。
- 验收：4 个只读子 Agent 的执行时间确实重叠；子 Agent 的审批显示来源；子 Agent 的用量不影响根 Agent 的上下文估算；`-race` 通过。
- 状态（2026-10-23）：已完成。测试覆盖 4 个只读子 Agent 同时运行、两个可写子 Agent 依次运行且只读子 Agent 不受影响、`[agents]` 配置的分层与校验、审批请求 ID 唯一与 TUI 排队、来源标签、子 Agent 校准不影响根 Agent，以及三种模型客户端的并发调用；涉及的包都通过 `-race`。会话库单连接在 4 个子 Agent 下的竞争尚未实测，留到 P3 面板轮询时一起评估。

### P2 快照隔离写入

- 内容：启动时在写入屏障下创建快照；所有模式保留基线；内容哈希判断；候选、ApplyPlan、应用日志与恢复；统一合并引擎（含 Git worktree，未完成，见第 8 节）；冲突解决尝试；Task 交付门禁；合并审批卡片。
- 主要文件：
  - `internal/workspacecoord/*`：新增 `snapshot.go`、`clone_{darwin,linux,other}.go`、`manifest.go`、`candidate.go`、`applyplan.go`、`applycommit.go`、`merge3.go`、`merge_snapshot.go`、`resolution.go`、`snapshot_review.go`、`preservation.go`。
  - `internal/codingtools/workspace.go`。
  - `internal/agentmanager/{supervisor_workspace,supervisor_assignment,supervisor_run,supervisor_merge,supervisor_delivery}.go`。
  - `internal/session/sqlitestore/merge_apply.go`。
  - `internal/app/{open,snapshot_merge}.go`。
  - `internal/tui/turn_state.go`。
- 验收：在非 Git 目录中，两个子 Agent 修改同一文件的不同位置后自动合并；同一位置的修改产生冲突，在解决视图中处理，主工作区始终没有冲突标记；在应用日志的每一步注入崩溃，恢复结果都正确；依赖任务在合并提交前不会启动。
- 状态（2026-10-25）：已完成。`internal/workspacecoord` 的测试覆盖快照隔离与复用、复制上限失败关闭、同文件不同位置自动合并、同位置冲突不触碰主工作区、冲突标记拒绝、计划摘要随主工作区变化、应用日志每一步崩溃后的恢复与恢复期间主工作区分叉；`internal/session/sqlitestore` 覆盖合并等待 Task、Task 转走后拒绝、审批绑定计划、无法决定的执行器保留合并、冲突经替代合并解决与日志续提交；`internal/agentmanager` 端到端覆盖两个快照子 Agent 并行写入后合并、依赖任务在两个合并都提交后才启动、冲突由自动派发的 Resolution Task 在带冲突标记的视图中解决、审查 Agent 在合并前以只读方式看到候选及其 diff；`internal/app` 覆盖快照存储接线和写入屏障与共享写入互斥；TUI 覆盖合并审批卡片直接展开计划 diff。涉及的包都通过 `-race`。快照视图位于 `<DataDirectory>/views`，屏障是整个工作区的写租约，最长等待 6 分钟以覆盖最长的 Shell 调用。交互模式下合并审批走 TUI，无头模式默认拒绝，评估模式的默认白名单包含 `merge_apply`；没有审批方且未开启 `auto_merge` 的执行器（例如 `control` 子命令）不处理快照合并，合并保持排队。

### P3 交互与回传

- 内容：`/agents` 面板、对话记录查看、后台输入类型与唤醒、邮箱消费账本、Turn 内结果注入。
- 主要文件：`internal/tui/*`（面板在 `agents_panel.go`、`activity.go`，唤醒在 `background.go`）、`internal/cli/root_conversation.go`、`internal/conversation/{run,background}.go`、`internal/agentruntime/{turn,request_context,background}.go`、`internal/agentmanager/{supervisor_mailbox,activity}.go`、`internal/session/sqlitestore/mailbox_ledger.go`。
- 验收：面板可以查看和中断子 Agent、审查合并；根 Agent 空闲时，子 Agent 完成能唤醒它；Turn 提交后、确认前崩溃，重启不会重复唤醒；辅助请求中不出现注入内容。
- 状态（2026-10-25）：已完成。`internal/session/sqlitestore` 的测试覆盖账本的认领、确认与释放，以及丢弃快照合并后 Task 失败、候选保留、主工作区不变；`internal/agentruntime` 覆盖后台输入按记录位置重放、辅助请求不含注入内容、子 Agent 的 Factory 不带账本、对话记录包含未完成 Turn 且不触发校准；`internal/cli` 覆盖根 Agent 在 Turn 中收到子 Agent 结果并在提交后确认、Turn 失败时消息保留、唤醒 Turn 消费结果、提交后确认前崩溃的 Turn 在重启时结算而不再唤醒；`internal/agentmanager` 覆盖实时活动叠加（Run 结束后清除）与用量汇总；TUI 覆盖唤醒条件、取消后抑制、跳过的唤醒不留痕迹、`auto_continue` 关闭时不唤醒、面板摘要与 token、两次确认的中断与丢弃、审查合并、打开与关闭对话记录、未聚焦时字母交给输入框。涉及的包都通过 `-race`。会话库单连接在面板轮询下的竞争仍未单独压测；面板查询走 Supervisor 的只读投影，默认刷新间隔 2～5 秒，暂不增加只读连接池。用真实模型（`gpt-5.6-sol`，Responses 接口）在 TUI 中端到端验证过：两个只读子 Agent 并行运行并在同一 Turn 内送达；根 Agent 派出子 Agent 后结束 Turn，空闲时被结果唤醒并作答；重开该 Session 不会再次唤醒；`/agents` 面板显示状态、实时活动与 token，连按两次 `x` 中断运行中的子 Agent，`Enter` 打开其对话记录、`Esc` 返回。被中断子 Agent 的取消结果同样会唤醒根 Agent。

### P4 共享目录并行写（可选）

- 内容：第 13 节全部内容。
- 验收：第 13 节的每个条件都有对应用例；两个拥有不相交路径的子 Agent 同时写文件、执行 Shell；越界写入在工具层和操作系统层都被拒绝。

与审计建议的顺序相比，交互（P3）放在共享写（P4）之前：快照写入需要合并审查界面才可用，而 `shared` 是可选优化，放在最后影响最小。

### P0–P3 遗留项（2026-10-26 核对）

P0–P3 的验收条件均已满足，`go test ./... -count=1` 于 2026-10-26 全部通过。该结论属于当日计划记录，不能替代当前验证；下列设计条目尚未落地，详见对应章节：

- Git worktree 未并入统一合并引擎：仍用 `git apply --3way` 改动主工作区，worktree 仍位于工作区内，没有内容基线（第 7.3、8 节）。
- 合并提交只持有整个工作区的屏障，没有逐路径批量租约。锁内 preimage 校验失败时直接判冲突，不重新生成计划。审批绑定只在单次执行内有效（第 8.2、8.3 节）。
- 审查结论没有记录候选摘要；合并后验证由可选的 `post_merge_check` 命令和持久回执绑定内容/检查配置摘要，尚无额外审查 Agent（第 9 节）。
- 没有增量扫描提示，清单同步计算（第 7.4 节）。
- 所有权租约、`revoking` 状态和 epoch 校验推迟到 P4（第 5.3 节）。
- 缺少的测试：保留 mtime 的同长度修改、删除/权限/符号链接冲突、锁内重新校验失败、采集期间并发写入被屏障阻止（现有测试只验证屏障与租约冲突）、审查结论绑定（第 16 节）。

## 16. 测试计划

- 协调：不同 DataDirectory、不同 `GSEA_HOME` 的实例打开同一工作区；持有进程被杀死后的租约回收；撤销时等待运行中的 Shell 退出。
- 快照与合并：采集期间的并发写入被屏障阻止；内容哈希能发现保留了 mtime 的同长度修改；三方合并的冲突策略（重叠编辑、同一位置的不同插入、二进制、删除、权限、符号链接）；ApplyPlan 每一步的崩溃注入；锁内重新校验失败时计划作废。截至 2026-10-26，已覆盖的有：重叠编辑、同位插入、二进制冲突和每一步的崩溃注入。其余几项见第 15 节的遗留项。
- 交付：依赖任务在合并提交前不能认领；审查结论绑定候选摘要或合并版本。
- 邮箱：同一 Turn 的多次模型请求不重复注入；Turn 提交后、确认前崩溃不重复唤醒；`compaction`、`goal_assessment` 请求中不注入。
- 并发：Flow 并行分支对同一文件的写入互斥；对 `agentmanager`、`agentruntime`、`codingtools`、`workspacecoord`、`workspacedomain` 运行 `go test -race`。
- 沙箱：在 macOS 和 Linux 上分别验证写入范围、硬链接、rename 和大小写，沿用 `internal/sandbox/isolation_test.go` 的写法。
- 端到端：用评测框架在非 Git 临时目录中运行多 Agent 任务。

## 17. 风险与待验证项

- 协调依赖本机 flock：`GSEA_HOME` 或汇合目录位于网络文件系统时，flock 可能失效或只在本机有效，因此不支持多台主机共享同一个 `GSEA_HOME` 协调同一工作区。
- 嵌套工作区（一个实例打开 `/repo`，另一个打开 `/repo/sub`）属于两个协调域，彼此不互斥。
- Shell 使用整个工作区的操作租约：多个会话在同一工作区执行长命令时会相互等待，超过 10 秒返回冲突。快照模式（P2）下子 Agent 的 Shell 在视图中运行，不再占用主工作区。
- 脱离进程组、又继承了后代锁的守护进程，会让已崩溃的实例一直被视为存活，直到守护进程退出；错误信息会给出实例的 PID 以便排查。
- 普通复制模式要同时保存视图和基线，成本约为工作区大小的两倍，依靠复制上限控制；屏障期间主工作区不能写入，普通复制时屏障可能较长。
- Seatbelt 的规则优先级和硬链接行为，直接决定 `shared` 能否开放。
- 并发数越高，Provider 限流压力和 token 成本越大，依靠模型分桶控制。
- 单连接 SQLite 在高并发下可能成为瓶颈，需要实测。

## 18. 默认值（已确定）

- `max_running = 4`
- `default_workspace = "auto"`：只读 Profile 用 `readonly`，其余用 `snapshot`。Git worktree Profile 保持 `git-worktree`，没有快照存储时退回 `shared-lease`，见第 6 节。
- `auto_merge = false`
- `auto_continue = true`，并限制唤醒频率
- `shared_writes = false`（P4 键，尚未实现）
- `snapshot_copy_limit = "1GiB"`
- 操作租约的等待上限为 10 秒（`operationLeaseWait`，`internal/codingtools/workspace.go`）

## 2026-10-29 动态任务、恢复结算与 Execution admission 增量

动态计划阶段现已落地：`task_update` 提供执行前版本化编辑，`task_refine` 以 v40 `task_refinement_members` 持久化原子细化成员，`task_reassign` 对可信 iteration-limit 失败执行有副作用证明的有限重试。普通 `ParentTaskID` 仍是分组关系，自动派发绑定 Task version，细化子任务从 draft 开始，必须显式转为 ready。

恢复阶段新增 completion receipt 的摘要、JSON 契约和 RunID 校验；worker 返回后先冻结结果，Task/交付/通知/资源结算失败只进入 pending 结算队列，不重新运行 worker。父细化成员未完成时释放已结束 worker 的 lease、保留 delivery view，子任务完成后恢复扫描补结算。已确认模型拒绝、已知工具错误、wait checkpoint 后的后续响应都按 journal/checkpoint marker 重放；未知副作用继续 fail closed。

Execution admission 现由 durable token 保护：`reserved → turn_started → execution_started → consumed` 使用 CAS 和输入/契约/配置摘要校验；缺失 Turn 或 Execution/Step 绑定的 token 进入 `unavailable`，恢复不会降级为普通聊天。
