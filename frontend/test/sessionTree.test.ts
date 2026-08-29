import { describe, expect, it } from "vitest";
import type { ExchangeSnapshot } from "../src/contracts";
import { groupSessions, sessionMatchesFilters, unplacedExchanges } from "../src/sessionTree";

interface Turn {
  id: string;
  parent?: string;
  position?: string;
  depth?: number;
  repeat?: number;
  fork?: boolean;
  model?: string;
  modelChanged?: boolean;
  updatedAt?: string;
}

function sessionExchanges(turns: Turn[]): ExchangeSnapshot[] {
  return turns.map((turn) => ({
    exchange_id: turn.id,
    protocol: "chat_completions",
    request: {
      envelope: { method: "POST", path: "/v1/chat/completions", escaped_path: "", raw_query: "", headers: {} },
      artifact_refs: [],
    },
    response: { envelope: { status: 200, headers: {}, trailers: {} }, artifact_refs: [] },
    policy: { request_gate: "pass", response_gate: "pass" },
    state: "completed",
    warnings: [],
    created_at: turn.updatedAt ?? "2026-08-27T10:00:00.000Z",
    updated_at: turn.updatedAt ?? "2026-08-27T10:00:00.000Z",
    summary: { model: turn.model ?? "m", message_count: 1, preview: "preview " + turn.id },
    session: {
      session_id: "sess-1",
      depth: turn.depth ?? 1,
      position: turn.position ?? "pos-" + turn.id,
      parent_exchange_id: turn.parent,
      repeat_index: turn.repeat ?? 0,
      fork: turn.fork,
      model_changed: turn.modelChanged,
      root: !turn.parent,
    },
  })) as ExchangeSnapshot[];
}

describe("groupSessions", () => {
  it("builds a turn chain with rollouts stacked on their leader", () => {
    const exchanges = sessionExchanges([
      { id: "t1", depth: 1 },
      { id: "t2", parent: "t1", depth: 2 },
      { id: "t2b", parent: "t1", depth: 2, position: "pos-t2", repeat: 1 },
      { id: "t3", parent: "t2", depth: 3 },
    ]);
    const [group] = groupSessions(exchanges);
    expect(group.members.map((exchange) => exchange.exchange_id)).toEqual(["t1", "t2", "t2b", "t3"]);
    expect(group.turnCount).toBe(3);
    expect(group.rolloutCount).toBe(1);
    expect(group.forkCount).toBe(0);
    expect(group.models).toEqual(["m"]);
  });

  it("renders forks as sibling branches under the shared parent", () => {
    const exchanges = sessionExchanges([
      { id: "t1", depth: 1 },
      { id: "t2", parent: "t1", depth: 2 },
      { id: "t2f", parent: "t1", depth: 2, fork: true, updatedAt: "2026-08-27T10:00:02.000Z" },
      { id: "t3", parent: "t2f", depth: 3 },
    ]);
    const [group] = groupSessions(exchanges);
    expect(group.forkCount).toBe(1);
    // DFS order: t1, then its children (t2, then the later fork), then t3 under the fork.
    expect(group.members.map((exchange) => exchange.exchange_id)).toEqual(["t1", "t2", "t2f", "t3"]);
    expect(group.root?.children).toHaveLength(2);
  });

  it("stacks root-level rollouts and aggregates session metadata from the latest turn", () => {
    const exchanges = sessionExchanges([
      { id: "t1", depth: 1, position: "pos-root" },
      { id: "t1b", depth: 1, position: "pos-root", repeat: 1, updatedAt: "2026-08-27T10:00:01.000Z" },
      { id: "t2", parent: "t1", depth: 2, model: "m2", modelChanged: true, updatedAt: "2026-08-27T10:00:02.000Z" },
    ]);
    const [group] = groupSessions(exchanges);
    expect(group.root?.rollouts).toHaveLength(1);
    expect(group.models).toEqual(["m2", "m"]);
    expect(group.model).toBe("m2");
    expect(group.latest.exchange_id).toBe("t2");
    expect(group.turnCount).toBe(2);
  });

  it("degrades orphaned members to roots so no exchange disappears", () => {
    const exchanges = sessionExchanges([
      { id: "orphan", parent: "gone", depth: 5 },
      { id: "t1", depth: 1 },
    ]);
    const [group] = groupSessions(exchanges);
    expect(group.members).toHaveLength(2);
    expect(group.turnCount).toBe(5);
  });

  it("separates sessions and leaves unplaced exchanges out", () => {
    const exchanges = sessionExchanges([
      { id: "a1", depth: 1 },
      { id: "b1", depth: 1 },
    ]);
    exchanges[1] = { ...exchanges[1], session: { ...exchanges[1].session!, session_id: "sess-2" } } as ExchangeSnapshot;
    const plain = { ...exchanges[0], session: undefined, protocol: "unknown" } as ExchangeSnapshot;
    const groups = groupSessions([...exchanges, plain]);
    expect(groups).toHaveLength(2);
    expect(groups[0].members).toHaveLength(1);
    expect(unplacedExchanges([...exchanges, plain])).toEqual([plain]);
  });

  it("filters sessions on the aggregate fields", () => {
    const exchanges = sessionExchanges([
      { id: "t1", depth: 1, model: "model-a" },
      { id: "t2", parent: "t1", depth: 2, model: "model-b", updatedAt: "2026-08-27T10:00:02.000Z" },
    ]);
    const [group] = groupSessions(exchanges);
    expect(sessionMatchesFilters(group, "", "all", "all", "all")).toBe(true);
    expect(sessionMatchesFilters(group, "preview t1", "all", "all", "all")).toBe(true);
    expect(sessionMatchesFilters(group, "model-b", "all", "all", "all")).toBe(true);
    expect(sessionMatchesFilters(group, "", "responses", "all", "all")).toBe(false);
    expect(sessionMatchesFilters(group, "", "all", "completed", "all")).toBe(true);
    expect(sessionMatchesFilters(group, "", "all", "all", "model-b")).toBe(true);
    expect(sessionMatchesFilters(group, "", "all", "all", "model-z")).toBe(false);
  });
});
