package routing

import "strings"

// Canonicalize maps a raw model identifier (provider:model or full version
// string) to a stable canonical name and a provider tag. The canonical name
// collapses point releases under one identifier so historical outcomes can
// carry forward when a specific dated model gets deprecated. The provider
// tag lets analyses bucket across model-ID rotations and across providers
// (Anthropic vs OpenAI vs Google vs Bedrock vs local-weights).
//
// Inputs we expect to see in the wild:
//
//   - "anthropic:claude-3-5-sonnet-20241022"
//   - "claude-3-5-sonnet-20241022"
//   - "openai:gpt-5.4-mini"
//   - "google:gemini-2.5-pro"
//   - "bedrock:anthropic.claude-3-5-sonnet-20240620-v1:0"
//   - "ollama:llama-3.1-70b" (local weights)
//
// Unknown or empty input returns ("", "unknown") so callers can detect the
// case and decide whether to skip aggregation.
//
// This function does NOT validate that the model exists. It just normalizes
// the identifier. Adding a new provider/model is additive — append to the
// table at the bottom of the file.
func Canonicalize(modelID string) (canonical, provider string) {
	if modelID == "" {
		return "", "unknown"
	}

	id, prefixProvider := splitProviderPrefix(modelID)
	id = strings.ToLower(strings.TrimSpace(id))

	for _, rule := range canonicalRules {
		if rule.match(id) {
			p := rule.provider
			// Explicit prefix wins over rule-inferred provider for ambiguous
			// cases (e.g., bedrock-prefixed Anthropic models).
			if prefixProvider != "" {
				p = prefixProvider
			}
			return rule.canonical, p
		}
	}

	// Unknown model — pass through with whatever provider hint we got.
	if prefixProvider != "" {
		return id, prefixProvider
	}
	return id, "unknown"
}

// splitProviderPrefix extracts a "provider:" prefix if present and returns the
// remaining identifier plus the prefix as a provider tag. Bedrock identifiers
// (which use "anthropic.claude-…" with a dot) are recognized via the leading
// "bedrock:" or via the "<vendor>.<model>" shape inside a bedrock prefix.
func splitProviderPrefix(s string) (rest, provider string) {
	if i := strings.Index(s, ":"); i > 0 {
		prefix := strings.ToLower(s[:i])
		switch prefix {
		case "anthropic", "openai", "google", "google_genai", "bedrock", "ollama",
			"azure", "groq", "fireworks", "together", "vertex", "vertex_ai",
			"openrouter", "spark":
			return s[i+1:], normalizeProvider(prefix)
		}
	}
	return s, ""
}

func normalizeProvider(p string) string {
	switch p {
	case "vertex", "vertex_ai", "google_genai":
		return "google"
	case "azure":
		return "openai" // Azure-hosted but OpenAI model family
	default:
		return p
	}
}

type canonicalRule struct {
	// match is invoked on the lowercased, prefix-stripped model id.
	match func(string) bool
	// canonical is the stable identifier we want to record.
	canonical string
	// provider is the provider tag if no explicit prefix was supplied.
	provider string
}

func contains(needles ...string) func(string) bool {
	return func(s string) bool {
		for _, n := range needles {
			if strings.Contains(s, n) {
				return true
			}
		}
		return false
	}
}

func hasPrefix(prefixes ...string) func(string) bool {
	return func(s string) bool {
		for _, p := range prefixes {
			if strings.HasPrefix(s, p) {
				return true
			}
		}
		return false
	}
}

// canonicalRules is evaluated in order; first match wins. Order from most
// specific to most general. Anchor on the model family substring rather than
// dated identifiers so new point releases inherit the canonical name.
//
// Bedrock-style identifiers (`anthropic.<model>`) come first because the
// generic Anthropic `contains` rules below would otherwise consume them with
// the wrong provider tag.
var canonicalRules = []canonicalRule{
	// --- Bedrock-style anthropic identifiers (must precede generic Anthropic) ---
	{match: hasPrefix("anthropic.claude-3-5-sonnet"), canonical: "claude-sonnet-3-5", provider: "bedrock"},
	{match: hasPrefix("anthropic.claude-3-5-haiku"), canonical: "claude-haiku-3-5", provider: "bedrock"},
	{match: hasPrefix("anthropic.claude-3-opus"), canonical: "claude-opus-3", provider: "bedrock"},
	{match: hasPrefix("anthropic.claude-3-sonnet"), canonical: "claude-sonnet-3", provider: "bedrock"},
	{match: hasPrefix("anthropic.claude-3-haiku"), canonical: "claude-haiku-3", provider: "bedrock"},

	// --- Anthropic ---
	{match: contains("claude-haiku-4-5", "claude-4-5-haiku"), canonical: "claude-haiku-4-5", provider: "anthropic"},
	{match: contains("claude-sonnet-4-6", "claude-4-6-sonnet"), canonical: "claude-sonnet-4-6", provider: "anthropic"},
	{match: contains("claude-opus-4-7", "claude-4-7-opus"), canonical: "claude-opus-4-7", provider: "anthropic"},
	{match: contains("claude-opus-4", "claude-4-opus"), canonical: "claude-opus-4", provider: "anthropic"},
	{match: contains("claude-sonnet-4", "claude-4-sonnet"), canonical: "claude-sonnet-4", provider: "anthropic"},
	{match: contains("claude-haiku-4", "claude-4-haiku"), canonical: "claude-haiku-4", provider: "anthropic"},
	{match: contains("claude-3-5-sonnet", "claude-3.5-sonnet"), canonical: "claude-sonnet-3-5", provider: "anthropic"},
	{match: contains("claude-3-5-haiku", "claude-3.5-haiku"), canonical: "claude-haiku-3-5", provider: "anthropic"},
	{match: contains("claude-3-opus"), canonical: "claude-opus-3", provider: "anthropic"},
	{match: contains("claude-3-sonnet"), canonical: "claude-sonnet-3", provider: "anthropic"},
	{match: contains("claude-3-haiku"), canonical: "claude-haiku-3", provider: "anthropic"},
	// --- OpenAI ---
	{match: hasPrefix("gpt-5.4-nano"), canonical: "gpt-5.4-nano", provider: "openai"},
	{match: hasPrefix("gpt-5.4-mini"), canonical: "gpt-5.4-mini", provider: "openai"},
	{match: hasPrefix("gpt-5.4"), canonical: "gpt-5.4", provider: "openai"},
	{match: hasPrefix("gpt-5-mini"), canonical: "gpt-5-mini", provider: "openai"},
	{match: hasPrefix("gpt-5"), canonical: "gpt-5", provider: "openai"},
	{match: hasPrefix("gpt-4o-mini"), canonical: "gpt-4o-mini", provider: "openai"},
	{match: hasPrefix("gpt-4o"), canonical: "gpt-4o", provider: "openai"},
	{match: hasPrefix("o1-mini"), canonical: "o1-mini", provider: "openai"},
	{match: hasPrefix("o1-preview"), canonical: "o1-preview", provider: "openai"},
	{match: hasPrefix("o1"), canonical: "o1", provider: "openai"},
	{match: hasPrefix("gpt-4-turbo"), canonical: "gpt-4-turbo", provider: "openai"},
	{match: hasPrefix("gpt-4"), canonical: "gpt-4", provider: "openai"},
	{match: hasPrefix("gpt-3.5-turbo"), canonical: "gpt-3.5-turbo", provider: "openai"},

	// --- Google ---
	{match: hasPrefix("gemini-3.1-flash-lite"), canonical: "gemini-3.1-flash-lite", provider: "google"},
	{match: hasPrefix("gemini-3.1-flash"), canonical: "gemini-3.1-flash", provider: "google"},
	{match: hasPrefix("gemini-3.1-pro"), canonical: "gemini-3.1-pro", provider: "google"},
	{match: hasPrefix("gemini-2.5-pro"), canonical: "gemini-2.5-pro", provider: "google"},
	{match: hasPrefix("gemini-2.5-flash-lite"), canonical: "gemini-2.5-flash-lite", provider: "google"},
	{match: hasPrefix("gemini-2.5-flash"), canonical: "gemini-2.5-flash", provider: "google"},
	{match: hasPrefix("gemini-2.0-flash"), canonical: "gemini-2.0-flash", provider: "google"},
	{match: hasPrefix("gemini-1.5-pro"), canonical: "gemini-1.5-pro", provider: "google"},
	{match: hasPrefix("gemini-1.5-flash"), canonical: "gemini-1.5-flash", provider: "google"},

	// --- Local / open-weights ---
	{match: hasPrefix("llama-3.1-405b", "llama-3.1:405b"), canonical: "llama-3.1-405b", provider: "local"},
	{match: hasPrefix("llama-3.1-70b", "llama-3.1:70b"), canonical: "llama-3.1-70b", provider: "local"},
	{match: hasPrefix("llama-3.1-8b", "llama-3.1:8b"), canonical: "llama-3.1-8b", provider: "local"},
	{match: hasPrefix("qwen2.5-coder"), canonical: "qwen2.5-coder", provider: "local"},
	{match: hasPrefix("deepseek-coder"), canonical: "deepseek-coder", provider: "local"},
}
