import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, describe, expect, it } from "vitest";
import { ChatTemplateView } from "../src/components/ChatTemplateView";

const TOOL_CALL_CLOSE = "</" + "tool_call>";

const chatRequest = JSON.stringify({
  messages: [
    { role: "system", content: "You are precise." },
    { role: "user", content: "Call the tool." },
    {
      role: "assistant",
      content: null,
      tool_calls: [
        { id: "t1", type: "function", function: { name: "lookup", arguments: "{\"key\":\"alpha\"}" } },
      ],
    },
    { role: "tool", tool_call_id: "t1", content: "{\"value\":\"fixture\"}" },
  ],
  tools: [
    {
      type: "function",
      function: {
        name: "lookup",
        parameters: { type: "object", properties: { key: { type: "string" } } },
      },
    },
  ],
});

const anthropicResponse = JSON.stringify({
  type: "message",
  role: "assistant",
  content: [
    { type: "thinking", thinking: "private reasoning", signature: "sig" },
    { type: "text", text: "Fixture answer." },
  ],
});

let root: Root | undefined;
let host: HTMLDivElement | undefined;

function renderView(props: React.ComponentProps<typeof ChatTemplateView>): HTMLDivElement {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  act(() => {
    root?.render(<ChatTemplateView {...props} />);
  });
  return host;
}

function click(element: Element | null): void {
  if (!element) throw new Error("expected an element to click");
  act(() => {
    element.dispatchEvent(new MouseEvent("click", { bubbles: true }));
  });
}

afterEach(() => {
  if (root && host) {
    act(() => root?.unmount());
    host.remove();
  }
  root = undefined;
  host = undefined;
});

describe("ChatTemplateView", () => {
  it("renders nested ChatML markers, tool tags and collapsible JSON", () => {
    const rendered = renderView({ protocol: "chat_completions", body: chatRequest });
    expect(rendered.textContent).toContain("<|im_start|>system");
    expect(rendered.textContent).toContain("<tools>");
    expect(rendered.textContent).toContain("You are precise.");
    expect(rendered.querySelector('[data-ctx-tag="tools"]')).not.toBeNull();
    expect(rendered.querySelector('[data-ctx-tag="tool_call"].ctx-template-scope')?.textContent).toContain("lookup");
    expect(rendered.querySelector('[data-ctx-tag="tool_call"].ctx-template-scope')?.textContent).toContain("alpha");
    expect(rendered.querySelector('[data-ctx-tag="tool_response"].ctx-template-scope')?.textContent).toContain("value");
    expect(rendered.textContent).toContain("<|im_end|>");
    expect(rendered.querySelector("button.chevron")).not.toBeNull();
    expect(rendered.querySelector('[data-source-json-pointer="/messages/2/tool_calls/0"]')).not.toBeNull();
  });

  it("collapses nested tag regions behind a subtle chevron", () => {
    const rendered = renderView({ protocol: "chat_completions", body: chatRequest });
    const callTag = rendered.querySelector('[data-ctx-tag="tool_call"].ctx-template-scope');
    expect(callTag?.textContent).toContain("alpha");
    click(callTag?.querySelector("button.chevron") ?? null);
    const collapsedTag = rendered.querySelector('[data-ctx-tag="tool_call"].ctx-template-scope');
    expect(collapsedTag?.textContent).not.toContain("alpha");
    expect(collapsedTag?.textContent).toContain(TOOL_CALL_CLOSE);
    click(collapsedTag?.querySelector("button.chevron") ?? null);
    expect(rendered.querySelector('[data-ctx-tag="tool_call"].ctx-template-scope')?.textContent).toContain("alpha");
  });

  it("collapses JSON containers independently of their tag", () => {
    const rendered = renderView({ protocol: "chat_completions", body: chatRequest });
    const jsonHead = rendered.querySelector('[data-ctx-tag="tool_call"].ctx-template-scope .ctx-json-head button.chevron');
    click(jsonHead);
    const callTag = rendered.querySelector('[data-ctx-tag="tool_call"].ctx-template-scope');
    expect(callTag?.textContent).not.toContain("alpha");
    expect(callTag?.textContent).toContain("{2}");
  });

  it("keeps thinking blocks collapsed by default for Anthropic responses", () => {
    const rendered = renderView({ protocol: "anthropic_messages", body: anthropicResponse });
    const think = rendered.querySelector('[data-ctx-tag="think"]');
    expect(think).not.toBeNull();
    expect(think?.textContent).not.toContain("private reasoning");
    click(think?.querySelector("button.chevron") ?? null);
    expect(rendered.querySelector('[data-ctx-tag="think"]')?.textContent).toContain("private reasoning");
  });

  it("renders XML scopes embedded in message content", () => {
    const request = JSON.stringify({
      messages: [
        {
          role: "user",
          content:
            "Use <reference><name>docs</name><url>https://example.invalid</url></reference> when answering.",
        },
      ],
    });
    const rendered = renderView({ protocol: "chat_completions", body: request });
    const reference = rendered.querySelector('[data-ctx-tag="reference"]');
    expect(reference).not.toBeNull();
    expect(reference?.textContent).toContain("docs");
    expect(reference?.querySelector('[data-ctx-tag="name"]')).not.toBeNull();
    expect(reference?.querySelector('[data-ctx-tag="url"]')).not.toBeNull();
    click(reference?.querySelector("button.chevron") ?? null);
    const collapsed = rendered.querySelector('[data-ctx-tag="reference"]');
    expect(collapsed?.textContent).not.toContain("docs");
    expect(collapsed?.textContent).toContain("</" + "reference>");
  });

  it("keeps unbalanced angle-bracket text literal", () => {
    const request = JSON.stringify({ messages: [{ role: "user", content: "a < b and <orphan> stays text" }] });
    const rendered = renderView({ protocol: "chat_completions", body: request });
    expect(rendered.querySelector('[data-ctx-tag="orphan"]')).toBeNull();
    expect(rendered.textContent).toContain("<orphan>");
  });

  it("compacts newlines around scopes and keeps inline tags on one line", () => {
    const request = JSON.stringify({
      messages: [{ role: "user", content: "Line1\n<foo>bar</foo>\nLine2\n<baz>\nmulti\nline\n</baz>\ntail" }],
    });
    const rendered = renderView({ protocol: "chat_completions", body: request });
    const foo = rendered.querySelector('[data-ctx-tag="foo"]');
    expect(foo?.querySelector(".ctx-inline-text")?.textContent).toBe("bar");
    const texts = [...rendered.querySelectorAll(".ctx-text")].map((e) => e.textContent);
    expect(texts).toContain("Line1");
    expect(texts).toContain("Line2");
    expect(texts).toContain("tail");
    expect(texts.every((t) => t.trim() !== "")).toBe(true);
  });

  it("renders JSON embedded in string values without escape noise", () => {
    const request = JSON.stringify({
      messages: [
        { role: "user", content: [{ type: "tool_result", tool_use_id: "t", content: [{ type: "text", text: "{\"ok\":true}" }] }] },
      ],
    });
    const rendered = renderView({ protocol: "anthropic_messages", body: request });
    const result = rendered.querySelector('[data-ctx-tag="tool_response"].ctx-template-scope');
    expect(result?.textContent).toContain("ok:");
    expect(result?.textContent).toContain("true");
    expect(result?.textContent.includes(String.fromCharCode(92, 34))).toBe(false);
  });

  it("colors ChatML blocks by role", () => {
    const rendered = renderView({ protocol: "chat_completions", body: chatRequest });
    expect(rendered.querySelector(".ctx-block.chat-template-tool_definition")).not.toBeNull();
    expect(rendered.querySelector(".ctx-block.chat-template-user")).not.toBeNull();
    expect(rendered.querySelector(".ctx-block.chat-template-assistant")).not.toBeNull();
    const plain = renderView({ protocol: "chat_completions", body: JSON.stringify({ messages: [{ role: "system", content: "s" }] }) });
    expect(plain.querySelector(".ctx-block.chat-template-system")).not.toBeNull();
  });

  it("renders multi-line strings as a collapsible text block", () => {
    const request = JSON.stringify({
      messages: [{ role: "user", content: "call the tool" }],
      tools: [{ type: "function", function: { name: "lookup", description: "line1\nline2\nline3", parameters: {} } }],
    });
    const rendered = renderView({ protocol: "chat_completions", body: request });
    const collapsed = rendered.querySelector(".ctx-json-text");
    expect(collapsed).not.toBeNull();
    expect(collapsed?.querySelector(".ctx-json-text-block")).toBeNull();
    expect(collapsed?.textContent).toContain("line1");
    click(collapsed?.querySelector("button.chevron") ?? null);
    const block = rendered.querySelector(".ctx-json-text-block");
    expect(block).not.toBeNull();
    expect(block?.textContent).toContain("line1\nline2");
  });

  it("renders SSE artifacts as typed live blocks via the stream reducer", () => {
    const rendered = renderView({
      protocol: "chat_completions",
      body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi \"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"there\"}}]}\n\ndata: [DONE]\n\n",
      artifact: {
        artifact_id: "a",
        stage: "response.upstream",
        direction: "response",
        content_type: "text/event-stream",
        content_encoding: "",
        size: 10,
        sha256: "0".repeat(64),
        complete: true,
        storage_ref: "mem://a",
      },
    });
    expect(rendered.textContent).toContain("response · completed · [DONE] · 3 events");
    expect(rendered.textContent).toContain("hi there");
    expect(rendered.textContent).not.toContain("SSE phase");
  });

  it("appends live stream blocks after the request context while streaming", () => {
    const rendered = renderView({
      protocol: "chat_completions",
      body: JSON.stringify({ model: "m", messages: [{ role: "user", content: "hi" }] }),
      live: {
        protocol: "chat_completions",
        status: "streaming",
        statusDetail: undefined,
        nextOrdinal: 1,
        blocks: [
          {
            id: "live:choice:0",
            kind: "assistant",
            role: "assistant",
            text: "growing answer",
            content: [],
            sourcePointer: "/events/0",
          },
        ],
        events: [{ ordinal: 0, data: "{\"delta\":\"growing\"}" }],
        eventCount: 1,
      },
    });
    expect(rendered.querySelector(".chat-template-view")?.getAttribute("data-live")).toBe("true");
    expect(rendered.querySelector(".chat-live-chip")?.textContent).toContain("response · streaming · 1 events");
    expect(rendered.textContent).toContain("hi");
    expect(rendered.textContent).toContain("growing answer");
  });
});
