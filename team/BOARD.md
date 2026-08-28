# Context Lens — Chat Template MVP Board

状态：**RUN — Phase 2 SSE (implementation complete, awaiting leader SSE acceptance)**

`team/CHARTER.md` 是运行约束；本文件是任务图、接缝、证据和重开条件。manager 单写。leader 已明确授权本次 RUN；Phase 2 SSE 在 P1/P2 联合验收并获 leader 接受前不得派发。

## Phase gates

| Gate | 状态 | 完成条件 | 验收者 |
|---|---|---|---|
| P0 contract / docs freeze | verified | 旧 team 已归档；本 charter、board、Chat Template spec 与 runtime/protocol 接缝一致 | manager |
| P1 Raw Tree | in_review | Three-protocol JSON/raw fixtures render as structured tree with semantic counts, message/tool summaries, search auto-expand/highlight, controls, source pointers, and plain-text fallback; `frontend/test/RawJsonTree.test.tsx` 7 passing | manager + Raw Tree worker | IR contract |
| P2 Chat Template | in_review | Three-protocol checked-in JSON fixtures normalize to loss-aware Context IR and render nested collapsible Qwen marker blocks (inner tool tags, collapsible JSON, thinking collapsed by default) with tools/reasoning/unknown/source pointers; `frontend/test/contextIr.test.ts`, `frontend/test/qwenChatTemplate.test.ts`, `frontend/test/ChatTemplateView.test.ts` 13 passing | manager + IR/Qwen worker | IR contract |
| P1/P2 joint acceptance | awaiting_leader | Browser screenshots `/tmp/context-lens-phase1-final.png`, `/tmp/context-lens-phase1-chat.png`, `/tmp/context-lens-responses-json-response-chat.png`, `/tmp/context-lens-anthropic-json-response-chat.png`, `/tmp/context-lens-phase1-sse-guard-chat.png`; controlled Chrome shows Raw Tree and Qwen view for all three protocols plus JSON response reasoning/tool/unknown blocks; exact artifact hashes remain visible. `evaluate_script` reports `bodyOverflow=hidden`, `shellOverflow=hidden`, one `.viewer-body` scroller (`overflow=auto`); local network and console are clean | leader | P1 + P2 |
| P3 SSE IR delta | implemented | 三协议 SSE 事件归一为 typed delta；原始事件保留；未知事件 passthrough。`backend/inspection/sse_stream.go`（增量扫描器，逐字节等价于整包解析）+ gateway stream tap → additive `stream_event` workspace 事件；前端 `streamIr.ts` 三协议归一 reducer（21 tests）。 | manager |
| P4 realtime renderer | implemented | assistant/reasoning/tool call/tool result 增量渲染及 completed/failed/cancelled：live Chat Template 追加 block + 状态 chip、SSE tab 实时事件列表、流结束自动切 response artifact（74 frontend tests / backend race 全过）。 | manager |
| P3/P4 SSE acceptance | awaiting_leader | 本地 mock stream（MOCK_SSE_DELAY_MS=120）、浏览器实时交互（streaming · N events 递增）、原始 SSE bytes 不变（curl 比对 + Go 测试断言 bypass 字节不变）、截断流 incomplete 标记证据通过；截图 `/tmp/context-lens-sse-live-mid.png`、`/tmp/context-lens-sse-final-chat.png`、`/tmp/context-lens-sse-anthropic-chat.png`、`/tmp/context-lens-sse-chat-toolcall.png` | leader |

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
| Existing frontend suite | verified | `cd frontend && npm test -- --run` (32 tests, now including Raw/IR/Qwen) | frontend changes |
| Frontend build | verified | `cd frontend && npm run build` | renderer/type changes |
| Local launcher | verified | `./scripts/start-local.sh` with mock upstream | runtime seam changes |
| Mock upstream + visual seed | verified | `scripts/start-local.sh` supports local-only fixture upstream; `CONTEXT_LENS_PROXY_URL=http://127.0.0.1:18080 ./scripts/seed-mock-workspace.sh` produced 12 exchanges across all three protocols (six additional seeded runs); mock flushes SSE fixture lines and does not log bodies | mock route/fixture changes |
| Chrome DevTools attachment | verified | Fresh `wsEndpoint` from `127.0.0.1:9222/json/version`; explicit attach succeeded, controlled page `http://127.0.0.1:15173/` listed as selected | Chrome session ends or endpoint changes |
| Baseline browser smoke | verified | Attached screenshot + accessibility snapshot; six seeded exchanges visible in latest-first order; collapse/expand and Raw/Pretty tab clicks work; network requests are local; after favicon/form-field cleanup console has no error/issue entries | P1/P2 implementation changes |
| SSE browser loop | verified | Three protocol streams seeded through the local mock (120ms/line): Chat Template showed `response · streaming · N events` growing live, completed via `response.completed` / `[DONE]` / `message_stop`, then auto-selected the response artifact; SSE tab showed the live event list during flow and the full parse after; unknown `response.custom_fixture` stayed visible as a passthrough block; console clean; all requests stayed on 127.0.0.1 | event/reducer mismatch or terminal rule drift |
| Secret scan | verified | `config.local.json` remains ignored and untracked; seeded synthetic fixtures contain no credentials; no key values printed; browser requests stayed on `127.0.0.1` | any fixture/config change |
| Real upstream | deferred | no probe in this cycle | leader explicitly authorizes safe provision |

## Decisions

- 第一版三种协议都做 Qwen ChatML，不做 Responses-only 分支。
- Raw 是结构化 JSON tree；解析失败回退原始文本。
- Chat Template 是连续上下文流，不是普通聊天气泡；UI 只显示模板名称。
- SSE 是 MVP 内能力，但放在 P1/P2 联合验收之后；不作为常驻主阅读 Tab。
- Qwen tool ordering was verified against official Qwen2.5/Qwen3 templates: tool definitions are injected into the initial system segment before messages; tool results use a user segment. The renderer was corrected accordingly. Human-readable JSON whitespace remains a display choice, not a token-exact claim.
- Chat Template renders as a nested collapsible structure: ChatML segments contain inner tool tags and collapsible JSON; thinking blocks default collapsed. Expand/collapse chevrons in both Chat Template and Raw Tree are subtle borderless SVG triangles that blend into the background (0 border, transparent background, opacity .55, accent on hover).
- The scope grammar is template-as-data: official Qwen2.5/Qwen3 chat_template Jinja files are vendored (`frontend/src/templates/`, SHA-256 in README) and parsed at runtime by `templateScopes.ts` into a scope registry (tools / tool_call / tool_response / think); the view no longer hardcodes scope names. Message text additionally gets generic balanced-tag detection so prompt-authored XML (e.g. Anthropic-style `<reference>` scopes) renders as collapsible scopes. Visual hierarchy uses one outer block border per ChatML segment; nested scopes use lighter 1px indent lines (no double border).
- UX polish after real-data feedback: newlines adjacent to scopes are compacted (no blank lines); short inline tags render on one line; JSON string leaves containing JSON render as nested JSON; multi-line strings render as collapsible pre-wrap text blocks; ChatML block borders and markers are colored per role (user blue / assistant green / tools amber / tool_result orange / unknown dashed gray).
- Split view: on wide viewports (width >= 1100 and aspect ratio >= 1.25) Raw and Chat Template render side by side in two independently scrolling panes sharing one artifact selection; a draggable divider resizes the panes (20%-80%), clicking any tab exits split, `\` toggles, the explicit choice persists in localStorage, and narrow viewports fall back to single-column automatically.
- Chat Template 首屏默认折叠所有顶层 ChatML blocks，并把阅读视口贴在内容底部；live 更新时只有仍在底部才跟随，用户主动向上滚动后锁定当前位置，新的内容在视口下方增长，不抢夺阅读位置。切换 artifact 重新开始贴底会话；stream 完成不自动跳 response artifact，继续在 inbound + live projection 上展示。Tab 顺序为 Chat Template / Raw / SSE；双栏顺序为 Chat Template / Raw。
- Exchange header metadata is inline with the request line; the viewer toolbar is one compact row. Search is a Raw-only control in that row (the existing tree matching/auto-expand/highlight behavior is retained), while the SHA-256 chip is intentionally removed from the viewer chrome.
- SSE live 观察走 additive `stream_event` workspace 事件：revision 恒为 0（流观察不提交修订），按 ordinal 去重（broker 回放/重连幂等），`byte_start/byte_end` 指回 response artifact 的原始字节。非 event-stream 响应不产生流事件。
- Stream tap 位于 gateway 响应体读取路径（直接转发与 hold 捕获两条路径共用一处包装）：仅消费字节副本，传输字节与 bypass body 完全不变（Go 测试逐字节断言）。
- 前端唯一归一层是 `streamIr.ts`：live 视图（workspace 事件流）与完整 SSE artifact 的 ChatML 渲染走同一 reducer（`applyStreamRecord`/`buildLiveStream`），两条路径按构造一致；未知事件进 passthrough block 保留可见。
- chat_completions SSE fixture 的 tool call arguments 修正为真实分片形态（`{"key":` + `"beta"}` 两段 delta），与 JSON fixture 语义一致；e2e response hash 与 protocol 测试事件数同步更新。
- provider extension / passthrough / unknown / source pointer 从第一版预留。
- request/response artifact 自动按需加载；原始 artifact immutable。

## Honest exits

- 无法从现有 artifact 保留语义时显示 unknown/raw，不猜测、不丢字段。
- 无法安全完成 SSE 事件归一化时保留原始事件 fallback，不伪造 delta。
- 发现 wire、secret、共享 DTO 或写入边界违规时停止新派发，记录证据并升级 manager/leader。
- 测试或浏览器证据不足时保持 phase `review`，不以 executor 自报完成。
