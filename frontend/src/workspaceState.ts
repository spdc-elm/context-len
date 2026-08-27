import {
  type CommandResult,
  type ExchangeEvent,
  type ExchangeSnapshot,
  type GateMode,
  type InspectionProjection,
  type WorkspacePolicy,
} from "./contracts";

export type DetailTab = "raw" | "pretty" | "sse";

export interface LoadedArtifact {
  artifactId: string;
  text: string;
  start: number;
  end: number;
  totalSize: number;
  complete: boolean;
}

export interface WorkspaceState {
  exchanges: ExchangeSnapshot[];
  revisions: Record<string, number>;
  selectedExchangeId?: string;
  activeTab: DetailTab;
  policy: WorkspacePolicy;
  loading: boolean;
  error?: string;
  bodyLoading: boolean;
  loadedBodies: Record<string, LoadedArtifact>;
  search: string;
  jsonPath: string;
}

export type WorkspaceAction =
  | { type: "load_started" }
  | { type: "load_succeeded"; exchanges: ExchangeSnapshot[]; policy: WorkspacePolicy }
  | { type: "load_failed"; error: string }
  | { type: "clear_error" }
  | { type: "select_exchange"; exchangeId: string }
  | { type: "set_tab"; tab: DetailTab }
  | { type: "set_policy"; gate: "request_gate" | "response_gate"; value: GateMode }
  | { type: "policy_saved"; policy: WorkspacePolicy }
  | { type: "event_received"; event: ExchangeEvent }
  | { type: "command_succeeded"; result: CommandResult }
  | { type: "body_load_started" }
  | { type: "body_loaded"; body: LoadedArtifact }
  | { type: "body_load_failed"; error: string }
  | { type: "set_search"; value: string }
  | { type: "set_json_path"; value: string };

export const initialWorkspaceState: WorkspaceState = {
  exchanges: [],
  revisions: {},
  activeTab: "raw",
  policy: { request_gate: "pass", response_gate: "pass" },
  loading: true,
  bodyLoading: false,
  loadedBodies: {},
  search: "",
  jsonPath: "",
};

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

function upsertExchange(state: WorkspaceState, exchange: ExchangeSnapshot, revision: number): WorkspaceState {
  const found = state.exchanges.some((item) => item.exchange_id === exchange.exchange_id);
  const exchanges = found
    ? state.exchanges.map((item) => (item.exchange_id === exchange.exchange_id ? exchange : item))
    : [exchange, ...state.exchanges];
  return {
    ...state,
    exchanges,
    revisions: { ...state.revisions, [exchange.exchange_id]: revision },
    selectedExchangeId: state.selectedExchangeId ?? exchange.exchange_id,
  };
}

export function workspaceReducer(state: WorkspaceState, action: WorkspaceAction): WorkspaceState {
  switch (action.type) {
    case "load_started":
      return { ...state, loading: true, error: undefined };
    case "load_succeeded": {
      const exchanges = action.exchanges.map(normalizeSnapshot);
      const revisions = Object.fromEntries(exchanges.map((exchange) => [exchange.exchange_id, revisionValue(exchange)]));
      return {
        ...state,
        exchanges,
        revisions,
        selectedExchangeId: state.selectedExchangeId && action.exchanges.some((item) => item.exchange_id === state.selectedExchangeId)
          ? state.selectedExchangeId
          : exchanges[0]?.exchange_id,
        policy: action.policy,
        loading: false,
        error: undefined,
      };
    }
    case "load_failed":
      return { ...state, loading: false, error: action.error };
    case "clear_error":
      return { ...state, error: undefined };
    case "select_exchange":
      return { ...state, selectedExchangeId: action.exchangeId, loadedBodies: {}, search: "", jsonPath: "" };
    case "set_tab":
      return { ...state, activeTab: action.tab };
    case "set_policy":
      return { ...state, policy: { ...state.policy, [action.gate]: action.value } };
    case "policy_saved":
      return { ...state, policy: action.policy };
    case "event_received": {
      const event = action.event;
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
      return upsertExchange(state, { ...merged, revision: event.revision }, event.revision);
    }
    case "command_succeeded": {
      const result = action.result;
      const nextSnapshot = { ...result.exchange, revision: result.revision };
      const next = upsertExchange(state, nextSnapshot, result.revision);
      return next;
    }
    case "body_load_started":
      return { ...state, bodyLoading: true, error: undefined };
    case "body_loaded":
      return { ...state, bodyLoading: false, loadedBodies: { ...state.loadedBodies, [action.body.artifactId]: action.body }, error: undefined };
    case "body_load_failed":
      return { ...state, bodyLoading: false, error: action.error };
    case "set_search":
      return { ...state, search: action.value };
    case "set_json_path":
      return { ...state, jsonPath: action.value };
  }
}

export function selectedExchange(state: WorkspaceState): ExchangeSnapshot | undefined {
  return state.exchanges.find((exchange) => exchange.exchange_id === state.selectedExchangeId);
}

export function selectedProjection(snapshot: ExchangeSnapshot | undefined, tab: DetailTab): InspectionProjection | undefined {
  if (!snapshot) return undefined;
  if (tab === "sse") return snapshot.response.projection;
  return snapshot.request.projection ?? snapshot.response.projection;
}
