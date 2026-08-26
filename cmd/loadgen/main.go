// Command loadgen drives load against the gateway and reports the numbers the
// capacity proof needs.
//
// k6 covers the scripted scenarios in test/load; this exists because a Go
// generator can be built into the same image as the gateway, run inside the
// cluster with no extra toolchain, and measure time-to-first-token on a
// streaming response without a plugin. Proving capacity from outside the
// network the gateway actually serves is proving the wrong thing.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agentgate/agentgate/internal/httpx"
	"github.com/agentgate/agentgate/internal/version"
)

type result struct {
	latency  time.Duration
	ttft     time.Duration
	status   int
	code     string
	backend  string
	cache    string
	attempts string
	stream   bool
	err      error
}

type stats struct {
	mu       sync.Mutex
	results  []result
	statuses map[int]int64
	codes    map[string]int64
	backends map[string]int64
	sent     atomic.Int64
}

func newStats() *stats {
	return &stats{statuses: map[int]int64{}, codes: map[string]int64{}, backends: map[string]int64{}}
}

func (s *stats) add(r result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results = append(s.results, r)
	s.statuses[r.status]++
	if r.code != "" {
		s.codes[r.code]++
	}
	if r.backend != "" {
		s.backends[r.backend]++
	}
}

func main() {
	var (
		target      = flag.String("url", envOr("AGENTGATE_GATEWAY_URL", "http://localhost:8080"), "gateway base URL")
		token       = flag.String("token", os.Getenv("AGENTGATE_TOKEN"), "bearer token")
		model       = flag.String("model", "general-chat", "logical model")
		concurrency = flag.Int("c", 16, "concurrent workers")
		duration    = flag.Duration("d", 30*time.Second, "test duration")
		rate        = flag.Int("rate", 0, "target requests per second; 0 means as fast as possible")
		stream      = flag.Bool("stream", false, "use streaming requests")
		maxTokens   = flag.Int("max-tokens", 128, "max output tokens")
		promptSize  = flag.Int("prompt-words", 120, "prompt length in words")
		priority    = flag.String("priority", "interactive", "interactive or batch")
		ramp        = flag.Bool("ramp", false, "ramp concurrency to find the knee")
		warmup      = flag.Duration("warmup", 3*time.Second, "warmup period excluded from the report")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println(version.String())
		return
	}
	if *token == "" {
		fmt.Fprintln(os.Stderr, "warning: no token supplied; the gateway will reject requests unless it runs with verification disabled")
	}

	client := &http.Client{
		Timeout: 5 * time.Minute,
		Transport: &http.Transport{
			MaxIdleConns:        *concurrency * 4,
			MaxIdleConnsPerHost: *concurrency * 4,
			MaxConnsPerHost:     *concurrency * 4,
			IdleConnTimeout:     90 * time.Second,
		},
	}
	prompt := strings.TrimSpace(strings.Repeat("analysis of a disputed transaction record ", (*promptSize/6)+1))
	body := map[string]any{
		"model":       *model,
		"messages":    []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens":  *maxTokens,
		"temperature": 0.7, // above the cache threshold, so the test measures the gateway rather than its cache
		"stream":      *stream,
	}
	if *stream {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	payload, _ := json.Marshal(body)

	if *ramp {
		runRamp(client, *target, *token, payload, *stream, *priority, *duration)
		return
	}

	fmt.Printf("load: %d workers, %s, stream=%v, rate=%s\n",
		*concurrency, *duration, *stream, rateLabel(*rate))
	s := runPhase(client, *target, *token, payload, *stream, *priority, *concurrency, *rate, *duration, *warmup)
	report(s, *duration-*warmup)
}

func rateLabel(r int) string {
	if r <= 0 {
		return "unbounded"
	}
	return fmt.Sprintf("%d/s", r)
}

func runPhase(client *http.Client, target, token string, payload []byte, stream bool, priority string,
	concurrency, rate int, duration, warmup time.Duration) *stats {

	s := newStats()
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	var ticker *time.Ticker
	var tickCh <-chan time.Time
	if rate > 0 {
		ticker = time.NewTicker(time.Second / time.Duration(rate))
		defer ticker.Stop()
		tickCh = ticker.C
	}

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				if tickCh != nil {
					select {
					case <-ctx.Done():
						return
					case <-tickCh:
					}
				}
				r := doRequest(ctx, client, target, token, payload, stream, priority)
				s.sent.Add(1)
				// Warmup requests are discarded so connection setup and cold
				// caches do not appear in the percentiles being reported.
				if time.Since(start) > warmup {
					s.add(r)
				}
			}
		}()
	}
	wg.Wait()
	return s
}

func doRequest(ctx context.Context, client *http.Client, target, token string, payload []byte, stream bool, priority string) result {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return result{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-agentgate-request-priority", priority)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return result{err: ctx.Err(), status: 0}
		}
		return result{err: err, latency: time.Since(start)}
	}
	defer func() { _ = resp.Body.Close() }()

	r := result{
		status:   resp.StatusCode,
		backend:  resp.Header.Get("x-agentgate-model"),
		cache:    resp.Header.Get("x-agentgate-cache"),
		attempts: resp.Header.Get("x-agentgate-attempts"),
		stream:   stream,
	}
	if !stream {
		raw, _ := io.ReadAll(resp.Body)
		r.latency = time.Since(start)
		if resp.StatusCode >= 300 {
			var p httpx.Problem
			if json.Unmarshal(raw, &p) == nil {
				r.code = string(p.Code)
			}
		}
		return r
	}

	first := true
	_ = httpx.ReadSSE(resp.Body, func(ev httpx.SSEEvent) error {
		if ev.Event != "" || ev.IsDone() {
			return nil
		}
		if first {
			r.ttft = time.Since(start)
			first = false
		}
		return nil
	})
	r.latency = time.Since(start)
	if resp.StatusCode >= 300 {
		r.code = "stream_error"
	}
	return r
}

func runRamp(client *http.Client, target, token string, payload []byte, stream bool, priority string, per time.Duration) {
	fmt.Printf("ramp: finding the concurrency at which p95 breaches\n\n")
	fmt.Printf("%-8s %-10s %-10s %-10s %-10s %-8s\n", "WORKERS", "RPS", "P50", "P95", "P99", "ERR%")
	for _, c := range []int{4, 8, 16, 32, 64, 128, 256} {
		s := runPhase(client, target, token, payload, stream, priority, c, 0, per, per/6)
		lat := latencies(s)
		if len(lat) == 0 {
			fmt.Printf("%-8d no successful requests\n", c)
			continue
		}
		errRate := errorRate(s)
		fmt.Printf("%-8d %-10.1f %-10s %-10s %-10s %-8.2f\n",
			c, float64(len(lat))/(per-per/6).Seconds(),
			pretty(percentile(lat, 0.5)), pretty(percentile(lat, 0.95)),
			pretty(percentile(lat, 0.99)), errRate*100)
		if errRate > 0.02 {
			fmt.Printf("\nknee: error rate exceeded 2%% at %d workers\n", c)
			return
		}
	}
}

func latencies(s *stats) []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]time.Duration, 0, len(s.results))
	for _, r := range s.results {
		if r.err == nil && r.status > 0 {
			out = append(out, r.latency)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func ttfts(s *stats) []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]time.Duration, 0, len(s.results))
	for _, r := range s.results {
		if r.ttft > 0 {
			out = append(out, r.ttft)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func errorRate(s *stats) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.results) == 0 {
		return 0
	}
	bad := 0
	for _, r := range s.results {
		if r.err != nil || r.status >= 500 || r.status == 0 {
			bad++
		}
	}
	return float64(bad) / float64(len(s.results))
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func pretty(d time.Duration) string { return d.Round(time.Millisecond).String() }

func report(s *stats, window time.Duration) {
	lat := latencies(s)
	s.mu.Lock()
	total := len(s.results)
	statuses := s.statuses
	codes := s.codes
	backends := s.backends
	s.mu.Unlock()

	fmt.Printf("\nrequests recorded: %d over %s (%.1f/s)\n", total, window, float64(total)/window.Seconds())
	if len(lat) > 0 {
		fmt.Printf("latency  p50 %s  p90 %s  p95 %s  p99 %s  max %s\n",
			pretty(percentile(lat, 0.5)), pretty(percentile(lat, 0.9)),
			pretty(percentile(lat, 0.95)), pretty(percentile(lat, 0.99)),
			pretty(lat[len(lat)-1]))
	}
	if t := ttfts(s); len(t) > 0 {
		fmt.Printf("ttft     p50 %s  p95 %s  p99 %s\n",
			pretty(percentile(t, 0.5)), pretty(percentile(t, 0.95)), pretty(percentile(t, 0.99)))
	}
	fmt.Printf("error rate: %.2f%%\n", errorRate(s)*100)

	fmt.Println("\nstatus codes:")
	keys := make([]int, 0, len(statuses))
	for k := range statuses {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	for _, k := range keys {
		fmt.Printf("  %d  %d\n", k, statuses[k])
	}
	if len(codes) > 0 {
		fmt.Println("\nerror codes:")
		for k, v := range codes {
			fmt.Printf("  %-24s %d\n", k, v)
		}
	}
	if len(backends) > 0 {
		fmt.Println("\nbackend distribution:")
		for k, v := range backends {
			fmt.Printf("  %-32s %6d  %5.1f%%\n", k, v, 100*float64(v)/float64(total))
		}
	}
	fmt.Println("\nAcceptance is judged against the SLOs in docs/07-slo-alerting.md, not against these numbers alone.")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
