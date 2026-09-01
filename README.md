# Context Lens

> **See what your model sees.** 看见真正进入模型上下文的一切，从系统提示词、可调用的工具，到 skills、memory，以及每一条最终决定模型如何理解和回应的隐藏上下文。
>
> Context Lens 让模型眼中的世界清晰可见。

![Context Lens 首页](https://cdn.jsdelivr.net/gh/fvjowe/imagebed@main/img/20260901194923346.png)

Context Lens 适合调试 Agent、检查长上下文、理解工具调用、排查流式输出，也适合在你想让请求经过人工确认时作为一个轻量的本地控制台使用。它位于你的客户端和模型服务之间，使用原本的协议转发请求，不要求你把应用改造成另一套 API。

## 它能做什么

### 看见模型真正看到的一切

Context Lens 不只展示你主动写下的消息，也帮助你发现那些经常藏在请求深处、却会直接影响模型行为的内容。系统提示词、开发者指令、可调用工具、skills、memory、历史消息、运行时注入的上下文，以及 provider 追加的扩展信息，都可以在同一次请求里被检查。

- system、developer、user、assistant 等消息的实际顺序
- Responses、Chat Completions、Anthropic Messages 中的输入与输出
- tools、tool call、tool result、reasoning 和 thinking
- 多模态内容、provider 扩展以及不认识的字段
- 原始 JSON 结构，以及按 Qwen ChatML 展示的上下文模板

### 实时观察流式输出

模型生成内容时，工作台会实时更新文本、思考过程和工具调用。原始 SSE 事件仍然可以查看，流式输出不会被拼接成另一份“看起来差不多”的结果。

### 在发送前或返回后介入

你可以针对一次 exchange 选择：

- 直接放行
- 暂停请求，确认后再转发
- 暂停响应，审核后再释放
- 在明确的基础上编辑 JSON 或事件内容
- 直接返回一份人工编写的同协议响应
- 丢弃或中止当前请求

所有修改都会生成新的派生内容，原始请求和响应保持不变，因此每一次变化都可以回看和比较。

### 本地优先

Context Lens 默认运行在本机。请求内容只在你配置的本地代理和工作台中流转；凭据不会进入前端展示、工作区事件或日志。你可以从最简单的本地 mock 开始，也可以连接到自己的模型网关或 provider endpoint。

![Context Lens 实际使用效果](https://cdn.jsdelivr.net/gh/fvjowe/imagebed@main/img/20260901194802879.png)

## 快速开始

### 环境要求

- Go 1.26+
- Node.js 与 npm
- 一个可访问的 Responses、Chat Completions 或 Anthropic Messages 上游地址

### 1. 获取项目并安装前端依赖

```bash
git clone https://github.com/spdc-elm/context-len.git
cd context-lens
make bootstrap
```

### 2. 启动 Context Lens

```bash
./scripts/start-local.sh
```

首次启动时，脚本会根据 [`config.example.json`](config.example.json) 创建本地配置文件 `config.local.json`。这个文件只保存在本机，并且仅当前用户可读。

启动后打开：

- 工作台：<http://127.0.0.1:5172/>
- 本地代理：<http://127.0.0.1:3001/>
- 健康检查：<http://127.0.0.1:3001/healthz>

启动脚本还会运行一个本地 synthetic mock upstream，方便你不配置真实 provider 就先浏览工作台。按 `Ctrl-C` 可停止本次启动的服务。

### 3. 配置你的上游服务

编辑本地的 `config.local.json`，填写上游地址和认证信息：

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

常用配置方式：

- `base_url`：你的模型服务或模型网关地址
- `upstream_auth_mode: "passthrough"`：保留客户端发来的标准认证信息，适合只把原来的 API base URL 指向 Context Lens
- `upstream_auth_mode: "configured"`：由 Context Lens 使用本地配置里的 `api_key` 访问上游
- `client_auth`：可选的本地代理访问保护，与上游凭据相互独立

修改配置后重新启动即可。不要把 `config.local.json` 提交到 Git，也不要把 API key 粘贴到 issue、截图或日志中。

### 4. 将客户端指向本地代理

把原来客户端使用的 API base URL 改为：

```text
http://127.0.0.1:3001
```

协议路径保持原样：

```text
/v1/responses
/v1/chat/completions
/v1/messages
/v1/models
```

因此，客户端仍然发送自己原本发送的协议；Context Lens 不会为了方便把一种协议转换成另一种协议。

## 用本地示例体验

如果服务已经启动，可以用仓库内的最小 synthetic fixture 填充工作台：

```bash
CONTEXT_LENS_PROXY_URL=http://127.0.0.1:3001 ./scripts/seed-mock-workspace.sh
```

这些示例只发送到本机，不连接真实第三方服务，也不包含真实凭据。

## 工作方式

Context Lens 把“传输”与“观察”分开：

- 原始 HTTP body bytes 是实际转发的依据
- JSON、Raw Tree、ChatML、摘要和界面状态都是观察视图
- 未修改的请求和响应会按原协议、原始 body 转发
- 原始 SSE 事件、顺序和未知字段会被保留
- 编辑操作会明确产生新的派生内容，并显示校验结果

这意味着你可以放心地展开、搜索、切换视图，而不用担心界面的格式化过程悄悄改变请求。

## 数据与隐私

默认情况下，工作区是临时的，重启后不会恢复内存中的观察数据。如果需要在本地重启后保留工作区，可以开启本地持久化：

```bash
CONTEXT_LENS_DURABLE=1 ./scripts/start-local.sh
```

持久化数据默认位于被 Git 忽略的 `.context-lens-run/`，也可以使用 `CONTEXT_LENS_DATA_DIR` 指定目录。工作台支持清空全部 workspace，也支持删除完整 session。

`/healthz` 用于探活；工作台 API 默认只供本机使用。若你的上游不在 loopback 地址上，请显式确认网络安全策略后再配置。

## 默认地址与端口

| 服务 | 默认地址 |
| --- | --- |
| 工作台 | `127.0.0.1:5172` |
| Go 网关与代理 | `127.0.0.1:3001` |
| 本地 mock upstream | `127.0.0.1:19091` |

如有端口冲突，可以在启动时覆盖：

```bash
CONTEXT_LENS_ADDR=127.0.0.1:3101 \
CONTEXT_LENS_FRONTEND_PORT=5272 \
./scripts/start-local.sh
```

## 项目状态

Context Lens 当前已经包含：

- 三种协议的同协议透明转发
- Raw JSON 与 Qwen ChatML 上下文视图
- Responses、Chat Completions、Anthropic Messages 的 SSE 实时观察
- 请求暂停、响应审核、原样放行、编辑释放和人工回复
- session、exchange、artifact 与工作区管理
- 可选的本地持久化与访问认证

这是一个正在持续打磨中的本地 AI 调试与控制产品。欢迎通过 issue 或 pull request 分享你的使用场景与反馈。

## 开发与验证

如果你想参与开发或在本地运行完整检查：

```bash
make test
```

更详细的协议边界、运行时接口和上下文视图说明见：

- [`docs/protocol-contract.md`](docs/protocol-contract.md)
- [`docs/runtime-contract.md`](docs/runtime-contract.md)
- [`docs/chat-template-spec.md`](docs/chat-template-spec.md)
- [`docs/session-spec.md`](docs/session-spec.md)
