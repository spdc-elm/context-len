import type { ContextBlock, ContextContentPart, ContextProtocol } from "./contextIr";

/**
 * Live SSE stream projection.
 *
 * The response artifact bytes stay the only wire authority.  This module
 * turns observed SSE records (from workspace stream events while the body is
 * still flowing, or from a complete artifact body) into typed Context IR
 * deltas and folds them into live ContextBlocks that the Chat Template
 * renderer can append.  Unrecognised events are retained as passthrough
 * blocks; nothing is fabricated or dropped.
 */

export interface StreamEventRecord {
  ordinal: number;
  name?: string;
  sse_id?: string;
  retry?: number;
  data?: string;
  complete?: boolean;
  byte_start?: number;
  byte_end?: number;
}

export type StreamStatus = "streaming" | "completed" | "failed" | "cancelled";

export interface LiveStreamEventLog {
  ordinal: number;
  name?: string;
  data?: string;
}

export interface LiveStreamState {
  protocol: ContextProtocol;
  status: StreamStatus;
  statusDetail?: string;
  nextOrdinal: number;
  blocks: ContextBlock[];
  events: LiveStreamEventLog[];
  eventCount: number;
  /** Records that arrived ahead of a missing ordinal; drained when the gap closes. */
  pendingRecords?: StreamEventRecord[];
}

const MAX_EVENT_LOG = 400;

export function initialLiveStream(protocol: ContextProtocol): LiveStreamState {
  return { protocol, status: "streaming", nextOrdinal: 0, blocks: [], events: [], eventCount: 0, pendingRecords: [] };
}

type StreamDelta =
  | { kind: "ensure_text"; key: string; text?: string }
  | { kind: "ensure_reasoning"; key: string; text?: string; thinking?: boolean }
  | { kind: "append_text"; key: string; append: string }
  | { kind: "append_reasoning"; key: string; append: string; thinking?: boolean }
  | { kind: "append_refusal"; key: string; append: string }
  | { kind: "tool_call_start"; key: string; callId?: string; name?: string }
  | { kind: "tool_call_args"; key: string; append: string; replace?: string }
  | { kind: "tool_result"; key: string; callId?: string; text: string }
  | { kind: "status"; status: StreamStatus; detail?: string }
  | { kind: "passthrough"; record: StreamEventRecord };

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function parseEventData(record: StreamEventRecord): Record<string, unknown> | undefined {
  const data = (record.data ?? "").trim();
  if (!data || data === "[DONE]") return undefined;
  try {
    const parsed = JSON.parse(data) as unknown;
    return isRecord(parsed) ? parsed : undefined;
  } catch {
    return undefined;
  }
}

const pointerFor = (ordinal: number) => `/events/${ordinal}`;

/* ---------------- Responses ---------------- */

// Protocol-defined Responses event types that carry no live-block content of
// their own (or are folded by dedicated cases above).  Anything outside this
// set is retained as a passthrough block: the stream stays observable even
// when the provider adds new event types.
const KNOWN_RESPONSES_TYPES = new Set([
  "response.created",
  "response.in_progress",
  "response.queued",
  "response.output_item.added",
  "response.output_item.done",
  "response.content_part.added",
  "response.content_part.done",
  "response.output_text.delta",
  "response.output_text.done",
  "response.refusal.delta",
  "response.refusal.done",
  "response.reasoning_summary_part.added",
  "response.reasoning_summary_part.done",
  "response.reasoning_summary_text.delta",
  "response.reasoning_summary_text.done",
  "response.reasoning_text.delta",
  "response.reasoning_text.done",
  "response.function_call_arguments.delta",
  "response.function_call_arguments.done",
  "response.completed",
  "response.failed",
  "response.incomplete",
]);

function responsesDeltas(record: StreamEventRecord): StreamDelta[] {
  if ((record.data ?? "").trim() === "[DONE]") {
    // [DONE] is a Chat Completions sentinel; in a Responses stream it is an
    // unknown record and must stay visible rather than terminating anything.
    return [{ kind: "passthrough", record }];
  }
  const payload = parseEventData(record);
  if (!payload) return [{ kind: "passthrough", record }];
  const type = typeof payload.type === "string" ? payload.type : "";
  const itemId = typeof payload.item_id === "string" ? payload.item_id : "";
  switch (type) {
    case "response.output_text.delta":
      return appendDelta("append_text", itemId, payload.delta);
    case "response.output_text.done":
      return [{ kind: "ensure_text", key: itemId, text: textOrUndef(payload.text) }];
    case "response.reasoning_summary_text.delta":
    case "response.reasoning_text.delta":
      return appendDelta("append_reasoning", itemId, payload.delta);
    case "response.reasoning_summary_text.done":
    case "response.reasoning_text.done":
      return [{ kind: "ensure_reasoning", key: itemId, text: textOrUndef(payload.text) }];
    case "response.function_call_arguments.delta":
      return appendDelta("tool_call_args", itemId, payload.delta);
    case "response.function_call_arguments.done":
      return [{ kind: "tool_call_args", key: itemId, append: "", replace: textOrUndef(payload.arguments) }];
    case "response.custom_tool_call_input.delta":
      // A custom tool's input is its arguments: the provider streams the
      // raw input (for example JS source for an exec tool) as deltas.
      return appendDelta("tool_call_args", itemId, payload.delta);
    case "response.custom_tool_call_input.done":
      return [{ kind: "tool_call_args", key: itemId, append: "", replace: textOrUndef(payload.input) }];
    case "response.refusal.delta":
      return appendDelta("append_refusal", itemId, payload.delta);
    case "response.output_item.added":
    case "response.output_item.done": {
      const item = isRecord(payload.item) ? payload.item : undefined;
      if (!item) return [{ kind: "passthrough", record }];
      // Responses records address items by item_id; output_item events carry
      // the same value as item.id, so both key spellings fold onto one block.
      const id = typeof item.id === "string" && item.id ? item.id : itemId;
      const itemType = typeof item.type === "string" ? item.type : "";
      if (itemType === "message") {
        return [{ kind: "ensure_text", key: id }];
      }
      if (itemType === "reasoning") {
        const summary = Array.isArray(item.summary)
          ? item.summary.map((part) => (isRecord(part) && typeof part.text === "string" ? part.text : "")).join("\n")
          : undefined;
        return [{ kind: "ensure_reasoning", key: id, text: summary || undefined }];
      }
      if (itemType === "function_call" || itemType === "custom_tool_call") {
        return [
          { kind: "tool_call_start", key: id, callId: textOrUndef(item.call_id), name: textOrUndef(item.name) },
          ...(typeof item.arguments === "string" ? [{ kind: "tool_call_args" as const, key: id, append: "", replace: item.arguments }] : []),
        ];
      }
      if (itemType === "function_call_output" || itemType === "custom_tool_call_output") {
        const output = typeof item.output === "string" ? item.output : JSON.stringify(item.output);
        return [{ kind: "tool_result", key: id, callId: textOrUndef(item.call_id), text: output }];
      }
      return [{ kind: "passthrough", record }];
    }
    case "response.completed":
      return [{ kind: "status", status: "completed", detail: type }];
    case "response.failed":
      return [{ kind: "status", status: "failed", detail: type }];
    case "response.incomplete":
      return [{ kind: "status", status: "completed", detail: `${type} (incomplete)` }];
    default:
      if (KNOWN_RESPONSES_TYPES.has(type)) return [];
      return [{ kind: "passthrough", record }];
  }
}

/* ---------------- Chat Completions ---------------- */

function chatDeltas(record: StreamEventRecord): StreamDelta[] {
  if ((record.data ?? "").trim() === "[DONE]") {
    return [{ kind: "status", status: "completed", detail: "[DONE]" }];
  }
  const payload = parseEventData(record);
  if (!payload) return [{ kind: "passthrough", record }];
  const choices = Array.isArray(payload.choices) ? payload.choices : [];
  const deltas: StreamDelta[] = [];
  for (const choice of choices) {
    if (!isRecord(choice)) continue;
    const index = typeof choice.index === "number" && choice.index >= 0 ? choice.index : 0;
    const key = `choice:${index}`;
    const delta = isRecord(choice.delta) ? choice.delta : undefined;
    if (!delta) continue;
    if (typeof delta.content === "string" && delta.content) {
      deltas.push({ kind: "append_text", key, append: delta.content });
    }
    const reasoning = delta.reasoning_content ?? delta.reasoning;
    if (typeof reasoning === "string" && reasoning) {
      deltas.push({ kind: "append_reasoning", key, append: reasoning });
    }
    if (typeof delta.refusal === "string" && delta.refusal) {
      deltas.push({ kind: "append_refusal", key, append: delta.refusal });
    }
    if (Array.isArray(delta.tool_calls)) {
      for (const call of delta.tool_calls) {
        if (!isRecord(call)) continue;
        const callIndex = typeof call.index === "number" && call.index >= 0 ? call.index : 0;
        const callKey = `${key}:tool:${callIndex}`;
        const fn = isRecord(call.function) ? call.function : undefined;
        const id = typeof call.id === "string" && call.id ? call.id : undefined;
        const name = fn && typeof fn.name === "string" && fn.name ? fn.name : undefined;
        if (id || name) deltas.push({ kind: "tool_call_start", key: callKey, callId: id, name });
        if (fn && typeof fn.arguments === "string" && fn.arguments) {
          deltas.push({ kind: "tool_call_args", key: callKey, append: fn.arguments });
        }
      }
    }
  }
  return deltas;
}

/* ---------------- Anthropic Messages ---------------- */

function anthropicDeltas(record: StreamEventRecord): StreamDelta[] {
  const name = record.name ?? "";
  const payload = parseEventData(record);
  if (!payload) {
    // Known control records carry JSON; an unparsable one is only ignored
    // when its event name is part of the protocol vocabulary.
    return knownAnthropicName(name) ? [] : [{ kind: "passthrough", record }];
  }
  const key = `block:${typeof payload.index === "number" ? payload.index : 0}`;
  switch (name) {
    case "message_start":
    case "ping":
    case "content_block_stop":
    case "message_delta":
      return [];
    case "content_block_start": {
      const block = isRecord(payload.content_block) ? payload.content_block : undefined;
      if (!block) return [];
      const type = typeof block.type === "string" ? block.type : "";
      if (type === "text") return [{ kind: "ensure_text", key, text: textOrUndef(block.text) }];
      if (type === "thinking") return [{ kind: "ensure_reasoning", key, thinking: true, text: textOrUndef(block.thinking) }];
      if (type === "redacted_thinking") return [{ kind: "ensure_reasoning", key, thinking: true, text: "[redacted thinking]" }];
      if (type === "tool_use") {
        return [
          { kind: "tool_call_start", key, callId: textOrUndef(block.id), name: textOrUndef(block.name) },
          ...(typeof block.input === "string" ? [{ kind: "tool_call_args" as const, key, append: "", replace: block.input }] : []),
        ];
      }
      return [{ kind: "passthrough", record }];
    }
    case "content_block_delta": {
      const delta = isRecord(payload.delta) ? payload.delta : undefined;
      if (!delta) return [];
      const type = typeof delta.type === "string" ? delta.type : "";
      if (type === "text_delta" && typeof delta.text === "string") return [{ kind: "append_text", key, append: delta.text }];
      if (type === "thinking_delta" && typeof delta.thinking === "string") {
        return [{ kind: "append_reasoning", key, thinking: true, append: delta.thinking }];
      }
      if (type === "input_json_delta" && typeof delta.partial_json === "string") {
        return [{ kind: "tool_call_args", key, append: delta.partial_json }];
      }
      if (type === "signature_delta") return [];
      return [{ kind: "passthrough", record }];
    }
    case "message_stop":
      return [{ kind: "status", status: "completed", detail: name }];
    case "error":
      return [{ kind: "status", status: "failed", detail: "error event" }];
    default:
      return [{ kind: "passthrough", record }];
  }
}

function knownAnthropicName(name: string): boolean {
  return ["message_start", "ping", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop", "error"].includes(name);
}

function textOrUndef(value: unknown): string | undefined {
  return typeof value === "string" && value ? value : undefined;
}

function appendDelta(kind: "append_text" | "append_reasoning" | "append_refusal" | "tool_call_args", key: string, value: unknown): StreamDelta[] {
  if (typeof value !== "string" || !value) return [];
  return [{ kind, key, append: value }];
}

/* ---------------- Reducer ---------------- */

function contentPart(kind: string, text: string, ordinal: number): ContextContentPart {
  return { kind, text, value: text, sourcePointer: pointerFor(ordinal) };
}

function ensureBlock(state: LiveStreamState, key: string, init: () => ContextBlock): ContextBlock {
  const existing = state.blocks.find((block) => block.id === `live:${key}`);
  if (existing) return existing;
  const created = init();
  state.blocks.push(created);
  return created;
}

function applyDelta(state: LiveStreamState, delta: StreamDelta, ordinal: number): void {
  switch (delta.kind) {
    case "ensure_text": {
      const block = ensureBlock(state, delta.key, () => ({
        id: `live:${delta.key}`, kind: "assistant", role: "assistant",
        text: delta.text ?? "", content: [contentPart("text", delta.text ?? "", ordinal)],
        sourcePointer: pointerFor(ordinal), original: undefined,
      }));
      if (!block.text && delta.text) {
        block.text = delta.text;
        block.content = [contentPart("text", delta.text, ordinal)];
      }
      return;
    }
    case "ensure_reasoning": {
      const block = ensureBlock(state, delta.key, () => ({
        id: `live:${delta.key}`, kind: delta.thinking ? "thinking" : "reasoning", role: "assistant",
        text: delta.text ?? "", content: [contentPart(delta.thinking ? "thinking" : "reasoning", delta.text ?? "", ordinal)],
        sourcePointer: pointerFor(ordinal), original: undefined,
      }));
      if (!block.text && delta.text) {
        block.text = delta.text;
        block.content = [contentPart(delta.thinking ? "thinking" : "reasoning", delta.text, ordinal)];
      }
      return;
    }
    case "append_text": {
      const block = ensureBlock(state, delta.key, () => ({
        id: `live:${delta.key}`, kind: "assistant", role: "assistant",
        text: "", content: [], sourcePointer: pointerFor(ordinal), original: undefined,
      }));
      block.text = (block.text ?? "") + delta.append;
      block.content = [contentPart("text", block.text, ordinal)];
      return;
    }
    case "append_reasoning": {
      const block = ensureBlock(state, delta.key, () => ({
        id: `live:${delta.key}`, kind: delta.thinking ? "thinking" : "reasoning", role: "assistant",
        text: "", content: [], sourcePointer: pointerFor(ordinal), original: undefined,
      }));
      block.text = (block.text ?? "") + delta.append;
      block.content = [contentPart(delta.thinking ? "thinking" : "reasoning", block.text, ordinal)];
      return;
    }
    case "append_refusal": {
      const block = ensureBlock(state, delta.key, () => ({
        id: `live:${delta.key}`, kind: "refusal", role: "assistant", text: "", content: [],
        sourcePointer: pointerFor(ordinal), original: undefined,
      }));
      block.text = (block.text ?? "") + delta.append;
      block.content = [contentPart("refusal", block.text, ordinal)];
      return;
    }
    case "tool_call_start": {
      const block = ensureBlock(state, delta.key, () => ({
        id: `live:${delta.key}`, kind: "tool_call", role: "assistant",
        toolName: delta.name, callId: delta.callId, text: undefined,
        content: [contentPart("tool_call", delta.name ?? delta.callId ?? "", ordinal)],
        sourcePointer: pointerFor(ordinal), original: undefined,
      }));
      if (delta.name && !block.toolName) block.toolName = delta.name;
      if (delta.callId && !block.callId) block.callId = delta.callId;
      return;
    }
    case "tool_call_args": {
      const block = ensureBlock(state, delta.key, () => ({
        id: `live:${delta.key}`, kind: "tool_call", role: "assistant",
        content: [contentPart("tool_call", "", ordinal)],
        sourcePointer: pointerFor(ordinal), original: undefined,
      }));
      const next = delta.replace !== undefined ? delta.replace : (block.rawArguments ?? "") + delta.append;
      block.rawArguments = next;
      // Tiny streaming fragments are intentionally kept raw. Parsing only when
      // a structural terminator arrives avoids repeated JSON work per delta;
      // the final `.done`/replacement path always parses.
      const candidate = next.trim();
      if (delta.replace !== undefined || candidate.endsWith("}") || candidate.endsWith("]")) {
        try {
          block.arguments = JSON.parse(next) as unknown;
        } catch {
          block.arguments = next;
        }
      } else {
        block.arguments = next;
      }
      return;
    }
    case "tool_result": {
      const block = ensureBlock(state, delta.key, () => ({
        id: `live:${delta.key}`, kind: "tool_result", role: "tool",
        callId: delta.callId, content: [contentPart("tool_result", delta.text, ordinal)],
        sourcePointer: pointerFor(ordinal), original: undefined,
      }));
      if (!block.text) block.text = delta.text;
      return;
    }
    case "status": {
      if (state.status === "streaming") {
        state.status = delta.status;
        state.statusDetail = delta.detail;
      }
      return;
    }
    case "passthrough": {
      state.blocks.push({
        id: `live:passthrough:${ordinal}`,
        kind: "unknown",
        passthrough: { name: delta.record.name, data: delta.record.data },
        content: [{ kind: "provider_extension", value: { name: delta.record.name, data: delta.record.data }, sourcePointer: pointerFor(ordinal) }],
        sourcePointer: pointerFor(ordinal),
        original: { name: delta.record.name, data: delta.record.data },
      });
      return;
    }
  }
}

function deltasForRecord(protocol: ContextProtocol, record: StreamEventRecord): StreamDelta[] {
  if (protocol === "responses") return responsesDeltas(record);
  if (protocol === "chat_completions") return chatDeltas(record);
  if (protocol === "anthropic_messages") return anthropicDeltas(record);
  return [{ kind: "passthrough", record }];
}

/** Fold one observed SSE record into the live state. Records are deduplicated
 * by ordinal. Ahead-of-order records are retained in a bounded gap queue and
 * drained once the missing ordinal arrives. */
export function applyStreamRecord(state: LiveStreamState, record: StreamEventRecord): LiveStreamState {
  if (record.ordinal < state.nextOrdinal) return state;
  if (record.ordinal > state.nextOrdinal) {
    const pending = [...(state.pendingRecords ?? [])];
    if (!pending.some((item) => item.ordinal === record.ordinal)) pending.push(record);
    pending.sort((a, b) => a.ordinal - b.ordinal);
    return { ...state, pendingRecords: pending.slice(0, MAX_EVENT_LOG) };
  }
  let next = applyContiguousRecord(state, record);
  while (next.pendingRecords?.length && next.pendingRecords[0].ordinal === next.nextOrdinal) {
    const [queued, ...rest] = next.pendingRecords;
    next = applyContiguousRecord({ ...next, pendingRecords: rest }, queued);
  }
  return next;
}

function applyContiguousRecord(state: LiveStreamState, record: StreamEventRecord): LiveStreamState {
  const next: LiveStreamState = {
    ...state,
    blocks: state.blocks.map((block) => ({ ...block })),
    events: state.events,
    pendingRecords: state.pendingRecords ?? [],
  };
  const log = [...next.events, { ordinal: record.ordinal, name: record.name, data: record.data }];
  next.events = log.length > MAX_EVENT_LOG ? log.slice(log.length - MAX_EVENT_LOG) : log;
  next.eventCount = state.eventCount + 1;
  next.nextOrdinal = record.ordinal + 1;
  for (const delta of deltasForRecord(next.protocol, record)) applyDelta(next, delta, record.ordinal);
  return next;
}

/** Fold a batch of records, retaining ordinal and duplicate semantics. */
export function applyStreamRecords(state: LiveStreamState, records: StreamEventRecord[]): LiveStreamState {
  let next = state;
  for (const record of records) next = applyStreamRecord(next, record);
  return next;
}

/** Observe an exchange lifecycle transition (completed/failed/cancelled/dropped). */
export function applyStreamTerminus(state: LiveStreamState, status: StreamStatus, detail?: string): LiveStreamState {
  if (state.status !== "streaming") return state;
  return { ...state, status, statusDetail: detail ?? state.statusDetail };
}

/** Fold a complete artifact body's records into a live stream state.  Used when
 * the operator selects the SSE response artifact: the same reducer that powers
 * the live view renders the finished stream, so both views agree by
 * construction.  A stream without a protocol terminal record is finalized as
 * completed with an explicit detail, never silently marked streaming. */
export function buildLiveStream(protocol: ContextProtocol, records: StreamEventRecord[]): LiveStreamState {
  let state = initialLiveStream(protocol);
  for (const record of records) state = applyStreamRecord(state, record);
  if (state.status === "streaming") {
    state = { ...state, status: "completed", statusDetail: "stream ended without a protocol terminal record" };
  }
  return state;
}

/** Parse an SSE artifact body into records.  Mirrors the backend SSE line
 * rules: CRLF/LF terminators, comments ignored, multi-line data joined with
 * newlines, and a trailing unterminated record retained as incomplete. */
export function parseSseRecords(body: string): StreamEventRecord[] {
  const records: StreamEventRecord[] = [];
  let name: string | undefined;
  let id: string | undefined;
  let retry: number | undefined;
  let data: string[] = [];
  let hasData = false;
  const flush = (terminated: boolean) => {
    // A record without a data field is not a client-visible event (the SSE
    // dispatch algorithm drops it); comment/id/retry-only records are skipped
    // the same way the backend inspection skips them.
    if (!hasData) {
      name = undefined;
      id = undefined;
      retry = undefined;
      data = [];
      return;
    }
    records.push({ ordinal: records.length, name, sse_id: id, retry, data: data.join("\n"), complete: terminated });
    name = undefined;
    id = undefined;
    retry = undefined;
    data = [];
    hasData = false;
  };
  for (const line of body.split(/\r?\n/)) {
    if (line === "") {
      flush(true);
      continue;
    }
    if (line.startsWith(":")) continue;
    const separator = line.indexOf(":");
    const field = separator < 0 ? line : line.slice(0, separator);
    const value = separator < 0 ? "" : line.slice(separator + 1).replace(/^ /, "");
    if (field === "event") name = value;
    else if (field === "id") id = value;
    else if (field === "retry") {
      const parsed = Number(value);
      if (Number.isFinite(parsed)) retry = parsed;
    } else if (field === "data") {
      data.push(value);
      hasData = true;
    }
  }
  flush(false);
  return records;
}
