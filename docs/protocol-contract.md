# Protocol and Wire Contract

本文件记录 `context-lens` 首个核心垂直切片的技术契约。它是实现和验收的依据；若实现便利与本契约冲突，应优先保护 wire authority 和显式操作语义。

## 资料来源

### 官方资料（检索于 2026-08-27）

OpenAI：

- Responses create：<https://platform.openai.com/docs/api-reference/responses/create>
- Responses streaming：<https://platform.openai.com/docs/api-reference/responses-streaming>
- Chat Completions create：<https://platform.openai.com/docs/api-reference/chat/create>
- Chat Completions streaming：<https://platform.openai.com/docs/api-reference/chat/streaming>
- 机器可读定义：<https://github.com/openai/openai-openapi>（`main` at `ecbf3ace93065f90a1e0d0b73a81b11fc084ad52`；本次抓取的 `openapi.yaml` SHA-256 为 `56297492effb0aea65daba949c3891f8724e9f4c3f258695f3dacb5d00a2fa49`）

Anthropic：

- Messages API：<https://docs.anthropic.com/en/api/messages>
- Streaming：<https://docs.anthropic.com/en/docs/build-with-claude/streaming>
- 官方 TypeScript SDK 的消息和流事件定义：<https://github.com/anthropics/anthropic-sdk-typescript/blob/main/src/resources/messages/messages.ts>（`main` at `7ba6a3fc3000f9bd1f6f9f45526cc66db3167e6b`；本次抓取文件 SHA-256 为 `63edc66ad2d37fb39d3bddf455a7efa1cd1f04dff6aa9b937c98ab80073d895a`）

### 本机参考材料

只读目录：

```text
/Users/littlefairy/projects/new-api/logs/relay-debug
```

截至 PREPARE 检查：

- 845 个 `raw-client-input.json`
- 845 个 `raw-input.json`
- 714 个 `raw-output.txt`
- 672 个目录同时含有请求和输出
- 请求结构覆盖 Responses、Chat Completions、Anthropic Messages
- 输出结构覆盖 Responses SSE、Chat Completions SSE、Anthropic Messages SSE
- `raw-client-input.json` 与 `raw-input.json` 在绝大多数目录不同，说明材料包含某个既有链路的改写前后记录
- 多数材料可能含凭据、完整上下文或模型输出

材料的使用规则：

1. 只读分析，不把目录作为运行时依赖。
2. 不整体复制，不把原文放入 prompt、日志或版本库。
3. 需要 fixture 时，选择最小样本，去除秘密和不必要的上下文，保留字段形状、事件顺序和边界特征。
4. fixture manifest 记录来源类别、脱敏方式和“是否仍为原始 bytes”的状态，不虚称脱敏样本是原始捕获。

## 三种协议的观察边界

### Responses

入口路径默认是 `/v1/responses`。请求 body 可能包含：

- `model`
- `input`（字符串或异构 item 数组）
- `instructions`
- `tools`、`tool_choice`、`parallel_tool_calls`
- `reasoning`
- `text.format`
- `previous_response_id`、`conversation`
- `include`、`metadata`、`store`、`stream`
- provider 扩展字段

非流式响应是一个 Response object，重点区域包括 `output` 异构数组、`status`、`error`、`usage`、`previous_response_id` 和 metadata。

流式响应是命名 SSE event。官方定义包含 `type`、`sequence_number`，以及创建、进度、文本 delta、reasoning、工具调用、失败、不完整和完成等事件。bypass 下必须逐字节转发其 body；Responses stream 不应套用 Chat Completions 的 `[DONE]` 终止判断。

### Chat Completions

入口路径默认是 `/v1/chat/completions`。请求 body 以 `messages` 为核心，可能包含多模态 content、tools、tool choice、sampling、response format、audio、logprobs 和 provider 扩展。

非流式响应通常包含 `id`、`object: chat.completion`、`created`、`model`、多个 `choices`、usage 和扩展字段。

流式响应的每个 data JSON 通常是 `object: chat.completion.chunk`，choice 可能多个；usage 可能在末尾单独出现；常见终止 sentinel 是 `data: [DONE]`。代理必须保留所有 choice 和未知 chunk 字段，不能只聚合第一项。

### Anthropic Messages

入口路径默认是 `/v1/messages`。官方请求包含 `model`、必需的 `max_tokens`、`messages`，以及可选的 `system`、`tools`、`tool_choice`、`thinking`、`output_config`、`metadata`、cache control、service tier 和其他扩展。

消息 content 可以是字符串或 content block 数组。常见 block 包括：

- `text`
- `thinking`
- `redacted_thinking`
- `tool_use`
- `tool_result`
- image、document、citation 和 server tool 相关 block

Messages response 是 `type: message`，包含 content、role、stop reason、stop sequence、usage、container 和扩展字段。

流式事件使用命名 SSE，典型顺序为：

```text
message_start
content_block_start
content_block_delta ...
content_block_stop
message_delta
message_stop
```

每个 content block 的 index 对应最终 content 数组位置。Anthropic 官方流由 `message_start`、每个 block 的 `content_block_start` / 若干 `content_block_delta` / `content_block_stop`、`message_delta` 和 `message_stop` 组成；bypass 下必须保留 event name、data 行、顺序和原始 body bytes。`redacted_thinking` 是不透明数据，官方要求在后续请求中原样传回；signature 和 thinking 相关字段不能被代理擅自重造。

### `/v1/models`

`GET /v1/models` 属于首个切片的透明探测入口。它使用同一个 upstream profile 和 header policy，默认把请求送到上游对应路径并把上游响应原样返回；context-lens 不生成虚假的本地模型目录，也不从响应中选择或改写后续请求的 `model`。模型列表失败应显示为上游错误，而不是被替换为空列表。

## Wire authority

每次 HTTP exchange 至少分成四个可比较 artifact：

```text
request.inbound
request.upstream
response.upstream
response.downstream
```

每个 artifact 包含：

```text
artifact_id
stage
direction
content_type
content_encoding
size
sha256
complete
body bytes 或 blob 引用
```

请求 envelope 还保存：

```text
method
path
escaped_path
raw_query
headers（按安全策略脱敏）
```

响应 envelope 还保存：

```text
status
headers（按安全策略脱敏）
trailers（若可用）
start/end timestamps
```

未发生编辑时，默认转发和释放使用原 artifact 的 reader，不经 JSON decode/encode，不经统一响应模型，不经 SSE 聚合器。

## 应用层保真范围

目标是：

- body bytes 必须保持一致
- JSON 字段、未知字段和数字文本不被改写
- bypass 下 SSE 原始 event bytes、顺序、注释、多行 data、id、retry 必须保持一致；完整成功交换必须通过 upstream/downstream body hash 相等验证
- `model` 不被本项目替换
- upstream status、错误 body 和可转发响应 headers 不被包装成另一种协议
- query 的原始编码和顺序保留

普通 Go HTTP server/client 边界不承诺：

- header 原始大小写和排列顺序
- HTTP/1 chunk 边界
- HTTP/2 frame 边界
- TLS 或 TCP packet 字节

必要的认证替换、Host、Content-Length、hop-by-hop header 处理属于代理 envelope 变化；必须以可解释的方式出现在观察警告和 fidelity 说明中。

### Fixture manifest

脱敏 fixture 放在 `tests/fixtures/`，清单放在 `tests/fixtures/manifest.json`。每项至少包含：

```json
{
  "id": "responses-stream-basic",
  "source_class": "local-relay-debug",
  "source_path_pattern": "20260729/**/raw-client-input.json + raw-output.txt",
  "protocol": "responses",
  "request_shape": "streaming",
  "redaction": "synthetic values and shortened text; no credentials",
  "raw_bytes_status": "derived",
  "sha256": "hash-of-checked-in-fixture",
  "checks": ["secret-scan", "json-parse", "sse-parse"]
}
```

`source_path_pattern` 可以指向原始材料的类别或日期，但不得把真实秘密或完整 prompt 放入清单。`raw_bytes_status` 必须区分 `captured`、`derived` 和 `synthetic`。

### Bypass

- 请求 body 可通过 tee reader 复制到 artifact sink，同时直接送 upstream。
- 响应 headers 到达后立即写给客户端，再用 streaming copy 转发 body。
- 观察 sink 不应因为 UI 或磁盘暂时拥塞而阻塞业务流；若捕获不完整，标记 `capture_incomplete`，优先保证流量。
- 不插入 SSE heartbeat、注释或 synthetic event。

### Request hold

- 在操作决定前不建立上游请求，或不发送 request body。
- 不提前提交 downstream status/header。
- `forward unchanged` 使用 inbound artifact。
- `edit and forward` 使用 derived request artifact。
- `manual response` 不访问 upstream，直接写用户选择的原协议 response artifact。

### Response hold

- 从 upstream response 开始就捕获完整 body。
- 在审核完成前不向客户端提交 status/header/body。
- `release unchanged` 使用 upstream artifact。
- `release edited` 使用经过校验的 derived artifact。
- upstream 4xx/5xx 也是正常 response artifact，可以原样审核和释放。

## Header 和 credential 规则

- context-lens 用于入站认证的 Authorization / x-api-key 默认不发送到 upstream。
- upstream profile 的 credential 在服务端注入，UI 只收到 configured 状态和 profile 元数据。
- `anthropic-version`、Anthropic beta header、OpenAI 相关协议 header 和合法 tracing header 按 header policy 转发。
- `Connection`、`Transfer-Encoding`、`Keep-Alive`、`Proxy-Authenticate`、`Proxy-Authorization` 等 hop-by-hop header 不盲目复制。
- header 名称和值必须拒绝 CRLF 注入。
- 自动跟随 redirect 关闭；Location 和 upstream status 按响应策略记录。
- 为保持 compressed body 的 bytes，transport 默认不自动解压；编辑 compressed artifact 时必须显式产生新 encoding 并标记 body changed。

## Upstream profile

profile 只负责：

```text
profile_id
label
origin / base URL
path mapping
credential scheme
credential reference
additional safe headers
network safety policy
timeout / capture limits
```

profile 不负责：

```text
model default
protocol conversion
sampling normalization
stream forcing
```

默认路径映射必须能把三个入口分别送到同协议上游路径。若上游路径不匹配，代理显示 mismatch 并把上游错误交回，不自动转换。

## Inspection projection

inspector 的输入是 artifact bytes 的副本，输出是可丢弃的 projection：

```text
protocol hint
parse status
sections
messages / input items
content blocks
tools
response items
stream events
unknown nodes
warnings
JSON pointer / byte span（若可定位）
```

projection 必须保留 unknown 节点的原始片段或指针。UI 的格式化、折叠、摘要、缩略图和文本提取不改变 artifact。

建议的 inspector：

```text
ResponsesInspector
ChatCompletionsInspector
AnthropicMessagesInspector
SSEInspector
GenericJSONInspector
```

## Mutation contract

编辑永远是显式动作，并绑定一个 base artifact hash：

```text
base artifact + patch operations -> derived artifact + validation
```

原始 artifact 只读。非流式 JSON 先支持 JSON Pointer / JSON Patch 和 raw replacement；SSE 先支持保留 event 外壳的 event-level 编辑。高层文本重排必须使用对应协议的 serializer，并明确显示 `reconstructed`，不能伪装成原样释放。

命令结果必须携带：

- 原始与派生 body hash
- protocol validation result
- 哪些未知字段被保留

## Manual response contract

人工回复不是 upstream response 的投影，而是一个新的、来源为 operator 的 response artifact。它必须按入站协议选择 envelope：

```text
Responses response / Responses SSE
Chat Completion response / Chat Completion SSE
Anthropic Message response / Anthropic Message SSE
```

允许使用结构化表单生成最小合法响应，但同时提供 raw 编辑和校验反馈。不得用一个跨协议的 `TurnResult` 作为最终 wire 权威。

## 取消和错误

- downstream client disconnect 必须取消尚未完成的 upstream request。
- operator abort 必须取消 upstream，并把 exchange 标成 cancelled/aborted。
- transport error、upstream HTTP error、body capture error 和 parser error 分开记录。
- parser error 不应阻塞 bypass。
- upstream 没有响应时，代理可以生成本地 gateway error；必须标明这是本地错误，不伪装成 upstream body。

## MVP 验收矩阵

协议 × body 形态：

```text
Responses JSON / SSE
Chat Completions JSON / SSE
Anthropic Messages JSON / SSE
```

策略 × 动作：

```text
pass/pass
hold/pass + unchanged forward
hold/pass + manual response
pass/hold + unchanged release
pass/hold + edited release
hold/hold
```


## Chat Template observation extension

三种协议的可读上下文观察采用 `protocol adapter → loss-aware Context IR → Qwen ChatML renderer`，不是 protocol 与模板的两两转换。Context IR 是派生投影，不是 wire protocol 或转发输入；必须保留 provider extension、passthrough、unknown item 及 source JSON pointer。Raw 是可折叠 JSON tree，解析失败回退纯文本；Chat Template 是连续的 marker/context 流。SSE 属于 MVP 后段，原始 SSE grammar/bytes 仍按本文件前述规则保留，实时内容通过 typed IR delta 投影到 Chat Template。

完整的 block、折叠、Qwen renderer 和 SSE phase 规则见 [`docs/chat-template-spec.md`](chat-template-spec.md)。
