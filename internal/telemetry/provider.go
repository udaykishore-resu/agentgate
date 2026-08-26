package telemetry

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agentgate/agentgate/internal/version"
)

// Exporter ships completed spans somewhere durable.
type Exporter interface {
	ExportSpans(ctx context.Context, spans []Snapshot) error
	Shutdown(ctx context.Context) error
}

// Config configures the telemetry provider.
type Config struct {
	// ServiceName, ServiceNamespace and Environment become resource attributes.
	ServiceName      string
	ServiceNamespace string
	Environment      string
	InstanceID       string

	// OTLPEndpoint is the collector base URL, e.g. http://otel-collector:4318.
	// Empty disables export; spans are still created so that trace ids appear
	// in logs and response headers.
	OTLPEndpoint string
	OTLPHeaders  map[string]string
	OTLPTimeout  time.Duration
	OTLPInsecure bool

	// SampleRatio is the head sampling ratio for traces started here. Keep it
	// at 1.0 and let the collector tail-sample; see ADR 0007.
	SampleRatio float64

	// BatchSize, BatchTimeout and QueueSize tune the span pipeline.
	BatchSize    int
	BatchTimeout time.Duration
	QueueSize    int

	// ContentCapture controls prompt/completion capture as span events.
	ContentCapture ContentCapture

	// ResourceAttributes are merged into the resource, letting a deployment
	// add cloud, cluster or region attributes without a code change.
	ResourceAttributes []KeyValue

	// Logger receives pipeline diagnostics. Never nil after New.
	Logger *slog.Logger
}

func (c *Config) applyDefaults() {
	if c.OTLPTimeout <= 0 {
		c.OTLPTimeout = 10 * time.Second
	}
	if c.SampleRatio == 0 {
		c.SampleRatio = 1
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 512
	}
	if c.BatchTimeout <= 0 {
		c.BatchTimeout = 5 * time.Second
	}
	if c.QueueSize <= 0 {
		c.QueueSize = 8192
	}
	if c.InstanceID == "" {
		host, _ := os.Hostname()
		c.InstanceID = host
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.ContentCapture == "" {
		c.ContentCapture = CaptureOff
	}
}

// Provider owns the span pipeline: resource, sampler, queue, batcher and
// exporter. One per process.
type Provider struct {
	cfg      Config
	resource []KeyValue
	sampler  sampler
	exporter Exporter

	queue   chan Snapshot
	wg      sync.WaitGroup
	stop    chan struct{}
	stopped atomic.Bool

	dropped atomic.Int64
	sent    atomic.Int64
	failed  atomic.Int64

	Metrics *Registry
}

// New builds a Provider and starts its batch processor.
func New(cfg Config) *Provider {
	cfg.applyDefaults()

	res := []KeyValue{
		Attr(AttrServiceName, cfg.ServiceName),
		Attr(AttrServiceVersion, version.Version),
		Attr(AttrServiceNamespace, cfg.ServiceNamespace),
		Attr(AttrServiceInstanceID, cfg.InstanceID),
		Attr(AttrDeploymentEnv, cfg.Environment),
		Attr(AttrTelemetrySDKName, "agentgate"),
		Attr(AttrTelemetrySDKLang, "go"),
		Attr(AttrTelemetrySDKVer, version.Version),
	}
	if host, err := os.Hostname(); err == nil {
		res = append(res, Attr(AttrHostName, host))
	}
	for _, k := range []string{"K8S_POD_NAME", "K8S_NAMESPACE", "CLOUD_PROVIDER", "CLOUD_REGION"} {
		if v := os.Getenv(k); v != "" {
			switch k {
			case "K8S_POD_NAME":
				res = append(res, Attr(AttrK8sPodName, v))
			case "K8S_NAMESPACE":
				res = append(res, Attr(AttrK8sNamespace, v))
			case "CLOUD_PROVIDER":
				res = append(res, Attr(AttrCloudProvider, v))
			case "CLOUD_REGION":
				res = append(res, Attr(AttrCloudRegion, v))
			}
		}
	}
	res = append(res, cfg.ResourceAttributes...)

	p := &Provider{
		cfg:      cfg,
		resource: res,
		sampler:  sampler{ratio: cfg.SampleRatio},
		queue:    make(chan Snapshot, cfg.QueueSize),
		stop:     make(chan struct{}),
		Metrics:  NewRegistry(res),
	}
	if cfg.OTLPEndpoint != "" {
		p.exporter = newOTLPExporter(cfg, res)
	}
	p.wg.Add(1)
	go p.run()
	return p
}

// Tracer returns a tracer for an instrumentation scope.
func (p *Provider) Tracer(name string) *Tracer { return &Tracer{name: name, provider: p} }

// Resource returns the process resource attributes.
func (p *Provider) Resource() []KeyValue { return p.resource }

// ContentCapture reports the configured content-capture mode.
func (p *Provider) ContentCapture() ContentCapture { return p.cfg.ContentCapture }

// Stats reports pipeline health. Exposed as metrics so that the observability
// plane can be observed; a plane that cannot report its own drops is not
// trustworthy.
func (p *Provider) Stats() (sent, failed, dropped int64) {
	return p.sent.Load(), p.failed.Load(), p.dropped.Load()
}

func (p *Provider) enqueue(s Snapshot) {
	if p.stopped.Load() {
		return
	}
	if !s.Context.IsSampled() {
		return
	}
	select {
	case p.queue <- s:
	default:
		// Never block the request path on telemetry. Dropped spans are counted
		// and alerted on rather than paid for in latency.
		p.dropped.Add(1)
	}
}

func (p *Provider) run() {
	defer p.wg.Done()
	ticker := time.NewTicker(p.cfg.BatchTimeout)
	defer ticker.Stop()

	batch := make([]Snapshot, 0, p.cfg.BatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		out := make([]Snapshot, len(batch))
		copy(out, batch)
		batch = batch[:0]
		p.export(out)
	}

	for {
		select {
		case s := <-p.queue:
			batch = append(batch, s)
			if len(batch) >= p.cfg.BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-p.stop:
			// Drain whatever is queued before exiting.
			for {
				select {
				case s := <-p.queue:
					batch = append(batch, s)
					if len(batch) >= p.cfg.BatchSize {
						flush()
					}
					continue
				default:
				}
				break
			}
			flush()
			return
		}
	}
}

func (p *Provider) export(batch []Snapshot) {
	if p.exporter == nil {
		p.sent.Add(int64(len(batch)))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.OTLPTimeout)
	defer cancel()
	if err := p.exporter.ExportSpans(ctx, batch); err != nil {
		p.failed.Add(int64(len(batch)))
		p.cfg.Logger.Warn("span export failed", "error", err, "spans", len(batch))
		return
	}
	p.sent.Add(int64(len(batch)))
}

// Shutdown drains the queue and closes the exporter.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p.stopped.Swap(true) {
		return nil
	}
	close(p.stop)
	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if p.exporter != nil {
		return p.exporter.Shutdown(ctx)
	}
	return nil
}
