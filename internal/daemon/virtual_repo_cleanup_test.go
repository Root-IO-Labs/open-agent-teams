// Regression test for Phase 7 item 9: the periodic git-cleanup loops must
// skip virtual assistant repos (_assistant-<name>, IsVirtual=true, no .git)
// instead of spamming per-cycle WARN/ERROR git failures. Confirmed from a real
// daemon log (2026-07-09): cleanupOrphanedWorktrees emitted WARN "Failed to
// prune worktrees" + ERROR "Failed to cleanup orphaned worktrees",
// cleanupMergedBranches emitted DEBUG "Failed to cleanup merged branches", and
// refreshWorktrees emitted DEBUG "Could not get remote", once per virtual repo
// per health-check cycle, forever.

package daemon

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/Root-IO-Labs/open-agent-teams/internal/logging"
	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

func TestGitCleanupLoops_SkipVirtualRepos(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()

	var buf bytes.Buffer
	d.logger = logging.New(&buf)

	const repoName = "_assistant-sparktestqwen"
	if err := d.state.AddRepo(repoName, &state.Repository{
		SessionName: repoName,
		IsVirtual:   true,
		Agents:      map[string]state.Agent{},
	}); err != nil {
		t.Fatalf("AddRepo: %v", err)
	}

	// Create the wts/<repo> dir so the os.Stat(wtRootDir) guard in
	// cleanupOrphanedWorktrees would NOT spare the repo on its own — the
	// IsVirtual early-skip is what must spare it (this is the exact asymmetry
	// the plan calls out).
	if err := os.MkdirAll(d.paths.WorktreeDir(repoName), 0o755); err != nil {
		t.Fatalf("MkdirAll worktree dir: %v", err)
	}

	d.cleanupOrphanedWorktrees(nil)
	d.cleanupMergedBranches()
	d.refreshWorktrees()

	logOut := buf.String()
	if strings.Contains(logOut, repoName) {
		t.Errorf("git-cleanup loops logged about virtual repo %q (should have been skipped):\n%s", repoName, logOut)
	}
	// Belt-and-suspenders: none of the known git-failure phrases should appear.
	for _, phrase := range []string{
		"Failed to prune worktrees",
		"Failed to cleanup orphaned worktrees",
		"Failed to cleanup merged branches",
		"Could not get remote",
	} {
		if strings.Contains(logOut, phrase) {
			t.Errorf("unexpected git-failure log %q for a virtual repo:\n%s", phrase, logOut)
		}
	}
}
