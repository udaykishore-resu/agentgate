// Command guardrails runs a content-safety callout service implementing the
// AgentGate guardrail contract.
//
// It exists for three reasons: the local stack needs a real callout target so
// the callout path is exercised rather than assumed; the client's own
// moderation service can be dropped in behind the same contract; and the
// contract itself needs a reference implementation that documents what a
// compliant response looks like.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/agentgate/agentgate/internal/guardrails"
	"github.com/agentgate/agentgate/internal/httpx"
	"github.com/agentgate/agentgate/internal/telemetry"
	"github.com/agentgate/agentgate/internal/version"
)

type calloutRequest struct {
	Text               string   `json:"text"`
	Direction          string   `json:"direction"`
	Pool               string   `json:"pool"`
	Tenant             string   `json:"tenant"`
	AgentID            string   `json:"agent_id"`
	DataClassification string   `json:"data_classification"`
	Categories         []string `json:"categories"`
}

type finding struct {
	Category   string  `json:"category"`
	Severity   string  `json:"severity"`
	Confidence float64 `json:"confidence"`
	Action     string  `json:"action"`
	Offset     int     `json:"offset"`
}

type calloutResponse struct {
	Action   string    `json:"action"`
	Text     string    `json:"text,omitempty"`
	Findings []finding `json:"findings"`
}

func main() {
	var (
		addr        = flag.String("addr", envOr("AGENTGATE_HTTP_ADDR", ":8083"), "HTTP listen address")
		env         = flag.String("env", envOr("AGENTGATE_ENV", "dev"), "deployment environment")
		mode        = flag.String("mode", envOr("AGENTGATE_GUARDRAIL_MODE", "builtin"), "builtin or permissive")
		latency     = flag.Duration("latency", durationOr("GUARDRAIL_LATENCY", 0), "artificial latency, for load testing")
		failureRate = flag.Float64("failure-rate", floatOr("GUARDRAIL_FAILURE_RATE", 0), "fraction of requests to fail, for resilience testing")
		logLevel    = flag.String("log-level", envOr("AGENTGATE_LOG_LEVEL", "info"), "log level")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println(version.String())
		return
	}

	logger := telemetry.NewLogger(os.Stdout, *logLevel, "agentgate-guardrails", *env)
	detector := guardrails.NewBuiltin()
	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "mode": *mode, "version": version.Get()})
	})
	mux.HandleFunc("POST /v1/inspect", func(w http.ResponseWriter, r *http.Request) {
		if *latency > 0 {
			time.Sleep(*latency)
		}
		if *failureRate > 0 && rnd.Float64() < *failureRate {
			// A deliberate 503 so the gateway's breaker and failure-mode
			// handling can be exercised under load.
			http.Error(w, "injected failure", http.StatusServiceUnavailable)
			return
		}
		var req calloutRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if *mode == "permissive" {
			httpx.WriteJSON(w, http.StatusOK, calloutResponse{Action: "allow"})
			return
		}

		policy := guardrails.DefaultPolicy(req.DataClassification)
		d, err := detector.Inspect(r.Context(), guardrails.Request{
			Text: req.Text, Direction: guardrails.Direction(req.Direction),
			Pool: req.Pool, Tenant: req.Tenant, AgentID: req.AgentID,
			DataClassification: req.DataClassification, Categories: req.Categories,
		}, policy)
		if err != nil {
			http.Error(w, "inspection failed", http.StatusInternalServerError)
			return
		}
		out := calloutResponse{Action: string(d.Action), Text: d.Text, Findings: []finding{}}
		for _, f := range d.Findings {
			out.Findings = append(out.Findings, finding{
				Category: f.Category, Severity: f.Severity, Confidence: f.Confidence,
				Action: string(f.Action), Offset: f.Offset,
			})
		}
		if len(out.Findings) > 0 {
			logger.Info("guardrail findings",
				"pool", req.Pool, "tenant", req.Tenant, "direction", req.Direction,
				"action", out.Action, "categories", categories(d))
		}
		httpx.WriteJSON(w, http.StatusOK, out)
	})

	logger.Info("guardrail service starting", "addr", *addr, "mode", *mode,
		"injected_latency", latency.String(), "injected_failure_rate", *failureRate)
	if err := httpx.Run(context.Background(), logger, 10*time.Second, nil,
		httpx.ServerSpec{Name: "guardrails", Addr: *addr, Handler: mux}); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func categories(d guardrails.Decision) string {
	seen := map[string]bool{}
	var out []string
	for _, f := range d.Findings {
		if !seen[f.Category] {
			seen[f.Category] = true
			out = append(out, f.Category)
		}
	}
	return strings.Join(out, ",")
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

func floatOr(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		var f float64
		if _, err := fmt.Sscanf(v, "%f", &f); err == nil {
			return f
		}
	}
	return def
}
