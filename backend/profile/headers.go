package profile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"context-lens/backend/config"
)

// HeaderAction identifies an intentional proxy header change.  Values are
// deliberately absent from this type: header diffs can be sent to a browser
// or log without accidentally disclosing a credential.
type HeaderAction string

const (
	HeaderAdded    HeaderAction = "added"
	HeaderRemoved  HeaderAction = "removed"
	HeaderReplaced HeaderAction = "replaced"
)

// HeaderChange describes one safe-to-display policy change.  Sensitive values
// are never included; use the outbound Header map only inside transport code.
type HeaderChange struct {
	Name      string       `json:"name"`
	Action    HeaderAction `json:"action"`
	Reason    string       `json:"reason"`
	Sensitive bool         `json:"sensitive,omitempty"`
}

// HeaderDiff is a secret-free audit of profile header policy.  It records
// names and reasons, not before/after values.
type HeaderDiff struct {
	Changes []HeaderChange `json:"changes,omitempty"`
}

// HeaderPolicyResult carries headers for transport and a separate, safe diff
// for workspace diagnostics.  Headers is excluded from JSON so marshaling a
// result cannot leak injected credentials.
type HeaderPolicyResult struct {
	Headers       http.Header `json:"-"`
	Diff          HeaderDiff  `json:"diff,omitempty"`
	CredentialRef string      `json:"credential_ref,omitempty"`
	CredentialSet bool        `json:"credential_configured"`
}

// MarshalJSON emits only secret-free metadata.  The outbound Headers map must
// stay in process and should be handed directly to http.NewRequest.
func (r HeaderPolicyResult) MarshalJSON() ([]byte, error) {
	type safeResult struct {
		Diff          HeaderDiff `json:"diff,omitempty"`
		CredentialRef string     `json:"credential_ref,omitempty"`
		CredentialSet bool       `json:"credential_configured"`
	}
	return marshalSafeResult(safeResult{Diff: r.Diff, CredentialRef: r.CredentialRef, CredentialSet: r.CredentialSet})
}

// marshalSafeResult is isolated to keep encoding/json out of the hot header
// path's imports and make the omitted Headers contract obvious in review.
func marshalSafeResult(v any) ([]byte, error) {
	// A tiny local alias avoids exposing HeaderPolicyResult.Headers through a
	// future addition to its fields.
	return json.Marshal(v)
}

func sortedHeaderNames(headers http.Header) []string {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		left, right := strings.ToLower(names[i]), strings.ToLower(names[j])
		if left == right {
			return names[i] < names[j]
		}
		return left < right
	})
	return names
}

// ApplyHeaders filters inbound headers, applies configured additional safe
// headers, and injects one server-side credential.  It does not inspect or
// mutate a request body.  The returned Headers map is transport-only; Diff is
// safe to publish to UI/event consumers.
func (p Profile) ApplyHeaders(ctx context.Context, inbound http.Header, store config.CredentialStore) (HeaderPolicyResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := p.Validate(); err != nil {
		return HeaderPolicyResult{}, err
	}
	if err := validateHeaderInput(inbound); err != nil {
		return HeaderPolicyResult{}, err
	}
	out := make(http.Header)
	var diff HeaderDiff
	for _, name := range sortedHeaderNames(inbound) {
		values := inbound[name]
		if isHopByHopHeader(name) {
			diff.Changes = append(diff.Changes, HeaderChange{Name: http.CanonicalHeaderKey(name), Action: HeaderRemoved, Reason: "hop_by_hop", Sensitive: isInboundCredentialHeader(name)})
			continue
		}
		if isInboundCredentialHeader(name) {
			diff.Changes = append(diff.Changes, HeaderChange{Name: http.CanonicalHeaderKey(name), Action: HeaderRemoved, Reason: "inbound_credential", Sensitive: true})
			continue
		}
		if strings.EqualFold(name, "host") || strings.EqualFold(name, "content-length") {
			diff.Changes = append(diff.Changes, HeaderChange{Name: http.CanonicalHeaderKey(name), Action: HeaderRemoved, Reason: "connection_owned"})
			continue
		}
		canonical := http.CanonicalHeaderKey(name)
		out[canonical] = append(out[canonical], values...)
	}

	// Additional headers are profile-owned and override an inbound value with
	// the same name.  This makes provider protocol headers deterministic while
	// still surfacing the replacement in a safe diff.
	for _, name := range sortedHeaderNames(p.AdditionalHeaders) {
		values := p.AdditionalHeaders[name]
		canonical := http.CanonicalHeaderKey(name)
		if _, existed := out[canonical]; existed {
			diff.Changes = append(diff.Changes, HeaderChange{Name: canonical, Action: HeaderReplaced, Reason: "profile_additional"})
		} else {
			diff.Changes = append(diff.Changes, HeaderChange{Name: canonical, Action: HeaderAdded, Reason: "profile_additional"})
		}
		deleteHeaderFold(out, canonical)
		out[canonical] = append([]string(nil), values...)
	}

	credential := p.Credential
	if credential.Scheme == "" {
		credential.Scheme = CredentialNone
	}
	result := HeaderPolicyResult{Headers: out, Diff: diff, CredentialRef: credential.Reference, CredentialSet: credential.Reference != ""}
	if credential.Scheme == CredentialNone {
		return result, nil
	}
	if store == nil {
		return HeaderPolicyResult{}, errors.New("profile: credential store is required")
	}
	secret, err := store.Resolve(ctx, credential.Reference)
	if err != nil {
		// Do not wrap arbitrary store errors: a misbehaving store might put
		// the secret in its error text.  Preserve only context/sentinel
		// identity whose messages are known not to contain credential values.
		if errors.Is(err, context.Canceled) {
			return HeaderPolicyResult{}, context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return HeaderPolicyResult{}, context.DeadlineExceeded
		}
		if errors.Is(err, config.ErrCredentialNotFound) {
			return HeaderPolicyResult{}, fmt.Errorf("profile: credential %q is not configured: %w", credential.Reference, config.ErrCredentialNotFound)
		}
		return HeaderPolicyResult{}, errors.New("profile: credential lookup failed")
	}
	if secret == "" {
		return HeaderPolicyResult{}, errors.New("profile: resolved credential is empty")
	}
	if err := validateHeaderValue(secret); err != nil {
		return HeaderPolicyResult{}, errors.New("profile: resolved credential contains forbidden header character")
	}
	var header, value string
	switch credential.Scheme {
	case CredentialBearer:
		header, value = "Authorization", "Bearer "+secret
	case CredentialAPIKey, CredentialScheme("api-key"), CredentialScheme("apikey"), CredentialScheme("x-api-key"):
		header = credential.Header
		if header == "" {
			header = "X-API-Key"
		}
		value = secret
	case CredentialBasic:
		header, value = "Authorization", "Basic "+BasicCredential(secret)
	case CredentialHeader:
		header, value = credential.Header, secret
	default:
		return HeaderPolicyResult{}, fmt.Errorf("profile: unsupported credential scheme %q", credential.Scheme)
	}
	canonical := http.CanonicalHeaderKey(header)
	if err := validateCredentialHeaderName(canonical); err != nil {
		return HeaderPolicyResult{}, err
	}
	if _, existed := out[canonical]; existed || headerExistsFold(out, canonical) {
		diff.Changes = append(diff.Changes, HeaderChange{Name: canonical, Action: HeaderReplaced, Reason: "profile_credential", Sensitive: true})
	} else {
		diff.Changes = append(diff.Changes, HeaderChange{Name: canonical, Action: HeaderAdded, Reason: "profile_credential", Sensitive: true})
	}
	deleteHeaderFold(out, canonical)
	out[canonical] = []string{value}
	result.Headers = out
	result.Diff = diff
	return result, nil
}

// OutboundHeaders is a convenience for callers that do not need a diff.  The
// returned map still contains a credential and must remain in process.
func (p Profile) OutboundHeaders(ctx context.Context, inbound http.Header, store config.CredentialStore) (http.Header, error) {
	result, err := p.ApplyHeaders(ctx, inbound, store)
	if err != nil {
		return nil, err
	}
	return result.Headers, nil
}

// InjectHeaders is an alias for ApplyHeaders.  It makes the secret boundary
// explicit at call sites that construct an outbound request.
func (p Profile) InjectHeaders(ctx context.Context, inbound http.Header, store config.CredentialStore) (HeaderPolicyResult, error) {
	return p.ApplyHeaders(ctx, inbound, store)
}

// FilterHeaders applies only the inbound-safe filtering policy.  It is useful
// for /v1/models and for transports that inject auth through another seam.
func FilterHeaders(inbound http.Header) (http.Header, HeaderDiff, error) {
	p := Profile{ID: "header-filter", Origin: "http://127.0.0.1"}
	result, err := p.ApplyHeaders(context.Background(), inbound, nil)
	if err != nil {
		return nil, HeaderDiff{}, err
	}
	return result.Headers, result.Diff, nil
}

func validateHeaderInput(headers http.Header) error {
	for name, values := range headers {
		if err := validateHeaderName(name); err != nil {
			return fmt.Errorf("profile: inbound header: %w", err)
		}
		for _, value := range values {
			if err := validateHeaderValue(value); err != nil {
				return fmt.Errorf("profile: inbound header %q: %w", name, err)
			}
		}
	}
	return nil
}

func headerExistsFold(headers http.Header, name string) bool {
	for key := range headers {
		if strings.EqualFold(key, name) {
			return true
		}
	}
	return false
}

func deleteHeaderFold(headers http.Header, name string) {
	for key := range headers {
		if strings.EqualFold(key, name) {
			delete(headers, key)
		}
	}
}

// isInboundCredentialHeader prevents credentials supplied by the harness from
// crossing the trust boundary.  The profile's own credential is injected
// after this filter and may intentionally use Authorization or X-API-Key.
func isInboundCredentialHeader(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "authorization" || name == "proxy-authorization" || name == "www-authenticate" || name == "x-api-key" || name == "api-key" || name == "apikey" || name == "x-apikey" || name == "cookie" || name == "set-cookie" {
		return true
	}
	for _, marker := range []string{"authorization", "api-key", "apikey", "client-secret", "access-token", "refresh-token", "password", "credential"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

func isHopByHopHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "connection", "proxy-connection", "keep-alive", "transfer-encoding", "te", "trailer", "upgrade", "proxy-authenticate", "proxy-authorization", "expect":
		return true
	default:
		return false
	}
}

// String intentionally omits Headers, whose map may contain an injected
// credential.  This keeps accidental fmt/logging of a policy result safe.
func (r HeaderPolicyResult) String() string {
	return fmt.Sprintf("HeaderPolicyResult{changes:%d credential_configured:%t}", len(r.Diff.Changes), r.CredentialSet)
}

// GoString keeps %#v diagnostics secret-free as well.
func (r HeaderPolicyResult) GoString() string { return r.String() }
