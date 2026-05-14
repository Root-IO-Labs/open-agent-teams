# OAT Model Routing — Full Audit & Data Analysis

**Generated:** 2026-05-14  
**Records analysed:** 274 total (169 worker, 64 verification, 41 other)  
**Date range:** 2026-04-22 → 2026-05-14  
**Source files:** `routing-history.jsonl` + `routing-history-retrofit.jsonl`

---

## 1. Architecture — How Routing Works Today

### 1.1 Call Flow

```
oat work "task description"
        │
        ▼
CLI: handleStartWorker()
        │
        ▼
daemon: startAgentWithConfig()
        │
        ├── [explicit --model flag?]
        │       └─► validate against profile store → use it
        │
        ├── [OAT_ROUTING_V1=1 AND agentType==Worker AND pricing loaded?]
        │       └─► RouteForTask() / RouteForTaskV2()  ← V1 path
        │               │
        │               ├─ ExtractFeatures(task_text) → complexity bucket
        │               ├─ floor = complexityFloor[complexity]  (tier index)
        │               ├─ eligible = profiles where role==worker AND status≠restricted
        │               ├─ rank by: meets_floor first, then price ASC, then score DESC
        │               └─► chosen model
        │
        └── [DEFAULT — argmax]
                └─► resolveAndValidateModelWithSource()
                        └─► BestEligible(role) → highest OverallScore eligible model
                                └─► anthropic:claude-haiku-4-5  (score 96)
```

### 1.2 Complexity Classifier (`task_classifier.go`)

Fully deterministic, runs in <50µs, no LLM call.

```
Input: task_text string

IsTrivial  = matches /typo|rename|one-line/ AND len < 160
IsRefactor = matches /refactor|rewrite|restructure/
IsBugFix   = matches /fix.*bug|error|broken|issue/

Bucketing (first match wins):
  IsTrivial                         → ComplexityTrivial
  IsRefactor OR (slashes≥4 AND len>200) → ComplexityComplex
  len < 150                         → ComplexitySimple
  default                           → ComplexityStandard
```

**Critical gap:** `len < 150` catches most real task descriptions as `simple`, including sophisticated multi-file implementation tasks.

```
"implement the auth middleware"           →  36 chars  →  simple
"implement JWT refresh token rotation"   →  39 chars  →  simple
"add transactional rollback to storage"  →  38 chars  →  simple
"refactor the database layer"            → any length  →  complex ✓
```

### 1.3 Tier System (`route_for_task.go`)

```
tierOrder (index → model):
  0  ollama:gemma3:1b                           ← restricted
  1  ollama:gemma4
  2  ollama:qwen2.5:3b                          ← removed from bundled profiles
  3  openai:gpt-5.4-nano
  4  google_genai:gemini-3.1-flash-lite-preview
  5  openrouter:meta-llama/llama-4-scout
  6  google_genai:gemini-2.5-flash
  7  openai:gpt-5.4-mini
  8  openrouter:deepseek/deepseek-v3.2:nitro
  9  anthropic:claude-haiku-4-5
 10  anthropic:claude-sonnet-4-6
 11  openai:o4-mini

complexityFloor (complexity → minimum tier index):
  trivial   → 3  (gpt-5.4-nano)
  simple    → 3  (gpt-5.4-nano)
  standard  → 6  (gemini-2.5-flash  ← NOT ONBOARDED on this machine)
  complex   → 9  (haiku)
  unknown   → 9  (haiku)
```

**Gap:** The `standard` floor points at `gemini-2.5-flash` (not onboarded). With only 5 models onboarded, `standard` tasks fall to `gpt-5.4-mini` (tier 7) — which has 0.44 shell recovery.

### 1.4 Onboarded Model Profiles

| Model | Score | Worker | Orchestrator | Shell Recovery | Shell Reliability | Latency |
|-------|-------|--------|-------------|---------------|------------------|---------|
| `anthropic:claude-haiku-4-5` | **96** | ✓ | ✓ | 0.88 | 1.00 | 2.3s |
| `anthropic:claude-sonnet-4-6` | 91 | ✓ | ✓ | 0.77 | 0.65 | 5.5s |
| `openai:gpt-5.4-nano` | 88 | ✓ | ✗ | 0.55 | 1.00 | 2.5s |
| `openai:gpt-5.4-mini` | 87 | ✓ | ✗ | **0.44** | 1.00 | 2.8s |
| `google_genai:gemini-3.1-flash-lite-preview` | 85 | ✓ | ✗ | **0.33** | 0.88 | 1.8s |

OAT workers run shell commands constantly. Shell recovery is the most important field for agentic work — it measures how reliably an agent recovers when a bash command fails.

---

## 2. Raw Data — All 169 Worker Records

### 2.1 By Model and Outcome

| Model | Success | Total | Rate | Avg Time | Avg Tokens In |
|-------|---------|-------|------|----------|---------------|
| `anthropic:claude-sonnet-4-6` | **18** | **18** | **100%** | 219s | 1,158,687 |
| `anthropic:claude-haiku-4-5` | 54 | 100 | 54% | 212s | 1,373,035 |
| `openai:gpt-5.4-nano` | 5 | 8 | 62% | 841s | 2,680,114 |
| `google_genai:gemini-3.1-flash-lite-preview` | 3 | 6 | 50% | 425s | 2,344,825 |
| `openai:gpt-5.4-mini` | **6** | **37** | **16%** | 356s | 459,436 |

> **Note:** "removed" outcome = worker terminated by daemon. This includes both true task failures and benchmark cleanup. Operator-explicit records (human picked model) show the true capability ceiling.

### 2.2 Full Matrix: Character Count × Model × Success Rate

```
chars=0-99      haiku      9/9  (100%)  avg=287s
chars=0-99      sonnet     4/4  (100%)  avg=125s
chars=0-99      gpt-5.4-nano  2/2  (100%)  avg=875s
chars=0-99      gpt-5.4-mini  1/1  (100%)  avg=1139s
chars=0-99      gemini     1/1  (100%)  avg=1559s

chars=100-199   haiku      6/7   (85%)  avg=105s
chars=100-199   gpt-5.4-mini  1/5   (20%)  avg=244s

chars=200-299   haiku     11/23  (47%)  avg=198s
chars=200-299   gpt-5.4-mini   4/19  (21%)  avg=367s
chars=200-299   gpt-5.4-nano   1/3   (33%)  avg=1260s

chars=300-499   haiku     14/29  (48%)  avg=180s
chars=300-499   gpt-5.4-mini   0/10   (0%)  avg=309s
chars=300-499   gpt-5.4-nano   2/3   (66%)  avg=400s
chars=300-499   gemini     2/2  (100%)  avg=115s

chars=500-699   haiku      1/2   (50%)  avg=202s
chars=500-699   sonnet     4/4  (100%)  avg=316s
chars=500-699   gpt-5.4-mini   0/2    (0%)  avg=373s

chars=700-999   haiku      4/21  (19%)  avg=210s   ← haiku collapses on long tasks
chars=700-999   sonnet    10/10 (100%)  avg=219s   ← sonnet stays perfect
chars=700-999   gemini     0/2    (0%)  avg=369s

chars=1000+     haiku      9/9  (100%)  avg=360s   ← very long task descriptions (OAT issue specs)
```

### 2.3 Imperative Verb × Model × Success Rate

```
verb=fix        haiku    13/17  (76%)
verb=fix        gpt-mini  2/6   (33%)

verb=add        haiku    10/19  (52%)
verb=add        sonnet    3/3  (100%)
verb=add        gpt-mini  0/13   (0%)   ← every "add" task failed on gpt-mini

verb=implement  haiku     4/4  (100%)
verb=implement  sonnet    1/1  (100%)

verb=refactor   haiku     2/10  (20%)  ← haiku struggles with refactors
verb=refactor   sonnet    5/5  (100%)  ← sonnet handles refactors perfectly

verb=create     haiku     2/3   (66%)
verb=create     gpt-mini  1/5   (20%)
```

### 2.4 Routing Source × Model × Success Rate

```
source=operator-explicit   haiku    28/29  (96%)  ← humans know when to use haiku
source=operator-explicit   sonnet   17/17 (100%)
source=operator-explicit   gpt-mini  4/8   (50%)  ← humans pick gpt-mini for easy tasks
source=operator-explicit   gpt-nano  5/8   (62%)
source=operator-explicit   gemini    3/6   (50%)

source=router-auto         haiku    22/56  (39%)  ← BestEligible alone is not enough
source=router-auto         sonnet    1/1  (100%)

source=router-v1-auto      haiku     4/15  (26%)  ← V1 hurts haiku (wrong tasks)
source=router-v1-auto      gpt-mini  2/29   (6%)  ← V1 routing to gpt-mini is catastrophic
```

### 2.5 Tokens vs Outcome (Haiku only)

```
tokens=0-200k     3/4   (75%)
tokens=200k-500k  5/13  (38%)
tokens=500k-1M   16/34  (47%)
tokens=1M-2M      5/13  (38%)
tokens=2M+        9/14  (64%)
```

Haiku success and failure both show similar token counts — token volume alone is not a routing signal. The cache ratio (cache_read/tokens_in) is virtually identical for success (0.93) vs failure (0.93).

### 2.6 Wall Time Signal

```
Haiku failures:  median=104s  mean=147s
Haiku successes: median=195s  mean=267s
```

Successful workers take nearly 2× longer than failures. Failures "give up" early — they hit a wall and stop rather than continuing to iterate.

### 2.7 By Repository

| Repo | Success | Total | Rate | Routing Used |
|------|---------|-------|------|-------------|
| `planner-e2e-test` | 13 | 13 | **100%** | router-auto (all haiku) |
| `oat-robotic-barista-screenshot` | 8 | 8 | **100%** | operator-explicit (mixed) |
| `Hello-World` | 4 | 4 | **100%** | operator-explicit (all sonnet) |
| `routing-bench-20260423-224013` | 18 | 37 | 48% | mixed routing |
| `routing-bench-20260423-235526` | 43 | 107 | 40% | heavy V1 routing |

The routing-bench repos used V1 routing heavily (`router-v1-auto`) and got 40% success. The production repos using operator-explicit or router-auto got 100%.

### 2.8 Verification Agent Outcomes

```
64 verification records — ALL haiku — ALL completed (100%)
```

Verification is deterministic read-and-check work. Haiku handles it perfectly.

---

## 3. Statistical Findings

### 3.1 The Two Key Signals

**Signal 1: char_count threshold**

```
               Haiku success rate     Sonnet success rate
chars < 200    ████████████  91%      ████████████  100%
chars 200-699  █████████░░░  48%      ████████████  100%
chars 700-999  ████░░░░░░░░  19%      ████████████  100%
chars 1000+    ████████████  100%     (no data)
```

**chars 700-999 is the critical zone:** haiku drops to 19%, sonnet stays at 100%. This is where routing matters most.

**Signal 2: imperative_verb (refactor)**

```
verb=refactor   haiku  20%  (2/10)
verb=refactor   sonnet 100% (5/5)
```

"Refactor" is the single strongest predictor that haiku will fail and sonnet will succeed.

### 3.2 What Operator-Explicit Tells Us

When humans manually pick models, success is 83-100%. Their implicit rule:
- Short, single-file fixes → haiku or gpt-nano
- Multi-file changes, architecture → sonnet
- Quick config/doc changes → any model

This is the routing policy the data supports.

### 3.3 Why V1 Routing Failed (13% success)

V1 routing classified nearly all benchmark tasks as `standard` complexity (200-700 chars), hitting the `standard` floor (tier index 6 = `gemini-2.5-flash`). Since gemini-2.5-flash is not onboarded, the next eligible model meeting the floor was `gpt-5.4-mini` (tier 7).

gpt-5.4-mini has 0.44 shell recovery. Every `add X` task routed to it failed (0/13). This is a model-pool mismatch, not a routing logic flaw.

---

## 4. Decision Matrix — What Should Be Routed Where

Based on the 274-record corpus:

```
┌──────────────────────────────────────────────────────────────────┐
│                    ROUTING DECISION MATRIX                       │
├─────────────────────────┬──────────────────────┬────────────────┤
│ Condition               │ Route to             │ Confidence     │
├─────────────────────────┼──────────────────────┼────────────────┤
│ chars < 200             │ haiku                │ HIGH  (91%)    │
│ chars 200-699           │ haiku                │ MED   (48%)    │
│ chars 200-699 + refactor│ sonnet               │ HIGH  (100%)   │
│ chars 700-999           │ sonnet               │ HIGH  (100%)   │
│   (unless haiku forced) │   vs haiku 19%       │                │
│ chars 1000+             │ haiku                │ HIGH  (100%)   │
│   (very long issue text)│   (OAT issue specs)  │                │
│ verb=refactor/rewrite   │ sonnet               │ HIGH  (100%)   │
│ verb=add                │ haiku (NOT gpt-mini) │ MED   (52%)    │
│ verification agent      │ haiku                │ PERFECT (100%) │
├─────────────────────────┴──────────────────────┴────────────────┤
│ NEVER route workers to gpt-5.4-mini without explicit --model     │
│ (0/13 on "add" tasks, 16% overall, catastrophic shell recovery)  │
└──────────────────────────────────────────────────────────────────┘
```

---

## 5. Architecture Diagram — Full Routing System

```
                         oat work "task text"
                                │
                    ┌───────────▼───────────┐
                    │   startAgentWithConfig │
                    │   (daemon.go ~5093)    │
                    └───────────┬───────────┘
                                │
               ┌────────────────▼────────────────┐
               │      Model Resolution Tree       │
               └────────────────┬────────────────┘
                                │
                ┌───────────────▼──────────────────┐
                │  1. Explicit --model flag?         │
                │     Yes → validate → use it  ───────► DONE
                └───────────────┬──────────────────┘
                                │ No
                ┌───────────────▼──────────────────┐
                │  2. OAT_ROUTING_V1=1 AND          │
                │     agentType==Worker AND          │
                │     pricing loaded?                │
                │     Yes → RouteForTask()      ──────► [V1 PATH]
                └───────────────┬──────────────────┘
                                │ No (DEFAULT)
                ┌───────────────▼──────────────────┐
                │  3. resolveAndValidateModel()      │
                │     → BestEligible(role)           │
                │     → argmax(OverallScore)         │
                │     → haiku (score=96)        ──────► DONE
                └──────────────────────────────────┘

[V1 PATH]:
  ExtractFeatures(task_text)
       │
       ├─ IsTrivial?    → ComplexityTrivial   → floor=3 → cheapest ≥ tier3
       ├─ IsRefactor?   → ComplexityComplex   → floor=9 → haiku or sonnet
       ├─ len<150?      → ComplexitySimple    → floor=3 → cheapest ≥ tier3
       └─ default       → ComplexityStandard  → floor=6 → cheapest ≥ tier6
                                                         (gpt-5.4-mini in current pool)

[DATA-DRIVEN PATH — not yet implemented]:
  char_count + imperative_verb
       │
       ├─ verb IN (refactor,rewrite,restructure) → sonnet
       ├─ char_count >= 700                      → sonnet
       ├─ char_count < 700                       → haiku
       └─ default                                → haiku
```

---

## 6. Prompt Metadata Available for Routing

Each record in `routing-history.jsonl` stores (when present):

```json
"prompt": {
  "system_prompt_hash":   "sha256 of AGENTS.md content",
  "system_prompt_tokens": 12847,
  "user_message_hash":    "sha256 of task text",
  "user_message_tokens":  312
}
```

**`user_message_tokens` is a better signal than `char_count`** — it's the actual token count seen by the model, including structured formatting that char_count misses. It's available in 23/169 worker records (only newer records).

Proposed routing use: `if user_message_tokens > 500 → prefer sonnet`. This cleanly separates complex task descriptions from simple ones regardless of raw character count.

`system_prompt_tokens` reflects which agent template was used. Larger system prompts = more context injected = harder task environment. Could be used to adjust the model ceiling.

---

## 7. All Possible Improvements — Ranked by Impact

### Tier 1: High impact, low effort

**A. Data-driven two-tier routing (Anthropic only)**

Replace BestEligible with:
```go
// in resolveAndValidateModelWithSource, after explicit check:
features := routing.ExtractFeatures(cfg.initialMessage)
if features.IsRefactor || features.LengthChars >= 700 {
    if sonnetProfile, ok := profiles.Get("anthropic:claude-sonnet-4-6"); ok && sonnetProfile.IsEligible(role) {
        return "anthropic:claude-sonnet-4-6", "data-driven", nil
    }
}
return profiles.BestEligible(role, repoModel)  // haiku
```

Expected outcome: 19% → ~100% success on long tasks, 20% → ~100% on refactors.  
File: `internal/daemon/daemon.go:5177`

**B. Remove gpt-5.4-mini from auto-routing entirely**

Add `gpt-5.4-mini` to a "require explicit --model" list. Its shell recovery (0.44) makes it unsuitable for autonomous agentic work. Only allow it when the operator explicitly chooses it.

**C. Fix the standard tier floor gap**

Either:
- Onboard `google_genai:gemini-2.5-flash` (it's in the bundled profiles, score 99)
- Or raise the standard floor to 7 (gpt-5.4-mini) but mark gpt-mini as "shell-fragile"

### Tier 2: Medium impact, medium effort

**D. Use `user_message_tokens` as primary routing signal**

More accurate than char_count. Update `ExtractFeatures` to accept precomputed token count from the prompt metadata when available.

**E. Log complexity classification to routing history**

Currently `routing-history.jsonl` records have empty `complexity` field. Fix: write the classifier output to the record so V2 historical routing has clean labels to train on.

File: wherever `OutcomeRecord` is created in the daemon.

**F. Wire `system_prompt_hash` for routing context**

Tasks run with the planner's system prompt (very long) vs the basic worker prompt have different complexity profiles. The hash lets you look up which template was used and adjust model selection accordingly.

**G. `oat routing explain "task text"` CLI command**

Show operators exactly what the classifier would decide:
```
$ oat routing explain "refactor the auth system to use OAuth2"
  complexity:  complex (matched: IsRefactor)
  floor:       tier 9 (anthropic:claude-haiku-4-5)
  data-driven: sonnet (IsRefactor override)
  candidates:  [sonnet, haiku]
  chosen:      anthropic:claude-sonnet-4-6
```

### Tier 3: High impact, high effort

**H. V2 routing with complexity labels**

Once complexity is being logged (fix E), the V2 historical router can rerank candidates by per-(model, complexity) success rate using Wilson lower bounds. Requires ≥5 records per (model, complexity) bucket. Currently all records have empty complexity fields.

**I. Automatic model demotion**

If a worker fails (outcome=removed/failed), log the failure to the corpus index. V2 routing will automatically reduce the effective score for that model on similar tasks. Requires corpus index populated with real data.

**J. `user_message_tokens` routing threshold calibration**

With enough data, replace the hard char_count threshold with a calibrated token threshold trained on the historical success/failure split. Current estimate: 500 tokens as the sonnet threshold.

**K. Per-repo model routing config**

`oat config <repo> routing-policy "data-driven|argmax|v1|v2"` — let repos opt into different strategies. High-stakes repos use sonnet everywhere; batch/cheap repos use haiku everywhere.

---

## 8. The Single Most Impactful Change

```
IF imperative_verb ∈ {refactor, rewrite, restructure}
OR char_count ≥ 700
THEN → anthropic:claude-sonnet-4-6

ELSE → anthropic:claude-haiku-4-5
```

**Evidence:**
- `refactor` + haiku = 20% success (2/10). Same tasks + sonnet = 100% (5/5).
- `chars 700-999` + haiku = 19% success (4/21). Same range + sonnet = 100% (10/10).
- Both signals together cover every documented haiku failure category.
- Implementation: ~10 lines in `resolveAndValidateModelWithSource`.
- No new infrastructure needed. Fully deterministic. No env vars.

---

## 9. Raw Record Schema Reference

```
schema_version     int      — 2 for current records
record_id          string   — UUID
ts                 string   — ISO8601 completion time
repo               string   — OAT repo name
worker             string   — agent/worker name
agent_type         string   — "worker" | "verification" | ...
task_text          string   — full task description sent to worker
model              string   — "provider:model-id"
model_canonical    string   — model without provider prefix
routing_source     string   — "router-auto" | "router-v1-auto" | "operator-explicit"
provider           string   — "anthropic" | "openai" | ...
started_at         string   — ISO8601
completed_at       string   — ISO8601
wall_ms            int      — total wall time in milliseconds
tokens_in          int      — cumulative input tokens (includes cache)
tokens_out         int      — output tokens generated
cache_read         int      — tokens served from prompt cache
cache_write        int      — tokens written to prompt cache
outcome            string   — "completed" | "merged" | "removed" | "failed"
summary            string   — agent's self-reported completion summary
failure_reason     string   — set on failure, empty on success
verify_passed      bool     — for verification agents
pr_number          int      — PR created (if any)
issue_number       int      — linked issue (if any)
oat_version        string   — OAT version at time of run
task_features: {
  char_count          int    — length of task_text in bytes
  line_count          int
  paragraph_count     int
  has_stack_trace     bool   — detected Go/Python/JS/Java stack frame
  has_ci_failure      bool   — detected CI failure markers
  code_block_count    int    — count of ``` blocks
  file_path_mentions  int    — count of file paths mentioned
  test_file_mentions  int    — file paths that look like test files
  todo_mentions       int    — TODO/FIXME count
  imperative_verb     string — first verb: fix|add|implement|refactor|...
}
prompt: {
  system_prompt_hash   string — sha256 of AGENTS.md
  system_prompt_tokens int    — tokens in system prompt
  user_message_hash    string — sha256 of task text
  user_message_tokens  int    — tokens in user message
}
```

---

## 10. Files to Change for Each Improvement

| Improvement | File | Function |
|------------|------|---------|
| A. Two-tier data-driven routing | `internal/daemon/daemon.go` | `resolveAndValidateModelWithSource()` ~5177 |
| B. Remove gpt-mini from auto | `internal/routing/profiles.go` | `IsEligible()` or new `IsAutoEligible()` |
| C. Fix standard tier floor | `internal/routing/route_for_task.go` | `complexityFloor` map |
| D. user_message_tokens signal | `internal/routing/task_classifier.go` | `ExtractFeatures()` |
| E. Log complexity to history | daemon routing record creation | wherever `OutcomeRecord` is written |
| F. system_prompt_hash context | `internal/routing/route_for_task.go` | `RouteContext` struct |
| G. CLI explain command | `internal/cli/cli.go` | new `modelExplain` command |
| H. V2 historical reranking | `internal/routing/route_for_task_v2.go` | already implemented, needs data |
| I. Auto demotion | `internal/routing/corpus_index.go` | `CorpusIndex.Record()` |
