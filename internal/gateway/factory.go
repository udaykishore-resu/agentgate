package gateway

import (
	"fmt"
	"os"
	"time"

	"github.com/agentgate/agentgate/internal/config"
	"github.com/agentgate/agentgate/internal/cost"
	"github.com/agentgate/agentgate/internal/guardrails"
	"github.com/agentgate/agentgate/internal/httpx"
	"github.com/agentgate/agentgate/internal/provider"
	"github.com/agentgate/agentgate/internal/resilience"
)

// NewBackendFactory builds provider adapters from configuration.
//
// Each backend gets its own HTTP client rather than sharing one. A shared
// client would share a connection pool across an on-premises endpoint behind
// ExpressRoute, a cloud endpoint behind a private endpoint, and a third party
// behind an inspecting proxy — three paths with completely different latency,
// TLS material and failure behaviour.
func NewBackendFactory(env string) BackendFactory {
	return func(bc config.Backend) (provider.Provider, error) {
		client, err := httpx.NewClient(httpx.ClientConfig{
			Timeout:             bc.Timeout.Or(60 * time.Second),
			MaxIdleConnsPerHost: bc.MaxConcurrent,
			MaxConnsPerHost:     bc.MaxConcurrent * 2,
			CABundlePath:        bc.CABundlePath,
			ClientCertPath:      bc.ClientCertPath,
			ClientKeyPath:       bc.ClientKeyPath,
			Proxy:               bc.Proxy,
		})
		if err != nil {
			return nil, err
		}
		caps := make([]provider.Capability, 0, len(bc.Capabilities))
		for _, c := range bc.Capabilities {
			caps = append(caps, provider.Capability(c))
		}
		apiKey := resolveSecret(bc.APIKey)

		opts := provider.Options{
			Name: bc.Name, Kind: provider.Kind(bc.Kind), BaseURL: bc.BaseURL,
			Model: bc.Model, APIKey: apiKey, APIVersion: bc.APIVersion,
			Deployment: bc.Deployment, Client: client,
			Timeout: bc.Timeout.Or(60 * time.Second), Capabilities: caps,
			ExtraHeaders: bc.ExtraHeaders,
		}
		switch provider.Kind(bc.Kind) {
		case provider.KindOpenAI, provider.KindAzureOpenAI, provider.KindVLLM:
			return provider.NewOpenAICompatible(opts)
		case provider.KindAnthropic, provider.KindBedrock:
			return provider.NewAnthropic(provider.AnthropicOptions{
				Options: opts, Region: bc.Region,
				AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
				SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
				SessionToken:    os.Getenv("AWS_SESSION_TOKEN"),
			})
		default:
			return nil, fmt.Errorf("unsupported provider kind %q", bc.Kind)
		}
	}
}

// resolveSecret indirects an "env:VAR" or "file:/path" reference so that a
// configuration file committed to a repository never contains a credential.
func resolveSecret(v string) string {
	switch {
	case len(v) > 4 && v[:4] == "env:":
		return os.Getenv(v[4:])
	case len(v) > 5 && v[:5] == "file:":
		raw, err := os.ReadFile(v[5:])
		if err != nil {
			return ""
		}
		return trimSpace(string(raw))
	default:
		return v
	}
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\n' || s[start] == '\r' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\n' || s[end-1] == '\r' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

// NewPriceBook builds the price list from configured backend unit costs.
func NewPriceBook(cfg *config.Config) *cost.Book {
	var prices []cost.Pricing
	for _, p := range cfg.Pools {
		for _, b := range p.Backends {
			source := b.CostSource
			if source == "" {
				source = "configured in " + p.Name
			}
			prices = append(prices, cost.Pricing{
				Backend: b.Name, InputPer1M: b.InputCostPer1M,
				OutputPer1M: b.OutputCostPer1M, CachedPer1M: b.CachedCostPer1M,
				Currency: "USD", Source: source,
			})
		}
	}
	return cost.NewBook(prices)
}

// NewGuardrailProviders builds one guardrail provider per pool.
func NewGuardrailProviders(cfg *config.Config) (map[string]guardrails.Provider, error) {
	out := map[string]guardrails.Provider{}
	builtin := guardrails.NewBuiltin()
	for _, p := range cfg.Pools {
		switch p.Guardrail.Provider {
		case "noop", "":
			out[p.Name] = guardrails.Noop{}
		case "builtin":
			out[p.Name] = builtin
		case "callout":
			client, err := httpx.NewClient(httpx.ClientConfig{Timeout: p.Guardrail.Timeout.Or(2 * time.Second)})
			if err != nil {
				return nil, err
			}
			breakerCfg := resilience.DefaultBreakerConfig()
			breakerCfg.OpenDuration = 10 * time.Second
			breakerCfg.ConsecutiveFailures = 5
			c, err := guardrails.NewCallout(guardrails.CalloutOptions{
				Name: p.Name, URL: p.Guardrail.URL, Client: client,
				Headers: p.Guardrail.Headers, Timeout: p.Guardrail.Timeout.Or(2 * time.Second),
				// The built-in detector backs the external service, so a
				// callout outage still leaves deterministic PII and secret
				// detection in place instead of nothing.
				Fallback: builtin,
				Breaker:  resilience.NewBreaker("guardrail:"+p.Name, breakerCfg, nil),
			})
			if err != nil {
				return nil, fmt.Errorf("pool %s guardrail: %w", p.Name, err)
			}
			out[p.Name] = c
		default:
			return nil, fmt.Errorf("pool %s: unknown guardrail provider %q", p.Name, p.Guardrail.Provider)
		}
	}
	return out, nil
}
