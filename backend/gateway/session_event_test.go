package gateway

import (
	"context-lens/backend/exchange"
	"context-lens/backend/session"
	"testing"
)

func TestSessionRemovedEventCarriesSessionIdentity(t *testing.T) {
	g := &Gateway{subs: make(map[uint64]func(exchange.Event))}
	ch := make(chan exchange.Event, 1)
	g.Subscribe(func(e exchange.Event) { ch <- e })
	g.emitEvent(exchange.Event{Kind: exchange.EventSessionRemoved, SnapshotDelta: exchange.SnapshotDelta{Session: &session.Assignment{SessionID: "sess-1"}}})
	got := <-ch
	if got.SnapshotDelta.Session == nil || got.SnapshotDelta.Session.SessionID != "sess-1" {
		t.Fatalf("identity missing: %+v", got)
	}
}
