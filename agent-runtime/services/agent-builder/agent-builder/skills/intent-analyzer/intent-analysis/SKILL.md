---
name: intent-analysis
description: |
  Guides the intent-analyzer through reasoning about a user's goal, proposing
  the minimum clean agent architecture, flagging ambiguities, and surfacing
  targeted open questions for the orchestrator.
---

# Intent Analysis

You receive a structured requirements block from the orchestrator. Your job
is to **reason about the goal** and produce a concrete architecture proposal
with targeted open questions — not just reformat what you received.

Use TODOs to track your steps.

## Reference documents

Use these to recommend the right deep-loom-kit capabilities for the architecture:

- `/skills/reference/backends.md` — all backend types (state, filesystem, local_shell, s3, composite, store)
- `/skills/reference/tools-and-mcp.md` — built-in tools, custom tools, MCP servers
- `/skills/reference/agent-config.md` — models, middleware, interrupt_on, compiled agents, LangSmith
- `/skills/reference/skill-format.md` — how skills work and what makes a good skill

Read only what is relevant to the stated goal. If the user needs shell execution,
read `backends.md`. If they mention web browsing or GitHub integration, read
`tools-and-mcp.md`.

---

## STEP 1: Understand the goal

Read the requirements block carefully. Ask yourself:
- What is the user actually trying to accomplish?
- What does "done" look like from the user's perspective?
- What inputs does the agent need to receive? What outputs does it produce?
- Does it need to persist anything? Run shell commands? Call external APIs?

Write a one-sentence goal statement before proceeding.

---

## STEP 2: Identify what is unclear or missing

Flag items in three categories:

**AMBIGUOUS** — stated but underspecified (e.g. "search the web" — one-off or persistent results?)

**MISSING** — not mentioned but needed for a working design (e.g. no mention of where output is saved)

**DESIGN RISK** — what the user asked for might not be the best approach (e.g. asked for one agent but the task clearly has two distinct phases that benefit from separate agents)

Not every category needs items. Only flag genuinely unclear things.

---

## STEP 3: Propose the architecture

Design the minimum number of agents that cleanly achieve the goal.

Rules:
- Prefer 1 agent (no subagents) if the task is single-phase and focused.
- Add a subagent only when there is a clear separation of concerns:
  - Different data access needs (e.g. one reads files, one reasons)
  - Different output types (e.g. one analyzes, one writes)
  - Tasks that could run in parallel
- Never add agents just to have more agents.
- Use `local_shell` only if the agent genuinely needs to execute shell
  commands or write real files. Prefer `state` (the default) for pure reasoning.
- The orchestrator always owns the backend config and subagent declarations.
  Subagents in deep-loom-kit do NOT inherit backend from the orchestrator.

For each agent, specify:
- `name` (kebab-case)
- `description` (1–2 sentences)
- `model` (default: `claude-sonnet-4-6`)
- `temperature` (default: `0`; use `0.3` for creative/prose tasks)
- `skills` (list of skill names the agent will use)
- `subagents` (list of `.yaml` file references, orchestrator only)
- `backend` (type + config, orchestrator only)

For each skill, specify:
- Which agent it belongs to
- The skill name (kebab-case, describes its function)
- What the skill guide should teach the agent to do

---

## STEP 4: Formulate open questions

Write at most 5 targeted questions that the orchestrator should relay to the
user. Each question should:
- Reference a specific ambiguity or gap identified in STEP 2
- Explain *why* it matters for the design (one clause is enough)
- Not be answerable by reasonable inference from the stated goal

Bad: "What tools does it need?"
Good: "Does it need to remember results between separate conversations, or is
each session self-contained? (This determines whether we need a filesystem
backend or can use the default in-memory state.)"

If nothing is genuinely unclear, write: `Open Questions: none — proceeding
with the architecture as designed.`

---

## Output format (strict)

```
## Draft Architecture

### Service: <kebab-case-name>
description: <one sentence>
entrypoint: orchestrator.yaml

### Agents

#### <orchestrator-name>
- description: ...
- model: claude-sonnet-4-6
- temperature: 0
- skills: [<skill-name>]
- subagents: [<subagent>.yaml, ...]  # omit if none
- backend:
    type: <state|filesystem|local_shell>
    root_dir: <path>                  # only for filesystem/local_shell
    virtual_mode: true                # only for filesystem/local_shell
    timeout: 30                       # only for local_shell
    max_output_bytes: 100000          # only for local_shell
    inherit_env: true                 # only for local_shell

#### <subagent-name>  # repeat for each subagent
- description: ...
- model: claude-sonnet-4-6
- temperature: 0
- skills: [<agent-name>/<skill-name>]

### Skills

| Agent | Skill | Purpose |
|---|---|---|
| <agent-name> | <skill-name> | <what the skill guide teaches the agent to do> |

## Open Questions
1. ...

## Design Notes
- <trade-offs, alternative approaches considered, or suggestions>
```

---

## Backend decision guide

| Need | Backend |
|---|---|
| Pure reasoning, no persistent I/O | `state` (default — omit backend key) |
| Read/write files that survive across sessions | `filesystem` |
| Write real files AND run shell commands | `local_shell` |
| Store structured data across threads | `store` |
| Different areas need different storage | `composite_backend` |
| Cloud-scale file storage | `s3` |

See `/skills/reference/backends.md` for full YAML configs for each backend type.
