import type { ArtifactBody, ArtifactReadRequest, WorkspaceApi, ArtifactRef } from "./contracts";

/** Bounded browser artifact loader. It deduplicates reads per artifact, aborts
 * obsolete generations, and evicts least-recently-used bodies by byte budget. */
export class ArtifactLoader {
  private readonly inflight = new Map<string, Promise<ArtifactBody>>();
  private readonly controllers = new Map<string, AbortController>();
  private readonly cache = new Map<string, ArtifactBody>();
  private bytes = 0;
  private generation = 0;
  constructor(private readonly api: WorkspaceApi, private readonly budget = 32 << 20) {}

  beginGeneration(): void {
    this.generation += 1;
    for (const controller of this.controllers.values()) controller.abort();
    this.controllers.clear();
    this.inflight.clear();
  }
  get(key: string): ArtifactBody | undefined {
    const value = this.cache.get(key);
    if (value) { this.cache.delete(key); this.cache.set(key, value); }
    return value;
  }
  clear(): void { this.beginGeneration(); this.cache.clear(); this.bytes = 0; }
  async load(ref: ArtifactRef, request: ArtifactReadRequest, signal?: AbortSignal, cacheResult = true): Promise<ArtifactBody> {
    const key = `${ref.artifact_id}:${request.start ?? 0}:${request.end ?? "full"}`;
    const cached = this.get(key);
    if (cached && cached.complete) return cached;
    const existing = this.inflight.get(key);
    if (existing) return existing;
    const generation = this.generation;
    const controller = new AbortController();
    if (signal?.aborted) controller.abort();
    const onAbort = () => controller.abort();
    signal?.addEventListener("abort", onAbort, { once: true });
    let promise: Promise<ArtifactBody>;
    promise = this.api.readArtifact(request, controller.signal).then((body) => {
      if (generation !== this.generation || controller.signal.aborted) throw new DOMException("stale artifact load", "AbortError");
      if (cacheResult) this.put(key, body);
      return body;
    }).finally(() => {
      signal?.removeEventListener("abort", onAbort);
      if (this.inflight.get(key) === promise) this.inflight.delete(key);
      if (this.controllers.get(key) === controller) this.controllers.delete(key);
    });
    this.inflight.set(key, promise); this.controllers.set(key, controller);
    return promise;
  }
  private put(key: string, body: ArtifactBody): void {
    const previous = this.cache.get(key);
    if (previous) this.bytes -= previous.bytes.byteLength;
    this.cache.delete(key);
    this.cache.set(key, body); this.bytes += body.bytes.byteLength;
    while (this.bytes > this.budget && this.cache.size > 1) {
      const oldest = this.cache.keys().next().value as string;
      const value = this.cache.get(oldest); this.cache.delete(oldest);
      if (value) this.bytes -= value.bytes.byteLength;
    }
  }
}
