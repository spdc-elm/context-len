import { describe, expect, it, vi } from "vitest";
import { ArtifactLoader } from "../src/artifactLoader";
import type { ArtifactRef, WorkspaceApi } from "../src/contracts";

const ref = (id: string): ArtifactRef => ({ artifact_id: id, stage: "request.inbound", direction: "request", content_type: "text/plain", content_encoding: "identity", size: 3, sha256: "", complete: true, storage_ref: id });
const body = (id: string): ReturnType<WorkspaceApi["readArtifact"]> => Promise.resolve({ artifact_id: id, bytes: new Uint8Array([1, 2, 3]), content_type: "text/plain", content_encoding: "identity", complete: true, start: 0, end: 3, total_size: 3 });

describe("ArtifactLoader", () => {
  it("deduplicates concurrent reads and caches by byte budget", async () => {
    const readArtifact = vi.fn((request: { artifact_id: string }) => body(request.artifact_id));
    const api = { readArtifact } as unknown as WorkspaceApi;
    const loader = new ArtifactLoader(api, 4);
    const [first, second] = await Promise.all([loader.load(ref("a"), { artifact_id: "a" }), loader.load(ref("a"), { artifact_id: "a" })]);
    expect(first).toBe(second);
    expect(readArtifact).toHaveBeenCalledTimes(1);
    await loader.load(ref("b"), { artifact_id: "b" });
    expect(loader.get("a")).toBeUndefined();
  });

  it("rejects stale generations and aborts the underlying request", async () => {
    let reject!: (error: Error) => void;
    const readArtifact = vi.fn((_request: unknown, signal?: AbortSignal) => new Promise((_, fail) => { reject = fail; signal?.addEventListener("abort", () => fail(new DOMException("aborted", "AbortError"))); }));
    const api = { readArtifact } as unknown as WorkspaceApi;
    const loader = new ArtifactLoader(api);
    const pending = loader.load(ref("a"), { artifact_id: "a" });
    loader.beginGeneration();
    reject(new Error("late"));
    await expect(pending).rejects.toBeDefined();
    expect(readArtifact).toHaveBeenCalledTimes(1);
  });
});
