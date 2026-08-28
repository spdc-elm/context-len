// Package exchange owns one HTTP exchange's lifecycle and the registry that
// exposes it to transport and workspace layers.  It intentionally deals in
// wire.BodyArtifact values rather than decoded JSON: inspection is a separate
// projection and never becomes the transport input by accident.
package exchange

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"context-lens/backend/policy"
	"context-lens/backend/wire"
)

// State is the externally-visible exchange lifecycle state.
type State string

const (
	StateReceived        State = "received"
	StateRequestHeld     State = "request_held"
	StateUpstreamRunning State = "upstream_running"
	StateResponseHeld    State = "response_held"
	StateCompleted       State = "completed"
	StateDropped         State = "dropped"
	StateCancelled       State = "cancelled"
	StateFailed          State = "failed"

	// Short aliases are kept for callers that prefer the names used by the
	// runtime contract prose.
	Received        State = StateReceived
	RequestHeld     State = StateRequestHeld
	UpstreamRunning State = StateUpstreamRunning
	ResponseHeld    State = StateResponseHeld
	Completed       State = StateCompleted
	Dropped         State = StateDropped
	Cancelled       State = StateCancelled
	Failed          State = StateFailed
)

func (s State) Terminal() bool {
	return s == StateCompleted || s == StateDropped || s == StateCancelled || s == StateFailed
}

// CommandKind is the set of explicit operator actions in the runtime
// contract.  There is intentionally no implicit "edit while viewing" action.
type CommandKind string

const (
	CommandForwardUnchanged CommandKind = "forward_unchanged"
	CommandForwardEdited    CommandKind = "forward_edited"
	CommandManualResponse   CommandKind = "manual_response"
	CommandReleaseUnchanged CommandKind = "release_unchanged"
	CommandReleaseEdited    CommandKind = "release_edited"
	CommandReplaceResponse  CommandKind = "replace_response"
	CommandDrop             CommandKind = "drop"
	CommandAbort            CommandKind = "abort"

	ForwardUnchanged CommandKind = CommandForwardUnchanged
	ForwardEdited    CommandKind = CommandForwardEdited
	ManualResponse   CommandKind = CommandManualResponse
	ReleaseUnchanged CommandKind = CommandReleaseUnchanged
	ReleaseEdited    CommandKind = CommandReleaseEdited
	ReplaceResponse  CommandKind = CommandReplaceResponse
	Drop             CommandKind = CommandDrop
	Abort            CommandKind = CommandAbort
)

// RequestPart and ResponsePart are the Go forms of the corresponding frozen
// runtime DTOs.  ArtifactRefs are metadata only; body bytes live in the
// registry's artifact store and are read explicitly by id.
type RequestPart struct {
	Envelope     wire.RequestEnvelope `json:"envelope"`
	ArtifactRefs []wire.ArtifactRef   `json:"artifact_refs"`
}

type ResponsePart struct {
	Envelope     wire.ResponseEnvelope `json:"envelope"`
	ArtifactRefs []wire.ArtifactRef    `json:"artifact_refs"`
}

// Snapshot is the serialisable exchange view consumed by the workspace.  The
// additive Revision and Error fields are useful for API clients while all
// fields named by docs/runtime-contract.md remain stable.
type Snapshot struct {
	ExchangeID string        `json:"exchange_id"`
	Protocol   string        `json:"protocol"`
	Request    RequestPart   `json:"request"`
	Response   ResponsePart  `json:"response"`
	Policy     policy.Policy `json:"policy"`
	State      State         `json:"state"`
	Warnings   []string      `json:"warnings"`
	CreatedAt  time.Time     `json:"created_at"`
	UpdatedAt  time.Time     `json:"updated_at"`
	Revision   uint64        `json:"revision"`
	Error      string        `json:"error,omitempty"`
}

// SnapshotDelta is intentionally a delta-shaped event payload.  A full
// Request/Response part is supplied only when artifact refs changed; ordinary
// state transitions need only state, warnings, and updated_at.
type SnapshotDelta struct {
	ExchangeID string         `json:"exchange_id,omitempty"`
	Protocol   string         `json:"protocol,omitempty"`
	Request    *RequestPart   `json:"request,omitempty"`
	Response   *ResponsePart  `json:"response,omitempty"`
	Policy     *policy.Policy `json:"policy,omitempty"`
	State      State          `json:"state,omitempty"`
	Warnings   []string       `json:"warnings,omitempty"`
	UpdatedAt  time.Time      `json:"updated_at,omitempty"`
	Error      string         `json:"error,omitempty"`
}

// EventKind is the event vocabulary from the frozen runtime contract.
type EventKind string

const (
	EventExchangeCreated EventKind = "exchange_created"
	EventRequestHeld     EventKind = "request_held"
	EventUpstreamStarted EventKind = "upstream_started"
	EventResponseHeld    EventKind = "response_held"
	EventUpdated         EventKind = "updated"
	EventCompleted       EventKind = "completed"
	EventFailed          EventKind = "failed"
	EventCancelled       EventKind = "cancelled"
	EventDropped         EventKind = "dropped"
	// EventStreamEvent is an additive observation kind: while a response body
	// streams, each client-visible SSE record reaches workspace subscribers as
	// its own event.  Stream events carry no revision and never mutate the
	// exchange; they exist only so realtime UI can project the stream.
	EventStreamEvent EventKind = "stream_event"
)

// Event is emitted after each revision is committed.  ArtifactRefs carry
// metadata for newly-created refs; no body bytes are ever placed inline.
// Stream carries one observed SSE record when Kind is EventStreamEvent.
type Event struct {
	EventID       string             `json:"event_id"`
	ExchangeID    string             `json:"exchange_id"`
	Revision      uint64             `json:"revision"`
	Kind          EventKind          `json:"kind"`
	SnapshotDelta SnapshotDelta      `json:"snapshot_delta"`
	ArtifactRefs  []wire.ArtifactRef `json:"artifact_refs"`
	CreatedAt     time.Time          `json:"created_at"`
	Stream        *StreamEvent       `json:"stream,omitempty"`
}

// StreamEvent retains one observed SSE record from a response body while it
// is still streaming.  It is a display-only projection of a copy of the wire
// bytes: the artifact remains the only wire authority, and no field here is
// ever used as transport input.  Ordinal is the record's index within the
// stream; ByteStart/ByteEnd locate the record's raw bytes in the response
// artifact.
type StreamEvent struct {
	Ordinal   int    `json:"ordinal"`
	Name      string `json:"name,omitempty"`
	ID        string `json:"sse_id,omitempty"`
	Data      string `json:"data,omitempty"`
	Complete  bool   `json:"complete,omitempty"`
	ByteStart int64  `json:"byte_start,omitempty"`
	ByteEnd   int64  `json:"byte_end,omitempty"`
}

// ExchangeEvent is an explicit alias matching the browser DTO terminology.
type ExchangeEvent = Event

// PatchOperation is the JSON-Patch subset accepted by MutationInput.  Value is
// kept as any so unknown/provider extension values survive an explicit edit.
type PatchOperation struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
	From  string `json:"from,omitempty"`
}

// MutationInput binds an edit to the artifact observed by the operator.  A
// raw replacement is text in the JSON API; RawReplacementBytes is a local Go
// seam for binary bodies and is never marshalled.
type MutationInput struct {
	Patch               []PatchOperation `json:"patch,omitempty"`
	RawReplacement      string           `json:"raw_replacement,omitempty"`
	RawReplacementBytes []byte           `json:"-"`
	BaseArtifactID      string           `json:"base_artifact_id,omitempty"`
	BaseSHA256          string           `json:"base_sha256,omitempty"`
}

// Command is the Go form of every workspace command.  It is safe to decode
// from the frozen JSON shape: mutation is present only on *_edited actions.
type Command struct {
	ExchangeID   string         `json:"exchange_id"`
	BaseRevision uint64         `json:"base_revision"`
	Kind         CommandKind    `json:"kind"`
	Mutation     *MutationInput `json:"mutation,omitempty"`
	RawResponse  string         `json:"raw_response,omitempty"`
	// RawResponseBytes is a binary-only local seam; RawResponse takes
	// precedence for JSON/API callers when it is non-empty.
	RawResponseBytes []byte `json:"-"`
	ContentType      string `json:"content_type,omitempty"`
	// ResponseStatus, ResponseHeaders and ResponseTrailers are optional local
	// envelope controls for manual_response/replace_response. Zero status
	// means 200; an empty header map preserves source response headers when
	// replacing an upstream response.
	ResponseStatus   int         `json:"response_status,omitempty"`
	ResponseHeaders  http.Header `json:"response_headers,omitempty"`
	ResponseTrailers http.Header `json:"response_trailers,omitempty"`
	Reason           string      `json:"reason,omitempty"`
}

type ValidationResult struct {
	Valid    bool     `json:"valid"`
	Protocol string   `json:"protocol,omitempty"`
	Errors   []string `json:"errors"`
	Warnings []string `json:"warnings"`
}

type MutationResult struct {
	BaseArtifactID  string            `json:"base_artifact_id,omitempty"`
	BaseSHA256      string            `json:"base_sha256,omitempty"`
	DerivedArtifact *wire.ArtifactRef `json:"derived_artifact,omitempty"`
	Validation      *ValidationResult `json:"validation,omitempty"`
}

type CommandResult struct {
	Exchange Snapshot        `json:"exchange"`
	Revision uint64          `json:"revision"`
	Mutation *MutationResult `json:"mutation,omitempty"`
	Event    *Event          `json:"event,omitempty"`
}

// UpstreamRequest is the explicit transport seam.  Artifact is the original
// inbound body for unchanged forwarding or a derived artifact for an edit.
type UpstreamRequest struct {
	ExchangeID string
	Envelope   wire.RequestEnvelope
	Artifact   wire.BodyArtifact
}

// UpstreamResponse is returned by the generic upstream transport.  HTTP 4xx
// and 5xx responses are represented here as successful returns, allowing the
// response gate to inspect/release them unchanged.
type UpstreamResponse struct {
	Envelope wire.ResponseEnvelope
	Artifact wire.BodyArtifact
}

// DownstreamResponse is the explicit writer seam.  The writer must commit the
// envelope and stream/copy Artifact itself; the exchange package never decodes
// or regenerates its bytes.
type DownstreamResponse struct {
	ExchangeID string
	Envelope   wire.ResponseEnvelope
	Artifact   wire.BodyArtifact
}

type UpstreamRoundTripper func(context.Context, UpstreamRequest) (UpstreamResponse, error)
type DownstreamWriter func(context.Context, DownstreamResponse) error

// CreateParams supplies the immutable request side and transport hooks for a
// new exchange.  PolicyOverride takes precedence when non-nil; otherwise
// Policy is used when non-zero and the registry policy is used as fallback.
type CreateParams struct {
	ExchangeID      string
	Protocol        string
	RequestEnvelope wire.RequestEnvelope
	RequestArtifact wire.BodyArtifact
	Policy          policy.Policy
	PolicyOverride  *policy.Policy
	Upstream        UpstreamRoundTripper
	Downstream      DownstreamWriter
	Context         context.Context
	// Events receives immutable event values after a state transition commits.
	// The callback is invoked without the exchange mutex held. A nil callback
	// simply disables event delivery.
	Events EventSink
}

// EventSink receives workspace events emitted by an exchange. Implementations
// must treat Event and its nested values as read-only; the registry gives each
// callback an independent value copy.
type EventSink func(Event)

// Errors are stable sentinels suitable for errors.Is checks by HTTP/WS layers.
var (
	ErrNotFound           = errors.New("exchange: not found")
	ErrRevisionConflict   = errors.New("exchange: stale revision")
	ErrInvalidCommand     = errors.New("exchange: invalid command")
	ErrInvalidState       = errors.New("exchange: command is not valid in current state")
	ErrArtifactConflict   = errors.New("exchange: artifact revision conflict")
	ErrMutationInvalid    = errors.New("exchange: invalid mutation")
	ErrAlreadyTerminal    = errors.New("exchange: exchange is already terminal")
	ErrUpstreamNotStarted = errors.New("exchange: upstream has not started")
	ErrNoResponse         = errors.New("exchange: upstream response is not available")
)

// RevisionConflictError carries both revisions while preserving
// errors.Is(err, ErrRevisionConflict).
type RevisionConflictError struct {
	Expected uint64
	Received uint64
}

func (e *RevisionConflictError) Error() string {
	return fmt.Sprintf("exchange: stale revision: expected %d, received %d", e.Expected, e.Received)
}
func (e *RevisionConflictError) Unwrap() error { return ErrRevisionConflict }

// ArtifactConflictError identifies a stale edit base while preserving
// errors.Is(err, ErrArtifactConflict).
type ArtifactConflictError struct {
	ExpectedID  string
	ReceivedID  string
	ExpectedSHA string
	ReceivedSHA string
}

func (e *ArtifactConflictError) Error() string {
	return fmt.Sprintf("exchange: stale artifact: expected id=%q sha256=%q, received id=%q sha256=%q", e.ExpectedID, e.ExpectedSHA, e.ReceivedID, e.ReceivedSHA)
}
func (e *ArtifactConflictError) Unwrap() error { return ErrArtifactConflict }

// InvalidStateError adds the state and action to ErrInvalidState.
type InvalidStateError struct {
	State  State
	Action CommandKind
}

func (e *InvalidStateError) Error() string {
	return fmt.Sprintf("exchange: action %q is invalid in state %q", e.Action, e.State)
}
func (e *InvalidStateError) Unwrap() error { return ErrInvalidState }

// Keep sync imported in this DTO file so generated docs show that exchange
// values are concurrency-owned by Registry/Exchange (the actual mutex lives in
// registry.go).  This compile-time assertion also prevents accidental removal
// during refactors without changing the public wire shape.
var _ sync.Locker

// Ensure imported net/http remains available for callers that use the response
// status constants with this package; this is a compile-time-only reference.
var _ = http.StatusOK
