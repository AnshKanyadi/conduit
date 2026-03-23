package protocol

import (
	"errors"
	"fmt"
)

// Sentinel errors for Marshal and Unmarshal.
//
// Why sentinel errors instead of string comparisons?
// errors.Is() lets callers match precisely without coupling to error text.
// The transport layer can switch on these to decide: drop silently, send NACK,
// or close the connection — each is the right response to a different failure.
var (
	// ErrInvalidMagic means the 4-byte prefix wasn't 0xC0DE1337.
	// This usually means we're reading from the wrong offset in a stream,
	// or the remote is speaking a different protocol entirely.
	ErrInvalidMagic = errors.New("protocol: invalid magic number")

	// ErrChecksumMismatch means the CRC32 we computed doesn't match the
	// one stored in the frame. The frame was corrupted in transit.
	ErrChecksumMismatch = errors.New("protocol: checksum mismatch")

	// ErrFrameTooShort means the byte slice can't possibly hold a valid frame.
	// Either the payload length field is lying, or the buffer was truncated.
	ErrFrameTooShort = errors.New("protocol: frame too short")

	// ErrPayloadTooLarge means the caller tried to marshal a payload that
	// won't fit in the 2-byte length field (max 65535 bytes).
	ErrPayloadTooLarge = errors.New("protocol: payload exceeds maximum size")

	// ErrUnknownType means the frame type byte isn't one of our defined
	// constants. We reject rather than forward unknown types to prevent
	// undefined behavior in routing logic upstream.
	ErrUnknownType = errors.New("protocol: unknown frame type")
)

// OutOfOrderError is returned by SequenceChecker when a frame arrives
// with a sequence number that doesn't match what we expected.
// Carrying both values lets the transport layer decide: NACK the gap,
// request a replay, or just log and continue.
type OutOfOrderError struct {
	Expected uint16
	Got      uint16
}

func (e *OutOfOrderError) Error() string {
	return fmt.Sprintf("protocol: out-of-order frame: expected seq %d, got %d",
		e.Expected, e.Got)
}
