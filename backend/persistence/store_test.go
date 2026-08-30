package persistence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"context-lens/backend/wire"
)

func TestClearInvalidatesBlockedCaptureCommitAndAllowsNextCapture(t *testing.T) {
	s, err := NewArtifactStore(Config{MaxArtifactBytes: 1 << 20, MaxTotalBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w, err := s.Begin(context.Background(), wire.ArtifactOptions{ArtifactID: "old", Stage: wire.StageRequestInbound, Direction: wire.DirectionInbound})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := s.Clear(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(context.Background(), true); !errors.Is(err, ErrStaleArtifact) {
		t.Fatalf("old commit error=%v, want ErrStaleArtifact", err)
	}
	if got := s.Stats().Artifacts; got != 0 {
		t.Fatalf("stale capture indexed %d artifacts", got)
	}

	n, err := s.Begin(context.Background(), wire.ArtifactOptions{ArtifactID: "new", Stage: wire.StageRequestInbound, Direction: wire.DirectionInbound})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Commit(context.Background(), true); err != nil {
		t.Fatalf("new commit: %v", err)
	}
	if got := s.Stats().Artifacts; got != 1 {
		t.Fatalf("new capture artifact count=%d, want 1", got)
	}
}
func TestStorePutGetMetadataAndImmutability(t *testing.T) {
	store, err := NewArtifactStore(Config{MaxArtifactBytes: 1 << 20, MaxTotalBytes: 2 << 20, MaxMemoryBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	body := []byte("{\"number\":1.2300,\"unknown\":[null,true]}\n\ndata: x\\x00\\xff\n\n")
	artifact := wire.NewArtifact(body, wire.ArtifactOptions{
		ArtifactID:      "original-1",
		Stage:           wire.StageRequestInbound,
		Direction:       wire.DirectionInbound,
		ContentType:     "application/json",
		ContentEncoding: "identity",
		StorageRef:      "caller/opaque-ref",
	})
	ref, err := store.Put(context.Background(), artifact)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if ref != artifact.Ref() {
		t.Fatalf("put changed metadata: got %#v want %#v", ref, artifact.Ref())
	}

	meta, err := store.Metadata(context.Background(), ref.ArtifactID)
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if meta.Size != int64(len(body)) || meta.SHA256 != wire.SHA256Hex(body) || !meta.Complete {
		t.Fatalf("metadata integrity = %#v", meta)
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, body) {
		t.Fatalf("metadata unexpectedly contains body bytes: %s", encoded)
	}

	got, err := store.Get(context.Background(), ref.ArtifactID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bytes.Equal(got.Bytes(), body) || !got.Verify() || got.Ref() != ref {
		t.Fatalf("get changed artifact: ref=%#v bytes=%q", got.Ref(), got.Bytes())
	}
	out := got.Bytes()
	out[0] = 'X'
	again, err := store.GetArtifact(ref.ArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again.Bytes(), body) {
		t.Fatal("mutating returned bytes changed stored artifact")
	}

	reader, openedRef, err := store.OpenArtifact(ref.ArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	opened, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("open read/close: read=%v close=%v", readErr, closeErr)
	}
	if openedRef != ref || !bytes.Equal(opened, body) {
		t.Fatalf("open = ref %#v body %q", openedRef, opened)
	}
}

func TestStoreRetainsIncompleteState(t *testing.T) {
	store, err := NewStore(Config{MaxMemoryBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	body := []byte("partial\\x00body")
	incomplete := wire.NewIncompleteArtifact(body, wire.ArtifactOptions{
		ArtifactID: "partial-1", Stage: wire.StageResponseUpstream, Direction: wire.DirectionUpstream,
	})
	ref, err := store.PutArtifact(incomplete)
	if err != nil {
		t.Fatalf("put incomplete: %v", err)
	}
	if ref.Complete {
		t.Fatal("incomplete artifact was marked complete")
	}
	got, err := store.GetArtifact(ref.ArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Complete() || got.Status() != wire.CaptureIncomplete || !bytes.Equal(got.Bytes(), body) {
		t.Fatalf("incomplete state/body not retained: complete=%v status=%q body=%q", got.Complete(), got.Status(), got.Bytes())
	}
}

func TestStoreSpillsToDiskWithSafePathAndExactBytes(t *testing.T) {
	root := t.TempDir()
	store, err := NewArtifactStore(Config{MaxArtifactBytes: 1 << 20, MaxTotalBytes: 2 << 20, MaxMemoryBytes: 2, SpillRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	body := []byte{0, '{', ' ', '}', '\n', 0xff, 1, 2, 3, 4}
	artifact := wire.NewArtifact(body, wire.ArtifactOptions{
		ArtifactID: "spill-1", StorageRef: "../../must-not-be-used-as-path",
	})
	ref, err := store.PutArtifact(artifact)
	if err != nil {
		t.Fatalf("put spill: %v", err)
	}
	stats := store.Stats()
	if stats.Artifacts != 1 || stats.Bytes != int64(len(body)) || stats.MemoryBytes != 0 || stats.DiskBytes != int64(len(body)) {
		t.Fatalf("stats = %#v", stats)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "spill-1.blob" {
		t.Fatalf("spill entries = %#v", entries)
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("spill mode = %o, want 600", info.Mode().Perm())
	}
	if !bytes.Equal(mustReadFile(t, filepath.Join(root, entries[0].Name())), body) {
		t.Fatal("spill file bytes changed")
	}
	got, err := store.GetArtifact(ref.ArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), body) || got.Ref() != ref {
		t.Fatalf("spilled get = %#v %q", got.Ref(), got.Bytes())
	}
	part, err := store.ReadRange(context.Background(), ref.ArtifactID, 2, 7)
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	if !bytes.Equal(part, body[2:7]) {
		t.Fatalf("range = %v, want %v", part, body[2:7])
	}
	if err := store.Delete(context.Background(), ref.ArtifactID); err != nil {
		t.Fatalf("delete spilled Put entry: %v", err)
	}
	if stats := store.Stats(); stats.Artifacts != 0 || stats.Bytes != 0 || stats.MemoryBytes != 0 || stats.DiskBytes != 0 {
		t.Fatalf("stats after deleting spilled Put entry = %#v", stats)
	}
}

func TestStoreRejectsTraversalAndNeverOverwrites(t *testing.T) {
	root := t.TempDir()
	store, err := NewArtifactStore(Config{SpillRoot: root, MaxMemoryBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for _, id := range []string{"../escape", "..\\escape", "/absolute", `C:\\escape`, "..", "a..b", "with/slash", "with\\slash"} {
		artifact := wire.NewArtifact([]byte("secret-body"), wire.ArtifactOptions{ArtifactID: id})
		if _, err := store.PutArtifact(artifact); !errors.Is(err, ErrInvalidArtifactID) {
			t.Errorf("id %q error = %v, want ErrInvalidArtifactID", id, err)
		}
	}
	artifact := wire.NewArtifact([]byte("first"), wire.ArtifactOptions{ArtifactID: "same-id"})
	if _, err := store.PutArtifact(artifact); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutArtifact(wire.NewArtifact([]byte("replacement"), wire.ArtifactOptions{ArtifactID: "same-id"})); !errors.Is(err, ErrArtifactExists) {
		t.Fatalf("duplicate put error = %v, want ErrArtifactExists", err)
	}
	got, err := store.GetArtifact("same-id")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Bytes()) != "first" {
		t.Fatalf("duplicate changed original body to %q", got.Bytes())
	}
	if _, err := os.Stat(filepath.Join(root, "escape.blob")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected escape file: %v", err)
	}
}

func TestStoreEnforcesArtifactTotalMemoryAndCountLimits(t *testing.T) {
	store, err := NewArtifactStore(Config{MaxArtifactBytes: 4, MaxTotalBytes: 6, MaxMemoryBytes: 6, MaxArtifacts: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.PutArtifact(wire.NewArtifact([]byte("12345"), wire.ArtifactOptions{ArtifactID: "too-large"})); !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("large error = %v", err)
	}
	if _, err := store.PutArtifact(wire.NewArtifact([]byte("1234"), wire.ArtifactOptions{ArtifactID: "one"})); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutArtifact(wire.NewArtifact([]byte("12"), wire.ArtifactOptions{ArtifactID: "two"})); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutArtifact(wire.NewArtifact([]byte("x"), wire.ArtifactOptions{ArtifactID: "three"})); !errors.Is(err, ErrStoreFull) {
		t.Fatalf("count/total error = %v", err)
	}

	memoryLimited, err := NewArtifactStore(Config{MaxMemoryBytes: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer memoryLimited.Close()
	if _, err := memoryLimited.PutArtifact(wire.NewArtifact([]byte("123"), wire.ArtifactOptions{ArtifactID: "memory"})); !errors.Is(err, ErrMemoryLimit) {
		t.Fatalf("memory error = %v", err)
	}
}

func TestStoreCleanupTTLIncludesIncompleteAndHonorsActiveReader(t *testing.T) {
	root := t.TempDir()
	var mu sync.Mutex
	now := time.Unix(100, 0).UTC()
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	setNow := func(next time.Time) {
		mu.Lock()
		now = next
		mu.Unlock()
	}
	store, err := NewArtifactStore(Config{SpillRoot: root, MaxMemoryBytes: 1, TTL: time.Minute, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	complete, err := store.PutArtifact(wire.NewArtifact([]byte("complete"), wire.ArtifactOptions{ArtifactID: "complete"}))
	if err != nil {
		t.Fatal(err)
	}
	incomplete, err := store.PutArtifact(wire.NewIncompleteArtifact([]byte("partial"), wire.ArtifactOptions{ArtifactID: "incomplete"}))
	if err != nil {
		t.Fatal(err)
	}
	reader, _, err := store.OpenArtifact(complete.ArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	setNow(time.Unix(161, 0).UTC())
	removed, err := store.Cleanup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("cleanup removed=%d, want 1 (inactive incomplete)", removed)
	}
	if _, err := store.GetArtifact(incomplete.ArtifactID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("incomplete after cleanup = %v", err)
	}
	if got := store.Stats().Artifacts; got != 1 {
		t.Fatalf("active artifact not retained: count=%d", got)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	removed, err = store.Cleanup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("cleanup after reader close removed=%d, want 1", removed)
	}
	if _, err := os.Stat(filepath.Join(root, "complete.blob")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("spill file remains after cleanup: %v", err)
	}
}

func TestStoreContextAndRangeErrors(t *testing.T) {
	store, err := NewArtifactStore(Config{MaxMemoryBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	artifact := wire.NewArtifact([]byte("abcdef"), wire.ArtifactOptions{ArtifactID: "range"})
	if _, err := store.PutArtifact(artifact); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadRange(context.Background(), "range", -1, 2); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("negative range = %v", err)
	}
	if _, err := store.ReadRange(context.Background(), "range", 3, 2); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("reversed range = %v", err)
	}
	if _, err := store.ReadRange(context.Background(), "range", 0, 7); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("overlong range = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Get(cancelled, "range"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled get = %v", err)
	}
	if _, err := store.Get(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing get = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetArtifact("range"); !errors.Is(err, ErrClosed) {
		t.Fatalf("get after close = %v", err)
	}
}

func TestStoreConcurrentAccessRaceSafe(t *testing.T) {
	store, err := NewArtifactStore(Config{MaxArtifactBytes: 1 << 20, MaxTotalBytes: 1 << 20, MaxMemoryBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.PutArtifact(wire.NewArtifact([]byte("concurrent-body"), wire.ArtifactOptions{ArtifactID: "concurrent"})); err != nil {
		t.Fatal(err)
	}

	var failures atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if ref, err := store.Metadata(context.Background(), "concurrent"); err != nil || ref.ArtifactID != "concurrent" {
					failures.Add(1)
				}
				got, err := store.GetArtifact("concurrent")
				if err != nil || string(got.Bytes()) != "concurrent-body" {
					failures.Add(1)
				}
				reader, _, err := store.OpenArtifact("concurrent")
				if err != nil {
					failures.Add(1)
					continue
				}
				if data, readErr := io.ReadAll(reader); readErr != nil || string(data) != "concurrent-body" {
					failures.Add(1)
				}
				_ = reader.Close()
			}
		}()
	}
	wg.Wait()
	if got := failures.Load(); got != 0 {
		t.Fatalf("concurrent failures = %d", got)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestReconcileSpillQuarantinesUnknownAndStaging(t *testing.T) {
	root := t.TempDir()
	s, err := NewArtifactStore(Config{SpillRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	key := strings.Repeat("a", 64) + "-3"
	blobDir := filepath.Join(root, "blobs", "aa", "aa")
	if err := os.MkdirAll(blobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobDir, key+".blob"), []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".staging"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".staging", "capture-x"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ReconcileSpill(context.Background(), map[string]struct{}{}, 10); err != nil || n != 2 {
		t.Fatalf("reconcile n=%d err=%v", n, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".quarantine", key+".blob")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".quarantine", "staging-capture-x")); err != nil {
		t.Fatal(err)
	}
}

func TestDurableAdoptAfterClose(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(Config{SpillRoot: root, MaxMemoryBytes: 1, PreserveFilesOnClose: true})
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Begin(context.Background(), wire.ArtifactOptions{ArtifactID: "durable-artifact", Stage: "request", Direction: "request"})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("durable exact bytes")
	if _, err = w.Write(body); err != nil {
		t.Fatal(err)
	}
	ref, err := w.Commit(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := NewStore(Config{SpillRoot: root, MaxMemoryBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if err := s2.Adopt(context.Background(), ref, StorageRef{Key: ref.StorageRef}); err != nil {
		t.Fatal(err)
	}
	got, err := s2.Get(context.Background(), ref.ArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), body) {
		t.Fatalf("body mismatch: %q", got.Bytes())
	}
}
