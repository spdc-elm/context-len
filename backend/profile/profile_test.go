package profile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"context-lens/backend/config"
)

func localProfile(t *testing.T, origin string) Profile {
	t.Helper()
	p, err := New(Profile{ID: "local", Label: "Local mock", Origin: origin})
	if err != nil {
		t.Fatalf("new profile: %v", err)
	}
	return p
}

func TestURLMappingPreservesRawQueryAndEscapedUnknownPath(t *testing.T) {
	p := localProfile(t, "http://127.0.0.1:43123/gateway")
	mapped, err := p.URLFor(DefaultResponsesPath, DefaultResponsesPath, "z=%26&x=1+2&x=%2F")
	if err != nil {
		t.Fatalf("responses URL: %v", err)
	}
	if got, want := mapped.Path, "/gateway/v1/responses"; got != want {
		t.Fatalf("mapped path = %q, want %q", got, want)
	}
	if got, want := mapped.RawQuery, "z=%26&x=1+2&x=%2F"; got != want {
		t.Fatalf("raw query = %q, want %q", got, want)
	}
	if got, want := mapped.EscapedPath(), "/gateway/v1/responses"; got != want {
		t.Fatalf("escaped mapped path = %q, want %q", got, want)
	}

	unknownPath := "/v1/custom/a/b"
	unknownEscaped := "/v1/custom/a%2Fb"
	unknown, err := p.URLFor(unknownPath, unknownEscaped, "q=%2F")
	if err != nil {
		t.Fatalf("unknown URL: %v", err)
	}
	if got, want := unknown.EscapedPath(), "/gateway/v1/custom/a%2Fb"; got != want {
		t.Fatalf("unknown escaped path = %q, want %q", got, want)
	}
	if unknown.RawQuery != "q=%2F" {
		t.Fatalf("unknown query changed: %q", unknown.RawQuery)
	}
}

func TestPathMappingCanSelectSameProtocolPaths(t *testing.T) {
	p, err := New(Profile{
		ID:     "custom",
		Origin: "http://127.0.0.1:43123/base",
		Paths: PathMapping{
			Responses:         "/provider/responses",
			ChatCompletions:   "/provider/chat",
			AnthropicMessages: "/provider/messages",
			Models:            "/provider/models",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ in, want string }{
		{DefaultResponsesPath, "/provider/responses"},
		{DefaultChatCompletionsPath, "/provider/chat"},
		{DefaultAnthropicMessagesPath, "/provider/messages"},
		{DefaultModelsPath, "/provider/models"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := p.ResolvePath(tc.in, tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("resolved path = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRejectsNonLoopbackOriginsAndURLInjection(t *testing.T) {
	bad := []string{
		"https://example.com",
		"http://169.254.169.254/latest/meta-data",
		"http://10.0.0.1:1234",
		"http://[::ffff:169.254.169.254]",
		"http://127.0.0.1.evil.test",
		"http://user:pass@127.0.0.1",
		"http://127.0.0.1?next=https://evil.test",
		"http://127.0.0.1/base/../escape",
		"http://127.0.0.1/base%2f..%2fescape",
		"http://127.0.0.1\r\nX-Evil: yes",
	}
	for _, origin := range bad {
		t.Run(fmt.Sprintf("origin_%q", origin), func(t *testing.T) {
			if _, err := New(Profile{ID: "bad", Origin: origin}); err == nil {
				t.Fatalf("unsafe origin accepted: %q", origin)
			}
		})
	}
	for _, path := range []string{"../escape", "/a/../escape", "/a/%2e%2e/escape", "/%2f%2fevil", "/a\\b", "/a\r\nb"} {
		t.Run("path_"+fmt.Sprintf("%q", path), func(t *testing.T) {
			p := localProfile(t, "http://127.0.0.1:43123")
			if _, err := p.BuildURL(path, ""); err == nil {
				t.Fatalf("unsafe path accepted: %q", path)
			}
		})
	}
}

func TestLocalhostOriginIsAllowedButOnlyLoopbackDialed(t *testing.T) {
	p, err := New(Profile{ID: "localhost", Origin: "http://localhost:43123"})
	if err != nil {
		t.Fatalf("localhost should be allowed: %v", err)
	}
	if _, err := p.OriginURL(); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Profile{ID: "ipv6", Origin: "http://[::1]:43123"}); err != nil {
		t.Fatalf("IPv6 loopback should be allowed: %v", err)
	}
}

func TestApplyHeadersSeparatesInboundAndUpstreamCredentials(t *testing.T) {
	store := config.NewMemoryCredentialStore()
	const secret = "upstream-secret-never-in-diff"
	if err := store.Put("local/provider", secret); err != nil {
		t.Fatal(err)
	}
	p, err := New(Profile{
		ID:     "local",
		Origin: "http://127.0.0.1:43123",
		Credential: CredentialSpec{
			Scheme:    CredentialBearer,
			Reference: "local/provider",
		},
		AdditionalHeaders: http.Header{
			"anthropic-version": {"2023-06-01"},
			"X-Trace":           {"trace-1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	inbound := http.Header{
		"Authorization":  {"Bearer harness-secret"},
		"X-API-Key":      {"harness-api-secret"},
		"Cookie":         {"session=harness-secret"},
		"Connection":     {"keep-alive"},
		"Content-Length": {"999"},
		"Content-Type":   {"application/json"},
		"Accept":         {"text/event-stream"},
		"X-Trace":        {"inbound-trace"},
	}
	result, err := p.ApplyHeaders(context.Background(), inbound, store)
	if err != nil {
		t.Fatalf("apply headers: %v", err)
	}
	out := result.Headers
	if got := out.Get("Authorization"); got != "Bearer "+secret {
		t.Fatalf("authorization = %q", got)
	}
	if got := out.Get("X-API-Key"); got != "" {
		t.Fatalf("inbound API key leaked: %q", got)
	}
	if got := out.Get("Cookie"); got != "" {
		t.Fatalf("inbound cookie leaked: %q", got)
	}
	for _, name := range []string{"Connection", "Content-Length"} {
		if got := out.Get(name); got != "" {
			t.Fatalf("connection-owned header %s leaked: %q", name, got)
		}
	}
	if got := out.Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q", got)
	}
	if got := out.Get("Anthropic-Version"); got != "2023-06-01" {
		t.Fatalf("additional protocol header = %q", got)
	}
	if got := out.Get("X-Trace"); got != "trace-1" {
		t.Fatalf("profile header did not override inbound value: %q", got)
	}
	if len(result.Diff.Changes) == 0 {
		t.Fatal("header diff is empty")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(secret)) || bytes.Contains(encoded, []byte("harness-secret")) {
		t.Fatalf("credential leaked in result JSON: %s", encoded)
	}
	if strings.Contains(fmt.Sprintf("%v", result), secret) || strings.Contains(fmt.Sprintf("%#v", result), secret) {
		t.Fatalf("credential leaked in result diagnostic")
	}
	// Filtering and injection must not mutate the inbound map.
	if got := inbound.Get("Authorization"); got != "Bearer harness-secret" {
		t.Fatalf("inbound headers mutated: %q", got)
	}
}

func TestCredentialSchemes(t *testing.T) {
	store := config.NewMemoryCredentialStore()
	if err := store.Put("secret", "user:password"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		scheme CredentialScheme
		header string
		want   string
	}{
		{"bearer", CredentialBearer, "Authorization", "Bearer user:password"},
		{"api key", CredentialAPIKey, "X-Provider-Key", "user:password"},
		{"basic", CredentialBasic, "Authorization", "Basic " + BasicCredential("user:password")},
		{"header", CredentialHeader, "X-Provider-Token", "user:password"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			credential := CredentialSpec{Scheme: tc.scheme, Reference: "secret"}
			if tc.scheme == CredentialAPIKey || tc.scheme == CredentialHeader {
				credential.Header = tc.header
			}
			p, err := New(Profile{ID: tc.name, Origin: "http://127.0.0.1:43123", Credential: credential})
			if err != nil {
				t.Fatal(err)
			}
			result, err := p.ApplyHeaders(context.Background(), nil, store)
			if err != nil {
				t.Fatal(err)
			}
			if got := result.Headers.Get(tc.header); got != tc.want {
				t.Fatalf("header %s = %q, want %q", tc.header, got, tc.want)
			}
		})
	}
}

func TestRejectsHeaderCRLFAndDoesNotEchoSecretFromStoreErrors(t *testing.T) {
	badHeaders := []http.Header{
		{"X-Bad\nName": {"value"}},
		{"X-Bad": {"value\r\nnext"}},
	}
	for i, headers := range badHeaders {
		if _, err := New(Profile{ID: fmt.Sprintf("bad-%d", i), Origin: "http://127.0.0.1:43123", AdditionalHeaders: headers}); err == nil {
			t.Fatalf("unsafe additional headers accepted: %#v", headers)
		}
	}

	p, err := New(Profile{ID: "error-store", Origin: "http://127.0.0.1:43123", Credential: CredentialSpec{Scheme: CredentialBearer, Reference: "secret-ref"}})
	if err != nil {
		t.Fatal(err)
	}
	const secret = "secret-in-error-must-not-echo"
	store := errorStore{err: errors.New("backend failed: " + secret)}
	result, err := p.ApplyHeaders(context.Background(), nil, store)
	if err == nil || result.Headers != nil {
		t.Fatal("error store unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("store secret echoed in error: %v", err)
	}
}

type errorStore struct{ err error }

func (s errorStore) Resolve(context.Context, string) (string, error) { return "", s.err }

func TestRedirectsAreNotFollowed(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		destinationCalls.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	defer destination.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", destination.URL+"/should-not-follow")
		w.WriteHeader(http.StatusFound)
		_, _ = io.WriteString(w, "redirect body")
	}))
	defer redirector.Close()

	p := localProfile(t, redirector.URL)
	client, err := NewHTTPClient(p)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get(redirector.URL + "/start")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusFound || string(body) != "redirect body" {
		t.Fatalf("redirect response = %d %q", resp.StatusCode, body)
	}
	if got := destinationCalls.Load(); got != 0 {
		t.Fatalf("redirect destination called %d times", got)
	}
}

func TestHTTPClientRejectsNonLoopbackDialEvenIfConstructedAddressIsUnsafe(t *testing.T) {
	p := localProfile(t, "http://127.0.0.1:43123")
	client, err := NewHTTPClient(p)
	if err != nil {
		t.Fatal(err)
	}
	// The client itself is loopback-only even when a caller accidentally hands
	// it an absolute URL unrelated to the validated profile.
	resp, err := client.Get("http://example.com/")
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("non-loopback absolute URL was allowed by loopback dialer")
	}
}

func TestURLForRequestRequiresConsistentPathComponents(t *testing.T) {
	p := localProfile(t, "http://127.0.0.1:43123")
	req := &http.Request{URL: &url.URL{Path: "/v1/a/b", RawPath: "/v1/a%2Fb"}}
	if _, err := p.URLForRequest(req); err != nil {
		t.Fatal(err)
	}
	bad := &http.Request{URL: &url.URL{Path: "/v1/a/b", RawPath: "/v1/other"}}
	if _, err := p.URLForRequest(bad); err == nil {
		t.Fatal("inconsistent path components accepted")
	}
	if _, err := p.URLForRequest(nil); err == nil {
		t.Fatal("nil request accepted")
	}
}

func TestHeaderPolicyContextCancellation(t *testing.T) {
	store := config.NewMemoryCredentialStore()
	if err := store.Put("ref", "secret"); err != nil {
		t.Fatal(err)
	}
	p, err := New(Profile{ID: "cancel", Origin: "http://127.0.0.1:43123", Credential: CredentialSpec{Scheme: CredentialBearer, Reference: "ref"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = p.ApplyHeaders(ctx, nil, store)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestProfileJSONContainsReferenceButNoSecret(t *testing.T) {
	const secret = "json-secret-never-here"
	p, err := New(Profile{ID: "json", Origin: "http://127.0.0.1:43123", Credential: CredentialSpec{Scheme: CredentialAPIKey, Reference: "ref"}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(secret)) || !bytes.Contains(encoded, []byte("credential_ref")) {
		t.Fatalf("profile JSON = %s", encoded)
	}
}

func TestURLForDoesNotChangeBodyBecauseItOnlyUsesEnvelopeValues(t *testing.T) {
	p := localProfile(t, "http://127.0.0.1:43123")
	body := []byte(`{"model":"untouched","unknown":{"x":1}}`)
	before := append([]byte(nil), body...)
	if _, err := p.URLFor(DefaultResponsesPath, DefaultResponsesPath, ""); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, before) {
		t.Fatal("body changed while building profile URL")
	}
}
