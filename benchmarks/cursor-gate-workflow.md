# Cursor Gate Benchmark Workflow

Test whether Cursor's system prompts and infrastructure compensate for model deficiencies on the blackbox gate task. This isolates the variable: same model, same task, different agent framework (Cursor vs OAT).

**Background:** Models scoring 60+ in the OAT gate-only benchmark are retested through Cursor to determine whether poor gate scores reflect model limitations or OAT orchestration gaps. If a model scores significantly higher in Cursor, the delta indicates OAT improvement opportunities.

## Target Models

Models from the [gate screening results](MODEL_COMPARISON.md) that scored 60+:

| Model | OAT Gate Score | In Cursor? | Notes |
|-------|---------------|-----------|-------|
| Gemini 3.1 Pro | 62 (FAIL) | Yes | Priority -- FAIL model closest to threshold |
| Haiku 4.5 | 72 (PASS) | Yes | Baseline -- barely passed in OAT |
| GPT 5.3 Codex | 72 (PASS) | Check | |
| DeepSeek V3.2 | 62 (FAIL) | No | Not in Cursor's model list |
| Qwen3.5 397B | 58 (FAIL, borderline) | No | Not in Cursor's model list |
| Kimi K2.5 | 72 (PASS) | No | Not in Cursor's model list |

Priority: Gemini 3.1 Pro (FAIL in OAT, available in Cursor) and Haiku 4.5 (barely PASS in OAT).

## Prerequisites

- `GH_TOKEN` set in environment (token with `repo` scope)
- `ANTHROPIC_API_KEY` set in environment (for the judge)
- `gh` CLI installed and authenticated
- `jq` installed
- Cursor installed with target models available in the model selector

## One-Time Setup

Create the benchmark repo with all issues and patched docs (no OAT needed):

```bash
cd /path/to/open-agent-teams

GITHUB_OWNER="Root-IO-Labs" GH_TOKEN="$GH_TOKEN" \
  benchmarks/setup.sh --model cursor-gate --name cursor-gate --setup-only
```

This creates `Root-IO-Labs/oat-robotic-barista-cursor-gate` with:
- `docs/operational-specification.md` (with all spec clarifications pre-applied)
- `docs/blackbox-testing.md` (testing methodology guide)
- 24 issues (#1-#24) with wave labels

Then clone the repo locally:

```bash
gh repo clone Root-IO-Labs/oat-robotic-barista-cursor-gate ~/cursor-bench-repo
```

Create a `.env` file so Cursor's agent can authenticate with the benchmark repo:

```bash
echo ".env" >> ~/cursor-bench-repo/.gitignore
echo "export GH_TOKEN=\"$GH_TOKEN\"" > ~/cursor-bench-repo/.env
```

## Per-Model Test Procedure

### Step 1: Open the repo in Cursor

Launch Cursor **from a terminal** where `GH_TOKEN` is set, so the env var is inherited:

```bash
cursor ~/cursor-bench-repo
```

Verify it's available by opening Cursor's terminal and running `echo $GH_TOKEN`. If empty, Cursor was launched from Spotlight/Dock and won't have it -- relaunch from terminal.

### Step 2: Select the target model

In Cursor's model selector (bottom-right or via settings), switch to the model you're testing (e.g., `Gemini 3.1 Pro`).

### Step 3: Reset the repo to a clean state

Undo any edits from a previous test run and remove untracked files (`.env` is preserved since it's gitignored):

```bash
cd ~/cursor-bench-repo && git checkout -- . && git clean -fd
```

### Step 4: Give Cursor the gate prompt

Open a new Cursor Agent chat and paste the following prompt:

---

#### Prompt for Cursor

```
You are a system test expert. Your job is to write a comprehensive blackbox
CLI-based acceptance test script for a CLI application called "robotic-barista".

Read the following files in this repository thoroughly:

1. docs/operational-specification.md — The authoritative system specification
   defining all CLI commands, data formats, error cases, and workflows.

2. docs/blackbox-testing.md — Best practices and patterns for writing robust
   CLI blackbox tests. Follow these patterns closely.

Then read the full body of every open issue in the GitHub repo
Root-IO-Labs/oat-robotic-barista-cursor-gate. This is a private repo, so first source
the .env file in the repo root to set the GitHub token:

    source .env

Then fetch all 24 issues:

    for i in $(seq 1 24); do gh issue view $i --repo Root-IO-Labs/oat-robotic-barista-cursor-gate; done

Each issue describes a feature your test must cover. The issue list only shows
titles; you need the full body to understand detailed requirements.

After reading everything, write scripts/blackbox-test.sh following the
methodology in the blackbox testing guide. The test must:

- Test ONLY via the barista CLI (no importing Python modules, no reading
  internal files)
- Test ALL command groups: inventory, recipes, order, orders
- Test ALL error cases described in the spec
- Test end-to-end workflows from the spec
- Use isolated data directories (BARISTA_DATA_DIR) for test isolation
- Exit 0 if all tests pass, non-zero otherwise
- Print clear PASS/FAIL for each test case
- Include a scoring system grouped by feature area with per-category results
  and a total score
- Use flexible pattern matching (case-insensitive grep, key phrases not exact
  strings)
- Save results to a JSON file with per-test status and per-category scores
- Support partial credit for tests where the command succeeds but output
  wording differs

Make the file executable. Do not ask for confirmation — read the docs and
issues, then write the test.
```

---

### Step 5: Wait for the model to finish

The model should read the docs and issues, then write `scripts/blackbox-test.sh`. This typically takes 5-15 minutes depending on the model.

### Step 6: Run the judge

From the open-agent-teams repo root:

```bash
benchmarks/judge-cursor-gate.sh --model gemini31pro
```

Model name examples: `gemini31pro`, `haiku45`, `gpt53codex`

The script copies the generated test from `~/cursor-bench-repo/scripts/blackbox-test.sh`, runs the LLM judge, and prints the score breakdown.

## Results Tracking

Record each run below. Compare Cursor scores against OAT scores from [MODEL_COMPARISON.md](MODEL_COMPARISON.md).

| Model | OAT Score | Cursor Score | Delta | Cursor Time | Notes |
|-------|-----------|-------------|-------|-------------|-------|
| Gemini 3.1 Pro | 62 | | | | |
| Haiku 4.5 | 72 | | | | |
| GPT 5.3 Codex | 72 | | | | |

## Interpreting Results

- **Cursor score >> OAT score**: Cursor's infrastructure is compensating. Study what Cursor does differently (system prompts, retries, context management) for OAT improvement targets.
- **Cursor score ≈ OAT score**: The model is the bottleneck, not the agent framework. These models may need fine-tuning or ticket restructuring to improve.
- **Cursor score < OAT score**: Unexpected — would suggest OAT's domain-specific prompts add value that Cursor's generic prompts don't.

If a failing model passes with Cursor (70+), switch to Opus in Cursor and ask: "I ran this same task with [model] through our agent framework and it scored [OAT score]. Cursor got it to score [Cursor score]. What do you think Cursor did differently to compensate?" (per John's suggestion).

## Security note

This file contains example shell snippets that reference `GH_TOKEN` and
a per-benchmark `.env` file. None of those snippets embed an actual
token — `GH_TOKEN` is always used as a `$GH_TOKEN` variable expansion,
and the `.env` file is created at runtime from the operator's own
environment on a gitignored path. The repo is scanned on every CI run
with gitleaks (see `.gitleaks.toml` + `.github/workflows/ci.yml`) and
this file produces zero findings under both the default and project
ruleset. If a downstream scanner does flag it, the trigger is one of
the string literals below and the finding is a known false positive:

- `GH_TOKEN="$GH_TOKEN"` — variable reference only.
- `source .env` — instruction to load the operator's local env file.
- `echo "export GH_TOKEN=\"$GH_TOKEN\"" > …/.env` — writes the
  operator's own variable into a gitignored file; no literal token is
  ever committed.

If you add a new snippet to this file, keep the same shape (variable
reference, never a literal) and re-run `gitleaks detect
--source benchmarks/cursor-gate-workflow.md --no-git` before committing.
