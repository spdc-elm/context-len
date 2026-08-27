package mutation

import (
	"context-lens/backend/wire"
	"testing"
)

func TestJSONPatchDerivesArtifact(t *testing.T) {
	b := wire.NewArtifact([]byte(`{"model":"old","x":1}`), wire.ArtifactOptions{Stage: "request.inbound"})
	r, e := JSONPatch(b, []Operation{{Op: "replace", Path: "/model", Value: "new"}})
	if e != nil {
		t.Fatal(e)
	}
	if string(r.Artifact.Bytes()) == string(b.Bytes()) || r.BaseSHA256 != b.Ref().SHA256 {
		t.Fatal("artifact not derived")
	}
}
