package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestNormalizeModelIDForEnv pins the CLI helper to the same shape the
// daemon's `normalizeModelIDForEnv` produces. If these diverge, the
// `oat agent add` error message would tell the operator to set an env
// var with a different name than the daemon actually reads -- a silent
// recovery-path bug. Keep this in sync with
// `internal/daemon/context_capacity_test.go::TestNormalizeModelIDForEnv`
// AND with `benchmarks/test_probe_context_detection.py` (which mirrors
// the same shape on the Python side).
func TestNormalizeModelIDForEnv(t *testing.T) {
	cases := map[string]string{
		"google_genai:gemini-2.5-flash": "google_genai_gemini-2.5-flash",
		"anthropic:claude-sonnet-4":     "anthropic_claude-sonnet-4",
		"openrouter:meta-llama/llama-3": "openrouter_meta-llama_llama-3",
		"OPENAI:GPT-4O":                 "openai_gpt-4o",
		"ollama:llama3:8b":              "ollama_llama3_8b",
	}
	for raw, want := range cases {
		t.Run(raw, func(t *testing.T) {
			got := normalizeModelIDForEnv(raw)
			if got != want {
				t.Fatalf("normalizeModelIDForEnv(%q) = %q; want %q", raw, got, want)
			}
		})
	}
}

// TestModelProfileExists_FoundInHomeDir verifies the preflight helper
// reports true when an onboarded profile sits under
// `~/.oat/model-profiles/<safe>.yaml`. Uses HOME redirection rather
// than `c.paths.*` so the test exercises the same lookup the
// CLI helper does without leaking into the user's real ~/.oat.
func TestModelProfileExists_FoundInHomeDir(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	profileDir := filepath.Join(tmpHome, ".oat", "model-profiles")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Mirror the probe-model.py writer's filename convention:
	// model-id with ":" and "/" replaced by "__".
	profile := filepath.Join(profileDir, "google_genai__gemini-2.5-flash.yaml")
	if err := os.WriteFile(profile, []byte("model_id: \"google_genai:gemini-2.5-flash\"\n"), 0o644); err != nil {
		t.Fatalf("write profile: %v", err)
	}

	c := &CLI{}
	if !c.modelProfileExists("google_genai:gemini-2.5-flash") {
		t.Fatalf("modelProfileExists should find profile at %s", profile)
	}
}

// TestModelProfileExists_NotFound asserts the preflight returns false
// when the model has not been onboarded -- the trigger condition for
// the CLI's "model is not onboarded; agent NOT added" error path.
func TestModelProfileExists_NotFound(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	c := &CLI{}
	if c.modelProfileExists("anthropic:claude-never-onboarded") {
		t.Fatalf("modelProfileExists should return false for unprofiled model")
	}
}

// TestModelProfileExists_HandlesSlashInModelID verifies that model IDs
// containing "/" (openrouter:org/model shape) resolve to the correct
// double-underscore filename. Regression cover for the path-join
// behavior on the openrouter-style ID shape, which is the most
// common case for OAT users running mixed-vendor models through
// OpenRouter.
func TestModelProfileExists_HandlesSlashInModelID(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	profileDir := filepath.Join(tmpHome, ".oat", "model-profiles")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	profile := filepath.Join(profileDir, "openrouter__meta-llama__llama-3.yaml")
	if err := os.WriteFile(profile, []byte("model_id: \"openrouter:meta-llama/llama-3\"\n"), 0o644); err != nil {
		t.Fatalf("write profile: %v", err)
	}

	c := &CLI{}
	if !c.modelProfileExists("openrouter:meta-llama/llama-3") {
		t.Fatalf("modelProfileExists should find slash-bearing ID at %s", profile)
	}
}
