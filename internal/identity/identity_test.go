package identity

import (
	"context"
	"crypto"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseAgentIdentity(t *testing.T) {
	good, err := ParseAgentIdentity("agent://fsclient/payments-risk/dispute-triage")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if good.Tenant != "fsclient" || good.Team != "payments-risk" || good.Name != "dispute-triage" {
		t.Fatalf("parsed = %#v", good)
	}
	if good.String() != "agent://fsclient/payments-risk/dispute-triage" {
		t.Errorf("round trip = %q", good.String())
	}

	for _, bad := range []string{
		"", "http://fsclient/team/name", "agent://fsclient/team",
		"agent://fsclient/team/name/extra", "agent://FSCLIENT/team/name",
		"agent://fsclient/team/name_with_underscore", "agent:///team/name",
	} {
		if _, err := ParseAgentIdentity(bad); err == nil {
			t.Errorf("expected %q to be rejected", bad)
		}
	}
}

func newClaims() *Claims {
	c := &Claims{
		AgentID: "agt_1", Tenant: "fsclient", Team: "payments-risk",
		AgentName: "dispute-triage", AgentVersion: "2.4.1", Env: EnvProd,
		CostCenter: "CC-4471", Scopes: []string{ScopeInvoke}, ModelPools: []string{"general-chat"},
		Attestation: AttestWorkloadIdentity,
	}
	c.Subject = "agent://fsclient/payments-risk/dispute-triage"
	return c
}

func TestClaimsValidate(t *testing.T) {
	if err := newClaims().Validate(); err != nil {
		t.Fatalf("valid claims rejected: %v", err)
	}

	t.Run("ownership mismatch is rejected", func(t *testing.T) {
		c := newClaims()
		c.Team = "another-team"
		if err := c.Validate(); err == nil {
			t.Error("a token whose team claim disagrees with its subject must be rejected")
		}
	})
	t.Run("production requires a cost centre", func(t *testing.T) {
		c := newClaims()
		c.CostCenter = ""
		if err := c.Validate(); err == nil {
			t.Error("a production token without a cost centre must be rejected")
		}
		c.Env = EnvDev
		if err := c.Validate(); err != nil {
			t.Errorf("dev without a cost centre should be allowed: %v", err)
		}
	})
	t.Run("unknown environment is rejected", func(t *testing.T) {
		c := newClaims()
		c.Env = "preprod"
		if err := c.Validate(); err == nil {
			t.Error("an unknown environment must be rejected")
		}
	})
}

func TestClaimsScopesAndPools(t *testing.T) {
	c := newClaims()
	if !c.HasScope(ScopeInvoke) || c.HasScope(ScopeEmbed) {
		t.Error("scope check is wrong")
	}
	c.Scopes = []string{ScopeAdmin}
	if !c.HasScope(ScopeEmbed) {
		t.Error("the admin scope should satisfy any scope check")
	}
	if !c.MayUsePool("general-chat") || c.MayUsePool("long-context") {
		t.Error("pool entitlement is wrong")
	}
	c.ModelPools = []string{"*"}
	if !c.MayUsePool("anything") {
		t.Error("a wildcard entitlement should permit any pool")
	}
	c.ModelPools = nil
	if c.MayUsePool("general-chat") {
		t.Error("an empty entitlement must permit nothing")
	}
	if c.QuotaKey() != "fsclient:payments-risk:dispute-triage:prod" {
		t.Errorf("quota key = %q", c.QuotaKey())
	}
}

// issuerAndVerifier wires a real issuer to a real verifier over a real JWKS
// endpoint, so the test covers the same path a running gateway takes.
func issuerAndVerifier(t *testing.T) (*Issuer, *Verifier, *httptest.Server) {
	t.Helper()
	iss := NewIssuer(IssuerOptions{
		Issuer: "https://cp.test", Audience: "https://gw.test", TTL: time.Minute,
	})
	if _, err := iss.GenerateKey(2048); err != nil {
		t.Fatalf("generate key: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(iss.JWKS())
	}))
	cache := NewJWKSCache(JWKSOptions{URL: srv.URL})
	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatalf("jwks refresh: %v", err)
	}
	v := NewVerifier(VerifierOptions{Issuer: "https://cp.test", Audience: "https://gw.test", JWKS: cache})
	return iss, v, srv
}

func TestIssueAndVerifyRoundTrip(t *testing.T) {
	iss, v, srv := issuerAndVerifier(t)
	defer srv.Close()

	id, _ := ParseAgentIdentity("agent://fsclient/payments-risk/dispute-triage")
	token, claims, err := iss.Mint(TokenRequest{
		AgentID: "agt_1", Identity: id, AgentVersion: "2.4.1", Env: EnvProd,
		CostCenter: "CC-4471", Scopes: []string{ScopeInvoke}, ModelPools: []string{"general-chat"},
		Attestation: AttestWorkloadIdentity,
	}, "client_credentials")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	got, err := v.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.AgentID != claims.AgentID || got.CostCenter != "CC-4471" || got.Env != EnvProd {
		t.Fatalf("verified claims = %#v", got)
	}

	// An access token is presented on every request; verifying it twice must
	// succeed. This is the regression guard for treating it as single-use.
	if _, err := v.Verify(context.Background(), token); err != nil {
		t.Fatalf("second verification of the same access token failed: %v", err)
	}
}

func TestVerifierRejections(t *testing.T) {
	iss, _, srv := issuerAndVerifier(t)
	defer srv.Close()
	id, _ := ParseAgentIdentity("agent://fsclient/payments-risk/dispute-triage")
	mint := func(o IssuerOptions) string {
		other := NewIssuer(o)
		if _, err := other.GenerateKey(2048); err != nil {
			t.Fatal(err)
		}
		tok, _, err := other.Mint(TokenRequest{
			AgentID: "agt_1", Identity: id, AgentVersion: "1", Env: EnvDev,
			Scopes: []string{ScopeInvoke}, ModelPools: []string{"p"},
		}, "test")
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}

	cache := NewJWKSCache(JWKSOptions{URL: srv.URL})
	_ = cache.Refresh(context.Background())
	v := NewVerifier(VerifierOptions{Issuer: "https://cp.test", Audience: "https://gw.test", JWKS: cache})

	t.Run("unknown signing key", func(t *testing.T) {
		tok := mint(IssuerOptions{Issuer: "https://cp.test", Audience: "https://gw.test", TTL: time.Minute})
		if _, err := v.Verify(context.Background(), tok); err == nil {
			t.Error("a token signed by an untrusted key must be rejected")
		}
	})
	t.Run("wrong audience", func(t *testing.T) {
		tok, _, err := iss.Mint(TokenRequest{
			AgentID: "agt_1", Identity: id, AgentVersion: "1", Env: EnvDev,
			Scopes: []string{ScopeInvoke}, ModelPools: []string{"p"},
		}, "test")
		if err != nil {
			t.Fatal(err)
		}
		other := NewVerifier(VerifierOptions{Issuer: "https://cp.test", Audience: "https://elsewhere", JWKS: cache})
		if _, err := other.Verify(context.Background(), tok); err == nil {
			t.Error("a token for another audience must be rejected")
		}
	})
	t.Run("expired", func(t *testing.T) {
		past := NewIssuer(IssuerOptions{
			Issuer: "https://cp.test", Audience: "https://gw.test", TTL: time.Second,
			Now: func() time.Time { return time.Now().Add(-2 * time.Hour) },
		})
		key, _ := past.GenerateKey(2048)
		vv := NewVerifier(VerifierOptions{
			Issuer: "https://cp.test", Audience: "https://gw.test",
			StaticKeys: map[string]crypto.PublicKey{key.Kid: &key.Private.PublicKey},
		})
		tok, _, err := past.Mint(TokenRequest{
			AgentID: "agt_1", Identity: id, AgentVersion: "1", Env: EnvDev,
			Scopes: []string{ScopeInvoke}, ModelPools: []string{"p"},
		}, "test")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := vv.Verify(context.Background(), tok); err == nil {
			t.Error("an expired token must be rejected")
		}
	})
	t.Run("garbage", func(t *testing.T) {
		for _, tok := range []string{"", "not-a-token", "a.b.c"} {
			if _, err := v.Verify(context.Background(), tok); err == nil {
				t.Errorf("%q must be rejected", tok)
			}
		}
	})
}

func TestKeyRotationKeepsOldTokensVerifiable(t *testing.T) {
	iss := NewIssuer(IssuerOptions{Issuer: "https://cp.test", Audience: "https://gw.test", TTL: time.Minute})
	if _, err := iss.GenerateKey(2048); err != nil {
		t.Fatal(err)
	}
	id, _ := ParseAgentIdentity("agent://fsclient/team/agent")
	before, _, err := iss.Mint(TokenRequest{
		AgentID: "agt_1", Identity: id, AgentVersion: "1", Env: EnvDev,
		Scopes: []string{ScopeInvoke}, ModelPools: []string{"p"},
	}, "test")
	if err != nil {
		t.Fatal(err)
	}

	oldKid := iss.ActiveKeyID()
	if _, err := iss.Rotate(); err != nil {
		t.Fatal(err)
	}
	if iss.ActiveKeyID() == oldKid {
		t.Fatal("rotation did not change the active key")
	}
	if len(iss.JWKS().Keys) != 2 {
		t.Fatalf("both keys must remain published during the overlap, got %d", len(iss.JWKS().Keys))
	}

	v := NewVerifier(VerifierOptions{
		Issuer: "https://cp.test", Audience: "https://gw.test", StaticKeys: iss.PublicKeys(),
	})
	if _, err := v.Verify(context.Background(), before); err != nil {
		t.Errorf("a token signed before rotation must still verify: %v", err)
	}
}

func TestClientCredentialVerification(t *testing.T) {
	cred, secret, err := NewClientSecret("cli_1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !cred.Verify(secret) {
		t.Fatal("the issued secret must verify")
	}
	if cred.Verify(secret + "x") {
		t.Error("a wrong secret must not verify")
	}
	expired, s2, _ := NewClientSecret("cli_2", -time.Second)
	if expired.Verify(s2) {
		t.Error("an expired credential must not verify")
	}
	retired, s3, _ := NewClientSecret("cli_3", time.Hour)
	retired.Retired = true
	if retired.Verify(s3) {
		t.Error("a retired credential must not verify")
	}
}

func TestFileSecretStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileSecretStore(dir + "/secrets.json")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := store.Put(ctx, "agentgate/clients/cli_1", []byte("s3cret")); err != nil {
		t.Fatal(err)
	}
	value, _, _, err := store.Get(ctx, "agentgate/clients/cli_1")
	if err != nil || string(value) != "s3cret" {
		t.Fatalf("get = %q, %v", value, err)
	}
	reopened, err := NewFileSecretStore(dir + "/secrets.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := reopened.Get(ctx, "agentgate/clients/cli_1"); err != nil {
		t.Errorf("secret did not survive a reopen: %v", err)
	}
	if err := store.Delete(ctx, "agentgate/clients/cli_1"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.Get(ctx, "agentgate/clients/cli_1"); err == nil {
		t.Error("a deleted secret must not be readable")
	}
}

func TestBearerToken(t *testing.T) {
	h := http.Header{}
	if _, ok := BearerToken(h); ok {
		t.Error("an empty header must not yield a token")
	}
	h.Set("Authorization", "Bearer abc.def.ghi")
	if tok, ok := BearerToken(h); !ok || tok != "abc.def.ghi" {
		t.Errorf("got %q %v", tok, ok)
	}
	h.Set("Authorization", "bearer abc")
	if _, ok := BearerToken(h); !ok {
		t.Error("the scheme must be matched case-insensitively")
	}
	h.Set("Authorization", "Basic abc")
	if _, ok := BearerToken(h); ok {
		t.Error("a non-bearer scheme must be rejected")
	}
}

func TestJWKSColdStartIgnoresTheCooldown(t *testing.T) {
	// The gateway's first fetch can race the control plane coming up. When it
	// does, the cache is empty and the next token must trigger a retry rather
	// than being refused for a whole cooldown window.
	var serving atomic.Bool
	iss := NewIssuer(IssuerOptions{Issuer: "https://cp.test", Audience: "https://gw.test", TTL: time.Minute})
	if _, err := iss.GenerateKey(2048); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !serving.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(iss.JWKS())
	}))
	defer srv.Close()

	cache := NewJWKSCache(JWKSOptions{URL: srv.URL, Cooldown: time.Hour})
	if err := cache.Refresh(context.Background()); err == nil {
		t.Fatal("the first fetch was supposed to fail")
	}

	serving.Store(true)
	id, _ := ParseAgentIdentity("agent://fsclient/team/agent")
	token, _, err := iss.Mint(TokenRequest{
		AgentID: "agt_1", Identity: id, AgentVersion: "1", Env: EnvDev,
		Scopes: []string{ScopeInvoke}, ModelPools: []string{"p"},
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	v := NewVerifier(VerifierOptions{Issuer: "https://cp.test", Audience: "https://gw.test", JWKS: cache})
	if _, err := v.Verify(context.Background(), token); err != nil {
		t.Fatalf("a cold cache must retry immediately rather than refuse for the cooldown: %v", err)
	}
}
