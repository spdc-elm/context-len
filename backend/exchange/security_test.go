package exchange

import (
	"context"
	"net/http"
	"testing"

	"context-lens/backend/policy"
	"context-lens/backend/wire"
)

func TestSnapshotRedactsHeadersButWriterGetsOriginalResponseHeaders(t *testing.T) {
	request := wire.NewRequestEnvelope("POST", "/v1/responses", "/v1/responses", "", http.Header{"Authorization": {"Bearer inbound-secret"}, "X-Trace": {"trace"}})
	written := make(chan DownstreamResponse, 1)
	r := NewRegistry(policy.Default())
	e, err := r.Create(CreateParams{
		ExchangeID:      "redaction",
		RequestEnvelope: request,
		RequestArtifact: requestArtifact("body"),
		Upstream: func(context.Context, UpstreamRequest) (UpstreamResponse, error) {
			resp := response("ok", 200)
			resp.Envelope.Headers.Set("Set-Cookie", "session-secret")
			return resp, nil
		},
		Downstream: func(_ context.Context, resp DownstreamResponse) error { written <- resp; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = waitExchange(t, e)
	s := e.Snapshot()
	if got := s.Request.Envelope.Headers.Get("Authorization"); got != wire.RedactedHeaderValue {
		t.Fatalf("request authorization = %q", got)
	}
	if got := s.Response.Envelope.Headers.Get("Set-Cookie"); got != wire.RedactedHeaderValue {
		t.Fatalf("response set-cookie = %q", got)
	}
	if got := (<-written).Envelope.Headers.Get("Set-Cookie"); got != "session-secret" {
		t.Fatalf("writer response header = %q", got)
	}
}
