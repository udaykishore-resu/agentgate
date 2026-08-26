package telemetry

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// otlpExporter ships spans to an OpenTelemetry Collector using OTLP/HTTP with
// the JSON encoding, which the collector's otlp receiver accepts natively.
// JSON rather than protobuf keeps the binary dependency-free and the payloads
// inspectable during a network security review, at a bandwidth cost that is
// immaterial next to model traffic. See docs/adr/0014-dependency-policy.md.
type otlpExporter struct {
	url     string
	headers map[string]string
	client  *http.Client
	res     []KeyValue
	retries int
}

func newOTLPExporter(cfg Config, res []KeyValue) *otlpExporter {
	endpoint := strings.TrimRight(cfg.OTLPEndpoint, "/")
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		scheme := "https://"
		if cfg.OTLPInsecure {
			scheme = "http://"
		}
		endpoint = scheme + endpoint
	}
	if !strings.HasSuffix(endpoint, "/v1/traces") {
		endpoint += "/v1/traces"
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return &otlpExporter{
		url:     endpoint,
		headers: cfg.OTLPHeaders,
		client:  &http.Client{Transport: transport, Timeout: cfg.OTLPTimeout},
		res:     res,
		retries: 2,
	}
}

// ExportSpans encodes and POSTs one batch, retrying transient failures. A
// permanent 4xx is not retried: the collector rejecting the payload is a bug
// to fix, not a condition to wait out.
func (e *otlpExporter) ExportSpans(ctx context.Context, spans []Snapshot) error {
	payload, err := json.Marshal(e.encode(spans))
	if err != nil {
		return fmt.Errorf("encode otlp payload: %w", err)
	}
	var lastErr error
	for attempt := 0; attempt <= e.retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * 250 * time.Millisecond):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		for k, v := range e.headers {
			req.Header.Set(k, v)
		}
		resp, err := e.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		switch {
		case resp.StatusCode < 300:
			return nil
		case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
			lastErr = fmt.Errorf("collector returned %s", resp.Status)
		default:
			return fmt.Errorf("collector rejected payload: %s", resp.Status)
		}
	}
	return lastErr
}

// Shutdown releases idle connections.
func (e *otlpExporter) Shutdown(context.Context) error {
	e.client.CloseIdleConnections()
	return nil
}

// --- OTLP/JSON wire types -------------------------------------------------
//
// Field names follow the protobuf JSON mapping. trace_id and span_id are
// hex-encoded strings, as the OTLP/HTTP JSON specification requires, and the
// 64-bit nanosecond timestamps are strings so they survive JSON parsers that
// would otherwise round them through a float64.

type otlpPayload struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

type otlpResourceSpans struct {
	Resource   otlpResource    `json:"resource"`
	ScopeSpans []otlpScopeSpan `json:"scopeSpans"`
}

type otlpResource struct {
	Attributes []otlpKeyValue `json:"attributes"`
}

type otlpScopeSpan struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type otlpScope struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type otlpSpan struct {
	TraceID           string         `json:"traceId"`
	SpanID            string         `json:"spanId"`
	ParentSpanID      string         `json:"parentSpanId,omitempty"`
	TraceState        string         `json:"traceState,omitempty"`
	Name              string         `json:"name"`
	Kind              int            `json:"kind"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	EndTimeUnixNano   string         `json:"endTimeUnixNano"`
	Attributes        []otlpKeyValue `json:"attributes,omitempty"`
	Events            []otlpEvent    `json:"events,omitempty"`
	Status            otlpStatus     `json:"status"`
}

type otlpEvent struct {
	TimeUnixNano string         `json:"timeUnixNano"`
	Name         string         `json:"name"`
	Attributes   []otlpKeyValue `json:"attributes,omitempty"`
}

type otlpStatus struct {
	Code    int    `json:"code"`
	Message string `json:"message,omitempty"`
}

type otlpKeyValue struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

type otlpValue struct {
	StringValue *string        `json:"stringValue,omitempty"`
	BoolValue   *bool          `json:"boolValue,omitempty"`
	IntValue    *string        `json:"intValue,omitempty"`
	DoubleValue *float64       `json:"doubleValue,omitempty"`
	ArrayValue  *otlpArrayValu `json:"arrayValue,omitempty"`
}

type otlpArrayValu struct {
	Values []otlpValue `json:"values"`
}

func (e *otlpExporter) encode(spans []Snapshot) otlpPayload {
	byScope := map[string][]otlpSpan{}
	for _, s := range spans {
		byScope[s.Scope] = append(byScope[s.Scope], encodeSpan(s))
	}
	scopes := make([]otlpScopeSpan, 0, len(byScope))
	for name, ss := range byScope {
		scopes = append(scopes, otlpScopeSpan{Scope: otlpScope{Name: name}, Spans: ss})
	}
	return otlpPayload{ResourceSpans: []otlpResourceSpans{{
		Resource:   otlpResource{Attributes: encodeAttrs(e.res)},
		ScopeSpans: scopes,
	}}}
}

func encodeSpan(s Snapshot) otlpSpan {
	out := otlpSpan{
		TraceID:           s.Context.TraceID.String(),
		SpanID:            s.Context.SpanID.String(),
		TraceState:        s.Context.TraceState,
		Name:              s.Name,
		Kind:              int(s.Kind),
		StartTimeUnixNano: strconv.FormatInt(s.Start.UnixNano(), 10),
		EndTimeUnixNano:   strconv.FormatInt(s.End.UnixNano(), 10),
		Attributes:        encodeAttrs(s.Attributes),
		Status:            otlpStatus{Code: int(s.Status), Message: s.StatusMsg},
	}
	if s.Parent.IsValid() {
		out.ParentSpanID = s.Parent.SpanID.String()
	}
	for _, ev := range s.Events {
		out.Events = append(out.Events, otlpEvent{
			TimeUnixNano: strconv.FormatInt(ev.Time.UnixNano(), 10),
			Name:         ev.Name,
			Attributes:   encodeAttrs(ev.Attributes),
		})
	}
	return out
}

func encodeAttrs(kvs []KeyValue) []otlpKeyValue {
	if len(kvs) == 0 {
		return nil
	}
	out := make([]otlpKeyValue, 0, len(kvs))
	for _, kv := range kvs {
		out = append(out, otlpKeyValue{Key: kv.Key, Value: encodeValue(kv.Value)})
	}
	return out
}

func encodeValue(v any) otlpValue {
	switch t := v.(type) {
	case nil:
		s := ""
		return otlpValue{StringValue: &s}
	case string:
		return otlpValue{StringValue: &t}
	case bool:
		return otlpValue{BoolValue: &t}
	case int:
		s := strconv.Itoa(t)
		return otlpValue{IntValue: &s}
	case int64:
		s := strconv.FormatInt(t, 10)
		return otlpValue{IntValue: &s}
	case float64:
		return otlpValue{DoubleValue: &t}
	case time.Duration:
		f := t.Seconds()
		return otlpValue{DoubleValue: &f}
	case []string:
		vals := make([]otlpValue, 0, len(t))
		for _, s := range t {
			sv := s
			vals = append(vals, otlpValue{StringValue: &sv})
		}
		return otlpValue{ArrayValue: &otlpArrayValu{Values: vals}}
	default:
		s := fmt.Sprint(v)
		return otlpValue{StringValue: &s}
	}
}
