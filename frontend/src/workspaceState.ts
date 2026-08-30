import {
  type CommandResult,
  type ExchangeEvent,
  type ExchangeSnapshot,
  type GateMode,
  type WorkspacePolicy,
} from "./contracts";
import {
  groupSessions,
  type SessionGroup,
} from "./sessionTree";
import {
  applyStreamRecord,
  applyStreamRecords,
  applyStreamTerminus,
  initialLiveStream,
  type LiveStreamState,
} from "./streamIr";

export type DetailTab = "raw" | "chat_template" | "sse";

export interface LoadedArtifact {
  artifactId: string;
  text: string;
  /** Exact bytes retained by the browser cache (UTF-8 text length is not exact). */
  byteLength?: number;
  start: number;
  end: number;
  totalSize: number;
  complete: boolean;
}

export interface WorkspaceState {
  exchanges: ExchangeSnapshot[];
  revisions: Record<string, number>;
  selectedExchangeId?: string;
  /** When set, selecting the session row follows that session's latest turn. */
  followSessionId?: string;
  activeTab: DetailTab;
  policy: WorkspacePolicy;
  loading: boolean;
  error?: string;
  bodyLoading: boolean;
  bodyLoadErrorArtifactId?: string;
  loadedBodies: Record<string, LoadedArtifact>;
  search: string;
  streams: Record<string, LiveStreamState>;
}

export type WorkspaceAction =
  | { type: "load_started" }
  | { type: "load_succeeded"; exchanges: ExchangeSnapshot[]; policy: WorkspacePolicy }
  | { type: "page_loaded"; exchanges: ExchangeSnapshot[] }
  | { type: "load_failed"; error: string }
  | { type: "clear_error" }
  | { type: "select_exchange"; exchangeId: string; followSessionId?: string }
  | { type: "set_tab"; tab: DetailTab }
  | { type: "set_policy"; gate: "request_gate" | "response_gate"; value: GateMode }
  | { type: "policy_saved"; policy: WorkspacePolicy }
  | { type: "event_received"; event: ExchangeEvent }
  | { type: "stream_events_received"; events: ExchangeEvent[] }
  | { type: "command_succeeded"; result: CommandResult }
  | { type: "exchanges_cleared" }
  | { type: "body_load_started"; artifactId?: string }
  | { type: "body_load_finished" }
  | { type: "body_loaded"; body: LoadedArtifact }
  | { type: "body_load_failed"; error: string; artifactId?: string }
  | { type: "set_search"; value: string };

export const initialWorkspaceState: WorkspaceState = {
  exchanges: [],
  revisions: {},
  activeTab: "raw",
  policy: { request_gate: "pass", response_gate: "pass" },
  loading: true,
  bodyLoading: false,
  loadedBodies: {},
  search: "",
  streams: {},
};

function latestSessionExchangeId(exchanges: ExchangeSnapshot[], sessionId: string | undefined): string | undefined {
  if (!sessionId) return undefined;
  const group = groupSessions(exchanges).find((item: SessionGroup) => item.sessionId === sessionId);
  return group?.latest.exchange_id;
}

function followLatestSession(state: WorkspaceState): WorkspaceState {
  if (!state.followSessionId) return state;
  const latest = latestSessionExchangeId(state.exchanges, state.followSessionId);
  if (!latest || latest === state.selectedExchangeId) return state;
  return { ...state, selectedExchangeId: latest, loadedBodies: {}, search: "" };
}

function loadedBodyBytes(item: LoadedArtifact): number {
  return item.byteLength ?? new TextEncoder().encode(item.text).byteLength;
}

function boundedLoadedBodies(current: Record<string, LoadedArtifact>, added: LoadedArtifact): Record<string, LoadedArtifact> {
  // Delete before re-inserting so replacing an entry also refreshes its LRU
  // position. Object property order is the reducer's compact recency list.
  const next = { ...current };
  delete next[added.artifactId];
  next[added.artifactId] = added;
  let total = Object.values(next).reduce((sum, item) => sum + loadedBodyBytes(item), 0);
  for (const id of Object.keys(next)) {
    if (total <= 32 << 20 || id === added.artifactId) break;
    total -= loadedBodyBytes(next[id]);
    delete next[id];
  }
  return next;
}
function revisionValue(exchange: ExchangeSnapshot): number {
  return typeof exchange.revision === "number" && Number.isFinite(exchange.revision) ? exchange.revision : 0;
}

function revisionOf(state: WorkspaceState, exchangeId: string): number {
  return state.revisions[exchangeId] ?? 0;
}

function normalizeSnapshot(snapshot: ExchangeSnapshot): ExchangeSnapshot {
  return {
    ...snapshot,
    warnings: snapshot.warnings ?? [],
    request: { ...snapshot.request, artifact_refs: snapshot.request?.artifact_refs ?? [] },
    response: { ...snapshot.response, artifact_refs: snapshot.response?.artifact_refs ?? [] },
  };
}

function snapshotFromEvent(event: ExchangeEvent): ExchangeSnapshot | undefined {
  const d = event.snapshot_delta;
  if (!d.request || !d.policy || !d.protocol || !d.state) return undefined;
  return normalizeSnapshot({
    exchange_id: event.exchange_id,
    protocol: d.protocol,
    request: { envelope: { method: "", path: "", escaped_path: "", raw_query: "", headers: {}, ...(d.request.envelope ?? {}) }, artifact_refs: d.request.artifact_refs ?? [], projection: d.request.projection },
    response: { envelope: { status: 0, headers: {}, trailers: {}, ...(d.response?.envelope ?? {}) }, artifact_refs: d.response?.artifact_refs ?? [], projection: d.response?.projection },
    policy: { request_gate: d.policy.request_gate ?? "pass", response_gate: d.policy.response_gate ?? "pass" },
    state: d.state,
    warnings: d.warnings ?? [],
    created_at: d.created_at ?? event.created_at,
    updated_at: d.updated_at ?? event.created_at,
    revision: event.revision,
    summary: d.summary,
    session: d.session,
  });
}

function mergeSnapshot(snapshot: ExchangeSnapshot, delta: ExchangeEvent["snapshot_delta"]): ExchangeSnapshot {
  const next: ExchangeSnapshot = { ...snapshot };
  const { request, response, policy, ...topLevel } = delta;
  Object.assign(next, topLevel);
  if (request) {
    next.request = {
      ...snapshot.request,
      ...request,
      envelope: { ...snapshot.request.envelope, ...(request.envelope ?? {}) },
    };
  }
  if (response) {
    next.response = {
      ...snapshot.response,
      ...response,
      envelope: { ...snapshot.response.envelope, ...(response.envelope ?? {}) },
    };
  }
  if (policy) next.policy = { ...snapshot.policy, ...policy };
  return next;
}

function appendEventArtifacts(snapshot: ExchangeSnapshot, refs: ExchangeEvent["artifact_refs"]): ExchangeSnapshot {
  const safeRefs = refs ?? [];
  if (safeRefs.length === 0) return snapshot;
  const requestRefs = [...(snapshot.request.artifact_refs ?? [])];
  const responseRefs = [...(snapshot.response.artifact_refs ?? [])];
  for (const ref of safeRefs) {
    // Backend wire directions are `inbound`/`upstream`/`downstream`, so the
    // stage prefix is the authoritative side discriminator for event refs.
    const target = ref.stage.startsWith("request.") || ref.direction === "request" ? requestRefs : responseRefs;
    if (!target.some((item) => item.artifact_id === ref.artifact_id)) target.push(ref);
  }
  return {
    ...snapshot,
    request: { ...snapshot.request, artifact_refs: requestRefs },
    response: { ...snapshot.response, artifact_refs: responseRefs },
  };
}

/** Exchange lifecycle transitions resolve an open live stream: completed,
 *  failed, cancelled, and dropped are observable terminal stream states even
 *  when the SSE record flow never carried a protocol terminator. */
function observeStreamTerminus(state: WorkspaceState, event: ExchangeEvent): WorkspaceState {
  const stream = state.streams[event.exchange_id];
  if (!stream) return state;
  const kind = String(event.kind);
  const status = kind === "completed" ? "completed" : kind === "failed" ? "failed" : "cancelled";
  const next = applyStreamTerminus(stream, status, kind);
  if (next === stream) return state;
  return { ...state, streams: { ...state.streams, [event.exchange_id]: next } };
}

function upsertExchange(state: WorkspaceState, exchange: ExchangeSnapshot, revision: number): WorkspaceState {
  const found = state.exchanges.some((item) => item.exchange_id === exchange.exchange_id);
  const exchanges = found
    ? state.exchanges.map((item) => (item.exchange_id === exchange.exchange_id ? exchange : item))
    : [exchange, ...state.exchanges];
  return followLatestSession({
    ...state,
    exchanges,
    revisions: { ...state.revisions, [exchange.exchange_id]: revision },
    selectedExchangeId: state.selectedExchangeId ?? exchange.exchange_id,
  });
}

export function workspaceReducer(state: WorkspaceState, action: WorkspaceAction): WorkspaceState {
  switch (action.type) {
    case "load_started":
      return { ...state, loading: true, error: undefined };
    case "load_succeeded": {
      const byId = new Map(state.exchanges.map((item) => [item.exchange_id, item]));
      const revisions = { ...state.revisions };
      for (const raw of action.exchanges) {
        const exchange = normalizeSnapshot(raw);
        const incomingRevision = revisionValue(exchange);
        const currentRevision = revisions[exchange.exchange_id] ?? -1;
        if (!byId.has(exchange.exchange_id) || incomingRevision >= currentRevision) {
          byId.set(exchange.exchange_id, exchange);
          revisions[exchange.exchange_id] = incomingRevision;
        }
      }
      const exchanges = [...byId.values()];
      const next = {
        ...state,
        exchanges,
        revisions,
        selectedExchangeId: state.selectedExchangeId && exchanges.some((item) => item.exchange_id === state.selectedExchangeId)
          ? state.selectedExchangeId
          : exchanges[0]?.exchange_id,
        policy: action.policy,
        loading: false,
        error: undefined,
      };
      return followLatestSession(next);
    }
    case "page_loaded": {
      const byId = new Map(state.exchanges.map((item) => [item.exchange_id, item]));
      const revisions = { ...state.revisions };
      for (const raw of action.exchanges) {
        const exchange = normalizeSnapshot(raw);
        const incomingRevision = revisionValue(exchange);
        const currentRevision = revisions[exchange.exchange_id] ?? -1;
        // A delayed page response must never roll back a newer realtime event.
        if (!byId.has(exchange.exchange_id) || incomingRevision >= currentRevision) {
          byId.set(exchange.exchange_id, exchange);
          revisions[exchange.exchange_id] = incomingRevision;
        }
      }
      return followLatestSession({ ...state, exchanges: [...byId.values()], revisions, loading: false });
    }
    case "load_failed":
      return { ...state, loading: false, error: action.error };
    case "clear_error":
      return { ...state, error: undefined };
    case "select_exchange":
      return { ...state, selectedExchangeId: action.exchangeId, followSessionId: action.followSessionId, bodyLoading: false, bodyLoadErrorArtifactId: undefined, loadedBodies: {}, search: "" };
    case "set_tab":
      return { ...state, activeTab: action.tab };
    case "set_policy":
      return { ...state, policy: { ...state.policy, [action.gate]: action.value } };
    case "policy_saved":
      return { ...state, policy: action.policy };
    case "event_received": {
      const event = action.event;
      if (event.kind === "stream_event" && event.stream) {
        // A stream observation never commits a revision; it is deduplicated
        // by ordinal inside the live reducer so broker replay and
        // Last-Event-ID reconnects stay idempotent.
        const current = state.exchanges.find((item) => item.exchange_id === event.exchange_id);
        if (!current) return state; // exchange_created always precedes records
        const existing = state.streams[event.exchange_id] ?? initialLiveStream(current.protocol);
        const next = applyStreamRecord(existing, event.stream);
        if (next === existing) return state;
        return { ...state, streams: { ...state.streams, [event.exchange_id]: next } };
      }
      const current = state.exchanges.find((item) => item.exchange_id === event.exchange_id);
      if (!current) {
        // `exchange_created` events may carry a full snapshot as an additive
        // field.  A delta without a base is intentionally not fabricated.
        const fullSnapshot = (event as ExchangeEvent & { snapshot?: ExchangeSnapshot }).snapshot ?? snapshotFromEvent(event);
        if (!fullSnapshot || event.revision <= revisionOf(state, event.exchange_id)) return state;
        return upsertExchange(state, { ...normalizeSnapshot(fullSnapshot), revision: event.revision }, event.revision);
      }
      if (event.revision <= revisionOf(state, event.exchange_id)) return state;
      const merged = appendEventArtifacts(mergeSnapshot(current, event.snapshot_delta), event.artifact_refs);
      const upserted = upsertExchange(state, { ...merged, revision: event.revision }, event.revision);
      return observeStreamTerminus(upserted, event);
    }
    case "stream_events_received": {
      const grouped = new Map<string, ExchangeEvent[]>();
      for (const event of action.events) {
        if (event.kind !== "stream_event" || !event.stream) continue;
        const list = grouped.get(event.exchange_id) ?? [];
        list.push(event);
        grouped.set(event.exchange_id, list);
      }
      if (grouped.size === 0) return state;
      const streams = { ...state.streams };
      let changed = false;
      for (const [exchangeId, events] of grouped) {
        const current = state.exchanges.find((item) => item.exchange_id === exchangeId);
        if (!current) continue;
        const existing = streams[exchangeId] ?? initialLiveStream(current.protocol);
        const next = applyStreamRecords(existing, events.map((event) => event.stream!));
        if (next !== existing) { streams[exchangeId] = next; changed = true; }
      }
      return changed ? { ...state, streams } : state;
    }
    case "command_succeeded": {
      const result = action.result;
      const nextSnapshot = { ...result.exchange, revision: result.revision };
      const next = upsertExchange(state, nextSnapshot, result.revision);
      return next;
    }
    case "exchanges_cleared":
      return {
        ...state,
        exchanges: [],
        revisions: {},
        selectedExchangeId: undefined,
        followSessionId: undefined,
        bodyLoading: false,
        bodyLoadErrorArtifactId: undefined,
        loadedBodies: {},
        search: "",
        streams: {},
        error: undefined,
      };
    case "body_load_started":
      return { ...state, bodyLoading: true, bodyLoadErrorArtifactId: undefined, error: undefined };
    case "body_load_finished":
      return { ...state, bodyLoading: false };
    case "body_loaded":
      return { ...state, bodyLoading: false, bodyLoadErrorArtifactId: undefined, loadedBodies: boundedLoadedBodies(state.loadedBodies, action.body), error: undefined };
    case "body_load_failed":
      return { ...state, bodyLoading: false, bodyLoadErrorArtifactId: action.artifactId, error: action.error };
    case "set_search":
      return { ...state, search: action.value };
  }
}

export function selectedExchange(state: WorkspaceState): ExchangeSnapshot | undefined {
  return state.exchanges.find((exchange) => exchange.exchange_id === state.selectedExchangeId);
}
