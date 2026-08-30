package gateway

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"context-lens/backend/persistence"
	"context-lens/backend/policy"
	"context-lens/backend/transport"
)

type captureErrorBody struct {
	data []byte
	err  error
	done bool
}

func (b *captureErrorBody) Read(p []byte) (int, error) {
	if b.done {
		return 0, io.EOF
	}
	b.done = true
	copy(p, b.data)
	return len(b.data), b.err
}
func (b *captureErrorBody) Close() error { return nil }

func captureCleanupGateway(t *testing.T, maxBody int64) (*Gateway, func()) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
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
		Upstream: tr, MaxBodyBytes: maxBody,
		InitialPolicy: policy.Policy{RequestGate: policy.Pass, ResponseGate: policy.Pass},
		StoreConfig:   persistence.Config{MaxArtifactBytes: 1 << 20, MaxTotalBytes: 1 << 20, MaxMemoryBytes: 1 << 20},
	})
	if err != nil {
		upstream.Close()
		t.Fatal(err)
	}
	return g, func() { _ = g.Store().Close(); upstream.Close() }
}

func TestServeHTTPCaptureLimitCleansCommittedArtifact(t *testing.T) {
	g, cleanup := captureCleanupGateway(t, 4)
	defer cleanup()
	r := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/responses", &captureErrorBody{data: []byte("12345"), err: nil})
	r.Header.Set("Authorization", "Bearer test")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d, want %d", w.Code, http.StatusRequestEntityTooLarge)
	}
	if got := g.Store().Stats().Artifacts; got != 0 {
		t.Fatalf("artifacts after capture limit = %d, want 0", got)
	}

	fresh := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/responses", io.NopCloser(strings.NewReader(`{}`)))
	fresh.Header.Set("Authorization", "Bearer test")
	fw := httptest.NewRecorder()
	g.ServeHTTP(fw, fresh)
	if fw.Code < 200 || fw.Code >= 300 {
		t.Fatalf("fresh request status=%d, body=%q", fw.Code, fw.Body.String())
	}
}

func TestServeHTTPCaptureReadErrorCleansCommittedArtifact(t *testing.T) {
	g, cleanup := captureCleanupGateway(t, 0)
	defer cleanup()
	r := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/responses", &captureErrorBody{data: []byte("partial"), err: errors.New("synthetic reader failure")})
	r.Header.Set("Authorization", "Bearer test")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want %d", w.Code, http.StatusBadRequest)
	}
	if got := g.Store().Stats().Artifacts; got != 0 {
		t.Fatalf("artifacts after read error = %d, want 0", got)
	}
}
