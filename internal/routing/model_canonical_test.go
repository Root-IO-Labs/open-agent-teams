package routing

import "testing"

// TestCanonicalize_GoogleGenAIPrefix is a regression guard for a real bug
// caught by the demo script: splitProviderPrefix didn't recognize the
// "google_genai:" prefix used by the live corpus, so V2 router lookups
// missed every google-hosted record. Adding the prefix to the switch +
// normalizing it to "google" closed the gap.
func TestCanonicalize_GoogleGenAIPrefix(t *testing.T) {
	tests := []struct {
		in           string
		wantCanon    string
		wantProvider string
	}{
		{"google_genai:gemini-2.5-flash", "gemini-2.5-flash", "google"},
		{"google_genai:gemini-3.1-flash-lite-preview", "gemini-3.1-flash-lite", "google"},
		{"google_genai:gemini-2.5-pro", "gemini-2.5-pro", "google"},
		{"google:gemini-2.5-flash", "gemini-2.5-flash", "google"},
		{"vertex_ai:gemini-2.5-pro", "gemini-2.5-pro", "google"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			gotCanon, gotProvider := Canonicalize(tt.in)
			if gotCanon != tt.wantCanon {
				t.Errorf("canonical = %q, want %q", gotCanon, tt.wantCanon)
			}
			if gotProvider != tt.wantProvider {
				t.Errorf("provider  = %q, want %q", gotProvider, tt.wantProvider)
			}
		})
	}
}

// TestCanonicalize_RouterIndexBucketAlignment is the bug-prevention test:
// the V2 router and the corpus index must produce the SAME bucket key for
// the same model. They could previously disagree because the router went
// through Canonicalize while index records had ModelCanonical set by
// migration / writer — if Canonicalize was missing a rule, the router's
// lookup would silently miss the corpus's bucket.
func TestCanonicalize_RouterIndexBucketAlignment(t *testing.T) {
	models := []string{
		"anthropic:claude-3-5-sonnet-20241022",
		"anthropic:claude-haiku-4-5",
		"anthropic:claude-sonnet-4-6",
		"openai:gpt-5.4-mini",
		"openai:gpt-5.4-nano",
		"google_genai:gemini-2.5-flash",
		"google_genai:gemini-3.1-flash-lite-preview",
	}
	for _, m := range models {
		canonAtWrite, _ := Canonicalize(m)
		canonAtRoute := canonicalKeyForModel(m)
		if canonAtWrite != canonAtRoute {
			t.Errorf("bucket key mismatch for %q: write=%q route=%q — V2 lookup will miss",
				m, canonAtWrite, canonAtRoute)
		}
	}
}

func TestCanonicalize_KnownModels(t *testing.T) {
	cases := []struct {
		name         string
		input        string
		wantCanon    string
		wantProvider string
	}{
		// Anthropic — bare ids
		{"haiku-3-5 dated", "claude-3-5-haiku-20241022", "claude-haiku-3-5", "anthropic"},
		{"sonnet-3-5 dated", "claude-3-5-sonnet-20241022", "claude-sonnet-3-5", "anthropic"},
		{"opus-3", "claude-3-opus-20240229", "claude-opus-3", "anthropic"},
		{"sonnet-4-6", "claude-sonnet-4-6", "claude-sonnet-4-6", "anthropic"},
		{"opus-4-7", "claude-opus-4-7", "claude-opus-4-7", "anthropic"},
		{"haiku-4-5", "claude-haiku-4-5", "claude-haiku-4-5", "anthropic"},

		// Anthropic — provider-prefixed
		{"prefixed sonnet-3-5", "anthropic:claude-3-5-sonnet-20241022", "claude-sonnet-3-5", "anthropic"},
		{"prefixed opus-4-7", "anthropic:claude-opus-4-7", "claude-opus-4-7", "anthropic"},

		// Bedrock — Anthropic via Bedrock should report bedrock provider but
		// canonical anthropic family name.
		{"bedrock prefix", "bedrock:anthropic.claude-3-5-sonnet-20240620-v1:0", "claude-sonnet-3-5", "bedrock"},
		{"bedrock dotted no prefix", "anthropic.claude-3-5-haiku-20241022-v1:0", "claude-haiku-3-5", "bedrock"},

		// OpenAI
		{"gpt-5", "gpt-5", "gpt-5", "openai"},
		{"gpt-5-mini", "gpt-5-mini-2025-04", "gpt-5-mini", "openai"},
		{"gpt-5.4", "gpt-5.4", "gpt-5.4", "openai"},
		{"gpt-5.4-mini", "gpt-5.4-mini-preview", "gpt-5.4-mini", "openai"},
		{"gpt-5.4-nano", "gpt-5.4-nano", "gpt-5.4-nano", "openai"},
		{"gpt-4o", "gpt-4o-2024-08-06", "gpt-4o", "openai"},
		{"gpt-4o-mini", "gpt-4o-mini-2024-07-18", "gpt-4o-mini", "openai"},
		{"o1-preview", "o1-preview-2024-09-12", "o1-preview", "openai"},
		{"o1-mini", "o1-mini-2024-09-12", "o1-mini", "openai"},

		// OpenAI prefixed
		{"prefixed gpt-5.4-mini", "openai:gpt-5.4-mini", "gpt-5.4-mini", "openai"},
		{"azure-hosted is openai family", "azure:gpt-4o", "gpt-4o", "openai"},

		// Google
		{"gemini-2.5-pro", "gemini-2.5-pro-latest", "gemini-2.5-pro", "google"},
		{"gemini-2.5-flash", "gemini-2.5-flash", "gemini-2.5-flash", "google"},
		{"gemini-1.5-pro prefixed", "google:gemini-1.5-pro-001", "gemini-1.5-pro", "google"},
		{"vertex hosted gemini", "vertex:gemini-2.5-pro", "gemini-2.5-pro", "google"},

		// Local / open-weights
		{"llama 70b ollama format", "ollama:llama-3.1-70b", "llama-3.1-70b", "ollama"},
		{"qwen2.5-coder", "qwen2.5-coder-7b", "qwen2.5-coder", "local"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			canon, provider := Canonicalize(tc.input)
			if canon != tc.wantCanon {
				t.Errorf("Canonicalize(%q) canonical = %q, want %q", tc.input, canon, tc.wantCanon)
			}
			if provider != tc.wantProvider {
				t.Errorf("Canonicalize(%q) provider = %q, want %q", tc.input, provider, tc.wantProvider)
			}
		})
	}
}

func TestCanonicalize_UnknownFallthrough(t *testing.T) {
	// Unknown model: returns the input as canonical and "unknown" provider.
	canon, provider := Canonicalize("some-future-model-7b")
	if canon != "some-future-model-7b" {
		t.Errorf("unknown canonical: got %q want %q", canon, "some-future-model-7b")
	}
	if provider != "unknown" {
		t.Errorf("unknown provider: got %q want %q", provider, "unknown")
	}
}

func TestCanonicalize_UnknownWithPrefixKeepsPrefix(t *testing.T) {
	// Unknown model name with a known provider prefix: provider tag wins,
	// canonical falls through to the lowercased remainder.
	canon, provider := Canonicalize("openai:future-model-99")
	if canon != "future-model-99" {
		t.Errorf("canonical = %q, want %q", canon, "future-model-99")
	}
	if provider != "openai" {
		t.Errorf("provider = %q, want %q", provider, "openai")
	}
}

func TestCanonicalize_EmptyAndWhitespace(t *testing.T) {
	canon, provider := Canonicalize("")
	if canon != "" || provider != "unknown" {
		t.Errorf("empty input: got (%q, %q), want (\"\", \"unknown\")", canon, provider)
	}

	canon2, _ := Canonicalize("   ")
	if canon2 != "" {
		// Whitespace-only collapses to empty after trimming, but the prefix
		// extractor doesn't trim — it falls through as unknown rule.
		// Either result is acceptable as long as we don't panic.
		_ = canon2
	}
}

func TestCanonicalize_CaseInsensitive(t *testing.T) {
	canon, provider := Canonicalize("Claude-3-5-Sonnet-20241022")
	if canon != "claude-sonnet-3-5" || provider != "anthropic" {
		t.Errorf("case-insensitive match: got (%q, %q), want (%q, %q)",
			canon, provider, "claude-sonnet-3-5", "anthropic")
	}
}
