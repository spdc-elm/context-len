package gateway

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"context-lens/backend/auth"
	"context-lens/backend/persistence"
	"context-lens/backend/transport"
)

func TestClientAuthProtectsProxyAndDoesNotForwardAccessKey(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-secret" {
			t.Errorf("upstream auth = %q, want configured server credential", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"auth-test","object":"response","status":"completed","output":[]}`))
	}))
	t.Cleanup(upstream.Close)
	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := transport.New(transport.Config{
		BaseURL: u,
		HeaderPolicy: transport.HeaderPolicy{Additional: http.Header{
			"Authorization": {"Bearer upstream-secret"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	g, err := New(Config{
		Upstream:   tr,
		ClientAuth: auth.Config{Enabled: true, APIKey: "client-secret"},
		StoreConfig: persistence.Config{
			MaxArtifactBytes: 1 << 20,
			MaxTotalBytes:    4 << 20,
			MaxMemoryBytes:   4 << 20,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Store().Close() })

	body := []byte(`{"model":"m","input":"hi"}`)
	unauthorized := httptest.NewRecorder()
	g.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/responses", bytes.NewReader(body)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d, want 401", unauthorized.Code)
	}
	if upstreamCalls.Load() != 0 {
		t.Fatal("unauthorized request reached upstream")
	}

	authorized := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-secret")
	g.ServeHTTP(authorized, req)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized status=%d, want 200", authorized.Code)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("upstream calls=%d, want 1", upstreamCalls.Load())
	}
}

func TestTransparentModeForwardsStandardClientAuthorization(t *testing.T) {
	var received atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer harness-provider-key" {
			received.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"passthrough-test","object":"response","status":"completed","output":[]}`))
	}))
	t.Cleanup(upstream.Close)
	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := transport.New(transport.Config{
		BaseURL:      u,
		HeaderPolicy: transport.HeaderPolicy{ForwardInboundCredentials: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	g, err := New(Config{
		Upstream: tr,
		StoreConfig: persistence.Config{
			MaxArtifactBytes: 1 << 20,
			MaxTotalBytes:    4 << 20,
			MaxMemoryBytes:   4 << 20,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Store().Close() })

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/responses", bytes.NewReader([]byte(`{"model":"m","input":"hi"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer harness-provider-key")
	g.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK || !received.Load() {
		t.Fatalf("transparent request status=%d upstream_received=%v", recorder.Code, received.Load())
	}
}
