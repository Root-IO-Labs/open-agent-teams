package factory_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Root-IO-Labs/open-agent-teams/internal/factory"
)

// TestBlueprintsIntegration tests loading templates from agent-blueprints repo
func TestBlueprintsIntegration(t *testing.T) {
	// Create registry
	registry := factory.NewTemplateRegistry()
	
	// Load builtin templates first
	if err := registry.LoadBuiltinTemplates(); err != nil {
		t.Fatalf("Failed to load builtin templates: %v", err)
	}
	
	// Test loading from GitHub raw content
	blueprintsURL := "https://raw.githubusercontent.com/oat-agent/agent-blueprints/main"
	if err := registry.FetchFromRegistry(blueprintsURL); err != nil {
		t.Fatalf("Failed to fetch from blueprints registry: %v", err)
	}
	
	// Verify templates were loaded
	templates, err := registry.SearchTemplates("")
	if err != nil {
		t.Fatalf("Failed to search templates: %v", err)
	}
	
	// Check we have expected templates
	expectedTemplates := []string{
		"api-builder",
		"security-auditor",
		"performance-profiler",
		"database-migrator",
		"component-builder",
		"spa-developer",
	}
	
	foundTemplates := make(map[string]bool)
	for _, tmpl := range templates {
		foundTemplates[tmpl.Name] = true
	}
	
	for _, expected := range expectedTemplates {
		if !foundTemplates[expected] {
			t.Errorf("Expected template %s not found", expected)
		}
	}
	
	t.Logf("Successfully loaded %d templates from blueprints", len(templates))
}

// TestAgentSelection tests the intelligent agent selection
func TestAgentSelection(t *testing.T) {
	registry := factory.NewTemplateRegistry()
	registry.LoadBuiltinTemplates()
	
	// Mock loading some templates
	mockFactory := factory.NewFactory(nil, nil)
	selector := factory.NewAgentSelector(registry, mockFactory)
	
	testCases := []struct {
		task           string
		expectedType   string
		expectedDomain string
	}{
		{
			task:           "Build a REST API for user management with authentication",
			expectedType:   "backend",
			expectedDomain: "api",
		},
		{
			task:           "Create a React component for displaying user profiles",
			expectedType:   "implementation",
			expectedDomain: "frontend",
		},
		{
			task:           "Optimize database queries for the dashboard",
			expectedType:   "performance",
			expectedDomain: "database",
		},
		{
			task:           "Write integration tests for the payment flow",
			expectedType:   "testing",
			expectedDomain: "general",
		},
		{
			task:           "Audit security vulnerabilities in the authentication system",
			expectedType:   "security",
			expectedDomain: "authentication",
		},
	}
	
	for _, tc := range testCases {
		t.Run(tc.task, func(t *testing.T) {
			analysis, err := selector.AnalyzeTask(tc.task)
			if err != nil {
				t.Fatalf("Task analysis failed: %v", err)
			}
			
			if string(analysis.TaskType) != tc.expectedType {
				t.Errorf("Expected task type %s, got %s", tc.expectedType, analysis.TaskType)
			}
			
			if analysis.Domain != tc.expectedDomain {
				t.Errorf("Expected domain %s, got %s", tc.expectedDomain, analysis.Domain)
			}
			
			// Check if specialized agent is recommended
			if selector.CanUseSpecializedAgent(tc.task) {
				t.Logf("✓ Specialized agent recommended for: %s", tc.task)
			}
		})
	}
}

// TestTemplateValidation tests template validation
func TestTemplateValidation(t *testing.T) {
	validator := factory.NewTemplateValidator()
	
	// Test valid template
	validTemplate := &factory.AgentTemplate{
		APIVersion: "agents.oat.dev/v1",
		Kind:       "AgentTemplate",
		Metadata: factory.TemplateMetadata{
			Name:        "test-agent",
			Version:     "1.0.0",
			Author:      "test",
			Description: "Test agent",
		},
		Spec: factory.TemplateSpec{
			Base: factory.BaseConfig{
				Type:  "worker",
				Model: "default",
			},
			Prompt: factory.PromptConfig{
				System: "Test prompt",
			},
		},
	}
	
	if err := validator.Validate(validTemplate); err != nil {
		t.Errorf("Valid template failed validation: %v", err)
	}
	
	// Test invalid template (missing name)
	invalidTemplate := &factory.AgentTemplate{
		APIVersion: "agents.oat.dev/v1",
		Kind:       "AgentTemplate",
		Metadata: factory.TemplateMetadata{
			Version:     "1.0.0",
			Author:      "test",
			Description: "Test agent",
		},
	}
	
	if err := validator.Validate(invalidTemplate); err == nil {
		t.Error("Invalid template passed validation")
	}
}

// TestResourceManagement tests resource allocation and limits
func TestResourceManagement(t *testing.T) {
	rm := factory.NewResourceManager()
	
	// Test allocation
	agent1 := &factory.Agent{
		ID:   "agent-1",
		Name: "test-agent-1",
	}
	
	limits1 := factory.ResourceLimits{
		Memory: "1Gi",
		CPU:    2,
	}
	
	if err := rm.CanAllocate(limits1); err != nil {
		t.Errorf("Should be able to allocate resources: %v", err)
	}
	
	if err := rm.Allocate(agent1, limits1); err != nil {
		t.Errorf("Failed to allocate resources: %v", err)
	}
	
	// Get usage report
	report := rm.GetUsageReport()
	if len(report.Agents) != 1 {
		t.Errorf("Expected 1 agent in report, got %d", len(report.Agents))
	}
	
	// Release resources
	if err := rm.Release(agent1.ID); err != nil {
		t.Errorf("Failed to release resources: %v", err)
	}
	
	// Check resources are available again
	report = rm.GetUsageReport()
	if len(report.Agents) != 0 {
		t.Errorf("Expected 0 agents after release, got %d", len(report.Agents))
	}
}

// TestCapabilityInjection tests tool and API injection
func TestCapabilityInjection(t *testing.T) {
	ci := factory.NewCapabilityInjector()
	
	agent := &factory.Agent{
		ID:   "test-agent",
		Name: "test",
	}
	
	caps := factory.CapabilityRequests{
		Tools: []factory.ToolRequirement{
			{Name: "eslint", Version: ">=8.0.0"},
			{Name: "pytest", Version: ">=7.0.0"},
		},
		APIs: []string{"github"},
		Models: factory.ModelRequirements{
			Primary: "claude-3-opus",
		},
	}
	
	// Test validation
	if err := ci.Validate(caps); err != nil {
		t.Logf("Note: Some tools may not be available: %v", err)
	}
	
	// Test injection (may fail if tools not installed)
	err := ci.Inject(agent, caps)
	if err != nil && !strings.Contains(err.Error(), "not available") {
		t.Errorf("Unexpected injection error: %v", err)
	}
	
	if agent.Capabilities != nil {
		t.Logf("Injected capabilities: %d tools, %d APIs, %d models",
			len(agent.Capabilities.Tools),
			len(agent.Capabilities.APIs),
			len(agent.Capabilities.Models))
	}
}

// TestTaskAnalysisPatterns tests pattern detection in tasks
func TestTaskAnalysisPatterns(t *testing.T) {
	registry := factory.NewTemplateRegistry()
	registry.LoadBuiltinTemplates()
	
	mockFactory := factory.NewFactory(nil, nil)
	selector := factory.NewAgentSelector(registry, mockFactory)
	
	testCases := []struct {
		task              string
		expectedPatterns  []string
		expectedSecurity  bool
		expectedDatabase  bool
		expectedTesting   bool
		expectedAPI       bool
	}{
		{
			task:             "Create CRUD operations for user management",
			expectedPatterns: []string{"CRUD"},
			expectedDatabase: true,
			expectedAPI:      false,
		},
		{
			task:             "Build a RESTful API with OpenAPI documentation",
			expectedPatterns: []string{"REST"},
			expectedAPI:      true,
		},
		{
			task:             "Implement event-driven message queue processing",
			expectedPatterns: []string{"EventDriven"},
		},
		{
			task:             "Fix SQL injection vulnerability in login endpoint",
			expectedSecurity: true,
			expectedDatabase: true,
			expectedAPI:      true,
		},
		{
			task:            "Write unit tests with 80% coverage",
			expectedTesting: true,
		},
	}
	
	for _, tc := range testCases {
		t.Run(tc.task, func(t *testing.T) {
			analysis, err := selector.AnalyzeTask(tc.task)
			if err != nil {
				t.Fatalf("Task analysis failed: %v", err)
			}
			
			// Check patterns
			foundPatterns := make(map[string]bool)
			for _, p := range analysis.DetectedPatterns {
				foundPatterns[p.Name] = true
			}
			
			for _, expected := range tc.expectedPatterns {
				if !foundPatterns[expected] {
					t.Errorf("Expected pattern %s not detected", expected)
				}
			}
			
			// Check flags
			if analysis.SecurityNeeded != tc.expectedSecurity {
				t.Errorf("Security flag mismatch: expected %v, got %v",
					tc.expectedSecurity, analysis.SecurityNeeded)
			}
			
			if analysis.DatabaseWork != tc.expectedDatabase {
				t.Errorf("Database flag mismatch: expected %v, got %v",
					tc.expectedDatabase, analysis.DatabaseWork)
			}
			
			if analysis.TestingNeeded != tc.expectedTesting {
				t.Errorf("Testing flag mismatch: expected %v, got %v",
					tc.expectedTesting, analysis.TestingNeeded)
			}
			
			if analysis.APIWork != tc.expectedAPI {
				t.Errorf("API flag mismatch: expected %v, got %v",
					tc.expectedAPI, analysis.APIWork)
			}
		})
	}
}

// TestEnd2EndFactoryFlow tests the complete factory flow
func TestEnd2EndFactoryFlow(t *testing.T) {
	ctx := context.Background()
	
	// Setup
	registry := factory.NewTemplateRegistry()
	registry.LoadBuiltinTemplates()
	
	testFactory := factory.NewFactory(nil, nil)
	selector := factory.NewAgentSelector(registry, testFactory)
	
	// Simulate a task
	task := "Build a React dashboard component with real-time data updates"
	
	// Analyze task
	analysis, err := selector.AnalyzeTask(task)
	if err != nil {
		t.Fatalf("Task analysis failed: %v", err)
	}
	
	t.Logf("Task Analysis Results:")
	t.Logf("  Type: %s", analysis.TaskType)
	t.Logf("  Domain: %s", analysis.Domain)
	t.Logf("  Complexity: %s", analysis.Complexity)
	t.Logf("  Keywords: %v", analysis.Keywords)
	
	// Get recommendations
	recommendations, err := selector.GetRecommendedAgents(analysis)
	if err != nil {
		t.Fatalf("Failed to get recommendations: %v", err)
	}
	
	if len(recommendations) == 0 {
		t.Fatal("No agent recommendations")
	}
	
	t.Logf("\nAgent Recommendations:")
	for i, rec := range recommendations {
		if i >= 3 {
			break
		}
		t.Logf("  %d. %s (%.0f%% match)", i+1, 
			rec.Template.Metadata.Name, rec.Score*100)
		t.Logf("     Reasoning: %s", rec.Reasoning)
	}
	
	// Select best agent
	selectedTemplate, err := selector.SelectAgent(ctx, task, analysis)
	if err != nil {
		t.Fatalf("Failed to select agent: %v", err)
	}
	
	t.Logf("\nSelected Agent: %s", selectedTemplate.Metadata.Name)
	t.Logf("  Description: %s", selectedTemplate.Metadata.Description)
}

// BenchmarkTaskAnalysis benchmarks task analysis performance
func BenchmarkTaskAnalysis(b *testing.B) {
	registry := factory.NewTemplateRegistry()
	registry.LoadBuiltinTemplates()
	
	mockFactory := factory.NewFactory(nil, nil)
	selector := factory.NewAgentSelector(registry, mockFactory)
	
	task := "Create a comprehensive API with authentication, database integration, and testing"
	
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = selector.AnalyzeTask(task)
	}
}

// BenchmarkAgentSelection benchmarks agent selection performance
func BenchmarkAgentSelection(b *testing.B) {
	ctx := context.Background()
	registry := factory.NewTemplateRegistry()
	registry.LoadBuiltinTemplates()
	
	mockFactory := factory.NewFactory(nil, nil)
	selector := factory.NewAgentSelector(registry, mockFactory)
	
	task := "Build a secure REST API with user authentication"
	
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = selector.SelectAgent(ctx, task, nil)
	}
}