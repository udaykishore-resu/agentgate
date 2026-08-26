package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agentgate/agentgate/internal/identity"
	"github.com/agentgate/agentgate/internal/registry"
	"github.com/agentgate/agentgate/internal/telemetry"
)

// otlpTrace builds an OTLP/HTTP JSON trace payload the way the collector
// forwards one, so the trust tracker is tested against the real wire shape.
func otlpTrace(agentID, env string, spans []map[string]string, resourceOverrides map[string]string) string {
	attrs := map[string]string{
		telemetry.AttrServiceName:   "dispute-triage",
		telemetry.AttrDeploymentEnv: env,
		telemetry.AttrAgentID:       agentID,
		telemetry.AttrAgentIdentity: "agent://fsclient/payments-risk/dispute-triage",
		telemetry.AttrTeamID:        "payments-risk",
		telemetry.AttrCostCenter:    "CC-4471",
		telemetry.AttrRuntime:       "aks",
	}
	for k, v := range resourceOverrides {
		if v == "" {
			delete(attrs, k)
			continue
		}
		attrs[k] = v
	}
	var resource []string
	for k, v := range attrs {
		resource = append(resource, fmt.Sprintf(`{"key":%q,"value":{"stringValue":%q}}`, k, v))
	}
	var encoded []string
	for _, s := range spans {
		encoded = append(encoded, fmt.Sprintf(
			`{"traceId":%q,"spanId":%q,"parentSpanId":%q,"name":%q,"startTimeUnixNano":%q,"attributes":[]}`,
			s["trace"], s["span"], s["parent"], s["name"],
			strconv.FormatInt(time.Now().UnixNano(), 10)))
	}
	return fmt.Sprintf(`{"resourceSpans":[{"resource":{"attributes":[%s]},"scopeSpans":[{"spans":[%s]}]}]}`,
		strings.Join(resource, ","), strings.Join(encoded, ","))
}

func TestTrustTrackerIngestsOTLPAndComputesCompleteness(t *testing.T) {
	tracker := NewTrustTracker(time.Millisecond, nil, "prod")
	srv := httptest.NewServer(tracker.Handler())
	defer srv.Close()

	// Four root spans arrive; the gateway saw five requests.
	var spans []map[string]string
	for i := 0; i < 4; i++ {
		spans = append(spans, map[string]string{
			"trace": fmt.Sprintf("%032x", i+1), "span": fmt.Sprintf("%016x", i+1),
			"parent": "", "name": "agent.invoke",
		})
	}
	resp, err := srv.Client().Post(srv.URL, "application/json", strings.NewReader(otlpTrace("agt_1", "prod", spans, nil)))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "spansAccepted") {
		t.Errorf("receiver response = %s", raw)
	}

	tracker.RecordExpected("agt_1", "prod", 5)
	time.Sleep(3 * time.Millisecond)

	stats := tracker.Stats()
	if len(stats) == 0 {
		t.Fatal("no trust statistics were produced")
	}
	var got TrustStats
	for _, s := range stats {
		if s.AgentID == "agt_1" {
			got = s
		}
	}
	if got.SpansReceived != 4 {
		t.Errorf("spans received = %d, want 4", got.SpansReceived)
	}
	if got.Completeness < 0.79 || got.Completeness > 0.81 {
		t.Errorf("completeness = %f, want about 0.8 (4 roots against 5 requests)", got.Completeness)
	}
	if got.Healthy(0.98) {
		t.Error("an agent at 80% completeness must not be reported as healthy")
	}
}

func TestTrustTrackerFlagsUnattributedSpans(t *testing.T) {
	tracker := NewTrustTracker(time.Millisecond, nil, "prod")
	spans := []map[string]string{{"trace": strings.Repeat("a", 32), "span": strings.Repeat("b", 16), "name": "agent.invoke"}}

	var payload otlpPayload
	// A resource with no cost centre cannot be charged back.
	body := otlpTrace("agt_2", "prod", spans, map[string]string{telemetry.AttrCostCenter: ""})
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatal(err)
	}
	tracker.Ingest(payload, time.Now())
	tracker.RecordExpected("agt_2", "prod", 1)
	time.Sleep(3 * time.Millisecond)

	got, ok := findStats(tracker.Stats(), "agt_2")
	if !ok {
		t.Fatal("no statistics for the agent")
	}
	if got.UnattributedRatio == 0 {
		t.Error("a span missing its cost centre must count as unattributed")
	}
	found := false
	for _, a := range got.MissingAttributes {
		if a == telemetry.AttrCostCenter {
			found = true
		}
	}
	if !found {
		t.Errorf("the missing attribute must be named so an owner can fix it, got %v", got.MissingAttributes)
	}
}

func TestTrustTrackerCountsOrphans(t *testing.T) {
	tracker := NewTrustTracker(time.Millisecond, nil, "prod")
	spans := []map[string]string{{
		"trace": strings.Repeat("c", 32), "span": strings.Repeat("d", 16),
		"parent": strings.Repeat("e", 16), // a parent that never arrived
		"name":   "agent.tool",
	}}
	var payload otlpPayload
	if err := json.Unmarshal([]byte(otlpTrace("agt_3", "prod", spans, nil)), &payload); err != nil {
		t.Fatal(err)
	}
	tracker.Ingest(payload, time.Now())
	time.Sleep(3 * time.Millisecond)

	got, ok := findStats(tracker.Stats(), "agt_3")
	if !ok {
		t.Fatal("no statistics for the agent")
	}
	if got.OrphanSpans != 1 || got.OrphanRatio != 1 {
		t.Errorf("orphans = %d ratio %f, want 1 and 1", got.OrphanSpans, got.OrphanRatio)
	}
}

func findStats(all []TrustStats, agentID string) (TrustStats, bool) {
	for _, s := range all {
		if s.AgentID == agentID {
			return s, true
		}
	}
	return TrustStats{}, false
}

// fakeProm serves canned instant-query responses.
func fakeProm(t *testing.T, values map[string]float64) *PromClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expr := r.URL.Query().Get("query")
		v, ok := values[expr]
		result := "[]"
		if ok {
			result = fmt.Sprintf(`[{"metric":{},"value":[1787000000,"%g"]}]`, v)
		}
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":%s}}`, result)
	}))
	t.Cleanup(srv.Close)
	return NewPromClient(srv.URL, srv.Client())
}

func TestObjectiveEvaluationAndErrorBudget(t *testing.T) {
	o := Objective{
		Name: "availability", GoodExpr: "good", TotalExpr: "total",
		Target: 0.999, Window: 28 * 24 * time.Hour,
	}
	// 1,000,000 requests with 500 failures: an SLI of 0.9995, which meets a
	// 99.9% objective and has spent half the budget.
	p := fakeProm(t, map[string]float64{"good": 999500, "total": 1000000})
	st := Evaluate(context.Background(), p, o)
	if st.Error != "" {
		t.Fatalf("evaluate: %s", st.Error)
	}
	if st.SLI < 0.9994 || st.SLI > 0.9996 {
		t.Errorf("SLI = %f", st.SLI)
	}
	if !st.Met {
		t.Error("0.9995 meets a 0.999 objective")
	}
	if st.BudgetRemaining < 0.49 || st.BudgetRemaining > 0.51 {
		t.Errorf("budget remaining = %f, want about 0.5", st.BudgetRemaining)
	}
	if st.BudgetMinutes <= 0 || st.MinutesRemaining <= 0 {
		t.Errorf("budget minutes = %f / %f", st.MinutesRemaining, st.BudgetMinutes)
	}
}

func TestObjectiveBreach(t *testing.T) {
	o := Objective{Name: "availability", GoodExpr: "good", TotalExpr: "total", Target: 0.999, Window: time.Hour}
	p := fakeProm(t, map[string]float64{"good": 900, "total": 1000})
	st := Evaluate(context.Background(), p, o)
	if st.Met {
		t.Error("90% availability does not meet a 99.9% objective")
	}
	if st.BudgetRemaining != 0 {
		t.Errorf("an exhausted budget must clamp to zero, got %f", st.BudgetRemaining)
	}
}

func TestNoTrafficIsNotABreach(t *testing.T) {
	o := Objective{Name: "availability", GoodExpr: "good", TotalExpr: "total", Target: 0.999, Window: time.Hour}
	p := fakeProm(t, map[string]float64{})
	st := Evaluate(context.Background(), p, o)
	if !st.Met || st.SLI != 1 {
		t.Errorf("an idle window must not read as an outage: met=%v sli=%f", st.Met, st.SLI)
	}
}

func TestBurnRate(t *testing.T) {
	o := Objective{
		Name: "availability", Target: 0.999, Window: 28 * 24 * time.Hour,
		GoodExpr:  `sum(increase(agentgate_gateway_requests_total{status!~"5.."}[28d]))`,
		TotalExpr: `sum(increase(agentgate_gateway_requests_total[28d]))`,
	}
	short := time.Hour
	p := fakeProm(t, map[string]float64{
		strings.ReplaceAll(o.GoodExpr, "28d", "1h"):  9900,
		strings.ReplaceAll(o.TotalExpr, "28d", "1h"): 10000,
	})
	rate, err := BurnRate(context.Background(), p, o, short)
	if err != nil {
		t.Fatal(err)
	}
	// A 1% failure ratio against a 0.1% budget is a burn rate of 10.
	if rate < 9.9 || rate > 10.1 {
		t.Errorf("burn rate = %f, want about 10", rate)
	}
}

func TestDefaultObjectivesCoverTheSpec(t *testing.T) {
	objs := DefaultObjectives(28 * 24 * time.Hour)
	want := map[string]bool{
		"gateway_availability": false, "gateway_overhead_latency": false,
		"gateway_ttft": false, "controlplane_token_exchange": false,
	}
	for _, o := range objs {
		if _, ok := want[o.Name]; !ok {
			t.Errorf("unexpected objective %s", o.Name)
		}
		want[o.Name] = true
		if o.Runbook == "" {
			t.Errorf("objective %s has no runbook; an alert without one wastes the on-call's first five minutes", o.Name)
		}
		if !strings.Contains(o.GoodExpr, "28d") {
			t.Errorf("objective %s does not use the SLO window: %s", o.Name, o.GoodExpr)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("objective %s is missing", name)
		}
	}
}

func TestAvailabilitySLIExcludesCallerFaults(t *testing.T) {
	o := DefaultObjectives(28 * 24 * time.Hour)[0]
	if !strings.Contains(o.GoodExpr, `code!="no_healthy_backend"`) {
		t.Error("a request refused because every backend is down must count against the budget")
	}
	if !strings.Contains(o.TotalExpr, `code!="client_closed_request"`) {
		t.Error("a caller that disconnects must not burn the platform's error budget")
	}
}

func newFleet(t *testing.T) (*Service, registry.Store) {
	t.Helper()
	store, err := registry.NewMemoryStore("")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := identity.ParseAgentIdentity("agent://fsclient/payments-risk/dispute-triage")
	agent := &registry.Agent{
		AgentID: "agt_1", Identity: id, IdentityS: id.String(), DisplayName: "Dispute Triage",
		Owner: registry.Owner{
			Team: "payments-risk", Email: "t@client.example", OnCall: "PD-PAYRISK", CostCenter: "CC-4471",
		},
		Runtime: "aks", DataClassification: registry.ClassConfidential,
		GrantedPools: []string{"general-chat"}, SecurityReviewRef: "CHG1",
		Versions: []registry.Version{
			{Version: "2.4.1", Env: identity.EnvProd, State: registry.StateActive},
		},
	}
	if err := store.PutAgent(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	svc := New(Options{
		Env: "prod", Store: store, Prom: fakeProm(t, nil),
		Trust: NewTrustTracker(time.Minute, nil, "prod"), MinCompleteness: 0.98,
	})
	return svc, store
}

func TestFleetViewReportsOwnershipAndHealth(t *testing.T) {
	svc, _ := newFleet(t)
	views, err := svc.Views(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("expected one agent, got %d", len(views))
	}
	v := views[0]
	if v.Owner.OnCall != "PD-PAYRISK" || v.Owner.CostCenter != "CC-4471" {
		t.Errorf("ownership not surfaced: %#v", v.Owner)
	}
	if v.ProdVersion != "2.4.1" {
		t.Errorf("prod version = %q", v.ProdVersion)
	}
	if v.Health == "" {
		t.Error("every agent must carry a health verdict")
	}
}

func TestFleetViewFlagsMissingOwnership(t *testing.T) {
	svc, store := newFleet(t)
	agent, _ := store.GetAgent(context.Background(), "agt_1")
	agent.Owner.OnCall = ""
	agent.SecurityReviewRef = ""
	if err := store.PutAgent(context.Background(), agent); err != nil {
		t.Fatal(err)
	}

	views, err := svc.build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	v := views[0]
	if v.Health == "healthy" {
		t.Error("an agent with no on-call rotation must not be reported as healthy")
	}
	joined := strings.Join(v.Issues, "; ")
	if !strings.Contains(joined, "on-call") || !strings.Contains(joined, "security review") {
		t.Errorf("issues = %v", v.Issues)
	}
}

func TestFleetAPIEndpoints(t *testing.T) {
	svc, _ := newFleet(t)
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	for _, path := range []string{
		"/api/v1/agents", "/api/v1/slo", "/api/v1/telemetry/health",
		"/api/v1/chargeback?period=day", "/healthz", "/readyz",
	} {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s = %d: %s", path, resp.StatusCode, raw)
		}
		var body any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("%s did not return JSON: %s", path, raw)
		}
	}

	resp, err := srv.Client().Get(srv.URL + "/api/v1/agents/agt_1")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("agent detail = %d: %s", resp.StatusCode, raw)
	}

	missing, err := srv.Client().Get(srv.URL + "/api/v1/agents/nope")
	if err != nil {
		t.Fatal(err)
	}
	_ = missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("unknown agent = %d", missing.StatusCode)
	}
}

func TestDashboardIsSelfContained(t *testing.T) {
	svc, _ := newFleet(t)
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := string(raw)
	if !strings.Contains(body, "<title>AgentGate Fleet</title>") {
		t.Error("the dashboard did not render")
	}
	for _, forbidden := range []string{"src=\"http", "href=\"http", "cdn."} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the dashboard must not load external assets; found %q", forbidden)
		}
	}
	for _, want := range []string{"/api/v1/agents", "/api/v1/slo", "/api/v1/chargeback"} {
		if !strings.Contains(body, want) {
			t.Errorf("the dashboard does not read %s", want)
		}
	}
}

func TestSignalsEndpointFeedsThePromotionGate(t *testing.T) {
	svc, _ := newFleet(t)
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/api/v1/signals/agt_1?env=staging")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
	var sig registry.AgentSignals
	if err := json.Unmarshal(raw, &sig); err != nil {
		t.Fatalf("the signals payload must decode into what the gate consumes: %v", err)
	}
	if sig.SecurityReviewRef != "CHG1" {
		t.Errorf("security review reference = %q", sig.SecurityReviewRef)
	}
}
