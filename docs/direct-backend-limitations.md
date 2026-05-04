# Known Backend Limitations

This document describes known limitations of the PTY backend. These are architectural constraints of the current implementation, not bugs.

## 1. Read-Only Attach (No Interactive Agent Access)

`oat attach <agent>` tails the agent's log file. This is **read-only** -- you can see agent output but cannot type to the agent directly.

### Root cause

The agent's PTY lives in the daemon's process memory. The CLI's `oat attach` is a separate process that cannot access the daemon's PTY file descriptor.

### Workaround

Use `oat agent tell <agent> "message"` to send messages to agents. This routes through the daemon's IPC, which does have access to the PTY.

### Proper fix

A PTY proxy protocol over the daemon's Unix socket would allow the CLI to attach interactively.

## 2. No Window Switching

Each `oat attach <agent>` is a separate process in a separate terminal. To monitor multiple agents, you need multiple terminal windows/tabs, each running a different `oat attach` command.

### Workaround

Open separate terminals for each agent you want to monitor:
```bash
# Terminal 1
oat attach supervisor --repo my-repo

# Terminal 2
oat attach default --repo my-repo

# Terminal 3
oat attach merge-queue --repo my-repo
```

Or use `oat status` and `oat history --repo my-repo` for a summary view.

## 3. Environment / Token Inheritance

The daemon starts agent processes (which inherit the daemon's environment), but the daemon may lack tokens like `GH_TOKEN` that were set in the user's shell.

### Workaround

Put tokens in `~/.oat/.env` so the daemon loads them regardless of shell:

```bash
# ~/.oat/.env
GH_TOKEN=<your-classic-pat-with-org-access>
```

The daemon's `loadEnvFiles()` reads this file and applies it to all agents.

### Fixes applied

**Fix 1 -- CLI forwards env to daemon:** The `start_repo_agents` socket command accepts a `cli_env` map. The CLI forwards critical env vars (`GH_TOKEN`, `ANTHROPIC_API_KEY`, `GH_TOKEN_ORG`, etc.) from its own environment to the daemon.

**Fix 2 -- Use user's shell:** `direct_backend.go` uses `$SHELL` (defaulting to `bash`) instead of hard-coding `bash -l -c`. On macOS where zsh is default, this correctly sources `.zshrc`.

**Fix 3 -- .env `export` prefix support:** `loadEnvFiles` strips `export ` prefix from keys, so `export GH_TOKEN=xxx` in `~/.oat/.env` works correctly.

## 4. Log Output vs Live TUI

`oat attach` shows the output of `cleanLogWriter`, which strips ANSI escape codes and deduplicates TUI redraws. This causes:

1. **No interactivity** -- elements like "click or Ctrl+E to expand" are visible in the text but non-functional.
2. **Garbled table/layout output** -- box-drawing characters appear without spatial arrangement.
3. **Thinking spinner / countdown noise** -- spinner frames and timer ticks appear as separate lines.

## 5. Limited Scrollback on Attach

`oat attach` tails the agent's log file. You only see whatever fits in your terminal window at the moment you attach.

### Workaround

Read the full log file directly:
```bash
# Full history
less ~/.oat/output/<repo>/<agent>.log

# Worker logs
less ~/.oat/output/<repo>/workers/<worker-name>.log
```

Or use `tail -n <lines>` for more context:
```bash
tail -n 500 ~/.oat/output/<repo>/default.log
```

## 6. Benchmark Compatibility

The direct backend works for benchmarks (agents run autonomously), but some benchmark scripts required updates:
- `run.sh` readiness checks were updated to use backend-agnostic `oat` CLI commands
- Agent instructions were updated to reference log files instead of terminal capture
- See `docs/known-issues.md` "Direct Backend (Tmux Decoupling) Issues" for the full list of fixes

## 7. Agent Interrupt Sends Ctrl-C, Not Escape (Fixed)

**Problem:** The TUI's "interrupt agent" keybinding (`ctrl+x`) called `SendInterrupt`, which sends Ctrl-C (0x03) to the agent's PTY. The agent's "Thinking..." state requires Escape (0x1b) to cancel gracefully. Ctrl-C could terminate the agent process entirely instead of just canceling the current API call.

**Fix applied:** Added `SendEscape` to the backend interface and both implementations. The TUI's `ctrl+x` now sends Escape via the new `escape_agent` socket command. `SendInterrupt`/`interrupt_agent` remain available for hard-kill scenarios.

## 8. No Streaming Idle Timeout (Fixed)

**Problem:** The agent's LLM API calls had no timeout. If the provider hung (observed with OpenRouter for 50+ minutes during DeepSeek V3.2 benchmarks), the agent waited indefinitely in "Thinking..." with no way to recover. This wasted tokens and blocked the worker from making progress.

**Fix applied:** Two complementary timeouts were added to `oat-agent`:

1. **Streaming idle timeout** (`OAT_STREAM_IDLE_TIMEOUT`, default 180s / 3 min): If no stream chunks arrive for this duration, the API call is aborted. Legitimate thinking sends tokens continuously; a hung connection sends zero bytes. This detects the difference.
2. **Hard request timeout** (`OAT_API_TIMEOUT`, default 1800s / 30 min): A generous safety net passed as `request_timeout` to the model. Catches edge cases where the provider doesn't support streaming or the idle timeout doesn't trigger.

## Summary

| Capability | Status |
|-----------|--------|
| Interactive attach (type to agent) | No (read-only log tail; use `oat agent tell`) |
| Window switching | No (separate terminals) |
| Token/env inheritance | Fixed: CLI forwards env to daemon; uses `$SHELL`; `.oat/.env` baseline |
| Live TUI rendering | No (garbled tables, spinner noise, no interactivity) |
| Scrollback / history on attach | No (only current screenful; use `less` on log file) |
| Agent interrupt | Fixed: `SendEscape` sends 0x1b via PTY |
| API timeout on hung providers | Fixed: streaming idle timeout (3 min) + hard timeout (30 min) |
| Agent survival after CLI exit | Yes (daemon children) |
| Benchmark support | Yes |
