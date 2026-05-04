# OAT - Open Agent Teams

> Build your AI dev team. Any model. Any repo.

OAT is a framework for running teams of AI coding agents that collaborate on a shared codebase. It assembles a coordinated team of AI agents — supervisor, merge queue, workers, reviewers — that plan, implement, test, and ship code while you focus on architecture and direction. Every agent gets its own process and git worktree. You coach. They deliver.

Works with Anthropic, OpenAI, Google, DeepSeek, Groq, Mistral, and [17+ LLM providers](docs/SUPPORTED_LLM_PROVIDERS.md). Not locked to any single model or vendor.

## Table of Contents

- [Getting Started](#getting-started)
- [Try the Benchmark](#try-the-benchmark)
- [How It Works](#how-it-works)
- [Dashboard (`oat ui`)](#dashboard-oat-ui)
- [What Makes OAT Different](#what-makes-oat-different)
- [Commands](#commands)
- [Built-in Agents](#built-in-agents)
- [Customize Your Team](#customize-your-team)
- [Documentation](#documentation)
- [Public Libraries](#public-libraries)
- [Building from Source](#building-from-source)
- [License](#license)

## Getting Started

### Option A: One-line install (recommended)

Pre-built binaries for macOS and Linux (x86_64 and arm64). Pulls the
latest release from GitHub, drops the binaries into `~/.local/bin/`,
and sets up the Python agent-runtime venv:

```bash
curl -fsSL https://raw.githubusercontent.com/Root-IO-Labs/open-agent-teams/main/install.sh | bash
```

Requires Python 3.11+ and [uv](https://docs.astral.sh/uv/) on the
target machine — the installer offers to install `uv` for you if it's
missing. Pin a specific release with `OAT_VERSION=v0.1.0 curl … | bash`.

After install, make sure `~/.local/bin` is on your `PATH`, then jump
to step **3. Authenticate with GitHub** below.

### Option B: Let your AI set it up

Open **Cursor**, **Claude Code**, or your preferred AI assistant and paste this prompt:

> **Copy and paste this into your AI assistant:**
>
> ```
> Clone https://github.com/Root-IO-Labs/open-agent-teams and follow
> docs/QUICKSTART.md to install and run OAT on my machine.
> ```

[QUICKSTART.md](docs/QUICKSTART.md) contains the full setup walkthrough — prerequisites, install, API keys, first run — everything an AI (or human) needs.

### Option C: Build from source

#### 1. Prerequisites

| Dependency | Minimum Version | Install |
|---|---|---|
| **Go** | 1.24.2+ | https://go.dev/dl/ |
| **Python** | 3.11+ | https://www.python.org/downloads/ |
| **uv** | latest | `curl -LsSf https://astral.sh/uv/install.sh \| sh` |
| **git** | any recent | https://git-scm.com/ |
| **gh** (GitHub CLI) | any recent | `brew install gh` / https://cli.github.com |

#### 2. Clone and install

```bash
git clone https://github.com/Root-IO-Labs/open-agent-teams.git
cd open-agent-teams
./scripts/install.sh
```

The install script builds the Go binaries (`oat`, `oat-agent`), symlinks the agent runtime, and creates the Python virtual environment — everything you need in one step.

#### 3. Authenticate with GitHub

```bash
gh auth login
gh auth status   # verify it worked
```

#### 4. Set up your LLM provider API key

OAT needs an API key for whichever LLM provider you want to use. If you're running a local model (e.g. Ollama), no key is needed.

**Recommended: OAT's built-in `.env` file** (persists across sessions, no shell config needed):

```bash
mkdir -p ~/.oat
echo 'ANTHROPIC_API_KEY=sk-ant-...' >> ~/.oat/.env
```

**Alternative: shell profile export** (OAT auto-sources `~/.zshrc` and `~/.bashrc`):

```bash
echo 'export ANTHROPIC_API_KEY=sk-ant-...' >> ~/.zshrc
```

**Per-repo override:** To use a different provider or key for a specific project, create `~/.oat/repos/<repo-name>/.env`. Per-repo keys take priority over the global `~/.oat/.env`.

<details>
<summary><strong>Common model strings</strong></summary>

| Provider | Model String | Env Var |
|----------|-------------|---------|
| Anthropic | `anthropic:claude-sonnet-4-6` | `ANTHROPIC_API_KEY` |
| OpenAI | `openai:gpt-5.2` | `OPENAI_API_KEY` |
| Google | `google_genai:gemini-3.1-pro-preview` | `GOOGLE_API_KEY` |
| DeepSeek | `deepseek:deepseek-v3.2` | `DEEPSEEK_API_KEY` |
| OpenRouter | `openrouter:deepseek/deepseek-v3.2` | `OPENROUTER_API_KEY` |
| Ollama | `ollama:llama3:70b` | *(none — local)* |

See [Supported LLM Providers](docs/SUPPORTED_LLM_PROVIDERS.md) for the full list and configuration details.

</details>

#### 5. Start OAT

```bash
oat start
oat init https://github.com/yourorg/yourrepo --model claude-sonnet-4-6
oat ui
```

That's it. You now have a supervisor, merge queue, and worker grinding away. Open `oat ui` to watch them all at once, or close the terminal — they keep working while you sleep.

## Try the Benchmark

OAT ships with a built-in benchmark: the **robotic barista** — a Python CLI project defined by a detailed spec, interface contracts, and 24 issues organized into dependency waves. No implementation code is provided; the model has to build the entire application from scratch, design its own acceptance test, and self-correct until it passes.

```bash
./benchmarks/run.sh --model anthropic:claude-sonnet-4-6 --name my-bench-run
```

This single command sets up the benchmark repo (under your authenticated GitHub account), drives OAT through all four waves, and collects results. See [benchmarks/README.md](benchmarks/README.md) for the full workflow.

## How It Works

When you initialize a repo, OAT assembles a team:

**The supervisor** coordinates everyone. It monitors workers, detects stuck agents, and nudges things forward. It never writes code — it orchestrates.

**The merge queue** watches CI. When a PR passes, it merges. When CI fails, it spawns a fixer worker. When main breaks, it enters emergency mode. For fork repos, the **PR shepherd** takes this role instead — coordinating with upstream maintainers.

**Workers** are the hands. Each one gets a task, a branch (`work/<name>`), and its own worktree. They implement, test, verify their work, open a PR, then go dormant. You can run as many in parallel as you want.

```bash
oat worker create "Add OAuth2 login with Google provider"
oat worker create "Fix flaky test in payments module" --issue 42
oat worker create "Refactor database layer" --model claude-opus-4-6
```

Each worker works independently. When done, they verify their changes (via `oat worker verify` or by requesting an independent review from a verification agent), open a PR with `oat pr create`, and enter a zero-token dormant state. The daemon monitors GitHub for CI results, merge conflicts, review comments, and merges — waking the worker only when action is needed.

If a worker gets stuck, a three-tier escalation kicks in automatically: gentle nudges, then supervisor intervention, then programmatic git-level diagnosis. Hard cap at ~30 minutes.

**Your workspace** is your persistent session. Chat with it, spawn workers, check status. It's always there when you come back.

You watch everything from `oat ui`, or close your laptop and come back to PRs.

## Dashboard (`oat ui`)

Full-screen terminal dashboard built with [Bubble Tea](https://github.com/charmbracelet/bubbletea). No tmux required.

- **Agent sidebar** — live status for every agent (active, dormant, completed)
- **Streaming output** — watch any agent's work in real time with syntax highlighting
- **Activity feed** — interleaved timeline across all agents
- **Status bar** — token usage, model, keybindings

```bash
oat ui                     # auto-detects repo
oat ui --repo my-project   # specific repo
```

See [Commands Reference](docs/COMMANDS.md#tui) for all keybindings.

## What Makes OAT Different

**Any model, any provider** — 17+ LLM providers out of the box. Set a default per-repo, override per-worker. Mix Claude for complex refactors with GPT for quick fixes. Add custom providers via `config.toml`. See [Supported Providers](docs/SUPPORTED_LLM_PROVIDERS.md).

**Built-in verification** — Workers don't just push code and hope. `oat worker verify` runs a composite quality check (file integrity, syntax, tests, task alignment). `oat worker request-review` spawns an independent verification agent that reviews the diff, runs tests, and delivers an approve/reject verdict — all before a PR is opened.

**Zero-token dormancy** — After opening a PR, workers stop burning tokens entirely. The daemon polls GitHub every 60 seconds for CI failures, merge conflicts, new comments, and merges. Workers wake only when they have something to do. Dormant workers don't count toward idle mode, so the system properly powers down when all work is waiting on CI.

**Self-healing** — Stuck workers get escalating nudges, then the supervisor investigates, then the daemon runs programmatic git checks (has work been pushed? is there a PR? are there conflicts?). Workers with merged PRs get fast-tracked to completion. The whole escalation resets if the worker shows new git activity.

**Idle mode** — When no workers are active in a repo, the daemon stops nudging the supervisor and merge queue. Zero tokens burned. When workers appear again, everything resumes automatically.

**Crash recovery** — State persists to `~/.oat/state.json`. If the daemon crashes, it reloads state on restart and reconciles with any still-running agent processes. Worktrees, messages, and sessions survive restarts.

**Extensible in markdown** — Customize any agent by creating `.oat/agents/worker.md` in your repo (team-shared) or `~/.oat/repos/<repo>/agents/worker.md` (local). Add project context for all workers via `oat-worker-prompt-extensions/` at your repo root. Spawn entirely custom agents with `oat agents spawn`.

## Commands

**Start working**

```bash
oat start                                           # start the daemon
oat init <github-url> --model <model>               # add a repo
oat ui                                              # open the dashboard
```

**Assign tasks**

```bash
oat worker create "task description"                # create a worker
oat worker create "task" --issue 42                 # tie to a GitHub issue
oat worker create "task" --model claude-opus-4-6    # specific model
oat worker create "Fix PR #48" \
  --branch origin/work/calm-deer \
  --push-to work/calm-deer                          # iterate on existing PR
oat worker list                                     # who is working?
```

**Observe and communicate**

```bash
oat attach <agent> --read-only                      # watch an agent work
oat tell <agent> "message"                          # send input to an agent
oat message send <agent> "message"                  # inter-agent message
oat status                                          # system overview
```

**Manage**

```bash
oat repo list                                       # tracked repos
oat repo use <name>                                 # set default repo
oat repo hibernate [--all]                          # pause and archive work
oat config                                          # view/modify repo config
oat worker rm <name> [--force]                      # remove a worker
```

**Maintain**

```bash
oat repair                                          # fix broken state
oat cleanup [--dry-run] [--merged]                  # clean orphaned resources
oat sync [--branch <branch>] [--repo <repo>]         # sync worktrees with remote
oat daemon status                                   # daemon health
oat daemon logs [-f]                                # daemon logs
oat stop-all [--clean]                              # stop everything
```

Run `oat --help` for the full command tree. See [Commands Reference](docs/COMMANDS.md) for details.

## Built-in Agents

| Agent | Role | Lifecycle |
|-------|------|-----------|
| **Supervisor** | Coordinates workers, detects stuck agents, reports status. Never writes code. | Persistent |
| **Merge Queue** | Merges PRs when CI passes, spawns fixers when CI fails. Emergency mode if main breaks. | Persistent |
| **PR Shepherd** | For forks: coordinates with upstream maintainers, tracks rebases. | Persistent |
| **Workspace** | Your persistent session. Spawn workers, check status, chat with your team. | Persistent |
| **Worker** | Executes a task on its own branch. Verify, PR, dormant, complete. | Ephemeral |
| **Reviewer** | Reviews PRs before merge. Posts blocking or non-blocking feedback. | Ephemeral |
| **Verification** | Independent quality gate. Reviews diff, runs tests, delivers approve/reject. | Ephemeral |

Agent definitions: [supervisor](internal/prompts/supervisor.md) | [merge-queue](internal/templates/agent-templates/merge-queue.md) | [pr-shepherd](internal/templates/agent-templates/pr-shepherd.md) | [workspace](internal/prompts/workspace.md) | [worker](internal/templates/agent-templates/worker.md) | [reviewer](internal/templates/agent-templates/reviewer.md) | [verification](internal/templates/agent-templates/verification.md)

## Customize Your Team

**Custom agent definitions** — Create `.oat/agents/worker.md` in your repo:

```markdown
Always run `make lint` before committing.
Use conventional commits (feat:, fix:, chore:).
Never modify files in vendor/.
```

**Worker prompt extensions** — Add `oat-worker-prompt-extensions/` at your repo root with coding standards, architecture constraints, or project context. Every worker reads these before starting.

**Custom agents** — Spawn persistent or ephemeral agents from any markdown prompt:

```bash
oat agents spawn --name security-auditor --class persistent --prompt-file ./auditor.md
oat agents list                # see available definitions
oat agents reset               # reset to defaults
```

## Documentation

| Doc | What it covers |
|-----|----------------|
| [Quick Start](docs/QUICKSTART.md) | Step-by-step setup guide (agent-readable) |
| [Commands Reference](docs/COMMANDS.md) | Every CLI command with examples |
| [Agent Guide](docs/AGENTS.md) | Agent types, lifecycle, communication, internals |
| [Workflows](docs/WORKFLOWS.md) | Usage patterns and real examples |
| [Advanced Usage](docs/ADVANCED_USAGE.md) | Custom agents, models, fork mode, dormancy, verification |
| [Architecture](ARCHITECTURE.md) | System design, data flows, design decisions |
| [Supported Providers](docs/SUPPORTED_LLM_PROVIDERS.md) | 17+ LLM providers, model format, custom setup |
| [Pause and Resume](docs/PAUSE_AND_RESUME.md) | Hibernate and resume workflows |
| [Crash Recovery](docs/CRASH_RECOVERY.md) | Recovery procedures |

## Public Libraries

- **[pkg/agent](pkg/agent/)** — Launch and interact with agent instances
- **[pkg/backend](pkg/backend/)** — Backend abstraction for agent process management

## Building from Source

For contributors working on OAT itself:

```bash
go build ./cmd/oat          # Build
go test ./...               # Test
go install ./cmd/oat        # Install to $GOPATH/bin
```

> **Note:** `go install ./cmd/oat` only installs the `oat` binary. For a complete working system (including `oat-agent` and the Python runtime), use `./scripts/install.sh` instead.

Requires: Go 1.24.2+, git, gh (authenticated)

## License

MIT
