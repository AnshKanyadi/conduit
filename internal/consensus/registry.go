package consensus

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"go.uber.org/zap"

	"github.com/anshk/conduit/internal/observability"
)

// leaseTTL is the etcd lease TTL in seconds.
// Keepalives are sent at TTL/3 frequency by the etcd client, so nodes missing
// three consecutive heartbeats are declared dead. 15 s gives a 5 s interval
// with a 15 s worst-case detection window — acceptable for interactive terminals.
const leaseTTL = 15

// Key prefixes. The hierarchy allows Watch to target subtrees cheaply.
const (
	prefixNodes    = "/conduit/nodes/"
	prefixSessions = "/conduit/sessions/"
)

// Node describes a live Conduit peer visible in the distributed store.
type Node struct {
	ID   string `json:"id"`
	Addr string `json:"addr"`
}

// NodeEvent is emitted by WatchNodes on membership changes.
type NodeEvent struct {
	Node    Node
	Removed bool // true if the node left or its lease expired
}

// Registry manages this node's presence in the distributed store and provides
// session routing information to the transport layer.
//
// Lifecycle:
//
//	reg := consensus.NewRegistry(store, nodeID, addr)
//	go reg.Run(ctx)               // publishes presence; blocks until cancelled
//	reg.PublishSession(ctx, id)   // record session ownership
//	reg.LookupSession(ctx, id)    // find owning node for routing
//	reg.ListNodes(ctx)            // enumerate live peers
//	reg.WatchNodes(ctx)           // stream membership events
//
// Run must be called before PublishSession. In single-node mode, set registry
// to nil in the transport handler and skip Run entirely.
type Registry struct {
	store    Store
	nodeID   string
	addr     string
	leaseTTL int64

	mu      sync.RWMutex
	leaseID int64 // non-zero only while Run is active
}

// NewRegistry creates a Registry backed by store. Call go reg.Run(ctx) to
// begin publishing this node's presence.
func NewRegistry(store Store, nodeID, addr string) *Registry {
	return &Registry{
		store:    store,
		nodeID:   nodeID,
		addr:     addr,
		leaseTTL: leaseTTL,
	}
}

// Run registers this node with the store and drives lease keepalives until ctx
// is cancelled. On cancellation, Run revokes the lease — atomically removing
// the node key and all associated session keys — before returning.
//
// Must be called exactly once, as a goroutine.
func (r *Registry) Run(ctx context.Context) error {
	leaseID, err := r.store.Grant(ctx, r.leaseTTL)
	if err != nil {
		return fmt.Errorf("consensus: grant lease: %w", err)
	}

	r.mu.Lock()
	r.leaseID = leaseID
	r.mu.Unlock()

	nodeKey := prefixNodes + r.nodeID
	nodeVal, err := json.Marshal(Node{ID: r.nodeID, Addr: r.addr})
	if err != nil {
		return fmt.Errorf("consensus: marshal node: %w", err)
	}
	if err := r.store.Put(ctx, nodeKey, string(nodeVal), leaseID); err != nil {
		return fmt.Errorf("consensus: put node key: %w", err)
	}

	observability.L.Info("consensus: node registered",
		zap.String("node_id", r.nodeID),
		zap.String("addr", r.addr),
		zap.Int64("lease_id", leaseID),
	)

	// Block until ctx is cancelled, sending keepalives.
	_ = r.store.KeepAlive(ctx, leaseID)

	// ctx is cancelled; use a fresh context for the revoke RPC.
	revokeCtx := context.Background()
	if err := r.store.Revoke(revokeCtx, leaseID); err != nil {
		observability.L.Warn("consensus: revoke lease failed", zap.Error(err))
	}

	r.mu.Lock()
	r.leaseID = 0
	r.mu.Unlock()

	observability.L.Info("consensus: node deregistered", zap.String("node_id", r.nodeID))
	return nil
}

// PublishSession records that this node owns sessionID.
//
// The key is attached to the node's existing lease, so it is automatically
// deleted if the node crashes without calling RevokeSession. Call this after
// the session is live and accepting clients.
func (r *Registry) PublishSession(ctx context.Context, sessionID uint32) error {
	r.mu.RLock()
	leaseID := r.leaseID
	r.mu.RUnlock()

	return r.store.Put(ctx, sessionKey(sessionID), r.nodeID, leaseID)
}

// RevokeSession removes the session routing entry on a clean shutdown.
// This is advisory — the node lease handles cleanup on crash.
func (r *Registry) RevokeSession(ctx context.Context, sessionID uint32) error {
	return r.store.Delete(ctx, sessionKey(sessionID))
}

// LookupSession returns the Node that currently owns sessionID.
// Returns (Node{}, false, nil) if no node claims the session (it may have
// ended or the owning node may have crashed and the lease not yet expired).
func (r *Registry) LookupSession(ctx context.Context, sessionID uint32) (Node, bool, error) {
	ownerIDBytes, ok, err := r.store.Get(ctx, sessionKey(sessionID))
	if err != nil || !ok {
		return Node{}, false, err
	}

	nodeVal, ok, err := r.store.Get(ctx, prefixNodes+string(ownerIDBytes))
	if err != nil || !ok {
		return Node{}, false, err
	}

	var n Node
	if err := json.Unmarshal(nodeVal, &n); err != nil {
		return Node{}, false, fmt.Errorf("consensus: unmarshal node: %w", err)
	}
	return n, true, nil
}

// ListNodes returns all currently-live Conduit nodes visible in the store.
func (r *Registry) ListNodes(ctx context.Context) ([]Node, error) {
	kvs, err := r.store.List(ctx, prefixNodes)
	if err != nil {
		return nil, err
	}
	nodes := make([]Node, 0, len(kvs))
	for _, kv := range kvs {
		var n Node
		if err := json.Unmarshal(kv.Value, &n); err != nil {
			continue // skip malformed entries rather than aborting
		}
		nodes = append(nodes, n)
	}
	return nodes, nil
}

// WatchNodes returns a channel that emits NodeEvents as nodes join or leave.
// The channel is closed when ctx is cancelled.
func (r *Registry) WatchNodes(ctx context.Context) <-chan NodeEvent {
	raw := r.store.Watch(ctx, prefixNodes)
	out := make(chan NodeEvent, 16)

	go func() {
		defer close(out)
		for ev := range raw {
			ne := NodeEvent{Removed: ev.Deleted}
			if ev.Deleted {
				// Node ID is the suffix after the prefix.
				ne.Node.ID = strings.TrimPrefix(ev.KV.Key, prefixNodes)
			} else {
				if err := json.Unmarshal(ev.KV.Value, &ne.Node); err != nil {
					continue
				}
			}
			select {
			case out <- ne:
			case <-ctx.Done():
				return
			}
		}
	}()

	return out
}

func sessionKey(id uint32) string {
	return fmt.Sprintf("%s%d", prefixSessions, id)
}
