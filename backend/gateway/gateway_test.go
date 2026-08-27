package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"context-lens/backend/exchange"
	"context-lens/backend/persistence"
	"context-lens/backend/policy"
	"context-lens/backend/transport"
	"context-lens/backend/wire"
)

func localGateway(t *testing.T, p policy.Policy, upstreamHandler http.Handler) (*Gateway, *httptest.Server) {
	t.Helper()
	upstream := httptest.NewServer(upstreamHandler)
	u, err := url.Parse(upstream.URL)
	if err != nil {
		upstream.Close()
		t.Fatal(err)
	}
	tr, err := transport.New(transport.Config{BaseURL: u})
	if err != nil {
		upstream.Close()
		t.Fatal(err)
	}
	g, err := New(Config{
		Upstream:      tr,
		InitialPolicy: p,
		StoreConfig: persistence.Config{
			MaxArtifactBytes: 1 << 20,
			MaxTotalBytes:    16 << 20,
			MaxMemoryBytes:   16 << 20,
		},
	})
	if err != nil {
		upstream.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = g.Store().Close()
		upstream.Close()
	})
	return g, upstream
}

func requestFor(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/responses?x=a%2Fb&x=two", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer inbound-only")
	return req
}

func waitSnapshot(t *testing.T, r *exchange.Registry, id string, want exchange.State) (*exchange.Exchange, exchange.Snapshot) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		e, ok := r.Get(id)
		if ok {
			s := e.Snapshot()
			if s.State == want {
				return e, s
			}
		}
		time.Sleep(time.Millisecond)
	}
	e, ok := r.Get(id)
	if !ok {
		t.Fatalf("exchange %q was not registered", id)
	}
	t.Fatalf("exchange %q state = %s, want %s", id, e.Snapshot().State, want)
	return nil, exchange.Snapshot{}
}

func waitAnyExchange(t *testing.T, r *exchange.Registry, want exchange.State) (*exchange.Exchange, exchange.Snapshot) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, snapshot := range r.List() {
			if snapshot.State == want {
				e, _ := r.Get(snapshot.ExchangeID)
				return e, snapshot
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no exchange reached %s; list=%+v", want, r.List())
	return nil, exchange.Snapshot{}
}

func TestRejectsNonLoopbackUpstreamByDefault(t *testing.T) {
	if _, err := New(Config{UpstreamURL: "http://169.254.169.254/latest"}); err == nil {
		t.Fatal("metadata upstream accepted")
	}
	if _, err := New(Config{UpstreamURL: "https://example.com"}); err == nil {
		t.Fatal("public upstream accepted without explicit opt-in")
	}
}

func TestPassPassCaptureLimitDoesNotTruncateTraffic(t *testing.T) {
	body := []byte("0123456789")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	defer upstream.Close()
	g, err := New(Config{UpstreamURL: upstream.URL, MaxBodyBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Store().Close()
	recorder := httptest.NewRecorder()
	g.ServeHTTP(recorder, requestFor(`{}`))
	if !bytes.Equal(recorder.Body.Bytes(), body) {
		t.Fatalf("traffic truncated: %q", recorder.Body.Bytes())
	}
	s := g.Registry().List()[0]
	if len(s.Response.ArtifactRefs) != 2 || s.Response.ArtifactRefs[0].Complete || s.Response.ArtifactRefs[1].Complete {
		t.Fatalf("capture not marked incomplete: %+v", s.Response.ArtifactRefs)
	}
}

func TestPassPassOpaqueBytesAndArtifacts(t *testing.T) {
	requestBody := []byte(`{"model":"wire-model","unknown":{"raw":1},"input":["x"]}`)
	responseBody := []byte("data: response.output_text.delta\\n\\ndata: {\"id\":1}\\n\\n")
	var upstreamBody []byte
	g, _ := localGateway(t, policy.Default(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Mock", "yes")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write(responseBody)
	}))

	recorder := httptest.NewRecorder()
	g.ServeHTTP(recorder, requestFor(string(requestBody)))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", recorder.Code)
	}
	if !bytes.Equal(recorder.Body.Bytes(), responseBody) {
		t.Fatalf("response bytes changed: %q", recorder.Body.Bytes())
	}
	if !bytes.Equal(upstreamBody, requestBody) {
		t.Fatalf("request bytes changed: %q", upstreamBody)
	}
	if recorder.Header().Get("X-Mock") != "yes" {
		t.Fatal("upstream response header missing")
	}
	items := g.Registry().List()
	if len(items) != 1 || items[0].State != exchange.StateCompleted {
		t.Fatalf("registry list = %+v", items)
	}
	if len(items[0].Warnings) == 0 || !strings.Contains(strings.Join(items[0].Warnings, "\n"), "Authorization") {
		t.Fatalf("header policy diff not visible: %v", items[0].Warnings)
	}
	refs := items[0].Request.ArtifactRefs
	if len(refs) != 2 || refs[0].SHA256 != wire.SHA256Hex(requestBody) || refs[1].SHA256 != wire.SHA256Hex(requestBody) {
		t.Fatalf("request refs = %+v", refs)
	}
	responseRefs := items[0].Response.ArtifactRefs
	if len(responseRefs) != 2 || responseRefs[0].SHA256 != wire.SHA256Hex(responseBody) || responseRefs[1].SHA256 != wire.SHA256Hex(responseBody) {
		t.Fatalf("response refs = %+v", responseRefs)
	}
	stored, err := g.Store().Get(context.Background(), responseRefs[0].ArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored.Bytes(), responseBody) {
		t.Fatal("stored response bytes differ")
	}
}

func TestHoldPassForwardUnchanged(t *testing.T) {
	var calls atomic.Int32
	seen := make(chan []byte, 1)
	g, _ := localGateway(t, policy.Policy{RequestGate: policy.GateHold, ResponseGate: policy.GatePass}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		seen <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		g.ServeHTTP(recorder, requestFor(`{"model":"hold-model","n":1}`))
		close(done)
	}()
	_, held := waitAnyExchange(t, g.Registry(), exchange.StateRequestHeld)
	if recorder.Body.Len() != 0 {
		t.Fatal("request hold committed downstream bytes before command")
	}
	e, _ := g.Registry().Get(held.ExchangeID)
	result, err := e.Command(exchange.Command{ExchangeID: held.ExchangeID, BaseRevision: held.Revision, Kind: exchange.CommandForwardUnchanged})
	if err != nil {
		t.Fatal(err)
	}
	if result.Exchange.State != exchange.StateUpstreamRunning {
		t.Fatalf("forward state = %s", result.Exchange.State)
	}
	select {
	case got := <-seen:
		if string(got) != `{"model":"hold-model","n":1}` {
			t.Fatalf("upstream body = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream was not called")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("held request did not return")
	}
	if calls.Load() != 1 || recorder.Code != http.StatusOK || recorder.Body.String() != `{"ok":true}` {
		t.Fatalf("calls=%d status=%d body=%q", calls.Load(), recorder.Code, recorder.Body.String())
	}
}

func TestHoldPassManualResponseDoesNotCallUpstream(t *testing.T) {
	var calls atomic.Int32
	g, _ := localGateway(t, policy.Policy{RequestGate: policy.GateHold, ResponseGate: policy.GatePass}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte("upstream"))
	}))
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		g.ServeHTTP(recorder, requestFor(`{"model":"manual"}`))
		close(done)
	}()
	_, held := waitAnyExchange(t, g.Registry(), exchange.StateRequestHeld)
	e, _ := g.Registry().Get(held.ExchangeID)
	result, err := e.Command(exchange.Command{
		ExchangeID: held.ExchangeID, BaseRevision: held.Revision, Kind: exchange.CommandManualResponse,
		RawResponse: `{"id":"operator","object":"response"}`, ContentType: "application/json", ResponseStatus: http.StatusCreated,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Exchange.State != exchange.StateCompleted {
		t.Fatalf("manual state = %s", result.Exchange.State)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("manual request did not return")
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d", calls.Load())
	}
	if recorder.Code != http.StatusCreated || recorder.Body.String() != `{"id":"operator","object":"response"}` {
		t.Fatalf("manual response status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestPassHoldReleaseUnchanged(t *testing.T) {
	responseBody := []byte(`{"answer":"held","unknown":[1,2]}`)
	g, _ := localGateway(t, policy.Policy{RequestGate: policy.GatePass, ResponseGate: policy.GateHold}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Held", "yes")
		w.WriteHeader(http.StatusNonAuthoritativeInfo)
		_, _ = w.Write(responseBody)
	}))
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		g.ServeHTTP(recorder, requestFor(`{"model":"response-hold"}`))
		close(done)
	}()
	_, held := waitAnyExchange(t, g.Registry(), exchange.StateResponseHeld)
	if recorder.Body.Len() != 0 {
		t.Fatal("response hold wrote body before release")
	}
	e, _ := g.Registry().Get(held.ExchangeID)
	if _, err := e.Command(exchange.Command{ExchangeID: held.ExchangeID, BaseRevision: held.Revision, Kind: exchange.CommandReleaseUnchanged}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("response hold request did not return")
	}
	if recorder.Code != http.StatusNonAuthoritativeInfo || !bytes.Equal(recorder.Body.Bytes(), responseBody) {
		t.Fatalf("released status=%d body=%q", recorder.Code, recorder.Body.Bytes())
	}
	if recorder.Header().Get("X-Held") != "yes" {
		t.Fatal("released response header missing")
	}
}

func TestPassHoldReleaseEditedUpdatesLength(t *testing.T) {
	g, _ := localGateway(t, policy.Policy{RequestGate: policy.GatePass, ResponseGate: policy.GateHold}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		original := []byte(`{"answer":"original-long-value"}`)
		w.Header().Set("Content-Length", strconv.Itoa(len(original)))
		_, _ = w.Write(original)
	}))
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { g.ServeHTTP(recorder, requestFor(`{"model":"edit"}`)); close(done) }()
	_, held := waitAnyExchange(t, g.Registry(), exchange.StateResponseHeld)
	ref := held.Response.ArtifactRefs[0]
	e, _ := g.Registry().Get(held.ExchangeID)
	invalid := `{"not":"a responses response"}`
	if _, err := e.Command(exchange.Command{ExchangeID: held.ExchangeID, BaseRevision: held.Revision, Kind: exchange.CommandReleaseEdited, Mutation: &exchange.MutationInput{RawReplacement: invalid, BaseArtifactID: ref.ArtifactID, BaseSHA256: ref.SHA256}}); err == nil {
		t.Fatal("invalid protocol edit accepted")
	}
	if after := e.Snapshot(); after.State != exchange.StateResponseHeld || after.Revision != held.Revision {
		t.Fatalf("invalid edit changed state: %+v", after)
	}
	body := `{"id":"resp_edit","object":"response","status":"completed","output":[]}`
	_, err := e.Command(exchange.Command{ExchangeID: held.ExchangeID, BaseRevision: held.Revision, Kind: exchange.CommandReleaseEdited, Mutation: &exchange.MutationInput{RawReplacement: body, BaseArtifactID: ref.ArtifactID, BaseSHA256: ref.SHA256}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("edited release did not return")
	}
	if recorder.Body.String() != body || recorder.Header().Get("Content-Length") != strconv.Itoa(len(body)) {
		t.Fatalf("body=%q length=%q", recorder.Body.String(), recorder.Header().Get("Content-Length"))
	}
}

func TestCancellationCancelsUpstream(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	var once sync.Once
	g, _ := localGateway(t, policy.Default(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(started)
		<-r.Context().Done()
		once.Do(func() { close(cancelled) })
	}))
	ctx, cancel := context.WithCancel(context.Background())
	req := requestFor(`{"model":"cancel","stream":true}`).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		g.ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("upstream did not start")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream context was not canceled")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("gateway did not return after cancellation")
	}
}
