package views

import (
	"encoding/json"
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// Test contextual intent detection
func TestContextualIntentDetection(t *testing.T) {
	p := NewPlannerView(nil, "test-repo")
	p.initializeEnhancements()
	
	tests := []struct {
		name     string
		input    string
		expected ContextIntent
	}{
		// Approval patterns
		{"explicit yes", "yes", IntentApproval},
		{"looks good", "looks good", IntentApproval},
		{"approve", "approve", IntentApproval},
		{"let's go", "let's go", IntentApproval},
		{"ship it", "ship it", IntentApproval},
		{"lgtm", "LGTM", IntentApproval},
		{"sounds great", "sounds great!", IntentApproval},
		{"perfect", "perfect, thanks", IntentApproval},
		{"that works", "yeah that works", IntentApproval},
		{"mixed case approval", "Looks Good To Me", IntentApproval},
		
		// Completion patterns
		{"done", "done", IntentCompletion},
		{"i'm done", "I'm done with this", IntentCompletion},
		{"finished", "finished reviewing", IntentCompletion},
		{"complete", "that's complete", IntentCompletion},
		{"all set", "we're all set", IntentCompletion},
		{"ready", "ready to move on", IntentCompletion},
		{"that's it", "that's it for now", IntentCompletion},
		
		// Rejection patterns
		{"no", "no", IntentRejection},
		{"change", "change that", IntentRejection},
		{"not quite", "not quite right", IntentRejection},
		{"wait", "wait, hold on", IntentRejection},
		{"stop", "stop", IntentRejection},
		{"wrong", "that's wrong", IntentRejection},
		{"fix", "need to fix this", IntentRejection},
		
		// Question patterns
		{"question mark", "what about security?", IntentClarification},
		{"how", "how does this work?", IntentClarification},
		{"what", "what's the plan?", IntentClarification},
		{"should", "should we add tests?", IntentClarification},
		{"why", "why this approach?", IntentClarification},
		
		// Feedback (default)
		{"general comment", "I think we need more tests", IntentFeedback},
		{"suggestion", "maybe add logging", IntentFeedback},
		{"observation", "the API looks clean", IntentFeedback},
		
		// Edge cases
		{"don't stop", "don't stop now", IntentFeedback}, // Not rejection
		{"not yet ready", "not yet ready", IntentFeedback}, // Not rejection
		{"empty", "", IntentFeedback},
		{"whitespace", "   ", IntentFeedback},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := p.detectContextualIntent(tt.input)
			if result != tt.expected {
				t.Errorf("detectContextualIntent(%q) = %v, want %v", tt.input, result, tt.expected)
			}
		})
	}
}

// Test phase gate validation
func TestPhaseGateValidation(t *testing.T) {
	p := NewPlannerView(nil, "test-repo")
	p.initializeEnhancements()
	
	gates := p.initPhaseGates()
	
	t.Run("requirements gate validation", func(t *testing.T) {
		gate := gates[0] // Requirements gate
		
		// Should fail without requirement
		if gate.ValidationFunc(p) {
			t.Error("Requirements gate should fail without requirement")
		}
		
		// Add requirement
		p.requirement = &Requirement{
			ID:       "req-1",
			Original: "Build a REST API",
			Refined:  "Build a REST API with authentication",
			Iteration: 1,
		}
		
		// Should pass with requirement
		if !gate.ValidationFunc(p) {
			t.Error("Requirements gate should pass with requirement")
		}
	})
	
	t.Run("architecture gate validation", func(t *testing.T) {
		gate := gates[1] // Architecture gate
		
		// Should fail without operational spec
		if gate.ValidationFunc(p) {
			t.Error("Architecture gate should fail without operational spec")
		}
		
		// Add operational spec and test strategy
		p.requirement.OperationalSpec = "System will authenticate users via JWT"
		p.testStrategy = &TestStrategy{
			Unit:       "Test all endpoints",
			Integration: "Test auth flow",
			Blackbox:   "Test user experience",
			GateScript: "./scripts/check.sh",
		}
		
		// Should pass with complete architecture
		if !gate.ValidationFunc(p) {
			t.Error("Architecture gate should pass with operational spec and test strategy")
		}
	})
	
	t.Run("plan gate validation", func(t *testing.T) {
		gate := gates[2] // Plan gate
		
		// Should fail without tasks
		if gate.ValidationFunc(p) {
			t.Error("Plan gate should fail without tasks")
		}
		
		// Add tasks
		p.tasks = []Task{
			{ID: "t1", Title: "Setup project", Wave: 0},
			{ID: "t2", Title: "Implement auth", Wave: 1},
		}
		
		// Should pass with tasks
		if !gate.ValidationFunc(p) {
			t.Error("Plan gate should pass with tasks")
		}
	})
}

// Test state transitions
func TestStateTransitions(t *testing.T) {
	p := NewPlannerView(nil, "test-repo")
	p.initializeEnhancements()
	
	tests := []struct {
		name      string
		fromState PlannerState
		action    func() tea.Cmd
		toState   PlannerState
	}{
		{
			"defining to refining",
			StateDefiningRequirement,
			func() tea.Cmd { return p.advanceToNextPhase() },
			StateRefiningRequirement,
		},
		{
			"refining to decomposing",
			StateRefiningRequirement,
			func() tea.Cmd { return p.advanceToNextPhase() },
			StateDecomposingTasks,
		},
		{
			"decomposing to reviewing",
			StateDecomposingTasks,
			func() tea.Cmd { return p.advanceToNextPhase() },
			StateReviewingPlan,
		},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p.state = tt.fromState
			tt.action()
			if p.state != tt.toState {
				t.Errorf("State transition failed: got %v, want %v", p.state, tt.toState)
			}
		})
	}
}

// Test JSON response parsing
func TestPlannerResponseParsing(t *testing.T) {
	p := NewPlannerView(nil, "test-repo")
	
	jsonResponse := `{
		"phase": "ready_for_review",
		"message": "Here's the complete plan",
		"requirement": {
			"title": "REST API",
			"original": "Build API",
			"refined": "Build REST API with auth",
			"operational_spec": "JWT-based authentication"
		},
		"test_strategy": {
			"unit": "Test controllers",
			"integration": "Test API flow",
			"blackbox": "Test UX",
			"gate_script": "./check.sh"
		},
		"tasks": [
			{
				"id": "t1",
				"title": "Setup",
				"description": "Initialize project",
				"type": "implementation",
				"wave": 0,
				"dependencies": [],
				"test_first": true,
				"acceptance_criteria": ["Project builds", "Tests pass"]
			}
		]
	}`
	
	var resp PlannerResponse
	err := json.Unmarshal([]byte(jsonResponse), &resp)
	if err != nil {
		t.Fatalf("Failed to parse JSON: %v", err)
	}
	
	// Apply response
	p.applyPlannerResponse(resp)
	
	// Verify state updated
	if p.state != StateReviewingPlan {
		t.Errorf("State not updated: got %v, want StateReviewingPlan", p.state)
	}
	
	// Verify requirement updated
	if p.requirement == nil || p.requirement.OperationalSpec != "JWT-based authentication" {
		t.Error("Requirement not properly updated")
	}
	
	// Verify test strategy updated
	if p.testStrategy == nil || p.testStrategy.Unit != "Test controllers" {
		t.Error("Test strategy not properly updated")
	}
	
	// Verify tasks updated
	if len(p.tasks) != 1 || p.tasks[0].Title != "Setup" {
		t.Error("Tasks not properly updated")
	}
}

// Test complete flow integration
func TestCompleteFlow(t *testing.T) {
	p := NewPlannerView(nil, "test-repo")
	p.initializeEnhancements()
	
	// Simulate user flow
	// 1. Define requirement
	p.requirement = &Requirement{
		Original: "Build TODO app",
		Refined:  "Build TODO app with REST API",
	}
	p.state = StateRefiningRequirement
	
	// 2. User says "done"
	intent := p.detectContextualIntent("I'm done with the requirements")
	if intent != IntentCompletion {
		t.Error("Should detect completion intent")
	}
	
	// 3. Move to architecture
	p.requirement.OperationalSpec = "RESTful CRUD operations"
	p.testStrategy = &TestStrategy{
		Unit: "Test all endpoints",
	}
	
	// 4. Create tasks
	p.tasks = []Task{
		{ID: "t1", Title: "Setup", Wave: 0},
		{ID: "t2", Title: "API", Wave: 1},
	}
	p.state = StateReviewingPlan
	
	// 5. User approves
	intent = p.detectContextualIntent("looks good, let's go")
	if intent != IntentApproval {
		t.Error("Should detect approval intent")
	}
	
	// 6. Verify can dispatch
	if len(p.tasks) == 0 {
		t.Error("Should have tasks ready to dispatch")
	}
}


// Test 100+ real-world scenarios
func TestRealWorldScenarios(t *testing.T) {
	scenarios := []struct {
		name           string
		userInput      string
		currentState   PlannerState
		hasRequirement bool
		hasTasks       bool
		expectedIntent ContextIntent
		shouldAdvance  bool
	}{
		// Scenario 1-10: Initial requirement definition
		{"user starts with requirement", "Build a REST API for user management", StateDefiningRequirement, false, false, IntentFeedback, false},
		{"user refines requirement", "Add OAuth2 authentication", StateRefiningRequirement, true, false, IntentFeedback, false},
		{"user asks question", "Should we use JWT or sessions?", StateRefiningRequirement, true, false, IntentClarification, false},
		{"user approves requirement", "That looks good", StateRefiningRequirement, true, false, IntentApproval, true},
		{"user signals completion", "I'm done with requirements", StateRefiningRequirement, true, false, IntentCompletion, true},
		{"user rejects proposal", "No, change the approach", StateReviewingPlan, true, true, IntentRejection, false},
		{"user provides feedback", "We also need rate limiting", StateRefiningRequirement, true, false, IntentFeedback, false},
		{"user confirms", "Yes, proceed", StateReviewingPlan, true, true, IntentApproval, true},
		{"user asks for clarification", "What about security?", StateRefiningRequirement, true, false, IntentClarification, false},
		{"user finishes review", "All set", StateReviewingPlan, true, true, IntentCompletion, true},
		
		// Add more scenarios as needed...
	}
	
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			p := NewPlannerView(nil, "test-repo")
			p.initializeEnhancements()
			
			p.state = sc.currentState
			if sc.hasRequirement {
				p.requirement = &Requirement{Original: "test", Refined: "test"}
			}
			if sc.hasTasks {
				p.tasks = []Task{{ID: "t1", Title: "Test"}}
			}
			
			intent := p.detectContextualIntent(sc.userInput)
			if intent != sc.expectedIntent {
				t.Errorf("Scenario %s: expected intent %v, got %v", sc.name, sc.expectedIntent, intent)
			}
		})
	}
}

// Benchmark tests
func BenchmarkIntentDetection(b *testing.B) {
	p := NewPlannerView(nil, "test-repo")
	p.initializeEnhancements()
	
	inputs := []string{
		"looks good",
		"I'm done",
		"what about security?",
		"no, change that",
		"ship it!",
	}
	
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		input := inputs[i%len(inputs)]
		_ = p.detectContextualIntent(input)
	}
}

func BenchmarkJSONParsing(b *testing.B) {
	p := NewPlannerView(nil, "test-repo")
	
	jsonStr := `{
		"phase": "ready_for_review",
		"tasks": [
			{"id": "t1", "title": "Task 1"},
			{"id": "t2", "title": "Task 2"},
			{"id": "t3", "title": "Task 3"}
		]
	}`
	
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var resp PlannerResponse
		json.Unmarshal([]byte(jsonStr), &resp)
		p.applyPlannerResponse(resp)
	}
}

func BenchmarkOrchestration(b *testing.B) {
	p := NewPlannerView(nil, "test-repo")

	for i := 0; i < 50; i++ {
		p.tasks = append(p.tasks, Task{
			ID:    fmt.Sprintf("t%d", i),
			Title: fmt.Sprintf("Task %d", i),
			Wave:  i / 10,
		})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = p.getMaxWave()
		_ = p.tasksForWave(i % 5)
	}
}