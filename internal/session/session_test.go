package session_test

import (
	"context"
	"testing"
	"time"

	"github.com/anshk/conduit/internal/session"
)

// --- Sink mock ---------------------------------------------------------------

// mockSink records all frames delivered to it, used to verify Broadcast.
type mockSink struct {
	id     uint32
	frames [][]byte
}

func (m *mockSink) Send(frame []byte) bool {
	cp := make([]byte, len(frame))
	copy(cp, frame)
	m.frames = append(m.frames, cp)
	return true
}

func (m *mockSink) count() int { return len(m.frames) }

// --- Helpers -----------------------------------------------------------------

// newTestManager returns a Manager with a short suspend grace for fast tests.
func newTestManager(grace time.Duration) *session.Manager {
	mgr := session.NewManager()
	mgr.SuspendGrace = grace
	return mgr
}

// defaultCfg is a shell that stays alive long enough for the test to run.
var defaultCfg = session.Config{
	Command: []string{"/bin/sh", "-c", "sleep 60"},
	Rows:    24,
	Cols:    80,
}

// --- Manager: Create ---------------------------------------------------------

func TestManager_Create_ReturnsSession(t *testing.T) {
	mgr := newTestManager(100 * time.Millisecond)
	ctx := context.Background()

	sess, err := mgr.Create(ctx, defaultCfg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer mgr.Remove(sess.ID)

	if sess.ID == 0 {
		t.Error("session ID must not be 0")
	}
	if sess.State() != session.StateActive {
		t.Errorf("initial state: got %v, want Active", sess.State())
	}
}

func TestManager_Create_AssignsUniqueIDs(t *testing.T) {
	mgr := newTestManager(100 * time.Millisecond)
	ctx := context.Background()

	s1, _ := mgr.Create(ctx, defaultCfg)
	s2, _ := mgr.Create(ctx, defaultCfg)
	defer mgr.Remove(s1.ID)
	defer mgr.Remove(s2.ID)

	if s1.ID == s2.ID {
		t.Errorf("duplicate session ID %d", s1.ID)
	}
}

func TestManager_Create_TracksCount(t *testing.T) {
	mgr := newTestManager(100 * time.Millisecond)
	ctx := context.Background()

	if n := mgr.ActiveCount(); n != 0 {
		t.Errorf("initial count: got %d, want 0", n)
	}

	s1, _ := mgr.Create(ctx, defaultCfg)
	if n := mgr.ActiveCount(); n != 1 {
		t.Errorf("after create: got %d, want 1", n)
	}

	s2, _ := mgr.Create(ctx, defaultCfg)
	if n := mgr.ActiveCount(); n != 2 {
		t.Errorf("after second create: got %d, want 2", n)
	}

	mgr.Remove(s1.ID)
	mgr.Remove(s2.ID)

	if n := mgr.ActiveCount(); n != 0 {
		t.Errorf("after removes: got %d, want 0", n)
	}
}

// --- Manager: Get ------------------------------------------------------------

func TestManager_Get_ExistingSession(t *testing.T) {
	mgr := newTestManager(100 * time.Millisecond)
	ctx := context.Background()

	sess, _ := mgr.Create(ctx, defaultCfg)
	defer mgr.Remove(sess.ID)

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get: session not found")
	}
	if got.ID != sess.ID {
		t.Errorf("Get: got ID %d, want %d", got.ID, sess.ID)
	}
}

func TestManager_Get_NonexistentSession(t *testing.T) {
	mgr := newTestManager(100 * time.Millisecond)

	_, ok := mgr.Get(99999)
	if ok {
		t.Error("Get: expected not found for nonexistent ID")
	}
}

// --- Manager: Remove ---------------------------------------------------------

func TestManager_Remove_Idempotent(t *testing.T) {
	mgr := newTestManager(100 * time.Millisecond)
	ctx := context.Background()

	sess, _ := mgr.Create(ctx, defaultCfg)
	mgr.Remove(sess.ID)
	mgr.Remove(sess.ID) // second call must not panic
}

func TestManager_Remove_DecreasesCount(t *testing.T) {
	mgr := newTestManager(100 * time.Millisecond)
	ctx := context.Background()

	sess, _ := mgr.Create(ctx, defaultCfg)
	mgr.Remove(sess.ID)

	if n := mgr.ActiveCount(); n != 0 {
		t.Errorf("after Remove: got count %d, want 0", n)
	}
}

// --- Session: Sink management ------------------------------------------------

func TestSession_AddSink_IncreasesSinkCount(t *testing.T) {
	mgr := newTestManager(100 * time.Millisecond)
	ctx := context.Background()

	sess, _ := mgr.Create(ctx, defaultCfg)
	defer mgr.Remove(sess.ID)

	sink := &mockSink{id: 1}
	if err := sess.AddSink(1, sink); err != nil {
		t.Fatalf("AddSink: %v", err)
	}
	if n := sess.SinkCount(); n != 1 {
		t.Errorf("SinkCount: got %d, want 1", n)
	}
}

func TestSession_RemoveSink_DecreasesSinkCount(t *testing.T) {
	mgr := newTestManager(100 * time.Millisecond)
	ctx := context.Background()

	sess, _ := mgr.Create(ctx, defaultCfg)
	defer mgr.Remove(sess.ID)

	_ = sess.AddSink(1, &mockSink{id: 1})
	sess.RemoveSink(1)

	if n := sess.SinkCount(); n != 0 {
		t.Errorf("SinkCount after remove: got %d, want 0", n)
	}
}

// --- Session: Broadcast ------------------------------------------------------

func TestSession_Broadcast_DeliveresToAllSinks(t *testing.T) {
	mgr := newTestManager(500 * time.Millisecond)
	ctx := context.Background()

	sess, _ := mgr.Create(ctx, defaultCfg)
	defer mgr.Remove(sess.ID)

	a, b := &mockSink{id: 1}, &mockSink{id: 2}
	_ = sess.AddSink(1, a)
	_ = sess.AddSink(2, b)

	sess.Broadcast([]byte("hello"))

	if a.count() != 1 {
		t.Errorf("sink a: got %d frames, want 1", a.count())
	}
	if b.count() != 1 {
		t.Errorf("sink b: got %d frames, want 1", b.count())
	}
}

// --- Reconnection (suspend/resume) -------------------------------------------

func TestSession_Reconnection_WithinGrace(t *testing.T) {
	// A session with a 200ms grace period should survive a brief disconnect.
	mgr := newTestManager(200 * time.Millisecond)
	ctx := context.Background()

	sess, err := mgr.Create(ctx, defaultCfg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Add a sink, then remove it — session should become Suspended.
	_ = sess.AddSink(1, &mockSink{id: 1})
	sess.RemoveSink(1)

	if sess.State() != session.StateSuspended {
		t.Fatalf("state after disconnect: got %v, want Suspended", sess.State())
	}

	// Reconnect within the grace period.
	time.Sleep(50 * time.Millisecond)
	if err := sess.AddSink(2, &mockSink{id: 2}); err != nil {
		t.Fatalf("AddSink on rejoin: %v", err)
	}

	if sess.State() != session.StateActive {
		t.Errorf("state after rejoin: got %v, want Active", sess.State())
	}

	// Session should still be in the manager.
	_, ok := mgr.Get(sess.ID)
	if !ok {
		t.Error("session should still be in manager after rejoin")
	}

	mgr.Remove(sess.ID)
}

func TestSession_GracePeriodExpiry_RemovesFromManager(t *testing.T) {
	// After the grace period elapses with no clients, the session must be
	// removed from the manager automatically.
	mgr := newTestManager(100 * time.Millisecond)
	ctx := context.Background()

	sess, _ := mgr.Create(ctx, defaultCfg)
	sessID := sess.ID

	// Add then remove the only sink to start the timer.
	_ = sess.AddSink(1, &mockSink{id: 1})
	sess.RemoveSink(1)

	// Wait beyond the grace period.
	time.Sleep(250 * time.Millisecond)

	if n := mgr.ActiveCount(); n != 0 {
		t.Errorf("after expiry: manager has %d sessions, want 0", n)
	}
	if _, ok := mgr.Get(sessID); ok {
		t.Error("session should be removed from manager after grace period")
	}
}

func TestSession_GracePeriod_NewClientPreventsExpiry(t *testing.T) {
	// If a client reconnects before the timer fires, the session must survive.
	mgr := newTestManager(200 * time.Millisecond)
	ctx := context.Background()

	sess, _ := mgr.Create(ctx, defaultCfg)
	defer mgr.Remove(sess.ID)

	_ = sess.AddSink(1, &mockSink{id: 1})
	sess.RemoveSink(1) // start timer

	time.Sleep(50 * time.Millisecond) // half the grace period

	_ = sess.AddSink(2, &mockSink{id: 2}) // rejoin — cancels timer

	time.Sleep(200 * time.Millisecond) // grace period would have elapsed

	// Session must still be in the manager.
	if _, ok := mgr.Get(sess.ID); !ok {
		t.Error("session should still exist after rejoin prevented expiry")
	}
}

// --- Session: Close ----------------------------------------------------------

func TestSession_Close_TransitionsToClosedState(t *testing.T) {
	mgr := newTestManager(100 * time.Millisecond)
	ctx := context.Background()

	sess, _ := mgr.Create(ctx, defaultCfg)
	mgr.Remove(sess.ID) // Remove calls sess.Close()

	if sess.State() != session.StateClosed {
		t.Errorf("state after Close: got %v, want Closed", sess.State())
	}
}

func TestSession_AddSink_AfterClose_ReturnsError(t *testing.T) {
	mgr := newTestManager(100 * time.Millisecond)
	ctx := context.Background()

	sess, _ := mgr.Create(ctx, defaultCfg)
	mgr.Remove(sess.ID) // closes the session

	err := sess.AddSink(99, &mockSink{id: 99})
	if err == nil {
		t.Error("AddSink on closed session: expected error, got nil")
	}
}

func TestSession_Close_Idempotent(t *testing.T) {
	mgr := newTestManager(100 * time.Millisecond)
	ctx := context.Background()

	sess, _ := mgr.Create(ctx, defaultCfg)
	mgr.Remove(sess.ID)
	mgr.Remove(sess.ID) // second Remove must not panic
}
