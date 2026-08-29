// Package auth contains the optional loopback proxy access gate.
package auth

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
)

// Config controls the optional client authentication gate for proxy routes.
// APIKey is a client access key, not the upstream credential. The latter is
// intentionally owned by the transport configuration and is never compared
// or exposed by this package.
type Config struct {
	Enabled bool
	APIKey  string
}

// Validate rejects an enabled gate without a usable key and rejects header
// injection characters. The key value is never included in an error.
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if strings.TrimSpace(c.APIKey) == "" {
		return errors.New("auth: client authentication requires a non-empty api key")
	}
	if strings.TrimSpace(c.APIKey) != c.APIKey {
		return errors.New("auth: client api key must not have surrounding whitespace")
	}
	if strings.ContainsAny(c.APIKey, "\r\n") {
		return errors.New("auth: client api key contains CRLF")
	}
	for _, r := range c.APIKey {
		if r < 0x20 || r == 0x7f {
			return errors.New("auth: client api key contains a control character")
		}
	}
	return nil
}

// Authorize checks one inbound request without logging or returning the key.
// Bearer Authorization, X-API-Key, and API-Key are accepted for compatibility
// with common OpenAI-compatible and Anthropic-compatible clients. Query
// parameters and cookies are deliberately not accepted.
func (c Config) Authorize(r *http.Request) bool {
	if !c.Enabled {
		return true
	}
	if r == nil {
		return false
	}
	candidates := []string{
		r.Header.Get("X-API-Key"),
		r.Header.Get("API-Key"),
	}
	if authorization := strings.TrimSpace(r.Header.Get("Authorization")); authorization != "" {
		const bearer = "Bearer "
		if strings.HasPrefix(strings.ToLower(authorization), strings.ToLower(bearer)) {
			candidates = append(candidates, strings.TrimSpace(authorization[len(bearer):]))
		}
	}
	for _, candidate := range candidates {
		if constantTimeEqual(candidate, c.APIKey) {
			return true
		}
	}
	return false
}

// Middleware protects a handler when enabled. The response intentionally
// contains no configuration detail, credential value, or upstream error.
func (c Config) Middleware(next http.Handler) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	if !c.Enabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !c.Authorize(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="context-lens"`)
			http.Error(w, "context-lens: authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func constantTimeEqual(left, right string) bool {
	leftBytes, rightBytes := []byte(left), []byte(right)
	if len(leftBytes) != len(rightBytes) {
		// Still perform work proportional to both values before returning, so a
		// length mismatch does not become an obvious fast-path oracle.
		max := len(leftBytes)
		if len(rightBytes) > max {
			max = len(rightBytes)
		}
		paddedLeft := make([]byte, max)
		paddedRight := make([]byte, max)
		copy(paddedLeft, leftBytes)
		copy(paddedRight, rightBytes)
		_ = subtle.ConstantTimeCompare(paddedLeft, paddedRight)
		return false
	}
	return subtle.ConstantTimeCompare(leftBytes, rightBytes) == 1
}
