package cost

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPricingArithmetic(t *testing.T) {
	p := Pricing{Backend: "b", InputPer1M: 0.15, OutputPer1M: 0.60}
	got := p.Cost(1_000_000, 1_000_000, 0)
	if math.Abs(got-0.75) > 1e-9 {
		t.Errorf("cost = %f, want 0.75", got)
	}
	// A provider-side prompt-cache hit is billed at the cached rate, not the
	// full input rate.
	cached := Pricing{Backend: "b", InputPer1M: 1.0, OutputPer1M: 0, CachedPer1M: 0.1}
	if got := cached.Cost(1_000_000, 0, 1_000_000); math.Abs(got-0.1) > 1e-9 {
		t.Errorf("cached cost = %f, want 0.1", got)
	}
}

func TestPriceMarksUnpricedBackends(t *testing.T) {
	book := NewBook(nil)
	rec := &Record{BackendModel: "mystery", InputTokens: 1000, OutputTokens: 100, Billable: true}
	Price(book, rec)
	if !rec.Unpriced {
		t.Fatal("a backend with no price entry must be marked, not silently free")
	}
	if rec.CostUSD != 0 {
		t.Errorf("cost = %f", rec.CostUSD)
	}
}

func TestPriceRecordsCacheSavingsRatherThanCost(t *testing.T) {
	book := NewBook([]Pricing{{Backend: "azure/gpt", InputPer1M: 1, OutputPer1M: 2}})
	rec := &Record{
		BackendModel: "azure/gpt", InputTokens: 1_000_000, OutputTokens: 1_000_000,
		Cache: "hit", Billable: false,
	}
	Price(book, rec)
	if rec.CostUSD != 0 {
		t.Errorf("a cache hit must cost nothing, got %f", rec.CostUSD)
	}
	if math.Abs(rec.SavingsUSD-3) > 1e-9 {
		t.Errorf("savings = %f, want 3", rec.SavingsUSD)
	}
}

func TestFileSinkWritesAndRotatesByDay(t *testing.T) {
	dir := t.TempDir()
	sink, err := NewFileSink(FileSinkOptions{Dir: dir, QueueSize: 16})
	if err != nil {
		t.Fatal(err)
	}
	day1 := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	day2 := day1.AddDate(0, 0, 1)
	sink.Write(context.Background(), Record{Timestamp: day1, RequestID: "r1", CostUSD: 0.5})
	sink.Write(context.Background(), Record{Timestamp: day2, RequestID: "r2", CostUSD: 0.25})
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"usage-2026-08-26.jsonl", "usage-2026-08-27.jsonl"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
		if !strings.Contains(string(raw), `"cost_usd"`) {
			t.Errorf("%s does not look like a usage record: %s", name, raw)
		}
		var rec Record
		if err := json.Unmarshal([]byte(strings.SplitN(strings.TrimSpace(string(raw)), "\n", 2)[0]), &rec); err != nil {
			t.Errorf("%s is not valid newline-delimited JSON: %v", name, err)
		}
	}
}

func TestFileSinkDropsRatherThanBlocks(t *testing.T) {
	dir := t.TempDir()
	sink, err := NewFileSink(FileSinkOptions{Dir: dir, QueueSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	// Far more records than the queue can hold, written without draining.
	for i := 0; i < 5000; i++ {
		sink.Write(context.Background(), Record{Timestamp: time.Now(), RequestID: "r"})
	}
	_ = sink.Close()
	// The point is that Write never blocked; drops are acceptable and counted.
	if sink.Dropped() == 0 && sink.Written() == 0 {
		t.Error("neither writes nor drops were recorded")
	}
}

func TestAggregatorRollsUp(t *testing.T) {
	agg := NewAggregator(72, nil)
	base := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		agg.Write(context.Background(), Record{
			Timestamp: base.Add(time.Duration(i) * time.Minute),
			Env:       "prod", Tenant: "fsclient", Team: "payments-risk",
			AgentID: "agt_1", CostCenter: "CC-4471", BackendModel: "azure/gpt",
			InputTokens: 100, OutputTokens: 50, CostUSD: 0.01, Billable: true,
		})
	}
	agg.Write(context.Background(), Record{
		Timestamp: base, Env: "prod", CostCenter: "CC-4471", AgentID: "agt_1",
		BackendModel: "azure/gpt", Cache: "hit", SavingsUSD: 0.02,
	})

	daily := agg.Rollups("day", "CC-4471")
	if len(daily) == 0 {
		t.Fatal("no rollups produced")
	}
	var requests int64
	var cost, savings float64
	var hits int64
	for _, r := range daily {
		requests += r.Requests
		cost += r.CostUSD
		savings += r.SavingsUSD
		hits += r.CacheHits
	}
	if requests != 4 {
		t.Errorf("requests = %d, want 4", requests)
	}
	if math.Abs(cost-0.03) > 1e-9 {
		t.Errorf("cost = %f, want 0.03", cost)
	}
	if math.Abs(savings-0.02) > 1e-9 {
		t.Errorf("savings = %f, want 0.02", savings)
	}
	if hits != 1 {
		t.Errorf("cache hits = %d, want 1", hits)
	}
	if other := agg.Rollups("day", "CC-9999"); len(other) != 0 {
		t.Error("rollups must be filterable by cost centre")
	}
}

func TestAnomalyDetectorFlagsHourlySpike(t *testing.T) {
	var flags []string
	d := NewAnomalyDetector(0.3, 3, 3, func(kind string, r Record, observed, expected float64) {
		flags = append(flags, kind)
	})
	base := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	// Six quiet hours establish the baseline.
	for h := 0; h < 6; h++ {
		d.Observe(Record{Timestamp: base.Add(time.Duration(h) * time.Hour), Env: "prod", AgentID: "agt_1", CostUSD: 1})
	}
	// The seventh hour is wildly out of character; the flag is raised when the
	// following hour closes the window.
	d.Observe(Record{Timestamp: base.Add(6 * time.Hour), Env: "prod", AgentID: "agt_1", CostUSD: 500})
	d.Observe(Record{Timestamp: base.Add(7 * time.Hour), Env: "prod", AgentID: "agt_1", CostUSD: 1})

	found := false
	for _, f := range flags {
		if f == "hourly_spend" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an hourly_spend anomaly, got %v", flags)
	}
}

func TestAnomalyDetectorFlagsDailyCeilingOnce(t *testing.T) {
	var flags int
	d := NewAnomalyDetector(0.3, 3, 3, func(kind string, _ Record, _, _ float64) {
		if kind == "daily_ceiling" {
			flags++
		}
	})
	d.SetCeiling("CC-4471", 10)
	base := time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 20; i++ {
		d.Observe(Record{Timestamp: base, Env: "prod", AgentID: "agt_1", CostCenter: "CC-4471", CostUSD: 1})
	}
	if flags != 1 {
		t.Errorf("the ceiling must be flagged exactly once per crossing, got %d", flags)
	}
	if spend := d.DailySpend("prod", "CC-4471", base); math.Abs(spend-20) > 1e-9 {
		t.Errorf("daily spend = %f, want 20", spend)
	}
}

func TestRecordKeyGroupsByAttribution(t *testing.T) {
	a := Record{Env: "prod", Tenant: "t", Team: "team", AgentID: "agt_1", CostCenter: "CC", BackendModel: "m"}
	b := a
	if a.Key() != b.Key() {
		t.Error("identical attribution must produce the same rollup key")
	}
	b.CostCenter = "OTHER"
	if a.Key() == b.Key() {
		t.Error("the cost centre must be part of the rollup key")
	}
}

func TestMultiSinkFansOut(t *testing.T) {
	dir := t.TempDir()
	one, _ := NewFileSink(FileSinkOptions{Dir: filepath.Join(dir, "a")})
	two, _ := NewFileSink(FileSinkOptions{Dir: filepath.Join(dir, "b")})
	m := MultiSink{Sinks: []Sink{one, two}}
	m.Write(context.Background(), Record{Timestamp: time.Now(), RequestID: "r"})
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"a", "b"} {
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		if err != nil || len(entries) == 0 {
			t.Errorf("sink %s received nothing", sub)
		}
	}
}
