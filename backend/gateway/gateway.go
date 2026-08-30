// Package gateway wires the opaque wire, transport, policy, and exchange
// layers into one local HTTP proxy handler.  It is intentionally a small
// vertical slice: request and response bodies are captured as immutable
// artifacts, while the exchange registry owns gate state and operator
// commands.  No JSON or SSE value is ever used as transport input.
package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"context-lens/backend/auth"
	"context-lens/backend/catalog"
	"context-lens/backend/exchange"
	"context-lens/backend/inspection"
	"context-lens/backend/persistence"
	"context-lens/backend/policy"
	"context-lens/backend/session"
	"context-lens/backend/transport"
	"context-lens/backend/wire"
)

// Config controls Gateway construction.  Upstream may be supplied directly;
// otherwise Transport (or the convenience UpstreamURL) is used to construct
// one.  Registry, Store, and Policy are optional and are created for the
// caller when omitted.
type Config struct {
	Upstream    *transport.Transport
	UpstreamURL string
	Transport   transport.Config

	Registry *exchange.Registry
	Store    *persistence.Store
	// ArtifactStore is a compatibility spelling for Store.  Store wins when
	// both are supplied.
	ArtifactStore *persistence.Store
	StoreConfig   persistence.Config
	// DurableCatalogPath enables explicit restart-safe metadata persistence.
	DurableCatalogPath string

	Policy        *policy.Store
	PolicyStore   *policy.Store
	InitialPolicy policy.Policy

	// AllowNonLoopback requires an explicit embedding decision. The standalone
	// MVP defaults to local mock upstreams and rejects SSRF-prone origins.
	AllowNonLoopback bool

	// ClientAuth optionally protects proxy routes (/v1/*). Healthz remains
	// mounted by the process app server and is intentionally public.
	ClientAuth auth.Config

	// MaxBodyBytes bounds each captured request/response body.  Zero means
	// unlimited.  A body exceeding the limit is rejected rather than being
	// forwarded with a misleading complete artifact.
	MaxBodyBytes int64

	// AnalysisBudget bounds capture-time request inspection. Zero selects the
	// session package default. It never limits or changes wire forwarding.
	AnalysisBudget int64

	// Events is an optional observer.  It receives the same immutable event
	// values as the exchange registry and is suitable for workspace adapters.
	Events exchange.EventSink

	// SessionMaxPositions bounds the session index's in-memory position
	// table. Zero means the session package default. When the bound is
	// exceeded, least-recently-active sessions are evicted wholesale and
	// their follow-ups become fresh roots.
	SessionMaxPositions int

	// ResponseDrainTimeout bounds how long the upstream response keeps
	// being drained after the protocol terminal record was delivered to the
	// client and the client disconnected. Zero selects the default (5s).
	// The drain exists only to complete the response artifact; the exchange
	// is already complete by delivery.
	ResponseDrainTimeout time.Duration
}

const (
	// DefaultResponseDrainTimeout bounds the post-terminal upstream drain when no
	// explicit value is configured.
	DefaultResponseDrainTimeout = 5 * time.Second
	// durableTrafficSession owns protocol snapshots that cannot be assigned to
	// the chat session index (for example Responses-free health/model traffic).
	// Exchanges have a NOT NULL session FK, so these observations still belong
	// to an explicit, unpinned durable ownership unit rather than being dropped.
	durableTrafficSession = "traffic-unassigned"
)

// ErrDurablePersistence is intentionally opaque. Catalog/storage failures can
// contain filesystem or driver details; those details must never cross the
// gateway API or be retained in workspace state.
var ErrDurablePersistence = errors.New("gateway: durable catalog persistence failed")

// Gateway is an HTTP handler and the owner of the concrete integration
// objects needed by workspace.NewWithRegistry.  The registry remains the
// source of truth for exchange state and commands.
type Gateway struct {
	upstream      *transport.Transport
	registry      *exchange.Registry
	store         *persistence.Store
	catalog       *catalog.Catalog
	policy        *policy.Store
	maxBody       int64
	analysisLimit int64
	observer      exchange.EventSink
	clientAuth    auth.Config

	// responseDrainTimeout bounds the post-terminal upstream drain.
	responseDrainTimeout time.Duration

	subMu        sync.RWMutex
	subs         map[uint64]func(exchange.Event)
	subID        atomic.Uint64
	generation   atomic.Uint64
	ingressMu    sync.RWMutex
	durableErrMu sync.Mutex
	durableErr   error

	// sessions places captured requests into session trees. It observes the
	// original inbound bytes only and never contributes transport input.
	sessions *session.Index
}

// New constructs a gateway. It performs all network/upstream validation via
// transport.New and creates a bounded-independent persistence store when one
// was not supplied.
func New(cfg Config) (*Gateway, error) {
	if err := cfg.ClientAuth.Validate(); err != nil {
		return nil, err
	}
	drainTimeout := cfg.ResponseDrainTimeout
	if drainTimeout <= 0 {
		drainTimeout = DefaultResponseDrainTimeout
	}
	upstream := cfg.Upstream
	if upstream == nil {
		tcfg := cfg.Transport
		if tcfg.BaseURL == nil && strings.TrimSpace(tcfg.BaseURLString) == "" {
			tcfg.BaseURLString = cfg.UpstreamURL
		}
		if err := tcfg.HeaderPolicy.Validate(); err != nil {
			return nil, fmt.Errorf("gateway: invalid header policy: %w", err)
		}
		if !cfg.AllowNonLoopback {
			tcfg.RequireLoopback = true
		}
		var err error
		upstream, err = transport.New(tcfg)
		if err != nil {
			return nil, err
		}
	}

	if !cfg.AllowNonLoopback {
		if err := requireLoopbackUpstream(upstream.BaseURL()); err != nil {
			return nil, err
		}
	}

	polStore := cfg.Policy
	if polStore == nil {
		polStore = cfg.PolicyStore
	}
	if polStore == nil {
		initial := cfg.InitialPolicy
		if initial.IsZero() {
			initial = policy.Default()
		}
		polStore = policy.NewStore(initial)
	}
	current := polStore.Get().Normalize()
	if err := current.Validate(); err != nil {
		return nil, fmt.Errorf("gateway: invalid policy: %w", err)
	}

	registry := cfg.Registry
	if registry == nil {
		registry = exchange.NewRegistry(current)
	}

	store := cfg.Store
	ownedStore := false
	if store == nil {
		store = cfg.ArtifactStore
	}
	if store == nil {
		var err error
		storeCfg := cfg.StoreConfig
		if strings.TrimSpace(cfg.DurableCatalogPath) != "" {
			storeCfg.PreserveFilesOnClose = true
			// Durable mode must never silently fall back to memory-only
			// capture. Keep blobs beside the catalog when the embedder did not
			// explicitly choose a spill root.
			if strings.TrimSpace(storeCfg.SpillRoot) == "" && strings.TrimSpace(storeCfg.Root) == "" {
				storeCfg.SpillRoot = filepath.Join(filepath.Dir(cfg.DurableCatalogPath), "artifacts")
			}
		}
		store, err = persistence.NewArtifactStore(storeCfg)
		if err != nil {
			return nil, err
		}
		ownedStore = true
	}

	var durable *catalog.Catalog
	var hydrated []session.HydrationRecord
	if strings.TrimSpace(cfg.DurableCatalogPath) != "" {
		var err error
		durable, err = catalog.Open(cfg.DurableCatalogPath)
		if err != nil {
			if ownedStore {
				_ = store.Close()
			}
			return nil, fmt.Errorf("gateway: durable catalog: %w", err)
		}
		if rows, err := durable.ListExchanges(context.Background(), ""); err != nil {
			_ = durable.Close()
			if ownedStore {
				_ = store.Close()
			}
			return nil, fmt.Errorf("gateway: durable exchanges: %w", err)
		} else {
			for _, x := range rows {
				var snap exchange.Snapshot
				if x.Envelope == "" {
					continue
				}
				if err := json.Unmarshal([]byte(x.Envelope), &snap); err != nil || snap.ExchangeID == "" {
					continue
				}
				if err := registry.Restore(snap); err != nil {
					_ = durable.Close()
					if ownedStore {
						_ = store.Close()
					}
					return nil, fmt.Errorf("gateway: restore exchange: %w", err)
				}
				if snap.Session != nil {
					hydrated = append(hydrated, session.HydrationRecord{ExchangeID: snap.ExchangeID, Protocol: inspection.Protocol(snap.Protocol), Assignment: *snap.Session, ResponseID: x.ResponseExchangeID})
				}
			}
			// Session metadata is rebuilt from snapshots only; artifact bodies remain lazy.
		}
		refs, err := durable.ListAllArtifactRefs(context.Background())
		if err != nil {
			_ = durable.Close()
			if ownedStore {
				_ = store.Close()
			}
			return nil, fmt.Errorf("gateway: durable artifacts: %w", err)
		}
		// Complete catalog-driven deletion before reconciling unknown files. A
		// pending row is authoritative ownership metadata; only after the
		// physical file is gone may it be removed from the catalog.
		if pending, e := durable.PendingBlobDeletes(context.Background()); e != nil {
			_ = durable.Close()
			if ownedStore {
				_ = store.Close()
			}
			return nil, fmt.Errorf("gateway: durable blob deletes: %w", e)
		} else {
			for _, b := range pending {
				if e := store.RemoveContentAddressedBlob(context.Background(), b.StorageRef); e == nil || errors.Is(e, persistence.ErrNotFound) {
					_ = durable.MarkBlobDeleted(context.Background(), b.StorageRef)
				} else if !errors.Is(e, persistence.ErrStoreFull) {
					// Preserve the pending row for a later retry; startup remains
					// available when an external file is temporarily inaccessible.
				}
			}
		}
		referenced := make(map[string]struct{}, len(refs))
		for _, a := range refs {
			referenced[a.StorageRef] = struct{}{}
		}
		if _, e := store.ReconcileSpill(context.Background(), referenced, 256); e != nil {
			_ = durable.Close()
			if ownedStore {
				_ = store.Close()
			}
			return nil, fmt.Errorf("gateway: reconcile durable blobs: %w", e)
		}
		for _, a := range refs {
			if err := store.Adopt(context.Background(), wire.ArtifactRef{ArtifactID: a.ID, Stage: a.Stage, Direction: a.Direction, ContentType: a.ContentType, ContentEncoding: a.ContentEncoding, Size: a.Size, SHA256: a.SHA256, Complete: a.Complete, StorageRef: a.StorageRef}, persistence.StorageRef{Key: a.StorageRef}); err != nil {
				_ = durable.Close()
				if ownedStore {
					_ = store.Close()
				}
				return nil, fmt.Errorf("gateway: restore artifact %s: %w", a.ID, err)
			}
		}
	}

	return &Gateway{
		upstream:             upstream,
		registry:             registry,
		store:                store,
		catalog:              durable,
		policy:               polStore,
		maxBody:              cfg.MaxBodyBytes,
		analysisLimit:        cfg.AnalysisBudget,
		observer:             cfg.Events,
		clientAuth:           cfg.ClientAuth,
		responseDrainTimeout: drainTimeout,
		subs:                 make(map[uint64]func(exchange.Event)),
		sessions: func() *session.Index {
			ix := session.NewIndex(cfg.SessionMaxPositions)
			ix.Hydrate(hydrated)
			return ix
		}(),
	}, nil
}

func requireLoopbackUpstream(u *url.URL) error {
	if u == nil {
		return errors.New("gateway: upstream URL is required")
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("gateway: non-loopback upstream %q requires explicit AllowNonLoopback", host)
	}
	return nil
}

// NewHandler constructs a gateway and returns the pieces needed to mount the
// workspace API.  The first result is a concrete *Gateway (which implements
// http.Handler), so callers may also use its SubscribeEvents method as the
// workspace event source.
func NewHandler(cfg Config) (*Gateway, *exchange.Registry, *persistence.Store, *policy.Store, error) {
	g, err := New(cfg)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return g, g.registry, g.store, g.policy, nil
}

// NewRuntime is an explicit alias for NewHandler for embedding processes.
func NewRuntime(cfg Config) (*Gateway, *exchange.Registry, *persistence.Store, *policy.Store, error) {
	return NewHandler(cfg)
}

// ClearQueue removes the current exchange, artifact, and session indexes
// while keeping the gateway usable for new requests. It is an operator-side
// workspace action and never enters the proxy forwarding path.
func (g *Gateway) ClearQueue(ctx context.Context) error {
	if g == nil || g.registry == nil || g.store == nil || g.sessions == nil {
		return errors.New("gateway: queue unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	// Serialize workspace reset with ingress registration and durable event writes.
	// Body capture remains outside this lock, so Clear can proceed while a client
	// is blocked; the store epoch rejects that pre-clear writer at commit.
	g.ingressMu.Lock()
	defer g.ingressMu.Unlock()
	g.generation.Add(1)
	if err := g.registry.Clear(); err != nil {
		return err
	}
	g.sessions.Clear()
	if err := g.store.Clear(ctx); err != nil {
		return err
	}
	if g.catalog != nil {
		g.durableErrMu.Lock()
		defer g.durableErrMu.Unlock()
		if err := g.catalog.Clear(ctx); err != nil {
			return err
		}
		// Catalog deletion is a two-phase protocol: remove the external file
		// first, then acknowledge the metadata row. Busy files remain pending.
		pending, err := g.catalog.PendingBlobDeletes(ctx)
		if err != nil {
			return err
		}
		for _, b := range pending {
			if err := g.store.RemoveContentAddressedBlob(ctx, b.StorageRef); err == nil || errors.Is(err, persistence.ErrNotFound) {
				_ = g.catalog.MarkBlobDeleted(ctx, b.StorageRef)
			}
		}
	}
	return nil
}

// DurableError reports the most recent catalog persistence failure, if any.
func (g *Gateway) DurableError() error {
	if g == nil {
		return nil
	}
	g.durableErrMu.Lock()
	defer g.durableErrMu.Unlock()
	return g.durableErr
}

func (g *Gateway) Close() error {
	if g == nil {
		return nil
	}
	// Invalidate callbacks racing shutdown/queue clear before closing durable
	// handles. Persistence failures are diagnostic only and must never alter
	// normal proxy traffic.
	g.generation.Add(1)
	var first error
	if g.catalog != nil {
		g.durableErrMu.Lock()
		first = g.catalog.Close()
		g.durableErrMu.Unlock()
	}
	if g.store != nil {
		if err := g.store.Close(); first == nil {
			first = err
		}
	}
	return first
}

func (g *Gateway) Registry() *exchange.Registry {
	if g == nil {
		return nil
	}
	return g.registry
}

// Store returns the shared immutable artifact store.
func (g *Gateway) Store() *persistence.Store {
	if g == nil {
		return nil
	}
	return g.store
}

// Policy returns the policy store used to snapshot every new exchange.
func (g *Gateway) Policy() *policy.Store {
	if g == nil {
		return nil
	}
	return g.policy
}

// Handler returns g as an http.Handler, with a not-found fallback for nil.
func (g *Gateway) Handler() http.Handler {
	if g == nil {
		return http.NotFoundHandler()
	}
	return g
}

// SubscribeEvents implements workspace.EventSourceWithEvents.  Existing
// events are not replayed; a subscriber observes events emitted after it is
// registered.  The callback is removed by the returned cancellation function.
func (g *Gateway) SubscribeEvents(fn func(exchange.Event)) func() {
	if g == nil || fn == nil {
		return func() {}
	}
	id := g.subID.Add(1)
	g.subMu.Lock()
	g.subs[id] = fn
	g.subMu.Unlock()
	return func() {
		g.subMu.Lock()
		delete(g.subs, id)
		g.subMu.Unlock()
	}
}

// Subscribe is an alias accepted by workspace.EventSource adapters.
func (g *Gateway) Subscribe(fn func(exchange.Event)) func() { return g.SubscribeEvents(fn) }

// ServeHTTP captures and registers one HTTP request, then waits until the
// exchange reaches a terminal state.  A held request therefore remains open
// until a command arrives through the shared registry.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if g == nil || g.upstream == nil || g.registry == nil || g.store == nil || g.policy == nil {
		http.Error(w, "gateway unavailable", http.StatusBadGateway)
		return
	}
	if r == nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !g.clientAuth.Authorize(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="context-lens"`)
		http.Error(w, "context-lens: authentication required", http.StatusUnauthorized)
		return
	}

	// Snapshot policy at ingress.  A later policy update affects only future
	// requests, never an exchange whose request body is still being captured.
	pol := g.policy.Get().Normalize()
	if err := pol.Validate(); err != nil {
		http.Error(w, "gateway policy unavailable", http.StatusInternalServerError)
		return
	}

	// Capture outside the ingress lock so Clear can invalidate blocked captures.
	requestGeneration := g.generation.Load()
	requestArtifact, err := g.captureRequest(r)
	if err != nil {
		// captureStored may have committed an incomplete artifact before
		// reporting a request-body read/limit error. Request capture owns that
		// artifact until ingress succeeds, so remove it on every error path.
		// Cleanup is best-effort and must never replace the original error.
		if id := requestArtifact.Ref().ArtifactID; id != "" {
			_ = g.store.Delete(context.Background(), id)
		}
		g.writeCaptureError(w, err)
		return
	}
	if requestGeneration != g.generation.Load() {
		_ = g.store.Delete(context.Background(), requestArtifact.Ref().ArtifactID)
		g.writeCaptureError(w, persistence.ErrStaleArtifact)
		return
	}
	// captureRequest commits directly to the store and returns a lazy handle.

	requestEnvelope := wire.RequestFromHTTP(r)
	preview := artifactPreview(requestArtifact, 64*1024)
	protocol := detectProtocol(requestEnvelope, preview)
	// Analyze through the lazy artifact reader with a hard memory budget. The
	// preview is used only for protocol detection; it is never treated as a
	// complete request for session identity.
	var analysis session.RequestAnalysis
	if reader, openErr := requestArtifact.OpenReader(); openErr == nil {
		analysis, _ = session.AnalyzeRequestReader(inspection.Protocol(protocol), reader, g.analysisBudget())
		_ = reader.Close()
	}
	var summary *session.Summary
	if !analysis.Summary.Empty() {
		summary = &analysis.Summary
	}
	exchangeID := exchange.NewExchangeID()
	var placement *session.Assignment
	if session.IsChatProtocol(inspection.Protocol(protocol)) && analysis.AnalysisComplete {
		assignment := g.sessions.Assign(exchangeID, session.RequestFacts{
			Protocol:           inspection.Protocol(protocol),
			MessageDigests:     analysis.MessageDigests,
			Model:              analysis.Summary.Model,
			ToolsDigest:        analysis.ToolsDigest,
			PreviousResponseID: analysis.PreviousResponseID,
		})
		placement = &assignment
	}
	committed := &writeState{}
	directWritten := &atomic.Bool{}
	direct := !pol.ResponseHeld()

	upstream := func(ctx context.Context, req exchange.UpstreamRequest) (exchange.UpstreamResponse, error) {
		return g.roundTrip(ctx, r, req, w, committed, directWritten, direct, protocol)
	}
	downstream := func(ctx context.Context, resp exchange.DownstreamResponse) error {
		// In the pass/pass (and hold/pass) path roundTrip streams directly as
		// soon as response headers arrive. Registry still invokes its ordinary
		// writer hook after the artifact is committed; make that second call a
		// no-op. Manual responses never set directWritten and therefore use the
		// same writer below.
		if directWritten.Load() {
			return nil
		}
		return g.writeDownstream(ctx, w, resp, committed)
	}

	g.ingressMu.RLock()
	if requestGeneration != g.generation.Load() {
		g.ingressMu.RUnlock()
		_ = g.store.Delete(context.Background(), requestArtifact.Ref().ArtifactID)
		g.writeCaptureError(w, persistence.ErrStaleArtifact)
		return
	}
	e, err := g.registry.Create(exchange.CreateParams{
		ExchangeID:      exchangeID,
		Protocol:        protocol,
		RequestEnvelope: requestEnvelope,
		RequestArtifact: requestArtifact,
		PolicyOverride:  &pol,
		Upstream:        upstream,
		Downstream:      downstream,
		Context:         r.Context(),
		Events:          g.eventSink,
		Summary:         summary,
		Session:         placement,
	})
	g.ingressMu.RUnlock()
	if err != nil {
		g.writeStoreError(w, err)
		return
	}

	// A background wait is intentional.  The exchange context is already tied
	// to r.Context, so disconnects cancel upstream; using that context to bound
	// Wait would return before the registry can commit its terminal state.
	final, waitErr := e.Wait(context.Background())
	if waitErr != nil && !committed.done() {
		http.Error(w, "gateway exchange wait failed", http.StatusBadGateway)
		return
	}
	if committed.done() {
		return
	}
	g.writeTerminal(w, r, final)
}

func detectProtocol(envelope wire.RequestEnvelope, body []byte) string {
	hint := inspection.DetectProtocol(envelope.Path, envelope.Headers.Get("Content-Type"), envelope.Headers, body)
	if hint.Protocol == "" {
		return string(inspection.ProtocolUnknown)
	}
	return string(hint.Protocol)
}

func (g *Gateway) captureRequest(r *http.Request) (wire.BodyArtifact, error) {
	body := r.Body
	if body == nil {
		body = http.NoBody
	}
	return captureStored(r.Context(), g.store, body, wire.ArtifactOptions{
		Stage: wire.StageRequestInbound, Direction: wire.DirectionInbound,
		ContentType: r.Header.Get("Content-Type"), ContentEncoding: r.Header.Get("Content-Encoding"),
	}, g.maxBody)
}

// captureStored spools bytes directly into the artifact repository. No body
// slice is retained on ordinary forwarding paths; the returned artifact is lazy.
func captureStored(ctx context.Context, store *persistence.Store, body io.ReadCloser, opts wire.ArtifactOptions, max int64) (wire.BodyArtifact, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if body == nil {
		body = http.NoBody
	}
	defer body.Close()
	stopClose := make(chan struct{})
	defer close(stopClose)
	go func() {
		select {
		case <-ctx.Done():
			_ = body.Close()
		case <-stopClose:
		}
	}()
	writer, err := store.BeginWriter(ctx, opts)
	if err != nil {
		return wire.BodyArtifact{}, err
	}
	buf := make([]byte, 32*1024)
	complete := false
	var readErr error
	var size int64
	for {
		if err := contextError(ctx); err != nil {
			readErr = err
			break
		}
		n, er := body.Read(buf)
		if n > 0 {
			if max > 0 && size+int64(n) > max {
				allowed := max - size
				if allowed > 0 {
					_, _ = writer.Write(buf[:allowed])
					size += allowed
				}
				readErr = wire.ErrCaptureLimit
				break
			}
			if _, err := writer.Write(buf[:n]); err != nil {
				readErr = err
				break
			}
			size += int64(n)
		}
		if er != nil {
			if errors.Is(er, io.EOF) {
				complete = true
			} else {
				readErr = er
			}
			break
		}
	}
	ref, commitErr := writer.Commit(context.Background(), complete)
	if commitErr != nil {
		return wire.BodyArtifact{}, commitErr
	}
	artifact, lazyErr := store.Lazy(context.Background(), ref.ArtifactID)
	if lazyErr != nil {
		return wire.BodyArtifact{}, lazyErr
	}
	if readErr != nil {
		return artifact, readErr
	}
	return artifact, nil
}

func (g *Gateway) analysisBudget() int64 {
	if g == nil || g.analysisLimit <= 0 {
		return session.DefaultAnalysisBudget
	}
	return g.analysisLimit
}
func artifactPreview(a wire.BodyArtifact, limit int64) []byte {
	if limit <= 0 {
		return nil
	}
	r, err := a.OpenReader()
	if err != nil {
		return nil
	}
	defer r.Close()
	b, _ := io.ReadAll(io.LimitReader(r, limit))
	return b
}

func (g *Gateway) roundTrip(ctx context.Context, inbound *http.Request, req exchange.UpstreamRequest, w http.ResponseWriter, committed *writeState, directWritten *atomic.Bool, direct bool, protocol string) (exchange.UpstreamResponse, error) {
	if err := contextError(ctx); err != nil {
		return exchange.UpstreamResponse{}, err
	}
	gen := g.generation.Load()
	// Reject work invalidated by Clear before creating fresh references.
	if gen != g.generation.Load() {
		return exchange.UpstreamResponse{}, persistence.ErrStaleArtifact
	}
	// The inbound request was persisted at ingress. Link its immutable blob for
	// the upstream-stage reference; this avoids reading/copying the body. The
	// legacy fallback is only for callers constructing an unpersisted artifact.
	upstreamRef, err := g.store.Link(context.Background(), req.Artifact.Ref().ArtifactID, wire.ArtifactOptions{
		Stage: wire.StageRequestUpstream, Direction: wire.DirectionUpstream,
		ContentType: req.Artifact.Ref().ContentType, ContentEncoding: req.Artifact.Ref().ContentEncoding,
	})
	if err != nil {
		if !errors.Is(err, persistence.ErrNotFound) {
			return exchange.UpstreamResponse{}, err
		}
		upstreamArtifact := copyArtifact(req.Artifact, wire.StageRequestUpstream, wire.DirectionUpstream)
		if err := g.putArtifact(upstreamArtifact); err != nil {
			return exchange.UpstreamResponse{}, err
		}
		upstreamRef = upstreamArtifact.Ref()
	}
	if gen != g.generation.Load() {
		_ = g.store.Delete(context.Background(), upstreamRef.ArtifactID)
		return exchange.UpstreamResponse{}, persistence.ErrStaleArtifact
	}
	if err := g.registry.AddArtifactRef(req.ExchangeID, true, upstreamRef); err != nil {
		return exchange.UpstreamResponse{}, err
	}

	outbound := inbound.Clone(ctx)
	outbound.Body = req.Artifact.Open()
	outbound.GetBody = func() (io.ReadCloser, error) { return req.Artifact.Open(), nil }
	outbound.ContentLength = req.Artifact.Len()
	outbound.TransferEncoding = nil
	if req.Artifact.Len() == 0 {
		outbound.Body = http.NoBody
		outbound.GetBody = func() (io.ReadCloser, error) { return http.NoBody, nil }
	}

	started := time.Now().UTC()
	// The upstream leg owns its own context. A downstream client disconnect
	// cancels the upstream request only until the protocol terminal record
	// has been delivered; after that the response is drained for a complete
	// artifact (bounded by the drain timeout) instead of being aborted.
	upCtx, upCancel := context.WithCancel(context.Background())
	defer upCancel()
	prepared, err := g.upstream.PrepareRequest(upCtx, outbound, nil)
	if err != nil {
		return exchange.UpstreamResponse{}, err
	}
	if warnings := safeHeaderDiff(outbound.Header, prepared.Header); len(warnings) > 0 {
		_ = g.registry.AddWarnings(req.ExchangeID, warnings...)
	}
	resp, err := g.upstream.Client().Do(prepared)
	if err != nil {
		return exchange.UpstreamResponse{}, err
	}
	if resp == nil {
		return exchange.UpstreamResponse{}, errors.New("gateway: upstream returned nil response")
	}
	if resp.Body == nil {
		resp.Body = http.NoBody
	}
	// Observation tap for streaming responses: a copy of every chunk is fed
	// to the SSE scanner so workspace subscribers see typed stream events in
	// real time.  The tap never touches the bytes the transport copies onward.
	var tap *streamTap
	resp.Body, tap = g.attachStreamTap(req.ExchangeID, resp.Body, resp.Header.Get("Content-Type"), protocol)
	defer resp.Body.Close()

	if tap != nil {
		stopWatch := make(chan struct{})
		defer close(stopWatch)
		go func() {
			select {
			case <-ctx.Done():
				// The client is gone. Unless the terminal record already reached
				// it (the response then drains to a completed artifact), the
				// upstream leg is released immediately.
				if !tap.isTerminalDelivered() && tap.terminalEnd == 0 {
					upCancel()
				}
			case <-stopWatch:
			}
		}()
	}

	if direct {
		streamed, err := g.captureAndStream(ctx, req.ExchangeID, resp, w, committed, tap, upCancel)
		if err != nil {
			// Once the scanner has observed a terminal record and its bytes were
			// delivered, client cancellation is a successful exchange outcome;
			// preserve the captured artifact even if the transport closes during
			// the final drain read.
			if tap != nil && tap.terminalEnd > 0 && streamed.deliveredLen >= tap.terminalEnd && streamed.deliveredRef.ArtifactID != "" {
				err = nil
			} else {
				// The bytes already streamed to the client stay inspectable even
				// when the stream was cut short (client disconnect or write
				// failure): retain the partial capture instead of dropping it.
				var deliveredRef *wire.ArtifactRef
				if streamed.deliveredRef.ArtifactID != "" {
					deliveredRef = &streamed.deliveredRef
				}
				g.retainPartialResponseRef(req.ExchangeID, streamed.artifact, protocol, deliveredRef)
				return exchange.UpstreamResponse{}, err
			}
		}
		directWritten.Store(true)
		// The downstream copy is exactly the bytes that reached the client;
		// captureAndStream commits that prefix directly with the downstream
		// stage and StorageRef. Reuse that exact reference rather than linking
		// the upstream artifact (which would leave the delivered entry orphaned).
		downstreamRef := streamed.deliveredRef
		if downstreamRef.ArtifactID == "" {
			return exchange.UpstreamResponse{}, errors.New("gateway: downstream capture unavailable")
		}
		if err := g.registry.AddArtifactRef(req.ExchangeID, false, downstreamRef); err != nil {
			return exchange.UpstreamResponse{}, err
		}
		g.noteContextTokens(req.ExchangeID, protocol, artifactPreview(streamed.artifact, 256*1024))
		g.noteResponseID(req.ExchangeID, protocol, artifactPreview(streamed.artifact, 256*1024))
		return exchange.UpstreamResponse{Envelope: envelopeWithTimes(resp, started), Artifact: streamed.artifact}, nil
	}

	artifact, err := captureStored(ctx, g.store, resp.Body, wire.ArtifactOptions{Stage: wire.StageResponseUpstream, Direction: wire.DirectionUpstream, ContentType: resp.Header.Get("Content-Type"), ContentEncoding: resp.Header.Get("Content-Encoding")}, g.maxBody)
	if err != nil {
		// The captured prefix of the upstream response remains available for
		// review even though the leg was interrupted.
		g.retainPartialResponse(req.ExchangeID, artifact, protocol, false)
		return exchange.UpstreamResponse{}, err
	}
	if !artifact.Ref().Complete {
		return exchange.UpstreamResponse{}, errors.New("gateway: response capture incomplete")
	}
	g.noteContextTokens(req.ExchangeID, protocol, artifactPreview(artifact, 256*1024))
	g.noteResponseID(req.ExchangeID, protocol, artifactPreview(artifact, 256*1024))
	return exchange.UpstreamResponse{
		Envelope: envelopeWithTimes(resp, started),
		Artifact: artifact,
	}, nil
}

func safeHeaderDiff(inbound, outbound http.Header) []string {
	var warnings []string
	seen := make(map[string]bool)
	for name := range inbound {
		canonical := http.CanonicalHeaderKey(name)
		seen[strings.ToLower(canonical)] = true
		if _, ok := outbound[canonical]; !ok {
			warnings = append(warnings, "upstream header policy removed "+canonical)
		}
	}
	for name := range outbound {
		canonical := http.CanonicalHeaderKey(name)
		if !seen[strings.ToLower(canonical)] {
			warnings = append(warnings, "upstream header policy added "+canonical)
		}
	}
	sort.Strings(warnings)
	return warnings
}

func envelopeWithTimes(resp *http.Response, started time.Time) wire.ResponseEnvelope {
	return wire.ResponseFromHTTP(resp, started, time.Now().UTC())
}

// streamOutcome carries what the direct streaming path produced: the full
// upstream capture (the response artifact) and the byte prefix that actually
// reached the client.
type streamOutcome struct {
	artifact     wire.BodyArtifact
	envelope     wire.ResponseEnvelope
	deliveredLen int64
	deliveredRef wire.ArtifactRef
}

// captureAndStream performs a byte-for-byte response copy while retaining a
// complete immutable artifact. It writes no synthetic SSE delimiters and does
// not inspect JSON.  Headers/body still reach pass-through clients immediately.
//
// Once the protocol terminal record has been delivered to the client, the
// downstream connection's context no longer governs the loop: a client that
// hung up right after the terminal record (the normal lifecycle of streaming
// harnesses) leaves the upstream leg draining until EOF or the drain timeout,
// so the exchange completes with the fullest artifact available.
func (g *Gateway) captureAndStream(ctx context.Context, exchangeID string, resp *http.Response, w http.ResponseWriter, committed *writeState, tap *streamTap, upCancel context.CancelFunc) (streamOutcome, error) {
	started := time.Now().UTC()
	envelope := wire.ResponseFromHTTP(resp, started, time.Time{})
	if err := writeResponseHeaders(w, envelope, committed); err != nil {
		return streamOutcome{}, err
	}
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	upOpts := wire.ArtifactOptions{Stage: wire.StageResponseUpstream, Direction: wire.DirectionUpstream, ContentType: resp.Header.Get("Content-Type"), ContentEncoding: resp.Header.Get("Content-Encoding")}
	downOpts := wire.ArtifactOptions{Stage: wire.StageResponseDownstream, Direction: wire.DirectionDownstream, ContentType: resp.Header.Get("Content-Type"), ContentEncoding: resp.Header.Get("Content-Encoding")}
	upWriter, err := g.store.BeginWriter(context.Background(), upOpts)
	if err != nil {
		return streamOutcome{}, err
	}
	downWriter, err := g.store.BeginWriter(context.Background(), downOpts)
	if err != nil {
		_ = upWriter.Abort()
		return streamOutcome{}, err
	}
	captured := int64(0)
	delivered := int64(0)
	captureIncomplete := false
	downIncomplete := false
	draining, clientGone := false, false
	var drainTimer *time.Timer
	var drainTimedOut atomic.Bool
	defer func() {
		if drainTimer != nil {
			drainTimer.Stop()
		}
	}()
	finish := func(complete bool, retErr error) (streamOutcome, error) {
		// Always finalize both legs exactly once, including cancellation and
		// writer failures. A committed incomplete ref is preferable to a
		// dangling capture and remains useful for inspection.
		upRef, ce := upWriter.Commit(context.Background(), complete && !captureIncomplete)
		if ce != nil {
			_ = downWriter.Abort()
			return streamOutcome{}, ce
		}
		downComplete := complete && !captureIncomplete && !downIncomplete
		downRef, de := downWriter.Commit(context.Background(), downComplete)
		artifact, le := g.store.Lazy(context.Background(), upRef.ArtifactID)
		if le != nil {
			return streamOutcome{}, le
		}
		if de != nil {
			// The upstream ref is already committed; return it so callers can
			// retain it even when the downstream leg cannot be committed.
			return streamOutcome{artifact: artifact, envelope: envelopeWithTimes(resp, started), deliveredLen: delivered}, de
		}
		return streamOutcome{artifact: artifact, envelope: envelopeWithTimes(resp, started), deliveredLen: delivered, deliveredRef: downRef}, retErr
	}
	buf := make([]byte, 32*1024)
	for {
		// A disconnect can race the write that crosses the terminal boundary.
		// Re-check the tap before honoring cancellation so the already-delivered
		// terminal record still enters the post-terminal drain path.
		if !draining && tap != nil && (tap.isTerminalDelivered() || (tap.terminalEnd > 0 && delivered >= tap.terminalEnd)) {
			draining = true
			drainTimer = time.AfterFunc(g.responseDrainTimeout, func() { drainTimedOut.Store(true); upCancel() })
		}
		if !draining {
			if e := contextError(ctx); e != nil {
				// The scanner may observe the terminal record just before the
				// downstream write and client cancellation. Finish that chunk and
				// drain rather than misclassifying the exchange as cancelled.
				if tap == nil || tap.terminalEnd == 0 {
					return finish(false, e)
				}
				draining = true
				drainTimer = time.AfterFunc(g.responseDrainTimeout, func() { drainTimedOut.Store(true); upCancel() })
			}
		}
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if !captureIncomplete {
				allowed := n
				if g.maxBody > 0 && captured+int64(allowed) > g.maxBody {
					allowed = int(g.maxBody - captured)
					if allowed < 0 {
						allowed = 0
					}
					captureIncomplete = true
				}
				if allowed > 0 {
					if _, e := upWriter.Write(buf[:allowed]); e != nil {
						captureIncomplete = true
					}
					captured += int64(allowed)
				}
			}
			if !clientGone {
				written, writeErr := w.Write(buf[:n])
				if written > 0 {
					if _, e := downWriter.Write(buf[:written]); e != nil {
						downIncomplete = true
					}
					delivered += int64(written)
				}
				if writeErr == nil && written == n {
					if tap != nil {
						tap.markTerminalDelivered(delivered)
					}
					if !draining && tap != nil && (tap.isTerminalDelivered() || (tap.terminalEnd > 0 && delivered >= tap.terminalEnd)) {
						draining = true
						drainTimer = time.AfterFunc(g.responseDrainTimeout, func() { drainTimedOut.Store(true); upCancel() })
					}
					if flusher != nil {
						flusher.Flush()
					}
				} else if !draining {
					if writeErr == nil {
						writeErr = io.ErrShortWrite
					}
					return finish(false, writeErr)
				} else {
					clientGone = true
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				finalEnvelope := wire.ResponseFromHTTP(resp, started, time.Now().UTC())
				setResponseTrailers(w, finalEnvelope.Trailers)
				out, e := finish(true, nil)
				out.envelope = finalEnvelope
				return out, e
			}
			if draining {
				g.noteDrainWarning(exchangeID, drainTimedOut.Load())
				return finish(false, nil)
			}
			return finish(false, readErr)
		}
	}
}

// noteDrainWarning records why a post-terminal drain ended without an
// upstream EOF. Observation only: it never changes the exchange outcome.
func (g *Gateway) noteDrainWarning(exchangeID string, timedOut bool) {
	if g == nil || g.registry == nil {
		return
	}
	if timedOut {
		_ = g.registry.AddWarnings(exchangeID, fmt.Sprintf("response drain timed out after %s; artifact incomplete", g.responseDrainTimeout))
	} else {
		_ = g.registry.AddWarnings(exchangeID, "upstream response ended during post-terminal drain; artifact incomplete")
	}
}

// retainPartialResponse preserves the observed bytes of an interrupted
// response leg. A cancelled exchange keeps whatever the upstream sent
// (response.upstream) and, when the direct path already streamed those bytes
// to the client, the delivered copy (response.downstream). The artifacts stay
// incomplete because no upstream EOF was observed. Retention is
// observation-only: failures are dropped because they must never change the
// transport error reported to the exchange.
func (g *Gateway) retainPartialResponse(exchangeID string, artifact wire.BodyArtifact, protocol string, streamedDownstream bool) {
	var downstreamRef *wire.ArtifactRef
	if streamedDownstream {
		if ref := artifact.Ref(); ref.ArtifactID != "" {
			downstreamRef = &ref
		}
	}
	g.retainPartialResponseRef(exchangeID, artifact, protocol, downstreamRef)
}

func (g *Gateway) retainPartialResponseRef(exchangeID string, artifact wire.BodyArtifact, protocol string, downstreamRef *wire.ArtifactRef) {
	if g == nil || g.registry == nil || artifact.Ref().ArtifactID == "" {
		return
	}
	_ = g.registry.AddWarnings(exchangeID, "response stream interrupted; partial response retained")
	if err := g.putArtifact(artifact); err != nil {
		return
	}
	if err := g.registry.AddArtifactRef(exchangeID, false, artifact.Ref()); err != nil {
		return
	}
	if downstreamRef != nil {
		ref := *downstreamRef
		if ref.Stage == wire.StageResponseDownstream {
			_ = g.registry.AddArtifactRef(exchangeID, false, ref)
			g.noteContextTokens(exchangeID, protocol, artifactPreview(artifact, 256*1024))
			g.noteResponseID(exchangeID, protocol, artifactPreview(artifact, 256*1024))
			return
		}
		// A legacy caller may provide only the upstream artifact. Link it
		// without materializing the body.
		linked, linkErr := g.store.Link(context.Background(), ref.ArtifactID, wire.ArtifactOptions{Stage: wire.StageResponseDownstream, Direction: wire.DirectionDownstream, ContentType: ref.ContentType, ContentEncoding: ref.ContentEncoding})
		if linkErr == nil {
			_ = g.registry.AddArtifactRef(exchangeID, false, linked)
		}
	}
	g.noteContextTokens(exchangeID, protocol, artifactPreview(artifact, 256*1024))
	g.noteResponseID(exchangeID, protocol, artifactPreview(artifact, 256*1024))
}

func (g *Gateway) writeDownstream(ctx context.Context, w http.ResponseWriter, response exchange.DownstreamResponse, committed *writeState) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if !response.Artifact.Ref().Complete {
		return errors.New("gateway: refusing incomplete response artifact")
	}
	downstreamRef, err := g.store.Link(context.Background(), response.Artifact.Ref().ArtifactID, wire.ArtifactOptions{
		Stage: wire.StageResponseDownstream, Direction: wire.DirectionDownstream,
		ContentType: response.Artifact.Ref().ContentType, ContentEncoding: response.Artifact.Ref().ContentEncoding,
	})
	if err != nil {
		if !errors.Is(err, persistence.ErrNotFound) {
			return err
		}
		// Legacy/unpersisted exchange artifacts may still arrive from direct
		// callers; only that compatibility path materializes a copy.
		downstreamArtifact := copyArtifact(response.Artifact, wire.StageResponseDownstream, wire.DirectionDownstream)
		if err := g.putArtifact(downstreamArtifact); err != nil {
			return err
		}
		downstreamRef = downstreamArtifact.Ref()
	}
	if err := g.registry.AddArtifactRef(response.ExchangeID, false, downstreamRef); err != nil {
		return err
	}
	envelope := response.Envelope.Clone()
	envelope.Headers.Del("Transfer-Encoding")
	envelope.Headers.Set("Content-Length", strconv.FormatInt(response.Artifact.Len(), 10))
	if err := writeResponseHeaders(w, envelope, committed); err != nil {
		return err
	}
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	reader := response.Artifact.Open()
	defer reader.Close()
	buf := make([]byte, 32*1024)
	for {
		if err := contextError(ctx); err != nil {
			return err
		}
		n, readErr := reader.Read(buf)
		if n > 0 {
			written, writeErr := w.Write(buf[:n])
			if writeErr != nil {
				return writeErr
			}
			if written != n {
				return io.ErrShortWrite
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				setResponseTrailers(w, response.Envelope.Trailers)
				return nil
			}
			return readErr
		}
	}
}

type writeState struct{ committed atomic.Bool }

func (s *writeState) mark() {
	if s != nil {
		s.committed.Store(true)
	}
}
func (s *writeState) done() bool { return s != nil && s.committed.Load() }

func writeResponseHeaders(w http.ResponseWriter, envelope wire.ResponseEnvelope, state *writeState) error {
	if w == nil {
		return errors.New("gateway: nil response writer")
	}
	env := envelope.Clone()
	status := env.Status
	if status == 0 {
		status = http.StatusOK
	}
	if status < 100 || status > 599 {
		return fmt.Errorf("gateway: invalid upstream status %d", status)
	}
	nominated := make(map[string]struct{})
	for name, values := range env.Headers {
		if strings.EqualFold(name, "Connection") {
			for _, value := range values {
				for _, token := range strings.Split(value, ",") {
					if token = strings.ToLower(strings.TrimSpace(token)); token != "" {
						nominated[token] = struct{}{}
					}
				}
			}
		}
	}
	for name, values := range env.Headers {
		if _, nominatedHop := nominated[strings.ToLower(strings.TrimSpace(name))]; nominatedHop {
			continue
		}
		if responseHopHeader(name) {
			continue
		}
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	// Trailers must be declared before WriteHeader. Keep the upstream Trailer
	// declaration if present and add names discovered after body capture.
	for name := range env.Trailers {
		if responseHopHeader(name) || strings.EqualFold(name, "Trailer") {
			continue
		}
		if !headerTokenPresent(w.Header().Values("Trailer"), name) {
			w.Header().Add("Trailer", name)
		}
	}
	if state != nil {
		state.mark()
	}
	w.WriteHeader(status)
	return nil
}

func setResponseTrailers(w http.ResponseWriter, trailers http.Header) {
	for name, values := range trailers {
		if responseHopHeader(name) || strings.EqualFold(name, "Trailer") {
			continue
		}
		w.Header()[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
	}
}

func headerTokenPresent(values []string, target string) bool {
	for _, value := range values {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), target) {
				return true
			}
		}
	}
	return false
}

func responseHopHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func (g *Gateway) writeTerminal(w http.ResponseWriter, r *http.Request, final exchange.Snapshot) {
	switch final.State {
	case exchange.StateDropped:
		w.WriteHeader(http.StatusNoContent)
	case exchange.StateCancelled:
		if r != nil && r.Context().Err() != nil {
			return
		}
		w.WriteHeader(499) // nginx-compatible client-closed/operator-abort status
	case exchange.StateFailed:
		http.Error(w, "context-lens upstream failure", http.StatusBadGateway)
	case exchange.StateCompleted:
		// A completed exchange normally committed through writeDownstream or
		// direct streaming. This fallback protects custom registry adapters.
		http.Error(w, "gateway completed without a response", http.StatusBadGateway)
	default:
		http.Error(w, "gateway exchange ended unexpectedly", http.StatusBadGateway)
	}
}

func (g *Gateway) writeCaptureError(w http.ResponseWriter, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	if errors.Is(err, wire.ErrCaptureLimit) {
		http.Error(w, "request body exceeds configured capture limit", http.StatusRequestEntityTooLarge)
		return
	}
	http.Error(w, "request body capture failed", http.StatusBadRequest)
}

func (g *Gateway) writeStoreError(w http.ResponseWriter, err error) {
	if err == nil {
		http.Error(w, "artifact storage unavailable", http.StatusServiceUnavailable)
		return
	}
	// A disconnected client has no response channel left to write. In
	// particular, do not turn cancellation into a misleading storage failure.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	status, message := http.StatusServiceUnavailable, "artifact storage unavailable"
	switch {
	case errors.Is(err, persistence.ErrArtifactTooLarge), errors.Is(err, wire.ErrCaptureLimit):
		status, message = http.StatusRequestEntityTooLarge, "artifact exceeds configured size limit"
	case errors.Is(err, persistence.ErrStoreFull):
		status, message = http.StatusInsufficientStorage, "artifact storage quota exceeded"
	case errors.Is(err, persistence.ErrMemoryLimit):
		status, message = http.StatusInsufficientStorage, "artifact storage memory limit exceeded; configure spill storage"
	case errors.Is(err, persistence.ErrClosed):
		status, message = http.StatusServiceUnavailable, "artifact storage is closed"
	case errors.Is(err, persistence.ErrInvalidArtifact),
		errors.Is(err, persistence.ErrInvalidArtifactID),
		errors.Is(err, persistence.ErrInvalidRange),
		errors.Is(err, persistence.ErrInvalidConfig),
		errors.Is(err, wire.ErrInvalidCaptureLimit):
		status, message = http.StatusBadRequest, "invalid artifact storage request"
	}
	http.Error(w, message, status)
}

func (g *Gateway) putArtifact(artifact wire.BodyArtifact) error {
	if g == nil || g.store == nil || artifact.Ref().ArtifactID == "" {
		return errors.New("gateway: artifact store unavailable")
	}
	// CaptureWriter-backed lazy artifacts are already committed. Checking
	// metadata first avoids materializing their full body merely to re-save it.
	if _, err := g.store.Metadata(context.Background(), artifact.Ref().ArtifactID); err == nil {
		return nil
	}
	_, err := g.store.PutArtifact(artifact)
	if errors.Is(err, persistence.ErrArtifactExists) {
		return nil
	}
	return err
}

func copyArtifact(source wire.BodyArtifact, stage, direction string) wire.BodyArtifact {
	ref := source.Ref()
	opts := wire.ArtifactOptions{Stage: stage, Direction: direction, ContentType: ref.ContentType, ContentEncoding: ref.ContentEncoding}
	if !ref.Complete {
		return wire.NewIncompleteArtifact(source.Bytes(), opts)
	}
	return wire.NewArtifact(source.Bytes(), opts)
}

// noteContextTokens observes the upstream-reported input-token occupancy from
// a completed response body and publishes it on the exchange summary. It is
// an observation only: failures are dropped because a missing token count
// must never affect the response the client receives.
func (g *Gateway) noteContextTokens(exchangeID string, protocol string, body []byte) {
	if g == nil || g.registry == nil {
		return
	}
	tokens := session.ExtractContextTokens(inspection.Protocol(protocol), body)
	if tokens == nil {
		return
	}
	_ = g.registry.SetContextTokens(exchangeID, *tokens)
}

// noteResponseID registers a Responses response identifier with the session
// index so a follow-up request carrying it as previous_response_id continues
// the same conversation.
func (g *Gateway) noteResponseID(exchangeID string, protocol string, body []byte) {
	if g == nil || g.sessions == nil || protocol != string(inspection.ProtocolResponses) {
		return
	}
	if responseID := session.ExtractResponseID(body); responseID != "" {
		g.sessions.NoteResponseID(exchangeID, responseID)
		if g.catalog != nil {
			_ = g.catalog.SetResponseID(context.Background(), exchangeID, responseID)
		}
	}
}

func (g *Gateway) eventSink(event exchange.Event) {
	// A cleared exchange is no longer registered; drop any late callback before
	// it can reach durable storage or workspace subscribers.
	if g == nil || g.registry == nil {
		return
	}
	if _, ok := g.registry.Get(event.ExchangeID); !ok {
		return
	}
	g.ingressMu.RLock()
	defer g.ingressMu.RUnlock()
	// Durable metadata is written from snapshots only; body bytes never enter SQLite.
	gen := g.generation.Load()
	// Stream events are realtime-only SSE observations. Persisting every token
	// would amplify SQLite writes and grow the durable event log; the terminal
	// snapshot/artifact event already records the durable state.
	if event.Kind != exchange.EventStreamEvent && g.catalog != nil && gen == g.generation.Load() {
		if e, ok := g.registry.Get(event.ExchangeID); ok {
			s := e.Snapshot()
			b, _ := json.Marshal(s)
			sessionID := durableTrafficSession
			position := int64(0)
			var sess catalog.Session
			if s.Session != nil {
				sessionID = s.Session.SessionID
				position = int64(s.Session.Depth)
			}
			// Session rows carry the latest snapshot ownership metadata. For
			// non-chat traffic this is the documented synthetic owner above.
			policyBytes, _ := json.Marshal(s.Policy)
			sess = catalog.Session{ID: sessionID, CreatedAt: s.CreatedAt.Format(time.RFC3339Nano), UpdatedAt: s.UpdatedAt.Format(time.RFC3339Nano), State: string(s.State), Revision: int64(s.Revision), Policy: string(policyBytes), Position: position}
			var refs []catalog.ArtifactRef
			for _, ref := range event.ArtifactRefs {
				sr := ref.StorageRef
				if sr == "" {
					sr = ref.ArtifactID
				}
				refs = append(refs, catalog.ArtifactRef{ID: ref.ArtifactID, ExchangeID: s.ExchangeID, Stage: ref.Stage, Direction: ref.Direction, ContentType: ref.ContentType, ContentEncoding: ref.ContentEncoding, Size: ref.Size, SHA256: ref.SHA256, Complete: ref.Complete, StorageRef: sr})
			}
			meta, _ := json.Marshal(struct {
				ExchangeID string             `json:"exchange_id"`
				Revision   uint64             `json:"revision"`
				Kind       exchange.EventKind `json:"kind"`
			}{event.ExchangeID, event.Revision, event.Kind})
			summaryBytes, _ := json.Marshal(s.Summary)
			g.durableErrMu.Lock()
			if gen != g.generation.Load() {
				g.durableErrMu.Unlock()
				return
			}
			err := g.catalog.UpsertSnapshot(context.Background(), sess, catalog.Exchange{ID: s.ExchangeID, SessionID: sessionID, Position: int(position), Protocol: s.Protocol, Method: s.Request.Envelope.Method, Path: s.Request.Envelope.Path, Status: s.Response.Envelope.Status, CreatedAt: s.CreatedAt.Format(time.RFC3339Nano), UpdatedAt: s.UpdatedAt.Format(time.RFC3339Nano), Envelope: string(b), Summary: string(summaryBytes)}, refs, catalog.Event{SessionID: sessionID, Kind: string(event.Kind), Metadata: string(meta)})
			g.durableErrMu.Unlock()
			if err != nil {
				// Keep the diagnostic deliberately stable and secret-free. The
				// underlying error may include a local path or SQL detail.
				g.durableErrMu.Lock()
				g.durableErr = ErrDurablePersistence
				g.durableErrMu.Unlock()
			}
		}
	}
	if g.observer != nil {
		if _, ok := g.registry.Get(event.ExchangeID); !ok {
			return
		}
		g.observer(event)
	}
	g.subMu.RLock()
	callbacks := make([]func(exchange.Event), 0, len(g.subs))
	for _, callback := range g.subs {
		callbacks = append(callbacks, callback)
	}
	g.subMu.RUnlock()
	for _, callback := range callbacks {
		callback(event)
	}
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

// Keep sha256 referenced in this integration package's public diagnostics;
// SHA256Hex is the canonical helper used by tests and callers comparing legs.
func SHA256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return fmt.Sprintf("%x", sum[:])
}

var _ http.Handler = (*Gateway)(nil)
var _ = SHA256Hex
