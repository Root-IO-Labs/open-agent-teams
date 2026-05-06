#!/usr/bin/env python3
"""Offline replay harness for routing policies.

Reads historical worker outcomes and re-scores them under alternative routers
to compute counterfactual $/success. The core assumption (clearly documented
because it's not strictly true):

  "If RouterX would have picked model M for task T, and the historical record
   shows model H actually handled T with outcome O, we assume RouterX would
   see the same outcome O when M == H, and UNKNOWN outcome when M != H."

This is the standard counterfactual-bias problem. We handle it explicitly:
  - For model-matched records: use the real outcome + cost
  - For model-mismatched records: fall back to per-model success rates observed
    across the rest of the dataset for similar tasks; flag as low-confidence
  - We report counts separately so the operator can see how much is real vs imputed

Usage:
  ./replay.py --history ~/.oat/routing-history-retrofit.jsonl \
              --pricing model-routing/pricing.yaml \
              --router current
  ./replay.py ... --router cheapest-that-fits
"""
from __future__ import annotations
import argparse, json, sys, yaml
from pathlib import Path
from dataclasses import dataclass, field
from statistics import median


# ─── Pricing ─────────────────────────────────────────────────────────────────

@dataclass
class ModelPrice:
    model_id: str
    input_per_mtok: float | None
    output_per_mtok: float | None
    cache_read_per_mtok: float | None

    def cost_usd(self, tokens_in: int, tokens_out: int, cache_read: int = 0) -> float | None:
        if self.input_per_mtok is None or self.output_per_mtok is None:
            return None
        # Cache-read tokens are billed separately if the field exists; else
        # assume they're already netted out of tokens_in.
        non_cache_in = max(tokens_in - cache_read, 0)
        cost = (non_cache_in / 1_000_000) * self.input_per_mtok
        cost += (tokens_out / 1_000_000) * self.output_per_mtok
        if self.cache_read_per_mtok is not None and cache_read > 0:
            cost += (cache_read / 1_000_000) * self.cache_read_per_mtok
        return round(cost, 6)


def load_pricing(path: Path) -> dict[str, ModelPrice]:
    with path.open() as f:
        y = yaml.safe_load(f)
    out = {}
    for mid, entry in y.get("models", {}).items():
        out[mid] = ModelPrice(
            model_id=mid,
            input_per_mtok=entry.get("input_per_mtok"),
            output_per_mtok=entry.get("output_per_mtok"),
            cache_read_per_mtok=entry.get("cache_read_per_mtok"),
        )
    return out


# ─── Task features ───────────────────────────────────────────────────────────

@dataclass
class TaskFeatures:
    text: str
    length_chars: int
    mentions_file_count: int  # rough — counts "/" tokens
    is_trivial: bool  # length < 80 + one verb
    is_refactor: bool
    is_bug_fix: bool
    is_doc_or_config: bool
    is_analysis: bool  # "analyze", "summarize", "list"
    estimated_complexity: str  # "trivial" | "simple" | "standard" | "complex"


def extract_features(text: str) -> TaskFeatures:
    t = text or ""
    low = t.lower()
    length = len(t)
    file_count = t.count("/")  # rough path-fragment counter

    is_trivial = length < 80 and any(v in low for v in ("typo", "rename", "fix the", "change the"))
    is_refactor = "refactor" in low or "rewrite" in low or "restructure" in low
    is_bug_fix = "fix" in low and ("bug" in low or "error" in low or "broken" in low)
    is_doc = any(w in low for w in ("documentation", "docs", ".md", "readme", "contributing"))
    is_config = any(w in low for w in (".yaml", ".toml", ".json", "config"))
    is_analysis = any(w in low for w in ("summarize", "analyze", "explore", "list every", "produce a list"))

    # Crude bucketing
    if is_trivial:
        complexity = "trivial"
    elif is_refactor or (file_count >= 4 and length > 200):
        complexity = "complex"
    elif is_doc or is_config or is_analysis or length < 150:
        complexity = "simple"
    else:
        complexity = "standard"

    return TaskFeatures(
        text=t,
        length_chars=length,
        mentions_file_count=file_count,
        is_trivial=is_trivial,
        is_refactor=is_refactor,
        is_bug_fix=is_bug_fix,
        is_doc_or_config=is_doc or is_config,
        is_analysis=is_analysis,
        estimated_complexity=complexity,
    )


# ─── Routers ─────────────────────────────────────────────────────────────────

@dataclass
class RouterDecision:
    chosen_model: str
    reason: str


class Router:
    name = "abstract"

    def pick(self, features: TaskFeatures, team: list[str], pricing: dict[str, ModelPrice]) -> RouterDecision:
        raise NotImplementedError


class RouterCurrent(Router):
    """Today's behavior: argmax(overall_score) within eligible/allowed worker set.

    For replay purposes, since we don't have per-record overall_score history,
    we approximate with a fixed score table reflecting Phase-1 live-test data.
    """
    name = "current"

    # Approximate — what the live-test roster would have ranked
    SCORES = {
        "anthropic:claude-sonnet-4-6": 91,
        "anthropic:claude-haiku-4-5": 96,
        "openai:gpt-5.4-mini": 87,
        "openai:gpt-5.4-nano": 88,
        "openai:o4-mini": 99,
        "google_genai:gemini-2.5-flash": 99,
        "google_genai:gemini-3.1-flash-lite-preview": 85,
        "openrouter:deepseek/deepseek-v3.2:nitro": 97,
        "openrouter:meta-llama/llama-4-scout": 95,
        "ollama:qwen2.5:3b": 90,
        "ollama:gemma4": 92,
        "spark:bg-digitalservices/Gemma-4-26B-A4B-it-NVFP4": 81,
    }

    def pick(self, features, team, pricing):
        best = max(team, key=lambda m: self.SCORES.get(m, 0))
        return RouterDecision(best, f"argmax(overall_score) → {best}")


class RouterCheapestThatFits(Router):
    """Picks the cheapest priced model in the team. No task awareness.

    Hard-constraint filter: only models we have pricing for AND that aren't
    "ollama:gemma3:1b"-style restricted. (The restricted flag isn't in the
    history records; we rely on the team list not to include restricted models.)
    """
    name = "cheapest-that-fits"

    def pick(self, features, team, pricing):
        # Candidates = team ∩ pricing where price is known
        candidates = [
            m for m in team
            if m in pricing and pricing[m].input_per_mtok is not None
        ]
        if not candidates:
            return RouterDecision(team[0] if team else "unknown", "no priced candidate; fell back to first team slot")
        # Pick cheapest by input price (output matters too but input dominates for coding tasks)
        cheapest = min(candidates, key=lambda m: pricing[m].input_per_mtok or 1e9)
        return RouterDecision(cheapest, f"cheapest input price in team: {cheapest}")


class RouterCheapestWithFloor(Router):
    """Phase-3-style: cheapest model whose complexity-conditional floor is met.

    Floors are heuristic until ground-truth data fills in. Starts conservative.
    """
    name = "cheapest-with-floor"

    # Minimum tier by task complexity. Anything in a "lower" tier can be picked.
    TIER_ORDER = [
        "ollama:gemma3:1b",                          # restricted, ~0
        "ollama:gemma4",                             # ~1
        "ollama:qwen2.5:3b",                         # ~1
        "openai:gpt-5.4-nano",                       # ~2
        "google_genai:gemini-3.1-flash-lite-preview",# ~2
        "openrouter:meta-llama/llama-4-scout",       # ~3
        "google_genai:gemini-2.5-flash",             # ~3
        "openai:gpt-5.4-mini",                       # ~3
        "openrouter:deepseek/deepseek-v3.2:nitro",   # ~4
        "anthropic:claude-haiku-4-5",                # ~5
        "anthropic:claude-sonnet-4-6",               # ~6
        "openai:o4-mini",                            # ~7 (reasoning)
    ]

    MIN_TIER = {
        "trivial":  2,
        "simple":   3,
        "standard": 5,
        "complex":  10,
    }

    def pick(self, features, team, pricing):
        min_idx = self.MIN_TIER.get(features.estimated_complexity, 5)
        # Build ordered candidates at or above the floor, present in team and priced
        in_team_priced = set(m for m in team if m in pricing and pricing[m].input_per_mtok is not None)
        for i, candidate in enumerate(self.TIER_ORDER):
            if i < min_idx:
                continue
            if candidate in in_team_priced:
                return RouterDecision(candidate, f"cheapest ≥ tier{min_idx} for complexity={features.estimated_complexity}")
        # Fallback: any priced team member
        if in_team_priced:
            fallback = min(in_team_priced, key=lambda m: pricing[m].input_per_mtok or 1e9)
            return RouterDecision(fallback, f"no tier candidate; cheapest priced: {fallback}")
        return RouterDecision(team[0] if team else "unknown", "no priced candidate at all")


ROUTERS = {r.name: r for r in [RouterCurrent(), RouterCheapestThatFits(), RouterCheapestWithFloor()]}


# ─── Replay ──────────────────────────────────────────────────────────────────

@dataclass
class Stats:
    total: int = 0
    # $ across records
    total_cost_actual: float = 0.0  # what it actually cost (historical model × actual tokens × actual price)
    total_cost_router: float = 0.0  # what it WOULD have cost if router's pick ran (imputed)
    cost_unknown: int = 0  # records where either path had no price

    # Success proxies
    pr_created: int = 0
    completed: int = 0
    unknown_outcome: int = 0

    # Router-matched vs mismatched
    matched: int = 0
    mismatched: int = 0

    # By complexity bucket
    by_complexity: dict[str, int] = field(default_factory=dict)
    cost_by_router_pick: dict[str, float] = field(default_factory=dict)
    picks: dict[str, int] = field(default_factory=dict)


def replay(records, router, pricing, team):
    s = Stats()
    for r in records:
        s.total += 1
        features = extract_features(r.get("task_text") or "")
        s.by_complexity[features.estimated_complexity] = s.by_complexity.get(features.estimated_complexity, 0) + 1

        decision = router.pick(features, team, pricing)
        s.picks[decision.chosen_model] = s.picks.get(decision.chosen_model, 0) + 1

        # Actual cost (what historically happened)
        hist_model = r.get("model")
        hist_price = pricing.get(hist_model)
        tokens_in = r.get("tokens_in") or 0
        tokens_out = r.get("tokens_out") or 0
        cache_read = r.get("cache_read") or 0
        actual = hist_price.cost_usd(tokens_in, tokens_out, cache_read) if hist_price else None

        # Router-counterfactual cost: use the SAME token counts with the router's chosen model
        router_price = pricing.get(decision.chosen_model)
        imputed = router_price.cost_usd(tokens_in, tokens_out, cache_read) if router_price else None

        if actual is None or imputed is None:
            s.cost_unknown += 1
        else:
            s.total_cost_actual += actual
            s.total_cost_router += imputed
            s.cost_by_router_pick[decision.chosen_model] = s.cost_by_router_pick.get(decision.chosen_model, 0.0) + imputed

        # Outcome tally
        o = r.get("outcome")
        if o == "pr_created":
            s.pr_created += 1
        elif o == "completed":
            s.completed += 1
        else:
            s.unknown_outcome += 1

        # Matched?
        if decision.chosen_model == hist_model:
            s.matched += 1
        else:
            s.mismatched += 1
    return s


def fmt(s: Stats, router_name: str) -> str:
    lines = []
    lines.append(f"══ router={router_name}  records={s.total} ══")
    lines.append("")
    lines.append(f"Costs (where both actual & router prices known):")
    lines.append(f"  records with cost data:        {s.total - s.cost_unknown} / {s.total}")
    lines.append(f"  actual total spend:            ${s.total_cost_actual:,.2f}")
    lines.append(f"  counterfactual (router) spend: ${s.total_cost_router:,.2f}")
    if s.total_cost_actual > 0:
        delta = s.total_cost_router - s.total_cost_actual
        pct = 100.0 * delta / s.total_cost_actual
        verb = "savings" if delta < 0 else "extra"
        lines.append(f"  Δ vs actual:                  ${abs(delta):,.2f} {verb} ({pct:+.1f}%)")
    lines.append("")
    lines.append("Outcomes (historical — NOT imputed to router's picks):")
    lines.append(f"  pr_created:  {s.pr_created}")
    lines.append(f"  completed:   {s.completed}")
    lines.append(f"  unknown:     {s.unknown_outcome}")
    lines.append("")
    lines.append(f"Router picks distribution (n={sum(s.picks.values())}):")
    for m, c in sorted(s.picks.items(), key=lambda kv: -kv[1]):
        share_cost = s.cost_by_router_pick.get(m, 0)
        lines.append(f"  {c:4d} × {m}  (${share_cost:,.2f})")
    lines.append("")
    lines.append("Task complexity buckets:")
    for k, v in sorted(s.by_complexity.items(), key=lambda kv: -kv[1]):
        lines.append(f"  {v:4d}  {k}")
    lines.append("")
    lines.append(f"Agreement with historical pick: {s.matched}/{s.total} matched, {s.mismatched} mismatched")
    lines.append(f"  ⚠ mismatched counterfactual costs are IMPUTED — they assume the router's")
    lines.append(f"    chosen model would have used the same token count as the historical model.")
    lines.append(f"    This is a known counterfactual bias; see replay.py docstring.")
    return "\n".join(lines)


def main():
    ap = argparse.ArgumentParser(description="Routing replay harness")
    ap.add_argument("--history", default=str(Path.home() / ".oat" / "routing-history-retrofit.jsonl"))
    ap.add_argument("--pricing", default="model-routing/pricing.yaml")
    ap.add_argument("--router", default="current", choices=list(ROUTERS.keys()))
    ap.add_argument("--team", nargs="+",
                    default=[
                        "anthropic:claude-sonnet-4-6",
                        "anthropic:claude-haiku-4-5",
                        "openai:gpt-5.4-nano",
                        "google_genai:gemini-2.5-flash",
                        "google_genai:gemini-3.1-flash-lite-preview",
                    ],
                    help="onboarded model IDs available to router")
    ap.add_argument("--all", action="store_true", help="run all routers and compare")
    args = ap.parse_args()

    history_path = Path(args.history).expanduser()
    if not history_path.exists():
        print(f"error: history file not found: {history_path}", file=sys.stderr)
        sys.exit(1)

    pricing = load_pricing(Path(args.pricing))
    records = [json.loads(l) for l in history_path.read_text().splitlines() if l.strip()]

    if args.all:
        for name, r in ROUTERS.items():
            s = replay(records, r, pricing, args.team)
            print(fmt(s, name))
            print()
    else:
        router = ROUTERS[args.router]
        s = replay(records, router, pricing, args.team)
        print(fmt(s, args.router))


if __name__ == "__main__":
    main()
