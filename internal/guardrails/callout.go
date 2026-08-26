package guardrails

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/agentgate/agentgate/internal/httpx"
	"github.com/agentgate/agentgate/internal/resilience"
)

// Callout sends content to an external filtering service.
//
// The wire shape is deliberately generic rather than modelled on one vendor,
// so that a managed content-safety service and the client's own moderation
// service can sit behind the same interface and be swapped without a gateway
// release. Vendor-specific field names are mapped in the adapter below.
type Callout struct {
	name     string
	url      string
	client   *http.Client
	headers  map[string]string
	breaker  *resilience.Breaker
	timeout  time.Duration
	fallback Provider

	mu      sync.Mutex
	lastErr error
}

// CalloutOptions configures the callout provider.
type CalloutOptions struct {
	Name    string
	URL     string
	Client  *http.Client
	Headers map[string]string
	Timeout time.Duration
	// Fallback runs when the external service is unavailable. Using the
	// built-in detector as a fallback means a fail-open pool still gets
	// deterministic PII and secret detection during an outage, which is
	// strictly better than allowing everything.
	Fallback Provider
	Breaker  *resilience.Breaker
}

// NewCallout builds the callout provider.
func NewCallout(o CalloutOptions) (*Callout, error) {
	if o.URL == "" {
		return nil, fmt.Errorf("guardrail callout: url is required")
	}
	if o.Timeout <= 0 {
		o.Timeout = 2 * time.Second
	}
	if o.Client == nil {
		c, err := httpx.NewClient(httpx.ClientConfig{Timeout: o.Timeout})
		if err != nil {
			return nil, err
		}
		o.Client = c
	}
	if o.Name == "" {
		o.Name = "callout"
	}
	if o.Breaker == nil {
		cfg := resilience.DefaultBreakerConfig()
		// A guardrail service is on the critical path of every request, so its
		// breaker opens sooner and probes sooner than a model backend's.
		cfg.OpenDuration = 10 * time.Second
		cfg.ConsecutiveFailures = 5
		o.Breaker = resilience.NewBreaker("guardrail:"+o.Name, cfg, nil)
	}
	return &Callout{
		name: o.Name, url: o.URL, client: o.Client, headers: o.Headers,
		timeout: o.Timeout, fallback: o.Fallback, breaker: o.Breaker,
	}, nil
}

// Name returns the provider name.
func (c *Callout) Name() string { return c.name }

type calloutRequest struct {
	Text               string   `json:"text"`
	Direction          string   `json:"direction"`
	Pool               string   `json:"pool"`
	Tenant             string   `json:"tenant"`
	AgentID            string   `json:"agent_id"`
	DataClassification string   `json:"data_classification,omitempty"`
	Categories         []string `json:"categories,omitempty"`
}

type calloutResponse struct {
	Action   string `json:"action"`
	Text     string `json:"text,omitempty"`
	Findings []struct {
		Category   string  `json:"category"`
		Severity   string  `json:"severity"`
		Confidence float64 `json:"confidence"`
		Action     string  `json:"action"`
		Offset     int     `json:"offset"`
	} `json:"findings"`
}

// Inspect calls the external service, falling back when the breaker is open or
// the call fails.
func (c *Callout) Inspect(ctx context.Context, req Request, p Policy) (Decision, error) {
	if !c.breaker.Allow() {
		return c.fallbackInspect(ctx, req, p, fmt.Errorf("guardrail breaker open"))
	}
	start := time.Now()
	timeout := c.timeout
	if p.Timeout > 0 {
		timeout = p.Timeout
	}

	body, err := json.Marshal(calloutRequest{
		Text: req.Text, Direction: string(req.Direction), Pool: req.Pool,
		Tenant: req.Tenant, AgentID: req.AgentID,
		DataClassification: req.DataClassification, Categories: req.Categories,
	})
	if err != nil {
		return Decision{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return Decision{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	for k, v := range c.headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := httpx.Do(ctx, c.client, httpReq, timeout)
	if err != nil {
		c.breaker.Failure()
		return c.fallbackInspect(ctx, req, p, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		c.breaker.Failure()
		return c.fallbackInspect(ctx, req, p, fmt.Errorf("guardrail service returned %s", resp.Status))
	}
	if resp.StatusCode >= 300 {
		// A 4xx is a contract bug on the gateway's side, not a service
		// outage: it must not trip the breaker and must not be masked.
		c.breaker.Success()
		return Decision{}, fmt.Errorf("guardrail service rejected the request: %s", resp.Status)
	}

	var out calloutResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		c.breaker.Failure()
		return c.fallbackInspect(ctx, req, p, err)
	}
	c.breaker.Success()

	d := Decision{Action: Action(strings.ToLower(out.Action)), Text: out.Text, Provider: c.name, Latency: time.Since(start)}
	if d.Text == "" {
		d.Text = req.Text
	}
	if d.Action == "" {
		d.Action = ActionAllow
	}
	for _, f := range out.Findings {
		action := Action(strings.ToLower(f.Action))
		// Pool policy has the final say over the service's suggested action,
		// so one pool can redact what another blocks without the filtering
		// service needing to know about pools at all.
		if cp, ok := p.Categories[f.Category]; ok {
			if f.Confidence < cp.Threshold {
				continue
			}
			action = cp.Action
		}
		d.Findings = append(d.Findings, Finding{
			Category: f.Category, Severity: f.Severity, Confidence: f.Confidence,
			Action: action, Offset: f.Offset,
		})
		d.Action = escalate(d.Action, action)
	}
	return d, nil
}

func (c *Callout) fallbackInspect(ctx context.Context, req Request, p Policy, cause error) (Decision, error) {
	c.mu.Lock()
	c.lastErr = cause
	c.mu.Unlock()
	if c.fallback == nil {
		return Decision{}, cause
	}
	d, err := c.fallback.Inspect(ctx, req, p)
	if err != nil {
		return Decision{}, cause
	}
	// The fallback cannot evaluate every category, so a fail-closed pool still
	// refuses: partial coverage is not coverage.
	if p.FailureMode == FailClosed && !d.Blocked() {
		return Decision{}, cause
	}
	d.FailedOpen = true
	d.Provider = c.fallback.Name() + "-fallback"
	return d, nil
}

// Healthy reports whether the external service is currently usable.
func (c *Callout) Healthy(ctx context.Context) bool {
	if c.breaker.State() == resilience.StateOpen {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.url, "/")+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := httpx.Do(ctx, c.client, req, time.Second)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode < 500
}

// LastError reports the most recent callout failure, for the health endpoint.
func (c *Callout) LastError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastErr
}
