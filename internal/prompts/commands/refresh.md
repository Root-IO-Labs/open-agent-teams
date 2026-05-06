# /refresh - Sync worktree with main branch

Sync your worktree with the latest changes from the main branch.

## Instructions

1. First, determine the correct remote to use. Check if an upstream remote exists (indicates a fork):
   ```bash
   git remote | grep -q upstream && echo "upstream" || echo "origin"
   ```
   Use `upstream` if it exists (fork mode), otherwise use `origin`.

2. Fetch the latest changes from the appropriate remote:
   ```bash
   # For forks (upstream remote exists):
   git fetch upstream main

   # For non-forks (origin only):
   git fetch origin main
   ```

3. Check if there are any uncommitted changes:
   ```bash
   git status --porcelain
   ```

4. If there are uncommitted changes, stash them first:
   ```bash
   git stash push -m "refresh-stash-$(date +%s)"
   ```

5. Rebase your current branch onto main from the correct remote:
   ```bash
   # For forks (upstream remote exists):
   git rebase upstream/main

   # For non-forks (origin only):
   git rebase origin/main
   ```

6. If you stashed changes, pop them:
   ```bash
   git stash pop
   ```

7. Report the result to the user, including:
   - Which remote was used (upstream or origin)
   - How many commits were rebased
   - Whether there were any conflicts
   - Current status after refresh

If there are rebase conflicts, stop and let the user know which files have conflicts.

**Note for forks:** When working in a fork, always rebase onto `upstream/main` (the original repo) to keep your work up to date with the latest upstream changes.
