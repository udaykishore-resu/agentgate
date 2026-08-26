// Package guardrails applies content safety policy at the gateway and provides
// the callout path to an external filtering service.
//
// Two decisions in here are worth stating plainly because a security reviewer
// will ask about both. First, the failure mode is per-pool and defaults to
// fail-closed for restricted data: if the filter cannot run, a restricted
// request does not go to a model. Second, output scanning on a stream is
// windowed rather than per-token, because scanning a three-character delta for
// a policy violation is meaningless and scanning the whole completion before
// releasing any of it destroys time-to-first-token. The window is the
// compromise, and its size is a policy knob.
package guardrails

import (
	"context"
	"time"
)

// Action is what policy says to do about a finding.
type Action string

// Guardrail actions.
const (
	ActionAllow    Action = "allow"
	ActionBlock    Action = "block"
	ActionRedact   Action = "redact"
	ActionAnnotate Action = "annotate"
)

// Direction distinguishes prompt scanning from completion scanning.
type Direction string

// Scan directions.
const (
	DirectionInput  Direction = "input"
	DirectionOutput Direction = "output"
)

// FailureMode is what happens when the guardrail cannot reach a verdict.
type FailureMode string

// Failure modes.
const (
	FailOpen   FailureMode = "fail_open"
	FailClosed FailureMode = "fail_closed"
)

// Finding is one policy hit.
type Finding struct {
	Category   string  `json:"category"`
	Severity   string  `json:"severity"`
	Confidence float64 `json:"confidence,omitempty"`
	Action     Action  `json:"action"`
	// Excerpt is a short, redacted indication of what matched. It never
	// contains the matched secret itself.
	Excerpt string `json:"excerpt,omitempty"`
	Offset  int    `json:"offset,omitempty"`
}

// Decision is the outcome of a scan.
type Decision struct {
	Action Action `json:"action"`
	// Text is the possibly-redacted content, set when Action is ActionRedact.
	Text     string    `json:"-"`
	Findings []Finding `json:"findings,omitempty"`
	// FailedOpen records that the scan could not be completed and the pool's
	// policy allowed the request through anyway. It is surfaced on the span
	// and counted, because a silent fail-open is indistinguishable from a
	// guardrail that works.
	FailedOpen bool          `json:"failed_open,omitempty"`
	Latency    time.Duration `json:"-"`
	Provider   string        `json:"provider,omitempty"`
}

// Blocked reports whether the request must be refused.
func (d Decision) Blocked() bool { return d.Action == ActionBlock }

// Category returns the first blocking or redacting category, for the
// x-agentgate-guardrail response header.
func (d Decision) Category() string {
	for _, f := range d.Findings {
		if f.Action == ActionBlock || f.Action == ActionRedact {
			return f.Category
		}
	}
	if len(d.Findings) > 0 {
		return d.Findings[0].Category
	}
	return ""
}

// Request is one scan request.
type Request struct {
	Text      string
	Direction Direction
	// Pool, Tenant and AgentID scope the policy and appear in the callout so an
	// external filtering service can apply its own per-tenant rules.
	Pool               string
	Tenant             string
	AgentID            string
	DataClassification string
	// Categories restricts the scan; empty means every category the provider
	// supports.
	Categories []string
}

// Policy is a pool's guardrail configuration.
type Policy struct {
	Enabled     bool
	FailureMode FailureMode
	// Categories and their thresholds. A threshold of 0 means any hit counts.
	Categories map[string]CategoryPolicy
	// OutputWindowTokens is how much streamed content is buffered before it is
	// scanned and released.
	OutputWindowTokens int
	Timeout            time.Duration
}

// CategoryPolicy is the action and threshold for one category.
type CategoryPolicy struct {
	Action    Action
	Threshold float64
}

// DefaultPolicy returns the default policy for a data classification. The
// classification, not the pool's convenience, decides the failure mode.
func DefaultPolicy(classification string) Policy {
	mode := FailOpen
	if classification == "restricted" {
		mode = FailClosed
	}
	return Policy{
		Enabled:     true,
		FailureMode: mode,
		Categories: map[string]CategoryPolicy{
			"hate":             {Action: ActionBlock, Threshold: 0.5},
			"self_harm":        {Action: ActionBlock, Threshold: 0.3},
			"sexual":           {Action: ActionBlock, Threshold: 0.5},
			"violence":         {Action: ActionBlock, Threshold: 0.5},
			"pii":              {Action: ActionRedact, Threshold: 0},
			"secrets":          {Action: ActionBlock, Threshold: 0},
			"prompt_injection": {Action: ActionAnnotate, Threshold: 0.7},
		},
		OutputWindowTokens: 256,
		Timeout:            2 * time.Second,
	}
}

// Provider evaluates content against policy.
type Provider interface {
	Name() string
	Inspect(ctx context.Context, req Request, p Policy) (Decision, error)
	Healthy(ctx context.Context) bool
}

// Noop allows everything. It exists so that a pool can be explicitly
// configured with no guardrail rather than accidentally having none.
type Noop struct{}

// Name returns the provider name.
func (Noop) Name() string { return "noop" }

// Inspect always allows.
func (Noop) Inspect(context.Context, Request, Policy) (Decision, error) {
	return Decision{Action: ActionAllow, Provider: "noop"}, nil
}

// Healthy always reports true.
func (Noop) Healthy(context.Context) bool { return true }

// Apply resolves a provider decision against a policy's failure mode. It is
// the single place where "the scan failed" becomes "allow" or "block", so the
// behaviour cannot drift between the input and output paths.
func Apply(p Policy, d Decision, err error) Decision {
	if err == nil {
		return d
	}
	if p.FailureMode == FailClosed {
		return Decision{
			Action:   ActionBlock,
			Findings: []Finding{{Category: "guardrail_unavailable", Severity: "high", Action: ActionBlock}},
		}
	}
	return Decision{Action: ActionAllow, FailedOpen: true}
}
