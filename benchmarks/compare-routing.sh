#!/usr/bin/env bash
set -euo pipefail

# Generate a markdown comparison report from two collect.json files.
#
# Usage:
#   ./benchmarks/compare-routing.sh \
#     --baseline results/<baseline>/collect.json \
#     --routing results/<routing>/collect.json \
#     --output comparison.md

usage() {
    cat <<'EOF'
Usage: ./benchmarks/compare-routing.sh --baseline <file> --routing <file> [--output <file>]

Compare a single-model baseline run against a multi-model routing run.

Required:
  --baseline <file>   collect.json from baseline (single-model) run
  --routing <file>    collect.json from routing (multi-model) run

Options:
  --output <file>     Output markdown file (default: stdout)
  --help              Show this help message
EOF
    exit 0
}

BASELINE=""
ROUTING=""
OUTPUT=""

while [[ $# -gt 0 ]]; do
    case $1 in
        --baseline) BASELINE="$2"; shift 2 ;;
        --routing) ROUTING="$2"; shift 2 ;;
        --output) OUTPUT="$2"; shift 2 ;;
        --help) usage ;;
        *) echo "Error: Unknown flag '$1'"; exit 1 ;;
    esac
done

if [[ -z "$BASELINE" || -z "$ROUTING" ]]; then
    echo "Error: --baseline and --routing are required"
    exit 1
fi

if [[ ! -f "$BASELINE" ]]; then
    echo "Error: Baseline file not found: $BASELINE"
    exit 1
fi

if [[ ! -f "$ROUTING" ]]; then
    echo "Error: Routing file not found: $ROUTING"
    exit 1
fi

REPORT=$(python3 -c "
import json, sys

with open('${BASELINE}') as f:
    base = json.load(f)
with open('${ROUTING}') as f:
    route = json.load(f)

def get(d, *keys, default='—'):
    v = d
    for k in keys:
        if isinstance(v, dict):
            v = v.get(k, default)
        else:
            return default
    return v

def delta(a, b, fmt='{}'):
    try:
        a, b = float(a), float(b)
        d = b - a
        sign = '+' if d > 0 else ''
        return f'{sign}{fmt.format(d)}'
    except (ValueError, TypeError):
        return '—'

def pct(n, d):
    try:
        return f'{float(n)/float(d)*100:.0f}%' if float(d) > 0 else '—'
    except:
        return '—'

# Extract metrics
b_issues_closed = get(base, 'issues', 'closed', default=0)
b_issues_total = get(base, 'issues', 'total', default=0)
r_issues_closed = get(route, 'issues', 'closed', default=0)
r_issues_total = get(route, 'issues', 'total', default=0)

b_prs_merged = get(base, 'pull_requests', 'merged', default=0)
b_prs_total = get(base, 'pull_requests', 'total', default=0)
r_prs_merged = get(route, 'pull_requests', 'merged', default=0)
r_prs_total = get(route, 'pull_requests', 'total', default=0)

b_ci_passed = get(base, 'pull_requests', 'ci_passed', default=0)
r_ci_passed = get(route, 'pull_requests', 'ci_passed', default=0)

b_self_rate = get(base, 'worker_autonomy', 'self_completion_rate', default=0)
r_self_rate = get(route, 'worker_autonomy', 'self_completion_rate', default=0)

# Routing details
routing_data = get(route, 'routing', default={})
per_model = routing_data.get('per_model', {}) if isinstance(routing_data, dict) else {}
auto_selected = routing_data.get('auto_selected', 0) if isinstance(routing_data, dict) else 0
explicit = routing_data.get('explicit', 0) if isinstance(routing_data, dict) else 0

lines = []
lines.append('# Routing A/B Comparison Report')
lines.append('')
lines.append(f'Generated: {get(route, \"collected_at\", default=\"unknown\")}')
lines.append('')
lines.append('## Summary')
lines.append('')
lines.append('| Metric | Baseline (single) | Routing (multi) | Delta |')
lines.append('|--------|-------------------|-----------------|-------|')
lines.append(f'| Issues Closed | {b_issues_closed}/{b_issues_total} ({pct(b_issues_closed, b_issues_total)}) | {r_issues_closed}/{r_issues_total} ({pct(r_issues_closed, r_issues_total)}) | {delta(b_issues_closed, r_issues_closed)} |')
lines.append(f'| PRs Merged | {b_prs_merged}/{b_prs_total} | {r_prs_merged}/{r_prs_total} | {delta(b_prs_merged, r_prs_merged)} |')
lines.append(f'| CI Passed | {b_ci_passed} | {r_ci_passed} | {delta(b_ci_passed, r_ci_passed)} |')
lines.append(f'| Self-Completion Rate | {b_self_rate} | {r_self_rate} | {delta(b_self_rate, r_self_rate, \"{:.2f}\")} |')
lines.append('')

if per_model:
    lines.append('## Model Assignment Breakdown (routing run)')
    lines.append('')
    lines.append('| Model | Workers Assigned |')
    lines.append('|-------|-----------------|')
    for model, stats in sorted(per_model.items()):
        assigned = stats.get('workers_assigned', 0) if isinstance(stats, dict) else 0
        lines.append(f'| {model} | {assigned} |')
    lines.append('')
    lines.append(f'Auto-selected: {auto_selected} | Explicit: {explicit}')
    lines.append('')

lines.append('## Notes')
lines.append('')
lines.append('- Baseline: supervisor uses a single model for all workers')
lines.append('- Routing: supervisor chooses model per-task from available pool')
lines.append('- Higher issues closed and PRs merged = better convergence')
lines.append('- Higher self-completion rate = less daemon intervention needed')

print('\\n'.join(lines))
")

if [[ -n "$OUTPUT" ]]; then
    mkdir -p "$(dirname "$OUTPUT")"
    echo "$REPORT" > "$OUTPUT"
    echo "Report written to: $OUTPUT"
else
    echo "$REPORT"
fi
