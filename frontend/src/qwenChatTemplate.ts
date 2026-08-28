import type { ContextBlock, ContextDocument } from "./contextIr";

export interface RenderedContextBlock {
  block: ContextBlock;
  text: string;
}

function safeJson(value: unknown, raw?: string): string {
  if (raw !== undefined && raw.trim()) {
    try { return JSON.stringify(JSON.parse(raw), null, 2); } catch { return raw; }
  }
  if (typeof value === "string") return value;
  try { return JSON.stringify(value, null, 2); } catch { return String(value); }
}

function blockRole(block: ContextBlock): string {
  if (block.kind === "tool_result") return "tool";
  if (block.kind === "tool_definition" || block.kind === "unknown") return "system";
  return block.role ?? block.kind;
}

function blockBody(block: ContextBlock): string {
  if (block.kind === "tool_definition") {
    const definition = block.content[0]?.value;
    return `<|tools|>\n${safeJson(definition)}\n</|tools|>`;
  }
  if (block.kind === "tool_call") {
    const name = block.toolName ?? "unknown_tool";
    const args = safeJson(block.arguments, block.rawArguments);
    return `<tool_call>\n${name}\n${args}\n</tool_call>`;
  }
  if (block.kind === "tool_result") {
    return `<tool_response>\n${block.text ?? block.content.map((item) => item.text ?? safeJson(item.value)).join("\n")}\n</tool_response>`;
  }
  if (block.text !== undefined) return block.text;
  return block.content.map((item) => item.text ?? (typeof item.value === "string" ? item.value : safeJson(item.value))).filter(Boolean).join("\n");
}

export function renderQwenBlock(block: ContextBlock): string {
  const role = blockRole(block);
  const body = blockBody(block);
  return `<|im_start|>${role}\n${body}\n<|im_end|>`;
}

export function renderQwenBlocks(document: ContextDocument): RenderedContextBlock[] {
  return document.blocks.map((block) => ({ block, text: renderQwenBlock(block) }));
}

export function renderQwenChatML(document: ContextDocument, addGenerationPrompt = false): string {
  const rendered = renderQwenBlocks(document).map((item) => item.text);
  if (addGenerationPrompt) rendered.push("<|im_start|>assistant\n");
  return rendered.join("\n");
}

export const QWEN_CHAT_TEMPLATE_NAME = "Chat Template · Qwen ChatML";
