export type ContextProtocol = "responses" | "chat_completions" | "anthropic_messages" | (string & {});
export type ContextRole = "system" | "developer" | "user" | "assistant" | "tool" | (string & {});
export type ContextBlockKind =
  | "system" | "developer" | "user" | "assistant"
  | "tool_definition" | "tool_call" | "tool_result"
  | "reasoning" | "thinking" | "refusal" | "unknown";

export interface ContextContentPart {
  kind: string;
  text?: string;
  value?: unknown;
  sourcePointer: string;
  original?: unknown;
}

export interface ContextBlock {
  id: string;
  kind: ContextBlockKind;
  role?: ContextRole;
  text?: string;
  content: ContextContentPart[];
  toolName?: string;
  callId?: string;
  arguments?: unknown;
  rawArguments?: string;
  sourcePointer: string;
  original?: unknown;
  providerExtensions?: Record<string, unknown>;
  passthrough?: unknown;
}

export interface ContextDocument {
  protocol: ContextProtocol;
  blocks: ContextBlock[];
  source: unknown;
  sourceText: string;
  providerExtensions: Record<string, unknown>;
  passthrough: unknown[];
  warnings: string[];
}

const KNOWN_ROOT_KEYS: Record<string, Set<string>> = {
  responses: new Set(["model", "input", "instructions", "tools", "tool_choice", "parallel_tool_calls", "reasoning", "text", "metadata", "stream", "store", "include", "output", "id", "object", "created_at", "status", "usage", "previous_response_id", "conversation", "error"]),
  chat_completions: new Set(["model", "messages", "tools", "tool_choice", "parallel_tool_calls", "stream", "stream_options", "temperature", "top_p", "response_format", "max_tokens", "id", "object", "created", "choices", "usage", "system_fingerprint", "error"]),
  anthropic_messages: new Set(["model", "max_tokens", "system", "messages", "tools", "tool_choice", "thinking", "output_config", "metadata", "service_tier", "stream", "id", "type", "role", "content", "stop_reason", "stop_sequence", "usage", "container", "error"]),
};

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}
function textOf(value: unknown): string | undefined {
  if (typeof value === "string") return value;
  if (isRecord(value) && typeof value.text === "string") return value.text;
  return undefined;
}
function pointer(base: string, key: string | number): string {
  const token = String(key).replaceAll("~", "~0").replaceAll("/", "~1");
  return `${base}/${token}`;
}
function part(kind: string, value: unknown, sourcePointer: string): ContextContentPart {
  return { kind, text: textOf(value), value, sourcePointer, original: value };
}
function block(kind: ContextBlockKind, sourcePointer: string, original: unknown, init: Partial<ContextBlock> = {}): ContextBlock {
  return { id: `${kind}:${sourcePointer}`, kind, sourcePointer, original, content: [], ...init };
}
function roleKind(role: unknown): ContextBlockKind {
  return role === "system" || role === "developer" || role === "user" || role === "assistant" ? role : role === "tool" ? "tool_result" : "unknown";
}
function addTextOrParts(out: ContextBlock[], role: ContextBlockKind, value: unknown, sourcePointer: string, original: unknown): void {
  if (typeof value === "string") {
    out.push(block(role, sourcePointer, original, { role, text: value, content: [part("text", value, sourcePointer)] }));
    return;
  }
  if (!Array.isArray(value)) {
    out.push(block(role, sourcePointer, original, { role, text: textOf(value), content: value === undefined ? [] : [part("value", value, sourcePointer)] }));
    return;
  }
  const parent = block(role, sourcePointer, original, { role, content: [] });
  const texts: string[] = [];
  value.forEach((item, index) => {
    const itemPointer = pointer(sourcePointer, index);
    const itemKind = isRecord(item) && typeof item.type === "string" ? item.type : "value";
    parent.content.push(part(itemKind, item, itemPointer));
    const text = textOf(item);
    if (text) texts.push(text);
  });
  parent.text = texts.join("\n") || undefined;
  out.push(parent);
}
function addToolDefinition(out: ContextBlock[], tool: unknown, sourcePointer: string): void {
  const record = isRecord(tool) ? tool : {};
  const fn = isRecord(record.function) ? record.function : record;
  const name = typeof fn.name === "string" ? fn.name : undefined;
  out.push(block("tool_definition", sourcePointer, tool, {
    role: "system",
    toolName: name,
    text: name ? `Tool ${name}` : "Tool definition",
    content: [part("tool_definition", tool, sourcePointer)],
  }));
}
function addToolCall(out: ContextBlock[], call: unknown, sourcePointer: string, owner?: unknown): void {
  const record = isRecord(call) ? call : {};
  const fn = isRecord(record.function) ? record.function : record;
  const name = typeof fn.name === "string" ? fn.name : undefined;
  const raw = typeof fn.arguments === "string" ? fn.arguments : undefined;
  let args: unknown = fn.arguments;
  if (raw !== undefined) {
    try { args = JSON.parse(raw); } catch { args = raw; }
  } else if (fn.input !== undefined) args = fn.input;
  out.push(block("tool_call", sourcePointer, owner ?? call, {
    role: "assistant",
    toolName: name,
    callId: typeof record.id === "string" ? record.id : typeof record.call_id === "string" ? record.call_id : typeof record.tool_call_id === "string" ? record.tool_call_id : undefined,
    arguments: args,
    rawArguments: raw ?? (args === undefined ? undefined : JSON.stringify(args)),
    content: [part("tool_call", call, sourcePointer)],
  }));
}
function addToolResult(out: ContextBlock[], result: unknown, sourcePointer: string): void {
  const record = isRecord(result) ? result : {};
  const value = record.content ?? record.output ?? result;
  const text = typeof value === "string" ? value : JSON.stringify(value);
  out.push(block("tool_result", sourcePointer, result, {
    role: "tool",
    callId: typeof record.tool_use_id === "string" ? record.tool_use_id : typeof record.call_id === "string" ? record.call_id : typeof record.tool_call_id === "string" ? record.tool_call_id : undefined,
    text,
    content: [part("tool_result", value, sourcePointer)],
  }));
}
function addMessage(out: ContextBlock[], message: unknown, sourcePointer: string): void {
  if (!isRecord(message)) {
    out.push(block("unknown", sourcePointer, message, { passthrough: message, content: [part("unknown", message, sourcePointer)] }));
    return;
  }
  const role = message.role;
  const kind = roleKind(role);
  if (kind === "tool_result") { addToolResult(out, message, sourcePointer); return; }
  if (kind === "unknown") {
    out.push(block("unknown", sourcePointer, message, { passthrough: message, content: [part(typeof message.type === "string" ? message.type : "unknown", message, sourcePointer)] }));
    return;
  }
  if (message.content !== undefined) addTextOrParts(out, kind, message.content, pointer(sourcePointer, "content"), message);
  const calls = Array.isArray(message.tool_calls) ? message.tool_calls : undefined;
  calls?.forEach((call, index) => addToolCall(out, call, pointer(pointer(sourcePointer, "tool_calls"), index), message));
  if (typeof message.refusal === "string") out.push(block("refusal", pointer(sourcePointer, "refusal"), message.refusal, { role: "assistant", text: message.refusal, content: [part("refusal", message.refusal, pointer(sourcePointer, "refusal"))] }));
}
function addResponsesItem(out: ContextBlock[], item: unknown, sourcePointer: string): void {
  if (!isRecord(item)) { out.push(block("unknown", sourcePointer, item, { passthrough: item, content: [part("unknown", item, sourcePointer)] })); return; }
  const type = typeof item.type === "string" ? item.type : "unknown";
  if (type === "message") addTextOrParts(out, roleKind(item.role ?? "assistant"), item.content, pointer(sourcePointer, "content"), item);
  else if (type === "function_call" || type === "custom_tool_call") addToolCall(out, item, sourcePointer);
  else if (type === "function_call_output" || type === "custom_tool_call_output") addToolResult(out, item, sourcePointer);
  else if (type === "reasoning") {
    const summary = Array.isArray(item.summary) ? item.summary.map(textOf).filter(Boolean).join("\n") : textOf(item.summary);
    out.push(block("reasoning", sourcePointer, item, { role: "assistant", text: summary, content: [part("reasoning", item, sourcePointer)] }));
  } else if (type === "output_text") addTextOrParts(out, "assistant", item.text, pointer(sourcePointer, "text"), item);
  else out.push(block("unknown", sourcePointer, item, { passthrough: item, content: [part(type, item, sourcePointer)] }));
}
function addAnthropicBlock(out: ContextBlock[], item: unknown, sourcePointer: string): void {
  if (!isRecord(item)) { out.push(block("unknown", sourcePointer, item, { passthrough: item, content: [part("unknown", item, sourcePointer)] })); return; }
  switch (item.type) {
    case "text": addTextOrParts(out, "assistant", item.text, pointer(sourcePointer, "text"), item); break;
    case "thinking": out.push(block("thinking", sourcePointer, item, { role: "assistant", text: typeof item.thinking === "string" ? item.thinking : undefined, content: [part("thinking", item, sourcePointer)] })); break;
    case "redacted_thinking": out.push(block("thinking", sourcePointer, item, { role: "assistant", text: "[redacted thinking]", content: [part("redacted_thinking", item, sourcePointer)] })); break;
    case "tool_use": addToolCall(out, item, sourcePointer); break;
    case "tool_result": addToolResult(out, item, sourcePointer); break;
    default: out.push(block("unknown", sourcePointer, item, { passthrough: item, content: [part(String(item.type ?? "unknown"), item, sourcePointer)] }));
  }
}

function addAnthropicMessage(out: ContextBlock[], message: unknown, sourcePointer: string): void {
  if (!isRecord(message)) { addAnthropicBlock(out, message, sourcePointer); return; }
  const content = message.content;
  if (typeof content === "string") addTextOrParts(out, roleKind(message.role), content, pointer(sourcePointer, "content"), message);
  else if (Array.isArray(content)) content.forEach((item, index) => {
    const itemPointer = pointer(pointer(sourcePointer, "content"), index);
    if (isRecord(item) && item.type === "text") addTextOrParts(out, roleKind(message.role), item.text, pointer(itemPointer, "text"), item);
    else addAnthropicBlock(out, item, itemPointer);
  });
  else if (content !== undefined) addTextOrParts(out, roleKind(message.role), content, pointer(sourcePointer, "content"), message);
}

export function normalizeContext(protocol: ContextProtocol, sourceText: string): ContextDocument {
  let source: unknown;
  try { source = JSON.parse(sourceText); } catch (error) {
    return { protocol, blocks: [block("unknown", "", sourceText, { text: sourceText, passthrough: sourceText, content: [part("raw", sourceText, "")] })], source: sourceText, sourceText, providerExtensions: {}, passthrough: [], warnings: [`JSON parse failed: ${error instanceof Error ? error.message : String(error)}`] };
  }
  const root = isRecord(source) ? source : {};
  const blocks: ContextBlock[] = [];
  const extensions: Record<string, unknown> = {};
  const known = KNOWN_ROOT_KEYS[protocol] ?? new Set<string>();
  Object.entries(root).forEach(([key, value]) => { if (!known.has(key)) extensions[key] = value; });
  if (protocol === "responses") {
    if (typeof root.instructions === "string") addTextOrParts(blocks, "system", root.instructions, "/instructions", root.instructions);
    if (Array.isArray(root.input)) root.input.forEach((item, index) => addResponsesItem(blocks, item, pointer("/input", index)));
    else if (root.input !== undefined) addTextOrParts(blocks, "user", root.input, "/input", root.input);
    if (Array.isArray(root.tools)) root.tools.forEach((tool, index) => addToolDefinition(blocks, tool, pointer("/tools", index)));
    if (Array.isArray(root.output)) root.output.forEach((item, index) => addResponsesItem(blocks, item, pointer("/output", index)));
  } else {
    if (protocol === "anthropic_messages" && root.system !== undefined) addTextOrParts(blocks, "system", root.system, "/system", root.system);
    if (Array.isArray(root.messages)) root.messages.forEach((message, index) => protocol === "anthropic_messages" ? addAnthropicMessage(blocks, message, pointer("/messages", index)) : addMessage(blocks, message, pointer("/messages", index)));
    if (Array.isArray(root.tools)) root.tools.forEach((tool, index) => addToolDefinition(blocks, tool, pointer("/tools", index)));
    if (Array.isArray(root.choices)) root.choices.forEach((choice, index) => {
      if (isRecord(choice) && choice.message) addMessage(blocks, choice.message, pointer(pointer("/choices", index), "message"));
      else if (isRecord(choice) && isRecord(choice.delta)) addMessage(blocks, { role: "assistant", content: choice.delta.content, ...choice.delta }, pointer(pointer("/choices", index), "delta"));
      else blocks.push(block("unknown", pointer("/choices", index), choice, { passthrough: choice, content: [part("choice", choice, pointer("/choices", index))] }));
    });
    if (protocol === "anthropic_messages" && Array.isArray(root.content)) root.content.forEach((item, index) => addAnthropicBlock(blocks, item, pointer("/content", index)));
  }
  const passthrough = Object.entries(extensions).map(([key, value]) => ({ key, value, sourcePointer: pointer("", key) }));
  if (passthrough.length) blocks.push(...passthrough.map((item) => block("unknown", item.sourcePointer, item.value, { passthrough: item.value, content: [part("provider_extension", item.value, item.sourcePointer)], providerExtensions: { [item.key]: item.value } })));
  return { protocol, blocks, source, sourceText, providerExtensions: extensions, passthrough, warnings: [] };
}

export function normalizeContextFromValue(protocol: ContextProtocol, source: unknown): ContextDocument {
  return normalizeContext(protocol, JSON.stringify(source));
}
