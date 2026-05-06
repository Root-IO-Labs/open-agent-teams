package routing

import "testing"

func TestNormalizeModelID(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"anthropic:claude-sonnet-4-6", "anthropic:claude-sonnet-4-6"},
		{"  anthropic:claude-sonnet-4-6  ", "anthropic:claude-sonnet-4-6"},
		{"\"anthropic:claude-sonnet-4-6\"", "anthropic:claude-sonnet-4-6"},
		{"Anthropic:claude-sonnet-4-6", "anthropic:claude-sonnet-4-6"},
		{"OPENAI:gpt-5.4-mini", "openai:gpt-5.4-mini"},
		{"google_genai:gemini-2.5-flash", "google_genai:gemini-2.5-flash"},
		{"", ""},
		{"qwen2.5:3b", "qwen2.5:3b"}, // colon in version preserved; only provider lowercased
	}
	for _, tc := range cases {
		got := NormalizeModelID(tc.in)
		if got != tc.want {
			t.Errorf("NormalizeModelID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCacheCreationPriceFor_ExplicitYAMLValueWins(t *testing.T) {
	p := &ModelPricing{
		ModelID:              "anthropic:claude-sonnet-4-6",
		InputPerMtok:         3.0,
		CacheCreationPerMtok: 3.75,
		HasCacheCreation:     true,
	}
	got := CacheCreationPriceFor(p.ModelID, p)
	if got != 3.75 {
		t.Errorf("CacheCreationPriceFor explicit = %v, want 3.75", got)
	}
}

func TestCacheCreationPriceFor_ProviderFallbacks(t *testing.T) {
	cases := []struct {
		name       string
		modelID    string
		inputPrice float64
		want       float64
	}{
		{"anthropic 1.25x input", "anthropic:claude-sonnet-4-6", 3.0, 3.75},
		{"anthropic haiku", "anthropic:claude-haiku-4-5", 1.0, 1.25},
		{"openai zero", "openai:gpt-5.4-mini", 0.40, 0},
		{"openai zero (o4-mini)", "openai:o4-mini", 2.20, 0},
		{"google 1.0x input", "google_genai:gemini-2.5-flash", 0.15, 0.15},
		{"google bare prefix", "google:gemini-pro", 1.0, 1.0},
		{"openrouter conservative 1.0x", "openrouter:deepseek/deepseek-v3.2:nitro", 0.27, 0.27},
		{"deepseek conservative 1.0x", "deepseek:reasoner", 0.5, 0.5},
		{"ollama free", "ollama:gemma4", 0.0, 0.0},
		{"unknown conservative 1.0x", "spark:bg-digitalservices/Gemma-4-26B", 0.0, 0.0},
		{"capitalized provider tolerated", "Anthropic:claude-sonnet-4-6", 3.0, 3.75},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &ModelPricing{ModelID: tc.modelID, InputPerMtok: tc.inputPrice}
			got := CacheCreationPriceFor(tc.modelID, p)
			if got != tc.want {
				t.Errorf("CacheCreationPriceFor(%q, input=%v) = %v, want %v",
					tc.modelID, tc.inputPrice, got, tc.want)
			}
		})
	}
}

func TestCacheCreationPriceFor_NilPricing(t *testing.T) {
	got := CacheCreationPriceFor("anthropic:claude-sonnet-4-6", nil)
	if got != 0 {
		t.Errorf("CacheCreationPriceFor(nil) = %v, want 0 (no input price)", got)
	}
}

func TestEmbeddedPricing_HasAnthropicCacheCreation(t *testing.T) {
	r := LoadEmbeddedPricing()
	for _, id := range []string{"anthropic:claude-sonnet-4-6", "anthropic:claude-haiku-4-5"} {
		entry := r.Lookup(id)
		if entry == nil {
			t.Errorf("Lookup(%q) returned nil; expected entry in embedded pricing.yaml", id)
			continue
		}
		if !entry.HasCacheCreation {
			t.Errorf("%q: HasCacheCreation = false; expected explicit cache_creation_per_mtok in pricing.yaml", id)
		}
		if entry.CacheCreationPerMtok != entry.InputPerMtok*1.25 {
			t.Errorf("%q: CacheCreationPerMtok = %v, want 1.25 * InputPerMtok = %v",
				id, entry.CacheCreationPerMtok, entry.InputPerMtok*1.25)
		}
	}
}

func TestPricingRegistry_LookupNormalizes(t *testing.T) {
	r := LoadEmbeddedPricing()
	if e := r.Lookup("Anthropic:claude-sonnet-4-6"); e == nil {
		t.Error("Lookup with capitalized provider should normalize and hit the embedded entry")
	}
	if e := r.Lookup("  anthropic:claude-sonnet-4-6  "); e == nil {
		t.Error("Lookup with surrounding whitespace should normalize and hit the embedded entry")
	}
}
