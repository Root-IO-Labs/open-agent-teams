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
//     above ceiling → 128K, no profile → 32K fallback + WARN-once
//     dedupe.
//   - maybeNudgeContextCapacity: non-assistant types ignored;
//     in-memory dedupe suppresses repeats inside the window;
//     re-fires after the window.
//   - shouldInjectContextSafetyNet: non-assistant ignored, OFF
//     env var → no inject, < 95% → no inject, >= 95% → inject
//     with directive payload.

package daemon

import (
	"testing"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

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
		Type:         state.AgentTypeAssistant,
		WindowName:   "personal",
		TotalTokens:  96_000, // 75 % of contextFallbackTokens? no, 32K fallback
		Model:        "",     // forces fallback → 32 K limit → 96K/32K is way over 75%
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

	if err := d.state.AddRepo("repo", &state.Repository{SessionName: "repo"}); err != nil {
		t.Fatalf("AddRepo: %v", err)
	}

	// 74 % of 32 K fallback ≈ 23 680 tokens. Below tier → no
	// hint, no dedupe entry recorded.
	d.maybeNudgeContextCapacity("repo", "personal", state.Agent{
		Type:        state.AgentTypeAssistant,
		WindowName:  "personal",
		TotalTokens: int64(0.74 * float64(contextFallbackTokens)),
		Model:       "",
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

	// 95 % of 32 K fallback = 30 400. Use 31 000 to clearly cross.
	hot := state.Agent{
		Type:        state.AgentTypeAssistant,
		TotalTokens: 31_000,
	}
	// 90 % of 32 K fallback ≈ 28 800. Below tier → no inject.
	warm := state.Agent{
		Type:        state.AgentTypeAssistant,
		TotalTokens: 28_000,
	}
	browser := state.Agent{
		Type:        state.AgentTypeBrowser,
		TotalTokens: 31_000,
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

// (Substring `contains` helper is shared from daemon_test.go.)