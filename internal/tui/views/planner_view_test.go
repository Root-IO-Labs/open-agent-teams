package views

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
)

func newTestPlanner() *PlannerView {
	return &PlannerView{
		state:    StateDefiningRequirement,
		feedback: []FeedbackEntry{},
	}
}

func systemMessages(p *PlannerView) []string {
	var msgs []string
	for _, e := range p.feedback {
		if e.Type == "system" {
			msgs = append(msgs, e.Content)
		}
	}
	return msgs
}

func chatMessages(p *PlannerView) []string {
	var msgs []string
	for _, e := range p.feedback {
		if e.Type == "ai" {
			msgs = append(msgs, e.Content)
		}
	}
	return msgs
}

// drainBuffer must parse JSON and update state — rendering is the viewport's job.
// Plain text is discarded (shown through the standard line renderer instead).
func TestDrainBuffer_PlainText(t *testing.T) {
	p := newTestPlanner()
	p.plannerBuffer = "What kind of calculator do you want?\n"
	p.drainBuffer()

	// Plain text is not added to feedback anymore — it shows via standard viewport.
	// Buffer should be cleared.
	if p.plannerBuffer != "" {
		t.Errorf("buffer should be cleared after plain text, got: %q", p.plannerBuffer)
	}
}

// A complete JSON fence must parse and update state (phase, requirement, tasks).
// The message field is NOT added to feedback — it shows via standard viewport.
func TestDrainBuffer_StructuredJSON(t *testing.T) {
	p := newTestPlanner()
	p.plannerBuffer = "```json\n" + `{
  "phase": "clarifying",
  "message": "A few questions before I plan:",
  "questions": ["Which language?"],
  "requirement": null,
  "tasks": []
}` + "\n```\n"
	p.drainBuffer()

	// State updated, buffer cleared
	if p.state != StateRefiningRequirement {
		t.Errorf("expected StateRefiningRequirement, got %v", p.state)
	}
	if p.plannerBuffer != "" {
		t.Errorf("buffer should be empty after fence, got: %q", p.plannerBuffer)
	}
	// No feedback additions — viewport shows raw output
	if len(chatMessages(p)) != 0 {
		t.Errorf("no AI chat messages should be added; rendering is viewport's job")
	}
}

// JSON with requirement and tasks must populate planning state.
func TestDrainBuffer_PreambleThenJSON(t *testing.T) {
	p := newTestPlanner()
	p.plannerBuffer = "Let me think about this.\n```json\n" + `{
  "phase": "draft_plan",
  "message": "Here is the plan.",
  "questions": [],
  "requirement": {"title":"T","original":"orig","refined":"refined"},
  "tasks": [{"id":"T1","title":"Task 1","description":"desc","wave":1,"dependencies":[],"acceptance_criteria":[]}]
}` + "\n```\n"
	p.drainBuffer()

	if p.state != StateReviewingPlan {
		t.Errorf("expected StateReviewingPlan, got %v", p.state)
	}
	if len(p.tasks) != 1 {
		t.Errorf("expected 1 task, got %d", len(p.tasks))
	}
	if p.requirement == nil || p.requirement.Refined != "refined" {
		t.Errorf("requirement not set correctly")
	}
}

// Incomplete fence must stay buffered.
func TestDrainBuffer_IncompleteFence(t *testing.T) {
	p := newTestPlanner()
	p.plannerBuffer = "```json\n{\"phase\": \"clarifying\","
	p.drainBuffer()

	if p.plannerBuffer == "" {
		t.Error("incomplete fence should stay buffered")
	}
	if p.state != StateDefiningRequirement {
		t.Error("state should not change with incomplete JSON")
	}
}

// JSON arriving in two batches must parse correctly.
func TestDrainBuffer_SplitAcrossBatches(t *testing.T) {
	p := newTestPlanner()

	// First batch: incomplete fence
	p.plannerBuffer = "```json\n{\"phase\":"
	p.drainBuffer()
	if p.plannerBuffer == "" {
		t.Fatal("incomplete fence should stay buffered")
	}

	// Second batch: completes the JSON
	p.plannerBuffer += `"clarifying","message":"Got it.","questions":[],"requirement":null,"tasks":[]}` + "\n```\n"
	p.drainBuffer()

	if p.state != StateRefiningRequirement {
		t.Errorf("state should be clarifying after complete JSON, got %v", p.state)
	}
}

// applyPlannerResponse must populate requirement, tasks, and state.
func TestApplyPlannerResponse_ReadyForReview(t *testing.T) {
	p := newTestPlanner()
	resp := PlannerResponse{
		Phase:   "ready_for_review",
		Message: "Approve when ready.",
		Requirement: &PlannerRequirement{
			Title:    "Calculator",
			Original: "make calc",
			Refined:  "Scientific CLI calculator in Python 3",
		},
		Tasks: []PlannerTask{
			{ID: "T1", Title: "Scaffold", Wave: 1, AcceptanceCriteria: []string{"runs without error"}},
			{ID: "T2", Title: "Core math", Wave: 2, Dependencies: []string{"T1"}},
		},
	}
	p.applyPlannerResponse(resp)

	if p.state != StateReviewingPlan {
		t.Errorf("expected StateReviewingPlan, got %v", p.state)
	}
	if p.requirement == nil {
		t.Fatal("requirement should be set")
	}
	if p.requirement.Refined != "Scientific CLI calculator in Python 3" {
		t.Errorf("unexpected refined: %q", p.requirement.Refined)
	}
	if len(p.tasks) != 2 {
		t.Errorf("expected 2 tasks, got %d", len(p.tasks))
	}
	if p.tasks[0].AcceptanceCriteria[0] != "runs without error" {
		t.Errorf("acceptance criteria not copied")
	}
	if p.tasks[1].Wave != 2 {
		t.Errorf("expected wave 2, got %d", p.tasks[1].Wave)
	}
	// Gate should be set when plan lands
	if p.currentGate == nil {
		t.Error("plan gate should be set in StateReviewingPlan with tasks")
	}
	// No feedback additions — rendering is viewport's job
	if len(chatMessages(p)) != 0 {
		t.Errorf("no AI chat messages should be added by applyPlannerResponse")
	}
}

// buildPlannerMessage was removed; sendToPlanner now sends raw text.
func TestBuildPlannerMessage(t *testing.T) {
	p := newTestPlanner()
	p.state = StateDefiningRequirement
	// sendToPlanner sends raw text now — no prefix leaking into PTY echo
	text := "I want a calculator"
	// Just verify sendToPlanner doesn't crash with nil client
	cmd := p.sendToPlanner(text)
	if cmd == nil {
		t.Error("sendToPlanner should return a cmd even with nil client")
	}
}

// Requirement.LastUpdated must be set when applying a response.
func TestApplyPlannerResponse_LastUpdated(t *testing.T) {
	p := newTestPlanner()
	before := time.Now()
	p.applyPlannerResponse(PlannerResponse{
		Phase:   "draft_plan",
		Message: "here",
		Requirement: &PlannerRequirement{
			Title: "T", Original: "o", Refined: "r",
		},
	})
	if p.requirement == nil {
		t.Fatal("requirement nil")
	}
	if p.requirement.LastUpdated.Before(before) {
		t.Error("LastUpdated not set")
	}
}

// approvePlan with no tasks must show a hint.
func TestApprovePlan_NoTasks(t *testing.T) {
	p := newTestPlanner()
	p.state = StateReviewingPlan
	cmd := p.approvePlan()
	if cmd != nil {
		t.Error("expected nil cmd when no tasks")
	}
	sysMsgs := systemMessages(p)
	if len(sysMsgs) == 0 {
		t.Fatal("expected a hint message")
	}
	if !strings.Contains(sysMsgs[0], "^p") {
		t.Errorf("hint should mention ^p, got: %q", sysMsgs[0])
	}
}

// approvePlan with tasks must return a dispatch cmd.
func TestApprovePlan_WithTasks(t *testing.T) {
	p := newTestPlanner()
	p.state = StateReviewingPlan
	p.requirement = &Requirement{Refined: "Scientific CLI calculator"}
	p.tasks = []Task{
		{ID: "T1", Title: "Scaffold", Wave: 1},
		{ID: "T2", Title: "Core math", Wave: 2, Dependencies: []string{"T1"}},
	}
	cmd := p.approvePlan()
	if cmd == nil {
		t.Error("expected dispatch cmd when tasks are present")
	}
	if p.state != StatePlanLocked {
		t.Errorf("expected StatePlanLocked, got %v", p.state)
	}
	sysMsgs := systemMessages(p)
	if len(sysMsgs) == 0 {
		t.Fatal("expected dispatch confirmation message")
	}
	if !strings.Contains(sysMsgs[0], "workspace") {
		t.Errorf("expected workspace mention, got: %q", sysMsgs[0])
	}
}

// buildWorkspaceHandoff must include requirement and wave breakdown.
func TestBuildWorkspaceHandoff(t *testing.T) {
	p := newTestPlanner()
	p.requirement = &Requirement{Refined: "A scientific CLI calculator in Python 3"}
	p.tasks = []Task{
		{ID: "T1", Title: "Scaffold", Description: "Set up project", Wave: 1,
			AcceptanceCriteria: []string{"runs without error"}},
		{ID: "T2", Title: "Core math", Description: "Implement trig", Wave: 2,
			Dependencies: []string{"T1"}},
	}
	msg := p.buildWorkspaceHandoff()

	if !strings.Contains(msg, "[PLANNER-APPROVED]") {
		t.Error("missing PLANNER-APPROVED header")
	}
	if !strings.Contains(msg, "scientific CLI calculator") {
		t.Error("missing requirement text")
	}
	if !strings.Contains(msg, "Wave 1") {
		t.Error("missing Wave 1 section")
	}
	if !strings.Contains(msg, "Wave 2") {
		t.Error("missing Wave 2 section")
	}
	if !strings.Contains(msg, "runs without error") {
		t.Error("acceptance criteria missing")
	}
	if !strings.Contains(msg, "[planner-task:T1]") || !strings.Contains(msg, "[planner-task:T2]") {
		t.Error("handoff should include stable planner task markers")
	}
	if !strings.Contains(msg, "Workspace owns worker creation") {
		t.Error("handoff should define the execution contract")
	}
	if !strings.Contains(msg, "The planner must not spawn workers") {
		t.Error("handoff should explicitly forbid planner worker spawning")
	}
	if !strings.Contains(msg, "WAVE_STATE: current_wave=1 total_waves=2") {
		t.Error("handoff should include persisted wave state")
	}
	if !strings.Contains(msg, `oat message send "$OAT_AGENT_NAME"`) {
		t.Error("handoff should tell workspace to persist state to itself")
	}
}

func TestWorkspaceDispatchTargetsPreferDefaultWorkspace(t *testing.T) {
	targets := workspaceDispatchTargets()
	if len(targets) < 2 {
		t.Fatalf("expected default and legacy workspace targets, got %v", targets)
	}
	if targets[0] != "default" {
		t.Fatalf("first workspace dispatch target = %q, want default", targets[0])
	}
	if targets[1] != "workspace" {
		t.Fatalf("second workspace dispatch target = %q, want workspace", targets[1])
	}
}

func TestTrackWorkerAssignmentFromPlannerMarker(t *testing.T) {
	p := newTestPlanner()
	p.tasks = []Task{{ID: "T1", Title: "Scaffold", Wave: 1}}

	p.TrackWorkerAssignment("worker-alpha", "[planner-task:T1] Scaffold the project")
	p.UpdateWorkerStatus("worker-alpha", 0, false)

	if got := p.taskWorkers["T1"]; got != "worker-alpha" {
		t.Fatalf("taskWorkers[T1] = %q, want worker-alpha", got)
	}
	if p.tasks[0].AssignedTo != "worker-alpha" {
		t.Fatalf("AssignedTo = %q, want worker-alpha", p.tasks[0].AssignedTo)
	}
	if p.tasks[0].Status != TaskStatusInProgress {
		t.Fatalf("Status = %v, want TaskStatusInProgress", p.tasks[0].Status)
	}
}

// TrackWorkerAssignment is called every TUI poll (2s) for every live worker.
// Without dedup, repeated identical assignments would re-persist the plan on
// every tick — which in production accumulated 4 GB / 1370 version files for a
// single plan. The fix is gating persistPlan on whether anything actually
// changed; this test pins that contract.
func TestApplyWorkerAssignments_DoesNotRepersistOnIdenticalCall(t *testing.T) {
	p := newTestPlanner()
	p.requirement = &Requirement{Refined: "test"}
	p.tasks = []Task{{ID: "T1", Title: "Scaffold", Wave: 1}}

	// First call mutates state and persists once.
	p.applyWorkerAssignments(map[string]string{"T1": "worker-alpha"})
	if p.persistCallCount != 1 {
		t.Fatalf("first call persistCallCount = %d, want 1", p.persistCallCount)
	}

	// Second identical call must not re-persist.
	p.applyWorkerAssignments(map[string]string{"T1": "worker-alpha"})
	if p.persistCallCount != 1 {
		t.Fatalf("idempotent call persistCallCount = %d, want 1 (no re-persist)", p.persistCallCount)
	}

	// New assignment must persist again.
	p.applyWorkerAssignments(map[string]string{"T1": "worker-beta"})
	if p.persistCallCount != 2 {
		t.Fatalf("changed call persistCallCount = %d, want 2", p.persistCallCount)
	}
}

// Same idempotency contract for UpdateWorkerStatus, which is also called every
// TUI poll for waiting workers.
func TestUpdateWorkerStatus_DoesNotRepersistOnIdenticalCall(t *testing.T) {
	p := newTestPlanner()
	p.requirement = &Requirement{Refined: "test"}
	p.tasks = []Task{{ID: "T1", Title: "Scaffold", Wave: 1, AssignedTo: "worker-alpha", Status: TaskStatusInProgress}}
	p.taskWorkers = map[string]string{"T1": "worker-alpha"}
	p.taskPRs = map[string]int{"T1": 42}

	// First call with same PR + status must NOT persist (nothing changed).
	p.UpdateWorkerStatus("worker-alpha", 42, false)
	if p.persistCallCount != 0 {
		t.Fatalf("no-op call persistCallCount = %d, want 0", p.persistCallCount)
	}

	// Completing the worker must persist once.
	p.UpdateWorkerStatus("worker-alpha", 42, true)
	if p.persistCallCount != 1 {
		t.Fatalf("completion call persistCallCount = %d, want 1", p.persistCallCount)
	}

	// Re-completing must not persist again.
	p.UpdateWorkerStatus("worker-alpha", 42, true)
	if p.persistCallCount != 1 {
		t.Fatalf("idempotent completion persistCallCount = %d, want 1", p.persistCallCount)
	}
}

// tasksForWave must return only tasks in the given wave.
func TestTasksForWave(t *testing.T) {
	p := newTestPlanner()
	p.tasks = []Task{
		{ID: "T1", Wave: 1},
		{ID: "T2", Wave: 1},
		{ID: "T3", Wave: 2},
	}
	wave1 := p.tasksForWave(1)
	if len(wave1) != 2 {
		t.Errorf("expected 2 Wave 1 tasks, got %d", len(wave1))
	}
	wave2 := p.tasksForWave(2)
	if len(wave2) != 1 {
		t.Errorf("expected 1 Wave 2 task, got %d", len(wave2))
	}
}

// SummaryForList must reflect thinking state.
func TestSummaryForList_Thinking(t *testing.T) {
	p := newTestPlanner()
	p.thinking = true
	if !strings.Contains(p.SummaryForList(), "●") {
		t.Error("thinking indicator missing when thinking=true")
	}
	p.thinking = false
	p.state = StateDecomposingTasks
	if !strings.Contains(p.SummaryForList(), "decomposing") {
		t.Errorf("expected decomposing in summary, got %q", p.SummaryForList())
	}
}

func TestHelpHintsExposeRefineAndBrainstormDuringDecomposition(t *testing.T) {
	p := newTestPlanner()
	p.state = StateDecomposingTasks
	p.requirement = &Requirement{Refined: "Build calculator"}
	p.brainstormThemes = []BrainstormTheme{{Name: "Tech Stack"}}

	hints := p.HelpHints()
	if !strings.Contains(hints, "^r:refine") {
		t.Fatalf("expected refine hint in decomposing state, got %q", hints)
	}
	if !strings.Contains(hints, "^b:brainstorm") {
		t.Fatalf("expected brainstorm hint in decomposing state, got %q", hints)
	}
}

func TestRenderPlannerMarkdownFormatsCommonMarkdown(t *testing.T) {
	rendered := renderPlannerMarkdown("## Title\n1. **UI Framework**: use `shadcn`\n- Save formulas", 80, lipgloss.NewStyle())

	if strings.Contains(rendered, "**UI Framework**") {
		t.Fatalf("bold markdown marker should be rendered, got %q", rendered)
	}
	if strings.Contains(rendered, "`shadcn`") {
		t.Fatalf("inline code marker should be rendered, got %q", rendered)
	}
	if strings.Contains(rendered, "## Title") {
		t.Fatalf("heading marker should be rendered, got %q", rendered)
	}
	if !strings.Contains(rendered, "Title") || !strings.Contains(rendered, "UI Framework") || !strings.Contains(rendered, "shadcn") {
		t.Fatalf("rendered output lost content: %q", rendered)
	}
}

// Gate validation — plan gate requires tasks.
func TestGateValidation_PlanNeedsTasks(t *testing.T) {
	p := newTestPlanner()
	gates := p.initPhaseGates()
	gate3 := gates[2]

	if gate3.ValidationFunc(p) {
		t.Error("gate 3 should fail without tasks")
	}
	p.tasks = []Task{{ID: "T1", Wave: 1}}
	if !gate3.ValidationFunc(p) {
		t.Error("gate 3 should pass with tasks")
	}
}
