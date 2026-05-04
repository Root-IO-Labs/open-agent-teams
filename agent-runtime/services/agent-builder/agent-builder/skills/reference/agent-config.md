# deep-loom-kit Agent Config Reference

## Models

Bare model names are auto-prefixed with `anthropic:`. Provider-prefixed names pass through as-is.

```yaml
model: claude-sonnet-4-6          # Anthropic Claude Sonnet 4.6 — default, recommended
model: claude-opus-4-6            # Anthropic Claude Opus 4.6 — most capable, slower
model: claude-haiku-4-5           # Anthropic Claude Haiku 4.5 — fast, low cost
model: openai:gpt-4o              # OpenAI GPT-4o (requires OPENAI_API_KEY)
model: openai:gpt-4o-mini
model: moonshot:moonshot-v1-8k    # Moonshot (requires MOONSHOT_API_KEY)
```

Aliases: `default_model` → `model`, `default_temperature` → `temperature`

## Temperature

```yaml
temperature: 0      # precise, structured tasks (JSON output, analysis, code)
temperature: 0.3    # natural prose (BRDs, summaries, reports)
temperature: 0.7    # creative generation (brainstorming, ideation)
```

Range: `0.0`–`1.0`

---

## Middleware

Wraps every model call and tool call. Applied in order (before hooks first-to-last,
after hooks last-to-first, wrap hooks outermost-first).

```yaml
middleware:
  - logger    # logs before/after each model call and tool call
  - retry     # retries failed model calls with backoff
```

Middleware modules live in `middleware_dir/` (default: `<service-dir>/middleware/`).

**Subagents do NOT inherit middleware** — declare it explicitly on each agent.

Custom middleware:
```python
# middleware/my_middleware.py
from langchain.agents.middleware.types import AgentMiddleware

class MyMiddleware(AgentMiddleware):
    async def before_agent(self, state, runtime): ...
    async def after_agent(self, state, runtime): ...
    async def before_model(self, state, runtime): ...
    async def after_model(self, state, runtime): ...
    async def wrap_model_call(self, call, state, runtime): ...
    async def wrap_tool_call(self, call, tool, state, runtime): ...
```

`DatetimeMiddleware` is auto-prepended to all agents (injects current date/time into
system prompt). Disable with `datetime_awareness: false`.

---

## Human-in-the-loop (`interrupt_on`)

Pause execution before a specific tool is called, waiting for human approval.
Resume by calling `runtime.invoke([])` with an empty messages list.

```yaml
interrupt_on:
  write_file: true      # pause before every write_file call
  run_command: true     # pause before every shell command
  my_tool: false        # explicit false = no pause (optional, clarity only)
```

---

## Compiled agents

Pre-built LangGraph runnables used as subagents alongside YAML-defined agents.

```yaml
# orchestrator.yaml
compiledagents:
  - analysis_pipeline   # module name in compiled_agents/ dir
```

Module format:
```python
# compiled_agents/analysis_pipeline.py
from langchain_core.runnables import RunnableLambda

analysis_pipeline = {
    "name": "analysis_pipeline",
    "description": "Runs the full data analysis pipeline",
    "runnable": RunnableLambda(lambda x: {"result": "done"}),
}
```

---

## LangSmith tracing

```yaml
# main.yaml
service:
  langsmith_project_name: my-service   # routes traces to this LangSmith project
```

Requires `LANGCHAIN_API_KEY` in the environment.
`create_runtime()` sets `LANGCHAIN_PROJECT` and `LANGCHAIN_TRACING_V2=true` automatically.

---

## Runtime context injection

For multi-tenant or user-scoped isolation. Define a dataclass and declare it in YAML.

```yaml
# orchestrator.yaml
context_schema: myapp.contexts.UserContext
```

```python
import dataclasses

@dataclasses.dataclass
class UserContext:
    tenant_id: str
    user_id: str

# At invoke time:
ctx = UserContext(tenant_id="acme", user_id="u-123")
result = await runtime.invoke([{"role": "user", "content": "..."}], context=ctx)
```

Use `isolation.mode: context` + `variable: tenant_id` in backend config to namespace
storage per context variable value.

---

## Complete AgentConfig field list

```yaml
# Identity
name: orchestrator          # required; kebab-case recommended
description: |              # optional; used by orchestrator to decide when to delegate
  What this agent does.

# Model
model: claude-sonnet-4-6    # bare → auto-prefixed anthropic:
temperature: 0.0            # 0.0–1.0

# System prompt
system_prompt: |            # alias: prompt
  You are an orchestrator.

# Skills
skills:
  - research                # orchestrator: /skills/research/SKILL.md
  - planner/strategy        # subagent 'planner': /skills/planner/strategy/SKILL.md

# Tools
tools:
  - web_search
  - mcp:github/*

# Middleware
middleware:
  - logger
  - retry

# Datetime in system prompt (default: true)
datetime_awareness: true

# Subagents (orchestrator only)
subagents:
  - planner.yaml
  - writer.yaml

# Compiled agents (orchestrator only)
compiledagents:
  - analysis_pipeline

# MCP servers (orchestrator only — subagent MCP not wired)
mcp_servers:
  - name: playwright
    type: stdio
    command: npx
    args: ["@playwright/mcp@latest"]

# Backend (orchestrator only — informational on subagents)
backend:
  type: local_shell
  root_dir: ./workspace
  virtual_mode: true
  timeout: 120
  max_output_bytes: 100000
  inherit_env: true

# Human-in-the-loop
interrupt_on:
  write_file: true

# Runtime context schema
context_schema: myapp.contexts.UserContext

# Production store (PostgresStore)
store:
  type: postgres
  connection_string_env: DATABASE_URL
  pool_size: 5
  max_overflow: 10
```
