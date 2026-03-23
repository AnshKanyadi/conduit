// Package relay implements the broadcast hub and adaptive flow control.
//
// When a PTY produces output it must be fanned out to every connected client
// in a session. The relay hub does this fan-out using one buffered channel
// per client, avoiding shared-memory locks on the hot path.
//
// Flow control uses a sliding window model: slow clients are caught up via
// delta compression or dropped entirely if they fall too far behind, keeping
// the fast path fast regardless of stragglers.
package relay
