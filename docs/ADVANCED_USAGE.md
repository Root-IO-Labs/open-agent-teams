# Advanced Usage

Patterns for getting more out of OAT beyond the basics in [WORKFLOWS.md](WORKFLOWS.md).

---

## Custom Agent Definitions

OAT loads agent prompts from three sources, in priority order:

| Priority | Location | Use case |
|----------|----------|----------|
| 1 (highest) | `<repo>/.oat/agents/<agent>.md` | Team-shared customizations (checked into git) |
| 2 | `~/.oat/repos/<repo>/agents/<agent>.md` | Local-only overrides |
| 3 (lowest) | Built-in templates | Default behavior |

When both a local and repo definition exist for the same agent type, OAT **merges** them: the repo content is appended under a `## Custom Instructions` header. This preserves the base template's safety constraints while adding project-specific guidance.

### Override a built-in agent

Create a file named after the agent type you want to customize:

```bash
# Team-shared (committed to repo)
mkdir -p .oat/agents
cat > .oat/agents/worker.md << 'EOF'
Always run `make lint` before committing.
Never modify files in the `vendor/` directory.
Use conventional commit messages (feat:, fix:, chore:).
EOF
```

Valid filenames: `worker.md`, `merge-queue.md`, `review.md`, `pr-shepherd.md`.

### Spawn a fully custom agent

```bash
oat agents spawn \
  --name my-analyzer \
  --class persistent \
  --prompt-file ./prompts/analyzer.md \
  --task "Audit all SQL queries for N+1 patterns"
```

**Flags:**
- `--name` (required) — agent name
- `--class` (required) — `persistent` (long-running, detached HEAD) or `ephemeral` (gets own branch like a worker)
- `--prompt-file` (required) — path to a markdown file with the agent's full prompt
- `--task` (optional) — task description stored in state
- `--repo` (optional) — inferred from CWD if omitted

Persistent agents get a detached-HEAD worktree (read-only view of the repo). Ephemeral agents get a `work/<name>` branch and can push changes.

---

## Model Selection

Set the default model per-repo, then override per-worker when needed.

```bash
# Set repo default
oat init https://github.com/org/repo --model claude-sonnet-4-6

# Override for a specific worker
oat worker create "Complex refactor" --model claude-opus-4-6
```

**Resolution order:** worker `--model` flag > repo default set at init.

See [SUPPORTED_LLM_PROVIDERS.md](SUPPORTED_LLM_PROVIDERS.md) for the full list of supported models and providers.

### Multi-Model Routing

When multiple models are onboarded, OAT can distribute workers across them based on task complexity. The workflow:

```bash
# 1. Onboard models to generate capability profiles
oat model onboard anthropic:claude-sonnet-4-6
oat model onboard openrouter:deepseek/deepseek-v3.2:nitro
# Use --verbose to see full probe output (JSON report + YAML profile)
# oat model onboard anthropic:claude-sonnet-4-6 --verbose

# 2. Restrict which models this repo can use for workers
oat config my-repo worker-models set "anthropic:claude-sonnet-4-6,openrouter:deepseek/deepseek-v3.2:nitro"

# 3. Start agents — the workspace/supervisor sees a roster and distributes by task complexity
oat agents spawn --repo my-repo
```

Without `--allowed-worker-models` set, all eligible onboarded models are available. Setting the list restricts the repo to only the specified models. You can also manage the list incrementally:

```bash
oat config my-repo worker-models add "openai:o4-mini"     # Add a model
oat config my-repo worker-models remove "openai:o4-mini"   # Remove a model
oat config my-repo worker-models list                       # Show current list
oat config my-repo worker-models clear                      # Clear (allow all)
```

The supervisor sees a model roster table in its prompt and routes tasks by complexity: complex tasks to the highest-scoring model, simple tasks to lower-scoring models. If `--model` is specified when creating a worker, it must be in the allowed list.

---

## Iterating on Existing PRs

When a PR needs fixes (review feedback, CI failures, additional work), spawn a worker that pushes to the existing branch instead of creating a new one:

```bash
oat worker create "Address review feedback on PR #48" \
  --branch origin/work/calm-deer \
  --push-to work/calm-deer
```

- `--branch` — the starting point (typically the remote tracking branch)
- `--push-to` — the branch name to push commits to

The worker checks out a worktree from `--branch` and pushes to `--push-to`. The existing PR updates automatically. No duplicate PRs, no branch confusion.

`--push-to` requires `--branch`. Using `--branch` alone (without `--push-to`) creates a new worktree from that starting point with a fresh `work/<name>` branch.

---

## Fork Mode and the PR Shepherd

When you initialize a forked repository, OAT auto-detects the fork relationship:

```bash
oat init https://github.com/yourfork/project myproject
# Output: Detected fork of upstream-org/project
```

Detection checks for an `upstream` git remote first, then falls back to the GitHub API (`gh api repos/owner/repo`).

**What changes in fork mode:**
- The **merge queue is disabled** (you can't merge into upstream)
- The **PR shepherd** is enabled instead — it monitors PRs, handles rebasing against upstream, fixes CI, and responds to maintainer feedback
- Workers create PRs from your fork to the upstream repo

The PR shepherd can't force-merge. It keeps PRs healthy and defers to upstream maintainers for final approval. When blocked on a maintainer decision, it reports status and waits.

---

## Worker Prompt Extensions

Add project-specific context that every worker reads automatically by creating a folder at the repo root:

```
my-project/
  oat-worker-prompt-extensions/
    coding-standards.md
    api-conventions.md
    deployment-notes.md
```

Workers are instructed (via their template) to check for this folder and incorporate any files they find before starting work. OAT doesn't inject these files programmatically — workers read them as their first step, which means the instructions are interpreted in context rather than blindly prepended.

This folder is version-controlled, so the whole team shares the same worker context. Use it for:
- Project-specific coding standards
- API design conventions
- Architecture constraints ("never add dependencies to the core module")
- Context about ongoing migrations or known pitfalls

---

## Dormancy and PR Monitoring

After a worker creates a PR, it enters a **dormant state** with zero token consumption. The daemon's PR monitor loop (every 60 seconds) watches the PR and wakes the worker when action is needed.

### Wake triggers

| Trigger | What the worker is told |
|---------|------------------------|
| PR merged | "Your PR has been merged. Run `oat agent complete` now." |
| PR closed (without merge) | "Your PR was closed without merging. Investigate or complete." |
| Merge conflict | "Your PR has a merge conflict. Rebase and fix." |
| CI failure | "CI failed on your PR. Investigate and fix." |

### Dormancy cap

If a worker stays dormant longer than 15 minutes (default) and the PR is green/mergeable, the daemon force-merges it. This compensates for a slow or stuck merge queue.

Configure via environment variable:

```bash
export OAT_WORKER_DORMANCY_CAP_MINUTES=60  # extend to 60 min
```

### Core agent stuck detection

The daemon monitors merge-queue and supervisor agents for extended thinking periods by checking their output log file modification time (`~/.oat/output/<repo>/<agent>.log`). This is backend-agnostic — it works with any model provider (Claude, OpenRouter, DeepSeek, etc.), not just Claude. If an agent's output log hasn't been modified for longer than the soft timeout, the daemon sends an ESC key to cancel the thinking state, then delivers a nudge message containing:

- An explanation that the agent was interrupted
- Any messages that were delivered during the thinking period (and lost when ESC cleared the agent's pending message queue)
- A fresh state summary (open PRs + CI status for merge-queue, worker status for supervisor)

If the agent remains unresponsive past the hard timeout, the daemon restarts it entirely.

Configure via environment variables:

```bash
export OAT_CORE_AGENT_SOFT_TIMEOUT=5   # minutes before ESC + nudge (default: 5)
export OAT_CORE_AGENT_HARD_TIMEOUT=15  # minutes before restart (default: 15)
```

### Idle mode

When no workers are active in a repo, the daemon enters **idle mode** and stops sending nudges entirely. Nudges resume automatically when a new worker appears. This prevents burning tokens on repos with no active work.

---

## Stuck Worker Recovery

The daemon runs a three-tier escalation for agents that stop making progress. Nudge counts reset whenever new git activity (commits, pushes) is detected. The wake loop runs every 60s (configurable via `OAT_WAKE_INTERVAL_SECONDS`), so nudge counts are roughly equal to elapsed minutes.

| Time window | Action | Cost |
|-------------|--------|------|
| 0-9 min | Status nudges every minute | Normal token use |
| 10-15 min | Daemon alerts supervisor, who inspects logs and intervenes. Supervisor can call `oat worker reset-nudge` once to buy another round. | Supervisor tokens |
| 16-29 min | Daemon takeover: checks git state, auto-completes workers with open PRs, sends directives. **No LLM calls.** | Zero tokens |
| ~30 min | Hard removal: worker is terminated, resources freed | Zero tokens |

The key insight: recovery at the 16-minute mark uses shell commands and git state checks, not LLM conversations. This prevents recovery from compounding costs.

---

## Verification System

Workers must pass verification before creating a PR. Three options, in order of rigor:

```bash
# Option 1: Spawn an independent verification agent (most rigorous)
oat worker request-review

# Option 2: Self-verify (worker checks its own work)
oat worker verify

# Option 3: Self-verify with auto-fix for common issues
oat worker verify --fix
```

### What `oat worker verify` checks

| Check | Weight | What it catches |
|-------|--------|----------------|
| File integrity | 35% | Truncated files, duplicate code blocks, corruption |
| Test execution | 25% | Breaking changes, test failures |
| Syntax validation | 20% | Parse errors (Python, JS, Go, Shell) |
| Task alignment | 15% | Implementation doesn't match the original task |
| Input validation | 5% | Incomplete requirement processing |

Pass threshold: 70/100.

**Flags:**
- `--fix` — auto-repair common issues (removes duplicate blocks)
- `--skip-tests` — skip test execution for speed
- `--verbose` — detailed output with per-check timing
- `--json` — machine-readable output

### Independent verification agent

`oat worker request-review` auto-commits and pushes any uncommitted changes (excluding files matching sensitive patterns like `.env`, `.pem`, `.key`), then spawns a separate agent that:
1. Reads the diff between the worker's branch and main
2. Reads the original task description
3. Builds the project
4. Runs tests
5. Runs black-box tests if applicable
6. Reviews logic and scope
7. Delivers a verdict via `oat worker set-verdict` (approve/reject with reason)

The verification agent **never modifies the worker's branch**. It operates read-only and defaults to rejection — approval requires all checks to pass.

---

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `OAT_FAST_MERGE` | `true` | Daemon auto-merges green PRs via `gh pr merge --squash`. Set to `false` to disable and require merge-queue LLM review |
| `OAT_WORKER_DORMANCY_CAP_MINUTES` | `15` | Max dormancy before force-merge |
| `OAT_CORE_AGENT_SOFT_TIMEOUT` | `5` | Minutes before ESC + nudge for stuck merge-queue/supervisor |
| `OAT_CORE_AGENT_HARD_TIMEOUT` | `15` | Minutes before restarting stuck merge-queue/supervisor |
| `OAT_TEST_MODE` | (unset) | Skip real agent spawning (for tests) |

---

## Monitoring Token Usage and Cache Efficiency

OAT routes every agent turn through Anthropic's [prompt caching
middleware](https://github.com/langchain-ai/langchain-anthropic), so
the bulk of a long-running agent's input tokens are served from cache
at roughly 10% of fresh-input pricing. The middleware is wired at
three layers — the workspace, individual workers, and sub-agents —
with `unsupported_model_behavior="ignore"` so non-Anthropic models
silently no-op rather than erroring. Cache TTL defaults to Anthropic's
5-minute behavior today; longer-TTL + independent-breakpoint work is
in flight (see the OATS gameplan "caching optimization" row).

Two surfaces make this observable.

**Live:** `oat status --tokens` reads the daemon's in-memory counters
off `state.json` and prints a per-agent table with INPUT, OUTPUT,
CACHE_READ, CACHE_CREATE, HIT%, and COST_USD columns. `HIT%` is the
share of input tokens served from cache (high is good); `COST_USD`
is the dollar cost computed from the embedded
[`internal/routing/pricing.yaml`](../internal/routing/pricing.yaml)
table. A `—` in `COST_USD` means the agent's model has no entry in
`pricing.yaml` (or the agent has no `Model` set in state) — add the
model + verified prices to the YAML and rebuild to fix.

**Historical:** `oat tokens report --repo <n> [--wave N] [--format
json]` parses the `[OAT_TOKENS]` lines already emitted to each
agent's log file and groups them by wave. Use for post-hoc benchmark
breakdowns or any scripted aggregation — the live snapshot can't tell
you wave boundaries. Same `cost_usd` column (and a `totals.cost_usd`
field in the JSON output) computed from the same pricing table.

Diagnostic heuristics when HIT% drops:

- **HIT% < 50% on a long-running agent** — usually cache-preamble
  churn. Something the agent re-reads each turn (a giant tool list,
  changing timestamps in the prompt, a huge state dump) is
  invalidating the cached prefix. Check whether a recent prompt edit
  reshuffled headers.
- **Near-zero `cache_read` on a supposedly-warm agent** — either the
  model isn't Anthropic (the middleware silently disables) or the
  TTL expired between turns (agent idled > 5 min). Harmless if
  intermittent, worth investigating if persistent.
- **`cache_creation` dwarfing `cache_read`** — first turn of a new
  agent is always high; if it persists turn-to-turn, the prefix isn't
  stable and caching is buying nothing. Treat as a bug.

When Raj's caching optimization fixes land (60m TTL, independent
breakpoints for tools vs system, `PromptAssembly` layer), expect the
`HIT%` numbers to shift further upward; the diagnostic rules above
will still hold.

## Socket API

For building custom tooling on top of OAT, see [extending/SOCKET_API.md](extending/SOCKET_API.md). The daemon exposes a Unix socket at `~/.oat/daemon.sock` with 30+ JSON commands for programmatic control.

## State File

For monitoring or integrating with external dashboards, see [extending/STATE_FILE_INTEGRATION.md](extending/STATE_FILE_INTEGRATION.md). The daemon persists all state to `~/.oat/state.json`.
