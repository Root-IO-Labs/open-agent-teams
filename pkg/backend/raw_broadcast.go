package backend

import (
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	rawBroadcastRingSize = 500 // lines retained for catch-up
	subscriberChanSize   = 512 // per-subscriber channel buffer
	recentSetSize        = 200 // sliding window for full-screen redraw dedup
)

// rawBroadcaster strips ANSI from PTY output and broadcasts clean lines to
// subscribers. It applies two layers of dedup to handle the agent CLI's
// Textual TUI behavior:
//
//  1. Full-screen redraw dedup: when the Textual TUI redraws its screen via
//     cursor-positioning sequences, the ANSI stripper produces ~50 lines per
//     redraw — most of which were already emitted. A sliding-window set
//     suppresses lines seen in the recent past.
//
//  2. Progressive streaming dedup: when the LLM streams tokens, the TUI
//     redraws the current line with each new token ("It" → "It looks" →
//     "It looks like..."). A pending-line buffer collapses these into the
//     final complete line.
//
// It uses a ring buffer so new subscribers can catch up on recent output.
// Non-blocking sends prevent slow subscribers from back-pressuring the PTY reader.
type rawBroadcaster struct {
	mu       sync.Mutex
	stripper *ansiStripper

	// Ring buffer of recent lines (for subscriber catch-up)
	ring    [rawBroadcastRingSize]string
	ringPos int
	ringLen int // how many slots are filled (max = rawBroadcastRingSize)

	// Layer 1: Sliding-window dedup for full-screen redraws
	recentSet  map[string]struct{}
	recentRing [recentSetSize]string
	recentPos  int

	// Layer 2: Progressive streaming dedup (pending line buffer)
	pendingLine    string // original line (preserves whitespace)
	pendingTrimmed string // trimmed for comparison
	hasPending     bool
	lastEmitTime   time.Time // when the last line was emitted

	// Output hygiene state
	lastWasBlank bool // true if the last emitted line was blank

	// Startup suppression: only active when SeedPromptContent() is called.
	// Suppresses all output for 5 seconds after creation to skip the Textual
	// TUI rendering the system prompt on startup.
	startupSuppress bool      // true when SeedPromptContent was called
	createdAt       time.Time // when the broadcaster was created
	userInputSeen   bool      // true after "Thinking..." seen (marks end of startup)

	// Subscribers
	subs   map[uint64]chan string
	nextID uint64
	closed bool
}

// newRawBroadcaster creates a broadcaster. Call Write() with raw PTY bytes.
func newRawBroadcaster() *rawBroadcaster {
	b := &rawBroadcaster{
		subs:      make(map[uint64]chan string),
		recentSet: make(map[string]struct{}, recentSetSize),
		createdAt: time.Now(),
	}
	b.stripper = newAnsiStripper(b.handleLine)

	// Periodic flush: commit pending line every 500ms so it appears in the
	// stream during pauses (tool execution, thinking). Without this, the last
	// streaming fragment stays buffered until the next line arrives.
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			b.mu.Lock()
			if b.closed {
				b.mu.Unlock()
				return
			}
			b.commitPending()
			b.mu.Unlock()
		}
	}()

	return b
}

// Write feeds raw PTY bytes through the ANSI stripper.
// Thread-safe — called from the PTY reader goroutine.
func (b *rawBroadcaster) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return len(p), nil
	}
	b.stripper.Write(p)
	return len(p), nil
}

// SeedPromptContent pre-loads the system prompt (AGENTS.md) into the
// recentSet so that when the agent CLI renders the prompt on its Textual TUI
// at startup, those lines are immediately suppressed as "already seen".
// Also enables 5-second startup suppression to catch prompt content that
// doesn't exactly match the seeded text (e.g., markdown rendering differences).
// Call this after creating the broadcaster but before the agent starts writing.
func (b *rawBroadcaster) SeedPromptContent(promptText string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.startupSuppress = true
	for _, line := range strings.Split(promptText, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			b.recordRecent(trimmed)
		}
	}
}

// brailleSpinners are single-character spinner frames from Textual.
var brailleSpinners = map[rune]bool{
	'⠋': true, '⠙': true, '⠹': true, '⠸': true,
	'⠼': true, '⠴': true, '⠦': true, '⠧': true,
	'⠇': true, '⠏': true,
}

// scrollbarChars are Textual scrollbar indicator characters.
var scrollbarChars = map[rune]bool{
	'▁': true, '▂': true, '▃': true, '▄': true,
	'▅': true, '▆': true, '▇': true, '█': true,
}

// boxChars are Textual box-drawing frame characters.
var boxChars = map[rune]bool{
	'┌': true, '┐': true, '└': true, '┘': true,
	'├': true, '┤': true, '┬': true, '┴': true,
	'┼': true, '─': true, '│': true,
	'╔': true, '╗': true, '╚': true, '╝': true, '═': true, '║': true,
}

// reCountdown matches "(Ns, esc to interrupt)" countdown lines.
var reCountdown = regexp.MustCompile(`^\(\d+s, esc to interrupt\)$`)

// reTokenCount matches "N.NK tokens" or "NK tokens" lines.
var reTokenCount = regexp.MustCompile(`^\d+\.?\d*K? tokens$`)

// stripInlineMarkdownPrefix checks if `s` starts with `prefix` after stripping
// inline markdown characters (backticks, bold/italic markers). This handles
// LLM streaming where partial emissions include markdown chars at different
// positions than the final text (e.g., "start `" → "start BotServer").
func stripInlineMarkdownPrefix(s, prefix string) bool {
	stripped := stripInlineMarkdownBE(s)
	strippedPrefix := stripInlineMarkdownBE(prefix)
	if strippedPrefix == prefix {
		return false // no markdown in prefix — raw HasPrefix already failed
	}
	return len(strippedPrefix) > 0 && strings.HasPrefix(stripped, strippedPrefix)
}

// stripInlineMarkdownBE strips backticks, bold/italic markers (*_), and
// strikethrough (~~) for comparison purposes.
func stripInlineMarkdownBE(s string) string {
	if !strings.ContainsAny(s, "`*_~") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '`':
			// skip backticks
		case '*', '_':
			// skip bold/italic markers
		case '~':
			if i+1 < len(s) && s[i+1] == '~' {
				i++ // skip ~~ pair
				continue
			}
			b.WriteByte(s[i])
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// isAgentChrome returns true for lines that are agent CLI TUI chrome, not
// actual agent output. This is a comprehensive filter matching the TUI filter's
// CatChrome + CatProgress categories.
func isAgentChrome(trimmed string) bool {
	if trimmed == "" {
		return false
	}

	runes := []rune(trimmed)

	// Single-character spinner frames (braille)
	if len(runes) == 1 && brailleSpinners[runes[0]] {
		return true
	}

	// Scrollbar-only lines (all scrollbar chars + whitespace)
	if isAllCharsFrom(runes, scrollbarChars) {
		return true
	}

	// Box-drawing-only lines (all box chars + whitespace)
	if isAllCharsFrom(runes, boxChars) {
		return true
	}

	// Textual sidebar/notification panels
	if strings.HasPrefix(trimmed, "▎") || strings.HasPrefix(trimmed, "▌") {
		return true
	}

	// Textual frame characters at start of line
	if strings.HasPrefix(trimmed, "│") || strings.HasPrefix(trimmed, "└") ||
		strings.HasPrefix(trimmed, "┌") || strings.HasPrefix(trimmed, "┐") ||
		strings.HasPrefix(trimmed, "┘") {
		return true
	}

	// Countdown timers: (0s, esc to interrupt)
	if reCountdown.MatchString(trimmed) {
		return true
	}

	// Token count lines: "16.7K tokens"
	if reTokenCount.MatchString(trimmed) {
		return true
	}

	// Model selector and mode indicators
	if strings.HasPrefix(trimmed, "anthropic:") || strings.HasPrefix(trimmed, "openai:") ||
		strings.HasPrefix(trimmed, "google") || strings.Contains(trimmed, "auto | shift+tab to cycle") ||
		strings.HasPrefix(trimmed, "Enter send") {
		return true
	}

	// Lines ending with ▌ are content + Textual sidebar character.
	// Strip the trailing sidebar char so the content can be evaluated cleanly.
	// If the line is JUST sidebar + spaces, it's chrome.
	if strings.HasSuffix(trimmed, "▌") {
		inner := strings.TrimSpace(strings.TrimSuffix(trimmed, "▌"))
		if inner == "" {
			return true
		}
	}

	// Echoed system prompt content (AGENTS.md rendered with "> " prefix)
	if strings.HasPrefix(trimmed, "> ##") || strings.HasPrefix(trimmed, "> ---") ||
		strings.HasPrefix(trimmed, "> ```") || trimmed == ">" {
		return true
	}

	// Standalone short noise from Textual partial renders
	if len(trimmed) <= 2 && !strings.ContainsAny(trimmed, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789>*-") {
		return true
	}

	// Version strings
	if len(trimmed) < 10 && strings.HasPrefix(trimmed, "v") && strings.Contains(trimmed, ".") {
		return true
	}

	// Banner ASCII art (lines of box-drawing or block chars ≥ 10 chars)
	if len(runes) >= 10 {
		artChars := 0
		for _, r := range runes {
			if boxChars[r] || scrollbarChars[r] || r == ' ' {
				artChars++
			}
		}
		if artChars > len(runes)*80/100 {
			return true // >80% art characters = banner/chrome
		}
	}

	// Thread/session IDs
	if strings.HasPrefix(trimmed, "Thread:") || strings.HasPrefix(trimmed, "Starting with thread:") {
		return true
	}

	// Textual prompt chrome
	if strings.Contains(trimmed, "What would you like to build?") ||
		strings.Contains(trimmed, "by [green]Root.io[/green]") ||
		strings.Contains(trimmed, "by Root.io") {
		return true
	}

	// Python runtime warnings
	if strings.Contains(trimmed, "RuntimeWarning:") || strings.HasPrefix(trimmed, "<frozen") {
		return true
	}

	return false
}

// isAllCharsFrom returns true if all non-space runes in the slice are in the charSet.
func isAllCharsFrom(runes []rune, charSet map[rune]bool) bool {
	nonSpace := 0
	for _, r := range runes {
		if r == ' ' || r == '\t' {
			continue
		}
		if !charSet[r] {
			return false
		}
		nonSpace++
	}
	return nonSpace > 0
}

// handleLine is the ansiStripper callback — delivers one clean line.
// Called under b.mu.
func (b *rawBroadcaster) handleLine(line string) {
	trimmed := strings.TrimSpace(line)

	// --- Blank line handling ---
	if trimmed == "" {
		if b.lastWasBlank {
			return // collapse consecutive blanks
		}
		b.lastWasBlank = true
		b.commitPending()
		b.emit(line)
		return
	}
	b.lastWasBlank = false

	// --- Layer 0a: Agent CLI chrome filter ---
	// Textual sidebar panels, system prompt headers, and rendered slash
	// command documentation are always suppressed regardless of dedup state.
	if isAgentChrome(trimmed) {
		return
	}

	// --- Layer 0b: Startup suppression ---
	// When SeedPromptContent was called, suppress output that matches seeded
	// content during the first 5 seconds (Textual TUI startup). Lines NOT
	// in the recentSet pass through — they're new content (e.g., agent responses).
	// After 5 seconds or after seeing real agent output, suppression ends.
	if b.startupSuppress && !b.userInputSeen {
		if strings.HasPrefix(trimmed, "Thinking") || strings.HasPrefix(trimmed, "(*) ") ||
			strings.HasPrefix(trimmed, "⏺ ") || strings.HasPrefix(trimmed, "● ") {
			b.userInputSeen = true
			// Fall through to emit this line
		} else if time.Since(b.createdAt) < 5*time.Second {
			// During startup, only suppress lines in the seeded set.
			// Genuinely new lines (agent output) pass through.
			if _, seeded := b.recentSet[trimmed]; seeded {
				return
			}
			// Not seeded — this is new content, let it through
		} else {
			b.userInputSeen = true
		}
	}

	// --- Layer 1: Full-screen redraw dedup ---
	// If we've seen this exact line recently, it's a TUI redraw. Skip it.
	// Exception: if this line extends the current pending line (Layer 2),
	// we need to let it through to update the pending buffer.
	if _, seen := b.recentSet[trimmed]; seen {
		// But check if it's a progressive extension of pending
		extends := b.hasPending && strings.HasPrefix(trimmed, b.pendingTrimmed) && trimmed != b.pendingTrimmed
		if !extends && b.hasPending {
			// Try markdown-stripped comparison
			extends = stripInlineMarkdownPrefix(trimmed, b.pendingTrimmed) && trimmed != b.pendingTrimmed
		}
		if !extends {
			return // seen before, suppress
		}
	}

	// --- Layer 2: Progressive streaming dedup ---
	if b.hasPending && b.pendingTrimmed != "" {
		// New line extends pending → update pending (streaming token).
		// Check both raw and markdown-stripped to handle backtick shifting.
		if strings.HasPrefix(trimmed, b.pendingTrimmed) || stripInlineMarkdownPrefix(trimmed, b.pendingTrimmed) {
			b.pendingLine = line
			b.pendingTrimmed = trimmed
			return
		}
		// New line is shorter version of pending → skip (cursor redraw)
		if strings.HasPrefix(b.pendingTrimmed, trimmed) || stripInlineMarkdownPrefix(b.pendingTrimmed, trimmed) {
			return
		}
	}

	// New line is unrelated to pending — commit pending first, then buffer new
	b.commitPending()
	b.pendingLine = line
	b.pendingTrimmed = trimmed
	b.hasPending = true
}

// commitPending writes the buffered pending line to the ring buffer and
// broadcasts it to subscribers.
func (b *rawBroadcaster) commitPending() {
	if !b.hasPending {
		return
	}
	b.recordRecent(b.pendingTrimmed)
	b.emit(b.pendingLine)
	b.hasPending = false
	b.pendingLine = ""
	b.pendingTrimmed = ""
}

// emit writes a line to the ring buffer and broadcasts to subscribers.
func (b *rawBroadcaster) emit(line string) {
	// Store in ring buffer
	idx := b.ringPos % rawBroadcastRingSize
	b.ring[idx] = line
	b.ringPos++
	if b.ringLen < rawBroadcastRingSize {
		b.ringLen++
	}
	b.lastEmitTime = time.Now()

	// Broadcast to all subscribers (non-blocking)
	for _, ch := range b.subs {
		select {
		case ch <- line:
		default:
			// Drop line if subscriber is full — prevents PTY reader back-pressure
		}
	}
}

// recordRecent adds a trimmed line to the sliding dedup window.
func (b *rawBroadcaster) recordRecent(trimmed string) {
	if trimmed == "" {
		return
	}
	idx := b.recentPos % recentSetSize
	if old := b.recentRing[idx]; old != "" {
		delete(b.recentSet, old)
	}
	b.recentRing[idx] = trimmed
	b.recentSet[trimmed] = struct{}{}
	b.recentPos++
}

// Subscribe returns a channel that receives stripped output lines.
// The channel is pre-filled with recent lines from the ring buffer.
// Call cancel() to unsubscribe (closes the returned channel).
func (b *rawBroadcaster) Subscribe() (id uint64, ch <-chan string, cancel func()) {
	return b.subscribe(false)
}

// SubscribeLive returns a channel that receives only new lines going forward.
// No ring buffer catch-up. Use when the caller already has prior content.
func (b *rawBroadcaster) SubscribeLive() (id uint64, ch <-chan string, cancel func()) {
	return b.subscribe(true)
}

func (b *rawBroadcaster) subscribe(liveOnly bool) (id uint64, ch <-chan string, cancel func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	subCh := make(chan string, subscriberChanSize)
	id = b.nextID
	b.nextID++
	b.subs[id] = subCh

	// Catch-up: send recent lines from ring buffer (unless liveOnly)
	if !liveOnly && b.ringLen > 0 {
		start := b.ringPos - b.ringLen
		for i := start; i < b.ringPos; i++ {
			idx := i % rawBroadcastRingSize
			select {
			case subCh <- b.ring[idx]:
			default:
				break // channel full during catch-up, skip remaining
			}
		}
	}

	cancel = func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(subCh)
		}
	}

	return id, subCh, cancel
}

// Close unsubscribes all subscribers and closes their channels.
func (b *rawBroadcaster) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	// Flush any partial line
	b.stripper.Flush()
	b.commitPending()
	for sid, ch := range b.subs {
		delete(b.subs, sid)
		close(ch)
	}
}
