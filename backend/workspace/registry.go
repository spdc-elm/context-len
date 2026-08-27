package workspace

import (
	"context"
	"errors"
	"sort"
	"sync"

	"context-lens/backend/exchange"
	"context-lens/backend/wire"
)

// RegistryAdapter exposes the current exchange.Registry through the workspace
// backend seam.  The registry's public API currently supports lookup and
// command execution on an Exchange but not enumeration or event publication;
// Track records ids created by the caller so ListExchanges can still provide a
// deterministic local queue.  A richer registry can be used directly as an
// ExchangeBackend when those capabilities become available.
type RegistryAdapter struct {
	registry RegistryLookup

	// The current concrete registry has List, Command, and Artifact methods
	// in addition to RegistryLookup.Get. Keep these optional so this adapter
	// also works with minimal test doubles.
	lister    interface{ List() []exchange.Snapshot }
	commander interface {
		Command(exchange.Command) (exchange.CommandResult, error)
	}
	artifacts interface {
		Artifact(string) (wire.BodyArtifact, bool)
	}

	mu    sync.RWMutex
	known map[string]struct{}
}

// NewRegistryAdapter constructs an adapter around a registry lookup.  A nil
// registry is accepted so a caller can install it later only by constructing a
// new adapter; operations on the nil adapter return a stable error.
func NewRegistryAdapter(registry RegistryLookup) *RegistryAdapter {
	a := &RegistryAdapter{registry: registry, known: make(map[string]struct{})}
	if registry != nil {
		if lister, ok := registry.(interface{ List() []exchange.Snapshot }); ok {
			a.lister = lister
		}
		if commander, ok := registry.(interface {
			Command(exchange.Command) (exchange.CommandResult, error)
		}); ok {
			a.commander = commander
		}
		if artifacts, ok := registry.(interface {
			Artifact(string) (wire.BodyArtifact, bool)
		}); ok {
			a.artifacts = artifacts
		}
	}
	return a
}

// Track adds an exchange id to the list view.  It is idempotent and safe to
// call immediately after Registry.Create returns.  Track does not perform a
// lookup and therefore does not block a producer that is still initialising.
func (a *RegistryAdapter) Track(exchangeID string) {
	if a == nil || exchangeID == "" {
		return
	}
	a.mu.Lock()
	a.known[exchangeID] = struct{}{}
	a.mu.Unlock()
}

// Untrack removes an id from the adapter's list view.  It does not delete the
// underlying exchange from the registry.
func (a *RegistryAdapter) Untrack(exchangeID string) {
	if a == nil || exchangeID == "" {
		return
	}
	a.mu.Lock()
	delete(a.known, exchangeID)
	a.mu.Unlock()
}

// ListExchanges returns tracked snapshots in lexicographic id order.  Missing
// ids are skipped because an exchange may have been evicted between tracking
// and enumeration; this is not an API error for the remaining queue.
func (a *RegistryAdapter) ListExchanges() ([]exchange.Snapshot, error) {
	if a == nil || a.registry == nil {
		return nil, errors.New("workspace: exchange registry is not configured")
	}
	if a.lister != nil {
		return append([]exchange.Snapshot(nil), a.lister.List()...), nil
	}
	a.mu.RLock()
	ids := make([]string, 0, len(a.known))
	for id := range a.known {
		ids = append(ids, id)
	}
	a.mu.RUnlock()
	sort.Strings(ids)

	out := make([]exchange.Snapshot, 0, len(ids))
	for _, id := range ids {
		e, ok := a.registry.Get(id)
		if !ok || e == nil {
			continue
		}
		out = append(out, e.Snapshot())
	}
	return out, nil
}

// GetExchange looks up and snapshots one exchange.  Successful lookups are
// automatically tracked, making a direct detail request populate subsequent
// list results even when the producer did not explicitly call Track.
func (a *RegistryAdapter) GetExchange(exchangeID string) (exchange.Snapshot, error) {
	if a == nil || a.registry == nil {
		return exchange.Snapshot{}, errors.New("workspace: exchange registry is not configured")
	}
	if exchangeID == "" {
		return exchange.Snapshot{}, exchange.ErrNotFound
	}
	e, ok := a.registry.Get(exchangeID)
	if !ok || e == nil {
		return exchange.Snapshot{}, exchange.ErrNotFound
	}
	a.Track(exchangeID)
	return e.Snapshot(), nil
}

// Command forwards a command to the Exchange returned by the registry.  The
// Exchange itself remains the owner of revision checks and state transitions.
func (a *RegistryAdapter) Command(command exchange.Command) (exchange.CommandResult, error) {
	if a == nil || a.registry == nil {
		return exchange.CommandResult{}, errors.New("workspace: exchange registry is not configured")
	}
	if command.ExchangeID == "" {
		return exchange.CommandResult{}, exchange.ErrNotFound
	}
	if a.commander != nil {
		a.Track(command.ExchangeID)
		return a.commander.Command(command)
	}
	e, ok := a.registry.Get(command.ExchangeID)
	if !ok || e == nil {
		return exchange.CommandResult{}, exchange.ErrNotFound
	}
	a.Track(command.ExchangeID)
	return e.Command(command)
}

// Get retrieves an artifact from a concrete exchange.Registry when it
// exposes Artifact. It allows NewWithRegistry to serve bodies without a
// second store, while still keeping bytes out of snapshots/events.
func (a *RegistryAdapter) Get(ctx context.Context, artifactID string) (wire.BodyArtifact, error) {
	if err := contextErr(ctx); err != nil {
		return wire.BodyArtifact{}, err
	}
	if a == nil || a.artifacts == nil {
		return wire.BodyArtifact{}, ErrArtifactNotFound
	}
	artifact, ok := a.artifacts.Artifact(artifactID)
	if !ok {
		return wire.BodyArtifact{}, ErrArtifactNotFound
	}
	return artifact, nil
}

// Registry reports the wrapped lookup. It is intended for integration code
// that needs to call Track alongside Registry.Create without exposing adapter
// internals.
func (a *RegistryAdapter) Registry() RegistryLookup {
	if a == nil {
		return nil
	}
	return a.registry
}

var _ ExchangeBackend = (*RegistryAdapter)(nil)
var _ ArtifactStore = (*RegistryAdapter)(nil)
