package tui

import (
	"fmt"
	"strings"
	"testing"
)

// =============================================================================
// BUG-REPRODUCING TESTS — these expose the current dedup bugs.
// Each test documents what SHOULD happen. Fix the code until they pass.
// =============================================================================

// Bug 1: First-match-wins replaces the WRONG line when a shorter prefix
// appears more recently in the buffer than the correct longer prefix.
func TestDedup_BestMatchNotFirstMatch(t *testing.T) {
	buf := []string{
		"This is the information", // index 0 — correct target (longer prefix)
		"Other stuff in between",  // index 1
		"This is the",             // index 2 — wrong target (shorter prefix)
	}
	buf, replacedIdx := DeduplicateAppend(buf, []string{
		"This is the information-bot project",
	})

	// The new line should replace "This is the information" (index 0),
	// NOT "This is the" (index 2).
	if len(buf) != 3 {
		t.Fatalf("expected 3 lines, got %d: %v", len(buf), buf)
	}

	// "This is the" should still be in the buffer untouched
	found := false
	for _, line := range buf {
		if strings.TrimSpace(line) == "This is the" {
			found = true
		}
	}
	if !found {
		t.Errorf("'This is the' should remain untouched in buffer, got: %v", buf)
	}

	// The extended line should be present (replacing "This is the information")
	foundExtended := false
	for _, line := range buf {
		if strings.TrimSpace(line) == "This is the information-bot project" {
			foundExtended = true
		}
	}
	if !foundExtended {
		t.Errorf("extended line should be in buffer, got: %v", buf)
	}

	if replacedIdx < 0 {
		t.Error("expected replacedIdx >= 0 for progressive extension")
	}
	if replacedIdx != 0 {
		t.Errorf("expected replacement at index 0, got %d", replacedIdx)
	}
}

// Bug 2: No word-boundary awareness — "This is the" falsely matches
// "This is therapy" because character-level prefix matches.
// "This is the" is 11 chars (>= progressivePrefixMin of 8), and
// "This is therapy"[0:11] == "This is the" at the byte level.
func TestDedup_WordBoundaryFalsePositive(t *testing.T) {
	buf := []string{"This is the"}
	buf, replacedIdx := DeduplicateAppend(buf, []string{
		"This is therapy for the soul",
	})

	// These are DIFFERENT sentences. Both should be in the buffer.
	// "the" vs "therapy" — the prefix match is a mid-word coincidence.
	if len(buf) != 2 {
		t.Errorf("expected 2 lines (different sentences), got %d: %v", len(buf), buf)
	}
	if replacedIdx >= 0 {
		t.Error("should not have replaced — different words at boundary")
	}
}

// Bug 2 variant: "understand" should not match "understanding" as a
// valid progressive extension — the extension continues the same word,
// not adds a new one.
func TestDedup_WordBoundaryMidWord(t *testing.T) {
	buf := []string{"I understand"}
	buf, replacedIdx := DeduplicateAppend(buf, []string{
		"I understanding the problem requires careful thought",
	})

	// "understand" → "understanding" is NOT a word-level extension.
	if len(buf) != 2 {
		t.Errorf("expected 2 lines (mid-word mismatch), got %d: %v", len(buf), buf)
	}
	if replacedIdx >= 0 {
		t.Error("should not have replaced — mid-word boundary")
	}
}

func TestDedup_WordBoundaryValidExtension(t *testing.T) {
	buf := []string{"This is the original"}
	buf, replacedIdx := DeduplicateAppend(buf, []string{
		"This is the original text with more content",
	})

	// This IS a valid progressive extension (word boundary: space after "original")
	if len(buf) != 1 {
		t.Errorf("expected 1 line (valid extension), got %d: %v", len(buf), buf)
	}
	if replacedIdx < 0 {
		t.Error("expected replacedIdx >= 0 for valid progressive extension")
	}
}

// Bug 2b: Word boundary — punctuation should be a valid boundary.
func TestDedup_WordBoundaryPunctuation(t *testing.T) {
	buf := []string{"I'll help you"}
	buf, replacedIdx := DeduplicateAppend(buf, []string{
		"I'll help you, starting with the tests",
	})

	// Comma after "you" is a valid word boundary
	if len(buf) != 1 {
		t.Errorf("expected 1 line (punctuation boundary), got %d: %v", len(buf), buf)
	}
	if replacedIdx < 0 {
		t.Error("expected replacedIdx >= 0 for punctuation boundary extension")
	}
}

// Bug 3: Stale fragment suppression eats legitimate new content when the
// lookback window is too large.
func TestDedup_StaleSuppression_DoesNotEatDistantContent(t *testing.T) {
	// Simulate: agent said a long sentence 20 lines ago, now starts a new
	// response that happens to begin the same way.
	buf := []string{
		"I'll help you review the configuration files and ensure everything is set up correctly.",
	}
	// Add 20 intervening lines
	for i := 0; i < 20; i++ {
		buf = append(buf, fmt.Sprintf("Intervening line %d with some content", i))
	}
	// Now the agent starts a new, different response with the same prefix
	buf, _ = DeduplicateAppend(buf, []string{
		"I'll help you review the configuration",
	})

	// The new line should NOT be suppressed — it's too far from the original
	// and is likely a new response, not a stale fragment.
	lastLine := buf[len(buf)-1]
	if strings.TrimSpace(lastLine) != "I'll help you review the configuration" {
		t.Errorf("new content was falsely suppressed as stale fragment, last line: %q", lastLine)
	}
}

// Bug 3b: Stale suppression SHOULD work for adjacent lines (real redraws).
func TestDedup_StaleSuppressionWorksForAdjacentLines(t *testing.T) {
	buf := []string{
		"I'll help you review the configuration files and ensure everything is set up correctly.",
	}
	// Immediately followed by a shorter version (cursor redraw)
	buf, _ = DeduplicateAppend(buf, []string{
		"I'll help you review the configuration",
	})

	// Should be suppressed — it's right next to the full version
	if len(buf) != 1 {
		t.Errorf("adjacent stale fragment should be suppressed, got %d lines: %v", len(buf), buf)
	}
}

// Short-fragment streaming: "Let" → "Let me check..." should collapse.
// These are the exact patterns from the screenshot bug.
func TestDedup_ShortFragmentStreaming_Let(t *testing.T) {
	buf, _ := DeduplicateAppend(nil, []string{
		"Let",
		"Let me check the current state of the project and agents.",
	})
	if len(buf) != 1 {
		t.Errorf("expected 1 line ('Let' extended), got %d: %v", len(buf), buf)
	}
}

func TestDedup_ShortFragmentStreaming_What(t *testing.T) {
	buf, _ := DeduplicateAppend(nil, []string{
		"What",
		"What can I help you with?",
	})
	if len(buf) != 1 {
		t.Errorf("expected 1 line ('What' extended), got %d: %v", len(buf), buf)
	}
}

// "Yes" → "Yesterday" should NOT collapse (different words, boundary check rejects).
func TestDedup_ShortFragment_FalsePositive_YesYesterday(t *testing.T) {
	buf, _ := DeduplicateAppend(nil, []string{
		"Yes",
		"Yesterday was a good day",
	})
	if len(buf) != 2 {
		t.Errorf("expected 2 lines (Yes != Yesterday), got %d: %v", len(buf), buf)
	}
}

// "the" → "therapy" should NOT collapse.
func TestDedup_ShortFragment_FalsePositive_TheTherapy(t *testing.T) {
	buf, _ := DeduplicateAppend(nil, []string{
		"the",
		"therapy sessions were helpful",
	})
	if len(buf) != 2 {
		t.Errorf("expected 2 lines (the != therapy), got %d: %v", len(buf), buf)
	}
}

// Bug: Multiple progressive extensions in a single batch should all resolve correctly.
func TestDedup_MultipleBatchExtensions(t *testing.T) {
	buf, _ := DeduplicateAppend(nil, []string{
		"The quick brown fox",
		"The quick brown fox jumps",
		"The quick brown fox jumps over the lazy",
		"The quick brown fox jumps over the lazy dog",
	})

	// All intermediate fragments should collapse to the final version
	if len(buf) != 1 {
		t.Errorf("expected 1 line after progressive collapse, got %d: %v", len(buf), buf)
	}
	if strings.TrimSpace(buf[0]) != "The quick brown fox jumps over the lazy dog" {
		t.Errorf("expected final extended line, got %q", buf[0])
	}
}

// Edge case: exact same line repeated many times in a batch
func TestDedup_RepeatedExactDupsInBatch(t *testing.T) {
	lines := make([]string, 50)
	for i := range lines {
		lines[i] = "Repeated line that appears many times in output"
	}
	buf, _ := DeduplicateAppend(nil, lines)
	if len(buf) != 1 {
		t.Errorf("expected 1 line after dedup of %d identical lines, got %d", len(lines), len(buf))
	}
}

// Edge case: lines that differ only in trailing whitespace
func TestDedup_TrailingWhitespaceDifference(t *testing.T) {
	buf, _ := DeduplicateAppend(nil, []string{
		"Hello world",
		"Hello world   ",
		"Hello world\t",
	})
	// All should dedup to 1 since we TrimSpace for comparison
	if len(buf) != 1 {
		t.Errorf("expected 1 line (whitespace variants deduped), got %d: %v", len(buf), buf)
	}
}

// Edge case: interleaved streaming from two conceptually different outputs
func TestDedup_InterleavedStreams(t *testing.T) {
	buf, _ := DeduplicateAppend(nil, []string{
		"(*) execute(\"git status\")",        // tool call
		"I'm checking the repository status", // agent text streaming
		"I'm checking the repository status and will report",
	})

	// Tool call + final text = 2 lines (intermediate text deduped)
	if len(buf) != 2 {
		t.Errorf("expected 2 lines (tool call + final text), got %d: %v", len(buf), buf)
	}
}

// Edge case: Unicode content in progressive extension
func TestDedup_UnicodeProgressive(t *testing.T) {
	buf, _ := DeduplicateAppend(nil, []string{
		"日本語のテスト",
	})
	buf, replacedIdx := DeduplicateAppend(buf, []string{
		"日本語のテストメッセージです",
	})
	if len(buf) != 1 {
		t.Errorf("expected 1 line after unicode extension, got %d: %v", len(buf), buf)
	}
	if replacedIdx < 0 {
		t.Error("expected replacedIdx >= 0 for unicode progressive extension")
	}
}

// =============================================================================
// BENCHMARKS — measure the hot paths under realistic load
// =============================================================================

// BenchmarkDedup_ProgressiveStream simulates realistic LLM token streaming:
// each line extends the previous by a few words.
func BenchmarkDedup_ProgressiveStream(b *testing.B) {
	// Pre-build streaming lines (simulating token-by-token growth)
	words := strings.Fields("The quick brown fox jumps over the lazy dog and then runs across the field to find shelter from the incoming storm that was predicted by the local weather station earlier today")
	lines := make([]string, len(words))
	for i := range words {
		lines[i] = strings.Join(words[:i+1], " ")
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf := make([]string, 0, 10)
		for _, line := range lines {
			buf, _ = DeduplicateAppend(buf, []string{line})
		}
	}
}

// BenchmarkDedup_LargeBuffer simulates appending to a nearly-full 5000-line buffer.
func BenchmarkDedup_LargeBuffer(b *testing.B) {
	// Pre-fill buffer with 4900 unique lines
	buf := make([]string, 4900)
	for i := range buf {
		buf[i] = fmt.Sprintf("Existing line %d with some content to make it realistic length for a terminal", i)
	}

	newLines := []string{
		"A completely new line of agent output",
		"Another new line that is different",
		"Yet another unique line of output",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Copy buffer to avoid mutation across iterations
		tmp := make([]string, len(buf))
		copy(tmp, buf)
		DeduplicateAppend(tmp, newLines)
	}
}

// BenchmarkDedup_BatchOf100 simulates a burst of 100 lines arriving at once
// (the max batch size from readStream).
func BenchmarkDedup_BatchOf100(b *testing.B) {
	existing := make([]string, 200)
	for i := range existing {
		existing[i] = fmt.Sprintf("Pre-existing line number %d in the buffer", i)
	}

	batch := make([]string, 100)
	for i := range batch {
		batch[i] = fmt.Sprintf("New batch line %d with unique content here", i)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tmp := make([]string, len(existing))
		copy(tmp, existing)
		DeduplicateAppend(tmp, batch)
	}
}

// BenchmarkDedup_WorstCase_AllDuplicates — every new line is an exact duplicate.
// Tests the O(n*lookback) worst-case scan.
func BenchmarkDedup_WorstCase_AllDuplicates(b *testing.B) {
	existing := make([]string, 30) // exactly lookback size
	for i := range existing {
		existing[i] = fmt.Sprintf("Line %d", i)
	}

	// All new lines are duplicates of existing lines
	batch := make([]string, 100)
	for i := range batch {
		batch[i] = existing[i%30]
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tmp := make([]string, len(existing))
		copy(tmp, existing)
		DeduplicateAppend(tmp, batch)
	}
}

// BenchmarkDedup_WorstCase_NearMissPrefixes — every line is a near-miss prefix
// match that requires scanning the full lookback window.
func BenchmarkDedup_WorstCase_NearMissPrefixes(b *testing.B) {
	existing := make([]string, 30)
	for i := range existing {
		existing[i] = fmt.Sprintf("This is line number %d and it has enough content to exceed the prefix minimum", i)
	}

	batch := make([]string, 50)
	for i := range batch {
		// Shares long prefix with existing lines but diverges after "number"
		batch[i] = fmt.Sprintf("This is line number %d but completely different ending here xyz", i+30)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tmp := make([]string, len(existing))
		copy(tmp, existing)
		DeduplicateAppend(tmp, batch)
	}
}

// BenchmarkDedup_RealisticMixed simulates a realistic mix: some progressive
// extensions, some exact dupes, some unique lines, some blanks.
func BenchmarkDedup_RealisticMixed(b *testing.B) {
	existing := []string{
		"(*) execute(\"git status\")",
		"",
		"On branch main",
		"Your branch is up to date with 'origin/main'.",
		"",
		"nothing to commit, working tree clean",
		"",
		"I can see the repository is clean. Let me",
	}

	batch := []string{
		"I can see the repository is clean. Let me check",                                         // progressive extension
		"I can see the repository is clean. Let me check the configuration",                       // progressive extension
		"I can see the repository is clean. Let me check the configuration files",                 // progressive extension
		"I can see the repository is clean. Let me check the configuration files for any issues.", // final
		"",
		"(*) read_file(\"config.yaml\")",
		"⎿ database:",
		"  host: localhost",
		"  port: 5432",
		"I can see the repository is clean. Let me check the configuration files for any issues.", // exact dup
		"",
		"The configuration looks correct. The database", // new streaming start
		"The configuration looks correct. The database is configured",
		"The configuration looks correct. The database is configured to connect to localhost:5432.",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tmp := make([]string, len(existing))
		copy(tmp, existing)
		DeduplicateAppend(tmp, batch)
	}
}

// BenchmarkAppendDeduped_SingleLine measures the per-line cost.
func BenchmarkAppendDeduped_SingleLine(b *testing.B) {
	buf := make([]string, 30)
	for i := range buf {
		buf[i] = fmt.Sprintf("Existing line %d with content", i)
	}
	newLine := "A completely unique new line of output"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		appendDeduped(buf, newLine)
	}
}
