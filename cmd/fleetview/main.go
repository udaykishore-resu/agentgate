// Command fleetview runs the AgentGate observability plane: the fleet
// inventory, telemetry-trust metrics, SLO and error-budget reporting,
// chargeback and the dashboard.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/agentgate/agentgate/internal/cost"
	"github.com/agentgate/agentgate/internal/fleet"
	"github.com/agentgate/agentgate/internal/httpx"
	"github.com/agentgate/agentgate/internal/registry"
	"github.com/agentgate/agentgate/internal/telemetry"
	"github.com/agentgate/agentgate/internal/version"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr        = flag.String("addr", envOr("AGENTGATE_HTTP_ADDR", ":8082"), "HTTP listen address")
		metricsAddr = flag.String("metrics-addr", envOr("AGENTGATE_METRICS_ADDR", ":9092"), "metrics listen address")
		env         = flag.String("env", envOr("AGENTGATE_ENV", "dev"), "deployment environment")
		statePath   = flag.String("state", envOr("AGENTGATE_STATE_PATH", "./data/registry.json"), "registry snapshot path")
		usageDir    = flag.String("usage-dir", envOr("AGENTGATE_USAGE_DIR", "./data/usage"), "usage record directory")
		promURL     = flag.String("prometheus", envOr("AGENTGATE_PROM_URL", "http://prometheus:9090"), "Prometheus query API base URL")
		otlp        = flag.String("otlp", os.Getenv("AGENTGATE_OTLP_ENDPOINT"), "OTLP HTTP endpoint for this service's own traces")
		logLevel    = flag.String("log-level", envOr("AGENTGATE_LOG_LEVEL", "info"), "log level")
		sloWindow   = flag.Duration("slo-window", 28*24*time.Hour, "SLO evaluation window")
		trustWindow = flag.Duration("trust-window", 5*time.Minute, "telemetry trust aggregation window")
		minComplete = flag.Float64("min-completeness", floatOr("AGENTGATE_MIN_COMPLETENESS", 0.98), "minimum acceptable trace completeness")
		ceilings    = flag.String("daily-ceilings", os.Getenv("AGENTGATE_DAILY_CEILINGS"), "comma-separated COST_CENTRE=USD hard daily spend ceilings")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println(version.String())
		return nil
	}

	logger := telemetry.NewLogger(os.Stdout, *logLevel, "agentgate-fleetview", *env)
	ctx := context.Background()

	tp := telemetry.New(telemetry.Config{
		ServiceName: "agentgate-fleetview", ServiceNamespace: "agentgate",
		Environment: *env, OTLPEndpoint: *otlp, OTLPInsecure: true, Logger: logger,
	})
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tp.Shutdown(sctx)
	}()
	metrics := telemetry.NewInstruments(tp.Metrics)

	store, err := registry.NewMemoryStore(*statePath)
	if err != nil {
		return fmt.Errorf("registry store: %w", err)
	}

	anomaly := cost.NewAnomalyDetector(0.3, 3, 6, func(kind string, r cost.Record, observed, expected float64) {
		metrics.CostAnomalies.Inc(r.Env, r.Tenant, r.Team, r.AgentID)
		logger.Warn("cost anomaly detected",
			"kind", kind, "agent", r.AgentIdentity, "cost_center", r.CostCenter,
			"observed_usd", observed, "expected_usd", expected, "env", r.Env)
	})
	for _, pair := range strings.Split(*ceilings, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		cc, amount, ok := strings.Cut(pair, "=")
		if !ok {
			return fmt.Errorf("-daily-ceilings entries must be COST_CENTRE=USD, got %q", pair)
		}
		v, err := strconv.ParseFloat(amount, 64)
		if err != nil {
			return fmt.Errorf("invalid ceiling for %s: %w", cc, err)
		}
		anomaly.SetCeiling(cc, v)
		logger.Info("daily spend ceiling configured", "cost_center", cc, "usd", v)
	}

	aggregator := cost.NewAggregator(72, anomaly)
	trust := fleet.NewTrustTracker(*trustWindow, metrics, *env)

	svc := fleet.New(fleet.Options{
		Env: *env, Store: store, Prom: fleet.NewPromClient(*promURL, nil),
		Trust: trust, Aggregator: aggregator, Anomaly: anomaly, Metrics: metrics,
		Logger: logger, UsageDir: *usageDir, SLOWindow: *sloWindow,
		MinCompleteness: *minComplete,
	})

	// The usage journal is replayed on start so that spend figures survive a
	// restart. In a deployed environment the warehouse is the system of record
	// and this is a warm cache; locally it is the whole story.
	go replayUsage(*usageDir, aggregator, logger)

	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", tp.Metrics.Handler())

	logger.Info("fleet view starting", "version", version.Version, "env", *env,
		"prometheus", *promURL, "slo_window", sloWindow.String())

	return httpx.Run(ctx, logger, 20*time.Second, nil,
		httpx.ServerSpec{Name: "fleetview", Addr: *addr, Handler: svc.Handler()},
		httpx.ServerSpec{Name: "metrics", Addr: *metricsAddr, Handler: metricsMux},
	)
}

func replayUsage(dir string, agg *cost.Aggregator, logger interface{ Info(string, ...any) }) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		raw, err := os.ReadFile(dir + "/" + e.Name())
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var rec cost.Record
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				continue
			}
			agg.Write(context.Background(), rec)
			n++
		}
	}
	logger.Info("usage journal replayed", "records", n)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func floatOr(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}
