package protocol

import "sync"

// SequenceGenerator assigns monotonically increasing sequence numbers to
// outgoing frames. It wraps at the uint16 boundary (65535 → 0) automatically.
//
// Why uint16 (max 65535) instead of uint32 or uint64?
// The sequence number lives in 2 bytes on the wire. Two bytes is enough for
// in-flight reordering detection — we don't need globally unique IDs here.
// For absolute ordering across a session lifetime, the replay log uses a
// uint64 SequenceID with nanosecond timestamps. The wire sequence number is
// strictly a per-connection, short-window reordering detector.
//
// Wraparound is safe because the checker (below) uses modular uint16 arithmetic
// and we only need to distinguish "one frame out of order" from "fine", not
// track 70k frames simultaneously.
type SequenceGenerator struct {
	mu      sync.Mutex
	current uint16
}

// Next returns the next sequence number and advances the counter.
// The increment wraps at 65535 → 0 automatically via uint16 overflow.
// Safe for concurrent use from multiple goroutines.
func (g *SequenceGenerator) Next() uint16 {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := g.current
	g.current++ // uint16 overflow wraps to 0, no conditional needed
	return n
}

// SequenceChecker detects out-of-order and dropped frames on the receive path.
//
// Design decision — we advance on error, not just on success:
// If frame 42 arrives out of order, we set next=43 rather than keeping
// next=42. This prevents one gap from cascading: if we kept expecting 42
// forever, every subsequent correctly-sequenced frame would also look wrong.
// The transport layer decides policy (NACK the gap, request replay, log it);
// the checker's job is only to detect and report the anomaly.
//
// Design decision — the first frame always succeeds:
// When a client joins mid-session (e.g. reconnect, new viewer) we have no
// baseline sequence. The first Observe call establishes the baseline.
// This avoids a chicken-and-egg problem where we'd need out-of-band
// negotiation to agree on a starting sequence before accepting any frames.
type SequenceChecker struct {
	mu      sync.Mutex
	next    uint16
	started bool
}

// Observe records a received sequence number and returns an OutOfOrderError
// if it does not match the expected next value. On error it still advances
// the baseline to seq+1 to stay in sync with the sender.
func (c *SequenceChecker) Observe(seq uint16) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.started {
		// First frame seen — establish baseline regardless of value.
		c.started = true
		c.next = seq + 1 // uint16 wraparound is intentional
		return nil
	}

	if seq != c.next {
		expected := c.next
		c.next = seq + 1 // advance past the gap so we don't cascade errors
		return &OutOfOrderError{Expected: expected, Got: seq}
	}

	c.next = seq + 1
	return nil
}

// Reset clears the checker back to an unstarted state.
// Called when a session reconnects and starts a fresh sequence.
func (c *SequenceChecker) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.started = false
	c.next = 0
}

// ExpectNext returns the sequence number the checker currently expects.
// Intended for testing and diagnostic logging only — not for hot-path use.
func (c *SequenceChecker) ExpectNext() uint16 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.next
}
