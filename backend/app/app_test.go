package app

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHealthEndpoints(t *testing.T) {
	server := NewServer(Config{})

	for _, path := range []string{HealthPath, "/health"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			recorder := httptest.NewRecorder()

			server.Handler().ServeHTTP(recorder, req)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
			}
			if got := recorder.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
				t.Fatalf("content type = %q", got)
			}
			if got := recorder.Body.String(); got != "{\"status\":\"ok\"}\n" {
				t.Fatalf("body = %q", got)
			}
		})
	}
}

func TestHealthRejectsNonGet(t *testing.T) {
	server := NewServer(Config{})
	req := httptest.NewRequest(http.MethodPost, HealthPath, nil)
	recorder := httptest.NewRecorder()

	server.Handler().ServeHTTP(recorder, req)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusMethodNotAllowed)
	}
}

func TestServeAndShutdown(t *testing.T) {
	server := NewServer(Config{Addr: "ignored by Serve"})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()

	client := &http.Client{Timeout: time.Second}
	response, err := client.Get("http://" + listener.Addr().String() + HealthPath)
	if err != nil {
		t.Fatalf("health request: %v", err)
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil {
		t.Fatalf("read health response: %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("close health response: %v", closeErr)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if string(body) != "{\"status\":\"ok\"}\n" {
		t.Fatalf("body = %q", body)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("serve error = %v, want %v", err, http.ErrServerClosed)
		}
	case <-ctx.Done():
		t.Fatal("server did not stop after shutdown")
	}
}
