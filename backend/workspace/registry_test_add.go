package workspace

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"context-lens/backend/exchange"
	"context-lens/backend/policy"
	"context-lens/backend/wire"
)

func TestRegistryAdapterUsesConcreteRegistryCapabilities(t *testing.T) {
	registry := exchange.NewRegistry(policy.Policy{RequestGate: policy.GateHold, ResponseGate: policy.GatePass})
	artifact := wire.NewArtifact([]byte("opaque"), wire.ArtifactOptions{ArtifactID: "registry-artifact", Stage: wire.StageRequestInbound, Direction: wire.DirectionInbound, ContentType: "text/plain"})
	ex, err := registry.Create(exchange.CreateParams{ExchangeID: "registry-exchange", Protocol: "responses", RequestArtifact: artifact})
	if err != nil {
		t.Fatal(err)
	}
	if ex == nil {
		t.Fatal("registry returned nil exchange")
	}
	server := NewWithRegistry(registry, nil, nil)
	list := httptest.NewRecorder()
	server.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/exchanges", nil))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "registry-exchange") {
		t.Fatalf("registry list = %d %s", list.Code, list.Body.String())
	}
	body := httptest.NewRecorder()
	server.ServeHTTP(body, httptest.NewRequest(http.MethodGet, "/api/artifacts/registry-artifact", nil))
	if body.Code != http.StatusOK || body.Body.String() != "opaque" {
		t.Fatalf("registry artifact = %d %q", body.Code, body.Body.String())
	}
}
