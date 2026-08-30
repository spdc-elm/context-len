package workspace

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type apiCaptureFake struct {
	mode string
	err  error
}

func (f *apiCaptureFake) CaptureMode() string { return f.mode }
func (f *apiCaptureFake) SetCaptureMode(m string) error {
	if f.err != nil {
		return f.err
	}
	f.mode = m
	return nil
}

type apiCaptureErr struct{}

func (apiCaptureErr) CaptureModeError(error) string { return "capture_mode_conflict" }

type apiCaptureConflictFake struct{ apiCaptureFake }

func (apiCaptureConflictFake) CaptureModeError(error) string { return "capture_mode_conflict" }

type apiStatsFake struct{}

func (apiStatsFake) StorageStats(context.Context) (StorageStats, error) {
	return StorageStats{MemoryUsed: 2, MemoryLimit: 10, DiskUsed: 3, DiskLimit: 20}, nil
}

type apiSessionFake struct {
	err error
	id  string
}

func (f *apiSessionFake) DeleteSession(_ context.Context, id string) error { f.id = id; return f.err }

func TestWorkspaceCaptureStorageAndSessionAPIs(t *testing.T) {
	cap := &apiCaptureFake{mode: "passthrough"}
	stats := apiStatsFake{}
	sess := &apiSessionFake{}
	s := New(Config{Capture: cap, Storage: stats, Sessions: sess})
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/settings/capture", nil))
	if rr.Code != 200 {
		t.Fatalf("capture GET=%d", rr.Code)
	}
	rr = httptest.NewRecorder()
	s.ServeHTTP(rr, jsonRequest(http.MethodPatch, "/api/settings/capture", `{"capture_mode":"capture"}`))
	if rr.Code != 200 || cap.mode != "capture" {
		t.Fatalf("capture PATCH=%d mode=%s", rr.Code, cap.mode)
	}
	rr = httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/storage", nil))
	if rr.Code != 200 || rr.Body.Len() == 0 {
		t.Fatalf("storage GET=%d body=%q", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest(http.MethodHead, "/api/storage", nil))
	if rr.Code != 200 || rr.Body.Len() != 0 {
		t.Fatalf("storage HEAD=%d body=%d", rr.Code, rr.Body.Len())
	}
	rr = httptest.NewRecorder()
	s.ServeHTTP(rr, jsonRequest(http.MethodDelete, "/api/sessions/s-1", `{}`))
	if rr.Code != 200 || sess.id != "s-1" {
		t.Fatalf("session DELETE=%d id=%s", rr.Code, sess.id)
	}
	rr = httptest.NewRecorder()
	s.ServeHTTP(rr, jsonRequest(http.MethodDelete, "/api/exchanges/e-1/turn", `{}`))
	if rr.Code != 404 {
		t.Fatalf("per-turn route=%d", rr.Code)
	}
}

func TestWorkspaceCaptureConflictClassifierReturns409(t *testing.T) {
	cap := &apiCaptureConflictFake{apiCaptureFake{mode: "capture", err: errors.New("opaque")}}
	s := New(Config{Capture: cap})
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, jsonRequest(http.MethodPatch, "/api/settings/capture", `{"capture_mode":"passthrough"}`))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d", rr.Code)
	}
}
func TestWorkspaceCaptureAndSessionConflicts(t *testing.T) {
	cap := &apiCaptureFake{mode: "capture", err: errors.New("gate")}
	sess := &apiSessionFake{err: errors.New("active")}
	s := New(Config{Capture: cap, Storage: apiStatsFake{}, Sessions: sess})
	// Classifier doubles are separate to keep free-form errors out of production.
	_ = apiCaptureErr{}
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, jsonRequest(http.MethodPatch, "/api/settings/capture", `{"capture_mode":"passthrough"}`))
	if rr.Code != 400 {
		t.Fatalf("uncategorized capture err=%d", rr.Code)
	}
	rr = httptest.NewRecorder()
	s.ServeHTTP(rr, jsonRequest(http.MethodDelete, "/api/sessions/s", `{}`))
	if rr.Code != 500 {
		t.Fatalf("uncategorized session err=%d", rr.Code)
	}
}
