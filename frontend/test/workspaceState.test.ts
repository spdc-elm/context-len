import { describe, expect, it } from "vitest";
import {
  initialWorkspaceState,
  selectedExchange,
  workspaceReducer,
  type WorkspaceState,
} from "../src/workspaceState";
import type { ExchangeEvent, ExchangeSnapshot } from "../src/contracts";

function snapshot(id = "exchange-1"): ExchangeSnapshot {
  return {
    exchange_id: id,
    protocol: "responses",
    request: {
      envelope: { method: "POST", path: "/v1/responses", escaped_path: "/v1/responses", raw_query: "", headers: {} },
      artifact_refs: [],
    },
    response: {
      envelope: { status: 200, headers: {}, trailers: {} },
      artifact_refs: [],
    },
    policy: { request_gate: "pass", response_gate: "hold" },
    state: "response_held",
    warnings: [],
    created_at: "2026-01-01T00:00:00.000Z",
    updated_at: "2026-01-01T00:00:01.000Z",
  };
}

describe("workspace reducer", () => {
  it("loads exchanges and selects the first one", () => {
    const exchange = snapshot();
    const state = workspaceReducer(initialWorkspaceState, { type: "load_succeeded", exchanges: [exchange], policy: exchange.policy });
    expect(state.loading).toBe(false);
    expect(state.selectedExchangeId).toBe(exchange.exchange_id);
    expect(selectedExchange(state)).toEqual(exchange);
    expect(state.revisions[exchange.exchange_id]).toBe(0);
  });

  it("applies realtime deltas in revision order and ignores stale events", () => {
    const exchange = snapshot();
    const loaded = workspaceReducer(initialWorkspaceState, { type: "load_succeeded", exchanges: [exchange], policy: exchange.policy });
    const newer: ExchangeEvent = {
      event_id: "event-2",
      exchange_id: exchange.exchange_id,
      revision: 2,
      kind: "completed",
      snapshot_delta: { state: "completed", warnings: ["done"] },
      artifact_refs: [],
      created_at: "2026-01-01T00:00:02.000Z",
    };
    const stale: ExchangeEvent = { ...newer, event_id: "event-1", revision: 1, snapshot_delta: { state: "request_held" } };
    const applied = workspaceReducer(loaded, { type: "event_received", event: newer });
    const ignored = workspaceReducer(applied, { type: "event_received", event: stale });
    expect(applied.revisions[exchange.exchange_id]).toBe(2);
    expect(selectedExchange(applied)?.state).toBe("completed");
    expect(selectedExchange(ignored)?.state).toBe("completed");
    expect(selectedExchange(ignored)?.warnings).toEqual(["done"]);
  });

  it("merges nested request and response deltas without discarding envelope data", () => {
    const exchange = snapshot();
    const loaded = workspaceReducer(initialWorkspaceState, { type: "load_succeeded", exchanges: [exchange], policy: exchange.policy });
    const event: ExchangeEvent = {
      event_id: "event-1",
      exchange_id: exchange.exchange_id,
      revision: 1,
      kind: "updated",
      snapshot_delta: { request: { envelope: { path: "/v1/responses?keep" } }, response: { envelope: { status: 202 } } },
      artifact_refs: [],
      created_at: "2026-01-01T00:00:02.000Z",
    };
    const next = workspaceReducer(loaded, { type: "event_received", event });
    expect(next.exchanges[0].request.envelope.method).toBe("POST");
    expect(next.exchanges[0].request.envelope.path).toBe("/v1/responses?keep");
    expect(next.exchanges[0].response.envelope.status).toBe(202);
  });

  it("normalizes null wire arrays from the Go API", () => {
    const raw = snapshot() as unknown as Record<string, unknown>;
    (raw.request as Record<string, unknown>).artifact_refs = null;
    (raw.response as Record<string, unknown>).artifact_refs = null;
    raw.warnings = null;
    const state = workspaceReducer(initialWorkspaceState, { type: "load_succeeded", exchanges: [raw as unknown as ExchangeSnapshot], policy: snapshot().policy });
    expect(state.exchanges[0].request.artifact_refs).toEqual([]);
    expect(state.exchanges[0].response.artifact_refs).toEqual([]);
    expect(state.exchanges[0].warnings).toEqual([]);
  });

  it("creates a queue item from a delta-only exchange_created event", () => {
    const event: ExchangeEvent = {
      event_id: "event-created", exchange_id: "new-live", revision: 1, kind: "exchange_created", created_at: "2026-01-01T00:00:00Z", artifact_refs: [],
      snapshot_delta: {
        exchange_id: "new-live", protocol: "responses", state: "upstream_running", policy: { request_gate: "pass", response_gate: "pass" },
        request: { envelope: { method: "POST", path: "/v1/responses", escaped_path: "/v1/responses", raw_query: "", headers: {} }, artifact_refs: [] },
        response: { artifact_refs: [] }, warnings: [], created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z",
      },
    };
    const state = workspaceReducer(initialWorkspaceState, { type: "event_received", event });
    expect(state.exchanges[0]?.exchange_id).toBe("new-live");
    expect(state.revisions["new-live"]).toBe(1);
  });

  it("resets body/search state when selecting another exchange", () => {
    const exchange = snapshot();
    const loaded = workspaceReducer(initialWorkspaceState, { type: "load_succeeded", exchanges: [exchange], policy: exchange.policy });
    const withSearch: WorkspaceState = { ...loaded, search: "foo", loadedBodies: { artifact: { artifactId: "artifact", text: "{}", start: 0, end: 2, totalSize: 2, complete: true } } };
    const next = workspaceReducer(withSearch, { type: "select_exchange", exchangeId: "another" });
    expect(next.search).toBe("");
    expect(next.loadedBodies).toEqual({});
  });

  it("applies stream events to a live stream keyed by exchange", () => {
    const exchange = snapshot();
    const loaded = workspaceReducer(initialWorkspaceState, { type: "load_succeeded", exchanges: [exchange], policy: exchange.policy });
    const streamEvent = (ordinal: number, data: string): ExchangeEvent => ({
      event_id: `${exchange.exchange_id}:stream:${ordinal}`,
      exchange_id: exchange.exchange_id,
      revision: 0,
      kind: "stream_event",
      created_at: "2026-01-01T00:00:00Z",
      artifact_refs: [],
      snapshot_delta: { exchange_id: exchange.exchange_id },
      stream: { ordinal, data, complete: true },
    });
    let state = workspaceReducer(loaded, { type: "event_received", event: streamEvent(0, JSON.stringify({ type: "response.output_text.delta", item_id: "msg_1", delta: "hi" })) });
    state = workspaceReducer(state, { type: "event_received", event: streamEvent(1, JSON.stringify({ type: "response.output_text.delta", item_id: "msg_1", delta: " there" })) });
    const stream = state.streams[exchange.exchange_id];
    expect(stream?.blocks.find((block) => block.id === "live:msg_1")?.text).toBe("hi there");
    // Replay (same ordinal) is idempotent.
    state = workspaceReducer(state, { type: "event_received", event: streamEvent(1, JSON.stringify({ choices: [{ index: 0, delta: { content: " there" } }] })) });
    expect(state.streams[exchange.exchange_id]?.eventCount).toBe(2);
    // Exchange lifecycle closes the stream.
    const completed: ExchangeEvent = {
      event_id: "e2", exchange_id: exchange.exchange_id, revision: 3, kind: "completed", created_at: "2026-01-01T00:00:01Z",
      artifact_refs: [], snapshot_delta: { exchange_id: exchange.exchange_id, state: "completed", updated_at: "2026-01-01T00:00:01Z" },
    };
    state = workspaceReducer(state, { type: "event_received", event: completed });
    expect(state.streams[exchange.exchange_id]?.status).toBe("completed");
    expect(state.streams[exchange.exchange_id]?.blocks.find((block) => block.id === "live:msg_1")?.text).toBe("hi there");
  });

  it("ignores stream events for unknown exchanges", () => {
    const event: ExchangeEvent = {
      event_id: "s", exchange_id: "ghost", revision: 0, kind: "stream_event", created_at: "2026-01-01T00:00:00Z",
      artifact_refs: [], snapshot_delta: { exchange_id: "ghost" }, stream: { ordinal: 0, data: "{}", complete: true },
    };
    const state = workspaceReducer(initialWorkspaceState, { type: "event_received", event });
    expect(state.streams["ghost"]).toBeUndefined();
  });
});
