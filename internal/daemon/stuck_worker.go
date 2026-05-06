package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

// Stuck-worker escalation thresholds (configurable via env vars).
// The wake loop runs every 1 minute, so nudge counts ≈ minutes.
// supervisor alert ~10 min, daemon takeover ~16 min, force-remove ~30 min.
var (
	stuckSupervisorNudge = getEnvInt("OAT_STUCK_SUPERVISOR_NUDGE", 10)
	stuckDaemonNudge     = getEnvInt("OAT_STUCK_DAEMON_NUDGE", 16)
	stuckMaxNudge        = getEnvInt("OAT_STUCK_MAX_NUDGE", 30)

	// Max verification rejections before the worker is auto-completed and the
	// task escalated to the supervisor for reassignment.
	maxRejections = getEnvInt("OAT_MAX_REJECTIONS", 3)
)

// getEnvInt reads an integer from the environment variable, returning the default if unset or invalid.
func getEnvInt(key string, defaultVal int) int {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return defaultVal
	}
	return n
}

// nudgeTier returns the escalation tier for a given nudge count.
//   - Tier 1 (count 1): wake-reason retry — always send (unique per wake event)
//   - Tier 2 (count 2-9): normal status check — de-dupe identical messages
//   - Tier 3 (count 10-15): escalated "nudged N times" — de-dupe (same instructions)
//   - Tier 4 (count 16+): daemon takeover — handled separately, no PTY message
func nudgeTier(count int) int {
	switch {
	case count <= 1:
		return 1
	case count < stuckSupervisorNudge:
		return 2
	case count < stuckDaemonNudge:
		return 3
	default:
		return 4
	}
}

// nudgeWorkerEscalating implements the stuck-worker escalation ladder for a single worker.
func (d *Daemon) nudgeWorkerEscalating(repoName, repoPath, agentName string, agent state.Agent, now time.Time) {
	// Re-check live state to avoid racing with oat agent complete or dormancy
	if current, exists := d.state.GetAgent(repoName, agentName); exists && (current.ReadyForCleanup || current.IsDormant()) {
		return
	}

	repo, _ := d.state.GetRepo(repoName)
	if repo == nil {
		return
	}

	// Check for new git activity and reset NudgeCount if branch has advanced
	branchName := "work/" + agentName
	currentSHA := d.getBranchSHA(repoPath, branchName)
	if currentSHA != "" && agent.LastBranchSHA != "" && currentSHA != agent.LastBranchSHA {
		d.logger.Info("Worker %s/%s shows new git activity (SHA changed), resetting nudge count", repoName, agentName)
		agent.NudgeCount = 0
	}
	if currentSHA != "" {
		agent.LastBranchSHA = currentSHA
	}

	agent.NudgeCount++
	count := agent.NudgeCount

	// Fast-track: workers woken for a merged PR get auto-completed after 2 nudges
	// (~2 min) instead of waiting for daemon takeover. They only need to run
	// `oat agent complete` — if they can't respond in 2 cycles, they're stuck.
	if !agent.WokenForMergedPRAt.IsZero() && count >= 2 {
		d.logger.Info("Worker %s/%s woken for merged PR %d min ago, fast-tracking to daemon takeover",
			repoName, agentName, int(time.Since(agent.WokenForMergedPRAt).Minutes()))
		if err := d.state.ModifyAgent(repoName, agentName, func(a *state.Agent) {
			if a.IsDormant() || a.ReadyForCleanup {
				return
			}
			a.NudgeCount = count
			if currentSHA != "" {
				a.LastBranchSHA = currentSHA
			}
		}); err != nil {
			d.logger.Error("Failed to update nudge count for worker %s: %v", agentName, err)
		}
		d.checkWorkerProgress(repoName, repoPath, agentName, agent, now)
		return
	}

	// Hard cap: force-remove at max nudges
	if count >= stuckMaxNudge {
		d.logger.Warn("Worker %s/%s hit max nudge limit (%d), force-removing", repoName, agentName, stuckMaxNudge)
		d.forceRemoveWorker(repoName, repoPath, agentName, agent)
		return
	}

	// Daemon takeover: programmatic git checks
	if count >= stuckDaemonNudge {
		d.logger.Info("Worker %s/%s at nudge %d (daemon takeover), checking git progress", repoName, agentName, count)
		// Persist the incremented nudge count so it eventually reaches stuckMaxNudge.
		// checkWorkerProgress updates LastNudge but not NudgeCount, so without this
		// the count stays frozen and force-removal at stuckMaxNudge never triggers.
		if err := d.state.ModifyAgent(repoName, agentName, func(a *state.Agent) {
			if a.IsDormant() || a.ReadyForCleanup {
				return
			}
			a.NudgeCount = count
			if currentSHA != "" {
				a.LastBranchSHA = currentSHA
			}
		}); err != nil {
			d.logger.Error("Failed to update nudge count for worker %s: %v", agentName, err)
		}
		d.checkWorkerProgress(repoName, repoPath, agentName, agent, now)
		return
	}

	// Supervisor alert tier
	if count >= stuckSupervisorNudge {
		d.logger.Info("Worker %s/%s at nudge %d, alerting supervisor", repoName, agentName, count)
		d.alertSupervisorAboutWorker(repoName, agentName, count)
	}

	// Send nudge to worker (normal or directive based on tier)
	var message string
	if count >= stuckSupervisorNudge {
		if agent.PRNumber > 0 {
			message = fmt.Sprintf("[daemon] You have been nudged %d times without completing. You already have PR #%d open. Check CI status with `gh run list --branch work/%s --limit 1`. If CI failed, fix the code and `git push`. If CI passed and PR is mergeable, run `oat agent waiting` to let the merge queue handle it. Do NOT create a new PR.", count, agent.PRNumber, agentName)
		} else {
			message = fmt.Sprintf("[daemon] You have been nudged %d times without completing. If your work is done, you MUST: git add, git commit, git push, then run oat worker request-review and oat agent waiting.", count)
		}
	} else if count == 1 && agent.LastWakeReason != "" {
		message = "(Retry — original notification may not have been received) " + agent.LastWakeReason
	} else {
		message = "[daemon] Status check: Update on your progress? If your work is done, commit, push, and run oat worker request-review, then oat agent waiting."
	}

	// Tier-based de-dupe: suppress identical consecutive nudge messages.
	// NudgeCount always increments (so thresholds fire on time) but the PTY
	// message is only sent when the tier changes, preventing stale duplicate
	// messages from queueing up while the worker is busy.
	tier := nudgeTier(count)
	suppressMessage := tier == agent.LastNudgeTier && tier >= 2

	// Re-check live state just before sending to avoid racing with oat agent complete
	if current, exists := d.state.GetAgent(repoName, agentName); exists && (current.ReadyForCleanup || current.IsDormant()) {
		return
	}

	if suppressMessage {
		d.logger.Debug("Suppressed tier-%d nudge for worker %s/%s (count: %d, %d prior suppressed)", tier, repoName, agentName, count, agent.SuppressedNudgeCount+1)
	} else {
		// Prepend suppression notice if prior nudges were suppressed
		if agent.SuppressedNudgeCount > 0 {
			message = fmt.Sprintf("[daemon] (%d prior identical status checks suppressed while you were busy) ", agent.SuppressedNudgeCount) + message
		}
		if err := d.backend.SendMessage(d.ctx, repo.SessionName, agent.WindowName, message); err != nil {
			d.logger.Error("Failed to send wake message to worker %s: %v", agentName, err)
		}
	}

	clearWakeReason := agent.LastWakeReason != ""
	branchSHA := currentSHA
	wasSuppressed := suppressMessage

	if err := d.state.ModifyAgent(repoName, agentName, func(a *state.Agent) {
		if a.IsDormant() || a.ReadyForCleanup {
			return
		}
		a.NudgeCount = count
		a.LastNudge = now
		if branchSHA != "" {
			a.LastBranchSHA = branchSHA
		}
		if clearWakeReason {
			a.LastWakeReason = ""
		}
		if wasSuppressed {
			a.SuppressedNudgeCount++
		} else {
			a.SuppressedNudgeCount = 0
			a.LastNudgeTier = tier
		}
	}); err != nil {
		d.logger.Error("Failed to update worker %s state: %v", agentName, err)
	}
	d.logger.Debug("Nudged worker %s in repo %s (count: %d, tier: %d, suppressed: %v)", agentName, repoName, count, tier, suppressMessage)
}

// alertSupervisorAboutWorker sends a message to the supervisor about a potentially stuck worker.
// If the worker is dormant with an open PR, the message tells the supervisor no action is needed
// (the worker must stay available for CI failures or merge conflicts). Otherwise, it provides
// guidance on completing or removing the worker.
func (d *Daemon) alertSupervisorAboutWorker(repoName, agentName string, nudgeCount int) {
	msgMgr := d.getMessageManager()

	var msg string
	agent, exists := d.state.GetAgent(repoName, agentName)
	if exists && agent.WaitingForPR && agent.PRNumber > 0 {
		msg = fmt.Sprintf(
			"[daemon] Worker '%s' was nudged %d times but is now dormant, waiting for PR #%d to be processed by merge-queue. No action needed.\n\n"+
				"Do NOT complete or remove this worker. If CI fails or merge conflicts arise on PR #%d, "+
				"the worker needs to remain available to fix them. The merge-queue will handle merging once CI is green.",
			agentName, nudgeCount, agent.PRNumber, agent.PRNumber,
		)
	} else if exists && agent.WaitingForVerification {
		msg = fmt.Sprintf(
			"[daemon] Worker '%s' was nudged %d times but is now dormant, waiting for verification verdict. No action needed -- "+
				"the daemon will deliver the verdict and wake the worker automatically.",
			agentName, nudgeCount,
		)
	} else {
		msg = fmt.Sprintf(
			"[daemon] Worker '%s' may be stuck (nudged %d times with no completion). Check their logs at ~/.oat/output/%s/workers/%s.log and intervene if needed.\n\n"+
				"IMPORTANT: Before removing this worker, check if it has an open PR.\n"+
				"- If it has an open PR: do NOT complete or remove it — the worker should go dormant via `oat agent waiting` so it remains available for CI failures or conflicts\n"+
				"- If it has NO open PR and the work is done: use `oat agent complete --worker %s`\n"+
				"- Do NOT use `oat worker rm` if the worker has an open PR — it will orphan the PR\n"+
				"- If you must remove the worker, message workspace to spawn a replacement:\n"+
				"  `oat message send workspace \"Worker %s was removed. Spawn a new worker for its task.\"`",
			agentName, nudgeCount, repoName, agentName,
			agentName,
			agentName,
		)
	}
	if _, err := msgMgr.Send(repoName, "daemon", "supervisor", msg); err != nil {
		d.logger.Error("Failed to alert supervisor about stuck worker %s: %v", agentName, err)
	}
}

// getBranchSHA returns the remote SHA for a branch, or empty string if not pushed.
func (d *Daemon) getBranchSHA(repoPath, branchName string) string {
	validBranchName := regexp.MustCompile(`^[a-zA-Z0-9_\-\./]+$`)
	if !validBranchName.MatchString(branchName) {
		d.logger.Error("Invalid branch name: %s", branchName)
		return ""
	}
	cmd := exec.CommandContext(d.ctx, "git", "ls-remote", "--heads", "origin", branchName)
	cmd.Dir = repoPath
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	parts := strings.Fields(strings.TrimSpace(string(output)))
	if len(parts) >= 1 {
		return parts[0]
	}
	return ""
}

// workerPRInfo holds the result of checking for a worker's PR.
type workerPRInfo struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
}

// getWorkerPR checks if an open PR exists for the worker's branch.
func (d *Daemon) getWorkerPR(repoPath, branchName string) *workerPRInfo {
	validBranchName := regexp.MustCompile(`^[a-zA-Z0-9_\-\./]+$`)
	if !validBranchName.MatchString(branchName) {
		d.logger.Error("Invalid branch name: %s", branchName)
		return nil
	}
	cmd := exec.CommandContext(d.ctx, "gh", "pr", "list", "--head", branchName, "--state", "open", "--json", "number,title")
	cmd.Dir = repoPath
	output, err := cmd.Output()
	if err != nil {
		return nil
	}
	var prs []workerPRInfo
	if err := json.Unmarshal(output, &prs); err != nil || len(prs) == 0 {
		return nil
	}
	return &prs[0]
}

// checkWorkerProgress runs programmatic git checks for a stuck worker during daemon takeover.
func (d *Daemon) checkWorkerProgress(repoName, repoPath, agentName string, agent state.Agent, now time.Time) {
	// Re-check live state to avoid racing with oat agent complete
	if current, exists := d.state.GetAgent(repoName, agentName); exists && current.ReadyForCleanup {
		return
	}

	repo, _ := d.state.GetRepo(repoName)
	if repo == nil {
		return
	}

	// Detect mid-rebase or mid-merge state and send targeted help instead of
	// generic directives. Workers stuck in rebase loops burn tokens retrying
	// the same failed rebase without specific guidance.
	if agent.WorktreePath != "" {
		gitDir := resolveGitDir(agent.WorktreePath)
		if gitDir != "" {
			if isDir(filepath.Join(gitDir, "rebase-merge")) || isDir(filepath.Join(gitDir, "rebase-apply")) {
				message := "[daemon] You are stuck in a mid-rebase state. Run: git rebase --abort && git fetch origin main && git rebase origin/main — then resolve conflicts one at a time. If conflicts persist after 2 attempts, run: oat agent complete --failure-reason 'unresolvable merge conflict'"
				if err := d.backend.SendMessage(d.ctx, repo.SessionName, agent.WindowName, message); err != nil {
					d.logger.Error("Failed to send rebase help to worker %s: %v", agentName, err)
				}
				d.logger.Info("Worker %s/%s stuck in mid-rebase, sent targeted help", repoName, agentName)
				if err := d.state.ModifyAgent(repoName, agentName, func(a *state.Agent) {
					a.LastNudge = now
				}); err != nil {
					d.logger.Error("Failed to update worker %s state: %v", agentName, err)
				}
				return
			}
			if fileExists(filepath.Join(gitDir, "MERGE_HEAD")) {
				message := "[daemon] You are stuck in a mid-merge state. Run: git merge --abort && git fetch origin main && git rebase origin/main — then resolve conflicts. If this keeps happening, run: oat agent complete --failure-reason 'unresolvable merge conflict'"
				if err := d.backend.SendMessage(d.ctx, repo.SessionName, agent.WindowName, message); err != nil {
					d.logger.Error("Failed to send merge help to worker %s: %v", agentName, err)
				}
				d.logger.Info("Worker %s/%s stuck in mid-merge, sent targeted help", repoName, agentName)
				if err := d.state.ModifyAgent(repoName, agentName, func(a *state.Agent) {
					a.LastNudge = now
				}); err != nil {
					d.logger.Error("Failed to update worker %s state: %v", agentName, err)
				}
				return
			}
		}
	}

	// If we already know the PR number, check if it's been merged/closed.
	// This catches cases where the branch was deleted after squash merge
	// and getWorkerPR (which only finds open PRs) would miss it.
	if agent.PRNumber > 0 {
		prNum := strconv.Itoa(agent.PRNumber)
		result, ok := d.queryPRStatus(repoPath, prNum, repoName, agentName)
		if ok && (result.State == "MERGED" || result.State == "CLOSED") {
			d.logger.Info("Worker %s/%s PR #%d already %s, auto-completing", repoName, agentName, agent.PRNumber, result.State)
			summary := fmt.Sprintf("Auto-completed by daemon (PR #%d %s, worker did not self-complete)", agent.PRNumber, strings.ToLower(result.State))
			d.autoCompleteWorker(repoName, agentName, agent, summary)
			return
		}
	}

	branchName := "work/" + agentName
	branchPushed := d.getBranchSHA(repoPath, branchName) != ""
	pr := d.getWorkerPR(repoPath, branchName)

	if branchPushed && pr != nil {
		d.logger.Info("Worker %s/%s has a PR (#%d), auto-completing", repoName, agentName, pr.Number)
		summary := pr.Title
		if summary == "" {
			summary = "Auto-completed by daemon (PR detected, worker did not self-complete)"
		}
		d.autoCompleteWorker(repoName, agentName, agent, summary)
		return
	}

	if branchPushed && pr == nil {
		message := "[daemon] Your branch is pushed but you have no PR. Run `oat worker request-review` and `oat agent waiting` NOW."
		if err := d.backend.SendMessage(d.ctx, repo.SessionName, agent.WindowName, message); err != nil {
			d.logger.Error("Failed to send directive nudge to worker %s: %v", agentName, err)
		}
		if err := d.state.ModifyAgent(repoName, agentName, func(a *state.Agent) {
			a.LastNudge = now
		}); err != nil {
			d.logger.Error("Failed to update worker %s state: %v", agentName, err)
		}
		return
	}

	// No branch pushed — check worktree for uncommitted changes
	if agent.WorktreePath != "" {
		validPath := regexp.MustCompile(`^[a-zA-Z0-9_\-\./\\]+$`)
		if !validPath.MatchString(agent.WorktreePath) {
			d.logger.Error("Invalid worktree path for worker %s: %s", agentName, agent.WorktreePath)
			return
		}
		cmd := exec.CommandContext(d.ctx, "git", "status", "--porcelain")
		cmd.Dir = agent.WorktreePath
		output, err := cmd.Output()
		if err == nil && len(strings.TrimSpace(string(output))) > 0 {
			message := "[daemon] You have uncommitted changes but have not pushed. Run: git add -A && git commit -m 'work in progress' && git push -u origin " + branchName + " && oat worker request-review && oat agent waiting"
			if err := d.backend.SendMessage(d.ctx, repo.SessionName, agent.WindowName, message); err != nil {
				d.logger.Error("Failed to send directive nudge to worker %s: %v", agentName, err)
			}
			if err := d.state.ModifyAgent(repoName, agentName, func(a *state.Agent) {
				a.LastNudge = now
			}); err != nil {
				d.logger.Error("Failed to update worker %s state: %v", agentName, err)
			}
			return
		}
	}

	// No activity at all — send a warning
	message := "[daemon] You have made no progress. If you cannot complete this task, run `oat agent complete --failure-reason 'unable to complete'` now."
	if err := d.backend.SendMessage(d.ctx, repo.SessionName, agent.WindowName, message); err != nil {
		d.logger.Error("Failed to send warning to worker %s: %v", agentName, err)
	}
	if err := d.state.ModifyAgent(repoName, agentName, func(a *state.Agent) {
		a.LastNudge = now
	}); err != nil {
		d.logger.Error("Failed to update worker %s state: %v", agentName, err)
	}
}

// closeAssociatedIssue closes the GitHub issue tied to a worker when the worker
// is auto-completed without a merged PR. Failures are logged but do not block
// the auto-complete flow.
func (d *Daemon) closeAssociatedIssue(repoName, agentName string, agent state.Agent, reason string) {
	if agent.IssueNumber == "" {
		return
	}
	validIssueNumber := regexp.MustCompile(`^[0-9]+$`)
	if !validIssueNumber.MatchString(agent.IssueNumber) {
		d.logger.Error("Invalid issue number for %s/%s: %s", repoName, agentName, agent.IssueNumber)
		return
	}
	repoPath := d.paths.RepoDir(repoName)
	comment := fmt.Sprintf("Auto-closed by daemon: %s\n\n— %s", reason, agentName)
	validComment := regexp.MustCompile(`^[a-zA-Z0-9_\-\.\s:,;!()\n/'"]+$`)
	if !validComment.MatchString(comment) {
		d.logger.Error("Invalid comment for issue #%s for %s/%s", agent.IssueNumber, repoName, agentName)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "issue", "close", agent.IssueNumber, "--comment", comment)
	cmd.Dir = repoPath
	if err := cmd.Run(); err != nil {
		d.logger.Warn("Failed to close issue #%s for %s/%s: %v", agent.IssueNumber, repoName, agentName, err)
	} else {
		d.logger.Info("Closed issue #%s for %s/%s: %s", agent.IssueNumber, repoName, agentName, reason)
	}
}

// cleanupVerifyAgent marks the associated verify-<worker> agent for cleanup if it exists.
// Called when a worker is completed, auto-completed, or force-removed to prevent
// orphaned verify agents from running indefinitely.
func (d *Daemon) cleanupVerifyAgent(repoName, workerName string) {
	verifierName := "verify-" + workerName
	verifier, exists := d.state.GetAgent(repoName, verifierName)
	if !exists || verifier.ReadyForCleanup {
		return
	}
	if err := d.state.ModifyAgent(repoName, verifierName, func(a *state.Agent) {
		a.ReadyForCleanup = true
		a.ReadyForCleanupAt = time.Now()
	}); err != nil {
		d.logger.Error("Failed to mark verify agent %s/%s for cleanup: %v", repoName, verifierName, err)
		return
	}
	d.logger.Info("Marked verify agent %s/%s for cleanup (worker %s being removed/completed)", repoName, verifierName, workerName)
}

// autoCompleteWorker marks a worker as ready for cleanup and sends completion notifications.
// Safe to call multiple times — silently returns if the worker is already completed.
func (d *Daemon) autoCompleteWorker(repoName, agentName string, agent state.Agent, summary string) {
	// Guard: skip if already completed to prevent duplicate notifications.
	if current, exists := d.state.GetAgent(repoName, agentName); exists && current.ReadyForCleanup {
		return
	}
	d.cleanupVerifyAgent(repoName, agentName)
	if err := d.state.ModifyAgent(repoName, agentName, func(a *state.Agent) {
		a.ReadyForCleanup = true
		a.ReadyForCleanupAt = time.Now()
		a.Summary = summary
	}); err != nil {
		d.logger.Error("Failed to auto-complete worker %s/%s: %v", repoName, agentName, err)
		return
	}

	d.logger.Info("Auto-completed worker %s/%s: %s", repoName, agentName, summary)

	msgMgr := d.getMessageManager()

	// If the PR is already merged, merge-queue has nothing to do -- only notify supervisor.
	prAlreadyMerged := strings.Contains(summary, "merged")
	if prAlreadyMerged {
		supervisorMsg := fmt.Sprintf("[daemon] Worker '%s' auto-completed (PR merged, worker did not self-complete after nudges).", agentName)
		if _, err := msgMgr.Send(repoName, "daemon", "supervisor", supervisorMsg); err != nil {
			d.logger.Error("Failed to send auto-complete notification to supervisor: %v", err)
		}
	} else {
		supervisorMsg := fmt.Sprintf("[daemon] Worker '%s' auto-completed (did not self-complete). May have a PR to merge. Merge-queue has also been notified.", agentName)
		if _, err := msgMgr.Send(repoName, "daemon", "supervisor", supervisorMsg); err != nil {
			d.logger.Error("Failed to send auto-complete notification to supervisor: %v", err)
		}

		mergeQueueMsg := fmt.Sprintf("[daemon] Worker '%s' auto-completed and may have a PR. Check for new PRs to process.", agentName)
		if _, err := msgMgr.Send(repoName, "daemon", "merge-queue", mergeQueueMsg); err != nil {
			d.logger.Error("Failed to send auto-complete notification to merge-queue: %v", err)
		}
	}

	d.triggerRouteMessages()
}

// forceRemoveWorker force-removes a stuck worker at the hard cap, preserving any work if possible.
func (d *Daemon) forceRemoveWorker(repoName, repoPath, agentName string, agent state.Agent) {
	// Check known PR number first (catches merged PRs whose branches were deleted)
	if agent.PRNumber > 0 {
		prNum := strconv.Itoa(agent.PRNumber)
		result, ok := d.queryPRStatus(repoPath, prNum, repoName, agentName)
		if ok && (result.State == "MERGED" || result.State == "CLOSED") {
			d.logger.Info("Worker %s/%s PR #%d already %s at force-remove, auto-completing instead", repoName, agentName, agent.PRNumber, result.State)
			summary := fmt.Sprintf("Auto-completed by daemon (PR #%d %s, worker did not self-complete)", agent.PRNumber, strings.ToLower(result.State))
			d.autoCompleteWorker(repoName, agentName, agent, summary)
			return
		}
	}

	branchName := "work/" + agentName
	pr := d.getWorkerPR(repoPath, branchName)

	if pr != nil {
		summary := pr.Title
		if summary == "" {
			summary = "Auto-completed by daemon (PR detected, worker did not self-complete)"
		}
		d.autoCompleteWorker(repoName, agentName, agent, summary)
		return
	}

	branchPushed := d.getBranchSHA(repoPath, branchName) != ""
	reason := "Force-removed by daemon: exceeded max nudge limit"
	if branchPushed {
		reason = "Force-removed by daemon: branch pushed but no PR created, exceeded max nudge limit"
	}

	d.cleanupVerifyAgent(repoName, agentName)
	if err := d.state.ModifyAgent(repoName, agentName, func(a *state.Agent) {
		a.ReadyForCleanup = true
		a.ReadyForCleanupAt = time.Now()
		a.FailureReason = reason
	}); err != nil {
		d.logger.Error("Failed to force-remove worker %s/%s: %v", repoName, agentName, err)
		return
	}

	d.logger.Warn("Force-removed worker %s/%s: %s", repoName, agentName, reason)

	msgMgr := d.getMessageManager()
	task := agent.Task
	if task == "" {
		task = "unknown task"
	}
	supervisorMsg := fmt.Sprintf("[daemon] Worker '%s' was force-removed after %d nudges without completion (task: %s). Consider spawning a new worker if the task is still needed.", agentName, stuckMaxNudge, task)
	if _, err := msgMgr.Send(repoName, "daemon", "supervisor", supervisorMsg); err != nil {
		d.logger.Error("Failed to notify supervisor about force-removed worker %s: %v", agentName, err)
	}

	d.triggerRouteMessages()
}

// rejectionCapReached handles the case where a worker has been rejected too many
// times. It auto-completes the worker and sends an escalation to the supervisor
// advising reassignment (potentially to a different model).
func (d *Daemon) rejectionCapReached(repoName, workerName string, agent state.Agent, rejectionCount int, lastReason string) {
	task := agent.Task
	if task == "" {
		task = "unknown task"
	}
	model := agent.Model
	if model == "" {
		model = "default"
	}

	d.logger.Warn("Worker %s/%s hit rejection cap (%d rejections), escalating to supervisor", repoName, workerName, rejectionCount)

	// Notify supervisor with context for reassignment
	msgMgr := d.getMessageManager()
	supervisorMsg := fmt.Sprintf(
		"[daemon] Worker '%s' was auto-completed after %d verification rejections (cap: %d). "+
			"The worker could not converge on an acceptable solution.\n\n"+
			"Task: %s\nModel: %s\nLast rejection reason: %s\n\n"+
			"Consider asking workspace to spawn a replacement worker for this task, "+
			"preferably with a different/stronger model:\n"+
			"  oat message send workspace \"Worker %s failed after %d rejections on task: %s. Spawn a replacement with a different model.\"",
		workerName, rejectionCount, maxRejections,
		task, model, lastReason,
		workerName, rejectionCount, task,
	)
	if _, err := msgMgr.Send(repoName, "daemon", "supervisor", supervisorMsg); err != nil {
		d.logger.Error("Failed to send rejection-cap escalation to supervisor for %s/%s: %v", repoName, workerName, err)
	}

	// Auto-complete the worker
	summary := fmt.Sprintf("Auto-completed: exceeded max rejection limit (%d rejections, cap: %d)", rejectionCount, maxRejections)
	d.autoCompleteWorker(repoName, workerName, agent, summary)
}

// resolveGitDir returns the actual .git directory for a worktree path.
// For regular repos this is <path>/.git, for git worktrees .git is a file
// containing "gitdir: <actual-path>".
func resolveGitDir(worktreePath string) string {
	gitPath := filepath.Join(worktreePath, ".git")
	info, err := os.Stat(gitPath)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		return gitPath
	}
	// .git is a file — read the gitdir pointer
	data, err := os.ReadFile(gitPath)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(data))
	if strings.HasPrefix(line, "gitdir: ") {
		resolved := strings.TrimPrefix(line, "gitdir: ")
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(worktreePath, resolved)
		}
		return resolved
	}
	return ""
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
