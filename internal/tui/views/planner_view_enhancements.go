package views

import (
	"strings"
	"time"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
)

// ContextIntent represents user's contextual intent
type ContextIntent int

const (
	IntentUnknown ContextIntent = iota
	IntentApproval
	IntentRejection
	IntentClarification
	IntentCompletion
	IntentFeedback
)

// PlannerContext provides contextual awareness for user intent
type PlannerContext struct {
	ApprovalPatterns   []string
	RejectionPatterns  []string
	QuestionPatterns   []string
	CompletionSignals  []string
}

// PhaseGate represents a checkpoint requiring explicit approval
type PhaseGate struct {
	ID               string
	Name             string
	RequiredElements []string
	ApprovalPrompt   string
	ValidationFunc   func(*PlannerView) bool
}

// BrainstormTheme represents a topic for Socratic dialogue
type BrainstormTheme struct {
	Name      string
	Questions []string
	Validator func(string) bool
}

// Enhanced PlannerView additions
func EnhancePlannerView(p *PlannerView) {
	p.context = initPlannerContext()
	p.pendingQuestions = []string{}
	p.brainstormThemes = initBrainstormThemes()
	p.currentGate = nil
}

func initPlannerContext() *PlannerContext {
	return &PlannerContext{
		ApprovalPatterns: []string{
			"looks good", "yes", "approve", "let's go", "sounds good",
			"perfect", "great", "confirmed", "agree", "proceed",
			"lgtm", "ship it", "do it", "go ahead", "ok",
			"that works", "all good", "ready", "let's do it",
			"i approve", "approved", "continue", "next",
		},
		RejectionPatterns: []string{
			"no", "change", "update", "not quite", "wait",
			"stop", "hold on", "actually", "instead", "different",
			"wrong", "incorrect", "fix", "revise", "redo",
		},
		QuestionPatterns: []string{
			"?", "what", "how", "should", "could", "would",
			"why", "when", "where", "which", "can you",
			"is it", "are we", "do we", "will",
		},
		CompletionSignals: []string{
			"done", "finished", "complete", "ready", "that's it",
			"i'm done", "we're done", "all set", "that's all",
			"nothing more", "finalize", "wrap up", "end",
		},
	}
}

func initBrainstormThemes() []BrainstormTheme {
	return []BrainstormTheme{
		{
			Name: "Tech Stack",
			Questions: []string{
				"What programming language should we use?",
				"Do you need a web framework? Which one?",
				"What database system fits your needs?",
				"Any specific libraries or tools required?",
			},
			Validator: func(s string) bool {
				// Validate tech stack choices
				return strings.Contains(strings.ToLower(s), "python") ||
					strings.Contains(strings.ToLower(s), "go") ||
					strings.Contains(strings.ToLower(s), "javascript") ||
					strings.Contains(strings.ToLower(s), "typescript")
			},
		},
		{
			Name: "Architecture",
			Questions: []string{
				"Will this be a monolithic or microservices architecture?",
				"Do you need real-time features?",
				"What are your scalability requirements?",
				"Any specific architectural patterns to follow?",
			},
		},
		{
			Name: "Testing Strategy",
			Questions: []string{
				"What's your target test coverage?",
				"Should we include integration tests?",
				"Do you need performance benchmarks?",
				"Any compliance or security testing required?",
			},
		},
		{
			Name: "Deployment",
			Questions: []string{
				"Where will this be deployed?",
				"Do you need CI/CD pipelines?",
				"Any specific deployment constraints?",
				"What's your scaling strategy?",
			},
		},
		{
			Name: "Security",
			Questions: []string{
				"What authentication method should we use?",
				"Do you need role-based access control?",
				"Any specific security compliance requirements?",
				"How should we handle sensitive data?",
			},
		},
		{
			Name: "User Experience",
			Questions: []string{
				"Who are your target users?",
				"What's the expected user load?",
				"Do you need mobile support?",
				"Any accessibility requirements?",
			},
		},
	}
}

// detectContextualIntent analyzes user input to determine intent
func (p *PlannerView) detectContextualIntent(input string) ContextIntent {
	normalized := strings.ToLower(strings.TrimSpace(input))
	
	// Check for completion signals first (more specific)
	for _, pattern := range p.context.CompletionSignals {
		if strings.Contains(normalized, pattern) {
			// Special case: "ready" alone or "ready to move on" is completion
			if pattern == "ready" && (normalized == "ready" || strings.Contains(normalized, "ready to")) {
				return IntentCompletion
			}
			// Other completion patterns
			if pattern != "ready" && strings.Contains(normalized, pattern) {
				return IntentCompletion
			}
		}
	}
	
	// Check for rejection patterns before approval
	for _, pattern := range p.context.RejectionPatterns {
		if strings.Contains(normalized, pattern) &&
			!strings.Contains(normalized, "don't") && // avoid "don't stop"
			!strings.Contains(normalized, "not yet") && // avoid "not yet ready"
			!strings.Contains(normalized, "looks") { // avoid "looks wrong" being detected when "looks good"
			return IntentRejection
		}
	}
	
	// Check for approval signals - but exclude false positives
	for _, pattern := range p.context.ApprovalPatterns {
		if strings.Contains(normalized, pattern) {
			// Exclude "ready" if it's part of "not yet ready" or "ready to move on"
			if pattern == "ready" && (strings.Contains(normalized, "not yet") || strings.Contains(normalized, "ready to")) {
				continue
			}
			// Exclude patterns that are observations
			if strings.Contains(normalized, "api") || strings.Contains(normalized, "code") {
				continue
			}
			return IntentApproval
		}
	}
	
	// Check for questions
	if strings.Contains(normalized, "?") ||
		strings.HasPrefix(normalized, "what") ||
		strings.HasPrefix(normalized, "how") ||
		strings.HasPrefix(normalized, "why") ||
		strings.HasPrefix(normalized, "when") ||
		strings.HasPrefix(normalized, "should") {
		return IntentClarification
	}
	
	return IntentFeedback
}

// handleContextualInput processes input based on detected intent
func (p *PlannerView) handleContextualInput(text string) tea.Cmd {
	intent := p.detectContextualIntent(text)
	
	switch intent {
	case IntentApproval:
		// Check if we have a pending gate
		if p.currentGate != nil {
			return p.passGate()
		}
		// Check if we're in review state
		if p.state == StateReviewingPlan && len(p.tasks) > 0 {
			p.feedback = append(p.feedback, FeedbackEntry{
				Type:      "system",
				Content:   "✅ Detected approval. Moving to dispatch...",
				Timestamp: time.Now(),
			})
			return p.approvePlan()
		}
		// Otherwise, check if we should advance phase
		if p.state == StateRefiningRequirement {
			return p.advanceToNextPhase()
		}
		
	case IntentCompletion:
		// User signaled they're done with current phase
		if p.state == StateDefiningRequirement || p.state == StateRefiningRequirement {
			p.feedback = append(p.feedback, FeedbackEntry{
				Type:      "system",
				Content:   "✅ Completion detected. Finalizing current phase...",
				Timestamp: time.Now(),
			})
			return p.completeCurrentPhase()
		}
		
	case IntentRejection:
		if p.state == StateReviewingPlan {
			return p.rejectPlan()
		}
		// Provide feedback that changes are needed
		p.feedback = append(p.feedback, FeedbackEntry{
			Type:      "system",
			Content:   "Understood. Please describe what needs to be changed.",
			Timestamp: time.Now(),
		})
		
	case IntentClarification:
		// User is asking a question - let it go through to planner
		break
	}
	
	// Default: send to planner as normal feedback
	return p.sendToPlanner(text)
}

// Enhanced handleInput with contextual awareness
func (p *PlannerView) handleInputEnhanced() (*PlannerView, tea.Cmd) {
	text := strings.TrimSpace(p.input.Value())
	if text == "" {
		return p, nil
	}

	p.input.SetValue("")

	p.feedback = append(p.feedback, FeedbackEntry{
		Type:      "user",
		Content:   text,
		Timestamp: time.Now(),
	})

	if p.requirement == nil {
		p.requirement = &Requirement{
			ID:          fmt.Sprintf("req-%d", time.Now().Unix()),
			Original:    text,
			Refined:     text,
			Iteration:   1,
			LastUpdated: time.Now(),
		}
		p.state = StateRefiningRequirement
	}

	// Track quiet turns in clarifying phase. After 3 turns with no phase
	// advance, proactively offer the next brainstorm theme to help the user
	// think through the requirement from a different angle.
	if p.state == StateRefiningRequirement || p.state == StateDefiningRequirement {
		p.clarifyingTurns++
		if p.clarifyingTurns >= 3 && len(p.brainstormThemes) > 0 {
			p.clarifyingTurns = 0
			cmd := p.handleContextualInput(text)
			// Append Socratic dialogue after the normal send.
			brainstormCmd := p.conductSocraticDialogue()
			if brainstormCmd != nil && cmd != nil {
				return p, tea.Batch(cmd, brainstormCmd)
			}
			if brainstormCmd != nil {
				return p, brainstormCmd
			}
			return p, cmd
		}
	} else {
		p.clarifyingTurns = 0
	}

	return p, p.handleContextualInput(text)
}

// Phase gate functions
func (p *PlannerView) initPhaseGates() []PhaseGate {
	return []PhaseGate{
		{
			ID:   "gate_1_requirements",
			Name: "Requirements Clarity Gate",
			RequiredElements: []string{
				"clear_scope",
				"success_criteria",
				"technical_constraints",
				"testing_requirements",
			},
			ApprovalPrompt: "Requirements are clear. Shall I proceed to architecture design? Say 'yes' or 'approve' to continue.",
			ValidationFunc: func(pv *PlannerView) bool {
				return p.validateRequirementsComplete(pv)
			},
		},
		{
			ID:   "gate_2_architecture",
			Name: "Architecture Approval Gate",
			RequiredElements: []string{
				"operational_spec",
				"interface_contracts",
				"test_strategy",
				"gate_mechanism",
			},
			ApprovalPrompt: "Architecture is complete. Approve to proceed to task planning? Say 'approve' to continue.",
			ValidationFunc: func(pv *PlannerView) bool {
				return p.validateArchitectureComplete(pv)
			},
		},
		{
			ID:   "gate_3_plan",
			Name: "Plan Approval Gate",
			RequiredElements: []string{
				"wave_organization",
				"task_dependencies",
				"acceptance_criteria",
			},
			ApprovalPrompt: fmt.Sprintf("Plan is ready with %d tasks in %d waves. Type 'approve' to dispatch or provide feedback.", len(p.tasks), p.getMaxWave()),
			ValidationFunc: func(pv *PlannerView) bool {
				return p.validatePlanComplete(pv)
			},
		},
	}
}

// Gate validation functions
func (p *PlannerView) validateRequirementsComplete(pv *PlannerView) bool {
	return pv.requirement != nil &&
		pv.requirement.Refined != "" &&
		pv.requirement.Iteration > 0
}

func (p *PlannerView) validateArchitectureComplete(pv *PlannerView) bool {
	return pv.requirement != nil &&
		pv.requirement.OperationalSpec != "" &&
		pv.testStrategy != nil
}

func (p *PlannerView) validatePlanComplete(pv *PlannerView) bool {
	return len(pv.tasks) > 0 &&
		pv.getMaxWave() > 0
}

// passGate handles phase gate approval. Gate_3 (plan approval) dispatches
// directly; earlier gates advance to the next planning phase.
func (p *PlannerView) passGate() tea.Cmd {
	if p.currentGate == nil {
		return nil
	}

	if p.currentGate.ValidationFunc != nil && !p.currentGate.ValidationFunc(p) {
		p.feedback = append(p.feedback, FeedbackEntry{
			Type:      "system",
			Content:   "Gate validation failed. Please complete all requirements before continuing.",
			Timestamp: time.Now(),
		})
		return nil
	}

	gateName := p.currentGate.Name
	gateID := p.currentGate.ID
	p.currentGate = nil

	p.feedback = append(p.feedback, FeedbackEntry{
		Type:      "system",
		Content:   fmt.Sprintf("%s passed.", gateName),
		Timestamp: time.Now(),
	})

	// Gate 3 is the plan-approval gate — dispatch rather than advance phase.
	if gateID == "gate_3_plan" || p.state == StateReviewingPlan {
		return p.approvePlan()
	}
	return p.advanceToNextPhase()
}

// advanceToNextPhase moves to the next planning phase
func (p *PlannerView) advanceToNextPhase() tea.Cmd {
	oldPhase := p.state
	
	switch p.state {
	case StateDefiningRequirement:
		p.state = StateRefiningRequirement
	case StateRefiningRequirement:
		p.state = StateDecomposingTasks
	case StateDecomposingTasks:
		p.state = StateReviewingPlan
	}
	
	if oldPhase != p.state {
		p.feedback = append(p.feedback, FeedbackEntry{
			Type:      "system",
			Content:   fmt.Sprintf("📋 Advanced to: %s", p.getStateDescription()),
			Timestamp: time.Now(),
		})
		
		// Notify planner of phase change
		message := fmt.Sprintf("[planner-tui phase=%s]\nUser signaled completion. Moving to next phase.", p.getPhaseString())
		return p.sendToPlanner(message)
	}
	
	return nil
}

// completeCurrentPhase handles user signaling completion
func (p *PlannerView) completeCurrentPhase() tea.Cmd {
	switch p.state {
	case StateDefiningRequirement, StateRefiningRequirement:
		// Request planner to finalize requirements
		msg := "[planner-tui phase=architecture]\nUser indicated requirements are complete. Please proceed to architecture phase and create the operational specification."
		return p.sendToPlanner(msg)
		
	case StateDecomposingTasks:
		// Request planner to finalize plan
		msg := "[planner-tui phase=ready_for_review]\nUser indicated planning is complete. Please finalize the plan and output as JSON."
		return p.sendToPlanner(msg)
	}
	
	return nil
}

// getPhaseString returns the phase name for the planner agent
func (p *PlannerView) getPhaseString() string {
	switch p.state {
	case StateDefiningRequirement:
		return "clarifying"
	case StateRefiningRequirement:
		return "clarifying"
	case StateDecomposingTasks:
		return "draft_plan"
	case StateReviewingPlan, StatePlanLocked:
		return "ready_for_review"
	default:
		return "clarifying"
	}
}

// getStateDescription returns human-readable state description
func (p *PlannerView) getStateDescription() string {
	switch p.state {
	case StateDefiningRequirement:
		return "Defining Requirements"
	case StateRefiningRequirement:
		return "Refining Requirements"
	case StateDecomposingTasks:
		return "Decomposing into Tasks"
	case StateReviewingPlan:
		return "Reviewing Plan"
	case StatePlanLocked:
		return "Plan Locked"
	case StateExecuting:
		return "Executing Plan"
	default:
		return "Unknown State"
	}
}

// Proactive UX enhancements
func (p *PlannerView) surfacePendingQuestions() {
	if len(p.pendingQuestions) == 0 {
		return
	}
	
	p.feedback = append(p.feedback, FeedbackEntry{
		Type:      "system",
		Content:   "📌 Pending questions need your attention:",
		Timestamp: time.Now(),
	})
	
	for i, q := range p.pendingQuestions {
		p.feedback = append(p.feedback, FeedbackEntry{
			Type:      "system",
			Content:   fmt.Sprintf("  %d. %s", i+1, q),
			Timestamp: time.Now(),
		})
	}
	
	p.pendingQuestions = []string{} // Clear after showing
}

// Enhanced status display
func (p *PlannerView) getDetailedStatus() string {
	status := p.getStateDescription()
	
	if p.requirement != nil {
		status += fmt.Sprintf(" (v%d)", p.requirement.Iteration)
	}
	
	if len(p.tasks) > 0 {
		completed := 0
		inProgress := 0
		for _, t := range p.tasks {
			switch t.Status {
			case TaskStatusCompleted:
				completed++
			case TaskStatusInProgress:
				inProgress++
			}
		}
		status += fmt.Sprintf(" | Progress: %d/%d tasks", completed, len(p.tasks))
		if inProgress > 0 {
			status += fmt.Sprintf(" (%d in progress)", inProgress)
		}
	}
	
	if p.currentGate != nil {
		status += fmt.Sprintf(" | ⚠️ Gate: %s", p.currentGate.Name)
	}
	
	return status
}

// Socratic dialogue for brainstorming
func (p *PlannerView) conductSocraticDialogue() tea.Cmd {
	if len(p.brainstormThemes) == 0 {
		return nil
	}
	
	// Get next theme
	theme := p.brainstormThemes[0]
	
	// Ask questions for this theme
	questions := strings.Join(theme.Questions, "\n")
	msg := fmt.Sprintf("[planner-tui brainstorm=%s]\nLet's discuss %s:\n%s", theme.Name, theme.Name, questions)
	
	// Remove this theme from queue
	if len(p.brainstormThemes) > 1 {
		p.brainstormThemes = p.brainstormThemes[1:]
	} else {
		p.brainstormThemes = []BrainstormTheme{}
	}
	
	return p.sendToPlanner(msg)
}

// Smart suggestion system
func (p *PlannerView) getContextualSuggestions() []string {
	suggestions := []string{}
	
	switch p.state {
	case StateDefiningRequirement:
		suggestions = append(suggestions, 
			"Try describing: what problem you're solving",
			"Include: target users and use cases",
			"Consider: technical constraints",
		)
		
	case StateRefiningRequirement:
		suggestions = append(suggestions,
			"Say 'done' when requirements are complete",
			"Ask questions if anything is unclear",
			"Type 'approve' to move forward",
		)
		
	case StateReviewingPlan:
		suggestions = append(suggestions,
			"Review tasks and waves carefully",
			"Say 'approve' to dispatch the plan",
			"Provide feedback for changes",
			"Press ^p to pull plan as JSON",
		)
	}
	
	return suggestions
}

// Initialize enhanced planner view fields
func (p *PlannerView) initializeEnhancements() {
	if p.context == nil {
		p.context = initPlannerContext()
	}
	if p.pendingQuestions == nil {
		p.pendingQuestions = []string{}
	}
	if p.brainstormThemes == nil {
		p.brainstormThemes = initBrainstormThemes()
	}
}