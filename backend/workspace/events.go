package workspace

import (
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"context-lens/backend/exchange"
	"context-lens/backend/wire"
)

// EventBroker is a bounded in-process event log plus fan-out hub.  Publishing
// never waits for a slow SSE client: a subscriber whose bounded queue is full
// is disconnected, and it can recover by reconnecting with Last-Event-ID while
// the event remains in the bounded history.
type EventBroker struct {
	mu          sync.Mutex
	capacity    int
	history     []exchange.Event
	subscribers map[uint64]*eventSubscriber
	nextSubID   uint64
	nextEvent   atomic.Uint64
	closed      bool
}

type eventSubscriber struct {
	ch     chan exchange.Event
	closed bool
}

// NewEventBroker creates a broker with bounded history and per-subscriber
// queues.  A non-positive capacity selects DefaultEventBuffer.
func NewEventBroker(capacity int) *EventBroker {
	if capacity <= 0 {
		capacity = DefaultEventBuffer
	}
	return &EventBroker{
		capacity:    capacity,
		history:     make([]exchange.Event, 0, capacity),
		subscribers: make(map[uint64]*eventSubscriber),
	}
}

// Publish normalises identity/timestamps, stores the metadata-only event, and
// fans it out without blocking.  The returned value is the exact normalised
// event that was retained and delivered.
func (b *EventBroker) Publish(event exchange.Event) exchange.Event {
	if b == nil {
		return event
	}
	event = normalizeEvent(event, b.nextEvent.Add(1))
	event = cloneEvent(event)

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return event
	}
	for _, retained := range b.history {
		if retained.EventID == event.EventID {
			return cloneEvent(retained)
		}
	}
	b.history = append(b.history, event)
	if len(b.history) > b.capacity {
		b.history = append([]exchange.Event(nil), b.history[len(b.history)-b.capacity:]...)
	}
	for id, subscriber := range b.subscribers {
		if subscriber.closed {
			delete(b.subscribers, id)
			continue
		}
		select {
		case subscriber.ch <- cloneEvent(event):
		default:
			// Never let a browser stall an exchange producer.  Closing here is
			// safe because publication and unsubscribe both hold b.mu.
			subscriber.closed = true
			close(subscriber.ch)
			delete(b.subscribers, id)
		}
	}
	return event
}

func cloneEvent(in exchange.Event) exchange.Event {
	out := in
	out.ArtifactRefs = append([]wire.ArtifactRef(nil), in.ArtifactRefs...)
	out.SnapshotDelta = in.SnapshotDelta
	if in.SnapshotDelta.Request != nil {
		req := *in.SnapshotDelta.Request
		req.Envelope = in.SnapshotDelta.Request.Envelope.Clone()
		req.ArtifactRefs = append([]wire.ArtifactRef(nil), in.SnapshotDelta.Request.ArtifactRefs...)
		out.SnapshotDelta.Request = &req
	}
	if in.SnapshotDelta.Response != nil {
		resp := *in.SnapshotDelta.Response
		resp.Envelope = in.SnapshotDelta.Response.Envelope.Clone()
		resp.ArtifactRefs = append([]wire.ArtifactRef(nil), in.SnapshotDelta.Response.ArtifactRefs...)
		out.SnapshotDelta.Response = &resp
	}
	out.SnapshotDelta.Warnings = append([]string(nil), in.SnapshotDelta.Warnings...)
	if in.Stream != nil {
		stream := *in.Stream
		out.Stream = &stream
	}
	return out
}
func normalizeEvent(event exchange.Event, sequence uint64) exchange.Event {
	if event.EventID == "" {
		if event.ExchangeID != "" && event.Revision != 0 {
			event.EventID = fmt.Sprintf("%s:%d", event.ExchangeID, event.Revision)
		} else {
			event.EventID = "event-" + strconv.FormatUint(sequence, 10)
		}
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	if event.SnapshotDelta.ExchangeID == "" {
		event.SnapshotDelta.ExchangeID = event.ExchangeID
	}
	if event.SnapshotDelta.UpdatedAt.IsZero() {
		event.SnapshotDelta.UpdatedAt = event.CreatedAt
	}
	// Event carries only refs.  Copying the slice prevents a producer from
	// mutating the retained history after Publish returns.
	if event.ArtifactRefs != nil {
		event.ArtifactRefs = append([]wireArtifactRef(nil), event.ArtifactRefs...)
	}
	return event
}

// Subscribe returns a channel and cancellation function.  Events retained
// after lastEventID are queued before new publications.  An unknown id means
// "from the oldest retained event"; this is deterministic after history
// compaction and avoids silently losing a reconnecting browser's view.
func (b *EventBroker) Subscribe(lastEventID string) (<-chan exchange.Event, func()) {
	if b == nil {
		ch := make(chan exchange.Event)
		close(ch)
		return ch, func() {}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		ch := make(chan exchange.Event)
		close(ch)
		return ch, func() {}
	}
	b.nextSubID++
	subID := b.nextSubID
	subscriber := &eventSubscriber{ch: make(chan exchange.Event, b.capacity)}
	start := 0
	if lastEventID != "" {
		for i, event := range b.history {
			if event.EventID == lastEventID {
				start = i + 1
				break
			}
		}
	}
	for _, event := range b.history[start:] {
		select {
		case subscriber.ch <- cloneEvent(event):
		default:
			// History is no larger than capacity, so this branch only occurs
			// if capacity was changed by a future implementation.
			break
		}
	}
	b.subscribers[subID] = subscriber
	cancel := func() { b.unsubscribe(subID) }
	return subscriber.ch, cancel
}

func (b *EventBroker) unsubscribe(id uint64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	subscriber, ok := b.subscribers[id]
	if !ok || subscriber.closed {
		return
	}
	subscriber.closed = true
	close(subscriber.ch)
	delete(b.subscribers, id)
}

// Snapshot returns a copy of retained events, primarily for tests and
// diagnostics.  It never includes body bytes because exchange.Event does not
// contain them.
func (b *EventBroker) Snapshot() []exchange.Event {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]exchange.Event, len(b.history))
	for i, event := range b.history {
		out[i] = cloneEvent(event)
	}
	return out
}

// Close disconnects every subscriber and prevents future publications.
func (b *EventBroker) Close() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for id, subscriber := range b.subscribers {
		if !subscriber.closed {
			subscriber.closed = true
			close(subscriber.ch)
		}
		delete(b.subscribers, id)
	}
}

// Len reports the retained event count.
func (b *EventBroker) Len() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.history)
}

// wireArtifactRef is an alias used solely to keep normalizeEvent's copying
// code independent of generated names.  The alias is declared here instead
// of changing exchange.Event's frozen DTO.
type wireArtifactRef = wire.ArtifactRef
