package replay_test

import (
	"testing"

	"github.com/anshk/conduit/internal/replay"
)

// payload returns a simple distinguishable byte slice for a given index.
func payload(n int) []byte {
	return []byte{byte(n), byte(n >> 8), byte(n >> 16)}
}

// collect runs Replay and returns every (seq, payload) pair delivered.
func collect(log *replay.Log, fromSeq uint16) [][2]interface{} {
	var out [][2]interface{}
	log.Replay(fromSeq, func(seq uint16, p []byte) {
		out = append(out, [2]interface{}{seq, p})
	})
	return out
}

// --- Append / Len ------------------------------------------------------------

func TestLog_Append_Len(t *testing.T) {
	l := replay.NewLog(16)

	if l.Len() != 0 {
		t.Fatalf("initial Len: got %d, want 0", l.Len())
	}
	for i := 1; i <= 5; i++ {
		if err := l.Append(uint16(i), payload(i)); err != nil {
			t.Fatalf("Append(%d): %v", i, err)
		}
		if l.Len() != i {
			t.Errorf("Len after %d appends: got %d, want %d", i, l.Len(), i)
		}
	}
}

// --- Replay: basic delivery --------------------------------------------------

func TestLog_Replay_AllFramesAfterFromSeq(t *testing.T) {
	l := replay.NewLog(16)
	// Append seqs 1..5.
	for i := 1; i <= 5; i++ {
		l.Append(uint16(i), payload(i))
	}

	// Ask for everything after seq 2 — expect 3, 4, 5.
	got := collect(l, 2)
	if len(got) != 3 {
		t.Fatalf("got %d frames, want 3", len(got))
	}
	for idx, want := uint16(3), 0; want < 3; idx, want = idx+1, want+1 {
		if got[want][0].(uint16) != idx {
			t.Errorf("frame[%d] seq: got %d, want %d", want, got[want][0], idx)
		}
	}
}

func TestLog_Replay_FromSeqIsLastFrame_NothingDelivered(t *testing.T) {
	l := replay.NewLog(16)
	l.Append(1, payload(1))
	l.Append(2, payload(2))

	got := collect(l, 2) // fromSeq == last stored seq
	if len(got) != 0 {
		t.Errorf("expected 0 frames, got %d", len(got))
	}
}

// --- Replay: fromSeq not found (best-effort recovery) -----------------------

func TestLog_Replay_FromSeqNotFound_ReturnsAll(t *testing.T) {
	l := replay.NewLog(16)
	for i := 1; i <= 4; i++ {
		l.Append(uint16(i), payload(i))
	}

	// fromSeq=99 is not in the log — expect all 4 stored frames.
	got := collect(l, 99)
	if len(got) != 4 {
		t.Errorf("fromSeq not found: got %d frames, want 4", len(got))
	}
}

func TestLog_Replay_EmptyLog_NothingDelivered(t *testing.T) {
	l := replay.NewLog(16)
	got := collect(l, 0)
	if len(got) != 0 {
		t.Errorf("empty log: got %d frames, want 0", len(got))
	}
}

// --- Ring buffer wrap-around -------------------------------------------------

func TestLog_RingBuffer_OldestEvictedWhenFull(t *testing.T) {
	const cap = 4
	l := replay.NewLog(cap)

	// Fill the buffer: seqs 1..4.
	for i := 1; i <= cap; i++ {
		l.Append(uint16(i), payload(i))
	}
	if l.Len() != cap {
		t.Fatalf("Len: got %d, want %d", l.Len(), cap)
	}

	// Append one more: seq 5 evicts seq 1.
	l.Append(5, payload(5))
	if l.Len() != cap {
		t.Errorf("Len after eviction: got %d, want %d", l.Len(), cap)
	}

	// fromSeq=1 is no longer in the log → best-effort: return seqs 2,3,4,5.
	got := collect(l, 1)
	if len(got) != cap {
		t.Fatalf("after eviction: got %d frames, want %d", len(got), cap)
	}
	for i, want := range []uint16{2, 3, 4, 5} {
		if got[i][0].(uint16) != want {
			t.Errorf("frame[%d] seq: got %d, want %d", i, got[i][0], want)
		}
	}
}

// --- Sequence number wrap-around (uint16: 65535 → 0) ------------------------

func TestLog_Replay_SequenceWraparound(t *testing.T) {
	l := replay.NewLog(16)

	// Append frames straddling the uint16 wrap.
	seqs := []uint16{65533, 65534, 65535, 0, 1, 2}
	for _, seq := range seqs {
		l.Append(seq, []byte{byte(seq)})
	}

	// Ask for frames after 65535 — expect seqs 0, 1, 2.
	got := collect(l, 65535)
	if len(got) != 3 {
		t.Fatalf("after wraparound: got %d frames, want 3", len(got))
	}
	for i, want := range []uint16{0, 1, 2} {
		if got[i][0].(uint16) != want {
			t.Errorf("frame[%d] seq: got %d, want %d", i, got[i][0], want)
		}
	}
}

// --- Payload round-trip (compression/decompression) -------------------------

func TestLog_Replay_PayloadIntact(t *testing.T) {
	l := replay.NewLog(16)
	orig := []byte("hello from the replay log — ANSI \x1b[1mBOLD\x1b[0m test")
	l.Append(42, orig)

	var got []byte
	l.Replay(41, func(seq uint16, p []byte) {
		if seq == 42 {
			got = p
		}
	})

	if string(got) != string(orig) {
		t.Errorf("payload mismatch: got %q, want %q", got, orig)
	}
}

// --- Concurrent safety -------------------------------------------------------

func TestLog_ConcurrentAppendReplay(t *testing.T) {
	// Smoke test: concurrent Appends and Replays must not race or panic.
	// Run with -race to catch data races.
	l := replay.NewLog(64)
	done := make(chan struct{})

	go func() {
		for i := 0; i < 200; i++ {
			l.Append(uint16(i), payload(i))
		}
		close(done)
	}()

	for i := 0; i < 5; i++ {
		go func() {
			l.Replay(0, func(uint16, []byte) {})
		}()
	}

	<-done
}
