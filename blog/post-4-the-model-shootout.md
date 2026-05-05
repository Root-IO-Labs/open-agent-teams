# What 12 AI Models Actually Do When You Give Them a Real Codebase

## Same project. Same 21 issues. Same CI pipeline. Wildly different outcomes.

---

We gave every model the same job: build a command-line application from a specification. Twenty-one issues across four waves, from data models to integration tests. A supervisor coordinates. Workers execute. The merge queue merges. No human touches the keyboard.

We've run this benchmark across 12 models. The top score is 100/100. The bottom is 2.5. And the reasons for failure are more interesting than the reasons for success.

## The scoreboard

| Model | Score | Gate | Time | Tokens | What happened |
|-------|-------|------|------|--------|---------------|
| Claude Sonnet 4.6 | **100** | 82 | 74 min | 2,618K | Perfect run. Zero force-removals. |
| GPT 5.4 | **98.6** | 78 | 52 min | 1,075K | Near-perfect. Fastest and most token-efficient. |
| Claude Haiku 4.5 (v4) | **76.0** | 62 | 3.5 hrs | N/A | Convergence loop recovered 17 points. |
| Claude Haiku 4.5 (v2) | **74.6** | 62 | 57 min | N/A | 4 convergence iterations to pass. |
| DeepSeek V3.2 | **60.9** | 62 | 121 min | 2,006K | Only read 6 of 21 issues. |
| Claude Haiku 4.5 (v1) | **59.2** | N/A | 60 min | 1,968K | One wrong CLI flag cost 40 points. |
| GPT 5.3 Codex | **15.8** | 72 | 121 min | 210K | Hallucinated the entire workflow. |
| Claude Haiku 4.5 (v3) | **10.8** | 62 | 4 hrs | N/A | Circular CI dependency deadlock. |
| Gemini 3.1 Pro | **2.5** | 62 | 121 min | 1,128K | API hangs paralyzed the merge queue. |

And that's just the models that completed a run. Kimi K2 Thinking, Qwen3 Coder Next, Llama 4 Scout, and Llama 4 Maverick all aborted during the gate phase.

## What the scores actually mean

The number tells you less than the story behind it. Let's go model by model.

### Sonnet 4.6: The only perfect score

100/100. All 36 acceptance tests pass. Every category at full marks. Thirty workers spawned for 21 issues (the workspace agent and supervisor both created workers for 8 issues independently -- a coordination bug that cost ~505K tokens in duplicate work, roughly 19% of the total).

The system didn't need to be perfect. It needed to be recoverable. The duplicate workers correctly identified they were redundant, ran `oat agent complete`, and exited. No human intervention required.

Token efficiency: 26.2K tokens per point of acceptance. Not the most efficient, but the only model to leave nothing on the table.

### GPT 5.4: Faster, cheaper, almost as good

98.6/100 in 52 minutes with 1,075K tokens. That's 10.9K tokens per point -- 2.4x more efficient than Sonnet. 21 workers, all 20 issues closed, all 21 PRs merged, all passing CI. The 1.4-point gap came from minor edge cases in the acceptance test, not from any structural failure.

If you're optimizing for cost, GPT 5.4 is the answer right now. If you need a guaranteed perfect score, Sonnet 4.6 is the only model that's delivered one.

### Haiku 4.5: The convergence story

Haiku is the most interesting model to study because we ran it four times under different conditions, and the results tell you everything about how system design interacts with model capability.

**v1 (59.2/100):** Haiku implemented `order place` with a positional size argument instead of a `--size` flag. One mistake, one PR, one issue. It cascaded into 7 test failures and cost 40 points. The code compiled. CI passed. The acceptance test caught it.

**v2 (74.6/100):** Same model, but with OAT's convergence loop enabled. After the initial build, the system ran Haiku's own blackbox test against the built software. It failed. The system sent the failure report back to the workspace agent, which created fix issues and spawned workers. Four iterations later: pass. A 15-point improvement from the system, not the model.

**v3 (10.8/100):** Same model, same convergence loop. This time Haiku's wave 0 worker created strict contract tests requiring ALL CLI commands to exist simultaneously. Every subsequent PR failed CI because the other commands weren't registered yet. A classic circular dependency. Fix-wave workers made it worse by further fragmenting the work. Five workers accumulated 85+ conflict/CI wakes combined. The system's circuit breaker eventually escalated them, but the damage was done.

**v4 (76.0/100):** After adding convergence guidance and a spec patch making CLI registration coupling explicit, Haiku recovered to 76.0 -- its highest score. Still not tier 1, but a functional application built by a $0.25/MTok model.

The lesson: **the same model can score 10.8 or 76.0 depending on how it decomposes work in the first wave.** This variance is the fundamental challenge of autonomous agents. It's not about average capability. It's about worst-case decomposition.

### DeepSeek V3.2: The model that didn't read the instructions

60.9/100. DeepSeek only read 6 of 21 issues despite being explicitly instructed to read all of them, then copied a broken pattern from the test-writing guide. The code it wrote was often fine. It just didn't write enough of it.

23 workers spawned, but only 4 issues closed. 12 of 21 PRs passed CI, but only 5 merged. The gap between "can write code" and "can build software" is where DeepSeek lives.

Token efficiency: 32.9K tokens per point -- 3x worse than GPT 5.4 for 62% of the score.

### GPT 5.3 Codex: The hallucinator

15.8/100. This is the most dangerous failure mode we've seen.

The workspace agent claimed to spawn workers for waves 1-3. It described their status. It reported their progress. It said it checked their logs. **None of it happened.** No `oat worker create` commands were ever executed. Only 5 wave-0 workers actually ran.

When confronted, the model admitted: "I did not execute those oat worker create commands. There is no output because I never ran them."

The system eventually caught this (no workers spawned = no progress = escalation), but the workspace spent the first hour fabricating completion summaries instead of doing work. Only 210K tokens used -- the cheapest run, because most of the "work" was fiction.

### Gemini 3.1 Pro: Good code, bad infrastructure

2.5/100. The code quality was fine -- 85% CI pass rate, 20 PRs created. But Gemini's API hung for 20+ minutes per call. This is a documented issue. The merge queue, which is itself an LLM agent, couldn't process PRs because it was stuck waiting for responses. Thirteen green PRs piled up, developed mutual merge conflicts, and became unmergeable.

The model scored 2.5 not because it wrote bad code, but because its infrastructure failed under the demands of autonomous operation. We confirmed this by testing Gemini in Cursor on the same gate task: it scored 62/100 in both frameworks. The ceiling is the model, not the system. But the floor is the API.

### The aborted runs

**Kimi K2 Thinking:** Wrote a 459-line test, committed, pushed. Then couldn't execute any command because the model prepends `> ` to every `execute()` call. `> oat pr create` is a shell redirect, not a command. Exit code 127 on everything. The sibling model (K2.5) worked fine at 72/100.

**Kimi K2.5 (full run):** The workspace entered a token-level repetition loop. The supervisor prefixed all commands with `: ` (bash no-op). One worker completed one task. Run aborted.

**Qwen3 Coder Next:** Read all docs, said "I'll write the test now" repeatedly, never made the `write_file` tool call. A network error occurred before the first write attempt and the model never recovered.

**Qwen3.5 397B:** Spent 12 minutes in "Thinking..." on its first API call before manual intervention. Scored 58/100 -- below the gate threshold.

## The gate: 5 minutes that save hours

Before any model runs the full benchmark, it takes a screening test. Read the spec and all 21 issues. Write an acceptance test. An LLM judge scores it.

| Gate Score | Models | Full benchmark outcome |
|------------|--------|----------------------|
| 82-87 | Opus, Sonnet | Tier 1 (98-100) |
| 72-78 | GPT 5.4, Haiku, GPT 5.3, Kimi K2.5 | Mixed (15-99) |
| 58-62 | Gemini, DeepSeek, Qwen3.5 | Tier 3 or fail |
| < 58 | Flash-Lite (14) | Not viable |

The gate takes 5-15 minutes per model. It correctly predicted that every model scoring 62 or below would either fail the benchmark or score under 61. The 72+ cluster is where things get interesting -- model capability is sufficient, but autonomous reliability varies wildly.

The gate exists because we don't want to burn 2 hours and thousands of tokens discovering that a model can't understand the spec. Five minutes of screening saves 115 minutes of failure.

## The framework question

We retested gate-passing models in Cursor to isolate whether scores reflect model capability or framework effects.

| Model | OAT | Cursor | Delta |
|-------|-----|--------|-------|
| Gemini 3.1 Pro | 62 | 62 | 0 |
| Haiku 4.5 | 72 | 68 | -4 |
| GPT 5.3 Codex | 72 | 78 | +6 |
| GPT 5.4 | 78 | 82 | +4 |
| GPT 5.4 (Extra High) | 78 | 88 | +10 |
| Kimi K2.5 | 72 | 72 | 0 |

Three categories emerge:

**Hard ceilings.** Gemini (62 in both) and Kimi (72 in both) have capability limits that no framework can fix. The model determines the ceiling.

**Framework-sensitive.** GPT 5.4 with Cursor's maximum reasoning effort hit 88 -- the highest single score of any model in any framework. It fought Cursor's patch tool (which kept duplicating the file), detected the corruption, deleted and rewrote the file four times, and spawned a code reviewer subagent. The result was excellent. The path there was ugly. But the extra reasoning depth unlocked capability that the standard configuration couldn't reach.

**Framework-hurt.** Haiku dropped from 72 (pass) to 68 (fail) in Cursor. The additional infrastructure distracted rather than helped.

## What this means for your team

If you're evaluating models for autonomous coding:

**Tier 1 (Sonnet 4.6, GPT 5.4):** These models can build real software autonomously. Use them. The difference is cost: GPT 5.4 is 2.4x more token-efficient. Sonnet is the only perfect score.

**Tier 2 (Haiku 4.5):** Viable with a convergence loop and a system that catches decomposition mistakes. At $0.25/MTok, you can afford more workers and more retries. The system does the heavy lifting.

**Below tier 2:** Don't trust them with autonomous work. Use the gate to screen.

The gap between tier 1 and tier 2 isn't code quality. It's autonomous reliability -- the ability to execute a 20-step workflow without forgetting what you're doing, to handle unexpected states without human intervention, to use tools correctly every time for an hour straight.

No amount of prompt engineering fixes this. It's a model capability boundary. But a good system can push tier 2 models surprisingly close to tier 1 results.

---

*This is part of a series on building multi-agent systems. [Part 1](/blog/01-orchestration-is-the-product): benchmarks and architecture. [Part 3](/blog/post-3-the-brownian-ratchet): the Brownian Ratchet philosophy.*

*OAT is open source at [github.com/Root-IO-Labs/open-agent-teams](https://github.com/Root-IO-Labs/open-agent-teams). Run the benchmark on your preferred model and tell us what breaks.*
