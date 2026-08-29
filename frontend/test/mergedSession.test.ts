import { describe, expect, it } from "vitest";
import type { ExchangeSnapshot } from "../src/contracts";
import { buildMergedSession, messageItems, sessionLineage } from "../src/mergedSession";

function snapshot(id: string, parent: string | undefined, depth: number, overrides: Partial<ExchangeSnapshot> = {}): ExchangeSnapshot {
  return {
    exchange_id: id,
    protocol: "anthropic_messages",
    request: {
      envelope: { method: "POST", path: "/v1/messages", escaped_path: "", raw_query: "", headers: {} },
      artifact_refs: [],
    },
    response: { envelope: { status: 200, headers: {}, trailers: {} }, artifact_refs: [] },
    policy: { request_gate: "pass", response_gate: "pass" },
    state: "completed",
    warnings: [],
    created_at: "2026-08-27T10:00:00.000Z",
    updated_at: "2026-08-27T10:00:00.000Z",
    summary: { model: "m", message_count: 2 },
    session: { session_id: "sess-1", depth, parent_exchange_id: parent, root: !parent },
    ...overrides,
  } as ExchangeSnapshot;
}

function turnInput(exchange: ExchangeSnapshot, requestBody?: string, responseBody?: string, responseIsSse = false) {
  return { exchange, requestBody, responseBody, responseIsSse };
}

const request1 = JSON.stringify({
  model: "m",
  max_tokens: 64,
  system: "be brief",
  messages: [{ role: "user", content: [{ type: "text", text: "call the tool" }] }],
});
const response1 = JSON.stringify({
  id: "msg_1",
  type: "message",
  role: "assistant",
  content: [
    { type: "thinking", thinking: "considering", signature: "s" },
    { type: "tool_use", id: "toolu_1", name: "lookup", input: { key: "a" } },
  ],
  stop_reason: "tool_use",
  usage: { input_tokens: 12, output_tokens: 4 },
});
// Turn 2 echoes the assistant turn, then adds the tool result and a new user turn.
const request2 = JSON.stringify({
  model: "m",
  max_tokens: 64,
  system: "be brief",
  messages: [
    { role: "user", content: [{ type: "text", text: "call the tool" }] },
    { role: "assistant", content: [
      { type: "thinking", thinking: "considering", signature: "s" },
      { type: "tool_use", id: "toolu_1", name: "lookup", input: { key: "a" } },
    ] },
    { role: "user", content: [{ type: "tool_result", tool_use_id: "toolu_1", content: [{ type: "text", text: "result payload" }] }] },
    { role: "user", content: [{ type: "text", text: "now summarize" }] },
  ],
});
const response2 = JSON.stringify({
  id: "msg_2",
  type: "message",
  role: "assistant",
  content: [{ type: "text", text: "final answer" }],
  stop_reason: "end_turn",
  usage: { input_tokens: 40, output_tokens: 6 },
});

describe("messageItems", () => {
  it("normalizes the virtual system element per protocol", () => {
    expect(messageItems("anthropic_messages", JSON.parse(request1))).toHaveLength(2);
    expect(messageItems("chat_completions", JSON.parse('{"messages":[{"role":"system","content":"s"},{"role":"user","content":"u"}]}'))).toHaveLength(2);
    expect(messageItems("responses", JSON.parse('{"instructions":"brief","input":["a","b"]}'))).toHaveLength(3);
  });
});

describe("sessionLineage", () => {
  it("walks parents from the selected exchange to the root", () => {
    const exchanges = [snapshot("t1", undefined, 1), snapshot("t2", "t1", 2), snapshot("t3", "t2", 3)];
    expect(sessionLineage(exchanges, "t3").map((item) => item.exchange_id)).toEqual(["t1", "t2", "t3"]);
    expect(sessionLineage(exchanges, "t1").map((item) => item.exchange_id)).toEqual(["t1"]);
    expect(sessionLineage(exchanges, "missing")).toEqual([]);
  });
});

describe("buildMergedSession", () => {
  it("keeps the tool call, tool result, and final answer in one stream", () => {
    const t1 = snapshot("t1", undefined, 1);
    const t2 = snapshot("t2", "t1", 2);
    t2.summary = { model: "m", message_count: 5 };
    const merged = buildMergedSession("anthropic_messages", [
      turnInput(t1, request1, response1),
      turnInput(t2, request2, response2),
    ], undefined, "t2");

    expect(merged.turns).toHaveLength(2);
    // Turn 1 renders its full request context and the authoritative response.
    const first = merged.turns[0];
    expect(first.contextDocument).toBeDefined();
    expect(first.responseBlocks.map((block) => block.kind)).toEqual(["thinking", "tool_call"]);
    // Turn 2 keeps only the new interstitials: tool result + new user turn.
    const second = merged.turns[1];
    const interstitialKinds = second.contextBlocks.map((block) => block.kind);
    expect(interstitialKinds).toEqual(["tool_result", "user"]);
    // The assistant echo is removed from the interstitials.
    expect(interstitialKinds).not.toContain("assistant");
    // The final answer renders from turn 2's response.
    expect(second.responseBlocks.map((block) => block.kind)).toEqual(["assistant"]);
  });

  it("keeps the request-side echo when the response is unavailable", () => {
    const t1 = snapshot("t1", undefined, 1);
    const t2 = snapshot("t2", "t1", 2);
    t2.summary = { model: "m", message_count: 5 };
    const merged = buildMergedSession("anthropic_messages", [
      turnInput(t1, request1, undefined),
      turnInput(t2, request2, response2),
    ], undefined, "t2");
    const kinds = merged.turns[1].contextBlocks.map((block) => block.kind);
    // The echo of the assistant turn is kept (thinking + tool call) instead of
    // being silently dropped, alongside the tool result and user turn.
    expect(kinds).toEqual(["thinking", "tool_call", "tool_result", "user"]);
    expect(merged.turns[1].markers).toContain("response unavailable · request echo shown");
  });

  it("marks model and tool changes across turns", () => {
    const t1 = snapshot("t1", undefined, 1);
    const t2 = snapshot("t2", "t1", 2);
    t2.summary = { model: "m2", message_count: 5 };
    t2.session = { ...t2.session!, model_changed: true, tools_changed: true };
    const merged = buildMergedSession("anthropic_messages", [
      turnInput(t1, request1, response1),
      turnInput(t2, request2, response2),
    ], undefined, "t2");
    expect(merged.turns[1].markers).toContain("model changed");
    expect(merged.turns[1].markers).toContain("tools changed");
  });

  it("uses the live stream as the streaming tail of the selected turn", () => {
    const t1 = snapshot("t1", undefined, 1, { state: "upstream_running" });
    const live = {
      protocol: "anthropic_messages",
      status: "streaming",
      eventCount: 3,
      events: [],
      blocks: [],
      warnings: [],
    } as never;
    const merged = buildMergedSession("anthropic_messages", [
      turnInput(t1, request1, undefined, true),
    ], live, "t1");
    expect(merged.turns[0].responseStream).toBe(live);
    expect(merged.turns[0].responseBlocks).toEqual([]);
  });

  it("folds a completed SSE response through the live reducer", () => {
    const t1 = snapshot("t1", undefined, 1);
    const sse = [
      'event: message_start',
      'data: {"type":"message_start","message":{"usage":{"input_tokens":9}}}',
      '',
      'event: content_block_start',
      'data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}',
      '',
      'event: content_block_delta',
      'data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"streamed answer"}}',
      '',
      'event: message_stop',
      'data: {"type":"message_stop"}',
      '',
    ].join("\n");
    const merged = buildMergedSession("anthropic_messages", [
      turnInput(t1, request1, sse, true),
    ], undefined, "t1");
    expect(merged.turns[0].responseStream?.status).toBe("completed");
    expect(merged.turns[0].responseStream?.blocks.map((block) => block.kind)).toContain("assistant");
  });

  it("renders chat_completions tool calls and results across turns", () => {
    const chatRequest1 = JSON.stringify({
      model: "m",
      messages: [{ role: "user", content: "call it" }],
      tools: [{ type: "function", function: { name: "lookup", parameters: { type: "object" } } }],
    });
    const chatResponse1 = JSON.stringify({
      id: "c1",
      object: "chat.completion",
      choices: [{ index: 0, message: { role: "assistant", content: null, tool_calls: [{ id: "call_1", type: "function", function: { name: "lookup", arguments: "{\"q\":\"x\"}" } }] }, finish_reason: "tool_calls" }],
      usage: { prompt_tokens: 9, completion_tokens: 3 },
    });
    const chatRequest2 = JSON.stringify({
      model: "m",
      messages: [
        { role: "user", content: "call it" },
        { role: "assistant", content: null, tool_calls: [{ id: "call_1", type: "function", function: { name: "lookup", arguments: "{\"q\":\"x\"}" } }] },
        { role: "tool", tool_call_id: "call_1", content: "{\"value\":42}" },
      ],
    });
    const chatResponse2 = JSON.stringify({
      id: "c2",
      object: "chat.completion",
      choices: [{ index: 0, message: { role: "assistant", content: "done" }, finish_reason: "stop" }],
    });
    const t1 = snapshot("t1", undefined, 1);
    t1.protocol = "chat_completions";
    const t2 = snapshot("t2", "t1", 2);
    t2.protocol = "chat_completions";
    t2.summary = { model: "m", message_count: 3 };
    const merged = buildMergedSession("chat_completions", [
      turnInput(t1, chatRequest1, chatResponse1),
      turnInput(t2, chatRequest2, chatResponse2),
    ], undefined, "t2");
    expect(merged.turns[0].responseBlocks.map((block) => block.kind)).toEqual(["tool_call"]);
    expect(merged.turns[1].contextBlocks.map((block) => block.kind)).toEqual(["tool_result"]);
    expect(merged.turns[1].responseBlocks.map((block) => block.kind)).toEqual(["assistant"]);
  });
});

describe("response body loading states", () => {
  it("keeps a terminal live stream visible while the artifact body loads", () => {
    const t1 = snapshot("t1", undefined, 1, { state: "cancelled" });
    t1.response.artifact_refs = [{ artifact_id: "a", stage: "response.upstream", direction: "upstream", content_type: "text/event-stream", content_encoding: "identity", size: 10, sha256: "s", complete: false, storage_ref: "" }];
    const live = {
      protocol: "anthropic_messages",
      status: "cancelled",
      eventCount: 5,
      events: [],
      blocks: [{ id: "live:block:0", kind: "assistant", role: "assistant", text: "streamed answer", content: [], sourcePointer: "/events/0" }],
      warnings: [],
    } as never;
    const merged = buildMergedSession("anthropic_messages", [
      turnInput(t1, request1, undefined, true),
    ], live, "t1");
    // The live blocks stay rendered after the terminal event instead of
    // flashing away while the artifact body loads.
    expect(merged.turns[0].responseStream).toBe(live);
    expect(merged.turns[0].markers).not.toContain("response loading");
    expect(merged.turns[0].markers).toContain("response incomplete");
  });

  it("prefers the loaded response artifact over the live fallback", () => {
    const t1 = snapshot("t1", undefined, 1);
    const live = {
      protocol: "anthropic_messages",
      status: "cancelled",
      eventCount: 5,
      events: [],
      blocks: [{ id: "live:block:0", kind: "assistant", role: "assistant", text: "streamed answer", content: [], sourcePointer: "/events/0" }],
      warnings: [],
    } as never;
    const merged = buildMergedSession("anthropic_messages", [
      turnInput(t1, request1, response1),
    ], live, "t1");
    expect(merged.turns[0].responseBlocks.map((block) => block.kind)).toEqual(["thinking", "tool_call"]);
    expect(merged.turns[0].responseStream).toBeUndefined();
  });

  it("distinguishes unavailable and still-loading responses", () => {
    const unavailable = snapshot("u1", undefined, 1, { state: "cancelled" });
    const loading = snapshot("l1", undefined, 1, { state: "cancelled" });
    loading.response.artifact_refs = [{ artifact_id: "a", stage: "response.upstream", direction: "upstream", content_type: "text/event-stream", content_encoding: "identity", size: 10, sha256: "s", complete: false, storage_ref: "" }];
    const merged = buildMergedSession("anthropic_messages", [
      turnInput(unavailable, request1, undefined),
      turnInput(loading, request1, undefined),
    ], undefined, "l1");
    expect(merged.turns[0].markers).toContain("response unavailable");
    expect(merged.turns[1].markers).toContain("response loading");
  });
});
