package exchange

import (
	"context"
	"context-lens/backend/policy"
	"context-lens/backend/wire"
	"testing"
	"time"
)

func TestRequestHoldAndRevision(t *testing.T) {
	called := make(chan struct{})
	r := NewRegistry(policy.Policy{RequestGate: policy.GateHold, ResponseGate: policy.GatePass})
	e, err := r.Create(CreateParams{ExchangeID: "x", RequestArtifact: wire.NewArtifact([]byte("{}"), wire.ArtifactOptions{Stage: "request.inbound"}), Upstream: func(context.Context, UpstreamRequest) (UpstreamResponse, error) {
		close(called)
		return UpstreamResponse{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if e.Snapshot().State != StateRequestHeld {
		t.Fatal("request was not held")
	}
	if _, err = e.Command(Command{ExchangeID: "x", BaseRevision: 0, Kind: CommandForwardUnchanged}); err == nil {
		t.Fatal("stale revision accepted")
	}
	res, err := e.Command(Command{ExchangeID: "x", BaseRevision: 1, Kind: CommandForwardUnchanged})
	if err != nil {
		t.Fatal(err)
	}
	if res.Revision != 2 {
		t.Fatalf("revision %d", res.Revision)
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("upstream not called")
	}
}
