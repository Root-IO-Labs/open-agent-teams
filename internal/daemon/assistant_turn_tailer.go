package daemon

import (
	"bufio"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// assistantTurnFrame is the wire shape sent over `stream_assistant_turns`.
// One frame per ASSISTANT block extracted from the agent's OAT_TOOL_LOG,
// OR one frame per TOOL/RESULT block (tool-activity frames, assistant
// agents only — see assistantTurnTailer.emitToolEvents).
//
// Frame shapes (exactly one applies):
//   - chat turn:  Text + Kind ("final" | "question")
//   - tool start: Kind "tool_start" + Tool + Arg
//   - tool end:   Kind "tool_end"   + Tool + Status ("ok" | "error")
//   - terminal:   Done OR Err
type assistantTurnFrame struct {
	Text string `json:"text,omitempty"`
	// Kind is "final" | "question" for chat turns, or
	// "tool_start" | "tool_end" for tool-activity frames.
	Kind string `json:"kind,omitempty"`
	// Tool is the tool name on tool_start / tool_end frames.
	Tool string `json:"tool,omitempty"`
	// Arg is a short, sanitized arg preview on tool_start frames.
	Arg string `json:"arg,omitempty"`
	// Status is "ok" | "error" on tool_end frames.
	Status string `json:"status,omitempty"`
	// ErrorMessage is a short, sanitized failure reason on tool_end
	// frames whose Status is "error" (e.g. the bridge's
	// EXTENSION_NOT_CONNECTED message). Lets the side panel show the
	// reason instead of "(detail not attached)" on a failed row.
	ErrorMessage string `json:"errorMessage,omitempty"`
	TS           string `json:"ts,omitempty"`
	Done         bool   `json:"done,omitempty"`
	Err          string `json:"error,omitempty"`

	// --- turn_end frame fields (Kind == "turn_end") ---
	// Emitted once per runtime turn (on the `[OAT_TURN_END]` sentinel).
	// The side panel uses this to stop its spinner even when a turn
	// produced no visible reply (a silent, tool-only errored turn), and
	// to render the self-healing outcome. All omitempty so non-turn_end
	// frames stay byte-identical on the wire (old panels ignore unknown
	// fields; additive + backward-compatible).

	// HadError is true when the turn's LAST tool call ended in error.
	HadError bool `json:"hadError,omitempty"`
	// Code is the last tool error's structured code (controlled enum),
	// for the panel's code->friendly-message map.
	Code string `json:"code,omitempty"`
	// Retryable mirrors the last tool error's retryable flag.
	Retryable bool `json:"retryable,omitempty"`
	// VisibleReply is true when the turn published at least one
	// ASSISTANT chat bubble (final/question). When true the panel shows
	// no outcome line — the agent already spoke.
	VisibleReply bool `json:"visibleReply,omitempty"`
	// Recovering is set by the daemon's recovery controller when it has
	// injected a bounded auto-recovery re-prompt for this errored turn.
	// The panel shows a quiet "trying another way" note instead of a
	// prominent error when true.
	Recovering bool `json:"recovering,omitempty"`

	// --- todos frame fields (Kind == "todos") ---
	// Emitted on each `[OAT_TODOS]` sentinel (every write_todos call).
	// Carries the agent's full live plan for the side panel's checklist
	// card. omitempty so non-todos frames stay byte-identical on the
	// wire; old panels ignore the unknown field.
	Todos []todoFrameItem `json:"todos,omitempty"`
}

// todoFrameItem is the wire shape of one checklist row forwarded to the
// side panel. Mirrors TodoItem; a distinct type keeps the JSON tags for
// the frame contract separate from the parser's internal struct.
type todoFrameItem struct {
	Content    string `json:"content"`
	Status     string `json:"status"`
	ActiveForm string `json:"activeForm,omitempty"`
}

// turnBroadcaster fans out parsed AssistantTurn values from one
// producer (the tailer goroutine) to many subscribers (each
// stream_assistant_turns connection from the bridge). Mirrors the
// pattern used by chunkBroadcaster for stream_agent_output (see
// pkg/backend's broadcaster) but at a higher abstraction level —
// one frame per turn instead of one per PTY chunk.
//
// Backpressure: each subscriber gets a small buffered channel
// (turnSubscriberBuf). If a subscriber falls behind, frames are
// dropped on the floor rather than blocking the producer — chat turns
// are infrequent enough that catching up on missed bubbles isn't
// worth the complexity of a gap-marker protocol like
// stream_agent_output uses. The dropped count is logged.
type turnBroadcaster struct {
	mu          sync.Mutex
	closed      bool
	subscribers map[int]chan assistantTurnFrame
	nextID      int
	logf        func(format string, args ...any)
}

const turnSubscriberBuf = 16

func newTurnBroadcaster(logf func(format string, args ...any)) *turnBroadcaster {
	return &turnBroadcaster{
		subscribers: make(map[int]chan assistantTurnFrame),
		logf:        logf,
	}
}

// Subscribe returns a channel that receives every subsequent frame
// (turns are NOT replayed for late subscribers — chat history lives
// in the side panel's chrome.storage.local), plus a cancel func the
// caller must invoke when done.
func (b *turnBroadcaster) Subscribe() (<-chan assistantTurnFrame, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		ch := make(chan assistantTurnFrame)
		close(ch)
		return ch, func() {}
	}
	ch := make(chan assistantTurnFrame, turnSubscriberBuf)
	id := b.nextID
	b.nextID++
	b.subscribers[id] = ch
	cancel := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if existing, ok := b.subscribers[id]; ok {
			delete(b.subscribers, id)
			close(existing)
		}
	}
	return ch, cancel
}

// Publish broadcasts a turn to all current subscribers. A slow
// subscriber whose buffer is full has the frame dropped; the producer
// never blocks on a subscriber.
func (b *turnBroadcaster) Publish(turn AssistantTurn) {
	preview := turn.SanitizedText
	if len(preview) > 80 {
		preview = preview[:80] + "…"
	}
	b.publishFrame(assistantTurnFrame{
		Text: turn.SanitizedText,
		Kind: turn.Kind,
		TS:   time.Now().UTC().Format(time.RFC3339Nano),
	}, "ASSISTANT turn", preview)
}

// PublishTool broadcasts a tool-activity frame (tool_start / tool_end).
// kind must be "tool_start" or "tool_end"; arg applies to tool_start,
// status + errorMessage to tool_end. Same fire-and-forget fan-out
// semantics as Publish.
func (b *turnBroadcaster) PublishTool(kind, tool, arg, status, errorMessage string) {
	b.publishFrame(assistantTurnFrame{
		Kind:         kind,
		Tool:         tool,
		Arg:          arg,
		Status:       status,
		ErrorMessage: errorMessage,
		TS:           time.Now().UTC().Format(time.RFC3339Nano),
	}, kind, tool)
}

// PublishTurnEnd broadcasts a turn_end frame — one per runtime turn,
// fired on the `[OAT_TURN_END]` sentinel. Same fire-and-forget fan-out
// as Publish. `recovering` reflects whether the daemon's recovery
// controller injected an auto-recovery re-prompt for this turn.
func (b *turnBroadcaster) PublishTurnEnd(info turnEndInfo, recovering bool) {
	b.publishFrame(assistantTurnFrame{
		Kind:         "turn_end",
		HadError:     info.HadError,
		Code:         info.Code,
		ErrorMessage: info.ErrorMessage,
		Retryable:    info.Retryable,
		VisibleReply: info.VisibleReply,
		Recovering:   recovering,
		TS:           time.Now().UTC().Format(time.RFC3339Nano),
	}, "turn_end", info.Code)
}

// PublishTodos broadcasts a todos frame — one per write_todos call
// (the `[OAT_TODOS]` sentinel). Same fire-and-forget fan-out as Publish.
// An empty list is valid (the panel clears the card).
func (b *turnBroadcaster) PublishTodos(items []TodoItem) {
	frameItems := make([]todoFrameItem, 0, len(items))
	for _, it := range items {
		frameItems = append(frameItems, todoFrameItem{
			Content:    it.Content,
			Status:     it.Status,
			ActiveForm: it.ActiveForm,
		})
	}
	b.publishFrame(assistantTurnFrame{
		Kind:  "todos",
		Todos: frameItems,
		TS:    time.Now().UTC().Format(time.RFC3339Nano),
	}, "todos", "")
}

// publishFrame is the shared fan-out path for Publish / PublishTool.
// `what` + `preview` only feed the 0-subscriber diagnostic log.
func (b *turnBroadcaster) publishFrame(frame assistantTurnFrame, what, preview string) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	subs := make([]chan assistantTurnFrame, 0, len(b.subscribers))
	for _, ch := range b.subscribers {
		subs = append(subs, ch)
	}
	subCount := len(subs)
	b.mu.Unlock()
	if subCount == 0 && b.logf != nil {
		// Critical diagnostic for the smoke-test regression: a frame
		// was produced but nobody is listening to hear it. Either the
		// bridge subscribe hasn't landed yet (race window) or the
		// bridge has died and not reconnected. Surfaced at Info level
		// on purpose — Debug gets lost in the noise and this is
		// exactly the "where did my chat reply go?" signal we want
		// operators to find.
		b.logf("turnBroadcaster: PUBLISHED %s with 0 subscribers (lost): %s", what, preview)
	}
	for _, ch := range subs {
		select {
		case ch <- frame:
		default:
			if b.logf != nil {
				b.logf("turnBroadcaster: dropped frame for slow subscriber")
			}
		}
	}
}

// Close terminates all subscriptions, sending a `Done: true` frame
// where possible. Safe to call multiple times.
func (b *turnBroadcaster) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	subs := b.subscribers
	b.subscribers = nil
	b.mu.Unlock()
	for _, ch := range subs {
		// Try to land a Done frame; if the subscriber is wedged,
		// just close — they'll see the channel close as end-of-stream.
		select {
		case ch <- assistantTurnFrame{Done: true}:
		default:
		}
		close(ch)
	}
}

// assistantTurnTailer tails an agent's OAT_TOOL_LOG file, parses
// ASSISTANT blocks, and publishes each as an AssistantTurn to its
// broadcaster.
//
// Lifecycle is bound to the agent process:
//   - Start() spawns the tailer goroutine and returns immediately.
//   - Stop() closes the broadcaster and signals the tailer to exit;
//     blocks briefly for the goroutine to drain.
//
// The tailer survives log truncation (some runtimes rotate) by
// detecting "current offset > file size" and seeking back to 0.
type assistantTurnTailer struct {
	logPath     string
	broadcaster *turnBroadcaster
	logf        func(format string, args ...any)
	cancel      context.CancelFunc
	wg          sync.WaitGroup

	// sidePanelActive flips to true once side-panel chat is known to be
	// underway for this agent. Until that happens, ASSISTANT turns are
	// NOT published — this suppresses post-restart noise like the
	// agent's habitual `/messages` ritual or the literal word
	// "Cleared." emitted in response to a screen-clear ANSI on PTY
	// restart.
	//
	// Two independent triggers set it (whichever fires first wins):
	//   1. Delivery-time arming via markSidePanelActive(), called by the
	//      daemon the instant it hands a side-panel message to the agent
	//      (handleAgentInput / handleRouteUserMessage). This is the
	//      authoritative signal — the daemon KNOWS it just delivered a
	//      side-panel turn, so it never has to re-discover that fact by
	//      parsing the sentinel back out of the agent's log.
	//   2. The legacy log-parse path: the tailer sees a USER block whose
	//      body begins with the `[SIDE-PANEL CHAT]` sentinel. Kept as a
	//      belt-and-suspenders fallback.
	//
	// Trigger 1 fixes the blackout where a working agent's replies were
	// all suppressed because the sentinel-bearing USER block was written
	// to the log AFTER the agent's response turns (the agent was busy /
	// blocked, so the conversation log writes landed out of order). The
	// daemon-side arming lands well before any reply is emitted.
	//
	// atomic.Bool because markSidePanelActive() is called from the
	// daemon's socket-handler goroutine while run() reads/writes it from
	// the tailer goroutine.
	sidePanelActive atomic.Bool

	// emitToolEvents gates publishing of TOOL/RESULT blocks as
	// tool_start/tool_end activity frames. Enabled ONLY for
	// AgentTypeAssistant: the browser agent's tool calls are already
	// surfaced as activity rows via the bridge's MCP onToolStart /
	// onToolEnd hooks, so parsing them from its log too would
	// double-render every row in the side panel. The assistant has
	// no such hook (its tools aren't bridge-mediated), so the log is
	// the only signal — hence this flag.
	emitToolEvents bool

	// onTurnEnd, when non-nil, is called by the tailer goroutine the
	// moment a turn ends (the `[OAT_TURN_END]` sentinel) with the
	// accumulated turn outcome. It returns whether an auto-recovery
	// re-prompt was initiated; the tailer stamps that onto the
	// published turn_end frame's `recovering` field. Set ONLY for
	// AgentTypeAssistant (recovery is assistant-scoped); nil for the
	// browser agent and under OAT_TEST_MODE, in which case the frame is
	// published with recovering=false. The callback runs synchronously
	// on the tailer goroutine, so it must be fast + non-blocking (the
	// recovery controller only touches in-memory budget state + a
	// fire-and-forget backend.SendMessage).
	onTurnEnd func(info turnEndInfo) (recovering bool)
}

// turnEndInfo is the accumulated per-turn outcome the tailer hands to
// PublishTurnEnd and the recovery controller. Built from the ordered
// event stream since the last turn boundary.
type turnEndInfo struct {
	// HadError is true when the turn's LAST tool call ended in error.
	HadError bool
	// Code / ErrorMessage / Retryable describe that last tool error.
	Code         string
	ErrorMessage string
	Retryable    bool
	// VisibleReply is true when the turn published >=1 ASSISTANT bubble.
	VisibleReply bool
	// HadTools is true when the turn saw at least one tool_start/tool_end
	// (used for silent-after-successful-tools incomplete recovery).
	HadTools bool
	// HasUnfinishedTodos is true when the last [OAT_TODOS] in this turn
	// still had pending/in_progress items (refines the incomplete nudge).
	HasUnfinishedTodos bool
}

// tailerPollInterval is how often the tailer wakes to check for new
// content when at EOF. Chosen tighter than the OutputWatcher's
// equivalent (which runs at 250 ms) because the assistant-turn
// stream is on the user-visible chat-reply path: every poll gap
// adds to the visible "I hit send" → "bubble appears" latency,
// which the side panel can't compensate for client-side. The cost
// is one extra fstat per active browser-agent every 100 ms; with
// O(1) browser-agents per repo this is in the noise compared to
// the daemon's other 2-min loops.
const tailerPollInterval = 100 * time.Millisecond

// pendingLineWindow is how many lines the parser holds before forcing
// a flush. A multi-paragraph ASSISTANT block written one line at a
// time should still emit promptly; this cap stops a wedged producer
// from buffering forever.
const pendingLineWindow = 2048

// trailingFlushGrace is how long a buffered-but-unterminated block may
// sit at EOF before the tailer force-flushes it. The normal flush path
// waits for a terminator marker after the last header — the next
// `[OAT_TOKENS]` envelope, USER/ASSISTANT header, or TOOL/RESULT block.
// A turn that ends WITHOUT any of those (a network-error / interrupted
// turn writes its ASSISTANT block but emits no token envelope, since a
// failed turn that consumed zero tokens commits nothing) would
// otherwise stay invisible in the side panel until the user sends the
// NEXT message — making a 3-second fast-fail look like a multi-minute
// hang.
//
// This is safe because the runtime writes each ASSISTANT block
// atomically (ConversationLogger.log_assistant emits header + body +
// trailing blank in one flush), so a block that has not grown for this
// long is definitively complete. Successful turns self-terminate via
// the `[OAT_TOKENS]` envelope the runtime emits in the same end-of-turn
// finalization (sub-second), so they flush via the terminator path and
// never wait on this grace.
const trailingFlushGrace = 1 * time.Second

func newAssistantTurnTailer(logPath string, broadcaster *turnBroadcaster, emitToolEvents bool, logf func(format string, args ...any)) *assistantTurnTailer {
	return &assistantTurnTailer{
		logPath:        logPath,
		broadcaster:    broadcaster,
		emitToolEvents: emitToolEvents,
		logf:           logf,
	}
}

// Start spawns the tailer goroutine. Idempotent; second call is a
// no-op.
func (t *assistantTurnTailer) Start(parent context.Context) {
	if t.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	t.cancel = cancel
	t.wg.Add(1)
	go t.run(ctx)
}

// Stop cancels the tailer and closes the broadcaster. Blocks until
// the tailer goroutine exits or 2s elapses (defensive — should be
// instant in practice).
func (t *assistantTurnTailer) Stop() {
	if t.cancel == nil {
		return
	}
	t.cancel()
	done := make(chan struct{})
	go func() { t.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		if t.logf != nil {
			t.logf("assistantTurnTailer: timed out waiting for goroutine to exit (%s)", t.logPath)
		}
	}
	t.broadcaster.Close()
}

// markSidePanelActive arms auto-emit from OUTSIDE the tailer goroutine
// — the daemon calls this the instant it delivers a side-panel message
// to the agent (handleAgentInput / handleRouteUserMessage). This is the
// authoritative "side-panel chat is underway" signal and is immune to
// the log-write-ordering race that previously suppressed a busy agent's
// replies (the sentinel could land in the log after the reply turns).
// Safe to call concurrently with run(); idempotent.
func (t *assistantTurnTailer) markSidePanelActive() {
	if t.sidePanelActive.Swap(true) {
		return
	}
	if t.logf != nil {
		t.logf("assistantTurnTailer: side-panel auto-emit ON via delivery (%s)", t.logPath)
	}
}

func (t *assistantTurnTailer) run(ctx context.Context) {
	defer t.wg.Done()
	defer func() {
		if r := recover(); r != nil && t.logf != nil {
			t.logf("assistantTurnTailer panic for %s: %v", t.logPath, r)
		}
	}()

	// Wait briefly for the log to exist — daemon may spawn us
	// before the agent has written its first byte. Bail out cleanly
	// on cancellation.
	for {
		if _, err := os.Stat(t.logPath); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(tailerPollInterval):
		}
	}

	f, err := os.Open(t.logPath)
	if err != nil {
		if t.logf != nil {
			t.logf("assistantTurnTailer: open %s: %v", t.logPath, err)
		}
		return
	}
	defer f.Close()

	// Start at end-of-file: the agent's startup banner is not
	// chat-relevant, and replaying it would spam the side panel with
	// stale bubbles on bridge restart.
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		// Non-fatal — fall through with offset 0.
		if t.logf != nil {
			t.logf("assistantTurnTailer: seek end %s: %v", t.logPath, err)
		}
	}

	reader := bufio.NewReader(f)

	// Pending-line window. We hold completed lines until either a
	// new ASSISTANT header arrives (which means we can finalize the
	// previous block) or the window is full. parseAssistantTurns is
	// pure so we can re-run it on the same buffer cheaply.
	pending := make([]string, 0, 64)

	// Lines from the previous read that ended without a newline. We
	// stitch them onto the next read so we don't accidentally split
	// the [HH:MM:SS] header.
	var carry strings.Builder

	// When the last line was appended to `pending`. Drives the
	// trailingFlushGrace idle-flush so an unterminated trailing block
	// (e.g. a network-error turn that emits no token envelope) still
	// reaches the side panel instead of waiting for the next turn.
	var lastAppend time.Time

	// Per-turn outcome accumulation. Reset at each turn boundary (the
	// `[OAT_TURN_END]` sentinel) and when a new side-panel USER turn
	// begins, so the turn_end frame + recovery classification reflect
	// only the current turn. Held here (not on the struct) because it's
	// single-goroutine state owned entirely by run().
	var turnVisibleReply, turnHadError, turnRetryable, turnHadTools, turnUnfinishedTodos bool
	var turnErrCode, turnErrMsg string
	resetTurnState := func() {
		turnVisibleReply = false
		turnHadError = false
		turnRetryable = false
		turnHadTools = false
		turnUnfinishedTodos = false
		turnErrCode = ""
		turnErrMsg = ""
	}

	flushBuffer := func(force bool) {
		if len(pending) == 0 {
			return
		}
		// Find the index of the LAST USER/ASSISTANT header. Anything
		// before it belongs to a prior block and is safe to parse;
		// anything from it onward might still grow.
		//
		// We accept BOTH USER and ASSISTANT here (the older
		// implementation only checked ASSISTANT, which let prior
		// USER blocks linger an extra cycle and was harmless but
		// inconsistent).
		lastHeader := -1
		for i := len(pending) - 1; i >= 0; i-- {
			if turnHeaderRE.MatchString(pending[i]) || userHeaderRE.MatchString(pending[i]) ||
				toolHeaderRE.MatchString(pending[i]) || resultHeaderRE.MatchString(pending[i]) {
				// TOOL/RESULT headers count alongside USER/ASSISTANT so
				// a trailing (still-growing) TOOL/RESULT block is held
				// back like an open ASSISTANT block instead of being
				// parsed with an incomplete body. Their arg/status only
				// becomes complete once the NEXT header lands. This does
				// NOT change ASSISTANT-prelude publish timing: the
				// prelude sits before this header in `pending`, so the
				// non-terminator branch below still parses + flushes it
				// up to (but not including) the open tool/result block.
				lastHeader = i
				break
			}
		}
		// Detect whether any structured marker arrived AFTER the last
		// USER/ASSISTANT header. nextMarkerRE matches:
		//   - any timestamped block header at column 0
		//     (USER:, ASSISTANT:, TOOL:, RESULT:, ERROR:, ...)
		//   - any [OAT_*] envelope (e.g. [OAT_MODEL], [OAT_TOKENS])
		//
		// Any such line strictly after lastHeader means the
		// ASSISTANT body is definitively over and the buffered block
		// is safe to publish — even if no new ASSISTANT/USER header
		// has arrived yet.
		//
		// Two separate regressions this fixes:
		//
		//   1. End-of-turn idle (user sends → agent replies → both
		//      wait). Caught by the [OAT_TOKENS] envelope the runtime
		//      emits at end-of-turn. (Fixed 2026-05-18.)
		//
		//   2. Mid-turn ASSISTANT-then-TOOL ("I'll click X" → TOOL:
		//      browser_click). The next [OAT_*] envelope is the
		//      end-of-turn marker, which doesn't arrive until after
		//      the tool result lands and the model finishes the
		//      whole turn. If the tool call hangs (a real failure
		//      mode we hit 2026-05-19 on Southwest's date dropdown),
		//      the ASSISTANT prelude sits buffered indefinitely and
		//      the side panel goes dark even though `oat ui` shows
		//      the reply just fine. Recognising TOOL/RESULT markers
		//      as block terminators publishes the prelude
		//      immediately, so the user sees "I'll click X" the
		//      moment the model commits to it.
		terminatorAfterLastHeader := false
		for i := len(pending) - 1; i > lastHeader; i-- {
			if nextMarkerRE.MatchString(pending[i]) {
				terminatorAfterLastHeader = true
				break
			}
		}

		if !force && !terminatorAfterLastHeader {
			if lastHeader < 0 {
				// No header AND no envelope yet — pending must
				// contain pre-block garbage. Wait for structure.
				return
			}
			if lastHeader == len(pending)-1 {
				// We literally just saw the header and have no
				// body lines yet — wait for more.
				return
			}
		}

		// Decide how much of pending to feed parseEvents.
		//   - force OR terminator-seen: parse the whole buffer; the
		//     latest block is definitively complete.
		//   - otherwise: parse up to (but not including) the most
		//     recent header so the open block keeps growing.
		var toParse []string
		if force || terminatorAfterLastHeader || lastHeader < 0 {
			toParse = pending
			pending = pending[:0]
		} else {
			toParse = pending[:lastHeader]
			// Move the still-open header (and anything after it,
			// which is its body so far) to the start of the buffer
			// so subsequent appends keep it growing.
			remaining := append([]string{}, pending[lastHeader:]...)
			pending = remaining
		}
		if len(toParse) == 0 {
			return
		}
		events := parseEvents(toParse)
		for _, ev := range events {
			switch ev.Kind {
			case EventSidePanelUser:
				// First side-panel input in this agent lifetime
				// unlocks auto-emit for subsequent ASSISTANT turns.
				// Once set, it stays set — a single chat session
				// often contains many round trips and we don't want
				// to gate every one.
				wasActive := t.sidePanelActive.Swap(true)
				if !wasActive && t.logf != nil {
					t.logf("assistantTurnTailer: side-panel sentinel detected, auto-emit ON (%s)", t.logPath)
				}
				// A fresh user turn resets the accumulated outcome so a
				// prior turn's error can't leak into this one.
				resetTurnState()
			case EventAssistantTurn:
				if !t.sidePanelActive.Load() {
					// Pre-side-panel chatter (startup banner,
					// /messages ritual, "Cleared." on restart, etc.)
					// is suppressed. Log at debug-ish volume so we
					// can confirm via daemon log that suppression
					// is firing.
					if t.logf != nil {
						preview := ev.Turn.SanitizedText
						if len(preview) > 80 {
							preview = preview[:80] + "…"
						}
						t.logf("assistantTurnTailer: suppressed pre-side-panel turn (%s)", preview)
					}
					continue
				}
				t.broadcaster.Publish(ev.Turn)
				// A published bubble means the turn "spoke" — the panel
				// suppresses the turn_end outcome line in that case.
				// Cleared again if a tool runs after this (below): mid-turn
				// narration is foldable preamble, not a final reply.
				turnVisibleReply = true
			case EventToolStart:
				if !t.emitToolEvents || !t.sidePanelActive.Load() {
					// Tool activity is assistant-only (browser tools
					// already flow via MCP hooks) and gated on the same
					// side-panel sentinel as ASSISTANT turns so terminal
					// / pre-chat tool calls don't spam the panel.
					continue
				}
				// Tools after mid-turn ASSISTANT narration mean that
				// text was preamble (the panel folds it into Thought),
				// not a closing reply. Clear so incomplete-silent
				// recovery can fire when the model later stops without
				// a post-tool bubble.
				turnVisibleReply = false
				turnHadTools = true
				t.broadcaster.PublishTool("tool_start", ev.Tool, ev.Arg, "", "")
			case EventToolEnd:
				// Track the last tool outcome for the turn_end frame
				// FIRST — regardless of whether we publish the row.
				// Browser agents don't emit tool rows from the log
				// (emitToolEvents=false) but their turn_end still needs
				// the outcome, so this update sits above the gate.
				// Same VisibleReply clear as tool_start: a tool after
				// narration invalidates "spoke to the user".
				turnVisibleReply = false
				turnHadTools = true
				if ev.ToolStatus == "error" {
					turnHadError = true
					turnErrCode = ev.Code
					turnErrMsg = ev.ErrorMessage
					turnRetryable = ev.Retryable
				} else {
					turnHadError = false
					turnErrCode = ""
					turnErrMsg = ""
					turnRetryable = false
				}
				if !t.emitToolEvents || !t.sidePanelActive.Load() {
					continue
				}
				t.broadcaster.PublishTool("tool_end", ev.Tool, "", ev.ToolStatus, ev.ErrorMessage)
			case EventTurnEnd:
				if !t.sidePanelActive.Load() {
					// Pre-side-panel turn ends aren't surfaced; still
					// reset so the next (real) turn starts clean.
					resetTurnState()
					continue
				}
				info := turnEndInfo{
					HadError:           turnHadError,
					Code:               turnErrCode,
					ErrorMessage:       turnErrMsg,
					Retryable:          turnRetryable,
					VisibleReply:       turnVisibleReply,
					HadTools:           turnHadTools,
					HasUnfinishedTodos: turnUnfinishedTodos,
				}
				recovering := false
				if t.onTurnEnd != nil {
					// A panicking recovery callback must never take down
					// the tailer goroutine.
					func() {
						defer func() {
							if r := recover(); r != nil && t.logf != nil {
								t.logf("assistantTurnTailer: onTurnEnd panic (%s): %v", t.logPath, r)
							}
						}()
						recovering = t.onTurnEnd(info)
					}()
				}
				t.broadcaster.PublishTurnEnd(info, recovering)
				resetTurnState()
			case EventTodos:
				// Live plan/checklist. Gated on the same side-panel
				// sentinel as chat + tool rows so pre-chat plans don't
				// spam the panel. textContent-only render downstream.
				if !t.sidePanelActive.Load() {
					continue
				}
				turnUnfinishedTodos = todosHaveUnfinished(ev.Todos)
				t.broadcaster.PublishTodos(ev.Todos)
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			flushBuffer(true)
			return
		default:
		}

		// Read what's available right now. ReadString returns
		// io.EOF when the file has no more bytes; we don't treat
		// that as fatal — it just means "park and poll".
		chunk, readErr := reader.ReadString('\n')
		if chunk != "" {
			full := carry.String() + chunk
			carry.Reset()
			if !strings.HasSuffix(full, "\n") {
				// Partial line — stash it and try again next read.
				carry.WriteString(full)
			} else {
				line := strings.TrimRight(full, "\r\n")
				pending = append(pending, line)
				lastAppend = time.Now()
				if len(pending) >= pendingLineWindow {
					// Defensive: prevent unbounded growth if the
					// runtime stops emitting a terminator marker.
					flushBuffer(true)
				} else {
					flushBuffer(false)
				}
			}
		}

		switch readErr {
		case nil:
			// More data may be immediately available — loop without
			// sleeping.
			continue
		case io.EOF:
			// Reached end of file; sleep before checking again.
			// Also detect truncation: if the file's current size is
			// less than our offset, seek back to 0 and reset state.
			if st, errStat := f.Stat(); errStat == nil {
				if cur, errCur := f.Seek(0, io.SeekCurrent); errCur == nil && st.Size() < cur {
					_, _ = f.Seek(0, io.SeekStart)
					reader = bufio.NewReader(f)
					carry.Reset()
					pending = pending[:0]
					if t.logf != nil {
						t.logf("assistantTurnTailer: detected truncation of %s, resetting", t.logPath)
					}
				}
			}
			// Idle-flush: a trailing block that the terminator path
			// couldn't publish (no [OAT_TOKENS]/header after it — the
			// network-error / interrupted-turn case) is force-flushed
			// once it has been stable for trailingFlushGrace. Without
			// this a fast model-side failure stays invisible until the
			// user's next message. See trailingFlushGrace for why this
			// is safe (atomic block writes; successful turns terminate
			// via [OAT_TOKENS] well within the grace window).
			if len(pending) > 0 && !lastAppend.IsZero() && time.Since(lastAppend) >= trailingFlushGrace {
				flushBuffer(true)
			}
			select {
			case <-ctx.Done():
				flushBuffer(true)
				return
			case <-time.After(tailerPollInterval):
			}
		default:
			// Unexpected read error — log and exit so we don't loop
			// hot on a dead file.
			if t.logf != nil {
				t.logf("assistantTurnTailer: read %s: %v", t.logPath, readErr)
			}
			flushBuffer(true)
			return
		}
	}
}
