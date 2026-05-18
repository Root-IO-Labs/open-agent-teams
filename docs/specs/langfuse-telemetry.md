# Langfuse Telemetry

**Status:** Draft
**Owner:** oat-agent
**Branch:** `feature/langfuse-telemetry`
**Last updated:** 2026-05-18

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

## Open questions

1. **Existing Go SDK** — does Langfuse have an official Go SDK or do we wrap the HTTP API ourselves? (Need to check before first commit.)
2. **Cost data** — Langfuse can compute cost if we tell it the model. Do we ship a model-pricing table or rely on Langfuse's catalog?
3. **Session ID** — reuse the existing OAT session/state ID or generate fresh trace IDs?
4. **Router decision fingerprint** — how do we identify "this is the same kind of input" without leaking the input itself? Probably a hash of normalized input + a category tag.
5. **Local repro** — should we ship a `docker-compose` for self-hosted Langfuse for devs without a cloud key?

## Validation plan

- Unit tests: NopTracer no-allocations, LangfuseTracer queues spans, redaction strips secrets.
- Integration: run a full planner session with telemetry enabled against a real free-tier Langfuse project; verify trace tree shape (1 session → N agents → M LLM calls + K tool calls + R router decisions).
- Off-state: with `telemetry.enabled: false`, no network egress to Langfuse (verified with packet capture or a stubbed transport).
- Performance: end-to-end session time with telemetry on vs off must not differ by more than 2%.

## Out of scope for this branch

- DAG code-index integration (handled by `feature/dag-code-index`; integration point is a single `index.Observed(...)` hook the indexer calls, which becomes a `SpanTool`).
- Dashboards or saved Langfuse views (one-time setup, not code).
- Multi-tenant telemetry (one user, one Langfuse project for v1).
