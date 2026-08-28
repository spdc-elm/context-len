package e2e_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"context-lens/backend/proxy"
	"context-lens/backend/transport"
)

type fixtureCase struct {
	name         string
	endpoint     string
	requestFile  string
	responseFile string
	requestHash  string
	responseHash string
	contentType  string
}

var fixtureCases = []fixtureCase{
	{
		name:         "responses-json",
		endpoint:     "/v1/responses",
		requestFile:  "responses/json/request.json",
		responseFile: "responses/json/response.json",
		requestHash:  "d71c8c92fca36bf2d2a43ed8746562c3bb258578dcc4908bfbd91fb4c37291ac",
		responseHash: "cfef4128d09384fece0f8880f9106202c83fbbab2020dc6f21d30872301671ae",
		contentType:  "application/json",
	},
	{
		name:         "responses-sse",
		endpoint:     "/v1/responses",
		requestFile:  "responses/sse/request.json",
		responseFile: "responses/sse/response.sse",
		requestHash:  "40937f7c120572f4839a31b273ee1f02dafedcf5ff9bcfe94b92be48f565f27c",
		responseHash: "d60c1daacd88cffe5798f78032a2eccbec94cd2aad74766180c8448c485faf70",
		contentType:  "text/event-stream; charset=utf-8",
	},
	{
		name:         "chat-completions-json",
		endpoint:     "/v1/chat/completions",
		requestFile:  "chat_completions/json/request.json",
		responseFile: "chat_completions/json/response.json",
		requestHash:  "79a5468dae3311be1b23d16a1c171a101d7ddce46b15b299a0f856d6780f7eda",
		responseHash: "03928165555fa0b6ea99eaea431e05432bd82f9bbea38793c2abeed27e3fcd1f",
		contentType:  "application/json",
	},
	{
		name:         "chat-completions-sse",
		endpoint:     "/v1/chat/completions",
		requestFile:  "chat_completions/sse/request.json",
		responseFile: "chat_completions/sse/response.sse",
		requestHash:  "98fa81c9368ffc3d10010adec13a7958f094035772a674ce4e5a50e93f3c7938",
		responseHash: "4735981ae8fcc3ca0933cdb9e53be0e8e0c3ab8d3ecfad7ac6a7d6a2782953ca",
		contentType:  "text/event-stream; charset=utf-8",
	},
	{
		name:         "anthropic-messages-json",
		endpoint:     "/v1/messages",
		requestFile:  "anthropic_messages/json/request.json",
		responseFile: "anthropic_messages/json/response.json",
		requestHash:  "61fd7415ad6c53b68f70bddbd924dbc4bc4b1ba445e17836ed6ac416af71e472",
		responseHash: "5bbcaab37ebf8bddf689ad95835658c4d8630856f40a47a1c4b89db67b880e73",
		contentType:  "application/json",
	},
	{
		name:         "anthropic-messages-sse",
		endpoint:     "/v1/messages",
		requestFile:  "anthropic_messages/sse/request.json",
		responseFile: "anthropic_messages/sse/response.sse",
		requestHash:  "2876791a6c653eaa7ad75cca62519877e1c422e1a86687959ed170e14281f46f",
		responseHash: "c450f5ac81c4bdc3c3c99eeb14975cf8f9d623957068dd272c9dea264e4bb7a4",
		contentType:  "text/event-stream; charset=utf-8",
	},
}

// TestFixturePassPass sends every checked-in protocol fixture through the real
// HTTP proxy and local mock upstream. The proxy is deliberately observed only
// through wire bytes: no JSON or SSE parser is involved in this test.
func TestFixturePassPass(t *testing.T) {
	for _, tc := range fixtureCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			requestBody := readFixture(t, tc.requestFile)
			responseBody := readFixture(t, tc.responseFile)
			assertSHA256(t, "fixture request", requestBody, tc.requestHash)
			assertSHA256(t, "fixture response", responseBody, tc.responseHash)

			observed := make(chan upstreamRequest, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					return
				}
				observed <- upstreamRequest{
					method:      r.Method,
					escapedPath: r.URL.EscapedPath(),
					rawQuery:    r.URL.RawQuery,
					body:        body,
					headers:     r.Header.Clone(),
				}
				w.Header().Set("Content-Type", tc.contentType)
				w.Header().Set("X-Upstream-Case", tc.name)
				w.Header().Set("X-Upstream-Connection", "must-not-be-hop-copied")
				w.Header().Set("Connection", "close")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(responseBody)
			}))
			defer upstream.Close()

			upstreamURL, err := url.Parse(upstream.URL)
			if err != nil {
				t.Fatal(err)
			}
			tr, err := transport.New(transport.Config{
				BaseURL: upstreamURL,
				HeaderPolicy: transport.HeaderPolicy{Additional: http.Header{
					"X-Profile-Marker": {"fixture-profile"},
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			proxyServer := httptest.NewServer(proxy.NewHandler(tr))
			defer proxyServer.Close()

			req, err := http.NewRequest(http.MethodPost, proxyServer.URL+tc.endpoint, bytes.NewReader(requestBody))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", requestContentType(tc.requestFile))
			req.Header.Set("X-Trace-ID", "fixture-trace")
			// These are intentionally synthetic inbound credentials. The default
			// header policy must not forward them to the local upstream.
			req.Header.Set("Authorization", "Bearer inbound-token")
			req.Header.Set("Cookie", "session=discarded")
			req.Header.Set("Connection", "keep-alive")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			gotResponse, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
			}
			if got := resp.Header.Get("Content-Type"); got != tc.contentType {
				t.Fatalf("content-type = %q, want %q", got, tc.contentType)
			}
			if got := resp.Header.Get("X-Upstream-Case"); got != tc.name {
				t.Fatalf("upstream marker = %q, want %q", got, tc.name)
			}
			if got := resp.Header.Get("Connection"); got != "" {
				t.Fatalf("hop-by-hop Connection leaked downstream: %q", got)
			}
			if got := resp.Header.Get("X-Upstream-Connection"); got != "must-not-be-hop-copied" {
				t.Fatalf("regular response header = %q", got)
			}
			assertSHA256(t, "downstream response", gotResponse, tc.responseHash)
			if !bytes.Equal(gotResponse, responseBody) {
				t.Fatal("downstream response bytes differ from fixture")
			}

			var got upstreamRequest
			select {
			case got = <-observed:
			case <-time.After(2 * time.Second):
				t.Fatal("mock upstream did not observe request")
			}
			if got.method != http.MethodPost {
				t.Fatalf("upstream method = %q, want POST", got.method)
			}
			if got.escapedPath != tc.endpoint {
				t.Fatalf("upstream escaped path = %q, want %q", got.escapedPath, tc.endpoint)
			}
			if got.rawQuery != "" {
				t.Fatalf("upstream raw query = %q, want empty", got.rawQuery)
			}
			assertSHA256(t, "upstream request", got.body, tc.requestHash)
			if !bytes.Equal(got.body, requestBody) {
				t.Fatal("upstream request bytes differ from fixture")
			}
			if got.headers.Get("X-Trace-ID") != "fixture-trace" {
				t.Fatalf("trace header was not forwarded: %q", got.headers.Get("X-Trace-ID"))
			}
			if got.headers.Get("X-Profile-Marker") != "fixture-profile" {
				t.Fatalf("profile header was not injected: %q", got.headers.Get("X-Profile-Marker"))
			}
			for _, name := range []string{"Authorization", "Cookie", "Connection", "Proxy-Authorization"} {
				if value := got.headers.Get(name); value != "" {
					t.Fatalf("inbound credential/hop header %s leaked upstream: %q", name, value)
				}
			}
		})
	}
}

type upstreamRequest struct {
	method      string
	escapedPath string
	rawQuery    string
	body        []byte
	headers     http.Header
}

func TestModelsTransparent(t *testing.T) {
	responseBody := readFixture(t, "models/response.json")
	const responseHash = "88e8332506921c51e3e0cea971288da4b571cdd8dfbf653b1a418978748f9a57"
	assertSHA256(t, "models fixture response", responseBody, responseHash)

	observed := make(chan upstreamRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		observed <- upstreamRequest{method: r.Method, escapedPath: r.URL.EscapedPath(), rawQuery: r.URL.RawQuery, body: body, headers: r.Header.Clone()}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Case", "models")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(responseBody)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := transport.New(transport.Config{BaseURL: upstreamURL})
	if err != nil {
		t.Fatal(err)
	}
	proxyServer := httptest.NewServer(proxy.NewHandler(tr))
	defer proxyServer.Close()

	const rawQuery = "owner=fixture%2Fowner&include=&duplicate=one&duplicate=two&space=a+b"
	req, err := http.NewRequest(http.MethodGet, proxyServer.URL+"/v1/models?"+rawQuery, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer inbound-model-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	gotBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type = %q, want application/json", got)
	}
	if got := resp.Header.Get("X-Upstream-Case"); got != "models" {
		t.Fatalf("upstream marker = %q, want models", got)
	}
	assertSHA256(t, "downstream models response", gotBody, responseHash)
	if !bytes.Equal(gotBody, responseBody) {
		t.Fatal("downstream models response differs from fixture")
	}
	got := waitObserved(t, observed)
	if got.method != http.MethodGet || got.escapedPath != "/v1/models" || got.rawQuery != rawQuery {
		t.Fatalf("upstream request = method %q path %q query %q", got.method, got.escapedPath, got.rawQuery)
	}
	if len(got.body) != 0 {
		t.Fatalf("GET /v1/models upstream body length = %d, want 0", len(got.body))
	}
	if got.headers.Get("Authorization") != "" {
		t.Fatal("inbound authorization leaked on /v1/models")
	}
}

func TestEscapedPathAndRawQueryPreserved(t *testing.T) {
	requestBody := readFixture(t, "responses/json/request.json")
	responseBody := readFixture(t, "responses/json/response.json")
	const requestHash = "d71c8c92fca36bf2d2a43ed8746562c3bb258578dcc4908bfbd91fb4c37291ac"
	const responseHash = "cfef4128d09384fece0f8880f9106202c83fbbab2020dc6f21d30872301671ae"
	assertSHA256(t, "escaped-path request", requestBody, requestHash)
	assertSHA256(t, "escaped-path response", responseBody, responseHash)

	observed := make(chan upstreamRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		observed <- upstreamRequest{method: r.Method, escapedPath: r.URL.EscapedPath(), rawQuery: r.URL.RawQuery, body: body, headers: r.Header.Clone()}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(responseBody)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := transport.New(transport.Config{BaseURL: upstreamURL})
	if err != nil {
		t.Fatal(err)
	}
	proxyServer := httptest.NewServer(proxy.NewHandler(tr))
	defer proxyServer.Close()

	const escapedPath = "/v1/responses/%2Fopaque"
	const rawQuery = "filter=a+b&path=%2F&empty=&duplicate=one&duplicate=two"
	req, err := http.NewRequest(http.MethodPost, proxyServer.URL+escapedPath+"?"+rawQuery, bytes.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	gotBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	assertSHA256(t, "escaped-path downstream response", gotBody, responseHash)
	got := waitObserved(t, observed)
	if got.escapedPath != escapedPath {
		t.Fatalf("upstream escaped path = %q, want %q", got.escapedPath, escapedPath)
	}
	if got.rawQuery != rawQuery {
		t.Fatalf("upstream raw query = %q, want %q", got.rawQuery, rawQuery)
	}
	assertSHA256(t, "escaped-path upstream request", got.body, requestHash)
	if !bytes.Equal(got.body, requestBody) {
		t.Fatal("escaped-path request bytes changed")
	}
}

func TestUpstreamHTTPErrorIsObserved(t *testing.T) {
	const body = `{"error":{"type":"fixture_upstream_failure","message":"local mock failure","unknown":true}}\n`
	const responseHeader = "application/json; charset=utf-8"
	observed := make(chan upstreamRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestBody, _ := io.ReadAll(r.Body)
		observed <- upstreamRequest{method: r.Method, escapedPath: r.URL.EscapedPath(), rawQuery: r.URL.RawQuery, body: requestBody, headers: r.Header.Clone()}
		w.Header().Set("Content-Type", responseHeader)
		w.Header().Set("X-Upstream-Error", "fixture")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(body))
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := transport.New(transport.Config{BaseURL: upstreamURL})
	if err != nil {
		t.Fatal(err)
	}
	proxyServer := httptest.NewServer(proxy.NewHandler(tr))
	defer proxyServer.Close()

	req, err := http.NewRequest(http.MethodPost, proxyServer.URL+"/v1/responses", strings.NewReader(`{"model":"fixture-error","stream":false}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Content-Type") != responseHeader {
		t.Fatalf("content-type = %q, want %q", resp.Header.Get("Content-Type"), responseHeader)
	}
	if resp.Header.Get("X-Upstream-Error") != "fixture" {
		t.Fatal("upstream error marker was not preserved")
	}
	if !bytes.Equal(got, []byte(body)) {
		t.Fatalf("upstream error body changed: %q", got)
	}
	_ = waitObserved(t, observed)
}

func TestUpstreamTransportErrorReturnsBadGateway(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	}))
	upstreamURL := upstream.URL
	upstream.Close()

	parsed, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := transport.New(transport.Config{BaseURL: parsed})
	if err != nil {
		t.Fatal(err)
	}
	proxyServer := httptest.NewServer(proxy.NewHandler(tr))
	defer proxyServer.Close()

	req, err := http.NewRequest(http.MethodGet, proxyServer.URL+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if len(body) == 0 {
		t.Fatal("bad gateway response had empty body")
	}
	if got := resp.Header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("bad gateway content-type = %q, want text/plain; charset=utf-8", got)
	}
}

func TestCancellationPropagatesToUpstream(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	var startedOnce, cancelledOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		startedOnce.Do(func() { close(started) })
		<-r.Context().Done()
		cancelledOnce.Do(func() { close(cancelled) })
	}))
	defer func() {
		// Ensure a failing cancellation assertion cannot leave the mock's
		// blocked response goroutine holding httptest.Server.Close hostage.
		upstream.CloseClientConnections()
		upstream.Close()
	}()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := transport.New(transport.Config{BaseURL: upstreamURL})
	if err != nil {
		t.Fatal(err)
	}

	// Invoke the real proxy handler with a request context representing a
	// client that disconnected. Calling ServeHTTP directly avoids relying on
	// net/http's server-side close-notify timing while still exercising the
	// transport round trip and upstream request context.
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/responses", strings.NewReader(`{"model":"fixture-cancel","stream":true}`)).WithContext(ctx)
	result := make(chan struct{})
	go func() {
		proxy.NewHandler(tr).ServeHTTP(httptest.NewRecorder(), req)
		close(result)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("mock upstream did not start before cancellation")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request context was not canceled")
	}
	select {
	case <-result:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy handler did not return after cancellation")
	}
}

func waitObserved(t *testing.T, observed <-chan upstreamRequest) upstreamRequest {
	t.Helper()
	select {
	case got := <-observed:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("mock upstream did not observe request")
		return upstreamRequest{}
	}
}

func readFixture(t *testing.T, relative string) []byte {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(source)))
	body, err := os.ReadFile(filepath.Join(root, "tests", "fixtures", filepath.FromSlash(relative)))
	if err != nil {
		t.Fatalf("read fixture %s: %v", relative, err)
	}
	return body
}

func assertSHA256(t *testing.T, label string, body []byte, expected string) {
	t.Helper()
	sum := sha256.Sum256(body)
	got := hex.EncodeToString(sum[:])
	if got != expected {
		t.Fatalf("%s sha256 = %s, want %s", label, got, expected)
	}
}

func requestContentType(requestFile string) string {
	if strings.HasSuffix(requestFile, "/request.json") {
		return "application/json"
	}
	return "application/octet-stream"
}
