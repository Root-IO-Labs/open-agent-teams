# OAT Roadmap

This document defines the project direction. **All work must align with this roadmap.**

## Mission

**OAT (Open Agent Teams) is a lightweight local orchestrator for running multiple AI coding agents on GitHub repositories, powered by OAT Agent Runtime.**

Key constraints:
- **Local-first**: No cloud dependencies, remote coordination, or external services
- **Model-agnostic**: Uses OAT Agent Runtime, which supports any LLM provider
- **Simple**: Prefer deleting code over adding complexity
- **Terminal-native**: No web dashboards, GUIs, or browser-based interfaces

## Operational Principles

1. **Zero Repo Requirements**: Users can use oat without adding anything to their repository. Repo-level customization via `.oat/` is optional.

2. **Self-Contained State**: All oat state lives in `$HOME/.oat/`. Agent session state is managed by OAT Agent Runtime's SQLite checkpointer.

3. **Optional Repo Config**: If users want repo-specific behavior, they can add a `.oat/` directory to their repo.

4. **Prompt Injection via `.oat/AGENTS.md`**: Agent-specific system prompts are written to `.oat/AGENTS.md` in each agent's Git worktree. OAT Agent Runtime's `MemoryMiddleware` picks these up automatically.

## Phase 1: Core Port (Current)

Focus: Make the OAT Agent Runtime-based orchestration work reliably.

### P0 - Must Have
- [x] **OAT Agent Runtime integration**: Replace legacy dependencies with OAT Agent Runtime for agent execution
- [x] **Session management**: Use `--thread-id` for new sessions, `--resume` for restarts
- [x] **Prompt injection**: Write agent prompts to `.oat/AGENTS.md` in worktrees
- [x] **Reliable worker lifecycle**: Workers start, complete, and clean up without manual intervention (escalating nudges, supervisor notification, daemon-initiated termination of stuck workers)
- [x] **Crash recovery**: Full crash-recovery cycle with `--resume`, health checks every 2 min, and auto-restart of persistent agents on daemon startup
- [x] **Isolated agent worktrees**: Persistent agents (supervisor, merge-queue, pr-shepherd) each get their own git worktree, preventing prompt overwrites from shared `AGENTS.md`
- [x] **Worker dormancy / PR monitoring**: Workers enter zero-token-burn dormant state after creating a PR; daemon monitors for CI failures, merge conflicts, comments, merges, and closures via `gh pr view`, then wakes the worker with a targeted message
- [x] **Workspace stuck detection**: Two-tier escalation for workspace agents stuck thinking — soft interrupt + nudge at 15 min, hard restart at 30 min. Off by default (per-repo opt-in via `oat config --workspace-stuck-detection=true`); benchmarks enable it automatically
- [x] **Daemon message tagging**: All daemon-initiated messages prefixed with `[daemon]` so agents can distinguish automated nudges from human input
- [x] **Message delivery reliability**: Backend message delivery uses atomic operations, preventing race conditions when multiple agents receive messages concurrently
- [x] **Graceful fetch failure handling**: Daemon tracks per-repo consecutive fetch failures and skips repos after 3 failures (e.g., deleted GitHub repos) to avoid log spam

### P1 - Should Have
- [x] **Worktree sync**: Keep agent worktrees in sync with main as PRs merge
- [x] **Clear error messages**: Every failure tells the user what went wrong and how to fix it
- [x] **Task history**: Track what workers have done and their outcomes
- [ ] **Coding-agent enforcement**: Workers must use coding-specific agents (e.g., Claude Code, Codex, Qwen Coder) rather than generic LLMs, which fail to complete final steps like committing and PR creation

### P2 - Nice to Have
- [x] **Better onboarding**: Guided first-run experience via `oat repo init`
- [ ] **Agent metrics**: Simple stats on agent activity (tasks completed, PRs created)

## Phase 1.5: Multi-Model Benchmarking

Focus: Build a standardized framework for comparing coding LLM effectiveness in a multi-agent orchestration setting.

- [x] **Benchmark suite**: Use the robotic-barista project (bundled in `benchmarks/robotic-barista/`) with preset tickets as a reproducible starting point (`benchmarks/setup.sh`, `benchmarks/run.sh`)
- [x] **Comparison framework**: Script to clone repo, set up foundational tickets, and run OAT with different coding LLMs from the same starting state (automated wave progression, configurable `--model` / `--worker-model`)
- [x] **Telemetry**: Track agent effectiveness (task completion rate, PR quality, convergence time) across different models (`benchmarks/collect.sh` with worker autonomy metrics, per-wave timing)
- [x] **Acceptance testing**: Blackbox functional smoke test with weighted 100-point scoring system (`benchmarks/acceptance-test.sh`)
- [x] **Blackbox test gate**: Model generates a blackbox acceptance test from spec alone; LLM judge scores it against the reference test; gate determines if the model can understand the spec well enough to proceed (`benchmarks/blackbox-gate.sh`, `benchmarks/judge-blackbox.sh`)
- [ ] **Open-source model support**: Test with SaaS-hosted open-source coding models (Ollama-compatible, Qwen, Gemini, etc.) in addition to proprietary ones
- [ ] **Research output**: Results to form a white paper comparing coding LLM effectiveness across agents

## Future

Items planned after the core port and benchmarking are stable. Grouped by theme, not ordered by priority.

### Infrastructure & Architecture

- [x] **Remove Deep Agents branding** - Integrated agent runtime internally with OAT branding
- [x] **Remove tmux dependency** - Agents run as direct PTY child processes; no terminal multiplexer required
- [ ] **Memory MCP Server** - Integrate a Reflection/Memory MCP server for structured context storage, episodic memory across restarts
- [ ] **Model selection & configuration** - Per-agent model selection, open-source model support via SaaS providers, cost-aware scheduling
- [ ] **PR/branching strategy** - Dev branch workflow: OAT worker branches merge to dev branch (smoke test), then dev merges to main

### Agent Intelligence

- [ ] **Skills-based architecture** - Move from hard-coded roles to skills-oriented system; agents self-organize around tasks
- [ ] **"Plays" system** - YAML-defined collections of roles with skills that coordinate for domain-specific work (legal review, DevSecOps, full-stack teams); analogous to sports plays
- [ ] **Agent mesh communication** - Message bus for any-to-any agent communication, enabling plays to dictate worker-to-worker coordination
- [ ] **Vassal agents** - Expensive agents delegate routine/chatty tasks to cheaper agents running on free/low-cost hardware
- [ ] **Codebase context for multi-agent coordination** - Tree-sitter AST maps, code snippet RAG, conflict detection when agents modify related areas
- [ ] **Application expert / product manager role** - Agent persona that deeply understands the spec and guides work toward it

### Convergence & Quality

- [ ] **Debug/QA agent** - Specialized agent that reviews failed CI, analyzes test output, and proposes fixes
- [ ] **OAT debug mode** - Verbose logging mode for diagnosing agent behavior issues; triggers automatically or via `oat debug`
- [ ] **Retro agent / post-mortem mode** - Analyzes completed runs, reviews what happened, produces actionable improvement suggestions
- [ ] **Fix workspace agent bypassing CI** - Workers/workspace agent must never push directly to main without PRs

### User Experience

- [ ] **Real-time worker monitoring** - `oat attach worker` CLI command to tail a worker's reasoning stream
- [ ] **Slack/Telegram integration** - Human-in-the-loop status updates; workers can ask humans questions when stuck
- [ ] **Co-work / desktop automation** - Playbooks for agents to control desktop applications

### Open Source & Community

- [ ] **Open source readiness** - contributor.md, labeling policy, OAT-driven contributor model for autonomous open-source contributions
- [ ] **Open-source model testing** - Test with N open-source models for credibility; demonstrate OAT works across many LLMs

## Out of Scope (Do Not Implement)

1. **Remote/hybrid deployment** - No cloud coordination or distributed orchestration
2. **Web interfaces or dashboards** - Terminal is the interface
3. **Plugin/extension systems** - Keep the codebase simple
4. **Enterprise features** (SSO, audit logs, role-based access)

## Changelog

- **2026-03-19**: Added blackbox test gate to Phase 1.5 (model generates acceptance test from spec, LLM judge gates benchmark progression)
- **2026-03-03**: Checked off completed items: isolated agent worktrees, worker dormancy/PR monitoring, workspace stuck detection, daemon message tagging, message delivery reliability, fetch failure handling; checked off Phase 1.5 benchmark suite, comparison framework, telemetry, and acceptance testing
- **2026-03-02**: Restructured roadmap: replaced Phase 2 with flat Future section grouped by theme; added items from Feb 27 and March 2 daily syncs (Remove Deep Agents branding, remove tmux dependency, Plays system, agent mesh communication, vassal agents, Slack integration, debug/QA agent, retro agent, PR branching strategy, worker monitoring, open source readiness); moved Slack from Out of Scope to Future
- **2026-02-25**: Updated roadmap from Feb 24 daily sync (checked off completed Phase 1 items, added coding-agent enforcement, benchmarking phase, model support expansion, codebase context section)
- **2026-02-19**: Updated roadmap for OAT v1 (OAT Agent Runtime port) and v2 vision
- **2026-01-20**: Initial roadmap after Phase 1 cleanup
