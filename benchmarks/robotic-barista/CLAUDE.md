# Repo rules for AI agents

## Non-negotiables
- Keep PRs small and focused.
- Never weaken CI.
- Run ./scripts/check.sh before opening a PR.
- Run `ruff check --fix && ruff format .` before every push. Fix all lint errors locally.
- If blocked: create a new issue labeled blocker:* and stop.

## Spec-First Development
- **Primary goal**: Implement the operational specification to deliver value.
- **Tests validate**: Tests are validation tools, not implementation guides.
- **Spec is truth**: Operational specification is the source of truth.
- **No test gaming**: Do not access test implementation code. Implement to spec, not to pass tests.
- **Test arbitration**: If tests fail and you believe test is wrong, create `blocker:test-arbitration` issue.

## Test Access Restrictions
- ✅ Access: Operational spec, user manual, interface contracts (as specs), test results, test specifications
- ❌ No access: Test implementation code, test internals, test fixtures (unless needed for requirements)

## Test Resilience
- Contract and interface tests must handle incremental implementation gracefully.
- Use conditional skip markers (e.g., `pytest.importorskip`, `@pytest.mark.skipif`) for command groups or features not yet implemented.
- Tests should validate what exists without hard-failing on what doesn't -- parallel workers are adding features concurrently.

## PR hygiene
- Link PR to issue: "Closes #123" in the PR description.
- Include verification output (CI link or gate output).
- Explain value delivered per operational specification.
