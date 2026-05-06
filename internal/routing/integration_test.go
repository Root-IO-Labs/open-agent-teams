package routing_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Root-IO-Labs/open-agent-teams/internal/routing"
)

// TestEndToEnd_ProfileLoadValidateAutoSelect tests the full chain:
// load profiles → validate models → auto-select → generate prompt.
// This mirrors what the daemon does at startup and on each agent spawn.
func TestEndToEnd_ProfileLoadValidateAutoSelect(t *testing.T) {
	// Setup: copy real profile data into a temp dir
	dir := t.TempDir()
	profiles := map[string]string{
		"anthropic__claude-sonnet-4-6.yaml": `model_id: "anthropic:claude-sonnet-4-6"
status: known
provider:
  name: anthropic
capabilities:
  tool_reliability: 1.0
  shell_reliability: 1.0
  shell_recovery: 1.0
  file_write_reliability: 1.0
  token_reporting: 0.95
  streaming: 1.0
  multi_turn: 1.0
  large_output: 1.0
  effective_context_class: medium
  max_input_tokens: 200000
  reasoning_controls: "thinking_budget_5000, thinking_budget_10000"
routing:
  autonomy_tier: full
  overall_score: 99
contract:
  onboarding_passed: true
  worker_eligible: true
  orchestrator_eligible: true
`,
		"ollama__gemma3__1b.yaml": `model_id: "ollama:gemma3:1b"
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
warnings:
  - "Tool calling not supported"
`,
		"openai__o4-mini.yaml": `model_id: "openai:o4-mini"
status: known
provider:
  name: openai
capabilities:
  tool_reliability: 1.0
  shell_reliability: 1.0
  shell_recovery: 0.7
  file_write_reliability: 1.0
  token_reporting: 1.0
  streaming: 1.0
  multi_turn: 1.0
  large_output: 1.0
  effective_context_class: medium
  max_input_tokens: 200000
  reasoning_controls: "low, medium, high"
routing:
  autonomy_tier: full
  overall_score: 96
contract:
  onboarding_passed: true
  worker_eligible: true
  orchestrator_eligible: true
`,
	}

	for name, content := range profiles {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// 1. Load profiles (daemon startup)
	ps, err := routing.NewProfileStore(dir)
	if err != nil {
		t.Fatalf("Failed to load profiles: %v", err)
	}
	if ps.Count() != 3 {
		t.Fatalf("Expected 3 profiles, got %d", ps.Count())
	}

	// 2. Validate: eligible model as worker → OK
	if err := ps.Validate("anthropic:claude-sonnet-4-6", routing.RoleWorker); err != nil {
		t.Errorf("Sonnet should be valid as worker: %v", err)
	}

	// 3. Validate: restricted model as worker → REJECT
	err = ps.Validate("ollama:gemma3:1b", routing.RoleWorker)
	if err == nil {
		t.Error("Restricted model should be rejected as worker")
	}
	if err != nil && !strings.Contains(err.Error(), "not eligible") {
		t.Errorf("Expected 'not eligible' error, got: %v", err)
	}

	// 4. Validate: unknown model → REJECT with onboard hint
	err = ps.Validate("mistral:large", routing.RoleWorker)
	if err == nil {
		t.Error("Unknown model should be rejected")
	}
	if err != nil && !strings.Contains(err.Error(), "not onboarded") {
		t.Errorf("Expected 'not onboarded' error, got: %v", err)
	}

	// 5. Auto-select worker with no preference → highest score (sonnet 99)
	best, err := ps.BestEligible(routing.RoleWorker, "")
	if err != nil {
		t.Fatalf("BestEligible failed: %v", err)
	}
	if best != "anthropic:claude-sonnet-4-6" {
		t.Errorf("Auto-select picked %q, want anthropic:claude-sonnet-4-6", best)
	}

	// 6. Auto-select with repo default (o4-mini) → respects preference
	best, err = ps.BestEligible(routing.RoleWorker, "openai:o4-mini")
	if err != nil {
		t.Fatalf("BestEligible with preferred failed: %v", err)
	}
	if best != "openai:o4-mini" {
		t.Errorf("Auto-select with preferred picked %q, want openai:o4-mini", best)
	}

	// 7. Auto-select with restricted model as preference → ignores, picks best
	best, err = ps.BestEligible(routing.RoleWorker, "ollama:gemma3:1b")
	if err != nil {
		t.Fatalf("BestEligible with restricted preferred failed: %v", err)
	}
	if best != "anthropic:claude-sonnet-4-6" {
		t.Errorf("Auto-select with restricted preferred picked %q, want sonnet", best)
	}

	// 8. Generate model roster for supervisor prompt
	roster := routing.GenerateModelRoster(ps, nil)
	if roster == "" {
		t.Fatal("Model roster is empty")
	}
	if !strings.Contains(roster, "anthropic:claude-sonnet-4-6") {
		t.Error("Roster missing sonnet")
	}
	if !strings.Contains(roster, "openai:o4-mini") {
		t.Error("Roster missing o4-mini")
	}
	if strings.Contains(roster, "ollama:gemma3:1b") {
		t.Error("Roster should not include restricted model")
	}
	if !strings.Contains(roster, "Model Selection Guidelines") {
		t.Error("Roster missing guidelines")
	}
	if !strings.Contains(roster, "--model") {
		t.Error("Roster missing --model usage hint")
	}

	// 9. Reload after adding new profile
	newProfile := `model_id: "google_genai:gemini-2.5-flash"
status: known
provider:
  name: google_genai
capabilities:
  tool_reliability: 1.0
  shell_reliability: 1.0
  shell_recovery: 0.7
  file_write_reliability: 1.0
  effective_context_class: large
  max_input_tokens: 1048576
routing:
  autonomy_tier: full
  overall_score: 95
contract:
  onboarding_passed: true
  worker_eligible: true
  orchestrator_eligible: true
`
	if err := os.WriteFile(filepath.Join(dir, "google__flash.yaml"), []byte(newProfile), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ps.Reload(); err != nil {
		t.Fatalf("Reload failed: %v", err)
	}
	if ps.Count() != 4 {
		t.Errorf("After reload: expected 4 profiles, got %d", ps.Count())
	}
	if err := ps.Validate("google_genai:gemini-2.5-flash", routing.RoleWorker); err != nil {
		t.Errorf("Flash should be valid after reload: %v", err)
	}

	// 10. Roster updates after reload
	newRoster := routing.GenerateModelRoster(ps, nil)
	if !strings.Contains(newRoster, "google_genai:gemini-2.5-flash") {
		t.Error("Roster should include flash after reload")
	}
}

// TestEndToEnd_PipelineTrace verifies the complete probe→YAML→parser→roster→supervisor pipeline.
// Uses profiles that mirror what probe-model.py v2 actually generates, including:
// latency block, probe_set, shell_roundtrip (renamed field), reasoning_controls variants.
func TestEndToEnd_PipelineTrace(t *testing.T) {
	dir := t.TempDir()

	// Profile 1: Fully probed model with latency (like a real default probe run)
	sonnet := `model_id: "anthropic:claude-sonnet-4-6"
status: known
provider:
  name: anthropic
capabilities:
  tool_reliability: 1.0
  shell_roundtrip: 1.0
  shell_recovery: 0.78
  file_write_reliability: 1.0
  token_reporting: 0.95
  streaming: 1.0
  multi_turn: 0.85
  large_output: 0.9
  effective_context_class: large
  max_input_tokens: 1000000
  reasoning_controls: "thinking_budget_5000, thinking_budget_10000"
latency:
  basic_inference_ms: 1790
  tool_calling_ms: 2366
  avg_ms: 2957
routing:
  autonomy_tier: full
  overall_score: 98
contract:
  onboarding_passed: true
  worker_eligible: true
  supervisor_eligible: true
evidence:
  probe_version: 2
  probe_set: default
`
	// Profile 2: Minimum probe set model with latency (untested probes default to 1.0)
	o4mini := `model_id: "openai:o4-mini"
status: known
provider:
  name: openai
capabilities:
  tool_reliability: 1.0
  shell_roundtrip: 1.0
  shell_recovery: 1.0
  file_write_reliability: 1.0
  token_reporting: 1.0
  streaming: 1.0
  multi_turn: 1.0
  large_output: 1.0
  effective_context_class: medium
  max_input_tokens: 200000
  reasoning_controls: "low, medium, high"
latency:
  basic_inference_ms: 5076
  tool_calling_ms: 5581
  avg_ms: 4243
routing:
  autonomy_tier: full
  overall_score: 98
contract:
  onboarding_passed: true
  worker_eligible: true
  supervisor_eligible: true
evidence:
  probe_version: 1
  probe_set: minimum
`
	// Profile 3: Fully probed, no reasoning, lower score
	flash := `model_id: "google_genai:gemini-2.5-flash"
status: known
provider:
  name: google_genai
capabilities:
  tool_reliability: 1.0
  shell_roundtrip: 1.0
  shell_recovery: 0.7
  file_write_reliability: 1.0
  token_reporting: 1.0
  streaming: 1.0
  multi_turn: 1.0
  large_output: 1.0
  effective_context_class: large
  max_input_tokens: 1048576
  reasoning_controls: "none"
routing:
  autonomy_tier: full
  overall_score: 95
contract:
  onboarding_passed: true
  worker_eligible: true
  supervisor_eligible: true
evidence:
  probe_version: 1
  probe_set: default
`
	// Profile 4: Restricted model
	gemma := `model_id: "ollama:gemma3:1b"
status: restricted
provider:
  name: ollama
capabilities:
  tool_reliability: 0.0
  shell_roundtrip: 0.0
  reasoning_controls: "none"
  effective_context_class: unknown
routing:
  autonomy_tier: restricted
  overall_score: 50
contract:
  onboarding_passed: false
  worker_eligible: false
  supervisor_eligible: false
warnings:
  - "Tool calling not supported — OAT incompatible"
`

	files := map[string]string{
		"anthropic__claude-sonnet-4-6.yaml":   sonnet,
		"openai__o4-mini.yaml":                o4mini,
		"google_genai__gemini-2.5-flash.yaml": flash,
		"ollama__gemma3__1b.yaml":             gemma,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	ps, err := routing.NewProfileStore(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ps.Count() != 4 {
		t.Fatalf("Count = %d, want 4", ps.Count())
	}

	// --- Verify parser correctly extracted all new fields ---

	p := ps.Get("anthropic:claude-sonnet-4-6")
	if p == nil {
		t.Fatal("sonnet profile nil")
	}
	// shell_roundtrip (new field name)
	if p.ShellReliability != 1.0 {
		t.Errorf("sonnet ShellReliability = %v, want 1.0 (from shell_roundtrip)", p.ShellReliability)
	}
	// Latency
	if p.LatencyAvgMs != 2957 {
		t.Errorf("sonnet LatencyAvgMs = %d, want 2957", p.LatencyAvgMs)
	}
	if !p.HasLatencyData() {
		t.Error("sonnet should have latency data")
	}
	// ProbeSet
	if p.ProbeSet != "default" {
		t.Errorf("sonnet ProbeSet = %q, want default", p.ProbeSet)
	}
	if !p.IsFullyProbed() {
		t.Error("sonnet should be fully probed")
	}
	// Reasoning
	if !p.HasReasoningControls() {
		t.Error("sonnet should have reasoning controls")
	}

	// o4-mini: minimum probe set, high latency
	o4 := ps.Get("openai:o4-mini")
	if o4 == nil {
		t.Fatal("o4-mini profile nil")
	}
	if o4.ProbeSet != "minimum" {
		t.Errorf("o4-mini ProbeSet = %q, want minimum", o4.ProbeSet)
	}
	if o4.IsFullyProbed() {
		t.Error("o4-mini should NOT be fully probed (minimum set)")
	}
	if o4.LatencyAvgMs != 4243 {
		t.Errorf("o4-mini LatencyAvgMs = %d, want 4243", o4.LatencyAvgMs)
	}

	// --- Verify BestEligible uses latency as tiebreaker ---
	// sonnet and o4-mini both score 98, but sonnet has avg_ms=2957 vs o4-mini avg_ms=4243
	best, err := ps.BestEligible(routing.RoleWorker, "")
	if err != nil {
		t.Fatalf("BestEligible: %v", err)
	}
	if best != "anthropic:claude-sonnet-4-6" {
		t.Errorf("BestEligible = %q, want anthropic:claude-sonnet-4-6 (latency tiebreaker: 2957 < 4243)", best)
	}

	// --- Verify roster output ---
	roster := routing.GenerateModelRoster(ps, nil)

	// Restricted model excluded
	if strings.Contains(roster, "ollama:gemma3:1b") {
		t.Error("restricted model should not appear in roster")
	}

	// All 3 eligible models present
	for _, model := range []string{"anthropic:claude-sonnet-4-6", "openai:o4-mini", "google_genai:gemini-2.5-flash"} {
		if !strings.Contains(roster, model) {
			t.Errorf("roster missing %s", model)
		}
	}

	// Latency column populated for models with data
	if !strings.Contains(roster, "~3.0s") {
		t.Error("roster should show sonnet latency ~3.0s")
	}
	if !strings.Contains(roster, "~4.2s") {
		t.Error("roster should show o4-mini latency ~4.2s")
	}
	// Flash has no latency data
	if !strings.Contains(roster, "n/a") {
		t.Error("roster should show n/a for flash (no latency data)")
	}

	// Reasoning controls visible in strengths
	if !strings.Contains(roster, "thinking_budget") {
		t.Error("roster should show sonnet reasoning controls")
	}
	if !strings.Contains(roster, "low, medium, high") {
		t.Error("roster should show o4-mini reasoning controls")
	}

	// Flash (fully probed, reasoning=none) should show weakness
	if !strings.Contains(roster, "no reasoning controls") {
		t.Error("roster should flag flash as having no reasoning controls")
	}

	// Sonnet (fully probed, shell_recovery=0.78) should show weakness
	if !strings.Contains(roster, "shell recovery 78%") {
		t.Error("roster should flag sonnet shell recovery 78%")
	}

	// o4-mini (minimum probe set, shell_recovery=1.0 defaulted) should NOT claim "strong error recovery"
	// because it's not fully probed
	// Similarly should NOT claim "good at long tasks" (multi_turn/large_output defaulted to 1.0)

	// Time-sensitive guideline present
	if !strings.Contains(roster, "Time-sensitive") {
		t.Error("roster should include time-sensitive routing guideline")
	}
	if !strings.Contains(roster, "lowest-latency") {
		t.Error("roster should mention lowest-latency for time-sensitive tasks")
	}

	// --- Verify supervisor roster ---
	supRoster := routing.GenerateOrchestratorModelRoster(ps)
	if !strings.Contains(supRoster, "anthropic:claude-sonnet-4-6") {
		t.Error("supervisor roster should include sonnet")
	}
}

// TestEndToEnd_SupervisorEligibility verifies the supervisor gate is stricter.
func TestEndToEnd_SupervisorEligibility(t *testing.T) {
	dir := t.TempDir()

	// Model with low multi_turn → should not be supervisor eligible
	weakProfile := `model_id: "weak:model"
status: known
provider:
  name: test
capabilities:
  tool_reliability: 1.0
  shell_reliability: 1.0
  shell_recovery: 0.5
  multi_turn: 0.6
routing:
  autonomy_tier: standard
  overall_score: 80
contract:
  onboarding_passed: true
  worker_eligible: true
  orchestrator_eligible: false
`
	if err := os.WriteFile(filepath.Join(dir, "weak__model.yaml"), []byte(weakProfile), 0644); err != nil {
		t.Fatal(err)
	}

	ps, _ := routing.NewProfileStore(dir)

	// Should be valid as worker
	if err := ps.Validate("weak:model", routing.RoleWorker); err != nil {
		t.Errorf("Should be valid as worker: %v", err)
	}

	// Should be rejected as orchestrator
	if err := ps.Validate("weak:model", routing.RoleOrchestrator); err == nil {
		t.Error("Should be rejected as orchestrator (orchestrator_eligible=false)")
	}
}
