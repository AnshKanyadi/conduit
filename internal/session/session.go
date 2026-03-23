// Package session manages the lifecycle of Conduit terminal sessions.
//
// A session is the authoritative unit of state — it outlives any individual
// WebSocket connection. When the last client disconnects, the session enters
// a suspended state and keeps the PTY alive for up to 30 seconds (configurable).
// If a client reconnects within that window, the session resumes seamlessly.
// If the grace period expires, the session and its PTY are torn down.
//
// Dependency direction: session imports pty, nothing else internal.
// transport imports session (not the reverse), keeping the graph acyclic.
package session

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/anshk/conduit/internal/pty"
)

// defaultSuspendGrace is how long a session stays alive after the last client
// disconnects. Set lower in tests via Manager.SuspendGrace.
const defaultSuspendGrace = 30 * time.Second

// State is the coarse lifecycle state of a session.
type State uint8

const (
	// StateActive: at least one client is connected; PTY is running.
	StateActive State = iota

	// StateSuspended: no clients connected; PTY is still running.
	// The session will transition to StateClosed after SuspendGrace elapses.
	StateSuspended

	// StateClosed: session is permanently torn down. PTY has been closed.
	// This state is terminal — once closed, a session cannot be reopened.
	StateClosed
)

func (s State) String() string {
	switch s {
	case StateActive:
		return "active"
	case StateSuspended:
		return "suspended"
	case StateClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// Sink is implemented by anything that can receive outbound frames.
// transport.Client implements this in Layer 4.
// In Layer 5, the relay hub will implement it with flow control.
//
// Send returns false if the sink cannot accept the frame (full buffer,
// closed connection). The session's Broadcast does not remove slow sinks —
// that policy belongs in the flow-control layer.
type Sink interface {
	Send(frame []byte) bool
}

// Config holds the parameters for starting a new session.
type Config struct {
	Command []string // e.g. []string{"/bin/sh"}
	Env     []string // extra environment variables for the shell
	Rows    uint16
	Cols    uint16
}

// Session represents a running (or suspended) terminal session.
// All exported methods are safe for concurrent use.
type Session struct {
	ID        uint32
	CreatedAt time.Time

	proc *pty.Process // the attached PTY and its child process

	mu           sync.RWMutex
	state        State
	sinks        map[uint32]Sink // registered output recipients, keyed by client ID
	suspendTimer *time.Timer
	suspendGrace time.Duration

	// onExpire is called (with this session's ID) when the suspend grace
	// period elapses. Typically set to manager.Remove so the manager cleans
	// up its own map.
	onExpire func(uint32)

	ctx    context.Context    // cancelled by Close(); passed to pty.Spawn
	cancel context.CancelFunc // cancels ctx, which kills the child process
}

// AddSink registers a client to receive PTY output frames.
//
// If the session is Suspended (last client disconnected, timer running),
// AddSink cancels the timer and transitions back to Active. This is the
// reconnection fast path — no new PTY is spawned, the existing shell
// continues running as if nothing happened.
//
// Returns an error if the session is already Closed.
func (s *Session) AddSink(id uint32, sink Sink) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state == StateClosed {
		return fmt.Errorf("session %d: already closed", s.ID)
	}
	if s.state == StateSuspended {
		// Race-free timer cancel: Stop() returns false if the timer already
		// fired. In that case, the onExpire callback is already queued or
		// running. We still flip the state here; the callback will call
		// manager.Remove which is idempotent.
		s.suspendTimer.Stop()
		s.state = StateActive
	}
	s.sinks[id] = sink
	return nil
}

// RemoveSink deregisters a client. If this was the last sink, the session
// transitions to Suspended and the grace-period timer is armed.
//
// Why keep the PTY alive on suspend?
// Reconnection is a normal event — flaky WiFi, browser tab refresh, mobile
// screen sleep. Tearing down and recreating the PTY would lose in-progress
// work (running commands, unsaved editor buffers). Suspension is cheap:
// the PTY just keeps running with its output buffered by the kernel.
func (s *Session) RemoveSink(id uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.sinks, id)

	if len(s.sinks) == 0 && s.state == StateActive {
		s.state = StateSuspended
		s.suspendTimer = time.AfterFunc(s.suspendGrace, func() {
			if s.onExpire != nil {
				s.onExpire(s.ID)
			}
		})
	}
}

// Broadcast sends frame to every registered sink.
// Non-blocking: if a sink's buffer is full, Send returns false and we move on.
// In Layer 4 this means slow clients silently drop frames; Layer 5 (relay)
// will replace this with sliding-window flow control and delta compression.
func (s *Session) Broadcast(frame []byte) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sink := range s.sinks {
		sink.Send(frame)
	}
}

// WriteKeystroke sends key input bytes to the PTY's stdin.
// Called by the transport layer for each KEYSTROKE frame received.
func (s *Session) WriteKeystroke(data []byte) (int, error) {
	return s.proc.Write(data)
}

// ReadOutput reads raw bytes from the PTY's stdout/stderr.
// Blocks until data is available or the PTY closes.
// The transport's pumpPTY goroutine calls this in a tight loop.
func (s *Session) ReadOutput(buf []byte) (int, error) {
	return s.proc.Read(buf)
}

// Resize updates the terminal window size and delivers SIGWINCH to the shell.
func (s *Session) Resize(rows, cols uint16) error {
	return s.proc.Resize(rows, cols)
}

// State returns the current lifecycle state. Safe for concurrent reads.
func (s *Session) State() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// SinkCount returns the number of currently registered sinks.
func (s *Session) SinkCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sinks)
}

// Context returns the session's context. It is cancelled when Close is called,
// which in turn kills the child process via exec.CommandContext.
func (s *Session) Context() context.Context {
	return s.ctx
}

// Suspend transitions an Active session to Suspended, arming the grace-period
// timer. Called by the relay Hub's OnEmpty callback when the last client
// disconnects. If the session is already Suspended or Closed, it is a no-op.
//
// Why expose this separately from RemoveSink?
// In Layer 5 the relay Hub manages per-client sinks — the session no longer
// tracks individual clients. The Hub calls Suspend/Resume directly instead of
// going through AddSink/RemoveSink, keeping session state accurate.
func (s *Session) Suspend() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state != StateActive {
		return
	}
	s.state = StateSuspended
	s.suspendTimer = time.AfterFunc(s.suspendGrace, func() {
		if s.onExpire != nil {
			s.onExpire(s.ID)
		}
	})
}

// Resume cancels a pending suspension and transitions the session back to
// Active. Called by the transport layer when a client joins a suspended
// session. Returns an error if the session is already Closed.
func (s *Session) Resume() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state == StateClosed {
		return fmt.Errorf("session %d: already closed", s.ID)
	}
	if s.state == StateSuspended {
		s.suspendTimer.Stop()
		s.state = StateActive
	}
	return nil
}

// Close terminates the session: cancels its context (which kills the PTY
// process via exec.CommandContext), stops the suspend timer, and marks the
// session as Closed. Idempotent — safe to call multiple times.
//
// Callers should note: after Close returns, ReadOutput will return an error
// (the PTY slave is gone), so the pumpPTY goroutine will exit cleanly.
func (s *Session) Close() {
	s.mu.Lock()
	if s.state == StateClosed {
		s.mu.Unlock()
		return
	}
	s.state = StateClosed
	if s.suspendTimer != nil {
		s.suspendTimer.Stop()
	}
	s.mu.Unlock()

	// Cancel the context AFTER releasing the lock so pumpPTY's ReadOutput
	// unblocks without holding the lock.
	s.cancel()
	s.proc.Close()
}
