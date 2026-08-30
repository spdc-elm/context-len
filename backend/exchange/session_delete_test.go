package exchange

import (
	"context"
	"errors"
	"testing"

	"context-lens/backend/policy"
	"context-lens/backend/session"
	"context-lens/backend/wire"
)

func TestDeleteSessionRejectsActiveThenRemovesWholeTree(t *testing.T) {
	r := NewRegistry(policy.Policy{RequestGate: policy.GateHold, ResponseGate: policy.GatePass})
	assignment := &session.Assignment{SessionID: "session-one", Root: true}
	e, err := r.Create(CreateParams{
		ExchangeID: "active", Session: assignment,
		RequestArtifact: wire.NewArtifact([]byte("{}"), wire.ArtifactOptions{Stage: "request.inbound"}),
		Upstream:        func(context.Context, UpstreamRequest) (UpstreamResponse, error) { return UpstreamResponse{}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Restore(Snapshot{ExchangeID: "old", State: StateCompleted, Session: assignment}); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteSession("session-one"); !errors.Is(err, ErrSessionActive) {
		t.Fatalf("delete active = %v", err)
	}
	if _, ok := r.Get("old"); !ok {
		t.Fatal("conflict partially deleted session")
	}
	if _, err := e.Command(Command{ExchangeID: "active", BaseRevision: e.Snapshot().Revision, Kind: CommandAbort}); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteSession("session-one"); err != nil {
		t.Fatal(err)
	}
	if got := r.List(); len(got) != 0 {
		t.Fatalf("session tree remains: %+v", got)
	}
	if err := r.DeleteSession("session-one"); err != nil {
		t.Fatal(err)
	}
}
