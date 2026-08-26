package httpx

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// SSEWriter writes Server-Sent Events with the framing the frozen contract
// specifies: OpenAI-shaped `data:` chunks, AgentGate `event:` frames that a
// strict OpenAI client can ignore, a comment heartbeat so idle proxies do not
// drop long generations, and a terminating `data: [DONE]`.
type SSEWriter struct {
	mu        sync.Mutex
	w         http.ResponseWriter
	flusher   http.Flusher
	closed    bool
	heartbeat *time.Ticker
	done      chan struct{}
	wrote     bool
}

// NewSSEWriter prepares the response for streaming. It returns an error when
// the ResponseWriter cannot flush, which would otherwise produce a stream that
// buffers to completion and destroys time-to-first-token.
func NewSSEWriter(w http.ResponseWriter, heartbeat time.Duration) (*SSEWriter, error) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("streaming unsupported by this ResponseWriter")
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	// Defeats response buffering in nginx and several enterprise reverse
	// proxies, which otherwise hold the whole stream.
	h.Set("X-Accel-Buffering", "no")

	s := &SSEWriter{w: w, flusher: f, done: make(chan struct{})}
	if heartbeat > 0 {
		s.heartbeat = time.NewTicker(heartbeat)
		go s.beat()
	}
	return s, nil
}

func (s *SSEWriter) beat() {
	for {
		select {
		case <-s.done:
			return
		case <-s.heartbeat.C:
			s.mu.Lock()
			if !s.closed {
				_, _ = io.WriteString(s.w, ": heartbeat\n\n")
				s.flusher.Flush()
			}
			s.mu.Unlock()
		}
	}
}

// WriteHeaderOnce writes the 200 status before the first frame.
func (s *SSEWriter) WriteHeaderOnce() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.wrote {
		s.w.WriteHeader(http.StatusOK)
		s.flusher.Flush()
		s.wrote = true
	}
}

// Data writes an unnamed `data:` frame containing v as JSON.
func (s *SSEWriter) Data(v any) error { return s.write("", v) }

// Raw writes an unnamed `data:` frame with a pre-encoded payload, avoiding a
// re-marshal on the pass-through path.
func (s *SSEWriter) Raw(payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeLocked("", payload)
}

// Event writes a named `event:` frame. Named frames carry AgentGate-specific
// information and are ignorable by a strict OpenAI client.
func (s *SSEWriter) Event(name string, v any) error { return s.write(name, v) }

func (s *SSEWriter) write(event string, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeLocked(event, payload)
}

func (s *SSEWriter) writeLocked(event string, payload []byte) error {
	if s.closed {
		return io.ErrClosedPipe
	}
	if !s.wrote {
		s.w.WriteHeader(http.StatusOK)
		s.wrote = true
	}
	var b strings.Builder
	if event != "" {
		b.WriteString("event: ")
		b.WriteString(event)
		b.WriteByte('\n')
	}
	b.WriteString("data: ")
	b.Write(payload)
	b.WriteString("\n\n")
	if _, err := io.WriteString(s.w, b.String()); err != nil {
		s.closed = true
		return err
	}
	s.flusher.Flush()
	return nil
}

// Done writes the terminating sentinel and stops the heartbeat.
func (s *SSEWriter) Done() {
	s.mu.Lock()
	if !s.closed {
		_, _ = io.WriteString(s.w, "data: [DONE]\n\n")
		s.flusher.Flush()
	}
	s.mu.Unlock()
	s.Close()
}

// ErrorFrame reports a mid-stream failure. Once content has been delivered the
// status line is long gone, so the contract carries the failure in-band and
// the caller must treat a stream that ends without [DONE] as failed.
func (s *SSEWriter) ErrorFrame(p *Problem, requestID, traceID string) {
	if p == nil {
		return
	}
	p.RequestID, p.TraceID = requestID, traceID
	_ = s.Event("error", p)
}

// Close stops the heartbeat and marks the stream finished.
func (s *SSEWriter) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.heartbeat != nil {
		s.heartbeat.Stop()
	}
	close(s.done)
}

// SSEEvent is one decoded server-sent event.
type SSEEvent struct {
	Event string
	Data  string
}

// IsDone reports whether this is the terminating sentinel.
func (e SSEEvent) IsDone() bool { return strings.TrimSpace(e.Data) == "[DONE]" }

// ReadSSE decodes an SSE stream, invoking fn for each event. It stops on
// [DONE], on read error, or when fn returns a non-nil error. Comments, which
// carry the heartbeat, are skipped.
func ReadSSE(r io.Reader, fn func(SSEEvent) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var ev SSEEvent
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if ev.Data == "" && ev.Event == "" {
				continue
			}
			if err := fn(ev); err != nil {
				return err
			}
			if ev.IsDone() {
				return nil
			}
			ev = SSEEvent{}
		case strings.HasPrefix(line, ":"):
			// comment / heartbeat
		case strings.HasPrefix(line, "event:"):
			ev.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			chunk := strings.TrimPrefix(line, "data:")
			chunk = strings.TrimPrefix(chunk, " ")
			if ev.Data != "" {
				ev.Data += "\n"
			}
			ev.Data += chunk
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if ev.Data != "" || ev.Event != "" {
		return fn(ev)
	}
	return nil
}
