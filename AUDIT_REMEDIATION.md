# A–E 审计复核与修复记录

日期：2026-10-26。对应 `CODE_READING_GUIDE.md` 原始 A–E 工作单，以当前工作区代码为准。本记录区分已修复、原有实现已满足、部分完成和明确遗留，不能把原始静态推断全部当作已复现故障。

## 实施范围

本轮修复安全边界、回合与合并的持久性、调度与 UI、SQL 存储和插件执行资源限制。保留 Eino 负责 Agent Loop 的架构；未实施暂放一边的 `TOOL_DISCOVERY_DESIGN.md`，也没有启动大范围代码重构。

关键代码入口：`internal/tool/schema.go`、`internal/codingtools/root_io.go`、`internal/workspacecoord/finalization.go`、`internal/session/sqlitestore/merge_apply_finalization.go`、`internal/tui/audit_regression_test.go`。新增 Schema 验证依赖固定为 `github.com/santhosh-tekuri/jsonschema/v6 v6.0.3`。

## A. 安全边界

| 编号 | 结论 | 当前行为与证据 |
|---|---|---|
| A.1 | 已修复 | 项目 `.gsea/config.toml` 默认拒绝加载，返回 `ErrProjectConfigUntrusted`；宿主可用 `LoadOptions.TrustProjectConfig` 或本次启动环境变量 `GSEA_TRUST_PROJECT_CONFIG=true` 显式信任。仓库文件不能给自己授信。用户配置与 profile 不受此门槛影响。配置只读常规文件，最多 1 MiB。见 `internal/config/trust_test.go`。 |
| A.2 | 已修复 | `MergeExecutor.RunPending` 在所有调用入口统一执行 Approver/AutoMerge 门禁；control 调用不再因缺 Approver 而自动获准。等待审批不占用整个 executor 锁，Discard 可取消该等待。 |
| A.3 | 已修复 | 绑定发布时编译 Schema，Pipeline 在策略、审批和执行前校验实参；拒绝不合法类型、额外字段、范围、长度等。禁止 Schema 外部 URL/文件加载；参数与 Schema 自身有大小上限。错误成为可恢复的 `invalid_arguments`。见 `internal/tool/schema_test.go`。 |
| A.4 | 已修复 | 结构化 write/edit 用 `os.Root` 读取和原子替换，叶子符号链接拒绝写入，避免读取外部文件后经 diff 泄漏；mode 在执行器直接校验，不能只依赖 Schema。见 `internal/codingtools/audit_security_test.go`。 |
| A.5 | 已修复原指名路径；补充生产采集加固 | evaluation 在交给 Agent 前创建独立、宿主管理的 bare capture，使用 `--no-hardlinks --dissociate` 保有自己的基线对象；Patch 在只读工作树沙箱中使用该 capture 和私有 index/object 目录，忽略 Agent 改写的 `.git`，没有不安全 fallback。生产 worktree 的 capture 也改用宿主临时 sterile metadata/index/objects，原 view 与源 object metadata 只读，仅 scratch 可写；普通 Git runner 不再继承全部宿主环境。见 `internal/evaluation/audit_security_test.go`、`internal/workspacecoord/git_capture.go`。checkout 的残余见下方。 |
| A.6 | 已修复 | LSP 经 OS 沙箱运行，只读 workspace、无网络、私有临时目录和白名单环境；stdin 写入有 2 秒上限，超时关闭客户端；协议头总量最多 16 KiB，消息体最多 8 MiB，重复 Content-Length 拒绝。见 `internal/lsp/client.go`、`protocol_security_test.go`。 |
| A.7 | 部分完成 | 环境改为白名单，DATABASE_URL、loader 配置、SSH command/socket 等任意宿主变量不再透传；增加只读 workspace 请求及测试。macOS 仍全局允许文件元数据读取，Linux namespace 扩展及更强进程隔离不在本轮完成范围；不能据此声称所有沙箱逃逸都已排除。 |
| A.8 | 部分完成 | `go version` 移入编译沙箱；哈希覆盖 go driver、pkg/tool、stdlib src、include、lib 和配置，编译前后检查。证据明确记录哈希范围及 `GOMEMLIMIT` 仅为软提示。当前使用受信任的宿主 Go 1.27.0，仍未提供不可变捆绑工具链或 OS 级进程 RSS 硬配额。见 `internal/pluginbuild/toolchain.go`。 |
| A.9 | 已修复 | 终端展示经有界、跨流块的 ESC/OSC/DCS/CSI 清洗，工具 JSON 解码后再清洗，原始 JSON 事件协议不变；文件链接只在 workspace 根句柄内 Stat。见 `internal/render/plaintext.go`、TUI/CLI 展示回归。 |
| A.10 | 已修复 | 纯策略/审批拒绝作为可恢复工具结果；与持久化故障合并的错误仍终止回合。Eino adapter 必须拿到持久调用 ID，不随机补 ID。见 `internal/einoadapter/audit_regression_test.go`。 |
| A.11 | 已修复 | ResourcePrefix 按完全相等、目录分隔符或显式 `:` 前缀匹配，`/workspace` 不匹配 `/workspace2`，`tool:read` 不匹配 `tool:read_all`。见 `internal/policy/policy_test.go`。 |
| A.12 | 已修复 | Artifact 只接受规范小写 `sha256:<64hex>`，所有对象 I/O 用 `os.Root`；Put 校验已有对象、同步文件及目录；ReadSegment 同一遍流式读取校验完整内容哈希，只保留请求片段。拒绝任意本地相对引用。见 `internal/artifact/audit_security_test.go`。 |
| A.13 | 部分完成 | WASM 同时调用默认最多 4 个（可配置 1–64），排队计入调用超时，关闭唤醒等待者；guest 错误最多 4096 字节，返回 blob 由宿主复制后释放，guest 不能释放宿主请求 blob。限制线性内存和活跃 blob 总量，但不构成编译缓存、宿主 JSON/GC 的总 RSS 硬上限。 |
| A.14 | 已修复实际缺口 | 委派 Limits 与父级逐项取交集，Profile 支持 snapshot 且不会被全局 shared 默认降级。shared 父级向隔离 worktree 委派本身不等于扩大写权限，不应机械禁止；实际根目录和权限仍由宿主 workspace view 决定。见 `internal/agentprofile/audit_regression_test.go`。 |

## B. 崩溃一致性与正确性

| 编号 | 结论 | 当前行为与证据 |
|---|---|---|
| B.1 | 已修复 | 完成事件 Append 失败走失败/恢复路径；完成事件持久化后，checkpoint/mailbox 清理在有界收尾上下文中进行，不把已提交副作用误判成可重做。清理仍可由恢复流程补做。 |
| B.2 | 已修复 | `turn.failed` 与恢复 checkpoint 进入同一批原子 Append，避免两次独立提交的窗口。见 `internal/agentruntime/audit_regression_test.go`。 |
| B.3 | 已修复可处理边界 | main 接收 SIGINT 和 SIGTERM；`session show/remove` 对 running 记录检查 PID 并 CAS 修复已死亡进程。SIGKILL 不能靠 Close 处理；未知 PID、权限不足和复用的活 PID 保守保留 running。见 `internal/sessionregistry/process_unix_test.go`。 |
| B.4 | 已修复 snapshot 收尾与审批窗口；Git 提交仍有差异 | 新增持久 MergeFinalization，绑定候选/patch/plan、主工作区内容摘要及检查配置摘要；验证的 prepared→dispatched→completed/unknown 与 Task/view 收尾分离，恢复不重复执行结果未知的检查命令；冲突 Resolution Task 的创建/链接/派发可收敛；等待审批可丢弃。原生 Git apply 仍未采用 snapshot 的逐路径 apply journal。 |
| B.5 | 已加固 | 条件更新统一核验 RowsAffected，版本冲突时不发布成功事件；修复 claim/dependency 忽略影响行数错误。原有单连接事务不能直接当作已发生并发丢失的证据，本项是明确 CAS 契约与防御性加固。 |
| B.6 | 已修真实问题，部分原判断不成立 | WaitSet 批量取消、Flow Session 绑定纳入事务并检查 Team 所有权；FinishPluginBuild 原本为单条 CAS，不需要另套事务；不可变内容寻址记录写入后回读、FindSpawn 两步读取不能仅凭“多步”认定漏洞。见 `internal/session/sqlitestore/audit_store_test.go`。 |
| B.7 | 已修复 | 追加 v37 finalization 和 v38 verification digest，校验和 fixture 覆盖 v1–v38，数量必须一致；旧版 DDL 未改写。升级和固定哈希测试覆盖 v0–v38。 |
| B.8 | 已修复 | turnDone 与事件按序进入同一队列，使用 App 生命周期投递，不随被取消的 Turn 丢失；App 退出仍可释放等待。见 `internal/tui/audit_regression_test.go`。 |
| B.9 | 已核实并加固 | TUI 在取消导致退出时返回 ctx.Err，使 CLI 能识别取消并映射 130，保留其他真实错误。 |
| B.10 | 已修复 | 只对明确旧模型别名选 p50k，不再把任意含 codex 的现代名字报告为精确；懒加载不会覆盖供应商校准后的状态。见 `internal/tokenizer/audit_regression_test.go`。 |
| B.11 | 已修复 | comparator 错误/取消写入带有界 Failure 的比较报告，失败报告不能激活策略；已有 fixture 失败本来就有完整报告。见 `internal/strategy/audit_regression_test.go`。 |
| B.12 | 已修复 | Stream/Generate 统一先持久化 assistant 与 settlement，再校准供应商输入用量，消除首次计数顺序差异。 |
| B.13 | 已修复计价；阈值差异是有意设计 | reasoning 是 output 的子集，单独价格只替换该子集价格，不重复相加；拒绝非法值和溢出，预留按最坏输出/无缓存情形。Reserve `>` 允许恰好用完预算，CanAdmit `>=` 拒绝耗尽后的新请求，二者职责不同。见 `internal/tokenusage/audit_regression_test.go`。 |
| B.14 | 已修复 | Goal BindTurn 使用 atomic pointer 保存绑定，消除无同步读写。见 `internal/agentgoal/audit_regression_test.go`。 |
| B.15 | 已补全本轮缺口 | 除 allow-task-verify 外，Supervisor 校验本 Task 当前 attempt 指派的 reviewer、Task 状态与 expected version；同 Profile 的其他 Agent 或旧 attempt reviewer 均拒绝。负面结论可回到 ready 并递增 attempt。 |
| B.16 | 已复核并加固 | git-worktree 自动 artifact/入队路径保留；同候选替换和 supersede 身份受限；待处理/运行/冲突候选冻结后拒绝 Continue/reuse，避免审查期间被同一 Agent 改写。 |
| B.17 | 已复核并补全 | 价格/Team budget/control 事件分页沿用已有修复；非 active Team 不允许 queued/recovered dispatch，Continue 同样拒绝；运行状态提交与取消共享 admission 门禁。合并后检查增加持久身份与不重复执行保障，仍有下节 Team 生命周期限制。 |

合并后检查在整个 workspace 屏障内、只读沙箱中执行；检查前后核对内容身份，继承 descendant liveness 锁。失败保留主工作区改动并将 Task 标 failed，不解锁依赖。若进程在检查执行后、记录结果前死亡，恢复将其标为 unknown/failed，不自动重跑任意外部命令；这保证不误报验证通过，并非 exactly-once 外部命令协议。

## C. 资源与性能

| 编号 | 结论 | 当前行为与边界 |
|---|---|---|
| C.1 | 已修复指名读取路径 | read/edit 最多读取 4 MiB 结构化内容，扩大替换在分配前计算并拒绝；大文件 read 返回有界前缀且 `fingerprint_complete=false`、无完整 SHA。artifact 分段保留内存；bridge manifest 读常规文件时先限 4 MiB；LSP 头 16 KiB；render fence 待决前缀 4096 字节，超限按文本输出。 |
| C.2 | 已修复已知平方路径 | textarea selection 的逐列探测改为单次 span 布局；Unicode、emoji、空白与真实 textarea 对照回归通过。100k ASCII 的局部基准约 19.5ms/op（3 次采样），不宣称病态超长组合字符也严格线性。 |
| C.3 | 部分完成 | SnapshotByID 不再持注册表锁计算所有快照 ID；历史 tool/profile revisions 仍保留。安全回收需要同时考虑持久 checkpoint、Tool outcome 和恢复引用，不能简单裁剪 map。 |
| C.4 | 保留待实施 | retention/plugin collection 的扫描与 preservation CAS 内的 O(n) 工作仍存在。后续应引入可证明完整的引用索引/增量计划，并测量规模后再改变扫描契约。 |

## D. 错误语义与一致性

| 编号 | 结论 | 当前行为与边界 |
|---|---|---|
| D.1 | 已修指名缺失记录路径 | Goal/Flow/Build/Test/Strategies/StageSettlement 和 plugin activation 缺失目标，转换为可 errors.Is 的领域 NotFound，不向外泄漏 sql.ErrNoRows；不把所有业务输入错误强行归为一个哨兵。 |
| D.2 | 已修复 | 旧空 hash 按原持久字段比对，不能自动视为匹配；SavePluginTest 同内容幂等、异内容冲突且校验原内容；merge Supersedes 进入身份且限制作用域。仅兼容可由旧证据核实的请求，极旧数据缺关键字段时会拒绝，未承诺任意旧版本无感重放。 |
| D.3 | 已修复运行错误路径 | Registry 有 nil/状态/PID 校验及 ErrNotFound；先占 DB 身份再创建资源，失败只反向移除本次新建的空目录，不删用户数据或改已有目录权限。进程在 mkdir/DB commit 之间死亡仍可能留下空孤儿目录，文件系统与 SQLite 不构成同一事务。 |
| D.4 | 已修复名称与契约 | 调度器正式命名 RoundRobin，WeightedRoundRobin 保留兼容别名；并发桶 Release 返回是否实际释放，可识别重复释放。没有假装实现权重或关键路径优先级。 |
| D.5 | 已修复 | policy/approval 审计事实用 WithoutCancel + 5 秒限时持久化；实际授权、执行仍遵循调用上下文，取消不能扩大权限。见 `internal/tool/audit_durability_test.go`。 |
| D.6 | 已修复 | profile 支持长短名与单/双横线及等号形式，eval 保护 flag 识别单横线；control 返回 Close 错误，TUI 取消保留可判定错误。见 `internal/cli/audit_flags_test.go`。 |
| D.7 | 保留并说明 | 未删除来源不明的 `cmd/local-agent/approval-test.txt`；遗留 lease 表保留在已发布迁移历史中，当前协调路径不依赖它们。清理迁移历史会破坏旧库协议，不能当普通死代码删除。 |

## E. 开放问题逐项结论

| 问题 | 核实结果 |
|---|---|
| 恢复父级如何读 MessageResult | 已有 durable mailbox、root 消费账本与 SettleBackground；openRootConversation 在打开会话时结算已提交 Turn，再把结果注入后续主请求/后台 Turn。见 `internal/cli/root_conversation.go` 及测试。 |
| reviewer 是否只读 | Supervisor 把候选路由到 ViewSharedReadonly；codingtools 的写入口检查 ensureWritable，shell/LSP 按实际 view 的沙箱权限运行。另补了指定 reviewer/attempt 校验及候选冻结；不声称能阻止用户从其他程序改 worktree。 |
| human_confirm 真正的门 | app 的 approve-human-confirm 策略为 require_approval；并非模型自行调用工具即获得确认。 |
| Blocked→Ready 与 supersede | 前者原有依赖检查及事务内解锁；后者已限制为同 Task、明确 resolution 链或同无 Task 的 Agent/view，禁止无关同 Team 替换。 |
| Lease trim | 原实现已按规范化路径存储，新增回归确认，不是新修漏洞。 |
| Team/Approval/Lease 状态 | Team 已有状态机且本轮补 dispatch/Continue 门禁；Lease 具有 CHECK 与受控生命周期；不存在需要补迁移表的任意可变 Approval SQL projection。 |
| 无 Close 是否泄漏 | Registry/Pipeline 自身不持数据库或独立后台进程；底层 DB、WASM、Supervisor、LSP、Coordinator 的资源由实际 owner 关闭，Artifact 每次操作打开/关闭根句柄。不为不持资源的对象增加空 Close。 |
| evalVerifier 与计分入口 | CLI 的测试注入项可 nil，Runner 在 nil 时使用真实检查实现；CLI 确实调用 ApplyHarborProtocolMetadata、ApplyTerminalBenchAudit，原“无人调用”推断不成立。本轮没有重新下载或运行外部 benchmark。 |

## 兼容性变化

1. 检查项目配置后，若要本次启动加载它，使用 `GSEA_TRUST_PROJECT_CONFIG=true go run ./cmd/local-agent chat ...`。默认用户配置和环境/CLI 仍正常加载；尚无持久化项目授权 UI。
2. 工具参数现在真实遵守 JSON Schema。模型传入未公开的 `owner_session` 等字段会在执行前拒绝；修正后的请求必须使用新的 CallID，不能用同一 ID 修改参数。
3. 结构化 write/edit 的输入与输出上限为 4 MiB。更大的文件应使用受审批的有界处理命令；read 不再为超限内容提供貌似完整的指纹。
4. Artifact 仅读取宿主生成的 SHA 引用，不支持任意路径 alias。现有 Put 生成的引用保持兼容。
5. 沙箱不继承任意宿主环境变量；依赖外部配置、网络或凭据的命令需要由宿主明确设计能力入口，不能恢复全环境透传。
6. 生产补丁采集不执行仓库自定义 clean/process filter。二进制、新建/删除、忽略文件、原 index、本地 exclude/autocrlf 行为有回归覆盖；依赖可执行 filter 的 LFS 等特殊发布语义需要后续显式可信适配，不保证透明兼容。
7. `workspace_read` 元数据升级为 builtin-v3，write/edit 升级为 builtin-v5；Goal 文件验证器明确兼容 read v2/v3 的 content/truncated 契约，仍拒绝未知版本与截断内容，不凭新版元数据绕过机械验收。

## 仍需单独实施的边界

- Git-patch 仍用 `git apply --3way`，未纳入 snapshot 的逐路径 journal；冲突可能在主工作区留下冲突标记，不具备统一原子合并契约。
- 通用宿主 Git 操作尚未整体沙箱化。已确认创建 worktree 的 checkout 可受原仓库 `.gitattributes` 加本地 `filter.*.smudge/process` 配置影响并触发宿主命令。过滤环境和禁 hooks/fsmonitor 不能消除该风险；需要可信元数据驱动、只给新 view 与必要 Git 管理目录写权限的生命周期路径，并一并复核 status/apply/清理等操作。本轮只关闭补丁采集的同类路径，不能宣称全部 Git 执行安全。
- `team.cancel` 停止子 Agent 和新的准入/后台合并轮次，保留 root 会话；已在等待审批的后台 merge 尚未获得独立的 per-Team 取消/等待生命周期。draining/completed/failed 有状态定义，但当前没有会话级生产收敛调用方。
- 合并后检查是宿主配置的命令，不包含第二轮审查 Agent；unknown 检查的显式人工恢复/新验证流程仍需产品设计。
- C.3/C.4 历史引用安全回收与增量扫描、A.8 编译进程硬内存限制/不可变捆绑工具链、A.13 宿主总 RSS 仍未完成。
- 本轮验证环境为 macOS arm64、Go 1.27.0；没有据此宣称 Linux bubblewrap 的全部隔离行为已验证。

## 验证记录

最终验证结果：

| 检查 | 结果 |
|---|---|
| `go build ./...` | 通过 |
| `go vet ./...` | 通过 |
| `go test -count=1 ./...` | 全部通过，包含普通 plugin 全套、真实沙箱 Git 捕获和 Goal 自动验收 |
| `gofmt -l`（cmd/internal/sdk 全部 Go 文件） | 无输出 |
| migration hash 与升级 | v1–v38 固定哈希、v0–v38 升级测试通过，未修改已发布 DDL |
| 相关包 `go test -race -count=1` | 下列范围全部通过；最后改动后另复验 conversation/agentruntime/codingtools/workspacecoord/agentmanager |

Race 范围：`tool`、`artifact`、`codingtools`、`config`、`lsp`、`evaluation`、`sessionregistry`、`sandbox`、`cli`、`app`、`modelgateway`、`agentflow`、`conversation`、`approval`、`toolbridge`、`evolutiontools`、`policy`、`session/sqlitestore`、`agentruntime`、`agentprofile`、`agentgoal`、`einoadapter`、`tokenusage`、`tokenizer`、`strategy`、`tui`、`render`、`scheduler`、`plugin/wasm`、`pluginbuild`、`workspacecoord`、`agentmanager`；实施阶段的 `workspacedomain`/`orchestration` race 亦通过。

测试使用本地脚本模型、临时 SQLite/workspace 与沙箱，不请求付费模型服务；没有下载外部 benchmark。普通受限外层环境无法执行本地套接字及嵌套沙箱的测试，因此集成测试在获准的外层执行环境中运行，产品自身的沙箱仍正常启用。

测试通过只能证明覆盖到的行为，不能替代以上未实现边界。插件普通全套、WASM/pluginbuild race 已在实施阶段通过；整个 plugin 包的 race 曾因反复编译 WASM 过慢中止，不计为通过，也不计为失败。
