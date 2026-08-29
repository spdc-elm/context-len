package wire

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// RedactedHeaderValue is the only value emitted for a header classified as
// sensitive.  It intentionally contains no hint of the original credential.
const RedactedHeaderValue = "[REDACTED]"

// CloneHeaders makes a deep copy of h.  Header field names are normalized to
// net/http's canonical spelling because HTTP names are case-insensitive and
// Header.Get/Set expect canonical map keys.  Case-colliding entries are
// retained in map iteration order and their values are appended in encounter
// order.  Keeping this helper local makes the copy contract explicit and
// keeps nil maps nil.
func CloneHeaders(h http.Header) http.Header {
	if h == nil {
		return nil
	}
	clone := make(http.Header, len(h))
	for name, values := range h {
		canonical := http.CanonicalHeaderKey(name)
		if canonical == "" {
			// Preserve an invalid name for diagnostics; ValidateHeaders can be
			// called before a value crosses an actual HTTP boundary.
			canonical = name
		}
		if values == nil {
			if _, exists := clone[canonical]; !exists {
				clone[canonical] = nil
			}
			continue
		}
		clone[canonical] = append(clone[canonical], values...)
	}
	return clone
}

// IsSensitiveHeader reports whether a header should be hidden from workspace
// projections.  The policy covers the credentials explicitly called out by
// the wire contract and common equivalent spellings.  Matching is
// case-insensitive and does not mutate the supplied name.
func IsSensitiveHeader(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	switch name {
	case "authorization", "proxy-authorization", "www-authenticate",
		"x-api-key", "api-key", "apikey", "x-apikey", "x-context-lens-key",
		"cookie", "set-cookie":
		return true
	}
	// Custom provider headers commonly carry secrets under one of these names.
	// Keep this conservative enough not to hide ordinary tracing or protocol
	// headers while preventing accidental credential exposure.
	for _, marker := range []string{"authorization", "api-key", "apikey", "auth-token", "client-secret", "access-token", "refresh-token", "session-token", "secret", "password", "credential"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

// RedactHeaders deep-copies h and replaces every value of sensitive headers
// with RedactedHeaderValue.  It never mutates the caller's map or slices and
// preserves non-sensitive values exactly, including multiple header values.
func RedactHeaders(h http.Header) http.Header {
	return RedactHeadersWith(h, IsSensitiveHeader)
}

// RedactHeadersWith is the policy-injectable form of RedactHeaders.  A nil
// predicate means IsSensitiveHeader.  Header names are passed exactly as they
// occur in h, while callers' predicate matching can be case-insensitive.
func RedactHeadersWith(h http.Header, sensitive func(name string) bool) http.Header {
	if h == nil {
		return nil
	}
	if sensitive == nil {
		sensitive = IsSensitiveHeader
	}
	redacted := make(http.Header, len(h))
	for name, values := range h {
		canonical := http.CanonicalHeaderKey(name)
		if canonical == "" {
			canonical = name
		}
		if sensitive(name) {
			// Multiple case variants of one sensitive field still collapse to a
			// single sentinel rather than retaining any credential-bearing value.
			redacted[canonical] = []string{RedactedHeaderValue}
			continue
		}
		// A case-colliding entry may have been classified sensitive already;
		// never append a non-sensitive value to that sentinel.
		if valuesEqualRedacted(redacted[canonical]) {
			continue
		}
		if values == nil {
			if _, exists := redacted[canonical]; !exists {
				redacted[canonical] = nil
			}
			continue
		}
		redacted[canonical] = append(redacted[canonical], values...)
	}
	return redacted
}

func valuesEqualRedacted(values []string) bool {
	return len(values) == 1 && values[0] == RedactedHeaderValue
}

// ValidateHeaders rejects the injection characters that must never cross a
// proxy header boundary.  It intentionally accepts the same broad set of
// names net/http accepts at map-construction time; transport code can apply
// stricter policy if needed.  Values are checked independently, including
// empty and multi-value entries.
func ValidateHeaders(h http.Header) error {
	for name, values := range h {
		if name == "" {
			return errors.New("wire: empty header name")
		}
		if strings.ContainsAny(name, "\r\n") {
			return fmt.Errorf("wire: header name %q contains CRLF", name)
		}
		for _, value := range values {
			if strings.ContainsAny(value, "\r\n") {
				return fmt.Errorf("wire: header %q value contains CRLF", name)
			}
		}
	}
	return nil
}
