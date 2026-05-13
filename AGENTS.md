# AGENTS.md

This file provides guidance to coding agents working with code in this repository.

## Project Overview

**OAT** (Open Agent Teams) is a lightweight orchestrator for running multiple AI coding agents on GitHub repositories. Each agent runs as its own process with an isolated git worktree, enabling parallel autonomous work on a shared codebase.

### Principles

Multiple agents work simultaneously, potentially duplicating effort or creating conflicts. CI is the quality gate: if tests pass, the code goes in. Progress is permanent.

**Core Beliefs (hardcoded, not configurable):**
- Never weaken CI: Never weaken or disable tests to make work pass; fix the code that causes the failure. Use test-driven verification (targeted tests for changed area; full regression when appropriate).
- Forward Progress > Perfection: Partial working solutions beat perfect incomplete ones
- Redundant work is acceptable: Redundant work is cheaper than blocked work
- Humans Approve, Agents Execute: Agents create PRs but do not bypass review

## Quick Reference

```bash
# Build & Install
go install ./cmd/oat       # Build + install to $GOPATH/bin
# AVOID: go build ./cmd/oat  -- drops ./oat in cwd, shadows $GOPATH/bin/oat

# CI Guard Rails (run before pushing)
make pre-commit                    # Fast checks: build + unit tests + verify docs
make check-all                     # Full CI: all checks that GitHub CI runs
make install-hooks                 # Install git pre-commit hook

# Test (run before pushing)
go test ./...                      # All tests
go test ./internal/daemon          # Single package
go test -v ./test/...              # E2E tests
go test ./internal/state -run TestSave  # Single test

# Development
go generate ./pkg/config           # Regenerate CLI docs for prompts
OAT_TEST_MODE=1 go test ./test/...  # Skip agent startup

# Key environment variables
OAT_FAST_MERGE=false               # Disable daemon auto-merge of green PRs (default: true)
OAT_WORKER_DORMANCY_CAP_MINUTES=30 # Extend worker dormancy cap (default: 15)
OAT_CORE_AGENT_SOFT_TIMEOUT=10     # Minutes before nudging stuck core agents (default: 5)
```

## Architecture Overview

```
┌─────────────────────────────────────────────────────────────────┐
│                         CLI (cmd/oat)                    │
└────────────────────────────────┬────────────────────────────────┘
                                 │ Unix Socket
┌────────────────────────────────▼────────────────────────────────┐
│                          Daemon (internal/daemon)                │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐        │
│  │ Health   │  │ Message  │  │ Wake/    │  │ Socket   │        │
│  │ Check    │  │ Router   │  │ Nudge    │  │ Server   │        │
│  │ (2min)   │  │ (2min)   │  │ (2min)   │  │          │        │
│  └──────────┘  └──────────┘  └──────────┘  └──────────┘        │
└────────────────────────────────┬────────────────────────────────┘
                                 │
    ┌────────────────────────────┼────────────────────────────────┐
    │                            │                                │
┌───▼───┐  ┌───────────┐  ┌─────▼─────┐  ┌──────────┐  ┌────────┐  ┌────────┐
│super- │  │merge-     │  │workspace  │  │worker-N  │  │review  │  │browser │
│visor  │  │queue      │  │           │  │          │  │        │  │agent   │
└───────┘  └───────────┘  └───────────┘  └──────────┘  └────────┘  └────────┘
    │           │              │              │             │           │
    └───────────┴──────────────┴──────────────┴─────────────┴───────────┘
              oat session: <repo>  (one process per agent)
```

### Package Responsibilities

| Package | Purpose | Key Types |
|---------|---------|-----------|
| `cmd/oat` | Entry point | `main()` |
| `internal/cli` | All CLI commands | `CLI`, `Command` |
| `internal/daemon` | Background process | `Daemon`, daemon loops |
| `internal/state` | Persistence | `State`, `Agent`, `Repository` |
| `internal/messages` | Inter-agent IPC | `Manager`, `Message` |
| `internal/prompts` | Agent system prompts | Embedded `*.md` files, `GetSlashCommandsPrompt()` |
| `internal/prompts/commands` | Slash command templates | `GenerateCommandsDir()`, embedded `*.md` |
| `internal/hooks` | agent hooks config | `CopyConfig()` |
| `internal/worktree` | Git worktree ops | `Manager`, `WorktreeInfo` |
| `internal/socket` | Unix socket IPC | `Server`, `Client`, `Request` |
| `internal/errors` | User-friendly errors | `CLIError`, error constructors |
| `internal/names` | Worker name generation | `Generate()` (adjective-animal) |
| `internal/templates` | Agent prompt templates | Template loading and embedding |
| `internal/templates/agent-templates/browser.md` | Browser agent prompt | MCP-based Chrome control |
| `internal/agents` | Agent management | Agent definition loading |
| `pkg/config` | Path configuration | `Paths`, `NewTestPaths()` |
| `pkg/backend` | **Public** backend abstraction | `ProcessBackend` interface |
| `pkg/agent` | **Public** agent runner | `Runner`, `Config` |

### Data Flow

1. **CLI** parses args → sends `Request` via Unix socket
2. **Daemon** handles request → updates `state.json` → manages agent processes
3. **Agents** run as daemon child processes with embedded prompts and per-agent slash commands (via `~/.oat/agent-config/`)
4. **Messages** flow via filesystem JSON files, routed by daemon
5. **Health checks** (every 2 min) attempt self-healing restoration before cleanup of dead agents

## Key Files to Understand

| File | What It Does |
|------|--------------|
| `internal/cli/cli.go` | **Large file** (~5500 lines) with all CLI commands |
| `internal/daemon/daemon.go` | Daemon implementation with all loops |
| `internal/state/state.go` | State struct with mutex-protected operations |
| `internal/prompts/*.md` | Supervisor/workspace prompts (embedded at compile) |
| `internal/templates/agent-templates/*.md` | Worker/merge-queue/reviewer/pr-shepherd prompt templates |
| `pkg/backend/direct_backend.go` | Direct PTY backend for agent process management |

## Patterns and Conventions

### Error Handling

Use structured errors from `internal/errors` for user-facing messages:

```go
// Good: User gets helpful message + suggestion
return errors.DaemonNotRunning()  // "daemon is not running" + "Try: oat start"

// Good: Wrap with context
return errors.GitOperationFailed("clone", err)

// Avoid: Raw errors lose context for users
return fmt.Errorf("clone failed: %w", err)
```

### State Mutations

Always use atomic writes for crash safety:

```go
// internal/state/state.go pattern
func (s *State) saveUnlocked() error {
    data, err := json.MarshalIndent(s, "", "  ")
    if err != nil {
        return fmt.Errorf("failed to marshal state: %w", err)
    }
    return atomicWrite(s.path, data)  // Atomic write via temp file + rename
}
```

### Agent Message Delivery

Use the backend's `SendMessage` for delivering text to agents:

```go
// Good: Atomic operation via backend
backend.SendMessage(session, agent, message)
```

### Agent Context Detection

Agents infer their context from working directory:

```go
// internal/cli/cli.go:3494
func (c *CLI) inferRepoFromCwd() (string, error) {
    // Checks if cwd is under ~/.oat/wts/<repo>/ or repos/<repo>/
}
```

## Testing

### Test Categories

| Directory | What | Requirements |
|-----------|------|--------------|
| `internal/*/` | Unit tests | None |
| `test/` | E2E integration | None |
| `test/recovery_test.go` | Crash recovery | None |

### Test Mode

```bash
# Skip actual agent startup in tests
OAT_TEST_MODE=1 go test ./test/...
```

### Writing Tests

```go
// Create isolated test environment using the helper
tmpDir, _ := os.MkdirTemp("", "oat-test-*")
paths := config.NewTestPaths(tmpDir)  // Sets up all paths correctly
defer os.RemoveAll(tmpDir)

// Use NewWithPaths for testing
cli := cli.NewWithPaths(paths)
```

## Agent System

**Worker prompt extensions:** Projects can add a folder **`oat-worker-prompt-extensions`** at the project root (repo root). Workers are instructed to read all files in that folder when present; they contain project-specific instructions and context. OAT does not create this folder—users or Overlord add it. See `docs/AGENTS.md` for details.

**Browser agent:** A persistent opt-in agent that controls Chrome through MCP tools (via the [oat-browser-agent](https://github.com/Root-IO-Labs/oat-browser-agent) Chrome extension) for web-based tasks such as scraping, form filling, and research. Opted in per-repo with `oat agent add browser-agent`; this writes `<worktree>/.oat/mcp.json` pointing at the resolved oat-browser-agent bridge (`OAT_BROWSER_AGENT_BRIDGE_PATH` env > `oat-browser-agent` on `$PATH` > `~/.oat/oat-browser-agent/dist/bridge/index.js`), which the Python agent-runtime reads on startup to load the bridge's tools as LangChain tools. The agent receives tasks via inter-agent messaging, logs actions to `~/.oat/output/<repo>/browser-agent-actions.jsonl` (canonical path; the daemon sets `OAT_BROWSER_AGENT_AUDIT_LOG_DIR` at spawn so this works without bridge-side repo awareness), and saves downloads to `~/.oat/downloads/<repo>/`. Its prompt template lives at `internal/templates/agent-templates/browser.md`.

See `docs/AGENTS.md` for detailed agent documentation including:
- Agent types and their roles
- Message routing implementation
- Prompt system and customization
- Agent lifecycle management
- Adding new agent types

**Token use / idle mode:** When a repo has no workers, the daemon stops nudging supervisor/merge-queue/PR shepherd (idle mode); `oat daemon status` and `oat status` show idle vs active by repo.

**Rejection cap:** Workers are auto-completed after repeated verification rejections (default: 3, configurable via `OAT_MAX_REJECTIONS`). The daemon escalates to the supervisor for task reassignment, preventing unbounded token waste from stuck workers.

## Extensibility

External tools can integrate via:

| Extension Point | Use Cases | Documentation |
|----------------|-----------|---------------|
| **State File** | Monitoring, analytics | [`docs/extending/STATE_FILE_INTEGRATION.md`](docs/extending/STATE_FILE_INTEGRATION.md) |
| **Socket API** | Custom CLIs, automation | [`docs/extending/SOCKET_API.md`](docs/extending/SOCKET_API.md) |

**Note:** Web UIs, event hooks, and notification systems are explicitly out of scope.

## Contributing Checklist

When modifying agent behavior:
- [ ] Update the relevant prompt (supervisor/workspace in `internal/prompts/*.md`, others in `internal/templates/agent-templates/*.md`)
- [ ] Run `go generate ./pkg/config` if CLI changed
- [ ] Test E2E: `go test ./test/...`
- [ ] Check state persistence: `go test ./internal/state/...`

When adding CLI commands:
- [ ] Add to `registerCommands()` in `internal/cli/cli.go`
- [ ] Use `internal/errors` for user-facing errors
- [ ] Add help text with `Usage` field
- [ ] Regenerate docs: `go generate ./pkg/config`

When modifying daemon loops:
- [ ] Consider interaction with health check (2 min cycle)
- [ ] Test crash recovery: `go test ./test/ -run Recovery`
- [ ] Verify state atomicity with concurrent access tests

When modifying extension points (state, socket API):
- [ ] Update relevant extension documentation in `docs/extending/`
- [ ] Update code examples in docs to match new behavior
- [ ] Note: Event hooks and web UI are not implemented (out of scope)

## Runtime Directories

```
~/.oat/
├── daemon.pid              # Daemon PID (lock file)
├── daemon.sock             # Unix socket for CLI<->daemon
├── daemon.log              # Daemon logs (rotated at 10MB)
├── state.json              # All state (repos, agents, config)
├── prompts/                # Generated prompt files for agents
├── repos/<repo>/           # Cloned repositories
├── wts/<repo>/<agent>/     # Git worktrees (one per agent)
├── messages/<repo>/<agent>/ # Message JSON files
├── output/<repo>/          # Agent output logs
│   ├── workers/            # Worker-specific logs
│   └── browser-agent-actions.jsonl  # Browser agent audit log
├── downloads/<repo>/       # Browser agent download directory
└── agent-config/<repo>/<agent>/ # Per-agent OAT config directory
    └── commands/           # Slash command files (*.md)
```

## Common Operations

### Debug a stuck agent

```bash
# Attach to see what it's doing
oat agent attach <agent-name> --read-only

# Check its messages
oat message list  # (from agent's session)

# Manually nudge via daemon logs
tail -f ~/.oat/daemon.log
```

### Repair inconsistent state

```bash
# Local repair (no daemon)
oat repair

# Daemon-side repair
oat cleanup --dry-run  # See what would be cleaned
oat cleanup            # Actually clean up
```

### Test prompt changes

```bash
# Prompts are embedded at compile time
# Supervisor/workspace prompts: internal/prompts/*.md
# Worker/merge-queue/reviewer prompts: internal/templates/agent-templates/*.md
vim internal/templates/agent-templates/worker.md
go build ./cmd/oat
# New workers will use updated prompt
```