package tui

import (
	"regexp"
	"strings"
)

// LineCategory classifies a line of agent output for filtering.
type LineCategory int

const (
	CatText       LineCategory = iota // Agent's natural language response
	CatToolCall                       // Tool invocation (read_file, execute, etc.)
	CatToolOutput                     // Tool result / command output
	CatThinking                       // Thinking indicator or reasoning
	CatSystem                         // OAT system messages, daemon messages
	CatUserInput                      // User/daemon input echoed back
	CatProgress                       // Spinners, countdowns, progress bars
	CatChrome                         // TUI chrome: borders, scrollbars, banners
)

// String returns a human-readable name for the category.
func (c LineCategory) String() string {
	switch c {
	case CatText:
		return "text"
	case CatToolCall:
		return "tool_call"
	case CatToolOutput:
		return "tool_output"
	case CatThinking:
		return "thinking"
	case CatSystem:
		return "system"
	case CatUserInput:
		return "user_input"
	case CatProgress:
		return "progress"
	case CatChrome:
		return "chrome"
	default:
		return "unknown"
	}
}

// FilterConfig controls which categories of output are shown.
type FilterConfig struct {
	ShowText       bool `json:"show_text"`
	ShowToolCall   bool `json:"show_tool_call"`
	ShowToolOutput bool `json:"show_tool_output"`
	ShowThinking   bool `json:"show_thinking"`
	ShowSystem     bool `json:"show_system"`
	ShowUserInput  bool `json:"show_user_input"`
	ShowProgress   bool `json:"show_progress"`
	ShowChrome     bool `json:"show_chrome"`
}

// DefaultFilterConfig returns a filter configuration optimized for readability.
// Shows agent text, tool calls, tool output, thinking, system messages, and user input.
// Hides spinners/progress and TUI chrome.
func DefaultFilterConfig() FilterConfig {
	return FilterConfig{
		ShowText:       true,
		ShowToolCall:   true,
		ShowToolOutput: true,
		ShowThinking:   true,
		ShowSystem:     true,
		ShowUserInput:  true,
		ShowProgress:   false,
		ShowChrome:     false,
	}
}

// ShouldShow returns true if the given category is visible under this config.
func (fc FilterConfig) ShouldShow(cat LineCategory) bool {
	switch cat {
	case CatText:
		return fc.ShowText
	case CatToolCall:
		return fc.ShowToolCall
	case CatToolOutput:
		return fc.ShowToolOutput
	case CatThinking:
		return fc.ShowThinking
	case CatSystem:
		return fc.ShowSystem
	case CatUserInput:
		return fc.ShowUserInput
	case CatProgress:
		return fc.ShowProgress
	case CatChrome:
		return fc.ShowChrome
	default:
		return true
	}
}

// OutputFilter classifies and filters agent output lines.
type OutputFilter struct {
	Config FilterConfig
}

// NewOutputFilter creates a filter with the given configuration.
func NewOutputFilter(cfg FilterConfig) *OutputFilter {
	return &OutputFilter{Config: cfg}
}

// Classify determines the category of a single output line.
func (f *OutputFilter) Classify(line string) LineCategory {
	trimmed := strings.TrimSpace(line)

	// Empty lines pass through as text
	if trimmed == "" {
		return CatText
	}

	// --- Internal: structured token usage lines ---
	if strings.Contains(trimmed, "[OAT_TOKENS]") {
		return CatChrome // hidden by default — token data shown in status bar
	}

	// --- Progress: spinners and countdown timers ---
	if isSpinnerLine(trimmed) {
		return CatProgress
	}
	if isCountdownLine(trimmed) {
		return CatProgress
	}

	// --- TUI chrome: borders, scrollbars, banner ---
	if isChromeLine(trimmed) {
		return CatChrome
	}

	// --- Chrome: Textual sidebar panels and frame characters ---
	// The agent CLI's Textual TUI renders sidebar warnings/notifications
	// as lines starting with ▌ (LEFT HALF BLOCK) or containing box-drawing
	// frame characters (└─, │, ┌─, etc.). These are UI elements,
	// not agent output. Suppress them entirely.
	if strings.HasPrefix(trimmed, "▌") || strings.HasPrefix(trimmed, "│") ||
		strings.HasPrefix(trimmed, "└") || strings.HasPrefix(trimmed, "┌") ||
		strings.HasPrefix(trimmed, "┐") || strings.HasPrefix(trimmed, "┘") {
		return CatChrome
	}

	// --- Chrome: echoed system prompt content ---
	// The agent CLI echoes AGENTS.md content with "> " prefix during startup.
	// These look like user input but are actually prompt content.
	if strings.HasPrefix(trimmed, "> ##") || strings.HasPrefix(trimmed, "> ---") ||
		strings.HasPrefix(trimmed, "> ```") {
		return CatChrome
	}

	// --- Tool calls: (*) prefix, ⏺ prefix, or ● prefix ---
	if strings.HasPrefix(trimmed, "(*) ") || strings.HasPrefix(trimmed, "⏺ ") || strings.HasPrefix(trimmed, "● ") {
		return CatToolCall
	}

	// --- Tool output: ⎿ prefix, [Command ...] ---
	if strings.HasPrefix(trimmed, "⎿") {
		return CatToolOutput
	}
	if strings.HasPrefix(trimmed, "[Command ") {
		return CatToolOutput
	}
	// "… N more — click or Ctrl+E to expand" is an interactive element from
	// the agent's own TUI that doesn't work in OAT's TUI — hide it.
	if reMoreLines.MatchString(trimmed) {
		return CatChrome
	}
	// Indented tool output (lines starting with pipe-space from context display)
	if strings.HasPrefix(line, "  | ") || strings.HasPrefix(line, "  ▎ ") {
		// Check if the inner content is a "… N more" interactive element —
		// these don't work in OAT's TUI and should be hidden.
		inner := strings.TrimSpace(line)
		inner = strings.TrimPrefix(inner, "▎")
		inner = strings.TrimPrefix(inner, "|")
		inner = strings.TrimSpace(inner)
		if reMoreLines.MatchString(inner) {
			return CatChrome
		}
		return CatToolOutput
	}

	// --- Thinking indicators ---
	if strings.HasPrefix(trimmed, "Thinking...") || trimmed == "Thinking…" {
		return CatThinking
	}

	// --- System warnings and config suggestions ---
	if strings.Contains(trimmed, "is not installed;") ||
		strings.HasPrefix(trimmed, "To suppress, add to") ||
		strings.HasPrefix(trimmed, "suppress =") ||
		trimmed == "[warnings]" {
		return CatSystem
	}

	// --- System / daemon messages ---
	if strings.Contains(trimmed, "📨 Message from daemon:") ||
		strings.HasPrefix(trimmed, "[daemon]") ||
		strings.HasPrefix(trimmed, "> 📨") {
		return CatSystem
	}

	// --- Chrome: agent TUI placeholders (BEFORE user input check) ---
	// The agent's own TUI has "> Talk to workspace agent..." which looks like
	// user input but is actually chrome that should be hidden.
	if isAgentTUIChrome(trimmed) {
		return CatChrome
	}

	// --- Chrome: token counts, mode indicators ---
	if reTokenCount.MatchString(trimmed) {
		return CatChrome
	}
	if trimmed == "auto | shift+tab to cycle" || strings.HasPrefix(trimmed, "anthropic:") {
		return CatChrome
	}

	// --- Chrome: startup banner ---
	if isBannerLine(trimmed) {
		return CatChrome
	}

	// --- User input (echoed back) ---
	if strings.HasPrefix(trimmed, "> ") && !strings.HasPrefix(trimmed, "> 📨") {
		return CatUserInput
	}

	return CatText
}

// FilterLine returns true if the line should be displayed.
func (f *OutputFilter) FilterLine(line string) bool {
	cat := f.Classify(line)
	return f.Config.ShouldShow(cat)
}

// FilterLines filters a slice of lines, returning only those that should be shown.
func (f *OutputFilter) FilterLines(lines []string) []string {
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		if f.FilterLine(line) {
			result = append(result, line)
		}
	}
	return result
}

// --- Pattern matchers ---

// Braille spinner characters used by the agent runtime TUI.
var brailleSpinners = map[rune]bool{
	'⠋': true, '⠙': true, '⠹': true, '⠸': true,
	'⠼': true, '⠴': true, '⠦': true, '⠧': true,
	'⠇': true, '⠏': true,
}

// isSpinnerLine detects spinner frames. Only matches unambiguous patterns:
// braille characters (single-char) and parenthesized ASCII spinners like (-).
// Bare ASCII chars like "-" and "|" are NOT matched — they're too common in content.
func isSpinnerLine(s string) bool {
	runes := []rune(s)
	if len(runes) == 1 {
		if brailleSpinners[runes[0]] {
			return true
		}
	}
	// Parenthesized single-char spinners: (-), (\), (|), (/), (*)
	if len(s) == 3 && s[0] == '(' && s[2] == ')' {
		switch s[1] {
		case '-', '\\', '|', '/', '*':
			return true
		}
	}
	return false
}

// reCountdown matches "(Ns, esc to interrupt)" countdown lines.
var reCountdown = regexp.MustCompile(`^\(\d+s, esc to interrupt\)$`)

// reVersionOnly matches bare version strings like "v0.0.26" on a line by itself.
var reVersionOnly = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// isCountdownLine detects thinking countdown indicators.
func isCountdownLine(s string) bool {
	return reCountdown.MatchString(s)
}

// reTokenCount matches "N.NK tokens" or "NK tokens" lines.
var reTokenCount = regexp.MustCompile(`^\d+\.?\d*K? tokens$`)

// reMoreLines matches "… N more — click or Ctrl+E to expand"
var reMoreLines = regexp.MustCompile(`^… \d+ more`)

// Box-drawing characters used by Textual TUI for borders.
var boxChars = map[rune]bool{
	'┌': true, '┐': true, '└': true, '┘': true,
	'├': true, '┤': true, '┬': true, '┴': true,
	'┼': true, '─': true, '│': true,
	'╔': true, '╗': true, '╚': true, '╝': true,
	'═': true, '║': true,
}

// Scrollbar/block characters from Textual TUI.
var scrollbarChars = map[rune]bool{
	'▁': true, '▂': true, '▃': true, '▄': true,
	'▅': true, '▆': true, '▇': true, '█': true,
	'▎': true, '▌': true, '▐': true,
}

// isChromeLine detects TUI chrome: borders, scrollbars, empty box rows.
// A line is chrome if every non-space character is a box-drawing char,
// scrollbar/block char, or a mix of both (the OAT banner uses both).
func isChromeLine(s string) bool {
	runes := []rune(s)
	if len(runes) == 0 {
		return false
	}

	for _, r := range runes {
		if r == ' ' || r == '\t' {
			continue
		}
		if !boxChars[r] && !scrollbarChars[r] {
			return false
		}
	}
	return true
}

// isAgentTUIChrome detects elements from the agent's own TUI that leak through
// the log file: input prompts, placeholder text, model selectors, status words, etc.
func isAgentTUIChrome(s string) bool {
	// Bare pipe characters from agent TUI context blocks
	if s == "|" || s == "| >" || s == "| " || s == "|>" {
		return true
	}

	// Strip leading "> " to handle the agent's own prompted lines
	// e.g. "> Talk to workspace agent..." from the agent's input
	inner := s
	if strings.HasPrefix(inner, "> ") {
		inner = strings.TrimSpace(inner[2:])
	}

	// Agent input placeholders: "Talk to workspace agent...", "> Talk to supervisor agent..."
	if strings.HasPrefix(inner, "Talk to") && strings.HasSuffix(inner, "...") {
		return true
	}
	// Bare ">" prompt from agent TUI input
	if s == ">" || s == "> " {
		return true
	}
	// Bare status words from the agent's Textual TUI status bar that leak
	// through PTY capture. These are single-word status indicators that don't
	// carry meaningful content for the user.
	lower := strings.ToLower(inner)
	if lower == "okay" || lower == "ok" || lower == "ready" || lower == "idle" ||
		lower == "running" || lower == "thinking" || lower == "working" {
		return true
	}
	// Model selector / provider indicators
	if strings.HasPrefix(inner, "claude-") || strings.Contains(inner, "claude-sonnet") ||
		strings.Contains(inner, "claude-opus") || strings.Contains(inner, "claude-haiku") {
		return true
	}
	// "compact | shift+tab" and similar mode toggles
	if strings.Contains(inner, "shift+tab") {
		return true
	}
	// Cost display "Cost: $0.00"
	if strings.HasPrefix(inner, "Cost:") || strings.HasPrefix(inner, "cost:") {
		return true
	}
	// Session/context info from agent TUI
	if strings.HasPrefix(inner, "Context:") || strings.HasPrefix(inner, "Session:") {
		return true
	}
	return false
}

// isBannerLine detects the OAT ASCII art banner at startup.
func isBannerLine(s string) bool {
	// The banner uses specific patterns: ___  /_\ |_  _|  etc.
	// Also the box-char banner variant: ██████╗  █████╗
	if strings.Contains(s, "___") && (strings.Contains(s, "/ \\") || strings.Contains(s, "\\_\\")) {
		return true
	}
	if strings.Contains(s, "██") && (strings.Contains(s, "╗") || strings.Contains(s, "╚")) {
		return true
	}
	if strings.Contains(s, "by Root.io") || strings.Contains(s, "by [green]Root.io") {
		return true
	}
	if strings.Contains(s, "Open Agent Teams ready!") {
		return true
	}
	if strings.Contains(s, "Enter send") && strings.Contains(s, "Ctrl+J") {
		return true
	}
	if strings.HasPrefix(strings.TrimSpace(s), "Thread:") {
		return true
	}
	if strings.HasPrefix(strings.TrimSpace(s), "Starting with thread:") {
		return true
	}
	if strings.Contains(s, "v0.") && (strings.Contains(s, "(local)") || reVersionOnly.MatchString(strings.TrimSpace(s))) {
		return true
	}
	if strings.Contains(s, "<frozen runpy>") {
		return true
	}
	return false
}
