package backend

import (
	"strconv"
	"strings"
)

// screenBufRows is the virtual screen height for detecting TUI redraws.
// Lines written to rows above this are tracked; redraws to the same row
// with the same content are suppressed.
const screenBufRows = 100

// ansiStripper is a byte-level state machine that strips ANSI escape sequences
// from raw PTY output and emits clean lines via a callback.
//
// It handles: ESC/CSI/OSC sequences, bare \r (clears line for spinners),
// and CUP/HVP cursor positioning from full-screen TUI redraws.
//
// Virtual screen buffer: Textual TUI apps redraw the entire screen on every
// update. The stripper maintains a virtual screen buffer so that when a CUP
// moves to a row that already has content, the line is only emitted if it
// actually changed. This prevents TUI redraws from producing duplicate lines.
//
// This is the shared core used by both cleanLogWriter (file-based log dedup)
// and rawBroadcaster (live streaming to TUI).
type ansiStripper struct {
	state    int    // 0=normal 1=esc 2=csi 3=osc 4=osc-esc
	lineBuf  []byte // current line accumulator
	csiParam []byte // accumulates CSI parameter bytes (digits, semicolons)
	sawCR    bool   // bare \r tracking
	onLine   func(string)

	// Virtual screen buffer for CUP-based redraw detection.
	// Maps row number → trimmed content last written to that row.
	// When CUP moves to a row and the new content matches, the line
	// is suppressed (it's a redraw, not new output).
	screen   [screenBufRows]string
	curRow   int  // current row from last CUP (0 = not in screen mode)
	inScreen bool // true after first CUP is seen (agent is using full-screen TUI)
}

// newAnsiStripper creates a stripper that calls onLine for each complete line.
func newAnsiStripper(onLine func(string)) *ansiStripper {
	return &ansiStripper{onLine: onLine}
}

// Write feeds raw PTY bytes through the state machine.
// Complete lines are delivered to onLine as they are detected.
func (s *ansiStripper) Write(p []byte) {
	for _, b := range p {
		switch s.state {
		case 0: // normal text
			switch {
			case b == 0x1b:
				s.sawCR = false
				s.state = 1 // start of escape sequence
			case b == '\n':
				s.sawCR = false
				s.flushLine()
			case b == '\r':
				s.sawCR = true
			case b >= 0x20 || b == '\t':
				if s.sawCR {
					s.lineBuf = s.lineBuf[:0]
					s.sawCR = false
				}
				s.lineBuf = append(s.lineBuf, b)
			}
		case 1: // after ESC
			switch b {
			case '[':
				s.csiParam = s.csiParam[:0] // reset CSI params
				s.state = 2                 // CSI sequence
			case ']':
				s.state = 3 // OSC sequence
			default:
				s.state = 0 // simple 2-char escape
			}
		case 2: // CSI: accumulate params, consume until final byte
			if (b >= '0' && b <= '9') || b == ';' || b == '?' {
				s.csiParam = append(s.csiParam, b)
			} else if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || b == '@' || b == '`' {
				s.handleCSIFinal(b)
				s.state = 0
			}
		case 3: // OSC: consume until BEL or ST
			switch b {
			case 0x07:
				s.state = 0
			case 0x1b:
				s.state = 4
			}
		case 4: // OSC after ESC — expecting '\' for ST
			s.state = 0
		}
	}
}

// handleCSIFinal processes the final byte of a CSI sequence.
func (s *ansiStripper) handleCSIFinal(final byte) {
	switch final {
	case 'H', 'f': // CUP (Cursor Position) / HVP
		if len(s.lineBuf) > 0 {
			s.flushLine()
		}
		// Parse row from CSI params: "\x1b[row;colH"
		// Default row is 1 if not specified
		row := 1
		params := string(s.csiParam)
		if params != "" {
			parts := strings.SplitN(params, ";", 2)
			if r, err := strconv.Atoi(parts[0]); err == nil && r > 0 {
				row = r
			}
		}
		s.curRow = row
		s.inScreen = true

	case 'J': // ED (Erase Display) — screen clear, reset virtual buffer
		if len(s.lineBuf) > 0 {
			s.flushLine()
		}
		// Clear virtual screen on full erase (ESC[2J or ESC[3J)
		param := string(s.csiParam)
		if param == "2" || param == "3" {
			s.screen = [screenBufRows]string{}
			s.curRow = 0
		}

	case 'K': // EL (Erase Line) — clear current line buffer
		s.lineBuf = s.lineBuf[:0]
	}
}

// Flush emits any remaining partial line.
func (s *ansiStripper) Flush() {
	if len(s.lineBuf) > 0 {
		s.flushLine()
	}
}

func (s *ansiStripper) flushLine() {
	line := strings.TrimRight(string(s.lineBuf), " \t")
	s.lineBuf = s.lineBuf[:0]

	// Strip trailing Textual sidebar characters (▌, ▎) that get concatenated
	// with content when the TUI renders text + sidebar on the same row.
	line = strings.TrimRight(line, " \t")
	for strings.HasSuffix(line, "▌") || strings.HasSuffix(line, "▎") {
		line = strings.TrimRight(strings.TrimSuffix(strings.TrimSuffix(line, "▌"), "▎"), " \t")
	}

	trimmed := strings.TrimSpace(line)

	// If we're in screen mode (agent is using full-screen TUI) and we have
	// a valid row, check if this line is a redraw of existing content.
	if s.inScreen && s.curRow > 0 && s.curRow <= screenBufRows {
		idx := s.curRow - 1 // 0-based
		existing := s.screen[idx]
		s.screen[idx] = trimmed

		// If the content at this row hasn't changed, suppress the line.
		// This is the key optimization: full-screen redraws output 50 lines
		// but only 1-2 actually change. We only emit the changed ones.
		if trimmed == existing && trimmed != "" {
			return
		}
	}

	s.onLine(line)
}
