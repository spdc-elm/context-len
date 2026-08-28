import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, describe, expect, it } from "vitest";
import { App } from "../src/App";
import { MockWorkspaceApi } from "../src/mockApi";

let root: Root | undefined;
let host: HTMLDivElement | undefined;

afterEach(() => {
  if (root && host) {
    act(() => root?.unmount());
    host.remove();
  }
  root = undefined;
  host = undefined;
});

async function renderApp(api = new MockWorkspaceApi()): Promise<HTMLDivElement> {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  await act(async () => {
    root?.render(<App api={api} />);
    await Promise.resolve();
  });
  return host;
}

describe("workbench shell", () => {
  it("renders a queue and selected exchange detail from the typed mock", async () => {
    const rendered = await renderApp();
    expect(rendered.querySelector('[aria-label="Traffic queue"]')).not.toBeNull();
    expect(rendered.textContent).toContain("Exchange queue");
    expect(rendered.textContent).toContain("/v1/responses");
    expect(rendered.textContent).toContain("Request is held");
    expect(rendered.querySelector('[role="tab"]')).not.toBeNull();
  });

  it("keeps the selected artifact when switching views", async () => {
    const rendered = await renderApp();
    const picker = rendered.querySelector('select[aria-label="Artifact"]') as HTMLSelectElement;
    expect(picker.options.length).toBeGreaterThan(1);
    const responseOption = [...picker.options].find((option) => option.value.includes("response"));
    expect(responseOption).toBeDefined();
    await act(async () => {
      picker.value = responseOption!.value;
      picker.dispatchEvent(new Event("change", { bubbles: true }));
      await Promise.resolve();
    });
    expect((rendered.querySelector('select[aria-label="Artifact"]') as HTMLSelectElement).value).toBe(responseOption!.value);
    const chatTemplate = [...rendered.querySelectorAll('[role="tab"]')].find((tab) => tab.textContent?.startsWith("Chat Template"));
    await act(async () => {
      chatTemplate?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await Promise.resolve();
    });
    expect((rendered.querySelector('select[aria-label="Artifact"]') as HTMLSelectElement).value).toBe(responseOption!.value);
  });

  it("renders Qwen ChatML as a derived continuous context stream", async () => {
    const rendered = await renderApp();
    await act(async () => { await Promise.resolve(); await Promise.resolve(); });
    const chatTemplate = [...rendered.querySelectorAll('[role="tab"]')].find((tab) => tab.textContent?.startsWith("Chat Template"));
    await act(async () => {
      chatTemplate?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(rendered.querySelector('[data-chat-template="qwen-chatml"]')).not.toBeNull();
    expect(rendered.textContent).toContain("Chat Template · Qwen ChatML");
    expect(rendered.textContent).toContain("<|im_start|>");
  });
  it("loads the selected artifact automatically", async () => {
    const rendered = await renderApp();
    await act(async () => { await Promise.resolve(); await Promise.resolve(); });
    expect(rendered.querySelector('[data-json-tree-view="true"]')?.textContent).toContain('model');
    expect(rendered.textContent).not.toContain("Load body");
    expect(rendered.textContent).not.toContain("Reload");
  });
});
