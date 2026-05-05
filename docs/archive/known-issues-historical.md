# Known Issues

> Last updated: 2026-04-24

This file tracks issues that are still open. For the full history of 75+ resolved issues from the pre-v0.1 development cycle, see the [archived record](history/known-issues-pre-v0.1.md) and [CHANGELOG.md](../CHANGELOG.md).

## How to report new issues

File at [github.com/Root-IO-Labs/open-agent-teams/issues](https://github.com/Root-IO-Labs/open-agent-teams/issues).

---

## Open provider/model observations

These are upstream model or provider behaviors that affect OAT runs but cannot be fixed with OAT code changes. Documented here so users know what to expect.

### OpenRouter latency with Gemini models

OpenRouter adds significant latency when routing to Gemini models — hangs of 10+ minutes per API call observed. **Workaround:** use `google_genai:` prefix to call Google's API directly instead of `openrouter:google/`.

### Gemini 3.1 Flash-Lite: insufficient agentic capability

Flash-Lite lacks the multi-step tool-use capability needed for OAT's merge-queue workflow. The merge-queue never executed a `gh pr merge` despite explicit messages. **Recommendation:** Flash-Lite class models are not suitable for OAT agent roles.

### Llama 4 (Scout / Maverick): tool calling broken via OpenRouter

Both Llama 4 models emit tool calls as plain text instead of structured `tool_calls` objects — a known model-family issue ([meta-llama/llama-api-python#31](https://github.com/meta-llama/llama-api-python/issues/31), [vllm#17109](https://github.com/vllm-project/vllm/issues/17109)). All OAT runs failed. Self-hosted vLLM with `--tool-call-parser llama4_pythonic` is the only untested option.

### DeepSeek V3.2 Speciale: no tool-use support on OpenRouter

OpenRouter does not expose function calling endpoints for the Speciale variant (`404 - No endpoints found that support tool use`). **Workaround:** use standard DeepSeek V3.2, or access Speciale via a direct API that supports function calling.

### DeepSeek V3.2: context window exhaustion (163K limit)

Workers on complex tasks can hit the 163,840-token limit after many nudge/conflict resolution cycles. **Recommendation:** models with < 200K context windows may struggle with long implementation tasks. Consider worker restart strategies.

### Kimi K2 Thinking: `> ` prefix on every tool call

Kimi K2 Thinking prepends `> ` to every `execute()` call, which the shell interprets as a redirect, breaking all commands. The sibling model Kimi K2.5 does not have this issue and scored 72/100. **Recommendation:** avoid K2 Thinking for agentic workflows; use K2.5.

### Gemini 2.5 Flash and o4-mini: poor OAT workflow adherence

In routed benchmarks, Gemini 2.5 Flash merged 0/16 PRs (88% of workers stuck) and o4-mini merged 2/12 (75% stuck). Both models frequently failed the commit-push-review cycle or tried nonexistent tools. Claude Sonnet 4.6 handling the same tasks merged 3/14 with 0% stuck rate. **Recommendation:** avoid these models as the primary worker model for full benchmark runs.

---

## Resolved issues

75 issues documented during the pre-v0.1 development cycle have been fixed and verified. Highlights include:

- Daemon socket wedge cascade with timeouts, bounded handlers, and nuke command
- Merge-queue blind merge (CI checks not validated before merge)
- Worker dormancy cap with force-merge for green PRs
- Post-completion token waste (agents looping after complete)
- Nudge loop clobbering dormancy state (read-modify-write race)
- Stuck worker escalation ladder (3-tier: nudge, supervisor, daemon takeover)
- Direct backend agent interrupt via ESC + streaming idle timeout
- OpenRouter API hang recovery (`OAT_STREAM_IDLE_TIMEOUT`, `OAT_API_TIMEOUT`)
- Infinite reasoning detection (`OAT_MAX_THINKING_SECONDS`)

For the full list with fix details and file references, see [docs/history/known-issues-pre-v0.1.md](history/known-issues-pre-v0.1.md).
