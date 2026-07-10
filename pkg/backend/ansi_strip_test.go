package backend

import (
	"strings"
	"testing"
)

// TestAnsiStripper_CapsRunawayLine pins Phase 7 item 4: a single PTY line far
// larger than maxAnsiLineBytes (e.g. a base64 screenshot with no newlines) must
// NOT grow lineBuf without bound; it is truncated at the cap with a one-time
// marker and further bytes are dropped until the next newline. This is the OOM
// guard for John's screenshot-heavy runs.
func TestAnsiStripper_CapsRunawayLine(t *testing.T) {
	orig := maxAnsiLineBytes
	maxAnsiLineBytes = 1024 // shrink for a fast test
	defer func() { maxAnsiLineBytes = orig }()

	var got []string
	s := newAnsiStripper(func(line string) { got = append(got, line) })

	// 1 MiB of 'A' as a single line, then a newline.
	huge := strings.Repeat("A", 1<<20)
	s.Write([]byte(huge + "\n"))

	if len(got) != 1 {
		t.Fatalf("expected exactly 1 emitted line, got %d", len(got))
	}
	line := got[0]
	// Emitted line must be bounded (~cap + marker), NOT the full 1 MiB.
	if len(line) > maxAnsiLineBytes+len(ansiTruncationMarker)+8 {
		t.Errorf("emitted line length %d exceeds bounded size ~%d; cap not enforced", len(line), maxAnsiLineBytes)
	}
	if !strings.Contains(line, "OAT: line truncated") {
		t.Errorf("truncated line missing marker: %.80q...", line)
	}
}

// TestAnsiStripper_ShortLineNotTruncated guards the false-positive direction:
// a normal-sized line (the shape of an [OAT_TOKENS] sentinel the capacity meter
// parses) must pass through byte-for-byte with no marker.
func TestAnsiStripper_ShortLineNotTruncated(t *testing.T) {
	var got []string
	s := newAnsiStripper(func(line string) { got = append(got, line) })

	const tokenLine = `[OAT_TOKENS] {"cumulative_input":12345,"context_input":12345}`
	s.Write([]byte(tokenLine + "\n"))

	if len(got) != 1 || got[0] != tokenLine {
		t.Fatalf("short line altered: got %q, want %q", got, tokenLine)
	}
	if strings.Contains(got[0], "truncated") {
		t.Errorf("short line was wrongly truncated: %q", got[0])
	}
}

// TestAnsiStripper_CapResetsBetweenLines ensures the truncation state is
// per-line: after a runaway line is capped, the NEXT line is accumulated
// normally (the flag reset on newline works).
func TestAnsiStripper_CapResetsBetweenLines(t *testing.T) {
	orig := maxAnsiLineBytes
	maxAnsiLineBytes = 512
	defer func() { maxAnsiLineBytes = orig }()

	var got []string
	s := newAnsiStripper(func(line string) { got = append(got, line) })

	s.Write([]byte(strings.Repeat("X", 4096) + "\n"))
	s.Write([]byte("hello after overflow\n"))

	if len(got) != 2 {
		t.Fatalf("expected 2 lines, got %d: %q", len(got), got)
	}
	if got[1] != "hello after overflow" {
		t.Errorf("line after overflow corrupted: %q", got[1])
	}
}
