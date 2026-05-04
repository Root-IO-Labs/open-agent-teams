package tui

import (
	"testing"
)

func TestDedup_ExactDuplicate(t *testing.T) {
	buf := []string{"hello world"}
	buf, _ = DeduplicateAppend(buf, []string{"hello world"})
	if len(buf) != 1 {
		t.Errorf("expected 1 line after exact dedup, got %d", len(buf))
	}
}

func TestDedup_DifferentLines(t *testing.T) {
	buf := []string{"first line"}
	buf, _ = DeduplicateAppend(buf, []string{"completely different line"})
	if len(buf) != 2 {
		t.Errorf("expected 2 lines for different content, got %d", len(buf))
	}
}

func TestDedup_PreservesBlanksBetweenDifferentContent(t *testing.T) {
	buf, _ := DeduplicateAppend(nil, []string{
		"First paragraph content",
		"",
		"Second paragraph content",
	})
	if len(buf) != 3 {
		t.Errorf("expected 3 lines (with blank separator), got %d: %v", len(buf), buf)
	}
}

func TestDedup_CollapsesExcessiveBlanks(t *testing.T) {
	buf, _ := DeduplicateAppend(nil, []string{
		"content",
		"",
		"",
		"",
		"more content",
	})
	// Consecutive blanks collapse to at most 1: content, blank, more content
	if len(buf) != 3 {
		t.Errorf("expected 3 lines (excess blanks collapsed to 1), got %d: %v", len(buf), buf)
	}
}

func TestDedup_ProgressiveExtension(t *testing.T) {
	buf, _ := DeduplicateAppend(nil, []string{
		"This is the information",
	})
	buf, replacedIdx := DeduplicateAppend(buf, []string{
		"This is the information-bot project",
	})
	if len(buf) != 1 {
		t.Errorf("expected 1 line after progressive dedup, got %d: %v", len(buf), buf)
	}
	if buf[0] != "This is the information-bot project" {
		t.Errorf("expected extended line, got %q", buf[0])
	}
	if replacedIdx < 0 {
		t.Error("expected replacedIdx >= 0 for progressive extension")
	}
}

func TestDedup_ShortFragmentNotProgressiveDeduped(t *testing.T) {
	buf, _ := DeduplicateAppend(nil, []string{"Yes"})
	buf, _ = DeduplicateAppend(buf, []string{"Yesterday was fun"})
	if len(buf) != 2 {
		t.Errorf("expected 2 lines (short fragment not deduped), got %d: %v", len(buf), buf)
	}
}

func TestDedup_StaleFragment(t *testing.T) {
	buf, _ := DeduplicateAppend(nil, []string{
		"This is the information-bot project — a Python-based system",
	})
	buf, replacedIdx := DeduplicateAppend(buf, []string{
		"This is the information-bot project",
	})
	if len(buf) != 1 {
		t.Errorf("expected 1 line (stale fragment skipped), got %d: %v", len(buf), buf)
	}
	if replacedIdx >= 0 {
		t.Error("expected replacedIdx < 0 for stale fragment skip")
	}
}

func TestDedup_ExactDuplicateAcrossBatches(t *testing.T) {
	buf, _ := DeduplicateAppend(nil, []string{"Scheduler + config system"})
	buf, _ = DeduplicateAppend(buf, []string{"Scheduler + config system"})
	if len(buf) != 1 {
		t.Errorf("expected 1 line across batches, got %d: %v", len(buf), buf)
	}
}

// Markdown-shifting tests: backticks at different positions between fragments.
// These reproduce the exact stuttering bug from the TUI screenshot.

func TestDedup_MarkdownShift_TrailingBacktick(t *testing.T) {
	// Fragment has trailing backtick (opening code span), full line doesn't
	buf, _ := DeduplicateAppend(nil, []string{
		"T3 + T4 together — once T2 is solid, `",
	})
	buf, replacedIdx := DeduplicateAppend(buf, []string{
		"T3 + T4 together — once T2 is solid, PersistenceWriter and SemanticDeduplicator",
	})
	if len(buf) != 1 {
		t.Errorf("expected 1 line (markdown-shifted extension), got %d: %v", len(buf), buf)
	}
	if replacedIdx < 0 {
		t.Error("expected replacedIdx >= 0 for markdown-shifted extension")
	}
}

func TestDedup_MarkdownShift_BacktickMidSentence(t *testing.T) {
	// Backtick appears in fragment, gets absorbed into code span in extension
	buf, _ := DeduplicateAppend(nil, []string{
		"Wire main.py to actually start `",
	})
	buf, _ = DeduplicateAppend(buf, []string{
		"Wire main.py to actually start BotScheduler + APIServer",
	})
	if len(buf) != 1 {
		t.Errorf("expected 1 line (backtick shifted), got %d: %v", len(buf), buf)
	}
}

func TestDedup_MarkdownShift_BoldMarkers(t *testing.T) {
	// Fragment has opening ** that gets completed in extension
	buf, _ := DeduplicateAppend(nil, []string{
		"The **",
	})
	buf, _ = DeduplicateAppend(buf, []string{
		"The critical fix involves updating the dedup logic",
	})
	if len(buf) != 1 {
		t.Errorf("expected 1 line (bold markers stripped for comparison), got %d: %v", len(buf), buf)
	}
}

func TestDedup_MarkdownShift_MultiStep(t *testing.T) {
	// Progressive streaming with backticks shifting at each step
	buf, _ := DeduplicateAppend(nil, []string{
		"T3 + T4 together — once T2 is solid, `",
	})
	buf, _ = DeduplicateAppend(buf, []string{
		"T3 + T4 together — once T2 is solid, PersistenceWriter and `",
	})
	buf, _ = DeduplicateAppend(buf, []string{
		"T3 + T4 together — once T2 is solid, PersistenceWriter and SemanticDeduplicator are just consumers of the event stream.",
	})
	if len(buf) != 1 {
		t.Errorf("expected 1 line after multi-step markdown shift, got %d: %v", len(buf), buf)
	}
}

func TestDedup_MarkdownShift_StaleFragment(t *testing.T) {
	// Full line exists, then a shorter markdown-shifted fragment arrives (stale)
	buf, _ := DeduplicateAppend(nil, []string{
		"T3 + T4 together — once T2 is solid, PersistenceWriter and SemanticDeduplicator",
	})
	buf, _ = DeduplicateAppend(buf, []string{
		"T3 + T4 together — once T2 is solid, `",
	})
	if len(buf) != 1 {
		t.Errorf("expected 1 line (stale markdown-shifted fragment), got %d: %v", len(buf), buf)
	}
}

func TestDedup_StripInlineMarkdown(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"no markdown here", "no markdown here"},
		{"The `code` block", "The code block"},
		{"**bold** text", "bold text"},
		{"use `npm install`", "use npm install"},
		{"trailing `", "trailing "},
		{"~~strikethrough~~", "strikethrough"},
		{"mixed `code` and **bold**", "mixed code and bold"},
		{"", ""},
	}
	for _, tt := range tests {
		got := stripInlineMarkdown(tt.input)
		if got != tt.want {
			t.Errorf("stripInlineMarkdown(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
