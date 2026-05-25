// Package daemon — context_capacity.go owns the Part 5e
// conversation-context safety net for AgentTypeAssistant. The
// background: a long-lived assistant accumulates tokens in its
// LLM context window until the LLM rejects the request with
// `prompt is too long`, the runtime exits, the daemon's health-
// check loop restarts it with `--resume`, the SAME context is
// re-sent, the LLM rejects again. After 3 crashes in 10 minutes
// the existing browser-agent back-off (inherited via Part 5a)
// marks the agent disabled and the user's assistant goes dark.
// The safety net prevents that loop with daemon-side capacity
// awareness layered on top of the existing token-tracking
// pipeline.
//
// V1 ships TWO of the four documented tiers:
//
//   - 75% (silent PTY hint): periodic poll from
//     handleTokenUsageEvent. Emits a one-line
//     `[OAT-system] You are at 75% effective context capacity…`
//     directive into the assistant's PTY, suppressed by an
//     in-memory dedupe so it doesn't spam every token-event.
//   - 95% (synthetic compaction inject before user msg): on
//     handleAgentInput (the side-panel chat path), if the agent
//     is at >= 95 % of its effective limit, prepend a synthetic
//     `[OAT-system] please call compact_conversation now`
//     directive before forwarding the user's message. Gated
//     behind `OAT_CONTEXT_SAFETY_NET` env var (default ON).
//
// The 85% (status-tab pill) and 90% (user-visible banner with
// Compact / Reset buttons) tiers are documented in the plan
// body but deferred to a follow-up: they need a new bridge WS
// frame + extension-side UI surface that's expensive to add
// right now and not on the critical path for the "assistant
// doesn't crash-loop at 100%" outcome. The 75% / 95% tiers
// alone cover the failure mode end-to-end.
//
// Why in-memory dedupe instead of a state.Agent field: a
// restarted daemon SHOULD re-emit the hint (semantically: "I
// just observed you're hot, here's the nudge"), and not
// persisting suppression to state.json saves a write+atomic-
// rename per token event. The dedupe is best-effort -- duplicate
// hints are harmless (the runtime sees a single line per send),
// just slightly annoying in the logs.
//
// Effective limit math: `min(profile.MaxInputTokens, 128_000)`.
// The 128 K ceiling reflects the "lost-in-the-middle" attention
// degradation finding -- past that, even on 200 K-context models
// the assistant gets less reliable answers, so we trigger
// compaction earlier rather than letting the user pay for
// degraded outputs. If no profile is loaded for the agent's
// model (rare; happens with bring-your-own-model not in the
// registry), fall back to 32 K and WARN once per agent process.

package daemon

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

// Tier thresholds as fractions. Floats here (not ints) because the
// capacity calculation is fractional; truncate-to-int only at the
// boundary so a true 74.9% doesn't ALSO trigger 75%.
const (
	contextTierHint       = 0.75 // PTY directive
	contextTierSafetyNet  = 0.95 // synthetic inject before user msg
	contextCeilingTokens  = int64(128_000)
	contextFallbackTokens = int64(32_000)

	// contextHintSuppressionWindow is the in-memory dedupe window
	// for the 75 % PTY hint. 5 minutes is long enough to cover
	// "the assistant got the hint, called compact_conversation,
	// usage dropped, climbed back to 75 %" without spamming the
	// user; short enough that a stuck assistant still gets
	// re-nudged within human-noticeable time.
	contextHintSuppressionWindow = 5 * time.Minute

	// safetyNetEnvVar is read at daemon startup. Empty / "1" /
	// "true" mean enabled (default ON). "0" / "false" mean
	// disabled. Anything else logs a WARN and defaults to ON
	// (fail-safe: a typo in the env var shouldn't silently leave
	// the user vulnerable to the crash loop).
	safetyNetEnvVar = "OAT_CONTEXT_SAFETY_NET"
)

// contextCapacityState holds in-memory dedupe state for the 75%
// hint. Keyed by "repoName/agentName" string composite — same
// shape the existing nudge / message routing maps use elsewhere
// in the daemon. Protected by its own mutex (NOT the daemon's
// state mutex) so token events can update it without contending
// with state.json IO.
type contextCapacityState struct {
	mu          sync.Mutex
	lastHintAt  map[string]time.Time
	fallbackLog map[string]bool // dedupe the "no profile, falling back" WARN per-agent
	// lastTier (Part 5e Slice B) is the most-recent tier the daemon
	// observed for this agent. publishCapacityFrameIfTierChanged
	// reads + writes it under the same mutex above so the
	// `stream_context_capacity` wire only emits frames on actual
	// transitions. Values: "ok" | "hint" | "amber" | "banner" |
	// "safety_net" (the same enum as contextCapacityFrame.Tier).
	lastTier map[string]string
}

func newContextCapacityState() *contextCapacityState {
	return &contextCapacityState{
		lastHintAt:  make(map[string]time.Time),
		fallbackLog: make(map[string]bool),
		lastTier:    make(map[string]string),
	}
}

// agentKey is the composite-key string used inside
// contextCapacityState. Centralized so the lookup in the tracker
// can never drift from the write.
func agentKey(repoName, agentName string) string {
	return repoName + "/" + agentName
}

// effectiveContextLimit returns the token budget the safety net
// computes its percentage against. Logic:
//
//  1. If a profile exists for modelID and reports a non-zero
//     MaxInputTokens, use min(MaxInputTokens, contextCeilingTokens).
//  2. Otherwise fall back to contextFallbackTokens and WARN once
//     per agent process (the WARN dedupe lives in
//     contextCapacityState.fallbackLog).
//
// `source` is one of: "profile", "ceiling", "fallback". Used by
// tests + by the WARN message so an operator can tell at a glance
// whether they're seeing the ceiling kicked in or a missing-
// profile fallback.
func (d *Daemon) effectiveContextLimit(modelID, repoName, agentName string) (limit int64, source string) {
	if d.modelProfiles != nil && modelID != "" {
		if p := d.modelProfiles.Get(modelID); p != nil && p.MaxInputTokens > 0 {
			if p.MaxInputTokens < contextCeilingTokens {
				return p.MaxInputTokens, "profile"
			}
			return contextCeilingTokens, "ceiling"
		}
	}
	// Fallback path. Log once per agent process so the operator
	// can correlate the safety-net behaviour with a missing
	// profile (typo? new model not yet onboarded?).
	if d.contextCap != nil {
		key := agentKey(repoName, agentName)
		d.contextCap.mu.Lock()
		warned := d.contextCap.fallbackLog[key]
		if !warned {
			d.contextCap.fallbackLog[key] = true
		}
		d.contextCap.mu.Unlock()
		if !warned {
			d.logger.Warn(
				"context capacity: no model profile for %s/%s (model=%q); using %dK fallback",
				repoName, agentName, modelID, contextFallbackTokens/1000,
			)
		}
	}
	return contextFallbackTokens, "fallback"
}

// computeCapacityPct returns total / limit as a float in [0, 1].
// Returns 0 for non-positive limits (treats them as "unknown" →
// no tier ever triggers, which is the right safe default).
// Returns 1.0 when total >= limit (no overshoot reporting -- the
// tier triggers are >= boundaries).
func computeCapacityPct(total, limit int64) float64 {
	if limit <= 0 {
		return 0
	}
	if total >= limit {
		return 1
	}
	if total < 0 {
		return 0
	}
	return float64(total) / float64(limit)
}

// safetyNetEnabled reads OAT_CONTEXT_SAFETY_NET. Default ON.
// Centralized so tests can stub the env var via os.Setenv +
// the function gets re-evaluated per call (no caching), which
// keeps the per-call cost low and avoids a global-state surprise
// when a test sets it.
func safetyNetEnabled() bool {
	raw := strings.TrimSpace(os.Getenv(safetyNetEnvVar))
	switch strings.ToLower(raw) {
	case "", "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		// Unknown value: fail-safe to ON. The caller (which has
		// d.logger) will WARN if it cares; here we just return.
		return true
	}
}

// maybeNudgeContextCapacity is called from handleTokenUsageEvent
// after the token counters have been persisted. Only acts on
// AgentTypeAssistant (the workflow-helper AgentTypeBrowser has
// compact_conversation denied in its tool list, so the hint would
// be useless; and the other agent types don't have side-panel
// chat).
//
// Action: at >= 75 % capacity AND not-recently-hinted, emit a
// silent PTY directive instructing the assistant to call
// compact_conversation now. Recorded in the in-memory dedupe map
// so the next 5 minutes of token events don't re-fire.
//
// The 95 % safety-net inject does NOT happen here -- it fires on
// the next user message via handleAgentInput. Splitting the two
// keeps the token-event loop fast (no extra socket round-trip per
// event) and keeps the safety-net atomically aligned with the
// user message it's protecting.
func (d *Daemon) maybeNudgeContextCapacity(repoName, agentName string, agent state.Agent) {
	if agent.Type != state.AgentTypeAssistant {
		return
	}
	if d.contextCap == nil {
		return
	}
	limit, _ := d.effectiveContextLimit(agent.Model, repoName, agentName)
	pct := computeCapacityPct(agent.TotalTokens, limit)

	// Part 5e Slice B: emit a tier-crossing frame on the
	// stream_context_capacity wire BEFORE the early-return below.
	// Crossings BELOW 75% (e.g. "hint" → "ok" after a successful
	// compact_conversation) are an important signal too -- they tell
	// the side panel to hide the amber pill / banner -- so we must
	// not gate this on pct >= contextTierHint the way the PTY hint
	// path does. The broadcaster's per-agent dedupe (lastTier)
	// ensures the wire only fires when the tier actually changes,
	// regardless of how often this function is called.
	if repo, ok := d.state.GetRepo(repoName); ok {
		d.publishCapacityFrameIfTierChanged(repoName, agentName, repo.SessionName, pct, agent.TotalTokens, limit)
	}

	if pct < contextTierHint {
		return
	}

	key := agentKey(repoName, agentName)
	now := time.Now()
	d.contextCap.mu.Lock()
	last := d.contextCap.lastHintAt[key]
	if !last.IsZero() && now.Sub(last) < contextHintSuppressionWindow {
		d.contextCap.mu.Unlock()
		return
	}
	d.contextCap.lastHintAt[key] = now
	d.contextCap.mu.Unlock()

	repo, ok := d.state.GetRepo(repoName)
	if !ok {
		return
	}
	directive := fmt.Sprintf(
		"[OAT-system] You are at %.0f%% of your effective context window (%d / %d tokens). Call compact_conversation now to free working memory before your next reply.",
		pct*100, agent.TotalTokens, limit,
	)
	if err := d.backend.SendMessage(d.ctx, repo.SessionName, agent.WindowName, directive); err != nil {
		d.logger.Warn(
			"context capacity hint failed for %s/%s at %.0f%%: %v",
			repoName, agentName, pct*100, err,
		)
		return
	}
	d.logger.Info(
		"context capacity hint sent to %s/%s: %.0f%% (%d / %d tokens)",
		repoName, agentName, pct*100, agent.TotalTokens, limit,
	)
}

// shouldInjectContextSafetyNet returns true iff the daemon should
// prepend a synthetic compact-conversation directive ahead of the
// user's next message. Called from handleAgentInput. Cheap (no
// IO; just reads in-memory state + env). The actual inject is the
// caller's responsibility -- this fn just makes the decision, so
// the agent_input handler stays in control of ordering vs.
// sanitization + sentinel-prefixing.
func (d *Daemon) shouldInjectContextSafetyNet(agent state.Agent, repoName, agentName string) (directive string, inject bool) {
	if agent.Type != state.AgentTypeAssistant {
		return "", false
	}
	if !safetyNetEnabled() {
		return "", false
	}
	limit, _ := d.effectiveContextLimit(agent.Model, repoName, agentName)
	pct := computeCapacityPct(agent.TotalTokens, limit)
	if pct < contextTierSafetyNet {
		return "", false
	}
	// Wording deliberately matches the `oat assistant compact`
	// directive in internal/cli/assistant.go so an assistant
	// receiving either path sees the same instruction shape and
	// can ignore-as-duplicate.
	return fmt.Sprintf(
		"[OAT-system] You are at %.0f%% of effective context capacity (%d / %d). Call compact_conversation now before responding to anything else.",
		pct*100, agent.TotalTokens, limit,
	), true
}
