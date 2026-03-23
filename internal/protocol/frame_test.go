package protocol_test

// Tests are in the _test package (external test package) — they can only
// access exported symbols. This enforces that we test the public API, not
// implementation details. If a test needs to reach inside, that's a signal
// the API needs redesigning.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/anshk/conduit/internal/protocol"
)

// --- Helpers -----------------------------------------------------------------

// roundtrip marshals f and immediately unmarshals it, asserting the result
// is field-for-field identical to the input. Used as the base case for all
// frame type tests.
func roundtrip(t *testing.T, f *protocol.Frame) []byte {
	t.Helper()
	data, err := protocol.Marshal(f)
	if err != nil {
		t.Fatalf("Marshal() unexpected error: %v", err)
	}

	got, err := protocol.Unmarshal(data)
	if err != nil {
		t.Fatalf("Unmarshal() unexpected error: %v", err)
	}

	if got.Type != f.Type {
		t.Errorf("Type: got %v, want %v", got.Type, f.Type)
	}
	if got.Sequence != f.Sequence {
		t.Errorf("Sequence: got %d, want %d", got.Sequence, f.Sequence)
	}
	if got.SessionID != f.SessionID {
		t.Errorf("SessionID: got %d, want %d", got.SessionID, f.SessionID)
	}
	if !bytes.Equal(got.Payload, f.Payload) {
		t.Errorf("Payload: got %v, want %v", got.Payload, f.Payload)
	}

	return data // return serialized form so callers can mutate it for corruption tests
}

// --- Round-trip: every frame type -------------------------------------------

func TestMarshalUnmarshal_AllTypes(t *testing.T) {
	types := []struct {
		name string
		ft   protocol.FrameType
	}{
		{"KEYSTROKE", protocol.TypeKeystroke},
		{"OUTPUT", protocol.TypeOutput},
		{"SYNC", protocol.TypeSync},
		{"ACK", protocol.TypeACK},
		{"NACK", protocol.TypeNACK},
		{"HEARTBEAT", protocol.TypeHeartbeat},
		{"REPLAY_REQUEST", protocol.TypeReplayRequest},
		{"REPLAY_FRAME", protocol.TypeReplayFrame},
	}

	for _, tc := range types {
		t.Run(tc.name, func(t *testing.T) {
			f := &protocol.Frame{
				Type:      tc.ft,
				Sequence:  42,
				SessionID: 0xDEADBEEF,
				Payload:   []byte("hello conduit"),
			}
			roundtrip(t, f)
		})
	}
}

// --- Round-trip: edge-case payloads -----------------------------------------

func TestMarshalUnmarshal_EmptyPayload(t *testing.T) {
	// Heartbeats and ACKs typically carry no payload — must serialize cleanly.
	f := &protocol.Frame{
		Type:      protocol.TypeHeartbeat,
		Sequence:  0,
		SessionID: 1,
		Payload:   []byte{},
	}
	roundtrip(t, f)
}

func TestMarshalUnmarshal_NilPayload(t *testing.T) {
	// nil and []byte{} must produce the same wire format.
	fNil := &protocol.Frame{Type: protocol.TypeACK, Sequence: 1, SessionID: 1, Payload: nil}
	fEmpty := &protocol.Frame{Type: protocol.TypeACK, Sequence: 1, SessionID: 1, Payload: []byte{}}

	dataNil, err := protocol.Marshal(fNil)
	if err != nil {
		t.Fatalf("Marshal(nil payload): %v", err)
	}
	dataEmpty, err := protocol.Marshal(fEmpty)
	if err != nil {
		t.Fatalf("Marshal(empty payload): %v", err)
	}
	if !bytes.Equal(dataNil, dataEmpty) {
		t.Error("nil and empty payload should produce identical wire bytes")
	}
}

func TestMarshalUnmarshal_MaxPayload(t *testing.T) {
	// The 2-byte payload length field caps us at 65535 bytes.
	// Marshal and Unmarshal must handle the full 65535-byte payload without error.
	big := make([]byte, protocol.MaxPayloadSize)
	for i := range big {
		big[i] = byte(i % 256)
	}
	f := &protocol.Frame{
		Type:      protocol.TypeOutput,
		Sequence:  1000,
		SessionID: 99,
		Payload:   big,
	}
	roundtrip(t, f)
}

func TestMarshalUnmarshal_BinaryPayload(t *testing.T) {
	// Terminal output can contain arbitrary bytes including 0x00 and 0xFF.
	// Ensure we're not treating payload as a string anywhere.
	payload := []byte{0x00, 0x01, 0xFE, 0xFF, 0x1B, 0x5B, 0x41} // includes ESC [ A (ANSI up-arrow)
	f := &protocol.Frame{
		Type:      protocol.TypeOutput,
		Sequence:  7,
		SessionID: 12345,
		Payload:   payload,
	}
	roundtrip(t, f)
}

func TestMarshalUnmarshal_SequenceWrapAround(t *testing.T) {
	// Sequence number wraps from 65535 back to 0. The wire value must survive
	// round-trip at the boundary.
	f := &protocol.Frame{
		Type:      protocol.TypeKeystroke,
		Sequence:  0xFFFF,
		SessionID: 1,
		Payload:   []byte("a"),
	}
	roundtrip(t, f)
}

// --- Marshal error cases -----------------------------------------------------

func TestMarshal_PayloadTooLarge(t *testing.T) {
	tooBig := make([]byte, protocol.MaxPayloadSize+1)
	f := &protocol.Frame{Type: protocol.TypeOutput, Payload: tooBig}
	_, err := protocol.Marshal(f)
	if !errors.Is(err, protocol.ErrPayloadTooLarge) {
		t.Errorf("got %v, want ErrPayloadTooLarge", err)
	}
}

func TestMarshal_UnknownType(t *testing.T) {
	f := &protocol.Frame{Type: protocol.FrameType(0xFF), Payload: []byte("x")}
	_, err := protocol.Marshal(f)
	if !errors.Is(err, protocol.ErrUnknownType) {
		t.Errorf("got %v, want ErrUnknownType", err)
	}
}

// --- Unmarshal error cases (malformed inputs) --------------------------------

func TestUnmarshal_TooShort_BelowMinimum(t *testing.T) {
	// Anything under 17 bytes can't be a valid frame.
	for size := 0; size < protocol.MinFrameSize; size++ {
		data := make([]byte, size)
		_, err := protocol.Unmarshal(data)
		if !errors.Is(err, protocol.ErrFrameTooShort) {
			t.Errorf("size=%d: got %v, want ErrFrameTooShort", size, err)
		}
	}
}

func TestUnmarshal_InvalidMagic(t *testing.T) {
	f := &protocol.Frame{Type: protocol.TypeHeartbeat, Sequence: 1, SessionID: 1}
	data, _ := protocol.Marshal(f)

	// Corrupt the first 4 bytes (magic).
	data[0] = 0xDE
	data[1] = 0xAD
	data[2] = 0xBE
	data[3] = 0xEF

	_, err := protocol.Unmarshal(data)
	if !errors.Is(err, protocol.ErrInvalidMagic) {
		t.Errorf("got %v, want ErrInvalidMagic", err)
	}
}

func TestUnmarshal_UnknownFrameType(t *testing.T) {
	// Build a syntactically valid frame with a good magic and good CRC,
	// but an undefined frame type byte.
	//
	// We can't use Marshal (it rejects unknown types), so we construct
	// the bytes manually to simulate a malicious or buggy sender.
	f := &protocol.Frame{Type: protocol.TypeACK, Sequence: 0, SessionID: 0, Payload: nil}
	data, _ := protocol.Marshal(f)

	// Overwrite the type byte (index 4) with an undefined value.
	// We must also recompute the CRC so the frame passes the checksum check —
	// otherwise we'd be testing the wrong error path.
	data[4] = 0xAA // undefined type
	recomputeCRC(data)

	_, err := protocol.Unmarshal(data)
	if !errors.Is(err, protocol.ErrUnknownType) {
		t.Errorf("got %v, want ErrUnknownType", err)
	}
}

func TestUnmarshal_PayloadLengthExceedsBuffer(t *testing.T) {
	// A lying payloadLen field that claims more data than the buffer holds.
	// This is the classic buffer over-read attack vector — we must detect it
	// before allocating or copying.
	f := &protocol.Frame{Type: protocol.TypeOutput, Sequence: 0, SessionID: 0, Payload: []byte("hi")}
	data, _ := protocol.Marshal(f)

	// payloadLen is at bytes 11–12 (after magic(4)+type(1)+seq(2)+sessionID(4)).
	// Set it to 9000, which is far larger than the actual buffer.
	binary.BigEndian.PutUint16(data[11:], 9000)
	// Don't bother recomputing CRC — the length check must fire first.

	_, err := protocol.Unmarshal(data)
	if !errors.Is(err, protocol.ErrFrameTooShort) {
		t.Errorf("got %v, want ErrFrameTooShort", err)
	}
}

// --- Checksum tests ----------------------------------------------------------

func TestUnmarshal_ChecksumMismatch_SingleBitFlip(t *testing.T) {
	// CRC32 must catch a single bit flip anywhere in the frame.
	// We test flips across every byte of the header and a payload byte.
	f := &protocol.Frame{
		Type:      protocol.TypeOutput,
		Sequence:  10,
		SessionID: 555,
		Payload:   []byte("important data"),
	}
	original, _ := protocol.Marshal(f)

	// Flip one bit in each byte of the header+payload region (exclude checksum).
	headerAndPayload := len(original) - protocol.ChecksumSize
	for i := 0; i < headerAndPayload; i++ {
		corrupted := make([]byte, len(original))
		copy(corrupted, original)
		corrupted[i] ^= 0x01 // flip the lowest bit

		_, err := protocol.Unmarshal(corrupted)
		// Any of these errors are acceptable: flipping a bit in the magic bytes
		// triggers ErrInvalidMagic; in the type byte triggers ErrUnknownType;
		// in the payloadLen bytes may make the frame appear truncated
		// (ErrFrameTooShort) before we even reach the CRC check — that is
		// intentional and correct behavior, not a test gap.
		isAcceptable := errors.Is(err, protocol.ErrChecksumMismatch) ||
			errors.Is(err, protocol.ErrInvalidMagic) ||
			errors.Is(err, protocol.ErrUnknownType) ||
			errors.Is(err, protocol.ErrFrameTooShort)
		if !isAcceptable {
			t.Errorf("byte %d flipped: got %v, want a protocol rejection error", i, err)
		}
	}
}

func TestUnmarshal_ChecksumMismatch_CorruptedChecksum(t *testing.T) {
	// Flip a bit in the checksum field itself — the stored value won't match
	// what we compute, so we get ErrChecksumMismatch.
	f := &protocol.Frame{
		Type:      protocol.TypeKeystroke,
		Sequence:  1,
		SessionID: 2,
		Payload:   []byte("x"),
	}
	data, _ := protocol.Marshal(f)

	// Checksum is the last 4 bytes.
	data[len(data)-1] ^= 0xFF

	_, err := protocol.Unmarshal(data)
	if !errors.Is(err, protocol.ErrChecksumMismatch) {
		t.Errorf("got %v, want ErrChecksumMismatch", err)
	}
}

func TestUnmarshal_ValidChecksumAfterCorruption_Impossible(t *testing.T) {
	// Sanity check: a cleanly marshaled frame must unmarshal without error.
	// (This is the baseline that makes the corruption tests meaningful.)
	f := &protocol.Frame{
		Type:      protocol.TypeSync,
		Sequence:  999,
		SessionID: 0xCAFEBABE,
		Payload:   []byte("sync payload"),
	}
	data, _ := protocol.Marshal(f)
	_, err := protocol.Unmarshal(data)
	if err != nil {
		t.Errorf("clean frame failed to unmarshal: %v", err)
	}
}

// --- Wire format structural tests -------------------------------------------

func TestWireFormat_CorrectOffsets(t *testing.T) {
	// Verify that each field lands at the exact byte offset we document.
	// If we ever change the layout, this test will catch it immediately.
	f := &protocol.Frame{
		Type:      protocol.TypeOutput,
		Sequence:  0x0102,
		SessionID: 0x03040506,
		Payload:   []byte{0xAA, 0xBB},
	}
	data, err := protocol.Marshal(f)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// magic at [0:4]
	if got := binary.BigEndian.Uint32(data[0:]); got != protocol.Magic {
		t.Errorf("magic at [0:4]: got 0x%08X, want 0x%08X", got, protocol.Magic)
	}
	// type at [4]
	if data[4] != uint8(protocol.TypeOutput) {
		t.Errorf("type at [4]: got 0x%02X, want 0x%02X", data[4], uint8(protocol.TypeOutput))
	}
	// sequence at [5:7]
	if got := binary.BigEndian.Uint16(data[5:]); got != 0x0102 {
		t.Errorf("sequence at [5:7]: got %d, want %d", got, 0x0102)
	}
	// sessionID at [7:11]
	if got := binary.BigEndian.Uint32(data[7:]); got != 0x03040506 {
		t.Errorf("sessionID at [7:11]: got 0x%08X, want 0x%08X", got, 0x03040506)
	}
	// payloadLen at [11:13]
	if got := binary.BigEndian.Uint16(data[11:]); got != 2 {
		t.Errorf("payloadLen at [11:13]: got %d, want %d", got, 2)
	}
	// payload at [13:15]
	if !bytes.Equal(data[13:15], []byte{0xAA, 0xBB}) {
		t.Errorf("payload at [13:15]: got %v, want %v", data[13:15], []byte{0xAA, 0xBB})
	}
	// total size: 13 header + 2 payload + 4 crc = 19
	if len(data) != 19 {
		t.Errorf("total size: got %d, want 19", len(data))
	}
}

// --- FrameType.String() ------------------------------------------------------

func TestFrameType_String(t *testing.T) {
	cases := []struct {
		ft   protocol.FrameType
		want string
	}{
		{protocol.TypeKeystroke, "KEYSTROKE"},
		{protocol.TypeOutput, "OUTPUT"},
		{protocol.TypeSync, "SYNC"},
		{protocol.TypeACK, "ACK"},
		{protocol.TypeNACK, "NACK"},
		{protocol.TypeHeartbeat, "HEARTBEAT"},
		{protocol.TypeReplayRequest, "REPLAY_REQUEST"},
		{protocol.TypeReplayFrame, "REPLAY_FRAME"},
		{protocol.FrameType(0xFF), "UNKNOWN(0xff)"},
	}
	for _, tc := range cases {
		if got := tc.ft.String(); got != tc.want {
			t.Errorf("FrameType(0x%02x).String() = %q, want %q", uint8(tc.ft), got, tc.want)
		}
	}
}

// --- helpers (internal to test file) ----------------------------------------

// recomputeCRC recomputes the CRC32 over data[:len-4] and writes it to
// the last 4 bytes. Used in tests that need to corrupt a field other than
// the checksum but still produce a frame that passes the checksum test.
func recomputeCRC(data []byte) {
	import_crc32 := func(b []byte) uint32 {
		// Re-derive the same table used in production code.
		// We can't import the unexported crcTable, so we recompute.
		import_table := crc32Table()
		var crc uint32 = 0xFFFFFFFF
		for _, v := range b {
			crc = import_table[byte(crc)^v] ^ (crc >> 8)
		}
		return crc ^ 0xFFFFFFFF
	}
	body := data[:len(data)-4]
	checksum := import_crc32(body)
	binary.BigEndian.PutUint32(data[len(data)-4:], checksum)
}

// crc32Table returns the IEEE CRC32 lookup table, matching what production
// code uses. We call this from recomputeCRC in tests.
func crc32Table() [256]uint32 {
	var table [256]uint32
	const poly = 0xEDB88320 // IEEE polynomial, bit-reversed
	for i := range table {
		crc := uint32(i)
		for j := 0; j < 8; j++ {
			if crc&1 != 0 {
				crc = (crc >> 1) ^ poly
			} else {
				crc >>= 1
			}
		}
		table[i] = crc
	}
	return table
}
