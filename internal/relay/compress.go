package relay

// Package relay uses LZ4 frame compression for two purposes:
//   1. (Layer 5, here) CompressPayload / DecompressPayload are exported helpers
//      that the relay hub will call when building catch-up or replay payloads
//      for slow clients.
//   2. (Layer 6) The append-only replay log will use these same helpers to
//      compress stored output frames before writing them to disk, and
//      decompress them on read-back.
//
// Why LZ4 over zstd / gzip?
//   - LZ4 Block compression decompresses at ~5 GB/s (hardware-limited) and
//     compresses at ~700 MB/s — well above our per-node PTY throughput.
//   - zstd achieves a better ratio but at 3-4× the CPU cost; for terminal
//     output (which is already small and bursty) the extra ratio gain is not
//     worth the latency.
//   - gzip is slower still and has no advantage here.

import (
	"bytes"

	lz4 "github.com/pierrec/lz4/v4"
)

// CompressPayload compresses data with LZ4 frame format.
// Returns the compressed bytes, which can be decoded with DecompressPayload.
// The caller owns the returned slice.
func CompressPayload(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := lz4.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DecompressPayload decompresses LZ4-frame-compressed data produced by
// CompressPayload. Returns the original bytes. The caller owns the slice.
func DecompressPayload(data []byte) ([]byte, error) {
	r := lz4.NewReader(bytes.NewReader(data))
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
