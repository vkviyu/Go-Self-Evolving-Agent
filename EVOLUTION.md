# 跨任务记忆与经验复用

上个项目排查模型流超时时，请求记录显示响应头已到，随后一直没有收到响应体字节。值得留给下个任务的经验是“先核对请求阶段，再决定查连接还是查流读取”。另一个项目遇到相似症状时，可以参考这条排查顺序。至于这次卡在哪一段，仍要看新请求自己的记录。

GSEA 把这种跨任务复用称为 **Evolving**。ATE 保存原始会话事实、带来源的记忆及采用记录。主 Agent 判断哪条经验值得查、证据能支持多大的结论、当前任务是否适用。

## 从第一次排查到第二次采用

1. **留下来源。** 第一个会话中的请求与工具结果已经持久化。主 Agent 可以通过 `memory_manage` 的 `create_candidate` 提出有适用条件的 `claim`，提供 `kind`（`fact`、`decision`、`procedure`、`failure_pattern`、`preference` 之一），指定已有语义空间 `space` 或创建 `new_space`（两者择一）。`event_ids` 可以指向本会话的原始事件。省略时仅以最近的持久用户输入或后台输入说明来历。工具返回候选的 `ref` 和来源，供之后核查。
2. **核对候选。** 在允许披露的范围内，用 `memory_query` 的 `read` 将准确引用放入 `refs` 数组，查看 `claim`、`status`、`reusable` 和 `source_refs`。候选要经过独立验证、晋升并符合当前复用策略，才进入搜索和计数。默认的两个记忆工具没有 `validate` 或 `promote` 操作，因此下面的跨任务复用以已有合格记忆为前提，不是普通 CLI 自动完成的全闭环。
3. **在另一个任务查找。** 第二个会话可以查询同一 ATE 中当前可复用的经验。例如，向 `memory_query` 传入下面的参数。用返回的 `ref` 精读，核对来源、适用条件和现场记录。如果这次还没收到响应头，就应沿这次的证据排查。

   ```json
   {"operation":"search","text":"响应头","limit":5}
   ```

4. **显式采用。** 决定采用时，主 Agent 请求 `memory_manage` 的 `adopt` 并提交一个可复用 `ref`。宿主绑定 Session、Turn、Call 和 Actor，经过审批，再核对来源并在 ATE Knowledge 中留下本次采用的 observation。承载记录的执行作用域随后退役。实际排查、工具授权和 Task 验收各有自己的回执。
5. **维护失效的结论。** 发现原始记录有误时，带 `ref` 和 `reason` 请求 `correct_source`。需要停止复用时用 `withdraw_source`。可通过 `source_ref` 指定该记忆声明过的来源，默认针对记忆自身。两种操作都保留历史并禁止再次复用相应来源。替代结论要另建候选，提供来源并重新验证。

## 对话里有哪些入口

| 入口 | 可做的事 |
| --- | --- |
| `memory_query`（只读） | `directory` 列语义空间，`count` 统计当前可复用记忆，`search` 搜索这些记忆（空文本可列举），`read` 精读指定记录，包括允许披露的候选。 |
| `memory_manage`（写，需审批） | `create_candidate`、`adopt`、`withdraw_source`、`correct_source`。变更结果未知时，保留同一调用的原请求等待对账。 |
| TUI `/memory`、`@memory:` | 用户主动浏览或引用。正文和空间描述按不可信历史材料处理，引用本身不会登记采用。 |

普通 CLI 把输入交给主 Agent。宿主提供工具说明，模型按需决定是否调用，工具结果回到原 Eino 循环。普通 Turn 没有每轮的检索或记忆正文注入，成功结束也不会自动提炼。嵌入宿主可以显式接入 `Prepare` / `Check` / `Complete`。这条可选路径按已保存的历史截点选择经验、复核来源，并可对提炼出的候选进行第二次基于原始证据的审查。审查记录描述模型对证据的判断，实际测试结果以工具事件为准。[工具契约](internal/app/memory_tools.go) · [来源与写入](internal/experience/tools_manage.go) · [ATE 接入](docs/ATE_INTEGRATION.md)

## 独立的宿主能力：LoopSpec 策略比较

`LoopSpec` 是版本化数据：可以追加任务策略提示、降低迭代和输出额度；不能更换模型、扩展工具权限、提高宿主上限、修改 Goal 验收规则或执行任意代码。`strategy_propose / strategy_inspect / strategy_shadow / strategy_publish` 仅用于有可信身份的根 Agent；候选和报告按会话归属校验，子 Agent 不能借根模型开展额外测试。

评测预期由用户注册，没有模型可调用的“注册/改分”工具。先按 [ATE 接入说明](docs/ATE_INTEGRATION.md) 构建配套二进制并配置 `PATH` 或 `GSEA_ATE_BINARY`；以下命令在仓库根目录运行。保留的示例是 [strategy.json](examples/evolution/strategy.json) 和 [shadow-suite.json](examples/evolution/shadow-suite.json)：

```bash
go run ./cmd/gsea strategy suite --file examples/evolution/shadow-suite.json --confirm
go run ./cmd/gsea strategy propose --file examples/evolution/strategy.json
go run ./cmd/gsea strategy inspect --candidate-id CANDIDATE_ID

# 此命令明确调用模型、产生用量；之前和之后的管理命令均不调用模型。
# -- 后使用与正式 chat 相同的模型/系统提示/额度参数；profile 放在 -- 前。
go run ./cmd/gsea strategy shadow --candidate-id CANDIDATE_ID --suite-id SUITE_ID --confirm -- --model YOUR_MODEL

go run ./cmd/gsea strategy report --report-id REPORT_ID
go run ./cmd/gsea strategy publish --report-id REPORT_ID --confirm
go run ./cmd/gsea strategy rollback --baseline-id BASELINE_CONFIG_ID --candidate-id CANDIDATE_ID --confirm
```

CLI 提交的候选默认属于宿主；聊天内的候选由可信 invocation 绑定到当前会话。要让 Agent 提出并修复策略，先把已注册的 `suite_id` 告诉它，具体 shadow 和 publish 仍分别审批。普通非交互 chat 默认拒绝这些审批，不为自进化增加自动批准后门。

影子验证每个 fixture 分别运行当前基线和候选，使用独立内存会话、空工具注册表、关闭 Goal 和发布工具。最多 8 个 fixture（注册时至少 1 个且必须含 `safety: true` 用例）、每次执行 30 秒、整体 2 分钟；使用当前模型网关和 Team 记账，失败/取消请求照常按原记账规则处理。隔离的是业务工具和历史写入，不是免用量的模拟器。

报告记录逐题精确匹配、模型请求数、输出字节数和耗时。晋升条件为完整比较、无执行失败、安全标记用例全部通过、通过题数不低于基线且模型请求数不增加；字节数和耗时是诊断指标，不是计费或统计显著性结论。示例仅为 smoke suite，**不是通用安全认证或能力 benchmark**。

发布通过 ATE authority CAS 复核被测试的旧策略，并绑定宿主程序摘要、宿主环境、原始执行配置及 CLI 实际模型设置。更换程序、模型、网关、提示、额度或工作区需重新验证。已创建的 Turn 不变，新 Turn 才选择新策略；native Session 的 `turn.started` 持久记录精确 LoopSpec 的内容寻址 ID（`loop_spec_id`）和 `base_execution_config_id`，恢复时按 ID 从不可变存储载入原 LoopSpec。恢复旧 Turn 时载入旧配置，而不是使用今天的 active 指针。

候选 Turn 执行失败（不含用户取消）会将同一 active 候选退回宿主基线，并审计、隔离该 revision；不能拿旧通过报告重新发布，必须新 revision + 新对比。旧 Turn 的失败不能撤销较新的 active 候选。回退只影响后续 Turn：不恢复文件、不重放请求、不退还用量。成功执行但答案质量退化仍须由用户发现和显式回退；这不是通用自动质量检测器。

失败记录和策略回退分别提交：`turn.failed` 与 checkpoint 事实使用一次 native Session append，之后单独调用策略 `Fallback`。任一侧都可能失败，错误会向上传递；不能把“Turn 已记录失败”当作“策略已回退”，也不能宣称跨域原子性。已尝试而结果未知的 ATE 请求按原 key/digest/store identity 对账，查询不到也不自动重发。

## 关闭恢复与工具快照引用

Turn 和 Flow 的工具快照固定了执行时的工具身份。需要关闭已完成消费者的恢复时，先退出持有运行库的实例，再预览和确认同一计划。

```bash
go run ./cmd/gsea maintenance plan --session SESSION_ID
go run ./cmd/gsea maintenance archive --session SESSION_ID --plan-id PLAN_ID --confirm

go run ./cmd/gsea maintenance compact --confirm
```

完成的直接 Turn、已持久返回结果的子 Agent 和关联终态 Flow 可以预览归档。活动子任务、Task、wait、merge 等依赖不允许被归档绕过；未知状态、未完成直接 Turn、无法证明身份的孤儿引用保留。中断根任务仍使用 `chat --session SESSION_ID --abandon-turn TURN_ID --abandon-reason TEXT --confirm-abandon` 入口。归档先提交 ATE 关闭回执，再清理本地恢复引用；后一步失败按原 plan_id 补做，不撤销已提交关闭事实。历史、checkpoint 和候选结果保留，已关闭消费者不能重新激活。

`compact` 只让本地 SQLite 归还空闲页。要停止复用某条经验，使用上面的来源维护操作。来源撤回、会话归档和释放本地快照引用有各自的权威记录。[架构导读](ARCHITECTURE.md)
