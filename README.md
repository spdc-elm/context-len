# context-lens

`context-lens` 是一个独立的 LLM 请求观察与拦截工作台。它以 HTTP 应用层 wire 数据为权威来源，把 Responses、Chat Completions、Anthropic Messages 请求和响应以原协议转发，同时提供人类可读的旁路解析视图，以及可审计的人工放行、人工回复和响应编辑。

当前状态：**PREPARE**。产品实现尚未开始。

## 必读文档

1. [`team/CHARTER.md`](team/CHARTER.md)：目标、硬约束、团队纪律和验收标准
2. [`team/BOARD.md`](team/BOARD.md)：任务图、接缝、证据和阶段状态
3. [`docs/protocol-contract.md`](docs/protocol-contract.md)：协议、wire 保真、拦截状态机和 UX 契约

## 参考资料

- 参考实现（只读）：`/Users/littlefairy/projects/Harness_model_coupling/ChatAPI`
- 本机抓包材料（只读、不得整体复制）：`/Users/littlefairy/projects/new-api/logs/relay-debug`

参考实现和抓包材料只用于理解协议形状与真实上下文，不是 `context-lens` 的运行时依赖，也不是产品行为的权威来源。
