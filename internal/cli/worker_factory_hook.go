package cli

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/Root-IO-Labs/open-agent-teams-8/internal/factory"
	"github.com/Root-IO-Labs/open-agent-teams-8/internal/state"
)

// enhanceWorkerCreation wraps the existing worker creation to add factory intelligence
func (c *CLI) enhanceWorkerCreation(originalFunc func([]string) error) func([]string) error {
	return func(args []string) error {
		// Check if factory integration is enabled
		if !c.isFactoryEnabled() {
			// Fall back to original implementation
			return originalFunc(args)
		}
		
		// Parse the task to see if we should use a specialized agent
		flags, posArgs := ParseFlags(args)
		task := strings.Join(posArgs, " ")
		
		if task == "" {
			return originalFunc(args) // Let original handle validation
		}
		
		// Initialize factory integration if not already done
		if c.factoryIntegration == nil {
			c.factoryIntegration = NewFactoryIntegration(c)
		}
		
		// Check if we can use a specialized agent
		selector := c.factoryIntegration.selector
		if selector.CanUseSpecializedAgent(task) {
			// Intercept and use factory
			return c.createWorkerWithFactory(args)
		}
		
		// Fall back to original for standard workers
		return originalFunc(args)
	}
}

func (c *CLI) createWorkerWithFactory(args []string) error {
	flags, posArgs := ParseFlags(args)
	task := strings.Join(posArgs, " ")
	
	if task == "" {
		return fmt.Errorf("usage: oat worker create <task description>")
	}
	
	// Determine repository
	repoName, err := c.resolveRepo(flags)
	if err != nil {
		return err
	}
	
	// Analyze the task
	ctx := context.Background()
	analysis, err := c.factoryIntegration.selector.AnalyzeTask(task)
	if err != nil {
		// Fall back to standard worker
		return c.createStandardWorkerFallback(args)
	}
	
	// Get agent recommendations
	recommendations, err := c.factoryIntegration.selector.GetRecommendedAgents(analysis)
	if err != nil || len(recommendations) == 0 {
		// Fall back to standard worker
		return c.createStandardWorkerFallback(args)
	}
	
	// Show analysis to user
	c.showTaskAnalysis(analysis, recommendations)
	
	// Select the best agent template
	selectedTemplate := recommendations[0].Template
	
	// Check if user wants to override
	if c.isInteractive() {
		selectedTemplate = c.promptForAgentSelection(recommendations)
		if selectedTemplate == nil {
			// User chose standard worker
			return c.createStandardWorkerFallback(args)
		}
	}
	
	// Parse additional flags
	workerName := names.Generate()
	if name, ok := flags["name"]; ok {
		workerName = name
	}
	
	model := flags["model"]
	
	var maxTokens int64
	if v, ok := flags["max-tokens"]; ok && v != "" {
		parsed, _ := strconv.ParseInt(v, 10, 64)
		maxTokens = parsed
	}
	
	var issueNumber *int
	if v, ok := flags["issue"]; ok && v != "" {
		if num, err := strconv.Atoi(v); err == nil {
			issueNumber = &num
		}
	}
	
	issueURL := flags["issue-url"]
	branch := flags["branch"]
	
	// Create specialized agent via factory
	agent, err := c.factoryIntegration.factory.CreateAgent(ctx, &factory.CreateAgentRequest{
		Name:       workerName,
		Template:   selectedTemplate.Metadata.Name,
		Task:       task,
		Repository: repoName,
		Issue:      issueNumber,
		Parameters: map[string]interface{}{
			"issue_url":   issueURL,
			"branch":      branch,
			"model":       model,
			"max_tokens":  maxTokens,
			"push_to":     flags["push-to"],
		},
	})
	
	if err != nil {
		return fmt.Errorf("failed to create specialized agent: %w", err)
	}
	
	// Register with state and start agent
	if err := c.registerFactoryAgent(agent, repoName); err != nil {
		return err
	}
	
	format.Success("Created specialized agent '%s' (%s) for task: %s", 
		agent.Name, selectedTemplate.Metadata.Name, task)
	
	// Show capabilities
	if len(agent.Capabilities.Tools) > 0 {
		fmt.Println("\nAgent capabilities:")
		for tool := range agent.Capabilities.Tools {
			fmt.Printf("  • Tool: %s\n", tool)
		}
	}
	
	return nil
}

func (c *CLI) showTaskAnalysis(analysis *factory.TaskAnalysis, recommendations []*factory.AgentRecommendation) {
	format.Header("Task Analysis")
	
	fmt.Printf("Task Type: %s\n", format.Bold(string(analysis.TaskType)))
	fmt.Printf("Domain: %s\n", analysis.Domain)
	fmt.Printf("Complexity: %s\n", analysis.Complexity)
	
	if len(analysis.DetectedPatterns) > 0 {
		fmt.Println("\nDetected Patterns:")
		for _, pattern := range analysis.DetectedPatterns {
			fmt.Printf("  • %s (%.0f%% confidence)\n", pattern.Name, pattern.Confidence*100)
		}
	}
	
	fmt.Println("\nCapabilities Required:")
	if analysis.SecurityNeeded {
		fmt.Println("  ✓ Security analysis")
	}
	if analysis.PerformanceNeeded {
		fmt.Println("  ✓ Performance optimization")
	}
	if analysis.DatabaseWork {
		fmt.Println("  ✓ Database operations")
	}
	if analysis.APIWork {
		fmt.Println("  ✓ API development")
	}
	if analysis.TestingNeeded {
		fmt.Println("  ✓ Testing")
	}
	if analysis.DocumentationNeeded {
		fmt.Println("  ✓ Documentation")
	}
	
	fmt.Printf("\nRecommended Agent: %s (%.0f%% match)\n", 
		format.Bold(recommendations[0].Template.Metadata.Name),
		recommendations[0].Score*100)
	fmt.Printf("  %s\n", recommendations[0].Template.Metadata.Description)
}

func (c *CLI) promptForAgentSelection(recommendations []*factory.AgentRecommendation) *factory.AgentTemplate {
	fmt.Println("\nAvailable specialized agents:")
	
	for i, rec := range recommendations {
		if i >= 3 {
			break
		}
		fmt.Printf("%d. %s (%.0f%% match)\n", i+1, 
			rec.Template.Metadata.Name, rec.Score*100)
		fmt.Printf("   %s\n", rec.Template.Metadata.Description)
	}
	
	fmt.Println("0. Use standard worker")
	fmt.Print("\nSelect agent (0-3): ")
	
	var choice int
	fmt.Scanln(&choice)
	
	if choice > 0 && choice <= len(recommendations) {
		return recommendations[choice-1].Template
	}
	
	return nil
}

func (c *CLI) registerFactoryAgent(agent *factory.Agent, repoName string) error {
	c.state.Lock()
	defer c.state.Unlock()
	
	repo, exists := c.state.GetRepositoryUnlocked(repoName)
	if !exists {
		return fmt.Errorf("repository %s not found", repoName)
	}
	
	// Convert factory agent to state agent
	stateAgent := &state.Agent{
		Name:         agent.Name,
		Type:         agent.Type,
		Task:         agent.Task,
		Status:       "running",
		WorktreePath: agent.Process.WorktreePath,
		SessionID:    agent.Process.SessionID,
		PID:          agent.Process.PID,
		CreatedAt:    agent.CreatedAt,
		Template:     agent.Template, // Store template name
	}
	
	repo.Agents[agent.Name] = stateAgent
	
	return c.state.SaveUnlocked()
}

func (c *CLI) createStandardWorkerFallback(args []string) error {
	// This would call the original createWorker implementation
	// For now, we'll just indicate it would create a standard worker
	format.Info("Using standard worker (no specialized agent matched)")
	
	// Call original implementation
	return c.createWorker(args)
}

func (c *CLI) isFactoryEnabled() bool {
	// Check environment variable or config
	return os.Getenv("OAT_FACTORY_ENABLED") == "true"
}

func (c *CLI) isInteractive() bool {
	// Check if running in interactive mode
	return os.Getenv("OAT_INTERACTIVE") == "true"
}

// Hook to be called during CLI initialization
func (c *CLI) initializeFactoryHooks() {
	// This would be called in NewCLI to set up the factory integration
	if c.isFactoryEnabled() {
		c.factoryIntegration = NewFactoryIntegration(c)
		
		// We could wrap the original createWorker function
		// but for now we'll use conditional checks in the main function
	}
}