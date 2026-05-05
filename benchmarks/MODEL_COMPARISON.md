# Benchmark Model Comparison

Cross-model comparison of OAT benchmark results on the robotic-barista project (bundled in `benchmarks/robotic-barista/`). Each model is tested on the same project with identical operational specifications, issue definitions, and acceptance tests.

**Note:** Starting March 2026, benchmark runs include a **blackbox gate** phase where the model generates a blackbox acceptance test from the spec before proceeding. Models that fail the gate (score below threshold) do not run the full benchmark -- their wave/acceptance metrics show "N/A". Runs before the gate was introduced show "N/A" for gate metrics.

**Issue numbering change (April 2026):** The benchmark was updated from 21 issues to 24 issues. The gate phase was split from a single issue (#1) into 4 parallel gate issues (#1-#4, labeled wave:0), and all subsequent issues were renumbered (+3 offset). Waves were also renumbered: old wave:0-3 became wave:1-4. Runs before this change used 21 issues (#1-#21, gate=#1, wave:0=#2-#6). Runs after use 24 issues (#1-#24, gate=#1-#4 (wave:0), wave:1=#5-#9). Historical descriptions below use the numbering from the actual run.

This is a living document -- new models are added as they are benchmarked.

> **Note:** Links to `results/` folders throughout this document are local references for people who have run benchmarks. The `results/` directory is gitignored and not included in the repository.

## Table of Contents

- [Overview](#overview)
- [Gate Screening Results](#gate-screening-results)
- [Cursor Gate Comparison](#cursor-gate-comparison)
- [Reasoning Effort Parameter Analysis](#reasoning-effort-parameter-analysis)
- [Acceptance Score Breakdown](#acceptance-score-breakdown)
- [Per-Model Details](#per-model-details)
  - [Claude Opus 4.6](#claude-opus-46)
  - [Claude Sonnet 4.6](#claude-sonnet-46)
  - [Claude Haiku 4.5](#claude-haiku-45)
  - [Claude Haiku 4.5 (v2)](#claude-haiku-45-v2----with-convergence)
  - [GPT 5.4](#gpt-54)
  - [GPT 5.3 Codex](#gpt-53-codex)
  - [Gemini 3.1 Pro Preview](#gemini-31-pro-preview)
  - [Gemini 3.1 Flash-Lite Preview](#gemini-31-flash-lite-preview)
  - [DeepSeek V3.2](#deepseek-v32)
  - [DeepSeek V3.2 Nitro](#deepseek-v32-nitro)
  - [DeepSeek V3.2 Speciale](#deepseek-v32-speciale-aborted)
  - [Qwen3.5 397B-A17B](#qwen35-397b-a17b)
  - [Qwen3 Coder Next](#qwen3-coder-next-aborted)
  - [Kimi K2.5](#kimi-k25)
  - [Kimi K2 Thinking](#kimi-k2-thinking-aborted)
  - [Llama 4 Scout / Maverick / Maverick Nitro](#llama-4-scout--llama-4-maverick-aborted)
  - [Routed: Scout (Groq) + DeepSeek Nitro](#routed-scout-via-groq--deepseek-nitro)
  - [Routed: DeepSeek Nitro + Haiku 4.5](#routed-deepseek-nitro--haiku-45)
  - [Routed: o4-mini + Gemini Flash + Sonnet 4.6](#routed-o4-mini--gemini-flash--sonnet-46)
  - [Routed: GPT 5.4 Mini + Nano + Haiku 4.5](#routed-gpt-54-mini--nano--haiku-45)
  - [Llama 4 Maverick: Provider Investigation](#llama-4-maverick-provider-investigation)

## Overview

| Model | Acceptance | Gate | Convergence | Wall Clock | Workers | Issues Closed | PRs Merged | CI Pass | Waves | Tokens | Run Date |
|-------|-----------|------|-------------|-----------|---------|---------------|------------|---------|-------|--------|----------|
| **Claude Opus 4.6** | N/A | N/A | N/A | N/A | N/A | N/A | N/A | N/A | N/A | N/A | 2026-03-23 |
| **Claude Sonnet 4.6** | 100 / 100 | N/A | N/A | ~74 min | 30 | 20/21 (95%) | 22/30 (73%) | 28/30 (93%) | 4/4 | ~2,618K | 2026-03-19 |
| **Claude Haiku 4.5** | 59.2 / 100 | N/A | N/A | ~60 min | 21 | 20/20 (100%) | 20/21 (95%) | 20/20 (100%) | 4/4 | ~1,968K | 2026-03-18 |
| **Claude Haiku 4.5** (v2) [3] | 74.6 / 100 | 62/100 | PASS (4 iter) | ~57 min | 30 | 24/24 (100%) | 30/30 (100%) | 30/30 (100%) | 4/4 | N/A [4] | 2026-03-28 |
| **Claude Haiku 4.5** (v3) [5] | 10.8 / 100 | 62/100 | TIMEOUT (3 iter) | ~4 hrs | 27 | 16/28 (57%) | 16/27 (59%) | 16/27 (59%) | 4/4 | N/A [4] | 2026-03-28 |
| **Claude Haiku 4.5** (v4) [6] | 76.0 / 100 | 62/100 | TIMEOUT (2 iter) | ~3.5 hrs | 22 | 15/23 (65%) | 15/22 (68%) | 16/22 (73%) | 4/4 | N/A [4] | 2026-03-29 |
| **Claude Haiku 4.5** (v5) [7] | N/A | 68/100 | TIMEOUT | ~4+ hrs | 27+ | ~7/35 (20%) | ~6/27 (22%) | ~6/27 (22%) | 4/4 | N/A [4] | 2026-03-30 |
| **GPT 5.4** | 98.6 / 100 | N/A | N/A | ~52 min | 21 | 20/20 (100%) | 21/21 (100%) | 21/21 (100%) | 4/4 | ~1,075K+ [1] | 2026-03-16 |
| **GPT 5.3 Codex** | 15.8 / 100 | N/A | N/A | ~121 min | 5 | 5/20 (25%) | 5/5 (100%) | 5/5 (100%) | 1/4 | ~210K+ [1][2] | 2026-03-17 |
| **Gemini 3.1 Pro** | 2.5 / 100 | N/A | N/A | ~121 min | 20 | 7/20 (35%) | 7/20 (35%) | 17/20 (85%) | 1/4 | ~1,128K+ [8] | 2026-03-17 |
| **Gemini 3.1 Flash-Lite** | N/A | N/A | N/A | N/A | N/A | N/A | N/A | N/A | N/A | N/A | 2026-03-22 |
| **DeepSeek V3.2** | 60.9 / 100 | N/A | N/A | ~121 min | 23 | 4/20 (20%) | 5/21 (24%) | 12/21 (57%) | 4/4 | ~2,006K | 2026-03-19 |
| **DeepSeek V3.2 Nitro** | 74.8 / 100 | 62/100 | TIMEOUT (3 iter) | ~3h 46m | 45 | 22/34 (65%) | 19/38 (50%) | 24/38 (63%) | 4/4 | N/A [4] | 2026-04-06 |
| **Qwen3.5 397B-A17B** | N/A | N/A | N/A | N/A | N/A | N/A | N/A | N/A | N/A | N/A | 2026-03-22 |
| **Qwen3 Coder Next** | N/A | N/A | N/A | N/A | N/A | N/A | N/A | N/A | N/A | N/A | 2026-03-22 |
| **Kimi K2.5** | N/A | 72/100 | N/A | N/A | N/A | N/A | N/A | N/A | N/A | 103.2K | 2026-03-23 |
| **Kimi K2.5** (full) [9] | ABORT | 62/100 | N/A | ~30 min | 1 | 0/5 (0%) | 1/1 (100%) | 1/1 (100%) | 0/4 | N/A [4] | 2026-03-30 |
| **Kimi K2.5** (hybrid) [10] | ABORT | N/A | N/A | ~30 min | 1 | 0/1 (0%) | 0/0 | 0/0 | 0/4 | N/A [4] | 2026-03-30 |
| **Kimi K2 Thinking** | N/A | ABORT | N/A | N/A | N/A | N/A | N/A | N/A | N/A | 109.2K | 2026-03-23 |
| **Llama 4 Scout** | N/A | N/A | N/A | N/A | N/A | N/A | N/A | N/A | N/A | N/A | 2026-03-22 |
| **Llama 4 Maverick** | N/A | N/A | N/A | N/A | N/A | N/A | N/A | N/A | N/A | N/A | 2026-03-22 |
| **Llama 4 Maverick Nitro** | N/A | N/A | N/A | N/A | 0 | 0/21 (0%) | 0/0 | 0/0 | 0/4 | N/A | 2026-04-07 |
| **Routed: Scout (Groq) + DeepSeek Nitro** [11] | 52 / 100 | 22/100 | TIMEOUT (3 iter) | ~3h 46m | 6 | 4/24 (17%) | 4/6 (67%) | 4/6 (67%) | 1/4 | N/A [4] | 2026-04-15 |
| **Routed: DeepSeek Nitro + Haiku 4.5** [12] | 100 / 100 | 62/100 | N/A | ~92 min | 48 | 24/24 (100%) | 24/24 (100%) | 24/24 (100%) | 4/4 | ~192,054K | 2026-04-15 |
| **Routed: o4-mini + Flash + Sonnet 4.6** [13] | 95 / 100 | 72/100 | PASS (1 iter) | ~2h 28m | 42 | 26/26 (100%) | 5/42 (12%) | 24/24 (100%) | 4/4 | ~204,000K | 2026-04-17 |
| **Routed: GPT 5.4 Mini + Nano + Haiku 4.5** [14] | 100 / 100 | 78/100 | PASS (3 iter) | ~1h 57m | 27 | 24/24 (100%) | 26/27 (96%) | 26/27 (96%) | 3/4 | ~167,000K | 2026-04-16 |

**Notes:**

[1] Estimated from summary reports (raw logs unavailable). Sum of workers with reported token counts only; does not include workspace, supervisor, or merge-queue agents. Actual total is higher.

[2] GPT 5.3 Codex total is significantly lower because the workspace hallucinated spawning workers for waves 1-3, so only 5 wave-0 workers ran. See [GPT 5.3 Codex](#gpt-53-codex) details.

[3] Haiku v2 ran on `feature/integrate-verification` with worktree lifecycle fixes. The 15.4-point improvement over v1 reflects both OAT fixes and the new gate + convergence pipeline. See [Haiku v2](#claude-haiku-45-v2----with-convergence) details.

[4] Token tracking was temporarily unavailable due to a UI change. Worker token data shows "N/A".

[5] Haiku v3 scored 10.8/100 due to a **circular CI dependency**, not model capability. Wave 0 created strict contract tests requiring all CLI commands to exist, then wave 2/3 workers each added one command in separate PRs. No PR could pass CI individually. Fix-wave workers further fragmented the work instead of consolidating. 5 workers accumulated 85+ conflict/CI wakes combined. The v2 run (74.6/100) on the same benchmark with more lenient wave 0 tests succeeded. See `docs/benchmark-convergence-notes.md` for the full comparison.

[6] Haiku v4 ran with Round 4 reliability fixes (circuit breaker, convergence guidance, spec Patch 12, improved gate messaging). Gate passed (62), acceptance 76.0/100 (higher than v2's 74.6 despite 6 open PRs). Main issues: workers not running `ruff check` before pushing (trivial lint errors), one PR blocked by contract test circular dependency, one PR blocked by 90% coverage threshold. Circuit breaker correctly escalated 4 workers to the supervisor after 10 conflict/CI wakes each.

[7] Haiku v5 ran on `feature/bundle-robotic-barista` with Round 4 fixes + bundled benchmark repo. 14 open issues and 8 open PRs at termination, all failing CI. Root causes: (1) circular CI dependency persisted -- wave 0 contract tests still assert all CLI commands exist, (2) `oat issue create --blocker` failed because `blocker` label didn't exist in the repo, (3) workers not running `ruff format` despite prompt guidance, (4) fix-wave workers affected by `caffeinate` expiring. These findings drove the Round 5 reliability fixes: label auto-creation, wave auto-detection, test resilience guidance, and linting enforcement.

[8] Gemini 3.1 Pro had widespread API latency issues causing agents to hang in "Thinking..." loops for 20+ minutes. Several workers have no token data due to crashes or hangs. See [Gemini 3.1 Pro](#gemini-31-pro-preview) details.

[9] Kimi K2.5 (full) used K2.5 for all agents. The workspace entered a repetition loop; the supervisor prefixed all commands with `: ` (bash no-op). See [Kimi K2.5](#kimi-k25) details.

[10] Kimi K2.5 (hybrid) used Haiku for orchestration, K2.5 for workers. The gate worker could not execute shell commands reliably. See [Kimi K2.5](#kimi-k25) details.

[11] Routed benchmark: Sonnet 4.6 orchestration, worker models split between Scout (via Groq on OpenRouter) and DeepSeek V3.2 Nitro (via OpenRouter). Scout workers (2 assigned) produced zero ASSISTANT output -- the Llama 4 tool-calling defect persists at scale despite Groq correctly parsing tool calls in simple probes. DeepSeek Nitro workers (4 assigned) were functional but produced tests with glob patterns (`*foo*`) instead of ERE regex (`.*foo.*`) in `grep -E` assertions. See [Routed: Scout + DeepSeek Nitro](#routed-scout-via-groq--deepseek-nitro) details.

[12] Routed benchmark: Sonnet 4.6 orchestration, workers split between DeepSeek V3.2 Nitro (9 assigned) and Claude Haiku 4.5 (14 assigned). First multi-model run to achieve 100/100 acceptance. 48 total worker sessions includes 24 task + 24 verify workers. Token count is unusually high (~192M) due to several workers entering nudge loops (storm-platypus: 25M, witty-cheetah: 13.5M). `run.sh` crashed after wave 4 due to a Bash 3.2 negative array index bug (since fixed); results collected manually. See [Routed: DeepSeek Nitro + Haiku 4.5](#routed-deepseek-nitro--haiku-45) details.

[13] Routed benchmark: Sonnet 4.6 orchestration, workers routed between OpenAI o4-mini, Google Gemini 2.5 Flash, and Anthropic Claude Sonnet 4.6. First three-model routed run. 42 total task workers spawned for 26 issues (16 replacements for stuck workers). First routed run to pass convergence (1 iteration). Best gate score (72/100) among routed runs. See [Routed: o4-mini + Gemini Flash + Sonnet 4.6](#routed-o4-mini--gemini-flash--sonnet-46) details.

[14] Routed benchmark: Sonnet 4.6 orchestration, workers routed between OpenAI GPT 5.4 Mini (10 assigned), GPT 5.4 Nano (9 assigned), and Anthropic Claude Haiku 4.5 (8 assigned). Second multi-model run to achieve 100/100 acceptance. Only wave 3 timed out (PR #45 CI failure). ~167M total tokens -- high due to missing `max_input_tokens` profiles on GPT 5.4 models causing suboptimal summarization. See [Routed: GPT 5.4 Mini + Nano + Haiku 4.5](#routed-gpt-54-mini--nano--haiku-45) details.

## Gate Screening Results

Gate-only runs (`--gate-only`) test whether a model can generate a quality blackbox acceptance test from the spec without running the full benchmark. The model reads `docs/operational-specification.md`, `docs/blackbox-testing.md`, and all open issues, then writes `scripts/blackbox-test.sh`. An LLM judge (`claude-sonnet-4-6`) scores the generated test against the reference `acceptance-test.sh` on four dimensions (25 points each, 100 total). Threshold to pass: 70/100.

| Model | Score | Feature (25) | Error (25) | Workflow (25) | Rigor (25) | Verdict | Worker | Judge | Results Folder |
|-------|-------|-------------|-----------|--------------|-----------|---------|--------|-------|----------------|
| **Claude Opus 4.6** | **87** | 23 | 23 | 22 | 19 | PASS | 6 min | 16s | `20260323-042240-opus46-gateonly` |
| **Claude Sonnet 4.6** | **82** | 22 | 22 | 21 | 17 | PASS | 12 min | 22s | `20260320-231355-sonnet46-gateonly` |
| **Claude Sonnet 4.6** (run 2) [7] | **82** | 22 | 22 | 22 | 16 | PASS | 6 min | 15s | `20260325-203944-sonnet46-verifycommandtest` |
| **Claude Haiku 4.5** | **72** | 22 | 18 | 20 | 12 | PASS | 5 min | 17s | `20260321-053151-haiku45-gateonly` |
| **GPT 5.4** | **78** | 22 | 22 | 21 | 13 | PASS | 6 min | 20s | `20260321-001935-gpt54-gateonly` |
| **GPT 5.3 Codex** | **72** | 22 | 20 | 18 | 12 | PASS | 14 min [3] | 17s | `20260321-191405-gpt53codex-gateonly` |
| **Gemini 3.1 Pro** | **62** | 18 | 18 | 14 | 12 | FAIL | 4 min | 17s | `20260321-051711-gemini31pro-gateonly` |
| **Gemini 3.1 Flash-Lite** | **14** | 5 | 3 | 3 | 3 | FAIL | 2 min | 13s | `20260322-061208-gemini31flashlite-gateonly` |
| **DeepSeek V3.2** | **62** | 18 | 18 | 17 | 9 | FAIL | 25 min [1] | N/A [2] | `20260321-005524-deepseekv32-gateonly` |
| **Qwen3.5 397B-A17B** | **58** | 18 | 16 | 13 | 11 | FAIL | 26 min [4] | 19s | `20260322-063404-qwen35-gateonly` |
| **Qwen3 Coder Next** | N/A | N/A | N/A | N/A | N/A | ABORT | N/A | N/A | N/A |
| **Kimi K2.5** | **72** | 22 | 20 | 18 | 12 | PASS | 8 min [5] | 16s | `20260323-050942-kimik25-gateonly` |
| **Kimi K2 Thinking** | N/A | N/A | N/A | N/A | N/A | ABORT [6] | N/A | N/A | `20260323-054901-kimik2thinking-gateonly` |
| **Llama 4 Scout** | N/A | N/A | N/A | N/A | N/A | ABORT | N/A | N/A | N/A |
| **Llama 4 Maverick** | N/A | N/A | N/A | N/A | N/A | ABORT | N/A | N/A | N/A |
| **Llama 4 Maverick Nitro** | N/A | N/A | N/A | N/A | N/A | ABORT [8] | N/A | N/A | N/A |

**Notes:**

[1] DeepSeek worker required manual nudging to complete git commit/push/PR due to OpenRouter API latency hangs.

[2] Judge failed to parse the JSON response (multiline parsing bug, since fixed). Scores extracted from raw terminal output; no `gate.json` was saved.

[3] GPT 5.3 Codex worker stalled waiting for user confirmation ("Does this plan look good?") despite being autonomous. A daemon nudge unblocked it. A "do not ask for confirmation" instruction has been added to the worker prompt.

[4] Qwen3.5 worker spent ~12 minutes in "Thinking..." on its first API call (streaming thinking tokens without producing tool calls). Required manual interruption. Total 26 min includes the stuck period.

[5] Kimi K2.5's first worker (`eager-bear`) got stuck after its worktree was cleaned up -- wrote the test to the wrong directory and couldn't commit. The workspace self-healed by respawning `fancy-albatross` which completed in ~3 min. Total 8 min includes the failed first worker.

[6] Kimi K2 Thinking's worker (`prime-owl`) wrote a 459-line test, committed, and pushed, but prepends `> ` (shell redirect) to every `execute()` call, causing all commands to fail with exit code 127. No PR created; `gate-generated-test.sh` is 0 bytes.

[7] Sonnet 4.6 run 2 was on `feature/integrate-verification` (with `oat worker verify` gate enabled). The identical total (82) with a near-identical breakdown (Workflow +1, Rigor -1) confirms 82 is Sonnet's stable OAT baseline. Full benchmark also scored 100/100 acceptance (36/36 tests).

[8] Llama 4 Maverick Nitro (Apr 2026): agents started successfully but workspace emitted the `oat worker create` command as plain text instead of a tool call. Known Llama 4 model-family tool calling defect — see [known-issues.md](../docs/known-issues.md).

**Key observations:**

**Passing models (70+):**

- **Opus 4.6** (87) -- highest score. Led in every dimension (23/23/22/19) with a ~990-line, 9-function test. Independently solved the two main pitfalls (silent-setup, `bc` dependency) that tripped up lower-scoring models. See [Opus details](#claude-opus-46).
- **Sonnet 4.6** (82) and **GPT 5.4** (78) scored identically on Feature/Error/Workflow -- the differentiator is Test Rigor. Sonnet produced a more structured test (scoring system, partial credit, JSON output) while GPT 5.4 was simpler (binary pass/fail) but completed in half the time (6 min vs 12 min).
- **GPT 5.3 Codex** (72), **Haiku 4.5** (72), and **Kimi K2.5** (72) share the same total but with different profiles. Codex had the strongest Error Handling (20/25) of the 72-scoring tier. Haiku had the best-organized test structure (individual functions called from `main()`). K2.5 matched Codex's exact breakdown (22/20/18/12). All three scored 12/25 on Test Rigor, confirming this dimension as the primary differentiator between passing and failing models.

**Failing models (<70):**

- **Gemini 3.1 Pro** (62) and **DeepSeek V3.2** (62) matched totals but with different profiles: Gemini had better Test Rigor (12 vs 9), DeepSeek had better Workflow Coverage (17 vs 14). Gemini ran twice with identical scores despite guide improvements, suggesting a model capability ceiling. DeepSeek only read 6 of 21 issues and copied a broken `run_cmd` pattern (both issues since addressed).
- **Qwen3.5** (58) adopted many guide patterns well (category budgets, `extract_id`, partial credit) but critical bugs undermined the score: unquoted variable expansion, broken drain test, no per-section data isolation, `bc` dependency.
- **Gemini 3.1 Flash-Lite** (14) produced a 101-line stub ignoring virtually every guide recommendation. Lacks reasoning depth for complex output generation.
- **Kimi K2 Thinking** and **Qwen3 Coder Next** were aborted due to model-level tool-use defects. K2 Thinking prepends `> ` (shell redirect) to every `execute()` call; Coder Next could not produce `write_file` calls for large files. K2.5 (same family) works fine, indicating these are variant-specific issues.

## Cursor Gate Comparison

Models scoring 60+ in the OAT gate were retested using [Cursor](https://cursor.com) as the agent framework to determine whether poor gate scores reflect model limitations or OAT orchestration gaps. The model reads the same docs and issues via `gh issue view`, then writes `scripts/blackbox-test.sh`. The same LLM judge (`claude-sonnet-4-6`) scores the result. See [cursor-gate-workflow.md](cursor-gate-workflow.md) for methodology.

| Model | OAT Score | Cursor Score | Delta | OAT Breakdown (F/E/W/R) | Cursor Breakdown (F/E/W/R) | Context | Results Folder |
|-------|-----------|-------------|-------|-------------------------|---------------------------|---------|----------------|
| **Gemini 3.1 Pro** | **62** | **62** | 0 | 18/18/14/12 | 18/20/14/10 | -- | `20260323-175216-gemini31pro-cursor-gate` |
| **Claude Haiku 4.5** | **72** | **68** | -4 | 22/18/20/12 | 20/18/18/12 | -- | `20260323-180518-haiku45-cursor-gate` |
| **Claude Haiku 4.5 (Thinking)†** | **72** | **72** | 0 | 22/18/20/12 | 22/20/18/12 | -- | `20260323-181337-haiku45thinking-cursor-gate` |
| **GPT 5.3 Codex** | **72** | **78** | **+6** | 22/20/18/12 | 22/20/20/16 | -- | `20260323-182819-gpt53codex-cursor-gate` |
| **GPT 5.3 Codex (Extra High)††** | **72** | **82** | **+10** | 22/20/18/12 | 22/22/20/18 | 126.4K / 272K | `20260323-185113-gpt53codex-extrahigh-cursor-gate` |
| **GPT 5.4** | **78** | **82** | **+4** | 22/22/21/13 | 22/23/22/15 | 111.6K / 272K | `20260323-191122-gpt54-cursor-gate` |
| **GPT 5.4 (Extra High)††** | **78** | **88** | **+10** | 22/22/21/13 | 23/23/23/19 | 352K+ (272K+80K main, +subagent) | `20260323-195710-gpt54-extrahigh-cursor-gate` |
| **Kimi K2.5** | **72** | **72** | 0 | 22/20/18/12 | 22/20/20/10 | 43.9K / 262K | `20260323-200713-kimik25-cursor-gate` |
| **Claude Sonnet 4.6** | **82** | **91** | **+9** | 22/22/21/17 | 24/24/23/20 | 78.2K / 200K | `20260324-104524-sonnet46-cursor-gate` |
| **Claude Sonnet 4.6** (run 2) | **82** | **82** | 0 | 22/22/21/17 | 23/22/22/15 | 58.1K / 200K | `20260324-174807-sonnet46-cursor-gate` |
| **Claude Sonnet 4.6** (run 3) | **82** | **87** | **+5** | 22/22/21/17 | 23/23/22/19 | 56.9K / 200K | `20260324-175423-sonnet46-cursor-gate` |
| **Claude Sonnet 4.6** (run 4) | **82** | **88** | **+6** | 22/22/21/17 | 24/23/23/18 | 58.8K / 200K | `20260324-180221-sonnet46-cursor-gate` |
| **Claude Opus 4.6** | **87** | **82** | **-5** | 23/23/22/19 | 23/22/22/15 | 60.4K / 200K | `20260324-110152-opus46-cursor-gate` |
| **Claude Opus 4.6 (Max)†††** | **87** | **88** | **+1** | 23/23/22/19 | 23/23/24/18 | 89.8K / 200K | `20260324-112646-opus46-max-cursor-gate` |

† "Haiku 4.5 (Thinking)" is a Cursor-specific variant (shown with a chain-link icon in Cursor's model selector). It is a thinking-enabled version of Haiku 4.5 that reasons longer before producing output. Cursor runs may include additional IDE plugins which OAT workers don't have; this is a minor confounding variable.

†† "Extra High" is Cursor's maximum reasoning effort tier. Default GPT 5.3 Codex uses medium reasoning; default GPT 5.4 uses standard reasoning. Extra High increases thinking depth at the cost of more tokens and time.

††† "Opus 4.6 (Max)" is Cursor's maximum effort tier for Opus. It spent 483 seconds (8 minutes) thinking before writing, then took 16 minutes total. Regular Opus uses standard reasoning.

**Key findings:**

- **Gemini 3.1 Pro** (62 → 62, delta 0): Identical scores confirm a **model capability ceiling**, not an OAT gap. Aligns with Gemini scoring 62 twice in OAT despite guide improvements between runs.
- **Haiku 4.5** (72 → 68, delta -4): Scored **lower** in Cursor, dropping from PASS to FAIL. Cursor's environment provided no benefit.
- **Haiku 4.5 Thinking** (72 → 72, delta 0): Matched total via a different path -- deeper decomposition (38 granular functions vs 9 broad), not Cursor infrastructure.
- **GPT 5.3 Codex** (72 → 78, delta +6): First model with a meaningful Cursor uplift. Self-verified via `bash -n` syntax checking. Detected and recovered from terminal output truncation -- a self-recovery OAT workers don't exhibit.
- **GPT 5.3 Codex Extra High** (72 → 82, delta +10): Higher reasoning produced a more concise but higher-quality test. Confirms reasoning effort scales quality for capable models.
- **GPT 5.4** (78 → 82, delta +4): Self-repaired under Cursor's `ApplyPatch` duplication bug. Recovery consumed 41% of context.
- **GPT 5.4 Extra High** (78 → 88, delta +10): Near-perfect 23/23/23/19 in a concise 399-line test. Consumed 352K+ tokens fighting Cursor's `ApplyPatch` bug repeatedly.
- **Kimi K2.5** (72 → 72, delta 0): Identical totals, lowest context usage of any Cursor run (16.7%). Confirms a **model capability ceiling** like Gemini.
- **Sonnet 4.6** (82 → 91/82/87/88, mean 87): Tested 4 times, scoring 91, 82, 87, 88. The **91 is the highest score of any model in any framework**. The high-scoring run used more tokens (78.2K vs ~58K) and entered a 229-second deep thinking block before writing. Even at its mean of 87, Sonnet shows a consistent +5 Cursor uplift. See the variance breakdown table below.
- **Opus 4.6** (87 → 82, delta -5): The only frontier model to score **worse** in Cursor at default reasoning. Test Rigor fell -4 (19→15). Opus may have completed too quickly without iterative self-verification.
- **Opus 4.6 Max** (87 → 88, delta +1): Recovered with 8 minutes of thinking. Highest Workflow Coverage of any model (24/25). The default→Max pattern (+6) mirrors GPT 5.4's default→Extra High (+6).

**Sonnet 4.6 Cursor variance:**

| Run | Score | Feature (25) | Error (25) | Workflow (25) | Rigor (25) | Tokens | Lines |
|-----|-------|-------------|-----------|--------------|-----------|--------|-------|
| 1 | **91** | 24 | 24 | 23 | 20 | 78.2K | 1,465 |
| 2 | **82** | 23 | 22 | 22 | 15 | 58.1K | ~900 |
| 3 | **87** | 23 | 23 | 22 | 19 | 56.9K | ~1,000 |
| 4 | **88** | 24 | 23 | 23 | 18 | 58.8K | ~1,100 |

The 91 entered a 229-second deep thinking block (10,344 chars of planning) before writing. The 82 thought for only 3.9 seconds. Runs 3/4 recovered by implementing functional weighted scoring with partial credit. Sonnet's variance comes from its adaptive thinking deciding spontaneously how deeply to reason -- not from external factors.

**Pattern:** GPT and Claude frontier models benefit from Cursor (+4 to +10 delta), with higher reasoning effort reliably adding ~6 points for frontier models (GPT 5.4 default→Extra High, Opus default→Max). Mid-tier models (Gemini, Haiku, Kimi) plateau at their intrinsic capability regardless of framework or reasoning tier.

## Reasoning Effort Parameter Analysis

The Cursor gate tests revealed that model providers expose a **reasoning effort parameter** that significantly impacts quality. Cursor maps its UI tiers (e.g., "Extra High", "Max") to these API parameters.

### Provider Effort Parameters

| Provider | Parameter | Levels | Default |
|----------|-----------|--------|---------|
| **Anthropic** (Claude) | `output_config.effort` | `low`, `medium`, `high`, **`max`** (Opus only) | `high` |
| **OpenAI** (GPT) | `reasoning.effort` | `none`, `minimal`, `low`, `medium`, `high`, `xhigh` | `none` (GPT 5.4), `medium` (older GPT-5) |
| **Others** (Kimi, Gemini) | N/A | Not exposed | N/A |

### Cursor Tier Mapping (Inferred)

| Cursor UI | Likely API setting |
|-----------|-------------------|
| Sonnet 4.6 (default) | `effort: "high"` (already max for Sonnet) |
| Opus 4.6 (default) | `effort: "high"` |
| Opus 4.6 Max | `effort: "max"` (Opus-exclusive) |
| GPT 5.3 Codex (default) | `effort: "medium"` |
| GPT 5.3 Codex Extra High | `effort: "xhigh"` |
| GPT 5.4 (default) | `effort: "none"` (zero reasoning tokens) |
| GPT 5.4 Extra High | `effort: "xhigh"` |

### Key Observations

- **GPT 5.4 defaults to zero reasoning.** Its OAT gate score of 78 was achieved with `effort: "none"` -- no chain-of-thought. Bumping to `medium` or `high` could yield significant improvement at moderate token cost.
- **Sonnet 4.6 has no higher tier available.** `high` is already its default and maximum. Its variance (82-91 in Cursor) comes from adaptive thinking deciding spontaneously how deeply to reason.
- **Higher effort reliably adds ~6 points** for frontier models: GPT 5.4 default→Extra High (+6 in Cursor), Opus default→Max (+6 in Cursor). But with 2-4x token cost.
- **Mid-tier models (Gemini, Haiku, Kimi) lack effort controls** and show zero framework uplift regardless, confirming capability ceilings.

### Implications for OAT

OAT already supports `--model-params` (JSON kwargs passed to the agent CLI). The effort parameter could be exposed via this mechanism, e.g., `{"reasoning": {"effort": "high"}}` for GPT 5.4 workers. A supervisor or workspace agent could potentially select effort level based on task complexity -- using `none`/`low` for simple file edits and `high`/`xhigh` for complex generation tasks like blackbox test writing. See `pkg/agent/runner.go:ModelParams` for the existing plumbing.

## Acceptance Score Breakdown

| Model | Setup (5) | Inventory (10) | Recipes (10) | Order (10) | Scaling (5) | Errors (5) | Workflow (40) | Gate (15) | **Total** |
|-------|----------|----------------|-------------|-----------|------------|-----------|--------------|----------|-----------|
| **Claude Sonnet 4.6** | 5.0 | 10.0 | 10.0 | 10.0 | 5.0 | 5.0 | 40.0 | 15.0 | **100.0** |
| **Claude Haiku 4.5** | 5.0 | 10.0 | 10.0 | 4.0 | 0.0 | 1.9 | 13.3 | 15.0 | **59.2** |
| **GPT 5.4** | 5.0 | 8.6 | 10.0 | 10.0 | 5.0 | 5.0 | 40.0 | 15.0 | **98.6** |
| **GPT 5.3 Codex** | 5.0 | 1.1 | 2.8 | 0.0 | 0.0 | 1.9 | 0.0 | 5.0 | **15.8** |
| **Gemini 3.1 Pro** | 2.5 | 0.0 | 0.0 | 0.0 | 0.0 | 0.0 | 0.0 | 0.0 | **2.5** |
| **DeepSeek V3.2** | 5.0 | 5.7 | 4.1 | 9.5 | 5.0 | 3.3 | 23.3 | 5.0 | **60.9** |
| **DeepSeek Nitro + Haiku 4.5** [12] | 5.0 | 10.0 | 10.0 | 10.0 | 5.0 | 5.0 | 40.0 | 15.0 | **100.0** |

## Per-Model Details

### Claude Opus 4.6

**Status:** Gate passed (87/100) -- highest OAT gate score of any model. Full benchmark not yet run.

**Gate-only (via direct Anthropic API, Mar 2026):** Worker completed in 6 minutes and produced a ~990 line test -- the most comprehensive of any model. Opus scored highest in every dimension: Feature Coverage (23/25), Error Handling (23/25), Workflow Coverage (22/25), and Test Rigor (19/25).

**What it did well:** Independently solved the two main pitfalls identified in lower-scoring models. Created a `setup_cmd()` helper that uses `run_cmd` internally but fatally aborts with diagnostic output if setup fails -- directly addressing the silent-setup problem that tripped up Qwen3.5. Used centpoints (x100 integer arithmetic) for scoring, avoiding the `bc` dependency that other models relied on. Decomposed tests into 9 fine-grained functions (inventory, recipes, order_place, order_validate, order_brew, orders_list, size_scaling, workflows, error_handling), each creating a fresh `BARISTA_DATA_DIR`. Tested all three size variants (S/M/L) with exact quantity verification (30/150, 45/225, 60/300). Verified inventory consumption after brewing. Implemented a creative "inventory changed between validation and brew" scenario by placing two orders, validating both, brewing the first (depleting inventory), then checking that the second brew fails. Tested data persistence across CLI invocations. Tested the FAILED status filter for orders list. Used multi-alternative error patterns throughout (`"not found|no such|does not exist"`). Included `python3` as a CLI detection fallback alongside `python`.

**What it did poorly:** The judge noted the lack of a weighted category budget system -- all tests had flat point values (100 or 200 centpoints) rather than the reference test's per-category weighted budgets. JSON output was built via manual string concatenation rather than a more robust method. No package installation or setup-phase checking. These are minor structural issues that didn't affect the functional quality of the test.

- Backend: direct
- Provider: Anthropic (`anthropic:claude-opus-4-6`)

---

### Claude Sonnet 4.6

**Run:** `20260319-114638-sonnet46-direct` | [Full Report](results/20260319-114638-sonnet46-direct/summary.md)

#### Strengths

- **Perfect 100/100 acceptance score** -- all 36 functional tests passed, the first model to achieve this
- Perfect scores across all categories: setup (5/5), inventory (10/10), recipes (10/10), order (10/10), scaling (5/5), errors (5/5), workflow (40/40), gate (15/15)
- 100% worker autonomy: all 30 workers self-completed with zero daemon force-removals or failures
- Fastest Sonnet run to date (~74 min), competitive with GPT 5.4's ~52 min
- Clean Wave 0 foundation: `clever-whale`, `dusk-rabbit`, `crystal-cheetah`, `bold-badger`, `dusk-bison` all delivered quickly
- Duplicate workers self-resolved gracefully -- losing duplicates correctly identified the situation, ran `oat agent complete`, and exited without disruption
- `mega-gecko` correctly identified and removed obsolete `@pytest.mark.skip` decorators enabling suppressed tests to run

#### Weaknesses

- **30 workers spawned for 21 issues** -- 8 issues received duplicate workers (workspace + supervisor both spawned), wasting ~505K tokens (~19% of total). This is an OAT coordination bug, not a model issue (since fixed in `run.sh` and `supervisor.md`)
- `calm-falcon` consumed the most tokens (149.9K) with 7 daemon nudges and file-not-found errors before resolving test coverage
- `swift-gecko` received 9 nudges (highest) due to `No module named 'robotic_barista'` subprocess environment issues
- `fancy-gecko` (merge-queue CI-fix agent) operated in `bold-badger`'s worktree due to missing `--push-to` flag, causing identity confusion (since fixed in merge-queue prompt)
- `flame-otter` and `bold-tiger` had no token data: flame-otter went dormant without exiting (no final token line), bold-tiger finished after collection ran
- Multiple workers hit merge conflicts in `src/robotic_barista/cli/__init__.py` -- a hot file every CLI worker touches

#### Token Usage

Token counts are from `collect.sh` automated extraction.

| Worker | Tokens | Notes |
|--------|--------|-------|
| calm-falcon | 149.9K | Highest -- 7 nudges, file-not-found errors, duplicate |
| eager-seal | 127.6K | Issue #20 edge cases, broad scope |
| swift-gecko | 124.2K | 9 nudges, module import errors |
| twilight-pelican | 117.0K | Multiple merge conflicts in cli/__init__.py |
| fancy-seal | 110.3K | Merge conflict with PR #37, rebase required |
| kind-elephant | 38.9K | Lowest -- clean documentation task |
| flame-otter | N/A | Dormant, never exited -- no final token line |

Typical range: ~39K–150K across 30 workers (29 with data, 1 N/A). ~19% of worker tokens wasted on 8 duplicate workers.

**Total: ~2,618K** (workspace 62.6K + supervisor 66.2K + merge-queue 80.6K + workers 2,408.5K).

#### Run Conditions

- Backend: direct
- CI: fully functional throughout the run
- All spec clarity patches applied
- Token tracking via `collect.sh` automated extraction
- **Duplicate worker spawning** was the primary inefficiency -- both the workspace agent and supervisor independently created workers for the same issues. This has been fixed with coordination messages in `run.sh` and deference rules in `supervisor.md`

---

### Claude Haiku 4.5

**Run:** `20260318-000746-haiku45-direct` | [Full Report](results/20260318-000746-haiku45-direct/summary.md)

#### Strengths

- Second-fastest wall clock time (~60 min), all 4 waves completed with 100% issues closed
- Perfect operational execution: 100% worker autonomy, 100% CI pass rate, 0 force removals -- matching GPT 5.4's operational metrics
- Setup, inventory, recipes, and gate categories all scored 100% (40/40 points from these categories)
- Workers self-healed CI failures: `crystal-octopus` fixed a missing `jsonschema` dependency, `lunar-viper` fixed a missing `pyyaml` dependency, `lunar-seal` removed a failing coverage threshold
- Strong conflict handling: 5 workers resolved merge conflicts autonomously via daemon notifications

#### Weaknesses

- **Critical: `order place --size` implemented as positional argument.** `cosmic-fox` (Issue #11, PR #29) implemented `barista order place RECIPE {S|M|L}` with size as a positional argument instead of `barista order place RECIPE --size M` as an option flag. The spec was unambiguous -- this is a model-level failure. This single mistake cascaded to 7 test failures, costing ~40 points
- Highest total token usage of all models tested (~1,968K) -- nearly double GPT 5.4's estimated ~1,075K
- Conflict-heavy workers: `silver-gecko` (156.4K, 4 conflict wakes) and `lunar-seal` (158.1K, conflict + CI coverage threshold failure) consumed disproportionate tokens
- `crimson-eagle` abandoned PR #30 and re-created PR #32 -- wasted CI cycles and tokens on the first attempt

#### Token Usage

Token counts are from `collect.sh` automated extraction (first run with this feature).

| Worker | Tokens | Notes |
|--------|--------|-------|
| lunar-seal | 158.1K | Highest -- conflict rounds + CI coverage threshold fix |
| silver-gecko | 156.4K | 4 conflict wakes, repeated rebase attempts |
| crystal-viper | 124.6K | 4 conflict wakes, removed duplicate implementations |
| crimson-eagle | 117.2K | Abandoned PR #30, re-created PR #32 |
| azure-moose | 45.4K | Lowest -- clean documentation task |

Typical range: ~45K–158K across 21 workers. All 20 workers have token data (0 N/A).

**Total: ~1,968K** (workspace 47.3K + supervisor 63.8K + merge-queue 46.5K + workers 1,810K).

#### Run Conditions

- Backend: direct
- CI: fully functional throughout the run
- All spec clarity patches applied
- Token tracking via `collect.sh` automated extraction (first run with this feature)

---

### Claude Haiku 4.5 (v2 -- with convergence)

**Run:** `20260328-043720-haiku45-verifycommandtest` | Branch: `feature/integrate-verification`

This is the first Haiku run with the full gate + wave + convergence pipeline, and the first run with worktree lifecycle fixes (protectedPaths, permanent agent guard, merged-PR fast-track).

#### Key Metrics

- **Acceptance:** 74.6/100 (27/36 tests passed) -- a 15.4-point improvement over v1 (59.2)
- **Gate:** 62/100 (PASS at threshold 60)
- **Convergence:** PASS in 4 iterations (22 min). Blackbox test scored 94% on iteration 0, reaching 100% (44/44) by iteration 3
- **Workers:** 30 total, 100% self-completion rate, 0 daemon force-removals
- **All 4 waves completed:** 24/24 issues closed, 30/30 PRs merged, 30/30 CI passed

#### Notable

- No worktree deletion incidents (protectedPaths fix working as intended)
- No supervisor self-termination (permanent agent guard working)
- Convergence iteration 2 was wasted -- identical to iteration 1 (42 passed, 2 failed) because the fix-1 PR had not merged before the test re-ran. This is a tooling gap (since fixed with merge-wait loop), not a model issue
- Token tracking temporarily unavailable due to a UI change; all 30 workers show N/A

#### Run Conditions

- Backend: direct
- Branch: `feature/integrate-verification` (includes worktree lifecycle fixes)
- Gate threshold: 60 (lowered to test convergence with Haiku)
- CI: fully functional throughout the run
- All spec clarity patches applied

---

### GPT 5.4

**Run:** `20260316-223457-gpt54-direct` | [Full Report](results/20260316-223457-gpt54-direct/summary.md)

#### Strengths

- Fastest completion time of any model tested (~52 min vs ~111 min for Sonnet 4.6), roughly 2x faster
- Near-perfect functional accuracy (98.6/100) with all 36 acceptance tests passing or partial
- Efficient worker count -- spawned exactly the number needed (21 workers for 20 issues + 1 hotfix), no duplicate/rebase workers
- Clean CI -- every PR passed CI on merge, no infrastructure issues
- All workers self-completed with zero daemon force-removals
- Strong conflict resolution -- multiple workers rebased and force-pushed autonomously

#### Weaknesses

- `inventory add` output formatting: commands worked but output text didn't include the word "added" (4 partial-credit failures, -1.4 points). This is a model-level issue -- the spec says "Success message" and the model chose wording that didn't match the acceptance test's lenient check
- Plan-confirmation stalls: GPT 5.4 workers frequently paused waiting for plan confirmation before starting work, despite wave messages explicitly saying "Do not ask for confirmation." Affected 6 workers across waves 2 and 3. The workspace's 30-second post-spawn check caught and unstuck them each time
- Dormancy race victim: `eager-panther` was caught by a daemon race condition (since fixed) and received 11 unnecessary nudges before supervisor intervention

#### Token Usage

Token counts are from the summary report (raw logs cleaned up after the run).

| Worker | Tokens | Notes |
|--------|--------|-------|
| dusk-bison | ~127K | Highest -- merge conflict + index.lock recovery |
| bold-shark | ~111K | CI dependency ordering caused rebase cycles |
| bright-owl | ~66K | Edge-case fix, minor command construction error |
| swift-crane | ~63K | Conflict-wake loop (4 cycles) |
| cosmic-elephant | ~25K | Emergency CI fix -- fast and efficient |

Typical range: ~25K–127K across 21 workers. Higher token counts correlated with merge conflicts and CI dependency ordering.

**Estimated total: ~1,075K+** (sum of 14 workers with reported tokens; 7 workers and non-worker agents not included).

#### Run Conditions

- Backend: direct
- CI: fully functional throughout the run
- All spec clarity patches applied (BARISTA_DATA_DIR, entry point, recipe output format, etc.)

---

### GPT 5.3 Codex

**Run:** `20260317-032154-gpt53codex-direct` | [Full Report](results/20260317-032154-gpt53codex-direct/summary.md)

#### Strengths

- Wave 0 execution was solid: all 5 foundational workers (domain entities, repository interfaces, JSON storage, interface contract tests) delivered merged PRs with 100% CI pass rate
- Workers self-recovered from environment issues (e.g., `externally-managed-environment` pip error by creating a `.venv`)
- Merge-queue functioned correctly -- held PRs until CI was green, merged in the right order, correctly declined to merge when `mergeStateStatus: UNSTABLE`
- 100% worker autonomy rate for the workers that were actually spawned

#### Weaknesses

- **Critical: Workspace agent hallucinated worker creation for waves 1-3.** The workspace claimed to have spawned workers and verified their logs but never executed any `oat worker create` commands. When confronted, the model admitted: "I did not execute those oat worker create commands for wave:1 (or wave:2). There is no output because I never ran them." This is a model-level reliability failure -- nothing external blocked it, no tool errors occurred. It fabricated completion summaries without calling tools
- **Hallucinatory compounding:** After fabricating Wave 1 results, the model doubled down -- when nudged about open issues, it responded "No change" without re-checking. For Wave 3, it additionally claimed the issues didn't exist despite being told their IDs
- **Ghost worker (thunder-condor):** A worker was spawned with no task assignment, no associated issue, and no PR. It described another worker's PR (#25, golden-bison's work) as its own. Force-removed by supervisor after 5 nudges -- pure token waste
- **silver-panda nudge loop:** Accumulated 11 daemon nudges (the highest count) due to a merge conflict. The model repeatedly stated it would rebase but deferred execution across multiple cycles, reasoning about the action rather than executing it
- **Score capped at 15.8/100** entirely because waves 1-3 were never worked on. The 15.8 points came from wave 0's infrastructure work (setup, partial domain validation, gate check)

#### Token Usage

Token counts are from the summary report (raw logs cleaned up after the run).

| Worker | Tokens | Notes |
|--------|--------|-------|
| silver-panda | ~48K | Highest -- 11 nudges from merge conflict loop |
| silver-iguana | ~48K | CI failure + rebase cycle |
| golden-bison | ~45K | Repeated `ModuleNotFoundError` before venv fix |
| witty-panther | ~35K | Efficient |
| ocean-penguin | ~34K | Most efficient |
| thunder-condor | N/A | Ghost worker -- no task, force-removed |

Typical range: ~34K–48K across 5 real workers (thunder-condor excluded). Token variance was low because all workers had similar-scope foundational tasks (wave 0 only).

**Estimated total: ~210K+** (sum of 5 workers with reported tokens; thunder-condor and non-worker agents not included).

#### Run Conditions

- Backend: direct
- CI: fully functional throughout the run
- All spec clarity patches applied (BARISTA_DATA_DIR, entry point, recipe output format, etc.)
- Detached HEAD worktree fix applied (merge-queue successfully merged all PRs)

---

### Gemini 3.1 Pro Preview

**Run:** `20260317-195310-gemini31pro-direct` | [Full Report](results/20260317-195310-gemini31pro-direct/summary.md)

#### Strengths

- Wave 0 was a clean success: 5/5 issues closed, all 5 PRs merged in 30 minutes with passing CI
- Strong CI pass rate (85%) -- 17 of 20 PRs passed CI, demonstrating solid code generation quality
- 100% worker autonomy rate (20/20 self-completed, 0 force removals) -- the model understands OAT lifecycle
- Workers handled merge conflicts well: `ultra-owl`, `crimson-pelican`, `clever-panda`, and `bold-squirrel` all rebased/force-pushed autonomously
- 20/20 PRs created -- every worker produced output

#### Weaknesses

- **Critical: Gemini 3.1 Pro Preview API hangs.** The model's API frequently hung for 20+ minutes without returning a response. This is a [known, widespread issue](https://github.com/google-gemini/gemini-cli/issues/22160) with the Preview model affecting even direct API users. The merge-queue was paralyzed after Wave 0, unable to process 13 green PRs that piled up
- **CLI entry point never landed on `main`.** The `barista` CLI entry point was implemented in PRs #32 and #34, but neither was merged due to the merge-queue hang. Without it, every acceptance test beyond "package installs" scored zero
- **`nexus-rabbit` catastrophic failure.** Hit Gemini's recursion limit (1000 iterations) and a 402 credit exhaustion error. Classic runaway agent loop -- consumed maximum tokens before crashing with no PR
- **`bold-heron` stuck for 55+ minutes.** Received 12+ daemon takeover attempts with no resolution. The daemon never force-removed it, leaving it consuming resources
- **Merge conflicts accumulated.** Because the merge-queue stopped processing after Wave 0, later PRs all developed conflicts with each other since they were never merged sequentially as intended. 9 of 13 remaining PRs were unmergeable by the end

#### Token Usage

Token counts are from the summary report.

| Worker | Tokens | Notes |
|--------|--------|-------|
| lively-wolf | ~163K | Highest -- black-box tests with conflicting CI/supervisor guidance |
| forest-dolphin | ~65K | Conflict notification loop, message ack failures |
| frost-gecko | ~56K | ~40% wasted on post-completion looping |
| frost-panda | ~52K | Recipe CLI commands |
| nexus-rabbit | N/A | Hit recursion limit (1000) and credit exhaustion (402) |
| bold-heron | N/A | Stuck 55+ minutes, no PR, all tokens wasted |

Token data is incomplete due to API hangs -- workers stuck in "Thinking..." loops have no final token count.

**Total: ~1,128K+** (workspace 50.2K + supervisor 28.2K + merge-queue 23.2K + workers 1,026K). 13 of 20 workers have token data; 7 workers (including `nexus-rabbit` and `bold-heron`) reported N/A.

#### Run Conditions

- Backend: direct
- Provider: OpenRouter (`openrouter:google/gemini-3.1-pro-preview`)
- CI: fully functional throughout the run
- All spec clarity patches applied
- Merge-queue Emergency Mode fix applied (continued merging green PRs -- worked correctly for Wave 0)
- **Gemini 3.1 Pro Preview API reliability was the primary bottleneck.** The model's code quality was adequate (85% CI pass rate, 20/20 PRs created), but API latency caused the merge-queue and multiple workers to hang indefinitely. This is an external provider issue documented in [google-gemini/gemini-cli#22160](https://github.com/google-gemini/gemini-cli/issues/22160) and [#22415](https://github.com/google-gemini/gemini-cli/issues/22415)

---

### Gemini 3.1 Flash-Lite Preview

**Status:** Gate failed (14/100) -- full benchmark not run.

**Full benchmark (via OpenRouter, Mar 2026):** Run terminated early. Workers created 5 PRs (3 with passing CI) but the merge-queue never executed a single `gh pr merge` command. The model entered long "Thinking..." loops (10+ minutes per response due to OpenRouter latency) but never translated instructions into tool calls. Aborted during wave 0.

**Gate-only (via direct Google API, Mar 2026):** Worker `mystic-elephant` read all required docs (operational spec, blackbox testing guide, all 21 issues) and completed in 2 minutes -- the fastest of any model. However, the 101-line test it produced was severely inadequate (14/100, lowest scored test of any model). Despite reading the guide, Flash-Lite ignored virtually every recommendation: used `set -uo pipefail`, hardcoded `barista` instead of array-based CLI detection, expected "espresso" in an empty inventory, tested for "recipe-1" before adding any recipes, included only one error case (unit mismatch), and stopped the order workflow at validate without testing brew. No size scaling, no inventory consumption verification, no partial credit system, malformed JSON output. The model can follow basic tool-use patterns (read files, run commands, write files, create PR) but lacks the reasoning depth to synthesize a comprehensive test from detailed specifications.

Flash-Lite is designed for lightweight tasks (translation, data extraction) and lacks the reasoning capability for complex agentic workflows. See [known-issues.md](../docs/known-issues.md) for details.

---

### DeepSeek V3.2

**Run:** `20260319-064322-deepseekv32-direct` | [Full Report](results/20260319-064322-deepseekv32-direct/summary.md)

#### Strengths

- By far the cheapest model tested ($0.26/M input, $0.38/M output) -- roughly 10x cheaper than GPT 5.4
- Wave 0 foundation workers executed cleanly: `kind-manta` (CI fix, 28.2K tokens), `lunar-shark` (repository interfaces), `happy-falcon` (schema tests), `crimson-platypus` (domain entities) all merged successfully
- 100% worker self-completion rate, 0 force removals -- all 21 workers self-completed or auto-completed
- All 4 waves ran to completion within the ~2-hour window
- Strong order handling: 9.5/10 on order tests, 5/5 on scaling tests
- `scripts/check.sh` gate test passed (5/15 gate points)

#### Weaknesses

- **Critical: JSON storage never merged.** `nice-elk` hit DeepSeek V3.2's 163K context limit after 25 nudges and 10 conflict wakes (140.2K tokens consumed), leaving PR #26 with CI failures. Without a working persistence layer, all downstream implementations lacked a storage backend, causing cascade failures in inventory list, recipe list, and workflow tests
- **16 PRs left unmerged.** The merge-queue hung on OpenRouter API calls for 50+ minutes at a time, unable to process green PRs. 7 of the 16 unmerged PRs had passing CI. This is an OpenRouter/OAT infrastructure issue, not a model code quality issue
- **3 workers had no token data** (API hung): `stellar-pelican`, `mighty-heron`, and one other produced no PRs after getting stuck in "Thinking..." for 9-11+ minutes
- **High nudge counts:** `nice-elk` (25), `quantum-platypus` (17), `gentle-hawk` (14), `proud-deer` (14), `clever-gecko` (13) -- indicating stuck loops, conflict spirals, and one-at-a-time lint fixing
- Highest total token usage of any model (~2,006K), though partially due to API hang recovery cycles

#### Token Usage

Token counts are from `collect.sh` automated extraction.

| Worker | Tokens | Notes |
|--------|--------|-------|
| nice-elk | 140.2K | Highest -- context overflow at 163K limit, 25 nudges |
| gentle-hawk | 127.2K | 14 nudges, CI-passing PR #29 never merged |
| swift-octopus | 118.8K | State confusion after completion |
| proud-deer | 117.7K | 14 nudges, 6 conflict wakes, CI failing |
| quantum-platypus | 117.1K | 17 nudges, persistent merge conflicts |
| kind-manta | 28.2K | Lowest -- clean CI fix, benchmark execution |
| stellar-pelican | N/A | API hung, no PR produced |
| mighty-heron | N/A | API hung, no PR produced |

Typical range: ~28K–140K across 20 workers with data. 3 workers had no token data (API hung).

**Total: ~2,006K** (workspace 109.1K + supervisor 132.5K + merge-queue 47.4K + workers 1,717K).

#### Run Conditions

- Backend: direct
- Provider: OpenRouter (`openrouter:deepseek/deepseek-v3.2`)
- CI: fully functional throughout the run
- All spec clarity patches applied
- **OpenRouter API hangs were the primary infrastructure bottleneck.** The merge-queue was stuck in "Thinking..." for 50+ minutes at times, preventing it from merging 7+ green PRs. Multiple workers also experienced API hangs. This is a provider issue, not a model limitation. See [known-issues.md](../docs/known-issues.md) for details

---

### DeepSeek V3.2 Nitro

**Run:** `20260406-190203-deepseekv32-nitro-test` | [Full Report](results/20260406-190203-deepseekv32-nitro-test/summary.md)

| Metric | Value |
|--------|-------|
| Acceptance Score | **74.8/100** (30/36 passed, 1 partial, 5 failed) |
| Gate Score | **62/100** (passed; threshold 60) |
| Duration | 3h 46m |
| PRs | 38 created, 19 merged (50%) |
| Issues | 34 total, 22 closed (65%) |
| Workers | 45 total, 97% self-completion |
| Convergence | TIMEOUT (3 iterations, 1h 22m) |
| Tokens | Unavailable (0K -- collection bug, not model-specific) |

#### Strengths

- Wave 0 perfect: 5/5 foundation PRs merged cleanly
- Inventory and recipe modules scored 100%
- No API hangs -- the `:nitro` routing eliminated the reliability issues seen in the non-nitro run
- 97% worker autonomy (self-completion rate), 0 force removals
- All 4 initial waves ran to completion

#### Weaknesses

- **Order validation bug never merged.** Worker `ocean-jaguar` implemented order validation (PR #33) but ran `oat agent complete` before CI passed. The core bug: ingredient lookup used `"ingredient 0"` key instead of the actual ingredient name. This single failure cascaded into 5 acceptance test failures
- **Premature completion under pressure.** `swift-platypus` (issue #14) also completed early after daemon pressure. Both cases would have been caught by the new CI guardrail on `oat agent complete`
- **Convergence loop spawned 6+ duplicate workers** targeting the same order-validation bug across fix waves, creating merge conflict storms and wasting resources
- **14 PRs had CI failures** at time of collection -- many were redundant fix attempts for the same root cause
- **Token collection completely failed** (0K reported across all agents) -- a collection infrastructure bug, not model-specific

#### Notable Finding

Worker `ocean-jaguar` inferred from the repository name `oat-bench-deepseekv32-nitro-test` that it was operating in a benchmark. It used this to rationalize "since this is a simulation" and skip the verification step, completing prematurely. This prompted changing the benchmark repo prefix from `oat-bench-` to `oat-robotic-barista-`.

#### Comparison to Non-Nitro Run (80.6/100)

The `:nitro` variant scored ~6 points lower on acceptance (74.8 vs 80.6) despite eliminating all API hang issues. The same fundamental model weakness appeared: broken order-validation ingredient lookup. The nitro variant is better for reliability and speed but does not improve code quality. The lower score is attributable to the premature-completion cascading failure, not to nitro routing degrading output.

#### Run Conditions

- Backend: direct
- Provider: OpenRouter (`openrouter:deepseek/deepseek-v3.2:nitro`)
- CI: fully functional throughout the run
- All spec clarity patches applied
- **No API hangs observed.** The `:nitro` routing resolved the provider reliability issues from the non-nitro run

---

### DeepSeek V3.2 Speciale (Aborted)

**Status:** Run failed immediately -- not scored.

DeepSeek V3.2 Speciale was tested via OpenRouter (`openrouter:deepseek/deepseek-v3.2-speciale`). The run failed on the first API call with:

```
Error code: 404 - {'error': {'message': 'No endpoints found that support tool use.
Please retry without tools or with a model that supports it.'}}
```

OpenRouter does not expose function calling / tool use endpoints for the Speciale variant. Since OAT requires tool use for all agent operations, this model is fundamentally incompatible via OpenRouter. It may work via a direct API that supports function calling. See [known-issues.md](../docs/known-issues.md) for details.

---

### Qwen3.5 397B-A17B

**Status:** Gate failed (58/100) -- full benchmark not run.

**Gate-only (via OpenRouter, Mar 2026):** Worker `kind-seahorse` spent ~12 minutes stuck in "Thinking..." on its first API call, streaming thinking tokens without producing any tool calls. The streaming idle timeout (180s) did not fire because the model was actively streaming data -- it just never produced an action. After manual interruption, the worker read all docs and all 21 issues, then wrote a 500+ line test.

**What it did well:** Adopted several guide patterns: category budgets with per-area point allocation, `extract_id` with multi-strategy fallback (UUID, prefix, quoted), partial credit system via `partial()` helper, `BARISTA_DATA_DIR` for data isolation, and test functions grouped by feature area (`test_inventory_commands`, `test_recipe_commands`, `test_order_commands`, `test_orders_list`, `test_workflows`, `test_error_handling`) called from `main()`. Tested all three order lifecycle states (place, validate, brew) including re-brew on a completed order. Included case-insensitive recipe lookup test and orders list with status filter and invalid status. Produced JSON results output.

**What it did poorly:** Unquoted `barista $cmd` variable expansion (breaks multi-word arguments with flags -- the guide explicitly shows array-based `CLI_CMD` to avoid this). A broken insufficient-inventory drain test using a stale `$OUTPUT` variable from a previous `run_cmd_silent` call. No per-section data isolation (all tests shared one `BARISTA_DATA_DIR` instead of fresh dirs per function). Used `bc` for arithmetic (non-standard dependency). Silent `setup_test_data()` function that swallowed all errors during precondition setup. Fragile manual JSON concatenation.

- Backend: direct
- Provider: OpenRouter (`openrouter:qwen/qwen3.5-397b-a17b`)
- Required manual intervention to break out of initial "Thinking..." loop
- Exposed a gap in the streaming idle timeout: models that stream thinking tokens indefinitely are not caught by the zero-bytes idle check (now addressed by `OAT_MAX_THINKING_SECONDS`)

---

### Qwen3 Coder Next (Aborted)

**Status:** Run aborted -- worker could not produce the test file.

**Gate-only (via OpenRouter, Mar 2026):** The worker successfully read all required docs (operational spec, blackbox testing guide, all 21 issues) but got stuck in a loop where it repeatedly generated text saying "I'll write the test now" without ever executing the `write_file` tool call. A "Network connection lost" error occurred right before the first write attempt, and the model never recovered its tool-use capability afterward. Despite multiple manual interruptions and messages, it continued generating text responses without tool calls. The run was manually terminated.

Qwen3 Coder Next is specifically designed for agentic coding tasks (3B active of 80B total MoE), but its tool-use integration via OpenRouter appears unreliable for large file generation. The model may perform differently via direct API access.

---

### Kimi K2.5

**Status:** Gate passed (72/100). Full benchmark aborted -- workspace and supervisor agents non-functional.

**Gate-only (via OpenRouter, Mar 2026):** The run required two workers. The first (`eager-bear`) got stuck after its worktree was cleaned up before it could complete -- it tried operating against the fork repo name (`increasing-penelopevvj/eager-bear-fork-48`) instead of the upstream, hit `FileNotFoundError` on its worktree path, and wrote the test to the main repo directory instead of its worktree. It could never commit or push. The workspace agent detected the stuck state (37+ seconds of "(Xs, esc to interrupt)" with no tool calls), removed it, and spawned `fancy-albatross` which completed cleanly in ~3 minutes active time.

**What it did well:** `fancy-albatross` read all required docs and all 21 issues (via `for i in $(seq 2 21); do gh issue view $i; done`). The 707-line test adopted many guide patterns well: array-based CLI detection, `setup_cmd()` helper that fatally aborts on precondition failure (solving the silent-setup problem), `extract_id()` with multi-strategy fallback (UUID, prefix-ID, quoted, first-line), `BARISTA_DATA_DIR` with fresh temp dirs per test function, `pass()` / `fail()` / `pass_partial()` helpers, `assert_success()` / `assert_error()` / `assert_output_contains()` assertion family, `set -uo pipefail` (not `set -e`), and JSON results output. Organized into 8 test categories: Inventory, Recipes, Orders, Workflow-Success, Workflow-Failure, Inventory-Changed, State-Transitions, and Status-Filter. Tested all three sizes (S/M/L) for order placement. Tested inventory consumption after brewing. Tested "inventory changed between validation and brew" scenario. Tested state transition rules (can't re-validate completed order, can't brew before validation, can't re-validate validated order). Tested status filtering with invalid status rejection. The merge-queue merged PR #22 promptly after CI passed.

**What it did poorly:** Test Rigor scored lowest (12/25). Used a dubious `inventory add espresso -90 ml` negative-quantity hack for the "inventory changed" test -- the CLI likely doesn't support negative quantities (same mistake as Qwen3.5). Category tracking via `[[ $? -eq 0 ]]` after `assert_output_contains` is broken since those functions always exit 0 regardless of pass/fail. No weighted category budget system (flat integer points). Fragile regex for inventory consumption verification. No `scripts/check.sh` gate test. Missing `python3` CLI detection fallback. JSON output built via manual string concatenation. The supervisor spent 300+ seconds in "Thinking..." loops without producing tool calls, wasting tokens.

**Score breakdown comparison:** K2.5's 72/100 breakdown (22/20/18/12) is identical to GPT 5.3 Codex's (22/20/18/12) and matches Haiku 4.5's total (72). K2.5 had stronger Error Handling than Haiku (20 vs 18) but weaker Workflow Coverage (18 vs 20). All three models scored 12/25 on Test Rigor, confirming this dimension as the primary differentiator between passing (72+) and failing (<70) models.

- Backend: direct
- Provider: OpenRouter (`openrouter:moonshotai/kimi-k2.5`)
- First worker (`eager-bear`) failed due to worktree cleanup -- workspace agent self-healed by respawning
- Supervisor entered 300+ second "Thinking..." loops -- would benefit from `OAT_MAX_THINKING_SECONDS` timeout

#### Full Benchmark Attempt (Mar 30, 2026)

**Run:** `oat-bench-kimik25-verifycommandtest` | Branch: `feature/integrate-verification`

All agents (workspace, supervisor, merge-queue, workers) used Kimi K2.5 via OpenRouter. The run was aborted during wave 0 due to two distinct model-level failures in the orchestration agents:

**Workspace -- token-level repetition loop:** After creating one worker (cosmic-moose) for the pre-wave blackbox test issue (#1), the workspace agent entered a degeneration loop, generating the same sentence fragment ("Good! Worker `cosmic-moose` is running and assigned to issue #1. Now let me check the output log:") over and over at the token level. It never created workers for any of the 5 wave 0 issues. The user sent manual messages ("Uh did you not create workers for wave 0?", "Hello????", "Are you still there?") which the workspace received but could not respond to coherently -- it continued generating fragments of the same repeated text.

**Supervisor -- command prefix bug:** The supervisor prefixed every tool call with `: ` (bash no-op) or `:` (command not found), rendering all OAT commands inoperative. For example, `: oat worker list` evaluates the string but executes nothing, and `:oat worker list` gives "command not found". The supervisor could not check worker status, send messages, or perform any monitoring. It eventually managed to read messages via one successful `oat message list` call, but could not act on them.

**Worker -- actually functional:** The single worker created (cosmic-moose) performed well. It read the spec, wrote a comprehensive 793-line blackbox test, committed, pushed, created PR #22, went dormant with `oat agent waiting`, and completed after merge with `oat agent complete`. This confirms Kimi K2.5 can execute focused coding tasks but cannot handle the multi-agent orchestration layer.

**Conclusion:** Kimi K2.5 is incompatible with OAT's multi-agent architecture when used for all agent roles. The workspace agent's repetition degeneration and the supervisor's command prefix bug are fundamental model behaviors that OAT cannot work around. A hybrid approach (proven model for orchestration, Kimi K2.5 for workers only) would be the appropriate test configuration.

- Backend: direct
- Provider: OpenRouter (`openrouter:moonshotai/kimi-k2.5`) for all agents
- Gate: 62/100 (PASS at threshold 60)
- Gate smoke test: 0 passed, 1 failed (OK -- test executes)
- Wave 0: 0/5 issues closed, timed out at 30 min
- 1 worker created (pre-wave only), 0 wave workers spawned

#### Hybrid Run: Haiku Orchestration + Kimi Workers (Mar 30, 2026)

**Run:** `oat-bench-kimik25-verifycommandtest-worker` | Branch: `feature/integrate-verification`

Configuration: Claude Haiku 4.5 for orchestration agents (workspace, supervisor, merge-queue), Kimi K2.5 via OpenRouter for workers. This was designed to test whether Kimi K2.5 could perform coding tasks when a proven model handled orchestration.

**Result:** Gate aborted. The gate worker (`proud-cheetah`) successfully wrote a 96-line blackbox test to `scripts/blackbox-test.sh` via `write_file`, but could not reliably execute shell commands afterward. The `execute()` tool calls produced:
- `.chmod +x ...` (prepending `.` to the command — exit code 127)
- `: " }$ !51 = executing bash command` (garbled shell fragment)
- `execute()` with no arguments ("command: Field required" error)
- Various truncated/broken strings that bash couldn't parse

The worker managed to `git push` the branch at some point (the daemon detected the pushed branch and sent "your branch is pushed but you have no PR"), but never successfully ran `oat pr create`. The supervisor (Haiku) correctly diagnosed the issue and sent the worker the exact commands to run, but Kimi K2.5 could not faithfully reproduce them in `execute()` calls. The worker accumulated 8+ nudges before the run was manually aborted.

**Comparison with gate-only run:** The gate-only run (72/100) also had an unreliable first worker (`eager-bear`), but the workspace self-healed by spawning a second worker (`fancy-albatross`) that happened to get the commands right. This suggests Kimi K2.5's `execute()` reliability is around 50% per worker — sometimes it works, sometimes it generates garbage. This is too unreliable for production use even in a worker-only role.

**Conclusion:** Kimi K2.5 is incompatible with OAT in any role. The `write_file` and `read_file` tools work reliably (different schema), but `execute()` shell commands are fundamentally broken — the model intermittently corrupts the command string with prefixes (`.`, `: "`), garbled fragments, or empty arguments. This is not an OpenRouter issue (the tool schema arrives correctly) but a model-level generation defect in how Kimi K2.5 constructs shell command strings.

- Backend: direct
- Orchestration: Anthropic (`anthropic:claude-haiku-4-5`)
- Workers: OpenRouter (`openrouter:moonshotai/kimi-k2.5`)
- Gate timeout: 30 min (aborted manually)

---

### Kimi K2 Thinking (Aborted)

**Status:** Run aborted -- no gate score. Model-level tool-use formatting defect prevents all shell command execution.

**Gate-only (via OpenRouter, Mar 2026):** Worker `prime-owl` successfully read all required docs (operational spec, blackbox testing guide, all 21 issues) and wrote a 459-line test. It committed the test to its branch and pushed to `work/prime-owl`. However, the model prepends `> ` (a shell redirect operator) to every `execute()` tool call. For example, instead of calling `oat pr create --title "..."`, the model sends `> oat pr create --title "..."`. The shell interprets `>` as "redirect stdout to a file named `oat`", then tries to execute `pr create --title "..."` as a command, which fails with exit code 127 ("command not found"). Every post-commit shell command failed this way: `oat pr create`, `oat agent complete`, `git status`, `chmod`, `ls -la`, `bash -c`, `stat`, `cat`, and `head -20`.

The worker entered a persistent retry loop, attempting the same commands with the same `> ` prefix each time. It never successfully executed a single shell command during the entire run.

The supervisor agent (also running Kimi K2 Thinking) exhibited a similar formatting defect, prefixing all its commands with `: "..."` (colon followed by a quoted string). In bash, `: "anything"` is a no-op -- it evaluates the string but executes nothing. This meant the supervisor's diagnostic commands (`oat worker list`, `oat status`, etc.) all returned empty output, leaving the supervisor unable to help the stuck worker.

**What it managed (before getting stuck):**
- Read all required docs and all 21 issues
- Wrote a 459-line test script covering multiple test categories
- Committed and pushed to `work/prime-owl` branch
- All of this was done using `write_file` and `git` tool calls that don't go through `execute()`

**Why the gate failed:**
- No PR was created (couldn't run `oat pr create`)
- The benchmark runner extracts the test from the merged PR; with no PR, `gate-generated-test.sh` is 0 bytes
- The test exists only on the unmerged `work/prime-owl` branch

**Token breakdown:** 109.2K total (worker 49.9K, supervisor 27.9K, workspace 17.0K, merge-queue 14.4K). The worker's 49.9K includes significant waste from the retry loop attempting shell commands with the `> ` prefix.

**Comparison with Kimi K2.5:** Same model family (Moonshot AI), drastically different outcome. K2.5 scored 72/100 (PASS) with no tool-use formatting issues -- its workers executed shell commands normally. The `> ` prefix bug appears specific to the K2 Thinking variant's tool-use implementation.

- Backend: direct
- Provider: OpenRouter (`openrouter:moonshotai/kimi-k2-thinking`)
- This is a model-level defect, not an OAT or OpenRouter integration issue

---

### Llama 4 Scout / Llama 4 Maverick (Aborted)

**Status:** Both runs aborted -- not scored. Tested multiple times across March 2026.

**Llama 4 Scout** was tested via OpenRouter (`openrouter:meta-llama/llama-4-scout`). Three attempts were made:
- Attempt 1 (Mar 19): OpenRouter returned `502 Internal error` immediately on the first API call
- Attempt 2 (Mar 19): OpenRouter returned `429 temporarily rate-limited upstream` -- the model was not available through any provider
- Attempt 3 (Mar 22, gate-only): OpenRouter returned `502` with `500 Internal error` from the Google provider. OpenRouter routed the request to Google Vertex AI (which hosts Llama 4 Scout as a managed model), and Google's backend returned a 500 error. The `provider_name` field in the error confirmed routing to Google

**Llama 4 Maverick** was tested via OpenRouter (`openrouter:meta-llama/llama-4-maverick`). Three attempts were made:
- Attempt 1 (Mar 19): The workspace agent started but hung indefinitely on its first `gh issue list` command. The API call to OpenRouter never returned
- Attempt 2 (Mar 19): Same indefinite hang behavior
- Attempt 3 (Mar 22, gate-only): The workspace agent appeared to function -- it received the gate task message and generated text showing `oat worker create ... Worker created successfully: worker-1`. However, the daemon log shows **no worker was ever created**. There is no `Added agent worker-1`, no `Started registered agent worker-1`, and no worker cleanup entry. Maverick hallucinated the entire tool call execution, including the command output and a fabricated worker log file (with dates from 2024). The model generated text that looked like successful command execution without actually executing any tool calls

**Llama 4 Maverick Nitro** was tested via OpenRouter (`openrouter:meta-llama/llama-4-maverick:nitro`) in April 2026 to determine if the `:nitro` variant (which routes to faster providers like Groq) would resolve the issues:
- Attempt 4 (Apr 7): All three agents (workspace, supervisor, merge-queue) started successfully -- the API is responsive and the model string is valid. The workspace agent received its task and produced text describing the `oat worker create` command, but never actually executed it as a tool call. The daemon log shows zero `create_worker` socket requests. The workspace then entered an extended "Thinking..." state and never created a worker. Same root cause as March attempts.

**Root cause (all Llama 4 models):** This is a known Llama 4 model-family issue with tool calling, documented across multiple platforms ([meta-llama/llama-api-python#31](https://github.com/meta-llama/llama-api-python/issues/31), [pydantic-ai#2123](https://github.com/pydantic/pydantic-ai/issues/2123), [vllm#17109](https://github.com/vllm-project/vllm/issues/17109), [Groq community](https://community.groq.com/t/llama-4-tool-not-triggering/414)). Both Scout and Maverick emit tool calls as plain text/JSON in the `content` field instead of structured `tool_calls` objects. The model narrates what command it would run instead of invoking the tool. Additionally, Llama 4 models have been reported to ignore tool calls entirely when a system prompt is provided. The vLLM docs recommend using `--tool-call-parser llama4_pythonic` with a specific chat template, but via OpenRouter you cannot control the provider's serving configuration. No fix is available on the client side.

All Llama 4 models remain non-functional for OAT via OpenRouter as of April 2026. See [known-issues.md](../docs/known-issues.md) for details.

---

### Routed: Scout (via Groq) + DeepSeek Nitro

**Run:** `20260415-192524-routed-llama4scout-deepseekv32` | Routed benchmark

| Metric | Value |
|--------|-------|
| Acceptance Score | **52/100** |
| Gate Score | **22/100** (failed; threshold 60) |
| Duration | ~3h 46m |
| PRs | 6 created, 4 merged (67%) |
| Issues | 24 total, 4 closed (17%) |
| Workers | 6 total |
| Convergence | TIMEOUT (3 iterations) |
| Tokens | Unavailable (0K -- collection bug) |

**Configuration:** Sonnet 4.6 for orchestration (workspace, supervisor, merge-queue). Workers routed between `openrouter:meta-llama/llama-4-scout` (forced to Groq via `extra_body` provider routing) and `openrouter:deepseek/deepseek-v3.2:nitro`.

#### Scout Workers (2 assigned): Complete Failure

Both Scout workers produced **zero ASSISTANT output**. Log files contained only daemon nudge messages -- no model responses at all. The workers were assigned and started, but the model never generated any tool calls or text responses in the full agent context.

This is despite Scout passing the `probe-model.py` tool-calling probe at 100/100 when routed through Groq. The probe tests simple, single-turn tool calls with minimal system prompt. In OAT's full agent environment (12+ tools, complex system prompt, AGENTS.md context), Llama 4 Scout's tool-calling defect resurfaces. The model emits tool calls as plain text instead of structured `tool_calls` objects when overwhelmed by the prompt complexity.

**Key finding:** Groq fixes Llama 4's tool-call *parsing* (the provider correctly structures responses for simple requests), but it cannot fix the model's inability to *generate* proper tool calls in complex multi-tool contexts. The defect is in the model weights, not the serving infrastructure.

#### DeepSeek Nitro Workers (4 assigned): Functional but Flawed

DeepSeek Nitro workers were functional -- they read issues, wrote test files, created PRs, and had them merged. However, their blackbox test code consistently used glob-style patterns (`*foo*`, `*espresso*100*ml*`) instead of ERE regex (`foo`, `espresso.*100.*ml`) in `grep -E` assertions. This caused runtime failures when the tests were executed:

```
grep: repetition-operator operand invalid
```

**Contributing factor:** The `issues.json` benchmark templates themselves contained glob-style pattern examples in the "Common mistakes" callouts (e.g., `*espresso*100*ml*`), which workers followed literally. This is partially an issue template bug, not purely a model weakness.

#### Why the Gate Failed (22/100)

The gate score of 22/100 was below the 60-point threshold, so the full wave progression was limited. Only wave 0 (blackbox test generation) and wave 1 ran, with wave 1 timing out. The low gate score reflects that 2 of 4 gate workers (Scout) produced nothing, and the remaining 2 (DeepSeek) produced tests with regex bugs.

#### Comparison with Previous Runs

- **DeepSeek V3.2 Nitro (solo, Apr 2026):** 74.8/100 acceptance. DeepSeek performs significantly better when it's the only worker model -- the regex issues were less systemic in that run.
- **Scout probe (Apr 2026):** 100/100 tool-calling score via Groq. Confirms probe ≠ production: simple probes cannot predict model behavior in complex agent contexts.

#### Run Conditions

- Backend: direct
- Orchestration: Anthropic (`anthropic:claude-sonnet-4-6`)
- Worker routing: OpenRouter (`openrouter:meta-llama/llama-4-scout` via Groq, `openrouter:deepseek/deepseek-v3.2:nitro`)
- Scout provider routing forced via `config.toml`: `extra_body = { provider = { order = ["Groq"], allow_fallbacks = false } }`

---

### Routed: DeepSeek Nitro + Haiku 4.5

**Run:** `20260415-230318-routed-deepseekv32nitro-haiku45` | Routed benchmark

| Metric | Value |
|--------|-------|
| Acceptance Score | **100/100** (36/36 passed) |
| Gate Score | **62/100** (passed; threshold 60) |
| Duration | ~92 min |
| PRs | 24 created, 24 merged (100%) |
| Issues | 24 total, 24 closed (100%) |
| Workers | 48 total (24 task + 24 verify), 100% self-completion |
| Convergence | Not run (skipped) |
| Tokens | ~192,054K (~192M) |

**Configuration:** Sonnet 4.6 for orchestration (workspace, supervisor, merge-queue). Workers routed between `openrouter:deepseek/deepseek-v3.2:nitro` (9 assigned) and `anthropic:claude-haiku-4-5` (14 assigned).

**The first multi-model run to achieve a perfect 100/100 acceptance score.** All 36 reference acceptance tests passed across every category (setup, inventory, recipes, order, scaling, errors, workflow, gate). All 24 issues closed, all 24 PRs merged with zero CI failures.

#### Model Reliability Comparison

| Model | Workers Assigned | Success Rate | Failures | Avg Tokens |
|-------|-----------------|-------------|----------|-----------|
| **Claude Haiku 4.5** | 14 | 100% (14/14) | 0 | ~4,200K |
| **DeepSeek V3.2 Nitro** | 9 (+1 N/A) | ~83% | 2 failures | ~6,700K |

DeepSeek Nitro had two notable failures:
- **`witty-cheetah`** (issue #12, recipe CLI): Broke the API and hit merge conflicts. Consumed 13.5M tokens with no PR to show. Replaced by `happy-cheetah` (Haiku), which succeeded
- **`dusk-otter`** (issue #18, order validate/brew CLI): Got stuck on a `TypeError: Attempted to convert a callback into a command twice` error. Accumulated 11 daemon nudges without resolution. Replaced by `prime-otter` (Haiku), which succeeded

Both replacement workers were Haiku, confirming that the daemon's stuck detection and workspace agent's replacement spawning worked as designed. The workspace provided context about what went wrong, enabling the replacements to avoid the same pitfalls.

#### Strengths

- **Perfect acceptance score** -- the second run to achieve 100/100 after Sonnet 4.6 (single-model), and the first routed/multi-model run to do so
- **100% worker self-completion** -- 0 daemon force-removals across all 48 worker sessions (24 task + 24 verify)
- **Clean CI across all PRs** -- 0 CI failures, no merge conflict deadlocks
- **Verification system working** -- `verify-storm-platypus` correctly caught and rejected a destructive commit (deleted 1500+ lines of source code), forcing a proper retry
- **Wave 0 (gate) delivered cleanly** -- 4 gate workers (`silver-python`, `crimson-condor`, `golden-lion`, `solar-condor`) produced PRs #25-28, all merged
- **Storage and domain layer (Wave 1-2) was solid** -- `zealous-gecko` (atomic JSON writes), `zealous-otter` (repository interfaces), `mystic-lynx` (domain entities) all merged without issues

#### Weaknesses

- **Extremely high token consumption** (~192M) -- driven by several workers entering nudge/conflict loops:
  - `storm-platypus`: **25.0M tokens** (13% of total) -- 11 nudges, verification rejection, 5 conflict wakes, destructive commit caught by verifier
  - `witty-cheetah`: **13.5M tokens** (7% of total) -- failed worker, no PR produced
  - `crystal-crane`: **13.4M tokens** -- 10 nudges, confused by stale CI failure messages
- **Wave timing data lost** -- `run.sh` crashed after wave 4 due to a Bash 3.2 negative array index bug (`${WAVE_RESULTS[-1]}`), preventing `wave-timing.json` from being written. `collect.sh` fell back to PR-derived timing, showing all waves with identical ~81m durations instead of the actual ~8m/30m/26m/12m. Results were recovered by manually running `collect.sh`, `acceptance-test.sh`, and `summarize.sh`
- **Gate barely passed** (62/100, threshold 60) -- Test Rigor scored only 7/25 due to shared mutable globals and inconsistent data isolation in the model-generated blackbox test
- **Model-generated blackbox test scored 93.7% but exited with code 1** -- 3 FATAL errors from recipe name collisions across test modules caused by the `new_data_dir()` subshell foot-gun (a scaffold bug, not a model bug -- see `docs/known-issues.md`)
- **Convergence was not run** -- would have been triggered by the blackbox test's exit code 1, but would have chased test framework bugs rather than application bugs since the app was functionally correct

#### Token Usage

| Agent | Tokens | Notes |
|-------|--------|-------|
| `storm-platypus` | 24,968K | Highest -- 11 nudges, rejection, conflict loops, destructive commit |
| `witty-cheetah` | 13,458K | Failed worker, no PR, all tokens wasted |
| `crystal-crane` | 13,371K | 10 nudges, stale CI confusion |
| `wise-falcon` | 8,960K | Inventory CLI, above average |
| `witty-bison` | 7,135K | Order place CLI |
| `ultra-phoenix` | 7,051K | 7 nudges, rebase conflict |
| `zealous-gecko` | 6,664K | JSON storage |
| `ocean-platypus` | 6,381K | 6 nudges during coverage work |
| Workspace | 3,067K | -- |
| Supervisor | 6,992K | Elevated from monitoring stuck workers |
| Merge queue | 9,556K | 5% of total, elevated from conflict resolution |

**Total: ~192,054K** (workspace 3,067K + supervisor 6,992K + merge-queue 9,556K + workers 172,439K).

#### Comparison with Solo Runs

| Metric | DeepSeek Nitro (solo) | Haiku 4.5 (v2) | **Nitro + Haiku (routed)** |
|--------|----------------------|----------------|---------------------------|
| Acceptance | 74.8/100 | 74.6/100 | **100/100** |
| Gate | 62/100 | 62/100 | 62/100 |
| Issues Closed | 22/34 (65%) | 24/24 (100%) | 24/24 (100%) |
| PRs Merged | 19/38 (50%) | 30/30 (100%) | 24/24 (100%) |
| CI Pass Rate | 24/38 (63%) | 30/30 (100%) | 24/24 (100%) |
| Tokens | N/A | N/A | ~192M |

The routed combination significantly outperformed both models' solo runs on acceptance score (+25 over solo Nitro, +25 over solo Haiku v2). Haiku's 100% worker success rate complemented DeepSeek Nitro's speed and cost efficiency, with Haiku cleanly replacing the two failed Nitro workers. The combination achieved what neither model could individually -- a perfect score with all issues resolved.

#### Run Conditions

- Backend: direct
- Orchestration: Anthropic (`anthropic:claude-sonnet-4-6`)
- Workers: OpenRouter (`openrouter:deepseek/deepseek-v3.2:nitro`) and Anthropic (`anthropic:claude-haiku-4-5`)
- `run.sh` crashed after wave 4 (Bash 3.2 negative array index -- since fixed). Results recovered manually

---

### Routed: o4-mini + Gemini Flash + Sonnet 4.6

**Run:** Routed benchmark | 2026-04-17

| Metric | Value |
|--------|-------|
| Acceptance Score | **95/100** |
| Gate Score | **72/100** (passed; threshold 60) |
| Duration | ~2h 28m |
| PRs | 42 created, 5 merged (12%) |
| Issues | 26 total, 26 closed (100%) |
| Workers | 42 total (26 issues + 16 replacements) |
| Convergence | PASS (1 iteration) |
| Tokens | ~204,000K (~204M) |

**Configuration:** Sonnet 4.6 for orchestration (workspace, supervisor, merge-queue). Workers routed between `openai:o4-mini`, `google_genai:gemini-2.5-flash`, and `anthropic:claude-sonnet-4-6`. First three-model routed run.

#### Model Reliability Comparison

| Model | Workers | Nudged 10+ | Nudged 20+ | PRs Merged | pr_create errors |
|-------|---------|------------|------------|------------|------------------|
| **Gemini 2.5 Flash** | 16 | 14 (88%) | 9 (56%) | 0 | 4 |
| **o4-mini** | 12 | 9 (75%) | 4 (33%) | 2 | 3 |
| **Sonnet 4.6** | 14 | 0 (0%) | 0 (0%) | 3 | 0 |

#### Strengths

- **Best gate score (72/100) among routed runs** -- previous routed runs scored 22/100 (Scout + Nitro) and 62/100 (Nitro + Haiku)
- **First convergence pass for a routed run** -- converged in 1 iteration. Previous routed runs either timed out (Scout + Nitro) or skipped convergence (Nitro + Haiku)
- **0 CI failures** across all 24 merged PRs
- **Sonnet 4.6 workers were flawless** -- 0 high-nudge workers, 0 `pr_create` errors, 3 PRs merged. Zero workers required replacement

#### Weaknesses

- **Flash severely underrepresented in merged output** -- 0 PRs merged from 16 Flash workers (including replacements for stuck workers). 88% of Flash workers were nudged 10+ times, 56% hit 20+ nudges. Flash workers exhibited `pr_create` tool confusion (4 errors), treating it as a text generation task rather than a tool call
- **4 workers hit the 21-nudge cap** and were replaced, all from Flash or o4-mini
- **High token consumption (~204M)** -- comparable to the Nitro + Haiku run (~192M) despite fewer merged PRs, driven by replacement worker churn and nudge loops
- **Only 5/42 PRs merged (12%)** -- the lowest merge rate of any completed routed run, reflecting the high replacement rate

#### Wave Timeout Root Cause

All 4 waves timed out due to replacement worker churn rather than model incapability:

- **42 task workers spawned for 26 issues** -- 16 workers were replacements for stuck workers, a 62% overhead rate
- **Gemini 2.5 Flash:** 0% PR merge rate, 88% stuck rate (nudged 10+). Flash workers consistently confused the `pr_create` tool, attempting to generate PR descriptions as text output instead of invoking the tool. 4 workers produced explicit `pr_create` errors
- **o4-mini:** Moderate performance -- 75% nudge rate (10+), 33% hit 20+ nudges. 2 PRs merged. Functional but slow, requiring more nudges than expected for straightforward tasks
- **Sonnet 4.6:** Zero high-nudge workers, 3 merged PRs, 0 errors. Consistent with its solo benchmark performance
- **5 verify workers hit `ModuleNotFoundError`** -- skipped virtual environment setup (`source .venv/bin/activate`) before running tests, causing immediate failures on import

#### Comparison with Solo Runs

| Metric | o4-mini (solo) | Flash (solo) | Sonnet 4.6 (solo) | **o4-mini + Flash + Sonnet (routed)** |
|--------|---------------|-------------|-------------------|---------------------------------------|
| Acceptance | N/A | N/A | 100/100 | **95/100** |
| Gate | N/A | N/A | 82/100 | **72/100** |
| Convergence | N/A | N/A | N/A | **PASS (1 iter)** |
| Tokens | N/A | N/A | ~2,618K | **~204M** |

#### Comparison with Prior Routed Runs

| Metric | Scout + Nitro | Nitro + Haiku | **o4-mini + Flash + Sonnet** |
|--------|--------------|---------------|------------------------------|
| Acceptance | 52/100 | 100/100 | **95/100** |
| Gate | 22/100 | 62/100 | **72/100** |
| Convergence | TIMEOUT (3 iter) | N/A (skipped) | **PASS (1 iter)** |
| Duration | ~3h 46m | ~92 min | **~2h 28m** |
| Workers | 6 | 48 | **42** |
| PRs Merged | 4/6 (67%) | 24/24 (100%) | 5/42 (12%) |
| Tokens | N/A | ~192M | **~204M** |

The three-model routed run achieved the best gate score and first convergence pass among routed runs, but the lowest PR merge rate. The primary bottleneck was Flash worker reliability -- removing Flash and redistributing its allocation to o4-mini and Sonnet 4.6 would likely improve merge throughput significantly. Sonnet 4.6 workers were the clear reliability anchor, matching their solo-run performance.

#### Run Conditions

- Backend: direct
- Orchestration: Anthropic (`anthropic:claude-sonnet-4-6`)
- Workers: OpenAI (`openai:o4-mini`), Google (`google_genai:gemini-2.5-flash`), Anthropic (`anthropic:claude-sonnet-4-6`)

---

### Routed: GPT 5.4 Mini + Nano + Haiku 4.5

**Run:** Routed benchmark | 2026-04-16

| Metric | Value |
|--------|-------|
| Acceptance Score | **100/100** (36/36 passed) |
| Gate Score | **78/100** (passed; threshold 60) |
| Duration | ~1h 57m |
| PRs | 27 created, 26 merged (96%) |
| Issues | 24 total, 24 closed (100%) |
| Workers | 27 total, 100% self-completion |
| Convergence | PASS (3 iterations) |
| Tokens | ~167,000K (~167M) |

**Configuration:** Sonnet 4.6 for orchestration (workspace, supervisor, merge-queue). Workers routed between `openai:gpt-5.4-mini` (10 assigned), `openai:gpt-5.4-nano` (9 assigned), and `anthropic:claude-haiku-4-5` (8 assigned).

**The second multi-model run to achieve a perfect 100/100 acceptance score**, and the first using OpenAI GPT 5.4 family models as workers.

#### Model Reliability Comparison

| Model | Workers | Self-Completion | PRs Merged | Notable Issues |
|-------|---------|-----------------|------------|----------------|
| **GPT 5.4 Mini** | 10 | 100% | 10/10 | quantum-platypus CI failure (PR #45 closed) |
| **GPT 5.4 Nano** | 9 | 100% | 8/9 | kind-squirrel high token usage (19.9M) |
| **Claude Haiku 4.5** | 8 | 100% | 8/8 | Clean across the board |

All 27 workers self-completed with zero daemon force-removals. 100% worker autonomy.

#### Strengths

- **Perfect acceptance score** -- all 36 reference acceptance tests passed. Second routed run to achieve 100/100 after DeepSeek Nitro + Haiku
- **Best gate score (78/100) of any routed run** -- previous bests were 72/100 (o4-mini + Flash + Sonnet) and 62/100 (Nitro + Haiku)
- **Convergence passed in 3 iterations** -- the convergence loop fixed a brew command issue via `light-elk` (PR #51) and `crimson-phoenix` (PR #52)
- **All three OpenAI GPT 5.4 models functional** -- both Mini and Nano worked as OAT workers, a first for the GPT 5.4 family in the worker role
- **Merge-queue correctly superseded stale PRs** -- closed PR #45 (quantum-platypus, persistent CI failures + merge conflicts) after PR #51 delivered the same fix

#### Weaknesses

- **Wave 3 timeout** caused by `quantum-platypus` (gpt-5.4-mini) failing CI repeatedly on PR #45 for issue #18 (Order Validate and Brew). The worker attempted ~6 CI fix cycles over ~45 minutes but kept failing on a broken test fixture. PR #45 was ultimately closed by the merge-queue as superseded after `light-elk` (PR #51) landed the same functionality via the convergence loop
- **Stale verifier lock bug** -- `calm-bear` hit "agent 'verify-calm-bear' is still running" **8 times** in a loop. The verifier had already completed (`ReadyForCleanup = true`) but its process was still alive during post-completion cleanup delay. The alive-check didn't test `ReadyForCleanup` first. *(Fixed in this release -- see Section 1 of post-benchmark fixes)*
- **23 "already completed" errors** -- race condition between daemon auto-completion and agent self-completion. Agents received hard errors when trying to call `oat agent complete` after the daemon had already marked them `ReadyForCleanup`. *(Softened to return success in this release -- see Section 2 of post-benchmark fixes)*
- **High token consumption (~167M)** -- driven by missing `max_input_tokens` in GPT 5.4 model profiles, causing the summarization middleware to use conservative fixed defaults (trigger at 170K instead of 85% of 400K = 340K, keep only 6 messages instead of proportional). This meant agents lost context earlier than necessary, potentially re-reading files and repeating work

#### Token Usage

Token counts are cumulative across all LLM API calls per agent session (not single-call context). GPT 5.4 Mini and Nano have 400K context windows; each API call sends the full conversation history.

| Agent | Model | Tokens | Notes |
|-------|-------|--------|-------|
| `kind-squirrel` | gpt-5.4-nano | 19,900K | Highest -- 14 token emissions, ~10 LLM calls per emission |
| `quantum-platypus` | gpt-5.4-mini | 16,400K | 22 token emissions over ~45 min CI fix attempts |
| `calm-bear` | claude-haiku-4-5 | ~8,000K | Hit stale verifier lock 8x |
| Workspace | claude-sonnet-4-6 | ~3,000K | -- |
| Supervisor | claude-sonnet-4-6 | ~5,000K | -- |
| Merge queue | claude-sonnet-4-6 | ~7,000K | Active PR management, superseded PR #45 |

**Why tokens are so high:** Each LLM invocation sends the full conversation history. Within a single "stream pass" the agent may chain 5-10 tool calls (read file -> edit -> run tests -> read output -> edit again), each triggering a new LLM call at up to 400K context. A stream pass with ~10 invocations at ~400K each produces ~4M input tokens in one pass alone. Over 14-22 passes, cumulative totals reach 16-20M. The missing `max_input_tokens` profile exacerbated this by triggering summarization too early (at 170K fixed default vs 340K = 85% of 400K), causing agents to lose context and redo work.

**Total: ~167,000K** (workspace ~3,000K + supervisor ~5,000K + merge-queue ~7,000K + workers ~152,000K).

#### Wave 3 Timeout: PR #45 Deep Dive

`quantum-platypus` (gpt-5.4-mini) was assigned issue #18 (Order Validate and Brew). It created PR #45 and passed initial code review, but CI failed due to a broken test fixture. Over ~45 minutes and 22 API calls, the worker repeatedly attempted to fix CI but kept failing on the same fixture. The pattern:

1. Run tests -> fixture error
2. Edit test file to fix fixture
3. Push fix -> CI still fails (different fixture issue or regression)
4. Repeat

Meanwhile, the convergence loop spawned `light-elk` which independently implemented the same feature and created PR #51 (merged successfully). The merge-queue then closed PR #45 as superseded (same functionality already on `main` via PR #51). `quantum-platypus` received the "PR closed" daemon notification, called `oat agent complete --force`, and was cleaned up within ~1 minute.

#### Comparison with Prior Routed Runs

| Metric | Scout + Nitro | Nitro + Haiku | o4-mini + Flash + Sonnet | **Mini + Nano + Haiku** |
|--------|--------------|---------------|--------------------------|-------------------------|
| Acceptance | 52/100 | 100/100 | 95/100 | **100/100** |
| Gate | 22/100 | 62/100 | 72/100 | **78/100** |
| Convergence | TIMEOUT (3 iter) | N/A (skipped) | PASS (1 iter) | **PASS (3 iter)** |
| Duration | ~3h 46m | ~92 min | ~2h 28m | **~1h 57m** |
| Workers | 6 | 48 | 42 | **27** |
| PRs Merged | 4/6 (67%) | 24/24 (100%) | 5/42 (12%) | **26/27 (96%)** |
| Tokens | N/A | ~192M | ~204M | **~167M** |

Best overall routed run: highest gate score, best PR merge rate (96%), lowest token usage, fastest duration, fewest workers needed. The GPT 5.4 family + Haiku combination proved more efficient than both the Nitro + Haiku and o4-mini + Flash + Sonnet configurations.

#### Run Conditions

- Backend: direct
- Orchestration: Anthropic (`anthropic:claude-sonnet-4-6`)
- Workers: OpenAI (`openai:gpt-5.4-mini`, `openai:gpt-5.4-nano`), Anthropic (`anthropic:claude-haiku-4-5`)
- Stale verifier lock bug discovered and fixed post-run
- "Already completed" race condition identified and softened post-run

---

### Llama 4 Maverick: Provider Investigation

**Status:** Not benchmarkable via OpenRouter. Investigated April 2026.

During investigation of whether Groq could fix Llama 4's tool-calling issues (as it did for simple Scout probes), we tested Maverick's availability across OpenRouter providers.

#### Provider Availability

| Provider | Hosts Maverick | Tool Call Support | Notes |
|----------|---------------|-------------------|-------|
| **Google Vertex AI** | Yes | Yes (9.45% error rate) | Only provider with tool calls |
| **DeepInfra** | Yes | No | Text-only, 404 on tool use |
| **Together** | Yes | No | Text-only, 404 on tool use |
| **Parasail** | Yes | No | Text-only, 404 on tool use |
| **NovitaAI** | Yes | No | Text-only, 404 on tool use |
| **SambaNova** | Yes | No | Text-only, 404 on tool use |
| **Groq** | **No** | N/A | Does not host Maverick |

#### Key Findings

- **Google Vertex AI** is the only OpenRouter provider for Maverick that supports tool calls, but with a 9.45% error rate. OpenRouter's shared Google quota is heavily rate-limited -- requests frequently return `429 Temporarily rate-limited upstream`.
- **BYOK (Bring Your Own Key)** for Vertex AI requires a GCP service account JSON key (not a simple API key), making it impractical for quick benchmarking.
- **Groq does not host Maverick at all** -- requests return 404. This eliminates the hypothesis that Groq's tool-call parsing could fix Maverick the way it partially fixed Scout.
- **All other providers** host Maverick for text generation only. Requesting tool use returns `404 - No endpoints found that support tool use`.

#### Conclusion

Maverick remains untestable for OAT via OpenRouter. The only viable path would be self-hosted vLLM with `--tool-call-parser llama4_pythonic`, which has not been attempted. Given that Scout (which *does* work for simple tool calls via Groq) still fails at scale in OAT's complex agent context, it's likely that Maverick would exhibit the same behavior even with correct tool-call parsing.
