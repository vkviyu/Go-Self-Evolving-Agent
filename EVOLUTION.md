# 受控自进化与生命周期运维

本实现保留 Eino Agent Loop。自进化意味着在同一 Loop 内提出、验证、修复和申请发布工具或策略，不意味着 Agent 可以自行扩权、改写宿主程序或跳过审批。以下命令默认使用当前配置；管理同一个数据库时必须先退出正在运行的 chat。各命令都接受 `--data-dir` 和 `--profile`；`plugins`、`strategy` 另接受 `--workspace`，`maintenance` 不接受 `--workspace`；`-p` 简写仅 `plugins`、`maintenance` 支持，`strategy` 只认 `--profile`。每个子操作只接受自己的专属 flag，混入其他操作的 flag 会直接报错（`--help` 列出的是整个命令族的 flag）。

## 1. Agent 生成工具

模型可用 `plugin_propose` 提交候选，`plugin_inspect` 读取本会话构建证据，`plugin_publish` 申请单独审批。编译或行为测试失败会返回结构化诊断；模型可以继续由 Eino 提交新 revision 修复。失败和通过的提案都不自动激活。发布后的 `extension_*` 调用仍需审批。

```text
提案 → 审批构建 → 隔离编译 → WASM 行为测试 → 候选等待审批 → 发布
                     ↓             ↓                          ↓
                   失败诊断 → 新 revision 修复             后续 Turn 使用
```

宿主可走同一服务手工操作：

```bash
go run ./cmd/local-agent plugins build --request examples/evolution/echo.json
go run ./cmd/local-agent plugins inspect-build --build-id BUILD_ID
# 先检查 source、cases、编译证据和 test_report，使用返回的 test_report_id：
go run ./cmd/local-agent plugins activate --tool extension_echo --revision echo-v1 --report-id REPORT_ID --confirm
go run ./cmd/local-agent plugins list
```

也支持已有 WASM 的 `stage --manifest FILE --wasm FILE`、`test --tool NAME --revision REV --cases FILE`、`retire --tool NAME --revision REV --confirm`，以及 `rollback --tool NAME --revision OLD_REV --report-id PASSING_REPORT --confirm`。回滚只是切换后续工具版本，不撤销历史副作用。`build` 输出的 `test_report_id` 与 `test` 输出的 `report_id` 都可作为 `--report-id`。CLI 不创建模型或启动 Supervisor；除 `gc-plan`/`collect` 只离线打开数据库外，其余 `plugins` 子命令都会完整打开 App（独占运行时所有权、WASM Runtime 和插件管理器）。

### 编译和执行边界

- 固定本机 Go **1.27.0** 和嵌入的 SDK。记录工具链可执行文件、SDK、源码和 WASM 摘要；不下载依赖，不接受自定义 go.mod、编译参数、输出路径或 SDK。
- 源码最多 64 KiB，整个 Request 最多 256 KiB。仅 `package main` 和 `Invoke(json.RawMessage) (json.RawMessage, error)`；禁止自定义 init/main、编译器指令和 import alias，标准库使用显式白名单。
- 仅生成 `extension_*` 纯计算工具，不能申请文件、网络、进程或凭证能力。需要文件内容时先用原有受控读取工具，再把内容作为 JSON 参数交给插件。
- macOS 使用 Seatbelt，Linux 使用 bubblewrap；后端不可用即失败，不退回裸进程。私有构建目录、离线环境、2 分钟墙钟上限、每进程 90 秒 CPU 和文件大小限制；日志最多 64 KiB，输出 WASM 最多 32 MiB。`GOMEMLIMIT=256MiB` 是 Go 软内存目标，**不是 OS 级 RAM 硬配额**。这里只提供受限离线编译，不宣称硬多租户资源隔离。
- 进程组在取消/退出时清理；App 关闭会取消并等待构建/影子工作。若工作未能停止，保留运行时所有权，不能让新进程与旧工作并发写入。
- 每个 WASM 调用使用 fresh instance，采用既有宿主内存/Blob 限额；生成候选调用最多 5 秒，省略 Timeout 时固定为 5 秒。1–32 个显式 JSON 用例，总测试最长 30 秒。`Timeout` JSON 数值单位为纳秒。
- 编译请求先持久预留再派发；完成结果幂等返回，中断状态不自动重编译。构建证据累计上限 64 MiB，达到上限时拒绝新提案；当前不提供证据清理，不能通过删除审计继续运行。
- 行为测试只证明给定用例；自生成用例尤其不能替代人工审阅。发布验证 exact revision、WASM 摘要和 Runtime 资源身份，不接受其他版本的报告。

## 2. 策略自进化

`LoopSpec` 是版本化数据：可以追加任务策略提示、降低迭代和输出额度；不能更换模型、扩展工具权限、提高宿主上限、修改 Goal 验收规则或执行任意代码。`strategy_propose / strategy_inspect / strategy_shadow / strategy_publish` 仅用于有可信身份的根 Agent；候选和报告按会话归属校验，子 Agent 不能借根模型开展额外测试。

评测预期由用户注册，没有模型可调用的“注册/改分”工具：

```bash
go run ./cmd/local-agent strategy suite --file examples/evolution/shadow-suite.json --confirm
go run ./cmd/local-agent strategy propose --file examples/evolution/strategy.json
go run ./cmd/local-agent strategy inspect --candidate-id CANDIDATE_ID

# 此命令明确调用模型、产生用量；之前和之后的管理命令均不调用模型。
# -- 后使用与正式 chat 相同的模型/系统提示/额度参数；profile 放在 -- 前。
go run ./cmd/local-agent strategy shadow --candidate-id CANDIDATE_ID --suite-id SUITE_ID --confirm -- --model YOUR_MODEL

go run ./cmd/local-agent strategy report --report-id REPORT_ID
go run ./cmd/local-agent strategy publish --report-id REPORT_ID --confirm
go run ./cmd/local-agent strategy rollback --baseline-id BASELINE_CONFIG_ID --candidate-id CANDIDATE_ID --confirm
```

CLI 提交的候选默认属于宿主；聊天内的候选由可信 invocation 绑定到当前会话。要让 Agent 提出并修复策略，先把已注册的 `suite_id` 告诉它，具体 shadow 和 publish 仍分别审批。普通非交互 chat 默认拒绝这些审批，不为自进化增加自动批准后门。

影子验证每个 fixture 分别运行当前基线和候选，使用独立内存会话、空工具注册表、关闭 Goal 和发布工具。最多 8 个 fixture（注册时至少 1 个且必须含 `safety: true` 用例）、每次执行 30 秒、整体 2 分钟；使用当前模型网关和 Team 记账，失败/取消请求照常按原记账规则处理。隔离的是业务工具和历史写入，不是免用量的模拟器。

报告记录逐题精确匹配、模型请求数、输出字节数和耗时。晋升条件为完整比较、无执行失败、安全标记用例全部通过、通过题数不低于基线且模型请求数不增加；字节数和耗时是诊断指标，不是计费或统计显著性结论。示例仅为 smoke suite，**不是通用安全认证或能力 benchmark**。

发布使用事务 CAS 复核被测试的旧策略，并绑定宿主程序摘要、宿主环境、原始执行配置及 CLI 实际模型设置。更换程序、模型、网关、提示、额度或工作区需重新验证。已创建的 Turn 不变，新 Turn 才选择新策略；`turn.started` 持久记录精确 LoopSpec 的内容寻址 ID（`loop_spec_id`）和 `base_execution_config_id`，恢复时按 ID 从不可变存储载入原 LoopSpec。恢复旧 Turn 时载入旧配置，而不是使用今天的 active 指针。

候选 Turn 执行失败（不含用户取消）会将同一 active 候选退回宿主基线，并审计、隔离该 revision；不能拿旧通过报告重新发布，必须新 revision + 新对比。旧 Turn 的失败不能撤销较新的 active 候选。回退只影响后续 Turn：不恢复文件、不重放请求、不退还用量。成功执行但答案质量退化仍须由用户发现和显式回退；这不是通用自动质量检测器。

## 3. 关闭恢复与安全回收

“执行完毕”“关闭恢复”和“删除插件字节”是三个独立决定，没有后台自动 GC。

```bash
go run ./cmd/local-agent maintenance plan --session SESSION_ID
go run ./cmd/local-agent maintenance archive --session SESSION_ID --plan-id PLAN_ID --confirm

go run ./cmd/local-agent plugins retire --tool TOOL --revision REV --confirm
go run ./cmd/local-agent plugins gc-plan --tool TOOL --revision REV
go run ./cmd/local-agent plugins collect --tool TOOL --revision REV --plan-id PLAN_ID --confirm
go run ./cmd/local-agent maintenance compact --confirm
```

完成的直接 Turn、终态 Goal、已持久返回结果的子 Agent 和关联终态 Flow 可以预览归档。活动 Goal、子任务、Task、wait、merge 等依赖不允许被归档绕过；未知状态、未完成直接 Turn、无法证明身份的孤儿引用保留。中断根任务仍使用原有 `chat --session SESSION_ID --abandon-turn TURN_ID|--abandon-goal GOAL_ID --abandon-reason TEXT --confirm-abandon` 入口。归档释放恢复引用并写入关闭记录，保留所有历史、checkpoint 和候选结果，不能重新激活已关闭的旧消费者。

GC 只针对明确的插件 revision。它要求离线独占、非 active、复核同一个 plan_id；检查保留快照引用和无引用但仍未关闭的历史消费者，未知 legacy 身份会阻止删除。事务内保留原 manifest/身份 tombstone 和回执，移除 revision，最后一个内容引用才删除共享 WASM blob。已删除 revision 永不重新绑定，即使重新提供原字节也必须新 revision。

`collect` 删除的 WASM 不能从本数据库恢复；不会删除历史、构建源码/报告、工作区、worktree、补丁和审计证据。`compact` 仅让 SQLite 归还已释放的页，不是数据清理命令。遗留身份不会自动回填或猜测，任何未释放引用数为零的状态也不能单独作为删除证明。

## 4. 验证与后续重构

本地回归包括真实 Go→WASM、Eino 提案失败/修复/发布/下一 Turn 调用、策略审批与跨重启恢复、回退隔离、报告篡改、事务失败回滚、共享引用/legacy 保留和进程组取消。模型由确定性测试替身驱动；这些测试验证实现，不宣称真实模型能力提升。未调用付费模型，也未下载 benchmark。

交付清单见 [IMPLEMENTATION_REMAINING.md](./IMPLEMENTATION_REMAINING.md)。大规模包重组、接口收敛和文件拆分在这一功能边界交付后单独进行，不与本轮混合。
