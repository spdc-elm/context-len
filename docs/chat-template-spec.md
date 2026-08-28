# Chat Template Observation Specification

状态：MVP design freeze（Qwen ChatML first）。

本文件定义 Context Lens 如何把三种 API 的请求/响应投影成面向模型上下文的阅读视图。它不改变代理的 wire 行为：原始 body bytes/blob 仍是唯一 authority；所有 IR、格式化文本、折叠状态和 streaming projection 都是 derived data。

## 1. Pipeline

```text
Responses / Chat Completions / Anthropic Messages artifact
  → protocol-specific normalizer
  → loss-aware Context IR
  → Qwen ChatML renderer
  → Raw Tree / Chat Template / live projection
```

不采用三种协议与模板的两两实现。协议 adapter 只负责识别 wire 结构；renderer 不知道输入协议。未知字段和 provider-specific 结构进入 passthrough/unknown，不得静默丢失。

## 2. Context IR

IR 是共享语义核心，不是新的 wire protocol，也不是转发输入。最小 block kinds：

```text
system | developer | user | assistant
tool_definition | tool_call | tool_result
reasoning | thinking | refusal | unknown
```

每个 block 应尽可能带有：

```text
ordered content parts
source JSON pointer / byte span（若可定位）
provider name and providerExtensions
original value / raw argument string（若存在）
```

通用字段表达跨 provider 的共同语义；特殊能力放入 `providerExtensions` 或 provider passthrough。Responses input/output item、Anthropic content block、Chat choice/chunk 等不能因归一化而丢失其原始值。

推荐的工具内容：

```text
ToolDefinition: name, description, parameters, extensions
ToolCall: call_id, name, arguments, raw_arguments, extensions
ToolResult: call_id, ordered content, extensions
```

## 3. Qwen ChatML

第一版内置 Qwen ChatML renderer。视觉中显示模板 marker，例如：

```text
<|im_start|>system
...
<|im_end|>
<|im_start|>user
...
<|im_end|>
<|im_start|>assistant
...
```

UI header 只显示：

```text
Chat Template · Qwen ChatML
```

不显示 accuracy、approximation 或其他额外置信度标签。模板名称是当前 renderer 的唯一说明。

Qwen 的 tools、tool call、generation prompt 等模板细节由 renderer 负责；不能把 Qwen 特殊语法散落在三个协议 adapter 或 React 组件中。

## 3.1 Qwen tool serialization order (verified against official templates)

The Qwen2.5 and Qwen3 `tokenizer_config.json` templates place the tool definitions in the initial `system` ChatML segment, before iterating over ordinary messages. They do not append a standalone tool-definition message after the conversation. In the tool-enabled branch the sequence is therefore, conceptually:

```text
<|im_start|>system
[system content or Qwen default]

# Tools
...
<tools>
[tool JSON definitions]
</tools>
...
<|im_end|>
<|im_start|>user
...
<|im_end|>
...
```

Qwen2.5/Qwen3 then serialize assistant tool calls inside the assistant segment using `<tool_call>...</tool_call>`. Tool results are wrapped in a `user` segment containing `<tool_response>...</tool_response>`; consecutive tool results share that user segment. The renderer may pretty-print JSON for observation readability, but must retain the original argument string and source pointer and must not claim byte/token identity with the model's Jinja template.

Primary references:

- Qwen2.5-7B-Instruct [`tokenizer_config.json`](https://huggingface.co/Qwen/Qwen2.5-7B-Instruct/raw/main/tokenizer_config.json)
- Qwen3-8B [`tokenizer_config.json`](https://huggingface.co/Qwen/Qwen3-8B/raw/main/tokenizer_config.json)


Raw 是 JSON 的结构化检视器，不是 Pretty JSON 文本。JSON 可解析时：

- root 默认展开；普通对象默认展开到浅层，深层对象收起。
- `messages`、`input`、`output`、`choices`、`content`、`tools`、`tool_calls`、`items` 等语义数组显示 item 数量。
- 每条 message 默认收起，但显示 index、role/type、name 和单行内容摘要。
- 大型 arguments、schema、长字符串和未知嵌套默认收起。
- 提供逐节点展开/收起、Collapse all、Expand all、自动展开深度和搜索定位。
- 搜索结果位于折叠节点内时，自动展开匹配路径并高亮。
- 所有节点可指回 source JSON pointer；格式化不写回 artifact。

JSON 解析失败时 Raw 回退纯文本；不得因为 projection 失败阻断 bypass。

## 5. Chat Template view

Chat Template 是连续上下文流，不使用普通聊天产品的左右气泡，也不把每条 message 渲染成孤立的大卡片。marker、role 和 block 类型用轻量底色/左边线区分，内容保持编辑器式连续阅读。

推荐视觉语义：

```text
system/developer  muted blue-gray
user               blue
assistant          green/purple
reasoning/thinking purple-gray, collapsed by default
tool call          amber
tool result        orange-gray
unknown            neutral dashed marker
```

tool call 的参数在 Chat Template 中默认格式化为易读 JSON；同时保留原始 argument string，并允许在 block 内切换 raw argument。格式化只影响显示，不影响 artifact 或命令输入。

message、tool、reasoning block 必须保留顺序。点击 block 可查看 source pointer 并定位 Raw。

## 6. Streaming

SSE 是 MVP 能力，但在 Raw + Chat Template 联合验收后实现。SSE 不作为常驻主阅读 Tab：

- Chat Template 是 response stream 的默认阅读面。
- 实时生成的 assistant text、reasoning/thinking、tool call arguments 和 tool result 更新已有/新建 block。
- stream start/running/completed/failed/cancelled 是 block 或 exchange 状态，不伪造模型内容。
- 原始命名 event、sequence/index、data bytes 保留在 response artifact 和辅助事件 fallback 中。
- Responses、Chat Completions、Anthropic 的 event grammar 与终止规则独立解析。
- 无法归一化的 event 进入 provider passthrough/Raw fallback。

建议的 IR delta kinds：

```text
stream_start
block_start
text_delta
reasoning_delta
tool_call_start
tool_call_delta
tool_result
block_end
usage
stream_end
provider_passthrough
```

前端通过 reducer 消费 delta；不得由组件自行拼接三种协议的 SSE。

## 7. Template extensibility

本周期只交付 Qwen ChatML 内置 renderer。接口必须允许后续增加：

```text
TemplateRenderer.render(ContextDocument)
TemplateRenderer.renderDelta(IRDelta)
```

后续可加入 Llama 3、Mistral、Gemma 和 Hugging Face `tokenizer_config.json` / Jinja template 导入。导入模板属于后续能力，不阻塞 Qwen MVP；导入后仍需保留 common IR、extensions 和 unknown passthrough。

## 8. Acceptance

Phase 1（Raw + Chat Template）：三协议代表性 JSON、工具、reasoning、unknown/source pointer、Raw fallback 和 Qwen marker 流可在本地浏览器观察；artifact hash 不变；测试和 build 通过。

Phase 2（SSE）：三协议 SSE fixture 和本地 mock stream 可驱动 typed IR delta；assistant/tool/reasoning 增量、结束/错误/取消正确；原始 SSE bytes 不被重建；浏览器实时交互和取消证据通过。
