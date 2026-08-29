package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"context-lens/backend/exchange"
	"context-lens/backend/policy"
)

// TestClientDisconnectRetainsStreamedResponse reproduces the observed harness
// behaviour: a client (Codex over the Responses protocol) aborts its SSE
// request right after the terminal event instead of waiting for the upstream
// EOF. The exchange must end cancelled, but the bytes that already streamed
// through the proxy stay inspectable as an incomplete response artifact, and
// the usage observed inside the partial capture still backfills the summary.
func TestClientDisconnectRetainsStreamedResponse(t *testing.T) {
	const sse = "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"sequence_number\":0}\n\n" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"sequence_number\":1,\"delta\":\"hello\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"id\":\"resp_partial\",\"usage\":{\"input_tokens\":7,\"output_tokens\":2}}}\n\n"

	release := make(chan struct{})
	g, _ := localGateway(t, policy.Default(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, sse)
		flusher.Flush()
		// Hold the body open like a provider whose stream has not closed
		// yet, so the client abort always wins the race against EOF.
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

	// Wait until the proxy read every record (three complete SSE records).
	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		seen := records
		mu.Unlock()
		if seen >= 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("stream records did not flow; saw %d", seen)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel() // the harness aborts the request after the terminal event
	<-done

	snapshots := g.Registry().List()
	if len(snapshots) != 1 {
		t.Fatalf("exchanges = %d, want 1", len(snapshots))
	}
	snapshot := snapshots[0]
	if snapshot.State != exchange.StateCancelled {
		t.Fatalf("state = %s, want cancelled", snapshot.State)
	}
	refs := snapshot.Response.ArtifactRefs
	if len(refs) == 0 {
		t.Fatal("cancelled exchange lost its partial response artifact")
	}
	foundTerminal := false
	for _, ref := range refs {
		if ref.Complete {
			t.Fatalf("interrupted response artifact %s must be incomplete", ref.ArtifactID)
		}
		artifact, err := g.Store().Get(context.Background(), ref.ArtifactID)
		if err != nil {
			t.Fatalf("read artifact %s: %v", ref.ArtifactID, err)
		}
		if !strings.Contains(string(artifact.Bytes()), "response.completed") {
			t.Fatalf("artifact %s does not contain the terminal event the client received", ref.ArtifactID)
		}
		foundTerminal = true
	}
	if !foundTerminal {
		t.Fatal("no retained artifact")
	}
	if snapshot.Summary == nil || snapshot.Summary.ContextTokens == nil || *snapshot.Summary.ContextTokens != 7 {
		t.Fatalf("usage from partial capture was not backfilled: %+v", snapshot.Summary)
	}
	joined := strings.Join(snapshot.Warnings, " | ")
	if !strings.Contains(joined, "partial response retained") {
		t.Fatalf("retention warning missing: %v", snapshot.Warnings)
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
