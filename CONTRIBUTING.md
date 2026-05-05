# Contributing to OAT

Thank you for your interest in contributing to OAT (Open Agent Teams). This guide covers everything you need to get started.

## Prerequisites

- **Go 1.24+** (with toolchain 1.25)
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
- Fill out the PR template — it asks for summary, motivation, changes, and test plan
- Ensure CI passes before requesting review
- PRs to `main` must come from `dev`

## Looking for something to work on?

- Browse [`good first issue`](https://github.com/Root-IO-Labs/open-agent-teams/labels/good%20first%20issue) — small, well-scoped tasks ideal for new contributors
- Browse [`help wanted`](https://github.com/Root-IO-Labs/open-agent-teams/labels/help%20wanted) — issues maintainers would love help on
- Read [ARCHITECTURE.md](ARCHITECTURE.md) before tackling anything that touches the daemon, backend, or agent runtime — it explains the moving parts
- Check the [roadmap](roadmap/ROADMAP.md) for the larger goals if you want context on where the project is heading

If you're unsure whether a change makes sense, open a [Discussion](https://github.com/Root-IO-Labs/open-agent-teams/discussions) before writing code.

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

## Build-time Version Metadata

`oat version` prints the build identity baked into the binary. Three
values are stamped via `-ldflags "-X internal/version.*"`:

- `Version` -- the release tag (or `0.0.0-dev` for local builds)
- `Commit` -- short git SHA the binary was built from
- `Date` -- UTC build timestamp (RFC-3339)

Dev builds: `make build` / `make dev-install` / `go install ./cmd/oat`
all embed whatever `git describe` resolves to, falling back to
`0.0.0-dev` when you're not on a tagged commit. `oat version` then
prints `oat 0.0.0+<sha>-dev` so the short SHA is still useful for bug
reports.

Release builds: goreleaser and the CI release workflow pass the real
tag (`v0.1.0`, etc.) through the same variables. No code change is
required to cut a release beyond tagging a `v*` commit — the build
system takes care of stamping.

If you need to override values by hand:

```bash
VERSION=v0.1.0-rc1 COMMIT=$(git rev-parse --short HEAD) DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ) make build
```

Programmatic access lives in `internal/version` (`version.Current()` /
`version.Info`); avoid reading the exported `internal/cli.Version`
string for new code.

## Tuning Model Runtime Parameters

Per-model overrides for daemon defaults such as `max_tokens` and the nudge
interval live in the `runtime:` block of each profile under
`model-routing/profiles/<provider>__<model>.yaml`. Zero values (or a missing
`runtime:` block) fall back to the daemon defaults, so profiles only need
to enumerate the knobs they want to override.

YAML shape:

```yaml
runtime:
  max_tokens: 16000           # passed through as --model-params max_tokens
  nudge_interval_seconds: 120 # minimum seconds between supervisor nudges
```

Two edit paths:

- **CLI (recommended for one-off tweaks):** `oat model set <provider:model> --max-tokens N --nudge-interval SECONDS`. Updates the YAML in place, mirrors the file to `~/.oat/model-profiles/`, and asks the running daemon to reload profiles. Either flag is optional; at least one must be supplied.
- **YAML-edit (recommended for checked-in defaults):** edit the profile file directly, commit the change, then reload with `oat daemon restart` or by running `oat model set <provider:model>` against any already-set field so the daemon picks up the edit.

Both paths end up writing the same YAML, so use whichever fits the workflow.

## Releasing

Releases are tag-driven and handled by GoReleaser + GitHub Actions. See [docs/RELEASING.md](docs/RELEASING.md) for the full process — including how to cut a release, the prerequisites (public repos + Homebrew tap secret), and how to do a local dry-run with `make release-snapshot`.

## Reporting Security Issues

Please do **not** file public issues for security vulnerabilities. Use [GitHub Security Advisories](https://github.com/Root-IO-Labs/open-agent-teams/security/advisories/new) to report them privately. See [SECURITY.md](SECURITY.md) for full details.

## Code of Conduct

This project follows the [Contributor Covenant Code of Conduct](CODE_OF_CONDUCT.md). By participating, you are expected to uphold this code.

## Questions?

Open a [GitHub Discussion](https://github.com/Root-IO-Labs/open-agent-teams/discussions) or file an issue.
