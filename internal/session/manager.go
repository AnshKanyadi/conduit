package session

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anshk/conduit/internal/observability"
	"github.com/anshk/conduit/internal/pty"
)

// Hooks carries optional callbacks that fire on session lifecycle events.
// Wire these in main.go to connect the session layer to consensus (Layer 8)
// without creating an import cycle.
type Hooks struct {
	// OnSessionCreate is called (in a goroutine) after a session is created
	// and registered. ctx is context.Background(); id is the new session ID.
	OnSessionCreate func(ctx context.Context, id uint32)

	// OnSessionRemove is called synchronously before a session is removed from
	// the manager map. id is the session being torn down.
	OnSessionRemove func(id uint32)
}

// Manager maintains the set of live sessions and handles session creation,
// lookup, and removal. All methods are safe for concurrent use.
//
// Why a Manager instead of a global map?
// Dependency injection: tests can create isolated managers with short
// SuspendGrace values without touching global state. Production code gets
// the standard 30-second grace. Multiple Managers could also exist for
// namespace isolation if we wanted per-tenant session pools.
type Manager struct {
	mu       sync.RWMutex
	sessions map[uint32]*Session

	// idCounter generates session IDs. Starts at 0; Add(1) makes the first
	// session ID = 1. ID 0 is reserved to mean "no session" in the protocol.
	idCounter atomic.Uint32

	// SuspendGrace controls how long a session lives without connected clients.
	// Default: 30 seconds. Tests should set this to a small value (e.g. 100ms)
	// to avoid slow tests.
	SuspendGrace time.Duration

	// Hooks carries optional lifecycle callbacks. All fields are nil by default
	// (no-op). Set them before the first call to Create.
	Hooks Hooks
}

// NewManager constructs a Manager with the default 30-second suspend grace.
func NewManager() *Manager {
	return &Manager{
		sessions:     make(map[uint32]*Session),
		SuspendGrace: defaultSuspendGrace,
	}
}

// Create spawns a new PTY process and registers a session for it.
//
// The returned session's context is derived from parentCtx — cancelling
// parentCtx will cancel the session context, which kills the PTY process.
// Typically parentCtx is the server's root context (cancelled on shutdown).
//
// The session's own cancel function is called by Session.Close(), which is
// triggered either by the suspend timer or by an explicit Remove.
func (m *Manager) Create(parentCtx context.Context, cfg Config) (*Session, error) {
	id := m.nextID()
	grace := m.grace()

	// Derive the session context from the parent. Cancelling this kills the
	// PTY's child process (exec.CommandContext semantics).
	sessCtx, cancel := context.WithCancel(parentCtx)

	proc, err := pty.Spawn(sessCtx, pty.Config{
		Command: cfg.Command,
		Env:     cfg.Env,
		Rows:    cfg.Rows,
		Cols:    cfg.Cols,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("session: spawn pty for session %d: %w", id, err)
	}

	sess := &Session{
		ID:           id,
		CreatedAt:    time.Now(),
		proc:         proc,
		state:        StateActive,
		sinks:        make(map[uint32]Sink),
		suspendGrace: grace,
		onExpire:     m.Remove, // timer fires → manager removes + closes session
		ctx:          sessCtx,
		cancel:       cancel,
	}

	m.mu.Lock()
	m.sessions[id] = sess
	m.mu.Unlock()

	observability.SessionsActive.Inc()

	if m.Hooks.OnSessionCreate != nil {
		go m.Hooks.OnSessionCreate(context.Background(), id)
	}

	return sess, nil
}

// Get retrieves an existing session by ID.
// Returns (nil, false) if the session doesn't exist or has been closed.
func (m *Manager) Get(id uint32) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sess, ok := m.sessions[id]
	return sess, ok
}

// Remove deregisters a session and closes it.
// Idempotent — if the session doesn't exist, Remove is a no-op.
// Called by the suspend timer's callback and by pumpPTY when the PTY exits.
func (m *Manager) Remove(id uint32) {
	m.mu.Lock()
	sess, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	m.mu.Unlock()

	if ok {
		if m.Hooks.OnSessionRemove != nil {
			m.Hooks.OnSessionRemove(id)
		}
		observability.SessionsActive.Dec()
		sess.Close() // idempotent — safe even if already closed
	}
}

// ActiveCount returns the number of sessions currently tracked.
// Includes both Active and Suspended sessions (they both have live PTYs).
func (m *Manager) ActiveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

// nextID atomically increments and returns the next session ID.
// IDs are monotonically increasing per-process; they are NOT globally unique
// across nodes. The consensus layer (Layer 8) will add node-scoped uniqueness.
func (m *Manager) nextID() uint32 {
	return m.idCounter.Add(1)
}

// grace returns the effective suspend grace duration.
func (m *Manager) grace() time.Duration {
	if m.SuspendGrace == 0 {
		return defaultSuspendGrace
	}
	return m.SuspendGrace
}
