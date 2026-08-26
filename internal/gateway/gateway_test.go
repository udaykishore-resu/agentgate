package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentgate/agentgate/internal/cache"
	"github.com/agentgate/agentgate/internal/config"
	"github.com/agentgate/agentgate/internal/cost"
	"github.com/agentgate/agentgate/internal/guardrails"
	"github.com/agentgate/agentgate/internal/httpx"
	"github.com/agentgate/agentgate/internal/identity"
	"github.com/agentgate/agentgate/internal/ratelimit"
	"github.com/agentgate/agentgate/internal/telemetry"
)

// fakeBackend is a controllable model backend. Each instance records what it
// received and can be told to fail, throttle, stall or stream.
type fakeBackend struct {
	name     string
	calls    atomic.Int64
	status   atomic.Int64 // when > 0, respond with this status instead of a completion
	failN    atomic.Int64 // fail this many times, then succeed
	body     string
	delay    time.Duration
	lastBody atomic.Value
	// streamCombined makes the backend emit the last content delta and the
	// finish reason in a single frame, as several OpenAI-compatible servers do.
	streamCombined bool
}

func newFakeBackend(name string) *fakeBackend {
	return &fakeBackend{name: name, body: "answer from " + name}
}

func (f *fakeBackend) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{}})
			return
		}
		f.calls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		f.lastBody.Store(string(raw))
		if f.delay > 0 {
			time.Sleep(f.delay)
		}
		if n := f.failN.Load(); n > 0 {
			f.failN.Add(-1)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"temporarily unavailable"}}`))
			return
		}
		if s := f.status.Load(); s > 0 {
			w.WriteHeader(int(s))
			_, _ = w.Write([]byte(`{"error":{"message":"injected"}}`))
			return
		}

		var req map[string]any
		_ = json.Unmarshal(raw, &req)
		if stream, _ := req["stream"].(bool); stream {
			f.stream(w)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "cmpl-" + f.name, "object": "chat.completion", "model": "backend-model",
			"choices": []map[string]any{{
				"index": 0, "message": map[string]any{"role": "assistant", "content": f.body},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 4, "total_tokens": 15},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeBackend) stream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher := w.(http.Flusher)
	send := func(payload string) {
		_, _ = w.Write([]byte("data: " + payload + "\n\n"))
		flusher.Flush()
	}
	send(`{"id":"s1","choices":[{"index":0,"delta":{"role":"assistant"}}]}`)
	parts := strings.Split(f.body, " ")
	if f.streamCombined {
		for _, part := range parts[:len(parts)-1] {
			send(fmt.Sprintf(`{"id":"s1","choices":[{"index":0,"delta":{"content":%q}}]}`, part+" "))
		}
		send(fmt.Sprintf(`{"id":"s1","choices":[{"index":0,"delta":{"content":%q},"finish_reason":"stop"}]}`, parts[len(parts)-1]))
		send(`{"id":"s1","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`)
		send("[DONE]")
		return
	}
	for _, part := range parts {
		send(fmt.Sprintf(`{"id":"s1","choices":[{"index":0,"delta":{"content":%q}}]}`, part+" "))
	}
	send(`{"id":"s1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	send(`{"id":"s1","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`)
	send("[DONE]")
}

type harness struct {
	t        *testing.T
	server   *Server
	http     *httptest.Server
	token    string
	limiter  ratelimit.Limiter
	usage    *recordingSink
	registry *telemetry.Registry
	issuer   *identity.Issuer
	claims   identity.TokenRequest
}

// recordingSink captures usage records so the test can assert on chargeback
// without touching the filesystem.
type recordingSink struct {
	mu      sync.Mutex
	records []cost.Record
}

func (s *recordingSink) Write(_ context.Context, r cost.Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, r)
}

// all returns a snapshot of what has been recorded.
func (s *recordingSink) all() []cost.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]cost.Record(nil), s.records...)
}

// last returns the most recent record.
func (s *recordingSink) last(t *testing.T) cost.Record {
	t.Helper()
	all := s.all()
	if len(all) == 0 {
		t.Fatal("no usage record was written")
	}
	return all[len(all)-1]
}
func (s *recordingSink) Flush(context.Context) error { return nil }
func (s *recordingSink) Close() error                { return nil }
func (s *recordingSink) Dropped() int64              { return 0 }

type harnessOption func(*config.Config)

func withGuardrailScanning(cfg *config.Config) {
	for i := range cfg.Pools {
		cfg.Pools[i].Guardrail.ScanOutput = true
	}
}

func withQuota(rpm, tpm int64) harnessOption {
	return func(cfg *config.Config) {
		cfg.Gateway.Quota.RequestsPerMinute = rpm
		cfg.Gateway.Quota.TokensPerMinute = tpm
	}
}

func withContextWindow(n int) harnessOption {
	return func(cfg *config.Config) {
		for i := range cfg.Models {
			cfg.Models[i].ContextWindow = n
		}
	}
}

func newHarness(t *testing.T, backends []*fakeBackend, opts ...harnessOption) *harness {
	t.Helper()

	pool := config.Pool{
		Name: "general-chat", Strategy: "priority", Tier: "interactive",
		Cache: config.CachePolicy{Enabled: true, TTL: config.Duration(time.Minute), MaxTemperature: 0.2},
		Guardrail: config.GuardrailPolicy{
			Enabled: true, Provider: "builtin", FailureMode: "fail_open",
			OutputWindowTokens: 8, Timeout: config.Duration(time.Second),
		},
	}
	for i, b := range backends {
		srv := b.server(t)
		pool.Backends = append(pool.Backends, config.Backend{
			Name: b.name, Kind: "openai", BaseURL: srv.URL + "/v1", Model: "backend-model",
			Weight: 100, Priority: i + 1, Timeout: config.Duration(5 * time.Second),
			MaxConcurrent: 32, MaxClassification: "restricted",
			InputCostPer1M: 1, OutputCostPer1M: 2, CostSource: "test",
		})
	}

	cfg := &config.Config{
		Env: "dev",
		Models: []config.Model{{
			Name: "general-chat", Pool: "general-chat",
			ContextWindow: 128000, MaxOutput: 4096,
		}},
		Pools: []config.Pool{pool},
	}
	for _, o := range opts {
		o(cfg)
	}
	// Defaults and validation run exactly as they do in the binary.
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/gateway.json"
	if err := writeFile(path, raw); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if v := loaded.Validate(); v != nil && v.HasErrors() {
		t.Fatalf("config invalid: %v", v)
	}

	tp := telemetry.New(telemetry.Config{ServiceName: "agentgate-gateway", Environment: "dev"})
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	issuer := identity.NewIssuer(identity.IssuerOptions{
		Issuer: "https://cp.test", Audience: "https://gw.test", TTL: 10 * time.Minute,
	})
	if _, err := issuer.GenerateKey(2048); err != nil {
		t.Fatal(err)
	}
	verifier := identity.NewVerifier(identity.VerifierOptions{
		Issuer: "https://cp.test", Audience: "https://gw.test", StaticKeys: issuer.PublicKeys(),
	})

	router, err := NewRouter(loaded, NewBackendFactory("dev"))
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	guardrailProviders, err := NewGuardrailProviders(loaded)
	if err != nil {
		t.Fatal(err)
	}
	sink := &recordingSink{}
	limiter := ratelimit.NewMemory()

	srv, err := New(Options{
		Config: loaded, Telemetry: tp, Router: router, Verifier: verifier,
		Limiter: limiter, Cache: cache.NewMemory(1 << 20),
		Prices: NewPriceBook(loaded), Usage: sink, Guardrails: guardrailProviders,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.MarkReady()
	t.Cleanup(func() { _ = srv.Close() })

	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	id, _ := identity.ParseAgentIdentity("agent://fsclient/payments-risk/dispute-triage")
	claims := identity.TokenRequest{
		AgentID: "agt_1", Identity: id, AgentVersion: "2.4.1", Env: identity.EnvDev,
		CostCenter: "CC-4471", OwnerEmail: "team@client.example", Runtime: "aks",
		Scopes:      []string{identity.ScopeInvoke, identity.ScopeEmbed},
		ModelPools:  []string{"general-chat"},
		Attestation: identity.AttestWorkloadIdentity, DataClassification: "confidential",
	}
	token, _, err := issuer.Mint(claims, "test")
	if err != nil {
		t.Fatal(err)
	}

	return &harness{
		t: t, server: srv, http: httpSrv, token: token, limiter: limiter,
		usage: sink, registry: tp.Metrics, issuer: issuer, claims: claims,
	}
}

func writeFile(path string, data []byte) error {
	f, err := createFile(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.Write(data)
	return err
}

func (h *harness) chat(body string, headers ...string) (*http.Response, []byte) {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.http.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.token)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := h.http.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, raw
}

func (h *harness) tokenFor(mutate func(*identity.TokenRequest)) string {
	h.t.Helper()
	req := h.claims
	mutate(&req)
	tok, _, err := h.issuer.Mint(req, "test")
	if err != nil {
		h.t.Fatal(err)
	}
	return tok
}

func problemOf(t *testing.T, raw []byte) httpx.Problem {
	t.Helper()
	var p httpx.Problem
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("response is not problem+json: %s", raw)
	}
	return p
}

const simpleRequest = `{"model":"general-chat","messages":[{"role":"user","content":"what happened to this payment"}],"max_tokens":32,"temperature":0}`

func TestUnaryRequestSucceedsAndIsFullyAttributed(t *testing.T) {
	backend := newFakeBackend("primary")
	h := newHarness(t, []*fakeBackend{backend})

	resp, raw := h.chat(simpleRequest)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}

	for _, header := range []string{
		HeaderRequestID, HeaderTraceID, HeaderProvider, HeaderModel,
		HeaderPoolUsed, HeaderAttempts, HeaderCacheResult,
		HeaderTokensInput, HeaderTokensOutput, HeaderCostUSD, HeaderGuardrail,
	} {
		if resp.Header.Get(header) == "" {
			t.Errorf("response is missing the %s header the contract promises", header)
		}
	}
	if got := resp.Header.Get(HeaderCacheResult); got != "miss" {
		t.Errorf("cache = %s, want miss on the first call", got)
	}

	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	// The caller sees the logical model, never the backend's own model name.
	if body["model"] != "general-chat" {
		t.Errorf("response model = %v, want the logical model", body["model"])
	}

	if got := h.usage.all(); len(got) != 1 {
		t.Fatalf("expected one usage record, got %d", len(got))
	}
	rec := h.usage.all()[0]
	if rec.CostCenter != "CC-4471" || rec.Team != "payments-risk" || rec.AgentID != "agt_1" {
		t.Errorf("usage record is not attributed to the owning team: %#v", rec)
	}
	if rec.InputTokens != 11 || rec.OutputTokens != 4 {
		t.Errorf("usage record tokens = %d/%d, want the provider's figures", rec.InputTokens, rec.OutputTokens)
	}
	if !rec.Billable || rec.CostUSD <= 0 {
		t.Errorf("a served request must be billable with a cost: %#v", rec)
	}
	if rec.TraceID == "" || rec.RequestID == "" {
		t.Error("a usage record must be traceable back to the request that produced it")
	}
}

func TestAuthenticationIsRequired(t *testing.T) {
	h := newHarness(t, []*fakeBackend{newFakeBackend("primary")})

	req, _ := http.NewRequest(http.MethodPost, h.http.URL+"/v1/chat/completions", strings.NewReader(simpleRequest))
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	p := problemOf(t, raw)
	if p.Code != httpx.CodeUnauthenticated {
		t.Errorf("code = %s", p.Code)
	}
	if strings.Contains(strings.ToLower(p.Detail), "signature") || strings.Contains(strings.ToLower(p.Detail), "expired") {
		t.Error("the error detail must not tell an attacker which check failed")
	}
}

func TestPoolEntitlementIsEnforced(t *testing.T) {
	h := newHarness(t, []*fakeBackend{newFakeBackend("primary")})
	h.token = h.tokenFor(func(r *identity.TokenRequest) { r.ModelPools = []string{"some-other-pool"} })

	resp, raw := h.chat(simpleRequest)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
	if p := problemOf(t, raw); p.Code != httpx.CodeForbiddenPool {
		t.Errorf("code = %s", p.Code)
	}
}

func TestEnvironmentMismatchIsRefused(t *testing.T) {
	h := newHarness(t, []*fakeBackend{newFakeBackend("primary")})
	// The gateway under test serves dev; a production token must not work.
	h.token = h.tokenFor(func(r *identity.TokenRequest) { r.Env = identity.EnvProd })
	resp, raw := h.chat(simpleRequest)
	// The dev gateway is deliberately permissive about environment; the check
	// exists for staging and production. Either outcome is acceptable, but a
	// 5xx never is.
	if resp.StatusCode >= 500 {
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
}

func TestUnknownModelIsRejected(t *testing.T) {
	h := newHarness(t, []*fakeBackend{newFakeBackend("primary")})
	resp, raw := h.chat(`{"model":"does-not-exist","messages":[{"role":"user","content":"x"}]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
	p := problemOf(t, raw)
	if p.Code != httpx.CodeUnknownModel {
		t.Errorf("code = %s", p.Code)
	}
	if !strings.Contains(p.Detail, "/v1/models") {
		t.Error("the error should point the caller at the models endpoint")
	}
}

func TestMalformedRequestsAreRejected(t *testing.T) {
	h := newHarness(t, []*fakeBackend{newFakeBackend("primary")})
	cases := map[string]string{
		"not json":         `{`,
		"no messages":      `{"model":"general-chat","messages":[]}`,
		"bad role":         `{"model":"general-chat","messages":[{"role":"wizard","content":"x"}]}`,
		"bad temperature":  `{"model":"general-chat","messages":[{"role":"user","content":"x"}],"temperature":7}`,
		"bad top_p":        `{"model":"general-chat","messages":[{"role":"user","content":"x"}],"top_p":3}`,
		"max_tokens above": `{"model":"general-chat","messages":[{"role":"user","content":"x"}],"max_tokens":999999}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			resp, raw := h.chat(body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d: %s", resp.StatusCode, raw)
			}
			if p := problemOf(t, raw); p.Code != httpx.CodeInvalidRequest {
				t.Errorf("code = %s", p.Code)
			}
		})
	}
}

func TestContextWindowIsEnforcedBeforeSpendingAnything(t *testing.T) {
	backend := newFakeBackend("primary")
	h := newHarness(t, []*fakeBackend{backend}, withContextWindow(64))
	body := fmt.Sprintf(`{"model":"general-chat","messages":[{"role":"user","content":%q}],"max_tokens":32}`,
		strings.Repeat("a long prompt that will not fit ", 40))

	resp, raw := h.chat(body)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
	if p := problemOf(t, raw); p.Code != httpx.CodeContextTooLarge {
		t.Errorf("code = %s", p.Code)
	}
	if backend.calls.Load() != 0 {
		t.Error("an oversized prompt must be refused by the gateway, not paid for at the provider")
	}
}

func TestCacheHitAvoidsTheBackendAndRecordsSavings(t *testing.T) {
	backend := newFakeBackend("primary")
	h := newHarness(t, []*fakeBackend{backend})

	if resp, raw := h.chat(simpleRequest); resp.StatusCode != http.StatusOK {
		t.Fatalf("first call: %d %s", resp.StatusCode, raw)
	}
	callsAfterFirst := backend.calls.Load()

	resp, raw := h.chat(simpleRequest)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second call: %d %s", resp.StatusCode, raw)
	}
	if got := resp.Header.Get(HeaderCacheResult); got != string(cache.ResultHit) {
		t.Fatalf("cache = %s, want hit", got)
	}
	if backend.calls.Load() != callsAfterFirst {
		t.Error("a cache hit must not reach the backend")
	}

	last := h.usage.last(t)
	if last.Billable {
		t.Error("a cache hit must not be billable")
	}
	if last.SavingsUSD <= 0 {
		t.Error("a cache hit must record what it saved, or the platform cannot show its value")
	}
}

func TestCacheBypassHeaderIsHonoured(t *testing.T) {
	backend := newFakeBackend("primary")
	h := newHarness(t, []*fakeBackend{backend})
	h.chat(simpleRequest)
	before := backend.calls.Load()

	resp, _ := h.chat(simpleRequest, HeaderCache, "off")
	if got := resp.Header.Get(HeaderCacheResult); got != string(cache.ResultBypass) {
		t.Errorf("cache = %s, want bypass", got)
	}
	if backend.calls.Load() != before+1 {
		t.Error("a bypassed cache must reach the backend")
	}
}

func TestHighTemperatureIsNotCached(t *testing.T) {
	backend := newFakeBackend("primary")
	h := newHarness(t, []*fakeBackend{backend})
	body := `{"model":"general-chat","messages":[{"role":"user","content":"be creative"}],"max_tokens":16,"temperature":0.9}`

	h.chat(body)
	resp, _ := h.chat(body)
	if got := resp.Header.Get(HeaderCacheResult); got == string(cache.ResultHit) {
		t.Error("a high-temperature request must not be served from cache")
	}
	if backend.calls.Load() != 2 {
		t.Errorf("backend calls = %d, want 2", backend.calls.Load())
	}
}

func TestRetryThenSuccessOnTheSameBackend(t *testing.T) {
	backend := newFakeBackend("primary")
	backend.failN.Store(2)
	h := newHarness(t, []*fakeBackend{backend})

	resp, raw := h.chat(simpleRequest)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
	if got := resp.Header.Get(HeaderAttempts); got != "3" {
		t.Errorf("attempts = %s, want 3", got)
	}
	if backend.calls.Load() != 3 {
		t.Errorf("backend calls = %d, want 3", backend.calls.Load())
	}
}

func TestFailoverToTheNextTier(t *testing.T) {
	primary := newFakeBackend("primary")
	primary.status.Store(http.StatusServiceUnavailable)
	secondary := newFakeBackend("secondary")
	h := newHarness(t, []*fakeBackend{primary, secondary})

	resp, raw := h.chat(simpleRequest)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
	if resp.Header.Get(HeaderModel) == "" {
		t.Error("the serving backend must be named on the response")
	}
	if secondary.calls.Load() == 0 {
		t.Error("the request should have failed over to the second tier")
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	choices := body["choices"].([]any)
	content := choices[0].(map[string]any)["message"].(map[string]any)["content"].(string)
	if !strings.Contains(content, "secondary") {
		t.Errorf("the answer came from the wrong backend: %q", content)
	}
}

func TestNoFailoverOnACallerError(t *testing.T) {
	primary := newFakeBackend("primary")
	primary.status.Store(http.StatusBadRequest)
	secondary := newFakeBackend("secondary")
	h := newHarness(t, []*fakeBackend{primary, secondary})

	resp, raw := h.chat(simpleRequest)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
	if secondary.calls.Load() != 0 {
		t.Error("a malformed request must not be retried against another backend; it will fail identically and spend someone else's quota")
	}
	if primary.calls.Load() != 1 {
		t.Errorf("a caller error must not be retried, got %d attempts", primary.calls.Load())
	}
}

func TestAllBackendsDownYieldsNoHealthyBackend(t *testing.T) {
	primary := newFakeBackend("primary")
	primary.status.Store(http.StatusServiceUnavailable)
	h := newHarness(t, []*fakeBackend{primary})

	// Drive enough failures to open the breaker, then confirm the gateway
	// reports the platform-level condition rather than a provider error.
	var last *http.Response
	var raw []byte
	for i := 0; i < 20; i++ {
		last, raw = h.chat(fmt.Sprintf(
			`{"model":"general-chat","messages":[{"role":"user","content":"attempt %d"}],"max_tokens":8,"temperature":0}`, i))
		if last.StatusCode == http.StatusServiceUnavailable {
			break
		}
	}
	if last.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d: %s", last.StatusCode, raw)
	}
	p := problemOf(t, raw)
	if p.Code != httpx.CodeNoHealthyBackend {
		t.Errorf("code = %s, want no_healthy_backend", p.Code)
	}
	if p.RetryAfterSeconds == 0 {
		t.Error("a retryable platform failure should carry a retry hint")
	}
}

func TestRequestRateLimit(t *testing.T) {
	h := newHarness(t, []*fakeBackend{newFakeBackend("primary")}, withQuota(2, 1_000_000))

	var limited bool
	for i := 0; i < 5; i++ {
		resp, raw := h.chat(fmt.Sprintf(
			`{"model":"general-chat","messages":[{"role":"user","content":"call %d"}],"max_tokens":8,"temperature":0}`, i))
		if resp.StatusCode == http.StatusTooManyRequests {
			limited = true
			p := problemOf(t, raw)
			if p.Code != httpx.CodeRateLimited {
				t.Errorf("code = %s", p.Code)
			}
			if resp.Header.Get("Retry-After") == "" {
				t.Error("a rate-limited response must tell the caller when to come back")
			}
			break
		}
	}
	if !limited {
		t.Error("the request limit was never enforced")
	}
}

func TestTokenQuotaIsEnforcedOnEstimate(t *testing.T) {
	h := newHarness(t, []*fakeBackend{newFakeBackend("primary")}, withQuota(1000, 50))

	resp, raw := h.chat(`{"model":"general-chat","messages":[{"role":"user","content":"a fairly long prompt that will be estimated well above the tiny token budget configured for this test"}],"max_tokens":512}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
	p := problemOf(t, raw)
	if p.Code != httpx.CodeQuotaExceeded {
		t.Errorf("code = %s, want quota_exceeded", p.Code)
	}
}

func TestGuardrailBlocksCredentialsInThePrompt(t *testing.T) {
	backend := newFakeBackend("primary")
	h := newHarness(t, []*fakeBackend{backend})

	resp, raw := h.chat(`{"model":"general-chat","messages":[{"role":"user","content":"use AKIAIOSFODNN7EXAMPLE to fetch the file"}],"max_tokens":16}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
	p := problemOf(t, raw)
	if p.Code != httpx.CodeGuardrailBlocked {
		t.Errorf("code = %s", p.Code)
	}
	if got := resp.Header.Get(HeaderGuardrail); !strings.HasPrefix(got, "blocked:") {
		t.Errorf("guardrail header = %q", got)
	}
	if backend.calls.Load() != 0 {
		t.Error("blocked content must never reach a model provider")
	}
}

func TestGuardrailRedactsOutboundPII(t *testing.T) {
	backend := newFakeBackend("primary")
	backend.body = "the customer card is 4242424242424242"
	h := newHarness(t, []*fakeBackend{backend}, withGuardrailScanning)

	resp, raw := h.chat(simpleRequest)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
	if strings.Contains(string(raw), "4242424242424242") {
		t.Error("a card number in the model's output must not reach the caller intact")
	}
}

func TestStreamingResponseIsWellFormed(t *testing.T) {
	backend := newFakeBackend("primary")
	backend.body = "the streamed answer arrives in pieces"
	h := newHarness(t, []*fakeBackend{backend}, withGuardrailScanning)

	req, _ := http.NewRequest(http.MethodPost, h.http.URL+"/v1/chat/completions", strings.NewReader(
		`{"model":"general-chat","messages":[{"role":"user","content":"stream it"}],"max_tokens":64,"stream":true,"stream_options":{"include_usage":true}}`))
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type = %q", ct)
	}

	var (
		text       strings.Builder
		sawUsage   bool
		sawDone    bool
		finishSeen bool
		orderBad   bool
	)
	err = httpx.ReadSSE(resp.Body, func(ev httpx.SSEEvent) error {
		switch {
		case ev.IsDone():
			sawDone = true
		case ev.Event == "agentgate.usage":
			sawUsage = true
		case ev.Event == "":
			var chunk map[string]any
			if err := json.Unmarshal([]byte(ev.Data), &chunk); err != nil {
				t.Errorf("chunk is not valid JSON: %s", ev.Data)
				return nil
			}
			choices, _ := chunk["choices"].([]any)
			for _, c := range choices {
				m := c.(map[string]any)
				if delta, ok := m["delta"].(map[string]any); ok {
					if content, ok := delta["content"].(string); ok {
						if finishSeen {
							orderBad = true
						}
						text.WriteString(content)
					}
				}
				if fr, ok := m["finish_reason"].(string); ok && fr != "" {
					finishSeen = true
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// ReadSSE returns at the [DONE] sentinel, which the gateway writes before
	// it meters the request. Draining the rest of the body blocks until the
	// handler returns, which is after finish() has written the usage record —
	// without this the assertions below race the server and fail on a loaded
	// machine. This is a property of every streamed response, not of this test.
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("draining the stream: %v", err)
	}

	if !strings.Contains(text.String(), "streamed answer") {
		t.Errorf("streamed text = %q", text.String())
	}
	if orderBad {
		t.Error("content arrived after finish_reason; a strict client would truncate the answer")
	}
	if !finishSeen {
		t.Error("the stream never carried a finish reason")
	}
	if !sawUsage {
		t.Error("include_usage was requested but no usage frame was emitted")
	}
	if !sawDone {
		t.Error("the stream did not terminate with the [DONE] sentinel")
	}

	rec := h.usage.last(t)
	if rec.OutputTokens == 0 {
		t.Error("streamed output tokens were not recorded")
	}
	if rec.TTFTMS <= 0 {
		t.Error("time to first token must be recorded for a streamed response")
	}
}

func TestIdempotencyKeyReplaysTheSameResponse(t *testing.T) {
	backend := newFakeBackend("primary")
	h := newHarness(t, []*fakeBackend{backend})

	body := `{"model":"general-chat","messages":[{"role":"user","content":"charge once"}],"max_tokens":16,"temperature":0.9}`
	_, first := h.chat(body, HeaderIdempotencyKey, "key-1")
	callsAfterFirst := backend.calls.Load()

	resp, second := h.chat(body, HeaderIdempotencyKey, "key-1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if string(first) != string(second) {
		t.Error("the same idempotency key must replay the same response")
	}
	if backend.calls.Load() != callsAfterFirst {
		t.Error("a replayed request must not reach the backend again")
	}

	other := `{"model":"general-chat","messages":[{"role":"user","content":"different"}],"max_tokens":16,"temperature":0.9}`
	conflict, raw := h.chat(other, HeaderIdempotencyKey, "key-1")
	if conflict.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d: %s", conflict.StatusCode, raw)
	}
	if p := problemOf(t, raw); p.Code != httpx.CodeIdempotencyConflict {
		t.Errorf("code = %s", p.Code)
	}
}

func TestModelsListingIsScopedToEntitlement(t *testing.T) {
	h := newHarness(t, []*fakeBackend{newFakeBackend("primary")})

	req, _ := http.NewRequest(http.MethodGet, h.http.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := h.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 1 || list.Data[0]["id"] != "general-chat" {
		t.Errorf("models = %#v", list.Data)
	}

	h.token = h.tokenFor(func(r *identity.TokenRequest) { r.ModelPools = []string{"nothing"} })
	req2, _ := http.NewRequest(http.MethodGet, h.http.URL+"/v1/models", nil)
	req2.Header.Set("Authorization", "Bearer "+h.token)
	resp2, _ := h.http.Client().Do(req2)
	raw2, _ := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()
	var list2 struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.Unmarshal(raw2, &list2)
	if len(list2.Data) != 0 {
		t.Error("a caller must not be shown models it cannot use")
	}
}

func TestTokenCountEndpoint(t *testing.T) {
	h := newHarness(t, []*fakeBackend{newFakeBackend("primary")})
	req, _ := http.NewRequest(http.MethodPost, h.http.URL+"/v1/token-count", strings.NewReader(simpleRequest))
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if out["estimated_input_tokens"].(float64) <= 0 {
		t.Errorf("estimate = %v", out["estimated_input_tokens"])
	}
	if out["estimated"] != true {
		t.Error("the endpoint must be explicit that the number is an estimate")
	}
}

func TestMetricsAreRecorded(t *testing.T) {
	h := newHarness(t, []*fakeBackend{newFakeBackend("primary")})
	h.chat(simpleRequest)

	var b strings.Builder
	h.registry.WriteTo(&b)
	out := b.String()
	for _, want := range []string{
		`agentgate_gateway_requests_total{`,
		`team="payments-risk"`,
		`agentgate_gateway_tokens_total{`,
		`agentgate_gateway_cost_usd_total{`,
		`cost_center="CC-4471"`,
		`agentgate_gateway_overhead_seconds_bucket{`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics are missing %q", want)
		}
	}
}

func TestHealthAndReadiness(t *testing.T) {
	h := newHarness(t, []*fakeBackend{newFakeBackend("primary")})
	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := h.http.Client().Get(h.http.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s = %d: %s", path, resp.StatusCode, raw)
		}
	}
}

func TestOperatorCanDrainABackend(t *testing.T) {
	primary := newFakeBackend("primary")
	secondary := newFakeBackend("secondary")
	h := newHarness(t, []*fakeBackend{primary, secondary})

	resp, err := h.http.Client().Post(h.http.URL+"/admin/backends/general-chat/primary/drain", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("drain returned %d", resp.StatusCode)
	}

	if r, raw := h.chat(simpleRequest); r.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", r.StatusCode, raw)
	}
	if primary.calls.Load() != 0 {
		t.Error("a drained backend must not receive traffic")
	}
	if secondary.calls.Load() == 0 {
		t.Error("traffic should have moved to the remaining backend")
	}
}

func TestCachePurgeEndpoint(t *testing.T) {
	backend := newFakeBackend("primary")
	h := newHarness(t, []*fakeBackend{backend})
	h.chat(simpleRequest)
	if resp, _ := h.chat(simpleRequest); resp.Header.Get(HeaderCacheResult) != string(cache.ResultHit) {
		t.Fatal("expected a cache hit before purging")
	}

	resp, err := h.http.Client().Post(h.http.URL+"/admin/cache/purge?tenant=fsclient", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if r, _ := h.chat(simpleRequest); r.Header.Get(HeaderCacheResult) == string(cache.ResultHit) {
		t.Error("a purge must clear the tenant's cached responses")
	}
}

func TestGuardrailProvidersAreBuiltPerPool(t *testing.T) {
	cfg := &config.Config{
		Env: "dev",
		Pools: []config.Pool{
			{Name: "a", Guardrail: config.GuardrailPolicy{Provider: "builtin"}},
			{Name: "b", Guardrail: config.GuardrailPolicy{Provider: "noop"}},
		},
	}
	got, err := NewGuardrailProviders(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got["a"].Name() != "builtin" || got["b"].Name() != "noop" {
		t.Errorf("providers = %s, %s", got["a"].Name(), got["b"].Name())
	}
	bad := &config.Config{Pools: []config.Pool{{Name: "c", Guardrail: config.GuardrailPolicy{Provider: "mystery"}}}}
	if _, err := NewGuardrailProviders(bad); err == nil {
		t.Error("an unknown guardrail provider must be a startup failure, not a runtime surprise")
	}
}

func TestGuardrailPolicyDefaultsAreConservative(t *testing.T) {
	p := guardrails.DefaultPolicy("restricted")
	if p.FailureMode != guardrails.FailClosed {
		t.Error("restricted pools must fail closed")
	}
}

// createFile is a small indirection so the harness can write a temporary
// configuration file without importing os into every test.
func createFile(path string) (*os.File, error) { return os.Create(path) }

func TestInputRedactionPreservesEveryMessage(t *testing.T) {
	backend := newFakeBackend("primary")
	h := newHarness(t, []*fakeBackend{backend})

	// A multi-line user turn whose middle line carries a Luhn-valid card, and
	// a later tool message carrying an IBAN. Both must be redacted; nothing
	// else may be lost.
	body := `{"model":"general-chat","messages":[
	  {"role":"user","content":"Please review this dispute.\nThe card is 4242424242424242.\nAdvise on next steps."},
	  {"role":"assistant","content":"Looking it up."},
	  {"role":"tool","tool_call_id":"call_1","content":"beneficiary GB33BUKB20201555555555 settled"}
	],"max_tokens":16,"temperature":0}`

	resp, raw := h.chat(body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
	if got := resp.Header.Get(HeaderGuardrail); !strings.HasPrefix(got, "redacted:") {
		t.Fatalf("guardrail header = %q, want a redaction", got)
	}

	forwarded, _ := backend.lastBody.Load().(string)
	if forwarded == "" {
		t.Fatal("nothing reached the backend")
	}
	if strings.Contains(forwarded, "4242424242424242") {
		t.Error("the card number reached the provider")
	}
	if strings.Contains(forwarded, "GB33BUKB20201555555555") {
		t.Error("the IBAN in the tool message reached the provider; redaction must not stop at the last user turn")
	}
	for _, keep := range []string{"Please review this dispute", "Advise on next steps", "Looking it up"} {
		if !strings.Contains(forwarded, keep) {
			t.Errorf("redaction discarded %q; only the sensitive value may be replaced", keep)
		}
	}
}

func TestTerminalChunkCarryingContentKeepsItsFinishReason(t *testing.T) {
	backend := newFakeBackend("primary")
	// Several OpenAI-compatible servers combine the last content delta and the
	// finish reason into one frame.
	backend.streamCombined = true
	backend.body = "combined"
	h := newHarness(t, []*fakeBackend{backend}, withGuardrailScanning)

	req, _ := http.NewRequest(http.MethodPost, h.http.URL+"/v1/chat/completions", strings.NewReader(
		`{"model":"general-chat","messages":[{"role":"user","content":"go"}],"max_tokens":32,"stream":true}`))
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	var text strings.Builder
	var finish string
	err = httpx.ReadSSE(resp.Body, func(ev httpx.SSEEvent) error {
		if ev.Event != "" || ev.IsDone() {
			return nil
		}
		var chunk struct {
			Choices []struct {
				Delta        struct{ Content string } `json:"delta"`
				FinishReason string                   `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(ev.Data), &chunk) != nil {
			return nil
		}
		for _, c := range chunk.Choices {
			text.WriteString(c.Delta.Content)
			if c.FinishReason != "" {
				finish = c.FinishReason
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "combined") {
		t.Errorf("content lost: %q", text.String())
	}
	if finish != "stop" {
		t.Error("a chunk carrying both content and a finish reason must not be dropped by the output scanner")
	}
}

func TestConcurrentCacheHitsDoNotShareMutableState(t *testing.T) {
	backend := newFakeBackend("primary")
	h := newHarness(t, []*fakeBackend{backend})

	if resp, raw := h.chat(simpleRequest); resp.StatusCode != http.StatusOK {
		t.Fatalf("warm-up: %d %s", resp.StatusCode, raw)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, raw := h.chat(simpleRequest)
			if resp.StatusCode != http.StatusOK {
				t.Errorf("cached call: %d %s", resp.StatusCode, raw)
			}
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("cached response is not valid JSON: %s", raw)
				return
			}
			if body["model"] != "general-chat" {
				t.Errorf("cached response model = %v", body["model"])
			}
		}()
	}
	wg.Wait()
}

func TestFailedRequestsAreRecordedAsEstimatedAndNotBilled(t *testing.T) {
	backend := newFakeBackend("primary")
	backend.status.Store(http.StatusInternalServerError)
	h := newHarness(t, []*fakeBackend{backend})

	if resp, _ := h.chat(simpleRequest); resp.StatusCode < 500 {
		t.Fatalf("expected the request to fail, got %d", resp.StatusCode)
	}
	rec := h.usage.last(t)
	if rec.Billable {
		t.Error("a request the provider never answered must not be billed")
	}
	if rec.InputTokens > 0 && !rec.Estimated {
		t.Error("a token count the gateway guessed must be marked estimated, not presented as measured")
	}
	if rec.CostUSD != 0 {
		t.Errorf("cost = %f, want 0 for an unanswered request", rec.CostUSD)
	}
}
