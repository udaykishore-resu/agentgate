// Package config loads and validates AgentGate configuration.
//
// Configuration is one file per service, YAML or JSON, with environment
// variable interpolation. Validation is strict and runs at start: a gateway
// that boots with a pool pointing at a backend that does not exist has simply
// deferred an outage to the first request. Several checks are also
// environment-aware — an in-memory rate limiter or a file-backed secret store
// is fine in development and refused in production — because the failure mode
// of a development shortcut reaching production is exactly the failure mode
// this platform exists to prevent.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/agentgate/agentgate/internal/yamlite"
)

// Duration accepts both "30s" and a bare number of seconds in configuration.
type Duration time.Duration

// UnmarshalJSON parses a duration string or a number of seconds.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", s, err)
		}
		*d = Duration(v)
		return nil
	}
	var n float64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("invalid duration %s", string(b))
	}
	*d = Duration(time.Duration(n * float64(time.Second)))
	return nil
}

// MarshalJSON renders the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// D returns the duration as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Or returns the duration, or def when unset.
func (d Duration) Or(def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return time.Duration(d)
}

// Service is the common server configuration every binary shares.
type Service struct {
	Name            string   `json:"name"`
	HTTPAddr        string   `json:"http_addr"`
	MetricsAddr     string   `json:"metrics_addr"`
	ShutdownTimeout Duration `json:"shutdown_timeout"`
	ReadTimeout     Duration `json:"read_timeout"`
	WriteTimeout    Duration `json:"write_timeout"`
	IdleTimeout     Duration `json:"idle_timeout"`
	LogLevel        string   `json:"log_level"`
}

// Telemetry configures the tracing pipeline.
type Telemetry struct {
	OTLPEndpoint   string            `json:"otlp_endpoint"`
	OTLPHeaders    map[string]string `json:"otlp_headers,omitempty"`
	OTLPInsecure   bool              `json:"otlp_insecure"`
	OTLPTimeout    Duration          `json:"otlp_timeout"`
	SampleRatio    float64           `json:"sample_ratio"`
	BatchSize      int               `json:"batch_size"`
	BatchTimeout   Duration          `json:"batch_timeout"`
	QueueSize      int               `json:"queue_size"`
	ContentCapture string            `json:"content_capture"`
	Namespace      string            `json:"namespace"`
}

// Identity configures token verification at the gateway.
type Identity struct {
	Issuer         string   `json:"issuer"`
	Audience       string   `json:"audience"`
	JWKSURL        string   `json:"jwks_url"`
	JWKSRefresh    Duration `json:"jwks_refresh"`
	JWKSMaxStale   Duration `json:"jwks_max_stale"`
	ClockSkew      Duration `json:"clock_skew"`
	StaticKeysPath string   `json:"static_keys_path,omitempty"`
	// AllowUnverified disables token verification entirely. It exists for a
	// local demo and is refused outside dev by validation.
	AllowUnverified bool `json:"allow_unverified"`
}

// Redis configures the shared state store.
type Redis struct {
	Addr        string   `json:"addr"`
	Password    string   `json:"password,omitempty"`
	DB          int      `json:"db"`
	DialTimeout Duration `json:"dial_timeout"`
	ReadTimeout Duration `json:"read_timeout"`
	PoolSize    int      `json:"pool_size"`
	TLS         bool     `json:"tls"`
}

// Retry configures per-attempt retry behaviour.
type Retry struct {
	MaxAttempts int      `json:"max_attempts"`
	BaseDelay   Duration `json:"base_delay"`
	MaxDelay    Duration `json:"max_delay"`
	BudgetRatio float64  `json:"budget_ratio"`
	// FleetBudgetRatio caps retries as a fraction of primary requests across
	// the whole gateway.
	FleetBudgetRatio float64  `json:"fleet_budget_ratio"`
	FleetWindow      Duration `json:"fleet_window"`
}

// Breaker configures circuit breaking.
type Breaker struct {
	WindowSize          int      `json:"window_size"`
	FailureRatio        float64  `json:"failure_ratio"`
	ConsecutiveFailures int      `json:"consecutive_failures"`
	MinimumRequests     int      `json:"minimum_requests"`
	OpenDuration        Duration `json:"open_duration"`
	HalfOpenProbes      int      `json:"half_open_probes"`
}

// CachePolicy configures response caching for a pool.
type CachePolicy struct {
	Enabled            bool     `json:"enabled"`
	TTL                Duration `json:"ttl"`
	MaxTemperature     float64  `json:"max_temperature"`
	AllowTools         bool     `json:"allow_tools"`
	Semantic           bool     `json:"semantic"`
	SemanticThreshold  float64  `json:"semantic_threshold"`
	SemanticCandidates int      `json:"semantic_candidates"`
	// SemanticEmbeddingModel names the logical model used to embed a request
	// for semantic matching.
	SemanticEmbeddingModel string `json:"semantic_embedding_model,omitempty"`
	MaxBytes               int64  `json:"max_bytes"`
}

// GuardrailCategory is one category's action and threshold.
type GuardrailCategory struct {
	Action    string  `json:"action"`
	Threshold float64 `json:"threshold"`
}

// GuardrailPolicy configures content safety for a pool.
type GuardrailPolicy struct {
	Enabled            bool                         `json:"enabled"`
	Provider           string                       `json:"provider"` // noop | builtin | callout
	URL                string                       `json:"url,omitempty"`
	Headers            map[string]string            `json:"headers,omitempty"`
	FailureMode        string                       `json:"failure_mode"` // fail_open | fail_closed
	Timeout            Duration                     `json:"timeout"`
	OutputWindowTokens int                          `json:"output_window_tokens"`
	ScanOutput         bool                         `json:"scan_output"`
	Categories         map[string]GuardrailCategory `json:"categories,omitempty"`
}

// Backend is one concrete model endpoint inside a pool.
type Backend struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	BaseURL    string `json:"base_url"`
	Model      string `json:"model"`
	Deployment string `json:"deployment,omitempty"`
	APIVersion string `json:"api_version,omitempty"`
	APIKey     string `json:"api_key,omitempty"`
	Region     string `json:"region,omitempty"`

	Weight        int      `json:"weight"`
	Priority      int      `json:"priority"`
	Timeout       Duration `json:"timeout"`
	MaxConcurrent int      `json:"max_concurrent"`
	Capabilities  []string `json:"capabilities,omitempty"`
	// MaxClassification is the most sensitive data this backend may receive.
	// A restricted-data request is never routed to a backend below its level,
	// which is how residency and third-party rules are enforced in code rather
	// than in a wiki page.
	MaxClassification string `json:"max_classification,omitempty"`
	Residency         string `json:"residency,omitempty"`
	Enabled           *bool  `json:"enabled,omitempty"`

	InputCostPer1M  float64 `json:"input_cost_per_1m"`
	OutputCostPer1M float64 `json:"output_cost_per_1m"`
	CachedCostPer1M float64 `json:"cached_cost_per_1m,omitempty"`
	CostSource      string  `json:"cost_source,omitempty"`

	// TLS and egress controls for enterprise network paths.
	CABundlePath   string `json:"ca_bundle_path,omitempty"`
	ClientCertPath string `json:"client_cert_path,omitempty"`
	ClientKeyPath  string `json:"client_key_path,omitempty"`
	Proxy          string `json:"proxy,omitempty"`

	ExtraHeaders map[string]string `json:"extra_headers,omitempty"`
}

// IsEnabled reports whether the backend should be built.
func (b Backend) IsEnabled() bool { return b.Enabled == nil || *b.Enabled }

// Pool is a set of interchangeable backends behind one logical model.
type Pool struct {
	Name      string          `json:"name"`
	Strategy  string          `json:"strategy"` // weighted | least_loaded | priority | round_robin
	Tier      string          `json:"tier"`     // interactive | batch
	Backends  []Backend       `json:"backends"`
	Cache     CachePolicy     `json:"cache"`
	Guardrail GuardrailPolicy `json:"guardrail"`
	Retry     *Retry          `json:"retry,omitempty"`
	Breaker   *Breaker        `json:"breaker,omitempty"`
}

// Model is a logical model as advertised to callers.
type Model struct {
	Name          string   `json:"name"`
	Pool          string   `json:"pool"`
	ContextWindow int      `json:"context_window"`
	MaxOutput     int      `json:"max_output_tokens"`
	Capabilities  []string `json:"capabilities,omitempty"`
	Description   string   `json:"description,omitempty"`
	Deprecated    bool     `json:"deprecated,omitempty"`
}

// Quota is the default envelope applied when the registry has no value.
type Quota struct {
	RequestsPerMinute  int64 `json:"requests_per_minute"`
	TokensPerMinute    int64 `json:"tokens_per_minute"`
	MonthlyTokenBudget int64 `json:"monthly_token_budget"`
}

// Chargeback configures usage recording.
type Chargeback struct {
	Enabled       bool               `json:"enabled"`
	Dir           string             `json:"dir"`
	QueueSize     int                `json:"queue_size"`
	DailyCeilings map[string]float64 `json:"daily_ceilings,omitempty"`
	AnomalyAlpha  float64            `json:"anomaly_alpha"`
	AnomalySigmas float64            `json:"anomaly_sigmas"`
}

// Gateway is the traffic plane's own configuration.
type Gateway struct {
	RequestTimeout    Duration `json:"request_timeout"`
	StreamTimeout     Duration `json:"stream_timeout"`
	StreamHeartbeat   Duration `json:"stream_heartbeat"`
	StreamStallAfter  Duration `json:"stream_stall_after"`
	MaxBodyBytes      int64    `json:"max_body_bytes"`
	MaxConcurrent     int64    `json:"max_concurrent"`
	BatchReserveRatio float64  `json:"batch_reserve_ratio"`
	DefaultMaxTokens  int      `json:"default_max_tokens"`
	IdempotencyTTL    Duration `json:"idempotency_ttl"`
	// RateLimiter selects "redis" or "memory".
	RateLimiter string  `json:"rate_limiter"`
	Retry       Retry   `json:"retry"`
	Breaker     Breaker `json:"breaker"`
	Quota       Quota   `json:"default_quota"`
}

// Config is the gateway service configuration file.
type Config struct {
	Env        string      `json:"env"`
	Service    Service     `json:"service"`
	Telemetry  Telemetry   `json:"telemetry"`
	Identity   Identity    `json:"identity"`
	Redis      Redis       `json:"redis"`
	Gateway    Gateway     `json:"gateway"`
	Cache      CachePolicy `json:"cache"`
	Chargeback Chargeback  `json:"chargeback"`
	Models     []Model     `json:"models"`
	Pools      []Pool      `json:"pools"`
	// ControlPlaneURL is used for registry lookups that are not carried in the
	// token, such as a live quota override.
	ControlPlaneURL string `json:"control_plane_url,omitempty"`
}

var envRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// ExpandEnv substitutes ${VAR} and ${VAR:-default} references.
func ExpandEnv(raw []byte) []byte {
	return envRE.ReplaceAllFunc(raw, func(m []byte) []byte {
		groups := envRE.FindSubmatch(m)
		if v, ok := os.LookupEnv(string(groups[1])); ok {
			return []byte(v)
		}
		return groups[3]
	})
}

// Load reads a configuration file, expanding environment references and
// applying defaults. It does not validate; call Validate separately so that a
// caller can inspect what was loaded before deciding what to do about it.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	raw = ExpandEnv(raw)
	cfg := &Config{}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		if err := json.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	default:
		if err := yamlite.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	cfg.applyDefaults()
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Env == "" {
		c.Env = envOr("AGENTGATE_ENV", "dev")
	}
	if c.Service.Name == "" {
		c.Service.Name = "agentgate-gateway"
	}
	if c.Service.HTTPAddr == "" {
		c.Service.HTTPAddr = ":8080"
	}
	if c.Service.MetricsAddr == "" {
		c.Service.MetricsAddr = ":9090"
	}
	if c.Service.LogLevel == "" {
		c.Service.LogLevel = envOr("AGENTGATE_LOG_LEVEL", "info")
	}
	if c.Service.ShutdownTimeout == 0 {
		c.Service.ShutdownTimeout = Duration(30 * time.Second)
	}
	if c.Service.ReadTimeout == 0 {
		c.Service.ReadTimeout = Duration(30 * time.Second)
	}
	if c.Service.WriteTimeout == 0 {
		// Long enough for a slow generation; streaming responses rely on the
		// handler writing continuously rather than on this deadline.
		c.Service.WriteTimeout = Duration(10 * time.Minute)
	}
	if c.Service.IdleTimeout == 0 {
		c.Service.IdleTimeout = Duration(120 * time.Second)
	}
	if c.Telemetry.SampleRatio == 0 {
		c.Telemetry.SampleRatio = 1
	}
	if c.Telemetry.ContentCapture == "" {
		c.Telemetry.ContentCapture = "off"
	}
	if c.Telemetry.Namespace == "" {
		c.Telemetry.Namespace = "agentgate"
	}
	if c.Telemetry.OTLPEndpoint == "" {
		c.Telemetry.OTLPEndpoint = os.Getenv("AGENTGATE_OTLP_ENDPOINT")
	}
	if c.Identity.JWKSURL == "" {
		c.Identity.JWKSURL = os.Getenv("AGENTGATE_OIDC_JWKS_URL")
	}
	if c.Identity.Issuer == "" {
		c.Identity.Issuer = os.Getenv("AGENTGATE_OIDC_ISSUER")
	}
	if c.Identity.ClockSkew == 0 {
		c.Identity.ClockSkew = Duration(30 * time.Second)
	}
	if c.Identity.JWKSRefresh == 0 {
		c.Identity.JWKSRefresh = Duration(10 * time.Minute)
	}
	if c.Identity.JWKSMaxStale == 0 {
		c.Identity.JWKSMaxStale = Duration(24 * time.Hour)
	}
	if c.Redis.Addr == "" {
		c.Redis.Addr = os.Getenv("AGENTGATE_REDIS_ADDR")
	}
	if c.Gateway.RequestTimeout == 0 {
		c.Gateway.RequestTimeout = Duration(60 * time.Second)
	}
	if c.Gateway.StreamTimeout == 0 {
		c.Gateway.StreamTimeout = Duration(10 * time.Minute)
	}
	if c.Gateway.StreamHeartbeat == 0 {
		c.Gateway.StreamHeartbeat = Duration(15 * time.Second)
	}
	if c.Gateway.StreamStallAfter == 0 {
		c.Gateway.StreamStallAfter = Duration(30 * time.Second)
	}
	if c.Gateway.MaxBodyBytes == 0 {
		c.Gateway.MaxBodyBytes = 8 << 20
	}
	if c.Gateway.MaxConcurrent == 0 {
		c.Gateway.MaxConcurrent = 512
	}
	if c.Gateway.BatchReserveRatio == 0 {
		c.Gateway.BatchReserveRatio = 0.2
	}
	if c.Gateway.DefaultMaxTokens == 0 {
		c.Gateway.DefaultMaxTokens = 1024
	}
	if c.Gateway.IdempotencyTTL == 0 {
		c.Gateway.IdempotencyTTL = Duration(24 * time.Hour)
	}
	if c.Gateway.RateLimiter == "" {
		if c.Redis.Addr != "" {
			c.Gateway.RateLimiter = "redis"
		} else {
			c.Gateway.RateLimiter = "memory"
		}
	}
	if c.Gateway.Retry.MaxAttempts == 0 {
		c.Gateway.Retry.MaxAttempts = 3
	}
	if c.Gateway.Retry.BaseDelay == 0 {
		c.Gateway.Retry.BaseDelay = Duration(100 * time.Millisecond)
	}
	if c.Gateway.Retry.MaxDelay == 0 {
		c.Gateway.Retry.MaxDelay = Duration(2 * time.Second)
	}
	if c.Gateway.Retry.BudgetRatio == 0 {
		c.Gateway.Retry.BudgetRatio = 0.25
	}
	if c.Gateway.Retry.FleetBudgetRatio == 0 {
		c.Gateway.Retry.FleetBudgetRatio = 0.1
	}
	if c.Gateway.Retry.FleetWindow == 0 {
		c.Gateway.Retry.FleetWindow = Duration(10 * time.Second)
	}
	if c.Gateway.Breaker.WindowSize == 0 {
		c.Gateway.Breaker = Breaker{
			WindowSize: 50, FailureRatio: 0.5, ConsecutiveFailures: 10,
			MinimumRequests: 10, OpenDuration: Duration(30 * time.Second), HalfOpenProbes: 5,
		}
	}
	if c.Gateway.Quota.RequestsPerMinute == 0 {
		c.Gateway.Quota.RequestsPerMinute = 600
	}
	if c.Gateway.Quota.TokensPerMinute == 0 {
		c.Gateway.Quota.TokensPerMinute = 120000
	}
	if c.Cache.MaxBytes == 0 {
		c.Cache.MaxBytes = 256 << 20
	}
	if c.Chargeback.Dir == "" {
		c.Chargeback.Dir = "./data/usage"
	}
	if c.Chargeback.QueueSize == 0 {
		c.Chargeback.QueueSize = 8192
	}
	if c.Chargeback.AnomalyAlpha == 0 {
		c.Chargeback.AnomalyAlpha = 0.3
	}
	if c.Chargeback.AnomalySigmas == 0 {
		c.Chargeback.AnomalySigmas = 3
	}
	for i := range c.Pools {
		p := &c.Pools[i]
		if p.Strategy == "" {
			p.Strategy = "weighted_least_loaded"
		}
		if p.Tier == "" {
			p.Tier = "interactive"
		}
		if p.Cache.TTL == 0 {
			p.Cache.TTL = Duration(10 * time.Minute)
		}
		if p.Cache.MaxTemperature == 0 {
			p.Cache.MaxTemperature = 0.2
		}
		if p.Cache.SemanticThreshold == 0 {
			p.Cache.SemanticThreshold = 0.97
		}
		if p.Cache.SemanticCandidates == 0 {
			p.Cache.SemanticCandidates = 200
		}
		for j := range p.Backends {
			b := &p.Backends[j]
			if b.MaxClassification == "" {
				b.MaxClassification = "confidential"
			}
		}
		if p.Guardrail.Provider == "" {
			p.Guardrail.Provider = "builtin"
		}
		// The failure mode is decided by the most sensitive data the pool's
		// backends are cleared to receive, not by whichever value is
		// convenient. A pool that can carry restricted data and cannot reach
		// its content filter must not send anything at all.
		if p.Guardrail.FailureMode == "" {
			p.Guardrail.FailureMode = "fail_open"
			if poolCarriesRestricted(p) {
				p.Guardrail.FailureMode = "fail_closed"
			}
		}
		if p.Guardrail.Timeout == 0 {
			p.Guardrail.Timeout = Duration(2 * time.Second)
		}
		if p.Guardrail.OutputWindowTokens == 0 {
			p.Guardrail.OutputWindowTokens = 256
		}
		for j := range p.Backends {
			b := &p.Backends[j]
			if b.Weight == 0 {
				b.Weight = 100
			}
			if b.Priority == 0 {
				b.Priority = 1
			}
			if b.Timeout == 0 {
				b.Timeout = Duration(60 * time.Second)
			}
			if b.MaxConcurrent == 0 {
				b.MaxConcurrent = 128
			}
			if b.MaxClassification == "" {
				b.MaxClassification = "confidential"
			}
		}
	}
}

// poolCarriesRestricted reports whether any backend in the pool is cleared for
// restricted data.
func poolCarriesRestricted(p *Pool) bool {
	for _, b := range p.Backends {
		if b.IsEnabled() && b.MaxClassification == "restricted" {
			return true
		}
	}
	return false
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
