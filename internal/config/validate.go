package config

import (
	"fmt"
	"strings"
)

// Problem is one validation finding.
type Problem struct {
	Path     string
	Message  string
	Severity string // "error" or "warning"
}

// Error renders the finding.
func (p Problem) Error() string { return fmt.Sprintf("%s: %s", p.Path, p.Message) }

// ValidationError collects findings.
type ValidationError struct{ Problems []Problem }

// Error renders every error-severity finding.
func (v *ValidationError) Error() string {
	var b strings.Builder
	b.WriteString("configuration is invalid:")
	for _, p := range v.Problems {
		if p.Severity == "error" {
			b.WriteString("\n  - " + p.Error())
		}
	}
	return b.String()
}

// HasErrors reports whether any finding is fatal.
func (v *ValidationError) HasErrors() bool {
	for _, p := range v.Problems {
		if p.Severity == "error" {
			return true
		}
	}
	return false
}

// Warnings returns the non-fatal findings, which the service logs at start.
func (v *ValidationError) Warnings() []Problem {
	var out []Problem
	for _, p := range v.Problems {
		if p.Severity == "warning" {
			out = append(out, p)
		}
	}
	return out
}

// Validate checks a gateway configuration. It returns a *ValidationError
// whenever there is anything to report, fatal or not; the caller checks
// HasErrors to decide whether to start.
func (c *Config) Validate() *ValidationError {
	v := &ValidationError{}
	errf := func(path, format string, args ...any) {
		v.Problems = append(v.Problems, Problem{Path: path, Message: fmt.Sprintf(format, args...), Severity: "error"})
	}
	warnf := func(path, format string, args ...any) {
		v.Problems = append(v.Problems, Problem{Path: path, Message: fmt.Sprintf(format, args...), Severity: "warning"})
	}
	prod := c.Env == "prod"

	switch c.Env {
	case "dev", "staging", "prod":
	default:
		errf("env", "must be one of dev, staging, prod; got %q", c.Env)
	}

	if len(c.Pools) == 0 {
		errf("pools", "at least one pool is required")
	}
	if len(c.Models) == 0 {
		errf("models", "at least one logical model is required")
	}

	poolNames := map[string]bool{}
	for i, p := range c.Pools {
		path := fmt.Sprintf("pools[%d]", i)
		if p.Name == "" {
			errf(path+".name", "is required")
			continue
		}
		if poolNames[p.Name] {
			errf(path+".name", "duplicate pool name %q", p.Name)
		}
		poolNames[p.Name] = true

		switch p.Strategy {
		case "weighted", "weighted_least_loaded", "least_loaded", "priority", "round_robin":
		default:
			errf(path+".strategy", "unknown strategy %q", p.Strategy)
		}

		enabled := 0
		names := map[string]bool{}
		for j, b := range p.Backends {
			bp := fmt.Sprintf("%s.backends[%d]", path, j)
			if b.Name == "" {
				errf(bp+".name", "is required")
			}
			if names[b.Name] {
				errf(bp+".name", "duplicate backend name %q in pool %s", b.Name, p.Name)
			}
			names[b.Name] = true
			switch b.Kind {
			case "openai", "azure-openai", "onprem-vllm", "bedrock", "anthropic":
			default:
				errf(bp+".kind", "unknown provider kind %q", b.Kind)
			}
			if b.Model == "" {
				errf(bp+".model", "is required")
			}
			if b.Kind != "bedrock" && b.BaseURL == "" {
				errf(bp+".base_url", "is required for kind %q", b.Kind)
			}
			if b.Kind == "bedrock" && b.Region == "" {
				errf(bp+".region", "is required for bedrock")
			}
			switch b.MaxClassification {
			case "public", "internal", "confidential", "restricted", "":
			default:
				errf(bp+".max_classification", "unknown classification %q", b.MaxClassification)
			}
			if b.InputCostPer1M == 0 && b.OutputCostPer1M == 0 {
				// Not fatal: an unpriced backend still serves traffic, but its
				// usage cannot be charged back, and someone will ask why.
				warnf(bp, "no unit cost configured; usage from %s will be recorded as unpriced", b.Name)
			}
			if prod && strings.HasPrefix(b.BaseURL, "http://") {
				errf(bp+".base_url", "plaintext HTTP is not permitted in production")
			}
			if b.IsEnabled() {
				enabled++
			}
		}
		if enabled == 0 {
			errf(path+".backends", "pool %q has no enabled backends", p.Name)
		}

		switch p.Guardrail.Provider {
		case "noop", "builtin", "callout":
		default:
			errf(path+".guardrail.provider", "unknown provider %q", p.Guardrail.Provider)
		}
		if p.Guardrail.Provider == "callout" && p.Guardrail.URL == "" {
			errf(path+".guardrail.url", "is required when provider is callout")
		}
		switch p.Guardrail.FailureMode {
		case "fail_open", "fail_closed":
		default:
			errf(path+".guardrail.failure_mode", "must be fail_open or fail_closed")
		}
		if p.Guardrail.Enabled && p.Guardrail.FailureMode == "fail_open" && poolCarriesRestricted(&c.Pools[i]) {
			msg := fmt.Sprintf("pool %q is cleared for restricted data but fails open; a content-filter outage would send restricted content to a model unscanned", p.Name)
			if prod {
				errf(path+".guardrail.failure_mode", "%s", msg)
			} else {
				warnf(path+".guardrail.failure_mode", "%s", msg)
			}
		}
		if prod && p.Guardrail.Provider == "noop" {
			warnf(path+".guardrail", "pool %q runs with no content safety policy in production", p.Name)
		}
		if p.Cache.Semantic && p.Cache.SemanticEmbeddingModel == "" {
			errf(path+".cache.semantic_embedding_model", "is required when semantic caching is enabled")
		}
		if p.Cache.Semantic && p.Cache.SemanticThreshold < 0.9 {
			warnf(path+".cache.semantic_threshold",
				"a threshold of %.2f will serve materially different questions from cache", p.Cache.SemanticThreshold)
		}
	}

	modelNames := map[string]bool{}
	for i, m := range c.Models {
		path := fmt.Sprintf("models[%d]", i)
		if m.Name == "" {
			errf(path+".name", "is required")
			continue
		}
		if modelNames[m.Name] {
			errf(path+".name", "duplicate logical model %q", m.Name)
		}
		modelNames[m.Name] = true
		if !poolNames[m.Pool] {
			errf(path+".pool", "model %q references unknown pool %q", m.Name, m.Pool)
		}
		if m.ContextWindow <= 0 {
			warnf(path+".context_window", "no context window declared for %q; oversized prompts will be refused by the backend instead of the gateway", m.Name)
		}
	}

	// Environment-aware refusals. Each of these is a development convenience
	// whose presence in production would be a control failure.
	if prod {
		if c.Identity.AllowUnverified {
			errf("identity.allow_unverified", "token verification cannot be disabled in production")
		}
		if c.Identity.JWKSURL == "" && c.Identity.StaticKeysPath == "" {
			errf("identity.jwks_url", "a key source is required")
		}
		if c.Gateway.RateLimiter != "redis" {
			errf("gateway.rate_limiter", "production requires the distributed rate limiter; %q enforces limits per replica only", c.Gateway.RateLimiter)
		}
		if c.Redis.Addr == "" {
			errf("redis.addr", "is required in production")
		}
		if c.Telemetry.OTLPEndpoint == "" {
			errf("telemetry.otlp_endpoint", "is required in production; an unobservable gateway cannot meet its SLOs")
		}
		if c.Telemetry.ContentCapture == "full" {
			errf("telemetry.content_capture", "full prompt capture is not permitted in production")
		}
		if !c.Chargeback.Enabled {
			warnf("chargeback.enabled", "usage records are disabled; consumption cannot be attributed to teams")
		}
		if c.Telemetry.SampleRatio < 1 {
			warnf("telemetry.sample_ratio",
				"head sampling at %.2f discards traces before the collector can tail-sample them", c.Telemetry.SampleRatio)
		}
	}
	if c.Identity.AllowUnverified {
		warnf("identity.allow_unverified", "token verification is disabled; every caller is trusted")
	}

	if len(v.Problems) == 0 {
		return nil
	}
	return v
}
