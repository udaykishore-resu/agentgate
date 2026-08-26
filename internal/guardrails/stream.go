package guardrails

import (
	"context"
	"strings"
)

// StreamScanner applies output guardrails to a streaming completion.
//
// Content is accumulated until the window is full, scanned, and only then
// released to the caller. That buys correctness — a violation is caught before
// the caller sees it — at the cost of delaying content by up to one window.
// Time to first token is unaffected because the first window is released as
// soon as it fills, and a scan of a few hundred tokens takes single-digit
// milliseconds against the built-in detector.
//
// The important property is that once a window has been released it cannot be
// recalled. A violation found later blocks the remainder of the stream and ends
// it with an error frame; it cannot un-send what the caller already has. That
// is an honest limitation of streaming, and it is why a pool handling
// restricted data can be configured to disable streaming entirely.
//
// Two coordinate spaces are tracked deliberately. `scannedTo` counts raw
// provider bytes, so the window size means what it says; `emitted` holds the
// exact text already released, which after a redaction is not a prefix of the
// raw buffer. Conflating the two silently deletes or duplicates content, which
// is worse than either blocking or allowing.
type StreamScanner struct {
	provider Provider
	policy   Policy
	req      Request

	buf       strings.Builder
	scannedTo int    // length of buf at the last scan, in raw provider bytes
	emitted   string // exactly what has been released to the caller
	window    int
	blocked   bool
	findings  []Finding
	failedOpn bool
}

// NewStreamScanner builds a scanner for one streaming response.
func NewStreamScanner(p Provider, policy Policy, req Request) *StreamScanner {
	window := policy.OutputWindowTokens
	if window <= 0 {
		window = 256
	}
	// The window is declared in tokens and applied in characters, at the same
	// four-characters-per-token ratio the estimator uses.
	return &StreamScanner{provider: p, policy: policy, req: req, window: window * 4}
}

// Push adds a delta and returns the text that may now be released to the
// caller. When ok is false the stream must be terminated: the decision carries
// the reason.
func (s *StreamScanner) Push(ctx context.Context, delta string) (release string, d Decision, ok bool) {
	if s.blocked {
		return "", Decision{Action: ActionBlock, Findings: s.findings}, false
	}
	if !s.policy.Enabled || s.provider == nil {
		s.emitted += delta
		return delta, Decision{Action: ActionAllow}, true
	}
	s.buf.WriteString(delta)
	if s.buf.Len()-s.scannedTo < s.window {
		return "", Decision{Action: ActionAllow}, true
	}
	return s.scan(ctx)
}

// Flush scans and releases whatever remains at the end of the stream.
func (s *StreamScanner) Flush(ctx context.Context) (release string, d Decision, ok bool) {
	if s.blocked {
		return "", Decision{Action: ActionBlock, Findings: s.findings}, false
	}
	if !s.policy.Enabled || s.provider == nil {
		return "", Decision{Action: ActionAllow}, true
	}
	if s.buf.Len() == s.scannedTo {
		return "", Decision{Action: ActionAllow, FailedOpen: s.failedOpn}, true
	}
	return s.scan(ctx)
}

func (s *StreamScanner) scan(ctx context.Context) (string, Decision, bool) {
	full := s.buf.String()

	req := s.req
	req.Direction = DirectionOutput
	// The whole completion so far is scanned, not just the pending window: a
	// violation can straddle a window boundary, and a detector shown only the
	// tail will miss it.
	req.Text = full

	raw, err := s.provider.Inspect(ctx, req, s.policy)
	d := Apply(s.policy, raw, err)
	if d.FailedOpen {
		s.failedOpn = true
	}
	s.findings = append(s.findings, d.Findings...)
	s.scannedTo = len(full)

	if d.Action == ActionBlock {
		s.blocked = true
		return "", d, false
	}

	out := full
	if d.Action == ActionRedact && d.Text != "" {
		out = d.Text
	}
	if !strings.HasPrefix(out, s.emitted) {
		// The scanner wants to change text that has already left the gateway.
		// It cannot be recalled, and continuing would leave the caller holding
		// a completion the platform does not consider safe, so the stream ends
		// here instead.
		s.blocked = true
		blocked := Decision{
			Action:     ActionBlock,
			FailedOpen: s.failedOpn,
			Findings: append(append([]Finding{}, d.Findings...), Finding{
				Category: "redaction_after_release", Severity: "high", Action: ActionBlock,
				Excerpt: "a redaction applied to content already sent to the caller",
			}),
		}
		s.findings = append(s.findings, blocked.Findings[len(blocked.Findings)-1])
		return "", blocked, false
	}

	pending := out[len(s.emitted):]
	s.emitted = out
	d.FailedOpen = s.failedOpn
	return pending, d, true
}

// Findings returns everything seen across the stream, for the span and the
// usage record.
func (s *StreamScanner) Findings() []Finding { return s.findings }

// FailedOpen reports whether any window was allowed through without a verdict.
func (s *StreamScanner) FailedOpen() bool { return s.failedOpn }

// Released returns the exact text handed to the caller so far, which is what a
// post-incident review needs rather than what the provider produced.
func (s *StreamScanner) Released() string { return s.emitted }
