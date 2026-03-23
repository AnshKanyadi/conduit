// Package transport implements network adapters for the Conduit protocol.
//
// Transport is the boundary between raw network bytes and typed protocol
// frames. It accepts WebSocket (and optionally raw TCP) connections, reads
// binary frames off the wire, validates them, and hands them to the session
// layer. It also writes outbound frames back to connected clients.
//
// Transport does NOT own session state — it is a pure I/O adapter.
package transport
