package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// maxActivityEntries is the maximum number of entries kept in the activity log.
const maxActivityEntries = 100

// ActivityEntry represents a single activity event from an agent.
type ActivityEntry struct {
	Timestamp time.Time
	Agent     string
	Action    string
}

// agentColorPalette maps agent names to colors for consistent visual identity.
// Entries are AdaptiveColors so the palette stays readable on light terminals.
var agentColorPalette = []lipgloss.TerminalColor{
	lipgloss.AdaptiveColor{Dark: "#06B6D4", Light: "#0891B2"}, // cyan
	lipgloss.AdaptiveColor{Dark: "#A78BFA", Light: "#6D28D9"}, // purple
	lipgloss.AdaptiveColor{Dark: "#34D399", Light: "#047857"}, // emerald
	lipgloss.AdaptiveColor{Dark: "#FBBF24", Light: "#B45309"}, // amber
	lipgloss.AdaptiveColor{Dark: "#F472B6", Light: "#BE185D"}, // pink
	lipgloss.AdaptiveColor{Dark: "#60A5FA", Light: "#1D4ED8"}, // blue
	lipgloss.AdaptiveColor{Dark: "#FB923C", Light: "#C2410C"}, // orange
	lipgloss.AdaptiveColor{Dark: "#4ADE80", Light: "#15803D"}, // green
}

// colorForAgent returns a consistent color for an agent name.
func colorForAgent(name string) lipgloss.TerminalColor {
	hash := 0
	for _, c := range name {
		hash = hash*31 + int(c)
	}
	if hash < 0 {
		hash = -hash
	}
	return agentColorPalette[hash%len(agentColorPalette)]
}

// detectActivity scans new output lines for an agent and extracts activity events.
// It looks for tool calls, commits, file reads/writes, and other notable actions.
func detectActivity(agent string, lines []string) []ActivityEntry {
	var entries []ActivityEntry
	now := time.Now()

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		var action string

		// Tool calls: (*) tool_name(args) or ⏺ tool_name(args)
		if strings.HasPrefix(trimmed, "(*) ") {
			action = summarizeToolCall(trimmed[4:])
		} else if strings.HasPrefix(trimmed, "⏺ ") {
			action = summarizeToolCall(strings.TrimPrefix(trimmed, "⏺ "))
		} else if strings.HasPrefix(trimmed, "● ") {
			action = summarizeToolCall(strings.TrimPrefix(trimmed, "● "))
		}

		// Git commits
		if action == "" && strings.Contains(trimmed, "Committed") {
			action = trimmed
			if len(action) > 60 {
				action = action[:57] + "..."
			}
		}

		// PR creation
		if action == "" && strings.Contains(trimmed, "https://github.com") && strings.Contains(trimmed, "/pull/") {
			action = "Created pull request"
		}

		// System/daemon messages
		if action == "" && strings.HasPrefix(trimmed, "[daemon]") {
			action = strings.TrimPrefix(trimmed, "[daemon] ")
			if len(action) > 60 {
				action = action[:57] + "..."
			}
		}

		if action != "" {
			entries = append(entries, ActivityEntry{
				Timestamp: now,
				Agent:     agent,
				Action:    action,
			})
		}
	}
	return entries
}

// summarizeToolCall extracts a short description from a tool call line.
func summarizeToolCall(call string) string {
	// Extract tool name and simplify
	paren := strings.Index(call, "(")
	if paren < 0 {
		if len(call) > 50 {
			return call[:47] + "..."
		}
		return call
	}

	toolName := call[:paren]
	args := call[paren:]

	switch {
	case strings.Contains(toolName, "execute") || strings.Contains(toolName, "bash"):
		// Extract the command being run
		if cmd := extractQuotedArg(args); cmd != "" {
			if len(cmd) > 45 {
				cmd = cmd[:42] + "..."
			}
			return fmt.Sprintf("Running: %s", cmd)
		}
		return "Running command"
	case strings.Contains(toolName, "read_file") || strings.Contains(toolName, "Read"):
		if path := extractFirstArg(args); path != "" {
			return fmt.Sprintf("Reading %s", path)
		}
		return "Reading file"
	case strings.Contains(toolName, "write_file") || strings.Contains(toolName, "Write"):
		if path := extractFirstArg(args); path != "" {
			return fmt.Sprintf("Writing %s", path)
		}
		return "Writing file"
	case strings.Contains(toolName, "edit") || strings.Contains(toolName, "Edit"):
		if path := extractFirstArg(args); path != "" {
			return fmt.Sprintf("Editing %s", path)
		}
		return "Editing file"
	case strings.Contains(toolName, "ls") || strings.Contains(toolName, "Glob"):
		return "Listing files"
	case strings.Contains(toolName, "grep") || strings.Contains(toolName, "Grep"):
		return "Searching code"
	case strings.Contains(toolName, "Task"):
		return "Managing tasks"
	default:
		summary := toolName
		if len(summary) > 50 {
			summary = summary[:47] + "..."
		}
		return summary
	}
}

// extractQuotedArg extracts the first quoted string from args like ("git status").
func extractQuotedArg(args string) string {
	for _, q := range []byte{'"', '\''} {
		start := strings.IndexByte(args, q)
		if start >= 0 {
			end := strings.IndexByte(args[start+1:], q)
			if end >= 0 {
				return args[start+1 : start+1+end]
			}
		}
	}
	return ""
}

// extractFirstArg extracts the first argument from args like (path/to/file).
// Always returns just the filename for paths to keep activity entries short.
func extractFirstArg(args string) string {
	// Strip parens
	args = strings.TrimPrefix(args, "(")
	args = strings.TrimSuffix(args, ")")
	// Take first comma-separated or space-separated arg
	for _, sep := range []string{",", " "} {
		if idx := strings.Index(args, sep); idx > 0 {
			args = args[:idx]
			break
		}
	}
	args = strings.Trim(args, "\"' ")
	// Always show just the filename for paths
	if slash := strings.LastIndex(args, "/"); slash >= 0 {
		return args[slash+1:]
	}
	if len(args) > 30 {
		return args[:27] + "..."
	}
	return args
}

// abbreviateAgentName shortens agent names to fit narrow panels.
// "bright-lynx" → "b-lynx", "supervisor" → "super.", "merge-queue" → "m-queue"
// maxLen is in display characters (runes), not bytes.
func abbreviateAgentName(name string, maxLen int) string {
	runes := []rune(name)
	if len(runes) <= maxLen {
		return name
	}
	// Try abbreviating at the hyphen: "bright-lynx" → "b-lynx"
	if idx := strings.Index(name, "-"); idx > 0 && idx < len(name)-1 {
		short := string(name[0]) + name[idx:]
		if len([]rune(short)) <= maxLen {
			return short
		}
	}
	// Fallback: truncate with dot (1 byte, not multi-byte ellipsis)
	if maxLen > 2 {
		return string(runes[:maxLen-1]) + "."
	}
	return string(runes[:maxLen])
}

// renderActivityLog renders the activity panel for the right side of the TUI.
func renderActivityLog(entries []ActivityEntry, width, height int) string {
	var b strings.Builder

	title := lipgloss.NewStyle().
		Foreground(colorPrimary).
		Background(colorBgPanel).
		Bold(true).
		Width(width).
		Render(" Activity")
	b.WriteString(title)
	b.WriteString("\n")

	divider := lipgloss.NewStyle().
		Foreground(colorBorder).
		Background(colorBgPanel).
		Width(width).
		Render(" " + strings.Repeat("─", width-2))
	b.WriteString(divider)
	b.WriteString("\n")

	// Hard limit: the total output must be EXACTLY `height` lines.
	// Title (1) + divider (1) = 2 lines of chrome, leaving the rest for entries.
	availLines := height - 2
	if availLines < 1 {
		availLines = 1
	}

	// Content width is panel width minus 1 (indent)
	contentWidth := width - 1
	if contentWidth < 10 {
		contentWidth = 10
	}

	// Show most recent entries, one per line
	start := len(entries) - availLines
	if start < 0 {
		start = 0
	}
	visible := entries[start:]

	linesRendered := 0
	nameLimit := 6
	if contentWidth >= 24 {
		nameLimit = 8
	}
	if contentWidth >= 30 {
		nameLimit = 10
	}

	for i, entry := range visible {
		if linesRendered >= availLines {
			break // hard stop — don't exceed panel height
		}

		agentColor := colorForAgent(entry.Agent)
		shortName := abbreviateAgentName(entry.Agent, nameLimit)
		nameFieldWidth := len([]rune(shortName))
		showAgentName := i == 0 || visible[i-1].Agent != entry.Agent

		// Truncate action to guarantee ONE line, no wrapping
		maxAction := contentWidth - nameFieldWidth - 2 // " name action"
		if maxAction < 3 {
			maxAction = 3
		}
		action := compactActivityAction(entry.Action)
		runes := []rune(action)
		if len(runes) > maxAction {
			if maxAction > 2 {
				action = string(runes[:maxAction-2]) + ".."
			} else {
				action = string(runes[:maxAction])
			}
		}

		nameDisplay := strings.Repeat(" ", nameFieldWidth)
		if showAgentName {
			nameDisplay = lipgloss.NewStyle().Foreground(agentColor).Bold(true).Render(shortName)
		}
		b.WriteString(" " + nameDisplay + " " + action + "\n")
		linesRendered++
	}

	// Pad to exactly `height` lines total (2 chrome + linesRendered + padding)
	totalLines := 2 + linesRendered
	for i := totalLines; i < height; i++ {
		b.WriteString("\n")
	}

	// Constrain to exact dimensions — prevent overflow in either direction
	return lipgloss.NewStyle().
		Width(width).
		Height(height).
		MaxHeight(height).
		Background(colorBgPanel).
		Foreground(colorText).
		Render(b.String())
}

func compactActivityAction(action string) string {
	if strings.HasPrefix(action, "Running: ") {
		return strings.TrimPrefix(action, "Running: ")
	}
	return action
}
