#!/usr/bin/env python3
"""Join suite-run JSONL with daemon routing-history.jsonl and compute metrics.

  suite JSONL (from run_suite.py): per-task verify result
  routing-history.jsonl (from daemon outcome_logger): per-worker cost + tokens

Key = (repo, worker). Output:
  - per-task: task_id, complexity, model, verify_ok, cost_usd, wall_ms, tokens
  - aggregates: success rate per (model × complexity), $ / success per tier,
    cost counterfactual vs "always sonnet"
"""
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

import yaml


def load_pricing(path: Path) -> dict[str, dict[str, float]]:
    with path.open() as f:
        y = yaml.safe_load(f)
    out = {}
    for mid, entry in y.get("models", {}).items():
        out[mid] = {
            "input": entry.get("input_per_mtok") or 0.0,
            "output": entry.get("output_per_mtok") or 0.0,
            "cache_read": entry.get("cache_read_per_mtok") or 0.0,
        }
    return out


def compute_cost(tokens_in: int, tokens_out: int, cache_read: int,
                 pricing: dict[str, float]) -> float:
    non_cache = max(tokens_in - cache_read, 0)
    return (
        non_cache / 1e6 * pricing["input"]
        + tokens_out / 1e6 * pricing["output"]
        + cache_read / 1e6 * pricing["cache_read"]
    )


def load_jsonl(path: Path) -> list[dict[str, Any]]:
    records = []
    for line in path.read_text().splitlines():
        if not line.strip():
            continue
        try:
            records.append(json.loads(line))
        except json.JSONDecodeError:
            pass
    return records


def join_suite_and_history(suite: list[dict], history: list[dict]) -> list[dict]:
    """For each suite record, find its matching routing-history entry by (repo, worker)."""
    by_key = {}
    for h in history:
        key = (h.get("repo"), h.get("worker"))
        by_key[key] = h

    joined = []
    for s in suite:
        worker = s.get("worker")
        if not worker:
            # Spawn-failed / no-worker records — keep as-is without cost.
            joined.append({**s, "cost_usd": None, "tokens_in": None, "tokens_out": None})
            continue
        # Match on worker name only. Prefer history's model (fully qualified
        # provider:id) over suite record (which stores the short form from
        # `oat worker list` output).
        match = None
        for h in history:
            if h.get("worker") == worker:
                match = h
                break
        if match is None:
            joined.append({**s, "cost_usd": None, "tokens_in": None, "tokens_out": None})
            continue
        merged = {
            **s,
            "tokens_in": match.get("tokens_in"),
            "tokens_out": match.get("tokens_out"),
            "cache_read": match.get("cache_read"),
            "cache_write": match.get("cache_write"),
            "daemon_wall_ms": match.get("wall_ms"),
            "daemon_outcome": match.get("outcome"),
            "routing_source": match.get("routing_source"),
            "summary": match.get("summary"),
        }
        # Override the short-form model with the fully-qualified one from history.
        if match.get("model"):
            merged["model"] = match["model"]
        joined.append(merged)
    return joined


def print_report(joined: list[dict], pricing: dict[str, dict[str, float]]) -> None:
    # Compute cost for each record
    for r in joined:
        if r.get("tokens_in") is None:
            r["cost_usd"] = None
            continue
        model = r.get("model", "")
        if model not in pricing:
            r["cost_usd"] = None
            continue
        r["cost_usd"] = compute_cost(
            r["tokens_in"], r["tokens_out"], r.get("cache_read") or 0, pricing[model]
        )

    # Per-task table
    print("══ Per-task results ══\n")
    cols = f"{'task_id':18s}  {'cx':9s}  {'model':36s}  {'ok':3s}  {'$':>7s}  {'wall_s':>7s}  {'tok_in':>9s}  {'tok_out':>7s}  {'status':18s}"
    print(cols)
    print("─" * len(cols))
    for r in joined:
        task_id = r.get("task_id", "?")
        cx = r.get("complexity", "?")
        model = (r.get("model") or "-")[:36]
        ok = r.get("verify_ok")
        ok_str = "PASS" if ok else "FAIL" if ok is False else "-"
        cost = r.get("cost_usd")
        cost_str = f"${cost:.3f}" if cost is not None else "-"
        wall = r.get("daemon_wall_ms") or r.get("spawn_to_done_ms") or 0
        wall_s = f"{wall/1000:.0f}" if wall else "-"
        tok_in = r.get("tokens_in") or 0
        tok_out = r.get("tokens_out") or 0
        status = r.get("status", "?")[:18]
        print(f"{task_id:18s}  {cx:9s}  {model:36s}  {ok_str:3s}  {cost_str:>7s}  {wall_s:>7s}  {tok_in:>9d}  {tok_out:>7d}  {status:18s}")

    # Aggregates: (model, complexity)
    print("\n══ Success rate by (model × complexity) ══\n")
    cells: dict[tuple[str, str], dict[str, Any]] = {}
    for r in joined:
        key = (r.get("model", "unknown"), r.get("complexity", "unknown"))
        c = cells.setdefault(key, {"n": 0, "ok": 0, "cost": 0.0, "wall_ms": 0})
        c["n"] += 1
        if r.get("verify_ok"):
            c["ok"] += 1
        if r.get("cost_usd") is not None:
            c["cost"] += r["cost_usd"]
        c["wall_ms"] += r.get("daemon_wall_ms") or 0

    cols2 = f"{'model':36s}  {'cx':9s}  {'n':>3s}  {'ok':>3s}  {'%':>5s}  {'$/success':>10s}  {'avg_s':>7s}"
    print(cols2)
    print("─" * len(cols2))
    for key in sorted(cells.keys()):
        model, cx = key
        c = cells[key]
        pct = 100.0 * c["ok"] / c["n"] if c["n"] else 0
        dps = f"${c['cost']/c['ok']:.3f}" if c["ok"] else "-"
        avg = f"{c['wall_ms']/c['n']/1000:.0f}" if c["n"] else "-"
        print(f"{model[:36]:36s}  {cx:9s}  {c['n']:>3d}  {c['ok']:>3d}  {pct:5.1f}  {dps:>10s}  {avg:>7s}")

    # Overall numbers
    print("\n══ Overall ══")
    total_cost = sum(r.get("cost_usd") or 0 for r in joined)
    total_ok = sum(1 for r in joined if r.get("verify_ok"))
    total_n = len(joined)
    print(f"  tasks run:       {total_n}")
    print(f"  verified passes: {total_ok}  ({100.0 * total_ok / total_n:.1f}%)")
    print(f"  total spend:     ${total_cost:.2f}")
    if total_ok:
        print(f"  $ / success:     ${total_cost / total_ok:.3f}")

    # Counterfactual: what if every task had gone to sonnet?
    sonnet_price = pricing.get("anthropic:claude-sonnet-4-6", {"input": 3.0, "output": 15.0, "cache_read": 0.3})
    cf_cost = 0.0
    for r in joined:
        if r.get("tokens_in") is None:
            continue
        cf_cost += compute_cost(r["tokens_in"], r["tokens_out"], r.get("cache_read") or 0, sonnet_price)
    if cf_cost > 0:
        print(f"  counterfactual (always sonnet): ${cf_cost:.2f}")
        if total_cost > 0:
            savings_pct = 100.0 * (cf_cost - total_cost) / cf_cost
            print(f"  savings vs sonnet: ${cf_cost - total_cost:.2f} ({savings_pct:.1f}%)")

    # Routing-source distribution
    sources: dict[str, int] = {}
    for r in joined:
        s = r.get("routing_source") or "n/a"
        sources[s] = sources.get(s, 0) + 1
    print(f"\n  routing_source mix: {sources}")


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--suite", type=Path, required=True, help="JSONL from run_suite.py")
    ap.add_argument("--history", type=Path,
                    default=Path.home() / ".oat" / "routing-history.jsonl")
    ap.add_argument("--pricing", type=Path, default=Path("model-routing/pricing.yaml"))
    args = ap.parse_args()

    suite = load_jsonl(args.suite)
    history = load_jsonl(args.history)
    pricing = load_pricing(args.pricing)

    print(f"Loaded {len(suite)} suite records and {len(history)} history records\n",
          file=sys.stderr)

    joined = join_suite_and_history(suite, history)
    print_report(joined, pricing)


if __name__ == "__main__":
    main()
