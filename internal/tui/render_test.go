package tui

import (
	"strings"
	"testing"
)

// helper to render a single line through the renderer
func renderOne(r *LineRenderer, line string) string {
	return r.RenderLines("test", []string{line})
}

func TestRender_Headings(t *testing.T) {
	r := NewLineRenderer(NewOutputFilter(DefaultFilterConfig()), 80)

	result := renderOne(r, "# What's Done")
	if !strings.Contains(result, "WHAT'S DONE") {
		t.Errorf("H1 should be uppercased, got: %q", result)
	}

	r.InvalidateCache()
	result = renderOne(r, "## Current State")
	if !strings.Contains(result, "Current State") {
		t.Errorf("H2 should contain text, got: %q", result)
	}
}

func TestRender_BulletPoints(t *testing.T) {
	r := NewLineRenderer(NewOutputFilter(DefaultFilterConfig()), 80)
	result := renderOne(r, "- Core storage layer")
	if !strings.Contains(result, "*") || !strings.Contains(result, "Core storage layer") {
		t.Errorf("Bullet should contain marker and text, got: %q", result)
	}
}

func TestRender_Bold(t *testing.T) {
	r := NewLineRenderer(NewOutputFilter(DefaultFilterConfig()), 80)
	result := renderOne(r, "This is **important** text")
	if strings.Contains(result, "**") {
		t.Errorf("Bold markers should be stripped, got: %q", result)
	}
	if !strings.Contains(result, "important") {
		t.Errorf("Bold text should be preserved, got: %q", result)
	}
}

func TestRender_InlineCode(t *testing.T) {
	r := NewLineRenderer(NewOutputFilter(DefaultFilterConfig()), 80)
	result := renderOne(r, "Run `oat ui` to start")
	if strings.Contains(result, "`") {
		t.Errorf("Code backticks should be stripped, got: %q", result)
	}
	if !strings.Contains(result, "oat ui") {
		t.Errorf("Code text should be preserved, got: %q", result)
	}
}

func TestRender_Links(t *testing.T) {
	r := NewLineRenderer(NewOutputFilter(DefaultFilterConfig()), 80)
	result := renderOne(r, "[#21](https://github.com/org/repo/pull/21) CI fix")
	if strings.Contains(result, "https://") {
		t.Errorf("URL should be stripped from links, got: %q", result)
	}
	if !strings.Contains(result, "#21") {
		t.Errorf("Link text should be preserved, got: %q", result)
	}
}

func TestRender_ToolCall(t *testing.T) {
	r := NewLineRenderer(NewOutputFilter(DefaultFilterConfig()), 80)
	result := renderOne(r, `(*) execute("git status")`)
	if !strings.Contains(result, "execute") {
		t.Errorf("Tool call should preserve command, got: %q", result)
	}
}

func TestRender_WordWrap(t *testing.T) {
	r := NewLineRenderer(NewOutputFilter(DefaultFilterConfig()), 40)
	longLine := "This is a very long line that should be wrapped at the terminal width boundary to avoid truncation"
	result := renderOne(r, longLine)
	lines := strings.Split(result, "\n")
	if len(lines) < 2 {
		t.Errorf("Long line should be wrapped into multiple lines at width 40, got %d lines", len(lines))
	}
}

func TestRender_CodeBlockClampedToWidth(t *testing.T) {
	r := NewLineRenderer(NewOutputFilter(DefaultFilterConfig()), 20)
	result := r.RenderLines("test", []string{
		"```go",
		"abcdefghijklmnopqrstuvwxyz",
		"```",
	})
	// Code blocks are clamped to viewport width to prevent overflow.
	// "  " prefix + 26 chars = 28, clamped to 20.
	for _, line := range strings.Split(result, "\n") {
		runeLen := len([]rune(line))
		if runeLen > 20 {
			t.Fatalf("code block line exceeds width 20: %d runes: %q", runeLen, line)
		}
	}
}

func TestRender_BlankLineStaysBlank(t *testing.T) {
	r := NewLineRenderer(NewOutputFilter(DefaultFilterConfig()), 40)
	result := renderOne(r, "")
	if result != "" {
		t.Fatalf("expected blank line to remain blank, got: %q", result)
	}
}

func TestRender_TableRowAlignment(t *testing.T) {
	r := NewLineRenderer(NewOutputFilter(DefaultFilterConfig()), 80)

	// Render two table rows and verify columns align
	r.InvalidateCache()
	row1 := renderOne(r, "| Worker | Issue | Task |")
	r.InvalidateCache()
	row2 := renderOne(r, "| silver-seahorse | #55 | Wire main.py |")

	// Both rows should have the same pipe positions (aligned columns)
	pipePositions := func(s string) []int {
		var positions []int
		for i, c := range s {
			if c == '|' {
				positions = append(positions, i)
			}
		}
		return positions
	}
	pos1 := pipePositions(row1)
	pos2 := pipePositions(row2)

	if len(pos1) != len(pos2) {
		t.Errorf("pipe count mismatch: row1 has %d, row2 has %d\nrow1: %q\nrow2: %q",
			len(pos1), len(pos2), row1, row2)
	} else {
		for i := range pos1 {
			if pos1[i] != pos2[i] {
				t.Errorf("pipe position %d misaligned: row1=%d, row2=%d\nrow1: %q\nrow2: %q",
					i, pos1[i], pos2[i], row1, row2)
				break
			}
		}
	}
}

func TestRender_TableWithoutTrailingPipe(t *testing.T) {
	r := NewLineRenderer(NewOutputFilter(DefaultFilterConfig()), 80)

	// Table row without trailing pipe should still be recognized
	result := renderOne(r, "| Worker | Issue | Task")
	// Should NOT contain leading pipe (table was parsed, not rendered as-is)
	if strings.HasPrefix(strings.TrimSpace(result), "|") {
		t.Errorf("table row without trailing pipe should be parsed, got: %q", result)
	}
	if !strings.Contains(result, "Worker") || !strings.Contains(result, "Task") {
		t.Errorf("table cells should be preserved, got: %q", result)
	}
}

func TestRender_TableCellTruncation(t *testing.T) {
	r := NewLineRenderer(NewOutputFilter(DefaultFilterConfig()), 60)

	// Very long cell should be truncated, not wrap
	result := renderOne(r, "| Name | This is an extremely long cell value that should be truncated to fit within the column width |")
	lines := strings.Split(result, "\n")
	if len(lines) > 1 {
		t.Errorf("table row should not wrap (got %d lines): %q", len(lines), result)
	}
	if !strings.Contains(result, "...") {
		t.Errorf("long cell should be truncated with ..., got: %q", result)
	}
}

func TestRender_TableSeparatorSkipped(t *testing.T) {
	r := NewLineRenderer(NewOutputFilter(DefaultFilterConfig()), 80)
	result := renderOne(r, "|---|---|---|")
	if strings.TrimSpace(result) != "" {
		t.Errorf("table separator should be empty, got: %q", result)
	}
}

func TestRender_CachePerformance(t *testing.T) {
	r := NewLineRenderer(NewOutputFilter(DefaultFilterConfig()), 80)

	lines := []string{"line one", "line two", "line three"}
	r.RenderLines("agent1", lines)

	// Adding one more line should only render the new one
	lines = append(lines, "line four")
	result := r.RenderLines("agent1", lines)

	if !strings.Contains(result, "line four") {
		t.Errorf("Should render new lines, got: %q", result)
	}
}
