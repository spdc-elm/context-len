import { useMemo, useState } from "react";
import type { ExchangeSnapshot } from "../contracts";
import { formatWorkspaceTime } from "../time";

interface TrafficQueueProps {
  exchanges: ExchangeSnapshot[];
  selectedExchangeId?: string;
  collapsed?: boolean;
  onToggle?: () => void;
  onSelect: (exchangeId: string) => void;
}

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

/** Compact occupancy formatting: 987, 52.3k, 1.2M. Unknown stays visible as —. */
function formatTokens(tokens: number | undefined): string {
  if (tokens === undefined || !Number.isFinite(tokens) || tokens < 0) return "—";
  if (tokens < 1000) return String(tokens);
  if (tokens < 1_000_000) {
    const value = tokens / 1000;
    return `${value >= 100 ? Math.round(value) : value.toFixed(1)}k`;
  }
  return `${(tokens / 1_000_000).toFixed(1)}M`;
}

function rowStatsLine(summary: ExchangeSnapshot["summary"]): string {
  const parts: string[] = [];
  if (summary?.model) parts.push(summary.model);
  if (summary?.message_count) parts.push(`${summary.message_count} msgs`);
  parts.push(`${formatTokens(summary?.context_tokens)} ctx`);
  return parts.join(" · ");
}

function rowPreview(summary: ExchangeSnapshot["summary"]): string | undefined {
  return summary?.preview;
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

export function TrafficQueue({ exchanges, selectedExchangeId, collapsed = false, onToggle, onSelect }: TrafficQueueProps) {
  const [filters, setFilters] = useState<FilterState>(NO_FILTERS);

  const orderedExchanges = useMemo(() => [...exchanges].sort((left, right) => {
    const rightTime = Date.parse(right.updated_at);
    const leftTime = Date.parse(left.updated_at);
    if (rightTime !== leftTime) return rightTime - leftTime;
    return right.exchange_id.localeCompare(left.exchange_id);
  }), [exchanges]);

  const protocolOptions = useMemo(() => uniqueOption(exchanges.map((exchange) => exchange.protocol)), [exchanges]);
  const stateOptions = useMemo(() => uniqueOption(exchanges.map((exchange) => exchange.state)), [exchanges]);
  const modelOptions = useMemo(() => uniqueOption(exchanges.map((exchange) => exchange.summary?.model ?? "")), [exchanges]);

  const filteredExchanges = useMemo(
    () => orderedExchanges.filter((exchange) => matchesFilters(exchange, filters)),
    [orderedExchanges, filters],
  );

  const filtersActive =
    filters.text.trim() !== "" || filters.protocol !== "all" || filters.state !== "all" || filters.model !== "all";

  return (
    <aside className={`traffic-panel ${collapsed ? "collapsed" : ""}`} aria-label="Traffic queue">
      <div className="panel-heading">
        {!collapsed && <div>
          <p className="eyebrow">LIVE TRAFFIC</p>
          <h2>Exchange queue</h2>
        </div>}
        <button type="button" className="collapse-button" aria-label={collapsed ? "Expand traffic" : "Collapse traffic"} onClick={onToggle}>{collapsed ? "›" : "‹"}</button>
      </div>
      {!collapsed && <>
        <p className="panel-note">Live local workspace · events in realtime; bodies load on demand</p>
        <div className="traffic-filters" role="search" aria-label="Exchange filters">
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
        {filteredExchanges.length === 0 ? (
          <div className="empty-state">
            {exchanges.length === 0
              ? "Waiting for an exchange…"
              : filtersActive
                ? "No exchanges match the current filters."
                : "Waiting for an exchange…"}
          </div>
        ) : filteredExchanges.map((exchange) => {
          const chat = isChatProtocol(exchange.protocol);
          const preview = rowPreview(exchange.summary);
          const stats = rowStatsLine(exchange.summary);
          return (
            <button
              type="button"
              role="option"
              aria-selected={exchange.exchange_id === selectedExchangeId}
              className={`traffic-row ${exchange.exchange_id === selectedExchangeId ? "selected" : ""}`}
              key={exchange.exchange_id}
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
        })}
      </div></>}
    </aside>
  );
}
