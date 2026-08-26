// Command controlplane runs the AgentGate trust plane: agent registration,
// workload identity issuance, token exchange and the promotion gate.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/agentgate/agentgate/internal/controlplane"
	"github.com/agentgate/agentgate/internal/httpx"
	"github.com/agentgate/agentgate/internal/identity"
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
		addr        = flag.String("addr", envOr("AGENTGATE_HTTP_ADDR", ":8081"), "HTTP listen address")
		metricsAddr = flag.String("metrics-addr", envOr("AGENTGATE_METRICS_ADDR", ":9091"), "metrics listen address")
		env         = flag.String("env", envOr("AGENTGATE_ENV", "dev"), "deployment environment")
		issuerURL   = flag.String("issuer", envOr("AGENTGATE_OIDC_ISSUER", "http://localhost:8081"), "token issuer URL")
		audience    = flag.String("audience", envOr("AGENTGATE_TOKEN_AUDIENCE", "https://gateway.agentgate.internal"), "token audience")
		keyPath     = flag.String("signing-key", os.Getenv("AGENTGATE_SIGNING_KEY_PATH"), "PEM RSA signing key; generated in memory when empty")
		statePath   = flag.String("state", envOr("AGENTGATE_STATE_PATH", "./data/registry.json"), "registry snapshot path for the file-backed store")
		secretsPath = flag.String("secrets", envOr("AGENTGATE_SECRETS_PATH", "./data/secrets.json"), "development secret store path")
		fleetURL    = flag.String("fleet-url", os.Getenv("AGENTGATE_FLEET_URL"), "fleet service base URL for promotion signals")
		otlp        = flag.String("otlp", os.Getenv("AGENTGATE_OTLP_ENDPOINT"), "OTLP HTTP endpoint")
		logLevel    = flag.String("log-level", envOr("AGENTGATE_LOG_LEVEL", "info"), "log level")
		tokenTTL    = flag.Duration("token-ttl", durationOr("AGENTGATE_TOKEN_TTL", 15*time.Minute), "access token lifetime")
		anonAdmin   = flag.Bool("allow-anonymous-admin", os.Getenv("AGENTGATE_ALLOW_ANONYMOUS_ADMIN") == "true", "development only: skip management API authentication")
		trustIssuer = flag.String("trusted-subject-issuers", os.Getenv("AGENTGATE_TRUSTED_SUBJECT_ISSUERS"), "comma-separated issuer=jwks_url pairs trusted for token exchange")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println(version.String())
		return nil
	}

	logger := telemetry.NewLogger(os.Stdout, *logLevel, "agentgate-controlplane", *env)
	if *env == "prod" && *anonAdmin {
		return fmt.Errorf("anonymous management access cannot be enabled in production")
	}
	if *env == "prod" && *keyPath == "" {
		return fmt.Errorf("a persistent signing key is required in production; an ephemeral key invalidates every token on restart")
	}

	ctx := context.Background()
	tp := telemetry.New(telemetry.Config{
		ServiceName: "agentgate-controlplane", ServiceNamespace: "agentgate",
		Environment: *env, OTLPEndpoint: *otlp, OTLPInsecure: true, Logger: logger,
	})
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tp.Shutdown(sctx)
	}()
	metrics := telemetry.NewInstruments(tp.Metrics)

	issuer := identity.NewIssuer(identity.IssuerOptions{
		Issuer: *issuerURL, Audience: *audience, TTL: *tokenTTL,
		OnMint: func(result, grant string) { metrics.TokenExchange.Inc(*env, result, grant) },
	})
	if *keyPath != "" {
		if _, err := issuer.LoadKeyPEM(*keyPath, true); err != nil {
			return fmt.Errorf("load signing key: %w", err)
		}
		logger.Info("signing key loaded", "path", *keyPath, "kid", issuer.ActiveKeyID())
	} else {
		if _, err := issuer.GenerateKey(2048); err != nil {
			return err
		}
		logger.Warn("using an ephemeral signing key; tokens will not survive a restart",
			"kid", issuer.ActiveKeyID())
	}

	store, err := registry.NewMemoryStore(*statePath)
	if err != nil {
		return fmt.Errorf("registry store: %w", err)
	}
	defer func() { _ = store.Close() }()

	secrets, err := identity.NewFileSecretStore(*secretsPath)
	if err != nil {
		return fmt.Errorf("secret store: %w", err)
	}

	// Promotion signals come from the observability plane. Without it the gate
	// cannot evaluate telemetry health, and in anything above development that
	// must block a promotion rather than wave it through.
	var signals registry.SignalSource
	switch {
	case *fleetURL != "":
		signals = controlplane.RemoteSignals{BaseURL: strings.TrimRight(*fleetURL, "/")}
	case *env == "dev":
		logger.Warn("no fleet service configured; promotion gates will evaluate against synthetic signals")
		signals = registry.StaticSignals{S: registry.AgentSignals{
			RequestCount: 500, TelemetryCompleteness: 1, SuccessRatio: 1,
			SuccessObjective: 0.99, ObservedAttestation: identity.AttestWorkloadIdentity,
			WindowSeconds: int64((24 * time.Hour).Seconds()),
		}}
	default:
		return fmt.Errorf("-fleet-url is required outside development; the promotion gate cannot evaluate telemetry health without it")
	}

	svc := registry.NewService(registry.ServiceOptions{
		Store: store, Issuer: issuer, Secrets: secrets, Signals: signals,
		Change:          registry.LogChangeSink{Logger: logger},
		Gate:            registry.DefaultGateConfig(),
		DefaultTokenTTL: *tokenTTL,
		Logger:          logger,
		OnGateEvaluation: func(from, to identity.Environment, gate, result string) {
			metrics.PromotionGate.Inc(*env, string(from), string(to), gate, result)
		},
	})

	subjectVerifiers := map[string]*identity.Verifier{}
	for _, pair := range strings.Split(*trustIssuer, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		iss, jwksURL, ok := strings.Cut(pair, "=")
		if !ok {
			return fmt.Errorf("-trusted-subject-issuers entries must be issuer=jwks_url, got %q", pair)
		}
		cache := identity.NewJWKSCache(identity.JWKSOptions{URL: jwksURL})
		cache.Start(ctx)
		subjectVerifiers[iss] = identity.NewVerifier(identity.VerifierOptions{
			Issuer: iss, JWKS: cache, AllowGenericClaims: true,
			// A subject token is presented once, to obtain an AgentGate token.
			// Refusing a replay closes the window in which a leaked platform
			// token could be exchanged twice.
			SingleUseWindow: 5 * time.Minute,
		})
		logger.Info("trusting subject token issuer", "issuer", iss, "jwks", jwksURL)
	}

	adminVerifier := identity.NewVerifier(identity.VerifierOptions{
		Issuer: *issuerURL, Audience: *audience,
		StaticKeys: issuer.PublicKeys(), AllowGenericClaims: true,
	})

	srv := controlplane.New(controlplane.Options{
		Env: *env, Service: svc, Store: store, Issuer: issuer, Logger: logger,
		Metrics: metrics, Tracer: tp.Tracer("agentgate/controlplane"),
		SubjectVerifiers: subjectVerifiers, AdminVerifier: adminVerifier,
		AllowAnonymousAdmin: *anonAdmin, TokenTTL: *tokenTTL,
	})

	go reportKeyAge(ctx, issuer, metrics, *env)

	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", tp.Metrics.Handler())

	logger.Info("control plane starting", "version", version.Version, "env", *env,
		"issuer", *issuerURL, "token_ttl", tokenTTL.String())

	return httpx.Run(ctx, logger, 30*time.Second, nil,
		httpx.ServerSpec{Name: "controlplane", Addr: *addr, Handler: srv.Handler()},
		httpx.ServerSpec{Name: "metrics", Addr: *metricsAddr, Handler: metricsMux},
	)
}

// reportKeyAge publishes signing key age so that a key which has quietly
// stopped rotating is visible before it becomes an incident.
func reportKeyAge(ctx context.Context, issuer *identity.Issuer, m *telemetry.Instruments, env string) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, k := range issuer.Keys() {
				m.JWKSKeyAge.Set(time.Since(k.CreatedAt).Seconds(), env, k.Kid)
			}
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func durationOr(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
