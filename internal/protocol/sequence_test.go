package protocol_test

import (
	"errors"
	"testing"

	"github.com/anshk/conduit/internal/protocol"
)

// --- SequenceGenerator -------------------------------------------------------

func TestSequenceGenerator_StartsAtZero(t *testing.T) {
	var g protocol.SequenceGenerator
	if got := g.Next(); got != 0 {
		t.Errorf("first Next() = %d, want 0", got)
	}
}

func TestSequenceGenerator_Monotonic(t *testing.T) {
	var g protocol.SequenceGenerator
	for i := uint16(0); i < 1000; i++ {
		got := g.Next()
		if got != i {
			t.Fatalf("Next() = %d, want %d", got, i)
		}
	}
}

func TestSequenceGenerator_WrapsAt65535(t *testing.T) {
	// Advance the generator to 65535, then confirm it wraps to 0.
	// We do this by calling Next() 65535 times to reach 65535, then once more.
	var g protocol.SequenceGenerator
	var last uint16
	for i := 0; i <= 65535; i++ {
		last = g.Next()
	}
	// last should be 65535
	if last != 0xFFFF {
		t.Errorf("at step 65535: got %d, want 65535", last)
	}
	// next call should wrap to 0
	if wrapped := g.Next(); wrapped != 0 {
		t.Errorf("wrapped value: got %d, want 0", wrapped)
	}
}

// --- SequenceChecker ---------------------------------------------------------

func TestSequenceChecker_FirstFrameAlwaysPasses(t *testing.T) {
	// No matter what sequence number arrives first, it establishes the baseline.
	for _, startSeq := range []uint16{0, 1, 100, 0xFFFF} {
		var c protocol.SequenceChecker
		if err := c.Observe(startSeq); err != nil {
			t.Errorf("Observe(%d) as first frame: got %v, want nil", startSeq, err)
		}
	}
}

func TestSequenceChecker_SequentialFrames(t *testing.T) {
	var c protocol.SequenceChecker
	for seq := uint16(0); seq < 100; seq++ {
		if err := c.Observe(seq); err != nil {
			t.Errorf("Observe(%d): unexpected error %v", seq, err)
		}
	}
}

func TestSequenceChecker_DetectsGap(t *testing.T) {
	var c protocol.SequenceChecker
	_ = c.Observe(10) // baseline: next expected = 11

	err := c.Observe(13) // skip 11 and 12
	if err == nil {
		t.Fatal("expected OutOfOrderError, got nil")
	}

	var ooe *protocol.OutOfOrderError
	if !errors.As(err, &ooe) {
		t.Fatalf("expected *OutOfOrderError, got %T: %v", err, err)
	}
	if ooe.Expected != 11 {
		t.Errorf("Expected field: got %d, want 11", ooe.Expected)
	}
	if ooe.Got != 13 {
		t.Errorf("Got field: got %d, want 13", ooe.Got)
	}
}

func TestSequenceChecker_AdvancesAfterGap(t *testing.T) {
	// After detecting a gap, the checker must advance so subsequent
	// correctly-sequenced frames don't also appear out of order.
	var c protocol.SequenceChecker
	_ = c.Observe(5)  // baseline: next = 6
	_ = c.Observe(10) // gap reported; next should now be 11

	// Frames 11, 12, 13 should be accepted cleanly.
	for _, seq := range []uint16{11, 12, 13} {
		if err := c.Observe(seq); err != nil {
			t.Errorf("Observe(%d) after gap: unexpected error %v", seq, err)
		}
	}
}

func TestSequenceChecker_DetectsDuplicate(t *testing.T) {
	// Receiving the same sequence number twice is also out-of-order.
	var c protocol.SequenceChecker
	_ = c.Observe(7) // baseline: next = 8
	_ = c.Observe(8) // ok, next = 9

	// Receive 8 again.
	err := c.Observe(8)
	if err == nil {
		t.Fatal("expected OutOfOrderError for duplicate, got nil")
	}
	var ooe *protocol.OutOfOrderError
	if !errors.As(err, &ooe) {
		t.Fatalf("expected *OutOfOrderError, got %T", err)
	}
	if ooe.Expected != 9 || ooe.Got != 8 {
		t.Errorf("Expected=9 Got=8, actual Expected=%d Got=%d", ooe.Expected, ooe.Got)
	}
}

func TestSequenceChecker_WraparoundDetected(t *testing.T) {
	// When sequence wraps from 65535 → 0, the checker should accept it
	// as the natural next value, not flag it as out-of-order.
	var c protocol.SequenceChecker
	_ = c.Observe(0xFFFE) // baseline: next = 0xFFFF
	_ = c.Observe(0xFFFF) // ok: next = 0x0000 (wraps)

	if err := c.Observe(0x0000); err != nil {
		t.Errorf("wraparound from 0xFFFF to 0x0000: unexpected error %v", err)
	}
}

func TestSequenceChecker_Reset(t *testing.T) {
	var c protocol.SequenceChecker
	_ = c.Observe(50) // baseline: next = 51
	_ = c.Observe(51)
	_ = c.Observe(52)

	c.Reset()

	// After Reset, the next Observe should behave as if it's the first frame.
	if err := c.Observe(999); err != nil {
		t.Errorf("first Observe after Reset: got %v, want nil", err)
	}
	// And the one after that should expect 1000.
	if err := c.Observe(1000); err != nil {
		t.Errorf("sequential Observe after Reset: got %v, want nil", err)
	}
}

func TestSequenceChecker_ExpectNext(t *testing.T) {
	var c protocol.SequenceChecker
	_ = c.Observe(100) // next = 101

	if got := c.ExpectNext(); got != 101 {
		t.Errorf("ExpectNext() = %d, want 101", got)
	}
}
