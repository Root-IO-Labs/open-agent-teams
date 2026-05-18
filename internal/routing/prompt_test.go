package routing

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateModelRoster(t *testing.T) {
	dir := setupTestProfiles(t)
	ps, _ := NewProfileStore(dir)

	roster := GenerateModelRoster(ps, nil)

	// Should contain the header
	if !strings.Contains(roster, "## Available Models for Workers") {
		t.Error("missing header")
	}

	// Should contain eligible models
	if !strings.Contains(roster, "anthropic:claude-sonnet-4-6") {
		t.Error("missing sonnet in roster")
	}
	if !strings.Contains(roster, "google_genai:gemini-2.5-flash") {
		t.Error("missing flash in roster")
	}

	// Should NOT contain restricted model
	if strings.Contains(roster, "ollama:gemma3:1b") {
		t.Error("restricted model should not appear in roster")
	}

	// Should contain guidelines with key restriction notice
	if !strings.Contains(roster, "Only use models in the table above") {
		t.Error("missing key restriction notice in guidelines")
	}
}

func TestGenerateModelRoster_Empty(t *testing.T) {
	dir := t.TempDir()
	ps, _ := NewProfileStore(dir)

	roster := GenerateModelRoster(ps, nil)
	if roster != "" {
		t.Errorf("expected empty roster, got %q", roster)
	}
}

func TestGenerateModelRoster_Filtered(t *testing.T) {
	dir := setupTestProfiles(t)
	ps, _ := NewProfileStore(dir)

	allowed := []string{"google_genai:gemini-2.5-flash"}
	roster := GenerateModelRoster(ps, allowed)

	if !strings.Contains(roster, "google_genai:gemini-2.5-flash") {
		t.Error("allowed model should appear in filtered roster")
	}
	if strings.Contains(roster, "anthropic:claude-sonnet-4-6") {
		t.Error("non-allowed model should not appear in filtered roster")
	}
	if strings.Contains(roster, "ollama:gemma3:1b") {
		t.Error("restricted model should not appear in filtered roster")
	}
}

func TestGenerateOrchestratorModelRoster(t *testing.T) {
	dir := setupTestProfiles(t)
	ps, _ := NewProfileStore(dir)

	roster := GenerateOrchestratorModelRoster(ps)

	if !strings.Contains(roster, "## Available Supervisor Models") {
		t.Error("missing supervisor header")
	}
	if !strings.Contains(roster, "anthropic:claude-sonnet-4-6") {
		t.Error("missing sonnet in orchestrator roster")
	}
	if strings.Contains(roster, "ollama:gemma3:1b") {
		t.Error("restricted model should not appear in orchestrator roster")
	}
}

func TestSummarizeStrengths(t *testing.T) {
	p := parseProfile(sampleProfileSonnet)
	s := summarizeStrengths(p)

	if !strings.Contains(s, "all tools reliable") {
		t.Errorf("expected 'all tools reliable' in %q", s)
	}
	if !strings.Contains(s, "reasoning") {
		t.Errorf("expected reasoning mention in %q", s)
	}
}

func TestSummarizeWeaknesses(t *testing.T) {
	p := parseProfile(sampleProfileFlash)
	w := summarizeWeaknesses(p)

	if !strings.Contains(w, "shell recovery") {
		t.Errorf("expected shell recovery weakness in %q", w)
	}
	if !strings.Contains(w, "no reasoning") {
		t.Errorf("expected no reasoning weakness in %q", w)
	}
}

func TestFormatContext(t *testing.T) {
	tests := []struct {
		name  string
		input *ModelProfile
		want  string
	}{
		{"large tokens", &ModelProfile{MaxInputTokens: 1048576}, "1.0M"},
		{"medium tokens", &ModelProfile{MaxInputTokens: 200000}, "200K"},
		{"unknown", &ModelProfile{MaxInputTokens: 0, EffectiveContextClass: "unknown"}, "unknown"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatContext(tc.input)
			if got != tc.want {
				t.Errorf("formatContext = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFormatLatency(t *testing.T) {
	tests := []struct {
		name string
		avg  int
		want string
	}{
		{"no data", 0, "n/a"},
		{"fast", 1500, "~1500ms (fast)"},
		{"boundary fast", 1999, "~1999ms (fast)"},
		{"boundary medium", 2000, "~2.0s"},
		{"medium", 2957, "~3.0s"},
		{"boundary slow", 5000, "~5.0s (slow)"},
		{"slow", 5076, "~5.1s (slow)"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &ModelProfile{LatencyAvgMs: tc.avg}
			got := formatLatency(p)
			if got != tc.want {
				t.Errorf("formatLatency(avg=%d) = %q, want %q", tc.avg, got, tc.want)
			}
		})
	}
}

func TestSummarizeWeaknesses_NotTestedReasoningControls(t *testing.T) {
	p := &ModelProfile{
		ReasoningControls:     "not_tested",
		ShellRecovery:         1.0,
		EffectiveContextClass: "large",
		TokenReporting:        1.0,
	}
	w := summarizeWeaknesses(p)
	if strings.Contains(w, "no reasoning") {
		t.Errorf("should not report reasoning weakness for 'not_tested', got %q", w)
	}
	if w != "none notable" {
		t.Errorf("expected 'none notable', got %q", w)
	}
}

func TestSummarizeWeaknesses_MinimumProbeSetZeroRecovery(t *testing.T) {
	// Minimum probe set: shell_recovery=0.0 should NOT be flagged (untested)
	p := &ModelProfile{
		ProbeSet:              "minimum",
		ShellRecovery:         0.0,
		ReasoningControls:     "not_tested",
		EffectiveContextClass: "large",
		TokenReporting:        1.0,
	}
	w := summarizeWeaknesses(p)
	if strings.Contains(w, "shell recovery") {
		t.Errorf("should not flag shell_recovery=0.0 on minimum probe set, got %q", w)
	}
}

func TestSummarizeWeaknesses_MinimumProbeSetTestedRecovery(t *testing.T) {
	// Minimum probe set: shell_recovery=0.5 SHOULD be flagged (tested and weak)
	p := &ModelProfile{
		ProbeSet:              "minimum",
		ShellRecovery:         0.5,
		ReasoningControls:     "low, high",
		EffectiveContextClass: "large",
		TokenReporting:        1.0,
	}
	w := summarizeWeaknesses(p)
	if !strings.Contains(w, "shell recovery") {
		t.Errorf("should flag shell_recovery=0.5 even on minimum probe set, got %q", w)
	}
}

func TestGenerateModelRoster_AnnotatesMinimumSet(t *testing.T) {
	dir := t.TempDir()
	minProfile := `model_id: "test:min-annotate"
status: known
provider:
  name: test
capabilities:
  tool_reliability: 1.0
  shell_roundtrip: 1.0
  file_write_reliability: 1.0
  token_reporting: 1.0
  effective_context_class: large
  max_input_tokens: 200000
  reasoning_controls: "not_tested"
routing:
  autonomy_tier: standard
  overall_score: 85
contract:
  onboarding_passed: true
  worker_eligible: true
  supervisor_eligible: true
evidence:
  probe_set: minimum
  probes_run: 6
  probes_passed: 6
`
	if err := os.WriteFile(filepath.Join(dir, "min.yaml"), []byte(minProfile), 0644); err != nil {
		t.Fatal(err)
	}
	ps, err := NewProfileStore(dir)
	if err != nil {
		t.Fatalf("NewProfileStore: %v", err)
	}
	roster := GenerateModelRoster(ps, nil)
	if !strings.Contains(roster, "⚠ minimum probe set") {
		t.Errorf("expected '⚠ minimum probe set' annotation in roster, got:\n%s", roster)
	}
}

func TestGenerateModelRoster_IncludesCoverageColumn(t *testing.T) {
	dir := setupTestProfiles(t)
	ps, _ := NewProfileStore(dir)

	roster := GenerateModelRoster(ps, nil)

	// Column header
	if !strings.Contains(roster, "Coverage") {
		t.Errorf("expected 'Coverage' column header in roster, got:\n%s", roster)
	}
	// sampleProfileSonnet has probes_run: 12, probes_passed: 12
	if !strings.Contains(roster, "12/12") {
		t.Errorf("expected '12/12' coverage cell for sonnet, got:\n%s", roster)
	}
	// sampleProfileFlash has no probes_run/probes_passed → legacy em-dash
	if !strings.Contains(roster, "—") {
		t.Errorf("expected '—' coverage cell for flash (no probe counts), got:\n%s", roster)
	}
}

func TestFormatCoverage(t *testing.T) {
	tests := []struct {
		name     string
		run      int
		passed   int
		expected string
	}{
		{"all passed", 13, 13, "13/13"},
		{"partial", 13, 11, "11/13"},
		{"zero run", 0, 0, "—"},
		{"negative guard", -1, 0, "—"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &ModelProfile{ProbesRun: tc.run, ProbesPassed: tc.passed}
			got := formatCoverage(p)
			if got != tc.expected {
				t.Errorf("formatCoverage(run=%d, passed=%d) = %q, want %q",
					tc.run, tc.passed, got, tc.expected)
			}
		})
	}
}
