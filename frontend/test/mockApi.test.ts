import { describe, expect, it } from "vitest";
import { createMockWorkspaceApi, MockWorkspaceApi, decodeArtifactBody } from "../src/mockApi";
import { requestArtifact, responseArtifact, type ExchangeSnapshot } from "../src/contracts";

describe("mock workspace API contract", () => {
  it("serves immutable opaque artifact bytes without network calls", async () => {
    const api = createMockWorkspaceApi();
    const exchanges = await api.listExchanges();
    expect(exchanges).toHaveLength(4);
    const request = requestArtifact(exchanges[0]);
    expect(request?.storage_ref.startsWith("mock://")).toBe(true);
    const body = await api.readArtifact({ artifact_id: request!.artifact_id });
    expect(body.bytes.constructor.name).toBe("Uint8Array");
    expect(body.bytes.byteLength).toBeGreaterThan(0);
    expect(decodeArtifactBody(body)).toContain('"model"');
    expect(body.complete).toBe(true);
  });

  it("emits an event and advances revision for an explicit hold command", async () => {
    const api = new MockWorkspaceApi();
    const exchange = (await api.listExchanges()).find((item) => item.state === "request_held")!;
    const received: string[] = [];
    const unsubscribe = api.subscribe((event) => received.push(`${event.kind}:${event.revision}`));
    const result = await api.command({ exchange_id: exchange.exchange_id, base_revision: 0, kind: "forward_unchanged" });
    unsubscribe();
    expect(result.exchange.state).toBe("completed");
    expect(result.revision).toBe(1);
    expect(received).toEqual(["completed:1"]);
  });

  it("rejects stale revisions without changing the exchange", async () => {
    const api = createMockWorkspaceApi();
    const exchange = (await api.listExchanges()).find((item) => item.state === "response_held")!;
    await api.command({ exchange_id: exchange.exchange_id, base_revision: 0, kind: "release_unchanged" });
    await expect(api.command({ exchange_id: exchange.exchange_id, base_revision: 0, kind: "release_unchanged" })).rejects.toThrow("stale revision");
    expect((await api.getExchange(exchange.exchange_id)).state).toBe("completed");
  });

  it("creates a derived artifact only for explicit mutation/manual commands", async () => {
    const api = createMockWorkspaceApi();
    const exchange = (await api.listExchanges()).find((item) => item.state === "response_held")!;
    const original = responseArtifact(exchange)!;
    const result = await api.command({
      exchange_id: exchange.exchange_id,
      base_revision: 0,
      kind: "release_edited",
      mutation: { raw_replacement: "{\"edited\":true}", base_artifact_id: original.artifact_id, base_sha256: original.sha256 },
    });
    expect(result.mutation?.derived_artifact?.artifact_id).toContain("mock-derived");
    expect(result.mutation?.base_sha256).toBe(original.sha256);
    const updated = await api.getExchange(exchange.exchange_id);
    expect(updated.response.artifact_refs).toHaveLength(2);
  });

  it("supports capture mode, storage stats, and whole-session deletion in the standard mock", async () => {
    const api = new MockWorkspaceApi();
    expect(await api.getCaptureMode?.()).toBe("passthrough");
    await api.setPolicy({ request_gate: "hold", response_gate: "pass" });
    await expect(api.setCaptureMode?.("passthrough")).rejects.toThrow("capture mode conflict");
    expect(await api.setCaptureMode?.("capture")).toBe("capture");
    const stats = await api.getStorageStats?.();
    expect(stats?.disk_limit).toBeGreaterThan(0);
    const removed: string[] = [];
    const unsubscribe = api.subscribe((event) => { if (event.kind === "exchange_removed") removed.push(event.exchange_id); });
    await api.deleteSession?.("sess-mock-responses");
    unsubscribe();
    expect(removed.length).toBeGreaterThan(0);
    expect((await api.listExchanges()).some((item) => item.session?.session_id === "sess-mock-responses")).toBe(false);
  });
  it("keeps policy updates separate from existing exchange snapshots", async () => {
    const api = createMockWorkspaceApi();
    const before = await api.listExchanges();
    await api.setPolicy({ request_gate: "hold", response_gate: "hold" });
    expect(await api.getPolicy()).toEqual({ request_gate: "hold", response_gate: "hold" });
    const after = await api.listExchanges();
    expect(after.map((item) => item.policy)).toEqual(before.map((item) => item.policy));
  });
});
