package gateway

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"context-lens/backend/exchange"
	"context-lens/backend/inspection"
)

// streamTap observes response bytes as a copy while the response body streams
// toward capture and the downstream client.  It never rewrites, delays, or
// aggregates transport bytes: the scanner consumes a copy of every chunk that
// already flowed through, and the emitted workspace events are display-only
// projections of those copies.  It additionally notes the protocol terminal
// record's position so the direct streaming path can distinguish a client
// disconnect after full delivery from a mid-stream interruption.
type streamTap struct {
	gateway    *Gateway
	exchangeID string
	protocol   inspection.Protocol
	scanner    inspection.StreamScanner
	emitted    atomic.Int64

	// terminalEnd is the byte offset just past the protocol terminal record;
	// zero until a terminal record is observed on the upstream leg.  Written
	// from the streaming goroutine only (feed runs inside Read).
	terminalEnd int64
	// terminalDelivered is set once the terminal record's bytes were written
	// toward the downstream client.
	terminalDelivered atomic.Bool
}

// feed consumes one observed chunk and emits a workspace stream event for
// every SSE record that became complete.
func (t *streamTap) feed(chunk []byte) {
	if t == nil || len(chunk) == 0 {
		return
	}
	for _, record := range t.scanner.Write(chunk) {
		t.emit(record)
		if t.terminalEnd == 0 && record.Complete && inspection.IsTerminalStreamRecord(t.protocol, record) {
			t.terminalEnd = int64(record.Span.End)
		}
	}
}

// finish flushes an unterminated trailing record; call once after the body
// reaches EOF or is closed.
func (t *streamTap) finish() {
	if t == nil {
		return
	}
	for _, record := range t.scanner.Flush() {
		t.emit(record)
	}
}

func (t *streamTap) emit(record inspection.SSEEvent) {
	t.gateway.emitStreamEvent(t.exchangeID, record)
}

// markTerminalDelivered records that the client received bytesWritten bytes,
// marking the terminal record as delivered once the count covers it.
func (t *streamTap) markTerminalDelivered(bytesWritten int64) {
	if t == nil || t.terminalDelivered.Load() {
		return
	}
	if t.terminalEnd > 0 && bytesWritten >= t.terminalEnd {
		t.terminalDelivered.Store(true)
	}
}

// isTerminalDelivered reports whether the protocol terminal record has been
// written toward the client. Safe from any goroutine.
func (t *streamTap) isTerminalDelivered() bool {
	if t == nil {
		return false
	}
	return t.terminalDelivered.Load()
}

// tapReadCloser interposes the response body so every read is observed by the
// tap before the bytes continue into capture and streaming.  The wrapped
// reader is returned unchanged; nothing is decoded or re-encoded here.
type tapReadCloser struct {
	inner    io.ReadCloser
	tap      *streamTap
	finished bool
}

func (r *tapReadCloser) Read(p []byte) (int, error) {
	n, err := r.inner.Read(p)
	if n > 0 {
		r.tap.feed(p[:n])
	}
	if err != nil && !r.finished {
		r.finished = true
		r.tap.finish()
	}
	return n, err
}

func (r *tapReadCloser) Close() error {
	if !r.finished {
		r.finished = true
		r.tap.finish()
	}
	return r.inner.Close()
}

// attachStreamTap wraps resp.Body with an observing reader when the upstream
// response is an event stream.  Responses with other content types pass
// through untouched: their bodies are not SSE records and the complete-body
// inspection remains the only projection.  The returned tap (nil for non-SSE
// bodies) tracks the protocol terminal record for the direct streaming path.
func (g *Gateway) attachStreamTap(exchangeID string, respBody io.ReadCloser, contentType string, protocol string) (io.ReadCloser, *streamTap) {
	if g == nil || respBody == nil {
		return respBody, nil
	}
	if !strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return respBody, nil
	}
	tap := &streamTap{gateway: g, exchangeID: exchangeID, protocol: inspection.Protocol(protocol)}
	return &tapReadCloser{inner: respBody, tap: tap}, tap
}

// emitStreamEvent publishes one observed SSE record to workspace subscribers.
// The event is metadata/projection only: revision stays zero because a stream
// observation never commits an exchange revision, and the artifact bytes stay
// the only wire authority.
func (g *Gateway) emitStreamEvent(exchangeID string, record inspection.SSEEvent) {
	if g == nil || exchangeID == "" {
		return
	}
	event := exchange.Event{
		EventID:       fmt.Sprintf("%s:stream:%d", exchangeID, record.Index),
		ExchangeID:    exchangeID,
		Kind:          exchange.EventStreamEvent,
		SnapshotDelta: exchange.SnapshotDelta{ExchangeID: exchangeID},
		CreatedAt:     time.Now().UTC(),
		Stream: &exchange.StreamEvent{
			Ordinal:   record.Index,
			Name:      record.Name,
			ID:        record.ID,
			Data:      record.Data,
			Complete:  record.Complete,
			ByteStart: int64(record.Span.Start),
			ByteEnd:   int64(record.Span.End),
		},
	}
	if g.observer != nil {
		g.observer(event)
	}
	g.subMu.RLock()
	callbacks := make([]func(exchange.Event), 0, len(g.subs))
	for _, callback := range g.subs {
		callbacks = append(callbacks, callback)
	}
	g.subMu.RUnlock()
	for _, callback := range callbacks {
		callback(event)
	}
}

// streamEventID is retained for diagnostics and tests: the deterministic id
// of the workspace event for one observed stream record.
func streamEventID(exchangeID string, ordinal int) string {
	return exchangeID + ":stream:" + strconv.Itoa(ordinal)
}
