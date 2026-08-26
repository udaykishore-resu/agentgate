package identity

import (
	"context"
	"crypto"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// VerifyResult carries the outcome of token verification. Reason is for the
// span, the log and the metric label; it is never returned to the caller,
// because a precise authentication error is an oracle.
type VerifyResult struct {
	Claims *Claims
	Reason string
}

// Verifier validates AgentGate access tokens at the gateway.
type Verifier struct {
	issuer       string
	audience     string
	jwks         *JWKSCache
	staticKeys   map[string]crypto.PublicKey
	clockSkew    time.Duration
	replay       *replayCache
	allowedAlgs  []string
	allowGeneric bool
	onValidate   func(result, reason string)
}

// VerifierOptions configures a Verifier.
type VerifierOptions struct {
	Issuer   string
	Audience string
	JWKS     *JWKSCache
	// StaticKeys allows an air-gapped or bootstrap deployment to trust a key
	// from disk instead of a JWKS endpoint.
	StaticKeys map[string]crypto.PublicKey
	ClockSkew  time.Duration
	// SingleUseWindow enables jti replay rejection, making a token valid for
	// exactly one presentation.
	//
	// This is correct for one-time client assertions on the token-exchange
	// path and WRONG for access tokens, which an agent legitimately reuses for
	// every request until they expire. The gateway therefore leaves it at
	// zero; the control plane enables it when verifying subject tokens.
	SingleUseWindow time.Duration
	// AllowGenericClaims skips the AgentGate claim checks, for verifying a
	// platform-issued subject token (a Kubernetes projected service account
	// token, a managed-identity token, a SPIFFE JWT-SVID) that predates the
	// agent's AgentGate identity.
	AllowGenericClaims bool
	OnValidate         func(result, reason string)
}

// NewVerifier builds a Verifier.
func NewVerifier(o VerifierOptions) *Verifier {
	if o.ClockSkew <= 0 {
		o.ClockSkew = 30 * time.Second
	}
	if o.OnValidate == nil {
		o.OnValidate = func(string, string) {}
	}
	v := &Verifier{
		issuer:       o.Issuer,
		audience:     o.Audience,
		jwks:         o.JWKS,
		staticKeys:   o.StaticKeys,
		clockSkew:    o.ClockSkew,
		onValidate:   o.OnValidate,
		allowGeneric: o.AllowGenericClaims,
		// Asymmetric algorithms only. Accepting HS256 alongside RS256 is how
		// "alg confusion" turns a public key into a signing secret.
		allowedAlgs: []string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512", "PS256"},
	}
	if o.SingleUseWindow > 0 {
		v.replay = newReplayCache(o.SingleUseWindow)
	}
	return v
}

// Common verification failure reasons, used as metric label values.
const (
	ReasonMissingToken = "missing_token"
	ReasonMalformed    = "malformed"
	ReasonBadSignature = "bad_signature"
	ReasonExpired      = "expired"
	ReasonNotYetValid  = "not_yet_valid"
	ReasonWrongIssuer  = "wrong_issuer"
	ReasonWrongAud     = "wrong_audience"
	ReasonBadClaims    = "bad_claims"
	ReasonReplay       = "replay"
	ReasonUnknownKey   = "unknown_key"
)

// ErrUnauthenticated is returned for every verification failure. The specific
// reason is carried out of band.
type ErrUnauthenticated struct{ Reason string }

// Error implements error.
func (e *ErrUnauthenticated) Error() string { return "unauthenticated: " + e.Reason }

// BearerToken extracts the credential from an Authorization header.
func BearerToken(h http.Header) (string, bool) {
	auth := h.Get("Authorization")
	if auth == "" {
		return "", false
	}
	const prefix = "bearer "
	if len(auth) <= len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(auth[len(prefix):]), true
}

// Verify validates a token and returns its claims.
func (v *Verifier) Verify(ctx context.Context, raw string) (*Claims, error) {
	fail := func(reason string) (*Claims, error) {
		v.onValidate("failure", reason)
		return nil, &ErrUnauthenticated{Reason: reason}
	}
	if raw == "" {
		return fail(ReasonMissingToken)
	}

	claims := &Claims{}
	parser := jwt.NewParser(
		jwt.WithValidMethods(v.allowedAlgs),
		jwt.WithLeeway(v.clockSkew),
		jwt.WithIssuedAt(),
	)
	token, err := parser.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, fmt.Errorf("token has no key id")
		}
		if key, ok := v.staticKeys[kid]; ok {
			return key, nil
		}
		if v.jwks == nil {
			return nil, fmt.Errorf("no key source configured")
		}
		return v.jwks.Key(ctx, kid)
	})
	if err != nil {
		switch {
		case strings.Contains(err.Error(), "unknown key id"), strings.Contains(err.Error(), "no key id"):
			return fail(ReasonUnknownKey)
		case errIs(err, jwt.ErrTokenExpired):
			return fail(ReasonExpired)
		case errIs(err, jwt.ErrTokenNotValidYet), errIs(err, jwt.ErrTokenUsedBeforeIssued):
			return fail(ReasonNotYetValid)
		case errIs(err, jwt.ErrTokenSignatureInvalid):
			return fail(ReasonBadSignature)
		default:
			return fail(ReasonMalformed)
		}
	}
	if !token.Valid {
		return fail(ReasonBadSignature)
	}

	if v.issuer != "" && claims.Issuer != v.issuer {
		return fail(ReasonWrongIssuer)
	}
	if v.audience != "" && !containsAudience(claims.Audience, v.audience) {
		return fail(ReasonWrongAud)
	}
	if !v.allowGeneric {
		if err := claims.Validate(); err != nil {
			return fail(ReasonBadClaims)
		}
	}
	if v.replay != nil && claims.ID != "" {
		exp := time.Now().Add(time.Hour)
		if claims.ExpiresAt != nil {
			exp = claims.ExpiresAt.Time
		}
		if !v.replay.admit(claims.ID, exp) {
			return fail(ReasonReplay)
		}
	}
	v.onValidate("success", "")
	return claims, nil
}

func errIs(err error, target error) bool {
	return err != nil && strings.Contains(err.Error(), target.Error())
}

func containsAudience(aud jwt.ClaimStrings, want string) bool {
	for _, a := range aud {
		if a == want {
			return true
		}
	}
	return false
}

// replayCache rejects a token id that has already been presented, within a
// bounded window. It is per-process: a token replayed against a different
// gateway replica is not caught, which is an accepted limitation documented in
// docs/05-identity.md — the window is short and tokens are single-audience.
// The distributed variant, backed by Redis, is enabled where the threat model
// requires it.
type replayCache struct {
	window time.Duration
	mu     sync.Mutex
	seen   map[string]time.Time
	lastGC time.Time
}

func newReplayCache(window time.Duration) *replayCache {
	return &replayCache{window: window, seen: map[string]time.Time{}, lastGC: time.Now()}
}

func (r *replayCache) admit(jti string, exp time.Time) bool {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if now.Sub(r.lastGC) > r.window {
		for k, v := range r.seen {
			if v.Before(now) {
				delete(r.seen, k)
			}
		}
		r.lastGC = now
	}
	if until, ok := r.seen[jti]; ok && until.After(now) {
		return false
	}
	horizon := now.Add(r.window)
	if exp.Before(horizon) {
		horizon = exp
	}
	r.seen[jti] = horizon
	return true
}
