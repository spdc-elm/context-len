import { useEffect, useMemo, useReducer, useState } from "react";
import type {
  ArtifactRef,
  GateMode,
  WorkspaceApi,
  WorkspaceCommand,
} from "./contracts";
import { createLocalWorkspaceApi } from "./workspaceApi";
import {
  initialWorkspaceState,
  selectedExchange,
  workspaceReducer,
  type DetailTab,
} from "./workspaceState";
import { TrafficQueue } from "./components/TrafficQueue";
import { ExchangeDetail, type CommandIntent } from "./components/ExchangeDetail";
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

export function App({ api }: AppProps) {
  // Do not default the prop parameter to the mock: production renders use the
  // local same-origin REST/WS client, while tests retain explicit injection.
  const runtimeApi = useMemo(() => api ?? createLocalWorkspaceApi(), [api]);
  const [state, dispatch] = useReducer(workspaceReducer, initialWorkspaceState);
  const [commandBusy, setCommandBusy] = useState(false);

  useEffect(() => {
    const controller = new AbortController();
    let cancelled = false;
    dispatch({ type: "load_started" });
    void Promise.all([runtimeApi.listExchanges(controller.signal), runtimeApi.getPolicy(controller.signal)]).then(([exchanges, policy]) => {
      if (!cancelled) dispatch({ type: "load_succeeded", exchanges, policy });
    }).catch((error: unknown) => {
      if (!cancelled && !controller.signal.aborted) dispatch({ type: "load_failed", error: error instanceof Error ? error.message : String(error) });
    });
    const unsubscribe = runtimeApi.subscribe((event) => {
      if (!cancelled) dispatch({ type: "event_received", event });
    });
    return () => {
      cancelled = true;
      controller.abort();
      unsubscribe();
    };
  }, [runtimeApi]);

  const exchange = selectedExchange(state);
  const loadedCount = Object.keys(state.loadedBodies).length;
  const heldCount = useMemo(() => state.exchanges.filter((item) => item.state === "request_held" || item.state === "response_held").length, [state.exchanges]);

  async function loadBody(artifact: ArtifactRef) {
    dispatch({ type: "body_load_started" });
    try {
      const body = await runtimeApi.readArtifact({ artifact_id: artifact.artifact_id, start: 0, end: 1 << 20 });
      dispatch({ type: "body_loaded", body: {
        artifactId: body.artifact_id,
        text: new TextDecoder().decode(body.bytes),
        start: body.start,
        end: body.end,
        totalSize: body.total_size,
        complete: body.complete,
      } });
    } catch (error: unknown) {
      dispatch({ type: "body_load_failed", error: error instanceof Error ? error.message : String(error) });
    }
  }

  async function downloadBody(artifact: ArtifactRef) {
    dispatch({ type: "body_load_started" });
    try {
      // A download always asks for the complete artifact, even when the viewer
      // only has a range loaded.  This keeps display truncation independent of
      // the bytes saved to disk.
      const body = await runtimeApi.readArtifact({ artifact_id: artifact.artifact_id, start: 0 });
      dispatch({ type: "body_loaded", body: {
        artifactId: body.artifact_id,
        text: new TextDecoder().decode(body.bytes),
        start: body.start,
        end: body.end,
        totalSize: body.total_size,
        complete: body.complete,
      } });
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
    } catch (error: unknown) {
      dispatch({ type: "body_load_failed", error: error instanceof Error ? error.message : String(error) });
    }
  }

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
    <div className="app-shell">
      <header className="topbar">
        <div className="brand-lockup"><div className="brand-mark">✦</div><div><div className="brand-name">context<span>-lens</span></div><div className="brand-subtitle">LLM request workbench</div></div></div>
        <div className="topbar-status"><span className="live-dot" /> <span>LOCAL API</span><span className="status-divider" /><span>{state.exchanges.length} exchanges</span><span className="status-divider" /><span>{heldCount} held</span></div>
      </header>
      <section className="policy-bar" aria-label="Intercept policy">
        <div className="policy-title"><span className="eyebrow">INTERCEPT POLICY</span><span className="policy-caption">New exchanges only</span></div>
        <label className="gate-toggle"><span>Request gate</span><button type="button" role="switch" aria-checked={state.policy.request_gate === "hold"} className={`switch ${state.policy.request_gate === "hold" ? "on" : ""}`} onClick={() => void changeGate("request_gate", state.policy.request_gate === "hold" ? "pass" : "hold")}><span /></button><strong>{state.policy.request_gate}</strong></label>
        <label className="gate-toggle"><span>Response gate</span><button type="button" role="switch" aria-checked={state.policy.response_gate === "hold"} className={`switch ${state.policy.response_gate === "hold" ? "on" : ""}`} onClick={() => void changeGate("response_gate", state.policy.response_gate === "hold" ? "pass" : "hold")}><span /></button><strong>{state.policy.response_gate}</strong></label>
        <div className="wire-note"><span className="wire-icon">⟷</span><span>wire authority: <strong>raw artifact</strong></span></div>
      </section>
      {state.error && <div className="error-banner" role="alert"><strong>Workspace error</strong> {state.error}<button type="button" onClick={() => dispatch({ type: "clear_error" })}>Dismiss</button></div>}
      <div className="workspace-grid">
        <TrafficQueue exchanges={state.exchanges} selectedExchangeId={state.selectedExchangeId} onSelect={(exchangeId) => dispatch({ type: "select_exchange", exchangeId })} />
        <ExchangeDetail
          exchange={exchange}
          activeTab={state.activeTab}
          onTabChange={(tab: DetailTab) => dispatch({ type: "set_tab", tab })}
          loadedBodies={state.loadedBodies}
          bodyLoading={state.bodyLoading}
          search={state.search}
          jsonPath={state.jsonPath}
          onSearchChange={(value) => dispatch({ type: "set_search", value })}
          onJsonPathChange={(value) => dispatch({ type: "set_json_path", value })}
          onLoadBody={(artifact) => void loadBody(artifact)}
          onDownloadBody={(artifact) => void downloadBody(artifact)}
          onCommand={(intent) => void runCommand(intent)}
          commandBusy={commandBusy}
          mutation={exchange ? state.mutationResults[exchange.exchange_id] : undefined}
        />
      </div>
      <footer className="footer-note"><span>Projection-only UI · local workspace API</span><span>{loadedCount} artifact{loadedCount === 1 ? "" : "s"} loaded locally</span></footer>
    </div>
  );
}
