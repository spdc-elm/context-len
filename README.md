# context-lens

`context-lens` 是一个独立的 LLM 请求观察与拦截工作台。它以 HTTP 应用层 wire 数据为权威来源，把 Responses、Chat Completions、Anthropic Messages 请求和响应以原协议转发，同时提供人类可读的旁路解析视图，以及可审计的人工放行、人工回复和响应编辑。

当前状态：核心 MVP 已进入最终验收。三协议透明转发、独立 request/response gate、artifact/hash、协议观察与显式 mutation、workspace REST/SSE API 以及真实 API 前端已经接通；最新证据和尚未关闭的验收项见 [`team/BOARD.md`](team/BOARD.md)。

## 必读文档

1. [`team/CHARTER.md`](team/CHARTER.md)：目标、硬约束、团队纪律和验收标准
2. [`team/BOARD.md`](team/BOARD.md)：任务图、接缝、证据和阶段状态
3. [`docs/protocol-contract.md`](docs/protocol-contract.md)：协议、wire 保真、拦截状态机和 UX 契约

## 本地运行

要求 Go 1.26+、Node.js 和 npm。只使用本地 mock upstream 时：

```bash
make bootstrap
CONTEXT_LENS_UPSTREAM=http://127.0.0.1:9000 make run
# 另一个终端
make frontend-dev
```

Go 服务默认监听 `127.0.0.1:8080`；Vite 工作台默认监听 `127.0.0.1:5173`，并把 `/api` 转发到 Go 服务。LLM 协议入口保持原路径：`/v1/responses`、`/v1/chat/completions`、`/v1/messages` 和 `/v1/models`。工作台 API 位于 `/api`。

`CONTEXT_LENS_UPSTREAM` 必须显式提供；可选的 `CONTEXT_LENS_UPSTREAM_BEARER` 或 `CONTEXT_LENS_UPSTREAM_API_KEY` 只在服务端进程内注入，二者不能同时设置。项目不拥有默认 model，不会向真实第三方发送探测请求。完整本地反馈命令：

```bash
make test
```

## 参考资料

- 参考实现（只读）：`/Users/littlefairy/projects/Harness_model_coupling/ChatAPI`
- 本机抓包材料（只读、不得整体复制）：`/Users/littlefairy/projects/new-api/logs/relay-debug`

参考实现和抓包材料只用于理解协议形状与真实上下文，不是 `context-lens` 的运行时依赖，也不是产品行为的权威来源。
