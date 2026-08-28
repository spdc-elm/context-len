import { useEffect, useMemo, useState } from "react";
import type { ArtifactRef } from "../contracts";
import { normalizeContext, type ContextBlock } from "../contextIr";
import {
  QWEN_CHAT_TEMPLATE_NAME,
  QWEN_DEFAULT_SYSTEM,
  QWEN_TOOL_CALL_INSTRUCTION,
  qwenBlockRole,
  renderQwenBlocks,
} from "../qwenChatTemplate";
import { loadQwenScopeRegistry, scopeByName } from "../templateScopes";

interface ChatTemplateViewProps {
  protocol: string;
  body?: string;
  artifact?: ArtifactRef;
}

type Segment =
  | { kind: "text"; id: string; text: string }
  | { kind: "json"; id: string; value: unknown }
  | {
      kind: "tag";
      id: string;
      name: string;
      marker?: boolean;
      blockKind?: string;
      pointer?: string;
      defaultOpen?: boolean;
      fromTemplate?: boolean;
      children: Segment[];
    };

type TagSegment = Extract<Segment, { kind: "tag" }>;

/** Closing tags are assembled at runtime so this file contains no literal
 * tool-call terminator sequences; the rendered text is identical. */
function tagFooter(name: string, marker: boolean | undefined): string {
  if (marker) return "<|im_end|>";
  return "</" + name + ">";
}

function tagHeader(name: string, marker: boolean | undefined): string {
  if (marker) return `<|im_start|>${name}`;
  return `<${name}>`;
}

export function Chevron({ open, onToggle, label }: { open: boolean; onToggle: () => void; label: string }) {
  return (
    <button type="button" className="chevron" aria-label={label} aria-expanded={open} onClick={onToggle}>
      <svg width="8" height="8" viewBox="0 0 8 8" aria-hidden="true" focusable="false">
        <path
          d={open ? "M1.5 3 L4 5.5 L6.5 3" : "M2.5 1.5 L5.5 4 L2.5 6.5"}
          stroke="currentColor"
          strokeWidth="1.3"
          fill="none"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
      </svg>
    </button>
  );
}

function parseMaybeJson(text: string | undefined): unknown {
  if (text === undefined) return "";
  try {
    return JSON.parse(text) as unknown;
  } catch {
    return text;
  }
}

const SCOPE_TAG_RE = /<([A-Za-z_][A-Za-z0-9_.:-]*)>/;
const MAX_PARSE_DEPTH = 8;
const MAX_SCOPES_PER_TEXT = 200;

/** Find the index of the close tag matching the scope opened at `from`,
 * counting nested same-name opens, or -1 when unbalanced. */
function findScopeClose(text: string, name: string, from: number): number {
  const open = `<${name}>`;
  const close = "</" + name + ">";
  let depth = 1;
  let cursor = from;
  while (cursor <= text.length) {
    const nextOpen = text.indexOf(open, cursor);
    const nextClose = text.indexOf(close, cursor);
    if (nextClose === -1) return -1;
    if (nextOpen !== -1 && nextOpen < nextClose) {
      depth += 1;
      cursor = nextOpen + open.length;
    } else {
      depth -= 1;
      if (depth === 0) return nextClose;
      cursor = nextClose + close.length;
    }
  }
  return -1;
}

/**
 * Split message text into text and nested-scope segments by detecting any
 * balanced `<tag>...</tag>` pair. This is a display-only heuristic: it covers
 * scopes the Qwen template defines (tools / tool_call / tool_response / think)
 * and the XML conventions providers and prompt authors embed in content
 * (Anthropic-style prompt XML, custom tags). Unbalanced or self-closing tags
 * stay literal text; nothing is rewritten back to the artifact.
 */
export function parseContentScopes(id: string, text: string, depth = 0): Segment[] {
  if (depth > MAX_PARSE_DEPTH) return [{ kind: "text", id: `${id}/overflow`, text }];
  const segments: Segment[] = [];
  let rest = text;
  let budget = MAX_SCOPES_PER_TEXT;
  while (rest.length > 0 && budget > 0) {
    budget -= 1;
    const match = rest.match(SCOPE_TAG_RE);
    if (!match || match.index === undefined) {
      segments.push({ kind: "text", id: `${id}/t${segments.length}`, text: rest });
      return segments;
    }
    const start = match.index;
    const name = match[1];
    const contentStart = start + match[0].length;
    if (start > 0) segments.push({ kind: "text", id: `${id}/t${segments.length}`, text: rest.slice(0, start) });
    const closeIndex = findScopeClose(rest, name, contentStart);
    if (closeIndex === -1) {
      // Unbalanced open: keep the literal tag in the text stream.
      segments.push({ kind: "text", id: `${id}/t${segments.length}`, text: rest.slice(start, contentStart) });
      rest = rest.slice(contentStart);
      continue;
    }
    const content = rest.slice(contentStart, closeIndex);
    segments.push({
      kind: "tag",
      id: `${id}/${name}/${start}`,
      name,
      children: parseContentScopes(`${id}/${name}/${start}`, content, depth + 1),
    });
    rest = rest.slice(closeIndex + name.length + 3);
  }
  if (rest.length > 0) segments.push({ kind: "text", id: `${id}/tail`, text: rest });
  return segments;
}

function textOrScopes(id: string, text: string): Segment[] {
  if (!text) return [];
  return parseContentScopes(id, text);
}

function blockToSegment(block: ContextBlock, index: number): TagSegment {
  const registry = loadQwenScopeRegistry();
  const role = qwenBlockRole(block);
  const children: Segment[] = [];
  if (block.kind === "tool_definition") {
    children.push(...textOrScopes(`${block.id}/system`, block.text || QWEN_DEFAULT_SYSTEM));
    const toolsName = scopeByName(registry, "tools")?.name ?? "tools";
    children.push({
      kind: "tag",
      id: `${block.id}/tools`,
      name: toolsName,
      fromTemplate: true,
      children: block.content.map((part, partIndex) => ({
        kind: "json" as const,
        id: `${block.id}/tool/${partIndex}`,
        value: part.value,
      })),
    });
    children.push(...textOrScopes(`${block.id}/instruction`, QWEN_TOOL_CALL_INSTRUCTION));
  } else if (block.kind === "tool_call") {
    const callName = scopeByName(registry, "tool_call")?.name ?? "tool_call";
    children.push({
      kind: "tag",
      id: `${block.id}/call`,
      name: callName,
      fromTemplate: true,
      children: [
        {
          kind: "json",
          id: `${block.id}/call/json`,
          value: { name: block.toolName ?? "unknown_tool", arguments: block.arguments },
        },
      ],
    });
  } else if (block.kind === "tool_result") {
    const responseName = scopeByName(registry, "tool_response")?.name ?? "tool_response";
    children.push({
      kind: "tag",
      id: `${block.id}/result`,
      name: responseName,
      fromTemplate: true,
      children: [{ kind: "json", id: `${block.id}/result/json`, value: parseMaybeJson(block.text) }],
    });
  } else if (block.kind === "reasoning" || block.kind === "thinking") {
    const thinkScope = scopeByName(registry, "think");
    const inner: Segment[] = textOrScopes(`${block.id}/think/text`, block.text ?? "");
    if (thinkScope) {
      children.push({
        kind: "tag",
        id: `${block.id}/think`,
        name: thinkScope.name,
        fromTemplate: true,
        defaultOpen: false,
        children: inner,
      });
    } else {
      children.push(...inner);
    }
  } else if (block.text !== undefined && block.text !== "") {
    children.push(...textOrScopes(`${block.id}/text`, block.text));
  } else {
    children.push(
      ...block.content.map((part, partIndex) => ({
        kind: "json" as const,
        id: `${block.id}/part/${partIndex}`,
        value: part.value,
      })),
    );
  }
  return {
    kind: "tag",
    id: `block/${index}:${block.id}`,
    name: role,
    marker: true,
    blockKind: block.kind,
    pointer: block.sourcePointer,
    children,
  };
}

interface CollapseProps {
  collapsed: Record<string, boolean>;
  onToggle: (id: string, defaultOpen: boolean) => void;
}

function JsonNode({
  id,
  value,
  depth,
  collapsed,
  onToggle,
}: { id: string; value: unknown; depth: number } & CollapseProps) {
  const isContainer = value !== null && typeof value === "object";
  if (!isContainer) {
    const text = typeof value === "string" ? JSON.stringify(value) : String(value);
    return (
      <span className={`ctx-json-leaf ctx-json-${value === null ? "null" : typeof value}`}>{text}</span>
    );
  }
  const isArray = Array.isArray(value);
  const entries: Array<[string, unknown]> = isArray
    ? (value as unknown[]).map((item, index) => [String(index), item])
    : Object.entries(value as Record<string, unknown>);
  const open = collapsed[id] ?? depth <= 3;
  const brace = isArray ? "[" : "{";
  const close = isArray ? "]" : "}";
  const label = `Toggle JSON ${brace}${id}`;
  if (!open) {
    return (
      <span className="ctx-json-inline">
        <Chevron open={false} onToggle={() => onToggle(id, depth <= 3)} label={label} />
        <code className="ctx-json-brace">
          {brace}
          {entries.length}
          {close}
        </code>
      </span>
    );
  }
  return (
    <div className="ctx-json-node">
      <span className="ctx-json-head">
        <Chevron open onToggle={() => onToggle(id, depth <= 3)} label={label} />
        <code className="ctx-json-brace">{brace}</code>
      </span>
      <div className="ctx-json-children">
        {entries.map(([key, child]) => (
          <div className="ctx-json-row" key={key}>
            <span className="ctx-json-key">{key}:</span>
            <JsonNode id={`${id}/${key}`} value={child} depth={depth + 1} collapsed={collapsed} onToggle={onToggle} />
          </div>
        ))}
      </div>
      <div className="ctx-json-close">
        <code className="ctx-json-brace">{close}</code>
      </div>
    </div>
  );
}

function SegmentList({ segments, depth, collapsed, onToggle }: { segments: Segment[]; depth: number } & CollapseProps) {
  return (
    <>
      {segments.map((child) =>
        child.kind === "text" ? (
          <div className="ctx-text" key={child.id}>
            {child.text}
          </div>
        ) : child.kind === "json" ? (
          <div className="ctx-json" key={child.id}>
            <JsonNode id={child.id} value={child.value} depth={depth + 1} collapsed={collapsed} onToggle={onToggle} />
          </div>
        ) : (
          <TagView key={child.id} segment={child} depth={depth + 1} collapsed={collapsed} onToggle={onToggle} />
        ),
      )}
    </>
  );
}

function TagView({ segment, depth, collapsed, onToggle }: { segment: TagSegment; depth: number } & CollapseProps) {
  const open = collapsed[segment.id] ?? segment.defaultOpen ?? true;
  const header = tagHeader(segment.name, segment.marker);
  const footer = tagFooter(segment.name, segment.marker);
  const headerClass = segment.marker ? "ctx-marker" : "ctx-inner-tag";
  const scopeClass = segment.marker ? "" : segment.fromTemplate ? " ctx-template-scope" : " ctx-content-scope";
  return (
    <div
      className={`ctx-tag${segment.marker ? " ctx-block" : ""}${scopeClass}${segment.blockKind ? ` chat-template-${segment.blockKind}` : ""}`}
      data-ctx-tag={segment.name}
      data-ctx-marker={segment.marker ? "chatml" : undefined}
      data-source-json-pointer={segment.pointer}
    >
      <div className="ctx-tag-line">
        <Chevron open={open} onToggle={() => onToggle(segment.id, segment.defaultOpen ?? true)} label={`Toggle ${header}`} />
        <code className={headerClass}>{header}</code>
        {!open && (
          <>
            <span className="ctx-ellipsis">…</span>
            <code className={headerClass}>{footer}</code>
          </>
        )}
      </div>
      {open && (
        <>
          <div className="ctx-tag-body">
            <SegmentList segments={segment.children} depth={depth} collapsed={collapsed} onToggle={onToggle} />
          </div>
          <div className="ctx-tag-line">
            <span className="ctx-chevron-spacer" aria-hidden="true" />
            <code className={headerClass}>{footer}</code>
          </div>
        </>
      )}
    </div>
  );
}

export function ChatTemplateView({ protocol, body, artifact }: ChatTemplateViewProps) {
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>({});
  const document = useMemo(() => normalizeContext(protocol, body ?? ""), [protocol, body]);
  const blocks = useMemo(() => renderQwenBlocks(document), [document]);
  const segments = useMemo(() => blocks.map(({ block }, index) => blockToSegment(block, index)), [blocks]);
  const isSse =
    artifact?.content_type.includes("event-stream") || /^\s*(?:event|data|id|retry):/m.test(body ?? "");

  useEffect(() => {
    setCollapsed({});
  }, [body]);

  if (body === undefined) {
    return (
      <div className="body-placeholder">Load an artifact to render the derived Qwen ChatML context.</div>
    );
  }
  if (isSse) {
    return (
      <section className="chat-template-view" aria-label={QWEN_CHAT_TEMPLATE_NAME} data-chat-template="qwen-chatml">
        <div className="chat-template-heading">
          <strong>{QWEN_CHAT_TEMPLATE_NAME}</strong>
          <span>SSE response · open SSE for raw events</span>
        </div>
        <div className="body-placeholder">
          This response artifact is an SSE stream. Its typed Chat Template delta projection is part of the SSE phase.
        </div>
      </section>
    );
  }

  const toggle = (id: string, defaultOpen: boolean) =>
    setCollapsed((current) => ({ ...current, [id]: !(current[id] ?? defaultOpen) }));

  return (
    <section className="chat-template-view" aria-label={QWEN_CHAT_TEMPLATE_NAME} data-chat-template="qwen-chatml">
      <div className="chat-template-heading">
        <strong>{QWEN_CHAT_TEMPLATE_NAME}</strong>
        <span>{document.blocks.length} blocks</span>
      </div>
      {document.warnings.length > 0 && (
        <div className="warning-box">
          {document.warnings.map((warning) => (
            <p key={warning}>{warning}</p>
          ))}
        </div>
      )}
      <div className="chat-template-stream">
        <SegmentList segments={segments} depth={-1} collapsed={collapsed} onToggle={toggle} />
      </div>
    </section>
  );
}

export default ChatTemplateView;
