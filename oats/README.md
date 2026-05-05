# oats/

Agent-targeted documentation. Files in this directory are written to be
consumed by AI coding assistants (Cursor, Claude Code, Codex, etc.) as
copy-pasteable prompts and contracts — not as human-facing tutorials.

| File | Purpose |
|---|---|
| [`INSTALL.md`](INSTALL.md) | Step-by-step install contract an AI assistant follows to set up OAT on a fresh machine. Verifies each gate; stops on failure. |
| [`INSTALL_PROMPT.txt`](INSTALL_PROMPT.txt) | Single copy-pasteable prompt that points the assistant at `INSTALL.md`. Drop into Cursor / Claude Code to bootstrap an install. |

Human-facing docs live under [`../docs/`](../docs/) and the main
[`../README.md`](../README.md).
