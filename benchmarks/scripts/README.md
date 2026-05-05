# Internal benchmark helpers

Scripts in this directory are internal helpers invoked by the
user-facing benchmark entry points (`benchmarks/run.sh` and
`benchmarks/setup.sh`) or by other benchmark scripts. They're not
intended to be run directly by operators in day-to-day use, though
doing so is supported when debugging.

## What lives here

- `run-blackbox.sh` — shim that runs a model-generated blackbox test
  against a completed benchmark repo and writes structured JSON
  results. Invoked by `run.sh` at the gate smoke test, during the
  convergence loop, and for the final blackbox run.
- `judge-blackbox.sh` — LLM judge that compares a model-generated
  blackbox test against the human-written reference
  (`benchmarks/acceptance-test.sh`) using a structured rubric. Invoked
  by `run.sh` and `judge-cursor-gate.sh`.
- `whitebox-shim.py` — reference whitebox-testing helper saved off
  from an older `acceptance-test.sh` revision; kept for future
  whitebox benchmark work.

## What doesn't live here (and why)

Entry points and user-callable scripts stay at `benchmarks/` root so
documented invocations in `README.md`, `cursor-gate-workflow.md`, and
user-authored scripts keep working without indirection:

- `run.sh`, `setup.sh` — primary entry points
- `acceptance-test.sh`, `summarize.sh`, `collect.sh`, `cleanup.sh` —
  user-callable analysis/cleanup commands
- `compare-routing.sh`, `run-comparison.sh`, `run-routing-comparison.sh`,
  `judge-cursor-gate.sh`, `probe-model.py` — user-callable evaluation
  commands documented in `README.md` and `MODEL_COMPARISON.md`

## Path conventions inside this directory

Scripts here use `SCRIPT_DIR` to find their own location and
`BENCH_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"` to reach the benchmarks
root (for shared results paths, the reference acceptance test, etc.).
If you add a new helper to this directory, follow the same pattern so
result layouts stay consistent with what the entry-point scripts
expect.
