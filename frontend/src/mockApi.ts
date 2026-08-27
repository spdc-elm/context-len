import {
  type ArtifactBody,
  type ArtifactReadRequest,
  type ArtifactRef,
  type CommandResult,
  type ExchangeEvent,
  type ExchangeSnapshot,
  type GateMode,
  type MutationResult,
  type WorkspaceApi,
  type WorkspaceCommand,
  type WorkspacePolicy,
  type ArtifactStage,
  requestArtifact,
  responseArtifact,
} from "./contracts";

const encoder = new TextEncoder();
const decoder = new TextDecoder();

/** A deterministic, clearly synthetic digest for mock artifacts. */
function mockDigest(text: string): string {
  let a = 0x811c9dc5;
  let b = 0x9e3779b9;
  for (const byte of encoder.encode(text)) {
    a = Math.imul(a ^ byte, 0x01000193);
    b = Math.imul(b ^ (byte + 17), 0x85ebca6b);
  }
  const head = `${(a >>> 0).toString(16).padStart(8, "0")}${(b >>> 0).toString(16).padStart(8, "0")}`;
  return head.repeat(4);
}

function now(): string {
  return new Date().toISOString();
}

function clone<T>(value: T): T {
  return JSON.parse(JSON.stringify(value)) as T;
}

function headers(contentType: string, extra: Record<string, string> = {}): Record<string, string> {
  return { "content-type": contentType, "x-context-lens-mock": "true", ...extra };
}

function makeArtifact(
  artifactId: string,
  stage: ArtifactStage,
  direction: "request" | "response",
  body: string,
  contentType: string,
): ArtifactRef {
  return {
    artifact_id: artifactId,
    stage,
    direction,
    content_type: contentType,
    content_encoding: "identity",
    size: encoder.encode(body).byteLength,
    sha256: mockDigest(body),
    complete: true,
    storage_ref: `mock://${artifactId}`,
  };
}

interface MockRecord {
  snapshot: ExchangeSnapshot;
  bodies: Map<string, string>;
}

function createRecord(
  snapshot: ExchangeSnapshot,
  bodies: Record<string, string>,
): MockRecord {
  return { snapshot, bodies: new Map(Object.entries(bodies)) };
}

const responsesRequest = JSON.stringify(
  {
    model: "mock-responses-model",
    input: [{ type: "message", role: "user", content: [{ type: "input_text", text: "Hello from the mock" }] }],
    reasoning: { effort: "low" },
    tools: [{ type: "function", name: "lookup", parameters: { type: "object" } }],
    provider_extension: { preserved: true },
    stream: false,
  },
  null,
  2,
);
const responsesResponse = JSON.stringify(
  {
    id: "resp_mock_01",
    object: "response",
    model: "mock-responses-model",
    status: "completed",
    output: [{ type: "message", id: "msg_01", role: "assistant", content: [{ type: "output_text", text: "Mock response" }] }],
    usage: { input_tokens: 12, output_tokens: 4 },
  },
  null,
  2,
);
const chatRequest = JSON.stringify(
  {
    model: "mock-chat-model",
    messages: [{ role: "user", content: "Stream a mock answer" }],
    tools: [{ type: "function", function: { name: "lookup", parameters: { type: "object" } } }],
    stream: true,
  },
  null,
  2,
);
const chatResponse = [
  "data: {\"id\":\"chatcmpl_mock_01\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Mock\"},\"finish_reason\":null}]}\n\n",
  "data: {\"id\":\"chatcmpl_mock_01\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" stream\"},\"finish_reason\":null}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\n",
  "data: [DONE]\n\n",
].join("");
const anthropicRequest = JSON.stringify(
  {
    model: "mock-claude-model",
    max_tokens: 128,
    thinking: { type: "enabled", budget_tokens: 64 },
    messages: [{ role: "user", content: [{ type: "text", text: "Inspect this request" }] }],
  },
  null,
  2,
);
const anthropicResponse = [
  "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_mock_01\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"mock-claude-model\",\"usage\":{}}}\n\n",
  "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"Mock reasoning\",\"signature\":\"mock-signature\"}}\n\n",
  "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
  "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n",
  "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
].join("");

function responsePart(artifact: ArtifactRef, body: string, contentType: string) {
  return {
    envelope: {
      status: 200,
      headers: headers(contentType, { "x-upstream": "mock" }),
      trailers: {},
      start_timestamp: "2026-08-27T10:12:00.000Z",
      end_timestamp: "2026-08-27T10:12:00.020Z",
    },
    artifact_refs: [artifact],
    projection: {
      protocol_hint: artifact.content_type.includes("event-stream") ? "sse" : undefined,
      parse_status: "not_attempted" as const,
      warnings: [],
    },
  };
}

function requestPart(artifact: ArtifactRef, protocolPath: string, contentType = "application/json") {
  return {
    envelope: {
      method: "POST",
      path: protocolPath,
      escaped_path: protocolPath,
      raw_query: "",
      headers: headers(contentType, { authorization: "[redacted]" }),
    },
    artifact_refs: [artifact],
    projection: {
      protocol_hint: undefined,
      parse_status: "not_attempted" as const,
      warnings: [],
    },
  };
}

function initialRecords(): MockRecord[] {
  const responseRequestArtifact = makeArtifact("mock-responses-request-in", "request.inbound", "request", responsesRequest, "application/json");
  const responseResponseArtifact = makeArtifact("mock-responses-response-up", "response.upstream", "response", responsesResponse, "application/json");
  const chatRequestArtifact = makeArtifact("mock-chat-request-in", "request.inbound", "request", chatRequest, "application/json");
  const chatResponseArtifact = makeArtifact("mock-chat-response-up", "response.upstream", "response", chatResponse, "text/event-stream");
  const anthropicRequestArtifact = makeArtifact("mock-anthropic-request-in", "request.inbound", "request", anthropicRequest, "application/json");
  const anthropicResponseArtifact = makeArtifact("mock-anthropic-response-up", "response.upstream", "response", anthropicResponse, "text/event-stream");

  return [
    createRecord(
      {
        exchange_id: "ex_mock_responses_01",
        protocol: "responses",
        request: requestPart(responseRequestArtifact, "/v1/responses"),
        response: responsePart(responseResponseArtifact, responsesResponse, "application/json"),
        policy: { request_gate: "hold", response_gate: "pass" },
        state: "request_held",
        warnings: ["Mock data only: no upstream request has been sent."],
        created_at: "2026-08-27T10:12:00.000Z",
        updated_at: "2026-08-27T10:12:00.000Z",
      },
      {
        [responseRequestArtifact.artifact_id]: responsesRequest,
        [responseResponseArtifact.artifact_id]: responsesResponse,
      },
    ),
    createRecord(
      {
        exchange_id: "ex_mock_chat_01",
        protocol: "chat_completions",
        request: requestPart(chatRequestArtifact, "/v1/chat/completions"),
        response: responsePart(chatResponseArtifact, chatResponse, "text/event-stream"),
        policy: { request_gate: "pass", response_gate: "hold" },
        state: "response_held",
        warnings: ["SSE body is held as opaque bytes until an explicit release command."],
        created_at: "2026-08-27T10:11:35.000Z",
        updated_at: "2026-08-27T10:11:58.000Z",
      },
      {
        [chatRequestArtifact.artifact_id]: chatRequest,
        [chatResponseArtifact.artifact_id]: chatResponse,
      },
    ),
    createRecord(
      {
        exchange_id: "ex_mock_anthropic_01",
        protocol: "anthropic_messages",
        request: requestPart(anthropicRequestArtifact, "/v1/messages"),
        response: responsePart(anthropicResponseArtifact, anthropicResponse, "text/event-stream"),
        policy: { request_gate: "pass", response_gate: "pass" },
        state: "completed",
        warnings: [],
        created_at: "2026-08-27T10:10:00.000Z",
        updated_at: "2026-08-27T10:10:03.000Z",
      },
      {
        [anthropicRequestArtifact.artifact_id]: anthropicRequest,
        [anthropicResponseArtifact.artifact_id]: anthropicResponse,
      },
    ),
  ];
}

function commandState(command: WorkspaceCommand, current: ExchangeSnapshot, policy: WorkspacePolicy): ExchangeSnapshot["state"] {
  switch (command.kind) {
    case "forward_unchanged":
    case "forward_edited":
      if (current.state !== "request_held") throw new Error("request is not held");
      return policy.response_gate === "hold" ? "response_held" : "completed";
    case "manual_response":
      if (current.state !== "request_held") throw new Error("request is not held");
      return "completed";
    case "release_unchanged":
    case "release_edited":
    case "replace_response":
      if (current.state !== "response_held") throw new Error("response is not held");
      return "completed";
    case "drop":
      if (current.state !== "request_held" && current.state !== "response_held") throw new Error("exchange is not held");
      return "dropped";
    case "abort":
      if (current.state === "completed" || current.state === "dropped") throw new Error("exchange is already terminal");
      return "cancelled";
  }
}

function syntheticDerivedArtifact(
  record: MockRecord,
  current: ExchangeSnapshot,
  command: WorkspaceCommand,
): MutationResult | undefined {
  let source: ArtifactRef | undefined;
  let body: string | undefined;
  let contentType = "application/json";
  if (command.kind === "forward_edited") {
    source = requestArtifact(current);
    body = command.mutation.raw_replacement ?? JSON.stringify({ edited: true, patch: command.mutation.patch ?? [] }, null, 2);
  } else if (command.kind === "release_edited") {
    source = responseArtifact(current);
    body = command.mutation.raw_replacement ?? JSON.stringify({ edited: true, patch: command.mutation.patch ?? [] }, null, 2);
    contentType = source?.content_type ?? contentType;
  } else if (command.kind === "manual_response" || command.kind === "replace_response") {
    source = responseArtifact(current);
    body = command.raw_response;
    contentType = command.content_type ?? source?.content_type ?? contentType;
  }
  if (!body) return undefined;

  const stage: ArtifactStage = source?.stage ?? "response.downstream";
  const id = `mock-derived-${current.exchange_id}-${command.kind}-${current.updated_at.replaceAll(/[^0-9]/g, "")}`;
  const derived = makeArtifact(id, stage, source?.direction === "request" ? "request" : "response", body, contentType);
  record.bodies.set(id, body);
  return {
    base_artifact_id: source?.artifact_id,
    base_sha256: source?.sha256,
    derived_artifact: derived,
    validation: { valid: true, protocol: current.protocol, errors: [], warnings: ["Mock validation only; backend must validate before transport."] },
  };
}

/**
 * In-memory API used by the shell until the real workspace transport is wired.
 * It deliberately never calls fetch or any external endpoint.
 */
export class MockWorkspaceApi implements WorkspaceApi {
  private readonly records = new Map<string, MockRecord>();
  private readonly revisions = new Map<string, number>();
  private readonly listeners = new Set<(event: ExchangeEvent) => void>();
  private policy: WorkspacePolicy = { request_gate: "pass", response_gate: "pass" };

  constructor(records = initialRecords()) {
    for (const record of records) {
      this.records.set(record.snapshot.exchange_id, record);
      this.revisions.set(record.snapshot.exchange_id, 0);
    }
  }

  async listExchanges(_signal?: AbortSignal): Promise<ExchangeSnapshot[]> {
    return [...this.records.values()].map(({ snapshot }) => clone(snapshot));
  }

  async getExchange(exchangeId: string, _signal?: AbortSignal): Promise<ExchangeSnapshot> {
    const record = this.records.get(exchangeId);
    if (!record) throw new Error(`exchange not found: ${exchangeId}`);
    return clone(record.snapshot);
  }

  async readArtifact(request: ArtifactReadRequest, _signal?: AbortSignal): Promise<ArtifactBody> {
    for (const record of this.records.values()) {
      const body = record.bodies.get(request.artifact_id);
      if (body === undefined) continue;
      const bytes = encoder.encode(body);
      const start = Math.max(0, Math.min(request.start ?? 0, bytes.byteLength));
      const requestedEnd = request.end ?? bytes.byteLength;
      const end = Math.max(start, Math.min(requestedEnd, bytes.byteLength));
      return {
        artifact_id: request.artifact_id,
        bytes: bytes.slice(start, end),
        content_type: [...record.snapshot.request.artifact_refs, ...record.snapshot.response.artifact_refs].find((artifact) => artifact.artifact_id === request.artifact_id)?.content_type ?? "application/octet-stream",
        content_encoding: "identity",
        complete: end >= bytes.byteLength,
        start,
        end,
        total_size: bytes.byteLength,
      };
    }
    throw new Error(`artifact not found: ${request.artifact_id}`);
  }

  async command(command: WorkspaceCommand, _signal?: AbortSignal): Promise<CommandResult> {
    const record = this.records.get(command.exchange_id);
    if (!record) throw new Error(`exchange not found: ${command.exchange_id}`);
    const current = record.snapshot;
    const revision = this.revisions.get(command.exchange_id) ?? 0;
    if (command.base_revision !== revision) {
      throw new Error(`stale revision: expected ${revision}, received ${command.base_revision}`);
    }
    const nextState = commandState(command, current, this.policy);
    const mutation = syntheticDerivedArtifact(record, current, command);
    const updated = now();
    const next: ExchangeSnapshot = {
      ...current,
      state: nextState,
      updated_at: updated,
      warnings: command.kind === "abort" ? [...current.warnings, "Cancelled by operator in mock workspace."] : current.warnings,
    };
    if (mutation?.derived_artifact) {
      if (mutation.derived_artifact.direction === "request") {
        next.request = {
          ...next.request,
          artifact_refs: [...next.request.artifact_refs, mutation.derived_artifact],
        };
      } else {
        next.response = {
          ...next.response,
          artifact_refs: [...next.response.artifact_refs, mutation.derived_artifact],
        };
      }
    }
    const nextRevision = revision + 1;
    this.revisions.set(command.exchange_id, nextRevision);
    record.snapshot = next;
    const event: ExchangeEvent = {
      event_id: `mock-event-${command.exchange_id}-${nextRevision}`,
      exchange_id: command.exchange_id,
      revision: nextRevision,
      kind: nextState === "completed" ? "completed" : nextState === "dropped" ? "dropped" : nextState === "cancelled" ? "cancelled" : "updated",
      snapshot_delta: { state: nextState, updated_at: updated },
      artifact_refs: mutation?.derived_artifact ? [mutation.derived_artifact] : [],
      created_at: updated,
    };
    for (const listener of this.listeners) listener(event);
    return { exchange: clone(next), revision: nextRevision, mutation, event };
  }

  async getPolicy(_signal?: AbortSignal): Promise<WorkspacePolicy> {
    return { ...this.policy };
  }

  async setPolicy(policy: WorkspacePolicy, _signal?: AbortSignal): Promise<WorkspacePolicy> {
    this.policy = { ...policy };
    return { ...this.policy };
  }

  subscribe(listener: (event: ExchangeEvent) => void): () => void {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  }
}

export function createMockWorkspaceApi(): WorkspaceApi {
  return new MockWorkspaceApi();
}

export function decodeArtifactBody(body: ArtifactBody): string {
  return decoder.decode(body.bytes);
}
