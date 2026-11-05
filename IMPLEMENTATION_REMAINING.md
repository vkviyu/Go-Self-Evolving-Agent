# 剩余边界交付清单

本轮先补齐功能与安全边界，再单独进行以架构稳定性、代码可读性为目标的大规模重构。下面的编号属于本轮收尾，不替代原设计文档的产品路线图。

## 交付顺序与验收

- [x] R1：完成任务的引用生命周期。覆盖直接 Turn、终态 Goal、子 Agent、Flow；先离线预览，按同一计划复核后释放。活跃依赖、未知状态和遗留身份无法证明时保留；关闭恢复不删除历史结果。
- [x] R2：安全插件产物回收。独占所有权、明确 revision、可复核计划、人工确认；保留引用或未知遗留消费者时拒绝。保留不可变身份和删除审计，不能重新绑定旧 revision。工作区与审计证据不作为通用目录清理目标。
- [x] R3：插件管理 CLI。列出、检查、暂存、行为测试、激活、停用、回滚和受控回收；不启动模型或 Supervisor，发布与回收需明确用户操作。实际子命令为 `plugins list|stage|build|inspect-build|test|activate|retire|rollback|gc-plan|collect`。“停用”对应 `retire`，“检查”对应 `inspect-build` 和 `list`；`activate`/`rollback` 需要 `--report-id` 与 `--confirm`，`collect` 需要 `--plan-id` 与 `--confirm`。
- [x] R4：隔离构建与验收发布。固定 Go 1.27.0/SDK、受限输入、离线构建、资源/超时上限、持久构建和测试证据；测试通过不自动授予权限或自动激活。
- [x] R5：上述边界的跨重启、失败注入、共享引用、权限和端到端测试；同步 README 与架构文档并进行全量测试、静态检查、相关竞态检查。

“零引用”不单独作为可删除证明；不把副作用回滚、任意历史 checkpoint 恢复或供应商精确计费列为可承诺的能力。

用户已确认：完整自进化能力也应在大规模重构前完成。继续交付：

- [x] R6：Agent 生成工具的受控闭环。能力提案、受限源码、构建/行为测试、可诊断修复、隔离候选、人工晋升，以及同一会话后续 Turn 使用新版本；不自写第二套 Agent Loop，不让模型自授能力。模型工具为 `plugin_propose`、`plugin_inspect`、`plugin_publish`（`internal/evolutiontools`）。
- [x] R7：策略自进化。版本化 LoopSpec、隔离影子验证与可比指标、显式准入、Turn 边界切换、失败回退和审计。策略变更不能改写历史或放宽宿主权限。模型工具为 `strategy_propose`、`strategy_inspect`、`strategy_shadow`、`strategy_publish`，CLI 为 `strategy propose|suite|inspect|report|shadow|publish|rollback`。R6、R7 合计 7 个自进化工具。
- [x] R8：完整链路验收与文档核对。固定本地测试样例、故障注入和重启恢复验证；不以测试替身的结果冒充真实模型效果提升，不下载大型 benchmark，不默认调用付费模型。

原路线图 Phase 6 的实验性 WASM Driver 仍是非 MVP 可选方向；通用 Agent Loop 继续由 Eino 承担，不为“完整”重写 Loop。

## 重构门槛

完成上述明确范围和验收后，再规划大规模重构。本轮只做实现功能所需的局部组织调整，不预先移动大量包、不替换 Eino Loop、不混入无关重写。

## 验收记录（2026-09-26）

环境：Go 1.27.0，darwin/arm64。以下均通过：

```bash
GOPROXY=off go test ./... -count=1
GOPROXY=off go vet ./...
GOPROXY=off go test -race -short ./internal/app ./internal/agentruntime ./internal/conversation ./internal/session/sqlitestore ./internal/plugin ./internal/pluginbuild ./internal/cli ./internal/sandbox ./internal/tool ./internal/strategy -count=1
```

最后的宿主程序身份绑定变更另行复跑了全量测试、vet 和 App/CLI 竞态测试。真实编译/执行测试覆盖 Eino 内的编译失败 → 行为测试失败 → 新 revision 修复 → 审批发布 → 下一 Turn 使用 → 归档释放引用 → 离线精确回收 → 重启保留共享候选及原证据。策略测试覆盖 CAS、审批/会话边界、旧 Turn 策略固定、跨重启读取、回退隔离、证据篡改与失败原子性。

说明：完整性限定于上面的受控产品范围。生成工具仅有纯 JSON 计算能力；LoopSpec 仅追加策略提示和降低额度；影子测试是有明确预期的工具禁用样例，不是通用效果认证。Go 编译器内存是软目标，不声称硬多租户隔离。没有运行真实模型效果评测、Linux 实机验收或任意 Driver 自重写。所有归档/回收测试使用临时数据库，没有清理用户真实历史或插件。

使用方法及完整限制：[EVOLUTION.md](./EVOLUTION.md)。本轮功能边界已交付，可进入独立的架构稳定性与可读性重构阶段。

## 复核记录（2026-10-26）

按当前代码静态核对，R1–R8 均有对应实现与测试，状态保持“已完成”：

| 项 | 实现 | 测试 |
| --- | --- | --- |
| R1 | `internal/retention`；`sqlitestore/retention*.go`、`maintenance_*.go`；`app/maintenance.go`；CLI `maintenance plan\|archive\|compact` | `sqlitestore/retention_test.go` |
| R2 | `sqlitestore/plugin_collection*.go`；删除前写入 `plugin_revision_tombstones`；旧 revision 不能重新绑定 | `retention_test.go` |
| R4 | `internal/pluginbuild`：要求 `go version go1.27.0`，`GOTOOLCHAIN=local`、`GOPROXY=off`、`GOWORK=off`、`CGO_ENABLED=0`、`GOOS=wasip1`；2 分钟超时，`ulimit -t 90`/`-f 131072`，在沙箱内执行。编译器使用宿主 `GOROOT`，宿主必须就是 Go 1.27.0 | `pluginbuild/{compiler,service}_test.go` |
| R6 | 见上文 | `app/plugin_loop_test.go` 的 Eino 端到端闭环 |
| R7 | 见上文 | `strategy/shadow_test.go`、`sqlitestore/strategies_test.go`、`app/strategy_test.go` |

Phase 6 的 WASM Driver 仍未实现。Agent Loop 仍只由 `internal/agentruntime/eino_turn.go` 构造（Eino v0.9.22），架构测试固定了这一点。

`GOPROXY=off go test ./... -count=1` 当天全量通过。之后新增的多 Agent 工作（迁移 v34–v36 等）不在本清单范围内，见 [MULTI_AGENT_PARALLEL_PLAN.md](./MULTI_AGENT_PARALLEL_PLAN.md)。
