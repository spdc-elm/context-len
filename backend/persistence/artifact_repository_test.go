package persistence

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"context-lens/backend/wire"
)

func TestStreamingCaptureLargeRangeSearchAndPartial(t *testing.T) {
	store, err := NewStore(Config{SpillRoot: t.TempDir(), MaxArtifactBytes: 8 << 20, MaxMemoryBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	w, err := store.Begin(context.Background(), wire.ArtifactOptions{ArtifactID: "large-stream", Stage: wire.StageResponseUpstream, Direction: wire.DirectionUpstream, ContentType: "application/octet-stream"})
	if err != nil {
		t.Fatal(err)
	}
	block := bytes.Repeat([]byte("0123456789abcdef"), 4096)
	for i := 0; i < 80; i++ {
		if _, err := w.Write(block); err != nil {
			t.Fatal(err)
		}
	}
	ref, err := w.Commit(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Complete || ref.Size != int64(len(block)*80) {
		t.Fatalf("unexpected ref: %+v", ref)
	}
	got, err := store.ReadRange(context.Background(), ref.ArtifactID, 65530, 65550)
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte("0123456789abcdef"), 4097)[65530:65550]
	if !bytes.Equal(got, want) {
		t.Fatalf("range mismatch")
	}
	matches, err := store.Search(context.Background(), ref.ArtifactID, []byte("ef0123"), 17)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 || matches[0].Start != 14 {
		t.Fatalf("boundary search mismatch: %+v", matches)
	}
	if stats := store.Stats(); stats.MemoryBytes != 0 {
		t.Fatalf("stream capture retained %d memory bytes", stats.MemoryBytes)
	}
}

func TestStreamingCaptureDeduplicatesBlobAcrossStages(t *testing.T) {
	store, err := NewStore(Config{SpillRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	makeOne := func(id, stage string) wire.ArtifactRef {
		w, err := store.Begin(context.Background(), wire.ArtifactOptions{ArtifactID: id, Stage: stage, Direction: wire.DirectionInbound})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, "same-body"); err != nil {
			t.Fatal(err)
		}
		ref, err := w.Commit(context.Background(), true)
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	a := makeOne("stage-a", wire.StageRequestInbound)
	b := makeOne("stage-b", wire.StageRequestUpstream)
	if a.StorageRef != b.StorageRef {
		t.Fatalf("blob refs differ: %q %q", a.StorageRef, b.StorageRef)
	}
	if stats := store.Stats(); stats.Bytes != int64(len("same-body")) || stats.Artifacts != 2 {
		t.Fatalf("shared blob accounting = %#v, want bytes=%d artifacts=2", stats, len("same-body"))
	}
	if err := store.Delete(context.Background(), a.ArtifactID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Metadata(context.Background(), b.ArtifactID); err != nil {
		t.Fatal(err)
	}
	if stats := store.Stats(); stats.Bytes != int64(len("same-body")) {
		t.Fatalf("bytes after deleting one ref = %d, want %d", stats.Bytes, len("same-body"))
	}
	if err := store.Delete(context.Background(), b.ArtifactID); err != nil {
		t.Fatal(err)
	}
	if stats := store.Stats(); stats.Bytes != 0 || stats.Artifacts != 0 {
		t.Fatalf("bytes after deleting last ref = %#v", stats)
	}
}

func TestStreamingCaptureReaderLeaseSurvivesClear(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(Config{SpillRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	w, err := store.Begin(context.Background(), wire.ArtifactOptions{ArtifactID: "clear-leased", Stage: wire.StageRequestInbound, Direction: wire.DirectionInbound})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Write([]byte("lease-safe")); err != nil {
		t.Fatal(err)
	}
	if _, err = w.Commit(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	r, _, err := store.Open(context.Background(), "clear-leased")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Clear(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(r); err != nil || string(got) != "lease-safe" {
		t.Fatalf("leased read after clear: %q, %v", got, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(root, "blobs", "*", "*", "*.blob"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("clear left blob files: %v", matches)
	}
}

func TestStreamingCaptureReaderLeaseSurvivesDelete(t *testing.T) {
	store, err := NewStore(Config{SpillRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	w, err := store.Begin(context.Background(), wire.ArtifactOptions{ArtifactID: "leased", Stage: wire.StageRequestInbound, Direction: wire.DirectionInbound})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Write([]byte("lease-safe")); err != nil {
		t.Fatal(err)
	}
	if _, err = w.Commit(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	r, _, err := store.Open(context.Background(), "leased")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Delete(context.Background(), "leased"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.Open(context.Background(), "leased"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("open after delete: %v", err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "lease-safe" {
		t.Fatalf("leased bytes %q", got)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
}
