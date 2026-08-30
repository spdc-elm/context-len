import type { CaptureMode, GateMode, StorageStats, WorkspacePolicy } from "../contracts";

interface WorkspaceTopbarProps {
  policy: WorkspacePolicy;
  exchangeCount: number;
  heldCount: number;
  theme: "light" | "dark";
  onGateChange: (gate: "request_gate" | "response_gate", value: GateMode) => void;
  captureMode: CaptureMode;
  captureBusy?: boolean;
  storage?: StorageStats;
  captureAvailable?: boolean;
  onCaptureToggle: () => void;
  onThemeToggle: () => void;
}

function formatBytes(value: number): string {
  if (!Number.isFinite(value) || value < 0) return "—";
  if (value >= 1 << 30) return `${(value / (1 << 30)).toFixed(1)} GiB`;
  if (value >= 1 << 20) return `${Math.round(value / (1 << 20))} MiB`;
  if (value >= 1 << 10) return `${Math.round(value / (1 << 10))} KiB`;
  return `${Math.round(value)} B`;
}

function GateControl({ label, value, onChange }: { label: string; value: GateMode; onChange: () => void }) {
  const holding = value === "hold";
  return (
    <div className="gate-control">
      <span className="gate-label">{label}</span>
      <button type="button" role="switch" aria-checked={holding} aria-label={`${label} ${holding ? "hold" : "pass"}`} className={`switch ${holding ? "on" : ""}`} onClick={onChange}>
        <span />
      </button>
      <strong>{value}</strong>
    </div>
  );
}

/** Shell-level controls and compact storage status. */
export function WorkspaceTopbar({
  policy,
  exchangeCount,
  heldCount,
  theme,
  onGateChange,
  onThemeToggle,
  captureMode,
  captureBusy = false,
  storage,
  captureAvailable = true,
  onCaptureToggle,
}: WorkspaceTopbarProps) {
  const gateHeld = policy.request_gate === "hold" || policy.response_gate === "hold";
  const captureDisabled = captureBusy || !captureAvailable || (captureMode === "capture" && gateHeld);
  const captureTitle = captureMode === "capture" && gateHeld
    ? "Capture is required while a gate is held"
    : "Applies only to subsequent requests";
  const captureLabel = captureAvailable
    ? `Capture mode: ${captureMode}`
    : "Capture mode unavailable";

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
        <div className="capture-control" title={captureTitle}>
          <span className="topbar-section-label">Mode</span>
          <button type="button" role="switch" aria-checked={captureMode === "capture"} aria-label={captureLabel} className={`switch ${captureMode === "capture" ? "on" : ""}`} disabled={captureDisabled} onClick={onCaptureToggle}>
            <span />
          </button>
          <strong>{captureMode === "capture" ? "Capture" : "Passthrough"}</strong>
        </div>
        <div className="gate-controls" aria-label="Intercept policy">
          <span className="topbar-section-label">Gates</span>
          <GateControl label="Request" value={policy.request_gate} onChange={() => onGateChange("request_gate", policy.request_gate === "hold" ? "pass" : "hold")} />
          <GateControl label="Response" value={policy.response_gate} onChange={() => onGateChange("response_gate", policy.response_gate === "hold" ? "pass" : "hold")} />
        </div>
        <div className="topbar-status" aria-label="Workspace status">
          {storage && <>
            <span>MEM {formatBytes(storage.memory_used)} / {formatBytes(storage.memory_limit)}</span>
            <span className="status-divider" />
            <span>DISK {formatBytes(storage.disk_used)} / {formatBytes(storage.disk_limit)}</span>
            <span className="status-divider" />
          </>}
          <span className="live-dot" />
          <span>LOCAL API</span>
          <span className="status-divider" />
          <span>{exchangeCount} exchanges</span>
          <span className="status-divider" />
          <span>{heldCount} held</span>
        </div>
        <button type="button" className="theme-toggle" aria-label={theme === "dark" ? "Switch to light theme" : "Switch to dark theme"} aria-pressed={theme === "dark"} onClick={onThemeToggle}>
          <span aria-hidden="true">{theme === "dark" ? "☀" : "☾"}</span>
          <span>{theme === "dark" ? "Dark" : "Light"}</span>
        </button>
      </div>
    </header>
  );
}

export { formatBytes };
