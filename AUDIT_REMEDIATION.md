# A–E 审计边界与整改状态

更新：2026-11-20。保留原问题编号，记录当前控制与遗留边界；不把静态推断当作已复现故障，也不沿用过去的测试通过结论。整体源码清理正在进行。

业务权威为 ATE native Session 与 authority records。`internal/runtimestore` 只保存运行资源和 pending 请求，不含业务历史或 outbox。旧 SQL 业务表、迁移和日志不再作为整改方案或兼容性要求；Agent Loop 仍由 Eino 承担。

## A. 安全边界

| 编号                    | 当前控制                                                                                                       | 限制与入口                                                                                |
| ----------------------- | -------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------- |
| A.1 配置信任            | 项目配置默认拒绝；宿主通过 TrustProjectConfig 或 `GSEA_TRUST_PROJECT_CONFIG=true` 显式授信；常规文件及大小限制 | `internal/config`；尚无持久项目授权 UI                                                    |
| A.2 合并授权            | MergeExecutor 统一执行 Approver/AutoMerge 门禁，control 调用不等于审批；Discard 可取消等待                     | `internal/workspacecoord`；per-Team 后台 merge 生命周期待补齐                             |
| A.3 工具参数            | 绑定时编译 Schema，策略/审批/执行前校验，拒绝外部 Schema 加载和超限参数                                        | `internal/tool/schema.go`；invalid_arguments 可恢复                                       |
| A.4 文件写入            | `os.Root`、原子替换、拒绝叶子符号链接；执行器校验 mode                                                         | `internal/codingtools`；结构化内容上限 4 MiB                                              |
| A.5 补丁采集            | evaluation 独立 bare capture；生产采集使用宿主管理的 metadata/index/objects 和受限环境                         | `internal/evaluation`、`internal/workspacecoord/git_capture.go`；不代表 checkout 全部安全 |
| A.6 LSP                 | 只读 workspace、无网络、白名单环境与 OS 沙箱；stdin 超时、协议头/消息体限额                                    | `internal/lsp`                                                                            |
| A.7 沙箱环境            | 不透传任意宿主变量，支持只读工作区                                                                             | `internal/sandbox`；macOS 元数据范围、Linux 实机隔离仍需专项核验                          |
| A.8 工具链（历史项）    | 原编号跟踪旧工具生成方案的编译环境                                                                             | 归档于早期设计                                                                            |
| A.9 终端展示            | 有界跨流块控制序列清洗；原始 JSON 协议不变；链接限于 workspace 根                                              | `internal/render`、TUI/CLI                                                                |
| A.10 工具拒绝           | 纯 policy/approval 拒绝可继续处理，持久化错误终止 Turn；不随机补调用 ID                                        | `internal/einoadapter`                                                                    |
| A.11 资源前缀           | 完全相等、目录分隔符或显式冒号前缀匹配，防相近字符串越权                                                       | `internal/policy`                                                                         |
| A.12 Artifact           | 规范小写 SHA 引用、根句柄 I/O、同步/校验、分段读取核验完整内容                                                 | `internal/artifact`；不接受任意本地路径 alias                                             |
| A.13 扩展隔离（历史项） | 原编号跟踪旧 WASM 方案的运行隔离                                                                               | 现行 Shell 隔离见 A.7                                                                     |
| A.14 委派权限           | Limits 与父级取交集，Profile/view 限制写权限，支持 snapshot                                                    | `internal/agentprofile`；shared 父级向隔离 view 委派本身不等于扩权                        |

## B. 崩溃一致性与正确性

记忆工具的审计要能回答三个问题。候选来自本会话的哪些原始事件？复用时的来源和资格是什么？哪次获准的调用写了采用或撤回记录？`memory_query` 的只读回执与 `memory_manage` 的变更回执分别核对。来源纠正后，旧事件仍可用于追溯，披露与再次复用遵守 ATE 当前策略。[工具契约](internal/app/memory_tools.go) · [来源维护](internal/experience/tools_manage.go) · [采用记录](internal/experience/tools_adopt.go)

| 编号           | 当前行为及边界                                                                                                                        |
| -------------- | ------------------------------------------------------------------------------------------------------------------------------------- |
| B.1 完成收尾   | 完成事件失败进入失败/恢复路径；已提交结果不因后续清理失败变为可重做。Session 完成事实与本地资源清理分别处理。                         |
| B.2 Turn 失败  | `turn.failed` 和 checkpoint 事实同次 native Session append；opaque checkpoint 字节另存。策略 Fallback 是后续独立 authority 提交。     |
| B.3 进程退出   | SIGINT/SIGTERM 处理；注册表核对 PID 并 CAS 修复死亡进程。SIGKILL 不能靠 Close，未知 PID/权限/复用保守处理。                           |
| B.4 合并恢复   | native apply 进度与 finalization 固定候选、内容、检查配置，收尾可重试；snapshot/Git apply 仍有差异。dispatched 检查未知时不自动重跑。 |
| B.5 并发更新   | 当前使用 authority CAS、expected version、状态与所有者校验；不再以旧 SQL RowsAffected 实现作为业务仓储索引。                          |
| B.6 跨对象提交 | WaitSet 取消、Flow 绑定等由 native 仓储维护 Team 所有权和提交边界；不能只凭多步读取判定一致性漏洞。                                   |
| B.7 格式       | OpenRuntime 校验当前资源格式并拒绝旧业务库/混合格式；旧业务升级 fixture 不属于 native 验收。                                          |
| B.8 UI 顺序    | turnDone 与事件按序排队，投递使用 App 生命周期，取消 Turn 不应丢失已提交结果。                                                        |
| B.9 取消返回   | TUI 保留可判定取消供 CLI 映射 130，同时保留真实错误。                                                                                 |
| B.10 Tokenizer | 明确模型别名匹配，不按含 codex 的任意名称声称精确；懒加载不覆盖供应商校准。                                                           |
| B.11 策略比较  | 比较错误/取消写失败报告，失败报告不能晋升；证据由 `internal/evolutionstore` 持久化。                                                  |
| B.12 用量顺序  | Stream/Generate 持久化 assistant 和 settlement 后再校准输入用量；Session 与用量 authority 仍是独立边界。                              |
| B.13 计价预算  | reasoning 为 output 子集，不重复计价；最坏输出预留，拒绝非法值/溢出。Reserve 与 CanAdmit 对恰好耗尽额度的判断职责不同。               |
| B.14 Goal      | atomic pointer 保护内存绑定；持久 admission token 记录阶段。Goal/首轮可同 authority batch，Session `turn.started` 另提交。            |
| B.15 reviewer  | 校验权限、当前 attempt 指派、Task 状态和 expected version；同 Profile 的其他 Agent/旧 reviewer 无权替代。                             |
| B.16 候选      | git-worktree 自动 artifact/入队；supersede 限定身份；冻结后限制 Continue/reuse。用户从其他程序修改 view 仍需内容核验。                |
| B.17 准入结算  | 非 active Team 阻止准入/Continue；completion receipt 冻结结果。Agent 终态和父级消息同 batch，Task/交付/收尾并非全部同一事务。         |

恢复必须保留以下约束：

- exact 信封在发送前记录 attempted。已尝试但无确定结果时，查询不到仍为 unknown，不自动重发或更换 key；已提交但清理失败只补清理。
- App 按 ATE store identity + Session ID 持有执行锁；Supervisor 仅恢复所属 Team 的 reservation、claim 和 completion。共享历史可见不等于全局执行权。
- Execution admission 通过 Prepare、Turn/Execution/Step 绑定、Consume 逐段推进；恢复核对 token/摘要，缺证据进入 unavailable。
- 工具 started 后结果未知不自动重做；模型及已确认工具结果按原身份复用。Session 失败记录和策略 Fallback 分别可能失败，一侧成功不能证明另一侧成功。
- 归档先取得 native 关闭回执，再释放本地工具快照引用。阶段间失败按原 plan_id 补做，离线维护受运行库所有权及会话锁约束。

## C. 资源与性能

| 编号 | 当前控制与剩余边界                                                                                       |
| ---- | -------------------------------------------------------------------------------------------------------- |
| C.1  | read/edit、artifact 分段、bridge manifest、LSP 协议、render 待决前缀有界；截断 read 不提供完整文件 SHA。 |
| C.2  | selection 逐列探测已改为 span 布局；本次未测性能，不沿用旧毫秒数，也不声称病态 Unicode 严格线性。        |
| C.3  | SnapshotByID 避免锁内计算全部 ID；历史 tool/profile revision 保留，不能只裁剪内存 map。                  |
| C.4  | 归档按保留计划和关闭回执释放快照引用。扫描仍可能 O(n)，完整引用索引和增量计划待实施。                    |

## D. 错误语义与一致性

| 编号 | 当前结论                                                                                                             |
| ---- | -------------------------------------------------------------------------------------------------------------------- |
| D.1  | 领域仓储使用可判定 NotFound/Conflict 等错误，不把所有业务输入错误归为一个哨兵；旧 SQL 错误映射不再作为当前代码索引。 |
| D.2  | 幂等依赖原身份、摘要和内容证据；缺证据拒绝，不补猜旧 hash、不承诺旧版本无感重放。                                    |
| D.3  | 注册表与初始化校验身份/PID并处理失败清理；文件系统创建和数据库提交并非同一事务，崩溃可能留下空孤儿目录。             |
| D.4  | 调度器仅使用 `RoundRobin`，没有权重/关键路径优先；Bucket Release 返回是否实际释放。                                  |
| D.5  | policy/approval 审计使用 WithoutCancel 加有界超时；实际授权/执行仍遵循调用上下文，取消不能扩大权限。                 |
| D.6  | CLI flag、退出与 Close 错误保持可判定，继续由现有参数/取消回归验证。                                                 |
| D.7  | 遗留业务迁移/日志不再有兼容保留要求；来源不明的非架构文件不构成存储设计，本次不操作。                                |

## E. 开放问题结论

| 问题           | 当前行为或待完成项                                                                            |
| -------------- | --------------------------------------------------------------------------------------------- |
| 父级结果       | durable Mailbox、root 消费账本与后台结算注入后续请求；恢复原 Turn 不消费新消息                |
| reviewer       | 候选只读 view、写入口拒绝、reviewer/attempt 校验和冻结                                        |
| human_confirm  | App 策略要求真实审批，模型调用本身不算确认                                                    |
| 依赖/supersede | 依赖检查/解锁及明确候选作用域；失败依赖不能自动忽略                                           |
| Lease          | 规范路径、单操作互斥、实例及后代存活核对；无 TTL/续租                                         |
| Team           | cancel/准入门禁已有；会话级 draining/completed/failed 自动收敛、后台 merge 独立生命周期待实施 |
| Close          | DB、ATE 进程、Supervisor、LSP、Coordinator 分别由其 owner 负责关闭                            |
| eval           | 评测元数据/协议入口有调用，不能据此声称本次运行 benchmark 或验证模型效果                      |

## 仍需实施与验证

- Git-patch 使用 `git apply --3way`，未统一到 snapshot 逐路径协议，冲突可能留在主目录。
- 通用宿主 Git 生命周期未整体沙箱化：worktree checkout 可受仓库属性和本地 smudge/process filter 影响。禁 hooks/fsmonitor、过滤环境不足以消除该边界。
- unknown 合并检查的人工恢复/新验证流程、第二轮审查 Agent 未实现。检查失败保留文件、Task failed、依赖不解锁。
- durable 审批、历史引用增量维护、硬 RSS、artifact 自动保留/回收未完成。

2026-11-24 已完成 build、vet、依赖与补丁格式检查，并运行全量测试及 tool/runtimestore/app 定向 race。全量测试仅 CLI 的 `TestControlScopeProductionJSONL` 因 ATE 运行库所有权冲突失败，其余包通过；定向 race 通过。具体范围见[本轮验证记录](IMPLEMENTATION_REMAINING.md#验证状态与待核实项)。旧日期的通过结论不能替代当前结果；Linux 实机隔离与真实模型效果无本次验证证据。

后续验证重点包括 native 仓储、exact request、Session/Team 锁范围、Execution admission 中断、Turn/Fallback 独立失败、合并 finalization 和离线归档。入口包括 `internal/orchestration/atestore`、`internal/session/atestore`、`internal/app`、`internal/aterequest`、`internal/agentruntime`、`internal/workspacecoord` 及本地运行资源测试。
