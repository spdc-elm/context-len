# Context Lens — Chat Template MVP Board

状态：**RUN — Phase 1 Raw + Chat Template**

`team/CHARTER.md` 是运行约束；本文件是任务图、接缝、证据和重开条件。manager 单写。leader 已明确授权本次 RUN；Phase 2 SSE 在 P1/P2 联合验收并获 leader 接受前不得派发。

## Phase gates

| Gate | 状态 | 完成条件 | 验收者 |
|---|---|---|---|
| P0 contract / docs freeze | verified | 旧 team 已归档；本 charter、board、Chat Template spec 与 runtime/protocol 接缝一致 | manager |
| P1 Raw Tree | in_progress | 三协议 JSON 结构化展示组件与交互，非 JSON fallback | manager + Raw Tree worker | IR contract |
| P2 Chat Template | in_progress | 三协议 normalizer + Context IR + Qwen ChatML renderer | manager + IR/Qwen worker | IR contract |
| P1/P2 joint acceptance | planned | Raw + Chat Template 的浏览器体验、测试、artifact 不变证据通过 | leader | P1 + P2 |
| P3 SSE IR delta | planned | 三协议 SSE 事件归一为 typed delta；原始事件保留；未知事件 passthrough | manager |
| P4 realtime renderer | planned | assistant/reasoning/tool call/tool result 增量渲染及 completed/failed/cancelled | manager |
| P3/P4 SSE acceptance | planned | 本地 mock stream、浏览器实时交互、原始 SSE bytes 和取消/错误证据通过 | leader |

## Active RUN assignments

| Agent | Scope | Exclusive files | Completion evidence | Status |
|---|---|---|---|---|
| Raw Tree worker | JSON tree component and focused UI tests | `frontend/src/components/RawJsonTree.tsx`, `frontend/test/RawJsonTree.test.tsx` and no other files unless manager reassigns | component tests, typed build compatibility, no wire writes | dispatched |
| IR/Qwen worker | browser Context IR normalizers and Qwen renderer pure modules/tests | `frontend/src/contextIr.ts`, `frontend/src/qwenChatTemplate.ts`, `frontend/test/contextIr.test.ts`, `frontend/test/qwenChatTemplate.test.ts` | three-protocol fixtures, unknown/source pointers, marker stream, pure tests | dispatched |

Manager owns `ExchangeDetail.tsx`, `contracts.ts` integration, shared CSS, phase integration, browser acceptance, and all `team/` writes. Workers must not edit `team/`, backend transport, config, references, or each other’s files.


| Workstream | Owner rule | Exclusive scope | Depends on |
|---|---|---|---|
| IR contract | manager freezes public seam; one worker may implement types/tests | `backend/inspection` shared DTO additions, `frontend/src/contracts.ts` only when assigned | P0 |
| Protocol normalization | one protocol worker at a time | protocol adapters / inspection tests | IR contract |
| Raw Tree | one frontend worker | Raw tree components, renderer tests, scoped styles | IR contract |
| Qwen renderer | one frontend worker | Chat Template components, Qwen preset, scoped tests/styles | IR contract + normalization |
| SSE | one backend/one frontend owner with manager-frozen seam | typed deltas, reducer, stream UI and tests | P1/P2 joint acceptance |
| Review | independent reviewer read-only by default | no writes unless manager assigns exact files | each phase |
| `team/` and docs | manager only | charter, board, specs, README and contract reconciliation | all |

No worker edits another owner’s files, `team/`, or reference directories. Shared DTO changes must be proposed with compatibility and tests before implementation.

## Context IR seam

```text
protocol artifact bytes
  → protocol adapter
  → ContextDocument / ContextBlock[]
  → Qwen ChatML renderer
  → derived UI projection
```

Minimum preserved dimensions: role, block kind, ordered content parts, tool name/call id/arguments, reasoning/thinking, unknown value, provider extension/passthrough, source JSON pointer, original value. IR is never used as transport input.

Streaming seam:

```text
protocol SSE bytes (copy only for inspection)
  → protocol-specific event parser
  → typed IR delta
  → workspace event/reducer
  → live Chat Template block
```

## Feedback loops

| Loop | Status | Command / evidence | Reopen condition |
|---|---|---|---|
| Existing backend suite | verified | `go test -race ./...`, `go vet ./...` | contract or backend changes |
| Existing frontend suite | verified | `cd frontend && npm test -- --run` | frontend changes |
| Frontend build | verified | `cd frontend && npm run build` | renderer/type changes |
| Local launcher | verified | `./scripts/start-local.sh` with mock upstream | runtime seam changes |
| Mock upstream + visual seed | verified | `scripts/start-local.sh` supports local-only fixture upstream; `CONTEXT_LENS_PROXY_URL=http://127.0.0.1:18080 ./scripts/seed-mock-workspace.sh` produced 12 exchanges across all three protocols (six additional seeded runs); mock flushes SSE fixture lines and does not log bodies | mock route/fixture changes |
| Chrome DevTools attachment | verified | Fresh `wsEndpoint` from `127.0.0.1:9222/json/version`; explicit attach succeeded, controlled page `http://127.0.0.1:15173/` listed as selected | Chrome session ends or endpoint changes |
| Baseline browser smoke | verified | Attached screenshot + accessibility snapshot; six seeded exchanges visible in latest-first order; collapse/expand and Raw/Pretty tab clicks work; network requests are local; after favicon/form-field cleanup console has no error/issue entries | P1/P2 implementation changes |
| SSE browser loop | planned | local mock stream: release, live assistant/tool/reasoning, end/error/cancel | event/reducer mismatch |
| Secret scan | planned | repository/config/log scan without printing values | any credential exposure |
| Real upstream | deferred | no probe in this cycle | leader explicitly authorizes safe provision |

## Decisions

- 第一版三种协议都做 Qwen ChatML，不做 Responses-only 分支。
- Raw 是结构化 JSON tree；解析失败回退原始文本。
- Chat Template 是连续上下文流，不是普通聊天气泡；UI 只显示模板名称。
- SSE 是 MVP 内能力，但放在 P1/P2 联合验收之后；不作为常驻主阅读 Tab。
- Qwen ChatML 是第一版内置模板；Llama/Mistral/Gemma 和导入 tokenizer config 后置。
- provider extension / passthrough / unknown / source pointer 从第一版预留。
- request/response artifact 自动按需加载；原始 artifact immutable。

## Honest exits

- 无法从现有 artifact 保留语义时显示 unknown/raw，不猜测、不丢字段。
- 无法安全完成 SSE 事件归一化时保留原始事件 fallback，不伪造 delta。
- 发现 wire、secret、共享 DTO 或写入边界违规时停止新派发，记录证据并升级 manager/leader。
- 测试或浏览器证据不足时保持 phase `review`，不以 executor 自报完成。
