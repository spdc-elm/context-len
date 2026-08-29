package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestConfigValidate(t *testing.T) {
	if err := (Config{Enabled: true, APIKey: "synthetic-key"}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []Config{
		{Enabled: true},
		{Enabled: true, APIKey: " synthetic-key"},
		{Enabled: true, APIKey: "synthetic\nkey"},
	} {
		if err := cfg.Validate(); err == nil {
			t.Fatalf("invalid config accepted")
		}
	}
	if err := (Config{}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestAuthorizeAcceptsSupportedHeaders(t *testing.T) {
	cfg := Config{Enabled: true, APIKey: "synthetic-client-key"}
	for _, tc := range []struct {
		name   string
		header string
		value  string
	}{
		{"x api key", "X-API-Key", "synthetic-client-key"},
		{"api key", "API-Key", "synthetic-client-key"},
		{"bearer", "Authorization", "Bearer synthetic-client-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", nil)
			req.Header.Set(tc.header, tc.value)
			if !cfg.Authorize(req) {
				t.Fatal("valid client key rejected")
			}
		})
	}
}

func TestAuthorizeRejectsWrongOrQueryKey(t *testing.T) {
	cfg := Config{Enabled: true, APIKey: "synthetic-client-key"}
	for _, tc := range []struct {
		name  string
		setup func(*http.Request)
	}{
		{"missing", func(_ *http.Request) {}},
		{"wrong", func(r *http.Request) { r.Header.Set("X-API-Key", "wrong-key") }},
		{"query", func(r *http.Request) { r.URL.RawQuery = "api_key=synthetic-client-key" }},
		{"custom context header", func(r *http.Request) { r.Header.Set("X-Context-Lens-Key", "synthetic-client-key") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", nil)
			tc.setup(req)
			if cfg.Authorize(req) {
				t.Fatal("invalid client key accepted")
			}
		})
	}
}

func TestMiddlewareReturnsOpaqueUnauthorizedResponse(t *testing.T) {
	cfg := Config{Enabled: true, APIKey: "synthetic-client-key"}
	handler := cfg.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", nil)
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", recorder.Code)
	}
	if recorder.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("missing WWW-Authenticate")
	}
	if recorder.Body.String() != "context-lens: authentication required\n" {
		t.Fatalf("unexpected unauthorized body")
	}

	recorder = httptest.NewRecorder()
	req.Header.Set("Authorization", "Bearer synthetic-client-key")
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("authorized status=%d, want 204", recorder.Code)
	}
}

func TestDisabledMiddlewareIsTransparent(t *testing.T) {
	called := false
	handler := (Config{}).Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/healthz", nil))
	if !called || recorder.Code != http.StatusNoContent {
		t.Fatalf("disabled middleware did not remain transparent")
	}
}
