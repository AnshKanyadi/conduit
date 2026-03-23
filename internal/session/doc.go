// Package session manages the lifecycle of a Conduit terminal session.
//
// A session is the authoritative unit of state in Conduit — it outlives
// any individual TCP/WebSocket connection. When a client drops and
// reconnects within the grace window, the session is restored rather
// than torn down. This package owns session creation, state tracking,
// reconnection logic, and clean teardown via context cancellation.
package session
