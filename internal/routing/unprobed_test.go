package routing

import (
	"testing"
)

func TestParseProfile_NullCapabilitiesTrackedAsUnprobed(t *testing.T) {
	yaml := `# OAT Model Capability Profile
model_id: "anthropic:claude-haiku-4-5"
status: known

provider:
  name: anthropic

capabilities:
  tool_reliability: 1.0
  shell_roundtrip: 1.0
  shell_recovery: null
  file_write_reliability: 1.0
  token_reporting: 0.95
  streaming: null
  multi_turn: null
  large_output: null

routing:
  autonomy_tier: full
  overall_score: 96

contract:
  onboarding_passed: true
  worker_eligible: true
  orchestrator_eligible: false

evidence:
  probe_version: 2
  probe_set: minimum
  probes_run: 6
  probes_passed: 6
`
	p := parseProfile(yaml)
	if p == nil {
		t.Fatal("parse returned nil")
	}
	if p.UnprobedCapabilities == nil {
		t.Fatal("UnprobedCapabilities not populated")
	}
	for _, cap := range []string{"shell_recovery", "streaming", "multi_turn", "large_output"} {
		if !p.UnprobedCapabilities[cap] {
			t.Errorf("%s should be marked unprobed", cap)
		}
		if p.IsProbed(cap) {
			t.Errorf("%s.IsProbed() should be false", cap)
		}
	}
	// Probed ones should be absent from the unprobed map.
	for _, cap := range []string{"tool_reliability", "shell_roundtrip", "file_write_reliability", "token_reporting"} {
		if p.UnprobedCapabilities[cap] {
			t.Errorf("%s should not be marked unprobed", cap)
		}
		if !p.IsProbed(cap) {
			t.Errorf("%s.IsProbed() should be true", cap)
		}
	}
	// Float fields of unprobed capabilities should be their zero value (0.0)
	// — the honest signal, distinct from the pre-fix 1.0 lie.
	if p.ShellRecovery != 0 {
		t.Errorf("ShellRecovery float should be 0 when null, got %v", p.ShellRecovery)
	}
	if p.Streaming != 0 {
		t.Errorf("Streaming float should be 0 when null, got %v", p.Streaming)
	}
}

func TestParseProfile_NumericCapabilitiesNotUnprobed(t *testing.T) {
	yaml := `model_id: "anthropic:claude-sonnet-4-6"
status: known
provider:
  name: anthropic
capabilities:
  tool_reliability: 1.0
  shell_roundtrip: 0.65
  shell_recovery: 0.77
  file_write_reliability: 1.0
  token_reporting: 0.95
  streaming: 0.75
  multi_turn: 1.0
  large_output: 0.9
routing:
  autonomy_tier: full
  overall_score: 91
contract:
  onboarding_passed: true
  worker_eligible: true
  orchestrator_eligible: true
`
	p := parseProfile(yaml)
	if p == nil {
		t.Fatal("parse returned nil")
	}
	if len(p.UnprobedCapabilities) != 0 {
		t.Errorf("fully-probed profile should have empty UnprobedCapabilities, got %v", p.UnprobedCapabilities)
	}
	if p.ShellRecovery != 0.77 {
		t.Errorf("ShellRecovery should parse 0.77, got %v", p.ShellRecovery)
	}
}

func TestIsProbed_NilSafeForLegacyProfiles(t *testing.T) {
	// Legacy profile loaded from a pre-null YAML: UnprobedCapabilities stays nil.
	// IsProbed must not panic and should return true (optimistic for legacy).
	p := &ModelProfile{}
	if !p.IsProbed("shell_recovery") {
		t.Error("legacy profile should default to IsProbed=true")
	}
	var nilProfile *ModelProfile
	if !nilProfile.IsProbed("shell_recovery") {
		t.Error("nil profile should default to IsProbed=true (no panic)")
	}
}
