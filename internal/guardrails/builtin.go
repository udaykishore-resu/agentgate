package guardrails

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Builtin is a dependency-free detector for the categories that can be found
// deterministically: structured personal data, credentials, and a small set of
// prompt-injection markers.
//
// It is not a replacement for a managed content-safety service, and it is not
// pretending to be one. It exists so the platform has a working default, so
// the local stack behaves like production, and so a pool can still enforce
// PII redaction when the external service is unavailable. Model-graded
// categories such as hate or self-harm are left to the callout provider.
type Builtin struct {
	patterns []pattern
}

type pattern struct {
	category string
	severity string
	re       *regexp.Regexp
	// validate is an optional second check that rejects false positives a
	// regular expression cannot, such as a card number that fails Luhn.
	validate func(string) bool
	// mask renders the redacted replacement.
	mask func(string) string
}

// NewBuiltin builds the detector.
func NewBuiltin() *Builtin {
	keep4 := func(s string) string {
		digits := onlyDigits(s)
		if len(digits) <= 4 {
			return "[REDACTED]"
		}
		return "[REDACTED:" + digits[len(digits)-4:] + "]"
	}
	return &Builtin{patterns: []pattern{
		{
			category: "pii", severity: "high",
			// Payment card numbers, validated with Luhn so that order numbers
			// and long identifiers do not trip it.
			re:       regexp.MustCompile(`\b(?:\d[ -]?){13,19}\b`),
			validate: luhn,
			mask:     keep4,
		},
		{
			category: "pii", severity: "high",
			// US social security numbers.
			re:   regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`),
			mask: func(string) string { return "[REDACTED:SSN]" },
		},
		{
			category: "pii", severity: "medium",
			// IBAN, which is what turns up in a payments context far more
			// often than a card number.
			re:   regexp.MustCompile(`\b[A-Z]{2}\d{2}[A-Z0-9]{11,30}\b`),
			mask: func(string) string { return "[REDACTED:IBAN]" },
		},
		{
			category: "pii", severity: "medium",
			re:   regexp.MustCompile(`\b[\w.+-]+@[\w-]+\.[\w.-]{2,}\b`),
			mask: func(string) string { return "[REDACTED:EMAIL]" },
		},
		{
			category: "secrets", severity: "critical",
			// Private key blocks, the single highest-value thing an agent can
			// accidentally paste into a prompt.
			re:   regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
			mask: func(string) string { return "[REDACTED:PRIVATE_KEY]" },
		},
		{
			category: "secrets", severity: "critical",
			re:   regexp.MustCompile(`\b(?:sk-[A-Za-z0-9]{20,}|ghp_[A-Za-z0-9]{30,}|AKIA[0-9A-Z]{16}|xox[baprs]-[A-Za-z0-9-]{10,})\b`),
			mask: func(string) string { return "[REDACTED:CREDENTIAL]" },
		},
		{
			category: "secrets", severity: "high",
			re:   regexp.MustCompile(`(?i)\b(?:password|passwd|api[_-]?key|secret|bearer)\s*[:=]\s*\S{6,}`),
			mask: func(string) string { return "[REDACTED:CREDENTIAL]" },
		},
		{
			category: "prompt_injection", severity: "medium",
			re:   regexp.MustCompile(`(?i)(ignore (all )?(previous|prior|above) instructions|disregard (the )?(system|previous) prompt|you are now (in )?developer mode|reveal your (system )?prompt)`),
			mask: func(s string) string { return s },
		},
	}}
}

// Name returns the provider name.
func (b *Builtin) Name() string { return "builtin" }

// Healthy always reports true; the detector is in-process.
func (b *Builtin) Healthy(context.Context) bool { return true }

// Inspect scans text and applies the pool's category policy.
func (b *Builtin) Inspect(_ context.Context, req Request, p Policy) (Decision, error) {
	start := time.Now()
	d := Decision{Action: ActionAllow, Provider: "builtin", Text: req.Text}
	text := req.Text
	worst := ActionAllow

	for _, pat := range b.patterns {
		cp, configured := p.Categories[pat.category]
		if !configured {
			continue
		}
		if len(req.Categories) > 0 && !contains(req.Categories, pat.category) {
			continue
		}
		matches := pat.re.FindAllStringIndex(text, -1)
		if len(matches) == 0 {
			continue
		}
		hit := false
		// Replace from the end so earlier offsets stay valid.
		for i := len(matches) - 1; i >= 0; i-- {
			m := matches[i]
			raw := text[m[0]:m[1]]
			if pat.validate != nil && !pat.validate(raw) {
				continue
			}
			hit = true
			d.Findings = append(d.Findings, Finding{
				Category: pat.category, Severity: pat.severity, Action: cp.Action,
				Excerpt: excerpt(raw), Offset: m[0], Confidence: 1,
			})
			if cp.Action == ActionRedact {
				text = text[:m[0]] + pat.mask(raw) + text[m[1]:]
			}
		}
		if hit {
			worst = escalate(worst, cp.Action)
		}
	}

	d.Text = text
	d.Action = worst
	d.Latency = time.Since(start)
	return d, nil
}

// escalate keeps the most restrictive action seen.
func escalate(current, next Action) Action {
	rank := map[Action]int{ActionAllow: 0, ActionAnnotate: 1, ActionRedact: 2, ActionBlock: 3}
	if rank[next] > rank[current] {
		return next
	}
	return current
}

// excerpt renders a non-revealing indication of what matched.
func excerpt(s string) string {
	if len(s) <= 4 {
		return fmt.Sprintf("%d chars", len(s))
	}
	return fmt.Sprintf("%s… (%d chars)", strings.Repeat("*", 4), len(s))
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func onlyDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// luhn validates a candidate card number.
func luhn(s string) bool {
	digits := onlyDigits(s)
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum, alt := 0, false
	for i := len(digits) - 1; i >= 0; i-- {
		n := int(digits[i] - '0')
		if alt {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
		alt = !alt
	}
	return sum%10 == 0
}
