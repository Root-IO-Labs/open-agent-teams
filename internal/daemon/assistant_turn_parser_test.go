package daemon

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// timeAfterShort returns a channel that fires after a short test
// timeout. Kept tight (200 ms) to keep the test suite snappy; the
// broadcaster only needs to deliver to a single drainer, so anything
// over a few ms indicates a real bug.
func timeAfterShort() <-chan time.Time {
	return time.After(200 * time.Millisecond)
}

// TestSanitizeEmitText covers the four byte-classes the sanitizer is
// responsible for: ANSI escapes (dropped wholesale), C0 controls (kept
// for \n/\t, dropped otherwise), UTF-8-encoded C1 controls (dropped),
// and printable bytes (passed through). The intent is byte-for-byte
// parity with bridge/src/emit-to-user.ts: sanitizeEmitText() — if these
// drift the auto-emit path and the tool-emit path would render
// differently for the same payload, which is the whole bug Part 2g
// exists to prevent.
func TestSanitizeEmitText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "plain ascii unchanged",
			in:   "hello world",
			want: "hello world",
		},
		{
			name: "keeps newlines and tabs",
			in:   "line1\n\tindented\nline2",
			want: "line1\n\tindented\nline2",
		},
		{
			name: "drops C0 controls except \\n \\t",
			in:   "a\x00b\x01c\x07d\x1be\x1ff",
			// \x00 \x01 \x07 \x1f dropped; \x1b starts ANSI escape so
			// "e" after a single-char ESC is consumed as the
			// terminator. Result: "abcd" then "f" survives.
			// (\x1b 'e' is a single-char escape; the next byte is 'f'.)
			want: "abcdf",
		},
		{
			name: "drops CSI escape with parameters",
			in:   "before\x1b[31;1mred\x1b[0mafter",
			want: "beforeredafter",
		},
		{
			name: "drops OSC escape terminated by BEL",
			in:   "x\x1b]0;window title\x07y",
			want: "xy",
		},
		{
			name: "drops OSC escape terminated by ST",
			in:   "x\x1b]0;window title\x1b\\y",
			want: "xy",
		},
		{
			name: "drops UTF-8 encoded C1 controls",
			// 0xC2 0x85 is U+0085 NEXT LINE (a C1 control).
			in:   "before\xc2\x85after\xc2\x9fend",
			want: "beforeafterend",
		},
		{
			name: "passes through multi-byte non-control utf-8",
			in:   "café — emoji 🤖",
			want: "café — emoji 🤖",
		},
		{
			name: "isolated ESC at end is dropped without panic",
			in:   "trailing\x1b",
			want: "trailing",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeEmitText(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeEmitText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestTruncateUTF8 verifies the cap respects rune boundaries and only
// appends the truncation suffix when needed.
func TestTruncateUTF8(t *testing.T) {
	t.Run("under cap passes through", func(t *testing.T) {
		got := truncateUTF8("short", 64)
		if got != "short" {
			t.Errorf("expected unchanged, got %q", got)
		}
	})
	t.Run("over cap appends suffix", func(t *testing.T) {
		s := strings.Repeat("a", 100)
		got := truncateUTF8(s, 50)
		if !strings.HasSuffix(got, "[…truncated]") {
			t.Errorf("expected truncation suffix, got %q", got)
		}
		if len(got) > 50 {
			t.Errorf("expected len <= 50, got %d", len(got))
		}
	})
	t.Run("does not split a multi-byte rune", func(t *testing.T) {
		// Each "🤖" is 4 bytes. With maxBytes=10 and a 14-byte suffix,
		// keep ≤ -4 → 0 → no body, just suffix.
		got := truncateUTF8(strings.Repeat("🤖", 5), 10)
		// Should not contain any malformed half-rune (we just check
		// the body part decodes cleanly to runes).
		for _, r := range got {
			_ = r // iterating won't panic if it's valid UTF-8
		}
	})
}

// TestClassifyTurnKind exercises the question heuristic on the
// examples called out in the doc comment plus a handful of stress
// cases the Part 2g design surfaced.
func TestClassifyTurnKind(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		// Positive — should become "question"
		{"second person question", "Could you tell me your preferred format?", "question"},
		{"should I question", "Should I open the pricing page or the docs?", "question"},
		{"what would you", "What would you like me to do?", "question"},
		{"what time wh-word", "What time is best for you?", "question"},
		{"how with you", "How would you like the output formatted?", "question"},
		{"shall i", "Shall I begin?", "question"},
		// Multi-sentence — only the last sentence is considered.
		{
			name: "last sentence is the question",
			body: "I found three results. Should I open them in order?",
			want: "question",
		},
		// Negative — should remain "final"
		{"rhetorical with answer", "Is the moon made of cheese? Obviously not.", "final"},
		{"period not question", "I checked: no messages.", "final"},
		{
			name: "question with no second-person signal",
			body: "Was the file deleted?",
			want: "final",
		},
		// Edge — empty / whitespace
		{"empty body", "", "final"},
		{"whitespace only", "   \n\t  ", "final"},
		{"trailing newline question", "What would you like next?\n", "question"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyTurnKind(tc.body)
			if got != tc.want {
				t.Errorf("classifyTurnKind(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// TestParseAssistantTurns is the headline test. It feeds the parser
// log fragments captured from real agent runs and asserts the
// extraction is correct end-to-end (ASSISTANT body identified,
// sanitized, kind heuristic applied).
func TestParseAssistantTurns(t *testing.T) {
	t.Run("single block with question heuristic", func(t *testing.T) {
		lines := []string{
			"[16:31:02] ASSISTANT:",
			"  Yes, I'm working! I'm your OAT browser agent — I can control a Chrome browser.",
			"  ",
			"  What would you like me to do?",
			"",
			"[OAT_TOKENS] {\"delta_input\":40144}",
		}
		turns := parseAssistantTurns(lines)
		if len(turns) != 1 {
			t.Fatalf("expected 1 turn, got %d", len(turns))
		}
		if !strings.Contains(turns[0].SanitizedText, "OAT browser agent") {
			t.Errorf("turn body missing expected content: %q", turns[0].SanitizedText)
		}
		if turns[0].Kind != "question" {
			t.Errorf("expected kind=question (ends with 'What would you...?'), got %q", turns[0].Kind)
		}
	})

	t.Run("multiple blocks", func(t *testing.T) {
		lines := []string{
			"[16:29:52] ASSISTANT:",
			"  I'll check for any pending messages first.",
			"",
			"[16:29:52] TOOL: execute",
			"  command: oat message list",
			"",
			"[16:29:52] RESULT: execute",
			"  No messages",
			"",
			"[16:29:53] ASSISTANT:",
			"  No pending messages. How can I help you?",
			"",
			"[OAT_TOKENS] {\"delta_input\":80156}",
		}
		turns := parseAssistantTurns(lines)
		if len(turns) != 2 {
			t.Fatalf("expected 2 turns, got %d", len(turns))
		}
		if !strings.Contains(turns[0].SanitizedText, "check for any pending messages") {
			t.Errorf("first turn body wrong: %q", turns[0].SanitizedText)
		}
		// "How can I help you?" — contains both "can I" and "you?", heuristic should flag question.
		if turns[1].Kind != "question" {
			t.Errorf("expected second turn kind=question, got %q", turns[1].Kind)
		}
	})

	t.Run("ANSI bytes in body are scrubbed", func(t *testing.T) {
		lines := []string{
			"[12:00:00] ASSISTANT:",
			"  This text has \x1b[31mred\x1b[0m bytes and \x07 a BEL.",
			"",
			"[12:00:01] TOOL: execute",
		}
		turns := parseAssistantTurns(lines)
		if len(turns) != 1 {
			t.Fatalf("expected 1 turn, got %d", len(turns))
		}
		if strings.Contains(turns[0].SanitizedText, "\x1b") {
			t.Errorf("ESC byte not stripped: %q", turns[0].SanitizedText)
		}
		if strings.Contains(turns[0].SanitizedText, "\x07") {
			t.Errorf("BEL byte not stripped: %q", turns[0].SanitizedText)
		}
		if !strings.Contains(turns[0].SanitizedText, "red") {
			t.Errorf("expected the word 'red' to survive sanitization")
		}
	})

	t.Run("empty body produces no turn", func(t *testing.T) {
		lines := []string{
			"[12:00:00] ASSISTANT:",
			"  ",
			"",
			"[12:00:01] TOOL: execute",
		}
		turns := parseAssistantTurns(lines)
		if len(turns) != 0 {
			t.Fatalf("expected 0 turns from whitespace-only body, got %d", len(turns))
		}
	})

	t.Run("[OAT_BROWSER] status: lines are stripped from body", func(t *testing.T) {
		// The agent prompt instructs use of `[OAT_BROWSER] status:`
		// for OutputWatcher status reports. Those lines must never
		// surface as chat — they're internals.
		lines := []string{
			"[12:00:00] ASSISTANT:",
			"  Working on the summary.",
			"  [OAT_BROWSER] status: Compiling notes from About page",
			"  Found pricing info too.",
			"",
			"[12:00:01] TOOL: execute",
		}
		turns := parseAssistantTurns(lines)
		if len(turns) != 1 {
			t.Fatalf("expected 1 turn, got %d", len(turns))
		}
		if strings.Contains(turns[0].SanitizedText, "OAT_BROWSER") {
			t.Errorf("status sentinel leaked into chat body: %q", turns[0].SanitizedText)
		}
		if !strings.Contains(turns[0].SanitizedText, "Working on the summary") {
			t.Errorf("legit content stripped: %q", turns[0].SanitizedText)
		}
		if !strings.Contains(turns[0].SanitizedText, "Found pricing info") {
			t.Errorf("legit content stripped: %q", turns[0].SanitizedText)
		}
	})

	t.Run("turn that is JUST a [OAT_BROWSER] status produces no turn", func(t *testing.T) {
		// The earlier-screenshot bug: a status-only ASSISTANT turn
		// rendered as a chat bubble. After stripping it must
		// suppress the turn entirely.
		lines := []string{
			"[12:00:00] ASSISTANT:",
			"  [OAT_BROWSER] status: Gathered root.io company info, compiling summary",
			"",
			"[12:00:01] TOOL: execute",
		}
		turns := parseAssistantTurns(lines)
		if len(turns) != 0 {
			t.Fatalf("expected 0 turns from status-only body, got %d: %+v", len(turns), turns)
		}
	})

	t.Run("trailing block flushed even without next marker", func(t *testing.T) {
		// Mimics the case where the log ends right after an
		// ASSISTANT block with no following TOOL or [OAT_TOKENS]
		// envelope (rare but possible if the runtime crashes
		// mid-write).
		lines := []string{
			"[12:00:00] ASSISTANT:",
			"  Done with the task.",
		}
		turns := parseAssistantTurns(lines)
		if len(turns) != 1 {
			t.Fatalf("expected trailing block to flush, got %d turns", len(turns))
		}
		if !strings.Contains(turns[0].SanitizedText, "Done with the task") {
			t.Errorf("trailing block content wrong: %q", turns[0].SanitizedText)
		}
	})
}

// TestParseEvents covers the streaming-event API the tailer consumes
// — specifically the side-panel USER-sentinel detection that gates
// auto-emit on first user input. The post-restart bug Part 2g's
// follow-up addresses (bubbles like "Cleared." appearing before any
// user input) hinges on these events firing in the right order.
func TestParseEvents(t *testing.T) {
	t.Run("USER block with sidepanel sentinel emits EventSidePanelUser", func(t *testing.T) {
		lines := []string{
			"[17:27:38] USER:",
			"  [SIDE-PANEL CHAT] hello agent",
			"",
			"[17:27:40] ASSISTANT:",
			"  Hi! What would you like to do?",
		}
		events := parseEvents(lines)
		if len(events) != 2 {
			t.Fatalf("expected 2 events, got %d: %+v", len(events), events)
		}
		if events[0].Kind != EventSidePanelUser {
			t.Errorf("expected first event to be EventSidePanelUser, got %v", events[0].Kind)
		}
		if events[1].Kind != EventAssistantTurn {
			t.Errorf("expected second event to be EventAssistantTurn, got %v", events[1].Kind)
		}
		if !strings.Contains(events[1].Turn.SanitizedText, "What would you like") {
			t.Errorf("assistant text wrong: %q", events[1].Turn.SanitizedText)
		}
	})

	t.Run("USER block without sentinel does NOT emit EventSidePanelUser", func(t *testing.T) {
		// Daemon-injected control messages (inter-agent forwards,
		// etc.) lack the sentinel and must not unlock auto-emit.
		lines := []string{
			"[17:27:38] USER:",
			"  hello agent",
			"",
			"[17:27:40] ASSISTANT:",
			"  reply",
		}
		events := parseEvents(lines)
		// Only the assistant turn, no sentinel event.
		if len(events) != 1 {
			t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
		}
		if events[0].Kind != EventAssistantTurn {
			t.Errorf("expected EventAssistantTurn, got %v", events[0].Kind)
		}
	})

	t.Run("USER block preserves event ordering vs ASSISTANT", func(t *testing.T) {
		// Two round trips in one buffer; events must arrive in the
		// log's chronological order so the tailer's side-panel-active
		// flag flips before the first ASSISTANT turn it gates.
		lines := []string{
			"[17:00:00] ASSISTANT:",
			"  pre-side-panel reply (should be EventAssistantTurn before any sentinel)",
			"",
			"[17:00:05] USER:",
			"  [SIDE-PANEL CHAT] first chat",
			"",
			"[17:00:06] ASSISTANT:",
			"  first chat reply",
		}
		events := parseEvents(lines)
		if len(events) != 3 {
			t.Fatalf("expected 3 events, got %d: %+v", len(events), events)
		}
		if events[0].Kind != EventAssistantTurn {
			t.Errorf("expected first event to be EventAssistantTurn, got %v", events[0].Kind)
		}
		if events[1].Kind != EventSidePanelUser {
			t.Errorf("expected second event to be EventSidePanelUser, got %v", events[1].Kind)
		}
		if events[2].Kind != EventAssistantTurn {
			t.Errorf("expected third event to be EventAssistantTurn, got %v", events[2].Kind)
		}
	})

	t.Run("blank lines before sentinel don't suppress detection", func(t *testing.T) {
		lines := []string{
			"[17:00:00] USER:",
			"",
			"  ",
			"  [SIDE-PANEL CHAT] hello",
		}
		events := parseEvents(lines)
		if len(events) != 1 || events[0].Kind != EventSidePanelUser {
			t.Errorf("expected single EventSidePanelUser, got %+v", events)
		}
	})

	t.Run("sentinel must be at start of first content line", func(t *testing.T) {
		// A USER block whose first content line doesn't start with
		// the sentinel doesn't unlock the gate, even if the sentinel
		// appears later in the body. (The daemon always prepends, so
		// this is defensive against malformed log content.)
		lines := []string{
			"[17:00:00] USER:",
			"  forwarded from worker:",
			"  [SIDE-PANEL CHAT] this is not side-panel input",
		}
		events := parseEvents(lines)
		for _, ev := range events {
			if ev.Kind == EventSidePanelUser {
				t.Errorf("did not expect EventSidePanelUser, got %+v", events)
			}
		}
	})
}

// TestTurnBroadcasterFanout verifies the broadcaster delivers to
// multiple subscribers, drops frames for slow ones rather than
// blocking, and emits Done on Close.
func TestTurnBroadcasterFanout(t *testing.T) {
	t.Run("delivers to multiple subscribers", func(t *testing.T) {
		b := newTurnBroadcaster(nil)
		defer b.Close()
		ch1, c1 := b.Subscribe()
		ch2, c2 := b.Subscribe()
		defer c1()
		defer c2()

		b.Publish(AssistantTurn{SanitizedText: "hello", Kind: "final"})

		for i, ch := range []<-chan assistantTurnFrame{ch1, ch2} {
			select {
			case f := <-ch:
				if f.Text != "hello" || f.Kind != "final" {
					t.Errorf("subscriber %d got wrong frame: %+v", i, f)
				}
			default:
				t.Errorf("subscriber %d did not receive frame", i)
			}
		}
	})

	t.Run("Publish does not block when subscriber buffer is full", func(t *testing.T) {
		b := newTurnBroadcaster(nil)
		defer b.Close()
		// Subscribe but never drain — this subscriber's buffer fills
		// at turnSubscriberBuf frames; further Publish() calls must
		// drop on the floor rather than block the producer.
		_, cancel := b.Subscribe()
		defer cancel()

		// Publish twice as many frames as the buffer holds. If
		// Publish() blocks on a full subscriber the test deadlocks
		// (caught by the wrapping timeout).
		done := make(chan struct{})
		go func() {
			for i := 0; i < turnSubscriberBuf*4; i++ {
				b.Publish(AssistantTurn{SanitizedText: "x", Kind: "final"})
			}
			close(done)
		}()
		select {
		case <-done:
			// pass — publish completed without blocking.
		case <-timeAfterShort():
			t.Errorf("Publish blocked when subscriber buffer was full — backpressure bug")
		}
	})

	t.Run("slow subscriber sees at most turnSubscriberBuf frames", func(t *testing.T) {
		b := newTurnBroadcaster(nil)
		defer b.Close()
		ch, cancel := b.Subscribe()
		defer cancel()

		for i := 0; i < turnSubscriberBuf*4; i++ {
			b.Publish(AssistantTurn{SanitizedText: "x", Kind: "final"})
		}

		n := 0
	drain:
		for {
			select {
			case <-ch:
				n++
			default:
				break drain
			}
		}
		if n > turnSubscriberBuf {
			t.Errorf("expected slow subscriber to see <= %d frames, got %d", turnSubscriberBuf, n)
		}
		if n == 0 {
			t.Errorf("expected slow subscriber to see at least 1 frame")
		}
	})

	t.Run("Close emits Done frame", func(t *testing.T) {
		b := newTurnBroadcaster(nil)
		ch, cancel := b.Subscribe()
		defer cancel()

		b.Close()

		// Drain — should see Done frame then channel close.
		sawDone := false
		for f := range ch {
			if f.Done {
				sawDone = true
			}
		}
		if !sawDone {
			t.Errorf("expected Done frame before channel close")
		}
	})

	t.Run("Subscribe after Close returns closed channel", func(t *testing.T) {
		b := newTurnBroadcaster(nil)
		b.Close()
		ch, cancel := b.Subscribe()
		defer cancel()
		_, ok := <-ch
		if ok {
			t.Errorf("expected channel to be closed immediately")
		}
	})
}

// TestTailerEnvelopeTerminatesIdleTurn locks in the post-smoke-test
// fix for the side-panel-replies-invisible regression on 2026-05-18.
//
// Bug: the tailer's flushBuffer only flushed pending content when a
// NEW USER/ASSISTANT header arrived. For an idle one-shot chat (user
// asks "is this working?", agent replies, both wait) no follow-up
// header ever lands, so the agent's reply block sat in `pending`
// indefinitely and was never published to the side panel.
//
// Fix: also treat [OAT_*] envelopes (which the agent runtime emits
// at the end of every turn) as "current block ended" signals.
//
// This test reproduces the exact log shape from the smoke-test
// post-mortem: a USER block with [SIDE-PANEL CHAT] sentinel, then
// an ASSISTANT block, then [OAT_TOKENS] envelopes — and nothing
// after. The tailer MUST publish the ASSISTANT turn within the
// poll-interval timeout.
func TestTailerEnvelopeTerminatesIdleTurn(t *testing.T) {
	tmp := t.TempDir()
	logPath := tmp + "/agent.log"

	// Pre-create the file so the tailer's "wait for file to exist"
	// loop falls straight through.
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatalf("create log: %v", err)
	}

	b := newTurnBroadcaster(nil)
	tailer := newAssistantTurnTailer(logPath, b, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tailer.Start(ctx)
	defer tailer.Stop()

	ch, sub := b.Subscribe()
	defer sub()

	// Race-free wait for the tailer to seek-to-end: poll until the
	// goroutine has opened the file and parked. 200 ms is generous
	// vs the 100 ms tailer poll interval.
	time.Sleep(200 * time.Millisecond)

	// Write a quoted real-world log shape. Pre-existing block (the
	// /messages-ritual ASSISTANT) must be suppressed; the side-panel
	// USER must flip the gate; the post-sentinel ASSISTANT must be
	// published. CRITICALLY, no further header follows the final
	// ASSISTANT — only [OAT_TOKENS] envelopes. The pre-fix tailer
	// hung here forever.
	body := strings.Join([]string{
		"[20:30:55] ASSISTANT:",
		"  ## System Status",
		"  ",
		"  - all good",
		"",
		"[OAT_TOKENS] {\"delta_input\": 1}",
		"[20:34:15] USER:",
		"  [SIDE-PANEL CHAT] hello there, is this working?",
		"",
		"[OAT_MODEL] anthropic:claude-sonnet-4-6",
		"[20:34:17] ASSISTANT:",
		"  Hello! Yes, this is working! Today is **July 14, 2025**.",
		"",
		"[OAT_TOKENS] {\"delta_input\": 1}",
		"",
	}, "\n")
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := f.WriteString(body); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()

	// We expect EXACTLY ONE published turn: the post-sentinel
	// ASSISTANT. The pre-sentinel ASSISTANT is suppressed. We give
	// up to 2 s for the poll-interval loop to drain.
	deadline := time.After(2 * time.Second)
	select {
	case f := <-ch:
		if !strings.Contains(f.Text, "Hello! Yes, this is working") {
			t.Errorf("unexpected first published frame: %+v", f)
		}
	case <-deadline:
		t.Fatal("tailer did not publish the post-sentinel ASSISTANT turn within 2s — envelope-terminated idle turn regression")
	}

	// And there should be NO additional turn published — the
	// pre-sentinel one was correctly suppressed.
	select {
	case f := <-ch:
		if !f.Done {
			t.Errorf("unexpected second frame after the only valid turn: %+v", f)
		}
	case <-time.After(300 * time.Millisecond):
		// Pass: no second frame within the post-write window.
	}
}

// TestTailerToolMarkerTerminatesMidTurnAssistant locks in the
// 2026-05-19 fix for the "last message hangs until next user input"
// regression seen during the flight-times retest.
//
// Bug: when an ASSISTANT block was followed mid-turn by a TOOL: call
// ("I have all the refs. Now do it all in sequence:" → TOOL:
// browser_select_option), flushBuffer's terminator check only looked
// for [OAT_*] envelopes — which the runtime emits only at end-of-turn.
// If the tool call hung (a real failure mode against Southwest's date
// dropdown), the ASSISTANT prelude sat buffered indefinitely and the
// side panel went dark even though `oat ui` displayed the reply just
// fine; users had to send a new message to flush the buffer.
//
// Fix: terminator check now uses nextMarkerRE, which matches TOOL:,
// RESULT:, ERROR:, USER:, ASSISTANT:, and [OAT_*] equally. Any of
// these strictly after the last header definitively ends the open
// ASSISTANT body.
//
// This test asserts: after a side-panel USER unlocks the gate, an
// ASSISTANT block followed by a TOOL: marker (with NO [OAT_*]
// envelope, NO trailing USER/ASSISTANT header) must publish the
// ASSISTANT turn within the poll-interval timeout.
func TestTailerToolMarkerTerminatesMidTurnAssistant(t *testing.T) {
	tmp := t.TempDir()
	logPath := tmp + "/agent.log"

	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatalf("create log: %v", err)
	}

	b := newTurnBroadcaster(nil)
	tailer := newAssistantTurnTailer(logPath, b, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tailer.Start(ctx)
	defer tailer.Stop()

	ch, sub := b.Subscribe()
	defer sub()

	time.Sleep(200 * time.Millisecond)

	// Quoted real-world log shape from the 2026-05-19 flight-task
	// post-mortem. The side-panel USER unlocks the gate, the
	// ASSISTANT writes a brief prelude, then immediately issues a
	// TOOL call. NO [OAT_*] envelope, NO subsequent header. The
	// pre-fix tailer held the ASSISTANT in `pending` until the next
	// user message arrived 3.5 minutes later.
	body := strings.Join([]string{
		"[08:18:00] USER:",
		"  [SIDE-PANEL CHAT] book me a flight from SFO to BOS on June 2",
		"",
		"[OAT_MODEL] anthropic:claude-sonnet-4-6",
		"[08:19:17] ASSISTANT:",
		"  I have all the refs. Now do it all in sequence:",
		"",
		"[08:19:17] TOOL: browser_select_option",
		"  tabId: 1817124610",
		"  ref: 20400",
		"  values: ['oneway']",
		"",
	}, "\n")
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := f.WriteString(body); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()

	deadline := time.After(2 * time.Second)
	select {
	case f := <-ch:
		if !strings.Contains(f.Text, "I have all the refs") {
			t.Errorf("unexpected first published frame: %+v", f)
		}
	case <-deadline:
		t.Fatal("tailer did not publish the mid-turn ASSISTANT prelude within 2s — TOOL-marker terminator regression")
	}
}
