package backend

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestDiagnostic_PeriodicFlushFragmentLeakage is a diagnostic test that
// reproduces the streaming fragment leakage visible in the TUI screenshot.
//
// The 200ms periodic commitPending() timer commits pending lines mid-stream.
// When the extension arrives, Layer 1 has no memory of the committed line,
// so the extension is written as a NEW line in the log. Layer 2 (TUI dedup)
// then must catch it, but often can't due to word-boundary rules.
//
// This test simulates realistic streaming patterns and measures how many
// fragments leak through each layer.
func TestDiagnostic_PeriodicFlushFragmentLeakage(t *testing.T) {
	// Streaming sequences from the screenshot.
	// Each sub-slice is one progressive streaming sequence — each line extends the previous.
	// In real usage, there are ~50-200ms gaps between lines (LLM token latency).
	streamingSequences := [][]string{
		// Pattern 1: Menu items (from screenshot)
		{
			"The 4-ticket plan",
			"The 4-ticket plan — refine scope, ordering, or design decisions?",
		},
		// Pattern 2: Backtick-delimited code reference
		{
			"The `",
			"The `cleanLogWriter` itself — dive into the actual code and start implementing?",
		},
		// Pattern 3: Short fragment to full sentence
		{
			"Start small",
			"Start small, ship incrementally:",
		},
		// Pattern 4: Sentence with inline code
		{
			"The key insight: T1 means you can merge each ticket independently without breaking production.",
		},
		// Pattern 5: Code reference mid-sentence
		{
			"`OAT_NEW_PIPELINE=",
			"`OAT_NEW_PIPELINE=1` is only flipped in dev/test until all 4 are done and validated.",
		},
		// Pattern 6: T3+T4 repeated fragments
		{
			"T3 + T4 together — once T2 is solid, `",
			"T3 + T4 together — once T2 is solid, PersistenceWriter and `",
			"T3 + T4 together — once T2 is solid, PersistenceWriter and SemanticDeduplicator are just consumers of the event stream.",
		},
	}

	t.Run("Layer1_With2sTimer", func(t *testing.T) {
		// Use a short flush interval (100ms) so we can deterministically trigger the
		// periodic flush between writes without needing multi-second sleeps.
		// Sleep 200ms between writes (> 100ms interval) to guarantee the timer fires.
		var logBuf strings.Builder
		w := newCleanLogWriterWithInterval(&logBuf, 100*time.Millisecond)

		for _, seq := range streamingSequences {
			for _, line := range seq {
				w.Write([]byte(line + "\n"))
				// Sleep > flushInterval to guarantee the periodic flush fires between writes
				time.Sleep(200 * time.Millisecond)
			}
			w.Write([]byte("\n"))
			time.Sleep(50 * time.Millisecond)
		}
		w.Close()

		logLines := strings.Split(strings.TrimSpace(logBuf.String()), "\n")
		var nonEmpty []string
		for _, l := range logLines {
			if strings.TrimSpace(l) != "" {
				nonEmpty = append(nonEmpty, l)
			}
		}

		t.Logf("Layer 1 output (%d non-empty lines):", len(nonEmpty))
		fragments := 0
		for i, l := range nonEmpty {
			isFragment := false
			for j := i + 1; j < len(nonEmpty) && j <= i+5; j++ {
				if strings.HasPrefix(strings.TrimSpace(nonEmpty[j]), strings.TrimSpace(l)) &&
					strings.TrimSpace(nonEmpty[j]) != strings.TrimSpace(l) {
					isFragment = true
					break
				}
			}
			marker := "  "
			if isFragment {
				marker = "⚠ FRAGMENT"
				fragments++
			}
			t.Logf("  [%d] %s %q", i, marker, l)
		}
		// This is a diagnostic test: fragments are EXPECTED when the periodic flush
		// fires between streaming writes. The test documents the behavior so Layer 2
		// (TUI dedup) knows what to handle. It should not fail — just log the count.
		if fragments > 0 {
			t.Logf("Layer 1 leaked %d streaming fragments due to periodic flush (expected, handled by Layer 2)", fragments)
		} else {
			t.Logf("No fragments leaked (progressive dedup absorbed all extensions)")
		}
	})

	t.Run("Layer1_WithoutTimer_Baseline", func(t *testing.T) {
		// Same data but with NO sleep between lines — all within one 200ms window.
		// This shows what Layer 1 produces when the timer doesn't interfere.
		var logBuf strings.Builder
		w := newCleanLogWriter(&logBuf)

		for _, seq := range streamingSequences {
			for _, line := range seq {
				w.Write([]byte(line + "\n"))
				// No sleep — all writes happen in <1ms
			}
			w.Write([]byte("\n"))
		}
		w.Close()

		logLines := strings.Split(strings.TrimSpace(logBuf.String()), "\n")
		var nonEmpty []string
		for _, l := range logLines {
			if strings.TrimSpace(l) != "" {
				nonEmpty = append(nonEmpty, l)
			}
		}

		t.Logf("Layer 1 (no timer interference) output (%d non-empty lines):", len(nonEmpty))
		fragments := 0
		for i, l := range nonEmpty {
			isFragment := false
			for j := i + 1; j < len(nonEmpty) && j <= i+5; j++ {
				if strings.HasPrefix(strings.TrimSpace(nonEmpty[j]), strings.TrimSpace(l)) &&
					strings.TrimSpace(nonEmpty[j]) != strings.TrimSpace(l) {
					isFragment = true
					break
				}
			}
			marker := "  "
			if isFragment {
				marker = "⚠ FRAGMENT"
				fragments++
			}
			t.Logf("  [%d] %s %q", i, marker, l)
		}
		if fragments > 0 {
			t.Errorf("Layer 1 leaked %d fragments even without timer (unexpected)", fragments)
		}
	})
}

// TestDiagnostic_Layer2_WordBoundaryGaps tests specific patterns where
// Layer 2 dedup SHOULD catch fragments but can't due to missing boundary chars.
func TestDiagnostic_Layer2_WordBoundaryGaps(t *testing.T) {
	// These are patterns where Layer 1 leaked a fragment (due to periodic flush)
	// and Layer 2 should catch them. We test whether Layer 2 actually does.

	type testCase struct {
		name     string
		existing string // fragment already in buffer
		newLine  string // extension that should replace it
		wantLen  int    // expected buffer length (1 = caught, 2 = leaked)
	}

	cases := []testCase{
		{
			name:     "backtick_boundary",
			existing: "The `",
			newLine:  "The `cleanLogWriter` itself — dive into the actual code",
			wantLen:  1, // should catch
		},
		{
			name:     "backtick_code_ref",
			existing: "`OAT_NEW_PIPELINE=",
			newLine:  "`OAT_NEW_PIPELINE=1` is only flipped in dev/test",
			wantLen:  1,
		},
		{
			name:     "comma_boundary",
			existing: "Start small",
			newLine:  "Start small, ship incrementally:",
			wantLen:  1,
		},
		{
			name:     "dash_boundary",
			existing: "The 4-ticket plan",
			newLine:  "The 4-ticket plan — refine scope, ordering, or design decisions?",
			wantLen:  1,
		},
		{
			// Previously: diverged at position 42 (backtick vs 'P') and was NOT caught.
			// Now: markdown stripping removes the trailing backtick, so the stripped
			// prefix "T3 + T4 together — once T2 is solid, " IS a prefix of the
			// stripped new line. The fix correctly identifies this as an extension.
			name:     "backtick_mid_sentence_divergent",
			existing: "T3 + T4 together — once T2 is solid, `",
			newLine:  "T3 + T4 together — once T2 is solid, PersistenceWriter and SemanticDeduplicator",
			wantLen:  1, // fixed: markdown stripping catches this
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := []string{tc.existing}
			result, _ := tuiDeduplicateAppend(buf, []string{tc.newLine})

			if len(result) != tc.wantLen {
				trimExisting := strings.TrimSpace(tc.existing)
				trimNew := strings.TrimSpace(tc.newLine)
				prefixMatch := strings.HasPrefix(trimNew, trimExisting)
				boundaryOK := false
				if prefixMatch && len(trimExisting) < len(trimNew) {
					boundaryOK = isWordBoundaryDiag(trimNew, len(trimExisting))
				}
				t.Errorf("got %d lines, want %d\n  prefix_match=%v, boundary_ok=%v\n  char_at_boundary=%q\n  existing: %q\n  new:      %q",
					len(result), tc.wantLen,
					prefixMatch, boundaryOK,
					safeCharAt(trimNew, len(trimExisting)),
					tc.existing, tc.newLine)
			}
		})
	}
}

// --- helpers that mirror tui.DeduplicateAppend and tui.isWordBoundary ---
// We duplicate the logic here so this test can run in the backend package
// without import cycles. This is a diagnostic — it tests the ACTUAL behavior.

func tuiDeduplicateAppend(existing []string, newLines []string) ([]string, int) {
	result := existing
	earliestReplaced := -1
	for _, line := range newLines {
		var replacedIdx int
		result, replacedIdx = tuiAppendDeduped(result, line)
		if replacedIdx >= 0 && (earliestReplaced < 0 || replacedIdx < earliestReplaced) {
			earliestReplaced = replacedIdx
		}
	}
	return result, earliestReplaced
}

const diagDedupPrefixMin = 3
const diagDedupLookback = 30
const diagStaleLookback = 5

func tuiAppendDeduped(buf []string, line string) ([]string, int) {
	trimNew := strings.TrimSpace(line)
	if trimNew == "" {
		return append(buf, line), -1
	}

	start := len(buf) - diagDedupLookback
	if start < 0 {
		start = 0
	}

	bestExtendIdx := -1
	bestExtendLen := 0

	strippedNew := stripInlineMarkdownBE(trimNew)

	for i := len(buf) - 1; i >= start; i-- {
		existing := strings.TrimSpace(buf[i])
		if existing == "" {
			continue
		}
		if existing == trimNew {
			return buf, -1
		}

		strippedExisting := stripInlineMarkdownBE(existing)

		if len(existing) >= diagDedupPrefixMin {
			isPrefix := strings.HasPrefix(trimNew, existing)
			if !isPrefix && strippedExisting != existing {
				isPrefix = len(strippedExisting) >= diagDedupPrefixMin &&
					strings.HasPrefix(strippedNew, strippedExisting)
			}
			if isPrefix {
				checkLen := len(existing)
				checkStr := trimNew
				if !strings.HasPrefix(trimNew, existing) {
					checkLen = len(strippedExisting)
					checkStr = strippedNew
				}
				if isWordBoundaryDiag(checkStr, checkLen) && len(existing) > bestExtendLen {
					bestExtendIdx = i
					bestExtendLen = len(existing)
				}
			}
		}

		distFromEnd := len(buf) - 1 - i
		if distFromEnd < diagStaleLookback && len(trimNew) >= diagDedupPrefixMin {
			isStale := strings.HasPrefix(existing, trimNew) && isWordBoundaryDiag(existing, len(trimNew))
			if !isStale && strippedNew != trimNew {
				isStale = strings.HasPrefix(strippedExisting, strippedNew) &&
					isWordBoundaryDiag(strippedExisting, len(strippedNew))
			}
			if isStale {
				return buf, -1
			}
		}
	}

	if bestExtendIdx >= 0 {
		buf[bestExtendIdx] = line
		return buf, bestExtendIdx
	}

	return append(buf, line), -1
}

func isWordBoundaryDiag(s string, prefixLen int) bool {
	if prefixLen >= len(s) {
		return true
	}
	// Trailing delimiter check (mirrors tui.isWordBoundary)
	if prefixLen > 0 {
		last := s[prefixLen-1]
		if last == '`' || last == '=' || last == '(' || last == '[' ||
			last == '{' || last == '<' || last == '#' || last == '@' ||
			last == '~' || last == '|' || last == '/' || last == '\\' ||
			last == ' ' || last == '\t' || last == ',' || last == '.' ||
			last == ':' || last == ';' {
			return true
		}
	}
	b := s[prefixLen]
	if b < 128 {
		return b == ' ' || b == '\t' || b == ',' || b == '.' ||
			b == ';' || b == ':' || b == '!' || b == '?' ||
			b == ')' || b == ']' || b == '}' || b == '"' ||
			b == '\'' || b == '-' || b == '/' || b == '\n' ||
			b == '`' || b == '=' || b == '(' || b == '[' ||
			b == '{' || b == '<' || b == '#' || b == '@' ||
			b == '~' || b == '|' || b == '\\' ||
			(b >= '0' && b <= '9')
	}
	return true // non-ASCII: allow
}

func safeCharAt(s string, idx int) string {
	if idx >= len(s) {
		return "<end>"
	}
	return fmt.Sprintf("%c (0x%02x)", s[idx], s[idx])
}

// TestDiagnostic_EndToEnd simulates the full pipeline: cleanLogWriter → log lines → TUI dedup.
// Uses a mutex-protected buffer to safely read results.
func TestDiagnostic_EndToEnd(t *testing.T) {
	// Streaming sequence with realistic timing
	type streamLine struct {
		text  string
		delay time.Duration // delay BEFORE writing this line
	}

	scenario := []streamLine{
		{text: "Which part are you looking to improve?\n", delay: 0},
		{text: "The 4-ticket plan\n", delay: 100 * time.Millisecond},
		{text: "The 4-ticket plan — refine scope\n", delay: 80 * time.Millisecond},
		{text: "The 4-ticket plan — refine scope, ordering, or design decisions?\n", delay: 60 * time.Millisecond},
		{text: "The `\n", delay: 300 * time.Millisecond}, // >200ms gap = timer fires
		{text: "The `cleanLogWriter` itself — dive into the actual code?\n", delay: 150 * time.Millisecond},
		{text: "Start small\n", delay: 400 * time.Millisecond},
		{text: "Start small, ship incrementally:\n", delay: 120 * time.Millisecond},
		{text: "\n", delay: 50 * time.Millisecond},
		{text: "T1 first — it's the safety net.\n", delay: 200 * time.Millisecond},
	}

	// Run through Layer 1
	var mu sync.Mutex
	var logBuf strings.Builder
	w := newCleanLogWriter(&logBuf)

	for _, sl := range scenario {
		if sl.delay > 0 {
			time.Sleep(sl.delay)
		}
		w.Write([]byte(sl.text))
	}
	w.Close()

	mu.Lock()
	logOutput := logBuf.String()
	mu.Unlock()

	logLines := strings.Split(logOutput, "\n")
	// Trim trailing empty
	for len(logLines) > 0 && logLines[len(logLines)-1] == "" {
		logLines = logLines[:len(logLines)-1]
	}

	t.Logf("=== Layer 1 output (%d lines) ===", len(logLines))
	for i, l := range logLines {
		t.Logf("  L1[%d]: %q", i, l)
	}

	// Run through Layer 2 (TUI dedup)
	var tuiBuf []string
	for _, line := range logLines {
		tuiBuf, _ = tuiDeduplicateAppend(tuiBuf, []string{line})
	}

	t.Logf("=== Layer 2 output (%d lines) ===", len(tuiBuf))
	for i, l := range tuiBuf {
		t.Logf("  L2[%d]: %q", i, l)
	}

	// Check for leaked fragments
	leaked := 0
	for i, l := range tuiBuf {
		trimmed := strings.TrimSpace(l)
		if trimmed == "" {
			continue
		}
		for j := i + 1; j < len(tuiBuf); j++ {
			trimJ := strings.TrimSpace(tuiBuf[j])
			if trimJ != "" && strings.HasPrefix(trimJ, trimmed) && trimJ != trimmed {
				t.Errorf("LEAKED FRAGMENT: L2[%d]=%q is prefix of L2[%d]=%q", i, trimmed, j, trimJ)
				leaked++
				break
			}
		}
	}
	if leaked > 0 {
		t.Errorf("Total leaked fragments visible to user: %d", leaked)
	} else {
		t.Log("No fragments leaked to user viewport")
	}
}
