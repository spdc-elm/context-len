import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import type { ArtifactRef } from "../contracts";
import { normalizeContext, type ContextBlock, type ContextDocument } from "../contextIr";
import type { MergedSession, MergedTurn } from "../mergedSession";
import { formatTokens } from "../format";
import {
  QWEN_CHAT_TEMPLATE_NAME,
  QWEN_DEFAULT_SYSTEM,
  QWEN_TOOL_CALL_INSTRUCTION,
  qwenBlockRole,
  renderQwenBlock,
  renderQwenBlocks,
  type RenderedContextBlock,
} from "../qwenChatTemplate";
import { buildLiveStream, parseSseRecords, type LiveStreamState, type StreamStatus } from "../streamIr";
import { loadQwenScopeRegistry, scopeByName } from "../templateScopes";

interface ChatTemplateViewProps {
  protocol: string;
  body?: string;
  artifact?: ArtifactRef;
  /** Live response stream while the SSE body is still flowing. */
  live?: LiveStreamState;
  /** Merged session lineage; when present the session scope renders it. */
  turns?: MergedSession;
  selectedExchangeId?: string;
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

/** Strings that are themselves JSON objects/arrays render as nested JSON
 * instead of an escaped one-liner; other strings display their raw content. */
function parseJsonContainer(text: string): unknown {
  try {
    const parsed = JSON.parse(text) as unknown;
    return parsed !== null && typeof parsed === "object" ? parsed : undefined;
  } catch {
    return undefined;
  }
}

const SCOPE_TAG_RE = /<([A-Za-z_][A-Za-z0-9_.:-]*)>/;
const MAX_PARSE_DEPTH = 8;
const MAX_SCOPES_PER_TEXT = 200;
const INLINE_TEXT_LIMIT = 80;

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
    if (start > 0) {
      // The tag renders on its own line; drop the newlines that only
      // separated it from the preceding text so no blank lines appear.
      const before = rest.slice(0, start).replace(/\n+$/, "");
      if (before) segments.push({ kind: "text", id: `${id}/t${segments.length}`, text: before });
    }
    const closeIndex = findScopeClose(rest, name, contentStart);
    if (closeIndex === -1) {
      // Unbalanced open: keep the literal tag in the text stream.
      segments.push({ kind: "text", id: `${id}/t${segments.length}`, text: rest.slice(start, contentStart) });
      rest = rest.slice(contentStart);
      continue;
    }
    const content = rest.slice(contentStart, closeIndex).replace(/^\n+/, "").replace(/\n+$/, "");
    segments.push({
      kind: "tag",
      id: `${id}/${name}/${start}`,
      name,
      children: parseContentScopes(`${id}/${name}/${start}`, content, depth + 1),
    });
    rest = rest.slice(closeIndex + name.length + 3).replace(/^\n+/, "");
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
    defaultOpen: true,
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
    if (typeof value === "string") {
      const nested = parseJsonContainer(value);
      if (nested !== undefined) {
        return <JsonNode id={`${id}~`} value={nested} depth={0} collapsed={collapsed} onToggle={onToggle} />;
      }
      if (value.includes("\n")) {
        // Multi-line strings render as a collapsible text block (like the Raw
        // view's long-string handling) so `\n` reads as real line breaks.
        const open = collapsed[id] ?? false;
        const label = `Toggle text ${id}`;
        return (
          <span className="ctx-json-text">
            <Chevron open={open} onToggle={() => onToggle(id, false)} label={label} />
            {open ? (
              <pre className="ctx-json-text-block">{value}</pre>
            ) : (
              <code className="ctx-json-leaf ctx-json-string" onClick={() => onToggle(id, false)} role="button" tabIndex={0}>{JSON.stringify(value.slice(0, 60))}{value.length > 60 ? "…" : ""}</code>
            )}
          </span>
        );
      }
      return <span className="ctx-json-leaf ctx-json-string">"{value}"</span>;
    }
    return <span className={`ctx-json-leaf ctx-json-${value === null ? "null" : typeof value}`}>{String(value)}</span>;
  }
  const isArray = Array.isArray(value);
  const entries: Array<[string, unknown]> = isArray
    ? (value as unknown[]).map((item, index) => [String(index), item])
    : Object.entries(value as Record<string, unknown>);
  const open = collapsed[id] ?? false;
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
  const defaultOpen = segment.defaultOpen ?? (segment.marker ? true : false);
  const open = collapsed[segment.id] ?? defaultOpen;
  const header = tagHeader(segment.name, segment.marker);
  const footer = tagFooter(segment.name, segment.marker);
  const headerClass = segment.marker ? "ctx-marker" : "ctx-inner-tag";
  const scopeClass = segment.marker ? "" : segment.fromTemplate ? " ctx-template-scope" : " ctx-content-scope";
  const only = segment.children.length === 1 ? segment.children[0] : undefined;
  const inlineText =
    only !== undefined && only.kind === "text" && !only.text.includes("\n") && only.text.length <= INLINE_TEXT_LIMIT
      ? only.text
      : undefined;
  // Tags that were inline in the source (single short text child) stay on one
  // line instead of being blown up into header/body/footer lines.
  const inlineLeaf = !segment.marker && (segment.children.length === 0 || inlineText !== undefined);
  return (
    <div
      className={`ctx-tag${segment.marker ? " ctx-block" : ""}${scopeClass}${segment.blockKind ? ` chat-template-${segment.blockKind}` : ""}`}
      data-ctx-tag={segment.name}
      data-ctx-marker={segment.marker ? "chatml" : undefined}
      data-source-json-pointer={segment.pointer}
    >
      <div className="ctx-tag-line">
        <Chevron open={open} onToggle={() => onToggle(segment.id, defaultOpen)} label={`Toggle ${header}`} />
        <code className={headerClass}>{header}</code>
        {!open && (
          <>
            <span className="ctx-ellipsis">…</span>
            <code className={headerClass}>{footer}</code>
          </>
        )}
        {open && inlineLeaf && (
          <>
            {inlineText !== undefined && <span className="ctx-text ctx-inline-text">{inlineText}</span>}
            <code className={headerClass}>{footer}</code>
          </>
        )}
      </div>
      {open && !inlineLeaf && (
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

function findScrollParent(element: HTMLElement | null): HTMLElement | null {
  let parent = element?.parentElement ?? null;
  while (parent) {
    const style = window.getComputedStyle(parent);
    if (style.overflowY === "auto" || style.overflowY === "scroll" || style.overflow === "auto" || style.overflow === "scroll") return parent;
    parent = parent.parentElement;
  }
  return null;
}

function streamChip(status: StreamStatus, detail: string | undefined, eventCount: number) {
  return (
    <span className={`chat-live-chip chat-live-${status}`}>
      {status === "streaming" ? <i className="chat-live-dot" aria-hidden="true" /> : null}
      response · {status}
      {detail ? ` · ${detail}` : ""} · {eventCount} events
    </span>
  );
}

function namespaceBlocks(blocks: ContextBlock[], namespace: string): ContextBlock[] {
  return blocks.map((block) => ({ ...block, id: `${namespace}:${block.id}` }));
}

function documentForBlocks(protocol: string, blocks: ContextBlock[]): ContextDocument {
  return { protocol, blocks, source: undefined, sourceText: "", providerExtensions: {}, passthrough: [], warnings: [] };
}

function jsonNodeIds(id: string, value: unknown): string[] {
  if (typeof value === "string") {
    const nested = parseJsonContainer(value);
    return nested === undefined ? [] : jsonNodeIds(`${id}~`, nested);
  }
  if (value === null || typeof value !== "object") return [];
  const entries = Array.isArray(value)
    ? value.map((item, index) => [String(index), item] as const)
    : Object.entries(value as Record<string, unknown>);
  return [id, ...entries.flatMap(([key, child]) => jsonNodeIds(`${id}/${key}`, child))];
}

function segmentIds(segments: Segment[]): string[] {
  return segments.flatMap((segment) => {
    if (segment.kind === "tag") return [segment.id, ...segmentIds(segment.children)];
    if (segment.kind === "json") return jsonNodeIds(segment.id, segment.value);
    return [];
  });
}

function defaultOpenForSegment(segment: TagSegment): boolean {
  return segment.defaultOpen ?? (segment.marker ? true : false);
}

function TurnBoundary({ turn, isLast, streaming }: { turn: MergedTurn; isLast: boolean; streaming: boolean }) {
  const model = turn.exchange.summary?.model;
  const tokens = turn.exchange.summary?.context_tokens;
  return (
    <div className={`chat-turn-boundary${isLast ? " chat-turn-latest" : ""}`} data-turn-depth={turn.depth}>
      <span className="chat-turn-badge">T{turn.depth}</span>
      {model ? <span className="chat-turn-model">{model}</span> : null}
      <span className="chat-turn-ctx">{formatTokens(tokens)} ctx</span>
      {turn.markers.map((marker) => (
        <span className="chat-turn-marker" key={marker}>{marker}</span>
      ))}
      {streaming ? (
        <span className="chat-live-chip chat-live-streaming">
          <i className="chat-live-dot" aria-hidden="true" />
          response · streaming
        </span>
      ) : null}
    </div>
  );
}

export function ChatTemplateView({ protocol, body, artifact, live, turns, selectedExchangeId }: ChatTemplateViewProps) {
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>({});
  const [scope, setScope] = useState<"session" | "request">("session");
  // Unknown passthrough blocks stay hidden by default: they are noise from
  // unrecognized provider events, kept available behind an explicit toggle
  // instead of drowning the conversation. The choice is display-only.
  const [showUnknown, setShowUnknown] = useState(false);
  const [toolbarCollapsed, setToolbarCollapsed] = useState(false);
  const viewRef = useRef<HTMLElement | null>(null);
  const scrollParentRef = useRef<HTMLElement | null>(null);
  const followLatestRef = useRef(true);
  const autoScrollingRef = useRef(false);
  const document = useMemo(() => normalizeContext(protocol, body ?? ""), [protocol, body]);
  const isSse =
    artifact?.content_type.includes("event-stream") || /^\s*(?:event|data|id|retry):/m.test(body ?? "");
  // A complete SSE artifact folds through the same live reducer that powers
  // realtime updates, so the finished view and the live view agree by
  // construction instead of by duplicated logic.
  const streamFromBody = useMemo(
    () => (isSse && body !== undefined ? buildLiveStream(protocol, parseSseRecords(body)) : undefined),
    [isSse, body, protocol],
  );
  const activeStream = streamFromBody ?? live;
  const useSessionScope = scope === "session" && (turns?.turns.length ?? 0) > 0;

  // Unknown blocks hidden per exchange: the default is hidden, and switching
  // exchanges starts a fresh reading session with the same default.
  useEffect(() => {
    setShowUnknown(false);
    setToolbarCollapsed(false);
  }, [selectedExchangeId]);

  const sessionSegments = useMemo(() => {
    if (!useSessionScope || !turns) return [];
    const out: Segment[][] = [];
    turns.turns.forEach((turn, index) => {
      const namespace = `t${index}`;
      const rawContext = turn.contextDocument
        ? turn.contextDocument.blocks.map((block) => ({ ...block, id: `${namespace}:${block.id}` }))
        : namespaceBlocks(turn.contextBlocks, namespace);
      const contextBlocks = showUnknown ? rawContext : rawContext.filter((block) => block.kind !== "unknown");
      const rendered: RenderedContextBlock[] = [];
      rendered.push(...renderQwenBlocks(documentForBlocks(protocol, contextBlocks)));
      const responseBlocks = turn.responseStream
        ? turn.responseStream.blocks
        : turn.responseBlocks;
      const visibleResponse = showUnknown ? responseBlocks : responseBlocks.filter((block) => block.kind !== "unknown");
      rendered.push(...renderQwenBlocks(documentForBlocks(protocol, namespaceBlocks(visibleResponse, namespace))));
      out.push(rendered.map(({ block }, blockIndex) => blockToSegment(block, blockIndex)));
    });
    return out;
  }, [useSessionScope, turns, protocol, showUnknown]);

  const blocks = useMemo<RenderedContextBlock[]>(() => {
    if (useSessionScope) return [];
    if (streamFromBody) {
      return renderQwenBlocks({ ...document, blocks: streamFromBody.blocks });
    }
    const base = renderQwenBlocks(document);
    if (!live || live.blocks.length === 0) return base;
    // The live response appends to the request context: the operator watches
    // the assistant's reply grow at the end of the context the model saw.
    return [...base, ...live.blocks.map((block) => ({ block, text: renderQwenBlock(block) }))];
  }, [document, live, streamFromBody, useSessionScope]);
  const visibleBlocks = useMemo(
    () => (showUnknown ? blocks : blocks.filter(({ block }) => block.kind !== "unknown")),
    [blocks, showUnknown],
  );
  const segments = useMemo(() => visibleBlocks.map(({ block }, index) => blockToSegment(block, index)), [visibleBlocks]);

  // The unknown count covers everything the active scope would render before
  // filtering, so the toggle always reports what is being hidden.
  const unknownCount = useMemo(() => {
    if (useSessionScope && turns) {
      return turns.turns.reduce((sum, turn) => {
        const context = turn.contextDocument ? turn.contextDocument.blocks : turn.contextBlocks;
        const response = turn.responseStream ? turn.responseStream.blocks : turn.responseBlocks;
        return sum + context.filter((block) => block.kind === "unknown").length + response.filter((block) => block.kind === "unknown").length;
      }, 0);
    }
    const source = streamFromBody ? streamFromBody.blocks : [...document.blocks, ...(live?.blocks ?? [])];
    return source.filter((block) => block.kind === "unknown").length;
  }, [useSessionScope, turns, streamFromBody, document, live]);

  const segmentGroups = useMemo(
    () => (useSessionScope ? sessionSegments : [segments]),
    [useSessionScope, sessionSegments, segments],
  );
  const allSegmentIds = useMemo(
    () => [...new Set(segmentGroups.flatMap((group) => segmentIds(group)))],
    [segmentGroups],
  );
  const outerSegments = useMemo(
    () => segmentGroups.flatMap((group) => group.filter((segment): segment is TagSegment => segment.kind === "tag")),
    [segmentGroups],
  );
  const allOuterClosed = outerSegments.length > 0 && outerSegments.every((segment) => {
    const open = collapsed[segment.id] ?? defaultOpenForSegment(segment);
    return !open;
  });
  const toggleAll = () => {
    const nextOpen = allOuterClosed;
    setCollapsed(Object.fromEntries(allSegmentIds.map((id) => [id, nextOpen])));
  };

  useLayoutEffect(() => {
    setCollapsed({});
    // Switching artifacts starts a fresh reading session. The next layout
    // pass will place the Chat Template at the end of that artifact.
    followLatestRef.current = true;
  }, [body, useSessionScope ? turns?.turns.length : 0]);

  const lastStream = useSessionScope && turns ? turns.turns[turns.turns.length - 1].responseStream : undefined;

  useEffect(() => {
    const container = findScrollParent(viewRef.current);
    scrollParentRef.current = container;
    if (!container) return undefined;
    const onScroll = () => {
      if (autoScrollingRef.current) return;
      const distanceFromBottom = container.scrollHeight - container.clientHeight - container.scrollTop;
      // A user who leaves the bottom owns the viewport. New stream blocks may
      // grow below it, but must never pull this reading position downward.
      followLatestRef.current = distanceFromBottom <= 20;
    };
    container.addEventListener("scroll", onScroll, { passive: true });
    return () => container.removeEventListener("scroll", onScroll);
  }, [body, isSse, useSessionScope]);

  useLayoutEffect(() => {
    const container = scrollParentRef.current ?? findScrollParent(viewRef.current);
    if (!container || !followLatestRef.current) return;
    autoScrollingRef.current = true;
    container.scrollTop = container.scrollHeight;
    // ScrollTop assignment is synchronous; release the guard after any scroll
    // notification queued by the browser so the initial pin is not interpreted
    // as a user's manual scroll.
    queueMicrotask(() => { autoScrollingRef.current = false; });
  }, [body, isSse, live?.eventCount, streamFromBody?.eventCount, segments.length, sessionSegments.length, lastStream?.eventCount]);

  if (body === undefined && !useSessionScope) {
    return (
      <div className="body-placeholder">Load an artifact to render the derived Qwen ChatML context.</div>
    );
  }

  const toggle = (id: string, defaultOpen: boolean) =>
    setCollapsed((current) => ({ ...current, [id]: !(current[id] ?? defaultOpen) }));

  const streaming = activeStream?.status === "streaming" || lastStream?.status === "streaming";
  const turnCount = turns?.turns.length ?? 0;

  return (
    <section
      className="chat-template-view"
      ref={viewRef}
      aria-label={QWEN_CHAT_TEMPLATE_NAME}
      data-chat-template="qwen-chatml"
      data-live={streaming ? "true" : undefined}
    >
      <div className={`chat-template-heading${toolbarCollapsed ? " chat-template-heading-collapsed" : ""}`} role="toolbar" aria-label="Chat Template toolbar">
        <button
          type="button"
          className="chat-toolbar-toggle"
          aria-expanded={!toolbarCollapsed}
          aria-label={toolbarCollapsed ? "Expand Chat Template toolbar" : "Collapse Chat Template toolbar"}
          title={toolbarCollapsed ? "Expand toolbar" : "Collapse toolbar"}
          onClick={() => setToolbarCollapsed((current) => !current)}
        >
          <span className="chat-toolbar-caret" aria-hidden="true" />
        </button>
        <strong>{QWEN_CHAT_TEMPLATE_NAME}</strong>
        {!toolbarCollapsed && <>
          {turns && turnCount > 0 ? (
            <div className="chat-scope-toggle" role="group" aria-label="Chat template scope">
              <button
                type="button"
                className={`chat-scope-button ${useSessionScope ? "active" : ""}`}
                aria-pressed={useSessionScope}
                onClick={() => setScope("session")}
              >
                Session · {turnCount} {turnCount === 1 ? "turn" : "turns"}
              </button>
              <button
                type="button"
                className={`chat-scope-button ${!useSessionScope ? "active" : ""}`}
                aria-pressed={!useSessionScope}
                onClick={() => setScope("request")}
              >
                Request
              </button>
            </div>
          ) : null}
          {!useSessionScope && activeStream && activeStream.eventCount > 0 ? (
            streamChip(activeStream.status, activeStream.statusDetail, activeStream.eventCount)
          ) : (
            !useSessionScope ? <span>{visibleBlocks.length} blocks</span> : null
          )}
          {unknownCount > 0 ? (
            <button
              type="button"
              className={`chat-unknown-toggle ${showUnknown ? "active" : ""}`}
              aria-pressed={showUnknown}
              onClick={() => setShowUnknown((current) => !current)}
            >
              {showUnknown ? "Hide" : "Show"} unknown ({unknownCount})
            </button>
          ) : null}
          {outerSegments.length > 0 ? (
            <button
              type="button"
              className="chat-fold-toggle"
              aria-label={allOuterClosed ? "Expand all ChatML blocks" : "Collapse all ChatML blocks"}
              onClick={toggleAll}
            >
              {allOuterClosed ? "Expand all" : "Collapse all"}
            </button>
          ) : null}
        </>}
      </div>
      {document.warnings.length > 0 && !isSse && !useSessionScope && (
        <div className="warning-box">
          {document.warnings.map((warning) => (
            <p key={warning}>{warning}</p>
          ))}
        </div>
      )}
      <div className="chat-template-stream">
        {useSessionScope && turns ? (
          <>
            {turns.turns.map((turn, index) => (
              <div className="chat-turn" key={turn.exchange.exchange_id}>
                <TurnBoundary turn={turn} isLast={index === turnCount - 1} streaming={index === turnCount - 1 && turn.responseStream?.status === "streaming"} />
                <SegmentList segments={sessionSegments[index] ?? []} depth={-1} collapsed={collapsed} onToggle={toggle} />
              </div>
            ))}
          </>
        ) : (
          <SegmentList segments={segments} depth={-1} collapsed={collapsed} onToggle={toggle} />
        )}
      </div>
    </section>
  );
}

export default ChatTemplateView;
