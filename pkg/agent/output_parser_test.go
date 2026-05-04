package agent

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestOutputWatcher_PRCreated(t *testing.T) {
	input := "Some output...\nCreated PR: https://github.com/owner/repo/pull/42\nMore output\n"
	w := NewOutputWatcher(strings.NewReader(input))
	defer w.Stop()

	event := waitForEvent(t, w, 2*time.Second)
	if event.Type != EventPRCreated {
		t.Errorf("expected EventPRCreated, got %s", event.Type)
	}
	if event.Message != "https://github.com/owner/repo/pull/42" {
		t.Errorf("unexpected message: %s", event.Message)
	}
}

func TestOutputWatcher_Error(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"rate limit", "Error: Rate limit exceeded\n"},
		{"permission denied", "Permission denied: cannot write to repo\n"},
		{"tool failed", "Tool execution failed: bash returned error\n"},
		{"api error", "API error: 500 internal server error\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := NewOutputWatcher(strings.NewReader(tt.input))
			defer w.Stop()

			event := waitForEvent(t, w, 2*time.Second)
			if event.Type != EventError {
				t.Errorf("expected EventError, got %s", event.Type)
			}
		})
	}
}

func TestOutputWatcher_TaskComplete(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"task complete", "Task complete. All changes committed.\n"},
		{"successfully merged", "PR #42 was successfully merged.\n"},
		{"pull request merged", "Pull request has been merged into main.\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := NewOutputWatcher(strings.NewReader(tt.input))
			defer w.Stop()

			event := waitForEvent(t, w, 2*time.Second)
			if event.Type != EventTaskComplete {
				t.Errorf("expected EventTaskComplete, got %s", event.Type)
			}
		})
	}
}

func TestOutputWatcher_StuckDetection(t *testing.T) {
	// Generate 5 very similar long lines (>100 chars each)
	repeatedLine := strings.Repeat("The agent is thinking about the problem and considering various approaches to solve it... ", 2)
	var lines []string
	for i := 0; i < 6; i++ {
		lines = append(lines, repeatedLine)
	}
	input := strings.Join(lines, "\n") + "\n"

	w := NewOutputWatcher(strings.NewReader(input), WithStuckBufferSize(5))
	defer w.Stop()

	event := waitForEvent(t, w, 2*time.Second)
	if event.Type != EventStuck {
		t.Errorf("expected EventStuck, got %s", event.Type)
	}
	if !strings.Contains(event.Message, "repeated output") {
		t.Errorf("unexpected message: %s", event.Message)
	}
}

func TestOutputWatcher_NoFalseStuck(t *testing.T) {
	// Different lines should not trigger stuck detection
	var lines []string
	for i := 0; i < 6; i++ {
		lines = append(lines, strings.Repeat(string(rune('a'+i)), 120))
	}
	input := strings.Join(lines, "\n") + "\n"

	w := NewOutputWatcher(strings.NewReader(input), WithStuckBufferSize(5))
	defer w.Stop()

	// Should not get a stuck event — only EOF close
	select {
	case event, ok := <-w.Events():
		if ok && event.Type == EventStuck {
			t.Error("unexpected EventStuck for diverse output")
		}
	case <-time.After(500 * time.Millisecond):
		// Expected: no events
	}
}

func TestTrigramJaccard(t *testing.T) {
	// Identical strings should have similarity 1.0
	sim := trigramJaccard("hello world foo bar", "hello world foo bar")
	if sim != 1.0 {
		t.Errorf("identical strings: expected 1.0, got %f", sim)
	}

	// Completely different strings should have low similarity
	sim = trigramJaccard("aaaaaaaaaa", "zzzzzzzzzz")
	if sim > 0.1 {
		t.Errorf("different strings: expected low similarity, got %f", sim)
	}

	// Short strings
	sim = trigramJaccard("ab", "ab")
	if sim != 0 {
		t.Errorf("too-short strings: expected 0, got %f", sim)
	}
}

func TestOutputWatcher_StopIsIdempotent(t *testing.T) {
	r, w := io.Pipe()
	watcher := NewOutputWatcher(r)
	watcher.Stop()
	watcher.Stop() // Should not panic
	w.Close()
}

func waitForEvent(t *testing.T, w *OutputWatcher, timeout time.Duration) AgentEvent {
	t.Helper()
	select {
	case event, ok := <-w.Events():
		if !ok {
			t.Fatal("events channel closed before receiving event")
		}
		return event
	case <-time.After(timeout):
		t.Fatal("timed out waiting for event")
		return AgentEvent{}
	}
}
