/**
 * Public browser-side types for the frozen workspace/runtime seam.
 *
 * JSON field names intentionally follow docs/runtime-contract.md.  The UI treats
 * artifact bytes as opaque and only consumes projections for display.
 */

export type Protocol = "responses" | "chat_completions" | "anthropic_messages";
export type GateMode = "pass" | "hold";
export type ExchangeState =
  | "received"
  | "request_held"
  | "upstream_running"
  | "response_held"
  | "completed"
  | "dropped"
  | "cancelled"
  | "failed";

export type ArtifactStage =
  | "request.inbound"
  | "request.upstream"
  | "response.upstream"
  | "response.downstream"
  | (string & {});
export type ArtifactDirection = "request" | "response" | (string & {});

export type HeaderMap = Record<string, string | string[]>;

export interface ArtifactRef {
  artifact_id: string;
  stage: ArtifactStage;
  direction: ArtifactDirection;
  content_type: string;
  content_encoding: string;
  size: number;
  sha256: string;
  complete: boolean;
  storage_ref: string;
}

export interface RequestEnvelope {
  method: string;
  path: string;
  escaped_path: string;
  raw_query: string;
  headers: HeaderMap;
}

export interface ResponseEnvelope {
  status: number;
  headers: HeaderMap;
  trailers: HeaderMap;
  start_timestamp?: string;
  end_timestamp?: string;
}

/** The request/response side of an exchange owns metadata and artifact refs. */
export interface RequestExchangePart {
  envelope: RequestEnvelope;
  artifact_refs: ArtifactRef[];
  /** Optional projection; never use this value as transport input. */
  projection?: InspectionProjection;
}

export interface ResponseExchangePart {
  envelope: ResponseEnvelope;
  artifact_refs: ArtifactRef[];
  /** Optional projection; never use this value as transport input. */
  projection?: InspectionProjection;
}

export interface ExchangePolicy {
  request_gate: GateMode;
  response_gate: GateMode;
}

/** Additive capture-time observation projection (docs/session-spec.md).
 *  Derived from wire bytes by the backend; display-only. */
export interface ExchangeSummary {
  model?: string;
  message_count?: number;
  preview?: string;
  tool_names?: string[];
  context_tokens?: number;
}

/** Additive session placement computed from the original inbound request.
 *  Structure comes from harness behaviour; never transport input. */
export interface SessionAssignment {
  session_id: string;
  depth: number;
  position?: string;
  parent_position?: string;
  parent_exchange_id?: string;
  repeat_index?: number;
  fork?: boolean;
  model_changed?: boolean;
  tools_changed?: boolean;
  root?: boolean;
}

export interface ExchangeSnapshot {
  exchange_id: string;
  protocol: Protocol | (string & {});
  request: RequestExchangePart;
  response: ResponseExchangePart;
  policy: ExchangePolicy;
  state: ExchangeState;
  warnings: string[];
  created_at: string;
  updated_at: string;
  /** Server revisions are additive to the frozen snapshot and seed command CAS. */
  revision?: number;
  error?: string;
  /** Capture-time observation projection; never transport input. */
  summary?: ExchangeSummary;
  /** Session placement of the original inbound request. */
  session?: SessionAssignment;
  /** Server may add fields; renderers must leave unknown fields untouched. */
  [extension: string]: unknown;
}

export interface ExchangeSnapshotDelta {
  [extension: string]: unknown;
  exchange_id?: string;
  protocol?: Protocol | (string & {});
  request?: {
    envelope?: Partial<RequestEnvelope>;
    artifact_refs?: ArtifactRef[];
    projection?: InspectionProjection;
    [extension: string]: unknown;
  };
  response?: {
    envelope?: Partial<ResponseEnvelope>;
    artifact_refs?: ArtifactRef[];
    projection?: InspectionProjection;
    [extension: string]: unknown;
  };
  policy?: Partial<ExchangePolicy>;
  state?: ExchangeState;
  warnings?: string[];
  created_at?: string;
  updated_at?: string;
  summary?: ExchangeSummary;
  session?: SessionAssignment;
}


export type ExchangeEventKind =
  | "exchange_created"
  | "request_held"
  | "upstream_started"
  | "response_held"
  | "updated"
  | "completed"
  | "failed"
  | "cancelled"
  | "dropped"
  | "stream_event"
  | (string & {});

/** One observed SSE record while a response body streams.  Display-only
 *  projection: the artifact bytes remain the only wire authority. */
export interface StreamEventRecord {
  ordinal: number;
  name?: string;
  sse_id?: string;
  data?: string;
  complete?: boolean;
  byte_start?: number;
  byte_end?: number;
}

export interface ExchangeEvent {
  event_id: string;
  exchange_id: string;
  revision: number;
  kind: ExchangeEventKind;
  snapshot_delta: ExchangeSnapshotDelta;
  artifact_refs: ArtifactRef[];
  created_at: string;
  /** Present only when kind is "stream_event": the observed SSE record. */
  stream?: StreamEventRecord;
  [extension: string]: unknown;
}

export type CommandKind =
  | "forward_unchanged"
  | "forward_edited"
  | "manual_response"
  | "release_unchanged"
  | "release_edited"
  | "replace_response"
  | "drop"
  | "abort";

export interface CommandBase {
  exchange_id: string;
  base_revision: number;
}

export interface JsonPatchOperation {
  op: "add" | "remove" | "replace" | "move" | "copy" | "test";
  path: string;
  value?: unknown;
  from?: string;
}

export interface MutationInput {
  /** JSON Patch operations against the selected artifact. */
  patch?: JsonPatchOperation[];
  /** A raw body replacement. Kept as text in the shell; transport owns bytes. */
  raw_replacement?: string;
  /** Client hint for the artifact being edited. */
  base_artifact_id?: string;
  base_sha256?: string;
}

export interface ForwardUnchangedCommand extends CommandBase {
  kind: "forward_unchanged";
}
export interface ForwardEditedCommand extends CommandBase {
  kind: "forward_edited";
  mutation: MutationInput;
}
export interface ManualResponseCommand extends CommandBase {
  kind: "manual_response";
  /** Raw protocol response authored by the operator. */
  raw_response: string;
  content_type?: string;
}
export interface ReleaseUnchangedCommand extends CommandBase {
  kind: "release_unchanged";
}
export interface ReleaseEditedCommand extends CommandBase {
  kind: "release_edited";
  mutation: MutationInput;
}
export interface ReplaceResponseCommand extends CommandBase {
  kind: "replace_response";
  /** Raw protocol response authored by the operator. */
  raw_response: string;
  content_type?: string;
}
export interface DropCommand extends CommandBase {
  kind: "drop";
  reason?: string;
}
export interface AbortCommand extends CommandBase {
  kind: "abort";
  reason?: string;
}

export type WorkspaceCommand =
  | ForwardUnchangedCommand
  | ForwardEditedCommand
  | ManualResponseCommand
  | ReleaseUnchangedCommand
  | ReleaseEditedCommand
  | ReplaceResponseCommand
  | DropCommand
  | AbortCommand;

export interface MutationResult {
  base_artifact_id?: string;
  base_sha256?: string;
  derived_artifact?: ArtifactRef;
  validation?: ValidationResult;
}

export interface ValidationResult {
  valid: boolean;
  protocol?: string;
  errors: string[];
  warnings: string[];
}

export interface CommandResult {
  exchange: ExchangeSnapshot;
  revision: number;
  mutation?: MutationResult;
  event?: ExchangeEvent;
}

export interface ArtifactReadRequest {
  artifact_id: string;
  /** Optional byte range for lazy/virtualized body viewers. */
  start?: number;
  end?: number;
}

export interface ArtifactBody {
  artifact_id: string;
  bytes: Uint8Array;
  content_type: string;
  content_encoding: string;
  complete: boolean;
  start: number;
  end: number;
  total_size: number;
}

export interface WorkspacePolicy {
  request_gate: GateMode;
  response_gate: GateMode;
}

export interface ExchangePage {
  exchanges: ExchangeSnapshot[];
  next_cursor?: string;
  has_more?: boolean;
}

export interface WorkspaceApi {
  listExchanges(signal?: AbortSignal): Promise<ExchangeSnapshot[]>;
  /** Optional bounded page API; older injected APIs may omit this method. */
  listExchangesPage?(limit: number, cursor?: string, signal?: AbortSignal): Promise<ExchangePage>;
  getExchange(exchangeId: string, signal?: AbortSignal): Promise<ExchangeSnapshot>;
  readArtifact(request: ArtifactReadRequest, signal?: AbortSignal): Promise<ArtifactBody>;
  command(command: WorkspaceCommand, signal?: AbortSignal): Promise<CommandResult>;
  getPolicy(signal?: AbortSignal): Promise<WorkspacePolicy>;
  setPolicy(policy: WorkspacePolicy, signal?: AbortSignal): Promise<WorkspacePolicy>;
  clearExchanges(signal?: AbortSignal): Promise<void>;
  subscribe(listener: (event: ExchangeEvent) => void): () => void;
}

export type ParseStatus = "parsed" | "warning" | "failed" | "not_attempted";

export interface InspectionProjection {
  protocol_hint?: string;
  parse_status: ParseStatus;
  sections?: ProjectionSection[];
  messages?: unknown[];
  input_items?: unknown[];
  content_blocks?: unknown[];
  tools?: unknown[];
  response_items?: unknown[];
  stream_events?: StreamEventProjection[];
  unknown_nodes?: UnknownNode[];
  warnings: string[];
  [extension: string]: unknown;
}

export interface ProjectionSection {
  id: string;
  label: string;
  value: unknown;
  json_pointer?: string;
  byte_start?: number;
  byte_end?: number;
}

export interface StreamEventProjection {
  event?: string;
  data: string;
  id?: string;
  retry?: number;
  sequence?: number;
  json_pointer?: string;
}

export interface UnknownNode {
  path?: string;
  raw?: string;
  value?: unknown;
}

export function artifactRefs(part: RequestExchangePart | ResponseExchangePart): ArtifactRef[] {
  return part.artifact_refs ?? [];
}

export function requestArtifact(snapshot: ExchangeSnapshot): ArtifactRef | undefined {
  return artifactRefs(snapshot.request).find((artifact) => artifact.stage === "request.inbound") ?? artifactRefs(snapshot.request)[0];
}

export function responseArtifact(snapshot: ExchangeSnapshot): ArtifactRef | undefined {
  return artifactRefs(snapshot.response).find((artifact) => artifact.stage === "response.upstream") ?? artifactRefs(snapshot.response)[0];
}

export function isRequestAction(command: WorkspaceCommand): boolean {
  return command.kind === "forward_unchanged" || command.kind === "forward_edited" || command.kind === "manual_response";
}

export function commandLabel(command: CommandKind): string {
  return command.replaceAll("_", " ");
}
