package cost

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Sink receives usage records. Production writes to an event stream that the
// warehouse consumes; the file sink is what the local stack and a
// disconnected environment use.
//
// Writing must never block the request path. Every implementation here buffers
// and drops on overflow rather than applying backpressure to a model call,
// and every drop is counted so the loss is visible instead of silent.
type Sink interface {
	Write(ctx context.Context, r Record)
	Flush(ctx context.Context) error
	Close() error
	Dropped() int64
}

// FileSink appends newline-delimited JSON records to a file, rotating by day.
type FileSink struct {
	dir     string
	logger  *slog.Logger
	queue   chan Record
	wg      sync.WaitGroup
	stop    chan struct{}
	dropped atomic.Int64
	written atomic.Int64

	mu      sync.Mutex
	file    *os.File
	fileDay string
	onWrite func(result string)
}

// FileSinkOptions configures the file sink.
type FileSinkOptions struct {
	Dir       string
	QueueSize int
	Logger    *slog.Logger
	OnWrite   func(result string)
}

// NewFileSink builds a file-backed usage sink.
func NewFileSink(o FileSinkOptions) (*FileSink, error) {
	if o.QueueSize <= 0 {
		o.QueueSize = 8192
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.OnWrite == nil {
		o.OnWrite = func(string) {}
	}
	if err := os.MkdirAll(o.Dir, 0o750); err != nil {
		return nil, err
	}
	s := &FileSink{
		dir: o.Dir, logger: o.Logger, onWrite: o.OnWrite,
		queue: make(chan Record, o.QueueSize), stop: make(chan struct{}),
	}
	s.wg.Add(1)
	go s.run()
	return s, nil
}

// Write enqueues a record, dropping it rather than blocking when the queue is
// full.
func (s *FileSink) Write(_ context.Context, r Record) {
	select {
	case s.queue <- r:
	default:
		s.dropped.Add(1)
		s.onWrite("dropped")
	}
}

func (s *FileSink) run() {
	defer s.wg.Done()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case r := <-s.queue:
			s.append(r)
		case <-ticker.C:
			s.mu.Lock()
			if s.file != nil {
				_ = s.file.Sync()
			}
			s.mu.Unlock()
		case <-s.stop:
			for {
				select {
				case r := <-s.queue:
					s.append(r)
					continue
				default:
				}
				break
			}
			s.mu.Lock()
			if s.file != nil {
				_ = s.file.Sync()
				_ = s.file.Close()
				s.file = nil
			}
			s.mu.Unlock()
			return
		}
	}
}

func (s *FileSink) append(r Record) {
	raw, err := json.Marshal(r)
	if err != nil {
		s.onWrite("error")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	day := r.Timestamp.UTC().Format("2006-01-02")
	if s.file == nil || s.fileDay != day {
		if s.file != nil {
			_ = s.file.Close()
		}
		f, err := os.OpenFile(filepath.Join(s.dir, "usage-"+day+".jsonl"),
			os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
		if err != nil {
			s.logger.Error("cannot open usage file", "error", err)
			s.onWrite("error")
			return
		}
		s.file, s.fileDay = f, day
	}
	if _, err := s.file.Write(append(raw, '\n')); err != nil {
		s.logger.Error("cannot write usage record", "error", err)
		s.onWrite("error")
		return
	}
	s.written.Add(1)
	s.onWrite("ok")
}

// Flush syncs the current file.
func (s *FileSink) Flush(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	return s.file.Sync()
}

// Close drains the queue and closes the file.
func (s *FileSink) Close() error {
	close(s.stop)
	s.wg.Wait()
	return nil
}

// Dropped reports how many records were lost to a full queue.
func (s *FileSink) Dropped() int64 { return s.dropped.Load() }

// Written reports how many records reached disk.
func (s *FileSink) Written() int64 { return s.written.Load() }

// MultiSink fans records out to several sinks, which is how the platform
// writes to both the durable event stream and a local audit file during
// migration without the gateway knowing about either.
type MultiSink struct{ Sinks []Sink }

// Write forwards to every sink.
func (m MultiSink) Write(ctx context.Context, r Record) {
	for _, s := range m.Sinks {
		s.Write(ctx, r)
	}
}

// Flush flushes every sink.
func (m MultiSink) Flush(ctx context.Context) error {
	var firstErr error
	for _, s := range m.Sinks {
		if err := s.Flush(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Close closes every sink.
func (m MultiSink) Close() error {
	var firstErr error
	for _, s := range m.Sinks {
		if err := s.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Dropped sums drops across sinks.
func (m MultiSink) Dropped() int64 {
	var n int64
	for _, s := range m.Sinks {
		n += s.Dropped()
	}
	return n
}

// Aggregator keeps in-memory rollups so the fleet view can answer "what has
// this team spent today" without querying the warehouse. It is a cache of a
// derived figure, not the system of record.
type Aggregator struct {
	mu       sync.RWMutex
	hourly   map[string]*Rollup
	daily    map[string]*Rollup
	retainH  int
	anomaly  *AnomalyDetector
	onRecord func(Record)
}

// NewAggregator builds an aggregator.
func NewAggregator(retainHours int, anomaly *AnomalyDetector) *Aggregator {
	if retainHours <= 0 {
		retainHours = 72
	}
	return &Aggregator{
		hourly: map[string]*Rollup{}, daily: map[string]*Rollup{},
		retainH: retainHours, anomaly: anomaly,
	}
}

// Write folds a record into the rollups and feeds the anomaly detector.
func (a *Aggregator) Write(_ context.Context, r Record) {
	hourKey := r.Timestamp.UTC().Format("2006-01-02T15") + "|" + r.Key()
	dayKey := r.Timestamp.UTC().Format("2006-01-02") + "|" + r.Key()
	a.mu.Lock()
	a.fold(a.hourly, hourKey, "hour", r, r.Timestamp.UTC().Truncate(time.Hour), time.Hour)
	a.fold(a.daily, dayKey, "day", r, r.Timestamp.UTC().Truncate(24*time.Hour), 24*time.Hour)
	a.mu.Unlock()
	if a.anomaly != nil {
		a.anomaly.Observe(r)
	}
	if a.onRecord != nil {
		a.onRecord(r)
	}
}

func (a *Aggregator) fold(m map[string]*Rollup, key, period string, r Record, start time.Time, d time.Duration) {
	roll, ok := m[key]
	if !ok {
		roll = &Rollup{
			Period: period, Start: start, End: start.Add(d),
			Env: r.Env, Tenant: r.Tenant, Team: r.Team, AgentID: r.AgentID, CostCenter: r.CostCenter,
		}
		m[key] = roll
	}
	roll.Requests++
	roll.InputTokens += int64(r.InputTokens)
	roll.OutputTokens += int64(r.OutputTokens)
	roll.CostUSD += r.CostUSD
	roll.SavingsUSD += r.SavingsUSD
	if r.Cache == "hit" || r.Cache == "semantic_hit" {
		roll.CacheHits++
	}
	if r.ErrorCode != "" {
		roll.Errors++
	}
}

// Rollups returns rollups for a period, optionally filtered by cost centre.
func (a *Aggregator) Rollups(period, costCenter string) []Rollup {
	a.mu.RLock()
	defer a.mu.RUnlock()
	src := a.daily
	if period == "hour" {
		src = a.hourly
	}
	out := make([]Rollup, 0, len(src))
	for _, r := range src {
		if costCenter != "" && r.CostCenter != costCenter {
			continue
		}
		out = append(out, *r)
	}
	return out
}

// Flush is a no-op; the aggregator holds derived state only.
func (a *Aggregator) Flush(context.Context) error { return nil }

// Close is a no-op.
func (a *Aggregator) Close() error { return nil }

// Dropped is always zero; the aggregator never drops.
func (a *Aggregator) Dropped() int64 { return 0 }

// AnomalyDetector flags per-agent hourly spend that departs from its own
// recent behaviour, and per-cost-centre daily spend that crosses a hard
// ceiling.
//
// An exponentially-weighted mean and variance rather than a fixed threshold,
// because agents differ by three orders of magnitude in normal spend and a
// single global threshold would either miss the small ones or page constantly
// for the large ones. The hard ceiling exists alongside it because a
// statistical detector cannot catch a first-day runaway that has no history.
type AnomalyDetector struct {
	alpha     float64
	sigmas    float64
	minSample int

	mu      sync.Mutex
	series  map[string]*ewma
	ceiling map[string]float64
	daily   map[string]float64
	onFlag  func(kind string, r Record, observed, expected float64)
}

type ewma struct {
	mean, variance float64
	samples        int
	bucket         string
	accum          float64
}

// NewAnomalyDetector builds a detector.
func NewAnomalyDetector(alpha, sigmas float64, minSample int, onFlag func(kind string, r Record, observed, expected float64)) *AnomalyDetector {
	if alpha <= 0 || alpha >= 1 {
		alpha = 0.3
	}
	if sigmas <= 0 {
		sigmas = 3
	}
	if minSample <= 0 {
		minSample = 6
	}
	if onFlag == nil {
		onFlag = func(string, Record, float64, float64) {}
	}
	return &AnomalyDetector{
		alpha: alpha, sigmas: sigmas, minSample: minSample,
		series: map[string]*ewma{}, ceiling: map[string]float64{}, daily: map[string]float64{},
		onFlag: onFlag,
	}
}

// SetCeiling configures a hard daily spend ceiling for a cost centre.
func (d *AnomalyDetector) SetCeiling(costCenter string, usd float64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ceiling[costCenter] = usd
}

// Ceilings returns the configured ceilings.
func (d *AnomalyDetector) Ceilings() map[string]float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]float64, len(d.ceiling))
	for k, v := range d.ceiling {
		out[k] = v
	}
	return out
}

// Observe folds one record into the detector.
func (d *AnomalyDetector) Observe(r Record) {
	if r.CostUSD <= 0 {
		return
	}
	hour := r.Timestamp.UTC().Format("2006-01-02T15")
	day := r.Timestamp.UTC().Format("2006-01-02")

	d.mu.Lock()
	defer d.mu.Unlock()

	key := r.Env + "|" + r.AgentID
	s, ok := d.series[key]
	if !ok {
		s = &ewma{bucket: hour}
		d.series[key] = s
	}
	if s.bucket != hour {
		// The completed hour is the sample.
		observed := s.accum
		if s.samples >= d.minSample {
			threshold := s.mean + d.sigmas*math.Sqrt(s.variance)
			if observed > threshold && observed > 0.01 {
				d.onFlag("hourly_spend", r, observed, s.mean)
			}
		}
		delta := observed - s.mean
		s.mean += d.alpha * delta
		s.variance = (1 - d.alpha) * (s.variance + d.alpha*delta*delta)
		s.samples++
		s.bucket, s.accum = hour, 0
	}
	s.accum += r.CostUSD

	dayKey := r.Env + "|" + r.CostCenter + "|" + day
	d.daily[dayKey] += r.CostUSD
	if ceiling, ok := d.ceiling[r.CostCenter]; ok && ceiling > 0 {
		if d.daily[dayKey] > ceiling && d.daily[dayKey]-r.CostUSD <= ceiling {
			d.onFlag("daily_ceiling", r, d.daily[dayKey], ceiling)
		}
	}
}

// DailySpend reports today's spend for a cost centre.
func (d *AnomalyDetector) DailySpend(env, costCenter string, day time.Time) float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.daily[env+"|"+costCenter+"|"+day.UTC().Format("2006-01-02")]
}
