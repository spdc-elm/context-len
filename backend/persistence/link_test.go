package persistence

import (
	"context"
	"context-lens/backend/wire"
	"io"
	"testing"
)

func TestLinkPhysicalAccountingTracksRefs(t *testing.T) {
	store, err := NewStore(Config{SpillRoot: t.TempDir(), MaxTotalBytes: int64(len("link-body"))})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	writer, err := store.Begin(context.Background(), wire.ArtifactOptions{
		ArtifactID: "link-source", Stage: wire.StageRequestInbound, Direction: wire.DirectionInbound,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(writer, "link-body"); err != nil {
		t.Fatal(err)
	}
	source, err := writer.Commit(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	linked, err := store.Link(context.Background(), source.ArtifactID, wire.ArtifactOptions{
		ArtifactID: "link-copy", Stage: wire.StageRequestUpstream, Direction: wire.DirectionUpstream,
	})
	if err != nil {
		t.Fatal(err)
	}
	if linked.StorageRef != source.StorageRef {
		t.Fatalf("link storage ref = %q, want %q", linked.StorageRef, source.StorageRef)
	}
	if got := store.Stats(); got.Artifacts != 2 || got.Bytes != int64(len("link-body")) || got.DiskBytes != int64(len("link-body")) {
		t.Fatalf("after link stats = %#v", got)
	}
	if err := store.Delete(context.Background(), source.ArtifactID); err != nil {
		t.Fatal(err)
	}
	if got := store.Stats(); got.Artifacts != 1 || got.Bytes != int64(len("link-body")) {
		t.Fatalf("after deleting source stats = %#v", got)
	}
	if err := store.Delete(context.Background(), linked.ArtifactID); err != nil {
		t.Fatal(err)
	}
	if got := store.Stats(); got.Artifacts != 0 || got.Bytes != 0 || got.DiskBytes != 0 {
		t.Fatalf("after deleting last link stats = %#v", got)
	}
}

func TestLinkSharesBlobWithoutReading(t *testing.T) {
	s, err := NewStore(Config{SpillRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w, _ := s.Begin(context.Background(), wire.ArtifactOptions{ArtifactID: "src", Stage: wire.StageRequestInbound, Direction: wire.DirectionInbound})
	_, _ = w.Write([]byte("body"))
	src, err := w.Commit(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := s.Link(context.Background(), "src", wire.ArtifactOptions{ArtifactID: "dst", Stage: wire.StageRequestUpstream, Direction: wire.DirectionUpstream})
	if err != nil {
		t.Fatal(err)
	}
	if dst.StorageRef != src.StorageRef || dst.ArtifactID == src.ArtifactID {
		t.Fatalf("refs: %+v %+v", src, dst)
	}
	if err := s.Delete(context.Background(), "src"); err != nil {
		t.Fatal(err)
	}
	r, _, err := s.Open(context.Background(), "dst")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
}
