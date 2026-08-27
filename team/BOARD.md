# Team Board

状态：**RUN — T0–T7 verified；T8 final review/browser/clean-state 验收中**

manager 是本文件唯一编辑者。RUN executor 不编辑 `team/`。

## Workspace

- `/Users/littlefairy/projects/context-lens`
- branch：`main`
- canonical baseline revision：`f36770a837fc978a93015f56beaeb8cf996318bd` (`docs: establish context-lens team charter and protocol contract`)
- kickoff 时必须重新检查 live `HEAD` 和 dirty state
- PREPARE 前 dirty state：空目录
- 当前 PREPARE 写入：`README.md`、`team/CHARTER.md`、`team/BOARD.md`、`docs/protocol-contract.md`

- kickoff 时 manager 已核对 HEAD `0eb805512bb4938bf750ad1ae65873d3e8f505fb`、branch `main`；发现并保留 `team/CHARTER.md` 的已有 dirty 修改（测试遵循奥卡姆剃刀原则）。
- `docs/runtime-contract.md` 已由 manager 冻结 wire/artifact/exchange/event DTO，作为 T1/T2/T4/T6 接缝。


| Feedback loop | 状态 | 证据 / 说明 | Reopen condition |
|---|---|---|---|
| Go 工具链 | verified | Go 1.26.1 可用 | 项目选择的 Go 版本不兼容 |
| Node 工具链 | verified | Node 25.9.0、npm 11.12.1 可用 | 前端脚手架要求不兼容版本 |
| Docker CLI | verified | Docker 29.2.1 可用；MVP 不依赖远端服务 | 本地集成测试需要容器而 daemon 不可用 |
| 参考前端测试 | verified | 56 tests pass | 只在参考实现行为需要复核时重开 |
| 参考后端核心测试 | verified | benchmark runner 之外的 package 通过 | 只在复用实现片段时重开 |
| 参考 benchmark runner | expected-red | 两个 Harbor 安装模拟测试在当前 macOS 环境失败；与本项目 proxy 核心无关 | 团队决定复用该模块时重开并修复 |
| 官方协议资料 | verified | OpenAI OpenAPI、Anthropic API / SDK 已定位 | 官方结构与本机样本冲突时重开 |
| 三协议真实样本 | verified | 本机只读目录包含三种 request / SSE 形态 | 选择脱敏 fixture 时复核具体样本 |
| Canonical docs audit | verified | 独立 worker 只读检查四份文档，无 Critical；范围、intent、secret 边界和 kickoff 可恢复性通过 | leader 修改 intent 或资源约束 |
| Fixture provenance manifest | ready | `docs/protocol-contract.md` 已定义 `tests/fixtures/manifest.json` 的最小字段；具体样本留给 RUN | 选择首批 fixture 时复核脱敏 |
| Mock upstream | expected-red | 语义已由 charter 确认，留给 RUN 实现 | handler 和 transport 能跑通后转 verified |
| Browser UX loop | expected-red | 前端尚未初始化，留给 RUN 实现 | 工作台可启动后转 verified |
| 真实第三方 endpoint | leader-accepted-deferred | MVP 使用本地 mock；任何真实调用需 leader 授权 | leader 提供安全 provision 和允许的 probe |

## Global task graph

状态值：`planned`、`ready`、`in_progress`、`review`、`verified`、`blocked`。

### T0 — Repository skeleton and feedback loop

- 状态：`verified`
- 依赖：kickoff
- 写入边界：根目录、构建配置、最小 backend/frontend/test 目录；不得触碰 `team/`
- 结果：一条命令可运行 backend tests、frontend tests 和本地 e2e smoke
- 证据：clean install、unit command、dev start、health probe

### T1 — Wire envelope and artifact contract

- 状态：`verified`
- 依赖：T0；manager 先冻结 DTO
- 写入边界：backend wire/artifact packages 和对应 tests
- 结果：immutable request/response envelope、body bytes/blob、hash、complete 状态
- 接缝：T2 transport、T4 exchange、T5 inspector、前端 artifact DTO
- 证据：round-trip bytes、large body、capture incomplete、header redaction tests

### T2 — Transparent transport and local mock upstream

- 状态：`verified`
- Owner：backend worker
- 依赖：T1
- 写入边界：transport、endpoint、header policy、mock upstream、tests
- 结果：三协议 JSON/SSE 在 pass/pass 下同协议转发；`GET /v1/models` 可透明转发；body 不 decode/encode
- 证据：六类 fixture 的 inbound/upstream 和 upstream/downstream hash equality；原始 path/escaped path/raw query 比较；header diff 可解释；status/error passthrough；cancellation

### T3 — Upstream profile and safe credential injection

- 状态：`verified`
- Owner：backend worker；与 T2 的共享 transport 接口由 manager 冻结
- 依赖：T0、T1
- 写入边界：profile、config、credential storage、network safety、API handler、tests
- 结果：单一通用 profile，URL/path/auth 可配置，model 不进入 profile
- 证据：save/load without secret echo、origin safety、header injection、path preview、model untouched

### T4 — Exchange state machine and gates

- 状态：`verified`
- Owner：backend worker
- 依赖：T1、T2、T3
- 写入边界：exchange registry、policy、request/response gate、commands、events、tests
- 结果：pass/pass、hold/pass、pass/hold、hold/hold；forward/unchanged、edit-forward、manual-reply、release/unchanged、edit-release、replace、drop、abort
- 接缝：T6 realtime/UI command schema
- 证据：state transition table；request hold 期间 upstream 未收到请求且 downstream 未提交 headers；revision conflict；client disconnect；upstream error；每类操作至少一条代表性测试

### T5 — Protocol inspectors and mutation

- 状态：`verified`
- 依赖：T1；可与 T3 部分并行
- 写入边界：inspection、protocol fixture、mutation、validation、tests
- 结果：Responses、Chat Completions、Anthropic Messages、generic JSON、SSE projection；原始 artifact 不变
- 证据：unknown nodes retained、tool/reasoning/content block/event coverage、parser failure pass-through、diff tests
- Fixture policy：只从抓包目录提取脱敏的最小形状；原始日志不进入仓库

### T6 — Workbench UI

- 状态：`verified`
- 依赖：manager 冻结 exchange/artifact/event DTO；可以先用 typed mock 开发
- 写入边界：frontend app、types、API client、components、tests
- 结果：traffic queue、intercept toggles、exchange detail、Raw / Pretty / Diff / SSE views、request/response actions；大 body 通过 artifact 按需读取，支持懒加载/虚拟化、搜索、JSON path 定位和完整下载
- 证据：component tests、state reducer tests、mock realtime interaction、浏览器 smoke

### T7 — Manual response and edited response integration

- 状态：`verified`
- Owner：backend + frontend 各一位 worker，manager 协调接口
- 依赖：T4、T5、T6
- 写入边界：各自模块，不交叉抢写共享 DTO
- 结果：三协议人工响应；JSON edit、event-level edit、replacement 和 drop 路径；原始 artifact 保留
- 证据：protocol validation、hash/diff、browser interaction、request hold 期间 upstream 未调用的 assertion

### T8 — Integration, adversarial review, and final acceptance

- 状态：`in_progress`
- Owner：manager + independent reviewer
- 依赖：T0–T7
- 写入边界：reviewer 默认只读；修正必须由 manager 分配独占范围
- 结果：完成 charter 的十项完成证据，包括 `/v1/models` 和 raw query/header diff
- 证据：manager 亲跑 full tests、本地 e2e、三协议代表性长轨迹、secret scan、关键代码审计

## Parallelization plan

- executor contract：`agent_type=worker`、`model=gpt-5.6-luna`、`reasoning_effort=max`
- active child 硬上限：16；按需使用，不为填满并发而派发
- 默认 depth：0；executor 不自行派生 agent
- manager 使用与 PREPARE 会话同等能力等级的强模型，负责全局任务图、接口冻结、单写者分配、阶段 gate、风险判断、集成和最终验收
- 如果 RUN manager 创建任何 goal，使用 `token_budget=10000000000`；kickoff 之前不得创建 goal
- 初始并发只在接口冻结后开启：
  - transport/wire
  - protocol inspection
  - frontend typed-mock shell
- profile、exchange 和 UI integration 在共享 DTO 稳定后进入
- reviewer 在 contract freeze、transparent transport gate、mutation gate、final gate 介入

## Single-writer map

| Area | Owner rule |
|---|---|
| `team/` | manager only |
| public exchange/artifact/event DTO | manager freezes; one assigned worker edits |
| backend wire/transport | one backend owner at a time |
| backend exchange/policy | one backend owner at a time |
| protocol inspection/mutation | one protocol owner at a time |
| frontend types/reducer | one frontend owner at a time |
| frontend views | may split by component only after shared types freeze |
| fixtures | one curator; manager verifies redaction and provenance |

## Decisions already made

- New repository from zero; no fork.
- Reference repository and relay-debug logs are local read-only sources.
- First release is the core MVP described in charter.
- Default is bypass with independent request and response intercept controls.
- Wire artifact is authority; projection is read-only.
- Same protocol in and out; no silent bridge.
- `model` belongs to the request and upstream; context-lens does not choose it.
- One generic upstream profile is sufficient for this delivery.
- Real external API probes are not required for readiness.
- 如果 RUN manager 创建任何 goal，使用 `token_budget=10000000000`；任何 goal 都遵守该预算


## RUN evidence notes

- Leader 已明确说明 executor user quota 问题解决；manager 于本轮重新派发 exchange/gates、workspace API、protocol mutation、proxy e2e、frontend integration 和只读 adversarial review。

- T0 manager verified `go test -race ./...`, `go vet ./...`, live health probe, app proxy-route unit test, frontend install/test/build, and `scripts/start-local.sh` one-click smoke (backend health, Vite index, fixture response identity, workspace four refs).
- T0 now includes `config.example.json` + ignored `config.local.json`: strict two-field (`base_url`, `api_key`) JSON loading, file permission check, server-only Bearer injection, and no-secret summary tests.
- T1 manager verified wire tests including opaque bytes, 8 MiB body, incomplete capture, hashes, escaped path/query, redaction.
- T2 generic transport/proxy and `tests/e2e` cover all six JSON/SSE fixtures, `/v1/models`, exact request/response hashes, escaped path/raw query, header policy, upstream HTTP/transport errors and client cancellation under local httptest.
- T3 profile/config tests cover loopback SSRF, CRLF, credential reference and header policy; runtime config now exposes only local `base_url`/`api_key`, injects API key server-side as Bearer, and requires explicit `CONTEXT_LENS_ALLOW_NON_LOOPBACK=1` for external origins. Manager reran full race suite.
- T4 race-tested exchange tests cover pass/pass, request hold, response hold, unchanged/edit/manual/release/replace/drop/abort, revision conflict, upstream/downstream error and cancellation. Real HTTP gateway integration remains under T7/T8.
- T5 protocol-aware inspector/mutation tests cover all checked-in fixtures, unknown nodes, Responses termination, Chat choices/usage, Anthropic block grammar, derived artifacts, protocol validation and immutable originals.
- T6 manager reran 13 frontend tests and production build successfully. Production default uses real local workspace REST/SSE, lazy artifact reads/downloads, policy changes and revisioned commands; mocks remain injectable for tests.
- Workspace API race tests cover list/get with redaction, artifact range/search/download, revisioned commands and SSE events, future-only policy changes and slow subscribers.
- T7/T8 gateway integration now has local HTTP tests for pass/pass, request hold unchanged/manual, response hold unchanged/edited, protocol-invalid edit rejection, cancellation, and a real workspace REST `hold/hold` command flow. Standalone process smoke verified `/v1/responses` plus `/api/exchanges` and four visible wire-stage artifact refs.
- Gateway pass/pass capture limits now preserve downstream traffic and mark observation artifacts incomplete instead of truncating. Standalone defaults restrict listen/upstream to loopback, cap bodies/artifact storage, and enable TTL cleanup.
- T8 remains `in_progress` pending independent review reconciliation, an attached real-browser interaction pass, final secret scan, commit and clean Git acceptance.
