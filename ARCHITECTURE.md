# Architecture

This document covers OAT's architecture, design decisions, and implementation details.

## Design Principles

1. **Observable** — Watch agents via `oat attach`. Poke them. Intervene if you want.
2. **Isolated** — Each agent gets its own git worktree. No stepping on toes.
3. **Recoverable** — State lives on disk. Daemon crashes? It comes back.
4. **Safe** — Agents can't weaken CI or bypass humans. That's the deal.
5. **Simple** — Files for state. PTY processes for agents. git for isolation. No magic.

## System Overview

OAT is a daemon-based orchestration system built in Go. It manages multiple OAT agents running as PTY child processes of the daemon, each with isolated git worktrees.

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              User's Machine                                  │
│                                                                              │
│  ┌──────────────┐     Unix Socket      ┌──────────────────────────────────┐ │
│  │  CLI Client  │ ◄──────────────────► │         Daemon Process           │ │
│  │              │                       │                                  │ │
│  │ oat          │                       │  ┌────────────────────────────┐ │ │
│  │   work       │                       │  │      Goroutine Pool        │ │ │
│  │   init       │                       │  │                            │ │ │
│  │   list       │                       │  │  • Server Loop             │ │ │
│  │   ...        │                       │  │  • Health Check Loop       │ │ │
│  └──────────────┘                       │  │  • Message Router Loop     │ │ │
│                                         │  │  • Wake/Nudge Loop         │ │ │
│                                         │  └────────────────────────────┘ │ │
│                                         │                                  │ │
│                                         │  ┌────────────────────────────┐ │ │
│                                         │  │     State Manager          │ │ │
│                                         │  │  (thread-safe, persisted)  │ │ │
│                                         │  └────────────────────────────┘ │ │
│                                         └──────────────────────────────────┘ │
│                                                         │                    │
│                                                         ▼                    │
│  ┌───────────────────────────────────────────────────────────────────────┐  │
│  │                        Agent Sessions (PTY)                            │  │
│  │                                                                        │  │
│  │  repo-a                                  repo-b                        │  │
│  │  ┌──────────┬──────────┬──────────┐    ┌──────────┬──────────┐        │  │
│  │  │supervisor│merge-q   │worker-1  │    │supervisor│worker-1  │        │  │
│  │  │(agent)   │(agent)   │(agent)   │    │(agent)   │(agent)   │        │  │
│  │  └────┬─────┴────┬─────┴────┬─────┘    └────┬─────┴────┬─────┘        │  │
│  │       │          │          │               │          │               │  │
│  └───────┼──────────┼──────────┼───────────────┼──────────┼───────────────┘  │
│          │          │          │               │          │                  │
│          ▼          ▼          ▼               ▼          ▼                  │
│  ┌───────────────────────────────────────────────────────────────────────┐  │
│  │                        Git Worktrees                                   │  │
│  │  ~/.oat/wts/repo-a/                 ~/.oat/wts/repo-b/                │  │
│  │    supervisor/  merge-queue/           supervisor/  worker-1/          │  │
│  │    worker-1/                                                          │  │
│  └───────────────────────────────────────────────────────────────────────┘  │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘
```

## Core Concepts

### Agents

An **agent** is an OAT agent instance with:
- A **PTY process** managed by the daemon for execution
- A **git worktree** for isolated file access
- A **role** defining its responsibilities (supervisor, worker, or merge-queue)
- A **thread ID** (UUID) for context persistence

Agents run autonomously. Humans can observe via `oat attach` or interact via `oat agent tell` at any time.

### Agent Types

**Supervisor**
- One per repository
- Monitors all other agents
- Answers status questions ("what's everyone up to?")
- Receives daemon alerts about potentially stuck workers and can intervene intelligently
- Makes high-level decisions about task coordination
- Keeps worktree synced with main branch

**Worker**
- Executes a specific task
- Creates a PR when work is complete
- Signals completion, then gets cleaned up
- Named with Docker-style names (e.g., "happy-platypus")
- Multiple workers can run concurrently

**Merge Queue**
- One per repository
- Monitors open PRs from workers
- Merges when CI passes
- Spawns fixup workers for CI failures
- Never weakens CI without human approval

### Daemon

A single daemon process coordinates everything:
- Manages state (repos, agents, messages)
- Routes messages between agents
- Periodically nudges agents (with idle mode to save tokens when no workers exist)
- Detects and auto-recovers stuck workers via escalating nudges and programmatic git checks
- Monitors agent health
- Handles CLI requests via Unix socket

## Package Structure

| Package | What It Does |
|---------|--------------|
| `cmd/oat` | Entry point. The `main()` lives here. |
| `internal/cli` | All the CLI commands. |
| `internal/daemon` | The brain. Runs the loops, manages everything. |
| `internal/state` | Persistence. `state.json` lives and breathes here. |
| `internal/messages` | How agents talk to each other. |
| `internal/prompts` | Embedded system prompts for agents. |
| `internal/worktree` | Git worktree wrangling. |
| `internal/socket` | Unix socket IPC between CLI and daemon. |
| `internal/errors` | Nice error messages for humans. |
| `internal/names` | Generates worker names (adjective-animal style). |
| `internal/tui` | Terminal dashboard (`oat ui`). Bubble Tea app with agent sidebar, output streaming, activity panel. |
| `pkg/backend` | **Public library** — backend abstraction for agent process management. |
| `pkg/agent` | **Public library** — launch and talk to OAT - Open Agent Teams. |

## Component Details

### Daemon (`internal/daemon/daemon.go`)

The daemon is the central coordinator. It runs as a background process and manages all state.

**Structure:**
```go
type Daemon struct {
    paths   *config.Paths
    state   *state.State
    backend backend.ProcessBackend
    logger  *logging.Logger
    server  *socket.Server
    pidFile *PIDFile

    ctx    context.Context
    cancel context.CancelFunc
    wg     sync.WaitGroup
}
```

**Lifecycle:**
1. `New()` — Initialize daemon, load state from disk
2. `Start()` — Claim PID file, start socket server, launch goroutines
3. `Wait()` — Block until shutdown
4. `Stop()` — Cancel context, wait for goroutines, save state, cleanup

**Goroutines:**

| Loop | Interval | Purpose |
|------|----------|---------|
| `serverLoop` | Continuous | Handle incoming socket requests |
| `healthCheckLoop` | 2 min | Verify agents are alive, cleanup dead ones |
| `messageRouterLoop` | 60 s | Deliver pending messages to agents |
| `wakeLoop` | 60 s | Nudge idle agents with status checks |
| `prMonitorLoop` | 60 s | Watch dormant workers' PRs for CI/merge events |

### State Management (`internal/state/state.go`)

State is stored in memory with automatic persistence to disk.

**Data Model:**
```go
type State struct {
    Repos map[string]*Repository
    mu    sync.RWMutex
    path  string
}

type Repository struct {
    GithubURL   string
    SessionName string
    Agents      map[string]Agent
    Model       string             // Default LLM model for all agents in this repo
}

type Agent struct {
    Type            AgentType  // supervisor, worker, merge-queue
    WorktreePath    string
    WindowName      string
    SessionID       string     // UUID for --thread-id
    PID             int
    Task            string
    CreatedAt       time.Time
    LastNudge       time.Time
    ReadyForCleanup bool
    NudgeCount      int        // Nudges since last git activity (workers only)
    LastBranchSHA   string     // Last known commit SHA on worker's branch
    Model           string     // Per-agent model override (takes precedence over repo default)
}
```

**Persistence:**
- Atomic writes using temp file + rename
- Auto-save after every state change
- Load on daemon startup with recovery

**Thread Safety:**
- `sync.RWMutex` protects all operations
- Read operations use `RLock()` for parallel reads
- Write operations use `Lock()` + auto-save
- `GetAllRepos()` returns deep copy for iteration

### Socket Communication (`internal/socket/socket.go`)

CLI and daemon communicate via Unix domain socket.

**Protocol:**
```
Request:  JSON { command: string, args: map[string]any }
Response: JSON { success: bool, data: any, error: string }
```

**Commands:**

| Command | Args | Description |
|---------|------|-------------|
| `ping` | — | Health check |
| `status` | — | Get daemon status |
| `stop` | — | Stop daemon |
| `list_repos` | — | List repositories |
| `add_repo` | name, github_url, session_name | Register repo |
| `add_agent` | repo, agent, type, worktree_path, ... | Register agent |
| `remove_agent` | repo, agent | Unregister agent |
| `list_agents` | repo | List agents in repo |
| `complete_agent` | repo, agent | Mark ready for cleanup |
| `trigger_cleanup` | — | Force cleanup run |
| `repair_state` | — | Fix state inconsistencies |

### Backend Integration (`pkg/backend/`)

All agent process operations are encapsulated in a `ProcessBackend` interface. The daemon uses the direct PTY backend, which manages agents as child processes with pseudo-terminal I/O.

**Key Operations:**
```go
CreateSession(name, startDir string) error
StopSession(name string) error
HasSession(name string) (bool, error)

StartAgent(session string, cfg AgentConfig) error
StopAgent(session, agent string) error
HasAgent(session, agent string) (bool, error)

SendMessage(session, agent, text string) error
GetAgentPID(session, agent string) (int, error)
```

**Message Delivery:**
Messages are written directly to the agent's PTY file descriptor, ensuring reliable delivery without shell interpretation issues.

### Git Worktree Management (`internal/worktree/worktree.go`)

Each agent gets an isolated working directory via git worktrees.

**Worktree Layout:**
```
~/.oat/wts/<repo>/
├── supervisor/
├── merge-queue/
├── happy-platypus/    # Worker worktree
│   └── (full repo)
└── clever-fox/        # Another worker worktree
    └── (full repo)
```

### Message System (`internal/messages/messages.go`)

Agents communicate via JSON files on the filesystem.

**Message Structure:**
```go
type Message struct {
    ID        string    `json:"id"`
    From      string    `json:"from"`
    To        string    `json:"to"`
    Timestamp time.Time `json:"timestamp"`
    Body      string    `json:"body"`
    Status    Status    `json:"status"`
    AckedAt   *time.Time `json:"acked_at"`
}
```

**Status Flow:**
```
pending → delivered → read → acked → (deleted)
```

### Prompt System (`internal/prompts/prompts.go`)

Role-specific prompts are embedded in the binary using Go's `//go:embed` and can be extended by repositories.

**Prompt Injection:**
Prompts are written to `.oat/AGENTS.md` in each agent's worktree before launch. OAT - Open Agent Teams automatically reads this file as part of its system prompt.

Repositories can customize agent behavior with files in `.oat/agents/`:
- `.oat/agents/worker.md` — Worker agent definition
- `.oat/agents/merge-queue.md` — Merge-queue agent definition
- `.oat/agents/reviewer.md` — Reviewer agent definition

### CLI (`internal/cli/cli.go`)

The CLI handles user commands and communicates with the daemon.

**Command Tree:**
```
oat
├── start                    # Start daemon (alias for daemon start)
├── stop-all [--clean]       # Stop everything
├── repo
│   ├── init <url>           # Initialize repo
│   ├── list                 # List repos
│   ├── rm <name>            # Remove repo
│   ├── use <name>           # Set default repo
│   ├── current              # Show default repo
│   ├── unset                # Clear default repo
│   └── history              # Show task history
├── worker
│   ├── create <task>        # Create worker
│   ├── list                 # List workers
│   └── rm <name> [--force]  # Remove worker
├── workspace
│   ├── add <name>           # Add workspace
│   ├── list                 # List workspaces
│   ├── connect <name>       # Connect to workspace
│   └── rm <name>            # Remove workspace
├── agent
│   ├── attach <name>        # Attach to agent output
│   ├── complete             # Signal completion
│   └── restart <name>       # Restart crashed agent
├── message
│   ├── send <to> <msg>      # Send message
│   ├── list                 # List messages
│   ├── read <id>            # Read message
│   └── ack <id>             # Acknowledge message
├── agents
│   ├── list                 # List agent definitions
│   ├── spawn                # Spawn from prompt file
│   └── reset                # Reset to defaults
├── ui                       # Terminal dashboard (TUI)
├── cleanup [--dry-run]      # Clean orphaned resources
├── repair                   # Fix state
├── review <pr-url>          # Spawn review agent
├── config                   # View/modify repo config
├── logs                     # View agent logs
├── bug                      # Generate diagnostic report
├── version                  # Show version
└── daemon
    ├── start
    ├── stop
    ├── restart        # Stop then start (with a short delay)
    ├── status
    └── logs [-f]
```

### Terminal Dashboard (`internal/tui`)

The TUI (`oat ui`) is a full-screen [Bubble Tea](https://github.com/charmbracelet/bubbletea) application that displays all agents in real time. It connects to the daemon via the same Unix socket as the CLI.

**Architecture:**
```
┌───────────────────────────────────────────────────────────────────────────┐
│                           oat ui (Bubble Tea)                             │
│                                                                           │
│  ┌──────────┐  ┌──────────────────────────────┐  ┌────────────────────┐ │
│  │ Agent    │  │       Output Viewport         │  │  Activity Panel    │ │
│  │ Sidebar  │  │  (live-streamed via socket)   │  │  (all agents,     │ │
│  │          │  │                                │  │   timestamped)    │ │
│  │ ● super  │  │  > Reading file auth.go...    │  │                    │ │
│  │ ● merge  │  │  > Running go test ./...      │  │  10:31 worker-1   │ │
│  │ ● work-1 │  │  > All 42 tests passed ✓     │  │    Edited auth.go │ │
│  │ ○ work-2 │  │                                │  │  10:31 merge-q    │ │
│  │          │  │                                │  │    Checking PR #5 │ │
│  └──────────┘  └──────────────────────────────┘  └────────────────────┘ │
│  ┌─────────────────────────────────────────────────────────────────────┐ │
│  │ Status: worker-1 (claude-sonnet-4-6) │ ↑↓:nav │ tab:list │ ^c:quit │ │
│  └─────────────────────────────────────────────────────────────────────┘ │
└───────────────────────────────────────────────────────────────────────────┘
```

**Key files:**

| File | Purpose |
|------|---------|
| `app.go` | Main Bubble Tea model, key handling, agent switching |
| `keys.go` | Keybinding definitions |
| `render.go` | ANSI-aware line rendering and wrapping |
| `filter.go` | Output filter (strips progress bars, chrome) |
| `stream.go` | Log file streaming (tails agent output logs) |
| `socket_stream.go` | Socket-based streaming (connects to daemon for live output) |
| `styles.go` | Lip Gloss styles for the UI chrome |
| `dedup.go` | Output deduplication (prevents repeated lines from nudges) |
| `activity.go` | Activity panel — aggregates tool calls across agents |

**Data flow:** The TUI connects to the daemon socket, requests the agent list and status, then tails each agent's output log file (`~/.oat/output/<repo>/`). Output is filtered, rendered with ANSI-aware line wrapping, and displayed in a scrollable viewport.

### Model Resolution

When launching an agent, the model is resolved in priority order:
1. **Agent-level override** (`Agent.Model`) — set via `oat worker create --model <model>`
2. **Repository default** (`Repository.Model`) — set via `oat init --model <model>`
3. **Auto-detect** — if neither is set, the OAT - Open Agent Teams CLI picks a model based on available API keys

The resolved model is passed as `--model <spec>` to the OAT - Open Agent Teams CLI. The spec can be a bare model
name (e.g., `claude-sonnet-4-6`) or provider-prefixed (e.g., `anthropic:claude-sonnet-4-6`).

All agent launch paths (daemon `startAgentWithConfig`, daemon `restartAgent`)
use `resolveAgentModel()` to apply this resolution.

## Data Flows

### Repository Initialization

```
User: oat init https://github.com/org/repo

CLI                          Daemon                       System
 │                              │                            │
 │──── ping ───────────────────►│                            │
 │◄─── pong ────────────────────│                            │
 │                              │                            │
 │ git clone ─────────────────────────────────────────────► │
 │                              │                            │
 │ create backend session ───────────────────────────────► │
 │ start agent process ─────────────────────────────────► │
 │                              │                            │
 │ write .oat/AGENTS.md ─────────────────────────── ►│
 │                              │                            │
 │ send initial message to agent ──────────────────────────► │
 │                              │                            │
 │──── add_repo ───────────────►│                            │
 │◄─── success ─────────────────│                            │
 │                              │──── save state ──────────► │
 │──── add_agent (supervisor) ─►│                            │
 │◄─── success ─────────────────│                            │
 │                              │──── save state ──────────► │
 │──── add_agent (merge-queue) ►│                            │
 │◄─── success ─────────────────│                            │
 │                              │──── save state ──────────► │
```

### Worker Creation

```
User: oat worker create "Add unit tests"

CLI                          Daemon                       System
 │                              │                            │
 │──── list_agents ────────────►│                            │
 │◄─── agent list ──────────────│                            │
 │                              │                            │
 │ git worktree add ──────────────────────────────────────► │
 │ start agent process ─────────────────────────────────► │
 │ write .oat/AGENTS.md ─────────────────────────── ►│
 │ send initial prompt to agent ────────────────────────► │
 │ send task message to agent ──────────────────────────► │
 │                              │                            │
 │──── add_agent (worker) ─────►│                            │
 │◄─── success ─────────────────│                            │
 │                              │──── save state ──────────► │
```

### Worker Completion

```
Worker: oat agent complete
  -- OR --
Supervisor: oat agent complete --worker <worker-name>

CLI                          Daemon                       System
 │                              │                            │
 │──── complete_agent ─────────►│                            │
 │    (+ target_agent if        │                            │
 │     --worker flag used)      │                            │
 │                              │──── guard: reject if       │
 │                              │     permanent agent type   │
 │                              │                            │
 │                              │──── mark ready_for_cleanup │
 │                              │                            │
 │                              │──── send completion msg ──►│
 │                              │     to supervisor          │
 │                              │                            │
 │◄─── success ─────────────────│                            │

[Health check loop runs]

                             Daemon                       System
                                │                            │
                                │──── stop agent process ───►│
                                │──── git worktree remove ──►│
                                │──── remove from state ────►│
                                │──── cleanup messages ─────►│
```

## The Nudge

Agents can get stuck. The daemon pokes them every 2 minutes, per repo. When a repo is in **idle mode** (no workers or review agents), the daemon stops nudging the supervisor, merge-queue, and PR shepherd until workers appear again — saving tokens.

| Agent | Nudge |
|-------|-------|
| supervisor | "Status check: Review worker progress and check merge queue." |
| merge-queue | "Status check: Review open PRs and check CI status." |
| worker | Escalating (see below) |
| workspace | **Never nudged** — that's your space |

### Worker Escalation Ladder

Workers use an escalating nudge system based on `NudgeCount` (tracked per worker, resets when new git activity is detected):

| NudgeCount | Time (~) | Action |
|------------|----------|--------|
| 1-9 | 0-9 min | Normal status check nudge |
| 10-15 | 10-15 min | Daemon alerts supervisor; worker gets directive nudge |
| 16-29 | 16-29 min | Daemon takeover: programmatic git checks with auto-complete or directive nudge |
| 30 | ~30 min | Hard cap: force-remove (auto-complete if PR exists, otherwise remove with failure reason) |

Thresholds are configurable via environment variables: `OAT_STUCK_SUPERVISOR_NUDGE` (default 10), `OAT_STUCK_DAEMON_NUDGE` (default 16), `OAT_STUCK_MAX_NUDGE` (default 30).

## Key Design Decisions

### Why direct PTY backend?

**Alternatives Considered:** tmux sessions, Custom terminal emulator, Docker containers, Screen.

**Rationale:**
- **Zero dependencies**: No external terminal multiplexer required
- **Observability**: Humans can attach to any agent via `oat attach` or interact via `oat agent tell`
- **Process control**: Daemon has direct control over agent processes as children
- **Simplicity**: Agent sessions are in-memory maps with PTY file descriptors; no external server to manage

**Trade-offs:** Attach is read-only (log tailing); interactive access requires `oat agent tell`. No built-in window switching (use separate terminals). Sessions do not survive daemon restarts (but state is recovered from disk).

### Why git worktrees?

**Alternatives Considered:** Single directory with branch switching, full repository clones per agent, shared directory with stashing.

**Rationale:**
- **True isolation**: Agents can have different files checked out simultaneously
- **No conflicts**: Branch switching in one agent doesn't affect others
- **Lightweight**: Worktrees share git objects, not full clones
- **Clean cleanup**: `git worktree remove` handles everything

**Trade-offs:** More disk usage than shared directory. Need to track worktree-to-agent mapping.

### Why filesystem for messages?

**Alternatives Considered:** SQLite database, Redis/message queue, Unix pipes, HTTP API.

**Rationale:**
- **Debuggability**: Just `cat` the files to see messages
- **Durability**: Files survive daemon restarts
- **Simplicity**: No additional dependencies
- **Inspectable**: Users can manually read/edit messages

**Trade-offs:** Polling instead of push (2-minute intervals). No guaranteed ordering across agents.

### Why single daemon?

**Alternatives Considered:** Per-repository daemons, no daemon (CLI-only), systemd services per repo.

**Rationale:**
- **Single state**: One source of truth for all repositories
- **Coordination**: Central point for cross-agent communication
- **Resource management**: Easier to track and clean up
- **Simple operations**: One process to start/stop

**Trade-offs:** Single point of failure. All repos share daemon lifecycle.

### Why JSON for state?

**Alternatives Considered:** SQLite database, multiple files per entity, Protocol buffers, YAML.

**Rationale:**
- **Human readable**: Easy to inspect with any editor
- **Simple recovery**: Can manually edit if corrupted
- **Atomic updates**: Temp file + rename pattern
- **No dependencies**: Standard library only

### Why embedded prompts?

**Alternatives Considered:** External files shipped with binary, fetch from remote URL, hardcoded strings in code.

**Rationale:**
- **Self-contained**: Binary has everything it needs
- **Version locked**: Prompts match binary version
- **Easy customization**: Repos can override with `.oat/agents/*.md`
- **Maintainable**: Prompts are real markdown files in source

## Concurrency Model

The daemon uses Go's standard concurrency primitives:

- `sync.WaitGroup` tracks all background goroutines
- `context.Context` enables graceful shutdown
- Each loop checks `ctx.Done()` before sleeping
- `sync.RWMutex` in State struct protects reads/writes

**Shutdown Sequence:**
```go
func (d *Daemon) Stop() error {
    d.cancel()           // Signal all goroutines to stop
    d.wg.Wait()          // Wait for them to finish
    d.server.Stop()      // Close socket server
    d.state.Save()       // Persist final state
    d.pidFile.Remove()   // Cleanup PID file
}
```

## Error Handling & Recovery

| Failure | Detection | Recovery |
|---------|-----------|----------|
| Agent process dies | Health check PID check | Log warning, attempt restart |
| Agent session lost | Health check session check | Attempt session restoration |
| All agents for repo lost | Health check session check | Remove all agents for repo |
| Daemon crashes | N/A | Reload state on restart |
| Worker stuck (no self-complete) | NudgeCount escalation | Alert supervisor → daemon git checks → auto-complete or force-remove |
| Orphan worktree | Cleanup loop | Remove directory |
| Orphan message dir | Cleanup loop | Remove directory |

See [docs/CRASH_RECOVERY.md](docs/CRASH_RECOVERY.md) for detailed recovery procedures.

## File System Layout

```
~/.oat/
├── daemon.pid              # Contains daemon PID
├── daemon.sock             # Unix socket (mode 0600)
├── daemon.log              # Append-only log file
├── state.json              # JSON state (atomically updated)
│
├── prompts/                # Generated prompt files
│   ├── supervisor.md
│   ├── merge-queue.md
│   └── happy-platypus.md
│
├── repos/                  # Cloned repositories
│   └── my-repo/
│       └── (git repository)
│
├── wts/                    # Git worktrees
│   └── my-repo/
│       ├── supervisor/
│       ├── merge-queue/
│       └── happy-platypus/
│
└── messages/               # Inter-agent messages
    └── my-repo/
        ├── supervisor/
        │   └── msg-abc.json
        ├── merge-queue/
        └── happy-platypus/
```

## Security Considerations

**Process Isolation:**
- Each agent runs in isolated git worktree
- No shared mutable state between agents
- Agents communicate only via filesystem messages

**Permission Model:**
- Agents run with `--auto-approve` for autonomous operation
- Agents are isolated to their worktree directories

**CI Safety:**
- Agents are instructed to never weaken CI; fix the code that causes test failures
- Run targeted tests for changed area; full regression when appropriate
- Merge queue requires human approval for CI changes
- This is a soft constraint enforced via prompts

## Golden Rules

These rules are embedded in agent prompts:

1. **If CI passes, the code can go in.** Never reduce or weaken CI without explicit human approval. Never weaken or disable tests to make work pass — fix the underlying code.

2. **Forward progress trumps all.** Any incremental progress is good. A reviewable PR is progress. The only failure is an agent that doesn't move forward at all.

## Testing Strategy

**Unit Tests:**
- Each package has `*_test.go` files
- Mock-free where possible (real git)
- `OAT_TEST_MODE=1` skips agent startup

**Integration Tests:**
- `test/e2e_test.go` tests full workflows
- Creates real backend sessions, git repos, and worktrees
- Verifies state persistence

```bash
go test ./...                    # All tests
go test ./internal/daemon -v     # Daemon tests
go test ./test -v                # E2E tests
```

## Extensibility

The core abstractions (backend, worktrees, messages) are stable. The current architecture supports:

- **New agent types**: Add to `AgentType` enum and create prompts
- **New commands**: Add to CLI command tree
- **Custom prompts**: Repository `.oat/agents/` directory
- **Hooks integration**: Via `.oat/hooks.json`

## Non-Goals

- **Web dashboard** — We use `oat attach` and `oat status` for observability. A web UI would add complexity without significant benefit for terminal-comfortable developers.
- **Remote daemon** — The daemon runs locally. Remote operation would require authentication, networking, and security infrastructure beyond scope.
- **Cross-repo coordination** — Each repository is independent.
- **Multi-user support** — Single user per daemon.
- **Automatic restart on crash** — When an agent crashes, we log it but don't auto-restart. Users can restart manually with `oat agent restart`. Auto-restart risks infinite loops.
