package routing

import (
	"strings"
	"testing"
)

func TestExtractFeatures_Trivial(t *testing.T) {
	f := ExtractFeatures("Fix the typo 'recieve' to 'receive' on line 42.")
	if f.Complexity != ComplexityTrivial {
		t.Errorf("want trivial, got %s", f.Complexity)
	}
	if !f.IsTrivial {
		t.Error("IsTrivial should be true for a typo fix")
	}
}

func TestExtractFeatures_Complex(t *testing.T) {
	f := ExtractFeatures(`Refactor the auth subsystem to support OAuth2, touching models/, controllers/, middleware/, and migrations.`)
	if f.Complexity != ComplexityComplex {
		t.Errorf("want complex, got %s", f.Complexity)
	}
	if !f.IsRefactor {
		t.Error("IsRefactor should be true")
	}
}

func TestExtractFeatures_Simple_ShortInstruction(t *testing.T) {
	f := ExtractFeatures("Add a --category filter to the list command.")
	if f.Complexity != ComplexitySimple {
		t.Errorf("want simple, got %s", f.Complexity)
	}
}

func TestExtractFeatures_Standard_Default(t *testing.T) {
	f := ExtractFeatures("Implement the new payment flow described in specs/payment.md so that the endpoint returns 200 on successful charge. Add tests covering idempotency and retries. Update the OpenAPI spec if needed.")
	if f.Complexity != ComplexityStandard {
		t.Errorf("want standard, got %s", f.Complexity)
	}
}

func TestExtractFeatures_DocClassifiesAsSimple(t *testing.T) {
	f := ExtractFeatures("Update the README.md with install instructions.")
	if f.Complexity != ComplexitySimple {
		t.Errorf("want simple, got %s", f.Complexity)
	}
	if !f.IsDocOrConfig {
		t.Error("IsDocOrConfig should be true for README task")
	}
}

func TestRouteForTask_TrivialPicksCheap(t *testing.T) {
	ps := minimalStoreForTests(t)
	pricing := LoadEmbeddedPricing()
	d, err := ps.RouteForTask(RouteContext{
		TaskText: "Fix typo 'recieve' on line 42",
		Role:     RoleWorker,
	}, pricing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Complexity != ComplexityTrivial {
		t.Errorf("want trivial, got %s", d.Complexity)
	}
	// Trivial floor is 2 — should NOT pick sonnet (tier 6) if a cheaper
	// eligible model exists.
	if d.ChosenModel == "anthropic:claude-sonnet-4-6" {
		t.Errorf("trivial task chose sonnet; expected a cheaper floor-passing model. Got reason: %s", d.Reason)
	}
}

func TestRouteForTask_ComplexPicksSonnetOrHaiku(t *testing.T) {
	ps := minimalStoreForTests(t)
	pricing := LoadEmbeddedPricing()
	d, err := ps.RouteForTask(RouteContext{
		TaskText: "Refactor the auth subsystem across models/, controllers/, middleware/, and migrations/. Preserve every existing test.",
		Role:     RoleWorker,
	}, pricing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Complexity != ComplexityComplex {
		t.Errorf("want complex, got %s", d.Complexity)
	}
	// Complex floor is 5 — expect haiku (tier 5) or sonnet (tier 6).
	if d.ChosenModel != "anthropic:claude-haiku-4-5" && d.ChosenModel != "anthropic:claude-sonnet-4-6" {
		t.Errorf("complex task chose %s — expected haiku or sonnet", d.ChosenModel)
	}
}

func TestRouteForTask_OperatorExplicitRespected(t *testing.T) {
	ps := minimalStoreForTests(t)
	pricing := LoadEmbeddedPricing()
	d, err := ps.RouteForTask(RouteContext{
		TaskText:       "Fix typo",
		Role:           RoleWorker,
		PreferredModel: "anthropic:claude-sonnet-4-6",
	}, pricing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.ChosenModel != "anthropic:claude-sonnet-4-6" {
		t.Errorf("operator-explicit ignored: got %s", d.ChosenModel)
	}
	if d.RoutingSource != "operator-explicit" {
		t.Errorf("routing_source should be operator-explicit, got %s", d.RoutingSource)
	}
}

func TestRouteForTask_RespectsAllowlist(t *testing.T) {
	ps := minimalStoreForTests(t)
	pricing := LoadEmbeddedPricing()
	// Allowlist restricts to haiku only. Trivial task should still pick haiku,
	// not a cheaper model that isn't allowed.
	d, err := ps.RouteForTask(RouteContext{
		TaskText:      "Fix typo 'recieve' on line 42",
		Role:          RoleWorker,
		AllowedModels: []string{"anthropic:claude-haiku-4-5"},
	}, pricing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.ChosenModel != "anthropic:claude-haiku-4-5" {
		t.Errorf("allowlist violated: chose %s when only haiku was allowed", d.ChosenModel)
	}
	// Candidate list must be from the allowlist only.
	for _, c := range d.Candidates {
		if c != "anthropic:claude-haiku-4-5" {
			t.Errorf("candidates include out-of-allowlist model: %s", c)
		}
	}
}

func TestRouteForTask_NoEligibleModelsReturnsError(t *testing.T) {
	ps := NewProfileStoreEmpty() // empty store; see helper below
	pricing := LoadEmbeddedPricing()
	_, err := ps.RouteForTask(RouteContext{
		TaskText: "anything",
		Role:     RoleWorker,
	}, pricing)
	if err == nil {
		t.Error("expected error on empty store")
	}
	if !strings.Contains(err.Error(), "no eligible models") {
		t.Errorf("error doesn't mention eligibility: %v", err)
	}
}

// ── test helpers ─────────────────────────────────────────────────────────────

// minimalStoreForTests builds a ProfileStore with 4 representative models
// covering the complexity tiers: nano (tier 2), flash (tier 3), haiku (tier 5),
// sonnet (tier 6). Uses the embedded context-registry for windows and the
// embedded pricing for costs.
func minimalStoreForTests(t *testing.T) *ProfileStore {
	t.Helper()
	dir := t.TempDir()
	for name, yaml := range map[string]string{
		"openai__gpt-5.4-nano.yaml": `model_id: "openai:gpt-5.4-nano"
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
  effective_context_class: medium
  max_input_tokens: 400000
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
`,
		"google_genai__gemini-2.5-flash.yaml": `model_id: "google_genai:gemini-2.5-flash"
status: known
provider:
  name: google_genai
capabilities:
  tool_reliability: 1.0
  shell_roundtrip: 1.0
  shell_recovery: 0.7
  file_write_reliability: 1.0
  token_reporting: 0.95
  streaming: 1.0
  multi_turn: 1.0
  large_output: 0.9
  effective_context_class: large
  max_input_tokens: 1048576
  reasoning_controls: "none"
routing:
  autonomy_tier: standard
  overall_score: 99
contract:
  onboarding_passed: true
  worker_eligible: true
  orchestrator_eligible: false
evidence:
  probe_set: default
  probes_run: 13
  probes_passed: 13
`,
		"anthropic__claude-haiku-4-5.yaml": `model_id: "anthropic:claude-haiku-4-5"
status: known
provider:
  name: anthropic
capabilities:
  tool_reliability: 1.0
  shell_roundtrip: 1.0
  shell_recovery: 0.88
  file_write_reliability: 1.0
  token_reporting: 0.95
  streaming: 1.0
  multi_turn: 1.0
  large_output: 0.9
  effective_context_class: medium
  max_input_tokens: 200000
  reasoning_controls: "thinking_budget_5000"
routing:
  autonomy_tier: full
  overall_score: 96
contract:
  onboarding_passed: true
  worker_eligible: true
  orchestrator_eligible: true
evidence:
  probe_set: default
  probes_run: 13
  probes_passed: 13
`,
		"anthropic__claude-sonnet-4-6.yaml": `model_id: "anthropic:claude-sonnet-4-6"
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
  effective_context_class: large
  max_input_tokens: 1000000
  reasoning_controls: "thinking_budget_5000"
routing:
  autonomy_tier: full
  overall_score: 91
contract:
  onboarding_passed: true
  worker_eligible: true
  orchestrator_eligible: true
evidence:
  probe_set: default
  probes_run: 13
  probes_passed: 13
`,
	} {
		if err := writeFile(dir+"/"+name, yaml); err != nil {
			t.Fatal(err)
		}
	}

	ps, err := NewProfileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

// NewProfileStoreEmpty returns a ProfileStore with no profiles, for error-path
// tests. Exported so other tests in this package can use it.
func NewProfileStoreEmpty() *ProfileStore {
	return &ProfileStore{profiles: map[string]*ModelProfile{}}
}
