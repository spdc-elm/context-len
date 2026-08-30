import { useEffect, useId, useMemo, useState, type CSSProperties, type ReactNode } from "react";

/** A JSON value accepted by the raw tree. `unknown` deliberately keeps the
 * component useful for protocol-specific values without imposing a schema. */
export type RawJsonValue = unknown;

export interface RawJsonTreeProps {
  /** Raw request/response bytes. JSON is parsed for display only. */
  rawBody?: string | null;
  /** Whether rawBody contains the complete artifact. Partial previews are
   * displayed as text and are never passed through JSON.parse. */
  rawBodyComplete?: boolean;
  /** Alias for rawBody, useful when the artifact is called body by a caller. */
  body?: string | null;
  /** A pre-parsed value. When present it takes precedence over rawBody/body. */
  node?: RawJsonValue;
  /** Alias for node. */
  value?: RawJsonValue;
  /** Optional pointer for the supplied root value. */
  sourceJsonPointer?: string;
  /** Alias for sourceJsonPointer. */
  sourcePointer?: string;
  /** Alias for sourceJsonPointer. */
  jsonPointer?: string;
  /** Search text. If omitted, the component owns the search input state. */
  search?: string;
  onSearchChange?: (value: string) => void;
  /** Number of object/array levels opened initially (root is always open). */
  initialExpandDepth?: number;
  /** Alias for initialExpandDepth. */
  autoExpandDepth?: number;
  /** Alias for initialExpandDepth. */
  depth?: number;
  onDepthChange?: (value: number) => void;
  className?: string;
  ariaLabel?: string;
  /** Set false when a host supplies its own search/actions toolbar. */
  showControls?: boolean;
}

interface TreeNode {
  value: RawJsonValue;
  pointer: string;
  depth: number;
  key?: string | number;
  parentKey?: string | number;
  parentIsSemanticArray: boolean;
  children: TreeNode[];
  isContainer: boolean;
  isExpandable: boolean;
  isLongString: boolean;
  isSemanticArray: boolean;
  defaultExpanded: boolean;
  label: string;
  summary: string;
  matches: boolean;
}

interface ParsedInput {
  kind: "json" | "text" | "partial" | "empty";
  value?: RawJsonValue;
  rawText?: string;
  error?: string;
  rootPointer: string;
}

const SEMANTIC_ARRAY_KEYS = new Set([
  "messages",
  "input",
  "output",
  "choices",
  "content",
  "tools",
  "tool_calls",
  "toolcalls",
  "items",
]);

const LARGE_VALUE_KEYS = new Set(["arguments", "argument", "schema", "parameters", "json_schema", "input_schema"]);
const DEFAULT_EXPAND_DEPTH = 2;
const LONG_STRING_LIMIT = 160;
const SUMMARY_LIMIT = 180;

const rowStyle: CSSProperties = {
  display: "flex",
  alignItems: "baseline",
  gap: "7px",
  minHeight: "26px",
  padding: "3px 8px 3px 0",
  fontFamily: "ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace",
  fontSize: "12px",
  lineHeight: 1.45,
};

const childrenStyle: CSSProperties = {
  marginLeft: "14px",
  borderLeft: "1px solid var(--line, #273142)",
  paddingLeft: "10px",
};

const controlButtonStyle: CSSProperties = {
  minHeight: "28px",
  padding: "2px 9px",
  border: "1px solid var(--line-bright, #354157)",
  borderRadius: "4px",
  color: "var(--subtle, #b0bacb)",
  background: "var(--surface, #121620)",
  fontSize: "11px",
};

function isRecord(value: RawJsonValue): value is Record<string, RawJsonValue> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function isContainer(value: RawJsonValue): boolean {
  return Array.isArray(value) || isRecord(value);
}

function keyName(key: string | number | undefined): string {
  return key === undefined ? "" : String(key).toLowerCase().replace(/[^a-z0-9_]/g, "");
}

function isSemanticArrayKey(key: string | number | undefined): boolean {
  return typeof key === "string" && SEMANTIC_ARRAY_KEYS.has(key.toLowerCase());
}

function isLargeValueKey(key: string | number | undefined): boolean {
  return typeof key === "string" && LARGE_VALUE_KEYS.has(key.toLowerCase());
}

function encodePointerToken(token: string | number): string {
  return String(token).replaceAll("~", "~0").replaceAll("/", "~1");
}

/** Append one RFC 6901 token without changing the supplied source pointer. */
export function appendJsonPointer(pointer: string, token: string | number): string {
  const base = pointer === "/" || pointer === "$" ? "" : pointer;
  return `${base}/${encodePointerToken(token)}`;
}

/** A readable root pointer while retaining the empty RFC 6901 root internally. */
export function visibleJsonPointer(pointer: string): string {
  return pointer === "" ? "/" : pointer;
}

function oneLine(value: string, limit = SUMMARY_LIMIT): string {
  const collapsed = value.replace(/[\r\n\t ]+/g, " ").trim();
  if (collapsed.length <= limit) return collapsed;
  return `${collapsed.slice(0, Math.max(0, limit - 1))}…`;
}

function quotedSummary(value: string): string {
  const json = JSON.stringify(oneLine(value, LONG_STRING_LIMIT));
  return json === undefined ? `"${oneLine(value, LONG_STRING_LIMIT)}"` : json;
}

function primitiveSummary(value: RawJsonValue): string {
  if (value === null) return "null";
  if (typeof value === "string") return quotedSummary(value);
  if (typeof value === "number") {
    if (Number.isNaN(value)) return "NaN";
    if (value === Infinity) return "Infinity";
    if (value === -Infinity) return "-Infinity";
    return String(value);
  }
  if (typeof value === "bigint") return `${String(value)}n`;
  if (typeof value === "boolean") return String(value);
  if (typeof value === "undefined") return "undefined";
  if (typeof value === "function") return "[Function]";
  if (typeof value === "symbol") return String(value);
  return oneLine(String(value));
}

function sizeSummary(value: RawJsonValue): string {
  if (Array.isArray(value)) return `[${value.length} ${value.length === 1 ? "item" : "items"}]`;
  if (isRecord(value)) {
    const count = Object.keys(value).length;
    return `{${count} ${count === 1 ? "key" : "keys"}}`;
  }
  return primitiveSummary(value);
}

function textFromValue(value: RawJsonValue): string | undefined {
  if (typeof value === "string") return oneLine(value);
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  return undefined;
}

function contentSummary(value: RawJsonValue): string | undefined {
  if (!isRecord(value)) return textFromValue(value);
  const parts: string[] = [];
  const directFields = ["content", "text", "input_text", "output_text", "reasoning", "thinking", "refusal"];
  for (const field of directFields) {
    if (!(field in value)) continue;
    const candidate = value[field];
    if (typeof candidate === "string") parts.push(`${field}=${quotedSummary(candidate)}`);
    else if (Array.isArray(candidate)) {
      const nested = candidate.flatMap((part) => {
        if (typeof part === "string") return [quotedSummary(part)];
        if (!isRecord(part)) return [];
        for (const textField of ["text", "content", "input_text", "output_text", "thinking", "reasoning"] as const) {
          if (typeof part[textField] === "string") return [`${textField}=${quotedSummary(part[textField])}`];
        }
        return [];
      });
      if (nested.length > 0) parts.push(`${field}=${nested.join(" | ")}`);
      else parts.push(`${field}=${sizeSummary(candidate)}`);
    } else if (isRecord(candidate)) {
      const nested = contentSummary(candidate);
      if (nested) parts.push(`${field}=${nested}`);
    } else if (candidate !== null && typeof candidate !== "object") {
      parts.push(`${field}=${primitiveSummary(candidate)}`);
    }
  }
  return parts.length > 0 ? oneLine(parts.join(" · ")) : undefined;
}

function objectSummary(value: Record<string, RawJsonValue>): string {
  const metadata: string[] = [];
  const pushUnique = (label: string, raw: RawJsonValue) => {
    const text = textFromValue(raw);
    if (text !== undefined && !metadata.some((entry) => entry === `${label}=${text}`)) metadata.push(`${label}=${text}`);
  };
  pushUnique("role", value.role);
  pushUnique("type", value.type);
  pushUnique("name", value.name);
  pushUnique("tool", value.tool_name);
  pushUnique("call_id", value.call_id);
  pushUnique("id", value.id);
  if (isRecord(value.function)) {
    pushUnique("name", value.function.name);
    if (value.function.arguments !== undefined) metadata.push(`arguments=${sizeSummary(value.function.arguments)}`);
  }
  if (isRecord(value.tool)) {
    pushUnique("name", value.tool.name);
    pushUnique("type", value.tool.type);
  }
  if (isRecord(value.message)) {
    pushUnique("role", value.message.role);
    pushUnique("type", value.message.type);
    pushUnique("name", value.message.name);
    const nestedContent = contentSummary(value.message);
    if (nestedContent) metadata.push(nestedContent);
  }
  if (value.arguments !== undefined) metadata.push(`arguments=${sizeSummary(value.arguments)}`);
  if (value.parameters !== undefined && !isRecord(value.parameters)) metadata.push(`parameters=${sizeSummary(value.parameters)}`);
  const content = contentSummary(value);
  if (content) metadata.push(content);
  return metadata.length > 0 ? oneLine(metadata.join(" · ")) : sizeSummary(value);
}

function summaryFor(value: RawJsonValue): string {
  if (Array.isArray(value) || isRecord(value)) return Array.isArray(value) ? sizeSummary(value) : objectSummary(value);
  return primitiveSummary(value);
}

function displayLabel(key: string | number | undefined): string {
  if (key === undefined) return "root";
  return typeof key === "number" ? `[${key}]` : key;
}

function isMessageLike(value: RawJsonValue, parentKey: string | number | undefined, parentIsSemanticArray: boolean): boolean {
  if (!isRecord(value)) return false;
  if (parentIsSemanticArray) return true;
  if (typeof value.role === "string") return true;
  if (typeof value.type === "string" && ("content" in value || "message" in value || "arguments" in value || "name" in value)) return true;
  return typeof parentKey === "string" && ["message", "tool", "tool_call", "tool_result"].includes(parentKey.toLowerCase());
}

function parseInput(props: RawJsonTreeProps): ParsedInput {
  const hasNode = props.node !== undefined || props.value !== undefined;
  const rootPointer = props.sourceJsonPointer ?? props.sourcePointer ?? props.jsonPointer ?? "";
  if (hasNode) return { kind: "json", value: props.node !== undefined ? props.node : props.value, rootPointer };
  const rawText = props.rawBody !== undefined ? props.rawBody : props.body;
  if (rawText === undefined || rawText === null) return { kind: "empty", rootPointer };
  if (props.rawBodyComplete === false) return { kind: "partial", rawText, rootPointer };
  try {
    return { kind: "json", value: JSON.parse(rawText) as RawJsonValue, rawText, rootPointer };
  } catch (error) {
    return { kind: "text", rawText, rootPointer, error: error instanceof Error ? error.message : String(error) };
  }
}

function isLongString(value: RawJsonValue): value is string {
  return typeof value === "string" && value.length > LONG_STRING_LIMIT;
}

function buildTree(
  value: RawJsonValue,
  pointer: string,
  depth: number,
  key: string | number | undefined,
  parentKey: string | number | undefined,
  parentIsSemanticArray: boolean,
  expandDepth: number,
  seen: WeakSet<object>,
): TreeNode {
  const container = isContainer(value);
  const longString = isLongString(value);
  const semanticArray = Array.isArray(value) && (isSemanticArrayKey(key) || depth === 0);
  const messageLike = isMessageLike(value, parentKey, parentIsSemanticArray);
  const largeValue = isLargeValueKey(key);
  const children: TreeNode[] = [];
  if (container) {
    // A supplied value normally comes from JSON and is acyclic. Guarding here
    // keeps a display-only viewer safe if a caller supplies a cyclic object.
    if (typeof value === "object" && value !== null && seen.has(value)) {
      return {
        value,
        pointer,
        depth,
        key,
        parentKey,
        parentIsSemanticArray,
        children,
        isContainer: true,
        isExpandable: false,
        isLongString: false,
        isSemanticArray: semanticArray,
        defaultExpanded: false,
        label: displayLabel(key),
        summary: "[Circular]",
        matches: false,
      };
    }
    if (typeof value === "object" && value !== null) seen.add(value);
    if (Array.isArray(value)) {
      value.forEach((child, index) => children.push(buildTree(child, appendJsonPointer(pointer, index), depth + 1, index, key, semanticArray, expandDepth, seen)));
    } else {
      Object.entries(value as Record<string, RawJsonValue>).forEach(([childKey, child]) => children.push(buildTree(child, appendJsonPointer(pointer, childKey), depth + 1, childKey, key, semanticArray, expandDepth, seen)));
    }
  }
  const expandable = container || longString;
  const largeContainer = children.length > 25;
  const defaultExpanded = expandable && !longString && !messageLike && !largeValue && !largeContainer && (depth === 0 || depth <= expandDepth);
  return {
    value,
    pointer,
    depth,
    key,
    parentKey,
    parentIsSemanticArray,
    children,
    isContainer: container,
    isExpandable: expandable,
    isLongString: longString,
    isSemanticArray: semanticArray,
    defaultExpanded,
    label: displayLabel(key),
    summary: summaryFor(value),
    matches: false,
  };
}

function collectSearchMatches(root: TreeNode, query: string): { matches: Set<string>; autoExpanded: Set<string> } {
  const matches = new Set<string>();
  const autoExpanded = new Set<string>();
  const needle = query.trim().toLocaleLowerCase();
  if (!needle) return { matches, autoExpanded };
  const walk = (node: TreeNode): boolean => {
    const valueText = typeof node.value === "string" ? node.value : primitiveSummary(node.value);
    const own = `${node.label} ${node.summary} ${visibleJsonPointer(node.pointer)} ${valueText}`.toLocaleLowerCase().includes(needle);
    let descendant = false;
    for (const child of node.children) if (walk(child)) descendant = true;
    const hit = own || descendant;
    if (own) {
      matches.add(node.pointer);
      // A long string is itself a collapsed leaf. Open it so the actual
      // matching bytes, rather than only its truncated summary, are visible.
      if (node.isLongString) autoExpanded.add(node.pointer);
    }
    if (descendant) autoExpanded.add(node.pointer);
    return hit;
  };
  walk(root);
  return { matches, autoExpanded };
}

function escapeRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

function highlightedText(text: string, query: string, keyBase: string): ReactNode {
  const needle = query.trim();
  if (!needle) return text;
  const parts = text.split(new RegExp(`(${escapeRegExp(needle)})`, "ig"));
  return parts.map((part, index) => part.toLocaleLowerCase() === needle.toLocaleLowerCase()
    ? <mark data-search-match="true" key={`${keyBase}-${index}`}>{part}</mark>
    : <span key={`${keyBase}-${index}`}>{part}</span>);
}

function PlainTextFallback({ text, query, partial = false }: { text: string; query: string; partial?: boolean }) {
  return (
    <div className="raw-json-tree-fallback" data-raw-json-fallback={partial ? "partial" : "text"}>
      <div className="raw-json-tree-fallback-note">{partial ? "Preview truncated · load or download the complete artifact before using the JSON tree." : "Plain text · JSON parse failed; showing the immutable body unchanged."}</div>
      <pre aria-label={partial ? "Artifact preview" : "Raw text fallback"}>{highlightedText(text, query, "fallback")}</pre>
    </div>
  );
}

interface TreeNodeViewProps {
  node: TreeNode;
  query: string;
  matches: Set<string>;
  autoExpanded: Set<string>;
  overrides: Record<string, boolean>;
  onToggle: (pointer: string, expanded: boolean) => void;
}

function TreeNodeView({ node, query, matches, autoExpanded, overrides, onToggle }: TreeNodeViewProps) {
  const expanded = overrides[node.pointer] ?? (autoExpanded.has(node.pointer) || node.defaultExpanded);
  const isMatch = matches.has(node.pointer);
  const pointerText = visibleJsonPointer(node.pointer);
  const toggleLabel = expanded ? `Collapse ${pointerText}` : `Expand ${pointerText}`;
  const keyText = highlightedText(node.label, query, `${node.pointer}-key`);
  const summaryText = highlightedText(node.summary, query, `${node.pointer}-summary`);
  return (
    <div
      className={`raw-json-node${isMatch ? " match" : ""}`}
      data-raw-json-node="true"
      data-node-pointer={node.pointer}
      data-json-pointer={node.pointer}
      data-node-key={node.key === undefined ? "" : String(node.key)}
      data-node-expanded={expanded ? "true" : "false"}
      role="treeitem"
      aria-level={node.depth + 1}
      aria-expanded={node.isExpandable ? expanded : undefined}
    >
      <div className="raw-json-node-row" style={rowStyle}>
        {node.isExpandable ? (
          <button
            type="button"
            className="raw-json-node-toggle"
            aria-label={toggleLabel}
            title={toggleLabel}
            onClick={() => onToggle(node.pointer, !expanded)}
          >
            <svg width="8" height="8" viewBox="0 0 8 8" aria-hidden="true" focusable="false">
              <path
                d={expanded ? "M1.5 3 L4 5.5 L6.5 3" : "M2.5 1.5 L5.5 4 L2.5 6.5"}
                stroke="currentColor"
                strokeWidth="1.3"
                fill="none"
                strokeLinecap="round"
                strokeLinejoin="round"
              />
            </svg>
          </button>
        ) : <span aria-hidden="true" style={{ display: "inline-block", width: "14px" }} />}
        <span className="raw-json-node-label" data-node-label="true">{keyText}</span>
        <span className="raw-json-node-summary" data-node-summary="true">{summaryText}</span>
        {node.isSemanticArray && <span className="raw-json-array-count" data-array-count="true">{node.children.length} {node.children.length === 1 ? "item" : "items"}</span>}
        <code className="raw-json-node-pointer" data-source-json-pointer={node.pointer} data-source-pointer={node.pointer} title="Source JSON pointer">{highlightedText(pointerText, query, `${node.pointer}-pointer`)}</code>
      </div>
      {expanded && node.isLongString && typeof node.value === "string" && (
        <pre className="raw-json-long-string" data-long-string="true">{highlightedText(node.value, query, `${node.pointer}-value`)}</pre>
      )}
      {expanded && node.children.length > 0 && (
        <div className="raw-json-node-children" role="group" style={childrenStyle}>
          {node.children.map((child) => <TreeNodeView key={child.pointer} node={child} query={query} matches={matches} autoExpanded={autoExpanded} overrides={overrides} onToggle={onToggle} />)}
        </div>
      )}
    </div>
  );
}

function normalizeDepth(value: number): number {
  if (!Number.isFinite(value)) return DEFAULT_EXPAND_DEPTH;
  return Math.max(0, Math.min(100, Math.floor(value)));
}

/**
 * Structured, loss-aware JSON inspection view.
 *
 * Parsing, expansion and highlighting are projection state only. `rawBody`
 * and a supplied `node` are never mutated or serialized back to the caller.
 */
export function RawJsonTree({
  rawBody,
  rawBodyComplete,
  body,
  node,
  value,
  sourceJsonPointer,
  sourcePointer,
  jsonPointer,
  search,
  onSearchChange,
  initialExpandDepth,
  autoExpandDepth,
  depth,
  onDepthChange,
  className,
  ariaLabel = "Raw JSON tree",
  showControls = true,
}: RawJsonTreeProps) {
  const parsed = useMemo(() => parseInput({ rawBody, rawBodyComplete, body, node, value, sourceJsonPointer, sourcePointer, jsonPointer }), [rawBody, rawBodyComplete, body, node, value, sourceJsonPointer, sourcePointer, jsonPointer]);
  const configuredDepth = autoExpandDepth ?? initialExpandDepth ?? depth;
  const [expandDepth, setExpandDepth] = useState(() => normalizeDepth(configuredDepth ?? DEFAULT_EXPAND_DEPTH));
  const [internalSearch, setInternalSearch] = useState("");
  const [overrides, setOverrides] = useState<Record<string, boolean>>({});
  const searchId = useId();
  const depthId = useId();
  const isControlledSearch = search !== undefined;
  const query = isControlledSearch ? search : internalSearch;

  useEffect(() => {
    if (configuredDepth !== undefined) setExpandDepth(normalizeDepth(configuredDepth));
  }, [configuredDepth]);

  // A new artifact/value starts with its own projection state. Keeping this
  // reset separate from parsing means search and toggles never touch the raw
  // artifact itself.
  useEffect(() => {
    setOverrides({});
  }, [parsed.value, parsed.rawText, parsed.kind, parsed.rootPointer]);

  const root = useMemo(() => {
    if (parsed.kind !== "json") return undefined;
    return buildTree(parsed.value, parsed.rootPointer, 0, undefined, undefined, false, expandDepth, new WeakSet<object>());
  }, [parsed.kind, parsed.value, parsed.rootPointer, expandDepth]);
  const searchState = useMemo(() => root ? collectSearchMatches(root, query) : { matches: new Set<string>(), autoExpanded: new Set<string>() }, [root, query]);

  const allExpandablePointers = useMemo(() => {
    const pointers: string[] = [];
    const walk = (treeNode: TreeNode) => {
      if (treeNode.isExpandable) pointers.push(treeNode.pointer);
      treeNode.children.forEach(walk);
    };
    if (root) walk(root);
    return pointers;
  }, [root]);

  const handleToggle = (pointer: string, expanded: boolean) => {
    setOverrides((current) => ({ ...current, [pointer]: expanded }));
  };
  const handleSearch = (next: string) => {
    setInternalSearch(next);
    onSearchChange?.(next);
  };
  const handleDepth = (next: string) => {
    const normalized = normalizeDepth(Number(next));
    setExpandDepth(normalized);
    setOverrides({});
    onDepthChange?.(normalized);
  };
  const expandAll = () => setOverrides(Object.fromEntries(allExpandablePointers.map((pointer) => [pointer, true])));
  const collapseAll = () => setOverrides(Object.fromEntries(allExpandablePointers.map((pointer) => [pointer, false])));

  const rootClassName = ["raw-json-tree", className].filter(Boolean).join(" ");
  return (
    <section className={rootClassName} aria-label={ariaLabel} data-raw-json-tree="true">
      {showControls && (
        <div className="raw-json-tree-toolbar" data-raw-json-tree-controls="true" style={{ display: "flex", alignItems: "center", flexWrap: "wrap", gap: "8px", padding: "8px 0" }}>
          <label htmlFor={searchId} style={{ display: "flex", alignItems: "center", gap: "6px" }}>
            <span>Search</span>
            <input id={searchId} aria-label="Search raw JSON" type="search" value={query} onChange={(event) => handleSearch(event.target.value)} placeholder="Search JSON" />
          </label>
          <button type="button" aria-label="Collapse all" style={controlButtonStyle} onClick={collapseAll}>Collapse all</button>
          <button type="button" aria-label="Expand all" style={controlButtonStyle} onClick={expandAll}>Expand all</button>
          <label htmlFor={depthId} style={{ display: "flex", alignItems: "center", gap: "6px" }}>
            <span>Depth</span>
            <input id={depthId} aria-label="Auto expand depth" type="number" min={0} max={100} value={expandDepth} onChange={(event) => handleDepth(event.target.value)} style={{ width: "58px" }} />
          </label>
        </div>
      )}
      {parsed.kind === "empty" && <div className="raw-json-tree-empty">Select an artifact to inspect its raw body.</div>}
      {(parsed.kind === "text" || parsed.kind === "partial") && parsed.rawText !== undefined && <PlainTextFallback text={parsed.rawText} query={query} partial={parsed.kind === "partial"} />}
      {parsed.kind === "json" && root && (
        <div className="raw-json-tree-view" role="tree" aria-label="JSON value tree" data-json-tree-view="true">
          <TreeNodeView node={root} query={query} matches={searchState.matches} autoExpanded={searchState.autoExpanded} overrides={overrides} onToggle={handleToggle} />
        </div>
      )}
    </section>
  );
}

export default RawJsonTree;
