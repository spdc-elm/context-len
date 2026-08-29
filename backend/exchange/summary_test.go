package exchange

import (
	"context"
	"testing"

	"context-lens/backend/policy"
	"context-lens/backend/session"
	"context-lens/backend/wire"
)

// TestSetContextTokens covers the additive summary update seam: tokens land
// on the snapshot while the exchange is live, and a terminal exchange stays
// untouched so terminal events remain final.
func TestSetContextTokens(t *testing.T) {
	r := NewRegistry(policy.Policy{RequestGate: policy.GateHold, ResponseGate: policy.GatePass})
	e, err := r.Create(CreateParams{
		ExchangeID:      "x",
		RequestArtifact: wire.NewArtifact([]byte(`{"model":"m"}`), wire.ArtifactOptions{Stage: "request.inbound"}),
		Upstream: func(context.Context, UpstreamRequest) (UpstreamResponse, error) {
			return UpstreamResponse{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetContextTokens("missing", 5); err != ErrNotFound {
		t.Fatalf("missing exchange error = %v, want ErrNotFound", err)
	}
	if err := r.SetContextTokens("x", 128); err != nil {
		t.Fatal(err)
	}
	snap := e.Snapshot()
	if snap.Summary == nil || snap.Summary.ContextTokens == nil || *snap.Summary.ContextTokens != 128 {
		t.Fatalf("summary tokens = %+v, want 128", snap.Summary)
	}
	// A nil summary is allocated on demand.
	if _, err := r.Create(CreateParams{
		ExchangeID:      "y",
		RequestArtifact: wire.NewArtifact([]byte("{}"), wire.ArtifactOptions{Stage: "request.inbound"}),
		Upstream: func(context.Context, UpstreamRequest) (UpstreamResponse, error) {
			return UpstreamResponse{}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.SetContextTokens("y", 7); err != nil {
		t.Fatal(err)
	}
	e2, _ := r.Get("y")
	if s := e2.Snapshot(); s.Summary == nil || s.Summary.ContextTokens == nil || *s.Summary.ContextTokens != 7 {
		t.Fatalf("on-demand summary tokens = %+v", s.Summary)
	}

	// Terminal exchanges reject further summary writes.
	if _, err = e.Command(Command{ExchangeID: "x", BaseRevision: e.Snapshot().Revision, Kind: CommandDrop}); err != nil {
		t.Fatal(err)
	}
	revision := e.Snapshot().Revision
	if err := r.SetContextTokens("x", 999); err != nil {
		t.Fatalf("terminal update must be a no-op, got %v", err)
	}
	final := e.Snapshot()
	if final.Revision != revision || final.Summary == nil || *final.Summary.ContextTokens != 128 {
		t.Fatalf("terminal snapshot changed: rev=%d summary=%+v", final.Revision, final.Summary)
	}
}

// TestCreateCarriesSummarySnapshotClone guards the snapshot-copy boundary:
// the stored summary is a clone of the caller's value, so later caller
// mutations cannot rewrite history.
func TestCreateCarriesSummarySnapshotClone(t *testing.T) {
	r := NewRegistry(policy.Policy{RequestGate: policy.GateHold, ResponseGate: policy.GateHold})
	summary := &session.Summary{Model: "m", MessageCount: 2, Preview: "p", ToolNames: []string{"t"}}
	e, err := r.Create(CreateParams{
		ExchangeID:      "s",
		RequestArtifact: wire.NewArtifact([]byte("{}"), wire.ArtifactOptions{Stage: "request.inbound"}),
		Upstream: func(context.Context, UpstreamRequest) (UpstreamResponse, error) {
			return UpstreamResponse{}, nil
		},
		Summary: summary,
	})
	if err != nil {
		t.Fatal(err)
	}
	summary.ToolNames[0] = "mutated"
	snap := e.Snapshot()
	if snap.Summary == nil || snap.Summary.ToolNames[0] != "t" {
		t.Fatalf("snapshot summary shared storage with caller: %+v", snap.Summary)
	}
}
