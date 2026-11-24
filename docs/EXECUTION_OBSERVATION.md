# GSEA 执行观察与排障

交互式和无界面的 `gsea chat` 会在本次会话选定的数据目录下创建 `diagnostics/trace-<UTC 时间>-<随机标识>.jsonl`。默认数据目录是 `~/.gsea`。使用 `--data-dir` 或恢复托管会话时，请到相应的运行目录查找。一次 CLI 调用对应一份轨迹，恢复同一会话会产生新文件。记录从执行入口确定运行目录后开始。此前的参数解析、会话目录选择，以及单独执行的恢复清单查询和放弃恢复，均在本观察范围之外。

## 观察路径

```text
chat → conversation → turn → model request → gateway → tool pipeline
                        └── Supervisor → child run → child turn
              存储调用 ──→ ATE 连接和操作阶段
                           │
上述组件在边界调用 Emit ────┴──→ 有界 channel → 独立写入协程 → JSONL
```

记录器随执行 context 传递。各组件只发布观察事件，执行路径无需订阅或等待诊断结果。没有记录器的 context 不收集这类事件。宿主的模型、工具、审批与 ATE 提交走原有执行路径。恢复写入结果时，以 ATE 事实、回执和本地 checkpoint 为依据。

| 组件 | 可以核对的边界 |
| --- | --- |
| `cli`、`conversation`、`runtime` | 调用开始与退出、会话准备、turn 启动或恢复、模型请求、首段模型输出经过运行时准备转发、turn 结束。汇总含可用的正文及推理摘要字节数、工具调用数。 |
| `modelgateway` | 原生 Responses 流的 HTTP、SSE 与解码边界。其他供应商仍有通用运行时事件，协议字节明细仅覆盖 Responses 流。 |
| `tool` | 工具提议、策略判定、申请和等待审批、开始执行、成功、失败及结果未知。按 `call_id` 对应模型提出的工具调用。 |
| `supervisor` | `agent.spawned` 的 `agent_id` 是父 Agent，`labels.child_agent_id` 和 `labels.child_session_id` 指向新子 Agent。子运行的 `agent.run.*` 事件携带自己的身份、父 Agent 及可用的 Team、Task、Run 标识。 |
| `ate` | 宿主同时将连接生命周期阶段送往诊断记录器。按 `metrics.batch` 关联连接排队、所有权、启动、握手等阶段，或租用已有连接时的 `lease.queued/acquired/finished`。租约的 `queue_us` 是等待取得连接的耗时，`elapsed_us` 从排队开始。底层磁盘操作不逐项采集。 |

每行的 `seq` 表示本次调用内的写出顺序，`at` 是发布事件的 UTC 时间。并行分支应结合 `team_id`、`agent_id`、`parent_agent_id`、`task_id`、`run_id`、`session_id` 和 `turn_id` 判读。一次调用内用 `request_id` 细分模型请求；跨进程恢复时，同时看轨迹文件、`turn_id` 和 `request_id`。已确认的模型决策及工具结果重放会产生重放标记。字段随所在边界填入。诊断只记录标识、受限标签、状态和计数，省略提示词、回答、推理内容、工具参数及结果、URL、凭据和原始响应字节。

## 从会话找到事件

将下面的 `session` 换为实际会话 ID。命令同时查找当前段和上一段，能定位多次恢复运行产生的文件。使用 `--data-dir` 时相应调整 `dir`。

```sh
dir="$HOME/.gsea/diagnostics"
session='chat-示例会话ID'
find "$dir" -type f \( -name 'trace-*.jsonl' -o -name 'trace-*.jsonl.1' \) \
  -exec jq -Rr --arg session "$session" \
    'fromjson? | select(.session_id == $session) | input_filename' {} + | sort -u
```

选中一次运行的路径。实时跟踪时使用当前段，`tail -F` 在轮转后继续按文件名跟踪。

```sh
trace='/替换为上一步选中的完整路径'
trace=${trace%.1}
tail -n 0 -F "$trace" | jq -Rc --unbuffered \
  'fromjson? | {at, component, name, status, agent_id, request_id, labels, metrics, dropped}'
```

回看一次执行时，先读上一段再读当前段。这个函数还可以用于查看父子 Agent 执行链。

```sh
trace_rows() {
  for part in "$trace.1" "$trace"; do
    if [ -f "$part" ]; then cat "$part"; fi
  done
}

trace_rows | jq -Rc 'fromjson? | select(.component == "supervisor") |
  {at, name, status, team_id, agent_id, parent_agent_id, task_id, run_id, labels}'

trace_rows | jq -Rc 'fromjson? |
  select(.name == "responses.stream" and .status == "stream_read_timeout") |
  {at, request_id, turn_id, labels, metrics, dropped}'
```

复制某条超时记录的 `request_id`，回看该次模型请求从连接至结束的全部已记录事件。

```sh
request='替换为实际请求ID'
trace_rows | jq -Rc --arg request "$request" 'fromjson? |
  select(.request_id == $request) | {seq, at, component, name, status, labels, metrics, dropped}'
```

## 判断连接收到的字节去了哪里

`responses.headers` 和 `responses.first_byte` 表示收到响应头和读到响应体首字节。`responses.first_frame` 给出首个解析事件类型，`responses.first_comment` 表示出现 SSE 注释行。`responses.first_output` 表示首次将事件解码成正文、推理摘要或工具调用。`runtime` 的 `model.first_output.forwarding` 表示该段输出到达运行时转发边界。两者的时间差有助于排查本地处理和持久化耗时。交互模式还会给出 `tui` 的 `transcript.block.started`，状态为 `text` 或 `reasoning`，说明 Bubble Tea 消费了该转录块的首段内容。终端实际绘制时间需另行测量。

流有新字节到达时，`responses.activity` 至多每 15 秒输出一次。计数包含刚读入的字节及此前已解析的内容。读操作阻塞期间不会产生新的进度样本。

请求结束后先看 `responses.stream` 的 `status` 和 `labels.stage`。前者标明完成或失败类型，后者可能是 `awaiting_headers`、`http_error`、`headers_without_bytes`、`bytes_without_frame`、`frames_without_output` 或 `decoded_output`。总计 `body_bytes` 是 HTTP Body 返回的字节数，按以下互斥类别拆分。

| 计数 | 含义 |
| --- | --- |
| `frame_bytes` | 已解码 SSE 事件的事件名行、数据行及终止空行。各事件类型的 `responses.frames` 汇总行提供 `labels.event`、`metrics.count` 和 `metrics.bytes`。未知类型归入 `other`。 |
| `comment_bytes`、`control_bytes` | SSE 注释行的字节数，以及独立空行、无数据事件或其他控制行的字节数。注释常用于心跳。 |
| `invalid_frame_bytes`、`unfinished_bytes` | 无法解码或类型不一致等被拒绝的帧，以及已经扫描、尚待派发的事件字节。 |
| `http_error_body_bytes`、`unclassified_bytes` | HTTP 错误正文以及尚未分类的读取量，例如完成帧之后的预读字节或超出扫描器限制的输入。 |

`text_bytes` 和 `reasoning_bytes` 单独衡量已解码内容的 UTF-8 大小，`tool_chunks` 计工具增量块。相对时间 `headers_ms`、`first_byte_ms`、`last_byte_ms`、`first_frame_ms`、`first_output_ms` 与 `duration_ms` 均从此次请求开始计算，尚未到达的边界没有对应时间字段。

收到明确的工具调用封装拒绝时，`responses.stream` 的状态为 `tool_call_format_rejected`。会话会尝试恢复原 turn 一次，在第一笔新模型请求中提示按已公布的函数和参数格式重新生成调用。轨迹以 `model.tool_format.retry` 标记恢复入口，已确认的模型输出和本地工具结果按原回执重放。如果再次拒绝，会话暂停，可通过 `/recoveries` 查看原 turn。新的恢复请求独立记账。

流式请求的超时分类为 `response_headers_timeout`（等待响应头）、`stream_read_timeout`（等待响应体的新字节）和 `stream_event_idle_timeout`（Responses 仍读到字节，但没有推进的事件）。有效正文、推理摘要和工具调用增量会推进事件计时。注释心跳、未完整帧、重复的初始化状态只能表明连接仍活动。持续推进的流可以运行超过空闲阈值。模型超时发生后，会话会等待短暂间隔，从原 turn 重试一次。轨迹以 `model.timeout.retry` 标记恢复入口，已确认的工具结果由持久回执提供。

例如，遇到“收到 32,311 字节，但屏幕无回答”，先看 `responses.frames` 中 `response.created` 占多少，再看 `comment_bytes` 和 `unfinished_bytes`，最后检查是否有 `first_output_ms`。若读取超时，`duration_ms - last_byte_ms` 给出末字节之后的等待时长。这些观测有助于定位最后通过的本地边界。要进一步区分上游模型停产、中转缓存或网络中断，仍需要对应链路的证据。启用此版本前的流量无法回溯分类。

## 丢失、保留与验证

记录器使用容量 1,024 的非阻塞队列。队满或事件超出字段限制时累计丢弃数，写出事件的 `dropped` 字段报告该记录器当时的累计值。CLI 退出时最多等待 500 ms 排空队列。进程崩溃、磁盘错误、超时退出或轮转都可能造成轨迹缺段。关闭正常完成时含 `recorder.closed`。排查时应同时检查丢弃数、是否截断、是否仍在运行；单凭一行缺失无法证明某步未执行。

每次调用使用独立文件，目录和文件创建权限分别为 `0700` 和 `0600`。当前段在下一行将超过 8 MiB 时轮转，同时保留上一段 `.jsonl.1`。打开新轨迹时，后台按最后修改时间清理超过七天、且带完整关闭记录的旧轨迹及其上一段。活动文件和崩溃后缺少关闭记录的文件会保留。

在 GSEA 仓库执行以下本地测试，验证慢写入、队列丢弃、写盘失败、轮转、并行父子作用域和可归类的 SSE 超时。Responses CLI 恢复测试使用受控服务和真实本地 ATE。

```sh
go test -race ./internal/observation
go test ./internal/modelgateway -run '^TestResponsesObserver'
go test ./internal/agentmanager -run '^TestParallelRunsKeepParentAndIndependentObservationScopes$'
go test ./internal/cli -run '^TestResponsesStreamDiagnosticsAndResumeThroughCLI$'
```
