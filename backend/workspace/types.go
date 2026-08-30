// Package workspace exposes the local operator workspace API.
//
// The package is deliberately a thin transport layer around exchange and wire
// interfaces.  It serialises exchange snapshots and events (metadata only),
// retrieves artifact bytes on demand, and forwards commands with their
// optimistic-concurrency revision.  It never decodes an artifact body as part
// of list/get/event handling.
package workspace

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"context-lens/backend/exchange"
	"context-lens/backend/policy"
	"context-lens/backend/wire"
)

const (
	// DefaultPrefix is the URL prefix used by Handler when no prefix is
	// configured.  All endpoints are local process APIs; callers can mount the
	// handler below another prefix with http.StripPrefix if needed.
	DefaultPrefix = "/api"

	// DefaultMaxArtifactBytes bounds one artifact response (or requested
	// range).  A range can be used to inspect a portion of a larger stored blob
	// without placing the entire blob in one response.
	DefaultMaxArtifactBytes int64 = 8 << 20

	// DefaultMaxRequestBytes bounds JSON command and policy request bodies.
	DefaultMaxRequestBytes int64 = 1 << 20

	// DefaultMaxSearchBytes bounds a fallback search against stores that do
	// not implement RangeArtifactStore.  A range-capable store can search its
	// backing blob without loading it into this process.
	DefaultMaxSearchBytes int64 = 8 << 20

	DefaultMaxSearchMatches = 1000
	DefaultEventBuffer      = 64
)

// ExchangeBackend is the explicit seam between workspace HTTP and the
// exchange registry.  The methods return frozen runtime-contract snapshots;
// workspace does not reach into exchange's private state.
//
// RegistryAdapter adapts the current exchange.Registry (whose public API is a
// Get method returning *exchange.Exchange) to this interface.  A future
// registry may implement this interface directly and provide complete list
// and event support.
type ExchangeBackend interface {
	ListExchanges() ([]exchange.Snapshot, error)
	GetExchange(exchangeID string) (exchange.Snapshot, error)
	Command(command exchange.Command) (exchange.CommandResult, error)
}

// PagedExchangeBackend is an optional metadata-only page seam. Cursor values are
// opaque to the HTTP layer; implementations must return at most limit rows and
// a non-empty cursor when more rows remain.
type PagedExchangeBackend interface {
	ListExchangesPage(context.Context, int, string) ([]exchange.Snapshot, string, error)
}

// ContextExchangeBackend is an optional context-aware form.  HTTP handlers use
// it in preference to ExchangeBackend's context-free methods when available,
// so a backend can stop expensive persistence work when a browser disconnects.
type ContextExchangeBackend interface {
	ListExchangesContext(context.Context) ([]exchange.Snapshot, error)
	GetExchangeContext(context.Context, string) (exchange.Snapshot, error)
	CommandContext(context.Context, exchange.Command) (exchange.CommandResult, error)
}

// RegistryLookup is the small public portion of exchange.Registry available
// to this package.  Keeping it as an interface avoids coupling workspace to
// registry internals and lets tests use a local registry double.
type RegistryLookup interface {
	Get(exchangeID string) (*exchange.Exchange, bool)
}

// EventSource lets a registry/application publish lifecycle events to the
// workspace stream. The callback must not be retained after the returned
// cancellation function is called.
type EventSource interface {
	Subscribe(func(exchange.Event)) (cancel func())
}

// EventSourceWithEvents is the explicit-name variant accepted by Config.Events
// for integrations that prefer to avoid a generic Subscribe method.
type EventSourceWithEvents interface {
	SubscribeEvents(func(exchange.Event)) (cancel func())
}

// ChannelEventSource is an optional event-source shape for integrations that
// already expose a channel.  The channel is observed until the workspace is
// closed or the source closes it.
type ChannelEventSource interface {
	Events() <-chan exchange.Event
}

// PolicyStore owns the default policy applied to future exchanges.  Existing
// snapshots must remain unchanged when Set succeeds; policy.Store has exactly
// those semantics.
type PolicyStore interface {
	Get() policy.Policy
	Set(policy.Policy) error
}

// ArtifactStore retrieves an immutable body artifact by id.  Implementations
// should return a fresh/independent reader or value on every call.  The
// workspace only calls this for an explicit artifact endpoint, never while
// listing exchanges or emitting events.
type ArtifactStore interface {
	Get(context.Context, string) (wire.BodyArtifact, error)
}

// ArtifactMetadataStore is an optional metadata-only extension. It lets HEAD
// requests and range planning avoid materialising the artifact body. Metadata
// must describe the immutable stored bytes and must not return body content.
type ArtifactMetadataStore interface {
	ArtifactRef(context.Context, string) (wire.ArtifactRef, error)
}

// RangeArtifactStore is an optional extension used to avoid loading a whole
// blob for range reads and searches.  End offsets are exclusive.  Ref must
// return metadata without body bytes; ReadRange must return exact bytes for
// [start,end).
type RangeArtifactStore interface {
	ArtifactRef(context.Context, string) (wire.ArtifactRef, error)
	ReadRange(context.Context, string, int64, int64) ([]byte, error)
	Search(context.Context, string, []byte, int) ([]ArtifactMatch, error)
}

// ArtifactErrorClassifier lets an artifact adapter expose a stable HTTP class
// without leaking implementation error text. Implementations should return a
// workspace error code/message, never body contents or credentials.
type ArtifactErrorClassifier interface {
	ArtifactHTTPError() (status int, code, message string)
}

type CaptureSettings interface {
	CaptureMode() string
	SetCaptureMode(string) error
}

type CaptureSettingsErrorClassifier interface{ CaptureModeError(error) string }
type StorageStats struct {
	MemoryUsed  int64 `json:"memory_used"`
	MemoryLimit int64 `json:"memory_limit"`
	DiskUsed    int64 `json:"disk_used"`
	DiskLimit   int64 `json:"disk_limit"`
}
type StorageStatsProvider interface {
	StorageStats(context.Context) (StorageStats, error)
}
type SessionDeleter interface {
	DeleteSession(context.Context, string) error
}
type SessionDeleteErrorClassifier interface{ SessionDeleteError(error) string }
type Config struct {
	// Backend is preferred.  Registry is a convenience for adapting the
	// current exchange.Registry when no backend has been supplied.
	Backend  ExchangeBackend
	Registry RegistryLookup

	// ClearQueue performs an operator-side queue reset. It should clear
	// exchanges, artifacts, and any derived session index while keeping the
	// runtime usable for new traffic.
	ClearQueue func(context.Context) error

	Artifacts ArtifactStore
	Policy    PolicyStore
	Capture   CaptureSettings
	Storage   StorageStatsProvider
	Sessions  SessionDeleter
	Events    any

	// Prefix is normally /api.  It must be an absolute path and has no
	// trailing slash (except for the root path).
	Prefix string

	MaxArtifactBytes int64
	MaxRequestBytes  int64
	MaxSearchBytes   int64
	MaxSearchMatches int
	MaxExchanges     int
	EventBuffer      int
	Heartbeat        time.Duration
}

// Server implements http.Handler for the workspace API.
type Server struct {
	config     Config
	backend    ExchangeBackend
	artifacts  ArtifactStore
	policy     PolicyStore
	clearQueue func(context.Context) error
	events     *EventBroker

	closeOnce    sync.Once
	done         chan struct{}
	closeHandler func()
}

// New constructs an API server.  It does not listen on a socket; mount Handler
// on an app mux or pass it to httptest.NewServer.
func New(cfg Config) *Server {
	cfg = normalizeConfig(cfg)
	backend := cfg.Backend
	if backend == nil && cfg.Registry != nil {
		adapter := NewRegistryAdapter(cfg.Registry)
		backend = adapter
		if cfg.Artifacts == nil {
			cfg.Artifacts = adapter
		}
	}
	if cfg.Artifacts == nil {
		if store, ok := backend.(ArtifactStore); ok {
			cfg.Artifacts = store
		}
	}
	if cfg.Policy == nil {
		cfg.Policy = policy.NewStore(policy.Default())
	}

	s := &Server{
		config:       cfg,
		backend:      backend,
		artifacts:    cfg.Artifacts,
		policy:       cfg.Policy,
		clearQueue:   cfg.ClearQueue,
		events:       NewEventBroker(cfg.EventBuffer),
		done:         make(chan struct{}),
		closeHandler: func() {},
	}
	s.attachEvents(cfg.Events)
	if cfg.Events == nil {
		// A backend may carry an event source as an optional extension.  Keep
		// this dynamic so the primary backend interface remains small.
		if source, ok := backend.(EventSource); ok {
			s.attachEvents(source)
		} else if source, ok := backend.(ChannelEventSource); ok {
			s.attachChannelEvents(source)
		}
	}
	return s
}

// NewServer is a descriptive constructor alias.
func NewServer(cfg Config) *Server { return New(cfg) }

// NewWithBackend is a convenience constructor for callers that already have
// a registry adapter/backend and do not need a Config literal.
func NewWithBackend(backend ExchangeBackend, artifacts ArtifactStore, policies PolicyStore) *Server {
	return New(Config{Backend: backend, Artifacts: artifacts, Policy: policies})
}

// NewWithRegistry adapts the current exchange.Registry through RegistryAdapter.
// Because that registry currently exposes no List/Subscribe methods, callers
// should call RegistryAdapter.Track after creating an exchange, or provide a
// richer ExchangeBackend when queue discovery/events are required.
func NewWithRegistry(registry RegistryLookup, artifacts ArtifactStore, policies PolicyStore) *Server {
	return New(Config{Registry: registry, Artifacts: artifacts, Policy: policies})
}

func normalizeConfig(cfg Config) Config {
	if cfg.Prefix == "" {
		cfg.Prefix = DefaultPrefix
	}
	if cfg.Prefix[0] != '/' {
		cfg.Prefix = "/" + cfg.Prefix
	}
	for len(cfg.Prefix) > 1 && cfg.Prefix[len(cfg.Prefix)-1] == '/' {
		cfg.Prefix = cfg.Prefix[:len(cfg.Prefix)-1]
	}
	if cfg.MaxArtifactBytes <= 0 {
		cfg.MaxArtifactBytes = DefaultMaxArtifactBytes
	}
	if cfg.MaxRequestBytes <= 0 {
		cfg.MaxRequestBytes = DefaultMaxRequestBytes
	}
	if cfg.MaxSearchBytes <= 0 {
		cfg.MaxSearchBytes = DefaultMaxSearchBytes
	}
	if cfg.MaxSearchMatches <= 0 {
		cfg.MaxSearchMatches = DefaultMaxSearchMatches
	}
	if cfg.MaxExchanges <= 0 {
		cfg.MaxExchanges = 1000
	}
	if cfg.EventBuffer <= 0 {
		cfg.EventBuffer = DefaultEventBuffer
	}
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = 15 * time.Second
	}
	return cfg
}

// Handler returns the server itself as an http.Handler.  It is provided to
// make embedding and httptest setup explicit.
func (s *Server) Handler() http.Handler {
	if s == nil {
		return http.NotFoundHandler()
	}
	return s
}

// Broker returns the process-local event broker.  It is useful to bridge
// exchange lifecycle hooks and to test realtime delivery without a network.
func (s *Server) Broker() *EventBroker {
	if s == nil {
		return nil
	}
	return s.events
}

// Publish emits an exchange event to all current/future SSE consumers.  The
// event is normalised and retained in the bounded broker history.
func (s *Server) Publish(event exchange.Event) exchange.Event {
	if s == nil || s.events == nil {
		return event
	}
	return s.events.Publish(event)
}

// EventSink returns an exchange-compatible callback that publishes every
// lifecycle event into this server's bounded realtime broker. It is intended
// for CreateParams.Events when wiring exchange.Registry to the workspace.
func (s *Server) EventSink() exchange.EventSink {
	if s == nil {
		return nil
	}
	return func(event exchange.Event) { s.Publish(event) }
}
func (s *Server) Close() error {
	if s == nil {
		return errors.New("workspace: nil server")
	}
	s.closeOnce.Do(func() {
		close(s.done)
		if s.closeHandler != nil {
			s.closeHandler()
		}
		if s.events != nil {
			s.events.Close()
		}
	})
	return nil
}

func (s *Server) attachEvents(source any) {
	if source == nil {
		return
	}
	var cancel func()
	switch typed := source.(type) {
	case EventSource:
		cancel = typed.Subscribe(s.receiveEvent)
	case EventSourceWithEvents:
		cancel = typed.SubscribeEvents(s.receiveEvent)
	default:
		return
	}
	if cancel == nil {
		cancel = func() {}
	}
	s.closeHandler = func() { cancel() }
}

func (s *Server) receiveEvent(event exchange.Event) {
	select {
	case <-s.done:
		return
	default:
		s.events.Publish(event)
	}
}

func (s *Server) attachChannelEvents(source ChannelEventSource) {
	if source == nil {
		return
	}
	done := make(chan struct{})
	events := source.Events()
	s.closeHandler = func() { close(done) }
	go func() {
		for {
			select {
			case <-done:
				return
			case event, ok := <-events:
				if !ok {
					return
				}
				s.events.Publish(event)
			}
		}
	}()
}

// Ensure policy.Store and exchange.Registry remain the intended integration
// types at compile time without making them hard dependencies in Config.
var _ PolicyStore = (*policy.Store)(nil)
var _ RegistryLookup = (*exchange.Registry)(nil)
