# OAT AI-First Install Contract

Audience: an AI coding assistant (Cursor, Claude Code, Codex, etc.) running
in a user's terminal with shell access. The user pasted the prompt from
[`INSTALL_PROMPT.txt`](INSTALL_PROMPT.txt). Your job is to install OAT
end-to-end, verify each gate, and stop on the first real failure.

This document is intentionally script-with-judgment, not a wizard. Run
each step, verify the gate, surface failures with the exact command and
last ~30 lines of output. Do not silently skip steps. Do not invent
behavior the user did not approve.

Provider tokens are assumed to already be present in the user's
environment (e.g. `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GOOGLE_API_KEY`,
`OPENROUTER_API_KEY`, etc.). If none are set, ask the user which provider
they want and request the key once.

---

## Step 1 — Detect environment

Run and record:

```bash
uname -s            # Darwin | Linux
uname -m            # arm64 | x86_64
echo "$SHELL"       # zsh | bash | fish
command -v brew     # macOS package manager (may be absent on Linux)
```

Use the results to pick install commands in step 2. Do **not** ask the
user for things you can derive from the system.

## Step 2 — Check and install prerequisites

| Tool | Min version | Detect | Install (macOS) | Install (Linux) |
|---|---|---|---|---|
| Go | 1.25 | `go version` | `brew install go` | `sudo apt-get install -y golang-go` (verify version) or download from https://go.dev/dl/ |
| Python | 3.11 | `python3 --version` | `brew install python@3.11` | `sudo apt-get install -y python3.11 python3.11-venv` |
| uv | any | `uv --version` | `curl -LsSf https://astral.sh/uv/install.sh \| sh` | same |
| git | any | `git --version` | usually present | `sudo apt-get install -y git` |
| gh | any | `gh --version` | `brew install gh` | `sudo apt-get install -y gh` or https://cli.github.com |
| jq | any | `jq --version` | `brew install jq` | `sudo apt-get install -y jq` |

Confirm with the user before any install that requires sudo or
`curl | sh`. If Go is below 1.25, **stop** and ask the user to upgrade.

Gate: every tool resolves via `command -v` and meets the minimum version.

## Step 3 — Authenticate with GitHub

```bash
gh auth status
```

If not logged in, instruct the user: "Run `gh auth login` in this
terminal, then tell me to continue." Wait — don't try to drive an
interactive flow yourself, it will block in a non-TTY subprocess.

Gate: `gh auth status` reports "Logged in to github.com".

## Step 4 — Clone the repo

Default location: `~/src/open-agent-teams`. If the directory exists and
is non-empty, confirm with the user before proceeding.

```bash
mkdir -p ~/src
git clone https://github.com/Root-IO-Labs/open-agent-teams.git ~/src/open-agent-teams
cd ~/src/open-agent-teams
```

Gate: `~/src/open-agent-teams/scripts/install.sh` exists.

## Step 5 — Pick a branch

Default to `main`. The user may name another branch explicitly.

```bash
git branch --show-current   # gate — should be `main`
```

## Step 6 — Run the installer

```bash
./scripts/install.sh
```

This builds `oat` and `oat-agent` into `$(go env GOPATH)/bin`, symlinks
the agent runtime, and creates the Python venv with the common provider
packages. Expect ~2-5 min depending on network.

If the installer errors, show the user the last ~30 lines of output and
**stop**.

Gate: installer prints "OAT installed successfully" and both
`$(go env GOPATH)/bin/oat` and `$(go env GOPATH)/bin/oat-agent` exist.

## Step 7 — Ensure `$GOPATH/bin` is on PATH

```bash
GOBIN="$(go env GOPATH)/bin"
echo "$PATH" | tr ':' '\n' | grep -Fx "$GOBIN"
```

If absent, append to the user's rc file (zsh → `~/.zshrc`,
bash → `~/.bashrc`) **after** confirming with the user:

```bash
echo 'export PATH="$PATH:$(go env GOPATH)/bin"' >> ~/.zshrc   # or .bashrc
```

Tell the user to `source` the rc file or open a new terminal before the
next step. For this session only, you may also export inline:
`export PATH="$PATH:$GOBIN"`.

Gate: `command -v oat` resolves.

## Step 8 — Provider API key

If any of `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GOOGLE_API_KEY`,
`DEEPSEEK_API_KEY`, `OPENROUTER_API_KEY` is already exported, proceed.
Otherwise ask the user which provider they want and ask for the key.
Treat it as sensitive — do not echo or log beyond what's necessary.

Create `~/.oat/.env` if it doesn't exist, then append the key only if
it isn't already present (do not overwrite an existing line):

```bash
mkdir -p ~/.oat
touch ~/.oat/.env
grep -q '^ANTHROPIC_API_KEY=' ~/.oat/.env \
  || printf 'ANTHROPIC_API_KEY=%s\n' "$KEY" >> ~/.oat/.env
```

Provider → env var map:

| Provider | Env var |
|---|---|
| anthropic | `ANTHROPIC_API_KEY` |
| openai | `OPENAI_API_KEY` |
| google_genai | `GOOGLE_API_KEY` |
| deepseek | `DEEPSEEK_API_KEY` |
| openrouter | `OPENROUTER_API_KEY` |
| ollama | (none — ensure `ollama serve` is running and a model is pulled) |

Gate: `grep -E '^[A-Z].*_API_KEY=.+' ~/.oat/.env` returns at least one
line with a non-empty value, OR the env var is exported in the current
shell.

## Step 9 — Start the daemon

```bash
oat start
oat daemon status
```

Gate: `oat daemon status` shows the daemon running.

## Step 10 — Verify models

```bash
oat model list
```

Expected: at least one entry. If the list is empty, the model-profile
directory wasn't populated by the installer for this branch — check
`ls ~/.oat/model-profiles/ 2>/dev/null` and report. Do **not** try to
hand-write profile YAML; tell the user and stop.

Gate: at least one model row shows `status=known` and `worker=true`.

## Step 11 — Optional end-to-end verification

Ask the user: "Want me to initialize a test repo to verify the install?
I'll need a GitHub URL you own. Otherwise the install is complete."

If yes:

```bash
oat init <github-url> --model <chosen-model>
oat repo use <repo-name>
oat worker create "Add a file named HELLO.md containing 'hello from oat'"
oat worker list
```

Wait ~5 minutes, re-run `oat worker list`. If the worker reaches
`dormant` or `complete`, the install is verified end-to-end.

If no, tell the user the install is complete and how to verify later:

```
oat init <github-url> --model <model>
oat worker create "<task>"
```

## Step 12 — Final report

Tell the user, in this order:

1. What was installed (binaries, venv, profiles, .env).
2. What is running now (daemon yes/no, test repo if any).
3. The next command they should run themselves (`oat ui` or
   `oat worker create …`).
4. Anything skipped or partial — be honest, do not exaggerate.

---

## Recovery / teardown

If anything is wedged:

```bash
oat stop-all --clean --yes        # kills daemon, wipes state, worktrees, messages
rm -rf ~/.oat                     # last resort — also wipes .env
```

After teardown, re-run `./scripts/install.sh` from the repo root.

## Known failure modes

- `uv sync` picks the wrong Python: `UV_PYTHON=python3.11 ./scripts/install.sh`.
- `oat model list` empty after a successful install: report it — the
  installer should have populated `~/.oat/model-profiles/`. Do not
  hand-roll profile YAML; let the user file an issue.
- Daemon won't start / stale socket: `oat stop-all --clean --yes`, then `oat start`.
- Model auto-detect picks the wrong provider when multiple keys are set:
  always pass `--model` explicitly to `oat init` and `oat worker create`.
