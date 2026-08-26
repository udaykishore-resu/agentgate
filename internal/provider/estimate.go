package provider

import (
	"strings"
	"unicode"
)

// EstimateTokens approximates the token count of a piece of text without a
// model-specific tokenizer.
//
// Why an estimate rather than a real tokenizer: the gateway must reserve quota
// *before* it knows which backend will serve the request, and the backends in
// a single pool do not share a vocabulary. Shipping four tokenizers to be
// exact about a number that is then reconciled against the provider's own
// count seconds later is not a good trade. The estimate is deliberately
// conservative — it rounds up — so an agent is never admitted on a lowball
// guess, and the settle step corrects the reservation once the provider
// reports actual usage.
//
// The heuristic combines a character-per-token ratio with a word count, which
// tracks real tokenizers substantially better than characters alone for the
// mixed prose, code and JSON that agent prompts contain.
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	chars := 0
	words := 0
	inWord := false
	nonASCII := 0
	for _, r := range text {
		chars++
		if r > 127 {
			nonASCII++
		}
		if unicode.IsSpace(r) {
			inWord = false
			continue
		}
		if !inWord {
			words++
			inWord = true
		}
	}
	// English prose averages close to 4 characters per token; code and JSON
	// run denser, and non-Latin scripts denser still.
	perToken := 4.0
	if float64(nonASCII)/float64(chars) > 0.2 {
		perToken = 2.0
	}
	byChars := float64(chars) / perToken
	byWords := float64(words) * 1.3
	est := byChars
	if byWords > est {
		est = byWords
	}
	return int(est) + 1
}

// EstimateRequestTokens estimates the input tokens for a chat request,
// including the per-message overhead every chat format adds for role markers
// and separators, plus tool definitions, which are frequently the largest part
// of an agent's prompt and are just as frequently forgotten in an estimate.
func EstimateRequestTokens(req *ChatRequest) int {
	total := 0
	for _, m := range req.Messages {
		total += EstimateTokens(m.Text()) + 4
		for _, tc := range m.ToolCalls {
			total += EstimateTokens(tc.Function.Name+tc.Function.Arguments) + 8
		}
	}
	for _, t := range req.Tools {
		total += EstimateTokens(string(t.Function)) + 8
	}
	if len(req.ResponseFormat) > 0 {
		total += EstimateTokens(string(req.ResponseFormat))
	}
	return total + 3
}

// EstimateEmbeddingsTokens estimates input tokens for an embeddings request.
func EstimateEmbeddingsTokens(req *EmbeddingsRequest) int {
	total := 0
	for _, in := range req.Inputs() {
		total += EstimateTokens(in)
	}
	return total
}

// CountStreamTokens estimates output tokens from accumulated stream text, used
// only when a provider does not report usage on the final frame. A record
// built from this is marked as estimated so it is never mistaken for a billed
// figure in reconciliation.
func CountStreamTokens(parts []string) int {
	return EstimateTokens(strings.Join(parts, ""))
}
