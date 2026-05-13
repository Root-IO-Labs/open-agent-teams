package views

import (
	"strings"
	"testing"
	"time"
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

// drainBuffer must flush plain text immediately (was the root cause of blank
// chat panel — non-JSON responses were silently held until 8000 chars).
func TestDrainBuffer_PlainText(t *testing.T) {
	p := newTestPlanner()
	p.plannerBuffer = "What kind of calculator do you want?\n"
	p.drainBuffer()

	msgs := chatMessages(p)
	if len(msgs) == 0 {
		t.Fatal("expected plain-text response to appear in chat; got none")
	}
	if !strings.Contains(msgs[0], "calculator") {
		t.Errorf("unexpected message: %q", msgs[0])
	}
	if p.plannerBuffer != "" {
		t.Errorf("buffer should be empty after flush, got: %q", p.plannerBuffer)
	}
}

// A complete JSON fence must parse and show only the message field.
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

	msgs := chatMessages(p)
	if len(msgs) == 0 {
		t.Fatal("expected JSON message field to appear in chat; got none")
	}
	if msgs[0] != "A few questions before I plan:" {
		t.Errorf("unexpected message: %q", msgs[0])
	}
	if p.state != StateRefiningRequirement {
		t.Errorf("expected StateRefiningRequirement, got %v", p.state)
	}
	if p.plannerBuffer != "" {
		t.Errorf("buffer should be empty after fence, got: %q", p.plannerBuffer)
	}
}

// Text before the JSON fence must appear in chat; the JSON drives state.
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

	msgs := chatMessages(p)
	if len(msgs) < 2 {
		t.Fatalf("expected preamble + message in chat, got %d entries: %v", len(msgs), msgs)
	}
	if !strings.Contains(msgs[0], "Let me think") {
		t.Errorf("preamble missing, first msg: %q", msgs[0])
	}
	if msgs[1] != "Here is the plan." {
		t.Errorf("unexpected JSON message: %q", msgs[1])
	}
	if p.state != StateReviewingPlan {
		t.Errorf("expected StateReviewingPlan, got %v", p.state)
	}
	if len(p.tasks) != 1 {
		t.Errorf("expected 1 task, got %d", len(p.tasks))
	}
}

// Incomplete fence must stay buffered (wait for next batch) without flushing.
func TestDrainBuffer_IncompleteFence(t *testing.T) {
	p := newTestPlanner()
	p.plannerBuffer = "```json\n{\"phase\": \"clarifying\","
	p.drainBuffer()

	msgs := chatMessages(p)
	if len(msgs) != 0 {
		t.Errorf("incomplete fence should not produce chat messages; got: %v", msgs)
	}
	if p.plannerBuffer == "" {
		t.Error("incomplete fence should stay buffered")
	}
}

// JSON arriving in two batches must parse correctly.
func TestDrainBuffer_SplitAcrossBatches(t *testing.T) {
	p := newTestPlanner()

	// First batch: preamble + opening fence (incomplete)
	p.plannerBuffer = "Thinking...\n```json\n{\"phase\":"
	p.drainBuffer()

	// Preamble is flushed, incomplete fence stays buffered
	msgs := chatMessages(p)
	if len(msgs) == 0 {
		t.Fatal("preamble should have been flushed on first batch")
	}
	if !strings.Contains(p.plannerBuffer, "```json") {
		t.Errorf("incomplete fence should remain in buffer: %q", p.plannerBuffer)
	}

	// Second batch: completes the JSON
	p.plannerBuffer += `"clarifying","message":"Got it.","questions":[],"requirement":null,"tasks":[]}` + "\n```\n"
	p.drainBuffer()

	allMsgs := chatMessages(p)
	found := false
	for _, m := range allMsgs {
		if m == "Got it." {
			found = true
		}
	}
	if !found {
		t.Errorf("second batch JSON message not shown; all messages: %v", allMsgs)
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

	// Message must appear in chat
	msgs := chatMessages(p)
	if len(msgs) == 0 || msgs[0] != "Approve when ready." {
		t.Errorf("message not in chat: %v", msgs)
	}
}

// approvePlan with no tasks must show a hint, not dispatch.
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

// approvePlan with tasks must return a dispatch cmd and show dispatch message.
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
	if !strings.Contains(msg, "after Wave 1") {
		t.Error("Wave 2 should mention dependency on Wave 1")
	}
	if !strings.Contains(msg, "runs without error") {
		t.Error("acceptance criteria missing")
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
	wave3 := p.tasksForWave(3)
	if len(wave3) != 0 {
		t.Errorf("expected 0 Wave 3 tasks, got %d", len(wave3))
	}
}

// buildPlannerMessage must include the phase hint.
func TestBuildPlannerMessage(t *testing.T) {
	p := newTestPlanner()
	p.state = StateDefiningRequirement
	msg := p.buildPlannerMessage("I want a calculator")
	if !strings.Contains(msg, "[planner-tui phase=") {
		t.Errorf("missing phase hint: %q", msg)
	}
	if !strings.Contains(msg, "I want a calculator") {
		t.Errorf("user text missing: %q", msg)
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
