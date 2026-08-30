# Runtime Contract

This is the frozen MVP seam between proxy backend and workspace UI. JSON names are stable; additive fields are allowed, unknown fields must be preserved in `projection`.

## Artifact

`ArtifactRef { artifact_id, stage, direction, content_type, content_encoding, size, sha256, complete, storage_ref }`.

Body bytes are stored externally and retrieved by artifact id. `sha256` is over exact application body bytes. Original artifacts are immutable; edits create a new artifact. The default process uses ephemeral in-memory metadata with bounded artifact memory and runtime spill files (spill files are not a restart contract). Standalone `cmd/context-lens` enables durable-local mode only when `CONTEXT_LENS_DURABLE=1`; its metadata catalog is `CONTEXT_LENS_DATA_DIR/catalog.sqlite` (default data dir `.context-lens-run`, overridable with `CONTEXT_LENS_DATA_DIR`) and private content-addressed blobs live below the data directory's `artifacts` root. SQLite stores sessions, exchanges, artifact references, blob relations, settings, and event metadata only—raw request/response bodies never enter the database. On restart, durable metadata and artifact references are hydrated lazily; missing/corrupt external blobs are reported as artifact errors rather than replaced by decoded projections. Standalone artifact TTL expiration and its janitor are currently disabled because independent cleanup cannot account for registry/catalog ownership; capacity is bounded by `MaxTotalBytes` and explicit Clear. Owner-aware retention is future work and is not currently implemented.

Artifact reads are explicit and bounded. `HEAD`/metadata access does not load body bytes; range reads use `Range: bytes=start-end` (end inclusive on HTTP, translated to the store's exclusive end) and full/download reads are subject to workspace limits. Search is performed through the range-capable store where available and returns bounded matches. These operations never become transport input.

Completeness has two independent meanings: `ArtifactRef.complete` says whether capture reached producer EOF, while an HTTP range/preview may represent only the loaded byte interval. A partial capture or partial load must remain visibly partial/truncated and must not be parsed as complete JSON or SSE. Streaming responses are forwarded from readers while capture and typed `stream_event` observations are emitted; observations are display-only and do not alter artifact bytes.

## Exchange snapshot

`ExchangeSnapshot { exchange_id, protocol, request, response, policy, state, warnings, created_at, updated_at }`.

`request` and `response` contain an HTTP envelope plus artifact refs. Request envelope has `method, path, escaped_path, raw_query, headers`; response envelope has `status, headers, trailers` and timestamps. Headers exposed to UI are redacted according to server policy.

`policy { request_gate: pass|hold, response_gate: pass|hold }`.

States are `received, request_held, upstream_running, response_held, completed, dropped, cancelled, failed`.

Cancellation semantics follow the upstream leg, not the downstream connection's post-delivery liveness. Once the protocol terminal record (Responses `response.completed`/`failed`/`incomplete`, Chat Completions `[DONE]`, Anthropic `message_stop`) has been written toward the client, a client disconnect no longer cancels the exchange: the gateway drains the remaining upstream body (bounded by a 5s drain timeout by default), the exchange completes, and the artifact is complete when EOF was observed or incomplete with an explicit warning otherwise. A disconnect before the terminal record cancels the exchange and retains the bytes captured so far as an incomplete response artifact (`response.upstream`; the direct path also keeps the streamed `response.downstream` prefix).

## Workspace listing, artifact access, and clearing

Workspace exchange listing is metadata-only and bounded. `GET /api/exchanges` accepts `limit` and an opaque `cursor`; when more rows remain it returns `X-Next-Cursor`. Implementations must not read artifact bodies while listing or emitting events. This is a page API, not a guarantee of a complete session projection: callers/UI that have not fetched later pages cannot infer or render exchanges that are outside the loaded page or lineage. Artifact body access is lazy and explicit via `/api/artifacts/{artifact_id}` (including HEAD, range, and bounded search behavior); full body loading is not implicit in snapshot/event requests.

`DELETE /api/exchanges` is the explicit Clear Workspace action. It clears in-flight queue work and derived session/event state, removes catalog metadata and unreferenced blobs where configured, and advances the workspace generation so callbacks from pre-clear work cannot repopulate the new workspace. It is not an automatic idle cleanup mechanism. No 30-minute Session GC or `favorite` retention feature is currently implemented; do not rely on either.

## Commands

`forward_unchanged`, `forward_edited`, `manual_response`, `release_unchanged`, `release_edited`, `replace_response`, `drop`, `abort`.

Every command includes `{ exchange_id, base_revision }`; mutations additionally include a patch/raw replacement and must return a derived artifact and validation result. Stale revisions are rejected without changing state.

## Events

`ExchangeEvent { event_id, exchange_id, revision, kind, snapshot_delta, artifact_refs, created_at }`.

Kinds include `exchange_created, request_held, upstream_started, response_held, updated, completed, failed, cancelled, dropped`. Large bodies never appear inline in snapshots/events.

The additive `stream_event` kind carries one observed SSE record while a response body is still streaming: `stream { ordinal, name, sse_id, data, complete, byte_start, byte_end }`. Stream events never commit a revision (revision stays 0), are deduplicated by ordinal on the client, and are display-only copies — the response artifact remains the only wire authority. `byte_start`/`byte_end` locate the record's raw bytes inside the response artifact. Non-event-stream responses emit no stream events.

## Optional proxy access authentication

The process can optionally require a separate client API key for `/v1/*` proxy requests. Configure it in the private local runtime file:

```json
"client_auth": {
  "enabled": true,
  "api_key": "a-local-client-key"
}
```

Accepted request headers are the standard credentials clients already use: `Authorization: Bearer <client key>`, `X-API-Key`, and `API-Key`. No context-lens-specific client header is required. The client key is checked before request-body capture. In `configured` upstream mode it is removed before forwarding; in `passthrough` mode it is the provider credential and is forwarded by definition. In both modes it is redacted from workspace projections. `GET /healthz` remains public for readiness checks; `/api` remains a loopback workspace surface. The existing top-level `api_key` is independent and is only the server-side upstream credential.

## Upstream authentication mode

`upstream_auth_mode` is `passthrough` or `configured`. In `passthrough`, inbound provider authentication headers are retained for the upstream request, which supports the pure transparent "change only base URL" path. In `configured`, inbound credential headers are removed and the server-side top-level `api_key` is injected as `Authorization: Bearer ...`. For backward compatibility, an omitted mode infers `configured` when top-level `api_key` is non-empty and `passthrough` otherwise. Explicit `passthrough` and a non-empty top-level `api_key` are rejected to avoid ambiguous credential ownership.

## Transport invariant

The proxy forwards artifact readers directly in bypass/release-unchanged paths. It never JSON decodes/re-encodes or aggregates/re-generates SSE on those paths. Inspector output is projection-only and cannot become transport input.

## Derived context projection seam

Workspace may add an additive context projection to request/response parts or events. It is derived from artifact bytes and is never accepted as transport input. The projection may contain a loss-aware Context IR, Qwen ChatML render blocks, provider extensions/passthrough, unknown items, source JSON pointers, and typed streaming deltas. Large rendered bodies remain lazy artifact reads; secrets remain server-side redacted.

The derived context projection is bounded by the artifacts and exchanges that have been explicitly loaded. It must preserve `partial`/`truncated` status when capture or reads are incomplete; a range/preview is never parsed or presented as a complete JSON/SSE body. The current Raw JSON tree uses bounded default expansion and summaries, but still materializes the complete node tree for the loaded JSON. SSE event/live projections render the observed or loaded records and are not fully virtualized/windowed. A future virtualized renderer must remain a derived view and must not become transport input.

The product behavior and phase acceptance for this seam are defined in [`docs/chat-template-spec.md`](chat-template-spec.md).
