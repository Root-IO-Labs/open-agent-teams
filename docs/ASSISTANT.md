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
oat assistant status [name]                                 # model / PID / state
oat assistant attach [name]                                 # alias for `oat ui --repo`
oat assistant set-model <id> [name]                         # update model (next restart)
oat assistant reset [name] [--full]                         # wipe session JSONL
oat assistant compact [name]                                # synthetic compaction
oat assistant logs [name] [--follow]                        # tail output log
oat assistant list                                          # all assistants
```

Default name is `personal` if you omit `[name]`.

## Context capacity safety net

Long-lived assistants accumulate conversation context until the LLM rejects the request. To prevent the resulting crash-loop:

- **75% capacity**: the daemon silently nudges the assistant via PTY to call `compact_conversation`. Suppressed for 5 minutes after firing.
- **95% capacity**: the daemon synthetically injects a compact-conversation directive *before* forwarding your next message. Logged at WARN.

The 95% safety net is gated by `OAT_CONTEXT_SAFETY_NET` (default `1` / on). Set to `0` to disable.

The 85% and 90% tiers (status pill, in-panel banner with Compact / Reset buttons) are planned but not yet shipped.

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
