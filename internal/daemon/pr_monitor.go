package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

// workerDormancyCap is the maximum time a worker can stay dormant waiting for a PR.
// Configurable via OAT_WORKER_DORMANCY_CAP_MINUTES (default 15).
var workerDormancyCap = time.Duration(getEnvInt("OAT_WORKER_DORMANCY_CAP_MINUTES", 15)) * time.Minute

// fastMergeEnabled controls whether the daemon merges green PRs directly
// instead of waiting for the merge-queue LLM agent. When true, the daemon
// runs `gh pr merge --squash` as soon as CI passes and the PR is mergeable,
// bypassing the merge-queue entirely. The merge-queue still runs for
// escalation (CI failures, conflicts) — this only affects the happy path.
// Configurable via OAT_FAST_MERGE (default true). Set to "false" or "0" to disable.
var fastMergeEnabled = os.Getenv("OAT_FAST_MERGE") != "false" && os.Getenv("OAT_FAST_MERGE") != "0"

// prWakeThreshold is the number of conflict/CI wakes before the daemon stops
// waking the worker and escalates to the supervisor instead. 5 retries gives
// the worker enough chances for a genuine fix before escalating.
const prWakeThreshold = 5

// prViewResult holds the parsed output of `gh pr view --json ...`
type prViewResult struct {
	Mergeable         string            `json:"mergeable"`
	State             string            `json:"state"`
	StatusCheckRollup []prCheckResult   `json:"statusCheckRollup"`
	Comments          []prCommentResult `json:"comments"`
}

type prCheckResult struct {
	Name       string `json:"name"`
	State      string `json:"state"`
	Conclusion string `json:"conclusion"`
}

type prCommentResult struct {
	DatabaseID int64  `json:"databaseId"`
	Body       string `json:"body"`
	Author     struct {
		Login string `json:"login"`
	} `json:"author"`
}

// checkWorkerPRs monitors PRs for all dormant workers and wakes them when action is needed.
// Also checks active workers for merge conflicts and CI failures (via checkActiveWorkerPRIssues),
// but skips the active-worker check if any dormant workers were just woken this cycle
// to give them time to process the wake message before receiving a second notification.
func (d *Daemon) checkWorkerPRs() {
	workersWoken := false
	repoNames := d.state.ListRepos()
	for _, repoName := range repoNames {
		repo, exists := d.state.GetRepo(repoName)
		if !exists || repo == nil {
			continue
		}

		repoPath := d.paths.RepoDir(repoName)

		for agentName, agent := range repo.Agents {
			if agent.Type != state.AgentTypeWorker || !agent.WaitingForPR || agent.ReadyForCleanup {
				continue
			}

			// Check dormancy cap
			if !agent.WaitingForPRSince.IsZero() && time.Since(agent.WaitingForPRSince) > workerDormancyCap {
				// If the worker has a green, mergeable PR, force-merge it instead of timing out.
				// A green PR past the dormancy cap signals the merge-queue is broken.
				if agent.PRNumber > 0 {
					prNum := strconv.Itoa(agent.PRNumber)
					result, ok := d.queryPRStatus(repoPath, prNum, repoName, agentName)
					if ok && result.State == "OPEN" && result.Mergeable == "MERGEABLE" && allChecksPass(result.StatusCheckRollup) {
						d.logger.Warn("Worker %s/%s past dormancy cap but PR #%d is green -- force-merging (merge-queue failed to process)", repoName, agentName, agent.PRNumber)
						d.forceMergeWorkerPR(repoName, repoPath, agentName, agent)
						continue
					}
				}
				d.handleDormancyTimeout(repoName, repoPath, agentName, agent)
				continue
			}

			// Discover PR if not yet known
			if agent.PRNumber == 0 {
				branchName := "work/" + agentName
				pr := d.getWorkerPR(repoPath, branchName)
				if pr == nil {
					continue
				}
				agent.PRNumber = pr.Number
				if err := d.state.ModifyAgent(repoName, agentName, func(a *state.Agent) {
					a.PRNumber = pr.Number
				}); err != nil {
					d.logger.Error("Failed to update PR number for %s/%s: %v", repoName, agentName, err)
				}
			}

			if d.checkSingleWorkerPR(repoName, repoPath, agentName, agent) {
				workersWoken = true
			}
		}
	}

	if !workersWoken {
		d.checkActiveWorkerPRIssues()
	}

	d.backfillRecoveredTasks()
}

// backfillRecoveredTasks checks task history entries marked "failed" that have
// a PR number. If the PR has since been merged, the entry is updated to
// "recovered" — distinguishing workers that failed but whose work landed anyway.
func (d *Daemon) backfillRecoveredTasks() {
	repoNames := d.state.ListRepos()
	for _, repoName := range repoNames {
		repo, exists := d.state.GetRepo(repoName)
		if !exists || repo == nil {
			continue
		}

		repoPath := d.paths.RepoDir(repoName)

		history, err := d.state.GetTaskHistory(repoName, 50)
		if err != nil {
			continue
		}

		for _, entry := range history {
			if entry.Status != state.TaskStatusFailed || entry.PRNumber == 0 {
				continue
			}

			prNum := strconv.Itoa(entry.PRNumber)
			result, ok := d.queryPRStatus(repoPath, prNum, repoName, entry.Name)
			if !ok {
				continue
			}

			if result.State == "MERGED" {
				prURL := fmt.Sprintf("%s/pull/%d", repo.GithubURL, entry.PRNumber)
				if err := d.state.UpdateTaskHistoryStatus(repoName, entry.Name, state.TaskStatusRecovered, prURL, entry.PRNumber); err != nil {
					d.logger.Warn("Failed to backfill recovered status for %s: %v", entry.Name, err)
				} else {
					d.logger.Info("Backfilled task %s/%s to 'recovered' (PR #%d merged)", repoName, entry.Name, entry.PRNumber)
				}
			}
		}
	}
}

// checkActiveWorkerPRIssues checks for merge conflicts and CI failures on PRs
// owned by active (non-dormant) workers. Dormant workers are handled by
// checkSingleWorkerPR; this covers the gap where a worker created a PR but
// never went dormant, or was woken and hasn't gone dormant again.
func (d *Daemon) checkActiveWorkerPRIssues() {
	repoNames := d.state.ListRepos()
	for _, repoName := range repoNames {
		repo, exists := d.state.GetRepo(repoName)
		if !exists || repo == nil {
			continue
		}

		repoPath := d.paths.RepoDir(repoName)

		for agentName, agent := range repo.Agents {
			if agent.Type != state.AgentTypeWorker || agent.IsDormant() ||
				agent.ReadyForCleanup || agent.PRNumber == 0 {
				continue
			}

			key := fmt.Sprintf("%s/%d", repoName, agent.PRNumber)

			d.conflictNotifiedMu.Lock()
			conflictAlreadyNotified := d.conflictNotified[key]
			d.conflictNotifiedMu.Unlock()

			d.ciFailureNotifiedMu.Lock()
			ciAlreadyNotified := d.ciFailureNotified[key]
			d.ciFailureNotifiedMu.Unlock()

			if conflictAlreadyNotified && ciAlreadyNotified {
				continue
			}

			prNum := strconv.Itoa(agent.PRNumber)
			result, ok := d.queryPRStatus(repoPath, prNum, repoName, agentName)
			if !ok {
				continue
			}

			if !conflictAlreadyNotified && result.Mergeable == "CONFLICTING" {
				d.conflictNotifiedMu.Lock()
				if d.conflictNotified == nil {
					d.conflictNotified = make(map[string]bool)
				}
				d.conflictNotified[key] = true
				d.conflictNotifiedMu.Unlock()

				msg := fmt.Sprintf("[daemon] Your PR #%d has a merge conflict. Run `git fetch origin main && git rebase origin/main`, resolve conflicts, then `git push --force-with-lease`.", agent.PRNumber)
				if err := d.backend.SendMessage(d.ctx, repo.SessionName, agent.WindowName, msg); err != nil {
					d.logger.Error("Failed to notify active worker %s/%s about conflict: %v", repoName, agentName, err)
				} else {
					d.logger.Info("Notified active worker %s/%s about merge conflict on PR #%d", repoName, agentName, agent.PRNumber)
				}
			}

			if !ciAlreadyNotified && hasFailedChecks(result.StatusCheckRollup) {
				d.ciFailureNotifiedMu.Lock()
				if d.ciFailureNotified == nil {
					d.ciFailureNotified = make(map[string]bool)
				}
				d.ciFailureNotified[key] = true
				d.ciFailureNotifiedMu.Unlock()

				msg := fmt.Sprintf("[daemon] CI failed on your PR #%d. First run `git fetch origin main && git rebase origin/main` to pick up any fixes that landed on main. Then run `gh run list --branch work/%s --limit 1` to find the run and `gh run view <run-id> --log-failed` to see failures. Fix and push.", agent.PRNumber, agentName)
				if err := d.backend.SendMessage(d.ctx, repo.SessionName, agent.WindowName, msg); err != nil {
					d.logger.Error("Failed to notify active worker %s/%s about CI failure: %v", repoName, agentName, err)
				} else {
					d.logger.Info("Notified active worker %s/%s about CI failure on PR #%d", repoName, agentName, agent.PRNumber)
				}
			}
		}
	}
}

// checkSingleWorkerPR checks a single worker's PR status and wakes the worker if needed.
// checkSingleWorkerPR checks a dormant worker's PR status and wakes it if action is needed.
// Returns true if the worker was woken.
func (d *Daemon) checkSingleWorkerPR(repoName, repoPath, agentName string, agent state.Agent) bool {
	prNum := strconv.Itoa(agent.PRNumber)

	result, ok := d.queryPRStatus(repoPath, prNum, repoName, agentName)
	if !ok {
		return false
	}

	switch {
	case result.State == "MERGED":
		d.resetPRWakeCount(repoName, agentName)
		d.wakeWorker(repoName, agentName, agent,
			fmt.Sprintf("[daemon] Your PR #%d has been merged. Run `oat agent complete` now.", agent.PRNumber))
		if err := d.state.ModifyAgent(repoName, agentName, func(a *state.Agent) {
			a.WokenForMergedPRAt = time.Now()
		}); err != nil {
			d.logger.Warn("Failed to record WokenForMergedPRAt for %s/%s: %v", repoName, agentName, err)
		}
		mergedPR := agent.PRNumber
		d.safeGo("post-merge-ci-check", func() {
			time.Sleep(90 * time.Second)
			d.checkMainCIAfterMerge(repoName, mergedPR)
		})
		return true

	case result.State == "CLOSED":
		d.resetPRWakeCount(repoName, agentName)
		d.wakeWorker(repoName, agentName, agent,
			fmt.Sprintf("[daemon] Your PR #%d was closed without merging. Investigate or run `oat agent complete`.", agent.PRNumber))
		return true

	case result.Mergeable == "CONFLICTING":
		tripped := d.incrementPRWakeCount(repoName, agentName)
		if tripped {
			d.escalatePRWakeToSupervisor(repoName, agentName, agent.PRNumber)
			return false
		}
		msg := fmt.Sprintf("[daemon] Your PR #%d has a merge conflict. Run `git fetch origin main && git rebase origin/main`, resolve conflicts, then `git push --force-with-lease`. After fixing, run `oat agent waiting` again.", agent.PRNumber)
		if hasFailedChecks(result.StatusCheckRollup) {
			msg += fmt.Sprintf(" Note: CI was also failing before these conflicts. After rebasing, check CI status (`gh run list --branch work/%s --limit 1`) and fix any failures before running `oat agent waiting`.", agentName)
		}
		d.wakeWorker(repoName, agentName, agent, msg)
		return true

	case hasFailedChecks(result.StatusCheckRollup):
		tripped := d.incrementPRWakeCount(repoName, agentName)
		if tripped {
			d.escalatePRWakeToSupervisor(repoName, agentName, agent.PRNumber)
			return false
		}
		d.wakeWorker(repoName, agentName, agent,
			fmt.Sprintf("[daemon] CI failed on your PR #%d. You MUST fix this before going dormant. Steps: 1) `git fetch origin main && git rebase origin/main` 2) Check failures: `gh run list --branch work/%s --limit 1` then `gh run view <run-id> --log-failed` 3) Fix the code and `git push`. Do NOT run `oat agent waiting` until your fix is pushed.", agent.PRNumber, agentName))
		return true

	case result.Mergeable == "UNKNOWN":
		d.logger.Debug("PR #%d for %s/%s: mergeable=UNKNOWN, will recheck", agent.PRNumber, repoName, agentName)

	default:
		// No state/merge issues; check for CI failures via fallback if we had no check data
		if len(result.StatusCheckRollup) == 0 {
			if d.hasCIFailuresFallback(repoPath, agentName, agent.PRNumber) {
				tripped := d.incrementPRWakeCount(repoName, agentName)
				if tripped {
					d.escalatePRWakeToSupervisor(repoName, agentName, agent.PRNumber)
					return false
				}
				d.wakeWorker(repoName, agentName, agent,
					fmt.Sprintf("[daemon] CI failed on your PR #%d. You MUST fix this before going dormant. Steps: 1) `git fetch origin main && git rebase origin/main` 2) Check failures: `gh run list --branch work/%s --limit 1` then `gh run view <run-id> --log-failed` 3) Fix the code and `git push`. Do NOT run `oat agent waiting` until your fix is pushed.", agent.PRNumber, agentName))
				return true
			}
		}

		// PR is green and mergeable -- reset circuit breaker and merge or notify
		ciPassed := allChecksPassed(result.StatusCheckRollup)
		if !ciPassed && len(result.StatusCheckRollup) == 0 {
			ciPassed = d.hasCIPassedFallback(repoPath, agentName)
		}
		if result.Mergeable == "MERGEABLE" && ciPassed {
			d.resetPRWakeCount(repoName, agentName)
			if fastMergeEnabled {
				d.fastMergeWorkerPR(repoName, repoPath, agentName, agent)
				return true
			}
			d.notifyMergeQueueAboutPR(repoName, agentName, agent)
		}

		d.checkPRActivity(repoName, repoPath, agentName, agent)
	}
	return false
}

// incrementPRWakeCount increments the conflict/CI wake counter for a worker
// and returns true if the threshold has been reached (circuit breaker tripped).
func (d *Daemon) incrementPRWakeCount(repoName, agentName string) bool {
	key := fmt.Sprintf("%s/%s", repoName, agentName)
	d.prWakeCountMu.Lock()
	defer d.prWakeCountMu.Unlock()
	d.prWakeCount[key]++
	return d.prWakeCount[key] >= prWakeThreshold
}

// resetPRWakeCount resets the circuit breaker counter (e.g., when PR state improves).
func (d *Daemon) resetPRWakeCount(repoName, agentName string) {
	key := fmt.Sprintf("%s/%s", repoName, agentName)
	d.prWakeCountMu.Lock()
	defer d.prWakeCountMu.Unlock()
	delete(d.prWakeCount, key)
	delete(d.prWakeEscalated, key)
}

// escalatePRWakeToSupervisor sends a one-time escalation message to the supervisor
// when a worker's conflict/CI wake count exceeds the threshold. Returns true if
// already escalated (to skip waking the worker).
func (d *Daemon) escalatePRWakeToSupervisor(repoName, agentName string, prNumber int) bool {
	key := fmt.Sprintf("%s/%s", repoName, agentName)
	d.prWakeCountMu.Lock()
	if d.prWakeEscalated[key] {
		d.prWakeCountMu.Unlock()
		return true
	}
	d.prWakeEscalated[key] = true
	d.prWakeCountMu.Unlock()

	msg := fmt.Sprintf(
		"[daemon] Worker '%s' has been woken %d+ times for conflict/CI issues on PR #%d without improvement. Please investigate and escalate:\n"+
			"1. Check what the worker has been doing: review PR #%d's recent commits (`gh pr view %d --json commits --jq '.commits[-3:]'`) and check messages (`oat message list`)\n"+
			"2. Check CI failures: `gh pr checks %d`\n"+
			"3. Check if other unmerged PRs are blocking this one (circular dependency)\n"+
			"4. Message the worker with your diagnosis of why CI keeps failing and see if they can resolve it\n"+
			"5. If the worker cannot fix it alone, escalate to workspace: `oat message send workspace \"ESCALATION: [describe the circular dependency and which PRs are involved]. Create a single consolidated fix issue and spawn a worker for it.\"`\n"+
			"6. If the worker cannot make further progress, complete it: `oat agent complete --worker %s`\n"+
			"Do NOT run `oat agent complete` without the --worker flag.",
		agentName, prWakeThreshold, prNumber,
		prNumber, prNumber,
		prNumber,
		agentName,
	)
	msgMgr := d.getMessageManager()
	if _, err := msgMgr.Send(repoName, "daemon", "supervisor", msg); err != nil {
		d.logger.Error("Failed to escalate PR wake circuit breaker for %s/%s to supervisor: %v", repoName, agentName, err)
	} else {
		d.logger.Info("PR wake circuit breaker: escalated %s/%s (PR #%d) to supervisor after %d wakes", repoName, agentName, prNumber, prWakeThreshold)
	}
	return true
}

// queryPRStatus tries to get PR status, falling back gracefully if statusCheckRollup
// is inaccessible due to token permissions.
func (d *Daemon) queryPRStatus(repoPath, prNum, repoName, agentName string) (prViewResult, bool) {
	validPRNum := regexp.MustCompile(`^[0-9]+$`)
	if !validPRNum.MatchString(prNum) {
		d.logger.Debug("Invalid PR number for %s/%s: %s", repoName, agentName, prNum)
		return prViewResult{}, false
	}
	// Try with statusCheckRollup first
	cmd := exec.CommandContext(d.ctx, "gh", "pr", "view", prNum,
		"--json", "mergeable,state,statusCheckRollup")
	cmd.Dir = repoPath
	output, err := cmd.Output()
	if err == nil {
		var result prViewResult
		if err := json.Unmarshal(output, &result); err == nil {
			return result, true
		}
	}

	// Fallback: query without statusCheckRollup (works with limited token scopes)
	cmd = exec.CommandContext(d.ctx, "gh", "pr", "view", prNum,
		"--json", "mergeable,state")
	cmd.Dir = repoPath
	output, err = cmd.Output()
	if err != nil {
		d.logger.Debug("Failed to query PR #%s for %s/%s: %v", prNum, repoName, agentName, err)
		return prViewResult{}, false
	}

	var result prViewResult
	if err := json.Unmarshal(output, &result); err != nil {
		d.logger.Warn("Failed to parse PR status for %s/%s: %v", repoName, agentName, err)
		return prViewResult{}, false
	}

	return result, true
}

// hasCIFailuresFallback checks CI status via `gh run list` when statusCheckRollup is unavailable.
func (d *Daemon) hasCIFailuresFallback(repoPath, agentName string, prNumber int) bool {
	branch := "work/" + agentName
	cmd := exec.CommandContext(d.ctx, "gh", "run", "list",
		"--branch", branch, "--limit", "1", "--json", "conclusion,status")
	cmd.Dir = repoPath
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	var runs []struct {
		Conclusion string `json:"conclusion"`
		Status     string `json:"status"`
	}
	if err := json.Unmarshal(output, &runs); err != nil || len(runs) == 0 {
		return false
	}

	run := runs[0]
	return run.Status == "completed" &&
		(run.Conclusion == "failure" || run.Conclusion == "canceled" || run.Conclusion == "timed_out")
}

// hasFailedChecks returns true if any status check has failed.
func hasFailedChecks(checks []prCheckResult) bool {
	for _, check := range checks {
		if check.Conclusion == "FAILURE" || check.Conclusion == "ERROR" ||
			check.Conclusion == "CANCELED" || check.Conclusion == "TIMED_OUT" ||
			check.State == "FAILURE" || check.State == "ERROR" {
			return true
		}
	}
	return false
}

// allChecksPassed returns true if there are status checks and all have succeeded.
func allChecksPassed(checks []prCheckResult) bool {
	if len(checks) == 0 {
		return false
	}
	for _, check := range checks {
		if check.Conclusion != "SUCCESS" && check.State != "SUCCESS" {
			return false
		}
	}
	return true
}

// hasCIPassedFallback checks if CI passed via `gh run list` when statusCheckRollup is unavailable.
func (d *Daemon) hasCIPassedFallback(repoPath, agentName string) bool {
	validAgentName := regexp.MustCompile(`^[a-zA-Z0-9_\-]+$`)
	if !validAgentName.MatchString(agentName) {
		return false
	}
	branch := "work/" + agentName
	cmd := exec.CommandContext(d.ctx, "gh", "run", "list",
		"--branch", branch, "--limit", "1", "--json", "conclusion,status")
	cmd.Dir = repoPath
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	var runs []struct {
		Conclusion string `json:"conclusion"`
		Status     string `json:"status"`
	}
	if err := json.Unmarshal(output, &runs); err != nil || len(runs) == 0 {
		return false
	}

	return runs[0].Status == "completed" && runs[0].Conclusion == "success"
}

// notifyMergeQueueAboutPR sends a message to the merge-queue that a PR is ready to merge.
// Only notifies once per PR to avoid spamming the merge-queue every monitor cycle.
func (d *Daemon) notifyMergeQueueAboutPR(repoName, agentName string, agent state.Agent) {
	// Dedup: only notify once per PR by checking a daemon-level map
	d.prGreenNotifiedMu.Lock()
	if d.prGreenNotified == nil {
		d.prGreenNotified = make(map[string]bool)
	}
	key := fmt.Sprintf("%s/%d", repoName, agent.PRNumber)
	if d.prGreenNotified[key] {
		d.prGreenNotifiedMu.Unlock()
		return
	}
	d.prGreenNotified[key] = true
	d.prGreenNotifiedMu.Unlock()

	// Clear idle mode so the merge-queue will actually receive and process the message
	if err := d.state.SetRepoIdleMode(repoName, false); err != nil {
		d.logger.Warn("Failed to clear idle mode for %s: %v", repoName, err)
	}

	msgMgr := d.getMessageManager()
	msg := fmt.Sprintf("[daemon] PR #%d from worker '%s' is green and mergeable. Please merge it.", agent.PRNumber, agentName)
	if _, err := msgMgr.Send(repoName, "daemon", "merge-queue", msg); err != nil {
		d.logger.Error("Failed to notify merge-queue about green PR #%d: %v", agent.PRNumber, err)
	} else {
		d.logger.Info("Notified merge-queue that PR #%d is ready to merge", agent.PRNumber)
	}
	d.triggerRouteMessages()
	// Also trigger a wake so the merge-queue gets nudged immediately to process
	// the message, rather than waiting up to 1 minute for the next wake tick.
	d.triggerWake()
}

// buildMergeQueueCISummary builds a multi-line summary of open PR CI statuses for a repo.
// Used to include CI status in periodic nudge messages to the merge-queue.
func (d *Daemon) buildMergeQueueCISummary(repoPath string) string {
	cmd := exec.CommandContext(d.ctx, "gh", "pr", "list", "--state", "open", "--json", "number,headRefName", "--limit", "50")
	cmd.Dir = repoPath
	output, err := cmd.Output()
	if err != nil {
		return "Open PRs: (unable to query)"
	}

	var prs []struct {
		Number      int    `json:"number"`
		HeadRefName string `json:"headRefName"`
	}
	if err := json.Unmarshal(output, &prs); err != nil || len(prs) == 0 {
		return "Open PRs: none"
	}

	lines := []string{"Open PRs:"}
	for _, pr := range prs {
		ciStatus := d.getBranchCIStatus(repoPath, pr.HeadRefName)
		lines = append(lines, fmt.Sprintf("  PR #%d (%s) CI: %s", pr.Number, pr.HeadRefName, ciStatus))
	}
	return strings.Join(lines, "\n")
}

// getBranchCIStatus returns a human-readable CI status for a branch.
func (d *Daemon) getBranchCIStatus(repoPath, branch string) string {
	validBranch := regexp.MustCompile(`^[a-zA-Z0-9_\-/]+$`)
	if !validBranch.MatchString(branch) {
		return "unknown"
	}
	cmd := exec.CommandContext(d.ctx, "gh", "run", "list", "--branch", branch, "--limit", "1", "--json", "conclusion,status")
	cmd.Dir = repoPath
	output, err := cmd.Output()
	if err != nil {
		return "unknown"
	}

	var runs []struct {
		Conclusion string `json:"conclusion"`
		Status     string `json:"status"`
	}
	if err := json.Unmarshal(output, &runs); err != nil || len(runs) == 0 {
		return "no runs"
	}

	run := runs[0]
	if run.Status != "completed" {
		return run.Status
	}
	switch run.Conclusion {
	case "success":
		return "passing"
	case "failure":
		return "failed"
	default:
		return run.Conclusion
	}
}

// checkPRActivity checks for new comments on the PR.
func (d *Daemon) checkPRActivity(repoName, repoPath, agentName string, agent state.Agent) {
	prNum := strconv.Itoa(agent.PRNumber)

	cmd := exec.CommandContext(d.ctx, "gh", "pr", "view", prNum, "--json", "comments")
	cmd.Dir = repoPath
	output, err := cmd.Output()
	if err != nil {
		return
	}

	var result struct {
		Comments []prCommentResult `json:"comments"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		return
	}

	// Find the latest comment ID
	var latestID int64
	for _, c := range result.Comments {
		if c.DatabaseID > latestID {
			latestID = c.DatabaseID
		}
	}

	if latestID > agent.LastPRCommentID && agent.LastPRCommentID > 0 {
		// New comments detected; update tracking and wake the worker
		agent.LastPRCommentID = latestID
		if err := d.state.UpdateAgent(repoName, agentName, agent); err != nil {
			d.logger.Error("Failed to persist PR comment tracking for %s/%s: %v", repoName, agentName, err)
		}
		d.wakeWorker(repoName, agentName, agent,
			fmt.Sprintf("[daemon] Your PR #%d has new activity. Run `gh pr view %d --comments` to check for feedback, then address any issues. After fixing, run `oat agent waiting` again.", agent.PRNumber, agent.PRNumber))
	} else if agent.LastPRCommentID == 0 && latestID > 0 {
		// First time tracking; just record the baseline
		agent.LastPRCommentID = latestID
		if err := d.state.UpdateAgent(repoName, agentName, agent); err != nil {
			d.logger.Error("Failed to persist initial PR comment ID for %s/%s: %v", repoName, agentName, err)
		}
	}
}

// wakeWorker clears all dormancy flags and sends a message to the worker.
// Also clears idle mode so the normal nudge cycle resumes (merge-queue will
// start checking PRs again on the next 2-minute tick).
func (d *Daemon) wakeWorker(repoName, agentName string, agent state.Agent, message string) {
	if err := d.state.ModifyAgent(repoName, agentName, func(a *state.Agent) {
		a.ClearDormancy()
		a.LastWakeReason = message
		a.LastNudge = time.Now()
	}); err != nil {
		d.logger.Error("Failed to wake worker %s/%s: %v", repoName, agentName, err)
		return
	}

	// Clear idle mode so the merge-queue resumes its normal nudge cycle.
	// No immediate nudge -- the worker needs time to fix and push first.
	if err := d.state.SetRepoIdleMode(repoName, false); err != nil {
		d.logger.Warn("Failed to clear idle mode for %s after waking worker: %v", repoName, err)
	} else {
		d.logger.Info("Cleared idle mode for %s (worker %s woken)", repoName, agentName)
	}

	repo, _ := d.state.GetRepo(repoName)
	if repo == nil {
		return
	}

	if err := d.backend.SendMessage(d.ctx, repo.SessionName, agent.WindowName, message); err != nil {
		d.logger.Error("Failed to send PR notification to worker %s/%s: %v", repoName, agentName, err)
	} else {
		d.logger.Info("Woke worker %s/%s: %s", repoName, agentName, message)
	}

	// Clear verification timeout dedup so it can re-trigger if needed.
	notifyKey := fmt.Sprintf("%s/%s", repoName, agentName)
	d.verificationTimeoutNotifiedMu.Lock()
	delete(d.verificationTimeoutNotified, notifyKey)
	d.verificationTimeoutNotifiedMu.Unlock()

	// Trigger immediate wake pass so the freshly woken worker and other agents
	// (merge-queue, supervisor) get nudged without waiting for the next tick.
	d.triggerWake()
}

// allChecksPass returns true if there are status checks and all have passing conclusions
// (SUCCESS, NEUTRAL, SKIPPED). Returns false if any check failed or if there are no checks.
func allChecksPass(checks []prCheckResult) bool {
	if len(checks) == 0 {
		return false
	}
	for _, check := range checks {
		conclusion := check.Conclusion
		if conclusion == "" {
			conclusion = check.State
		}
		switch conclusion {
		case "SUCCESS", "NEUTRAL", "SKIPPED":
			continue
		default:
			return false
		}
	}
	return true
}

// fastMergeWorkerPR merges a green PR directly via `gh pr merge --squash` and
// auto-completes the worker, bypassing the merge-queue LLM. Used when
// OAT_FAST_MERGE is enabled. Falls back to notifyMergeQueueAboutPR on failure.
func (d *Daemon) fastMergeWorkerPR(repoName, repoPath, agentName string, agent state.Agent) {
	prNum := strconv.Itoa(agent.PRNumber)
	cmd := exec.CommandContext(d.ctx, "gh", "pr", "merge", prNum, "--squash")
	cmd.Dir = repoPath
	output, err := cmd.CombinedOutput()
	if err != nil {
		d.logger.Warn("Fast-merge failed for PR #%d (%s/%s): %v (output: %s) — falling back to merge-queue",
			agent.PRNumber, repoName, agentName, err, strings.TrimSpace(string(output)))
		d.notifyMergeQueueAboutPR(repoName, agentName, agent)
		return
	}

	d.logger.Info("Fast-merged PR #%d for %s/%s", agent.PRNumber, repoName, agentName)

	summary := fmt.Sprintf("PR #%d merged by daemon (fast-merge)", agent.PRNumber)
	d.autoCompleteWorker(repoName, agentName, agent, summary)

	// Inform merge-queue and supervisor so they stay in sync
	msgMgr := d.getMessageManager()
	mqMsg := fmt.Sprintf("[daemon] Fast-merged PR #%d from worker '%s' (CI green, mergeable). No action needed.", agent.PRNumber, agentName)
	if _, err := msgMgr.Send(repoName, "daemon", "merge-queue", mqMsg); err != nil {
		d.logger.Error("Failed to notify merge-queue about fast-merge: %v", err)
	}
	supervisorMsg := fmt.Sprintf("[daemon] Fast-merged PR #%d from worker '%s' (CI green, mergeable).", agent.PRNumber, agentName)
	if _, err := msgMgr.Send(repoName, "daemon", "supervisor", supervisorMsg); err != nil {
		d.logger.Error("Failed to notify supervisor about fast-merge: %v", err)
	}
	d.triggerRouteMessages()
}

// forceMergeWorkerPR force-merges a green PR via `gh pr merge --squash`, auto-completes
// the worker, and notifies supervisor and merge-queue. This is the daemon's safety net
// when the merge-queue fails to process a ready PR within the dormancy cap.
func (d *Daemon) forceMergeWorkerPR(repoName, repoPath, agentName string, agent state.Agent) {
	prNum := strconv.Itoa(agent.PRNumber)
	cmd := exec.CommandContext(d.ctx, "gh", "pr", "merge", prNum, "--squash")
	cmd.Dir = repoPath
	output, err := cmd.CombinedOutput()
	if err != nil {
		d.logger.Error("Failed to force-merge PR #%d for %s/%s: %v (output: %s)", agent.PRNumber, repoName, agentName, err, strings.TrimSpace(string(output)))
		d.handleDormancyTimeout(repoName, repoPath, agentName, agent)
		return
	}

	d.logger.Info("Daemon force-merged PR #%d for %s/%s (merge-queue bypass)", agent.PRNumber, repoName, agentName)

	summary := fmt.Sprintf("Daemon force-merged PR #%d (merge-queue failed to process within %v)", agent.PRNumber, workerDormancyCap)
	d.autoCompleteWorker(repoName, agentName, agent, summary)

	msgMgr := d.getMessageManager()
	supervisorMsg := fmt.Sprintf("[daemon] Force-merged PR #%d from worker '%s' (merge-queue failed to process within %v). The PR was green and mergeable.", agent.PRNumber, agentName, workerDormancyCap)
	if _, err := msgMgr.Send(repoName, "daemon", "supervisor", supervisorMsg); err != nil {
		d.logger.Error("Failed to notify supervisor about force-merge: %v", err)
	}
	mqMsg := fmt.Sprintf("[daemon] Force-merged PR #%d from worker '%s' because merge-queue did not process it within %v. Please check your processing pipeline.", agent.PRNumber, agentName, workerDormancyCap)
	if _, err := msgMgr.Send(repoName, "daemon", "merge-queue", mqMsg); err != nil {
		d.logger.Error("Failed to notify merge-queue about force-merge: %v", err)
	}

	d.triggerRouteMessages()
}

// handleDormancyTimeout force-completes a worker that has been waiting too long.
func (d *Daemon) handleDormancyTimeout(repoName, repoPath, agentName string, agent state.Agent) {
	d.logger.Warn("Worker %s/%s timed out after %v waiting for PR #%d", repoName, agentName, workerDormancyCap, agent.PRNumber)

	prNumber := agent.PRNumber
	if err := d.state.ModifyAgent(repoName, agentName, func(a *state.Agent) {
		a.ReadyForCleanup = true
		a.ReadyForCleanupAt = time.Now()
		a.ClearDormancy()
		a.FailureReason = fmt.Sprintf("Timed out after %v waiting for PR #%d", workerDormancyCap, prNumber)
	}); err != nil {
		d.logger.Error("Failed to timeout worker %s/%s: %v", repoName, agentName, err)
		return
	}

	// Notify supervisor and merge-queue
	msgMgr := d.getMessageManager()
	supervisorMsg := fmt.Sprintf("[daemon] Worker '%s' timed out after %v waiting for PR #%d. The PR may still need attention -- consider reassigning the task. Merge-queue has also been notified.", agentName, workerDormancyCap, agent.PRNumber)
	if _, err := msgMgr.Send(repoName, "daemon", "supervisor", supervisorMsg); err != nil {
		d.logger.Error("Failed to notify supervisor about timeout: %v", err)
	}

	mqMsg := fmt.Sprintf("[daemon] Worker '%s' timed out after %v waiting for PR #%d. Check if the PR needs CI fixes or merge conflict resolution.", agentName, workerDormancyCap, agent.PRNumber)
	if _, err := msgMgr.Send(repoName, "daemon", "merge-queue", mqMsg); err != nil {
		d.logger.Error("Failed to notify merge-queue about timeout: %v", err)
	}

	d.triggerRouteMessages()
	d.safeGo("health-check-pr-complete", d.checkAgentHealth)
}

// checkMainCIAfterMerge checks main branch CI after a PR merge and alerts the
// merge-queue if CI is red. Deduplicates alerts to at most one per 5 minutes
// per repo to avoid flooding when multiple PRs merge in quick succession.
func (d *Daemon) checkMainCIAfterMerge(repoName string, prNumber int) {
	// Guard: repo may have been removed during the delay
	repo, exists := d.state.GetRepo(repoName)
	if !exists || repo == nil {
		return
	}

	// Dedup: skip if we already alerted for this repo recently
	d.mainCIAlertTimeMu.Lock()
	if last, ok := d.mainCIAlertTime[repoName]; ok && time.Since(last) < 5*time.Minute {
		d.mainCIAlertTimeMu.Unlock()
		d.logger.Debug("Skipping main CI check for %s (alerted %s ago)", repoName, time.Since(last).Round(time.Second))
		return
	}
	d.mainCIAlertTimeMu.Unlock()

	repoPath := d.paths.RepoDir(repoName)

	conclusion, status := d.queryMainCIStatus(repoPath)
	if status == "in_progress" || status == "queued" {
		// CI hasn't finished yet — retry once after 60s
		time.Sleep(60 * time.Second)
		// Re-check repo still exists
		if _, exists := d.state.GetRepo(repoName); !exists {
			return
		}
		conclusion, _ = d.queryMainCIStatus(repoPath)
	}

	if conclusion != "failure" {
		return
	}

	d.logger.Warn("Main CI is red for %s after merge of PR #%d", repoName, prNumber)

	// Record alert time under lock
	d.mainCIAlertTimeMu.Lock()
	// Re-check dedup after the potential 60s retry wait
	if last, ok := d.mainCIAlertTime[repoName]; ok && time.Since(last) < 5*time.Minute {
		d.mainCIAlertTimeMu.Unlock()
		return
	}
	d.mainCIAlertTime[repoName] = time.Now()
	d.mainCIAlertTimeMu.Unlock()

	msgMgr := d.getMessageManager()
	msg := fmt.Sprintf("[daemon] ALERT: Main branch CI is failing (checked after PR #%d was merged). "+
		"If multiple PRs merged recently, the failure may not be from this specific PR. "+
		"Run `gh run list --branch main --limit 3` and follow your Main Branch CI Failures protocol.",
		prNumber)
	if _, err := msgMgr.Send(repoName, "daemon", "merge-queue", msg); err != nil {
		d.logger.Error("Failed to alert merge-queue about main CI failure: %v", err)
		return
	}
	d.triggerRouteMessages()
}

// queryMainCIStatus runs `gh run list --branch main --limit 1` and returns
// (conclusion, status). Returns ("", "") on error.
func (d *Daemon) queryMainCIStatus(repoPath string) (conclusion, status string) {
	cmd := exec.CommandContext(d.ctx, "gh", "run", "list", "--branch", "main", "--limit", "1", "--json", "status,conclusion")
	cmd.Dir = repoPath
	output, err := cmd.Output()
	if err != nil {
		d.logger.Debug("Failed to query main CI: %v", err)
		return "", ""
	}

	var runs []struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	}
	if err := json.Unmarshal(output, &runs); err != nil || len(runs) == 0 {
		return "", ""
	}
	return runs[0].Conclusion, runs[0].Status
}
