import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, describe, expect, it, vi } from "vitest";
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
    expect(rendered.textContent).toContain("MEM ");
    expect(rendered.textContent).toContain("DISK ");
    expect(rendered.querySelector('[role="tab"]')).not.toBeNull();
  });

  it("renders an intentional ready state before the first exchange", async () => {
    class EmptyWorkspaceApi extends MockWorkspaceApi {
      override async listExchanges(): Promise<never[]> {
        return [];
      }
    }
    const rendered = await renderApp(new EmptyWorkspaceApi());
    expect(rendered.querySelector(".empty-detail-grid")).not.toBeNull();
    expect(rendered.querySelector(".empty-detail-hero h1")?.textContent).toBe("See what your model sees.");
    expect(rendered.querySelector(".empty-detail-status-card h2")?.textContent).toContain("Waiting for your first request");
    expect(rendered.querySelector(".empty-detail-flow")).not.toBeNull();
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

  it("allows passthrough to enter capture while a gate is held", async () => {
    class HeldWorkspaceApi extends MockWorkspaceApi {
      override async getPolicy(): Promise<{ request_gate: "hold"; response_gate: "pass" }> {
        return { request_gate: "hold", response_gate: "pass" };
      }
    }
    const api = new HeldWorkspaceApi();
    const rendered = await renderApp(api);
    const toggle = rendered.querySelector('button[role="switch"][aria-label="Capture mode: passthrough"]') as HTMLButtonElement;
    expect(toggle.disabled).toBe(false);
    await act(async () => { toggle.click(); await Promise.resolve(); });
    expect(api).toBeDefined();
    expect(rendered.querySelector('button[aria-label="Capture mode: capture"]')).not.toBeNull();
  });

  it("surfaces capture API failures in the workspace error banner", async () => {
    class FailingCaptureApi extends MockWorkspaceApi {
      override async setCaptureMode(): Promise<never> { throw new Error("capture unavailable"); }
    }
    const rendered = await renderApp(new FailingCaptureApi());
    const toggle = rendered.querySelector('button[role="switch"][aria-label="Capture mode: passthrough"]') as HTMLButtonElement;
    await act(async () => { toggle.click(); await Promise.resolve(); });
    expect(rendered.querySelector('[role="alert"]')?.textContent).toContain("capture unavailable");
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

describe("split view", () => {
  const originalWidth = window.innerWidth;
  const originalHeight = window.innerHeight;

  function setViewport(width: number, height: number): void {
    Object.defineProperty(window, "innerWidth", { configurable: true, writable: true, value: width });
    Object.defineProperty(window, "innerHeight", { configurable: true, writable: true, value: height });
  }

  // The jsdom storage global is a broken stub in this environment; the
  // component guards its own access, and tests only clear best-effort.
  function clearSplitPref(): void {
    try {
      window.localStorage.removeItem("context-lens-split");
    } catch {
      // storage unavailable
    }
  }

  afterEach(() => {
    setViewport(originalWidth, originalHeight);
    clearSplitPref();
  });

  it("shows raw and chat template side by side on wide viewports", async () => {
    setViewport(1600, 900);
    clearSplitPref();
    const rendered = await renderApp();
    await act(async () => { await Promise.resolve(); await Promise.resolve(); });
    expect(rendered.querySelector('[data-split-view="true"]')).not.toBeNull();
    const panes = [...rendered.querySelectorAll("[data-split-view=true] .viewer-pane")];
    expect(panes[0]?.querySelector('[data-chat-template="qwen-chatml"]')).not.toBeNull();
    expect(panes[1]?.querySelector(".raw-json-tree")).not.toBeNull();
  });

  it("falls back to a single column on narrow viewports", async () => {
    setViewport(900, 800);
    clearSplitPref();
    const rendered = await renderApp();
    await act(async () => { await Promise.resolve(); await Promise.resolve(); });
    expect(rendered.querySelector('[data-split-view="true"]')).toBeNull();
    const splitButton = rendered.querySelector(".layout-option.split") as HTMLButtonElement;
    expect(splitButton.disabled).toBe(true);
  });

  it("exits split when a tab is clicked and re-enters via the toggle", async () => {
    setViewport(1600, 900);
    clearSplitPref();
    const rendered = await renderApp();
    await act(async () => { await Promise.resolve(); await Promise.resolve(); });
    expect(rendered.querySelector('[data-split-view="true"]')).not.toBeNull();
    const sseTab = [...rendered.querySelectorAll('[role="tab"]')].find((tab) => tab.textContent?.startsWith("SSE"));
    await act(async () => {
      sseTab?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await Promise.resolve();
    });
    expect(rendered.querySelector('[data-split-view="true"]')).toBeNull();
    const splitButton = rendered.querySelector(".layout-option.split") as HTMLButtonElement;
    await act(async () => {
      splitButton.dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await Promise.resolve();
    });
    expect(rendered.querySelector('[data-split-view="true"]')).not.toBeNull();
  });

  it("adapts when the viewport becomes narrow", async () => {
    setViewport(1600, 900);
    clearSplitPref();
    const rendered = await renderApp();
    await act(async () => { await Promise.resolve(); await Promise.resolve(); });
    expect(rendered.querySelector('[data-split-view="true"]')).not.toBeNull();
    setViewport(800, 900);
    await act(async () => {
      window.dispatchEvent(new Event("resize"));
      await Promise.resolve();
    });
    expect(rendered.querySelector('[data-split-view="true"]')).toBeNull();
  });

  it("supports dragging the divider to resize panes", async () => {
    setViewport(1600, 900);
    clearSplitPref();
    const rendered = await renderApp();
    const getRectSpy = vi
      .spyOn(HTMLElement.prototype, "getBoundingClientRect")
      .mockReturnValue({ width: 1000 } as DOMRect);
    const divider = rendered.querySelector(".pane-divider") as HTMLElement;
    await act(async () => {
      divider.dispatchEvent(new MouseEvent("mousedown", { bubbles: true, clientX: 500 }));
      await Promise.resolve();
    });
    await act(async () => {
      window.dispatchEvent(new MouseEvent("mousemove", { clientX: 700 }));
      await Promise.resolve();
    });
    await act(async () => {
      window.dispatchEvent(new MouseEvent("mouseup"));
      await Promise.resolve();
    });
    const left = rendered.querySelector(".pane-left") as HTMLElement;
    expect(left.style.flexBasis).toBe("70%");
    expect(document.body.classList.contains("pane-resizing")).toBe(false);
    getRectSpy.mockRestore();
  });
});
