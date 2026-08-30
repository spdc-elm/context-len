package wire

import (
	"bytes"
	"io"
	"testing"
)

func TestLazyArtifactLengthOpenAndMarshal(t *testing.T) {
	body := []byte("lazy-wire-body")
	ref := NewArtifactRef(body, ArtifactOptions{ArtifactID: "lazy-test", Stage: StageRequestInbound, Direction: DirectionInbound, ContentType: "text/plain"})
	opened := 0
	a, err := NewLazyArtifact(ref, func() (io.ReadCloser, error) { opened++; return io.NopCloser(bytes.NewReader(body)), nil })
	if err != nil {
		t.Fatal(err)
	}
	if a.Len() != int64(len(body)) {
		t.Fatalf("Len=%d", a.Len())
	}
	if opened != 0 {
		t.Fatal("lazy opener called early")
	}
	r, err := a.OpenReader()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("body=%q", got)
	}
	if opened != 1 {
		t.Fatalf("opened=%d", opened)
	}
	encoded, err := a.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, body) {
		t.Fatal("marshal included body")
	}
}
