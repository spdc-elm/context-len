# Runtime Contract

This is the frozen MVP seam between proxy backend and workspace UI. JSON names are stable; additive fields are allowed, unknown fields must be preserved in `projection`.

## Artifact

`ArtifactRef { artifact_id, stage, direction, content_type, content_encoding, size, sha256, complete, storage_ref }`.

Body bytes are stored externally and retrieved by artifact id. `sha256` is over exact application body bytes. Original artifacts are immutable; edits create a new artifact.

## Exchange snapshot

`ExchangeSnapshot { exchange_id, protocol, request, response, policy, state, warnings, created_at, updated_at }`.

`request` and `response` contain an HTTP envelope plus artifact refs. Request envelope has `method, path, escaped_path, raw_query, headers`; response envelope has `status, headers, trailers` and timestamps. Headers exposed to UI are redacted according to server policy.

`policy { request_gate: pass|hold, response_gate: pass|hold }`.

States are `received, request_held, upstream_running, response_held, completed, dropped, cancelled, failed`.

## Commands

`forward_unchanged`, `forward_edited`, `manual_response`, `release_unchanged`, `release_edited`, `replace_response`, `drop`, `abort`.

Every command includes `{ exchange_id, base_revision }`; mutations additionally include a patch/raw replacement and must return a derived artifact and validation result. Stale revisions are rejected without changing state.

## Events

`ExchangeEvent { event_id, exchange_id, revision, kind, snapshot_delta, artifact_refs, created_at }`.

Kinds include `exchange_created, request_held, upstream_started, response_held, updated, completed, failed, cancelled, dropped`. Large bodies never appear inline in snapshots/events.

The additive `stream_event` kind carries one observed SSE record while a response body is still streaming: `stream { ordinal, name, sse_id, data, complete, byte_start, byte_end }`. Stream events never commit a revision (revision stays 0), are deduplicated by ordinal on the client, and are display-only copies — the response artifact remains the only wire authority. `byte_start`/`byte_end` locate the record's raw bytes inside the response artifact. Non-event-stream responses emit no stream events.

## Transport invariant

The proxy forwards artifact readers directly in bypass/release-unchanged paths. It never JSON decodes/re-encodes or aggregates/re-generates SSE on those paths. Inspector output is projection-only and cannot become transport input.

## Derived context projection seam

Workspace may add an additive context projection to request/response parts or events. It is derived from artifact bytes and is never accepted as transport input. The projection may contain a loss-aware Context IR, Qwen ChatML render blocks, provider extensions/passthrough, unknown items, source JSON pointers, and typed streaming deltas. Large rendered bodies remain lazy artifact reads; secrets remain server-side redacted.

The product behavior and phase acceptance for this seam are defined in [`docs/chat-template-spec.md`](chat-template-spec.md).
