import type { ContextBlock, ContextDocument } from "./contextIr";

export interface RenderedContextBlock {
  block: ContextBlock;
  text: string;
}

export const QWEN_CHAT_TEMPLATE_NAME = "Chat Template · Qwen ChatML";
export const QWEN_DEFAULT_SYSTEM = "You are Qwen, created by Alibaba Cloud. You are a helpful assistant.";
export const QWEN_TOOLS_HEADER =
  "# Tools\n\nYou may call one or more functions to assist with the user query.\n\nYou are provided with function signatures within <tools></tools> XML tags:";

// Closing tag literals are assembled from parts so source files stay plain
// ASCII-safe and easy to grep; behaviour is identical to a single literal.
const TOOL_CALL_CLOSE = "</" + "tool_call>";
const TOOL_RESPONSE_OPEN = "<" + "tool_response>";
const TOOL_RESPONSE_CLOSE = "</" + "tool_response>";

export const QWEN_TOOL_CALL_INSTRUCTION =
  "For each function call, return a json object with function name and arguments within " +
  "<" + "tool_call>" + " XML tags:\n" +
  "<" + "tool_call>" + '\n{"name": <function-name>, "arguments": <args-json-object>}\n' +
  TOOL_CALL_CLOSE;

function safeJson(value: unknown, raw?: string): string {
  if (raw !== undefined && raw.trim()) {
    try {
      return JSON.stringify(JSON.parse(raw), null, 2);
    } catch {
      return raw;
    }
  }
  if (typeof value === "string") return value;
  try {
    return JSON.stringify(value, null, 2);
  } catch {
    return String(value);
  }
}

/** The ChatML role a context block serializes into. Mirrors the official Qwen
 * templates: tool results ride in a user segment, and tool definitions plus
 * unknown passthrough live in the system segment. */
export function qwenBlockRole(block: ContextBlock): string {
  if (block.kind === "tool_result") return "user";
  if (block.kind === "tool_definition" || block.kind === "unknown") return "system";
  return block.role ?? block.kind;
}

function blockBody(block: ContextBlock): string {
  if (block.kind === "tool_definition") {
    const definitions = block.content.map((item) => item.value);
    return `${block.text || QWEN_DEFAULT_SYSTEM}\n\n${QWEN_TOOLS_HEADER}\n<tools>\n${definitions
      .map((definition) => safeJson(definition))
      .join("\n")}\n</tools>\n\n${QWEN_TOOL_CALL_INSTRUCTION}`;
  }
  if (block.kind === "tool_call") {
    const name = block.toolName ?? "unknown_tool";
    const args = safeJson(block.arguments, block.rawArguments);
    return `<${"tool_call"}>\n{"name": ${JSON.stringify(name)}, "arguments": ${args}}\n${TOOL_CALL_CLOSE}`;
  }
  if (block.kind === "tool_result") {
    const content =
      block.text ?? block.content.map((item) => item.text ?? safeJson(item.value)).join("\n");
    return `${TOOL_RESPONSE_OPEN}\n${content}\n${TOOL_RESPONSE_CLOSE}`;
  }
  if (block.text !== undefined) return block.text;
  return block.content
    .map((item) => item.text ?? (typeof item.value === "string" ? item.value : safeJson(item.value)))
    .filter(Boolean)
    .join("\n");
}

export function renderQwenBlock(block: ContextBlock): string {
  const role = qwenBlockRole(block);
  const body = blockBody(block);
  return `<|im_start|>${role}\n${body}\n<|im_end|>`;
}

export function renderQwenBlocks(document: ContextDocument): RenderedContextBlock[] {
  const definitions = document.blocks.filter((block) => block.kind === "tool_definition");
  const ordinary = document.blocks.filter((block) => block.kind !== "tool_definition");
  const rendered: RenderedContextBlock[] = [];
  if (definitions.length > 0) {
    const system = ordinary[0]?.kind === "system" ? ordinary[0] : undefined;
    const merged: ContextBlock = {
      ...definitions[0],
      id: "tool_definitions:/tools",
      sourcePointer: "/tools",
      text: system?.text,
      content: definitions.flatMap((block) => block.content),
      original: definitions.map((block) => block.original),
    };
    rendered.push({ block: merged, text: renderQwenBlock(merged) });
    if (system) ordinary.splice(ordinary.indexOf(system), 1);
  }
  return [...rendered, ...ordinary.map((block) => ({ block, text: renderQwenBlock(block) }))];
}

export function renderQwenChatML(document: ContextDocument, addGenerationPrompt = false): string {
  const rendered = renderQwenBlocks(document).map((item) => item.text);
  if (addGenerationPrompt) rendered.push("<|im_start|>assistant\n");
  return rendered.join("\n");
}
