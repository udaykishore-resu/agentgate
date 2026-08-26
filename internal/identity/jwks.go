package identity

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// JWK is one key from a JSON Web Key Set.
type JWK struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use,omitempty"`
	Alg string `json:"alg,omitempty"`
	N   string `json:"n,omitempty"`
	E   string `json:"e,omitempty"`
	Crv string `json:"crv,omitempty"`
	X   string `json:"x,omitempty"`
	Y   string `json:"y,omitempty"`
}

// JWKSet is a JSON Web Key Set document.
type JWKSet struct {
	Keys []JWK `json:"keys"`
}

// Public converts a JWK into a public key usable for signature verification.
func (k JWK) Public() (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		n, err := b64uint(k.N)
		if err != nil {
			return nil, fmt.Errorf("jwk %s: modulus: %w", k.Kid, err)
		}
		e, err := b64uint(k.E)
		if err != nil {
			return nil, fmt.Errorf("jwk %s: exponent: %w", k.Kid, err)
		}
		if !e.IsInt64() || e.Int64() > 1<<31 {
			return nil, fmt.Errorf("jwk %s: implausible exponent", k.Kid)
		}
		return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
	case "EC":
		var curve elliptic.Curve
		switch k.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, fmt.Errorf("jwk %s: unsupported curve %q", k.Kid, k.Crv)
		}
		x, err := b64uint(k.X)
		if err != nil {
			return nil, fmt.Errorf("jwk %s: x: %w", k.Kid, err)
		}
		y, err := b64uint(k.Y)
		if err != nil {
			return nil, fmt.Errorf("jwk %s: y: %w", k.Kid, err)
		}
		return &ecdsa.PublicKey{Curve: curve, X: x, Y: y}, nil
	default:
		return nil, fmt.Errorf("jwk %s: unsupported key type %q", k.Kid, k.Kty)
	}
}

func b64uint(s string) (*big.Int, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(raw), nil
}

// JWKSCache fetches and caches a JWKS, refreshing on a schedule and on a
// cache miss for an unknown key id.
//
// Two properties matter operationally. First, an unknown kid triggers at most
// one refresh per cooldown window, so a flood of tokens signed by a key the
// gateway has never seen cannot turn into a denial-of-service against the
// control plane. Second, a stale key set is served rather than failing closed
// when the control plane is unreachable: an issuer outage must not take the
// traffic plane down with it. Staleness is bounded and alerted on.
type JWKSCache struct {
	url       string
	client    *http.Client
	refresh   time.Duration
	cooldown  time.Duration
	maxStale  time.Duration
	onRefresh func(result string)

	mu          sync.RWMutex
	keys        map[string]crypto.PublicKey
	fetchedAt   time.Time
	lastAttempt time.Time
}

// JWKSOptions configures a JWKSCache.
type JWKSOptions struct {
	URL      string
	Client   *http.Client
	Refresh  time.Duration // background refresh interval
	Cooldown time.Duration // minimum gap between unknown-kid refreshes
	MaxStale time.Duration // how long a stale set may still be trusted
	// OnRefresh receives "ok", "error" or "stale" for metrics.
	OnRefresh func(result string)
}

// NewJWKSCache builds a cache. It does not fetch until Start or Key is called.
func NewJWKSCache(o JWKSOptions) *JWKSCache {
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 5 * time.Second}
	}
	if o.Refresh <= 0 {
		o.Refresh = 10 * time.Minute
	}
	if o.Cooldown <= 0 {
		o.Cooldown = 30 * time.Second
	}
	if o.MaxStale <= 0 {
		o.MaxStale = 24 * time.Hour
	}
	if o.OnRefresh == nil {
		o.OnRefresh = func(string) {}
	}
	return &JWKSCache{
		url: o.URL, client: o.Client, refresh: o.Refresh,
		cooldown: o.Cooldown, maxStale: o.MaxStale, onRefresh: o.OnRefresh,
		keys: map[string]crypto.PublicKey{},
	}
}

// Start performs an initial fetch and then refreshes in the background until
// ctx is cancelled. A failed initial fetch is not fatal; the first request
// that needs a key will retry.
func (c *JWKSCache) Start(ctx context.Context) {
	_ = c.Refresh(ctx)
	go func() {
		t := time.NewTicker(c.refresh)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = c.Refresh(ctx)
			}
		}
	}()
}

// Refresh fetches the key set immediately.
func (c *JWKSCache) Refresh(ctx context.Context) error {
	c.mu.Lock()
	c.lastAttempt = time.Now()
	c.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		c.onRefresh("error")
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		c.onRefresh("error")
		return fmt.Errorf("fetch jwks: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		c.onRefresh("error")
		return fmt.Errorf("fetch jwks: %s", resp.Status)
	}
	var set JWKSet
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		c.onRefresh("error")
		return fmt.Errorf("decode jwks: %w", err)
	}
	keys := make(map[string]crypto.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		pub, err := k.Public()
		if err != nil {
			continue // one unusable key must not invalidate the set
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		c.onRefresh("error")
		return fmt.Errorf("jwks contained no usable signing keys")
	}
	c.mu.Lock()
	c.keys = keys
	c.fetchedAt = time.Now()
	c.mu.Unlock()
	c.onRefresh("ok")
	return nil
}

// Key returns the public key for a key id, refreshing once per cooldown window
// when the id is unknown.
func (c *JWKSCache) Key(ctx context.Context, kid string) (crypto.PublicKey, error) {
	c.mu.RLock()
	key, ok := c.keys[kid]
	fetched, attempted := c.fetchedAt, c.lastAttempt
	c.mu.RUnlock()
	if ok {
		if !fetched.IsZero() && time.Since(fetched) > c.maxStale {
			return nil, fmt.Errorf("jwks is stale beyond %s and cannot be refreshed", c.maxStale)
		}
		return key, nil
	}
	// The cooldown exists to stop a flood of tokens signed by an unknown key
	// turning into a denial-of-service against the control plane. It must not
	// apply when the cache is simply empty: that is a cold start, where the
	// first fetch raced the issuer coming up, and refusing for a whole
	// cooldown window would make every gateway restart begin with a period of
	// rejecting perfectly valid tokens.
	c.mu.RLock()
	cold := len(c.keys) == 0
	c.mu.RUnlock()
	if !cold && time.Since(attempted) < c.cooldown {
		return nil, fmt.Errorf("unknown key id %q", kid)
	}
	if err := c.Refresh(ctx); err != nil {
		return nil, fmt.Errorf("unknown key id %q and refresh failed: %w", kid, err)
	}
	c.mu.RLock()
	key, ok = c.keys[kid]
	c.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown key id %q", kid)
	}
	return key, nil
}

// Age reports how long ago the key set was successfully fetched.
func (c *JWKSCache) Age() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.fetchedAt.IsZero() {
		return 0
	}
	return time.Since(c.fetchedAt)
}

// KeyIDs lists the currently trusted key ids.
func (c *JWKSCache) KeyIDs() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.keys))
	for k := range c.keys {
		out = append(out, k)
	}
	return out
}
