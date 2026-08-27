// Package app contains the process-level HTTP server for context-lens.
//
// The server currently exposes only the liveness endpoint. Proxy routes are
// added by later layers without changing the process/bootstrap contract.
package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

const (
	// DefaultAddr is the loopback listener used when no address is configured.
	// Keeping the default local avoids exposing a development proxy on the LAN.
	DefaultAddr = "127.0.0.1:8080"

	// HealthPath is the canonical liveness endpoint.
	HealthPath = "/healthz"
)

// Config controls the process-level HTTP server. An empty Addr uses
// DefaultAddr. Timeout values of zero retain net/http's zero-value behavior,
// except for ReadHeaderTimeout and MaxHeaderBytes, which receive conservative
// defaults in NewServer.
type Config struct {
	Addr              string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	MaxHeaderBytes    int
	// ProxyHandler, when set, handles non-health routes such as /v1/responses.
	ProxyHandler http.Handler
}

// Server owns the HTTP mux and its lifecycle. Construct it once during
// bootstrap, register any additional routes before serving, then call
// ListenAndServe (or Serve when a listener is supplied by the caller).
type Server struct {
	config Config
	mux    *http.ServeMux
	http   *http.Server
}

// NewServer constructs a server with the health endpoint registered.
func NewServer(cfg Config) *Server {
	if cfg.Addr == "" {
		cfg.Addr = DefaultAddr
	}
	if cfg.ReadHeaderTimeout == 0 {
		cfg.ReadHeaderTimeout = 10 * time.Second
	}
	if cfg.MaxHeaderBytes == 0 {
		cfg.MaxHeaderBytes = 1 << 20 // 1 MiB
	}

	mux := http.NewServeMux()
	s := &Server{
		config: cfg,
		mux:    mux,
		http: &http.Server{
			Addr:              cfg.Addr,
			Handler:           mux,
			ReadHeaderTimeout: cfg.ReadHeaderTimeout,
			ReadTimeout:       cfg.ReadTimeout,
			WriteTimeout:      cfg.WriteTimeout,
			IdleTimeout:       cfg.IdleTimeout,
			MaxHeaderBytes:    cfg.MaxHeaderBytes,
		},
	}

	// GET patterns also match HEAD requests under net/http's ServeMux rules.
	// Keep /health as a small compatibility alias while /healthz remains the
	// canonical endpoint used by probes.
	mux.HandleFunc("GET "+HealthPath, s.health)
	mux.HandleFunc("GET /health", s.health)
	if cfg.ProxyHandler != nil {
		mux.Handle("/", cfg.ProxyHandler)
	}
	return s
}

// New is a concise alias for NewServer for callers that prefer constructor
// naming consistent with the other backend packages.
func New(cfg Config) *Server { return NewServer(cfg) }

// Handler returns the server's mux. It is useful for httptest and for an
// embedding process that owns the listener itself.
func (s *Server) Handler() http.Handler {
	if s == nil {
		return http.NotFoundHandler()
	}
	return s.mux
}

// Addr reports the configured listen address.
func (s *Server) Addr() string {
	if s == nil {
		return ""
	}
	return s.config.Addr
}

// Handle registers an additional route. Routes must be registered before the
// server starts serving, just as with http.ServeMux.
func (s *Server) Handle(pattern string, handler http.Handler) {
	if s == nil || s.mux == nil {
		panic("app: nil server")
	}
	s.mux.Handle(pattern, handler)
}

// HandleFunc registers an additional route. Routes must be registered before
// the server starts serving, just as with http.ServeMux.
func (s *Server) HandleFunc(pattern string, handler http.HandlerFunc) {
	if s == nil || s.mux == nil {
		panic("app: nil server")
	}
	s.mux.HandleFunc(pattern, handler)
}

// ListenAndServe binds Config.Addr and serves until Shutdown or Close is
// called. A normal shutdown returns http.ErrServerClosed, matching
// net/http.Server's lifecycle contract.
func (s *Server) ListenAndServe() error {
	if s == nil || s.http == nil {
		return errors.New("app: nil server")
	}
	return s.http.ListenAndServe()
}

// Serve serves on l. The caller owns l and may use this method to request an
// ephemeral port in tests or to share listener setup with another process.
func (s *Server) Serve(l net.Listener) error {
	if s == nil || s.http == nil {
		return errors.New("app: nil server")
	}
	if l == nil {
		return errors.New("app: nil listener")
	}
	return s.http.Serve(l)
}

// Shutdown gracefully stops accepting new requests and waits for active
// handlers until ctx expires.
func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil || s.http == nil {
		return errors.New("app: nil server")
	}
	if ctx == nil {
		return errors.New("app: nil context")
	}
	return s.http.Shutdown(ctx)
}

// Close immediately closes listeners and active connections.
func (s *Server) Close() error {
	if s == nil || s.http == nil {
		return errors.New("app: nil server")
	}
	return s.http.Close()
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
}
