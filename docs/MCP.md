# MCP (Model Context Protocol) in OAT

OAT agents can load tools from external MCP servers at startup, alongside
their built-in tool set. Today the canonical consumer is the **Browser
Agent**, which uses `oat-browser-agent`'s stdio bridge to drive Chrome; the
mechanism is general and any agent type can declare additional MCP servers
the same way.

This document covers:

- The `.oat/mcp.json` schema (with an annotated example).
- Where the daemon writes it and how the agent-runtime reads it.
- Bridge resolution and env-var contract.
- Result-type semantics (text, image, error) returned to the LLM.
- How to add an MCP server for a custom agent type.

For the user-visible commands (`oat agent add browser-agent`, etc.) see
[COMMANDS.md](COMMANDS.md). For the agent-system architecture in general
see [AGENTS.md](AGENTS.md).

## The `.oat/mcp.json` file

Each agent worktree gets its own `.oat/mcp.json`. The Python agent-runtime
(`agent-runtime/libs/oat_sdk/oat_sdk/mcp_client.py`) reads it once at
startup, spawns each server, and registers its tools as LangChain
`StructuredTool` instances on the agent. If the file is absent, no MCP
tools are loaded and the agent runs with its built-in tools only -- there
is no error.

### Schema

```json
{
  "servers": [
    {
      "name": "browser_bridge",
      "command": "node",
      "args": ["/Users/you/.oat/oat-browser-agent/dist/bridge/index.js"],
      "transport": "stdio",
      "env": {
        "OAT_BROWSER_AGENT_AUDIT_LOG_DIR": "~/.oat/output/my-repo",
        "OAT_BROWSER_AGENT_SESSION": "my-repo",
        "OAT_BROWSER_AGENT_NAME": "browser-agent"
      }
    }
  ]
}
```

| Field | Required | Notes |
|---|---|---|
| `name` | yes | Unique identifier; used as a namespace for tool-name collisions. |
| `command` | yes | Executable to spawn. `~` and `$VAR` are expanded. |
| `args` | no  | Argv passed to the executable. Each entry is `~`/`$VAR`-expanded. |
| `transport` | no  | Only `"stdio"` is supported today. Other values are skipped with a warning. |
| `env` | no  | Env vars merged into the spawned process. Values are `~`/`$VAR`-expanded. |

A malformed file, a server that refuses to start, or an unreachable
transport never crashes the agent. The loader logs a warning and proceeds
with no MCP tools (mirrors how the bridge handles a malformed
`config.toml`).

### Lifetime

`load_mcp_tools` returns the tools plus an `AsyncExitStack`. The CLI's
`async with` scope owns the stack and closes it on shutdown so each MCP
child process is reaped cleanly. Concurrent tool calls on the same server
are serialised with an `asyncio.Lock` -- stdio JSON-RPC interleaving from
two parallel LangGraph tool dispatches would corrupt framing otherwise.

## Where the daemon writes it

`oat agent add browser-agent` invokes the daemon's
`buildBrowserAgentMCPConfig` ([internal/daemon/daemon.go](../internal/daemon/daemon.go))
which:

1. Resolves the bridge command (see below).
2. Sets `OAT_BROWSER_AGENT_AUDIT_LOG_DIR` to the canonical per-repo output
   directory `~/.oat/output/<repo>` so the bridge writes its audit log
   alongside `supervisor.log` / `default.log` without bridge-side repo
   awareness.
3. Sets `OAT_BROWSER_AGENT_SESSION` (the repo's session name) and
   `OAT_BROWSER_AGENT_NAME` (the browser-agent's window/agent name) so the
   bridge can address the right PTY when relaying side-panel chat through
   the daemon's `agent_input` / `agent_output_subscribe` socket verbs.
   Bridges spawned outside OAT (e.g. directly under Cursor or Claude
   Code) will see these vars absent and treat the chat path as disabled.
4. Marshals the config and writes it to `<worktree>/.oat/mcp.json` with
   `0644` permissions.

The env block is intentionally minimal: the WS sidecar port is
OS-assigned and the per-launch session token is delivered to the
extension via Native Messaging (`extension/src/nm-port.ts` +
`bridge/src/nm-broker.ts`), so OAT no longer needs to pin a port
or trust localhost. Two bridges running side-by-side no longer
collide on `bind()`, though they still contend for the single
extension client (last NM push wins; documented v1 limitation).

The Python agent-runtime resolves `<worktree>/.oat/mcp.json` from the
agent's CWD on startup. The same file is regenerated on every
`oat agent restart browser-agent` so a bridge path that moves between
installs is picked up automatically.

## Bridge resolution

Both the daemon (when generating the config) and the CLI (when running
`oat agent add browser-agent` preflight) call
`agents.ResolveBrowserBridge` ([internal/agents/browser_bridge.go](../internal/agents/browser_bridge.go)),
which probes in this order:

1. `$OAT_BROWSER_AGENT_BRIDGE_PATH` (literal path). Treated as a Node
   script when it ends in `.js`/`.mjs` and invoked via `node`, otherwise
   invoked directly. Use this for development against a local checkout.
2. `oat-browser-agent` on `$PATH` (npm-installed shim).
3. `~/.oat/oat-browser-agent/dist/bridge/index.js` (release-tarball
   unpack), run through `node`.

If none match, `oat agent add browser-agent` fails with structured
remediation pointing at install options. The browser-agent record is not
added to state on failure, so re-running after fixing the install picks
up where you left off.

## Env vars the agent-runtime honours

The agent-runtime passes through every entry in `env` verbatim (after `~`
+ `$VAR` expansion). The browser-bridge respects the following; other MCP
servers define their own:

| Var | Set by daemon? | Purpose |
|---|---|---|
| `OAT_BROWSER_AGENT_AUDIT_LOG_DIR` | yes (`~/.oat/output/<repo>`) | Highest-precedence override for the bridge audit-log directory. The bridge falls back to `<repo-root>/.oat-logs/` only when no env var is set (legacy path; see [DIRECTORY_STRUCTURE.md](DIRECTORY_STRUCTURE.md)). |
| `OAT_BROWSER_AGENT_SESSION` | yes (repo session name) | Identity for Part 2 side-panel chat. The bridge uses this to scope `agent_input` socket calls to the right session when relaying user messages from the side panel. Absent when the bridge runs outside OAT (Cursor/Claude Code) — the bridge then disables the chat path and the side panel shows the disabled-state banner from Part 4. |
| `OAT_BROWSER_AGENT_NAME` | yes (e.g. `browser-agent`) | Companion to `OAT_BROWSER_AGENT_SESSION`. Identifies which agent within the session owns the PTY. Same absence semantics. |
| `OAT_BRIDGE_WS_PORT` | no | Pin the WS sidecar to a fixed port. Default is OS-assigned (port 0). Useful for debugging when you want a predictable port; otherwise leave unset and let the bridge publish its assigned port via Native Messaging. |
| `OAT_BRIDGE_TRUST_LOCALHOST` | no | Accept anonymous localhost WS opens. Default is token-required (since plan Part 9a). Only set this if you are running the bridge in an isolated VM where localhost is trusted by construction; production end-user installs should leave it unset and let the Native-Messaging broker deliver the per-launch session token. |
| `OAT_BRIDGE_ALLOW_MULTI` | no | Opt back in to the pre-1.0 multi-client WS fan-out. Default is single-client (one extension per bridge). |
| `OAT_BRIDGE_STRICT_MODE` | no | Schema-runtime drift telemetry mode: `warn` (default) / `reject` / `off`. See `oat-browser-agent` CHANGELOG. |

## Result-type semantics

MCP servers can return three flavours of content. The agent-runtime
canonicalises each to a shape LangChain accepts:

| Content type | LangChain shape | Notes |
|---|---|---|
| Text | `ToolMessage(content=text)` | The common case. |
| Image (`data` + `mimeType`) | Multimodal content block (`{"type": "image", "source": {...}}`) | Renders correctly on Claude / Gemini / GPT-5. Text-only models see a typed error rather than silent loss. |
| Embedded resource | Text representation of the resource. | The MCP spec allows servers to return file-like resources; we collapse them to text today. |
| Error (`isError: true`) | `ToolMessage(content=text, status="error")` | The LLM sees the structured error and can decide to retry or change tactics. |

Every MCP tool call emits `KIND_TOOL_CALL` and `KIND_TOOL_RESULT` sidecar
events on both success and error paths, so OAT's cost tracking and
`model-bench` observability see MCP work the same as built-in tools.

## Adding an MCP server for a custom agent type

The browser-agent path is hardcoded today (the daemon writes
`.oat/mcp.json` from `buildBrowserAgentMCPConfig`), but the agent-runtime
loader is server-agnostic. To add a custom MCP server for a new agent
type:

1. **Write the config to the worktree.** Drop a `.oat/mcp.json` in the
   agent's worktree at agent creation time -- via the daemon's
   `AgentTypeXxx` spawn path, or by hand for one-off experimentation.
2. **Use stdio.** That's the only supported transport today. SSE and
   WebSocket-server transports are a future extension; the schema
   already leaves room for `transport: "sse" | "ws"` to land without a
   breaking change.
3. **Set `name` to something stable.** It's used as a namespace for
   tool-name collisions when multiple servers are loaded simultaneously.
4. **Expand `~` and `$VARs` in your config.** The loader does this for
   `command`, every `args` entry, and every `env` value. Portable configs
   should write `~/.oat/foo/bin` rather than `/Users/you/...`.
5. **Return structured errors with `isError: true`.** The agent-runtime
   surfaces these to the LLM as `ToolMessage(status="error")` so the
   model can react. Throwing an uncaught exception in your server will
   surface as a generic tool-execution error instead.

A planned future helper -- `oat agent add <type> --mcp <name>=<command>`
-- would let users compose MCP servers onto any agent type without
hand-editing JSON. Until then, drop the file by hand or extend the
daemon's per-type spawn path.

## Troubleshooting

- **Agent starts but the browser tools aren't available.** Check the
  agent's `~/.oat/output/<repo>/<agent>.log` for a warning starting with
  `Failed to read MCP config at` or `MCP server '...' failed to start`.
  Most often: a `command` path that doesn't exist or a `transport` other
  than `stdio`.
- **`oat agent add browser-agent` fails with "bridge not found".** Set
  `OAT_BROWSER_AGENT_BRIDGE_PATH` to an absolute path on your dev
  checkout's `dist/bridge/index.js`, or `npm install -g oat-browser-agent`
  to put the shim on `$PATH`.
- **Tools load but every call returns "trustLocalhost disabled".** The
  Chrome extension is not getting the per-launch session token. Confirm
  that `~/Library/Application Support/Google/Chrome/NativeMessagingHosts/com.oat.browser_agent.json`
  exists (on macOS; analogous paths on Linux / Windows). If it does not,
  run `npm run install-host` in `oat-browser-agent` and reload Chrome.
  If it does, check the bridge log for `nm-broker` lines -- if the
  broker isn't pushing `oat_session_init`, the extension's first-run
  badge in the side panel will surface the failure with a more specific
  reason.
- **The browser audit log isn't where I expect.** Check
  `OAT_BROWSER_AGENT_AUDIT_LOG_DIR` in `.oat/mcp.json`. The daemon
  always sets it to `~/.oat/output/<repo>`; an older config that
  pre-dated Part 4 may still point at the legacy
  `<worktree>/.oat-logs/`. `oat agent restart browser-agent` regenerates.
