package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestErrorCodeStatusMappingIsComplete(t *testing.T) {
	// Every code in the frozen contract must have a status. A code without one
	// would silently become a 500 in production.
	codes := []ErrorCode{
		CodeInvalidRequest, CodeUnauthenticated, CodeForbiddenPool, CodeAgentNotPromoted,
		CodeGuardrailBlocked, CodeUnknownModel, CodeClientTimeout, CodeIdempotencyConflict,
		CodeContextTooLarge, CodeRateLimited, CodeQuotaExceeded, CodeClientClosedRequest,
		CodeProviderError, CodeNoHealthyBackend, CodeProviderTimeout,
	}
	want := map[ErrorCode]int{
		CodeInvalidRequest: 400, CodeUnauthenticated: 401, CodeForbiddenPool: 403,
		CodeAgentNotPromoted: 403, CodeGuardrailBlocked: 403, CodeUnknownModel: 404,
		CodeClientTimeout: 408, CodeIdempotencyConflict: 409, CodeContextTooLarge: 413,
		CodeRateLimited: 429, CodeQuotaExceeded: 429, CodeClientClosedRequest: 499,
		CodeProviderError: 502, CodeNoHealthyBackend: 503, CodeProviderTimeout: 504,
	}
	for _, c := range codes {
		if got := StatusFor(c); got != want[c] {
			t.Errorf("StatusFor(%s) = %d, want %d; this mapping is frozen", c, got, want[c])
		}
		p := NewProblem(c, "detail")
		if p.Title == "" {
			t.Errorf("code %s has no human-readable title", c)
		}
		if p.Type == "" || !strings.HasPrefix(p.Type, "https://") {
			t.Errorf("code %s has no documentation URI", c)
		}
	}
}

func TestProblemSerialisation(t *testing.T) {
	rec := httptest.NewRecorder()
	p := Errorf(CodeQuotaExceeded, "agent %s exceeded %d tokens per minute", "agent://a/b/c", 120000).
		WithRetryAfter(17)
	WriteProblem(rec, p, "req_1", "trace_1")

	if rec.Code != 429 {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("content type = %q", ct)
	}
	if rec.Header().Get("Retry-After") != "17" {
		t.Errorf("Retry-After = %q", rec.Header().Get("Retry-After"))
	}
	var got Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Code != CodeQuotaExceeded || got.RequestID != "req_1" || got.TraceID != "trace_1" {
		t.Errorf("problem = %#v", got)
	}
	if !strings.Contains(got.Detail, "120000") {
		t.Error("the detail must tell the caller what limit was hit")
	}
}

func TestClientClosedRequestWritesNothing(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteProblem(rec, NewProblem(CodeClientClosedRequest, "gone"), "r", "t")
	if rec.Body.Len() != 0 {
		t.Error("nothing should be written to a caller that has already disconnected")
	}
}

func TestAsProblemHidesUnexpectedErrors(t *testing.T) {
	p := AsProblem(errNotAProblem{})
	if p.Code != CodeInternal {
		t.Errorf("code = %s", p.Code)
	}
	if strings.Contains(p.Detail, "database password") {
		t.Fatal("an unexpected error's text must never be forwarded to the caller")
	}
	original := NewProblem(CodeRateLimited, "slow down")
	if AsProblem(original) != original {
		t.Error("a Problem must pass through unchanged")
	}
	if AsProblem(nil) != nil {
		t.Error("nil must map to nil")
	}
}

type errNotAProblem struct{}

func (errNotAProblem) Error() string {
	return "connection to postgres failed: database password rejected"
}

func TestSSEFraming(t *testing.T) {
	rec := httptest.NewRecorder()
	sse, err := NewSSEWriter(rec, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content type = %q", ct)
	}
	if rec.Header().Get("X-Accel-Buffering") != "no" {
		t.Error("proxy buffering must be disabled or time to first token is meaningless")
	}
	if err := sse.Data(map[string]any{"a": 1}); err != nil {
		t.Fatal(err)
	}
	if err := sse.Event("agentgate.usage", map[string]any{"cost_usd": 0.01}); err != nil {
		t.Fatal(err)
	}
	sse.Done()

	body := rec.Body.String()
	if !strings.Contains(body, "data: {\"a\":1}\n\n") {
		t.Errorf("unnamed frame is malformed: %q", body)
	}
	if !strings.Contains(body, "event: agentgate.usage\ndata: ") {
		t.Errorf("named frame is malformed: %q", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("the stream must end with the sentinel: %q", body)
	}
}

func TestSSERoundTrip(t *testing.T) {
	rec := httptest.NewRecorder()
	sse, _ := NewSSEWriter(rec, 0)
	_ = sse.Data(map[string]any{"n": 1})
	_ = sse.Event("error", map[string]any{"code": "provider_error"})
	sse.Done()

	var events []SSEEvent
	if err := ReadSSE(strings.NewReader(rec.Body.String()), func(ev SSEEvent) error {
		events = append(events, ev)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("decoded %d events, want 3", len(events))
	}
	if events[1].Event != "error" {
		t.Errorf("named event lost: %#v", events[1])
	}
	if !events[2].IsDone() {
		t.Error("the sentinel was not recognised")
	}
}

func TestReadSSESkipsHeartbeats(t *testing.T) {
	stream := ": heartbeat\n\ndata: {\"n\":1}\n\ndata: [DONE]\n\n"
	var count int
	if err := ReadSSE(strings.NewReader(stream), func(ev SSEEvent) error {
		count++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("decoded %d events; heartbeat comments must be skipped", count)
	}
}

func TestSSEHeartbeatKeepsIdleStreamsOpen(t *testing.T) {
	rec := httptest.NewRecorder()
	sse, err := NewSSEWriter(rec, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	sse.WriteHeaderOnce()
	time.Sleep(20 * time.Millisecond)
	sse.Close()
	if !strings.Contains(rec.Body.String(), ": heartbeat") {
		t.Error("an idle stream must emit heartbeats or an intermediary will drop it")
	}
}

func TestSSERequiresAFlusher(t *testing.T) {
	if _, err := NewSSEWriter(&nonFlushingWriter{rec: httptest.NewRecorder()}, 0); err == nil {
		t.Error("a ResponseWriter that cannot flush must be refused rather than silently buffering the whole stream")
	}
}

// nonFlushingWriter deliberately does not embed the recorder, so it does not
// inherit its Flush method.
type nonFlushingWriter struct{ rec *httptest.ResponseRecorder }

func (n *nonFlushingWriter) Header() http.Header         { return n.rec.Header() }
func (n *nonFlushingWriter) Write(b []byte) (int, error) { return n.rec.Write(b) }
func (n *nonFlushingWriter) WriteHeader(code int)        { n.rec.WriteHeader(code) }

func TestClientTLSAndProxyConfiguration(t *testing.T) {
	if _, err := NewClient(ClientConfig{CABundlePath: "/does/not/exist"}); err == nil {
		t.Error("a missing CA bundle must fail at startup, not at the first request")
	}
	if _, err := NewClient(ClientConfig{Proxy: "://not a url"}); err == nil {
		t.Error("an unparseable proxy must be refused")
	}
	c, err := NewClient(ClientConfig{Timeout: 5 * time.Second, MaxIdleConnsPerHost: 8})
	if err != nil {
		t.Fatal(err)
	}
	if c.Timeout != 5*time.Second {
		t.Errorf("timeout = %s", c.Timeout)
	}
}
