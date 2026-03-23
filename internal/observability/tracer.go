package observability

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// instrumentationName is the OTel instrumentation scope — identifies the
// library that produced the spans. Follows the convention of using the
// module path for uniqueness.
const instrumentationName = "github.com/anshk/conduit"

// Tracer returns the package-scoped OpenTelemetry tracer.
//
// By default otel.GetTracerProvider() returns a no-op provider, meaning all
// spans are created but immediately discarded with zero overhead. This lets
// us instrument the entire codebase now without requiring a tracing backend.
//
// Layer 10 will call otel.SetTracerProvider(sdkProvider) at server startup,
// switching every Tracer() call in the codebase to the real SDK simultaneously —
// no code changes needed in the instrumented packages.
//
// Why OpenTelemetry over Jaeger's or Zipkin's native Go clients?
// OTel is now the CNCF standard — vendor-neutral. The same instrumentation
// code works with Jaeger, Tempo, Honeycomb, Datadog, and any other OTLP-
// compatible backend. Switching backends requires only a different exporter
// in the SDK configuration, not changes to the instrumented code.
func Tracer() trace.Tracer {
	return otel.Tracer(instrumentationName)
}

// Attribute constructors — thin wrappers around otel/attribute that use
// Conduit-specific key names. Centralising them here means renaming a key
// only requires one edit, not a grep across every package.

// AttrSessionID returns an OTel attribute for a session ID.
func AttrSessionID(id uint32) attribute.KeyValue {
	return attribute.Int("conduit.session_id", int(id))
}

// AttrClientID returns an OTel attribute for a client (WebSocket connection) ID.
func AttrClientID(id uint32) attribute.KeyValue {
	return attribute.Int("conduit.client_id", int(id))
}

// AttrReplayFrames returns an OTel attribute for the number of replay frames sent.
func AttrReplayFrames(n int) attribute.KeyValue {
	return attribute.Int("conduit.replay_frames", n)
}
