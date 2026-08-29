import type { ExchangeSnapshot } from "./contracts";

/**
 * Pure session-tree derivation from exchange snapshots (docs/session-spec.md).
 *
 * The backend assigns each exchange a session placement at capture time; this
 * module rebuilds the tree on the client from those additive fields. It is
 * recomputed whenever snapshots change, so the queue never needs a separate
 * aggregate endpoint.
 */

export interface SessionMemberNode {
  exchange: ExchangeSnapshot;
  /** Later rollout siblings of the same position (identical context resent). */
  rollouts: ExchangeSnapshot[];
  /** Child turns extending this exchange's context. */
  children: SessionMemberNode[];
}

export interface SessionGroup {
  sessionId: string;
  protocol: string;
  /** DFS-ordered members (rollouts adjacent to their leader). */
  members: ExchangeSnapshot[];
  root: SessionMemberNode | undefined;
  latest: ExchangeSnapshot;
  turnCount: number;
  forkCount: number;
  rolloutCount: number;
  preview: string;
  model: string | undefined;
  models: string[];
  messageCount: number;
  contextTokens: number | undefined;
  state: ExchangeSnapshot["state"];
  updatedAt: string;
}

export function groupSessions(exchanges: ExchangeSnapshot[]): SessionGroup[] {
  const bySession = new Map<string, ExchangeSnapshot[]>();
  for (const exchange of exchanges) {
    const sessionId = exchange.session?.session_id;
    if (!sessionId) continue;
    const bucket = bySession.get(sessionId);
    if (bucket) bucket.push(exchange);
    else bySession.set(sessionId, [exchange]);
  }

  const groups: SessionGroup[] = [];
  for (const [sessionId, members] of bySession) {
    groups.push(buildGroup(sessionId, members));
  }
  // Most recently active sessions first; ties break on the session id so the
  // order is stable across recomputes.
  groups.sort((left, right) => {
    const delta = Date.parse(right.updatedAt) - Date.parse(left.updatedAt);
    if (delta !== 0) return delta;
    return right.sessionId.localeCompare(left.sessionId);
  });
  return groups;
}

function buildGroup(sessionId: string, members: ExchangeSnapshot[]): SessionGroup {
  const sorted = [...members].sort((left, right) => {
    const delta = Date.parse(left.updated_at) - Date.parse(right.updated_at);
    if (delta !== 0) return delta;
    return left.exchange_id.localeCompare(right.exchange_id);
  });

  // Index every member by id; children attach through parent_exchange_id.
  const nodes = new Map<string, SessionMemberNode>();
  for (const exchange of sorted) {
    nodes.set(exchange.exchange_id, { exchange, rollouts: [], children: [] });
  }

  const roots: SessionMemberNode[] = [];
  for (const node of nodes.values()) {
    const parentID = node.exchange.session?.parent_exchange_id ?? "";
    const position = node.exchange.session?.position ?? "";
    const parent = parentID ? nodes.get(parentID) : undefined;
    if (parent && parentID !== node.exchange.exchange_id) {
      // A rollout repeats the position of an existing child of the same
      // parent; it stacks on that child instead of adding a branch.
      const sibling = parent.children.find((child) => (child.exchange.session?.position ?? "") === position);
      if (sibling) sibling.rollouts.push(node.exchange);
      else parent.children.push(node);
      continue;
    }
    // Root-level rollouts stack on the first node at the same position.
    const sibling = roots.find((root) => (root.exchange.session?.position ?? "") === position && position !== "");
    if (sibling) sibling.rollouts.push(node.exchange);
    else roots.push(node);
  }

  const ordered: ExchangeSnapshot[] = [];
  const walk = (node: SessionMemberNode) => {
    ordered.push(node.exchange);
    ordered.push(...node.rollouts);
    node.children.sort((left, right) => {
      const delta = Date.parse(left.exchange.updated_at) - Date.parse(right.exchange.updated_at);
      if (delta !== 0) return delta;
      return left.exchange.exchange_id.localeCompare(right.exchange.exchange_id);
    });
    for (const child of node.children) walk(child);
  };
  for (const root of roots) walk(root);

  const latest = sorted[sorted.length - 1];
  const models: string[] = [];
  for (const exchange of [...sorted].reverse()) {
    const model = exchange.summary?.model;
    if (model && !models.includes(model)) models.push(model);
  }
  let turnCount = 0;
  let forkCount = 0;
  let rolloutCount = 0;
  for (const exchange of sorted) {
    turnCount = Math.max(turnCount, exchange.session?.depth ?? 0);
    if (exchange.session?.fork) forkCount++;
    if ((exchange.session?.repeat_index ?? 0) > 0) rolloutCount++;
  }

  const rootExchange = roots.length > 0 ? roots[0].exchange : undefined;
  return {
    sessionId,
    protocol: rootExchange?.protocol ?? latest.protocol,
    members: ordered,
    root: roots[0],
    latest,
    turnCount,
    forkCount,
    rolloutCount,
    preview: rootExchange?.summary?.preview ?? latest.summary?.preview ?? "",
    model: latest.summary?.model,
    models,
    messageCount: latest.summary?.message_count ?? 0,
    contextTokens: latest.summary?.context_tokens,
    state: latest.state,
    updatedAt: latest.updated_at,
  };
}

/** Exchanges without a session placement (non-conversation traffic). */
export function unplacedExchanges(exchanges: ExchangeSnapshot[]): ExchangeSnapshot[] {
  return exchanges.filter((exchange) => !exchange.session);
}

export function sessionMatchesFilters(group: SessionGroup, text: string, protocol: string, state: string, model: string): boolean {
  if (protocol !== "all" && group.protocol !== protocol) return false;
  if (state !== "all" && group.state !== state) return false;
  if (model !== "all" && group.model !== model) return false;
  const query = text.trim().toLowerCase();
  if (query) {
    const haystack = [group.preview ?? "", group.model ?? "", group.sessionId].join(" ").toLowerCase();
    if (!haystack.includes(query)) return false;
  }
  return true;
}
