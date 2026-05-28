# Assistant + browser-agent idle-mode audit

*Read-only audit conducted 2026-05-28 as part of [Part 8 plan
"Multi-agent chat parity + smoke-test fallout"](../uploads/multi-agent_chat_parity_d0f510b6.plan.md);
findings landed in Part 8 Commit 8.7.*

## TL;DR

**Steady-state idle token burn from daemon periodic nudges is ~0
tokens/day for chat-capable agents (`AgentTypeAssistant` +
`AgentTypeBrowser`).** Protection is two-layer:

1. **Explicit type exclusion** in `nudgeAgentsInRepo()`
   ([`internal/daemon/daemon.go`](../internal/daemon/daemon.go)).
   Browser + assistant agents short-circuit before any message is
   constructed (Part 8 Commit 8.7 added an explicit
   `if agent.Type == state.AgentTypeBrowser || ... AgentTypeAssistant
    { continue }` guard at the top of the loop; the pre-existing
   `default: continue` switch arm covered them too, but the explicit
   early-skip is grep-friendly and prevents regression if someone
   adds a new nudge case for one of those types).
2. **Repo-level `IdleMode`**. When a repo has no active workers,
   `repo.IdleMode` is set and the wake loop skips the entire repo —
   supervisor / merge-queue / PR-shepherd get no periodic nudges
   either. Always true for `_assistant-*` virtual repos (an
   assistant lives in its own virtual repo with no workers).

The defense-in-depth fix in Commit 8.7 also adds a one-time
debug-log breadcrumb per chat-capable agent per daemon process
lifetime so operators can grep `~/.oat/daemon.log` for evidence
that the guard is reached:

```
[DEBUG] chat-capable nudge skip: <repo>/<agent> (type=<browser|assistant>) excluded from wake-loop nudges (Part 8 Commit 8.7 defense-in-depth)
```

## Critical env-var naming correction

`OAT_WORKER_DORMANCY_CAP_MINUTES` (default 15) is for **PR force-
merge timing** in `pr_monitor.go`, NOT for wake-loop idle
suppression. It controls how long a worker can sit dormant on a
green PR before the daemon force-merges. The wake-loop suppression
mechanism is a completely separate gate: `repo.IdleMode +
repoHasActiveWorkers()` in `daemon.go`, with no env-var knob.

Earlier copies of `AGENTS.md` conflated the two. Commit 8.7
corrects the AGENTS.md blurb to disambiguate.

## Residual conditional risks (NOT periodic burn)

These can cost LLM turns, but only when triggered by a real event —
not on a periodic timer:

- **75% context-capacity PTY hint.** Fires only after a real LLM
  turn emits `[OAT_TOKENS]` in its output. Deduped to once per 5
  minutes per agent. Zero at idle.
- **95% context-capacity safety-net inject.** Fires only on
  `handleAgentInput` (a real user message landing on the PTY).
  Zero at idle.
- **Health-check auto-restart on agent crash.** Bounded by the
  3-failure / 10-minute bridge backoff window
  (`bridgeUnreachable`). May cost one LLM startup per restart;
  not a periodic burn.

## Carryover for Part 9 (token efficiency, not safety)

These items are tracked as carryover from Part 8 but deferred —
none affect the steady-state idle cost confirmed above:

- **Per-agent token budgets** (`OAT_AGENT_TOKEN_BUDGET_*`).
  Requires per-agent token tracking from LLM responses.
  Medium-large work.
- **Assistant restart-storm backoff parity.** Assistants don't
  currently have the 3-failure / 10-minute limit that bridges
  do; consider adding for parity.
- **75% hint user-idle gate.** Optionally skip the 75% PTY hint
  if the side-panel hasn't received user input in N minutes (the
  agent is already long-context but no user is listening — the
  hint won't change behaviour either way).

## Confirming the audit

To verify the guard is reached in your local daemon:

```bash
# Enable debug logging (default level is INFO).
OAT_LOG_LEVEL=debug oat start --foreground

# In another shell, list chat-capable agents.
oat agent list | grep -E 'browser|assistant'

# Grep daemon.log for the one-time skip breadcrumb. You should
# see exactly one entry per chat-capable agent regardless of how
# long the daemon has been running.
grep 'chat-capable nudge skip' ~/.oat/daemon.log
```

To verify the test that pins this behaviour:

```bash
go test ./internal/daemon/ -run TestNudgeAgentsInRepo_ChatCapableSkip -v
go test ./internal/daemon/ -run TestMaybeLogChatCapableNudgeSkip_PerAgentDedup -v
```
