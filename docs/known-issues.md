# Known Issues & Fixes

This document catalogs bugs and issues encountered during OAT development, along with their root causes and fixes. It serves as a reference for understanding what broke and how each issue was resolved.

## Daemon & Agent Lifecycle Issues

Issues in the daemon's agent management, nudging, PR monitoring, and completion logic. These are not related to the oat-agent migration — they are daemon design bugs surfaced primarily through benchmark runs.

### Merge-Queue Stuck in Extended Thinking

When using slower models (e.g., DeepSeek V3.2 Nitro via OpenRouter), the merge-queue agent spent 5-6 minutes per "Thinking..." step, leaving 4+ green PRs unmerged for 20+ minutes. The daemon had no mechanism to detect or interrupt this behavior for core agents (merge-queue, supervisor).

Additionally, the `oat-agent` Textual TUI's ESC handler (`action_interrupt`) clears `_pending_messages`, so messages delivered to the PTY while the agent was thinking were lost when the thinking state was eventually cancelled.

- **Impact:** PRs with passing CI sat unmerged for the entire run duration. Dormancy cap timeouts fired before the merge-queue could process its queue.
- **Fix:** Added `stuck_core_agents.go` with thinking-timeout detection: soft timeout (default 5 min) sends ESC + nudge with recovered missed messages and fresh state summary; hard timeout (default 15 min) restarts the agent. Also fixed hardcoded "30 minutes" in timeout messages to use actual `workerDormancyCap` value, and made merge-queue nudges explicitly directive about merging passing PRs. **Note:** The initial implementation relied on Claude session files for activity detection, which was later replaced with backend-agnostic output log monitoring — see "Stuck Detection Blind for Non-Claude Backends" below.
- **Files changed:** `internal/daemon/stuck_core_agents.go` (new), `internal/daemon/daemon.go`, `internal/daemon/pr_monitor.go`, `internal/templates/agent-templates/merge-queue.md`, `internal/prompts/supervisor.md`

### Stuck Detection Blind for Non-Claude Backends

Both `stuck_workspace.go` and `stuck_core_agents.go` relied on Claude session file mtime (`~/.claude/projects/<encoded-path>/<SessionID>.jsonl`) to detect stuck agents. When using non-Claude backends (OpenRouter, DeepSeek, etc.), these files don't exist, so the stuck detection silently skipped every check — `os.Stat()` returned an error and the function continued to the next agent.

- **Impact:** During a DeepSeek V3.2 Nitro benchmark run, the merge-queue was stuck thinking for 37 minutes with no detection or intervention. Workers with green PRs timed out (15-min dormancy cap) and were cleaned up before the merge-queue could process their PRs, resulting in orphaned PRs with merge conflicts.
- **Fix:** Replaced Claude-specific `getSessionFilePath()` with backend-agnostic output log monitoring (`d.paths.AgentLogFile()`). The daemon already writes structured output events (ASSISTANT, TOOL, RESULT) to `~/.oat/output/<repo>/<agent>.log` for every agent regardless of backend. During "Thinking...", no output events are produced, so the log mtime stays stale — the same signal as before, but backend-agnostic. Also removed the now-unused `getSessionFilePath()` function.
- **Files changed:** `internal/daemon/stuck_workspace.go`, `internal/daemon/stuck_core_agents.go`, `internal/daemon/stuck_workspace_test.go`

### Merge-Queue Stuck Thinking Causes Cascading Worker Timeouts

When the merge-queue gets stuck thinking for an extended period, workers with green, mergeable PRs enter dormancy and wait for the merge-queue to process them. If the merge-queue remains stuck longer than the worker dormancy cap (default 15 minutes), the daemon times out and cleans up the workers. Meanwhile, other PRs that do get merged cause merge conflicts on the orphaned PRs, which no longer have workers to fix them.

- **Impact:** During a DeepSeek V3.2 Nitro benchmark run, the merge-queue was stuck for 37 minutes. PRs #22, #24, #25 had workers that timed out. Subsequent merges created conflicts on these orphaned PRs. The cascade resulted in multiple open PRs with merge conflicts and no workers to resolve them.
- **Fix:** Enabled `OAT_FAST_MERGE` by default (`pr_monitor.go`). When enabled, the daemon directly merges green, mergeable PRs via `gh pr merge --squash` as soon as CI passes, bypassing the merge-queue LLM on the happy path. The merge-queue still handles edge cases (conflicts, CI failures, review enforcement). Users who want the merge-queue LLM to review every PR before merging can set `OAT_FAST_MERGE=false`. Also added supervisor notification to `fastMergeWorkerPR` for parity with `forceMergeWorkerPR`.
- **Files changed:** `internal/daemon/pr_monitor.go`, `internal/templates/agent-templates/merge-queue.md`, `internal/prompts/supervisor.md`

### Systemic Nudge Loop After Worker Wake

After the daemon woke a dormant worker (e.g., "PR merged, run `oat agent complete`"), the worker became active but `wakeWorker()` did not set `agent.LastNudge`. The next 2-minute nudge cycle immediately sent a conflicting "Status check: Update on your progress?" message. Workers receiving both messages often ran `oat agent waiting` (going dormant again), and the PR monitor would detect the merged PR and wake them again — creating an infinite wake → nudge → waiting → wake loop.

- **Impact:** Every worker in the benchmark was affected. Some workers (e.g., shadow-raven) accumulated 688+ `oat agent complete` calls and 1,550+ daemon nudges.
- **Fix:** Added `agent.LastNudge = time.Now()` in `wakeWorker()` to give freshly-woken workers a 2-minute grace period before the next nudge.
- **Files changed:** `internal/daemon/pr_monitor.go`

### Stale Merge-Conflict Rejection on Merged PRs

When a worker called `oat agent waiting` after its PR was already merged, `handleAgentWaiting` queried GitHub's `mergeable` field and found "CONFLICTING" — a stale value that GitHub doesn't update after merge. The daemon rejected the dormancy request with "PR still has a merge conflict" even though the PR was already merged, trapping the worker in a loop.

- **Fix:** Added a check for `result.State == "MERGED" || "CLOSED"` before the mergeable check. When the PR is already done, the worker is auto-completed and the supervisor is notified.
- **Files changed:** `internal/daemon/daemon.go`

### Repeated `oat agent complete` Calls Not Rejected

`handleCompleteAgent` had no guard against repeated calls — every call re-saved state and re-triggered notifications (though the dedup map prevented double-notifications). Workers stuck in nudge loops would call `oat agent complete` hundreds of times.

- **Fix:** Added a `ReadyForCleanup` guard at the top of `handleCompleteAgent` that returns an emphatic "You are DONE. Stop all activity immediately." error message.
- **Files changed:** `internal/daemon/daemon.go`

### Workspace Agent Telling Workers to Complete Prematurely

The workspace LLM interpreted "check if workers are stuck, fix any problems" as "tell workers to run `oat agent complete`" — even when their PR was still open and unmerged. This caused premature worker cleanup.

- **Fix:** Added explicit guidance to the workspace prompt: do not tell workers to run `oat agent complete` unless their PR is confirmed merged/closed or no PR was needed. The daemon handles worker lifecycle automatically.
- **Files changed:** `internal/prompts/workspace.md`

### Idle Mode Deadlock — Dormant Worker with No PR

Workers that ran `oat agent waiting` without having a PR (e.g., fixup workers, or workers whose assigned issue was already resolved) entered a dormant state with `PRNumber == 0`. The PR monitor never wakes dormant workers with no PR, and idle mode prevents nudging the supervisor, creating a deadlock.

- **Fix:** `handleAgentWaiting` now auto-completes workers that have no PR instead of allowing them to go dormant.
- **Files changed:** `internal/daemon/daemon.go`, `internal/templates/agent-templates/worker.md`

### Final Nudge Messages Silently Skipped Before Idle Mode

`sendFinalNudgeToRepo()` called `shouldNudgeAgent()`, which has a 2-minute cooldown. When agents were nudged less than 2 minutes before idle mode was entered, the critical final nudge was silently skipped.

- **Fix:** Created `shouldSendFinalNudge()` that bypasses the time-based cooldown for final nudges.
- **Files changed:** `internal/daemon/daemon.go`

### Active Workers Not Notified of PR Issues

The PR monitor only checked PRs for dormant workers. Active workers with open PRs never received merge conflict or CI failure notifications.

- **Fix:** Added `checkActiveWorkerPRIssues()` to the PR monitor loop for active workers with open PRs. Uses dedup maps to avoid spamming.
- **Files changed:** `internal/daemon/pr_monitor.go`, `internal/daemon/daemon.go`

### `gh pr checks` Failing Due to Token Permissions

Daemon notifications and the worker prompt instructed workers to run `gh pr checks <number>`, which requires `checks:read` permission that fine-grained PATs often lack.

- **Fix:** Changed to recommend `gh run list --branch work/<name> --limit 1` and `gh run view <run-id> --log-failed` instead.
- **Files changed:** `internal/daemon/pr_monitor.go`, `internal/templates/agent-templates/worker.md`

### Dormant Workers Leaving Merge-Queue Unaware

Workers going dormant didn't notify the merge-queue about their pending PRs, leaving PRs unmonitored.

- **Fix:** Added explicit notifications to merge-queue when workers enter dormant state.

### Duplicate CI/Conflict Notifications Flooding Agents

Automatic worktree refresh and duplicate merge-queue notifications caused message flooding loops between agents.

- **Fix:** Disabled automatic worktree refresh and added deduplication logic for notifications.

### Daemon Socket Timeout During Restore

CLI commands like `oat repo use` hit read timeouts because `restoreTrackedRepos()` ran before the server loop started, blocking the socket.

- **Fix:** Started the server loop before running restore operations.

### Daemon-Generated Messages Attributed to Worker Name

Messages from `handleCompleteAgent` and dormancy notifications used `from: agentName` instead of `from: "daemon"`, making it unclear which messages came from the daemon vs the agent itself.

- **Fix:** Changed all daemon-generated messages to use `from: "daemon"` with `[daemon]` prefix.
- **Files changed:** `internal/daemon/daemon.go`

### Verbose Dormancy Notifications to Merge-Queue

Dormancy notifications included the full task description, flooding the merge-queue with long messages.

- **Fix:** Shortened to just worker name and PR number.
- **Files changed:** `internal/daemon/daemon.go`

### Supervisor Not Told Merge-Queue Was Already Notified

When the daemon sent completion/timeout notifications to both supervisor and merge-queue, the supervisor didn't know the merge-queue was already informed and could send duplicate notifications.

- **Fix:** Appended "Merge-queue has also been notified." to supervisor messages in all 4 dual-notification paths.
- **Files changed:** `internal/daemon/daemon.go`, `internal/daemon/stuck_worker.go`, `internal/daemon/pr_monitor.go`

### Stuck Worker Nudges Telling Workers to Run `oat agent complete` Instead of `oat agent waiting`

Workers who had created PRs were told to run `oat agent complete` (skip dormancy) instead of `oat agent waiting` (enter dormancy and let daemon monitor PR).

- **Fix:** Changed 3 message paths in `stuck_worker.go` from `oat agent complete` to `oat agent waiting`.
- **Files changed:** `internal/daemon/stuck_worker.go`

### Force-Removed Worker Notification Missing Task Context

When a worker was force-removed, the supervisor notification didn't include the original task, making it hard to decide whether to respawn.

- **Fix:** Included the worker's task and explicitly suggested respawning in the supervisor notification.
- **Files changed:** `internal/daemon/stuck_worker.go`

### `checkWorkerProgress` Not Detecting Merged/Closed PRs

At daemon takeover (nudge 8+), `checkWorkerProgress` only searched for open PRs via `getWorkerPR`. If the PR was already merged and branch deleted (squash merge), the daemon fell through to "no progress" warnings instead of auto-completing. Same gap existed in `forceRemoveWorker`.

- **Fix:** Added `agent.PRNumber` status checks at the start of both `checkWorkerProgress` and `forceRemoveWorker`. If the PR is already MERGED or CLOSED, the worker is auto-completed immediately.
- **Files changed:** `internal/daemon/stuck_worker.go`

### Active Worktrees Deleted by Orphan Cleanup During Concurrent Git Operations

`cleanupOrphanedWorktrees` runs at the end of every `checkAgentHealth()` call (every 2 minutes). It calls `git worktree list`, then deletes any directory in the worktree root that isn't in the returned list. When the daemon also runs `git worktree remove --force` for dead agents on the same repo, this modifies `.git/worktrees/` concurrently with active agents' git operations (`git rebase`, `git push`), which can cause `git worktree list` to return incomplete results. Active worktrees then appear "orphaned" and get deleted.

- **Impact:** Observed in the Haiku 4.5 benchmark (`oat-bench-haiku45-verifycommandtest`). Three workers (gentle-elk, twilight-raccoon, witty-elk) had their worktree directories deleted mid-operation. Each then spent 16-28 minutes unable to run any command (`FileNotFoundError` on every `execute()` call), wasting tokens and blocking convergence.
- **Fix:** Added a `protectedPaths` parameter to `CleanupOrphanedWithDetails`. The daemon populates this from its own state — any agent that is not `ReadyForCleanup` has its worktree path protected from deletion. Also moved `wt.Prune()` to run before the orphan check (previously ran after) so git tracking is as fresh as possible.
- **Files changed:** `internal/worktree/worktree.go`, `internal/daemon/daemon.go`

### Supervisor Self-Terminated via Hallucinated Flag

The Haiku supervisor ran `oat agent complete --worker twilight-raccoon` to complete a stuck worker on its behalf. Since `--worker` did not exist, the command completed the supervisor itself (the caller). No guard prevented permanent agents (supervisor, workspace, merge-queue) from being completed.

- **Impact:** Supervisor was permanently removed at 19:27:11, leaving the repo without orchestration for the remainder of the run.
- **Fix:** Two-part fix: (1) Added a permanent-agent guard in `handleCompleteAgent` — supervisor, workspace, merge-queue, pr-shepherd, and generic-persistent agent types are rejected with an error message guiding the caller to use `--worker`. (2) Implemented the `--worker <name>` flag on `oat agent complete` so the supervisor can legitimately complete a worker on its behalf.
- **Files changed:** `internal/daemon/daemon.go`, `internal/cli/cli.go`, `internal/prompts/supervisor.md`

### Merged-PR Workers Stuck 16 Minutes Before Auto-Completion

When a PR merges, the daemon wakes the worker to run `oat agent complete`. If the worker can't comply (worktree deleted, model ignoring instructions), it takes 8 nudges (16 minutes) before the daemon auto-completes it.

- **Impact:** Workers like witty-elk (whose worktree was deleted) spent 16+ minutes receiving nudges they couldn't act on, wasting tokens and delaying convergence.
- **Fix:** Added `WokenForMergedPRAt` timestamp to the `Agent` struct. When the PR monitor wakes a worker for a merged PR, it records the time. In the stuck-worker escalation, workers with this timestamp set are fast-tracked to daemon takeover after 3 nudges (6 minutes) instead of 8 (16 minutes). The self-completion metric is preserved — the summary still reads "worker did not self-complete," correctly flagging model non-compliance.
- **Files changed:** `internal/state/state.go`, `internal/daemon/pr_monitor.go`, `internal/daemon/stuck_worker.go`

### No Supervisor Override for Daemon Escalation

The daemon escalated independently of the supervisor — even if the supervisor determined a worker was actively working (e.g., running a long test suite), it had no way to pause the escalation.

- **Fix:** Added `oat worker reset-nudge <worker-name>` command. The supervisor can use this once per worker to reset the nudge count to zero, buying another full escalation cycle. `NudgeResetUsed` resets when the worker enters dormancy.
- **Files changed:** `internal/state/state.go`, `internal/daemon/daemon.go`, `internal/cli/cli.go`, `internal/prompts/supervisor.md`

### Repeated `oat agent waiting` Calls Flooding Merge-Queue

Workers (LLMs) sometimes called `oat agent waiting` multiple times in rapid succession without being woken between calls. Each call sent a fresh "Worker X has submitted PR #Y" notification to the merge-queue, flooding it with duplicates.

- **Fix:** Added a `WaitingForPR` early-return guard in `handleAgentWaiting`. If the worker is already dormant, the handler returns immediately with a success response ("You are already dormant") without re-processing or sending notifications. `wakeWorker` clears `WaitingForPR`, so legitimate re-entries after being woken still work.
- **Files changed:** `internal/daemon/daemon.go`

### Truncation Retry False Positives Causing Duplicate Message Delivery

The truncation detection added to `SendKeysLiteralWithEnter` (fix for truncated IPC messages) used a small capture window (-S -20, last 20 lines) and short settle delay (300ms). When agents processed messages quickly (scrolling them off the visible pane), the check couldn't find the message text, falsely concluded it was truncated, and retried — delivering every message twice. Daemon logs showed "message truncated at end, retrying" warnings on nearly every delivery.

Initial mitigation (increasing capture window to -S -50, settle delay to 1s, adding input-line detection) reduced but did not eliminate false positives. The fundamental issue is that fast-consuming agents (especially merge-queue) scroll past the message before any pane-capture check can run.

- **Fix:** Removed the truncation retry block entirely from `SendKeysLiteralWithEnter`. The initial `paste-buffer` + `send-keys Enter` is reliable when it returns exit code 0, and a separate Enter-retry check handles the main failure mode (Enter key lost). An explanatory comment was left in place of the removed block.
- **Files changed:** `pkg/tmux/client.go`

### Daemon Prompt Delivery Bug for Configurable Agent Types

The daemon's `writePromptFile` called `prompts.GetPrompt()`, which returns an empty string for configurable agent types (merge-queue, worker, review). These types are supposed to use the `agents.Reader` system to load templates from `~/.oat/repos/<repo>/agents/*.md`, but only the CLI had this logic (`getAgentDefinition()`). The daemon never called it.

Additionally, two other code paths bypassed the fix even after `writePromptFileWithPrefix` was corrected:

1. **`handleSpawnAgent`** wrote the prompt directly from the supervisor's socket request text (`promptText` arg) to `~/.oat/prompts/<name>.md`, bypassing `writePromptFileWithPrefix` entirely. This is the code path used when the supervisor spawns agents via `oat agents spawn`.
2. **`restartAgent`** checked if `~/.oat/prompts/<name>.md` already existed and skipped regeneration if so. After a daemon restart, stale prompt files from the old binary persisted and were never regenerated with the new template-loading logic.

- **Impact:** Merge-queue, worker (on daemon restart), and review agents received only slash commands and the backend runtime note -- no operational instructions at all. The merge-queue didn't know about `gh run list` fallbacks, CI gate rules, or any of its core instructions. This was a latent bug on the `fix/benchmark-reliability-round1` branch (where the CLI started agents directly), promoted to critical when agent startup moved to the daemon via `start_repo_agents`.
- **Fix:** (1) Updated `writePromptFileWithPrefix` to use `agents.Reader` for configurable agent types, copying default templates if not found. (2) Changed `handleSpawnAgent` to call `writePromptFile` instead of writing the prompt directly. (3) Changed `restartAgent` to always regenerate prompt files so code/template changes take effect after daemon restart.
- **Files changed:** `internal/daemon/daemon.go`

### Merge-Queue Merging PRs with Failing CI (Blind Merge)

The merge-queue merged 19 PRs without ever verifying GitHub CI. When `gh pr checks` failed with `GraphQL: Resource not accessible by personal access token`, the merge-queue abandoned CI verification entirely and relied on local test runs instead.

- **Root cause:** The merge-queue never received its full operational instructions (including the `gh run list` fallback for CI checking) due to the daemon prompt delivery bug above. It made a reasonable but incorrect inference that the GitHub API was entirely inaccessible for CI checks.
- **Fix:** Three-part fix: (1) Fixed daemon prompt delivery so the merge-queue receives its full instructions including fallback commands. (2) Added explicit "NEVER merge a PR with failing CI" rule to the merge-queue prompt template. (3) Daemon now includes CI status summary in periodic nudge messages to the merge-queue, so it doesn't need to discover fallback commands itself.
- **Files changed:** `internal/daemon/daemon.go`, `internal/daemon/pr_monitor.go`, `internal/templates/agent-templates/merge-queue.md`

### Post-Completion Token Waste (Agents Looping After Complete)

Workers continued executing commands (burning 100K+ tokens each) even after `oat agent complete` succeeded and they received the "You are DONE" error. The daemon's `handleCompleteAgent` marked state but didn't kill the process. There was a 1-minute `workerPostCompletionDelay` before cleanup, and the health check ran every 2 minutes -- leaving a 1-3 minute window where the agent was "done" but still running.

- **Fix:** Added a goroutine in `handleCompleteAgent` that kills the agent process via `backend.StopAgent` after a 15-second grace period. Reduced `workerPostCompletionDelay` from 1 minute to 15 seconds.
- **Files changed:** `internal/daemon/daemon.go`

### Stale Local Binary Shadowing `$GOPATH/bin/oat`

`go build ./cmd/oat` (without `-o`) drops a compiled binary as `./oat` in the project root. When `oat daemon start` is run from the project root, the shell finds `./oat` before `$GOPATH/bin/oat`. The daemon's `RunDetached` calls `os.Executable()` to get its own path and forks that binary as the daemon process -- perpetuating the stale binary even after `go install` puts a newer version in `$GOPATH/bin`.

- **Impact:** Code changes compiled via `go install` or `scripts/install.sh` had no effect on the running daemon, because the daemon kept forking the stale `./oat` from the project root. Debugging was extremely difficult because the source was correct, tests passed, and `which oat` returned the correct path -- but `lsof -p <daemon-pid>` revealed the wrong binary.
- **Fix:** Deleted the stale `./oat` binary. Updated `AGENTS.md` quick reference to recommend `go install ./cmd/oat` instead of `go build ./cmd/oat`, with a warning that the latter shadows the installed binary.
- **Files changed:** `AGENTS.md`
- **Prevention:** The binary is already in `.gitignore` (`/oat`). Developers should use `go install ./cmd/oat` for building (installs to `$GOPATH/bin`) and avoid `go build ./cmd/oat` from the project root.

### `oat pr create` Failing in Git Worktrees

`oat pr create` called `gh pr create` without `--repo` or `--head` flags, relying on `gh` to auto-detect the repository and branch from the local git context. In git worktrees (where `.git` is a file pointing to the main repo's `.git/worktrees/` directory), `gh` sometimes failed to resolve the remote or branch, causing `gh pr create failed: exit status 1`.

- **Impact:** 2 of 22 workers in the GPT 5.4 benchmark (storm-lion, light-elephant) had to fall back to `gh pr create` directly, bypassing the `oat` label and auto-dormancy features.
- **Fix:** `prCreate()` now loads state to get `GithubURL` and passes `--repo owner/repo` explicitly. It also runs `git rev-parse --abbrev-ref HEAD` to detect the current branch and passes `--head`. Both are graceful fallbacks — if state loading or branch detection fails, the flags are omitted and `gh` auto-detects (matching previous behavior). Additionally, `--closes` is now auto-detected from the agent's `IssueNumber` in state when not explicitly provided.
- **Files changed:** `internal/cli/cli.go`

### Issues Not Auto-Closed When Workers Skip PR Creation

Workers that completed without creating a PR (e.g., issue already resolved by another worker, or issue doesn't require code changes) had no mechanism to close their assigned GitHub issue. The only issue-closing path was through `oat pr create --closes`, which adds `Closes #N` to the PR body for GitHub's auto-close on merge.

- **Impact:** Issue #8 in the GPT 5.4 benchmark remained open despite PR #32 being merged, because the worker's PR body didn't include `Closes #8` and the worker never ran `gh issue close`.
- **Fix:** `oat agent complete` now auto-closes the agent's assigned issue via `gh issue close` when: (1) no `--failure` flag was provided (successful completion), (2) the agent has an `IssueNumber` in state, and (3) the agent has no associated `PRNumber` (no PR was created). If a PR exists, the PR's `Closes #N` handles it at merge time.
- **Files changed:** `internal/cli/cli.go`

### CI Failure Nudge Not Instructing Workers to Rebase

The daemon's CI failure nudge message told workers to run `gh run list` and `gh run view --log-failed` to investigate failures, but did not instruct them to rebase onto main first. Workers whose tests depend on other workers' implementations (e.g., black-box system tests waiting for order validate/brew to land) would investigate the failure, correctly determine it's caused by missing dependencies, and go dormant — without ever rebasing to pick up those dependencies when they eventually merge to main.

- **Impact:** `ultra-penguin` in the GPT 5.4 benchmark wrote black-box system tests (issue #17) that correctly failed because order validate/brew hadn't landed yet. When those implementations merged to main (PR #38), ultra-penguin was repeatedly woken for CI failure but only checked if a new CI run appeared — it never rebased to pull the implementations. This created a 2-minute wake/check/dormant loop that burned 145K+ tokens (23 cycles) without ever self-healing. The repo never went idle because each wake-up briefly cleared idle mode.
- **Fix:** Updated all three CI failure nudge messages in `pr_monitor.go` to instruct workers to `git fetch origin main && git rebase origin/main` first, then push, before investigating CI logs. This ensures workers pick up dependency code that landed on main since their last push.
- **Files changed:** `internal/daemon/pr_monitor.go`

### Workspace Not Passing `--issue` Flag When Spawning Workers

The workspace agent spawns workers with `oat worker create "Work issue #N: ..."` but never passes the `--issue N` flag. This means `agent.IssueNumber` is never populated in state, and `oat pr create`'s auto-detection of `Closes #N` has nothing to work with. PRs merge without `Closes #N`, so GitHub issues stay open.

- **Impact:** In the second GPT 5.4 benchmark run, 16 of 18 merged PRs had no `Closes` reference. Only 2 issues were closed despite 18 PRs being merged. The benchmark's wave progress monitor showed 0/5 issues closed for wave 0 despite all work being completed and merged.
- **Fix:** (1) Updated workspace.md and supervisor.md prompts to show `--issue <number>` in worker spawning examples. (2) Added a regex fallback in `prCreate()` and `completeWorker()` that parses "issue #N" from the agent's Task field in state — since the workspace always includes the issue number in the task description, this catches the case even when `--issue` is forgotten. (3) Updated worker.md to instruct workers to pass `--closes <issue-number>` when they know their issue number.
- **Files changed:** `internal/prompts/workspace.md`, `internal/prompts/supervisor.md`, `internal/cli/cli.go`, `internal/templates/agent-templates/worker.md`

### Literal `\n` in GitHub Issue Comments

The worker prompt example for `gh issue comment` used `\n\n` inside double-quoted `--body` arguments. `gh` does not interpret `\n` escape sequences — it passes them literally. GitHub comments showed `\n\n— agent-name` as plain text instead of a newline followed by the agent signature.

- **Impact:** All worker issue comments in benchmarks had literal `\n` characters visible in the comment body on GitHub.
- **Fix:** Changed the prompt examples to use `$'...'` shell quoting, which does interpret `\n` escape sequences.
- **Files changed:** `internal/templates/agent-templates/worker.md`

### Nudge Loop Clobbers Dormancy State (Read-Modify-Write Race)

When `oat pr create` finishes, it auto-calls `agent_waiting` which sets `WaitingForPR=true` via `handleAgentWaiting`. If the daemon's nudge loop has already read the agent struct (with `WaitingForPR=false`) before `handleAgentWaiting` writes, the nudge loop's subsequent `UpdateAgent` call writes back its stale copy, overwriting `WaitingForPR` back to `false`. This is a classic read-modify-write race condition — both operations use the mutex individually, but the nudge loop's read-then-write spans a gap during which `handleAgentWaiting` can interleave.

Once dormancy is clobbered, the PR monitor never checks the worker's PR (it only monitors dormant workers), and the nudge loop keeps sending "Status check" messages every 2 minutes. The worker responds "I'm waiting for merge" but never re-runs `oat agent waiting` because it already did.

- **Impact:** Observed with `eager-panther` in the GPT 5.4 benchmark (98.6/100 run, `20260316-223457-gpt54-direct`). The daemon logged "Agent is now dormant" and nudged the worker at the exact same second (15:52:37). The worker received 11 nudges and required supervisor intervention before completing. This is a timing-dependent edge case — it requires the nudge loop and `agent_waiting` handler to execute within the same scheduling window.
- **Fix:** Added `State.ModifyAgent(repoName, agentName, func(*Agent))` — an atomic read-modify-write method that reads the current agent state, applies a callback, and writes back under a single lock hold. Converted all `UpdateAgent` calls in `stuck_worker.go` and `pr_monitor.go` to use `ModifyAgent`, so nudge-field updates never clobber concurrent changes to `WaitingForPR`, `ReadyForCleanup`, etc. The nudge callback also includes a guard: if the agent has gone dormant or completed since the nudge was initiated, the update is skipped entirely.
- **Files changed:** `internal/state/state.go`, `internal/daemon/stuck_worker.go`, `internal/daemon/pr_monitor.go`

### Merge-Queue Emergency Mode Halts All Progress on Pre-Existing CI Failures

The merge-queue prompt's Emergency Mode section said "Main branch CI red = stop everything." When the merge-queue ran `gh run list --branch main --limit 3` and saw a red CI run, it halted all merges, sent an EMERGENCY message to the supervisor, and spawned a fix worker. It then refused to process any PR — even those with green CI — until the fix worker resolved the main branch failure.

In the Gemini 3.1 Pro benchmark (`20260317-124323-gemini31pro-direct`), a pre-existing CI failure on `main` (from a legacy `.multiclaude` config file) triggered Emergency Mode on the very first status check. The fix worker (`azure-phoenix`) was stuck in an API "Thinking..." loop for the entire run (a known Gemini 3.1 Pro Preview latency bug — see GitHub issues [#22160](https://github.com/google-gemini/gemini-cli/issues/22160), [#22021](https://github.com/google-gemini/gemini-cli/issues/22021)). The merge-queue never rechecked main CI status and never exited Emergency Mode. The supervisor was alerted about the stuck fix worker but reset its nudge counter instead of removing it (see below).

- **Impact:** 14 PRs with passing CI sat unmerged for the entire 2-hour run. Final acceptance score was 2.5/100 despite workers producing functional code. The Emergency Mode freeze was the primary bottleneck — not the model's code quality.
- **Fix:** Rewrote the Emergency Mode section to "Main Branch CI Failures." The merge-queue now: (1) notifies the supervisor and spawns a fix worker, (2) **continues merging PRs that have green CI** (each PR's CI runs independently), (3) rechecks main CI on every status nudge, (4) monitors the fix worker and respawns it if stuck for >10 minutes.
- **Files changed:** `internal/templates/agent-templates/merge-queue.md`

### Supervisor Resets Nudge Counters Instead of Removing Stuck Workers (Model Behavior)

When the daemon escalated stuck workers after 5+ nudges, the supervisor (Gemini 3.1 Pro) ran `oat worker reset-nudge` to "buy them more time" instead of checking logs and considering removal. The supervisor prompt explicitly says this can only be used once per worker, and the daemon's `NudgeResetUsed` boolean correctly rejected second attempts. However, even one reset per worker buys a full additional escalation cycle (~10 minutes), which is enough to let frozen workers consume resources for the remainder of a benchmark run.

- **Impact:** Stuck workers like `bold-albatross`, `noble-dolphin`, and `azure-phoenix` ran for the entire 2-hour Gemini benchmark without producing any output. The daemon's escalation was working correctly — the supervisor just chose leniency every time.
- **Fix:** No prompt change needed — the supervisor prompt already instructs checking logs and considering removal. The `NudgeResetUsed` enforcement is working correctly. This is documented as a model-behavior observation: some models are too lenient with stuck workers.

### `--delete-branch` Flag Fails with Active Worktrees

The merge-queue prompt instructed `gh pr merge <number> --squash --delete-branch` for merging PRs. The `--delete-branch` flag attempts to delete both the remote and local branch after merging. Git refuses to delete a local branch that is checked out in a worktree, causing an error like: `error: cannot delete branch 'work/silly-dolphin' used by worktree at '~/.oat/wts/.../silly-dolphin'`. This caused the merge-queue to interpret the merge as failed, even though the PR was successfully merged on GitHub.

- **Impact:** PR #28 in the Gemini benchmark appeared to fail during merge, confusing the merge-queue. The remote branch was deleted but the local error caused a non-zero exit code.
- **Fix:** Removed `--delete-branch` from the merge command. Branch cleanup is handled automatically by the daemon's `cleanupMergedBranches()` function, which runs every 2 minutes as part of the health check loop. It skips branches still in use by worktrees and cleans up both local and remote branches once the worktree is removed.
- **Files changed:** `internal/templates/agent-templates/merge-queue.md`

### `git fetch origin main:main` Fails in Merge-Queue Worktree

The merge-queue prompt instructed running `git fetch origin main:main` after each merge to keep the local `main` in sync. The `:main` syntax tells Git to update the local `main` branch ref, but Git refuses because `main` is already checked out by the bare repo at `~/.oat/repos/<repo>/`. Error: `fatal: refusing to fetch into branch 'refs/heads/main' checked out at '~/.oat/repos/...'`.

- **Impact:** Every post-merge sync in the Gemini benchmark failed. The Gemini model worked around this by falling back to `git fetch origin main` + `git rebase origin/main`, but less capable models might not recover.
- **Fix:** Changed the prompt from `git fetch origin main:main` to `git fetch origin main`, which updates `origin/main` without touching the local branch ref.
- **Files changed:** `internal/templates/agent-templates/merge-queue.md`

### NudgeCount Not Persisted During Daemon Takeover (Stuck Workers Never Force-Removed)

When a worker's nudge count reached `stuckDaemonNudge` (8), `nudgeWorkerEscalating` entered the daemon takeover path, calling `checkWorkerProgress()` and returning. The incremented `NudgeCount` was only stored in a local variable -- the `ModifyAgent` call that persists it to state was in the normal nudge path (after the takeover branch), so it was never reached. Each cycle, the daemon read the stale count (7), incremented to 8, entered takeover, and returned without saving. The count could never reach `stuckMaxNudge` (20) to trigger force-removal.

- **Impact:** Workers that entered daemon takeover without having pushed a branch or created a PR (e.g., API-hung workers producing no output) were trapped indefinitely. Observed with `bold-heron` in the Gemini 3.1 Pro benchmark -- 14+ daemon takeover attempts over ~1 hour with no force-removal, preventing the repo from entering idle mode.
- **Fix:** Added a `ModifyAgent` call in the daemon takeover branch to persist the incremented `NudgeCount` before calling `checkWorkerProgress`. The count now correctly increments from 8 toward 20 on each cycle, eventually triggering force-removal.
- **Files changed:** `internal/daemon/stuck_worker.go`

### Merge-Queue Cannot Run `gh pr merge` in Detached HEAD Worktree

Non-worker agent worktrees (supervisor, merge-queue, pr-shepherd) are created with `CreateDetached(path, "HEAD")`, which puts them in detached HEAD state. The `gh` CLI requires a branch context for commands like `gh pr merge`; in detached HEAD it fails with `could not determine current branch: failed to run git: not on any branch`.

Smarter models (Sonnet 4.6, GPT 5.4) work around this by checking out `main` or using alternative approaches. GPT 5.3 Codex hit this error once and stopped trying to merge any PRs entirely, causing all waves to time out with 0 issues closed despite 12+ PRs with green CI.

- **Impact:** Observed with GPT 5.3 Codex benchmark. 12+ PRs with passing CI sat unmerged. All benchmark waves timed out because no issues were ever closed.
- **Fix:** (1) Added `CheckoutBranch` method to `worktree.Manager` and called it after `CreateDetached` for supervisor, merge-queue, and pr-shepherd worktrees, so they start on a branch instead of detached HEAD. If `main` is already checked out in the main clone (which prevents checking it out in a worktree), `CheckoutBranch` falls back to creating a local tracking branch (e.g., `supervisor-main`, `merge-queue-main`) that tracks `origin/main`. This gives the `gh` CLI the branch context it needs without conflicting with the main clone. Non-fatal warning on failure (smarter models handle detached HEAD anyway). (2) Added a fallback instruction to the merge-queue prompt: if `gh pr merge` fails with "not on any branch", run `git checkout main` first.
- **Files changed:** `internal/worktree/worktree.go`, `internal/cli/cli.go`, `internal/templates/agent-templates/merge-queue.md`

---

## Provider and Model Observations

Observations from benchmark runs related to external LLM providers and model capabilities, not OAT bugs.

### OpenRouter Latency with Gemini Models

OpenRouter adds significant latency when routing requests to Gemini models. During benchmark runs using `openrouter:google/gemini-3.1-pro-preview` and `openrouter:google/gemini-3.1-flash-lite-preview`, agents experienced "Thinking..." hangs lasting 10+ minutes per API call.

- **Gemini 3.1 Pro Preview** hangs are a [known upstream bug](https://github.com/google-gemini/gemini-cli/issues/22160) specific to the Pro Preview model. Flash models are reportedly unaffected at the Google API level.
- **Gemini 3.1 Flash-Lite Preview** experienced similar (though intermittent) hangs when routed through OpenRouter. Users of other tools (e.g., [avante.nvim#1176](https://github.com/yetone/avante.nvim/issues/1176)) report that using Google's API directly resolves the issue, suggesting OpenRouter's Gemini integration adds latency or has a hanging bug.
- **Impact:** Merge-queue and workers get stuck waiting for API responses, preventing merges and stalling wave progression. Affects all Gemini models routed through OpenRouter.
- **Workaround:** Use `google_genai:` model prefix to hit Google's API directly instead of `openrouter:google/`. Requires `GOOGLE_API_KEY` to be set.

### Gemini 3.1 Flash-Lite Preview: Insufficient Agentic Capability

Flash-Lite is designed for lightweight tasks (translation, content moderation, data extraction) and lacks the multi-step tool-use capability needed for OAT's merge-queue workflow. During the benchmark run, the merge-queue received explicit "please merge this PR" messages for 3 PRs with green CI but never executed a single `gh pr merge` tool call. Workers were able to produce PRs but the merge-queue was non-functional.

- **Impact:** Run terminated early. 0 PRs merged, no waves completed, effectively 0/100 score.
- **Recommendation:** Flash-Lite class models are not suitable for OAT benchmarks. The merge-queue and supervisor roles require strong instruction-following and tool-use capabilities.

### OpenRouter Llama 4 Scout / Llama 4 Maverick: Tool Calling Broken

Both Llama 4 models were tested via OpenRouter multiple times in March-April 2026. Neither produced a usable benchmark run.

- **Llama 4 Scout** (`openrouter:meta-llama/llama-4-scout`): Three attempts in March 2026. First returned `502 Internal error`. Second returned `429 temporarily rate-limited upstream` -- no providers had capacity. Third (Mar 22) returned `502` with a `500 Internal error` from the Google provider -- OpenRouter routed the request to Google Vertex AI (which hosts Llama 4 Scout as a managed model), and Google's backend returned an internal error.
- **Llama 4 Maverick** (`openrouter:meta-llama/llama-4-maverick`): Three attempts in March 2026. First two hung indefinitely on the first `gh issue list` command (API call never returned). Third (Mar 22) appeared to show progress -- the workspace agent generated text showing `oat worker create ... Worker created successfully: worker-1`. However, the daemon log confirms **no worker was ever created** (no `Added agent`, `Started registered agent`, or cleanup entries). Maverick hallucinated the entire tool call execution, including a fabricated worker log file with dates from 2024. The model generated plausible-looking command output without actually executing any tool calls.
- **Llama 4 Maverick Nitro** (`openrouter:meta-llama/llama-4-maverick:nitro`): Tested April 2026. All three agents (workspace, supervisor, merge-queue) started successfully, confirming the model string is valid and the API is responsive. However, when the workspace agent received its task, it output text *describing* the `oat worker create` command instead of executing it as a tool call. The daemon log shows zero `create_worker` socket requests. The workspace then entered an extended "Thinking..." state and never recovered. Same root cause as March attempt: the model emits tool calls as plain text in the content field.
- **Root cause:** This is a **known Llama 4 model-family issue** with tool calling, documented across multiple platforms: [meta-llama/llama-api-python#31](https://github.com/meta-llama/llama-api-python/issues/31), [pydantic-ai#2123](https://github.com/pydantic/pydantic-ai/issues/2123), [vllm#17109](https://github.com/vllm-project/vllm/issues/17109), [Groq community](https://community.groq.com/t/llama-4-tool-not-triggering/414). Both Scout and Maverick emit tool calls as plain text/JSON in the `content` field instead of structured `tool_calls` objects. The vLLM docs recommend using the `llama4_pythonic` tool parser with a specific chat template, but via OpenRouter you cannot control the provider's serving configuration. Additionally, Llama 4 models ignore tool calls entirely when a system prompt is provided (which oat-agent always does via AGENTS.md).
- **Impact:** All runs aborted. No workers created, no PRs, no progress.
- **Groq routing partially fixes parsing but not generation.** When Scout is routed through Groq on OpenRouter (via `extra_body` provider routing), simple single-turn tool-calling probes pass at 100/100 -- Groq correctly structures the response. However, in OAT's full agent context (12+ tools, complex system prompt, AGENTS.md), Scout workers produce **zero ASSISTANT output**. The model's tool-calling defect is in the weights, not the serving infrastructure: Groq fixes parsing but cannot fix the model's inability to generate proper tool calls in complex multi-tool contexts. Tested April 2026 with 2 Scout workers in a routed benchmark -- both produced 0.0 MB logs containing only daemon nudges.
- **Maverick provider investigation (April 2026):** Google Vertex AI is the only OpenRouter provider for Maverick that supports tool calls (9.45% error rate). All other providers (DeepInfra, Together, Parasail, NovitaAI, SambaNova) host Maverick for text only -- requesting tool use returns 404. Groq does not host Maverick at all. Vertex AI BYOK requires a GCP service account JSON key (not a simple API key), and OpenRouter's shared Google quota is heavily rate-limited.
- **No viable path via OpenRouter.** Even with correct tool-call parsing (Groq), the model fails at scale. Self-hosted vLLM with `--tool-call-parser llama4_pythonic` is the only untested option.

### DeepSeek V3.2 Speciale: No Tool Use Support on OpenRouter

DeepSeek V3.2 Speciale was tested via OpenRouter (`openrouter:deepseek/deepseek-v3.2-speciale`). The run failed on the first API call with `404 - No endpoints found that support tool use`. OpenRouter does not expose function calling / tool use endpoints for the Speciale variant.

- **Impact:** Run failed immediately. OAT requires tool use for all agent operations.
- **Recommendation:** Use standard DeepSeek V3.2 via OpenRouter, or try the Speciale model via a direct API that supports function calling.

### DeepSeek V3.2: Context Window Exhaustion (163K Limit)

During the DeepSeek V3.2 benchmark (`20260319-064322-deepseekv32-direct`), worker `nice-elk` hit the model's 163,840-token context limit after 25 nudges and 10 conflict wakes (140.2K tokens logged). The API returned `400 - maximum context length is 163840 tokens`. The worker was implementing JSON storage (the persistence backbone), so its failure caused a cascade: all downstream implementations lacked a working storage backend.

- **Impact:** PR #26 left with CI failures, directly causing "JSON data files exist" gate test to fail and contributing to inventory list, recipe list, and workflow cascade failures. Final score was 60.9/100 with only 5/21 PRs merged.
- **Contributing factor:** The merge-queue was hung on OpenRouter API calls for 50+ minutes at a time, unable to merge 7+ green PRs. This exacerbated the cascade because implementations that depended on the storage layer couldn't benefit from it even if it had merged.
- **Recommendation:** Models with small context windows (< 200K) may struggle with complex implementation tasks that require many nudge/conflict resolution cycles. Consider implementing context pruning or worker restart strategies for such models.

### OpenRouter API Hangs (All Models)

OpenRouter API calls hung indefinitely (50+ minutes observed) during the DeepSeek V3.2, Gemini 3.1 Pro, Gemini 3.1 Flash-Lite, and Llama 4 Maverick benchmark runs. The agent enters "Thinking..." and never receives a response. This affects both worker and merge-queue agents.

- **Impact:** Workers stuck in "Thinking..." produce no output and consume wave time slots. The merge-queue being stuck prevents all PR merges, causing green PRs to pile up. In the DeepSeek V3.2 run, 16 PRs went unmerged partly due to merge-queue hangs.
- **Root cause:** Unknown. Likely a combination of OpenRouter's routing layer, upstream provider capacity, and model-specific latency characteristics. No client-side timeout existed to detect and recover from the hang.
- **Fix applied:** Added streaming idle timeout (`OAT_STREAM_IDLE_TIMEOUT`, default 180s) and hard request timeout (`OAT_API_TIMEOUT`, default 1800s) to oat-agent. Also added `SendEscape` backend method so stuck agents can be interrupted via the TUI.

### Infinite Reasoning Without Action (Thinking Token Streams)

Some models stream continuous "thinking" tokens without ever producing text output or tool calls. The existing `OAT_STREAM_IDLE_TIMEOUT` does not catch this because stream chunks *are* arriving -- they just contain no actionable content (no `text` blocks, no `tool_call` blocks).

- **Observed with:** Qwen3.5 397B-A17B via OpenRouter (`openrouter:qwen/qwen3.5-397b-a17b`). The worker spent 12+ minutes in "Thinking..." on its first API call, continuously streaming thinking tokens. The 180s idle timeout never fired because the connection was actively sending data. Manual interruption was required to proceed.
- **Root cause:** Unclear whether this is a Qwen model behavior (extended reasoning that never converges) or an OpenRouter integration issue. The model may perform differently via direct API access.
- **Fix applied:** Added `OAT_MAX_THINKING_SECONDS` (default 600s / 10 min) to the non-interactive agent loop. This timeout tracks elapsed time since the last stream chunk that contained text or tool-call content. If the model streams for longer than this without producing any actionable output, the API call is aborted and the agent retries on the next nudge. Set to 0 to disable.
- **Related:** Qwen3 Coder Next (`openrouter:qwen/qwen3-coder-next`) exhibited a different failure mode -- the worker read all docs but got stuck in a loop claiming "I'll write the test now" without ever executing the `write_file` tool call. A "Network connection lost" error preceded the first write attempt and the model never recovered its tool-use capability. This is likely a tool-use limitation with large file generation via OpenRouter rather than a thinking timeout issue.

### Kimi K2 Thinking: `> ` Prefix on Every Tool Call

Kimi K2 Thinking (via OpenRouter, `openrouter:moonshotai/kimi-k2-thinking`) prepends `> ` to every `execute()` call. The `> ` is a shell redirect operator, causing `/bin/sh` to interpret `> oat pr create ...` as "redirect nothing to a file named `oat`" followed by bare words `pr create ...`, which fail with exit code 127 ("command not found"). The worker successfully read all documentation, wrote a 459-line test, committed, and pushed to its branch -- but could never execute any post-commit shell command (`oat pr create`, `oat agent complete`, `git status`, `chmod`, `ls -la`, `bash -c`, `stat`, `cat`, `head -20`). The supervisor (also K2 Thinking) exhibited a similar bug, prefixing commands with `: "..."` (colon = bash no-op), so all diagnostic commands returned empty output.

- **Impact:** Run aborted. Worker produced code but couldn't create a PR. Gate test was 0 bytes because the runner couldn't extract the test from the unmerged branch. 109.2K tokens consumed with no usable result.
- **Recommendation:** This is a model-level tool-use formatting defect. The sibling model Kimi K2.5 (`openrouter:moonshotai/kimi-k2.5`) does not exhibit this issue and scored 72/100. Avoid Kimi K2 Thinking for agentic coding workflows until the `> ` prefix issue is resolved upstream.

### Gemini 2.5 Flash and o4-mini: Poor OAT Workflow Adherence (Routed Benchmark)

In the routed o4-mini + Gemini 2.5 Flash + Sonnet 4.6 benchmark (2026-04-16), both Gemini 2.5 Flash and o4-mini struggled with OAT's multi-step commit-push-review workflow:

| Model | Workers | Nudged 10+ | PRs Merged | `pr_create` errors |
|-------|---------|------------|------------|-------------------|
| Gemini 2.5 Flash | 16 | 14 (88%) | **0** (0%) | 4 |
| o4-mini | 12 | 9 (75%) | 2 (17%) | 3 |
| Sonnet 4.6 | 14 | 0 (0%) | 3 (21%) | 0 |

- **Gemini 2.5 Flash:** Zero merged PRs across 16 worker assignments. 88% of workers were nudged 10+ times, 56% hit 20+ nudges. 4 workers tried a nonexistent `pr_create` tool instead of `oat worker request-review`. Workers frequently got stuck in edit loops or failed to complete the commit-push-review cycle.
- **o4-mini:** Moderate performance with 2 merged PRs. 75% nudge rate at 10+. 3 workers also exhibited `pr_create` tool confusion. Better than Flash but significantly worse than Sonnet.
- **Sonnet 4.6:** Zero high-nudge workers, 3 merged PRs, zero tool confusion. Served as the reliability anchor for the run.
- **Impact:** The high stuck rate caused replacement worker churn (42 workers for 26 issues = 16 replacements), with all 4 waves timing out. The run still achieved 95/100 acceptance and 72/100 gate thanks to Sonnet workers finishing what Flash/o4-mini started.
- **Recommendation:** Gemini 2.5 Flash's 0% PR merge rate and 88% stuck rate suggest it is not currently suitable for OAT worker tasks. o4-mini is marginal. Both models may improve with prompt tuning or model updates, but at present Sonnet-class models are required for reliable OAT workflows.

---

## OAT Bugs (Found During Benchmarks)

Bugs in OAT's daemon, backend, and agent runtime discovered during benchmark runs.

### Model Roster Shows Stale Profiles from Previous Runs

`GenerateModelRoster()` in `internal/routing/prompt.go` called `ps.Eligible(RoleWorker)` which returned ALL worker-eligible profiles from `~/.oat/model-profiles/`. There was no per-repo filtering, so models onboarded in previous benchmark runs (e.g., Llama 4 Scout) leaked into the roster for the current run, even when not specified in `--available-worker-models`. The daemon's `resolveAndValidateModel` and `BestEligible` also operated on the global profile pool, so workers could be auto-assigned stale models.

- **Impact:** Workers were assigned models not intended for the current benchmark run (e.g., Scout from a prior run appeared alongside DeepSeek + Haiku). This polluted benchmark results and wasted tokens.
- **Fix:** Added `AllowedWorkerModels []string` field to the `Repository` struct. Added `EligibleFiltered()` method to `ProfileStore`. Updated `GenerateModelRoster()` to accept and filter by allowed models. Updated `resolveAndValidateModel()` to enforce the allow-list for workers. Added `oat config <repo> worker-models set|add|remove|list|clear` subcommand and `--allowed-worker-models` flag. Benchmark `run.sh` now calls `oat config --allowed-worker-models` after onboarding to restrict each run.
- **Files changed:** `internal/state/state.go`, `internal/routing/profiles.go`, `internal/routing/prompt.go`, `internal/daemon/daemon.go`, `internal/cli/cli.go`, `benchmarks/run.sh`

### OpenRouter Context-Window Fetch Uses Nonexistent Endpoint

`_fetch_openrouter_context_length()` in `benchmarks/probe-model.py` queried `GET /api/v1/models/{model_id}` — a single-model endpoint that does not exist on OpenRouter (returns 404). Additionally, routing suffixes like `:nitro` and `:extended` are not part of the canonical model ID, so even the correct endpoint would fail to find them.

- **Impact:** Models onboarded via OpenRouter (e.g., `deepseek/deepseek-v3.2:nitro`) had "No context window limit in model profile" warnings despite having known context windows. This caused the profile to record `max_input_tokens: 0`, reducing routing quality.
- **Fix:** Rewrote the function to query the full `GET /api/v1/models` list endpoint, strip known routing suffixes (`:nitro`, `:extended`, `:free`, `:floor`) before searching, and cache the full list at module level. Falls back to original model ID if suffix-stripped version not found.
- **Files changed:** `benchmarks/probe-model.py`

### Direct Backend PTY Short Writes (Message Truncation)

`SendMessage()` in `pkg/backend/direct_backend.go` performed a single `syscall.Write` to the agent's PTY and ignored the return value `n`. If the PTY buffer was full, remaining bytes were silently dropped, causing truncated messages. The merge-queue received garbled messages with `▆▆` artifacts mid-word during the DeepSeek V3.2 benchmark.

- **Impact:** Agents received incomplete instructions, leading to confusion and wasted tokens.
- **Fix:** Replaced the single write with a loop that retries until all bytes are written.
- **Files changed:** `pkg/backend/direct_backend.go`

### Post-Completion Nudge Race Condition

The daemon's `wakeAgents()` took a snapshot of all agents via `GetAllRepos()` (deep copy under RLock). If a worker completed between the snapshot and the nudge send, the stale snapshot still showed `ReadyForCleanup=false`, causing unnecessary nudges to already-completed workers. Observed with `swift-octopus` in the DeepSeek V3.2 run, which received "you have uncommitted changes" nudges after successfully completing.

- **Impact:** Completed workers received confusing messages and wasted tokens trying to re-enter dormant mode.
- **Fix:** Added fresh state re-checks (via `GetAgent`) before sending nudges in `nudgeWorkerEscalating()` and `checkWorkerProgress()`. If `ReadyForCleanup` is true, the nudge is skipped.
- **Files changed:** `internal/daemon/stuck_worker.go`

### Dormancy Cap Abandoned Green PRs

`handleDormancyTimeout()` force-completed workers after 30 minutes regardless of PR status. In the DeepSeek V3.2 run, `noble-bear` had a green, mergeable PR #39 that the merge-queue never processed because it was hung on an OpenRouter API call. The dormancy cap killed the worker, wasting its work.

- **Impact:** Working, mergeable PRs were abandoned because the merge-queue was broken.
- **Fix:** Before timing out, the daemon now queries PR status. If the PR is open, mergeable, and all CI checks pass, it force-merges via `gh pr merge --squash` and auto-completes the worker. The dormancy cap is now configurable via `OAT_WORKER_DORMANCY_CAP_MINUTES` (default 30).
- **Files changed:** `internal/daemon/pr_monitor.go`

### Fast-Merge and Force-Merge Failing Due to `--delete-branch` with Active Worktrees

`fastMergeWorkerPR` and `forceMergeWorkerPR` used `gh pr merge --squash --delete-branch`. The `--delete-branch` flag tries to delete the local branch after merging, but git refuses because the worker's worktree still references it: `error: cannot delete branch 'work/<name>' used by worktree`. This caused every fast-merge attempt to fail and fall back to the merge-queue LLM, effectively disabling `OAT_FAST_MERGE` entirely. Same root cause as the earlier merge-queue prompt issue (see "`--delete-branch` Flag Fails with Active Worktrees" above) but in daemon code rather than the prompt.

- **Impact:** During a DeepSeek V3.2 Nitro benchmark, all 5 fast-merge attempts failed. The merge-queue LLM had to handle every merge as a fallback, negating the purpose of `OAT_FAST_MERGE`.
- **Fix:** Removed `--delete-branch` from both `fastMergeWorkerPR` and `forceMergeWorkerPR`. Branch cleanup is handled by the daemon's `cleanupMergedBranches()` after the worktree is removed.
- **Files changed:** `internal/daemon/pr_monitor.go`

### Workers Going Dormant Immediately After CI Failure Wake (Dormancy Loop)

When the daemon woke a dormant worker for CI failure, the worker (especially with DeepSeek) would immediately run `oat agent waiting` again without fixing the issue. `handleAgentWaiting` checked for merge conflicts before allowing dormancy but did not check CI status. The worker went dormant, the daemon detected CI failure again, woke the worker again — creating a rapid wake-dormant-wake loop. After 5 cycles (typically under 2 minutes), the daemon escalated to the supervisor, which force-removed the worker, leaving the PR orphaned with failing CI.

- **Impact:** During a DeepSeek V3.2 Nitro benchmark, workers `cosmic-badger` (PR #30) and `mega-lion` (PR #28) were caught in this loop and force-removed, leaving PRs with failing CI and no worker to fix them. Issue #10 (Inventory CLI) remained open with an orphaned PR.
- **Fix:** Added a CI failure guard in `handleAgentWaiting`: if `hasFailedChecks()` returns true for the worker's PR, dormancy is rejected with an error message directing the worker to check and fix CI first. This breaks the loop because the worker cannot go dormant until CI passes (or at least is no longer in a failed state — pending is allowed since the worker may have just pushed a fix).
- **Files changed:** `internal/daemon/daemon.go`

### No Escape Key Mechanism for Stuck Agents

The TUI's "interrupt agent" keybinding (`ctrl+x`) sent Ctrl-C (0x03) via `SendInterrupt`, which could kill the agent process. The agent's "Thinking..." state requires Escape (0x1b) to cancel gracefully without terminating.

- **Impact:** No way to safely interrupt a stuck agent from the TUI without risking process death.
- **Fix:** Added `SendEscape` to the `ProcessBackend` interface (both direct PTY and tmux implementations). Added `escape_agent` socket command. TUI's `ctrl+x` now sends Escape instead of Ctrl-C.
- **Files changed:** `pkg/backend/backend.go`, `pkg/backend/direct_backend.go`, `pkg/backend/tmux_backend.go`, `pkg/tmux/client.go`, `internal/daemon/daemon.go`, `internal/tui/app.go`, `internal/tui/keys.go`

### No Client-Side API Timeout (oat-agent)

The Python agent runtime (`deepagents_cli`) made LLM API calls with no timeout. If the provider hung (observed for 50+ minutes with OpenRouter), the agent waited indefinitely in "Thinking..." with no way to recover.

- **Impact:** Workers and merge-queue agents became permanently stuck, blocking wave progression and PR processing.
- **Fix:** Two complementary timeouts: (1) Streaming idle timeout (`OAT_STREAM_IDLE_TIMEOUT`, default 180s) -- aborts if no stream chunks arrive for 3 minutes. Legitimate thinking sends tokens continuously; a hung connection sends zero bytes. (2) Hard request timeout (`OAT_API_TIMEOUT`, default 1800s) -- generous safety net passed as `request_timeout` to the model constructor.
- **Files changed:** `agent-runtime/libs/cli/deepagents_cli/config.py`, `agent-runtime/libs/cli/deepagents_cli/non_interactive.py`

### `request_timeout` Parameter Name Varies by Provider (Fixed)

The hard request timeout fix initially used `request_timeout` for all providers. Anthropic's langchain integration uses `default_request_timeout` instead, causing `AsyncMessages.create() got an unexpected keyword argument 'request_timeout'` on startup.

- **Impact:** All Anthropic-backed agents crashed immediately on startup. Discovered in the Sonnet 4.6 direct-backend run.
- **Fix:** Made the parameter provider-aware: Anthropic gets `default_request_timeout`, all others get `request_timeout`.
- **Files changed:** `agent-runtime/libs/cli/deepagents_cli/config.py`

### Duplicate Worker Spawning (Workspace + Supervisor)

During benchmark runs where `run.sh` instructs the workspace agent to manage wave progression, the supervisor also independently spawned workers for the same issues. Both agents saw open issues with no active workers and created workers, resulting in 8 duplicate workers (30 total for 21 issues) in the Sonnet 4.6 run -- ~505K tokens wasted (~19% of total).

- **Impact:** ~19% token waste from duplicate workers doing redundant work. The brownian ratchet philosophy means duplicates self-resolve (faster worker wins), but the waste is significant.
- **Fix:** (1) `run.sh` now sends a coordination message to the supervisor at the same time as the workspace wave message, telling it the workspace owns worker creation. (2) `supervisor.md` now instructs the supervisor to defer spawning when the workspace is driving worker creation, and to check `oat worker list` before spawning any worker.
- **Files changed:** `benchmarks/run.sh`, `internal/prompts/supervisor.md`
- **Follow-up (2026-04-17):** The original fix only addressed the supervisor side. The workspace prompt was missing a duplicate-issue guard, causing the workspace to spawn workers for issues that already had active (stuck) workers. Added `### Avoiding Duplicate Issue Assignments` section to `workspace.md` instructing the workspace to check `oat worker list` before spawning. Observed in the routed o4-mini + Flash + Sonnet 4.6 benchmark where wave:1 issues #5 and #6 had both stuck workers and duplicate replacements simultaneously.

### Supervisor Spawning Agents Proactively on Restore

When the daemon restores a repo (daemon restart, health check, or after hibernate), `restoreRepoAgents()` previously only started supervisor + workspace. The daemon's init message told the supervisor to "Review these definitions and determine which agents to spawn." On restore, the supervisor followed this literally: it scanned for open GitHub issues and spawned workers, even when no work was intended (e.g., after a `--gate-only` benchmark run). Combined with `run.sh --gate-only` not stopping agents after results collection, this produced runaway agents in the Opus 4.6 benchmark that merged Wave 0 PRs and spawned workers for issues #4 and #5 unsupervised.

- **Impact:** Unattended agents burned tokens and made unsupervised changes to the benchmark repo after the gate-only run had completed.
- **Fix:** Four-part fix: (1) `run.sh` stops agents after gate-only via `oat repo hibernate --all --yes`, (2) supervisor prompt removes proactive spawning instructions (replaced with "What You Don't Do" section), (3) daemon init message becomes informational instead of instructional, (4) `restoreRepoAgents()` now starts all persistent agents (merge-queue, pr-shepherd) directly instead of delegating to the supervisor.
- **Files changed:** `benchmarks/run.sh`, `internal/prompts/supervisor.md`, `internal/daemon/daemon.go`

### inferAgentContext Identity Confusion in Worktrees

`inferAgentContext()` derived agent identity from the cwd path for worktree directories (`~/.oat/wts/<repo>/<agent>`). If a fix worker operated in another worker's worktree (e.g., `fancy-gecko` running in `bold-badger`'s worktree to fix CI), `oat agent complete` would mark the wrong agent as complete. The `OAT_AGENT_NAME` env var (set by the backend for all agents) was checked for repo paths but ignored for worktree paths.

- **Impact:** In the Sonnet 4.6 run, `fancy-gecko` (a merge-queue CI-fix agent) completed `bold-badger` instead of itself. The daemon continued nudging the orphaned `fancy-gecko` (5 nudges).
- **Fix:** The worktree branch of `inferAgentContext()` now checks `OAT_AGENT_NAME` first, falling back to the path-derived name only if the env var is unset.
- **Files changed:** `internal/cli/cli.go`

### Merge-Queue CI-Fix Workers Missing `--push-to`

The merge-queue prompt instructed spawning CI-fix workers with `--branch <pr-branch>` but without `--push-to <pr-branch>`. The fix worker would create a separate `work/<fix-worker>` branch instead of pushing to the existing PR branch, unable to update the original PR.

- **Impact:** CI-fix workers could not directly fix the PR they were spawned for. In the Sonnet 4.6 run, `fancy-gecko` had to manually navigate to `bold-badger`'s worktree and push from there, triggering the identity confusion bug above.
- **Fix:** Updated the merge-queue prompt to include `--push-to <pr-branch>` in the fix worker spawn command.
- **Files changed:** `internal/templates/agent-templates/merge-queue.md`

### Benchmark: Wave 0 and Post-Wave-4 Missing Merge Grace Period

When wave 0 times out (30m), `assemble_gate_test` runs immediately with no wait for pending PR merges. Similarly, after waves 1-4 complete, the convergence loop immediately runs the blackbox test. Fix waves have a merge wait (180s), but wave 0 and the post-wave-4 transition have none.

- **Impact:** PRs that pass CI right after the wave timeout are missed by the gate test or convergence test. In the Scout+DeepSeek benchmark, PR #27 (submitted by `stellar-raccoon`) was likely not merged before the gate test was assembled.
- **Fix:** Added a 60-second merge grace period after wave 0 timeout (before `assemble_gate_test`) and after the wave 1-4 loop (before the convergence test), both on the timeout path only. The "all issues closed" path doesn't need it because issues close via `Closes #N` in the merged PR.
- **Files changed:** `benchmarks/run.sh`

### Stale Verifier Lock Blocking Re-Request

When a worker requests verification re-review (e.g., after fixing CI), `handleStartVerificationAgent` checks whether the old verifier is still alive. If the verifier had already called `oat agent complete` (setting `ReadyForCleanup = true`) but its process was still running during the 3-second post-completion cleanup delay, the alive check returned `true` and the re-request was rejected with "agent 'verify-X' is still running." In the GPT 5.4 Mini + Nano + Haiku benchmark, `calm-bear` hit this error 8 times in a loop.

- **Impact:** Workers unable to get re-verification; loop until the health check cleans up the verifier (up to 2 minutes)
- **Root cause:** The alive check ran before checking `ReadyForCleanup`, so a completed-but-not-yet-killed verifier blocked re-requests
- **Fix:** Check `ReadyForCleanup` first in `handleStartVerificationAgent`. If the existing verifier is already marked complete, force-retire it (stop process + remove from state) regardless of whether its process is still alive, then proceed with re-request.
- **Files changed:** `internal/daemon/daemon.go`

### "Already Completed" Race Condition (Agent Self-Completion After Daemon Auto-Complete)

When the daemon auto-completes an agent (e.g., on PR merge or verdict delivery), it marks the agent `ReadyForCleanup` and schedules process kill after a 3-second grace period. During that grace period, the agent may call `oat agent complete` or `oat agent waiting`, receiving a hard error: "Agent 'X' has already completed. You are DONE." This alarming error causes agents to panic and attempt more commands, generating noise in logs. In the GPT 5.4 Mini + Nano + Haiku benchmark, 23 agents hit this error (19 verifiers, 3 workers, 1 verifier running a test command).

- **Impact:** Log noise; agents attempt unnecessary recovery commands after receiving the error
- **Root cause:** `handleCompleteAgent` and `handleAgentWaiting` returned `ErrorResponse` when the agent was already `ReadyForCleanup`, despite the desired state being achieved
- **Fix:** Changed both handlers to return `SuccessResponse` with `{"status": "already_complete"}` when `ReadyForCleanup` is true. For `handleCompleteAgent`, also captures the agent's provided summary if the daemon's auto-completion left it blank.
- **Files changed:** `internal/daemon/daemon.go`

### Token Delta Emission Bug in Non-Interactive Agent Runtime

In `agent-runtime/libs/cli/deepagents_cli/non_interactive.py`, the `_emit_oat_tokens` function emitted cumulative token values in both the `delta_input`/`delta_output` and `cumulative_input`/`cumulative_output` fields. The actual per-call deltas (computed on lines 536-537) were never passed to the emission function.

- **Impact:** The `delta_input`/`delta_output` fields in `[OAT_TOKENS]` log lines showed cumulative values masquerading as deltas. Anyone debugging per-call token usage from logs would get incorrect numbers. Total token counts were unaffected since `collect.sh` uses cumulative values.
- **Root cause:** `_emit_oat_tokens` only accepted cumulative values and used them for both delta and cumulative fields
- **Fix:** Updated `_emit_oat_tokens` signature to accept all four values (delta_input, delta_output, cumulative_input, cumulative_output) and passed the computed deltas from the call site. Aligns with the existing correct implementation in `textual_adapter.py`.
- **Files changed:** `agent-runtime/libs/cli/deepagents_cli/non_interactive.py`
- **Note:** The interactive TUI runtime (`textual_adapter.py`) already had the correct implementation with separate delta tracking via `_spend_tracker`.

### Missing `max_input_tokens` Profile for OpenAI GPT 5.4 Models

When onboarding OpenAI models (GPT 5.4 Mini, GPT 5.4 Nano) via `oat model onboard`, `probe-model.py` could not detect `max_input_tokens` because LangChain's model profile doesn't expose it for direct OpenAI models and there was no provider-specific API fallback (only OpenRouter had one).

- **Impact:** The summarization middleware (`agent-runtime/libs/deepagents/deepagents/middleware/summarization.py`) falls back to conservative fixed defaults: triggers at 170K tokens (vs 85% of actual 400K = 340K) and keeps only 6 messages (vs proportional). This causes agents to lose context earlier than necessary, potentially re-reading files and repeating work, driving up token consumption. The GPT 5.4 Mini + Nano + Haiku benchmark used ~167M tokens, partly attributable to this.
- **Fix:** Added `_fetch_openai_context_length()` to `probe-model.py` that queries OpenAI's `GET /v1/models/{model}` endpoint for `context_window` when LangChain doesn't provide it. The fallback requires `OPENAI_API_KEY` and gracefully returns None on failure.
- **Files changed:** `benchmarks/probe-model.py`
- **Note:** Other direct-API providers (xAI/Grok, DeepSeek) may need similar provider-specific fallbacks in the future.

---

## Benchmark Tooling Issues

> **Note:** Some entries below reference tmux commands from the original architecture. OAT now uses a direct PTY backend — these tmux references are historical.

Issues in the benchmark scripts (`run.sh`, `collect.sh`, `summarize.sh`, `acceptance-test.sh`).

### Convergence Loop Running Stale Test Snapshot

The convergence loop ran `$RUN_DIR/gate-generated-test.sh` (a snapshot copied at gate time), but fix-wave workers modified `scripts/blackbox-test.sh` in the repo. Workers successfully fixed bugs (e.g., the `run_cmd` nesting anti-pattern), but the convergence loop never picked up the fix -- all iterations ran the identical stale, buggy test.

- **Impact:** Observed in the Haiku 4.5 benchmark (`sonnet46-verifycommandtest`). Workers merged PR #51 fixing the blackbox test's bash incompatibility, but all 4 convergence iterations ran the original broken test. The convergence loop declared failure despite the fix being merged.
- **Fix:** Before each `run-blackbox.sh` invocation in the convergence loop, the script now re-extracts `scripts/blackbox-test.sh` from the repo via the GitHub API and overwrites the local copy. The original gate snapshot is preserved as `gate-generated-test-original.sh` for audit. This does not break anti-cheat: the test is still run by deterministic benchmark software, agents never see test output, and the acceptance test remains untouched.
- **Files changed:** `benchmarks/run.sh`

### False-Positive Convergence on Zero Test Results

When a model-generated blackbox test crashed silently (e.g., `declare -A` on macOS bash 3.2), it exited with code 0 but produced zero PASS/FAIL results. `run-blackbox.sh` only checked exit code, so it reported "0 passed, 0 failed" as a passing test, causing false-positive convergence.

- **Impact:** The gate scored 82/100 for a test that crashed on execution. The convergence loop would have declared success on a test that never actually tested anything.
- **Fix:** Added a zero-test guard in `run-blackbox.sh`: if exit code is 0 but total tests is 0, override exit code to 1. Also added an execution smoke test in `run.sh`'s gate phase that runs the generated test before investing time in waves.
- **Files changed:** `benchmarks/run-blackbox.sh`, `benchmarks/run.sh`

### Confusing Blackbox Output on FATAL Abort

When the blackbox test had FATAL aborts (e.g., setup steps failing and aborting remaining tests), the output showed "17 passed, 0 failed" with exit code 1. This looked like success to a human observer -- the exit code 1 was the only indication of failure, and it was unclear why it failed.

- **Impact:** During convergence, terminal watchers couldn't understand why the test was "failing" when it showed 0 failures. Made debugging convergence issues unnecessarily difficult.
- **Fix:** `run-blackbox.sh` now counts FATAL/ABORT lines in the raw output and includes them in the results summary ("17 passed, 0 failed (3 FATAL aborts)"), the JSON output (`fatal_count`, `fatal_lines` fields), and the terminal results banner.
- **Files changed:** `benchmarks/run-blackbox.sh`

### Diagnostic Truncation in Blackbox Test Output

The blackbox test's `setup_cmd` pattern used `head -c 200` to truncate error output in FATAL messages. A Python traceback through Click is 1000+ chars, so critical information like `KeyError: 'ingredient_name'` was completely lost. The convergence loop received only `"Traceback (most recent call last):"` with no actual exception, making it impossible to identify the root cause.

Additionally, `run.sh` extracted failure lines via `grep -iE 'FAIL|ERROR'`, which captured symptom lines ("validate order fails") but not the underlying exceptions.

- **Impact:** During the Haiku 4.5 convergence run, fix-wave workers repeatedly fixed peripheral issues (state persistence) while the actual crash (`KeyError`) was invisible. Two convergence iterations were wasted on symptoms.
- **Fix:** (1) Updated `blackbox-testing.md` `setup_cmd` pattern from `head -c 200` to `head -c 500` plus `tail -1` for the last error line. (2) `run.sh` now extracts Python exceptions (KeyError, TypeError, etc.) from raw output and includes them as "Error diagnostics" in the convergence message. (3) `fatal_lines` from the JSON output are also included.
- **Files changed:** `benchmarks/blackbox-testing.md`, `benchmarks/run.sh`

### Workspace Skipping Convergence Fix Iterations

The Haiku workspace agent received the fix-1 convergence message but didn't create issues, reasoning "failures are identical, workers still in progress" when fix-0 workers had already completed. This was a compound failure: identical symptom-only messages gave no signal that previous fixes were ineffective, `oat worker list` showed dormant workers as "running," and Haiku's reasoning incorrectly concluded same symptoms meant workers were still handling it.

- **Impact:** fix-1 iteration was entirely skipped, wasting one convergence cycle.
- **Fix:** (1) `run.sh` convergence prompt now includes previous iteration summary (issues created, PRs merged) and result diffs. (2) `oat worker list` shows "waiting for PR" for dormant workers. (3) Convergence prompt instructs checking `gh pr list --state merged`. (4) `workspace.md` mandates immediate action on every convergence message.
- **Files changed:** `benchmarks/run.sh`, `internal/prompts/workspace.md`, `internal/daemon/daemon.go`, `internal/cli/selector.go`

### `collect.sh` Crash on Missing Token Data

`collect.sh` uses `set -euo pipefail` and passes token variables to `jq --argjson`. When token tracking is unavailable (e.g., UI change removing token display), variables like `TU_PER_WORKER` are empty strings, which is invalid JSON for `--argjson`. The script crashes before writing `collect.json`, losing all operational metrics (worker autonomy, wave timing, PR data) -- not just token data.

- **Impact:** No `collect.json` produced for the entire benchmark run. Observed in the Haiku 4.5 v2 benchmark (`20260328-043720-haiku45-verifycommandtest`) where all 30 workers had N/A token data.
- **Fix:** Added defensive defaults for all token variables before the `jq --argjson` calls (empty -> `0` or `{}`). Also wrapped the final report assembly in an error handler that writes a partial `collect.json` rather than losing everything.
- **Files changed:** `benchmarks/collect.sh`

### Convergence Fix-Wave PRs Not Merged Before Re-Test

After convergence fix-wave issues close (workers call `oat agent complete`), `run.sh` waits 30 seconds then re-runs the blackbox test against `main`. But issues close when workers complete, which happens before the merge-queue merges their PR into main. The test runs against `main` without the fix, producing identical results and wasting a convergence iteration.

- **Impact:** Observed in the Haiku 4.5 v2 benchmark (`20260328-043720-haiku45-verifycommandtest`). Convergence iteration 2 was identical to iteration 1 (42 passed, 2 failed) because PR #52 hadn't merged yet. Convergence consumed all 4 iterations when 3 would have sufficed.
- **Fix:** Added a merge verification loop after fix-wave completion: polls `gh pr list --state merged` every 15 seconds (up to 2 minutes) before re-running the blackbox test.
- **Files changed:** `benchmarks/run.sh`

### `collect.sh` Partial Report on Non-Token argjson Failure

The previous fix for `collect.sh` (defensive defaults for token variables) was incomplete. The final REPORT `jq` assembly at line 561 uses `--argjson` for many variables beyond tokens: `$WORKER_AUTONOMY`, `$VERIFICATION_METRICS`, `$WAVE0`-`$WAVE3`, `$PRS_DETAIL`, `$TOTAL_ACTIVE_SECONDS`, and more. If any of these contain empty strings or invalid JSON (due to upstream `jq` failures, missing data, or the script running before all data is available), the entire report assembly fails and only a minimal partial `collect.json` is written.

- **Impact:** Observed in the Haiku 4.5 v3 benchmark (`20260328-072046-haiku45-verifycommandtest`). `collect.json` contained only model, repo, timestamps, and basic issue/PR counts. All wave detail, worker autonomy, verification metrics, and per-PR data were lost.
- **Fix:** Added `ensure_json()` and `ensure_number()` helper functions that validate and default every `--argjson` variable before the REPORT assembly. Complex JSON objects (`WAVE0`-`WAVE3`, `WORKER_AUTONOMY`, `VERIFICATION_METRICS`, `TOKEN_USAGE`, `PRS_DETAIL`) are validated with `jq empty` and defaulted to `{}` or `[]`. Scalar variables (`ISSUES_TOTAL`, `PRS_CI_PASSED`, etc.) are validated as numbers and defaulted to `0`.
- **Files changed:** `benchmarks/collect.sh`

### Negative Issue Counts in Convergence Loop

The `wait_for_wave` function captured `total_issues` once at the start, but the workspace may create additional issues with the same wave label during the wave. When computing `closed = total - open`, the result went negative because `open > total`, displaying confusing output like "Wave fix-0: -4/1 issues closed."

- **Impact:** Observed in the Haiku 4.5 v3 benchmark (`20260328-072046-haiku45-verifycommandtest`). Terminal output showed negative issue counts, making it impossible to understand convergence progress.
- **Fix:** `total_issues` is now re-fetched on every poll iteration, right after `open_count`, so the count always reflects the current state.
- **Files changed:** `benchmarks/run.sh`

### Circular CI Dependency from Parallel CLI Registration PRs

When the workspace decomposed CLI command implementation into separate parallel issues (one per command group), each worker's PR only registered its own commands. But the CI test suite (established by a wave 0 contract test worker) required ALL commands to be present. No PR could pass CI individually, creating a deadlock where none could merge.

Fix-wave workers compounded the problem by creating even more granular "register one command" issues instead of consolidating.

- **Impact:** Root cause of the 10.8/100 acceptance score in the Haiku 4.5 v3 benchmark (`20260328-072046-haiku45-verifycommandtest`). 11 PRs remained open with failing CI. The same benchmark with a more lenient wave 0 contract test scored 74.6/100 in the v2 run.
- **Fix:** (1) Added Patch 12 to `setup.sh` making CLI registration coupling explicit in the spec. (2) Added circular CI dependency guidance to `workspace.md` convergence section advising consolidation instead of fragmentation.
- **Files changed:** `benchmarks/setup.sh`, `internal/prompts/workspace.md`

### Rebase Loop Token Waste

Dormant workers stuck in a CI-failure/conflict loop were woken every 2-minute daemon cycle to rebase and push, even when the CI failure was caused by an external dependency (another PR needing to merge first). Workers like quantum-leopard were woken 23 times with no CI improvement, each wake consuming tokens for a fruitless rebase-push-wait cycle.

Additionally, when a PR had both merge conflicts AND failing CI, the dormant worker wake message only mentioned conflicts (due to switch statement priority). The worker would fix conflicts, go dormant, then get a separate CI wake -- ping-ponging between two wakes for what should be one.

- **Impact:** In the Haiku 4.5 v3 benchmark, 5 workers accumulated 85+ conflict/CI wakes combined with zero CI improvement.
- **Fix:** (1) Added an in-memory circuit breaker counter in `pr_monitor.go` that tracks conflict/CI wakes per worker. After 10 wakes without improvement, the daemon escalates to the supervisor with investigation steps instead of waking the worker again. Counter resets when PR state improves (merged, CI passes). (2) Combined wake messages: when a dormant worker's PR has both conflicts and failing CI, the conflict wake message now also notes the CI failure so the worker can address both in one cycle.
- **Files changed:** `internal/daemon/pr_monitor.go`, `internal/daemon/daemon.go`

### Wave Timing Using PR Timestamps Instead of Actual Wall-Clock

`collect.sh`'s `wave_timing()` derived wave start/end from PR `createdAt`/`mergedAt` timestamps. Since workers create PRs at different times than the benchmark script starts each wave, this produced overlapping and incorrect wave timings.

- **Fix:** `run.sh` now writes actual wall-clock wave start/end epochs to `wave-timing.json`. `collect.sh` reads this file when available, falling back to PR-derived timing only if absent.
- **Files changed:** `benchmarks/run.sh`, `benchmarks/collect.sh`

### Worker Count Always Zero in collect.json

`collect.sh` read `task_history` from `state.json` for worker counts, but `task_history` is only populated when `cleanupDeadAgents` runs (requires the completion delay to pass). If the daemon was restarted or workers were trapped in nudge loops, `task_history` remained empty.

- **Fix:** Added fallback that counts workers from PR data (`work/*` branches) when `task_history` is empty.
- **Files changed:** `benchmarks/collect.sh`

### Daemon Log Signals Missing Cleanup Events

`summarize.sh`'s daemon log grep pattern didn't include "ready for cleanup" or "marked as cleanup" events, making it harder to diagnose worker lifecycle issues.

- **Fix:** Added `ready.*cleanup|marked.*cleanup` to the grep pattern.
- **Files changed:** `benchmarks/summarize.sh`

### Workspace Not Receiving Messages from Benchmark Script

Messages sent to the workspace agent via `tmux send-keys` / `paste-buffer` were silently lost when the Textual TUI was stalled or not accepting input.

- **Fix:** Refactored `send_to_workspace()` with SIGWINCH pre-wake, empty Enter to re-engage the TUI, exit code checking, pane content change verification, and retry logic.
- **Files changed:** `benchmarks/run.sh`

### Truncated IPC Messages to Merge-Queue

Long messages sent via `SendKeysLiteralWithEnter()` were occasionally truncated by tmux, delivering partial messages to agents.

- **Fix:** Enhanced `SendKeysLiteralWithEnter()` to verify both the beginning and end of the message are present in the pane, with retry on truncation.
- **Files changed:** `pkg/tmux/client.go`

### Summarizer Nudge Counts Not Scoped by Repo

`summarize.sh`'s nudge count grep (`grep -c "Nudged worker ${worker_name} "`) searched the entire daemon log without scoping by repo. Since daemon.log contains entries from all repos, nudge counts from previous benchmark runs bled into the current run's metrics. All other loop-detection counters (completion, rebase, sync) correctly included `${repo}` in the pattern.

- **Impact:** The Opus 4.6 benchmark summary reported 4-6 nudges for workers that were actually nudged 0-2 times in the current run. The LLM summarizer then falsely concluded workers were "nudged while dormant."
- **Fix:** Added `.*${repo}` to the nudge count grep pattern, matching the style of the other counters.
- **Files changed:** `benchmarks/summarize.sh`

### Summarizer Log Rotation Creating Phantom Duplicate Workers

OAT rotates worker output logs when they exceed 10MB (copy-then-truncate, keeping the same inode for the log writer). This creates `worker.log` + `worker.log.YYYYMMDD-HHMMSS`. The summarizer's `find -name "*.log*"` picked up both files, extracted the same worker name from each, and presented two entries. The LLM summarizer then incorrectly inferred the worker was "spawned twice."

- **Impact:** Workers with large logs (silver-panda at 13.5 MB, mega-rabbit at 12.8 MB) appeared as duplicates in the Opus 4.6 summary, with narratives about "the first instance burned significant tokens before being superseded."
- **Fix:** Changed the `find` pattern from `"*.log*"` to `"*.log"` to only pick up active log files. Pre-rotation content is already captured in daemon.log-based metrics. Added an LLM prompt hint explaining log rotation.
- **Files changed:** `benchmarks/summarize.sh`

### Stale Output Logs Surviving Repo Removal

`oat repo rm` and `cleanup.sh` did not clean up `~/.oat/output/<repo>/` (worker output logs). When a benchmark was restarted with the same repo name, old worker logs from the aborted run survived. The summarizer's time-window filter picked them up as workers from the current run.

- **Impact:** The Opus 4.6 summary reported 5 "orphaned workers" (forest-bison, storm-octopus, azure-cheetah, frost-octopus, stellar-eagle) that were actually from a 70-second aborted run that preceded the successful benchmark.
- **Fix:** Added output directory cleanup to `oat repo rm` (in `cli.go` `removeRepo`), `cleanup.sh`, and a pre-run cleanup step in `run.sh`.
- **Files changed:** `internal/cli/cli.go`, `benchmarks/cleanup.sh`, `benchmarks/run.sh`

### Spec Gap: BARISTA_DATA_DIR Not Documented

The acceptance test (`acceptance-test.sh`) creates an isolated temp directory and exports `BARISTA_DATA_DIR` to configure the data file location. However, `BARISTA_DATA_DIR` was never mentioned in the operational specification that workers follow. Workers implemented the spec correctly (fixed data path), but the acceptance test expected configurable paths.

- **Impact:** The Opus 4.6 run scored 49.4/100 on acceptance tests. 50 of the 50.6 lost points traced to this single undocumented requirement (40 points from workflow, 10 from gate/persistence). The recipe add output format was also vague ("Success message with recipe ID" with no example), costing an additional 0.6 points.
- **Fix:** Added two spec patches to `setup.sh`: Patch 5 adds `BARISTA_DATA_DIR` env var to the Persistence section, Patch 6 adds an explicit output example for `recipes add`. Also added explicit `docs/operational-specification.md` paths to all 20 benchmark issues in `issues.json`.
- **Files changed:** `benchmarks/setup.sh`, `benchmarks/issues.json`

### Spec Patch Appended to End of File Instead of CLI Section

**Note:** This issue is historical — the spec patches are now pre-applied in the bundled `benchmarks/robotic-barista/` source. The patching code has been removed from `setup.sh`.

`setup.sh` Patch 7 (entry point requirement) checked for headings like `## CLI Command Groups`, `### CLI Command Groups`, and `## Commands` to find the CLI section in the operational specification. None of those headings existed in the robotic-barista spec — it uses `## CLI Commands`. The code fell through to the `else: content += entry_point_section` fallback, which appended the entry point requirement to the very end of the file (~line 401) instead of inserting it before the CLI commands section.

- **Impact:** Workers never saw the entry point requirement because it was buried at the bottom of the spec. The `barista` CLI entry point was not configured, causing acceptance test failures.
- **Fix:** Added `## CLI Commands` as the first heading check target in Patch 7, so it matches the actual heading in the spec. Patch 9 (BARISTA_DATA_DIR in CLI section) was written without an `else: content +=` fallback to avoid the same class of bug — if no heading matches, the patch is silently skipped rather than appended to the wrong location.
- **Files changed:** `benchmarks/setup.sh`
- **Prevention:** When adding new spec patches to `setup.sh`, always verify the target heading exists in the actual spec. Avoid `else: content +=` fallbacks that append to the end of the file — a silently skipped patch is better than a misplaced one.

### Summarizer Rebase/Sync Counts Inflated by TUI Re-Renders

`summarize.sh` counted rebase mentions and worktree sync events from worker output logs, which are inflated by Textual TUI re-renders (same bug class as the previously-fixed nudge/completion counts). Produced counts like 1,136 rebases when daemon log showed 4 actual events.

- **Fix:** Sourced all loop-detection counts from `daemon.log` instead of worker output logs. Added `| uniq` to key signals extraction to deduplicate consecutive identical lines. Added a TUI inflation caveat to the LLM prompt.
- **Files changed:** `benchmarks/summarize.sh`

### `run.sh --gate-only` Not Stopping Agents After Results Collection

After collecting gate results, `run.sh` exits without stopping OAT agents. The daemon's health check restarts the supervisor (which lost its gate-only context), and the supervisor follows its default prompt to spawn workers for open issues. This created runaway agents in both the Opus 4.6 and Kimi K2.5 gate-only benchmark runs.

- **Impact:** Unattended agents continued operating on the benchmark repo after the gate-only run was conceptually complete, burning tokens and making unsupervised changes.
- **Fix:** Added `oat repo hibernate --all --yes` after results collection in gate-only mode. Also added a `[benchmark]` coordination message to the supervisor during gate-only runs to prevent proactive spawning even before the script exits.
- **Files changed:** `benchmarks/run.sh`

### `new_data_dir()` Subshell Foot-Gun in Blackbox Test Scaffold

The `new_data_dir()` helper in `benchmarks/robotic-barista/scripts/blackbox-tests/helpers.sh` exports `BARISTA_DATA_DIR` as a side-effect. When models call it via command substitution (`data_dir=$(new_data_dir)`), the `export` runs in a subshell and the parent shell's `BARISTA_DATA_DIR` never changes. This silently breaks data isolation between test groups, causing "recipe already exists" FATAL aborts in blackbox scoring.

3 of 4 model-generated test modules in the DeepSeek Nitro / Haiku 4.5 benchmark fell into this trap. The documentation in `blackbox-testing.md` showed a similar problematic pattern (`export APP_DATA_DIR=$(new_data_dir)`), compounding the issue.

- **Impact:** Blackbox test scores were artificially lowered by data pollution across test groups. FATAL aborts from duplicate data caused cascading test failures that were not the model's fault.
- **Fix:** Added a prominent warning comment in `helpers.sh` documenting that `new_data_dir` must not be called via `$()`. Updated `blackbox-testing.md` with the correct two-line pattern and an explicit warning. Updated `issues.json` wave:0 issue bodies with a data-isolation callout.
- **Files changed:** `benchmarks/robotic-barista/scripts/blackbox-tests/helpers.sh`, `benchmarks/blackbox-testing.md`, `benchmarks/issues.json`

### `collect.sh` Missing Wave 0 Data

`collect.sh` only collected data for waves 1–4 but the benchmark has wave 0 (4 pre-implementation gate issues). This caused `ISSUES_TOTAL` (24) to not match the sum of wave issue counts (20), wave 0 PRs and timing to be invisible in the report, and `summarize.sh` to get null when reading `waves["0"].timing.started_at`.

- **Impact:** Wave 0 metrics were missing from `collect.json`, causing downstream tools (`summarize.sh`) to silently produce incomplete reports.
- **Fix:** Added `WAVE0=$(wave_stats "wave:0")` and included wave 0 in PR merge counts, timing, ensure/validation steps, and the final JSON assembly.
- **Files changed:** `benchmarks/collect.sh`

### Bash 3.2 Negative Array Index Crash in `run.sh`

`run.sh` used `${WAVE_RESULTS[-1]}` (negative array indexing) to check if the last wave timed out. This syntax is not supported on macOS Bash 3.2, causing a "bad array subscript" crash after wave 4 completes.

The crash killed the script before `wave-timing.json` could be written, forcing `collect.sh` to fall back to inaccurate PR-derived timing (all waves showed identical 81m durations instead of actual 8m/30m/26m/12m).

- **Impact:** Observed in the DeepSeek Nitro / Haiku 4.5 benchmark. The script crashed at line 1020 after all waves completed successfully. No `wave-timing.json` was produced.
- **Fix:** Replaced `${WAVE_RESULTS[-1]}` with portable `${WAVE_RESULTS[$((WAVE_RESULTS_LEN - 1))]}`. Additionally moved the `wave-timing.json` write to before the merge grace period check as defense in depth.
- **Files changed:** `benchmarks/run.sh`

---

## Backend Issues (Historical)

> **Note:** These issues were discovered during the transition from tmux to the direct PTY backend. All have been resolved.

Issues discovered during the development of the PTY backend (`pkg/backend`). The backend runs agents as PTY child processes managed by the daemon.

### CLI-Spawned Agents Dying on Exit

During `oat repo init`, the CLI called `c.backend.CreateSession()` and `c.backend.StartAgent()` directly. Since sessions are in-memory maps and agents are child processes of the CLI, when the CLI exits, all agents die.

The daemon's health check (running ~2 minutes later) detected the missing session and attempted restoration, but the agents were already gone.

- **Impact:** After `oat repo init`, all agents would die immediately. The benchmark's "Workspace agent did not become ready within 120s" error was a direct consequence.
- **Fix:** Added a `start_repo_agents` daemon socket command. The CLI now registers agents in state via `add_agent` (no PID, no session), then sends `start_repo_agents` to the daemon. The daemon creates the backend session and starts all agents as its own child processes, so they survive CLI exit.
- **Files changed:** `internal/daemon/daemon.go`, `internal/cli/cli.go`

### Workspace Restoration Using Wrong Agent Name

`restoreRepoAgents` hard-coded the workspace worktree path as `AgentWorktree(repoName, "workspace")`, but `initRepo` creates the workspace worktree at `AgentWorktree(repoName, "default")`. After the health check cleared stale agents and tried to restore, the workspace agent was never recreated because the expected path didn't exist.

- **Impact:** Workspace agent was permanently missing after restoration. `oat attach default` reported "agent 'default' not found".
- **Fix:** Changed `restoreRepoAgents` to check both `"default"` and `"workspace"` worktree paths (matching the pattern already used by the workspace health-check loop). When neither exists, it creates at `"default"` to match the CLI convention.
- **Files changed:** `internal/daemon/daemon.go`

### Benchmark Scripts Using Raw Tmux Commands (Historical)

`benchmarks/run.sh` previously used `tmux send-keys`, `tmux has-session`, `tmux list-windows`, and `tmux capture-pane` directly for agent interaction and readiness checks.

- **Fix:** Replaced all raw tmux commands with backend-agnostic `oat` CLI commands: `oat agent tell` for message delivery (with retry logic), `oat status` for readiness checks, and `oat attach` for monitoring. Updated agent instructions to reference log files.
- **Files changed:** `benchmarks/run.sh`, `benchmarks/cleanup.sh`, `benchmarks/README.md`

### Direct Backend Test Flush Ordering

`TestDirectBackend_SendMessage` in `pkg/backend/direct_backend_test.go` failed with an empty log file. The `cleanLogWriter` buffers the last line of output until the process terminates or a new unrelated line arrives.

- **Fix:** Moved `b.StopAgent()` before reading the log file, ensuring the writer flushes its pending buffer.
- **Files changed:** `pkg/backend/direct_backend_test.go`

### Benchmark Readiness Check Using Wrong Grep Pattern

`run.sh` checked workspace readiness with `oat status | grep -q "default"`, but `oat status` outputs a high-level summary (repo names and agent counts) and never lists individual agent names. The grep always failed, causing a 120-second timeout even though agents were alive and interactive.

- **Impact:** Every benchmark run timed out at the readiness check, despite agents being fully operational (verified via `oat attach`).
- **Fix:** Changed the grep pattern from `"default"` to `"${REPO_NAME}"`, which matches the repo name that `oat status` does output.
- **Files changed:** `benchmarks/run.sh`

### AGENTS.md Write Path Regression (re-introduced migration bug #3)

The backend abstraction code wrote agent prompts to `.deepagents/AGENTS.md`, but the Python runtime auto-loads from `.oat/AGENTS.md`. This is the same path mismatch fixed during the oat-agent migration (see Migration Issue #3 below), re-introduced when the backend abstraction was written using the old path.

- **Impact:** Workers never received their OAT-specific instructions (including "use `oat pr create`"), causing them to use `gh pr create` directly, omitting the `oat` label and `Closes #N` links. This led to issues not being auto-closed, all benchmark waves timing out, and only 2/20 issues closed. The merge-queue also found its prompt only because it manually browsed for it, not via auto-loading.
- **Fix:** Changed `.deepagents` to `.oat` in `direct_backend.go`. Updated the comment in `backend.go` and all test assertions.
- **Files changed:** `pkg/backend/direct_backend.go`, `pkg/backend/backend.go`, `pkg/backend/direct_backend_test.go`, `test/backend_integration_test.go`

### BinaryPath Not Quoted for Spaces (re-introduced migration bug #5)

`direct_backend.go` concatenated `cfg.BinaryPath` into the shell command without quoting.

- **Impact:** Agent startup would fail if the binary path contained spaces (e.g., `Root Projects/`).
- **Fix:** Applied `shellQuote()` to `cfg.BinaryPath` in `direct_backend.go`.
- **Files changed:** `pkg/backend/direct_backend.go`

### findAgentChild Retry Count and Process Name Matching (re-introduced migration bug #6)

The backend refactoring reduced `FindAgentChildWithRetry` from 30 retries (~15s) back to 10 (~5s), and the process name matching only checked `oat-agent` instead of also matching `oatagent` and `deepagents`.

- **Impact:** Agent process detection could fail on slower systems or when the process name appeared differently.
- **Fix:** Increased retry count back to 30. Expanded process name matching to include `oatagent` and `deepagents`.
- **Files changed:** `pkg/agent/runner.go`

### Merge-Queue Caching Empty PR List Results

The merge-queue prompt instructed agents to "not re-check or re-run the full workflow" when no PRs were found. Agents misapplied this as "never re-query the API on subsequent nudges" -- after one empty `gh pr list` result, they responded "No change" to all future daemon nudges without re-running the command, even though workers had created PRs in the meantime.

- **Impact:** PRs sat unmerged for ~10 minutes until the supervisor manually told the merge-queue about specific PR numbers. The merge-queue confirmed this behavior when asked directly.
- **Fix:** Changed the merge-queue prompt from "Do not re-check" to "Always re-check on every status nudge." Clarified that "No change" means "I checked and confirmed nothing changed," not "I assumed nothing changed."
- **Files changed:** `internal/templates/agent-templates/merge-queue.md`

### Summarizer Curl Silently Swallowing Errors

`summarize.sh` used `curl -s` (silent mode) without `-S` (show-error). When curl encountered a connection error, it silently returned an empty string with no error message. The script also didn't check curl's exit code.

- **Impact:** Benchmark summary generation silently failed with "Claude API returned empty response" and no diagnostic information.
- **Fix:** Changed to `curl -sS`, added exit code checking, and wrapped the call in a retry loop (3 attempts with exponential backoff).
- **Files changed:** `benchmarks/summarize.sh`

### Direct Backend Hardcoding bash Instead of User's Shell

`direct_backend.go` hardcoded `bash -l -c` to launch agents, which skips `.zshrc` on macOS where zsh is the default shell. This prevented environment variables set in `.zshrc` from being inherited.

- **Impact:** Agents didn't inherit tokens and API keys from the user's shell profile, causing authentication failures on first attempt.
- **Fix:** Changed to use `$SHELL` (falling back to `bash` if unset).
- **Files changed:** `pkg/backend/direct_backend.go`

### CLI Environment Not Forwarded to Daemon-Spawned Agents

The daemon starts agent processes (which inherit the daemon's environment), but the daemon often lacks tokens like `GH_TOKEN` that were set in the user's shell. The `~/.oat/.env` workaround helps but requires manual setup.

- **Impact:** Agents had to retry with different authentication methods before finding valid credentials.
- **Fix:** Two-layer approach: (1) hardened `loadEnvFiles` to accept `export KEY=value` format in `.env` files, (2) CLI now forwards relevant env vars (`GH_TOKEN`, `ANTHROPIC_API_KEY`, etc.) via the `start_repo_agents` socket request, which the daemon injects into agent startup.
- **Files changed:** `internal/cli/cli.go`, `internal/daemon/daemon.go`

### Incomplete Provider API Key Forwarding

The `collectCLIEnvVars()` function that forwards environment variables from the CLI to the daemon only listed `GH_TOKEN`, `GITHUB_TOKEN`, `GH_TOKEN_CLASSIC`, `GH_TOKEN_ORG`, `ANTHROPIC_API_KEY`, and `OPENAI_API_KEY`. API keys for other LLM providers (Google, OpenRouter, DeepSeek, Mistral, etc.) were not forwarded.

- **Impact:** Observed during the Gemini 3.1 Pro benchmark. Agents hit `429 Too Many Requests` with "Quota exceeded... limit: 25" errors because `GOOGLE_API_KEY` was set in the user's shell but never reached the daemon-spawned agents. The agents fell back to some default/free-tier authentication with a 25 requests/minute limit.
- **Fix:** Added `GOOGLE_API_KEY`, `GOOGLE_CLOUD_PROJECT`, `DEEPSEEK_API_KEY`, `MISTRAL_API_KEY`, `GROQ_API_KEY`, `XAI_API_KEY`, `TOGETHER_API_KEY`, and `OPENROUTER_API_KEY` to the forwarding list.
- **Files changed:** `internal/cli/cli.go`

### `--repo org/repo` Format Causes Wrong Git Fetch Path

`resolveRepo()` returned the `--repo` flag value verbatim. When LLM agents passed `--repo Root-IO-Labs/oat-bench-foo` (full GitHub `org/repo` format), `c.paths.RepoDir()` produced `~/.oat/repos/Root-IO-Labs/oat-bench-foo` instead of `~/.oat/repos/oat-bench-foo`. The `git fetch` pre-step failed with `chdir ... no such file or directory`.

- **Impact:** Non-blocking — the warning says "continuing with local refs" and worker creation still succeeds via the daemon socket. But workers start from potentially stale local refs instead of the latest origin/main. Observed in both GPT 5.3 Codex and Gemini 3.1 Pro benchmark runs.
- **Fix:** Added `org/repo` normalization in `resolveRepo()`: strips trailing slashes, then takes only the part after the last `/` if present.
- **Files changed:** `internal/cli/cli.go`

---

## oat-agent Migration Issues (Historical)

> **Note:** These issues are historical. OAT no longer uses tmux — agents run as direct PTY child processes of the daemon. References to tmux commands, `pkg/tmux/`, and `send-keys` below describe the old architecture and are preserved for context only.

Issues caused by the migration from `deepagents` to `oat-agent`. Categorized by attribution.

### Directly Caused by the oat-agent Switch (8 bugs)

These issues would not have occurred if we had stayed on `deepagents` directly. They were introduced by rewriting launch code, rebranding paths, or changing the process architecture.

#### 1. Agents Starting in the Wrong Directory / Missing Prompts

**Commit:** `4e059ad`

Persistent agents (supervisor, merge-queue, pr-shepherd) started in the repo clone directory instead of their assigned worktree. No `AGENTS.md` existed there, so agents didn't know their roles and sometimes operated on the wrong project.

- **Cause:** `startAgentInTmux` created tmux windows with `-c repoPath` instead of the agent's worktree path. This code was rewritten as part of the migration.
- **Fix:** Prepended a `cd` to the worktree path before launching the agent in the tmux command. Also passed `WorkDir` in the daemon's `restartAgent` so restarts land correctly.
- **Files changed:** `internal/cli/cli.go`, `internal/daemon/daemon.go`

#### 2. `--append-system-prompt-file` Crashing Agents

The flag OAT passed to inject prompts was not recognized by the Python `argparse` in `deepagents_cli`, causing `sys.exit(2)` — agents crashed immediately on startup.

- **Cause:** New code added during the migration introduced a `--append-system-prompt-file` flag that the `deepagents_cli` runtime never defined in its argument parser.
- **Fix:** Removed the flag entirely. The daemon now writes prompt content directly to `{workDir}/.oat/AGENTS.md` for the runtime's memory middleware to discover.
- **Files changed:** `internal/daemon/daemon.go`, `internal/cli/cli.go`

#### 3. `.deepagents` → `.oat` Path Mismatch for Prompt Discovery

Even when prompts were written to disk, the Python runtime's memory middleware was looking for `.deepagents/AGENTS.md` while OAT was writing to `.oat/AGENTS.md`. Prompts were silently undiscovered.

- **Cause:** OAT's Go code was rebranded from `.deepagents` to `.oat` during the migration, but the Python runtime was not updated to match.
- **Fix:** Comprehensive rebranding of all `.deepagents/` filesystem paths to `.oat/` across the entire Python runtime (`config.py`, `project_utils.py`, `model_config.py`, `sessions.py`, `agent.py`, `ui.py`, skills modules, etc.) and related Go code (`hooks.go`, test files).
- **Files changed:** ~20 files across `agent-runtime/libs/cli/deepagents_cli/` and `internal/`

#### 4. `oat-agent` Binary Not Found from Worktree Directories

**Commits:** `93766c2`, `c70da76`

`oat-agent` couldn't locate its `agent-runtime` directory when launched with CWD set to a git worktree (not the project root).

- **Cause:** `findAgentRuntimeDir()` in the new `oat-agent` binary searched relative to CWD first, but worktrees don't contain an `agent-runtime` directory. Also, symlinks in PATH weren't being resolved. This function was entirely new code written for oat-agent.
- **Initial fix:** Changed search order to prefer the directory next to the binary itself (`exeDir/agent-runtime`) over CWD. Added `filepath.EvalSymlinks()` to resolve symlinks when the binary is invoked via PATH.
- **Initial fix was incomplete:** The fix handled symlinks and search order but not the `go install` case where the binary ends up in `$GOPATH/bin` with no `agent-runtime` nearby. When installed via `go install ./cmd/oat-agent`, the binary at `$GOPATH/bin/oat-agent` still couldn't find `agent-runtime` because none of the search paths reached back to the source repository.
- **Complete fix:** Added `OAT_AGENT_RUNTIME_DIR` env var as a priority override in `findAgentRuntimeDir()`. Updated `scripts/install.sh` to also install `oat-agent` and create a symlink from `$GOPATH/bin/agent-runtime` to the source `agent-runtime/` directory, making the existing `exeDir/agent-runtime` search path work.
- **Files changed:** `cmd/oat-agent/main.go`, `scripts/install.sh`, `Makefile`

#### 5. Paths with Spaces Breaking Agent Startup

**Commits:** `93766c2`, `c70da76`

Agent startup failed when workspace paths contained spaces (e.g., `Root Projects`).

- **Cause:** Tmux command strings were reconstructed for the oat-agent launch flow without proper quoting of the binary path and working directory, causing shell word splitting.
- **Fix:** Added a `quoteForShell()` helper that properly handles single-quote escaping, and wrapped both `binaryPath` and `workDir` in the tmux command construction.
- **Files changed:** `internal/cli/cli.go`, `internal/daemon/daemon.go`

#### 6. Agent Process Detection Failing (Grandchild Processes)

**Commits:** `ae55614`, `907d01f`

After spawning agents, OAT couldn't find the agent process PID, causing "failed to find agent process" errors.

- **Cause:** `oat-agent` is a Go wrapper that launches the Python runtime, adding an extra process layer that didn't exist when running `deepagents` directly. The agent process is now a grandchild of the tmux pane. The original `findAgentChild()` only checked direct children, and the startup timeout (5s) was too short for the extra indirection.
- **Fix:** Extended `findAgentChild()` to check both direct children and grandchildren (2 levels deep). Increased retry count from 10 to 30 (~15s total). Expanded recognized process names to include `oat-agent`, `oatagent`, `oat agent`, and `deepagents` for backwards compatibility.
- **Files changed:** `internal/daemon/daemon.go`

#### 7. `.gitignore` Pattern `oat` Excluding Source Files

**Commit:** `22e0040`

The gitignore pattern `oat` was excluding `cmd/oat/main.go` from the repository because it matched any path containing "oat".

- **Cause:** The new binary was named `oat`, and the `.gitignore` entry used the broad unanchored pattern `oat` which matched anywhere in a file path.
- **Fix:** Changed the pattern from `oat` to `/oat` to only ignore the compiled binary at the repo root.
- **Files changed:** `.gitignore`

#### 8. `oat agent` Subcommand Tree Overwritten by Key Collision

All `oat agent <subcommand>` commands (`complete`, `waiting`, `restart`, `attach`, and legacy message aliases) silently invoked `restartAgentInContext` instead of their intended handlers. Workers running `oat agent complete` would resume their agent session indefinitely instead of signaling completion to the daemon, causing 120-second timeouts.

- **Cause:** Before the migration, the agent session restart command was registered under the key `"deepagents"` (`c.rootCmd.Subcommands["deepagents"]`), which didn't conflict with the `"agent"` subcommand tree. During the migration rebranding, this was renamed to `c.rootCmd.Subcommands["agent"]`, creating a key collision that overwrote the entire agent subcommand tree with a bare command.
- **Impact:** `oat agent complete` (workers couldn't signal completion) and `oat agent waiting` (workers couldn't enter dormant state manually) were the most impactful. Other subcommands had working alternatives: `attach` has a top-level `oat attach` alias, `restart` accidentally did the right thing (same function), and legacy message commands have `oat message` equivalents.
- **Fix:** Set `agentCmd.Run = c.restartAgentInContext` as the fallback on the existing command (for `oat agent` with no subcommand) and removed the duplicate registration that overwrote the tree. Added `TestAgentSubcommandRouting` unit test to prevent regression.
- **Files changed:** `internal/cli/cli.go`, `internal/cli/cli_test.go`

### Caused by Runtime Version Drift (2 bugs)

These were caused by OAT being built against a newer version of the `deepagents` runtime that changed its CLI flags. The switch to `oat-agent` was the occasion, but the root cause is version incompatibility.

#### 1. `--session-id` → `--thread-id` Flag Rename

**Commits:** `ae55614`, `907d01f`

The daemon was passing `--session-id` to the agent CLI, but the runtime expected `--thread-id`.

- **Cause:** The `deepagents_cli` renamed this flag between versions. The migration pulled in a newer version.
- **Fix:** Updated the daemon's command construction from `--session-id` to `--thread-id`. Also changed `--dangerously-skip-permissions` to `--auto-approve` to match the new CLI.
- **Files changed:** `internal/daemon/daemon.go`

#### 2. `--resume <id>` → `-r` Flag Change

**Commits:** `204a931`, `cb5816d`

OAT was passing `--resume <sessionID>` but the runtime expected `-r` (no explicit session ID — it resumes the most recent thread by working directory).

- **Cause:** The runtime changed its resume behavior from accepting an explicit session ID to implicitly selecting the most recent thread for the current directory. This was a runtime API change, not an oat-agent architecture issue.
- **Fix:** Updated `buildCommand()` to use `-r` without a session ID when resuming. The `SessionID` field is kept for OAT's internal tracking only.
- **Files changed:** `pkg/agent/runner.go`

### Pre-existing Bug Fixed During Migration (1 bug)

#### 1. Initial Message Delivery Race Condition

**Commit:** `204a931`

Initial messages sent via `tmux send-keys` after a fixed 2-second delay were racing with agent initialization, causing lost messages.

- **Cause:** The fixed delay was insufficient — the agent might not be ready to receive input yet. This was a general tmux timing problem that existed regardless of which binary was used.
- **Fix:** Switched to using the runtime's `-m` flag to inject the initial message directly on the command line at startup, eliminating the timing dependency entirely.
- **Files changed:** `pkg/agent/runner.go`

### Migration Summary

| Section | # | Issue | Attribution | Root Cause Category |
|---------|---|-------|-------------|-------------------|
| Switch | 1 | Wrong working directory | **oat-agent switch** | Path handling (rewritten launch code) |
| Switch | 2 | Unrecognized CLI flag (`--append-system-prompt-file`) | **oat-agent switch** | New code added non-existent flag |
| Switch | 3 | `.deepagents` vs `.oat` path mismatch | **oat-agent switch** | Incomplete rebranding |
| Switch | 4 | Binary not found from worktrees | **oat-agent switch** | New binary's path resolution |
| Switch | 5 | Spaces in paths | **oat-agent switch** | Shell quoting missed in rewrite |
| Switch | 6 | Grandchild process detection | **oat-agent switch** | Go wrapper added process layer |
| Switch | 7 | Gitignore too broad | **oat-agent switch** | New binary name collision |
| Switch | 8 | `oat agent` subcommand tree key collision | **oat-agent switch** | Rebranding `"deepagents"` → `"agent"` key |
| Drift | 1 | `--session-id` → `--thread-id` | Version drift | Runtime API change |
| Drift | 2 | `--resume` → `-r` | Version drift | Runtime API change |
| Pre-existing | 1 | Message delivery race condition | Pre-existing | tmux timing (fixed opportunistically) |

**Breakdown: 8 caused by the switch, 2 caused by runtime version drift, 1 pre-existing.**

---

## Benchmark Reliability Round 5 Issues (2026-03-30)

### `blocker` Label Missing from `setup.sh`

**Root cause:** `benchmarks/setup.sh` LABELS array did not include `"blocker"`. When workers used `oat issue create --blocker`, `gh issue create --label blocker` failed with "label 'blocker' not found".

**Fix:** Added `"blocker"` to the LABELS array in `setup.sh`. Additionally, hardened `oat issue create` to auto-create all labels on the GitHub repo (`gh label create --force`) before creating the issue, making the command self-sufficient in any repo.

**Files changed:** `benchmarks/setup.sh`, `internal/cli/cli.go`

### Missing Wave Labels on Blocker Issues

**Root cause:** When workers create blocker issues with `oat issue create --blocker`, they rarely pass `--wave <N>` explicitly. The command had no way to infer the wave, so blocker issues were created without wave labels. This made it harder to track which wave spawned the blocker.

**Fix:** Added wave auto-detection to `issueCreate`. When `--wave` is not passed, the command queries the worker's assigned issue's GitHub labels for a `wave:*` label and auto-applies it to the new issue.

**Files changed:** `internal/cli/cli.go`

### Circular CI Dependency from Strict Contract Tests (Wave 0)

**Root cause:** Haiku wave 0 workers write contract tests that assert ALL CLI commands exist (hard `assert` failures). When later waves add new command groups in separate PRs, no PR can pass CI individually because the contract test fails for missing commands. This creates a circular dependency where PRs need each other to pass CI.

**Why Sonnet avoids this:** Sonnet writes more lenient contract tests that use conditional checks or skip markers for unimplemented commands.

**Fix:** Added test resilience guidance to `benchmarks/robotic-barista/CLAUDE.md` and issue #2 body in `benchmarks/issues.json`, instructing test workers to use `pytest.importorskip` / `@pytest.mark.skipif` for features not yet implemented. Also added "Writing Tests" section to the OAT worker prompt (`internal/templates/agent-templates/worker.md`) to help all users, not just benchmarks.

**Files changed:** `benchmarks/robotic-barista/CLAUDE.md`, `benchmarks/issues.json`, `internal/templates/agent-templates/worker.md`

### Workers Not Running Linters Before Pushing

**Root cause:** Despite a "Tip" in the worker prompt's CI failure section, Haiku workers consistently did not run `ruff check` / `ruff format` before pushing, leading to trivial CI lint failures across multiple PRs.

**Fix:** Added a standalone "Before Pushing" section to the worker prompt with explicit linting instructions. Added `ruff check --fix && ruff format .` to the benchmark's `CLAUDE.md` non-negotiables and ruff checks to the Definition of Done in implementation issue bodies.

**Files changed:** `internal/templates/agent-templates/worker.md`, `benchmarks/robotic-barista/CLAUDE.md`, `benchmarks/issues.json`

## Worker Lifecycle Issues (2026-03-31)

Discovered during a Haiku 4.5 benchmark run where a verification agent crash triggered a chain of failures: zombie agent, orphaned PR, and merge-queue stall.

### Verification Agent PTY Crash

**Root cause:** Verification agents were started by the CLI's ephemeral `DirectBackend` (inside `requestVerification`), not the daemon's persistent backend. When the `oat worker request-review` CLI command completed and the process exited, the PTY master fd closed, sending SIGHUP to the verification agent — killing it within seconds of startup with zero output.

**Fix:** Added `start_verification_agent` socket command to the daemon. The CLI now delegates agent process creation to the daemon, which owns the PTY lifecycle. Mirrors how workers are started via `handleStartWorker`.

**Files changed:** `internal/daemon/daemon.go`, `internal/cli/cli.go`

### Zombie Agent on Force-Remove

**Root cause:** `oat worker rm --force` tried to kill the agent via the CLI's ephemeral backend (`c.backend.StopAgent`), which has no record of agents started by the daemon. The daemon's `handleRemoveAgent` only deleted the state entry without calling `d.backend.StopAgent()` to kill the process. Result: the agent process survived removal and continued executing as a ghost.

**Fix:** Enhanced `handleRemoveAgent` in the daemon to call `d.backend.StopAgent()` before removing state. Removed the useless CLI-side `StopAgent` call from `removeWorker`.

**Files changed:** `internal/daemon/daemon.go`, `internal/cli/cli.go`

### Orphaned PR from Branch Cleanup

**Root cause:** `cleanupMergedBranches` used `git branch --merged origin/main` to identify branches for deletion, then deleted both local and remote branches. After a worker was removed and its worktree deleted, the local branch could be falsely flagged as "merged" if its tip was an ancestor of the updated main. Deleting the remote branch caused GitHub to auto-close the associated open PR.

**Fix:** Changed `cleanupMergedBranches` to delete local branches only, then selectively delete remote branches only if no open PR uses them. Uses a single batched `gh pr list` API call per repo per cleanup cycle.

**Files changed:** `internal/daemon/daemon.go`

### Missing Workspace Notification on Worker Removal

**Root cause:** When the supervisor removed a stuck worker (via `oat worker rm --force`), no notification was sent to workspace to spawn a replacement. The stuck-worker alert also lacked guidance about checking for open PRs before removing.

**Fix:** (1) Enhanced `alertSupervisorAboutWorker` with actionable guidance: prefer `oat agent complete --worker` over `oat worker rm` when a PR exists, and message workspace for replacement if removing. (2) Added auto-notification in `handleRemoveAgent` — the daemon now messages workspace when a worker with an unfinished task is removed.

**Files changed:** `internal/daemon/stuck_worker.go`, `internal/daemon/daemon.go`

## PR Label and Completion Guardrails (2026-03-31)

Discovered during a Haiku 4.5 benchmark convergence run where fix-wave PRs never appeared as merged and a worker prematurely completed with an orphaned conflicting PR.

### `oat pr create` Missing Wave Label Propagation

**Root cause:** `oat pr create` only added the `oat` label to new PRs. It did not copy wave labels (e.g., `wave:fix-0`) from the worker's assigned issue. The benchmark convergence loop in `run.sh` queries `gh pr list --label wave:fix-N` to detect merged fix PRs, so the query always returned 0 and the merge wait always timed out — even when the PRs had actually merged.

**Fix:** Added wave label detection to `prCreate` using the existing `detectWaveFromIssue()` helper (already used by `oat issue create`). If the worker's assigned issue has a `wave:*` label, it is automatically applied to the PR.

**Files changed:** `internal/cli/cli.go`

### `oat agent complete` Premature Completion with Conflicting PR

**Root cause:** A worker (lively-panther) created PR #37 with merge conflicts. `oat agent waiting` correctly rejected dormancy, but after failing to resolve conflicts and hitting context limits, the worker called `oat agent complete`. Two problems: (1) The CLI's issue-close path used `PRNumber == 0` as a proxy for "no PR exists," but `PRNumber` is only set when `oat agent waiting` succeeds — which was rejected. The CLI closed the issue with "no PR needed" despite an open PR. (2) The daemon's `handleCompleteAgent` had no guardrail against completing with a conflicting PR.

**Fix:** (1) CLI now checks `gh pr list --head work/<agent> --state open` before closing an issue when `PRNumber == 0`, skipping the close if an open PR exists. (2) Daemon rejects `oat agent complete` when the worker has an open PR with `mergeable == "CONFLICTING"`, returning an actionable error message directing the worker to create a blocker issue. (3) Verification approval message made commit-specific so workers know to re-verify after rebasing. (4) Worker prompt updated with merge-conflict escalation guidance (create blocker issue via `oat issue create --blocker`).

**Files changed:** `internal/cli/cli.go`, `internal/daemon/daemon.go`, `internal/templates/agent-templates/worker.md`

## Idle Mode and Stuck-Worker Fixes (2026-03-31)

Discovered during a Sonnet 4.6 sanity-check benchmark run where the last PR (#42) was never merged because the merge-queue stopped receiving nudges.

### Stuck-Worker Alert Causes Premature Worker Completion

**Root cause:** The "Missing Workspace Notification on Worker Removal" fix (above) changed `alertSupervisorAboutWorker()` to say "prefer `oat agent complete --worker` over `oat worker rm` when a PR exists." This correctly prevented PR orphaning from `oat worker rm`, but caused a new problem: the supervisor completed a dormant worker (`storm-bear`) whose PR (#42) had CI still in-progress. With the worker completed, its worktree was removed, leaving no agent available to fix CI failures or merge conflicts.

**Chain of events:** (1) `storm-bear` hit nudge 10, triggering `alertSupervisorAboutWorker`. (2) Supervisor saw PR #42 was OPEN, followed the daemon's advice to run `oat agent complete --worker storm-bear`. (3) Worker completed, triggering idle mode. (4) Merge-queue received one final nudge but CI was still running. (5) No further nudges arrived — PR #42 was never merged.

**Fix:** `alertSupervisorAboutWorker()` now checks the worker's state. If the worker is dormant with an open PR (`WaitingForPR && PRNumber > 0`), the message tells the supervisor "no action needed — worker is dormant, PR is with merge-queue. Do NOT complete or remove." If the worker has no PR, the existing guidance applies. The `handleRemoveAgent` auto-notification to workspace (part 2 of the earlier fix) is unchanged.

**Files changed:** `internal/daemon/stuck_worker.go`

### Idle Mode Strands Merge-Queue When CI Is In-Progress

**Root cause:** Two issues combined: (1) The `finalNudgeMergeQueue` message said "check for any open PRs to process" — the merge-queue interpreted this as a single-shot check and stopped. (2) No follow-up nudge was scheduled after the final nudge, so when CI was in-progress at that moment, the merge-queue was never prompted to recheck.

**Fix:** (1) Updated `finalNudgeMergeQueue` to explicitly instruct the merge-queue to poll CI every 30 seconds for up to 5 minutes if any PR has CI still running. (2) Added `scheduleDelayedMergeQueueNudge()` — after entering idle mode, the daemon schedules one follow-up nudge to the merge-queue 3 minutes later as a programmatic safety net.

**Files changed:** `internal/daemon/daemon.go`

## Acceptance Test Fixes (2026-04-01)

### Order ID Extraction Regex Truncates Prefixed IDs

**Root cause:** `benchmarks/acceptance-test.sh` `extract_order_id()` used regex `[a-f0-9-]{8,}` which only matches hex characters and hyphens. When a model generates `ord-`prefixed order IDs (e.g. `ord-98cb4194`), the regex skips the alphabetic characters `o` and `r`, capturing `d-98cb4194` instead. The truncated ID doesn't match any stored order, causing "Order not found" failures.

**Impact:** 5 test failures and a 24-point loss on the acceptance test (76/100 instead of ~100/100). All Sonnet 4.6 benchmark runs were affected. Additionally, 3 error-path tests were passing for the wrong reason — they got "not found" (from the truncated ID) instead of the correct rejection message.

**Why this surfaced now:** Older models generated pure hex UUID-style IDs that the regex captured correctly. Sonnet 4.6 generates `ord-` prefixed IDs.

**Why the blackbox (gate) test was unaffected:** The gate-generated test uses its own `extract_id` function with a different regex pattern that correctly captures `ord-` prefixed IDs.

**Fix:** Expanded `extract_order_id` to first try alphanumeric-prefixed patterns (`[a-zA-Z]+-[a-f0-9]{6,}`), falling back to the existing hex-only pattern for pure UUID IDs.

**Files changed:** `benchmarks/acceptance-test.sh`

## Auto-Commit and collect.sh Fixes (2026-04-01)

### `requestVerification` Auto-Commit

**Root cause:** Workers consistently call `oat worker request-review` without committing first, despite prompt reminders and improved error messages. The command returned an error ("uncommitted changes detected"), which workers would retry repeatedly — up to 20+ times per benchmark run — burning tokens without progress.

**Fix:** `requestVerification` now auto-commits and pushes uncommitted changes instead of erroring. Includes a safety check that unstages files matching sensitive patterns (`.env`, `.pem`, `.key`, `.secret`, `credentials`, `.p12`, `.pfx`, `.jks`) before committing, with actionable guidance printed for any skipped files. Also detects and pushes committed-but-not-pushed changes.

**Files changed:** `internal/cli/cli.go`, `internal/templates/agent-templates/worker.md`

### `ensure_json` Bash Escaping Bug

**Root cause:** The `ensure_json` function in `benchmarks/collect.sh` used `${2:-\{\}}` as the default fallback. Bash parameter expansion treats the backslashes literally, producing `\{\}` instead of `{}`. Any variable relying on the fallback contained invalid JSON, which cascaded into `collect.json` assembly failures ("some --argjson variables contained invalid JSON").

**Fix:** Replaced the parameter expansion with a simple conditional that assigns `'{}'` when no fallback argument is provided.

**Files changed:** `benchmarks/collect.sh`

### collect.sh/run.sh Owner Detection

**Root cause:** Both `collect.sh` and `run.sh` default `GITHUB_OWNER` to `gh api /user --jq '.login'`, which returns the user's personal account (e.g. `albertgiang-root`) instead of the organization (e.g. `Root-IO-Labs`). This causes "Repository not found" errors when rerunning `collect.sh` standalone against org repos.

**Fix:** Added `_detect_owner()` function that reads the GitHub URL from OAT's `state.json` and extracts the owner. Falls back to the existing `gh api /user` default if state.json is unavailable.

**Files changed:** `benchmarks/collect.sh`, `benchmarks/run.sh`

## Benchmark Reliability Fixes (2026-04-02)

Analysis of the `20260401-185006-sonnet46-sanitycheck` run (100/100 acceptance score, but operational friction) identified several systemic issues.

### `.oat/AGENTS.md` merge conflicts

**Root cause:** OAT writes `.oat/AGENTS.md` (the agent's prompt) into each worker's worktree at startup. Workers doing `git add -A` would stage this file, and subsequent merges would conflict when multiple workers had different prompts. Hit 6/23 workers in the benchmark run.

**Fix:** OAT now writes a `.oat/.gitignore` alongside `AGENTS.md` that ignores `AGENTS.md` and `settings.json`. The nested gitignore only affects OAT runtime files, not user-managed files in `.oat/` like `hooks.json` or agent definitions.

**Files changed:** `pkg/backend/direct_backend.go`, `benchmarks/robotic-barista/.gitignore`

### Verdict delivery race condition (SHA mismatch / worker-not-found)

**Root cause:** When a worker polled (instead of going dormant) after `oat worker request-review`, it could run `oat worker verify` (self-verify) and then `oat pr create` while the verification agent was still running. The `oat pr create` verification gate had a fallback that allowed PR creation based on self-verify when the independent verifier was "pending." The merge queue could then merge the PR before the verifier finished, causing SHA mismatch and worker-not-found errors when the verdict was finally delivered.

**Fix (multi-layered):**
1. Worker prompt updated to explicitly instruct `oat agent waiting` after `oat worker request-review`
2. `oat pr create` now enforces a 5-minute timeout before allowing the self-verify fallback when a verification agent is pending
3. Daemon verdict handler now tolerates worker-not-found and SHA-mismatch when the worker has already completed or is ready for cleanup

**Files changed:** `internal/templates/agent-templates/worker.md`, `internal/cli/cli.go`, `internal/daemon/daemon.go`

### Self-verify rubric false negatives

**Root cause:** The `checkAlignmentWithLLM` function (misleadingly named — it used no LLM) applied brittle keyword heuristics with aggressive deductions (up to -25 for keyword mismatches). Open-ended tasks like "Performance and Edge Cases" would easily trigger deductions, causing the self-verify to score 67/100 on work the independent verifier approved.

**Fix:** Renamed to `checkTaskAlignmentHeuristic`, raised base score from 70 to 80, reduced deduction amounts (max -15 per heuristic), and added a floor of 40 to prevent extreme penalties from noisy keyword matching.

**Files changed:** `internal/cli/verify_helpers.go`, `internal/cli/verify.go`

### CI failure message causing premature dormancy

**Root cause:** The daemon's CI failure wake message ended with "After fixing, run `oat agent waiting` again." Models tend to overweight the final instruction, so `dawn-dragon` immediately called `oat agent waiting` without fixing anything.

**Fix:** Restructured the message to put the negative constraint last: "Do NOT run `oat agent waiting` until your fix is pushed." Also made the high-nudge stuck worker message conditional on whether the worker already has an open PR.

**Files changed:** `internal/daemon/pr_monitor.go`, `internal/daemon/stuck_worker.go`

### Stale verification worktree branch surviving cleanup

**Root cause:** When the daemon cleaned up a completed verification agent, `git worktree remove` deleted the directory but not the git branch (`verify/verify-<worker>`). A subsequent `oat worker request-review` for the same worker failed with `exit status 255` because `git worktree add -b verify/verify-<worker>` found the branch already existed.

**Fix:** `requestVerification` now auto-repairs stale worktrees before creating new ones: removes the directory if it exists, and deletes the stale branch. Modeled after the existing workspace creation auto-repair pattern.

**Files changed:** `internal/cli/cli.go`

### Stale messages processed after `oat agent complete`

**Root cause:** `mega-hawk` processed 4 stale approval messages after calling `oat agent complete`. The messages had been delivered before the `ReadyForCleanup` flag was set.

**Fix:** `handleCompleteAgent` now purges all pending (undelivered) messages for the agent immediately after marking it as ready for cleanup.

**Files changed:** `internal/daemon/daemon.go`, `internal/messages/messages.go`

### Crashed verifier leaves worker in phantom "pending" state

**Root cause:** If a verification agent crashes and is cleaned up by the daemon, the linked worker's `VerificationStatus` remains "pending" with no agent to deliver a verdict. The `oat pr create` pending timeout would eventually allow fallback, but the phantom state is confusing.

**Fix:** `cleanupDeadAgents` now resets the linked worker's `VerificationStatus` from "pending" to "" when cleaning up a verification agent.

**Files changed:** `internal/daemon/daemon.go`

### Gate smoke test fails when CLI not yet built

**Root cause:** The execution smoke test in `run.sh` calls `run-blackbox.sh` at gate time to verify the generated test script runs without crashing (catching unbound variables, syntax errors, bash version issues). However, `run-blackbox.sh` bails at CLI detection ("No working CLI entry point") before ever executing the test script, because the app hasn't been built yet — only the test script exists from issue #1.

Previously this worked accidentally: a stale system-level `barista` shim at `/opt/homebrew/bin/barista` (leaked from a prior benchmark worker's `pip install -e .` on system Python) was found by `command -v barista`, allowing the test to run. All tests failed (expected), but the script executed and produced PASS/FAIL results, which is all the smoke test checks for. The stale shim was removed when an orphaned Kimi K2.5 benchmark repo unexpectedly reactivated and its workers re-installed `robotic-barista` globally without a console script entry.

**Fix:** Added a `--smoke` flag to `run-blackbox.sh`. When set, CLI detection failure falls back to a stub `barista` function (returns exit code 1) instead of bailing. The gate smoke test in `run.sh` passes `--smoke`; the convergence loop and final blackbox run do not, so they still require a real CLI.

**Files changed:** `benchmarks/run-blackbox.sh`, `benchmarks/run.sh`

### Verification-Dormancy Interaction: Workers Auto-Completed While Waiting for Verdict

Workers following the correct flow (`oat worker request-review` → `oat agent waiting`) were auto-completed by the daemon because `handleAgentWaiting` treated any worker without a PR as a "dead-end." This forced recovery workers to create PRs for every issue.

- **Impact:** In one Sonnet benchmark, 20+ recovery workers were spawned. Every primary worker was auto-completed before creating a PR.
- **Root cause:** `handleAgentWaiting` (daemon.go) checks `PRNumber == 0` and auto-completes, without considering that workers waiting for verification don't have a PR yet. The `oat pr create` verification gate blocks PR creation while verification is pending, and tells workers to run `oat agent waiting` — but the daemon then kills them for not having a PR.
- **Fix:** Added `VerificationStatus == "pending"` check before auto-completion to allow dormancy. Added `wakeWorker()` call in `handleVerificationVerdict` to wake dormant workers when verdicts arrive. Added `checkVerificationTimeouts()` function with 5-minute timeout and safety net for orphaned dormant workers. Fixed `cleanupDeadAgents` to wake dormant workers when their verifier crashes. Fixed CLI to surface `auto_completed` vs `dormant_verification` status. Fixed worker prompt contradiction about no-PR dormancy.
- **Edge cases handled:** (1) Worker self-verifies while verifier delivers verdict simultaneously — `oat pr create` gate handles both inputs. (2) Verifier crashes before delivering verdict — cleanup wakes worker immediately, timeout scan is safety net. (3) Timeout and verdict arrive near-simultaneously — message tells worker to follow verdict if received.
- **Files changed:** `internal/daemon/daemon.go`, `internal/daemon/pr_monitor.go`, `internal/cli/cli.go`, `internal/templates/agent-templates/worker.md`, `internal/daemon/handlers_test.go`

### Blocker Issue Not Closed When Worker Auto-Completed (Wave Timeout)

A worker (wise-bison) created an unnecessary blocker issue (#34) for missing "order validate" functionality that was already planned in wave:2. The spawned worker (clever-manta) created PR #35, but a command timeout during `oat pr create` prevented `PRNumber` from being registered in daemon state. GitHub auto-closed PR #35 (empty diff after rebase). The daemon auto-completed clever-manta through the "no PR found" path, which had no issue-closing logic. Issue #34 stayed open, causing wave 1 to time out.

- **Impact:** Wave 1 timeout due to stale blocker issue
- **Fix:** (1) Worker prompt now guides workers to diagnose out-of-scope CI failures and fix root causes (e.g., coarse skip guards) before creating blockers. (2) Daemon `closeAssociatedIssue` helper closes issues in `handleAgentWaiting` when the worker has no PR or the PR was closed (not merged). Does NOT close issues from `autoCompleteWorker` to avoid confusing the supervisor for genuinely incomplete work. (3) `oat agent complete` CLI extended to close issues when `PRNumber > 0` and PR was CLOSED.
- **Files changed:** `internal/templates/agent-templates/worker.md`, `internal/daemon/stuck_worker.go`, `internal/daemon/daemon.go`, `internal/cli/cli.go`

### Worker Self-Verified While Verification Agent Still Running

After running `oat worker request-review` and `oat agent waiting`, clever-manta hallucinated a daemon wake and immediately ran `oat worker verify` (self-verification), which had no guard against running while a verification agent was active. The self-verify passed, but `oat pr create` correctly blocked ("verification agent still reviewing, 18s ago"). The worker then fell into a `sleep && oat pr create` polling loop, leading to a command timeout.

- **Impact:** Worker burned tokens polling; command timeout prevented PRNumber from being set in daemon state, contributing to the issue-not-closed cascade
- **Fix:** (1) `oat worker verify` now refuses to run while a verification agent has been active < 5 minutes (matching `oat pr create`'s gate). Falls through as fallback when verifier is > 5 min old or gone from state. (2) `oat worker request-review` and `oat agent waiting` output now include explicit STOP instructions to prevent hallucinated wakes and polling.
- **Files changed:** `internal/cli/cli.go`

### Worker Polled Instead of Going Dormant After Verification Guard Blocked Self-Verify

Despite prior STOP instructions, `solar-gecko` ran `sleep 150 && oat worker verify` instead of `oat agent waiting` after `oat worker request-review`. The verification guard correctly blocked self-verification (< 5 min), but the worker wasted time polling with `sleep` instead of entering dormancy. Similarly, `oat worker request-review` required a manual `oat agent waiting` call afterward, which workers sometimes skipped or forgot.

- **Impact:** Workers burned time in sleep loops (2.5+ min each) and consumed unnecessary tokens before eventually self-verifying or being nudged.
- **Fix:** (1) `oat worker request-review` now auto-calls `oat agent waiting` via the daemon socket after spawning the verifier, so the worker goes dormant immediately. Includes fallback message if daemon is unreachable. (2) `oat worker verify` guard (< 5 min) now auto-calls `oat agent waiting` instead of returning a plain error, putting the worker dormant until the daemon delivers the verdict.
- **Files changed:** `internal/cli/cli.go`

### Workspace Spawned Replacement Worker for Removed Worker Whose PR Was Still Active

When `frost-panda` was removed (by the supervisor via `nexus-shark`), the daemon sent a generic "Consider spawning a replacement worker" notification to the workspace. The workspace blindly spawned `crimson-kraken` for the same issue, even though `frost-panda`'s PR #36 was still open and being processed by the merge-queue.

- **Impact:** Duplicate work and wasted agent slot. `crimson-kraken` created a redundant PR for an issue that was already being addressed.
- **Fix:** (1) Daemon's "worker removed" notification now includes PR number, issue number, and a `gh pr view` command so the workspace can check PR state before spawning a replacement. (2) Added "Worker Removal Notifications" section to workspace prompt with explicit guidance to check PR state first.
- **Files changed:** `internal/daemon/daemon.go`, `internal/prompts/workspace.md`

### Supervisor Spawned Full Worker for One-Line Admin Command

The supervisor spawned `nexus-shark` as a full worker just to run `oat agent complete --worker frost-panda`. This is a single CLI command that the supervisor can execute directly in its own session. The worker received 11 nudges and wasted an agent slot for a trivial task.

- **Impact:** One agent slot consumed for ~20 minutes doing a task that takes 1 second. Unnecessary daemon nudges.
- **Fix:** Added explicit guidance to supervisor prompt: "Run these commands directly in your own session. Do NOT spawn a worker to run administrative commands like `oat worker rm` or `oat agent complete --worker`."
- **Files changed:** `internal/prompts/supervisor.md`

### Convergence Loop Stuck on Contradictory Blackbox Test

During a benchmark run that achieved 100/100 on the acceptance test, the convergence loop timed out after 1h 12m making zero progress on 2 persistent failures. The model-generated blackbox test had a **state isolation bug**: test assertions shared mutable state (e.g., test A validates an order, then test B checks the same order is in "placed" state -- but it's now "validated"). Fix-wave workers correctly identified the contradiction but were only told to fix the implementation, not the test.

- **Impact:** 1h+ wasted in convergence with 11 nudges to the fix worker
- **Fix:** (1) Convergence message now tells fix-wave workers they can modify `scripts/blackbox-test.sh` if the test has bugs, and that they can call `oat agent complete` for genuinely impossible tasks. (2) Added `no_progress` early exit: if the exact same failure lines (by hash) appear in 2 consecutive iterations, the loop exits instead of timing out. (3) Added state-mutation advice to `benchmarks/blackbox-testing.md`.
- **Files changed:** `benchmarks/run.sh`, `benchmarks/blackbox-testing.md`

### Worker Polling After Post-Verification Dormancy

After `oat worker request-review` auto-called `oat agent waiting`, the daemon returned `already_dormant` with "waiting for PR #43" -- even though the worker was waiting for a verification verdict, not PR resolution. The worker reasoned the system didn't know about verification and started polling with `sleep && oat message list`.

- **Impact:** Worker burned tokens polling instead of staying dormant for the daemon's wake message
- **Root cause:** The `WaitingForPR` field was used as a general dormancy flag for both PR waiting and verification waiting. The `already_dormant` response had no verification-specific path.
- **Fix:** Split `WaitingForPR` into separate `WaitingForPR` and `WaitingForVerification` fields. The daemon now returns `dormant_verification` status with explicit "STOP" instructions when a worker is already dormant for verification. Added `IsDormant()` and `ClearDormancy()` helpers.
- **Files changed:** `internal/state/state.go`, `internal/daemon/daemon.go`, `internal/daemon/pr_monitor.go`, `internal/daemon/stuck_worker.go`, `internal/format/format.go`, `internal/cli/selector.go`, `internal/tui/app.go`

### `oat agent complete` Does Not Close Associated Issue

When a worker calls `oat agent complete` directly without creating a PR (the "impossible task" scenario), the agent gets cleaned up but the associated GitHub issue stays open. This caused convergence waves to hang waiting for issues that would never close.

- **Impact:** Convergence wave timeouts when fix workers correctly determined their task was impossible
- **Fix:** `handleCompleteAgent` now calls `closeAssociatedIssue` when a self-completing worker (callerName == agentName) has no PR. Supervisor force-completes are excluded (they have their own replacement-worker process).
- **Files changed:** `internal/daemon/daemon.go`

### Agent Log Files Showed UI Display, Not LLM Context

The `.log` files captured the deepagents CLI's Textual TUI display via OAT's `cleanLogWriter`. Tool outputs appeared as 4-line previews with "... N more lines" markers -- not what the LLM actually saw. This made diagnosing agent behavior significantly harder (initially misdiagnosed truncation as the root cause of a polling bug).

- **Impact:** Misleading logs for benchmark analysis; required manual code tracing to determine what agents actually saw
- **Fix:** Replaced PTY-captured logs with full-content conversation logs. The deepagents CLI now writes human-readable logs (full tool calls/results, ANSI-stripped) directly to the `.log` file via the `OAT_TOOL_LOG` env var. OAT's `cleanLogWriter` is skipped when this env var is set. Live `oat ui` streaming is unaffected.
- **Files changed:** `agent-runtime/libs/cli/deepagents_cli/textual_adapter.py`, `agent-runtime/libs/cli/deepagents_cli/app.py`, `pkg/backend/direct_backend.go`, `internal/daemon/daemon.go`

### Verify Agent Leaked When Worker Force-Removed or Completed

When a worker was force-removed (`forceRemoveWorker`) or auto-completed (`autoCompleteWorker`), the associated `verify-<worker>` agent was not cleaned up. The verify agent continued running indefinitely, burning compute. Similarly, when a worker self-completed via `oat agent complete`, no cleanup of its verify agent occurred. Observed during DeepSeek V3.2 Nitro benchmark: `verify-nice-eagle` ran for 30+ minutes stuck in "Thinking..." after worker `nice-eagle` was killed.

- **Impact:** Orphaned verify agents consuming compute indefinitely; confusing `oat status` output showing active verify agents for dead workers
- **Root cause:** `forceRemoveWorker`, `autoCompleteWorker`, and `handleCompleteAgent` did not check for or clean up associated `verify-<worker>` agents
- **Fix:** Added `cleanupVerifyAgent(repoName, workerName)` helper that marks `verify-<worker>` as `ReadyForCleanup`. Called from `forceRemoveWorker`, `autoCompleteWorker`, and `handleCompleteAgent` (for worker-type agents).
- **Files changed:** `internal/daemon/stuck_worker.go`, `internal/daemon/daemon.go`

### No Hard Timeout for Verify Agents

`checkVerificationTimeouts()` sent a "self-verify" message to the worker at the 5-minute soft timeout, then set `verificationTimeoutNotified[key] = true` and never checked again. If the worker did not respond (e.g., model stopped producing output, or the worker was stuck), the verify agent ran forever.

- **Impact:** Verify agents running indefinitely when workers fail to self-verify after soft timeout
- **Root cause:** The notification tracking was a boolean (`map[string]bool`), preventing any follow-up action after the first notification
- **Fix:** Changed tracking to `map[string]time.Time` to record when the soft notification was sent. Added a hard timeout at 10 minutes (`2 * verificationTimeout`): the daemon kills the verify agent directly, resets the worker's verification fields, and wakes the worker with an explicit "self-verify" instruction.
- **Files changed:** `internal/daemon/daemon.go`

### Stuck Detection Hard Timeout Never Fires

Both workspace and core agent (merge-queue, supervisor) stuck detection had the same bug: the soft timeout's nudge message updated the agent's output log file via PTY, which the next health check saw as "new activity" and reset the tracking state. This prevented the hard timeout from ever triggering, causing stuck agents to receive infinite soft nudges instead of being restarted. Confirmed in daemon logs: supervisor was soft-nudged every 5-10 minutes for 5+ hours without ever being restarted.

- **Impact:** Truly stuck agents (hung LLM requests) were never restarted, only nudged repeatedly forever
- **Root cause:** The `nudged bool` flag was reset to `false` whenever the output log's modification time changed — but the nudge message itself (delivered via `SendMessage` -> PTY) caused the log to update, immediately resetting the flag
- **Fix:** Replaced `nudged bool` with `nudgedAt time.Time`. A 10-second grace period after sending a nudge ignores log updates (which are the nudge's own PTY output). Genuine agent activity (tool calls, responses) outside the grace period resets tracking normally. The hard timeout condition now checks `!nudgedAt.IsZero()` instead of `nudged == true`.
- **Files changed:** `internal/daemon/stuck_workspace.go`, `internal/daemon/stuck_core_agents.go`

### Workspace Stuck Detection False Positives on Idle Repos

Workspace stuck detection monitored all repos, including those where the user had stepped away. Since the workspace is user-driven, going quiet just means the human isn't at their desk — not that the agent is stuck thinking. After the soft timeout, a harmless nudge was sent; after the hard timeout, the workspace was restarted, destroying conversation context for no reason.

- **Impact:** Workspace agents restarted unnecessarily when users were away, losing conversation history
- **Root cause:** Stuck detection didn't distinguish "stuck thinking" from "waiting for human input"
- **Fix:** Made workspace stuck detection off by default (per-repo opt-in via `oat config --workspace-stuck-detection=true`). Benchmark runs enable it automatically because they are unattended. Core agent stuck detection remains always-on since merge-queue/supervisor blocking impacts PR throughput for all users.
- **Files changed:** `internal/daemon/stuck_workspace.go`, `internal/state/state.go`, `internal/cli/cli.go`, `internal/daemon/daemon.go`, `benchmarks/setup.sh`

### Daemon Idle-Transition Message Races with Benchmark Wave Control

`notifyWorkspaceIdleTransition()` in `daemon.go` sends an aggressive "spawn workers now" message to the workspace agent whenever a repo transitions to idle mode (0 active workers). This fires on freshly initialized repos that never had workers, and also after every batch of workers completes -- racing with the benchmark's explicit wave control in `run.sh`.

In benchmark runs, the workspace received this message before the gate phase finished and immediately spawned workers for wave:1 issues, bypassing the gate entirely. After gate failure, the daemon kept sending the message every wake cycle, spawning runaway workers that burned tokens indefinitely.

- **Impact:** Premature worker spawning during benchmark gate phase; runaway agents after gate failure
- **Root cause:** `notifyWorkspaceIdleTransition` (added in `feature/bundle-robotic-barista`, not on `dev`) doesn't distinguish "fresh repo, never had workers" from "all workers finished a wave"
- **Fix (temporary):** Call to `d.notifyWorkspaceIdleTransition(repoName, repo)` in `wakeAgents()` commented out with a TODO pending redesign. The function itself is preserved. Idle mode transitions and final supervisor/merge-queue nudges are unaffected.
- **Files changed:** `internal/daemon/daemon.go`

### Assembled Blackbox Test Crashes Due to Inlined Source Commands

The `assemble_gate_test()` function in `run.sh` concatenates helpers, test modules, and the entry point into a single script for LLM judging. The entry point (`blackbox-test.sh`) contains `source` commands that reference external files (`helpers.sh`, `test-*.sh` modules). When the assembled script runs from a temporary directory, these `source` commands fail because the files don't exist there -- their content is already inlined above in the assembled script.

- **Impact:** Gate smoke test crashes with `helpers.sh: No such file or directory` / `No test modules found`
- **Root cause:** Assembly appended the raw entry point without stripping its `source` commands and module discovery loop
- **Fix:** Replaced the raw entry point append with an inline runner block that calls `detect_cli`, iterates `test_*` functions (already defined by inlined content), and runs `print_results`/`save_results`
- **Files changed:** `benchmarks/run.sh`

### `.oat/.gitignore` False Positive in HasUncommittedChanges

OAT writes `.oat/.gitignore` into every agent worktree at runtime. `git status --porcelain` reports it as an untracked file, causing `HasUncommittedChanges` to return `true` even when the agent has no real uncommitted work. This made `oat repo hibernate` report `[has uncommitted changes]` for every non-worker agent (workspace, supervisor, merge-queue, verify-*) and triggered unnecessary patch/untracked archival.

- **Impact:** Cosmetic false positive during hibernate; confusing output suggesting agents modified files when they didn't
- **Root cause:** `HasUncommittedChanges` checked `len(output) > 0` without filtering OAT's own runtime files
- **Fix:** `HasUncommittedChanges` now parses `--porcelain` output line-by-line and skips paths under `.oat/`. The `hibernateRepo` archival commands (`git diff HEAD`, `git ls-files --others`) also use the `:!.oat` pathspec to exclude OAT runtime files from patches and untracked file lists.
- **Files changed:** `internal/worktree/worktree.go`, `internal/cli/cli.go`, `internal/worktree/worktree_test.go`

### Stale Daemon Nudges Queue Up During Active Work

During a worker's active working period (10-25+ minutes), the daemon sends periodic nudge messages via `SendMessage` which writes `message + "\r"` to the PTY fd. Each `\r` is processed by the LLM backend as a separate user turn. While the worker is busy, these messages queue up in the PTY buffer. After the worker goes dormant via `oat agent waiting`, the stale nudges are delivered one by one as new user turns, confusing the LLM into responding to each (e.g., calling `oat pr create --force`), wasting tokens and time (~1-2 minutes per worker).

- **Impact:** Token waste and confusion after dormancy; workers process stale nudges as if they were fresh instructions
- **Root cause:** Identical daemon nudge messages queue up in the PTY with no deduplication; the LLM has no way to know they're stale
- **Fix (multi-pronged):**
  1. **Tier-based nudge de-dupe**: `nudgeWorkerEscalating` now tracks the escalation tier of the last sent message (`LastNudgeTier` / `SuppressedNudgeCount` in `Agent` state). Consecutive nudges at the same tier are suppressed at the daemon level — `NudgeCount` still increments (so escalation thresholds fire on time) but the PTY message is only written when the tier changes. When a new tier fires, suppressed count is reported in a prefix.
  2. **Dormancy watermark**: `oat agent waiting` prints a prominent banner warning the LLM to ignore stale `[daemon] Status check` messages that appear below.
  3. **Defensive `IsDormant()` re-check**: The live-state re-checks in `nudgeWorkerEscalating` now also check `IsDormant()` alongside `ReadyForCleanup`, preventing any future race where a snapshot shows non-dormant but live state has transitioned.
  4. **Dormant+PR state transition**: When a worker creates a PR while dormant for verification, `handleAgentWaiting` now transitions from `WaitingForVerification` to `WaitingForPR` so the PR monitor tracks it.
  5. **Outdated nudge messages updated**: Daemon nudge messages now reference `oat worker request-review` instead of directing workers to skip straight to `oat pr create`.
  6. **Worker prompt**: Added note to `worker.md` warning about stale status check messages after dormancy.
- **Files changed:** `internal/state/state.go`, `internal/daemon/stuck_worker.go`, `internal/daemon/daemon.go`, `internal/cli/cli.go`, `internal/templates/agent-templates/worker.md`

### Duplicate PR on Same Branch

`oat pr create` had no guard against creating a duplicate PR when the worker's existing PR was already open or recently merged. In benchmark testing, crystal-seal created PR #34 on a branch whose PR #31 had just been squash-merged 6 seconds earlier, because the merge-queue merged #31 between the worker's dormancy wake and its `oat pr create` call.

- **Impact:** Duplicate PRs on the same branch; wasted CI resources; confusing PR history
- **Root cause:** `prCreate` called `gh pr create` directly without checking for existing PRs on the branch or whether the agent's known PR was already merged
- **Fix:** Added two pre-flight checks before `gh pr create`: (1) `gh pr list --head <branch> --state open` to detect and reuse an existing open PR, and (2) check the agent's known PRNumber in daemon state — if that PR is already merged, auto-complete instead of creating a duplicate. `--force` bypasses both checks.
- **Files changed:** `internal/cli/cli.go`

### LLM Hallucination of Verification Verdict

frost-wolf fabricated an `[APPROVED]` verdict in its own ASSISTANT turn 6 seconds after going dormant via `oat agent waiting`, before any daemon message was delivered (confirmed by daemon log showing zero messages sent in that window). The model then entered a polling spiral (29 wasted tool calls checking PR status). The existing `oat pr create` verification guardrail correctly blocked the unverified PR attempt.

- **Impact:** Token waste from hallucinated verdict and subsequent polling spiral; no incorrect merge (guardrail held)
- **Root cause:** The model's ASSISTANT turn auto-completed with expected text (`[APPROVED]`) primed by dormancy instructions that mentioned the verdict format. The dormancy watermark said "ignore stale messages" but didn't forcefully stop the model from generating.
- **Fix:** Strengthened dormancy watermark with explicit STOP instructions: "Do NOT generate any text, run any commands, or take any action until you see a new USER message from the daemon." Applied to all dormancy banner locations (verification and PR variants in `agentWaiting`, `prCreate`, and `requestReview`).
- **Files changed:** `internal/cli/cli.go`

### Verifier ModuleNotFoundError from Shared Editable Install

Editable `pip install -e .` into system Python creates a global `.pth` file (at e.g. `/opt/homebrew/lib/python3.11/site-packages/__editable__.*.pth`) that points to whichever worktree last ran the install. In benchmark testing, 75% of verifiers (18 of 24) encountered `ModuleNotFoundError` because a different agent's worktree had overwritten the `.pth` file. The 3 verifiers that organically created isolated virtual environments had zero import issues.

- **Impact:** Verifiers unable to run tests; false rejections due to import failures rather than actual code issues
- **Root cause:** Editable installs into a shared system Python are inherently incompatible with OAT's multi-worktree model — the `.pth` file is a global singleton
- **Fix:** Added "Step 0: Environment setup" to the verification prompt instructing Python project verifiers to create an isolated venv (with `uv` preferred, `pip` as fallback) before running any tests.
- **Files changed:** `internal/templates/agent-templates/verification.md`
- **Follow-up (2026-04-17):** Despite the Step 0 addition, 5 verify workers in the routed o4-mini + Flash + Sonnet 4.6 benchmark still skipped venv setup. Root cause: "Step 0" with "(Python projects only)" qualifier signaled the step was optional. Strengthened by renaming to "Step 1: Environment setup" with mandatory language ("**Always run this step.**") and added a bold warning: "If you see `ModuleNotFoundError`, you skipped this step." Subsequent steps renumbered 2-8.
- **Follow-up (2026-04-16, GPT 5.4 Mini + Nano + Haiku run):** `verify-wise-shark` hit `ModuleNotFoundError` but correctly diagnosed it as a venv activation issue -- the benchmark repo's `check.sh` runs `python -m pytest` which picks up whichever `python` is on `PATH`, not the venv. The verifier activated the venv, re-ran (85 tests passed), and approved. The strict "Infrastructure failure = automatic REJECT" rule in the verification prompt was softened to allow re-running after fixing environment issues before rejecting.

### Benchmark run.sh Crash on Gate Timeout (`local` Outside Function)

Commit `85a9b19` introduced `local modules_exist` at the top level of `benchmarks/run.sh` (inside a `while`/`if` but not inside a function). Bash's `local` keyword is only valid inside functions; with `set -e`, this kills the script when the gate timeout handler fires, preventing partial-recovery or `GATE_PASSED=false` logic from executing.

- **Impact:** Benchmark crashes instead of gracefully handling gate timeout; only triggered when wave:0 doesn't complete within the timeout
- **Root cause:** `local` keyword used outside a function in the gate timeout handler
- **Fix:** Removed the `local` keyword, using a plain variable assignment instead.
- **Files changed:** `benchmarks/run.sh`

### Model Routing Not Working for Wave:0 Gate Path

The gate worker creation in `benchmarks/run.sh` only set `MODEL_FLAG` when `--worker-model` was used (single model mode). In `--routing-mode`, it created worker commands without `--model` and told the workspace "Do not modify the commands," preventing the workspace from adding per-worker model selection. The daemon's `BestEligible` auto-select just picks the highest-scoring model every time, so all workers got the same model.

- **Impact:** In routing mode, gate workers all used the same model instead of distributing across available models by task complexity
- **Root cause:** Gate path was written before routing existed; later wave messages had routing instructions but the gate path was missed
- **Fix:** When `ROUTING_MODE` is true, routing instructions are appended to the gate message telling the workspace to add `--model` based on task complexity. The constraint was narrowed from "do not modify the commands" to "do not change issue numbers, repo names, or task descriptions."
- **Files changed:** `benchmarks/run.sh`

### Supervisor Removing Verification-Dormant Workers

The supervisor prompt's stuck-worker cleanup section did not mention that "waiting for verification with no PR" is a normal expected state. The supervisor treated `gh pr list` returning empty as proof a worker was stuck, when the worker was correctly dormant awaiting a verification verdict (PRs are only created after approval). This orphaned verification agents and caused excessive worker churn (12 workers created for 4 issues).

- **Impact:** Workers waiting for verification were incorrectly removed, orphaning verifiers and tripling worker churn
- **Root cause:** Supervisor prompt didn't distinguish verification-dormant from stuck; `oat worker list` shows "waiting for verification" status but the prompt didn't tell the supervisor to check it
- **Fix:** Added a callout in the supervisor prompt's stuck-worker section instructing it to check `oat worker list` STATUS column before removing, with specific guidance for "waiting for verification" and "waiting for PR" states, plus an exception for when the verifier has been gone >10 minutes.
- **Files changed:** `internal/prompts/supervisor.md`, `internal/prompts/workspace.md`

### Wave:0 Workers Confused by Empty Scaffold CLI

Wave:0 issue bodies said `Wave: 0` and referenced the spec as source of truth, but never explicitly stated the CLI is not implemented yet. Workers found the empty `robotic_barista/cli/__init__.py`, got confused, and some tried to implement the CLI instead of writing tests.

- **Impact:** Workers wasted time debugging/implementing the CLI instead of writing blackbox tests
- **Root cause:** Issue bodies lacked explicit "pre-implementation" context
- **Fix:** Added a "Wave 0 context" banner to each wave:0 issue body in `issues.json` clarifying the CLI is not yet implemented and tests should encode specified behavior, not test running code.
- **Files changed:** `benchmarks/issues.json`

### helpers.sh Argument Count Errors Under `set -u`

Workers called `assert_error` with 1 argument (missing required `pattern`), causing `$2: unbound variable` under `set -u`. Workers also passed unregistered category names to assertions, causing `CAT_TOTAL_<cat>: unbound variable`. The cryptic bash errors gave no indication of what was actually wrong.

- **Impact:** Tests crashed with confusing error messages instead of clear usage guidance
- **Root cause:** No argument-count validation in helper functions; reliance on `set -u` for catching misuse
- **Fix:** Added argument-count guards to all assertion functions (`assert_success`, `assert_error`, `assert_success_with_output`, `assert_empty`, `register_category`) and category validation in `_cat_inc_total`/`_cat_add_passed`. Wrong calls now produce explicit `USAGE ERROR` messages and record a test failure. Also added an API reference table to `blackbox-testing.md`.
- **Files changed:** `benchmarks/robotic-barista/scripts/blackbox-tests/helpers.sh`, `benchmarks/blackbox-testing.md`

### Verifiers Running Test Files Standalone Instead of Via Entry Point

Verification agents ran individual `scripts/blackbox-tests/test-*.sh` files directly instead of using the entry script `scripts/blackbox-test.sh`. The individual files depend on `helpers.sh` being sourced by the entry script, so standalone execution causes `register_category: command not found` and other undefined function errors. Sonnet-class models figured this out from context; weaker models did not.

- **Impact:** False rejections from verifiers due to test infrastructure errors, not actual code issues
- **Root cause:** `verification.md` Step 4 gave generic "run the project's test suite" guidance without mentioning entry scripts
- **Fix:** Added a note to Step 4 in `verification.md` instructing verifiers to use top-level test entry scripts when they exist, rather than running individual test modules directly.
- **Files changed:** `internal/templates/agent-templates/verification.md`

### Self-Verify Fallback Bypassing REJECTED Verdict

After a verifier delivered `[REJECTED]`, `oat pr create` printed the rejection but fell through to the self-verify fallback. If the worker ran `oat worker verify` (which uses `pytest`, not the blackbox harness) for the same commit SHA and it passed, the PR gate opened. This contradicts the worker prompt which says after `[REJECTED]`, fix and re-request review.

- **Impact:** Workers could bypass formal verification rejections by running self-verify on the same commit with a different test surface
- **Root cause:** The `prCreate` verification gate treated self-verify as a valid override of `REJECTED` for the same commit; no distinction between "verifier unavailable" fallback and "verifier explicitly rejected"
- **Fix:** Implemented commit-scoped blocking: when `VerificationStatus == "rejected"` and `VerifiedCommitSHA` matches the current HEAD, block the self-verify fallback and require the worker to fix, push a new commit, and `request-review` again. If the worker pushed a new commit (different SHA), self-verify is allowed as a valid fallback. `--force` always bypasses. Also updated `worker.md` to explicitly state self-verify cannot override a `[REJECTED]` verdict on the same commit.
- **Files changed:** `internal/cli/cli.go`, `internal/templates/agent-templates/worker.md`

### Ollama Cold-Start Crashes in Routing Mode

When multiple workers are spawned simultaneously using a local Ollama model, Ollama hasn't loaded the model into GPU/RAM yet. The agent processes time out and die within 20-25 seconds because the first inference triggers lazy model loading which can take 30-60+ seconds. In the routed Flash Lite + Gemma4 benchmark run, all 4 Gemma4 workers died this way, and the workspace permanently abandoned the model.

- **Impact:** All workers assigned to a local model fail on first use; workspace gives up on the model entirely
- **Root cause:** Ollama loads models lazily on first inference. Simultaneous requests from multiple workers compete for a model that isn't loaded yet, and agent processes time out before loading completes.
- **Fix:** Added an Ollama warm-up step in `benchmarks/run.sh` that detects `ollama:*` models and pre-loads them via the Ollama API (`/api/generate` with `keep_alive=30m`) before starting agents. Also warns when multiple Ollama models are detected, since `OLLAMA_MAX_LOADED_MODELS` defaults to 1. Added retry guidance to `workspace.md` so the workspace retries a model once before switching.
- **Files changed:** `benchmarks/run.sh`, `internal/prompts/workspace.md`

### Weak Orchestrator Model in Routing Mode

Using a cheap/weak model (e.g., `gemini-3.1-flash-lite-preview`) as the `--model` flag sets it for all orchestrator agents (workspace, supervisor, merge-queue), not just workers. The orchestrator needs to make model routing decisions, handle worker failures, and coordinate complex multi-agent workflows -- tasks that require a more capable model. In the routed benchmark run, Flash Lite couldn't write correct bash tests (score 28/100) and its workspace agent abandoned Gemma4 after transient startup failures instead of retrying.

- **Impact:** Gate failure (28/100), routing decisions ignored, worker failures mishandled
- **Root cause:** `--model` controls the orchestrator model. Users assumed it was only for workers when using `--routing-mode`.
- **Fix:** Updated `benchmarks/README.md` to document that `--model` sets the orchestrator and should be a strong model in routing mode. Updated the routing example to show `--model anthropic:claude-sonnet-4-6` with cheap models in `--available-worker-models`. Added `orchestrator_model` field to routing metadata for clarity.
- **Files changed:** `benchmarks/README.md`, `benchmarks/run.sh`, `benchmarks/collect.sh`

### Verifiers Approving Code with Infrastructure Failures

Verification agents saw `USAGE ERROR: assert_success requires at least 1 arg`, `ImportError`, `ModuleNotFoundError`, and `Error: No such command 'order'` in test output but still approved the PRs. These are infrastructure failures indicating the worker misused the test framework, not application bugs.

- **Impact:** Broken test modules were approved and merged, contributing to the gate failure
- **Root cause:** `verification.md` had no explicit guidance about infrastructure errors vs application errors. Weaker models couldn't distinguish between "test framework is broken" and "this specific test case failed."
- **Fix:** Added explicit infrastructure failure rejection criteria to `verification.md` Step 4: if test output contains `USAGE ERROR:`, `ImportError`, `ModuleNotFoundError`, `command not found`, or `syntax error`, auto-REJECT.
- **Files changed:** `internal/templates/agent-templates/verification.md`

### Verifier Approving Stub Tests (return 0 / exit 0)

Verification agent `verify-jade-hawk` approved a test where the main function contained `return 0` at the top, making the entire test suite a no-op that always passes with zero assertions. The verifier saw 0/25 scores but approved anyway.

- **Impact:** A stub test was merged, contributing to a 22/100 gate score since the category earned zero real points.
- **Root cause:** `verification.md` caught infrastructure errors but had no stub-detection criteria. A test that calls `register_category` but never executes any `run_cmd`/`assert_*` calls is effectively empty.
- **Fix:** Added stub detection criteria to `verification.md`: unconditional `return 0`/`exit 0` before any assertions = auto-REJECT; `register_category` with no subsequent test calls = scaffold-only REJECT. Also added a mandatory pre-approval checklist requiring verifiers to grep for error patterns before approving. Expanded "Common mistakes" in all wave:0 issue bodies (`issues.json`) to warn workers about stub patterns, double-counting, and fragile exact-string matching.
- **Files changed:** `internal/templates/agent-templates/verification.md`, `benchmarks/issues.json`

### Worker Force-Removing Its Own Verification Agent

Worker `bold-moose` ran `oat worker rm verify-bold-moose --force` after `oat worker request-review` failed with "agent 'verify-bold-moose' already exists". The worker was trying to clear the obstacle to re-request review, but force-removing its own verifier is incorrect behavior.

- **Impact:** Verification lifecycle disrupted. The worker eventually gave up on re-requesting review and self-verified instead, bypassing independent review.
- **Root cause:** After a REJECTED verdict, the old verifier agent was cleaned up from disk (worktree removed) but not from `state.json`. When the worker retried `request-review`, `handleStartVerificationAgent` rejected the spawn. The worker had no guidance on how to handle this error and improvised destructively.
- **Fix (prompt):** Added explicit rule to `worker.md`: "Never run `oat worker rm` or `oat worker create` targeting `verify-*` agents. If `oat worker request-review` fails with 'already exists', wait 30 seconds and retry."
- **Fix (daemon):** Made `handleStartVerificationAgent` idempotent for stale verifiers -- if the existing agent is not alive, auto-retire it from state before spawning the new one. Also mark verifiers `ReadyForCleanup` in `handleVerificationVerdict` so the health check picks them up faster.
- **Files changed:** `internal/templates/agent-templates/worker.md`, `internal/daemon/daemon.go`

### Stale Verifier Agent Blocking Re-Request Review

When a worker re-requests review after a REJECTED verdict, the CLI cleans up the old worktree on disk but never removes the old verifier agent entry from `state.json`. The daemon's `handleStartVerificationAgent` then rejects the spawn with "agent already exists."

- **Impact:** Workers that receive REJECTED verdicts and fix their code cannot re-request review until the daemon's async health check cleans up the stale verifier (up to 5 minutes due to startup grace period).
- **Root cause chain:** (1) `handleVerificationVerdict` records the verdict on the worker but does nothing to the verifier agent entry; (2) the verifier process exits or calls `oat agent complete`, but cleanup depends on the 2-minute health check cycle; (3) the 5-minute startup grace period keeps even dead verifiers in state; (4) the CLI's worktree cleanup doesn't include state cleanup.
- **Fix:** `handleStartVerificationAgent` now checks if the existing agent is alive before rejecting. Dead agents are auto-retired (stopped + removed from state). `handleVerificationVerdict` now marks the verifier `ReadyForCleanup` immediately after recording the verdict, so the health check picks it up after the 3-second post-completion delay instead of waiting for the full 5-minute grace.
- **Files changed:** `internal/daemon/daemon.go`

### Double-Counting in Worker Test Assertions

Workers called both `assert_success "test name" "category"` and `pass "test name" "category"` for the same test, inflating scores. `assert_success` already calls `pass` internally, so the second call double-counts.

- **Impact:** Gate scores were inflated, masking actual test coverage gaps.
- **Root cause:** The helpers API wasn't obvious about which functions call `pass`/`fail` internally. Workers treated assertion helpers and scoring helpers as independent steps.
- **Fix:** Added double-counting to the "Common mistakes" block in all wave:0 issue bodies (`issues.json`): "Calling both `assert_success` and `pass` for the same check is WRONG -- `assert_success` already calls `pass` internally."
- **Files changed:** `benchmarks/issues.json`

### Token Tracking Showing 0K for All Workers in collect.json

`collect.sh` reported zero tokens for every agent despite the runtime emitting `[OAT_TOKENS]` JSON lines during execution.

- **Impact:** Benchmark reports had no token usage data, making cost analysis impossible.
- **Root cause:** The agent runtime emits `[OAT_TOKENS]` to `sys.__stdout__` (the PTY), but the daemon always sets `OAT_TOOL_LOG`, which disables the PTY-to-file tee in `direct_backend.go`. The log files are written by the Python `ConversationLogger`, which only records conversation sections -- never `[OAT_TOKENS]` lines. Additionally, `collect.sh`'s `extract_tokens()` only looked for the legacy `X.XK tokens` pattern, not the `[OAT_TOKENS]` JSON format.
- **Fix:** Updated `_emit_oat_tokens()` in both `textual_adapter.py` and `non_interactive.py` to also append the `[OAT_TOKENS]` line to the `OAT_TOOL_LOG` file. Updated `collect.sh`'s `extract_tokens()` to parse `[OAT_TOKENS]` JSON lines with `jq`, falling back to the legacy pattern for older logs.
- **Files changed:** `agent-runtime/libs/cli/deepagents_cli/textual_adapter.py`, `agent-runtime/libs/cli/deepagents_cli/non_interactive.py`, `benchmarks/collect.sh`
