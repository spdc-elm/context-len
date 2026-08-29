// Package gateway wires the opaque wire, transport, policy, and exchange
// layers into one local HTTP proxy handler.  It is intentionally a small
// vertical slice: request and response bodies are captured as immutable
// artifacts, while the exchange registry owns gate state and operator
// commands.  No JSON or SSE value is ever used as transport input.
package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"context-lens/backend/auth"
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

	// Events is an optional observer.  It receives the same immutable event
	// values as the exchange registry and is suitable for workspace adapters.
	Events exchange.EventSink
}

// Gateway is an HTTP handler and the owner of the concrete integration
// objects needed by workspace.NewWithRegistry.  The registry remains the
// source of truth for exchange state and commands.
type Gateway struct {
	upstream   *transport.Transport
	registry   *exchange.Registry
	store      *persistence.Store
	policy     *policy.Store
	maxBody    int64
	observer   exchange.EventSink
	clientAuth auth.Config

	subMu sync.RWMutex
	subs  map[uint64]func(exchange.Event)
	subID atomic.Uint64
}

// New constructs a gateway. It performs all network/upstream validation via
// transport.New and creates a bounded-independent persistence store when one
// was not supplied.
func New(cfg Config) (*Gateway, error) {
	if err := cfg.ClientAuth.Validate(); err != nil {
		return nil, err
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
	if store == nil {
		store = cfg.ArtifactStore
	}
	if store == nil {
		var err error
		store, err = persistence.NewArtifactStore(cfg.StoreConfig)
		if err != nil {
			return nil, err
		}
	}

	return &Gateway{
		upstream:   upstream,
		registry:   registry,
		store:      store,
		policy:     polStore,
		maxBody:    cfg.MaxBodyBytes,
		observer:   cfg.Events,
		clientAuth: cfg.ClientAuth,
		subs:       make(map[uint64]func(exchange.Event)),
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

// Registry returns the shared exchange registry.
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

	requestArtifact, err := g.captureRequest(r)
	if err != nil {
		g.writeCaptureError(w, err)
		return
	}
	if err := g.putArtifact(requestArtifact); err != nil {
		g.writeStoreError(w, err)
		return
	}

	requestEnvelope := wire.RequestFromHTTP(r)
	protocol := detectProtocol(requestEnvelope, requestArtifact.Bytes())
	// Summary is an additive observation of the original inbound bytes. It is
	// computed after capture and never becomes transport input; an unparseable
	// or non-chat body simply yields an empty summary that is dropped.
	var summary *session.Summary
	if requestSummary := session.SummarizeRequest(inspection.Protocol(protocol), requestArtifact.Bytes()); !requestSummary.Empty() {
		summary = &requestSummary
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

	e, err := g.registry.Create(exchange.CreateParams{
		Protocol:        protocol,
		RequestEnvelope: requestEnvelope,
		RequestArtifact: requestArtifact,
		PolicyOverride:  &pol,
		Upstream:        upstream,
		Downstream:      downstream,
		Context:         r.Context(),
		Events:          g.eventSink,
		Summary:         summary,
	})
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
	artifact, err := captureReadCloser(r.Context(), body, wire.CaptureOptions{
		ArtifactOptions: wire.ArtifactOptions{
			Stage:           wire.StageRequestInbound,
			Direction:       wire.DirectionInbound,
			ContentType:     r.Header.Get("Content-Type"),
			ContentEncoding: r.Header.Get("Content-Encoding"),
		},
		MaxBytes: g.maxBody,
	})
	if err != nil {
		return artifact, err
	}
	if !artifact.Ref().Complete {
		return artifact, errors.New("gateway: request capture incomplete")
	}
	return artifact, nil
}

// captureReadCloser closes body on cancellation so a custom/local response
// body that blocks in Read cannot keep an exchange alive after disconnect.
func captureReadCloser(ctx context.Context, body io.ReadCloser, opts wire.CaptureOptions) (wire.BodyArtifact, error) {
	if body == nil {
		body = http.NoBody
	}
	if ctx == nil {
		ctx = context.Background()
	}
	type result struct {
		artifact wire.BodyArtifact
		err      error
	}
	resultCh := make(chan result, 1)
	go func() {
		a, err := wire.CaptureReaderContext(ctx, body, opts)
		resultCh <- result{artifact: a, err: err}
	}()
	select {
	case got := <-resultCh:
		_ = body.Close()
		return got.artifact, got.err
	case <-ctx.Done():
		_ = body.Close()
		got := <-resultCh
		if got.err == nil {
			got.err = ctx.Err()
		}
		return got.artifact, got.err
	}
}

func (g *Gateway) roundTrip(ctx context.Context, inbound *http.Request, req exchange.UpstreamRequest, w http.ResponseWriter, committed *writeState, directWritten *atomic.Bool, direct bool, protocol string) (exchange.UpstreamResponse, error) {
	if err := contextError(ctx); err != nil {
		return exchange.UpstreamResponse{}, err
	}
	// The exchange interface carries the original inbound artifact but has no
	// explicit request.upstream artifact slot. Retain a distinct immutable copy
	// in the shared store so workspace users can compare both legs.
	upstreamArtifact := copyArtifact(req.Artifact, wire.StageRequestUpstream, wire.DirectionUpstream)
	if err := g.putArtifact(upstreamArtifact); err != nil {
		return exchange.UpstreamResponse{}, err
	}
	if err := g.registry.AddArtifactRef(req.ExchangeID, true, upstreamArtifact.Ref()); err != nil {
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
	prepared, err := g.upstream.PrepareRequest(ctx, outbound, nil)
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
	resp.Body = g.attachStreamTap(req.ExchangeID, resp.Body, resp.Header.Get("Content-Type"))
	defer resp.Body.Close()

	if direct {
		artifact, _, err := g.captureAndStream(ctx, resp, w, committed)
		if err != nil {
			return exchange.UpstreamResponse{}, err
		}
		directWritten.Store(true)
		downstreamArtifact := copyArtifact(artifact, wire.StageResponseDownstream, wire.DirectionDownstream)
		if err := g.putArtifact(downstreamArtifact); err != nil {
			return exchange.UpstreamResponse{}, err
		}
		if err := g.registry.AddArtifactRef(req.ExchangeID, false, downstreamArtifact.Ref()); err != nil {
			return exchange.UpstreamResponse{}, err
		}
		g.noteContextTokens(req.ExchangeID, protocol, artifact.Bytes())
		return exchange.UpstreamResponse{Envelope: envelopeWithTimes(resp, started), Artifact: artifact}, nil
	}

	artifact, err := captureReadCloser(ctx, resp.Body, wire.CaptureOptions{
		ArtifactOptions: wire.ArtifactOptions{
			Stage:           wire.StageResponseUpstream,
			Direction:       wire.DirectionUpstream,
			ContentType:     resp.Header.Get("Content-Type"),
			ContentEncoding: resp.Header.Get("Content-Encoding"),
		},
		MaxBytes: g.maxBody,
	})
	if err != nil {
		return exchange.UpstreamResponse{}, err
	}
	if !artifact.Ref().Complete {
		return exchange.UpstreamResponse{}, errors.New("gateway: response capture incomplete")
	}
	g.noteContextTokens(req.ExchangeID, protocol, artifact.Bytes())
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

// captureAndStream performs a byte-for-byte response copy while retaining a
// complete immutable artifact. It writes no synthetic SSE delimiters and does
// not inspect JSON.  The buffer is required by exchange.UpstreamRoundTripper's
// artifact-returning contract; headers/body still reach pass-through clients
// immediately.
func (g *Gateway) captureAndStream(ctx context.Context, resp *http.Response, w http.ResponseWriter, committed *writeState) (wire.BodyArtifact, wire.ResponseEnvelope, error) {
	started := time.Now().UTC()
	envelope := wire.ResponseFromHTTP(resp, started, time.Time{})
	if err := writeResponseHeaders(w, envelope, committed); err != nil {
		return wire.BodyArtifact{}, wire.ResponseEnvelope{}, err
	}
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	var captured bytes.Buffer
	captureIncomplete := false
	buf := make([]byte, 32*1024)
	for {
		if err := contextError(ctx); err != nil {
			return incompleteResponseArtifact(resp, captured.Bytes()), envelopeWithTimes(resp, started), err
		}
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if !captureIncomplete {
				if g.maxBody > 0 && int64(captured.Len()+n) > g.maxBody {
					remaining := int(g.maxBody - int64(captured.Len()))
					if remaining > 0 {
						_, _ = captured.Write(buf[:remaining])
					}
					captureIncomplete = true
				} else {
					_, _ = captured.Write(buf[:n])
				}
			}
			written, writeErr := w.Write(buf[:n])
			if writeErr != nil {
				return incompleteResponseArtifact(resp, captured.Bytes()), envelopeWithTimes(resp, started), writeErr
			}
			if written != n {
				return incompleteResponseArtifact(resp, captured.Bytes()), envelopeWithTimes(resp, started), io.ErrShortWrite
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				var artifact wire.BodyArtifact
				if captureIncomplete {
					artifact = incompleteResponseArtifact(resp, captured.Bytes())
				} else {
					artifact = wire.NewArtifact(captured.Bytes(), wire.ArtifactOptions{
						Stage: wire.StageResponseUpstream, Direction: wire.DirectionUpstream,
						ContentType: resp.Header.Get("Content-Type"), ContentEncoding: resp.Header.Get("Content-Encoding"),
					})
				}
				finalEnvelope := wire.ResponseFromHTTP(resp, started, time.Now().UTC())
				setResponseTrailers(w, finalEnvelope.Trailers)
				return artifact, finalEnvelope, nil
			}
			return incompleteResponseArtifact(resp, captured.Bytes()), envelopeWithTimes(resp, started), readErr
		}
	}
}

func incompleteResponseArtifact(resp *http.Response, body []byte) wire.BodyArtifact {
	return wire.NewIncompleteArtifact(body, wire.ArtifactOptions{
		Stage: wire.StageResponseUpstream, Direction: wire.DirectionUpstream,
		ContentType: resp.Header.Get("Content-Type"), ContentEncoding: resp.Header.Get("Content-Encoding"),
	})
}

func (g *Gateway) writeDownstream(ctx context.Context, w http.ResponseWriter, response exchange.DownstreamResponse, committed *writeState) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if !response.Artifact.Ref().Complete {
		return errors.New("gateway: refusing incomplete response artifact")
	}
	downstreamArtifact := copyArtifact(response.Artifact, wire.StageResponseDownstream, wire.DirectionDownstream)
	if err := g.putArtifact(downstreamArtifact); err != nil {
		return err
	}
	if err := g.registry.AddArtifactRef(response.ExchangeID, false, downstreamArtifact.Ref()); err != nil {
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
	if errors.Is(err, context.Canceled) {
		return
	}
	http.Error(w, "gateway artifact store unavailable", http.StatusInsufficientStorage)
}

func (g *Gateway) putArtifact(artifact wire.BodyArtifact) error {
	if g == nil || g.store == nil || artifact.Ref().ArtifactID == "" {
		return errors.New("gateway: artifact store unavailable")
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

func (g *Gateway) eventSink(event exchange.Event) {
	// Exchange saves each referenced artifact before emitting its event. Mirror
	// those refs into the persistence store for workspace artifact reads.
	for _, ref := range event.ArtifactRefs {
		if artifact, ok := g.registry.Artifact(ref.ArtifactID); ok {
			_ = g.putArtifact(artifact)
		}
	}
	if g.observer != nil {
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
