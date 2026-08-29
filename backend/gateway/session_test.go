package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"context-lens/backend/exchange"
	"context-lens/backend/inspection"
)

func postChat(t *testing.T, g *Gateway, body string) exchange.Snapshot {
	t.Helper()
	before := len(g.Registry().List())
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	g.ServeHTTP(recorder, req)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		items := g.Registry().List()
		if len(items) > before && items[len(items)-1].State == exchange.StateCompleted {
			return items[len(items)-1]
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("chat exchange never completed; status=%d", recorder.Code)
	return exchange.Snapshot{}
}

// TestSessionAssignmentThroughGateway drives two append-only chat requests
// through the real gateway and verifies the session placement rides on the
// snapshot: same session, second turn, fork detection, and rollout counting.
func TestSessionAssignmentThroughGateway(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`))
	}))
	defer upstream.Close()
	g, err := New(Config{UpstreamURL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Store().Close()

	turn1 := postChat(t, g, `{"model":"m","messages":[{"role":"user","content":"first"}]}`)
	if turn1.Session == nil || !turn1.Session.Root || turn1.Session.Depth != 1 {
		t.Fatalf("turn1 session = %+v", turn1.Session)
	}

	turn2 := postChat(t, g, `{"model":"m","messages":[{"role":"user","content":"first"},{"role":"assistant","content":"ok"},{"role":"user","content":"second"}]}`)
	if turn2.Session == nil || turn2.Session.SessionID != turn1.Session.SessionID || turn2.Session.Depth != 2 {
		t.Fatalf("turn2 session = %+v", turn2.Session)
	}
	if turn2.Session.ParentExchangeID != turn1.ExchangeID {
		t.Fatalf("turn2 parent = %s, want %s", turn2.Session.ParentExchangeID, turn1.ExchangeID)
	}
	if turn2.Session.Fork {
		t.Fatalf("plain continuation must not be a fork")
	}

	// A fork from the turn-1 state with a different continuation.
	fork := postChat(t, g, `{"model":"m","messages":[{"role":"user","content":"first"},{"role":"assistant","content":"ok"},{"role":"user","content":"second alternative"}]}`)
	if !fork.Session.Fork || fork.Session.SessionID != turn1.Session.SessionID || fork.Session.Depth != 2 {
		t.Fatalf("fork session = %+v", fork.Session)
	}

	// A rollout: the exact turn-2 context resent.
	rollout := postChat(t, g, `{"model":"m","messages":[{"role":"user","content":"first"},{"role":"assistant","content":"ok"},{"role":"user","content":"second"}]}`)
	if rollout.Session.RepeatIndex != 1 || rollout.Session.SessionID != turn1.Session.SessionID {
		t.Fatalf("rollout session = %+v", rollout.Session)
	}

	// Non-chat traffic carries no placement.
	other := httptest.NewRecorder()
	otherReq := httptest.NewRequest(http.MethodGet, "http://proxy.test/v1/models", nil)
	g.ServeHTTP(other, otherReq)
	for _, snapshot := range g.Registry().List() {
		if snapshot.Protocol == string(inspection.ProtocolUnknown) && snapshot.Session != nil {
			t.Fatalf("non-chat exchange received a session placement: %+v", snapshot.Session)
		}
	}
}

// TestSessionResponseIDContinuation drives a Responses pair where the second
// request carries previous_response_id matching the first response.
func TestSessionResponseIDContinuation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_seq_1","object":"response","status":"completed","output":[],"usage":{"input_tokens":4}}`))
	}))
	defer upstream.Close()
	g, err := New(Config{UpstreamURL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Store().Close()

	post := func(body string) exchange.Snapshot {
		before := len(g.Registry().List())
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/responses", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		g.ServeHTTP(recorder, req)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			items := g.Registry().List()
			if len(items) > before && items[len(items)-1].State == exchange.StateCompleted {
				return items[len(items)-1]
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("responses exchange never completed")
		return exchange.Snapshot{}
	}

	first := post(`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	if !first.Session.Root {
		t.Fatalf("first responses session = %+v", first.Session)
	}
	second := post(`{"model":"m","previous_response_id":"resp_seq_1","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"again"}]}]}`)
	if second.Session.SessionID != first.Session.SessionID || second.Session.Depth != 2 || second.Session.ParentExchangeID != first.ExchangeID {
		t.Fatalf("response continuation = %+v", second.Session)
	}
}
