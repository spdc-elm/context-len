# Context Lens — Session 重建与观察面板 spec

状态：已实现（Phase A 面板投影、Phase B 位置链归属、Phase C 合并视图均已落地）。本文是左侧面板改造、上下文占用、session 去重与合并视图的唯一设计来源。wire/协议边界以 `docs/protocol-contract.md` 为准，运行时接缝以 `docs/runtime-contract.md` 为准，Chat Template 投影规则以 `docs/chat-template-spec.md` 为准。

## 1. 目标与非目标

目标：

- 左侧 exchange 队列提供真实信息量：preview、model、上下文占用、message 数，并可筛选。
- 把同一 harness 会话内 append-only 的多轮请求重建成一棵 session 树，默认按 session 分组显示，替代逐条堆叠。
- 精确识别 fork（同前缀不同后缀）与 rollout/repeat（完全相同的请求体重复发送）。
- 合并视图中，一个 session 的 tool call → tool result → 下一轮 assistant 回复在同一个连续上下文流里阅读。

非目标：

- 不做跨协议合并；session 匹配只在同 protocol 内进行。
- 不改变 wire/artifact authority；本 spec 全部为观察侧 additive 投影，不进入转发路径。
- 不内置 tokenizer、不估算 token 数。
- 不把原始 body 写入 SQLite；SQLite 只保存可恢复的 metadata、session/exchange 位置与 artifact/blob 关系。默认运行仍是 ephemeral，进程退出后内存索引消失；设置 `CONTEXT_LENS_DURABLE=1` 后才启用 restart-safe 的本地 catalog 与文件 artifact 恢复。session 位置会从已保存 snapshot hydration 重建，但位置索引本身不是独立远端服务。

## 2. 左侧 Exchange 队列改造

### 2.1 行内容

三种聊天协议（Responses、Chat Completions、Anthropic Messages）的请求行移除 method/path 行：protocol 分类已表达该信息。非聊天透传路由（如 `GET /v1/models`）保留 `method path` 作为身份行。

session 视图的行（一棵树一行）：

```text
● Chat Completions                    [completed]
  qwen3-235b-a22b · 18 msgs · 52.3k ctx · 2 forks
  帮我看看这个函数为什么死锁，偶尔在 CI 上…
  12:34:56
```

字段来源：

- protocol dot 与 state pill：现有字段。
- model：session 内最新轮次的 model。session 中途换 model 不断链，行内显示最新值，多 model 时加标记（如 `qwen3-235b ⁺¹`）。
- message count：最新轮次请求的规范化消息序列长度。
- ctx：最新完成轮次的上下文占用（见第 3 节；未知显示 `—`）。
- preview：首条 user 消息文本截断（约 96 字符、折叠空白、单行）；无 user 消息时回退到任意消息的首个文本块；再回退到 `model · N msgs`。preview 始终取自原始 inbound artifact。
- fork 数、rollout 计数（`×N`）、更新时间。
- Session 行点击最新轮次并进入跟随模式：同一 session 捕获到新的 `Tn` 时，选中项自动移动到该最新轮次；展开/折叠只改变树的可见性，不改变跟随状态。点击具体成员 `Tn` 则固定到该 exchange，后续新轮次不抢占选中项。

flat 视图（可切换，gate 调试用）保持现有单 exchange 行，但应用同样的字段升级：移除 URL、加 preview / model / ctx。exchange_id 从行内移除（详情面板仍可见）。

非聊天透传请求在 session 视图归入折叠的 "Other traffic" 组。

### 2.2 筛选

客户端过滤，无需后端搜索接口：

- free text：匹配 preview、model、session_id。
- protocol、state、model 的 chip/dropdown 筛选。
- session / flat 视图切换。

## 3. 上下文占用（token）

定义：单个请求的上下文占用 = upstream 在对应响应中报告的 input/prompt tokens。它反映实际转发到 upstream 的 body（wire 真相），是 provider 自己的 tokenizer 计数，不引入估算。

| 协议 | 非流式 | 流式 |
|---|---|---|
| chat_completions | `usage.prompt_tokens` | 仅当请求带 `stream_options.include_usage` 时末 chunk 携带 |
| responses | `usage.input_tokens` | `response.completed` 事件 |
| anthropic | `usage` | `message_start` 事件 |

anthropic 的上下文占用 = `input_tokens + cache_read_input_tokens + cache_creation_input_tokens`；`input_tokens` 不含缓存命中，不加和会低估真实上下文。

显示规则：

- flat 视图：每个 exchange 行显示该请求的占用。
- session 视图：session 行显示最新完成轮次的占用。
- 合并视图：每轮的 boundary 处显示该轮占用。
- 拿不到（流式未带 usage、dropped/manual 轮次、尚未完成）一律显示 `—`，不用字节数近似。
- usage 与 preview 同属 capture 时计算的 additive summary 投影；usage 字段在 response 完成时随 snapshot 更新回填。

## 4. Session 归属算法

### 4.1 规范化消息序列

每个请求在 capture 时复用 `backend/inspection` 的协议解析，构造规范化消息序列：

- chat_completions：`messages` 原样（system 即 `messages[0]`，天然在序列内）。
- anthropic：`[system（顶层，string 或 blocks 规范化）] + messages`。
- responses：`[instructions（顶层）] + input items`；顶层 system/instructions 作为虚拟首元素，使三个协议语义统一：改 system 即改历史，断链。

`tools` 与 `model` 不进入序列身份（见 4.4）。

序列化规则：对象 key 排序、稳定字符串转义、UTF-8；数值按原文保留（`1` 与 `1.0` 视为不同消息，宁可断链不误合）。每条消息得到 `digest(msg) = SHA-256(canonical bytes)`。

### 4.2 位置链

```text
H_1 = SHA-256("ctxlens-pos/v1" ‖ protocol ‖ digest(msg_1))
H_m = SHA-256("ctxlens-pos/v1" ‖ "chain" ‖ H_{m-1} ‖ digest(msg_m))
```

位置表只注册请求的 tip（完整上下文 H_m），不注册中间前缀。这是推导后的设计决策：append-only 会话的下一轮总是延伸上一轮的 tip，同前缀不同后缀的分叉同样以 tip 为分叉点；而以中间前缀为父会把 fork 归到错误的轮次深度。tip-to-tip 边同时是匹配、轮次深度和 fork 归属的正确依据，且内存开销降为每轮一个位置。

归属判定（k = 请求链中已存在于位置表的最深 tip）：

| 情形 | 判定 |
|---|---|
| k == m | 该完整上下文已是注册 tip：rollout/repeat 兄弟（完全相同的上下文再次发送，如并行采样、重试）；repeat 计数 +1 |
| 0 ≤ k < m | 正常续轮；若父 tip 已有其它后继，则标记 fork，新分支挂在其下 |
| k 无命中 | 新 session 根，分配新 session_id |

fork 语义：同一父 tip 的不同后继在同一 session 树上形成两条分支，后到的分支点打 fork 标记。rollout 语义：同一轮位置上并列多个 exchange（相同上下文的多次采样）。在 tip 表中无法定位的非 tip 分叉点（例如首个请求内部的改写）保守地成为新 session，不误合。

### 4.3 计算位置与数据来源

归属计算是 capture 时的 additive 投影（与 `stream_tap` 同构）：只读原始 inbound artifact 的字节副本，不进入转发路径，不影响 gate 状态机。gate 编辑后的请求归属仍按原始 inbound 计算——session 结构描述 harness 的行为轨迹，harness 自己维护的才是 append-only 历史。

`previous_response_id` 特例：Responses 的服务端状态会话只发送增量，无全量历史，前缀匹配不适用。此时按显式链归属：`previous_response_id → 上一 response.id`，串成 session；首个请求（无 previous_response_id）为根。

### 4.4 软硬信号

| 变化 | 处理 |
|---|---|
| system / instructions（虚拟首元素） | 硬切：新 session |
| 历史消息被改写（compaction 等） | 硬切：前缀断裂，新 session |
| tools 集变化 | 软信号：同 session，轮次 boundary 标记 |
| model 变化 | 软信号：同 session，轮次 boundary 标记，面板 model 显示更新为最新 |
| 协议变化 | 不同 session（匹配域隔离） |

### 4.5 边界与生命周期

- 位置索引为进程内存态；未启用 durable 时重启即清空。启用 durable-local 时，启动从 SQLite 保存的 session/exchange metadata hydration，artifact body 仍按需读取。
- 容量上限：位置表设全局上限，超限时按 session 活跃度 LRU 整树淘汰；被淘汰 session 的后续请求成为新根。上限可配置。
- 同一开头的两个独立会话（相同 system + 相同首条 user）会合入同一棵树，表现为同上下文的两个分支/rollout——与 Polar 的前缀定义一致，属接受的行为。

## 5. 数据模型与 API

全部为 additive 字段，不修改、不删除现有 snapshot 形状：

- `exchange.Snapshot` 增加 additive `session` 对象：`session_id`、`position`（H_m hex）、`parent_position`、`depth`、`repeat_index`、fork 起点 depth（非 fork 省略）。
- `exchange.Snapshot` 增加 additive `summary` 对象：`model`、`message_count`、`preview`、`tool_names`、`context_tokens`（response 完成时回填）。
- `exchange.Snapshot` 增加 additive `session` 对象：`session_id`、`depth`、`position`、`parent_position`、`parent_exchange_id`、`repeat_index`、`fork`、`model_changed`、`tools_changed`、`root`。
- 面板的 session 分组是前端纯推导：从快照的 session 字段构建树（parent_exchange_id 连边，同 position 为 rollout 组），事件流更新快照时即时重算，不新增后端聚合端点（避免两处树逻辑漂移）。workspace HTTP/SSE 层不改语义，仅透传新增字段。

## 6. 合并 Session 视图（Phase C）

结构来自 harness 行为（原始 inbound 链），轮次内容来自 wire 真相（实际转发的 artifact）：

```text
req₁ 的初始上下文（inbound messages）
+ resp₁ 的 assistant blocks（来自 response artifact：含 reasoning/thinking、tool call）
+ req₂ 相对 req₁ 新增的消息（剔除对 resp₁ 的 canonical echo：tool result、新 user turn）
+ resp₂ …
+ 当前 streaming response（复用现有 live block，含 tool call 参数增量）
```

- assistant 轮内容以 response artifact 为准（harness 下一轮常剥离 thinking/reasoning，response 才是权威来源）；interstitial（tool result、新 user 消息）以 request 为准。
- echo 消除按 role + 规范化内容与 response 派生消息匹配；匹配失败时不静默丢弃，两份都显示并打 "canonical echo 不一致" 标记。
- 编辑过的轮次渲染实际转发的 derived artifact 并打 edited 标记；manual_response 轮次渲染人工响应；dropped 轮次显示占位标记；这些轮次无 upstream usage。
- boundary 线：tools 变化、model 变化（含 old → new）、fork 点、rollout 分组，均以分隔线 + 标签呈现。
- 渲染复用 Chat Template 现有 block 体系（assistant/reasoning/tool call/tool result/unknown），合并视图本质是把 N 个 exchange 的 IR 串成一个连续上下文流；live 流式尾部复用现有 live block 与滚动行为。

## 7. Edge cases

| 情形 | 行为 |
|---|---|
| context compaction / 历史改写 | 前缀断裂 → 新 session（保守正确） |
| subagent / 并行 agent | 各自独立消息历史 → 独立分支或 session |
| 中途改 tools / model | 同 session，boundary 标记 |
| 完全相同请求体重发 | rollout 兄弟，`×N` 计数，不开新轮 |
| gate 编辑 | 归属按原始 inbound；合并视图渲染转发 artifact + edited 标记 |
| manual response / dropped | 合并视图标记该轮，无 usage |
| 流式无 usage | ctx 显示 `—`，不估算 |
| key 顺序漂移 | 规范化序列化消化 |
| 位置索引 LRU 淘汰 | 被淘汰 session 的后续请求成为新根；接受 |
| 未启用 durable 的重启 | 内存位置索引清空，后续请求成为新根 |
| durable-local 重启 | 从保存的 snapshot/session metadata hydration；artifact body 仍 lazy 读取 |
| 同开头独立会话 | 合入同一棵树为分支/rollout；接受 |
| `previous_response_id` 会话 | 按显式 response id 链归属 |
| gate 挂起中的请求 | 已可归属（capture 即算），session state 派生为 running |

## 8. 当前实现与边界

A/B/C 的 session 投影、位置链、fork/rollout、summary 与合并视图已经落地。session 位置索引仍是进程内结构；仅当启动时 `CONTEXT_LENS_DURABLE=1`，standalone `cmd/context-lens` 才会启用 durable-local，从 SQLite 保存 metadata（包括 session/exchange 关系）并以 data directory 下的文件 blob 保存 artifact，重启后再惰性 hydration。未设置该环境变量时仍是 ephemeral，重启即清空内存索引和元数据。本文不承诺自动 retention：30 分钟 inactivity Session GC 未实现且已撤回，`favorite` retention 也不是当前能力（如未来需要，须另行设计 durable `pinned/keep` 语义）。

当前 workspace 列表是 metadata-only、有界分页（`limit` 与 opaque `cursor`，响应 `X-Next-Cursor`）；长 artifact 通过 range/full/download/search 按需访问。浏览器侧已验证 bounded artifact loader（按 artifact/range 去重、generation 取消过期读取、LRU 字节预算），但这不是完整 session 投影：当前页面只对已加载的 exchange/session lineage 建树，较早或未分页加载的轮次不会自动出现在 projection 中，需显式加载更多页面/上下文。捕获不完整或请求范围不足时，投影必须保持 partial/truncated，不得当作完整 JSON/SSE 解析。

Raw JSON tree 目前只提供有界默认展开、长值摘要和 partial 文本回退；它仍会物化已加载 JSON 的完整节点树，尚未实现真正的节点级 virtualization/windowing。SSE 事件列表和 live stream projection 也仍按已观察/已加载记录渲染，尚未实现完全虚拟化；大 SSE/JSON 的进一步窗口化仍是后续工作。

## 9. 分期与验收

历史分期名称仅用于说明实现边界，不表示待办状态。Phase A（面板）、Phase B（归属）和 Phase C（合并视图）均已实现。当前已验证的有界能力是：workspace metadata 分页（limit/cursor）、artifact range/full/download/search 的边界、浏览器 artifact loader 的去重/取消/LRU 字节预算，以及 incomplete/partial artifact 的安全回退。Raw JSON tree 和 SSE/live projection 仍非完全虚拟化，session projection 也不是跨全部历史的全量投影。后续如扩展全量 session hydration、节点/SSE windowing 或持久化/保留策略，必须先更新本节和 runtime contract。

## 10. 已拍板决策

- 链归属结构永远按原始 inbound 计算（"结构看 harness，内容看 wire"）。
- system/instructions 折叠为虚拟首元素进入链身份，三协议统一：改 system 即新 session。
- tools 与 model 为轮次级软信号：不断链，打 boundary 标记；面板 model 显示最新轮次值。
- 聊天协议请求行移除 URL；非聊天透传保留 method/path。
- 上下文占用只用 upstream usage，不估算、不内置 tokenizer；anthropic 需加和 cache 字段。
- 位置索引为进程内存态；未启用 durable 时重启即清空。启用 durable-local 时，启动从 SQLite 保存的 session/exchange metadata hydration，artifact body 仍按需读取。
