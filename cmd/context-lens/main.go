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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"context-lens/backend/app"
	"context-lens/backend/auth"
	"context-lens/backend/config"
	"context-lens/backend/gateway"
	"context-lens/backend/persistence"
	"context-lens/backend/transport"
	"context-lens/backend/workspace"
)

func main() {
	addr := flag.String("addr", configuredAddr(), "HTTP listen address")
	configPath := flag.String("config", configuredConfigPath(), "JSON runtime config path (upstream and optional client auth)")
	flag.Parse()
	if err := validateListenAddr(*addr); err != nil {
		log.Fatalf("invalid listen address: %v", err)
	}

	var fileConfig config.RuntimeConfig
	if strings.TrimSpace(*configPath) != "" {
		var err error
		fileConfig, err = config.LoadRuntimeConfig(*configPath)
		if err != nil {
			log.Fatalf("invalid runtime config: %v", err)
		}
	}

	var workspaceServer *workspace.Server
	var gatewayServer *gateway.Gateway
	serverConfig := app.Config{Addr: *addr}
	upstream := fileConfig.BaseURL
	if upstream == "" {
		upstream = strings.TrimSpace(os.Getenv("CONTEXT_LENS_UPSTREAM"))
	}
	if upstream != "" {
		var err error
		serverCredentialConfigured := fileConfig.EffectiveUpstreamAuthMode() == "configured"
		transportConfig := transport.Config{
			BaseURLString: upstream,
			HeaderPolicy:  transport.HeaderPolicy{ForwardInboundCredentials: !serverCredentialConfigured},
		}
		bearer := ""
		apiKey := ""
		if serverCredentialConfigured {
			bearer = fileConfig.APIKey
		}
		if bearer == "" {
			bearer = strings.TrimSpace(os.Getenv("CONTEXT_LENS_UPSTREAM_BEARER"))
			apiKey = strings.TrimSpace(os.Getenv("CONTEXT_LENS_UPSTREAM_API_KEY"))
		}
		if bearer != "" && apiKey != "" {
			log.Fatal("configure only one upstream credential scheme")
		}
		if bearer != "" {
			transportConfig.HeaderPolicy.Additional = http.Header{"Authorization": {"Bearer " + bearer}}
			transportConfig.HeaderPolicy.ForwardInboundCredentials = false
		}
		if apiKey != "" {
			transportConfig.HeaderPolicy.Additional = http.Header{"X-API-Key": {apiKey}}
			transportConfig.HeaderPolicy.ForwardInboundCredentials = false
		}
		gatewayServer, err = gateway.New(gateway.Config{
			Transport:        transportConfig,
			ClientAuth:       auth.Config{Enabled: fileConfig.ClientAuth.Enabled, APIKey: fileConfig.ClientAuth.APIKey},
			AllowNonLoopback: os.Getenv("CONTEXT_LENS_ALLOW_NON_LOOPBACK") == "1",
			MaxBodyBytes:     128 << 20,
			DurableCatalogPath: func() string {
				if os.Getenv("CONTEXT_LENS_DURABLE") == "1" {
					return filepath.Join(configuredDataDir(), "catalog.sqlite")
				}
				return ""
			}(),
			StoreConfig: persistence.Config{
				MaxArtifactBytes: 128 << 20,
				MaxTotalBytes:    2 << 30,
				MaxMemoryBytes:   64 << 20,
				MaxArtifacts:     10_000,
				SpillRoot:        filepath.Join(configuredDataDir(), "artifacts"),
				// Standalone artifacts can still be referenced by the registry/catalog after
				// capture completes. Disable independent expiry until retention is
				// owner-aware; capacity stays bounded by MaxTotalBytes and explicit Clear.
				TTL:             0,
				CleanupInterval: 0,
			},
		})
		if err != nil {
			log.Fatalf("invalid CONTEXT_LENS_UPSTREAM: %v", err)
		}
		workspaceServer = workspace.New(workspace.Config{
			Backend:          gatewayServer.WorkspaceBackend(),
			Artifacts:        gatewayServer.Store(),
			Policy:           gatewayServer.Policy(),
			Events:           gatewayServer,
			ClearQueue:       gatewayServer.ClearQueue,
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
		if gatewayServer != nil {
			_ = gatewayServer.Close()
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

func configuredConfigPath() string {
	if value := strings.TrimSpace(os.Getenv("CONTEXT_LENS_CONFIG")); value != "" {
		return value
	}
	if _, err := os.Stat("config.local.json"); err == nil {
		return "config.local.json"
	}
	return ""
}

func configuredDataDir() string {
	if value := strings.TrimSpace(os.Getenv("CONTEXT_LENS_DATA_DIR")); value != "" {
		return value
	}
	return ".context-lens-run"
}

func configuredAddr() string {
	if value := strings.TrimSpace(os.Getenv("CONTEXT_LENS_ADDR")); value != "" {
		return value
	}
	return app.DefaultAddr
}
