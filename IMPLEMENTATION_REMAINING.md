# 功能范围与剩余工作

更新：2026-11-20。当前正在进行整体架构清理：业务事实以 ATE 为权威，移除旧 SQLite 业务适配器与遗留日志，不保留旧库兼容。Eino 继续负责 Agent Loop；runtime SQLite 仍保存执行资源和 pending 请求，包名暂按当前源码保留。

下表的“已有”表示可在当前生产装配和源码中找到实现，不表示清理后的最终验收已通过。原 R1–R8 编号用于追踪受控自进化功能，不替代多 Agent 产品路线。

## R1–R8 当前实现

| 项目 | 已有能力与当前入口 | 验收重点 |
| --- | --- | --- |
| R1 引用生命周期 | `internal/orchestration/atestore` 生成保留计划/权威关闭回执；`internal/app/maintenance.go` 再清理本地 runtime 引用 | 直接 Turn、终态 Goal、子 Agent、Flow；活跃/未知依赖保留；关闭事实持久后清理失败可重试 |
| R2 插件回收 | App 协调 native collection 意图/完成回执与 runtime 插件资源清理；保留 revision tombstone | 独占资源作用域、同 plan_id、共享 blob、未知消费者阻断；不能重新绑定已删除 revision |
| R3 管理 CLI | `plugins list\|stage\|build\|inspect-build\|test\|activate\|retire\|rollback\|gc-plan\|collect` | 发布使用 report_id，回收使用 plan_id，均需显式确认；不启动模型或 Supervisor |
| R4 隔离构建 | `internal/pluginbuild`，固定可信 Go 1.27.0/嵌入 SDK、离线沙箱、超时/输出限制、ATE 构建与测试证据 | 工具链摘要、进程组取消、失败不激活；Go 软内存目标不等于硬 RSS 上限 |
| R5 边界回归 | native 仓储、runtime 资源、App/CLI 均有相关测试 | 清理后重新执行集成/静态/相关竞态检查；不能沿用旧 SQL 测试结果 |
| R6 Agent 生成工具 | `plugin_propose/inspect/publish`，由 `internal/evolutiontools` 接入原 Eino Loop | 编译失败→修复 revision→测试→审批发布→后续 Turn 使用；仅纯 JSON 计算 |
| R7 策略自进化 | `strategy_propose/inspect/shadow/publish`；`internal/evolutionstore` 保存 LoopSpec、报告、active 和隔离状态 | Turn 固定精确配置、报告/环境绑定、CAS、权限不升级；Fallback 与 Session `turn.failed` 分别提交 |
| R8 完整链路核对 | App、CLI、策略、构建和恢复已有本地脚本模型测试 | 配套 ATE binary 的跨重启及失败注入；不把测试替身视为真实模型效果提升 |

R6/R7 合计七个模型工具。策略 CLI 为 `strategy propose|suite|inspect|report|shadow|publish|rollback`。插件 `gc-plan/collect` 与 Session 归档均需离线访问 ATE 权威事实及本地资源；`compact` 仅整理 runtime SQLite 空闲页。

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
- 资源：历史引用索引/增量扫描、artifact 保留/清理策略、编译进程硬 RSS 和宿主总 RSS 限制。
- 高级编排：委派收益、跨 provider 路由、历史表现选 Profile、自动修订/验证任务、Team 模板、可视化 DAG、完整 headless 协议及容量/效果评测。
- 原 Phase 6 的实验性 WASM Driver 仍是非 MVP 可选方向，未实现；通用 Loop 不因此重写。

详细多 Agent 阶段与状态机见 `MULTI_AGENT_ARCHITECTURE.md`；操作与资源限制见 `EVOLUTION.md`；审计边界见 `AUDIT_REMEDIATION.md`。

## 验证状态与待核实项

本次仅做源码/文档静态核对，未运行全量测试、vet 或 race。此前日期的“全部通过”与旧业务库升级记录不构成本次清理后的验收结论。

清理结束后需要配套 ATE/GSEA 构建与 native 集成测试，重点覆盖请求对账、Session/Team 作用域、Execution admission、Turn/策略独立失败、completion、合并、保留/回收阶段间中断。现有测试文件仅是验证入口，不是已执行证据。

暂不能核实或不应继续承诺：runtime 资源包最终名称、遗留文件最终删除清单、当前工作区全量测试结果、Linux 实机验收、真实模型效果和容量指标。无需保留旧业务格式、旧 API 别名或自动数据迁移来满足本轮目标。
