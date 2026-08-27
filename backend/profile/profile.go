// Package profile defines the single generic upstream profile used by the
// proxy.  A profile chooses a loopback-safe origin, path mapping, credential
// reference, and safe outbound headers.  It never chooses or rewrites a model
// and it never parses a request body.
package profile

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"context-lens/backend/config"
	"context-lens/backend/wire"
)

const (
	DefaultResponsesPath         = "/v1/responses"
	DefaultChatCompletionsPath   = "/v1/chat/completions"
	DefaultAnthropicMessagesPath = "/v1/messages"
	DefaultModelsPath            = "/v1/models"
)

// CredentialScheme controls how a secret resolved from Credential.Reference is
// placed on an outbound request.  Schemes affect headers only; request body
// bytes, including the model field, are never read by this package.
type CredentialScheme string

const (
	CredentialNone   CredentialScheme = "none"
	CredentialBearer CredentialScheme = "bearer"
	CredentialAPIKey CredentialScheme = "api_key"
	CredentialBasic  CredentialScheme = "basic"
	CredentialHeader CredentialScheme = "header"

	// Short aliases make configuration literals readable while retaining the
	// explicit Credential* names in documentation and JSON.
	SchemeNone   = CredentialNone
	SchemeBearer = CredentialBearer
	SchemeAPIKey = CredentialAPIKey
	SchemeBasic  = CredentialBasic
	SchemeHeader = CredentialHeader
)

// PathMapping maps known protocol ingress paths to same-protocol upstream
// paths.  Empty fields use the defaults above.  Unknown paths are forwarded
// unchanged under the validated origin, which keeps GET /v1/models and future
// provider endpoints transparent without introducing protocol conversion.
type PathMapping struct {
	Responses         string `json:"responses,omitempty"`
	ChatCompletions   string `json:"chat_completions,omitempty"`
	AnthropicMessages string `json:"anthropic_messages,omitempty"`
	Models            string `json:"models,omitempty"`
}

// DefaultPathMapping returns the same-protocol mapping used by a zero profile.
func DefaultPathMapping() PathMapping {
	return PathMapping{
		Responses:         DefaultResponsesPath,
		ChatCompletions:   DefaultChatCompletionsPath,
		AnthropicMessages: DefaultAnthropicMessagesPath,
		Models:            DefaultModelsPath,
	}
}

func (m PathMapping) withDefaults() PathMapping {
	d := DefaultPathMapping()
	if m.Responses != "" {
		d.Responses = m.Responses
	}
	if m.ChatCompletions != "" {
		d.ChatCompletions = m.ChatCompletions
	}
	if m.AnthropicMessages != "" {
		d.AnthropicMessages = m.AnthropicMessages
	}
	if m.Models != "" {
		d.Models = m.Models
	}
	return d
}

// CredentialSpec contains only a reference and placement metadata.  No secret
// value belongs in this type, so marshaling a Profile cannot disclose one.
type CredentialSpec struct {
	Scheme    CredentialScheme `json:"scheme,omitempty"`
	Reference string           `json:"credential_ref,omitempty"`
	// Header is used by api_key and header schemes.  api_key defaults to
	// X-API-Key.  Header names are validated before a request is built.
	Header string `json:"header,omitempty"`
}

// CredentialRef is an alias retaining the profile's secret-free reference
// shape for callers that use reference terminology.
type CredentialRef = CredentialSpec

// NetworkPolicy controls local upstream client behavior.  Loopback-only and
// no-redirect behavior are mandatory for the MVP and cannot be disabled by a
// profile.  Timeout zero means the standard client's no-total-timeout mode.
type NetworkPolicy struct {
	Timeout      time.Duration `json:"timeout,omitempty"`
	CaptureLimit int64         `json:"capture_limit,omitempty"`
}

// Profile is the persisted, secret-free description of one upstream.  Origin
// must be http(s) on a loopback literal or localhost.  AdditionalHeaders are
// constrained by Validate and are copied before every request.
type Profile struct {
	ID                string         `json:"profile_id"`
	Label             string         `json:"label,omitempty"`
	Origin            string         `json:"origin"`
	Paths             PathMapping    `json:"path_mapping,omitempty"`
	Credential        CredentialSpec `json:"credential,omitempty"`
	AdditionalHeaders http.Header    `json:"additional_headers,omitempty"`
	Network           NetworkPolicy  `json:"network,omitempty"`
}

// UpstreamProfile and ProfileConfig are aliases for callers whose domain
// vocabulary distinguishes an upstream profile from other profiles.  They do
// not create a second configuration shape or behavior.
type UpstreamProfile = Profile
type ProfileConfig = Profile

// New constructs and validates a profile.  It does not contact the origin or
// resolve credentials.
func New(p Profile) (Profile, error) {
	if err := p.Validate(); err != nil {
		return Profile{}, err
	}
	p = p.Clone()
	p.Paths = p.Paths.withDefaults()
	if p.Credential.Scheme == "" {
		p.Credential.Scheme = CredentialNone
	}
	return p, nil
}

// NewProfile is a readable constructor alias.
func NewProfile(p Profile) (Profile, error) { return New(p) }

// Clone returns a profile copy with independent header values.  It does not
// copy or resolve any secret because Profile contains references only.
func (p Profile) Clone() Profile {
	p.AdditionalHeaders = cloneHeaders(p.AdditionalHeaders)
	return p
}

// Validate checks origin, path, credential metadata, and header policy.  It
// intentionally does not perform DNS or network I/O; NewHTTPClient installs a
// loopback-only dialer for the final resolution check.
func (p Profile) Validate() error {
	if err := validateIdentifier(p.ID, "profile id", 128); err != nil {
		return err
	}
	if strings.TrimSpace(p.Label) != p.Label || strings.ContainsAny(p.Label, "\r\n") {
		return errors.New("profile: label contains surrounding whitespace or CRLF")
	}
	origin, err := parseOrigin(p.Origin)
	if err != nil {
		return err
	}
	if err := validateOriginPath(origin); err != nil {
		return err
	}
	mapping := p.Paths.withDefaults()
	for name, value := range map[string]string{
		"responses":          mapping.Responses,
		"chat_completions":   mapping.ChatCompletions,
		"anthropic_messages": mapping.AnthropicMessages,
		"models":             mapping.Models,
	} {
		if err := validateMappedPath(value); err != nil {
			return fmt.Errorf("profile: %s path: %w", name, err)
		}
	}
	if p.Network.Timeout < 0 {
		return errors.New("profile: timeout cannot be negative")
	}
	if p.Network.CaptureLimit < 0 {
		return errors.New("profile: capture limit cannot be negative")
	}
	if err := validateCredentialSpec(p.Credential); err != nil {
		return err
	}
	if err := validateAdditionalHeaders(p.AdditionalHeaders); err != nil {
		return err
	}
	return nil
}

func validateCredentialSpec(c CredentialSpec) error {
	scheme := c.Scheme
	if scheme == "" {
		scheme = CredentialNone
	}
	switch scheme {
	case CredentialNone:
		if c.Reference != "" || c.Header != "" {
			return errors.New("profile: credential reference/header require a credential scheme")
		}
	case CredentialBearer, CredentialBasic:
		if err := config.ValidateCredentialReference(c.Reference); err != nil {
			return fmt.Errorf("profile: credential reference: %w", err)
		}
	case CredentialAPIKey, CredentialScheme("api-key"), CredentialScheme("apikey"), CredentialScheme("x-api-key"):
		if err := config.ValidateCredentialReference(c.Reference); err != nil {
			return fmt.Errorf("profile: credential reference: %w", err)
		}
		header := c.Header
		if header == "" {
			header = "X-API-Key"
		}
		if err := validateCredentialHeaderName(header); err != nil {
			return fmt.Errorf("profile: credential header: %w", err)
		}
	case CredentialHeader:
		if err := config.ValidateCredentialReference(c.Reference); err != nil {
			return fmt.Errorf("profile: credential reference: %w", err)
		}
		if err := validateCredentialHeaderName(c.Header); err != nil {
			return fmt.Errorf("profile: credential header: %w", err)
		}
	default:
		return fmt.Errorf("profile: unsupported credential scheme %q", scheme)
	}
	return nil
}

func validateIdentifier(value, label string, max int) error {
	if value == "" {
		return fmt.Errorf("profile: %s is required", label)
	}
	if len(value) > max || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("profile: invalid %s", label)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("profile: invalid %s", label)
		}
	}
	return nil
}

func parseOrigin(raw string) (*url.URL, error) {
	if raw == "" || strings.ContainsAny(raw, "\r\n") {
		return nil, errors.New("profile: origin is required and must not contain CRLF")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("profile: invalid origin: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("profile: origin scheme %q is not allowed", u.Scheme)
	}
	if u.Host == "" || u.Opaque != "" || u.User != nil {
		return nil, errors.New("profile: origin must be an absolute URL without userinfo")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("profile: origin must not contain query or fragment")
	}
	if strings.ContainsAny(u.Host, "\r\n") {
		return nil, errors.New("profile: origin host contains CRLF")
	}
	host := u.Hostname()
	if host == "" || strings.Contains(host, "%") {
		return nil, errors.New("profile: origin host is invalid")
	}
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return nil, fmt.Errorf("profile: origin host %q is not loopback", host)
		}
	} else if !strings.EqualFold(host, "localhost") {
		return nil, fmt.Errorf("profile: origin host %q is not loopback", host)
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, errors.New("profile: origin port is invalid")
		}
	}
	return u, nil
}

func validateOriginPath(u *url.URL) error {
	if u == nil {
		return errors.New("profile: nil origin")
	}
	escaped := u.EscapedPath()
	if escaped == "" {
		return nil
	}
	return validatePath(escaped, true)
}

func validateMappedPath(path string) error { return validatePath(path, false) }

// validatePath permits only a path absolute to the origin.  It rejects dot
// segments after unescaping, backslashes, controls, and malformed escapes to
// prevent path traversal or parser differentials at the transport boundary.
func validatePath(path string, origin bool) error {
	if path == "" {
		return errors.New("path is empty")
	}
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return errors.New("path must begin with a single slash")
	}
	if strings.ContainsAny(path, "\\\r\n?#") {
		return errors.New("path contains forbidden characters")
	}
	for _, r := range path {
		if unicode.IsControl(r) {
			return errors.New("path contains control character")
		}
	}
	decoded, err := url.PathUnescape(path)
	if err != nil {
		return fmt.Errorf("path has malformed escape: %w", err)
	}
	if strings.ContainsAny(decoded, "\x00\r\n\\") {
		return errors.New("path contains forbidden decoded character")
	}
	if strings.HasPrefix(decoded, "//") {
		return errors.New("path must not decode to a network-path reference")
	}
	for _, segment := range strings.Split(decoded, "/") {
		if segment == "." || segment == ".." {
			return errors.New("path contains dot segment")
		}
	}
	if origin && strings.Contains(decoded, "?") {
		return errors.New("origin path must not contain query")
	}
	return nil
}

func validateAdditionalHeaders(headers http.Header) error {
	if err := wire.ValidateHeaders(headers); err != nil {
		return fmt.Errorf("profile: additional headers: %w", err)
	}
	for name, values := range headers {
		if err := validateHeaderName(name); err != nil {
			return fmt.Errorf("profile: additional header name: %w", err)
		}
		if isHopByHopHeader(name) || isInboundCredentialHeader(name) || strings.EqualFold(name, "host") || strings.EqualFold(name, "content-length") {
			return fmt.Errorf("profile: additional header %q is not safe", name)
		}
		if values == nil {
			continue
		}
		for _, value := range values {
			if err := validateHeaderValue(value); err != nil {
				return fmt.Errorf("profile: additional header %q: %w", name, err)
			}
		}
	}
	return nil
}

func validateCredentialHeaderName(name string) error {
	if err := validateHeaderName(name); err != nil {
		return err
	}
	if isHopByHopHeader(name) || strings.EqualFold(name, "host") || strings.EqualFold(name, "content-length") {
		return errors.New("credential header is hop-by-hop or connection-owned")
	}
	return nil
}

func validateHeaderName(name string) error {
	if name == "" || strings.ContainsAny(name, "\r\n") {
		return errors.New("header name is empty or contains CRLF")
	}
	for _, r := range name {
		// RFC 9110 field-name is a token.  Keep this local rather than relying
		// on net/http's internal httpguts package.
		if !isTokenRune(r) {
			return fmt.Errorf("invalid header name %q", name)
		}
	}
	return nil
}

func validateHeaderValue(value string) error {
	for _, r := range value {
		// HTAB is permitted by the HTTP field-value grammar; all other C0
		// controls and DEL are rejected to prevent parser differentials.
		if (r < 0x20 && r != '\t') || r == 0x7f {
			return errors.New("header value contains control character")
		}
	}
	return nil
}

func isTokenRune(r rune) bool {
	if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
		return true
	}
	switch r {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	default:
		return false
	}
}

// parseRequestPath validates the path components supplied by the inbound
// envelope.  escapedPath is retained when it carries a valid non-canonical
// escape (for example %2F), while path remains the decoded matching value.
func parseRequestPath(path, escapedPath string) (string, string, error) {
	if path == "" {
		return "", "", errors.New("profile: request path is required")
	}
	if escapedPath == "" {
		escapedPath = (&url.URL{Path: path}).EscapedPath()
	}
	if err := validatePath(escapedPath, false); err != nil {
		return "", "", fmt.Errorf("profile: request escaped path: %w", err)
	}
	decoded, err := url.PathUnescape(escapedPath)
	if err != nil {
		return "", "", fmt.Errorf("profile: request escaped path: %w", err)
	}
	if decoded != path {
		return "", "", errors.New("profile: request path and escaped path disagree")
	}
	return decoded, escapedPath, nil
}

// ResolvePath applies the mapping without returning a value that can be used
// as a URL authority.  Unknown paths are preserved exactly in escaped form.
func (p Profile) ResolvePath(path, escapedPath string) (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	decoded, escaped, err := parseRequestPath(path, escapedPath)
	if err != nil {
		return "", err
	}
	m := p.Paths.withDefaults()
	switch decoded {
	case DefaultResponsesPath:
		return m.Responses, nil
	case DefaultChatCompletionsPath:
		return m.ChatCompletions, nil
	case DefaultAnthropicMessagesPath:
		return m.AnthropicMessages, nil
	case DefaultModelsPath:
		return m.Models, nil
	default:
		return escaped, nil
	}
}

// MapPath is a compatibility-friendly alias for ResolvePath.  If escapedPath
// is omitted, path is treated as decoded and escaped with net/url rules.
func (p Profile) MapPath(path string, escapedPath ...string) (string, error) {
	if len(escapedPath) > 1 {
		return "", errors.New("profile: MapPath accepts at most one escaped path")
	}
	escaped := ""
	if len(escapedPath) == 1 {
		escaped = escapedPath[0]
	}
	return p.ResolvePath(path, escaped)
}

// URLFor builds the validated upstream URL for separated path/query values.
// rawQuery is assigned verbatim, preserving ordering and percent encoding.
func (p Profile) URLFor(path, escapedPath, rawQuery string) (*url.URL, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if strings.ContainsAny(rawQuery, "\r\n\x00") {
		return nil, errors.New("profile: request query contains forbidden control character")
	}
	decoded, escaped, err := parseRequestPath(path, escapedPath)
	if err != nil {
		return nil, err
	}
	mapped, err := p.ResolvePath(decoded, escaped)
	if err != nil {
		return nil, err
	}
	if err := validateMappedPath(mapped); err != nil {
		return nil, fmt.Errorf("profile: mapped path: %w", err)
	}
	origin, err := parseOrigin(p.Origin)
	if err != nil {
		return nil, err
	}
	mappedDecoded, err := url.PathUnescape(mapped)
	if err != nil {
		return nil, fmt.Errorf("profile: mapped path: %w", err)
	}
	basePath := origin.Path
	baseEscaped := origin.EscapedPath()
	if basePath == "/" {
		basePath = ""
		baseEscaped = ""
	}
	joinedPath := strings.TrimRight(basePath, "/") + mappedDecoded
	if joinedPath == "" {
		joinedPath = "/"
	}
	joinedEscaped := strings.TrimRight(baseEscaped, "/") + mapped
	if joinedEscaped == "" {
		joinedEscaped = "/"
	}
	out := *origin
	out.Path = joinedPath
	out.RawPath = ""
	// Retain exact percent-escape spelling when it is a valid encoding of
	// Path.  url.URL.String then emits the encoded path without normalization.
	if (&url.URL{Path: joinedPath, RawPath: joinedEscaped}).EscapedPath() == joinedEscaped {
		out.RawPath = joinedEscaped
	}
	out.RawQuery = rawQuery
	out.Fragment = ""
	return &out, nil
}

// BuildURL is a convenience for callers that have a decoded path and no
// separately captured RawPath.  The original query bytes remain unchanged.
func (p Profile) BuildURL(path, rawQuery string) (*url.URL, error) {
	if err := validatePath(path, false); err != nil {
		return nil, err
	}
	return p.URLFor(path, path, rawQuery)
}

// URLForRequest builds a URL from an inbound request without reading its body.
// It preserves URL.Path, EscapedPath, and RawQuery as separate values.
func (p Profile) URLForRequest(req *http.Request) (*url.URL, error) {
	if req == nil || req.URL == nil {
		return nil, errors.New("profile: nil request URL")
	}
	escaped := req.URL.EscapedPath()
	if req.URL.RawPath != "" {
		if _, _, err := parseRequestPath(req.URL.Path, req.URL.RawPath); err != nil {
			return nil, err
		}
		escaped = req.URL.RawPath
	}
	return p.URLFor(req.URL.Path, escaped, req.URL.RawQuery)
}

// OriginURL returns a validated origin copy with no credential/user info.
func (p Profile) OriginURL() (*url.URL, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	u, err := parseOrigin(p.Origin)
	if err != nil {
		return nil, err
	}
	return cloneURL(u), nil
}

// PathPreview returns the URL shown to an operator before sending.  It never
// resolves or includes credentials.
func (p Profile) PathPreview(path, escapedPath, rawQuery string) (string, error) {
	u, err := p.URLFor(path, escapedPath, rawQuery)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func cloneURL(u *url.URL) *url.URL {
	if u == nil {
		return nil
	}
	copy := *u
	return &copy
}

func cloneHeaders(in http.Header) http.Header {
	if in == nil {
		return nil
	}
	out := make(http.Header, len(in))
	for name, values := range in {
		if values == nil {
			out[name] = nil
		} else {
			out[name] = append([]string(nil), values...)
		}
	}
	return out
}

// BasicCredential encodes the raw store value as username:password.  It is
// exported for tests and custom stores; the returned string is sensitive and
// must only be used as an HTTP header value.
func BasicCredential(raw string) string {
	return base64.StdEncoding.EncodeToString([]byte(raw))
}

// NewHTTPClient builds a client that never follows redirects, preserves
// compressed response bytes, and rejects non-loopback dial targets (including
// DNS answers for localhost).  It is safe to use in local httptest tests.
func NewHTTPClient(p Profile) (*http.Client, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("profile: default transport is not *http.Transport")
	}
	transport := base.Clone()
	transport.DisableCompression = true
	transport.DialContext = loopbackDialContext(transport.DialContext)
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			// Return the response that carried Location rather than making a
			// second request to an unvalidated target.
			return http.ErrUseLastResponse
		},
	}
	if p.Network.Timeout > 0 {
		client.Timeout = p.Network.Timeout
	}
	return client, nil
}

// HTTPClient is a method form of NewHTTPClient.
func (p Profile) HTTPClient() (*http.Client, error) { return NewHTTPClient(p) }

func loopbackDialContext(base func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	if base == nil {
		base = (&net.Dialer{}).DialContext
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("profile: invalid dial address")
		}
		if ip := net.ParseIP(host); ip != nil {
			if !ip.IsLoopback() {
				return nil, errors.New("profile: blocked non-loopback dial target")
			}
			return base(ctx, network, net.JoinHostPort(host, port))
		}
		if !strings.EqualFold(host, "localhost") {
			return nil, errors.New("profile: blocked non-loopback dial hostname")
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, errors.New("profile: localhost resolution failed")
		}
		for _, candidate := range ips {
			if !candidate.IP.IsLoopback() {
				continue
			}
			conn, dialErr := base(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
			if dialErr == nil {
				return conn, nil
			}
		}
		return nil, errors.New("profile: localhost resolved without a loopback address")
	}
}
