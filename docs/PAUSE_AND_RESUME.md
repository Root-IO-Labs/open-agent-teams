# Pause & Resume

How to stop OAT when you need your machine for other things, and pick up where you left off later.

## Assistants

Assistants are interactive, chat-style agents (the side-panel
Chat tab's targets) that live alongside the autonomous worker
/ supervisor agents in a repo. They have their own pause /
delete semantics that differ from worker semantics:

- **`oat assistant stop <name>`** — pause the assistant.
  Process is killed but the `state.Agent` record (worktree
  path, session JSONL, model preference) is **preserved**.
  Subsequent `oat assistant restart <name>` resumes at the
  same session JSONL.
- **`oat assistant restart <name>` `[--fresh]`** — bring a
  stopped assistant back. Without `--fresh`, the previous
  session JSONL is reused. With `--fresh`, the JSONL is
  **rotated**, not deleted: it becomes
  `<name>.session.jsonl.1` (existing `.1` rotates to `.2`,
  etc.). Default cap is 3 archives × 50 MB; older archives
  evict oldest-first. The rotation policy is local-storage
  only — there is no external backup.
- **`oat assistant remove <name>`** (alias **`rm`**) — destroy
  the assistant. State record wiped, virtual repo at
  `_assistant-<name>` removed. This is the destructive flow;
  `stop` is the right command for everyday "I'm done chatting
  for now".
- **`oat agent stop --repo <repo> --agent <name>`** —
  universal pause for any pausable agent (Assistant +
  browser-agent). Same semantics as `oat assistant stop` but
  also accepts a browser-agent target. Workers / supervisors
  / merge-queues are rejected with
  `RPC_AGENT_TYPE_NOT_PAUSABLE` — use `oat repo hibernate`
  for those.
- **`oat agent remove <name> [--repo <repo>]`** (alias **`rm`**)
  — generic, type-aware remove. Routes to the right
  type-specific cleanup automatically: assistants get the full
  JSONL + virtual repo dir wipe, workers get worktree teardown
  (still gated on `--force`), and other types
  (browser-agent, merge-queue, supervisor, workspace, pr-shepherd,
  review, verification, generic-persistent, agent-builder) get
  process-kill + state-record removal + an `agent_removed`
  lifecycle frame. The side-panel Delete buttons dispatch this
  verb. See `docs/COMMANDS.md` for the full routing table.

The **side-panel "Pause OAT" button** wraps the bulk-pause
verb (`pause_web_agents`) which enumerates every Assistant
+ browser-agent across every repo and stops them. Workers,
supervisor, merge-queues, and the daemon itself **keep
running** — pause those from the terminal with
`oat repo hibernate`. See `docs/extending/SOCKET_API.md`
for the wire schema.

## Quick Pause (Per-Repo)

Stop workers only (persistent agents keep running but enter idle mode):

```bash
oat repo hibernate --repo my-project
```

Stop everything in a repo (supervisor, workspace, merge-queue, workers):

```bash
oat repo hibernate --repo my-project --all --yes
```

This:
- Stops all agent processes
- Archives uncommitted changes as `.patch` files under `~/.oat/archive/<repo>/<timestamp>/`
- Removes agent worktrees (frees disk space)
- Preserves output logs at `~/.oat/output/<repo>/`
- Keeps the repo registered in `state.json` for auto-restore on resume

## Full Pause (System-Wide)

Stop all agents across all repos and shut down the daemon:

```bash
oat stop-all
```

This kills all agent sessions and stops the daemon. State is preserved -- repos remain registered and will be restored on next startup.

## Resume

Start the daemon. It auto-restores all persistent agents for every registered repo:

```bash
oat start
```

On startup, the daemon:
1. Reads `state.json` to find registered repos
2. Creates backend sessions for each repo
3. Starts supervisor, workspace, and merge-queue (or pr-shepherd in fork mode)
4. Sends informational context to the supervisor about available agent types

Workers are **not** auto-restored -- they're ephemeral. Create new ones as needed:

```bash
oat worker create "Continue work on feature X" --repo my-project
```

## Full Stop + Cleanup

Destroy all worktrees and state (cloned repos are kept):

```bash
oat stop-all --clean
```

After this, repos must be re-initialized with `oat init`.

## What Is Preserved vs Lost

| Resource | After Hibernate | After `stop-all` | After `stop-all --clean` |
|----------|----------------|-------------------|--------------------------|
| `state.json` (repo registrations) | Kept | Kept | Deleted |
| Output logs (`~/.oat/output/`) | Kept | Kept | Deleted |
| Cloned repos (`~/.oat/repos/`) | Kept | Kept | Kept |
| Agent worktrees (`~/.oat/wts/`) | Removed | Removed | Removed |
| Worker assignments | Lost (archived as patches) | Lost | Lost |
| Pushed branches (on GitHub) | Kept | Kept | Kept |
| Open PRs (on GitHub) | Kept | Kept | Kept |

## Recovering Archived Patches

When hibernate archives uncommitted changes, it saves them as git patches:

```bash
# Find archived patches
ls ~/.oat/archive/<repo>/<timestamp>/

# View a patch
cat ~/.oat/archive/<repo>/<timestamp>/<agent-name>.patch

# Apply a patch to a new worktree
cd <worktree-path>
git apply ~/.oat/archive/<repo>/<timestamp>/<agent-name>.patch
```

A `hibernate-summary.json` file in each archive directory lists which agents were archived and when.

## Example: Gaming Break

```bash
# You've been coding with OAT all morning. Time to game.
oat repo hibernate --repo my-project --all --yes
oat stop

# ... hours of gaming ...

# Back to work
oat start
# Supervisor, workspace, and merge-queue are running again.
# Any open PRs from before are still on GitHub.

# Pick up where you left off
oat worker create "Continue implementing the auth module" --repo my-project
```
