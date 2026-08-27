# Team Charter

状态：**PREPARE**。本文件是团队运行时的 canonical state；manager 是唯一维护者。产品实现只有在 leader 明确发送 kickoff 后才开始。

## Workspace identity

- 项目：`context-lens`
- 绝对工作区：`/Users/littlefairy/projects/context-lens`
- Git：独立本地仓库，branch `main`
- PREPARE 基线：`6c4c4c614a7880246f33659f6158d2e5733591e2`；空仓库初始化后由 manager 写入本 charter、board 和协议契约文档
- 参考实现（只读）：`/Users/littlefairy/projects/Harness_model_coupling/ChatAPI`
- 参考实现基线：`f65a7afeedf6984f26cff637c17b8affed17d57a`
- 真实抓包材料（只读）：`/Users/littlefairy/projects/new-api/logs/relay-debug`
- PREPARE 记录时间：`2026-08-27T10:11:50Z`

## Why / job to be done

当一个 Harness 调用 LLM 时，操作者需要在不被中间层悄悄改变行为的前提下，看清它实际发出的完整上下文、上游实际收到的请求、上游返回的完整响应，以及最终交回 Harness 的内容；需要时可以在明确、可逆、可比较的节点暂停、人工回答或修改响应，用于调试协议、上下文和模型行为。

`context-lens` 的核心是 **观察和可控实验**，不是替上游决定模型、替协议做隐式翻译，或把不同协议强行压成一个共同格式。

## 首个完成态

交付一个本地可运行的核心垂直切片，包含：

- 独立的 HTTP LLM 代理入口
- OpenAI Responses、OpenAI Chat Completions、Anthropic Messages 三种协议的同协议转发；`GET /v1/models` 作为上游能力探测和透明转发入口
- 默认 bypass：请求和响应不暂停、不改写；完整成功交换中的应用层 body 与 SSE bytes 必须 byte-identical
- 独立的请求拦截和响应拦截开关
- 请求拦截后的原样放行、编辑后放行、人工构造原协议响应、丢弃
- 响应拦截后的原样释放、显式编辑后释放、替换和丢弃
- JSON body、SSE body、headers 和 HTTP 元数据的观察
- 面向人的消息、内容块、工具、reasoning、响应 item 和 SSE event 视图
- 一个通用上游 profile；profile 选择端点和认证，不拥有或改写请求中的 model
- 本地 mock upstream、协议 fixture、wire hash 和关键交互测试

没有进入以上清单的能力不主动加入首个交付。不要为了“完整”添加未经需求驱动的产品面或抽象。

## 硬约束

### 1. Wire authority

原始请求和响应 body 以 bytes / blob 保存，是唯一转发权威。解析出的 map、AST、摘要、消息列表和 UI projection 都是派生数据，不能回流成为默认转发输入。

### 2. 同协议、同语义

默认路径必须遵守：

- Responses → Responses
- Chat Completions → Chat Completions
- Anthropic Messages → Anthropic Messages

不得把一种协议静默转换成另一种协议。兼容转换若未来需要，必须是显式选择、带差异说明的独立模式。

### 3. Model transparency

请求中的 `model` 原样传给上游。上游 profile 不保存默认 model，不提供隐式模型目录，不按模型前缀猜测 provider。任何 model rewrite 都必须是用户明确创建的实验操作，并产生可见 diff。

### 4. Projection cannot affect traffic

协议 inspector 在 raw bytes 的副本上运行。解析失败、遇到未知字段或未知 event 时，观察模式仍应继续转发，并在 projection 中显示 warning 和原始节点。

### 5. Explicit mutation only

没有明确的 forward/edit/reply/release 操作，就不改变 body。所有编辑产生新的 derived artifact，原始 artifact 永不覆盖；释放前显示 base hash、result hash 和结构化 diff。

### 6. Streaming fidelity

bypass 路径不聚合、不重新编码 SSE；按收到的 bytes 转发。Responses 的命名 event / sequence number、Chat 的 chunk / `[DONE]`、Anthropic 的 content block event grammar 各自独立处理，不能共用一个终止规则。完整成功交换必须通过 upstream/downstream body hash 相等验证。

### 7. Necessary proxy changes are visible

上游认证替换、Host、Content-Length、HTTP hop-by-hop headers 等代理必要变化可以存在，但必须单独记录和展示。目标是应用层 body、协议语义和可转发 headers 的透明，不声称保留 TCP、TLS 或 HTTP frame 的字节划分。

### 8. Secrets and source boundaries

- Harness 入站凭据与 upstream 凭据分离
- upstream secret 只在服务端安全存储和 transport 内使用
- secret 不进入 prompt、team 文档、日志、WS snapshot 或 fixture
- 参考实现和抓包目录只读
- 原始抓包不整体复制；需要的样本必须脱敏、缩小并标记来源和完整性状态

### 9. Scope discipline

所有代码、测试、fixture、文档和临时产品工件只写入 `/Users/littlefairy/projects/context-lens`。不得修改参考仓库。外部 endpoint、真实账号、真实 API key 和付费请求需要 leader 明确授权；默认使用本地 mock。

## 推荐架构边界

建议新建独立的 proxy domain，而不是继续扩展参考实现中的 `chat/relay` 或 `TurnRequest`：

- `wire`：不可变 HTTP envelope、bytes/blob、hash、capture 状态
- `transport`：通用 HTTP upstream round trip、header policy、stream copy、取消
- `policy`：request/response gate 和 timeout 行为
- `exchange`：以 exchange ID 管理一次 HTTP 交换，不以 conversation 充当生命周期主键
- `inspection`：Responses / Chat Completions / Anthropic / generic JSON / SSE projection
- `mutation`：JSON pointer、event-level 和 raw replacement 的派生 artifact
- `profile`：上游 URL、路径映射、认证方式和安全校验
- `persistence`：exchange 元数据和外置 wire artifact
- `workspace`：队列、详情、命令、diff 和实时事件

参考实现中的 `turn`、`pending`、`protocolruntime`、`egress` 可以作为人工响应思路的参考，但不能成为透明 bypass 的必经链路。

## 产品交互契约

每个 exchange 有两个独立 gate：

| request gate | response gate | 行为 |
|---|---|---|
| pass | pass | 上游请求和响应直接流过，同时旁路捕获 |
| hold | pass | 请求等待操作；放行后响应直接流过 |
| pass | hold | 请求直接流过；响应完整捕获后等待操作 |
| hold | hold | 两端都由操作员决定 |

请求 hold 的操作：原样放行、编辑后放行、人工原协议响应、丢弃。

响应 hold 的操作：原样释放、编辑后释放、人工响应替换、丢弃。

策略在 exchange 创建时快照；切换全局开关只影响后续 exchange，不回溯改变在途交换。

## 完成证据

manager 只有在以下证据齐全后才可宣布完成：

1. 三种协议的 JSON 和 SSE fixture，以及 `/v1/models` 的透明响应，均通过真实 proxy handler。
2. bypass 下 request body 的入站 hash 与 upstream request hash 相同；原始 path、escaped path、raw query 的顺序与编码按 fixture 比较，header policy 的每一项差异均可解释。
3. bypass 下 upstream response body 与 downstream body 逐字节相同；状态码、Content-Type、Content-Encoding、可转发响应 headers 和 SSE event bytes 均有断言。
4. 未知字段、并行工具调用、reasoning / thinking、Responses output items 和 SSE event 均能在 projection 中观察，且 projection 失败不阻断 bypass。
5. request gate 的原样放行、编辑后放行、人工原协议回复和丢弃均可用；request hold 期间 upstream 没有收到请求，downstream 没有提前提交 status/header。
6. response gate 的原样释放保持 hash；编辑、替换和丢弃均有代表性测试，至少一种显式编辑路径产生可审计 diff 并通过协议校验。
7. 客户端取消能够取消上游；上游错误不会无声地让客户端永久等待。
8. credentials、访问隔离、SSRF 防护、body size 和 artifact 清理有测试证据。
9. 前端能从实时事件定位 exchange，并在 Raw / Pretty / Diff / SSE 视图之间切换；大 body 通过 artifact 按需读取，支持懒加载/虚拟化、搜索、JSON path 定位和完整下载，展示截断不改变 artifact。
10. manager 亲自检查关键 diff、运行测试和一条端到端本地交互；executor 自报不算完成证据。

## 团队运行契约

### Manager

当前 manager 负责全局任务图、接口冻结、单写者分配、阶段 gate、风险判断、集成、最终验证和本文件维护。manager 使用与本次 PREPARE 会话同等能力等级的强模型，不把 executor 的“完成”当作证据。

如果 RUN manager 创建任何 goal，使用 `token_budget=10000000000`；kickoff 之前不得创建 goal。

### Executor / reviewer

- executor 使用 `agent_type=worker`、`model=gpt-5.6-luna`、`reasoning_effort=max`
- active child 硬上限为 16，按边际价值使用，不为填满并发而派发
- 默认递归深度为 0，executor 不自行派生 agent
- reviewer 也遵守同一写入边界；review 可以只读或在明确分配的文件范围内修正
- 每次 dispatch 必须写明绝对工作区、独占文件范围、已知事实、完成证据、禁止事项和诚实失败出口

### 写入和接缝

- `team/` 是 manager 单写区
- 同一文件同一时间只有一个 owner
- wire contract、公共 DTO、WS event schema 先由 manager 冻结，再允许并行实现
- 跨层修改必须附接口变更说明和测试
- 不通过删除测试、放宽断言或隐藏 warning 来制造绿色

### PREPARE / RUN 门

PREPARE 允许调查、文档、只读测试和本地 mock 设计，不实现产品行为。收到 leader 的明确 kickoff 后才进入 RUN。RUN 中持续到 `complete`、`blocked` 或 `budget-exhausted`，保留证据和精确缺口。

## 已验证的准备事实

- 目标目录在 PREPARE 开始时为空，现已初始化本地 Git。
- 参考仓库前端测试：56 项通过。
- 参考仓库后端除 benchmark runner 外的测试：通过。
- 参考仓库两个 benchmark runner 安装模拟测试在当前 macOS 环境失败，属于参考实现既有环境相关 RED，不阻塞 context-lens 的本地 proxy 开工。
- 官方协议资料和本机抓包材料清单见 `docs/protocol-contract.md`。
