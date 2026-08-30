package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"context-lens/backend/persistence"
	"context-lens/backend/wire"
)

func TestWriteStoreErrorClassificationIsStableAndOpaque(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		status  int
		message string
	}{
		{"quota", fmtWrap(persistence.ErrStoreFull), http.StatusInsufficientStorage, "artifact storage quota exceeded"},
		{"memory", fmtWrap(persistence.ErrMemoryLimit), http.StatusInsufficientStorage, "artifact storage memory limit exceeded; configure spill storage"},
		{"too large", fmtWrap(persistence.ErrArtifactTooLarge), http.StatusRequestEntityTooLarge, "artifact exceeds configured size limit"},
		{"closed", fmtWrap(persistence.ErrClosed), http.StatusServiceUnavailable, "artifact storage is closed"},
		{"invalid", fmtWrap(persistence.ErrInvalidArtifact), http.StatusBadRequest, "invalid artifact storage request"},
		{"capture limit", wire.ErrCaptureLimit, http.StatusRequestEntityTooLarge, "artifact exceeds configured size limit"},
		{"io", errors.New("open /secret/private.blob: permission denied"), http.StatusServiceUnavailable, "artifact storage unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			(&Gateway{}).writeStoreError(rec, tc.err)
			if rec.Code != tc.status {
				t.Fatalf("status=%d, want %d", rec.Code, tc.status)
			}
			body := strings.TrimSpace(rec.Body.String())
			if body != tc.message {
				t.Fatalf("body=%q, want %q", body, tc.message)
			}
			if strings.Contains(body, "secret") || strings.Contains(body, "private.blob") {
				t.Fatalf("error leaked sensitive detail: %q", body)
			}
		})
	}
}

func TestWriteStoreErrorCancellationDoesNotWrite(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		rec := httptest.NewRecorder()
		(&Gateway{}).writeStoreError(rec, err)
		if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
			t.Fatalf("cancel error wrote status=%d body=%q", rec.Code, rec.Body.String())
		}
	}
}

func TestDurableErrorIsOpaque(t *testing.T) {
	if strings.Contains(ErrDurablePersistence.Error(), "/") {
		t.Fatalf("durable error contains path: %q", ErrDurablePersistence)
	}
}

// fmtWrap keeps the tests exercising errors.Is through an adapter-like wrapper
// without importing fmt solely for a one-line formatting operation.
func fmtWrap(err error) error { return wrappedStoreError{err} }

type wrappedStoreError struct{ error }

func (e wrappedStoreError) Unwrap() error { return e.error }
