package views

import (
	"encoding/json"
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
	ID              string
	Original        string
	Refined         string
	OperationalSpec string // Overlord: How the system works
	Iteration       int
	LastUpdated     time.Time
}

// Task represents an atomic task in the decomposed plan
type Task struct {
	ID                 string
	Title              string
	Description        string
	Type               string   // test|implementation|documentation (Overlord)
	Dependencies       []string
	AcceptanceCriteria []string
	Wave               int // Execution wave (parallel tasks have same wave, Wave 0 = foundation)
	SpecReference      string // Reference to operational spec section (Overlord)
	TestFirst          bool   // TDD flag for implementation tasks (Overlord)
	Status             TaskStatus
	AssignedTo         string // Worker agent name
	EstimatedDuration  time.Duration
}

type TaskStatus int

const (
	TaskStatusPending TaskStatus = iota
	TaskStatusInProgress
	TaskStatusCompleted
	TaskStatusFailed
	TaskStatusBlocked
)

// PlannerResponse is the structured JSON the planner agent emits every turn.
type PlannerResponse struct {
	Phase        string              `json:"phase"`
	Message      string              `json:"message"`
	Questions    []string            `json:"questions"`
	Requirement  *PlannerRequirement `json:"requirement"`
	TestStrategy *TestStrategy       `json:"test_strategy"`
	Tasks        []PlannerTask       `json:"tasks"`
	// Action signals a system-level intent from the planner:
	// "advance_phase", "dispatch_tasks", "revise", "clarify", or "none".
	Action   string `json:"action"`
	PlanID   string `json:"plan_id"`
	SavePath string `json:"save_path"`
}

// PlannerRequirement is the requirement block inside a PlannerResponse.
type PlannerRequirement struct {
	Title           string `json:"title"`
	Original        string `json:"original"`
	Refined         string `json:"refined"`
	OperationalSpec string `json:"operational_spec"`
}

// TestStrategy defines the testing approach (Overlord methodology)
type TestStrategy struct {
	Unit       string `json:"unit"`
	Integration string `json:"integration"`
	Blackbox   string `json:"blackbox"`
	GateScript string `json:"gate_script"`
}

// PlannerTask is a single task inside a PlannerResponse.
type PlannerTask struct {
	ID                 string   `json:"id"`
	Title              string   `json:"title"`
	Description        string   `json:"description"`
	Type               string   `json:"type"`           // test|implementation|documentation
	Wave               int      `json:"wave"`
	Dependencies       []string `json:"dependencies"`
	SpecReference      string   `json:"spec_reference"`  // Reference to operational spec section
	TestFirst          bool     `json:"test_first"`      // TDD flag for implementation tasks
	AcceptanceCriteria []string `json:"acceptance_criteria"`
}

// PlannerView handles collaborative requirement definition and task decomposition
type PlannerView struct {
	// State
	state        PlannerState
	requirement  *Requirement
	testStrategy *TestStrategy // Overlord: Test strategy
	tasks        []Task
	isLocked     bool
	thinking     bool
	
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
	feedback      []FeedbackEntry
	thinkingText  string
	plannerBuffer string // accumulates planner output for JSON detection
	
	// Enhanced contextual awareness (Overlord integration)
	context           *PlannerContext
	currentGate       *PhaseGate
	pendingQuestions  []string
	brainstormThemes  []BrainstormTheme
	
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

	p := &PlannerView{
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
	
	// Initialize enhanced fields
	p.initializeEnhancements()
	
	return p
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

	case plannerSentMsg:
		p.thinking = true
		p.thinkingText = "Planner is thinking..."
		return p, p.tickThinking()

	case plannerDispatchedMsg:
		p.thinking = false
		p.state = StateExecuting
		target := msg.target
		if target == "" {
			target = "workspace"
		}
		p.feedback = append(p.feedback, FeedbackEntry{
			Type:      "system",
			Content:   fmt.Sprintf("Plan dispatched to %s agent. Workers are starting.", target),
			Timestamp: time.Now(),
		})
		return p, nil

	case plannerErrorMsg:
		p.thinking = false
		errText := "Error communicating with planner agent"
		if msg.err != nil {
			errText += ": " + msg.err.Error()
		}
		p.feedback = append(p.feedback, FeedbackEntry{
			Type:      "system",
			Content:   errText,
			Timestamp: time.Now(),
		})
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
	// Enter submits the current input text to the planner state machine.
	// handleInputEnhanced wraps handleInput with contextual intent detection
	// (approval signals, completion signals, revision requests).
	if msg.Type == tea.KeyEnter && p.input.Focused() {
		return p.handleInputEnhanced()
	}

	// Action shortcuts use ctrl-prefixed bindings so they don't collide with
	// regular typing (the prior keymap intercepted plain letters like n, r,
	// a, x, l, u, e, j, k — which made it impossible to type any word
	// containing those letters into the requirement input).
	if msg.Type == tea.KeyCtrlN {
		p.startNewRequirement()
		return p, nil
	}
	if msg.Type == tea.KeyCtrlR && p.requirement != nil {
		return p, p.refineRequirement()
	}
	// Ctrl+P — stop the planner (interrupt any running tool) and request the
	// current plan as structured JSON so the TUI can populate tasks/waves and
	// dispatch workers. Works at any point in the conversation.
	if msg.Type == tea.KeyCtrlP && p.requirement != nil {
		return p, p.stopAndPullPlan()
	}
	// Ctrl+A — approve the plan and dispatch to workspace/workers.
	// If tasks haven't been parsed yet, prompt the user to use Ctrl+P first.
	if msg.Type == tea.KeyCtrlA {
		return p, p.approvePlan()
	}
	if msg.Type == tea.KeyCtrlX && p.state == StateReviewingPlan {
		return p, p.rejectPlan()
	}

	// Everything else — including printable characters, backspace, arrows,
	// home/end, etc. — goes to the textinput so the user can actually type.
	var cmd tea.Cmd
	p.input, cmd = p.input.Update(msg)
	return p, cmd
}

func (p *PlannerView) handleInput() (*PlannerView, tea.Cmd) {
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

	return p, p.sendToPlanner(text)
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

// approvePlan locks the plan and dispatches it. If no tasks have been parsed
// yet, it prompts the user to run Ctrl+P (pull plan as JSON) first.
func (p *PlannerView) approvePlan() tea.Cmd {
	if len(p.tasks) == 0 {
		p.feedback = append(p.feedback, FeedbackEntry{
			Type:      "system",
			Content:   "No tasks to dispatch yet. Press ^p to stop the planner and pull the plan as JSON.",
			Timestamp: time.Now(),
		})
		return nil
	}
	p.state = StatePlanLocked
	p.isLocked = true
	p.feedback = append(p.feedback, FeedbackEntry{
		Type:      "system",
		Content:   fmt.Sprintf("Plan approved. Dispatching %d tasks (%d waves) to workspace agent...", len(p.tasks), p.getMaxWave()),
		Timestamp: time.Now(),
	})
	return p.dispatchToWorkspace()
}

// stopAndPullPlan interrupts the planner agent (stopping any in-flight tool
// use) and sends it a message requesting its current plan as structured JSON.
// Use this when the planner has started implementing instead of planning.
func (p *PlannerView) stopAndPullPlan() tea.Cmd {
	p.feedback = append(p.feedback, FeedbackEntry{
		Type:      "system",
		Content:   "Interrupting planner and requesting plan as JSON...",
		Timestamp: time.Now(),
	})

	client := p.client
	repoName := p.repoName
	extractMsg := fmt.Sprintf(
		"[planner-tui phase=ready_for_review]\n"+
			"STOP all implementation work immediately. You are the PLANNER, not the implementer. "+
			"Output your complete current plan as a single JSON code block (```json ... ```) "+
			"in the required format with phase, message, requirement, and tasks fields. "+
			"Include wave numbers, dependencies, and acceptance_criteria for every task. "+
			"Do NOT create files, run commands, or implement anything.",
	)

	return func() tea.Msg {
		if client == nil {
			return plannerErrorMsg{err: fmt.Errorf("not connected to daemon")}
		}
		// Interrupt any running tool first so the message lands on a clean turn.
		_, _ = client.Send(socket.Request{
			Command: "interrupt_agent",
			Args:    map[string]interface{}{"repo": repoName, "agent": "planner"},
		})
		resp, err := client.Send(socket.Request{
			Command: "send_agent_input",
			Args: map[string]interface{}{
				"repo":    repoName,
				"agent":   "planner",
				"message": extractMsg,
			},
		})
		if err != nil {
			return plannerErrorMsg{err: err}
		}
		if !resp.Success {
			return plannerErrorMsg{err: fmt.Errorf("%s", resp.Error)}
		}
		return plannerSentMsg{}
	}
}

// dispatchToWorkspace sends the approved plan to the workspace agent so it
// can spawn workers in the correct waves. Falls back to direct spawn_agent
// calls for Wave 1 if the workspace agent is unreachable.
func (p *PlannerView) dispatchToWorkspace() tea.Cmd {
	client := p.client
	repoName := p.repoName
	handoffMsg := p.buildWorkspaceHandoff()
	wave1Tasks := p.tasksForWave(1)
	req := p.requirement

	return func() tea.Msg {
		if client == nil {
			return plannerErrorMsg{err: fmt.Errorf("not connected to daemon")}
		}

		// Primary path: hand off to workspace agent.
		resp, err := client.Send(socket.Request{
			Command: "send_agent_input",
			Args: map[string]interface{}{
				"repo":    repoName,
				"agent":   "workspace",
				"message": handoffMsg,
			},
		})
		if err == nil && resp.Success {
			return plannerDispatchedMsg{target: "workspace"}
		}

		// Fallback: workspace not running — spawn Wave 1 workers directly.
		var errs []string
		for _, task := range wave1Tasks {
			wr, werr := client.Send(socket.Request{
				Command: "spawn_agent",
				Args: map[string]interface{}{
					"repo":   repoName,
					"name":   fmt.Sprintf("worker-%s", task.ID),
					"class":  "ephemeral",
					"prompt": buildWorkerPrompt(task, req),
					"task":   task.Title + ": " + task.Description,
				},
			})
			if werr != nil {
				errs = append(errs, task.ID+": "+werr.Error())
			} else if !wr.Success {
				errs = append(errs, task.ID+": "+wr.Error)
			}
		}
		if len(errs) > 0 {
			return plannerErrorMsg{err: fmt.Errorf("direct dispatch errors: %s", strings.Join(errs, "; "))}
		}
		return plannerDispatchedMsg{target: "direct"}
	}
}

// buildWorkspaceHandoff formats the approved plan as a structured message the
// workspace agent can parse to spawn and sequence workers.
func (p *PlannerView) buildWorkspaceHandoff() string {
	var sb strings.Builder
	sb.WriteString("[PLANNER-APPROVED] Plan ready for execution.\n\n")

	if p.requirement != nil && p.requirement.Refined != "" {
		sb.WriteString("## Requirement\n")
		sb.WriteString(p.requirement.Refined)
		sb.WriteString("\n\n")
	}

	waves := make(map[int][]Task)
	for _, t := range p.tasks {
		waves[t.Wave] = append(waves[t.Wave], t)
	}
	for wave := 1; wave <= p.getMaxWave(); wave++ {
		tasks := waves[wave]
		if len(tasks) == 0 {
			continue
		}
		if wave == 1 {
			sb.WriteString(fmt.Sprintf("## Wave %d — spawn immediately\n", wave))
		} else {
			sb.WriteString(fmt.Sprintf("## Wave %d — spawn after Wave %d completes\n", wave, wave-1))
		}
		for _, t := range tasks {
			sb.WriteString(fmt.Sprintf("### %s: %s\n", t.ID, t.Title))
			sb.WriteString(t.Description + "\n")
			if len(t.Dependencies) > 0 {
				sb.WriteString("Depends on: " + strings.Join(t.Dependencies, ", ") + "\n")
			}
			if len(t.AcceptanceCriteria) > 0 {
				sb.WriteString("Acceptance criteria:\n")
				for _, c := range t.AcceptanceCriteria {
					sb.WriteString("- " + c + "\n")
				}
			}
			sb.WriteString("\n")
		}
	}
	sb.WriteString("Spawn Wave 1 workers immediately. Advance to the next wave when all tasks in the current wave are complete.")
	return sb.String()
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

func (p *PlannerView) sendToPlanner(text string) tea.Cmd {
	client := p.client
	repoName := p.repoName
	// Prefix the user text with the current planning phase so the agent always
	// knows its context, even after a restart or a confused turn.
	message := p.buildPlannerMessage(text)
	return func() tea.Msg {
		if client == nil {
			return plannerErrorMsg{err: fmt.Errorf("not connected to daemon")}
		}
		resp, err := client.Send(socket.Request{
			Command: "send_agent_input",
			Args: map[string]interface{}{
				"repo":    repoName,
				"agent":   "planner",
				"message": message,
			},
		})
		if err != nil {
			return plannerErrorMsg{err: err}
		}
		if !resp.Success {
			return plannerErrorMsg{err: fmt.Errorf("%s", resp.Error)}
		}
		return plannerSentMsg{}
	}
}

// buildPlannerMessage prefixes the user text with a one-line phase hint so
// the planner agent always knows which conversation phase it is in, even if
// it was restarted mid-session. The hint is invisible in the TUI (user only
// sees their own text in the chat panel).
func (p *PlannerView) buildPlannerMessage(userText string) string {
	var phase string
	switch p.state {
	case StateDefiningRequirement:
		phase = "clarifying — gathering initial requirements"
	case StateRefiningRequirement, StateDecomposingTasks:
		phase = "clarifying — refining requirements"
	case StateReviewingPlan:
		phase = "draft_plan — plan under review"
	case StatePlanLocked:
		phase = "ready_for_review — plan locked awaiting approval"
	default:
		phase = "clarifying"
	}
	return fmt.Sprintf("[planner-tui phase=%s]\n%s", phase, userText)
}

func (p *PlannerView) refineRequirement() tea.Cmd {
	return p.sendToPlanner("Please refine the current requirement further.")
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
	wave1 := p.tasksForWave(1)
	if len(wave1) == 0 {
		p.feedback = append(p.feedback, FeedbackEntry{
			Type:      "system",
			Content:   "No Wave 1 tasks to execute.",
			Timestamp: time.Now(),
		})
		return nil
	}
	p.state = StateExecuting
	p.feedback = append(p.feedback, FeedbackEntry{
		Type:      "system",
		Content:   fmt.Sprintf("Dispatching %d Wave 1 worker(s)...", len(wave1)),
		Timestamp: time.Now(),
	})

	client := p.client
	repoName := p.repoName
	tasks := wave1
	req := p.requirement

	return func() tea.Msg {
		if client == nil {
			return plannerErrorMsg{err: fmt.Errorf("not connected to daemon")}
		}
		var errs []string
		for _, task := range tasks {
			workerPrompt := buildWorkerPrompt(task, req)
			resp, err := client.Send(socket.Request{
				Command: "spawn_agent",
				Args: map[string]interface{}{
					"repo":   repoName,
					"name":   fmt.Sprintf("worker-%s", task.ID),
					"class":  "ephemeral",
					"prompt": workerPrompt,
					"task":   task.Title + ": " + task.Description,
				},
			})
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", task.ID, err))
				continue
			}
			if !resp.Success {
				errs = append(errs, fmt.Sprintf("%s: %s", task.ID, resp.Error))
			}
		}
		if len(errs) > 0 {
			return plannerErrorMsg{err: fmt.Errorf("spawn errors: %s", strings.Join(errs, "; "))}
		}
		return plannerSentMsg{}
	}
}

// tasksForWave returns all tasks with the given wave number.
func (p *PlannerView) tasksForWave(wave int) []Task {
	var result []Task
	for _, t := range p.tasks {
		if t.Wave == wave {
			result = append(result, t)
		}
	}
	return result
}

// buildWorkerPrompt creates a concise system prompt for a spawned worker.
func buildWorkerPrompt(task Task, req *Requirement) string {
	var sb strings.Builder
	sb.WriteString("You are a worker agent. Complete exactly the task assigned to you, then stop.\n\n")
	if req != nil && req.Refined != "" {
		sb.WriteString("## Project context\n")
		sb.WriteString(req.Refined)
		sb.WriteString("\n\n")
	}
	sb.WriteString("## Your task\n")
	sb.WriteString("**" + task.Title + "**\n\n")
	sb.WriteString(task.Description + "\n\n")
	if len(task.AcceptanceCriteria) > 0 {
		sb.WriteString("## Acceptance criteria\n")
		for _, c := range task.AcceptanceCriteria {
			sb.WriteString("- " + c + "\n")
		}
	}
	sb.WriteString("\nWhen done, verify your work against the acceptance criteria and report completion.")
	return sb.String()
}

func (p *PlannerView) tickThinking() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(t time.Time) tea.Msg {
		return plannerTickMsg(t)
	})
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

// SummaryForList returns a short, one-line description of the planner's
// current state. The TUI agent sidebar renders this on the planner row.
func (p *PlannerView) SummaryForList() string {
	if len(p.tasks) > 0 {
		return fmt.Sprintf("%d tasks · %d waves", len(p.tasks), p.getMaxWave())
	}
	switch p.state {
	case StateDefiningRequirement:
		return "defining requirement"
	case StateRefiningRequirement:
		if p.requirement != nil {
			return fmt.Sprintf("refining (v%d)", p.requirement.Iteration)
		}
		return "refining requirement"
	case StateDecomposingTasks:
		return "decomposing tasks"
	case StateReviewingPlan:
		return "reviewing plan"
	case StatePlanLocked:
		return "plan locked"
	case StateExecuting:
		return "executing plan"
	default:
		return "idle"
	}
}

// Thinking reports whether the planner is mid-roundtrip with the overlord.
// Used by the TUI sidebar to drive the per-agent status indicator.
func (p *PlannerView) Thinking() bool {
	return p.thinking
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

	// Render test strategy (Overlord methodology)
	if p.testStrategy != nil {
		content.WriteString(p.renderTestStrategy())
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

	// Add operational spec if present
	if p.requirement.OperationalSpec != "" {
		content += fmt.Sprintf("\n\nOperational Spec: %s", 
			lipgloss.NewStyle().Foreground(lipgloss.Color("220")).Render(p.requirement.OperationalSpec))
	}

	return style.Render(content)
}

func (p *PlannerView) renderTestStrategy() string {
	if p.testStrategy == nil {
		return ""
	}

	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("34")).
		Padding(1).
		Margin(0, 0, 1, 0)

	title := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("34")).
		Render("🧪 Test Strategy (Overlord Methodology)")

	content := fmt.Sprintf("%s\n\n", title)
	content += fmt.Sprintf("Unit Tests: %s\n", p.testStrategy.Unit)
	content += fmt.Sprintf("Integration Tests: %s\n", p.testStrategy.Integration)
	content += fmt.Sprintf("Blackbox Tests: %s\n", p.testStrategy.Blackbox)
	content += fmt.Sprintf("Gate Script: %s", 
		lipgloss.NewStyle().Foreground(lipgloss.Color("220")).Render(p.testStrategy.GateScript))

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

	// Wave names for Overlord methodology
	waveNames := map[int]string{
		0: "Foundation (Tests, CI, Contracts)",
		1: "Core (Primary Implementation)",
		2: "Features (Secondary Implementation)",
		3: "Polish (Docs, Performance)",
	}

	for wave := 0; wave <= p.getMaxWave(); wave++ {
		if tasks, exists := waves[wave]; exists {
			waveName := waveNames[wave]
			if waveName == "" {
				waveName = "Extended"
			}
			waveTitle := lipgloss.NewStyle().
				Foreground(lipgloss.Color("220")).
				Render(fmt.Sprintf("Wave %d - %s:", wave, waveName))
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

				// Add type indicator
				typeIcon := ""
				switch task.Type {
				case "test":
					typeIcon = "🧪"
				case "implementation":
					typeIcon = "⚙️"
				case "documentation":
					typeIcon = "📝"
				}

				taskStyle := lipgloss.NewStyle()
				if i == p.selectedTask {
					taskStyle = taskStyle.Background(lipgloss.Color("237"))
				}

				taskLine := fmt.Sprintf("  %s %s %s", status, typeIcon, task.Title)
				if task.TestFirst {
					taskLine += " [TDD]"
				}
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
		helps = []string{"Enter: submit", "esc: back"}
	case StateRefiningRequirement, StateDecomposingTasks:
		helps = []string{"Enter: feedback", "^p: pull plan", "^r: refine", "esc: back"}
	case StateReviewingPlan:
		if len(p.tasks) > 0 {
			helps = []string{"^a: approve+dispatch", "^x: reject", "^p: repull plan", "Enter: feedback", "esc: back"}
		} else {
			helps = []string{"^p: pull plan as JSON", "^x: reject", "Enter: feedback", "esc: back"}
		}
	case StatePlanLocked:
		helps = []string{"^a: dispatch", "Enter: feedback", "esc: back"}
	case StateExecuting:
		helps = []string{"esc: back"}
	}

	helps = append(helps, "^n: new")

	helpText := strings.Join(helps, " • ")
	
	return lipgloss.NewStyle().
		Width(p.width).
		Foreground(lipgloss.Color("8")).
		Padding(0, 1).
		Render(helpText)
}

// ReceiveOutput forwards lines from the planner agent's output stream.
// Text lines (lineType == "" or "text") are accumulated into plannerBuffer.
// When a complete ```json ... ``` fence is detected, the JSON is parsed to
// update requirement/tasks/state and the message field is shown in chat.
// Any freeform text that precedes a JSON block is also shown in chat.
func (p *PlannerView) ReceiveOutput(lines []string, lineTypes []string) {
	p.thinking = false
	for i, line := range lines {
		lt := ""
		if i < len(lineTypes) {
			lt = lineTypes[i]
		}
		if lt != "" && lt != "text" {
			continue
		}
		if strings.ContainsAny(line, "\x1b\x00\x01\x02\x03") {
			continue
		}
		p.plannerBuffer += line + "\n"
	}
	p.drainBuffer()
}

// drainBuffer scans plannerBuffer for complete ```json...``` fences.
//
// Freeform text (no fence) is shown in chat immediately — it is never held
// silently. Text before a fence is shown, the JSON drives state updates and
// only its message field appears in chat, and any remainder after the fence
// stays buffered for the next batch.
//
// An incomplete fence (opening found, closing not yet arrived) is held in
// the buffer unchanged so the next batch can complete it.
func (p *PlannerView) drainBuffer() {
	const fenceOpen = "```json"
	const fenceClose = "```"

	for {
		fenceStart := strings.Index(p.plannerBuffer, fenceOpen)
		if fenceStart < 0 {
			// No JSON fence at all — show everything as plain chat and clear.
			if trimmed := strings.TrimSpace(p.plannerBuffer); trimmed != "" {
				p.addAIChat(trimmed)
			}
			p.plannerBuffer = ""
			return
		}

		// Freeform text before the opening fence → show in chat.
		if preamble := strings.TrimSpace(p.plannerBuffer[:fenceStart]); preamble != "" {
			p.addAIChat(preamble)
		}

		// Advance past "```json" and optional newline.
		jsonStart := fenceStart + len(fenceOpen)
		if jsonStart < len(p.plannerBuffer) && p.plannerBuffer[jsonStart] == '\n' {
			jsonStart++
		}

		// Find the closing "```".
		closeIdx := strings.Index(p.plannerBuffer[jsonStart:], fenceClose)
		if closeIdx < 0 {
			// Incomplete fence — keep from the opening marker, wait for more.
			p.plannerBuffer = p.plannerBuffer[fenceStart:]
			return
		}

		jsonStr := p.plannerBuffer[jsonStart : jsonStart+closeIdx]
		p.plannerBuffer = p.plannerBuffer[jsonStart+closeIdx+len(fenceClose):]

		var resp PlannerResponse
		if err := json.Unmarshal([]byte(jsonStr), &resp); err == nil {
			p.applyPlannerResponse(resp)
		} else {
			// Malformed JSON — show raw so the problem is visible.
			p.addAIChat(jsonStr)
		}
		// Loop: process any further fences in the remaining buffer.
	}
}

func (p *PlannerView) addAIChat(text string) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return
	}
	p.feedback = append(p.feedback, FeedbackEntry{
		Type:      "ai",
		Content:   trimmed,
		Timestamp: time.Now(),
	})
}

// applyPlannerResponse updates TUI state from a parsed planner JSON response.
func (p *PlannerView) applyPlannerResponse(resp PlannerResponse) {
	// Transition state machine
	switch resp.Phase {
	case "clarifying":
		if p.state == StateDefiningRequirement || p.state == StateRefiningRequirement {
			p.state = StateRefiningRequirement
		}
	case "architecture":
		// New phase for Overlord methodology
		p.state = StateRefiningRequirement
	case "draft_plan":
		p.state = StateReviewingPlan
	case "ready_for_review":
		p.state = StateReviewingPlan
	}

	// Update requirement
	if resp.Requirement != nil {
		if p.requirement == nil {
			p.requirement = &Requirement{
				ID:          fmt.Sprintf("req-%d", time.Now().Unix()),
				LastUpdated: time.Now(),
			}
		}
		p.requirement.Original = resp.Requirement.Original
		p.requirement.Refined = resp.Requirement.Refined
		p.requirement.OperationalSpec = resp.Requirement.OperationalSpec
		p.requirement.Iteration++
		p.requirement.LastUpdated = time.Now()
	}

	// Update test strategy (Overlord methodology)
	if resp.TestStrategy != nil {
		p.testStrategy = resp.TestStrategy
	}

	// Update tasks
	if len(resp.Tasks) > 0 {
		p.tasks = make([]Task, len(resp.Tasks))
		for i, t := range resp.Tasks {
			p.tasks[i] = Task{
				ID:                 t.ID,
				Title:              t.Title,
				Description:        t.Description,
				Type:               t.Type,
				Wave:               t.Wave,
				Dependencies:       t.Dependencies,
				SpecReference:      t.SpecReference,
				TestFirst:          t.TestFirst,
				AcceptanceCriteria: t.AcceptanceCriteria,
				Status:             TaskStatusPending,
			}
		}
	}

	// Show the human-readable message in chat
	if resp.Message != "" {
		p.addAIChat(resp.Message)
	} else if len(resp.Questions) > 0 {
		// Fallback: render questions inline if message is empty
		var sb strings.Builder
		for i, q := range resp.Questions {
			sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, q))
		}
		p.addAIChat(strings.TrimSpace(sb.String()))
	}

	// Handle the action field — the planner signals system-level intent.
	switch resp.Action {
	case "dispatch_tasks":
		// Planner says the plan is ready — auto-trigger approval if tasks parsed.
		if len(p.tasks) > 0 && p.state != StatePlanLocked && p.state != StateExecuting {
			p.feedback = append(p.feedback, FeedbackEntry{
				Type:      "system",
				Content:   "Planner signalled dispatch_tasks. Press ^a to approve and send to workspace.",
				Timestamp: time.Now(),
			})
		}
	case "advance_phase":
		// Already handled by phase transition above.
	case "revise":
		if p.state == StateReviewingPlan || p.state == StatePlanLocked {
			p.state = StateRefiningRequirement
			p.isLocked = false
		}
	}
}

// Message types for bubbletea
type plannerSentMsg struct{}

type plannerErrorMsg struct{ err error }

type plannerDispatchedMsg struct{ target string } // "workspace" or "direct"

type plannerTickMsg time.Time

