package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"context-lens/backend/exchange"
	"context-lens/backend/policy"
)

// collectStreamEvents subscribes to the gateway event fan-out and gathers
// every stream observation until stop is closed.
func collectStreamEvents(g *Gateway) (events chan exchange.Event, stop func()) {
	out := make(chan exchange.Event, 256)
	cancel := g.SubscribeEvents(func(event exchange.Event) {
		if event.Kind == exchange.EventStreamEvent {
			select {
			case out <- event:
			default:
			}
		}
	})
	return out, cancel
}

const testSSEBody = "event: response.created\ndata: {\"type\":\"response.created\"}\n\n" +
	"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello \"}\n\n" +
	"data: {\"type\":\"response.output_text.delta\",\"delta\":\"world\"}\n\n" +
	"event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"

func streamingUpstream(body string, delay time.Duration, status int, contentType string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		flusher := w.(http.Flusher)
		for _, chunk := range strings.SplitAfter(body, "\n\n") {
			if chunk == "" {
				continue
			}
			_, _ = w.Write([]byte(chunk))
			flusher.Flush()
			time.Sleep(delay)
		}
	})
}

func TestStreamTapEmitsOrderedStreamEvents(t *testing.T) {
	upstream := streamingUpstream(testSSEBody, 5*time.Millisecond, http.StatusOK, "text/event-stream; charset=utf-8")
	g, _ := localGateway(t, policy.Default(), upstream)

	events, stop := collectStreamEvents(g)
	defer stop()

	recorder := httptest.NewRecorder()
	g.ServeHTTP(recorder, requestFor(`{"model":"m","input":"hi","stream":true}`))
	if got := recorder.Body.String(); got != testSSEBody {
		t.Fatalf("bypass body mutated:\n got  %q\n want %q", got, testSSEBody)
	}

	seen := 0
	deadline := time.After(3 * time.Second)
	for seen < 4 {
		select {
		case event := <-events:
			if event.Stream == nil {
				t.Fatalf("stream event %d carries no stream payload", seen)
			}
			if event.Stream.Ordinal != seen {
				t.Fatalf("ordinal out of order: got %d, want %d", event.Stream.Ordinal, seen)
			}
			if event.Revision != 0 {
				t.Fatalf("stream event must not commit a revision, got %d", event.Revision)
			}
			if event.EventID != streamEventID(event.ExchangeID, seen) {
				t.Fatalf("unexpected event id %q", event.EventID)
			}
			seen++
		case <-deadline:
			t.Fatalf("timed out after %d stream events", seen)
		}
	}
	if seen != 4 {
		t.Fatalf("expected 4 stream events, got %d", seen)
	}
}

func TestStreamTapDataIsExact(t *testing.T) {
	upstream := streamingUpstream(testSSEBody, 0, http.StatusOK, "text/event-stream")
	g, _ := localGateway(t, policy.Default(), upstream)

	events, stop := collectStreamEvents(g)
	defer stop()

	g.ServeHTTP(httptest.NewRecorder(), requestFor(`{"model":"m","input":"hi","stream":true}`))

	wantData := []string{
		`{"type":"response.created"}`,
		`{"type":"response.output_text.delta","delta":"hello "}`,
		`{"type":"response.output_text.delta","delta":"world"}`,
		`{"type":"response.completed"}`,
	}
	wantNames := []string{"response.created", "", "", "response.completed"}
	var got []exchange.Event
	deadline := time.After(3 * time.Second)
	for len(got) < 4 {
		select {
		case event := <-events:
			got = append(got, event)
		case <-deadline:
			t.Fatalf("timed out after %d stream events", len(got))
		}
	}
	for i, event := range got {
		if event.Stream.Data != wantData[i] {
			t.Fatalf("event %d data mutated: got %q, want %q", i, event.Stream.Data, wantData[i])
		}
		if event.Stream.Name != wantNames[i] {
			t.Fatalf("event %d name mismatch: got %q, want %q", i, event.Stream.Name, wantNames[i])
		}
		if event.Stream.ByteStart < 0 || event.Stream.ByteEnd <= event.Stream.ByteStart {
			t.Fatalf("event %d has invalid byte span %v", i, event.Stream.ByteStart)
		}
		if !event.Stream.Complete {
			t.Fatalf("event %d must be complete", i)
		}
	}
}

func TestStreamTapEmitsDuringHeldCapture(t *testing.T) {
	// Response gate holds the complete response; the stream tap must still
	// observe records while the upstream body is being captured.
	upstream := streamingUpstream(testSSEBody, 3*time.Millisecond, http.StatusOK, "text/event-stream")
	g, _ := localGateway(t, policy.Policy{RequestGate: "pass", ResponseGate: "hold"}, upstream)

	events, stop := collectStreamEvents(g)
	defer stop()

	go func() {
		recorder := httptest.NewRecorder()
		g.ServeHTTP(recorder, requestFor(`{"model":"m","input":"hi","stream":true}`))
	}()

	seen := 0
	deadline := time.After(3 * time.Second)
	for seen < 4 {
		select {
		case <-events:
			seen++
		case <-deadline:
			t.Fatalf("timed out after %d stream events during held capture", seen)
		}
	}
}

func TestStreamTapIgnoresJSONResponses(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	})
	g, _ := localGateway(t, policy.Default(), upstream)

	events, stop := collectStreamEvents(g)
	defer stop()

	recorder := httptest.NewRecorder()
	g.ServeHTTP(recorder, requestFor(`{"model":"m","messages":[]}`))
	select {
	case event := <-events:
		t.Fatalf("JSON response must not emit stream events, got %+v", event)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestStreamTapTruncatedStreamIsIncomplete(t *testing.T) {
	truncated := "data: {\"type\":\"response.created\"\n\ndata: {\"delta\":\"partial"
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(truncated))
	})
	g, _ := localGateway(t, policy.Default(), upstream)

	events, stop := collectStreamEvents(g)
	defer stop()

	recorder := httptest.NewRecorder()
	g.ServeHTTP(recorder, requestFor(`{"model":"m","input":"hi","stream":true}`))
	if got := recorder.Body.String(); got != truncated {
		t.Fatalf("bypass body mutated:\n got  %q\n want %q", got, truncated)
	}

	collect := func(n int) []exchange.Event {
		var got []exchange.Event
		deadline := time.After(3 * time.Second)
		for len(got) < n {
			select {
			case event := <-events:
				got = append(got, event)
			case <-deadline:
				t.Fatalf("timed out after %d stream events", len(got))
			}
		}
		return got
	}
	got := collect(2)
	if !got[0].Stream.Complete {
		t.Fatal("first terminated record should be complete")
	}
	if got[1].Stream.Complete {
		t.Fatal("truncated final event must be marked incomplete")
	}
	if !strings.Contains(got[1].Stream.Data, "partial") {
		t.Fatalf("incomplete record data lost: %q", got[1].Stream.Data)
	}
}

// streamEventID determinism guard used by the tests above.
func TestStreamEventIDFormat(t *testing.T) {
	if got := streamEventID("ex1", 7); got != "ex1:stream:7" {
		t.Fatalf("unexpected id %q", got)
	}
}
