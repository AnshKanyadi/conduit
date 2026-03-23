package transport_test

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/anshk/conduit/internal/protocol"
	"github.com/anshk/conduit/internal/session"
	"github.com/anshk/conduit/internal/transport"
)

// --- Helpers -----------------------------------------------------------------

// newTestServer starts an httptest.Server with a transport.Handler that uses
// a short suspend grace and a shell that cats (echoes) its input.
func newTestServer(t *testing.T, grace time.Duration) (*httptest.Server, *session.Manager) {
	t.Helper()
	mgr := session.NewManager()
	mgr.SuspendGrace = grace

	h := transport.NewHandler(mgr, session.Config{
		// "cat" echoes whatever we write to its stdin back to its stdout.
		// This lets us test the full keystroke → PTY → output round-trip.
		Command: []string{"/bin/sh", "-c", "cat"},
		Rows:    24,
		Cols:    80,
	}, nil) // nil registry: single-node mode, no redirect hints

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, mgr
}

// wsConnect opens a WebSocket connection to srv at path /ws.
func wsConnect(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// sendFrame marshals and sends a frame over a WebSocket connection.
func sendFrame(t *testing.T, conn *websocket.Conn, f *protocol.Frame) {
	t.Helper()
	data, err := protocol.Marshal(f)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

// recvFrame reads and unmarshals a frame with a deadline.
func recvFrame(t *testing.T, conn *websocket.Conn, timeout time.Duration) *protocol.Frame {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(timeout))
	defer conn.SetReadDeadline(time.Time{})

	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	frame, err := protocol.Unmarshal(msg)
	if err != nil {
		t.Fatalf("unmarshal frame: %v", err)
	}
	return frame
}

// doHandshake sends a SYNC frame and expects an ACK back.
// sessionID=0 requests a new session; sessionID=N requests rejoin.
// Returns the confirmed session ID from the ACK.
func doHandshake(t *testing.T, conn *websocket.Conn, sessionID uint32) uint32 {
	t.Helper()
	sendFrame(t, conn, &protocol.Frame{
		Type:      protocol.TypeSync,
		SessionID: sessionID,
	})
	ack := recvFrame(t, conn, 5*time.Second)
	if ack.Type != protocol.TypeACK {
		t.Fatalf("handshake: got %v, want ACK", ack.Type)
	}
	if ack.SessionID == 0 {
		t.Fatal("handshake: ACK carries zero session ID")
	}
	return ack.SessionID
}

// --- Handshake: new session --------------------------------------------------

func TestHandshake_NewSession_ReturnsACK(t *testing.T) {
	srv, mgr := newTestServer(t, 200*time.Millisecond)
	conn := wsConnect(t, srv)

	sessID := doHandshake(t, conn, 0)
	if sessID == 0 {
		t.Error("expected non-zero session ID from new-session handshake")
	}

	// Manager should now know about it.
	if mgr.ActiveCount() != 1 {
		t.Errorf("manager count after create: got %d, want 1", mgr.ActiveCount())
	}
}

func TestHandshake_MalformedFirstFrame_ReturnsNACK(t *testing.T) {
	srv, _ := newTestServer(t, 200*time.Millisecond)
	conn := wsConnect(t, srv)

	// Send raw garbage bytes — not a valid Conduit frame.
	conn.WriteMessage(websocket.BinaryMessage, []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
	resp := recvFrame(t, conn, 3*time.Second)
	if resp.Type != protocol.TypeNACK {
		t.Errorf("malformed frame: got %v, want NACK", resp.Type)
	}
}

func TestHandshake_WrongFirstFrameType_ReturnsNACK(t *testing.T) {
	srv, _ := newTestServer(t, 200*time.Millisecond)
	conn := wsConnect(t, srv)

	// Send a valid KEYSTROKE frame instead of SYNC.
	sendFrame(t, conn, &protocol.Frame{
		Type:      protocol.TypeKeystroke,
		SessionID: 0,
		Payload:   []byte("a"),
	})
	resp := recvFrame(t, conn, 3*time.Second)
	if resp.Type != protocol.TypeNACK {
		t.Errorf("wrong frame type: got %v, want NACK", resp.Type)
	}
}

func TestHandshake_JoinNonexistentSession_ReturnsNACK(t *testing.T) {
	srv, _ := newTestServer(t, 200*time.Millisecond)
	conn := wsConnect(t, srv)

	sendFrame(t, conn, &protocol.Frame{
		Type:      protocol.TypeSync,
		SessionID: 99999, // does not exist
	})
	resp := recvFrame(t, conn, 3*time.Second)
	if resp.Type != protocol.TypeNACK {
		t.Errorf("join nonexistent: got %v, want NACK", resp.Type)
	}
}

// --- Keystroke → PTY → Output round-trip ------------------------------------

func TestKeystroke_RoutedToPTY_OutputReturned(t *testing.T) {
	// "cat" echoes stdin to stdout. We send "hello\n" and expect to receive
	// it back as an OUTPUT frame (possibly with PTY echo mixed in).
	srv, _ := newTestServer(t, 500*time.Millisecond)
	conn := wsConnect(t, srv)

	doHandshake(t, conn, 0)

	// Send "hello\n" as a KEYSTROKE.
	sendFrame(t, conn, &protocol.Frame{
		Type:    protocol.TypeKeystroke,
		Payload: []byte("hello\n"),
	})

	// Collect OUTPUT frames until we see "hello" or timeout.
	deadline := time.Now().Add(4 * time.Second)
	var received strings.Builder

	for time.Now().Before(deadline) {
		conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		conn.SetReadDeadline(time.Time{})

		frame, err := protocol.Unmarshal(msg)
		if err != nil {
			continue
		}
		if frame.Type == protocol.TypeOutput {
			received.Write(frame.Payload)
			if strings.Contains(received.String(), "hello") {
				return // success
			}
		}
	}

	t.Errorf("did not receive 'hello' in OUTPUT frames; got: %q", received.String())
}

// --- Reconnection ------------------------------------------------------------

func TestReconnection_WithinGrace_JoinsExistingSession(t *testing.T) {
	srv, mgr := newTestServer(t, 500*time.Millisecond)

	// First connection: create a new session.
	conn1 := wsConnect(t, srv)
	sessID := doHandshake(t, conn1, 0)

	// Verify session exists.
	if _, ok := mgr.Get(sessID); !ok {
		t.Fatal("session not found in manager after creation")
	}

	// Disconnect client 1.
	conn1.Close()

	// Wait a bit — but still within the grace period.
	time.Sleep(100 * time.Millisecond)

	// Reconnect with the same session ID.
	conn2 := wsConnect(t, srv)
	rejoinedID := doHandshake(t, conn2, sessID)

	if rejoinedID != sessID {
		t.Errorf("rejoin: got session ID %d, want %d", rejoinedID, sessID)
	}
	if mgr.ActiveCount() != 1 {
		t.Errorf("manager count after rejoin: got %d, want 1", mgr.ActiveCount())
	}

	conn2.Close()
}

func TestReconnection_AfterGrace_SessionExpired(t *testing.T) {
	srv, mgr := newTestServer(t, 100*time.Millisecond)

	conn1 := wsConnect(t, srv)
	sessID := doHandshake(t, conn1, 0)
	conn1.Close()

	// Wait for the grace period to expire.
	time.Sleep(300 * time.Millisecond)

	if mgr.ActiveCount() != 0 {
		t.Errorf("manager count after expiry: got %d, want 0", mgr.ActiveCount())
	}

	// Attempt to rejoin the expired session — should get NACK.
	conn2 := wsConnect(t, srv)
	sendFrame(t, conn2, &protocol.Frame{
		Type:      protocol.TypeSync,
		SessionID: sessID,
	})
	resp := recvFrame(t, conn2, 3*time.Second)
	if resp.Type != protocol.TypeNACK {
		t.Errorf("rejoin after expiry: got %v, want NACK", resp.Type)
	}
}

// --- Heartbeat ---------------------------------------------------------------

func TestHeartbeat_ReturnsACKWithPayload(t *testing.T) {
	srv, _ := newTestServer(t, 500*time.Millisecond)
	conn := wsConnect(t, srv)
	doHandshake(t, conn, 0)

	// Send a HEARTBEAT with a fake timestamp payload.
	ts := []byte("1234567890")
	sendFrame(t, conn, &protocol.Frame{
		Type:    protocol.TypeHeartbeat,
		Payload: ts,
	})

	// We should receive an ACK. (OUTPUT frames from cat may arrive first.)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		frame, err := protocol.Unmarshal(msg)
		if err != nil {
			continue
		}
		if frame.Type == protocol.TypeACK {
			// The ACK should echo our timestamp payload.
			if string(frame.Payload) == string(ts) {
				return // success
			}
		}
	}

	t.Error("did not receive HEARTBEAT ACK with echoed payload")
}

// --- Multiple clients on same session ----------------------------------------

func TestMultipleClients_BothReceiveOutput(t *testing.T) {
	srv, _ := newTestServer(t, 500*time.Millisecond)

	// Client A: create session.
	connA := wsConnect(t, srv)
	sessID := doHandshake(t, connA, 0)

	// Client B: join same session.
	connB := wsConnect(t, srv)
	joinedID := doHandshake(t, connB, sessID)
	if joinedID != sessID {
		t.Fatalf("client B joined wrong session: %d vs %d", joinedID, sessID)
	}

	// Client A sends a keystroke.
	sendFrame(t, connA, &protocol.Frame{
		Type:    protocol.TypeKeystroke,
		Payload: []byte("world\n"),
	})

	// Both clients should receive OUTPUT containing "world".
	checkOutput := func(conn *websocket.Conn, name string) {
		deadline := time.Now().Add(4 * time.Second)
		var buf strings.Builder
		for time.Now().Before(deadline) {
			conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			_, msg, err := conn.ReadMessage()
			if err != nil {
				break
			}
			frame, _ := protocol.Unmarshal(msg)
			if frame != nil && frame.Type == protocol.TypeOutput {
				buf.Write(frame.Payload)
				if strings.Contains(buf.String(), "world") {
					return
				}
			}
		}
		t.Errorf("client %s did not receive 'world' in output; got: %q", name, buf.String())
	}

	// Run checks concurrently — both clients receive from the same broadcast.
	done := make(chan struct{}, 2)
	go func() { checkOutput(connA, "A"); done <- struct{}{} }()
	go func() { checkOutput(connB, "B"); done <- struct{}{} }()
	<-done
	<-done
}

// --- Replay ------------------------------------------------------------------

func TestReplay_ClientReceivesReplayFrames(t *testing.T) {
	// Full round-trip:
	//   1. Connect, generate some output (cat echoes "ping\n").
	//   2. Send TypeReplayRequest with Sequence=0 (before the first frame).
	//      Because seq=0 is never stored (first real frame has seq≥1), the
	//      log falls back to "replay everything" — best-effort recovery.
	//   3. Verify that at least one TypeReplayFrame arrives with non-empty
	//      payload.
	srv, _ := newTestServer(t, 500*time.Millisecond)
	conn := wsConnect(t, srv)
	doHandshake(t, conn, 0)

	// Generate output so the replay log has at least one entry.
	sendFrame(t, conn, &protocol.Frame{
		Type:    protocol.TypeKeystroke,
		Payload: []byte("ping\n"),
	})

	// Drain OUTPUT frames until we see "ping", proving the log has been
	// populated (record-before-dispatch in pumpPTY).
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		frame, err := protocol.Unmarshal(msg)
		if err != nil {
			continue
		}
		if frame.Type == protocol.TypeOutput && len(frame.Payload) > 0 {
			break // output recorded; log is populated
		}
	}
	conn.SetReadDeadline(time.Time{})

	// Send a TypeReplayRequest for Sequence=65535 — a sentinel meaning
	// "I have received nothing yet; replay from the very beginning."
	// SequenceGenerator.Next() returns 0 for the first frame, so 65535
	// will not be found in the log, triggering the best-effort fallback
	// that replays all stored entries.
	sendFrame(t, conn, &protocol.Frame{
		Type:     protocol.TypeReplayRequest,
		Sequence: 65535,
	})

	// Expect at least one TypeReplayFrame with non-empty payload.
	deadline = time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		frame, err := protocol.Unmarshal(msg)
		if err != nil {
			continue
		}
		if frame.Type == protocol.TypeReplayFrame && len(frame.Payload) > 0 {
			return // success
		}
	}
	t.Error("did not receive any TypeReplayFrame after TypeReplayRequest")
}
