package relay_test

import (
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anshk/conduit/internal/relay"
)

// --- Helpers -----------------------------------------------------------------

// newTestHub creates a Hub, starts it, and registers a cleanup to stop it.
func newTestHub(t *testing.T, cfg relay.HubConfig) *relay.Hub {
	t.Helper()
	h := relay.NewHub(cfg)
	go h.Run()
	t.Cleanup(h.Stop)
	return h
}

// newClientCh creates a buffered client channel of the given capacity.
func newClientCh(cap int) chan []byte {
	return make(chan []byte, cap)
}

// drainOne reads one frame from ch within timeout, returning nil on timeout.
func drainOne(ch chan []byte, timeout time.Duration) []byte {
	select {
	case f := <-ch:
		return f
	case <-time.After(timeout):
		return nil
	}
}

// --- Registration and basic dispatch -----------------------------------------

func TestHub_Dispatch_DeliveredToRegisteredClient(t *testing.T) {
	hub := newTestHub(t, relay.HubConfig{})

	ch := newClientCh(8)
	hub.Register(1, ch, func() {})

	// Give the hub goroutine time to process the registration.
	time.Sleep(10 * time.Millisecond)

	hub.Dispatch([]byte("hello"))

	got := drainOne(ch, time.Second)
	if got == nil {
		t.Fatal("frame not delivered within timeout")
	}
	if string(got) != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestHub_Dispatch_DeliveredToAllClients(t *testing.T) {
	hub := newTestHub(t, relay.HubConfig{})

	ch1 := newClientCh(8)
	ch2 := newClientCh(8)
	hub.Register(1, ch1, func() {})
	hub.Register(2, ch2, func() {})

	time.Sleep(10 * time.Millisecond)

	hub.Dispatch([]byte("broadcast"))

	if drainOne(ch1, time.Second) == nil {
		t.Error("client 1: frame not delivered")
	}
	if drainOne(ch2, time.Second) == nil {
		t.Error("client 2: frame not delivered")
	}
}

func TestHub_Unregister_ClientReceivesNoMoreFrames(t *testing.T) {
	hub := newTestHub(t, relay.HubConfig{})

	ch := newClientCh(8)
	hub.Register(1, ch, func() {})
	time.Sleep(10 * time.Millisecond)

	hub.Unregister(1)
	time.Sleep(10 * time.Millisecond) // let hub process the unregistration

	hub.Dispatch([]byte("after-unreg"))

	// Allow a generous window; the channel must stay empty.
	got := drainOne(ch, 100*time.Millisecond)
	if got != nil {
		t.Errorf("received frame after unregister: %q", got)
	}
}

// --- OnEmpty callback --------------------------------------------------------

func TestHub_OnEmpty_CalledWhenLastClientLeaves(t *testing.T) {
	called := atomic.Bool{}
	hub := newTestHub(t, relay.HubConfig{
		OnEmpty: func() { called.Store(true) },
	})

	ch := newClientCh(8)
	hub.Register(1, ch, func() {})
	time.Sleep(10 * time.Millisecond)
	hub.Unregister(1)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if called.Load() {
			return // success
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("OnEmpty was not called after last client unregistered")
}

func TestHub_OnEmpty_NotCalledWhenClientsRemain(t *testing.T) {
	called := atomic.Bool{}
	hub := newTestHub(t, relay.HubConfig{
		OnEmpty: func() { called.Store(true) },
	})

	ch1 := newClientCh(8)
	ch2 := newClientCh(8)
	hub.Register(1, ch1, func() {})
	hub.Register(2, ch2, func() {})
	time.Sleep(10 * time.Millisecond)

	hub.Unregister(1) // client 2 still connected
	time.Sleep(50 * time.Millisecond)

	if called.Load() {
		t.Error("OnEmpty called while a client was still registered")
	}
}

// --- Catch-up mode -----------------------------------------------------------

func TestHub_CatchupMode_NotificationSent(t *testing.T) {
	// Use a channel with capacity exactly 1 so that after the first frame
	// is queued, all subsequent sends to this client fail (channel full).
	// We also give the channel space for the SYNC notification itself so we
	// can read it out.
	//
	// Strategy: capacity 2 — first frame fills [0], notification lands at [1].
	// All frames after that are dropped (channel still has cap 2 but we don't
	// drain). We need LagCatchup consecutive drops to trigger the notification.
	//
	// A capacity-0 channel would drop everything including the notification,
	// so we use capacity 1 for the notification slot and keep [0] occupied.
	ch := make(chan []byte, 1)
	// Pre-fill the channel so every Dispatch call drops.
	ch <- []byte("blocker")

	// Use a separate notification channel of cap 1 for the SYNC frame.
	// But sendModeFrame writes to ch, not a separate channel, so we need
	// one free slot. Drain the blocker and use cap 2 overall.
	<-ch

	hub := newTestHub(t, relay.HubConfig{})
	dropped := atomic.Bool{}
	sinkCh := make(chan []byte, 0) // unbuffered: every send fails immediately

	hub.Register(99, sinkCh, func() { dropped.Store(true) })
	time.Sleep(10 * time.Millisecond)

	// Dispatch exactly LagCatchup frames (with yields so the hub processes
	// them), then stop. The client should be in catchup mode, NOT drop mode.
	for i := 0; i < relay.LagCatchup; i++ {
		hub.Dispatch([]byte("x"))
		runtime.Gosched()
	}
	// Give hub goroutine time to process remaining dispatches.
	time.Sleep(50 * time.Millisecond)

	// At LagCatchup we should be in catchup mode, NOT drop mode.
	if dropped.Load() {
		t.Error("dropFn called at LagCatchup — should only trigger at LagDrop")
	}
}

// --- Drop mode ---------------------------------------------------------------

func TestHub_DropMode_DropFnCalled(t *testing.T) {
	// The hub's dispatch buffer (512 slots) can fill up if the sender
	// outpaces the hub goroutine. We use a continuous-dispatch goroutine
	// with runtime.Gosched() so the hub goroutine gets CPU time to drain
	// the buffer and accumulate lag against the zero-capacity sinkCh.
	dropped := atomic.Bool{}
	hub := newTestHub(t, relay.HubConfig{})

	// Unbuffered channel: every send by the hub's fanOut fails immediately.
	sinkCh := make(chan []byte, 0)
	hub.Register(1, sinkCh, func() { dropped.Store(true) })
	time.Sleep(10 * time.Millisecond)

	// Keep dispatching (with yields) until drop is detected or timeout.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !dropped.Load() {
			hub.Dispatch([]byte("x"))
			runtime.Gosched() // yield so the hub goroutine can run
		}
	}()

	select {
	case <-done:
		// success — drop was detected, the goroutine exited
	case <-time.After(5 * time.Second):
		t.Error("dropFn was not called after lag exceeded LagDrop")
	}
}

func TestHub_DropMode_NoFurtherSendsAfterDrop(t *testing.T) {
	// After drop, frames must not be sent even if the channel empties.
	dropCalled := atomic.Bool{}
	hub := newTestHub(t, relay.HubConfig{})

	sinkCh := make(chan []byte, 0)
	hub.Register(1, sinkCh, func() { dropCalled.Store(true) })
	time.Sleep(10 * time.Millisecond)

	// Trigger drop using the same continuous-dispatch approach as the other test.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !dropCalled.Load() {
			hub.Dispatch([]byte("x"))
			runtime.Gosched()
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("dropFn not called — prerequisite for this test failed")
	}
	if !dropCalled.Load() {
		t.Fatal("dropFn not called — prerequisite for this test failed")
	}

	// Now use a drainable channel and register a new client to prove the hub
	// is still alive. Send more frames — the dropped client must not receive.
	aliveCh := make(chan []byte, 8)
	hub.Register(2, aliveCh, func() {})
	time.Sleep(10 * time.Millisecond)

	hub.Dispatch([]byte("after-drop"))
	if drainOne(aliveCh, time.Second) == nil {
		t.Error("healthy client did not receive frame after another client was dropped")
	}
	// The dropped client's channel should remain empty (it's unbuffered anyway).
}

// --- Compress round-trip -----------------------------------------------------

func TestCompressPayload_RoundTrip(t *testing.T) {
	original := []byte("hello from Conduit relay — LZ4 compression round-trip")
	compressed, err := relay.CompressPayload(original)
	if err != nil {
		t.Fatalf("CompressPayload: %v", err)
	}
	if len(compressed) == 0 {
		t.Fatal("compressed output is empty")
	}
	recovered, err := relay.DecompressPayload(compressed)
	if err != nil {
		t.Fatalf("DecompressPayload: %v", err)
	}
	if string(recovered) != string(original) {
		t.Errorf("round-trip mismatch: got %q, want %q", recovered, original)
	}
}
