package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"context-lens/backend/exchange"
)

// TestSummaryProjectionOnPassPath verifies the additive summary projection
// computed at capture time: model, message count, preview, tool names, and
// the upstream-reported context occupancy on the completed response.
func TestSummaryProjectionOnPassPath(t *testing.T) {
	upstreamBody := `{"id":"resp_1","object":"response","status":"completed","model":"fixture-responses-model","output":[],"usage":{"input_tokens":4242,"output_tokens":9}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer upstream.Close()
	g2, err := New(Config{UpstreamURL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer g2.Store().Close()

	requestBody := `{"model":"fixture-responses-model","instructions":"be brief","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Check the deadlock"}]}],"tools":[{"type":"function","name":"lookup_fixture"}]}`
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/responses", strings.NewReader(requestBody))
	req.Header.Set("Content-Type", "application/json")
	g2.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}

	deadline := time.Now().Add(3 * time.Second)
	var snapshot exchange.Snapshot
	for time.Now().Before(deadline) {
		for _, item := range g2.Registry().List() {
			if item.State == exchange.StateCompleted {
				snapshot = item
			}
		}
		if snapshot.ExchangeID != "" && snapshot.Summary != nil && snapshot.Summary.ContextTokens != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if snapshot.ExchangeID == "" {
		t.Fatalf("no completed exchange")
	}
	summary := snapshot.Summary
	if summary == nil {
		t.Fatalf("summary projection missing on snapshot")
	}
	if summary.Model != "fixture-responses-model" {
		t.Fatalf("summary model = %q", summary.Model)
	}
	if summary.MessageCount != 2 {
		t.Fatalf("summary message count = %d, want 2 (instructions + user message)", summary.MessageCount)
	}
	if summary.Preview != "Check the deadlock" {
		t.Fatalf("summary preview = %q", summary.Preview)
	}
	if len(summary.ToolNames) != 1 || summary.ToolNames[0] != "lookup_fixture" {
		t.Fatalf("summary tool names = %v", summary.ToolNames)
	}
	if summary.ContextTokens == nil || *summary.ContextTokens != 4242 {
		t.Fatalf("summary context tokens = %v, want 4242", summary.ContextTokens)
	}
}

// TestSummaryStreamContextTokens covers the direct streaming path: the usage
// is read from the captured SSE artifact after the stream completes, without
// altering the bytes the client received.
func TestSummaryStreamContextTokens(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":77}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer upstream.Close()
	g, err := New(Config{UpstreamURL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Store().Close()

	requestBody := `{"model":"claude-fixture","max_tokens":16,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/messages", strings.NewReader(requestBody))
	req.Header.Set("Content-Type", "application/json")
	g.ServeHTTP(recorder, req)
	if recorder.Body.String() != sse {
		t.Fatalf("stream bytes altered:\n%q", recorder.Body.String())
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		list := g.Registry().List()
		if len(list) > 0 && list[0].Summary != nil && list[0].Summary.ContextTokens != nil {
			if *list[0].Summary.ContextTokens != 77 {
				t.Fatalf("context tokens = %d, want 77", *list[0].Summary.ContextTokens)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("stream summary never received context tokens: %+v", g.Registry().List())
}

// TestSummaryDroppedForUnparseableBody keeps the projection honest: a body
// that is not a recognizable conversation request yields no summary, and the
// exchange still completes normally.
func TestSummaryDroppedForUnparseableBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	g, err := New(Config{UpstreamURL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Store().Close()

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/responses", strings.NewReader("definitely not json"))
	req.Header.Set("Content-Type", "application/json")
	g.ServeHTTP(recorder, req)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		list := g.Registry().List()
		if len(list) > 0 && list[0].State == exchange.StateCompleted {
			if list[0].Summary != nil {
				t.Fatalf("unexpected summary for minimal body: %+v", list[0].Summary)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("exchange never completed")
}
