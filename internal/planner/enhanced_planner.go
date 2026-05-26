package planner

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams-8/internal/factory"
	"github.com/Root-IO-Labs/open-agent-teams-8/internal/state"
)

// EnhancedPlanner uses the agent factory to create specialized agents
type EnhancedPlanner struct {
	factory      factory.AgentFactory
	selector     factory.AgentSelector
	registry     factory.TemplateRegistry
	state        *state.State
	repository   string
	codeIndexer  CodeIndexer // Interface for code intelligence
}

// CodeIndexer provides code intelligence (interface to avoid circular deps)
type CodeIndexer interface {
	AnalyzeRepository(path string) (*CodeAnalysis, error)
	GetImpactRadius(files []string) map[string][]string
	GetCommunities() []Community
}

type CodeAnalysis struct {
	TotalFiles   int
	Communities  []Community
	KeyFiles     []string
	Dependencies map[string][]string
}

type Community struct {
	Name  string
	Files []string
	Type  string // frontend, backend, testing, infrastructure
}

// NewEnhancedPlanner creates a planner with factory integration
func NewEnhancedPlanner(f factory.AgentFactory, s *state.State, repo string) *EnhancedPlanner {
	registry := factory.NewTemplateRegistry()
	registry.LoadBuiltinTemplates()
	
	// Try to load from agent-blueprints repo
	registry.FetchFromRegistry("https://raw.githubusercontent.com/oat-agent/agent-blueprints/main")
	
	return &EnhancedPlanner{
		factory:    f,
		selector:   factory.NewAgentSelector(registry, f),
		registry:   registry,
		state:      s,
		repository: repo,
	}
}

// PlanAndExecute analyzes a requirement and creates specialized agents
func (p *EnhancedPlanner) PlanAndExecute(ctx context.Context, requirement string) (*ExecutionPlan, error) {
	// Phase 1: Analyze the requirement
	reqAnalysis, err := p.analyzeRequirement(requirement)
	if err != nil {
		return nil, fmt.Errorf("requirement analysis failed: %w", err)
	}
	
	// Phase 2: Decompose into tasks
	tasks, err := p.decomposeTasks(reqAnalysis)
	if err != nil {
		return nil, fmt.Errorf("task decomposition failed: %w", err)
	}
	
	// Phase 3: Select specialized agents for each task
	agentAssignments, err := p.assignSpecializedAgents(ctx, tasks)
	if err != nil {
		return nil, fmt.Errorf("agent assignment failed: %w", err)
	}
	
	// Phase 4: Create execution waves based on dependencies
	waves := p.createExecutionWaves(agentAssignments)
	
	// Phase 5: Execute the plan
	plan := &ExecutionPlan{
		Requirement:  requirement,
		Analysis:     reqAnalysis,
		Tasks:        tasks,
		Assignments:  agentAssignments,
		Waves:        waves,
		Status:       PlanStatusReady,
		CreatedAt:    time.Now(),
	}
	
	// Start execution
	if err := p.executePlan(ctx, plan); err != nil {
		return plan, fmt.Errorf("execution failed: %w", err)
	}
	
	return plan, nil
}

// SpawnSpecializedAgent creates the right agent for a given task
func (p *EnhancedPlanner) SpawnSpecializedAgent(ctx context.Context, task string) (*factory.Agent, error) {
	// Analyze the task
	analysis, err := p.selector.AnalyzeTask(task)
	if err != nil {
		return nil, fmt.Errorf("task analysis failed: %w", err)
	}
	
	// Select the best agent template
	template, err := p.selector.SelectAgent(ctx, task, analysis)
	if err != nil {
		// Fall back to standard worker
		return p.spawnStandardWorker(ctx, task)
	}
	
	// Create the specialized agent
	agent, err := p.factory.CreateAgent(ctx, &factory.CreateAgentRequest{
		Name:       generateAgentName(template.Metadata.Name),
		Template:   template.Metadata.Name,
		Task:       task,
		Repository: p.repository,
		Parameters: map[string]interface{}{
			"task_analysis": analysis,
			"repository":    p.repository,
			"task":          task,
		},
	})
	
	if err != nil {
		// Fall back to standard worker if specialized agent fails
		return p.spawnStandardWorker(ctx, task)
	}
	
	return agent, nil
}

func (p *EnhancedPlanner) analyzeRequirement(requirement string) (*RequirementAnalysis, error) {
	analysis := &RequirementAnalysis{
		Requirement: requirement,
		Timestamp:   time.Now(),
	}
	
	// Analyze complexity
	analysis.Complexity = p.estimateComplexity(requirement)
	
	// Detect affected areas
	analysis.AffectedAreas = p.detectAffectedAreas(requirement)
	
	// Identify required capabilities
	analysis.RequiredCapabilities = p.identifyRequiredCapabilities(requirement)
	
	// Estimate effort
	analysis.EstimatedEffort = p.estimateEffort(analysis.Complexity)
	
	// Identify risks
	analysis.Risks = p.identifyRisks(requirement)
	
	// If code indexer is available, use it
	if p.codeIndexer != nil {
		codeAnalysis, err := p.codeIndexer.AnalyzeRepository(p.repository)
		if err == nil {
			analysis.CodeAnalysis = codeAnalysis
			analysis.AffectedCommunities = p.findAffectedCommunities(requirement, codeAnalysis)
		}
	}
	
	return analysis, nil
}

func (p *EnhancedPlanner) decomposeTasks(analysis *RequirementAnalysis) ([]*Task, error) {
	var tasks []*Task
	
	// Create tasks based on affected areas
	for _, area := range analysis.AffectedAreas {
		task := p.createTaskForArea(area, analysis)
		tasks = append(tasks, task)
	}
	
	// Add specialized tasks based on capabilities
	if contains(analysis.RequiredCapabilities, "security") {
		tasks = append(tasks, &Task{
			ID:          generateTaskID(),
			Type:        "security-audit",
			Description: fmt.Sprintf("Security audit for %s", analysis.Requirement),
			Priority:    PriorityHigh,
			Template:    "security-auditor",
		})
	}
	
	if contains(analysis.RequiredCapabilities, "performance") {
		tasks = append(tasks, &Task{
			ID:          generateTaskID(),
			Type:        "performance-analysis",
			Description: fmt.Sprintf("Performance analysis for %s", analysis.Requirement),
			Priority:    PriorityMedium,
			Template:    "performance-profiler",
		})
	}
	
	if contains(analysis.RequiredCapabilities, "database") {
		tasks = append(tasks, &Task{
			ID:          generateTaskID(),
			Type:        "database-migration",
			Description: fmt.Sprintf("Database changes for %s", analysis.Requirement),
			Priority:    PriorityHigh,
			Template:    "database-migrator",
		})
	}
	
	// Add testing task if needed
	if analysis.Complexity != ComplexityLow {
		tasks = append(tasks, &Task{
			ID:          generateTaskID(),
			Type:        "testing",
			Description: fmt.Sprintf("Write tests for %s", analysis.Requirement),
			Priority:    PriorityMedium,
			Template:    "integration-tester",
		})
	}
	
	// Add documentation task for complex changes
	if analysis.Complexity == ComplexityHigh {
		tasks = append(tasks, &Task{
			ID:          generateTaskID(),
			Type:        "documentation",
			Description: fmt.Sprintf("Document %s", analysis.Requirement),
			Priority:    PriorityLow,
			Template:    "api-documenter",
		})
	}
	
	// Establish dependencies
	p.establishTaskDependencies(tasks)
	
	return tasks, nil
}

func (p *EnhancedPlanner) assignSpecializedAgents(ctx context.Context, tasks []*Task) ([]*AgentAssignment, error) {
	var assignments []*AgentAssignment
	
	for _, task := range tasks {
		// If task specifies a template, use it
		var template *factory.AgentTemplate
		var err error
		
		if task.Template != "" {
			template, err = p.registry.GetTemplate(task.Template)
			if err != nil {
				// Fall back to selection
				template, err = p.selector.SelectAgent(ctx, task.Description, nil)
			}
		} else {
			// Let selector choose
			template, err = p.selector.SelectAgent(ctx, task.Description, nil)
		}
		
		if err != nil {
			// Use default worker
			template = p.getDefaultWorkerTemplate()
		}
		
		assignment := &AgentAssignment{
			TaskID:       task.ID,
			Task:         task,
			AgentTemplate: template.Metadata.Name,
			Capabilities: p.extractTemplateCapabilities(template),
			Status:       AssignmentStatusPending,
		}
		
		assignments = append(assignments, assignment)
	}
	
	return assignments, nil
}

func (p *EnhancedPlanner) createExecutionWaves(assignments []*AgentAssignment) []*ExecutionWave {
	// Group tasks by dependencies into waves
	waves := make(map[int][]*AgentAssignment)
	maxWave := 0
	
	for _, assignment := range assignments {
		wave := p.calculateWaveNumber(assignment, assignments)
		waves[wave] = append(waves[wave], assignment)
		if wave > maxWave {
			maxWave = wave
		}
	}
	
	// Convert to slice
	var result []*ExecutionWave
	for i := 0; i <= maxWave; i++ {
		if assignments, ok := waves[i]; ok {
			result = append(result, &ExecutionWave{
				Number:      i,
				Assignments: assignments,
				Status:      WaveStatusPending,
			})
		}
	}
	
	return result
}

func (p *EnhancedPlanner) executePlan(ctx context.Context, plan *ExecutionPlan) error {
	plan.Status = PlanStatusExecuting
	
	for _, wave := range plan.Waves {
		// Execute wave in parallel
		if err := p.executeWave(ctx, wave); err != nil {
			plan.Status = PlanStatusFailed
			return fmt.Errorf("wave %d failed: %w", wave.Number, err)
		}
		
		// Wait for wave completion before proceeding
		if err := p.waitForWaveCompletion(ctx, wave); err != nil {
			plan.Status = PlanStatusFailed
			return fmt.Errorf("wave %d completion failed: %w", wave.Number, err)
		}
	}
	
	plan.Status = PlanStatusCompleted
	plan.CompletedAt = &[]time.Time{time.Now()}[0]
	
	return nil
}

func (p *EnhancedPlanner) executeWave(ctx context.Context, wave *ExecutionWave) error {
	wave.Status = WaveStatusExecuting
	wave.StartedAt = &[]time.Time{time.Now()}[0]
	
	// Launch agents in parallel
	for _, assignment := range wave.Assignments {
		go func(a *AgentAssignment) {
			agent, err := p.factory.CreateAgent(ctx, &factory.CreateAgentRequest{
				Name:       generateAgentName(a.AgentTemplate),
				Template:   a.AgentTemplate,
				Task:       a.Task.Description,
				Repository: p.repository,
			})
			
			if err != nil {
				a.Status = AssignmentStatusFailed
				a.Error = err.Error()
				return
			}
			
			a.AgentID = agent.ID
			a.AgentName = agent.Name
			a.Status = AssignmentStatusRunning
			a.StartedAt = &[]time.Time{time.Now()}[0]
		}(assignment)
	}
	
	return nil
}

func (p *EnhancedPlanner) waitForWaveCompletion(ctx context.Context, wave *ExecutionWave) error {
	// Poll for completion
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	
	timeout := time.After(30 * time.Minute)
	
	for {
		select {
		case <-ticker.C:
			if p.isWaveComplete(wave) {
				wave.Status = WaveStatusCompleted
				wave.CompletedAt = &[]time.Time{time.Now()}[0]
				return nil
			}
		case <-timeout:
			return fmt.Errorf("wave %d timed out", wave.Number)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (p *EnhancedPlanner) isWaveComplete(wave *ExecutionWave) bool {
	for _, assignment := range wave.Assignments {
		if assignment.Status != AssignmentStatusCompleted && 
		   assignment.Status != AssignmentStatusFailed {
			return false
		}
	}
	return true
}

func (p *EnhancedPlanner) spawnStandardWorker(ctx context.Context, task string) (*factory.Agent, error) {
	return p.factory.CreateAgent(ctx, &factory.CreateAgentRequest{
		Name:       generateAgentName("worker"),
		Template:   "worker",
		Task:       task,
		Repository: p.repository,
	})
}

// Helper functions

func (p *EnhancedPlanner) estimateComplexity(requirement string) ComplexityLevel {
	// Similar to selector's complexity estimation
	wordCount := len(strings.Fields(requirement))
	
	complexIndicators := []string{"architecture", "refactor", "migrate", "overhaul", "redesign"}
	complexCount := 0
	
	reqLower := strings.ToLower(requirement)
	for _, indicator := range complexIndicators {
		if strings.Contains(reqLower, indicator) {
			complexCount++
		}
	}
	
	if complexCount >= 2 || wordCount > 100 {
		return ComplexityHigh
	} else if complexCount >= 1 || wordCount > 50 {
		return ComplexityMedium
	}
	
	return ComplexityLow
}

func (p *EnhancedPlanner) detectAffectedAreas(requirement string) []string {
	var areas []string
	reqLower := strings.ToLower(requirement)
	
	areaKeywords := map[string][]string{
		"authentication": {"auth", "login", "session", "jwt", "oauth"},
		"database":       {"database", "db", "sql", "schema", "migration"},
		"api":            {"api", "endpoint", "rest", "graphql", "route"},
		"frontend":       {"ui", "frontend", "react", "vue", "component"},
		"backend":        {"backend", "server", "service", "handler"},
		"testing":        {"test", "spec", "coverage", "mock"},
		"infrastructure": {"docker", "kubernetes", "deploy", "ci/cd"},
	}
	
	for area, keywords := range areaKeywords {
		for _, keyword := range keywords {
			if strings.Contains(reqLower, keyword) {
				areas = append(areas, area)
				break
			}
		}
	}
	
	if len(areas) == 0 {
		areas = append(areas, "general")
	}
	
	return areas
}

func (p *EnhancedPlanner) identifyRequiredCapabilities(requirement string) []string {
	var capabilities []string
	reqLower := strings.ToLower(requirement)
	
	if strings.Contains(reqLower, "security") || strings.Contains(reqLower, "vulnerab") {
		capabilities = append(capabilities, "security")
	}
	
	if strings.Contains(reqLower, "performance") || strings.Contains(reqLower, "optimize") {
		capabilities = append(capabilities, "performance")
	}
	
	if strings.Contains(reqLower, "database") || strings.Contains(reqLower, "migrat") {
		capabilities = append(capabilities, "database")
	}
	
	if strings.Contains(reqLower, "test") {
		capabilities = append(capabilities, "testing")
	}
	
	if strings.Contains(reqLower, "document") || strings.Contains(reqLower, "api doc") {
		capabilities = append(capabilities, "documentation")
	}
	
	return capabilities
}

func (p *EnhancedPlanner) estimateEffort(complexity ComplexityLevel) string {
	switch complexity {
	case ComplexityHigh:
		return "3-5 days"
	case ComplexityMedium:
		return "1-2 days"
	case ComplexityLow:
		return "2-4 hours"
	default:
		return "unknown"
	}
}

func (p *EnhancedPlanner) identifyRisks(requirement string) []string {
	var risks []string
	reqLower := strings.ToLower(requirement)
	
	if strings.Contains(reqLower, "migration") || strings.Contains(reqLower, "database") {
		risks = append(risks, "Data loss risk - ensure backups")
	}
	
	if strings.Contains(reqLower, "auth") || strings.Contains(reqLower, "security") {
		risks = append(risks, "Security implications - thorough testing required")
	}
	
	if strings.Contains(reqLower, "performance") {
		risks = append(risks, "Performance degradation - benchmark before/after")
	}
	
	if strings.Contains(reqLower, "refactor") {
		risks = append(risks, "Breaking changes - comprehensive test coverage needed")
	}
	
	return risks
}

func (p *EnhancedPlanner) findAffectedCommunities(requirement string, codeAnalysis *CodeAnalysis) []string {
	// This would use code intelligence to identify affected code communities
	var communities []string
	
	for _, community := range codeAnalysis.Communities {
		// Simple keyword matching for now
		if strings.Contains(strings.ToLower(requirement), strings.ToLower(community.Type)) {
			communities = append(communities, community.Name)
		}
	}
	
	return communities
}

func (p *EnhancedPlanner) createTaskForArea(area string, analysis *RequirementAnalysis) *Task {
	return &Task{
		ID:          generateTaskID(),
		Type:        "implementation",
		Description: fmt.Sprintf("Implement %s changes for: %s", area, analysis.Requirement),
		Priority:    PriorityMedium,
		Area:        area,
	}
}

func (p *EnhancedPlanner) establishTaskDependencies(tasks []*Task) {
	// Database migrations should happen first
	for _, task := range tasks {
		if task.Type == "database-migration" {
			for _, other := range tasks {
				if other.Type == "implementation" {
					other.Dependencies = append(other.Dependencies, task.ID)
				}
			}
		}
	}
	
	// Testing depends on implementation
	for _, task := range tasks {
		if task.Type == "testing" {
			for _, other := range tasks {
				if other.Type == "implementation" {
					task.Dependencies = append(task.Dependencies, other.ID)
				}
			}
		}
	}
	
	// Documentation comes last
	for _, task := range tasks {
		if task.Type == "documentation" {
			for _, other := range tasks {
				if other.Type != "documentation" {
					task.Dependencies = append(task.Dependencies, other.ID)
				}
			}
		}
	}
}

func (p *EnhancedPlanner) calculateWaveNumber(assignment *AgentAssignment, all []*AgentAssignment) int {
	// Calculate based on dependencies
	maxDepWave := 0
	
	for _, depID := range assignment.Task.Dependencies {
		for _, other := range all {
			if other.TaskID == depID {
				depWave := p.calculateWaveNumber(other, all)
				if depWave >= maxDepWave {
					maxDepWave = depWave + 1
				}
			}
		}
	}
	
	return maxDepWave
}

func (p *EnhancedPlanner) extractTemplateCapabilities(template *factory.AgentTemplate) []string {
	var caps []string
	
	for _, tool := range template.Spec.Capabilities.Tools {
		caps = append(caps, fmt.Sprintf("tool:%s", tool.Name))
	}
	
	for _, api := range template.Spec.Capabilities.APIs {
		caps = append(caps, fmt.Sprintf("api:%s", api))
	}
	
	return caps
}

func (p *EnhancedPlanner) getDefaultWorkerTemplate() *factory.AgentTemplate {
	template, _ := p.registry.GetTemplate("worker")
	if template == nil {
		// Return minimal template
		return &factory.AgentTemplate{
			Metadata: factory.TemplateMetadata{
				Name: "worker",
			},
		}
	}
	return template
}

// Utility functions

func generateAgentName(templateName string) string {
	// Generate unique name based on template
	return fmt.Sprintf("%s-%d", templateName, time.Now().Unix())
}

func generateTaskID() string {
	return fmt.Sprintf("task-%d", time.Now().UnixNano())
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}