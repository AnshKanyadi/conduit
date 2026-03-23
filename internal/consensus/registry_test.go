package consensus_test

import (
	"context"
	"testing"
	"time"

	"github.com/anshk/conduit/internal/consensus"
)

// helper: create a Registry backed by a fresh MemStore.
func newTestRegistry(nodeID, addr string) (*consensus.Registry, *consensus.MemStore) {
	store := consensus.NewMemStore()
	return consensus.NewRegistry(store, nodeID, addr), store
}

// helper: start a registry's Run loop; return cancel and a channel for the
// run error. Waits briefly for the node key to be published before returning.
func runRegistry(t *testing.T, reg *consensus.Registry) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- reg.Run(ctx) }()
	// Give Run time to Grant the lease and Put the node key.
	time.Sleep(10 * time.Millisecond)
	return cancel, errCh
}

// --- Node registration -------------------------------------------------------

func TestRegistry_NodeAppearsAfterRun(t *testing.T) {
	reg, _ := newTestRegistry("node-1", "localhost:8080")
	cancel, _ := runRegistry(t, reg)
	defer cancel()

	nodes, err := reg.ListNodes(context.Background())
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("want 1 node, got %d", len(nodes))
	}
	if nodes[0].ID != "node-1" {
		t.Errorf("node ID: got %q, want %q", nodes[0].ID, "node-1")
	}
	if nodes[0].Addr != "localhost:8080" {
		t.Errorf("node Addr: got %q, want %q", nodes[0].Addr, "localhost:8080")
	}
}

func TestRegistry_NodeRemovedAfterCancel(t *testing.T) {
	reg, _ := newTestRegistry("node-1", "localhost:8080")
	cancel, errCh := runRegistry(t, reg)

	// Verify the node is present.
	nodes, _ := reg.ListNodes(context.Background())
	if len(nodes) != 1 {
		t.Fatalf("pre-cancel: want 1 node, got %d", len(nodes))
	}

	// Cancel Run; wait for it to revoke the lease.
	cancel()
	if err := <-errCh; err != nil && err != context.Canceled {
		t.Errorf("Run returned unexpected error: %v", err)
	}

	nodes, err := reg.ListNodes(context.Background())
	if err != nil {
		t.Fatalf("ListNodes after cancel: %v", err)
	}
	if len(nodes) != 0 {
		t.Errorf("post-cancel: want 0 nodes, got %d", len(nodes))
	}
}

func TestRegistry_MultipleNodes(t *testing.T) {
	store := consensus.NewMemStore()
	reg1 := consensus.NewRegistry(store, "node-1", "host1:8080")
	reg2 := consensus.NewRegistry(store, "node-2", "host2:8080")

	cancel1, _ := runRegistry(t, reg1)
	defer cancel1()
	cancel2, _ := runRegistry(t, reg2)
	defer cancel2()

	nodes, err := reg1.ListNodes(context.Background())
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(nodes) != 2 {
		t.Errorf("want 2 nodes, got %d", len(nodes))
	}
}

// --- Session routing ---------------------------------------------------------

func TestRegistry_PublishAndLookupSession(t *testing.T) {
	reg, _ := newTestRegistry("node-1", "localhost:8080")
	cancel, _ := runRegistry(t, reg)
	defer cancel()

	if err := reg.PublishSession(context.Background(), 42); err != nil {
		t.Fatalf("PublishSession: %v", err)
	}

	node, ok, err := reg.LookupSession(context.Background(), 42)
	if err != nil {
		t.Fatalf("LookupSession: %v", err)
	}
	if !ok {
		t.Fatal("LookupSession: session not found")
	}
	if node.ID != "node-1" {
		t.Errorf("owning node: got %q, want %q", node.ID, "node-1")
	}
	if node.Addr != "localhost:8080" {
		t.Errorf("owning addr: got %q, want %q", node.Addr, "localhost:8080")
	}
}

func TestRegistry_LookupSession_NotFound(t *testing.T) {
	reg, _ := newTestRegistry("node-1", "localhost:8080")
	cancel, _ := runRegistry(t, reg)
	defer cancel()

	_, ok, err := reg.LookupSession(context.Background(), 999)
	if err != nil {
		t.Fatalf("LookupSession: %v", err)
	}
	if ok {
		t.Error("expected session 999 to not be found")
	}
}

func TestRegistry_RevokeSession(t *testing.T) {
	reg, _ := newTestRegistry("node-1", "localhost:8080")
	cancel, _ := runRegistry(t, reg)
	defer cancel()

	_ = reg.PublishSession(context.Background(), 7)

	if err := reg.RevokeSession(context.Background(), 7); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}

	_, ok, err := reg.LookupSession(context.Background(), 7)
	if err != nil {
		t.Fatalf("LookupSession after revoke: %v", err)
	}
	if ok {
		t.Error("session 7 still visible after RevokeSession")
	}
}

func TestRegistry_SessionRemovedWithNode(t *testing.T) {
	// Publishing a session with the node's lease means the session key must
	// disappear when the node deregisters (Run ctx cancelled → Revoke).
	reg, _ := newTestRegistry("node-1", "localhost:8080")
	cancel, errCh := runRegistry(t, reg)

	_ = reg.PublishSession(context.Background(), 55)

	cancel()
	if err := <-errCh; err != nil && err != context.Canceled {
		t.Fatalf("Run: %v", err)
	}

	_, ok, _ := reg.LookupSession(context.Background(), 55)
	if ok {
		t.Error("session 55 still present after node lease revoke")
	}
}

// --- Membership watch --------------------------------------------------------

func TestRegistry_WatchNodes_Join(t *testing.T) {
	watchCtx, watchCancel := context.WithCancel(context.Background())
	defer watchCancel()

	reg, _ := newTestRegistry("node-1", "localhost:8080")
	events := reg.WatchNodes(watchCtx)

	cancel, _ := runRegistry(t, reg)
	defer cancel()

	select {
	case ev := <-events:
		if ev.Removed {
			t.Errorf("first event should be a join, got remove")
		}
		if ev.Node.ID != "node-1" {
			t.Errorf("join event node ID: got %q, want %q", ev.Node.ID, "node-1")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for join event")
	}
}

func TestRegistry_WatchNodes_Leave(t *testing.T) {
	watchCtx, watchCancel := context.WithCancel(context.Background())
	defer watchCancel()

	reg, _ := newTestRegistry("node-1", "localhost:8080")
	events := reg.WatchNodes(watchCtx)

	cancel, errCh := runRegistry(t, reg)

	// Drain the join event.
	select {
	case <-events:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for join event")
	}

	cancel()
	if err := <-errCh; err != nil && err != context.Canceled {
		t.Fatalf("Run: %v", err)
	}

	select {
	case ev := <-events:
		if !ev.Removed {
			t.Errorf("expected remove event, got join for %q", ev.Node.ID)
		}
		if ev.Node.ID != "node-1" {
			t.Errorf("remove event node ID: got %q, want %q", ev.Node.ID, "node-1")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for leave event")
	}
}

// --- MemStore lease mechanics ------------------------------------------------

func TestMemStore_RevokeDeletesAllLeaseKeys(t *testing.T) {
	store := consensus.NewMemStore()
	ctx := context.Background()

	leaseID, _ := store.Grant(ctx, 10)
	_ = store.Put(ctx, "/a", "1", leaseID)
	_ = store.Put(ctx, "/b", "2", leaseID)
	_ = store.Put(ctx, "/c", "3", 0) // no lease — must survive

	if err := store.Revoke(ctx, leaseID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	if _, ok, _ := store.Get(ctx, "/a"); ok {
		t.Error("/a should be gone after Revoke")
	}
	if _, ok, _ := store.Get(ctx, "/b"); ok {
		t.Error("/b should be gone after Revoke")
	}
	if _, ok, _ := store.Get(ctx, "/c"); !ok {
		t.Error("/c (no lease) should still exist after Revoke")
	}
}

func TestMemStore_PutUpdatesLeaseOwnership(t *testing.T) {
	// Re-putting a key with a different lease should move it to the new lease.
	store := consensus.NewMemStore()
	ctx := context.Background()

	lease1, _ := store.Grant(ctx, 10)
	lease2, _ := store.Grant(ctx, 10)

	_ = store.Put(ctx, "/k", "v1", lease1)
	_ = store.Put(ctx, "/k", "v2", lease2) // re-assign to lease2

	// Revoking lease1 must NOT delete /k (it belongs to lease2 now).
	_ = store.Revoke(ctx, lease1)
	if _, ok, _ := store.Get(ctx, "/k"); !ok {
		t.Error("/k should still exist after revoking its former lease")
	}

	// Revoking lease2 must delete /k.
	_ = store.Revoke(ctx, lease2)
	if _, ok, _ := store.Get(ctx, "/k"); ok {
		t.Error("/k should be gone after revoking its current lease")
	}
}
