import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, describe, expect, it } from "vitest";
import { TrafficQueue } from "../src/components/TrafficQueue";
import type { ExchangeSnapshot } from "../src/contracts";

function snapshot(overrides: Partial<ExchangeSnapshot> & Pick<ExchangeSnapshot, "exchange_id" | "protocol" | "state">): ExchangeSnapshot {
  return {
    request: {
      envelope: { method: "POST", path: "/v1/chat/completions", escaped_path: "/v1/chat/completions", raw_query: "", headers: {} },
      artifact_refs: [],
    },
    response: { envelope: { status: 200, headers: {}, trailers: {} }, artifact_refs: [] },
    policy: { request_gate: "pass", response_gate: "pass" },
    warnings: [],
    created_at: "2026-08-27T10:00:00.000Z",
    updated_at: "2026-08-27T10:00:01.000Z",
    ...overrides,
  } as ExchangeSnapshot;
}

const chatExchange = snapshot({
  exchange_id: "ex-chat",
  protocol: "chat_completions",
  state: "completed",
  summary: { model: "qwen3-235b", message_count: 18, preview: "Check the deadlock", tool_names: ["bash", "read"], context_tokens: 52300 },
  session: { session_id: "sess-chat", depth: 1, position: "pos-chat", root: true },
});

const responsesExchange = snapshot({
  exchange_id: "ex-responses",
  protocol: "responses",
  state: "upstream_running",
  summary: { model: "gpt-mock", message_count: 2 },
  session: { session_id: "sess-responses", depth: 1, position: "pos-responses", root: true },
});

const otherExchange = snapshot({
  exchange_id: "ex-models",
  protocol: "unknown",
  state: "completed",
  request: {
    envelope: { method: "GET", path: "/v1/models", escaped_path: "/v1/models", raw_query: "", headers: {} },
    artifact_refs: [],
  },
});

describe("TrafficQueue", () => {
  let container: HTMLElement;
  let root: Root;

  afterEach(() => {
    act(() => root.unmount());
    container.remove();
  });

  function render(exchanges: ExchangeSnapshot[]) {
    container = document.createElement("div");
    document.body.appendChild(container);
    root = createRoot(container);
    act(() => {
      root.render(<TrafficQueue exchanges={exchanges} selectedExchangeId={exchanges[0]?.exchange_id} onSelect={() => {}} />);
    });
  }

  function switchView(label: string) {
    const button = [...container.querySelectorAll("button")].find((item) => item.textContent?.trim() === label);
    if (!button) throw new Error(`view toggle ${label} not found`);
    act(() => {
      button.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    });
  }

  function setFilter(label: string, value: string) {
    const control = container.querySelector(`[aria-label="${label}"]`) as HTMLInputElement | HTMLSelectElement | null;
    if (!control) throw new Error(`filter ${label} not found`);
    act(() => {
      const setter = Object.getOwnPropertyDescriptor(control instanceof HTMLInputElement ? HTMLInputElement.prototype : HTMLSelectElement.prototype, "value")!;
      setter.set!.call(control, value);
      control.dispatchEvent(new Event("input", { bubbles: true }));
      control.dispatchEvent(new Event("change", { bubbles: true }));
    });
  }

  it("groups conversation exchanges into session rows by default", () => {
    render([chatExchange]);
    expect(container.textContent).toContain("qwen3-235b");
    expect(container.textContent).toContain("Check the deadlock");
    expect(container.textContent).toContain("1 turn");
    expect(container.textContent).not.toContain("/v1/chat/completions");
  });

  it("shows summary fields instead of the URL for conversation protocols", () => {
    render([chatExchange]);
    switchView("Exchanges");
    expect(container.textContent).toContain("qwen3-235b");
    expect(container.textContent).toContain("18 msgs");
    expect(container.textContent).toContain("52.3k ctx");
    expect(container.textContent).toContain("Check the deadlock");
    expect(container.textContent).not.toContain("/v1/chat/completions");
  });

  it("keeps the method and path as identity for non-conversation traffic", () => {
    render([otherExchange]);
    switchView("Exchanges");
    expect(container.textContent).toContain("GET");
    expect(container.textContent).toContain("/v1/models");
  });

  it("renders an explicit dash when the upstream reported no usage", () => {
    render([responsesExchange]);
    switchView("Exchanges");
    expect(container.textContent).toContain("— ctx");
  });

  it("narrows the queue by text, protocol, state, and model filters", () => {
    render([chatExchange, responsesExchange, otherExchange]);
    switchView("Exchanges");
    setFilter("Filter exchanges by text", "deadlock");
    expect(container.querySelectorAll(".traffic-row")).toHaveLength(1);
    expect(container.textContent).toContain("Check the deadlock");

    // The id is not shown in the row but still searchable.
    setFilter("Filter exchanges by text", "ex-responses");
    expect(container.querySelectorAll(".traffic-row")).toHaveLength(1);
    expect(container.textContent).toContain("gpt-mock");

    setFilter("Filter exchanges by text", "");
    setFilter("Filter by protocol", "responses");
    expect(container.querySelectorAll(".traffic-row")).toHaveLength(1);
    expect(container.textContent).toContain("gpt-mock");

    setFilter("Filter by protocol", "all");
    setFilter("Filter by state", "upstream_running");
    expect(container.querySelectorAll(".traffic-row")).toHaveLength(1);

    setFilter("Filter by state", "all");
    setFilter("Filter by model", "qwen3-235b");
    expect(container.querySelectorAll(".traffic-row")).toHaveLength(1);

    setFilter("Filter by model", "all");
    setFilter("Filter exchanges by text", "no-such-thing");
    expect(container.textContent).toContain("No exchanges match the current filters.");
  });
});
