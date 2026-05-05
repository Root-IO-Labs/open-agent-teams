package tui

import (
	"bufio"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// TypedLine is a line with optional metadata from the daemon.
// When LineType is set (from SocketStream), the TUI can skip regex classification.
// When empty (from LogStream fallback), the TUI falls back to Classify().
type TypedLine struct {
	Text     string
	LineType string // "tool_call", "tool_output", "thinking", "system", "user_input", "text", or "" (unknown)
}

// OutputStreamI is the interface for output streams. Both LogStream (file-based)
// and SocketStream (live daemon connection) implement this.
type OutputStreamI interface {
	Lines() <-chan string
	TypedLines() <-chan TypedLine
	IsClosed() bool
	Stop()
}

// LogStream tails an agent's log file and sends new lines to a channel.
// It handles basic filtering but does NOT dedup progressive streaming —
// that's handled at the display buffer level in App where we can look backwards.
type LogStream struct {
	path   string
	lines  chan string
	done   chan struct{}
	once   sync.Once
	filter *OutputFilter
	closed atomic.Bool // set when the goroutine exits
}

// NewLogStream creates a stream that tails the given log file.
func NewLogStream(path string, filter *OutputFilter) *LogStream {
	return &LogStream{
		path:   path,
		lines:  make(chan string, 256),
		done:   make(chan struct{}),
		filter: filter,
	}
}

// Lines returns the channel of new output lines.
func (s *LogStream) Lines() <-chan string {
	return s.lines
}

// TypedLines returns nil — LogStream doesn't have daemon metadata.
// The TUI falls back to regex classification for file-tailed output.
func (s *LogStream) TypedLines() <-chan TypedLine { return nil }

// IsClosed returns true if the stream goroutine has exited.
func (s *LogStream) IsClosed() bool {
	return s.closed.Load()
}

// Start begins tailing the log file in a goroutine.
func (s *LogStream) Start(catchUp bool) {
	go s.run(catchUp)
}

// Stop signals the stream to stop.
func (s *LogStream) Stop() {
	s.once.Do(func() {
		close(s.done)
	})
}

func (s *LogStream) run(catchUp bool) {
	defer func() {
		s.closed.Store(true)
		close(s.lines)
	}()

	// Retry opening the file — it may not exist yet if the agent just started.
	// Try every 500ms for up to 30 seconds before giving up.
	var f *os.File
	for attempts := 0; attempts < 60; attempts++ {
		var err error
		f, err = os.Open(s.path)
		if err == nil {
			break
		}
		select {
		case <-s.done:
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
	if f == nil {
		return // gave up after 30s
	}
	defer f.Close()

	if !catchUp {
		_, _ = f.Seek(0, io.SeekEnd) // best-effort tail; on failure we start at 0
	} else {
		// Read last 4KB on catchup — enough for recent context without
		// dumping too much history that overwhelms dedup on first render.
		if info, statErr := f.Stat(); statErr == nil && info.Size() > 4096 {
			_, _ = f.Seek(-4096, io.SeekEnd) // best-effort; fallthrough reads from 0
		}
	}

	reader := bufio.NewReader(f)
	if catchUp {
		// Skip the first (likely partial) line after a mid-file seek.
		_, _ = reader.ReadString('\n')
	}

	// Stream-side progressive dedup: the log file may contain streaming
	// fragments ("It" → "It looks" → "It looks like your message...")
	// committed by the cleanLogWriter's periodic flush. We buffer the last
	// non-blank line and only emit it when the next non-blank line does NOT
	// extend it. Blank lines between fragments are deferred (the agent TUI
	// often inserts blank lines between progressive streaming chunks).
	var pendingLine string
	var pendingTrimmed string
	hasPending := false
	deferredBlanks := 0

	emitPending := func() {
		if !hasPending {
			return
		}
		// Emit any deferred blank lines before the pending content
		for i := 0; i < deferredBlanks; i++ {
			select {
			case s.lines <- "":
			case <-s.done:
				return
			}
		}
		deferredBlanks = 0
		select {
		case s.lines <- pendingLine:
		case <-s.done:
			return
		}
		hasPending = false
		pendingLine = ""
		pendingTrimmed = ""
	}

	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			line = strings.TrimRight(line, "\r\n")

			// Apply category filter
			if s.filter != nil && !s.filter.FilterLine(line) {
				continue
			}

			trimmed := strings.TrimSpace(line)

			// Blank lines: defer them while we have a pending line
			// (the next non-blank may extend the pending, in which case
			// the blanks were just TUI redraw artifacts)
			if trimmed == "" {
				if hasPending {
					deferredBlanks++
				} else {
					select {
					case s.lines <- line:
					case <-s.done:
						return
					}
				}
				continue
			}

			// Progressive dedup: if new line extends pending, replace pending.
			// Compare both raw and markdown-stripped versions to handle
			// streaming fragments where backticks shift position.
			if hasPending && pendingTrimmed != "" {
				extends := strings.HasPrefix(trimmed, pendingTrimmed)
				if !extends {
					strippedPending := stripInlineMarkdown(pendingTrimmed)
					strippedNew := stripInlineMarkdown(trimmed)
					extends = strippedPending != pendingTrimmed &&
						strings.HasPrefix(strippedNew, strippedPending)
				}
				if extends {
					pendingLine = line
					pendingTrimmed = trimmed
					deferredBlanks = 0 // blanks between fragments are artifacts
					continue
				}
				// If pending extends new line (redraw of shorter version), skip new
				isStale := strings.HasPrefix(pendingTrimmed, trimmed)
				if !isStale {
					strippedPending := stripInlineMarkdown(pendingTrimmed)
					strippedNew := stripInlineMarkdown(trimmed)
					isStale = strippedNew != trimmed &&
						strings.HasPrefix(strippedPending, strippedNew)
				}
				if isStale && len(trimmed) > 2 {
					continue
				}
			}

			// New line doesn't extend pending — emit pending, buffer new
			emitPending()
			pendingLine = line
			pendingTrimmed = trimmed
			hasPending = true
		}

		if err != nil {
			// At EOF — emit pending line (it's complete since no more data)
			emitPending()
			deferredBlanks = 0

			select {
			case <-s.done:
				return
			case <-time.After(25 * time.Millisecond):
				reader.Reset(f)
			}
		}
	}
}
