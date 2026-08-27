// Package transport implements the protocol-agnostic upstream HTTP leg.
//
// The transport deliberately treats a request body as an opaque stream.  It
// does not inspect, decode, encode, aggregate, or otherwise reconstruct JSON
// or SSE.  URL joining keeps the escaped path and raw query supplied by the
// caller, while HeaderPolicy only performs the connection/authentication
// changes required at a proxy boundary.
package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"context-lens/backend/wire"
)

var (
	// ErrNilRequest indicates that a caller supplied no request to prepare.
	ErrNilRequest = errors.New("transport: nil request")
	// ErrNilUpstream indicates that a transport has no configured origin.
	ErrNilUpstream = errors.New("transport: upstream URL is required")
)

// hopByHopHeaders are managed by the HTTP connection and must not be copied
// blindly between hops.  In particular, preserving a client Connection value
// could nominate unrelated headers for removal on the upstream leg.
var hopByHopHeaders = map[string]struct{}{
	"connection":          {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
}

// HeaderPolicy controls the intentionally visible header changes made while
// creating an upstream request.  Additional headers are profile-owned values
// (for example a server-side credential) and therefore override an inbound
// value with the same name.  Header values are copied before they are used.
type HeaderPolicy struct {
	// ForwardInboundCredentials defaults to false.  When false, inbound
	// Authorization, Proxy-Authorization and x-api-key headers are removed.
	// Cookie is also removed by default because a local proxy must not leak
	// browser/session credentials to an unrelated upstream origin.
	ForwardInboundCredentials bool
	// Additional are safe, server-controlled headers to inject upstream.
	Additional http.Header
	// Remove contains optional case-insensitive header names to remove after
	// the built-in policy has run.
	Remove []string
}

// DefaultHeaderPolicy returns the safe default policy for a local proxy.
func DefaultHeaderPolicy() HeaderPolicy { return HeaderPolicy{} }

// Apply clones and filters inbound headers.  It validates names and values so
// a programmatic caller cannot introduce CRLF injection at the next hop.
func (p HeaderPolicy) Apply(in http.Header) (http.Header, error) {
	if err := wire.ValidateHeaders(in); err != nil {
		return nil, err
	}
	if err := wire.ValidateHeaders(p.Additional); err != nil {
		return nil, fmt.Errorf("transport: invalid additional header: %w", err)
	}

	nominated := make(map[string]struct{})
	for name, values := range in {
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
	out := make(http.Header, len(in)+len(p.Additional))
	for name, values := range in {
		lower := strings.ToLower(strings.TrimSpace(name))
		if _, nominatedHop := nominated[lower]; nominatedHop {
			continue
		}
		if _, hop := hopByHopHeaders[lower]; hop || lower == "host" || lower == "content-length" {
			continue
		}
		if !p.ForwardInboundCredentials && isInboundCredential(lower) {
			continue
		}
		if containsFold(p.Remove, name) {
			continue
		}
		if values == nil {
			out[name] = nil
		} else {
			out[name] = append([]string(nil), values...)
		}
	}
	for name, values := range p.Additional {
		// A nil value remains a nil value.  net/http treats it as an omitted
		// header, and retaining the shape is useful to callers inspecting the
		// prepared request.
		if values == nil {
			out[name] = nil
		} else {
			out[name] = append([]string(nil), values...)
		}
	}
	return out, nil
}

func containsFold(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), target) {
			return true
		}
	}
	return false
}

func isInboundCredential(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	switch name {
	case "authorization", "proxy-authorization", "x-api-key", "api-key", "apikey", "x-apikey", "cookie", "set-cookie":
		return true
	}
	for _, marker := range []string{"authorization", "api-key", "apikey", "auth-token", "access-token", "refresh-token", "client-secret", "secret", "credential", "password", "session-token"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

// Config configures a generic HTTP transport.  BaseURL must be an absolute
// http(s) URL with no query or fragment.  Incoming request paths are appended
// to BaseURL.Path without URL normalisation, and incoming RawQuery replaces
// any (forbidden) base query.
type Config struct {
	BaseURL *url.URL
	// BaseURLString is a convenience for callers loading profile data.  If
	// BaseURL is non-nil this field is ignored.
	BaseURLString string
	// Client is optional.  A shallow clone is made so setting a safe default
	// CheckRedirect does not mutate a caller-owned client.
	Client *http.Client
	// RoundTripper is used when Client is nil, or when Client.Transport is nil.
	// Supplying one is useful for local mock upstreams and cancellation tests.
	RoundTripper http.RoundTripper
	HeaderPolicy HeaderPolicy
	// RequireLoopback rejects non-loopback dial targets after DNS resolution.
	RequireLoopback bool
}

// Transport performs one same-protocol upstream round trip.  It is safe for
// concurrent use after construction.
type Transport struct {
	baseURL      url.URL
	client       *http.Client
	headerPolicy HeaderPolicy
}

// New validates cfg and constructs a transport.  Redirects are not followed
// by default; an upstream 3xx response is returned as-is for inspection and
// explicit operator handling.
func New(cfg Config) (*Transport, error) {
	base, err := configuredURL(cfg)
	if err != nil {
		return nil, err
	}
	if err := validateBaseURL(base); err != nil {
		return nil, err
	}

	client := http.Client{}
	if cfg.Client != nil {
		client = *cfg.Client
	}
	if client.Transport == nil {
		if cfg.RoundTripper != nil {
			client.Transport = cfg.RoundTripper
		} else {
			client.Transport = DefaultRoundTripper()
		}
	}
	if cfg.RequireLoopback {
		baseTransport, ok := client.Transport.(*http.Transport)
		if !ok {
			return nil, errors.New("transport: loopback safety requires *http.Transport")
		}
		safeTransport := baseTransport.Clone()
		safeTransport.Proxy = nil
		safeTransport.DisableCompression = true
		safeTransport.DialContext = loopbackDialContext(safeTransport.DialContext)
		client.Transport = safeTransport
	}
	if client.CheckRedirect == nil {
		client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
	if client.Jar != nil {
		// A caller-owned cookie jar would allow inbound/session cookies to
		// cross origins implicitly.  Keep explicit HeaderPolicy in charge.
		client.Jar = nil
	}

	return &Transport{baseURL: *base, client: &client, headerPolicy: cfg.HeaderPolicy}, nil
}

func loopbackDialContext(base func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	if base == nil {
		base = (&net.Dialer{}).DialContext
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, errors.New("transport: invalid dial address")
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, errors.New("transport: upstream resolution failed")
		}
		for _, candidate := range ips {
			if candidate.IP.IsLoopback() {
				return base(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
			}
		}
		return nil, errors.New("transport: blocked non-loopback dial target")
	}
}

func configuredURL(cfg Config) (*url.URL, error) {
	if cfg.BaseURL != nil {
		copy := *cfg.BaseURL
		return &copy, nil
	}
	if strings.TrimSpace(cfg.BaseURLString) == "" {
		return nil, ErrNilUpstream
	}
	parsed, err := url.Parse(cfg.BaseURLString)
	if err != nil {
		return nil, fmt.Errorf("transport: parse upstream URL: %w", err)
	}
	return parsed, nil
}

func validateBaseURL(base *url.URL) error {
	if base == nil {
		return ErrNilUpstream
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return fmt.Errorf("transport: unsupported upstream scheme %q", base.Scheme)
	}
	if base.Host == "" {
		return errors.New("transport: upstream URL must include a host")
	}
	if base.User != nil {
		return errors.New("transport: upstream URL userinfo is not allowed; inject credentials via HeaderPolicy")
	}
	if base.RawQuery != "" || base.ForceQuery {
		return errors.New("transport: upstream URL must not include a query")
	}
	if base.Fragment != "" {
		return errors.New("transport: upstream URL must not include a fragment")
	}
	if _, err := url.PathUnescape(base.EscapedPath()); err != nil {
		return fmt.Errorf("transport: upstream URL has invalid escaped path: %w", err)
	}
	return nil
}

// DefaultRoundTripper returns a conservative standard-library transport.  In
// particular DisableCompression is true so response bytes are not silently
// decoded before the proxy can release them unchanged.
func DefaultRoundTripper() http.RoundTripper {
	return &http.Transport{
		Proxy:                 nil, // profiles inject credentials directly; environment proxies cannot bypass origin safety,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90e9, // 90 seconds, without importing time here
		TLSHandshakeTimeout:   10e9,
		ExpectContinueTimeout: 1e9,
		DisableCompression:    true,
	}
}

// BaseURL returns a copy of the configured origin URL.  The returned value may
// be changed by the caller without affecting this transport.
func (t *Transport) BaseURL() *url.URL {
	if t == nil {
		return nil
	}
	copy := t.baseURL
	return &copy
}

// Client returns the configured HTTP client.  It is intended for diagnostics
// and tests; callers should prefer Do so URL and header policy cannot be
// bypassed accidentally.
func (t *Transport) Client() *http.Client {
	if t == nil {
		return nil
	}
	return t.client
}

// URLFor constructs the upstream URL for inbound.  EscapedPath and RawQuery
// are copied verbatim (apart from joining BaseURL.Path), while URL.Path is set
// to the corresponding decoded value for net/http's request machinery.
func (t *Transport) URLFor(inbound *http.Request) (*url.URL, error) {
	if t == nil {
		return nil, ErrNilUpstream
	}
	if inbound == nil || inbound.URL == nil {
		return nil, ErrNilRequest
	}
	return JoinURL(&t.baseURL, inbound.URL)
}

// JoinURL appends incoming's escaped path to base and copies incoming's raw
// query.  It intentionally does not use ResolveReference, path.Clean, or
// Query().Encode(), all of which can change wire-visible bytes.
func JoinURL(base, incoming *url.URL) (*url.URL, error) {
	if err := validateBaseURL(base); err != nil {
		return nil, err
	}
	if incoming == nil {
		return nil, ErrNilRequest
	}
	incomingEscaped := incoming.EscapedPath()
	decodedIncoming, err := url.PathUnescape(incomingEscaped)
	if err != nil {
		return nil, fmt.Errorf("transport: invalid request escaped path: %w", err)
	}
	for _, segment := range strings.Split(decodedIncoming, "/") {
		if segment == "." || segment == ".." {
			return nil, errors.New("transport: request path contains dot segment")
		}
	}
	if incomingEscaped == "" {
		incomingEscaped = "/"
	}
	baseEscaped := base.EscapedPath()
	if baseEscaped == "" {
		baseEscaped = "/"
	}
	escaped := joinEscapedPath(baseEscaped, incomingEscaped)
	decoded, err := url.PathUnescape(escaped)
	if err != nil {
		return nil, fmt.Errorf("transport: invalid request escaped path %q: %w", escaped, err)
	}
	out := *base
	out.Path = decoded
	// RawPath must be a valid encoding of Path.  Retaining it is what keeps
	// escaped slash case and other non-canonical-but-valid bytes observable
	// in RequestURI; clear it only when URL considers the encoding canonical.
	out.RawPath = escaped
	if out.EscapedPath() != escaped {
		out.RawPath = ""
		if out.EscapedPath() != escaped {
			return nil, fmt.Errorf("transport: cannot preserve escaped request path %q", escaped)
		}
	}
	out.RawQuery = incoming.RawQuery
	out.ForceQuery = incoming.ForceQuery
	out.Fragment = ""
	out.RawFragment = ""
	out.Opaque = ""
	out.User = nil
	return &out, nil
}

func joinEscapedPath(basePath, incomingPath string) string {
	if basePath == "" || basePath == "/" {
		if strings.HasPrefix(incomingPath, "/") {
			return incomingPath
		}
		return "/" + incomingPath
	}
	if incomingPath == "" || incomingPath == "/" {
		return strings.TrimRight(basePath, "/") + "/"
	}
	return strings.TrimRight(basePath, "/") + "/" + strings.TrimLeft(incomingPath, "/")
}

// PrepareRequest creates an upstream request without reading or modifying
// inbound.Body.  If bodyOverride is non-nil it is installed as the outbound
// body; this is the hook used by a future request gate for an explicit derived
// artifact.  A nil override means the inbound body is reused directly.
func (t *Transport) PrepareRequest(ctx context.Context, inbound *http.Request, bodyOverride io.ReadCloser) (*http.Request, error) {
	if t == nil {
		return nil, ErrNilUpstream
	}
	if inbound == nil {
		return nil, ErrNilRequest
	}
	if ctx == nil {
		ctx = context.Background()
	}
	upstreamURL, err := t.URLFor(inbound)
	if err != nil {
		return nil, err
	}
	headers, err := t.headerPolicy.Apply(inbound.Header)
	if err != nil {
		return nil, err
	}

	out := inbound.Clone(ctx)
	out.URL = upstreamURL
	out.RequestURI = ""
	out.Host = upstreamURL.Host
	out.Header = headers
	out.Trailer = nil
	out.TransferEncoding = nil
	if bodyOverride != nil {
		out.Body = bodyOverride
		out.GetBody = nil
		out.ContentLength = -1
	}
	// Request.Clone retains these fields, but client requests must not carry
	// server-only response bookkeeping.
	out.Response = nil
	return out, nil
}

// Do performs the prepared request using the configured http.Client.  The
// returned response body is the upstream body stream and must be closed by
// the caller.  No JSON/SSE processing occurs here.
func (t *Transport) Do(ctx context.Context, inbound *http.Request, bodyOverride io.ReadCloser) (*http.Response, error) {
	prepared, err := t.PrepareRequest(ctx, inbound, bodyOverride)
	if err != nil {
		return nil, err
	}
	return t.client.Do(prepared)
}

// DoRequest is a concise alias for Do when no body override is needed.
func (t *Transport) DoRequest(ctx context.Context, inbound *http.Request) (*http.Response, error) {
	return t.Do(ctx, inbound, nil)
}

// ValidateHeaderPolicy is useful for profile/configuration code that wants to
// fail before serving traffic.
func (p HeaderPolicy) Validate() error {
	_, err := p.Apply(nil)
	return err
}

// Ensure the package keeps io in its public API docs when a future compiler
// prunes an otherwise-unused alias in generated examples.
var _ io.Reader
