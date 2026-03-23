package protocol

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

// Magic is the 4-byte sentinel at the start of every Conduit frame.
//
// The spec calls this 0xC0ND1337, but N and D are not valid hex digits —
// this is a mnemonic, not a literal. We use 0xC0DE1337 ("CODE" + leet)
// which preserves the intent. Any deviation from this value during Unmarshal
// is an immediate rejection — no further parsing occurs.
const Magic uint32 = 0xC0DE1337

// Frame size constants — kept as named values so they're self-documenting
// and changing the layout in the future produces compile errors at every
// site that assumed a fixed size.
const (
	// HeaderSize: magic(4) + type(1) + seq(2) + sessionID(4) + payloadLen(2)
	HeaderSize = 13

	// ChecksumSize: CRC32 trailer
	ChecksumSize = 4

	// MinFrameSize is the smallest valid Conduit frame (zero-length payload).
	MinFrameSize = HeaderSize + ChecksumSize // 17 bytes

	// MaxPayloadSize is constrained by the 2-byte length field.
	// 65535 bytes is plenty for any terminal output burst.
	MaxPayloadSize = 0xFFFF
)

// FrameType is the 1-byte discriminant that tells the receiver how to
// interpret the payload. Using a named type (not plain uint8) means the
// compiler catches accidental type mixups at call sites.
type FrameType uint8

const (
	TypeKeystroke     FrameType = 0x01 // client → server: a key was pressed
	TypeOutput        FrameType = 0x02 // server → client: PTY produced output
	TypeSync          FrameType = 0x03 // server → client: catch-up or drop notice
	TypeACK           FrameType = 0x04 // either direction: frame acknowledged
	TypeNACK          FrameType = 0x05 // either direction: frame rejected, please resend
	TypeHeartbeat     FrameType = 0x06 // either direction: keep-alive probe
	TypeReplayRequest FrameType = 0x07 // client → server: request replay from timestamp
	TypeReplayFrame   FrameType = 0x08 // server → client: a single replayed event
)

// String returns a human-readable frame type name for logging.
// The default %v verb on a FrameType will call this automatically.
func (t FrameType) String() string {
	switch t {
	case TypeKeystroke:
		return "KEYSTROKE"
	case TypeOutput:
		return "OUTPUT"
	case TypeSync:
		return "SYNC"
	case TypeACK:
		return "ACK"
	case TypeNACK:
		return "NACK"
	case TypeHeartbeat:
		return "HEARTBEAT"
	case TypeReplayRequest:
		return "REPLAY_REQUEST"
	case TypeReplayFrame:
		return "REPLAY_FRAME"
	default:
		return fmt.Sprintf("UNKNOWN(0x%02x)", uint8(t))
	}
}

// Frame is the fundamental unit of the Conduit wire protocol.
// All fields are exported so higher layers can construct frames directly
// without going through a builder — keeping the API surface small.
type Frame struct {
	Type      FrameType
	Sequence  uint16
	SessionID uint32
	Payload   []byte
}

// crcTable is computed once at startup using the IEEE polynomial.
//
// Why CRC32 instead of MD5, SHA-1, or xxHash?
//   - CRC32 is designed specifically for burst-error detection in data streams,
//     not for cryptographic security. That's exactly our use case.
//   - It fits in 4 bytes. MD5 is 16 bytes — that's 12 bytes of overhead per
//     frame on the hot path, which matters at 100k frames/sec.
//   - The IEEE polynomial is hardware-accelerated on x86 (CLMUL instruction)
//     and ARM (CRC32 extension), so it's effectively free at our throughput.
//   - We don't need tamper-resistance — TLS at the transport layer handles that.
//     We need fast corruption detection for noisy network conditions.
var crcTable = crc32.MakeTable(crc32.IEEE)

// Marshal serializes a Frame into its wire representation.
//
// Why big-endian byte order?
// Big-endian is the network byte order standard (RFC 1700). It's what TCP/IP
// headers, DNS, and most binary protocols use. Wireshark and hex editors show
// multi-byte fields in the same left-to-right order humans read them, making
// debugging significantly easier than little-endian.
//
// Wire layout:
//
//	[ magic     : 4 bytes, big-endian uint32 ]
//	[ type      : 1 byte                     ]
//	[ sequence  : 2 bytes, big-endian uint16 ]
//	[ sessionID : 4 bytes, big-endian uint32 ]
//	[ payloadLen: 2 bytes, big-endian uint16 ]
//	[ payload   : payloadLen bytes           ]
//	[ crc32     : 4 bytes, big-endian uint32 ] ← over all preceding bytes
func Marshal(f *Frame) ([]byte, error) {
	if len(f.Payload) > MaxPayloadSize {
		return nil, ErrPayloadTooLarge
	}
	if !isKnownType(f.Type) {
		return nil, ErrUnknownType
	}

	totalSize := HeaderSize + len(f.Payload) + ChecksumSize
	buf := make([]byte, totalSize)
	off := 0

	binary.BigEndian.PutUint32(buf[off:], Magic)
	off += 4

	buf[off] = uint8(f.Type)
	off++

	binary.BigEndian.PutUint16(buf[off:], f.Sequence)
	off += 2

	binary.BigEndian.PutUint32(buf[off:], f.SessionID)
	off += 4

	binary.BigEndian.PutUint16(buf[off:], uint16(len(f.Payload)))
	off += 2

	copy(buf[off:], f.Payload)
	off += len(f.Payload)

	// Checksum covers every byte written so far — header corruption (e.g. a
	// flipped bit in SessionID or Sequence) is caught the same as payload
	// corruption.
	checksum := crc32.Checksum(buf[:off], crcTable)
	binary.BigEndian.PutUint32(buf[off:], checksum)

	return buf, nil
}

// Unmarshal deserializes a wire-format byte slice into a Frame.
//
// Validation is done in cheapest-first order so we fail fast:
//  1. Length check  (O(1), no memory alloc)
//  2. Magic check   (O(1), single uint32 compare)
//  3. Type check    (O(1), switch)
//  4. Payload bounds (O(1), arithmetic)
//  5. CRC32         (O(n), must read the whole frame — done last)
//
// Each error is a distinct sentinel so the transport layer can route
// failures to the right handler (log-and-continue vs. send NACK vs. close).
func Unmarshal(data []byte) (*Frame, error) {
	if len(data) < MinFrameSize {
		return nil, ErrFrameTooShort
	}

	off := 0

	// Step 1: Magic — cheapest rejection. Garbage bytes, misaligned reads,
	// and wrong-protocol connections all fail here before we parse anything.
	magic := binary.BigEndian.Uint32(data[off:])
	if magic != Magic {
		return nil, ErrInvalidMagic
	}
	off += 4

	// Step 2: Frame type — reject before allocating payload buffer.
	ft := FrameType(data[off])
	if !isKnownType(ft) {
		return nil, ErrUnknownType
	}
	off++

	seq := binary.BigEndian.Uint16(data[off:])
	off += 2

	sessionID := binary.BigEndian.Uint32(data[off:])
	off += 4

	payloadLen := int(binary.BigEndian.Uint16(data[off:]))
	off += 2

	// Step 3: Bounds check before we trust payloadLen.
	// A malicious or corrupted payloadLen could cause us to read beyond the
	// buffer end (classic buffer over-read). We validate before any allocation.
	needed := HeaderSize + payloadLen + ChecksumSize
	if len(data) < needed {
		return nil, ErrFrameTooShort
	}

	payload := make([]byte, payloadLen)
	copy(payload, data[off:off+payloadLen])
	off += payloadLen

	// Step 4: CRC32 — most expensive check, done after all cheap validations.
	// We compute over data[0:off] (header + payload) and compare to the stored
	// checksum in the final 4 bytes.
	storedCRC := binary.BigEndian.Uint32(data[off:])
	computedCRC := crc32.Checksum(data[:off], crcTable)
	if storedCRC != computedCRC {
		return nil, ErrChecksumMismatch
	}

	return &Frame{
		Type:      ft,
		Sequence:  seq,
		SessionID: sessionID,
		Payload:   payload,
	}, nil
}

// isKnownType returns true if ft matches one of our defined FrameType constants.
// Rejecting unknown types at the deserialization boundary prevents undefined
// behavior when routers and handlers upstream switch on FrameType exhaustively.
func isKnownType(ft FrameType) bool {
	switch ft {
	case TypeKeystroke, TypeOutput, TypeSync, TypeACK, TypeNACK,
		TypeHeartbeat, TypeReplayRequest, TypeReplayFrame:
		return true
	}
	return false
}
