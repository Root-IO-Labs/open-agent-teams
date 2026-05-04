package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Root-IO-Labs/open-agent-teams/internal/socket"
	"github.com/Root-IO-Labs/open-agent-teams/pkg/config"
)

// ViewMode tracks what the TUI is displaying.
type ViewMode int

const (
	ViewWorkspace ViewMode = iota
	ViewAgent              // viewing a specific non-workspace agent
	ViewAgentList          // agent list panel is focused
)

// AgentInfo holds display information about an agent.
type AgentInfo struct {
	Name                   string
	Type                   string
	Alive                  bool
	Task                   string
	TaskSummary            string // short (~60 char) description of current work
	Waiting                bool
	WaitingForVerification bool
	Stuck                  bool
	LogPath                string
	PendingMessages        int
	Model                  string // LLM model this agent is using
	InputTokens            int64  // Cumulative input tokens spent
	OutputTokens           int64  // Cumulative output tokens spent
	TotalTokens            int64  // InputTokens + OutputTokens
	HasTokenData           bool   // true if the agent has ever reported token usage
	MaxTokens              int64  // Token budget (0 = unlimited)
}

type recentInput struct {
	text   string
	sentAt time.Time
}

const recentInputMaxAge = 2 * time.Second

// App is the main bubbletea model.
type App struct {
	// Configuration
	socketPath string
	repoName   string
	paths      *config.Paths

	// Daemon connection
	client *socket.Client

	// View state
	mode          ViewMode
	activeAgent   string // name of agent whose output is shown
	showAgentList bool
	filterEnabled bool
	readOnly      bool // when true in non-workspace view, input is disabled
	expandedView  bool // when true, hides sidebar/chrome for full-width agent output

	// Activity log
	activityLog []ActivityEntry // rolling log of recent agent actions

	// Agents
	agents     []AgentInfo
	agentIndex int // cursor in agent list

	// Components
	viewport viewport.Model
	input    textinput.Model
	filter   *OutputFilter
	renderer *LineRenderer

	// Output streaming
	streams        map[string]OutputStreamI
	streamMode     map[string]string       // agent -> "socket" or "file"
	outputContent  map[string][]string     // agent -> accumulated lines
	outputTypes    map[string][]string     // agent -> line types parallel to outputContent (daemon metadata)
	autoScroll     map[string]bool         // agent -> whether to auto-scroll
	eventStreams   map[string]*EventStream // agent -> sidecar event subscription (nil when OAT_USE_SIDECAR is off)
	eventCounts    map[string]uint64       // agent -> cumulative sidecar events received (observability)
	chatFromEvents map[string]bool         // agent -> true once chat content has arrived via sidecar; activates PTY chat-line suppression for that agent
	turnInFlight   map[string]bool         // agent -> true between turn_start and turn_end; drives the thinking indicator precisely instead of time-since-last-output

	// PTY echo suppression: tracks recently sent input to filter out the
	// PTY echo that appears as bare text in the output stream.
	recentInputs []recentInput // last few inputs sent, for echo detection

	// Thinking indicator: tracks when each agent last produced output.
	// Used to show a "processing..." indicator when the agent is alive but quiet.
	lastOutputTime map[string]time.Time

	// Layout
	width               int
	height              int
	ready               bool
	layoutShowAgentList bool // computed by recalcLayout — whether agent list fits
	layoutShowActivity  bool // computed by recalcLayout — whether activity panel fits

	// Status
	daemonOK  bool
	lastPoll  time.Time
	statusMsg string
	err       error
}

// NewApp creates a new TUI application.
func NewApp(socketPath, repoName string, paths *config.Paths) *App {
	ti := textinput.New()
	ti.Prompt = "" // we render our own prompt in renderInputBar
	ti.Placeholder = "Talk to workspace agent..."
	ti.Focus()
	ti.CharLimit = 4096
	ti.Width = 80

	filter := NewOutputFilter(DefaultFilterConfig())
	return &App{
		socketPath:     socketPath,
		repoName:       repoName,
		paths:          paths,
		client:         socket.NewClient(socketPath),
		filter:         filter,
		renderer:       NewLineRenderer(filter, 80),
		filterEnabled:  true,
		input:          ti,
		streams:        make(map[string]OutputStreamI),
		streamMode:     make(map[string]string),
		eventStreams:   make(map[string]*EventStream),
		eventCounts:    make(map[string]uint64),
		chatFromEvents: make(map[string]bool),
		turnInFlight:   make(map[string]bool),
		outputContent:  make(map[string][]string),
		outputTypes:    make(map[string][]string),
		autoScroll:     make(map[string]bool),
		lastOutputTime: make(map[string]time.Time),
	}
}

// --- Bubbletea messages ---

type tickMsg time.Time       // daemon poll tick (every 2s)
type streamTickMsg time.Time // stream read tick (every 50ms)

type pollResultMsg struct {
	agents   []AgentInfo
	daemonOK bool
	repoName string
}

type sendResultMsg struct {
	err error
}
type openLogDoneMsg struct {
	err error
}

// --- Init ---

func (a *App) Init() tea.Cmd {
	return tea.Batch(
		textinput.Blink,
		a.pollDaemon(),
		a.tick(),
		a.streamTick(),
	)
}

// --- Update ---

func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width = msg.Width
		a.height = msg.Height
		a.recalcLayout()
		return a, nil

	case tickMsg:
		// Daemon poll every 2s
		cmds = append(cmds, a.pollDaemon(), a.tick())
		return a, tea.Batch(cmds...)

	case streamTickMsg:
		// Read from all active streams at high frequency
		cmds = append(cmds, a.streamTick())
		for agent, stream := range a.streams {
			cmds = append(cmds, a.readStream(agent, stream))
		}
		// Also drain structured sidecar events for every agent that has
		// an event subscription. readEventStream returns streamBatchMsg
		// with fromSidecar=true, so the batch handler knows to arm PTY
		// suppression and append the lines authoritatively.
		for agent, es := range a.eventStreams {
			if es.IsClosed() {
				continue
			}
			cmds = append(cmds, a.readEventStream(agent, es))
		}
		// Refresh viewport every ~200ms when the thinking indicator is visible,
		// so the spinner animates. Without this, the viewport only updates on
		// new stream content, and the spinner would be frozen.
		if a.activeAgent != "" {
			if lastOut, ok := a.lastOutputTime[a.activeAgent]; ok {
				elapsed := time.Since(lastOut)
				if elapsed >= 3*time.Second {
					// Only refresh every 4th tick (~200ms) to keep CPU low
					tickCount := int(elapsed.Milliseconds() / 50)
					if tickCount%4 == 0 {
						a.updateViewport()
					}
				}
			}
		}
		return a, tea.Batch(cmds...)

	case pollResultMsg:
		a.daemonOK = msg.daemonOK
		a.lastPoll = time.Now()
		// When daemon is down, keep existing agent list and streams alive.
		// This preserves output content and avoids killing file-based streams
		// that can still tail the log files independently.
		if msg.daemonOK {
			// Remember the agent name at the current cursor position so we
			// can restore it after sorting (the list order from the daemon
			// is non-deterministic because it comes from a Go map).
			cursorAgent := ""
			if a.agentIndex >= 0 && a.agentIndex < len(a.agents) {
				cursorAgent = a.agents[a.agentIndex].Name
			}

			a.agents = msg.agents
			// Sort agents for stable display order: primary agents first,
			// then infrastructure (merge-queue, pr-shepherd), then workers,
			// then transient agents. Within the same type, sort by name.
			sortAgents(a.agents)

			a.statusMsg = "" // clear stale errors on successful poll
			a.syncStreams()

			// Restore agentIndex to point at the same agent name after sort
			if len(a.agents) == 0 {
				a.agentIndex = -1
			} else if cursorAgent != "" {
				a.agentIndex = -1
				for i, ag := range a.agents {
					if ag.Name == cursorAgent {
						a.agentIndex = i
						break
					}
				}
				// If the cursor agent was removed, clamp to last valid index
				if a.agentIndex < 0 {
					a.agentIndex = 0
				}
			} else if a.agentIndex >= len(a.agents) {
				a.agentIndex = len(a.agents) - 1
			}

			// If the active agent was removed from the list, reset it
			if a.activeAgent != "" {
				found := false
				for _, ag := range a.agents {
					if ag.Name == a.activeAgent {
						found = true
						break
					}
				}
				if !found {
					a.activeAgent = "" // will be re-set below
				}
			}
		}
		// If no active agent set, default to workspace/supervisor (the primary agent)
		if a.activeAgent == "" {
			for _, ag := range a.agents {
				if isPrimaryAgent(ag.Type) {
					a.activeAgent = ag.Name
					break
				}
			}
			// Fallback: just pick the first agent
			if a.activeAgent == "" && len(a.agents) > 0 {
				a.activeAgent = a.agents[0].Name
			}
			// Update viewport for the new active agent
			if a.activeAgent != "" {
				a.updateViewport()
			}
		}
		return a, nil

	case streamBatchMsg:
		// Sidecar-first rendering: when a batch arrives from the sidecar
		// event stream (fromSidecar=true), arm chat-content suppression
		// for this agent so subsequent PTY batches don't double-render
		// the same chat content. The sidecar is authoritative for the
		// chat kinds it covers (assistant text, tool calls, tool results);
		// the PTY continues to deliver chrome (spinners, status banners,
		// user-input echoes).
		//
		// When a batch arrives from the PTY (fromSidecar=false) AND
		// sidecar chat content has already arrived for this agent, drop
		// the chat-content line types. This is the "clean UX" mode — we
		// trust the structured event source and hide the noisy scrape.
		if msg.fromSidecar {
			if len(msg.lines) > 0 {
				a.chatFromEvents[msg.agent] = true
			}
			// Turn boundaries drive the thinking indicator directly, so
			// it hides the instant turn_end arrives rather than waiting
			// for 2 minutes of elapsed-since-last-output to expire.
			if msg.sawTurnStart {
				a.turnInFlight[msg.agent] = true
			}
			if msg.sawTurnEnd {
				a.turnInFlight[msg.agent] = false
			}
		} else if a.chatFromEvents[msg.agent] {
			filtered := msg.lines[:0]
			filteredTypes := msg.lineTypes[:0]
			for i, line := range msg.lines {
				lt := ""
				if i < len(msg.lineTypes) {
					lt = msg.lineTypes[i]
				}
				if isChatContentLineType(lt) {
					continue // sidecar is rendering this; skip the PTY copy
				}
				filtered = append(filtered, line)
				filteredTypes = append(filteredTypes, lt)
			}
			msg.lines = filtered
			msg.lineTypes = filteredTypes
		}

		// Filter out PTY echo of recently sent user input.
		// When the user types "hello" and presses enter, the PTY echoes
		// "hello" back as output. Since we already show "> hello" from the
		// input handler, the echo is noise.
		if len(a.recentInputs) > 0 {
			now := time.Now()
			a.pruneRecentInputs(now)
			filtered := msg.lines[:0]
			var matchedInputThisBatch string
			for _, line := range msg.lines {
				normalized := normalizeEchoCandidate(line)
				isEcho := false
				for _, input := range a.recentInputs {
					if normalized == input.text {
						// Mark as echo but keep the entry in recentInputs so
						// fragment detection in later batches still works.
						// The entry expires naturally via pruneRecentInputs.
						matchedInputThisBatch = input.text
						isEcho = true
						break
					}
				}
				if !isEcho && isLikelyEchoFragment(line, matchedInputThisBatch, a.recentInputs, now) {
					isEcho = true
				}
				if !isEcho {
					filtered = append(filtered, line)
				}
			}
			msg.lines = filtered
		}
		if len(msg.lines) == 0 {
			return a, nil
		}

		// Both socket and file streams go through DeduplicateAppend.
		// DeduplicateAppendTyped keeps the types array in sync with content.
		prevLen := len(a.outputContent[msg.agent])
		result, types, replacedIdx := DeduplicateAppendTyped(
			a.outputContent[msg.agent], a.outputTypes[msg.agent],
			msg.lines, msg.lineTypes,
		)
		a.outputContent[msg.agent] = result
		a.outputTypes[msg.agent] = types
		if replacedIdx >= 0 {
			a.renderer.InvalidateCacheFromIndex(msg.agent, replacedIdx)
		}

		// Detect activity events from new lines for the activity log
		activities := detectActivity(msg.agent, msg.lines)
		if len(activities) > 0 {
			hadActivity := len(a.activityLog) > 0
			// Dedup: skip entries that duplicate a recent entry (same agent+action).
			// The daemon and PTY redraws often produce the same tool call line
			// multiple times, flooding the activity panel with identical entries.
			for _, act := range activities {
				isDup := false
				lookback := 8
				for i := len(a.activityLog) - 1; i >= 0 && i >= len(a.activityLog)-lookback; i-- {
					if a.activityLog[i].Agent == act.Agent && a.activityLog[i].Action == act.Action {
						isDup = true
						break
					}
				}
				if !isDup {
					a.activityLog = append(a.activityLog, act)
				}
			}
			if len(a.activityLog) > maxActivityEntries {
				a.activityLog = a.activityLog[len(a.activityLog)-maxActivityEntries:]
			}
			// Recalc layout when activity panel first appears so word wrap adjusts.
			// recalcLayout() already invalidates the entire render cache when
			// width changes and calls updateViewport() for the active agent,
			// so no extra invalidation or viewport update is needed here.
			if !hadActivity {
				a.recalcLayout()
			}
		}

		// Cap buffer at 5000 lines
		if lines := a.outputContent[msg.agent]; len(lines) > 5000 {
			trimmed := lines[len(lines)-4000:]
			trimmed[0] = "--- [older output trimmed] ---"
			a.outputContent[msg.agent] = trimmed
			// Trim types in parallel
			if t := a.outputTypes[msg.agent]; len(t) > 5000 {
				a.outputTypes[msg.agent] = t[len(t)-4000:]
			}
			a.renderer.InvalidateCacheForAgent(msg.agent)
		}
		// Only update the viewport if content actually changed for the active agent.
		// This avoids re-rendering the entire viewport on no-op dedup batches.
		contentChanged := len(a.outputContent[msg.agent]) != prevLen || replacedIdx >= 0
		if contentChanged {
			a.lastOutputTime[msg.agent] = time.Now()
		}
		if msg.agent == a.activeAgent && contentChanged {
			a.updateViewport()
		}
		return a, nil

	case sendResultMsg:
		if msg.err != nil {
			a.statusMsg = fmt.Sprintf("Send failed: %v", msg.err)
		} else {
			a.statusMsg = ""
		}
		return a, nil

	case openLogDoneMsg:
		// Returned from less pager — refresh the viewport
		if msg.err != nil {
			a.statusMsg = fmt.Sprintf("Log viewer error: %v", msg.err)
		}
		a.recalcLayout()
		return a, nil

	case tea.KeyMsg:
		return a.handleKey(msg)
	}

	// Update sub-components
	if a.mode != ViewAgentList {
		var cmd tea.Cmd
		a.input, cmd = a.input.Update(msg)
		cmds = append(cmds, cmd)
	}

	return a, tea.Batch(cmds...)
}

func (a *App) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Global keys that always work
	switch {
	case key.Matches(msg, keys.Quit):
		a.cleanup()
		return a, tea.Quit

	case key.Matches(msg, keys.TogglePanel):
		a.showAgentList = !a.showAgentList
		if a.showAgentList {
			a.mode = ViewAgentList
			a.input.Blur()
		} else {
			if a.activeAgent != "" {
				a.mode = a.modeForAgent(a.activeAgent)
			}
			a.input.Focus()
		}
		a.recalcLayout()
		return a, nil

	case key.Matches(msg, keys.Workspace):
		// Return to primary agent view (workspace or supervisor)
		for _, ag := range a.agents {
			if isPrimaryAgent(ag.Type) {
				a.switchToAgent(ag.Name)
				break
			}
		}
		a.showAgentList = false
		a.mode = ViewWorkspace
		a.input.Focus()
		a.input.Placeholder = "Talk to workspace agent..."
		a.recalcLayout()
		return a, nil

	case key.Matches(msg, keys.ToggleFilter):
		a.filterEnabled = !a.filterEnabled
		// Rebuild filter and re-filter existing content
		if a.filterEnabled {
			a.filter = NewOutputFilter(DefaultFilterConfig())
		} else {
			cfg := DefaultFilterConfig()
			cfg.ShowProgress = true
			cfg.ShowChrome = true
			a.filter = NewOutputFilter(cfg)
		}
		a.renderer = NewLineRenderer(a.filter, a.renderer.width)
		a.rebuildStreams()
		return a, nil

	case key.Matches(msg, keys.Interrupt):
		if a.activeAgent != "" {
			return a, a.interruptAgent(a.activeAgent)
		}
		return a, nil

	case key.Matches(msg, keys.ToggleReadOnly):
		if a.mode == ViewAgent {
			a.readOnly = !a.readOnly
			if a.readOnly {
				a.input.Blur()
			} else {
				a.input.Focus()
			}
		}
		return a, nil

	case key.Matches(msg, keys.ExpandView):
		// Toggle expanded/focused view. When expanding: hide sidebar and
		// activity panel for full-width output. When collapsing: restore
		// the sidebar so agents are visible.
		a.expandedView = !a.expandedView
		if a.expandedView {
			a.showAgentList = false
			a.mode = a.modeForAgent(a.activeAgent)
			a.input.Focus()
		} else {
			a.showAgentList = true
			a.mode = ViewAgentList
			a.input.Blur()
		}
		if a.activeAgent != "" {
			a.autoScroll[a.activeAgent] = true
		}
		a.recalcLayout()
		return a, nil

	case key.Matches(msg, keys.OpenLog):
		// Open the active agent's full log file in a pager (less).
		// This suspends the TUI temporarily and returns when the user exits less.
		if a.activeAgent != "" {
			logPath := a.getAgentLogPath(a.activeAgent)
			if logPath != "" {
				if _, err := os.Stat(logPath); err == nil {
					return a, tea.ExecProcess(exec.Command("less", "+G", "-R", logPath), func(err error) tea.Msg {
						return openLogDoneMsg{err: err}
					})
				}
				a.statusMsg = fmt.Sprintf("Log file not found: %s", logPath)
			} else {
				a.statusMsg = "No log file for this agent"
			}
		}
		return a, nil

	case key.Matches(msg, keys.NewWorker):
		a.statusMsg = "Use: oat work \"task description\" (from CLI)"
		return a, nil
	}

	// Agent list navigation
	if a.mode == ViewAgentList {
		switch {
		case key.Matches(msg, keys.ScrollUp), key.Matches(msg, keys.PrevAgent):
			if len(a.agents) > 0 {
				if a.agentIndex <= 0 {
					a.agentIndex = 0
				} else {
					a.agentIndex--
				}
			}
			return a, nil
		case key.Matches(msg, keys.ScrollDown), key.Matches(msg, keys.NextAgent):
			if len(a.agents) > 0 {
				if a.agentIndex < 0 {
					a.agentIndex = 0
				} else if a.agentIndex < len(a.agents)-1 {
					a.agentIndex++
				}
			}
			return a, nil
		case key.Matches(msg, keys.SelectAgent):
			if a.agentIndex >= 0 && a.agentIndex < len(a.agents) {
				ag := a.agents[a.agentIndex]
				a.switchToAgent(ag.Name)
				a.showAgentList = false
				a.mode = a.modeForAgent(ag.Name)
				a.input.Focus()
				a.recalcLayout()
			}
			return a, nil
		}
		return a, nil
	}

	// Viewport scrolling
	switch {
	case key.Matches(msg, keys.ScrollUp):
		a.autoScroll[a.activeAgent] = false
		a.viewport.LineUp(1)
		return a, nil
	case key.Matches(msg, keys.ScrollDown):
		a.viewport.LineDown(1)
		if a.viewport.AtBottom() {
			a.autoScroll[a.activeAgent] = true
		}
		return a, nil
	case key.Matches(msg, keys.PageUp):
		a.autoScroll[a.activeAgent] = false
		a.viewport.HalfViewUp()
		return a, nil
	case key.Matches(msg, keys.PageDown):
		a.viewport.HalfViewDown()
		if a.viewport.AtBottom() {
			a.autoScroll[a.activeAgent] = true
		}
		return a, nil
	case key.Matches(msg, keys.GoToTop):
		a.autoScroll[a.activeAgent] = false
		a.viewport.GotoTop()
		return a, nil
	case key.Matches(msg, keys.GoToBottom):
		a.autoScroll[a.activeAgent] = true
		a.viewport.GotoBottom()
		return a, nil
	}

	// Input submission
	if msg.Type == tea.KeyEnter {
		text := strings.TrimSpace(a.input.Value())
		if text != "" {
			a.input.SetValue("")
			// Track input for PTY echo suppression (keep last 5)
			now := time.Now()
			a.pruneRecentInputs(now)
			a.recentInputs = append(a.recentInputs, recentInput{text: text, sentAt: now})
			if len(a.recentInputs) > 5 {
				a.recentInputs = a.recentInputs[len(a.recentInputs)-5:]
			}
			// Show the sent message immediately in the viewport.
			// Only add a blank separator if the last line isn't already empty,
			// so rapid multi-message sends don't create double blanks.
			sentLine := fmt.Sprintf("> %s", text)
			lines := a.outputContent[a.activeAgent]
			if len(lines) == 0 || lines[len(lines)-1] != "" {
				a.outputContent[a.activeAgent] = append(a.outputContent[a.activeAgent], "", sentLine)
			} else {
				a.outputContent[a.activeAgent] = append(a.outputContent[a.activeAgent], sentLine)
			}
			a.autoScroll[a.activeAgent] = true
			// Reset thinking indicator timer so it counts from this send,
			// not from the last stream output. Without this the elapsed
			// time shown is stale and misleading after user input.
			a.lastOutputTime[a.activeAgent] = time.Now()
			a.updateViewport()
			return a, a.sendInput(a.activeAgent, text)
		}
		return a, nil
	}

	// Pass to text input
	var cmd tea.Cmd
	a.input, cmd = a.input.Update(msg)
	return a, cmd
}

// --- View ---

func (a *App) View() string {
	if !a.ready {
		return "\n" +
			styleBrandTag.Render(" OAT ") + "\n" +
			styleStatusAgent.Render("  Open Agent Teams") + "\n\n" +
			styleHelp.Render("  Loading...")
	}

	var sections []string

	// Status bar (top)
	sections = append(sections, a.renderStatusBar())

	// Main content area — panel visibility determined by recalcLayout() budget.
	vpHeight := a.viewportHeight()
	vpWidth := a.viewport.Width
	showList := a.layoutShowAgentList
	showActivity := a.layoutShowActivity

	vpView := a.viewport.View()

	if showList || showActivity {
		divLines := make([]string, vpHeight)
		for i := range divLines {
			divLines[i] = "│"
		}
		divider := lipgloss.NewStyle().
			Width(1).
			Height(vpHeight).
			Foreground(colorBorder).
			Render(strings.Join(divLines, "\n"))

		var panels []string

		if showList {
			panels = append(panels, a.renderAgentList(30, vpHeight))
			panels = append(panels, divider)
		}

		panels = append(panels, lipgloss.NewStyle().MaxWidth(vpWidth).Render(vpView))

		if showActivity {
			actWidth := a.activityPanelWidth()
			actView := renderActivityLog(a.activityLog, actWidth, vpHeight)
			panels = append(panels, divider)
			panels = append(panels, lipgloss.NewStyle().MaxWidth(actWidth).Render(actView))
		}

		sections = append(sections, lipgloss.JoinHorizontal(lipgloss.Top, panels...))
	} else {
		sections = append(sections, vpView)
	}

	// Token/cost bar (persistent footer)
	sections = append(sections, a.renderTokenBar())

	// Input bar (bottom)
	sections = append(sections, a.renderInputBar())

	// Help line
	sections = append(sections, a.renderHelp())

	return lipgloss.JoinVertical(lipgloss.Left, sections...)
}

func (a *App) renderStatusBar() string {
	w := a.width

	// Left: OAT brand + repo + active agent
	brand := styleBrandTag.Render("OAT")
	left := brand + " " + styleStatusRepo.Render(a.repoName)
	if a.activeAgent != "" {
		agentType := ""
		for _, ag := range a.agents {
			if ag.Name == a.activeAgent {
				agentType = ag.Type
				break
			}
		}
		left += " " + styleStatusAgent.Render(agentType+":"+a.activeAgent)
	}

	// Right: daemon status + stream mode (compact, high priority info)
	right := ""
	if a.daemonOK {
		right = styleStatusOK.Render("ok")
	} else {
		right = styleStatusWarn.Render("--")
	}
	if a.activeAgent != "" {
		if mode, ok := a.streamMode[a.activeAgent]; ok {
			if mode == "socket" {
				right += " " + styleStatusOK.Render("[live]")
			} else {
				right += " [log]"
			}
		}
	}
	if a.filterEnabled {
		right += " " + styleFilterActive.Render("[f]")
	}

	// Center: agent count + message badge
	aliveCount := 0
	totalPendingMsgs := 0
	for _, ag := range a.agents {
		if ag.Alive {
			aliveCount++
		}
		totalPendingMsgs += ag.PendingMessages
	}
	center := fmt.Sprintf("%d/%d agents", aliveCount, len(a.agents))
	if totalPendingMsgs > 0 {
		center += " " + styleStatusWarn.Render(fmt.Sprintf("[%d msg]", totalPendingMsgs))
	}

	// Status message (error/info) — append to right, truncated to fit
	if a.statusMsg != "" {
		// Will be added after we know remaining space
	}

	// Fit everything within terminal width. Priority: left > right > center.
	leftW := lipgloss.Width(left)
	rightW := lipgloss.Width(right)
	centerW := lipgloss.Width(center)

	// Truncate left if it alone exceeds half the width
	if leftW > w/2 {
		// Rebuild left with abbreviated agent name
		left = brand + " " + styleStatusRepo.Render(a.repoName)
		leftW = lipgloss.Width(left)
	}

	// Add status message to right if it fits
	if a.statusMsg != "" {
		maxMsg := w - leftW - rightW - centerW - 4 // 4 = minimum gaps
		if maxMsg > 5 {
			msg := a.statusMsg
			msgRunes := []rune(msg)
			if len(msgRunes) > maxMsg {
				msg = string(msgRunes[:maxMsg-2]) + ".."
			}
			right += " " + styleStatusWarn.Render(msg)
			rightW = lipgloss.Width(right)
		}
	}

	// Drop center if there's not enough room
	totalNeeded := leftW + centerW + rightW + 2 // minimum 2 gaps
	if totalNeeded > w {
		center = fmt.Sprintf("%d/%d", aliveCount, len(a.agents))
		centerW = lipgloss.Width(center)
	}
	if leftW+centerW+rightW+2 > w {
		center = ""
		centerW = 0
	}

	// Compose with even spacing
	gap := w - leftW - rightW - centerW
	if gap < 2 {
		gap = 2
	}
	leftGap := gap / 2
	rightGap := gap - leftGap

	bar := left + strings.Repeat(" ", leftGap) + center + strings.Repeat(" ", rightGap) + right

	return styleStatusBar.Width(w).MaxWidth(w).MaxHeight(1).Render(bar)
}

func shortenModelID(model string) string {
	if model == "" {
		return ""
	}
	if i := strings.Index(model, ":"); i >= 0 {
		return model[i+1:]
	}
	return model
}

func (a *App) renderAgentList(width, height int) string {
	var b strings.Builder

	// Title with underline separator
	title := styleAgentListTitle.Width(width).Render("Agents")
	b.WriteString(title)
	b.WriteString("\n")
	b.WriteString(styleAgentListDivider.Width(width).Render(strings.Repeat("─", width-2)))
	b.WriteString("\n")

	for i, ag := range a.agents {
		// Status indicator
		var status string
		if ag.Waiting {
			status = styleAgentWaiting.Render("~")
		} else if ag.Alive {
			status = styleAgentAlive.Render("●")
		} else {
			status = styleAgentDead.Render("○")
		}

		// Name on its own line, type + summary below
		name := ag.Name
		nameRunes := []rune(name)
		maxNameLen := width - 6 // account for status + padding
		if maxNameLen > 3 && len(nameRunes) > maxNameLen {
			name = string(nameRunes[:maxNameLen-3]) + "..."
		}
		label := fmt.Sprintf(" %s %s", status, name)

		// Build type line with optional model
		short := shortenModelID(ag.Model)
		var typeLine string
		if short != "" {
			typeLine = fmt.Sprintf("     %s (%s)", ag.Type, short)
		} else {
			typeLine = fmt.Sprintf("     %s", ag.Type)
		}
		typeRunes := []rune(typeLine)
		if width > 5 && len(typeRunes) > width-2 {
			typeLine = string(typeRunes[:width-5]) + "..."
		}

		// Build summary line
		summary := ag.TaskSummary
		if summary == "" && ag.Task != "" {
			summary = ag.Task
			summaryRunes := []rune(summary)
			if len(summaryRunes) > 50 {
				summary = string(summaryRunes[:47]) + "..."
			}
		}
		var summaryLine string
		if summary != "" {
			summaryLine = fmt.Sprintf("     %s", summary)
		} else if ag.Waiting {
			if ag.WaitingForVerification {
				summaryLine = "     waiting for verification"
			} else {
				summaryLine = "     waiting for PR"
			}
		} else if !ag.Alive {
			summaryLine = "     stopped"
		} else {
			summaryLine = ""
		}
		if summaryLine != "" {
			summaryRunes := []rune(summaryLine)
			if width > 5 && len(summaryRunes) > width-2 {
				summaryLine = string(summaryRunes[:width-5]) + "..."
			}
		}

		isCursor := i == a.agentIndex && a.mode == ViewAgentList && a.agentIndex >= 0
		isActive := ag.Name == a.activeAgent

		if isCursor {
			b.WriteString(styleAgentCursor.Width(width).Render(label))
			b.WriteString("\n")
			b.WriteString(styleAgentCursor.Width(width).Render(typeLine))
			b.WriteString("\n")
			b.WriteString(styleAgentCursor.Width(width).Render(summaryLine))
		} else if isActive {
			b.WriteString(styleAgentActive.Width(width).Render(label))
			b.WriteString("\n")
			b.WriteString(styleAgentActive.Width(width).Render(typeLine))
			b.WriteString("\n")
			b.WriteString(styleAgentActive.Width(width).Render(summaryLine))
		} else {
			b.WriteString(styleAgentNormal.Width(width).Render(label))
			b.WriteString("\n")
			b.WriteString(styleAgentNormalType.Width(width).Render(typeLine))
			b.WriteString("\n")
			b.WriteString(styleAgentNormalType.Width(width).Render(summaryLine))
		}
		b.WriteString("\n")

		// Separator between agents
		if i < len(a.agents)-1 {
			b.WriteString(styleAgentListDivider.Width(width).Render(strings.Repeat("─", width-2)))
			b.WriteString("\n")
		}
	}

	// Pad remaining height — count lines as we go instead of scanning the buffer
	lineCount := 2 // title + divider
	for i := range a.agents {
		lineCount += 3 // label + typeLine + summaryLine
		if i < len(a.agents)-1 {
			lineCount++ // separator
		}
	}
	for i := lineCount; i < height; i++ {
		b.WriteString("\n")
	}

	return lipgloss.NewStyle().Width(width).MaxHeight(height).Render(b.String())
}

func (a *App) renderTokenBar() string {
	agentCount := len(a.agents)
	if agentCount == 0 {
		return styleTokenBar.Width(a.width).MaxWidth(a.width).MaxHeight(1).Render("  No agents connected")
	}

	var parts []string

	// Active agent model
	var activeAgent *AgentInfo
	for i := range a.agents {
		if a.agents[i].Name == a.activeAgent {
			activeAgent = &a.agents[i]
			break
		}
	}
	if activeAgent != nil && activeAgent.Model != "" {
		parts = append(parts, styleTokenBarModel.Render("Model: "+activeAgent.Model))
	}

	// Token spend for active agent
	if activeAgent != nil {
		if activeAgent.HasTokenData {
			tokenStr := fmt.Sprintf("In: %s  Out: %s",
				formatTokenCount(activeAgent.InputTokens),
				formatTokenCount(activeAgent.OutputTokens))
			if activeAgent.MaxTokens > 0 {
				pct := float64(activeAgent.TotalTokens) / float64(activeAgent.MaxTokens) * 100
				tokenStr += fmt.Sprintf(" (%s/%s %.0f%%)",
					formatTokenCount(activeAgent.TotalTokens),
					formatTokenCount(activeAgent.MaxTokens),
					pct)
			}
			parts = append(parts, tokenStr)
		} else {
			if activeAgent.MaxTokens > 0 {
				parts = append(parts, fmt.Sprintf("Tokens: — (budget: %s)", formatTokenCount(activeAgent.MaxTokens)))
			} else {
				parts = append(parts, "Tokens: —")
			}
		}
	}

	aliveCount := 0
	for _, ag := range a.agents {
		if ag.Alive {
			aliveCount++
		}
	}
	parts = append(parts, fmt.Sprintf("%d/%d agents", aliveCount, agentCount))

	content := "  " + strings.Join(parts, "  |  ")
	return styleTokenBar.Width(a.width).MaxWidth(a.width).MaxHeight(1).Render(content)
}

// formatTokenCount formats a token count for human display.
func formatTokenCount(count int64) string {
	switch {
	case count >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(count)/1_000_000)
	case count >= 1_000:
		return fmt.Sprintf("%.1fK", float64(count)/1_000)
	default:
		return fmt.Sprintf("%d", count)
	}
}

func (a *App) renderInputBar() string {
	if a.readOnly && a.mode == ViewAgent {
		return styleHelp.Width(a.width).Render("  [read-only] ctrl+r to enable input")
	}

	prompt := styleInputPrompt.Render("> ")
	a.input.Width = a.width - 4
	return prompt + a.input.View()
}

func (a *App) renderHelp() string {
	// Full and short variants for narrow terminals
	var help string
	if a.mode == ViewAgentList {
		if a.width < 50 {
			help = "↑↓:nav  enter:sel  esc:back  ^c:quit"
		} else {
			help = "↑↓:nav  enter:select  esc:back  ^o:log  ^c:quit"
		}
	} else if a.mode == ViewAgent {
		if a.width < 70 {
			help = "tab:agents  esc:back  ^e:expand  ^r:input  ^c:quit"
		} else {
			help = "tab:agents  esc:workspace  ^o:log  ^e:expand  ^r:input  ^f:filter  ^c:quit"
		}
	} else {
		if a.width < 70 {
			help = "tab:agents  esc:back  ^e:expand  ^f:filter  ^c:quit"
		} else {
			help = "tab:agents  esc:workspace  ^o:log  ^e:expand  ^f:filter  ^x:interrupt  ^c:quit"
		}
	}
	return styleHelp.Width(a.width).MaxWidth(a.width).MaxHeight(1).Render("  " + help)
}

// --- Layout ---

func (a *App) viewportHeight() int {
	// status bar (1) + token bar (1) + input bar (1) + help (1) = 4 lines of chrome
	h := a.height - 4
	if h < 5 {
		h = 5
	}
	return h
}

func (a *App) recalcLayout() {
	vpHeight := a.viewportHeight()

	// Layout budget system: viewport gets priority over side panels.
	// If adding a panel would crush the viewport below minViewportWidth,
	// that panel is disabled. Priority: viewport > agent list > activity.
	const minViewportWidth = 40
	const agentListWidth = 30
	const dividerWidth = 1

	vpWidth := a.width

	// Determine which panels can fit, starting from highest priority.
	wantAgentList := a.showAgentList && !a.expandedView
	wantActivity := len(a.activityLog) > 0 && !a.expandedView && a.width > 80
	actWidth := a.activityPanelWidth()

	// Try agent list first (higher priority than activity)
	showList := false
	if wantAgentList {
		remaining := vpWidth - agentListWidth - dividerWidth
		if remaining >= minViewportWidth {
			showList = true
			vpWidth = remaining
		}
	}

	// Then try activity panel (lower priority — dropped first)
	a.layoutShowActivity = false
	if wantActivity {
		remaining := vpWidth - actWidth - dividerWidth
		if remaining >= minViewportWidth {
			a.layoutShowActivity = true
			vpWidth = remaining
		}
	}

	// Override showAgentList based on budget (View() reads this)
	a.layoutShowAgentList = showList

	// Update renderer width for word wrapping
	if a.renderer.width != vpWidth-2 {
		a.renderer.width = vpWidth - 2
		a.renderer.wrapStyle = lipgloss.NewStyle().Width(vpWidth - 2)
		a.renderer.InvalidateCache()
	}

	if !a.ready {
		a.viewport = viewport.New(vpWidth, vpHeight)
		a.viewport.YPosition = 1
		a.ready = true
		// Auto-scroll on for all agents by default
		for _, ag := range a.agents {
			a.autoScroll[ag.Name] = true
		}
	} else {
		a.viewport.Width = vpWidth
		a.viewport.Height = vpHeight
	}

	// Update viewport content for current agent
	if a.activeAgent != "" {
		a.updateViewport()
	}
}

// --- Agent management ---

func (a *App) switchToAgent(name string) {
	a.activeAgent = name

	// Initialize auto-scroll for new agents, but preserve existing state
	if _, exists := a.autoScroll[name]; !exists {
		a.autoScroll[name] = true
	}

	// Update input placeholder
	for _, ag := range a.agents {
		if ag.Name == name {
			if isPrimaryAgent(ag.Type) {
				a.input.Placeholder = fmt.Sprintf("Talk to %s agent...", ag.Type)
				a.readOnly = false
			} else {
				a.input.Placeholder = fmt.Sprintf("Talk to %s...", name)
				a.readOnly = false
			}
			break
		}
	}

	// Update viewport content and sync position with auto-scroll state
	a.viewport.SetContent(a.renderContentForViewport(name))
	a.syncViewportWithAutoScroll(name)
}

// syncViewportWithAutoScroll ensures viewport position matches auto-scroll state
func (a *App) syncViewportWithAutoScroll(agentName string) {
	if a.autoScroll[agentName] {
		a.viewport.GotoBottom()
	}
	// If auto-scroll is false, maintain current viewport position
	// This preserves user's manual scroll position
}

func (a *App) modeForAgent(name string) ViewMode {
	for _, ag := range a.agents {
		if ag.Name == name && isPrimaryAgent(ag.Type) {
			return ViewWorkspace
		}
	}
	return ViewAgent
}

// getAgentLogPath returns the log file path for the given agent name.
func (a *App) getAgentLogPath(agentName string) string {
	for _, ag := range a.agents {
		if ag.Name == agentName && ag.LogPath != "" {
			return ag.LogPath
		}
	}
	return ""
}

// activityPanelWidth returns the width for the activity panel, scaled to
// terminal width. Uses 20% of width, clamped between 20 and 35.
func (a *App) activityPanelWidth() int {
	w := a.width / 5
	if w < 20 {
		w = 20
	}
	if w > 35 {
		w = 35
	}
	return w
}

// isPrimaryAgent returns true for agent types that act as the main orchestrator.
func isPrimaryAgent(agentType string) bool {
	switch agentType {
	case "workspace", "supervisor":
		return true
	}
	return false
}

// agentTypePriority returns a numeric sort priority for each agent type.
// Lower numbers sort first. This ensures a stable, logical display order:
// primary agents at top, infrastructure next, then workers and transient agents.
func agentTypePriority(agentType string) int {
	switch agentType {
	case "workspace":
		return 0
	case "supervisor":
		return 1
	case "merge-queue":
		return 2
	case "pr-shepherd":
		return 3
	case "generic-persistent":
		return 4
	case "worker":
		return 5
	case "review":
		return 6
	case "verification":
		return 7
	default:
		return 8
	}
}

// sortAgents sorts agents by type priority (primary first, then infrastructure,
// then workers/transient), and alphabetically by name within the same type.
func sortAgents(agents []AgentInfo) {
	sort.SliceStable(agents, func(i, j int) bool {
		pi, pj := agentTypePriority(agents[i].Type), agentTypePriority(agents[j].Type)
		if pi != pj {
			return pi < pj
		}
		return agents[i].Name < agents[j].Name
	})
}

// --- Stream management ---

func (a *App) syncStreams() {
	// Build set of current agent names
	current := make(map[string]bool)
	for _, ag := range a.agents {
		current[ag.Name] = true
	}

	// Start streams for new agents, or restart dead streams
	for _, ag := range a.agents {
		if ag.LogPath == "" {
			continue
		}
		existing, exists := a.streams[ag.Name]
		if exists && !existing.IsClosed() {
			continue // stream still alive — leave it alone
		}
		socketFailed := false
		if exists {
			// Stream died — if it was a socket stream, don't retry socket
			if a.streamMode[ag.Name] == "socket" {
				socketFailed = true
			}
			delete(a.streams, ag.Name)
			delete(a.streamMode, ag.Name)
		}

		var filter *OutputFilter
		if a.filterEnabled {
			filter = a.filter
		}

		// Try SocketStream for alive agents that haven't already failed socket.
		// Socket streaming requires a live PTY broadcaster (not available for
		// re-adopted agents). If socket fails, we fall through to LogStream.
		if ag.Alive && !socketFailed {
			hasPrior := len(a.outputContent[ag.Name]) > 0
			ss := NewSocketStream(a.client, a.repoName, ag.Name, filter, hasPrior)
			ss.Start()
			a.streams[ag.Name] = ss
			a.streamMode[ag.Name] = "socket"
			if !hasPrior {
				a.autoScroll[ag.Name] = true
			}
			// When OAT_USE_SIDECAR=1, also subscribe to structured events.
			// The EventStream runs alongside the SocketStream — current
			// behavior stays, events land in eventCounts for observability.
			// Future work (Day 4c) switches chat rendering to be driven by
			// events directly, at which point the SocketStream becomes chrome-
			// only. Starting the subscription now gives us a live traffic
			// sample to verify the pipeline on real workloads.
			a.startEventStreamFor(ag.Name)
			continue
		}

		// LogStream fallback — used for dead agents, adopted agents without
		// a PTY broadcaster, or when socket streaming failed.
		hasPrior := len(a.outputContent[ag.Name]) > 0
		stream := NewLogStream(ag.LogPath, filter)
		stream.Start(!hasPrior) // catchUp=true only if no prior content
		a.streams[ag.Name] = stream
		a.streamMode[ag.Name] = "file"
		if !hasPrior {
			a.autoScroll[ag.Name] = true
		}
	}

	// Stop streams for removed agents
	for name, stream := range a.streams {
		if !current[name] {
			stream.Stop()
			delete(a.streams, name)
			delete(a.streamMode, name)
			a.stopEventStreamFor(name)
		}
	}
}

func (a *App) rebuildStreams() {
	// Stop all streams and restart with new filter settings.
	// Preserve output buffers so users don't lose visible history on filter toggle.
	for _, stream := range a.streams {
		stream.Stop()
	}
	for _, es := range a.eventStreams {
		es.Stop()
	}
	a.streams = make(map[string]OutputStreamI)
	a.streamMode = make(map[string]string)
	a.eventStreams = make(map[string]*EventStream)
	// eventCounts intentionally NOT reset — keeps the lifetime counter
	// stable across filter toggles, which is what observability wants.
	// Invalidate all renderer caches so content is re-rendered with new filter
	a.renderer.InvalidateCache()
	a.syncStreams()
	if a.activeAgent != "" {
		a.updateViewport()
	}
}

func (a *App) cleanup() {
	for _, stream := range a.streams {
		stream.Stop()
	}
	for _, es := range a.eventStreams {
		es.Stop()
	}
}

// --- Commands (async operations) ---

func (a *App) tick() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (a *App) streamTick() tea.Cmd {
	return tea.Tick(50*time.Millisecond, func(t time.Time) tea.Msg {
		return streamTickMsg(t)
	})
}

func (a *App) pollDaemon() tea.Cmd {
	client := a.client
	repoName := a.repoName
	outputDir := a.paths.OutputDir

	return func() tea.Msg {
		result := pollResultMsg{}

		// Ping daemon
		resp, err := client.Send(socket.Request{Command: "ping"})
		if err != nil || !resp.Success {
			return result
		}
		result.daemonOK = true

		// Get agent list with rich status info
		resp, err = client.Send(socket.Request{
			Command: "list_agents",
			Args: map[string]interface{}{
				"repo": repoName,
				"rich": true,
			},
		})
		if err != nil || !resp.Success {
			return result
		}

		// Parse agent list from response
		if data, ok := resp.Data.([]interface{}); ok {
			for _, item := range data {
				if agentMap, ok := item.(map[string]interface{}); ok {
					info := AgentInfo{
						Name: getString(agentMap, "name"),
						Type: getString(agentMap, "type"),
					}
					// Use daemon-provided log path if available, else compute client-side
					if lp := getString(agentMap, "log_path"); lp != "" {
						info.LogPath = lp
					} else {
						isWorker := info.Type == "worker" || info.Type == "review"
						if isWorker {
							info.LogPath = filepath.Join(outputDir, repoName, "workers", info.Name+".log")
						} else {
							info.LogPath = filepath.Join(outputDir, repoName, info.Name+".log")
						}
					}
					info.Task = getString(agentMap, "task")
					info.TaskSummary = getString(agentMap, "summary")
					info.Model = getString(agentMap, "model")
					info.Alive = getString(agentMap, "status") == "running"
					info.Waiting = getBool(agentMap, "waiting_for_pr") || getBool(agentMap, "waiting_for_verification")
					info.WaitingForVerification = getBool(agentMap, "waiting_for_verification")
					info.PendingMessages = getInt(agentMap, "messages_pending")
					info.InputTokens = getInt64(agentMap, "input_tokens")
					info.OutputTokens = getInt64(agentMap, "output_tokens")
					info.TotalTokens = getInt64(agentMap, "total_tokens")
					info.HasTokenData = getString(agentMap, "last_token_update") != ""
					info.MaxTokens = getInt64(agentMap, "max_tokens")

					result.agents = append(result.agents, info)
				}
			}
		}

		return result
	}
}

// streamBatchMsg delivers multiple lines at once to reduce per-line overhead.
type streamBatchMsg struct {
	agent     string
	lines     []string
	lineTypes []string // parallel to lines — daemon-provided type ("tool_call", etc.) or "" if unknown
	// fromSidecar distinguishes batches produced by readEventStream
	// (structured sidecar events) from batches produced by readStream
	// (PTY-scraped text). When true, the batch is authoritative chat
	// content and arms PTY suppression for this agent; when false, the
	// Update handler filters out chat-content line types if sidecar
	// has already arrived for this agent.
	fromSidecar bool
	// sawTurnStart / sawTurnEnd carry turn-boundary signals out of
	// readEventStream to the Update handler. turnInFlight[agent] is
	// flipped on these, which is what drives the thinking indicator
	// (previously it used time-since-last-output, which left the
	// indicator stuck visible after the agent finished).
	sawTurnStart bool
	sawTurnEnd   bool
}

func (a *App) readStream(agent string, stream OutputStreamI) tea.Cmd {
	return func() tea.Msg {
		var lines []string
		var lineTypes []string

		// Prefer TypedLines (SocketStream with daemon metadata).
		// Fall back to plain Lines (LogStream file tailing).
		typedCh := stream.TypedLines()
		plainCh := stream.Lines()

		for {
			if typedCh != nil {
				select {
				case tl, ok := <-typedCh:
					if !ok {
						if len(lines) > 0 {
							return streamBatchMsg{agent: agent, lines: lines, lineTypes: lineTypes}
						}
						return nil
					}
					lines = append(lines, tl.Text)
					lineTypes = append(lineTypes, tl.LineType)
					if len(lines) >= 100 {
						return streamBatchMsg{agent: agent, lines: lines, lineTypes: lineTypes}
					}
				default:
					if len(lines) > 0 {
						return streamBatchMsg{agent: agent, lines: lines, lineTypes: lineTypes}
					}
					return nil
				}
			} else {
				select {
				case line, ok := <-plainCh:
					if !ok {
						if len(lines) > 0 {
							return streamBatchMsg{agent: agent, lines: lines, lineTypes: lineTypes}
						}
						return nil
					}
					lines = append(lines, line)
					lineTypes = append(lineTypes, "")
					if len(lines) >= 100 {
						return streamBatchMsg{agent: agent, lines: lines, lineTypes: lineTypes}
					}
				default:
					if len(lines) > 0 {
						return streamBatchMsg{agent: agent, lines: lines, lineTypes: lineTypes}
					}
					return nil
				}
			}
		}
	}
}

func (a *App) sendInput(agent, text string) tea.Cmd {
	client := a.client
	repoName := a.repoName

	return func() tea.Msg {
		resp, err := client.Send(socket.Request{
			Command: "send_agent_input",
			Args: map[string]interface{}{
				"repo":    repoName,
				"agent":   agent,
				"message": text,
			},
		})
		if err != nil {
			return sendResultMsg{err: err}
		}
		if !resp.Success {
			return sendResultMsg{err: fmt.Errorf("%s", resp.Error)}
		}
		return sendResultMsg{}
	}
}

func (a *App) interruptAgent(agent string) tea.Cmd {
	client := a.client
	repoName := a.repoName

	return func() tea.Msg {
		_, err := client.Send(socket.Request{
			Command: "escape_agent",
			Args: map[string]interface{}{
				"repo":  repoName,
				"agent": agent,
			},
		})
		if err != nil {
			return sendResultMsg{err: err}
		}
		return sendResultMsg{}
	}
}

// --- Rendering helpers ---

// renderContentForViewport formats all accumulated lines through the renderer.
// Braille spinner frames for the thinking indicator.
var thinkingSpinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func (a *App) renderContentForViewport(agent string) string {
	lines, ok := a.outputContent[agent]
	if !ok || len(lines) == 0 {
		msg := fmt.Sprintf("\n  %s  %s\n\n  Waiting for output from %s...\n",
			styleBrandTag.Render("OAT"),
			styleStatusRepo.Render(a.repoName),
			agent)
		if logPath := a.getAgentLogPath(agent); logPath != "" {
			msg += fmt.Sprintf("\n  Log: %s", logPath)
			if _, err := os.Stat(logPath); err != nil {
				msg += " (not yet created)"
			}
			msg += "\n  Press ctrl+o to open full log in pager"
		}
		if mode, ok := a.streamMode[agent]; ok {
			msg += fmt.Sprintf("\n  Stream: %s", mode)
		}
		return msg
	}

	rendered := a.renderer.RenderLines(agent, lines, a.outputTypes[agent])

	// Append thinking indicator when agent is alive but quiet for 3+ seconds.
	// This prevents the "is it dead?" confusion when the agent is mid-tool-execution.
	if indicator := a.thinkingIndicator(agent); indicator != "" {
		rendered += "\n" + indicator
	}

	return rendered
}

// thinkingIndicator returns a subtle "processing..." line when the agent is alive
// and actively working but hasn't produced output recently. Returns empty string
// if the agent is dead, waiting, or has been idle too long.
func (a *App) thinkingIndicator(agent string) string {
	// Only show for alive, non-waiting agents
	var agentInfo *AgentInfo
	for i := range a.agents {
		if a.agents[i].Name == agent && a.agents[i].Alive {
			agentInfo = &a.agents[i]
			break
		}
	}
	if agentInfo == nil || agentInfo.Waiting {
		return ""
	}

	// When the sidecar has told us the turn ended, suppress the
	// indicator immediately regardless of elapsed time. This is the
	// precise signal; elapsed-time was a best-effort proxy from the
	// PTY era and left the indicator stuck for up to 2 minutes after
	// an agent finished responding.
	//
	// If chatFromEvents is set we have authoritative turn signals for
	// this agent — trust turnInFlight entirely. If not (agent has no
	// active sidecar or hasn't emitted yet), fall through to the
	// elapsed-time heuristic so we don't regress non-sidecar paths.
	if a.chatFromEvents[agent] {
		if !a.turnInFlight[agent] {
			return ""
		}
		// Turn is in-flight — still want a small grace period before
		// showing the spinner so one-line responses don't flash it.
		lastOutput, hasOutput := a.lastOutputTime[agent]
		if !hasOutput {
			return ""
		}
		elapsed := time.Since(lastOutput)
		if elapsed < 3*time.Second {
			return ""
		}
		frame := thinkingSpinner[(int(elapsed.Milliseconds()/200))%len(thinkingSpinner)]
		elapsedStr := fmt.Sprintf("%ds", int(elapsed.Seconds()))
		return fmt.Sprintf("\n  %s  %s",
			styleThinkingLine.Render(frame+" processing..."),
			styleToolOutputLine.Render("("+elapsedStr+" since last output)"))
	}

	lastOutput, hasOutput := a.lastOutputTime[agent]
	if !hasOutput {
		return ""
	}

	elapsed := time.Since(lastOutput)
	if elapsed < 3*time.Second {
		return "" // recently active, no indicator needed
	}
	if elapsed > 120*time.Second {
		return "" // idle too long — agent is likely waiting for input, not processing
	}

	// Pick spinner frame based on elapsed time (rotates every 200ms)
	frame := thinkingSpinner[(int(elapsed.Milliseconds()/200))%len(thinkingSpinner)]
	elapsedStr := fmt.Sprintf("%ds", int(elapsed.Seconds()))

	return fmt.Sprintf("\n  %s  %s",
		styleThinkingLine.Render(frame+" processing..."),
		styleToolOutputLine.Render("("+elapsedStr+" since last output)"))
}

// updateViewport refreshes the viewport with rendered content for the active agent.
func (a *App) updateViewport() {
	a.viewport.SetContent(a.renderContentForViewport(a.activeAgent))
	a.syncViewportWithAutoScroll(a.activeAgent)
}

// --- Helpers ---

func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func getBool(m map[string]interface{}, key string) bool {
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

func getInt64(m map[string]interface{}, key string) int64 {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case float64:
			return int64(n)
		case int64:
			return n
		case int:
			return int64(n)
		}
	}
	return 0
}

func (a *App) pruneRecentInputs(now time.Time) {
	if len(a.recentInputs) == 0 {
		return
	}
	filtered := a.recentInputs[:0]
	for _, input := range a.recentInputs {
		if now.Sub(input.sentAt) <= recentInputMaxAge {
			filtered = append(filtered, input)
		}
	}
	a.recentInputs = filtered
}

func normalizeEchoCandidate(line string) string {
	trimmed := strings.TrimSpace(line)
	for _, prefix := range []string{"> ", "› ", "❯ ", ">> "} {
		if strings.HasPrefix(trimmed, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, prefix))
		}
	}
	return trimmed
}

func isLikelyEchoFragment(line, matchedInput string, recentInputs []recentInput, _ time.Time) bool {
	normalized := normalizeEchoCandidate(line)
	if normalized == "" {
		return false
	}

	runes := []rune(normalized)
	if len(runes) == 0 || len(runes) > 2 {
		return false
	}

	for _, r := range runes {
		if !unicode.IsLetter(r) {
			return false
		}
	}

	// If we matched a full echo in this batch, ANY short letter-only fragment
	// is PTY noise — suppress it regardless of content. The echo detection
	// already established that this batch contains PTY artifacts.
	if matchedInput != "" {
		return true
	}

	// Check against all recent inputs. The recentInputs list is already pruned
	// to entries within recentInputMaxAge, so no additional time check is needed.
	// Both 1-char and 2-char letter-only fragments are suppressed — these are
	// PTY keystroke echoes that may arrive in a different batch than the full echo.
	for _, input := range recentInputs {
		if strings.Contains(input.text, normalized) {
			return true
		}
	}

	return false
}

func getInt(m map[string]interface{}, key string) int {
	return int(getInt64(m, key))
}
