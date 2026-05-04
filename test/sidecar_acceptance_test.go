// Package test_sidecar_acceptance_test holds end-to-end acceptance
// tests for the astream sidecar pipeline.
//
// Status: SKELETON (Phase 5c of Path B). The test cases below are
// scaffolding — each has a clear Scenario/GIVEN/WHEN/THEN comment and
// calls t.Skip("TODO: implement") until its harness is wired. The
// commit that removes the Skip from a given test is the commit that
// lands that test's coverage.
//
// Why skeletons first: gives reviewers a contract to read, locks in
// the acceptance bar from the project plan, and prevents "I forgot
// we were going to test that" regressions. Filling them in is the
// follow-up work; merging the stubs is a pre-commitment.
//
// These live under test/ (not pkg/.../*_test.go) because they exercise
// the full daemon + backend + Python-subprocess stack and are expensive
// enough to deserve their own package. Run with:
//
//	go test ./test/... -run "Acceptance_Sidecar" -v -timeout=5m
package test

import (
	"testing"
)

// Acceptance_Sidecar_AssistantMessageRendersInTUI verifies the visible
// fix for the original ghosting bug.
//
// Given: a daemon running with OAT_USE_SIDECAR=1 and one fresh worker
// When: the worker completes a turn that produces assistant text
// Then: the text appears in App.outputContent for that agent with
//
//	line_type="text", sourced from the sidecar (not PTY scrape),
//	byte-identical to what the Python sidecar_emitter emitted.
//
// Strongest proof that chat content now rides the lossless event path
// all the way to the TUI's render buffer.
func TestAcceptance_Sidecar_AssistantMessageRendersInTUI(t *testing.T) {
	t.Skip("TODO: stand up daemon + fake Python agent + TUI App; emit one assistant_message; assert outputContent")
}

// Acceptance_Sidecar_ToolCallAndResultRenderStructured verifies the
// tool-call pipeline delivers structured data (name, args, result)
// end-to-end rather than reconstructing from PTY regex scrape.
//
// Given: worker with OAT_USE_SIDECAR=1
// When: Python emits tool_call with known name+args, then tool_result
// Then: TUI renders two lines — "● <name>" with line_type=tool_call,
//
//	"⎿ <content>" with line_type=tool_output — and no PTY-
//	originated duplicate appears (PTY suppression armed).
func TestAcceptance_Sidecar_ToolCallAndResultRenderStructured(t *testing.T) {
	t.Skip("TODO: emit tool_call + tool_result via sidecar; assert lineTypes + PTY suppression")
}

// Acceptance_Sidecar_TokenParityWithConversationLogger is the byte-
// parity acceptance: every token-accounting cumulative reported to
// the daemon via the sidecar path matches the corresponding
// ConversationLogger output byte-for-byte.
//
// Given: agent runs a multi-turn conversation with token emissions
// When: collecting both the daemon's recorded token state and the
//
//	agent's ConversationLogger.jsonl
//
// Then: per-turn cumulative_input / cumulative_output match exactly,
//
//	and cache_read / cache_creation match exactly when present.
//
// If this fails, the monotonicity guard's dedup is off or the
// sidecar is dropping events — either case, accounting is broken.
func TestAcceptance_Sidecar_TokenParityWithConversationLogger(t *testing.T) {
	t.Skip("TODO: parse daemon state + ConversationLogger jsonl; diff cumulatives field-by-field")
}

// Acceptance_Sidecar_NoLossUnderChromeFlood reproduces the original
// rawBroadcaster failure mode and proves the sidecar is immune.
//
// Given: worker emitting structured events via sidecar AND chrome
//
//	PTY output at 200 lines/sec (simulates a Textual redraw storm
//	that would saturate rawBroadcaster's 512-slot channel)
//
// When: the worker emits 50 assistant_message events over the burst
// Then: all 50 messages reach App.outputContent unaltered — zero
//
//	drops, zero reorders, zero truncations. If the event stream
//	were riding rawBroadcaster's channel this would fail; it rides
//	eventBroadcaster's blocking-send-with-timeout path so it must
//	succeed.
func TestAcceptance_Sidecar_NoLossUnderChromeFlood(t *testing.T) {
	t.Skip("TODO: synthetic chrome flood goroutine + emit-and-count verification")
}

// Acceptance_Sidecar_ReconnectReplaysCatchup verifies that a TUI
// reconnecting mid-session receives the ring-buffered history and
// picks up live events seamlessly.
//
// Given: a worker with an active event stream and at least 50 events
//
//	already emitted this session
//
// When: a new TUI subscriber connects via stream_events
// Then: the subscriber receives the ring-buffer catchup (oldest-first)
//
//	before any new live event, the catchup is in order, and
//	subsequent live events arrive without gaps (no seq skipping).
func TestAcceptance_Sidecar_ReconnectReplaysCatchup(t *testing.T) {
	t.Skip("TODO: emit N events, subscribe late, assert catchup ordering + live resumption")
}

// Acceptance_Sidecar_TurnBoundariesDriveThinkingIndicator is the
// correctness test for the Phase-5c thinking-indicator fix.
//
// Given: a worker mid-turn (sidecar has delivered turn_start but not
//
//	turn_end)
//
// When: elapsed > 3s with no output
// Then: App.thinkingIndicator returns a non-empty "processing..."
//
//	string; after turn_end arrives, it returns "".
//
// Prevents the stuck-indicator regression: without this check, the
// indicator would stay visible for up to 120s after the agent
// finished responding, confusing users.
func TestAcceptance_Sidecar_TurnBoundariesDriveThinkingIndicator(t *testing.T) {
	t.Skip("TODO: simulate turn_start → idle → turn_end and assert indicator visibility")
}

// Acceptance_Sidecar_FlagOffIsByteIdenticalToToday is the no-
// regression acceptance. With OAT_USE_SIDECAR unset, every visible
// behavior must match what shipped before this branch.
//
// Given: daemon started without OAT_USE_SIDECAR
// When: same worker scenario as the other acceptance tests
// Then: no /tmp/oat-sdcr-*.sock files are created; no
//
//	stream_events subscriptions in daemon.log; TUI renders from
//	PTY exactly as before; token tracking still works via the
//	[OAT_TOKENS] stdout path.
//
// Lock-in against accidental sidecar-always-on during PR review.
func TestAcceptance_Sidecar_FlagOffIsByteIdenticalToToday(t *testing.T) {
	t.Skip("TODO: assert zero sockets, PTY-only rendering, token tracking still works")
}
