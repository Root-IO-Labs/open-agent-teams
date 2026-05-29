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
// Effective limit math (precedence order, highest first):
//
//  1. `OAT_MODEL_CONTEXT_<normalized-modelID>` env var (operator
//     override; takes precedence over everything else; clamped to
//     `[1024, 16_000_000]` tokens).
//  2. `min(profile.MaxInputTokens, 128_000)` if a `ModelProfile`
//     exists for the agent's model and its `MaxInputTokens > 0`.
//  3. 128 K fallback otherwise, with a WARN that names the model
//     ID + the literal `oat model onboard <modelID>` recovery
//     command. Deduped to once per agent process.
//
// The 128 K ceiling reflects the "lost-in-the-middle" attention
// degradation finding -- past that, even on 200 K-context models
// the assistant gets less reliable answers, so we trigger
// compaction earlier rather than letting the user pay for
// degraded outputs. 128 K is also the modern floor for shipping
// models in 2026: every flagship from Anthropic / OpenAI / Google
// supports at least this much, so an unprofiled model gets a
// budget that's right for the common case (vs. the older 32 K
// fallback that wedged a `google_genai:gemini-2.5-flash` agent at
// "100% capacity" the instant a Wikipedia article landed in its
// history).
//
// The env-override exists as an escape hatch for true bring-your-
// own-model setups (local Ollama instances, custom routers,
// internal proxies) where the operator already knows the correct
// budget and either can't or doesn't want to run the full
// `oat model onboard` probe. It also lets CI workflows skip the
// onboard round-trip in unattended automation.

package daemon

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

// Tier thresholds as fractions. Floats here (not ints) because the
// capacity calculation is fractional; truncate-to-int only at the
// boundary so a true 74.9% doesn't ALSO trigger 75%.
const (
	contextTierHint      = 0.75 // PTY directive
	contextTierSafetyNet = 0.95 // synthetic inject before user msg
	contextCeilingTokens = int64(128_000)
	// contextFallbackTokens is the budget used when no
	// `ModelProfile` exists for an agent's model and no env
	// override is set. Bumped from 32 K to 128 K in 2026 because
	// 128 K is the floor for every shipping flagship model
	// (Anthropic / OpenAI / Google) -- the older 32 K fallback
	// turned a 1 M-context model into a 100%-effective-capacity
	// failure mode the instant a single large tool result landed in
	// its history. 128 K is the right "we don't know, assume the
	// modern floor" guess; operators with a true bring-your-own-
	// model setup can bypass this with `OAT_MODEL_CONTEXT_<id>` or
	// run `oat model onboard <id>` for an accurate ModelProfile.
	contextFallbackTokens = int64(128_000)

	// contextEnvOverridePrefix is the prefix for the per-model env
	// override. Concatenated with a normalized model ID
	// (lowercase + `:` and `/` replaced with `_`) so e.g.
	// `google_genai:gemini-2.5-flash` becomes
	// `OAT_MODEL_CONTEXT_google_genai_gemini-2.5-flash`. Read once
	// per call (no caching) so test setups using `t.Setenv` see the
	// expected value without process restart.
	contextEnvOverridePrefix = "OAT_MODEL_CONTEXT_"

	// contextEnvOverrideMin and contextEnvOverrideMax bound the env-
	// override value. The range covers everything from a small
	// quantized local model (a few thousand tokens) up to a future
	// 16 M-context flagship -- absurd values (e.g. someone exports
	// `=2000000000`) are clamped + WARNed so the agent never sees
	// the bad number.
	contextEnvOverrideMin = int64(1_024)
	contextEnvOverrideMax = int64(16_000_000)

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

// normalizeModelIDForEnv produces the env-var suffix for a model
// ID. Lowercases everything, then maps `:` and `/` (the two
// characters that show up in real-world IDs like
// `google_genai:gemini-2.5-flash` and `anthropic/claude-opus-4`)
// to underscores so the result is a valid POSIX env-var name.
// Other characters are passed through unchanged -- `-` and `.`
// are valid in env-var names on every POSIX shell that matters
// for OAT, and aggressive sanitization would create silent
// collisions (e.g. `gemini-2.5` and `gemini.2.5` both becoming
// `gemini_2_5`).
//
// Centralized so the lookup in `contextEnvOverride` can never
// drift from documentation / tests; both call this helper.
func normalizeModelIDForEnv(modelID string) string {
	lowered := strings.ToLower(modelID)
	replacer := strings.NewReplacer(":", "_", "/", "_")
	return replacer.Replace(lowered)
}

// contextEnvOverride returns the operator-set context budget for
// `modelID` via `OAT_MODEL_CONTEXT_<normalized-modelID>`, or
// `(0, false)` when the env var is unset / empty. Returned values
// are clamped to `[contextEnvOverrideMin, contextEnvOverrideMax]`
// with a startup WARN on clamp; the agent never sees the
// pre-clamp value. Returns `(0, false)` -- not the clamped value
// -- if the env var is non-numeric, so the caller can fall
// through to profile / fallback instead of trusting garbage.
//
// `logSink` is invoked with a WARN-level message on every clamp
// or parse-error path. Tests pass a capturing closure; the
// daemon passes a closure that forwards to `d.logger.Warn`. The
// closure is called synchronously so test assertions on capture
// state are race-free.
func contextEnvOverride(modelID string, logSink func(format string, args ...any)) (int64, bool) {
	if modelID == "" {
		return 0, false
	}
	envName := contextEnvOverridePrefix + normalizeModelIDForEnv(modelID)
	raw := strings.TrimSpace(os.Getenv(envName))
	if raw == "" {
		return 0, false
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		if logSink != nil {
			logSink(
				"context capacity: %s=%q is not a valid integer; ignoring (will use profile / 128K fallback). To set a runtime override, use an integer token count like %s=128000.",
				envName, raw, envName,
			)
		}
		return 0, false
	}
	if parsed < contextEnvOverrideMin {
		if logSink != nil {
			logSink(
				"context capacity: %s=%d below minimum %d; clamping. Did you mean %dK tokens (e.g. %s=128000)?",
				envName, parsed, contextEnvOverrideMin, parsed/1000, envName,
			)
		}
		return contextEnvOverrideMin, true
	}
	if parsed > contextEnvOverrideMax {
		if logSink != nil {
			logSink(
				"context capacity: %s=%d above maximum %d; clamping. The largest shipping model context window is well under %dM tokens; double-check the value.",
				envName, parsed, contextEnvOverrideMax, contextEnvOverrideMax/1_000_000,
			)
		}
		return contextEnvOverrideMax, true
	}
	return parsed, true
}

// effectiveContextLimit returns the token budget the safety net
// computes its percentage against. Precedence (highest first):
//
//  1. `OAT_MODEL_CONTEXT_<normalized-modelID>` env override.
//     Returns `source = "env"`.
//  2. ModelProfile for modelID with `MaxInputTokens > 0`. Returns
//     `source = "profile"` when used as-is, or `"ceiling"` when
//     clamped down to the 128 K attention-degradation ceiling.
//  3. Fallback to `contextFallbackTokens` (128 K). Returns
//     `source = "fallback"` and emits a once-per-agent-process
//     WARN naming the model ID + the literal
//     `oat model onboard <modelID>` recovery command so an
//     operator can copy-paste the fix.
//
// `source` is consumed by tests + by the WARN message so an
// operator can tell at a glance whether they're seeing the env
// override, the ceiling kicked in, or a missing-profile fallback.
func (d *Daemon) effectiveContextLimit(modelID, repoName, agentName string) (limit int64, source string) {
	// 1. Env override wins. Reading per call (no cache) keeps the
	// surface trivial to test via t.Setenv.
	if val, ok := contextEnvOverride(modelID, d.warnf); ok {
		return val, "env"
	}
	// 2. Profile.
	if d.modelProfiles != nil && modelID != "" {
		if p := d.modelProfiles.Get(modelID); p != nil && p.MaxInputTokens > 0 {
			if p.MaxInputTokens < contextCeilingTokens {
				return p.MaxInputTokens, "profile"
			}
			return contextCeilingTokens, "ceiling"
		}
	}
	// 3. Fallback. Log once per agent process so the operator can
	// correlate the safety-net behaviour with a missing profile.
	// The WARN includes the literal `oat model onboard <modelID>`
	// command + the `OAT_MODEL_CONTEXT_<id>` env override path so
	// the operator has both recovery paths in one place without
	// chasing docs.
	if d.contextCap != nil {
		key := agentKey(repoName, agentName)
		d.contextCap.mu.Lock()
		warned := d.contextCap.fallbackLog[key]
		if !warned {
			d.contextCap.fallbackLog[key] = true
		}
		d.contextCap.mu.Unlock()
		if !warned {
			d.warnf(
				"context capacity: no ModelProfile for model %q (agent %s/%s); using %dK fallback. "+
					"To get the correct value, run:\n\n    oat model onboard %s\n\n"+
					"Or set this env var for a runtime override (skip the probe):\n\n    %s%s=<tokens>",
				modelID, repoName, agentName, contextFallbackTokens/1000,
				modelID,
				contextEnvOverridePrefix, normalizeModelIDForEnv(modelID),
			)
		}
	}
	return contextFallbackTokens, "fallback"
}

// warnf forwards to `d.logger.Warn` and is the WARN sink that
// `contextEnvOverride` calls (the override doesn't have direct
// access to the daemon -- threading the sink through the call
// keeps the helper pure-Go and trivially testable). Nil-safe so
// tests that exercise the override helper without a fully-wired
// daemon don't have to construct a logger.
func (d *Daemon) warnf(format string, args ...any) {
	if d == nil || d.logger == nil {
		return
	}
	d.logger.Warn(format, args...)
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
