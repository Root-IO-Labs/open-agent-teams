# Stop Tuning Prompts. Fix Your System.

## The data says your AI agent's biggest problem isn't the model.

---

There's a belief in the AI engineering community that goes something like this: if your agent isn't performing well, you need a better model. Or a better prompt. Or more reasoning tokens. Or a bigger context window.

We have data that says otherwise.

## One model, four scores

We ran Claude Haiku 4.5 on the same benchmark four times. Same model. Same weights. Same 21-issue project. Same CI pipeline.

| Run | Score | What changed |
|-----|-------|-------------|
| v1 | 59.2 | Baseline OAT |
| v2 | 74.6 | Added convergence loop |
| v3 | 10.8 | Same as v2 (bad decomposition luck) |
| v4 | 76.0 | Added convergence guidance + spec patch |

The model didn't change. The system around it did. And the scores swung from 10.8 to 76.0 -- a 65-point range from the same model.

v3 is the one that should bother you. Same model, same system as v2, and the score dropped from 74.6 to 10.8. What happened? The model's wave 0 worker created strict contract tests that required ALL CLI commands to exist simultaneously. Every subsequent PR failed CI because the other commands weren't registered yet. Circular dependency. Deadlock. Thirteen PRs stuck in a rebase loop.

v4 fixed this not by improving the model but by adding guidance to the system: a spec patch making CLI registration coupling explicit, and convergence prompts that help the workspace recognize circular dependencies.

The prompt tuning community would tell you to fix this by adding "don't create circular dependencies" to the system prompt. We tried that. It doesn't work. LLMs don't reliably follow negative instructions across a multi-hour autonomous workflow. What works is building a system that detects circular dependencies and intervenes.

## The infrastructure failure that cost 97 points

Gemini 3.1 Pro scored 2.5/100 on our benchmark. Its code quality was fine -- 85% CI pass rate, 20 PRs created. But the API hung for 20+ minutes per call. The merge queue, which runs as an LLM agent, couldn't process any PRs because it was waiting for an API response that never came. Thirteen green PRs piled up, developed mutual merge conflicts, and became unmergeable.

We confirmed the model ceiling separately: Gemini scored 62/100 on the gate test in both OAT and Cursor. The model writes mediocre-but-functional code. But in the full benchmark, infrastructure unreliability turned mediocre into catastrophic.

No prompt will fix API latency. No reasoning tokens will unstick a hung HTTP connection. This is a systems problem: critical-path agents need timeouts, fallbacks, and recovery mechanisms that don't depend on the LLM being responsive.

## The $0.25 model that almost matched the $15 model

Haiku 4.5 costs roughly $0.25/MTok (blended). Sonnet 4.6 is roughly 60x more expensive per token. On our benchmark:

- **Haiku v4 with convergence:** 76.0/100
- **Sonnet without convergence:** 100/100

Sonnet gets there in one shot. Haiku needs a system that lets it iterate. But 76% of a perfect score at 1.7% of the token cost is a real engineering trade-off. If your system can tolerate iteration, cheap models plus good infrastructure beat expensive models plus bad infrastructure.

The math changes when you factor in wall clock time. Haiku v4 took 3.5 hours with convergence. Sonnet took 74 minutes without it. If time matters more than money, use the better model. If you're running overnight batch jobs, use the cheap model and let the system iterate.

## The framework comparison

We tested the same models in both OAT and Cursor to isolate framework effects.

Three patterns emerged:

**Models with hard ceilings don't benefit from better frameworks.** Gemini 3.1 Pro scored 62 in both OAT and Cursor. Kimi K2.5 scored 72 in both. Their limitations are in the weights, not the tooling.

**Some models respond to better scaffolding.** GPT 5.4 went from 78 (OAT) to 88 (Cursor, Extra High reasoning). The extra reasoning depth unlocked capability that the standard configuration couldn't reach. But it cost 352K+ tokens and involved the model fighting Cursor's patch tool, detecting file corruption, and spawning a reviewer subagent. The score was excellent. The path was ugly.

**More infrastructure can hurt.** Haiku dropped from 72 (OAT) to 68 (Cursor). The additional system prompts and plugins distracted rather than helped. For models at the boundary of capability, complexity is a tax, not a gift.

## What actually moves the needle

Based on 12 models and hundreds of benchmark runs, here's what improves agent performance -- ranked by impact:

### 1. Isolation (eliminates an entire class of bugs)

Every agent gets its own git worktree. Five agents can edit the same file simultaneously without conflicts. Conflicts only surface at PR time, where CI is the arbiter. Our early version used branches in a shared checkout. One agent's `git stash` would clobber another's working state. Worktrees eliminated this completely.

This isn't a prompt improvement. It's a `git worktree add` command in the daemon.

### 2. Recovery (stuck agents are inevitable)

Agents get stuck. Every model, every run. The question is what happens next.

Our three-tier escalation:
- Minutes 0-8: status nudges (normal operation)
- Minutes 8-14: supervisor investigates (targeted intervention)
- Minutes 16-38: daemon takeover with zero-token recovery (git state checks, auto-completion)
- Minute 40: hard removal

The key: recovery at the 20-minute mark uses shell commands, not LLM calls. Check git state. Check PR state. Check process health. If the worker has an open PR, promote it to complete. This prevents recovery from compounding costs.

In our GPT 5.4 run, one worker received 11 unnecessary nudges due to a dormancy race condition. The supervisor caught it, intervened, and the worker completed its task. Total impact: some wasted nudge tokens. In a system without escalation, that's a permanently stuck agent burning API credits until timeout.

### 3. Quality gates (CI is the ratchet)

Multiple agents work simultaneously. Some produce great work. Some produce garbage. CI filters. Only PRs with passing tests merge. Progress is permanent.

This is the Brownian Ratchet philosophy: chaos in, progress out. But it only works if you never weaken the gate. The moment you bypass CI for a flaky test or skip review because the agent "probably got it right," the ratchet breaks. OAT hardcodes this in the agent prompts: **never weaken CI.** It's not configurable because it shouldn't be.

### 4. Convergence loops (let cheap models iterate)

Run the model's own test against its output. If it fails, send the failure back and let it fix the issues. Repeat up to 4 times.

This turned Haiku from 59.2 (one-shot) to 76.0 (with convergence). A 17-point improvement from a system feature, not a model upgrade.

### 5. Screening (don't waste money on bad models)

Our gate test takes 5-15 minutes per model. It correctly predicted that every model scoring below 62 would fail the full benchmark. Screening saves 2+ hours and thousands of tokens per model that would have failed anyway.

## The uncomfortable truth

The AI industry is investing billions in making models smarter. That matters -- the gap between Sonnet (100/100) and Haiku (76/100) is real, and it comes from model capability.

But the gap between Haiku-in-a-good-system (76) and Haiku-in-a-bad-system (10.8) is larger. And the gap between Gemini-with-working-infrastructure (62) and Gemini-with-broken-infrastructure (2.5) is larger still.

Model capability determines the ceiling. **The system determines how close you get to it.**

If your agent isn't performing well, check the system before you upgrade the model. Is it isolated? Does it recover from stuck states? Does CI actually run and actually block bad code? Can it iterate on failures? Have you verified that your verification actually works?

The models will keep getting better. But right now, the cheapest performance improvement for most teams isn't a model upgrade. It's a systems upgrade.

---

*OAT is open source at [github.com/Root-IO-Labs/open-agent-teams](https://github.com/Root-IO-Labs/open-agent-teams). This is the final post in our launch series. [Start from the beginning](/blog/01-orchestration-is-the-product) or [read about the Brownian Ratchet](/blog/post-3-the-brownian-ratchet).*
