import { useCallback, useEffect, useMemo, useReducer, useRef, useState } from "react";
import type {
  ArtifactRef,
  CaptureMode,
  GateMode,
  StorageStats,
  WorkspaceApi,
  WorkspaceCommand,
} from "./contracts";
import { createLocalWorkspaceApi } from "./workspaceApi";
import { ArtifactLoader } from "./artifactLoader";
import {
  initialWorkspaceState,
  selectedExchange,
  workspaceReducer,
  type DetailTab,
} from "./workspaceState";
import { sessionLineage } from "./mergedSession";
import { TrafficQueue } from "./components/TrafficQueue";
import { ExchangeDetail, type CommandIntent } from "./components/ExchangeDetail";
import { WorkspaceTopbar } from "./components/WorkspaceTopbar";
import "./styles.css";

interface AppProps {
  /** Tests and embedded hosts can inject the deterministic mock or another API. */
  api?: WorkspaceApi;
}

function manualResponseFor(protocol: string): { raw_response: string; content_type: string } {
  if (protocol === "chat_completions") {
    return { content_type: "application/json", raw_response: JSON.stringify({ id: "manual_response", object: "chat.completion", choices: [{ index: 0, message: { role: "assistant", content: "Manual response" }, finish_reason: "stop" }] }, null, 2) };
  }
  if (protocol === "anthropic_messages") {
    return { content_type: "application/json", raw_response: JSON.stringify({ id: "manual_response", type: "message", role: "assistant", content: [{ type: "text", text: "Manual response" }], stop_reason: "end_turn", usage: { input_tokens: 0, output_tokens: 2 } }, null, 2) };
  }
  return { content_type: "application/json", raw_response: JSON.stringify({ id: "manual_response", object: "response", status: "completed", output: [{ type: "message", role: "assistant", content: [{ type: "output_text", text: "Manual response" }] }] }, null, 2) };
}

function makeCommand(intent: CommandIntent, exchangeId: string, baseRevision: number, protocol: string): WorkspaceCommand {
  const base = { exchange_id: exchangeId, base_revision: baseRevision };
  switch (intent.kind) {
    case "forward_edited":
    case "release_edited":
      return { ...base, kind: intent.kind, mutation: intent.mutation ?? { raw_replacement: "" } };
    case "manual_response":
      return { ...base, kind: intent.kind, raw_response: intent.raw_response ?? manualResponseFor(protocol).raw_response, content_type: intent.content_type ?? manualResponseFor(protocol).content_type };
    case "replace_response":
      return { ...base, kind: intent.kind, raw_response: intent.raw_response ?? "", content_type: intent.content_type };
    case "drop":
    case "abort":
      return { ...base, kind: intent.kind, reason: intent.reason ?? "operator action from workbench" };
    case "forward_unchanged":
    case "release_unchanged":
      return { ...base, kind: intent.kind };
  }
}

function downloadName(artifact: ArtifactRef): string {
  const safe = artifact.artifact_id.replace(/[^a-zA-Z0-9._-]+/g, "-").slice(0, 100) || "artifact";
  return `${safe}.bin`;
}

function initialTheme(): "light" | "dark" {
  try {
    const stored = window.localStorage.getItem("context-lens-theme");
    return stored === "dark" ? "dark" : "light";
  } catch {
    return "light";
  }
}

const PREVIEW_RANGE_BYTES = 1 << 20;

export function App({ api }: AppProps) {
  // Do not default the prop parameter to the mock: production renders use the
  // local same-origin REST/WS client, while tests retain explicit injection.
  const runtimeApi = useMemo(() => api ?? createLocalWorkspaceApi(), [api]);
  const [state, dispatch] = useReducer(workspaceReducer, initialWorkspaceState);
  const [commandBusy, setCommandBusy] = useState(false);
  const [clearBusy, setClearBusy] = useState(false);
  const [captureMode, setCaptureMode] = useState<CaptureMode>("passthrough");
  const [captureBusy, setCaptureBusy] = useState(false);
  const [storage, setStorage] = useState<StorageStats | undefined>(undefined);
  const [sidebarCollapsed, setSidebarCollapsed] = useState(false);
  const [theme, setTheme] = useState<"light" | "dark">(initialTheme);
  const loader = useMemo(() => new ArtifactLoader(runtimeApi), [runtimeApi]);
  const [pageCursor, setPageCursor] = useState<string | undefined>(undefined);
  const [hasMoreExchanges, setHasMoreExchanges] = useState(false);
  const [pageLoading, setPageLoading] = useState(false);
  // Stream observations are high frequency. Keep transport callbacks cheap and
  // commit at most one reducer update per animation frame. Every record remains
  // in the batch; the reducer owns ordinal deduplication/gap handling.
  const pendingStreamEvents = useRef<import("./contracts").ExchangeEvent[]>([]);
  const streamFlushHandle = useRef<number | undefined>(undefined);

  useEffect(() => {
    try {
      window.localStorage.setItem("context-lens-theme", theme);
    } catch {
      // Persistence is a convenience; private browsing and embedded hosts may
      // deliberately disable local storage.
    }
  }, [theme]);

  useEffect(() => {
    const controller = new AbortController();
    let cancelled = false;
    dispatch({ type: "load_started" });
    const loadPage = runtimeApi.listExchangesPage
      ? runtimeApi.listExchangesPage(50, undefined, controller.signal).then((page) => {
        setPageCursor(page.next_cursor); setHasMoreExchanges(page.has_more ?? Boolean(page.next_cursor)); return page.exchanges;
      })
      : runtimeApi.listExchanges(controller.signal).then((items) => { setHasMoreExchanges(false); return items; });
    if (runtimeApi.getCaptureMode || runtimeApi.getStorageStats) {
      Promise.all([runtimeApi.getCaptureMode?.(controller.signal), runtimeApi.getStorageStats?.(controller.signal)]).then(([mode, stats]) => { if (!cancelled) { if (mode) setCaptureMode(mode); if (stats) setStorage(stats); } }).catch(() => undefined);
    }
    void Promise.all([loadPage, runtimeApi.getPolicy(controller.signal)]).then(([exchanges, policy]) => {
      if (!cancelled) dispatch({ type: "load_succeeded", exchanges, policy });
    }).catch((error: unknown) => {
      if (!cancelled && !controller.signal.aborted) dispatch({ type: "load_failed", error: error instanceof Error ? error.message : String(error) });
    });
    let storageRefreshTimer: number | undefined;
    const scheduleStorageRefresh = () => {
      if (!runtimeApi.getStorageStats || storageRefreshTimer !== undefined) return;
      storageRefreshTimer = window.setTimeout(() => {
        storageRefreshTimer = undefined;
        void runtimeApi.getStorageStats?.().then((next) => { if (!cancelled) setStorage(next); }).catch(() => undefined);
      }, 100);
    };
    const flushStreams = () => {
      streamFlushHandle.current = undefined;
      const pending = pendingStreamEvents.current;
      pendingStreamEvents.current = [];
      if (pending.length) dispatch({ type: "stream_events_received", events: pending });
    };
    const scheduleStreamFlush = () => {
      if (streamFlushHandle.current !== undefined) return;
      if (typeof window !== "undefined" && typeof window.requestAnimationFrame === "function") {
        streamFlushHandle.current = window.requestAnimationFrame(flushStreams);
      } else {
        streamFlushHandle.current = window.setTimeout(flushStreams, 16);
      }
    };
    const storageTimer = runtimeApi.getStorageStats ? window.setInterval(() => { void runtimeApi.getStorageStats!().then(setStorage).catch(() => undefined); }, 30000) : undefined;
    const unsubscribe = runtimeApi.subscribe((event) => {
      if (cancelled) return;
      if (event.kind === "stream_event" && event.stream) {
        pendingStreamEvents.current.push(event);
        scheduleStreamFlush();
      } else {
        dispatch({ type: "event_received", event });
        if (event.kind !== "stream_event") scheduleStorageRefresh();
      }
    });
    return () => {
      cancelled = true;
      controller.abort();
      if (storageTimer !== undefined) window.clearInterval(storageTimer);
      if (storageRefreshTimer !== undefined) window.clearTimeout(storageRefreshTimer);
      unsubscribe();
      if (streamFlushHandle.current !== undefined) {
        if (typeof window !== "undefined" && typeof window.cancelAnimationFrame === "function") window.cancelAnimationFrame(streamFlushHandle.current);
        else window.clearTimeout(streamFlushHandle.current);
      }
      pendingStreamEvents.current = [];
    };
  }, [runtimeApi]);

  const loadMoreExchanges = useCallback(async () => {
    if (!runtimeApi.listExchangesPage || pageLoading || !hasMoreExchanges) return;
    setPageLoading(true);
    try {
      const page = await runtimeApi.listExchangesPage(50, pageCursor);
      setPageCursor(page.next_cursor);
      setHasMoreExchanges(page.has_more ?? Boolean(page.next_cursor));
      dispatch({ type: "page_loaded", exchanges: page.exchanges });
    } catch (error: unknown) {
      dispatch({ type: "load_failed", error: error instanceof Error ? error.message : String(error) });
    } finally { setPageLoading(false); }
  }, [runtimeApi, pageLoading, hasMoreExchanges, pageCursor]);

  const exchange = selectedExchange(state);
  const lineage = useMemo(() => sessionLineage(state.exchanges, state.selectedExchangeId), [state.exchanges, state.selectedExchangeId]);
  const heldCount = useMemo(() => state.exchanges.filter((item) => item.state === "request_held" || item.state === "response_held").length, [state.exchanges]);

  const loadBody = useCallback(async (artifact: ArtifactRef, range?: { start: number; end?: number }) => {
    dispatch({ type: "body_load_started", artifactId: artifact.artifact_id });
    try {
      const start = range?.start ?? 0;
      const end = range?.end ?? (artifact.size > PREVIEW_RANGE_BYTES ? start + PREVIEW_RANGE_BYTES : undefined);
      const body = await loader.load(artifact, { artifact_id: artifact.artifact_id, start, ...(end !== undefined ? { end } : {}) });
      dispatch({ type: "body_loaded", body: {
        artifactId: body.artifact_id,
        text: new TextDecoder().decode(body.bytes),
        byteLength: body.bytes.byteLength,
        start: body.start,
        end: body.end,
        totalSize: body.total_size,
        complete: body.complete,
      } });
    } catch (error: unknown) {
      if (error instanceof DOMException && error.name === "AbortError") return;
      dispatch({ type: "body_load_failed", artifactId: artifact.artifact_id, error: error instanceof Error ? error.message : String(error) });
    }
  }, [loader]);

  const downloadBody = useCallback(async (artifact: ArtifactRef) => {
    dispatch({ type: "body_load_started", artifactId: artifact.artifact_id });
    try {
      // A download always asks for the complete artifact, even when the viewer
      // only has a range loaded.  This keeps display truncation independent of
      // the bytes saved to disk.
      const body = await loader.load(artifact, { artifact_id: artifact.artifact_id, start: 0 }, undefined, false);
      const blob = new Blob([new Uint8Array(body.bytes)], { type: body.content_type || artifact.content_type || "application/octet-stream" });
      if (typeof URL.createObjectURL !== "function") throw new Error("browser does not support artifact downloads");
      const objectUrl = URL.createObjectURL(blob);
      const anchor = document.createElement("a");
      anchor.href = objectUrl;
      anchor.download = downloadName(artifact);
      anchor.rel = "noopener";
      anchor.click();
      // Let the navigation consume the blob before revoking it.
      window.setTimeout(() => URL.revokeObjectURL(objectUrl), 0);
      dispatch({ type: "body_load_finished" });
    } catch (error: unknown) {
      if (error instanceof DOMException && error.name === "AbortError") return;
      dispatch({ type: "body_load_failed", artifactId: artifact.artifact_id, error: error instanceof Error ? error.message : String(error) });
    }
  }, [loader]);

  const handleSelectExchange = useCallback((exchangeId: string, followSessionId?: string) => {
    loader.beginGeneration();
    dispatch({ type: "select_exchange", exchangeId, followSessionId });
  }, [loader]);

  async function runCommand(intent: CommandIntent) {
    if (!exchange || commandBusy) return;
    setCommandBusy(true);
    try {
      const revision = state.revisions[exchange.exchange_id] ?? exchange.revision ?? 0;
      const result = await runtimeApi.command(makeCommand(intent, exchange.exchange_id, revision, exchange.protocol));
      // Command responses are authoritative even when a WS server is not
      // available.  A duplicated event is safely ignored by revision CAS.
      dispatch({ type: "command_succeeded", result });
      if (result.event) dispatch({ type: "event_received", event: result.event });
    } catch (error: unknown) {
      dispatch({ type: "load_failed", error: error instanceof Error ? error.message : String(error) });
    } finally {
      setCommandBusy(false);
    }
  }

  async function toggleCapture() {
    if (!runtimeApi.setCaptureMode || captureBusy) return;
    const next = captureMode === "capture" ? "passthrough" : "capture";
    if (next === "passthrough" && (state.policy.request_gate === "hold" || state.policy.response_gate === "hold")) return;
    setCaptureBusy(true);
    try {
      setCaptureMode(await runtimeApi.setCaptureMode(next));
      dispatch({ type: "clear_error" });
    } catch (error: unknown) { dispatch({ type: "load_failed", error: error instanceof Error ? error.message : String(error) }); }
    finally { setCaptureBusy(false); }
  }

  async function deleteSession(sessionId: string) {
    if (!runtimeApi.deleteSession || typeof window !== "undefined" && !window.confirm("Delete this entire session?")) return;
    try { await runtimeApi.deleteSession(sessionId); loader.beginGeneration(); dispatch({ type: "session_deleted", sessionId }); if (runtimeApi.getStorageStats) void runtimeApi.getStorageStats().then(setStorage).catch(() => undefined); }
    catch (error: unknown) { dispatch({ type: "load_failed", error: error instanceof Error ? error.message : String(error) }); }
  }

  async function clearExchanges() {
    if (clearBusy || state.exchanges.length === 0) return;
    if (typeof window !== "undefined" && !window.confirm("Clear all exchange records and captured artifacts?")) return;
    setClearBusy(true);
    try {
      await runtimeApi.clearExchanges();
      if (runtimeApi.getStorageStats) void runtimeApi.getStorageStats().then(setStorage).catch(() => undefined);
      loader.clear();
      setPageCursor(undefined);
      setHasMoreExchanges(false);
      dispatch({ type: "exchanges_cleared" });
    } catch (error: unknown) {
      dispatch({ type: "load_failed", error: error instanceof Error ? error.message : String(error) });
    } finally {
      setClearBusy(false);
    }
  }

  async function changeGate(gate: "request_gate" | "response_gate", value: GateMode) {
    const previous = state.policy;
    dispatch({ type: "set_policy", gate, value });
    try {
      const policy = await runtimeApi.setPolicy({ ...previous, [gate]: value });
      dispatch({ type: "policy_saved", policy });
    } catch (error: unknown) {
      dispatch({ type: "policy_saved", policy: previous });
      dispatch({ type: "load_failed", error: error instanceof Error ? error.message : String(error) });
    }
  }

  return (
    <div className={`app-shell theme-${theme}`}>
      <WorkspaceTopbar
        policy={state.policy}
        exchangeCount={state.exchanges.length}
        heldCount={heldCount}
        theme={theme}
        onGateChange={(gate, value) => void changeGate(gate, value)}
        captureMode={captureMode}
        captureBusy={captureBusy}
        captureAvailable={Boolean(runtimeApi.setCaptureMode)}
        storage={storage}
        onCaptureToggle={() => void toggleCapture()}
        onThemeToggle={() => setTheme((current) => current === "dark" ? "light" : "dark")}
      />
      {state.error && <div className="error-banner" role="alert"><strong>Workspace error</strong> {state.error}<button type="button" onClick={() => dispatch({ type: "clear_error" })}>Dismiss</button></div>}
      <div className={`workspace-grid ${sidebarCollapsed ? "sidebar-collapsed" : ""}`}>
        <TrafficQueue
          exchanges={state.exchanges}
          hasMore={hasMoreExchanges}
          loadingMore={pageLoading}
          onLoadMore={() => void loadMoreExchanges()}
          selectedExchangeId={state.selectedExchangeId}
          collapsed={sidebarCollapsed}
          onToggle={() => setSidebarCollapsed((collapsed) => !collapsed)}
          onClear={() => void clearExchanges()}
          clearBusy={clearBusy}
          onSelect={handleSelectExchange}
          onDeleteSession={(sessionId) => void deleteSession(sessionId)}
        />
        <ExchangeDetail
          exchange={exchange}
          liveStream={exchange ? state.streams[exchange.exchange_id] : undefined}
          sessionLineage={lineage}
          activeTab={state.activeTab}
          onTabChange={(tab: DetailTab) => dispatch({ type: "set_tab", tab })}
          loadedBodies={state.loadedBodies}
          bodyLoading={state.bodyLoading}
          bodyLoadErrorArtifactId={state.bodyLoadErrorArtifactId}
          search={state.search}
          onSearchChange={(value) => dispatch({ type: "set_search", value })}
          onLoadBody={(artifact, range) => void loadBody(artifact, range)}
          onRetryBody={(artifact, range) => void loadBody(artifact, range)}
          onDownloadBody={(artifact) => void downloadBody(artifact)}
          onCommand={(intent) => void runCommand(intent)}
          commandBusy={commandBusy}
        />
      </div>
    </div>
  );
}
