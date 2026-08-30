# AGENTS.md

## Subagent delegation defaults

When delegation is useful, use `agent_type: worker`, model `gpt-5.6-luna`, and `reasoning_effort: max` by default. Only inherit the current session model when the task genuinely needs stronger reasoning than that worker profile; keep delegated tasks read-only unless an explicit implementation task is assigned.

## Project purpose
 and same-protocol proxy for Responses, Chat Completions, and Anthropic Messages. It observes derived projections while keeping exact application-layer body bytes as the wire authority.

## Read first

- `README.md` — local setup, ports, configuration, and user-facing behavior
- `docs/protocol-contract.md` — protocol, wire fidelity, authentication, and mutation boundaries
- `docs/runtime-contract.md` — backend/workspace DTOs, events, and runtime seams
- `docs/chat-template-spec.md` — Context IR, Qwen ChatML, UI and SSE projection rules

## Non-negotiable boundaries

- Never use a decoded or re-encoded projection as transport input.
- Responses, Chat Completions, and Anthropic Messages remain same-protocol forwards. Do not add protocol conversion as a convenience.
- Raw request/response body bytes are immutable wire authority. Edits create derived artifacts; originals are never overwritten.
- Bypass and unchanged release paths must not JSON decode/re-encode, aggregate/rebuild SSE, or insert synthetic SSE records.
- Inspection, Context IR, ChatML text, hashes, summaries, and UI state are derived observations only.
- Preserve unknown fields/events, provider extensions, passthrough values, original values, and source pointers whenever the projection can locate them.
- Do not print, serialize, or place credentials in logs, workspace events, snapshots, frontend payloads, fixtures, screenshots, or errors.
- `config.local.json` is ignored, must remain mode `0600`, and must never be committed. Do not print its contents.
- Reference material under `Harness_model_coupling/ChatAPI` and `new-api/logs/relay-debug` is read-only. Do not copy it wholesale or call real third-party endpoints.

## Architecture and maintainability

- Prefer high cohesion and low coupling. Keep protocol adapters, shared IR, renderers, transport, state machines, persistence, and UI components behind narrow seams.
- Avoid god objects and god components. If a file starts owning unrelated parsing, transport, state, and presentation responsibilities, split the seam before adding more behavior.
- Reuse one canonical parser/reducer or token source rather than maintaining parallel implementations that can drift.
- Keep provider-specific logic in protocol adapters or template data, not scattered through React components.
- Make behavior explicit and inspectable. Do not hide mutations, credential replacement, or wire changes behind implicit UI actions.
- Keep comments focused on invariants and non-obvious constraints, not changelog history.

## Testing and verification — Occam's razor

- Test behavior, contracts, state transitions, security boundaries, and representative protocol cases. Do not add a separate test for every color, class name, trivial wrapper, or mechanically duplicated fixture.
- Prefer one representative matrix test or browser audit for equivalent visual/token states over many near-identical tests.
- Every new test should answer a concrete failure question. If removing it would not reduce confidence in a meaningful behavior, do not add it.
- Do not calculate hashes merely for display, repeated comparison, or speculative future use. Hash exact body bytes when required by artifact identity, integrity validation, ETag/fidelity checks, mutation CAS, or an explicit acceptance assertion; reuse an existing digest otherwise.
- Keep test fixtures synthetic and minimal. Do not add real prompts, credentials, or unnecessary large payloads.
- Before handoff, run the smallest relevant focused test, then the project-level checks when shared seams changed: `make test`, `go test -race ./...`, `go vet ./...`, `git diff --check`.
- For UI changes, perform a browser smoke check at relevant light/dark and responsive states. Use computed-style or a compact state matrix for equivalent visual states instead of multiplying tests.

## Local runtime

- Default workbench: `http://127.0.0.1:5172/`
- Default backend: `http://127.0.0.1:3001/`
- Bundled synthetic mock: `http://127.0.0.1:19091/`
- Override ports with `CONTEXT_LENS_ADDR` and `CONTEXT_LENS_FRONTEND_PORT`.
- `upstream_auth_mode=passthrough` supports base-URL-only harness integration by retaining inbound provider authentication headers. `configured` removes inbound credentials and injects the server-side top-level `api_key`.
- `client_auth` is optional and accepts standard client headers only: `Authorization: Bearer`, `X-API-Key`, or `API-Key`. Do not introduce a context-lens-specific client header.
- `/healthz` is public for readiness. Workspace `/api` is a loopback workbench surface; `/v1/*` is the proxy surface.
