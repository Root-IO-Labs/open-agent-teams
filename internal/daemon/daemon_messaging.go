package daemon

import (
	"os"
	"strings"

	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

// Daemon → supervisor / merge-queue messages carry a pre-computed state
// snapshot (worker list, open PRs) so recipients don't poll the shell to
// re-discover state the daemon already knows. This file holds the single
// chokepoint used by every call site that sends such a message, so the
// "attach snapshot or not" decision lives in one place.
//
// Design notes:
//   - Pure message-transformation, not a Send wrapper. Callers keep their
//     existing delivery path (msgMgr.Send or d.backend.SendMessage) — we
//     just rewrite the payload.
//   - Non-supervisor / non-merge-queue recipients are passed through
//     unchanged. That makes the helper safe to drop into dispatch loops
//     that iterate over mixed agent types.
//   - Snapshot generation never blocks the send: on timeout, disabled
//     kill-switch, or missing repo state, we return msg unchanged.

// snapshotDisabledEnv is the env var used to disable snapshot injection
// at runtime without a code change. Set to "1" to fall back to the
// pre-snapshot behavior (bare messages) — useful if a regression is
// observed in production and a deploy isn't immediately available.
const snapshotDisabledEnv = "OAT_DAEMON_SNAPSHOT_DISABLED"

// snapshotMarker is the substring we check for to detect a message
// that already contains a daemon-state block (from an earlier
// withRepoSnapshot pass or a hand-built kickoff snapshot in
// benchmarks/.../run.sh). Matches both "## Current State (injected
// by daemon)" and "## Current State (benchmark kickoff)".
const snapshotMarker = "## Current State"

// isSnapshotTarget reports whether a given agent type should receive
// daemon-injected state snapshots. Today only supervisor and merge-queue
// are supported; other types fall through untouched.
func isSnapshotTarget(t state.AgentType) bool {
	return t == state.AgentTypeSupervisor || t == state.AgentTypeMergeQueue
}

// withRepoSnapshot returns msg with the appropriate daemon state snapshot
// appended for supervisor or merge-queue recipients. For any other target,
// for an empty snapshot (repo unknown, gh timeout), when the kill-switch
// env var is set, or when msg already contains a snapshot block, it
// returns msg unchanged.
//
// Safe to call from any goroutine. Cost is bounded: a 3s gh timeout and a
// 3s in-memory cache protect against slow or repeated calls.
func (d *Daemon) withRepoSnapshot(repoName string, target state.AgentType, msg string) string {
	if os.Getenv(snapshotDisabledEnv) == "1" {
		return msg
	}
	if !isSnapshotTarget(target) {
		return msg
	}
	// Double-injection guard: if someone upstream already attached a
	// snapshot block (e.g. the benchmark kickoff in run.sh, or a
	// previous withRepoSnapshot pass on the same message), skip.
	// Prevents two "## Current State" sections landing in the agent's
	// context and confusing the LLM.
	if strings.Contains(msg, snapshotMarker) {
		return msg
	}
	supSnap, mqSnap := d.buildRepoSnapshots(repoName)
	switch target {
	case state.AgentTypeSupervisor:
		return msg + supSnap
	case state.AgentTypeMergeQueue:
		return msg + mqSnap
	}
	return msg
}
