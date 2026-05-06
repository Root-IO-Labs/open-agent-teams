package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Root-IO-Labs/open-agent-teams/internal/routing"
	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
	"github.com/Root-IO-Labs/open-agent-teams/pkg/config"
)

const testProfileEligible = `model_id: "anthropic:claude-sonnet-4-6"
status: known
provider:
  name: anthropic
capabilities:
  tool_reliability: 1.0
  shell_reliability: 1.0
  shell_recovery: 1.0
  file_write_reliability: 1.0
  multi_turn: 1.0
routing:
  autonomy_tier: full
  overall_score: 99
contract:
  onboarding_passed: true
  worker_eligible: true
  orchestrator_eligible: true
`

const testProfileSecond = `model_id: "openai:o4-mini"
status: known
provider:
  name: openai
capabilities:
  tool_reliability: 1.0
  shell_reliability: 1.0
  shell_recovery: 0.7
  file_write_reliability: 1.0
  multi_turn: 1.0
routing:
  autonomy_tier: full
  overall_score: 96
contract:
  onboarding_passed: true
  worker_eligible: true
  orchestrator_eligible: true
`

const testProfileRestricted = `model_id: "ollama:gemma3:1b"
status: restricted
provider:
  name: ollama
capabilities:
  tool_reliability: 0.0
  shell_reliability: 0.0
routing:
  autonomy_tier: restricted
  overall_score: 50
contract:
  onboarding_passed: false
  worker_eligible: false
  orchestrator_eligible: false
`

// setupDaemonWithProfiles creates a daemon with model profiles loaded.
func setupDaemonWithProfiles(t *testing.T, profiles map[string]string) (*Daemon, func()) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "daemon-routing-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}

	paths := config.NewTestPaths(tmpDir)
	paths.EnsureDirectories()

	// Write profiles to ModelProfilesDir
	for name, content := range profiles {
		if err := os.WriteFile(filepath.Join(paths.ModelProfilesDir, name), []byte(content), 0644); err != nil {
			t.Fatalf("Failed to write profile %s: %v", name, err)
		}
	}

	d, err := New(paths)
	if err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("Failed to create daemon: %v", err)
	}

	// Add a test repo
	d.state.AddRepo("test-repo", &state.Repository{
		GithubURL:   "https://github.com/test/repo",
		SessionName: "test-session",
		Model:       "openai:o4-mini",
		Agents:      make(map[string]state.Agent),
	})

	cleanup := func() { os.RemoveAll(tmpDir) }
	return d, cleanup
}

func TestResolveAndValidate_ExplicitEligibleWorker(t *testing.T) {
	d, cleanup := setupDaemonWithProfiles(t, map[string]string{
		"sonnet.yaml":     testProfileEligible,
		"o4.yaml":         testProfileSecond,
		"restricted.yaml": testProfileRestricted,
	})
	defer cleanup()

	model, err := d.resolveAndValidateModel("anthropic:claude-sonnet-4-6", "openai:o4-mini", state.AgentTypeWorker, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if model != "anthropic:claude-sonnet-4-6" {
		t.Errorf("model = %q, want anthropic:claude-sonnet-4-6", model)
	}
}

func TestResolveAndValidate_ExplicitEligibleSupervisor(t *testing.T) {
	d, cleanup := setupDaemonWithProfiles(t, map[string]string{
		"sonnet.yaml": testProfileEligible,
	})
	defer cleanup()

	model, err := d.resolveAndValidateModel("anthropic:claude-sonnet-4-6", "", state.AgentTypeSupervisor, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if model != "anthropic:claude-sonnet-4-6" {
		t.Errorf("model = %q, want anthropic:claude-sonnet-4-6", model)
	}
}

func TestResolveAndValidate_ExplicitRestricted(t *testing.T) {
	d, cleanup := setupDaemonWithProfiles(t, map[string]string{
		"sonnet.yaml":     testProfileEligible,
		"restricted.yaml": testProfileRestricted,
	})
	defer cleanup()

	_, err := d.resolveAndValidateModel("ollama:gemma3:1b", "", state.AgentTypeWorker, nil)
	if err == nil {
		t.Fatal("expected error for restricted model")
	}
}

func TestResolveAndValidate_ExplicitUnknown(t *testing.T) {
	d, cleanup := setupDaemonWithProfiles(t, map[string]string{
		"sonnet.yaml": testProfileEligible,
	})
	defer cleanup()

	_, err := d.resolveAndValidateModel("mistral:large", "", state.AgentTypeWorker, nil)
	if err == nil {
		t.Fatal("expected error for unknown model")
	}
	if got := err.Error(); !contains(got, "not onboarded") {
		t.Errorf("error = %q, want 'not onboarded' message", got)
	}
}

func TestResolveAndValidate_AutoSelectBest(t *testing.T) {
	d, cleanup := setupDaemonWithProfiles(t, map[string]string{
		"sonnet.yaml": testProfileEligible,
		"o4.yaml":     testProfileSecond,
	})
	defer cleanup()

	model, err := d.resolveAndValidateModel("", "", state.AgentTypeWorker, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if model != "anthropic:claude-sonnet-4-6" {
		t.Errorf("model = %q, want anthropic:claude-sonnet-4-6", model)
	}
}

func TestResolveAndValidate_AutoSelectPrefersPreferred(t *testing.T) {
	d, cleanup := setupDaemonWithProfiles(t, map[string]string{
		"sonnet.yaml": testProfileEligible,
		"o4.yaml":     testProfileSecond,
	})
	defer cleanup()

	model, err := d.resolveAndValidateModel("", "openai:o4-mini", state.AgentTypeWorker, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if model != "openai:o4-mini" {
		t.Errorf("model = %q, want openai:o4-mini", model)
	}
}

func TestResolveAndValidate_AutoSelectIgnoresIneligiblePreferred(t *testing.T) {
	d, cleanup := setupDaemonWithProfiles(t, map[string]string{
		"sonnet.yaml":     testProfileEligible,
		"restricted.yaml": testProfileRestricted,
	})
	defer cleanup()

	model, err := d.resolveAndValidateModel("", "ollama:gemma3:1b", state.AgentTypeWorker, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if model != "anthropic:claude-sonnet-4-6" {
		t.Errorf("model = %q, want anthropic:claude-sonnet-4-6", model)
	}
}

func TestResolveAndValidate_NoProfiles_ExplicitPassthrough(t *testing.T) {
	d, cleanup := setupDaemonWithProfiles(t, map[string]string{})
	defer cleanup()

	model, err := d.resolveAndValidateModel("some:model", "", state.AgentTypeWorker, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if model != "some:model" {
		t.Errorf("model = %q, want some:model", model)
	}
}

func TestResolveAndValidate_NoProfiles_RepoFallback(t *testing.T) {
	d, cleanup := setupDaemonWithProfiles(t, map[string]string{})
	defer cleanup()

	model, err := d.resolveAndValidateModel("", "repo:default", state.AgentTypeWorker, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if model != "repo:default" {
		t.Errorf("model = %q, want repo:default", model)
	}
}

func TestResolveAndValidate_NilProfileStore(t *testing.T) {
	d, cleanup := setupDaemonWithProfiles(t, map[string]string{})
	defer cleanup()
	d.modelProfiles = nil // Explicitly nil

	model, err := d.resolveAndValidateModel("explicit:model", "repo:model", state.AgentTypeWorker, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if model != "explicit:model" {
		t.Errorf("model = %q, want explicit:model", model)
	}

	model, err = d.resolveAndValidateModel("", "repo:model", state.AgentTypeWorker, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if model != "repo:model" {
		t.Errorf("model = %q, want repo:model", model)
	}
}

func TestResolveAndValidate_AllowedModels_ExplicitAllowed(t *testing.T) {
	d, cleanup := setupDaemonWithProfiles(t, map[string]string{
		"sonnet.yaml": testProfileEligible,
		"o4.yaml":     testProfileSecond,
	})
	defer cleanup()

	allowed := []string{"openai:o4-mini"}
	model, err := d.resolveAndValidateModel("openai:o4-mini", "", state.AgentTypeWorker, allowed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if model != "openai:o4-mini" {
		t.Errorf("model = %q, want openai:o4-mini", model)
	}
}

func TestResolveAndValidate_AllowedModels_ExplicitNotAllowed(t *testing.T) {
	d, cleanup := setupDaemonWithProfiles(t, map[string]string{
		"sonnet.yaml": testProfileEligible,
		"o4.yaml":     testProfileSecond,
	})
	defer cleanup()

	allowed := []string{"openai:o4-mini"}
	_, err := d.resolveAndValidateModel("anthropic:claude-sonnet-4-6", "", state.AgentTypeWorker, allowed)
	if err == nil {
		t.Fatal("expected error for model not in allowed list")
	}
	if got := err.Error(); !contains(got, "not in the allowed worker models") {
		t.Errorf("error = %q, want 'not in the allowed worker models' message", got)
	}
}

func TestResolveAndValidate_AllowedModels_AutoSelectFromAllowed(t *testing.T) {
	d, cleanup := setupDaemonWithProfiles(t, map[string]string{
		"sonnet.yaml": testProfileEligible,
		"o4.yaml":     testProfileSecond,
	})
	defer cleanup()

	allowed := []string{"openai:o4-mini"}
	model, err := d.resolveAndValidateModel("", "", state.AgentTypeWorker, allowed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if model != "openai:o4-mini" {
		t.Errorf("model = %q, want openai:o4-mini (only allowed model)", model)
	}
}

func TestResolveAndValidate_AllowedModels_NoAllowedEligible_FallbackToRepo(t *testing.T) {
	d, cleanup := setupDaemonWithProfiles(t, map[string]string{
		"sonnet.yaml": testProfileEligible,
	})
	defer cleanup()

	allowed := []string{"nonexistent:model"}
	model, err := d.resolveAndValidateModel("", "anthropic:claude-sonnet-4-6", state.AgentTypeWorker, allowed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if model != "anthropic:claude-sonnet-4-6" {
		t.Errorf("model = %q, want anthropic:claude-sonnet-4-6 (repo fallback)", model)
	}
}

func TestResolveAndValidate_AllowedModels_NotEnforcedForSupervisor(t *testing.T) {
	d, cleanup := setupDaemonWithProfiles(t, map[string]string{
		"sonnet.yaml": testProfileEligible,
		"o4.yaml":     testProfileSecond,
	})
	defer cleanup()

	allowed := []string{"openai:o4-mini"}
	model, err := d.resolveAndValidateModel("anthropic:claude-sonnet-4-6", "", state.AgentTypeSupervisor, allowed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if model != "anthropic:claude-sonnet-4-6" {
		t.Errorf("model = %q, want anthropic:claude-sonnet-4-6 (allowed list not enforced for supervisor)", model)
	}
}

func TestDaemon_ProfilesLoadedAtStartup(t *testing.T) {
	d, cleanup := setupDaemonWithProfiles(t, map[string]string{
		"sonnet.yaml":     testProfileEligible,
		"o4.yaml":         testProfileSecond,
		"restricted.yaml": testProfileRestricted,
	})
	defer cleanup()

	if d.modelProfiles == nil {
		t.Fatal("modelProfiles should not be nil")
	}
	if d.modelProfiles.Count() != 3 {
		t.Errorf("profile count = %d, want 3", d.modelProfiles.Count())
	}

	// Verify eligible counts
	workers := d.modelProfiles.Eligible(routing.RoleWorker)
	if len(workers) != 2 {
		t.Errorf("eligible workers = %d, want 2", len(workers))
	}
}

// contains and containsHelper are defined in daemon_test.go
