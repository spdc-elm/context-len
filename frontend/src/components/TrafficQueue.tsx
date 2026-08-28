import type { ExchangeSnapshot } from "../contracts";

interface TrafficQueueProps {
  exchanges: ExchangeSnapshot[];
  selectedExchangeId?: string;
  collapsed?: boolean;
  onToggle?: () => void;
  onSelect: (exchangeId: string) => void;
}

function protocolLabel(protocol: ExchangeSnapshot["protocol"]): string {
  const labels: Record<string, string> = {
    responses: "Responses",
    chat_completions: "Chat Completions",
    anthropic_messages: "Anthropic Messages",
  };
  return labels[protocol] ?? protocol;
}

function stateLabel(state: ExchangeSnapshot["state"]): string {
  return state.replaceAll("_", " ");
}

function requestPath(exchange: ExchangeSnapshot): string {
  const { method, path, raw_query } = exchange.request.envelope;
  return `${method} ${path}${raw_query ? `?${raw_query}` : ""}`;
}

export function TrafficQueue({ exchanges, selectedExchangeId, collapsed = false, onToggle, onSelect }: TrafficQueueProps) {
  const orderedExchanges = [...exchanges].sort((left, right) => {
    const rightTime = Date.parse(right.updated_at);
    const leftTime = Date.parse(left.updated_at);
    if (rightTime !== leftTime) return rightTime - leftTime;
    return right.exchange_id.localeCompare(left.exchange_id);
  });
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
      <div className="traffic-list" role="listbox" aria-label="Exchanges">
        {orderedExchanges.length === 0 ? (
          <div className="empty-state">Waiting for an exchange…</div>
        ) : orderedExchanges.map((exchange) => (
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
              <strong>{protocolLabel(exchange.protocol)}</strong>
              <span className={`state-pill state-${exchange.state}`}>{stateLabel(exchange.state)}</span>
            </div>
            <div className="traffic-row-path">{requestPath(exchange)}</div>
            <div className="traffic-row-meta">
              <span>{exchange.exchange_id}</span>
              <span>r{exchange.updated_at.slice(11, 19)}</span>
            </div>
          </button>
        ))}
      </div></>}
    </aside>
  );
}
