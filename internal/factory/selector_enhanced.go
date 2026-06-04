package factory

import (
	"context"
	"fmt"
	"strings"
)

// EnhancedAgentMatcher provides improved matching for blueprints templates
type EnhancedAgentMatcher struct {
	selector *agentSelector
}

// NewEnhancedAgentMatcher creates a matcher optimized for agent-blueprints templates
func NewEnhancedAgentMatcher(registry TemplateRegistry, factory AgentFactory) *EnhancedAgentMatcher {
	return &EnhancedAgentMatcher{
		selector: &agentSelector{
			registry: registry,
			factory:  factory,
		},
	}
}

// MatchTaskToBlueprint finds the best blueprint agent for a task
func (m *EnhancedAgentMatcher) MatchTaskToBlueprint(ctx context.Context, task string) (*AgentTemplate, float32, error) {
	analysis, err := m.selector.AnalyzeTask(task)
	if err != nil {
		return nil, 0, err
	}
	
	// Enhanced matching for blueprint categories
	bestMatch := m.findBestBlueprintMatch(analysis)
	
	if bestMatch.template != nil {
		return bestMatch.template, bestMatch.score, nil
	}
	
	// Fall back to standard selection
	template, err := m.selector.SelectAgent(ctx, task, analysis)
	return template, 0.5, err
}

type blueprintMatch struct {
	template *AgentTemplate
	score    float32
	reason   string
}

func (m *EnhancedAgentMatcher) findBestBlueprintMatch(analysis *TaskAnalysis) blueprintMatch {
	taskLower := strings.ToLower(analysis.OriginalTask)
	
	// UI/Frontend matches
	if strings.Contains(taskLower, "component") || strings.Contains(taskLower, "react") || 
	   strings.Contains(taskLower, "vue") || strings.Contains(taskLower, "angular") {
		if strings.Contains(taskLower, "accessible") || strings.Contains(taskLower, "a11y") {
			return m.tryLoadTemplate("accessibility-auditor", 0.9, "UI accessibility task")
		}
		if strings.Contains(taskLower, "responsive") {
			return m.tryLoadTemplate("responsive-optimizer", 0.85, "Responsive UI task")
		}
		return m.tryLoadTemplate("component-builder", 0.8, "UI component task")
	}
	
	// SPA/Frontend app matches
	if strings.Contains(taskLower, "spa") || strings.Contains(taskLower, "single page") ||
	   strings.Contains(taskLower, "frontend app") {
		return m.tryLoadTemplate("spa-developer", 0.9, "SPA development task")
	}
	
	// PWA matches
	if strings.Contains(taskLower, "pwa") || strings.Contains(taskLower, "progressive web") {
		return m.tryLoadTemplate("pwa-builder", 0.9, "PWA development task")
	}
	
	// Backend/API matches
	if strings.Contains(taskLower, "api") || strings.Contains(taskLower, "endpoint") {
		if strings.Contains(taskLower, "rest") || strings.Contains(taskLower, "graphql") {
			return m.tryLoadTemplate("api-builder", 0.9, "API development task")
		}
		if strings.Contains(taskLower, "microservice") {
			return m.tryLoadTemplate("microservice-architect", 0.9, "Microservice task")
		}
	}
	
	// Authentication matches
	if strings.Contains(taskLower, "auth") || strings.Contains(taskLower, "login") ||
	   strings.Contains(taskLower, "jwt") || strings.Contains(taskLower, "oauth") {
		return m.tryLoadTemplate("auth-implementer", 0.9, "Authentication task")
	}
	
	// Queue/messaging matches
	if strings.Contains(taskLower, "queue") || strings.Contains(taskLower, "message") ||
	   strings.Contains(taskLower, "rabbitmq") || strings.Contains(taskLower, "kafka") {
		return m.tryLoadTemplate("queue-processor", 0.85, "Queue processing task")
	}
	
	// Database matches
	if analysis.DatabaseWork {
		if strings.Contains(taskLower, "migration") {
			return m.tryLoadTemplate("database-migrator", 0.9, "Database migration task")
		}
		if strings.Contains(taskLower, "schema") {
			return m.tryLoadTemplate("schema-validator", 0.85, "Schema validation task")
		}
		if strings.Contains(taskLower, "sync") {
			return m.tryLoadTemplate("data-synchronizer", 0.8, "Data sync task")
		}
	}
	
	// Testing matches
	if analysis.TestingNeeded {
		if strings.Contains(taskLower, "integration") {
			return m.tryLoadTemplate("integration-tester", 0.9, "Integration testing")
		}
		if strings.Contains(taskLower, "e2e") || strings.Contains(taskLower, "end-to-end") {
			return m.tryLoadTemplate("e2e-tester", 0.9, "E2E testing")
		}
		if strings.Contains(taskLower, "chaos") {
			return m.tryLoadTemplate("chaos-engineer", 0.85, "Chaos engineering")
		}
	}
	
	// DevOps matches
	if strings.Contains(taskLower, "ci") || strings.Contains(taskLower, "cd") ||
	   strings.Contains(taskLower, "pipeline") {
		return m.tryLoadTemplate("ci-cd-optimizer", 0.9, "CI/CD task")
	}
	
	if strings.Contains(taskLower, "docker") || strings.Contains(taskLower, "container") {
		return m.tryLoadTemplate("docker-builder", 0.9, "Docker/container task")
	}
	
	if strings.Contains(taskLower, "kubernetes") || strings.Contains(taskLower, "k8s") {
		return m.tryLoadTemplate("kubernetes-deployer", 0.9, "Kubernetes deployment")
	}
	
	if strings.Contains(taskLower, "terraform") || strings.Contains(taskLower, "infrastructure as code") {
		return m.tryLoadTemplate("terraform-planner", 0.9, "Infrastructure as code")
	}
	
	// Security matches
	if analysis.SecurityNeeded {
		if strings.Contains(taskLower, "vulnerability") || strings.Contains(taskLower, "scan") {
			return m.tryLoadTemplate("vulnerability-scanner", 0.9, "Security scanning")
		}
		if strings.Contains(taskLower, "compliance") || strings.Contains(taskLower, "audit") {
			return m.tryLoadTemplate("compliance-checker", 0.85, "Compliance audit")
		}
		return m.tryLoadTemplate("security-auditor", 0.85, "Security task")
	}
	
	// Performance matches
	if analysis.PerformanceNeeded {
		if strings.Contains(taskLower, "load") || strings.Contains(taskLower, "stress") {
			return m.tryLoadTemplate("load-tester", 0.9, "Load testing")
		}
		if strings.Contains(taskLower, "benchmark") {
			return m.tryLoadTemplate("benchmark-runner", 0.9, "Benchmarking")
		}
		return m.tryLoadTemplate("performance-profiler", 0.85, "Performance optimization")
	}
	
	// Documentation matches
	if analysis.DocumentationNeeded {
		if strings.Contains(taskLower, "api doc") || strings.Contains(taskLower, "openapi") {
			return m.tryLoadTemplate("api-documenter", 0.9, "API documentation")
		}
		if strings.Contains(taskLower, "readme") {
			return m.tryLoadTemplate("readme-generator", 0.9, "README generation")
		}
		if strings.Contains(taskLower, "changelog") {
			return m.tryLoadTemplate("changelog-maintainer", 0.85, "Changelog maintenance")
		}
	}
	
	// Automation matches
	if strings.Contains(taskLower, "workflow") || strings.Contains(taskLower, "automate") {
		return m.tryLoadTemplate("workflow-automator", 0.85, "Workflow automation")
	}
	
	if strings.Contains(taskLower, "pipeline") && strings.Contains(taskLower, "data") {
		return m.tryLoadTemplate("data-pipeline-builder", 0.9, "Data pipeline")
	}
	
	if strings.Contains(taskLower, "release") || strings.Contains(taskLower, "deploy") {
		return m.tryLoadTemplate("release-manager", 0.85, "Release management")
	}
	
	if strings.Contains(taskLower, "monitor") || strings.Contains(taskLower, "alert") {
		return m.tryLoadTemplate("monitoring-alerter", 0.85, "Monitoring/alerting")
	}
	
	// Data/ML matches
	if strings.Contains(taskLower, "ml") || strings.Contains(taskLower, "machine learning") {
		return m.tryLoadTemplate("ml-pipeline-builder", 0.9, "ML pipeline")
	}
	
	if strings.Contains(taskLower, "analytics") || strings.Contains(taskLower, "data analysis") {
		return m.tryLoadTemplate("analytics-engineer", 0.9, "Analytics engineering")
	}
	
	if strings.Contains(taskLower, "data quality") || strings.Contains(taskLower, "data validation") {
		return m.tryLoadTemplate("data-quality-validator", 0.9, "Data quality")
	}
	
	// Infrastructure matches
	if strings.Contains(taskLower, "cloud") || strings.Contains(taskLower, "aws") ||
	   strings.Contains(taskLower, "azure") || strings.Contains(taskLower, "gcp") {
		return m.tryLoadTemplate("cloud-architect", 0.85, "Cloud infrastructure")
	}
	
	if strings.Contains(taskLower, "network") {
		return m.tryLoadTemplate("network-optimizer", 0.85, "Network optimization")
	}
	
	if strings.Contains(taskLower, "disaster") || strings.Contains(taskLower, "backup") {
		return m.tryLoadTemplate("disaster-recovery", 0.85, "Disaster recovery")
	}
	
	if strings.Contains(taskLower, "cost") && strings.Contains(taskLower, "optim") {
		return m.tryLoadTemplate("cost-optimizer", 0.85, "Cost optimization")
	}
	
	// No specific match
	return blueprintMatch{}
}

func (m *EnhancedAgentMatcher) tryLoadTemplate(name string, score float32, reason string) blueprintMatch {
	template, err := m.selector.registry.GetTemplate(name)
	if err != nil {
		// Template not available, return empty match
		return blueprintMatch{}
	}
	
	return blueprintMatch{
		template: template,
		score:    score,
		reason:   reason,
	}
}

// GetBlueprintRecommendations returns all matching blueprints with scores
func (m *EnhancedAgentMatcher) GetBlueprintRecommendations(task string) ([]BlueprintRecommendation, error) {
	analysis, err := m.selector.AnalyzeTask(task)
	if err != nil {
		return nil, err
	}
	
	var recommendations []BlueprintRecommendation
	
	// Get all potential matches
	potentialMatches := m.getAllPotentialMatches(analysis)
	
	// Score and sort
	for _, match := range potentialMatches {
		if match.score > 0.3 { // Minimum threshold
			recommendations = append(recommendations, BlueprintRecommendation{
				TemplateName: match.template.Metadata.Name,
				Template:     match.template,
				Score:        match.score,
				Reasoning:    match.reason,
				Capabilities: m.extractCapabilities(match.template),
			})
		}
	}
	
	// Sort by score
	sortBlueprintRecommendations(recommendations)
	
	return recommendations, nil
}

func (m *EnhancedAgentMatcher) getAllPotentialMatches(analysis *TaskAnalysis) []blueprintMatch {
	var matches []blueprintMatch
	
	// Get all templates
	templates, err := m.selector.registry.SearchTemplates("")
	if err != nil {
		return matches
	}
	
	for _, info := range templates {
		template, err := m.selector.registry.GetTemplate(info.Name)
		if err != nil {
			continue
		}
		
		score, reason := m.scoreTemplateForTask(template, analysis)
		if score > 0 {
			matches = append(matches, blueprintMatch{
				template: template,
				score:    score,
				reason:   reason,
			})
		}
	}
	
	return matches
}

func (m *EnhancedAgentMatcher) scoreTemplateForTask(template *AgentTemplate, analysis *TaskAnalysis) (float32, string) {
	return m.selector.scoreTemplate(template, analysis)
}

func (m *EnhancedAgentMatcher) extractCapabilities(template *AgentTemplate) []string {
	return m.selector.extractCapabilities(template)
}

// BlueprintRecommendation represents a recommended blueprint agent
type BlueprintRecommendation struct {
	TemplateName string
	Template     *AgentTemplate
	Score        float32
	Reasoning    string
	Capabilities []string
}

func sortBlueprintRecommendations(recs []BlueprintRecommendation) {
	for i := 0; i < len(recs); i++ {
		for j := i + 1; j < len(recs); j++ {
			if recs[j].Score > recs[i].Score {
				recs[i], recs[j] = recs[j], recs[i]
			}
		}
	}
}

// TaskRouter routes tasks to appropriate specialized agents
type TaskRouter struct {
	matcher *EnhancedAgentMatcher
}

// NewTaskRouter creates a router for task-to-agent mapping
func NewTaskRouter(registry TemplateRegistry, factory AgentFactory) *TaskRouter {
	return &TaskRouter{
		matcher: NewEnhancedAgentMatcher(registry, factory),
	}
}

// RouteTask determines the best agent(s) for a complex task
func (r *TaskRouter) RouteTask(ctx context.Context, task string) (*RoutingDecision, error) {
	// Get recommendations
	recommendations, err := r.matcher.GetBlueprintRecommendations(task)
	if err != nil {
		return nil, err
	}
	
	decision := &RoutingDecision{
		Task:          task,
		Complexity:    r.assessComplexity(task),
		RequiresTeam:  false,
	}
	
	if len(recommendations) == 0 {
		// No specialized agent found, use standard worker
		decision.PrimaryAgent = "worker"
		decision.Reasoning = "No specialized agent matched, using standard worker"
		return decision, nil
	}
	
	// Check if task needs multiple agents
	if r.needsMultipleAgents(task, recommendations) {
		decision.RequiresTeam = true
		decision.TeamComposition = r.composeTeam(recommendations)
		decision.Reasoning = "Complex task requires multiple specialized agents"
	} else {
		// Single agent sufficient
		best := recommendations[0]
		decision.PrimaryAgent = best.TemplateName
		decision.Confidence = best.Score
		decision.Reasoning = fmt.Sprintf("Best match: %s (%.0f%% confidence) - %s",
			best.TemplateName, best.Score*100, best.Reasoning)
		
		// Add alternatives if close matches exist
		for i := 1; i < len(recommendations) && i < 3; i++ {
			if recommendations[i].Score > 0.7 {
				decision.Alternatives = append(decision.Alternatives, 
					recommendations[i].TemplateName)
			}
		}
	}
	
	return decision, nil
}

func (r *TaskRouter) assessComplexity(task string) string {
	wordCount := len(strings.Fields(task))
	
	if wordCount > 50 || strings.Contains(task, "complete") || 
	   strings.Contains(task, "full") || strings.Contains(task, "comprehensive") {
		return "high"
	}
	
	if wordCount > 20 {
		return "medium"
	}
	
	return "low"
}

func (r *TaskRouter) needsMultipleAgents(task string, recommendations []BlueprintRecommendation) bool {
	// Check if task explicitly mentions multiple components
	multiComponentKeywords := []string{
		"frontend and backend",
		"ui and api",
		"with testing",
		"and documentation",
		"complete feature",
		"full stack",
		"end to end",
	}
	
	taskLower := strings.ToLower(task)
	for _, keyword := range multiComponentKeywords {
		if strings.Contains(taskLower, keyword) {
			return true
		}
	}
	
	// Check if multiple high-scoring agents match
	highScoreCount := 0
	for _, rec := range recommendations {
		if rec.Score > 0.7 {
			highScoreCount++
		}
	}
	
	return highScoreCount > 2
}

func (r *TaskRouter) composeTeam(recommendations []BlueprintRecommendation) []TeamMember {
	var team []TeamMember
	
	// Add primary agents (high confidence)
	for _, rec := range recommendations {
		if rec.Score > 0.7 {
			team = append(team, TeamMember{
				Agent:        rec.TemplateName,
				Role:         r.determineRole(rec.TemplateName),
				Confidence:   rec.Score,
				Capabilities: rec.Capabilities,
			})
		}
		
		if len(team) >= 5 {
			break // Limit team size
		}
	}
	
	return team
}

func (r *TaskRouter) determineRole(agentName string) string {
	roleMap := map[string]string{
		"api-builder":           "Backend Development",
		"component-builder":     "Frontend Development",
		"database-migrator":     "Database Management",
		"integration-tester":    "Quality Assurance",
		"security-auditor":      "Security Review",
		"api-documenter":        "Documentation",
		"ci-cd-optimizer":       "DevOps",
		"performance-profiler":  "Performance Optimization",
	}
	
	if role, ok := roleMap[agentName]; ok {
		return role
	}
	
	return "Specialized Development"
}

// RoutingDecision represents the routing decision for a task
type RoutingDecision struct {
	Task            string
	Complexity      string
	PrimaryAgent    string
	Alternatives    []string
	RequiresTeam    bool
	TeamComposition []TeamMember
	Confidence      float32
	Reasoning       string
}

// TeamMember represents a member of an agent team
type TeamMember struct {
	Agent        string
	Role         string
	Confidence   float32
	Capabilities []string
}