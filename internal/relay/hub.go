// Package relay implements the Layer-5 broadcast hub for Conduit.
//
// The hub sits between the PTY pump goroutine and every connected client's
// WebSocket write loop. Its job: fan out PTY output frames to all clients
// while ensuring a slow client never stalls fast ones.
//
// Design principles:
//
//  1. Single-goroutine ownership — one goroutine (Run) owns the client map
//     and all send operations. No mutexes on the hot path; channels provide
//     the synchronisation and act as the linearisation point.
//
//  2. Non-blocking sends — every send to a client channel uses a select with
//     a default branch. If the channel is full, we record the miss and move on.
//     pumpPTY never blocks regardless of client behaviour.
//
//  3. Flow-control state machine — each client moves through:
//     normal → catchup (lag ≥ LagCatchup) → drop (lag ≥ LagDrop)
//     with hysteresis on the recovery path to prevent rapid oscillation.
package relay

import (
	"encoding/json"
	"sync"

	"github.com/anshk/conduit/internal/observability"
	"github.com/anshk/conduit/internal/protocol"
)

// Flow-control thresholds. Exported so tests can drive clients into each
// mode without hard-coding magic numbers.
const (
	// LagCatchup is the number of consecutively-missed sends at which the hub
	// notifies the client to request a replay from Layer 6's append-only log.
	// The client is still forwarded new frames — it may catch up on its own.
	LagCatchup = 500

	// LagDrop is the number of consecutively-missed sends at which the hub
	// forcibly disconnects the client. At this depth, a full replay is the
	// only recovery path; continuing to forward frames wastes hub capacity.
	LagDrop = 2000

	// ClientChannelBuf is the per-client outbound channel buffer depth.
	// Raised from Layer 4's 64 to 256 because the hub's flow-control catches
	// runaway clients well before they fill this buffer in normal operation.
	ClientChannelBuf = 256

	// dispatchBuf is the hub's inbound channel depth. Sized to absorb PTY
	// burst flushes (kernel PTY buffer ~ 4 KB reads at up to 100k/s) without
	// stalling pumpPTY.
	dispatchBuf = 512
)

// clientMode is the flow-control state of a single client.
type clientMode uint8

const (
	modeNormal  clientMode = iota // sending normally; lag within bounds
	modeCatchup                   // lag ≥ LagCatchup; client notified to request replay
	modeDrop                      // lag ≥ LagDrop; client disconnected; stop sending
)

// registration is sent over Hub.regCh to add a client.
type registration struct {
	id     uint32
	sendCh chan []byte // outbound channel owned by the client's write loop
	dropFn func()     // called by hub to initiate client disconnect
}

// hubClient is the hub goroutine's per-client state.
// Only the hub goroutine (Run) reads or writes these fields — no locking needed.
type hubClient struct {
	id     uint32
	sendCh chan []byte
	dropFn func()
	lag    int64      // net missed sends: increments on drop, decrements on success
	ema    EMA        // smoothed delivery rate: 1.0 = healthy, 0.0 = fully stalled
	mode   clientMode
}

// HubConfig parameterises a Hub at construction time.
type HubConfig struct {
	// OnEmpty is called from the hub goroutine when the last client unregisters.
	// Typically wired to session.Suspend() so the PTY keeps running but the
	// session transitions to Suspended state, arming the grace-period timer.
	// Must not block — it runs on the hub goroutine's stack.
	OnEmpty func()
}

// Hub fans out PTY output frames to connected clients with adaptive flow control.
//
// Lifecycle:
//
//	hub := relay.NewHub(cfg)
//	go hub.Run()             // start the hub goroutine
//	hub.Register(id, ch, fn) // add a client
//	hub.Dispatch(frame)      // called by pumpPTY for each output frame
//	hub.Unregister(id)       // remove a client (called on disconnect)
//	hub.Stop()               // signal hub to exit; blocks until it does
type Hub struct {
	cfg        HubConfig
	dispatchCh chan []byte    // inbound PTY frames from pumpPTY
	regCh      chan registration
	unregCh    chan uint32
	stopCh     chan struct{}
	doneCh     chan struct{} // closed by Run() on exit; guards post-stop calls

	stopOnce sync.Once
}

// NewHub creates a Hub but does not start it. Call go hub.Run() to start.
func NewHub(cfg HubConfig) *Hub {
	return &Hub{
		cfg:        cfg,
		dispatchCh: make(chan []byte, dispatchBuf),
		regCh:      make(chan registration, 16),
		unregCh:    make(chan uint32, 16),
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
	}
}

// Register adds a client to the hub.
// sendCh is the client's outbound channel; the hub is the sole broadcast
// writer. dropFn is called when the hub decides to forcibly disconnect.
// Non-blocking: the request is enqueued and processed by the hub goroutine.
func (h *Hub) Register(id uint32, sendCh chan []byte, dropFn func()) {
	select {
	case h.regCh <- registration{id: id, sendCh: sendCh, dropFn: dropFn}:
	case <-h.doneCh: // hub already stopped — registration silently ignored
	}
}

// Unregister removes a client from the hub. Non-blocking.
// Typically called from the transport layer's deferred cleanup on disconnect.
func (h *Hub) Unregister(id uint32) {
	select {
	case h.unregCh <- id:
	case <-h.doneCh: // hub already stopped — no-op
	}
}

// Dispatch enqueues a PTY output frame for fan-out. Called by pumpPTY.
// Non-blocking: if the dispatch buffer is full, the frame is dropped rather
// than stalling pumpPTY (which would block all clients, not just slow ones).
func (h *Hub) Dispatch(frame []byte) {
	select {
	case h.dispatchCh <- frame:
	case <-h.doneCh:
	default:
		// dispatch buffer full — pumpPTY is producing faster than the hub
		// goroutine can consume. Extremely unlikely under normal load.
	}
}

// Run is the hub goroutine. Start it with go hub.Run() exactly once.
// It exits when Stop() is called, closing doneCh.
//
// Why a single goroutine and not a mutex?
// With a mutex, every Dispatch call would contend with every other writer
// (heartbeat ACKs, registrations). With a single goroutine and channels,
// the client map is owned by exactly one goroutine — zero contention on the
// hot path, and the select statement provides fair scheduling across all
// inbound event types.
func (h *Hub) Run() {
	defer close(h.doneCh)
	clients := make(map[uint32]*hubClient)

	for {
		select {
		case reg := <-h.regCh:
			clients[reg.id] = &hubClient{
				id:     reg.id,
				sendCh: reg.sendCh,
				dropFn: reg.dropFn,
				ema:    newEMA(0.1),
				mode:   modeNormal,
			}

		case id := <-h.unregCh:
			delete(clients, id)
			if len(clients) == 0 && h.cfg.OnEmpty != nil {
				h.cfg.OnEmpty()
			}

		case frame := <-h.dispatchCh:
			h.fanOut(clients, frame)

		case <-h.stopCh:
			return
		}
	}
}

// Stop signals the hub goroutine to exit and blocks until it has.
// Idempotent — safe to call multiple times.
func (h *Hub) Stop() {
	h.stopOnce.Do(func() { close(h.stopCh) })
	<-h.doneCh
}

// fanOut delivers frame to every registered client, applying flow control.
// Must only be called from the hub goroutine (Run).
func (h *Hub) fanOut(clients map[uint32]*hubClient, frame []byte) {
	for _, c := range clients {
		if c.mode == modeDrop {
			// Client has been disconnected; skip until Unregister arrives.
			continue
		}

		select {
		case c.sendCh <- frame:
			// Successful delivery: lag recovers by 1.
			if c.lag > 0 {
				c.lag--
			}
			c.ema.Update(1.0)

			// Hysteresis: only recover from catchup when lag drops to half the
			// threshold, to prevent rapid normal ↔ catchup oscillation.
			if c.mode == modeCatchup && c.lag < LagCatchup/2 {
				c.mode = modeNormal
			}

		default:
			// Client channel full — client is falling behind.
			c.lag++
			c.ema.Update(0.0)

			switch {
			case c.lag >= LagDrop:
				// Hopelessly behind. Notify, then call dropFn to close the
				// client's closeCh, which makes the write loop exit cleanly.
				// We do NOT close sendCh ourselves — that would race with the
				// transport's direct HEARTBEAT ACK writes to the same channel.
				c.mode = modeDrop
				h.sendModeFrame(c, "drop")
				observability.HubDropsTotal.Inc()
				c.dropFn()

			case c.lag >= LagCatchup && c.mode == modeNormal:
				// Struggling but recoverable. Notify the client to issue a
				// TypeReplayRequest to Layer 6 while we keep forwarding new frames.
				c.mode = modeCatchup
				h.sendModeFrame(c, "catchup")
				observability.HubCatchupsTotal.Inc()
			}
		}
	}
}

// modePayload is the JSON body of the SYNC frame sent on flow-control transitions.
type modePayload struct {
	Mode string `json:"mode"`
	Lag  int64  `json:"lag,omitempty"`
}

// sendModeFrame sends a TypeSync frame to notify a client of a flow-control
// mode change. Non-blocking: if the channel is full, the notification is
// dropped (the downstream effect — dropFn or continued skipping — still runs).
//
// Why TypeSync for control messages?
// TypeOutput carries raw PTY bytes; TypeSync signals session/relay state.
// Both the initial handshake and these flow-control transitions represent
// "something fundamental changed about this session" — the frame type is apt.
func (h *Hub) sendModeFrame(c *hubClient, mode string) {
	payload, err := json.Marshal(modePayload{Mode: mode, Lag: c.lag})
	if err != nil {
		return
	}
	frame, err := protocol.Marshal(&protocol.Frame{
		Type:    protocol.TypeSync,
		Payload: payload,
	})
	if err != nil {
		return
	}
	select {
	case c.sendCh <- frame:
	default:
	}
}
