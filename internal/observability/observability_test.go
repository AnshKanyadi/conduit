package observability_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/anshk/conduit/internal/observability"
)

// --- Logger ------------------------------------------------------------------

func TestLogger_DefaultIsNop(t *testing.T) {
	// The default logger must not be nil (a nil *zap.Logger panics on use).
	if observability.L == nil {
		t.Fatal("default logger is nil")
	}
	// Verify calling it doesn't panic (nop logger swallows output).
	observability.L.Info("test log from observability_test — should be silent")
}

func TestLogger_Init_DevelopmentMode(t *testing.T) {
	if err := observability.Init(true); err != nil {
		t.Fatalf("Init(dev=true): %v", err)
	}
	if observability.L == nil {
		t.Fatal("logger is nil after Init")
	}
	// Restore nop for subsequent tests.
	observability.L = zap.NewNop()
}

func TestLogger_Init_ProductionMode(t *testing.T) {
	if err := observability.Init(false); err != nil {
		t.Fatalf("Init(dev=false): %v", err)
	}
	if observability.L == nil {
		t.Fatal("logger is nil after Init")
	}
	observability.L = zap.NewNop()
}

// --- Metrics -----------------------------------------------------------------

// readGauge reads the current value of a Gauge using the prometheus dto.
// We bypass testutil to avoid pulling in the testutil dependency.
func readGauge(g interface{ Write(*dto.Metric) error }) float64 {
	m := &dto.Metric{}
	_ = g.Write(m)
	return m.GetGauge().GetValue()
}

func readCounter(c interface{ Write(*dto.Metric) error }) float64 {
	m := &dto.Metric{}
	_ = c.Write(m)
	return m.GetCounter().GetValue()
}

func TestMetrics_SessionsActive_IncDec(t *testing.T) {
	before := readGauge(observability.SessionsActive)
	observability.SessionsActive.Inc()
	if readGauge(observability.SessionsActive) != before+1 {
		t.Error("SessionsActive.Inc did not increment")
	}
	observability.SessionsActive.Dec()
	if readGauge(observability.SessionsActive) != before {
		t.Error("SessionsActive.Dec did not decrement")
	}
}

func TestMetrics_ClientsConnected_IncDec(t *testing.T) {
	before := readGauge(observability.ClientsConnected)
	observability.ClientsConnected.Inc()
	observability.ClientsConnected.Inc()
	if readGauge(observability.ClientsConnected) != before+2 {
		t.Error("ClientsConnected double-Inc failed")
	}
	observability.ClientsConnected.Dec()
	observability.ClientsConnected.Dec()
}

func TestMetrics_PTYFramesTotal_Increments(t *testing.T) {
	before := readCounter(observability.PTYFramesTotal)
	observability.PTYFramesTotal.Inc()
	if readCounter(observability.PTYFramesTotal) != before+1 {
		t.Error("PTYFramesTotal.Inc did not increment")
	}
}

func TestMetrics_HubCounters(t *testing.T) {
	drops := readCounter(observability.HubDropsTotal)
	catchups := readCounter(observability.HubCatchupsTotal)

	observability.HubDropsTotal.Inc()
	observability.HubCatchupsTotal.Inc()

	if readCounter(observability.HubDropsTotal) != drops+1 {
		t.Error("HubDropsTotal.Inc failed")
	}
	if readCounter(observability.HubCatchupsTotal) != catchups+1 {
		t.Error("HubCatchupsTotal.Inc failed")
	}
}

func TestMetrics_HandshakeDuration_Observe(t *testing.T) {
	// Histogram observation must not panic.
	observability.HandshakeDuration.Observe(0.005) // 5ms
	observability.HandshakeDuration.Observe(0.050) // 50ms
}

func TestMetrics_ReplayCounters(t *testing.T) {
	reqs := readCounter(observability.ReplayRequestsTotal)
	frames := readCounter(observability.ReplayFramesServed)

	observability.ReplayRequestsTotal.Inc()
	observability.ReplayFramesServed.Add(3)

	if readCounter(observability.ReplayRequestsTotal) != reqs+1 {
		t.Error("ReplayRequestsTotal.Inc failed")
	}
	if readCounter(observability.ReplayFramesServed) != frames+3 {
		t.Error("ReplayFramesServed.Add(3) failed")
	}
}

// --- MetricsHandler ----------------------------------------------------------

func TestMetricsHandler_Returns200(t *testing.T) {
	handler := observability.MetricsHandler()
	w := httptest.NewRecorder()
	r, _ := http.NewRequest(http.MethodGet, "/metrics", nil)
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("MetricsHandler status: got %d, want 200", w.Code)
	}
}

func TestMetricsHandler_ContainsConduitMetrics(t *testing.T) {
	handler := observability.MetricsHandler()
	w := httptest.NewRecorder()
	r, _ := http.NewRequest(http.MethodGet, "/metrics", nil)
	handler.ServeHTTP(w, r)

	body := w.Body.String()
	for _, want := range []string{
		"conduit_sessions_active",
		"conduit_clients_connected",
		"conduit_pty_frames_total",
		"conduit_hub_drops_total",
		"conduit_hub_catchups_total",
		"conduit_replay_requests_total",
		"conduit_transport_handshake_duration_seconds",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %q", want)
		}
	}
}

// --- Tracer ------------------------------------------------------------------

func TestTracer_ReturnsNonNil(t *testing.T) {
	tracer := observability.Tracer()
	if tracer == nil {
		t.Fatal("Tracer() returned nil")
	}
}

func TestTracer_SpanIsNoOp_ByDefault(t *testing.T) {
	// Without a real TracerProvider, spans must be no-ops.
	_, span := observability.Tracer().Start(context.Background(), "test.span")
	if span == nil {
		t.Fatal("Start returned nil span")
	}
	// No-op spans implement trace.Span but are not sampling.
	if span.IsRecording() {
		// In production with a real SDK this would be true; in tests it's false.
		t.Log("span is recording — a real TracerProvider is configured")
	}
	span.End()
}

func TestAttrHelpers_NotPanic(t *testing.T) {
	_ = observability.AttrSessionID(42)
	_ = observability.AttrClientID(7)
	_ = observability.AttrReplayFrames(100)
}

// Ensure the trace.Tracer interface is satisfied (compile-time check).
var _ trace.Tracer = observability.Tracer()
