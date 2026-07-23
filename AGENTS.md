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
OAT_ASSISTANT_WAKEUP_MARKER_INTERVAL_MIN=10  # Minutes between wake-up markers for the same assistant (default: 10)
OAT_ASSISTANT_WAKEUP_MARKER_DISABLED=1       # Disable autonomous wake-up safeguard (dev/test only; default: off)
OAT_ASSISTANT_RECOVERY_MAX=1                  # Self-healing re-prompt budget per stuck sequence for assistants (default: 1; 0 disables; no-op under OAT_TEST_MODE)
OAT_BROWSER_AGENT_DOWNLOAD_DIR=~/.oat/downloads/<repo>  # Per-repo file sandbox for browser_file_download + browser_save_screenshot (set by the daemon at bridge spawn)
OAT_MODEL_CONTEXT_<normalized-modelID>=128000 # Runtime override for an agent's max input tokens; clamped to [1024, 16000000]
OAT_BRIDGE_TOOL_OUTPUT_CAP_FRACTION=0.20      # Bridge cap on a single tool result as fraction of effective context (default: 0.20; clamped [0.01, 1.0]; 0 disables with WARN)
OAT_BRIDGE_TOOL_OUTPUT_CAP_CHARS=32000        # Absolute char cap on a single tool result; takes precedence over fraction; clamped [4096, 8000000]
OAT_BRIDGE_BLOB_CACHE_MAX_BYTES=50000000      # Bridge blob-cache hard cap for over-cap tool results (default 50 MB; clamped [4096, 1000000000])
OAT_BRIDGE_BLOB_CACHE_TTL_MS=300000           # Blob cache entry TTL (default 5 min; clamped [1000, 86400000])
OAT_CONTEXT_RESERVE_OUTPUT=0                   # Reserve output headroom: tiers use (window-output) budget, ring uses full window (default: ON; tri-state, fail-safe ON)
OAT_DISABLE_OUTPUT_TOKEN_CAP=1                # Disable the per-call output-token cap defense-in-depth in summarization (default: cap ON)
OAT_LOOP_BREAKER_MAX=3                        # Consecutive same-tool+same-error trips before the loop-breaker stops+reports (default: 3; invalid falls back to 3)
OAT_BRIDGE_CANCEL=0                           # Propagate MCP notifications/cancelled to the bridge on interrupt so in-flight browser tool calls abort (default: ON)
```

> **Browser-agent (oat-browser-agent) env:** `OAT_SCREENSHOT_BYTE_CAP` sets the
> bridge-side base64 byte cap that drops+explains an oversized screenshot image
> block before it reaches the model (default ~4.5 MB, provider-safe). The
> extension also enforces a ≤2000px/side long-edge clamp + PNG downscale to the
> same byte budget pre-send.

**`OAT_MODEL_CONTEXT_<modelID>` env override.** Highest-precedence
context-window source consulted by the daemon: env override >
`ModelProfile` > 128K fallback. Normalization rule for the env-var
suffix: lowercase the model ID + replace `:` and `/` with `_`.
Examples:

| Model ID                       | Env var                                            |
|--------------------------------|----------------------------------------------------|
| `google_genai:gemini-2.5-flash`| `OAT_MODEL_CONTEXT_google_genai_gemini-2.5-flash`  |
| `anthropic:claude-opus-4-7`    | `OAT_MODEL_CONTEXT_anthropic_claude-opus-4-7`      |
| `openai/gpt-5-mini`            | `OAT_MODEL_CONTEXT_openai_gpt-5-mini`              |

Out-of-range values are clamped to `[1024, 16_000_000]` tokens
with a startup WARN. Non-numeric values are rejected (the daemon
falls through to profile / 128K fallback like the env var wasn't
set). The recommended setup path for any model is `oat model
onboard <modelID>`; this env override is the escape hatch for
bring-your-own-model setups (local Ollama, custom routers,
internal proxies) and unattended CI workflows where the onboard
probe is impractical.

**Bridge-side tool-result cap.** The bridge bounds every read-
tool response (`browser_get_text`, `browser_snapshot`,
`browser_extract`, `browser_find`, `browser_observe`,
`browser_console_messages`, `browser_network_requests`,
`browser_evaluate`, `browser_cookies_list`) so a single Wikipedia-
class page can't blow the assistant's entire context window in
one call. Default cap = `0.20` × effective context budget
(`OAT_BRIDGE_TOOL_OUTPUT_CAP_FRACTION`); absolute override via
`OAT_BRIDGE_TOOL_OUTPUT_CAP_CHARS`. Before the bridge has
received its first `stream_context_capacity` frame from the
daemon, a conservative `32_000`-char static default is used.
When a response gets truncated, the bridge stashes the full
content in an in-process LRU blob cache
(`OAT_BRIDGE_BLOB_CACHE_MAX_BYTES`,
`OAT_BRIDGE_BLOB_CACHE_TTL_MS`) and the agent gets a structured
`[TRUNCATED: ... blob_id=<uuid> ...]` marker pointing at the new
`browser_fetch_blob(id, range)` recovery tool. Action tools
(`browser_click`, `browser_navigate`, ...) return small metadata
and are NEVER capped or cached.

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

**Browser agent:** A persistent opt-in agent that controls Chrome through MCP tools (via the [oat-browser-agent](https://github.com/Root-IO-Labs/oat-browser-agent) Chrome extension) for web-based tasks such as scraping, form filling, and research. Opted in per-repo with `oat agent add browser-agent`; this writes `<worktree>/.oat/mcp.json` pointing at the resolved oat-browser-agent bridge (`OAT_BROWSER_AGENT_BRIDGE_PATH` env > `oat-browser-agent` on `$PATH` > `~/.oat/oat-browser-agent/dist/bridge/index.js`), which the Python agent-runtime reads on startup to load the bridge's tools as LangChain tools. The agent receives tasks via inter-agent messaging, logs actions to `~/.oat/output/<repo>/browser-agent-actions.jsonl` (canonical path; the daemon sets `OAT_BROWSER_AGENT_AUDIT_LOG_DIR` at spawn so this works without bridge-side repo awareness), and saves downloads to `~/.oat/downloads/<repo>/`. Its prompt template lives at `internal/templates/agent-templates/browser.md`. Connection model: each browser-capable agent runs its own bridge process, and the single Chrome extension now holds one WebSocket per live bridge (keyed by agent), so multiple browser-capable agents drive their own tab(s) in parallel — the broker-chosen "winner" is the extension's primary stream socket; the rest are tool-only CDP channels. Cross-agent isolation is enforced by per-agent tab ownership at the extension's tool-dispatch boundary (an agent may only drive tabs it created); the daemon needs no changes for this. See `oat-browser-agent`'s `docs/THREAT_MODEL.md` Invariant 1 / 1a. Action gating: consequential browser actions (payment/banking/email-send interactions, file downloads) pause for an explicit side-panel Approve/Deny before the bridge executes them (`actionGating`, default on); the approval is bound to the exact call by a bridge-minted token the page/model never see, with deny-default on timeout/socket-loss/panic and optional user-managed `(tool, origin)` auto-allow rules. See `oat-browser-agent`'s `docs/THREAT_MODEL.md` Attack Vector 19.

**Personal Assistant** (`AgentTypeAssistant`): A persistent conversational agent separate from the workflow-helper browser-agent. Lives in a *virtual repo* (`_assistant-<name>`; `Repository.IsVirtual=true`, no git, no supervisor/merge-queue spawned) under `~/.oat/repos/`. Spawned via `oat assistant start [name]` (default name `personal`; multiple in parallel allowed). Shares the bridge + MCP wiring with the browser-agent (`usesBrowserBridge()` returns true for both) but keeps `compact_conversation` (denied for browser) because the daemon-side context-capacity safety net at 75 % / 95 % capacity relies on it (`OAT_CONTEXT_SAFETY_NET=1` default ON; see `internal/daemon/context_capacity.go`). Its prompt template lives at `internal/templates/agent-templates/assistant.md`, concatenated after `_shared-browser-safety.md`. Full walkthrough: `docs/ASSISTANT.md`.

**Agent activity surface** (`AgentActivityFrame` → side-panel inline rows; Part 9, 2026-05-29): when a chat-capable agent (browser-agent or assistant) invokes an observable MCP tool, the bridge emits an `agent_activity` frame (`oat-browser-agent/bridge/src/agent-activity.ts`) carrying `{tool, kind: tool_start|tool_end, status, ts, paramsPreview?, resultPreview?, correlationTs?}`. The side panel renders these as compact `<details class="oat-chat-activity-row">` rows interleaved inline with the chat scrollback (`extension/src/chat-activity-renderer.ts` + persistence in `chat-history-store.ts`'s `PersistedChatTurn.kind === 'activity'` branch). Runs of `>= 3` consecutive activity rows collapse under a single `<details class="oat-chat-activity-group">` "Activity (N actions)" disclosure to keep long tool chains scannable. This is the user-facing observability layer the operator was previously missing — every browser tool call is visible in the same scrollback as the conversation, with per-call `paramsPreview` (URL for navigate, ref+text for click, etc.) and `resultPreview` (mime+dimensions for screenshots, length cue for evaluate, etc.) redacted via the same `bridge/src/data-redaction.ts` pipeline that scrubs the audit log. Renderer is `textContent`-only; activity rows never re-enter the agent's LLM context. The activity surface also carries a `turn_end` frame kind (see the self-healing turn ladder below): a once-per-turn outcome marker the panel uses to stop the spinner on a silent, tool-only turn and render the self-healing result.

**Harness liveness (primary UX):** long tool-arg generation emits early name-only `TOOL` + timer-based `[OAT_GENERATING]` heartbeats while args are incomplete (UI/observability only — never model context), including when the provider buffers the full body with no further stream chunks, so the side panel shows **last tool + elapsed** instead of a false “no activity” stall. Mid-turn side-panel Send is **interrupt-then-route** (daemon fires Ctrl-C while a turn is open, then delivers the new text; stuck wording only adds the diagnose prefix + parachute). Partial interrupted writes stay on disk with next-turn disclosure unless the user asked to delete them. Agent restart (burger **or** Manage-tab Restart) triggers bounded jittered NM registry reconnect retries; reload-extension is the fallback after budget exhaust.

**Self-healing turn ladder (backstop):** a three-layer mechanism for when a turn still ends silent after tools. **Layer 1 (bridge):** transient failures on SAFE/read-only tools are silently retried with jittered backoff before the model sees them (`oat-browser-agent/bridge/src/silent-retry.ts`; mutating tools excluded). **Layer 2 (daemon):** when a turn ends with no visible reply, the daemon may inject exactly one bounded, code-only `[OAT-system]` re-prompt (`internal/daemon/assistant_recovery.go`; shared budget `OAT_ASSISTANT_RECOVERY_MAX`, default 1). Triggers: (a) last tool errored on a curated fail-closed allowlist — tab-addressing (`TAB_CLOSED`, `NO_ACTIVE_TAB`, `DEBUGGER_ATTACH_FAILED`, …), stale/failed element interactions (`STALE_REF` / `REF_STALE`, `ELEMENT_NOT_FOUND`, `CLICK_FAILED`, `TYPE_FAILED`, …), nav/wait (`NAVIGATION_FAILED`, `WAIT_TIMEOUT`), bad args (`INVALID_PARAMS`, …), transient captures (`SCREENSHOT_FAILED`, …); security/policy/user-recoverable/generic codes are surfaced, never auto-recovered; or (b) **incomplete silent stop** — the turn ran tools successfully then ended with no chat bubble (nudge: report blocker or continue next step; unfinished todos bias the wording). If that incomplete-silent nudge is followed by more tool progress, its budget consume is **refunded once** so a later stall in the same sequence can still recover. If a prior recovery consume is followed by another silent allowlisted error (e.g. incomplete-silent → `REF_STALE`), Layer 2 may **chain one more** inject once per stuck sequence. Stuck-flavored status asks ("are you stuck?") grant a **one-shot parachute** if the budget is already spent; soft checks ("ping") do not. User Stop / mid-turn Send latches the turn so Layer 2 does not fire. Pure status pings otherwise keep the recovery budget; new work resets it. **Layer 3 (panel):** non-recoverable errors surface as a plain-language outcome note. The turn boundary is detected via the `[OAT_TURN_END]` sentinel the runtime writes to `OAT_TOOL_LOG` (`emit_turn_end`), parsed into the `turn_end` agent-activity frame carrying `{hadError, code, retryable, visibleReply, recovering}`. Separately, a **plan-stale nudge** (`internal/daemon/assistant_plan_nudge.go`) may fire once when progress tools succeed but `write_todos` was not called while unfinished Plan items remain — it does **not** share `OAT_ASSISTANT_RECOVERY_MAX`.

**Live plan / todo card** (`[OAT_TODOS]` sentinel → `todos` agent-activity frame → side-panel card): each `write_todos` call has the runtime write a dedicated `[OAT_TODOS] <json>` line to `OAT_TOOL_LOG` (`sidecar_emitter.emit_todos`) carrying the full `[{content,status,activeForm}]` plan. This rides its own sentinel — NOT the ordinary `TOOL: write_todos` block — because the parser's generic tool-arg preview is truncated to 200 bytes, which would clip any real plan. The daemon parser turns it into an `EventTodos` (bounded item count + per-field length; allocation-guarded before `json.Unmarshal`; forgeable like the other sentinels, so the card is `textContent`-only and never re-enters the model context), the tailer forwards a `todos` frame, and the panel renders a single evolving `<details class="oat-chat-todos-card">` checklist (one card per agent, updated in place, persisted under `PersistedChatTurn.kind === 'todos'`).

See `docs/AGENTS.md` for detailed agent documentation including:
- Agent types and their roles
- Message routing implementation
- Prompt system and customization
- Agent lifecycle management
- Adding new agent types

**Token use / idle mode:** When a repo has no active workers, `repo.IdleMode` is set and the wake loop skips the entire repo — supervisor / merge-queue / PR-shepherd receive no periodic nudges. `oat daemon status` and `oat status` show idle vs active by repo. Chat-capable agents (browser-agent + assistant) are excluded from wake-loop nudges entirely regardless of repo idle state, since they receive their work via inter-agent messaging and side-panel chat respectively (see `nudgeAgentsInRepo` in `internal/daemon/daemon.go` — explicit early-skip for `AgentTypeBrowser` + `AgentTypeAssistant` plus a `default: continue` belt-and-suspenders).

> **Note on env-var naming:** `OAT_WORKER_DORMANCY_CAP_MINUTES` (default 15) configures **PR force-merge timing** in `pr_monitor.go` (how long a worker can sit dormant on a green PR before the daemon force-merges it). It does NOT control wake-loop idle suppression — that gate is `repo.IdleMode + repoHasActiveWorkers()` in `daemon.go` with no env-var knob.

**Autonomous wake-up safeguard:** the daemon prepends an `[OAT-system]` "you just (re)started, wait for a fresh trigger" PTY marker on every (re)spawn of assistant agents (fresh spawn, auto-restart from health check, manual restart from the side panel, daemon-restart-driven re-adoption of an alive process). This is the deterministic, daemon-enforced source of truth for "agent doesn't act without a fresh trigger this lifetime"; the matching "Stale-intent guard" rule in `internal/templates/agent-templates/assistant.md` is defense-in-depth. Rate-limited per-(repo, agent) via `~/.oat/runtime/<repo>/<agent>/wakeup-marker.ts` (atomic write, persists across daemon restarts) so a crash-loop doesn't spam the PTY with redundant markers. Browser-agent (`AgentTypeBrowser`) is scoped out — workflow helpers are designed for single-task autonomous execution. Parallel mechanism to the oat-browser-agent's `bridge-restart-marker` (the bridge fires its own one-shot notice for bridge restarts; the wake-up marker fires for agent restarts; both firing is by design — non-conflicting messages). Implementation in `internal/daemon/wakeup_marker.go`.

**Rejection cap:** Workers are auto-completed after repeated verification rejections (default: 3, configurable via `OAT_MAX_REJECTIONS`). The daemon escalates to the supervisor for task reassignment, preventing unbounded token waste from stuck workers.

**Loop-breaker (retry-spiral backstop):** An agent-runtime middleware (`agent-runtime` `loop_breaker.py`, wired in `graph.py`) stops an agent that hits **N consecutive same-tool + same-error-code** failures with no intervening progress (e.g. `REF_STALE`×3, a repeated auth/session-expiry) and reports what it tried instead of grinding forever. The counter resets on any successful tool call or DOM/URL change so benign polling (`browser_wait_for`) doesn't trip it. Threshold `OAT_LOOP_BREAKER_MAX` (default 3). In side-panel chat mode it stops and asks the user; for task-bound agents it escalates. Reuses the `OAT_MAX_REJECTIONS` philosophy rather than a parallel mechanism.

**Model-suitability guard (advisory):** `oat agent add browser-agent` / `oat agent set-model` prints a non-fatal WARN when a Browser/Assistant model's profile is a poor fit for interactive browsing (low `shell_recovery` / very high `basic_inference_ms`), tunable via `OAT_BROWSER_MODEL_MIN_SHELL_RECOVERY` / `OAT_BROWSER_MODEL_MAX_INFERENCE_MS`. Advisory only — bring-your-own-model still works. See `docs/AGENTS.md`.

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
├── repos/_assistant-<name>/ # Virtual repo for an Assistant (no git;
│                           # one per assistant; hidden from `oat repo list`)
├── sessions/_assistant-<name>/<name>.session.jsonl     # Active session
├── sessions/_assistant-<name>/<name>.session.jsonl.1   # Rotation archive #1
├── sessions/_assistant-<name>/<name>.session.jsonl.2   # Rotation archive #2
├── sessions/_assistant-<name>/<name>.session.jsonl.3   # Rotation archive #3
│                           # (--fresh rotates instead of deleting; 50 MB /
│                           # 3-archive cap; older archives evicted
│                           # oldest-first.)
├── wts/<repo>/<agent>/     # Git worktrees (one per agent)
├── messages/<repo>/<agent>/ # Message JSON files
├── output/<repo>/          # Agent output logs
│   ├── workers/            # Worker-specific logs
│   ├── browser-agent-actions.jsonl  # Browser agent audit log
│   └── <agent>.routes.jsonl  # route_user_message audit log
│                           # (per-route entries with ts + target + byte_count
│                           # + sha256; full text NEVER persisted here -- lives
│                           # in session JSONL.)
├── downloads/<repo>/       # Browser agent download directory
└── agent-config/<repo>/<agent>/ # Per-agent OAT config directory
    └── commands/           # Slash command files (*.md)
```

**Removal reasons** (`remove_agent` socket verb):
- `(default)` — generic removal; recovery paths may attempt
  to spawn a replacement worker if the original had an open
  task.
- `user_cleanup_after_pause` —
  workspace-replacement notifier is **suppressed**: the
  user explicitly chose Delete, so no replacement worker
  is requested. Audit-logged for forensic visibility.

**Agent pausability** (`AgentType.IsPausable()`) — the
whitelist that gates `stop_agent` and `pause_web_agents`:

| AgentType | Pausable? | Pause mechanism |
|-----------|-----------|-----------------|
| `Assistant` | YES | `oat assistant stop` / `oat agent stop` / side-panel Stop |
| `Browser` | YES | `oat agent stop` / side-panel Stop |
| `Worker` | NO | `oat repo hibernate` (repo-scoped pause) |
| `Supervisor` | NO | `oat repo hibernate` |
| `MergeQueue` | NO | `oat repo hibernate` |
| `Reviewer` | NO | task-scoped; `oat agent remove` to cancel |
| `Verification` | NO | task-scoped; `oat agent remove` to cancel |
| `PRShepherd` | NO | `oat repo hibernate` |
| `Workspace` | NO | `oat repo hibernate` |

Non-pausable agents return `RPC_AGENT_TYPE_NOT_PAUSABLE`
from `stop_agent` with a pointer at `oat repo hibernate`.

`stop_agent` (pause) is distinct from **interrupt-and-redirect**
(side-panel Stop / `oat agent interrupt` — cancels the current
turn but keeps the process alive so the next message continues
the thread; propagates MCP cancellation to the bridge with
`OAT_BRIDGE_CANCEL`) and from **Emergency Stop**
(`emergency_stop_all` — hard kill until resumed). Full table:
`docs/AGENTS.md` § "Three distinct 'stop' mechanisms".

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

### Keep the daemon alive / recover from a crash (macOS)

```bash
# Auto-restart on crash + start at login (opt-in launchd supervisor)
oat daemon install-service     # oat daemon uninstall-service to remove

# Clean recovery after a bad crash / stale pid+sock / "Bridge not running"
oat daemon nuke && oat start && oat agent restart browser-agent --repo <repo>
```

`oat status` distinguishes a **dead daemon** from a **stopped/auto-disabled
browser-agent** (the daemon can be up while the agent is down). Full runbook —
including the daemon→bridge cascade and launchd setup — in
[docs/CRASH_RECOVERY.md](docs/CRASH_RECOVERY.md).

### Change which model an agent uses

```bash
# Persist the change; agent picks it up at its next natural restart.
oat agent set-model <agent-name> --model <model-id>

# Persist and restart immediately (drops in-flight context).
oat agent set-model <agent-name> --model <model-id> --restart
```

Model must already be onboarded (`oat model onboard <id>`); typos are rejected here rather than at the agent's next restart. Replaces the old hand-edit-`state.json` workflow.

### Test prompt changes

```bash
# Prompts are embedded at compile time
# Supervisor/workspace prompts: internal/prompts/*.md
# Worker/merge-queue/reviewer prompts: internal/templates/agent-templates/*.md
vim internal/templates/agent-templates/worker.md
go build ./cmd/oat
# New workers will use updated prompt
```