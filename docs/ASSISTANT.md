# Personal AI Assistants

OAT can run **personal AI assistants** — persistent, conversational agents that live in your browser's side panel and stick around between tasks. Unlike the browser-agent (which is a one-shot helper a worker spawns to QA a UI it just built), an assistant is a long-lived collaborator: you start it once, chat with it whenever, and it keeps its conversation history across days.

## Quickstart

1. **Install the bridge**: follow [oat-browser-agent install instructions](https://github.com/Root-IO-Labs/oat-browser-agent#install).
2. **Install the Chrome extension**: same repo, separate one-click install.
3. **Start the assistant:**

   ```bash
   oat assistant start
   ```

   That's it. The daemon auto-starts if it wasn't running. You can also pass `--model` to pick a specific model (`oat assistant start --model anthropic:claude-opus-4-7`) and `--open-panel` to print a click-this-hint.

4. **Open the side panel**: click the oat-browser-agent extension icon in Chrome's toolbar. Start typing in the chat box at the bottom.

## When to use what

| Use case | Right tool |
|---|---|
| "Help me write code; iterate on a PR" | A worker (`oat worker create`). |
| "Run a QA pass on this preview URL; report findings" | A workflow-helper browser-agent (`oat agent add browser-agent`). |
| "Be my chat companion; remember what we discussed yesterday" | A personal assistant (`oat assistant start`). |
| "Same as above but a separate one for work vs personal" | Multiple assistants: `oat assistant start work` + `oat assistant start personal`. |

The assistant lives in a **virtual repo** (`_assistant-<name>` under `~/.oat/repos/`) — no `.git`, no worktree, no supervisor or merge-queue spawned. Virtual repos are hidden from `oat repo list` by default; use `oat repo list --all` to see them.

## Command tree

```bash
oat assistant start [name] [--model <id>] [--open-panel]   # idempotent
oat assistant stop [name]                                   # gracefully stop
oat assistant restart [name] [--fresh]                      # --fresh wipes JSONL
oat assistant status [name] [--json]                        # model / PID / state (--json for machine output)
oat assistant attach [name]                                 # alias for `oat ui --repo`
oat assistant set-model <id> [name]                         # update model (next restart)
oat assistant reset [name] [--full]                         # wipe session JSONL
oat assistant compact [name]                                # synthetic compaction
oat assistant logs [name] [--follow]                        # tail output log
oat assistant list [--json]                                 # all assistants (--json for machine output)
```

Default name is `personal` if you omit `[name]`.

## Context capacity safety net

Long-lived assistants accumulate conversation context until the LLM rejects the request. To prevent the resulting crash-loop:

- **75% capacity**: the daemon silently nudges the assistant via PTY to call `compact_conversation`. Suppressed for 5 minutes after firing.
- **95% capacity**: the daemon synthetically injects a compact-conversation directive *before* forwarding your next message. Logged at WARN.

The 95% safety net is gated by `OAT_CONTEXT_SAFETY_NET` (default `1` / on). Set to `0` to disable.

The 85% and 90% tiers (status pill, in-panel banner with Compact / Reset buttons) are planned but not yet shipped.

## Self-healing: the assistant recovers from tool errors on its own

A persistent assistant used to occasionally go silent after a browser tool call failed — you'd have to prod it ("are you stuck?") before it explained what went wrong. That's fixed by a three-layer ladder:

- **Silent retry (bridge):** transient failures on safe, read-only tools are retried automatically before the model ever sees them.
- **Auto-recovery re-prompt (daemon):** if a turn ends with *no* reply to you, the daemon may inject one bounded, code-only nudge so the assistant re-plans and keeps going. Two triggers share one budget (`OAT_ASSISTANT_RECOVERY_MAX`, default `1`, `0` to disable):
  - **Allowlisted tool error** (stale element refs, wrong/closed tab, bad arguments, a transient screenshot failure) — same as before; user-fixable problems (extension reload, security/policy blocks, emergency stop) are always surfaced instead.
  - **Incomplete silent stop** — the turn ran at least one tool successfully, then ended with no chat bubble (the model simply stopped generating). The nudge asks it to report the blocker or continue the next concrete step; if an unfinished Plan card exists, it biases toward that open item.
  - **User Stop suppresses recovery** — an interrupt latches the turn so the daemon does not inject a recovery nudge after you cancelled it.
  - **Status questions vs new work:** a pure status ping ("are you stuck?", "ping") gets a fixed diagnose-first prefix and does **not** reset the recovery budget. Status-plus-new-instructions or any real new task **does** reset the budget, then briefly answers the status ask before following the new work.
- **Graceful surface (panel):** anything not auto-recoverable shows as a short plain-language outcome so you always know how the turn ended.

The assistant is also prompted to **always finish with a plain-language outcome**, to **never end a side-panel turn silent after tools**, and to **answer status questions with a concrete cause first** (last tool / error code / "stopped after X with no error") rather than apologizing and resuming tools.

Prompt placement: chat/status/silent-turn rules live in `assistant.md` only; pin / leftover-tab rules for both assistant and browser-agent live in `_shared-browser-safety.md`.

## Watching the assistant work

- **Live plan card.** When the assistant plans with `write_todos`, its checklist renders inline in the chat as a single **Plan (done/total)** card that updates in place as items move through pending → in progress → done. It's a compact, read-only card — the full plan, not the truncated one-line tool preview — and it's persisted, so reopening the panel or switching agents replays the latest plan. (Under the hood the runtime writes an `[OAT_TODOS]` line the daemon forwards; the rendered card never re-enters the model's context.) The card sits outside collapsed "Thought for Ns" disclosures so it stays visible while the agent works.
- **Narration.** While the assistant works, the activity indicator shows what it's doing right now ("reading a file…", "planning…", "clicking…"). Specific narration is held on screen briefly even for sub-second steps so it doesn't flash past, then falls back to the generic "working…"/"still working…" during genuine idle gaps.
- **Work in my current tab.** For browser tasks, you can point the assistant at the tab you're already on instead of letting it open a new window: either tick **"Use my current tab"** in the chat header (sticky across sends, keyed per chat target) or just say so in your message ("use my current tab", "do it here", "in this tab"). While pinned, tool calls that omit or name a different already-attached tab are **hard-overridden** to your pinned tab (tab-creating tools like `browser_new_tab` are exempt). Pin intent is applied on the **executing** agent's path — under cross-agent chat the routing bridge does not keep the pin/attach. Reads/navigation on the pinned tab are allowed; consequential actions still ask for approval; `allowUserTab` is scoped to that tab only and every interaction is audited.
- **Context ring vs Restart.** Side-panel **Restart** (and `oat assistant restart --fresh`) wipe conversation memory; a fresh turn typically shows ~20–30% ring occupancy from system prompt + tool schemas + your new message alone — that baseline is expected, not a failed wipe. A high ring % after a long browse loop is **session fill** from snapshots/tool results, not leftover pre-restart memory. Existing auto-compact + the 75%/95% capacity safety net remain the mid-session pressure relief (no extra mid-stuck compact nudge).

## System status (Manage tab)

The side panel's **Manage** tab has a **System status** card showing the live health of the pieces the assistant depends on — OAT service (daemon), Bridge, Browser (extension/CDP link), and OAT CLI. If any of these goes unhealthy while you're chatting, an amber notice also appears in the chat tab with the fix action (e.g. "run `oat start`" when the daemon is down, or reload the extension when the browser link drops), so you don't have to open the Manage tab to notice.

## Coexistence with workflow-helper browser-agents

You can have an assistant chatting in the side panel *and* a workflow-helper browser-agent QA-ing a deploy preview at the same time. The extension knows about both — the side panel sticks with the chat-capable assistant; the workflow-helper drives its own Chrome window for non-chat work. See [coexistence-design.md](https://github.com/Root-IO-Labs/oat-browser-agent/blob/main/docs/coexistence-design.md) in the bridge repo for the full trust-model walkthrough.

## Troubleshooting

**"Assistant crashed on startup and stays disabled"**: the assistant's context might be too big to even `--resume`. Recover with:

```bash
oat assistant restart personal --fresh   # wipes session JSONL
# or
oat assistant compact personal           # forces compaction next turn
```

If 3 crashes happen in 10 minutes the daemon's back-off (inherited from the browser-agent) marks the agent disabled. `oat assistant status` will say so. After `--fresh` the back-off resets automatically on the next successful start.

**"`oat assistant list` shows no assistants but I'm sure I started one"**: the daemon may have died and not auto-restarted. `oat start` (or just run any `oat assistant` verb — they all auto-start the daemon). Then `oat assistant status personal` should show its state.

**"Set-model didn't take effect"**: model changes apply on next restart. `oat assistant restart personal` to apply now.

## Memory

Cross-session memory ("what I know about you" pane, `save_memory` HITL, memory inspection CLI) is the subject of a separate OAT design effort and is **not** built in here. What this version *does* ship is env-var preparation at assistant spawn time so any future memory middleware has a clean signal to opt in:

- `OAT_AGENT_TYPE=assistant`
- `OAT_REPO=<virtual-repo-name>`
- `OAT_MEMORY_ENABLED=1`

If the future memory system uses a different design, these emissions become dead weight — drop them at that point.

## Related docs

- [COMMANDS.md](COMMANDS.md) — full verb reference.
- [AGENTS.md](AGENTS.md) — assistant agent type in the broader agent system.
- [MCP.md](MCP.md) — bridge env-var contract.
