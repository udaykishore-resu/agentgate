package cache

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/agentgate/agentgate/internal/provider"
)

func req(text string) *provider.ChatRequest {
	temp := 0.0
	return &provider.ChatRequest{
		Model:       "general-chat",
		Messages:    []provider.Message{provider.TextMessage("user", text)},
		Temperature: &temp,
	}
}

func TestKeyIsStableAndTenantScoped(t *testing.T) {
	a := Key("tenant-a", "general-chat", req("what is the dispute status"))
	b := Key("tenant-a", "general-chat", req("what   is the   dispute status"))
	if a != b {
		t.Error("whitespace differences must not defeat the cache")
	}
	if Key("tenant-b", "general-chat", req("what is the dispute status")) == a {
		t.Fatal("two tenants must never share a cache key")
	}
	if Key("tenant-a", "long-context", req("what is the dispute status")) == a {
		t.Error("the logical model must be part of the key")
	}
	different := req("what is the dispute status")
	max := 100
	different.MaxTokens = &max
	if Key("tenant-a", "general-chat", different) == a {
		t.Error("max_tokens must be part of the key")
	}
}

func TestKeyIgnoresToolDefinitionOrdering(t *testing.T) {
	withTools := func(order ...string) *provider.ChatRequest {
		r := req("go")
		for _, name := range order {
			r.Tools = append(r.Tools, provider.Tool{
				Type:     "function",
				Function: json.RawMessage(`{"name":"` + name + `","description":"d","parameters":{"b":1,"a":2}}`),
			})
		}
		return r
	}
	// Object key ordering inside a tool schema must not change the cache key;
	// the order of the tool list itself is meaningful and is preserved.
	one := withTools("alpha")
	two := withTools("alpha")
	two.Tools[0].Function = json.RawMessage(`{"parameters":{"a":2,"b":1},"description":"d","name":"alpha"}`)
	if Key("t", "m", one) != Key("t", "m", two) {
		t.Error("JSON key ordering inside a tool schema must not change the cache key")
	}
}

func TestCacheablePolicy(t *testing.T) {
	p := DefaultPolicy()
	if ok, why := Cacheable(p, req("hello")); !ok {
		t.Errorf("a plain low-temperature request should be cacheable: %s", why)
	}

	hot := req("hello")
	temp := 0.9
	hot.Temperature = &temp
	if ok, _ := Cacheable(p, hot); ok {
		t.Error("a high-temperature request must not be cached")
	}

	tooled := req("hello")
	tooled.Tools = []provider.Tool{{Type: "function", Function: json.RawMessage(`{"name":"x"}`)}}
	if ok, _ := Cacheable(p, tooled); ok {
		t.Error("tool-calling requests must not be cached by default")
	}
	p.AllowTools = true
	if ok, why := Cacheable(p, tooled); !ok {
		t.Errorf("tool calls should be cacheable when explicitly allowed: %s", why)
	}

	multi := req("hello")
	n := 3
	multi.N = &n
	if ok, _ := Cacheable(DefaultPolicy(), multi); ok {
		t.Error("a request for several completions must not be cached")
	}

	off := DefaultPolicy()
	off.Enabled = false
	if ok, why := Cacheable(off, req("hello")); ok || why == "" {
		t.Error("a disabled cache must report why it was bypassed")
	}
}

func TestMemoryCacheHitMissAndExpiry(t *testing.T) {
	now := time.Now()
	c := NewMemory(1 << 20)
	c.now = func() time.Time { return now }
	ctx := context.Background()

	entry := &Entry{Response: &provider.ChatResponse{ID: "r1"}, TraceID: "trace-1"}
	c.SetWithScope(ctx, "k", "tenant-a", "general-chat", entry, time.Minute)

	got, ok := c.Get(ctx, "k")
	if !ok || got.TraceID != "trace-1" {
		t.Fatalf("expected a hit carrying provenance, got %#v %v", got, ok)
	}
	if _, ok := c.Get(ctx, "other"); ok {
		t.Error("an unknown key must miss")
	}

	now = now.Add(2 * time.Minute)
	if _, ok := c.Get(ctx, "k"); ok {
		t.Error("an expired entry must not be served")
	}
	if c.Stats().Expired == 0 {
		t.Error("expiries must be counted")
	}
}

func TestMemoryCacheEvictsUnderByteBudget(t *testing.T) {
	c := NewMemory(4096)
	ctx := context.Background()
	big := func(id string) *Entry {
		msg := provider.TextMessage("assistant", string(make([]byte, 1024)))
		return &Entry{Response: &provider.ChatResponse{
			ID: id, Choices: []provider.Choice{{Message: &msg}},
		}}
	}
	for i := 0; i < 20; i++ {
		c.SetWithScope(ctx, string(rune('a'+i)), "t", "p", big("r"), time.Minute)
	}
	stats := c.Stats()
	if stats.BytesApx > 4096 {
		t.Errorf("cache exceeded its byte budget: %d", stats.BytesApx)
	}
	if stats.Evicted == 0 {
		t.Error("entries must be evicted once the budget is reached")
	}
}

func TestPurgeIsTenantScoped(t *testing.T) {
	c := NewMemory(1 << 20)
	ctx := context.Background()
	c.SetWithScope(ctx, "a", "tenant-a", "p", &Entry{Response: &provider.ChatResponse{}}, time.Minute)
	c.SetWithScope(ctx, "b", "tenant-b", "p", &Entry{Response: &provider.ChatResponse{}}, time.Minute)

	if n := c.Purge(ctx, "tenant-a"); n != 1 {
		t.Fatalf("purged %d entries, want 1", n)
	}
	if _, ok := c.Get(ctx, "a"); ok {
		t.Error("the purged tenant's entry must be gone")
	}
	if _, ok := c.Get(ctx, "b"); !ok {
		t.Error("another tenant's entry must survive a scoped purge")
	}
}

func TestCosineSimilarityAndBestMatch(t *testing.T) {
	if got := CosineSimilarity([]float64{1, 0}, []float64{1, 0}); got < 0.999 {
		t.Errorf("identical vectors = %f", got)
	}
	if got := CosineSimilarity([]float64{1, 0}, []float64{0, 1}); got > 0.001 {
		t.Errorf("orthogonal vectors = %f", got)
	}
	if got := CosineSimilarity([]float64{1, 0}, []float64{1, 0, 0}); got != -1 {
		t.Errorf("mismatched lengths must be incomparable, got %f", got)
	}

	candidates := []*Entry{
		{Text: "far", Embedding: []float64{0, 1}},
		{Text: "near", Embedding: []float64{0.99, 0.01}},
	}
	best, score := BestMatch([]float64{1, 0}, candidates, 0.97)
	if best == nil || best.Text != "near" {
		t.Fatalf("best match = %#v (score %f)", best, score)
	}
	if _, _ = BestMatch([]float64{1, 0}, candidates, 0.9999); best == nil {
		t.Skip()
	}
	if strict, _ := BestMatch([]float64{1, 0}, candidates, 0.99999); strict != nil {
		t.Error("a threshold above every candidate must return no match")
	}
}

func TestCandidatesAreScopedAndRequireEmbeddings(t *testing.T) {
	c := NewMemory(1 << 20)
	ctx := context.Background()
	c.SetWithScope(ctx, "1", "t", "p", &Entry{Response: &provider.ChatResponse{}, Embedding: []float64{1, 0}}, time.Minute)
	c.SetWithScope(ctx, "2", "t", "p", &Entry{Response: &provider.ChatResponse{}}, time.Minute)
	c.SetWithScope(ctx, "3", "other", "p", &Entry{Response: &provider.ChatResponse{}, Embedding: []float64{1, 0}}, time.Minute)

	got := c.Candidates(ctx, "t", "p", 10)
	if len(got) != 1 {
		t.Fatalf("expected exactly the embedded, in-scope entry, got %d", len(got))
	}
}
