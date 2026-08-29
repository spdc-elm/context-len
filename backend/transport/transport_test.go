package transport

import (
	"net/http"
	"testing"
)

func TestApplyRemovesContextLensClientKey(t *testing.T) {
	inbound := http.Header{
		"X-Context-Lens-Key": {"synthetic-client-key"},
		"Authorization":      {"Bearer another-client-key"},
		"X-API-Key":          {"another-client-key"},
		"Content-Type":       {"application/json"},
	}
	out, err := (DefaultHeaderPolicy()).Apply(inbound)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"X-Context-Lens-Key", "Authorization", "X-API-Key"} {
		if got := out.Get(name); got != "" {
			t.Fatalf("%s leaked to upstream: %q", name, got)
		}
	}
	if out.Get("Content-Type") != "application/json" {
		t.Fatal("non-credential header was removed")
	}
}
