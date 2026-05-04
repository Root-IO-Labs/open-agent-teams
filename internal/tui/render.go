package tui

import (
	"regexp"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Line rendering styles for different content categories.
var (
	styleToolCallLine = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#06B6D4")). // cyan
				Bold(true)

	styleToolOutputLine = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#D1D5DB")) // brighter gray for dark terminals

	styleThinkingLine = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#A78BFA")). // lighter purple
				Italic(true)

	styleSystemLine = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FBBF24")) // amber

	styleUserInputLine = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#34D399")). // emerald
				Bold(true)

	styleHeading = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#E5E7EB")).
			Bold(true).
			Underline(true)

	styleBold = lipgloss.NewStyle().
			Bold(true)

	styleCode = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#A78BFA")) // light purple for inline code

	// Code block lines get a dim background and monospace-friendly color
	styleCodeBlock = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#D1D5DB")). // light gray text
			Background(lipgloss.Color("#1F2937"))  // dark background

	// Code block fence line (``` or ```python)
	styleCodeFence = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#6B7280")) // muted fence markers

	styleBullet = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#06B6D4")) // cyan bullet

	styleCheckDone = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#22C55E")) // green check

	// Agent text gets a soft warm white — distinct from raw terminal default,
	// giving text responses a consistent, readable look alongside colored categories.
	styleAgentText = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#E2E8F0")) // slate-200: warm off-white

	// Divider between user input and agent response
	styleDivider = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#374151")) // border gray

	// Blockquote lines (> text) — left bar + dimmed text
	styleBlockquote = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#9CA3AF")) // gray-400

	styleBlockquoteBar = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#4B5563")) // gray-600

	// Horizontal rule (--- or ***)
	styleHRule = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#374151")) // border gray

	// Strikethrough
	styleStrikethrough = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#6B7280")). // muted
				Strikethrough(true)
)

// Regex for inline markdown patterns.
var (
	reBold          = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reInlineCode    = regexp.MustCompile("`([^`]+)`")
	reHeading       = regexp.MustCompile(`^(#{1,3})\s+(.+)`)
	reNumbered      = regexp.MustCompile(`^\s*(\d+)\.\s+(.+)`)
	reLink          = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`)
	reTableSep      = regexp.MustCompile(`^[\s|:-]+$`)
	reCodeFence     = regexp.MustCompile("^\\s*```")
	reHRule         = regexp.MustCompile(`^[-*_]{3,}\s*$`)
	reStrikethrough = regexp.MustCompile(`~~(.+?)~~`)
	reBlockquote    = regexp.MustCompile(`^>\s?(.*)`)
)

// LineRenderer formats agent output lines for display in the TUI.
// It caches rendered output so only new lines need rendering.
type LineRenderer struct {
	filter    *OutputFilter
	width     int
	wrapStyle lipgloss.Style // cached style for word-wrapping (avoids per-line alloc)

	// Render cache: maps agent name → per-agent cache
	cache map[string]*renderCache

	// Code block state: tracks whether we're inside a fenced code block.
	// Per-agent since different agents may be at different states.
	inCodeBlock map[string]bool
}

type renderCache struct {
	rawCount       int      // how many raw lines were last rendered
	rendered       []string // cached rendered lines
	codeBlockAfter []bool   // inCodeBlock state after rendering each line
	cachedJoin     string   // cached result of joining rendered lines
	cachedCount    int      // how many rendered lines are in cachedJoin
}

// NewLineRenderer creates a renderer with the given terminal width.
func NewLineRenderer(filter *OutputFilter, width int) *LineRenderer {
	return &LineRenderer{
		filter:      filter,
		width:       width,
		wrapStyle:   lipgloss.NewStyle().Width(width),
		cache:       make(map[string]*renderCache),
		inCodeBlock: make(map[string]bool),
	}
}

// RenderLines renders lines for an agent, using cache for already-rendered content.
// lineTypes is optional — when provided (from SocketStream), it gives authoritative
// classification so the renderer doesn't need regex guessing. Pass nil to use regex.
func (r *LineRenderer) RenderLines(agent string, lines []string, lineTypes ...[]string) string {
	var types []string
	if len(lineTypes) > 0 {
		types = lineTypes[0]
	}
	return r.renderLinesImpl(agent, lines, types)
}

func (r *LineRenderer) renderLinesImpl(agent string, lines []string, lineTypes []string) string {
	if len(lines) == 0 {
		return ""
	}

	c, ok := r.cache[agent]
	if !ok {
		c = &renderCache{}
		r.cache[agent] = c
	}

	// If the line count decreased (buffer was trimmed or rebuilt), invalidate cache
	if len(lines) < c.rawCount {
		c.rawCount = 0
		c.rendered = nil
		c.codeBlockAfter = nil
		c.cachedJoin = ""
		c.cachedCount = 0
		r.inCodeBlock[agent] = false
	}

	// Render only new lines
	if len(lines) > c.rawCount {
		newLines := lines[c.rawCount:]
		for i, line := range newLines {
			lt := ""
			idx := c.rawCount + i
			if idx < len(lineTypes) {
				lt = lineTypes[idx]
			}
			c.rendered = append(c.rendered, r.renderLine(agent, line, lt))
			c.codeBlockAfter = append(c.codeBlockAfter, r.inCodeBlock[agent])
		}
		c.rawCount = len(lines)
	}

	// Trim rendered cache if it got too big (matches buffer trimming)
	if len(c.rendered) > 5000 {
		c.rendered = c.rendered[len(c.rendered)-4000:]
		c.codeBlockAfter = c.codeBlockAfter[len(c.codeBlockAfter)-4000:]
		c.rawCount = len(lines)
		c.cachedJoin = ""
		c.cachedCount = 0
	}

	// Incrementally build the joined string — only join newly rendered lines
	if len(c.rendered) > c.cachedCount {
		newPart := strings.Join(c.rendered[c.cachedCount:], "\n")
		if c.cachedCount > 0 {
			c.cachedJoin = c.cachedJoin + "\n" + newPart
		} else {
			c.cachedJoin = newPart
		}
		c.cachedCount = len(c.rendered)
	}

	return c.cachedJoin
}

// InvalidateCache clears all cached renders (e.g., on width change or filter toggle).
func (r *LineRenderer) InvalidateCache() {
	r.cache = make(map[string]*renderCache)
	r.inCodeBlock = make(map[string]bool)
}

// InvalidateCacheFromIndex re-renders lines from the given index forward
// for a single agent. This is the surgical path used when dedup does an
// in-place replacement — only the changed line and everything after it
// needs re-rendering, not the entire buffer.
func (r *LineRenderer) InvalidateCacheFromIndex(agent string, fromIdx int) {
	c, ok := r.cache[agent]
	if !ok || fromIdx >= c.rawCount {
		return // nothing cached past this point anyway
	}
	// Restore code block state to what it was after rendering line fromIdx-1.
	// Without this, re-rendering from fromIdx with inCodeBlock=false would
	// invert code block detection for all subsequent content if fromIdx is
	// inside a code block.
	if fromIdx > 0 && fromIdx <= len(c.codeBlockAfter) {
		r.inCodeBlock[agent] = c.codeBlockAfter[fromIdx-1]
	} else {
		r.inCodeBlock[agent] = false
	}
	// Truncate rendered lines and code block state to the replacement point
	if fromIdx < len(c.rendered) {
		c.rendered = c.rendered[:fromIdx]
	}
	if fromIdx < len(c.codeBlockAfter) {
		c.codeBlockAfter = c.codeBlockAfter[:fromIdx]
	}
	// Reset the join cache — we'll need to rebuild from fromIdx
	if fromIdx < c.cachedCount {
		// Rebuild the join from scratch up to fromIdx
		if fromIdx > 0 {
			c.cachedJoin = strings.Join(c.rendered[:fromIdx], "\n")
		} else {
			c.cachedJoin = ""
		}
		c.cachedCount = fromIdx
	}
	c.rawCount = fromIdx
}

// InvalidateCacheForAgent clears the cache for a single agent.
// Prefer InvalidateCacheFromIndex for dedup replacements.
func (r *LineRenderer) InvalidateCacheForAgent(agent string) {
	delete(r.cache, agent)
	delete(r.inCodeBlock, agent)
}

// lineTypeToCategory converts a daemon-provided line_type string to LineCategory.
// Returns -1 if the type is empty/unknown (caller should fall back to Classify).
func lineTypeToCategory(lt string) LineCategory {
	switch lt {
	case "tool_call":
		return CatToolCall
	case "tool_output":
		return CatToolOutput
	case "thinking":
		return CatThinking
	case "system":
		return CatSystem
	case "user_input":
		return CatUserInput
	case "text":
		return CatText
	default:
		return -1 // unknown — fall back to regex
	}
}

// renderLine formats a single line with category-based styling and markdown rendering.
// lineType is the daemon-provided classification (empty string = use regex fallback).
func (r *LineRenderer) renderLine(agent string, line string, lineType ...string) string {
	// Code block fence detection (``` or ```lang) — toggle state
	if reCodeFence.MatchString(line) {
		if r.inCodeBlock[agent] {
			// Closing fence
			r.inCodeBlock[agent] = false
			return styleCodeFence.Render("  " + strings.TrimSpace(line))
		}
		// Opening fence
		r.inCodeBlock[agent] = true
		return styleCodeFence.Render("  " + strings.TrimSpace(line))
	}

	// Inside a code block — render with code block styling, no markdown processing.
	// Wrap first, then style each rendered row so long code lines don't clip.
	if r.inCodeBlock[agent] {
		codeLine := "  " + line
		return renderStyledLines(styleCodeBlock, r.wrapLine(codeLine))
	}

	if r.filter == nil {
		return r.wrapLine(r.formatMarkdown(line))
	}

	// Use daemon-provided classification when available; fall back to regex.
	cat := LineCategory(-1)
	if len(lineType) > 0 && lineType[0] != "" {
		cat = lineTypeToCategory(lineType[0])
	}
	if cat < 0 {
		cat = r.filter.Classify(line)
	}
	switch cat {
	case CatText:
		return renderStyledLines(styleAgentText, r.wrapLine(r.formatMarkdown(line)))
	case CatToolCall:
		// Add a blank line before tool calls for visual breathing room
		return "\n" + renderStyledLines(styleToolCallLine, r.wrapLine(line))
	case CatToolOutput:
		return renderStyledLines(styleToolOutputLine, r.wrapLine(line))
	case CatThinking:
		return renderStyledLines(styleThinkingLine, r.wrapLine(line))
	case CatSystem:
		return renderStyledLines(styleSystemLine, r.wrapLine(line))
	case CatUserInput:
		// Render user input with a left accent bar and a thin divider below
		// to visually separate user→agent exchanges.
		rendered := renderStyledLines(styleUserInputLine, r.wrapLine(line))
		divWidth := r.width
		if divWidth <= 0 {
			divWidth = 60
		}
		if divWidth > 80 {
			divWidth = 80
		}
		rendered += "\n" + styleDivider.Render(strings.Repeat("─", divWidth))
		return rendered
	default:
		return r.wrapLine(line)
	}
}

// formatMarkdown handles basic markdown-to-terminal rendering for agent text output.
func (r *LineRenderer) formatMarkdown(line string) string {
	trimmed := strings.TrimSpace(line)

	if trimmed == "" {
		return ""
	}

	// Table separator lines (|---|---|) → skip entirely
	if reTableSep.MatchString(trimmed) {
		return ""
	}

	// Horizontal rules: ---, ***, ___
	if reHRule.MatchString(trimmed) {
		w := r.width
		if w <= 0 {
			w = 60
		}
		if w > 80 {
			w = 80
		}
		return styleHRule.Render(strings.Repeat("─", w))
	}

	// Blockquotes: > text
	if m := reBlockquote.FindStringSubmatch(line); m != nil {
		inner := r.inlineFormat(m[1])
		return styleBlockquoteBar.Render("  │ ") + styleBlockquote.Render(inner)
	}

	// Headings: # Title, ## Subtitle, ### Section
	if m := reHeading.FindStringSubmatch(line); m != nil {
		level := len(m[1])
		text := r.inlineFormat(m[2])
		switch level {
		case 1:
			return "\n" + styleHeading.Render(strings.ToUpper(text)) + "\n"
		case 2:
			return "\n" + styleHeading.Render(text)
		default:
			return styleBold.Render(text)
		}
	}

	// Checkbox items
	if strings.HasPrefix(trimmed, "- [x] ") || strings.HasPrefix(trimmed, "- [X] ") {
		return "  " + styleCheckDone.Render("[done]") + " " + r.inlineFormat(trimmed[6:])
	}
	if strings.HasPrefix(trimmed, "- [ ] ") {
		return "  " + styleBullet.Render("[ ]") + "  " + r.inlineFormat(trimmed[6:])
	}

	// Nested bullet points (2+ spaces before - or *)
	if (strings.HasPrefix(line, "  - ") || strings.HasPrefix(line, "  * ") ||
		strings.HasPrefix(line, "    - ") || strings.HasPrefix(line, "    * ")) &&
		len(trimmed) > 2 {
		return "    " + styleBullet.Render("◦") + " " + r.inlineFormat(trimmed[2:])
	}

	// Bullet points
	if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
		return "  " + styleBullet.Render("*") + " " + r.inlineFormat(trimmed[2:])
	}

	// Numbered lists
	if m := reNumbered.FindStringSubmatch(line); m != nil {
		return "  " + styleBullet.Render(m[1]+".") + " " + r.inlineFormat(m[2])
	}

	// Table rows: | col1 | col2 | or | col1 | col2 (trailing pipe optional)
	// Requires leading pipe and at least 2 total pipes to distinguish from
	// random lines starting with |. CatToolOutput lines are already handled
	// by the filter, so only text lines reach here.
	if strings.HasPrefix(trimmed, "|") && strings.Count(trimmed, "|") >= 2 {
		cells := parseTableCells(trimmed)
		if len(cells) > 0 {
			return r.formatTableRow(cells)
		}
	}

	return r.inlineFormat(line)
}

// parseTableCells extracts non-empty cell contents from a markdown table row.
func parseTableCells(row string) []string {
	parts := strings.Split(row, "|")
	var cells []string
	for _, p := range parts {
		c := strings.TrimSpace(p)
		if c != "" {
			cells = append(cells, c)
		}
	}
	return cells
}

// formatTableRow renders table cells with proportional column widths.
// Each column gets an equal share of the available width, with cells
// truncated and padded so columns align across rows.
func (r *LineRenderer) formatTableRow(cells []string) string {
	numCols := len(cells)
	indent := 2
	sepStr := " | "
	sepWidth := (numCols - 1) * len(sepStr)
	availWidth := r.width - indent - sepWidth
	if availWidth < numCols*4 {
		availWidth = numCols * 4 // minimum 4 chars per column
	}
	colWidth := availWidth / numCols

	var parts []string
	for _, cell := range cells {
		// Truncate raw text before applying formatting to avoid breaking ANSI
		runes := []rune(cell)
		if len(runes) > colWidth {
			if colWidth > 3 {
				cell = string(runes[:colWidth-3]) + "..."
			} else {
				cell = string(runes[:colWidth])
			}
		}
		// Apply inline formatting (bold, code, links)
		formatted := r.inlineFormat(cell)
		// Pad to column width using visual width (accounts for ANSI escape codes)
		visWidth := lipgloss.Width(formatted)
		if visWidth < colWidth {
			formatted += strings.Repeat(" ", colWidth-visWidth)
		}
		parts = append(parts, formatted)
	}

	return strings.Repeat(" ", indent) + strings.Join(parts, sepStr)
}

// inlineFormat applies bold, code, and link formatting within a line.
func (r *LineRenderer) inlineFormat(line string) string {
	// Convert [text](url) links → just text
	line = reLink.ReplaceAllString(line, "$1")

	// Replace **bold** with styled text (matched pairs)
	line = reBold.ReplaceAllStringFunc(line, func(match string) string {
		inner := match[2 : len(match)-2]
		return styleBold.Render(inner)
	})

	// Strip remaining unpaired ** markers
	if strings.Contains(line, "**") {
		line = strings.ReplaceAll(line, "**", "")
	}

	// Replace `code` with styled text
	line = reInlineCode.ReplaceAllStringFunc(line, func(match string) string {
		inner := match[1 : len(match)-1]
		return styleCode.Render(inner)
	})

	// Replace ~~strikethrough~~ with styled text
	line = reStrikethrough.ReplaceAllStringFunc(line, func(match string) string {
		inner := match[2 : len(match)-2]
		return styleStrikethrough.Render(inner)
	})

	return line
}

// wrapLine soft-wraps a line to fit within the terminal width.
func (r *LineRenderer) wrapLine(line string) string {
	if line == "" || r.width <= 0 {
		return line
	}
	return r.wrapStyle.Render(line)
}

func renderStyledLines(style lipgloss.Style, content string) string {
	if content == "" {
		return ""
	}

	lines := strings.Split(content, "\n")
	for i, line := range lines {
		lines[i] = style.Render(line)
	}
	return strings.Join(lines, "\n")
}
