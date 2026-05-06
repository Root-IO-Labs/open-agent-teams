#!/usr/bin/env python3
"""Baseline routing-quality harness.

Three modes:

  --bootstrap
      Copy benchmarks/routing-test-repo/seed/ into a fresh scratch repo,
      git-init, push to a local bare repo, and run `oat init` on it.
      Prints the resulting repo name. Leaves the repo sitting idle; no
      workers are spawned.

  --run --repo <name> --out <path>
      For each task in task-manifest.yaml:
        1. Reset the scratch repo to the seed state (git reset --hard)
        2. `oat worker create "<task_text>"` — router picks a model
        3. Poll until the worker completes, fails, or times out
        4. Run the verify script from the task entry
        5. Append a JSONL record to --out
      Does NOT push PRs to GitHub (the scratch repo is local-only).

  --summarize <results.jsonl>
      Read the JSONL output and print a metrics table:
      $ / success per complexity tier, success rate per (model, tier).

Usage:
    python3 run_baseline.py --bootstrap
    python3 run_baseline.py --run --repo <name-from-bootstrap> --out results.jsonl
    python3 run_baseline.py --summarize results.jsonl

Limitations (documented in README.md):
- Tasks are run SEQUENTIALLY. The harness doesn't handle parallel routing.
- `git reset --hard` between tasks assumes main branch history. Tasks that
  a previous task has merged into main will affect subsequent tasks.
- Verify scripts run in the repo's worktree — they can see each other's
  state changes if `git reset` didn't run.
- No retry on network/LLM flakes; a single worker timeout counts as failure.
"""
from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

# yaml is in the pricing.py dependency chain — we assume it's importable
import yaml

HERE = Path(__file__).resolve().parent
FIXTURE_ROOT = HERE.parent
SEED_DIR = FIXTURE_ROOT / "seed"
MANIFEST_PATH = FIXTURE_ROOT / "task-manifest.yaml"
DEFAULT_BARE_REPO_ROOT = Path.home() / ".oat-routing-bench"


# ── Shell helpers ────────────────────────────────────────────────────────────


def run(cmd: list[str], cwd: Path | None = None, check: bool = True,
        capture: bool = True, timeout: float | None = None, env: dict[str, str] | None = None) -> subprocess.CompletedProcess:
    """Wrapper around subprocess.run with sane defaults."""
    result = subprocess.run(
        cmd,
        cwd=str(cwd) if cwd else None,
        capture_output=capture,
        text=True,
        timeout=timeout,
        env=env,
    )
    if check and result.returncode != 0:
        print(f"CMD FAILED: {' '.join(cmd)} (cwd={cwd})", file=sys.stderr)
        if result.stdout:
            print(f"STDOUT:\n{result.stdout}", file=sys.stderr)
        if result.stderr:
            print(f"STDERR:\n{result.stderr}", file=sys.stderr)
        raise SystemExit(result.returncode)
    return result


# ── Bootstrap ────────────────────────────────────────────────────────────────


def bootstrap() -> str:
    """Create a fresh scratch repo + oat-init it. Return the repo name."""
    DEFAULT_BARE_REPO_ROOT.mkdir(parents=True, exist_ok=True)

    ts = datetime.now().strftime("%Y%m%d-%H%M%S")
    repo_name = f"routing-bench-{ts}"
    work_dir = DEFAULT_BARE_REPO_ROOT / f"{repo_name}.git"
    bare_path = DEFAULT_BARE_REPO_ROOT / f"{repo_name}-bare.git"
    seed_copy = DEFAULT_BARE_REPO_ROOT / f"{repo_name}-src"

    # Clean up any prior attempts with the same name
    for p in (work_dir, bare_path, seed_copy):
        if p.exists():
            shutil.rmtree(p)

    # Copy seed → scratch source tree
    shutil.copytree(SEED_DIR, seed_copy)

    # Init + first commit
    run(["git", "init", "-b", "main"], cwd=seed_copy)
    run(["git", "add", "-A"], cwd=seed_copy)
    run(["git", "-c", "user.email=bench@routing.local",
         "-c", "user.name=routing-bench",
         "commit", "-m", "initial seed"], cwd=seed_copy)

    # Create bare repo to act as "origin"
    run(["git", "init", "--bare", "-b", "main", str(bare_path)])
    run(["git", "remote", "add", "origin", str(bare_path)], cwd=seed_copy)
    run(["git", "push", "origin", "main"], cwd=seed_copy)

    # oat-init on the bare URL
    print(f"Running: oat init file://{bare_path} {repo_name}", file=sys.stderr)
    result = run(["oat", "init", f"file://{bare_path}", repo_name], check=False)
    if result.returncode != 0:
        print("oat init rejected file:// URL — falling back to manual workflow:", file=sys.stderr)
        print(f"  1. gh repo create {repo_name} --private --source={seed_copy} --push", file=sys.stderr)
        print(f"  2. oat init https://github.com/<your-user>/{repo_name}", file=sys.stderr)
        print(f"Scratch source tree: {seed_copy}", file=sys.stderr)
        raise SystemExit(1)

    # CRITICAL: disable merge-queue so worker PRs don't auto-merge into main.
    # Without this, each subsequent worker branches from a contaminated main
    # containing all prior workers' work — invalidating the isolation guarantee
    # of the suite. Also disable PR-shepherd for the same reason.
    print("Disabling merge-queue and PR-shepherd on the bench repo...", file=sys.stderr)
    run(["oat", "config", repo_name, "--mq-enabled=false"])
    run(["oat", "config", repo_name, "--ps-enabled=false"])

    # Pre-install the budget-cli package + dev deps into a shared venv that
    # verify scripts can source. Without this, pytest in a worker's worktree
    # can't import budget_cli and every verify step silently fails.
    venv_dir = DEFAULT_BARE_REPO_ROOT / ".verify-venv"
    if not venv_dir.exists():
        print(f"Creating shared verify venv at {venv_dir}...", file=sys.stderr)
        run(["python3", "-m", "venv", str(venv_dir)])
        run([str(venv_dir / "bin" / "pip"), "install", "--quiet", "--upgrade", "pip"])
        # Install from the seed copy (not a worker worktree) so the venv works
        # regardless of which worker's tree we verify against.
        run([str(venv_dir / "bin" / "pip"), "install", "--quiet", "-e", f"{seed_copy}[dev]"])
        print(f"Verify venv ready: {venv_dir / 'bin' / 'pytest'}", file=sys.stderr)

    print(repo_name)  # stdout = just the repo name
    return repo_name


# ── Run suite ────────────────────────────────────────────────────────────────


def load_manifest() -> dict[str, Any]:
    with MANIFEST_PATH.open() as f:
        return yaml.safe_load(f)


def git_reset_to_seed(worktree_path: Path) -> None:
    """Reset the worker's checkout to the seed state.

    Assumes there's a `seed` tag or that origin/main still points at the
    initial commit. Simplest approach: `git fetch origin main && git reset --hard origin/main`.
    """
    run(["git", "fetch", "origin", "main"], cwd=worktree_path)
    run(["git", "reset", "--hard", "origin/main"], cwd=worktree_path)
    run(["git", "clean", "-fdx"], cwd=worktree_path)


def worker_list(repo: str) -> list[dict[str, Any]]:
    """Parse `oat worker list --repo <repo>` into structured rows."""
    r = run(["oat", "worker", "list", "--repo", repo], check=False)
    if r.returncode != 0:
        return []
    rows = []
    for line in r.stdout.splitlines():
        line = line.strip()
        if not line or line.startswith("NAME") or line.startswith("-") or "No workers" in line:
            continue
        parts = line.split()
        if len(parts) < 3:
            continue
        # Rough columns: NAME MODEL STATUS TOKENS BRANCH TASK
        rows.append({
            "name": parts[0],
            "model": parts[1] if len(parts) > 1 else "",
            "raw": line,
        })
    return rows


def poll_worker(repo: str, task_text_prefix: str, timeout_sec: int = 600) -> dict[str, Any] | None:
    """Wait for a worker matching task_text_prefix to appear and complete.

    Returns the worker's final snapshot (name, model, tokens if extractable)
    or None on timeout.
    """
    deadline = time.time() + timeout_sec
    matched_name = None

    # Phase 1: wait for worker to spawn
    while time.time() < deadline:
        for w in worker_list(repo):
            # Heuristic match: the displayed TASK column is truncated, but the
            # worker name and model are stable. We can't match exactly; fall
            # back to "any new worker that appeared after we sent the task."
            matched_name = w["name"]
            return w  # naive: first worker seen is our worker. Single-task mode.
        time.sleep(5)

    return None


def run_verify(entry: dict[str, Any], worktree: Path) -> tuple[bool, list[str]]:
    """Run all verify steps in `entry.verify` from `worktree`. Return (ok, notes)."""
    notes = []
    for step in entry.get("verify", []):
        cmd = step.get("cmd", "")
        expected_exit = int(step.get("expect_exit", 0))
        proc = subprocess.run(
            ["/bin/bash", "-c", cmd],
            cwd=str(worktree),
            capture_output=True,
            text=True,
            timeout=180,
        )
        ok = proc.returncode == expected_exit
        notes.append(f"[{'PASS' if ok else 'FAIL'}] {cmd!r} exit={proc.returncode} want={expected_exit}")
        if not ok:
            return False, notes
    return True, notes


def run_suite(repo: str, out_path: Path, repo_worktree: Path | None,
              timeout_sec: int = 600) -> None:
    """Run every task in the manifest against `repo`, appending JSONL to out_path."""
    manifest = load_manifest()
    tasks = manifest.get("tasks", [])
    print(f"Running {len(tasks)} tasks against repo={repo}, out={out_path}", file=sys.stderr)

    out_path.parent.mkdir(parents=True, exist_ok=True)
    with out_path.open("a") as out_f:
        for i, t in enumerate(tasks, 1):
            print(f"\n── [{i}/{len(tasks)}] {t['id']} ({t['complexity']}) ──", file=sys.stderr)

            if repo_worktree is not None:
                git_reset_to_seed(repo_worktree)

            # Send the task — this kicks off routing + worker spawn
            task_text = t["task_text"].strip()
            t_start = time.time()
            r = run(["oat", "worker", "create", task_text, "--repo", repo], check=False)
            if r.returncode != 0:
                rec = {
                    "ts": datetime.now(timezone.utc).isoformat(),
                    "task_id": t["id"],
                    "complexity": t["complexity"],
                    "expected_tier_floor": t.get("expected_tier_floor"),
                    "status": "spawn_failed",
                    "error": (r.stderr or "")[:500],
                }
                out_f.write(json.dumps(rec) + "\n")
                out_f.flush()
                print(f"  spawn failed: {rec['error'][:200]}", file=sys.stderr)
                continue

            # Poll for completion
            worker = poll_worker(repo, task_text[:40], timeout_sec=timeout_sec)
            wall_ms = int((time.time() - t_start) * 1000)

            if worker is None:
                rec = {
                    "ts": datetime.now(timezone.utc).isoformat(),
                    "task_id": t["id"],
                    "complexity": t["complexity"],
                    "expected_tier_floor": t.get("expected_tier_floor"),
                    "status": "timeout",
                    "wall_ms": wall_ms,
                }
                out_f.write(json.dumps(rec) + "\n")
                out_f.flush()
                print(f"  TIMEOUT after {wall_ms}ms", file=sys.stderr)
                continue

            # Give the worker time to complete — in practice workers self-complete
            # via `oat agent complete`. We wait briefly then check verify.
            # TODO: wire to routing-history.jsonl to get real completion time.
            time.sleep(10)

            # Verify
            verify_worktree = repo_worktree or Path.cwd()
            ok, notes = run_verify(t, verify_worktree) if repo_worktree else (False, ["no worktree provided"])

            rec = {
                "ts": datetime.now(timezone.utc).isoformat(),
                "task_id": t["id"],
                "complexity": t["complexity"],
                "expected_tier_floor": t.get("expected_tier_floor"),
                "worker": worker.get("name"),
                "model": worker.get("model"),
                "wall_ms": wall_ms,
                "verify_ok": ok,
                "verify_notes": notes,
                "status": "verified" if ok else "verify_failed",
            }
            out_f.write(json.dumps(rec) + "\n")
            out_f.flush()

            print(f"  {'OK' if ok else 'FAIL'}: model={worker.get('model')} wall={wall_ms}ms", file=sys.stderr)


# ── Summarize ────────────────────────────────────────────────────────────────


def summarize(jsonl_path: Path) -> None:
    """Print a metrics table from the run JSONL."""
    records = []
    for line in jsonl_path.read_text().splitlines():
        if not line.strip():
            continue
        records.append(json.loads(line))

    if not records:
        print("No records to summarize.", file=sys.stderr)
        return

    print(f"\n══ routing baseline: {len(records)} task runs ══\n")

    # Success rate by (model, complexity)
    cells: dict[tuple[str, str], dict[str, int]] = {}
    for r in records:
        key = (r.get("model", "unknown"), r.get("complexity", "unknown"))
        c = cells.setdefault(key, {"n": 0, "ok": 0, "wall_ms": 0})
        c["n"] += 1
        if r.get("verify_ok"):
            c["ok"] += 1
        c["wall_ms"] += r.get("wall_ms", 0) or 0

    print("Success rate by (model, complexity):")
    print(f"  {'MODEL':40s}  {'COMPLEXITY':12s}  {'N':>3s}  {'OK':>3s}  {'%':>5s}  {'avg_wall_ms':>12s}")
    for key in sorted(cells.keys()):
        model, cx = key
        c = cells[key]
        pct = 100.0 * c["ok"] / c["n"] if c["n"] else 0
        avg = c["wall_ms"] // c["n"] if c["n"] else 0
        print(f"  {model[:40]:40s}  {cx:12s}  {c['n']:3d}  {c['ok']:3d}  {pct:5.1f}  {avg:12d}")

    # Overall
    n_ok = sum(1 for r in records if r.get("verify_ok"))
    n_fail = len(records) - n_ok
    print(f"\nOverall: {n_ok}/{len(records)} passed, {n_fail} failed")

    # Status breakdown
    status_counts: dict[str, int] = {}
    for r in records:
        s = r.get("status", "unknown")
        status_counts[s] = status_counts.get(s, 0) + 1
    print(f"\nStatus counts: {status_counts}")


# ── CLI ──────────────────────────────────────────────────────────────────────


def main() -> None:
    ap = argparse.ArgumentParser()
    mode = ap.add_mutually_exclusive_group(required=True)
    mode.add_argument("--bootstrap", action="store_true",
                      help="Create a fresh scratch repo and oat-init it")
    mode.add_argument("--run", action="store_true",
                      help="Run all tasks against --repo, write to --out")
    mode.add_argument("--summarize", type=Path,
                      help="Print a metrics table from a JSONL results file")
    ap.add_argument("--repo", help="OAT repo name (required with --run)")
    ap.add_argument("--worktree", type=Path,
                    help="Worktree path for git reset + verify (required with --run)")
    ap.add_argument("--out", type=Path, help="JSONL output path (required with --run)")
    ap.add_argument("--timeout-sec", type=int, default=600)
    args = ap.parse_args()

    if args.bootstrap:
        bootstrap()
    elif args.run:
        if not args.repo or not args.out:
            ap.error("--run requires --repo and --out")
        run_suite(args.repo, args.out, args.worktree, timeout_sec=args.timeout_sec)
    elif args.summarize:
        summarize(args.summarize)


if __name__ == "__main__":
    main()
