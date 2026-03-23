package observability

// Layer 7 — Prometheus metrics for Conduit.
//
// All metrics use a custom registry (not prometheus.DefaultRegisterer) so that:
//   a) Tests can import this package without polluting the global registry.
//   b) The /metrics handler serves only Conduit metrics, not Go runtime metrics
//      mixed with unrelated process-level data (we add those explicitly below).
//
// Why Prometheus and not StatsD or InfluxDB line protocol?
// Prometheus uses a pull model: the server exposes /metrics and Prometheus
// scrapes it. This means the server needs zero knowledge of the collection
// infrastructure — a huge operational win. StatsD pushes metrics to a UDP
// receiver; if the receiver is down, metrics are silently lost. Prometheus'
// pull model naturally handles gaps and restarts with staleness markers.
//
// Naming convention: conduit_<subsystem>_<unit>_<suffix>
// where suffix is one of: total (Counter), active (Gauge), seconds (Histogram)

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// reg is the custom Prometheus registry for all Conduit metrics.
	// Using a non-default registry avoids registration conflicts in tests
	// and keeps the /metrics output scoped to Conduit-specific data.
	reg = prometheus.NewRegistry()

	// SessionsActive tracks the number of live sessions (Active + Suspended).
	// A session is counted from Create until Remove, regardless of whether any
	// client is currently connected. This lets us distinguish "no clients" from
	// "no sessions" in dashboards.
	SessionsActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "conduit",
		Subsystem: "sessions",
		Name:      "active",
		Help:      "Number of live terminal sessions (Active or Suspended state).",
	})

	// ClientsConnected tracks the number of open WebSocket connections.
	// Multiple clients can be connected to a single session (multiplayer mode).
	ClientsConnected = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "conduit",
		Subsystem: "clients",
		Name:      "connected",
		Help:      "Number of currently open WebSocket connections.",
	})

	// PTYFramesTotal counts OUTPUT frames dispatched from pumpPTY → Hub.
	// A sudden drop indicates the PTY process exited; a sustained high rate
	// with HubDropsTotal > 0 indicates clients falling behind.
	PTYFramesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "conduit",
		Subsystem: "pty",
		Name:      "frames_total",
		Help:      "Total OUTPUT frames read from PTY and dispatched to the relay hub.",
	})

	// HubCatchupsTotal counts clients promoted to catch-up mode.
	// Each increment means a client accumulated lag ≥ LagCatchup (500 frames)
	// and was notified to request a replay from the log.
	HubCatchupsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "conduit",
		Subsystem: "hub",
		Name:      "catchups_total",
		Help:      "Clients promoted to catch-up mode (lag ≥ 500 frames).",
	})

	// HubDropsTotal counts clients forcibly disconnected for being too slow.
	// Each increment means a client accumulated lag ≥ LagDrop (2000 frames)
	// and was disconnected. Alert if this is consistently > 0 in production.
	HubDropsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "conduit",
		Subsystem: "hub",
		Name:      "drops_total",
		Help:      "Clients forcibly disconnected for exceeding the lag drop threshold (2000 frames).",
	})

	// ReplayRequestsTotal counts TypeReplayRequest frames processed.
	ReplayRequestsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "conduit",
		Subsystem: "replay",
		Name:      "requests_total",
		Help:      "Total TypeReplayRequest frames served.",
	})

	// ReplayFramesServed counts TypeReplayFrame messages sent to clients.
	// Divide by ReplayRequestsTotal to get the average replay depth.
	ReplayFramesServed = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "conduit",
		Subsystem: "replay",
		Name:      "frames_served_total",
		Help:      "Total TypeReplayFrame messages sent during replay.",
	})

	// HandshakeDuration measures the latency of the WS upgrade + SYNC/ACK exchange.
	// Buckets chosen for interactive latency: < 5ms is excellent, > 500ms is alarming.
	HandshakeDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "conduit",
		Subsystem: "transport",
		Name:      "handshake_duration_seconds",
		Help:      "Time from WebSocket upgrade to ACK sent (handshake round-trip).",
		Buckets:   []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0},
	})
)

func init() {
	// Register all Conduit metrics with the custom registry.
	reg.MustRegister(
		SessionsActive,
		ClientsConnected,
		PTYFramesTotal,
		HubCatchupsTotal,
		HubDropsTotal,
		ReplayRequestsTotal,
		ReplayFramesServed,
		HandshakeDuration,
	)

	// Also collect standard Go runtime and process metrics so dashboards have
	// GC pause times, goroutine counts, and memory usage in one endpoint.
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
}

// MetricsHandler returns an HTTP handler that serves the /metrics endpoint.
// Wire it into the server mux: mux.Handle("/metrics", observability.MetricsHandler())
func MetricsHandler() http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		// Expose errors to the client (useful during development).
		ErrorHandling: promhttp.ContinueOnError,
	})
}
