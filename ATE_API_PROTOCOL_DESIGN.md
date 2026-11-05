# ATE API 与协议设计

> 版本：0.1.0，API 协议审计草案。日期：2026-10-30。  
> 关联：[语义设计](AGENT_TEMPORAL_ENGINE_DESIGN.md)、[实施架构](ATE_V1_IMPLEMENTATION_ARCHITECTURE.md)、[执行 Profile 场景](ATE_EXECUTION_PROFILE_WALKTHROUGH.md)。  
> 本文是 API 语义、Agent-facing 投影和本地 IPC 的集中规范；字段编号、canonical bytes、frame magic 和平台支持仍由 `docs/formats` 与实施阶段冻结。  
> ATE 是独立基础设施。GSEA、Eino、模型供应商和具体 UI 都是外部适配对象，不是本协议的依赖或公共类型来源。

## 1. 协议目标和边界

ATE 同时提供：

1. **嵌入式 Go API**：Go Agent 宿主直接调用同一内核；
2. **独立服务 API**：`ate serve` 拥有 store，客户端通过本地 IPC 调用；
3. **Agent-facing 能力**：模型或 Agent Loop 只看到受保护的渐进查询和版本化 Profile 命令。

三种接入共享相同的对象身份、SnapshotAnchor、权限、错误、幂等和恢复语义。传输、序列化和句柄形态可以不同。

第一版包括命令、查询、上下文展开和本地请求/响应 IPC。广播、订阅、server push、多路复用和远程服务平台属于后续版本，不进入本协议的第一版实现条件。

### 1.1 三层责任

~~~text
Core API
  通用事务、View、Anchor、NodeRef、持久发布和恢复

agent/v1 Profile API
  Query、Task、Execution、LoopStep、Invocation、消息、目标、取消、恢复

Host Adapter
  Agent Loop、模型、工具、Workspace、业务验收、用户界面
~~~

ATE 不提供 GSEA 专用 API。GSEA 可以实现一个外部 Host Adapter，并用本协议做集成验收；ATE 仓库和 module 不导入 GSEA。

### 1.2 绝不直接暴露的能力

Agent-facing 客户端禁止直接调用：

- 任意 `StateOps`；
- `WriteObject`、任意 RootSet 修改；
- `CURRENT`、manifest、segment 或物理 offset；
- 任意 CommitDigest 拼接；
- 预算、epoch、authority、ACL 的普通字段写入；
- 未经 Profile 校验的 Task/Execution 状态更新。

模型提出意图，Host/Profile Controller 负责校验和提交。审计管理员可以有单独的 Verify/Inspect 能力，但它不等于 Agent 写权限。

## 2. 统一身份和引用

### 2.1 逻辑身份

| 类型 | 作用 |
| --- | --- |
| `EntityRef` | `ProfileID + EntityKind + CanonicalLocalID`，例如 agent/Task/T0 |
| `NodeRef` | 带 kind 的 Entity/Event/Revision/Object 引用 |
| `ObjectRef` | 不可变内容对象，直接按 ObjectID 读取 |
| `EventRef` | 不可变发生事实，按 EventID 读取 |
| `SnapshotAnchor` | 持久历史读取锚点：Fence + 可选 Cursor/KnowledgeKey |
| `ViewRef` | 临时查询能力和 generation pin，不是历史锚点 |
| `RequestKey` | Space + 稳定 Principal + RequestID，命令幂等身份 |
| `CorrelationID` | 传输请求关联号，不参与业务幂等或内容摘要 |
| `ContinuationToken` | 固定 View/过滤/排序/投影后的分页或展开位置 |

已有 `SnapshotAnchor` 时，API 另带的 Cursor/KnowledgeKey 必须与 Anchor 一致，否则返回 `AnchorMismatch`。`latest` 只能由服务端一次捕获并返回完整 Anchor，不允许调用者逐字段拼接当前状态。

### 2.2 统一请求上下文

所有读取都拥有同一组语义字段：

~~~go
type ReadContext struct {
    ViewRef      ViewRef
    Anchor       SnapshotAnchor
    KnowledgeKey *KnowledgeKey
    Mode         ViewMode
    Principal    PrincipalRef // 由可信边界绑定；wire body 不自报权限
}
~~~

服务端可以只在 wire 中传 `ViewRef`；核心在 registry 中解析它对应的 Anchor、主体和 pin。客户端不能自行构造一个具有权限的 ViewRef。

## 3. 第一版请求/响应模型

### 3.1 命令 envelope

~~~go
type CommandEnvelope struct {
    ProtocolVersion uint32
    Method          MethodName
    CorrelationID   CorrelationID
    RequestKey      *RequestKey // 有业务效果/幂等要求的命令必填
    Body            TypedBody
}
~~~

`RequestKey` 的摘要覆盖逻辑 Body、Space、稳定 Principal 和前置版本；不覆盖连接、CorrelationID、等待 deadline、重试时间、服务端 TxID/LSN。命令响应丢失时，客户端用同一 RequestKey 对账。

命令结果至少区分：`DurableAck`、`Rejected`、`Conflict`、`WaitInterrupted`、`CommitUnknown`、`StoreFenced`、`CorruptionDetected`、`ResourceLimit`。错误字符串不是协议分支条件。

### 3.2 查询 envelope

~~~go
type ExploreRequest struct {
    Read         ReadContext
    Operation    ExploreOperation // list/inspect/expand/trace/search/read_content
    Layer        ExploreLayer     // entities/events/content
    Subject      *NodeRef
    Filter       *TypedFilter
    Projection   ProjectionID
    ContextMode  ContextMode
    Limits       QueryLimits
    Continuation *ContinuationToken
}
~~~

查询响应统一包含：

~~~go
type ExploreResponse struct {
    Anchor       SnapshotAnchor
    Items        []TypedSummaryOrDetail
    Relations    []TypedRelation
    Continuation *ContinuationToken
    Truncated    bool
    Unavailable  []UnavailableRange
    Coverage     CoverageFence
}
~~~

查询是只读的。查询发现的结果要交给模型，必须经过显式 Observation/KnowledgeImport；查询本身不扩展 KnowledgeKey，也不修改冻结历史 View。

### 3.3 Profile 命令

`agent/v1` 通过封闭的 typed command union 暴露：

- `StartWork` / `GoalAdopted` / `GoalRebase`；
- `Spawn` / `Claim` / `Cancel`；
- `AcceptResult` / `Reconcile` / `Settle`；
- `MessageConsume` / `Observation` / `KnowledgeImport`；
- `MergeProposal` / `CommitMerge` / `PublishArtifact`；
- `Resume` / `RebindPendingAction`。

每个命令由 Profile 注册表声明：schema/transition 版本、命名空间、前置 Root/版本、强内容引用、逻辑关系、唯一动作槽和错误条件。没有 `agent/v1` capability 的 Core-only store 不接受这些命令。

Profile 命令不能携带“模型已验证”“权限已扩大”“外部效果一定未发生”等自声明字段来绕过 Host 校验。

## 4. Agent-facing 渐进查询

### 4.1 能力集合

模型默认只看到少量、职责清晰的工具：

| 工具 | 默认层级 | 作用 |
| --- | --- | --- |
| `list` | Entity/Execution/Step/Activity 摘要 | 发现对象和可展开引用 |
| `inspect` | 一个 NodeRef | 查看类型化详情和来源 |
| `expand` | Context 分区 | 展开 inputs、upstream、downstream、stateDiff、messages、receipts、artifacts |
| `trace` | 有界关系路径 | 查看因果、来源或后果 |
| `read_content` | Object/Blob 范围 | 分块读取输出、patch、ContextPacket 或文件内容 |
| `search` | 固定 View | 结构过滤和明确文本条件，返回候选 |

Agent 不默认从 Event、Commit、Blob 或 Frame 开始。审计模式可以展开这些层，但仍受 Anchor、权限、limits 和当前 grant 约束。

### 4.2 渐进路径

~~~text
Task/Execution 摘要
  → LoopStep 摘要
  → ToolInvocation/Activity 摘要
  → Attempt/Result/Artifact 详情
  → 原始 Blob/ContextPacket 分块
  → Event/Commit 审计细节
~~~

每次操作都返回稳定 NodeRef、关系类型、来源 Anchor、继续位置和截断状态。模型可以选择继续展开，但不能把任意 ObjectID、深度或 projection 当成授权提升。

### 4.3 统一入口和 SDK 方法

wire 协议可以把上面的操作编码为一个 `ExploreRequest` typed union。Go SDK 同时提供语义更清晰的：

~~~go
ListNodes
GetNode
GetNodeContext
Trace
SearchNodes
ReadContent
~~~

这些方法必须等价地执行相同的 View/Anchor/权限/limit 规则。禁止实现一个同时拥有任意查询和任意写入字段的万能端点。

## 5. 本地 IPC 协议

### 5.1 传输

第一版本地服务使用 Unix domain socket（其他平台单独声明支持）和有界长度帧。帧内是版本化 UTF-8 JSON envelope；JSON 只用于传输，不参与 ObjectID、EventID 或 RequestDigest。

单连接顺序请求/响应；不实现 server push、订阅或多路复用。并行客户端可以使用有界连接池。

长度前缀、最大帧、未知字段/枚举、整数、bytes、ID 和错误码进入 `protocol-v1` registry。服务端在分配内存前检查长度。

### 5.2 身份、权限和句柄

- Unix socket 的 OS 身份或本地凭据映射到 Principal；客户端不能在 Body 中自报 admin/Actor/Space；
- 服务端只保存 `session → ViewRef` 归属，核心 registry 独占 generation pin；
- ViewRef 与连接绑定，断线后不能偷偷用另一连接续页；可按 Cursor/Anchor 重新打开 View；
- service 不导入 storage、不实现第二套 receipt/恢复/权限状态机；
- 同一 store 只能有一个 owner，库模式和 server 模式不能同时直接打开目录。

### 5.3 断线、ACK 和未知结果

连接断开不撤销已接收命令。响应丢失时返回未知并用原 RequestKey 对账。`CorrelationID` 可以变化，`RequestKey` 不能为同一逻辑重试变化。

ReadView/Explore 的响应只证明服务端返回了数据；不证明模型已阅读。Agent 处理结果、Observation、下一动作和必要的命令 receipt 按普通 Commit 规则记录。

## 6. 版本、能力和错误兼容

协议版本、Core schema、agent Profile、projection、query index 和 renderer/tokenizer 分别版本化。未知 mandatory method/transition/字段明确拒绝，不当作空成功。

服务启动时声明 capabilities，例如：

~~~text
core/storage-v1
agent/v1
protocol/local-v1
query/explore-v1
~~~

客户端不得根据字符串猜能力。Profile 不兼容只能 inspect/只读或明确失败，不能让新客户端把旧状态当成同一语义。

## 7. 与 GSEA 的关系

GSEA 可以实现：

- Agent Loop Adapter；
- 模型与工具 Executor；
- Workspace/Artifact Adapter；
- UI 和业务验收策略；
- 真实长程任务的外部验收。

ATE 不导入 GSEA 包、不读取 GSEA 内部 schema、不依赖 GSEA 的 SQLite 或 checkpoint。ATE 仓库中不得把 GSEA adapter 作为核心生产包；适配器应在 GSEA 仓库或独立外部仓库，通过本协议接入。

## 8. 冻结和验收

协议进入 `protocol-v1` 前必须有：

- envelope/错误/能力 golden corpus；
- Go 库调用与 SDK/IPC 调用的 conformance 场景；
- Anchor/KnowledgeKey/分页/权限撤回测试；
- Query/StartWork/Spawn/AcceptResult/MessageConsume 的幂等与 ACK 丢失测试；
- 超大帧、未知字段、未知 capability、断线和 ViewRef 回收测试；
- 不同 Profile/renderer 版本的拒绝和只读降级测试。

本文是协议集中草案，不表示 `protocol-v1` 已冻结。
