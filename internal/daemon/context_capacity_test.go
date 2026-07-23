// Tests for the Part 5e context-capacity safety net. Focused on
// the pure-Go decision logic + a couple of integration-shape tests
// that pin the agent-type gate and env-var override behavior.
// What we pin:
//
//   - computeCapacityPct: handles overshoot, zero/negative limit,
//     normal range. No NaN, no panics.
//   - safetyNetEnabled: every documented enable/disable token +
//     the fail-safe-to-ON fallback for garbage values.
//   - effectiveContextLimit: profile → MaxInputTokens, profile
//     above ceiling → 200K, no profile → 128K fallback + WARN-once
//     dedupe.
//   - maybeNudgeContextCapacity: non-assistant types ignored;
//     in-memory dedupe suppresses repeats inside the window;
//     re-fires after the window.
//   - shouldInjectContextSafetyNet: non-assistant ignored, OFF
//     env var → no inject, < 95% → no inject, >= 95% → inject
//     with directive payload.

package daemon

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/logging"
	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

func TestAgentContextOccupancy(t *testing.T) {
	cases := []struct {
		name      string
		agent     state.Agent
		want      int64
		wantKnown bool
	}{
		{
			name:      "window known is used and reported known",
			agent:     state.Agent{ContextWindowTokens: 42_000, TotalTokens: 900_000},
			want:      42_000,
			wantKnown: true,
		},
		{
			name: "window unknown does NOT fall back to cumulative",
			// Cumulative TotalTokens over-counts wildly; the meter +
			// hint/safety-net must treat occupancy as unknown rather
			// than guess from it.
			agent:     state.Agent{ContextWindowTokens: 0, TotalTokens: 51_200},
			want:      0,
			wantKnown: false,
		},
		{
			name:      "both zero is unknown",
			agent:     state.Agent{},
			want:      0,
			wantKnown: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, known := agentContextOccupancy(tc.agent)
			if got != tc.want || known != tc.wantKnown {
				t.Errorf("agentContextOccupancy = (%d, %v), want (%d, %v)", got, known, tc.want, tc.wantKnown)
			}
		})
	}
}

func TestComputeCapacityPct_Part5e(t *testing.T) {
	cases := []struct {
		name  string
		total int64
		limit int64
		want  float64
	}{
		{"empty over default 128K", 0, 128_000, 0},
		{"half over 128K", 64_000, 128_000, 0.5},
		{"75 % tier exact", 96_000, 128_000, 0.75},
		{"95 % tier exact", 121_600, 128_000, 0.95},
		{"100 % exact returns 1.0", 128_000, 128_000, 1.0},
		{"overshoot caps at 1.0", 200_000, 128_000, 1.0},
		{"zero limit returns 0 (unknown)", 50_000, 0, 0},
		{"negative limit returns 0 (defensive)", 50_000, -1, 0},
		{"negative total returns 0 (defensive)", -100, 128_000, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeCapacityPct(tc.total, tc.limit)
			if got != tc.want {
				t.Errorf("computeCapacityPct(%d, %d) = %v, want %v", tc.total, tc.limit, got, tc.want)
			}
		})
	}
}

func TestSafetyNetEnabled_Part5e(t *testing.T) {
	cases := []struct {
		envVal string
		want   bool
	}{
		// Documented enable tokens.
		{"", true},
		{"1", true},
		{"true", true},
		{"True", true},
		{"TRUE", true},
		{"yes", true},
		{"on", true},
		// Documented disable tokens.
		{"0", false},
		{"false", false},
		{"FALSE", false},
		{"no", false},
		{"off", false},
		// Whitespace tolerated around either side.
		{"  1  ", true},
		{"  0  ", false},
		// Garbage values fail-safe to enabled (we'd rather hint
		// users than silently let them crash-loop).
		{"maybe", true},
		{"???", true},
		{"OAT_CONTEXT_SAFETY_NET=1", true}, // accidental nested assignment
	}
	for _, tc := range cases {
		t.Run(tc.envVal, func(t *testing.T) {
			t.Setenv(safetyNetEnvVar, tc.envVal)
			if got := safetyNetEnabled(); got != tc.want {
				t.Errorf("safetyNetEnabled() with %q = %v, want %v", tc.envVal, got, tc.want)
			}
		})
	}
}

func TestEffectiveContextLimit_Fallback_Part5e(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()

	// No profile loaded for "unknown:model" → fallback path.
	limit, source := d.effectiveContextLimit("unknown:model", "repo", "agent")
	if limit != contextFallbackTokens {
		t.Errorf("limit = %d, want fallback %d", limit, contextFallbackTokens)
	}
	if source != "fallback" {
		t.Errorf("source = %q, want %q", source, "fallback")
	}

	// Second call with same (repo, agent) → still fallback, but
	// WARN dedupe should have suppressed the second log. We can't
	// easily assert on logger output without plumbing, so just
	// verify the second call doesn't panic and returns the same
	// value (regression guard against the dedupe map breaking
	// fallback selection).
	limit2, source2 := d.effectiveContextLimit("unknown:model", "repo", "agent")
	if limit2 != limit || source2 != source {
		t.Errorf("second call returned (%d, %q), want (%d, %q)", limit2, source2, limit, source)
	}
}

func TestEffectiveContextLimit_EmptyModelID_Part5e(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()

	// Empty modelID skips the profile lookup → fallback path.
	// Pins the defensive nil/empty handling so a future refactor
	// that drops the `modelID != ""` guard doesn't accidentally
	// pass an empty string to ProfileStore.Get and crash.
	limit, source := d.effectiveContextLimit("", "repo", "agent")
	if limit != contextFallbackTokens {
		t.Errorf("limit = %d, want fallback %d", limit, contextFallbackTokens)
	}
	if source != "fallback" {
		t.Errorf("source = %q, want %q", source, "fallback")
	}
}

func TestMaybeNudgeContextCapacity_NonAssistant_Part5e(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()

	// Browser, worker, supervisor: all must be no-ops. The hint
	// would be wasted (compact_conversation is denied for browser;
	// other types don't have side-panel chat). Pin this explicitly
	// so a future refactor that extends the gate doesn't silently
	// start emitting hints to worker PTYs.
	cases := []state.AgentType{
		state.AgentTypeBrowser,
		state.AgentTypeWorker,
		state.AgentTypeSupervisor,
		state.AgentTypeMergeQueue,
		state.AgentTypePRShepherd,
		state.AgentTypeWorkspace,
	}
	for _, typ := range cases {
		t.Run(string(typ), func(t *testing.T) {
			// Pre-state: empty dedupe map.
			d.contextCap.mu.Lock()
			before := len(d.contextCap.lastHintAt)
			d.contextCap.mu.Unlock()

			d.maybeNudgeContextCapacity("repo", "agent", state.Agent{
				Type:        typ,
				TotalTokens: 128_000, // would be 100% if it ran
				Model:       "anthropic:claude-opus-4-7",
			})

			// Post-state: still empty -- the dedupe map should not
			// have been touched for a non-assistant.
			d.contextCap.mu.Lock()
			after := len(d.contextCap.lastHintAt)
			d.contextCap.mu.Unlock()
			if after != before {
				t.Errorf("non-assistant %q triggered dedupe write: before=%d after=%d", typ, before, after)
			}
		})
	}
}

func TestMaybeNudgeContextCapacity_Suppression_Part5e(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()

	// Seed a fake repo so backend.SendMessage's session lookup
	// doesn't trip. We don't assert on the message itself
	// (backend is a no-op in setupTestDaemon's mode); the test is
	// purely about the dedupe-map state transitions.
	if err := d.state.AddRepo("repo", &state.Repository{SessionName: "repo"}); err != nil {
		t.Fatalf("AddRepo: %v", err)
	}

	assistant := state.Agent{
		Type:                state.AgentTypeAssistant,
		WindowName:          "personal",
		ContextWindowTokens: 96_000, // above the 75 % hint tier (fallback budget)
		Model:               "",     // forces fallback source
	}

	// First call: should record a lastHintAt entry.
	d.maybeNudgeContextCapacity("repo", "personal", assistant)
	d.contextCap.mu.Lock()
	last1, ok1 := d.contextCap.lastHintAt[agentKey("repo", "personal")]
	d.contextCap.mu.Unlock()
	if !ok1 {
		t.Fatal("first hint did not record dedupe entry")
	}

	// Second call within the suppression window: must be a no-op
	// (the timestamp must NOT advance).
	time.Sleep(2 * time.Millisecond) // make any "is the time advancing?" bug visible
	d.maybeNudgeContextCapacity("repo", "personal", assistant)
	d.contextCap.mu.Lock()
	last2 := d.contextCap.lastHintAt[agentKey("repo", "personal")]
	d.contextCap.mu.Unlock()
	if !last2.Equal(last1) {
		t.Errorf("second call inside suppression window advanced timestamp: %v → %v", last1, last2)
	}

	// Backdate the dedupe entry to outside the suppression
	// window. Third call should re-fire (and advance the
	// timestamp).
	d.contextCap.mu.Lock()
	d.contextCap.lastHintAt[agentKey("repo", "personal")] = time.Now().Add(-2 * contextHintSuppressionWindow)
	d.contextCap.mu.Unlock()

	d.maybeNudgeContextCapacity("repo", "personal", assistant)
	d.contextCap.mu.Lock()
	last3 := d.contextCap.lastHintAt[agentKey("repo", "personal")]
	d.contextCap.mu.Unlock()
	if !last3.After(last2) {
		t.Errorf("third call after suppression window did not advance timestamp: %v -> %v", last2, last3)
	}
}

func TestMaybeNudgeContextCapacity_BelowTier_Part5e(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()

	// Isolate the tier-threshold logic from the Phase 1 output-headroom
	// reservation: with reservation ON the tier denominator would be the
	// window minus reserved output, which is exercised separately by
	// TestEffectiveContextBudget_ReservesOutput. Here we assert the raw
	// 75 % tier boundary against the full window.
	t.Setenv(outputReservationEnvVar, "0")

	if err := d.state.AddRepo("repo", &state.Repository{SessionName: "repo"}); err != nil {
		t.Fatalf("AddRepo: %v", err)
	}

	// 74 % of the 128 K fallback ≈ 94 720 tokens. Below tier → no
	// hint, no dedupe entry recorded.
	d.maybeNudgeContextCapacity("repo", "personal", state.Agent{
		Type:                state.AgentTypeAssistant,
		WindowName:          "personal",
		ContextWindowTokens: int64(0.74 * float64(contextFallbackTokens)),
		Model:               "",
	})
	d.contextCap.mu.Lock()
	_, ok := d.contextCap.lastHintAt[agentKey("repo", "personal")]
	d.contextCap.mu.Unlock()
	if ok {
		t.Error("hint fired below 75 % tier")
	}
}

func TestShouldInjectContextSafetyNet_Part5e(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()

	// Isolate the 95 % tier-threshold logic from the Phase 1 output-headroom
	// reservation (tested separately). Assert against the full window.
	t.Setenv(outputReservationEnvVar, "0")

	// 95 % of 128 K fallback = 121 600. Use 122 000 to clearly cross.
	hot := state.Agent{
		Type:                state.AgentTypeAssistant,
		ContextWindowTokens: 122_000,
	}
	// 90 % of 128 K fallback = 115 200. Below tier → no inject.
	warm := state.Agent{
		Type:                state.AgentTypeAssistant,
		ContextWindowTokens: 115_000,
	}
	browser := state.Agent{
		Type:                state.AgentTypeBrowser,
		ContextWindowTokens: 122_000,
	}

	t.Run("assistant at 95% with safety net ON → inject", func(t *testing.T) {
		t.Setenv(safetyNetEnvVar, "1")
		directive, inject := d.shouldInjectContextSafetyNet(hot, "repo", "agent")
		if !inject {
			t.Fatal("expected inject, got false")
		}
		if directive == "" {
			t.Error("inject true but directive is empty")
		}
		// Pin the wire format: the agent's prompt teaches it to
		// recognize `[OAT-system]` and `compact_conversation` as
		// the directive shape; if either token moves we should
		// know.
		if !contains(directive, "[OAT-system]") {
			t.Errorf("directive missing [OAT-system] sentinel: %q", directive)
		}
		if !contains(directive, "compact_conversation") {
			t.Errorf("directive missing compact_conversation token: %q", directive)
		}
	})

	t.Run("assistant at 95% with safety net OFF → no inject", func(t *testing.T) {
		t.Setenv(safetyNetEnvVar, "0")
		_, inject := d.shouldInjectContextSafetyNet(hot, "repo", "agent")
		if inject {
			t.Error("safety net OFF but inject returned true")
		}
	})

	t.Run("assistant below 95% → no inject regardless of env", func(t *testing.T) {
		t.Setenv(safetyNetEnvVar, "1")
		_, inject := d.shouldInjectContextSafetyNet(warm, "repo", "agent")
		if inject {
			t.Error("inject returned true below 95 % tier")
		}
	})

	t.Run("non-assistant type → no inject regardless of capacity", func(t *testing.T) {
		t.Setenv(safetyNetEnvVar, "1")
		_, inject := d.shouldInjectContextSafetyNet(browser, "repo", "agent")
		if inject {
			t.Error("browser agent triggered safety-net inject (should be assistant-only)")
		}
	})
}

// TestShouldInjectContextSafetyNet_UnknownOccupancy pins the
// 2026-06-04 behavior change: when an assistant has no live
// context-window reading yet (ContextWindowTokens == 0), the safety
// net must NOT fire even if cumulative TotalTokens is enormous. The
// old code fell back to TotalTokens here, which could force a spurious
// compaction on the first message after a restart/wake.
func TestShouldInjectContextSafetyNet_UnknownOccupancy(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()
	t.Setenv(safetyNetEnvVar, "1")

	unknown := state.Agent{
		Type:                state.AgentTypeAssistant,
		ContextWindowTokens: 0,         // no live reading yet
		TotalTokens:         9_999_999, // inflated cumulative — must be ignored
	}
	if _, inject := d.shouldInjectContextSafetyNet(unknown, "repo", "agent"); inject {
		t.Error("safety net fired on unknown occupancy (should suppress, not fall back to cumulative)")
	}
}

// (Substring `contains` helper is shared from daemon_test.go.)

// ---------------------------------------------------------------------
// Context-overflow protections: the env-override layer.
//
// Verifies the precedence path of effectiveContextLimit:
//
//   1. OAT_MODEL_CONTEXT_<normalized-modelID> env override wins.
//   2. ModelProfile wins when env unset.
//   3. 128K fallback fires when both miss.
//
// Plus the supporting machinery: env-var name normalization,
// out-of-range clamping with WARN, and the load-bearing WARN
// content (the literal `oat model onboard <modelID>` copy-paste
// recovery string).
// ---------------------------------------------------------------------

const testProfileGeminiFlash = `model_id: "google_genai:gemini-2.5-flash"
status: known
provider:
  name: google_genai
capabilities:
  tool_reliability: 1.0
  shell_reliability: 1.0
  shell_recovery: 1.0
  file_write_reliability: 1.0
  multi_turn: 1.0
routing:
  autonomy_tier: full
  overall_score: 90
max_input_tokens: 1000000
contract:
  onboarding_passed: true
  worker_eligible: true
  orchestrator_eligible: true
`

// TestNormalizeModelIDForEnv pins the env-var name shape for the
// real-world ID styles operators are likely to type. If anyone ever
// adds aggressive sanitization (e.g. mapping `-` or `.` to `_`),
// silent collisions become possible (`gemini-2.5` and `gemini.2.5`
// both mapping to the same env var) -- this test breaks first so a
// reviewer can think about whether that's actually desired.
func TestNormalizeModelIDForEnv(t *testing.T) {
	cases := []struct {
		modelID string
		want    string
	}{
		{"anthropic:claude-opus-4-7", "anthropic_claude-opus-4-7"},
		{"google_genai:gemini-2.5-flash", "google_genai_gemini-2.5-flash"},
		{"openai/gpt-5-mini", "openai_gpt-5-mini"},
		{"OPENAI:GPT-5", "openai_gpt-5"},
		{"ollama:qwen3-coder:32b", "ollama_qwen3-coder_32b"},
		{"local-ollama/llama3.2:3b-instruct", "local-ollama_llama3.2_3b-instruct"},
		{"plainmodelname", "plainmodelname"},
	}
	for _, tc := range cases {
		t.Run(tc.modelID, func(t *testing.T) {
			if got := normalizeModelIDForEnv(tc.modelID); got != tc.want {
				t.Errorf("normalizeModelIDForEnv(%q) = %q, want %q", tc.modelID, got, tc.want)
			}
		})
	}
}

// TestContextEnvOverride_ParseValid pins that valid integer values
// pass through unchanged and the helper reports `clamped=false`. The
// caller distinguishes "operator set a sane value" from "clamped"
// by the second return slot's truthiness combined with the value, so
// it must remain false for in-range inputs.
func TestContextEnvOverride_ParseValid(t *testing.T) {
	t.Setenv("OAT_MODEL_CONTEXT_anthropic_claude-opus-4-7", "200000")
	got, ok := contextEnvOverride("anthropic:claude-opus-4-7", nil)
	if !ok {
		t.Fatal("contextEnvOverride returned ok=false for valid integer")
	}
	if got != 200_000 {
		t.Errorf("got = %d, want 200000", got)
	}
}

// TestContextEnvOverride_ClampBelowMin verifies sub-1K values clamp
// to contextEnvOverrideMin AND emit a WARN. The WARN content matters
// because operators reading the log need to see the actual env var
// name + bad value so they know what to fix.
func TestContextEnvOverride_ClampBelowMin(t *testing.T) {
	t.Setenv("OAT_MODEL_CONTEXT_anthropic_claude-opus-4-7", "5")
	var captured []string
	sink := func(format string, args ...any) {
		captured = append(captured, fmt.Sprintf(format, args...))
	}
	got, ok := contextEnvOverride("anthropic:claude-opus-4-7", sink)
	if !ok {
		t.Fatal("contextEnvOverride returned ok=false for too-small (should clamp + ok)")
	}
	if got != contextEnvOverrideMin {
		t.Errorf("got = %d, want clamped %d", got, contextEnvOverrideMin)
	}
	if len(captured) != 1 {
		t.Fatalf("expected 1 WARN, got %d: %v", len(captured), captured)
	}
	if !contains(captured[0], "below minimum") {
		t.Errorf("WARN missing 'below minimum': %q", captured[0])
	}
	if !contains(captured[0], "OAT_MODEL_CONTEXT_anthropic_claude-opus-4-7") {
		t.Errorf("WARN missing env-var name: %q", captured[0])
	}
}

// TestContextEnvOverride_ClampAboveMax pins the same shape but on
// the high side -- including the "billions" scenario from the plan
// where someone accidentally exports 2_000_000_000. The agent never
// sees the bad value.
func TestContextEnvOverride_ClampAboveMax(t *testing.T) {
	t.Setenv("OAT_MODEL_CONTEXT_openai_gpt-5", "2000000000")
	var captured []string
	sink := func(format string, args ...any) {
		captured = append(captured, fmt.Sprintf(format, args...))
	}
	got, ok := contextEnvOverride("openai:gpt-5", sink)
	if !ok {
		t.Fatal("contextEnvOverride returned ok=false for too-large (should clamp + ok)")
	}
	if got != contextEnvOverrideMax {
		t.Errorf("got = %d, want clamped %d", got, contextEnvOverrideMax)
	}
	if len(captured) != 1 {
		t.Fatalf("expected 1 WARN, got %d: %v", len(captured), captured)
	}
	if !contains(captured[0], "above maximum") {
		t.Errorf("WARN missing 'above maximum': %q", captured[0])
	}
}

// TestContextEnvOverride_NonNumeric pins that a garbage value
// (typo, accidental shell expansion, etc.) does NOT clamp to a
// surprise value -- the helper returns (0, false) so the caller
// can fall through to profile / fallback like the env var wasn't
// set at all. WARN explains why so the operator can fix the typo.
func TestContextEnvOverride_NonNumeric(t *testing.T) {
	t.Setenv("OAT_MODEL_CONTEXT_openai_gpt-5", "lots")
	var captured []string
	sink := func(format string, args ...any) {
		captured = append(captured, fmt.Sprintf(format, args...))
	}
	got, ok := contextEnvOverride("openai:gpt-5", sink)
	if ok {
		t.Errorf("contextEnvOverride returned ok=true for non-numeric (got %d); should fall through", got)
	}
	if got != 0 {
		t.Errorf("got = %d, want 0 (fall-through)", got)
	}
	if len(captured) != 1 {
		t.Fatalf("expected 1 WARN, got %d: %v", len(captured), captured)
	}
	if !contains(captured[0], "not a valid integer") {
		t.Errorf("WARN missing 'not a valid integer': %q", captured[0])
	}
}

// TestContextEnvOverride_Unset pins that the env helper is silent
// when the var isn't set -- no WARN, no log spam.
func TestContextEnvOverride_Unset(t *testing.T) {
	// Clear out anything a sibling test may have left set.
	t.Setenv("OAT_MODEL_CONTEXT_unset_test", "")
	var captured []string
	sink := func(format string, args ...any) {
		captured = append(captured, fmt.Sprintf(format, args...))
	}
	got, ok := contextEnvOverride("unset:test", sink)
	if ok || got != 0 {
		t.Errorf("got (%d, %v), want (0, false) for unset env var", got, ok)
	}
	if len(captured) != 0 {
		t.Errorf("expected 0 WARN, got %d: %v", len(captured), captured)
	}
}

// TestEffectiveContextLimit_EnvWinsOverProfile is the precedence
// test the plan body calls out (B.0 test (a)): an env override
// must beat a present-and-valid ModelProfile so an operator can
// hot-fix a wrong profile without re-running `oat model onboard`.
func TestEffectiveContextLimit_EnvWinsOverProfile(t *testing.T) {
	d, cleanup := setupDaemonWithProfiles(t, map[string]string{
		"gemini-flash.yaml": testProfileGeminiFlash, // profile says 1M
	})
	defer cleanup()

	// Env override pins it to 300K instead.
	t.Setenv("OAT_MODEL_CONTEXT_google_genai_gemini-2.5-flash", "300000")

	limit, source := d.effectiveContextLimit(
		"google_genai:gemini-2.5-flash", "repo", "agent",
	)
	if source != "env" {
		t.Errorf("source = %q, want %q (env override should beat profile)", source, "env")
	}
	if limit != 300_000 {
		t.Errorf("limit = %d, want 300000 (env override value)", limit)
	}
}

// TestEffectiveContextLimit_ProfileWinsWhenEnvUnset is B.0 test
// (b): if the operator hasn't set an env override, the loaded
// profile must take over -- not silently fall back to 128K.
func TestEffectiveContextLimit_ProfileWinsWhenEnvUnset(t *testing.T) {
	d, cleanup := setupDaemonWithProfiles(t, map[string]string{
		"gemini-flash.yaml": testProfileGeminiFlash,
	})
	defer cleanup()
	// Ensure no env override is set for this model.
	t.Setenv("OAT_MODEL_CONTEXT_google_genai_gemini-2.5-flash", "")

	limit, source := d.effectiveContextLimit(
		"google_genai:gemini-2.5-flash", "repo", "agent",
	)
	// Profile says 1M, ceiling is 200K → ceiling wins (path = "ceiling").
	if source != "ceiling" {
		t.Errorf("source = %q, want %q (profile MaxInputTokens > ceiling)", source, "ceiling")
	}
	if limit != contextCeilingTokens {
		t.Errorf("limit = %d, want ceiling %d", limit, contextCeilingTokens)
	}
	if limit != 200_000 {
		t.Errorf("limit = %d, want 200000 (cross-check constant)", limit)
	}
}

// TestEffectiveContextLimit_FallbackIs128K is B.0 test (c): the
// fallback bump from 32K to 128K. The whole point of the change.
// Locks the value as a constant cross-check so a future refactor
// that flips the constant the wrong way trips this test first.
func TestEffectiveContextLimit_FallbackIs128K(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()

	limit, source := d.effectiveContextLimit("unknown:model-9999", "repo", "agent")
	if source != "fallback" {
		t.Errorf("source = %q, want %q", source, "fallback")
	}
	if limit != 128_000 {
		t.Errorf("limit = %d, want 128000 (the 2026 modern floor)", limit)
	}
	if limit != contextFallbackTokens {
		t.Errorf("limit = %d, contextFallbackTokens = %d; constants should agree", limit, contextFallbackTokens)
	}
}

// TestEffectiveContextLimit_WarnContainsRecoveryCommand is B.0
// test (e): the WARN message must contain the LITERAL
// `oat model onboard <modelID>` text so an operator can copy-paste
// the command directly from the log without ambiguity. This is
// load-bearing -- if the WARN message ever drifts to a generic
// "no profile found" without the recovery command, operators have
// to chase docs that may have moved.
func TestEffectiveContextLimit_WarnContainsRecoveryCommand(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()

	// Swap in a buffer-backed logger so we can inspect the WARN
	// body. *logging.Logger writes to any io.Writer; a bytes.Buffer
	// captures everything without race surfaces (single-test, no
	// other writers contending).
	var buf bytes.Buffer
	d.logger = logging.New(&buf)

	const modelID = "google_genai:gemini-2.5-flash"
	_, _ = d.effectiveContextLimit(modelID, "repo", "agent")

	msg := buf.String()
	if msg == "" {
		t.Fatal("no WARN emitted for fallback path")
	}
	wantCmd := "oat model onboard " + modelID
	if !contains(msg, wantCmd) {
		t.Errorf("WARN missing recovery command %q in body:\n%s", wantCmd, msg)
	}
	wantEnv := "OAT_MODEL_CONTEXT_google_genai_gemini-2.5-flash"
	if !contains(msg, wantEnv) {
		t.Errorf("WARN missing env-override hint %q in body:\n%s", wantEnv, msg)
	}
}

// TestEffectiveContextLimit_EmptyModelIDStillFallsBack pins that an
// empty model ID short-circuits BOTH the env-override lookup AND the
// profile lookup -- no empty-string env-var lookup, no nil-pointer
// surprises in ProfileStore.Get.
func TestEffectiveContextLimit_EmptyModelIDStillFallsBack(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()

	// Even if someone left OAT_MODEL_CONTEXT_ (literal prefix) set,
	// an empty model ID must not key off it.
	t.Setenv("OAT_MODEL_CONTEXT_", "1")

	limit, source := d.effectiveContextLimit("", "repo", "agent")
	if source != "fallback" {
		t.Errorf("source = %q, want %q (empty modelID must not match any env var)", source, "fallback")
	}
	if limit != contextFallbackTokens {
		t.Errorf("limit = %d, want fallback %d", limit, contextFallbackTokens)
	}
}

// TestEffectiveContextLimit_EnvOverrideClampedEndToEnd is B.0 test
// (f): a wildly-out-of-range env value (e.g. someone exported
// MAX_INT or a number they thought was bytes not tokens) gets
// clamped before the agent's budget is computed against it.
func TestEffectiveContextLimit_EnvOverrideClampedEndToEnd(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()

	t.Setenv("OAT_MODEL_CONTEXT_anthropic_claude-opus-4-7", "5000000000")
	limit, source := d.effectiveContextLimit(
		"anthropic:claude-opus-4-7", "repo", "agent",
	)
	if source != "env" {
		t.Errorf("source = %q, want %q", source, "env")
	}
	if limit != contextEnvOverrideMax {
		t.Errorf("limit = %d, want clamped %d", limit, contextEnvOverrideMax)
	}
}

// TestEffectiveContextBudget_ReservesOutput pins the Phase 1 output-headroom
// reservation: the safety-net budget = window − reserved output tokens, while
// the display window (effectiveContextLimit) is unchanged so the ring still
// reads "full means full" (Phase 3 item 7).
func TestEffectiveContextBudget_ReservesOutput(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()

	t.Setenv(outputReservationEnvVar, "1")
	budget, _ := d.effectiveContextBudget("", "repo", "agent")
	if want := contextFallbackTokens - int64(defaultMaxTokens); budget != want {
		t.Errorf("budget = %d, want %d (window − reserved output)", budget, want)
	}
	limit, _ := d.effectiveContextLimit("", "repo", "agent")
	if limit != contextFallbackTokens {
		t.Errorf("display limit = %d, want %d (window must be unchanged)", limit, contextFallbackTokens)
	}

	// Disabled → budget collapses back to the full window.
	t.Setenv(outputReservationEnvVar, "0")
	if budget2, _ := d.effectiveContextBudget("", "repo", "agent"); budget2 != contextFallbackTokens {
		t.Errorf("with reservation OFF budget = %d, want %d", budget2, contextFallbackTokens)
	}
}

// TestShouldInject_ReservedBudgetFiresEarlier verifies the reservation makes
// the 95 % safety-net inject trip sooner (measured against the reserved input
// budget) than it would against the full window — the whole point of reserving
// output headroom so the compaction leaves room for the reply.
func TestShouldInject_ReservedBudgetFiresEarlier(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()

	t.Setenv(safetyNetEnvVar, "1")

	// window = 128K, reserved output = 32K → budget = 96K.
	// 95 % of budget = 91 200; 95 % of window = 121 600.
	// used = 92 000 is above the budget tier but below the window tier.
	hot := state.Agent{Type: state.AgentTypeAssistant, ContextWindowTokens: 92_000}

	t.Setenv(outputReservationEnvVar, "1")
	if _, inject := d.shouldInjectContextSafetyNet(hot, "repo", "agent"); !inject {
		t.Error("expected inject at >=95 % of the reserved budget")
	}

	t.Setenv(outputReservationEnvVar, "0")
	if _, inject := d.shouldInjectContextSafetyNet(hot, "repo", "agent"); inject {
		t.Error("did not expect inject at ~72 % of the full window (reservation off)")
	}
}

// testProfileQwenSmall mimics the DGX-Spark Qwen profile that triggered the
// Phase 3 meter bug: a conservative max_input_tokens BELOW the 200K ceiling
// (so effectiveContextLimit source == "profile"), while the model's real
// window is larger. 96000 is the exact value from the real profile YAML.
const testProfileQwenSmall = `model_id: "spark:Qwen/Qwen3.5-35B-A3B-FP8"
status: known
provider:
  name: spark
capabilities:
  tool_reliability: 0.9
  token_reporting: 0.85
routing:
  autonomy_tier: limited
  overall_score: 40
max_input_tokens: 96000
contract:
  onboarding_passed: true
`

// TestEffectiveContextBudget_ProfileNoDoubleReserve pins the DGX-Spark Qwen
// fix: a profile-sourced limit is already max_input_tokens (which excludes the
// output budget), so the compaction budget must EQUAL it — not subtract the
// output reserve a second time. The double-count cut Qwen's 96000 input budget
// to 64000, so compaction fired at ~50% of the real window while the ring read
// ~56% ("why did it compact at 56%?"). Window-representing sources (fallback /
// env) still subtract one output reserve so compaction leaves room for the reply.
func TestEffectiveContextBudget_ProfileNoDoubleReserve(t *testing.T) {
	d, cleanup := setupDaemonWithProfiles(t, map[string]string{
		"qwen.yaml":         testProfileQwenSmall,
		"gemini-flash.yaml": testProfileGeminiFlash,
	})
	defer cleanup()
	t.Setenv(outputReservationEnvVar, "1")

	const qwen = "spark:Qwen/Qwen3.5-35B-A3B-FP8"

	// Profile source: budget == max_input_tokens; NO second subtraction.
	if got, src := d.effectiveContextBudget(qwen, "repo", "agent"); got != 96_000 || src != "profile" {
		t.Errorf("profile budget = %d (src %q), want 96000 profile (no double reserve)", got, src)
	}

	// Fallback source: window − one output reserve.
	fbReserve := int64(d.resolveOutputMaxTokens("unknown:model-x"))
	if got, src := d.effectiveContextBudget("unknown:model-x", "repo", "agent"); got != contextFallbackTokens-fbReserve || src != "fallback" {
		t.Errorf("fallback budget = %d (src %q), want %d fallback", got, src, contextFallbackTokens-fbReserve)
	}

	// Env source: operator number is the window → window − one output reserve.
	t.Setenv("OAT_MODEL_CONTEXT_spark_qwen_qwen3.5-35b-a3b-fp8", "200000")
	envReserve := int64(d.resolveOutputMaxTokens(qwen))
	if got, src := d.effectiveContextBudget(qwen, "repo", "agent"); got != 200_000-envReserve || src != "env" {
		t.Errorf("env budget = %d (src %q), want %d env", got, src, 200_000-envReserve)
	}
}

// TestHandleTokenUsageEvent_ClampsImplausibleOccupancy pins Phase 3 item 8: a
// single wildly-huge context reading (flaky-reporting model) must NOT be stored,
// so it can't pin the ring at 100% forever. A subsequent sane reading updates
// normally, proving the meter isn't stranded.
func TestHandleTokenUsageEvent_ClampsImplausibleOccupancy(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()

	const repo = "_assistant-personal"
	const agent = "personal"
	if err := d.state.AddRepo(repo, &state.Repository{SessionName: repo, Agents: map[string]state.Agent{}}); err != nil {
		t.Fatalf("AddRepo: %v", err)
	}
	if err := d.state.AddAgent(repo, agent, state.Agent{Type: state.AgentTypeAssistant, PID: 1}); err != nil {
		t.Fatalf("AddAgent: %v", err)
	}

	// Implausible reading (14M vs 128K fallback window) — must be ignored.
	d.handleTokenUsageEvent(repo, agent, `{"cumulative_input":100,"cumulative_output":0,"context_input":14185804}`)
	if got, _ := d.state.GetAgent(repo, agent); got.ContextWindowTokens != 0 {
		t.Errorf("implausible occupancy was stored (%d); expected it to be rejected", got.ContextWindowTokens)
	}

	// A sane reading afterwards updates normally (meter not stranded).
	d.handleTokenUsageEvent(repo, agent, `{"cumulative_input":50000,"cumulative_output":0,"context_input":50000}`)
	if got, _ := d.state.GetAgent(repo, agent); got.ContextWindowTokens != 50_000 {
		t.Errorf("sane occupancy = %d, want 50000 (meter must recover after a bad reading)", got.ContextWindowTokens)
	}
}
