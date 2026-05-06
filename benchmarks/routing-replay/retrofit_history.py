#!/usr/bin/env python3
"""Retrofit routing-history records from existing worker logs.

Reads ~/.oat/output/<repo>/workers/*.log, extracts:
  - task_text (first "Task:" USER turn, truncated)
  - model (from [OAT_MODEL] lines; uses the FIRST one)
  - tokens_in/out/cache (cumulative, from the LAST [OAT_TOKENS] line)
  - wall_ms (first turn timestamp → last turn timestamp)
  - outcome (heuristic: PR URL present → "pr_created"; "complete" signal → "completed"; else "unknown")

Writes JSONL to stdout.
"""
import json, os, re, sys
from pathlib import Path
from datetime import datetime

OAT_OUTPUT = Path.home() / ".oat" / "output"
MODEL_RE = re.compile(r"^\[OAT_MODEL\]\s+(\S+)", re.M)
TOKENS_RE = re.compile(r"^\[OAT_TOKENS\]\s+(\{.*\})", re.M)
TS_RE = re.compile(r"^\[(\d{2}):(\d{2}):(\d{2})\]\s+(USER|ASSISTANT|TOOL|RESULT):", re.M)
TASK_RE = re.compile(r"^\s+Task:\s*(.+?)(?:\n\s*$|\n\n)", re.M | re.S)
PR_RE = re.compile(r"https://github\.com/[^\s]+/pull/(\d+)")
COMPLETE_RE = re.compile(r"oat agent complete|\bcomplete\b.*(?:signal|successfully)", re.I)


def parse_log(log_path: Path) -> dict | None:
    try:
        content = log_path.read_text(errors="replace")
    except Exception:
        return None
    if len(content) < 200:
        return None

    m_model = MODEL_RE.search(content)
    if not m_model:
        return None  # no model annotation — skip

    # First USER turn's "Task:" line
    task_text = None
    m_task = TASK_RE.search(content)
    if m_task:
        task_text = m_task.group(1).strip()[:400]

    # Last OAT_TOKENS line = cumulative totals
    tokens_in = tokens_out = cache_read = 0
    for m in TOKENS_RE.finditer(content):
        try:
            obj = json.loads(m.group(1))
            tokens_in = obj.get("cumulative_input", tokens_in)
            tokens_out = obj.get("cumulative_output", tokens_out)
            cache_read = obj.get("cache_read", cache_read)
        except Exception:
            pass

    # First and last timestamps (HH:MM:SS only; no date)
    ts_matches = TS_RE.findall(content)
    wall_ms = None
    if len(ts_matches) >= 2:
        def to_s(ts):
            h, mn, s, _ = ts
            return int(h) * 3600 + int(mn) * 60 + int(s)
        first = to_s(ts_matches[0])
        last = to_s(ts_matches[-1])
        if last < first:
            # crossed midnight — approximate
            last += 86400
        wall_ms = (last - first) * 1000

    # Outcome heuristics
    pr_match = PR_RE.search(content)
    has_pr = bool(pr_match)
    has_complete = bool(COMPLETE_RE.search(content))

    if has_pr:
        outcome = "pr_created"
    elif has_complete:
        outcome = "completed"
    else:
        outcome = "unknown"

    # Repo + worker name from path: ~/.oat/output/<repo>/workers/<name>.log
    repo = log_path.parent.parent.name
    worker = log_path.stem

    # File mtime as rough timestamp for ordering
    mtime = datetime.fromtimestamp(log_path.stat().st_mtime).isoformat()

    return {
        "ts": mtime,
        "repo": repo,
        "worker": worker,
        "task_text": task_text,
        "model": m_model.group(1).strip(),
        "tokens_in": tokens_in,
        "tokens_out": tokens_out,
        "cache_read": cache_read,
        "wall_ms": wall_ms,
        "outcome": outcome,
        "pr_number": int(pr_match.group(1)) if pr_match else None,
        "source": "retrofit_worker_log",
    }


def main():
    outdir = Path.home() / ".oat"
    out_path = outdir / "routing-history.jsonl"

    records = []
    for log_path in OAT_OUTPUT.glob("*/workers/*.log"):
        rec = parse_log(log_path)
        if rec and rec["task_text"] and rec["model"]:
            records.append(rec)

    # Sort by timestamp (oldest first)
    records.sort(key=lambda r: r["ts"])

    # Write (append mode — retrofit is additive)
    retrofit_path = outdir / "routing-history-retrofit.jsonl"
    with retrofit_path.open("w") as f:
        for r in records:
            f.write(json.dumps(r, default=str) + "\n")

    # Summary
    by_model = {}
    by_outcome = {}
    for r in records:
        by_model[r["model"]] = by_model.get(r["model"], 0) + 1
        by_outcome[r["outcome"]] = by_outcome.get(r["outcome"], 0) + 1

    print(f"wrote {len(records)} retrofit records → {retrofit_path}", file=sys.stderr)
    print(f"by model:", file=sys.stderr)
    for m, c in sorted(by_model.items(), key=lambda kv: -kv[1]):
        print(f"  {c:4d}  {m}", file=sys.stderr)
    print(f"by outcome:", file=sys.stderr)
    for o, c in sorted(by_outcome.items(), key=lambda kv: -kv[1]):
        print(f"  {c:4d}  {o}", file=sys.stderr)


if __name__ == "__main__":
    main()
