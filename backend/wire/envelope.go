package wire

import (
	"net/http"
	"net/url"
	"strings"
	"time"
)

// RequestEnvelope is the application-level request metadata captured around a
// request artifact.  Path and EscapedPath intentionally coexist: Path is the
// URL path value while EscapedPath and RawQuery retain the encoded path/query
// representation needed for fidelity diagnostics.
//
// Headers are copied by constructors and by Clone.  Callers should treat the
// returned envelope as a value and avoid mutating its exported map directly;
// use HeadersClone when a mutable HTTP header map is needed.
type RequestEnvelope struct {
	Method      string      `json:"method"`
	Path        string      `json:"path"`
	EscapedPath string      `json:"escaped_path"`
	RawQuery    string      `json:"raw_query"`
	Headers     http.Header `json:"headers"`
}

// ResponseEnvelope is the application-level response metadata captured around
// a response artifact.  Status and headers are preserved independently from
// body bytes, and trailers are retained when the HTTP stack exposes them.
type ResponseEnvelope struct {
	Status    int         `json:"status"`
	Headers   http.Header `json:"headers"`
	Trailers  http.Header `json:"trailers"`
	StartedAt time.Time   `json:"started_at"`
	EndedAt   time.Time   `json:"ended_at"`
}

// NewRequestEnvelope constructs a request envelope from already-separated URL
// components.  Header values are deep-copied so subsequent caller mutation
// does not alter the captured metadata.
func NewRequestEnvelope(method, path, escapedPath, rawQuery string, headers http.Header) RequestEnvelope {
	if escapedPath == "" && path != "" {
		// This fallback is only for callers that do not have an escaped form.
		// RequestFromHTTP always uses URL.EscapedPath and therefore preserves a
		// valid RawPath supplied by net/http.
		escapedPath = (&url.URL{Path: path}).EscapedPath()
	}
	return RequestEnvelope{
		Method:      method,
		Path:        path,
		EscapedPath: escapedPath,
		RawQuery:    rawQuery,
		Headers:     CloneHeaders(headers),
	}
}

// RequestFromHTTP captures the relevant metadata from req without reading or
// interpreting its body.  A nil request is represented by a zero envelope.
func RequestFromHTTP(req *http.Request) RequestEnvelope {
	if req == nil {
		return RequestEnvelope{}
	}
	var path, escapedPath, rawQuery string
	if req.URL != nil {
		path = req.URL.Path
		escapedPath = req.URL.EscapedPath()
		rawQuery = req.URL.RawQuery
	}
	return NewRequestEnvelope(req.Method, path, escapedPath, rawQuery, req.Header)
}

// Clone returns an independent value copy, including a deep copy of headers.
func (e RequestEnvelope) Clone() RequestEnvelope {
	e.Headers = CloneHeaders(e.Headers)
	return e
}

// HeadersClone returns a mutable copy of the envelope headers.
func (e RequestEnvelope) HeadersClone() http.Header { return CloneHeaders(e.Headers) }

// Redacted returns an independent envelope with sensitive header values
// replaced by RedactedHeaderValue.  The original envelope remains unchanged.
func (e RequestEnvelope) Redacted() RequestEnvelope {
	e.Headers = RedactHeaders(e.Headers)
	e.RawQuery = redactRawQuery(e.RawQuery)
	return e
}

func redactRawQuery(raw string) string {
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, "&")
	for i, part := range parts {
		key, _, found := strings.Cut(part, "=")
		decoded, err := url.QueryUnescape(key)
		if err != nil {
			decoded = key
		}
		lower := strings.ToLower(decoded)
		sensitive := false
		for _, marker := range []string{"key", "token", "secret", "password", "credential", "signature", "authorization"} {
			if strings.Contains(lower, marker) {
				sensitive = true
				break
			}
		}
		if sensitive && found {
			parts[i] = key + "=" + url.QueryEscape(RedactedHeaderValue)
		}
	}
	return strings.Join(parts, "&")
}

// NewResponseEnvelope constructs a response envelope from status, headers,
// trailers, and capture timestamps.  Header maps are deep-copied.
func NewResponseEnvelope(status int, headers, trailers http.Header, startedAt, endedAt time.Time) ResponseEnvelope {
	return ResponseEnvelope{
		Status:    status,
		Headers:   CloneHeaders(headers),
		Trailers:  CloneHeaders(trailers),
		StartedAt: startedAt,
		EndedAt:   endedAt,
	}
}

// ResponseFromHTTP captures response metadata without reading its body.  The
// caller supplies timestamps because the response may have started before the
// HTTP client returned it and ended only after body capture completes.
func ResponseFromHTTP(resp *http.Response, startedAt, endedAt time.Time) ResponseEnvelope {
	if resp == nil {
		return ResponseEnvelope{StartedAt: startedAt, EndedAt: endedAt}
	}
	return NewResponseEnvelope(resp.StatusCode, resp.Header, resp.Trailer, startedAt, endedAt)
}

// Clone returns an independent value copy, including deep copies of headers
// and trailers.
func (e ResponseEnvelope) Clone() ResponseEnvelope {
	e.Headers = CloneHeaders(e.Headers)
	e.Trailers = CloneHeaders(e.Trailers)
	return e
}

// HeadersClone returns a mutable copy of response headers.
func (e ResponseEnvelope) HeadersClone() http.Header { return CloneHeaders(e.Headers) }

// TrailersClone returns a mutable copy of response trailers.
func (e ResponseEnvelope) TrailersClone() http.Header { return CloneHeaders(e.Trailers) }

// Redacted returns an independent response envelope with sensitive header
// values replaced.  Trailers are redacted too because they may carry tokens.
func (e ResponseEnvelope) Redacted() ResponseEnvelope {
	e.Headers = RedactHeaders(e.Headers)
	e.Trailers = RedactHeaders(e.Trailers)
	return e
}

// HTTPRequest and HTTPResponse are descriptive aliases for callers that want
// to make the HTTP nature of envelopes explicit in a composite DTO.
type HTTPRequest = RequestEnvelope
type HTTPResponse = ResponseEnvelope
