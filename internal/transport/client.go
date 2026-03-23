// Package transport implements WebSocket adapters for the Conduit protocol.
// It is the boundary between raw network I/O and the session/protocol layers.
package transport

import (
	"sync"
	"sync/atomic"

	"github.com/gorilla/websocket"
)

// clientSendBuf is the number of outbound frames buffered per client before
// we start dropping. 64 frames × ~4KB average payload = ~256KB per client.
// Layer 5 (relay) will raise this to 256 and add flow-control on top.
const clientSendBuf = 64

// clientIDCounter generates unique IDs for Client instances within a process.
// These are not session IDs — they identify a specific WebSocket connection.
var clientIDCounter atomic.Uint32

// Client represents a single WebSocket connection to a session.
// A client is associated with exactly one session for its lifetime.
//
// Why a separate write goroutine (not writing in the read loop)?
// WebSocket connections are not safe for concurrent writes. If we wrote
// directly from multiple goroutines (e.g., the PTY pump and the HEARTBEAT
// handler), we'd need a mutex around every write. The write goroutine pattern
// serialises all writes through a single goroutine — no mutex needed on the
// hot path, and the channel provides natural backpressure.
type Client struct {
	id   uint32
	conn *websocket.Conn

	// sendCh carries outbound frame bytes. Non-blocking sends: if the channel
	// is full, Send returns false and the caller (Broadcast) moves on.
	sendCh chan []byte

	closeOnce sync.Once    // ensures we only close sendCh once
	closeCh   chan struct{} // closed when the write loop should exit
}

// newClient constructs a Client for the given WebSocket connection.
// The client is not yet running — call run() to start the read/write goroutines.
func newClient(conn *websocket.Conn) *Client {
	return &Client{
		id:      clientIDCounter.Add(1),
		conn:    conn,
		sendCh:  make(chan []byte, clientSendBuf),
		closeCh: make(chan struct{}),
	}
}

// ID returns the unique client identifier.
func (c *Client) ID() uint32 {
	return c.id
}

// SinkCh returns the client's outbound channel. The relay Hub writes broadcast
// frames here; the write loop drains them to the WebSocket connection.
// Exposed so Hub.Register can wire up without the hub needing a Client reference.
func (c *Client) SinkCh() chan []byte {
	return c.sendCh
}

// Send queues a frame for delivery to the WebSocket client.
// Implements session.Sink. Returns false if the send buffer is full,
// which signals the caller that this client is falling behind.
// Non-blocking by design — blocking here would stall the PTY pump goroutine
// for all clients, not just the slow one.
func (c *Client) Send(frame []byte) bool {
	// Make a defensive copy: the caller (pumpPTY) reuses its read buffer,
	// so we must not hold a reference to frame past this function.
	cp := make([]byte, len(frame))
	copy(cp, frame)

	select {
	case c.sendCh <- cp:
		return true
	default:
		return false // buffer full — drop frame (Layer 5 adds recovery)
	}
}

// writeLoop drains sendCh and writes frames to the WebSocket connection.
// Runs as a dedicated goroutine for each client.
// Exits when closeCh is closed or a write error occurs.
func (c *Client) writeLoop() {
	defer c.conn.Close()
	for {
		select {
		case frame, ok := <-c.sendCh:
			if !ok {
				return // channel closed
			}
			if err := c.conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
				return
			}
		case <-c.closeCh:
			return
		}
	}
}

// close signals the write loop to exit and drains any pending frames.
// Idempotent — safe to call multiple times.
func (c *Client) close() {
	c.closeOnce.Do(func() {
		close(c.closeCh)
	})
}
