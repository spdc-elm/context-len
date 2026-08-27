// Command context-lens starts the local context-lens HTTP process.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"context-lens/backend/app"
	"context-lens/backend/gateway"
	"context-lens/backend/persistence"
	"context-lens/backend/transport"
	"context-lens/backend/workspace"
)

func main() {
	addr := flag.String("addr", configuredAddr(), "HTTP listen address")
	flag.Parse()
	if err := validateListenAddr(*addr); err != nil {
		log.Fatalf("invalid listen address: %v", err)
	}

	var workspaceServer *workspace.Server
	var gatewayServer *gateway.Gateway
	serverConfig := app.Config{Addr: *addr}
	if upstream := strings.TrimSpace(os.Getenv("CONTEXT_LENS_UPSTREAM")); upstream != "" {
		var err error
		transportConfig := transport.Config{BaseURLString: upstream, HeaderPolicy: transport.DefaultHeaderPolicy()}
		bearer := strings.TrimSpace(os.Getenv("CONTEXT_LENS_UPSTREAM_BEARER"))
		apiKey := strings.TrimSpace(os.Getenv("CONTEXT_LENS_UPSTREAM_API_KEY"))
		if bearer != "" && apiKey != "" {
			log.Fatal("configure only one upstream credential scheme")
		}
		if bearer != "" {
			transportConfig.HeaderPolicy.Additional = http.Header{"Authorization": {"Bearer " + bearer}}
		}
		if apiKey != "" {
			transportConfig.HeaderPolicy.Additional = http.Header{"X-API-Key": {apiKey}}
		}
		gatewayServer, err = gateway.New(gateway.Config{
			Transport:    transportConfig,
			MaxBodyBytes: 128 << 20,
			StoreConfig: persistence.Config{
				MaxArtifactBytes: 128 << 20,
				MaxTotalBytes:    2 << 30,
				MaxMemoryBytes:   512 << 20,
				MaxArtifacts:     10_000,
				TTL:              24 * time.Hour,
				CleanupInterval:  5 * time.Minute,
			},
		})
		if err != nil {
			log.Fatalf("invalid CONTEXT_LENS_UPSTREAM: %v", err)
		}
		workspaceServer = workspace.New(workspace.Config{
			Registry:         gatewayServer.Registry(),
			Artifacts:        gatewayServer.Store(),
			Policy:           gatewayServer.Policy(),
			Events:           gatewayServer,
			MaxArtifactBytes: 128 << 20,
		})
		routes := http.NewServeMux()
		routes.Handle("/api/", workspaceServer)
		routes.Handle("/api", workspaceServer)
		routes.Handle("/", gatewayServer)
		serverConfig.ProxyHandler = routes
	}
	server := app.NewServer(serverConfig)
	defer func() {
		if workspaceServer != nil {
			_ = workspaceServer.Close()
		}
		if gatewayServer != nil && gatewayServer.Store() != nil {
			_ = gatewayServer.Store().Close()
		}
	}()
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()

	log.Printf("context-lens listening on %s", server.Addr())

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("context-lens server stopped: %v", err)
			os.Exit(1)
		}
	case sig := <-signals:
		log.Printf("received %s; shutting down", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := server.Shutdown(ctx)
		cancel()
		if err != nil {
			log.Printf("context-lens shutdown failed: %v", err)
			_ = server.Close()
			os.Exit(1)
		}

		// Wait for the serving goroutine so the process does not exit while a
		// handler is still unwinding.
		if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("context-lens server stopped: %v", err)
			os.Exit(1)
		}
	}
}

func validateListenAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("context-lens must listen on loopback, got %q", host)
	}
	return nil
}

func configuredAddr() string {
	if value := strings.TrimSpace(os.Getenv("CONTEXT_LENS_ADDR")); value != "" {
		return value
	}
	return app.DefaultAddr
}
