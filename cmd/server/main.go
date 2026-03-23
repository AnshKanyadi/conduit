// Package main is the Conduit server entrypoint.
// It wires together the session manager and WebSocket transport, then
// listens for connections. All real logic lives in internal/ packages.
package main

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"

	"github.com/anshk/conduit/internal/consensus"
	"github.com/anshk/conduit/internal/observability"
	"github.com/anshk/conduit/internal/session"
	"github.com/anshk/conduit/internal/transport"
)

func main() {
	// Detect development mode via the CONDUIT_DEV environment variable.
	// Production deployments leave it unset (JSON logging for log aggregators).
	dev := os.Getenv("CONDUIT_DEV") != ""
	if err := observability.Init(dev); err != nil {
		// Fall back to stderr if the logger can't be built — this should
		// never happen in practice (zap.NewProduction/Development rarely fail).
		panic("observability.Init: " + err.Error())
	}
	defer observability.L.Sync() //nolint:errcheck

	// --- Session manager -----------------------------------------------------

	manager := session.NewManager()

	// --- Consensus registry (optional) ---------------------------------------
	// Set CONDUIT_ETCD_ENDPOINTS to a comma-separated list of etcd addresses
	// (e.g. "localhost:2379") to enable multi-node mode. When unset, Conduit
	// runs in single-node mode with no distributed coordination.

	var registry *consensus.Registry

	etcdEndpoints := os.Getenv("CONDUIT_ETCD_ENDPOINTS")
	if etcdEndpoints != "" {
		nodeID := os.Getenv("CONDUIT_NODE_ID")
		if nodeID == "" {
			if h, err := os.Hostname(); err == nil {
				nodeID = h
			} else {
				nodeID = "unknown"
			}
		}
		nodeAddr := os.Getenv("CONDUIT_NODE_ADDR")
		if nodeAddr == "" {
			nodeAddr = ":8080"
		}

		store, etcdClient, err := consensus.NewEtcdStore(clientv3.Config{
			Endpoints:   strings.Split(etcdEndpoints, ","),
			DialTimeout: 5 * time.Second,
		})
		if err != nil {
			observability.L.Fatal("etcd connect failed", zap.Error(err))
		}
		defer etcdClient.Close()

		registry = consensus.NewRegistry(store, nodeID, nodeAddr)

		// Wire session lifecycle hooks so the registry mirrors the manager's
		// session map. Both hooks are goroutine-safe.
		manager.Hooks.OnSessionCreate = func(ctx context.Context, id uint32) {
			if err := registry.PublishSession(ctx, id); err != nil {
				observability.L.Warn("consensus: publish session failed",
					zap.Uint32("session_id", id),
					zap.Error(err),
				)
			}
		}
		manager.Hooks.OnSessionRemove = func(id uint32) {
			if err := registry.RevokeSession(context.Background(), id); err != nil {
				observability.L.Warn("consensus: revoke session failed",
					zap.Uint32("session_id", id),
					zap.Error(err),
				)
			}
		}

		// Start the registry's keepalive loop. It blocks until the server's
		// root context is cancelled (on shutdown), then revokes the lease.
		// We use a background goroutine; graceful shutdown is handled by
		// OS signal handling in a future layer.
		go func() {
			if err := registry.Run(context.Background()); err != nil {
				observability.L.Error("consensus: registry stopped", zap.Error(err))
			}
		}()

		observability.L.Info("multi-node mode enabled",
			zap.String("node_id", nodeID),
			zap.String("node_addr", nodeAddr),
			zap.Strings("etcd_endpoints", strings.Split(etcdEndpoints, ",")),
		)
	} else {
		observability.L.Info("single-node mode (CONDUIT_ETCD_ENDPOINTS not set)")
	}

	// --- HTTP mux -------------------------------------------------------------

	handler := transport.NewHandler(manager, session.Config{
		Command: []string{"/bin/sh"},
		Rows:    24,
		Cols:    80,
	}, registry)

	mux := http.NewServeMux()
	mux.Handle("/ws", handler)
	mux.Handle("/metrics", observability.MetricsHandler())

	addr := ":8080"
	observability.L.Info("Conduit server starting",
		zap.String("addr", addr),
		zap.Bool("dev_mode", dev),
	)

	if err := http.ListenAndServe(addr, mux); err != nil {
		observability.L.Fatal("server exited", zap.Error(err))
	}
}
