package yamlite

import (
	"reflect"
	"testing"
)

func TestParseScalarsAndNesting(t *testing.T) {
	got, err := Parse([]byte(`
# a comment
env: prod
service:
  name: agentgate-gateway   # trailing comment
  port: 8080
  ratio: 0.25
  enabled: true
  disabled: false
  nothing: null
  quoted: "value: with colon"
  single: 'it''s fine'
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	m := got.(map[string]any)
	if m["env"] != "prod" {
		t.Errorf("env = %v", m["env"])
	}
	svc := m["service"].(map[string]any)
	for _, tc := range []struct {
		key  string
		want any
	}{
		{"name", "agentgate-gateway"},
		{"port", int64(8080)},
		{"ratio", 0.25},
		{"enabled", true},
		{"disabled", false},
		{"nothing", nil},
		{"quoted", "value: with colon"},
		{"single", "it's fine"},
	} {
		if !reflect.DeepEqual(svc[tc.key], tc.want) {
			t.Errorf("service.%s = %#v, want %#v", tc.key, svc[tc.key], tc.want)
		}
	}
}

func TestParseSequences(t *testing.T) {
	got, err := Parse([]byte(`
pools:
  - name: general-chat
    strategy: weighted
    backends:
      - name: a
        weight: 60
      - name: b
        weight: 40
  - name: long-context
    strategy: priority
simple: [one, two, 3]
inline: {a: 1, b: two}
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	m := got.(map[string]any)
	pools := m["pools"].([]any)
	if len(pools) != 2 {
		t.Fatalf("want 2 pools, got %d", len(pools))
	}
	first := pools[0].(map[string]any)
	if first["name"] != "general-chat" || first["strategy"] != "weighted" {
		t.Errorf("first pool = %#v", first)
	}
	backends := first["backends"].([]any)
	if len(backends) != 2 {
		t.Fatalf("want 2 backends, got %d", len(backends))
	}
	if backends[1].(map[string]any)["weight"] != int64(40) {
		t.Errorf("second backend weight = %#v", backends[1])
	}
	if got := m["simple"].([]any); len(got) != 3 || got[2] != int64(3) {
		t.Errorf("simple = %#v", got)
	}
	if got := m["inline"].(map[string]any); got["a"] != int64(1) || got["b"] != "two" {
		t.Errorf("inline = %#v", got)
	}
}

func TestUnmarshalIntoStruct(t *testing.T) {
	type backend struct {
		Name   string `json:"name"`
		Weight int    `json:"weight"`
	}
	type pool struct {
		Name     string    `json:"name"`
		Backends []backend `json:"backends"`
	}
	type doc struct {
		Env   string `json:"env"`
		Pools []pool `json:"pools"`
	}
	var d doc
	err := Unmarshal([]byte(`
env: staging
pools:
  - name: p1
    backends:
      - name: b1
        weight: 70
      - name: b2
        weight: 30
`), &d)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.Env != "staging" || len(d.Pools) != 1 || len(d.Pools[0].Backends) != 2 {
		t.Fatalf("decoded = %#v", d)
	}
	if d.Pools[0].Backends[0].Weight != 70 {
		t.Errorf("weight = %d", d.Pools[0].Backends[0].Weight)
	}
}

func TestBlockScalar(t *testing.T) {
	got, err := Parse([]byte("note: |\n  line one\n  line two\nfolded: >\n  a\n  b\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	m := got.(map[string]any)
	if m["note"] != "line one\nline two\n" {
		t.Errorf("note = %q", m["note"])
	}
	if m["folded"] != "a b\n" {
		t.Errorf("folded = %q", m["folded"])
	}
}

func TestRejectsUnsupportedConstructs(t *testing.T) {
	for name, src := range map[string]string{
		"tab indentation": "a:\n\tb: 1\n",
		"anchor":          "a: &anchor\n  b: 1\n",
	} {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: expected an error, got none", name)
		}
	}
}

func TestCommentInsideQuotesIsPreserved(t *testing.T) {
	got, err := Parse([]byte(`url: "http://host/path#fragment"`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v := got.(map[string]any)["url"]; v != "http://host/path#fragment" {
		t.Errorf("url = %q", v)
	}
}

func TestBareURLValueKeepsScheme(t *testing.T) {
	got, err := Parse([]byte("endpoint: http://otel-agent:4318\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v := got.(map[string]any)["endpoint"]; v != "http://otel-agent:4318" {
		t.Errorf("endpoint = %q", v)
	}
}
