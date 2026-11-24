# 渐进式工具发现与加载设计

日期：2026-10-22（2026-10-26 复核：仍未实施）  
状态：待实施设计；本文件不表示运行代码已经具备这些能力。2026-10-26 对照代码复核：`internal/tooldiscovery`、`internal/discoverytools`、`tools_groups/tools_group/tools_load`、目录/曝光相关存储与迁移（当前 v1–v36）均不存在；第 3、9 节的现状描述已按当前代码校正。  
本提案覆盖宿主已注册工具的发现、模型侧定义加载、目录维护与恢复。现行记忆工具及其使用示例见[跨任务记忆指南](EVOLUTION.md)。

## 1. 结论与已确认的产品契约

采用“稳定分组目录 + 三级信息披露 + 按需加载定义”。模型负责理解需求并选择导航路径，宿主负责目录真实性、预算、版本、持久化和执行权限。不引入外部知识库，不把关键词匹配作为主路由，也不为此另写 Agent Loop。

| 层级         | 模型看到的内容                                        | 不包含什么                            |
| ------------ | ----------------------------------------------------- | ------------------------------------- |
| L1，组目录   | 每组的用途简介、组内工具名称                          | 非常驻工具的参数 Schema、逐工具长说明 |
| L2，组详情   | 该组工具名称、每个工具的简短用途说明                  | 参数 Schema、实现代码                 |
| L3，工具定义 | 选中工具的完整可调用定义：名称、完整说明、输入 Schema | 源码、凭证、执行授权                  |

必须遵守以下决定：

1. 宿主在注册工具或显式维护目录时分组，普通对话复用目录。
2. 模型提出分类与简介；宿主校验后，保存稳定 ID、不可变版本和成员关系。
3. 分组是导航，不是唯一入口。允许跨组浏览、返回目录、直接加载已知工具；一个工具可属于多个组，但始终指向同一工具身份。
4. 分组、查看简介、加载定义都不增加权限。执行仍经过原有 Profile、Policy、审批与 Tool Pipeline。
5. 常用工具可以常驻；足够小的组可以按宿主阈值一次展开到 L3；不强迫每个请求都走满三级。
6. 本轮固定执行快照和目录视图；本轮内仅逐步展开信息。发布工具、重新分类不会在半轮中改变工具含义。

三级是信息边界，不是强制三次模型请求。常驻工具第一次就有 L3；已知名称可直接加载；小组可由 L1 直接整体展开。

## 2. 目标、非目标与规模边界

### 2.1 目标

- 首次请求不携带所有动态工具的完整 Schema，降低无关输入、上下文挤占和选择干扰。
- 不要求模型先猜中检索关键词；通过功能目录和语义简介进行选择。
- 分类有误或工具跨领域时仍可发现，不因单一路径漏掉能力。
- 任何一次请求、工具调用、崩溃恢复都能解释“当时看到了哪些定义、实际绑定了哪个版本”。
- 延续现有 Eino Loop、不可变 Registry Snapshot、统一执行管线、Goal 与 Multi-Agent 边界。
- 正常运行不要求用户手动配置工具分类、上下文窗口或每轮可见工具列表。

### 2.2 非目标

- 不新增向量库、Embedding 服务、外部知识库或模型工具选择训练。
- 不把目录元数据当成可执行指令、权限策略或可信的安全分类。
- 本文的“加载”指将已注册工具的定义加入模型请求。执行器由宿主装配，调用沿用既有审批管线。
- 不在本阶段实现任意深度目录树、每请求 LLM 重排、自动淘汰已加载 Schema、跨 Turn 无条件继承已加载集合。
- 不声称复刻任何商业 Agent 的内部实现；本方案以本项目的可验证约束为依据。

### 2.3 不能被渐进式加载掩盖的限制

Runtime Snapshot 最多包含 1024 个工具，Manifest 上限为 1 MiB（`tool.MaxSnapshotManifestBytes`），保存完整描述与 Schema。按需披露可以减少模型输入。恢复仍需解析完整工具身份清单。

第一版在现有限额内工作；生成压力测试时应同时满足工具总数与 Manifest 字节限制。未来若要支持更多工具，应另立容量改造，不通过删减审计信息或绕开 Manifest 校验“扩容”。

## 3. 当前实现与需要新增的部分

以下是设计时核实、2026-10-26 复核的代码事实，不是新增功能完成声明。

| 当前实现                                                                               | 本方案的变化                                              |
| -------------------------------------------------------------------------------------- | --------------------------------------------------------- |
| `NewTurn` 固定 Registry Snapshot；恢复解析原快照                                       | 保持；额外固定目录版本与派生视图                          |
| Eino Adapter 为快照内工具建立 handler，`Info()` 提供完整 Schema；Turn 使用固定工具快照 | handler 仍来自固定快照；模型侧只投影常驻和已加载定义      |
| `ChatModelAgent` / `Runner` 承担 ReAct Loop；当前未注册任何 Eino middleware            | 保持；使用 Eino 官方 middleware 扩展点                    |
| Turn 的工具定义来自固定 Registry Snapshot，模型请求不再根据 Goal 阶段动态增删工具      | 纳入统一可见列表投影，保留固定快照与 Policy/Approval 语义 |
| 所有执行走 Pipeline、Policy、审批和事件存储                                            | 导航工具同样走管线；额外检查模型调用的请求曝光凭据        |
| 每次模型请求已有上下文估算、压缩、预算与使用记录                                       | 统计实际目录覆盖层和实际可见 Schema，而不是全部注册工具   |
| Turn/Flow 已持有 Runtime Snapshot 恢复引用                                             | 保留整个执行快照，为目录与加载状态增加各自的恢复引用      |

主要对接位置：

- [Turn 建立与快照固定](./internal/agentruntime/factory_turn.go)
- [Eino 装配入口](./internal/agentruntime/eino_turn.go)
- [工具适配](./internal/einoadapter/tool.go)
- [模型请求记录](./internal/agentruntime/model.go)
- [请求上下文准备](./internal/agentruntime/request_context.go)
- [上下文统计](./internal/agentruntime/context_usage.go)
- [统一执行管线](./internal/tool/pipeline.go)
- [工作流前置条件](./internal/tool/turn_guard.go)
- [Runtime Manifest](./internal/tool/snapshot_manifest.go)
- [宿主工具装配](./internal/app/open.go)
- [架构依赖检查](./internal/architecture/architecture_test.go)

## 4. 四种状态必须分开

### 4.1 执行快照 R：能绑定哪些实现

现有 `RuntimeSnapshot`：工具名称、Schema、revision、implementation binding 的不可变集合。它决定本 Turn 能执行的确切实现。

目录简介和分类单独更新。工具的 `Descriptor` 参与 Snapshot ID 计算，原执行快照继续引用原描述和绑定。

### 4.2 目录版本 C：这些能力如何组织

`CatalogRevision`：工具简介、分组说明、稳定 Group ID、工具与组的多对多关系。它不保存执行授权，不决定调用哪个二进制。

目录可以尚未覆盖 R 的全部工具。宿主把缺失或绑定不匹配的工具放入“未分类”组，以确定性规则生成最小简介。即使目录维护失败，新注册的工具仍有可发现的入口。

### 4.3 Turn 视图 V：该 Agent 可以发现哪些能力

`TurnCatalogView = Derive(R, C, Profile, DisclosurePolicy)`，由宿主确定性派生并固定。

先应用 Profile 和宿主 discoverability 约束，再渲染 L1/L2；隐藏工具不能通过组名、成员数、简介或报错侧信道泄漏。对可见成员为空的组不展示。某组全局说明若提及隐藏成员，应使用宿主安全说明模板或可见成员导出的简述；不能临时让模型重新分类。

视图不预先批准执行。一个需要人工批准的工具可以被发现，但执行时仍必须审批。

### 4.4 曝光状态 E：该 Turn 已经向模型加载哪些定义

包含目录焦点、分页位置、已加载 ToolRef、有序加载记录和单调递增的 exposure revision。

第一版中，本 Turn 的 deferred 定义集合只增不减；导航焦点可以替换。达到 Schema 预算后明确拒绝新增，保留原集合。这样避免随意卸载导致历史 tool-call、重试、恢复和缓存语义变得不确定。

每次模型请求还固定 `RequestExposureManifest`：该次**实际发送**的工具引用与曝光版本。执行校验基于调用来源请求，不以“此刻最新 E”代替。

关系是：R 决定绑定，C 决定组织，V 决定可发现范围，E 决定请求披露进度。不存在由 C 反向授予 R/Profile 权限的路径。

## 5. 三级披露的交互与内容契约

### 5.1 L1：第一次看到组用途和工具名称

下面用已注册的工具名演示提议中的目录格式。

```text
工具目录：8 组、46 个唯一工具；本页 2 组。定义按需加载。

workspace — 读取、定位、修改工作区内容
  workspace_read, workspace_grep, workspace_write

memory — 查阅跨任务经验和维护来源
  memory_query, memory_manage

当前成员列表完整；其余组请使用 tools_groups 的 next_cursor。
```

不能把第一次展示悄悄改成“只有组名和组简介”，因为成员名称是已确认的产品契约。

“L1 有成员名”意味着总体目录仍为 O(成员关系数)，多组引用还会重复名称。不能承诺工具数无限增长而首轮 Token 恒定。超预算必须显式分页，见第 11 节。

### 5.2 L2：选组后看到名称和逐工具简介

```text
memory — 查阅跨任务经验和维护来源
memory_query   浏览语义空间、计数、搜索可复用记忆或按引用读取记录。
memory_manage  经审批记录候选或采用，撤回或纠正来源。
```

名称来自 R，简介来自与 ToolRef 匹配的已校验元数据；没有有效简介时采用宿主 fallback。不得让 LLM 生成不存在的名称，也不得把实现敏感信息放入目录。

### 5.3 L3：加载定义，而不是复制 Schema 两遍

模型调用 `tools_load` 后，宿主把对应完整定义加入**下一次**模型请求的 `tools` 参数。下一次请求才是模型实际获得该工具 L3 的边界。

`tools_load` 的 tool result 只返回加载回执、ToolRef、曝光版本与下一请求生效标记，不再把完整 Schema JSON 放在 tool result 中重复发送。

第一版不另设“查看完整 Schema 但不加载”的入口，避免 inspect/load 两套语义。如果以后增加，必须明确 inspect 不赋予可调用资格，并单独计入正文 Token。

### 5.4 快捷路径

- 常驻工具：初始请求直接有完整定义。
- 已知工具：按规范名称直接 `tools_load`，不要求先访问所属组。
- 小组：`tools_group(mode="expand")` 校验成员数和完整 Schema Token 后，可一次把该组全部定义加载到下一请求；失败不部分加载。
- 跨组与返回：可随时 `tools_groups`、切换 `tools_group`；不需要维持强制父子浏览栈。
- 多组引用：同一 ToolID 只注册一个绑定，同次请求中只出现一份定义；重复加载不增加集合成员。已加载 Schema 仍在后续请求中占输入/缓存 Token，重复导航本身也有请求和消息成本，不能解释成“整个 Turn 只计费一次”。

## 6. 身份、版本与数据模型

### 6.1 身份决定

第一版以 **Registry 中的规范工具名称作为 ToolID**，不再让模型传一套难以记忆的长 ID。该名称在快照内唯一；重命名视为新 ToolID，不能静默改名重绑定。未来引入外部命名空间时，由宿主生成唯一规范名称，并独立保留显示名。

ToolID 是逻辑身份；执行与简介校验必须使用 ToolRef：

```go
// 以下为设计类型，不是现有代码声明。
type ToolRef struct {
    ToolID             string
    Revision           string
    BindingFingerprint string // 覆盖实现绑定及规范化完整定义
}

type ToolBrief struct {
    Ref         ToolRef
    Summary     string
    UseWhen     []string // 维护元数据；L2 渲染时有总长度上限
    NotFor      []string
    Locale      string
}

type Group struct {
    ID          string // 宿主分配，语义身份稳定，展示名可变
    DisplayName string
    Purpose     string
    Members     []ToolRef // 多组可引用同一 ToolRef
}

type CatalogRevision struct {
    FormatVersion int
    ID            string // 规范化 manifest 的内容哈希
    Briefs        []ToolBrief
    Groups        []Group
}
```

GroupID 由宿主分配短而稳定的 ID，模型只能引用已有 ID 或提出“新组候选”；不能自作主张覆盖已有组身份。组拆分、合并、含义显著改变时产生新 ID；普通改名不改变语义身份。

### 6.2 哈希与引用方向

- Catalog ID 覆盖规范化说明、成员关系、精确 ToolRef 和格式版本。
- 时间戳、模型用量、提案来源等审计属性独立保存，不影响内容寻址。
- View ID 覆盖 R ID、C ID、Profile fingerprint、disclosure policy hash 和实际派生结果。
- Exposure revision 引用 View ID；请求 manifest 引用 exposure revision 和实际 ToolRef 列表。
- Runtime Snapshot **不反向引用** Catalog/View ID。导航工具 implementation ID 是稳定实现版本，不拼入本轮 View ID，避免哈希环。

同一 C 对不同 R 可以派生不同 V：不匹配的说明降级为未分类，而不是绑定“最新工具”。R、C、V 均不可变；E 通过已提交事件演进。

## 7. 模型辅助分组与目录发布

### 7.1 什么时候运行

1. 宿主注册工具时，依据描述符中的用途元数据做增量分类。
2. 内置工具初始目录：可以由宿主提供种子分组，再让模型提出审阅建议。
3. 显式目录维护：管理员发起重命名、拆组、合组、修订简介或补全未分类工具。

普通用户每次发消息、每轮执行工具、每次恢复都不触发 LLM 分类。新工具完成发布不要求目录模型同步成功；它可先进入未分类组。

### 7.2 模型输入与输出

输入仅包含限量、可信来源的工具名称、完整说明或宿主裁剪的语义元数据，以及已有组的稳定 ID/用途。不要发送工具源码、执行结果、用户文件、环境变量或凭证。大量工具采用增量批处理；整库维护必须有独立总预算。

输出是 `CatalogDraft`：

- `expected_base_catalog_id`；
- 输入工具指纹与精确 revision；
- 逐工具简介、建议复用的 GroupID、新组候选及成员；
- 可选分类理由，用于维护审阅，不进入常规模型目录；
- 不包含可生效的权限、执行入口、resident 标记或审批豁免。

分类模型不开放任何执行工具。调用使用宿主选择的可信 ModelFactory、普通费用台账、独立 purpose（例如 `tool_catalog_grouping`）、超时、取消和资源限额。JSON 中的 provider/model/owner 配置不采纳。

### 7.3 宿主校验

最低校验项：

- 格式版本、字段白名单、编码、大小、字符长度与数量上限；
- ToolRef 属于当前维护输入，名称、revision 和 fingerprint 匹配；
- 已有组引用存在；新组 ID 由宿主分配；重复成员规范化去重；
- 所有输入工具有归属或明确未分类，不能通过遗漏让工具消失；
- 简介不能覆盖真实名称、完整 Schema 或权限属性；
- 说明/组名按数据安全转义，不能注入宿主模板、控制字符或工具协议；
- 维护操作者权限、修改范围、base catalog CAS；
- 分组总量、单工具归属数和元数据总大小预算。

结构校验不能证明语义分类一定正确。用覆盖测试、人工可审阅 diff、未分类入口和多组引用缓解，不伪称“模型简介已被证明正确”。较大的全量重组、合并或拆分要求显式维护确认；正常的受限增量分类可按宿主已批准的维护策略提交。

### 7.4 提交与失败

`Draft → Validated → Committed`。以 `expected_base_catalog_id` 做 CAS；工具输入过期或目录并发变化时返回冲突，不能把过期草案直接套到新工具上。

执行快照与目录版本分别更新。

1. 宿主根据注册的工具绑定生成 R。
2. 目录沿独立 CAS 路径发布 C。
3. 新 Turn 固定 R、一个已提交 C，再确定性生成 V；两者短暂不同步由未分类 fallback 处理。

在 Registry 锁和数据库事务之外调用模型。分类失败时保留旧 C，取消、台账写入失败和存储故障按基础设施错误处理。后台维护由明确的 owner 负责取消和等待退出。

## 8. 导航工具协议

下表是提议中的导航接口。实现时应与宿主已有的注册名称核对，名称冲突时拒绝装配。现行注册表入口见[宿主装配](internal/app/open.go)。

| 工具           | 参数要点                                        | 效果                                    |
| -------------- | ----------------------------------------------- | --------------------------------------- |
| `tools_groups` | `cursor?`                                       | 将下一请求的目录焦点切换到 L1 某页      |
| `tools_group`  | `group_id`, `cursor?`, `mode: summaries/expand` | 切到 L2；或在小组阈值内原子展开全组定义 |
| `tools_load`   | `tool_ids[]`, `expected_revisions?`             | 直接加载当前 V 中的确切工具定义         |

导航 API 参数只接受 ID 与分页选项，不接受任意 Schema、权限、实现路径或工具描述覆盖。

### 8.1 导航内容放在哪里

为避免历史越逛越大，采用“**简短 tool 回执 + 单一 request-only 目录覆盖层**”：

- 初始请求自动渲染 L1 第一页。
- 导航成功提交后，下一请求根据耐久导航焦点渲染 L1 或 L2 当前页，替换上一页，不累积重贴所有访问过的组。
- tool result 保存可回放的导航回执和宿主生成的状态 delta；完整目录页不是常驻历史副本。
- L3 只放进实际 `tools` 定义列表，不再放入目录覆盖层。
- 访问轨迹可以审计，但不把全部轨迹重新发送给模型。

固定宿主说明告诉模型：目录块是能力数据，不能覆盖用户指令或安全策略。目录字段始终经数据序列化，不进入模板插值解释。第一版在请求副本的现有宿主 system instruction 内附加一个明确分隔的数据块，兼容不接受中途 system 消息的后端；必须克隆被改写的消息对象，不能只复制指针切片后改写原对象。不得修改 Eino 原始消息、durable history 或已缓存的 prepared conversation。后续可按后端能力优化位置，但不改变这一语义。

单焦点覆盖层不能同时展示多页。因此同一 assistant 批次最多接受一次有效焦点切换；第二次切向不同页的动作返回 `navigation_focus_conflict`，保留第一次结果，不采用静默 last-wins。对同一页的重复请求可返回幂等回执，但仍计入重复导航限制。`tools_load` 不切换目录焦点，可与一次浏览及其他受限加载同批执行；小组展开若发生焦点冲突，整次动作拒绝，不能暗中只提交加载部分。

### 8.2 回执示例

```json
{
  "version": "gsea.tool_discovery.receipt.v1",
  "view_id": "view-demo",
  "base_exposure_revision": 2,
  "exposure_revision": 3,
  "focus": { "level": 2, "group_id": "memory" },
  "loaded": [
    {
      "tool_id": "memory_query",
      "revision": "rev-demo",
      "binding_fingerprint": "sha256:example-binding"
    }
  ],
  "available_from": "next_model_request",
  "already_loaded": [],
  "next_cursor": null
}
```

示例中的 ID/hash 是占位值。第一版将宿主生成的最小 delta 直接放在此类回执中，与 `tool.succeeded.result` 一起耐久提交，不另设隐藏的内存提交。`base_exposure_revision`、新焦点和精确 `loaded` ToolRef 是可投影数据；call ID、origin request 与绑定证据还要同外层事件和固定 R 校验。回执出现这些字段不表示它本身可信，只有指定原生导航绑定的耐久成功事件能被应用。

工具完整定义在下一请求出现，而非“加载回执返回那一刻，当前 assistant 批次就获得调用资格”。

### 8.3 分页与错误

cursor 是宿主生成的有界不透明游标，绑定 View ID、page-plan hash、层级、组 ID、排序与分页位置，并做完整性校验；不包含凭证。第一版可用固定页 ID 与内容哈希查验，不依赖重启即失效的进程临时密钥。不同 V 的 cursor 不互用。第一版不接受模型传入 `limit` 改写既定分页参数。排序稳定，游标失效不能回退到最新目录。

`tools_group` 默认 `mode=summaries`；`mode=expand` 面向整组可见成员，不允许同时传入分页 cursor，避免“只展开这一页还是全组”歧义。`tools_load` 的名称列表必须非空并去重；提供 `expected_revisions` 时，它只能约束所请求名称，任何一项不匹配都整次拒绝，不能部分成功。

主要错误码：

| 错误                               | 处理                                                     |
| ---------------------------------- | -------------------------------------------------------- |
| `unknown_group` / `invalid_cursor` | 保留原 E，允许返回目录                                   |
| `tool_not_visible`                 | 未知与不可发现工具统一响应，避免泄露                     |
| `stale_tool_revision`              | 不加载，不改绑最新版本                                   |
| `group_too_large`                  | 提示改用 L2 或逐个加载，不部分展开                       |
| `discovery_budget_exhausted`       | 保留已加载集合，提示具体受限维度                         |
| `tool_not_loaded`                  | 可恢复前置条件；仅对可发现工具提示 `tools_load`          |
| `discovery_loop_limit`             | 停止重复导航，返回已知信息与受限原因                     |
| `navigation_focus_conflict`        | 同一来源请求已有不同焦点切换，保留先前页，下一请求再浏览 |
| `exposure_evidence_missing`        | 不执行；作为恢复/协议错误报告                            |

业务层可恢复错误不制造副作用。目录损坏、持久化失败、来源凭据不一致等不能伪装成“工具不存在”。

## 9. Eino 集成：保留 Loop，分离执行集合与曝光集合

当前依赖为 `github.com/cloudwego/eino v0.9.22`（设计时为 v0.9.14，2026-10-26 在 v0.9.22 源码复核以下事实）：

- `ChatModelAgentState.ToolInfos` 控制发送给模型的工具定义。
- `BeforeModelRewriteState` 是官方动态筛选入口；返回 state 会被持久化到 Eino state。
- `BeforeAgent` 修改 Tools 会影响执行 handler；只改 `ToolInfos` 不会新增执行 handler。
- 现成 `dynamictool/toolsearch` 默认有关键词检索，并自行加入工具 handler；不能直接拿来替代本项目的目录协议和 Pipeline。

因此采用一个薄的项目 middleware，而非启用现成 search middleware：

1. `buildTurn` 用固定 R 装配工具 handlers，并遵守 Profile 和宿主硬限制。
2. 新 middleware 在**每次** `BeforeModelRewriteState` 中从固定 V 和已提交 E 重建精确 `ToolInfos`。
3. 计算：`本次完整定义 = 可见常驻工具 ∪ 已加载 deferred 工具`，再应用现有 Goal 阶段等请求级限制。
4. 即使结果为空，也设置 non-nil empty slice；不能依赖 nil，因为 Eino 会在某些路径回填完整工具集合。
5. 不信任 checkpoint 中缓存的旧 `ToolInfos`，恢复后重新由宿主投影覆盖；不采用“首次删减、后面随意 append”的隐式状态。
6. middleware 不把目录块加入 `state.Messages`；那会进入 Eino state/checkpoint。目录只在 `recordingModel` 的出站请求准备边界注入副本。
7. `recordingModel` 负责最终上下文计数、请求 manifest、预算和模型调用，不用一个临时 `WithTools` 覆盖代替 Eino state 投影。

Eino 的状态更新与宿主事件不是一个事务。Eino 的 ToolInfos 仅是可重建缓存，宿主已提交曝光事件与 request manifest 才是事实来源。

目录和曝光逻辑只作用于主任务 Agent 的正常工具调用请求。summary、evaluator、`tool_catalog_grouping` 等辅助 purpose 显式跳过目录注入与导航/曝光 middleware，保留各自独立的结构化输入及原有 tool-free 契约；不能因为复用 ModelGateway 就自动附带目录。

## 10. 请求来源校验、耐久曝光与恢复

### 10.1 为什么必须按来源请求检查

假设请求 Q1 只暴露导航工具，模型在同一 assistant 批次产生：

```text
tools_load(["memory_query"])
memory_query({"operation":"count"})
```

按此设计，Q1 尚未发送 `memory_query` 的定义。同一批中的第二条调用应返回 `tool_not_loaded`。Q2 带完整定义后才允许调用。

因此不能仅检查“当前 loaded set 包含名称”，也不能把前一个 tool result 当作已经完成下一轮曝光。

### 10.2 请求证据

每次 `model.requested` 保存或引用：

- request ID、Turn ID、R/C/V ID；
- exposure revision、disclosure policy hash；
- 按实际 wire 定义顺序记录的 ToolRef 列表及定义 hash；
- 目录焦点、页内容 hash、prepared input hash 与计数结果。

assistant 响应事件保留 `request_id → tool_call_id[]` 关联。由明确的宿主入口构造可信 dispatch origin：`model_request`、`host` 或 `flow`；不能从模型 arguments 中读取来源身份。Eino 顶层的导航和业务调用统一为 `model_request`；缺失或未知 origin 必须 fail closed，不能默认按 `host` 豁免曝光检查。

模型来源调用必须同时满足：精确实现属于 R、Profile 可用、目标定义在其来源请求 manifest 中、当前原有安全前置条件通过。曝光校验应位于审批和执行开始之前，以可恢复 precondition 表达未加载；持久证据缺失则 fail closed。

宿主 Goal verifier、Flow 内部确定性调用没有模型请求 manifest，不应被全局“未展示定义”规则误挡；它们依旧遵守各自 snapshot、Profile、Policy、审批和调用审计。曝光不是另一套权限系统。

现有 Goal 协议允许同批次 `goal_start` 后进入报告流程；必须作为既有 resident 控制协议保留，不因 deferred 规则错误阻断。任何例外必须由宿主代码定义，不能由模型分组标记。

### 10.3 曝光变更的提交点

导航工具先计算纯计划与回执，不在 executor 内提前修改共享 loaded set。计划带 base exposure revision、所需 ToolRef、焦点和下一版本。

导航仍走普通 Pipeline：提议、Policy、必要审批、执行、`tool.succeeded`。只有**原生导航绑定产生的成功事件耐久提交后**，才把其中的宿主 delta 投影为新 E。

- 不以“executor 返回成功”作为提交点。
- 只应用来自指定宿主绑定的持久回执。普通工具结果中即使含有同格式 JSON，曝光状态也保持原值。
- 投影检查 tool 名、确切 native implementation binding、R/V、call ID、base revision、ToolRef 和事件完整性。
- 重放同 call ID 的成功结果不再次增加 revision；新 call ID 重复加载同工具不增加定义成员，不能因此声称其请求和消息不计费。
- 当前工具批次顺序执行；多个加载动作可依次累积 next-request 状态，目录焦点遵守第 8.1 节的单批切换规则。业务调用仍以共同的来源请求 manifest 校验。

若未来并行化导航，需要把“导航计划、base revision CAS、成功提交”收敛到同一宿主提交协议；不能先改 E 再尝试追加审计。第一版保持 turn 内导航串行，不为纯读目录引入无界并行。

### 10.4 崩溃矩阵

| 发生位置                                | 恢复行为                                                 |
| --------------------------------------- | -------------------------------------------------------- |
| 计划已计算，成功事件未提交              | E 不变；导航是无外部副作用的纯计划，可安全重新计算       |
| `tool.succeeded` 已提交，内存投影未更新 | 重放已提交事件恢复 E；不重复加载                         |
| E 已重建，Eino checkpoint 落后          | 覆盖 checkpoint 的 ToolInfos，以耐久 E 为准              |
| 请求已记录，响应未完整落盘              | 沿现有请求恢复/不确定结果协议处理，不猜造 tool-call 来源 |
| 已有 assistant 调用，曝光凭据缺失       | 不执行；报告协议错误，不套用最新 E                       |
| 原 R/C/V 无法解析                       | 报告恢复所缺的清单、目录或视图                           |

现有成功事件写入失败可能形成 `OutcomeUnknown`。即使导航计划本身可重算，也不能绕开 Pipeline 的失败协议，向用户伪报完成。

### 10.5 Checkpoint 与旧数据兼容

Turn 开始保存 discovery protocol version、R/C/V 与初始 E；后续事件重建状态，可加带事件序号/hash 的投影 checkpoint 加速。Eino checkpoint 不成为目录主库。

新 Turn 默认只加载符合策略的 resident 工具，不直接继承上一 Turn 的 deferred 集合。可保留最近使用提示作为已知名称导航辅助，但必须重新对照当前 V 和确切 revision。

旧 Turn 没有 discovery 元数据时，按其原有 eager 协议恢复，而不是中途切换为 progressive；仍须满足既有 host/binding 和 checkpoint 兼容检查，不为旧 Turn 放宽实现身份校验。新协议 Turn 降级时也不能直接发送全部 Schema；需要先结束/归档或重建显式的新 Turn，并重新做预算检查。

## 11. Token 预算、分页、压缩与缓存

### 11.1 统一预算，不双算

每次实际请求满足：

```text
input_tokens(宿主说明 + 单一目录块 + 会话 + 工具结果 + 实际 tools)
  + output_reserve + safety_margin
  <= model_working_window
```

`model_working_window` 继续复用现有模型元数据解析与用户覆盖机制，不从分组名称或模型自述猜测。Token 数优先用当前后端计数与已实现校准；启发式结果显示其估算性质。

目录预算、Schema 子预算是组织输入的约束，不是再次从 context window 扣除同一份 tokens。最终以实际 prepared request 一次总计为准。

### 11.2 请求准备顺序

1. 从 V/E 及 Goal 阶段生成 ToolInfos，固定本次候选曝光。
2. 将原始 conversation、request-only directory、实际 tools 分开建模；不先把目录塞进待摘要历史。
3. 读取本 Turn 固定分页计划中当前焦点对应的目录页，生成出站消息副本；不因本次会话变长而暗改同一 cursor 对应的内容。
4. 统计完整输入与输出预留；按现有机制压缩 conversation，再重新计数。
5. 压缩不改写目录事实、ToolRef、请求来源证据、当前 Goal 或本轮必要工具调用配对。
6. 若最低 resident、固定说明和必要数据仍放不下，发送请求前明确报错；不静默删除关键 Schema，不无限摘要重试。
7. 记录最终 request exposure manifest、预算并 dispatch。

Turn 后评估下一请求压力，沿现有 pending compaction 机制；不额外调用模型重新分类。Turn 后统计也必须使用实际曝光和当前目录，而非 R 中全部定义。

### 11.3 L1/L2 分页规则

- 目录可容纳时，首次列出全部组用途和成员名称。
- 放不下时，首次列出 L1 第一页，明确总组数、唯一工具数、本页范围和 `next_cursor`。
- 大组允许跨页展示成员名，每页重复该组用途，并标 `membership_complete=false`；不能截断后宣称完整。
- L2 分页按工具数和实测 Token 双限；简介超长在目录提交时就校验，不在出站时随意截断语义。
- pages 是同一 V 的稳定切片，按规范化排序，不能因模型每次请求改变顺序。
- 分页计划在 Turn 开始时按固定目录预算与计数策略确定，其 hash 纳入 V；计数策略、模型有效窗口与相关配置也应纳入派生输入。固定页的字节/Token 边界策略必须版本化；同一游标重试应得到同一页。若当前请求连该页都放不下，先压缩会话或明确报预算不足，不偷偷换一个更小页并复用原 cursor。
- 不使用关键词召回替代已确认的目录层级；后续可增加辅助搜索，但不能关闭目录和直接加载入口。

### 11.4 第一版建议默认值

下表是**待实现和实测校准的宿主默认策略**，不是当前配置或商业 Agent 常数。默认以自动策略为主，专家可在受控配置中覆盖，模型不可覆盖。

| 参数                          | 建议起点                                                       |
| ----------------------------- | -------------------------------------------------------------- |
| 组用途 / 逐工具简介           | 各不超过 160 个 Unicode 字符；另有限量编码字节                 |
| 目录元数据 / 分组数量         | 每个规范化版本最多 2 MiB、最多 64 组                           |
| 单工具分组数量                | 最多 3 组；再多应复审组设计                                    |
| L1 每页                       | 最多 8 组片段、32 个成员名称，同时受目录 Token 限制            |
| L2 每页                       | 最多 12 个工具，同时受目录 Token 限制                          |
| 单次 `tools_load`             | 最多 4 个工具，原子校验/加载                                   |
| 小组整体展开                  | 最多 4 个唯一 deferred 工具，且完整 Schema 总量可容纳          |
| 目录覆盖层目标上限            | `min(4096 tokens, 可用输入预算的 10%)`，低预算需保留最小可用页 |
| 总可见 Schema 目标上限        | `min(16384 tokens, 可用输入预算的 20%)`，含导航与 resident     |
| 本 Turn deferred 加载数量上限 | 24；还需满足总 Schema Token 上限                               |
| Turn 导航调用上限             | 12；仍计入原工具调用/迭代总预算                                |
| 连续无进展重复导航            | 3 次后返回 `discovery_loop_limit`                              |

百分比的分母是扣除输出预留与安全量后的输入预算，不是 R 的大小。首次 resident 超出目标时不能裁剪安全协议工具；应报告配置不兼容，或采用另一个已版本化、经过测试的宿主策略。不能以“自动”为理由发送超限请求。

导航限流不重置计数，不自动启动 Goal，也不取消已经可用的业务工具。达到硬上限后的导航继续拒绝，模型可使用已有定义或解释限制；整个 Loop 仍受现有迭代与资源上限约束。

加载动作在提交前用统一 estimator 检查数量、Schema 预算和最低请求空间；最终发送前再计数。最终环境不足时保留 E，压缩或报错，而不是撤销已提交加载。

### 11.5 缓存取舍

resident 定义顺序固定；deferred 按首次成功加载顺序稳定追加，多组引用去重。不每轮重排/重新分组，不因简介修订影响本 Turn。

request-only 目录焦点变化会改变出站内容，`tools` 列表变更也可能影响后端 prompt cache。**稳定排序不等于保证缓存命中**。第一版选择正确性与通用后端兼容，记录缓存成本，后续再验证 provider 原生 deferred tools 或特殊消息布局。

历史摘要不承担保存 authoritative catalog 的职责；摘要写“之前使用了某工具”也不能替代 R/V/E 或曝光证明。

## 12. 权限、Profile、Goal、Flow 与 Multi-Agent

### 12.1 权限边界

- 组元数据不能写 Policy、风险等级、网络/文件能力或自动审批标记。
- `tools_load` 只登记后续请求的可见定义。实际调用单独经过工具管线与审批。
- 导航工具是宿主原生能力，同样走 Pipeline；按现有策略配置为元数据读取/曝光动作，不授予目标动作权限。
- 常驻是曝光优化，不是授权等级。任何工具成为 resident 前，都要满足 Profile 与宿主注册条件。
- 显式空工具 allowlist 仍为空；不得因为“导航常驻”自动塞入工具。保留现有 nil/空集合差异。
- 新建 progressive Turn 时进行 Profile 完整性检查：存在 deferred 工具就必须允许本方案所需的三个导航工具，且它们满足 resident 最低预算。否则报告配置不兼容；只有宿主预先明确选择并验证过的 eager 协议可以另建 Turn 使用。不能自动给 Profile 加导航权限，也不能交付“名称可见但无法加载”的配置。工具集合为空或所有允许工具均已 resident 时不强求导航能力。
- 对需要保密的工具，L1 名称也不能泄露；对可发现但受审批工具，可以显示需要审批的宿主标记，但执行判断以实时 Policy 为准。
- 模型生成的说明是低信任数据；不解释其中命令，不让其覆盖宿主提示词。

### 12.2 控制协议

Goal 工具按现有 durable Goal 状态投影，并保留必须的阶段切换协议。其执行绑定仍固定，本次曝光列表的阶段限制不能简单当作持久禁用。

Flow 内部执行依据宿主注册的 Flow 与其固定快照；模型能够发现 `flow_run` 不等于可以通过它绕开 Profile 或扩大内部工具集合。内部动作保持原 Pipeline、审批、确定性 call ID 和恢复协议。

### 12.3 Multi-Agent

根 Agent 和 child Agent 共享可复用的目录内容，不共享可变曝光状态。

- 各自根据 Profile 派生 V，拥有独立 Turn 的 E 与请求 manifest。
- parent 提供工具名称建议只是导航信息，不能给 child 加权限。
- 同名工具仍按 child 的 R 解析；不同 Turn 不以名称相同假定 revision 一致。
- 目录维护、分类用量归属明确的宿主维护任务，不偷偷消耗某个 child 的用户任务预算。
- Team 总费用与资源限额依旧生效；导航不能成为绕开 max iterations 的隐藏循环。

## 13. 工具快照、目录维护和恢复引用

工具绑定变化后，新 Turn 根据新快照派生视图。已开始的 Turn 按原 R、C、V 与绑定继续执行。

若 C 有精确匹配的 ToolRef，新视图可复用其说明。对新旧实现不能确认匹配时，将工具归入未分类组。

引用生命周期：

- Turn 继续保留**完整执行快照**，不因只曝光一小部分就释放其他 revision；之后可能仍需加载，也可能存在 Flow/恢复消费者。
- 目录版本、View 和曝光事件被可恢复 Turn、审计请求及明确的维护消费者引用。
- 删除目录元数据前检查目录消费者。关闭 Turn/Flow 恢复后，宿主再按关闭回执释放对应的工具快照引用。
- 已归档的历史可保留目录与请求证据。能否继续执行取决于原始绑定和本地恢复资源。

每项目录清理规则都应指定目标元数据、计划和回执。评测数据与历史会话各循已有的维护策略。

## 14. 模块架构与接口责任

建议新增边界如下，类型和文件名可在实施时做小幅调整，但依赖方向不变：

```text
internal/tooldiscovery
  model / identity / catalog validation / deterministic view
  paging / token-aware planning / exposure projection / store ports

internal/discoverytools
  navigation JSON protocol / native tool bindings / pure load plans

internal/agentruntime
  Eino exposure middleware / request manifest / model-origin guard
  request-only rendering / compaction integration / grouping model adapter

internal/orchestration/atestore
  exposure records and catalog updates in ATE

internal/app
  composition / publication integration / maintenance lifecycle / defaults
```

`tooldiscovery` 不导入 Eino、provider SDK、SQLite、`app` 或 TUI。它可通过显式端口依赖所需工具元数据和 TokenCounter，但不能持有全局容器或从上下文取 service locator。

`discoverytools` 不直接调用模型、不操作 Eino state、不修改权限。Eino Adapter 保持通用：不在通用适配层硬编码全部导航业务。

示意端口职责：

| 端口                      | 责任                                                |
| ------------------------- | --------------------------------------------------- |
| `CatalogStore`            | 读不可变版本、以 base ID CAS 提交版本、维护引用     |
| `ViewBuilder`             | 根据精确 R/C/Profile/策略纯派生 V                   |
| `ExposureProjector`       | 从可信事件重建 E；重复事件幂等                      |
| `DiscoveryPlanner`        | 计算分页、加载预算、next exposure delta；不提前提交 |
| `GroupingProposer`        | 接收限量元数据，返回草案；不发布、不授权            |
| `RequestExposureRecorder` | 固定请求实际定义与来源关系，供执行验证              |

目录、模型调用、工具装配和审批各由对应模块负责。修改依赖时同步更新架构 guard 的精确白名单。

## 15. 存储、事件与迁移设计

### 15.1 耐久数据

建议逻辑记录如下；可复用现有 blob/事件存储，不要求每项都建一张表：

| 记录                       | 关键约束                                                     |
| -------------------------- | ------------------------------------------------------------ |
| catalog revisions          | ID 唯一、规范内容 hash、格式版本、不可变                     |
| active catalog pointer     | CAS base ID；一次只指向一个已提交版本                        |
| catalog drafts/audit       | 提案来源、校验报告、模型账单、维护确认；不参与目录内容 hash  |
| turn discovery binding     | Turn 唯一；R/C/V/协议版本/策略 hash 固定                     |
| exposure events/projection | 有序 revision、可信事件来源、base revision、幂等 call ID     |
| request exposure manifest  | request ID 唯一；精确工具顺序/hash、目录 hash、曝光 revision |
| references                 | 明确 consumer kind/id，支持 plan/receipt 与恢复关闭          |

按现有 SQLite append-only migration 规则新增迁移；不能改写已发布 migration 或假定固定“下一个编号”。内存 store 与 SQLite store 的语义必须一致。

### 15.2 事件协议

建议新增 `discovery.turn_bound`、目录发布审计、请求曝光 manifest 引用等版本化 payload。导航状态的权威变更复用可信 `tool.succeeded` 中的宿主 delta；可以建立派生 `discovery.exposure_applied` 索引，但不能有两个互相矛盾的提交点。

如果为了事务需要在同一 EventStore append 中写多条事件，必须明确实际原子性；不能把两次独立 append 当作事务。投影可以落后，只要能够从已提交事件确定性重建。

后端重试、流式和非流式请求都必须产生相同的曝光证据语义。模型输出不完整时不能提前执行尚未 durable 记录的 tool calls。

## 16. 后端兼容与能力降级

第一版使用普通 `tools` Schema 列表和宿主导航工具，覆盖现有 OpenAI-compatible Chat/Responses 与 Ollama 路径。兼容性需要逐 adapter 测试，不能仅凭 base URL 或模型名称宣告支持。

Eino 已有 `DeferredToolInfos` 不代表本项目当前 ModelGateway 与远端服务完整支持原生 deferred tools。第一版保持该路径关闭，避免同时发送全量 deferred 定义而抵消节省效果。

以后可引入 capability-negotiated 原生检索适配，但必须同时满足：Schema 传输语义、计数、请求审计、精确实现绑定、调用曝光证据与恢复行为通过测试。目录和权限契约不随 provider 变化。

关闭 progressive 功能只影响新 Turn，使用 eager 或 progressive 的协议版本随 Turn 固定。eager 也必须执行输入预算校验；不将“发送所有工具”当作无条件保底。

## 17. 配置与可观测性

默认用户只需选择模型和正常启动。宿主配置可选包含：模式、resident allowlist、分页/Schema/导航预算、小组展开阈值、目录维护模型和维护预算；配置在 Turn 开始时冻结并生成 hash。

resident 初始集合建议采用导航工具与少量工作区、Artifact 和控制工具。具体名单由宿主依据使用场景和实施测试确定。

建议状态栏显示：

```text
ctx 18k/128k  tools 9/68  schema 3.2k  catalog 0.8k
```

含义：9 为该次实际暴露的唯一定义数，68 为本 Agent 可发现的唯一工具数；多组成员引用不重复计数。`ctx` 是完整有效请求上下文，不是只算消息正文。空闲时应标明“上次请求”或“下次估计”，避免把不同快照数据拼在一起。

最少记录：

- R/C/V ID、exposure revision、每次请求的 schema/catalog/conversation tokens；
- 导航次数、加载次数、重复加载、无进展次数、未分类数量；
- `tool_not_loaded`、分页/预算拒绝、未知 ID、版本冲突；
- 目录维护延迟/费用/失败原因、CAS 冲突；
- 恢复重放次数、缺失曝光证据、被拒绝的伪造 delta；
- 首次正确业务调用前的导航轮数、端到端耗时、实际 cache usage。

日志不输出 API Key、源码或整份敏感 Schema/用户内容。正常运行用 hash 与 ID 关联，详细目录调试输出需明确启用。

## 18. 实施顺序与交付门槛

这是一份后续实施计划，本轮仅交付设计文档。

| 阶段                | 工作                                                                | 进入下一阶段的条件                             |
| ------------------- | ------------------------------------------------------------------- | ---------------------------------------------- |
| D1：领域与耐久目录  | ToolRef/Group/Catalog、校验、CAS、fallback、确定性 V、迁移          | 身份/并发/权限过滤/旧数据测试通过              |
| D2：导航与曝光状态  | 三个原生工具、分页、纯计划、可信成功事件投影、预算与幂等            | 崩溃矩阵和伪造结果测试通过，尚不默认启用       |
| D3：Eino 与请求集成 | 精确 ToolInfos、request-only 目录、来源 manifest、前置 guard        | 真实 Eino 流式/非流式、Goal/Flow/恢复不回归    |
| D4：统计与体验      | 实际 ctx/tools 统计、缓存观测、导航错误提示、小组快捷展开           | 统计能与最终 prepared request 对账             |
| D5：分组维护闭环    | 模型草案、维护预览、发布准备集成、CAS、失败保底                     | 无每轮分类、无锁内模型调用、维护生命周期可关闭 |
| D6：发布与稳定性    | Profile/Multi-Agent、恢复引用生命周期、旧 Turn 兼容、文档与默认策略 | 验收矩阵通过，再对新 Turn 默认启用             |

每阶段保持可单独评审与回滚，不同时重写整个 Agent Runtime。测试不通过时保留原 eager 默认；不拿真实用户任务验证未完成的耐久协议。

## 19. 测试与验收

### 19.1 确定性与数据层

- C 相同规范内容得到相同 ID；顺序规范化、重复成员、多组引用都稳定。
- 简介变化不改变 R；工具 revision/schema/实现变化使旧简介匹配失败并降级。
- 错误 ToolRef、未知成员、超大字段、控制字符、权限注入、草案遗漏被拒绝或按明确规则降级。
- CAS 并发、模型维护取消、存储失败，不污染 active catalog。
- V 先过滤再渲染；空 allowlist 不新增导航，隐藏名称和计数不泄漏。
- deferred 工具与导航 allowlist 不完整时启动失败，不静默扩权；显式 eager 配置按自身预算校验。

### 19.2 导航与执行协议

- 第一次的每个组片段同时有用途和成员名；L2 有逐工具简介；L3 完整且只在实际 tools 中出现一次。
- 分页完整性可穷举：无丢失、无重复遗漏、cursor 与 V 绑定、预算变化不偷偷改页。
- 已知名称直接加载、跨组返回、同工具多组引用、小组展开成功/超预算均符合契约。
- 多工具加载原子拒绝、重复加载幂等、导航无进展限流可预测。
- 同批次多个不同焦点切换只接受第一次，后续冲突不覆盖页面、不部分加载；多个合法加载可累积。
- 同批 `load(X)+X()` 不通过来源 Q1 的曝光校验；Q2 正常调用 X。
- 模型幻觉调用未加载工具只得到 recoverable precondition，不进入审批/执行。
- 伪造回执、普通文本里的假工具 ID、错误的宿主绑定都应被拒绝。
- model/host/flow origin 只能由宿主构造；后两者不受错误曝光规则阻断，也不绕过权限。

### 19.3 真实 Eino 与恢复

- 通过真实 `ChatModelAgent/Runner` 和脚本模型测试，不用自写循环模拟集成成功。
- Generate/Stream、重试、nil/空 ToolInfos、Eino checkpoint 旧列表均不泄漏全量 Schema。
- 目录切页、压缩和恢复均不修改原消息对象；历史、摘要与 prepared conversation 不残留旧目录副本。
- summary/evaluator/分类等辅助 purpose 没有目录与导航工具；未知 origin 不被当作宿主调用放行。
- 每个成功提交前后注入失败，验证内存/事件/checkpoint 的恢复一致性。
- 保留现有 Goal 同批调用、普通问答、审批暂停恢复、Flow 回放和 child Agent 的行为。
- 注册表与目录维护期间，在途 Turn 保持原快照。旧协议 Turn 按旧协议恢复。
- 保留原快照引用；归档及引用释放测试确保不提前释放后续可能加载的工具身份。

### 19.4 Token 与质量测试

用本地合成目录构造少量、百级、接近现有限额但不越界的工具，包含相似名称、交叉领域、错误分类、缺失分类和复杂 Schema。不下载大 benchmark，不默认调用付费模型。

先做不依赖真实模型的强制验收：

1. 存在 deferred 工具的 progressive 请求，不包含其未加载完整 Schema。
2. 相同会话下对比 eager，报告首请求输入及 schema/catalog 分项，不能只比较 schema 忽略目录成本。
3. 所有请求输入加输出预留不越有效窗口；压缩、统计与实际发送内容一致。
4. 同一个场景的恢复后可见集合与原轨迹一致，无重复执行、无越权、无版本漂移。
5. 目录中每个可发现工具均可通过分页到达，也可在已知名称时直接加载。

实际模型语义导航质量另做**显式授权、固定模型/预算**的小样本对比：

- 任务包括直接已知工具、正常分组、跨组、误分类和工具不适用。
- 指标为正确工具选择率、任务完成率、首次正确调用前轮数、总输入/输出/缓存 Token、端到端耗时。
- 对照 eager 与 progressive 的相同任务、同模型、同权限和相同执行验证标准。
- 同时报告失败与样本数，不以某一轮 Token 减少代替任务成功，不预写未经测得的提升百分比。
- 默认启用前要求安全/恢复强制项全部通过；语义效果及可接受时延需根据实测明确门槛后决定，不能凭本设计宣称已经达标。

## 20. 关键风险与取舍记录

| 风险/选择                  | 决定与后果                                                            |
| -------------------------- | --------------------------------------------------------------------- |
| L1 成员名称仍很长          | 保留用户契约；预算内全显，超预算明确分页，不承诺常数成本              |
| 分类错误                   | 多组引用、未分类入口、返回/跨组和已知名称直接加载，不建立唯一检索闸门 |
| 模型维护简介不可信         | 仅作数据；宿主结构与绑定校验，不授予权限；语义质量另验                |
| 目录与工具清单更新时点不同 | 按独立版本核对 ToolRef，为尚未匹配的工具提供未分类入口                |
| 已加载集合持续膨胀         | 第一版 Turn 内单调有界，超限拒绝；卸载/LRU 另立协议                   |
| 新增导航轮次               | resident、直接加载、小组展开缓解；按端到端成本实测                    |
| prompt cache 失效          | 稳定版本与顺序降低无意义变化，但不保证后端命中                        |
| Eino 状态与事件不一致      | Eino 状态可重建；durable 成功事件与请求证据为准                       |
| 新旧 Turn 语义混用         | Turn 固定协议版本，默认开关只影响新 Turn                              |
| 目录元数据驱动错误释放     | 工具快照/目录元数据引用分开审计，不按已加载子集释放 R                 |
| 模型把分组当权限           | discoverability、exposure、authorization 三者分离并分别测试           |

## 21. 完成定义

后续实现只有同时满足以下条件，才能宣布“渐进式工具发现已完成”：

- 本文确认的 L1/L2/L3 与快捷路径均可在真实 Eino Loop 使用。
- 模型辅助分组可维护、可校验、可持久化，不在每轮重新运行。
- 完整定义只按实际曝光发送，统计与模型请求对账。
- 权限、来源请求、版本、恢复、并发和清理边界都有自动化测试。
- 新旧 Turn 兼容、失败保底、目录维护失败和模型超限都有明确用户可见结果。
- 已跑过受控场景对照，公开实际成本与失败，不把设计预期写成已测成绩。

在这些验收前，README 的“已实现”列表不得把本方案写成已交付能力。
