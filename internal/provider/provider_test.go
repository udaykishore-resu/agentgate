package provider

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEstimateTokensIsConservative(t *testing.T) {
	if EstimateTokens("") != 0 {
		t.Error("empty text is zero tokens")
	}
	short := EstimateTokens("hello world")
	long := EstimateTokens(strings.Repeat("hello world ", 100))
	if long <= short {
		t.Error("longer text must estimate higher")
	}
	// The estimate must not be wildly below a real tokenizer, or quota
	// reservation admits requests it should not.
	text := strings.Repeat("the quick brown fox jumps over the lazy dog ", 10)
	if got := EstimateTokens(text); got < 80 {
		t.Errorf("estimate %d is implausibly low for %d characters", got, len(text))
	}
	if EstimateTokens("日本語のテキストです") <= 3 {
		t.Error("dense scripts must estimate higher per character")
	}
}

func TestEstimateRequestIncludesToolDefinitions(t *testing.T) {
	base := &ChatRequest{Messages: []Message{TextMessage("user", "hi")}}
	withTools := &ChatRequest{
		Messages: []Message{TextMessage("user", "hi")},
		Tools: []Tool{{Type: "function", Function: json.RawMessage(
			`{"name":"lookup_transaction","description":"Look up a transaction by id","parameters":{"type":"object","properties":{"id":{"type":"string"}}}}`)}},
	}
	if EstimateRequestTokens(withTools) <= EstimateRequestTokens(base) {
		t.Error("tool definitions are part of the prompt and must be estimated")
	}
}

func TestClassifyMapsProviderErrors(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrorKind
	}{
		{429, "", ErrThrottled},
		{503, "", ErrUnavailable},
		{500, "", ErrUnavailable},
		{504, "", ErrTimeout},
		{401, "", ErrAuth},
		{404, "", ErrNotFound},
		{400, "This model's maximum context length is 8192 tokens", ErrContextLength},
		{400, "The response was filtered due to the prompt triggering content filter", ErrContentFilter},
		{400, "unknown parameter", ErrBadRequest},
	}
	for _, tc := range cases {
		if got := Classify(tc.status, tc.body); got != tc.want {
			t.Errorf("Classify(%d, %q) = %s, want %s", tc.status, tc.body, got, tc.want)
		}
	}
}

func TestRetryableClassification(t *testing.T) {
	for kind, want := range map[ErrorKind]bool{
		ErrThrottled: true, ErrUnavailable: true, ErrTimeout: true, ErrTransport: true,
		ErrBadRequest: false, ErrContextLength: false, ErrContentFilter: false, ErrAuth: false,
	} {
		if got := (&Error{Kind: kind}).Retryable(); got != want {
			t.Errorf("%s retryable = %v, want %v", kind, got, want)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "3")
	if got := ParseRetryAfter(h); got != 3*time.Second {
		t.Errorf("got %s", got)
	}
	h.Set("Retry-After", "Wed, 21 Oct 2026 07:28:00 GMT")
	if got := ParseRetryAfter(h); got != 0 {
		t.Errorf("an HTTP-date must be treated as absent rather than mis-parsed, got %s", got)
	}
	h.Set("Retry-After", "100000")
	if got := ParseRetryAfter(h); got != 0 {
		t.Errorf("an absurd value must be ignored, got %s", got)
	}
}

// --- OpenAI-compatible adapter -------------------------------------------

func TestOpenAICompatibleChat(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "cmpl-1", "model": "backend-model",
			"choices": []map[string]any{{
				"index": 0, "message": map[string]any{"role": "assistant", "content": "hi"},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 7, "completion_tokens": 2, "total_tokens": 9},
		})
	}))
	defer srv.Close()

	p, err := NewOpenAICompatible(Options{
		Name: "test/backend", Kind: KindOpenAI, BaseURL: srv.URL + "/v1",
		Model: "backend-model", APIKey: "sk-test", Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.Chat(context.Background(), &ChatRequest{
		Model: "logical-model", Messages: []Message{TextMessage("user", "hello")},
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %s", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("authorization = %q", gotAuth)
	}
	if gotBody["model"] != "backend-model" {
		t.Errorf("the concrete backend model must be sent, got %v", gotBody["model"])
	}
	if resp.FirstText() != "hi" || resp.Usage.TotalTokens != 9 {
		t.Errorf("response = %#v", resp)
	}
}

func TestOpenAICompatibleErrorMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit"}}`))
	}))
	defer srv.Close()

	p, _ := NewOpenAICompatible(Options{
		Name: "test/backend", Kind: KindOpenAI, BaseURL: srv.URL + "/v1", Model: "m", Timeout: time.Second,
	})
	_, err := p.Chat(context.Background(), &ChatRequest{Messages: []Message{TextMessage("user", "x")}})
	pe, ok := AsError(err)
	if !ok {
		t.Fatalf("expected a provider error, got %v", err)
	}
	if pe.Kind != ErrThrottled || !pe.Retryable() {
		t.Errorf("kind = %s", pe.Kind)
	}
	if pe.RetryAfter != 2*time.Second {
		t.Errorf("retry after = %s", pe.RetryAfter)
	}
	if pe.Backend != "test/backend" {
		t.Errorf("the failing backend must be named, got %q", pe.Backend)
	}
}

func TestAzureURLShape(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "1", "choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "ok"}}},
		})
	}))
	defer srv.Close()

	p, _ := NewOpenAICompatible(Options{
		Name: "azure/gpt", Kind: KindAzureOpenAI, BaseURL: srv.URL,
		Model: "gpt-4o-mini", Deployment: "my-deployment", APIVersion: "2024-10-21",
		APIKey: "k", Timeout: time.Second,
	})
	if _, err := p.Chat(context.Background(), &ChatRequest{Messages: []Message{TextMessage("user", "x")}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotURL, "/openai/deployments/my-deployment/chat/completions") ||
		!strings.Contains(gotURL, "api-version=2024-10-21") {
		t.Errorf("azure URL = %s", gotURL)
	}
}

func TestOpenAIStreamDecoding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		frames := []string{
			`{"id":"1","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
			`{"id":"1","choices":[{"index":0,"delta":{"content":"he"}}]}`,
			`{"id":"1","choices":[{"index":0,"delta":{"content":"llo"}}]}`,
			`{"id":"1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`{"id":"1","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
		}
		for _, f := range frames {
			_, _ = w.Write([]byte("data: " + f + "\n\n"))
			flusher.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer srv.Close()

	p, _ := NewOpenAICompatible(Options{
		Name: "test", Kind: KindOpenAI, BaseURL: srv.URL + "/v1", Model: "m", Timeout: 5 * time.Second,
	})
	stream, err := p.ChatStream(context.Background(), &ChatRequest{
		Messages: []Message{TextMessage("user", "x")}, Stream: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()

	var text strings.Builder
	for {
		chunk, ok, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		text.WriteString(chunk.DeltaText())
	}
	if text.String() != "hello" {
		t.Errorf("streamed text = %q", text.String())
	}
	if u := stream.Usage(); u == nil || u.TotalTokens != 7 {
		t.Errorf("usage = %#v", stream.Usage())
	}
}

// --- Anthropic dialect ----------------------------------------------------

func TestAnthropicRequestTranslation(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_1", "model": "claude", "stop_reason": "end_turn",
			"content": []map[string]any{{"type": "text", "text": "answer"}},
			"usage":   map[string]any{"input_tokens": 11, "output_tokens": 3},
		})
	}))
	defer srv.Close()

	p, err := NewAnthropic(AnthropicOptions{Options: Options{
		Name: "anthropic/claude", Kind: KindAnthropic, BaseURL: srv.URL,
		Model: "claude", APIKey: "k", Timeout: 5 * time.Second,
	}})
	if err != nil {
		t.Fatal(err)
	}
	max := 256
	resp, err := p.Chat(context.Background(), &ChatRequest{
		Messages: []Message{
			TextMessage("system", "You are careful."),
			TextMessage("user", "What happened?"),
		},
		MaxTokens: &max,
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}

	// The system prompt becomes a top-level field, not a message.
	if _, ok := body["system"]; !ok {
		t.Error("the system prompt must be lifted out of the message list")
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Errorf("expected only the user turn in messages, got %d", len(msgs))
	}
	if body["max_tokens"] != float64(256) {
		t.Errorf("max_tokens = %v; the Anthropic dialect requires it", body["max_tokens"])
	}
	// The response comes back in the canonical shape.
	if resp.Object != "chat.completion" || resp.FirstText() != "answer" {
		t.Errorf("response = %#v", resp)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish reason = %q; end_turn must be normalised to stop", resp.Choices[0].FinishReason)
	}
	if resp.Usage.PromptTokens != 11 || resp.Usage.CompletionTokens != 3 {
		t.Errorf("usage = %#v", resp.Usage)
	}
}

func TestAnthropicStreamTranslation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		write := func(event, data string) {
			_, _ = w.Write([]byte("event: " + event + "\ndata: " + data + "\n\n"))
			flusher.Flush()
		}
		write("message_start", `{"type":"message_start","message":{"id":"m","usage":{"input_tokens":9,"output_tokens":0}}}`)
		write("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"par"}}`)
		write("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"tial"}}`)
		write("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`)
		write("message_stop", `{"type":"message_stop"}`)
	}))
	defer srv.Close()

	p, _ := NewAnthropic(AnthropicOptions{Options: Options{
		Name: "anthropic/claude", Kind: KindAnthropic, BaseURL: srv.URL,
		Model: "claude", APIKey: "k", Timeout: 5 * time.Second,
	}})
	stream, err := p.ChatStream(context.Background(), &ChatRequest{
		Messages: []Message{TextMessage("user", "x")}, Stream: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()

	var text strings.Builder
	var finish string
	for {
		chunk, ok, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		text.WriteString(chunk.DeltaText())
		if len(chunk.Choices) > 0 && chunk.Choices[0].FinishReason != "" {
			finish = chunk.Choices[0].FinishReason
		}
	}
	if text.String() != "partial" {
		t.Errorf("streamed text = %q", text.String())
	}
	if finish != "stop" {
		t.Errorf("finish reason = %q", finish)
	}
	if u := stream.Usage(); u == nil || u.PromptTokens != 9 || u.CompletionTokens != 2 {
		t.Errorf("usage = %#v", stream.Usage())
	}
}

// --- SigV4 ---------------------------------------------------------------

func TestSigV4IsDeterministicAndBodyBound(t *testing.T) {
	s := &SigV4Signer{
		Region: "eu-west-1", Service: "bedrock",
		AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
	}
	at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	sign := func(body string) *http.Request {
		req, _ := http.NewRequest(http.MethodPost,
			"https://bedrock-runtime.eu-west-1.amazonaws.com/model/m/invoke", bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		if err := s.Sign(req, []byte(body), at); err != nil {
			t.Fatal(err)
		}
		return req
	}
	a, b := sign(`{"x":1}`), sign(`{"x":1}`)
	if a.Header.Get("Authorization") == "" {
		t.Fatal("no Authorization header was produced")
	}
	if a.Header.Get("Authorization") != b.Header.Get("Authorization") {
		t.Error("signing the same request twice must produce the same signature")
	}
	if a.Header.Get("X-Amz-Date") != "20260826T120000Z" {
		t.Errorf("X-Amz-Date = %q", a.Header.Get("X-Amz-Date"))
	}
	if a.Header.Get("X-Amz-Content-Sha256") == "" {
		t.Error("the payload hash header must be present")
	}
	if c := sign(`{"x":2}`); c.Header.Get("Authorization") == a.Header.Get("Authorization") {
		t.Error("a different body must produce a different signature")
	}
	auth := a.Header.Get("Authorization")
	for _, want := range []string{"AWS4-HMAC-SHA256", "Credential=AKIDEXAMPLE/20260826/eu-west-1/bedrock/aws4_request", "SignedHeaders=", "Signature="} {
		if !strings.Contains(auth, want) {
			t.Errorf("Authorization %q is missing %q", auth, want)
		}
	}
}

func TestSigV4RequiresCredentials(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/", nil)
	if err := (&SigV4Signer{Region: "r", Service: "s"}).Sign(req, nil, time.Now()); err == nil {
		t.Error("signing without credentials must fail loudly rather than send an unsigned request")
	}
}

func TestCanonicalQuerySorts(t *testing.T) {
	if got := canonicalQuery("b=2&a=1"); got != "a=1&b=2" {
		t.Errorf("canonicalQuery = %q", got)
	}
	if got := canonicalQuery(""); got != "" {
		t.Errorf("empty query = %q", got)
	}
}

// --- AWS event stream ----------------------------------------------------

// buildEventStreamFrame constructs a frame in the AWS event-stream encoding.
func buildEventStreamFrame(t *testing.T, eventType string, payload []byte) []byte {
	t.Helper()
	var headers bytes.Buffer
	name := ":event-type"
	headers.WriteByte(byte(len(name)))
	headers.WriteString(name)
	headers.WriteByte(7) // string
	_ = binary.Write(&headers, binary.BigEndian, uint16(len(eventType)))
	headers.WriteString(eventType)

	total := 12 + headers.Len() + len(payload) + 4
	var frame bytes.Buffer
	_ = binary.Write(&frame, binary.BigEndian, uint32(total))
	_ = binary.Write(&frame, binary.BigEndian, uint32(headers.Len()))
	_ = binary.Write(&frame, binary.BigEndian, uint32(0)) // prelude CRC, not verified
	frame.Write(headers.Bytes())
	frame.Write(payload)
	_ = binary.Write(&frame, binary.BigEndian, uint32(0)) // message CRC, not verified
	return frame.Bytes()
}

func TestEventStreamDecoder(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(buildEventStreamFrame(t, "chunk", []byte(`{"bytes":"eyJhIjoxfQ=="}`)))
	buf.Write(buildEventStreamFrame(t, "chunk", []byte(`{"b":2}`)))

	d := NewEventStreamDecoder(&buf)
	first, err := d.Next()
	if err != nil {
		t.Fatal(err)
	}
	if first.Headers[":event-type"] != "chunk" {
		t.Errorf("headers = %#v", first.Headers)
	}
	if string(first.Payload) != `{"bytes":"eyJhIjoxfQ=="}` {
		t.Errorf("payload = %s", first.Payload)
	}
	if _, err := d.Next(); err != nil {
		t.Fatalf("second frame: %v", err)
	}
	if _, err := d.Next(); err == nil {
		t.Error("the decoder must report the end of the stream")
	}
}

func TestEventStreamRejectsImplausibleLengths(t *testing.T) {
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.BigEndian, uint32(4)) // total shorter than the prelude
	_ = binary.Write(&buf, binary.BigEndian, uint32(0))
	_ = binary.Write(&buf, binary.BigEndian, uint32(0))
	if _, err := NewEventStreamDecoder(&buf).Next(); err == nil {
		t.Error("a malformed frame must be rejected rather than turned into an allocation")
	}
}

func TestMessageTextHandlesContentParts(t *testing.T) {
	m := Message{Role: "user", Content: json.RawMessage(
		`[{"type":"text","text":"first"},{"type":"text","text":"second"}]`)}
	got := m.Text()
	if !strings.Contains(got, "first") || !strings.Contains(got, "second") {
		t.Errorf("multi-part content flattened to %q", got)
	}
}
