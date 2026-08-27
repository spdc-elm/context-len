package inspection

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Protocol is the protocol family suggested by an inspection.  A hint is an
// observation only; it must never be used to rewrite a request or response.
type Protocol string

const (
	ProtocolUnknown           Protocol = "unknown"
	ProtocolGenericJSON       Protocol = "generic_json"
	ProtocolResponses         Protocol = "responses"
	ProtocolChatCompletions   Protocol = "chat_completions"
	ProtocolAnthropicMessages Protocol = "anthropic_messages"
	// ProtocolAnthropic is kept as a readable shorthand for callers that do
	// not need to distinguish Anthropic's Messages endpoint from its provider.
	ProtocolAnthropic Protocol = ProtocolAnthropicMessages
)

// BodyFormat is the wire body format observed by the inspector.
type BodyFormat string

const (
	FormatUnknown BodyFormat = "unknown"
	FormatJSON    BodyFormat = "json"
	FormatSSE     BodyFormat = "sse"
)

// ParseStatus describes how much of a projection could be built.  A parser
// warning is intentionally non-blocking: a caller may still forward the
// untouched artifact.
type ParseStatus string

const (
	ParseOK      ParseStatus = "ok"
	ParsePartial ParseStatus = "partial"
	ParseInvalid ParseStatus = "invalid"
)

// ByteSpan is a half-open byte range in Source.  Offsets are byte offsets,
// rather than rune offsets, so a span can be used to locate the exact wire
// fragment even when the body contains multibyte UTF-8 text.
type ByteSpan struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

func (s ByteSpan) validFor(n int) bool {
	return s.Start >= 0 && s.End >= s.Start && s.End <= n
}

// Warning records a parser/inspection issue.  Warnings are projections and do
// not alter the source bytes or stop bypass forwarding.
type Warning struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Pointer string   `json:"pointer,omitempty"`
	Span    ByteSpan `json:"span"`
	Fatal   bool     `json:"fatal,omitempty"`
}

func warning(code, message, pointer string, span ByteSpan, fatal bool) Warning {
	return Warning{Code: code, Message: message, Pointer: pointer, Span: span, Fatal: fatal}
}

// UnknownNode retains a field/event/node that a higher-level protocol
// projection did not recognise.  Raw is a copy of the exact source fragment;
// callers may safely annotate or mutate it without touching the artifact.
type UnknownNode struct {
	Pointer string   `json:"pointer,omitempty"`
	Kind    string   `json:"kind,omitempty"`
	Raw     []byte   `json:"raw,omitempty"`
	Span    ByteSpan `json:"span"`
	Reason  string   `json:"reason,omitempty"`
}

// SourceHash returns the SHA-256 of exact application-layer body bytes.  It is
// useful for projection diagnostics, but does not imply that projection bytes
// are valid transport input.
func SourceHash(source []byte) string {
	sum := sha256.Sum256(source)
	return hex.EncodeToString(sum[:])
}

func cloneBytes(source []byte) []byte {
	if source == nil {
		return nil
	}
	clone := make([]byte, len(source))
	copy(clone, source)
	return clone
}

// JSONInspection is a generic, loss-aware JSON projection.  Source is a
// private copy of the body passed to InspectJSON.  Root/fields/array items
// carry Raw and Span, allowing UI code to inspect without serialising a map
// back into the request.
type JSONInspection struct {
	Format       BodyFormat    `json:"format"`
	Status       ParseStatus   `json:"status"`
	Valid        bool          `json:"valid"`
	Complete     bool          `json:"complete"`
	Source       []byte        `json:"-"`
	SourceHash   string        `json:"source_hash"`
	Root         *JSONNode     `json:"root,omitempty"`
	UnknownNodes []UnknownNode `json:"unknown_nodes,omitempty"`
	Warnings     []Warning     `json:"warnings,omitempty"`
}

// JSONKind identifies the JSON value represented by a node.
type JSONKind string

const (
	JSONInvalid JSONKind = "invalid"
	JSONNull    JSONKind = "null"
	JSONObject  JSONKind = "object"
	JSONArray   JSONKind = "array"
	JSONString  JSONKind = "string"
	JSONNumber  JSONKind = "number"
	JSONBoolean JSONKind = "boolean"
)

// JSONNode is a loss-aware generic JSON value.  Raw is always the exact value
// token from Source (including object/array descendants and their formatting).
// Object fields and array items retain order and duplicate keys.
type JSONNode struct {
	Kind    JSONKind    `json:"kind"`
	Pointer string      `json:"pointer"`
	Span    ByteSpan    `json:"span"`
	Raw     []byte      `json:"raw"`
	Value   any         `json:"value,omitempty"`
	Fields  []JSONField `json:"fields,omitempty"`
	Items   []*JSONNode `json:"items,omitempty"`
}

// JSONField is an object member.  Key is decoded for display, while RawKey
// preserves its exact JSON spelling (including escapes).
type JSONField struct {
	Key     string    `json:"key"`
	RawKey  []byte    `json:"raw_key"`
	Pointer string    `json:"pointer"`
	Span    ByteSpan  `json:"span"`
	Raw     []byte    `json:"raw"`
	Value   *JSONNode `json:"value,omitempty"`
	Unknown bool      `json:"unknown,omitempty"`
}

// Field returns the first object field with key.  Duplicate fields remain
// available in Fields; this helper is intentionally conservative.
func (n *JSONNode) Field(key string) (*JSONField, bool) {
	if n == nil || n.Kind != JSONObject {
		return nil, false
	}
	for i := range n.Fields {
		if n.Fields[i].Key == key {
			return &n.Fields[i], true
		}
	}
	return nil, false
}

// Lookup follows an RFC 6901 JSON pointer over the projection.  It returns the
// node and false for an absent/invalid pointer.  Duplicate object keys resolve
// to their first occurrence, matching Field.
func (n *JSONNode) Lookup(pointer string) (*JSONNode, bool) {
	if n == nil {
		return nil, false
	}
	if pointer == "" {
		return n, true
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, false
	}
	parts := strings.Split(pointer[1:], "/")
	cur := n
	for _, encoded := range parts {
		part := strings.NewReplacer("~1", "/", "~0", "~").Replace(encoded)
		switch cur.Kind {
		case JSONObject:
			field, ok := cur.Field(part)
			if !ok || field.Value == nil {
				return nil, false
			}
			cur = field.Value
		case JSONArray:
			if part == "" || (len(part) > 1 && part[0] == '0') {
				return nil, false
			}
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(cur.Items) {
				return nil, false
			}
			cur = cur.Items[index]
		default:
			return nil, false
		}
	}
	return cur, true
}

// SSEInspection is a projection of an event-stream body.  Source is a private
// copy.  Each Event.Raw includes all of that event's original lines and its
// terminating blank-line bytes when present; an incomplete final event has no
// synthetic terminator.
type SSEInspection struct {
	Format       BodyFormat    `json:"format"`
	Status       ParseStatus   `json:"status"`
	Valid        bool          `json:"valid"`
	Complete     bool          `json:"complete"`
	Source       []byte        `json:"-"`
	SourceHash   string        `json:"source_hash"`
	Events       []SSEEvent    `json:"events,omitempty"`
	Comments     []SSEComment  `json:"comments,omitempty"`
	UnknownNodes []UnknownNode `json:"unknown_nodes,omitempty"`
	Warnings     []Warning     `json:"warnings,omitempty"`
}

// SSEEvent is one dispatched SSE event (or one incomplete event at EOF).
type SSEEvent struct {
	Index     int             `json:"index"`
	Name      string          `json:"name,omitempty"`
	ID        string          `json:"id,omitempty"`
	Retry     *int            `json:"retry,omitempty"`
	Data      string          `json:"data,omitempty"`
	DataLines []string        `json:"data_lines,omitempty"`
	Fields    []SSEField      `json:"fields,omitempty"`
	Unknown   []SSEField      `json:"unknown_fields,omitempty"`
	Raw       []byte          `json:"raw"`
	Span      ByteSpan        `json:"span"`
	Complete  bool            `json:"complete"`
	JSON      *JSONInspection `json:"json,omitempty"`
}

// SSEField retains one parsed line.  Raw includes the line ending exactly as
// received.  Name and Value are the SSE-decoded values (one optional leading
// space after ':' is removed from Value per the SSE algorithm).
type SSEField struct {
	Name  string   `json:"name"`
	Value string   `json:"value"`
	Raw   []byte   `json:"raw"`
	Span  ByteSpan `json:"span"`
}

// SSEComment retains comments, which have no dispatched event semantics but
// are still relevant when explaining a wire stream.
type SSEComment struct {
	Text string   `json:"text"`
	Raw  []byte   `json:"raw"`
	Span ByteSpan `json:"span"`
}

// HintConfidence communicates how directly a protocol hint was observed.
type HintConfidence string

const (
	ConfidenceNone   HintConfidence = "none"
	ConfidenceLow    HintConfidence = "low"
	ConfidenceMedium HintConfidence = "medium"
	ConfidenceHigh   HintConfidence = "high"
)

// HintInput contains non-mutating context for protocol detection.  Headers are
// accepted as http.Header so callers can pass an HTTP request directly.  Body
// is copied by the detector only when it needs to parse it.
type HintInput struct {
	Method      string
	Path        string
	ContentType string
	Headers     http.Header
	Body        []byte
}

// ProtocolHint is an observation with human-readable evidence.  A hint may be
// Unknown or GenericJSON; neither outcome should block transparent forwarding.
type ProtocolHint struct {
	Protocol   Protocol       `json:"protocol"`
	Format     BodyFormat     `json:"format"`
	Confidence HintConfidence `json:"confidence"`
	Evidence   []string       `json:"evidence,omitempty"`
	Warnings   []Warning      `json:"warnings,omitempty"`
}

// Inspection is a convenience union for callers that only know an artifact's
// bytes and metadata.  Exactly one of JSON or SSE is populated when Format is
// recognised.  Hint, JSON, and SSE are all projections; Source remains the
// caller's immutable artifact.
type Inspection struct {
	Hint ProtocolHint    `json:"hint"`
	JSON *JSONInspection `json:"json,omitempty"`
	SSE  *SSEInspection  `json:"sse,omitempty"`
	// Protocol is populated when the hint identifies one of the supported
	// provider protocols. It is an additive semantic projection; JSON and SSE
	// continue to expose the generic loss-aware trees above.
	Protocol *ProtocolProjection `json:"protocol,omitempty"`
}

// HeaderValue performs case-insensitive lookup without exposing credentials.
// It is intentionally small and local so hinting cannot accidentally log a
// header value.
func HeaderValue(headers http.Header, name string) string {
	if headers == nil {
		return ""
	}
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) != 0 {
			return values[0]
		}
	}
	return ""
}

// bodyLooksLikeSSE is deliberately conservative: a single data/event field at
// a line boundary is enough to classify a body as SSE, but arbitrary text that
// merely contains "data:" is not.
func bodyLooksLikeSSE(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	for _, line := range bytes.Split(body, []byte{'\n'}) {
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if bytes.HasPrefix(line, []byte("data:")) || bytes.HasPrefix(line, []byte("event:")) || bytes.HasPrefix(line, []byte("id:")) || bytes.HasPrefix(line, []byte("retry:")) {
			return true
		}
	}
	return false
}

// normalizedPath strips a query, removes a trailing slash, and unescapes path
// segments for matching.  It does not return the normalized path to transport.
func normalizedPath(raw string) string {
	path := raw
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	if parsed, err := url.Parse(path); err == nil && parsed.Path != "" {
		path = parsed.Path
	}
	if unescaped, err := url.PathUnescape(path); err == nil {
		path = unescaped
	}
	path = strings.TrimRight(path, "/")
	return path
}
