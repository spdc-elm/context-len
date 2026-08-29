import { describe, expect, it, vi } from "vitest";
import type { ExchangeEvent, ExchangeSnapshot } from "../src/contracts";
import { LocalWorkspaceApi } from "../src/workspaceApi";

function snapshot(): ExchangeSnapshot {
  return {
    exchange_id: "ex-1",
    protocol: "responses",
    request: { envelope: { method: "POST", path: "/v1/responses", escaped_path: "/v1/responses", raw_query: "a=1", headers: {} }, artifact_refs: [] },
    response: { envelope: { status: 200, headers: {}, trailers: {} }, artifact_refs: [] },
    policy: { request_gate: "pass", response_gate: "hold" },
    state: "response_held",
    warnings: [],
    created_at: "2026-01-01T00:00:00.000Z",
    updated_at: "2026-01-01T00:00:01.000Z",
    revision: 3,
  };
}

class FakeSocket {
  onopen: ((event: Event) => void) | null = null;
  onmessage: ((event: MessageEvent) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  onclose: ((event: CloseEvent) => void) | null = null;
  closed = false;
  close() { this.closed = true; }
  emit(value: unknown) { this.onmessage?.({ data: JSON.stringify(value) } as MessageEvent); }
}

describe("local workspace API", () => {
  it("uses local REST routes, preserves artifact bytes, and serializes commands", async () => {
    const calls: Array<{ url: string; init?: RequestInit }> = [];
    const exchange = snapshot();
    const fetcher = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      calls.push({ url, init });
      if (url.endsWith("/exchanges")) return Response.json({ exchanges: [exchange] });
      if (url.endsWith("/policy") && init?.method === "GET") return Response.json({ policy: exchange.policy });
      if (url.endsWith("/policy")) return Response.json(exchange.policy);
      if (url.endsWith("/command") || url.endsWith("/commands")) return Response.json({ exchange, revision: 4 });
      return new Response(new Uint8Array([0, 1, 255]), { status: 206, headers: { "content-type": "application/octet-stream", "content-range": "bytes 2-4/8", "x-artifact-complete": "false" } });
    });
    const api = new LocalWorkspaceApi({ baseUrl: "http://127.0.0.1:3001/", fetch: fetcher });
    expect(await api.listExchanges()).toEqual([exchange]);
    expect(await api.getPolicy()).toEqual(exchange.policy);
    const body = await api.readArtifact({ artifact_id: "artifact", start: 2, end: 5 });
    expect([...body.bytes]).toEqual([0, 1, 255]);
    expect(body.start).toBe(2);
    expect(body.end).toBe(5);
    expect(body.total_size).toBe(8);
    await api.command({ exchange_id: "ex-1", base_revision: 3, kind: "release_unchanged" });
    await api.setPolicy({ request_gate: "hold", response_gate: "pass" });
    expect(calls.some((call) => call.url.endsWith("/command") && call.init?.method === "POST")).toBe(true);
    const commandCall = calls.find((call) => call.url.endsWith("/command"));
    expect(commandCall?.init?.body).toContain("release_unchanged");
  });

  it("subscribes to WS events and closes after the last listener leaves", () => {
    let socket: FakeSocket | undefined;
    const api = new LocalWorkspaceApi({
      baseUrl: "http://127.0.0.1:3001",
      fetch: vi.fn(),
      reconnect: false,
      webSocketFactory: (url) => {
        expect(url).toBe("ws://127.0.0.1:3001/api/events");
        socket = new FakeSocket();
        return socket;
      },
    });
    const events: ExchangeEvent[] = [];
    const unsubscribe = api.subscribe((event) => events.push(event));
    const event: ExchangeEvent = { event_id: "event-1", exchange_id: "ex-1", revision: 4, kind: "completed", snapshot_delta: { state: "completed" }, artifact_refs: [], created_at: "2026-01-01T00:00:02.000Z" };
    socket?.emit(event);
    expect(events).toEqual([event]);
    unsubscribe();
    expect(socket?.closed).toBe(true);
  });
});
