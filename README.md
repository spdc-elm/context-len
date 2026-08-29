# context-lens

`context-lens` 是一个独立的 LLM 请求观察与拦截工作台。它以 HTTP 应用层 wire 数据为权威来源，把 Responses、Chat Completions、Anthropic Messages 请求和响应以原协议转发，同时提供人类可读的旁路解析视图，以及可审计的人工放行、人工回复和响应编辑。

当前状态：透明代理核心 MVP 已完成；Chat Template MVP 的 Raw + Chat Template（含宽屏分栏）与 SSE 实时观察（typed stream_event、live Chat Template、SSE tab 实时事件）均已实现。产品契约与设计规范见 [`docs/chat-template-spec.md`](docs/chat-template-spec.md)、[`docs/protocol-contract.md`](docs/protocol-contract.md) 和 [`docs/runtime-contract.md`](docs/runtime-contract.md)；session 重建与观察面板的设计见 [`docs/session-spec.md`](docs/session-spec.md)（待实现）。

## 必读文档

1. [`docs/protocol-contract.md`](docs/protocol-contract.md)：三协议、wire 保真、拦截状态机和观察边界
2. [`docs/chat-template-spec.md`](docs/chat-template-spec.md)：Raw Tree、Qwen ChatML、Context IR 和 SSE 观察设计
3. [`docs/runtime-contract.md`](docs/runtime-contract.md)：backend 与 workspace UI 的运行时接缝
4. [`docs/session-spec.md`](docs/session-spec.md)：session 重建、fork/rollout 检测、队列面板与合并视图设计（已实现）

## 本地运行

最简单的启动方式是：

```bash
cd /Users/littlefairy/projects/context-lens
./scripts/start-local.sh
```

脚本会自动启动本地 mock upstream、Go gateway、workspace API 和 Vite 工作台（默认不再自动打开浏览器；如需要可设 `CONTEXT_LENS_OPEN_BROWSER=1`）。若端口已被占用，脚本会列出占用进程并询问是否停掉它们；非交互场景可用 `CONTEXT_LENS_KILL_EXISTING=1` 自动停掉旧进程，或回答 `n` 保持原进程继续运行。按 `Ctrl-C` 会停止本轮启动的进程。运行日志只写入被忽略的 `.context-lens-run/` 目录。

首次启动会从 [`config.example.json`](config.example.json) 创建未跟踪的 `config.local.json`，并设置为仅当前用户可读。运行配置至少包含 upstream 的 `base_url` / `api_key`，也可以用可选的 `client_auth` 保护发往 context-lens `/v1/*` 的连接：

```json
{
  "base_url": "http://127.0.0.1:19091",
  "api_key": "",
  "upstream_auth_mode": "passthrough",
  "client_auth": {
    "enabled": false,
    "api_key": ""
  }
}
```

`client_auth.api_key` 是访问 context-lens 的客户端 key，与 upstream 的认证完全分开。启用后，发往 `/v1/*` 的请求需要使用客户端本来就会发送的标准认证方式：`Authorization: Bearer <client key>`、`X-API-Key: <client key>` 或 `API-Key: <client key>`；不需要适配 context-lens 专用 header。错误响应不会泄露 key。`/healthz` 保持公开，便于探活；workspace `/api` 默认仍只通过 loopback 使用。

upstream 认证有两种模式：`upstream_auth_mode: "passthrough"` 时保留 harness 发来的认证 header，适合只改 base URL 的纯透明接入；`upstream_auth_mode: "configured"` 时移除客户端认证并使用配置中的 top-level `api_key` 作为 upstream credential。为兼容旧配置，省略该字段时，非空 `api_key` 自动采用 `configured`，空 `api_key` 自动采用 `passthrough`。upstream 的 credential 只在 Go 进程内处理，不进入 workspace snapshot、SSE、日志或前端 payload。端口不放在这个包含凭据的文件里，使用 `CONTEXT_LENS_ADDR` 和 `CONTEXT_LENS_FRONTEND_PORT` 覆盖。若改成非 loopback upstream，默认会拒绝；只有明确设置 `CONTEXT_LENS_ALLOW_NON_LOOPBACK=1` 才允许启动。

也可以不使用启动脚本，直接运行：

```bash
CONTEXT_LENS_CONFIG=/absolute/path/to/config.local.json \
go run ./cmd/context-lens
```

Go 服务默认监听 `127.0.0.1:3001`；Vite 工作台默认监听 `127.0.0.1:5172`，并把 `/api` 转发到 Go 服务。LLM 协议入口保持原路径：`/v1/responses`、`/v1/chat/completions`、`/v1/messages` 和 `/v1/models`。工作台 API 位于 `/api`。如需覆盖端口，可使用 `CONTEXT_LENS_ADDR=127.0.0.1:<port>` 和 `CONTEXT_LENS_FRONTEND_PORT=<port>`，无需修改包含凭据的 `config.local.json`。

也可以用确定性本地 fixture 填充工作台，供浏览器验收三种协议的 JSON / SSE exchange：

```bash
CONTEXT_LENS_PROXY_URL=http://127.0.0.1:3001 ./scripts/seed-mock-workspace.sh
```

脚本只向本地 context-lens proxy 发送仓库内 synthetic fixture，不连接真实上游，也不打印 body。

完整本地反馈命令：

```bash
make test
```


## 参考资料

- 参考实现（只读）：`/Users/littlefairy/projects/Harness_model_coupling/ChatAPI`
- 本机抓包材料（只读、不得整体复制）：`/Users/littlefairy/projects/new-api/logs/relay-debug`

参考实现和抓包材料只用于理解协议形状与真实上下文，不是 `context-lens` 的运行时依赖，也不是产品行为的权威来源。
