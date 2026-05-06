package routing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCandidateLiteLLMKeys_Anthropic(t *testing.T) {
	cs := candidateLiteLLMKeys("anthropic:claude-sonnet-4-6")
	if len(cs) == 0 {
		t.Fatal("expected at least one candidate for anthropic key")
	}
	if cs[0].key != "claude-sonnet-4-6" {
		t.Errorf("first candidate = %q, want %q", cs[0].key, "claude-sonnet-4-6")
	}
	if _, ok := cs[0].providerWhitelist["anthropic"]; !ok {
		t.Errorf("anthropic candidate must whitelist provider=anthropic, got %v", cs[0].providerWhitelist)
	}
	// "anthropic.claude-sonnet-4-6" (the Bedrock variant from LiteLLM) must
	// NOT be among our candidates — that lookup would pull bedrock_converse
	// pricing, not direct Anthropic.
	for _, c := range cs {
		if strings.Contains(c.key, ".") {
			t.Errorf("candidate %q uses '.' separator (Bedrock-style); we only want direct Anthropic", c.key)
		}
	}
}

func TestCandidateLiteLLMKeys_OpenAI(t *testing.T) {
	cs := candidateLiteLLMKeys("openai:gpt-5.4-mini")
	if len(cs) == 0 || cs[0].key != "gpt-5.4-mini" {
		t.Fatalf("openai candidates = %+v", cs)
	}
	if _, ok := cs[0].providerWhitelist["openai"]; !ok {
		t.Error("openai candidate must whitelist provider=openai")
	}
}

func TestCandidateLiteLLMKeys_Gemini(t *testing.T) {
	cs := candidateLiteLLMKeys("google_genai:gemini-2.5-flash")
	if len(cs) == 0 || cs[0].key != "gemini/gemini-2.5-flash" {
		t.Fatalf("gemini first candidate should be 'gemini/<model>', got %+v", cs)
	}
}

func TestCandidateLiteLLMKeys_OpenRouter_StripsVariant(t *testing.T) {
	cs := candidateLiteLLMKeys("openrouter:deepseek/deepseek-v3.2:nitro")
	if len(cs) == 0 {
		t.Fatal("expected openrouter candidate")
	}
	want := "openrouter/deepseek/deepseek-v3.2"
	if cs[0].key != want {
		t.Errorf("openrouter candidate = %q, want %q (':nitro' must be stripped)", cs[0].key, want)
	}
}

func TestCandidateLiteLLMKeys_LocalProvidersSkipped(t *testing.T) {
	for _, k := range []string{
		"ollama:qwen2.5:3b",
		"ollama:gemma3:1b",
		"spark:bg-digitalservices/Gemma-4-26B-A4B-it-NVFP4",
	} {
		if cs := candidateLiteLLMKeys(k); cs != nil {
			t.Errorf("%s should produce no candidates (local/internal), got %v", k, cs)
		}
	}
}

func TestCandidateLiteLLMKeys_MalformedKey(t *testing.T) {
	if cs := candidateLiteLLMKeys("no-colon-here"); cs != nil {
		t.Errorf("malformed key without ':' should yield nil, got %v", cs)
	}
}

func TestResolveLiteLLM_HappyPath(t *testing.T) {
	manifest := map[string]liteLLMEntry{
		"claude-sonnet-4-6": {
			InputCostPerToken:           3e-06,
			OutputCostPerToken:          15e-06,
			CacheReadInputTokenCost:     3e-07,
			CacheCreationInputTokenCost: 3.75e-06,
			LiteLLMProvider:             "anthropic",
			Mode:                        "chat",
		},
	}
	key, e := resolveLiteLLM("anthropic:claude-sonnet-4-6", manifest)
	if e == nil {
		t.Fatalf("expected match, got nil. key=%q", key)
	}
	if key != "claude-sonnet-4-6" {
		t.Errorf("matched key = %q, want %q", key, "claude-sonnet-4-6")
	}
}

func TestResolveLiteLLM_RejectsWrongProvider(t *testing.T) {
	// LiteLLM's "anthropic.claude-sonnet-4-6" entry is Bedrock, not direct
	// Anthropic. Our anthropic:* lookup must NOT pick it up even if the
	// model name overlaps — wrong provider = wrong price tier.
	manifest := map[string]liteLLMEntry{
		"claude-sonnet-4-6": {
			InputCostPerToken:  4e-06,
			OutputCostPerToken: 20e-06,
			LiteLLMProvider:    "bedrock_converse",
			Mode:               "chat",
		},
	}
	_, e := resolveLiteLLM("anthropic:claude-sonnet-4-6", manifest)
	if e != nil {
		t.Errorf("expected rejection of bedrock_converse provider, got match")
	}
}

func TestResolveLiteLLM_RejectsNonChatMode(t *testing.T) {
	// Embedding models share names occasionally (e.g., gemini text-embedding).
	// We must skip them or the cost computation gets nonsense input rates.
	manifest := map[string]liteLLMEntry{
		"gemini/gemini-2.5-flash": {
			InputCostPerToken:  1e-08,
			OutputCostPerToken: 0,
			LiteLLMProvider:    "gemini",
			Mode:               "embedding",
		},
	}
	_, e := resolveLiteLLM("google_genai:gemini-2.5-flash", manifest)
	if e != nil {
		t.Errorf("expected embedding-mode entry to be rejected, got match")
	}
}

func TestResolveAll_SkipsUnmappableKeys(t *testing.T) {
	embedded := LoadEmbeddedPricing()
	if embedded.Count() == 0 {
		t.Fatal("embedded pricing is empty — fixture broken")
	}
	manifest := map[string]liteLLMEntry{
		"claude-sonnet-4-6": {
			InputCostPerToken:  2.5e-06, // pretend price drift: $2.50/M
			OutputCostPerToken: 12e-06,
			LiteLLMProvider:    "anthropic",
			Mode:               "chat",
		},
	}
	resolved := resolveAll(embedded, manifest)
	if len(resolved) != 1 {
		t.Fatalf("expected 1 resolution, got %d: %+v", len(resolved), resolved)
	}
	r := resolved[0]
	if r.OurKey != "anthropic:claude-sonnet-4-6" {
		t.Errorf("resolved.OurKey = %q, want %q", r.OurKey, "anthropic:claude-sonnet-4-6")
	}
	if r.InputPerMtok != 2.5 {
		t.Errorf("InputPerMtok = %v, want 2.5 (per-token * 1e6)", r.InputPerMtok)
	}
	// Critically: ollama:* and spark:* embedded keys should produce ZERO
	// resolutions — they're not in LiteLLM and we should skip them silently.
	for _, rr := range resolved {
		if strings.HasPrefix(rr.OurKey, "ollama:") || strings.HasPrefix(rr.OurKey, "spark:") {
			t.Errorf("local/internal model leaked into resolveAll output: %s", rr.OurKey)
		}
	}
}

func TestFetchLiteLLM_RoundTrip(t *testing.T) {
	body := `{
		"sample_spec": {"input_cost_per_token": 1.0},
		"claude-sonnet-4-6": {
			"input_cost_per_token": 3e-06,
			"output_cost_per_token": 15e-06,
			"cache_read_input_token_cost": 3e-07,
			"litellm_provider": "anthropic",
			"mode": "chat"
		}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	m, err := fetchLiteLLM(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("fetchLiteLLM: %v", err)
	}
	if _, ok := m["sample_spec"]; ok {
		t.Error("sample_spec should be filtered out")
	}
	e, ok := m["claude-sonnet-4-6"]
	if !ok {
		t.Fatal("claude-sonnet-4-6 missing from manifest")
	}
	if e.LiteLLMProvider != "anthropic" {
		t.Errorf("provider = %q, want anthropic", e.LiteLLMProvider)
	}
	if e.InputCostPerToken != 3e-06 {
		t.Errorf("input cost = %v, want 3e-06", e.InputCostPerToken)
	}
}

func TestFetchLiteLLM_HTTP500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	_, err := fetchLiteLLM(context.Background(), srv.Client(), srv.URL)
	if err == nil {
		t.Fatal("expected error on HTTP 500, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status code, got: %v", err)
	}
}
