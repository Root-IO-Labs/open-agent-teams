# DAG Code Index

**Status:** Draft
**Owner:** oat-agent
**Branch:** `feature/dag-code-index`
**Last updated:** 2026-05-18

## Context

OAT works well on greenfield repos but agents waste tokens and tool calls re-exploring the same brownfield codebases on every session. When an agent needs to make a change, it has no inherent sense of the blast radius — "if I change function `X`, which other files reference it, and which tests should I expect to break?" Today the planner and workspace agents discover this from scratch each run, which is slow and inconsistent.

Two of the May 14 commitments converge on this gap:
- "Map brownfield with Rahul using Probe MCP once it's standing"
- AST research for code-mapping context (Tree-sitter)

Probe MCP + AST is the *parsing* substrate. This feature is the *graph* on top of it: a persistent, queryable index that agents consult before they explore.

## Goal

Build a code-base dependency graph that:

1. **Indexes a repo's structure** as a directed acyclic graph where nodes are symbols (functions, types, modules, files) and edges are reference/import/call relationships.
2. **Lets agents query impact** — "what depends on `X`?" and "what does `X` depend on?" — without re-grepping the whole tree.
3. **Persists per-repo** so the cost of indexing is paid once, then incrementally updated on file changes.
4. **Exposes a stable interface** the planner, workspace, and router-complexity scoring can all consume.

The brownfield-repo case is the primary target: a repo that already has substantial code when OAT is initialized into it.

## Non-goals

- Not a semantic / vector search index — that's a different layer and may be added later.
- Not a replacement for the planner's wave execution (those are tasks, not code).
- Not a runtime call-graph (no profiling, no dynamic dispatch resolution).
- Not language-universal at v1 — see Approach.

## Approach (sketch — to refine before implementation)

### Phase 1: minimal viable index
- **Language scope:** start with Go (the OAT codebase itself is the first dog-food target). Add Python next (covers `agent-runtime/` and `benchmarks/`).
- **Parser:** use Tree-sitter via the Probe MCP work as the parsing layer. If Probe MCP isn't ready, fall back to language-native AST parsers (`go/ast`, `ast` module) for the first cut.
- **Index store:** SQLite in `~/.oat/index/<repo-hash>.db`. Schema: `nodes(id, kind, name, file, line)`, `edges(src, dst, kind)` where `kind ∈ {imports, calls, references, declares}`.
- **Incremental update:** on file change, re-parse only the changed file and replace its nodes/edges. Triggered by a daemon hook or an explicit `oat index refresh` command.
- **Cycle handling:** the *file-level* graph is rarely acyclic in real code (mutual imports exist in Python; cycles exist via interface satisfaction in Go). We will store the full graph but expose acyclic *subgraphs* for traversal queries (topological dependents/dependencies).

### Phase 2: agent surface
- New internal package `internal/index/` exposing:
  - `Build(repoRoot string) (*Index, error)`
  - `Dependents(symbol string) []Node`
  - `Dependencies(symbol string) []Node`
  - `ImpactSet(symbol string, depth int) []Node`
- New MCP-style tool the planner and workspace agents can call: `code_index.impact(symbol)`, `code_index.find(query)`.
- Router-complexity gets a new signal: "how connected is the file the user is asking about" → routes high-fanout edits to bigger models.

### Phase 3: brownfield onboarding
- `oat init` on a non-empty repo runs an initial indexing pass and reports stats: "indexed 2,341 symbols, 8,902 edges across 312 files."
- The planner consults the index during decomposition so its task list references real symbols, not hallucinated ones.

## Open questions

1. **Probe MCP timing** — does this branch wait for Probe MCP to stand up, or do we ship a minimal `go/ast`-based v0 and swap the parser later? Recommendation: ship v0 in parallel, swap parser when Probe MCP is ready. The graph schema doesn't care about the parser.
2. **Index storage location** — `~/.oat/index/` vs in-repo `.oat/index/`. Per-user is friendlier for shared repos; in-repo lets the index be committed/cached in CI. Lean toward per-user for v1.
3. **Update trigger** — explicit command, file-watcher daemon, or both?
4. **Query language** — start with hard-coded methods (`Dependents`, `Dependencies`, `ImpactSet`) or design a small query DSL up front?
5. **Cross-language edges** — Python calling a CLI invocation of a Go binary: do we model that, or out of scope?

## Validation plan

- Unit tests on a synthetic small Go package: build graph, query dependents, assert correctness.
- Dog-food on the OAT repo itself: index `internal/`, query "what depends on `internal/state.State`", compare against `grep -r` ground truth.
- Brownfield integration test with Rahul: pick a target repo (TBD), run `oat init`, run a planner request that should benefit from the index (e.g., "rename function X"), compare token usage and accuracy against a baseline run with the index disabled.
- Performance: indexing a ~10kLOC Go repo should complete in under 30s on a laptop.

## Out of scope for this branch

- Langfuse telemetry on indexing operations (handled by `feature/langfuse-telemetry`; integration point is a single `index.Observed(...)` call).
- UI/TUI surfacing of the index (a debug command is fine for v1; rich UI is a follow-up).
- Cross-repo indexing (monorepo support beyond a single repo root).
