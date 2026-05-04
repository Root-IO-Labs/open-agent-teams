# deep-loom-kit Tools & MCP Reference

## Built-in tools (available based on backend type)

These tools are automatically available when the corresponding backend is configured.
You do NOT need to declare them in `tools:` — they are injected by the SDK.

| Tool | Available when |
|---|---|
| `read_file` | backend: filesystem, local_shell, s3, store, or composite |
| `write_file` | backend: filesystem, local_shell, s3, or composite |
| `list_files` | backend: filesystem, local_shell, s3, or composite |
| `run_command` | backend: local_shell (or composite with local_shell route) |
| `search_store` | backend: store (or composite with store route) |

`run_command` executes shell commands in the sandboxed root_dir. Available tools:
`mkdir`, `cat`, `sed`, `grep`, `awk`, `head`, `tail`, `find`, `ls`, `cp`, `mv`,
`chmod`, `echo`, `wc`, `sort`, `uniq`, `diff`, `tar`, `curl`, and any other
program on the host PATH (when `inherit_env: true`).

---

## Custom tools (Python)

Place Python files in `tools_dir` (default: `<service-dir>/../tools/`, or set
`tools_dir: ./tools` in `main.yaml` for co-located tools).

The SDK imports each file and registers every public function automatically.

```python
# tools/summarise.py
def summarise(text: str, max_words: int = 200) -> str:
    """Summarise the given text in at most max_words words."""
    ...
```

Rules:
- Function must have a docstring (used as the tool description).
- Function name = tool name used in YAML `tools:` list.
- `register_tool("name", fn, description="...")` for manual registration.

Accessing thread_id inside a tool:
```python
from typing import Annotated
from langchain_core.runnables import RunnableConfig
from langchain_core.tools import InjectedToolArg
from deep_loom_kit import get_thread_id

def my_tool(text: str, config: Annotated[RunnableConfig, InjectedToolArg]) -> str:
    """Do something with thread awareness."""
    thread_id = get_thread_id(config)
    ...
```

Declare tools in agent YAML:
```yaml
tools:
  - summarise
  - web_search
```

---

## Tool isolation (critical)

Subagents do **NOT** inherit tools from the orchestrator. Every tool must be
declared explicitly in each agent's YAML that needs it.

```yaml
# orchestrator.yaml
tools:
  - web_search
  - summarise

# subagent.yaml — must declare its own tools; inherits nothing
tools:
  - summarise       # web_search NOT available here unless explicitly listed
```

---

## MCP Servers

Connects agents to external tool servers over stdio or HTTP/SSE.
Requires: `pip install deep-loom-kit[mcp]`

**IMPORTANT: Declare `mcp_servers` on the orchestrator only.** Subagent
MCP config is parsed but NOT connected (a warning is emitted at runtime).

### stdio subprocess

```yaml
mcp_servers:
  - name: playwright
    type: stdio
    command: npx
    args: ["@playwright/mcp@latest", "--headless"]
    env:                          # optional env for the subprocess
      DISPLAY: ":0"
    max_connections: 10           # LRU pool cap (default: 10)
```

### HTTP / SSE

```yaml
mcp_servers:
  - name: github
    type: http
    url: "${env.GITHUB_MCP_URL}"  # resolved at create_runtime()
    headers:
      Authorization: "Bearer ${env.GITHUB_TOKEN}"
      X-Thread-Id: "${thread_id}" # resolved per invoke()/stream()
```

### Template variables in MCP config

| Syntax | Resolved at | Raises if missing |
|---|---|---|
| `${env.VAR}` | `create_runtime()` | `ValueError` immediately |
| `${thread_id}` | each `invoke()`/`stream()` call | `ValueError` at call time |
| `${context.field}` | each `invoke()`/`stream()` call | `ValueError` at call time |

Configs with `${thread_id}` or `${context.X}` are **dynamic** — a new connection
is made per invocation (stdio: LRU pool; HTTP: fresh client per call).
Configs with only `${env.VAR}` are **static** — connected once at `create_runtime()`.

### Referencing MCP tools in `tools:`

```yaml
tools:
  - mcp:playwright/browser_navigate      # specific tool from server
  - mcp:playwright/browser_screenshot
  - mcp:github/*                         # wildcard: all tools from server
```

MCP tools appear in the agent's tool list as `mcp:<server>/<tool_name>`.
