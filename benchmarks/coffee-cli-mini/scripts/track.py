#!/usr/bin/env python3
"""
track.py — show all coffee-cli-mini runs in chronological order with token trends.

Usage:
    python3 benchmarks/coffee-cli-mini/scripts/track.py
    python3 benchmarks/coffee-cli-mini/scripts/track.py --baseline BASELINE_base-*.json
"""

from __future__ import annotations

import argparse
import glob
import json
import os
import sys
from pathlib import Path


def load(path: str) -> dict:
    with open(path) as f:
        return json.load(f)


def run_mtime(path: str) -> float:
    return os.path.getmtime(path)


def grand(doc: dict) -> dict:
    # BASELINE_ aggregate format: summary.{input,output}.mean
    if "summary" in doc:
        s = doc["summary"]
        return {
            "input_tokens":  s.get("input",  {}).get("mean", 0),
            "output_tokens": s.get("output", {}).get("mean", 0),
        }
    # Per-run collect format: tokens.totals.{input,output}
    t = doc.get("tokens", {}).get("totals", {})
    if "grand" in t:
        g = t["grand"]
        return {
            "input_tokens":  g.get("input_tokens", 0),
            "output_tokens": g.get("output_tokens", 0),
        }
    return {
        "input_tokens":  t.get("input", 0),
        "output_tokens": t.get("output", 0),
    }


def cost(doc: dict) -> dict:
    # BASELINE_ aggregate does not track cost — return empty
    if "summary" in doc:
        return {}
    return doc.get("tokens", {}).get("totals", {}).get("cost", {}) or {}


def agents_tokens(doc: dict) -> dict[str, dict]:
    """Return {name: {input, output, cache_read, turns, tools}} for all agents."""
    out = {}
    t = doc.get("tokens", {})
    for a in list(t.get("agents", [])) + list(t.get("workers", [])):
        out[a.get("name", "?")] = {
            "input":      a.get("input_tokens", 0),
            "output":     a.get("output_tokens", 0),
            "cache_read": a.get("cache_read_tokens", 0),
            "turns":      a.get("assistant_turns", 0),
            "tools":      a.get("tool_calls", 0),
            "user_msgs":  a.get("user_messages", 0),
        }
    return out


def acceptance_score(results_dir: Path, repo_name: str) -> str:
    pattern = str(results_dir / f"{repo_name}*acceptance*.json")
    files = sorted(glob.glob(pattern))
    if not files:
        return " N/A"
    try:
        d = load(files[-1])
        pct = d.get("score_pct", "?")
        return f"{pct:>3}%"
    except Exception:
        return " ERR"


def delta_str(val: float, base: float, fmt: str = "+.1f") -> str:
    if base == 0:
        return "    N/A"
    pct = (val - base) / base * 100
    sign = "+" if pct >= 0 else ""
    return f"{sign}{pct:{fmt}}%"


def color(s: str, code: str) -> str:
    if not sys.stdout.isatty():
        return s
    return f"\033[{code}m{s}\033[0m"


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--dir", default=None, help="Path to results/ dir")
    ap.add_argument("--baseline", default=None, help="Glob for baseline file(s)")
    ap.add_argument("--limit", type=int, default=20, help="Max runs to show")
    args = ap.parse_args()

    script_dir = Path(__file__).parent
    results_dir = Path(args.dir) if args.dir else script_dir.parent / "results"

    if not results_dir.exists():
        print(f"Results dir not found: {results_dir}", file=sys.stderr)
        return 1

    # Load baseline
    baseline_doc = None
    if args.baseline:
        baseline_files = sorted(glob.glob(args.baseline))
    else:
        baseline_files = sorted(glob.glob(str(results_dir / "BASELINE_*.json")))

    if baseline_files:
        try:
            baseline_doc = load(baseline_files[-1])
            print(f"Baseline: {Path(baseline_files[-1]).name}")
        except Exception as e:
            print(f"Could not load baseline: {e}")

    # Find all collect JSONs (exclude BASELINE_ files)
    all_files = sorted(
        [
            p for p in results_dir.glob("*collect*.json")
            if "BASELINE_" not in p.name
        ],
        key=lambda p: p.stat().st_mtime,
    )

    if not all_files:
        print("No result files found in", results_dir)
        return 0

    # Take last N
    files = all_files[-args.limit :]

    base_cost   = cost(baseline_doc).get("cold_start", 0.0) if baseline_doc else 0.0
    base_input  = grand(baseline_doc).get("input_tokens", 0) if baseline_doc else 0
    base_output = grand(baseline_doc).get("output_tokens", 0) if baseline_doc else 0

    print()
    # Header
    w_name = 34
    print(
        f"{'Run':<{w_name}} {'Date':<11} {'Accept':>6}  "
        f"{'Cost(cold)':>10}  {'vs base':>8}  "
        f"{'Input':>10}  {'Δinput':>8}  "
        f"{'Output':>8}"
    )
    print("─" * (w_name + 11 + 7 + 11 + 10 + 11 + 11 + 9))

    for fpath in files:
        try:
            doc = load(str(fpath))
        except Exception:
            continue

        # Derive repo name from filename for acceptance lookup
        stem = fpath.stem  # e.g. oat-coffee-mini-tokred-1776999-collect
        repo_name = stem.replace("-collect", "")

        mtime = fpath.stat().st_mtime
        from datetime import datetime
        date_str = datetime.fromtimestamp(mtime).strftime("%b %d %H:%M")

        c      = cost(doc)
        g      = grand(doc)
        c_cold = c.get("cold_start", 0.0)
        inp    = g.get("input_tokens", 0)
        outp   = g.get("output_tokens", 0)

        acc_str = acceptance_score(results_dir, repo_name)

        # vs baseline
        d_cost  = delta_str(c_cold, base_cost) if base_cost else "       -"
        d_input = delta_str(inp,    base_input) if base_input else "       -"

        # Color: green for improvement, red for regression
        if sys.stdout.isatty():
            if "-" in d_cost and base_cost > 0 and c_cold < base_cost:
                d_cost  = color(d_cost, "32")  # green
                d_input = color(d_input, "32") if "-" in d_input else d_input
            elif "+" in d_cost and base_cost > 0 and c_cold > base_cost:
                d_cost  = color(d_cost, "31")  # red

            if "100" in acc_str:
                acc_str = color(acc_str, "32")
            elif "N/A" not in acc_str:
                acc_str = color(acc_str, "31")

        name = fpath.stem.replace("-collect", "")
        if len(name) > w_name:
            name = name[:w_name - 1] + "…"

        print(
            f"{name:<{w_name}} {date_str:<11} {acc_str:>6}  "
            f"${c_cold:>9.4f}  {d_cost:>8}  "
            f"{inp:>10,}  {d_input:>8}  "
            f"{outp:>8,}"
        )

    print()

    # Per-agent breakdown for the most recent run
    if files:
        latest = files[-1]
        try:
            doc = load(str(latest))
            print(f"Per-agent breakdown — {latest.stem}:")
            ag = agents_tokens(doc)
            base_ag = agents_tokens(baseline_doc) if baseline_doc else {}
            for name, tok in sorted(ag.items(), key=lambda x: -x[1]["input"]):
                base_in = base_ag.get(name, {}).get("input", 0)
                d = delta_str(tok["input"], base_in) if base_in else "       -"
                cr_pct = (
                    tok["cache_read"] / tok["input"] * 100
                    if tok["input"] else 0
                )
                # Chattiness: turns-per-user-message tells you whether the agent
                # is doing a long tool-use loop per incoming message (high = bad)
                # or responding concisely (low = good).
                if tok["user_msgs"] > 0:
                    tpm = tok["turns"] / tok["user_msgs"]
                    chat = f"{tok['turns']:>3}t/{tok['user_msgs']:>2}u ({tpm:.1f}t/msg)"
                else:
                    chat = f"{tok['turns']:>3}t/--u"
                print(
                    f"  {name:<28} input {tok['input']:>10,}  Δ {d:>8}  "
                    f"cache_hit {cr_pct:>5.1f}%  output {tok['output']:>7,}  "
                    f"{chat:>18}  tools {tok['tools']:>4}"
                )
        except Exception as e:
            print(f"Could not show per-agent breakdown: {e}")

    print()
    return 0


if __name__ == "__main__":
    sys.exit(main())
