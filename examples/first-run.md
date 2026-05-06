# First-run walkthrough

Goal: from a clean machine, get OAT to open and merge a real pull request on a toy repository you control, in about ten minutes.

This is the shortest "I understand what this thing does" loop. Once you've been through it once, everything else in the project (the dashboard, the benchmark, the agent customization) is a variation on this same flow.

## What you'll end up with

- A local OAT install (`oat`, `oat-agent`, and the daemon).
- A GitHub repo of your own, registered with OAT.
- One supervisor and one merge-queue agent running in the background.
- One worker that took a task, wrote code, opened a PR, and watched the merge-queue merge it.

## Prerequisites

Run `oat doctor` after install — it checks these for you. If you don't have `oat` yet, the short list is:

- Go 1.24.2+
- Python 3.11+
- `uv` ([install](https://astral.sh/uv))
- `gh` authenticated (`gh auth login`)
- One provider API key exported in your shell (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `OPENROUTER_API_KEY`, etc.)

## Step 1 — Install

```bash
git clone https://github.com/Root-IO-Labs/open-agent-teams.git
cd open-agent-teams
./scripts/install.sh
```

Sanity check:

```bash
oat doctor
```

You want all seven checks green. If one fails, `oat doctor` tells you exactly what to fix. Don't move on until it's clean.

## Step 2 — Pick a toy repo

You need a GitHub repo you own (or have push access to) where OAT can open a PR. Two easy options:

**Option A: fork something small.** Any public Python CLI with a `tests/` directory and one obvious small bug is fine. Fork it into your account.

**Option B: start empty.**

```bash
gh repo create my-oat-test --public --clone
cd my-oat-test

cat > README.md <<'EOF'
# my-oat-test

A scratch repo for trying OAT. Safe to nuke.
EOF

cat > hello.py <<'EOF'
def greet(name: str) -> str:
    return "Hi " + name + "!"

if __name__ == "__main__":
    print(greet("world"))
EOF

cat > test_hello.py <<'EOF'
from hello import greet

def test_greet_says_hello():
    # The function should say "Hello", not "Hi".
    assert greet("world") == "Hello, world!"
EOF

git add -A && git commit -m "initial: hello world with a deliberately failing test"
git push -u origin main
```

The test fails on purpose. That's the task: make the test pass.

File an issue so the worker has something concrete to take:

```bash
gh issue create \
  --title "test_greet_says_hello fails: greet() returns the wrong string" \
  --body "The test expects 'Hello, world!' but greet() returns 'Hi world!'. Fix greet() so the test passes. Keep the function signature."
```

Note the issue number (probably `#1`). You'll pass it to the worker in step 4.

## Step 3 — Start the daemon and register the repo

```bash
oat start                                                # start the background daemon
oat init https://github.com/<your-user>/my-oat-test      # register the repo
```

What just happened:

1. `oat start` launched the daemon (a background Go process that manages agents, routes messages, and monitors PRs).
2. `oat init` cloned the repo into `~/.oat/repos/my-oat-test/` and started a **supervisor** (coordinator) and a **merge-queue** (the thing that watches CI and merges green PRs).
3. State lives in `~/.oat/state.json`.

Confirm:

```bash
oat status
# my-oat-test
#   supervisor (active)
#   merge-queue (active)
```

## Step 4 — Create a worker

```bash
oat worker create "Fix greet() so test_greet_says_hello passes" \
  --repo my-oat-test \
  --issue 1
```

That spawns one worker with its own git worktree (`~/.oat/wts/my-oat-test/<worker-name>/`) on branch `work/<worker-name>`. The worker inherits your API key, reads the issue, edits `hello.py`, runs the test, and pushes a PR.

Watch it live:

```bash
oat ui
```

You should see, in order:

1. The worker appears in the sidebar.
2. Its activity feed shows it reading files, running `pytest`, editing `hello.py`.
3. It opens a PR via `gh pr create` (labeled `oat`).
4. CI runs; the merge-queue picks it up once CI is green.
5. The merge-queue merges the PR.
6. The worker calls `oat agent complete` and disappears.

Press `q` to leave the dashboard. Agents keep running.

## Step 5 — Verify the outcome

```bash
gh pr list --repo <your-user>/my-oat-test --state merged
#  #2  Fix greet() so test passes  work/<worker-name>  MERGED
```

Check out the merged main branch:

```bash
cd ~/.oat/repos/my-oat-test
git pull
cat hello.py
# def greet(name: str) -> str:
#     return "Hello, " + name + "!"
```

The tests pass. Your scratch repo is now green.

## Cleanup (when you're done experimenting)

```bash
oat stop                    # stops the daemon and all agents
rm -rf ~/.oat/              # full local reset; keeps your GitHub repos untouched
gh repo delete <user>/my-oat-test --yes   # optional, nuke the GitHub scratch repo
```

## What to try next

- `oat worker create` with a more interesting task on your actual project.
- `oat init --model <model>` to try a different provider (see [docs/SUPPORTED_LLM_PROVIDERS.md](../docs/SUPPORTED_LLM_PROVIDERS.md)).
- Run the built-in robotic-barista benchmark (`cd benchmarks && ./scripts/run.sh --model anthropic:claude-sonnet-4-6 --repo my-bench`).
- Customize worker behaviour by dropping a prompt file into `.oat/agents/worker.md` — see [docs/AGENTS.md](../docs/AGENTS.md).

If something doesn't work, `oat doctor` first, then [`docs/QUICKSTART.md`](../docs/QUICKSTART.md#troubleshooting), then open an issue.
