package gateway

import (
	"context"
	"testing"

	"context-lens/backend/workspace"
)

func TestWorkspaceBackendImplementsRuntimeAdapters(t *testing.T) {
	var _ workspace.SessionDeleter = (*workspaceBackend)(nil)
	var _ workspace.StorageStatsProvider = (*workspaceBackend)(nil)
	var _ workspace.CaptureSettings = captureAdapter{}
	b := &workspaceBackend{}
	if err := b.DeleteSession(context.Background(), "s"); err == nil {
		t.Fatal("nil workspace backend unexpectedly succeeded")
	}
	if _, err := b.StorageStats(context.Background()); err == nil {
		t.Fatal("nil workspace backend unexpectedly returned storage stats")
	}
}
