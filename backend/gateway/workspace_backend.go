package gateway

import (
	"context"
	"encoding/json"
	"errors"

	"context-lens/backend/catalog"
	"context-lens/backend/exchange"
	"context-lens/backend/workspace"
)

// workspaceBackend is the production workspace seam. Durable catalogs use
// bounded keyset queries; ephemeral runtimes delegate to the registry adapter.
type workspaceBackend struct {
	gateway  *Gateway
	fallback *workspace.RegistryAdapter
}

func (g *Gateway) WorkspaceBackend() workspace.ExchangeBackend {
	if g == nil {
		return nil
	}
	return &workspaceBackend{gateway: g, fallback: workspace.NewRegistryAdapter(g.registry)}
}

func snapshotFromCatalog(x catalog.Exchange) (exchange.Snapshot, error) {
	if x.Envelope == "" {
		return exchange.Snapshot{}, exchange.ErrNotFound
	}
	var s exchange.Snapshot
	if err := json.Unmarshal([]byte(x.Envelope), &s); err != nil || s.ExchangeID == "" {
		return exchange.Snapshot{}, exchange.ErrNotFound
	}
	return s, nil
}
func (b *workspaceBackend) ListExchanges() ([]exchange.Snapshot, error) {
	if b == nil || b.gateway == nil {
		return nil, errors.New("workspace: backend unavailable")
	}
	if b.gateway.catalog == nil {
		return b.fallback.ListExchanges()
	}
	rows, err := b.gateway.catalog.ListExchanges(context.Background(), "")
	if err != nil {
		return nil, err
	}
	out := make([]exchange.Snapshot, 0, len(rows))
	for _, x := range rows {
		if s, e := snapshotFromCatalog(x); e == nil {
			out = append(out, s)
		}
	}
	return out, nil
}
func (b *workspaceBackend) ListExchangesContext(ctx context.Context) ([]exchange.Snapshot, error) {
	if b == nil || b.gateway == nil {
		return nil, errors.New("workspace: backend unavailable")
	}
	if b.gateway.catalog == nil {
		return b.fallback.ListExchanges()
	}
	rows, err := b.gateway.catalog.ListExchanges(ctx, "")
	if err != nil {
		return nil, err
	}
	out := make([]exchange.Snapshot, 0, len(rows))
	for _, x := range rows {
		if s, e := snapshotFromCatalog(x); e == nil {
			out = append(out, s)
		}
	}
	return out, nil
}
func (b *workspaceBackend) ListExchangesPage(ctx context.Context, limit int, cursor string) ([]exchange.Snapshot, string, error) {
	if b == nil || b.gateway == nil {
		return nil, "", errors.New("workspace: backend unavailable")
	}
	if b.gateway.catalog == nil {
		return b.fallback.ListExchangesPage(ctx, limit, cursor)
	}
	rows, next, err := b.gateway.catalog.ListExchangesPage(ctx, "", limit, cursor)
	if err != nil {
		return nil, "", err
	}
	out := make([]exchange.Snapshot, 0, len(rows))
	for _, x := range rows {
		if s, e := snapshotFromCatalog(x); e == nil {
			out = append(out, s)
		}
	}
	return out, next, nil
}
func (b *workspaceBackend) GetExchange(id string) (exchange.Snapshot, error) {
	return b.GetExchangeContext(context.Background(), id)
}
func (b *workspaceBackend) GetExchangeContext(ctx context.Context, id string) (exchange.Snapshot, error) {
	if b == nil || b.gateway == nil {
		return exchange.Snapshot{}, errors.New("workspace: backend unavailable")
	}
	if b.gateway.catalog == nil {
		return b.fallback.GetExchange(id)
	}
	x, err := b.gateway.catalog.GetExchange(ctx, id)
	if err != nil {
		return exchange.Snapshot{}, err
	}
	return snapshotFromCatalog(x)
}
func (b *workspaceBackend) Command(c exchange.Command) (exchange.CommandResult, error) {
	return b.CommandContext(context.Background(), c)
}
func (b *workspaceBackend) CommandContext(_ context.Context, c exchange.Command) (exchange.CommandResult, error) {
	return b.fallback.Command(c)
}

type captureAdapter struct{ gateway *Gateway }

func (a captureAdapter) CaptureMode() string { return string(a.gateway.CaptureMode()) }
func (a captureAdapter) SetCaptureMode(mode string) error {
	return a.gateway.SetCaptureMode(CaptureMode(mode))
}
func (a captureAdapter) CaptureModeError(err error) string {
	if errors.Is(err, ErrCaptureModeConflict) {
		return "capture_mode_conflict"
	}
	return ""
}
func (g *Gateway) WorkspaceCapture() workspace.CaptureSettings { return captureAdapter{gateway: g} }

func (b *workspaceBackend) SessionDeleteError(err error) string {
	if errors.Is(err, exchange.ErrSessionActive) {
		return "session_active"
	}
	return ""
}

func (b *workspaceBackend) DeleteSession(ctx context.Context, id string) error {
	if b == nil || b.gateway == nil {
		return errors.New("workspace: backend unavailable")
	}
	return b.gateway.DeleteSession(ctx, id)
}
func (b *workspaceBackend) StorageStats(ctx context.Context) (workspace.StorageStats, error) {
	if b == nil || b.gateway == nil {
		return workspace.StorageStats{}, errors.New("workspace: backend unavailable")
	}
	m, ml, d, dl, err := b.gateway.StorageStats(ctx)
	return workspace.StorageStats{MemoryUsed: m, MemoryLimit: ml, DiskUsed: d, DiskLimit: dl}, err
}

var _ workspace.SessionDeleter = (*workspaceBackend)(nil)
var _ workspace.StorageStatsProvider = (*workspaceBackend)(nil)

var _ workspace.ContextExchangeBackend = (*workspaceBackend)(nil)
