import { describe, expect, it } from "vitest";
import { loadQwenScopeRegistry, parseTemplateScopes } from "../src/templateScopes";

describe("template scope registry", () => {
  it("derives the Qwen closed-scope grammar from the vendored templates", () => {
    const registry = loadQwenScopeRegistry();
    const names = registry.scopes.map((scope) => scope.name);
    expect(names).toContain("tools");
    expect(names).toContain("tool_call");
    expect(names).toContain("tool_response");
    expect(names).toContain("think");
    expect(registry.segment.open).toBe("<|im_start|>");
    expect(registry.segment.close).toBe("<|im_end|>");
    expect(registry.templateNames).toEqual(["Qwen2.5-7B-Instruct", "Qwen3-8B"]);
    const toolCall = registry.scopes.find((scope) => scope.name === "tool_call");
    expect(toolCall?.open).toBe("<tool_call>");
    expect(toolCall?.close).toBe("</" + "tool_call>");
    expect(toolCall?.templates.length).toBeGreaterThan(0);
  });

  it("only keeps balanced tag pairs and skips templates without ChatML markers", () => {
    const registry = parseTemplateScopes({
      paired: "<|im_start|>x<alpha>1</alpha><beta>2...<|im_end|>",
      unmatched: "<|im_start|>x<gamma>1</gamma><delta>2...<|im_end|>",
    });
    const names = registry.scopes.map((scope) => scope.name);
    expect(names).toContain("alpha");
    expect(names).toContain("gamma");
    expect(names).not.toContain("beta"); // open without close stays literal
    expect(names).not.toContain("delta");
    const empty = parseTemplateScopes({ other: "<a>1</a>" });
    expect(empty.scopes).toEqual([]);
  });
});
