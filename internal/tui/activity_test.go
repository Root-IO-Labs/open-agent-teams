package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestRenderActivityLog_DropsRunningPrefix(t *testing.T) {
	view := renderActivityLog([]ActivityEntry{
		{Agent: "merge-queue", Action: `Running: gh pr list --label oat`},
	}, 30, 6)

	plain := ansi.Strip(view)
	if strings.Contains(plain, "Running: ") {
		t.Fatalf("expected running prefix to be removed, got: %q", plain)
	}
	if !strings.Contains(plain, "gh pr list") {
		t.Fatalf("expected compact action text to remain visible, got: %q", plain)
	}
}

func TestRenderActivityLog_HidesRepeatedAgentNames(t *testing.T) {
	view := renderActivityLog([]ActivityEntry{
		{Agent: "merge-queue", Action: "First action"},
		{Agent: "merge-queue", Action: "Second action"},
	}, 30, 8)

	plain := ansi.Strip(view)
	lines := strings.Split(plain, "\n")
	shortName := abbreviateAgentName("merge-queue", 8)
	if len(lines) < 4 {
		t.Fatalf("expected activity panel with content, got: %q", plain)
	}

	if !strings.Contains(lines[2], shortName) {
		t.Fatalf("expected first activity row to include agent name, got: %q", lines[2])
	}
	if strings.Contains(lines[3], shortName) {
		t.Fatalf("expected repeated agent row to omit the repeated name, got: %q", lines[3])
	}
	if !strings.Contains(lines[3], "Second action") {
		t.Fatalf("expected repeated agent row to keep action text, got: %q", lines[3])
	}
}
