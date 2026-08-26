package registry

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/agentgate/agentgate/internal/identity"
)

func healthySignals() AgentSignals {
	return AgentSignals{
		RequestCount: 500, TelemetryCompleteness: 0.99,
		SuccessRatio: 0.999, SuccessObjective: 0.99,
		CriticalGuardrailHits: 0, ObservedAttestation: identity.AttestWorkloadIdentity,
		ProjectedMonthlyCostUSD: 100, TeamMonthlyBudgetUSD: 1000,
		TeamTokenEnvelopePerMin: 500000, SecurityReviewRef: "CHG0012345",
		WindowSeconds: int64((24 * time.Hour).Seconds()),
	}
}

func testAgent() *Agent {
	id, _ := identity.ParseAgentIdentity("agent://fsclient/payments-risk/dispute-triage")
	return &Agent{
		AgentID: "agt_1", Identity: id, IdentityS: id.String(),
		DisplayName: "Dispute Triage",
		Owner: Owner{
			Team: "payments-risk", Email: "payments-risk@client.example",
			OnCall: "PD-PAYRISK", CostCenter: "CC-4471",
		},
		Runtime: "aks", DataClassification: ClassConfidential,
		RequestedPools: []string{"general-chat"}, GrantedPools: []string{"general-chat"},
		Quota:             Quota{TokensPerMinute: 120000, RequestsPerMinute: 600},
		SecurityReviewRef: "CHG0012345",
		Versions: []Version{
			{Version: "2.4.1", Env: identity.EnvStaging, State: StateActive, RegisteredAt: time.Now()},
		},
	}
}

func TestGatePassesWhenEverythingIsHealthy(t *testing.T) {
	res, err := Evaluate(context.Background(), DefaultGateConfig(), testAgent(), "2.4.1",
		identity.EnvStaging, identity.EnvProd, StaticSignals{S: healthySignals()}, nil, time.Now())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !res.Passed {
		t.Fatalf("gate failed unexpectedly: %v", res.Failed())
	}
	if len(res.Checks) != 8 {
		t.Errorf("expected 8 recorded checks, got %d", len(res.Checks))
	}
}

func TestGateBlocksOnEachSignal(t *testing.T) {
	cases := map[string]struct {
		mutate func(*AgentSignals)
		agent  func(*Agent)
		gate   string
	}{
		"incomplete telemetry": {
			mutate: func(s *AgentSignals) { s.TelemetryCompleteness = 0.5 },
			gate:   GateTelemetryHealthy,
		},
		"too few observed requests": {
			mutate: func(s *AgentSignals) { s.RequestCount = 3 },
			gate:   GateTelemetryHealthy,
		},
		"error budget exhausted": {
			mutate: func(s *AgentSignals) { s.SuccessRatio = 0.90 },
			gate:   GateErrorBudget,
		},
		"weak attestation": {
			mutate: func(s *AgentSignals) { s.ObservedAttestation = identity.AttestClientSecret },
			gate:   GateIdentityAttested,
		},
		"guardrail violations": {
			mutate: func(s *AgentSignals) { s.CriticalGuardrailHits = 4 },
			gate:   GateGuardrailClean,
		},
		"quota beyond the team envelope": {
			mutate: func(s *AgentSignals) { s.TeamTokenEnvelopePerMin = 1000 },
			gate:   GateQuotaDeclared,
		},
		"cost beyond the team budget": {
			mutate: func(s *AgentSignals) { s.ProjectedMonthlyCostUSD = 100000 },
			gate:   GateCostProjection,
		},
		"missing security review": {
			mutate: func(s *AgentSignals) { s.SecurityReviewRef = "" },
			agent:  func(a *Agent) { a.SecurityReviewRef = "" },
			gate:   GateSecurityReview,
		},
		"missing on-call": {
			mutate: func(*AgentSignals) {},
			agent:  func(a *Agent) { a.Owner.OnCall = "" },
			gate:   GateRegistrationComplete,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			sig := healthySignals()
			tc.mutate(&sig)
			agent := testAgent()
			if tc.agent != nil {
				tc.agent(agent)
			}
			res, err := Evaluate(context.Background(), DefaultGateConfig(), agent, "2.4.1",
				identity.EnvStaging, identity.EnvProd, StaticSignals{S: sig}, nil, time.Now())
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			if res.Passed {
				t.Fatalf("gate passed but %s should have blocked it", tc.gate)
			}
			found := false
			for _, f := range res.Failed() {
				if f == tc.gate {
					found = true
				}
			}
			if !found {
				t.Errorf("failed gates %v do not include %s", res.Failed(), tc.gate)
			}
		})
	}
}

func TestGateEvaluatesEveryCheckEvenAfterAFailure(t *testing.T) {
	sig := healthySignals()
	sig.TelemetryCompleteness = 0
	sig.SuccessRatio = 0
	agent := testAgent()
	agent.Owner.OnCall = ""
	res, _ := Evaluate(context.Background(), DefaultGateConfig(), agent, "2.4.1",
		identity.EnvStaging, identity.EnvProd, StaticSignals{S: sig}, nil, time.Now())
	if len(res.Failed()) < 3 {
		t.Errorf("expected every failing gate to be reported, got %v", res.Failed())
	}
}

func TestWaiverConvertsFailureToWaived(t *testing.T) {
	sig := healthySignals()
	sig.CriticalGuardrailHits = 5
	waivers := []Waiver{{
		ID: "wvr_1", AgentID: "agt_1", Gate: GateGuardrailClean,
		Reason: "false positives under investigation", ExpiresAt: time.Now().Add(time.Hour),
	}}
	res, _ := Evaluate(context.Background(), DefaultGateConfig(), testAgent(), "2.4.1",
		identity.EnvStaging, identity.EnvProd, StaticSignals{S: sig}, waivers, time.Now())
	if !res.Passed {
		t.Fatalf("an active waiver should let the gate pass: %v", res.Failed())
	}
	var waived bool
	for _, c := range res.Checks {
		if c.Name == GateGuardrailClean && c.Status == CheckWaived {
			waived = true
			if c.WaiverID != "wvr_1" {
				t.Errorf("waiver id not recorded, got %q", c.WaiverID)
			}
		}
	}
	if !waived {
		t.Error("the waived gate must be recorded as waived, not as passing")
	}
}

func TestExpiredWaiverDoesNotApply(t *testing.T) {
	sig := healthySignals()
	sig.CriticalGuardrailHits = 5
	waivers := []Waiver{{
		ID: "wvr_1", AgentID: "agt_1", Gate: GateGuardrailClean,
		ExpiresAt: time.Now().Add(-time.Hour),
	}}
	res, _ := Evaluate(context.Background(), DefaultGateConfig(), testAgent(), "2.4.1",
		identity.EnvStaging, identity.EnvProd, StaticSignals{S: sig}, waivers, time.Now())
	if res.Passed {
		t.Error("an expired waiver must not suppress a gate failure")
	}
}

func TestTwoPartyApproval(t *testing.T) {
	agent := testAgent()
	gate := GateResult{Passed: true}
	now := time.Now()
	req := NewPromotionRequest(agent, "2.4.1", identity.EnvStaging, identity.EnvProd, "alice", gate, time.Hour)
	if req.State != PromotionPending {
		t.Fatalf("state = %s", req.State)
	}

	if err := req.Record(Approval{Actor: "alice", Role: RoleOwner, Approved: true}, now); !errors.Is(err, ErrSelfApproval) {
		t.Errorf("a requester approving their own promotion must be refused, got %v", err)
	}
	if err := req.Record(Approval{Actor: "bob", Role: RoleOwner, Approved: true}, now); err != nil {
		t.Fatalf("first approval: %v", err)
	}
	if req.State != PromotionPending {
		t.Fatalf("one approval must not be enough, state = %s", req.State)
	}
	if err := req.Record(Approval{Actor: "bob", Role: RolePlatform, Approved: true}, now); !errors.Is(err, ErrDuplicateActor) {
		t.Errorf("the same actor must not satisfy both roles, got %v", err)
	}
	if err := req.Record(Approval{Actor: "carol", Role: RoleOwner, Approved: true}, now); !errors.Is(err, ErrRoleSatisfied) {
		t.Errorf("a second owning-team approval must not complete the request, got %v", err)
	}
	if err := req.Record(Approval{Actor: "dave", Role: RolePlatform, Approved: true}, now); err != nil {
		t.Fatalf("platform approval: %v", err)
	}
	if req.State != PromotionApproved {
		t.Fatalf("state = %s, want approved after one approval from each role", req.State)
	}
}

func TestRejectionEndsTheRequest(t *testing.T) {
	req := NewPromotionRequest(testAgent(), "2.4.1", identity.EnvStaging, identity.EnvProd, "alice", GateResult{Passed: true}, time.Hour)
	if err := req.Record(Approval{Actor: "bob", Role: RolePlatform, Approved: false, Comment: "no"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if req.State != PromotionRejected {
		t.Fatalf("state = %s, want rejected", req.State)
	}
}

func TestExpiredPromotionCannotBeApproved(t *testing.T) {
	req := NewPromotionRequest(testAgent(), "2.4.1", identity.EnvStaging, identity.EnvProd, "alice", GateResult{Passed: true}, time.Minute)
	err := req.Record(Approval{Actor: "bob", Role: RoleOwner, Approved: true}, time.Now().Add(time.Hour))
	if !errors.Is(err, ErrPromotionExpiry) {
		t.Errorf("an expired request must be refused, got %v", err)
	}
	if req.State != PromotionExpired {
		t.Errorf("state = %s, want expired", req.State)
	}
}

func TestLowerEnvironmentsNeedNoApproval(t *testing.T) {
	if RequiredApprovals(identity.EnvStaging) != 0 {
		t.Error("staging must not require human approval")
	}
	if RequiredApprovals(identity.EnvProd) != 2 {
		t.Error("production must require two approvals")
	}
}

func newService(t *testing.T) (*Service, Store) {
	t.Helper()
	store, err := NewMemoryStore("")
	if err != nil {
		t.Fatal(err)
	}
	iss := identity.NewIssuer(identity.IssuerOptions{Issuer: "https://cp.test", Audience: "https://gw.test"})
	if _, err := iss.GenerateKey(2048); err != nil {
		t.Fatal(err)
	}
	svc := NewService(ServiceOptions{
		Store: store, Issuer: iss, Signals: StaticSignals{S: healthySignals()},
		Gate: DefaultGateConfig(),
	})
	return svc, store
}

func registration() RegistrationRequest {
	return RegistrationRequest{
		Identity:    "agent://fsclient/payments-risk/dispute-triage",
		DisplayName: "Dispute Triage", Runtime: "aks",
		DataClassification: ClassConfidential,
		RequestedPools:     []string{"general-chat"},
		Quota:              Quota{TokensPerMinute: 120000, RequestsPerMinute: 600},
		Version:            "2.4.1", Env: identity.EnvDev,
		SecurityReviewRef: "CHG0012345",
		Owner: Owner{
			Team: "payments-risk", Email: "payments-risk@client.example",
			OnCall: "PD-PAYRISK", CostCenter: "CC-4471",
		},
	}
}

func TestRegistrationIsIdempotent(t *testing.T) {
	svc, store := newService(t)
	ctx := context.Background()
	first, err := svc.Register(ctx, registration(), "ci", false)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	second, err := svc.Register(ctx, registration(), "ci", false)
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if first.Agent.AgentID != second.Agent.AgentID {
		t.Error("re-running a pipeline must not create a second agent")
	}
	agents, _ := store.ListAgents(ctx, Filter{})
	if len(agents) != 1 {
		t.Fatalf("expected one agent, got %d", len(agents))
	}
	if len(agents[0].Versions) != 1 {
		t.Errorf("expected one version record, got %d", len(agents[0].Versions))
	}
}

func TestRegistrationRejectsOwnershipTransfer(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	if _, err := svc.Register(ctx, registration(), "ci", false); err != nil {
		t.Fatal(err)
	}
	req := registration()
	req.Owner.Team = "another-team"
	_, err := svc.Register(ctx, req, "ci", false)
	if err == nil {
		t.Fatal("a change of owning team must not be an implicit update")
	}
}

func TestRegistrationValidation(t *testing.T) {
	for name, mutate := range map[string]func(*RegistrationRequest){
		"missing on-call":        func(r *RegistrationRequest) { r.Owner.OnCall = "" },
		"missing cost centre":    func(r *RegistrationRequest) { r.Owner.CostCenter = "" },
		"missing classification": func(r *RegistrationRequest) { r.DataClassification = "" },
		"bad identity":           func(r *RegistrationRequest) { r.Identity = "not-an-agent-uri" },
		"team mismatch":          func(r *RegistrationRequest) { r.Owner.Team = "different" },
		"no pools":               func(r *RegistrationRequest) { r.RequestedPools = nil },
		"zero quota":             func(r *RegistrationRequest) { r.Quota.TokensPerMinute = 0 },
		"bad environment":        func(r *RegistrationRequest) { r.Env = "preprod" },
	} {
		t.Run(name, func(t *testing.T) {
			req := registration()
			mutate(&req)
			if err := req.Validate(); err == nil {
				t.Errorf("%s should have been rejected", name)
			}
		})
	}
}

func TestPromotionAppliesAndRetiresPreviousVersion(t *testing.T) {
	svc, store := newService(t)
	ctx := context.Background()

	// 2.4.0 reaches production first.
	base := registration()
	base.Version = "2.4.0"
	res, err := svc.Register(ctx, base, "ci", false)
	if err != nil {
		t.Fatal(err)
	}
	agentID := res.Agent.AgentID
	promoteTo := func(version string, env identity.Environment) *PromotionRequest {
		t.Helper()
		reg := registration()
		reg.Version = version
		reg.Env = envBefore(env)
		if _, err := svc.Register(ctx, reg, "ci", false); err != nil {
			t.Fatal(err)
		}
		p, err := svc.RequestPromotion(ctx, agentID, version, env, "alice")
		if err != nil {
			t.Fatalf("promote %s to %s: %v", version, env, err)
		}
		return p
	}

	if p := promoteTo("2.4.0", identity.EnvStaging); p.State != PromotionApplied {
		t.Fatalf("staging promotion state = %s, want applied without human approval", p.State)
	}
	prod := promoteTo("2.4.0", identity.EnvProd)
	if prod.State != PromotionPending {
		t.Fatalf("production promotion state = %s, want pending approval", prod.State)
	}
	if _, err := svc.Approve(ctx, prod.ID, Approval{Actor: "bob", Role: RoleOwner, Approved: true}); err != nil {
		t.Fatal(err)
	}
	applied, err := svc.Approve(ctx, prod.ID, Approval{Actor: "carol", Role: RolePlatform, Approved: true})
	if err != nil {
		t.Fatal(err)
	}
	if applied.State != PromotionApplied {
		t.Fatalf("state = %s, want applied", applied.State)
	}

	agent, _ := store.GetAgent(ctx, agentID)
	v, ok := agent.VersionIn(identity.EnvProd, StateActive)
	if !ok || v.Version != "2.4.0" {
		t.Fatalf("active production version = %#v", v)
	}
	if v.LastGate == nil {
		t.Error("the gate snapshot must be retained on the promoted version for audit")
	}

	// 2.5.0 replaces it; the previous version is retired rather than deleted.
	next := promoteTo("2.5.0", identity.EnvStaging)
	_ = next
	prod2 := promoteTo("2.5.0", identity.EnvProd)
	if _, err := svc.Approve(ctx, prod2.ID, Approval{Actor: "bob", Role: RoleOwner, Approved: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Approve(ctx, prod2.ID, Approval{Actor: "carol", Role: RolePlatform, Approved: true}); err != nil {
		t.Fatal(err)
	}
	agent, _ = store.GetAgent(ctx, agentID)
	if v, ok := agent.VersionIn(identity.EnvProd, StateActive); !ok || v.Version != "2.5.0" {
		t.Fatalf("active production version = %#v", v)
	}
	old, ok := agent.Find(identity.EnvProd, "2.4.0")
	if !ok || old.State != StateRetired {
		t.Errorf("the previous version must be retired, not removed: %#v", old)
	}
	if old.RetiredAt == nil {
		t.Error("retirement time must be recorded so a rollback is auditable")
	}
}

func envBefore(e identity.Environment) identity.Environment {
	prev, ok := e.Previous()
	if !ok {
		return identity.EnvDev
	}
	return prev
}

func TestBlockedPromotionIsRecordedNotApplied(t *testing.T) {
	store, _ := NewMemoryStore("")
	iss := identity.NewIssuer(identity.IssuerOptions{Issuer: "i", Audience: "a"})
	if _, err := iss.GenerateKey(2048); err != nil {
		t.Fatal(err)
	}
	bad := healthySignals()
	bad.TelemetryCompleteness = 0.1
	svc := NewService(ServiceOptions{Store: store, Issuer: iss, Signals: StaticSignals{S: bad}})
	ctx := context.Background()

	reg := registration()
	reg.Env = identity.EnvStaging
	res, err := svc.Register(ctx, reg, "ci", false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := svc.RequestPromotion(ctx, res.Agent.AgentID, reg.Version, identity.EnvProd, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if p.State != PromotionBlocked {
		t.Fatalf("state = %s, want blocked", p.State)
	}
	agent, _ := store.GetAgent(ctx, res.Agent.AgentID)
	if _, ok := agent.VersionIn(identity.EnvProd, StateActive); ok {
		t.Error("a blocked promotion must not activate the version")
	}
}

func TestQuarantineStopsTokenIssuance(t *testing.T) {
	svc, store := newService(t)
	ctx := context.Background()
	res, err := svc.Register(ctx, registration(), "ci", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.MintToken(ctx, res.Agent.AgentID, "2.4.1", identity.EnvDev, identity.AttestWorkloadIdentity, "test"); err != nil {
		t.Fatalf("token before quarantine: %v", err)
	}
	if err := svc.Quarantine(ctx, res.Agent.AgentID, "2.4.1", identity.EnvDev, "runaway spend", "oncall"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.MintToken(ctx, res.Agent.AgentID, "2.4.1", identity.EnvDev, identity.AttestWorkloadIdentity, "test"); err == nil {
		t.Error("a quarantined version must not receive new tokens")
	}
	agent, _ := store.GetAgent(ctx, res.Agent.AgentID)
	v, _ := agent.Find(identity.EnvDev, "2.4.1")
	if v.Notes == "" {
		t.Error("the quarantine reason must be recorded")
	}
}

func TestCredentialRotationKeepsOldSecretValidDuringOverlap(t *testing.T) {
	svc, store := newService(t)
	ctx := context.Background()
	res, err := svc.Register(ctx, registration(), "ci", true)
	if err != nil {
		t.Fatal(err)
	}
	oldID := res.ClientID
	if oldID == "" {
		t.Fatal("a credential should have been issued")
	}
	newCred, newSecret, err := svc.RotateCredential(ctx, res.Agent.AgentID, oldID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if newCred.ClientID == oldID {
		t.Error("rotation must issue a new client id")
	}
	if !newCred.Verify(newSecret) {
		t.Error("the new secret must verify")
	}
	old, _, err := store.GetCredential(ctx, oldID)
	if err != nil {
		t.Fatal(err)
	}
	if !old.Verify(res.ClientSecret) {
		t.Error("the previous secret must remain valid during the overlap window")
	}
	if old.ExpiresAt.After(time.Now().Add(2 * time.Hour)) {
		t.Error("the previous secret's expiry must be shortened to the overlap window")
	}
}

func TestMemoryStorePersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/registry.json"
	store, err := NewMemoryStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.PutAgent(ctx, testAgent()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewMemoryStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.GetAgentByIdentity(ctx, "agent://fsclient/payments-risk/dispute-triage")
	if err != nil {
		t.Fatalf("agent did not survive a reopen: %v", err)
	}
	if got.Owner.CostCenter != "CC-4471" {
		t.Errorf("owner not restored: %#v", got.Owner)
	}
}

func TestDataClassificationRank(t *testing.T) {
	if ClassPublic.Rank() >= ClassRestricted.Rank() {
		t.Error("restricted must rank above public")
	}
	if ClassInternal.Rank() >= ClassConfidential.Rank() {
		t.Error("confidential must rank above internal")
	}
	if DataClassification("nonsense").Rank() != ClassConfidential.Rank() {
		t.Error("an unknown classification must default to a conservative rank")
	}
}

func TestPendingPromotionIsVisibleOnTheVersionRecord(t *testing.T) {
	svc, store := newService(t)
	ctx := context.Background()

	staged := registration()
	staged.Env = identity.EnvStaging
	res, err := svc.Register(ctx, staged, "ci", false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := svc.RequestPromotion(ctx, res.Agent.AgentID, staged.Version, identity.EnvProd, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if p.State != PromotionPending {
		t.Fatalf("state = %s", p.State)
	}
	agent, _ := store.GetAgent(ctx, res.Agent.AgentID)
	v, ok := agent.Find(identity.EnvProd, staged.Version)
	if !ok {
		t.Fatal("no production version record was created for the pending promotion")
	}
	if v.State != StatePendingPromotion {
		t.Errorf("version state = %s, want pending_promotion so the fleet view shows the wait", v.State)
	}
}

func TestBlockedPromotionMarksTheVersionBlocked(t *testing.T) {
	store, _ := NewMemoryStore("")
	iss := identity.NewIssuer(identity.IssuerOptions{Issuer: "i", Audience: "a"})
	if _, err := iss.GenerateKey(2048); err != nil {
		t.Fatal(err)
	}
	bad := healthySignals()
	bad.TelemetryCompleteness = 0.1
	svc := NewService(ServiceOptions{Store: store, Issuer: iss, Signals: StaticSignals{S: bad}})
	ctx := context.Background()

	reg := registration()
	reg.Env = identity.EnvStaging
	res, err := svc.Register(ctx, reg, "ci", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RequestPromotion(ctx, res.Agent.AgentID, reg.Version, identity.EnvProd, "alice"); err != nil {
		t.Fatal(err)
	}
	agent, _ := store.GetAgent(ctx, res.Agent.AgentID)
	v, ok := agent.Find(identity.EnvProd, reg.Version)
	if !ok {
		t.Fatal("no production version record was created for the blocked promotion")
	}
	if v.State != StateBlocked {
		t.Errorf("version state = %s, want blocked", v.State)
	}
	if v.Notes == "" || v.LastGate == nil {
		t.Error("a blocked version must carry the reason and the gate snapshot")
	}
}

func TestMemoryStoreSeesAnotherProcessesWrites(t *testing.T) {
	// The local stack has the control plane writing the snapshot and the fleet
	// view reading it. A reader that never re-reads shows an empty fleet
	// forever, which is exactly the bug this guards against.
	path := t.TempDir() + "/registry.json"
	writer, err := NewMemoryStore(path)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewMemoryStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if agents, _ := reader.ListAgents(ctx, Filter{}); len(agents) != 0 {
		t.Fatalf("reader started with %d agents", len(agents))
	}
	if err := writer.PutAgent(ctx, testAgent()); err != nil {
		t.Fatal(err)
	}
	// Snapshot modification times have one-second resolution on some
	// filesystems; nudge it so the change is unambiguous.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	agents, err := reader.ListAgents(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("the reader did not pick up the writer's snapshot, got %d agents", len(agents))
	}
	if _, err := reader.GetAgentByIdentity(ctx, "agent://fsclient/payments-risk/dispute-triage"); err != nil {
		t.Errorf("identity index was not rebuilt on refresh: %v", err)
	}
}
