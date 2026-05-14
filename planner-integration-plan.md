# OAT Planner Enhancement: Overlord Integration Plan

## Executive Summary

After deep analysis of the Overlord-V1 repository, this plan outlines critical enhancements to integrate Overlord's proven methodologies into the OAT planner. The focus is on **contextual awareness**, **explicit phase gates**, **strong spec generation**, **perfect UX flow**, and **multi-agent task profiling**.

## Critical Issues to Address

### 1. **Lack of Contextual Awareness**
**Current Problem**: OAT planner doesn't recognize when user says "I'm done" or provides approval cues
**Overlord Solution**: Explicit phase gates with clear approval language requirements
**Implementation**: Add contextual state machine that recognizes approval patterns

### 2. **Weak Spec Generation**
**Current Problem**: Specs are not strong enough to produce quality output
**Overlord Solution**: Interactive brainstorming, document internalization, operational specs
**Implementation**: Multi-phase spec refinement with validation loops

### 3. **Poor UX Flow**
**Current Problem**: User must explicitly trigger phase transitions
**Overlord Solution**: Proactive pending questions, automatic phase progression
**Implementation**: Enhanced state machine with contextual triggers

### 4. **No Task Profiling**
**Current Problem**: Tasks don't specify which agents should execute them
**Overlord Solution**: Task metadata includes agent assignment, dependencies, wave organization
**Implementation**: Enhanced task structure with agent profiles

## Core Integration Components

### Phase 1: Enhanced State Machine

```go
// Add to planner_view.go
type PlannerContext struct {
    ApprovalPatterns   []string // "looks good", "yes", "approve", "let's go", "I'm done"
    RejectionPatterns  []string // "no", "change", "update", "not quite"
    QuestionPatterns   []string // "?", "what", "how", "should"
    CompletionSignals  []string // "done", "finished", "complete", "ready"
}

func (p *PlannerView) detectContextualIntent(input string) ContextIntent {
    normalized := strings.ToLower(strings.TrimSpace(input))
    
    // Check for approval signals
    for _, pattern := range p.context.ApprovalPatterns {
        if strings.Contains(normalized, pattern) {
            return IntentApproval
        }
    }
    
    // Check for completion signals
    for _, pattern := range p.context.CompletionSignals {
        if strings.Contains(normalized, pattern) {
            return IntentCompletion
        }
    }
    
    // Check for questions
    for _, pattern := range p.context.QuestionPatterns {
        if strings.Contains(normalized, pattern) {
            return IntentClarification
        }
    }
    
    return IntentFeedback
}
```

### Phase 2: Overlord-Style Phase Gates

```go
// Enhanced phase transitions with explicit gates
type PhaseGate struct {
    ID               string
    Name             string
    RequiredElements []string // What must be present
    ApprovalPrompt   string   // What to ask user
    ValidationFunc   func(*PlannerView) bool
}

var plannerGates = []PhaseGate{
    {
        ID:   "gate_1_requirements",
        Name: "Requirements Clarity Gate",
        RequiredElements: []string{
            "clear_scope",
            "success_criteria", 
            "technical_constraints",
            "testing_requirements",
        },
        ApprovalPrompt: "Requirements are clear. Shall I proceed to architecture design? Say 'yes' to continue.",
        ValidationFunc: validateRequirementsComplete,
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
        ValidationFunc: validateArchitectureComplete,
    },
    {
        ID:   "gate_3_plan",
        Name: "Plan Approval Gate",
        RequiredElements: []string{
            "wave_organization",
            "task_dependencies",
            "acceptance_criteria",
            "agent_assignments",
        },
        ApprovalPrompt: "Plan is ready with %d tasks in %d waves. Type 'approve' to dispatch or provide feedback.",
        ValidationFunc: validatePlanComplete,
    },
}
```

### Phase 3: Interactive Brainstorming Integration

```go
// Socratic questioning system
type BrainstormTheme struct {
    Name      string
    Questions []string
    Validator func(string) bool
}

var brainstormThemes = []BrainstormTheme{
    {
        Name: "Tech Stack",
        Questions: []string{
            "What programming language should we use?",
            "Do you need a web framework? Which one?",
            "What database system fits your needs?",
            "Any specific libraries or tools required?",
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
}

func (p *PlannerView) conductSocraticDialogue() {
    for _, theme := range brainstormThemes {
        for _, question := range theme.Questions {
            p.askAndValidate(question, theme.Validator)
        }
        p.presentDesignSection(theme.Name) // 200-300 words
        p.waitForValidation()
    }
}
```

### Phase 4: Strong Spec Generation

```go
// Operational specification structure
type OperationalSpec struct {
    Title            string
    ExecutiveSummary string
    SystemBehavior   string // How the system works
    InterfaceContracts []InterfaceContract
    TestStrategy     TestStrategy
    GateMechanism    string // check.sh equivalent
    
    // Overlord methodology
    SpecSections map[string]SpecSection
}

type SpecSection struct {
    Content   string
    Validated bool
    Emphasis  []string // Critical points
    TestRefs  []string // Related test specifications
}

func (p *PlannerView) generateStrongSpec() *OperationalSpec {
    spec := &OperationalSpec{
        Title: p.requirement.Title,
        SpecSections: make(map[string]SpecSection),
    }
    
    // Document internalization - read relevant patterns
    patterns := p.internalizeDocumentPatterns()
    
    // Generate each section with proper emphasis
    for _, section := range requiredSpecSections {
        content := p.generateSpecSection(section, patterns)
        spec.SpecSections[section] = SpecSection{
            Content:   content,
            Validated: false,
            Emphasis:  p.identifyCriticalPoints(content),
        }
        
        // Present and validate each section
        p.presentSection(section, content)
        if p.waitForValidation() {
            spec.SpecSections[section].Validated = true
        }
    }
    
    return spec
}
```

### Phase 5: Task Profiling and Agent Assignment

```go
// Enhanced task structure with agent profiling
type ProfiledTask struct {
    Task
    
    // Agent profiling
    AssignedAgents   []string // Can be multiple agents
    AgentCapabilities []string // Required capabilities
    EstimatedEffort  string   // T-shirt sizing: S, M, L, XL
    Complexity       string   // Simple, Moderate, Complex
    
    // Execution metadata
    ExecutionProfile ExecutionProfile
    ResourceNeeds    []string
    Parallelizable   bool
}

type ExecutionProfile struct {
    RequiresFileAccess   bool
    RequiresNetworkAccess bool
    RequiresUserInput    bool
    CanRunHeadless       bool
}

func (p *PlannerView) profileTask(task *Task) *ProfiledTask {
    profiled := &ProfiledTask{
        Task: *task,
    }
    
    // Analyze task type and assign agents
    switch task.Type {
    case "test":
        profiled.AssignedAgents = []string{"test-writer", "test-runner"}
        profiled.AgentCapabilities = []string{"testing", "validation"}
    case "implementation":
        if strings.Contains(task.Description, "API") {
            profiled.AssignedAgents = []string{"backend-developer"}
        } else if strings.Contains(task.Description, "UI") {
            profiled.AssignedAgents = []string{"frontend-developer"}
        } else {
            profiled.AssignedAgents = []string{"full-stack-developer"}
        }
    case "documentation":
        profiled.AssignedAgents = []string{"technical-writer"}
    }
    
    // Estimate complexity
    profiled.Complexity = p.estimateComplexity(task)
    profiled.EstimatedEffort = p.estimateEffort(task)
    
    return profiled
}
```

### Phase 6: Perfect UX Flow

```go
// Proactive status and contextual responses
type UXEnhancement struct {
    ProactiveQuestions bool
    StatusStream       bool
    ContextualHelp     bool
    SlashCommands      map[string]func()
}

func (p *PlannerView) enhanceUX() {
    // Proactive pending questions
    if len(p.pendingQuestions) > 0 {
        p.surfacePendingQuestions()
    }
    
    // Status stream during execution
    if p.state == StateExecuting {
        go p.streamExecutionStatus()
    }
    
    // Contextual help based on state
    p.showContextualHelp()
    
    // Slash commands
    p.registerSlashCommands(map[string]func(){
        "/status": p.showStatus,
        "/plan":   p.showCurrentPlan,
        "/tasks":  p.showTaskBreakdown,
        "/waves":  p.showWaveOrganization,
        "/help":   p.showHelp,
    })
}

func (p *PlannerView) surfacePendingQuestions() {
    for _, q := range p.pendingQuestions {
        p.feedback = append(p.feedback, FeedbackEntry{
            Type:    "system",
            Content: fmt.Sprintf("📌 Pending: %s", q),
            Timestamp: time.Now(),
        })
    }
}
```

## Implementation Plan

### Week 1: Core State Machine Enhancement
1. Implement contextual intent detection
2. Add phase gate system
3. Create approval pattern matching
4. Test with various user inputs

### Week 2: Spec Generation Improvements
1. Add operational spec structure
2. Implement document internalization
3. Create section validation loops
4. Add test strategy generation

### Week 3: Interactive Brainstorming
1. Implement Socratic dialogue system
2. Add theme-based questioning
3. Create design section presentation
4. Add validation checkpoints

### Week 4: Task Profiling
1. Enhance task structure with profiles
2. Implement agent assignment logic
3. Add complexity estimation
4. Create execution profiles

### Week 5: UX Polish
1. Add proactive question surfacing
2. Implement status streaming
3. Add contextual help system
4. Create slash commands

### Week 6: Testing and Integration
1. End-to-end testing with real scenarios
2. Performance optimization
3. Documentation updates
4. Rollout preparation

## Test Scenarios

### Scenario 1: Quick Approval Flow
```
User: "Build a REST API for user management"
Planner: [Clarifying questions]
User: "Python, FastAPI, PostgreSQL"
Planner: [Presents architecture]
User: "Looks good"
Planner: [Detects approval, moves to next phase]
User: "I'm done with the requirements"
Planner: [Recognizes completion signal, finalizes plan]
```

### Scenario 2: Multi-Agent Task
```
Task: "Implement authentication system"
Profiling:
- Agents: ["backend-dev", "security-auditor", "test-writer"]
- Complexity: Complex
- Effort: XL
- Dependencies: ["database-schema", "user-model"]
```

### Scenario 3: Strong Spec Generation
```
Input: "Todo app with authentication"
Output:
- Operational Spec: 2000+ words
- Test Strategy: 4 layers (unit, integration, e2e, security)
- Interface Contracts: REST API, WebSocket events
- Gate Mechanism: ./scripts/check.sh with full validation
```

## Success Metrics

1. **Contextual Recognition**: 95% accuracy in detecting user intent
2. **Spec Quality**: Specs produce working implementations 80% of the time
3. **UX Satisfaction**: Users report smooth flow without manual phase triggers
4. **Task Success**: 90% of profiled tasks completed by assigned agents
5. **Time to Plan**: 50% reduction in planning time with better flow

## Risk Mitigation

1. **Over-automation**: Keep manual override options
2. **False positives**: Confirmation prompts for critical transitions
3. **Spec bloat**: Balance thoroughness with practicality
4. **Agent overload**: Load balancing for multi-agent tasks

## Rollout Strategy

1. **Alpha**: Internal testing with team
2. **Beta**: Select users with feedback loops
3. **GA**: Full rollout with monitoring

## Conclusion

This integration brings Overlord's battle-tested methodologies to OAT, creating a planner that:
- Understands context and user intent
- Generates specifications strong enough for quality output
- Provides perfect UX with proactive assistance
- Profiles tasks for optimal agent assignment
- Follows proven phase gates and validation patterns

The result will be a planner that truly understands when users are done, produces actionable specifications, and orchestrates multi-agent execution effectively.