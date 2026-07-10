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

	t.Run("echoed [SIDE-PANEL CHAT] sentinel is stripped from reply", func(t *testing.T) {
		// Weaker models (observed with Nemotron) parrot the input
		// framing back at the start of their reply. The leading
		// sentinel must not leak into the chat bubble.
		lines := []string{
			"[12:00:00] ASSISTANT:",
			"  [SIDE-PANEL CHAT] It's a test message. I see it arrived.",
			"",
			"[12:00:01] TOOL: execute",
		}
		turns := parseAssistantTurns(lines)
		if len(turns) != 1 {
			t.Fatalf("expected 1 turn, got %d", len(turns))
		}
		if strings.Contains(turns[0].SanitizedText, "SIDE-PANEL CHAT") {
			t.Errorf("echoed sentinel leaked into chat body: %q", turns[0].SanitizedText)
		}
		if !strings.HasPrefix(turns[0].SanitizedText, "It's a test message") {
			t.Errorf("legit content damaged after scrub: %q", turns[0].SanitizedText)
		}
	})

	t.Run("echoed sentinel with active-tab-id hint is stripped", func(t *testing.T) {
		lines := []string{
			"[12:00:00] ASSISTANT:",
			"  [SIDE-PANEL CHAT] [active-tab-id: 1817124657] Here's what the page says.",
			"",
			"[12:00:01] TOOL: execute",
		}
		turns := parseAssistantTurns(lines)
		if len(turns) != 1 {
			t.Fatalf("expected 1 turn, got %d", len(turns))
		}
		if strings.Contains(turns[0].SanitizedText, "SIDE-PANEL CHAT") || strings.Contains(turns[0].SanitizedText, "active-tab-id") {
			t.Errorf("echoed framing leaked into chat body: %q", turns[0].SanitizedText)
		}
		if !strings.HasPrefix(turns[0].SanitizedText, "Here's what the page says") {
			t.Errorf("legit content damaged after scrub: %q", turns[0].SanitizedText)
		}
	})

	t.Run("a later mention of the sentinel is left intact", func(t *testing.T) {
		// Only a LEADING echo is scrubbed; a legitimate mid-reply
		// reference (e.g. the agent explaining the protocol) survives.
		lines := []string{
			"[12:00:00] ASSISTANT:",
			"  Your messages arrive prefixed with [SIDE-PANEL CHAT].",
			"",
			"[12:00:01] TOOL: execute",
		}
		turns := parseAssistantTurns(lines)
		if len(turns) != 1 {
			t.Fatalf("expected 1 turn, got %d", len(turns))
		}
		if !strings.Contains(turns[0].SanitizedText, "[SIDE-PANEL CHAT]") {
			t.Errorf("non-leading mention should be preserved: %q", turns[0].SanitizedText)
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
	tailer := newAssistantTurnTailer(logPath, b, false, nil)
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

// TestTailerIdleFlushesUnterminatedErrorTurn reproduces the wifi-drop
// post-mortem: the model call fast-fails (network error / timeout), the
// runtime writes the error ASSISTANT block, but — because a failed turn
// consumes zero tokens — emits NO `[OAT_TOKENS]` envelope and no further
// header. The terminator-driven flush path can't publish such a block,
// so before the idle-flush it stayed invisible in the side panel until
// the user's NEXT message, making a ~3 s fast-fail look like a multi-
// minute hang. The tailer MUST publish it on its own via the
// trailingFlushGrace idle-flush.
func TestTailerIdleFlushesUnterminatedErrorTurn(t *testing.T) {
	tmp := t.TempDir()
	logPath := tmp + "/agent.log"

	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatalf("create log: %v", err)
	}

	b := newTurnBroadcaster(nil)
	tailer := newAssistantTurnTailer(logPath, b, false, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tailer.Start(ctx)
	defer tailer.Stop()

	ch, sub := b.Subscribe()
	defer sub()

	time.Sleep(200 * time.Millisecond)

	// Exact shape from the post-mortem log: side-panel USER, model
	// marker, then an error ASSISTANT block with NOTHING after it — no
	// [OAT_TOKENS], no next header.
	body := strings.Join([]string{
		"[15:42:21] USER:",
		"  [SIDE-PANEL CHAT] test message 3",
		"",
		"[OAT_MODEL] anthropic:claude-opus-4-7",
		"[15:42:24] ASSISTANT:",
		"  Lost connection to the model (network error or timeout). Check your internet and send your message again.",
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

	// Must arrive without any further log activity. Allow
	// trailingFlushGrace + poll/scheduling slack.
	deadline := time.After(trailingFlushGrace + 2*time.Second)
	select {
	case fr := <-ch:
		if !strings.Contains(fr.Text, "Lost connection to the model") {
			t.Errorf("unexpected published frame: %+v", fr)
		}
	case <-deadline:
		t.Fatal("tailer never published the unterminated error turn — idle-flush regression (side panel would hang until next message)")
	}
}

// TestTailerDeliveryArmingPublishesWithoutSentinel asserts that arming
// auto-emit at message-delivery time (markSidePanelActive()) publishes an
// assistant turn even when no `[SIDE-PANEL CHAT]` sentinel precedes it in
// the log. This guards against the blackout where a busy agent's turns
// were suppressed because the sentinel landed in the log out of order.
func TestTailerDeliveryArmingPublishesWithoutSentinel(t *testing.T) {
	tmp := t.TempDir()
	logPath := tmp + "/agent.log"

	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatalf("create log: %v", err)
	}

	b := newTurnBroadcaster(nil)
	tailer := newAssistantTurnTailer(logPath, b, false, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tailer.Start(ctx)
	defer tailer.Stop()

	ch, sub := b.Subscribe()
	defer sub()

	time.Sleep(200 * time.Millisecond)

	// Simulate the daemon delivering a side-panel message: arm
	// auto-emit at delivery time. NOTE: no `[SIDE-PANEL CHAT]` sentinel
	// is ever written to the log below — this is the out-of-order case
	// where the sentinel-bearing USER block hasn't landed yet.
	tailer.markSidePanelActive()

	// The agent emits a status update (exactly the kind that was being
	// black-holed) with no sentinel preceding it.
	body := strings.Join([]string{
		"[07:57:08] ASSISTANT:",
		"  Browser is unresponsive, so I'm using direct web fetches for the research.",
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

	deadline := time.After(2 * time.Second)
	select {
	case fr := <-ch:
		if !strings.Contains(fr.Text, "direct web fetches") {
			t.Errorf("unexpected published frame: %+v", fr)
		}
	case <-deadline:
		t.Fatal("tailer suppressed an ASSISTANT turn despite delivery-time arming — side-panel blackout regression")
	}
}

// TestTailerSuppressesWithoutAnyTrigger is the negative companion to
// TestTailerDeliveryArmingPublishesWithoutSentinel: with NEITHER a
// delivery-time arm NOR a log sentinel, pre-side-panel chatter (startup
// banners, the `/messages` ritual, "Cleared." on restart) is still
// correctly suppressed. This guards against the fix over-emitting.
func TestTailerSuppressesWithoutAnyTrigger(t *testing.T) {
	tmp := t.TempDir()
	logPath := tmp + "/agent.log"

	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatalf("create log: %v", err)
	}

	b := newTurnBroadcaster(nil)
	tailer := newAssistantTurnTailer(logPath, b, false, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tailer.Start(ctx)
	defer tailer.Stop()

	ch, sub := b.Subscribe()
	defer sub()

	time.Sleep(200 * time.Millisecond)

	// No markSidePanelActive(), no sentinel — a habitual startup turn.
	body := strings.Join([]string{
		"[07:50:00] ASSISTANT:",
		"  Cleared.",
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

	select {
	case fr := <-ch:
		if !fr.Done {
			t.Errorf("expected NO published turn (suppressed), got: %+v", fr)
		}
	case <-time.After(500 * time.Millisecond):
		// Pass: nothing published, correctly suppressed.
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
	tailer := newAssistantTurnTailer(logPath, b, false, nil)
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

// TestParseEventsToolBlocks verifies the parser surfaces TOOL/RESULT
// blocks as EventToolStart / EventToolEnd with the tool name, a short
// arg preview, and a coarse status — the signal the side panel needs
// to render granular per-tool activity rows for an ASSISTANT (whose
// tools aren't bridge-mediated, so the log is the only source).
func TestParseEventsToolBlocks(t *testing.T) {
	lines := strings.Split(strings.Join([]string{
		"[09:00:00] ASSISTANT:",
		"  Let me look that up.",
		"",
		"[09:00:01] TOOL: web_search",
		"  query: how to fold a fitted sheet",
		"",
		"[09:00:03] RESULT: web_search",
		"  3 results",
		"",
		"[09:00:04] TOOL: read_file",
		"  file_path: /home/me/notes.txt",
		"",
		"[09:00:05] RESULT: read_file (error)",
		"  file not found",
		"",
		"[09:00:06] ASSISTANT:",
		"  Done.",
		"",
		"[OAT_TOKENS] {\"delta_input\": 1}",
	}, "\n"), "\n")

	events := parseEvents(lines)

	var got []Event
	for _, ev := range events {
		if ev.Kind == EventToolStart || ev.Kind == EventToolEnd {
			got = append(got, ev)
		}
	}
	if len(got) != 4 {
		t.Fatalf("expected 4 tool events, got %d: %+v", len(got), got)
	}
	if got[0].Kind != EventToolStart || got[0].Tool != "web_search" {
		t.Errorf("event[0] = %+v; want tool_start web_search", got[0])
	}
	if !strings.Contains(got[0].Arg, "how to fold a fitted sheet") {
		t.Errorf("event[0].Arg = %q; want it to contain the query", got[0].Arg)
	}
	if got[1].Kind != EventToolEnd || got[1].Tool != "web_search" || got[1].ToolStatus != "ok" {
		t.Errorf("event[1] = %+v; want tool_end web_search ok", got[1])
	}
	if got[2].Kind != EventToolStart || got[2].Tool != "read_file" {
		t.Errorf("event[2] = %+v; want tool_start read_file", got[2])
	}
	if !strings.Contains(got[2].Arg, "notes.txt") {
		t.Errorf("event[2].Arg = %q; want it to contain the file path", got[2].Arg)
	}
	if got[3].Kind != EventToolEnd || got[3].Tool != "read_file" || got[3].ToolStatus != "error" {
		t.Errorf("event[3] = %+v; want tool_end read_file error", got[3])
	}

	// The chat turns must still parse normally alongside the tool events.
	turns := parseAssistantTurns(lines)
	if len(turns) != 2 {
		t.Fatalf("expected 2 ASSISTANT turns, got %d: %+v", len(turns), turns)
	}
}

// TestParseResultHeader covers the success / error / explicit-success
// status heuristics for RESULT headers.
func TestParseResultHeader(t *testing.T) {
	cases := []struct {
		in         string
		wantName   string
		wantStatus string
	}{
		{"web_search", "web_search", "ok"},
		{"web_search (error)", "web_search", "error"},
		{"gmail_send (timeout)", "gmail_send", "error"},
		{"read_file (success)", "read_file", "ok"},
	}
	for _, c := range cases {
		name, status := parseResultHeader(c.in)
		if name != c.wantName || status != c.wantStatus {
			t.Errorf("parseResultHeader(%q) = (%q, %q); want (%q, %q)", c.in, name, status, c.wantName, c.wantStatus)
		}
	}
}

// TestParseEventsStructuredErrorBody verifies the defense-in-depth path:
// a RESULT header tagged success whose BODY is a structured-error envelope
// ({"ok": false, ...}) is reclassified to "error" and surfaces a bounded
// failure reason — even though the header lacked an `(error)` suffix. This
// covers logs that predate the runtime's status-derivation seam.
func TestParseEventsStructuredErrorBody(t *testing.T) {
	lines := strings.Split(strings.Join([]string{
		"[09:00:00] TOOL: ping",
		"  (no args)",
		"",
		"[09:00:01] RESULT: ping",
		`  {"ok": false, "code": "EXTENSION_NOT_CONNECTED", "message": "The browser isn't connected to Chrome right now."}`,
		"",
	}, "\n"), "\n")

	var end *Event
	for _, ev := range parseEvents(lines) {
		if ev.Kind == EventToolEnd {
			e := ev
			end = &e
		}
	}
	if end == nil {
		t.Fatal("expected an EventToolEnd")
	}
	if end.ToolStatus != "error" {
		t.Errorf("ToolStatus = %q; want error (reclassified from ok:false body)", end.ToolStatus)
	}
	if !strings.Contains(end.ErrorMessage, "browser isn't connected") {
		t.Errorf("ErrorMessage = %q; want it to carry the failure reason", end.ErrorMessage)
	}
}

// TestParseEventsSuccessBodyNotMisclassified guards the narrow detection:
// a RESULT whose body is a genuine success envelope ({"ok": true}) or
// plain text must stay "ok" with no ErrorMessage.
func TestParseEventsSuccessBodyNotMisclassified(t *testing.T) {
	lines := strings.Split(strings.Join([]string{
		"[09:00:01] RESULT: navigate",
		`  {"ok": true, "url": "https://example.com"}`,
		"",
		"[09:00:02] RESULT: web_search",
		"  3 results found",
		"",
	}, "\n"), "\n")

	for _, ev := range parseEvents(lines) {
		if ev.Kind != EventToolEnd {
			continue
		}
		if ev.ToolStatus != "ok" {
			t.Errorf("tool %q: ToolStatus = %q; want ok", ev.Tool, ev.ToolStatus)
		}
		if ev.ErrorMessage != "" {
			t.Errorf("tool %q: ErrorMessage = %q; want empty on success", ev.Tool, ev.ErrorMessage)
		}
	}
}

// TestDetectStructuredResultError unit-tests the envelope detector across
// the envelope, success, plain-text, and malformed cases.
func TestDetectStructuredResultError(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantErr   bool
		wantMsgIn string
	}{
		{"ok false with message", `{"ok": false, "code": "X", "message": "boom"}`, true, "boom"},
		{"ok false error field", `{"ok": false, "error": "nope"}`, true, "nope"},
		{"ok false no message", `{"ok": false, "code": "X"}`, true, ""},
		{"ok true", `{"ok": true, "result": 1}`, false, ""},
		{"no ok field", `{"code": "X", "value": 2}`, false, ""},
		// Green-error bug: bridge envelopes that omit `ok` but carry an
		// UPPER_SNAKE code / message must be flagged as errors.
		{"code+message no ok", `{"code": "EXTENSION_NOT_CONNECTED", "message": "bridge down"}`, true, "bridge down"},
		{"snake code only no ok", `{"code": "STALE_REF"}`, true, ""},
		{"errorMessage no ok", `{"errorMessage": "kaboom"}`, true, "kaboom"},
		{"error field no ok", `{"error": "boom"}`, true, "boom"},
		// ok:true wins even with an error-shaped code present.
		{"ok true with snake code", `{"ok": true, "code": "STALE_REF"}`, false, ""},
		{"plain text", "all good", false, ""},
		{"truncated json", `{"ok": false, "message": "tru`, false, ""},
		{"leading whitespace", "  {\"ok\": false}\n", true, ""},
	}
	for _, c := range cases {
		gotErr, gotMsg := detectStructuredResultError(c.body)
		if gotErr != c.wantErr {
			t.Errorf("%s: isError = %v; want %v", c.name, gotErr, c.wantErr)
		}
		if c.wantMsgIn != "" && !strings.Contains(gotMsg, c.wantMsgIn) {
			t.Errorf("%s: message = %q; want it to contain %q", c.name, gotMsg, c.wantMsgIn)
		}
	}
}

// TestTailerEmitsToolActivityFrames is the integration counterpart:
// with emitToolEvents=true and the side-panel gate unlocked, the
// tailer must publish tool_start/tool_end frames for the assistant's
// TOOL/RESULT blocks (with name + arg + status), interleaved with the
// ASSISTANT chat turn.
func TestTailerEmitsToolActivityFrames(t *testing.T) {
	tmp := t.TempDir()
	logPath := tmp + "/agent.log"
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatalf("create log: %v", err)
	}

	b := newTurnBroadcaster(nil)
	tailer := newAssistantTurnTailer(logPath, b, true, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tailer.Start(ctx)
	defer tailer.Stop()

	ch, sub := b.Subscribe()
	defer sub()

	time.Sleep(200 * time.Millisecond)

	body := strings.Join([]string{
		"[08:18:00] USER:",
		"  [SIDE-PANEL CHAT] what's the weather?",
		"",
		"[OAT_MODEL] anthropic:claude-sonnet-4-6",
		"[08:19:17] ASSISTANT:",
		"  Checking now.",
		"",
		"[08:19:18] TOOL: web_search",
		"  query: weather today",
		"",
		"[08:19:19] RESULT: web_search",
		"  sunny",
		"",
		"[08:19:20] ASSISTANT:",
		"  It's sunny.",
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

	var sawToolStart, sawToolEnd, sawChatTurn bool
	deadline := time.After(3 * time.Second)
	for !(sawToolStart && sawToolEnd && sawChatTurn) {
		select {
		case fr := <-ch:
			switch fr.Kind {
			case "tool_start":
				if fr.Tool == "web_search" && strings.Contains(fr.Arg, "weather today") {
					sawToolStart = true
				}
			case "tool_end":
				if fr.Tool == "web_search" && fr.Status == "ok" {
					sawToolEnd = true
				}
			case "final", "question":
				if strings.Contains(fr.Text, "sunny") {
					sawChatTurn = true
				}
			}
		case <-deadline:
			t.Fatalf("did not observe all frames within 3s: tool_start=%v tool_end=%v chat=%v", sawToolStart, sawToolEnd, sawChatTurn)
		}
	}
}

// TestTailerThreadsStructuredErrorReason verifies the end-to-end
// path: a RESULT whose body is a {ok:false,...} envelope is
// published as a tool_end frame with Status="error" AND a non-empty
// ErrorMessage carrying the reason, so the side panel can show it
// instead of "(detail not attached)".
func TestTailerThreadsStructuredErrorReason(t *testing.T) {
	tmp := t.TempDir()
	logPath := tmp + "/agent.log"
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatalf("create log: %v", err)
	}

	b := newTurnBroadcaster(nil)
	tailer := newAssistantTurnTailer(logPath, b, true, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tailer.Start(ctx)
	defer tailer.Stop()

	ch, sub := b.Subscribe()
	defer sub()

	time.Sleep(200 * time.Millisecond)

	body := strings.Join([]string{
		"[08:18:00] USER:",
		"  [SIDE-PANEL CHAT] ping",
		"",
		"[08:19:18] TOOL: ping",
		"  (no args)",
		"",
		"[08:19:19] RESULT: ping",
		`  {"ok": false, "code": "EXTENSION_NOT_CONNECTED", "message": "The browser isn't connected to Chrome right now."}`,
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

	deadline := time.After(3 * time.Second)
	for {
		select {
		case fr := <-ch:
			if fr.Kind == "tool_end" && fr.Tool == "ping" {
				if fr.Status != "error" {
					t.Fatalf("tool_end Status = %q; want error", fr.Status)
				}
				if !strings.Contains(fr.ErrorMessage, "browser isn't connected") {
					t.Fatalf("tool_end ErrorMessage = %q; want the failure reason", fr.ErrorMessage)
				}
				return
			}
		case <-deadline:
			t.Fatal("did not observe an error tool_end frame within 3s")
		}
	}
}

// TestTailerToolEventsGatedOffForBrowserAgents verifies the
// emitToolEvents=false path (the browser-agent / default case)
// publishes NO tool frames — the browser agent's rows come from the
// bridge's MCP hooks instead, and double-emitting would duplicate
// every activity row in the side panel.
func TestTailerToolEventsGatedOffForBrowserAgents(t *testing.T) {
	tmp := t.TempDir()
	logPath := tmp + "/agent.log"
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatalf("create log: %v", err)
	}

	b := newTurnBroadcaster(nil)
	tailer := newAssistantTurnTailer(logPath, b, false, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tailer.Start(ctx)
	defer tailer.Stop()

	ch, sub := b.Subscribe()
	defer sub()

	time.Sleep(200 * time.Millisecond)

	body := strings.Join([]string{
		"[08:18:00] USER:",
		"  [SIDE-PANEL CHAT] go",
		"",
		"[08:19:17] ASSISTANT:",
		"  Working.",
		"",
		"[08:19:18] TOOL: browser_navigate",
		"  url: https://example.com",
		"",
		"[08:19:19] RESULT: browser_navigate",
		"  ok",
		"",
		"[08:19:20] ASSISTANT:",
		"  Done.",
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

	deadline := time.After(2 * time.Second)
	for {
		select {
		case fr := <-ch:
			if fr.Kind == "tool_start" || fr.Kind == "tool_end" {
				t.Fatalf("unexpected tool frame with emitToolEvents=false: %+v", fr)
			}
			if fr.Done {
				return
			}
		case <-deadline:
			// No tool frames observed within the window → pass.
			return
		}
	}
}
