package views

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Root-IO-Labs/open-agent-teams/internal/planner"
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
	Type               string // test|implementation|documentation (Overlord)
	Dependencies       []string
	AcceptanceCriteria []string
	Wave               int    // Execution wave (parallel tasks have same wave, Wave 0 = foundation)
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

// ConversationEntry is a single message in the clean planner conversation.
// Unlike the raw PTY viewport, this shows only extracted user messages and
// planner response text — no JSON, no tool calls, no terminal noise.
type ConversationEntry struct {
	Role string // "user" | "planner" | "system"
	Text string
}

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
	Unit        string `json:"unit"`
	Integration string `json:"integration"`
	Blackbox    string `json:"blackbox"`
	GateScript  string `json:"gate_script"`
}

// PlannerTask is a single task inside a PlannerResponse.
type PlannerTask struct {
	ID                 string   `json:"id"`
	Title              string   `json:"title"`
	Description        string   `json:"description"`
	Type               string   `json:"type"` // test|implementation|documentation
	Wave               int      `json:"wave"`
	Dependencies       []string `json:"dependencies"`
	SpecReference      string   `json:"spec_reference"` // Reference to operational spec section
	TestFirst          bool     `json:"test_first"`     // TDD flag for implementation tasks
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
	width        int
	height       int
	scrollY      int
	selectedTask int

	// Communication
	client   *socket.Client
	repoName string

	// Feedback and collaboration
	feedback        []FeedbackEntry
	thinkingText    string
	plannerBuffer   string // accumulates planner output for JSON detection
	clarifyingTurns int    // turns in clarifying phase without advancing

	// Clean conversation: extracted message fields + user messages, shown
	// instead of raw PTY output in the planner viewport.
	conversation []ConversationEntry

	// Execution tracking: maps task ID → worker name and PR number
	// populated after dispatch, updated from daemon completion events.
	taskWorkers map[string]string // task.ID → worker agent name
	taskPRs     map[string]int    // task.ID → PR number (0 = no PR yet)
	wavesDone   map[int]bool      // wave → all tasks complete

	// persistCallCount counts persistPlan invocations. Test seam: lets the
	// idempotency tests assert that no-op TUI ticks don't re-write the plan
	// file. (TUI polls workers every 2s; without dedup the plan file would
	// rewrite ~4×/sec per active repo and accumulate GB of version history.)
	persistCallCount int

	// Enhanced contextual awareness (Overlord integration)
	context          *PlannerContext
	currentGate      *PhaseGate
	pendingQuestions []string
	brainstormThemes []BrainstormTheme

	// Key bindings
	keyMap PlannerKeyMap
}

type FeedbackEntry struct {
	Type      string // "user", "ai", "system"
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
		state:        StateDefiningRequirement,
		input:        ti,
		inputPrompt:  "Requirement",
		client:       client,
		repoName:     repoName,
		selectedTask: -1,
		keyMap:       defaultPlannerKeys(),
		feedback: []FeedbackEntry{
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
		p.applyWorkerAssignments(msg.assignments)
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
	// Ctrl+B — trigger the next brainstorm theme (Socratic dialogue). Useful
	// when the conversation has stalled and needs a new angle.
	if msg.Type == tea.KeyCtrlB && len(p.brainstormThemes) > 0 {
		return p, p.conductSocraticDialogue()
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
	p.currentGate = nil // gate passed or bypassed via ^a

	// Persist the plan so it survives TUI restarts.
	p.persistPlan()

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
		"[planner-tui phase=ready_for_review]\n" +
			"STOP all implementation work immediately. You are the PLANNER, not the implementer. " +
			"Output your complete current plan as a single JSON code block (```json ... ```) " +
			"in the required format with phase, message, requirement, and tasks fields. " +
			"Include wave numbers, dependencies, and acceptance_criteria for every task. " +
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
// can spawn workers in the correct waves. The planner must never spawn
// workers directly; workspace owns execution and wave advancement.
func (p *PlannerView) dispatchToWorkspace() tea.Cmd {
	client := p.client
	repoName := p.repoName
	handoffMsg := p.buildWorkspaceHandoff()

	// Initialize execution tracking maps.
	p.taskWorkers = make(map[string]string)
	p.taskPRs = make(map[string]int)
	p.wavesDone = make(map[int]bool)
	// Mark Wave 1 tasks as InProgress when dispatched.
	for i, t := range p.tasks {
		if t.Wave == 1 {
			p.tasks[i].Status = TaskStatusInProgress
		}
	}

	return func() tea.Msg {
		if client == nil {
			return plannerErrorMsg{err: fmt.Errorf("not connected to daemon")}
		}

		var lastErr string
		for _, target := range workspaceDispatchTargets() {
			resp, err := client.Send(socket.Request{
				Command: "send_agent_input",
				Args: map[string]interface{}{
					"repo":    repoName,
					"agent":   target,
					"message": handoffMsg,
				},
			})
			if err == nil && resp.Success {
				return plannerDispatchedMsg{target: target}
			}
			if err != nil {
				lastErr = err.Error()
			} else if !resp.Success {
				lastErr = resp.Error
			}
		}
		if lastErr == "" {
			lastErr = "workspace agent was not reachable"
		}
		return plannerErrorMsg{err: fmt.Errorf("could not hand off plan to workspace/default; planner did not spawn workers directly: %s", lastErr)}
	}
}

func workspaceDispatchTargets() []string {
	return []string{"default", "workspace"}
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
	sb.WriteString("## Execution Contract\n")
	sb.WriteString("- Workspace owns worker creation, wave advancement, and PR/issue coordination. The planner must not spawn workers.\n")
	sb.WriteString("- Preserve each task ID exactly as written below.\n")
	sb.WriteString("- When spawning a worker, include the marker `[planner-task:<task-id>]` at the start of the worker task text so planner progress can be mapped back deterministically.\n")
	sb.WriteString("- Do not start a wave until every dependency and every task in the previous wave is complete.\n\n")
	sb.WriteString("## Workspace State\n")
	sb.WriteString("Before spawning any workers, persist this execution state to yourself using your actual workspace agent name, usually `default`:\n")
	sb.WriteString("```bash\n")
	sb.WriteString(fmt.Sprintf("oat message send \"$OAT_AGENT_NAME\" %q\n", p.waveStateMessage()))
	sb.WriteString("```\n\n")

	waves := make(map[int][]Task)
	for _, t := range p.tasks {
		waves[t.Wave] = append(waves[t.Wave], t)
	}
	for _, wave := range sortedWaveKeys(waves) {
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
			sb.WriteString("Task marker: " + plannerTaskMarker(t.ID) + "\n")
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

func (p *PlannerView) waveStateMessage() string {
	requirement := ""
	if p.requirement != nil {
		requirement = p.requirement.Refined
	}
	var taskIDs []string
	for _, task := range p.tasks {
		taskIDs = append(taskIDs, fmt.Sprintf("%s:wave%d", task.ID, task.Wave))
	}
	return fmt.Sprintf("WAVE_STATE: current_wave=1 total_waves=%d tasks=%s requirement=%q", p.getMaxWave(), strings.Join(taskIDs, ","), requirement)
}

func plannerTaskMarker(taskID string) string {
	return "[planner-task:" + taskID + "]"
}

func sortedWaveKeys(waves map[int][]Task) []int {
	keys := make([]int, 0, len(waves))
	for wave := range waves {
		keys = append(keys, wave)
	}
	sort.Ints(keys)
	return keys
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
	// Send raw text — no prefix. The prefix was causing PTY echo to leak
	// "[planner-tui phase=...]user text" into the output viewport.
	message := text
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
	p.conversation = append(p.conversation, ConversationEntry{
		Role: "system",
		Text: "Asking planner to refine the current requirement.",
	})
	return p.sendToPlanner("Please refine the current requirement further.")
}

func (p *PlannerView) rejectPlan() tea.Cmd {
	p.state = StateRefiningRequirement
	p.isLocked = false
	p.currentGate = nil
	// Notify the planner agent so it knows the plan was rejected and can revise.
	return p.sendToPlanner("The plan has been rejected. Please revise it — " +
		"what changes would you like to make?")
}

// persistPlan saves the current approved plan to ~/.oat/plans/<repo>/ so it
// survives TUI restarts and can be referenced later.
func (p *PlannerView) persistPlan() {
	if p.requirement == nil || len(p.tasks) == 0 {
		return
	}
	p.persistCallCount++
	plansDir := filepath.Join(os.Getenv("HOME"), ".oat", "plans", p.repoName)
	storage, err := planner.NewPlanStorage(plansDir)
	if err != nil {
		return // non-fatal — persistence is best-effort
	}

	doc := &planner.PlanDocument{
		ID:        p.requirement.ID,
		Version:   p.requirement.Iteration,
		CreatedAt: p.requirement.LastUpdated,
		UpdatedAt: time.Now(),
		Status:    plannerStatusForState(p.state),
		Requirement: planner.RequirementDoc{
			Title:           p.requirement.Refined,
			Original:        p.requirement.Original,
			Refined:         p.requirement.Refined,
			OperationalSpec: p.requirement.OperationalSpec,
			LastUpdated:     p.requirement.LastUpdated,
		},
	}
	if p.testStrategy != nil {
		doc.TestStrategy = planner.TestStrategyDoc{
			Unit:        p.testStrategy.Unit,
			Integration: p.testStrategy.Integration,
			Blackbox:    p.testStrategy.Blackbox,
			GateScript:  p.testStrategy.GateScript,
		}
	}
	for _, t := range p.tasks {
		prNumber := 0
		if p.taskPRs != nil {
			prNumber = p.taskPRs[t.ID]
		}
		doc.Tasks = append(doc.Tasks, planner.TaskDoc{
			ID:                 t.ID,
			Title:              t.Title,
			Description:        t.Description,
			Type:               t.Type,
			Wave:               t.Wave,
			Dependencies:       t.Dependencies,
			SpecReference:      t.SpecReference,
			TestFirst:          t.TestFirst,
			AcceptanceCriteria: t.AcceptanceCriteria,
			Status:             taskStatusString(t.Status),
			AssignedTo:         t.AssignedTo,
			PRNumber:           prNumber,
		})
	}
	_ = storage.SavePlan(doc) // best-effort; failure is non-fatal
}

func plannerStatusForState(state PlannerState) string {
	switch state {
	case StateExecuting:
		return "executing"
	case StatePlanLocked:
		return "approved"
	default:
		return "draft"
	}
}

func taskStatusString(status TaskStatus) string {
	switch status {
	case TaskStatusInProgress:
		return "in_progress"
	case TaskStatusCompleted:
		return "completed"
	case TaskStatusFailed:
		return "failed"
	case TaskStatusBlocked:
		return "blocked"
	default:
		return "pending"
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

// buildWorkerPrompt creates the system prompt for a spawned worker, injecting
// all available context from the planner: requirement, operational spec,
// test strategy, and task-level spec references.
func buildWorkerPrompt(task Task, req *Requirement) string {
	var sb strings.Builder
	sb.WriteString("You are a worker agent. Complete exactly the task assigned to you, then stop.\n\n")

	if req != nil {
		if req.Refined != "" {
			sb.WriteString("## Project requirement\n")
			sb.WriteString(req.Refined)
			sb.WriteString("\n\n")
		}
		if req.OperationalSpec != "" {
			sb.WriteString("## How the system works (operational spec)\n")
			sb.WriteString(req.OperationalSpec)
			sb.WriteString("\n\n")
		}
	}

	if task.TestFirst {
		sb.WriteString("## Approach: Test-First (TDD)\n")
		sb.WriteString("Write the tests BEFORE writing implementation code. " +
			"Ensure tests fail first, then implement until they pass.\n\n")
	}

	if task.SpecReference != "" {
		sb.WriteString("## Specification reference\n")
		sb.WriteString(task.SpecReference)
		sb.WriteString("\n\n")
	}

	sb.WriteString("## Your task\n")
	sb.WriteString("**" + task.Title + "**\n\n")
	sb.WriteString(task.Description + "\n\n")

	if len(task.Dependencies) > 0 {
		sb.WriteString("## Dependencies (must be complete before this task)\n")
		for _, d := range task.Dependencies {
			sb.WriteString("- " + d + "\n")
		}
		sb.WriteString("\n")
	}

	if len(task.AcceptanceCriteria) > 0 {
		sb.WriteString("## Acceptance criteria (your definition of done)\n")
		for _, c := range task.AcceptanceCriteria {
			sb.WriteString("- " + c + "\n")
		}
	}

	sb.WriteString("\nVerify your work against every acceptance criterion before submitting. " +
		"Run `./scripts/check.sh` if it exists.")
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
	thinking := ""
	if p.thinking {
		thinking = " ●"
	}

	if len(p.tasks) > 0 {
		switch p.state {
		case StatePlanLocked:
			return fmt.Sprintf("plan locked · %d tasks", len(p.tasks))
		case StateExecuting:
			return fmt.Sprintf("executing · %d tasks · %d waves", len(p.tasks), p.getMaxWave())
		default:
			return fmt.Sprintf("%d tasks · %d waves%s", len(p.tasks), p.getMaxWave(), thinking)
		}
	}

	switch p.state {
	case StateDefiningRequirement:
		return "waiting for requirement" + thinking
	case StateRefiningRequirement:
		if p.requirement != nil {
			return fmt.Sprintf("clarifying (v%d)%s", p.requirement.Iteration, thinking)
		}
		return "clarifying" + thinking
	case StateDecomposingTasks:
		return "decomposing tasks" + thinking
	case StateReviewingPlan:
		return "plan ready for review"
	case StatePlanLocked:
		return "plan locked"
	case StateExecuting:
		return "executing"
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
	contentHeight := p.height - 6
	if contentHeight < 5 {
		contentHeight = 5
	}

	var content strings.Builder

	// Compact requirement strip — only when a refined requirement exists.
	// No heavy border box; just a single dim line so it stays out of the way.
	if p.requirement != nil && p.requirement.Refined != "" {
		label := lipgloss.NewStyle().Foreground(lipgloss.Color("62")).Bold(true).Render("Req: ")
		text := lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Render(p.requirement.Refined)
		// Truncate if too wide
		maxW := p.width - 10
		if maxW > 0 && lipgloss.Width(p.requirement.Refined) > maxW {
			runes := []rune(p.requirement.Refined)
			if maxW > 3 {
				text = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Render(string(runes[:maxW-3]) + "…")
			}
		}
		content.WriteString(label + text + "\n")
		if len(p.tasks) > 0 {
			taskInfo := lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render(
				fmt.Sprintf("  %d tasks · %d waves", len(p.tasks), p.getMaxWave()))
			content.WriteString(taskInfo + "\n")
		}
		content.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("237")).Render(strings.Repeat("─", p.width-4)))
		content.WriteString("\n\n")
	}

	// Tasks compact view — only show if in review/locked/executing state
	if len(p.tasks) > 0 && (p.state == StateReviewingPlan || p.state == StatePlanLocked || p.state == StateExecuting) {
		content.WriteString(p.renderTasksCompact())
		content.WriteString("\n")
	}

	// Main conversation — takes all remaining space
	content.WriteString(p.renderFeedback())

	return lipgloss.NewStyle().
		Width(p.width-2).
		Height(contentHeight).
		Padding(0, 1).
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

	wrapWidth := p.width - 6
	if wrapWidth < 20 {
		wrapWidth = 0
	}

	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	userStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Bold(true)
	aiStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	systemStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Italic(true)

	for _, entry := range p.feedback {
		switch entry.Type {
		case "user":
			// User messages: "> text" in green, like a prompt line
			line := userStyle.Render("> " + entry.Content)
			if wrapWidth > 0 {
				line = lipgloss.NewStyle().Width(wrapWidth).Render(line)
			}
			content.WriteString(line)
			content.WriteString("\n\n")

		case "ai":
			// AI messages: markdown-ish text, no prefix, indented slightly
			body := renderPlannerMarkdown(entry.Content, wrapWidth-2, aiStyle)
			if wrapWidth > 0 {
				body = lipgloss.NewStyle().Width(wrapWidth-2).Padding(0, 0, 0, 2).Render(body)
			}
			content.WriteString(body)
			content.WriteString("\n\n")

		case "system":
			// System messages: dim italic, used only for important gate/state changes
			body := systemStyle.Render("⋯ " + entry.Content)
			if wrapWidth > 0 {
				body = lipgloss.NewStyle().Width(wrapWidth).Render(body)
			}
			content.WriteString(dimStyle.Render(body))
			content.WriteString("\n")
		}
	}

	if p.thinking {
		content.WriteString(p.renderThinking())
		content.WriteString("\n")
	}

	return content.String()
}

func renderPlannerMarkdown(text string, width int, base lipgloss.Style) string {
	if width < 20 {
		width = 80
	}

	headingStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("62")).Bold(true)
	boldStyle := lipgloss.NewStyle().Bold(true)
	codeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	listStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	quoteStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Italic(true)

	var out strings.Builder
	lines := strings.Split(text, "\n")
	inFence := false
	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			out.WriteString("\n")
			continue
		}

		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			out.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("220")).Width(width).Render(line))
			out.WriteString("\n")
			continue
		}

		if headingText, ok := markdownHeadingText(trimmed); ok {
			out.WriteString(headingStyle.Width(width).Render(headingText))
			out.WriteString("\n")
			continue
		}

		if strings.HasPrefix(trimmed, ">") {
			out.WriteString(quoteStyle.Width(width).Render(strings.TrimSpace(strings.TrimPrefix(trimmed, ">"))))
			out.WriteString("\n")
			continue
		}

		prefix, body := markdownListParts(line)
		renderedBody := renderInlineMarkdown(body, base, boldStyle, codeStyle)
		if prefix != "" {
			out.WriteString(listStyle.Render(prefix))
			out.WriteString(lipgloss.NewStyle().Width(width - lipgloss.Width(prefix)).Render(renderedBody))
			out.WriteString("\n")
			continue
		}

		out.WriteString(lipgloss.NewStyle().Width(width).Render(renderInlineMarkdown(line, base, boldStyle, codeStyle)))
		out.WriteString("\n")
	}

	return strings.TrimRight(out.String(), "\n")
}

func markdownHeadingText(line string) (string, bool) {
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || level >= len(line) || line[level] != ' ' {
		return "", false
	}
	return strings.TrimSpace(line[level:]), true
}

func markdownListParts(line string) (string, string) {
	indentLen := len(line) - len(strings.TrimLeft(line, " "))
	trimmed := strings.TrimLeft(line, " ")
	indent := strings.Repeat(" ", indentLen)
	for _, marker := range []string{"- ", "* ", "+ "} {
		if strings.HasPrefix(trimmed, marker) {
			return indent + "• ", strings.TrimSpace(trimmed[len(marker):])
		}
	}
	if dot := strings.Index(trimmed, ". "); dot > 0 && allDigits(trimmed[:dot]) {
		return indent + trimmed[:dot+2], strings.TrimSpace(trimmed[dot+2:])
	}
	return "", line
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func renderInlineMarkdown(text string, base, boldStyle, codeStyle lipgloss.Style) string {
	var out strings.Builder
	for len(text) > 0 {
		nextBold := strings.Index(text, "**")
		nextCode := strings.Index(text, "`")
		switch {
		case nextCode >= 0 && (nextBold < 0 || nextCode < nextBold):
			if nextCode > 0 {
				out.WriteString(base.Render(text[:nextCode]))
			}
			rest := text[nextCode+1:]
			end := strings.Index(rest, "`")
			if end < 0 {
				out.WriteString(base.Render("`" + rest))
				return out.String()
			}
			out.WriteString(codeStyle.Render(rest[:end]))
			text = rest[end+1:]
		case nextBold >= 0:
			if nextBold > 0 {
				out.WriteString(base.Render(text[:nextBold]))
			}
			rest := text[nextBold+2:]
			end := strings.Index(rest, "**")
			if end < 0 {
				out.WriteString(base.Render("**" + rest))
				return out.String()
			}
			out.WriteString(boldStyle.Render(rest[:end]))
			text = rest[end+2:]
		default:
			out.WriteString(base.Render(text))
			return out.String()
		}
	}
	return out.String()
}

// renderTasksCompact shows tasks as a dense single-line list by wave,
// used when plan is in review/locked/executing state.
func (p *PlannerView) renderTasksCompact() string {
	if len(p.tasks) == 0 {
		return ""
	}
	waves := make(map[int][]Task)
	for _, t := range p.tasks {
		waves[t.Wave] = append(waves[t.Wave], t)
	}
	var sb strings.Builder
	for wave := 0; wave <= p.getMaxWave(); wave++ {
		tasks, ok := waves[wave]
		if !ok {
			continue
		}
		waveLabel := lipgloss.NewStyle().Foreground(lipgloss.Color("220")).Render(fmt.Sprintf("W%d", wave))
		var titles []string
		for _, t := range tasks {
			icon := "○"
			switch t.Status {
			case TaskStatusInProgress:
				icon = "●"
			case TaskStatusCompleted:
				icon = "✓"
			case TaskStatusFailed:
				icon = "✗"
			}
			titles = append(titles, icon+" "+t.Title)
		}
		sb.WriteString(waveLabel + " " + lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render(strings.Join(titles, "  ")) + "\n")
	}
	return sb.String()
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
		helps = []string{"Enter: describe requirement", "esc: back"}
	case StateRefiningRequirement:
		helps = []string{"Enter: reply", "^p: extract plan", "^r: refine", "^n: restart", "esc: back"}
		if len(p.brainstormThemes) > 0 {
			helps = append(helps, "^b: brainstorm")
		}
	case StateDecomposingTasks:
		helps = []string{"Enter: reply", "^p: extract plan", "^r: refine", "^n: restart", "esc: back"}
		if len(p.brainstormThemes) > 0 {
			helps = append(helps, "^b: brainstorm")
		}
	case StateReviewingPlan:
		if len(p.tasks) > 0 {
			helps = []string{"^a: approve & dispatch", "^x: reject", "^p: re-extract", "^r: refine", "Enter: feedback", "^n: restart", "esc: back"}
		} else {
			helps = []string{"^p: extract plan as JSON", "^x: reject", "^r: refine", "Enter: feedback", "^n: restart", "esc: back"}
		}
		if len(p.brainstormThemes) > 0 {
			helps = append(helps, "^b: brainstorm")
		}
	case StatePlanLocked:
		helps = []string{"^a: dispatch to workspace", "Enter: feedback", "esc: back"}
	case StateExecuting:
		helps = []string{"esc: back"}
	}

	helpText := strings.Join(helps, " • ")

	// Show the first contextual suggestion as a dim tip below the key bindings.
	suggestions := p.getContextualSuggestions()
	if len(suggestions) > 0 && p.state != StateExecuting && p.state != StatePlanLocked {
		tip := "Tip: " + suggestions[0]
		return lipgloss.JoinVertical(lipgloss.Left,
			lipgloss.NewStyle().Width(p.width).Foreground(lipgloss.Color("8")).Padding(0, 1).Render(helpText),
			lipgloss.NewStyle().Width(p.width).Foreground(lipgloss.Color("240")).Padding(0, 1).Render(tip),
		)
	}

	return lipgloss.NewStyle().
		Width(p.width).
		Foreground(lipgloss.Color("8")).
		Padding(0, 1).
		Render(helpText)
}

// ReceiveOutput is called when new lines arrive from the planner agent's
// output stream. It accumulates text into plannerBuffer and tries to parse
// JSON planner responses to update planning state (requirement, tasks, phase).
// It does NOT render anything — rendering is handled by the standard viewport
// via renderContentForViewport("planner") in app.go.
func (p *PlannerView) ReceiveOutput(lines []string, lineTypes []string) {
	// Only stop the thinking spinner when we receive a closing fence or a
	// non-empty plain-text line. Stopping on every batch causes the spinner
	// to flicker off while the planner is still streaming.
	for i, line := range lines {
		lt := ""
		if i < len(lineTypes) {
			lt = lineTypes[i]
		}
		// Accept text and unknown-type lines; skip tool calls, tool output, etc.
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

// drainBuffer extracts planner JSON responses from the accumulated buffer.
//
// The planner may output JSON with or without ```json fences — PTY rendering
// sometimes strips fence markers. We handle both:
//
//  1. Fenced:    ```json\n{...}\n```  — highest priority, always correct
//  2. Bare JSON: a complete {…} object that parses as PlannerResponse
//  3. Hold:      buffer looks like mid-stream JSON (has " but no complete
//     object yet) — hold up to 8KB before flushing as plain text
//  4. Plain text: no JSON indicators — show immediately
func (p *PlannerView) drainBuffer() {
	const fenceOpen = "```json"
	const fenceClose = "```"
	const maxHold = 8192

	for {
		// ── Pass 1: look for a ```json … ``` fence ────────────────────────
		if fenceStart := strings.Index(p.plannerBuffer, fenceOpen); fenceStart >= 0 {
			jsonStart := fenceStart + len(fenceOpen)
			if jsonStart < len(p.plannerBuffer) && p.plannerBuffer[jsonStart] == '\n' {
				jsonStart++
			}
			closeIdx := strings.Index(p.plannerBuffer[jsonStart:], fenceClose)
			if closeIdx < 0 {
				p.plannerBuffer = p.plannerBuffer[fenceStart:]
				return // incomplete fence — wait
			}
			jsonStr := p.plannerBuffer[jsonStart : jsonStart+closeIdx]
			p.plannerBuffer = p.plannerBuffer[jsonStart+closeIdx+len(fenceClose):]
			var resp PlannerResponse
			if err := json.Unmarshal([]byte(jsonStr), &resp); err == nil {
				p.thinking = false // complete response — stop spinner
				p.applyPlannerResponse(resp)
			}
			continue
		}

		// ── Pass 2: try to extract a bare JSON object {…} ─────────────────
		if braceAt := strings.Index(p.plannerBuffer, "{"); braceAt >= 0 &&
			strings.Contains(p.plannerBuffer[braceAt:], "}") {
			obj, end := extractJSONObject(p.plannerBuffer[braceAt:])
			if obj != "" {
				var resp PlannerResponse
				if err := json.Unmarshal([]byte(obj), &resp); err == nil && resp.Phase != "" {
					p.thinking = false // complete response — stop spinner
					p.applyPlannerResponse(resp)
				}
				p.plannerBuffer = p.plannerBuffer[braceAt+end:]
				continue
			}
		}

		// ── Pass 3: hold or flush to conversation ────────────────────────
		// If the buffer looks like mid-stream JSON, hold up to maxHold.
		// Otherwise treat it as plain-text prose from the planner and add
		// it to the conversation — the planner may respond in prose, markdown,
		// or other formats rather than JSON.
		looksLikeJSON := strings.ContainsAny(p.plannerBuffer, `{"`) &&
			(strings.Contains(p.plannerBuffer, "```json") || strings.Contains(p.plannerBuffer, `"phase"`))
		if looksLikeJSON && len(p.plannerBuffer) < maxHold {
			return // wait for the fence / complete object
		}
		// Prose or oversized buffer — add to conversation.
		if trimmed := strings.TrimSpace(p.plannerBuffer); trimmed != "" {
			p.thinking = false
			p.conversation = append(p.conversation, ConversationEntry{Role: "planner", Text: trimmed})
		}
		p.plannerBuffer = ""
		return
	}
}

// extractJSONObject finds the first complete JSON object {…} in s using
// brace counting. Returns the object string and the index just past its
// closing brace. Returns ("", 0) if no complete object is found.
func extractJSONObject(s string) (string, int) {
	depth := 0
	inStr := false
	escape := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escape {
			escape = false
			continue
		}
		if c == '\\' && inStr {
			escape = true
			continue
		}
		if c == '"' {
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}
		if c == '{' {
			depth++
		} else if c == '}' {
			depth--
			if depth == 0 {
				return s[:i+1], i + 1
			}
		}
	}
	return "", 0
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
	// Transition state machine based on planner phase.
	// "architecture" is the active-decomposition phase — tasks are being built
	// but not yet ready for review, so show StateDecomposingTasks.
	switch resp.Phase {
	case "clarifying":
		if p.state == StateDefiningRequirement || p.state == StateRefiningRequirement {
			p.state = StateRefiningRequirement
		}
	case "architecture":
		p.state = StateDecomposingTasks
	case "draft_plan":
		// Tasks exist but user hasn't approved — show decomposing while tasks
		// are partially formed, reviewing once we have a complete set.
		if len(resp.Tasks) > 0 {
			p.state = StateReviewingPlan
		} else {
			p.state = StateDecomposingTasks
		}
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

	// Add the planner's message to the clean conversation log.
	// If the message ends without the questions listed, append them inline
	// so the user always sees what was asked.
	if resp.Message != "" {
		text := resp.Message
		if len(resp.Questions) > 0 {
			// Append numbered questions if the message doesn't already contain them.
			hasQ := strings.Contains(strings.ToLower(text), "1.") || strings.Contains(text, "1)")
			if !hasQ {
				text += "\n"
				for i, q := range resp.Questions {
					text += fmt.Sprintf("\n%d. %s", i+1, q)
				}
			}
		}
		p.conversation = append(p.conversation, ConversationEntry{Role: "planner", Text: text})
	} else if len(resp.Questions) > 0 {
		// No message but questions exist — render them directly.
		var sb strings.Builder
		for i, q := range resp.Questions {
			sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, q))
		}
		p.conversation = append(p.conversation, ConversationEntry{Role: "planner", Text: strings.TrimSpace(sb.String())})
	}

	// Set plan-approval gate when a complete plan lands in StateReviewingPlan.
	if p.state == StateReviewingPlan && len(p.tasks) > 0 && p.currentGate == nil {
		gates := p.initPhaseGates()
		if len(gates) >= 3 {
			gate := gates[2]
			gate.ApprovalPrompt = fmt.Sprintf(
				"Plan ready: %d tasks in %d waves. Press ^a to dispatch.",
				len(p.tasks), p.getMaxWave(),
			)
			p.currentGate = &gate
		}
	}

	switch resp.Action {
	case "revise":
		if p.state == StateReviewingPlan || p.state == StatePlanLocked {
			p.state = StateRefiningRequirement
			p.isLocked = false
			p.currentGate = nil
		}
	}
}

// Message types for bubbletea
type plannerSentMsg struct{}

type plannerErrorMsg struct{ err error }

type plannerDispatchedMsg struct {
	target      string // "workspace" or "direct"
	assignments map[string]string
}

type plannerTickMsg time.Time

// --- Host integration ---
//
// These methods let the planner render inside the host TUI's shared chrome
// (sidebar, status bar, viewport, input bar) instead of taking over the screen.
// The PlannerView remains the source of truth for state, JSON-protocol parsing,
// and planner-specific shortcuts; the host owns chrome and routing.

// RenderEmbeddedContent returns the planner's body content (requirement card,
// test strategy, task waves, conversation, thinking indicator) sized to the
// given width, with no header/input/help — those come from the host's chrome.
// Height is informational only; the host viewport handles scrolling.
// RenderEmbeddedContent returns the clean planner conversation display.
// This replaces the raw PTY viewport for the planner agent — showing only
// extracted message fields (clean AI responses) and user messages, not raw
// JSON or terminal noise.
func (p *PlannerView) RenderEmbeddedContent(width, height int) string {
	if width <= 0 {
		width = 80
	}
	p.width = width

	var out strings.Builder
	divStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("237"))
	div := divStyle.Render(strings.Repeat("─", width-2))

	// ── State header strip ────────────────────────────────────────────────
	if p.requirement != nil && p.requirement.Refined != "" {
		phase := lipgloss.NewStyle().Foreground(lipgloss.Color("62")).Bold(true).
			Render(p.phaseLabel())
		req := p.requirement.Refined
		maxW := width - lipgloss.Width(phase) - 6
		if maxW > 0 && len([]rune(req)) > maxW {
			req = string([]rune(req)[:maxW-1]) + "…"
		}
		reqText := lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Render(req)
		out.WriteString("  " + phase + "  " + reqText + "\n")

		if len(p.tasks) > 0 {
			info := lipgloss.NewStyle().Foreground(lipgloss.Color("240")).
				Render(fmt.Sprintf("  %d tasks · %d waves", len(p.tasks), p.getMaxWave()))
			out.WriteString(info + "\n")
		}
		out.WriteString(div + "\n")
	}

	// ── Compact task status when plan is active ───────────────────────────
	if len(p.tasks) > 0 && (p.state == StateReviewingPlan || p.state == StatePlanLocked || p.state == StateExecuting) {
		out.WriteString(p.renderTasksCompact())
		out.WriteString(div + "\n")
	}

	// ── Clean conversation ────────────────────────────────────────────────
	userStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Bold(true)
	plannerStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	systemStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Italic(true)
	wrapW := width - 4
	if wrapW < 20 {
		wrapW = width
	}

	if len(p.conversation) == 0 {
		// Empty state — show onboarding hint
		hint := lipgloss.NewStyle().Foreground(lipgloss.Color("240")).
			Render("  Describe what you want to build.")
		out.WriteString("\n" + hint + "\n")
	}

	for _, entry := range p.conversation {
		switch entry.Role {
		case "user":
			line := userStyle.Render("> " + entry.Text)
			out.WriteString(lipgloss.NewStyle().Width(wrapW).Render(line))
			out.WriteString("\n\n")
		case "planner":
			body := renderPlannerMarkdown(entry.Text, wrapW-2, plannerStyle)
			out.WriteString(lipgloss.NewStyle().Width(wrapW).Padding(0, 0, 0, 2).Render(body))
			out.WriteString("\n\n")
		case "system":
			out.WriteString(systemStyle.Render("  ⋯ " + entry.Text))
			out.WriteString("\n")
		}
	}

	// Gate prompt
	if p.currentGate != nil {
		gate := lipgloss.NewStyle().Foreground(lipgloss.Color("220")).Bold(true).
			Render("  ▸ " + p.currentGate.ApprovalPrompt)
		out.WriteString("\n" + gate + "\n")
	}

	// Thinking indicator with animated braille spinner
	if p.thinking {
		frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		frame := frames[(int(time.Now().UnixMilli()/120))%len(frames)]
		spinner := lipgloss.NewStyle().Foreground(lipgloss.Color("62")).Render(frame + " thinking…")
		out.WriteString("  " + spinner + "\n")
	}

	return out.String()
}

func (p *PlannerView) phaseLabel() string {
	switch p.state {
	case StateDefiningRequirement:
		return "clarifying"
	case StateRefiningRequirement:
		return "clarifying"
	case StateDecomposingTasks:
		return "decomposing"
	case StateReviewingPlan:
		return "review"
	case StatePlanLocked:
		return "approved"
	case StateExecuting:
		return "executing"
	default:
		return "planning"
	}
}

// HandleAppInput processes a line of text submitted from the host app's input
// bar. Mirrors the contextual-intent path used by handleInputEnhanced but
// takes text as an argument instead of reading from the planner's internal
// textinput (which the host bypasses in integrated mode).
func (p *PlannerView) HandleAppInput(text string) tea.Cmd {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	// Flush any buffered prose from the previous planner response before
	// recording the new user message. This ensures partial plain-text
	// responses (e.g. when the planner replies without JSON) always appear.
	if trimmed := strings.TrimSpace(p.plannerBuffer); trimmed != "" {
		p.conversation = append(p.conversation, ConversationEntry{Role: "planner", Text: trimmed})
		p.plannerBuffer = ""
	}

	// Add user message to clean conversation log.
	p.conversation = append(p.conversation, ConversationEntry{Role: "user", Text: text})

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

	// After 3 quiet clarifying turns, append a Socratic brainstorm prompt.
	if p.state == StateRefiningRequirement || p.state == StateDefiningRequirement {
		p.clarifyingTurns++
		if p.clarifyingTurns >= 3 && len(p.brainstormThemes) > 0 {
			p.clarifyingTurns = 0
			cmd := p.handleContextualInput(text)
			brainstormCmd := p.conductSocraticDialogue()
			if brainstormCmd != nil && cmd != nil {
				return tea.Batch(cmd, brainstormCmd)
			}
			if brainstormCmd != nil {
				return brainstormCmd
			}
			return cmd
		}
	} else {
		p.clarifyingTurns = 0
	}

	return p.handleContextualInput(text)
}

// HandleAppShortcut tries to consume a planner-specific keyboard shortcut.
// Returns handled=true if the key was a planner shortcut; the host should
// not then dispatch it elsewhere. Non-planner keys return handled=false so
// the host's global handler runs as usual.
//
// Conflict policy with global app keys:
//   - ^p, ^n, ^r, ^a, ^b are consumed by the planner whenever it is active.
//   - ^x is only consumed in StateReviewingPlan (to reject); otherwise the
//     global Interrupt binding wins so the user can still cancel a thinking turn.
func (p *PlannerView) HandleAppShortcut(msg tea.KeyMsg) (handled bool, cmd tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlN:
		p.startNewRequirement()
		return true, nil
	case tea.KeyCtrlR:
		if p.requirement != nil {
			return true, p.refineRequirement()
		}
		return true, nil
	case tea.KeyCtrlP:
		if p.requirement != nil {
			return true, p.stopAndPullPlan()
		}
		return false, nil
	case tea.KeyCtrlB:
		if len(p.brainstormThemes) > 0 {
			return true, p.conductSocraticDialogue()
		}
		return false, nil
	case tea.KeyCtrlA:
		return true, p.approvePlan()
	case tea.KeyCtrlX:
		if p.state == StateReviewingPlan {
			return true, p.rejectPlan()
		}
		return false, nil
	}
	return false, nil
}

// HelpHints returns a short " • "-separated list of planner-specific
// shortcuts to append to the host's help line when the planner is active.
func (p *PlannerView) HelpHints() string {
	var hints []string
	switch p.state {
	case StateDefiningRequirement:
		hints = []string{"^n:restart"}
		if len(p.brainstormThemes) > 0 {
			hints = append(hints, "^b:brainstorm")
		}
	case StateRefiningRequirement:
		hints = []string{"^p:extract", "^r:refine", "^n:restart"}
		if len(p.brainstormThemes) > 0 {
			hints = append(hints, "^b:brainstorm")
		}
	case StateDecomposingTasks:
		hints = []string{"^p:extract", "^r:refine", "^n:restart"}
		if len(p.brainstormThemes) > 0 {
			hints = append(hints, "^b:brainstorm")
		}
	case StateReviewingPlan:
		if len(p.tasks) > 0 {
			hints = []string{"^a:approve", "^x:reject", "^p:re-extract", "^r:refine", "^n:restart"}
		} else {
			hints = []string{"^p:extract", "^r:refine", "^n:restart"}
		}
		if len(p.brainstormThemes) > 0 {
			hints = append(hints, "^b:brainstorm")
		}
	case StatePlanLocked:
		hints = []string{"^a:dispatch"}
	}
	return strings.Join(hints, "  ")
}

// PlaceholderText returns the placeholder shown in the host's input bar when
// the planner is active. It reflects the current phase so users always know
// what kind of input the planner expects.
func (p *PlannerView) PlaceholderText() string {
	switch p.state {
	case StateDefiningRequirement:
		return "Describe what you want to accomplish..."
	case StateRefiningRequirement:
		return "Reply, or say 'done' to advance..."
	case StateDecomposingTasks:
		return "Reply to refine the task breakdown..."
	case StateReviewingPlan:
		if len(p.tasks) > 0 {
			return "Type 'approve' (or ^a) to dispatch, or give feedback..."
		}
		return "Press ^p to extract the plan as JSON, or give feedback..."
	case StatePlanLocked:
		return "Plan locked — press ^a to dispatch, or feedback..."
	case StateExecuting:
		return "Plan executing — give feedback..."
	}
	return "Talk to the planner..."
}

// HandleStreamMsg dispatches a planner-related bubbletea message originating
// from the planner's own async commands (sent/dispatched/error/tick). The
// host calls this when it sees a planner message type so PlannerView can
// update its internal state (e.g., thinking flag, dispatched feedback).
//
// Returns a follow-up cmd (e.g., the next tick for the thinking spinner)
// or nil.
func (p *PlannerView) HandleStreamMsg(msg tea.Msg) tea.Cmd {
	switch m := msg.(type) {
	case plannerSentMsg:
		p.thinking = true
		p.thinkingText = "Planner is thinking..."
		return p.tickThinking()
	case plannerDispatchedMsg:
		p.thinking = false
		p.state = StateExecuting
		p.applyWorkerAssignments(m.assignments)
		target := m.target
		if target == "" {
			target = "workspace"
		}
		p.feedback = append(p.feedback, FeedbackEntry{
			Type:      "system",
			Content:   fmt.Sprintf("Plan dispatched to %s agent. Workers are starting.", target),
			Timestamp: time.Now(),
		})
		return nil
	case plannerErrorMsg:
		p.thinking = false
		errText := "Could not reach planner agent"
		if m.err != nil {
			errText += ": " + m.err.Error()
		}
		p.conversation = append(p.conversation, ConversationEntry{Role: "system", Text: errText})
		return nil
	case plannerTickMsg:
		if p.thinking {
			return p.tickThinking()
		}
		return nil
	}
	return nil
}

// UpdateWorkerStatus updates task status when a worker completes or submits a PR.
// Called from app.go when daemon completion/PR events arrive.
// workerName: the agent name (e.g. "gentle-whale")
// prNumber: PR number (0 = no PR / just completed)
func (p *PlannerView) UpdateWorkerStatus(workerName string, prNumber int, completed bool) {
	if p.taskWorkers == nil || p.tasks == nil {
		return
	}
	// Find which task this worker was assigned to.
	taskID := ""
	for id, w := range p.taskWorkers {
		if w == workerName {
			taskID = id
			break
		}
	}
	if taskID == "" {
		// Worker not tracked in this plan — record it by name.
		taskID = workerName
	}

	changed := false
	if prNumber > 0 && p.taskPRs != nil {
		if p.taskPRs[taskID] != prNumber {
			p.taskPRs[taskID] = prNumber
			changed = true
		}
	}

	// Update the matching task status.
	for i, t := range p.tasks {
		if t.ID == taskID || t.AssignedTo == workerName {
			newStatus := p.tasks[i].Status
			if completed {
				newStatus = TaskStatusCompleted
			} else if prNumber > 0 {
				// PR submitted but not merged yet
				newStatus = TaskStatusInProgress
			}
			if p.tasks[i].Status != newStatus {
				p.tasks[i].Status = newStatus
				changed = true
			}
			if p.tasks[i].AssignedTo != workerName {
				p.tasks[i].AssignedTo = workerName
				changed = true
			}
			break
		}
	}
	if changed {
		p.persistPlan()
	}

	// Check if the current wave is complete and note it in conversation.
	p.checkWaveCompletion()
}

func (p *PlannerView) TrackWorkerAssignment(workerName, taskText string) {
	if workerName == "" || taskText == "" {
		return
	}
	taskID := taskIDFromPlannerMarker(taskText)
	if taskID == "" {
		return
	}
	p.applyWorkerAssignments(map[string]string{taskID: workerName})
}

func (p *PlannerView) applyWorkerAssignments(assignments map[string]string) {
	if len(assignments) == 0 {
		return
	}
	if p.taskWorkers == nil {
		p.taskWorkers = make(map[string]string)
	}
	changed := false
	for taskID, workerName := range assignments {
		if taskID == "" || workerName == "" {
			continue
		}
		if p.taskWorkers[taskID] != workerName {
			p.taskWorkers[taskID] = workerName
			changed = true
		}
		for i := range p.tasks {
			if p.tasks[i].ID == taskID {
				if p.tasks[i].AssignedTo != workerName {
					p.tasks[i].AssignedTo = workerName
					changed = true
				}
				if p.tasks[i].Status == TaskStatusPending {
					p.tasks[i].Status = TaskStatusInProgress
					changed = true
				}
				break
			}
		}
	}
	if changed {
		p.persistPlan()
	}
}

func taskIDFromPlannerMarker(text string) string {
	const prefix = "[planner-task:"
	start := strings.Index(text, prefix)
	if start < 0 {
		return ""
	}
	start += len(prefix)
	end := strings.Index(text[start:], "]")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(text[start : start+end])
}

// checkWaveCompletion checks if all tasks in the current execution wave are
// done, and if so adds a conversation event and marks the wave complete.
func (p *PlannerView) checkWaveCompletion() {
	if p.wavesDone == nil {
		return
	}
	for wave := 0; wave <= p.getMaxWave(); wave++ {
		if p.wavesDone[wave] {
			continue
		}
		tasks := p.tasksForWave(wave)
		if len(tasks) == 0 {
			continue
		}
		allDone := true
		for _, t := range tasks {
			if t.Status != TaskStatusCompleted {
				allDone = false
				break
			}
		}
		if allDone {
			p.wavesDone[wave] = true
			p.conversation = append(p.conversation, ConversationEntry{
				Role: "system",
				Text: fmt.Sprintf("Wave %d complete — %d tasks done.", wave, len(tasks)),
			})
		}
	}
}

// IsPlannerMsg reports whether a bubbletea message originated from the planner.
// The host uses this to decide whether to dispatch the message to HandleStreamMsg.
func IsPlannerMsg(msg tea.Msg) bool {
	switch msg.(type) {
	case plannerSentMsg, plannerErrorMsg, plannerDispatchedMsg, plannerTickMsg:
		return true
	}
	return false
}
