# Routing Test Repo — `budget-cli`

Purpose-built fixture for evaluating OAT routing quality.

**What it is:** a minimal Python CLI for tracking personal expenses. Small, self-contained, runs on pytest. Structurally similar to `coffee-cli` but deliberately independent so both can coexist in OAT's test pool without confusion.

**Why it exists:** we need a repo whose tasks have (a) known, auto-verifiable success criteria and (b) graded complexity so routing decisions can be measured. Real repos don't give us that — bug tasks are written by humans who didn't label difficulty, and success is measured by "did the PR merge," which is slow and noisy.

## Layout

```
benchmarks/routing-test-repo/
├── README.md              ← you are here
├── task-manifest.yaml     ← graded tasks + verify scripts
├── seed/                  ← files copied into the fresh test repo
│   ├── pyproject.toml
│   ├── README.md
│   ├── AGENTS.md
│   ├── src/budget_cli/
│   │   ├── __init__.py
│   │   ├── cli.py         ← entry point; intentionally contains bugs
│   │   ├── storage.py     ← JSON persistence
│   │   └── models.py      ← typed dataclasses
│   └── tests/
│       └── test_cli.py    ← some tests xfail-marked, passing them = success
└── harness/
    └── run_baseline.py    ← orchestrator (reads manifest, runs tasks, records outcomes)
```

## How to run a baseline

```bash
# 1. Bootstrap: create a fresh copy of seed/ in a tempdir and oat-init it
python3 benchmarks/routing-test-repo/harness/run_baseline.py --bootstrap

# 2. Run the task suite against whichever router is currently live
python3 benchmarks/routing-test-repo/harness/run_baseline.py \
    --repo oat-routing-bench-<timestamp> \
    --out ./results/baseline-$(date +%Y%m%d).jsonl

# 3. Analyze
python3 benchmarks/routing-test-repo/harness/summarize.py \
    results/baseline-*.jsonl
```

## What the baseline measures

Per (task, model) cell:

- `success` — did the verify script pass?
- `wall_ms` — wall-clock from spawn → completion
- `tokens_in/out/cache` — cost proxy
- `$` — computed from `internal/routing/pricing.yaml`
- `escalations` — was a stronger model needed after initial failure?

Aggregated:

- `$ / success` per task-complexity tier
- Success rate per (model, complexity) cell
- Model-mix distribution vs what routing decided
- Override rate (0 in baseline; becomes meaningful when comparing router variants)

## Design constraints

- **Tasks must be independent.** The harness runs them sequentially but doesn't coordinate state cleanup between them. Each task either (a) operates on files no other task touches, or (b) accepts that later tasks inherit earlier changes. The manifest flags this with `depends_on: [...]`.
- **Verify scripts must be deterministic.** No time-of-day, no network, no randomness. `pytest` with pinned seed, file-presence checks, grep assertions.
- **Task text is self-contained.** A worker sees the task text and the repo state — nothing else. If a task references "the spec document," the document must be in the repo.

## What this fixture does NOT cover

- **Long-context tasks.** The repo is ~200 files of code total. Real "read this 600K-token monorepo" scenarios need a bigger fixture. Deferred.
- **External-API tasks.** We avoid network-dependent tasks because flaky upstreams would pollute the signal.
- **Inter-task coordination.** No "multi-worker collaboration" tests. Each routing decision is made on a single task in isolation.
