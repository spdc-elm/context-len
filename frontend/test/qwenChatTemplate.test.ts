import { describe, expect, it } from "vitest";
import { normalizeContext } from "../src/contextIr";
import { QWEN_CHAT_TEMPLATE_NAME, renderQwenBlocks, renderQwenChatML } from "../src/qwenChatTemplate";

describe("Qwen ChatML renderer", () => {
  it("renders a continuous marker stream for semantic blocks", () => {
    const document = normalizeContext("responses", JSON.stringify({ instructions: "rules", input: [{ type: "message", role: "user", content: "hello" }], tools: [{ type: "function", name: "lookup", parameters: {} }], output: [{ type: "message", role: "assistant", content: [{ type: "output_text", text: "hi" }] }] }));
    const rendered = renderQwenChatML(document);
    expect(rendered.indexOf("# Tools")).toBeGreaterThanOrEqual(0);
    expect(rendered.indexOf("# Tools")).toBeLessThan(rendered.indexOf("<|im_start|>user"));
    expect(rendered).toContain("<|im_start|>system\n");
    expect(rendered).toContain("<|im_start|>user\nhello");
    expect(rendered).toContain("<|im_start|>assistant\nhi\n<|im_end|>");
    expect(rendered).not.toContain("accuracy");
  });

  it("formats tool arguments while retaining the source block", () => {
    const document = normalizeContext("chat_completions", JSON.stringify({ messages: [{ role: "assistant", tool_calls: [{ id: "t", type: "function", function: { name: "lookup", arguments: "{\"key\":\"alpha\"}" } }] }] }));
    const blocks = renderQwenBlocks(document);
    expect(blocks[1]?.text ?? blocks[0].text).toContain("<tool_call>");
    expect(renderQwenChatML(document)).toContain('"key": "alpha"');
    expect(document.blocks.find((item) => item.kind === "tool_call")?.rawArguments).toBe('{"key":"alpha"}');
  });

  it("supports a generation prompt and exposes the fixed template name", () => {
    const document = normalizeContext("anthropic_messages", JSON.stringify({ messages: [{ role: "user", content: "hello" }] }));
    expect(renderQwenChatML(document, true).endsWith("<|im_start|>assistant\n")).toBe(true);
    expect(QWEN_CHAT_TEMPLATE_NAME).toBe("Chat Template · Qwen ChatML");
  });
});
