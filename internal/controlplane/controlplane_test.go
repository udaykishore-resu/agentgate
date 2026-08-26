package controlplane

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/agentgate/agentgate/internal/identity"
	"github.com/agentgate/agentgate/internal/registry"
)

type fixture struct {
	t      *testing.T
	http   *httptest.Server
	store  registry.Store
	issuer *identity.Issuer
	svc    *registry.Service
}

func newFixture(t *testing.T, signals registry.AgentSignals) *fixture {
	t.Helper()
	store, err := registry.NewMemoryStore("")
	if err != nil {
		t.Fatal(err)
	}
	issuer := identity.NewIssuer(identity.IssuerOptions{
		Issuer: "https://cp.test", Audience: "https://gw.test", TTL: 15 * time.Minute,
	})
	if _, err := issuer.GenerateKey(2048); err != nil {
		t.Fatal(err)
	}
	secrets, err := identity.NewFileSecretStore(t.TempDir() + "/secrets.json")
	if err != nil {
		t.Fatal(err)
	}
	svc := registry.NewService(registry.ServiceOptions{
		Store: store, Issuer: issuer, Secrets: secrets,
		Signals: registry.StaticSignals{S: signals}, Gate: registry.DefaultGateConfig(),
	})
	srv := New(Options{
		Env: "dev", Service: svc, Store: store, Issuer: issuer,
		AllowAnonymousAdmin: true, TokenTTL: 15 * time.Minute,
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)
	return &fixture{t: t, http: httpSrv, store: store, issuer: issuer, svc: svc}
}

func healthySignals() registry.AgentSignals {
	return registry.AgentSignals{
		RequestCount: 500, TelemetryCompleteness: 0.99, SuccessRatio: 0.999,
		SuccessObjective: 0.99, ObservedAttestation: identity.AttestWorkloadIdentity,
		ProjectedMonthlyCostUSD: 10, TeamMonthlyBudgetUSD: 1000,
		TeamTokenEnvelopePerMin: 500000, SecurityReviewRef: "CHG0012345",
	}
}

func (f *fixture) post(path string, body any, actor string) (*http.Response, []byte) {
	f.t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, f.http.URL+path, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	if actor != "" {
		req.Header.Set("x-agentgate-actor", actor)
	}
	resp, err := f.http.Client().Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, out
}

func registration() map[string]any {
	return map[string]any{
		"identity":     "agent://fsclient/payments-risk/dispute-triage",
		"display_name": "Dispute Triage", "runtime": "aks",
		"data_classification": "confidential",
		"requested_pools":     []string{"general-chat"},
		"quota":               map[string]any{"tokens_per_minute": 120000, "requests_per_minute": 600},
		"version":             "2.4.1", "env": "dev",
		"security_review_ref": "CHG0012345",
		"owner": map[string]any{
			"team": "payments-risk", "email": "payments-risk@client.example",
			"oncall": "PD-PAYRISK", "cost_center": "CC-4471",
		},
	}
}

func TestDiscoveryAndJWKS(t *testing.T) {
	f := newFixture(t, healthySignals())

	resp, err := f.http.Client().Get(f.http.URL + "/.well-known/openid-configuration")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&doc)
	_ = resp.Body.Close()
	for _, key := range []string{"issuer", "jwks_uri", "token_endpoint", "grant_types_supported"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("discovery document is missing %s", key)
		}
	}

	resp2, err := f.http.Client().Get(f.http.URL + "/.well-known/jwks.json")
	if err != nil {
		t.Fatal(err)
	}
	var set identity.JWKSet
	_ = json.NewDecoder(resp2.Body).Decode(&set)
	_ = resp2.Body.Close()
	if len(set.Keys) == 0 {
		t.Fatal("no signing keys published")
	}
	if set.Keys[0].Kty != "RSA" || set.Keys[0].Kid == "" {
		t.Errorf("published key = %#v", set.Keys[0])
	}
	if resp2.Header.Get("Cache-Control") == "" {
		t.Error("the key set should be cacheable so the gateway does not hammer the endpoint")
	}
}

func TestRegisterThenIssueAndVerifyAToken(t *testing.T) {
	f := newFixture(t, healthySignals())

	reg := registration()
	reg["issue_credential"] = true
	resp, raw := f.post("/api/v1/agents", reg, "ci@client.example")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register: %d %s", resp.StatusCode, raw)
	}
	var out registry.RegistrationResult
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.ClientID == "" || out.ClientSecret == "" {
		t.Fatal("a credential was requested but not issued")
	}
	if len(out.Warnings) == 0 {
		t.Error("issuing a shared secret should warn that workload identity is preferred")
	}

	form := url.Values{
		"grant_type": {"client_credentials"}, "client_id": {out.ClientID},
		"client_secret": {out.ClientSecret}, "env": {"dev"},
	}
	tokResp, err := f.http.Client().PostForm(f.http.URL+"/oauth2/token", form)
	if err != nil {
		t.Fatal(err)
	}
	tokRaw, _ := io.ReadAll(tokResp.Body)
	_ = tokResp.Body.Close()
	if tokResp.StatusCode != http.StatusOK {
		t.Fatalf("token: %d %s", tokResp.StatusCode, tokRaw)
	}
	if cc := tokResp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("a token response must not be cacheable, got %q", cc)
	}
	var token struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(tokRaw, &token); err != nil {
		t.Fatal(err)
	}
	if token.TokenType != "Bearer" || token.ExpiresIn <= 0 {
		t.Errorf("token response = %#v", token)
	}

	verifier := identity.NewVerifier(identity.VerifierOptions{
		Issuer: "https://cp.test", Audience: "https://gw.test", StaticKeys: f.issuer.PublicKeys(),
	})
	claims, err := verifier.Verify(t.Context(), token.AccessToken)
	if err != nil {
		t.Fatalf("the issued token does not verify at the gateway: %v", err)
	}
	if claims.CostCenter != "CC-4471" || claims.Team != "payments-risk" {
		t.Errorf("claims = %#v", claims)
	}
	if claims.Attestation != identity.AttestClientSecret {
		t.Errorf("attestation = %s; a shared secret must be recorded as the weaker proof it is", claims.Attestation)
	}
}

func TestTokenEndpointRejectsBadCredentialsWithoutAnOracle(t *testing.T) {
	f := newFixture(t, healthySignals())
	reg := registration()
	reg["issue_credential"] = true
	_, raw := f.post("/api/v1/agents", reg, "ci")
	var out registry.RegistrationResult
	_ = json.Unmarshal(raw, &out)

	cases := map[string]url.Values{
		"wrong secret":   {"grant_type": {"client_credentials"}, "client_id": {out.ClientID}, "client_secret": {"wrong"}},
		"unknown client": {"grant_type": {"client_credentials"}, "client_id": {"cli_nope"}, "client_secret": {"wrong"}},
	}
	bodies := map[string]string{}
	for name, form := range cases {
		resp, err := f.http.Client().PostForm(f.http.URL+"/oauth2/token", form)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %d", name, resp.StatusCode)
		}
		bodies[name] = string(body)
	}
	if bodies["wrong secret"] != bodies["unknown client"] {
		t.Error("a wrong secret and an unknown client must be indistinguishable, or the endpoint enumerates clients")
	}
}

func TestTokenEndpointRejectsUnsupportedGrant(t *testing.T) {
	f := newFixture(t, healthySignals())
	resp, err := f.http.Client().PostForm(f.http.URL+"/oauth2/token", url.Values{"grant_type": {"password"}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(string(raw), "unsupported_grant_type") {
		t.Errorf("body = %s", raw)
	}
}

func TestPromotionRequiresTwoDistinctApprovers(t *testing.T) {
	f := newFixture(t, healthySignals())
	_, raw := f.post("/api/v1/agents", registration(), "ci")
	var reg registry.RegistrationResult
	_ = json.Unmarshal(raw, &reg)

	staged := registration()
	staged["env"] = "staging"
	if resp, out := f.post("/api/v1/agents", staged, "ci"); resp.StatusCode != http.StatusOK {
		t.Fatalf("staging registration: %d %s", resp.StatusCode, out)
	}
	// dev -> staging applies without human approval.
	if resp, out := f.post("/api/v1/promotions", map[string]any{
		"agent_id": reg.Agent.AgentID, "version": "2.4.1", "to": "staging",
	}, "alice@client.example"); resp.StatusCode != http.StatusCreated {
		t.Fatalf("staging promotion: %d %s", resp.StatusCode, out)
	}

	resp, out := f.post("/api/v1/promotions", map[string]any{
		"agent_id": reg.Agent.AgentID, "version": "2.4.1", "to": "prod",
	}, "alice@client.example")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("production promotion: %d %s", resp.StatusCode, out)
	}
	var promotion registry.PromotionRequest
	if err := json.Unmarshal(out, &promotion); err != nil {
		t.Fatal(err)
	}
	if promotion.State != registry.PromotionPending {
		t.Fatalf("state = %s, want pending", promotion.State)
	}

	// The requester cannot approve their own promotion.
	if resp, _ := f.post("/api/v1/promotions/"+promotion.ID+"/approve", map[string]any{
		"role": "owning_team", "approved": true,
	}, "alice@client.example"); resp.StatusCode != http.StatusConflict {
		t.Errorf("self approval status = %d, want conflict", resp.StatusCode)
	}

	if resp, o := f.post("/api/v1/promotions/"+promotion.ID+"/approve", map[string]any{
		"role": "owning_team", "approved": true,
	}, "bob@client.example"); resp.StatusCode != http.StatusOK {
		t.Fatalf("first approval: %d %s", resp.StatusCode, o)
	}
	resp2, o2 := f.post("/api/v1/promotions/"+promotion.ID+"/approve", map[string]any{
		"role": "platform", "approved": true,
	}, "carol@client.example")
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second approval: %d %s", resp2.StatusCode, o2)
	}
	var applied registry.PromotionRequest
	_ = json.Unmarshal(o2, &applied)
	if applied.State != registry.PromotionApplied {
		t.Fatalf("state = %s, want applied", applied.State)
	}
	if len(applied.Approvals) != 2 {
		t.Errorf("the audit trail must record both approvals, got %d", len(applied.Approvals))
	}
	for _, a := range applied.Approvals {
		if a.Actor == "" || a.At.IsZero() {
			t.Errorf("approval is missing an actor or timestamp: %#v", a)
		}
	}
}

func TestBlockedPromotionReturnsTheGateEvidence(t *testing.T) {
	bad := healthySignals()
	bad.TelemetryCompleteness = 0.1
	f := newFixture(t, bad)

	staged := registration()
	staged["env"] = "staging"
	_, raw := f.post("/api/v1/agents", staged, "ci")
	var reg registry.RegistrationResult
	_ = json.Unmarshal(raw, &reg)

	resp, out := f.post("/api/v1/promotions", map[string]any{
		"agent_id": reg.Agent.AgentID, "version": "2.4.1", "to": "prod",
	}, "alice")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want conflict for a blocked gate: %s", resp.StatusCode, out)
	}
	var envelope struct {
		Code        string                    `json:"code"`
		FailedGates []string                  `json:"failed_gates"`
		Promotion   registry.PromotionRequest `json:"promotion"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Code != "promotion_gate_blocked" {
		t.Errorf("code = %q; a pipeline needs a stable code to branch on", envelope.Code)
	}
	if len(envelope.FailedGates) == 0 {
		t.Error("the failing gates must be named at the top level, not only inside the evaluation")
	}
	promotion := envelope.Promotion
	if promotion.State != registry.PromotionBlocked {
		t.Errorf("state = %s", promotion.State)
	}
	if len(promotion.Gate.Checks) == 0 {
		t.Fatal("a blocked promotion must return the evidence, not just a refusal")
	}
	failed := promotion.Gate.Failed()
	if len(failed) == 0 {
		t.Error("no failing gate was named")
	}
	for _, c := range promotion.Gate.Checks {
		if c.Name == registry.GateTelemetryHealthy && c.Status == registry.CheckFail {
			if c.Observed == "" || c.Required == "" {
				t.Error("a failing gate must report what was observed and what was required")
			}
		}
	}
}

func TestWaiverRequiresAReasonAndExpires(t *testing.T) {
	f := newFixture(t, healthySignals())
	if resp, _ := f.post("/api/v1/waivers", map[string]any{
		"agent_id": "agt_1", "gate": registry.GateGuardrailClean,
	}, "platform@client.example"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a waiver without a reason must be refused, got %d", resp.StatusCode)
	}
	resp, raw := f.post("/api/v1/waivers", map[string]any{
		"agent_id": "agt_1", "gate": registry.GateGuardrailClean,
		"reason": "false positives under investigation", "days": 500,
	}, "platform@client.example")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
	var w registry.Waiver
	_ = json.Unmarshal(raw, &w)
	if w.ExpiresAt.After(time.Now().AddDate(0, 0, 91)) {
		t.Error("a waiver must not be granted for longer than the policy maximum")
	}
	if w.GrantedBy == "" {
		t.Error("the granting actor must be recorded")
	}
}

func TestQuarantineRequiresAReason(t *testing.T) {
	f := newFixture(t, healthySignals())
	_, raw := f.post("/api/v1/agents", registration(), "ci")
	var reg registry.RegistrationResult
	_ = json.Unmarshal(raw, &reg)

	if resp, _ := f.post("/api/v1/agents/"+reg.Agent.AgentID+"/quarantine", map[string]any{
		"version": "2.4.1", "env": "dev",
	}, "oncall"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a quarantine without a reason must be refused, got %d", resp.StatusCode)
	}
	if resp, out := f.post("/api/v1/agents/"+reg.Agent.AgentID+"/quarantine", map[string]any{
		"version": "2.4.1", "env": "dev", "reason": "runaway spend",
	}, "oncall"); resp.StatusCode != http.StatusOK {
		t.Fatalf("quarantine: %d %s", resp.StatusCode, out)
	}
}

func TestReadinessReflectsDependencies(t *testing.T) {
	f := newFixture(t, healthySignals())
	resp, err := f.http.Client().Get(f.http.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "signing_key") {
		t.Errorf("readiness must report the signing key, without which no token can be issued: %s", raw)
	}
}
