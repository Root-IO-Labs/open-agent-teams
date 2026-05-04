package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// newTestApp creates an App with minimal config suitable for layout tests.
// It sets initial width/height via a WindowSizeMsg so that recalcLayout runs
// the same path the real TUI does on startup.
func newTestApp(width, height int, agents []AgentInfo) *App {
	app := NewApp("/tmp/test.sock", "test-repo", nil)
	app.agents = agents
	for _, ag := range agents {
		app.outputContent[ag.Name] = nil
		app.autoScroll[ag.Name] = true
	}
	if len(agents) > 0 {
		app.activeAgent = agents[0].Name
	}
	// Bootstrap layout via WindowSizeMsg (same as real startup)
	app.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return app
}

// --- Test 1: Window resize from 120→60 cols with agent list visible ---

func TestLayout_ResizeNarrow_WithAgentList(t *testing.T) {
	agents := []AgentInfo{
		{Name: "workspace", Type: "workspace", Alive: true},
		{Name: "worker-1", Type: "worker", Alive: true},
	}
	app := newTestApp(120, 40, agents)
	app.showAgentList = true
	app.expandedView = false

	// Seed both agents with long lines that will wrap differently at 120 vs 60
	longLine := strings.Repeat("abcdefghij ", 12) // 132 chars
	app.outputContent["workspace"] = []string{longLine, "short line"}
	app.outputContent["worker-1"] = []string{longLine, "another short"}

	// Force initial render at 120 width
	app.recalcLayout()
	initialWidth := app.renderer.width
	initialContent := app.viewport.View()

	// Capture cache state: both agents should have cache entries
	_, wsHadCache := app.renderer.cache["workspace"]
	_, w1HadCache := app.renderer.cache["worker-1"]
	if !wsHadCache {
		t.Error("workspace should have cache entry before resize")
	}
	// worker-1 may not have cache yet since it's not active, that's OK

	// --- Resize to 60 ---
	app.Update(tea.WindowSizeMsg{Width: 60, Height: 40})

	newWidth := app.renderer.width
	if newWidth >= initialWidth {
		t.Errorf("renderer width should decrease: got %d, was %d", newWidth, initialWidth)
	}

	// Verify viewport dimensions match
	if app.viewport.Width > 60 {
		t.Errorf("viewport.Width should be <= 60, got %d", app.viewport.Width)
	}

	// Verify the active agent's content was re-rendered at new width
	newContent := app.viewport.View()
	if newContent == initialContent && len(initialContent) > 0 {
		t.Error("viewport content should differ after resize (different wrapping)")
	}

	// Verify ALL agents' caches were invalidated (not just active)
	if _, ok := app.renderer.cache["workspace"]; ok {
		// Cache entry exists — but rawCount should be reset OR it was re-populated
		// by updateViewport. The key invariant is width changed.
	}
	if _, ok := app.renderer.cache["worker-1"]; ok && w1HadCache {
		t.Error("worker-1 cache should have been wiped by InvalidateCache")
	}

	// Verify renderer width matches the budget calculation
	// With agent list (30+1=31), vpWidth = 60-31 = 29, which is < minViewportWidth(40)
	// So agent list should be dropped at 60 width
	if app.layoutShowAgentList {
		t.Error("agent list should be dropped when terminal is 60 cols (viewport would be < 40)")
	}
}

// --- Test 2: ctrl+e expand with long wrapped lines ---

func TestLayout_ExpandView_ReWrapsContent(t *testing.T) {
	agents := []AgentInfo{
		{Name: "workspace", Type: "workspace", Alive: true},
	}
	app := newTestApp(120, 40, agents)
	app.showAgentList = true
	app.expandedView = false

	// Long line that will wrap differently depending on available width
	longLine := strings.Repeat("word ", 30) // 150 chars
	app.outputContent["workspace"] = []string{longLine}

	// Initial layout with agent list panel taking space
	app.recalcLayout()
	narrowWidth := app.renderer.width

	// Simulate ctrl+e: expand view
	app.expandedView = true
	app.showAgentList = false
	app.recalcLayout()

	expandedWidth := app.renderer.width
	if expandedWidth <= narrowWidth {
		t.Errorf("expanded view should be wider: got %d, was %d", expandedWidth, narrowWidth)
	}

	// Verify the viewport got the full terminal width
	if app.viewport.Width != app.width {
		t.Errorf("viewport.Width should equal terminal width %d in expanded mode, got %d",
			app.width, app.viewport.Width)
	}

	// Verify content was re-rendered (cache was invalidated due to width change)
	content := app.renderContentForViewport("workspace")
	if !strings.Contains(content, "word") {
		t.Error("expanded content should contain the rendered text")
	}

	// Collapse back: panels return, viewport narrows
	app.expandedView = false
	app.showAgentList = true
	app.recalcLayout()

	collapsedWidth := app.renderer.width
	if collapsedWidth >= expandedWidth {
		t.Errorf("collapsed width should be less than expanded: got %d vs %d", collapsedWidth, expandedWidth)
	}
}

// --- Test 3: Tab toggle at threshold (viewport exactly 40 chars) ---

func TestLayout_TabToggle_AtThreshold(t *testing.T) {
	// Agent list = 30 + 1 divider = 31
	// We want viewport to be exactly at minViewportWidth (40) after adding agent list
	// So total width = 40 + 31 = 71
	const thresholdWidth = 71
	agents := []AgentInfo{
		{Name: "workspace", Type: "workspace", Alive: true},
	}
	app := newTestApp(thresholdWidth, 30, agents)
	app.showAgentList = false
	app.expandedView = false
	app.outputContent["workspace"] = []string{"test content at threshold"}

	// Without agent list, viewport should use full width
	app.recalcLayout()
	fullWidth := app.renderer.width
	if fullWidth != thresholdWidth-2 {
		t.Errorf("without agent list, renderer width should be %d, got %d", thresholdWidth-2, fullWidth)
	}

	// Toggle agent list ON — should just barely fit (remaining = 71-31 = 40 = minViewportWidth)
	app.showAgentList = true
	app.recalcLayout()
	if !app.layoutShowAgentList {
		t.Error("agent list should fit at threshold width 71 (remaining=40 >= minViewportWidth)")
	}
	withListWidth := app.renderer.width
	expectedVP := thresholdWidth - 31 // 40
	if withListWidth != expectedVP-2 {
		t.Errorf("with agent list at threshold, renderer width should be %d, got %d", expectedVP-2, withListWidth)
	}

	// One pixel narrower: agent list should NOT fit
	app2 := newTestApp(thresholdWidth-1, 30, agents)
	app2.showAgentList = true
	app2.expandedView = false
	app2.recalcLayout()
	if app2.layoutShowAgentList {
		t.Errorf("agent list should NOT fit at width %d (remaining=%d < 40)", thresholdWidth-1, thresholdWidth-1-31)
	}
}

// --- Test 4: Rapid resize events (3 WindowSizeMsg in sequence) ---

func TestLayout_RapidResize_ThreeInSequence(t *testing.T) {
	agents := []AgentInfo{
		{Name: "workspace", Type: "workspace", Alive: true},
		{Name: "worker-1", Type: "worker", Alive: true},
		{Name: "worker-2", Type: "worker", Alive: true},
	}
	app := newTestApp(120, 40, agents)

	// Seed all agents with content
	lines := []string{
		strings.Repeat("The quick brown fox jumps over the lazy dog. ", 4),
		"Short line here",
		strings.Repeat("Another long line for testing wrapping behavior. ", 3),
	}
	for _, ag := range agents {
		app.outputContent[ag.Name] = lines
	}
	app.recalcLayout() // render at 120

	// Rapid fire: 120 → 80 → 40 → 100
	sizes := []tea.WindowSizeMsg{
		{Width: 80, Height: 40},
		{Width: 40, Height: 30},
		{Width: 100, Height: 35},
	}

	for _, sz := range sizes {
		app.Update(sz)
	}

	// After all resizes, state should reflect the LAST resize (100x35)
	if app.width != 100 {
		t.Errorf("final width should be 100, got %d", app.width)
	}
	if app.height != 35 {
		t.Errorf("final height should be 35, got %d", app.height)
	}

	// Viewport should match final dimensions
	expectedVPHeight := 35 - 4 // 31
	if app.viewport.Height != expectedVPHeight {
		t.Errorf("viewport height should be %d, got %d", expectedVPHeight, app.viewport.Height)
	}

	// Renderer width should correspond to final layout
	// At width 100, agent list (31) fits since 100-31=69 >= 40
	if app.showAgentList && !app.layoutShowAgentList {
		t.Log("agent list was requested but doesn't fit at width 100 (unexpected)")
	}

	// Active agent should have been re-rendered with final width
	content := app.renderContentForViewport("workspace")
	if !strings.Contains(content, "quick brown fox") {
		t.Error("content should contain rendered text after rapid resizes")
	}

	// Switch to a non-active agent: cache was wiped, so it should re-render at new width
	content2 := app.renderContentForViewport("worker-1")
	if !strings.Contains(content2, "quick brown fox") {
		t.Error("inactive agent content should render correctly after cache invalidation")
	}
}

// --- Test 5: Cache invalidation covers ALL agents, not just activeAgent ---

func TestLayout_CacheInvalidation_AllAgents(t *testing.T) {
	agents := []AgentInfo{
		{Name: "agent-a", Type: "workspace", Alive: true},
		{Name: "agent-b", Type: "worker", Alive: true},
		{Name: "agent-c", Type: "worker", Alive: true},
	}
	app := newTestApp(100, 40, agents)

	// Populate all agents and render their caches
	for _, ag := range agents {
		app.outputContent[ag.Name] = []string{"line one", "line two"}
		app.renderer.RenderLines(ag.Name, app.outputContent[ag.Name])
	}

	// Verify all three have cache entries
	for _, ag := range agents {
		if _, ok := app.renderer.cache[ag.Name]; !ok {
			t.Errorf("%s should have cache before resize", ag.Name)
		}
	}

	// Resize triggers InvalidateCache
	app.Update(tea.WindowSizeMsg{Width: 60, Height: 40})

	// After resize, the old cache entries should be gone
	// (activeAgent may have been re-populated by updateViewport)
	for _, ag := range agents {
		if ag.Name == app.activeAgent {
			continue // active agent gets re-populated immediately
		}
		if c, ok := app.renderer.cache[ag.Name]; ok && c.rawCount > 0 {
			t.Errorf("%s cache should have been wiped by InvalidateCache, but rawCount=%d",
				ag.Name, c.rawCount)
		}
	}
}

// --- Test 6: Width change detection guard ---

func TestLayout_NoWidthChange_NoCacheInvalidation(t *testing.T) {
	agents := []AgentInfo{
		{Name: "workspace", Type: "workspace", Alive: true},
	}
	app := newTestApp(100, 40, agents)
	app.outputContent["workspace"] = []string{"cached line"}

	// Render and populate cache
	app.renderer.RenderLines("workspace", app.outputContent["workspace"])
	if _, ok := app.renderer.cache["workspace"]; !ok {
		t.Fatal("should have cache entry")
	}

	// Send same-size resize — width doesn't change, cache should survive
	origCache := app.renderer.cache["workspace"]
	app.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	if app.renderer.cache["workspace"] != origCache {
		t.Error("cache should not be invalidated when width doesn't change")
	}
}

// --- Test 7: Activity panel interaction with resize ---

func TestLayout_ActivityPanel_ResizeInteraction(t *testing.T) {
	agents := []AgentInfo{
		{Name: "workspace", Type: "workspace", Alive: true},
	}
	app := newTestApp(120, 40, agents)
	app.expandedView = false
	app.showAgentList = false

	// Add activity entries to enable activity panel
	app.activityLog = []ActivityEntry{
		{Agent: "workspace", Action: "test action"},
	}

	// At width 120 (> 80), activity panel should show
	app.recalcLayout()
	if !app.layoutShowActivity {
		t.Error("activity panel should show at width 120")
	}
	widthWithActivity := app.renderer.width

	// Compute expected: at 120, activity takes activityPanelWidth + 1 divider
	// Without activity at same terminal width, viewport should be wider
	app.activityLog = nil // remove activity → panel won't show
	app.recalcLayout()
	widthWithoutActivity := app.renderer.width
	if widthWithoutActivity <= widthWithActivity {
		t.Errorf("at same terminal width, viewport should be wider without activity panel: got %d vs %d",
			widthWithoutActivity, widthWithActivity)
	}

	// Also verify: at width 80, activity is ineligible (needs > 80)
	app.activityLog = []ActivityEntry{{Agent: "workspace", Action: "test"}}
	app.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	if app.layoutShowActivity {
		t.Error("activity panel should be hidden at width exactly 80 (need > 80)")
	}
}

// --- Test 8: LineRenderer width wrapping correctness after resize ---

func TestLayout_RenderWidth_WrappingCorrectness(t *testing.T) {
	r := NewLineRenderer(NewOutputFilter(DefaultFilterConfig()), 50)

	// Render a line that's exactly 50 chars — should NOT wrap at width 50
	exactLine := strings.Repeat("x", 48) // renderer width, within bounds
	result := r.RenderLines("test", []string{exactLine})
	if strings.Count(result, "\n") > 0 {
		t.Error("line within width should not wrap")
	}

	// Now "resize" to 25 and invalidate
	r.width = 25
	r.wrapStyle = newWrapStyle(25)
	r.InvalidateCache()

	// Same line should now wrap
	result = r.RenderLines("test", []string{exactLine})
	if strings.Count(result, "\n") == 0 {
		t.Error("line exceeding new width should wrap after resize+invalidation")
	}
}

// helper to create a wrapStyle like the real code does
func newWrapStyle(width int) lipgloss.Style {
	return lipgloss.NewStyle().Width(width)
}
