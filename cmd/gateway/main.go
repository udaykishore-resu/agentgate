// Command gateway runs the AgentGate traffic plane: the provider-agnostic
// model API that every agent calls.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/agentgate/agentgate/internal/cache"
	"github.com/agentgate/agentgate/internal/config"
	"github.com/agentgate/agentgate/internal/cost"
	"github.com/agentgate/agentgate/internal/gateway"
	"github.com/agentgate/agentgate/internal/httpx"
	"github.com/agentgate/agentgate/internal/identity"
	"github.com/agentgate/agentgate/internal/ratelimit"
	"github.com/agentgate/agentgate/internal/telemetry"
	"github.com/agentgate/agentgate/internal/version"
	"github.com/redis/go-redis/v9"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath   = flag.String("config", envOr("AGENTGATE_CONFIG", "config/gateway.yaml"), "path to the gateway configuration file")
		showVersion  = flag.Bool("version", false, "print the version and exit")
		validateOnly = flag.Bool("validate", false, "validate the configuration and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	logger := telemetry.NewLogger(os.Stdout, cfg.Service.LogLevel, cfg.Service.Name, cfg.Env)

	if v := cfg.Validate(); v != nil {
		for _, w := range v.Warnings() {
			logger.Warn("configuration warning", "path", w.Path, "detail", w.Message)
		}
		if v.HasErrors() {
			return v
		}
	}
	if *validateOnly {
		fmt.Printf("configuration at %s is valid for env=%s: %d pools, %d models\n",
			*configPath, cfg.Env, len(cfg.Pools), len(cfg.Models))
		return nil
	}
	logger.Info("starting", "version", version.Version, "commit", version.Commit,
		"env", cfg.Env, "pools", len(cfg.Pools), "models", len(cfg.Models))

	ctx := context.Background()

	tp := telemetry.New(telemetry.Config{
		ServiceName:      cfg.Service.Name,
		ServiceNamespace: cfg.Telemetry.Namespace,
		Environment:      cfg.Env,
		OTLPEndpoint:     cfg.Telemetry.OTLPEndpoint,
		OTLPHeaders:      cfg.Telemetry.OTLPHeaders,
		OTLPInsecure:     cfg.Telemetry.OTLPInsecure,
		OTLPTimeout:      cfg.Telemetry.OTLPTimeout.Or(10 * time.Second),
		SampleRatio:      cfg.Telemetry.SampleRatio,
		BatchSize:        cfg.Telemetry.BatchSize,
		BatchTimeout:     cfg.Telemetry.BatchTimeout.Or(5 * time.Second),
		QueueSize:        cfg.Telemetry.QueueSize,
		ContentCapture:   telemetry.ParseContentCapture(cfg.Telemetry.ContentCapture),
		Logger:           logger,
	})
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := tp.Shutdown(shutdownCtx); err != nil {
			logger.Warn("telemetry shutdown incomplete", "error", err)
		}
	}()
	metrics := telemetry.NewInstruments(tp.Metrics)

	// Identity. The JWKS cache is warmed before the first request so that a
	// cold start does not spend its first requests fetching keys.
	var verifier *identity.Verifier
	if !cfg.Identity.AllowUnverified {
		jwks := identity.NewJWKSCache(identity.JWKSOptions{
			URL:      cfg.Identity.JWKSURL,
			Refresh:  cfg.Identity.JWKSRefresh.Or(10 * time.Minute),
			MaxStale: cfg.Identity.JWKSMaxStale.Or(24 * time.Hour),
			OnRefresh: func(result string) {
				metrics.JWKSRefresh.Inc(cfg.Env, result)
			},
		})
		jwks.Start(ctx)
		verifier = identity.NewVerifier(identity.VerifierOptions{
			Issuer: cfg.Identity.Issuer, Audience: cfg.Identity.Audience,
			JWKS: jwks, ClockSkew: cfg.Identity.ClockSkew.Or(30 * time.Second),
			OnValidate: func(result, reason string) {
				metrics.TokenValidations.Inc(cfg.Env, result, reason)
			},
		})
	} else {
		logger.Warn("token verification is disabled; every caller is trusted")
	}

	// Rate limiting. Redis is the enforcement point; the in-memory limiter is
	// the documented degraded mode rather than a silent fallback.
	memLimiter := ratelimit.NewMemory()
	var limiter ratelimit.Limiter = memLimiter
	if cfg.Gateway.RateLimiter == "redis" {
		rdb := redis.NewClient(&redis.Options{
			Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: cfg.Redis.DB,
			DialTimeout: cfg.Redis.DialTimeout.Or(2 * time.Second),
			ReadTimeout: cfg.Redis.ReadTimeout.Or(500 * time.Millisecond),
			PoolSize:    orInt(cfg.Redis.PoolSize, 64),
		})
		distributed := ratelimit.NewRedis(ratelimit.RedisOptions{Client: rdb})
		limiter = ratelimit.NewFallback(distributed, memLimiter, logger, func(degraded bool) {
			state := "0"
			if degraded {
				state = "1"
			}
			logger.Warn("rate limiter degradation state changed", "degraded", state)
		})
	}
	defer func() { _ = limiter.Close() }()

	// Chargeback.
	var usage cost.Sink
	if cfg.Chargeback.Enabled {
		fileSink, err := cost.NewFileSink(cost.FileSinkOptions{
			Dir: cfg.Chargeback.Dir, QueueSize: cfg.Chargeback.QueueSize, Logger: logger,
			OnWrite: func(result string) { metrics.UsageRecords.Inc(cfg.Env, result) },
		})
		if err != nil {
			return fmt.Errorf("usage sink: %w", err)
		}
		defer func() { _ = fileSink.Close() }()
		usage = fileSink
	}

	guardrailProviders, err := gateway.NewGuardrailProviders(cfg)
	if err != nil {
		return err
	}
	router, err := gateway.NewRouter(cfg, gateway.NewBackendFactory(cfg.Env))
	if err != nil {
		return err
	}

	srv, err := gateway.New(gateway.Options{
		Config: cfg, Logger: logger, Telemetry: tp, Router: router,
		Verifier: verifier, Limiter: limiter,
		Cache:  cache.NewMemory(cfg.Cache.MaxBytes),
		Prices: gateway.NewPriceBook(cfg), Usage: usage,
		Guardrails: guardrailProviders,
	})
	if err != nil {
		return err
	}
	defer func() { _ = srv.Close() }()

	// Readiness is only signalled once the backends have been probed, so the
	// first request never arrives before the gateway knows where to send it.
	go func() {
		probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		health := router.Health(probeCtx)
		reachable := 0
		for name, status := range health {
			if status == "ok" {
				reachable++
			} else {
				logger.Warn("backend probe failed at startup", "backend", name, "status", status)
			}
		}
		logger.Info("backend probe complete", "reachable", reachable, "total", len(health))
		srv.MarkReady()
	}()

	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", tp.Metrics.Handler())

	return httpx.Run(ctx, logger, cfg.Service.ShutdownTimeout.Or(30*time.Second),
		func() { _ = srv.Close() },
		httpx.ServerSpec{
			Name: "gateway", Addr: cfg.Service.HTTPAddr, Handler: srv.Handler(),
			ReadTimeout: cfg.Service.ReadTimeout.Or(30 * time.Second),
			// No write timeout: a streaming completion may legitimately run
			// for minutes and the per-request deadline governs instead.
			WriteTimeout: 0,
			IdleTimeout:  cfg.Service.IdleTimeout.Or(120 * time.Second),
		},
		httpx.ServerSpec{Name: "metrics", Addr: cfg.Service.MetricsAddr, Handler: metricsMux},
	)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func orInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}
