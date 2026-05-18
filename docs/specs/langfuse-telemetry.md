# Langfuse Telemetry

**Status:** v1 implemented
**Owner:** oat-agent
**Branch:** `feature/langfuse-telemetry`
**Last updated:** 2026-05-18

## Decisions made during v1 implementation

These pin down the open questions the original spec left unresolved.

| Question | Decision | Rationale |
|---|---|---|
| Zero-to-first-trace UX | Dedicated `oat telemetry setup` command — interactive prompts, persists to state.json, verifies via synchronous ping before saving | Four keystrokes from zero to first trace; failed ping leaves prior config intact |
| v1 scope | All four span types: LLM call, agent lifecycle, tool call, router decision | Single PR ships the full router-tuning dataset John needs |
| Self-hosted support | Cloud + custom host (default `https://cloud.langfuse.com`, override via config or `LANGFUSE_HOST` env) | Two lines of code; serves both free-tier and privacy-conscious users |
| Discoverability | One-time hint appended to `oat init` output: `Tip: enable Langfuse telemetry with oat telemetry setup`. `HintShown` flag in state suppresses subsequent surfacings | Visible without being naggy |
| Egress architecture | Both runtimes emit directly to Langfuse over HTTP; share trace ID via `OAT_TRACE_ID` env var | Two code paths but no inter-process plumbing; both runtimes use the same `/api/public/ingestion` endpoint and Basic auth so the wire is identical |
| Config storage | `~/.oat/state.json` under a `telemetry:` key (alongside MergeQueueConfig, ForkConfig) | Matches existing OAT pattern; no new file format introduced |
| Langfuse client | Direct HTTP (Go: stdlib `net/http`; Python: stdlib `urllib`) — neither runtime depends on the official Langfuse SDK | Avoids pulling in Pydantic v2 / heavy deps; protocols stay identical across runtimes; trivially audited |
| Span model | Fire-and-forget events; no long-lived Span objects on the Go side | Agents run for minutes/hours — keeping Span handles alive across that span is complexity without payoff. Langfuse computes duration from event timestamps |

## Context

OAT runs LLM calls, spawns agents, executes tool calls, and routes between models — but none of this is observable from outside the running process. There's no aggregate view of cost, latency, error rates, or routing decisions across sessions. This blocks two things:

1. **Router-complexity tuning** — John needs a real dataset of routing decisions + outcomes to know whether the complexity scoring is picking the right model.
2. **Agent-level debugging** — when a session goes wrong, the only artifact is the log file. There's no trace tree showing "agent A spawned agent B which made 4 LLM calls and 12 tool calls before crashing."

Langfuse (free tier) gives us hosted tracing without standing up our own observability stack. It's a 3-day commitment, not a multi-week project.

## Goal

Instrument OAT so that **every meaningful operation** emits a Langfuse trace span:

| Event | Span data |
|---|---|
| LLM call | model, input/output tokens, latency, cost, prompt category (if known), trace parent = current agent |
| Agent spawn | agent type, parent agent id, spawn reason |
| Agent finish | exit reason (success / crash / timeout / cancelled), duration, total token cost |
| Tool call | tool name, args (redacted), result success/error, latency |
| Router decision | input fingerprint, complexity score, candidate models, chosen model, reason |

A full session forms a tree rooted at the entry-point agent, with all child operations as nested spans.

## Non-goals

- Not a replacement for local logs — Langfuse is the aggregate view; structured logs remain the source of truth on a single machine.
- Not collecting user input verbatim — redaction must be on by default (see Privacy).
- Not a hard dependency — OAT must run fully offline with telemetry disabled.
- Not a real-time alerting system — we ship traces, dashboards/alerts come later.

## Approach

### Configuration

- New section in OAT config (`~/.oat/config.yaml`):
  ```yaml
  telemetry:
    enabled: false           # opt-in default
    provider: langfuse
    public_key: ""
    secret_key: ""
    host: "https://cloud.langfuse.com"
    redact_args: true        # redact tool args by default
    sample_rate: 1.0         # 0.0–1.0
  ```
- Env-var overrides: `OAT_LANGFUSE_PUBLIC_KEY`, `OAT_LANGFUSE_SECRET_KEY`, `OAT_TELEMETRY_ENABLED`.
- When disabled, all telemetry calls are no-ops with zero allocation cost.

### Wire-up

- New package `internal/telemetry/` with a single interface:
  ```go
  type Tracer interface {
      StartSession(ctx context.Context, sessionID string) (context.Context, Session)
      SpanLLM(ctx context.Context, model string, attrs LLMAttrs) Span
      SpanAgent(ctx context.Context, agentID, agentType string) Span
      SpanTool(ctx context.Context, toolName string, attrs ToolAttrs) Span
      SpanRouter(ctx context.Context, decision RouterDecision) Span
  }
  ```
- Default implementation: `LangfuseTracer` (uses the official Langfuse Go SDK if available; falls back to direct HTTP if not).
- Null implementation: `NopTracer{}` used when telemetry is disabled.
- Injection: the tracer is created once in `cmd/oat/main.go` and passed via context to all subsystems.

### Instrumentation points (call sites to add spans)

| Subsystem | File | What to wrap |
|---|---|---|
| Router | `internal/routing/model_selection.go` | `SpanRouter` around each decision |
| Agent lifecycle | `internal/agents/`, `internal/daemon/daemon.go` | `SpanAgent` on spawn, end on terminal state |
| LLM calls | wherever providers are invoked (TBD — need to map) | `SpanLLM` around each call |
| Tool execution | `internal/tui/` or wherever tools dispatch (TBD — need to map) | `SpanTool` per call |

(The "TBD — need to map" entries get resolved in the first commit by reading the merged planner PR's call structure.)

### Privacy / redaction

- Default: tool args are redacted to `<redacted:length=N,sha256=...>`.
- LLM prompts: send a hash + length + first 80 chars by default; full prompt opt-in via `telemetry.send_full_prompts: true`.
- Secrets pattern: the existing `internal/redact/` package is reused for any free-text fields.
- A test asserts that no Langfuse span ever carries a string matching common secret patterns (API keys, JWTs).

### Failure mode

- Langfuse client runs in a background goroutine with a bounded queue.
- If Langfuse is unreachable, drop spans silently after a single warning log per session.
- Telemetry **never** blocks the hot path.

## Open questions (resolved or deferred)

1. ~~Existing Go SDK~~ — **Resolved**: no official Langfuse Go SDK; we wrap the HTTP ingestion endpoint directly. Implementation is in `internal/telemetry/langfuse.go`. ~120 LOC.
2. **Cost data** — **Deferred**. v1 ships token counts but not cost. Langfuse can compute cost from `model` if its catalog matches OAT's model IDs; we'll observe the match rate before deciding whether to ship our own pricing table.
3. ~~Session ID~~ — **Resolved**: per-agent trace IDs (one Langfuse trace = one OAT agent spawn). Generated fresh in Go, propagated to Python via `OAT_TRACE_ID` env var.
4. **Router decision fingerprint** — **Resolved**: SHA-256 of the task text (first 8 bytes hex) + a length field. The hash lets you group "this user has filed similar requests" without storing content. Live in `internal/telemetry/redact.go::HashAndLen`.
5. **Local repro** — **Deferred**. Self-hosted Langfuse already works via `LANGFUSE_HOST` override. We're not shipping a `docker-compose` until someone asks.

## Validation plan

What v1 has shipped tests for:

- **Go unit tests** (`internal/telemetry/telemetry_test.go`): Nop returns Nop on disabled config + missing keys; round-trip against an in-process stub Langfuse server emits `trace-create`, `event-create` (router + agent_start + agent_end); network failure is silent.
- **Go end-to-end CLI tests** (`/tmp/test-langfuse-e2e.sh` — kept outside the repo): `oat telemetry status/setup/disable` against a stub. 28 assertions, all pass. Verifies state.json round-trips, key masking in `status` output, failed pings preserve previous config.
- **Python unit tests** (`tests/unit_tests/middleware/test_telemetry_middleware.py`): 7 tests covering env-driven disable, singleton thread-safety, end-to-end wire payload (against stub HTTP server), refused-host silent degrade, middleware swallows internal errors. 222 total middleware tests pass.

What v1 has not yet been validated against:

- A real Langfuse free-tier project — needs human-driven `oat telemetry setup` with real keys, then a planner session, then visual inspection of the trace tree in the Langfuse UI.
- Performance — v1 ships with non-blocking enqueue + background flush, but the 2% bound hasn't been measured. Will check before declaring v1 done-done.

## Out of scope for this branch

- DAG code-index integration (handled by `feature/dag-code-index`; integration point is a single `index.Observed(...)` hook the indexer calls, which becomes a `SpanTool`).
- Dashboards or saved Langfuse views (one-time setup, not code).
- Multi-tenant telemetry (one user, one Langfuse project for v1).
- Live cost computation in OAT (Langfuse computes it from token counts + model name; we'll add local cost math only if Langfuse's catalog turns out to miss too many of our model IDs).
- Crash detection that doesn't go through the existing exit paths (handleRemoveAgent / handleCompleteAgent / stopAgentOverBudget). If an agent dies without one of these being called, its trace stays open with no agent_end event. Fixing this requires hooking process-exit detection in `pkg/backend/direct_backend.go`; deferred unless we see it in practice.

## Files touched by v1

| Layer | File | Purpose |
|---|---|---|
| Telemetry core | `internal/telemetry/telemetry.go` | Tracer interface + Config |
| | `internal/telemetry/nop.go` | Disabled-default no-op tracer |
| | `internal/telemetry/langfuse.go` | HTTP client + background worker + `Ping` |
| | `internal/telemetry/redact.go` | Secret-pattern scrubbing + hashing |
| | `internal/telemetry/telemetry_test.go` | Unit tests |
| Persistence | `internal/state/state.go` | `TelemetryConfig` + `Get/Set/MarkTelemetryHintShown` |
| CLI | `internal/cli/cli.go` | `telemetry` subcommand registration + init hint |
| | `internal/cli/telemetry.go` | `setup` / `status` / `disable` handlers |
| Daemon | `internal/daemon/daemon.go` | Tracer field; init from state; per-spawn trace ID; router + agent_start emit; env propagation; `endAgentTelemetry` at three exit paths |
| Python runtime | `agent-runtime/libs/oat_sdk/oat_sdk/middleware/_langfuse_client.py` | HTTP client mirror |
| | `agent-runtime/libs/oat_sdk/oat_sdk/middleware/telemetry.py` | `LangfuseMiddleware` |
| | `agent-runtime/libs/oat_sdk/oat_sdk/graph.py` | Middleware registration |
| | `agent-runtime/libs/oat_sdk/tests/.../test_telemetry_middleware.py` | Pytest coverage |
