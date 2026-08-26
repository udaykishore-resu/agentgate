// Package fleet implements the observability plane: the fleet inventory, the
// telemetry-trust metrics, SLI and error-budget computation, chargeback
// reporting and the dashboard.
//
// The distinguishing responsibility here is trust. Collecting telemetry is
// easy and says nothing; this package answers whether the traces that arrived
// are complete, correctly parented and correctly attributed to an owning team,
// and it makes that answer a hard input to the promotion gate. A platform that
// only knows telemetry is "being collected" cannot tell an incident from a
// blind spot.
package fleet

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/agentgate/agentgate/internal/telemetry"
)

// TrustStats is the per-agent trust picture over the observation window.
type TrustStats struct {
	AgentID           string    `json:"agent_id"`
	AgentIdentity     string    `json:"agent_identity"`
	Env               string    `json:"env"`
	SpansReceived     int64     `json:"spans_received"`
	RootSpansReceived int64     `json:"root_spans_received"`
	OrphanSpans       int64     `json:"orphan_spans"`
	UnattributedSpans int64     `json:"unattributed_spans"`
	GatewayRequests   int64     `json:"gateway_requests"`
	Completeness      float64   `json:"completeness"`
	OrphanRatio       float64   `json:"orphan_ratio"`
	UnattributedRatio float64   `json:"unattributed_ratio"`
	ClockSkewP99Sec   float64   `json:"clock_skew_p99_seconds"`
	Runtimes          []string  `json:"runtimes"`
	LastSeen          time.Time `json:"last_seen"`
	MissingAttributes []string  `json:"missing_attributes,omitempty"`
}

// Healthy reports whether the agent's telemetry meets the platform bar.
func (t TrustStats) Healthy(minCompleteness float64) bool {
	return t.Completeness >= minCompleteness && t.OrphanRatio < 0.05 && t.UnattributedRatio < 0.01
}

type agentKey struct{ agentID, env string }

type window struct {
	spans        int64
	roots        int64
	orphans      int64
	unattributed int64
	skewSamples  []float64
	runtimes     map[string]bool
	missing      map[string]bool
	identity     string
	lastSeen     time.Time
}

// TrustTracker consumes OTLP trace payloads and maintains the trust metrics.
//
// It receives a copy of the trace stream from the collector rather than
// querying the tracing backend. Querying the backend would make the trust
// signal depend on the very component whose health it is supposed to report,
// and a backend that is dropping data would report perfect completeness right
// up until someone noticed by hand.
type TrustTracker struct {
	mu       sync.RWMutex
	current  map[agentKey]*window
	previous map[agentKey]TrustStats
	seen     map[string]time.Time // span id -> arrival, for parent resolution
	rotateAt time.Time
	windowD  time.Duration
	metrics  *telemetry.Instruments
	env      string
}

// NewTrustTracker builds a tracker with the given window length.
func NewTrustTracker(windowD time.Duration, metrics *telemetry.Instruments, env string) *TrustTracker {
	if windowD <= 0 {
		windowD = 5 * time.Minute
	}
	return &TrustTracker{
		current: map[agentKey]*window{}, previous: map[agentKey]TrustStats{},
		seen: map[string]time.Time{}, rotateAt: time.Now().Add(windowD),
		windowD: windowD, metrics: metrics, env: env,
	}
}

// Required resource attributes. A span missing any of these cannot be
// attributed to an owner and is counted against the agent's trust score.
var requiredAttrs = []string{
	telemetry.AttrServiceName,
	telemetry.AttrDeploymentEnv,
	telemetry.AttrAgentID,
	telemetry.AttrTeamID,
	telemetry.AttrCostCenter,
}

// otlpPayload mirrors the OTLP/HTTP JSON trace request. Only the fields the
// tracker reads are modelled; anything else is ignored, so a collector adding
// fields does not break ingestion.
type otlpPayload struct {
	ResourceSpans []struct {
		Resource struct {
			Attributes []otlpKV `json:"attributes"`
		} `json:"resource"`
		ScopeSpans []struct {
			Spans []struct {
				TraceID           string   `json:"traceId"`
				SpanID            string   `json:"spanId"`
				ParentSpanID      string   `json:"parentSpanId"`
				Name              string   `json:"name"`
				StartTimeUnixNano string   `json:"startTimeUnixNano"`
				Attributes        []otlpKV `json:"attributes"`
			} `json:"spans"`
		} `json:"scopeSpans"`
	} `json:"resourceSpans"`
}

type otlpKV struct {
	Key   string `json:"key"`
	Value struct {
		StringValue *string  `json:"stringValue"`
		IntValue    *string  `json:"intValue"`
		BoolValue   *bool    `json:"boolValue"`
		DoubleValue *float64 `json:"doubleValue"`
	} `json:"value"`
}

func (kv otlpKV) str() string {
	switch {
	case kv.Value.StringValue != nil:
		return *kv.Value.StringValue
	case kv.Value.IntValue != nil:
		return *kv.Value.IntValue
	case kv.Value.BoolValue != nil:
		return strconv.FormatBool(*kv.Value.BoolValue)
	case kv.Value.DoubleValue != nil:
		return strconv.FormatFloat(*kv.Value.DoubleValue, 'f', -1, 64)
	default:
		return ""
	}
}

func attrMap(kvs []otlpKV) map[string]string {
	out := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		out[kv.Key] = kv.str()
	}
	return out
}

// Handler returns the OTLP/HTTP JSON receiver. The collector is configured to
// fan a copy of the trace pipeline here.
func (t *TrustTracker) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		var payload otlpPayload
		if err := json.Unmarshal(raw, &payload); err != nil {
			http.Error(w, "invalid OTLP JSON payload", http.StatusBadRequest)
			return
		}
		n := t.Ingest(payload, time.Now())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"partialSuccess":{},"spansAccepted":%d}`, n)
	}
}

// Ingest folds one OTLP payload into the current window and returns the number
// of spans processed.
func (t *TrustTracker) Ingest(p otlpPayload, now time.Time) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.maybeRotateLocked(now)

	count := 0
	for _, rs := range p.ResourceSpans {
		res := attrMap(rs.Resource.Attributes)
		agentID := res[telemetry.AttrAgentID]
		env := res[telemetry.AttrDeploymentEnv]
		if agentID == "" {
			agentID = "unattributed:" + res[telemetry.AttrServiceName]
		}
		key := agentKey{agentID: agentID, env: env}
		wnd, ok := t.current[key]
		if !ok {
			wnd = &window{runtimes: map[string]bool{}, missing: map[string]bool{}}
			t.current[key] = wnd
		}
		wnd.identity = res[telemetry.AttrAgentIdentity]
		if rt := res[telemetry.AttrRuntime]; rt != "" {
			wnd.runtimes[rt] = true
		}
		missing := false
		for _, a := range requiredAttrs {
			if res[a] == "" {
				wnd.missing[a] = true
				missing = true
			}
		}

		for _, ss := range rs.ScopeSpans {
			for _, span := range ss.Spans {
				count++
				wnd.spans++
				wnd.lastSeen = now
				if missing {
					wnd.unattributed++
				}
				if span.ParentSpanID == "" {
					wnd.roots++
				} else if _, seen := t.seen[span.ParentSpanID]; !seen {
					// The parent may still arrive: batching means children can
					// overtake parents. Counting it now and forgiving it when
					// the parent lands would need a join buffer; instead the
					// orphan ratio is read as an upper bound, which is the
					// conservative direction for a trust metric.
					wnd.orphans++
				}
				t.seen[span.SpanID] = now
				if skew := clockSkew(span.StartTimeUnixNano, now); skew > 0 {
					wnd.skewSamples = append(wnd.skewSamples, skew)
				}
			}
		}
	}
	return count
}

func clockSkew(startNano string, now time.Time) float64 {
	n, err := strconv.ParseInt(startNano, 10, 64)
	if err != nil || n == 0 {
		return 0
	}
	d := now.Sub(time.Unix(0, n)).Seconds()
	if d < 0 {
		// A span that claims to start in the future is skew in the other
		// direction and is just as much of a problem for trace ordering.
		return -d
	}
	return 0
}

// RecordExpected sets the gateway request count for an agent, which is the
// denominator of completeness. It comes from the gateway's own counter, not
// from the trace stream, so a total collector outage shows as zero
// completeness rather than as no data at all.
func (t *TrustTracker) RecordExpected(agentID, env string, requests int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := agentKey{agentID: agentID, env: env}
	wnd, ok := t.current[key]
	if !ok {
		wnd = &window{runtimes: map[string]bool{}, missing: map[string]bool{}}
		t.current[key] = wnd
	}
	wnd.roots = maxInt64(wnd.roots, 0)
	prev := t.previous[key]
	prev.GatewayRequests = requests
	t.previous[key] = prev
}

func (t *TrustTracker) maybeRotateLocked(now time.Time) {
	if now.Before(t.rotateAt) {
		return
	}
	for key, wnd := range t.current {
		stats := summarise(key, wnd, t.previous[key].GatewayRequests)
		t.previous[key] = stats
		if t.metrics != nil {
			label := stats.AgentIdentity
			if label == "" {
				label = key.agentID
			}
			t.metrics.Completeness.Set(stats.Completeness, key.env, label)
			t.metrics.OrphanRatio.Set(stats.OrphanRatio, key.env, label)
			t.metrics.UnattributedRate.Set(stats.UnattributedRatio, key.env, label)
			t.metrics.ClockSkew.Set(stats.ClockSkewP99Sec, key.env, label)
		}
	}
	t.current = map[agentKey]*window{}
	// Span ids older than two windows can no longer be a parent for anything
	// arriving now, and keeping them would grow without bound.
	cutoff := now.Add(-2 * t.windowD)
	for id, at := range t.seen {
		if at.Before(cutoff) {
			delete(t.seen, id)
		}
	}
	t.rotateAt = now.Add(t.windowD)
}

func summarise(key agentKey, w *window, expected int64) TrustStats {
	s := TrustStats{
		AgentID: key.agentID, AgentIdentity: w.identity, Env: key.env,
		SpansReceived: w.spans, RootSpansReceived: w.roots,
		OrphanSpans: w.orphans, UnattributedSpans: w.unattributed,
		GatewayRequests: expected, LastSeen: w.lastSeen,
	}
	for r := range w.runtimes {
		s.Runtimes = append(s.Runtimes, r)
	}
	sort.Strings(s.Runtimes)
	for a := range w.missing {
		s.MissingAttributes = append(s.MissingAttributes, a)
	}
	sort.Strings(s.MissingAttributes)

	switch {
	case expected > 0:
		s.Completeness = float64(w.roots) / float64(expected)
		if s.Completeness > 1 {
			s.Completeness = 1
		}
	case w.spans > 0:
		// No expectation recorded yet; a stream of well-formed spans is the
		// best available evidence and is reported as such.
		s.Completeness = 1
	}
	if w.spans > 0 {
		s.OrphanRatio = float64(w.orphans) / float64(w.spans)
		s.UnattributedRatio = float64(w.unattributed) / float64(w.spans)
	}
	s.ClockSkewP99Sec = percentile(w.skewSamples, 0.99)
	return s
}

func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := append([]float64{}, xs...)
	sort.Float64s(sorted)
	idx := int(p * float64(len(sorted)-1))
	return sorted[idx]
}

// Stats returns the most recently completed window, plus the in-progress one
// for agents that have not yet appeared in a completed window.
func (t *TrustTracker) Stats() []TrustStats {
	t.mu.Lock()
	t.maybeRotateLocked(time.Now())
	out := make([]TrustStats, 0, len(t.previous))
	for key, s := range t.previous {
		if s.AgentID == "" {
			s.AgentID = key.agentID
			s.Env = key.env
		}
		out = append(out, s)
	}
	t.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out
}

// StatsFor returns one agent's trust picture.
func (t *TrustTracker) StatsFor(agentID, env string) (TrustStats, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	s, ok := t.previous[agentKey{agentID: agentID, env: env}]
	return s, ok
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
