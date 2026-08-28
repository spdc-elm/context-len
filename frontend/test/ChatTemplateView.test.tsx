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
    expect(rendered.querySelector('[data-ctx-tag="tool_call"]')?.textContent).toContain("lookup");
    expect(rendered.querySelector('[data-ctx-tag="tool_call"]')?.textContent).toContain("alpha");
    expect(rendered.querySelector('[data-ctx-tag="tool_response"]')?.textContent).toContain("value");
    expect(rendered.textContent).toContain("<|im_end|>");
    expect(rendered.querySelector("button.chevron")).not.toBeNull();
    expect(rendered.querySelector('[data-source-json-pointer="/messages/2/tool_calls/0"]')).not.toBeNull();
  });

  it("collapses nested tag regions behind a subtle chevron", () => {
    const rendered = renderView({ protocol: "chat_completions", body: chatRequest });
    const callTag = rendered.querySelector('[data-ctx-tag="tool_call"]');
    expect(callTag?.textContent).toContain("alpha");
    click(rendered.querySelector('[aria-label^="Toggle <tool_call"]'));
    const collapsedTag = rendered.querySelector('[data-ctx-tag="tool_call"]');
    expect(collapsedTag?.textContent).not.toContain("alpha");
    expect(collapsedTag?.textContent).toContain(TOOL_CALL_CLOSE);
    click(rendered.querySelector('[aria-label^="Toggle <tool_call"]'));
    expect(rendered.querySelector('[data-ctx-tag="tool_call"]')?.textContent).toContain("alpha");
  });

  it("collapses JSON containers independently of their tag", () => {
    const rendered = renderView({ protocol: "chat_completions", body: chatRequest });
    const jsonHead = rendered.querySelector('[data-ctx-tag="tool_call"] .ctx-json-head button.chevron');
    click(jsonHead);
    const callTag = rendered.querySelector('[data-ctx-tag="tool_call"]');
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

  it("shows the SSE phase placeholder for event-stream artifacts", () => {
    const rendered = renderView({
      protocol: "chat_completions",
      body: "event: message\ndata: {\"delta\":\"hi\"}\n\n",
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
    expect(rendered.textContent).toContain("SSE response");
  });
});
