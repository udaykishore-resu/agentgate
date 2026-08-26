package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimal = `
env: dev
models:
  - name: general-chat
    pool: general-chat
    context_window: 128000
pools:
  - name: general-chat
    strategy: weighted
    guardrail:
      provider: builtin
      failure_mode: fail_open
    backends:
      - name: b1
        kind: openai
        base_url: "https://backend.invalid/v1"
        model: m
        input_cost_per_1m: 1
        output_cost_per_1m: 2
`

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := Load(write(t, "gateway.yaml", minimal))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Service.HTTPAddr != ":8080" || cfg.Service.MetricsAddr != ":9090" {
		t.Errorf("addresses = %s %s", cfg.Service.HTTPAddr, cfg.Service.MetricsAddr)
	}
	if cfg.Gateway.Retry.MaxAttempts != 3 || cfg.Gateway.Breaker.WindowSize != 50 {
		t.Errorf("resilience defaults not applied: %#v", cfg.Gateway)
	}
	if cfg.Gateway.RateLimiter != "memory" {
		t.Errorf("rate limiter = %q; with no Redis configured it should default to memory", cfg.Gateway.RateLimiter)
	}
	b := cfg.Pools[0].Backends[0]
	if b.Weight != 100 || b.Priority != 1 || b.Timeout.D() != 60*time.Second {
		t.Errorf("backend defaults not applied: %#v", b)
	}
	if b.MaxClassification != "confidential" {
		t.Errorf("a backend with no declared classification must default conservatively, got %q", b.MaxClassification)
	}
	if v := cfg.Validate(); v != nil && v.HasErrors() {
		t.Errorf("minimal dev config should be valid: %v", v)
	}
}

func TestEnvironmentInterpolation(t *testing.T) {
	t.Setenv("AGENTGATE_TEST_KEY", "from-environment")
	cfg, err := Load(write(t, "gateway.yaml", minimal+`
redis:
  addr: "${AGENTGATE_TEST_KEY}"
telemetry:
  otlp_endpoint: "${AGENTGATE_UNSET_VAR:-http://fallback:4318}"
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Redis.Addr != "from-environment" {
		t.Errorf("redis addr = %q", cfg.Redis.Addr)
	}
	if cfg.Telemetry.OTLPEndpoint != "http://fallback:4318" {
		t.Errorf("otlp endpoint = %q; the :- default was not applied", cfg.Telemetry.OTLPEndpoint)
	}
}

func TestDurationAcceptsStringsAndSeconds(t *testing.T) {
	cfg, err := Load(write(t, "gateway.yaml", minimal+`
gateway:
  request_timeout: 45s
  stream_heartbeat: 20
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Gateway.RequestTimeout.D() != 45*time.Second {
		t.Errorf("request timeout = %s", cfg.Gateway.RequestTimeout.D())
	}
	if cfg.Gateway.StreamHeartbeat.D() != 20*time.Second {
		t.Errorf("a bare number must be read as seconds, got %s", cfg.Gateway.StreamHeartbeat.D())
	}
}

func TestValidationCatchesStructuralMistakes(t *testing.T) {
	cases := map[string]string{
		"model points at a missing pool": `
env: dev
models:
  - {name: m, pool: nowhere}
pools:
  - name: p
    strategy: weighted
    guardrail: {provider: builtin, failure_mode: fail_open}
    backends:
      - {name: b, kind: openai, base_url: "https://x.invalid", model: m, input_cost_per_1m: 1, output_cost_per_1m: 1}
`,
		"pool with no backends": `
env: dev
models:
  - {name: m, pool: p}
pools:
  - name: p
    strategy: weighted
    guardrail: {provider: builtin, failure_mode: fail_open}
    backends: []
`,
		"unknown provider kind": `
env: dev
models:
  - {name: m, pool: p}
pools:
  - name: p
    strategy: weighted
    guardrail: {provider: builtin, failure_mode: fail_open}
    backends:
      - {name: b, kind: telepathy, base_url: "https://x.invalid", model: m}
`,
		"unknown strategy": `
env: dev
models:
  - {name: m, pool: p}
pools:
  - name: p
    strategy: vibes
    guardrail: {provider: builtin, failure_mode: fail_open}
    backends:
      - {name: b, kind: openai, base_url: "https://x.invalid", model: m}
`,
		"callout without a url": `
env: dev
models:
  - {name: m, pool: p}
pools:
  - name: p
    strategy: weighted
    guardrail: {provider: callout, failure_mode: fail_open}
    backends:
      - {name: b, kind: openai, base_url: "https://x.invalid", model: m}
`,
		"semantic cache without an embedding model": `
env: dev
models:
  - {name: m, pool: p}
pools:
  - name: p
    strategy: weighted
    cache: {enabled: true, semantic: true}
    guardrail: {provider: builtin, failure_mode: fail_open}
    backends:
      - {name: b, kind: openai, base_url: "https://x.invalid", model: m}
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, err := Load(write(t, "gateway.yaml", body))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			v := cfg.Validate()
			if v == nil || !v.HasErrors() {
				t.Fatalf("%s should have been rejected", name)
			}
		})
	}
}

func TestProductionRefusesDevelopmentShortcuts(t *testing.T) {
	body := strings.Replace(minimal, "env: dev", "env: prod", 1) + `
identity:
  allow_unverified: true
`
	cfg, err := Load(write(t, "gateway.yaml", body))
	if err != nil {
		t.Fatal(err)
	}
	v := cfg.Validate()
	if v == nil || !v.HasErrors() {
		t.Fatal("a production configuration with verification disabled must be refused")
	}
	msg := v.Error()
	for _, want := range []string{"allow_unverified", "rate_limiter", "otlp_endpoint"} {
		if !strings.Contains(msg, want) {
			t.Errorf("production validation did not flag %s:\n%s", want, msg)
		}
	}
}

func TestProductionRefusesPlaintextBackends(t *testing.T) {
	body := strings.Replace(minimal, "env: dev", "env: prod", 1)
	body = strings.Replace(body, `"https://backend.invalid/v1"`, `"http://backend.invalid/v1"`, 1)
	cfg, err := Load(write(t, "gateway.yaml", body))
	if err != nil {
		t.Fatal(err)
	}
	v := cfg.Validate()
	if v == nil || !strings.Contains(v.Error(), "plaintext HTTP") {
		t.Error("plaintext backend traffic must be refused in production")
	}
}

func TestWarningsAreSeparateFromErrors(t *testing.T) {
	body := strings.Replace(minimal, `        input_cost_per_1m: 1
        output_cost_per_1m: 2
`, "", 1)
	cfg, err := Load(write(t, "gateway.yaml", body))
	if err != nil {
		t.Fatal(err)
	}
	v := cfg.Validate()
	if v == nil {
		t.Fatal("expected a warning about the unpriced backend")
	}
	if v.HasErrors() {
		t.Errorf("an unpriced backend is a warning, not a startup failure: %v", v)
	}
	found := false
	for _, w := range v.Warnings() {
		if strings.Contains(w.Message, "unpriced") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v", v.Warnings())
	}
}

func TestJSONAndYAMLProduceTheSameConfig(t *testing.T) {
	fromYAML, err := Load(write(t, "gateway.yaml", minimal))
	if err != nil {
		t.Fatal(err)
	}
	fromJSON, err := Load(write(t, "gateway.json", `{
  "env": "dev",
  "models": [{"name": "general-chat", "pool": "general-chat", "context_window": 128000}],
  "pools": [{
    "name": "general-chat", "strategy": "weighted",
    "guardrail": {"provider": "builtin", "failure_mode": "fail_open"},
    "backends": [{"name": "b1", "kind": "openai", "base_url": "https://backend.invalid/v1",
                  "model": "m", "input_cost_per_1m": 1, "output_cost_per_1m": 2}]
  }]
}`))
	if err != nil {
		t.Fatal(err)
	}
	if fromYAML.Pools[0].Backends[0].BaseURL != fromJSON.Pools[0].Backends[0].BaseURL {
		t.Error("the two supported formats must produce identical configuration")
	}
	if fromYAML.Models[0].ContextWindow != fromJSON.Models[0].ContextWindow {
		t.Error("context window differs between formats")
	}
}
