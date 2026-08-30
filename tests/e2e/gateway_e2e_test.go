package e2e_test

import (
	"bytes"
	"context"
	"context-lens/backend/exchange"
	"context-lens/backend/gateway"
	"context-lens/backend/wire"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestGatewayFixturePassPass proves the integrated capture/gate handler keeps
// the same exact fixture bytes while also persisting comparable artifacts.
func TestGatewayFixturePassPass(t *testing.T) {
	for _, tc := range fixtureCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			reqBody := readFixture(t, tc.requestFile)
			respBody := readFixture(t, tc.responseFile)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, _ := io.ReadAll(r.Body)
				if !bytes.Equal(got, reqBody) {
					t.Errorf("upstream request changed")
				}
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(respBody)
			}))
			defer upstream.Close()
			g, err := gateway.New(gateway.Config{UpstreamURL: upstream.URL, CaptureMode: gateway.CaptureModeCapture})
			if err != nil {
				t.Fatal(err)
			}
			defer g.Store().Close()
			proxy := httptest.NewServer(g)
			defer proxy.Close()
			response, err := http.Post(proxy.URL+tc.endpoint, requestContentType(tc.requestFile), bytes.NewReader(reqBody))
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if !bytes.Equal(got, respBody) {
				t.Fatal("downstream response changed")
			}
			var snapshots []exchange.Snapshot
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				snapshots = g.Registry().List()
				if len(snapshots) == 1 && snapshots[0].State.Terminal() {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if len(snapshots) != 1 {
				t.Fatalf("snapshots=%d", len(snapshots))
			}
			s := snapshots[0]
			if len(s.Request.ArtifactRefs) < 2 || len(s.Response.ArtifactRefs) < 2 {
				t.Fatalf("artifact refs request=%d response=%d", len(s.Request.ArtifactRefs), len(s.Response.ArtifactRefs))
			}
			for _, ref := range append(append([]wire.ArtifactRef{}, s.Request.ArtifactRefs...), s.Response.ArtifactRefs...) {
				a, e := g.Store().Get(context.Background(), ref.ArtifactID)
				if e != nil {
					t.Fatal(e)
				}
				if a.Ref().SHA256 != ref.SHA256 {
					t.Fatalf("stored hash mismatch for %s", ref.Stage)
				}
			}
		})
	}
}
