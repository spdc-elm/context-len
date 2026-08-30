import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { type StreamEventRecord } from "../src/contracts";
import {
  applyStreamRecord,
  applyStreamRecords,
  applyStreamTerminus,
  buildLiveStream,
  initialLiveStream,
  parseSseRecords,
  type LiveStreamState,
} from "../src/streamIr";

const FIXTURES = resolve(__dirname, "../../tests/fixtures");

function recordsFromFixture(path: string): StreamEventRecord[] {
  return parseSseRecords(readFileSync(resolve(FIXTURES, path), "utf8"));
}

function feed(protocol: LiveStreamState["protocol"], records: StreamEventRecord[]): LiveStreamState {
  let state = initialLiveStream(protocol);
  for (const record of records) state = applyStreamRecord(state, record);
  return state;
}

describe("parseSseRecords", () => {
  it("parses events, ids, multi-line data, and the DONE sentinel", () => {
    const records = parseSseRecords(": comment\nid: 1\nevent: a\ndata: {\"x\":\ndata: 1}\n\ndata: [DONE]\n\n");
    expect(records).toHaveLength(2);
    expect(records[0]).toMatchObject({ name: "a", sse_id: "1", data: '{"x":\n1}', complete: true });
    expect(records[1]).toMatchObject({ data: "[DONE]", complete: true });
  });

  it("retains an unterminated trailing record as incomplete", () => {
    const records = parseSseRecords("data: {\"trunc");
    expect(records).toHaveLength(1);
    expect(records[0].complete).toBe(false);
  });
});

describe("responses stream deltas", () => {
  it("retains an out-of-order gap and drains it in ordinal order", () => {
    let state = initialLiveStream("chat_completions");
    const later = { ordinal: 1, data: JSON.stringify({ choices: [{ index: 0, delta: { content: " later" } }] }) };
    state = applyStreamRecord(state, later);
    expect(state.eventCount).toBe(0);
    state = applyStreamRecords(state, [{ ordinal: 0, data: JSON.stringify({ choices: [{ index: 0, delta: { content: "first" } }] }) }]);
    expect(state.eventCount).toBe(2);
    expect(state.nextOrdinal).toBe(2);
    expect(state.blocks[0].text).toBe("first later");
  });


  const state = feed("responses", recordsFromFixture("responses/sse/response.sse"));

  it("accumulates assistant text from output_text deltas", () => {
    const message = state.blocks.find((block) => block.id === "live:msg_fixture_stream_001");
    expect(message?.kind).toBe("assistant");
    expect(message?.text).toBe("fixture stream answer.");
  });

  it("accumulates reasoning summary", () => {
    const reasoning = state.blocks.find((block) => block.id === "live:rs_fixture_stream_001");
    expect(reasoning?.kind).toBe("reasoning");
    expect(reasoning?.text).toBe("fixture reasoning");
  });

  it("keeps unknown events as passthrough blocks", () => {
    const unknown = state.blocks.filter((block) => block.kind === "unknown");
    expect(unknown).toHaveLength(1);
    expect(unknown[0].original).toMatchObject({ name: "response.custom_fixture" });
  });

  it("reaches completed via response.completed", () => {
    expect(state.status).toBe("completed");
    expect(state.statusDetail).toBe("response.completed");
  });

  it("marks [DONE] in a responses stream as passthrough, not terminal", () => {
    const state = feed("responses", [{ ordinal: 0, data: "[DONE]", complete: true }]);
    expect(state.status).toBe("streaming");
    expect(state.blocks.filter((block) => block.kind === "unknown")).toHaveLength(1);
  });

  it("assembles tool calls and tool results from items and argument deltas", () => {
    const records: StreamEventRecord[] = [
      { ordinal: 0, data: JSON.stringify({ type: "response.output_item.added", item: { id: "fc_1", type: "function_call", call_id: "call_1", name: "lookup" } }), complete: true },
      { ordinal: 1, data: JSON.stringify({ type: "response.function_call_arguments.delta", item_id: "fc_1", delta: '{"key":' }), complete: true },
      { ordinal: 2, data: JSON.stringify({ type: "response.function_call_arguments.delta", item_id: "fc_1", delta: '"beta"}' }), complete: true },
      { ordinal: 3, data: JSON.stringify({ type: "response.output_item.added", item: { id: "fco_1", type: "function_call_output", call_id: "call_1", output: "42" } }), complete: true },
    ];
    const state = feed("responses", records);
    const call = state.blocks.find((block) => block.id === "live:fc_1");
    expect(call?.kind).toBe("tool_call");
    expect(call?.toolName).toBe("lookup");
    expect(call?.arguments).toEqual({ key: "beta" });
    const result = state.blocks.find((block) => block.id === "live:fco_1");
    expect(result?.kind).toBe("tool_result");
    expect(result?.text).toBe("42");
  });
});

describe("chat completions stream deltas", () => {
  const state = feed("chat_completions", recordsFromFixture("chat_completions/sse/response.sse"));

  it("accumulates parallel choices separately", () => {
    expect(state.blocks.find((block) => block.id === "live:choice:0")?.text).toBe("fixture tool result");
    expect(state.blocks.find((block) => block.id === "live:choice:1")?.text).toBe("alternate answer");
  });

  it("assembles the streamed tool call with parsed arguments", () => {
    const call = state.blocks.find((block) => block.id === "live:choice:0:tool:0");
    expect(call?.kind).toBe("tool_call");
    expect(call?.toolName).toBe("lookup_fixture");
    expect(call?.callId).toBe("tool_fixture_stream");
    expect(call?.arguments).toEqual({ key: "beta" });
  });

  it("reaches completed via the [DONE] sentinel", () => {
    expect(state.status).toBe("completed");
    expect(state.statusDetail).toBe("[DONE]");
  });

  it("keeps in-flight argument fragments as raw strings until they parse", () => {
    let state = initialLiveStream("chat_completions");
    state = applyStreamRecord(state, {
      ordinal: 0,
      data: JSON.stringify({ choices: [{ index: 0, delta: { tool_calls: [{ index: 0, id: "t", function: { name: "lookup", arguments: '{"key":' } }] } }] }),
      complete: true,
    });
    const partial = state.blocks.find((block) => block.id === "live:choice:0:tool:0");
    expect(partial?.rawArguments).toBe('{"key":');
    expect(partial?.arguments).toBe('{"key":');
  });
});

describe("anthropic stream deltas", () => {
  const state = feed("anthropic_messages", recordsFromFixture("anthropic_messages/sse/response.sse"));

  it("accumulates thinking and text in block order", () => {
    const thinking = state.blocks.find((block) => block.id === "live:block:0");
    expect(thinking?.kind).toBe("thinking");
    expect(thinking?.text).toBe("fixture private reasoning");
    const text = state.blocks.find((block) => block.id === "live:block:1");
    expect(text?.kind).toBe("assistant");
    expect(text?.text).toBe("Fixture stream answer.");
  });

  it("retains redacted thinking", () => {
    const redacted = state.blocks.find((block) => block.id === "live:block:2");
    expect(redacted?.kind).toBe("thinking");
    expect(redacted?.text).toBe("[redacted thinking]");
  });

  it("assembles tool_use blocks and their streamed input json", () => {
    let state = initialLiveStream("anthropic_messages");
    state = applyStreamRecord(state, { ordinal: 0, name: "content_block_start", data: JSON.stringify({ index: 3, content_block: { type: "tool_use", id: "toolu_1", name: "lookup" } }), complete: true });
    state = applyStreamRecord(state, { ordinal: 1, name: "content_block_delta", data: JSON.stringify({ index: 3, delta: { type: "input_json_delta", partial_json: '{"key":' } }), complete: true });
    state = applyStreamRecord(state, { ordinal: 2, name: "content_block_delta", data: JSON.stringify({ index: 3, delta: { type: "input_json_delta", partial_json: '"beta"}' } }), complete: true });
    const call = state.blocks.find((block) => block.id === "live:block:3");
    expect(call?.kind).toBe("tool_call");
    expect(call?.toolName).toBe("lookup");
    expect(call?.callId).toBe("toolu_1");
    expect(call?.arguments).toEqual({ key: "beta" });
  });

  it("reaches completed via message_stop", () => {
    expect(state.status).toBe("completed");
    expect(state.statusDetail).toBe("message_stop");
  });

  it("marks error events as failed", () => {
    const state = feed("anthropic_messages", [
      { ordinal: 0, name: "message_start", data: JSON.stringify({ type: "message_start" }), complete: true },
      { ordinal: 1, name: "error", data: JSON.stringify({ type: "error", error: { message: "boom" } }), complete: true },
    ]);
    expect(state.status).toBe("failed");
  });
});

describe("stream reducer semantics", () => {
  it("deduplicates records by ordinal (broker replay safe)", () => {
    const record: StreamEventRecord = { ordinal: 0, data: JSON.stringify({ choices: [{ index: 0, delta: { content: "hi" } }] }), complete: true };
    let state = initialLiveStream("chat_completions");
    state = applyStreamRecord(state, record);
    const once = state.blocks.find((block) => block.id === "live:choice:0")?.text;
    state = applyStreamRecord(state, record);
    expect(state.blocks.find((block) => block.id === "live:choice:0")?.text).toBe(once);
    expect(state.eventCount).toBe(1);
  });

  it("applies lifecycle termini from exchange events", () => {
    let state = initialLiveStream("responses");
    state = applyStreamTerminus(state, "cancelled", "dropped");
    expect(state.status).toBe("cancelled");
    state = applyStreamTerminus(state, "completed");
    expect(state.status).toBe("cancelled"); // terminal status is sticky
  });

  it("bounds the raw event log", () => {
    let state = initialLiveStream("responses");
    for (let i = 0; i < 450; i++) {
      state = applyStreamRecord(state, { ordinal: i, data: JSON.stringify({ type: "response.created", sequence_number: i }), complete: true });
    }
    expect(state.events.length).toBeLessThanOrEqual(400);
    expect(state.eventCount).toBe(450);
  });

  it("buildLiveStream finalizes complete bodies without terminal records", () => {
    const state = buildLiveStream("chat_completions", [{ ordinal: 0, data: JSON.stringify({ choices: [{ index: 0, delta: { content: "hi" } }] }), complete: true }]);
    expect(state.status).toBe("completed");
    expect(state.statusDetail).toContain("without a protocol terminal");
  });
});

describe("responses custom tool input streaming", () => {
  it("folds custom_tool_call_input deltas into the tool call arguments", () => {
    const records: StreamEventRecord[] = [
      { ordinal: 0, data: JSON.stringify({ type: "response.output_item.added", item: { id: "ctc_1", type: "custom_tool_call", call_id: "call_1", name: "exec" } }), complete: true },
      { ordinal: 1, data: JSON.stringify({ type: "response.custom_tool_call_input.delta", item_id: "ctc_1", delta: "const r" }), complete: true },
      { ordinal: 2, data: JSON.stringify({ type: "response.custom_tool_call_input.delta", item_id: "ctc_1", delta: " = await run()" }), complete: true },
      { ordinal: 3, data: JSON.stringify({ type: "response.custom_tool_call_input.done", item_id: "ctc_1", input: "const r = await run();\n" }), complete: true },
    ];
    const state = feed("responses", records);
    const call = state.blocks.find((block) => block.id === "live:ctc_1");
    expect(call?.kind).toBe("tool_call");
    expect(call?.toolName).toBe("exec");
    // The done event replaces the streamed fragments with the final input.
    expect(call?.rawArguments).toBe("const r = await run();\n");
    expect(state.blocks.filter((block) => block.kind === "unknown")).toHaveLength(0);
  });
});
