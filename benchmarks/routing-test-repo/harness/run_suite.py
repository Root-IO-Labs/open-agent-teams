#!/usr/bin/env python3
"""Simplified suite runner — runs the manifest against a bootstrapped repo.

Replaces the earlier run_baseline.py --run mode which had a naive poll_worker
that matched the first worker it saw (not by task text). This version:

  - Matches worker → task by polling the TASK column against a prefix of the
    expected text.
  - Handles per-task cleanup: removes the worker and its worktree before
    moving on to the next task. No main-branch mutation — each task runs
    against the seed state via a fresh worker worktree from origin/main.
  - Writes one JSONL record per task to the results file, including
    whether the verify script passed against the worker's worktree.
  - Leaves the daemon's routing-history.jsonl alone — the daemon writes
    its own structured record when agents complete; that's our source of
    truth for cost + routing decisions.

Usage:
  python3 run_suite.py --repo <name> --out results.jsonl [--timeout-sec 600]
"""
from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import yaml

HERE = Path(__file__).resolve().parent
FIXTURE_ROOT = HERE.parent
MANIFEST_PATH = FIXTURE_ROOT / "task-manifest.yaml"


def run(cmd: list[str], cwd: Path | None = None, check: bool = False,
        capture: bool = True, timeout: float | None = None) -> subprocess.CompletedProcess:
    return subprocess.run(
        cmd,
        cwd=str(cwd) if cwd else None,
        capture_output=capture,
        text=True,
        timeout=timeout,
    )


def load_manifest() -> dict[str, Any]:
    with MANIFEST_PATH.open() as f:
        return yaml.safe_load(f)


def wait_for_worker_matching(repo: str, task_prefix: str, timeout_sec: int) -> dict[str, Any] | None:
    """Poll `oat worker list` until we see a worker whose task column matches.

    Returns the row {name, model, status, tokens, branch, task} once found, or
    None on timeout.
    """
    deadline = time.time() + timeout_sec
    # OAT truncates task text in list output — compare against a normalized short prefix.
    # Take first 30 chars, lowercase, strip punctuation to be robust.
    norm_prefix = _norm(task_prefix)[:30]

    seen_first_pass = set()
    while time.time() < deadline:
        r = run(["oat", "worker", "list", "--repo", repo])
        if r.returncode != 0:
            time.sleep(3)
            continue
        rows = _parse_worker_list(r.stdout)
        for row in rows:
            if _norm(row.get("task", ""))[:30] == norm_prefix:
                return row
            if row["name"] not in seen_first_pass:
                seen_first_pass.add(row["name"])
        time.sleep(5)
    return None


def wait_for_worker_done(repo: str, worker_name: str, timeout_sec: int) -> dict[str, Any]:
    """Block until the named worker reaches a terminal state.

    Terminal states for our purposes: "waiting for PR", "waiting for verification"
    (both indicate the worker believes it's done), or "completed". We treat
    any of these as "done enough to verify."
    """
    deadline = time.time() + timeout_sec
    last_status = ""
    while time.time() < deadline:
        r = run(["oat", "worker", "list", "--repo", repo])
        if r.returncode != 0:
            time.sleep(3)
            continue
        rows = _parse_worker_list(r.stdout)
        me = next((w for w in rows if w["name"] == worker_name), None)
        if me is None:
            # Worker disappeared (already cleaned up). Treat as done.
            return {"name": worker_name, "status": "gone", "last_status": last_status}
        status = me.get("status", "")
        last_status = status
        if any(flag in status for flag in ("waiting for PR", "waiting for verification", "completed")):
            return me
        time.sleep(10)
    return {"name": worker_name, "status": "timeout", "last_status": last_status}


def _norm(s: str) -> str:
    """Normalize text for loose matching: lowercase, strip punctuation."""
    import re
    return re.sub(r"[^a-z0-9 ]+", " ", s.lower()).strip()


def _parse_worker_list(stdout: str) -> list[dict[str, Any]]:
    rows = []
    in_table = False
    for line in stdout.splitlines():
        if line.startswith("NAME "):
            in_table = True
            continue
        if not in_table:
            continue
        if not line.strip() or line.startswith("-"):
            continue
        # Split on 2+ spaces (column separator in OAT's formatted table)
        import re
        parts = re.split(r"\s{2,}", line.strip())
        if len(parts) < 2:
            continue
        row = {"name": parts[0]}
        if len(parts) > 1:
            row["model"] = parts[1]
        if len(parts) > 2:
            row["status"] = parts[2]
        if len(parts) > 3:
            row["tokens"] = parts[3]
        if len(parts) > 4:
            row["branch"] = parts[4]
        if len(parts) > 5:
            row["task"] = parts[5]
        rows.append(row)
    return rows


VERIFY_VENV = Path.home() / ".oat-routing-bench" / ".verify-venv"


def verify_task(task: dict[str, Any], worktree: Path) -> tuple[bool, list[str]]:
    """Run the verify steps and return (ok, step-by-step notes).

    Before running verify commands, reinstall the worktree into the shared
    verify venv so `pytest` imports the worker's version of budget_cli.
    Without this, the venv keeps pointing at the seed package and verify
    silently tests the seed instead of the worker's work (or fails to import).
    """
    notes = []

    # Step 0: re-point the venv at this worktree.
    pip = VERIFY_VENV / "bin" / "pip"
    if pip.exists():
        reinstall = subprocess.run(
            [str(pip), "install", "--quiet", "--no-deps", "-e", str(worktree)],
            capture_output=True,
            text=True,
            timeout=60,
        )
        if reinstall.returncode != 0:
            notes.append(f"[WARN] venv reinstall failed: {reinstall.stderr[-200:]}")
        else:
            notes.append(f"[OK] venv re-pointed at worktree {worktree.name}")

    # Prepend the venv's bin to PATH so `pytest` resolves to the one that
    # imports the freshly-reinstalled package.
    env = os.environ.copy()
    if VERIFY_VENV.exists():
        env["PATH"] = f"{VERIFY_VENV / 'bin'}:{env.get('PATH', '')}"

    for i, step in enumerate(task.get("verify", []), 1):
        cmd = step.get("cmd", "")
        expected = int(step.get("expect_exit", 0))
        try:
            proc = subprocess.run(
                ["/bin/bash", "-c", cmd],
                cwd=str(worktree),
                capture_output=True,
                text=True,
                timeout=180,
                env=env,
            )
        except subprocess.TimeoutExpired:
            notes.append(f"[TIMEOUT] step {i}: {cmd!r}")
            return False, notes
        ok = proc.returncode == expected
        tag = "PASS" if ok else "FAIL"
        notes.append(f"[{tag}] step {i}: exit={proc.returncode} want={expected}  cmd={cmd!r}")
        if not ok:
            if proc.stdout:
                notes.append(f"  stdout (last 200): {proc.stdout[-200:]}")
            if proc.stderr:
                notes.append(f"  stderr (last 200): {proc.stderr[-200:]}")
            return False, notes
    return True, notes


def find_worktree(repo: str, worker_name: str) -> Path | None:
    """Compute the expected worktree path for a worker."""
    p = Path.home() / ".oat" / "wts" / repo / worker_name
    return p if p.exists() else None


def archive_worker_branch(bare_repo: Path, worker_name: str, dest: Path) -> bool:
    """Extract the worker's branch to `dest` via git archive. Returns True on success.

    Used as a fallback when the worker's worktree has been cleaned up but
    we still need to verify against its work (which got pushed to the bare
    repo as `work/<name>`). Also ensures `.venv-verify/` is copied in for
    pytest access — we install dev dependencies once per bootstrap.
    """
    dest.mkdir(parents=True, exist_ok=True)
    r = subprocess.run(
        ["git", "-C", str(bare_repo), "archive", f"work/{worker_name}"],
        capture_output=True,
    )
    if r.returncode != 0:
        return False
    extract = subprocess.run(
        ["tar", "-x", "-C", str(dest)],
        input=r.stdout,
    )
    return extract.returncode == 0


def bare_repo_for(repo: str) -> Path:
    """Locate the bare repo that backs `repo` (what origin points at)."""
    return Path.home() / ".oat-routing-bench" / f"{repo}-bare.git"


def cleanup_worker(repo: str, worker_name: str) -> None:
    run(["oat", "worker", "rm", worker_name, "--force", "--repo", repo])


def run_task(repo: str, task: dict[str, Any], timeout_sec: int) -> dict[str, Any]:
    """Execute a single task end-to-end. Returns a structured record."""
    task_id = task["id"]
    task_text = task["task_text"].strip()
    complexity = task["complexity"]
    floor = task.get("expected_tier_floor")

    print(f"\n── {task_id} ({complexity}, floor={floor}) ──", file=sys.stderr)
    print(f"  task: {task_text[:100]}...", file=sys.stderr)

    rec: dict[str, Any] = {
        "ts_start": datetime.now(timezone.utc).isoformat(),
        "task_id": task_id,
        "complexity": complexity,
        "expected_tier_floor": floor,
    }
    t0 = time.time()

    # 1. Spawn worker
    r = run(["oat", "worker", "create", task_text, "--repo", repo])
    if r.returncode != 0:
        rec.update({
            "status": "spawn_failed",
            "error": (r.stderr or "")[:500],
            "wall_ms": int((time.time() - t0) * 1000),
            "ts_end": datetime.now(timezone.utc).isoformat(),
        })
        print(f"  spawn FAILED: {rec['error'][:200]}", file=sys.stderr)
        return rec

    # 2. Wait for the matching worker to appear in the list
    worker = wait_for_worker_matching(repo, task_text, timeout_sec=60)
    if worker is None:
        rec.update({
            "status": "worker_not_found",
            "wall_ms": int((time.time() - t0) * 1000),
            "ts_end": datetime.now(timezone.utc).isoformat(),
        })
        print(f"  FAILED to locate worker in listing", file=sys.stderr)
        return rec

    worker_name = worker["name"]
    model = worker.get("model", "unknown")
    print(f"  worker: {worker_name} model={model}", file=sys.stderr)

    # 3. Wait for the worker to hit a terminal state
    final = wait_for_worker_done(repo, worker_name, timeout_sec=timeout_sec)
    spawn_to_done_ms = int((time.time() - t0) * 1000)

    rec.update({
        "worker": worker_name,
        "model": model,
        "worker_final_status": final.get("status"),
        "spawn_to_done_ms": spawn_to_done_ms,
    })

    if "timeout" in final.get("status", "") or final.get("status") == "timeout":
        # Give it 10 more seconds for any in-flight completion, then give up
        time.sleep(10)
        rec["status"] = "worker_timeout"
        print(f"  TIMEOUT after {spawn_to_done_ms}ms (last={final.get('last_status')})", file=sys.stderr)
        cleanup_worker(repo, worker_name)
        rec["ts_end"] = datetime.now(timezone.utc).isoformat()
        return rec

    # Let async token-accounting messages + any pending git pushes flush
    # before we read state. Without this, ~25% of runs have tokens=0 in
    # the daemon's outcome log because the rm fires before the final
    # [OAT_TOKENS] message is applied.
    time.sleep(15)

    # 4. Verify — prefer the worker's worktree; fall back to archiving
    # the pushed branch if the worktree was auto-cleaned.
    worktree = find_worktree(repo, worker_name)
    verify_source = None
    if worktree is not None:
        verify_source = worktree
    else:
        archive_dir = Path("/tmp") / f"routing-verify-{worker_name}"
        if archive_dir.exists():
            subprocess.run(["rm", "-rf", str(archive_dir)])
        if archive_worker_branch(bare_repo_for(repo), worker_name, archive_dir):
            verify_source = archive_dir
            print(f"  worktree missing; verifying from archive at {archive_dir}", file=sys.stderr)
        else:
            rec.update({
                "status": "worktree_and_archive_missing",
                "ts_end": datetime.now(timezone.utc).isoformat(),
            })
            cleanup_worker(repo, worker_name)
            return rec

    ok, notes = verify_task(task, verify_source)
    rec.update({
        "verify_ok": ok,
        "verify_notes": notes,
        "status": "verified" if ok else "verify_failed",
        "ts_end": datetime.now(timezone.utc).isoformat(),
    })
    print(f"  verify: {'PASS' if ok else 'FAIL'}", file=sys.stderr)
    for n in notes[-3:]:  # last few lines for context
        print(f"    {n}", file=sys.stderr)

    # 5. Cleanup — next task needs a clean slate
    cleanup_worker(repo, worker_name)
    time.sleep(2)  # let daemon state settle

    return rec


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--repo", required=True)
    ap.add_argument("--out", type=Path, required=True)
    ap.add_argument("--timeout-sec", type=int, default=600)
    ap.add_argument("--only", help="comma-separated list of task_ids to run (default: all)")
    args = ap.parse_args()

    manifest = load_manifest()
    tasks = manifest.get("tasks", [])
    if args.only:
        wanted = set(s.strip() for s in args.only.split(","))
        tasks = [t for t in tasks if t["id"] in wanted]

    args.out.parent.mkdir(parents=True, exist_ok=True)
    with args.out.open("a") as f:
        for i, task in enumerate(tasks, 1):
            print(f"\n══ task {i}/{len(tasks)} ══", file=sys.stderr)
            rec = run_task(args.repo, task, timeout_sec=args.timeout_sec)
            f.write(json.dumps(rec) + "\n")
            f.flush()

    print(f"\nDone. Results at {args.out}", file=sys.stderr)


if __name__ == "__main__":
    main()
