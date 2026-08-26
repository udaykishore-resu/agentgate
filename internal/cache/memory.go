package cache

import (
	"container/list"
	"context"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

type node struct {
	key       string
	tenant    string
	pool      string
	entry     *Entry
	expiresAt time.Time
	elem      *list.Element
	size      int64
}

// Memory is an LRU cache with per-entry TTL and a byte budget.
//
// A byte budget rather than an entry count, because cached completions vary in
// size by three orders of magnitude and an entry-count limit sized for short
// answers will happily hold a gigabyte of long ones.
type Memory struct {
	mu       sync.Mutex
	items    map[string]*node
	lru      *list.List
	byTenant map[string]map[string]*node
	maxBytes int64
	bytes    int64

	hits    atomic.Int64
	misses  atomic.Int64
	evicted atomic.Int64
	expired atomic.Int64
	now     func() time.Time
}

// NewMemory builds an in-process cache with a byte budget.
func NewMemory(maxBytes int64) *Memory {
	if maxBytes <= 0 {
		maxBytes = 256 << 20
	}
	return &Memory{
		items: map[string]*node{}, lru: list.New(),
		byTenant: map[string]map[string]*node{}, maxBytes: maxBytes, now: time.Now,
	}
}

// Get returns a cached entry, removing it if it has expired.
func (m *Memory) Get(_ context.Context, key string) (*Entry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.items[key]
	if !ok {
		m.misses.Add(1)
		return nil, false
	}
	if m.now().After(n.expiresAt) {
		m.removeLocked(n)
		m.expired.Add(1)
		m.misses.Add(1)
		return nil, false
	}
	m.lru.MoveToFront(n.elem)
	m.hits.Add(1)
	return n.entry, true
}

// SetWithScope stores an entry and records the tenant and pool it belongs to,
// which is what makes tenant-scoped purge and semantic candidate lookup
// possible.
func (m *Memory) SetWithScope(_ context.Context, key, tenant, pool string, e *Entry, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	size := estimateSize(e)
	m.mu.Lock()
	defer m.mu.Unlock()
	if n, ok := m.items[key]; ok {
		m.removeLocked(n)
	}
	n := &node{key: key, tenant: tenant, pool: pool, entry: e, expiresAt: m.now().Add(ttl), size: size}
	n.elem = m.lru.PushFront(n)
	m.items[key] = n
	if m.byTenant[tenant] == nil {
		m.byTenant[tenant] = map[string]*node{}
	}
	m.byTenant[tenant][key] = n
	m.bytes += size
	for m.bytes > m.maxBytes && m.lru.Len() > 0 {
		back := m.lru.Back()
		if back == nil {
			break
		}
		m.removeLocked(back.Value.(*node))
		m.evicted.Add(1)
	}
}

// Set stores an entry without tenant scoping. Prefer SetWithScope.
func (m *Memory) Set(ctx context.Context, key string, e *Entry, ttl time.Duration) {
	m.SetWithScope(ctx, key, "", "", e, ttl)
}

func (m *Memory) removeLocked(n *node) {
	m.lru.Remove(n.elem)
	delete(m.items, n.key)
	if t, ok := m.byTenant[n.tenant]; ok {
		delete(t, n.key)
		if len(t) == 0 {
			delete(m.byTenant, n.tenant)
		}
	}
	m.bytes -= n.size
}

// Delete removes one entry.
func (m *Memory) Delete(_ context.Context, key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n, ok := m.items[key]; ok {
		m.removeLocked(n)
	}
}

// Purge removes every entry for a tenant. This is the emergency control when a
// cache is suspected of holding a wrong or unsafe response.
func (m *Memory) Purge(_ context.Context, tenant string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if tenant == "" {
		n := len(m.items)
		m.items = map[string]*node{}
		m.byTenant = map[string]map[string]*node{}
		m.lru.Init()
		m.bytes = 0
		return n
	}
	entries := m.byTenant[tenant]
	n := len(entries)
	for _, node := range entries {
		m.removeLocked(node)
	}
	return n
}

// Candidates returns recent entries with embeddings for one tenant and pool.
func (m *Memory) Candidates(_ context.Context, tenant, pool string, limit int) []*Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Entry, 0, limit)
	now := m.now()
	for e := m.lru.Front(); e != nil && len(out) < limit; e = e.Next() {
		n := e.Value.(*node)
		if n.tenant != tenant || n.pool != pool || now.After(n.expiresAt) {
			continue
		}
		if len(n.entry.Embedding) == 0 {
			continue
		}
		out = append(out, n.entry)
	}
	return out
}

// Stats reports cache behaviour.
func (m *Memory) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Stats{
		Entries: len(m.items), Hits: m.hits.Load(), Misses: m.misses.Load(),
		Evicted: m.evicted.Load(), Expired: m.expired.Load(), BytesApx: m.bytes,
	}
}

func estimateSize(e *Entry) int64 {
	if e == nil {
		return 0
	}
	size := int64(256)
	if e.Response != nil {
		for _, c := range e.Response.Choices {
			if c.Message != nil {
				size += int64(len(c.Message.Content))
			}
		}
	}
	size += int64(len(e.Embedding) * 8)
	size += int64(len(e.Text))
	return size
}

// CosineSimilarity returns the cosine similarity of two vectors, or -1 when
// they are not comparable.
func CosineSimilarity(a, b []float64) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return -1
	}
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return -1
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// BestMatch returns the highest-similarity candidate at or above threshold.
func BestMatch(query []float64, candidates []*Entry, threshold float64) (*Entry, float64) {
	var best *Entry
	bestScore := -1.0
	for _, c := range candidates {
		s := CosineSimilarity(query, c.Embedding)
		if s > bestScore {
			best, bestScore = c, s
		}
	}
	if best != nil && bestScore >= threshold {
		return best, bestScore
	}
	return nil, bestScore
}
