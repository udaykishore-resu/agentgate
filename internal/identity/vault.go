package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ErrSecretNotFound is returned when a secret does not exist.
var ErrSecretNotFound = errors.New("secret not found")

// SecretStore abstracts the enterprise vault. Production binds this to Azure
// Key Vault or AWS Secrets Manager reached over a private endpoint with
// workload identity; the file implementation exists so the stack runs locally
// and so tests never need a vault.
//
// The interface is deliberately narrow. A store that can only put, get,
// version and delete cannot grow into a general-purpose configuration service,
// which is how secret stores end up holding things that should not be secret.
type SecretStore interface {
	Put(ctx context.Context, name string, value []byte) (version string, err error)
	Get(ctx context.Context, name string) (value []byte, version string, createdAt time.Time, err error)
	Delete(ctx context.Context, name string) error
	List(ctx context.Context, prefix string) ([]SecretMeta, error)
}

// SecretMeta describes a stored secret without revealing it.
type SecretMeta struct {
	Name      string    `json:"name"`
	Version   string    `json:"version"`
	CreatedAt time.Time `json:"created_at"`
}

// Age reports how long ago the secret was written, which is what the rotation
// alert fires on.
func (m SecretMeta) Age() time.Duration { return time.Since(m.CreatedAt) }

type fileEntry struct {
	Value     string    `json:"value"`
	Version   string    `json:"version"`
	CreatedAt time.Time `json:"created_at"`
}

// FileSecretStore is a development SecretStore backed by a single JSON file
// with 0600 permissions. It is refused in production by config validation.
type FileSecretStore struct {
	path string
	mu   sync.RWMutex
	data map[string]fileEntry
}

// NewFileSecretStore opens or creates a file-backed store.
func NewFileSecretStore(path string) (*FileSecretStore, error) {
	s := &FileSecretStore{path: path, data: map[string]fileEntry{}}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &s.data); err != nil {
			return nil, fmt.Errorf("parse secret store: %w", err)
		}
	case os.IsNotExist(err):
	default:
		return nil, err
	}
	return s, nil
}

func (s *FileSecretStore) flushLocked() error {
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Put stores a secret and returns its new version.
func (s *FileSecretStore) Put(_ context.Context, name string, value []byte) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sum := sha256.Sum256(value)
	e := fileEntry{
		Value:     base64.StdEncoding.EncodeToString(value),
		Version:   hex.EncodeToString(sum[:8]),
		CreatedAt: time.Now().UTC(),
	}
	s.data[name] = e
	return e.Version, s.flushLocked()
}

// Get retrieves a secret.
func (s *FileSecretStore) Get(_ context.Context, name string) ([]byte, string, time.Time, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.data[name]
	if !ok {
		return nil, "", time.Time{}, ErrSecretNotFound
	}
	raw, err := base64.StdEncoding.DecodeString(e.Value)
	if err != nil {
		return nil, "", time.Time{}, err
	}
	return raw, e.Version, e.CreatedAt, nil
}

// Delete removes a secret.
func (s *FileSecretStore) Delete(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, name)
	return s.flushLocked()
}

// List returns metadata for secrets under a prefix.
func (s *FileSecretStore) List(_ context.Context, prefix string) ([]SecretMeta, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []SecretMeta
	for name, e := range s.data {
		if prefix == "" || len(name) >= len(prefix) && name[:len(prefix)] == prefix {
			out = append(out, SecretMeta{Name: name, Version: e.Version, CreatedAt: e.CreatedAt})
		}
	}
	return out, nil
}

// ClientCredential is the fallback credential for runtimes with no workload
// identity. Only the hash is persisted in the registry; the secret itself
// lives in the vault and is returned to the caller exactly once.
type ClientCredential struct {
	ClientID   string    `json:"client_id"`
	SecretHash string    `json:"secret_hash"`
	Version    string    `json:"version"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Retired    bool      `json:"retired"`
}

// SecretName is the vault key for this credential.
func (c ClientCredential) SecretName() string { return "agentgate/clients/" + c.ClientID }

// NewClientSecret generates a credential and returns the plaintext secret,
// which the caller must hand to the requester and then forget.
func NewClientSecret(clientID string, lifetime time.Duration) (ClientCredential, string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return ClientCredential{}, "", err
	}
	secret := "ags_" + base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(secret))
	now := time.Now().UTC()
	return ClientCredential{
		ClientID:   clientID,
		SecretHash: hex.EncodeToString(sum[:]),
		CreatedAt:  now,
		ExpiresAt:  now.Add(lifetime),
	}, secret, nil
}

// Verify checks a presented secret in constant time.
func (c ClientCredential) Verify(presented string) bool {
	if c.Retired || time.Now().After(c.ExpiresAt) {
		return false
	}
	sum := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(c.SecretHash)) == 1
}
