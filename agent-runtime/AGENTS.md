# OAT Agent Runtime

This directory holds the Python agent runtime that ships with OAT. The runtime
is what each `oat-agent` process actually runs — built on top of LangChain's
[Deep Agents](https://github.com/langchain-ai/deepagents) framework with
OAT-specific extensions for orchestration, token tracking, sidecar event
emission, and the `oat` CLI integrations.

## Layout

```
agent-runtime/
├── libs/
│   ├── deepagents/           # Vendored Deep Agents SDK
│   ├── cli/                  # The oat-agent CLI (Click + Textual TUI)
│   ├── acp/                  # Agent Context Protocol support
│   ├── harbor/               # Eval harness used by benchmarks
│   └── partners/             # Provider-specific adapters (Daytona, Modal, …)
└── examples/                 # Standalone agent examples
```

Each library has its own `README.md` with build, test, and usage instructions.

## Building

The Go-side `./scripts/install.sh` symlinks `agent-runtime/libs/cli` into
`~/.oat/agent-runtime/` and creates a `uv`-managed virtualenv inside it. After
that, `oat-agent` is available on your PATH.

If you want to work on the runtime directly without the install script:

```bash
cd agent-runtime/libs/cli
uv sync --frozen
uv run oat-agent --help
```

## Testing

```bash
cd agent-runtime/libs/cli
uv sync --frozen --group test
uv run --group test pytest -q tests/unit_tests/
```

The Go side of OAT has Python-side token-tracking integration tests; see
`.github/workflows/main.yml` for the full matrix.

## Conventions

- Python 3.11+ required.
- `uv` for package management — do not use `pip` directly inside these libs.
- `ruff` for linting and formatting; `ty` for static type checks. Prefer inline
  `# noqa: RULE` over `[tool.ruff.lint.per-file-ignores]` for one-off
  exceptions; reserve `per-file-ignores` for categorical policy (e.g.
  `tests/**`).
- Use single backticks (`code`) for inline code in docstrings — not the
  Sphinx-style double-backtick form.

## Where things live

| Need | Look in |
|------|---------|
| `oat-agent` CLI entry point | `libs/cli/deepagents_cli/` |
| Token tracking + emission | `libs/cli/deepagents_cli/oat_tokens.py` |
| Sidecar event emitter | `libs/cli/deepagents_cli/sidecar_emitter.py` |
| Tool allowlist & sandbox | `libs/acp/` |
| Eval harness | `libs/harbor/` |
| Example agents | `examples/` |

For the broader OAT architecture and how agents fit into the daemon →
worker → merge-queue flow, see [`/ARCHITECTURE.md`](../ARCHITECTURE.md) and
[`/docs/AGENTS.md`](../docs/AGENTS.md).
