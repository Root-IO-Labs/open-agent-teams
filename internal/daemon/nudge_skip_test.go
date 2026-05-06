package daemon

import (
	"fmt"
	"os"
	"testing"

	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

// TestHashNudgeContent_Deterministic pins the output shape: hex-encoded
// sha256 (64 chars) and stable for identical input, different for any
// one-byte change.
func TestHashNudgeContent_Deterministic(t *testing.T) {
	a := hashNudgeContent("[daemon] Status check: Review worker progress.")
	b := hashNudgeContent("[daemon] Status check: Review worker progress.")
	if a != b {
		t.Errorf("hash is non-deterministic: %q vs %q", a, b)
	}
	if len(a) != 64 {
		t.Errorf("expected 64-char hex sha256, got %d chars: %q", len(a), a)
	}

	c := hashNudgeContent("[daemon] Status check: Review worker progress!")
	if a == c {
		t.Error("hash did not change on content change")
	}
}

// TestShouldSkipNudgeForAgent confirms only supervisor/merge-queue opt
// into the skip path. Workers have their own tier dedup; review /
// verification / pr-shepherd get the pre-existing unconditional nudge
// so behavior on those paths is unchanged.
func TestShouldSkipNudgeForAgent(t *testing.T) {
	d := &Daemon{}
	cases := []struct {
		t    state.AgentType
		want bool
	}{
		{state.AgentTypeSupervisor, true},
		{state.AgentTypeMergeQueue, true},
		{state.AgentTypeWorker, false},
		{state.AgentTypePRShepherd, false},
		{state.AgentTypeReview, false},
		{state.AgentTypeVerification, false},
		{state.AgentTypeWorkspace, false},
		{state.AgentTypeGenericPersistent, false},
	}
	for _, c := range cases {
		if got := d.shouldSkipNudgeForAgent(c.t); got != c.want {
			t.Errorf("%s: got %v, want %v", c.t, got, c.want)
		}
	}
}

// TestNudgeSkipMax reads the env override and clamps negative values
// to 0 (disabled). Default is 5 when the var is unset or malformed.
func TestNudgeSkipMax(t *testing.T) {
	t.Setenv("OAT_NUDGE_SKIP_MAX", "")
	if got := nudgeSkipMax(); got != 5 {
		t.Errorf("default: got %d, want 5", got)
	}
	t.Setenv("OAT_NUDGE_SKIP_MAX", "10")
	if got := nudgeSkipMax(); got != 10 {
		t.Errorf("override=10: got %d, want 10", got)
	}
	t.Setenv("OAT_NUDGE_SKIP_MAX", "-1")
	if got := nudgeSkipMax(); got != 0 {
		t.Errorf("negative: got %d, want 0 (disabled)", got)
	}
	t.Setenv("OAT_NUDGE_SKIP_MAX", "garbage")
	if got := nudgeSkipMax(); got != 5 {
		t.Errorf("garbage: got %d, want 5 (default on parse failure)", got)
	}
	_ = os.Unsetenv("OAT_NUDGE_SKIP_MAX")
}

// TestNudgeSkip_HashChangesWithSnapshotState verifies the core premise
// of the skip logic: when repo state changes (worker added, PR opened,
// PR status flipped), the post-snapshot-injection message hash changes,
// so a follow-up nudge is NOT skipped. Conversely, identical state
// produces identical hashes and the skip path fires.
func TestNudgeSkip_HashChangesWithSnapshotState(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()
	resetSnapshotCache(t)

	const repoName = "repo-skip"

	// Initial state: one worker, no PRs.
	withFakePRs(t, nil)
	addTestRepo(t, d, repoName, map[string]state.Agent{
		"alpha-fox": {Type: state.AgentTypeWorker, WindowName: "w1"},
	})

	base := "[daemon] Status check: Review worker progress and check merge queue."
	msg1 := d.withRepoSnapshot(repoName, state.AgentTypeSupervisor, base)
	h1 := hashNudgeContent(msg1)

	// Immediate repeat (within snapshot TTL) — identical state, identical hash.
	msg1b := d.withRepoSnapshot(repoName, state.AgentTypeSupervisor, base)
	if hashNudgeContent(msg1b) != h1 {
		t.Error("hash changed across two calls with identical state")
	}

	// Invalidate the cache so the next call re-reads live state.
	resetSnapshotCache(t)

	// Add a PR — snapshot content changes → hash changes → nudge sends.
	withFakePRs(t, []prSummary{{
		Number: 7, Title: "new pr", URL: fmt.Sprintf("https://github.com/test/%s/pull/7", repoName),
		Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN",
	}})
	msg2 := d.withRepoSnapshot(repoName, state.AgentTypeSupervisor, base)
	h2 := hashNudgeContent(msg2)
	if h1 == h2 {
		t.Error("hash did not change when a PR was added to repo state")
	}
}
