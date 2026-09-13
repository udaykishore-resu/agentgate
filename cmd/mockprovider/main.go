// Command mockprovider simulates a model backend.
//
// It speaks both dialects the gateway adapts — the OpenAI chat-completions
// format and the Anthropic Messages format — and it can be told to be slow,
// to throttle, or to fail. That combination is what makes the resilience
// behaviour testable: retries, failover, breaker transitions and load shedding
// are all reproducible on a laptop, in CI, and in a load test, without
// spending a single real token.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"hash/crc32"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/agentgate/agentgate/internal/httpx"
	"github.com/agentgate/agentgate/internal/telemetry"
	"github.com/agentgate/agentgate/internal/version"
)

type config struct {
	flavor       string
	model        string
	latency      time.Duration
	jitter       time.Duration
	ttft         time.Duration
	interToken   time.Duration
	failureRate  float64
	throttleRate float64
	tokens       int
}

var requests atomic.Int64

func main() {
	cfg := config{}
	var (
		addr        = flag.String("addr", envOr("MOCK_ADDR", ":8090"), "HTTP listen address")
		logLevel    = flag.String("log-level", envOr("AGENTGATE_LOG_LEVEL", "info"), "log level")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.StringVar(&cfg.flavor, "flavor", envOr("MOCK_PROVIDER_FLAVOR", "openai"), "wire dialect: openai or anthropic")
	flag.StringVar(&cfg.model, "model", envOr("MOCK_MODEL", "mock-model-8b"), "model name to report")
	flag.DurationVar(&cfg.latency, "latency", durationOr("MOCK_LATENCY_MS", 120*time.Millisecond), "base response latency")
	flag.DurationVar(&cfg.jitter, "jitter", durationOr("MOCK_JITTER_MS", 60*time.Millisecond), "latency jitter")
	flag.DurationVar(&cfg.ttft, "ttft", durationOr("MOCK_TTFT_MS", 80*time.Millisecond), "streaming time to first token")
	flag.DurationVar(&cfg.interToken, "inter-token", durationOr("MOCK_INTERTOKEN_MS", 8*time.Millisecond), "streaming inter-token delay")
	flag.Float64Var(&cfg.failureRate, "failure-rate", floatOr("MOCK_FAILURE_RATE", 0), "fraction of requests to fail with 503")
	flag.Float64Var(&cfg.throttleRate, "throttle-rate", floatOr("MOCK_THROTTLE_RATE", 0), "fraction of requests to throttle with 429")
	flag.IntVar(&cfg.tokens, "tokens", intOr("MOCK_TOKENS", 48), "number of tokens to generate")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}
	// The flavour names accepted here match the backend kinds in the gateway
	// configuration, so a compose file or a load test can name the backend it
	// is simulating rather than the dialect that backend happens to speak.
	switch cfg.flavor {
	case "azure-openai", "vllm", "onprem-vllm", "openai":
		cfg.flavor = "openai"
	case "bedrock", "anthropic":
		cfg.flavor = "anthropic"
	default:
		fmt.Fprintf(os.Stderr, "unknown -flavor %q; expected one of openai, azure-openai, onprem-vllm, bedrock, anthropic\n", cfg.flavor)
		os.Exit(2)
	}

	logger := telemetry.NewLogger(os.Stdout, *logLevel, "agentgate-mockprovider", "dev")
	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"status": "ok", "flavor": cfg.flavor, "model": cfg.model,
			"requests_served": requests.Load(),
		})
	})
	// Both dialects advertise a models listing, which the gateway uses as a
	// cheap health probe.
	models := func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"object": "list",
			"data":   []map[string]any{{"id": cfg.model, "object": "model", "owned_by": "mockprovider"}},
		})
	}
	mux.HandleFunc("GET /v1/models", models)
	mux.HandleFunc("GET /openai/models", models)

	handler := func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if !admit(w, cfg, rnd) {
			return
		}
		body, _ := readJSON(r)
		if cfg.flavor == "anthropic" {
			serveAnthropic(w, cfg, rnd, body)
			return
		}
		serveOpenAI(w, cfg, rnd, body)
	}
	mux.HandleFunc("POST /v1/chat/completions", handler)
	mux.HandleFunc("POST /v1/messages", handler)
	// Azure OpenAI shape: /openai/deployments/{deployment}/chat/completions
	mux.HandleFunc("POST /openai/deployments/{deployment}/chat/completions", handler)
	// Bedrock runtime shape. The gateway's bedrock adapter never calls
	// /v1/messages: it carries the model in the URL, chooses streaming by
	// operation rather than a body field, and reads the streamed response in
	// the AWS event-stream binary framing, not SSE. Bedrock's Anthropic models
	// speak the Messages dialect on both operations, so this serves it
	// whatever -flavor was given.
	bedrock := func(stream bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			if !admit(w, cfg, rnd) {
				return
			}
			body, _ := readJSON(r)
			if stream {
				serveBedrockStream(w, cfg, rnd, body)
				return
			}
			serveAnthropic(w, cfg, rnd, body)
		}
	}
	mux.HandleFunc("POST /model/{model}/invoke", bedrock(false))
	mux.HandleFunc("POST /model/{model}/invoke-with-response-stream", bedrock(true))
	mux.HandleFunc("POST /v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if !admit(w, cfg, rnd) {
			return
		}
		body, _ := readJSON(r)
		serveEmbeddings(w, cfg, body)
	})

	logger.Info("mock provider starting", "addr", *addr, "flavor", cfg.flavor,
		"model", cfg.model, "latency", cfg.latency.String(),
		"failure_rate", cfg.failureRate, "throttle_rate", cfg.throttleRate)
	if err := httpx.Run(context.Background(), logger, 5*time.Second, nil,
		httpx.ServerSpec{Name: "mockprovider", Addr: *addr, Handler: mux}); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

// admit applies the injected failure behaviour and the base latency.
func admit(w http.ResponseWriter, cfg config, rnd *rand.Rand) bool {
	if cfg.throttleRate > 0 && rnd.Float64() < cfg.throttleRate {
		w.Header().Set("Retry-After", "1")
		http.Error(w, `{"error":{"message":"rate limit exceeded","type":"rate_limit_error"}}`, http.StatusTooManyRequests)
		return false
	}
	if cfg.failureRate > 0 && rnd.Float64() < cfg.failureRate {
		http.Error(w, `{"error":{"message":"service temporarily unavailable"}}`, http.StatusServiceUnavailable)
		return false
	}
	delay := cfg.latency
	if cfg.jitter > 0 {
		delay += time.Duration(rnd.Int63n(int64(cfg.jitter)))
	}
	time.Sleep(delay)
	return true
}

func readJSON(r *http.Request) (map[string]any, error) {
	var body map[string]any
	err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 16<<20)).Decode(&body)
	return body, err
}

func isStream(body map[string]any) bool {
	v, ok := body["stream"].(bool)
	return ok && v
}

// generate produces deterministic filler that is long enough to exercise
// streaming and chunking without pretending to be a model.
func generate(cfg config, body map[string]any) []string {
	n := cfg.tokens
	if v, ok := body["max_tokens"].(float64); ok && int(v) > 0 && int(v) < n {
		n = int(v)
	}
	words := []string{
		"The", "gateway", "routed", "this", "request", "through", "the", "policy",
		"chain", "and", "a", "backend", "answered", "it", "with", "deterministic",
		"filler", "so", "that", "latency", "streaming", "and", "token", "accounting",
		"can", "be", "measured", "without", "spending", "money", "on", "inference.",
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, " "+words[i%len(words)])
	}
	return out
}

func promptTokens(body map[string]any) int {
	raw, _ := json.Marshal(body["messages"])
	return len(raw)/4 + 3
}

func serveOpenAI(w http.ResponseWriter, cfg config, rnd *rand.Rand, body map[string]any) {
	id := "chatcmpl-" + strconv.FormatInt(rnd.Int63(), 36)
	parts := generate(cfg, body)
	in, out := promptTokens(body), len(parts)

	if !isStream(body) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"id": id, "object": "chat.completion", "created": time.Now().Unix(),
			"model": cfg.model,
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": strings.TrimSpace(strings.Join(parts, ""))},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{
				"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out,
			},
		})
		return
	}

	sse, err := httpx.NewSSEWriter(w, 0)
	if err != nil {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	defer sse.Close()
	time.Sleep(cfg.ttft)
	_ = sse.Data(map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": cfg.model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant"}}},
	})
	for _, p := range parts {
		time.Sleep(cfg.interToken)
		if err := sse.Data(map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": cfg.model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": p}}},
		}); err != nil {
			return
		}
	}
	_ = sse.Data(map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": cfg.model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
	})
	// Usage on the final frame, which is what a well-behaved provider does and
	// what the gateway needs to avoid estimating.
	_ = sse.Data(map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": cfg.model,
		"choices": []map[string]any{},
		"usage": map[string]any{
			"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out,
		},
	})
	sse.Done()
}

func serveAnthropic(w http.ResponseWriter, cfg config, rnd *rand.Rand, body map[string]any) {
	id := "msg_" + strconv.FormatInt(rnd.Int63(), 36)
	parts := generate(cfg, body)
	in, out := promptTokens(body), len(parts)

	if !isStream(body) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": cfg.model,
			"content":     []map[string]any{{"type": "text", "text": strings.TrimSpace(strings.Join(parts, ""))}},
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": in, "output_tokens": out},
		})
		return
	}

	sse, err := httpx.NewSSEWriter(w, 0)
	if err != nil {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	defer sse.Close()
	anthropicEvents(cfg, id, parts, in, out, sse.Event)
	sse.Done()
}

// anthropicEvents plays the Messages streaming event sequence through emit,
// which is what differs between the native API (SSE, event name on the frame)
// and Bedrock (event-stream binary frames, event type in a header).
func anthropicEvents(cfg config, id string, parts []string, in, out int, emit func(name string, v any) error) {
	time.Sleep(cfg.ttft)
	_ = emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": cfg.model,
			"usage": map[string]any{"input_tokens": in, "output_tokens": 0},
		},
	})
	_ = emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
	for _, p := range parts {
		time.Sleep(cfg.interToken)
		if err := emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": p},
		}); err != nil {
			return
		}
	}
	_ = emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	_ = emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn"},
		"usage": map[string]any{"output_tokens": out},
	})
	_ = emit("message_stop", map[string]any{"type": "message_stop"})
}

// serveBedrockStream answers InvokeModelWithResponseStream: the same Messages
// events, each JSON-encoded, base64-wrapped in a {"bytes": ...} envelope and
// framed as a `chunk` message in the AWS event-stream encoding.
func serveBedrockStream(w http.ResponseWriter, cfg config, rnd *rand.Rand, body map[string]any) {
	id := "msg_" + strconv.FormatInt(rnd.Int63(), 36)
	parts := generate(cfg, body)
	in, out := promptTokens(body), len(parts)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
	w.Header().Set("X-Amzn-Bedrock-Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	anthropicEvents(cfg, id, parts, in, out, func(_ string, v any) error {
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		envelope, _ := json.Marshal(map[string]any{"bytes": raw}) // []byte marshals as base64
		if _, err := w.Write(eventStreamFrame("chunk", envelope)); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	})
}

// eventStreamFrame encodes one AWS event-stream message carrying the
// :event-type, :content-type and :message-type headers Bedrock sends.
//
//	 0..3   total length      uint32 big-endian
//	 4..7   headers length    uint32 big-endian
//	 8..11  prelude CRC32     uint32 (IEEE, over bytes 0..7)
//	12..    headers           name-len(1) name value-type(1) value
//	 ...    payload
//	last 4  message CRC32     uint32 (IEEE, over everything before it)
func eventStreamFrame(eventType string, payload []byte) []byte {
	var headers bytes.Buffer
	for _, h := range [][2]string{
		{":event-type", eventType},
		{":content-type", "application/json"},
		{":message-type", "event"},
	} {
		headers.WriteByte(byte(len(h[0])))
		headers.WriteString(h[0])
		headers.WriteByte(7) // string
		_ = binary.Write(&headers, binary.BigEndian, uint16(len(h[1])))
		headers.WriteString(h[1])
	}
	total := 12 + headers.Len() + len(payload) + 4
	frame := make([]byte, 0, total)
	frame = binary.BigEndian.AppendUint32(frame, uint32(total))
	frame = binary.BigEndian.AppendUint32(frame, uint32(headers.Len()))
	frame = binary.BigEndian.AppendUint32(frame, crc32.ChecksumIEEE(frame))
	frame = append(frame, headers.Bytes()...)
	frame = append(frame, payload...)
	frame = binary.BigEndian.AppendUint32(frame, crc32.ChecksumIEEE(frame))
	return frame
}

func serveEmbeddings(w http.ResponseWriter, cfg config, body map[string]any) {
	inputs := 1
	switch v := body["input"].(type) {
	case []any:
		inputs = len(v)
	case string:
		inputs = 1
	}
	const dims = 64
	data := make([]map[string]any, 0, inputs)
	for i := 0; i < inputs; i++ {
		vec := make([]float64, dims)
		for j := range vec {
			// Deterministic per index so a semantic cache test is repeatable.
			vec[j] = float64((i*31+j*17)%100) / 100
		}
		data = append(data, map[string]any{"object": "embedding", "index": i, "embedding": vec})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"object": "list", "data": data, "model": cfg.model,
		"usage": map[string]any{"prompt_tokens": inputs * 8, "total_tokens": inputs * 8},
	})
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func durationOr(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if ms, err := strconv.Atoi(v); err == nil {
		return time.Duration(ms) * time.Millisecond
	}
	return def
}

func floatOr(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func intOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}
