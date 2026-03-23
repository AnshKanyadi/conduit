/**
 * useConduit — WebSocket connection state machine for the Conduit protocol.
 *
 * Phases:
 *   idle         → no connection
 *   connecting   → WebSocket.CONNECTING
 *   handshaking  → WS open; SYNC sent; waiting for ACK
 *   connected    → ACK received; terminal is live
 *   disconnected → WS closed or NACK received
 *
 * The hook is intentionally side-effect-free between renders; all WS state
 * lives in refs so callbacks are always current without stale closures.
 */

import { useReducer, useRef, useCallback, useEffect } from 'react';
import { TYPE, marshal, unmarshal, payloadText } from '../lib/protocol.js';

// ---- State machine ----------------------------------------------------------

const PHASES = {
  IDLE:         'idle',
  CONNECTING:   'connecting',
  HANDSHAKING:  'handshaking',
  CONNECTED:    'connected',
  DISCONNECTED: 'disconnected',
};

const initialState = { phase: PHASES.IDLE, sessionId: null, error: null };

function reducer(state, action) {
  switch (action.type) {
    case 'CONNECT':       return { phase: PHASES.CONNECTING,   sessionId: null, error: null };
    case 'HANDSHAKING':   return { ...state, phase: PHASES.HANDSHAKING };
    case 'CONNECTED':     return { phase: PHASES.CONNECTED,    sessionId: action.sessionId, error: null };
    case 'DISCONNECTED':  return { phase: PHASES.DISCONNECTED, sessionId: state.sessionId, error: action.error ?? null };
    case 'RESET':         return initialState;
    default:              return state;
  }
}

// ---- Hook -------------------------------------------------------------------

const HEARTBEAT_INTERVAL_MS = 30_000;
const WS_URL = `${window.location.protocol === 'https:' ? 'wss:' : 'ws:'}//${window.location.host}/ws`;

/**
 * @param {object} opts
 * @param {() => { rows: number, cols: number }} opts.getTerminalDims
 *   Called when opening a new session to send accurate PTY dimensions.
 * @param {(data: Uint8Array) => void} opts.onOutput
 *   Called with raw PTY bytes to write to the terminal.
 */
export function useConduit({ getTerminalDims, onOutput }) {
  const [state, dispatch] = useReducer(reducer, initialState);

  const wsRef          = useRef(null);
  const seqRef         = useRef(0);
  const lastSeqRef     = useRef(0xFFFF); // sentinel: "replay everything"
  const heartbeatRef   = useRef(null);
  const onOutputRef    = useRef(onOutput);

  // Keep callback refs fresh without triggering effect re-runs.
  useEffect(() => { onOutputRef.current = onOutput; }, [onOutput]);

  const nextSeq = useCallback(() => {
    const s = seqRef.current;
    seqRef.current = (s + 1) & 0xFFFF;
    return s;
  }, []);

  const sendFrame = useCallback((frame) => {
    if (wsRef.current?.readyState === WebSocket.OPEN) {
      wsRef.current.send(marshal(frame));
    }
  }, []);

  const stopHeartbeat = useCallback(() => {
    if (heartbeatRef.current !== null) {
      clearInterval(heartbeatRef.current);
      heartbeatRef.current = null;
    }
  }, []);

  const startHeartbeat = useCallback((sessionId) => {
    stopHeartbeat();
    heartbeatRef.current = setInterval(() => {
      sendFrame({
        type:      TYPE.HEARTBEAT,
        sequence:  nextSeq(),
        sessionID: sessionId,
      });
    }, HEARTBEAT_INTERVAL_MS);
  }, [sendFrame, nextSeq, stopHeartbeat]);

  const closeWS = useCallback(() => {
    stopHeartbeat();
    if (wsRef.current) {
      // Nullify before close to suppress the onclose error handler.
      const ws = wsRef.current;
      wsRef.current = null;
      ws.close(1000, 'client disconnect');
    }
  }, [stopHeartbeat]);

  /**
   * Handle a fully-parsed inbound frame from the server.
   * Captures `dispatch` and `sendFrame` (both stable), plus `startHeartbeat`.
   */
  const handleFrame = useCallback((frame) => {
    switch (frame.type) {

      case TYPE.ACK: {
        // Handshake complete. frame.sessionID is the confirmed session ID.
        const sid = frame.sessionID;
        dispatch({ type: 'CONNECTED', sessionId: sid });
        startHeartbeat(sid);
        // Persist for page-refresh reconnection.
        sessionStorage.setItem('conduit_session_id', String(sid));
        break;
      }

      case TYPE.NACK: {
        const reason = payloadText(frame.payload) || 'connection rejected';
        dispatch({ type: 'DISCONNECTED', error: reason });
        closeWS();
        break;
      }

      case TYPE.OUTPUT:
      case TYPE.REPLAY_FRAME: {
        // Track sequence for future replay requests.
        lastSeqRef.current = frame.sequence;
        onOutputRef.current?.(frame.payload);
        break;
      }

      case TYPE.SYNC: {
        // Flow-control notification from the relay hub.
        try {
          const { mode } = JSON.parse(payloadText(frame.payload));
          if (mode === 'catchup') {
            // We're falling behind — request a replay from our last-known seq.
            sendFrame({
              type:      TYPE.REPLAY_REQUEST,
              sequence:  lastSeqRef.current,
              sessionID: frame.sessionID,
            });
          } else if (mode === 'drop') {
            // Hub forcibly disconnected us — too far behind to recover inline.
            dispatch({ type: 'DISCONNECTED', error: 'Session dropped: connection too slow. Reconnect to replay.' });
            closeWS();
          }
        } catch {
          // Malformed SYNC payload — ignore.
        }
        break;
      }

      case TYPE.HEARTBEAT: {
        // Echo heartbeats back as ACK with the same sequence.
        sendFrame({
          type:      TYPE.ACK,
          sequence:  frame.sequence,
          sessionID: frame.sessionID,
          payload:   frame.payload,
        });
        break;
      }

      default:
        break;
    }
  }, [dispatch, sendFrame, startHeartbeat, closeWS]);

  /**
   * Open a WebSocket connection and perform the SYNC handshake.
   * Pass rejoinId=0 to create a new session.
   */
  const connect = useCallback((rejoinId = 0) => {
    if (wsRef.current) closeWS();

    dispatch({ type: 'CONNECT' });
    seqRef.current = 0;
    lastSeqRef.current = 0xFFFF;

    const ws = new WebSocket(WS_URL);
    ws.binaryType = 'arraybuffer';
    wsRef.current = ws;

    ws.onopen = () => {
      dispatch({ type: 'HANDSHAKING' });

      let payload;
      if (rejoinId === 0) {
        // New session: include terminal dimensions so the PTY is sized correctly.
        const dims = getTerminalDims?.() ?? { rows: 24, cols: 80 };
        payload = JSON.stringify({ rows: dims.rows, cols: dims.cols });
      } else {
        payload = ''; // server ignores payload for rejoin
      }

      ws.send(marshal({
        type:      TYPE.SYNC,
        sequence:  nextSeq(),
        sessionID: rejoinId,
        payload,
      }));
    };

    ws.onmessage = (ev) => {
      try {
        handleFrame(unmarshal(ev.data));
      } catch {
        // Malformed frame — skip silently.
      }
    };

    ws.onerror = () => {
      if (wsRef.current === ws) {
        dispatch({ type: 'DISCONNECTED', error: 'WebSocket error — server unreachable?' });
        wsRef.current = null;
        stopHeartbeat();
      }
    };

    ws.onclose = (ev) => {
      if (wsRef.current === ws) {
        wsRef.current = null;
        stopHeartbeat();
        if (ev.code !== 1000) {
          dispatch({ type: 'DISCONNECTED', error: null });
        }
      }
    };
  }, [closeWS, getTerminalDims, handleFrame, nextSeq, stopHeartbeat]);

  const disconnect = useCallback(() => {
    closeWS();
    dispatch({ type: 'RESET' });
    sessionStorage.removeItem('conduit_session_id');
  }, [closeWS]);

  const sendKeystroke = useCallback((data, sessionId) => {
    sendFrame({
      type:      TYPE.KEYSTROKE,
      sequence:  nextSeq(),
      sessionID: sessionId,
      payload:   typeof data === 'string' ? new TextEncoder().encode(data) : data,
    });
  }, [sendFrame, nextSeq]);

  // Clean up on unmount.
  useEffect(() => () => closeWS(), [closeWS]);

  return {
    phase:        state.phase,
    sessionId:    state.sessionId,
    error:        state.error,
    PHASES,
    connect,
    disconnect,
    sendKeystroke,
  };
}
