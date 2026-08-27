package exchange

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"context-lens/backend/policy"
	"context-lens/backend/wire"
)

func requestArtifact(body string) wire.BodyArtifact {
	return wire.NewArtifact([]byte(body), wire.ArtifactOptions{
		ArtifactID:  "request-inbound",
		Stage:       wire.StageRequestInbound,
		Direction:   wire.DirectionInbound,
		ContentType: "application/json",
	})
}

func response(body string, status int) UpstreamResponse {
	return UpstreamResponse{
		Envelope: wire.NewResponseEnvelope(status, http.Header{"Content-Type": {"application/json"}}, nil, time.Now(), time.Now()),
		Artifact: wire.NewArtifact([]byte(body), wire.ArtifactOptions{
			ArtifactID:  "response-upstream",
			Stage:       wire.StageResponseUpstream,
			Direction:   wire.DirectionUpstream,
			ContentType: "application/json",
		}),
	}
}

func waitExchange(t *testing.T, e *Exchange) Snapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := e.Wait(ctx)
	if err != nil {
		t.Fatalf("wait exchange: %v", err)
	}
	return s
}

func assertNoCall[T any](t *testing.T, ch <-chan T) {
	t.Helper()
	select {
	case value := <-ch:
		t.Fatalf("unexpected callback: %#v", value)
	case <-time.After(40 * time.Millisecond):
	}
}

func TestPassPassForwardsOpaqueBytesAndResponse(t *testing.T) {
	requestBody := `{"model":"provider-model","unknown":{"n":1}}`
	upstreamCalls := make(chan UpstreamRequest, 1)
	downstreamCalls := make(chan DownstreamResponse, 1)
	r := NewRegistry(policy.Default())
	e, err := r.Create(CreateParams{
		ExchangeID:      "pass-pass",
		Protocol:        "responses",
		RequestArtifact: requestArtifact(requestBody),
		Upstream: func(ctx context.Context, req UpstreamRequest) (UpstreamResponse, error) {
			upstreamCalls <- req
			return response(`{"id":"r1","output":[{"type":"unknown"}]}`, 200), nil
		},
		Downstream: func(_ context.Context, resp DownstreamResponse) error {
			downstreamCalls <- resp
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	final := waitExchange(t, e)
	if final.State != StateCompleted {
		t.Fatalf("state = %s", final.State)
	}
	gotReq := <-upstreamCalls
	if string(gotReq.Artifact.Bytes()) != requestBody {
		t.Fatalf("request bytes changed: %q", gotReq.Artifact.Bytes())
	}
	gotResp := <-downstreamCalls
	if string(gotResp.Artifact.Bytes()) != `{"id":"r1","output":[{"type":"unknown"}]}` {
		t.Fatalf("response bytes changed: %q", gotResp.Artifact.Bytes())
	}
	if gotResp.Envelope.Status != 200 {
		t.Fatalf("response status = %d", gotResp.Envelope.Status)
	}
}

func TestRequestHoldDoesNotCallUpstreamUntilForward(t *testing.T) {
	called := make(chan UpstreamRequest, 1)
	r := NewRegistry(policy.Policy{RequestGate: policy.GateHold, ResponseGate: policy.GatePass})
	e, err := r.Create(CreateParams{
		ExchangeID:      "request-hold",
		RequestArtifact: requestArtifact("opaque"),
		Upstream: func(_ context.Context, req UpstreamRequest) (UpstreamResponse, error) {
			called <- req
			return response("ok", 200), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.Snapshot().State != StateRequestHeld {
		t.Fatalf("state = %s", e.Snapshot().State)
	}
	assertNoCall(t, called)
	base := e.Snapshot().Revision
	result, err := e.Command(Command{ExchangeID: "request-hold", BaseRevision: base, Kind: CommandForwardUnchanged})
	if err != nil {
		t.Fatal(err)
	}
	if result.Exchange.State != StateUpstreamRunning {
		t.Fatalf("forward state = %s", result.Exchange.State)
	}
	final := waitExchange(t, e)
	if final.State != StateCompleted {
		t.Fatalf("final state = %s", final.State)
	}
	if got := <-called; string(got.Artifact.Bytes()) != "opaque" {
		t.Fatalf("forwarded body = %q", got.Artifact.Bytes())
	}
}

func TestRequestEditForwardCreatesDerivedArtifact(t *testing.T) {
	inbound := requestArtifact(`{"model":"old","x":1}`)
	called := make(chan UpstreamRequest, 1)
	r := NewRegistry(policy.Default())
	e, err := r.Create(CreateParams{
		ExchangeID:      "edit-forward",
		Policy:          policy.Policy{RequestGate: policy.GateHold, ResponseGate: policy.GatePass},
		RequestArtifact: inbound,
		Upstream: func(_ context.Context, req UpstreamRequest) (UpstreamResponse, error) {
			called <- req
			return response("ok", 200), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	base := inbound.Ref()
	result, err := e.Command(Command{
		ExchangeID: "edit-forward", BaseRevision: e.Snapshot().Revision, Kind: CommandForwardEdited,
		Mutation: &MutationInput{RawReplacement: `{"model":"new","x":1}`, BaseArtifactID: base.ArtifactID, BaseSHA256: base.SHA256},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Mutation == nil || result.Mutation.DerivedArtifact == nil {
		t.Fatalf("missing mutation result: %#v", result.Mutation)
	}
	if result.Mutation.DerivedArtifact.ArtifactID == base.ArtifactID {
		t.Fatal("derived artifact reused inbound id")
	}
	if string(inbound.Bytes()) != `{"model":"old","x":1}` {
		t.Fatal("inbound artifact was mutated")
	}
	_ = waitExchange(t, e)
	got := <-called
	if string(got.Artifact.Bytes()) != `{"model":"new","x":1}` {
		t.Fatalf("edited body = %q", got.Artifact.Bytes())
	}
	refs := e.Snapshot().Request.ArtifactRefs
	if len(refs) != 2 || refs[0].ArtifactID != base.ArtifactID || refs[1].ArtifactID != result.Mutation.DerivedArtifact.ArtifactID {
		t.Fatalf("request refs = %#v", refs)
	}
}

func TestManualResponseNeverCallsUpstream(t *testing.T) {
	upstream := make(chan struct{}, 1)
	downstream := make(chan DownstreamResponse, 1)
	r := NewRegistry(policy.Policy{RequestGate: policy.GateHold, ResponseGate: policy.GateHold})
	e, err := r.Create(CreateParams{
		ExchangeID:      "manual",
		RequestArtifact: requestArtifact("request"),
		Upstream: func(context.Context, UpstreamRequest) (UpstreamResponse, error) {
			upstream <- struct{}{}
			return response("wrong", 500), nil
		},
		Downstream: func(_ context.Context, resp DownstreamResponse) error {
			downstream <- resp
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := e.Command(Command{ExchangeID: "manual", BaseRevision: e.Snapshot().Revision, Kind: CommandManualResponse, RawResponse: `{"id":"operator"}`, ContentType: "application/json"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Exchange.State != StateCompleted {
		t.Fatalf("manual state = %s", result.Exchange.State)
	}
	assertNoCall(t, upstream)
	got := <-downstream
	if string(got.Artifact.Bytes()) != `{"id":"operator"}` || got.Envelope.Status != 200 {
		t.Fatalf("manual response = %#v body=%q", got.Envelope, got.Artifact.Bytes())
	}
}

func TestDropRequestHoldNeverCallsUpstreamOrDownstream(t *testing.T) {
	upstream := make(chan struct{}, 1)
	downstream := make(chan struct{}, 1)
	r := NewRegistry(policy.Policy{RequestGate: policy.GateHold, ResponseGate: policy.GatePass})
	e, err := r.Create(CreateParams{
		ExchangeID:      "drop-request",
		RequestArtifact: requestArtifact("request"),
		Upstream: func(context.Context, UpstreamRequest) (UpstreamResponse, error) {
			upstream <- struct{}{}
			return response("wrong", 200), nil
		},
		Downstream: func(context.Context, DownstreamResponse) error {
			downstream <- struct{}{}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := e.Command(Command{ExchangeID: "drop-request", BaseRevision: e.Snapshot().Revision, Kind: CommandDrop})
	if err != nil {
		t.Fatal(err)
	}
	if result.Exchange.State != StateDropped {
		t.Fatalf("state = %s", result.Exchange.State)
	}
	assertNoCall(t, upstream)
	assertNoCall(t, downstream)
}

func TestResponseHoldReleaseUnchangedAndEdited(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind CommandKind
		body string
	}{
		{name: "unchanged", kind: CommandReleaseUnchanged, body: `{"answer":"raw"}`},
		{name: "edited", kind: CommandReleaseEdited, body: `{"answer":"edited"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := make(chan DownstreamResponse, 1)
			upstreamBody := `{"answer":"raw"}`
			r := NewRegistry(policy.Policy{RequestGate: policy.GatePass, ResponseGate: policy.GateHold})
			e, err := r.Create(CreateParams{
				ExchangeID:      "response-hold-" + tc.name,
				RequestArtifact: requestArtifact("request"),
				Upstream: func(context.Context, UpstreamRequest) (UpstreamResponse, error) {
					return response(upstreamBody, 201), nil
				},
				Downstream: func(_ context.Context, resp DownstreamResponse) error { called <- resp; return nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			held := waitForState(t, e, StateResponseHeld)
			if len(held.Response.ArtifactRefs) != 1 {
				t.Fatalf("response refs = %#v", held.Response.ArtifactRefs)
			}
			cmd := Command{ExchangeID: e.Snapshot().ExchangeID, BaseRevision: held.Revision, Kind: tc.kind}
			if tc.kind == CommandReleaseEdited {
				ref := held.Response.ArtifactRefs[0]
				cmd.Mutation = &MutationInput{RawReplacement: tc.body, BaseArtifactID: ref.ArtifactID, BaseSHA256: ref.SHA256}
			}
			result, err := e.Command(cmd)
			if err != nil {
				t.Fatal(err)
			}
			if result.Exchange.State != StateCompleted {
				t.Fatalf("result state = %s", result.Exchange.State)
			}
			final := waitExchange(t, e)
			if final.State != StateCompleted {
				t.Fatalf("final state = %s", final.State)
			}
			got := <-called
			if string(got.Artifact.Bytes()) != tc.body || got.Envelope.Status != 201 {
				t.Fatalf("released response status=%d body=%q", got.Envelope.Status, got.Artifact.Bytes())
			}
			if tc.kind == CommandReleaseEdited && len(final.Response.ArtifactRefs) != 2 {
				t.Fatalf("edited response refs = %#v", final.Response.ArtifactRefs)
			}
		})
	}
}

func TestResponseHoldReplaceAndDrop(t *testing.T) {
	for _, kind := range []CommandKind{CommandReplaceResponse, CommandDrop} {
		t.Run(string(kind), func(t *testing.T) {
			called := make(chan DownstreamResponse, 1)
			r := NewRegistry(policy.Policy{ResponseGate: policy.GateHold})
			e, err := r.Create(CreateParams{
				ExchangeID: "response-" + string(kind), Protocol: "responses", RequestArtifact: requestArtifact("request"),
				Upstream: func(context.Context, UpstreamRequest) (UpstreamResponse, error) {
					return response("upstream", 200), nil
				},
				Downstream: func(_ context.Context, resp DownstreamResponse) error { called <- resp; return nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			held := waitForState(t, e, StateResponseHeld)
			cmd := Command{ExchangeID: e.Snapshot().ExchangeID, BaseRevision: held.Revision, Kind: kind}
			if kind == CommandReplaceResponse {
				cmd.RawResponse = `{"id":"resp_operator","object":"response","status":"completed","output":[]}`
			}
			result, err := e.Command(cmd)
			if err != nil {
				t.Fatal(err)
			}
			if kind == CommandReplaceResponse {
				if result.Exchange.State != StateCompleted {
					t.Fatalf("replace state = %s", result.Exchange.State)
				}
				got := <-called
				if string(got.Artifact.Bytes()) != `{"id":"resp_operator","object":"response","status":"completed","output":[]}` {
					t.Fatalf("replacement body = %q", got.Artifact.Bytes())
				}
			} else {
				if result.Exchange.State != StateDropped {
					t.Fatalf("drop state = %s", result.Exchange.State)
				}
				assertNoCall(t, called)
			}
		})
	}
}

func TestRevisionConflictDoesNotChangeState(t *testing.T) {
	var calls atomic.Int32
	r := NewRegistry(policy.Default())
	e, err := r.Create(CreateParams{
		ExchangeID: "revision", RequestArtifact: requestArtifact("request"),
		Upstream: func(context.Context, UpstreamRequest) (UpstreamResponse, error) {
			calls.Add(1)
			return response("ok", 200), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	base := e.Snapshot().Revision
	if _, err := e.Command(Command{ExchangeID: "revision", BaseRevision: base, Kind: CommandAbort}); err != nil {
		t.Fatal(err)
	}
	before := e.Snapshot()
	_, err = e.Command(Command{ExchangeID: "revision", BaseRevision: base, Kind: CommandDrop})
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("error = %v, want revision conflict", err)
	}
	after := e.Snapshot()
	if after.State != before.State || after.Revision != before.Revision {
		t.Fatalf("stale command changed snapshot: before=%+v after=%+v", before, after)
	}
	_ = calls.Load() // Upstream may already have started; cancellation behavior is covered separately.
}

func TestUpstreamErrorAndDownstreamErrorAreTerminal(t *testing.T) {
	r := NewRegistry(policy.Default())
	upstreamErr, err := r.Create(CreateParams{
		ExchangeID: "upstream-error", RequestArtifact: requestArtifact("request"),
		Upstream: func(context.Context, UpstreamRequest) (UpstreamResponse, error) {
			return UpstreamResponse{}, errors.New("boom")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	failed := waitExchange(t, upstreamErr)
	if failed.State != StateFailed || failed.Error != "exchange operation failed" {
		t.Fatalf("upstream failure = %+v", failed)
	}

	downstreamErr, err := r.Create(CreateParams{
		ExchangeID: "downstream-error", RequestArtifact: requestArtifact("request"),
		Upstream:   func(context.Context, UpstreamRequest) (UpstreamResponse, error) { return response("ok", 200), nil },
		Downstream: func(context.Context, DownstreamResponse) error { return errors.New("write failed") },
	})
	if err != nil {
		t.Fatal(err)
	}
	failed = waitExchange(t, downstreamErr)
	if failed.State != StateFailed || failed.Error != "exchange operation failed" {
		t.Fatalf("downstream failure = %+v", failed)
	}
}

func TestAbortCancelsUpstreamAndPreventsDownstream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	cancelled := make(chan struct{})
	downstream := make(chan struct{}, 1)
	r := NewRegistry(policy.Default())
	e, err := r.Create(CreateParams{
		ExchangeID: "cancel", Context: ctx, RequestArtifact: requestArtifact("request"),
		Upstream: func(ctx context.Context, _ UpstreamRequest) (UpstreamResponse, error) {
			close(started)
			<-ctx.Done()
			close(cancelled)
			return UpstreamResponse{}, ctx.Err()
		},
		Downstream: func(context.Context, DownstreamResponse) error { downstream <- struct{}{}; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := e.Command(Command{ExchangeID: "cancel", BaseRevision: e.Snapshot().Revision, Kind: CommandAbort}); err != nil {
		t.Fatal(err)
	}
	final := waitExchange(t, e)
	if final.State != StateCancelled {
		t.Fatalf("state = %s", final.State)
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("upstream was not cancelled")
	}
	assertNoCall(t, downstream)
}

func waitForState(t *testing.T, e *Exchange, want State) Snapshot {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		s := e.Snapshot()
		if s.State == want {
			return s
		}
		select {
		case <-deadline:
			t.Fatalf("state = %s, want %s", s.State, want)
			return Snapshot{}
		case <-time.After(time.Millisecond):
		}
	}
}
