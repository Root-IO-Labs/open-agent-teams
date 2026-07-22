package daemon

import (
	"strings"
	"unicode"
)

// Side-panel status-check classification (Workstream B hybrid 3).
// Pure status pings must answer with a concrete diagnosis before tools
// and must NOT reset the assistant recovery budget. Messages that also
// carry new/changed task instructions reset the budget as a fresh sequence.

type sidePanelMsgKind int

const (
	sidePanelMsgNewWork sidePanelMsgKind = iota
	sidePanelMsgPureStatus
)

// Pure-status fail-closed allowlist fragments. Matching is case-insensitive
// on the stripped user text (after removing the side-panel sentinel /
// active-tab prefix if present). A match alone is NOT enough — the message
// must also lack task-instruction signals (see hasTaskInstructionSignal).
var pureStatusPhrases = []string{
	"are you stuck",
	"are you still working",
	"still working",
	"what's the problem",
	"whats the problem",
	"what is the problem",
	"what's causing",
	"whats causing",
	"what happened",
	"why are you stuck",
	"why did you stop",
	"why don't you answer",
	"why dont you answer",
	"you seem stuck",
	"you've been stuck",
	"youve been stuck",
	"stuck again",
	"you are stuck",
	"you're stuck",
	"youre stuck",
	"what's going on",
	"whats going on",
	"what is going on",
	"hello???",
	"hello?",
	"you there",
	"still there",
	"ping",
}

// stuckStatusPhrases are the pure-status asks that mean "I think you
// stalled" (vs soft presence checks like "ping" / "hello?"). Matching
// one of these while the recovery budget is spent grants a one-shot
// stuck-status parachute so Layer-2 can fire again if the next turn
// also ends silent.
var stuckStatusPhrases = []string{
	"are you stuck",
	"stuck again",
	"you are stuck",
	"you're stuck",
	"youre stuck",
	"you seem stuck",
	"you've been stuck",
	"youve been stuck",
	"why are you stuck",
	"why did you stop",
	"why don't you answer",
	"why dont you answer",
	"what's the problem",
	"whats the problem",
	"what is the problem",
	"what's causing",
	"whats causing",
	"what happened",
	"what's going on",
	"whats going on",
	"what is going on",
	"are you still working",
	"still working",
}

// Task-instruction signals — if present alongside a status phrase, treat
// as new work (reset budget) rather than pure status.
var taskInstructionSignals = []string{
	"also ",
	"instead ",
	"now do ",
	"please do ",
	"can you ",
	"could you ",
	"i want you to",
	"i need you to",
	"continue with",
	"switch to",
	"map the",
	"document ",
	"navigate ",
	"open ",
	"go to ",
	"write ",
	"create ",
	"fix ",
	"try again",
	"retry",
}

// statusDiagnosePrefix is prepended (as [OAT-system]) before the user's
// side-panel text for pure status pings. Code-only — does not echo user text.
// After the short diagnosis the agent must CONTINUE the task (unless
// blocked waiting on the user) — answering-only then going idle is the
// "you're stuck again" failure mode this prefix exists to prevent.
const statusDiagnosePrefix = "[OAT-system] The user asked a status question. " +
	"First answer in 1-2 short lines with a CONCRETE cause " +
	"(last tool that succeeded or failed and its error code if any, " +
	"or that you stopped generating after a successful tool with no next step). " +
	"Do NOT apologize-only. " +
	"THEN continue the task with tools unless you are blocked waiting on the user " +
	"(e.g. login/SSO). Do not end the turn after only the status answer.\n"

// statusPlusTaskPrefix for status+new-work combos.
const statusPlusTaskPrefix = "[OAT-system] The user asked about status AND gave new instructions. " +
	"First answer the status ask in one concrete line (no apology-only), " +
	"then follow their new instructions.\n"

// stripSidePanelDecorations removes the daemon-added sentinel / tab prefix
// so classification sees the user's words. Safe on already-raw text.
func stripSidePanelDecorations(text string) string {
	s := strings.TrimSpace(text)
	if strings.HasPrefix(s, sidePanelInputSentinel) {
		s = strings.TrimSpace(strings.TrimPrefix(s, sidePanelInputSentinel))
	}
	if strings.HasPrefix(s, "[active-tab-id:") {
		if idx := strings.Index(s, "]"); idx >= 0 {
			s = strings.TrimSpace(s[idx+1:])
		}
	}
	return s
}

func normalizeForStatusMatch(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		prevSpace = false
		b.WriteRune(r)
	}
	return b.String()
}

func hasTaskInstructionSignal(norm string) bool {
	for _, sig := range taskInstructionSignals {
		if strings.Contains(norm, sig) {
			return true
		}
	}
	// Long messages are almost never pure status.
	if len(norm) > 160 {
		return true
	}
	return false
}

func matchesPureStatusPhrase(norm string) bool {
	for _, p := range pureStatusPhrases {
		if p == "ping" || p == "hello?" || p == "hello???" {
			// Ultra-short phrases: exact match only (avoid "mapping"/"shopping").
			if norm == p || strings.TrimRight(norm, "?!") == strings.TrimRight(p, "?!") {
				return true
			}
			continue
		}
		if norm == p || strings.HasPrefix(norm, p) || strings.Contains(norm, p) {
			return true
		}
	}
	return false
}

// classifySidePanelUserMessage returns whether the (sanitized, possibly
// pre-sentinel) user text is a pure status ping vs new work.
func classifySidePanelUserMessage(text string) sidePanelMsgKind {
	norm := normalizeForStatusMatch(stripSidePanelDecorations(text))
	if norm == "" {
		return sidePanelMsgNewWork
	}
	if !matchesPureStatusPhrase(norm) {
		return sidePanelMsgNewWork
	}
	if hasTaskInstructionSignal(norm) {
		return sidePanelMsgNewWork
	}
	return sidePanelMsgPureStatus
}

// looksLikeStatusAsk is true when the text contains a status phrase even
// if it also has task instructions (used to choose statusPlusTaskPrefix).
func looksLikeStatusAsk(text string) bool {
	norm := normalizeForStatusMatch(stripSidePanelDecorations(text))
	return matchesPureStatusPhrase(norm)
}

// looksLikeStuckStatusAsk is true for "I think you stalled" status
// phrasing (not soft presence checks like "ping" / "hello?").
func looksLikeStuckStatusAsk(text string) bool {
	norm := normalizeForStatusMatch(stripSidePanelDecorations(text))
	if norm == "" {
		return false
	}
	for _, p := range stuckStatusPhrases {
		if norm == p || strings.HasPrefix(norm, p) || strings.Contains(norm, p) {
			return true
		}
	}
	return false
}
