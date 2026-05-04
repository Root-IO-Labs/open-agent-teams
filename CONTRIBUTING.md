# Contributing to OAT

Thank you for your interest in contributing to OAT (Open Agent Teams). This guide covers everything you need to get started.

## Prerequisites

- **Go 1.24.2+** (with toolchain 1.25)
- **Python 3.11+** and **[uv](https://docs.astral.sh/uv/)** for agent-runtime work
- **[GitHub CLI (`gh`)](https://cli.github.com/)** authenticated with your account
- **Git** with access to the repository

## Getting Started

```bash
# Clone and build
git clone https://github.com/Root-IO-Labs/open-agent-teams.git
cd open-agent-teams
go install ./cmd/oat

# Run the fast pre-commit checks
make pre-commit
```

See [docs/QUICKSTART.md](docs/QUICKSTART.md) for a full setup walkthrough and architecture overview.

## Development Workflow

### Branch Naming

- `feature/*` -- new features and enhancements
- `bugfix/*` -- bug fixes

### Making Changes

1. Create a branch off `dev`:
   ```bash
   git checkout dev && git pull
   git checkout -b feature/your-feature-name
   ```

2. Make your changes and run checks before pushing:
   ```bash
   make pre-commit    # Fast: build + unit tests + verify docs
   make check-all     # Full: all checks that GitHub CI runs
   ```

3. Push and open a PR targeting the `dev` branch.

### Commit Style

Use conventional-style prefixes:

- `feat(scope): description` -- new features
- `fix(scope): description` -- bug fixes
- `docs(scope): description` -- documentation changes
- `refactor(scope): description` -- code restructuring
- `test(scope): description` -- test additions or fixes

The scope is optional but encouraged (e.g., `daemon`, `cli`, `routing`, `tui`).

## Pull Requests

- Target the **`dev`** branch (not `main`)
- Add the **`oat`** label to your PR
- Ensure CI passes before requesting review
- PRs to `main` must come from `dev`

## Testing

### Go Tests

```bash
go test ./...                              # All tests
go test ./internal/daemon                  # Single package
go test -v ./test/...                      # E2E integration tests
go test ./internal/state -run TestSave     # Single test

# Skip actual agent startup in E2E tests
OAT_TEST_MODE=1 go test ./test/...
```

### Python Tests (agent-runtime)

```bash
cd agent-runtime/libs/cli
uv sync --frozen --group test
uv run --group test pytest -q tests/unit_tests/
```

### Pre-commit Hook

Install the git pre-commit hook to catch issues early:

```bash
make install-hooks
```

## Project Structure

| Directory | Purpose |
|-----------|---------|
| `cmd/oat` | CLI entry point |
| `internal/cli` | CLI command implementations |
| `internal/daemon` | Background daemon process |
| `internal/state` | State persistence |
| `internal/routing` | Model routing logic |
| `internal/tui` | Terminal UI |
| `internal/prompts` | Agent system prompts (embedded at compile) |
| `internal/templates` | Agent prompt templates |
| `pkg/backend` | Public backend abstraction |
| `pkg/config` | Path configuration |
| `agent-runtime` | Python agent runtime |
| `benchmarks` | Benchmark suite and results |

## Regenerating Docs

If you modify CLI commands, regenerate the documentation:

```bash
go generate ./pkg/config
```

## Code of Conduct

This project follows the [Contributor Covenant Code of Conduct](CODE_OF_CONDUCT.md). By participating, you are expected to uphold this code.

## Questions?

Open a [GitHub Discussion](https://github.com/Root-IO-Labs/open-agent-teams/discussions) or file an issue.
