package telemetry

import (
	"context"
	"encoding/hex"
	"net/http"
	"strings"
)

// W3C trace context headers. The gateway continues the caller's trace rather
// than starting a new one, which is what makes an agent run and its model
// calls a single trace across runtimes.
const (
	HeaderTraceParent = "traceparent"
	HeaderTraceState  = "tracestate"
)

type remoteKey struct{}

// ExtractHTTP reads W3C trace context from an inbound request and returns a
// context that new spans will parent themselves to. A malformed or absent
// header is not an error: the request simply starts a new trace.
func ExtractHTTP(ctx context.Context, h http.Header) context.Context {
	sc, ok := ParseTraceParent(h.Get(HeaderTraceParent))
	if !ok {
		return ctx
	}
	sc.TraceState = h.Get(HeaderTraceState)
	sc.Remote = true
	return context.WithValue(ctx, remoteKey{}, sc)
}

// InjectHTTP writes the active span context onto outbound request headers.
func InjectHTTP(ctx context.Context, h http.Header) {
	sc := SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return
	}
	h.Set(HeaderTraceParent, FormatTraceParent(sc))
	if sc.TraceState != "" {
		h.Set(HeaderTraceState, sc.TraceState)
	}
}

// ParseTraceParent parses a W3C traceparent header value.
//
//	version "-" trace-id "-" parent-id "-" trace-flags
//	00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01
func ParseTraceParent(v string) (SpanContext, bool) {
	if len(v) < 55 {
		return SpanContext{}, false
	}
	parts := strings.Split(v, "-")
	if len(parts) < 4 {
		return SpanContext{}, false
	}
	if len(parts[0]) != 2 || parts[0] == "ff" {
		return SpanContext{}, false
	}
	if len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) < 2 {
		return SpanContext{}, false
	}
	tid, err := hex.DecodeString(parts[1])
	if err != nil {
		return SpanContext{}, false
	}
	sid, err := hex.DecodeString(parts[2])
	if err != nil {
		return SpanContext{}, false
	}
	flags, err := hex.DecodeString(parts[3][:2])
	if err != nil {
		return SpanContext{}, false
	}
	var sc SpanContext
	copy(sc.TraceID[:], tid)
	copy(sc.SpanID[:], sid)
	sc.TraceFlags = flags[0]
	if !sc.IsValid() {
		return SpanContext{}, false
	}
	return sc, true
}

// FormatTraceParent renders a span context as a traceparent header value.
func FormatTraceParent(sc SpanContext) string {
	flags := "00"
	if sc.IsSampled() {
		flags = "01"
	}
	return "00-" + sc.TraceID.String() + "-" + sc.SpanID.String() + "-" + flags
}
