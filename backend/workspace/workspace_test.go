package workspace

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"context-lens/backend/exchange"
	"context-lens/backend/policy"
	"context-lens/backend/wire"
)

type testBackend struct {
	mu      sync.Mutex
	items   map[string]exchange.Snapshot
	command func(exchange.Command, *exchange.Snapshot) (exchange.CommandResult, error)
}

func (b *testBackend) ListExchanges() ([]exchange.Snapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	items := make([]exchange.Snapshot, 0, len(b.items))
	for _, item := range b.items {
		items = append(items, item)
	}
	return items, nil
}
func (b *testBackend) GetExchange(id string) (exchange.Snapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	item, ok := b.items[id]
	if !ok {
		return exchange.Snapshot{}, exchange.ErrNotFound
	}
	return item, nil
}
func (b *testBackend) Command(command exchange.Command) (exchange.CommandResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	item, ok := b.items[command.ExchangeID]
	if !ok {
		return exchange.CommandResult{}, exchange.ErrNotFound
	}
	if command.BaseRevision != item.Revision {
		return exchange.CommandResult{}, &exchange.RevisionConflictError{Expected: item.Revision, Received: command.BaseRevision}
	}
	if b.command != nil {
		result, err := b.command(command, &item)
		if err != nil {
			return exchange.CommandResult{}, err
		}
		b.items[command.ExchangeID] = result.Exchange
		return result, nil
	}
	item.Revision++
	item.UpdatedAt = time.Now().UTC()
	item.State = exchange.StateCompleted
	b.items[command.ExchangeID] = item
	return exchange.CommandResult{Exchange: item, Revision: item.Revision}, nil
}

func testSnapshot(id string, body wire.BodyArtifact) exchange.Snapshot {
	now := time.Now().UTC()
	return exchange.Snapshot{
		ExchangeID: id,
		Protocol:   "responses",
		Request: exchange.RequestPart{
			Envelope:     wire.NewRequestEnvelope("POST", "/v1/responses", "/v1/responses", "x=1", http.Header{"Authorization": {"Bearer super-secret"}, "X-Trace": {"keep"}}),
			ArtifactRefs: []wire.ArtifactRef{body.Ref()},
		},
		Policy:    policy.Default(),
		State:     exchange.StateRequestHeld,
		Warnings:  []string{"warning"},
		CreatedAt: now,
		UpdatedAt: now,
		Revision:  1,
	}
}

func decodeJSONResponse(t *testing.T, recorder *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(recorder.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode response: %v; body=%q", err, recorder.Body.String())
	}
}

func TestArtifactHeadDoesNotReadRange(t *testing.T) {
	a := wire.NewArtifact([]byte("metadata-only"), wire.ArtifactOptions{ArtifactID: "head-only"})
	store := &headOnlyRangeStore{ref: a.Ref()}
	r := httptest.NewRecorder()
	New(Config{Artifacts: store}).ServeHTTP(r, httptest.NewRequest(http.MethodHead, "/api/artifacts/head-only?start=0&end=4", nil))
	if r.Code != http.StatusPartialContent || r.Header().Get("Content-Length") != "4" || store.reads != 0 || store.gets != 0 {
		t.Fatalf("HEAD = %d headers=%#v reads=%d gets=%d", r.Code, r.Header(), store.reads, store.gets)
	}
}

type headOnlyRangeStore struct {
	ref         wire.ArtifactRef
	reads, gets int
}

func (s *headOnlyRangeStore) ArtifactRef(context.Context, string) (wire.ArtifactRef, error) {
	return s.ref, nil
}
func (s *headOnlyRangeStore) ReadRange(context.Context, string, int64, int64) ([]byte, error) {
	s.reads++
	panic("ReadRange called by HEAD")
}
func (s *headOnlyRangeStore) Search(context.Context, string, []byte, int) ([]ArtifactMatch, error) {
	return nil, nil
}
func (s *headOnlyRangeStore) Get(context.Context, string) (wire.BodyArtifact, error) {
	s.gets++
	panic("Get called by HEAD")
}

func TestArtifactErrorClassifierDoesNotLeakMessage(t *testing.T) {
	store := &classifiedArtifactStore{}
	r := httptest.NewRecorder()
	New(Config{Artifacts: store}).ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/api/artifacts/nope", nil))
	if r.Code != http.StatusBadRequest || strings.Contains(r.Body.String(), "secret-path") || strings.Contains(r.Body.String(), "token") {
		t.Fatalf("classifier leak/status: %d %q", r.Code, r.Body.String())
	}
}

type classifiedArtifactStore struct{}

func (*classifiedArtifactStore) Get(context.Context, string) (wire.BodyArtifact, error) {
	return wire.BodyArtifact{}, classifiedArtifactError{}
}

type classifiedArtifactError struct{}

func (classifiedArtifactError) Error() string { return "secret-path token" }
func (classifiedArtifactError) ArtifactHTTPError() (int, string, string) {
	return http.StatusInternalServerError, "backend_secret", "secret-path token"
}

func TestWorkspaceLargeRangeSeparatesCaptureAndLoadedCompleteness(t *testing.T) {
	body := make([]byte, 2<<20)
	for i := range body {
		body[i] = byte(i)
	}
	a := wire.NewArtifact(body, wire.ArtifactOptions{ArtifactID: "large", Stage: wire.StageResponseUpstream})
	store := NewMemoryArtifactStore(0)
	if err := store.Put(a); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRecorder()
	New(Config{Artifacts: store, MaxArtifactBytes: 2 << 20}).ServeHTTP(r, httptest.NewRequest(http.MethodHead, "/api/artifacts/large?start=0&end=1048576", nil))
	if r.Code != http.StatusPartialContent || r.Header().Get("X-Artifact-Complete") != "true" || r.Header().Get("X-Artifact-Loaded-Range") != "bytes 0-1048576" {
		t.Fatalf("headers = %d %#v", r.Code, r.Header())
	}
}

func TestWorkspacePaginationCursor(t *testing.T) {
	backend := &pagedTestBackend{items: []exchange.Snapshot{{ExchangeID: "a"}, {ExchangeID: "b"}, {ExchangeID: "c"}}}
	r := httptest.NewRecorder()
	New(Config{Backend: backend, MaxExchanges: 2}).ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/api/exchanges?limit=2", nil))
	if r.Code != http.StatusOK || r.Header().Get("X-Next-Cursor") != "2" {
		t.Fatalf("first page = %d %q", r.Code, r.Header().Get("X-Next-Cursor"))
	}
	var got struct {
		Exchanges  []exchange.Snapshot `json:"exchanges"`
		NextCursor string              `json:"next_cursor"`
		HasMore    bool                `json:"has_more"`
	}
	decodeJSONResponse(t, r, &got)
	if len(got.Exchanges) != 2 || got.NextCursor != "2" || !got.HasMore {
		t.Fatalf("page = %#v", got)
	}
}

type pagedTestBackend struct{ items []exchange.Snapshot }

func (b *pagedTestBackend) ListExchanges() ([]exchange.Snapshot, error) { return b.items, nil }
func (b *pagedTestBackend) GetExchange(string) (exchange.Snapshot, error) {
	return exchange.Snapshot{}, exchange.ErrNotFound
}
func (b *pagedTestBackend) Command(exchange.Command) (exchange.CommandResult, error) {
	return exchange.CommandResult{}, exchange.ErrNotFound
}
func (b *pagedTestBackend) ListExchangesPage(_ context.Context, limit int, cursor string) ([]exchange.Snapshot, string, error) {
	off := 0
	if cursor != "" {
		off, _ = strconv.Atoi(cursor)
	}
	if off > len(b.items) {
		off = len(b.items)
	}
	end := off + limit
	if end > len(b.items) {
		end = len(b.items)
	}
	next := ""
	if end < len(b.items) {
		next = strconv.Itoa(end)
	}
	return b.items[off:end], next, nil
}

func TestWorkspaceListGetRedactsHeaders(t *testing.T) {
	artifact := wire.NewArtifact([]byte(`{"model":"gpt-test"}`), wire.ArtifactOptions{ArtifactID: "req-1", Stage: wire.StageRequestInbound, Direction: wire.DirectionInbound, ContentType: "application/json"})
	backend := &testBackend{items: map[string]exchange.Snapshot{"ex-1": testSnapshot("ex-1", artifact)}}
	store := NewMemoryArtifactStore(0)
	if err := store.Put(artifact); err != nil {
		t.Fatal(err)
	}
	server := New(Config{Backend: backend, Artifacts: store})

	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/exchanges", nil),
		httptest.NewRequest(http.MethodGet, "/api/exchanges/ex-1", nil),
	} {
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d; body=%s", recorder.Code, recorder.Body.String())
		}
		if strings.Contains(recorder.Body.String(), "super-secret") {
			t.Fatalf("response leaked credential: %s", recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), wire.RedactedHeaderValue) {
			t.Fatalf("response did not expose redaction marker: %s", recorder.Body.String())
		}
	}
}

func TestArtifactRangeSearchAndDownload(t *testing.T) {
	artifact := wire.NewArtifact([]byte("0123456789"), wire.ArtifactOptions{ArtifactID: "artifact-1", Stage: wire.StageRequestInbound, Direction: wire.DirectionInbound, ContentType: "application/octet-stream", ContentEncoding: "gzip"})
	store := NewMemoryArtifactStore(0)
	if err := store.Put(artifact); err != nil {
		t.Fatal(err)
	}
	server := New(Config{Artifacts: store, MaxArtifactBytes: 5})

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/artifacts/artifact-1?start=2&end=5", nil))
	if recorder.Code != http.StatusPartialContent {
		t.Fatalf("range status = %d; body=%q", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Body.String(); got != "234" {
		t.Fatalf("range body = %q", got)
	}
	if got := recorder.Header().Get("Content-Range"); got != "bytes 2-4/10" {
		t.Fatalf("content range = %q", got)
	}
	if got := recorder.Header().Get("X-Artifact-Content-Encoding"); got != "gzip" {
		t.Fatalf("content encoding metadata = %q", got)
	}
	if recorder.Header().Get("Content-Encoding") != "" {
		t.Fatal("raw artifact endpoint must not cause transparent content decoding")
	}

	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/artifacts/artifact-1?search=34", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("search status = %d; body=%s", recorder.Code, recorder.Body.String())
	}
	var result ArtifactSearchResult
	decodeJSONResponse(t, recorder, &result)
	if len(result.Matches) != 1 || result.Matches[0].Start != 3 || result.Matches[0].End != 5 {
		t.Fatalf("search matches = %#v", result.Matches)
	}

	recorder = httptest.NewRecorder()
	serverSmall := New(Config{Artifacts: store, MaxArtifactBytes: 20})
	serverSmall.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/artifacts/artifact-1?start=0&end=1048576", nil))
	if recorder.Code != http.StatusPartialContent || recorder.Body.String() != "0123456789" {
		t.Fatalf("clamped preview = status %d body=%q", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/artifacts/artifact-1?start=0&end=5&download=true", nil))
	if recorder.Code != http.StatusPartialContent || !strings.HasPrefix(recorder.Header().Get("Content-Disposition"), "attachment;") {
		t.Fatalf("download response = status %d disposition %q", recorder.Code, recorder.Header().Get("Content-Disposition"))
	}

	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/artifacts/artifact-1", nil))
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("full body status = %d; body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestWorkspaceCommandPublishesSSEAndRevision(t *testing.T) {
	artifact := wire.NewArtifact([]byte("request"), wire.ArtifactOptions{ArtifactID: "req-2", Stage: wire.StageRequestInbound, Direction: wire.DirectionInbound, ContentType: "text/plain"})
	item := testSnapshot("ex-2", artifact)
	backend := &testBackend{items: map[string]exchange.Snapshot{"ex-2": item}}
	server := New(Config{Backend: backend, Artifacts: NewMemoryArtifactStore(0), Heartbeat: time.Hour})

	events, cancel := server.Broker().Subscribe("")
	defer cancel()
	request := jsonRequest(http.MethodPost, "/api/exchanges/ex-2/command", `{"exchange_id":"ex-2","base_revision":1,"kind":"forward_unchanged"}`)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("command status = %d; body=%s", recorder.Code, recorder.Body.String())
	}
	var result exchange.CommandResult
	decodeJSONResponse(t, recorder, &result)
	if result.Revision != 2 || result.Event == nil {
		t.Fatalf("command result = %#v", result)
	}
	select {
	case event := <-events:
		if event.Revision != 2 || event.ExchangeID != "ex-2" {
			t.Fatalf("event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("command did not publish event")
	}

	stale := httptest.NewRecorder()
	server.ServeHTTP(stale, jsonRequest(http.MethodPost, "/api/exchanges/ex-2/command", `{"exchange_id":"ex-2","base_revision":1,"kind":"abort"}`))
	if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), "revision_conflict") {
		t.Fatalf("stale response = %d %s", stale.Code, stale.Body.String())
	}

	streamServer := httptest.NewServer(server)
	defer streamServer.Close()
	ctx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, streamServer.URL+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("sse response = %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	cancelRequest()
}

func TestWorkspacePolicyOnlyAffectsFutureStore(t *testing.T) {
	store := NewMemoryArtifactStore(0)
	server := New(Config{Policy: policy.NewStore(policy.Default()), Artifacts: store})
	get := httptest.NewRecorder()
	server.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/api/policy", nil))
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), `"request_gate":"pass"`) {
		t.Fatalf("initial policy = %d %s", get.Code, get.Body.String())
	}
	set := httptest.NewRecorder()
	server.ServeHTTP(set, jsonRequest(http.MethodPut, "/api/policy", `{"request_gate":"hold","response_gate":"pass"}`))
	if set.Code != http.StatusOK || !strings.Contains(set.Body.String(), `"request_gate":"hold"`) {
		t.Fatalf("set policy = %d %s", set.Code, set.Body.String())
	}
	bad := httptest.NewRecorder()
	server.ServeHTTP(bad, jsonRequest(http.MethodPut, "/api/policy", `{"request_gate":"leak","response_gate":"pass"}`))
	if bad.Code != http.StatusBadRequest || strings.Contains(bad.Body.String(), "leak") {
		t.Fatalf("invalid policy = %d %s", bad.Code, bad.Body.String())
	}
}

func TestEventBrokerHistoryAndSlowSubscriber(t *testing.T) {
	broker := NewEventBroker(2)
	for i := 1; i <= 3; i++ {
		broker.Publish(exchange.Event{EventID: "event-" + string(rune('0'+i)), ExchangeID: "ex", Revision: uint64(i), Kind: exchange.EventUpdated})
	}
	if broker.Len() != 2 {
		t.Fatalf("history len = %d", broker.Len())
	}
	stream, cancel := broker.Subscribe("event-2")
	defer cancel()
	select {
	case event := <-stream:
		if event.EventID != "event-3" {
			t.Fatalf("replayed event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("history event not replayed")
	}
	for i := 0; i < 3; i++ {
		broker.Publish(exchange.Event{ExchangeID: "ex", Revision: uint64(10 + i), Kind: exchange.EventUpdated})
	}
}
