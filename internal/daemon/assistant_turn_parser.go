package daemon

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// AssistantTurn is the structured form of one ASSISTANT block extracted
// from an agent's OAT_TOOL_LOG. It carries the sanitized text (control
// chars + ANSI escapes stripped) plus a heuristic-detected `Kind` that
// the side panel uses to pick a render style.
//
// Kind is the same vocabulary as `browser_emit_to_user`:
//   - "final"    — normal left-aligned chat bubble (default)
//   - "question" — dotted-border bubble, hints "agent is waiting on you"
//
// We deliberately do NOT auto-emit "progress" here: a progress ping is
// supposed to render as the activity-indicator line, not a chat bubble,
// and that distinction only matters when the model explicitly asked for
// it via `browser_emit_to_user`. Falling back to "final" for everything
// auto-detected keeps the bubble vs activity-line boundary clean.
type AssistantTurn struct {
	// SanitizedText is the body of the ASSISTANT block, with all C0
	// controls (except \n and \t), C1 controls, and ANSI escape
	// sequences stripped. Mirrors `sanitizeEmitText` in the bridge so
	// auto-emitted turns and tool-emitted turns get identical scrub
	// rules.
	SanitizedText string

	// Kind is the render hint: "final" or "question".
	Kind string
}

// emitTurnMaxBytes caps how large a single auto-emitted turn can be on
// the wire. Mirrors the 64 KiB limit `browser_emit_to_user` enforces
// (bridge/src/emit-to-user.ts: EMIT_MAX_TEXT_BYTES) so the two paths
// have identical safety properties. Oversize turns are truncated; a
// suffix marker tells the side panel the text was clipped.
const emitTurnMaxBytes = 64 * 1024

// turnHeaderRE matches the ASSISTANT block header written by the agent
// runtime's OAT_TOOL_LOG hook. Format: "[HH:MM:SS] ASSISTANT:".
// We intentionally accept any HH:MM:SS so the parser doesn't break if
// the runtime ever switches to 24h ISO format.
var turnHeaderRE = regexp.MustCompile(`^\[\d{1,2}:\d{2}:\d{2}\] ASSISTANT:\s*$`)

// userHeaderRE matches a USER block header — symmetric to
// turnHeaderRE. The parser uses these to detect when the side panel's
// `[SIDE-PANEL CHAT]` sentinel has arrived, which gates whether
// subsequent ASSISTANT turns auto-emit (see Part 2g post-smoke fix).
var userHeaderRE = regexp.MustCompile(`^\[\d{1,2}:\d{2}:\d{2}\] USER:\s*$`)

// nextMarkerRE matches the start of any other structured log marker:
// USER:, TOOL:, RESULT:, ERROR:, etc.; or any [OAT_*] envelope; or
// any other timestamped block header. Used to detect the end of an
// ASSISTANT body without having to enumerate every marker the runtime
// might emit. Anchored to start-of-line because indented body lines
// can legitimately contain "[HH:MM:SS]" if the model echoes a
// timestamp.
var nextMarkerRE = regexp.MustCompile(`^(\[\d{1,2}:\d{2}:\d{2}\] [A-Z]+:|\[OAT_[A-Z_]+\])`)

// sidePanelSentinelBody is the literal text the daemon prepends to
// side-panel user input via handleAgentInput. Mirrors
// `sidePanelInputSentinel` in assistant_turn_lifecycle.go — keep in
// sync if that constant ever changes.
const sidePanelSentinelBody = "[SIDE-PANEL CHAT]"

// oatBrowserStatusPrefix marks the agent's status-reporting sentinel.
// The browser.md prompt instructs the agent to emit
// `[OAT_BROWSER] status: <msg>` lines for the daemon's OutputWatcher;
// they are NOT meant for the user. We strip these from auto-emitted
// chat bodies (and skip the turn entirely if the body is JUST a
// status sentinel).
const oatBrowserStatusPrefix = "[OAT_BROWSER] status:"

// EventKind enumerates the events the streaming parser surfaces. The
// tailer consumes the event stream so it can flip side-panel mode on
// based on USER sentinel arrival, in addition to publishing ASSISTANT
// turns.
type EventKind int

const (
	// EventAssistantTurn carries a parsed ASSISTANT block. Look at
	// the AssistantTurn embedded in the Event for body + kind.
	EventAssistantTurn EventKind = iota
	// EventSidePanelUser fires when a USER block begins with the
	// `[SIDE-PANEL CHAT]` sentinel. Carries no payload — the tailer
	// only needs to know "the user just spoke to me" to flip its
	// gating flag.
	EventSidePanelUser
)

// Event is one item produced by the streaming parser. Exactly one of
// the kind-specific payload fields is meaningful per the Kind value.
type Event struct {
	Kind EventKind
	Turn AssistantTurn
}

// parseEvents extracts ordered Events (USER side-panel sentinels +
// ASSISTANT turns) from a slice of log lines. The tailer drives its
// side-panel-mode gating from this ordering.
//
// Block kinds the parser recognizes:
//   - `[HH:MM:SS] USER:` block — examined for a `[SIDE-PANEL CHAT]`
//     prefix in the body's first non-blank line. If present, an
//     EventSidePanelUser is emitted.
//   - `[HH:MM:SS] ASSISTANT:` block — body is sanitized, scrubbed of
//     `[OAT_BROWSER] status:` lines (those are OutputWatcher sentinels,
//     not chat content), and emitted as an EventAssistantTurn iff
//     non-empty after scrubbing.
//
// Pure function: []string in → []Event out, no I/O, no goroutines.
func parseEvents(lines []string) []Event {
	var out []Event
	var bodyBuf strings.Builder
	type blockKind int
	const (
		none blockKind = iota
		assistantBlock
		userBlock
	)
	current := none

	flushAssistant := func() {
		raw := strings.Trim(bodyBuf.String(), "\n")
		bodyBuf.Reset()
		// Strip `[OAT_BROWSER] status:` lines line-by-line. These are
		// the agent's status sentinels for the daemon OutputWatcher;
		// surfacing them as chat would leak internals to the user.
		// `[OAT_TOKENS]` envelopes are filtered the same way for
		// belt-and-braces — they're already terminated by nextMarkerRE
		// in the line loop, but if a malformed/inline one slipped
		// into a body we drop it here.
		var cleaned strings.Builder
		for _, ln := range strings.Split(raw, "\n") {
			trim := strings.TrimSpace(ln)
			if strings.HasPrefix(trim, oatBrowserStatusPrefix) {
				continue
			}
			if strings.HasPrefix(trim, "[OAT_TOKENS]") {
				continue
			}
			cleaned.WriteString(ln)
			cleaned.WriteByte('\n')
		}
		sanitized := sanitizeEmitText(strings.Trim(cleaned.String(), "\n"))
		sanitized = truncateUTF8(sanitized, emitTurnMaxBytes)
		if sanitized == "" {
			return
		}
		out = append(out, Event{
			Kind: EventAssistantTurn,
			Turn: AssistantTurn{
				SanitizedText: sanitized,
				Kind:          classifyTurnKind(sanitized),
			},
		})
	}

	flushUser := func() {
		raw := strings.Trim(bodyBuf.String(), "\n")
		bodyBuf.Reset()
		// USER blocks themselves are not surfaced as turns — we only
		// care whether the body begins with the side-panel sentinel.
		// Walk lines to find the first non-blank one (the daemon
		// strips ANSI before delivering, so the sentinel will be at
		// the start of the first content line).
		for _, ln := range strings.Split(raw, "\n") {
			trim := strings.TrimSpace(ln)
			if trim == "" {
				continue
			}
			if strings.HasPrefix(trim, sidePanelSentinelBody) {
				out = append(out, Event{Kind: EventSidePanelUser})
			}
			break
		}
	}

	flush := func() {
		switch current {
		case assistantBlock:
			flushAssistant()
		case userBlock:
			flushUser()
		}
		current = none
	}

	for _, line := range lines {
		if turnHeaderRE.MatchString(line) {
			flush()
			current = assistantBlock
			continue
		}
		if userHeaderRE.MatchString(line) {
			flush()
			current = userBlock
			continue
		}
		if current == none {
			continue
		}
		// Any other structured marker terminates the current body.
		if nextMarkerRE.MatchString(line) {
			flush()
			continue
		}
		// Body lines from the runtime hook are indented with two
		// spaces; strip the prefix so the visible text is flush-left.
		// We tolerate either two-space or no-indent so log-format
		// drift doesn't silently swallow content. Empty lines are
		// preserved as blank lines in the body.
		if strings.HasPrefix(line, "  ") {
			bodyBuf.WriteString(line[2:])
			bodyBuf.WriteByte('\n')
			continue
		}
		if line == "" {
			bodyBuf.WriteByte('\n')
			continue
		}
		// Non-indented, non-marker line: treat as accidental
		// continuation rather than discarding (runtime sometimes
		// emits unindented continuation lines from rich text).
		bodyBuf.WriteString(line)
		bodyBuf.WriteByte('\n')
	}
	flush()
	return out
}

// parseAssistantTurns is a thin wrapper around parseEvents that
// returns only ASSISTANT turns. Kept for tests written against the
// older API and for any caller that doesn't care about USER events.
// New callers should prefer parseEvents.
func parseAssistantTurns(lines []string) []AssistantTurn {
	events := parseEvents(lines)
	var out []AssistantTurn
	for _, ev := range events {
		if ev.Kind == EventAssistantTurn {
			out = append(out, ev.Turn)
		}
	}
	return out
}

// sanitizeEmitText strips C0 controls (except \n and \t), C1 controls,
// and ANSI escape sequences from text. Byte-for-byte equivalent to the
// bridge's TS sanitizeEmitText (bridge/src/emit-to-user.ts) — keeping
// the two implementations in sync is a documented invariant of Part 2g.
func sanitizeEmitText(text string) string {
	var out strings.Builder
	out.Grow(len(text))

	i := 0
	for i < len(text) {
		b := text[i]
		// ESC starts an ANSI sequence — drop until we exit the
		// envelope. Conservative: anything after ESC up to the
		// terminator is discarded.
		if b == 0x1b {
			i = skipAnsiEscape(text, i)
			continue
		}
		// C0 controls (0x00..0x1F): drop except \n and \t.
		if b < 0x20 {
			if b == '\n' || b == '\t' {
				out.WriteByte(b)
			}
			i++
			continue
		}
		// C1 controls (0x80..0x9F): drop. Note these are encoded
		// as 2-byte UTF-8 sequences in the wild (0xC2 + 0x80..0x9F).
		if b == 0xc2 && i+1 < len(text) {
			next := text[i+1]
			if next >= 0x80 && next <= 0x9f {
				i += 2
				continue
			}
		}
		out.WriteByte(b)
		i++
	}
	return out.String()
}

// skipAnsiEscape consumes one ANSI escape sequence beginning at i (where
// text[i] == 0x1b) and returns the index one past the terminator.
//
// Recognized shapes:
//   - CSI: ESC '[' params* final-byte (0x40..0x7E)
//   - OSC: ESC ']' ... ST or BEL
//   - SS2/SS3/single-char: ESC 'N'|'O'|7-bit char — consume exactly 2 bytes
//
// Anything malformed falls back to "consume the ESC and one trailing
// byte" so we never get stuck.
func skipAnsiEscape(text string, i int) int {
	if i >= len(text) {
		return i
	}
	// Skip the ESC byte itself.
	i++
	if i >= len(text) {
		return i
	}
	c := text[i]
	switch c {
	case '[':
		// CSI: parameters and intermediates, then a final byte.
		i++
		for i < len(text) {
			b := text[i]
			i++
			if b >= 0x40 && b <= 0x7e {
				return i
			}
		}
		return i
	case ']', 'P', 'X', '^', '_':
		// OSC / DCS / SOS / PM / APC: terminated by ST (ESC \) or BEL (0x07).
		i++
		for i < len(text) {
			b := text[i]
			if b == 0x07 {
				return i + 1
			}
			if b == 0x1b && i+1 < len(text) && text[i+1] == '\\' {
				return i + 2
			}
			i++
		}
		return i
	default:
		// Single-char escape; consume the byte.
		return i + 1
	}
}

// truncateUTF8 returns text clipped to at most maxBytes bytes, never
// splitting a UTF-8 code point and appending a "[…truncated]" suffix
// when truncation actually happened. The suffix itself counts against
// the cap.
func truncateUTF8(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	const suffix = "\n[…truncated]"
	keep := maxBytes - len(suffix)
	if keep < 0 {
		keep = 0
	}
	for keep > 0 && !utf8.RuneStart(text[keep]) {
		keep--
	}
	return text[:keep] + suffix
}

// classifyTurnKind applies the question-detection heuristic from Part
// 2g. Returns "question" when the body's last non-empty sentence ends
// in "?" AND contains a second-person pronoun or clarification
// signaller; "final" otherwise.
//
// Examples that qualify as "question":
//   - "Could you tell me your preferred format?"
//   - "Should I open the pricing page or the docs?"
//   - "What would you like me to do?"
//
// Examples that don't (rhetorical / model self-talk):
//   - "Is the moon made of cheese? Obviously not."
//   - "I checked: no messages."
func classifyTurnKind(body string) string {
	trimmed := strings.TrimRight(body, " \t\r\n")
	if trimmed == "" || !strings.HasSuffix(trimmed, "?") {
		return "final"
	}
	// Look only at the last sentence — splits on ".", "!", "?" or
	// blank line boundary. Walking backward avoids a regex over the
	// whole body for what's usually a short reply.
	end := len(trimmed)
	start := end - 1
	for start > 0 {
		r := trimmed[start-1]
		if r == '.' || r == '!' || r == '?' || r == '\n' {
			break
		}
		start--
	}
	lastSentence := strings.ToLower(strings.TrimSpace(trimmed[start:end]))
	// Must contain at least one second-person / clarification signal.
	signals := []string{
		" you ",
		" your ",
		" you?",
		"could you",
		"would you",
		"should i",
		"shall i",
		"want me to",
		"do you",
		"which ",
		"what ",
		"where ",
		"when ",
		"how ",
		"why ",
	}
	padded := " " + lastSentence
	for _, s := range signals {
		if strings.Contains(padded, s) {
			return "question"
		}
	}
	return "final"
}
