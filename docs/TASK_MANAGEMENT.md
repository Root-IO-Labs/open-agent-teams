# Task Management in oat

## Overview

oat agents can leverage OAT Agent Runtime's built-in task management tools to track complex, multi-step work. This document explains how these tools work and when to use them.

## OAT Agent Runtime's Task Management Tools

OAT Agent Runtime provides four task management tools available to all agents:

### TaskCreate
Creates a new task in the task list.

**When to use:**
- Complex multi-step tasks requiring 3+ distinct steps
- Non-trivial operations that benefit from progress tracking
- User provides multiple tasks in a list
- You want to demonstrate thoroughness and organization

**When NOT to use:**
- Single, straightforward tasks
- Trivial operations (1-2 steps)
- Tasks completable in <3 steps
- Purely conversational or informational work

**Example:**
```
TaskCreate({
  subject: "Fix authentication bug in login flow",
  description: "Investigate and fix the issue where users can't log in with OAuth. Need to check middleware, token validation, and error handling.",
  activeForm: "Fixing authentication bug"
})
```

### TaskUpdate
Updates an existing task's status, owner, or details.

**Status workflow:** `pending` → `in_progress` → `completed`

**When to use:**
- Mark task as `in_progress` when starting work
- Mark task as `completed` when finished
- Update task details as requirements clarify
- Establish dependencies between tasks

**IMPORTANT:** Only mark tasks as `completed` when FULLY done. If you encounter errors, blockers, or partial completion, keep status as `in_progress`.

### TaskList
Lists all tasks with their current status, owner, and blockedBy dependencies.

**When to use:**
- Check what tasks are available to work on
- See overall progress on a project
- Find tasks that are blocked
- After completing a task, to find next work

### TaskGet
Retrieves full details of a specific task by ID.

**When to use:**
- Before starting work on an assigned task
- To understand task dependencies
- To get complete requirements and context

## Task Management vs Task Tool

**Task Management (TaskCreate/Update/List/Get):**
- Tracks progress on multi-step work within a single agent session
- Creates todo-style checklists visible to users
- Helps organize complex workflows
- Persists within the conversation context

**Task Tool (spawning sub-agents):**
- Delegates work to parallel sub-agents
- Enables concurrent execution of independent operations
- oat already does this at the orchestration level with workers!

## Best Practices for oat Agents

### For Worker Agents

**Use task management when:**
- Your assigned task has multiple logical steps (e.g., "Implement authentication: add middleware, update routes, write tests")
- You want to show progress on a complex feature
- The user asks you to track progress explicitly

**Don't overuse:**
- For simple bug fixes or single-file changes
- When you're just doing research/exploration
- For trivial operations

**Example workflow:**
```bash
# Starting a complex task
TaskCreate({
  subject: "Add user authentication endpoint",
  description: "Create /api/auth endpoint with JWT validation, rate limiting, and tests",
  activeForm: "Adding authentication endpoint"
})

# Start work
TaskUpdate({ taskId: "1", status: "in_progress" })

# ... do the work ...

# Complete when done
TaskUpdate({ taskId: "1", status: "completed" })
```

### For Supervisor Agent

**Use task management for:**
- Tracking multiple workers' overall progress
- Coordinating complex multi-worker efforts
- Breaking down large features into assignable chunks

**Pattern for supervision:**
1. Create high-level tasks for major work items
2. Assign tasks to workers (use task metadata to track which worker owns what)
3. Update task status as workers report completion
4. Use TaskList to monitor overall progress

### For Merge Queue Agent

**Use task management for:**
- Tracking PRs through the merge process
- Managing multiple PR reviews/merges concurrently
- Organizing complex merge conflict resolutions

## Task Management and PR Creation

**IMPORTANT:** Task management is for tracking work, NOT for delaying PRs.

- Create tasks to organize your work into logical blocks
- When a block (task) is complete and tests pass, create a PR immediately
- Don't wait for all tasks to be complete before creating PRs
- Each completed task should generally result in a focused PR

**Good pattern:**
```
Task 1: "Add validation function" → Complete → Create PR #1
Task 2: "Wire validation into API" → Complete → Create PR #2
Task 3: "Add error handling" → Complete → Create PR #3
```

**Bad pattern:**
```
Task 1: "Complete validation system"
  - Subtask: Add function
  - Subtask: Wire into API
  - Subtask: Add error handling
  → Wait for ALL to complete → Create massive PR
```

## Checking if Task Management is Available

oat automatically detects task management capabilities during daemon startup. Agents can assume these tools are available if running OAT Agent Runtime v2.0+.

To check manually:
```bash
oat diagnostics --json | jq '.capabilities.task_management'
```

## Related Documentation

- [agent Agent SDK - Todo Tracking](https://platform.agent.com/docs/en/agent-sdk/todo-tracking) - Official documentation
- [AGENTS.md](AGENTS.md) - oat agent architecture
- [AGENTS.md](AGENTS.md) - Development guide for oat itself
