# Planner Agent UI — Complete Interaction Map

**Generated:** 2026-05-14  
**Branch:** feature/planner-agent  
**Scope:** Every action, data flow, state transition, and gap in the planner TUI.

---

## 1. Architecture Overview

```
┌─────────────────────────────────────────────────────────────────────┐
│                         app.go (TUI host)                           │
│                                                                     │
│  Input bar ──► handleKey() ──► planner.HandleAppInput(text)         │
│                    │                      │                         │
│                    │                      ▼                         │
│                    │             handleContextualInput()             │
│                    │             detectContextualIntent()            │
│                    │                      │                         │
│                    │                      ▼                         │
│                    │             sendToPlanner(text)                 │
│                    │              → daemon send_agent_input          │
│                    │                                                 │
│  streamBatchMsg ───────────────► planner.ReceiveOutput()            │
│  (planner agent                          │                          │
│   PTY output)                     drainBuffer()                     │
│                                          │                          │
│                                   applyPlannerResponse()            │
│                                   (state update only)               │
│                                          │                          │
│  renderContentForViewport("planner") ◄───┘                          │
│    └─ planner.RenderEmbeddedContent()  ← thin state strip           │
│    └─ renderer.RenderLines()           ← raw PTY output             │
│                                                                     │
│  Keyboard shortcuts ──► planner.HandleAppShortcut()                 │
└─────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────┐
│                     Planner Agent (PTY process)                     │
│                                                                     │
│  Receives: raw text via send_agent_input                            │
│  Responds: Claude Code terminal output including JSON responses     │
│  Prompt:   internal/prompts/planner.md (spec-first, JSON-only)      │
│  Model:    anthropic:claude-sonnet-4-6 (set at oat init)            │
└─────────────────────────────────────────────────────────────────────┘
```

---

## 2. User Input — Complete Path

### When user types text and presses Enter

| Step | Location | Code | What happens |
|------|----------|------|-------------|
| 1 | app.go:734 | `tea.KeyEnter` in `handleKey()` | Input bar text captured |
| 2 | app.go:744 | `if a.activeAgent == "planner"` | Planner-specific branch taken |
| 3 | app.go:745 | `a.planner.HandleAppInput(text)` | Routed to PlannerView |
| 4 | app.go:749 | `a.autoScroll["planner"] = true` | Viewport auto-scrolls |
| 5 | app.go:750 | `a.updateViewport()` | Refresh display immediately |
| 6 | planner_view.go:1583 | Requirement check | If nil: creates Requirement{}, sets StateRefiningRequirement |
| 7 | planner_view.go:1595 | Clarifying-turn counter | After 3 quiet turns: fires Socratic brainstorm |
| 8 | planner_view.go:1613 | `handleContextualInput(text)` | Intent detection routing |
| 9 | planner_view_enhancements.go:206 | `detectContextualIntent(text)` | Classify: Approval/Rejection/Clarification/Completion/Feedback |
| 10 | planner_view_enhancements.go:208-256 | Intent-based routing | See intent table below |
| 11 | planner_view.go:602 | `sendToPlanner(text)` | Build socket request |
| 12 | planner_view.go:612 | `client.Send("send_agent_input")` | → daemon → planner PTY |

### Intent Detection Routing (step 9-10)

| Detected Intent | Condition | Action |
|----------------|-----------|--------|
| `IntentApproval` | Gate is set (`p.currentGate != nil`) | `passGate()` → validate → `approvePlan()` |
| `IntentApproval` | In `StateReviewingPlan` with tasks | `approvePlan()` directly |
| `IntentApproval` | In `StateRefiningRequirement` | `advanceToNextPhase()` |
| `IntentCompletion` | In Defining or Refining state | `completeCurrentPhase()` → sends signal to planner |
| `IntentRejection` | In `StateReviewingPlan` | `rejectPlan()` → back to Refining |
| `IntentRejection` | Other states | Feedback: "please describe what needs to change" |
| `IntentClarification` | Any state | Falls through to `sendToPlanner(text)` |
| `IntentFeedback` (default) | Any state | `sendToPlanner(text)` |

### Intent Detection Rules (`detectContextualIntent`)

**Approval patterns:** "looks good", "yes", "approve", "let's go", "sounds good", "perfect", "great", "confirmed", "agree", "proceed", "lgtm", "ship it", "do it", "go ahead", "ok", "that works", "all good", "ready", "let's do it", "i approve", "approved", "continue", "next"  
*Exclusion: skipped if text contains "api" or "code"*

**Rejection patterns:** "no", "change", "update", "not quite", "wait", "stop", "hold on", "actually", "instead", "different", "wrong", "incorrect", "fix", "revise", "redo"  
*Exclusion: skipped if "don't", "not yet", or "looks" present*

**Completion signals:** "done", "finished", "complete", "ready", "that's it", "i'm done", "we're done", "all set", "that's all", "nothing more", "finalize", "wrap up", "end"

**Clarification:** text starts with "what", "how", "why", "when", "should", or contains "?"

---

## 3. Planner Output — Complete Path

### When planner agent responds

| Step | Location | Code | What happens |
|------|----------|------|-------------|
| 1 | daemon | PTY read | Planner agent output arrives |
| 2 | app.go:398-419 | `streamBatchMsg` handler | Lines stored in `outputContent["planner"]` |
| 3 | app.go:411 | `if msg.agent == "planner"` | Planner-specific forward |
| 4 | app.go:418 | `planner.ReceiveOutput(newLines, newTypes)` | Sent to PlannerView |
| 5 | planner_view.go:1273 | `p.thinking = false` | Thinking spinner stops |
| 6 | planner_view.go:1274-1287 | Line filtering | Skip: non-text types, ANSI/control chars |
| 7 | planner_view.go:1286 | `p.plannerBuffer += line` | Accumulate for JSON detection |
| 8 | planner_view.go:1288 | `p.drainBuffer()` | Try to extract JSON |

### drainBuffer() logic

```
For each call:
│
├─ Pass 1: Look for ```json ... ``` fence
│   ├─ Found + complete → unmarshal → applyPlannerResponse() → continue loop
│   ├─ Found + incomplete → hold buffer from fence start, RETURN (wait)
│   └─ Not found → Pass 2
│
├─ Pass 2: Look for bare {…} JSON object
│   ├─ Found complete object with "phase" field → applyPlannerResponse() → continue loop
│   ├─ Found object without "phase" field → discard object
│   └─ No complete object → Pass 3
│
└─ Pass 3: Hold or discard
    ├─ Buffer has " or { AND < 8KB → RETURN (wait for more batches)
    └─ Otherwise → clear buffer (raw output renders via standard viewport)
```

### applyPlannerResponse() state updates

| Field | What updates | Where stored |
|-------|-------------|-------------|
| `resp.Phase` | `p.state` | PlannerView.state |
| `resp.Requirement` | `p.requirement.*` | PlannerView.requirement |
| `resp.TestStrategy` | `p.testStrategy` | PlannerView.testStrategy |
| `resp.Tasks[]` | `p.tasks[]` | PlannerView.tasks |
| `resp.Action == "revise"` | `p.state`, `p.isLocked`, `p.currentGate` | Reset to refining |
| tasks in StateReviewingPlan | `p.currentGate` | Gate set for approval |

**Nothing is rendered by applyPlannerResponse.** Rendering happens via the standard viewport refresh triggered by the next Update cycle.

---

## 4. Rendering Pipeline

### What appears in the planner viewport

```
renderContentForViewport("planner")  [app.go:1767]
        │
        ├─ planner.RenderEmbeddedContent(width, 0)  [planner_view.go:1511]
        │       │
        │       ├─ If no requirement AND no tasks: returns ""
        │       └─ Otherwise returns:
        │               "  clarifying  A Python CLI calculator\n"
        │               "    5 tasks · 2 waves\n"
        │               "──────────────────────────────────────\n"
        │
        └─ renderer.RenderLines("planner", lines, lineTypes)  [app.go:1787]
                │
                └─ Standard line renderer: same as workspace, supervisor, etc.
                   Raw Claude Code terminal output, deduped, formatted
```

### What the standard line renderer shows

The planner agent outputs its full conversation: the JSON responses, its thinking, tool calls, etc. This is exactly what you'd see if you opened the planner agent directly. The JSON fence blocks (`` ```json ... ``` ``) show as formatted code blocks.

---

## 5. Keyboard Shortcuts (Planner Mode)

Handled at **app.go:535-539** → `planner.HandleAppShortcut()` → **planner_view.go:1620-1660**

| Key | Available when | Function called | What it does |
|-----|---------------|-----------------|-------------|
| `Ctrl+N` | Always | `startNewRequirement()` | Clears all state, resets to `StateDefiningRequirement` |
| `Ctrl+R` | Requirement exists | `refineRequirement()` | Sends "Please refine the current requirement further." to planner |
| `Ctrl+P` | Requirement exists | `stopAndPullPlan()` | Interrupts planner, sends STOP + JSON extraction message |
| `Ctrl+B` | Brainstorm themes remain | `conductSocraticDialogue()` | Sends next brainstorm theme (Tech Stack/Architecture/Testing/Deployment/Security/UX) |
| `Ctrl+A` | Always | `approvePlan()` | If tasks: lock + persist + dispatch to workspace. If no tasks: shows hint |
| `Ctrl+X` | `StateReviewingPlan` only | `rejectPlan()` | Back to `StateRefiningRequirement`, NO message sent to planner |
| `Esc` | Always (via app.go) | `mode = ViewWorkspace` | Exit planner view |

### stopAndPullPlan() full chain

```
Ctrl+P
  │
  ├─ client.Send("interrupt_agent", {repo, agent: "planner"})
  │   └─ Sends interrupt signal to stop current tool execution
  │
  └─ client.Send("send_agent_input", {message: "STOP all implementation...
                                      Output your complete current plan
                                      as a single JSON code block..."})
      └─ Returns plannerSentMsg{} → thinking=true
```

### approvePlan() full chain

```
Ctrl+A (or IntentApproval detected)
  │
  ├─ If len(p.tasks) == 0:
  │   └─ Adds system feedback: "No tasks to dispatch yet. Press ^p..."
  │       Returns nil
  │
  └─ If tasks exist:
      ├─ p.state = StatePlanLocked
      ├─ p.isLocked = true
      ├─ p.currentGate = nil
      ├─ persistPlan()
      │   └─ Saves to ~/.oat/plans/<repo>/plan-<id>.json via PlanStorage
      │       (best-effort, errors ignored)
      │
      └─ dispatchToWorkspace()
          ├─ buildWorkspaceHandoff() → formatted [PLANNER-APPROVED] message
          ├─ Try: client.Send("send_agent_input", {agent: "workspace", message})
          │   └─ Success → plannerDispatchedMsg{target: "workspace"}
          └─ Fallback: spawn Wave 1 workers via spawn_agent
              └─ plannerDispatchedMsg{target: "direct"}
```

---

## 6. State Machine

```
StateDefiningRequirement (0)  ─── initial state ───────────────────────────┐
  │ trigger: user enters first text                                         │
  ▼                                                                         │
StateRefiningRequirement (1) ◄──────────────────────────────────────────   │
  │ trigger: applyPlannerResponse phase="architecture"                   ^X │
  ▼                                                                         │
StateDecomposingTasks (2)                                                   │
  │ trigger: applyPlannerResponse phase="draft_plan" + tasks > 0           │
  ▼                                                                         │
StateReviewingPlan (3) ──────────────────────────────────────────────────► │
  │ trigger: ^A or IntentApproval + tasks populated               reject    │
  ▼                                                                         │
StatePlanLocked (4)                                                         │
  │ trigger: plannerDispatchedMsg received                                  │
  ▼                                                                         │
StateExecuting (5)                                                          │
                                                                            │
  ◄──────────────────────────────────────────────────── Ctrl+N (any state) ┘
```

### State → UI mapping

| State | `SummaryForList()` shows | Help bar shows | `renderMainContent` shows |
|-------|--------------------------|----------------|--------------------------|
| Defining | "waiting for requirement" + ● | "Enter: describe requirement" | No state strip (empty viewport) |
| Refining | "clarifying (vN)" + ● if thinking | "Enter: reply, ^p: extract plan, ^r: refine, ^b: brainstorm, ^n: restart" | State strip when requirement exists |
| Decomposing | "decomposing tasks" + ● | "Enter: reply, ^p: extract plan, ^n: restart" | State strip |
| Reviewing (no tasks) | "N tasks · N waves" | "^p: extract plan as JSON, ^x: reject, Enter: feedback" | State strip |
| Reviewing (tasks set) | "N tasks · N waves" | "^a: approve & dispatch, ^x: reject, ^p: re-extract, Enter: feedback" | State strip + compact task list |
| Locked | "plan locked · N tasks" | "^a: dispatch to workspace, Enter: feedback" | State strip + compact task list |
| Executing | "executing · N tasks · N waves" | "esc: back" | State strip + compact task list |

---

## 7. Phase Gates

Gates require explicit user approval before advancing. Currently only Gate 3 is auto-activated.

| Gate | ID | Triggered when | Validation | What happens on pass |
|------|-----|---------------|-----------|---------------------|
| Gate 1 | `gate_1_requirements` | Manual only (initPhaseGates) | requirement.Refined != "" AND iteration > 0 | `advanceToNextPhase()` |
| Gate 2 | `gate_2_architecture` | Manual only | operational_spec != "" AND testStrategy != nil | `advanceToNextPhase()` |
| Gate 3 | `gate_3_plan` | Auto: when plan lands in StateReviewingPlan | len(tasks) > 0 AND getMaxWave() > 0 | `approvePlan()` → dispatch |

**Gap:** Gates 1 and 2 are defined but never auto-set. They exist in `initPhaseGates()` but nothing calls that method to set `p.currentGate` for gates 1 or 2. Only Gate 3 is set in `applyPlannerResponse()`.

---

## 8. What Actually Works vs. What Doesn't

### ✅ Working

| Feature | Where | Status |
|---------|-------|--------|
| Input → daemon → planner agent | app.go:744, planner_view.go:602 | ✓ |
| Output → standard viewport | app.go:411-418, 1772-1793 | ✓ |
| JSON parsing → state update | drainBuffer, applyPlannerResponse | ✓ |
| State strip (requirement + tasks) | RenderEmbeddedContent | ✓ |
| Ctrl+P stop + extract | stopAndPullPlan() | ✓ |
| Ctrl+A approve + dispatch | approvePlan() + dispatchToWorkspace() | ✓ |
| Workspace fallback | spawn_agent for Wave 1 tasks | ✓ |
| Plan persistence | persistPlan() → ~/.oat/plans/ | ✓ (silent on error) |
| Model selection | daemon model router, planner uses orchestrator-eligible model | ✓ |
| Workspace gets all completion events | daemon.go:2698+ | ✓ |
| Agent restart on crash | daemon restoreRepoAgents() fixed | ✓ |
| Sidebar shows planner summary + thinking | SummaryForList() called by app.go:1030 | ✓ |

### ❌ Broken / Not Working

| Feature | Root Cause | Impact |
|---------|-----------|--------|
| Raw JSON shows in viewport | Planner streams JSON without fences; drainBuffer discards it but viewport shows raw output | User sees `"phase": "clarifying",` in output |
| `[planner-tui]` prefix echoes back | Removed but still: send_agent_input echoes back user text to PTY | User text shows twice in viewport |
| Gates 1 + 2 never activate | `initPhaseGates()` defined but never called to SET p.currentGate | Users can't gate on requirements clarity or architecture approval |
| Intent detection false positives | "looks wrong" detects rejection; "api works" detects approval | Unpredictable state transitions on normal conversation |
| `completeCurrentPhase()` broken in StateDecomposing | Only handles Defining/Refining states | "done" doesn't work in all phases |
| Thinking spinner dies on first output | `ReceiveOutput` sets thinking=false unconditionally | Spinner disappears while planner is still streaming |
| RenderEmbeddedContent empty until first JSON | No requirement = empty strip | First response shows nothing in state area |
| Planner has no error feedback | plannerErrorMsg adds system feedback but not visible in viewport | Users don't see connection failures |

### 🔶 Dead Code (defined, never called in integrated mode)

| Function | File | Why dead |
|----------|------|---------|
| `View()` full render | planner_view.go:839 | app.go uses embedded mode only |
| `renderRequirement()` boxed | planner_view.go:947 | Replaced by compact strip |
| `renderTestStrategy()` boxed | planner_view.go:979 | Not called from renderMainContent |
| `renderTasks()` full | planner_view.go:1005 | Replaced by renderTasksCompact |
| `renderFeedback()` | planner_view.go:1109 | Not called in embedded mode |
| `EnhancePlannerView()` | planner_view_enhancements.go:48 | initializeEnhancements() used instead |
| `getDetailedStatus()` | planner_view_enhancements.go:486 | Never called |
| `BrainstormTheme.Validator` | planner_view_enhancements.go:92 | Never invoked |
| Gates 1 + 2 validation fns | planner_view_enhancements.go:304-351 | currentGate never set to these |
| `buildPlannerMessage()` prefix | planner_view.go:634 | Removed to fix echo |

---

## 9. Data Structures

### PlannerResponse (JSON from planner agent)

```go
type PlannerResponse struct {
    Phase        string              // "clarifying"|"architecture"|"draft_plan"|"ready_for_review"
    Message      string              // human-readable text
    Questions    []string            // clarifying questions
    Requirement  *PlannerRequirement // {title, original, refined, operational_spec}
    TestStrategy *TestStrategy       // {unit, integration, blackbox, gate_script}
    Tasks        []PlannerTask       // [{id, title, description, type, wave, dependencies, spec_reference, test_first, acceptance_criteria}]
    Action       string              // "advance_phase"|"dispatch_tasks"|"revise"|"clarify"|"none"
    PlanID       string              // "plan-<unix_timestamp>"
    SavePath     string              // "~/.oat/plans/plan-<id>/"
}
```

### Internal Task

```go
type Task struct {
    ID, Title, Description, Type string
    Dependencies, AcceptanceCriteria []string
    SpecReference string
    TestFirst     bool
    Wave          int        // execution wave number
    Status        TaskStatus // Pending/InProgress/Completed/Failed/Blocked
    AssignedTo    string     // worker agent name
}
```

### Workspace Handoff Format

```
[PLANNER-APPROVED] Plan ready for execution.

## Requirement
<requirement.Refined>

## Wave 1 — spawn immediately
### T1: <title>
<description>
Acceptance criteria:
- <criterion>

## Wave 2 — spawn after Wave 1 completes
### T2: <title>
...

Spawn Wave 1 workers immediately. Advance to next wave when current wave completes.
```

---

## 10. The Key Gap: Viewport Shows Raw JSON

The biggest UX problem: the planner agent outputs JSON in its terminal (that's how Claude Code works — it writes its response to the terminal). The standard viewport shows this raw JSON. Users see:

```
```json
{
  "phase": "clarifying",
  "message": "What would you like to build?",
  "questions": ["Which platform?"],
  "requirement": null,
  "tasks": []
}
```
```

The `message` field IS parsed and could be shown cleanly, but currently there's no mechanism to suppress the raw JSON from the viewport and show only the extracted message.

**The gap:** The planner needs to either:
1. Use sidecar events (structured event stream) that separate tool output from chat content — then `isChatContentLineType` filtering would suppress the raw JSON from the PTY stream and the sidecar would deliver just the text content
2. OR: have the planner use a different output format (e.g., separate the JSON from a human-readable response)
3. OR: add a post-processing filter that hides ` ```json ... ``` ` blocks from the planner's viewport

**Option 1 (sidecar)** is the right long-term solution — it's how the workspace agent works cleanly. But requires `OAT_USE_SIDECAR=1` to be set.

---

## 11. What To Add / Fix (Priority Order)

| Priority | Item | What to do |
|----------|------|-----------|
| P0 | Raw JSON showing in viewport | Filter out ` ```json...``` ` blocks from planner output in LineRenderer OR enable sidecar for planner |
| P0 | Ctrl+X doesn't notify planner | `rejectPlan()` should send a message: "The plan has been rejected. Please revise." |
| P1 | PTY echo shows user text | In app.go, after HandleAppInput, add text to `recentInputs` so PTY echo is filtered |
| P1 | Intent detection false positives | Tighten approval/rejection detection; require longer match or phrase-level patterns |
| P1 | Thinking spinner premature stop | Only set thinking=false when we've seen a complete JSON response, not on first line |
| P2 | Gate 1 + 2 never activate | Wire gates 1 and 2 into state transitions (after requirement clarified, after architecture approved) |
| P2 | completeCurrentPhase all states | Handle Decomposing + Reviewing states in `completeCurrentPhase()` |
| P2 | Persist errors not shown | Log persist errors to TUI status bar |
| P3 | Dead code cleanup | Remove View(), renderRequirement(), renderTestStrategy(), renderTasks(), renderFeedback() |
| P3 | BrainstormTheme validators | Either use validators or remove the field |
