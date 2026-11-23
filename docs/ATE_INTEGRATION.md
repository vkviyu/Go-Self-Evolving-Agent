# GSEA 原生 ATE 接入

GSEA 将 ATE 作为会话和执行事实的唯一权威存储。普通任务不需要在 prompt 中提到 ATE，也不需要调用 MCP 或 Skill。GSEA 仍拥有 Agent Loop、模型调用、工具执行、工作区写入、Task 调度规则和恢复决策；会话、任务、消息、模型请求与响应、工具审批与结果，以及可复用经验直接保存在 ATE。

应用启动后由 `internal/app` 持有一个 ATE 子进程管理器。它通过原生 stdio 帧协议连接 `ate`，使用 `--all-spaces --allow-global-snapshot --agent-profile --event-profile`，并为每次启动生成私有认证凭证。默认查找顺序是与 `gsea` 同目录的 `ate`，然后才使用 `PATH` 中的 `ate`。本地构建可以运行：

```sh
make build
./bin/gsea chat
```

`make build` 构建成对的 `bin/gsea` 和 `bin/ate`。当前仓库通过本地 sibling module replacement 使用尚未发布的 ATE 模块，适合工作区开发；发布独立安装包时需要将两个版本一起打包。`go run ./cmd/gsea chat` 使用临时可执行目录，需要通过 `PATH` 或 `GSEA_ATE_BINARY="$PWD/bin/ate"` 指定配套 ATE。

默认配置开启 ATE：

```toml
[ate]
enabled = true
binary = "ate"
directory = "/private/path/gsea-temporal"
timeout = "30s"
```

也可以使用 `GSEA_ATE_ENABLED`、`GSEA_ATE_BINARY`、`GSEA_ATE_DIRECTORY` 和 `GSEA_ATE_TIMEOUT` 覆盖配置。应用必须启用 ATE；设置 `enabled = false` 会拒绝打开应用。代码嵌入场景的 `config.Config` 零值仍是关闭状态，宿主需要显式设置 `ATE.Enabled`、二进制路径和存储目录。

Session Event Store 使用 ATE 原生追加接口；编排仓库在 ATE 上逐条保存实体，并将相关状态、事件与幂等记录原子提交。GSEA 不保存业务历史副本，也没有启动或关闭时的历史补送。ATE 不可用时，依赖事实提交的操作停止。

本地 `runtime-v3.db` 保存 checkpoint、工具快照、插件等宿主运行资源，以及未完成原生请求。结果未知时保留请求的原始字节、RequestID 和目标 Store ID；核验提交回执后删除正文。明确拒绝会保留无正文的终结标识。启动核对不是历史复制，不能把“未找到回执”当成“可以重新执行”。已有 `runtime.db`、`runtime-v2.db` 和 `sessions.db` 原地保留，当前版本不自动接管、迁移或回填旧库。

不同本地运行库可以共享 ATE 历史。同一会话执行受 Store ID 和 Session ID 对应的独占锁保护，Supervisor 只恢复自己持有的 Team。运行库也绑定 Store ID，防止替换 ATE 目录后把旧 checkpoint 用到另一个存储。

工具结果和审批记录的持久化使用配置的 ATE 批次时限加关闭余量，不再被固定的 5 秒时限提前截断。完整追加计划已经保存后，若提交结果未知或超时，宿主会先释放旧连接，再用独立时限进行一次原请求核验。核验保留原 RequestKey、事件 ID、序号和字节；上传确认后才允许发送原计划里尚未尝试的追加。已尝试的请求查无回执时仍保持未知，不重新执行工具，也不生成另一笔追加来绕过阻塞。

无法自动核实的工具结果在 TUI 中显示为 `Paused`。退出占用该会话的运行进程后，可以只恢复存储请求：

```sh
./bin/gsea session reconcile <session-id> --data-dir <原运行库目录> --workspace <原工作区>
```

目录默认取正常配置；使用独立 Session 运行目录时必须指定对应的 `--data-dir`，使用配置 profile 时保留 `--profile`。该命令核验所属会话及其子会话的原始保存计划，不调用模型或工具，不自动继续 Agent turn；仍有活动运行进程持锁时拒绝执行。`/recoveries` 只查看中断 turn，不能替代存储回执核验。核验成功后再通过原会话的正常恢复流程继续。

TUI 的请求阶段显示当前操作及其持续时间；存储操作同时显示 ATE 子阶段，例如 `Persisting request · 8s · ATE waiting for lock 6s`。子阶段区分本进程排队、跨进程存储锁、目录检查、子进程启动、存储打开与握手、原生操作和关闭。stdio 是持续双向的帧连接，不是一次性输入输出；GSEA 启动后由 App 持有一个经过身份校验的 ATE 子进程、transport 和 client，普通 repository callback 串行复用它，直到 App 关闭。握手等待包含 ATE 打开和恢复存储的时间，不能仅凭 `handshake` 判断为网络延迟。

交互界面可以先于运行时就绪显示。此时提交的输入显示 `Waiting for runtime startup`，并展示模型初始化、根 Agent 所有权、运行时工厂、Supervisor 恢复、会话与邮箱、维护等启动阶段。只有必要恢复完成后才执行排队输入；启动失败直接显示错误。启动完成时同时装配历史、用量、恢复和子 Agent 查看能力。

本地启动存储步骤通过 `App.WithStorageBatch` 复用 App 已持有的原生连接，减少反复打开和恢复整个 ATE 存储的开销。整个批次受 ATE 时限约束；它不是跨仓库原子事务，失败不能整体重试。批次上下文只用于同步存储步骤，不能跨模型调用、用户等待或长期后台任务持有。作用域结束后拒绝旧上下文复用；提交结果未知会使当前连接失效，普通业务 callback 不会自动重放。显式恢复会先关闭旧 lease、释放 owner，再建立新 stdio 连接，并只用原始 RequestKey、请求字节和 Store ID 核验原请求。App 关闭时先等待使用者退出，再关闭 client、ATE 子进程和 owner。

启动性能可通过 `TestInteractiveStartupTiming` 复验：显式设置 `GSEA_TEST_STARTUP_TIMING=1`、`GSEA_TEST_STARTUP_STORE=<已有存储目录>` 和 `GSEA_ATE_BINARY=<配套二进制绝对路径>`。测试持锁复制出两个隔离临时存储，执行相同的根 Agent、Supervisor、会话和维护流程，不调用模型、工具或后台调度。2026-11-22 最终配套二进制复验中，两组都使用优化后的 ATE：启动准备由 28.17 秒、19 次原生启动降为 1.40 秒、1 次；包含 `app.Open` 后由 30.97 秒降为 4.15 秒。这是该机器和数据规模下的本地阶段测量，不代表供应商延迟或完整回答时间。

配套 ATE 在单次恢复期间复用已经验证的不可变对象字节，并在打开完成前丢弃这份有界缓存。它仍完整扫描历史，不缓存授权结论，也不影响运行期间的磁盘完整性检查。同一份 1,638 笔事务、约 20 MB 已发布数据的隔离副本中，单次打开、恢复和关闭的中位耗时由 2.04 秒降为 1.42 秒；该优化需要重新构建配套 `ate` 二进制。

`Building request` 属于模型发送前的本地准备；会话状态读取、工具快照保存和 Agent 构建有独立阶段。`Persisting request` 表示记录模型请求，发送前的持久化标记另行显示；到 `Requesting model` 才进入供应商调用。这些通知仅服务实时观察，不写入会话事实，也不改变重试、审批或提交协议。旧会话未记录这些子阶段时间，不能从旧标签还原各步骤耗时。

到达 HTTP 调用阶段的 Responses 流式请求会在 `model.failed` 或 `assistant.message` 事件中记录 `transport`。所属 Session、Turn 和 GSEA `request_id` 可与 `model.requested` 的 `request_sequence` 对照。记录包括 HTTP 开始、响应头、首字节、最后字节与结束时间、状态码、内容类型、已读字节数、最后解析的 SSE 类型、是否收到有效的 `response.completed`，以及有界的上游请求标识。诊断字段只保存这些元数据。时间线以宿主读到的字节为准，可区分响应头等待、响应体首次无字节和部分响应后的停顿。超时时，CLI 和 TUI 的错误会附上本地请求 ID、已读字节数和最后 SSE 类型。

普通 turn 先交给主 Agent 理解。共享系统指令说明记忆属于 Agent 的跨会话历史，并区分查看、采用和维护；主 Agent 根据当前请求决定是否使用工具。没有 TUI 自然语言句式分流，也没有每轮必调的辅助路由模型。

例如，询问记忆数量时，主模型可以选择 `memory_query` 的 `count` 操作，再根据工具结果回答；询问一个完全不依赖历史的问题时，可以直接回答。执行任务时，可以先搜索、读取候选，再判断是否适用。自然语言理解仍需要主模型请求，供应商网络故障仍可能导致请求暂停。

- `memory_query` 提供 `count`、`search`、`read` 和 `directory`。搜索和计数针对当前可复用记忆；精确读取返回状态和可复用标记，不把候选伪装成已验证事实。查询不会自动采用或修改记忆。
- `memory_manage` 提供候选写入、来源撤回、事实纠正和显式采用。写入绑定宿主提供的 Session、Turn、Call 和 Actor 身份，经过统一工具审批。候选引用当前会话的原始事件；模型不能凭自己的成功声明完成验证或发布。
- `/memory` 和 `@memory:` 仍支持用户直接浏览与引用。引用以不可信历史材料提供给 Agent，由它判断相关性，不会成为系统指令。
- 普通成功 turn 不会自动启动 `Experience.Complete`。自动经验选择和提炼服务仍可由嵌入宿主显式配置，但不是 CLI 默认路径；其测试不能代表主 Agent 的工具选择效果。

工具结果沿正常 Agent Loop 返回主模型，由主模型完成答复。当前没有“任意工具结果直接替代最终回答”的通用完成协议。查询有分页与总量上限，读取不完整时返回错误，不把截断结果当成完整计数。恢复继续使用原工具调用身份和已确认回执；未知结果保持待核验。

显式采用按调用建立 record-only Knowledge 范围，记录后使用原生取消命令退役承载它的 Task/Execution，保留采用记录，不伪造用户工作的验证成功。退役不依赖来源仍可复用；丢失回执后的同身份核对和来源撤回也不会绕过收尾。`source_refs` 包含直接来源、证据、验证证据及反例，来源级撤回按这个完整集合校验。

原生写入结果未知会暂停工具链，并保留原始请求供核对；普通恢复不会盲目再次执行该修改。候选在独立验证和发布前不进入可复用记忆的搜索或计数，当前工具只支持按精确引用检查候选。

2026-11-22 的真实供应商定向验收使用正常模型配置、两条合成请求和隔离的临时存储：打招呼产生 1 次主模型请求、0 次工具调用；询问可复用记忆数量产生 2 次主模型请求、1 次成功的 `memory_query/count`。两者都没有辅助 Experience 请求，实测 turn 分别约 6 秒和 18 秒。测试入口为 `GSEA_TEST_REAL_MEMORY=1 go test ./internal/app -run '^TestMemoryRealProviderDecision$' -count=1 -v`；它验证这两个场景的实际工具选择，不代表所有表达或记忆维护场景的模型效果。

2026-11-20 已完成权威存储切换并通过 ATE/GSEA 整库测试、静态检查和定向竞态检查。新路径验证包括：全新本地运行库读取原 ATE 历史、提交后正文清理、上传与追加按所属会话恢复、会话执行锁、Team 恢复范围、原生分页、任务改派历史栅栏、Flow 已完成工具结果复用，以及跨项目经验采用和重新打开后的读取。受控断流恢复检查通过；本轮未重新运行真实模型供应商端到端测试。完整验证范围和未知请求限制见 [迁移记录](./ATE_AUTHORITY_CUTOVER.md)。

显式配置自动经验服务时，维护失败不会改变已经完成的 turn 结果，而会留下 pending maintenance。默认工具路径的维护失败则作为工具结果交给主 Agent；来源撤回、隐私拒绝、版本冲突和未知写入结果仍由 ATE 当前策略与幂等回执约束。
