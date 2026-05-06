# Crash Recovery Guide

This document describes what happens when various oat components crash, what resources may become orphaned, and how to recover from each scenario.

## Overview of Components

OAT consists of several components that can fail independently:

| Component | Description | Persistence |
|-----------|-------------|-------------|
| **Daemon** | Central coordinator process | `daemon.pid`, `state.json` |
| **Supervisor** | LLM agent managing workers | in-memory session, no special state |
| **Merge-Queue** | LLM agent processing PRs | in-memory session, no special state |
| **Workers** | LLM agents executing tasks | in-memory sessions, git worktrees, branches |
| **Workspace** | User's interactive LLM agent | in-memory session, git worktree |
| **Backend Session** | Container for all agents in a repo | daemon process memory |
| **Git Worktrees** | Isolated working directories | filesystem + git metadata |

## Crash Scenarios and Recovery

### 1. Daemon Crash

**What happens:**
- The daemon process (`oat daemon _run`) terminates unexpectedly
- All background loops stop: health checks, message routing, agent waking
- Socket communication becomes unavailable
- CLI commands that need daemon will fail

**What gets orphaned:**
- `daemon.pid` file remains with stale PID
- `daemon.sock` file may remain
- All agent processes terminate (they are children of the daemon)
- State file (`state.json`) remains valid - last atomic write is preserved

**Automatic recovery:**
- On next `oat start`, daemon detects stale PID file via signal 0 check
- Stale PID file is removed and new daemon takes over
- State is loaded from `state.json`
- First health check runs immediately to verify agents

**Manual recovery:**
```bash
# Check if daemon is actually dead
ps -p $(cat ~/.oat/daemon.pid) 2>/dev/null

# Start daemon (handles stale PID automatically)
oat start

# Verify recovery
oat daemon status
```

**Impact:**
- All agents stop (they are daemon child processes)
- No periodic status nudges
- On restart, daemon restores sessions and relaunches agents from state

---

### 2. Supervisor Crash

**What happens:**
- Supervisor agent process exits unexpectedly
- Supervisor stops coordinating workers
- Workers continue independently but without guidance

**What gets orphaned:**
- State still shows supervisor as active

**Automatic recovery:**
- Health check detects the process is dead (every 2 minutes)
- Daemon automatically restarts supervisor using `--resume` to preserve session context
- PID is updated in state

**Manual recovery (if auto-restart fails):**
```bash
# Check supervisor window
oat agent attach supervisor

# Use the oat agent command to restart (auto-detects context and resumes)
oat agent
```

**Impact:**
- Brief interruption until health check runs (up to 2 minutes)
- Session context is preserved via --resume
- New workers won't be supervised during the gap

---

### 3. Merge-Queue Crash

**What happens:**
- Merge-queue agent process exits unexpectedly
- PR processing stops
- PRs may sit in queue without review/merge

**What gets orphaned:**
- State still shows merge-queue as active

**Automatic recovery:**
- Same as supervisor - health check auto-restarts with --resume

**Impact:**
- Brief interruption until health check runs (up to 2 minutes)
- PRs won't be processed during the gap
- Session context is preserved

---

### 4. Worker Crash

**What happens:**
- Worker agent process exits unexpectedly
- Work on task stops mid-progress
- Worker may have:
  - Uncommitted changes
  - Unpushed commits
  - Partial PR

**What gets orphaned:**
- git worktree with potential uncommitted work
- git branch
- Message directory for worker
- State entry for worker

**Automatic recovery:**
- Health check detects agent process is dead
- If `ReadyForCleanup` is not set, worker is preserved
- Changes are NOT automatically committed or pushed

### 4b. Verifier Crash

**What happens:**
- A `verify-<worker>` agent spawned by `oat worker request-review`
  exits before delivering an `approved`/`rejected` verdict.

**What gets orphaned:**
- The worker is still dormant with `WaitingForVerification: true`,
  pointing at a `VerificationAgent` that no longer has a process.

**Automatic recovery:**
- `cleanupDeadAgents` detects the dead verifier, clears
  `worker.VerificationStatus` and `worker.VerificationAgent`, and wakes
  the worker with the message: "your verification agent crashed before
  delivering a verdict — self-verify (`oat worker verify`) and create
  your PR (`oat pr create`)".
- The crash wake-message is **gated on `!verifier.ReadyForCleanup`**.
  A verifier that successfully delivered its verdict via
  `verification_verdict` sets `ReadyForCleanup=true`; the cleanup pass
  for that verifier therefore clears the stale worker pointer **without**
  emitting the bogus crash message. This prevents a race where another
  worker's concurrent `request-review` resets a still-pending status
  field and looks like a crash to the cleanup loop.

**Manual recovery:**
```bash
# Check worker status
oat agent attach <worker-name>

# Option 1: Continue the work manually
cd ~/.oat/wts/<repo>/<worker-name>
git status  # Check for uncommitted work
# Continue or restart agent

# Option 2: Save work and remove worker
cd ~/.oat/wts/<repo>/<worker-name>
git stash  # or: git add . && git commit -m "WIP"
git push -u origin work/<worker-name>
oat worker rm <worker-name>  # alias: remove

# Option 3: Force remove (lose uncommitted work)
oat worker rm <worker-name>
# Answer 'y' to warnings about uncommitted changes
```

**Impact:**
- Task incomplete
- Uncommitted work at risk
- Branch may have partial work

---

### 5. Workspace Crash

**What happens:**
- User's interactive agent session exits
- Similar to worker but typically has more user interaction

**Recovery:**
```bash
# Attach to check workspace status
oat agent attach workspace

# Use oat agent to restart (preserves session context)
oat agent
```

- Workspace worktree and branch are preserved
- Session context is resumed via --resume flag
- MOTD in terminal reminds you of this command

---

### 6. Backend Session Loss

**What happens:**
- All agent processes for a repo terminate (e.g., daemon crash, system restart)
- All agents in that repo are affected

**What gets orphaned:**
- State still shows all agents as active
- All worktrees remain on disk
- All branches remain in git
- All message directories remain

**Automatic recovery:**
- On daemon restart, health check detects missing sessions
- Daemon attempts session restoration: recreates sessions and relaunches agents
- If restoration fails, agents are marked for cleanup
- Worktrees removed (if workers)
- State updated

**Manual recovery:**
```bash
# Repair will handle session detection
oat repair

# Or reinitialize if needed
oat stop-all
oat start
oat init <github-url>  # Will fail if repo exists
```

**Impact:**
- All work in progress lost (if uncommitted)
- Daemon will attempt automatic restoration on restart

---

### 7. Git Worktree Corruption

**What happens:**
- Worktree directory deleted manually
- Git metadata out of sync
- Agent can't operate on files

**What gets orphaned:**
- Git worktree metadata (in .git/worktrees)
- State references non-existent path

**Automatic recovery:**
- `cleanupOrphanedWorktrees()` detects directories not in git
- `git worktree prune` cleans up stale metadata

**Manual recovery:**
```bash
# Prune git worktree metadata
git -C ~/.oat/repos/<repo> worktree prune

# Run cleanup
oat cleanup

# Repair state
oat repair
```

---

### 8. System Crash / Power Loss

**What happens:**
- All processes terminate immediately
- No graceful shutdown possible
- State file may be mid-write (but atomic rename protects this)

**What gets orphaned:**
- Stale `daemon.pid`
- Stale `daemon.sock`
- All agent processes gone (daemon died)
- All worktrees remain
- All branches remain
- State.json is valid (atomic write via rename)

**Recovery:**
```bash
# Start daemon (handles stale files)
oat start

# Repair state (detects missing sessions)
oat repair

# Check what remains
oat repo list
oat worker list
```

---

## Recovery Commands

### `oat repair`

**When to use:** After crashes, when state seems inconsistent with reality.

**What it does:**
1. Verifies each backend session exists
2. Verifies each agent process is alive
3. Checks worktree paths exist (for workers)
4. Removes agents with missing resources
5. Cleans up orphaned worktree directories
6. Cleans up orphaned message directories

**Limitations:**
- Requires daemon to be running
- Does not restore lost work
- Does not restart crashed agent processes

### `oat cleanup`

**When to use:** To clean orphaned files without full state repair.

**What it does:**
- With daemon: Triggers health check and cleanup
- Without daemon: Local cleanup of orphaned worktrees

**Dry-run mode:**
```bash
oat cleanup --dry-run
```

### `oat stop-all`

**When to use:** To completely stop everything and optionally reset state.

**What it does:**
1. Stops all agent sessions
2. Stops daemon
3. Optionally (`--clean`): Removes state files

**Preserves:**
- Repository clones
- Worktree directories
- Messages

---

## What Cannot Be Recovered

| Lost Resource | Cause | Prevention |
|---------------|-------|------------|
| Uncommitted changes | Worker crash before commit | Workers should commit early and often |
| Unpushed commits | Crash before push | Workers should push regularly |
| In-flight operations | Daemon crash during request | CLI will timeout; retry command |
| Message delivery | Crash during delivery | Message stays pending; delivered on restart |
| Partial PR | Worker crash mid-PR creation | Check GitHub for draft PRs |

---

## Diagnostic Commands

### Check Component Health

```bash
# Daemon status
oat daemon status

# View daemon logs
oat daemon logs
tail -f ~/.oat/daemon.log

# List all agents
oat status

# Check state file
cat ~/.oat/state.json | jq .
```

### Check for Orphaned Resources

```bash
# Orphaned worktrees (directories without git tracking)
ls ~/.oat/wts/<repo>/
git -C ~/.oat/repos/<repo> worktree list

# Orphaned message directories
ls ~/.oat/messages/<repo>/
# Compare with agents in state.json

# Orphaned branches
git -C ~/.oat/repos/<repo> branch | grep work/
```

### Check Process Status

```bash
# Is daemon running?
ps -p $(cat ~/.oat/daemon.pid) 2>/dev/null

# Find all agent processes
ps aux | grep agent

# Check agent process
oat status  # Shows PIDs for each agent
```

### Workers idle / last_nudge stays zero

If workers spawn but never start their task and `last_nudge` stays at zero in `state.json`, the daemon is **skipping** the wake/nudge for those workers. The wake loop only sends a nudge when it believes the pane is running the agent process (`isagentProcess(pid)`). On macOS, the process name from `ps -o comm=` is truncated to 16 characters, which can make that check fail so nudges are never sent.

**Fix:** Rebuild the oat binary and restart the daemon so it uses the updated check (full command line via `ps -o args=`). Then confirm in daemon logs that workers are being woken (e.g. no "skipping wake" / "not agent process" for those agents).

```bash
go build ./cmd/oat
oat stop
# run the new binary (e.g. ./oat start or reinstall)
oat start
tail -f ~/.oat/daemon.log   # look for "Woke agent" or "skipping wake"
```

Task messages from the supervisor are delivered by the **message router** (every 60 seconds) and do not depend on the PID check; only the periodic nudge does.

### Workers: messages appear in prompt but are not processed

If text sent to agents (nudges, task messages, or manual `oat agent tell`) appears in the worker's prompt area but the agent never acts on it, OAT Agent Runtime may not be accepting input correctly. OAT sets `TERM_PROGRAM=terminal` (or your existing value from the environment) when starting agents so the runtime treats the session as a normal terminal. Ensure you are running a **rebuilt binary** and that agents were started by that binary (restart the daemon, and create new workers or restart existing ones so they inherit the env). You can override the value in `~/.oat/.env` or `~/.oat/repos/<repo>/.env`, e.g. `TERM_PROGRAM=Apple_Terminal` or `TERM_PROGRAM=iTerm.app` if needed.

---

## Best Practices for Resilience

### For Workers

1. **Commit early and often** - Don't accumulate too many uncommitted changes
2. **Push to remote** - Unpushed commits are only local
3. **Signal completion** - Always run `oat agent complete` when done

### For Operators

1. **Monitor daemon logs** - Watch for repeated errors
2. **Run periodic repair** - `oat repair` is safe to run regularly
3. **Check orphaned resources** - Especially after system crashes

### System Configuration

1. **Process supervisor** - Use systemd/launchd to auto-restart daemon
3. **Log rotation** - Daemon log can grow large

---

## Architecture Decisions

### Why we preserve agent state on crash

When an agent process dies, we preserve its state entry and worktree because:

1. User may want to inspect the final state (via log files)
2. User may want to manually restart the agent
3. Cleaning up could lose context about what happened

### Agent auto-restart behavior

Persistent agents (supervisor, merge-queue, workspace) are automatically restarted by the daemon's health check loop with `--resume` to preserve session context. Workers are not auto-restarted to avoid potential restart loops and because their task context may need manual intervention.

### Why state.json uses atomic writes

The state file is written atomically (write to temp file, then rename) because:

1. Prevents corruption from mid-write crashes
2. Ensures consistent reads even during writes
3. Rename is atomic on most filesystems

---

## Future Improvements

See GitHub issue #23 for tracking. Potential enhancements:

1. **State backup** - Periodic backups of state.json
2. **Process monitoring** - Detect dead agent processes, not just missing windows
3. **Work-in-progress protection** - Auto-stash uncommitted changes before cleanup
4. **Graceful worker shutdown** - Allow workers to save state on SIGTERM
5. **Health status API** - Expose detailed health info via CLI
