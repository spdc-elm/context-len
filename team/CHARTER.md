# Context Lens — Chat Template MVP Charter

状态：**PREPARE**。本周期用于把已完成的透明代理 MVP 推进为三协议的 Raw / Chat Template / SSE 观察体验。manager 是 `team/` canonical state 的唯一写入者；收到 leader 明确 kickoff 前不得运行产品 executor。

## Workspace identity

- 项目：`context-lens`
- 绝对工作区：`/Users/littlefairy/projects/context-lens`
- Git：独立本地仓库，branch `main`
- 本周期基线：`a6e755fbcefaef149af51cb97451162a2c54a8cb`
- 旧周期归档：`team/archive/2026-08-mvp-core/`
- 协议 / wire source of truth：`docs/protocol-contract.md`
- backend/frontend runtime seam：`docs/runtime-contract.md`
- 新设计 source of truth：`docs/chat-template-spec.md`
- 参考实现和抓包目录只读，不复制整体，不访问真实第三方 endpoint。

## Why / job

操作者要从 LLM 的上下文阅读角度观察真实请求和响应：Raw 视图保证结构完整，Chat Template 视图把统一语义渲染成模型可理解的上下文序列，SSE 视图在响应流动时显示正在生成的 assistant、reasoning 和 tool call。观察与渲染永远是 artifact 的派生投影，不得改变透明转发。

## MVP completion

### Phase 1 — Raw + Chat Template，联合验收

- Responses、Chat Completions、Anthropic Messages 三种协议都可投影。
- Raw 是结构化、可收起/展开的 JSON tree；非 JSON 自动回到纯文本。
- Raw 显示数组数量、message/role/type/tool 摘要；支持展开深度、全部收起/展开和搜索定位。
- 三种协议归一到可保真 Context IR，再渲染 Qwen ChatML。
- Chat Template 以连续上下文流展示 `<|im_start|>` / `<|im_end|>` 等 marker，不做普通聊天气泡堆叠。
- 支持 system/developer/user/assistant、text、tool definition、tool call、tool result、reasoning/thinking 和 unknown block。
- tool call 的 JSON 在 Chat Template 中可人类阅读地格式化，同时保留原始字符串和 Raw 定位。
- UI 只显示模板名称，例如 `Chat Template · Qwen ChatML`；不显示 accuracy/approximation 标签。
- `providerExtensions`、provider passthrough、原始值和 source JSON pointer 从第一版保留。
- request 和 response 均支持；artifact 自动按需读取，原始 bytes immutable。

### Phase 2 — SSE，MVP 后段验收

- Responses、Chat Completions、Anthropic Messages 的 SSE 各自按原协议观察，不共享错误的终止规则。
- SSE 原始 bytes 仍然保留；不要求把 SSE 作为常驻主 Tab。
- response stream 时，Chat Template 中实时更新 assistant text、reasoning/thinking、tool call arguments 和 tool result。
- 通过统一 IR delta/reducer 追加 block；stream completed/failed/cancelled 可观察。
- 无法归一化的事件保留为 provider passthrough / raw event fallback。

## Non-goals for this cycle

- 不做协议之间的透明转换；仍然同协议转发。
- 不改变 wire/artifact authority；不让 projection 反向成为转发输入。
- 不在第一版实现 Llama 3、Mistral、Gemma 或自定义模板导入；模板导入作为后续扩展，但 IR 与 renderer 接缝必须预留。
- 不承诺仅凭 API body 复原上游隐藏 tokenizer；第一版使用内置 Qwen ChatML renderer。
- 不连接真实上游、不把 secret 写入 prompt、日志、fixture、snapshot 或前端 payload。
- 不为测试而测试；每项测试应对应协议保真、投影保真、状态转换或关键 UX 验收。

## Hard boundaries

1. 原始 request/response body bytes/blob 是唯一 wire authority；projection、IR、Chat Template text 均是派生数据。
2. Responses、Chat Completions、Anthropic Messages 必须同协议透明转发；bypass 不 decode/re-encode、不聚合/重建 SSE。
3. 所有未知字段、未知 item、provider extension 和原始 source pointer 必须可追溯，不能静默丢弃。
4. Qwen renderer 不得把 provider-specific 逻辑散落到 UI；协议 adapter → Context IR → renderer。
5. SSE 只能在已有真实事件 / artifact 基础上增量更新，不伪造模型事件。
6. `team/` 仅 manager 写入；executor 不改 charter/board；共享 DTO 先冻结再并行。
7. 参考路径 `/Users/littlefairy/projects/Harness_model_coupling/ChatAPI` 和 `/Users/littlefairy/projects/new-api/logs/relay-debug` 只读。

## Team / budget

沿用旧团队合同：

- executor：`agent_type=worker`
- model：`gpt-5.6-luna`
- reasoning effort：`max`
- manager persistent goal：仅 kickoff 后创建，`token_budget=10000000000`
- executor 默认递归深度：`0`
- active child 上限：`16`，按边际价值使用，不为填满并发而派发
- manager 保留任务图、接口冻结、单写者分配、集成、亲自验收和最终状态 authority。

## PREPARE / RUN gate

PREPARE 可做：归档旧 team、更新设计/运行契约、读取代码、运行现有本地测试、冻结接口和写 kickoff。PREPARE 不实现 Raw Tree、Chat Template 或 SSE 产品行为。

只有 leader 明确发送 kickoff 后进入 RUN。RUN 必须按 Phase 1 → leader 联合验收 → Phase 2 → leader SSE 验收推进。任何实质改变完成态、预算、写入边界或安全红线的变化须升级给 leader。

## Evidence required

Phase 1：三协议代表性 JSON fixture 均能生成可读 Raw Tree 和 Qwen Chat Template；工具、reasoning、unknown/source pointer 有证据；解析失败仍可回 Raw；frontend tests/build、Go tests 和本地浏览器交互通过；artifact hash 不变。

Phase 2：三协议 SSE fixture 和本地 mock stream 可驱动 IR delta；assistant/tool/reasoning 增量显示正确，终止/错误/取消状态正确；原始 SSE bytes 保真；浏览器实时交互、race tests、build 和 `git diff --check` 通过。

manager 必须亲读关键 diff、亲跑关键命令和本地交互；executor 自报不算完成证据。
