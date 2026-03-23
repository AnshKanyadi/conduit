// Package observability provides structured logging, distributed tracing,
// and Prometheus metrics for the Conduit server.
//
// Every other internal package imports this one — so it must have zero
// imports from other internal packages to avoid circular dependencies.
//
// Components:
//   - Logger:  uber-go/zap structured JSON logger.
//   - Tracer:  OpenTelemetry trace context that follows a keystroke end-to-end.
//   - Metrics: Prometheus histograms, gauges, and counters for latency,
//     throughput, session count, and replication lag.
package observability
