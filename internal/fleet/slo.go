package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// PromClient is a minimal Prometheus query client.
//
// The fleet view computes error budgets from the same recording rules the
// alerts fire on, rather than from a second implementation. Two definitions of
// the same SLI is how a dashboard ends up disagreeing with a page.
type PromClient struct {
	base   string
	client *http.Client
}

// NewPromClient builds a client against a Prometheus-compatible query API.
func NewPromClient(base string, client *http.Client) *PromClient {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &PromClient{base: strings.TrimRight(base, "/"), client: client}
}

type promResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
		} `json:"result"`
	} `json:"data"`
	Error string `json:"error,omitempty"`
}

// Sample is one instant-query result.
type Sample struct {
	Labels map[string]string
	Value  float64
}

// Query runs an instant query.
func (p *PromClient) Query(ctx context.Context, expr string) ([]Sample, error) {
	if p == nil || p.base == "" {
		return nil, fmt.Errorf("no Prometheus endpoint is configured")
	}
	u := p.base + "/api/v1/query?query=" + url.QueryEscape(expr)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("prometheus returned %s", resp.Status)
	}
	var out promResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Status != "success" {
		return nil, fmt.Errorf("prometheus error: %s", out.Error)
	}
	samples := make([]Sample, 0, len(out.Data.Result))
	for _, r := range out.Data.Result {
		if len(r.Value) != 2 {
			continue
		}
		raw, _ := r.Value[1].(string)
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			continue
		}
		samples = append(samples, Sample{Labels: r.Metric, Value: v})
	}
	return samples, nil
}

// Scalar runs a query expected to return a single value.
func (p *PromClient) Scalar(ctx context.Context, expr string) (float64, error) {
	s, err := p.Query(ctx, expr)
	if err != nil {
		return 0, err
	}
	if len(s) == 0 {
		return math.NaN(), nil
	}
	return s[0].Value, nil
}

// Objective is one service level objective.
type Objective struct {
	Name string `json:"name"`
	// GoodExpr and TotalExpr are PromQL over the SLO window. The ratio is the
	// SLI; keeping numerator and denominator separate is what allows the
	// error-budget arithmetic below to be exact rather than approximate.
	GoodExpr    string        `json:"good_expr"`
	TotalExpr   string        `json:"total_expr"`
	Target      float64       `json:"target"`
	Window      time.Duration `json:"window"`
	Description string        `json:"description"`
	Runbook     string        `json:"runbook"`
}

// Status is a computed objective.
type Status struct {
	Objective
	SLI               float64   `json:"sli"`
	Good              float64   `json:"good_events"`
	Total             float64   `json:"total_events"`
	BudgetTotalEvents float64   `json:"budget_total_events"`
	BudgetSpent       float64   `json:"budget_spent_events"`
	BudgetRemaining   float64   `json:"budget_remaining_fraction"`
	BudgetMinutes     float64   `json:"budget_minutes_total"`
	MinutesRemaining  float64   `json:"budget_minutes_remaining"`
	Met               bool      `json:"met"`
	Error             string    `json:"error,omitempty"`
	EvaluatedAt       time.Time `json:"evaluated_at"`
}

// DefaultObjectives are the SLOs from SPEC.md section 5, expressed against the
// metric names the gateway actually exports.
func DefaultObjectives(window time.Duration) []Objective {
	w := durationLabel(window)
	return []Objective{
		{
			Name: "gateway_availability",
			// A request refused because every backend is unavailable is a
			// platform failure and counts against the budget. A request
			// refused because the caller sent nonsense, exceeded its own
			// quota, or disconnected does not: an SLI that moves when a
			// consuming team ships a bug is an SLI nobody trusts.
			GoodExpr: fmt.Sprintf(
				`sum(increase(agentgate_gateway_requests_total{status!~"5..",code!="no_healthy_backend"}[%s]))`, w),
			TotalExpr:   fmt.Sprintf(`sum(increase(agentgate_gateway_requests_total{code!="client_closed_request"}[%s]))`, w),
			Target:      0.999,
			Window:      window,
			Description: "Requests served without a platform-caused failure.",
			Runbook:     "docs/runbooks/gateway-availability-burn.md",
		},
		{
			Name: "gateway_overhead_latency",
			GoodExpr: fmt.Sprintf(
				`sum(increase(agentgate_gateway_overhead_seconds_bucket{le="0.06",stream="false"}[%s]))`, w),
			TotalExpr:   fmt.Sprintf(`sum(increase(agentgate_gateway_overhead_seconds_count{stream="false"}[%s]))`, w),
			Target:      0.99,
			Window:      window,
			Description: "Unary requests whose gateway overhead stayed under 60ms.",
			Runbook:     "docs/runbooks/gateway-latency-regression.md",
		},
		{
			Name:        "gateway_ttft",
			GoodExpr:    fmt.Sprintf(`sum(increase(agentgate_gateway_ttft_seconds_bucket{le="1.2"}[%s]))`, w),
			TotalExpr:   fmt.Sprintf(`sum(increase(agentgate_gateway_ttft_seconds_count[%s]))`, w),
			Target:      0.99,
			Window:      window,
			Description: "Streaming responses whose first token arrived within 1.2s.",
			Runbook:     "docs/runbooks/gateway-ttft-regression.md",
		},
		{
			Name:        "controlplane_token_exchange",
			GoodExpr:    fmt.Sprintf(`sum(increase(agentgate_controlplane_token_exchange_total{result="success"}[%s]))`, w),
			TotalExpr:   fmt.Sprintf(`sum(increase(agentgate_controlplane_token_exchange_total[%s]))`, w),
			Target:      0.9995,
			Window:      window,
			Description: "Token exchanges that succeeded.",
			Runbook:     "docs/runbooks/controlplane-token-exchange-failures.md",
		},
	}
}

func durationLabel(d time.Duration) string {
	if d >= 24*time.Hour && d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
	if d >= time.Hour && d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

// Evaluate computes an objective's current status and error budget.
func Evaluate(ctx context.Context, p *PromClient, o Objective) Status {
	st := Status{Objective: o, EvaluatedAt: time.Now().UTC()}
	good, err := p.Scalar(ctx, o.GoodExpr)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	total, err := p.Scalar(ctx, o.TotalExpr)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	if math.IsNaN(total) || total == 0 {
		// No traffic is not a breach. Reporting 0% availability for an idle
		// hour is how an SLO dashboard trains people to ignore it.
		st.SLI, st.Met, st.BudgetRemaining = 1, true, 1
		return st
	}
	if math.IsNaN(good) {
		good = 0
	}
	st.Good, st.Total = good, total
	st.SLI = good / total
	st.Met = st.SLI >= o.Target

	allowedBad := (1 - o.Target) * total
	actualBad := total - good
	st.BudgetTotalEvents = allowedBad
	st.BudgetSpent = actualBad
	if allowedBad > 0 {
		st.BudgetRemaining = 1 - actualBad/allowedBad
	} else {
		st.BudgetRemaining = 0
	}
	if st.BudgetRemaining < 0 {
		st.BudgetRemaining = 0
	}
	st.BudgetMinutes = o.Window.Minutes() * (1 - o.Target)
	st.MinutesRemaining = st.BudgetMinutes * st.BudgetRemaining
	return st
}

// BurnRate reports how fast the budget is being consumed over a short window
// relative to the objective. A burn rate of 1 exhausts the budget exactly at
// the end of the SLO window; the alerting thresholds are multiples of it.
func BurnRate(ctx context.Context, p *PromClient, o Objective, short time.Duration) (float64, error) {
	w := durationLabel(short)
	good := strings.ReplaceAll(o.GoodExpr, durationLabel(o.Window), w)
	total := strings.ReplaceAll(o.TotalExpr, durationLabel(o.Window), w)
	g, err := p.Scalar(ctx, good)
	if err != nil {
		return 0, err
	}
	t, err := p.Scalar(ctx, total)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(t) || t == 0 {
		return 0, nil
	}
	if math.IsNaN(g) {
		g = 0
	}
	failureRatio := (t - g) / t
	budget := 1 - o.Target
	if budget <= 0 {
		return 0, nil
	}
	return failureRatio / budget, nil
}
