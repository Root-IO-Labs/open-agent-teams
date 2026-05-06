package routing

import (
	"os"
	"testing"
)

func TestEmbeddedContextRegistry_LoadsModels(t *testing.T) {
	r := LoadEmbeddedContextRegistry()
	if r == nil {
		t.Fatal("embedded registry should load")
	}
	if r.Count() == 0 {
		t.Fatal("embedded registry should have entries")
	}

	// Spot-check a few known entries.
	sonnet := r.Lookup("anthropic:claude-sonnet-4-6")
	if sonnet == nil {
		t.Fatal("sonnet-4-6 missing from registry")
	}
	if sonnet.MaxInputTokens != 1_000_000 {
		t.Errorf("sonnet max_input_tokens want 1M, got %d", sonnet.MaxInputTokens)
	}
	nano := r.Lookup("openai:gpt-5.4-nano")
	if nano == nil || nano.MaxInputTokens == 0 {
		t.Fatal("gpt-5.4-nano missing or unset")
	}
	flashLite := r.Lookup("google_genai:gemini-3.1-flash-lite-preview")
	if flashLite == nil || flashLite.MaxInputTokens == 0 {
		t.Fatal("flash-lite missing or unset")
	}
}

func TestApplyToProfile_FillsUnknownWindow(t *testing.T) {
	r := LoadEmbeddedContextRegistry()
	p := &ModelProfile{
		ModelID:               "openai:gpt-5.4-nano",
		EffectiveContextClass: "unknown",
		MaxInputTokens:        0,
	}
	modified := r.ApplyToProfile(p)
	if !modified {
		t.Fatal("should have modified profile with unknown window")
	}
	if p.MaxInputTokens == 0 {
		t.Error("MaxInputTokens should be filled from registry")
	}
	if p.EffectiveContextClass == "unknown" {
		t.Error("EffectiveContextClass should be derived from registry window")
	}
}

func TestApplyToProfile_PreservesKnownWindow(t *testing.T) {
	r := LoadEmbeddedContextRegistry()
	p := &ModelProfile{
		ModelID:               "anthropic:claude-sonnet-4-6",
		EffectiveContextClass: "large",
		MaxInputTokens:        500_000, // intentionally less than registry's 1M
	}
	modified := r.ApplyToProfile(p)
	if modified {
		t.Error("should NOT modify profile when window already populated")
	}
	if p.MaxInputTokens != 500_000 {
		t.Errorf("existing MaxInputTokens should be preserved, got %d", p.MaxInputTokens)
	}
}

func TestApplyToProfile_NilSafe(t *testing.T) {
	var r *ContextRegistry
	if r.ApplyToProfile(&ModelProfile{ModelID: "x"}) {
		t.Error("nil registry should return false")
	}
	r = &ContextRegistry{entries: map[string]*ContextRegistryEntry{}}
	if r.ApplyToProfile(nil) {
		t.Error("nil profile should return false")
	}
}

func TestProfileStore_AppliesRegistryOnLoad(t *testing.T) {
	dir := t.TempDir()
	// Write a profile whose max_input_tokens is "unknown" (parses to 0).
	// Registry should fill it on load.
	yamlContent := `model_id: "openai:gpt-5.4-nano"
status: known
provider:
  name: openai
capabilities:
  tool_reliability: 1.0
  shell_roundtrip: 0.8
  shell_recovery: 0.55
  file_write_reliability: 1.0
  token_reporting: 1.0
  streaming: 1.0
  multi_turn: 1.0
  large_output: 0.9
  effective_context_class: unknown
  max_input_tokens: unknown
  reasoning_controls: "low, medium, high"
routing:
  autonomy_tier: standard
  overall_score: 88
contract:
  onboarding_passed: true
  worker_eligible: true
  orchestrator_eligible: false
evidence:
  probe_set: default
  probes_run: 13
  probes_passed: 13
`
	path := dir + "/openai__gpt-5.4-nano.yaml"
	if err := writeFile(path, yamlContent); err != nil {
		t.Fatal(err)
	}

	ps, err := NewProfileStore(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	ps.SetContextRegistry(LoadEmbeddedContextRegistry())
	if err := ps.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	p := ps.Get("openai:gpt-5.4-nano")
	if p == nil {
		t.Fatal("profile not loaded")
	}
	if p.MaxInputTokens == 0 {
		t.Error("MaxInputTokens should have been filled by registry — still 0")
	}
	if p.EffectiveContextClass == "" || p.EffectiveContextClass == "unknown" {
		t.Errorf("EffectiveContextClass should be derived from registry, got %q", p.EffectiveContextClass)
	}
}

// writeFile is a small helper to keep tests readable.
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
