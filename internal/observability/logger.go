package observability

import "go.uber.org/zap"

// L is the global structured logger. Defaults to a no-op logger so that
// packages importing observability work correctly in tests without any
// log output appearing on stderr.
//
// The application (cmd/server) calls Init() once at startup to replace this
// with a production or development logger. Tests that want to verify log
// output can replace L directly: observability.L = zaptest.NewLogger(t)
var L = zap.NewNop()

// Init configures the global logger.
//
//   - dev=true:  console format, DEBUG level, coloured output — for local development
//   - dev=false: JSON format, INFO level — for log aggregators (Loki, ELK, Datadog)
//
// Why zap instead of slog (stdlib) or logrus?
//   - Zero-allocation on the hot path: zap pre-allocates a buffer per logger
//     and never uses fmt.Sprintf for structured fields.
//   - 10–100× faster than fmt-based loggers at high log volume, which matters
//     when every PTY output frame can trigger a log entry.
//   - slog (Go 1.21+) is a good alternative but lacks the ecosystem of
//     zap sinks, sampling, and middleware that we'll need in Layer 10.
func Init(dev bool) error {
	var (
		l   *zap.Logger
		err error
	)
	if dev {
		l, err = zap.NewDevelopment()
	} else {
		l, err = zap.NewProduction()
	}
	if err != nil {
		return err
	}
	L = l
	return nil
}
