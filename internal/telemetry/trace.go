package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// TraceID is a W3C 16-byte trace identifier.
type TraceID [16]byte

// SpanID is a W3C 8-byte span identifier.
type SpanID [8]byte

// String renders the identifier as lowercase hex, the form used on the wire,
// in the x-agentgate-trace-id response header and in log records.
func (t TraceID) String() string { return hex.EncodeToString(t[:]) }

// String renders the identifier as lowercase hex.
func (s SpanID) String() string { return hex.EncodeToString(s[:]) }

// IsValid reports whether the identifier is non-zero.
func (t TraceID) IsValid() bool { return t != TraceID{} }

// IsValid reports whether the identifier is non-zero.
func (s SpanID) IsValid() bool { return s != SpanID{} }

// SpanKind mirrors the OTLP span kind enumeration.
type SpanKind int

// Span kinds, numbered as OTLP requires.
const (
	KindInternal SpanKind = 1
	KindServer   SpanKind = 2
	KindClient   SpanKind = 3
	KindProducer SpanKind = 4
	KindConsumer SpanKind = 5
)

// StatusCode mirrors the OTLP status code enumeration.
type StatusCode int

// Status codes, numbered as OTLP requires.
const (
	StatusUnset StatusCode = 0
	StatusOK    StatusCode = 1
	StatusError StatusCode = 2
)

// KeyValue is one span, event or resource attribute.
type KeyValue struct {
	Key   string
	Value any
}

// Attr constructs a KeyValue. Values may be string, bool, int/int64, float64,
// or a slice of strings; anything else is rendered with fmt.Sprint at export.
func Attr(key string, value any) KeyValue { return KeyValue{Key: key, Value: value} }

// SpanContext identifies a span for propagation purposes.
type SpanContext struct {
	TraceID    TraceID
	SpanID     SpanID
	TraceFlags byte
	TraceState string
	Remote     bool
}

// IsValid reports whether both identifiers are populated.
func (sc SpanContext) IsValid() bool { return sc.TraceID.IsValid() && sc.SpanID.IsValid() }

// IsSampled reports whether the sampled flag is set.
func (sc SpanContext) IsSampled() bool { return sc.TraceFlags&0x01 == 1 }

// Event is a timestamped annotation on a span. Prompt and completion content,
// when capture is enabled, is carried as events rather than attributes so it
// can be routed to a separate, access-controlled pipeline.
type Event struct {
	Name       string
	Time       time.Time
	Attributes []KeyValue
}

// Span is an in-progress or completed unit of work.
type Span struct {
	tracer *Tracer

	mu         sync.Mutex
	name       string
	kind       SpanKind
	sc         SpanContext
	parent     SpanContext
	start      time.Time
	end        time.Time
	attributes []KeyValue
	events     []Event
	status     StatusCode
	statusMsg  string
	ended      bool
}

type spanKey struct{}

// ContextWithSpan returns a context carrying span.
func ContextWithSpan(ctx context.Context, s *Span) context.Context {
	return context.WithValue(ctx, spanKey{}, s)
}

// SpanFromContext returns the active span, or a no-op span that is safe to
// call methods on when none is active.
func SpanFromContext(ctx context.Context) *Span {
	if s, ok := ctx.Value(spanKey{}).(*Span); ok && s != nil {
		return s
	}
	return noopSpan
}

var noopSpan = &Span{ended: true}

// SpanContextFromContext returns the active span context, which may be the
// zero value when no span is active.
func SpanContextFromContext(ctx context.Context) SpanContext {
	if s, ok := ctx.Value(spanKey{}).(*Span); ok && s != nil {
		return s.SpanContext()
	}
	if sc, ok := ctx.Value(remoteKey{}).(SpanContext); ok {
		return sc
	}
	return SpanContext{}
}

// SpanContext returns the span's own context.
func (s *Span) SpanContext() SpanContext {
	if s == nil {
		return SpanContext{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sc
}

// SetAttributes adds or replaces attributes on the span. It is a no-op after
// the span has ended, so late callbacks cannot corrupt an exported span.
func (s *Span) SetAttributes(kvs ...KeyValue) {
	if s == nil || s.tracer == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	for _, kv := range kvs {
		replaced := false
		for i := range s.attributes {
			if s.attributes[i].Key == kv.Key {
				s.attributes[i].Value = kv.Value
				replaced = true
				break
			}
		}
		if !replaced {
			s.attributes = append(s.attributes, kv)
		}
	}
}

// AddEvent records a timestamped event on the span.
func (s *Span) AddEvent(name string, kvs ...KeyValue) {
	if s == nil || s.tracer == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	s.events = append(s.events, Event{Name: name, Time: time.Now(), Attributes: kvs})
}

// SetStatus sets the span status. Once set to error it is not downgraded.
func (s *Span) SetStatus(code StatusCode, msg string) {
	if s == nil || s.tracer == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended || (s.status == StatusError && code != StatusError) {
		return
	}
	s.status = code
	s.statusMsg = msg
}

// RecordError marks the span as failed and records an exception event.
func (s *Span) RecordError(err error) {
	if s == nil || s.tracer == nil || err == nil {
		return
	}
	s.AddEvent("exception",
		Attr("exception.type", fmt.Sprintf("%T", err)),
		Attr("exception.message", err.Error()),
	)
	s.SetStatus(StatusError, err.Error())
}

// End completes the span and hands it to the processor. Calling End twice is
// safe and the second call is ignored.
func (s *Span) End() {
	if s == nil || s.tracer == nil {
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	s.end = time.Now()
	snap := s.snapshotLocked()
	s.mu.Unlock()
	s.tracer.provider.enqueue(snap)
}

func (s *Span) snapshotLocked() Snapshot {
	attrs := make([]KeyValue, len(s.attributes))
	copy(attrs, s.attributes)
	events := make([]Event, len(s.events))
	copy(events, s.events)
	return Snapshot{
		Name:       s.name,
		Kind:       s.kind,
		Context:    s.sc,
		Parent:     s.parent,
		Start:      s.start,
		End:        s.end,
		Attributes: attrs,
		Events:     events,
		Status:     s.status,
		StatusMsg:  s.statusMsg,
		Scope:      s.tracer.name,
	}
}

// Snapshot is an immutable, exportable view of a completed span.
type Snapshot struct {
	Name       string
	Kind       SpanKind
	Context    SpanContext
	Parent     SpanContext
	Start      time.Time
	End        time.Time
	Attributes []KeyValue
	Events     []Event
	Status     StatusCode
	StatusMsg  string
	Scope      string
}

// Duration returns the span's wall-clock duration.
func (s Snapshot) Duration() time.Duration { return s.End.Sub(s.Start) }

// Tracer creates spans for one instrumentation scope.
type Tracer struct {
	name     string
	provider *Provider
}

// SpanOption customises span creation.
type SpanOption func(*spanConfig)

type spanConfig struct {
	kind       SpanKind
	attributes []KeyValue
	newRoot    bool
	startTime  time.Time
}

// WithSpanKind sets the span kind.
func WithSpanKind(k SpanKind) SpanOption { return func(c *spanConfig) { c.kind = k } }

// WithAttributes sets attributes at span start, which is cheaper than setting
// them afterwards and makes them visible to sampling decisions.
func WithAttributes(kvs ...KeyValue) SpanOption {
	return func(c *spanConfig) { c.attributes = append(c.attributes, kvs...) }
}

// WithNewRoot starts a new trace even when a parent is present. Used for
// background work that should not inherit a request's sampling decision.
func WithNewRoot() SpanOption { return func(c *spanConfig) { c.newRoot = true } }

// Start creates a span and returns a context carrying it.
func (t *Tracer) Start(ctx context.Context, name string, opts ...SpanOption) (context.Context, *Span) {
	if t == nil || t.provider == nil {
		return ctx, noopSpan
	}
	cfg := spanConfig{kind: KindInternal, startTime: time.Now()}
	for _, o := range opts {
		o(&cfg)
	}

	parent := SpanContextFromContext(ctx)
	sc := SpanContext{SpanID: newSpanID()}
	if parent.IsValid() && !cfg.newRoot {
		sc.TraceID = parent.TraceID
		sc.TraceState = parent.TraceState
		sc.TraceFlags = parent.TraceFlags
	} else {
		parent = SpanContext{}
		sc.TraceID = newTraceID()
		if t.provider.sampler.sample(sc.TraceID) {
			sc.TraceFlags = 0x01
		}
	}

	s := &Span{
		tracer:     t,
		name:       name,
		kind:       cfg.kind,
		sc:         sc,
		parent:     parent,
		start:      cfg.startTime,
		attributes: append([]KeyValue{}, cfg.attributes...),
	}
	return ContextWithSpan(ctx, s), s
}

func newTraceID() TraceID {
	var t TraceID
	_, _ = rand.Read(t[:])
	return t
}

func newSpanID() SpanID {
	var s SpanID
	_, _ = rand.Read(s[:])
	return s
}

type sampler struct{ ratio float64 }

func (s sampler) sample(t TraceID) bool {
	if s.ratio >= 1 {
		return true
	}
	if s.ratio <= 0 {
		return false
	}
	// Deterministic on the trace id so the same trace samples identically in
	// every process that sees it.
	v := uint64(t[8])<<56 | uint64(t[9])<<48 | uint64(t[10])<<40 | uint64(t[11])<<32 |
		uint64(t[12])<<24 | uint64(t[13])<<16 | uint64(t[14])<<8 | uint64(t[15])
	return float64(v>>11)/float64(1<<53) < s.ratio
}
