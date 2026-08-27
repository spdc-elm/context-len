package proxy

import (
	"context-lens/backend/transport"
	"io"
	"net/http"
)

// Handler is a protocol-agnostic transparent HTTP proxy. It never parses body bytes.
type Handler struct{ Upstream *transport.Transport }

func New(upstream string) (*Handler, error) {
	t, err := transport.New(transport.Config{BaseURLString: upstream})
	if err != nil {
		return nil, err
	}
	return &Handler{Upstream: t}, nil
}
func NewHandler(t *transport.Transport) *Handler { return &Handler{Upstream: t} }
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.Upstream == nil {
		http.Error(w, "proxy unavailable", http.StatusBadGateway)
		return
	}
	resp, err := h.Upstream.Do(r.Context(), r, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		if isHop(k) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
func isHop(k string) bool {
	switch http.CanonicalHeaderKey(k) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	}
	return false
}

var _ http.Handler = (*Handler)(nil)
