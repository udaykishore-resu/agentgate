package telemetry

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry is a metric registry with Prometheus text exposition. Prometheus
// scrapes each service directly; the collector's OTLP pipeline carries traces
// and logs. Keeping metrics on a pull model means a collector outage cannot
// silently lose the signals the SLOs are computed from.
type Registry struct {
	mu       sync.RWMutex
	families map[string]*family
	resource []KeyValue
}

// NewRegistry creates a registry. Resource attributes are not attached to
// every series — that would explode cardinality — but service.name and
// deployment.environment.name are, because every alert groups by them.
func NewRegistry(resource []KeyValue) *Registry {
	return &Registry{families: map[string]*family{}, resource: resource}
}

type metricType string

const (
	typeCounter   metricType = "counter"
	typeGauge     metricType = "gauge"
	typeHistogram metricType = "histogram"
)

type family struct {
	name    string
	help    string
	kind    metricType
	labels  []string
	buckets []float64

	mu     sync.RWMutex
	series map[string]*series
}

type series struct {
	values  []string
	counter atomic.Uint64 // float64 bits for counters and gauges
	sum     atomic.Uint64 // float64 bits for histogram sums
	count   atomic.Uint64
	buckets []atomic.Uint64
}

func addFloat(a *atomic.Uint64, delta float64) {
	for {
		old := a.Load()
		next := math.Float64bits(math.Float64frombits(old) + delta)
		if a.CompareAndSwap(old, next) {
			return
		}
	}
}

func (r *Registry) family(name, help string, kind metricType, labels []string, buckets []float64) *family {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.families[name]; ok {
		return f
	}
	f := &family{name: name, help: help, kind: kind, labels: labels, buckets: buckets, series: map[string]*series{}}
	r.families[name] = f
	return f
}

func (f *family) series_(values []string) *series {
	key := strings.Join(values, "\x1f")
	f.mu.RLock()
	s, ok := f.series[key]
	f.mu.RUnlock()
	if ok {
		return s
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.series[key]; ok {
		return s
	}
	s = &series{values: append([]string{}, values...)}
	if f.kind == typeHistogram {
		s.buckets = make([]atomic.Uint64, len(f.buckets))
	}
	f.series[key] = s
	return s
}

// Counter is a monotonically increasing metric.
type Counter struct{ f *family }

// Gauge is a metric that can go up and down.
type Gauge struct{ f *family }

// Histogram observes a distribution into fixed buckets.
type Histogram struct{ f *family }

// Counter registers or returns a counter. Label names are fixed at
// registration; values are supplied positionally at record time, which keeps
// the hot path allocation-light and makes a label-count mistake a compile-time
// shape rather than a silent cardinality bug.
func (r *Registry) Counter(name, help string, labels ...string) *Counter {
	return &Counter{f: r.family(name, help, typeCounter, labels, nil)}
}

// Gauge registers or returns a gauge.
func (r *Registry) Gauge(name, help string, labels ...string) *Gauge {
	return &Gauge{f: r.family(name, help, typeGauge, labels, nil)}
}

// Histogram registers or returns a histogram with the given upper bounds.
func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) *Histogram {
	b := append([]float64{}, buckets...)
	sort.Float64s(b)
	return &Histogram{f: r.family(name, help, typeHistogram, labels, b)}
}

// Add increments the counter for the series identified by values.
func (c *Counter) Add(delta float64, values ...string) {
	if c == nil {
		return
	}
	addFloat(&c.f.series_(normalise(values, len(c.f.labels))).counter, delta)
}

// Inc increments the counter by one.
func (c *Counter) Inc(values ...string) { c.Add(1, values...) }

// Set replaces the gauge value.
func (g *Gauge) Set(v float64, values ...string) {
	if g == nil {
		return
	}
	g.f.series_(normalise(values, len(g.f.labels))).counter.Store(math.Float64bits(v))
}

// Add changes the gauge by delta, which may be negative.
func (g *Gauge) Add(delta float64, values ...string) {
	if g == nil {
		return
	}
	addFloat(&g.f.series_(normalise(values, len(g.f.labels))).counter, delta)
}

// Observe records one sample.
func (h *Histogram) Observe(v float64, values ...string) {
	if h == nil {
		return
	}
	s := h.f.series_(normalise(values, len(h.f.labels)))
	s.count.Add(1)
	addFloat(&s.sum, v)
	for i, ub := range h.f.buckets {
		if v <= ub {
			s.buckets[i].Add(1)
		}
	}
}

// normalise pads or truncates label values so a caller mistake degrades to an
// "unknown" label rather than a panic in the request path.
func normalise(values []string, want int) []string {
	if len(values) == want {
		return values
	}
	out := make([]string, want)
	for i := range out {
		if i < len(values) {
			out[i] = values[i]
		} else {
			out[i] = "unknown"
		}
	}
	return out
}

// Handler serves the Prometheus text exposition format.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		var b strings.Builder
		r.WriteTo(&b)
		_, _ = w.Write([]byte(b.String()))
	})
}

// WriteTo renders the whole registry in Prometheus exposition format.
func (r *Registry) WriteTo(b *strings.Builder) {
	r.mu.RLock()
	names := make([]string, 0, len(r.families))
	for n := range r.families {
		names = append(names, n)
	}
	fams := make(map[string]*family, len(r.families))
	for k, v := range r.families {
		fams[k] = v
	}
	r.mu.RUnlock()
	sort.Strings(names)

	for _, name := range names {
		f := fams[name]
		fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", f.name, f.help, f.name, f.kind)
		f.mu.RLock()
		keys := make([]string, 0, len(f.series))
		for k := range f.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s := f.series[k]
			switch f.kind {
			case typeHistogram:
				cumulative := uint64(0)
				for i, ub := range f.buckets {
					cumulative = s.buckets[i].Load()
					fmt.Fprintf(b, "%s_bucket%s %d\n", f.name, labelSet(f.labels, s.values, "le", formatBucket(ub)), cumulative)
				}
				total := s.count.Load()
				fmt.Fprintf(b, "%s_bucket%s %d\n", f.name, labelSet(f.labels, s.values, "le", "+Inf"), total)
				fmt.Fprintf(b, "%s_sum%s %s\n", f.name, labelSet(f.labels, s.values, "", ""), formatFloat(math.Float64frombits(s.sum.Load())))
				fmt.Fprintf(b, "%s_count%s %d\n", f.name, labelSet(f.labels, s.values, "", ""), total)
			default:
				fmt.Fprintf(b, "%s%s %s\n", f.name, labelSet(f.labels, s.values, "", ""), formatFloat(math.Float64frombits(s.counter.Load())))
			}
		}
		f.mu.RUnlock()
	}
}

func labelSet(names, values []string, extraName, extraValue string) string {
	if len(names) == 0 && extraName == "" {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	first := true
	for i, n := range names {
		if i >= len(values) {
			break
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		b.WriteString(n)
		b.WriteString(`="`)
		b.WriteString(escapeLabel(values[i]))
		b.WriteString(`"`)
	}
	if extraName != "" {
		if !first {
			b.WriteByte(',')
		}
		b.WriteString(extraName)
		b.WriteString(`="`)
		b.WriteString(escapeLabel(extraValue))
		b.WriteString(`"`)
	}
	b.WriteByte('}')
	return b.String()
}

func escapeLabel(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

func formatFloat(f float64) string {
	if math.IsInf(f, 1) {
		return "+Inf"
	}
	if math.IsInf(f, -1) {
		return "-Inf"
	}
	if math.IsNaN(f) {
		return "NaN"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func formatBucket(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

// DefaultLatencyBuckets covers gateway overhead, which lives in the low
// milliseconds, through provider round trips, which do not.
var DefaultLatencyBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 10, 20, 30, 60, 120,
}

// OverheadBuckets is tuned for the gateway's own processing time, where the
// SLO threshold is 60ms and resolution below that is what makes a regression
// visible before it breaches.
var OverheadBuckets = []float64{
	0.001, 0.0025, 0.005, 0.0075, 0.01, 0.015, 0.02, 0.03, 0.04, 0.05, 0.06, 0.08, 0.1, 0.25, 0.5, 1,
}

// TTFTBuckets is tuned for time-to-first-token, where the SLO threshold is
// 1200ms.
var TTFTBuckets = []float64{
	0.05, 0.1, 0.2, 0.3, 0.5, 0.75, 1, 1.2, 1.5, 2, 3, 5, 8, 13, 21,
}
