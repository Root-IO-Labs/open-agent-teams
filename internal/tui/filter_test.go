package tui

import (
	"testing"
)

func TestClassify_ToolCalls(t *testing.T) {
	f := NewOutputFilter(DefaultFilterConfig())

	tests := []struct {
		line string
		want LineCategory
	}{
		{`(*) execute("git remote get-url origin")`, CatToolCall},
		{`(*) read_file(config.py)`, CatToolCall},
		{`⏺ ls(.)`, CatToolCall},
	}
	for _, tt := range tests {
		got := f.Classify(tt.line)
		if got != tt.want {
			t.Errorf("Classify(%q) = %s, want %s", tt.line, got, tt.want)
		}
	}
}

func TestClassify_ToolOutput(t *testing.T) {
	f := NewOutputFilter(DefaultFilterConfig())

	tests := []struct {
		line string
		want LineCategory
	}{
		{`⎿     .deepagents/`, CatToolOutput},
		{`  [Command succeeded with exit code 0]`, CatToolOutput},
		{`… 6 more — click or Ctrl+E to expand`, CatChrome},
		{`  | > What repository am I working in?`, CatToolOutput},
		{`  ▎ ⏺ ls(.)`, CatToolOutput},
	}
	for _, tt := range tests {
		got := f.Classify(tt.line)
		if got != tt.want {
			t.Errorf("Classify(%q) = %s, want %s", tt.line, got, tt.want)
		}
	}
}

func TestClassify_LPrefixNotToolOutput(t *testing.T) {
	// "L " prefix was a false positive — regular text starting with "L" should be CatText
	f := NewOutputFilter(DefaultFilterConfig())
	got := f.Classify("L https://github.com/owner/repo")
	if got != CatText {
		t.Errorf("Classify(\"L ...\") = %s, want text (no longer tool_output)", got)
	}
}

func TestClassify_Thinking(t *testing.T) {
	f := NewOutputFilter(DefaultFilterConfig())

	tests := []struct {
		line string
		want LineCategory
	}{
		{` Thinking...`, CatThinking},
		{`Thinking...`, CatThinking},
	}
	for _, tt := range tests {
		got := f.Classify(tt.line)
		if got != tt.want {
			t.Errorf("Classify(%q) = %s, want %s", tt.line, got, tt.want)
		}
	}
}

func TestClassify_Progress(t *testing.T) {
	f := NewOutputFilter(DefaultFilterConfig())

	tests := []struct {
		line string
		want LineCategory
	}{
		{`⠋`, CatProgress},
		{`⠙`, CatProgress},
		{`(-)`, CatProgress},
		{`(\)`, CatProgress},
		{`(|)`, CatProgress},
		{`(/)`, CatProgress},
		{`(0s, esc to interrupt)`, CatProgress},
		{`(2s, esc to interrupt)`, CatProgress},
		{`(15s, esc to interrupt)`, CatProgress},
	}
	for _, tt := range tests {
		got := f.Classify(tt.line)
		if got != tt.want {
			t.Errorf("Classify(%q) = %s, want %s", tt.line, got, tt.want)
		}
	}
}

func TestClassify_Chrome(t *testing.T) {
	f := NewOutputFilter(DefaultFilterConfig())

	tests := []struct {
		line string
		want LineCategory
	}{
		{`┌──────────────────────────────────────┐`, CatChrome},
		{`├──────────────────────────────────────┤`, CatChrome},
		{`└──────────────────────────────────────┘`, CatChrome},
		{`▁▁`, CatChrome},
		{`▇▇`, CatChrome},
		{`13.4K tokens`, CatChrome},
		{`auto | shift+tab to cycle`, CatChrome},
		{`anthropic:claude-sonnet-4-6`, CatChrome},
		// Bare status words from agent TUI
		{`okay`, CatChrome},
		{`Okay`, CatChrome},
		{`ok`, CatChrome},
		{`ready`, CatChrome},
		{`idle`, CatChrome},
		{`Running`, CatChrome},
	}
	for _, tt := range tests {
		got := f.Classify(tt.line)
		if got != tt.want {
			t.Errorf("Classify(%q) = %s, want %s", tt.line, got, tt.want)
		}
	}
}

func TestClassify_System(t *testing.T) {
	f := NewOutputFilter(DefaultFilterConfig())

	tests := []struct {
		line string
		want LineCategory
	}{
		{`> 📨 Message from daemon: Agent definitions available`, CatSystem},
		{`[daemon] Status check: Update on your review progress?`, CatSystem},
	}
	for _, tt := range tests {
		got := f.Classify(tt.line)
		if got != tt.want {
			t.Errorf("Classify(%q) = %s, want %s", tt.line, got, tt.want)
		}
	}
}

func TestClassify_UserInput(t *testing.T) {
	f := NewOutputFilter(DefaultFilterConfig())

	tests := []struct {
		line string
		want LineCategory
	}{
		{`> What repository am I working in?`, CatUserInput},
		{`> your instruction here`, CatUserInput},
	}
	for _, tt := range tests {
		got := f.Classify(tt.line)
		if got != tt.want {
			t.Errorf("Classify(%q) = %s, want %s", tt.line, got, tt.want)
		}
	}
}

func TestClassify_Banner(t *testing.T) {
	f := NewOutputFilter(DefaultFilterConfig())

	tests := []struct {
		line string
		want LineCategory
	}{
		{`   ██████╗  █████╗ ████████╗`, CatChrome},
		{`                                                                   by Root.io`, CatChrome},
		{`   OAT - Open Agent Teams ready! What would you like to build?`, CatChrome},
		{`   Enter send - Ctrl+J newline - @ files - / commands`, CatChrome},
		{`   Thread: 6e9f918d`, CatChrome},
		{`Starting with thread: 6e9f918d`, CatChrome},
		{`<frozen runpy>:128: RuntimeWarning: 'deepagents_cli.main' found in sys.modules`, CatChrome},
	}
	for _, tt := range tests {
		got := f.Classify(tt.line)
		if got != tt.want {
			t.Errorf("Classify(%q) = %s, want %s", tt.line, got, tt.want)
		}
	}
}

func TestClassify_Text(t *testing.T) {
	f := NewOutputFilter(DefaultFilterConfig())

	tests := []struct {
		line string
		want LineCategory
	}{
		{`This is a normal response from the agent.`, CatText},
		{`I'll help you with that task.`, CatText},
		{``, CatText},
	}
	for _, tt := range tests {
		got := f.Classify(tt.line)
		if got != tt.want {
			t.Errorf("Classify(%q) = %s, want %s", tt.line, got, tt.want)
		}
	}
}

func TestFilterLines(t *testing.T) {
	f := NewOutputFilter(DefaultFilterConfig())

	lines := []string{
		"Starting with thread: abc123",
		"┌─────────────────────┐",
		"⠋",
		"(0s, esc to interrupt)",
		"(*) execute(\"ls\")",
		"L file1.go",
		"  [Command succeeded with exit code 0]",
		"Here is the result of my analysis.",
		"13.4K tokens",
	}

	filtered := f.FilterLines(lines)

	// Should include: tool call, "L file1.go" (now text), tool output, text = 4
	// Should exclude: banner, chrome, spinner, countdown, token count
	if len(filtered) != 4 {
		t.Errorf("expected 4 lines after filtering, got %d: %v", len(filtered), filtered)
	}
}

func TestFilterConfig_ShouldShow(t *testing.T) {
	// Default config hides progress and chrome
	cfg := DefaultFilterConfig()

	if cfg.ShouldShow(CatProgress) {
		t.Error("default config should hide progress")
	}
	if cfg.ShouldShow(CatChrome) {
		t.Error("default config should hide chrome")
	}
	if !cfg.ShouldShow(CatText) {
		t.Error("default config should show text")
	}
	if !cfg.ShouldShow(CatToolCall) {
		t.Error("default config should show tool calls")
	}
}
