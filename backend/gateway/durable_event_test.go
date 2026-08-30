package gateway

import (
	"context"
	"testing"

	"context-lens/backend/catalog"
	"context-lens/backend/exchange"
	"context-lens/backend/policy"
)

func TestEventSinkSkipsDurableStreamEvents(t *testing.T) {
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	const exchangeID = "ex-stream-persistence"
	registry := exchange.NewRegistry(policy.Default())
	if err := registry.Restore(exchange.Snapshot{ExchangeID: exchangeID, Protocol: "responses"}); err != nil {
		t.Fatal(err)
	}
	var observed []exchange.EventKind
	g := &Gateway{
		catalog:  cat,
		registry: registry,
		observer: func(event exchange.Event) { observed = append(observed, event.Kind) },
	}

	g.eventSink(exchange.Event{ExchangeID: exchangeID, Kind: exchange.EventStreamEvent, Revision: 1})
	if got := eventRows(t, cat); got != 0 {
		t.Fatalf("stream event persisted %d catalog rows, want 0", got)
	}
	if err := g.DurableError(); err != nil {
		t.Fatalf("stream event set durable error: %v", err)
	}
	if len(observed) != 1 || observed[0] != exchange.EventStreamEvent {
		t.Fatalf("stream observer got %#v, want one stream event", observed)
	}

	g.eventSink(exchange.Event{ExchangeID: exchangeID, Kind: exchange.EventCompleted, Revision: 2})
	if got := eventRows(t, cat); got != 1 {
		t.Fatalf("durable event rows=%d, want 1 after non-stream event", got)
	}
	if err := g.DurableError(); err != nil {
		t.Fatalf("non-stream event set durable error: %v", err)
	}
	if len(observed) != 2 || observed[1] != exchange.EventCompleted {
		t.Fatalf("observer got %#v, want stream and completed events", observed)
	}
}

func eventRows(t *testing.T, cat *catalog.Catalog) int {
	t.Helper()
	var rows int
	if err := cat.DB().QueryRowContext(context.Background(), "SELECT COUNT(*) FROM events").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	return rows
}
