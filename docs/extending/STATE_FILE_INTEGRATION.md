# State File Integration (Read-Only)

<!-- state-struct: State repos current_repo -->
<!-- state-struct: Repository github_url session_name agents task_history merge_queue_config pr_shepherd_config fork_config target_branch idle_mode model allowed_worker_models workspace_stuck_detection -->
<!-- state-struct: Agent type worktree_path window_name session_id pid task summary failure_reason created_at last_nudge last_nudge_hash nudge_skip_count ready_for_cleanup ready_for_cleanup_at issue_number issue_url nudge_count nudge_reset_used last_branch_sha model routing_source base_sha input_tokens output_tokens total_tokens cache_read_tokens cache_creation_tokens last_token_update max_tokens waiting_for_pr waiting_for_pr_since waiting_for_verification waiting_for_verification_since pr_number verification_agent verification_status verified_commit_sha verification_at verification_reason last_pr_comment_id last_wake_reason woken_for_merged_pr_at last_nudge_tier suppressed_nudge_count -->
<!-- state-struct: TaskHistoryEntry name task branch pr_url pr_number status summary failure_reason model created_at completed_at input_tokens output_tokens total_tokens -->
<!-- state-struct: MergeQueueConfig enabled track_mode -->
<!-- state-struct: PRShepherdConfig enabled track_mode -->
<!-- state-struct: ForkConfig is_fork upstream_url upstream_owner upstream_repo force_fork_mode -->

The daemon persists state to `~/.oat/state.json` and writes it atomically. This file is safe for external tools to **read only**. Write access belongs to the daemon.

## Schema (from `internal/state/state.go`)
```json
{
  "repos": {
    "<repo-name>": { /* Repository object */ }
  },
  "current_repo": "my-repo",  // Optional: default repository
  "hooks": { /* HookConfig object */ }
}
```

### Repository Object

```json
{
  "github_url": "https://github.com/user/repo",
  "session_name": "oat-my-repo",
  "agents": {
    "<agent-name>": { /* Agent object */ }
  },
  "task_history": [ /* TaskHistoryEntry objects */ ],
  "merge_queue_config": { /* MergeQueueConfig object */ },
  "pr_shepherd_config": { /* PRShepherdConfig object */ },
  "fork_config": { /* ForkConfig object */ },
  "target_branch": "main",
  "allowed_worker_models": ["anthropic:claude-sonnet-4-6", "openrouter:deepseek/deepseek-v3.2:nitro"]
}
```

### Agent Object

```json
{
  "type": "worker",                    // "supervisor" | "worker" | "merge-queue" | "workspace" | "review" | "verification" | "pr-shepherd" | "generic-persistent"
  "worktree_path": "/path/to/worktree",
  "window_name": "0",                  // Agent name within backend session
  "session_id": "agent-session-id",
  "pid": 12345,                        // Process ID (0 if not running)
  "task": "Implement feature X",       // Only for workers
  "summary": "Added auth module",      // Only for workers (completion summary)
  "failure_reason": "Tests failed",    // Only for workers (if task failed)
  "created_at": "2024-01-15T10:30:00Z",
  "last_nudge": "2024-01-15T10:35:00Z",
  "ready_for_cleanup": false,          // Only for workers (signals completion)
  "ready_for_cleanup_at": "...",       // When marked for cleanup (grace period)
  "issue_number": "42",               // GitHub issue number (workers only)
  "issue_url": "...",                  // Optional issue URL (workers only)
  "nudge_count": 0,                   // Nudges since last git activity
  "nudge_reset_used": false,          // One-time supervisor reset flag
  "last_branch_sha": "abc123",        // Last known commit SHA on agent branch
  "model": "claude-sonnet-4-6",       // LLM model override (agent-level)
  "input_tokens": 0,                  // Cumulative input tokens
  "output_tokens": 0,                 // Cumulative output tokens
  "total_tokens": 0,                  // input + output
  "cache_read_tokens": 0,             // Tokens served from prompt cache
  "cache_creation_tokens": 0,        // Tokens written to prompt cache (first call)
  "last_token_update": "...",         // When token counts last updated
  "max_tokens": 0,                    // Token budget (0 = unlimited)
  "waiting_for_pr": false,            // Worker dormant waiting for PR resolution
  "waiting_for_pr_since": "...",      // When PR dormancy started
  "waiting_for_verification": false,  // Worker dormant waiting for verification verdict
  "waiting_for_verification_since": "...", // When verification dormancy started
  "pr_number": 0,                     // PR number being monitored
  "last_pr_comment_id": 0,            // Last seen PR comment ID
  "last_wake_reason": "",             // Wake message on dormancy transition
  "last_nudge_tier": 0,              // Tier of last nudge sent (for de-duplication)
  "suppressed_nudge_count": 0,       // Consecutive suppressed nudges at same tier
  "verification_agent": "",           // Name of linked verification agent (workers only)
  "verification_status": "",          // "" | "pending" | "approved" | "rejected" (workers only)
  "verified_commit_sha": "",          // Commit SHA the verdict applies to (workers only)
  "verification_at": "...",           // When verdict was set (workers only)
  "verification_reason": "",          // Verdict reason text (workers only)
  "base_sha": "",                     // Pinned remote default-branch SHA for the verifier diff (workers only; set at request-review)
  "routing_source": "",               // Tag identifying which routing rule chose this agent's model
  "last_nudge_hash": "",              // Hash of last nudge body for deduplication
  "nudge_skip_count": 0               // Consecutive nudges suppressed by hash dedup
}
```

**Agent Types:**
- `supervisor`: Main orchestrator for the repository
- `merge-queue`: Monitors and merges approved PRs
- `worker`: Executes specific tasks
- `workspace`: Interactive workspace agent
- `review`: Reviews a specific PR
- `verification`: Independent pre-PR reviewer (ephemeral, spawned by `oat worker request-review`)
- `pr-shepherd`: Monitors PRs in fork mode
- `generic-persistent`: Custom persistent agents

### TaskHistoryEntry Object

```json
{
  "name": "clever-fox",                // Worker name
  "task": "Add user authentication",   // Task description
  "branch": "oat/clever-fox",  // Git branch
  "pr_url": "https://github.com/user/repo/pull/42",
  "pr_number": 42,
  "status": "merged",                  // See status values below
  "summary": "Implemented JWT-based auth with refresh tokens",
  "failure_reason": "",                // Populated if status is "failed"
  "model": "anthropic:claude-sonnet-4-6",
  "created_at": "2024-01-15T10:00:00Z",
  "completed_at": "2024-01-15T11:30:00Z",
  "input_tokens": 120000,
  "output_tokens": 8000,
  "total_tokens": 128000
}
```

**Status Values:**
- `open`: PR created, not yet merged or closed
- `merged`: PR was merged successfully
- `closed`: PR was closed without merging
- `no-pr`: Task completed but no PR was created
- `failed`: Task failed (see `failure_reason`)
- `unknown`: Status couldn't be determined

### MergeQueueConfig Object

```json
{
  "enabled": true,                     // Whether merge-queue agent runs
  "track_mode": "all"                  // "all" | "author" | "assigned"
}
```

**Track Modes:**
- `all`: Monitor all PRs in the repository
- `author`: Only PRs where oat user is the author
- `assigned`: Only PRs where oat user is assigned

### PRShepherdConfig Object

```json
{
  "enabled": true,                     // Whether pr-shepherd agent runs
  "track_mode": "author"               // "all" | "author" | "assigned"
}
```

### ForkConfig Object

```json
{
  "is_fork": true,
  "upstream_url": "https://github.com/upstream/repo",
  "upstream_owner": "upstream",
  "upstream_repo": "repo",
  "force_fork_mode": false
}
```

### HookConfig Object

```json
{
  "on_event": "/usr/local/bin/notify.sh",          // Catch-all hook
  "on_pr_created": "/usr/local/bin/slack-pr.sh",
  "on_agent_idle": "",
  "on_merge_complete": "",
  "on_agent_started": "",
  "on_agent_stopped": "",
  "on_task_assigned": "",
  "on_ci_failed": "/usr/local/bin/alert-ci.sh",
  "on_worker_stuck": "",
  "on_message_sent": ""
}
```

## Example State File

```json
{
  "repos": {
    "my-app": {
      "github_url": "https://github.com/user/my-app",
      "session_name": "oat-my-app",
      "agents": {
        "supervisor": {
          "type": "supervisor",
          "pid": 12345,
          "created_at": "2025-01-01T00:00:00Z",
          "last_nudge": "2025-01-01T00:00:00Z",
          "ready_for_cleanup": false
        }
      },
      "task_history": [
        {
          "name": "clever-fox",
          "task": "Add auth",
          "branch": "work/clever-fox",
          "pr_url": "https://github.com/user/my-app/pull/42",
          "pr_number": 42,
          "status": "merged",
          "created_at": "2025-01-01T00:00:00Z",
          "completed_at": "2025-01-02T00:00:00Z"
        }
      ],
      "merge_queue_config": {
        "enabled": true,
        "track_mode": "all"
      },
      "pr_shepherd_config": {
        "enabled": true,
        "track_mode": "author"
      },
      "fork_config": {
        "is_fork": true,
        "upstream_url": "https://github.com/original/my-app",
        "upstream_owner": "original",
        "upstream_repo": "my-app",
        "force_fork_mode": false
      },
      "target_branch": "main",
      "allowed_worker_models": ["anthropic:claude-sonnet-4-6"]
    }
  },
  "current_repo": "my-app"
}
```

## Reading the state file

### Go
```go
package main

import (
    "encoding/json"
    "fmt"
    "os"

    "github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

func main() {
    data, err := os.ReadFile("/home/user/.oat/state.json")
    if err != nil {
        panic(err)
    }

    var st state.State
    if err := json.Unmarshal(data, &st); err != nil {
        panic(err)
    }

    for name := range st.Repos {
        fmt.Println("repo", name)
    }
}
```

### Python
```python
import json
from pathlib import Path

state_path = Path.home() / ".oat" / "state.json"
state = json.loads(state_path.read_text())
for repo, data in state.get("repos", {}).items():
    print("repo", repo, "agents", list(data.get("agents", {}).keys()))
```

## Updating this doc
- Keep the `state-struct` markers above in sync with `internal/state/state.go`.
- Do **not** add fields here unless they exist in the structs.
- Run `go run ./cmd/verify-docs` after schema changes; CI will block if docs drift.