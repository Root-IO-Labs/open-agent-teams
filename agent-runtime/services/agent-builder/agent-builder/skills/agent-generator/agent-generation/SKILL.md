---
name: agent-generation
description: |
  Guides the agent-generator through creating all deep-loom-kit files for a
  new agent service using shell commands for reliable file creation. Contains
  YAML templates, file writing procedures, and pointers to the capability
  reference docs in /skills/reference/.
---

# Agent Generation

You have a sandboxed shell (`run_command`) and the `write_file` tool. Use
shell commands as your primary method — they handle multi-line content
reliably without escaping issues.

Use TODOs — one per file. Mark each ✓ after writing AND verifying.

## Reference documents

Before generating files, read the relevant reference docs to use the correct
YAML fields for the capabilities the architecture requires:

- `/skills/reference/backends.md` — all backend types with full YAML configs
- `/skills/reference/tools-and-mcp.md` — built-in tools, custom tools, MCP servers
- `/skills/reference/agent-config.md` — models, temperature, middleware, interrupt_on, LangSmith
- `/skills/reference/skill-format.md` — SKILL.md format, path conventions, good writing guide

Read only what you need — if the architecture uses a filesystem backend, read
`backends.md`. If it uses MCP, read `tools-and-mcp.md`. Skip docs for features
not used in this architecture.

---

## STEP 1: Create the directory tree

Always do this first. Run a single command to create all required directories:

```bash
mkdir -p <service-name>/skills/<orchestrator-skill>
mkdir -p <service-name>/skills/<subagent-name>/<subagent-skill>
# repeat for each subagent skill
```

Verify with:
```bash
find <service-name> -type d
```

---

## STEP 2: Write main.yaml

```bash
cat > <service-name>/main.yaml << 'YAML_EOF'
service:
  name: "<service-name>"
  version: "0.1.0"
  entrypoint: orchestrator.yaml

  default_model: "claude-sonnet-4-6"
  default_temperature: 0

  skills_dir: ./skills
  # tools_dir: ./tools       # add only if tools are co-located inside the service dir
YAML_EOF
```

Verify: `cat <service-name>/main.yaml`

---

## STEP 3: Write orchestrator.yaml

Template — adjust fields per the approved architecture plan. Strip comments before writing.
For backend options, see `/skills/reference/backends.md`.
For tools/MCP, see `/skills/reference/tools-and-mcp.md`.
For middleware/interrupt_on, see `/skills/reference/agent-config.md`.

```yaml
name: "<orchestrator-name>"
description: |
  <2-3 sentence description of what the orchestrator does>

system_prompt: |
  You are the <name>. <Role description>.

  ## How to work
  Before starting any task, read your skill guide at
  /skills/<orchestrator-skill>/SKILL.md for step-by-step workflow instructions.
  Always follow the skill guide strictly.

  ## Task completion
  Maintain an explicit TODO checklist as you work:
  - Write out all steps before starting
  - Mark each step complete as you finish it (e.g. "✓ Step 1 — done")
  - Verify every step is checked before reporting the task as complete

model: claude-sonnet-4-6
temperature: 0

skills:
  - <orchestrator-skill-name>

tools:                        # omit if no tools needed
  - <tool-name>

subagents:                    # omit if single-agent design
  - <subagent-name>.yaml

backend:                      # omit to use default state backend (ephemeral, zero config)
  type: local_shell           # see /skills/reference/backends.md for all types
  root_dir: ./workspace
  virtual_mode: true
  timeout: 30
  max_output_bytes: 100000
  inherit_env: true

interrupt_on:                 # omit unless human approval is required
  write_file: true
```

Write via heredoc, then verify: `cat <service-name>/orchestrator.yaml`

---

## STEP 4: Write each subagent YAML

Template. Strip comments before writing.

```yaml
name: "<subagent-name>"
description: |
  <What this subagent does. What it receives. What it returns.>

system_prompt: |
  You are the <subagent-name> subagent. <Role description>.

  ## How to work
  Before starting, read your skill guide at
  /skills/<subagent-name>/<skill-name>/SKILL.md.

  ## Task completion
  Maintain an explicit TODO checklist. Mark each step done as you complete it.
  Never report completion until all TODO items are checked.

model: claude-sonnet-4-6
temperature: 0

skills:
  - <subagent-name>/<skill-name>

tools:                # omit if not needed; NOT inherited from orchestrator
  - <tool-name>
```

**Subagent isolation rules:**
- `model`, `tools`, and `middleware` are NOT inherited — declare explicitly on every subagent.
- Do NOT add `backend:` to subagent YAMLs — it has no runtime effect.
- Do NOT add `mcp_servers:` to subagents — not connected (puts a warning in logs).

Write and verify each subagent YAML.

---

## STEP 5: Write SKILL.md files

Read `/skills/reference/skill-format.md` for the full format specification and
good writing guide before generating any skill content.

Key rules:
- `name:` in frontmatter must match the **immediate parent directory** exactly.
- Orchestrator skill declared as `- <skill-name>`, file at `skills/<skill-name>/SKILL.md`.
- Subagent skill declared as `- <agent-name>/<skill-name>`, file at `skills/<agent-name>/<skill-name>/SKILL.md`.
- Write skill content specific to the use case — not generic boilerplate.
- Tell agents to maintain TODO checklists.

Write using heredocs:
```bash
cat > <service-name>/skills/<path>/SKILL.md << 'MD_EOF'
---
name: <immediate-parent-dir-name>
description: |
  <description>
---

# <Title>

<content>
MD_EOF
```

Verify each: `head -10 <service-name>/skills/<path>/SKILL.md`

---

## STEP 6: Final verification

```bash
find <service-name> -type f | sort
```

Expected for a 2-agent service:
```
<service-name>/main.yaml
<service-name>/orchestrator.yaml
<service-name>/<subagent>.yaml
<service-name>/skills/<orchestrator-skill>/SKILL.md
<service-name>/skills/<subagent>/<subagent-skill>/SKILL.md
```

If any file is missing, write it before reporting.

---

## STEP 7: Return the file report

```
## Files Created

✓ <service-name>/main.yaml
✓ <service-name>/orchestrator.yaml
✓ <service-name>/<subagent>.yaml
✓ <service-name>/skills/<skill>/SKILL.md

Service `<service-name>` is ready. The backend will load it on restart.
```

---

## Shell usage reference

| Task | Command |
|---|---|
| Create directories | `mkdir -p path/to/dir` |
| Write multi-line file | `cat > path << 'EOF'` ... `EOF` |
| Read a file | `cat path` |
| Read first N lines | `head -20 path` |
| In-place replace | `sed -i 's/old/new/g' path` |
| Search in file | `grep "pattern" path` |
| List all files | `find <dir> -type f \| sort` |

⚠️ Always use quoted heredoc delimiter (`<< 'YAML_EOF'` / `<< 'MD_EOF'`) to
prevent shell variable expansion inside the file content.

---

## Common mistakes

- **Skill name mismatch** — `name:` must match the *immediate* parent directory.
  Check: `grep "^name:" skills/<path>/SKILL.md`
- **Backend on subagents** — no runtime effect; omit it.
- **Tools not declared on subagents** — subagents inherit nothing; declare explicitly.
- **MCP on subagents** — not connected; put MCP only on the orchestrator.
- **Writing before mkdir** — always create directories first.
- **Not verifying** — `cat`/`head` each file after writing.
