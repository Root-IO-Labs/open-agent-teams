package daemon

import (
	"bufio"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// assistantTurnFrame is the wire shape sent over `stream_assistant_turns`.
// One frame per ASSISTANT block extracted from the agent's OAT_TOOL_LOG.
//
// Either the turn fields are set (the normal case) or Done/Err is set
// (terminal frame). Never both.
type assistantTurnFrame struct {
	Text string `json:"text,omitempty"`
	Kind string `json:"kind,omitempty"`
	TS   string `json:"ts,omitempty"`
	Done bool   `json:"done,omitempty"`
	Err  string `json:"error,omitempty"`
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
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	frame := assistantTurnFrame{
		Text: turn.SanitizedText,
		Kind: turn.Kind,
		TS:   time.Now().UTC().Format(time.RFC3339Nano),
	}
	subs := make([]chan assistantTurnFrame, 0, len(b.subscribers))
	for _, ch := range b.subscribers {
		subs = append(subs, ch)
	}
	subCount := len(subs)
	b.mu.Unlock()
	if subCount == 0 && b.logf != nil {
		// Critical diagnostic for the smoke-test regression: an
		// ASSISTANT turn was produced but nobody is listening to
		// hear it. Either the bridge subscribe hasn't landed yet
		// (race window) or the bridge has died and not reconnected.
		// Surfaced at Info level on purpose — Debug gets lost in
		// the noise and this is exactly the "where did my chat
		// reply go?" signal we want operators to find.
		preview := turn.SanitizedText
		if len(preview) > 80 {
			preview = preview[:80] + "…"
		}
		b.logf("turnBroadcaster: PUBLISHED ASSISTANT turn with 0 subscribers (lost): %s", preview)
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

	// sidePanelActive flips to true once the parser sees a USER
	// block whose body begins with the `[SIDE-PANEL CHAT]` sentinel.
	// Until that happens, ASSISTANT turns are NOT published — this
	// suppresses post-restart noise like the agent's habitual
	// `/messages` ritual or the literal word "Cleared." emitted in
	// response to a screen-clear ANSI on PTY restart.
	//
	// Owned exclusively by the tailer goroutine; no mutex needed.
	sidePanelActive bool
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

func newAssistantTurnTailer(logPath string, broadcaster *turnBroadcaster, logf func(format string, args ...any)) *assistantTurnTailer {
	return &assistantTurnTailer{
		logPath:     logPath,
		broadcaster: broadcaster,
		logf:        logf,
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
			if turnHeaderRE.MatchString(pending[i]) || userHeaderRE.MatchString(pending[i]) {
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
				wasActive := t.sidePanelActive
				t.sidePanelActive = true
				if !wasActive && t.logf != nil {
					t.logf("assistantTurnTailer: side-panel sentinel detected, auto-emit ON (%s)", t.logPath)
				}
			case EventAssistantTurn:
				if !t.sidePanelActive {
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
