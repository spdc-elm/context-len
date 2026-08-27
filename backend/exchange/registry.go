package exchange

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"context-lens/backend/inspection"
	"context-lens/backend/mutation"
	"context-lens/backend/policy"
	"context-lens/backend/wire"
)

// Registry owns exchange identities and the policy used for newly-created
// exchanges.  An Exchange has its own mutex because upstream and downstream
// operations may complete concurrently with workspace commands.
type Registry struct {
	mu       sync.RWMutex
	items    map[string]*Exchange
	defaults policy.Policy
}

// Exchange is one HTTP exchange state machine.  All fields other than params,
// ctx, cancel, and done are guarded by mu.  BodyArtifact values are immutable
// copies, so retaining them here cannot let a projection mutate wire bytes.
type Exchange struct {
	mu     sync.Mutex
	snap   Snapshot
	params CreateParams

	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	doneOnce sync.Once

	forwardArtifact   wire.BodyArtifact
	responseArtifact  wire.BodyArtifact
	responseEnvelope  wire.ResponseEnvelope
	responseAvailable bool
	operation         bool
	upstreamStarted   bool
	artifacts         map[string]wire.BodyArtifact
}

var exchangeIDCounter atomic.Uint64
var eventIDCounter atomic.Uint64

// NewRegistry creates a registry. Invalid defaults are replaced with the
// conservative bypass policy rather than being allowed to poison new work.
func NewRegistry(defaults policy.Policy) *Registry {
	if defaults.IsZero() {
		defaults = policy.Default()
	}
	if err := defaults.Validate(); err != nil {
		defaults = policy.Default()
	}
	return &Registry{items: make(map[string]*Exchange), defaults: defaults.Normalize()}
}

// Create registers an exchange before starting any asynchronous upstream
// work. This ordering is important: a caller can always Get an exchange after
// Create returns, and a very fast upstream cannot race registry insertion.
func (r *Registry) Create(p CreateParams) (*Exchange, error) {
	if r == nil {
		return nil, errors.New("exchange: nil registry")
	}
	id := p.ExchangeID
	if id == "" {
		id = newExchangeID()
	}
	pol := p.Policy
	if p.PolicyOverride != nil {
		pol = *p.PolicyOverride
	}
	if pol.IsZero() {
		r.mu.RLock()
		pol = r.defaults
		r.mu.RUnlock()
	}
	if err := pol.Validate(); err != nil {
		return nil, err
	}
	pol = pol.Normalize()
	if p.Context == nil {
		p.Context = context.Background()
	}

	now := time.Now().UTC()
	state := StateReceived
	initialKind := EventExchangeCreated
	if pol.RequestHeld() {
		state = StateRequestHeld
		initialKind = EventRequestHeld
	}
	snap := Snapshot{
		ExchangeID: id,
		Protocol:   p.Protocol,
		Policy:     pol,
		State:      state,
		CreatedAt:  now,
		UpdatedAt:  now,
		Revision:   1,
		Warnings:   []string{},
		Response:   ResponsePart{ArtifactRefs: []wire.ArtifactRef{}},
		Request: RequestPart{
			Envelope:     p.RequestEnvelope.Clone().Redacted(),
			ArtifactRefs: []wire.ArtifactRef{p.RequestArtifact.Ref()},
		},
	}
	ctx, cancel := context.WithCancel(p.Context)
	e := &Exchange{
		snap:            snap,
		params:          p,
		ctx:             ctx,
		cancel:          cancel,
		done:            make(chan struct{}),
		forwardArtifact: p.RequestArtifact,
		artifacts:       make(map[string]wire.BodyArtifact),
	}
	e.saveArtifactLocked(p.RequestArtifact)

	r.mu.Lock()
	if _, exists := r.items[id]; exists {
		r.mu.Unlock()
		cancel()
		return nil, fmt.Errorf("exchange: duplicate id %q", id)
	}
	r.items[id] = e
	r.mu.Unlock()

	initialEvent := e.initialEvent(initialKind)
	go e.watchContext()
	if state == StateRequestHeld {
		e.emit(initialEvent)
		return e, nil
	}

	// Begin pass-through before delivering events. The launch itself occurs
	// after event delivery; executeUpstream checks the context/state again, so
	// an event subscriber can safely abort before any upstream call.
	startEvent, startSink, launch := e.beginUpstream(p.RequestArtifact)
	e.emit(initialEvent)
	if startSink != nil {
		emitEvent(startSink, startEvent)
	}
	if launch {
		e.launchUpstream()
	}
	return e, nil
}

func newExchangeID() string {
	seq := exchangeIDCounter.Add(1)
	return fmt.Sprintf("ex-%d-%d", time.Now().UTC().UnixNano(), seq)
}

func newEventID() string {
	seq := eventIDCounter.Add(1)
	return fmt.Sprintf("event-%d-%d", time.Now().UTC().UnixNano(), seq)
}

// Get returns an exchange by id.
func (r *Registry) Get(id string) (*Exchange, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.items[id]
	return e, ok
}

func (r *Registry) AddWarnings(exchangeID string, warnings ...string) error {
	e, ok := r.Get(exchangeID)
	if !ok {
		return ErrNotFound
	}
	e.mu.Lock()
	changed := false
	for _, warning := range warnings {
		if warning == "" {
			continue
		}
		seen := false
		for _, existing := range e.snap.Warnings {
			if existing == warning {
				seen = true
				break
			}
		}
		if !seen {
			e.snap.Warnings = append(e.snap.Warnings, warning)
			changed = true
		}
	}
	if !changed {
		e.mu.Unlock()
		return nil
	}
	event, sink := e.commitLocked(EventUpdated, e.snap.State, SnapshotDelta{Warnings: cloneStrings(e.snap.Warnings)})
	e.mu.Unlock()
	e.emitTo(sink, event)
	return nil
}

func (r *Registry) AddArtifactRef(exchangeID string, requestSide bool, ref wire.ArtifactRef) error {
	e, ok := r.Get(exchangeID)
	if !ok {
		return ErrNotFound
	}
	e.mu.Lock()
	var refs *[]wire.ArtifactRef
	if requestSide {
		refs = &e.snap.Request.ArtifactRefs
	} else {
		refs = &e.snap.Response.ArtifactRefs
	}
	for _, existing := range *refs {
		if existing.ArtifactID == ref.ArtifactID || (existing.Stage == ref.Stage && existing.SHA256 == ref.SHA256) {
			e.mu.Unlock()
			return nil
		}
	}
	*refs = append(*refs, ref)
	var delta SnapshotDelta
	if requestSide {
		delta.Request = cloneRequestPartPtr(e.snap.Request)
	} else {
		delta.Response = cloneResponsePartPtr(e.snap.Response)
	}
	event, sink := e.commitLocked(EventUpdated, e.snap.State, delta)
	e.mu.Unlock()
	e.emitTo(sink, event)
	return nil
}

// List returns immutable snapshot copies in creation order. It is convenient
// for a workspace queue and deliberately never exposes Exchange pointers.
func (r *Registry) List() []Snapshot {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	items := make([]*Exchange, 0, len(r.items))
	for _, e := range r.items {
		items = append(items, e)
	}
	r.mu.RUnlock()
	out := make([]Snapshot, 0, len(items))
	for _, e := range items {
		out = append(out, e.Snapshot())
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ExchangeID < out[j].ExchangeID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// Command dispatches a workspace command by exchange id.
func (r *Registry) Command(c Command) (CommandResult, error) {
	if r == nil {
		return CommandResult{}, errors.New("exchange: nil registry")
	}
	e, ok := r.Get(c.ExchangeID)
	if !ok {
		return CommandResult{}, ErrNotFound
	}
	return e.Command(c)
}

// Artifact resolves a body by id without exposing the mutable registry map.
func (r *Registry) Artifact(id string) (wire.BodyArtifact, bool) {
	if r == nil || id == "" {
		return wire.BodyArtifact{}, false
	}
	r.mu.RLock()
	items := make([]*Exchange, 0, len(r.items))
	for _, e := range r.items {
		items = append(items, e)
	}
	r.mu.RUnlock()
	for _, e := range items {
		if artifact, ok := e.Artifact(id); ok {
			return artifact, true
		}
	}
	return wire.BodyArtifact{}, false
}

// Snapshot returns a deep value copy suitable for JSON/WS serialization.
func (e *Exchange) Snapshot() Snapshot {
	if e == nil {
		return Snapshot{}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return cloneSnapshot(e.snap)
}

// Artifact returns an immutable artifact retained by this exchange. Bytes
// remain copied by BodyArtifact, so callers cannot mutate the authority.
func (e *Exchange) Artifact(id string) (wire.BodyArtifact, bool) {
	if e == nil || id == "" {
		return wire.BodyArtifact{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	a, ok := e.artifacts[id]
	return a, ok
}

// Done closes once the exchange reaches a terminal state.
func (e *Exchange) Done() <-chan struct{} {
	if e == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return e.done
}

// Wait waits for a terminal exchange and returns its final snapshot. The
// caller context only bounds the wait; it does not cancel the exchange.
func (e *Exchange) Wait(ctx context.Context) (Snapshot, error) {
	if e == nil {
		return Snapshot{}, ErrNotFound
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-e.done:
		return e.Snapshot(), nil
	case <-ctx.Done():
		return e.Snapshot(), ctx.Err()
	}
}

// Command applies exactly one explicit operator action. Revision and state
// checks happen under the exchange mutex; potentially blocking transport
// callbacks run after unlocking and are guarded by operation/state checks.
func (e *Exchange) Command(c Command) (CommandResult, error) {
	if e == nil {
		return CommandResult{}, ErrNotFound
	}
	if c.ExchangeID != e.snapID() {
		return CommandResult{}, ErrNotFound
	}

	var (
		firstEvent Event
		firstSink  EventSink
		mutation   *MutationResult
		launch     bool
		deliver    *DownstreamResponse
		deliverCtx context.Context
	)

	e.mu.Lock()
	if c.BaseRevision != e.snap.Revision {
		expected := e.snap.Revision
		e.mu.Unlock()
		return CommandResult{}, &RevisionConflictError{Expected: expected, Received: c.BaseRevision}
	}
	if e.snap.State.Terminal() {
		e.mu.Unlock()
		return CommandResult{}, ErrAlreadyTerminal
	}
	if c.ExchangeID != e.snap.ExchangeID {
		e.mu.Unlock()
		return CommandResult{}, ErrNotFound
	}

	switch c.Kind {
	case CommandDrop:
		cancel := e.cancel
		firstEvent, firstSink, _ = e.terminalLocked(StateDropped, nil, EventDropped, c.Reason)
		result := e.resultLocked(nil, &firstEvent)
		e.mu.Unlock()
		e.emitTo(firstSink, firstEvent)
		if cancel != nil {
			cancel()
		}
		return result, nil

	case CommandAbort:
		cancel := e.cancel
		firstEvent, firstSink, _ = e.terminalLocked(StateCancelled, nil, EventCancelled, c.Reason)
		result := e.resultLocked(nil, &firstEvent)
		e.mu.Unlock()
		e.emitTo(firstSink, firstEvent)
		if cancel != nil {
			cancel()
		}
		return result, nil

	case CommandForwardUnchanged:
		if e.snap.State != StateRequestHeld && e.snap.State != StateReceived {
			stateErr := &InvalidStateError{State: e.snap.State, Action: c.Kind}
			e.mu.Unlock()
			return CommandResult{}, stateErr
		}
		firstEvent, firstSink, launch = e.beginUpstreamLocked(e.forwardArtifact, false)

	case CommandForwardEdited:
		if e.snap.State != StateRequestHeld && e.snap.State != StateReceived {
			stateErr := &InvalidStateError{State: e.snap.State, Action: c.Kind}
			e.mu.Unlock()
			return CommandResult{}, stateErr
		}
		base := e.forwardArtifact
		var err error
		var result ArtifactMutation
		result, err = e.deriveLocked(base, c.Mutation)
		if err != nil {
			e.mu.Unlock()
			return CommandResult{}, err
		}
		e.forwardArtifact = result.Artifact
		e.saveArtifactLocked(result.Artifact)
		e.appendRequestArtifactLocked(result.Artifact.Ref())
		mutation = result.Result
		firstEvent, firstSink, launch = e.beginUpstreamLocked(e.forwardArtifact, true)

	case CommandManualResponse:
		if e.snap.State != StateRequestHeld {
			stateErr := &InvalidStateError{State: e.snap.State, Action: c.Kind}
			e.mu.Unlock()
			return CommandResult{}, stateErr
		}
		artifact, env, result, err := e.manualArtifactLocked(c, false)
		if err != nil {
			e.mu.Unlock()
			return CommandResult{}, err
		}
		e.responseArtifact = artifact
		e.responseEnvelope = env.Clone()
		e.responseAvailable = true
		e.saveArtifactLocked(artifact)
		e.snap.Response = ResponsePart{Envelope: env.Redacted(), ArtifactRefs: []wire.ArtifactRef{artifact.Ref()}}
		mutation = result
		firstEvent, firstSink, deliver = e.beginDeliveryLocked(env, artifact)
		deliverCtx = e.ctx

	case CommandReleaseUnchanged:
		if e.snap.State != StateResponseHeld {
			stateErr := &InvalidStateError{State: e.snap.State, Action: c.Kind}
			e.mu.Unlock()
			return CommandResult{}, stateErr
		}
		if !e.responseAvailable {
			e.mu.Unlock()
			return CommandResult{}, ErrNoResponse
		}
		artifact := e.responseArtifact
		env := e.responseEnvelope.Clone()
		firstEvent, firstSink, deliver = e.beginDeliveryLocked(env, artifact)
		deliverCtx = e.ctx

	case CommandReleaseEdited:
		if e.snap.State != StateResponseHeld {
			stateErr := &InvalidStateError{State: e.snap.State, Action: c.Kind}
			e.mu.Unlock()
			return CommandResult{}, stateErr
		}
		if !e.responseAvailable {
			e.mu.Unlock()
			return CommandResult{}, ErrNoResponse
		}
		base := e.responseArtifact
		result, err := e.deriveLocked(base, c.Mutation)
		if err != nil {
			e.mu.Unlock()
			return CommandResult{}, err
		}
		e.saveArtifactLocked(result.Artifact)
		e.appendResponseArtifactLocked(result.Artifact.Ref())
		mutation = result.Result
		env := e.responseEnvelope.Clone()
		firstEvent, firstSink, deliver = e.beginDeliveryLocked(env, result.Artifact)
		deliverCtx = e.ctx

	case CommandReplaceResponse:
		if e.snap.State != StateResponseHeld {
			stateErr := &InvalidStateError{State: e.snap.State, Action: c.Kind}
			e.mu.Unlock()
			return CommandResult{}, stateErr
		}
		if !e.responseAvailable {
			e.mu.Unlock()
			return CommandResult{}, ErrNoResponse
		}
		artifact, env, result, err := e.manualArtifactLocked(c, true)
		if err != nil {
			e.mu.Unlock()
			return CommandResult{}, err
		}
		e.saveArtifactLocked(artifact)
		e.appendResponseArtifactLocked(artifact.Ref())
		mutation = result
		firstEvent, firstSink, deliver = e.beginDeliveryLocked(env, artifact)
		deliverCtx = e.ctx

	default:
		e.mu.Unlock()
		return CommandResult{}, ErrInvalidCommand
	}

	// Save the state after the first transition before callbacks can run.
	initialResult := e.resultLocked(mutation, &firstEvent)
	e.mu.Unlock()
	e.emitTo(firstSink, firstEvent)

	if launch {
		e.launchUpstream()
	}
	if deliver != nil {
		finalEvent := e.executeDelivery(deliverCtx, *deliver)
		if finalEvent != nil {
			initialResult.Event = finalEvent
			initialResult.Exchange = e.Snapshot()
			initialResult.Revision = initialResult.Exchange.Revision
		}
	}
	return initialResult, nil
}

// snapID reads the id without promising a live snapshot. It is used only as a
// fast reject before the authoritative locked check in Command.
func (e *Exchange) snapID() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.snap.ExchangeID
}

// ArtifactMutation is an internal pair that keeps the public mutation result
// and body artifact together until the state transition commits.
type ArtifactMutation struct {
	Artifact wire.BodyArtifact
	Result   *MutationResult
}

func (e *Exchange) deriveLocked(base wire.BodyArtifact, in *MutationInput) (ArtifactMutation, error) {
	if in == nil {
		return ArtifactMutation{}, fmt.Errorf("%w: mutation is required", ErrMutationInvalid)
	}
	ref := base.Ref()
	if encoding := strings.TrimSpace(ref.ContentEncoding); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return ArtifactMutation{}, fmt.Errorf("%w: compressed artifacts require explicit re-encoding", ErrMutationInvalid)
	}
	if in.BaseArtifactID != "" || in.BaseSHA256 != "" {
		if (in.BaseArtifactID != "" && in.BaseArtifactID != ref.ArtifactID) || (in.BaseSHA256 != "" && in.BaseSHA256 != ref.SHA256) {
			return ArtifactMutation{}, &ArtifactConflictError{ExpectedID: ref.ArtifactID, ReceivedID: in.BaseArtifactID, ExpectedSHA: ref.SHA256, ReceivedSHA: in.BaseSHA256}
		}
	}
	if len(in.Patch) > 0 && (in.RawReplacement != "" || in.RawReplacementBytes != nil) {
		return ArtifactMutation{}, fmt.Errorf("%w: patch and raw replacement are mutually exclusive", ErrMutationInvalid)
	}

	var candidate mutation.Result
	var err error
	protocol := inspection.Protocol(e.snap.Protocol)
	format := inspection.FormatJSON
	if strings.Contains(strings.ToLower(ref.ContentType), "event-stream") {
		format = inspection.FormatSSE
	}
	switch {
	case len(in.Patch) > 0:
		ops := make([]mutation.Operation, 0, len(in.Patch))
		for _, op := range in.Patch {
			ops = append(ops, mutation.Operation{Op: op.Op, Path: op.Path, Value: op.Value})
		}
		candidate, err = mutation.JSONPatchProtocol(base, ops, protocol, format)
	case in.RawReplacementBytes != nil:
		candidate, err = mutation.ReplaceProtocol(base, in.RawReplacementBytes, protocol, format)
	case in.RawReplacement != "":
		candidate, err = mutation.ReplaceProtocol(base, []byte(in.RawReplacement), protocol, format)
	default:
		return ArtifactMutation{}, fmt.Errorf("%w: patch or raw replacement is required", ErrMutationInvalid)
	}
	if err != nil {
		return ArtifactMutation{}, fmt.Errorf("%w: %v", ErrMutationInvalid, err)
	}
	if _, err = mutation.RequireValid(candidate); err != nil {
		return ArtifactMutation{}, fmt.Errorf("%w: %v", ErrMutationInvalid, err)
	}
	artifact := candidate.Artifact
	validation := &ValidationResult{Valid: candidate.Validated, Protocol: e.snap.Protocol, Errors: candidate.Validation.ErrorMessages(), Warnings: candidate.Validation.WarningMessages()}
	result := &MutationResult{
		BaseArtifactID:  ref.ArtifactID,
		BaseSHA256:      ref.SHA256,
		DerivedArtifact: artifactRefPtr(artifact.Ref()),
		Validation:      validation,
	}
	return ArtifactMutation{Artifact: artifact, Result: result}, nil
}

func newDerivedArtifact(base wire.BodyArtifact, body []byte) wire.BodyArtifact {
	ref := base.Ref()
	return wire.NewArtifact(body, wire.ArtifactOptions{
		Stage:           "derived",
		Direction:       ref.Direction,
		ContentType:     ref.ContentType,
		ContentEncoding: ref.ContentEncoding,
	})
}

func artifactRefPtr(ref wire.ArtifactRef) *wire.ArtifactRef { return &ref }

func (e *Exchange) manualArtifactLocked(c Command, replacement bool) (wire.BodyArtifact, wire.ResponseEnvelope, *MutationResult, error) {
	var source wire.BodyArtifact
	var sourceEnv wire.ResponseEnvelope
	if replacement {
		if !e.responseAvailable {
			return wire.BodyArtifact{}, wire.ResponseEnvelope{}, nil, ErrNoResponse
		}
		source = e.responseArtifact
		sourceEnv = e.responseEnvelope.Clone()
	}
	body := responseBytes(c.RawResponse, c.RawResponseBytes)
	ref := source.Ref()
	contentType := c.ContentType
	if contentType == "" {
		contentType = ref.ContentType
	}
	if contentType == "" {
		contentType = sourceEnv.Headers.Get("Content-Type")
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	format := inspection.FormatJSON
	if strings.Contains(strings.ToLower(contentType), "event-stream") {
		format = inspection.FormatSSE
	}
	protocol := inspection.Protocol(e.snap.Protocol)
	protocolValidation := inspection.ValidateProtocol(protocol, body, format)
	if !protocolValidation.Valid {
		return wire.BodyArtifact{}, wire.ResponseEnvelope{}, nil, fmt.Errorf("%w: protocol validation failed: %s", ErrMutationInvalid, strings.Join(protocolValidation.ErrorMessages(), "; "))
	}
	artifact := wire.NewArtifact(body, wire.ArtifactOptions{
		Stage:           wire.StageResponseDownstream,
		Direction:       wire.DirectionDownstream,
		ContentType:     contentType,
		ContentEncoding: "",
	})

	env := sourceEnv
	if env.Status == 0 {
		env.Status = httpStatusOK
	}
	if !replacement {
		env = wire.NewResponseEnvelope(httpStatusOK, nil, nil, time.Now().UTC(), time.Now().UTC())
	}
	if c.ResponseStatus != 0 {
		env.Status = c.ResponseStatus
	}
	if env.Status < 100 || env.Status > 599 {
		return wire.BodyArtifact{}, wire.ResponseEnvelope{}, nil, fmt.Errorf("%w: response status %d is invalid", ErrMutationInvalid, env.Status)
	}
	if c.ResponseHeaders != nil {
		env.Headers = c.ResponseHeaders
	}
	if env.Headers == nil {
		env.Headers = make(http.Header)
	}
	env.Headers.Del("Content-Encoding")
	if c.ContentType != "" {
		env.Headers.Set("Content-Type", c.ContentType)
	} else if env.Headers.Get("Content-Type") == "" {
		env.Headers.Set("Content-Type", contentType)
	}
	if c.ResponseTrailers != nil {
		env.Trailers = c.ResponseTrailers
	}
	now := time.Now().UTC()
	env.StartedAt, env.EndedAt = now, now
	var baseID, baseSHA string
	if replacement {
		baseID, baseSHA = ref.ArtifactID, ref.SHA256
	}
	result := &MutationResult{
		BaseArtifactID:  baseID,
		BaseSHA256:      baseSHA,
		DerivedArtifact: artifactRefPtr(artifact.Ref()),
		Validation:      &ValidationResult{Valid: protocolValidation.Valid, Protocol: e.snap.Protocol, Errors: protocolValidation.ErrorMessages(), Warnings: protocolValidation.WarningMessages()},
	}
	return artifact, env.Clone(), result, nil
}

func responseBytes(text string, bytes []byte) []byte {
	if bytes != nil {
		return append([]byte(nil), bytes...)
	}
	return []byte(text)
}

const httpStatusOK = 200

// beginUpstreamLocked transitions a request decision to upstream_running. It
// does not call user code while holding mu. launch is false if context was
// already cancelled and a terminal cancellation event was committed instead.
func (e *Exchange) beginUpstreamLocked(artifact wire.BodyArtifact, requestChanged bool) (event Event, sink EventSink, launch bool) {
	if e.snap.State.Terminal() {
		return Event{}, nil, false
	}
	if e.ctx.Err() != nil {
		event, sink, _ := e.terminalLocked(StateCancelled, nil, EventCancelled, "context cancelled before upstream start")
		return event, sink, false
	}
	if e.upstreamStarted {
		return Event{}, nil, false
	}
	e.forwardArtifact = artifact
	e.upstreamStarted = true
	delta := SnapshotDelta{}
	if requestChanged {
		part := cloneRequestPart(e.snap.Request)
		delta.Request = &part
	}
	event, sink = e.commitLocked(EventUpstreamStarted, StateUpstreamRunning, delta)
	return event, sink, true
}

// beginUpstream is the unlocked form used during Create.
func (e *Exchange) beginUpstream(artifact wire.BodyArtifact) (Event, EventSink, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.beginUpstreamLocked(artifact, false)
}

func (e *Exchange) launchUpstream() {
	e.mu.Lock()
	if !e.upstreamStarted || e.snap.State.Terminal() {
		e.mu.Unlock()
		return
	}
	ctx := e.ctx
	upstream := e.params.Upstream
	req := UpstreamRequest{ExchangeID: e.snap.ExchangeID, Envelope: e.params.RequestEnvelope.Clone(), Artifact: e.forwardArtifact}
	e.mu.Unlock()
	go e.executeUpstream(ctx, upstream, req)
}

// runUpstream remains a small compatibility seam for package users/tests that
// previously started the hook directly. Normal paths use launchUpstream.
func (e *Exchange) runUpstream() {
	e.launchUpstream()
}

func (e *Exchange) executeUpstream(ctx context.Context, upstream UpstreamRoundTripper, req UpstreamRequest) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		e.finishContext(err)
		return
	}
	if upstream == nil {
		e.finishFailure(errors.New("upstream is not configured"))
		return
	}
	resp, err := upstream(ctx, req)
	if err != nil {
		if ctx.Err() != nil {
			e.finishContext(ctx.Err())
		} else {
			e.finishFailure(err)
		}
		return
	}

	e.mu.Lock()
	if e.snap.State.Terminal() {
		e.mu.Unlock()
		return
	}
	if ctx.Err() != nil {
		e.mu.Unlock()
		e.finishContext(ctx.Err())
		return
	}
	e.responseArtifact = resp.Artifact
	e.responseEnvelope = resp.Envelope.Clone()
	e.responseAvailable = true
	e.saveArtifactLocked(resp.Artifact)
	e.snap.Response = ResponsePart{Envelope: resp.Envelope.Redacted(), ArtifactRefs: e.responseRefsWith(resp.Artifact.Ref())}
	if e.snap.Policy.ResponseHeld() {
		delta := SnapshotDelta{Response: cloneResponsePartPtr(e.snap.Response)}
		event, sink := e.commitLocked(EventResponseHeld, StateResponseHeld, delta)
		e.mu.Unlock()
		e.emitTo(sink, event)
		return
	}

	// For a real writer expose the response artifact while the writer is in
	// progress; with no writer, complete atomically in one revision.
	if e.params.Downstream == nil {
		delta := SnapshotDelta{Response: cloneResponsePartPtr(e.snap.Response)}
		event, sink, cancel := e.terminalLockedWithDelta(StateCompleted, nil, EventCompleted, "", delta)
		e.mu.Unlock()
		e.emitTo(sink, event)
		if cancel != nil {
			cancel()
		}
		return
	}
	e.operation = true
	delta := SnapshotDelta{Response: cloneResponsePartPtr(e.snap.Response)}
	event, sink := e.commitLocked(EventUpdated, StateUpstreamRunning, delta)
	e.mu.Unlock()
	e.emitTo(sink, event)
	e.executeDelivery(ctx, DownstreamResponse{ExchangeID: req.ExchangeID, Envelope: resp.Envelope.Clone(), Artifact: resp.Artifact})
}

func (e *Exchange) beginDeliveryLocked(env wire.ResponseEnvelope, artifact wire.BodyArtifact) (Event, EventSink, *DownstreamResponse) {
	if e.ctx.Err() != nil {
		event, sink, _ := e.terminalLocked(StateCancelled, nil, EventCancelled, "context cancelled before downstream write")
		return event, sink, nil
	}
	e.operation = true
	e.responseEnvelope = env.Clone()
	e.snap.Response = ResponsePart{Envelope: env.Redacted(), ArtifactRefs: e.responseRefsWith(artifact.Ref())}
	delta := SnapshotDelta{Response: cloneResponsePartPtr(e.snap.Response)}
	event, sink := e.commitLocked(EventUpdated, StateUpstreamRunning, delta)
	return event, sink, &DownstreamResponse{ExchangeID: e.snap.ExchangeID, Envelope: env.Clone(), Artifact: artifact}
}

func (e *Exchange) executeDelivery(ctx context.Context, response DownstreamResponse) *Event {
	if ctx == nil {
		ctx = context.Background()
	}
	e.mu.Lock()
	if e.snap.State.Terminal() || !e.operation {
		e.mu.Unlock()
		return nil
	}
	writer := e.params.Downstream
	e.mu.Unlock()

	var err error
	if writer != nil {
		if ctx.Err() != nil {
			e.finishContext(ctx.Err())
			return nil
		}
		err = writer(ctx, response)
	}
	if err != nil {
		if ctx.Err() != nil {
			e.finishContext(ctx.Err())
		} else {
			e.finishFailure(err)
		}
		return nil
	}
	if ctx.Err() != nil {
		e.finishContext(ctx.Err())
		return nil
	}
	e.mu.Lock()
	if e.snap.State.Terminal() {
		e.operation = false
		e.mu.Unlock()
		return nil
	}
	e.operation = false
	event, sink, cancel := e.terminalLocked(StateCompleted, nil, EventCompleted, "")
	e.mu.Unlock()
	e.emitTo(sink, event)
	if cancel != nil {
		cancel()
	}
	return &event
}

func (e *Exchange) finishFailure(err error) {
	if err == nil {
		err = errors.New("exchange: upstream failed")
	}
	e.mu.Lock()
	if e.snap.State.Terminal() {
		e.mu.Unlock()
		return
	}
	e.operation = false
	event, sink, cancel := e.terminalLocked(StateFailed, err, EventFailed, "")
	e.mu.Unlock()
	e.emitTo(sink, event)
	if cancel != nil {
		cancel()
	}
}

func (e *Exchange) finishContext(err error) {
	reason := "context cancelled"
	if err != nil {
		reason = err.Error()
	}
	e.mu.Lock()
	if e.snap.State.Terminal() {
		e.mu.Unlock()
		return
	}
	e.operation = false
	event, sink, cancel := e.terminalLocked(StateCancelled, nil, EventCancelled, reason)
	e.mu.Unlock()
	e.emitTo(sink, event)
	if cancel != nil {
		cancel()
	}
}

func (e *Exchange) watchContext() {
	select {
	case <-e.ctx.Done():
		e.finishContext(e.ctx.Err())
	case <-e.done:
	}
}

func (e *Exchange) saveArtifactLocked(artifact wire.BodyArtifact) {
	id := artifact.Ref().ArtifactID
	if id != "" {
		e.artifacts[id] = artifact
	}
}

func (e *Exchange) appendRequestArtifactLocked(ref wire.ArtifactRef) {
	if ref.ArtifactID == "" {
		return
	}
	for _, existing := range e.snap.Request.ArtifactRefs {
		if existing.ArtifactID == ref.ArtifactID {
			return
		}
	}
	e.snap.Request.ArtifactRefs = append(e.snap.Request.ArtifactRefs, ref)
}

func (e *Exchange) appendResponseArtifactLocked(ref wire.ArtifactRef) {
	if ref.ArtifactID == "" {
		return
	}
	for _, existing := range e.snap.Response.ArtifactRefs {
		if existing.ArtifactID == ref.ArtifactID {
			return
		}
	}
	e.snap.Response.ArtifactRefs = append(e.snap.Response.ArtifactRefs, ref)
}

func (e *Exchange) responseRefsWith(ref wire.ArtifactRef) []wire.ArtifactRef {
	refs := append([]wire.ArtifactRef(nil), e.snap.Response.ArtifactRefs...)
	if ref.ArtifactID != "" {
		found := false
		for _, existing := range refs {
			if existing.ArtifactID == ref.ArtifactID {
				found = true
				break
			}
		}
		if !found {
			refs = append(refs, ref)
		}
	}
	return refs
}

func (e *Exchange) initialEvent(kind EventKind) Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	delta := SnapshotDelta{
		ExchangeID: e.snap.ExchangeID,
		Protocol:   e.snap.Protocol,
		Request:    cloneRequestPartPtr(e.snap.Request),
		Policy:     policyPtr(e.snap.Policy),
		State:      e.snap.State,
		UpdatedAt:  e.snap.UpdatedAt,
	}
	return Event{
		EventID:       newEventID(),
		ExchangeID:    e.snap.ExchangeID,
		Revision:      e.snap.Revision,
		Kind:          kind,
		SnapshotDelta: delta,
		ArtifactRefs:  append([]wire.ArtifactRef(nil), e.snap.Request.ArtifactRefs...),
		CreatedAt:     e.snap.CreatedAt,
	}
}

func (e *Exchange) commitLocked(kind EventKind, state State, delta SnapshotDelta) (Event, EventSink) {
	e.snap.State = state
	e.snap.UpdatedAt = time.Now().UTC()
	e.snap.Revision++
	delta.ExchangeID = e.snap.ExchangeID
	delta.State = state
	delta.UpdatedAt = e.snap.UpdatedAt
	return Event{
		EventID:       newEventID(),
		ExchangeID:    e.snap.ExchangeID,
		Revision:      e.snap.Revision,
		Kind:          kind,
		SnapshotDelta: cloneDelta(delta),
		ArtifactRefs:  artifactRefsFromDelta(delta),
		CreatedAt:     e.snap.CreatedAt,
	}, e.params.Events
}

func (e *Exchange) terminalLocked(state State, err error, kind EventKind, reason string) (Event, EventSink, context.CancelFunc) {
	return e.terminalLockedWithDelta(state, err, kind, reason, SnapshotDelta{})
}

func (e *Exchange) terminalLockedWithDelta(state State, err error, kind EventKind, reason string, delta SnapshotDelta) (Event, EventSink, context.CancelFunc) {
	if e.snap.State.Terminal() {
		return Event{}, nil, nil
	}
	e.snap.State = state
	if err != nil {
		e.snap.Error = "exchange operation failed"
	}
	if reason != "" {
		e.snap.Warnings = append(e.snap.Warnings, reason)
	}
	e.snap.UpdatedAt = time.Now().UTC()
	e.snap.Revision++
	delta.ExchangeID = e.snap.ExchangeID
	delta.State = state
	delta.UpdatedAt = e.snap.UpdatedAt
	if delta.Error == "" {
		delta.Error = e.snap.Error
	}
	if len(delta.Warnings) == 0 {
		delta.Warnings = append([]string(nil), e.snap.Warnings...)
	}
	e.doneOnce.Do(func() { close(e.done) })
	event := Event{
		EventID:       newEventID(),
		ExchangeID:    e.snap.ExchangeID,
		Revision:      e.snap.Revision,
		Kind:          kind,
		SnapshotDelta: cloneDelta(delta),
		ArtifactRefs:  artifactRefsFromDelta(delta),
		CreatedAt:     e.snap.CreatedAt,
	}
	return event, e.params.Events, e.cancel
}

func (e *Exchange) resultLocked(mutation *MutationResult, event *Event) CommandResult {
	result := CommandResult{Exchange: cloneSnapshot(e.snap), Revision: e.snap.Revision, Mutation: cloneMutationResult(mutation)}
	if event != nil && event.EventID != "" {
		copy := cloneEvent(*event)
		result.Event = &copy
	}
	return result
}

func (e *Exchange) emit(event Event)                   { emitEvent(e.params.Events, event) }
func (e *Exchange) emitTo(sink EventSink, event Event) { emitEvent(sink, event) }

func emitEvent(sink EventSink, event Event) {
	if sink != nil && event.EventID != "" {
		sink(cloneEvent(event))
	}
}

func cloneSnapshot(s Snapshot) Snapshot {
	s.Request = cloneRequestPart(s.Request)
	s.Response = cloneResponsePart(s.Response)
	s.Warnings = cloneStrings(s.Warnings)
	return s
}

func cloneRequestPart(p RequestPart) RequestPart {
	p.Envelope = p.Envelope.Clone()
	p.ArtifactRefs = cloneRefs(p.ArtifactRefs)
	return p
}
func cloneResponsePart(p ResponsePart) ResponsePart {
	p.Envelope = p.Envelope.Clone()
	p.ArtifactRefs = cloneRefs(p.ArtifactRefs)
	return p
}
func cloneRequestPartPtr(p RequestPart) *RequestPart    { c := cloneRequestPart(p); return &c }
func cloneResponsePartPtr(p ResponsePart) *ResponsePart { c := cloneResponsePart(p); return &c }
func policyPtr(p policy.Policy) *policy.Policy          { c := p; return &c }

func cloneDelta(d SnapshotDelta) SnapshotDelta {
	if d.Request != nil {
		p := cloneRequestPart(*d.Request)
		d.Request = &p
	}
	if d.Response != nil {
		p := cloneResponsePart(*d.Response)
		d.Response = &p
	}
	if d.Policy != nil {
		p := *d.Policy
		d.Policy = &p
	}
	d.Warnings = cloneStrings(d.Warnings)
	return d
}

func artifactRefsFromDelta(d SnapshotDelta) []wire.ArtifactRef {
	var refs []wire.ArtifactRef
	if d.Request != nil {
		refs = append(refs, d.Request.ArtifactRefs...)
	}
	if d.Response != nil {
		refs = append(refs, d.Response.ArtifactRefs...)
	}
	return refs
}

func cloneEvent(e Event) Event {
	e.SnapshotDelta = cloneDelta(e.SnapshotDelta)
	e.ArtifactRefs = cloneRefs(e.ArtifactRefs)
	return e
}

func cloneRefs(in []wire.ArtifactRef) []wire.ArtifactRef {
	if in == nil {
		return []wire.ArtifactRef{}
	}
	return append([]wire.ArtifactRef{}, in...)
}
func cloneStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return append([]string{}, in...)
}

func cloneMutationResult(m *MutationResult) *MutationResult {
	if m == nil {
		return nil
	}
	c := *m
	if m.DerivedArtifact != nil {
		r := *m.DerivedArtifact
		c.DerivedArtifact = &r
	}
	if m.Validation != nil {
		v := *m.Validation
		v.Errors = append([]string(nil), m.Validation.Errors...)
		v.Warnings = append([]string(nil), m.Validation.Warnings...)
		c.Validation = &v
	}
	return &c
}
