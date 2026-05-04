package routing

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// captureLogger is a test-local Logger implementation that records every
// emitted line (with its severity label) so tests can assert the routing
// package logged the diagnostics we expect. It is safe for concurrent use;
// load() itself is single-threaded but tests may run in parallel.
type captureLogger struct {
	mu    sync.Mutex
	lines []string
}

func (c *captureLogger) Infof(format string, args ...interface{})  { c.record("INFO", format, args) }
func (c *captureLogger) Warnf(format string, args ...interface{})  { c.record("WARN", format, args) }
func (c *captureLogger) Errorf(format string, args ...interface{}) { c.record("ERROR", format, args) }

func (c *captureLogger) record(level, format string, args []interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, level+": "+sprintfSafe(format, args...))
}

// snapshot returns a copy of the captured lines so tests can iterate without
// racing against later log writes.
func (c *captureLogger) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.lines))
	copy(out, c.lines)
	return out
}

// sprintfSafe is a trivial alias for fmt.Sprintf kept in a helper so the
// captureLogger can format its messages without exposing fmt in its API.
func sprintfSafe(format string, args ...interface{}) string {
	return fmt.Sprintf(format, args...)
}

// containsLine reports whether any captured line at the given level contains
// the substring. Useful so tests can be loose about exact formatting.
func (c *captureLogger) containsLine(level, substring string) bool {
	for _, line := range c.snapshot() {
		if strings.HasPrefix(line, level+": ") && strings.Contains(line, substring) {
			return true
		}
	}
	return false
}

const sampleProfileSonnet = `# OAT Model Capability Profile
model_id: "anthropic:claude-sonnet-4-6"
status: known
source: onboarded
onboarded_at: "2026-04-08"

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

evidence:
  probe_version: 1
  probes_run: 12
  probes_passed: 12
`

const sampleProfileRestricted = `model_id: "ollama:gemma3:1b"
status: restricted
source: onboarded

provider:
  name: ollama

capabilities:
  tool_reliability: 0.0
  shell_reliability: 0.0
  shell_recovery: 0.7
  file_write_reliability: 0.0
  token_reporting: 0.85
  streaming: 1.0
  multi_turn: 1.0
  large_output: 1.0
  effective_context_class: unknown
  max_input_tokens: unknown
  reasoning_controls: "none"

routing:
  autonomy_tier: restricted
  overall_score: 50

contract:
  onboarding_passed: false
  worker_eligible: false
  orchestrator_eligible: false

warnings:
  - "Tool calling not supported"
  - "Shell execution not working"
`

const sampleProfileFlash = `model_id: "google_genai:gemini-2.5-flash"
status: known
source: onboarded

provider:
  name: google_genai

capabilities:
  tool_reliability: 1.0
  shell_reliability: 1.0
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
  orchestrator_eligible: true
`

func setupTestProfiles(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"anthropic__claude-sonnet-4-6.yaml":   sampleProfileSonnet,
		"ollama__gemma3__1b.yaml":             sampleProfileRestricted,
		"google_genai__gemini-2.5-flash.yaml": sampleProfileFlash,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestProfileStore_Load(t *testing.T) {
	dir := setupTestProfiles(t)
	ps, err := NewProfileStore(dir)
	if err != nil {
		t.Fatalf("NewProfileStore: %v", err)
	}
	if ps.Count() != 3 {
		t.Errorf("Count = %d, want 3", ps.Count())
	}
}

func TestProfileStore_Get(t *testing.T) {
	dir := setupTestProfiles(t)
	ps, _ := NewProfileStore(dir)

	p := ps.Get("anthropic:claude-sonnet-4-6")
	if p == nil {
		t.Fatal("expected sonnet profile, got nil")
	}
	if p.OverallScore != 99 {
		t.Errorf("OverallScore = %d, want 99", p.OverallScore)
	}
	if !p.WorkerEligible {
		t.Error("expected WorkerEligible = true")
	}
	if !p.HasReasoningControls() {
		t.Error("expected HasReasoningControls = true")
	}
	if p.MaxInputTokens != 200000 {
		t.Errorf("MaxInputTokens = %d, want 200000", p.MaxInputTokens)
	}
}

func TestProfileStore_GetRestricted(t *testing.T) {
	dir := setupTestProfiles(t)
	ps, _ := NewProfileStore(dir)

	p := ps.Get("ollama:gemma3:1b")
	if p == nil {
		t.Fatal("expected gemma profile, got nil")
	}
	if p.Status != "restricted" {
		t.Errorf("Status = %q, want restricted", p.Status)
	}
	if p.WorkerEligible {
		t.Error("expected WorkerEligible = false")
	}
	if p.OrchestratorEligible {
		t.Error("expected OrchestratorEligible = false")
	}
	if len(p.Warnings) != 2 {
		t.Errorf("Warnings count = %d, want 2", len(p.Warnings))
	}
}

func TestProfileStore_Validate(t *testing.T) {
	dir := setupTestProfiles(t)
	ps, _ := NewProfileStore(dir)

	tests := []struct {
		name    string
		modelID string
		role    AgentRole
		wantErr bool
	}{
		{"eligible worker", "anthropic:claude-sonnet-4-6", RoleWorker, false},
		{"eligible orchestrator", "anthropic:claude-sonnet-4-6", RoleOrchestrator, false},
		{"restricted worker", "ollama:gemma3:1b", RoleWorker, true},
		{"restricted orchestrator", "ollama:gemma3:1b", RoleOrchestrator, true},
		{"unknown model", "openai:gpt-99", RoleWorker, true},
		{"flash as worker", "google_genai:gemini-2.5-flash", RoleWorker, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ps.Validate(tc.modelID, tc.role)
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate(%q, %s) error = %v, wantErr = %v", tc.modelID, tc.role, err, tc.wantErr)
			}
		})
	}
}

func TestProfileStore_BestEligible(t *testing.T) {
	dir := setupTestProfiles(t)
	ps, _ := NewProfileStore(dir)

	// Best worker should be sonnet (score 99)
	best, err := ps.BestEligible(RoleWorker, "")
	if err != nil {
		t.Fatalf("BestEligible: %v", err)
	}
	if best != "anthropic:claude-sonnet-4-6" {
		t.Errorf("BestEligible = %q, want anthropic:claude-sonnet-4-6", best)
	}
}

func TestProfileStore_BestEligible_PrefersPreferred(t *testing.T) {
	dir := setupTestProfiles(t)
	ps, _ := NewProfileStore(dir)

	// If preferred model is eligible, use it even if not highest score
	best, err := ps.BestEligible(RoleWorker, "google_genai:gemini-2.5-flash")
	if err != nil {
		t.Fatalf("BestEligible: %v", err)
	}
	if best != "google_genai:gemini-2.5-flash" {
		t.Errorf("BestEligible = %q, want google_genai:gemini-2.5-flash", best)
	}
}

func TestProfileStore_BestEligible_IgnoresIneligiblePreferred(t *testing.T) {
	dir := setupTestProfiles(t)
	ps, _ := NewProfileStore(dir)

	// Restricted model as preferred → ignored, falls back to best
	best, err := ps.BestEligible(RoleWorker, "ollama:gemma3:1b")
	if err != nil {
		t.Fatalf("BestEligible: %v", err)
	}
	if best != "anthropic:claude-sonnet-4-6" {
		t.Errorf("BestEligible = %q, want anthropic:claude-sonnet-4-6", best)
	}
}

func TestProfileStore_Eligible(t *testing.T) {
	dir := setupTestProfiles(t)
	ps, _ := NewProfileStore(dir)

	workers := ps.Eligible(RoleWorker)
	if len(workers) != 2 {
		t.Errorf("Eligible(Worker) = %d profiles, want 2", len(workers))
	}

	orchestrators := ps.Eligible(RoleOrchestrator)
	if len(orchestrators) != 2 {
		t.Errorf("Eligible(Orchestrator) = %d profiles, want 2", len(orchestrators))
	}
}

func TestProfileStore_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	ps, err := NewProfileStore(dir)
	if err != nil {
		t.Fatalf("NewProfileStore: %v", err)
	}
	if ps.Count() != 0 {
		t.Errorf("Count = %d, want 0", ps.Count())
	}

	_, err = ps.BestEligible(RoleWorker, "")
	if err == nil {
		t.Error("expected error from BestEligible with no profiles")
	}
}

func TestProfileStore_NonExistentDir(t *testing.T) {
	ps, err := NewProfileStore("/tmp/does-not-exist-routing-test")
	if err != nil {
		t.Fatalf("NewProfileStore should not error on missing dir: %v", err)
	}
	if ps.Count() != 0 {
		t.Errorf("Count = %d, want 0", ps.Count())
	}
}

func TestProfileStore_Reload(t *testing.T) {
	dir := setupTestProfiles(t)
	ps, _ := NewProfileStore(dir)
	if ps.Count() != 3 {
		t.Fatalf("initial Count = %d, want 3", ps.Count())
	}

	// Add a new profile
	newProfile := `model_id: "openai:o4-mini"
status: known
provider:
  name: openai
capabilities:
  tool_reliability: 1.0
  shell_reliability: 1.0
  shell_recovery: 0.7
  file_write_reliability: 1.0
routing:
  autonomy_tier: full
  overall_score: 96
contract:
  onboarding_passed: true
  worker_eligible: true
  orchestrator_eligible: true
`
	if err := os.WriteFile(filepath.Join(dir, "openai__o4-mini.yaml"), []byte(newProfile), 0644); err != nil {
		t.Fatal(err)
	}

	if err := ps.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if ps.Count() != 4 {
		t.Errorf("after Reload Count = %d, want 4", ps.Count())
	}
	if ps.Get("openai:o4-mini") == nil {
		t.Error("expected o4-mini profile after reload")
	}
}

func TestParseProfile_Warnings(t *testing.T) {
	p := parseProfile(sampleProfileRestricted)
	if len(p.Warnings) != 2 {
		t.Errorf("Warnings = %d, want 2", len(p.Warnings))
	}
	if p.Warnings[0] != "Tool calling not supported" {
		t.Errorf("Warnings[0] = %q", p.Warnings[0])
	}
}

func TestProfileStore_MalformedProfile_NoModelID(t *testing.T) {
	dir := t.TempDir()
	// Profile missing model_id should be silently skipped
	malformed := `status: known
provider:
  name: test
routing:
  autonomy_tier: standard
  overall_score: 80
contract:
  worker_eligible: true
`
	os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte(malformed), 0644)
	// Also add a valid one
	os.WriteFile(filepath.Join(dir, "good.yaml"), []byte(sampleProfileSonnet), 0644)

	ps, err := NewProfileStore(dir)
	if err != nil {
		t.Fatalf("NewProfileStore: %v", err)
	}
	// Only the valid profile should be loaded
	if ps.Count() != 1 {
		t.Errorf("Count = %d, want 1 (malformed should be skipped)", ps.Count())
	}
}

func TestProfileStore_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "empty.yaml"), []byte(""), 0644)
	os.WriteFile(filepath.Join(dir, "good.yaml"), []byte(sampleProfileSonnet), 0644)

	ps, _ := NewProfileStore(dir)
	if ps.Count() != 1 {
		t.Errorf("Count = %d, want 1", ps.Count())
	}
}

func TestProfileStore_NonYAMLFilesIgnored(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("not a profile"), 0644)
	os.WriteFile(filepath.Join(dir, "good.yaml"), []byte(sampleProfileSonnet), 0644)

	ps, _ := NewProfileStore(dir)
	if ps.Count() != 1 {
		t.Errorf("Count = %d, want 1", ps.Count())
	}
}

func TestAgentRole_String(t *testing.T) {
	if RoleWorker.String() != "worker" {
		t.Errorf("RoleWorker.String() = %q", RoleWorker.String())
	}
	if RoleOrchestrator.String() != "orchestrator" {
		t.Errorf("RoleOrchestrator.String() = %q", RoleOrchestrator.String())
	}
}

func TestParseProfile_ShellRoundtripFallback(t *testing.T) {
	// New field name: shell_roundtrip
	profileRoundtrip := `model_id: "test:roundtrip"
status: known
capabilities:
  shell_roundtrip: 0.9
contract:
  onboarding_passed: true
  worker_eligible: true
`
	p := parseProfile(profileRoundtrip)
	if p == nil {
		t.Fatal("expected profile, got nil")
	}
	if p.ShellReliability != 0.9 {
		t.Errorf("shell_roundtrip: got %v, want 0.9", p.ShellReliability)
	}

	// Old field name: shell_reliability (fallback)
	profileOld := `model_id: "test:old"
status: known
capabilities:
  shell_reliability: 0.8
contract:
  onboarding_passed: true
  worker_eligible: true
`
	p2 := parseProfile(profileOld)
	if p2.ShellReliability != 0.8 {
		t.Errorf("shell_reliability fallback: got %v, want 0.8", p2.ShellReliability)
	}
}

func TestParseProfile_LatencyFields(t *testing.T) {
	profile := `model_id: "test:latency"
status: known
capabilities:
  tool_reliability: 1.0
latency:
  basic_inference_ms: 1790
  tool_calling_ms: 2366
  avg_ms: 2957
contract:
  onboarding_passed: true
  worker_eligible: true
`
	p := parseProfile(profile)
	if p == nil {
		t.Fatal("expected profile, got nil")
	}
	if p.LatencyAvgMs != 2957 {
		t.Errorf("LatencyAvgMs = %d, want 2957", p.LatencyAvgMs)
	}
	if p.LatencyBasicInferenceMs != 1790 {
		t.Errorf("LatencyBasicInferenceMs = %d, want 1790", p.LatencyBasicInferenceMs)
	}
	if p.LatencyToolCallMs != 2366 {
		t.Errorf("LatencyToolCallMs = %d, want 2366", p.LatencyToolCallMs)
	}
	if !p.HasLatencyData() {
		t.Error("expected HasLatencyData = true")
	}
}

func TestParseProfile_NoLatency(t *testing.T) {
	p := parseProfile(sampleProfileFlash) // No latency block
	if p.HasLatencyData() {
		t.Error("expected HasLatencyData = false for profile without latency")
	}
	if p.LatencyAvgMs != 0 {
		t.Errorf("LatencyAvgMs = %d, want 0", p.LatencyAvgMs)
	}
}

func TestParseProfile_ProbeSet(t *testing.T) {
	profileMinimum := `model_id: "test:min"
status: known
evidence:
  probe_set: minimum
contract:
  onboarding_passed: true
  worker_eligible: true
`
	p := parseProfile(profileMinimum)
	if p.ProbeSet != "minimum" {
		t.Errorf("ProbeSet = %q, want minimum", p.ProbeSet)
	}
	if p.IsFullyProbed() {
		t.Error("expected IsFullyProbed = false for minimum probe set")
	}
}

func TestIsFullyProbed(t *testing.T) {
	tests := []struct {
		probeSet string
		want     bool
	}{
		{"", true},
		{"default", true},
		{"minimum", false},
		{"unknown", false},
	}
	for _, tc := range tests {
		p := &ModelProfile{ProbeSet: tc.probeSet}
		if got := p.IsFullyProbed(); got != tc.want {
			t.Errorf("IsFullyProbed(%q) = %v, want %v", tc.probeSet, got, tc.want)
		}
	}
}

func TestBestEligible_MinimumSetPenalty(t *testing.T) {
	dir := t.TempDir()
	// Two worker-eligible profiles with identical overall_score=70:
	// one fully-probed, one minimum. Default should win (70 vs 70-5=65).
	defaultProfile := `model_id: "test:default-70"
status: known
provider:
  name: test
capabilities:
  tool_reliability: 1.0
routing:
  autonomy_tier: standard
  overall_score: 70
contract:
  onboarding_passed: true
  worker_eligible: true
  supervisor_eligible: true
evidence:
  probe_set: default
`
	minimumProfile := `model_id: "test:minimum-70"
status: known
provider:
  name: test
capabilities:
  tool_reliability: 1.0
routing:
  autonomy_tier: standard
  overall_score: 70
contract:
  onboarding_passed: true
  worker_eligible: true
  supervisor_eligible: true
evidence:
  probe_set: minimum
`
	if err := os.WriteFile(filepath.Join(dir, "default.yaml"), []byte(defaultProfile), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "minimum.yaml"), []byte(minimumProfile), 0644); err != nil {
		t.Fatal(err)
	}

	ps, err := NewProfileStore(dir)
	if err != nil {
		t.Fatalf("NewProfileStore: %v", err)
	}
	best, err := ps.BestEligible(RoleWorker, "")
	if err != nil {
		t.Fatalf("BestEligible: %v", err)
	}
	if best != "test:default-70" {
		t.Errorf("BestEligible = %q, want test:default-70 (minimum should be penalised)", best)
	}
}

func TestBestEligible_MinimumSetStillWinsIfMuchHigher(t *testing.T) {
	dir := t.TempDir()
	// Minimum at 90 vs default at 80: minimum should still win (90-5=85 > 80).
	highMinimum := `model_id: "test:minimum-90"
status: known
provider:
  name: test
routing:
  autonomy_tier: standard
  overall_score: 90
contract:
  onboarding_passed: true
  worker_eligible: true
  supervisor_eligible: true
evidence:
  probe_set: minimum
`
	lowDefault := `model_id: "test:default-80"
status: known
provider:
  name: test
routing:
  autonomy_tier: standard
  overall_score: 80
contract:
  onboarding_passed: true
  worker_eligible: true
  supervisor_eligible: true
evidence:
  probe_set: default
`
	os.WriteFile(filepath.Join(dir, "min.yaml"), []byte(highMinimum), 0644)
	os.WriteFile(filepath.Join(dir, "def.yaml"), []byte(lowDefault), 0644)

	ps, _ := NewProfileStore(dir)
	best, err := ps.BestEligible(RoleWorker, "")
	if err != nil {
		t.Fatalf("BestEligible: %v", err)
	}
	if best != "test:minimum-90" {
		t.Errorf("BestEligible = %q, want test:minimum-90 (90-5=85 > 80)", best)
	}
}

func TestBestEligible_MinimumSetTiebreakAtPenaltyBoundary(t *testing.T) {
	dir := t.TempDir()
	// minimum=75 vs default=70: 75-5=70 == 70 → lexicographic tiebreaker kicks in.
	// "test:default-70" < "test:minimum-75", so default wins.
	minAt75 := `model_id: "test:minimum-75"
status: known
provider:
  name: test
routing:
  autonomy_tier: standard
  overall_score: 75
contract:
  onboarding_passed: true
  worker_eligible: true
  supervisor_eligible: true
evidence:
  probe_set: minimum
`
	defAt70 := `model_id: "test:default-70"
status: known
provider:
  name: test
routing:
  autonomy_tier: standard
  overall_score: 70
contract:
  onboarding_passed: true
  worker_eligible: true
  supervisor_eligible: true
evidence:
  probe_set: default
`
	os.WriteFile(filepath.Join(dir, "min75.yaml"), []byte(minAt75), 0644)
	os.WriteFile(filepath.Join(dir, "def70.yaml"), []byte(defAt70), 0644)

	ps, _ := NewProfileStore(dir)
	best, err := ps.BestEligible(RoleWorker, "")
	if err != nil {
		t.Fatalf("BestEligible: %v", err)
	}
	// Latency is 0 for both; lexicographic fallback picks smaller ID.
	if best != "test:default-70" {
		t.Errorf("BestEligible = %q, want test:default-70 (tie on effective score → lex)", best)
	}
}

func TestParseProfile_PopulatesProbeCounts(t *testing.T) {
	profile := `model_id: "test:counts"
status: known
contract:
  onboarding_passed: true
  worker_eligible: true
evidence:
  probes_run: 13
  probes_passed: 12
`
	p := parseProfile(profile)
	if p.ProbesRun != 13 {
		t.Errorf("ProbesRun = %d, want 13", p.ProbesRun)
	}
	if p.ProbesPassed != 12 {
		t.Errorf("ProbesPassed = %d, want 12", p.ProbesPassed)
	}

	// Legacy profile with no probe counts → zero values (treated as "unknown").
	legacy := `model_id: "test:legacy"
status: known
contract:
  onboarding_passed: true
  worker_eligible: true
`
	p2 := parseProfile(legacy)
	if p2.ProbesRun != 0 {
		t.Errorf("legacy ProbesRun = %d, want 0", p2.ProbesRun)
	}
	if p2.ProbesPassed != 0 {
		t.Errorf("legacy ProbesPassed = %d, want 0", p2.ProbesPassed)
	}
}

func TestParseProfile_ReasoningNotTested(t *testing.T) {
	profile := `model_id: "test:nort"
status: known
capabilities:
  reasoning_controls: "not_tested"
contract:
  onboarding_passed: true
  worker_eligible: true
`
	p := parseProfile(profile)
	if p.HasReasoningControls() {
		t.Error("expected HasReasoningControls = false for 'not_tested'")
	}
	if p.ReasoningControls != "not_tested" {
		t.Errorf("ReasoningControls = %q, want not_tested", p.ReasoningControls)
	}
}

// -----------------------------------------------------------------------
// Diagnostic-logging tests (PR #1: routing hardening).
// Each test installs a captureLogger via NewProfileStoreWithLogger, writes
// a mix of valid and malformed fixtures, and asserts both the resulting
// profile count and the exact set of WARN/ERROR messages emitted.
// -----------------------------------------------------------------------

// TestLoad_LogsMalformedProfiles asserts that load() emits a WARN for a file
// whose content cannot be parsed into a profile with a model_id (in this
// case: inline contents that simply lack the model_id key). The valid
// control file must still load.
func TestLoad_LogsMalformedProfiles(t *testing.T) {
	dir := t.TempDir()
	// File with no model_id (simulates a typo like "mode_id:").
	malformed := `status: known
provider:
  name: test
`
	if err := os.WriteFile(filepath.Join(dir, "missing-model-id.yaml"), []byte(malformed), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "good.yaml"), []byte(sampleProfileSonnet), 0644); err != nil {
		t.Fatal(err)
	}

	cap := &captureLogger{}
	ps, err := NewProfileStoreWithLogger(dir, cap)
	if err != nil {
		t.Fatalf("NewProfileStoreWithLogger: %v", err)
	}
	if ps.Count() != 1 {
		t.Errorf("Count = %d, want 1", ps.Count())
	}
	if !cap.containsLine("WARN", "missing-model-id.yaml") {
		t.Errorf("expected WARN referencing missing-model-id.yaml, got %v", cap.snapshot())
	}
	if !cap.containsLine("WARN", "missing or malformed model_id") {
		t.Errorf("expected WARN mentioning missing model_id, got %v", cap.snapshot())
	}
}

// TestLoad_SkipsMissingModelID_LogsWarn is a variant of the above that
// specifically checks the existing "malformed with no model_id" skip path
// still works AND now produces a WARN (the pre-existing regression suite
// covered the skip; this extends it to the new log emission).
func TestLoad_SkipsMissingModelID_LogsWarn(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "empty.yaml"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "good.yaml"), []byte(sampleProfileSonnet), 0644); err != nil {
		t.Fatal(err)
	}

	cap := &captureLogger{}
	ps, err := NewProfileStoreWithLogger(dir, cap)
	if err != nil {
		t.Fatalf("NewProfileStoreWithLogger: %v", err)
	}
	if ps.Count() != 1 {
		t.Errorf("Count = %d, want 1", ps.Count())
	}
	if !cap.containsLine("WARN", "empty.yaml") {
		t.Errorf("expected WARN for empty.yaml, got %v", cap.snapshot())
	}
}

// TestLoad_SkipsMissingRequiredFields exercises validateRequiredFields by
// writing one fixture per required field and asserting each is rejected with
// a dedicated ERROR message. The control file (sampleProfileSonnet) loads
// normally so Count == 1 proves we didn't over-reject.
func TestLoad_SkipsMissingRequiredFields(t *testing.T) {
	dir := t.TempDir()

	// Missing provider.name — has model_id, autonomy_tier, score+passed.
	missingProvider := `model_id: "test:missing-provider"
status: known
routing:
  autonomy_tier: full
  overall_score: 80
contract:
  onboarding_passed: true
  worker_eligible: true
`
	// Missing routing.autonomy_tier.
	missingTier := `model_id: "test:missing-tier"
status: known
provider:
  name: test
routing:
  overall_score: 80
contract:
  onboarding_passed: true
  worker_eligible: true
`
	// Missing both overall_score and onboarding_passed (the literal joint
	// check from Q3=A). A profile that writes onboarding_passed: false AND
	// omits a score should be rejected as "skeleton / unfinished".
	missingScoreAndPass := `model_id: "test:unfinished"
status: known
provider:
  name: test
routing:
  autonomy_tier: full
contract:
  worker_eligible: true
`
	writes := map[string]string{
		"missing-provider.yaml": missingProvider,
		"missing-tier.yaml":     missingTier,
		"unfinished.yaml":       missingScoreAndPass,
		"good.yaml":             sampleProfileSonnet,
	}
	for name, content := range writes {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	cap := &captureLogger{}
	ps, err := NewProfileStoreWithLogger(dir, cap)
	if err != nil {
		t.Fatalf("NewProfileStoreWithLogger: %v", err)
	}
	if ps.Count() != 1 {
		t.Errorf("Count = %d, want 1 (only good.yaml should load)", ps.Count())
	}
	if !cap.containsLine("ERROR", "missing-provider.yaml") {
		t.Errorf("expected ERROR for missing-provider.yaml, got %v", cap.snapshot())
	}
	if !cap.containsLine("ERROR", "provider.name") {
		t.Errorf("expected ERROR mentioning provider.name, got %v", cap.snapshot())
	}
	if !cap.containsLine("ERROR", "missing-tier.yaml") {
		t.Errorf("expected ERROR for missing-tier.yaml, got %v", cap.snapshot())
	}
	if !cap.containsLine("ERROR", "routing.autonomy_tier") {
		t.Errorf("expected ERROR mentioning routing.autonomy_tier, got %v", cap.snapshot())
	}
	if !cap.containsLine("ERROR", "unfinished.yaml") {
		t.Errorf("expected ERROR for unfinished.yaml, got %v", cap.snapshot())
	}
}

// TestLoad_LogsCountOnSuccess asserts the INFO "loaded N model profile(s) from DIR"
// emission is present for a populated directory.
func TestLoad_LogsCountOnSuccess(t *testing.T) {
	dir := setupTestProfiles(t)
	cap := &captureLogger{}
	ps, err := NewProfileStoreWithLogger(dir, cap)
	if err != nil {
		t.Fatalf("NewProfileStoreWithLogger: %v", err)
	}
	if ps.Count() != 3 {
		t.Fatalf("Count = %d, want 3", ps.Count())
	}
	if !cap.containsLine("INFO", "loaded 3 model profile(s)") {
		t.Errorf("expected INFO 'loaded 3 model profile(s)', got %v", cap.snapshot())
	}
	if !cap.containsLine("INFO", dir) {
		t.Errorf("expected INFO to reference profile dir %s, got %v", dir, cap.snapshot())
	}
}

// TestLoad_LogsZeroOnNonexistentDir asserts we still emit a count line for
// the missing-directory path so daemon.log always shows which directory was
// checked at startup, even when the operator never ran `oat model onboard`.
func TestLoad_LogsZeroOnNonexistentDir(t *testing.T) {
	cap := &captureLogger{}
	ps, err := NewProfileStoreWithLogger("/tmp/definitely-does-not-exist-routing-diag-test", cap)
	if err != nil {
		t.Fatalf("NewProfileStoreWithLogger: %v", err)
	}
	if ps.Count() != 0 {
		t.Errorf("Count = %d, want 0", ps.Count())
	}
	if !cap.containsLine("INFO", "loaded 0 model profile(s)") {
		t.Errorf("expected INFO for zero-count load, got %v", cap.snapshot())
	}
}

// TestProfileStore_IsEmpty_True_NonexistentDir verifies IsEmpty reports true
// for the missing-directory case.
func TestProfileStore_IsEmpty_True_NonexistentDir(t *testing.T) {
	ps, err := NewProfileStore("/tmp/definitely-does-not-exist-routing-isempty")
	if err != nil {
		t.Fatalf("NewProfileStore: %v", err)
	}
	if !ps.IsEmpty() {
		t.Error("IsEmpty() = false, want true for nonexistent dir")
	}
}

// TestProfileStore_IsEmpty_True_EmptyDir verifies IsEmpty reports true for a
// present-but-empty directory.
func TestProfileStore_IsEmpty_True_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	ps, err := NewProfileStore(dir)
	if err != nil {
		t.Fatalf("NewProfileStore: %v", err)
	}
	if !ps.IsEmpty() {
		t.Error("IsEmpty() = false, want true for empty dir")
	}
}

// TestProfileStore_IsEmpty_False verifies IsEmpty reports false when the
// store has at least one loaded profile.
func TestProfileStore_IsEmpty_False(t *testing.T) {
	dir := setupTestProfiles(t)
	ps, err := NewProfileStore(dir)
	if err != nil {
		t.Fatalf("NewProfileStore: %v", err)
	}
	if ps.IsEmpty() {
		t.Error("IsEmpty() = true, want false for populated dir")
	}
}

// TestNewProfileStore_NoLoggerStaysSilent sanity-checks that the zero-arg
// constructor still works and produces no observable side effects even when
// it would otherwise log — important because several existing tests rely on
// NewProfileStore not flooding stdout during go test runs.
func TestNewProfileStore_NoLoggerStaysSilent(t *testing.T) {
	dir := setupTestProfiles(t)
	ps, err := NewProfileStore(dir) // no logger
	if err != nil {
		t.Fatalf("NewProfileStore: %v", err)
	}
	if ps.Count() != 3 {
		t.Errorf("Count = %d, want 3", ps.Count())
	}
	// There's no direct observable to assert "silent" on, but the test
	// exists to lock in the wrapper-calls-noop-logger contract so a future
	// refactor doesn't accidentally re-introduce log.Printf spam.
}

// TestValidateRequiredFields directly exercises the helper so the branch
// matrix stays covered even if load() changes shape later.
func TestValidateRequiredFields(t *testing.T) {
	tests := []struct {
		name string
		p    *ModelProfile
		want []string
	}{
		{
			name: "all present via score",
			p:    &ModelProfile{Provider: "test", AutonomyTier: "full", OverallScore: 50},
		},
		{
			name: "all present via onboarding_passed",
			p:    &ModelProfile{Provider: "test", AutonomyTier: "restricted", OnboardingPassed: true},
		},
		{
			name: "missing provider only",
			p:    &ModelProfile{AutonomyTier: "full", OverallScore: 80},
			want: []string{"provider.name"},
		},
		{
			name: "missing tier only",
			p:    &ModelProfile{Provider: "test", OverallScore: 80},
			want: []string{"routing.autonomy_tier"},
		},
		{
			name: "missing score and passed",
			p:    &ModelProfile{Provider: "test", AutonomyTier: "full"},
			want: []string{"routing.overall_score or contract.onboarding_passed"},
		},
		{
			name: "all three missing",
			p:    &ModelProfile{},
			want: []string{"provider.name", "routing.autonomy_tier", "routing.overall_score or contract.onboarding_passed"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := validateRequiredFields(tc.p)
			if len(got) != len(tc.want) {
				t.Fatalf("validateRequiredFields = %v, want %v", got, tc.want)
			}
			for i, field := range tc.want {
				if got[i] != field {
					t.Errorf("validateRequiredFields[%d] = %q, want %q", i, got[i], field)
				}
			}
		})
	}
}
