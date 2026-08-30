package catalog

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestRestartHydratesMetadataAndUsesWAL(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "catalog.db")
	c, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.UpsertSession(ctx, Session{ID: "s1", Owner: "local", Title: "one", Pinned: true}); err != nil {
		t.Fatal(err)
	}
	if err = c.UpsertExchange(ctx, Exchange{ID: "e1", SessionID: "s1", Position: 2, Protocol: "responses"}); err != nil {
		t.Fatal(err)
	}
	if err = c.UpsertBlob(ctx, Blob{StorageRef: "blob-sha", SHA256: "abc", Size: 3, Path: "blobs/abc"}); err != nil {
		t.Fatal(err)
	}
	if err = c.PutArtifactRef(ctx, ArtifactRef{ID: "a1", ExchangeID: "e1", Stage: "request.inbound", Direction: "inbound", Size: 3, SHA256: "abc", Complete: true, StorageRef: "blob-sha"}); err != nil {
		t.Fatal(err)
	}
	if err = c.SetSetting(ctx, "workspace.generation", "7"); err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s, err := c.GetSession(ctx, "s1")
	if err != nil || !s.Pinned || s.Owner != "local" {
		t.Fatalf("session after restart: %+v %v", s, err)
	}
	xs, err := c.ListExchanges(ctx, "s1")
	if err != nil || len(xs) != 1 || xs[0].Position != 2 {
		t.Fatalf("exchanges: %+v %v", xs, err)
	}
	as, err := c.ListArtifactRefs(ctx, "e1")
	if err != nil || len(as) != 1 || as[0].StorageRef != "blob-sha" {
		t.Fatalf("artifacts: %+v %v", as, err)
	}
	if v, err := c.GetSetting(ctx, "workspace.generation"); err != nil || v != "7" {
		t.Fatalf("setting %q %v", v, err)
	}
	var mode string
	if err = c.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal mode %q %v", mode, err)
	}
	var version int
	if err = c.db.QueryRow("SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil || version != CurrentSchemaVersion {
		t.Fatalf("version %d %v", version, err)
	}
}

func TestExplicitDeletionIgnoresLegacyPinAndSweepsBlob(t *testing.T) {
	ctx := context.Background()
	c, err := Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	must := func(e error) {
		t.Helper()
		if e != nil {
			t.Fatal(e)
		}
	}
	must(c.UpsertSession(ctx, Session{ID: "s", Pinned: true}))
	must(c.UpsertExchange(ctx, Exchange{ID: "e", SessionID: "s"}))
	must(c.UpsertBlob(ctx, Blob{StorageRef: "b", SHA256: "hash", Size: 10}))
	must(c.PutArtifactRef(ctx, ArtifactRef{ID: "a", ExchangeID: "e", Stage: "response.upstream", Direction: "upstream", Size: 10, SHA256: "hash", StorageRef: "b"}))
	must(c.DeleteSession(ctx, "s"))
	// Repeating the same explicit deletion is a no-op.
	must(c.DeleteSession(ctx, "s"))
	pending, err := c.PendingBlobDeletes(ctx)
	if err != nil || len(pending) != 1 || pending[0].StorageRef != "b" {
		t.Fatalf("pending %+v %v", pending, err)
	}
	must(c.MarkBlobDeleted(ctx, "b"))
	if _, err = c.GetSession(ctx, "s"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session lookup %v", err)
	}
}

func TestDeleteSessionPreservesSharedBlobUntilLastOwner(t *testing.T) {
	ctx := context.Background()
	c, err := Open(filepath.Join(t.TempDir(), "shared.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, id := range []string{"one", "two"} {
		if err := c.UpsertSession(ctx, Session{ID: "s-" + id}); err != nil {
			t.Fatal(err)
		}
		if err := c.UpsertExchange(ctx, Exchange{ID: "e-" + id, SessionID: "s-" + id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.UpsertBlob(ctx, Blob{StorageRef: "shared", SHA256: "hash", Size: 12}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two"} {
		if err := c.PutArtifactRef(ctx, ArtifactRef{ID: "a-" + id, ExchangeID: "e-" + id, StorageRef: "shared", SHA256: "hash", Size: 12}); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := c.Stats(ctx)
	if err != nil || stats.PhysicalBytes != 12 || stats.LogicalArtifacts != 2 {
		t.Fatalf("stats %+v %v", stats, err)
	}
	if err := c.DeleteSession(ctx, "s-one"); err != nil {
		t.Fatal(err)
	}
	if pending, err := c.PendingBlobDeletes(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("pending after first owner %+v %v", pending, err)
	}
	blobs, err := c.ListBlobs(ctx)
	if err != nil || len(blobs) != 1 || blobs[0].RefCount != 1 {
		t.Fatalf("blobs %+v %v", blobs, err)
	}
	if err := c.DeleteSession(ctx, "s-two"); err != nil {
		t.Fatal(err)
	}
	pending, err := c.PendingBlobDeletes(ctx)
	if err != nil || len(pending) != 1 || pending[0].RefCount != 0 {
		t.Fatalf("pending after last owner %+v %v", pending, err)
	}
}

func TestContextCancellation(t *testing.T) {
	c, err := Open(filepath.Join(t.TempDir(), "ctx.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.UpsertSession(ctx, Session{ID: "cancelled"}); err == nil {
		t.Fatal("expected context cancellation")
	}
}

func TestClearIsIdempotentAndLeavesDeletionWork(t *testing.T) {
	ctx := context.Background()
	c, err := Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, err := range []error{c.UpsertSession(ctx, Session{ID: "s"}), c.UpsertExchange(ctx, Exchange{ID: "e", SessionID: "s"}), c.UpsertBlob(ctx, Blob{StorageRef: "b", SHA256: "h"}), c.PutArtifactRef(ctx, ArtifactRef{ID: "a", ExchangeID: "e", StorageRef: "b"})} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = c.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	if err = c.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	ss, err := c.ListSessions(ctx)
	if err != nil || len(ss) != 0 {
		t.Fatalf("sessions %+v %v", ss, err)
	}
	pending, err := c.PendingBlobDeletes(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending %+v %v", pending, err)
	}
}
