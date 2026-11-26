# 功能范围与剩余工作

更新：2026-11-25（代码清理与回归验证）。业务事实以 ATE 为权威，旧 SQLite 业务适配器与遗留日志已退出生产路径，不提供旧业务库自动迁移。Eino 继续负责 Agent Loop；`internal/runtimestore` 保存执行资源和 pending 请求。受支持的 ATE-linked runtime v1 库原地保留；它与旧业务库是不同格式。

例如，另一个项目再次遇到流超时，可以搜索上次留下的排查经验，核对其来源和现场条件，再决定是否采用。GSEA 的 Evolving 以 ATE 管理的跨任务记忆为核心。当前模型工具和具体示例见[跨任务记忆指南](EVOLUTION.md)。

## 现行范围

| 项目            | 已有能力与当前入口                                                                                                                            | 验收重点                                                                             |
| --------------- | --------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------ |
| R1 引用生命周期 | `internal/orchestration/atestore` 生成保留计划/权威关闭回执；`internal/app/maintenance.go` 再清理本地 runtime 引用                            | 直接 Turn、执行契约、子 Agent、Flow；活跃/未知依赖保留；关闭事实持久后清理失败可重试 |
| R5 边界回归     | native 仓储、runtime 资源、App/CLI 均有相关测试                                                                                               | 清理后重新执行集成/静态/相关竞态检查；不能沿用旧 SQL 测试结果                        |
| R7 宿主策略     | `strategy_propose`、`strategy_inspect`、`strategy_shadow`、`strategy_publish` 管理有界 LoopSpec，`internal/evolutionstore` 保存策略报告与状态 | Turn 固定精确配置，Fallback 与 Session `turn.failed` 分别提交                        |
| R8 完整链路核对 | App、CLI、策略与恢复有本地脚本模型测试                                                                                                        | 配套 ATE 的跨重启及失败注入，模型效果需单独验证                                      |

记忆验收关注 `memory_query` 的查询范围、`memory_manage` 的审批、原始事件引用、采用回执和来源撤回。Session 归档另行取得 ATE 关闭回执并释放本地引用，`compact` 整理 runtime SQLite 空闲页。[记忆接入与验证记录](docs/ATE_INTEGRATION.md)

## 本轮架构清理的验收门槛

1. App 仅装配 native Session、编排和自进化仓储；runtime SQLite 不再承担消息、Task、用量或领域事件权威。旧业务 DDL、迁移、适配器与日志清理以主代理最终源码为准。
2. native Session append 与 authority batch 保持独立；Execution admission 使用 token/版本与阶段核对，不声称跨 store 原子提交。
3. Session 执行锁绑定共享 ATE store identity；自动恢复仅限所属 Team。离线维护不派发无法证明作用域的旧 pending 请求。
4. attempted 且结果 unknown 的请求只能按原身份对账，查询不存在不授权重发。已提交而清理失败只补清理，不换 key 重做业务。
5. completion/归档/回收均保留阶段回执；工具 started 后结果未知、合并检查 dispatched 后结果未知，不自动重做外部副作用。
6. 策略回退只改变后续 Turn，独立于 `turn.failed` 提交；恢复旧 Turn 按持久 LoopSpec ID，不能套用今天的 active 配置。

## 尚未完成的产品与安全边界

- 多 Agent：steer、durable 审批、Agent 独立预算、tester Profile、关键路径调度、Team 会话级 draining/completed/failed 自动收敛。
- 工作区：宿主 Git checkout/filter 等生命周期安全、git-patch 与 snapshot 统一 apply 协议、后台 merge 的 per-Team 取消/等待、unknown 检查的显式人工恢复、worktree 池和所有权 epoch。
- 资源方面仍需完善历史引用索引、增量扫描、artifact 保留策略及宿主总 RSS 限制。
- 高级编排：委派收益、跨 provider 路由、历史表现选 Profile、自动修订/验证任务、Team 模板、可视化 DAG、完整 headless 协议及容量/效果评测。

详细多 Agent 阶段与状态机见 `MULTI_AGENT_ARCHITECTURE.md`；操作与资源限制见 `EVOLUTION.md`；审计边界见 `AUDIT_REMEDIATION.md`。

## 验证状态与待核实项

2026-11-25 在 macOS arm64 / Go 1.27.0 上完成以下验证：

- `go build -mod=readonly ./...`、`go vet -mod=readonly ./...` 和 `git diff --check` 通过。
- `go test -mod=readonly ./...` 已执行完毕。除 CLI 包中的 `TestControlScopeProductionJSONL` 外，其余包通过；该测试因聊天进程持有同一 ATE 运行库、独立 control 进程取得所有权超时而失败。该问题仍未解决，不能声明全量通过。
- resume 的 `SessionCatalog` 单 ownership 回归、显式会话路径、App adoption 和 ATE process 生命周期定向测试通过；测试按 `ownership + start` 统计持久 owner，同一 lease 的 reconnect 不算第二次打开。
- `internal/tool`、`internal/runtimestore`、`internal/app` 的定向验证通过，覆盖快照恢复、运行库格式、Memory 工具、策略审批和关闭时的所有权保护；不代表全库 race。
- 交互式 resume 会话隔离、Memory 查询与写入审批、策略重开、旧 runtime v1 原位打开及损坏格式拒绝回归通过。旧库测试使用临时 fixture，核对 checkpoint、pending 请求、工具快照及历史字节保留。

本轮未执行 Linux 实机隔离验收、真实付费模型效果或容量评测。此前日期的全库通过结论不能替代上述结果。真实会话和 ATE 数据未清理；运行库旧格式识别保留已有执行证据。
