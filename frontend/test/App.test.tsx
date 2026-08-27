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

  it("loads an artifact only after the operator asks, keeping Raw view explicit", async () => {
    const rendered = await renderApp();
    const loadButton = [...rendered.querySelectorAll("button")].find((button) => button.textContent?.includes("Load body"));
    expect(loadButton).toBeDefined();
    await act(async () => {
      loadButton?.click();
      await Promise.resolve();
    });
    expect(rendered.querySelector('[aria-label="Raw artifact body"]')?.textContent).toContain('"model"');
  });
});
