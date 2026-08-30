package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"context-lens/backend/catalog"
	"context-lens/backend/exchange"
	"context-lens/backend/persistence"
	"context-lens/backend/policy"
	"context-lens/backend/transport"
	"context-lens/backend/wire"
	"context-lens/backend/workspace"
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
		CaptureMode:   CaptureModeCapture,
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

type isolatedResponseWriter struct {
	mu     sync.Mutex
	status int
	body   bytes.Buffer
}

func (w *isolatedResponseWriter) Header() http.Header { return make(http.Header) }
func (w *isolatedResponseWriter) WriteHeader(status int) {
	w.mu.Lock()
	w.status = status
	w.mu.Unlock()
}
func (w *isolatedResponseWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.Write(p)
}

func TestClearRaceBlockedResponseInvalidatesOldCaptureAndAllowsNewRequest(t *testing.T) {
	responseStarted := make(chan struct{})
	release := make(chan struct{})
	requestBodies := make(chan []byte, 2)
	var calls atomic.Int32
	g, _ := localGateway(t, policy.Policy{RequestGate: policy.Pass, ResponseGate: policy.Pass}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream request: %v", err)
		}
		requestBodies <- append([]byte(nil), body...)
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			if _, err := io.WriteString(w, `{"id":"old","output":[`); err != nil {
				t.Errorf("write first response chunk: %v", err)
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			close(responseStarted)
			<-release
			_, _ = io.WriteString(w, `"stale"]}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"fresh","output":[]}`)
	}))

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		g.ServeHTTP(&isolatedResponseWriter{}, requestFor(`{"model":"m","input":"old"}`))
	}()
	select {
	case <-responseStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream response body did not become active")
	}

	if err := g.ClearQueue(context.Background()); err != nil {
		t.Fatalf("clear: %v", err)
	}
	// Let the canceled old round trip unwind. Its response capture must reject
	// the pre-clear store epoch and it must not recreate registry state.
	close(release)
	select {
	case <-firstDone:
	case <-time.After(3 * time.Second):
		t.Fatal("old request did not finish after clear")
	}
	if got := len(g.Registry().List()); got != 0 {
		t.Fatalf("old exchange survived clear: %d", got)
	}
	if stats := g.Store().Stats(); stats.Artifacts != 0 || stats.Bytes != 0 {
		t.Fatalf("stale artifacts survived clear: %+v", stats)
	}

	// Clear must leave the runtime usable for a fresh request, and the fresh
	// request must reach the same-protocol upstream byte-for-byte.
	freshBody := `{"model":"m","input":"new"}`
	fresh := httptest.NewRecorder()
	g.ServeHTTP(fresh, requestFor(freshBody))
	if fresh.Code != http.StatusOK {
		t.Fatalf("fresh request status=%d body=%s", fresh.Code, fresh.Body.String())
	}
	if got := <-requestBodies; string(got) != `{"model":"m","input":"old"}` {
		t.Fatalf("old upstream request bytes = %q", got)
	}
	if got := <-requestBodies; string(got) != freshBody {
		t.Fatalf("fresh upstream request bytes = %q, want %q", got, freshBody)
	}
	if stats := g.Store().Stats(); stats.Artifacts == 0 {
		t.Fatal("fresh request did not create artifacts")
	}
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
	g, err := New(Config{UpstreamURL: upstream.URL, CaptureMode: CaptureModeCapture, MaxBodyBytes: 4})
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

func TestLargeBodyUsesStoredRefsAndPreservesTransportBytes(t *testing.T) {
	requestBody := bytes.Repeat([]byte("request-wire-"), 200000)
	responseBody := bytes.Repeat([]byte("response-wire-"), 180000)
	var gotRequest []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRequest, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(responseBody)
	}))
	defer upstream.Close()
	g, err := New(Config{UpstreamURL: upstream.URL, CaptureMode: CaptureModeCapture, StoreConfig: persistence.Config{
		SpillRoot: t.TempDir(), MaxMemoryBytes: 1, MaxTotalBytes: int64(len(requestBody) + len(responseBody) + 1024),
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Store().Close()
	req := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/responses", bytes.NewReader(requestBody))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer synthetic")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if !bytes.Equal(gotRequest, requestBody) || !bytes.Equal(rec.Body.Bytes(), responseBody) {
		t.Fatal("large body bytes changed in transport")
	}
	s := g.Registry().List()[0]
	if len(s.Request.ArtifactRefs) != 2 || len(s.Response.ArtifactRefs) != 2 {
		t.Fatalf("unexpected artifact refs: request=%d response=%d", len(s.Request.ArtifactRefs), len(s.Response.ArtifactRefs))
	}
	if s.Request.ArtifactRefs[0].StorageRef != s.Request.ArtifactRefs[1].StorageRef || s.Response.ArtifactRefs[0].StorageRef != s.Response.ArtifactRefs[1].StorageRef {
		t.Fatal("logical refs did not share physical stored blobs")
	}
	stats := g.Store().Stats()
	if stats.DiskBytes < int64(len(requestBody)+len(responseBody)) {
		t.Fatalf("store physical bytes=%d, want at least %d", stats.DiskBytes, len(requestBody)+len(responseBody))
	}
}
func TestDurableLocalRestartWorkspaceAndBlobIntegrity(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	catalogPath := filepath.Join(root, "catalog.db")
	spillRoot := filepath.Join(root, "spill")
	requestBody := []byte(`{"model":"durable-model","input":"restart"}`)
	responseBody := []byte(`{"id":"resp-durable","object":"response","output":[{"type":"message","text":"exact"}]}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if !bytes.Equal(got, requestBody) {
			t.Errorf("upstream request bytes = %q, want %q", got, requestBody)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(responseBody)
	}))
	defer upstream.Close()
	cfg := Config{UpstreamURL: upstream.URL, CaptureMode: CaptureModeCapture, DurableCatalogPath: catalogPath,
		StoreConfig: persistence.Config{SpillRoot: spillRoot, MaxMemoryBytes: 1, MaxTotalBytes: 1 << 20}}
	g1, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/responses", bytes.NewReader(requestBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer synthetic")
	g1.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), responseBody) {
		t.Fatalf("proxy response code=%d body=%q", rec.Code, rec.Body.Bytes())
	}
	items := g1.Registry().List()
	if len(items) != 1 || items[0].State != exchange.StateCompleted {
		t.Fatalf("initial registry = %+v", items)
	}
	old := items[0]
	var refs []wire.ArtifactRef
	refs = append(refs, old.Request.ArtifactRefs...)
	refs = append(refs, old.Response.ArtifactRefs...)
	if len(refs) == 0 {
		t.Fatal("durable exchange has no artifact refs")
	}
	for _, ref := range refs {
		got, err := g1.Store().Get(ctx, ref.ArtifactID)
		if err != nil {
			t.Fatalf("initial get %s: %v", ref.ArtifactID, err)
		}
		if ref.Stage == wire.StageRequestInbound && !bytes.Equal(got.Bytes(), requestBody) {
			t.Fatal("request artifact differs")
		}
		if ref.Stage == wire.StageResponseUpstream && !bytes.Equal(got.Bytes(), responseBody) {
			t.Fatal("response artifact differs")
		}
	}
	if err := g1.Close(); err != nil {
		t.Fatal(err)
	}

	// Durable reopen must reject absent and modified content-addressed blobs.
	probe := refs[0]
	parts := strings.Split(probe.StorageRef, "-")
	blobPath := filepath.Join(spillRoot, "blobs", parts[0][:2], parts[0][2:4], probe.StorageRef+".blob")
	blob, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(blobPath); err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("missing blob reopen error=%v, want ErrNotFound", err)
	}
	if err := os.WriteFile(blobPath, blob, 0600); err != nil {
		t.Fatal(err)
	}
	corrupt := append([]byte(nil), blob...)
	corrupt[0] ^= 0xff
	if err := os.WriteFile(blobPath, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg); !errors.Is(err, persistence.ErrCorruptArtifact) {
		t.Fatalf("corrupt blob reopen error=%v, want ErrCorruptArtifact", err)
	}
	if err := os.WriteFile(blobPath, blob, 0600); err != nil {
		t.Fatal(err)
	}

	g2, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer g2.Close()
	items = g2.Registry().List()
	if len(items) != 1 || items[0].ExchangeID != old.ExchangeID || items[0].State != exchange.StateCompleted {
		t.Fatalf("reopened registry = %+v", items)
	}
	for _, ref := range refs {
		got, err := g2.Store().Get(ctx, ref.ArtifactID)
		if err != nil {
			t.Fatalf("reopened get %s: %v", ref.ArtifactID, err)
		}
		want := requestBody
		if strings.HasPrefix(ref.Stage, "response.") {
			want = responseBody
		}
		if !bytes.Equal(got.Bytes(), want) {
			t.Fatalf("reopened artifact %s bytes changed", ref.ArtifactID)
		}
		rng, err := g2.Store().ReadRange(ctx, ref.ArtifactID, 0, int64(minInt(len(want), 7)))
		if err != nil || !bytes.Equal(rng, want[:minInt(len(want), 7)]) {
			t.Fatalf("range %s = %q, err=%v", ref.ArtifactID, rng, err)
		}
	}
	ws := workspace.New(workspace.Config{Registry: g2.Registry(), Artifacts: g2.Store(), ClearQueue: g2.ClearQueue})
	listRec := httptest.NewRecorder()
	ws.ServeHTTP(listRec, httptest.NewRequest(http.MethodGet, "http://proxy.test/api/exchanges", nil))
	if listRec.Code != http.StatusOK {
		t.Fatalf("workspace list status=%d body=%s", listRec.Code, listRec.Body.String())
	}
	var listed []exchange.Snapshot
	if err := json.Unmarshal(listRec.Body.Bytes(), &listed); err != nil || len(listed) != 1 {
		t.Fatalf("workspace list=%s err=%v", listRec.Body, err)
	}
	artRec := httptest.NewRecorder()
	ws.ServeHTTP(artRec, httptest.NewRequest(http.MethodGet, "http://proxy.test/api/artifacts/"+refs[0].ArtifactID, nil))
	if artRec.Code != http.StatusOK {
		t.Fatalf("workspace artifact status=%d body=%s", artRec.Code, artRec.Body.String())
	}
	workspaceWant := requestBody
	if strings.HasPrefix(refs[0].Stage, "response.") {
		workspaceWant = responseBody
	}
	if !bytes.Equal(artRec.Body.Bytes(), workspaceWant) {
		t.Fatalf("workspace artifact body=%q, want=%q", artRec.Body.Bytes(), workspaceWant)
	}
	clearRec := httptest.NewRecorder()
	clearReq := httptest.NewRequest(http.MethodDelete, "http://proxy.test/api/exchanges", nil)
	clearReq.Header.Set("Content-Type", "application/json")
	ws.ServeHTTP(clearRec, clearReq)
	if clearRec.Code != http.StatusOK || len(g2.Registry().List()) != 0 {
		t.Fatalf("clear status=%d registry=%+v", clearRec.Code, g2.Registry().List())
	}
	if _, err := g2.Store().Get(ctx, refs[0].ArtifactID); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("cleared artifact error=%v", err)
	}
}

// rangeOnlyArtifactProbe makes accidental whole-artifact materialisation visible
// while retaining the production RangeArtifactStore seam used by workspace.
type rangeOnlyArtifactProbe struct {
	store    *persistence.Store
	gets     atomic.Int32
	refs     atomic.Int32
	ranges   atomic.Int32
	searches atomic.Int32
}

func (p *rangeOnlyArtifactProbe) Get(context.Context, string) (wire.BodyArtifact, error) {
	p.gets.Add(1)
	return wire.BodyArtifact{}, errors.New("whole artifact read was used")
}
func (p *rangeOnlyArtifactProbe) ArtifactRef(ctx context.Context, id string) (wire.ArtifactRef, error) {
	p.refs.Add(1)
	return p.store.ArtifactRef(ctx, id)
}
func (p *rangeOnlyArtifactProbe) ReadRange(ctx context.Context, id string, start, end int64) ([]byte, error) {
	p.ranges.Add(1)
	return p.store.ReadRange(ctx, id, start, end)
}
func (p *rangeOnlyArtifactProbe) Search(ctx context.Context, id string, query []byte, limit int) ([]wire.ArtifactMatch, error) {
	p.searches.Add(1)
	return p.store.Search(ctx, id, query, limit)
}

func TestDurableWorkspaceBackendPaginationArtifactsAndClear(t *testing.T) {
	root := t.TempDir()
	cfg := Config{UpstreamURL: "", CaptureMode: CaptureModeCapture, DurableCatalogPath: filepath.Join(root, "catalog.db"), StoreConfig: persistence.Config{
		SpillRoot: filepath.Join(root, "spill"), MaxMemoryBytes: 1, MaxTotalBytes: 1 << 20, PreserveFilesOnClose: true,
	}}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"echo":"` + strings.TrimSpace(string(body)) + `"}`))
	}))
	defer upstream.Close()
	cfg.UpstreamURL = upstream.URL
	g1, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"model":"one","needle":"alpha"}`, `{"model":"two","needle":"beta"}`} {
		req := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/responses", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer synthetic")
		rec := httptest.NewRecorder()
		g1.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("proxy status=%d body=%s", rec.Code, rec.Body)
		}
	}
	before := g1.Registry().List()
	if len(before) != 2 {
		t.Fatalf("registry count=%d", len(before))
	}
	artifactID := before[0].Request.ArtifactRefs[0].ArtifactID
	if err := g1.Close(); err != nil {
		t.Fatal(err)
	}

	g2, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer g2.Close()
	probe := &rangeOnlyArtifactProbe{store: g2.Store()}
	ws := workspace.New(workspace.Config{Backend: g2.WorkspaceBackend(), Artifacts: probe, ClearQueue: g2.ClearQueue})

	first := httptest.NewRecorder()
	ws.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "http://proxy.test/api/exchanges?limit=1", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("page one status=%d body=%s", first.Code, first.Body)
	}
	var page struct {
		Exchanges []exchange.Snapshot `json:"exchanges"`
		Next      string              `json:"next_cursor"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Exchanges) != 1 || page.Next == "" {
		t.Fatalf("page one envelope=%s", first.Body)
	}
	second := httptest.NewRecorder()
	ws.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "http://proxy.test/api/exchanges?limit=1&cursor="+url.QueryEscape(page.Next), nil))
	var page2 struct {
		Exchanges []exchange.Snapshot `json:"exchanges"`
		Next      string              `json:"next_cursor"`
	}
	if second.Code != http.StatusOK || json.Unmarshal(second.Body.Bytes(), &page2) != nil || len(page2.Exchanges) != 1 || page2.Exchanges[0].ExchangeID == page.Exchanges[0].ExchangeID {
		t.Fatalf("page two status=%d body=%s", second.Code, second.Body)
	}
	get := httptest.NewRecorder()
	ws.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "http://proxy.test/api/exchanges/"+page.Exchanges[0].ExchangeID, nil))
	if get.Code != http.StatusOK {
		t.Fatalf("catalog get status=%d body=%s", get.Code, get.Body)
	}
	var got exchange.Snapshot
	if err := json.Unmarshal(get.Body.Bytes(), &got); err != nil || got.ExchangeID != page.Exchanges[0].ExchangeID {
		t.Fatalf("catalog get=%s", get.Body)
	}

	head := httptest.NewRecorder()
	ws.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "http://proxy.test/api/artifacts/"+artifactID, nil))
	if head.Code != http.StatusOK || probe.gets.Load() != 0 {
		t.Fatalf("HEAD status=%d gets=%d", head.Code, probe.gets.Load())
	}
	full := httptest.NewRecorder()
	ws.ServeHTTP(full, httptest.NewRequest(http.MethodGet, "http://proxy.test/api/artifacts/"+artifactID, nil))
	if full.Code != http.StatusOK || probe.gets.Load() != 0 || full.Body.Len() == 0 {
		t.Fatalf("GET status=%d bytes=%d gets=%d", full.Code, full.Body.Len(), probe.gets.Load())
	}
	rng := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "http://proxy.test/api/artifacts/"+artifactID+"?range=bytes=2-6", nil)
	ws.ServeHTTP(rng, r)
	if rng.Code != http.StatusPartialContent || !bytes.Equal(rng.Body.Bytes(), []byte(`model`)) {
		t.Fatalf("range status=%d body=%q", rng.Code, rng.Body.Bytes())
	}
	search := httptest.NewRecorder()
	ws.ServeHTTP(search, httptest.NewRequest(http.MethodGet, "http://proxy.test/api/artifacts/"+artifactID+"?search=alpha", nil))
	if search.Code != http.StatusOK || probe.searches.Load() == 0 || probe.ranges.Load() < 2 || probe.gets.Load() != 0 {
		t.Fatalf("search status=%d searches=%d ranges=%d gets=%d", search.Code, probe.searches.Load(), probe.ranges.Load(), probe.gets.Load())
	}
	clear := httptest.NewRecorder()
	clearReq := httptest.NewRequest(http.MethodDelete, "http://proxy.test/api/exchanges", nil)
	clearReq.Header.Set("Content-Type", "application/json")
	ws.ServeHTTP(clear, clearReq)
	if clear.Code != http.StatusOK || len(g2.Registry().List()) != 0 {
		t.Fatalf("clear status=%d registry=%d", clear.Code, len(g2.Registry().List()))
	}
	if _, err := g2.Store().ArtifactRef(context.Background(), artifactID); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("artifact after clear=%v", err)
	}
	if _, err := g2.WorkspaceBackend().GetExchange(page.Exchanges[0].ExchangeID); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("exchange after clear=%v", err)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
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
	if stats := g.Store().Stats(); stats.Artifacts != 4 {
		t.Fatalf("store artifact count = %d, want 4 (one per referenced stage)", stats.Artifacts)
	}
	if got := len(items[0].Request.ArtifactRefs) + len(items[0].Response.ArtifactRefs); got != 4 {
		t.Fatalf("exchange artifact ref count = %d, want 4", got)
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
