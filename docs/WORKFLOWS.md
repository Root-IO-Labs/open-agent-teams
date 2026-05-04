# Workflows

How to actually use this thing.

## Watching Agents

Attach to an agent to see what it's doing:

```bash
oat attach supervisor --repo myrepo
```

```
╭─────────────────────────────────────────────────────────────────────────╮
│ I'll check on the current workers and see if anyone needs help.        │
│                                                                         │
│ > oat worker list                                               │
│ Workers (2):                                                            │
│   - swift-eagle: working on issue #44                                   │
│   - calm-deer: working on issue #24                                     │
│                                                                         │
│ Both workers are making progress. swift-eagle just pushed a commit.    │
╰─────────────────────────────────────────────────────────────────────────╯
```

Use separate terminals to monitor multiple agents. Press `Ctrl-C` to stop watching (agents keep running).

## Spawning Workers

You're in your workspace. You want stuff done. Spawn workers.

```
┌─────────────────────────────────────────────────────────────────────────────┐
│ You:                                                                        │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│  > Let's tackle issues #44 and #45 in parallel                              │
│                                                                             │
│  ╭─────────────────────────────────────────────────────────────────────────╮│
│  │ On it.                                                                  ││
│  │                                                                         ││
│  │ > oat worker create "Implement rich list commands per issue #44"││
│  │ ✓ Worker created: swift-eagle (branch: work/swift-eagle)                ││
│  │                                                                         ││
│  │ > oat worker create "Improve error messages per issue #45"      ││
│  │ ✓ Worker created: calm-deer (branch: work/calm-deer)                    ││
│  │                                                                         ││
│  │ Two workers deployed. Check on them with:                               ││
│  │   oat worker list                                               ││
│  │   oat agent attach swift-eagle                                  ││
│  ╰─────────────────────────────────────────────────────────────────────────╯│
│                                                                             │
│  > Cool. Going to lunch.                                                    │
│                                                                             │
│  ╭─────────────────────────────────────────────────────────────────────────╮│
│  │ Enjoy. I'll keep an eye on things. Workers will keep running.           ││
│  ╰─────────────────────────────────────────────────────────────────────────╯│
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

Come back later:

```
│  > Back. What happened?                                                     │
│                                                                             │
│  ╭─────────────────────────────────────────────────────────────────────────╮│
│  │ Welcome back.                                                           ││
│  │                                                                         ││
│  │ ✓ swift-eagle completed - PR #47 for rich list commands                 ││
│  │ ✓ calm-deer completed - PR #48 for error messages                       ││
│  │                                                                         ││
│  │ Both PRs passing CI. Merge queue is on it.                              ││
│  ╰─────────────────────────────────────────────────────────────────────────╯│
```

## Watching the Supervisor

The supervisor is air traffic control. Watch it coordinate:

```bash
oat agent attach supervisor --read-only
```

```
│  ╭─────────────────────────────────────────────────────────────────────────╮│
│  │ [Periodic check - 14:32]                                                ││
│  │                                                                         ││
│  │ Checking agent status...                                                ││
│  │                                                                         ││
│  │ Agents:                                                                 ││
│  │   supervisor: healthy (me)                                              ││
│  │   merge-queue: healthy, monitoring 2 PRs                                ││
│  │   workspace: healthy, user attached                                     ││
│  │   swift-eagle: healthy, working on #44                                  ││
│  │   calm-deer: stuck on test failure                                      ││
│  │                                                                         ││
│  │ Sending help to calm-deer...                                            ││
│  │                                                                         ││
│  │ > oat message send calm-deer "Stuck on tests? The flaky test    ││
│  │   in auth_test.go has timing issues. Try mocking the clock."            ││
│  ╰─────────────────────────────────────────────────────────────────────────╯│
```

## Watching the Merge Queue

The merge queue is the bouncer. CI passes? You're in.

```bash
oat agent attach merge-queue --read-only
```

```
│  ╭─────────────────────────────────────────────────────────────────────────╮│
│  │ [PR Check - 14:45]                                                      ││
│  │                                                                         ││
│  │ > gh pr list --author @me                                               ││
│  │ #47  Add rich list commands      work/swift-eagle                       ││
│  │ #48  Improve error messages      work/calm-deer                         ││
│  │                                                                         ││
│  │ Checking #47...                                                         ││
│  │ > gh pr checks 47                                                       ││
│  │ ✓ All checks passed                                                     ││
│  │                                                                         ││
│  │ Merging.                                                                ││
│  │ > gh pr merge 47 --squash --delete-branch --auto                        ││
│  │ ✓ Merged #47 into main                                                  ││
│  │                                                                         ││
│  │ > oat message send supervisor "Merged PR #47"                   ││
│  ╰─────────────────────────────────────────────────────────────────────────╯│
```

CI fails? Merge queue spawns a fixer:

```
│  │ Checking #48...                                                         ││
│  │ ✗ Tests failed: 2 failures in error_test.go                             ││
│  │                                                                         ││
│  │ Spawning fixup worker...                                                ││
│  │ > oat worker create "Fix test failures in PR #48" \             ││
│  │     --branch work/calm-deer                                             ││
│  │ ✓ Worker created: quick-fox                                             ││
│  │                                                                         ││
│  │ I'll check back after quick-fox pushes.                                 ││
```

## Iterating on a PR

Got review comments? Spawn a worker to fix them:

```bash
oat worker create "Fix review comments on PR #48" \
  --branch origin/work/calm-deer \
  --push-to work/calm-deer
```

Worker pushes to the existing branch. Same PR. No mess.

## Agent Stuck?

```bash
# Watch what it's doing
oat agent attach <name> --read-only

# Check its messages
oat message list

# Watch daemon logs
tail -f ~/.oat/daemon.log
```

## State Broken?

```bash
# Quick fix
oat repair

# See what's wrong
oat cleanup --dry-run

# Nuke the cruft
oat cleanup
```

---

For custom agents, model selection, fork mode, dormancy, and verification, see [Advanced Usage](ADVANCED_USAGE.md).
