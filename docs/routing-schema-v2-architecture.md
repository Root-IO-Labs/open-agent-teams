# Routing Schema v2 — Architecture

How the loop works end-to-end. Every diagram below renders in GitHub's
Markdown viewer; no extra tooling needed.

---

## 1. The full data flow

```mermaid
flowchart TB
    USER[User runs<br/>'oat run' or task spawn] --> SOCK[CLI → daemon socket]
    SOCK --> ROUTE{Router<br/>V0 / V1 / V2<br/>policy gated by env}

    ROUTE --> SPAWN[startAgentWithConfig<br/>creates state.Agent<br/>with v2 fields]
    SPAWN --> WORKER[Worker process<br/>actually runs the task]
    WORKER --> LIFE{Worker<br/>completion?}

    LIFE -->|completed| LOG_OK[handleCompleteAgent<br/>→ logOutcome 'completed']
    LIFE -->|failed/killed| LOG_FAIL[handleRemoveAgent<br/>→ logOutcome 'removed' +<br/>removal_reason]
    LIFE -->|verifier verdict| LOG_OK

    LOG_OK --> MAIN[(routing-history.jsonl<br/>MAIN file<br/>append-only<br/>immutable)]
    LOG_FAIL --> MAIN

    MAIN -.->|every 5 min| BACKFILL{PRBackfiller<br/>ticker}
    BACKFILL -->|gh pr view| GH[(GitHub API<br/>PR state +<br/>statusCheckRollup)]
    GH --> SIDE[(routing-history.backfill.jsonl<br/>SIDECAR<br/>append-only<br/>1h/24h/7d snapshots)]

    MAIN -.->|every 10 min| JOIN
    SIDE -.->|every 10 min| JOIN
    JOIN[LoadCorpusJoined<br/>+ BuildCorpusIndex<br/>per model_canonical × complexity bucket<br/>Wilson 95% lower bound] --> IDX[(CorpusIndex<br/>in-memory, RWLocked<br/>cached on Daemon)]

    IDX -.->|"V2 only<br/>passed by daemon"| ROUTE

    classDef store fill:#e3f2fd,stroke:#0d47a1,stroke-width:2px
    classDef compute fill:#fff3e0,stroke:#e65100,stroke-width:2px
    classDef decision fill:#f3e5f5,stroke:#4a148c,stroke-width:2px
    classDef external fill:#fce4ec,stroke:#880e4f,stroke-width:2px

    class MAIN,SIDE,IDX store
    class ROUTE,LIFE,BACKFILL decision
    class JOIN,LOG_OK,LOG_FAIL,SPAWN compute
    class GH,USER external
```

The dashed arrows are async — they don't block the synchronous worker
spawn / completion path. The synchronous path (solid arrows) goes from
user action straight to the worker, never blocked by I/O on the corpus
files.

---

## 2. The cardinal rule (router does not depend on logger)

```mermaid
flowchart LR
    subgraph SYNC[Synchronous spawn path · MUST stay fast & deterministic]
        IN[Router inputs:<br/>task text<br/>profiles<br/>pricing<br/>corpus snapshot] --> RT{Pure function<br/>RouteForTask*}
        RT --> OUT[RouteDecision]
    end

    subgraph ASYNC[Async observation · NEVER blocks SYNC path]
        OUT -.->|fire-and-forget| LOG[OutcomeLogger.Log]
        LOG -.->|may fail silently| FILE[(JSONL file)]
    end

    classDef sync fill:#c8e6c9,stroke:#1b5e20,stroke-width:3px
    classDef async fill:#ffe0b2,stroke:#bf360c,stroke-width:2px,stroke-dasharray: 5 5
    class IN,RT,OUT sync
    class LOG,FILE async
```

Enforced by three regression tests:

- `TestRouteForTask_DeterministicGivenInputs` — 200 calls with identical
  inputs must return byte-identical outputs.
- `TestRouteForTask_NoLoggerImport` — `route_for_task.go`'s import set must
  be a subset of `{fmt, sort}`. Adding requires explicit review.
- `TestRouteForTask_NoSymbolReferences` — AST scan: the router file must
  not reference `OutcomeLogger`, `OutcomeRecord`, `PRBackfiller`,
  `LoggedTaskFeatures`, etc. by name.

Plus 5 logger-resilience tests prove broken-disk / nil-receiver / concurrent
writes don't propagate errors back to the router.

---

## 3. The three router policies (gated by env vars)

```mermaid
flowchart LR
    TASK[Worker spawn] --> CHECK{Env flags?}
    CHECK -->|"OAT_ROUTER_VERSION=v2"| V2
    CHECK -->|"OAT_ROUTING_V1=1"| V1
    CHECK -->|default| V0

    V0[V0: argmax static score<br/>within allowlist<br/>ignores cost & history] --> PICK
    V1[V1: cheapest meeting<br/>complexity floor<br/>tied → higher static score] --> PICK
    V2[V2: highest effective_score<br/>= static × historical_factor<br/>tied → cheapest] --> PICK
    PICK[Chosen model]

    classDef def fill:#e8eaf6,stroke:#283593
    classDef opt fill:#e0f7fa,stroke:#006064
    class V0 def
    class V1,V2 opt
```

| | V0 (default) | V1 | V2 |
|---|---|---|---|
| **Activation** | always on | `OAT_ROUTING_V1=1` | `OAT_ROUTER_VERSION=v2` |
| **Optimizes for** | capability (static score) | cost | evidence-weighted capability |
| **Reads corpus?** | no | no | yes (snapshot, every 10 min) |
| **Tiebreaker** | tier index | static score → tier | cost → tier |
| **Risk profile** | predictable | aggressive cost | self-correcting via history |

V2's `historical_factor`:

- `1.0` (no adjustment) when `bucket.Scored < MinHistoricalSamples` (=5)
- `max(0.5, wilson_lower_bound_95)` when ≥5 samples

The 0.5 floor stops a bad streak from fully banishing a model. The
threshold of 5 stops noisy small-N data from dominating the static prior.

---

## 4. The bucket key — why model_canonical × complexity

```mermaid
flowchart TB
    REC[OutcomeRecord] --> CANON[ModelCanonical<br/>'claude-3-5-sonnet-20241022'<br/>→ 'claude-sonnet-3-5']
    REC --> EXTRACT[ExtractFeatures<br/>task_text → complexity bucket<br/>trivial / simple / standard / complex]
    CANON --> KEY[(corpusKey<br/>Model + Complexity)]
    EXTRACT --> KEY
    KEY --> STATS[CorpusStats<br/>Total, Scored, Successes,<br/>MeanScore, WilsonLowerBound]
```

**Why canonical model**: a deprecated point release of sonnet shouldn't
lose its history when the user upgrades. `claude-3-5-sonnet-20240620` and
`claude-3-5-sonnet-20241022` both bucket as `claude-sonnet-3-5` so signal
survives rotation.

**Why complexity**: a model that fails refactors might still ace typos.
Scoring per-bucket prevents one failure mode from poisoning the model's
overall reputation. The `ExtractFeatures` heuristic classifier (in
`task_classifier.go`) runs at index-build time so the same task always
hashes to the same bucket.

---

## 5. Privacy & kill switch — orthogonal to routing

```mermaid
flowchart TB
    OFF{OAT_OUTCOME_LOG=off?} -->|yes| NIL[NewOutcomeLogger returns nil<br/>logOutcome is a no-op<br/>routing still works]
    OFF -->|no| MODE{OAT_LOG_PRIVACY?}
    MODE -->|"strict"| STRICT[Redact task_text<br/>+ summary<br/>+ failure_reason<br/>at write time<br/>Hashes preserved]
    MODE -->|"local default"| LOCAL[Full text on disk<br/>Never uploaded]
    MODE -->|"share-features"| SF[Full local<br/>Opt-in upload<br/>features only]
    MODE -->|"share-all"| SA[Full local<br/>Opt-in upload<br/>full text]

    classDef off fill:#ffebee,stroke:#b71c1c
    classDef redact fill:#fff9c4,stroke:#f57f17
    classDef ok fill:#c8e6c9,stroke:#1b5e20
    class NIL off
    class STRICT redact
    class LOCAL,SF,SA ok
```

Critical: privacy operates at write time; redaction is irreversible. A
strict-mode record never had `task_text` and never can. Hashes
(`prompt.user_message_hash`, etc.) are preserved across every mode by
design — they're privacy-safe and downstream analyses still work.

---

## 6. The recovered-worker case (the loop's hardest test)

```mermaid
sequenceDiagram
    participant Worker
    participant Daemon
    participant Main as Main JSONL
    participant Sidecar
    participant Backfill as Backfiller
    participant GH as GitHub
    participant Index as CorpusIndex

    Worker->>Daemon: spawn task X
    Daemon->>Daemon: V2 router picks model M
    Worker->>Worker: runs, creates PR #42, fails internally
    Worker->>Daemon: complete? no — supervisor force-removes
    Daemon->>Main: outcome=removed, removal_reason=failed, pr_number=42

    Note over Main,Sidecar: Several hours pass.<br/>Someone fixes the PR; it merges.

    loop every 5 min
        Backfill->>Main: scan for records with PR
        Backfill->>GH: gh pr view 42 --json state,statusCheckRollup
        GH->>Backfill: state=merged, ci_status=passed
        Backfill->>Sidecar: append snapshot {state:merged, lag_bucket:24h}
    end

    loop every 10 min
        Daemon->>Main: read all
        Daemon->>Sidecar: read all
        Daemon->>Index: BuildCorpusIndex(joined)
        Note right of Index: Bucket(M, complexity_X) now has<br/>1 record with success_score=1.0<br/>(pr_merged outranks the<br/>original outcome=removed)
    end

    Note over Worker,Index: Next time someone routes a similar task...

    Worker->>Daemon: spawn task X' (same complexity)
    Daemon->>Index: Lookup(M, complexity_X)
    Index->>Daemon: scored, factor would apply at N≥5
    Daemon->>Daemon: V2 routes accordingly
```

The key insight: the main file is **immutable**. The original
`outcome=removed/failed` record is never overwritten. The merge observation
lives in the sidecar. `LoadCorpusJoined` folds the sidecar into the
record's `PRStateHistory` at read time. `DeriveSuccessScore` then sees
the merged state and produces 1.0, basis=`pr_merged` — the strongest
positive evidence available.

That's the loop closing.

---

## 7. Failure modes & what happens

| If this breaks... | Effect |
|---|---|
| `~/.oat/routing-history.jsonl` is unwritable (disk full, permission) | Logger warns, continues. Routing decisions unaffected. Records for that period are lost. |
| `gh` CLI fails / no auth | Backfiller warns, retries next tick. PR observations don't accrue, but main file keeps growing. |
| `routing-history.backfill.jsonl` missing or malformed | LoadCorpusJoined returns main records with empty PRStateHistory. Reports show immediate-signal-only scores. |
| CorpusIndex refresh fails | V2 router uses the previous snapshot (or nil → V1-equivalent). |
| Daemon crashes mid-write | Main file uses append + line-buffered writes; at most one record torn. Sidecar same. |
| User on a fresh install, no history | All buckets are empty → V2 falls through to "no corpus" branch (V1-equivalent ranking). |
| `OAT_OUTCOME_LOG=off` | NewOutcomeLogger returns nil. Routing still works exactly the same; nothing's logged. |
| `OAT_LOG_PRIVACY=strict` | task_text/summary/failure_reason redacted on disk. Hashes preserved. Reports work; sharing still requires explicit opt-in. |

The architecture's point: every async/observational component can fail
without breaking the routing-decision hot path.

---

## 8. Try it yourself

```bash
# Preview what the router would pick for a given task (no worker spawn)
oat routing route --task "Fix the typo 'recieve' in cli.py" --all

# Compare V1 vs V2 — they pick differently because they optimize for
# different things (cost vs evidence-weighted capability).

# See aggregate per-model success on your real corpus
oat routing report

# See the privacy mode you're currently in
oat routing privacy

# Walk the entire pipeline against a synthetic corpus (real ~/.oat/ untouched)
./scripts/demo-schema-v2.sh
```
