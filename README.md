# context-lens

`context-lens` 是一个独立的 LLM 请求观察与拦截工作台。它以 HTTP 应用层 wire 数据为权威来源，把 Responses、Chat Completions、Anthropic Messages 请求和响应以原协议转发，同时提供人类可读的旁路解析视图，以及可审计的人工放行、人工回复和响应编辑。

当前状态：透明代理核心 MVP 已完成并归档验收；Chat Template MVP 的 Raw + Chat Template（含宽屏分栏）与 SSE 实时观察（typed stream_event、live Chat Template、SSE tab 实时事件）均已实现，等待 leader 分阶段验收。最新任务图和设计规范见 [`team/BOARD.md`](team/BOARD.md) 与 [`docs/chat-template-spec.md`](docs/chat-template-spec.md)。

## 必读文档

1. [`team/CHARTER.md`](team/CHARTER.md)：本轮 Chat Template MVP 目标、阶段验收、硬约束和团队纪律
2. [`team/BOARD.md`](team/BOARD.md)：本轮任务图、接缝、证据和阶段状态
3. [`docs/protocol-contract.md`](docs/protocol-contract.md)：三协议、wire 保真、拦截状态机和观察边界
4. [`docs/chat-template-spec.md`](docs/chat-template-spec.md)：Raw Tree、Qwen ChatML、Context IR 和 SSE 观察设计
5. [`docs/runtime-contract.md`](docs/runtime-contract.md)：backend 与 workspace UI 的运行时接缝

## 本地运行

最简单的启动方式是：

```bash
cd /Users/littlefairy/projects/context-lens
./scripts/start-local.sh
```

脚本会自动启动本地 mock upstream、Go gateway、workspace API 和 Vite 工作台，并在 macOS 上打开浏览器。按 `Ctrl-C` 会停止本轮启动的进程。运行日志只写入被忽略的 `.context-lens-run/` 目录。

首次启动会从 [`config.example.json`](config.example.json) 创建未跟踪的 `config.local.json`，并设置为仅当前用户可读。实际运行配置只有两个字段：

```json
{
  "base_url": "http://127.0.0.1:19091",
  "api_key": ""
}
```

`api_key` 只在 Go 进程内读取，并按 `Authorization: Bearer <api_key>` 注入上游，不会进入 workspace snapshot、SSE、日志或前端。默认 `base_url` 指向仓库内的 mock upstream。若改成非 loopback 地址，默认会拒绝；只有明确设置 `CONTEXT_LENS_ALLOW_NON_LOOPBACK=1` 才允许启动外部 upstream。

也可以不使用启动脚本，直接运行：

```bash
CONTEXT_LENS_CONFIG=/absolute/path/to/config.local.json \
go run ./cmd/context-lens
```

Go 服务默认监听 `127.0.0.1:8080`；Vite 工作台默认监听 `127.0.0.1:5173`，并把 `/api` 转发到 Go 服务。LLM 协议入口保持原路径：`/v1/responses`、`/v1/chat/completions`、`/v1/messages` 和 `/v1/models`。工作台 API 位于 `/api`。

也可以用确定性本地 fixture 填充工作台，供浏览器验收三种协议的 JSON / SSE exchange：

```bash
CONTEXT_LENS_PROXY_URL=http://127.0.0.1:8080 ./scripts/seed-mock-workspace.sh
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
