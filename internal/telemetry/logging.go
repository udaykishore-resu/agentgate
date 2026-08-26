package telemetry

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
)

// LogHandler wraps a slog handler and stamps every record with the active
// trace and span id plus the service identity. Correlating a log line to a
// trace is the difference between an alert that names the failing request and
// one that names the service.
type LogHandler struct {
	inner slog.Handler
	base  []slog.Attr
}

// NewLogger builds the process logger. Output is JSON in every environment so
// the log pipeline never has to guess at a format; level is configurable.
func NewLogger(w io.Writer, level string, service, env string) *slog.Logger {
	if w == nil {
		w = os.Stdout
	}
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: lvl,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			// Align with the log data model the collector expects.
			switch a.Key {
			case slog.TimeKey:
				a.Key = "timestamp"
			case slog.MessageKey:
				a.Key = "message"
			case slog.LevelKey:
				a.Key = "severity"
			}
			return a
		},
	})
	return slog.New(&LogHandler{
		inner: h,
		base: []slog.Attr{
			slog.String(AttrServiceName, service),
			slog.String(AttrDeploymentEnv, env),
		},
	})
}

// Enabled reports whether the level is enabled.
func (h *LogHandler) Enabled(ctx context.Context, l slog.Level) bool { return h.inner.Enabled(ctx, l) }

// Handle stamps trace correlation onto the record and forwards it.
func (h *LogHandler) Handle(ctx context.Context, r slog.Record) error {
	r.AddAttrs(h.base...)
	if sc := SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID.String()),
			slog.String("span_id", sc.SpanID.String()),
		)
	}
	return h.inner.Handle(ctx, r)
}

// WithAttrs returns a handler with additional attributes.
func (h *LogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &LogHandler{inner: h.inner.WithAttrs(attrs), base: h.base}
}

// WithGroup returns a handler with a nested group.
func (h *LogHandler) WithGroup(name string) slog.Handler {
	return &LogHandler{inner: h.inner.WithGroup(name), base: h.base}
}
