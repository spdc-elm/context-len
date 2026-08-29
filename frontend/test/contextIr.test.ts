import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { normalizeContext } from "../src/contextIr";

describe("normalizeContext", () => {
  it("normalizes Responses request and preserves tools/extensions/pointers", () => {
    const document = normalizeContext("responses", JSON.stringify({
      model: "fixture", instructions: "Be brief", input: [
        { type: "message", role: "user", content: [{ type: "input_text", text: "hello" }] },
        { type: "function_call_output", call_id: "call-1", output: "{\"ok\":true}" },
      ], tools: [{ type: "function", name: "lookup", parameters: { type: "object" } }], provider_extension: { keep: true },
    }));
    expect(document.blocks.map((item) => item.kind)).toEqual(["system", "user", "tool_result", "tool_definition", "unknown"]);
    expect(document.blocks[1].sourcePointer).toBe("/input/0/content");
    expect(document.blocks[2].callId).toBe("call-1");
    expect(document.providerExtensions.provider_extension).toEqual({ keep: true });
    expect(document.blocks.at(-1)?.sourcePointer).toBe("/provider_extension");
  });

  it("normalizes Chat choices and tool calls without dropping alternate choices", () => {
    const document = normalizeContext("chat_completions", JSON.stringify({
      messages: [{ role: "system", content: "rules" }, { role: "assistant", tool_calls: [{ id: "t1", type: "function", function: { name: "lookup", arguments: "{\"key\":\"a\"}" } }] }],
      choices: [{ index: 0, message: { role: "assistant", content: "done" } }, { index: 1, message: { role: "assistant", content: "alternate" } }],
      unknown_response_field: { preserve: true },
    }));
    expect(document.blocks.filter((item) => item.kind === "tool_call")[0].arguments).toEqual({ key: "a" });
    expect(document.blocks.filter((item) => item.kind === "assistant").map((item) => item.text)).toEqual(["done", "alternate"]);
    expect(document.passthrough).toHaveLength(1);
  });

  it("normalizes Anthropic thinking, redacted thinking and tool blocks", () => {
    const document = normalizeContext("anthropic_messages", JSON.stringify({
      system: [{ type: "text", text: "system" }], messages: [{ role: "assistant", content: [
        { type: "thinking", thinking: "private" }, { type: "tool_use", id: "toolu-1", name: "lookup", input: { key: "a" } },
      ] }], content: [{ type: "redacted_thinking", data: "opaque" }, { type: "text", text: "answer" }, { type: "tool_use", id: "toolu-2", name: "lookup", input: {} }],
    }));
    expect(document.blocks.map((item) => item.kind)).toEqual(["system", "thinking", "tool_call", "thinking", "assistant", "tool_call"]);
    expect(document.blocks[2].rawArguments).toContain("key");
    expect(document.blocks[3].text).toBe("[redacted thinking]");
  });

  it("covers the checked-in representative JSON fixtures for all protocols", () => {
    const cases = [
      ["responses", "responses/json/request.json"], ["responses", "responses/json/response.json"],
      ["chat_completions", "chat_completions/json/request.json"], ["chat_completions", "chat_completions/json/response.json"],
      ["anthropic_messages", "anthropic_messages/json/request.json"], ["anthropic_messages", "anthropic_messages/json/response.json"],
    ] as const;
    for (const [protocol, relativePath] of cases) {
      const body = readFileSync(resolve(process.cwd(), "..", "tests", "fixtures", relativePath), "utf8");
      const document = normalizeContext(protocol, body);
      expect(document.blocks.length, relativePath).toBeGreaterThan(0);
      expect(document.sourceText).toBe(body);
      expect(document.blocks.every((item) => item.sourcePointer !== undefined), relativePath).toBe(true);
    }
  });
  it("falls back to an unknown raw block for malformed JSON", () => {
    const document = normalizeContext("responses", "{broken");
    expect(document.blocks[0].kind).toBe("unknown");
    expect(document.blocks[0].text).toBe("{broken");
    expect(document.warnings.length).toBeGreaterThan(0);
  });
});

describe("Responses custom tool items", () => {
  it("maps custom_tool_call_output to a tool result block", () => {
    const document = normalizeContext("responses", JSON.stringify({
      input: [
        { type: "custom_tool_call", id: "ctc_1", call_id: "call_1", name: "exec", input: "ls -la" },
        { type: "custom_tool_call_output", id: "ctco_1", call_id: "call_1", output: "total 0" },
      ],
    }));
    const call = document.blocks.find((item) => item.kind === "tool_call");
    expect(call?.toolName).toBe("exec");
    const result = document.blocks.find((item) => item.kind === "tool_result");
    expect(result).toBeDefined();
    expect(result?.callId).toBe("call_1");
    expect(result?.text).toContain("total 0");
  });
});
