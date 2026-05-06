# Pause & Resume

How to stop OAT when you need your machine for other things, and pick up where you left off later.

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
