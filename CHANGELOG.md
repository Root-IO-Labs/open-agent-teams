# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `oat agent add <type> [name] [--repo <repo>]` CLI verb for opt-in
  persistent agents. Only `browser-agent` is supported today; the
  dispatcher is structured so additional opt-in types can land
  without restructuring. The browser-agent flow runs a preflight
  bridge probe (`OAT_BROWSER_AGENT_BRIDGE_PATH` env > `PATH` >
  `~/.oat/oat-browser-agent/dist/bridge/index.js`) and bails with
  an actionable message listing every install option if no bridge
  is found. Idempotency: re-adding a healthy browser-agent is a
  hard fail (suggesting `oat agent restart` or `remove`); a stopped
  record is silently respawned. Worktree at
  `~/.oat/wts/<repo>/<agent>/` is created on first add and reused
  on respawn. End-to-end: `bridge preflight -> list_agents idempotency
  check -> worktree create -> add_agent -> start_repo_agents`.
- `AgentConfig.MCPConfig` (`pkg/backend`): when non-empty, the
  direct backend writes the string to `<WorkDir>/.oat/mcp.json`
  before launching the agent process, alongside the existing
  `AGENTS.md` write. The daemon populates this for `AgentType ==
  browser` in both initial spawn (`startRegisteredAgent`) and
  manual restart (`restartAgent`) paths, so the bridge command and
  audit-log dir are always fresh. `.oat/.gitignore` now also lists
  `mcp.json` so worktrees never accidentally commit the bridge
  configuration.
- `internal/agents/browser_bridge.go::ResolveBrowserBridge` --
  shared bridge resolver used by both the CLI preflight and the
  daemon's MCP config builder, so they agree byte-for-byte on
  what command will be spawned. Returns a `BridgeCommand` with
  `Command`, `Args`, and a human-readable `Source` describing
  where it was found.

### Fixed

- Daemon recovery now restores the opt-in browser-agent after a
  backend-session loss. `restoreRepoAgents` previously rebuilt only
  the always-on agent set (supervisor, merge-queue or pr-shepherd,
  workspace) and left a `~/.oat/wts/<repo>/browser-agent/` worktree
  orphaned in state. The fix: when the worktree path exists,
  `restoreRepoAgents` now calls `startAgent(AgentTypeBrowser, ...)`
  so the agent is respawned. The worktree path acts as the "user
  opted in" persistence marker; no extra state-file field is
  needed. Without this, `oat agent add browser-agent` followed by
  any daemon crash + restart would silently drop the agent.
- `startAgentWithConfig` (used by `restoreRepoAgents` and other
  generic spawn paths) now also writes `.oat/mcp.json` for
  `AgentTypeBrowser` -- previously only `startRegisteredAgent`
  (the `start_repo_agents` path) did this, so a recovery-restored
  browser-agent would launch with no MCP tools. Both spawn paths
  now go through `buildBrowserAgentMCPConfig`.

### Changed

- Docs canonicalised on the browser-agent audit log path:
  `~/.oat/output/<repo>/browser-agent-actions.jsonl`. The daemon
  already passes `OAT_BROWSER_AGENT_AUDIT_LOG_DIR=<that dir>` in the
  MCP server's env block (Part 2), so this is the path actually
  written when the agent runs under OAT. Older docs in
  `docs/DIRECTORY_STRUCTURE.md` and `docs/COMMANDS.md` claimed
  `<repo-root>/.oat-logs/...`, which was never accurate under OAT
  and has been corrected. Root `AGENTS.md` also updated to call out
  that the browser-agent is opt-in via `oat agent add browser-agent`
  rather than auto-started with the repo.

- `handleStartRepoAgents` now skips agents whose PID is still alive,
  making the verb idempotent. Required to safely re-call after
  `oat agent add` registers a single new agent on an
  already-running repo (without the skip, every existing supervisor
  / merge-queue / worker would be double-spawned).
- `list_agents` socket response includes the agent's `pid` field
  alongside the existing name / type / worktree_path / window_name /
  task / summary / model / created_at fields. Used by `oat agent
  add`'s liveness check; backwards-compatible (new key, no shape
  change).

- MCP (Model Context Protocol) client support in agent-runtime.
  `oat_sdk.mcp_client` loads MCP servers declared in
  `<cwd>/.oat/mcp.json` at agent startup and exposes their tools as
  LangChain `BaseTool` instances merged into the existing
  `create_cli_agent(tools=...)` call. Stdio transport only for now;
  the file shape is `{servers: [{name, command, args, env,
  transport: "stdio"}]}`. The daemon writes this file at agent
  spawn time when `MCPConfig` is non-empty (analogous to how it
  already writes `AGENTS.md`); when no MCP is configured the file
  is absent and the agent runs unchanged. Both the interactive
  (`oat_cli.main`) and daemon-spawn / non-interactive
  (`oat_cli.non_interactive`) paths are wired. SIGTERM from the
  daemon cancels the running task; the resulting `CancelledError`
  propagates through a `try/finally` that calls
  `AsyncExitStack.aclose()` so each MCP server's stdio child is
  reaped, not orphaned. Concerns the adapter owns directly:
  per-session `asyncio.Lock` to serialise parallel tool calls on
  one stdio stream, sidecar `KIND_TOOL_CALL`/`KIND_TOOL_RESULT`
  event emission on both success and error paths, canonicalisation
  of MCP `TextContent`/`ImageContent`/`EmbeddedResource` blocks to
  LangChain-friendly shapes, tool-name collision resolution
  (built-in tools win; colliding MCP tools are exposed as
  `<server>__<tool>`), and graceful degradation on malformed
  `mcp.json` (warning + zero MCP tools; never a crash).

### Changed

- Browser-agent system prompt (`internal/templates/agent-templates/browser.md`)
  gains a "Perception cost hierarchy" section that teaches the cheapest-tool-
  that-gets-the-job-done order for read-only, interaction, and state-change
  tasks. Read-only "what's on this page?" tasks now default to
  `browser_get_text {mode: "main", maxChars: 4000}` (Mozilla Readability
  extraction; ~80% token reduction on Wikipedia-class long-form pages vs.
  the full-page walk that Part 4.5 confirmed real LLMs reach for first),
  with `browser_snapshot {interactiveOnly: true}` as the interaction
  primary and a fallback ladder for `NO_MAIN_CONTENT` cases. The Strategy
  section's `browser_get_text` / `browser_snapshot` entries also gain the
  `mode: "main"` and `interactiveOnly: true` hints. Land in lockstep with
  oat-browser-agent's Part 7.5c (`mode: "main"` Readability path + the
  Part 7.5d post-completion / `tabId` enrichment). End users running the
  browser-agent against article-style pages will see ~5x lower perception
  token cost on the first read.

- Browser-agent system prompt (`internal/templates/agent-templates/browser.md`)
  gains a "Deliberate action" section. Production browser-agents are now
  guided to act like a careful operator: one destructive action at a time per
  domain, re-snapshot before clicking visually close controls, confirm
  intermediate state before the next destructive call, prefer to stop and
  explain on password fields / sensitive pages / unfamiliar UI patterns, and
  use slower deliberate motion on logged-in or session-bearing pages. End
  users will see fewer simultaneous clicks and a more measured pace on
  multi-step flows. The change ships in lockstep with the oat-browser-agent
  model bench so the same prompt drives both production and benchmark
  scoring.

### Added

- `benchmarks/llm_call.py` — provider-agnostic LangChain wrapper used by the
  benchmark scripts. Resolves any `provider:model` string OAT supports
  (anthropic, openai, google_genai, openrouter, deepseek, ollama, ...),
  surfaces a clear `missing FOO_API_KEY` error per provider, and emits
  normalized `{text, input_tokens, output_tokens, model, provider}` JSON
  on stdout (logs go to stderr).
- `benchmarks/test_llm_call.py` — fully-mocked unit tests for `llm_call.py`
  covering bare-vs-`provider:model` parsing, missing-API-key paths,
  stdout/stderr discipline, token-usage normalization across provider
  response shapes, and exit-code mapping for resolution / API failures.
- `benchmarks/run.sh --summary-model <provider:model>` — symmetric with
  `--judge-model`, controls the post-run summary model. Defaults to the
  orchestrator `--model` so multi-provider runs don't incur surprise
  charges from a different provider's key sitting in the environment.
- `benchmarks/run-comparison.sh --judge-model` and `--summary-model` —
  forwarded to each leg's inner `run.sh` invocation; each leg defaults
  to its own orchestrator model when the override is omitted.
- `benchmark-helpers-tests` job in `.github/workflows/ci.yml` — runs
  `pytest` over `benchmarks/test_probe_model.py` (already present, was
  not gated by CI before) and the new `benchmarks/test_llm_call.py`.
  Both files mock LangChain entirely so the job needs no provider keys.
- Provenance HTML comment in `summary.md`
  (`<!-- Generated by <provider:model> at <RFC3339> -->`) and a
  `model: <resolved provider:model>` field in `gate.json`, so future
  readers can tell exactly which judge / summarizer produced each run's
  outputs (a bare `claude-sonnet-4-6` is normalized to
  `anthropic:claude-sonnet-4-6`).
- OSS meta files: `CHANGELOG.md`, `MAINTAINERS.md`, `AUTHORS`, `.github/FUNDING.yml`,
  `.github/ISSUE_TEMPLATE/*`, `.github/PULL_REQUEST_TEMPLATE.md`.
- `.github/dependabot.yml` for automated Go, Python, and GitHub Actions dependency updates.
- `.github/workflows/codeql.yml` for weekly CodeQL security analysis (Go + Python).
- `.github/workflows/auto-uv-lock.yml` to refresh `uv.lock` on Dependabot Python PRs.
- `.golangci.yml` with an aggressive linter ruleset (gofmt, govet, errcheck,
  staticcheck, ineffassign, unused, unconvert, goimports, misspell) wired into CI.
- `internal/version` package with `Version`, `Commit`, `Date` injected via
  `ldflags -X` at build time; `oat version` now reports all three.
- `.github/workflows/release.yml` + `.goreleaser.yml` for tag-triggered binary
  releases (linux/darwin × amd64/arm64) with GitHub Releases artifacts and
  Homebrew tap auto-update.
- `oat model set <provider:model> [--nudge-interval SECONDS] [--max-tokens N]`
  CLI subcommand for tuning per-model runtime parameters.
- `oat tokens report --repo <name> [--since <ts>] [--until <ts>] [--format json|table] [--wave N]`
  CLI subcommand for historical per-wave token-usage analysis from agent logs
  (distinct from `oat status --tokens` which reads live daemon state).
- `runtime.max_tokens` and `runtime.nudge_interval_seconds` fields in model
  profile YAMLs; daemon falls back to existing defaults when unset.
- `benchmarks/summarize.sh` per-wave token-usage table via `oat tokens report`.

### Changed

- `LICENSE` copyright year updated from 2025 to 2026 (development began Jan/Feb 2026).
- Final-nudge message templates in the daemon (`finalNudgeSupervisor`,
  `finalNudgeMergeQueue`, `finalNudgePRShepherd`) compacted by ~55-65% with
  no actionable information lost; merge-queue template preserves the
  `sleep 30` polling instruction verbatim.
- Benchmark script layout: internal helpers (`run-blackbox.sh`,
  `judge-blackbox.sh`, `whitebox-shim.py`) moved to `benchmarks/scripts/`.
  User-facing entry points (`run.sh`, `setup.sh`, `acceptance-test.sh`,
  `summarize.sh`, `collect.sh`, `cleanup.sh`, comparison commands) remain at
  the top level. All internal callers (`benchmarks/run.sh`,
  `benchmarks/judge-cursor-gate.sh`, `benchmarks/README.md`) updated to the
  new paths.
- `benchmarks/summarize.sh` and `benchmarks/scripts/judge-blackbox.sh`
  are now provider-agnostic: both call the new `llm_call.py` helper
  instead of curling `https://api.anthropic.com/v1/messages` directly.
  Model resolution order for both scripts: explicit flag
  (`--model` / `--judge-model`) -> `OAT_BENCH_LLM_MODEL` env var ->
  orchestrator model from `collect.json` (summarize only) ->
  `anthropic:claude-sonnet-4-6` hard fallback. A missing API key for
  the resolved provider now produces a clear `missing FOO_API_KEY`
  error from `llm_call.py` instead of a 401 from Anthropic.
- `benchmarks/run-comparison.sh` no longer hard-fails at startup when
  `ANTHROPIC_API_KEY` is unset. Provider keys are checked per-run by
  the inner scripts, so cross-provider comparisons (e.g. Sonnet vs
  GPT-5) work without requiring keys for both providers up front.
- Go and Python dependencies refreshed to current minor/patch versions.
- GitHub Actions pinned to latest stable versions across all workflows.

### Documented

- `oat status --tokens` CLI command and prompt-caching feature in
  [`docs/COMMANDS.md`](docs/COMMANDS.md), [`docs/ADVANCED_USAGE.md`](docs/ADVANCED_USAGE.md),
  and [`README.md`](README.md). The feature itself shipped earlier but was
  previously undocumented.

### Removed

- Dead code surfaced by `unused` linter: `getOSInfo`, `writeMergeQueuePromptFile`,
  `writePRShepherdPromptFile`, `quoteForShell`, `stdLogger` and its methods,
  `worktreeRefreshLoop` empty shell, `App.err` and `pollResultMsg.repoName`
  fields, `internal/cli/verify_simple.go` (abandoned duplicate-block detector,
  superseded by `verify.go`).

### Fixed

- **Wave 0 timing in `collect.json` is no longer derived from a fuzzy
  GitHub PR search.** `benchmarks/run.sh` now records `wave 0`
  `started_epoch` / `completed_epoch` to `wave-timing.json` alongside
  waves 1–4, and `benchmarks/collect.sh` reads that data via
  `wave_timing_from_file "0"` instead of falling back to
  `gh pr list --search "closes #N OR fixes #N"`. The GitHub search was
  too fuzzy and matched PRs whose body referenced unrelated issues whose
  numbers contained `N` as a digit substring (e.g. issue #1 spuriously
  matching a PR body that mentioned #17), inflating wave 0's reported
  duration on a real run from ~24 min to ~119 min. Older result
  directories without a `"0"` key in `wave-timing.json` continue to fall
  back to the PR-derived path, so historical analysis is unchanged.
- **`benchmarks/llm_call.py` now imports `create_model` from the canonical
  `oat_cli.config` path** used by the rest of the benchmark tooling
  (e.g. `benchmarks/probe-model.py`), fixing a runtime
  `ModuleNotFoundError` warning that masked custom-provider support from
  `~/.oat/config.toml`. The langchain `init_chat_model` fallback was hiding
  the breakage but routing all bench LLM calls through the non-config path.
  `.gitignore` cleaned up: removed a stale entry that was redundant with the
  existing `.oat/*` ignore rule covering the project-local cache directory.
- **`benchmarks/setup.sh` no longer silently swallows `gh label create`
  failures.** GitHub's secondary rate limit can throttle the burst of 28
  label creates against a fresh repo, leaving a subset of labels uncreated.
  The script previously redirected those errors to `/dev/null` and
  continued, so the failure surfaced ~30s later as a confusing
  `gh issue create` rejection mid-loop (and `set -euo pipefail` then killed
  the run, tripping `run.sh`'s cleanup trap). Now: each `gh label create`
  and `gh issue create` is wrapped in a bounded retry-with-exponential-
  backoff helper (`gh_with_retry`), the bursts are paced
  (200ms / 300ms between calls), and any final failure exits with a clear
  diagnostic listing the offending labels/issue and pointing at
  `gh label list --repo <repo>` for inspection.
- Duplicate `.github/workflows/main.yml` removed (byte-equivalent to the
  `check-source` job in `ci.yml`).
- **Verifier no longer rejects work on a stale-base race.** The daemon now
  snapshots the remote default-branch SHA at `oat worker request-review` and
  pins it on the worker as `BaseSHA`. Verifier prompts and self-verify both
  diff against `${BASE_SHA}..HEAD` instead of live `origin/main`, so commits
  that landed on `main` between the worker's rebase and the verifier's review
  no longer appear as "deletions" and incorrectly fail the diff. Falls back
  to live `origin/main` when `BaseSHA` is empty (in-flight verifications
  during upgrade).
- **Daemon false "verifier crashed" message.** `cleanupDeadAgents` now
  guards the crash wake-message with `!agent.ReadyForCleanup`, so a verifier
  that successfully delivered a verdict but had its worker status concurrently
  reset by another `request-review` no longer prints a bogus "your verifier
  crashed" message in the worker log.
- **`benchmarks/collect.sh` worker-name collection on macOS.** Replaced
  `declare -A WORKER_NAMES` (bash-4-only) with the same jq + `sort -u`
  pattern already used in `summarize.sh`. macOS's default bash 3.2 was
  silently failing the script and producing no `collect.json`.
- **`benchmarks/run.sh` bash-3.2 portability.** Several latent
  `set -euo pipefail` × bash-3.2 bugs that crashed real runs on macOS
  default bash:
  - `PRE_COUNT` / `PROFILE_COUNT` were computed with
    `grep -c <pattern> || echo "0"`. `grep -c` always emits the count to
    stdout AND exits 1 on zero matches, so the fallback was *appending* a
    second `0`, producing values like `"0\n0"` and a downstream
    `[[: 0 0: syntax error in expression`. Switched to `|| true` plus an
    empty-string guard.
  - `assemble_gate_test()` expanded `"${module_files[@]}"` and
    `"${sorted_modules[@]}"` without length guards. Bash 3.2 + `set -u`
    treats an empty array as unset; this crashed with
    `unbound variable` on the new sanity-check fixture (zero
    `test-*.sh` modules). All `[@]` expansions in that function now
    sit behind `${#arr[@]} -gt 0` guards, and the `IFS=$'\n' arr=($(sort
    <<< ...))` one-liner was replaced with a portable `while IFS= read`
    loop that skips the printf entirely when the source array is empty.
  - The convergence-loop result writer now guards
    `for cr in "${CONVERGENCE_RESULTS[@]}"` so an early grand-timeout
    or convergence-timeout (which can break out before the first
    iteration appends a result) doesn't crash the JSON emitter.
  - Removed an orphaned `"${SMOKE_REASONS[@]}"` reference left behind
    by a partial revert (added in `9bcc051`, mostly removed in
    `8a2f71d`'s execution-based smoke runner, but the read-side line
    snuck back in `f87f6d6`). The `RAW_SNIPPET` immediately below
    already provides the same diagnostic info from the actual runner
    output.
- **Benchmarks: harden `run.sh` and `collect.sh` against degraded GitHub
  issue-list/search index.** Wave 0 gate discovery, per-wave kickoff totals,
  `wait_for_wave`'s completion arithmetic, and post-run analysis collection
  now treat [`benchmarks/issues.json`](benchmarks/issues.json) as the source
  of truth and fall back to it (with a loud `WARNING:`, or `ERROR:` for
  issues that genuinely 404 per per-issue probe) when `gh issue list`
  returns fewer issues than expected. Per-issue endpoints are unaffected by
  ElasticSearch degradation and are used both as the satisfaction check and
  as the fallback fetch path. Prevents under-spawning workers, false-positive
  wave completion, and silently-wrong analysis numbers during GitHub Issues
  indexing degradation (e.g. the Apr 27 2026 incident, where
  `gh issue list --label wave:0` returned only `#4` for ~14 minutes despite
  `#1`/`#2`/`#3` being live and reachable). Polling timeout/interval are
  tunable via `OAT_INDEX_POLL_TIMEOUT` (default 120s) and
  `OAT_INDEX_POLL_INTERVAL` (default 10s); on the healthy path the new
  helpers add ~0.5s per benchmark. Shared logic lives in the new
  [`benchmarks/lib.sh`](benchmarks/lib.sh).

### Added

- **Pre-flight Python import check (verifier Step 5b).** Verifier prompt
  now instructs Python projects to run `python -m pytest --collect-only
  <test-file>` before writing black-box tests so hallucinated import paths
  fail in seconds instead of waiting for full collection.
- **Cost reporting in `oat tokens report` and `oat status --tokens`.** New
  `COST_USD` column derived from the embedded `internal/routing/pricing.yaml`
  (now with explicit `cache_creation_per_mtok` for Anthropic models and a
  per-provider fallback helper for everything else). `--format json` exposes
  `cost_usd` per agent and on the totals block. `benchmarks/summarize.sh`
  appends a "Total cost (priced agents only)" line to the markdown summary.

## [0.1.0] - TBD

Initial public release.

[Unreleased]: https://github.com/Root-IO-Labs/open-agent-teams/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/Root-IO-Labs/open-agent-teams/releases/tag/v0.1.0
