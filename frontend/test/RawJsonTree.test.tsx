import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, describe, expect, it } from "vitest";
import { RawJsonTree } from "../src/components/RawJsonTree";

let root: Root | undefined;
let host: HTMLDivElement | undefined;

function renderTree(props: React.ComponentProps<typeof RawJsonTree>): HTMLDivElement {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  act(() => {
    root?.render(<RawJsonTree {...props} />);
  });
  return host;
}

function click(element: Element | null): void {
  if (!element) throw new Error("expected an element to click");
  act(() => {
    element.dispatchEvent(new MouseEvent("click", { bubbles: true }));
  });
}

function changeInput(input: HTMLInputElement, value: string): void {
  act(() => {
    const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")?.set;
    setter?.call(input, value);
    input.dispatchEvent(new Event("input", { bubbles: true }));
    input.dispatchEvent(new Event("change", { bubbles: true }));
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

describe("RawJsonTree", () => {
  it("renders loss-aware semantic arrays, message/tool summaries, and source pointers", () => {
    const body = JSON.stringify({
      messages: [
        { role: "user", name: "alice", content: "hello" },
        { role: "assistant", type: "message", content: [{ type: "text", text: "done" }] },
      ],
      tools: [{ type: "function", function: { name: "weather", arguments: "{\"city\":\"Paris\"}" } }],
      unknown_field: { retained: false, nested: [null, 0, true] },
    });
    const rendered = renderTree({ rawBody: body });

    expect(rendered.querySelector('[data-json-tree-view="true"]')).not.toBeNull();
    expect(rendered.querySelector('[data-node-pointer="/messages"]')).not.toBeNull();
    expect(rendered.querySelector('[data-node-pointer="/messages/0"]')).not.toBeNull();
    expect(rendered.textContent).toContain("2 items");
    expect(rendered.textContent).toContain("role=user");
    expect(rendered.textContent).toContain("name=alice");
    expect(rendered.textContent).toContain("content=\"hello\"");
    expect(rendered.textContent).toContain("type=function");
    expect(rendered.textContent).toContain("name=weather");
    expect(rendered.textContent).toContain("/unknown_field");
    expect(rendered.textContent).toContain("retained");
    expect(rendered.textContent).toContain("false");
    expect(rendered.textContent).toContain("/messages/0");

    // A default tree is a projection: parsing/rendering never rewrites body bytes.
    expect(body).toBe(JSON.stringify({
      messages: [
        { role: "user", name: "alice", content: "hello" },
        { role: "assistant", type: "message", content: [{ type: "text", text: "done" }] },
      ],
      tools: [{ type: "function", function: { name: "weather", arguments: "{\"city\":\"Paris\"}" } }],
      unknown_field: { retained: false, nested: [null, 0, true] },
    }));
  });

  it("shows a count for a semantic or root array", () => {
    const rendered = renderTree({ node: [{ role: "user" }, { role: "tool" }] });
    const root = rendered.querySelector('[data-node-pointer=""]');
    expect(root?.querySelector('[data-array-count="true"]')?.textContent).toBe("2 items");
    expect(rendered.textContent).toContain("role=user");
  });
  it("supports per-node toggles, collapse/expand all, and depth control", () => {
    const rendered = renderTree({
      node: { outer: { middle: { deep: { answer: 42 } } } },
      initialExpandDepth: 0,
    });

    expect(rendered.querySelector('[data-node-pointer=""]')?.getAttribute("data-node-expanded")).toBe("true");
    expect(rendered.querySelector('[data-node-pointer="/outer"]')).not.toBeNull();
    expect(rendered.querySelector('[data-node-pointer="/outer/middle"]')).toBeNull();

    const depth = rendered.querySelector('input[aria-label="Auto expand depth"]') as HTMLInputElement;
    changeInput(depth, "3");
    expect(rendered.querySelector('[data-node-pointer="/outer/middle/deep"]')).not.toBeNull();

    click(rendered.querySelector('button[aria-label="Collapse /outer"]'));
    expect(rendered.querySelector('[data-node-pointer="/outer/middle"]')).toBeNull();

    // The dedicated action remains available even when a child is collapsed.
    click(rendered.querySelector('button[aria-label="Expand all"]'));
    expect(rendered.querySelector('[data-node-pointer="/outer/middle/deep"]')).not.toBeNull();
    click(rendered.querySelector('button[aria-label="Collapse all"]'));
    expect(rendered.querySelector('[data-node-pointer=""]')?.getAttribute("data-node-expanded")).toBe("false");
    expect(rendered.querySelector('[data-node-pointer="/outer"]')).toBeNull();
    click(rendered.querySelector('button[aria-label="Expand all"]'));
    expect(rendered.querySelector('[data-node-pointer="/outer/middle/deep"]')).not.toBeNull();
  });

  it("searches collapsed paths, auto-expands ancestors, and highlights matches", () => {
    const rendered = renderTree({
      node: { branch: { deeper: { target: "needle value" } }, untouched: "other" },
      initialExpandDepth: 0,
    });
    expect(rendered.querySelector('[data-node-pointer="/branch/deeper/target"]')).toBeNull();

    const search = rendered.querySelector('input[aria-label="Search raw JSON"]') as HTMLInputElement;
    changeInput(search, "needle");

    expect(rendered.querySelector('[data-node-pointer="/branch"]')?.getAttribute("data-node-expanded")).toBe("true");
    expect(rendered.querySelector('[data-node-pointer="/branch/deeper"]')).not.toBeNull();
    expect(rendered.querySelector('[data-node-pointer="/branch/deeper/target"]')).not.toBeNull();
    expect(rendered.querySelector('mark[data-search-match="true"]')?.textContent?.toLocaleLowerCase()).toBe("needle");

    changeInput(search, "");
    expect(rendered.querySelector('[data-node-pointer="/branch/deeper/target"]')).toBeNull();
  });

  it("opens a long string when the match is inside its collapsed value", () => {
    const long = `${"x".repeat(190)}needle at the end`;
    const rendered = renderTree({ node: { payload: long }, initialExpandDepth: 1 });
    const payload = rendered.querySelector('[data-node-pointer="/payload"]');
    expect(payload?.getAttribute("data-node-expanded")).toBe("false");
    const search = rendered.querySelector('input[aria-label="Search raw JSON"]') as HTMLInputElement;
    changeInput(search, "needle at the end");
    expect(payload?.getAttribute("data-node-expanded")).toBe("true");
    expect(rendered.querySelector('[data-long-string="true"]')?.textContent).toContain("needle at the end");
  });
  it("falls back to immutable plain text when JSON parsing fails", () => {
    const raw = "event: message\ndata: {not json}\n\n";
    const rendered = renderTree({ rawBody: raw });
    const fallback = rendered.querySelector('[data-raw-json-fallback="text"]');
    expect(fallback).not.toBeNull();
    expect(fallback?.textContent).toContain(raw);
    expect(rendered.querySelector('[data-json-tree-view="true"]')).toBeNull();
  });

  it("accepts a supplied node and encodes source JSON pointer tokens", () => {
    const supplied = { "a/b": { "tilde~key": "kept" } };
    const rendered = renderTree({ node: supplied, sourceJsonPointer: "/payload", initialExpandDepth: 0 });
    expect(rendered.querySelector('[data-node-pointer="/payload"]')?.getAttribute("data-node-expanded")).toBe("true");
    expect(rendered.querySelector('[data-node-pointer="/payload/a~1b"]')).not.toBeNull();
    expect(rendered.querySelector('[data-node-pointer="/payload/a~1b/tilde~0key"]')).toBeNull();
    click(rendered.querySelector('button[aria-label="Expand /payload/a~1b"]'));
    expect(rendered.querySelector('[data-node-pointer="/payload/a~1b/tilde~0key"]')).not.toBeNull();
    expect(supplied).toEqual({ "a/b": { "tilde~key": "kept" } });
  });
});
