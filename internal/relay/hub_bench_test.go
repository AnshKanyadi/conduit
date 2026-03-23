package relay_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/anshk/conduit/internal/relay"
)

// BenchmarkHub_Dispatch measures single-hub throughput for N concurrent clients.
//
// Target: ≥ 100,000 frames/sec on a single node (design requirement Layer 5).
//
// Run with:
//
//	go test -bench=BenchmarkHub_Dispatch -benchtime=5s ./internal/relay/
func BenchmarkHub_Dispatch(b *testing.B) {
	for _, numClients := range []int{1, 4, 16, 64} {
		b.Run(fmt.Sprintf("clients=%d", numClients), func(b *testing.B) {
			hub := relay.NewHub(relay.HubConfig{})
			go hub.Run()
			defer hub.Stop()

			// Each client has a large channel so sends never block during the bench.
			// We want to measure hub dispatch throughput, not channel contention.
			for i := 0; i < numClients; i++ {
				ch := make(chan []byte, b.N+1024)
				hub.Register(uint32(i+1), ch, func() {})
			}
			// Let registrations settle.
			time.Sleep(10 * time.Millisecond)

			frame := []byte("benchmark frame payload — 40 bytes padded!!")

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				hub.Dispatch(frame)
			}

			// Drain the dispatch channel before Stop() to avoid dropping frames
			// from the benchmark count (Stop closes the hub goroutine).
			b.StopTimer()
		})
	}
}

// BenchmarkCompressPayload measures LZ4 compression throughput on typical
// PTY output chunks (mix of ASCII text and ANSI escape sequences).
func BenchmarkCompressPayload(b *testing.B) {
	// Simulate a 4 KB PTY read — typical kernel PTY buffer flush.
	payload := make([]byte, 4096)
	for i := range payload {
		payload[i] = byte('a' + i%26)
	}

	b.ResetTimer()
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))

	for i := 0; i < b.N; i++ {
		if _, err := relay.CompressPayload(payload); err != nil {
			b.Fatal(err)
		}
	}
}
