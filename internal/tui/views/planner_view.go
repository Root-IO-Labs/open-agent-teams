package views

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Root-IO-Labs/open-agent-teams/internal/socket"
)

// PlannerState represents the current state of the planning session
type PlannerState int

const (
	StateDefiningRequirement PlannerState = iota
	StateRefiningRequirement
	StateDecomposingTasks
	StateReviewingPlan
	StatePlanLocked
	StateExecuting
)

// Requirement represents a user requirement being refined
type Requirement struct {
	ID          string
	Original    string
	Refined     string
	Iteration   int
	LastUpdated time.Time
}

// Task represents an atomic task in the decomposed plan
type Task struct {
	ID           string
	Title        string
	Description  string
	Dependencies []string
	Wave         int // Execution wave (parallel tasks have same wave)
	Status       TaskStatus
	AssignedTo   string // Worker agent name
	EstimatedDuration time.Duration
}

type TaskStatus int

const (
	TaskStatusPending TaskStatus = iota
	TaskStatusInProgress
	TaskStatusCompleted
	TaskStatusFailed
	TaskStatusBlocked
)

// PlannerView handles collaborative requirement definition and task decomposition
type PlannerView struct {
	// State
	state       PlannerState
	requirement *Requirement
	tasks       []Task
	isLocked    bool
	thinking    bool
	
	// Input handling
	input       textinput.Model
	inputPrompt string
	
	// UI state
	width       int
	height      int
	scrollY     int
	selectedTask int
	
	// Communication
	client      *socket.Client
	repoName    string
	
	// Feedback and collaboration
	feedback    []FeedbackEntry
	lastAIResponse time.Time
	thinkingText   string
	
	// Key bindings
	keyMap PlannerKeyMap
}

type FeedbackEntry struct {
	Type      string    // "user", "ai", "system"
	Content   string
	Timestamp time.Time
}

type PlannerKeyMap struct {
	NewRequirement key.Binding
	RefineReq      key.Binding
	ApprovePlan    key.Binding
	RejectPlan     key.Binding
	LockPlan       key.Binding
	UnlockPlan     key.Binding
	Execute        key.Binding
	Back           key.Binding
	ScrollUp       key.Binding
	ScrollDown     key.Binding
	SelectTask     key.Binding
	Help           key.Binding
}

// NewPlannerView creates a new planner view instance
func NewPlannerView(client *socket.Client, repoName string) *PlannerView {
	ti := textinput.New()
	ti.Prompt = ""
	ti.Placeholder = "Describe what you want to accomplish..."
	ti.Focus()
	ti.CharLimit = 1000
	ti.Width = 80

	return &PlannerView{
		state:       StateDefiningRequirement,
		input:       ti,
		inputPrompt: "Requirement",
		client:      client,
		repoName:    repoName,
		selectedTask: -1,
		keyMap:      defaultPlannerKeys(),
		feedback:    []FeedbackEntry{
			{
				Type:      "system", 
				Content:   "Welcome to the OAT Planner! Describe your requirements and I'll help break them down into executable tasks.",
				Timestamp: time.Now(),
			},
		},
	}
}

func defaultPlannerKeys() PlannerKeyMap {
	return PlannerKeyMap{
		NewRequirement: key.NewBinding(
			key.WithKeys("n"),
			key.WithHelp("n", "new requirement"),
		),
		RefineReq: key.NewBinding(
			key.WithKeys("r"),
			key.WithHelp("r", "refine requirement"),
		),
		ApprovePlan: key.NewBinding(
			key.WithKeys("a"),
			key.WithHelp("a", "approve plan"),
		),
		RejectPlan: key.NewBinding(
			key.WithKeys("x"),
			key.WithHelp("x", "reject plan"),
		),
		LockPlan: key.NewBinding(
			key.WithKeys("l"),
			key.WithHelp("l", "lock plan"),
		),
		UnlockPlan: key.NewBinding(
			key.WithKeys("u"),
			key.WithHelp("u", "unlock plan"),
		),
		Execute: key.NewBinding(
			key.WithKeys("e"),
			key.WithHelp("e", "execute plan"),
		),
		Back: key.NewBinding(
			key.WithKeys("esc"),
			key.WithHelp("esc", "back"),
		),
		ScrollUp: key.NewBinding(
			key.WithKeys("up", "k"),
			key.WithHelp("↑/k", "scroll up"),
		),
		ScrollDown: key.NewBinding(
			key.WithKeys("down", "j"),
			key.WithHelp("↓/j", "scroll down"),
		),
		SelectTask: key.NewBinding(
			key.WithKeys("enter"),
			key.WithHelp("enter", "select task"),
		),
		Help: key.NewBinding(
			key.WithKeys("?"),
			key.WithHelp("?", "toggle help"),
		),
	}
}

// Update handles bubbletea updates
func (p *PlannerView) Update(msg tea.Msg) (*PlannerView, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		p.width = msg.Width
		p.height = msg.Height
		p.input.Width = msg.Width - 20
		return p, nil

	case tea.KeyMsg:
		return p.handleKey(msg)

	case plannerThinkingMsg:
		p.thinking = true
		p.thinkingText = msg.text
		return p, p.tickThinking()

	case plannerResponseMsg:
		p.thinking = false
		p.lastAIResponse = time.Now()
		p.handleAIResponse(msg)
		return p, nil

	case plannerTickMsg:
		// Update thinking animation
		if p.thinking {
			cmds = append(cmds, p.tickThinking())
		}
		return p, tea.Batch(cmds...)
	}

	// Update input
	var cmd tea.Cmd
	p.input, cmd = p.input.Update(msg)
	cmds = append(cmds, cmd)

	return p, tea.Batch(cmds...)
}

func (p *PlannerView) handleKey(msg tea.KeyMsg) (*PlannerView, tea.Cmd) {
	switch {
	case key.Matches(msg, p.keyMap.NewRequirement):
		p.startNewRequirement()
		return p, nil

	case key.Matches(msg, p.keyMap.RefineReq):
		if p.requirement != nil {
			return p, p.refineRequirement()
		}
		return p, nil

	case key.Matches(msg, p.keyMap.ApprovePlan):
		if p.state == StateReviewingPlan {
			p.approvePlan()
		}
		return p, nil

	case key.Matches(msg, p.keyMap.RejectPlan):
		if p.state == StateReviewingPlan {
			return p, p.rejectPlan()
		}
		return p, nil

	case key.Matches(msg, p.keyMap.LockPlan):
		if len(p.tasks) > 0 && !p.isLocked {
			p.lockPlan()
		}
		return p, nil

	case key.Matches(msg, p.keyMap.UnlockPlan):
		if p.isLocked {
			p.unlockPlan()
		}
		return p, nil

	case key.Matches(msg, p.keyMap.Execute):
		if p.isLocked && p.state == StatePlanLocked {
			return p, p.executePlan()
		}
		return p, nil

	case key.Matches(msg, p.keyMap.ScrollUp):
		if p.scrollY > 0 {
			p.scrollY--
		}
		return p, nil

	case key.Matches(msg, p.keyMap.ScrollDown):
		p.scrollY++
		return p, nil

	case key.Matches(msg, p.keyMap.SelectTask):
		if len(p.tasks) > 0 {
			p.selectedTask = (p.selectedTask + 1) % len(p.tasks)
		}
		return p, nil

	case msg.Type == tea.KeyEnter && p.input.Focused():
		return p.handleInput()
	}

	return p, nil
}

func (p *PlannerView) handleInput() (*PlannerView, tea.Cmd) {
	text := strings.TrimSpace(p.input.Value())
	if text == "" {
		return p, nil
	}

	p.input.SetValue("")

	// Add user input to feedback
	p.feedback = append(p.feedback, FeedbackEntry{
		Type:      "user",
		Content:   text,
		Timestamp: time.Now(),
	})

	switch p.state {
	case StateDefiningRequirement:
		p.requirement = &Requirement{
			ID:          fmt.Sprintf("req-%d", time.Now().Unix()),
			Original:    text,
			Refined:     text,
			Iteration:   1,
			LastUpdated: time.Now(),
		}
		p.state = StateRefiningRequirement
		return p, p.sendToOverlord("refine_requirement", map[string]interface{}{
			"requirement": text,
			"context":     p.buildContext(),
		})

	case StateRefiningRequirement, StateReviewingPlan:
		// Send feedback to overlord
		return p, p.sendToOverlord("process_feedback", map[string]interface{}{
			"feedback":    text,
			"requirement": p.requirement.Refined,
			"tasks":       p.tasksToMap(),
			"context":     p.buildContext(),
		})

	default:
		// General chat/feedback
		return p, p.sendToOverlord("chat", map[string]interface{}{
			"message": text,
			"context": p.buildContext(),
		})
	}
}

func (p *PlannerView) startNewRequirement() {
	p.state = StateDefiningRequirement
	p.requirement = nil
	p.tasks = nil
	p.isLocked = false
	p.selectedTask = -1
	p.input.Placeholder = "Describe what you want to accomplish..."
	p.input.Focus()
	p.feedback = append(p.feedback, FeedbackEntry{
		Type:      "system",
		Content:   "Starting new requirement definition. What would you like to accomplish?",
		Timestamp: time.Now(),
	})
}

func (p *PlannerView) approvePlan() {
	p.state = StatePlanLocked
	p.isLocked = true
	p.feedback = append(p.feedback, FeedbackEntry{
		Type:      "system",
		Content:   "Plan approved and locked. You can now execute the plan or unlock to make changes.",
		Timestamp: time.Now(),
	})
}

func (p *PlannerView) lockPlan() {
	p.isLocked = true
	p.state = StatePlanLocked
	p.feedback = append(p.feedback, FeedbackEntry{
		Type:      "system",
		Content:   "Plan locked. Use 'e' to execute or 'u' to unlock for changes.",
		Timestamp: time.Now(),
	})
}

func (p *PlannerView) unlockPlan() {
	p.isLocked = false
	p.state = StateReviewingPlan
	p.feedback = append(p.feedback, FeedbackEntry{
		Type:      "system",
		Content:   "Plan unlocked. You can now provide feedback or make changes.",
		Timestamp: time.Now(),
	})
}

// Commands for async operations
func (p *PlannerView) sendToOverlord(command string, args map[string]interface{}) tea.Cmd {
	return func() tea.Msg {
		// First show thinking state
		thinking := plannerThinkingMsg{text: "Thinking..."}
		
		// For now, simulate overlord response since we don't have the actual overlord agent
		// In a real implementation, this would send to the overlord agent via socket
		
		// Simulate processing time
		time.Sleep(500 * time.Millisecond)
		
		switch command {
		case "refine_requirement":
			return plannerResponseMsg{
				responseType: "requirement_refined",
				data: map[string]interface{}{
					"refined_requirement": args["requirement"].(string) + " (refined with better specificity and clear acceptance criteria)",
					"questions": []string{
						"Should this be implemented as a web interface or CLI tool?",
						"What's the expected scale/performance requirements?",
						"Are there any security considerations?",
					},
				},
			}
		case "process_feedback":
			return plannerResponseMsg{
				responseType: "tasks_decomposed",
				data: map[string]interface{}{
					"tasks": []map[string]interface{}{
						{
							"id": "task-1",
							"title": "Setup project structure",
							"description": "Initialize project with proper directory structure and dependencies",
							"wave": 1,
							"dependencies": []string{},
							"estimated_duration": "30m",
						},
						{
							"id": "task-2", 
							"title": "Implement core functionality",
							"description": "Build the main features according to requirements",
							"wave": 2,
							"dependencies": []string{"task-1"},
							"estimated_duration": "2h",
						},
						{
							"id": "task-3",
							"title": "Add testing",
							"description": "Write unit and integration tests",
							"wave": 3,
							"dependencies": []string{"task-2"},
							"estimated_duration": "1h",
						},
					},
				},
			}
		default:
			return plannerResponseMsg{
				responseType: "chat_response",
				data: map[string]interface{}{
					"response": "I understand. Let me help you with that.",
				},
			}
		}
		
		return thinking
	}
}

func (p *PlannerView) refineRequirement() tea.Cmd {
	return p.sendToOverlord("refine_requirement", map[string]interface{}{
		"requirement": p.requirement.Refined,
		"iteration":   p.requirement.Iteration + 1,
		"context":     p.buildContext(),
	})
}

func (p *PlannerView) rejectPlan() tea.Cmd {
	p.state = StateRefiningRequirement
	p.feedback = append(p.feedback, FeedbackEntry{
		Type:    "system",
		Content: "Plan rejected. Please provide feedback on what needs to change.",
		Timestamp: time.Now(),
	})
	return nil
}

func (p *PlannerView) executePlan() tea.Cmd {
	p.state = StateExecuting
	p.feedback = append(p.feedback, FeedbackEntry{
		Type:    "system",
		Content: "Executing plan... Workers will be spawned for each task wave.",
		Timestamp: time.Now(),
	})
	
	return p.sendToOverlord("execute_plan", map[string]interface{}{
		"tasks":       p.tasksToMap(),
		"requirement": p.requirement.Refined,
	})
}

func (p *PlannerView) tickThinking() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(t time.Time) tea.Msg {
		return plannerTickMsg(t)
	})
}

func (p *PlannerView) handleAIResponse(msg plannerResponseMsg) {
	switch msg.responseType {
	case "requirement_refined":
		if data, ok := msg.data.(map[string]interface{}); ok {
			if refined, ok := data["refined_requirement"].(string); ok {
				p.requirement.Refined = refined
				p.requirement.Iteration++
				p.requirement.LastUpdated = time.Now()
			}
			
			responseText := "Requirement refined: " + p.requirement.Refined
			if questions, ok := data["questions"].([]string); ok && len(questions) > 0 {
				responseText += "\n\nQuestions for clarification:\n"
				for i, q := range questions {
					responseText += fmt.Sprintf("%d. %s\n", i+1, q)
				}
			}
			
			p.feedback = append(p.feedback, FeedbackEntry{
				Type:      "ai",
				Content:   responseText,
				Timestamp: time.Now(),
			})
		}
		p.state = StateDecomposingTasks
		
	case "tasks_decomposed":
		p.parseTasksFromResponse(msg.data)
		p.state = StateReviewingPlan
		p.feedback = append(p.feedback, FeedbackEntry{
			Type:      "ai",
			Content:   fmt.Sprintf("I've decomposed your requirement into %d tasks across %d execution waves. Please review the plan below.", len(p.tasks), p.getMaxWave()),
			Timestamp: time.Now(),
		})
		
	case "chat_response":
		if data, ok := msg.data.(map[string]interface{}); ok {
			if response, ok := data["response"].(string); ok {
				p.feedback = append(p.feedback, FeedbackEntry{
					Type:      "ai",
					Content:   response,
					Timestamp: time.Now(),
				})
			}
		}
	}
}

func (p *PlannerView) parseTasksFromResponse(data interface{}) {
	if dataMap, ok := data.(map[string]interface{}); ok {
		if tasksData, ok := dataMap["tasks"].([]map[string]interface{}); ok {
			p.tasks = nil
			for _, taskData := range tasksData {
				task := Task{
					ID:          getString(taskData, "id"),
					Title:       getString(taskData, "title"), 
					Description: getString(taskData, "description"),
					Wave:        getInt(taskData, "wave"),
					Status:      TaskStatusPending,
				}
				
				if deps, ok := taskData["dependencies"].([]string); ok {
					task.Dependencies = deps
				}
				
				if durStr, ok := taskData["estimated_duration"].(string); ok {
					if dur, err := time.ParseDuration(durStr); err == nil {
						task.EstimatedDuration = dur
					}
				}
				
				p.tasks = append(p.tasks, task)
			}
		}
	}
}

func (p *PlannerView) buildContext() map[string]interface{} {
	return map[string]interface{}{
		"repo":         p.repoName,
		"state":        p.state,
		"has_requirement": p.requirement != nil,
		"task_count":   len(p.tasks),
		"locked":       p.isLocked,
	}
}

func (p *PlannerView) tasksToMap() []map[string]interface{} {
	result := make([]map[string]interface{}, len(p.tasks))
	for i, task := range p.tasks {
		result[i] = map[string]interface{}{
			"id":                task.ID,
			"title":             task.Title,
			"description":       task.Description,
			"dependencies":      task.Dependencies,
			"wave":              task.Wave,
			"status":            task.Status,
			"assigned_to":       task.AssignedTo,
			"estimated_duration": task.EstimatedDuration.String(),
		}
	}
	return result
}

func (p *PlannerView) getMaxWave() int {
	maxWave := 0
	for _, task := range p.tasks {
		if task.Wave > maxWave {
			maxWave = task.Wave
		}
	}
	return maxWave
}

// View renders the planner view
func (p *PlannerView) View() string {
	if p.width <= 0 || p.height <= 0 {
		return "Initializing planner..."
	}

	sections := []string{
		p.renderHeader(),
		p.renderMainContent(),
		p.renderInputBar(),
		p.renderHelp(),
	}

	return lipgloss.JoinVertical(lipgloss.Left, sections...)
}

func (p *PlannerView) renderHeader() string {
	title := "🚀 OAT Planner - Collaborative Task Planning"
	
	stateInfo := ""
	switch p.state {
	case StateDefiningRequirement:
		stateInfo = "Defining Requirement"
	case StateRefiningRequirement:
		stateInfo = "Refining Requirement"
	case StateDecomposingTasks:
		stateInfo = "Decomposing Tasks"
	case StateReviewingPlan:
		stateInfo = "Reviewing Plan"
	case StatePlanLocked:
		if p.isLocked {
			stateInfo = "Plan Locked ✓"
		} else {
			stateInfo = "Plan Ready"
		}
	case StateExecuting:
		stateInfo = "Executing Plan..."
	}

	rightInfo := stateInfo
	if p.requirement != nil {
		rightInfo += fmt.Sprintf(" | Tasks: %d | Waves: %d", len(p.tasks), p.getMaxWave())
	}

	// Calculate spacing
	titleW := lipgloss.Width(title)
	rightW := lipgloss.Width(rightInfo)
	gap := p.width - titleW - rightW
	if gap < 2 {
		gap = 2
	}

	header := title + strings.Repeat(" ", gap) + rightInfo

	return lipgloss.NewStyle().
		Width(p.width).
		Background(lipgloss.Color("62")).
		Foreground(lipgloss.Color("15")).
		Padding(0, 1).
		Render(header)
}

func (p *PlannerView) renderMainContent() string {
	contentHeight := p.height - 6 // header + input + help + padding
	if contentHeight < 5 {
		contentHeight = 5
	}

	var content strings.Builder

	// Render current requirement
	if p.requirement != nil {
		content.WriteString(p.renderRequirement())
		content.WriteString("\n")
	}

	// Render tasks if any
	if len(p.tasks) > 0 {
		content.WriteString(p.renderTasks())
		content.WriteString("\n")
	}

	// Render conversation/feedback
	content.WriteString(p.renderFeedback())

	// Render thinking indicator
	if p.thinking {
		content.WriteString("\n")
		content.WriteString(p.renderThinking())
	}

	return lipgloss.NewStyle().
		Width(p.width - 2).
		Height(contentHeight).
		Padding(1).
		Render(content.String())
}

func (p *PlannerView) renderRequirement() string {
	if p.requirement == nil {
		return ""
	}

	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("62")).
		Padding(1).
		Margin(0, 0, 1, 0)

	title := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("62")).
		Render(fmt.Sprintf("Requirement (v%d)", p.requirement.Iteration))

	content := fmt.Sprintf("%s\n\n%s", title, p.requirement.Refined)
	
	if p.requirement.Original != p.requirement.Refined {
		content += fmt.Sprintf("\n\nOriginal: %s", 
			lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render(p.requirement.Original))
	}

	return style.Render(content)
}

func (p *PlannerView) renderTasks() string {
	if len(p.tasks) == 0 {
		return ""
	}

	var content strings.Builder
	
	title := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("34")).
		Render("📋 Task Breakdown")
	
	content.WriteString(title)
	content.WriteString("\n\n")

	// Group tasks by wave
	waves := make(map[int][]Task)
	for _, task := range p.tasks {
		waves[task.Wave] = append(waves[task.Wave], task)
	}

	for wave := 1; wave <= p.getMaxWave(); wave++ {
		if tasks, exists := waves[wave]; exists {
			waveTitle := lipgloss.NewStyle().
				Foreground(lipgloss.Color("220")).
				Render(fmt.Sprintf("Wave %d (Parallel Execution):", wave))
			content.WriteString(waveTitle)
			content.WriteString("\n")

			for i, task := range tasks {
				status := "⏳"
				switch task.Status {
				case TaskStatusInProgress:
					status = "🔄"
				case TaskStatusCompleted:
					status = "✅"
				case TaskStatusFailed:
					status = "❌"
				case TaskStatusBlocked:
					status = "🚫"
				}

				taskStyle := lipgloss.NewStyle()
				if i == p.selectedTask {
					taskStyle = taskStyle.Background(lipgloss.Color("237"))
				}

				taskLine := fmt.Sprintf("  %s %s", status, task.Title)
				if task.EstimatedDuration > 0 {
					taskLine += fmt.Sprintf(" (%s)", task.EstimatedDuration)
				}
				
				content.WriteString(taskStyle.Render(taskLine))
				content.WriteString("\n")
				
				if task.Description != "" {
					desc := lipgloss.NewStyle().
						Foreground(lipgloss.Color("8")).
						Render(fmt.Sprintf("    %s", task.Description))
					content.WriteString(desc)
					content.WriteString("\n")
				}

				if len(task.Dependencies) > 0 {
					deps := lipgloss.NewStyle().
						Foreground(lipgloss.Color("3")).
						Render(fmt.Sprintf("    Depends on: %s", strings.Join(task.Dependencies, ", ")))
					content.WriteString(deps)
					content.WriteString("\n")
				}
			}
			content.WriteString("\n")
		}
	}

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("34")).
		Padding(1).
		Margin(0, 0, 1, 0).
		Render(content.String())
}

func (p *PlannerView) renderFeedback() string {
	var content strings.Builder

	title := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("205")).
		Render("💬 Conversation")
	
	content.WriteString(title)
	content.WriteString("\n\n")

	// Show recent feedback (last 10 entries)
	start := 0
	if len(p.feedback) > 10 {
		start = len(p.feedback) - 10
	}

	for _, entry := range p.feedback[start:] {
		timestamp := entry.Timestamp.Format("15:04")
		
		var prefix string
		var style lipgloss.Style
		switch entry.Type {
		case "user":
			prefix = lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Render("You:")
			style = lipgloss.NewStyle()
		case "ai":
			prefix = lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Render("AI:")
			style = lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
		case "system":
			prefix = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render("System:")
			style = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
		}

		timeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
		
		content.WriteString(fmt.Sprintf("%s %s %s\n", 
			timeStyle.Render("["+timestamp+"]"), 
			prefix, 
			style.Render(entry.Content)))
		content.WriteString("\n")
	}

	return content.String()
}

func (p *PlannerView) renderThinking() string {
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	frame := frames[(int(time.Now().UnixMilli()/200))%len(frames)]
	
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color("8")).
		Render(fmt.Sprintf("%s %s", frame, p.thinkingText))
}

func (p *PlannerView) renderInputBar() string {
	prompt := lipgloss.NewStyle().
		Foreground(lipgloss.Color("62")).
		Render(p.inputPrompt + ": ")

	input := p.input.View()
	
	return lipgloss.NewStyle().
		Width(p.width).
		Padding(0, 1).
		Render(prompt + input)
}

func (p *PlannerView) renderHelp() string {
	var helps []string

	switch p.state {
	case StateDefiningRequirement:
		helps = []string{"Enter: submit requirement", "esc: back"}
	case StateRefiningRequirement, StateDecomposingTasks:
		helps = []string{"Enter: provide feedback", "r: refine", "esc: back"}
	case StateReviewingPlan:
		helps = []string{"a: approve", "x: reject", "l: lock", "Enter: feedback", "esc: back"}
	case StatePlanLocked:
		if p.isLocked {
			helps = []string{"e: execute", "u: unlock", "esc: back"}
		} else {
			helps = []string{"l: lock", "Enter: feedback", "esc: back"}
		}
	case StateExecuting:
		helps = []string{"esc: back"}
	}

	helps = append(helps, "n: new requirement")

	helpText := strings.Join(helps, " • ")
	
	return lipgloss.NewStyle().
		Width(p.width).
		Foreground(lipgloss.Color("8")).
		Padding(0, 1).
		Render(helpText)
}

// Message types for bubbletea
type plannerThinkingMsg struct {
	text string
}

type plannerResponseMsg struct {
	responseType string
	data         interface{}
}

type plannerTickMsg time.Time

// Helper functions
func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func getInt(m map[string]interface{}, key string) int {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		case int64:
			return int(n)
		}
	}
	return 0
}