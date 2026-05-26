package factory

import (
	"context"
	"fmt"
	"strings"
)

// AgentSelector intelligently selects the right agent template for a task
type AgentSelector interface {
	SelectAgent(ctx context.Context, task string, analysis *TaskAnalysis) (*AgentTemplate, error)
	AnalyzeTask(task string) (*TaskAnalysis, error)
	GetRecommendedAgents(analysis *TaskAnalysis) ([]*AgentRecommendation, error)
	CanUseSpecializedAgent(task string) bool
}

type TaskAnalysis struct {
	OriginalTask    string
	TaskType        TaskType
	Domain          string
	RequiredTools   []string
	RequiredAPIs    []string
	Complexity      ComplexityLevel
	Keywords        []string
	DetectedPatterns []Pattern
	SecurityNeeded  bool
	PerformanceNeeded bool
	DatabaseWork    bool
	APIWork         bool
	TestingNeeded   bool
	DocumentationNeeded bool
}

type TaskType string

const (
	TaskTypeImplementation TaskType = "implementation"
	TaskTypeBugFix        TaskType = "bugfix"
	TaskTypeRefactor      TaskType = "refactor"
	TaskTypeTesting       TaskType = "testing"
	TaskTypeSecurity      TaskType = "security"
	TaskTypePerformance   TaskType = "performance"
	TaskTypeDocumentation TaskType = "documentation"
	TaskTypeMigration     TaskType = "migration"
	TaskTypeDeployment    TaskType = "deployment"
	TaskTypeAnalysis      TaskType = "analysis"
)

type ComplexityLevel string

const (
	ComplexityLow    ComplexityLevel = "low"
	ComplexityMedium ComplexityLevel = "medium"
	ComplexityHigh   ComplexityLevel = "high"
)

type Pattern struct {
	Name       string
	Confidence float32
	Indicators []string
}

type AgentRecommendation struct {
	Template    *AgentTemplate
	Score       float32
	Reasoning   string
	Capabilities []string
}

type agentSelector struct {
	registry TemplateRegistry
	factory  AgentFactory
}

func NewAgentSelector(registry TemplateRegistry, factory AgentFactory) AgentSelector {
	return &agentSelector{
		registry: registry,
		factory:  factory,
	}
}

func (s *agentSelector) SelectAgent(ctx context.Context, task string, analysis *TaskAnalysis) (*AgentTemplate, error) {
	// If no analysis provided, do it now
	if analysis == nil {
		var err error
		analysis, err = s.AnalyzeTask(task)
		if err != nil {
			return nil, fmt.Errorf("task analysis failed: %w", err)
		}
	}

	// Get recommendations
	recommendations, err := s.GetRecommendedAgents(analysis)
	if err != nil {
		return nil, fmt.Errorf("failed to get recommendations: %w", err)
	}

	// If no specialized agent matches well, fall back to standard worker
	if len(recommendations) == 0 || recommendations[0].Score < 0.5 {
		return s.getDefaultWorkerTemplate()
	}

	// Return the best match
	return recommendations[0].Template, nil
}

func (s *agentSelector) AnalyzeTask(task string) (*TaskAnalysis, error) {
	taskLower := strings.ToLower(task)
	
	analysis := &TaskAnalysis{
		OriginalTask: task,
		Keywords:     extractKeywords(task),
		Complexity:   estimateComplexity(task),
	}

	// Detect task type
	analysis.TaskType = detectTaskType(taskLower)
	
	// Detect domain
	analysis.Domain = detectDomain(taskLower)
	
	// Detect patterns
	analysis.DetectedPatterns = detectPatterns(taskLower)
	
	// Check for specific needs
	analysis.SecurityNeeded = containsSecurityKeywords(taskLower)
	analysis.PerformanceNeeded = containsPerformanceKeywords(taskLower)
	analysis.DatabaseWork = containsDatabaseKeywords(taskLower)
	analysis.APIWork = containsAPIKeywords(taskLower)
	analysis.TestingNeeded = containsTestingKeywords(taskLower)
	analysis.DocumentationNeeded = containsDocumentationKeywords(taskLower)
	
	// Determine required tools based on patterns
	analysis.RequiredTools = determineRequiredTools(analysis)
	analysis.RequiredAPIs = determineRequiredAPIs(analysis)
	
	return analysis, nil
}

func (s *agentSelector) GetRecommendedAgents(analysis *TaskAnalysis) ([]*AgentRecommendation, error) {
	templates, err := s.registry.SearchTemplates("")
	if err != nil {
		return nil, err
	}

	var recommendations []*AgentRecommendation

	for _, info := range templates {
		template, err := s.registry.GetTemplate(info.Name)
		if err != nil {
			continue
		}

		score, reasoning := s.scoreTemplate(template, analysis)
		if score > 0 {
			recommendations = append(recommendations, &AgentRecommendation{
				Template:  template,
				Score:     score,
				Reasoning: reasoning,
				Capabilities: s.extractCapabilities(template),
			})
		}
	}

	// Sort by score (highest first)
	sortRecommendations(recommendations)
	
	return recommendations, nil
}

func (s *agentSelector) CanUseSpecializedAgent(task string) bool {
	analysis, err := s.AnalyzeTask(task)
	if err != nil {
		return false
	}

	// Check if any specialized patterns are detected
	if analysis.SecurityNeeded || analysis.PerformanceNeeded || 
	   analysis.DatabaseWork || len(analysis.DetectedPatterns) > 0 {
		// Check if we have matching templates
		recommendations, _ := s.GetRecommendedAgents(analysis)
		return len(recommendations) > 0 && recommendations[0].Score >= 0.5
	}

	return false
}

func (s *agentSelector) scoreTemplate(template *AgentTemplate, analysis *TaskAnalysis) (float32, string) {
	score := float32(0)
	var reasons []string

	// Check for direct matches in tags
	for _, tag := range template.Metadata.Tags {
		tagLower := strings.ToLower(tag)
		
		// Domain match
		if tagLower == strings.ToLower(analysis.Domain) {
			score += 0.3
			reasons = append(reasons, fmt.Sprintf("domain match: %s", analysis.Domain))
		}
		
		// Task type match
		if tagLower == strings.ToLower(string(analysis.TaskType)) {
			score += 0.3
			reasons = append(reasons, fmt.Sprintf("task type match: %s", analysis.TaskType))
		}
		
		// Keyword matches
		for _, keyword := range analysis.Keywords {
			if strings.Contains(tagLower, strings.ToLower(keyword)) {
				score += 0.1
				reasons = append(reasons, fmt.Sprintf("keyword match: %s", keyword))
			}
		}
	}

	// Check for capability matches
	if analysis.SecurityNeeded && strings.Contains(template.Metadata.Name, "security") {
		score += 0.4
		reasons = append(reasons, "security capabilities needed")
	}
	
	if analysis.PerformanceNeeded && strings.Contains(template.Metadata.Name, "performance") {
		score += 0.4
		reasons = append(reasons, "performance analysis needed")
	}
	
	if analysis.DatabaseWork && strings.Contains(template.Metadata.Name, "database") {
		score += 0.4
		reasons = append(reasons, "database operations needed")
	}
	
	if analysis.APIWork && strings.Contains(template.Metadata.Name, "api") {
		score += 0.3
		reasons = append(reasons, "API work detected")
	}
	
	if analysis.TestingNeeded && strings.Contains(template.Metadata.Name, "test") {
		score += 0.3
		reasons = append(reasons, "testing required")
	}
	
	if analysis.DocumentationNeeded && strings.Contains(template.Metadata.Name, "doc") {
		score += 0.3
		reasons = append(reasons, "documentation needed")
	}

	// Check tool requirements
	templateTools := make(map[string]bool)
	for _, tool := range template.Spec.Capabilities.Tools {
		templateTools[tool.Name] = true
	}
	
	matchedTools := 0
	for _, requiredTool := range analysis.RequiredTools {
		if templateTools[requiredTool] {
			matchedTools++
		}
	}
	
	if matchedTools > 0 && len(analysis.RequiredTools) > 0 {
		toolScore := float32(matchedTools) / float32(len(analysis.RequiredTools))
		score += toolScore * 0.3
		reasons = append(reasons, fmt.Sprintf("tool match: %d/%d", matchedTools, len(analysis.RequiredTools)))
	}

	// Complexity alignment
	if analysis.Complexity == ComplexityHigh && template.Spec.Base.Type == "persistent" {
		score += 0.1
		reasons = append(reasons, "complex task suits persistent agent")
	}

	// Cap score at 1.0
	if score > 1.0 {
		score = 1.0
	}

	reasoning := strings.Join(reasons, "; ")
	return score, reasoning
}

func (s *agentSelector) getDefaultWorkerTemplate() (*AgentTemplate, error) {
	// Try to get the standard worker template
	template, err := s.registry.GetTemplate("worker")
	if err != nil {
		// Return a minimal default template
		return &AgentTemplate{
			APIVersion: "agents.oat.dev/v1",
			Kind:       "AgentTemplate",
			Metadata: TemplateMetadata{
				Name:        "worker",
				Version:     "1.0.0",
				Author:      "oat-core",
				Description: "Standard worker agent",
			},
			Spec: TemplateSpec{
				Base: BaseConfig{
					Type:        "worker",
					Model:       "default",
					Temperature: 0.7,
				},
				Behavior: BehaviorConfig{
					AutoComplete: true,
					PRCreation:   "required",
				},
			},
		}, nil
	}
	return template, nil
}

func (s *agentSelector) extractCapabilities(template *AgentTemplate) []string {
	var caps []string
	
	for _, tool := range template.Spec.Capabilities.Tools {
		caps = append(caps, fmt.Sprintf("tool:%s", tool.Name))
	}
	
	for _, api := range template.Spec.Capabilities.APIs {
		caps = append(caps, fmt.Sprintf("api:%s", api))
	}
	
	if template.Spec.Capabilities.Models.Primary != "" {
		caps = append(caps, fmt.Sprintf("model:%s", template.Spec.Capabilities.Models.Primary))
	}
	
	return caps
}

// Helper functions

func detectTaskType(task string) TaskType {
	switch {
	case strings.Contains(task, "fix") || strings.Contains(task, "bug") || strings.Contains(task, "error"):
		return TaskTypeBugFix
	case strings.Contains(task, "test") || strings.Contains(task, "spec"):
		return TaskTypeTesting
	case strings.Contains(task, "security") || strings.Contains(task, "vulnerability") || strings.Contains(task, "audit"):
		return TaskTypeSecurity
	case strings.Contains(task, "performance") || strings.Contains(task, "optimize") || strings.Contains(task, "speed"):
		return TaskTypePerformance
	case strings.Contains(task, "document") || strings.Contains(task, "readme") || strings.Contains(task, "docs"):
		return TaskTypeDocumentation
	case strings.Contains(task, "refactor") || strings.Contains(task, "clean"):
		return TaskTypeRefactor
	case strings.Contains(task, "migrate") || strings.Contains(task, "migration"):
		return TaskTypeMigration
	case strings.Contains(task, "deploy") || strings.Contains(task, "release"):
		return TaskTypeDeployment
	case strings.Contains(task, "analyze") || strings.Contains(task, "review"):
		return TaskTypeAnalysis
	default:
		return TaskTypeImplementation
	}
}

func detectDomain(task string) string {
	switch {
	case strings.Contains(task, "auth") || strings.Contains(task, "login") || strings.Contains(task, "session"):
		return "authentication"
	case strings.Contains(task, "database") || strings.Contains(task, "sql") || strings.Contains(task, "schema"):
		return "database"
	case strings.Contains(task, "api") || strings.Contains(task, "endpoint") || strings.Contains(task, "rest"):
		return "api"
	case strings.Contains(task, "ui") || strings.Contains(task, "frontend") || strings.Contains(task, "react"):
		return "frontend"
	case strings.Contains(task, "backend") || strings.Contains(task, "server"):
		return "backend"
	case strings.Contains(task, "infra") || strings.Contains(task, "docker") || strings.Contains(task, "kubernetes"):
		return "infrastructure"
	default:
		return "general"
	}
}

func detectPatterns(task string) []Pattern {
	var patterns []Pattern
	
	if strings.Contains(task, "crud") || (strings.Contains(task, "create") && strings.Contains(task, "read")) {
		patterns = append(patterns, Pattern{
			Name:       "CRUD",
			Confidence: 0.8,
			Indicators: []string{"create", "read", "update", "delete"},
		})
	}
	
	if strings.Contains(task, "rest") || strings.Contains(task, "restful") {
		patterns = append(patterns, Pattern{
			Name:       "REST",
			Confidence: 0.9,
			Indicators: []string{"rest", "api", "endpoint"},
		})
	}
	
	if strings.Contains(task, "event") || strings.Contains(task, "queue") || strings.Contains(task, "async") {
		patterns = append(patterns, Pattern{
			Name:       "EventDriven",
			Confidence: 0.7,
			Indicators: []string{"event", "queue", "async", "message"},
		})
	}
	
	return patterns
}

func containsSecurityKeywords(task string) bool {
	keywords := []string{"security", "vulnerability", "audit", "penetration", "exploit", 
		"injection", "xss", "csrf", "authentication", "authorization", "encryption", "hash"}
	for _, kw := range keywords {
		if strings.Contains(task, kw) {
			return true
		}
	}
	return false
}

func containsPerformanceKeywords(task string) bool {
	keywords := []string{"performance", "optimize", "speed", "latency", "throughput", 
		"benchmark", "profile", "cache", "memory", "cpu", "load"}
	for _, kw := range keywords {
		if strings.Contains(task, kw) {
			return true
		}
	}
	return false
}

func containsDatabaseKeywords(task string) bool {
	keywords := []string{"database", "sql", "postgres", "mysql", "mongodb", "redis",
		"migration", "schema", "table", "index", "query", "transaction"}
	for _, kw := range keywords {
		if strings.Contains(task, kw) {
			return true
		}
	}
	return false
}

func containsAPIKeywords(task string) bool {
	keywords := []string{"api", "endpoint", "rest", "graphql", "grpc", "webhook",
		"openapi", "swagger", "route", "http", "request", "response"}
	for _, kw := range keywords {
		if strings.Contains(task, kw) {
			return true
		}
	}
	return false
}

func containsTestingKeywords(task string) bool {
	keywords := []string{"test", "spec", "unit", "integration", "e2e", "coverage",
		"mock", "stub", "fixture", "assertion", "expect", "should"}
	for _, kw := range keywords {
		if strings.Contains(task, kw) {
			return true
		}
	}
	return false
}

func containsDocumentationKeywords(task string) bool {
	keywords := []string{"document", "docs", "readme", "tutorial", "guide", "manual",
		"api doc", "comment", "jsdoc", "docstring", "markdown", "wiki"}
	for _, kw := range keywords {
		if strings.Contains(task, kw) {
			return true
		}
	}
	return false
}

func extractKeywords(task string) []string {
	// Simple keyword extraction - in production, use NLP
	words := strings.Fields(strings.ToLower(task))
	stopWords := map[string]bool{
		"the": true, "a": true, "an": true, "and": true, "or": true,
		"but": true, "in": true, "on": true, "at": true, "to": true,
		"for": true, "of": true, "with": true, "by": true, "from": true,
		"as": true, "is": true, "was": true, "are": true, "were": true,
	}
	
	var keywords []string
	for _, word := range words {
		word = strings.Trim(word, ".,!?;:")
		if !stopWords[word] && len(word) > 2 {
			keywords = append(keywords, word)
		}
	}
	
	return keywords
}

func estimateComplexity(task string) ComplexityLevel {
	wordCount := len(strings.Fields(task))
	
	// Check for complexity indicators
	complexIndicators := []string{"architecture", "refactor", "migrate", "optimize", 
		"redesign", "rewrite", "overhaul", "integration", "distributed"}
	
	complexCount := 0
	taskLower := strings.ToLower(task)
	for _, indicator := range complexIndicators {
		if strings.Contains(taskLower, indicator) {
			complexCount++
		}
	}
	
	if complexCount >= 2 || wordCount > 50 {
		return ComplexityHigh
	} else if complexCount >= 1 || wordCount > 20 {
		return ComplexityMedium
	}
	
	return ComplexityLow
}

func determineRequiredTools(analysis *TaskAnalysis) []string {
	var tools []string
	
	if analysis.SecurityNeeded {
		tools = append(tools, "semgrep", "gitleaks", "trivy")
	}
	
	if analysis.PerformanceNeeded {
		tools = append(tools, "pprof", "benchmark", "loadtest")
	}
	
	if analysis.TestingNeeded {
		tools = append(tools, "pytest", "jest", "mocha")
	}
	
	if strings.Contains(analysis.Domain, "frontend") {
		tools = append(tools, "eslint", "prettier", "webpack")
	}
	
	if strings.Contains(analysis.Domain, "backend") {
		tools = append(tools, "golint", "gofmt", "go-critic")
	}
	
	return tools
}

func determineRequiredAPIs(analysis *TaskAnalysis) []string {
	var apis []string
	
	apis = append(apis, "github") // Always need GitHub
	
	if analysis.SecurityNeeded {
		apis = append(apis, "snyk", "dependabot")
	}
	
	if strings.Contains(analysis.OriginalTask, "monitor") || strings.Contains(analysis.OriginalTask, "observability") {
		apis = append(apis, "datadog", "prometheus")
	}
	
	if strings.Contains(analysis.OriginalTask, "deploy") {
		apis = append(apis, "kubernetes", "docker")
	}
	
	return apis
}

func sortRecommendations(recs []*AgentRecommendation) {
	// Simple bubble sort for now
	for i := 0; i < len(recs); i++ {
		for j := i + 1; j < len(recs); j++ {
			if recs[j].Score > recs[i].Score {
				recs[i], recs[j] = recs[j], recs[i]
			}
		}
	}
}