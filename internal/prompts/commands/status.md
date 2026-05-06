# /status - Show system status

Display the current oat system status including agent information.

## Instructions

Run the following commands and summarize the results:

1. List tracked repos and agents:
   ```bash
   oat repo list
   ```

2. Check daemon status:
   ```bash
   oat daemon status
   ```

3. Show git status of the current worktree:
   ```bash
   git status
   ```

4. Show the current branch and recent commits:
   ```bash
   git log --oneline -5
   ```

5. Check for any pending messages:
   ```bash
   oat message list
   ```

Present the results in a clear, organized format with sections for:
- Tracked repositories and agents
- Daemon status
- Tracked repositories and agents
- Current branch and git status
- Recent commits
- Pending messages (if any)
