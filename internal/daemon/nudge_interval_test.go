package daemon

import (
	"os"
	"testing"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/routing"
	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

// TestNudgeIntervalFallbackLadder exercises the three-step resolution order
// for nudge intervals:
//
//  1. Per-model ModelProfile.Runtime.NudgeIntervalSeconds (when non-zero).
//  2. OAT_NUDGE_INTERVAL_SECONDS env var.
//  3. The hard-coded 60s default when neither override is set.
//
// Direct-backend supervisors are clamped to at least 10 minutes, which we
// exercise separately.
func TestNudgeIntervalFallbackLadder(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, nil)
	defer cleanup()

	// Install a profile store we can mutate per sub-test.
	profileDir := t.TempDir()
	ps, err := routing.NewProfileStore(profileDir)
	if err != nil {
		t.Fatalf("NewProfileStore: %v", err)
	}
	d.modelProfiles = ps

	writeProfile := func(t *testing.T, modelID string, nudgeSecs int) {
		t.Helper()
		content := "model_id: " + modelID + "\n" +
			"name: test-provider\n" +
			"autonomy_tier: full\n" +
			"overall_score: 90\n" +
			"onboarding_passed: true\n" +
			"worker_eligible: true\n" +
			"orchestrator_eligible: true\n"
		if nudgeSecs > 0 {
			content += "nudge_interval_seconds: " + itoa(nudgeSecs) + "\n"
		}
		safeName := safeFilename(modelID)
		if err := os.WriteFile(profileDir+"/"+safeName+".yaml", []byte(content), 0o644); err != nil {
			t.Fatalf("write profile: %v", err)
		}
		if err := ps.Reload(); err != nil {
			t.Fatalf("reload: %v", err)
		}
	}

	repo := &state.Repository{Model: "test:default"}
	// Use a worker agent for the fallback-ladder checks so the direct-backend
	// supervisor clamp doesn't interfere. The clamp is exercised separately.
	agent := state.Agent{Type: state.AgentTypeWorker, Model: ""}

	// Step 3: default 60s when no profile / env var set.
	t.Setenv("OAT_NUDGE_INTERVAL_SECONDS", "")
	if got := d.nudgeIntervalFor(repo, agent); got != 60*time.Second {
		t.Errorf("default fallback: got %v, want 60s", got)
	}

	// Step 2: env var overrides the 60s default.
	t.Setenv("OAT_NUDGE_INTERVAL_SECONDS", "15")
	if got := d.nudgeIntervalFor(repo, agent); got != 15*time.Second {
		t.Errorf("env override: got %v, want 15s", got)
	}

	// Step 1: per-model profile overrides the env var.
	writeProfile(t, "test:default", 42)
	if got := d.nudgeIntervalFor(repo, agent); got != 42*time.Second {
		t.Errorf("profile override: got %v, want 42s", got)
	}

	// Per-model resolution also follows agent.Model when set (takes
	// precedence over repo.Model).
	writeProfile(t, "test:agent-specific", 7)
	agentWithModel := state.Agent{Type: state.AgentTypeWorker, Model: "test:agent-specific"}
	if got := d.nudgeIntervalFor(repo, agentWithModel); got != 7*time.Second {
		t.Errorf("agent.Model override: got %v, want 7s", got)
	}

	// Zero Runtime.NudgeIntervalSeconds falls back to the env var, not zero.
	writeProfile(t, "test:zero-runtime", 0)
	agentZero := state.Agent{Type: state.AgentTypeWorker, Model: "test:zero-runtime"}
	if got := d.nudgeIntervalFor(repo, agentZero); got != 15*time.Second {
		t.Errorf("zero runtime falls back: got %v, want 15s", got)
	}

	// Direct-backend supervisor clamp: even when the resolved interval is
	// below 10m, the supervisor is bumped up to reduce chat churn.
	t.Setenv("OAT_NUDGE_INTERVAL_SECONDS", "30")
	supAgent := state.Agent{Type: state.AgentTypeSupervisor, Model: ""}
	if got := d.nudgeIntervalFor(repo, supAgent); got < 10*time.Minute {
		t.Errorf("direct-backend supervisor clamp: got %v, want >= 10m", got)
	}
}

// itoa is a tiny local helper so the test file keeps no strconv import noise.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// safeFilename strips ':' and '/' so a model ID can be used as a YAML filename
// under the test temp directory.
func safeFilename(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ':' || c == '/' {
			out = append(out, '-')
			continue
		}
		out = append(out, c)
	}
	return string(out)
}
