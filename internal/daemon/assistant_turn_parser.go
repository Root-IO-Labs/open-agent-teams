package daemon

import (
	"encoding/json"
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

// toolHeaderRE matches a TOOL block header and captures the tool name.
// The runtime writes `[HH:MM:SS] TOOL: <name>` (textual_adapter.py
// log_tool_call), so unlike USER/ASSISTANT the name sits ON the header
// line. Used to surface per-tool activity rows for the side panel when
// chatting with an ASSISTANT (whose tool calls are NOT mediated by the
// bridge's MCP server, so they have no onToolStart hook — the log is
// the only signal). The trailing `.*` is non-greedy-safe because the
// name is a single token followed by EOL.
var toolHeaderRE = regexp.MustCompile(`^\[\d{1,2}:\d{2}:\d{2}\] TOOL:\s*(\S.*?)\s*$`)

// resultHeaderRE matches a RESULT block header and captures the name +
// optional status. The runtime writes `[HH:MM:SS] RESULT: <name>` on
// success and `[HH:MM:SS] RESULT: <name> (<status>)` otherwise
// (log_tool_result). We treat any parenthesised suffix as a non-success
// status so the side panel can flag the row red.
var resultHeaderRE = regexp.MustCompile(`^\[\d{1,2}:\d{2}:\d{2}\] RESULT:\s*(\S.*?)\s*$`)

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

// echoedSidePanelPrefixRE matches a `[SIDE-PANEL CHAT]` sentinel (plus the
// optional `[active-tab-id: <N>]` hint) parroted back at the very START of an
// assistant reply. The daemon prepends this framing to the user's message on
// the way IN (handleAgentInput); models with weaker instruction-following
// (observed with Nemotron) sometimes echo it on the way OUT, leaking internal
// scaffolding into the chat bubble. Stripping it at this single render
// chokepoint keeps the bubble clean regardless of model. Anchored to the
// start so only a leading echo is removed — a legitimate later mention is
// left intact.
var echoedSidePanelPrefixRE = regexp.MustCompile(`^\s*\[SIDE-PANEL CHAT\]\s*(?:\[active-tab-id:\s*\d+\]\s*)?`)

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
	// EventToolStart carries a parsed TOOL block: the tool's name
	// (Event.Tool) plus a short, human-readable arg preview
	// (Event.Arg) built from the block body. Surfaced as a
	// `tool_start` activity row in the side panel.
	EventToolStart
	// EventToolEnd carries a parsed RESULT block: the tool's name
	// (Event.Tool) plus a status (Event.ToolStatus: "ok" for a
	// successful result, "error" otherwise). Surfaced as the
	// matching `tool_end` activity-row update.
	EventToolEnd
	// EventTurnEnd fires when the runtime writes the `[OAT_TURN_END]`
	// sentinel to OAT_TOOL_LOG — the authoritative once-per-turn
	// end-of-turn marker (emit_turn_end in the Python runtime, which
	// runs exactly once per turn on every completion path). The tailer
	// uses it to stop the side panel's spinner on a silent, tool-only
	// turn and to drive the self-healing recovery ladder. It carries no
	// direct payload; the tailer accumulates the turn's last-tool
	// outcome + whether a visible reply was published across the event
	// stream. Deliberately keyed on this dedicated sentinel and NOT on
	// `[OAT_TOKENS]` — the latter is ALSO emitted right after a mid-turn
	// compaction, so it would false-fire mid-turn.
	EventTurnEnd
	// EventTodos carries the full plan/checklist the runtime wrote to
	// OAT_TOOL_LOG via the `[OAT_TODOS] <json>` sentinel (emit_todos,
	// fired on every write_todos call). Event.Todos holds the structured
	// list. This rides its own sentinel — NOT the ordinary
	// `TOOL: write_todos` block — because the generic tool-arg preview is
	// truncated to toolArgPreviewMaxBytes (200), which would clip any
	// real plan. The tailer forwards it as a `todos` frame for the side
	// panel's live checklist card.
	EventTodos
	// EventGenerating fires on `[OAT_GENERATING] {"tool","bytes"}` while
	// the runtime is still assembling tool-call args (large write_file
	// bodies). UI/observability only — never model context. Event.Tool +
	// Event.Bytes carry the payload.
	EventGenerating
)

// turnEndSentinel is the literal marker the runtime writes to
// OAT_TOOL_LOG at emit_turn_end. Format: "[OAT_TURN_END] <turn_id>".
// We match on the prefix only; the trailing turn_id is informational.
const turnEndSentinel = "[OAT_TURN_END]"

// todosSentinel is the literal marker the runtime writes at emit_todos.
// Format: "[OAT_TODOS] <json-array>". The JSON is the full checklist so
// it deliberately bypasses the 200-byte tool-arg preview cap.
const todosSentinel = "[OAT_TODOS]"

// generatingSentinel is written while tool args are still streaming.
// Format: `[OAT_GENERATING] {"tool":"write_file","bytes":123}`.
// UI/observability only — forgeable like other sentinels; bounded parse.
const generatingSentinel = "[OAT_GENERATING]"

// generatingMaxBytes bounds the JSON payload for [OAT_GENERATING].
const generatingMaxBytes = 512

// generatingToolMaxLen caps the tool name field.
const generatingToolMaxLen = 64

// generatingBytesFieldMax caps the reported arg byte count.
const generatingBytesFieldMax = 50_000_000

// todosMaxBytes bounds the JSON payload we will attempt to json.Unmarshal
// from an `[OAT_TODOS]` line — an allocation guard against a runaway /
// forged plan (the sentinel is model-forgeable, same class as
// [OAT_TURN_END]; a forged card can only draw an inert checklist). The
// runtime already caps each item, but this is defense-in-depth.
const todosMaxBytes = 32 * 1024

// todosMaxItems bounds how many items we keep from a single todos frame.
const todosMaxItems = 50

// todosItemFieldMaxBytes clamps each string field of a parsed todo item.
const todosItemFieldMaxBytes = 500

// Event is one item produced by the streaming parser. Exactly one of
// the kind-specific payload fields is meaningful per the Kind value:
//   - EventAssistantTurn → Turn
//   - EventSidePanelUser → (no payload)
//   - EventToolStart     → Tool, Arg
//   - EventToolEnd       → Tool, ToolStatus
type Event struct {
	Kind EventKind
	Turn AssistantTurn
	// Tool is the tool name for EventToolStart / EventToolEnd.
	Tool string
	// Arg is a short, sanitized arg preview for EventToolStart
	// (e.g. `query: how to fold a shirt`). Empty when the tool had
	// no body lines.
	Arg string
	// ToolStatus is "ok" or "error" for EventToolEnd.
	ToolStatus string
	// ErrorMessage is a short, sanitized failure reason for an
	// EventToolEnd whose ToolStatus is "error" (e.g. the bridge's
	// EXTENSION_NOT_CONNECTED message). Empty for "ok" results and for
	// errors with no extractable message. Lets the side panel show the
	// reason instead of "(detail not attached)" on a failed row. Capped
	// at toolArgPreviewMaxBytes; the bridge redacts on top.
	ErrorMessage string
	// Code is the bridge error code (e.g. "EXTENSION_NOT_CONNECTED",
	// "STALE_REF") extracted from a structured-error RESULT body, when
	// present. Empty for success rows and for errors with no `code`
	// field. Used by the daemon's recovery controller to classify a
	// silent errored turn against the recoverable-code allowlist — a
	// controlled enum, NOT free text, so no page-derived bytes reach
	// the model on a recovery re-prompt.
	Code string
	// Retryable mirrors the bridge error's `retryable` flag when the
	// RESULT body carries one. Defaults to false when absent. Secondary
	// signal for recovery classification (the code allowlist is primary).
	Retryable bool
	// Bytes is the in-progress tool-arg byte count for EventGenerating.
	Bytes int
	// Todos is the parsed checklist for EventTodos. Bounded in count and
	// per-field length by the parser. Nil for every other kind.
	Todos []TodoItem
}

// TodoItem is one entry of an assistant's live plan/checklist, mirroring
// the runtime's write_todos item shape. Rendered as a live card row in
// the side panel (textContent-only; never re-enters the model context).
type TodoItem struct {
	Content    string `json:"content"`
	Status     string `json:"status"`
	ActiveForm string `json:"activeForm"`
}

// toolArgPreviewMaxBytes caps the per-tool arg preview the parser
// surfaces on EventToolStart. The bridge redacts + re-caps before it
// reaches the side panel; this is a first-line defense so an enormous
// tool arg (e.g. a pasted document) can't bloat the daemon→bridge
// frame.
const toolArgPreviewMaxBytes = 200

// toolArgPreviewMaxLines is how many leading `key: value` body lines
// the parser folds into the arg preview. One is usually the salient
// argument (path / query / url / command); a small allowance covers
// tools whose primary arg isn't first.
const toolArgPreviewMaxLines = 2

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
		toolBlock
		resultBlock
	)
	current := none
	// Name + status captured from the current TOOL/RESULT header line
	// (the name sits ON the header, unlike USER/ASSISTANT).
	var curTool, curStatus string

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
		// Defensive: strip a `[SIDE-PANEL CHAT]` sentinel the model echoed
		// back as the first thing in its reply (see echoedSidePanelPrefixRE).
		sanitized = strings.TrimSpace(echoedSidePanelPrefixRE.ReplaceAllString(sanitized, ""))
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

	flushTool := func() {
		raw := strings.Trim(bodyBuf.String(), "\n")
		bodyBuf.Reset()
		if curTool == "" {
			return
		}
		out = append(out, Event{
			Kind: EventToolStart,
			Tool: curTool,
			Arg:  buildToolArgPreview(raw),
		})
	}

	flushResult := func() {
		// The result BODY can be large and carries page/tool-derived
		// content (potential secrets), so we never surface it verbatim.
		// We DO inspect it for two narrow signals: a structured-error
		// envelope (to flip the status, defense-in-depth) and a bounded
		// error message (Event.ErrorMessage, so a failed row can show
		// the reason instead of "(detail not attached)").
		raw := strings.Trim(bodyBuf.String(), "\n")
		bodyBuf.Reset()
		if curTool == "" {
			return
		}
		status := curStatus
		if status == "" {
			status = "ok"
		}
		// Defense-in-depth: some tools return a structured-error envelope
		// ({"ok": false, ...}) in the body while the header was still
		// tagged success (older logs, or a path that didn't run the
		// runtime's status-derivation seam). Reclassify from the body so
		// the side panel flags the row even without an `(error)` suffix.
		isErr, errMsg := detectStructuredResultError(raw)
		if status == "ok" && isErr {
			status = "error"
		}
		ev := Event{
			Kind:       EventToolEnd,
			Tool:       curTool,
			ToolStatus: status,
		}
		// Surface a bounded failure reason ONLY for error rows. Cap it so
		// a verbose body can't bloat the frame; the bridge redacts on top.
		if status == "error" && errMsg != "" {
			ev.ErrorMessage = clampToolPreview(sanitizeEmitText(errMsg), toolArgPreviewMaxBytes)
		}
		// Extract the structured error CODE + retryable flag for error
		// rows so the recovery controller can classify the turn against
		// its code allowlist. The code is a controlled enum, never echoed
		// as free text to the model.
		if status == "error" {
			ev.Code, ev.Retryable = extractResultErrorMeta(raw)
		}
		out = append(out, ev)
	}

	flush := func() {
		switch current {
		case assistantBlock:
			flushAssistant()
		case userBlock:
			flushUser()
		case toolBlock:
			flushTool()
		case resultBlock:
			flushResult()
		}
		current = none
		curTool = ""
		curStatus = ""
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
		if m := toolHeaderRE.FindStringSubmatch(line); m != nil {
			flush()
			current = toolBlock
			curTool = strings.TrimSpace(m[1])
			continue
		}
		if m := resultHeaderRE.FindStringSubmatch(line); m != nil {
			flush()
			current = resultBlock
			curTool, curStatus = parseResultHeader(m[1])
			continue
		}
		// The dedicated once-per-turn end marker. Detected BEFORE the
		// `current == none` skip below (and it also matches nextMarkerRE,
		// so it correctly terminates an open block via flush()). This is
		// the sole trigger for EventTurnEnd — see the EventTurnEnd doc for
		// why we do NOT key on `[OAT_TOKENS]`.
		if strings.HasPrefix(line, turnEndSentinel) {
			flush()
			out = append(out, Event{Kind: EventTurnEnd})
			continue
		}
		// The live-plan checklist sentinel. Like [OAT_TURN_END] it also
		// matches nextMarkerRE, so flush() correctly terminates any open
		// block first. The JSON payload is bounded + validated in
		// parseTodosSentinel; a malformed/oversize line yields no event.
		if strings.HasPrefix(line, todosSentinel) {
			flush()
			if items, ok := parseTodosSentinel(line); ok {
				out = append(out, Event{Kind: EventTodos, Todos: items})
			}
			continue
		}
		if strings.HasPrefix(line, generatingSentinel) {
			flush()
			if tool, n, ok := parseGeneratingSentinel(line); ok {
				out = append(out, Event{Kind: EventGenerating, Tool: tool, Bytes: n})
			}
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

// parseResultHeader splits a RESULT header tail like `web_search` or
// `web_search (error)` into the tool name and a coarse status. Any
// parenthesised suffix other than `success` is treated as an error so
// the side panel flags the row; a bare name (or a `(success)` suffix)
// is "ok".
func parseResultHeader(tail string) (name, status string) {
	tail = strings.TrimSpace(tail)
	if i := strings.LastIndex(tail, " ("); i >= 0 && strings.HasSuffix(tail, ")") {
		inner := strings.TrimSpace(tail[i+2 : len(tail)-1])
		name = strings.TrimSpace(tail[:i])
		if inner == "" || strings.EqualFold(inner, "success") {
			return name, "ok"
		}
		return name, "error"
	}
	return tail, "ok"
}

// errorCodeRE matches an UPPER_SNAKE structured error code — an uppercase
// leading segment followed by at least one `_`-joined uppercase segment
// (e.g. STALE_REF, EXTENSION_NOT_CONNECTED, INPUT_ON_USER_TAB_REFUSED).
// The mandatory underscore is what distinguishes a real bridge error code
// from a benign short code like "X" in a {code, value} success payload,
// which must NOT be painted red (see detectStructuredResultError + the
// "no ok field" test case).
var errorCodeRE = regexp.MustCompile(`^[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+$`)

// looksLikeErrorCode reports whether s is shaped like a bridge error code.
func looksLikeErrorCode(s string) bool {
	return errorCodeRE.MatchString(s)
}

// detectStructuredResultError inspects a RESULT body for the in-band
// structured-error envelope that MCP tools (notably the browser bridge)
// return on failure. Returns whether the body is an error envelope plus a
// best-effort short message (the `message` field, falling back to `error`
// then `errorMessage`).
//
// Two shapes are recognised:
//   - Explicit: a JSON object whose `ok` is exactly `false`. When `ok` is
//     present it is authoritative (`ok:true` / non-bool `ok` => not an error).
//   - Implicit (bridge envelopes that omit `ok`): a non-empty
//     `error`/`errorMessage` string, OR a `code` string shaped like an
//     UPPER_SNAKE error code (looksLikeErrorCode). This catches the
//     green-error bug — e.g. {"code":"EXTENSION_NOT_CONNECTED","message":..}
//     — that was previously classified "ok" because it has no `ok` field.
//
// The benign {"code":"X","value":2} success payload is still NOT an error:
// it has no `ok`, no message/error string, and its short non-snake code
// fails looksLikeErrorCode. Mirrors the runtime-side `_derive_tool_status`
// so the daemon stays correct even for logs that predate that seam.
func detectStructuredResultError(body string) (isError bool, message string) {
	t := strings.TrimSpace(body)
	if !strings.HasPrefix(t, "{") {
		return false, ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(t), &m); err != nil {
		return false, ""
	}
	// Best-effort short message, shared by both shapes.
	msg := ""
	if s, ok := m["message"].(string); ok && s != "" {
		msg = s
	} else if s, ok := m["error"].(string); ok && s != "" {
		msg = s
	} else if s, ok := m["errorMessage"].(string); ok && s != "" {
		msg = s
	}

	// Explicit `ok` is authoritative when present.
	if okVal, hasOK := m["ok"]; hasOK {
		okBool, isBool := okVal.(bool)
		if isBool && !okBool {
			return true, msg
		}
		return false, ""
	}

	// No `ok` field: infer from error signals.
	if s, ok := m["error"].(string); ok && s != "" {
		return true, msg
	}
	if s, ok := m["errorMessage"].(string); ok && s != "" {
		return true, msg
	}
	if s, ok := m["code"].(string); ok && looksLikeErrorCode(s) {
		return true, msg
	}
	return false, ""
}

// extractResultErrorMeta pulls the structured error `code` (a controlled
// enum like "STALE_REF" / "EXTENSION_NOT_CONNECTED") and `retryable`
// flag out of a RESULT body that is a JSON object. Best-effort: returns
// ("", false) for a non-object, unparseable, or field-less body. The
// code is used only for classification (not echoed to the model), so a
// missing code simply means "not on the recoverable allowlist" — the
// fail-closed default.
func extractResultErrorMeta(body string) (code string, retryable bool) {
	t := strings.TrimSpace(body)
	if !strings.HasPrefix(t, "{") {
		return "", false
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(t), &m); err != nil {
		return "", false
	}
	if s, ok := m["code"].(string); ok {
		code = s
	}
	if b, ok := m["retryable"].(bool); ok {
		retryable = b
	}
	return code, retryable
}

// parseTodosSentinel extracts the bounded checklist from an
// `[OAT_TODOS] <json>` line. Returns (items, true) on a well-formed,
// in-bounds payload; (nil, false) for a missing/oversize/malformed one
// (the caller then emits no event). An empty list is valid and returns
// (nil-or-empty, true) so the panel can clear a stale card. Fail-closed:
// anything we can't confidently parse is dropped, never surfaced.
func parseTodosSentinel(line string) ([]TodoItem, bool) {
	payload := strings.TrimSpace(strings.TrimPrefix(line, todosSentinel))
	if payload == "" {
		return nil, false
	}
	// Allocation guard: never hand an unbounded blob to json.Unmarshal.
	if len(payload) > todosMaxBytes {
		return nil, false
	}
	var raw []TodoItem
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return nil, false
	}
	if len(raw) > todosMaxItems {
		raw = raw[:todosMaxItems]
	}
	out := make([]TodoItem, 0, len(raw))
	for _, it := range raw {
		status := strings.TrimSpace(it.Status)
		if status == "" {
			status = "pending"
		}
		out = append(out, TodoItem{
			Content:    clampToolPreview(sanitizeEmitText(it.Content), todosItemFieldMaxBytes),
			Status:     clampToolPreview(sanitizeEmitText(status), 32),
			ActiveForm: clampToolPreview(sanitizeEmitText(it.ActiveForm), todosItemFieldMaxBytes),
		})
	}
	return out, true
}

// parseGeneratingSentinel extracts tool + bytes from
// `[OAT_GENERATING] {"tool":"…","bytes":N}`. Fail-closed on
// missing/oversize/malformed payloads or invalid tool names.
func parseGeneratingSentinel(line string) (tool string, bytes int, ok bool) {
	payload := strings.TrimSpace(strings.TrimPrefix(line, generatingSentinel))
	if payload == "" || len(payload) > generatingMaxBytes {
		return "", 0, false
	}
	var raw struct {
		Tool  string `json:"tool"`
		Bytes int    `json:"bytes"`
	}
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return "", 0, false
	}
	tool = strings.TrimSpace(raw.Tool)
	if tool == "" || len(tool) > generatingToolMaxLen {
		return "", 0, false
	}
	for _, r := range tool {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '.' || r == '-' {
			continue
		}
		return "", 0, false
	}
	bytes = raw.Bytes
	if bytes < 0 {
		bytes = 0
	}
	if bytes > generatingBytesFieldMax {
		bytes = generatingBytesFieldMax
	}
	return tool, bytes, true
}

// buildToolArgPreview folds the leading `key: value` body lines of a
// TOOL block into a single short, sanitized preview string. The
// runtime indents each arg with two spaces (stripped by the line loop
// before it reaches bodyBuf), so the body arrives as `key: value`
// lines. We keep only the first toolArgPreviewMaxLines non-blank
// lines, join them, sanitize control bytes, and clamp to
// toolArgPreviewMaxBytes. The bridge applies redaction on top.
func buildToolArgPreview(body string) string {
	if body == "" {
		return ""
	}
	picked := make([]string, 0, toolArgPreviewMaxLines)
	for _, ln := range strings.Split(body, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		picked = append(picked, t)
		if len(picked) >= toolArgPreviewMaxLines {
			break
		}
	}
	if len(picked) == 0 {
		return ""
	}
	joined := sanitizeEmitText(strings.Join(picked, ", "))
	return clampToolPreview(joined, toolArgPreviewMaxBytes)
}

// clampToolPreview clips s to at most maxBytes bytes without splitting
// a UTF-8 code point, appending an ellipsis when truncation happens.
// Distinct from truncateUTF8 (which adds a multi-line "[…truncated]"
// suffix suited to chat bodies) — a one-line arg preview wants a bare
// "…".
func clampToolPreview(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	const ellipsis = "…"
	keep := maxBytes - len(ellipsis)
	if keep < 0 {
		keep = 0
	}
	for keep > 0 && !utf8.RuneStart(s[keep]) {
		keep--
	}
	return s[:keep] + ellipsis
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
