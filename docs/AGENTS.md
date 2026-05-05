# AGENTS.md

This file documents the agent system in oat for contributors working on agent-related code or extending the system.

## How the Team Works

Multiple workers execute tasks in parallel, each on its own branch. CI is the quality gate: any PR that passes CI gets merged. Progress is permanent.

```
Workers (parallel)                CI Gate                    Main Branch (progress)
┌─────────────────────────┐       ┌─────────────┐           ┌─────────────────────┐
│ Worker A: auth feature  │──PR──▶│             │           │                     │
│ Worker B: auth feature  │──PR──▶│  CI Passes? │──merge──▶ │  ████████████████   │
│ Worker C: bugfix #42    │──PR──▶│             │           │  (irreversible)     │
└─────────────────────────┘       └─────────────┘           └─────────────────────┘
       ▲                                │
       │                                │ fail
       └────────────────────────────────┘
              (retry or spawn fixup)
```

**Key implications for agent design:**
- Redundant work is acceptable and expected
- Failed attempts are not wasted; they eliminate paths
- Any PR that passes CI represents valid forward progress
- The merge-queue agent enforces the CI gate

## Agent Types

### 1. Supervisor (`internal/prompts/supervisor.md`)

**Role**: Orchestration and coordination (singleton)
**Worktree**: Isolated worktree (prevents prompt conflicts with other persistent agents)
**Lifecycle**: Persistent (runs as long as repo is tracked)

The supervisor monitors all other agents and nudges them toward progress. It:
- Receives automatic notifications when workers complete
- Sends guidance to stuck agents via inter-agent messaging
- Reports status when humans ask "what's everyone up to?"
- Never directly merges or modifies PRs (that's merge-queue's job)
- Never makes code changes itself (no `edit_file`, `write_file`, `git commit`, `git push`)

**Key constraint**: The supervisor coordinates but doesn't execute. It communicates through `oat message send` rather than taking direct action on PRs.

### 2. Merge-Queue (`internal/templates/agent-templates/merge-queue.md`)

**Role**: The CI gate - merges passing PRs into permanent progress (singleton)
**Worktree**: Isolated worktree (prevents prompt conflicts with other persistent agents)
**Lifecycle**: Persistent

This is the most complex agent with multiple responsibilities:

| Responsibility | Commands Used |
|----------------|---------------|
| Monitor PRs | `gh pr list --label oat` |
| Check CI | `gh run list --branch main`, `gh run view <run-id> --log-failed` (note: `gh pr checks` may fail with fine-grained PATs lacking `checks:read`) |
| Verify reviews | `gh pr view <n> --json reviews,reviewRequests` |
| Merge PRs | `gh pr merge <n> --squash --delete-branch` |
| Spawn fix workers | `oat worker create "Fix CI for PR #N"` |
| Handle emergencies | Enter "emergency fix mode" when main is broken |

**Critical behaviors:**
- Never merges PRs with unaddressed review feedback
- Never directly edits files, commits code, or pushes to any branch
- Enters emergency mode and halts all merges when main branch CI fails
- Tracks PRs needing human input with `needs-human-input` label
- Can close unsalvageable PRs but must preserve learnings in issues

**CI failure escalation ladder:**
1. Message the original worker first (wait 45s for response)
2. If original worker is gone, spawn ONE fix worker at a time (wait 45s before checking again)

### 3. Worker (`internal/templates/agent-templates/worker.md`)

**Role**: Execute specific tasks and create PRs
**Worktree**: Isolated branch (`work/<worker-name>`)
**Lifecycle**: Ephemeral (cleaned up after completion)

Workers are the "muscle" of the system. They:
- Receive a task assignment at spawn time
- Work in isolation on their own branch
- Create PRs with detailed summaries (so other agents can continue if needed)
- Enter dormant state after PR creation (`oat agent waiting`)
- Never spawn sub-workers
- Can ask supervisor for help via messaging

**Completion flow (dormancy model):**
```
Worker creates PR → Worker runs `oat agent waiting`
                         ↓
              Worker enters dormant state (zero token burn)
                         ↓
              Daemon monitors PR via `gh pr view` every 2 min
                         ↓
          ┌──────────────┼──────────────────────────┐
          │              │                          │
     PR merged      CI failure /              New comment
          │         merge conflict                  │
          ↓              ↓                          ↓
  Worker runs      Daemon wakes worker       Daemon wakes worker
  `oat agent       with targeted message     with comment details
   complete`       to fix the issue
```

The daemon monitors dormant workers for:
- **CI failures** — wakes worker to fix failing checks
- **Merge conflicts** — wakes worker to rebase/resolve
- **PR merged** — wakes worker to run `oat agent complete`
- **PR closed** — wakes worker to run `oat agent complete`

Workers also run `oat agent complete` when the supervisor explicitly instructs them to (e.g. stuck worker cleanup).
- **New comments** — wakes worker to address review feedback
- **Dormancy cap** (default 15 min, configurable via `OAT_WORKER_DORMANCY_CAP_MINUTES`) — if the PR is green and mergeable, the daemon force-merges it via `gh pr merge --squash` and auto-completes the worker (this compensates for a broken or hung merge-queue). If the PR is not green/mergeable, the worker is timed out as before

Dormant workers do not burn tokens and are excluded from idle mode calculations, so the system can properly enter idle mode even with dormant workers present.

**Issue comments (when task is issue-tied):** When a worker's task is tied to a GitHub issue (task or branch mentions it, or `oat worker create` was called with `--issue <number>`), the worker must:
- Post a **start comment** on the issue (e.g. "I have started working on this issue.") and **sign with their agent name** (same as branch: `work/<name>` → sign as `<name>`).
- Before running `oat agent complete`, post a **result comment** with the outcome (PR opened, no PR – reason, partial handoff, etc.) and sign with their agent name.

For the full list of result scenarios and example phrasing, see the worker template (`internal/templates/agent-templates/worker.md`). Using `--issue <number>` (and optionally `--issue-url <url>`) when creating the worker ensures reliable issue binding.

### 4. Workspace (`internal/prompts/workspace.md`)

**Role**: User's persistent interactive session
**Worktree**: Own branch (`workspace/<name>`)
**Lifecycle**: Persistent (user's home base)

The workspace is unique - it's the only agent that:
- Receives direct human input
- Is NOT part of the automated nudge/wake cycle
- Can spawn workers on behalf of the user
- Persists conversation history across sessions

### 5. Review (`internal/templates/agent-templates/reviewer.md`)

**Role**: Code review and quality gate
**Worktree**: PR branch (ephemeral)
**Lifecycle**: Ephemeral (spawned by merge-queue)

Review agents are spawned by merge-queue to evaluate PRs before merge. They:
- Post blocking (`[BLOCKING]`) or non-blocking suggestions as PR comments
- Report summary to merge-queue for merge decision
- Default to non-blocking suggestions unless security/correctness issues

### 6. Verification Agent (`internal/templates/agent-templates/verification.md`)

**Role**: Reviews a worker's changes before merge
**Worktree**: Detached at worker's commit SHA (ephemeral)
**Lifecycle**: Ephemeral (spawned when a worker calls `oat worker request-review`)

Verification agents are daemon-started (matching worker lifecycle) to ensure the PTY is owned by the long-running daemon process. The CLI command `oat worker request-review` prepares the worktree and prompt, then delegates agent process creation to the daemon via the `start_verification_agent` socket command. After spawning the verifier, `request-review` auto-calls `oat agent waiting` to put the worker dormant immediately — no manual dormancy step required.

If a worker calls `oat worker verify` while a verification agent has been active for < 5 minutes, the guard auto-calls `oat agent waiting` (putting the worker dormant) instead of returning a plain error. The >= 5 min and verifier-gone branches still fall through to self-verify as a fallback.

Verification agents:
- Review the diff on the worker's branch
- Run tests in an isolated worktree
- Deliver a verdict via `oat worker set-verdict`

### 7. PR Shepherd (`internal/templates/agent-templates/pr-shepherd.md`)

**Role**: Monitors and manages PRs in fork mode
**Worktree**: Main repository
**Lifecycle**: Persistent (used when working with forks)

The PR Shepherd is similar to the merge-queue but designed for fork workflows where you contribute to upstream repositories. It:
- Monitors PRs created by workers
- Tracks PR status on the upstream repository
- Helps coordinate rebases and conflict resolution

## Daemon (Background Process)

The daemon is **not an AI agent**. It is a deterministic Go process (`internal/daemon/`) that runs in the background, managing agent lifecycles and coordinating work through timers and programmatic checks. It has no LLM, no reasoning ability, and no capacity for nuanced judgment. It can check binary conditions (is a PR merged? has a timer expired?) but it cannot investigate *why* something is stuck or decide whether an agent is making meaningful progress on a complex task.

### Daemon Loops

The daemon runs four periodic loops (`time.NewTicker`). The health check fires every 2 minutes; the message router, wake/nudge, and PR monitor each fire every 60 seconds.

| Loop | Function | What it does |
|------|----------|-------------|
| Health check | `checkAgentHealth()` | Verifies agent sessions exist via the backend; self-heals by restarting sessions if missing; cleans up agents marked `ReadyForCleanup`; prunes orphaned worktrees and message directories |
| Message routing | `routeMessages()` | Delivers pending messages from `~/.oat/messages/` to agents via `backend.SendMessage`; serialized with a mutex to prevent concurrent delivery |
| Wake/Nudge | `wakeAgents()` | Sends periodic status-check nudges to active agents; manages idle mode transitions; skips agents nudged within the last 2 minutes |
| PR monitoring | `prMonitorLoop()` | Checks the status of PRs from dormant workers; wakes workers on CI failure, merge conflicts, new comments, or PR merge/close; at dormancy cap, force-merges green PRs or times out the worker |

### Worker Lifecycle Management

The daemon tracks worker state transitions and acts on them:

```
active ──(oat agent waiting)──────────────> dormant/waiting
  │                                               │
  ├──(oat worker request-review)──> auto-dormant  │
  │   (spawns verifier, auto-calls agent waiting) │
  │                                               ├──(PR merged)──> auto-completed ──> ready-for-cleanup
  │                                               ├──(PR closed, not merged)──> auto-completed (issue closed) ──> ready-for-cleanup
  │                                               ├──(no PR found)──> auto-completed (issue closed) ──> ready-for-cleanup
  │                                               ├──(CI failure/conflict)──> woken, back to active
  │                                               └──(15 min cap)──> timed out ──> ready-for-cleanup
  │
  ├──(oat agent complete)──> complete ──> ready-for-cleanup ──(health check)──> removed
  │
  └──(oat agent complete --worker <name>)──> supervisor completes worker ──> ready-for-cleanup
```

When a worker is auto-completed via `handleAgentWaiting` and its PR was not merged (or no PR exists), the daemon closes the worker's associated GitHub issue via `closeAssociatedIssue`. This prevents stale issues from blocking wave completion. Note: `autoCompleteWorker` (force-remove, daemon takeover) does NOT close issues — those scenarios may involve incomplete work, and the supervisor is notified to handle them.

When a worker enters `ready-for-cleanup`, the health check loop (next 2-minute cycle) stops the agent process, removes the worktree, and prunes messages.

### Stuck Worker Escalation

When the daemon detects a worker that isn't making progress (not completing, not entering dormancy), it follows a fixed escalation ladder based on nudge count:

| Nudge count | Action | Constant |
|-------------|--------|----------|
| < 5 | Normal status nudge | — |
| >= 5 | Alert supervisor + directive nudge | `stuckSupervisorNudge` (env: `OAT_STUCK_SUPERVISOR_NUDGE`) |
| >= 8 | Daemon takeover: programmatic git checks, possible auto-complete | `stuckDaemonNudge` (env: `OAT_STUCK_DAEMON_NUDGE`) |
| >= 20 | Force-remove worker and notify supervisor | `stuckMaxNudge` (env: `OAT_STUCK_MAX_NUDGE`) |

The escalation to the supervisor at nudge 5 is important: the supervisor is an AI agent that can actually investigate whether the worker is genuinely stuck or just thinking through a complex problem. The daemon cannot make this distinction -- it only knows the nudge count.

**Merged-PR fast-track:** Workers whose PR has been merged get fast-tracked — the daemon escalates to auto-complete after 3 nudges (6 minutes) instead of 8 (16 minutes). These workers only need to run `oat agent complete`, and if they can't (worktree deleted, model ignoring), waiting 16 minutes is wasteful.

**Supervisor nudge reset:** If the supervisor determines a worker is actively working (e.g., running a long test suite) and not actually stuck, it can run `oat worker reset-nudge <worker-name>` to reset the nudge count to zero. This gives the worker another full escalation cycle. The reset can only be used **once per worker** -- if the worker still hasn't completed after a second round of nudges, the daemon proceeds with its escalation. The `NudgeResetUsed` flag resets when a worker enters dormancy (starting a fresh active cycle).

**Supervisor-initiated completion:** If a worker's work is done (PR exists) but the worker process can't complete itself, the supervisor can complete it on its behalf: `oat agent complete --worker <worker-name>`. Permanent agent types (supervisor, workspace, merge-queue) are guarded against self-completion — running `oat agent complete` without `--worker` from the supervisor window will return an error instead of completing the supervisor.

**Admin commands are not tasks:** The supervisor prompt explicitly instructs the supervisor to run `oat worker rm` (alias: `remove`) and `oat agent complete --worker` directly in its own session. Spawning a worker for these one-line commands wastes an agent slot and generates unnecessary daemon nudges.

### PR Monitoring

When a worker enters dormancy via `oat agent waiting`, the daemon monitors the PR every 60 seconds:

| PR condition | Daemon action |
|-------------|---------------|
| CI passes, PR mergeable | Notifies merge-queue that PR is green |
| CI fails | Wakes worker with failure details to fix |
| Merge conflict | Wakes worker to rebase/resolve |
| PR merged | Wakes worker to run `oat agent complete` |
| PR closed | Wakes worker to run `oat agent complete` |
| New review comments | Wakes worker to address feedback |
| Dormancy cap elapsed (default 15 min) | If PR is green and mergeable: daemon force-merges via `gh pr merge --squash` and auto-completes worker. Otherwise: times out worker; notifies supervisor and merge-queue. Configurable via `OAT_WORKER_DORMANCY_CAP_MINUTES` |

The daemon checks PR status using `gh pr view` and `gh run list` -- it parses the output programmatically but does not interpret the meaning of failures or make strategic decisions about how to fix them.

**Post-merge main CI check:** When a PR is merged, the daemon launches a delayed check (90 seconds) of main branch CI via `gh run list --branch main`. If main CI is red, the daemon alerts the merge-queue to follow its "Main Branch CI Failures" protocol (notify supervisor, spawn fix worker, continue merging green PRs). Alerts are deduplicated to at most one per 5 minutes per repo to avoid flooding when multiple PRs merge in quick succession.

### Idle Mode

A repo enters idle mode when it has no active workers or review agents (agents that are dormant/waiting-for-PR or ready-for-cleanup don't count as active). In idle mode:

- Supervisor, merge-queue, and PR shepherd are **not nudged** (saves tokens)
- Workspace stuck detection is **skipped** (the workspace is expected to be idle)
- On transition to idle, one final nudge is sent so agents can do a last check
- A **delayed follow-up nudge** is sent to the merge-queue ~3 minutes after entering idle, catching PRs whose CI was still in-progress during the final nudge

The final nudge to the merge-queue instructs it to actively poll CI status (every 30 seconds, up to 5 minutes) if any PR has CI still running. The delayed follow-up is a safety net in case the merge-queue's polling is interrupted.

The repo returns to active mode as soon as a worker or review agent is spawned.

### Message Attribution

When the daemon sends messages on behalf of agent lifecycle events (completion notifications, auto-completion, PR status updates, timeout alerts), it uses `from: "daemon"` so recipients can distinguish automated messages from agent-authored ones. These messages are also prefixed with `[daemon]` in the body.

### Daemon vs Supervisor

The daemon and supervisor have complementary roles. The daemon handles mechanical, timer-driven operations; the supervisor handles judgment calls that require LLM reasoning.

| Responsibility | Daemon (programmatic) | Supervisor (AI agent) |
|---------------|----------------------|----------------------|
| Nudge workers periodically | Yes (2-min timer) | No |
| Detect stuck workers | Yes (nudge count threshold) | Yes (can investigate why) |
| Decide if worker is truly stuck vs thinking | No (only counts nudges) | Yes (can read context, assess complexity) |
| Auto-complete workers with merged PRs | Yes (programmatic PR check) | No |
| Force-remove unresponsive workers | Yes (after 20 nudges) | No |
| Route inter-agent messages | Yes (filesystem + backend delivery) | No |
| Monitor PR CI status | Yes (parses `gh` output) | No |
| Wake dormant workers | Yes (on PR events) | No |
| Coordinate task assignment strategy | No | Yes (decides what workers should work on) |
| Investigate worker problems | No (binary checks only) | Yes (can read logs, assess situation) |
| Spawn new workers | No | Yes (via `oat worker create`) |
| Manage idle mode transitions | Yes (automatic) | No |
| Restart crashed agents | Yes (health check self-healing) | No |

In short: the daemon is the **infrastructure layer** (keep things running, deliver messages, enforce timeouts) while the supervisor is the **strategic layer** (decide what to work on, investigate problems, coordinate agents).

## Agent Communication

Agents communicate via filesystem-based messaging in `~/.oat/messages/<repo>/<agent>/`.

### Message Lifecycle

```
pending → delivered → read → acked
```

| Status | Meaning |
|--------|---------|
| `pending` | Written to disk, not yet sent to agent |
| `delivered` | Sent to agent's session |
| `read` | Agent has seen it (implicit) |
| `acked` | Agent explicitly acknowledged |

### Message Commands

```bash
# From any agent:
oat message send <target> "<message>"
oat message list
oat message read <id>
oat message ack <id>
```

Note: The old `agent send-message`, `agent list-messages`, `agent read-message`, and `agent ack-message` commands are still available as aliases for backward compatibility.

### Implementation Details

Messages are JSON files in `~/.oat/messages/<repo>/<agent>/<msg-id>.json`:

```json
{
  "id": "msg-abc123",
  "from": "worker-clever-fox",
  "to": "supervisor",
  "timestamp": "2024-01-15T10:30:00Z",
  "body": "I need clarification on the auth requirements",
  "status": "pending"
}
```

The daemon routes messages every 60 seconds via `backend.SendMessage()`, which writes messages directly to the agent's PTY. See `pkg/backend/backend.go` for the `ProcessBackend` interface.

### Issue Creation

Agents create issues using `oat issue create` instead of raw `gh issue create`. This ensures proper labeling (`wave:N`, `blocker`, `oat`), structured issue bodies with DO/DON'T guidance, and (for blocker issues) automatic workspace notification.

```bash
# Worker reports a blocker -- wave label auto-detected from assigned issue
oat issue create --blocker --title "Blocker: ..." --body "..."

# Workspace creates fix issues during convergence
oat issue create --title "Fix: ..." --body "..." --wave fix-0 --label wave:fix-0 --file path/to/file
```

**Convergence loop:** After all waves complete, the benchmark runs a model-generated blackbox test in a loop. Fix-wave workers can modify both the implementation code and `scripts/blackbox-test.sh` (the test itself may have bugs, e.g., state isolation issues). If a worker determines a task is genuinely impossible, it should call `oat agent complete` to close the issue. The convergence loop exits early with a `no_progress` verdict if the exact same failure lines appear in 2 consecutive iterations (detected via failure-line hashing).

**Key behaviors:**
- All labels are auto-created on the GitHub repo if they don't exist (`gh label create --force`), so `oat issue create` works in any repo without prior label setup.
- If `--wave` is omitted, the command auto-detects the wave from the worker's assigned issue labels (e.g., if the worker is assigned to an issue labeled `wave:0`, the new issue inherits `wave:0`).
- The `--blocker` flag sends a message to the workspace agent via `oat message send workspace "Blocker issue #N created: ..."`, which triggers the workspace to spawn a worker with `oat work "Fix blocker #N" --issue N`.
- **Before creating blockers for CI failures**, workers should diagnose whether the failing test is for out-of-scope functionality, check if it's planned in later waves, and fix root causes (e.g., coarse skip guards) when possible. See the full guidance in the worker template (`internal/templates/agent-templates/worker.md`).

### Daemon Message Tagging

All daemon-initiated messages (nudges, stuck worker alerts, PR monitoring notifications, workspace health checks) are prefixed with `[daemon]` so agents can distinguish automated messages from human input. This helps agents calibrate their response — for example, a workspace agent receiving a `[daemon]` stuck detection nudge knows it may be a false positive rather than a direct human instruction.

## Daemon Health & Resilience

### Workspace Stuck Detection

"Stuck" in this context means the agent has been in a prolonged "Thinking..." state — the LLM is unresponsive or hanging on a request. It does NOT mean the agent is idle, waiting for user input, or between tasks. The signal is the agent's output log file (`~/.oat/output/<repo>/<agent>.log`) not being updated (no new PTY output).

The daemon monitors workspace agents for stuck thinking using output log modification times:

1. **Soft timeout (15 min)** — sends `Ctrl+C` to interrupt and a `[daemon]` nudge message asking the workspace to resume
2. **Hard timeout (30 min)** — restarts the workspace agent entirely

**Off by default.** The workspace is user-driven — it naturally goes quiet when the user steps away from their desk, and that's not "stuck," it's just waiting for human input. Restarting the workspace would destroy conversation context for no reason.

Enable per-repo: `oat config --workspace-stuck-detection=true`

Benchmark runs enable it automatically (in `benchmarks/setup.sh`) because benchmarks are unattended — no human is at the keyboard to notice or intervene if the workspace gets stuck thinking, so stuck detection is the only safety net and a stuck workspace wastes the benchmark's fixed time budget. Any similar unattended/autonomous workflow should enable it.

### Core Agent Stuck Detection

The daemon monitors merge-queue and supervisor agents for extended thinking periods using the same output log mtime approach as workspace stuck detection. When an agent's output log hasn't been modified for longer than the configured soft timeout (default 5 min, `OAT_CORE_AGENT_SOFT_TIMEOUT`):

1. **Soft timeout (5 min)** — sends ESC (0x1b) to cancel the thinking state, waits 2 seconds, then delivers a `[daemon]` nudge message. The nudge includes:
   - An explanation that the agent was auto-interrupted
   - Any messages delivered during the thinking period that were lost when ESC cleared the agent's pending message queue (capped to the 5 most recent)
   - A fresh state summary (open PRs + CI for merge-queue, worker status for supervisor)
2. **Hard timeout (15 min, `OAT_CORE_AGENT_HARD_TIMEOUT`)** — restarts the agent entirely via `restartAgent`

ESC (0x1b) is used instead of Ctrl+C because the `oat-agent` Textual TUI maps ESC to `action_interrupt` (cancels the in-flight API call) while Ctrl+C maps to `action_quit_or_interrupt` (can quit the app). ESC is the correct choice for soft interruption and works for all model backends.

**Always on** (skips idle repos). Core agents blocking directly impacts PR throughput for all users, unlike the workspace which is user-driven.

### Fetch Failure Handling

The daemon tracks consecutive `git fetch` failures per repo. After 3 consecutive failures (e.g., from a deleted GitHub repo returning 403), the daemon skips that repo's worktree refresh and branch cleanup to avoid log spam. The counter resets on a successful fetch.

## Agent Slash Commands

Each agent has access to oat-specific slash commands via `~/.oat/agent-config/`. These are automatically set up when agents spawn.

### Available Commands

| Command | Description |
|---------|-------------|
| `/refresh` | Sync worktree with main branch (fetch, rebase) |
| `/status` | Show system status, git status, and pending messages |
| `/workers` | List active workers for the repository |
| `/messages` | Check and manage inter-agent messages |

### Implementation

Slash commands are embedded in `internal/prompts/commands/` and deployed per-agent:

```
~/.oat/agent-config/<repo>/<agent>/
└── commands/
    ├── refresh.md
    ├── status.md
    ├── workers.md
    └── messages.md
```

The daemon sets up `~/.oat/agent-config/<repo>/<agent>/` when starting agents, which configures the slash commands available to each agent.

### Adding New Commands

1. Create `internal/prompts/commands/<name>.md` with instructions
2. Add to `AvailableCommands` in `commands.go`
3. Rebuild: `go install ./cmd/oat`

## Agent Prompts System

### Embedded Prompts

Default prompts are embedded at compile time via `//go:embed`:

```go
// internal/prompts/prompts.go
//go:embed supervisor.md
var defaultSupervisorPrompt string
```

### Custom Prompts (Configurable Agents System)

Repositories can customize agent behavior by creating markdown files in `.oat/agents/`:

| Agent Type | Definition File |
|------------|-----------------|
| worker | `.oat/agents/worker.md` |
| merge-queue | `.oat/agents/merge-queue.md` |
| reviewer | `.oat/agents/reviewer.md` |

**Precedence order:**
1. `<repo>/.oat/agents/<agent>.md` (checked into repo, highest priority)
2. `~/.oat/repos/<repo>/agents/<agent>.md` (local overrides)
3. Built-in templates (fallback)

Note: Supervisor and workspace agents use embedded prompts only and cannot be customized via this system.

**Deprecated:** The old system using `SUPERVISOR.md`, `WORKER.md`, `REVIEWER.md`, etc. directly in `.oat/` is deprecated. Migrate your custom prompts to the new `.oat/agents/` directory structure.

### Worker prompt extensions (project root)

You can add a folder **`oat-worker-prompt-extensions`** at the **project root** (repo root). Workers are instructed to read **all files** in that folder when it exists; the contents are treated as additional project-specific instructions and context. OAT does not create or manage this folder—you (or tools like Overlord) add it and commit it to the repo. Use it for specs, constraints, or context you want every worker to see without editing the base worker template.

### Managing Agent Definitions via CLI

```bash
# List agent definitions for the current repo
oat agents list

# Reset definitions to built-in defaults
oat agents reset

# Spawn a custom agent from a prompt file
oat agents spawn --name my-agent --class persistent --prompt-file ./custom.md
```

### Example: Customizing Worker Behavior

To customize how workers operate for your project:

1. First, ensure the default templates are copied to your local definitions:
   ```bash
   oat agents reset
   ```

2. Edit the worker definition:
   ```bash
   $EDITOR ~/.oat/repos/my-repo/agents/worker.md
   ```

3. Add project-specific instructions at the end:
   ```markdown
   ## Project-Specific Guidelines

   ### Commit Conventions
   - Use conventional commits: feat:, fix:, docs:, refactor:, test:
   - Reference issue numbers in commit messages

   ### Code Style
   - Follow the patterns in existing code
   - Run `make lint` before creating PRs
   - All new public functions need docstrings

   ### Testing Requirements
   - Add tests for all new functionality
   - Ensure `make test` passes before marking complete
   ```

4. To share with your team, move the customization to the repo:
   ```bash
   mkdir -p .oat/agents
   cp ~/.oat/repos/my-repo/agents/worker.md .oat/agents/
   git add .oat/agents/worker.md
   git commit -m "docs: Add worker agent conventions"
   ```

### Prompt Assembly

```
Final Prompt = Default Prompt + CLI Documentation + Custom Prompt
```

CLI docs are auto-generated via `go generate ./pkg/config`.

## Agent Lifecycle Management

### Spawn Flow (Worker Example)

```
CLI: oat worker create "task description"
         ↓
1. Generate unique name (adjective-animal pattern)
2. Create git worktree at ~/.oat/wts/<repo>/<name>
3. Start agent process via backend (creates PTY session)
4. Write prompt file with task embedded
5. Write prompt to {workDir}/.oat/AGENTS.md for runtime discovery
6. Register agent in state.json
7. Start output capture (log writer)
```

### Cleanup Flow

```
Worker runs: oat agent complete
  -- OR --
Supervisor runs: oat agent complete --worker <name>
         ↓
1. Agent marked with ReadyForCleanup=true in state
2. Daemon notifies supervisor + merge-queue
3. Health check (every 2 min) finds marked agents
4. Stop agent process via backend
5. Remove from state.json
6. Delete worktree
7. Clean up messages directory
```

### Health Check Cycle

The daemon runs health checks every 2 minutes with **self-healing behavior**:

1. Check if backend session exists for each repo
2. If session is missing, **attempt restoration** before cleanup:
   - Recreate the backend session
   - Restart supervisor, merge-queue/pr-shepherd, and workspace agents
   - Only mark agents for cleanup if restoration fails
3. For each agent, verify the agent process is alive via the backend
4. Clean up any agents with `ReadyForCleanup=true`
5. Prune orphaned worktrees (disk but not in git)
6. Prune orphaned message directories

This self-healing makes the daemon resilient to daemon restarts and other unexpected session losses.

## State Management

All agent state lives in `~/.oat/state.json`:

```json
{
  "repos": {
    "my-repo": {
      "github_url": "https://github.com/owner/repo",
      "session_name": "oat-my-repo",
      "agents": {
        "supervisor": {
          "type": "supervisor",
          "worktree_path": "/path/to/repo",
          "window_name": "supervisor",
          "session_id": "uuid-v4",
          "pid": 12345,
          "created_at": "2024-01-15T10:00:00Z"
        },
        "clever-fox": {
          "type": "worker",
          "worktree_path": "~/.oat/wts/my-repo/clever-fox",
          "task": "Implement auth feature",
          "ready_for_cleanup": false
        }
      }
    }
  }
}
```

State updates use atomic write pattern: write to `.tmp`, then `rename()`.

## Wake/Nudge System

The daemon periodically "nudges" agents to keep them active:

| Agent Type | Nudge Message |
|------------|---------------|
| supervisor | "Status check: Review worker progress and check merge queue." |
| merge-queue | "Status check: Review open PRs and check CI status." |
| worker | "Status check: Update on your progress?" |
| review | "Status check: Update on your review progress?" |
| workspace | **Not nudged** (user-driven only) |

Nudges are sent every 60 seconds, but agents are skipped if nudged within the last cycle.

**Idle mode:** For a given repo, when it is in **idle mode** (no workers or review agents, or all are ready for cleanup), the daemon does **not** nudge the supervisor, merge-queue, or PR shepherd for that repo until workers (or review agents) appear again. On transition to idle, the daemon sends one final nudge to those agents so they can do a last check, then schedules a delayed follow-up nudge to the merge-queue ~3 minutes later to catch PRs whose CI was still in-progress during the final nudge. After that, nudges pause. When at least one worker or review agent is present again, the repo returns to **active mode** and normal nudges resume.

## Testing Agents

### Unit Tests

Agent-related tests are in:
- `internal/messages/messages_test.go` - Message system
- `internal/state/state_test.go` - State management
- `internal/prompts/prompts_test.go` - Prompt loading

### Integration Tests

E2E tests use `OAT_TEST_MODE=1` to skip actual agent startup:

```bash
# Run integration tests
go test ./test/...

# Test specific recovery scenarios
go test ./test/ -run TestDaemonCrashRecovery
```

### Recovery Tests

`test/recovery_test.go` covers:
- Corrupted state file recovery
- Orphaned session cleanup
- Orphaned worktree cleanup
- Stale socket cleanup
- Orphaned message directory cleanup
- Daemon crash recovery
- Concurrent state access

## Adding a New Agent Type

1. **Define the type** in `internal/state/state.go`:
   ```go
   const AgentTypeMyAgent AgentType = "my-agent"
   ```

2. **Create the prompt template** at `internal/templates/agent-templates/my-agent.md`
   - Note: Only supervisor and workspace prompts are embedded directly in `internal/prompts/`
   - Other agent types (worker, merge-queue, review) use templates that can be customized

3. **Add the template** to `internal/templates/templates.go` for embedding

4. **Add prompt loading** in `GetDefaultPrompt()` if needed (for embedded prompts only)

5. **Add wake message** in `daemon.go:wakeAgents()` if needed

6. **Add CLI commands** if the agent needs special handling

7. **Write tests** for the new agent's behavior
