import { useMemo, useState } from "react";
import type { ExchangeSnapshot } from "../contracts";
import { formatTokens } from "../format";
import { groupSessions, sessionMatchesFilters, unplacedExchanges, type SessionGroup } from "../sessionTree";
import { formatWorkspaceTime } from "../time";

interface TrafficQueueProps {
  exchanges: ExchangeSnapshot[];
  selectedExchangeId?: string;
  collapsed?: boolean;
  onToggle?: () => void;
  onClear?: () => void;
  clearBusy?: boolean;
  onSelect: (exchangeId: string, followSessionId?: string) => void;
  onDeleteSession?: (sessionId: string) => void;
  hasMore?: boolean;
  loadingMore?: boolean;
  onLoadMore?: () => void;
}

type QueueView = "sessions" | "exchanges";

const CHAT_PROTOCOLS = new Set(["responses", "chat_completions", "anthropic_messages"]);

function protocolLabel(protocol: ExchangeSnapshot["protocol"]): string {
  const labels: Record<string, string> = {
    responses: "Responses",
    chat_completions: "Chat Completions",
    anthropic_messages: "Anthropic Messages",
    unknown: "Other",
  };
  return labels[protocol] ?? protocol;
}

function stateLabel(state: string): string {
  return state.replaceAll("_", " ");
}

function isChatProtocol(protocol: ExchangeSnapshot["protocol"]): boolean {
  return CHAT_PROTOCOLS.has(protocol);
}

function statsLine(summary: ExchangeSnapshot["summary"]): string {
  const parts: string[] = [];
  if (summary?.model) parts.push(summary.model);
  if (summary?.message_count) parts.push(`${summary.message_count} msgs`);
  parts.push(`${formatTokens(summary?.context_tokens)} ctx`);
  return parts.join(" · ");
}

interface FilterState {
  text: string;
  protocol: string;
  state: string;
  model: string;
}

const NO_FILTERS: FilterState = { text: "", protocol: "all", state: "all", model: "all" };

function matchesFilters(exchange: ExchangeSnapshot, filters: FilterState): boolean {
  const { text, protocol, state, model } = filters;
  if (protocol !== "all" && exchange.protocol !== protocol) return false;
  if (state !== "all" && exchange.state !== state) return false;
  if (model !== "all" && exchange.summary?.model !== model) return false;
  const query = text.trim().toLowerCase();
  if (query) {
    const haystack = [exchange.summary?.preview ?? "", exchange.summary?.model ?? "", exchange.exchange_id]
      .join(" ")
      .toLowerCase();
    if (!haystack.includes(query)) return false;
  }
  return true;
}

function uniqueOption(values: string[]): string[] {
  return [...new Set(values.filter((value) => value !== ""))].sort((left, right) => left.localeCompare(right));
}

export function TrafficQueue({ exchanges, selectedExchangeId, collapsed = false, onToggle, onClear, clearBusy = false, onSelect, onDeleteSession, hasMore = false, loadingMore = false, onLoadMore }: TrafficQueueProps) {
  const [filters, setFilters] = useState<FilterState>(NO_FILTERS);
  const [view, setView] = useState<QueueView>("sessions");
  const [expandedSessions, setExpandedSessions] = useState<Set<string>>(new Set());
  const [otherTrafficOpen, setOtherTrafficOpen] = useState(false);

  const orderedExchanges = useMemo(() => [...exchanges].sort((left, right) => {
    const rightTime = Date.parse(right.updated_at);
    const leftTime = Date.parse(left.updated_at);
    if (rightTime !== leftTime) return rightTime - leftTime;
    return right.exchange_id.localeCompare(left.exchange_id);
  }), [exchanges]);

  const placedExchanges = useMemo(() => exchanges, [exchanges]);
  const sessions = useMemo(() => groupSessions(placedExchanges), [placedExchanges]);
  const otherTraffic = useMemo(() => unplacedExchanges(orderedExchanges), [orderedExchanges]);

  const protocolOptions = useMemo(() => uniqueOption(exchanges.map((exchange) => exchange.protocol)), [exchanges]);
  const stateOptions = useMemo(() => uniqueOption(exchanges.map((exchange) => exchange.state)), [exchanges]);
  const modelOptions = useMemo(() => uniqueOption(exchanges.map((exchange) => exchange.summary?.model ?? "")), [exchanges]);

  const filteredExchanges = useMemo(
    () => orderedExchanges.filter((exchange) => matchesFilters(exchange, filters)),
    [orderedExchanges, filters],
  );
  const filteredSessions = useMemo(
    () => sessions.filter((group) => sessionMatchesFilters(group, filters.text, filters.protocol, filters.state, filters.model)),
    [sessions, filters],
  );
  const filteredOther = useMemo(
    () => otherTraffic.filter((exchange) => matchesFilters(exchange, filters)),
    [otherTraffic, filters],
  );

  const filtersActive =
    filters.text.trim() !== "" || filters.protocol !== "all" || filters.state !== "all" || filters.model !== "all";
  const listEmpty = view === "sessions"
    ? filteredSessions.length === 0 && filteredOther.length === 0
    : filteredExchanges.length === 0;

  const toggleSession = (sessionId: string) => {
    setExpandedSessions((current) => {
      const next = new Set(current);
      if (next.has(sessionId)) next.delete(sessionId);
      else next.add(sessionId);
      return next;
    });
  };

  return (
    <aside className={`traffic-panel ${collapsed ? "collapsed" : ""}`} aria-label="Traffic queue">
      <div className="panel-heading">
        {!collapsed && <div>
          <p className="eyebrow">LIVE TRAFFIC</p>
          <h2>Exchange queue</h2>
        </div>}
        {!collapsed && <div className="panel-heading-actions">
          {onClear && <button
            type="button"
            className="clear-queue-button"
            aria-label="Clear exchange queue"
            title="Clear exchange queue"
            disabled={clearBusy || exchanges.length === 0}
            onClick={onClear}
          >
            <svg viewBox="0 0 16 16" width="15" height="15" aria-hidden="true" focusable="false">
              <path d="M5 5.25 3.9 13h8.2L11 5.25M2.5 5.25h11M6.25 3.25h3.5l.7 2H5.55l.7-2Z" fill="none" stroke="currentColor" strokeWidth="1.15" strokeLinecap="round" strokeLinejoin="round" />
              <path d="m6.25 7.2.35 4.1m3.15-4.1-.35 4.1" fill="none" stroke="currentColor" strokeWidth=".9" strokeLinecap="round" />
            </svg>
          </button>}
          <button type="button" className="collapse-button" aria-label="Collapse traffic" onClick={onToggle}>‹</button>
        </div>}
        {collapsed && <button type="button" className="collapse-button" aria-label="Expand traffic" onClick={onToggle}>›</button>}
      </div>
      {!collapsed && <>
        <p className="panel-note">Live local workspace · events in realtime; bodies load on demand</p>
        <div className="traffic-filters" role="search" aria-label="Exchange filters">
          <div className="traffic-view-toggle" role="group" aria-label="Queue view">
            <button
              type="button"
              className={`view-toggle-button ${view === "sessions" ? "active" : ""}`}
              aria-pressed={view === "sessions"}
              onClick={() => setView("sessions")}
            >
              Sessions
            </button>
            <button
              type="button"
              className={`view-toggle-button ${view === "exchanges" ? "active" : ""}`}
              aria-pressed={view === "exchanges"}
              onClick={() => setView("exchanges")}
            >
              Exchanges
            </button>
          </div>
          <input
            className="traffic-filter-input"
            type="search"
            placeholder="Filter preview, model, id…"
            aria-label="Filter exchanges by text"
            value={filters.text}
            onChange={(event) => setFilters((current) => ({ ...current, text: event.target.value }))}
          />
          <div className="traffic-filter-row">
            <select
              className="traffic-filter-select"
              aria-label="Filter by protocol"
              value={filters.protocol}
              onChange={(event) => setFilters((current) => ({ ...current, protocol: event.target.value }))}
            >
              <option value="all">All protocols</option>
              {protocolOptions.map((option) => (
                <option key={option} value={option}>{protocolLabel(option)}</option>
              ))}
            </select>
            <select
              className="traffic-filter-select"
              aria-label="Filter by state"
              value={filters.state}
              onChange={(event) => setFilters((current) => ({ ...current, state: event.target.value }))}
            >
              <option value="all">All states</option>
              {stateOptions.map((option) => (
                <option key={option} value={option}>{stateLabel(option)}</option>
              ))}
            </select>
            <select
              className="traffic-filter-select"
              aria-label="Filter by model"
              value={filters.model}
              onChange={(event) => setFilters((current) => ({ ...current, model: event.target.value }))}
              disabled={modelOptions.length === 0}
            >
              <option value="all">All models</option>
              {modelOptions.map((option) => (
                <option key={option} value={option}>{option}</option>
              ))}
            </select>
          </div>
        </div>
      <div className="traffic-list" role="listbox" aria-label="Exchanges">
        {listEmpty ? (
          <div className="empty-state">
            {exchanges.length === 0
              ? "Waiting for an exchange…"
              : filtersActive
                ? "No exchanges match the current filters."
                : "Waiting for an exchange…"}
          </div>
        ) : view === "sessions" ? (
          <>
            {filteredSessions.map((group) => (
              <SessionRow
                key={group.sessionId}
                group={group}
                expanded={expandedSessions.has(group.sessionId)}
                selectedExchangeId={selectedExchangeId}
                onToggle={() => toggleSession(group.sessionId)}
                onSelect={onSelect}
                onDeleteSession={onDeleteSession}
              />
            ))}
            {filteredOther.length > 0 && (
              <div className="traffic-other">
                <button
                  type="button"
                  className="traffic-other-header"
                  aria-expanded={otherTrafficOpen}
                  onClick={() => setOtherTrafficOpen((open) => !open)}
                >
                  <span className={`traffic-other-chevron ${otherTrafficOpen ? "open" : ""}`} aria-hidden="true">›</span>
                  Other traffic · {filteredOther.length}
                </button>
                {otherTrafficOpen && filteredOther.map((exchange) => (
                  <button
                    type="button"
                    role="option"
                    aria-selected={exchange.exchange_id === selectedExchangeId}
                    className={`traffic-row traffic-row-nested ${exchange.exchange_id === selectedExchangeId ? "selected" : ""}`}
                    key={exchange.exchange_id}
                    onClick={() => onSelect(exchange.exchange_id)}
                  >
                    <div className="traffic-row-top">
                      <span className={`protocol-dot protocol-${exchange.protocol}`} aria-hidden="true" />
                      <strong>{exchange.request.envelope.method}</strong>
                      <span className={`state-pill state-${exchange.state}`}>{stateLabel(exchange.state)}</span>
                    </div>
                    <div className="traffic-row-path">{`${exchange.request.envelope.method} ${exchange.request.envelope.path}${exchange.request.envelope.raw_query ? `?${exchange.request.envelope.raw_query}` : ""}`}</div>
                    <div className="traffic-row-meta">
                      <span>{formatWorkspaceTime(exchange.updated_at)}</span>
                    </div>
                  </button>
                ))}
              </div>
            )}
          </>
        ) : (
          filteredExchanges.map((exchange) => (
            <ExchangeRow
              key={exchange.exchange_id}
              exchange={exchange}
              selected={exchange.exchange_id === selectedExchangeId}
              onSelect={onSelect}
            />
          ))
        )}
      </div>
      {hasMore && !collapsed && (
        <button type="button" className="button quiet traffic-load-more" onClick={onLoadMore} disabled={loadingMore}>
          {loadingMore ? "Loading…" : "Load more exchanges"}
        </button>
      )}
      </>}
    </aside>
  );
}

function ExchangeRow({ exchange, selected, onSelect }: {
  exchange: ExchangeSnapshot;
  selected: boolean;
  onSelect: (exchangeId: string) => void;
}) {
  const chat = isChatProtocol(exchange.protocol);
  const preview = exchange.summary?.preview;
  const stats = statsLine(exchange.summary);
  return (
    <button
      type="button"
      role="option"
      aria-selected={selected}
      className={`traffic-row ${selected ? "selected" : ""}`}
      onClick={() => onSelect(exchange.exchange_id)}
    >
      <div className="traffic-row-top">
        <span className={`protocol-dot protocol-${exchange.protocol}`} aria-hidden="true" />
        <strong>{chat ? protocolLabel(exchange.protocol) : exchange.request.envelope.method}</strong>
        <span className={`state-pill state-${exchange.state}`}>{stateLabel(exchange.state)}</span>
      </div>
      {chat ? (
        <>
          <div className="traffic-row-stats">{stats}</div>
          {preview ? <div className="traffic-row-preview">{preview}</div> : null}
        </>
      ) : (
        <div className="traffic-row-path">{`${exchange.request.envelope.method} ${exchange.request.envelope.path}${exchange.request.envelope.raw_query ? `?${exchange.request.envelope.raw_query}` : ""}`}</div>
      )}
      <div className="traffic-row-meta">
        <span>{exchange.summary?.tool_names?.length ? `${exchange.summary.tool_names.length} tools` : ""}</span>
        <span>{formatWorkspaceTime(exchange.updated_at)}</span>
      </div>
    </button>
  );
}

function SessionRow({ group, expanded, selectedExchangeId, onToggle, onSelect, onDeleteSession }: {
  group: SessionGroup;
  expanded: boolean;
  selectedExchangeId?: string;
  onToggle: () => void;
  onSelect: (exchangeId: string, followSessionId?: string) => void;
  onDeleteSession?: (sessionId: string) => void;
}) {
  const stats = statsLine(group.latest.summary);
  const selectedInGroup = group.members.some((exchange) => exchange.exchange_id === selectedExchangeId);
  const turnBadges = [
    group.forkCount > 0 ? `${group.forkCount} fork${group.forkCount > 1 ? "s" : ""}` : "",
    group.rolloutCount > 0 ? `×${group.rolloutCount + 1}` : "",
    group.models.length > 1 ? `+${group.models.length - 1} model` : "",
  ].filter(Boolean).join(" · ");
  return (
    <div className="traffic-session">
      <div
        role="option"
        aria-selected={selectedInGroup}
        tabIndex={0}
        className={`traffic-row ${selectedInGroup ? "selected" : ""}`}
        onClick={() => onSelect(group.latest.exchange_id, group.sessionId)}
        onKeyDown={(event) => {
          if (event.key === "Enter" || event.key === " ") {
            event.preventDefault();
            onSelect(group.latest.exchange_id, group.sessionId);
          }
        }}
      >
        <div className="traffic-row-top">
          <span className={`protocol-dot protocol-${group.protocol}`} aria-hidden="true" />
          <strong>{protocolLabel(group.protocol)}</strong>
          <span className={`state-pill state-${group.state}`}>{stateLabel(group.state)}</span>
          <button type="button" className="session-delete-button" aria-label={`Delete session ${group.sessionId}`} title="Delete entire session" onClick={(event) => { event.stopPropagation(); onDeleteSession?.(group.sessionId); }} onKeyDown={(event) => { event.stopPropagation(); }}>×</button>
          <span
            className={`traffic-session-chevron ${expanded ? "open" : ""}`}
            role="button"
            aria-label={expanded ? "Collapse session turns" : "Expand session turns"}
            aria-expanded={expanded}
            tabIndex={0}
            onClick={(event) => {
              event.stopPropagation();
              onToggle();
            }}
            onKeyDown={(event) => {
              if (event.key === "Enter" || event.key === " ") {
                event.stopPropagation();
                event.preventDefault();
                onToggle();
              }
            }}
          >›</span>
        </div>
        <div className="traffic-row-stats">{stats}</div>
        {group.preview ? <div className="traffic-row-preview">{group.preview}</div> : null}
        <div className="traffic-row-meta">
          <span>{[`${group.turnCount} turn${group.turnCount > 1 ? "s" : ""}`, turnBadges].filter(Boolean).join(" · ")}</span>
          <span>{formatWorkspaceTime(group.updatedAt)}</span>
        </div>
      </div>
      {expanded && (
        <div className="traffic-session-members" role="group" aria-label="Session turns">
          {group.members.map((exchange) => (
            <MemberRow
              key={exchange.exchange_id}
              exchange={exchange}
              selected={exchange.exchange_id === selectedExchangeId}
              onSelect={onSelect}
            />
          ))}
        </div>
      )}
    </div>
  );
}

function MemberRow({ exchange, selected, onSelect }: {
  exchange: ExchangeSnapshot;
  selected: boolean;
  onSelect: (exchangeId: string) => void;
}) {
  const assignment = exchange.session;
  const badges: string[] = [];
  if (assignment?.fork) badges.push("fork");
  if ((assignment?.repeat_index ?? 0) > 0) badges.push(`×${(assignment?.repeat_index ?? 0) + 1}`);
  if (assignment?.model_changed) badges.push("model changed");
  if (assignment?.tools_changed) badges.push("tools changed");
  const indent = Math.min(Math.max((assignment?.depth ?? 1) - 1, 0), 6);
  const stats = statsLine(exchange.summary);
  return (
    <button
      type="button"
      role="option"
      aria-selected={selected}
      className={`traffic-member-row ${selected ? "selected" : ""}`}
      style={{ paddingLeft: `${10 + indent * 12}px` }}
      onClick={() => onSelect(exchange.exchange_id)}
    >
      <span className="traffic-member-depth">T{assignment?.depth ?? "?"}</span>
      <span className={`state-pill state-${exchange.state}`}>{stateLabel(exchange.state)}</span>
      {badges.length > 0 && <span className="traffic-member-badges">{badges.join(" · ")}</span>}
      <span className="traffic-member-time">{formatWorkspaceTime(exchange.updated_at)}</span>
    </button>
  );
}
