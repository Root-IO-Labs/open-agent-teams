# deep-loom-kit Skill Format Reference

## What is a skill?

A skill is a markdown file that serves as the agent's instruction manual for a
specific task. The SDK loads it lazily from disk and makes it available at a
virtual path the agent can read via `read_file`. Skills are **read-only** —
agents cannot write to `/skills/*`.

---

## File format

```markdown
---
name: research
description: Use this skill to research topics and summarise findings
---

# Research Skill

## Steps
1. Gather sources
2. Synthesise results
```

Rules:
- Frontmatter is **required**. Must be the first thing in the file.
- `name:` must match the **immediate parent directory name** exactly (case-sensitive).
- File must be named `SKILL.md` exactly.
- `description:` is used by the agent to decide when to use the skill.

---

## Directory layout and path conventions

```
<service>/
  skills/
    <orchestrator-skill>/
      SKILL.md            → /skills/<orchestrator-skill>/SKILL.md
    <subagent-name>/
      <subagent-skill>/
        SKILL.md          → /skills/<subagent-name>/<subagent-skill>/SKILL.md
    reference/
      backends.md         → /skills/reference/backends.md  (readable, not a skill)
```

### Orchestrator skills
- Disk path: `skills/<skill-name>/SKILL.md`
- Virtual path: `/skills/<skill-name>/SKILL.md`
- Declared in orchestrator YAML: `skills: [<skill-name>]`
- `name:` in frontmatter: `<skill-name>`

### Subagent skills
- Disk path: `skills/<agent-name>/<skill-name>/SKILL.md`
- Virtual path: `/skills/<agent-name>/<skill-name>/SKILL.md`
- Declared in subagent YAML: `skills: [<agent-name>/<skill-name>]`
- `name:` in frontmatter: `<skill-name>` (immediate parent, NOT `<agent-name>/<skill-name>`)

### Example (name: matching)

```
skills/
  research/
    SKILL.md              → name: research
  planner/
    content-strategy/
      SKILL.md            → name: content-strategy   ← NOT "planner/content-strategy"
```

---

## Writing good skill content

Skills are the agent's operating manual. Write them to be concrete and
goal-oriented. Bad skills produce unfocused agents.

### Structure to follow

```markdown
---
name: <skill-name>
description: |
  One or two sentences: when is this skill used and what does it help the agent do.
---

# <Skill Title>

<One paragraph stating the goal of this agent in this skill.>

## STEP 1: <First action>
<Clear instruction. What to do, how, and why.>

## STEP 2: <Second action>
<Instruction. Include any decision rules the agent needs.>

## Output format
<Describe exactly what output the agent should produce — format, structure, field names.>

## Constraints
- <What the agent must NOT do>
- <Edge cases and guard rails>
```

### Principles

**Be concrete.** "Search for relevant documents" is vague. "Use `list_files` to
scan `workspace/` and present the file list to the user before reading anything"
is actionable.

**Match the use case.** A skill for a BRD writer is different from a skill for
a code reviewer. Don't copy-paste boilerplate — write for the specific task.

**Include output formats.** If the agent produces a structured block (JSON,
markdown table, handoff block), show the exact template in the skill.

**State constraints clearly.** "Never generate the BRD yourself — always delegate
to brd-writer" is a constraint that prevents common mistakes.

**Use TODO guidance.** Tell agents to maintain a TODO checklist and mark steps
as ✓ when done. This dramatically reduces incomplete outputs.

### Example: orchestration skill

```markdown
---
name: summarisation-orchestration
description: |
  Step-by-step workflow for summarising meeting recordings.
  Read this before starting any summarisation task.
---

# Meeting Summarisation Workflow

Guide the user through summarising a meeting recording from upload to final report.

## STEP 1: Confirm the input
Ask the user to confirm which file to process before reading anything.
List available files in `workspace/recordings/` with `list_files`.

## STEP 2: Delegate transcription
Pass the confirmed file path to the `transcriber` subagent.
Wait for the transcript before proceeding.

## STEP 3: Delegate summarisation
Pass the full transcript to the `summariser` subagent.
Include any focus areas the user mentioned.

## STEP 4: Save and confirm
Write the summary to `workspace/summaries/<meeting-name>-summary.md`.
Confirm the save path to the user.

## Constraints
- Do not read or interpret the transcript yourself — pass it to `summariser`.
- Do not overwrite an existing summary without asking the user first.
```

---

## Reference files (non-skill docs)

Any `.md` file in `skills_dir` is accessible at its virtual path, not just
`SKILL.md` files. Use a `reference/` subdirectory for shared reference docs
that multiple skills can point to:

```
skills/
  reference/
    backends.md         → /skills/reference/backends.md
    tools-and-mcp.md    → /skills/reference/tools-and-mcp.md
    agent-config.md     → /skills/reference/agent-config.md
    skill-format.md     → /skills/reference/skill-format.md
```

Reference these from skill files:
```markdown
For backend configuration options, read `/skills/reference/backends.md`.
```

These are NOT skills themselves — they have no frontmatter and are not declared
in any agent's `skills:` list. They are plain documentation the agent reads on demand.
