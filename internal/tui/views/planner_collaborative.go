package views

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Root-IO-Labs/open-agent-teams/internal/socket"
)

// CollaborativePlanner extends PlannerView with supervisor-like orchestration capabilities
type CollaborativePlanner struct {
	*PlannerView
	
	// Orchestration state
	activeWorkers   map[string]WorkerStatus
	pendingTasks    []Task
	executionWaves  map[int][]Task
	currentWave     int
	
	// Collaborative features
	agentProfiles   map[string]AgentProfile
	workloadBalance map[string]int // agent -> current task count
	
	// Real-time monitoring
	statusSnapshots []StatusSnapshot
	lastSnapshot    time.Time
	autoDispatch    bool
	
	// Communication
	messageQueue    []AgentMessage
	coordinator     *WorkspaceCoordinator
}

// WorkerStatus tracks active worker state
type WorkerStatus struct {
	Name      string
	TaskID    string
	Task      Task
	State     string // "working", "stuck", "pr_open", "waiting_merge"
	StartTime time.Time
	LastSeen  time.Time
	NudgeCount int
	Progress   float64 // 0.0 to 1.0
	PR         *PRInfo
}

// AgentProfile defines capabilities of available agents
type AgentProfile struct {
	Name         string
	Type         string
	Capabilities []string
	MaxTasks     int
	CurrentLoad  int
	Performance  float64 // historical success rate
}

// StatusSnapshot captures system state at a point in time
type StatusSnapshot struct {
	Timestamp     time.Time
	Workers       []WorkerStatus
	OpenPRs       []PRInfo
	CompletedTasks int
	TotalTasks    int
	SystemHealth  string
}

// PRInfo represents pull request state
type PRInfo struct {
	Number      int
	Title       string
	Author      string
	Status      string // "open", "merged", "closed"
	CIStatus    string // "pending", "passing", "failing"
	ReviewStatus string // "pending", "approved", "changes_requested"
}

// AgentMessage for inter-agent communication
type AgentMessage struct {
	From      string
	To        string
	Type      string // "status", "task", "help", "complete"
	Content   interface{}
	Timestamp time.Time
}

// WorkspaceCoordinator manages shared state between planner and workspace
type WorkspaceCoordinator struct {
	SharedContext map[string]interface{}
	TaskQueue     chan Task
	StatusChan    chan StatusSnapshot
}

// Initialize collaborative planner
func NewCollaborativePlanner(client *socket.Client, repoName string) *CollaborativePlanner {
	base := NewPlannerView(client, repoName)
	
	cp := &CollaborativePlanner{
		PlannerView:     base,
		activeWorkers:   make(map[string]WorkerStatus),
		pendingTasks:    []Task{},
		executionWaves:  make(map[int][]Task),
		agentProfiles:   initAgentProfiles(),
		workloadBalance: make(map[string]int),
		statusSnapshots: []StatusSnapshot{},
		messageQueue:    []AgentMessage{},
		autoDispatch:    true,
		coordinator: &WorkspaceCoordinator{
			SharedContext: make(map[string]interface{}),
			TaskQueue:     make(chan Task, 100),
			StatusChan:    make(chan StatusSnapshot, 10),
		},
	}
	
	// Enhance base planner with collaborative features
	EnhancePlannerView(base)
	
	return cp
}

func initAgentProfiles() map[string]AgentProfile {
	return map[string]AgentProfile{
		"worker-general": {
			Name:         "worker-general",
			Type:         "worker",
			Capabilities: []string{"implementation", "testing", "documentation"},
			MaxTasks:     3,
			Performance:  0.85,
		},
		"worker-specialist": {
			Name:         "worker-specialist",
			Type:         "worker",
			Capabilities: []string{"complex-implementation", "architecture", "optimization"},
			MaxTasks:     1,
			Performance:  0.92,
		},
		"verifier": {
			Name:         "verifier",
			Type:         "verifier",
			Capabilities: []string{"testing", "validation", "review"},
			MaxTasks:     5,
			Performance:  0.95,
		},
	}
}

// Enhanced Update with collaborative features
func (cp *CollaborativePlanner) Update(msg tea.Msg) (*CollaborativePlanner, tea.Cmd) {
	var cmds []tea.Cmd
	
	// Handle base planner updates
	newBase, cmd := cp.PlannerView.Update(msg)
	cp.PlannerView = newBase
	if cmd != nil {
		cmds = append(cmds, cmd)
	}
	
	// Handle collaborative features
	switch msg := msg.(type) {
	case statusUpdateMsg:
		cp.handleStatusUpdate(msg.snapshot)
		
	case workerCompleteMsg:
		cp.handleWorkerCompletion(msg.workerName, msg.taskID)
		cmds = append(cmds, cp.checkAndDispatchNextWave())
		
	case prMergedMsg:
		cp.handlePRMerged(msg.prNumber)
		
	case orchestrationTickMsg:
		cmds = append(cmds, cp.performOrchestration())
	}
	
	return cp, tea.Batch(cmds...)
}

// Collaborative orchestration logic
func (cp *CollaborativePlanner) performOrchestration() tea.Cmd {
	// Take snapshot of current state
	snapshot := cp.captureSnapshot()
	cp.statusSnapshots = append(cp.statusSnapshots, snapshot)
	cp.lastSnapshot = time.Now()
	
	// Check for stuck workers
	for name, worker := range cp.activeWorkers {
		if cp.isWorkerStuck(worker) {
			cp.handleStuckWorker(name, worker)
		}
	}
	
	// Auto-dispatch if enabled
	if cp.autoDispatch && cp.state == StateExecuting {
		if cmd := cp.dispatchAvailableTasks(); cmd != nil {
			return cmd
		}
	}
	
	// Schedule next tick
	return tea.Tick(30*time.Second, func(t time.Time) tea.Msg {
		return orchestrationTickMsg(t)
	})
}

// Smart task dispatching with load balancing
func (cp *CollaborativePlanner) dispatchAvailableTasks() tea.Cmd {
	if len(cp.pendingTasks) == 0 {
		return nil
	}
	
	// Get available capacity
	availableAgents := cp.getAvailableAgents()
	if len(availableAgents) == 0 {
		return nil
	}
	
	var cmds []tea.Cmd
	for _, task := range cp.pendingTasks {
		// Find best agent for task
		agent := cp.findBestAgentForTask(task, availableAgents)
		if agent == nil {
			continue
		}
		
		// Dispatch task
		cmd := cp.dispatchTaskToAgent(task, agent.Name)
		if cmd != nil {
			cmds = append(cmds, cmd)
			
			// Update state
			cp.workloadBalance[agent.Name]++
			agent.CurrentLoad++
			
			// Remove from pending
			cp.pendingTasks = cp.removeTask(cp.pendingTasks, task)
		}
	}
	
	return tea.Batch(cmds...)
}

// Find best agent for a specific task
func (cp *CollaborativePlanner) findBestAgentForTask(task Task, agents []AgentProfile) *AgentProfile {
	var bestAgent *AgentProfile
	bestScore := 0.0
	
	for i, agent := range agents {
		score := cp.scoreAgentForTask(agent, task)
		if score > bestScore {
			bestScore = score
			bestAgent = &agents[i]
		}
	}
	
	return bestAgent
}

// Score agent suitability for task
func (cp *CollaborativePlanner) scoreAgentForTask(agent AgentProfile, task Task) float64 {
	score := 0.0
	
	// Check capability match
	capMatch := 0
	for _, cap := range agent.Capabilities {
		if strings.Contains(task.Type, cap) || strings.Contains(task.Description, cap) {
			capMatch++
		}
	}
	score += float64(capMatch) * 0.3
	
	// Factor in performance history
	score += agent.Performance * 0.3
	
	// Consider current load (prefer less loaded agents)
	loadFactor := 1.0 - (float64(agent.CurrentLoad) / float64(agent.MaxTasks))
	score += loadFactor * 0.2
	
	// Task complexity matching
	if task.Type == "complex-implementation" && agent.Type == "worker-specialist" {
		score += 0.2
	}
	
	return score
}

// Dispatch task to specific agent
func (cp *CollaborativePlanner) dispatchTaskToAgent(task Task, agentName string) tea.Cmd {
	client := cp.client
	repoName := cp.repoName
	
	// Build worker prompt with enhanced context
	prompt := cp.buildEnhancedWorkerPrompt(task)
	
	return func() tea.Msg {
		if client == nil {
			return plannerErrorMsg{err: fmt.Errorf("not connected to daemon")}
		}
		
		// Spawn worker with specific configuration
		resp, err := client.Send(socket.Request{
			Command: "spawn_agent",
			Args: map[string]interface{}{
				"repo":   repoName,
				"name":   fmt.Sprintf("worker-%s-%s", task.ID, agentName),
				"class":  "ephemeral",
				"prompt": prompt,
				"task":   task.Title,
				"model":  cp.selectModelForTask(task),
			},
		})
		
		if err != nil {
			return plannerErrorMsg{err: err}
		}
		if !resp.Success {
			return plannerErrorMsg{err: fmt.Errorf("dispatch failed: %s", resp.Error)}
		}
		
		// Track worker
		cp.activeWorkers[agentName] = WorkerStatus{
			Name:      agentName,
			TaskID:    task.ID,
			Task:      task,
			State:     "working",
			StartTime: time.Now(),
			LastSeen:  time.Now(),
		}
		
		return workerSpawnedMsg{worker: agentName, task: task}
	}
}

// Build enhanced worker prompt with full context
func (cp *CollaborativePlanner) buildEnhancedWorkerPrompt(task Task) string {
	var sb strings.Builder
	
	sb.WriteString("You are a specialized worker agent. Complete your assigned task with precision.\n\n")
	
	// Include operational spec if available
	if cp.requirement != nil && cp.requirement.OperationalSpec != "" {
		sb.WriteString("## Operational Specification\n")
		sb.WriteString(cp.requirement.OperationalSpec)
		sb.WriteString("\n\n")
	}
	
	// Include test strategy
	if cp.testStrategy != nil && task.Type == "test" {
		sb.WriteString("## Test Strategy\n")
		sb.WriteString(fmt.Sprintf("Unit: %s\n", cp.testStrategy.Unit))
		sb.WriteString(fmt.Sprintf("Integration: %s\n", cp.testStrategy.Integration))
		sb.WriteString(fmt.Sprintf("Blackbox: %s\n", cp.testStrategy.Blackbox))
		sb.WriteString("\n")
	}
	
	// Task details
	sb.WriteString("## Your Task\n")
	sb.WriteString(fmt.Sprintf("**ID:** %s\n", task.ID))
	sb.WriteString(fmt.Sprintf("**Title:** %s\n", task.Title))
	sb.WriteString(fmt.Sprintf("**Type:** %s\n", task.Type))
	sb.WriteString(fmt.Sprintf("**Wave:** %d\n", task.Wave))
	sb.WriteString("\n**Description:**\n")
	sb.WriteString(task.Description)
	sb.WriteString("\n\n")
	
	// Dependencies
	if len(task.Dependencies) > 0 {
		sb.WriteString("## Dependencies\n")
		sb.WriteString("The following tasks must be completed first:\n")
		for _, dep := range task.Dependencies {
			sb.WriteString(fmt.Sprintf("- %s\n", dep))
		}
		sb.WriteString("\n")
	}
	
	// Acceptance criteria
	if len(task.AcceptanceCriteria) > 0 {
		sb.WriteString("## Acceptance Criteria\n")
		for _, criteria := range task.AcceptanceCriteria {
			sb.WriteString(fmt.Sprintf("- %s\n", criteria))
		}
		sb.WriteString("\n")
	}
	
	// Spec reference
	if task.SpecReference != "" {
		sb.WriteString("## Specification Reference\n")
		sb.WriteString(task.SpecReference)
		sb.WriteString("\n\n")
	}
	
	// TDD flag
	if task.TestFirst {
		sb.WriteString("## Test-Driven Development\n")
		sb.WriteString("This task follows TDD. Write tests first, then implementation.\n\n")
	}
	
	// Coordination instructions
	sb.WriteString("## Coordination\n")
	sb.WriteString("- Run `oat agent complete` when your work is done\n")
	sb.WriteString("- Create a PR when implementation is ready\n")
	sb.WriteString("- Respond to review comments if needed\n")
	sb.WriteString("- Do NOT weaken CI or disable tests\n")
	
	return sb.String()
}

// Select optimal model for task
func (cp *CollaborativePlanner) selectModelForTask(task Task) string {
	// Complex tasks get stronger models
	if task.Type == "complex-implementation" || strings.Contains(task.Description, "architecture") {
		return "anthropic:claude-sonnet-4-6"
	}
	
	// Simple tasks can use lighter models
	if task.Type == "documentation" || strings.Contains(task.Description, "typo") {
		return "anthropic:claude-haiku-3"
	}
	
	// Default to auto-selection
	return ""
}

// Check if worker is stuck
func (cp *CollaborativePlanner) isWorkerStuck(worker WorkerStatus) bool {
	// Worker is stuck if:
	// 1. No activity for >10 minutes
	// 2. Nudged >5 times
	// 3. PR failing CI for >30 minutes
	
	idleTime := time.Since(worker.LastSeen)
	if idleTime > 10*time.Minute && worker.NudgeCount > 5 {
		return true
	}
	
	if worker.PR != nil && worker.PR.CIStatus == "failing" {
		prAge := time.Since(worker.StartTime)
		if prAge > 30*time.Minute {
			return true
		}
	}
	
	return false
}

// Handle stuck worker
func (cp *CollaborativePlanner) handleStuckWorker(name string, worker WorkerStatus) {
	// Send alert to feedback
	cp.feedback = append(cp.feedback, FeedbackEntry{
		Type:      "system",
		Content:   fmt.Sprintf("⚠️ Worker %s appears stuck on task %s", name, worker.Task.Title),
		Timestamp: time.Now(),
	})
	
	// Attempt recovery
	if worker.NudgeCount < 10 {
		// Nudge the worker
		cp.sendNudgeToWorker(name)
		worker.NudgeCount++
	} else {
		// Replace the worker
		cp.replaceStuckWorker(name, worker)
	}
}

// Handle worker completion
func (cp *CollaborativePlanner) handleWorkerCompletion(workerName string, taskID string) {
	worker, exists := cp.activeWorkers[workerName]
	if !exists {
		return
	}
	
	// Update task status
	for i, task := range cp.tasks {
		if task.ID == taskID {
			cp.tasks[i].Status = TaskStatusCompleted
			break
		}
	}
	
	// Remove from active workers
	delete(cp.activeWorkers, workerName)
	
	// Update workload balance
	cp.workloadBalance[workerName]--
	
	// Add to feedback
	cp.feedback = append(cp.feedback, FeedbackEntry{
		Type:      "system",
		Content:   fmt.Sprintf("✅ Worker %s completed task: %s", workerName, worker.Task.Title),
		Timestamp: time.Now(),
	})
}

// Check and dispatch next wave
func (cp *CollaborativePlanner) checkAndDispatchNextWave() tea.Cmd {
	// Check if current wave is complete
	currentWaveTasks := cp.executionWaves[cp.currentWave]
	allComplete := true
	
	for _, task := range currentWaveTasks {
		if task.Status != TaskStatusCompleted {
			allComplete = false
			break
		}
	}
	
	if !allComplete {
		return nil
	}
	
	// Move to next wave
	cp.currentWave++
	nextWaveTasks := cp.executionWaves[cp.currentWave]
	
	if len(nextWaveTasks) == 0 {
		// All waves complete
		cp.feedback = append(cp.feedback, FeedbackEntry{
			Type:      "system",
			Content:   "🎉 All waves complete! Plan execution finished.",
			Timestamp: time.Now(),
		})
		return nil
	}
	
	// Add next wave to pending
	cp.pendingTasks = append(cp.pendingTasks, nextWaveTasks...)
	
	// Dispatch available tasks
	return cp.dispatchAvailableTasks()
}

// Capture current state snapshot
func (cp *CollaborativePlanner) captureSnapshot() StatusSnapshot {
	completed := 0
	for _, task := range cp.tasks {
		if task.Status == TaskStatusCompleted {
			completed++
		}
	}
	
	workers := []WorkerStatus{}
	for _, w := range cp.activeWorkers {
		workers = append(workers, w)
	}
	
	return StatusSnapshot{
		Timestamp:      time.Now(),
		Workers:        workers,
		CompletedTasks: completed,
		TotalTasks:     len(cp.tasks),
		SystemHealth:   cp.assessSystemHealth(),
	}
}

// Assess overall system health
func (cp *CollaborativePlanner) assessSystemHealth() string {
	stuckCount := 0
	for _, worker := range cp.activeWorkers {
		if cp.isWorkerStuck(worker) {
			stuckCount++
		}
	}
	
	totalWorkers := len(cp.activeWorkers)
	if totalWorkers == 0 {
		return "healthy"
	}
	
	// If more than half workers are stuck, system is degraded
	// For single worker, only degrade if multiple issues
	if totalWorkers == 1 && stuckCount == 1 {
		return "warning"
	}
	if stuckCount > totalWorkers/2 {
		return "degraded"
	}
	// If any workers are stuck, system is in warning state
	if stuckCount > 0 {
		return "warning"
	}
	return "healthy"
}

// Get available agents
func (cp *CollaborativePlanner) getAvailableAgents() []AgentProfile {
	available := []AgentProfile{}
	
	for _, profile := range cp.agentProfiles {
		if profile.CurrentLoad < profile.MaxTasks {
			available = append(available, profile)
		}
	}
	
	return available
}

// Helper functions
func (cp *CollaborativePlanner) removeTask(tasks []Task, target Task) []Task {
	result := []Task{}
	for _, t := range tasks {
		if t.ID != target.ID {
			result = append(result, t)
		}
	}
	return result
}

func (cp *CollaborativePlanner) sendNudgeToWorker(name string) {
	// Implementation would send actual nudge via daemon
	cp.messageQueue = append(cp.messageQueue, AgentMessage{
		From:      "planner",
		To:        name,
		Type:      "status",
		Content:   "Status check: Please report progress on your current task.",
		Timestamp: time.Now(),
	})
}

func (cp *CollaborativePlanner) replaceStuckWorker(name string, worker WorkerStatus) {
	// Mark original task as pending again
	cp.pendingTasks = append(cp.pendingTasks, worker.Task)
	
	// Remove stuck worker
	delete(cp.activeWorkers, name)
	
	cp.feedback = append(cp.feedback, FeedbackEntry{
		Type:      "system",
		Content:   fmt.Sprintf("Replacing stuck worker %s, reassigning task: %s", name, worker.Task.Title),
		Timestamp: time.Now(),
	})
}

func (cp *CollaborativePlanner) handlePRMerged(prNumber int) {
	cp.feedback = append(cp.feedback, FeedbackEntry{
		Type:      "system",
		Content:   fmt.Sprintf("✅ PR #%d merged successfully", prNumber),
		Timestamp: time.Now(),
	})
}

func (cp *CollaborativePlanner) handleStatusUpdate(snapshot StatusSnapshot) {
	cp.statusSnapshots = append(cp.statusSnapshots, snapshot)
	cp.lastSnapshot = time.Now()
}

// Enhanced View with orchestration status
func (cp *CollaborativePlanner) View() string {
	baseView := cp.PlannerView.View()
	
	// Add orchestration status panel
	orchestrationPanel := cp.renderOrchestrationPanel()
	
	return lipgloss.JoinVertical(lipgloss.Left, baseView, orchestrationPanel)
}

func (cp *CollaborativePlanner) renderOrchestrationPanel() string {
	if cp.state != StateExecuting {
		return ""
	}
	
	var content strings.Builder
	
	content.WriteString(lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("34")).
		Render("🎯 Orchestration Status"))
	content.WriteString("\n\n")
	
	// Active workers
	content.WriteString(fmt.Sprintf("Active Workers: %d\n", len(cp.activeWorkers)))
	for name, worker := range cp.activeWorkers {
		status := "🔄"
		if worker.State == "stuck" {
			status = "⚠️"
		} else if worker.State == "pr_open" {
			status = "🔀"
		}
		content.WriteString(fmt.Sprintf("  %s %s: %s (%.0f%%)\n", 
			status, name, worker.Task.Title, worker.Progress*100))
	}
	
	// Wave progress
	content.WriteString(fmt.Sprintf("\nCurrent Wave: %d\n", cp.currentWave))
	content.WriteString(fmt.Sprintf("Pending Tasks: %d\n", len(cp.pendingTasks)))
	
	// System health
	health := "🟢"
	if cp.assessSystemHealth() == "warning" {
		health = "🟡"
	} else if cp.assessSystemHealth() == "degraded" {
		health = "🔴"
	}
	content.WriteString(fmt.Sprintf("System Health: %s\n", health))
	
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("34")).
		Padding(1).
		Render(content.String())
}

// Message types for orchestration
type statusUpdateMsg struct {
	snapshot StatusSnapshot
}

type workerCompleteMsg struct {
	workerName string
	taskID     string
}

type workerSpawnedMsg struct {
	worker string
	task   Task
}

type prMergedMsg struct {
	prNumber int
}

type orchestrationTickMsg time.Time