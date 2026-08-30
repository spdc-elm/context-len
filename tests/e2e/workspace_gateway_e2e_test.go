package e2e_test

import (
	"bytes"
	"context-lens/backend/exchange"
	"context-lens/backend/gateway"
	"context-lens/backend/policy"
	"context-lens/backend/workspace"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWorkspaceGatewayHoldHold(t *testing.T) {
	upstreamBody := []byte(`{"id":"resp_hold","object":"response","status":"completed","output":[]}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(upstreamBody)
	}))
	defer upstream.Close()
	g, err := gateway.New(gateway.Config{UpstreamURL: upstream.URL, CaptureMode: gateway.CaptureModeCapture, InitialPolicy: policy.Policy{RequestGate: policy.GateHold, ResponseGate: policy.GateHold}})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Store().Close()
	ws := workspace.New(workspace.Config{Registry: g.Registry(), Artifacts: g.Store(), Policy: g.Policy(), Events: g})
	defer ws.Close()
	mux := http.NewServeMux()
	mux.Handle("/api/", ws)
	mux.Handle("/", g)
	server := httptest.NewServer(mux)
	defer server.Close()
	type clientResult struct {
		body []byte
		err  error
	}
	done := make(chan clientResult, 1)
	go func() {
		resp, e := http.Post(server.URL+"/v1/responses", "application/json", bytes.NewReader([]byte(`{"model":"fixture-model","input":"hello"}`)))
		if e != nil {
			done <- clientResult{err: e}
			return
		}
		b, e := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		done <- clientResult{body: b, err: e}
	}()
	held := pollHTTPExchange(t, server.URL, exchange.StateRequestHeld)
	postWorkspaceCommand(t, server.URL, exchange.Command{ExchangeID: held.ExchangeID, BaseRevision: held.Revision, Kind: exchange.CommandForwardUnchanged})
	responseHeld := pollHTTPExchange(t, server.URL, exchange.StateResponseHeld)
	postWorkspaceCommand(t, server.URL, exchange.Command{ExchangeID: responseHeld.ExchangeID, BaseRevision: responseHeld.Revision, Kind: exchange.CommandReleaseUnchanged})
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if !bytes.Equal(result.body, upstreamBody) {
			t.Fatalf("body changed: %q", result.body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("held HTTP client did not finish")
	}
	final := pollHTTPExchange(t, server.URL, exchange.StateCompleted)
	if len(final.Request.ArtifactRefs) != 2 || len(final.Response.ArtifactRefs) != 2 {
		t.Fatalf("four stages absent: %+v %+v", final.Request.ArtifactRefs, final.Response.ArtifactRefs)
	}
}
func pollHTTPExchange(t *testing.T, base string, want exchange.State) exchange.Snapshot {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, e := http.Get(base + "/api/exchanges")
		if e == nil {
			var list []exchange.Snapshot
			e = json.NewDecoder(resp.Body).Decode(&list)
			_ = resp.Body.Close()
			if e == nil && len(list) > 0 && list[0].State == want {
				return list[0]
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no exchange reached %s", want)
	return exchange.Snapshot{}
}
func postWorkspaceCommand(t *testing.T, base string, cmd exchange.Command) {
	t.Helper()
	body, _ := json.Marshal(cmd)
	resp, e := http.Post(base+"/api/exchanges/"+cmd.ExchangeID+"/command", "application/json", bytes.NewReader(body))
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("command status=%d body=%s", resp.StatusCode, b)
	}
}
