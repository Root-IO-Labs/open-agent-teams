# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Daemon socket verb `restart_browser_agent`.** A purpose-built
  side-panel-driven restart path that mirrors the security model of
  `agent_input`: it accepts the bridge's own (session, agent)
  identity (no repo needed, since the bridge has no repo env), and
  refuses to restart anything other than a browser-type agent. The
  side panel's "Restart agent" overflow-menu item in the oat-browser-
  agent extension is the only intended caller. Always forces because
  a user clicking the menu has unambiguously asked for a fresh start;
  gating on `force=false` would surface a confusing "use --force"
  error to a UI that has no `--force` checkbox. Files:
  [`internal/daemon/daemon.go`](internal/daemon/daemon.go),
  [`internal/daemon/agent_input_test.go`](internal/daemon/agent_input_test.go)
  (new `TestHandleRestartBrowserAgent_Validation` covering the
  type-guard, unknown-session, unknown-agent, and missing-arg
  branches — the success branch is covered by the e2e suite).

### Fixed

- **`restartAgent` no longer makes the model run `clear` as a shell
  command.** `restartAgent` was sending the literal string `clear`
  via `backend.SendMessage` to "clear the pane buffer" (Bug 1 Option
  C from the tmux era). On the tmux backend that ran the `clear`
  shell builtin; on the PTY backend, stdin goes directly to the
  model, which dutifully invoked its `execute` tool with the
  `clear` command on every restart — surfacing in `oat ui` as the
  recurring "(Screen cleared — ready for your next task.)"
  ASSISTANT line operators reported after `oat agent restart
  --force`. The PTY backend allocates a fresh terminal for the
  new process anyway, so there is no buffer to inherit. The call
  is removed and the comment archaeology is preserved as a
  forward-looking warning for future contributors. File:
  [`internal/daemon/daemon.go`](internal/daemon/daemon.go).
- **`oat ui` "processing..." spinner could stick indefinitely.** When
  the sidecar emits `turn_start` but the corresponding `turn_end`
  event is lost (sidecar restart, transient network blip, etc.),
  `turnInFlight` stayed pinned at `true` and the indicator showed
  "processing... (1869s since last output)" while the model was
  actually idle waiting for stdin (no API call in flight, no tokens
  being burned). Added a 5-minute safety cap in `thinkingIndicator`
  — past that threshold the spinner is suppressed regardless of
  `turnInFlight`. Also made `Ctrl-X` (interrupt) clear the local
  flag as a manual recovery path: ESC is the user's natural "shut
  up, I'm not interested" gesture and now actually flips the UI
  even when the daemon-side ESC has nothing to interrupt. The
  `^x:interrupt` binding is also surfaced in the `ViewAgent` help
  bar (it was only documented in the default-view help, which is
  rarely visible). Files:
  [`internal/tui/app.go`](internal/tui/app.go).
- **Zombie agent processes on force-restart.** `oat agent restart
  --force` was logging "PID %d was still running" but then calling
  `restartAgent` without sending any kill signal first, leaving the
  prior `oat-agent` (and its python wrapper + MCP bridge children)
  alive while `StartAgent` overwrote the backend's map entry. The
  comment on the adopted-restart path (`internal/daemon/daemon.go`
  line 964: "Must kill the adopted process first — restartAgent
  calls StartAgent which would overwrite the map entry, orphaning
  the old process forever") was already warning against this exact
  failure mode; the force-restart path was missing the same step.
  Surfaced during browser-agent side-panel smoke tests where the
  side panel reproducibly displayed a "mystery bubble" from the
  previous session's agent. Now `handleRestartAgent` calls
  `d.backend.StopAgent(...)` before `restartAgent` whenever the
  prior PID is alive.
- **`StopAgent` only signaled the immediate process, not the
  process tree.** `killProcess` was calling
  `proc.cmd.Process.Signal(SIGTERM)`, which signals exactly one
  pid. The agent's child python wrapper kept running (Go binaries
  don't auto-propagate SIGTERM to children), which in turn kept
  the MCP bridge alive — the bridge's `ppid=1` orphan check never
  fired because its immediate parent was still running. Switched
  to `syscall.Kill(-pid, sig)` (process-group signal) which works
  because `pty.StartWithSize` installs `setsid`, making the agent
  its own process-group leader. New regression test
  `TestDirectBackend_StopAgentKillsProcessTree` locks in the
  whole-subtree behavior with a portable `sh → sleep` pipeline.

### Changed

- **Browser-agent prompt: `browser_screenshot` now full-page by
  default (Part 4.F.2).** Tightens
  `internal/templates/agent-templates/browser.md` to reflect the
  upstream schema flip in `oat-browser-agent` where
  `browser_screenshot.fullPage` now defaults to `true`. Three
  coordinated edits:
  - The tool-list line now explicitly says "Captures the entire
    scrollable page by default — no scrolling or window resizing
    needed."
  - A new short section, `browser_screenshot — defaults to
    full-page`, lives next to the perception cost hierarchy and
    spells out the new contract (one screenshot reads the whole
    page; `fullPage: false` is the rare opt-out).
  - Dropped the now-obsolete parenthetical in the
    `browser_show_window` paragraph that previously hand-held the
    agent toward `fullPage: true`. The replacement copy reinforces
    that screenshots work regardless of window state, so the agent
    never has a reason to call `browser_show_window` as a
    screenshot prerequisite — closes the loop on the 2026-05-20
    flight-task regression where the agent shrunk a
    fullscreen-Space window unprompted and then took a viewport-
    only screenshot that missed the substantive content.

- **Browser-agent prompt: documented `browser_emit_to_user` and
  side-panel chat (Part 2f).** Adds two pieces to
  `internal/templates/agent-templates/browser.md`:
  - A new bullet under the "Available Tools" list calling out
    `browser_emit_to_user` as a user-facing chat side-channel that
    does NOT touch the browser (so the model doesn't mistake it
    for a browser action).
  - A new "Real-Time User Chat (side panel)" section between
    "Receiving Tasks from Other Agents" and "Task Completion".
    Tells the agent:
    - User messages arrive on stdin (same channel as inter-agent
      messages, no special handling required).
    - Reply via `browser_emit_to_user(text, kind?)`, not free-form
      stdout. Stdout reaches debug-mode viewers and other agents
      but does NOT render as a chat bubble.
    - `kind: 'final'` (default) → bubble + clears activity
      indicator; `'progress'` → activity-indicator line only, keeps
      conversation history clean; `'question'` → dotted-border
      bubble; agent should then stop and wait for the user's next
      stdin message.
    - 64 KiB text cap (matches the bridge sanitizer in Part 2d).
    - The tool does NOT count toward `oat agent complete` — when
      the chat task is done, the agent still emits a final summary
      via `browser_emit_to_user(kind:'final')` AND runs
      `oat agent complete`. Prevents silent task-mode drift where
      the supervisor never sees the completion signal.
    - When the user's message is "do something on the web",
      acknowledge with `progress`, do the browser work, then
      `final` — same shape as a task from another agent.

  Sequencing: this update sits on top of Part 0a's prompt
  corrections (fetch_url framing, `oat agent complete` shell-
  command vs tool-name disambiguation). The new section is a
  sibling of the existing "Receiving Tasks from Other Agents" —
  no duplicated text. Mode-independent: assistant-mode (Part 5)
  also wants `browser_emit_to_user` for user-facing replies, so
  Part 5's prompt refactor won't need to change this section.

  Verification: `go test ./internal/templates/` passes (the
  embed validates that `browser.md` exists and is non-empty;
  there's no automated content check, but the markdown change
  is plain text inside the `//go:embed` glob — nothing about the
  edit could break the embed).

### Added

- **`stream_agent_output` socket verb + raw byte chunk broadcaster (Part 2c).**
  New long-lived streaming verb that fans out unmodified PTY byte chunks
  from a browser-agent to the oat-browser-agent bridge. The bridge uses
  this stream for two purposes simultaneously: (1) pretty-mode activity
  indicator — a heartbeat showing "the agent is doing something" derived
  from byte-flow timing alone, and (2) debug-mode terminal rendering —
  the side panel's optional power-user view that renders ANSI exactly
  as the agent's TUI emits it.

  Why a new broadcaster instead of reusing the existing `rawBroadcaster`?
  `rawBroadcaster` does ANSI stripping, dedup, and line buffering for the
  oat-attach / line-based `stream_output` consumers — all the wrong
  primitives for the side-panel debug view (which wants the ANSI back
  and doesn't want lines coalesced) and for the heartbeat use (which
  just needs to observe byte timing without paying for the line-level
  processing). The new `chunkBroadcaster` in `pkg/backend/chunk_broadcast.go`
  is a pure byte fan-out: copies its input, non-blocking sends to a
  64-slot per-subscriber channel, accumulates dropped bytes into a
  `pendingGap` counter when the channel fills, and surfaces the gap as
  a `{Gap: N, TS: t}` frame the next time the channel drains. Race-detector
  smoke test covers concurrent subscribe/cancel/write under load.

  The daemon stream handler batches chunk frames at 16 ms minimum interval
  before writing to the socket — both the bridge's pretty-mode and the
  debug-mode path get the same throttle envelope, so a chatty agent
  emitting many tiny chunks can't pin the WS connection. Frame schema:
  `{"chunk": "<base64>", "ts": "<rfc3339nano>"}` for bytes,
  `{"gap": N, "ts": "..."}` for backpressure drops, `{"done": true}`
  on agent exit. Restricted to `AgentTypeBrowser` agents to mirror the
  `agent_input` security boundary from Part 2b — siphoning raw PTY
  bytes from the supervisor or a worker through this verb is the kind
  of escalation a misconfigured or malicious bridge could attempt, so
  the daemon refuses at the edge.
  ([pkg/backend/chunk_broadcast.go](pkg/backend/chunk_broadcast.go),
  [internal/daemon/stream_handler.go](internal/daemon/stream_handler.go)
  `handleStreamAgentOutput`,
  [docs/extending/SOCKET_API.md](docs/extending/SOCKET_API.md);
  7 broadcaster unit tests in
  [pkg/backend/chunk_broadcast_test.go](pkg/backend/chunk_broadcast_test.go)
  plus 7 handler tests (19 cases with the agent-type-matrix sub-tests) in
  [internal/daemon/stream_agent_output_test.go](internal/daemon/stream_agent_output_test.go).)

- **`agent_input` socket verb + `SanitizePTYInput` helper (Part 2b).** New
  daemon socket verb that lets the oat-browser-agent bridge inject text into
  the browser-agent's PTY on behalf of a side-panel chat message. The verb
  addresses the agent by `(session, agent_name)` — matching the
  `OAT_BROWSER_AGENT_SESSION` + `OAT_BROWSER_AGENT_NAME` env vars from
  Part 2a — rather than by `(repo, agent)`, so the bridge does not need to
  reverse-resolve the repo name. Restricted to `AgentTypeBrowser` agents
  (rejected with a structured error for any other type), so a misconfigured
  or malicious bridge cannot spray text into the supervisor/worker PTY via
  this verb. Optional `interrupt: true` argument delivers a single `0x03`
  (Ctrl-C) for the side-panel's 60-second-stall interrupt button.
  ([internal/daemon/daemon.go](internal/daemon/daemon.go) `handleAgentInput`,
  [docs/extending/SOCKET_API.md](docs/extending/SOCKET_API.md).)

  All input is filtered through the new `internal/socket.SanitizePTYInput`
  helper to mitigate control-character prompt injection
  ([Dropbox 2024](https://dropbox.tech/machine-learning/prompt-injection-with-control-characters-openai-chatgpt-llm),
  [OWASP LLM cheat sheet](https://cheatsheetseries.owasp.org/cheatsheets/LLM_Prompt_Injection_Prevention_Cheat_Sheet.html)).
  The sanitizer strips C0 controls (except `\n`/`\t`), C1 controls (even when
  encoded as multi-byte UTF-8), ANSI escape sequences (CSI/OSC/single-byte),
  and bare `\r`; collapses `\r\n` to `\n`; and rejects inputs larger than
  32 KiB, invalid UTF-8, or inputs where more than 5 % of the bytes were
  *injection-class* C0 controls (counting backspace/NUL/etc. but excluding
  ESC consumed by a legitimate ANSI sequence and CR consumed by line-ending
  normalization). Interrupt mode opens a single-byte carve-out for `0x03`
  but rejects any other input shape — padding the request with extra bytes
  cannot smuggle prompt text past the C0 filter under cover of the
  interrupt flag.
  ([internal/socket/sanitize.go](internal/socket/sanitize.go),
  19 unit-test cases in [internal/socket/sanitize_test.go](internal/socket/sanitize_test.go).)

- **Browser-agent identity plumbing for side-panel chat (Part 2a).**
  `buildBrowserAgentMCPConfig` now sets `OAT_BROWSER_AGENT_SESSION` (the repo's
  session name) and `OAT_BROWSER_AGENT_NAME` (the browser-agent's window/agent
  name) in the bridge's env block, alongside the existing
  `OAT_BROWSER_AGENT_AUDIT_LOG_DIR`. The Part 2b/2c daemon socket verbs
  (`agent_input` and `agent_output_subscribe`, landing next) need these to
  scope PTY relays to the right agent. Bridges spawned outside OAT (Cursor /
  Claude Code) will see these vars absent and the side panel will surface the
  Part 4 disabled-state banner.
  ([internal/daemon/daemon.go](internal/daemon/daemon.go) `buildBrowserAgentMCPConfig`,
  [docs/MCP.md](docs/MCP.md) env-var table.)

- **`--deny-tool` CLI flag and `excluded_tools` SDK kwarg for runtime tool
  filtering.** The `oat-agent` CLI now accepts `--deny-tool NAME` (repeatable)
  to hide a named tool from the LLM. Built-in tools (`http_request`,
  `fetch_url`, `web_search`), MCP-provided tools, and the subagent `task` tool
  are all filterable. The SDK's `create_oat_agent` and the CLI's
  `create_cli_agent` accept the same set via the `excluded_tools` parameter.
  Excluding `"task"` skips `SubAgentMiddleware` entirely (no general-purpose
  subagent spawn path); excluding `"compact_conversation"` skips
  `SummarizationToolMiddleware`. The daemon
  ([internal/daemon/daemon.go](internal/daemon/daemon.go) `denyToolArgs`)
  unconditionally appends `--deny-tool task --deny-tool http_request
  --deny-tool fetch_url --deny-tool compact_conversation` to every
  `AgentTypeBrowser` argv (other agent types keep the full catalog). This
  closes the leak that caused the "iana mystery" — a browser-agent calling
  `task` to spawn a subagent that hit the CDP timeout and left the parent
  agent stuck "processing…" with no recovery. The deny list is enforced at
  every spawn site (`startAgentWithConfig`, `startRegisteredAgent`, and the
  restart path), so daemon-restart-restored browser agents inherit the same
  filter as freshly-spawned ones.

- **MCP child stderr capture.** The agent-runtime's MCP client now
  redirects each stdio MCP server's stderr to a per-server file
  under the canonical per-repo output dir
  (`~/.oat/output/<repo>/mcp-<server-name>.stderr.log`), passed as
  the SDK's `errlog` argument and registered for close on the same
  `AsyncExitStack` that owns the session.
  ([agent-runtime/libs/oat_sdk/oat_sdk/mcp_client.py](agent-runtime/libs/oat_sdk/oat_sdk/mcp_client.py)
  `_resolve_stderr_log_path`, `_open_stderr_log_for_spec`.) Without
  this capture the daemon's `OAT_TOOL_LOG` mode silently dropped
  the MCP child's stderr: the daemon defers to the Python
  `oat_cli` for conversation logging under that mode, but the
  conversation log only records LLM/tool events, not the MCP
  subprocess's startup banner or connection diagnostics. The
  immediate motivation is the `oat-browser-agent` bridge -- its
  `[OAT Bridge] BOOT_TOKEN=...` and
  `[OAT Bridge] WebSocket client connected` lines now reach a
  durable file the bench's preflight (and any operator triaging
  a hung browser-agent) can grep. Capture is opt-out via
  filesystem permission errors (we log a warning and fall back
  to the SDK's inherit-stderr default; the agent never crashes
  because stderr capture failed). Path resolution prefers
  `spec.env["OAT_BROWSER_AGENT_AUDIT_LOG_DIR"]` (set by
  `buildBrowserAgentMCPConfig` for browser-bridge) and falls back
  to `<cwd>/.oat/` for hand-authored mcp.json configs.

### Changed

- `buildBrowserAgentMCPConfig` ([internal/daemon/daemon.go](internal/daemon/daemon.go))
  no longer pins `OAT_BRIDGE_WS_PORT=19222` or
  `OAT_BRIDGE_TRUST_LOCALHOST=1` in the bridge env block. Both were
  back-compat workarounds for the pre-Native-Messaging era of the
  `oat-browser-agent` plan (Parts 8 / 9a). With the NM broker
  shipped in `oat-browser-agent` (`bridge/src/nm-broker.ts` +
  `extension/src/nm-port.ts`) the per-launch (port, token) pair
  is delivered to the extension via Native Messaging, so the
  daemon can let the bridge use its post-9b defaults: OS-assigned
  port + token-required handshake. End-user effect: two bridges
  running side-by-side (e.g. Cursor MCP + `oat agent add
  browser-agent`) no longer collide on `bind()`. They still
  contend for the single Chrome extension client (last NM push
  wins) -- that's the documented v1 limitation in plan §8a, not
  a regression.
- Updated [docs/MCP.md](docs/MCP.md) annotated example, "Where the
  daemon writes it" walk-through, env-var table, and
  troubleshooting bullet to match the dropped pins.
- Updated [`internal/templates/agent-templates/browser.md`](internal/templates/agent-templates/browser.md)
  to document the dedicated agent window pivot (visible-small
  default, platform-aware `browser_hide_window`) that the companion
  `oat-browser-agent` release ships. OAT browser agents now receive
  prompt guidance about the `isAgentTab` annotation on
  `browser_tabs` rows, the `browser_show_window` /
  `browser_hide_window` pair (including the macOS Mission Control
  Space workflow for hiding), the bridge's auto-activate-before-input
  behaviour, and the drag-out warning badge.

### Notes

- The `oat-browser-agent` bridge shipped a dedicated agent window
  pivot that fixes the systemic silent failures of `browser_click`,
  `browser_type`, and `browser_scroll` on tabs that are not the
  active tab in their window. Tabs opened by `browser_new_tab` now
  live in a separate `type: 'normal'` Chrome window managed by the
  extension, kept visible-small at top-left by default so macOS
  Chrome does not migrate web tabs out via its window-consolidation
  pass (which was triggered by the earlier minimize-at-creation
  attempts). `browser_new_tab` now defaults to `active: true`, and
  the bridge force-activates the target tab before every
  input-dispatch tool (`browser_click`, `browser_type`,
  `browser_scroll`, `browser_press_key`, `browser_hover`,
  `browser_fill`, `browser_drag`, `browser_scroll_to`) so the
  target is always the active tab when input is dispatched.
  `browser_hide_window` is platform-aware — on macOS it transitions
  to `state: 'fullscreen'` (macOS auto-places fullscreen windows in
  their own Mission Control Space), on Linux/Windows it minimizes
  normally; the result includes a `mode` field so callers know
  which path executed. `browser_show_window` brings the agent
  window to `state: 'normal', focused: true` regardless of prior
  state. `browser_tabs` rows gain an `isAgentTab` boolean so the
  agent can tell its own tabs from the user's. Drag-out detection
  surfaces a passive amber `!` badge on the extension toolbar when
  an agent-debugged tab is moved into a user window. See the
  [oat-browser-agent CHANGELOG](https://github.com/Root-IO-Labs/oat-browser-agent/blob/main/CHANGELOG.md)
  for full details. Pull a fresh `oat-browser-agent` build to pick
  these up — no other OAT-side code changes required.

- The same `oat-browser-agent` release also ships a layered set of
  security mitigations on top of the agent-window architecture.
  The headline items: input-dispatch tools (`browser_click`,
  `browser_type`, etc.) refuse to run on tabs outside the dedicated
  agent window by default, with an opt-in `allowUserTab: true`
  override that is logged as a security event. `browser_new_tab`
  enforces a per-agent-window tab cap (env-overridable via
  `OAT_BROWSER_AGENT_MAX_TABS`, default 20). Visibility transitions
  via `browser_show_window` / `browser_hide_window` are audit-
  logged as `window_shown` / `window_hidden` events, and the
  toolbar badge picks up a hide indicator so the user keeps a
  visual signal of agent activity even while the agent window is
  in its own Space. Three additional tools (`browser_tabs`,
  `browser_navigate`, `browser_file_download`) are now wrapped in
  the existing `[UNTRUSTED-<nonce>:…]` envelope because their
  responses carry page- or server-controlled strings, and the
  `browser_screenshot` MCP response now leads with a text warning
  block before the image bytes to flag instruction-shaped text
  rendered inside the screenshot. Operators in sensitive contexts
  should re-read the `oat-browser-agent` [`docs/THREAT_MODEL.md`](https://github.com/Root-IO-Labs/oat-browser-agent/blob/main/docs/THREAT_MODEL.md);
  the expanded sections cover the agent-window architecture, the
  hide/show audit events, `chrome.debugger` residual risks, and
  the deferred research avenues for screenshot prompt injection
  that we explicitly do NOT ship today. No OAT-side code changes
  required for any of this — pulling a fresh `oat-browser-agent`
  build is enough.
- The `oat-browser-agent` bridge shipped a follow-up tool-correctness
  batch that benefits OAT browser-agents without any OAT-side code
  changes: `browser_go_back` / `browser_go_forward` now resolve the
  correct CDP `NavigationEntry.id` (previously failed with
  `"No entry with passed id"` on any non-trivial history);
  `browser_close_tab` is now allowed against any tab the agent ever
  attached, including ones it has since explicitly detached
  (cleanup-after-detach was previously blocked by the
  `TAB_NOT_ATTACHED` guard); `browser_file_download` switched from
  the unreachable browser-scope CDP `Browser.setDownloadBehavior`
  path to the native `chrome.downloads` API (the old path always
  failed at tab-scope attachments); and `browser_handle_dialog`'s
  "no dialog" error is now a structured, actionable
  `DIALOG_NOT_PRESENT` instead of the opaque CDP string. See the
  [oat-browser-agent CHANGELOG](https://github.com/Root-IO-Labs/oat-browser-agent/blob/main/CHANGELOG.md)
  for details. Pull a fresh `oat-browser-agent` build to pick up
  these fixes.
- The `oat-browser-agent` bridge shipped a batch of connection-
  robustness improvements that benefit OAT browser-agents without
  any OAT-side code changes: WebSocket heartbeat (keeps the Chrome
  MV3 service worker alive while the bridge is reachable),
  long-lived NM broker (keeps the SW alive whenever a bridge is
  reachable, via the documented MV3 NM-port escape hatch), atomic
  `bridge-runtime.json` discovery file with PID-aware cleanup and
  a 60-second self-heal heartbeat (fixes a race during Cursor
  restarts where the old bridge's cleanup would clobber the new
  bridge's discovery file), and a per-user `npm run doctor` +
  postinstall script that detects and self-heals a missing Native
  Messaging host registration. See the
  [oat-browser-agent CHANGELOG](https://github.com/Root-IO-Labs/oat-browser-agent/blob/main/CHANGELOG.md)
  for details. Pull a fresh `oat-browser-agent` build and re-run
  `npm run install-host` (or just `npm run doctor:fix` if you've
  registered before) to pick up the new behaviour.
- The bridge also now advertises MCP server-side prompts. The
  canonical `browser_agent_system` prompt covers the generic
  operating guidance (perception cost hierarchy, click fallback
  ladder, untrusted-content handling, cross-tab discipline,
  common failure modes). OAT continues to ship the full
  agent-template prompt from `internal/templates/agent-templates/browser.md`
  and does not auto-load MCP server prompts, so OAT agents are
  not double-fed. The MCP prompt is intended for non-OAT MCP
  clients (Cursor, Claude Code, Claude Desktop, etc.) that want
  to bootstrap browser-agent guidance without re-deriving it from
  tool descriptions.

### Fixed

- Browser-agent prompt template
  (`internal/templates/agent-templates/browser.md`) flips the
  `browser_wait_for` selector-vs-text guidance. The previous
  wording — "Use selectors over text whenever possible" — was the
  exact pattern that caused the half-rendered-snapshot failure
  mode the testbed reverify run hit. On SPA route transitions,
  many apps mount the new route's container element before
  populating its content, so a selector-only wait resolves the
  instant the empty container appears. The prompt now leads with
  `text:` for "wait until content rendered" cases and reserves
  `selector:` for "wait for a structural element to exist" cases
  (or as a scoping bound on a `text:` search). Same template now
  also documents the `browser_get_text mode='full'` shadow-DOM /
  cross-origin-iframe underreport risk and points at
  `browser_observe` + `browser_find` as the recovery (they walk
  the AX tree, which descends into shadow roots). Both updates
  are prompt-only and ship alongside the oat-browser-agent
  schema changes that document the same contracts at the tool
  layer.
- Browser-agent prompt template
  (`internal/templates/agent-templates/browser.md`) "Prompt
  Injection Defense" section now enumerates every read-tool whose
  result is wrapped in `[UNTRUSTED-<nonce>:…]` delimiters, rather
  than the previous vague "page-derived text returned by tools".
  The wrap now covers `browser_find`, `browser_observe`,
  `browser_console_messages`, `browser_network_requests`,
  `browser_evaluate`, `browser_cookies_list`, and the outer
  `browser_batch` envelope in addition to the canonical three
  (`browser_get_text` / `browser_snapshot` / `browser_extract`).
  The prompt also explicitly clarifies that action tools
  (`browser_click`, `browser_navigate`, etc.) return only
  bridge-issued metadata and are not wrapped. Ships in lockstep
  with the oat-browser-agent extension to TEXT_TOOLS /
  REDACT_RESULT_TOOLS.
- Browser-agent prompt template
  (`internal/templates/agent-templates/browser.md`) "Cross-Tab
  Discipline" section now documents `browser_new_tab`'s new
  `attach: true` default (the oat-browser-agent change auto-attaches
  the new tab before returning, so the agent does not need a
  separate `debugger_attach` round-trip for the common
  "spawn-and-drive" workflow). Adds the `attach: false` opt-out
  for fire-and-forget tabs and the `attachError` recovery path for
  restricted-scheme initial URLs. No behaviour change in this repo —
  the prompt update ships alongside the oat-browser-agent code
  change so the agent's mental model matches the bridge.
- Browser-agent prompt template
  (`internal/templates/agent-templates/browser.md`) now describes
  `browser_fill` accurately: it commits to React / Vue / Angular
  controlled inputs (the underlying oat-browser-agent change routes
  `browser_fill` through CDP `Input.insertText` instead of the legacy
  DOM-setter path, which silently no-op'd on framework-controlled
  inputs). The tool list line for `browser_type` also gains a hint
  about when per-keystroke typing matters (autocomplete that fires on
  each keypress). No behaviour change in this repo — this is a prompt
  doc fix that ships alongside the oat-browser-agent code fix so the
  agent's mental model matches the bridge.
- Daemon no longer sends periodic status-check nudges to
  `AgentTypeBrowser`. Browser-agent is a tool, not a worker: it
  receives tasks via inter-agent messaging from the supervisor /
  workers and sits silent between tasks. The pre-existing nudge
  ("Update on your browser task progress?") was a Part 2 miss from
  the mcp-and-opt-in-browser-agent plan -- it wasted an LLM turn
  every nudge interval to answer "nothing happening" between tool
  calls. The `case state.AgentTypeBrowser:` arm in
  `nudgeAgentsInRepo` is now intentionally absent (commented to
  prevent future re-additions).
- Daemon now backs off auto-restarting an unreachable browser-agent
  after `bridgeUnreachableThreshold=3` consecutive health-check
  failures inside a `bridgeUnreachableWindow=10m` sliding window.
  Hit the threshold and the daemon stops respawning until the user
  explicitly runs `oat agent restart browser-agent`, which also
  clears the failure counter. Closes the Part 2 miss where the
  2-min health-check loop would spawn a doomed bridge subprocess
  every cycle when Chrome was closed or the extension uninstalled,
  burning tokens on the bridge's startup banner each time. New
  `recordBridgeUnreachable` / `clearBridgeUnreachable` helpers
  covered by `TestBridgeUnreachableBackoff`.

### Changed

- Daemon's browser-agent MCP config now pins `OAT_BRIDGE_WS_PORT=19222`
  AND `OAT_BRIDGE_TRUST_LOCALHOST=1` in the bridge env block, both as
  back-compat for the bridge's Part 8 / Part 9a flips:
  - `OAT_BRIDGE_WS_PORT=19222`: the bridge upstream flipped its
    default to OS-assigned (port 0) so concurrent bridges don't
    collide on 19222, but the Chrome extension's
    `chrome.storage.local` fallback is still 19222 until Part 9b's
    NM-based port delivery channel ships. Without this pin, an
    OAT-spawned bridge would bind to e.g. :51234 and the extension
    would silently dial :19222 (the fallback) and never reach it.
  - `OAT_BRIDGE_TRUST_LOCALHOST=1`: the bridge upstream flipped its
    `trustLocalhost` default from `true` to `false` (token-required
    is now the secure default; the pre-flip
    `OAT_BRIDGE_REQUIRE_TOKEN=1` opt-in env var is retired). Until
    Part 9b's NM-based token delivery channel ships, an OAT-spawned
    bridge has no way to seed the extension's
    `chrome.storage.local.oat_session_token`, so without this
    escape-hatch the extension would be rejected at the WS handshake.

  Both env entries lift together when Part 9b lands; at that point
  every OAT-spawned bridge gets its own OS-assigned port and the NM
  channel teaches the extension the per-launch token (collision-safe
  thanks to the bridge's new orphan watchdog).
  `TestBuildBrowserAgentMCPConfig_StructureAndContents` covers both
  pins so they can't regress silently.

### Added

- `oat agent add <type> [name] [--repo <repo>]` CLI verb for opt-in
  persistent agents. Only `browser-agent` is supported today; the
  dispatcher is structured so additional opt-in types can land
  without restructuring. The browser-agent flow runs a preflight
  bridge probe (`OAT_BROWSER_AGENT_BRIDGE_PATH` env > `PATH` >
  `~/.oat/oat-browser-agent/dist/bridge/index.js`) and bails with
  an actionable message listing every install option if no bridge
  is found. Idempotency: re-adding a healthy browser-agent is a
  hard fail (suggesting `oat agent restart` or `remove`); a stopped
  record is silently respawned. Worktree at
  `~/.oat/wts/<repo>/<agent>/` is created on first add and reused
  on respawn. End-to-end: `bridge preflight -> list_agents idempotency
  check -> worktree create -> add_agent -> start_repo_agents`.
- `AgentConfig.MCPConfig` (`pkg/backend`): when non-empty, the
  direct backend writes the string to `<WorkDir>/.oat/mcp.json`
  before launching the agent process, alongside the existing
  `AGENTS.md` write. The daemon populates this for `AgentType ==
  browser` in both initial spawn (`startRegisteredAgent`) and
  manual restart (`restartAgent`) paths, so the bridge command and
  audit-log dir are always fresh. `.oat/.gitignore` now also lists
  `mcp.json` so worktrees never accidentally commit the bridge
  configuration.
- `internal/agents/browser_bridge.go::ResolveBrowserBridge` --
  shared bridge resolver used by both the CLI preflight and the
  daemon's MCP config builder, so they agree byte-for-byte on
  what command will be spawned. Returns a `BridgeCommand` with
  `Command`, `Args`, and a human-readable `Source` describing
  where it was found.

### Fixed

- Daemon recovery now restores the opt-in browser-agent after a
  backend-session loss. `restoreRepoAgents` previously rebuilt only
  the always-on agent set (supervisor, merge-queue or pr-shepherd,
  workspace) and left a `~/.oat/wts/<repo>/browser-agent/` worktree
  orphaned in state. The fix: when the worktree path exists,
  `restoreRepoAgents` now calls `startAgent(AgentTypeBrowser, ...)`
  so the agent is respawned. The worktree path acts as the "user
  opted in" persistence marker; no extra state-file field is
  needed. Without this, `oat agent add browser-agent` followed by
  any daemon crash + restart would silently drop the agent.
- `startAgentWithConfig` (used by `restoreRepoAgents` and other
  generic spawn paths) now also writes `.oat/mcp.json` for
  `AgentTypeBrowser` -- previously only `startRegisteredAgent`
  (the `start_repo_agents` path) did this, so a recovery-restored
  browser-agent would launch with no MCP tools. Both spawn paths
  now go through `buildBrowserAgentMCPConfig`.

### Changed

- New user-facing [docs/MCP.md](docs/MCP.md): annotated `.oat/mcp.json`
  schema, where the daemon writes it, the bridge-resolution order
  (`OAT_BROWSER_AGENT_BRIDGE_PATH` env > `$PATH` > `~/.oat`
  bundle), the full env-var contract (including the temporary
  `OAT_BRIDGE_WS_PORT` / `OAT_BRIDGE_TRUST_LOCALHOST` back-compat
  pins), the text/image/error result-type semantics returned to the
  LLM, and a checklist for adding an MCP server to a future agent
  type. Referenced from `docs/AGENTS.md` in the browser-agent
  section.
- Docs canonicalised on the browser-agent audit log path:
  `~/.oat/output/<repo>/browser-agent-actions.jsonl`. The daemon
  already passes `OAT_BROWSER_AGENT_AUDIT_LOG_DIR=<that dir>` in the
  MCP server's env block (Part 2), so this is the path actually
  written when the agent runs under OAT. Older docs in
  `docs/DIRECTORY_STRUCTURE.md` and `docs/COMMANDS.md` claimed
  `<repo-root>/.oat-logs/...`, which was never accurate under OAT
  and has been corrected. Root `AGENTS.md` also updated to call out
  that the browser-agent is opt-in via `oat agent add browser-agent`
  rather than auto-started with the repo.

- `handleStartRepoAgents` now skips agents whose PID is still alive,
  making the verb idempotent. Required to safely re-call after
  `oat agent add` registers a single new agent on an
  already-running repo (without the skip, every existing supervisor
  / merge-queue / worker would be double-spawned).
- `list_agents` socket response includes the agent's `pid` field
  alongside the existing name / type / worktree_path / window_name /
  task / summary / model / created_at fields. Used by `oat agent
  add`'s liveness check; backwards-compatible (new key, no shape
  change).

- MCP (Model Context Protocol) client support in agent-runtime.
  `oat_sdk.mcp_client` loads MCP servers declared in
  `<cwd>/.oat/mcp.json` at agent startup and exposes their tools as
  LangChain `BaseTool` instances merged into the existing
  `create_cli_agent(tools=...)` call. Stdio transport only for now;
  the file shape is `{servers: [{name, command, args, env,
  transport: "stdio"}]}`. The daemon writes this file at agent
  spawn time when `MCPConfig` is non-empty (analogous to how it
  already writes `AGENTS.md`); when no MCP is configured the file
  is absent and the agent runs unchanged. Both the interactive
  (`oat_cli.main`) and daemon-spawn / non-interactive
  (`oat_cli.non_interactive`) paths are wired. SIGTERM from the
  daemon cancels the running task; the resulting `CancelledError`
  propagates through a `try/finally` that calls
  `AsyncExitStack.aclose()` so each MCP server's stdio child is
  reaped, not orphaned. Concerns the adapter owns directly:
  per-session `asyncio.Lock` to serialise parallel tool calls on
  one stdio stream, sidecar `KIND_TOOL_CALL`/`KIND_TOOL_RESULT`
  event emission on both success and error paths, canonicalisation
  of MCP `TextContent`/`ImageContent`/`EmbeddedResource` blocks to
  LangChain-friendly shapes, tool-name collision resolution
  (built-in tools win; colliding MCP tools are exposed as
  `<server>__<tool>`), and graceful degradation on malformed
  `mcp.json` (warning + zero MCP tools; never a crash).

### Changed

- Browser-agent system prompt (`internal/templates/agent-templates/browser.md`)
  gains a "Perception cost hierarchy" section that teaches the cheapest-tool-
  that-gets-the-job-done order for read-only, interaction, and state-change
  tasks. Read-only "what's on this page?" tasks now default to
  `browser_get_text {mode: "main", maxChars: 4000}` (Mozilla Readability
  extraction; ~80% token reduction on Wikipedia-class long-form pages vs.
  the full-page walk that Part 4.5 confirmed real LLMs reach for first),
  with `browser_snapshot {interactiveOnly: true}` as the interaction
  primary and a fallback ladder for `NO_MAIN_CONTENT` cases. The Strategy
  section's `browser_get_text` / `browser_snapshot` entries also gain the
  `mode: "main"` and `interactiveOnly: true` hints. Land in lockstep with
  oat-browser-agent's Part 7.5c (`mode: "main"` Readability path + the
  Part 7.5d post-completion / `tabId` enrichment). End users running the
  browser-agent against article-style pages will see ~5x lower perception
  token cost on the first read.

- Browser-agent system prompt (`internal/templates/agent-templates/browser.md`)
  gains a "One decision at a time" section. Production browser-agents are now
  guided to act like a careful operator: one destructive action at a time per
  domain, re-snapshot before clicking visually close controls, confirm
  intermediate state before the next destructive call, prefer to stop and
  explain on password fields / sensitive pages / unfamiliar UI patterns, and
  pace themselves on logged-in or session-bearing pages. End users will see
  fewer simultaneous clicks and a more measured pace on multi-step flows.
  The change ships in lockstep with the oat-browser-agent model bench so the
  same prompt drives both production and benchmark scoring.

### Added

- `benchmarks/llm_call.py` — provider-agnostic LangChain wrapper used by the
  benchmark scripts. Resolves any `provider:model` string OAT supports
  (anthropic, openai, google_genai, openrouter, deepseek, ollama, ...),
  surfaces a clear `missing FOO_API_KEY` error per provider, and emits
  normalized `{text, input_tokens, output_tokens, model, provider}` JSON
  on stdout (logs go to stderr).
- `benchmarks/test_llm_call.py` — fully-mocked unit tests for `llm_call.py`
  covering bare-vs-`provider:model` parsing, missing-API-key paths,
  stdout/stderr discipline, token-usage normalization across provider
  response shapes, and exit-code mapping for resolution / API failures.
- `benchmarks/run.sh --summary-model <provider:model>` — symmetric with
  `--judge-model`, controls the post-run summary model. Defaults to the
  orchestrator `--model` so multi-provider runs don't incur surprise
  charges from a different provider's key sitting in the environment.
- `benchmarks/run-comparison.sh --judge-model` and `--summary-model` —
  forwarded to each leg's inner `run.sh` invocation; each leg defaults
  to its own orchestrator model when the override is omitted.
- `benchmark-helpers-tests` job in `.github/workflows/ci.yml` — runs
  `pytest` over `benchmarks/test_probe_model.py` (already present, was
  not gated by CI before) and the new `benchmarks/test_llm_call.py`.
  Both files mock LangChain entirely so the job needs no provider keys.
- Provenance HTML comment in `summary.md`
  (`<!-- Generated by <provider:model> at <RFC3339> -->`) and a
  `model: <resolved provider:model>` field in `gate.json`, so future
  readers can tell exactly which judge / summarizer produced each run's
  outputs (a bare `claude-sonnet-4-6` is normalized to
  `anthropic:claude-sonnet-4-6`).
- OSS meta files: `CHANGELOG.md`, `MAINTAINERS.md`, `AUTHORS`, `.github/FUNDING.yml`,
  `.github/ISSUE_TEMPLATE/*`, `.github/PULL_REQUEST_TEMPLATE.md`.
- `.github/dependabot.yml` for automated Go, Python, and GitHub Actions dependency updates.
- `.github/workflows/codeql.yml` for weekly CodeQL security analysis (Go + Python).
- `.github/workflows/auto-uv-lock.yml` to refresh `uv.lock` on Dependabot Python PRs.
- `.golangci.yml` with an aggressive linter ruleset (gofmt, govet, errcheck,
  staticcheck, ineffassign, unused, unconvert, goimports, misspell) wired into CI.
- `internal/version` package with `Version`, `Commit`, `Date` injected via
  `ldflags -X` at build time; `oat version` now reports all three.
- `.github/workflows/release.yml` + `.goreleaser.yml` for tag-triggered binary
  releases (linux/darwin × amd64/arm64) with GitHub Releases artifacts and
  Homebrew tap auto-update.
- `oat model set <provider:model> [--nudge-interval SECONDS] [--max-tokens N]`
  CLI subcommand for tuning per-model runtime parameters.
- `oat tokens report --repo <name> [--since <ts>] [--until <ts>] [--format json|table] [--wave N]`
  CLI subcommand for historical per-wave token-usage analysis from agent logs
  (distinct from `oat status --tokens` which reads live daemon state).
- `runtime.max_tokens` and `runtime.nudge_interval_seconds` fields in model
  profile YAMLs; daemon falls back to existing defaults when unset.
- `benchmarks/summarize.sh` per-wave token-usage table via `oat tokens report`.

### Changed

- `LICENSE` copyright year updated from 2025 to 2026 (development began Jan/Feb 2026).
- Final-nudge message templates in the daemon (`finalNudgeSupervisor`,
  `finalNudgeMergeQueue`, `finalNudgePRShepherd`) compacted by ~55-65% with
  no actionable information lost; merge-queue template preserves the
  `sleep 30` polling instruction verbatim.
- Benchmark script layout: internal helpers (`run-blackbox.sh`,
  `judge-blackbox.sh`, `whitebox-shim.py`) moved to `benchmarks/scripts/`.
  User-facing entry points (`run.sh`, `setup.sh`, `acceptance-test.sh`,
  `summarize.sh`, `collect.sh`, `cleanup.sh`, comparison commands) remain at
  the top level. All internal callers (`benchmarks/run.sh`,
  `benchmarks/judge-cursor-gate.sh`, `benchmarks/README.md`) updated to the
  new paths.
- `benchmarks/summarize.sh` and `benchmarks/scripts/judge-blackbox.sh`
  are now provider-agnostic: both call the new `llm_call.py` helper
  instead of curling `https://api.anthropic.com/v1/messages` directly.
  Model resolution order for both scripts: explicit flag
  (`--model` / `--judge-model`) -> `OAT_BENCH_LLM_MODEL` env var ->
  orchestrator model from `collect.json` (summarize only) ->
  `anthropic:claude-sonnet-4-6` hard fallback. A missing API key for
  the resolved provider now produces a clear `missing FOO_API_KEY`
  error from `llm_call.py` instead of a 401 from Anthropic.
- `benchmarks/run-comparison.sh` no longer hard-fails at startup when
  `ANTHROPIC_API_KEY` is unset. Provider keys are checked per-run by
  the inner scripts, so cross-provider comparisons (e.g. Sonnet vs
  GPT-5) work without requiring keys for both providers up front.
- Go and Python dependencies refreshed to current minor/patch versions.
- GitHub Actions pinned to latest stable versions across all workflows.

### Documented

- `oat status --tokens` CLI command and prompt-caching feature in
  [`docs/COMMANDS.md`](docs/COMMANDS.md), [`docs/ADVANCED_USAGE.md`](docs/ADVANCED_USAGE.md),
  and [`README.md`](README.md). The feature itself shipped earlier but was
  previously undocumented.

### Removed

- Dead code surfaced by `unused` linter: `getOSInfo`, `writeMergeQueuePromptFile`,
  `writePRShepherdPromptFile`, `quoteForShell`, `stdLogger` and its methods,
  `worktreeRefreshLoop` empty shell, `App.err` and `pollResultMsg.repoName`
  fields, `internal/cli/verify_simple.go` (abandoned duplicate-block detector,
  superseded by `verify.go`).

### Fixed

- **Wave 0 timing in `collect.json` is no longer derived from a fuzzy
  GitHub PR search.** `benchmarks/run.sh` now records `wave 0`
  `started_epoch` / `completed_epoch` to `wave-timing.json` alongside
  waves 1–4, and `benchmarks/collect.sh` reads that data via
  `wave_timing_from_file "0"` instead of falling back to
  `gh pr list --search "closes #N OR fixes #N"`. The GitHub search was
  too fuzzy and matched PRs whose body referenced unrelated issues whose
  numbers contained `N` as a digit substring (e.g. issue #1 spuriously
  matching a PR body that mentioned #17), inflating wave 0's reported
  duration on a real run from ~24 min to ~119 min. Older result
  directories without a `"0"` key in `wave-timing.json` continue to fall
  back to the PR-derived path, so historical analysis is unchanged.
- **`benchmarks/llm_call.py` now imports `create_model` from the canonical
  `oat_cli.config` path** used by the rest of the benchmark tooling
  (e.g. `benchmarks/probe-model.py`), fixing a runtime
  `ModuleNotFoundError` warning that masked custom-provider support from
  `~/.oat/config.toml`. The langchain `init_chat_model` fallback was hiding
  the breakage but routing all bench LLM calls through the non-config path.
  `.gitignore` cleaned up: removed a stale entry that was redundant with the
  existing `.oat/*` ignore rule covering the project-local cache directory.
- **`benchmarks/setup.sh` no longer silently swallows `gh label create`
  failures.** GitHub's secondary rate limit can throttle the burst of 28
  label creates against a fresh repo, leaving a subset of labels uncreated.
  The script previously redirected those errors to `/dev/null` and
  continued, so the failure surfaced ~30s later as a confusing
  `gh issue create` rejection mid-loop (and `set -euo pipefail` then killed
  the run, tripping `run.sh`'s cleanup trap). Now: each `gh label create`
  and `gh issue create` is wrapped in a bounded retry-with-exponential-
  backoff helper (`gh_with_retry`), the bursts are paced
  (200ms / 300ms between calls), and any final failure exits with a clear
  diagnostic listing the offending labels/issue and pointing at
  `gh label list --repo <repo>` for inspection.
- Duplicate `.github/workflows/main.yml` removed (byte-equivalent to the
  `check-source` job in `ci.yml`).
- **Verifier no longer rejects work on a stale-base race.** The daemon now
  snapshots the remote default-branch SHA at `oat worker request-review` and
  pins it on the worker as `BaseSHA`. Verifier prompts and self-verify both
  diff against `${BASE_SHA}..HEAD` instead of live `origin/main`, so commits
  that landed on `main` between the worker's rebase and the verifier's review
  no longer appear as "deletions" and incorrectly fail the diff. Falls back
  to live `origin/main` when `BaseSHA` is empty (in-flight verifications
  during upgrade).
- **Daemon false "verifier crashed" message.** `cleanupDeadAgents` now
  guards the crash wake-message with `!agent.ReadyForCleanup`, so a verifier
  that successfully delivered a verdict but had its worker status concurrently
  reset by another `request-review` no longer prints a bogus "your verifier
  crashed" message in the worker log.
- **`benchmarks/collect.sh` worker-name collection on macOS.** Replaced
  `declare -A WORKER_NAMES` (bash-4-only) with the same jq + `sort -u`
  pattern already used in `summarize.sh`. macOS's default bash 3.2 was
  silently failing the script and producing no `collect.json`.
- **`benchmarks/run.sh` bash-3.2 portability.** Several latent
  `set -euo pipefail` × bash-3.2 bugs that crashed real runs on macOS
  default bash:
  - `PRE_COUNT` / `PROFILE_COUNT` were computed with
    `grep -c <pattern> || echo "0"`. `grep -c` always emits the count to
    stdout AND exits 1 on zero matches, so the fallback was *appending* a
    second `0`, producing values like `"0\n0"` and a downstream
    `[[: 0 0: syntax error in expression`. Switched to `|| true` plus an
    empty-string guard.
  - `assemble_gate_test()` expanded `"${module_files[@]}"` and
    `"${sorted_modules[@]}"` without length guards. Bash 3.2 + `set -u`
    treats an empty array as unset; this crashed with
    `unbound variable` on the new sanity-check fixture (zero
    `test-*.sh` modules). All `[@]` expansions in that function now
    sit behind `${#arr[@]} -gt 0` guards, and the `IFS=$'\n' arr=($(sort
    <<< ...))` one-liner was replaced with a portable `while IFS= read`
    loop that skips the printf entirely when the source array is empty.
  - The convergence-loop result writer now guards
    `for cr in "${CONVERGENCE_RESULTS[@]}"` so an early grand-timeout
    or convergence-timeout (which can break out before the first
    iteration appends a result) doesn't crash the JSON emitter.
  - Removed an orphaned `"${SMOKE_REASONS[@]}"` reference left behind
    by a partial revert (added in `9bcc051`, mostly removed in
    `8a2f71d`'s execution-based smoke runner, but the read-side line
    snuck back in `f87f6d6`). The `RAW_SNIPPET` immediately below
    already provides the same diagnostic info from the actual runner
    output.
- **Benchmarks: harden `run.sh` and `collect.sh` against degraded GitHub
  issue-list/search index.** Wave 0 gate discovery, per-wave kickoff totals,
  `wait_for_wave`'s completion arithmetic, and post-run analysis collection
  now treat [`benchmarks/issues.json`](benchmarks/issues.json) as the source
  of truth and fall back to it (with a loud `WARNING:`, or `ERROR:` for
  issues that genuinely 404 per per-issue probe) when `gh issue list`
  returns fewer issues than expected. Per-issue endpoints are unaffected by
  ElasticSearch degradation and are used both as the satisfaction check and
  as the fallback fetch path. Prevents under-spawning workers, false-positive
  wave completion, and silently-wrong analysis numbers during GitHub Issues
  indexing degradation (e.g. the Apr 27 2026 incident, where
  `gh issue list --label wave:0` returned only `#4` for ~14 minutes despite
  `#1`/`#2`/`#3` being live and reachable). Polling timeout/interval are
  tunable via `OAT_INDEX_POLL_TIMEOUT` (default 120s) and
  `OAT_INDEX_POLL_INTERVAL` (default 10s); on the healthy path the new
  helpers add ~0.5s per benchmark. Shared logic lives in the new
  [`benchmarks/lib.sh`](benchmarks/lib.sh).

### Added

- **Pre-flight Python import check (verifier Step 5b).** Verifier prompt
  now instructs Python projects to run `python -m pytest --collect-only
  <test-file>` before writing black-box tests so hallucinated import paths
  fail in seconds instead of waiting for full collection.
- **Cost reporting in `oat tokens report` and `oat status --tokens`.** New
  `COST_USD` column derived from the embedded `internal/routing/pricing.yaml`
  (now with explicit `cache_creation_per_mtok` for Anthropic models and a
  per-provider fallback helper for everything else). `--format json` exposes
  `cost_usd` per agent and on the totals block. `benchmarks/summarize.sh`
  appends a "Total cost (priced agents only)" line to the markdown summary.

## [0.1.0] - TBD

Initial public release.

[Unreleased]: https://github.com/Root-IO-Labs/open-agent-teams/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/Root-IO-Labs/open-agent-teams/releases/tag/v0.1.0
