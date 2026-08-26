package identity

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// SigningKey is one key in the issuer's rotation set.
type SigningKey struct {
	Kid       string
	Private   *rsa.PrivateKey
	CreatedAt time.Time
	// Retired keys still serve verification through the JWKS but no longer
	// sign. A key stays published for at least one token lifetime past
	// retirement so tokens signed just before rotation remain verifiable.
	Retired bool
}

// Issuer mints AgentGate access tokens and publishes the JWKS the gateway
// verifies against.
type Issuer struct {
	issuer   string
	audience string
	ttl      time.Duration

	mu      sync.RWMutex
	keys    map[string]*SigningKey
	active  string
	onMint  func(result, grant string)
	nowFunc func() time.Time
}

// IssuerOptions configures an Issuer.
type IssuerOptions struct {
	Issuer   string
	Audience string
	// TTL is the access token lifetime. Short by design: an agent's identity
	// is re-proved often, so revocation is a matter of minutes without a
	// revocation list on the request path.
	TTL    time.Duration
	OnMint func(result, grant string)
	Now    func() time.Time
}

// NewIssuer builds an Issuer with no keys; call AddKey or GenerateKey.
func NewIssuer(o IssuerOptions) *Issuer {
	if o.TTL <= 0 {
		o.TTL = 15 * time.Minute
	}
	if o.OnMint == nil {
		o.OnMint = func(string, string) {}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Issuer{
		issuer: o.Issuer, audience: o.Audience, ttl: o.TTL,
		keys: map[string]*SigningKey{}, onMint: o.OnMint, nowFunc: o.Now,
	}
}

// GenerateKey creates a new RSA signing key and makes it active.
func (i *Issuer) GenerateKey(bits int) (*SigningKey, error) {
	if bits < 2048 {
		bits = 2048
	}
	priv, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		return nil, err
	}
	return i.AddKey(priv, true)
}

// LoadKeyPEM loads a PKCS#1 or PKCS#8 RSA private key from disk.
func (i *Issuer) LoadKeyPEM(path string, makeActive bool) (*SigningKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read signing key: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("signing key is not PEM encoded")
	}
	var priv *rsa.PrivateKey
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		priv = k
	} else if any8, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rk, ok := any8.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("signing key is not RSA")
		}
		priv = rk
	} else {
		return nil, errors.New("signing key could not be parsed as PKCS#1 or PKCS#8")
	}
	return i.AddKey(priv, makeActive)
}

// AddKey registers a key, deriving its key id from the public key so the id is
// stable across restarts and reproducible from the JWKS alone.
func (i *Issuer) AddKey(priv *rsa.PrivateKey, makeActive bool) (*SigningKey, error) {
	kid, err := thumbprint(&priv.PublicKey)
	if err != nil {
		return nil, err
	}
	k := &SigningKey{Kid: kid, Private: priv, CreatedAt: i.nowFunc()}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.keys[kid] = k
	if makeActive || i.active == "" {
		if prev, ok := i.keys[i.active]; ok && prev.Kid != kid {
			prev.Retired = true
		}
		i.active = kid
	}
	return k, nil
}

// thumbprint computes the RFC 7638 JWK thumbprint used as the key id.
func thumbprint(pub *rsa.PublicKey) (string, error) {
	n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(bigEndian(pub.E))
	canonical := fmt.Sprintf(`{"e":"%s","kty":"RSA","n":"%s"}`, e, n)
	sum := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func bigEndian(e int) []byte {
	var b []byte
	for e > 0 {
		b = append([]byte{byte(e & 0xff)}, b...)
		e >>= 8
	}
	if len(b) == 0 {
		b = []byte{0}
	}
	return b
}

// TokenRequest describes the token to mint.
type TokenRequest struct {
	AgentID            string
	Identity           AgentIdentity
	AgentVersion       string
	Env                Environment
	CostCenter         string
	OwnerEmail         string
	Runtime            string
	Framework          string
	Scopes             []string
	ModelPools         []string
	Attestation        Attestation
	DataClassification string
	// TTL overrides the issuer default, used to shorten a developer token.
	TTL time.Duration
}

// Mint signs an access token for a registered agent version.
func (i *Issuer) Mint(req TokenRequest, grant string) (string, *Claims, error) {
	i.mu.RLock()
	key := i.keys[i.active]
	i.mu.RUnlock()
	if key == nil {
		i.onMint("error", grant)
		return "", nil, errors.New("issuer has no active signing key")
	}
	ttl := i.ttl
	if req.TTL > 0 {
		ttl = req.TTL
	}
	now := i.nowFunc()
	claims := &Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    i.issuer,
			Subject:   req.Identity.String(),
			Audience:  jwt.ClaimStrings{i.audience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now.Add(-5 * time.Second)),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			ID:        uuid.NewString(),
		},
		AgentID:            req.AgentID,
		Tenant:             req.Identity.Tenant,
		Team:               req.Identity.Team,
		AgentName:          req.Identity.Name,
		AgentVersion:       req.AgentVersion,
		Env:                req.Env,
		CostCenter:         req.CostCenter,
		OwnerEmail:         req.OwnerEmail,
		Runtime:            req.Runtime,
		Framework:          req.Framework,
		Scopes:             req.Scopes,
		ModelPools:         req.ModelPools,
		Attestation:        req.Attestation,
		DataClassification: req.DataClassification,
	}
	if err := claims.Validate(); err != nil {
		i.onMint("error", grant)
		return "", nil, fmt.Errorf("refusing to mint an invalid token: %w", err)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = key.Kid
	signed, err := tok.SignedString(key.Private)
	if err != nil {
		i.onMint("error", grant)
		return "", nil, err
	}
	i.onMint("success", grant)
	return signed, claims, nil
}

// TTL reports the configured access-token lifetime.
func (i *Issuer) TTL() time.Duration { return i.ttl }

// JWKS renders the public key set, including retired keys still inside their
// verification window.
func (i *Issuer) JWKS() JWKSet {
	i.mu.RLock()
	defer i.mu.RUnlock()
	set := JWKSet{}
	for _, k := range i.keys {
		pub := &k.Private.PublicKey
		set.Keys = append(set.Keys, JWK{
			Kty: "RSA",
			Kid: k.Kid,
			Use: "sig",
			Alg: "RS256",
			N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			E:   base64.RawURLEncoding.EncodeToString(bigEndian(pub.E)),
		})
	}
	return set
}

// ActiveKeyID returns the key id currently used for signing.
func (i *Issuer) ActiveKeyID() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.active
}

// Keys returns a snapshot of the key set for age reporting.
func (i *Issuer) Keys() []SigningKey {
	i.mu.RLock()
	defer i.mu.RUnlock()
	out := make([]SigningKey, 0, len(i.keys))
	for _, k := range i.keys {
		out = append(out, *k)
	}
	return out
}

// Rotate generates a new active key and retires the previous one. The retired
// key keeps serving verification until Prune removes it, which must not happen
// before every token it signed has expired.
func (i *Issuer) Rotate() (*SigningKey, error) { return i.GenerateKey(2048) }

// Prune removes retired keys older than the retention window.
func (i *Issuer) Prune(retention time.Duration) int {
	i.mu.Lock()
	defer i.mu.Unlock()
	removed := 0
	for kid, k := range i.keys {
		if k.Retired && i.nowFunc().Sub(k.CreatedAt) > retention && kid != i.active {
			delete(i.keys, kid)
			removed++
		}
	}
	return removed
}

// PublicKeys exposes the trusted public keys for an in-process verifier, used
// by the control plane's own API and by tests.
func (i *Issuer) PublicKeys() map[string]crypto.PublicKey {
	i.mu.RLock()
	defer i.mu.RUnlock()
	out := make(map[string]crypto.PublicKey, len(i.keys))
	for kid, k := range i.keys {
		out[kid] = &k.Private.PublicKey
	}
	return out
}
