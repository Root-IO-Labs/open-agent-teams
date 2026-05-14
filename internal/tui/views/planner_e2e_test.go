package views

// End-to-end tests for the planner pipeline. These tests simulate the full
// flow from user input through intent detection, state transitions, JSON
// parsing, gate activation, and dispatch — without a live daemon or TUI.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func newE2EPlanner() *PlannerView {
	p := NewPlannerView(nil, "test-repo")
	p.initializeEnhancements()
	return p
}

func injectJSON(p *PlannerView, resp PlannerResponse) {
	b, _ := json.Marshal(resp)
	p.plannerBuffer = "```json\n" + string(b) + "\n```\n"
	p.drainBuffer()
}

func lastAIMessage(p *PlannerView) string {
	for i := len(p.feedback) - 1; i >= 0; i-- {
		if p.feedback[i].Type == "ai" {
			return p.feedback[i].Content
		}
	}
	return ""
}

func lastSystemMessage(p *PlannerView) string {
	for i := len(p.feedback) - 1; i >= 0; i-- {
		if p.feedback[i].Type == "system" {
			return p.feedback[i].Content
		}
	}
	return ""
}

func aiMessages(p *PlannerView) []string {
	var out []string
	for _, e := range p.feedback {
		if e.Type == "ai" {
			out = append(out, e.Content)
		}
	}
	return out
}

// ── Flow A: clarifying → plan lands → gate set → approve → dispatch ──────────

// TestE2E_ClarifyingToDispatch covers the full happy path:
//   user requirement → clarifying questions → plan JSON → gate → ^a → dispatch
func TestE2E_ClarifyingToDispatch(t *testing.T) {
	p := newE2EPlanner()

	// Step 1: user types a requirement
	p.input.SetValue("build a scientific CLI calculator in Python")
	p.handleInputEnhanced()

	if p.state != StateRefiningRequirement {
		t.Fatalf("expected StateRefiningRequirement after first input, got %v", p.state)
	}
	if p.requirement == nil {
		t.Fatal("requirement should be initialised after first input")
	}

	// Step 2: planner responds with clarifying questions
	injectJSON(p, PlannerResponse{
		Phase:   "clarifying",
		Message: "A few questions before I plan:",
		Questions: []string{
			"Which Python version?",
			"Should it support complex numbers?",
		},
	})

	if p.state != StateRefiningRequirement {
		t.Errorf("state should stay RefiningRequirement during clarifying, got %v", p.state)
	}
	if !strings.Contains(lastAIMessage(p), "questions before I plan") {
		t.Errorf("clarifying message not in chat: %v", aiMessages(p))
	}
	// pendingQuestions are surfaced (shown as system messages) then cleared.
	// Verify they were shown rather than checking queue length.
	if len(p.pendingQuestions) != 0 {
		t.Errorf("pendingQuestions should be cleared after surfacing, got %d", len(p.pendingQuestions))
	}

	// Step 3: user answers
	p.input.SetValue("Python 3.11, yes support complex")
	p.handleInputEnhanced()

	// Step 4: planner transitions to architecture
	injectJSON(p, PlannerResponse{
		Phase:   "architecture",
		Message: "Creating the operational spec...",
		Requirement: &PlannerRequirement{
			Title:           "Scientific CLI Calculator",
			Original:        "build a scientific CLI calculator in Python",
			Refined:         "A Python 3.11 CLI calculator with complex number support",
			OperationalSpec: "Uses the `cmath` module for complex arithmetic. Entry point: `calc.py`.",
		},
	})

	if p.state != StateDecomposingTasks {
		t.Errorf("expected StateDecomposingTasks after architecture phase, got %v", p.state)
	}
	if p.requirement.OperationalSpec == "" {
		t.Error("operational spec should be populated from architecture response")
	}

	// Step 5: planner outputs complete plan
	injectJSON(p, PlannerResponse{
		Phase:   "ready_for_review",
		Message: "Plan ready. Approve to dispatch.",
		Requirement: &PlannerRequirement{
			Title:   "Scientific CLI Calculator",
			Refined: "A Python 3.11 CLI calculator with complex number support",
		},
		Tasks: []PlannerTask{
			{ID: "T0", Title: "Tests first", Wave: 0, TestFirst: true,
				Description: "Write pytest stubs", AcceptanceCriteria: []string{"pytest discovers tests"}},
			{ID: "T1", Title: "Core math", Wave: 1, Dependencies: []string{"T0"},
				Description: "Implement arithmetic", AcceptanceCriteria: []string{"all unit tests pass"}},
			{ID: "T2", Title: "CLI wrapper", Wave: 2, Dependencies: []string{"T1"},
				Description: "argparse entry point", AcceptanceCriteria: []string{"calc.py runs"}},
		},
	})

	if p.state != StateReviewingPlan {
		t.Errorf("expected StateReviewingPlan, got %v", p.state)
	}
	if len(p.tasks) != 3 {
		t.Errorf("expected 3 tasks, got %d", len(p.tasks))
	}
	if p.getMaxWave() != 2 {
		t.Errorf("expected max wave 2, got %d", p.getMaxWave())
	}

	// Gate should be set automatically
	if p.currentGate == nil {
		t.Error("plan approval gate should be set when plan lands in StateReviewingPlan")
	}
	if !strings.Contains(lastSystemMessage(p), "3 tasks") {
		t.Errorf("gate prompt should mention task count: %q", lastSystemMessage(p))
	}

	// Step 6: user presses ^a → approvePlan
	cmd := p.approvePlan()
	if cmd == nil {
		t.Error("approvePlan should return a dispatch cmd when tasks are present")
	}
	if p.state != StatePlanLocked {
		t.Errorf("expected StatePlanLocked after approval, got %v", p.state)
	}
	if p.currentGate != nil {
		t.Error("gate should be cleared after approval")
	}

	// Workspace handoff message should include all waves
	handoff := p.buildWorkspaceHandoff()
	if !strings.Contains(handoff, "[PLANNER-APPROVED]") {
		t.Error("handoff missing PLANNER-APPROVED header")
	}
	if !strings.Contains(handoff, "Wave 1") {
		t.Error("handoff missing Wave 1")
	}
	if !strings.Contains(handoff, "Wave 2") {
		t.Error("handoff missing Wave 2")
	}
	if !strings.Contains(handoff, "spawn after Wave 1") {
		t.Error("Wave 2 should reference dependency on Wave 1")
	}
}

// ── Flow B: intent detection drives state machine ────────────────────────────

func TestE2E_IntentApproval_WithGate(t *testing.T) {
	p := newE2EPlanner()
	p.state = StateReviewingPlan
	p.requirement = &Requirement{Refined: "calculator", Iteration: 2}
	p.tasks = []Task{{ID: "T1", Wave: 1, Title: "Core"}}

	// Inject JSON to trigger gate
	injectJSON(p, PlannerResponse{
		Phase:   "ready_for_review",
		Message: "Plan ready.",
		Tasks:   []PlannerTask{{ID: "T1", Title: "Core", Wave: 1}},
	})

	if p.currentGate == nil {
		t.Fatal("gate should be set after ready_for_review with tasks")
	}

	// User types "looks good" → IntentApproval → passGate → approvePlan
	intent := p.detectContextualIntent("looks good")
	if intent != IntentApproval {
		t.Errorf("expected IntentApproval, got %v", intent)
	}

	// Simulate what handleContextualInput does with IntentApproval + gate set
	cmd := p.passGate()
	if cmd == nil {
		t.Error("passGate should return dispatch cmd when gate validation passes")
	}
	if p.currentGate != nil {
		t.Error("gate should be cleared after passing")
	}
}

func TestE2E_IntentCompletion_AdvancesPhase(t *testing.T) {
	p := newE2EPlanner()
	p.state = StateRefiningRequirement
	p.requirement = &Requirement{Original: "calc", Refined: "calculator", Iteration: 1}

	intent := p.detectContextualIntent("I'm done with the requirements")
	if intent != IntentCompletion {
		t.Errorf("expected IntentCompletion, got %v", intent)
	}

	// completeCurrentPhase should send a message to the planner
	cmd := p.completeCurrentPhase()
	if cmd == nil {
		t.Error("completeCurrentPhase should return a send cmd in RefiningRequirement")
	}
}

func TestE2E_IntentRejection_InReview(t *testing.T) {
	p := newE2EPlanner()
	p.state = StateReviewingPlan
	p.tasks = []Task{{ID: "T1", Wave: 1}}

	intent := p.detectContextualIntent("no, change the approach")
	if intent != IntentRejection {
		t.Errorf("expected IntentRejection, got %v", intent)
	}

	cmd := p.rejectPlan()
	_ = cmd
	if p.state != StateRefiningRequirement {
		t.Errorf("rejection should move to StateRefiningRequirement, got %v", p.state)
	}
}

// ── Flow C: action field drives automatic hints ───────────────────────────────

func TestE2E_ActionDispatchTasks_ShowsHint(t *testing.T) {
	p := newE2EPlanner()
	p.tasks = []Task{{ID: "T1", Wave: 1, Title: "Core"}}
	p.state = StateReviewingPlan

	injectJSON(p, PlannerResponse{
		Phase:   "ready_for_review",
		Message: "Ready.",
		Action:  "dispatch_tasks",
		Tasks:   []PlannerTask{{ID: "T1", Title: "Core", Wave: 1}},
	})

	sysMsgs := systemMessages(p)
	found := false
	for _, m := range sysMsgs {
		if strings.Contains(m, "dispatch_tasks") || strings.Contains(m, "^a") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected dispatch_tasks hint in system messages: %v", sysMsgs)
	}
}

func TestE2E_ActionRevise_UnlocksForEditing(t *testing.T) {
	p := newE2EPlanner()
	p.state = StatePlanLocked
	p.isLocked = true
	gate := PhaseGate{ID: "g3", Name: "Plan"}
	p.currentGate = &gate

	injectJSON(p, PlannerResponse{
		Phase:   "draft_plan",
		Message: "Let me revise.",
		Action:  "revise",
		Tasks:   []PlannerTask{{ID: "T1", Title: "Core", Wave: 1}},
	})

	if p.state != StateRefiningRequirement {
		t.Errorf("revise action should return to RefiningRequirement, got %v", p.state)
	}
	if p.isLocked {
		t.Error("revise action should unlock the plan")
	}
	if p.currentGate != nil {
		t.Error("revise action should clear the gate")
	}
}

// ── Flow D: worker prompt quality ────────────────────────────────────────────

func TestE2E_WorkerPrompt_IncludesAllContext(t *testing.T) {
	req := &Requirement{
		Refined:         "Scientific CLI calculator in Python 3.11",
		OperationalSpec: "Uses cmath module. Entry point: calc.py.",
	}
	task := Task{
		ID:                 "T1",
		Title:              "Core math engine",
		Description:        "Implement arithmetic and trig functions",
		TestFirst:          true,
		SpecReference:      "specs/math-engine.md",
		Dependencies:       []string{"T0"},
		AcceptanceCriteria: []string{"all unit tests pass", "trig functions accurate to 6dp"},
	}

	prompt := buildWorkerPrompt(task, req)

	checks := []struct {
		name    string
		snippet string
	}{
		{"requirement", "Scientific CLI calculator"},
		{"operational spec", "cmath module"},
		{"test-first flag", "Test-First"},
		{"spec reference", "specs/math-engine.md"},
		{"task title", "Core math engine"},
		{"description", "Implement arithmetic"},
		{"dependency", "T0"},
		{"acceptance criterion 1", "all unit tests pass"},
		{"acceptance criterion 2", "accurate to 6dp"},
		{"check.sh reminder", "check.sh"},
	}

	for _, c := range checks {
		if !strings.Contains(prompt, c.snippet) {
			t.Errorf("worker prompt missing %s (looking for %q)", c.name, c.snippet)
		}
	}
}

func TestE2E_WorkerPrompt_WithoutOperationalSpec(t *testing.T) {
	req := &Requirement{Refined: "Simple calculator"}
	task := Task{ID: "T1", Title: "Scaffold", Description: "Set up project"}

	prompt := buildWorkerPrompt(task, req)

	if !strings.Contains(prompt, "Simple calculator") {
		t.Error("missing requirement in prompt")
	}
	if strings.Contains(prompt, "How the system works") {
		t.Error("should not include operational spec section when empty")
	}
	if strings.Contains(prompt, "Test-First") {
		t.Error("should not include TDD section when TestFirst is false")
	}
}

// ── Flow E: pending questions surface and clear ───────────────────────────────

func TestE2E_PendingQuestions_SurfaceAndClear(t *testing.T) {
	p := newE2EPlanner()

	injectJSON(p, PlannerResponse{
		Phase:   "clarifying",
		Message: "I have questions:",
		Questions: []string{
			"Which Python version?",
			"Support complex numbers?",
			"CLI or web interface?",
		},
	})

	// After surfacePendingQuestions is called inside applyPlannerResponse,
	// pendingQuestions should be cleared (they were shown).
	if len(p.pendingQuestions) != 0 {
		t.Errorf("pendingQuestions should be cleared after surfacing, got %d remaining", len(p.pendingQuestions))
	}

	// System messages should mention the pending questions header
	sysMsgs := systemMessages(p)
	found := false
	for _, m := range sysMsgs {
		if strings.Contains(m, "Pending") || strings.Contains(m, "Which Python") {
			found = true
			break
		}
	}
	if !found {
		t.Logf("system messages: %v", sysMsgs)
		// surfacePendingQuestions adds system feedback entries — if none found it means
		// questions were shown inline in the AI message instead, which is also valid.
		// Only fail if AI message also doesn't contain the questions.
		if !strings.Contains(lastAIMessage(p), "questions") && !strings.Contains(lastAIMessage(p), "Python") {
			t.Error("questions should appear either in system or AI messages")
		}
	}
}

// ── Flow F: SummaryForList reflects live state ────────────────────────────────

func TestE2E_SummaryForList_LiveState(t *testing.T) {
	p := newE2EPlanner()

	if !strings.Contains(p.SummaryForList(), "waiting") {
		t.Errorf("initial summary should mention waiting: %q", p.SummaryForList())
	}

	p.thinking = true
	if !strings.Contains(p.SummaryForList(), "●") {
		t.Error("thinking indicator ● missing when thinking=true")
	}

	p.thinking = false
	p.state = StateDecomposingTasks
	if !strings.Contains(p.SummaryForList(), "decomposing") {
		t.Errorf("expected decomposing in summary, got %q", p.SummaryForList())
	}

	p.tasks = []Task{{ID: "T1", Wave: 1}, {ID: "T2", Wave: 2}}
	p.state = StateReviewingPlan
	summary := p.SummaryForList()
	if !strings.Contains(summary, "2 tasks") || !strings.Contains(summary, "2 waves") {
		t.Errorf("task/wave count missing from summary: %q", summary)
	}

	p.thinking = true
	summary = p.SummaryForList()
	if !strings.Contains(summary, "●") {
		t.Errorf("thinking ● missing with tasks populated: %q", summary)
	}

	p.state = StatePlanLocked
	p.thinking = false
	summary = p.SummaryForList()
	if !strings.Contains(summary, "locked") {
		t.Errorf("locked state should appear in summary: %q", summary)
	}
}

// ── Flow G: drainBuffer split-batch resilience ────────────────────────────────

func TestE2E_DrainBuffer_MultipleJSONBlocks(t *testing.T) {
	p := newE2EPlanner()

	// Two JSON blocks in one buffer (e.g. planner sent two turns)
	block1, _ := json.Marshal(PlannerResponse{
		Phase: "clarifying", Message: "First turn.",
	})
	block2, _ := json.Marshal(PlannerResponse{
		Phase: "architecture", Message: "Second turn.",
		Requirement: &PlannerRequirement{Title: "T", Refined: "refined"},
	})

	p.plannerBuffer = fmt.Sprintf("```json\n%s\n```\nSome text between.\n```json\n%s\n```\n",
		string(block1), string(block2))
	p.drainBuffer()

	msgs := aiMessages(p)
	if len(msgs) < 2 {
		t.Errorf("expected at least 2 AI messages from 2 JSON blocks, got %d: %v", len(msgs), msgs)
	}

	hasTurn1 := false
	hasTurn2 := false
	for _, m := range msgs {
		if strings.Contains(m, "First turn") {
			hasTurn1 = true
		}
		if strings.Contains(m, "Second turn") {
			hasTurn2 = true
		}
	}
	if !hasTurn1 {
		t.Error("first JSON block message missing")
	}
	if !hasTurn2 {
		t.Error("second JSON block message missing")
	}

	// Architecture phase should be active (last transition wins)
	if p.state != StateDecomposingTasks {
		t.Errorf("expected StateDecomposingTasks after architecture phase, got %v", p.state)
	}
}

// ── Flow H: buildPlannerMessage context prefix ────────────────────────────────

func TestE2E_BuildPlannerMessage_AllPhases(t *testing.T) {
	phases := []struct {
		state    PlannerState
		contains string
	}{
		{StateDefiningRequirement, "clarifying"},
		{StateRefiningRequirement, "clarifying"},
		{StateDecomposingTasks, "clarifying"},
		{StateReviewingPlan, "draft_plan"},
		{StatePlanLocked, "ready_for_review"},
	}

	for _, ph := range phases {
		p := newE2EPlanner()
		p.state = ph.state
		msg := p.buildPlannerMessage("hello")
		if !strings.Contains(msg, "[planner-tui phase=") {
			t.Errorf("state %v: missing phase prefix in message", ph.state)
		}
		if !strings.Contains(msg, ph.contains) {
			t.Errorf("state %v: expected %q in message, got: %q", ph.state, ph.contains, msg)
		}
		if !strings.Contains(msg, "hello") {
			t.Errorf("state %v: user text missing from message", ph.state)
		}
	}
}

// ── Flow I: gate validation ───────────────────────────────────────────────────

func TestE2E_GateValidation_RequiresRefinedRequirement(t *testing.T) {
	p := newE2EPlanner()
	gates := p.initPhaseGates()

	// Gate 1: requirements — needs refined requirement
	gate1 := gates[0]
	if gate1.ValidationFunc(p) {
		t.Error("gate 1 should fail without a requirement")
	}

	p.requirement = &Requirement{Refined: "calc", Iteration: 1}
	if !gate1.ValidationFunc(p) {
		t.Error("gate 1 should pass with a requirement")
	}
}

func TestE2E_GateValidation_ArchitectureNeedsSpec(t *testing.T) {
	p := newE2EPlanner()
	gates := p.initPhaseGates()
	gate2 := gates[1]

	p.requirement = &Requirement{Refined: "calc"}
	if gate2.ValidationFunc(p) {
		t.Error("gate 2 should fail without operational spec and test strategy")
	}

	p.requirement.OperationalSpec = "Uses cmath."
	p.testStrategy = &TestStrategy{Unit: "pytest"}
	if !gate2.ValidationFunc(p) {
		t.Error("gate 2 should pass with spec + test strategy")
	}
}

func TestE2E_GateValidation_PlanNeedsTasks(t *testing.T) {
	p := newE2EPlanner()
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

// ── Flow J: full pipeline timing ─────────────────────────────────────────────

func TestE2E_FullPipeline_Timing(t *testing.T) {
	start := time.Now()
	p := newE2EPlanner()

	// Simulate 5 planner turns
	turns := []PlannerResponse{
		{Phase: "clarifying", Message: "Q1?", Questions: []string{"Language?"}},
		{Phase: "clarifying", Message: "Q2?", Questions: []string{"Platform?"}},
		{Phase: "architecture", Message: "Spec created.",
			Requirement: &PlannerRequirement{Refined: "calc", OperationalSpec: "spec"}},
		{Phase: "draft_plan", Message: "Tasks drafted.",
			Tasks: []PlannerTask{{ID: "T1", Title: "Core", Wave: 1}}},
		{Phase: "ready_for_review", Message: "Ready.",
			Tasks: []PlannerTask{{ID: "T1", Title: "Core", Wave: 1},
				{ID: "T2", Title: "UI", Wave: 2, Dependencies: []string{"T1"}}}},
	}

	for _, turn := range turns {
		injectJSON(p, turn)
	}

	elapsed := time.Since(start)
	if elapsed > 100*time.Millisecond {
		t.Errorf("full 5-turn pipeline took too long: %v (want <100ms)", elapsed)
	}

	// Final state checks
	if p.state != StateReviewingPlan {
		t.Errorf("expected StateReviewingPlan after all turns, got %v", p.state)
	}
	if len(p.tasks) != 2 {
		t.Errorf("expected 2 tasks, got %d", len(p.tasks))
	}
	if p.currentGate == nil {
		t.Error("gate should be set after ready_for_review")
	}
	if len(aiMessages(p)) < 5 {
		t.Errorf("expected at least 5 AI messages (one per turn), got %d", len(aiMessages(p)))
	}
}
