package gateway

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"context-lens/backend/policy"
)

func TestPassthroughPreservesBytesWithoutCaptureSideEffects(t *testing.T) {
	requestBody := []byte{0, 1, '{', '\n', 0xff, 'x'}
	responseBody := []byte("data: one\n\ndata: [DONE]\n\n")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
			return
		}
		if !bytes.Equal(got, requestBody) {
			t.Errorf("request bytes changed: %x", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write(responseBody)
	}))
	defer upstream.Close()

	g, err := New(Config{UpstreamURL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	req := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/responses?x=1", bytes.NewReader(requestBody))
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), responseBody) {
		t.Fatalf("response bytes changed: %x", rec.Body.Bytes())
	}
	if got := len(g.Registry().List()); got != 0 {
		t.Fatalf("passthrough created %d registry entries", got)
	}
	if got := g.Store().Stats().Artifacts; got != 0 {
		t.Fatalf("passthrough created %d artifacts", got)
	}
}

func TestCaptureModeToggleAndHoldConflict(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()
	g, err := New(Config{UpstreamURL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	if got := g.CaptureMode(); got != CaptureModePassthrough {
		t.Fatalf("default mode=%q", got)
	}
	if err := g.SetCaptureMode(CaptureModeCapture); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/responses", bytes.NewBufferString(`{"input":"x"}`)))
	if got := len(g.Registry().List()); got != 1 {
		t.Fatalf("capture created %d exchanges", got)
	}

	if err := g.Policy().Set(policy.Policy{RequestGate: policy.GateHold, ResponseGate: policy.GatePass}); err != nil {
		t.Fatal(err)
	}
	if err := g.SetCaptureMode(CaptureModePassthrough); !errors.Is(err, ErrCaptureModeConflict) {
		t.Fatalf("hold conflict=%v", err)
	}
	if got := g.CaptureMode(); got != CaptureModeCapture {
		t.Fatalf("conflict changed mode to %q", got)
	}
}

func TestCaptureModeIsSnapshottedAtIngress(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()
	g, err := New(Config{UpstreamURL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	done := make(chan struct{})
	go func() {
		g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/responses", bytes.NewBufferString("first")))
		close(done)
	}()
	<-entered
	if err := g.SetCaptureMode(CaptureModeCapture); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	if got := len(g.Registry().List()); got != 0 {
		t.Fatalf("in-flight passthrough became captured: %d exchanges", got)
	}

	g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/responses", bytes.NewBufferString("second")))
	if got := len(g.Registry().List()); got != 1 {
		t.Fatalf("subsequent capture created %d exchanges", got)
	}
}
