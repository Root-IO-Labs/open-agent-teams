package tui

import (
	"testing"
	"time"
)

func TestStreamBatchMsg_SuppressesPrefixedEcho(t *testing.T) {
	app := newTestApp(100, 30, []AgentInfo{
		{Name: "workspace", Type: "workspace", Alive: true},
	})
	app.recentInputs = []recentInput{
		{text: "hello there", sentAt: time.Now()},
	}

	model, _ := app.Update(streamBatchMsg{
		agent: "workspace",
		lines: []string{"> hello there"},
	})
	app = model.(*App)

	if got := len(app.outputContent["workspace"]); got != 0 {
		t.Fatalf("expected prefixed echo to be suppressed, got %d lines: %v", got, app.outputContent["workspace"])
	}
	// recentInputs entries are kept (not consumed) so fragment detection
	// works across later batches. They expire via pruneRecentInputs.
	if got := len(app.recentInputs); got != 1 {
		t.Fatalf("expected recentInputs to be preserved for fragment detection, got %d entries", got)
	}
}

func TestStreamBatchMsg_SuppressesTinyEchoFragmentsAfterMatchedEcho(t *testing.T) {
	app := newTestApp(100, 30, []AgentInfo{
		{Name: "workspace", Type: "workspace", Alive: true},
	})
	app.recentInputs = []recentInput{
		{text: "helllo", sentAt: time.Now()},
	}

	model, _ := app.Update(streamBatchMsg{
		agent: "workspace",
		lines: []string{"> helllo", "e", "r", "real output"},
	})
	app = model.(*App)

	want := []string{"real output"}
	got := app.outputContent["workspace"]
	if len(got) != len(want) {
		t.Fatalf("expected %d visible lines, got %d: %v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected line %d to be %q, got %q", i, want[i], got[i])
		}
	}
}

// Regression: fragments arrive in a SEPARATE batch from the full echo.
// The old code only suppressed 2-char fragments when the full echo was
// in the same batch (matchedInputThisBatch), and only suppressed 1-char
// fragments within 1 second. Both restrictions are now removed.
func TestStreamBatchMsg_SuppressesFragmentsInSeparateBatch(t *testing.T) {
	app := newTestApp(100, 30, []AgentInfo{
		{Name: "workspace", Type: "workspace", Alive: true},
	})
	// Input was sent recently but full echo was already consumed in a previous batch.
	// Only the recentInput entry remains (not yet pruned).
	app.recentInputs = []recentInput{
		{text: "testing", sentAt: time.Now()},
	}

	// Fragments arrive without the full echo in this batch
	model, _ := app.Update(streamBatchMsg{
		agent: "workspace",
		lines: []string{"t", "e", "I'm ready to help."},
	})
	app = model.(*App)

	got := app.outputContent["workspace"]
	if len(got) != 1 || got[0] != "I'm ready to help." {
		t.Fatalf("expected only real output, got %d lines: %v", len(got), got)
	}
}

// Regression: 2-char fragments were only suppressed via matchedInputThisBatch.
// Now they check recentInputs directly.
func TestStreamBatchMsg_SuppressesTwoCharFragments(t *testing.T) {
	app := newTestApp(100, 30, []AgentInfo{
		{Name: "workspace", Type: "workspace", Alive: true},
	})
	app.recentInputs = []recentInput{
		{text: "hello world", sentAt: time.Now()},
	}

	model, _ := app.Update(streamBatchMsg{
		agent: "workspace",
		lines: []string{"he", "wo", "The agent is working."},
	})
	app = model.(*App)

	got := app.outputContent["workspace"]
	if len(got) != 1 || got[0] != "The agent is working." {
		t.Fatalf("expected 2-char fragments suppressed, got %d lines: %v", len(got), got)
	}
}

// Non-letter short lines (numbers, punctuation) should NOT be suppressed.
func TestStreamBatchMsg_PreservesNonLetterShortLines(t *testing.T) {
	app := newTestApp(100, 30, []AgentInfo{
		{Name: "workspace", Type: "workspace", Alive: true},
	})
	app.recentInputs = []recentInput{
		{text: "run tests", sentAt: time.Now()},
	}

	model, _ := app.Update(streamBatchMsg{
		agent: "workspace",
		lines: []string{"2", "42", "ok done"},
	})
	app = model.(*App)

	got := app.outputContent["workspace"]
	// "2" and "42" are not letter-only, so they should pass through
	if len(got) != 3 {
		t.Fatalf("expected non-letter short lines preserved, got %d lines: %v", len(got), got)
	}
}

// When recentInputs is empty, short lines pass through normally.
func TestStreamBatchMsg_NoSuppressionWithoutRecentInputs(t *testing.T) {
	app := newTestApp(100, 30, []AgentInfo{
		{Name: "workspace", Type: "workspace", Alive: true},
	})
	// No recent inputs
	app.recentInputs = nil

	model, _ := app.Update(streamBatchMsg{
		agent: "workspace",
		lines: []string{"I", "am", "here"},
	})
	app = model.(*App)

	got := app.outputContent["workspace"]
	if len(got) != 3 {
		t.Fatalf("expected all lines to pass through, got %d lines: %v", len(got), got)
	}
}

// Expired recentInputs should not cause fragment suppression.
func TestStreamBatchMsg_ExpiredInputsNoSuppression(t *testing.T) {
	app := newTestApp(100, 30, []AgentInfo{
		{Name: "workspace", Type: "workspace", Alive: true},
	})
	// Input sent 10 seconds ago — well past recentInputMaxAge (2s)
	app.recentInputs = []recentInput{
		{text: "hello", sentAt: time.Now().Add(-10 * time.Second)},
	}

	model, _ := app.Update(streamBatchMsg{
		agent: "workspace",
		lines: []string{"h", "e", "The response."},
	})
	app = model.(*App)

	got := app.outputContent["workspace"]
	// Expired input is pruned, so "h" and "e" should pass through
	if len(got) != 3 {
		t.Fatalf("expected expired inputs to not suppress, got %d lines: %v", len(got), got)
	}
}
