import { useEffect, useState } from "react";
import type {
  ArtifactRef,
  ExchangeSnapshot,
  InspectionProjection,
  MutationInput,
  Protocol,
} from "../contracts";
import { formatWorkspaceTime } from "../time";
import type { DetailTab, LoadedArtifact } from "../workspaceState";
import RawJsonTree from "./RawJsonTree";
import ChatTemplateView from "./ChatTemplateView";

export type OperatorAction =
  | "forward_unchanged"
  | "forward_edited"
  | "manual_response"
  | "release_unchanged"
  | "release_edited"
  | "replace_response"
  | "drop"
  | "abort";

export interface CommandIntent {
  kind: OperatorAction;
  mutation?: MutationInput;
  raw_response?: string;
  content_type?: string;
  reason?: string;
}

interface ExchangeDetailProps {
  exchange?: ExchangeSnapshot;
  activeTab: DetailTab;
  onTabChange: (tab: DetailTab) => void;
  loadedBodies: Record<string, LoadedArtifact>;
  bodyLoading: boolean;
  search: string;
  onSearchChange: (value: string) => void;
  onLoadBody: (artifact: ArtifactRef) => void;
  onDownloadBody: (artifact: ArtifactRef) => void;
  onCommand: (intent: CommandIntent) => void;
  commandBusy: boolean;
}

const tabs: Array<{ id: DetailTab; label: string; description: string }> = [
  { id: "raw", label: "Raw", description: "Opaque artifact bytes" },
  { id: "chat_template", label: "Chat Template", description: "Qwen ChatML" },
  { id: "sse", label: "SSE", description: "Event projection" },
];

function protocolLabel(protocol: ExchangeSnapshot["protocol"]): string {
  const labels: Record<string, string> = {
    responses: "Responses",
    chat_completions: "Chat Completions",
    anthropic_messages: "Anthropic Messages",
  };
  return labels[String(protocol)] ?? String(protocol);
}

function stateDescription(snapshot: ExchangeSnapshot): string {
  if (snapshot.state === "request_held") return "Request is held. No upstream call has been made.";
  if (snapshot.state === "response_held") return "Response is held. Release or edit it explicitly.";
  if (snapshot.state === "completed") return "Exchange completed. Original artifacts remain immutable.";
  if (snapshot.state === "dropped") return "Exchange dropped by operator.";
  if (snapshot.state === "cancelled") return "Exchange cancelled.";
  if (snapshot.state === "failed") return snapshot.error ? `Upstream failed: ${snapshot.error}` : "Upstream failed; inspect the response artifact.";
  return `Exchange is ${snapshot.state.replaceAll("_", " ")}.`;
}

function allArtifacts(snapshot: ExchangeSnapshot): ArtifactRef[] {
  return [...(snapshot.request.artifact_refs ?? []), ...(snapshot.response.artifact_refs ?? [])];
}

function actionArtifactFor(snapshot: ExchangeSnapshot, mode: CommandIntent["kind"]): ArtifactRef | undefined {
  const requestRefs = snapshot.request.artifact_refs ?? [];
  const responseRefs = snapshot.response.artifact_refs ?? [];
  if (mode === "forward_edited") return requestRefs.find((artifact) => artifact.stage === "request.inbound") ?? requestRefs[requestRefs.length - 1];
  if (mode === "release_edited" || mode === "replace_response" || mode === "manual_response") return responseRefs.find((artifact) => artifact.stage === "response.upstream") ?? responseRefs[responseRefs.length - 1];
  return undefined;
}

function artifactForTab(snapshot: ExchangeSnapshot | undefined, tab: DetailTab, selectedArtifactId?: string): ArtifactRef | undefined {
  if (!snapshot) return undefined;
  const refs = allArtifacts(snapshot);
  if (selectedArtifactId) {
    const selected = refs.find((artifact) => artifact.artifact_id === selectedArtifactId);
    if (selected) return selected;
  }
  const responseRefs = snapshot.response.artifact_refs ?? [];
  const requestRefs = snapshot.request.artifact_refs ?? [];
  if (tab === "sse") return responseRefs.find((artifact) => artifact.content_type.includes("event-stream")) ?? responseRefs[0];
  if (tab === "raw" || tab === "chat_template") return requestRefs[0] ?? responseRefs[0];
  return responseRefs[0] ?? requestRefs[0];
}

function bodyFor(artifact: ArtifactRef | undefined, loadedBodies: Record<string, LoadedArtifact>): string | undefined {
  if (!artifact) return undefined;
  return loadedBodies[artifact.artifact_id]?.text;
}

function parseSse(body: string): NonNullable<InspectionProjection["stream_events"]> {
  const events: NonNullable<InspectionProjection["stream_events"]> = [];
  let eventName: string | undefined;
  let eventId: string | undefined;
  let retry: number | undefined;
  let data: string[] = [];
  const flush = () => {
    if (data.length === 0 && !eventName && !eventId && retry === undefined) return;
    events.push({ event: eventName, id: eventId, retry, data: data.join("\n"), sequence: events.length });
    eventName = undefined;
    eventId = undefined;
    retry = undefined;
    data = [];
  };
  for (const line of body.split(/\r?\n/)) {
    if (line === "") {
      flush();
      continue;
    }
    if (line.startsWith(":")) continue;
    const separator = line.indexOf(":");
    const field = separator < 0 ? line : line.slice(0, separator);
    const value = separator < 0 ? "" : line.slice(separator + 1).replace(/^ /, "");
    if (field === "event") eventName = value;
    else if (field === "id") eventId = value;
    else if (field === "retry") {
      const parsed = Number(value);
      if (Number.isFinite(parsed)) retry = parsed;
    } else if (field === "data") data.push(value);
  }
  flush();
  return events;
}

function projectionFromBody(body: string, artifact: ArtifactRef | undefined, protocol: string): InspectionProjection {
  const contentType = artifact?.content_type ?? "";
  if (contentType.includes("event-stream") || /^event:/m.test(body) || /^data:/m.test(body)) {
    return {
      protocol_hint: "sse",
      parse_status: "parsed",
      stream_events: parseSse(body),
      warnings: [],
    };
  }
  try {
    const parsed = JSON.parse(body) as unknown;
    const projection: InspectionProjection = {
      protocol_hint: protocol,
      parse_status: "parsed",
      sections: [],
      warnings: [],
    };
    if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
      projection.sections = Object.entries(parsed).map(([key, value]) => ({ id: key, label: key, value, json_pointer: `/${key.replaceAll("~", "~0").replaceAll("/", "~1")}` }));
    }
    const record = parsed && typeof parsed === "object" && !Array.isArray(parsed) ? parsed as Record<string, unknown> : undefined;
    if (protocol === "responses") {
      projection.input_items = Array.isArray(record?.input) ? record.input : undefined;
      projection.response_items = Array.isArray(record?.output) ? record.output : undefined;
      projection.tools = Array.isArray(record?.tools) ? record.tools : undefined;
    } else if (protocol === "chat_completions") {
      projection.messages = Array.isArray(record?.messages) ? record.messages : Array.isArray(record?.choices) ? record.choices : undefined;
      projection.tools = Array.isArray(record?.tools) ? record.tools : undefined;
    } else if (protocol === "anthropic_messages") {
      projection.messages = Array.isArray(record?.messages) ? record.messages : undefined;
      projection.content_blocks = Array.isArray(record?.content) ? record.content : undefined;
      projection.tools = Array.isArray(record?.tools) ? record.tools : undefined;
    }
    return projection;
  } catch (error: unknown) {
    return {
      protocol_hint: protocol,
      parse_status: "warning",
      warnings: [`Client projection could not parse this body: ${error instanceof Error ? error.message : String(error)}`],
    };
  }
}

function ProjectionBody({ exchange, tab, body, artifact }: { exchange: ExchangeSnapshot; tab: DetailTab; body?: string; artifact?: ArtifactRef }) {
  const stored = tab === "sse" ? exchange.response.projection : exchange.request.projection ?? exchange.response.projection;
  const projection = body !== undefined && (!stored || stored.parse_status === "not_attempted" || (tab === "sse" && !stored.stream_events?.length))
    ? projectionFromBody(body, artifact, String(exchange.protocol))
    : stored;
  if (!projection) return <div className="body-placeholder">Load an artifact to calculate a display-only projection.</div>;
  const stream = projection.stream_events;
  return (
    <div className="projection-view">
      <div className="projection-status"><span className={`status-dot status-${projection.parse_status}`} /> parser: <strong>{projection.parse_status}</strong>{projection.protocol_hint && <span> · {projection.protocol_hint}</span>}</div>
      {projection.warnings.length > 0 && <div className="warning-box">{projection.warnings.map((warning, index) => <p key={`${warning}-${index}`}>{warning}</p>)}</div>}
      {tab === "sse" && stream && stream.length > 0 ? (
        <ol className="event-list">{stream.map((event, index) => <li key={`${event.event ?? "data"}-${event.id ?? index}`}><code>{event.event ?? "data"}</code>{event.id && <small> id={event.id}</small>}<pre>{event.data}</pre></li>)}</ol>
      ) : (
        <pre className="code-view">{JSON.stringify({
          sections: projection.sections,
          messages: projection.messages,
          input_items: projection.input_items,
          content_blocks: projection.content_blocks,
          tools: projection.tools,
          response_items: projection.response_items,
          unknown_nodes: projection.unknown_nodes,
        }, null, 2)}</pre>
      )}
    </div>
  );
}

function ArtifactPicker({ exchange, activeTab, selectedArtifactId, loadedBodies, bodyLoading, onDownloadBody, onArtifactSelect }: Pick<ExchangeDetailProps, "exchange" | "activeTab" | "loadedBodies" | "bodyLoading" | "onDownloadBody"> & { selectedArtifactId?: string; onArtifactSelect: (artifactId: string) => void }) {
  if (!exchange) return null;
  const refs = allArtifacts(exchange);
  const preferred = artifactForTab(exchange, activeTab, selectedArtifactId);
  return (
    <div className="artifact-picker">
      <span className="picker-label">Artifact</span>
      <select name="artifact" aria-label="Artifact" value={preferred?.artifact_id ?? ""} onChange={(event) => {
        const ref = refs.find((artifact) => artifact.artifact_id === event.target.value);
        if (ref) {
          onArtifactSelect(ref.artifact_id);
        }
      }}>
        {refs.length === 0 && <option value="">No artifacts</option>}
        {refs.map((artifact) => <option value={artifact.artifact_id} key={artifact.artifact_id}>{artifact.stage} · {artifact.size.toLocaleString()} B{artifact.complete ? "" : " · incomplete"}</option>)}
      </select>
      {preferred && <>
        <span className="artifact-load-status">{bodyLoading ? "Loading…" : loadedBodies[preferred.artifact_id] ? "Loaded" : "Waiting…"}</span>
        <button type="button" className="button quiet" onClick={() => onDownloadBody(preferred)} disabled={bodyLoading} aria-label={`Download ${preferred.artifact_id}`}>Download</button>
      </>}
    </div>
  );
}

function manualResponseFor(protocol: Protocol | string): string {
  if (protocol === "chat_completions") return JSON.stringify({ id: "manual_response", object: "chat.completion", choices: [{ index: 0, message: { role: "assistant", content: "Manual response" }, finish_reason: "stop" }] }, null, 2);
  if (protocol === "anthropic_messages") return JSON.stringify({ id: "manual_response", type: "message", role: "assistant", content: [{ type: "text", text: "Manual response" }], stop_reason: "end_turn", usage: { input_tokens: 0, output_tokens: 2 } }, null, 2);
  return JSON.stringify({ id: "manual_response", object: "response", status: "completed", output: [{ type: "message", role: "assistant", content: [{ type: "output_text", text: "Manual response" }] }] }, null, 2);
}

function editorTitle(mode: CommandIntent["kind"]): string {
  if (mode === "forward_edited") return "Edit request before forwarding";
  if (mode === "release_edited") return "Edit response before releasing";
  if (mode === "replace_response") return "Replace held response";
  return "Author manual protocol response";
}

function editorSubmitLabel(mode: CommandIntent["kind"]): string {
  if (mode === "forward_edited") return "Forward edited";
  if (mode === "release_edited") return "Release edited";
  if (mode === "replace_response") return "Replace response";
  return "Send manual response";
}

function MutationEditor({ mode, value, onChange, onSubmit, onCancel, busy }: { mode: CommandIntent["kind"]; value: string; onChange: (value: string) => void; onSubmit: () => void; onCancel: () => void; busy: boolean }) {
  return (
    <section className="mutation-editor" aria-label="Explicit artifact edit">
      <div className="editor-heading"><div><p className="eyebrow">EXPLICIT MUTATION</p><h3>{editorTitle(mode)}</h3></div><span className="muted">Original artifact remains immutable</span></div>
      <textarea aria-label="Edited artifact body" value={value} onChange={(event) => onChange(event.target.value)} spellCheck={false} placeholder="Enter the complete raw protocol body" />
      <div className="editor-actions"><span className="muted">The backend validates protocol shape and binds this edit to the base hash.</span><div className="action-group"><button type="button" className="button quiet" onClick={onCancel} disabled={busy}>Cancel</button><button type="button" className="button primary" onClick={onSubmit} disabled={busy || value.length === 0}>{busy ? "Submitting…" : editorSubmitLabel(mode)}</button></div></div>
    </section>
  );
}

export function ExchangeDetail({
  exchange,
  activeTab,
  onTabChange,
  loadedBodies,
  bodyLoading,
  search,
  onSearchChange,
  onLoadBody,
  onDownloadBody,
  onCommand,
  commandBusy,
}: ExchangeDetailProps) {
  const [selectedArtifactId, setSelectedArtifactId] = useState<string>();
  const [editorMode, setEditorMode] = useState<CommandIntent["kind"]>();
  const [editorText, setEditorText] = useState("");
  const [editorDirty, setEditorDirty] = useState(false);

  useEffect(() => {
    setSelectedArtifactId(undefined);
    setEditorMode(undefined);
    setEditorText("");
    setEditorDirty(false);
  }, [exchange?.exchange_id]);

  const artifact = artifactForTab(exchange, activeTab, selectedArtifactId);
  const body = bodyFor(artifact, loadedBodies);

  useEffect(() => {
    if (artifact && !loadedBodies[artifact.artifact_id] && !bodyLoading) onLoadBody(artifact);
  }, [artifact?.artifact_id, bodyLoading, loadedBodies, onLoadBody]);

  const editorArtifact = exchange && editorMode ? actionArtifactFor(exchange, editorMode) : undefined;
  const editorBody = bodyFor(editorArtifact, loadedBodies);

  useEffect(() => {
    if (!editorMode || editorDirty) return;
    if (editorMode === "manual_response" || editorMode === "replace_response") {
      if (!editorText) setEditorText(manualResponseFor(exchange?.protocol ?? "responses"));
    } else if (editorBody !== undefined) {
      setEditorText(editorBody);
    }
  }, [editorBody, editorDirty, editorMode, editorText, exchange?.protocol]);

  if (!exchange) {
    return <main className="detail-panel empty-detail"><div className="empty-detail-icon">⌁</div><h2>Choose an exchange</h2><p>Traffic captured by the proxy will appear here. Select a row to inspect its immutable artifacts.</p></main>;
  }

  const canRequestAction = exchange.state === "request_held";
  const canResponseAction = exchange.state === "response_held";
  const openEditor = (mode: CommandIntent["kind"]) => {
    setEditorMode(mode);
    setEditorDirty(false);
    if (mode === "manual_response" || mode === "replace_response") setEditorText(manualResponseFor(exchange.protocol));
    else {
      const source = actionArtifactFor(exchange, mode);
      setEditorText(bodyFor(source, loadedBodies) ?? "");
      if (source && !loadedBodies[source.artifact_id]) onLoadBody(source);
    }
  };
  const submitEditor = () => {
    if (!editorMode || !editorText) return;
    if (editorMode === "forward_edited" || editorMode === "release_edited") {
      const source = actionArtifactFor(exchange, editorMode);
      onCommand({ kind: editorMode, mutation: { raw_replacement: editorText, base_artifact_id: source?.artifact_id, base_sha256: source?.sha256 } });
    } else {
      const source = actionArtifactFor(exchange, editorMode) ?? artifact;
      onCommand({ kind: editorMode, raw_response: editorText, content_type: source?.content_type ?? "application/json" });
    }
    setEditorMode(undefined);
  };

  return (
    <main className="detail-panel" aria-label="Exchange detail">
      <header className="detail-header">
        <div>
          <div className="detail-title-line"><span className={`protocol-dot protocol-${exchange.protocol}`} /><p className="eyebrow">{protocolLabel(exchange.protocol)}</p><span className={`state-pill state-${exchange.state}`}>{exchange.state.replaceAll("_", " ")}</span></div>
          <h1>{exchange.request.envelope.method} <span>{exchange.request.envelope.path}</span></h1>
          <p className="detail-subtitle">{stateDescription(exchange)}</p>
        </div>
        <div className="exchange-id"><span>EXCHANGE</span><code>{exchange.exchange_id}</code>{typeof exchange.revision === "number" && <span>REVISION {exchange.revision}</span>}</div>
      </header>

      <section className="metadata-strip" aria-label="Exchange metadata">
        <div><span>status</span><strong>{exchange.response.envelope.status || "—"}</strong></div>
        <div><span>request gate</span><strong>{exchange.policy.request_gate}</strong></div>
        <div><span>response gate</span><strong>{exchange.policy.response_gate}</strong></div>
        <div><span>updated</span><strong>{formatWorkspaceTime(exchange.updated_at)} UTC+8</strong></div>
      </section>

      <section className="viewer-card">
        <div className="viewer-toolbar">
          <div className="tabs" role="tablist" aria-label="Artifact views">
            {tabs.map((tab) => <button type="button" role="tab" aria-selected={activeTab === tab.id} className={`tab ${activeTab === tab.id ? "active" : ""}`} key={tab.id} onClick={() => onTabChange(tab.id)}>{tab.label}<small>{tab.description}</small></button>)}
          </div>
          <ArtifactPicker exchange={exchange} activeTab={activeTab} selectedArtifactId={selectedArtifactId} loadedBodies={loadedBodies} bodyLoading={bodyLoading} onDownloadBody={onDownloadBody} onArtifactSelect={setSelectedArtifactId} />
        </div>
        {(activeTab === "raw" || activeTab === "chat_template") && <div className="search-toolbar"><label>Search <input name="search" value={search} onChange={(event) => onSearchChange(event.target.value)} placeholder="Find in body" /></label>{artifact && <span className="hash-chip">sha256 {artifact.sha256.slice(0, 16)}…</span>}</div>}
        <div className="viewer-body">
          {activeTab === "raw" && <RawJsonTree rawBody={body} search={search} onSearchChange={onSearchChange} showControls={false} ariaLabel="Raw artifact JSON tree" />}
          {activeTab === "chat_template" && <ChatTemplateView protocol={String(exchange.protocol)} body={body} artifact={artifact} />}
          {activeTab === "sse" && <ProjectionBody exchange={exchange} tab={activeTab} body={body} artifact={artifact} />}
        </div>
      </section>

      {editorMode && <MutationEditor mode={editorMode} value={editorText} onChange={(value) => { setEditorText(value); setEditorDirty(true); }} onSubmit={submitEditor} onCancel={() => setEditorMode(undefined)} busy={commandBusy} />}

      <footer className="action-bar">
        <div className="action-group"><span className="action-label">Operator actions</span>{canRequestAction && <><button type="button" className="button primary" onClick={() => onCommand({ kind: "forward_unchanged" })} disabled={commandBusy}>Forward unchanged</button><button type="button" className="button" onClick={() => openEditor("forward_edited")} disabled={commandBusy}>Edit &amp; forward</button><button type="button" className="button" onClick={() => openEditor("manual_response")} disabled={commandBusy}>Manual response</button></>}{canResponseAction && <><button type="button" className="button primary" onClick={() => onCommand({ kind: "release_unchanged" })} disabled={commandBusy}>Release unchanged</button><button type="button" className="button" onClick={() => openEditor("release_edited")} disabled={commandBusy}>Edit &amp; release</button><button type="button" className="button" onClick={() => openEditor("replace_response")} disabled={commandBusy}>Replace response</button></>}{!canRequestAction && !canResponseAction && <span className="muted">No pending gate action</span>}</div>
        <div className="action-group danger-actions"><button type="button" className="button danger" onClick={() => onCommand({ kind: exchange.state === "request_held" || exchange.state === "response_held" ? "drop" : "abort", reason: "operator action from workbench" })} disabled={commandBusy || exchange.state === "completed" || exchange.state === "dropped" || exchange.state === "cancelled"}>{exchange.state === "request_held" || exchange.state === "response_held" ? "Drop" : "Abort"}</button></div>
      </footer>
    </main>
  );
}
