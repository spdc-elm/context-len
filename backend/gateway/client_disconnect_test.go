package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"context-lens/backend/exchange"
	"context-lens/backend/persistence"
	"context-lens/backend/policy"
	"context-lens/backend/transport"
)

// sseWithoutTerminal is a Responses stream whose terminal record has not
// arrived yet: a client disconnect here is a genuine mid-stream interruption.
const sseWithoutTerminal = "event: response.created\n" +
	"data: {\"type\":\"response.created\",\"sequence_number\":0}\n\n" +
	"event: response.output_text.delta\n" +
	"data: {\"type\":\"response.output_text.delta\",\"sequence_number\":1,\"delta\":\"hello\"}\n\n"

// sseWithTerminal ends with the protocol terminal record and carries usage
// inside it, like a real Responses stream.
const sseWithTerminal = sseWithoutTerminal +
	"event: response.completed\n" +
	"data: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"id\":\"resp_partial\",\"usage\":{\"input_tokens\":7,\"output_tokens\":2}}}\n\n"

// TestDisconnectBeforeTerminalRetainsPartialResponse disconnects the client
// while the stream is still flowing. The exchange is cancelled and the bytes
// observed so far stay inspectable as incomplete artifacts.
func TestDisconnectBeforeTerminalRetainsPartialResponse(t *testing.T) {
	release := make(chan struct{})
	g, _ := localGateway(t, policy.Default(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, sseWithoutTerminal)
		flusher.Flush()
		<-release
	}))
	defer close(release)

	var mu sync.Mutex
	records := 0
	unsubscribe := g.SubscribeEvents(func(event exchange.Event) {
		if event.Kind == exchange.EventStreamEvent {
			mu.Lock()
			records++
			mu.Unlock()
		}
	})
	defer unsubscribe()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.ServeHTTP(httptest.NewRecorder(), requestFor(`{"model":"partial","stream":true}`).WithContext(ctx))
	}()

	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		seen := records
		mu.Unlock()
		if seen >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("stream records did not flow; saw %d", seen)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel() // client disconnects before any terminal record
	<-done

	snapshot := g.Registry().List()[0]
	if snapshot.State != exchange.StateCancelled {
		t.Fatalf("state = %s, want cancelled", snapshot.State)
	}
	refs := snapshot.Response.ArtifactRefs
	if len(refs) == 0 {
		t.Fatal("cancelled exchange lost its partial response artifact")
	}
	for _, ref := range refs {
		if ref.Complete {
			t.Fatalf("interrupted response artifact %s must be incomplete", ref.ArtifactID)
		}
		artifact, err := g.Store().Get(context.Background(), ref.ArtifactID)
		if err != nil {
			t.Fatalf("read artifact %s: %v", ref.ArtifactID, err)
		}
		if !strings.Contains(string(artifact.Bytes()), "response.output_text.delta") {
			t.Fatalf("artifact %s does not contain the streamed prefix", ref.ArtifactID)
		}
	}
	if joined := strings.Join(snapshot.Warnings, " | "); !strings.Contains(joined, "partial response retained") {
		t.Fatalf("retention warning missing: %v", snapshot.Warnings)
	}
}

// TestTerminalDeliveredThenDisconnectDrainsToComplete reproduces the harness
// lifecycle: the client aborts its SSE request right after the terminal
// record instead of waiting for the upstream EOF. The gateway drains the
// remaining upstream leg, so the exchange completes with a full artifact and
// the usage observed inside the terminal record backfills the summary.
func TestTerminalDeliveredThenDisconnectDrainsToComplete(t *testing.T) {
	upstreamEOF := make(chan struct{})
	g, _ := localGateway(t, policy.Default(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, sseWithTerminal)
		flusher.Flush()
		// Hold the body open briefly so the client abort always wins the
		// race against EOF: only a real drain can complete the artifact.
		time.Sleep(300 * time.Millisecond)
		close(upstreamEOF)
	}))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var received sync.Map
	go func() {
		defer close(done)
		g.ServeHTTP(httptest.NewRecorder(), requestFor(`{"model":"drain","stream":true}`).WithContext(ctx))
	}()

	// Client reads until it has seen the terminal record, then aborts like
	// a harness that closes its connection on response.completed.
	unsubscribe := g.SubscribeEvents(func(event exchange.Event) {
		if event.Kind == exchange.EventStreamEvent && event.Stream != nil {
			received.Store(event.Stream.Ordinal, event.Stream.Name)
		}
	})
	defer unsubscribe()
	deadline := time.After(5 * time.Second)
	for {
		_, sawTerminal := received.Load(2)
		if sawTerminal {
			break
		}
		select {
		case <-deadline:
			t.Fatal("terminal record never reached the client")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
	<-upstreamEOF

	snapshot := g.Registry().List()[0]
	if snapshot.State != exchange.StateCompleted {
		t.Fatalf("state = %s, want completed after post-terminal drain", snapshot.State)
	}
	var upstream []byte
	for _, ref := range snapshot.Response.ArtifactRefs {
		if ref.Stage != "response.upstream" {
			continue
		}
		if !ref.Complete {
			t.Fatalf("drained response artifact %s must be complete", ref.ArtifactID)
		}
		artifact, err := g.Store().Get(context.Background(), ref.ArtifactID)
		if err != nil {
			t.Fatalf("read artifact %s: %v", ref.ArtifactID, err)
		}
		upstream = artifact.Bytes()
	}
	if upstream == nil {
		t.Fatal("completed exchange lost its response.upstream artifact")
	}
	if !strings.Contains(string(upstream), "response.completed") {
		t.Fatal("drained artifact does not contain the terminal record")
	}
	if snapshot.Summary == nil || snapshot.Summary.ContextTokens == nil || *snapshot.Summary.ContextTokens != 7 {
		t.Fatalf("usage from the terminal record was not backfilled: %+v", snapshot.Summary)
	}
	if joined := strings.Join(snapshot.Warnings, " | "); strings.Contains(joined, "drain timed out") {
		t.Fatalf("unexpected drain timeout warning: %v", snapshot.Warnings)
	}
}

// TestDrainTimeoutCompletesWithIncompleteArtifact covers an upstream that
// never closes its stream after the terminal record. The drain is bounded,
// and the exchange still completes because the terminal record was delivered;
// the artifact stays incomplete with an explicit warning.
func TestDrainTimeoutCompletesWithIncompleteArtifact(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, sseWithTerminal)
		flusher.Flush()
		<-release
	}))
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		upstream.Close()
		t.Fatal(err)
	}
	tr, err := transport.New(transport.Config{BaseURL: upstreamURL})
	if err != nil {
		upstream.Close()
		t.Fatal(err)
	}
	g, err := New(Config{
		Upstream:             tr,
		CaptureMode:          CaptureModeCapture,
		InitialPolicy:        policy.Default(),
		ResponseDrainTimeout: 150 * time.Millisecond,
		StoreConfig: persistence.Config{
			MaxArtifactBytes: 1 << 20,
			MaxTotalBytes:    16 << 20,
			MaxMemoryBytes:   16 << 20,
		},
	})
	if err != nil {
		upstream.Close()
		t.Fatal(err)
	}
	// Release the upstream handler before closing its server, or Close
	// blocks waiting for the deliberately hung response.
	t.Cleanup(func() {
		close(release)
		_ = g.Store().Close()
		upstream.CloseClientConnections()
		upstream.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var received sync.Map
	go func() {
		defer close(done)
		g.ServeHTTP(httptest.NewRecorder(), requestFor(`{"model":"timeout","stream":true}`).WithContext(ctx))
	}()
	unsubscribe := g.SubscribeEvents(func(event exchange.Event) {
		if event.Kind == exchange.EventStreamEvent && event.Stream != nil {
			received.Store(event.Stream.Ordinal, event.Stream.Name)
		}
	})
	defer unsubscribe()
	deadline := time.After(5 * time.Second)
	for {
		_, sawTerminal := received.Load(2)
		if sawTerminal {
			break
		}
		select {
		case <-deadline:
			t.Fatal("terminal record never reached the client")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done

	snapshot := g.Registry().List()[0]
	if snapshot.State != exchange.StateCompleted {
		t.Fatalf("state = %s, want completed: the terminal record was delivered", snapshot.State)
	}
	var sawIncompleteArtifact bool
	for _, ref := range snapshot.Response.ArtifactRefs {
		if ref.Stage != "response.upstream" {
			continue
		}
		sawIncompleteArtifact = true
		if ref.Complete {
			t.Fatalf("artifact %s must stay incomplete after a drain timeout", ref.ArtifactID)
		}
		artifact, err := g.Store().Get(context.Background(), ref.ArtifactID)
		if err != nil {
			t.Fatalf("read artifact %s: %v", ref.ArtifactID, err)
		}
		if !strings.Contains(string(artifact.Bytes()), "response.completed") {
			t.Fatal("artifact does not contain the delivered terminal record")
		}
	}
	if !sawIncompleteArtifact {
		t.Fatal("drain-timeout exchange lost its response artifact")
	}
	if joined := strings.Join(snapshot.Warnings, " | "); !strings.Contains(joined, "drain timed out") {
		t.Fatalf("drain timeout warning missing: %v", snapshot.Warnings)
	}
	if snapshot.Summary == nil || snapshot.Summary.ContextTokens == nil || *snapshot.Summary.ContextTokens != 7 {
		t.Fatalf("usage from the delivered terminal record was not backfilled: %+v", snapshot.Summary)
	}
}

// TestHeldResponseInterruptedRetainsUpstreamPrefix covers the hold/pass leg:
// when the client disconnects while the gateway is still capturing the
// upstream response, the captured prefix stays available for review even
// though nothing was delivered downstream.
func TestHeldResponseInterruptedRetainsUpstreamPrefix(t *testing.T) {
	const prefix = "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":11}}}\n\n"
	release := make(chan struct{})
	hold := policy.Policy{RequestGate: policy.GatePass, ResponseGate: policy.GateHold}
	g, _ := localGateway(t, hold, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, prefix)
		flusher.Flush()
		<-release
	}))
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	records := 0
	unsubscribe := g.SubscribeEvents(func(event exchange.Event) {
		if event.Kind == exchange.EventStreamEvent {
			mu.Lock()
			records++
			mu.Unlock()
		}
	})
	defer unsubscribe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.ServeHTTP(httptest.NewRecorder(), requestFor(`{"model":"held","stream":true}`).WithContext(ctx))
	}()

	// Wait until the gateway actually read the upstream prefix.
	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		seen := records
		mu.Unlock()
		if seen >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("upstream prefix was never read; records=%d", seen)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done

	snapshot := g.Registry().List()[0]
	if snapshot.State != exchange.StateCancelled {
		t.Fatalf("state = %s, want cancelled", snapshot.State)
	}
	refs := snapshot.Response.ArtifactRefs
	if len(refs) == 0 {
		t.Fatal("cancelled held exchange lost its captured upstream prefix")
	}
	for _, ref := range refs {
		if ref.Stage != "response.upstream" {
			t.Fatalf("held leg must retain only the upstream stage, got %s", ref.Stage)
		}
		artifact, err := g.Store().Get(context.Background(), ref.ArtifactID)
		if err != nil {
			t.Fatalf("read artifact %s: %v", ref.ArtifactID, err)
		}
		if !strings.Contains(string(artifact.Bytes()), "message_start") {
			t.Fatalf("artifact %s lost the captured prefix", ref.ArtifactID)
		}
	}
}
