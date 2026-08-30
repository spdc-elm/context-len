package gateway

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"context-lens/backend/policy"
)

// CaptureMode determines whether new ingress is persisted for inspection.
type CaptureMode string

const (
	CaptureModePassthrough CaptureMode = "passthrough"
	CaptureModeCapture     CaptureMode = "capture"
)

var (
	ErrInvalidCaptureMode  = errors.New("gateway: invalid capture mode")
	ErrCaptureModeConflict = errors.New("gateway: passthrough unavailable while capture gate is held")
)

func normalizeCaptureMode(mode CaptureMode) CaptureMode {
	if strings.TrimSpace(string(mode)) == "" {
		return CaptureModePassthrough
	}
	return mode
}

func validCaptureMode(mode CaptureMode) bool {
	return mode == CaptureModePassthrough || mode == CaptureModeCapture
}

// CaptureMode returns the process mode used for subsequent ingress.
func (g *Gateway) CaptureMode() CaptureMode {
	if g == nil {
		return CaptureModePassthrough
	}
	g.modeMu.RLock()
	defer g.modeMu.RUnlock()
	return g.captureMode
}

// SetCaptureMode changes the process mode. Existing exchanges retain their
// snapshotted mode. Passthrough is rejected while either gate is held.
func (g *Gateway) SetCaptureMode(mode CaptureMode) error {
	mode = normalizeCaptureMode(mode)
	if !validCaptureMode(mode) {
		return ErrInvalidCaptureMode
	}
	if g == nil {
		return errors.New("gateway: unavailable")
	}
	g.modeMu.Lock()
	defer g.modeMu.Unlock()
	if mode == CaptureModePassthrough && g.policy != nil {
		p := g.policy.Get().Normalize()
		if p.RequestGate == policy.GateHold || p.ResponseGate == policy.GateHold {
			return ErrCaptureModeConflict
		}
	}
	g.captureMode = mode
	return nil
}

// servePassthrough performs an opaque same-protocol forward. It intentionally
// does not create artifacts, registry entries, sessions, summaries, or events.
func (g *Gateway) servePassthrough(w http.ResponseWriter, r *http.Request) {
	prepared, err := g.upstream.PrepareRequest(r.Context(), r, nil)
	if err != nil {
		http.Error(w, "gateway upstream unavailable", http.StatusBadGateway)
		return
	}
	resp, err := g.upstream.Client().Do(prepared)
	if err != nil {
		http.Error(w, "gateway upstream unavailable", http.StatusBadGateway)
		return
	}
	if resp.Body == nil {
		resp.Body = http.NoBody
	}
	defer resp.Body.Close()
	for k, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if resp.Body == nil {
		return
	}
	_, _ = io.Copy(w, resp.Body)
}
