package consensus

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
)

// KV is a single key-value pair returned by List and Watch operations.
type KV struct {
	Key   string
	Value []byte
}

// WatchEvent signals a change to a key under a watched prefix.
type WatchEvent struct {
	KV      KV
	Deleted bool // true if the key was deleted (or its lease expired)
}

// Store is the minimal distributed KV interface used by Registry.
//
// The production implementation (etcdStore) wraps etcd clientv3.
// MemStore is provided for unit tests that run without an etcd cluster.
//
// All methods accept a context; implementations must respect cancellation.
type Store interface {
	// Put sets key=value. leaseID=0 means no lease (key persists indefinitely).
	Put(ctx context.Context, key, value string, leaseID int64) error

	// Delete removes a single key. No-op if the key does not exist.
	Delete(ctx context.Context, key string) error

	// Get returns the value for key. Returns (nil, false, nil) on a miss.
	Get(ctx context.Context, key string) ([]byte, bool, error)

	// List returns all keys sharing the given prefix, in any order.
	List(ctx context.Context, prefix string) ([]KV, error)

	// Watch streams events for all keys sharing prefix until ctx is cancelled.
	// The returned channel is closed when ctx is done.
	Watch(ctx context.Context, prefix string) <-chan WatchEvent

	// Grant creates a lease that expires after ttl seconds. Returns an opaque
	// lease ID. All keys Put with this ID are deleted when the lease expires.
	Grant(ctx context.Context, ttl int64) (int64, error)

	// KeepAlive sends periodic keepalives for leaseID until ctx is cancelled.
	// Returns ctx.Err() on cancellation, or an error if the lease expired.
	KeepAlive(ctx context.Context, leaseID int64) error

	// Revoke explicitly cancels a lease, atomically deleting all its keys.
	Revoke(ctx context.Context, leaseID int64) error
}

// ---- MemStore ---------------------------------------------------------------

// memEntry is one record in the MemStore.
type memEntry struct {
	value   []byte
	leaseID int64 // 0 = no lease
}

// memWatcher is one active Watch subscription.
type memWatcher struct {
	prefix string
	ch     chan WatchEvent
}

// MemStore is an in-memory, goroutine-safe Store used in tests.
//
// Leases never expire — KeepAlive blocks until ctx is cancelled and Grant just
// allocates a monotonically increasing ID. Revoke deletes all keys attached to
// a lease, mirroring the etcd behaviour that tests need to exercise.
type MemStore struct {
	mu        sync.Mutex
	kvs       map[string]memEntry
	leases    map[int64]map[string]struct{} // leaseID → set of owned keys
	nextLease atomic.Int64
	watchers  []*memWatcher
}

// NewMemStore returns an empty MemStore ready for use.
func NewMemStore() *MemStore {
	return &MemStore{
		kvs:    make(map[string]memEntry),
		leases: make(map[int64]map[string]struct{}),
	}
}

func (m *MemStore) Put(_ context.Context, key, value string, leaseID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Remove key from its old lease bucket, if any.
	if old, ok := m.kvs[key]; ok && old.leaseID != 0 {
		delete(m.leases[old.leaseID], key)
	}
	m.kvs[key] = memEntry{value: []byte(value), leaseID: leaseID}
	if leaseID != 0 {
		if m.leases[leaseID] == nil {
			m.leases[leaseID] = make(map[string]struct{})
		}
		m.leases[leaseID][key] = struct{}{}
	}
	m.notifyLocked(WatchEvent{KV: KV{Key: key, Value: []byte(value)}})
	return nil
}

func (m *MemStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteLocked(key)
	return nil
}

// deleteLocked removes a key and fires watchers. Must hold m.mu.
func (m *MemStore) deleteLocked(key string) {
	e, ok := m.kvs[key]
	if !ok {
		return
	}
	delete(m.kvs, key)
	if e.leaseID != 0 {
		delete(m.leases[e.leaseID], key)
	}
	m.notifyLocked(WatchEvent{KV: KV{Key: key}, Deleted: true})
}

func (m *MemStore) Get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.kvs[key]
	if !ok {
		return nil, false, nil
	}
	cp := make([]byte, len(e.value))
	copy(cp, e.value)
	return cp, true, nil
}

func (m *MemStore) List(_ context.Context, prefix string) ([]KV, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []KV
	for k, e := range m.kvs {
		if strings.HasPrefix(k, prefix) {
			cp := make([]byte, len(e.value))
			copy(cp, e.value)
			out = append(out, KV{Key: k, Value: cp})
		}
	}
	return out, nil
}

func (m *MemStore) Watch(ctx context.Context, prefix string) <-chan WatchEvent {
	ch := make(chan WatchEvent, 64)
	w := &memWatcher{prefix: prefix, ch: ch}

	m.mu.Lock()
	m.watchers = append(m.watchers, w)
	m.mu.Unlock()

	go func() {
		<-ctx.Done()
		m.mu.Lock()
		for i, ww := range m.watchers {
			if ww == w {
				m.watchers = append(m.watchers[:i], m.watchers[i+1:]...)
				break
			}
		}
		m.mu.Unlock()
		close(ch)
	}()

	return ch
}

// notifyLocked broadcasts ev to all watchers whose prefix matches the event's
// key. Must be called with m.mu held. Sends are non-blocking; if a watcher's
// buffer is full the event is dropped (tests should use generous buffers).
func (m *MemStore) notifyLocked(ev WatchEvent) {
	for _, w := range m.watchers {
		if strings.HasPrefix(ev.KV.Key, w.prefix) {
			select {
			case w.ch <- ev:
			default:
			}
		}
	}
}

func (m *MemStore) Grant(_ context.Context, _ int64) (int64, error) {
	id := m.nextLease.Add(1)
	m.mu.Lock()
	m.leases[id] = make(map[string]struct{})
	m.mu.Unlock()
	return id, nil
}

// KeepAlive blocks until ctx is cancelled. Leases in MemStore never expire.
func (m *MemStore) KeepAlive(ctx context.Context, _ int64) error {
	<-ctx.Done()
	return ctx.Err()
}

func (m *MemStore) Revoke(_ context.Context, leaseID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.leases[leaseID] {
		m.deleteLocked(key)
	}
	delete(m.leases, leaseID)
	return nil
}
