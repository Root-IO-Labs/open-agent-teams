# Benchmark Convergence Strategy

## Current Architecture

The benchmark has four phases:

```
Gate Phase (wave:0)
  |-- Model generates scripts/blackbox-test.sh from spec + docs + issues
  |-- LLM judge scores it against human-written acceptance-test.sh (0-100)
  |-- If score < threshold (default 70): STOP
  |-- If score >= threshold: proceed to waves
  v
Waves 1-4
  |-- Each wave has a 30-min timeout
  |-- Grand timeout: 240 min across all waves
  |-- Agents build the software, create PRs, merge via CI
  v
Convergence Loop (wave:fix-0 through wave:fix-3)
  |-- Run model-generated blackbox-test.sh against the built software
  |-- If exit code 0 (PASS): proceed to final scoring
  |-- If FAIL: send failure report to workspace agent
  |-- Workspace creates issues labeled wave:fix-N, spawns workers to fix
  |-- Wait for fix issues to close, then re-run the test
  |-- Repeat until pass, max 4 iterations, or convergence timeout (default 60 min)
  v
Final Scoring
  |-- Run acceptance-test.sh (human ground truth)
  |-- Last convergence blackbox test result saved as blackbox-acceptance.json
```

### Anti-Cheat Design

- `acceptance-test.sh` (human-written reference): agents **never** see this. It lives outside the repo in the benchmarks folder.
- `scripts/blackbox-test.sh` (model-generated): agents **can** see this. It's committed to the repo by the gate worker. A copy is stored externally in the results directory so agents can't weaken it.
- The benchmark script (deterministic software) runs both tests, not the agents.

Agents reading the model-generated blackbox test is **not cheating**. It was written by a peer agent from the spec alone. Only seeing the human-written `acceptance-test.sh` would be cheating.

### Why This Makes Sense

- The gate proves the model can **understand** the spec (write a good test)
- The waves prove the model can **build** the software (close issues)
- The convergence loop proves the model can **finish** the software (pass its own test)
- The acceptance test proves the software **actually works** (human ground truth)

---

## Design Decisions (confirmed 2026-03-20)

### 1. Convergence Loop Timeout

**Decision: Finite timeout (configurable, default 60 min).**

A maximum timeout prevents runaway token costs. Some models enter infinite loops or burn tokens without progress. The convergence loop has both an overall timeout (`--convergence-timeout`) and a per-iteration timeout (`--convergence-iter-timeout`, default 30 min matching wave timeouts). Maximum 4 iterations (wave:fix-0 through wave:fix-3).

### 2. Agents and the Blackbox Test During Waves

**Decision: Agents have passive awareness, plus testing issues reference it.**

Agents are not explicitly told to use `scripts/blackbox-test.sh` as a development north star during waves, but they can read it since it's in the repo. Testing issues (#17-19) now reference it as context for writing integration and system tests. Per John: "it should use the blackbox test knowledge to inform how it's going to make integration tests."

### 3. Wave Timeouts

**Decision: Keep wave timeouts as-is, add convergence loop after.**

Per John: "We'll have to experiment." Wave timeouts remain at 30 min as a safety net for models that freeze or can't make progress. This may be revisited after more data from model screening.

### 4. Convergence Loop Mechanism

**Decision: Supervisor-driven -- send failure report to workspace agent, let it create issues and coordinate.**

When the blackbox test fails, the benchmark script sends the failure details to the workspace agent. The workspace creates new issues labeled `wave:fix-N` (one per failure), spawns workers to fix them, and the script waits for those issues to close before re-running the test. This mirrors the existing wave pattern.

### 5. Blackbox Test Score in Convergence

**Decision: Binary (pass/fail based on exit code).**

Per John: "Does it test everything or not? It's a binary thing. If it doesn't test all of the features, it's not ready for prime time." Convergence requires exit code 0 from the blackbox test. The acceptance test (human ground truth) retains its weighted scoring system for final evaluation.

---

## Future Considerations

### LLM Tutoring

John suggested using winning models as "tutors" -- asking Sonnet (or other high-performing models) to write a `blackbox-testing.md` guide that helps any LLM write better blackbox tests from a specification. "Use the winners to train the losers." This is a separate exploratory task.

### Winner Retrospectives

Winning models could also be asked: "How would you structure these tickets better to make any LLM be successful at building this application?" This retroactive analysis would inform issue design for the overlord.

### Model Screening

Before running full benchmarks with the convergence loop, all models should be screened via `--gate-only` to see which ones can achieve a 70+ gate score. This filters out models that can't understand the spec well enough to write a good test, saving significant time and tokens.

---

## Haiku Worker Behavior Observations (2026-03-25)

During the Haiku 4.5 convergence benchmark (`sonnet46-verifycommandtest` branch), several model-capability issues were observed. These are not OAT bugs -- Sonnet-class workers do not exhibit these patterns.

### Premature Completion

Workers like `flame-deer` (issue #6) correctly identified that CI failures were caused by cross-issue dependencies ("CLI contract tests need issue #5 work, not issue #6"), but then ran `oat agent complete` anyway, leaving the issue open since the PR never merged. The daemon nudged 3 times about CI failure; the worker rationalized its way to "done."

### Verification Bypass

When `oat pr create` rejected a PR due to failing verification, workers fell back to `gh pr create` directly, bypassing the verification gate and losing merge-queue integration. This is explicitly prohibited in the worker prompt, but Haiku workers don't follow the constraint reliably.

### Orphaned PRs

`forest-albatross` (issue #10) had PR #32 merged successfully, then pushed a whitespace cleanup commit that created PR #34. The daemon said "PR #32 merged, run `oat agent complete`" -- the worker obeyed and exited, leaving PR #34 open with failing CI.

### Blocker Issue Routing Gap

Issue #42 (a blocker) was created without a `wave:` label, so the supervisor never routed it and no worker was spawned. The `oat issue create --blocker` command (added in the benchmark reliability fixes) addresses this by enforcing proper labeling and automatically notifying the workspace.

### Diagnostic Truncation in Convergence Messages (2026-03-28)

During the Haiku 4.5 convergence run (68/100 gate, `feature/integrate-verification` branch), fix-wave workers repeatedly fixed peripheral issues (state persistence, output formatting) while the actual crash -- `KeyError: 'ingredient_name'` in the application's order validation -- was invisible.

**Root cause chain:**
1. The blackbox test's `setup_cmd` pattern truncated error output at 200 chars (`head -c 200`). A Python traceback through Click is 1000+ chars, so only `"Traceback (most recent call last):"` survived -- the actual `KeyError` was completely lost.
2. `run.sh` extracted failure lines via `grep -iE 'FAIL|ERROR'`, which captured symptoms ("validate order fails", "not in VALIDATED state") but not root causes.
3. The workspace received identical symptom-only failure messages across iterations, with no traceback or exception information.

**Fix:** Three changes: (1) `blackbox-testing.md` updated `setup_cmd` pattern from `head -c 200` to `head -c 500` plus `tail -1` for the actual exception line. (2) `run.sh` now extracts Python exceptions (KeyError, TypeError, etc.) from raw output and includes them as "Error diagnostics" in the convergence message. (3) `run.sh` now includes `fatal_lines` from the JSON output.

### Workspace Skipping Fix Iterations (fix-1 skip, 2026-03-28)

The workspace agent received the fix-1 convergence message but decided not to create issues, reasoning that failures were identical to fix-0 and previous workers were "still in progress." In reality, all fix-0 workers had already finished and submitted their PRs -- but `oat worker list` showed them as "running" because the status display did not distinguish between actively running workers and dormant workers. This has been fixed: workers now show distinct "waiting for PR" and "waiting for verification" statuses depending on their dormancy type.

This was a compound failure: (1) identical symptom messages gave no signal that the previous fixes were ineffective, (2) `oat worker list` misrepresented dormant workers as "running," and (3) Haiku's reasoning incorrectly concluded "same symptoms = workers still handling it."

**Fix:** (1) `run.sh` convergence prompt now includes previous iteration summary (issues created, PRs merged) and result diffs to flag identical results. (2) `oat worker list` now shows "waiting for PR" status for dormant workers instead of "running." (3) Convergence prompt instructs workspace to also check `gh pr list --state merged` for authoritative merge status. (4) `workspace.md` now mandates immediate action on every convergence message.

### Worktree Deletion During Active Use (2026-03-28)

During the Haiku 4.5 benchmark (`oat-bench-haiku45-verifycommandtest`), three workers had their worktree directories deleted while their processes were still active:

- **gentle-elk** (issue #9, PR #31): Worktree deleted between a successful `read_file` and a failed `edit_file`. Every subsequent command failed with `FileNotFoundError`. The worker spent 20+ minutes in this state before auto-completion. PR #31 had merge conflicts that gentle-elk could never fix because its worktree was gone.
- **twilight-raccoon** (issue #15): Worktree deleted mid-operation. The supervisor attempted to help by running `oat agent complete --worker twilight-raccoon` — but since `--worker` didn't exist as a flag, this completed the supervisor instead. The repo lost its supervisor at 19:27:11.
- **witty-elk** (issue #12): Worktree deleted after its PR was merged. The daemon woke it to run `oat agent complete`, but every command failed. Witty-elk accumulated 16+ nudges before the daemon auto-completed it, and its continued "running" status prevented idle mode from enabling.

**Root cause:** The daemon's `cleanupOrphanedWorktrees` (every 2 minutes) calls `git worktree list` and deletes any directory not in the list. When the daemon was simultaneously running `git worktree remove --force` for other dead agents, the concurrent modification of `.git/worktrees/` caused `git worktree list` to return incomplete results. Active worktrees were mistakenly identified as orphans and deleted.

**Fixes applied:** (1) `cleanupOrphanedWorktrees` now builds a protected set from daemon state — active (non-completed) agent worktrees are never deleted even if git tracking is out of sync. (2) `wt.Prune()` moved to run before the orphan check. (3) `--worker` flag added to `oat agent complete` so supervisors can legitimately complete workers. (4) Permanent agent types (supervisor, workspace, merge-queue) are guarded against self-completion. (5) Merged-PR workers are fast-tracked to auto-complete after 3 nudges (6 min) instead of 8 (16 min).

### Wasted Convergence Iteration Due to Unmerged PRs (2026-03-28)

During the Haiku 4.5 v2 benchmark (`20260328-043720`), convergence iteration 2 produced identical results to iteration 1 (42 passed, 2 failed). The fix-1 worker (lively-tiger, PR #52) had completed and the issue was closed, but the merge-queue hadn't merged the PR before the next convergence test ran against `main`. The fix code was on an unmerged branch, so the test saw the same unfixed codebase.

This is a tooling gap, not a model issue. The 30-second straggler wait after `wait_for_wave` is not enough for the merge-queue to process the PR. **Fix:** Added a merge verification loop in `run.sh` that polls for merged PRs (every 15s, up to 2 minutes) before re-running the blackbox test.

### Circular CI Dependency and Issue Decomposition (2026-03-29)

The Haiku 4.5 v3 benchmark (`20260328-072046`) scored 10.8/100 -- a dramatic drop from the v2 run's 74.6/100 on the same benchmark. The root cause was a **circular CI dependency** created by how Haiku decomposed wave 2/3 work.

**Comparison of the two Haiku runs:**

| Aspect | v2 (74.6/100) | v3 (10.8/100) |
|--------|---------------|---------------|
| Wave 0 contract tests | Lenient: didn't require all commands to exist | Strict (PR #25): tested ALL CLI commands |
| CLI command PRs | Each self-contained, merged individually | Each only added one command group, CI required all |
| Fix-wave strategy | Targeted bug fixes | Further fragmented into per-command registration PRs |
| Final state | 33 PRs merged, convergence PASS | 11 PRs open with failing CI, convergence timeout |
| Rebase loops | Minor (2-4 per worker) | Severe: quantum-leopard 23, proud-badger 20, ocean-iguana 18 |

**The deadlock pattern:**
1. Wave 0 worker (`nexus-elephant`) created "CLI Commands Interface Contract" (PR #25) with tests requiring ALL CLI commands to be registered
2. Wave 2/3 workers each created separate PRs adding one command group (inventory, recipe, order, etc.)
3. CI ran the contract tests on each PR -- every PR failed because the other commands weren't registered yet
4. Fix-0 wave created 5 more granular "register one command" issues (#45, #50-53), worsening the deadlock
5. Fix-1 worker (dusk-moose, PR #55) tried to consolidate but failed CI due to conflicting test expectations from all the open PRs

**Why this didn't happen in v2:** The v2 run's wave 0 contract tests were more lenient -- they didn't hard-require all commands to exist simultaneously. CLI workers in v2 could merge independently.

**Fixes applied:** (1) Spec patch making CLI registration coupling explicit. (2) Workspace convergence prompt guidance about recognizing and consolidating circular CI dependencies. (3) Rebase loop circuit breaker (10 wakes threshold) to prevent token waste.

### Model Reliability for Convergence

Based on observations:

- **Sonnet 4.6, GPT 5.4**: Reliable for full convergence loops. Follow instructions consistently, handle CI failures correctly, don't bypass verification.
- **Haiku 4.5**: After worktree lifecycle fixes (protectedPaths, permanent agent guard, merged-PR fast-track), Haiku successfully completed a full convergence run (`20260328-043720`): 74.6/100 acceptance, PASS in 4 iterations, 100% self-completion rate, all 4 waves completed. However, a subsequent run (`20260328-072046`) scored 10.8/100 due to a circular CI dependency caused by strict wave 0 contract tests and fragmented CLI registration PRs (see "Circular CI Dependency" above). This demonstrates that Haiku's convergence viability depends heavily on how it decomposes work -- the same model can produce dramatically different outcomes based on issue decomposition variance.
- **Haiku 4.5 Thinking**: Marginally better than regular Haiku in gate scoring but insufficient data on convergence.

### Haiku 4.5 v5 Run Analysis (2026-03-30)

**Run:** `oat-bench-haiku45-verifycommandtest` | Branch: `feature/bundle-robotic-barista` (bundled benchmark repo)

The v5 run used the bundled benchmark approach (no private repo clone) and included all Round 4 fixes from the merged PR #45. Despite these fixes, the run produced 14 open issues and 8 open PRs, all failing CI. Key findings:

1. **Circular CI dependency persists**: Wave 0 contract tests still assert all CLI commands exist, creating the same deadlock pattern seen in v3. The spec's "Architecture note" about CLI registration coupling was not sufficient for Haiku -- it continued writing strict assertion-based tests.

2. **`blocker` label failure**: Workers used `oat issue create --blocker` but the benchmark repo didn't have a `blocker` label, causing `gh issue create` to fail silently on the label. Workers retried without `--blocker`, losing the label and workspace notification.

3. **Missing wave labels on blockers**: Workers creating blocker issues didn't pass `--wave`, so blocker issues lacked wave context. Only 1 of 5 blocker issues had a wave label.

4. **Linting non-compliance**: Despite the "Tip" in the worker prompt about running linters, Haiku workers consistently skipped `ruff format` before pushing. Multiple PRs failed CI with trivial formatting errors.

5. **Fix-wave timeout**: The first fix wave timed out partly because `caffeinate` expired (set for 2 hours, fix-0 started after waves 0-3 consumed the time budget).

**Comparison with stronger models**: Sonnet 4.6 avoids the circular CI dependency by writing contract tests that use conditional checks (`pytest.importorskip`, `skipif`) for unimplemented features. This is a model capability difference -- Sonnet anticipates incremental implementation while Haiku writes tests that assume all features exist.

**Round 5 fixes applied**: (1) `blocker` label added to `setup.sh`. (2) `oat issue create` auto-creates all labels and auto-detects wave from assigned issue. (3) Test resilience guidance added to `CLAUDE.md` and issue #2 body. (4) Linting enforcement added to `CLAUDE.md`, issue bodies, and the OAT worker prompt.

### Haiku 4.5 v6 Run Analysis (2026-03-31)

**Run:** `oat-bench-haiku45-verifycommandtest` | Branch: with Round 5 fixes

This run revealed a chain of worker lifecycle failures centered on the `silly-viper` worker (Issue #6: JSON Storage). The run was stopped manually after the merge-queue stalled.

**Chain of failures:**

1. **Verification agent crash** (13s lifetime): `silly-viper` called `oat worker request-review`, which spawned `verify-silly-viper`. The verification agent died instantly because it was started by the CLI's ephemeral PTY backend — when the CLI command finished, the PTY closed and killed the agent. Log was completely empty.

2. **Worker stuck in thinking loop**: After the verification agent crashed, `silly-viper` attempted to resolve merge conflicts in its PR (#26) but got stuck in a 397-second "Thinking" loop while trying to reconcile its implementation with code merged from other PRs.

3. **Supervisor force-removed the worker**: At nudge 11, the supervisor ran `oat worker rm silly-viper --force`. The CLI tried to kill the process via its own empty backend — failed silently. The daemon removed the state entry but did not kill the process. `silly-viper` continued running as a zombie.

4. **Branch cleanup orphaned PR #26**: The daemon's `cleanupMergedBranches` flagged `work/silly-viper` as "merged" (false positive from `git branch --merged`) and deleted both local and remote branches. GitHub auto-closed PR #26 when its head branch was deleted.

5. **Zombie created PR #28**: The zombie `silly-viper` agent, still running, re-created its worktree via a spawned sub-agent and created PR #28 for the same work. This PR had no worker in state to manage it.

6. **Merge-queue stall**: The merge-queue got stuck with two unmergeable PRs — PR #26/28 (no CI triggered) and PR #27 (circular CI dependency). It eventually recovered when PR #27's worker pushed a fix, but PR #28 was left orphaned with merge conflicts.

**Root causes identified:** (1) Verification agents spawned by wrong process (CLI vs daemon). (2) `handleRemoveAgent` doesn't kill agent processes. (3) `cleanupMergedBranches` doesn't check for open PRs before remote branch deletion. (4) Stuck-worker alert lacks guidance about open PRs and workspace notification.

**Round 6 fixes applied**: See "Worker Lifecycle Issues (2026-03-31)" in `docs/known-issues.md`.
