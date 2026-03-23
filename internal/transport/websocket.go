package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.uber.org/zap"

	"github.com/anshk/conduit/internal/consensus"
	"github.com/anshk/conduit/internal/observability"
	"github.com/anshk/conduit/internal/protocol"
	"github.com/anshk/conduit/internal/relay"
	"github.com/anshk/conduit/internal/replay"
	"github.com/anshk/conduit/internal/session"
)

// handshakeTimeout is how long we wait for the initial SYNC frame after a
// WebSocket connection is established. Clients that never send a SYNC are
// silently dropped after this window to prevent connection exhaustion.
const handshakeTimeout = 10 * time.Second

// syncPayload is the JSON body of a SYNC frame when creating a NEW session.
// For session rejoin (SessionID != 0), the payload is empty.
type syncPayload struct {
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
}

// Handler is an http.Handler that upgrades HTTP connections to WebSocket,
// negotiates a session, and drives the read/write loops for each client.
type Handler struct {
	upgrader   websocket.Upgrader
	manager    *session.Manager
	defaultCfg session.Config

	// registry is the Layer-8 consensus registry. nil in single-node mode.
	// Used to provide redirect hints when a session lives on another node.
	registry *consensus.Registry

	// hubs stores one *relay.Hub per session, keyed by session ID (uint32).
	hubs sync.Map

	// logs stores one *replay.Log per session, keyed by session ID (uint32).
	logs sync.Map
}

// NewHandler creates a Handler. defaultCfg provides the PTY command and
// fallback terminal dimensions for new sessions. registry may be nil for
// single-node deployments; when set, it enables redirect hints on handshake
// misses (the NACK payload includes the owning node's address).
func NewHandler(manager *session.Manager, defaultCfg session.Config, registry *consensus.Registry) *Handler {
	return &Handler{
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		manager:    manager,
		defaultCfg: defaultCfg,
		registry:   registry,
	}
}

// ServeHTTP handles an incoming WebSocket connection end-to-end.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		observability.L.Warn("ws upgrade failed", zap.Error(err))
		return
	}

	start := time.Now()
	sess, isNew, err := h.handshake(conn)
	if err != nil {
		// handshake already sent NACK; log at debug (not error) — rejections
		// from malformed frames are noisy in tests and expected in production.
		observability.L.Debug("handshake rejected", zap.Error(err))
		conn.Close()
		return
	}
	observability.HandshakeDuration.Observe(time.Since(start).Seconds())
	observability.ClientsConnected.Inc()
	defer observability.ClientsConnected.Dec()

	observability.L.Info("client connected",
		zap.Uint32("session_id", sess.ID),
		zap.Bool("new_session", isNew),
	)

	hub, err := h.getOrCreateHub(sess, isNew)
	if err != nil {
		h.sendNACK(conn, sess.ID, "session ended")
		conn.Close()
		return
	}

	if !isNew {
		if err := sess.Resume(); err != nil {
			h.sendNACK(conn, sess.ID, "session closed")
			conn.Close()
			return
		}
	}

	client := newClient(conn)
	hub.Register(client.ID(), client.SinkCh(), client.close)
	defer func() {
		hub.Unregister(client.ID())
		client.close()
		observability.L.Debug("client disconnected",
			zap.Uint32("session_id", sess.ID),
			zap.Uint32("client_id", client.ID()),
		)
	}()

	if isNew {
		rlog := replay.NewLog(0)
		h.logs.Store(sess.ID, rlog)
		go h.pumpPTY(sess, hub, rlog)
	}

	go client.writeLoop()
	h.readLoop(conn, client, sess)
}

// getOrCreateHub returns the relay Hub for the session, creating one if new.
func (h *Handler) getOrCreateHub(sess *session.Session, isNew bool) (*relay.Hub, error) {
	if isNew {
		hub := relay.NewHub(relay.HubConfig{
			OnEmpty: sess.Suspend,
		})
		go hub.Run()
		h.hubs.Store(sess.ID, hub)
		return hub, nil
	}

	v, ok := h.hubs.Load(sess.ID)
	if !ok {
		return nil, errors.New("hub not found")
	}
	return v.(*relay.Hub), nil
}

// handshake reads the first WebSocket frame (must be SYNC) and either
// creates a new session or rejoins an existing one. Emits an OTel span.
//
// Wire protocol:
//
//	Client → Server: SYNC frame
//	  SessionID=0:  create new session. Payload = JSON syncPayload (optional).
//	  SessionID=N:  rejoin session N. Payload empty.
//
//	Server → Client: ACK frame (success) — SessionID = confirmed session ID.
//	               or NACK frame (failure) — Payload = human-readable reason.
func (h *Handler) handshake(conn *websocket.Conn) (*session.Session, bool, error) {
	ctx, span := observability.Tracer().Start(context.Background(), "transport.handshake")
	defer span.End()

	conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	defer conn.SetReadDeadline(time.Time{})

	_, msg, err := conn.ReadMessage()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "read SYNC failed")
		return nil, false, fmt.Errorf("read SYNC: %w", err)
	}

	frame, err := protocol.Unmarshal(msg)
	if err != nil {
		h.sendNACK(conn, 0, "malformed frame: "+err.Error())
		span.RecordError(err)
		span.SetStatus(codes.Error, "malformed frame")
		return nil, false, fmt.Errorf("unmarshal SYNC: %w", err)
	}
	if frame.Type != protocol.TypeSync {
		h.sendNACK(conn, 0, "first frame must be SYNC")
		span.SetStatus(codes.Error, "wrong frame type")
		return nil, false, fmt.Errorf("expected SYNC, got %v", frame.Type)
	}

	var (
		sess  *session.Session
		isNew bool
	)

	if frame.SessionID == 0 {
		sess, err = h.createSession(ctx, frame.Payload)
		if err != nil {
			h.sendNACK(conn, 0, "cannot create session: "+err.Error())
			span.RecordError(err)
			span.SetStatus(codes.Error, "create session failed")
			return nil, false, fmt.Errorf("create session: %w", err)
		}
		isNew = true
	} else {
		var ok bool
		sess, ok = h.manager.Get(frame.SessionID)
		if !ok {
			reason := "session not found or expired"
			// If a registry is configured, check whether another node owns
			// this session and embed the redirect address in the NACK so
			// clients can reconnect to the correct node.
			if h.registry != nil {
				if node, found, _ := h.registry.LookupSession(ctx, frame.SessionID); found {
					reason = "session on " + node.Addr
				}
			}
			h.sendNACK(conn, frame.SessionID, reason)
			span.SetStatus(codes.Error, "session not found")
			return nil, false, fmt.Errorf("session %d not found", frame.SessionID)
		}
		if sess.State() == session.StateClosed {
			h.sendNACK(conn, frame.SessionID, "session is closed")
			span.SetStatus(codes.Error, "session closed")
			return nil, false, errors.New("session already closed")
		}
	}

	span.SetAttributes(
		observability.AttrSessionID(sess.ID),
		attribute.Bool("conduit.new_session", isNew),
	)

	ack, err := protocol.Marshal(&protocol.Frame{
		Type:      protocol.TypeACK,
		SessionID: sess.ID,
	})
	if err != nil {
		h.manager.Remove(sess.ID)
		span.RecordError(err)
		return nil, false, fmt.Errorf("marshal ACK: %w", err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, ack); err != nil {
		h.manager.Remove(sess.ID)
		span.RecordError(err)
		return nil, false, fmt.Errorf("send ACK: %w", err)
	}

	span.SetStatus(codes.Ok, "")
	return sess, isNew, nil
}

// createSession parses the optional syncPayload and spawns a new session.
func (h *Handler) createSession(ctx context.Context, payload []byte) (*session.Session, error) {
	cfg := h.defaultCfg

	if len(payload) > 0 {
		var sp syncPayload
		if err := json.Unmarshal(payload, &sp); err == nil {
			if sp.Rows > 0 {
				cfg.Rows = sp.Rows
			}
			if sp.Cols > 0 {
				cfg.Cols = sp.Cols
			}
		}
	}

	if cfg.Rows == 0 {
		cfg.Rows = 24
	}
	if cfg.Cols == 0 {
		cfg.Cols = 80
	}

	return h.manager.Create(ctx, cfg)
}

// readLoop reads frames from the WebSocket and dispatches them.
// Blocks until the connection closes or an unrecoverable read error occurs.
func (h *Handler) readLoop(conn *websocket.Conn, client *Client, sess *session.Session) {
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}

		frame, err := protocol.Unmarshal(msg)
		if err != nil {
			continue
		}

		h.dispatch(frame, client, sess)
	}
}

// dispatch routes a validated inbound frame to the appropriate handler.
func (h *Handler) dispatch(frame *protocol.Frame, client *Client, sess *session.Session) {
	switch frame.Type {
	case protocol.TypeKeystroke:
		if _, err := sess.WriteKeystroke(frame.Payload); err != nil {
			return
		}

	case protocol.TypeHeartbeat:
		ack, err := protocol.Marshal(&protocol.Frame{
			Type:      protocol.TypeACK,
			SessionID: sess.ID,
			Sequence:  frame.Sequence,
			Payload:   frame.Payload,
		})
		if err != nil {
			return
		}
		client.Send(ack)

	case protocol.TypeReplayRequest:
		observability.ReplayRequestsTotal.Inc()
		h.serveReplay(frame.Sequence, client, sess)

	default:
	}
}

// serveReplay delivers missed frames to a single client as TypeReplayFrame
// messages. Emits an OTel span recording how many frames were replayed.
func (h *Handler) serveReplay(fromSeq uint16, client *Client, sess *session.Session) {
	_, span := observability.Tracer().Start(context.Background(), "transport.replay_request")
	defer span.End()
	span.SetAttributes(
		observability.AttrSessionID(sess.ID),
		observability.AttrClientID(client.ID()),
		attribute.Int("conduit.from_seq", int(fromSeq)),
	)

	v, ok := h.logs.Load(sess.ID)
	if !ok {
		span.SetStatus(codes.Error, "log not found")
		return
	}
	rlog := v.(*replay.Log)

	var delivered int
	rlog.Replay(fromSeq, func(seq uint16, payload []byte) {
		frame, err := protocol.Marshal(&protocol.Frame{
			Type:      protocol.TypeReplayFrame,
			SessionID: sess.ID,
			Sequence:  seq,
			Payload:   payload,
		})
		if err != nil {
			return
		}
		if client.Send(frame) {
			delivered++
		}
	})

	observability.ReplayFramesServed.Add(float64(delivered))
	span.SetAttributes(observability.AttrReplayFrames(delivered))
	span.SetStatus(codes.Ok, "")

	observability.L.Debug("replay served",
		zap.Uint32("session_id", sess.ID),
		zap.Uint32("client_id", client.ID()),
		zap.Uint16("from_seq", fromSeq),
		zap.Int("frames_delivered", delivered),
	)
}

// pumpPTY reads raw bytes from the PTY master, records them in the replay
// log, and dispatches them as OUTPUT frames via the relay Hub.
// One goroutine per session; exits when the PTY closes.
func (h *Handler) pumpPTY(sess *session.Session, hub *relay.Hub, rlog *replay.Log) {
	defer func() {
		h.manager.Remove(sess.ID)
		h.logs.Delete(sess.ID)
		h.hubs.Delete(sess.ID)
		hub.Stop()
		observability.L.Info("session ended", zap.Uint32("session_id", sess.ID))
	}()

	observability.L.Info("session started", zap.Uint32("session_id", sess.ID))

	buf := make([]byte, 32*1024)
	var seqGen protocol.SequenceGenerator

	for {
		n, err := sess.ReadOutput(buf)
		if err != nil {
			return
		}

		seq := seqGen.Next()
		payload := make([]byte, n)
		copy(payload, buf[:n])

		_ = rlog.Append(seq, payload)

		frame, err := protocol.Marshal(&protocol.Frame{
			Type:      protocol.TypeOutput,
			Sequence:  seq,
			SessionID: sess.ID,
			Payload:   payload,
		})
		if err != nil {
			continue
		}

		observability.PTYFramesTotal.Inc()
		hub.Dispatch(frame)
	}
}

// sendNACK sends a NACK frame during handshake to explain why the connection
// was rejected.
func (h *Handler) sendNACK(conn *websocket.Conn, sessionID uint32, reason string) {
	nack, err := protocol.Marshal(&protocol.Frame{
		Type:      protocol.TypeNACK,
		SessionID: sessionID,
		Payload:   []byte(reason),
	})
	if err != nil {
		return
	}
	_ = conn.WriteMessage(websocket.BinaryMessage, nack)
}

// r_ctx returns the context for session creation.
// In Layer 8 this will be the server's root context tied to the node lifecycle.
func r_ctx() context.Context {
	return context.Background()
}
