package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/Root-IO-Labs/open-agent-teams-8/internal/factory"
	"github.com/Root-IO-Labs/open-agent-teams-8/internal/planner"
	"github.com/Root-IO-Labs/open-agent-teams-8/internal/state"
)

// FactoryIntegration handles the integration between CLI and agent factory
type FactoryIntegration struct {
	cli      *CLI
	factory  factory.AgentFactory
	selector factory.AgentSelector
	registry factory.TemplateRegistry
	planner  *planner.EnhancedPlanner
}

// NewFactoryIntegration creates a new factory integration
func NewFactoryIntegration(cli *CLI) *FactoryIntegration {
	// Initialize components
	registry := factory.NewTemplateRegistry()
	registry.LoadBuiltinTemplates()
	
	// Try to load from agent-blueprints
	registry.FetchFromRegistry("https://raw.githubusercontent.com/oat-agent/agent-blueprints/main")
	
	// Create factory with daemon and state from CLI
	agentFactory := factory.NewFactory(nil, cli.state) // Daemon will be set later
	selector := factory.NewAgentSelector(registry, agentFactory)
	
	return &FactoryIntegration{
		cli:      cli,
		factory:  agentFactory,
		selector: selector,
		registry: registry,
	}
}

// CreateWorkerWithFactory creates a worker using the agent factory
func (fi *FactoryIntegration) CreateWorkerWithFactory(task string, options CreateWorkerOptions) error {
	ctx := context.Background()
	
	// Check if we should use a specialized agent
	if fi.selector.CanUseSpecializedAgent(task) {
		return fi.createSpecializedAgent(ctx, task, options)
	}
	
	// Fall back to standard worker creation
	return fi.createStandardWorker(ctx, task, options)
}

func (fi *FactoryIntegration) createSpecializedAgent(ctx context.Context, task string, options CreateWorkerOptions) error {
	// Analyze the task
	analysis, err := fi.selector.AnalyzeTask(task)
	if err != nil {
		return fmt.Errorf("task analysis failed: %w", err)
	}
	
	// Get recommendations
	recommendations, err := fi.selector.GetRecommendedAgents(analysis)
	if err != nil {
		return fmt.Errorf("failed to get agent recommendations: %w", err)
	}
	
	// If interactive mode, let user choose
	var selectedTemplate *factory.AgentTemplate
	if options.Interactive && len(recommendations) > 1 {
		selectedTemplate = fi.interactiveAgentSelection(recommendations)
	} else if len(recommendations) > 0 {
		// Use the best match
		selectedTemplate = recommendations[0].Template
		
		// Inform user about specialized agent selection
		fi.cli.format.Header("Specialized Agent Selection")
		fmt.Printf("Task Analysis:\n")
		fmt.Printf("  Type: %s\n", analysis.TaskType)
		fmt.Printf("  Domain: %s\n", analysis.Domain)
		fmt.Printf("  Complexity: %s\n", analysis.Complexity)
		
		if analysis.SecurityNeeded {
			fmt.Printf("  ✓ Security analysis required\n")
		}
		if analysis.PerformanceNeeded {
			fmt.Printf("  ✓ Performance optimization needed\n")
		}
		if analysis.DatabaseWork {
			fmt.Printf("  ✓ Database operations detected\n")
		}
		if analysis.TestingNeeded {
			fmt.Printf("  ✓ Testing required\n")
		}
		
		fmt.Printf("\nSelected Agent: %s\n", fi.cli.format.Bold(selectedTemplate.Metadata.Name))
		fmt.Printf("  %s\n", selectedTemplate.Metadata.Description)
		fmt.Printf("  Score: %.1f%%\n", recommendations[0].Score*100)
		fmt.Printf("  Reasoning: %s\n\n", recommendations[0].Reasoning)
	}
	
	if selectedTemplate == nil {
		// No suitable specialized agent, fall back
		return fi.createStandardWorker(ctx, task, options)
	}
	
	// Create the specialized agent
	agent, err := fi.factory.CreateAgent(ctx, &factory.CreateAgentRequest{
		Name:       options.Name,
		Template:   selectedTemplate.Metadata.Name,
		Task:       task,
		Repository: options.Repository,
		Parameters: map[string]interface{}{
			"issue":        options.Issue,
			"branch":       options.Branch,
			"model":        options.Model,
			"max_tokens":   options.MaxTokens,
		},
	})
	
	if err != nil {
		return fmt.Errorf("failed to create specialized agent: %w", err)
	}
	
	fi.cli.format.Success("Created specialized agent: %s", agent.Name)
	
	// Register with state
	return fi.registerAgentWithState(agent, options)
}

func (fi *FactoryIntegration) createStandardWorker(ctx context.Context, task string, options CreateWorkerOptions) error {
	// Use standard worker template
	agent, err := fi.factory.CreateAgent(ctx, &factory.CreateAgentRequest{
		Name:       options.Name,
		Template:   "worker",
		Task:       task,
		Repository: options.Repository,
		Parameters: map[string]interface{}{
			"issue":      options.Issue,
			"branch":     options.Branch,
			"model":      options.Model,
			"max_tokens": options.MaxTokens,
		},
	})
	
	if err != nil {
		return fmt.Errorf("failed to create worker: %w", err)
	}
	
	fi.cli.format.Success("Created worker: %s", agent.Name)
	
	return fi.registerAgentWithState(agent, options)
}

func (fi *FactoryIntegration) interactiveAgentSelection(recommendations []*factory.AgentRecommendation) *factory.AgentTemplate {
	fi.cli.format.Header("Multiple Specialized Agents Available")
	fmt.Println("\nSelect the most appropriate agent for your task:\n")
	
	for i, rec := range recommendations {
		if i >= 5 {
			break // Show max 5 options
		}
		
		fmt.Printf("%d. %s (%.0f%% match)\n", i+1, 
			fi.cli.format.Bold(rec.Template.Metadata.Name), 
			rec.Score*100)
		fmt.Printf("   %s\n", rec.Template.Metadata.Description)
		fmt.Printf("   Reasoning: %s\n", rec.Reasoning)
		
		if len(rec.Capabilities) > 0 {
			fmt.Printf("   Capabilities: %s\n", strings.Join(rec.Capabilities[:min(3, len(rec.Capabilities))], ", "))
		}
		fmt.Println()
	}
	
	fmt.Printf("0. Use standard worker\n\n")
	
	var choice int
	fmt.Print("Enter your choice (0-5): ")
	fmt.Scanln(&choice)
	
	if choice > 0 && choice <= len(recommendations) {
		return recommendations[choice-1].Template
	}
	
	return nil
}

func (fi *FactoryIntegration) registerAgentWithState(agent *factory.Agent, options CreateWorkerOptions) error {
	// Convert factory agent to state agent
	stateAgent := &state.Agent{
		Name:         agent.Name,
		Type:         state.AgentType(agent.Type),
		Task:         agent.Task,
		Status:       "running",
		SessionID:    agent.Process.SessionID,
		PID:          agent.Process.PID,
		WorktreePath: agent.Process.WorktreePath,
		CreatedAt:    agent.CreatedAt,
		Model:        options.Model,
		MaxTokens:    options.MaxTokens,
	}
	
	// Add to state
	fi.cli.state.Lock()
	defer fi.cli.state.Unlock()
	
	repo, exists := fi.cli.state.GetRepositoryUnlocked(options.Repository)
	if !exists {
		return fmt.Errorf("repository %s not found", options.Repository)
	}
	
	repo.Agents[agent.Name] = stateAgent
	
	return fi.cli.state.SaveUnlocked()
}

// CreateWorkerOptions holds options for worker creation
type CreateWorkerOptions struct {
	Name        string
	Repository  string
	Issue       *int
	IssueURL    string
	Branch      string
	Model       string
	MaxTokens   int64
	Interactive bool
}

// PlanCommand handles the new plan command for complex requirements
func (fi *FactoryIntegration) PlanCommand(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: oat plan <requirement>")
	}
	
	requirement := strings.Join(args, " ")
	
	// Get current repository
	repo, err := fi.cli.inferRepoFromCwd()
	if err != nil {
		return err
	}
	
	// Create enhanced planner
	if fi.planner == nil {
		fi.planner = planner.NewEnhancedPlanner(fi.factory, fi.cli.state, repo)
	}
	
	fi.cli.format.Header("Planning Execution for Requirement")
	fmt.Printf("Requirement: %s\n\n", requirement)
	
	ctx := context.Background()
	
	// Analyze and plan
	fmt.Println("Analyzing requirement...")
	plan, err := fi.planner.PlanAndExecute(ctx, requirement)
	if err != nil {
		return fmt.Errorf("planning failed: %w", err)
	}
	
	// Display the plan
	fi.displayExecutionPlan(plan)
	
	return nil
}

func (fi *FactoryIntegration) displayExecutionPlan(plan *planner.ExecutionPlan) {
	fi.cli.format.Header("Execution Plan")
	
	fmt.Printf("Complexity: %s\n", plan.Analysis.Complexity)
	fmt.Printf("Estimated Effort: %s\n", plan.Analysis.EstimatedEffort)
	
	if len(plan.Analysis.Risks) > 0 {
		fmt.Println("\nRisks:")
		for _, risk := range plan.Analysis.Risks {
			fmt.Printf("  ⚠ %s\n", risk)
		}
	}
	
	fmt.Printf("\nTasks: %d tasks in %d waves\n\n", len(plan.Tasks), len(plan.Waves))
	
	for _, wave := range plan.Waves {
		fmt.Printf("Wave %d:\n", wave.Number+1)
		for _, assignment := range wave.Assignments {
			statusIcon := "⏳"
			if assignment.Status == planner.AssignmentStatusCompleted {
				statusIcon = "✅"
			} else if assignment.Status == planner.AssignmentStatusFailed {
				statusIcon = "❌"
			} else if assignment.Status == planner.AssignmentStatusRunning {
				statusIcon = "🔄"
			}
			
			fmt.Printf("  %s [%s] %s\n", statusIcon, assignment.AgentTemplate, assignment.Task.Description)
			
			if len(assignment.Capabilities) > 0 {
				fmt.Printf("      Capabilities: %s\n", strings.Join(assignment.Capabilities[:min(3, len(assignment.Capabilities))], ", "))
			}
		}
		fmt.Println()
	}
	
	fmt.Printf("Status: %s\n", plan.Status)
}

// FactoryCommands adds factory-specific commands to the CLI
func (fi *FactoryIntegration) RegisterCommands() {
	// These would be registered in the main CLI registerCommands function
	// oat factory list - List available agent templates
	// oat factory show <template> - Show template details
	// oat factory validate <file> - Validate a template file
	// oat factory resources - Show resource usage
	// oat plan <requirement> - Plan and execute complex requirements
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}