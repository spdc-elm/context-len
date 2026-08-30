import type { GateMode, WorkspacePolicy } from "../contracts";

interface WorkspaceTopbarProps {
  policy: WorkspacePolicy;
  exchangeCount: number;
  heldCount: number;
  theme: "light" | "dark";
  onGateChange: (gate: "request_gate" | "response_gate", value: GateMode) => void;
  onThemeToggle: () => void;
}

function GateControl({
  label,
  value,
  onChange,
}: {
  label: string;
  value: GateMode;
  onChange: () => void;
}) {
  const holding = value === "hold";
  return (
    <div className="gate-control">
      <span className="gate-label">{label}</span>
      <button
        type="button"
        role="switch"
        aria-checked={holding}
        aria-label={`${label} ${holding ? "hold" : "pass"}`}
        className={`switch ${holding ? "on" : ""}`}
        onClick={onChange}
      >
        <span />
      </button>
      <strong>{value}</strong>
    </div>
  );
}

/**
 * The topbar owns only shell-level controls.  Gate controls live here rather
 * than in a second policy row so the workbench keeps one stable chrome area.
 */
export function WorkspaceTopbar({
  policy,
  exchangeCount,
  heldCount,
  theme,
  onGateChange,
  onThemeToggle,
}: WorkspaceTopbarProps) {
  return (
    <header className="topbar">
      <div className="brand-lockup">
        <div className="brand-mark" aria-hidden="true">✦</div>
        <div>
          <div className="brand-name">context<span>-lens</span></div>
          <div className="brand-subtitle">LLM request workbench</div>
        </div>
      </div>

      <div className="topbar-tools">
        <div className="gate-controls" aria-label="Intercept policy">
          <span className="topbar-section-label">Gates</span>
          <GateControl
            label="Request"
            value={policy.request_gate}
            onChange={() => onGateChange("request_gate", policy.request_gate === "hold" ? "pass" : "hold")}
          />
          <GateControl
            label="Response"
            value={policy.response_gate}
            onChange={() => onGateChange("response_gate", policy.response_gate === "hold" ? "pass" : "hold")}
          />
        </div>
        <div className="topbar-status" aria-label="Workspace status">
          <span className="live-dot" />
          <span>LOCAL API</span>
          <span className="status-divider" />
          <span>{exchangeCount} exchanges</span>
          <span className="status-divider" />
          <span>{heldCount} held</span>
        </div>
        <button
          type="button"
          className="theme-toggle"
          aria-label={theme === "dark" ? "Switch to light theme" : "Switch to dark theme"}
          aria-pressed={theme === "dark"}
          onClick={onThemeToggle}
        >
          <span aria-hidden="true">{theme === "dark" ? "☀" : "☾"}</span>
          <span>{theme === "dark" ? "Dark" : "Light"}</span>
        </button>
      </div>
    </header>
  );
}
