package replay

// Layer 6: in-memory append-only replay log.
//
// Every OUTPUT frame emitted by pumpPTY is recorded here before being
// dispatched to the relay Hub. When a client falls behind and requests
// a replay (TypeReplayRequest), this log delivers the missed frames as
// TypeReplayFrame messages directly to that client.
//
// Storage model: circular (ring) buffer of LZ4-compressed payload entries.
// "Append-only" means we never modify an existing entry; we only advance
// the write cursor, evicting the oldest entry when the buffer is full.
// This matches the doc.go intent — the log is logically append-only even
// though the physical storage is bounded and reuses slots.
//
// What's NOT here yet:
//   - Segment files / index files (Layer 8/10 — requires durable storage)
//   - Delta encoding (future optimisation — needs a VT100 parser)
//   - Cross-node replication (Layer 8 — etcd-backed)
//
// Why in-memory for Layer 6?
// An in-memory log already solves the primary use-case: a client with a
// flaky connection rejoins within seconds and recovers missed frames.
// Durable storage adds I/O latency and complexity that are out of scope
// until we have a storage layer (Layer 8).

import (
	"sync"

	"github.com/anshk/conduit/internal/relay"
)

// DefaultMaxEntries is the ring buffer capacity used when NewLog is called
// with maxEntries ≤ 0. At ~4 KB per compressed entry this is ~16 MB max.
const DefaultMaxEntries = 4096

// entry is one slot in the ring buffer.
type entry struct {
	seq     uint16 // protocol sequence number of this OUTPUT frame
	data    []byte // LZ4-compressed PTY payload (not the full wire frame)
	origLen int    // uncompressed length, for metrics / debugging
}

// Log is a bounded, append-only in-memory log of PTY output frames.
// All exported methods are safe for concurrent use.
type Log struct {
	mu    sync.RWMutex
	buf   []entry
	cap   int
	wpos  int // index of the next slot to write (advances mod cap)
	count int // number of valid entries currently in buf (≤ cap)
}

// NewLog creates a Log with the given ring-buffer capacity.
// maxEntries ≤ 0 selects DefaultMaxEntries.
func NewLog(maxEntries int) *Log {
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	return &Log{
		buf: make([]entry, maxEntries),
		cap: maxEntries,
	}
}

// Append records one OUTPUT frame. The payload (raw PTY bytes, not the full
// wire frame) is LZ4-compressed before storage to reduce peak memory.
//
// If the ring buffer is full, the oldest entry is silently overwritten.
// This is correct — a client that is further behind than MaxEntries frames
// must request a full replay from persistent storage (Layer 8), not from
// this in-memory log.
func (l *Log) Append(seq uint16, payload []byte) error {
	compressed, err := relay.CompressPayload(payload)
	if err != nil {
		return err
	}

	l.mu.Lock()
	l.buf[l.wpos] = entry{
		seq:     seq,
		data:    compressed,
		origLen: len(payload),
	}
	l.wpos = (l.wpos + 1) % l.cap
	if l.count < l.cap {
		l.count++
	}
	l.mu.Unlock()
	return nil
}

// Replay calls fn for every stored frame whose sequence number comes AFTER
// fromSeq, in order from oldest to newest.
//
// If fromSeq is not present in the log (evicted or never received), fn is
// called for ALL stored frames — best-effort recovery is better than nothing.
//
// Payloads are decompressed before delivery. fn is called without the lock
// held so callers may perform I/O (e.g. client.Send) inside fn safely.
//
// Why synchronous and not a channel?
// The caller (dispatch goroutine) is already a dedicated per-client goroutine.
// Synchronous delivery keeps the implementation simple and avoids an extra
// goroutine and channel allocation per replay request. If replay becomes
// latency-sensitive (large logs), we can make it async in Layer 8.
func (l *Log) Replay(fromSeq uint16, fn func(seq uint16, payload []byte)) {
	// Snapshot entries under the read lock so fn can run without holding it.
	l.mu.RLock()
	snap := l.snapshot()
	l.mu.RUnlock()

	// Find the position of fromSeq. We scan oldest→newest; because the ring
	// holds at most cap entries (≤ 4096 << uint16 max 65536), a given sequence
	// number appears at most once — no ambiguity around the wraparound point.
	startIdx := -1
	for i, e := range snap {
		if e.seq == fromSeq {
			startIdx = i
			break
		}
	}
	// startIdx == -1 means fromSeq wasn't found → replay from the beginning.

	for i, e := range snap {
		if i <= startIdx {
			continue // skip up to and including the client's last-seen frame
		}
		payload, err := relay.DecompressPayload(e.data)
		if err != nil {
			continue // corrupted slot — skip rather than crash
		}
		fn(e.seq, payload)
	}
}

// Len returns the number of entries currently stored in the ring buffer.
func (l *Log) Len() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.count
}

// snapshot returns all current entries in insertion order (oldest → newest).
// Must be called with l.mu held (at least RLock).
func (l *Log) snapshot() []entry {
	if l.count == 0 {
		return nil
	}
	out := make([]entry, l.count)
	oldest := (l.wpos - l.count + l.cap) % l.cap
	for i := 0; i < l.count; i++ {
		out[i] = l.buf[(oldest+i)%l.cap]
	}
	return out
}
