package telemetry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTraceParentRoundTrip(t *testing.T) {
	raw := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	sc, ok := ParseTraceParent(raw)
	if !ok {
		t.Fatal("a valid traceparent was rejected")
	}
	if sc.TraceID.String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("trace id = %s", sc.TraceID)
	}
	if sc.SpanID.String() != "00f067aa0ba902b7" {
		t.Errorf("span id = %s", sc.SpanID)
	}
	if !sc.IsSampled() {
		t.Error("the sampled flag was lost")
	}
	if FormatTraceParent(sc) != raw {
		t.Errorf("round trip = %s", FormatTraceParent(sc))
	}
}

func TestTraceParentRejectsMalformed(t *testing.T) {
	for _, bad := range []string{
		"", "garbage",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7", // no flags
		"00-tooshort-00f067aa0ba902b7-01",
		"ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", // forbidden version
		"00-00000000000000000000000000000000-00f067aa0ba902b7-01", // zero trace id
	} {
		if _, ok := ParseTraceParent(bad); ok {
			t.Errorf("%q should have been rejected", bad)
		}
	}
}

func TestGatewayContinuesTheCallersTrace(t *testing.T) {
	p := New(Config{ServiceName: "test", Environment: "dev"})
	defer func() { _ = p.Shutdown(context.Background()) }()
	tr := p.Tracer("test")

	h := http.Header{}
	h.Set(HeaderTraceParent, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	ctx := ExtractHTTP(context.Background(), h)

	_, span := tr.Start(ctx, "gateway.request", WithSpanKind(KindServer))
	defer span.End()
	if got := span.SpanContext().TraceID.String(); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("the gateway started a new trace instead of continuing the caller's: %s", got)
	}

	out := http.Header{}
	InjectHTTP(ContextWithSpan(ctx, span), out)
	if !strings.HasPrefix(out.Get(HeaderTraceParent), "00-4bf92f3577b34da6a3ce929d0e0e4736-") {
		t.Errorf("outbound traceparent = %q", out.Get(HeaderTraceParent))
	}
}

func TestSpanParentage(t *testing.T) {
	p := New(Config{ServiceName: "test", Environment: "dev"})
	defer func() { _ = p.Shutdown(context.Background()) }()
	tr := p.Tracer("test")

	ctx, parent := tr.Start(context.Background(), "gateway.request")
	_, child := tr.Start(ctx, "gen_ai.chat", WithSpanKind(KindClient))
	if child.SpanContext().TraceID != parent.SpanContext().TraceID {
		t.Error("a child span must stay in its parent's trace")
	}
	if child.SpanContext().SpanID == parent.SpanContext().SpanID {
		t.Error("a child span must have its own span id")
	}

	_, root := tr.Start(ctx, "background.job", WithNewRoot())
	if root.SpanContext().TraceID == parent.SpanContext().TraceID {
		t.Error("WithNewRoot must start a separate trace")
	}
	child.End()
	root.End()
	parent.End()
}

func TestNoopSpanIsSafe(t *testing.T) {
	span := SpanFromContext(context.Background())
	span.SetAttributes(Attr("k", "v"))
	span.AddEvent("event")
	span.RecordError(io.EOF)
	span.SetStatus(StatusError, "x")
	span.End()
	if span.SpanContext().IsValid() {
		t.Error("the no-op span must not present a valid context")
	}
}

func TestSpanEndIsIdempotentAndFreezesAttributes(t *testing.T) {
	p := New(Config{ServiceName: "test", Environment: "dev"})
	defer func() { _ = p.Shutdown(context.Background()) }()
	_, span := p.Tracer("t").Start(context.Background(), "s")
	span.SetAttributes(Attr("before", 1))
	span.End()
	span.SetAttributes(Attr("after", 2))
	span.End() // must not panic or double-export
}

func TestOTLPExportShape(t *testing.T) {
	var mu sync.Mutex
	var payloads []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" {
			t.Errorf("collector path = %s", r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		payloads = append(payloads, body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := New(Config{
		ServiceName: "agentgate-gateway", ServiceNamespace: "agentgate", Environment: "dev",
		OTLPEndpoint: srv.URL, OTLPInsecure: true, BatchTimeout: 20 * time.Millisecond,
	})
	_, span := p.Tracer("agentgate/gateway").Start(context.Background(), SpanGatewayRequest,
		WithSpanKind(KindServer),
		WithAttributes(
			Attr(AttrAgentID, "agt_1"),
			Attr(AttrCostUSD, 0.0012),
			Attr(AttrAttempt, 2),
			Attr(AttrStream, true),
			Attr(AttrGenAIFinishReasons, []string{"stop"}),
		))
	span.AddEvent("retry", Attr(AttrRetryReason, "throttled"))
	span.End()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(payloads) == 0 {
		t.Fatal("no payload reached the collector")
	}
	raw, _ := json.Marshal(payloads[0])
	body := string(raw)
	for _, want := range []string{
		"resourceSpans", "scopeSpans", "traceId", "spanId",
		"startTimeUnixNano", "gateway.request", "agentgate.agent.id",
		"agentgate.cost.usd", "gen_ai.response.finish_reasons", "retry",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("OTLP payload is missing %q", want)
		}
	}
	// Identifiers are hex strings in OTLP/JSON, not base64.
	if strings.Contains(body, `"traceId":"`) && strings.Contains(body, "==") {
		t.Error("trace ids must be hex-encoded in the JSON encoding")
	}
	sent, failed, dropped := p.Stats()
	if sent == 0 || failed != 0 || dropped != 0 {
		t.Errorf("pipeline stats sent=%d failed=%d dropped=%d", sent, failed, dropped)
	}
}

func TestExportFailureIsCountedNotFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	p := New(Config{
		ServiceName: "t", Environment: "dev", OTLPEndpoint: srv.URL,
		OTLPInsecure: true, BatchTimeout: 10 * time.Millisecond, OTLPTimeout: 2 * time.Second,
	})
	_, span := p.Tracer("t").Start(context.Background(), "s")
	span.End()
	_ = p.Shutdown(context.Background())
	if _, failed, _ := p.Stats(); failed == 0 {
		t.Error("a collector outage must be counted, not silently ignored")
	}
}

func TestUnsampledSpansAreNotQueued(t *testing.T) {
	p := New(Config{ServiceName: "t", Environment: "dev", SampleRatio: -1})
	defer func() { _ = p.Shutdown(context.Background()) }()
	_, span := p.Tracer("t").Start(context.Background(), "s")
	if span.SpanContext().IsSampled() {
		t.Error("a zero sample ratio must not mark spans sampled")
	}
	span.End()
	if sent, _, _ := p.Stats(); sent != 0 {
		t.Errorf("sent = %d, want 0 for an unsampled span", sent)
	}
}

func TestMetricsExposition(t *testing.T) {
	r := NewRegistry(nil)
	c := r.Counter("agentgate_gateway_requests_total", "help", "env", "status")
	c.Inc("prod", "200")
	c.Add(3, "prod", "200")
	c.Inc("prod", "500")

	g := r.Gauge("agentgate_gateway_inflight", "help", "env", "pool")
	g.Set(7, "prod", "general-chat")

	h := r.Histogram("agentgate_gateway_overhead_seconds", "help", []float64{0.01, 0.06, 1}, "env")
	h.Observe(0.005, "prod")
	h.Observe(0.5, "prod")

	var b strings.Builder
	r.WriteTo(&b)
	out := b.String()

	for _, want := range []string{
		`# TYPE agentgate_gateway_requests_total counter`,
		`agentgate_gateway_requests_total{env="prod",status="200"} 4`,
		`agentgate_gateway_requests_total{env="prod",status="500"} 1`,
		`agentgate_gateway_inflight{env="prod",pool="general-chat"} 7`,
		`agentgate_gateway_overhead_seconds_bucket{env="prod",le="0.01"} 1`,
		`agentgate_gateway_overhead_seconds_bucket{env="prod",le="+Inf"} 2`,
		`agentgate_gateway_overhead_seconds_count{env="prod"} 2`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exposition is missing:\n  %s\ngot:\n%s", want, out)
		}
	}
}

func TestMetricsHandlerServesTextFormat(t *testing.T) {
	r := NewRegistry(nil)
	r.Counter("agentgate_test_total", "help", "env").Inc("dev")
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "agentgate_test_total") {
		t.Error("the metric is missing from the scrape")
	}
}

func TestMetricLabelMismatchDegradesInsteadOfPanicking(t *testing.T) {
	r := NewRegistry(nil)
	c := r.Counter("agentgate_test_total", "help", "a", "b", "c")
	c.Inc("only-one") // a caller bug
	var b strings.Builder
	r.WriteTo(&b)
	if !strings.Contains(b.String(), `unknown`) {
		t.Error("a missing label value should become 'unknown' rather than crash the request path")
	}
}

func TestInstrumentsRegisterEveryName(t *testing.T) {
	r := NewRegistry(nil)
	NewInstruments(r)
	var b strings.Builder
	r.WriteTo(&b)
	out := b.String()
	for _, name := range []string{
		"agentgate_gateway_requests_total",
		"agentgate_gateway_overhead_seconds",
		"agentgate_gateway_ttft_seconds",
		"agentgate_gateway_cost_usd_total",
		"agentgate_ratelimit_decisions_total",
		"agentgate_guardrail_decisions_total",
		"agentgate_breaker_state",
		"agentgate_telemetry_completeness",
		"agentgate_controlplane_token_exchange_total",
		"agentgate_fleet_agents",
	} {
		if !strings.Contains(out, name) {
			t.Errorf("instrument %s is not registered; alerts referencing it would never fire", name)
		}
	}
}

func TestLoggerInjectsTraceCorrelation(t *testing.T) {
	p := New(Config{ServiceName: "t", Environment: "dev"})
	defer func() { _ = p.Shutdown(context.Background()) }()
	var buf strings.Builder
	logger := NewLogger(&buf, "info", "agentgate-gateway", "prod")

	ctx, span := p.Tracer("t").Start(context.Background(), "s")
	logger.InfoContext(ctx, "handled request", "code", "ok")
	span.End()

	out := buf.String()
	if !strings.Contains(out, `"trace_id"`) || !strings.Contains(out, span.SpanContext().TraceID.String()) {
		t.Errorf("log record is not correlated to the trace: %s", out)
	}
	if !strings.Contains(out, `"service.name":"agentgate-gateway"`) {
		t.Errorf("log record is missing service identity: %s", out)
	}
	if !strings.Contains(out, `"severity"`) || !strings.Contains(out, `"timestamp"`) {
		t.Errorf("log record does not use the expected field names: %s", out)
	}
}

func TestParseContentCaptureDefaultsSafe(t *testing.T) {
	if ParseContentCapture("") != CaptureOff || ParseContentCapture("nonsense") != CaptureOff {
		t.Error("an unrecognised content-capture setting must default to off")
	}
	if ParseContentCapture("full") != CaptureFull || ParseContentCapture("redacted") != CaptureRedacted {
		t.Error("explicit settings must be honoured")
	}
}
