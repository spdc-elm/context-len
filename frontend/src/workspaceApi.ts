import type {
  ArtifactBody,
  ArtifactReadRequest,
  CommandResult,
  ExchangeEvent,
  ExchangeSnapshot,
  WorkspaceApi,
  WorkspaceCommand,
  WorkspacePolicy,
} from "./contracts";

/**
 * HTTP/WebSocket transport used by the browser workbench.
 *
 * The browser never talks to an upstream provider directly.  By default this
 * client uses the same origin as the UI (the Vite dev proxy can forward
 * `/api` to the local Go process); a base URL can be supplied for an embedded
 * or separately-served workspace.  Body responses are kept as bytes and are
 * not parsed or re-encoded by this layer.
 */

export interface WorkspaceSocket {
  onopen: ((event: Event) => void) | null;
  onmessage: ((event: MessageEvent) => void) | null;
  onerror: ((event: Event) => void) | null;
  onclose: ((event: CloseEvent) => void) | null;
  close(code?: number, reason?: string): void;
}

export interface WorkspaceApiOptions {
  /** API origin, for example `http://127.0.0.1:3001`. Defaults to location.origin. */
  baseUrl?: string;
  /** Prefix shared by REST and realtime routes. */
  apiPrefix?: string;
  /** Override fetch in tests or an embedding host. */
  fetch?: typeof globalThis.fetch;
  /** Override WebSocket in tests or an embedding host. */
  webSocketFactory?: (url: string) => WorkspaceSocket;
  /** Optional EventSource fallback for deployments without WebSocket support. */
  eventSourceFactory?: (url: string) => WorkspaceEventSource;
  /** Relative route for realtime events. Defaults to `${apiPrefix}/events`. */
  eventPath?: string;
  /** Reconnect an interrupted realtime stream. */
  reconnect?: boolean;
  reconnectDelayMs?: number;
  maxReconnectDelayMs?: number;
  /** Override command route; `:exchange_id` is replaced with the encoded id. */
  commandPath?: string;
  /** Override artifact route when an embedding server uses a different shape. */
  artifactPath?: string;
}

export interface WorkspaceEventSource {
  onopen: ((event: Event) => void) | null;
  onmessage: ((event: MessageEvent) => void) | null;
  onerror: ((event: Event) => void) | null;
  addEventListener?: (type: string, listener: (event: MessageEvent) => void) => void;
  removeEventListener?: (type: string, listener: (event: MessageEvent) => void) => void;
  close(): void;
}

export class WorkspaceHttpError extends Error {
  readonly status: number;
  readonly responseBody: string;

  constructor(status: number, responseBody: string, statusText?: string) {
    const suffix = responseBody.trim() ? `: ${responseBody.trim().slice(0, 300)}` : "";
    super(`workspace API ${status}${statusText ? ` ${statusText}` : ""}${suffix}`);
    this.name = "WorkspaceHttpError";
    this.status = status;
    this.responseBody = responseBody;
  }
}

type JsonRecord = Record<string, unknown>;

const DEFAULT_API_PREFIX = "/api";
const DEFAULT_RECONNECT_DELAY = 500;
const DEFAULT_MAX_RECONNECT_DELAY = 10_000;

function trimSlash(value: string): string {
  return value.replace(/\/+$/, "");
}

function normalisePrefix(value: string): string {
  const prefix = value.trim();
  if (!prefix) return DEFAULT_API_PREFIX;
  return `/${prefix.replace(/^\/+|\/+$/g, "")}`;
}

function locationOrigin(): string {
  if (typeof window !== "undefined" && window.location?.origin) return window.location.origin;
  return "http://127.0.0.1:3001";
}

function configuredOrigin(): string {
  // `import.meta.env` is replaced by Vite and is intentionally optional for
  // tests/builds outside Vite.  A same-origin default keeps credentials and
  // CORS policy local by default.
  const env = (import.meta as ImportMeta & { env?: Record<string, string | undefined> }).env;
  return trimSlash(env?.VITE_CONTEXT_LENS_API_URL?.trim() || locationOrigin());
}

function asRecord(value: unknown): JsonRecord | undefined {
  return value !== null && typeof value === "object" && !Array.isArray(value) ? value as JsonRecord : undefined;
}

function unwrap<T>(payload: unknown, keys: string[]): T {
  const record = asRecord(payload);
  if (record) {
    for (const key of keys) {
      if (key in record) return record[key] as T;
    }
  }
  return payload as T;
}

function parseNumberHeader(headers: Headers, name: string): number | undefined {
  const value = headers.get(name);
  if (!value) return undefined;
  const parsed = Number(value);
  return Number.isFinite(parsed) ? parsed : undefined;
}

function parseContentRange(value: string | null): { start?: number; end?: number; total?: number } {
  if (!value) return {};
  const match = /^bytes\s+(\d+)-(\d+)\/(\d+|\*)$/i.exec(value.trim());
  if (!match) return {};
  return {
    start: Number(match[1]),
    end: Number(match[2]) + 1,
    total: match[3] === "*" ? undefined : Number(match[3]),
  };
}

function decodeBase64(value: string): Uint8Array {
  if (typeof atob === "function") {
    const binary = atob(value);
    const bytes = new Uint8Array(binary.length);
    for (let index = 0; index < binary.length; index += 1) bytes[index] = binary.charCodeAt(index);
    return bytes;
  }
  // Node's Buffer is deliberately accessed through globalThis so the browser
  // bundle does not require a Node polyfill.
  const bufferCtor = (globalThis as typeof globalThis & { Buffer?: { from(value: string, encoding: string): Uint8Array } }).Buffer;
  if (bufferCtor) return new Uint8Array(bufferCtor.from(value, "base64"));
  throw new Error("workspace API returned base64 bytes but this runtime has no base64 decoder");
}

function dataFromEnvelope(payload: JsonRecord): Uint8Array | undefined {
  const candidate = payload.bytes ?? payload.base64 ?? payload.body_bytes;
  if (typeof candidate === "string") return decodeBase64(candidate);
  if (Array.isArray(candidate) && candidate.every((item) => typeof item === "number")) return new Uint8Array(candidate as number[]);
  if (typeof payload.body === "string") return new TextEncoder().encode(payload.body);
  return undefined;
}

function eventFromPayload(value: unknown): ExchangeEvent | undefined {
  let payload = value;
  if (typeof payload === "string") {
    try {
      payload = JSON.parse(payload) as unknown;
    } catch {
      return undefined;
    }
  }
  const record = asRecord(payload);
  if (!record) return undefined;

  // Some WS servers wrap messages in `{type: "exchange_event", event: {...}}`
  // while SSE uses `{event, data}`.  Accept both without changing the frozen
  // event DTO delivered to subscribers.
  const nested = record.payload ?? record.data ?? (typeof record.event === "object" ? record.event : undefined);
  if (nested && nested !== payload) {
    const nestedEvent = eventFromPayload(nested);
    if (nestedEvent) return nestedEvent;
    if (typeof nested === "string") {
      try {
        const parsed = JSON.parse(nested) as unknown;
        const parsedEvent = eventFromPayload(parsed);
        if (parsedEvent) return parsedEvent;
      } catch {
        // Continue to direct DTO parsing below.
      }
    }
  }
  if (typeof record.exchange_id !== "string" || typeof record.event_id !== "string") return undefined;
  if (typeof record.revision !== "number" || typeof record.kind !== "string") return undefined;
  if (!asRecord(record.snapshot_delta)) return undefined;
  return record as unknown as ExchangeEvent;
}

function toWebSocketUrl(httpUrl: string): string {
  const url = new URL(httpUrl);
  url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
  return url.toString();
}

function joinPath(prefix: string, suffix: string): string {
  if (/^https?:\/\//i.test(suffix) || /^wss?:\/\//i.test(suffix)) return suffix;
  return `${trimSlash(prefix)}/${suffix.replace(/^\/+/, "")}`;
}

export class LocalWorkspaceApi implements WorkspaceApi {
  private readonly origin: string;
  private readonly prefix: string;
  private readonly fetchImpl: typeof globalThis.fetch;
  private readonly socketFactory?: (url: string) => WorkspaceSocket;
  private readonly eventSourceFactory?: (url: string) => WorkspaceEventSource;
  private readonly eventPath: string;
  private readonly shouldReconnect: boolean;
  private readonly reconnectDelay: number;
  private readonly maxReconnectDelay: number;
  private readonly commandPath: string;
  private readonly artifactPath: string;
  private readonly listeners = new Set<(event: ExchangeEvent) => void>();
  private socket?: WorkspaceSocket;
  private eventSource?: WorkspaceEventSource;
  private reconnectTimer?: ReturnType<typeof setTimeout>;
  private reconnectAttempt = 0;
  private lastEventId = "";
  private closedByClient = false;

  constructor(options: WorkspaceApiOptions = {}) {
    this.origin = trimSlash(options.baseUrl?.trim() || configuredOrigin());
    this.prefix = normalisePrefix(options.apiPrefix ?? DEFAULT_API_PREFIX);
    const fetchImpl = options.fetch ?? globalThis.fetch?.bind(globalThis);
    if (!fetchImpl) throw new Error("workspace API requires fetch");
    this.fetchImpl = fetchImpl;
    this.socketFactory = options.webSocketFactory ?? (typeof WebSocket !== "undefined" ? (url) => new WebSocket(url) : undefined);
    this.eventSourceFactory = options.eventSourceFactory ?? (typeof EventSource !== "undefined" ? (url) => new EventSource(url) : undefined);
    this.eventPath = options.eventPath ?? `${this.prefix}/events`;
    this.shouldReconnect = options.reconnect ?? true;
    this.reconnectDelay = Math.max(50, options.reconnectDelayMs ?? DEFAULT_RECONNECT_DELAY);
    this.maxReconnectDelay = Math.max(this.reconnectDelay, options.maxReconnectDelayMs ?? DEFAULT_MAX_RECONNECT_DELAY);
    this.commandPath = options.commandPath ?? `${this.prefix}/exchanges/:exchange_id/command`;
    this.artifactPath = options.artifactPath ?? `${this.prefix}/artifacts`;
  }

  private url(path: string): string {
    if (/^https?:\/\//i.test(path)) return path;
    // URL handles a base origin with or without a trailing slash.  Prefixes
    // are absolute paths by design, so an API base path is retained only when
    // explicitly included by the caller in `apiPrefix`.
    return new URL(path.startsWith("/") ? path : `/${path}`, `${this.origin}/`).toString();
  }

  private async request<T>(path: string, init: RequestInit = {}, signal?: AbortSignal): Promise<T> {
    const headers = new Headers(init.headers);
    headers.set("accept", "application/json");
    const response = await this.fetchImpl(this.url(path), { ...init, headers, signal });
    const text = await response.text();
    if (!response.ok) throw new WorkspaceHttpError(response.status, text, response.statusText);
    if (!text.trim()) return undefined as T;
    try {
      return JSON.parse(text) as T;
    } catch {
      throw new Error(`workspace API returned invalid JSON for ${path}`);
    }
  }

  async listExchangesPage(limit: number, cursor?: string, signal?: AbortSignal): Promise<import("./contracts").ExchangePage> {
    const params = new URLSearchParams({ limit: String(Math.max(1, Math.min(limit, 1000))) });
    if (cursor) params.set("cursor", cursor);
    const payload = await this.request<unknown>(`${this.prefix}/exchanges?${params.toString()}`, { method: "GET" }, signal);
    const record = asRecord(payload);
    const exchanges = unwrap<unknown>(payload, ["exchanges", "items", "data"]);
    if (!Array.isArray(exchanges)) throw new Error("workspace API exchanges response is not an array");
    return { exchanges: exchanges as ExchangeSnapshot[], next_cursor: typeof record?.next_cursor === "string" ? record.next_cursor : undefined, has_more: record?.has_more === true };
  }

  async listExchanges(signal?: AbortSignal): Promise<ExchangeSnapshot[]> {
    const payload = await this.request<unknown>(`${this.prefix}/exchanges`, { method: "GET" }, signal);
    const exchanges = unwrap<unknown>(payload, ["exchanges", "items", "data"]);
    if (!Array.isArray(exchanges)) throw new Error("workspace API exchanges response is not an array");
    return exchanges as ExchangeSnapshot[];
  }

  async getExchange(exchangeId: string, signal?: AbortSignal): Promise<ExchangeSnapshot> {
    const payload = await this.request<unknown>(`${this.prefix}/exchanges/${encodeURIComponent(exchangeId)}`, { method: "GET" }, signal);
    return unwrap<ExchangeSnapshot>(payload, ["exchange", "snapshot"]);
  }

  async readArtifact(request: ArtifactReadRequest, signal?: AbortSignal): Promise<ArtifactBody> {
    const params = new URLSearchParams();
    if (request.start !== undefined) params.set("start", String(Math.max(0, request.start)));
    if (request.end !== undefined) params.set("end", String(Math.max(0, request.end)));
    const suffix = params.toString() ? `?${params.toString()}` : "";
    const response = await this.fetchImpl(this.url(`${trimSlash(this.artifactPath)}/${encodeURIComponent(request.artifact_id)}${suffix}`), {
      method: "GET",
      headers: { accept: "application/octet-stream, application/json" },
      signal,
    });
    if (!response.ok) {
      throw new WorkspaceHttpError(response.status, await response.text(), response.statusText);
    }

    const contentType = response.headers.get("x-artifact-content-type") ?? response.headers.get("content-type") ?? "application/octet-stream";
    const envelopeHeader = response.headers.get("x-context-lens-artifact-envelope") === "true";
    let bytes: Uint8Array;
    let envelope: JsonRecord | undefined;
    if (envelopeHeader || contentType.includes("vnd.context-lens.artifact+json")) {
      envelope = asRecord(await response.json());
      bytes = envelope ? dataFromEnvelope(envelope) ?? new Uint8Array() : new Uint8Array();
    } else {
      // The workspace endpoint returns exact artifact bytes. Ordinary
      // application/json is never interpreted as a transport envelope.
      bytes = new Uint8Array(await response.arrayBuffer());
    }

    const range = parseContentRange(response.headers.get("content-range"));
    const start = envelope && typeof envelope.start === "number" ? envelope.start : range.start ?? request.start ?? 0;
    const end = envelope && typeof envelope.end === "number" ? envelope.end : range.end ?? start + bytes.byteLength;
    const totalSize = envelope && typeof envelope.total_size === "number"
      ? envelope.total_size
      : range.total ?? parseNumberHeader(response.headers, "x-artifact-total-size") ?? Math.max(end, parseNumberHeader(response.headers, "content-length") ?? end);
    const completeHeader = response.headers.get("x-artifact-complete");
    const artifactComplete = envelope && typeof envelope.complete === "boolean"
      ? envelope.complete
      : completeHeader ? completeHeader === "true" : end >= totalSize;
    // `complete` describes the bytes loaded into this browser value, not just
    // whether the capture itself reached EOF. A bounded range of a complete
    // artifact is still an incomplete client-side document and must never be
    // parsed as if it were whole JSON.
    const complete = artifactComplete && start === 0 && end >= totalSize;
    return {
      artifact_id: typeof envelope?.artifact_id === "string" ? envelope.artifact_id : request.artifact_id,
      bytes,
      content_type: typeof envelope?.content_type === "string" ? envelope.content_type : contentType,
      content_encoding: typeof envelope?.content_encoding === "string" ? envelope.content_encoding : response.headers.get("x-artifact-content-encoding") ?? response.headers.get("content-encoding") ?? "identity",
      complete,
      start,
      end,
      total_size: totalSize,
    };
  }

  async command(command: WorkspaceCommand, signal?: AbortSignal): Promise<CommandResult> {
    const path = this.commandPath.replace(":exchange_id", encodeURIComponent(command.exchange_id));
    const payload = await this.request<unknown>(path, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(command),
    }, signal);
    return unwrap<CommandResult>(payload, ["result"]);
  }

  async getPolicy(signal?: AbortSignal): Promise<WorkspacePolicy> {
    const payload = await this.request<unknown>(`${this.prefix}/policy`, { method: "GET" }, signal);
    return unwrap<WorkspacePolicy>(payload, ["policy"]);
  }

  async setPolicy(policy: WorkspacePolicy, signal?: AbortSignal): Promise<WorkspacePolicy> {
    const payload = await this.request<unknown>(`${this.prefix}/policy`, {
      method: "PUT",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(policy),
    }, signal);
    return unwrap<WorkspacePolicy>(payload, ["policy"]);
  }

  async clearExchanges(signal?: AbortSignal): Promise<void> {
    await this.request<unknown>(`${this.prefix}/exchanges`, {
      method: "DELETE",
      headers: { "content-type": "application/json" },
      body: "{}",
    }, signal);
  }

  subscribe(listener: (event: ExchangeEvent) => void): () => void {
    this.listeners.add(listener);
    this.closedByClient = false;
    if (this.listeners.size === 1) this.openRealtime();
    return () => {
      this.listeners.delete(listener);
      if (this.listeners.size === 0) this.closeRealtime();
    };
  }

  private openRealtime(): void {
    if (this.socket || this.eventSource || this.listeners.size === 0) return;
    if (this.eventSourceFactory) {
      this.openEventSource();
      return;
    }
    if (this.socketFactory) this.openWebSocket();
  }

  private openWebSocket(): void {
    let socket: WorkspaceSocket;
    try {
      socket = this.socketFactory!(toWebSocketUrl(this.url(this.eventPath)));
    } catch {
      this.scheduleReconnect();
      return;
    }
    this.socket = socket;
    socket.onopen = () => {
      this.reconnectAttempt = 0;
    };
    socket.onmessage = (message) => {
      const event = eventFromPayload(message.data);
      if (event) this.emit(event);
    };
    socket.onerror = () => {
      // onclose normally follows; closing here avoids sockets that stay in a
      // half-open state in lightweight dev/test implementations.
      try { socket.close(); } catch { /* noop */ }
    };
    socket.onclose = () => {
      if (this.socket === socket) this.socket = undefined;
      this.scheduleReconnect();
    };
  }

  private openEventSource(): void {
    let source: WorkspaceEventSource;
    try {
      const eventUrl = new URL(this.url(this.eventPath));
      if (this.lastEventId) eventUrl.searchParams.set("last_event_id", this.lastEventId);
      source = this.eventSourceFactory!(eventUrl.toString());
    } catch {
      this.scheduleReconnect();
      return;
    }
    this.eventSource = source;
    const handleMessage = (message: MessageEvent) => {
      if (message.lastEventId) this.lastEventId = message.lastEventId;
      const event = eventFromPayload(message.data);
      if (event) this.emit(event);
    };
    source.onopen = () => {
      this.reconnectAttempt = 0;
    };
    source.onmessage = handleMessage;
    // The backend emits named SSE events (`event: completed`, etc.). Native
    // EventSource does not route those through `onmessage`, so subscribe to
    // every frozen event kind as well as the default message channel.
    const eventKinds = ["exchange_created", "request_held", "upstream_started", "response_held", "updated", "completed", "failed", "cancelled", "dropped", "stream_event"];
    for (const kind of eventKinds) source.addEventListener?.(kind, handleMessage);
    source.onerror = () => {
      try { source.close(); } catch { /* noop */ }
      if (this.eventSource === source) this.eventSource = undefined;
      this.scheduleReconnect();
    };
  }

  private emit(event: ExchangeEvent): void {
    for (const listener of [...this.listeners]) {
      try { listener(event); } catch { /* A subscriber must not break the stream. */ }
    }
  }

  private scheduleReconnect(): void {
    if (this.closedByClient || !this.shouldReconnect || this.listeners.size === 0 || this.reconnectTimer) return;
    const delay = Math.min(this.maxReconnectDelay, this.reconnectDelay * 2 ** this.reconnectAttempt);
    this.reconnectAttempt = Math.min(this.reconnectAttempt + 1, 16);
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = undefined;
      this.openRealtime();
    }, delay);
  }

  private closeRealtime(): void {
    this.closedByClient = true;
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = undefined;
    }
    const socket = this.socket;
    this.socket = undefined;
    if (socket) {
      socket.onopen = null;
      socket.onmessage = null;
      socket.onerror = null;
      socket.onclose = null;
      try { socket.close(1000, "workspace unsubscribed"); } catch { /* noop */ }
    }
    const source = this.eventSource;
    this.eventSource = undefined;
    if (source) {
      source.onopen = null;
      source.onmessage = null;
      source.onerror = null;
      try { source.close(); } catch { /* noop */ }
    }
  }
}

export function createLocalWorkspaceApi(options: WorkspaceApiOptions = {}): WorkspaceApi {
  return new LocalWorkspaceApi(options);
}

/** Alias useful to embedders that call the runtime seam simply `createWorkspaceApi`. */
export const createWorkspaceApi = createLocalWorkspaceApi;
export const HttpWorkspaceApi = LocalWorkspaceApi;
