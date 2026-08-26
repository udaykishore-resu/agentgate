// Package cost turns token usage into money and attributes it to a cost
// centre.
//
// A shared platform that cannot say what each team spent is a shared platform
// that will eventually be asked to stop spending. Every completed request
// emits an immutable usage record; records roll up hourly and daily by cost
// centre; cache hits are recorded as savings rather than silently omitted, so
// the platform can show what it saved as well as what it cost.
package cost

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Pricing is the unit cost of one backend, per million tokens.
type Pricing struct {
	Backend       string  `json:"backend"`
	InputPer1M    float64 `json:"input_per_1m"`
	OutputPer1M   float64 `json:"output_per_1m"`
	CachedPer1M   float64 `json:"cached_input_per_1m,omitempty"`
	Currency      string  `json:"currency,omitempty"`
	EffectiveFrom string  `json:"effective_from,omitempty"`
	// Source records where the number came from — a price list, a negotiated
	// rate card, or an internal cost model for on-premises inference. A cost
	// figure whose provenance is unknown cannot be defended in a chargeback
	// dispute.
	Source string `json:"source,omitempty"`
}

// Cost computes the cost of one call.
func (p Pricing) Cost(inputTokens, outputTokens, cachedTokens int) float64 {
	billableInput := inputTokens - cachedTokens
	if billableInput < 0 {
		billableInput = 0
	}
	c := float64(billableInput)/1e6*p.InputPer1M + float64(outputTokens)/1e6*p.OutputPer1M
	if cachedTokens > 0 {
		rate := p.CachedPer1M
		if rate == 0 {
			rate = p.InputPer1M
		}
		c += float64(cachedTokens) / 1e6 * rate
	}
	return c
}

// Book is the price list, indexed by backend name.
type Book struct {
	mu     sync.RWMutex
	prices map[string]Pricing
}

// NewBook builds a price book.
func NewBook(prices []Pricing) *Book {
	b := &Book{prices: map[string]Pricing{}}
	for _, p := range prices {
		b.prices[p.Backend] = p
	}
	return b
}

// Set adds or replaces a price.
func (b *Book) Set(p Pricing) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prices[p.Backend] = p
}

// Lookup returns the price for a backend. The second return reports whether a
// price was found; an unpriced backend produces a zero-cost record marked as
// unpriced rather than a silently free one.
func (b *Book) Lookup(backend string) (Pricing, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	p, ok := b.prices[backend]
	return p, ok
}

// All returns the price list, sorted.
func (b *Book) All() []Pricing {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]Pricing, 0, len(b.prices))
	for _, p := range b.prices {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Backend < out[j].Backend })
	return out
}

// Record is one immutable usage record. The field set is the chargeback
// contract: it is what the monthly export contains and what a dispute is
// resolved against.
type Record struct {
	Timestamp     time.Time `json:"ts"`
	RequestID     string    `json:"request_id"`
	TraceID       string    `json:"trace_id"`
	Tenant        string    `json:"tenant"`
	Team          string    `json:"team"`
	AgentID       string    `json:"agent_id"`
	AgentIdentity string    `json:"agent_identity"`
	AgentVersion  string    `json:"agent_version"`
	Env           string    `json:"env"`
	CostCenter    string    `json:"cost_center"`
	LogicalModel  string    `json:"logical_model"`
	Pool          string    `json:"pool"`
	Provider      string    `json:"provider"`
	BackendModel  string    `json:"backend_model"`
	Operation     string    `json:"operation"`

	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	CachedTokens int `json:"cached_tokens"`

	UnitCostInputPer1M  float64 `json:"unit_cost_input_per_1m"`
	UnitCostOutputPer1M float64 `json:"unit_cost_output_per_1m"`
	CostUSD             float64 `json:"cost_usd"`
	SavingsUSD          float64 `json:"savings_usd,omitempty"`

	Cache    string `json:"cache"`
	Attempts int    `json:"attempts"`
	Billable bool   `json:"billable"`
	// Estimated marks a record whose token counts came from the gateway's
	// estimator because the provider did not report usage. Reconciliation
	// treats these separately; they are not charged as if they were measured.
	Estimated bool `json:"estimated,omitempty"`
	// Unpriced marks a record for a backend with no price entry.
	Unpriced   bool    `json:"unpriced,omitempty"`
	StatusCode int     `json:"status_code"`
	ErrorCode  string  `json:"error_code,omitempty"`
	DurationMS float64 `json:"duration_ms"`
	TTFTMS     float64 `json:"ttft_ms,omitempty"`
}

// Key returns the rollup key for a record.
func (r Record) Key() string {
	return strings.Join([]string{r.Env, r.Tenant, r.Team, r.AgentID, r.CostCenter, r.BackendModel}, "|")
}

// Rollup is an aggregated slice of usage.
type Rollup struct {
	Period       string    `json:"period"`
	Start        time.Time `json:"start"`
	End          time.Time `json:"end"`
	Env          string    `json:"env"`
	Tenant       string    `json:"tenant"`
	Team         string    `json:"team"`
	AgentID      string    `json:"agent_id"`
	CostCenter   string    `json:"cost_center"`
	Requests     int64     `json:"requests"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	CostUSD      float64   `json:"cost_usd"`
	SavingsUSD   float64   `json:"savings_usd"`
	CacheHits    int64     `json:"cache_hits"`
	Errors       int64     `json:"errors"`
}

// Price computes the cost of a record from a book, marking it unpriced when
// the backend has no entry.
func Price(b *Book, r *Record) {
	p, ok := b.Lookup(r.BackendModel)
	if !ok {
		p, ok = b.Lookup(r.Provider)
	}
	if !ok {
		r.Unpriced = true
		return
	}
	r.UnitCostInputPer1M = p.InputPer1M
	r.UnitCostOutputPer1M = p.OutputPer1M
	c := p.Cost(r.InputTokens, r.OutputTokens, r.CachedTokens)
	if r.Billable {
		r.CostUSD = c
	} else {
		// A cache hit costs nothing and saves what it would have cost.
		r.CostUSD = 0
		r.SavingsUSD = c
	}
}

// Format renders a record for a human, used in the CLI and in runbooks.
func (r Record) Format() string {
	return fmt.Sprintf("%s %s %s/%s %s in=%d out=%d $%.6f cache=%s attempts=%d",
		r.Timestamp.UTC().Format(time.RFC3339), r.CostCenter, r.Team, r.AgentIdentity,
		r.BackendModel, r.InputTokens, r.OutputTokens, r.CostUSD, r.Cache, r.Attempts)
}
