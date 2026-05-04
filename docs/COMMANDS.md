# Commands Reference

Everything you can tell oat to do.

## Daemon

The daemon is the brain. Start it, and agents come alive.

```bash
oat start              # Wake up
oat stop               # Go to sleep
oat restart            # Stop then start (with a short delay)
oat daemon status      # You alive? (also shows which repos are idle vs active by name)
oat daemon logs -f     # What are you thinking?
oat stop-all           # Kill everything
oat stop-all --clean   # Kill everything and forget it ever happened
```

`oat start`, `oat stop`, and `oat restart` are short aliases for `oat daemon start`, `oat daemon stop`, and `oat daemon restart`.

**Status:** `oat status` (root command) shows a system overview including idle vs active per repo when the daemon is running.

## Repositories

Point oat at a repo and watch it go.

```bash
oat init <github-url>                   # Track a repo (alias for 'oat repo init')
oat init <github-url> [name]            # Track with a custom name
oat init <url> --model claude-sonnet-4-6  # Specify the LLM model
oat repo list                           # What repos do I have?
oat repo rm <name>                      # Forget about this one
oat repo use <name>                     # Set active repo context (avoid --repo everywhere)
oat repo current                        # Show active repo
oat repo unset                          # Clear active repo context
oat repo history                        # Show task history for a repo
oat repo hibernate [--repo <repo>]      # Stop workers, archive uncommitted changes
oat repo hibernate --all [--yes]        # Also stop persistent agents (supervisor, etc.)
```

## Pause & Resume

Need to step away? Hibernate your repos and come back later.

```bash
# Pause one repo (workers only)
oat repo hibernate --repo my-project

# Pause one repo (everything -- supervisor, merge-queue, workspace too)
oat repo hibernate --repo my-project --all --yes

# Pause everything
oat stop-all                    # Stops all agents + daemon, preserves state

# Resume
oat start                       # Daemon auto-restores all persistent agents

# Nuclear: forget everything
oat stop-all --clean            # Destroys worktrees and state
```

Uncommitted changes are archived as patches under `~/.oat/archive/`. Workers are not auto-restored (they're ephemeral); pushed branches and remote PRs survive. See [Pause & Resume Guide](PAUSE_AND_RESUME.md) for details.

## Workspaces

Your workspace is your home base. A persistent agent session that remembers you.

```bash
oat workspace add <name>           # New workspace
oat workspace add <name> --branch main  # New workspace from a specific branch
oat workspace list                 # Show all workspaces
oat workspace connect <name>       # Direct terminal session (prefer oat ui for monitoring)
oat workspace rm <name>            # Tear it down (warns if you have uncommitted work)
oat workspace                      # List (shorthand)
oat workspace <name>               # Connect (shorthand)
```

Workspaces use `workspace/<name>` branches. A "default" workspace spawns automatically when you init a repo.

## Workers

Workers do the grunt work. Give them a task, they make a PR.

```bash
oat worker create "task description"        # Spawn a worker
oat worker create "task" --branch feature   # Start from a specific branch
oat worker create "task" --model gpt-5.2    # Override repo model for this worker
oat worker create "Fix tests" --branch origin/work/fox --push-to work/fox  # Iterate on existing PR
oat worker list                      # Who's working?
oat worker rm <name> [--force]      # Remove worker (alias: `remove`); --force skips confirmations
```

Use `--force` to force-remove a worker without confirmations (e.g. when cleaning up a stuck worker after ensuring work is preserved). `oat work` works too. We're flexible.

The `--push-to` flag is for iterating on existing PRs. Worker pushes to that branch instead of making a new one.

## Verification

Quality gates for worker output. Run before creating PRs.

```bash
oat worker verify                    # Run verification checks
oat worker verify --fix              # Auto-fix common issues (duplicate blocks)
oat worker verify --verbose          # Detailed per-file output
oat worker verify --skip-tests       # Skip test execution
oat worker verify --json             # Machine-readable output
oat worker request-review            # Auto-commit/push, then spawn an independent verification agent
oat worker set-verdict <verdict>     # Set verification result (used by verify agents)
```

Workers must pass verification (score >= 70/100) before `oat pr create` will proceed. Three paths: independent review (most rigorous), self-verify, or force-skip (logged).

`oat worker request-review` auto-commits and pushes any uncommitted changes before spawning the verification agent. Files matching sensitive patterns (`.env`, `.pem`, `.key`, etc.) are excluded from the auto-commit with actionable guidance printed.

## Pull Requests

```bash
oat pr create                        # Create PR from worker's branch (auto-detects repo/branch)
```

`oat pr create` enforces the verification gate. If verification hasn't passed, it offers three options: request-review, self-verify, or force-skip. It also auto-detects wave labels from the worker's assigned issue (e.g., `wave:fix-0`) and applies them to the PR.

## Model Configuration

Control which LLM model agents use.

```bash
# Set repo-wide default during init
oat init <url> --model claude-sonnet-4-6

# Override for a specific worker
oat worker create "task" --model gpt-5.2
```

**Resolution order:** agent-level override > repo default > auto-detect from API keys.

Without `--model`, OAT uses the agent runtime to auto-detect a model based on available API keys
(checks OpenAI first, then Anthropic, then Google). Setting `--model` explicitly avoids
surprises when you add new API keys.

For the full list of supported providers, model format options, and custom provider setup, see [Supported LLM Providers](SUPPORTED_LLM_PROVIDERS.md).

## Observing

Watch the magic happen.

```bash
oat attach <agent-name>            # Watch an agent's output
oat attach <agent-name> --read-only # Watch without touching
```

## Messaging

Agents talk to each other. You can eavesdrop. Or join the conversation.

```bash
oat message send <to> "msg"        # Slide into their DMs
oat message list                   # What's in my inbox?
oat message read <id>              # Read a message
oat message ack <id>               # Mark it read
```

## Issues

Create structured issues with proper labeling. Primarily used by agents during convergence loops and blocker reporting.

```bash
# Create a fix issue with structured body
oat issue create --title "Fix: order validation fails" \
  --body "The order command returns exit 0 on invalid input" \
  --wave fix-0 --label wave:fix-0 \
  --file src/order.py \
  --expected "Exit code 1 with error message" \
  --actual "Exit code 0, order silently accepted"

# Create a blocker issue (auto-notifies workspace to spawn a worker)
oat issue create --blocker --wave 2 \
  --title "Blocker: spec contradicts acceptance test" \
  --body "The spec says X but the test expects Y" \
  --spec-ref "Section 3.2 of operational spec"
```

**Flags:**
- `--title` (required) — Issue title
- `--body` — Problem description
- `--label` (repeatable) — Additional labels
- `--wave <N>` — Auto-applies `wave:N` label. If omitted, auto-detected from the worker's assigned issue labels
- `--blocker` — Adds `blocker` label and sends a message to the workspace agent to spawn a worker
- `--file` (repeatable) — Relevant file paths (included in body)
- `--expected` — Expected behavior
- `--actual` — Actual behavior
- `--spec-ref` — Reference to the relevant spec section

**Behavior:**
- Auto-detects `--repo` from the agent's worktree context (same as `oat pr create`)
- Auto-creates all labels on the GitHub repo if they don't exist (idempotent via `gh label create --force`)
- If `--wave` is not passed, auto-detects the wave from the worker's currently assigned issue labels
- Issues get a structured body with Guidance (DO/DON'T lists) and optional Files to Touch sections

## Agent Commands

Commands agents run (not you, usually). But some are useful for debugging.

```bash
oat agent complete                          # Worker says "I'm done, clean me up"
oat agent complete --worker <worker-name>   # Supervisor completes a worker on its behalf
oat agent waiting                           # Worker enters dormant state (PR or verification)
oat agent restart <name>                    # Restart a stuck agent
oat agent tell <name> "message"             # Send a message to an agent
oat agent interrupt <name>                  # Send Ctrl-C to an agent
```

`oat agent waiting` marks the worker as dormant (zero token burn). When a PR exists, the daemon monitors it for CI failures, merge conflicts, new comments, merges, and closures, then wakes the worker with a targeted message when action is needed. When verification is pending (no PR yet), the daemon sets `WaitingForVerification` and returns a `dormant_verification` status. If the worker is already dormant for verification, the response includes explicit "STOP" instructions to prevent polling.

## Sync

Pull latest changes from remote and sync agent worktrees.

```bash
oat sync                             # sync all repos, default branch
oat sync --branch dev                # sync against a specific branch
oat sync --repo my-repo              # sync a specific repo only
oat sync --branch dev --repo my-repo # combine both
```

`oat refresh` is an alias for `oat sync`.

## Slash Commands

Inside agent sessions, agents get these superpowers:

- `/refresh` - Sync with main (fetch, rebase, the works)
- `/status` - What's the situation?
- `/workers` - Who else is working?
- `/messages` - Check the group chat

## Custom Agents

Roll your own agents with markdown.

```bash
oat agents list                    # What agent types exist?
oat agents reset                   # Reset to factory defaults
oat agents spawn --name <n> --class <c> --prompt-file <f>  # Birth a custom agent
```

Local definitions: `~/.oat/repos/<repo>/agents/`
Shared with team: `<repo>/.oat/agents/`

## Logs

Manage agent and daemon logs.

```bash
oat logs list                  # List available log files
oat logs search "error"        # Search across logs
oat logs clean                 # Remove old logs
```

## Debugging

Things broken? Here's how to poke around.

```bash
# Watch an agent think
oat attach <agent-name> --read-only

# Check messages
oat message list

# Daemon brain dump
tail -f ~/.oat/daemon.log

# Fix broken state
oat repair                 # Local fix
oat cleanup --dry-run      # What would we clean?
oat cleanup                # Actually clean it

# System info
oat version                # Show version
oat diagnostics            # Full system diagnostic report
oat config                 # Show/edit configuration
oat config --workspace-stuck-detection=true  # Enable workspace stuck detection for a repo
oat bug                    # Generate a bug report template

# Worker model management
oat config my-repo worker-models list              # Show allowed worker models
oat config my-repo worker-models set "sonnet,deepseek"  # Replace allowed list (CSV)
oat config my-repo worker-models add "deepseek"    # Add to allowed list
oat config my-repo worker-models remove "sonnet"   # Remove from allowed list
oat config my-repo worker-models clear             # Clear list (allow all eligible models)
oat config my-repo --allowed-worker-models "a,b"   # Shorthand for worker-models set
```

## TUI

Launch the terminal dashboard for watching all agents in real time.

```bash
oat ui                     # Launch TUI for auto-detected repo
oat ui --repo my-project   # Launch TUI for a specific repo
```

The TUI displays a multi-pane dashboard built with [Bubble Tea](https://github.com/charmbracelet/bubbletea):

- **Agent sidebar** — Lists all agents with status indicators (● alive, ○ dead, ⏳ waiting)
- **Output pane** — Live-streamed agent output with syntax highlighting
- **Activity panel** — Interleaved activity across all agents with timestamps
- **Status bar** — Token usage, model, active agent name, and keybinding hints

### Keybindings

| Key | Action |
|-----|--------|
| **Tab** | Toggle agent list sidebar |
| **↑ / ↓** | Navigate agent list |
| **Enter** | Select / switch to agent |
| **Esc** | Return to workspace view |
| **Ctrl+E** | Expand/collapse (full-width output, hide sidebar + activity) |
| **Ctrl+F** | Toggle output filter (hide progress bars, chrome, etc.) |
| **Ctrl+R** | Toggle read-only mode (enable/disable input to agent) |
| **Ctrl+X** | Send interrupt (Ctrl+C) to the active agent |
| **Ctrl+O** | Open agent's full log file in `less` |
| **Ctrl+N / Ctrl+P** | Next / previous agent (without sidebar) |
| **PgUp / PgDn** | Scroll output |
| **Home / End** | Jump to top / bottom of output |
| **Ctrl+C** | Quit TUI |

### Output Filtering

Press **Ctrl+F** to toggle output filtering. When enabled, the filter removes:
- Progress bars and spinner animations
- Terminal chrome (escape sequences, window titles)
- Redundant blank lines

This gives a cleaner view of just the substantive tool calls and agent reasoning.
