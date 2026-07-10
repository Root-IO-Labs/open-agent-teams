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

	// contextOccupancySanityFactor bounds a single incoming context-window
	// occupancy reading (Phase 3 item 8). A flaky-reporting model (the Qwen
	// profile shows token_reporting 0.85, no per-chunk usage_metadata) can
	// emit a wildly-wrong huge reading — the real daemon log showed an
	// "incoming total 14185804" against a ~128K window — which, once stored,
	// pins the ring at 100% forever because no strictly-lower value follows.
	// We reject only readings FAR above the model's real window (this factor ×
	// the effective limit), never merely-above the conservative input budget
	// (legit near-limit sessions exceed that), so one bad [OAT_TOKENS] line
	// can't strand the meter. 4× the window is generous enough to admit any
	// honest reading (real window ~1.36× the conservative input budget) while
	// still catching a 100×-type garbage value.
	contextOccupancySanityFactor = int64(4)

	// outputReservationEnvVar gates the Phase 1 output-headroom
	// reservation (default ON). When enabled, the 75%/95% safety-net
	// tiers are computed against the model's window MINUS the reserved
	// output-token budget (resolveOutputMaxTokens), so compaction fires
	// with enough room left for the model to actually generate its reply.
	// The user-visible ring meter is NOT affected — it always measures
	// against the real window so "full means full" (Phase 3 item 7).
	// Feature-flagged per the plan's cross-cutting guidance so a
	// misbehaving reservation can be disabled without a rollback.
	outputReservationEnvVar = "OAT_CONTEXT_RESERVE_OUTPUT"
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

// outputReservationEnabled reads OAT_CONTEXT_RESERVE_OUTPUT. Default ON.
// Same tri-state parsing as safetyNetEnabled (fail-safe to ON on garbage).
func outputReservationEnabled() bool {
	raw := strings.TrimSpace(os.Getenv(outputReservationEnvVar))
	switch strings.ToLower(raw) {
	case "", "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// effectiveContextBudget returns the INPUT token budget the safety-net TIERS
// (75% hint, 95% synthetic compaction inject) are computed against: the model's
// context window (effectiveContextLimit) minus the reserved output-token
// headroom (resolveOutputMaxTokens, the same value modelParamsJSON sends to the
// runtime). Reserving output headroom means the daemon nudges the assistant to
// compact while there's still room for the model to generate its reply, instead
// of treating the whole window as available for input and only reacting once the
// request itself would overflow.
//
// This is DISTINCT from the display denominator: the user-visible ring always
// measures used/window (via effectiveContextLimit) so "100% means the model will
// actually reject the next turn" (Phase 3 item 7 — full means full). Using the
// smaller budget only for the internal tier decisions keeps the two from
// fighting (naively subtracting output from the ring would peg it early).
//
// Floored at half the window so a pathological max_tokens or a very small window
// can't collapse the budget to near-zero and make the safety net fire
// constantly. Returns effectiveContextLimit's source unchanged.
func (d *Daemon) effectiveContextBudget(modelID, repoName, agentName string) (budget int64, source string) {
	limit, source := d.effectiveContextLimit(modelID, repoName, agentName)
	if !outputReservationEnabled() || limit <= 0 {
		return limit, source
	}
	reserved := int64(d.resolveOutputMaxTokens(modelID))
	budget = limit - reserved
	if floor := limit / 2; budget < floor {
		budget = floor
	}
	if budget < 1 {
		budget = limit
	}
	return budget, source
}

// effectiveDisplayLimit returns the denominator the USER-VISIBLE ring meter
// measures occupancy against — an approximation of the model's REAL usable
// context window, so "100% means the model will actually reject the next turn"
// (Phase 3 decision 3, "full means full").
//
// Why this differs from effectiveContextLimit: an onboarded ModelProfile records
// only `max_input_tokens` (the PROMPT budget ≈ real_window − max_output), never
// the full context window. For the DGX-Spark Qwen that's max_input_tokens=96000
// while the model's real hard limit is ~131072 — so pegging the ring at 96000
// shows a permanent, broken-looking 100% while the model happily keeps accepting
// turns (exactly John's "indicators say 100% but it keeps working"). The real
// window isn't stored anywhere in OAT's data, so when the limit came from a
// profile's input budget we approximate the window as input_budget + reserved
// output headroom (≈ real_window, because max_input_tokens ≈ real_window −
// max_output). For Qwen: 96000 + 32000 = 128000 ≈ the real 131072.
//
// Only the "profile" source is adjusted. "env" (operator-set), "ceiling" (the
// 128K attention-degradation cap) and "fallback" (the 128K "assume the modern
// floor" guess) already represent the intended WINDOW, not an input budget, so
// they are returned unchanged — this also keeps them from over-stating.
//
// This is an approximation, not a probed value; a dedicated real-context-window
// profile field would let the ring be exact (noted as a follow-up). The internal
// safety-net TIERS keep using the smaller effectiveContextBudget so compaction
// still fires early — the display denominator is deliberately larger than the
// tier denominator.
func (d *Daemon) effectiveDisplayLimit(modelID, repoName, agentName string) int64 {
	limit, source := d.effectiveContextLimit(modelID, repoName, agentName)
	if source != "profile" || limit <= 0 {
		return limit
	}
	reserved := int64(d.resolveOutputMaxTokens(modelID))
	if reserved <= 0 {
		return limit
	}
	return limit + reserved
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

// agentContextOccupancy returns the token count the capacity % is
// computed against: the current context-WINDOW occupancy (latest
// main-turn prompt + response) reported by the runtime. This is the
// honest "how full is the window right now" number — distinct from
// agent.TotalTokens, which is the cumulative lifetime spend and
// over-counts wildly because every turn re-sends the whole growing
// context.
//
// Returns known=false when the window value is not yet known (== 0):
// an agent that hasn't emitted a token event this process has no
// window reading. We deliberately do NOT fall back to cumulative
// TotalTokens — that over-counts wildly (every turn re-sends the
// growing context), so a long-lived browser agent would peg the meter
// at a false 100% and could trip the assistant safety net off a
// number that has nothing to do with live window occupancy. Callers
// render a neutral/hidden meter and skip the hint/safety-net while
// occupancy is unknown; once the runtime emits context_input on turn 1
// the value becomes known and normal behavior resumes.
func agentContextOccupancy(agent state.Agent) (used int64, known bool) {
	if agent.ContextWindowTokens > 0 {
		return agent.ContextWindowTokens, true
	}
	return 0, false
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
// after the token counters have been persisted. It does two things
// with different agent-type scopes:
//
//   - Display frame: published for any chat-capable agent
//     (usesBrowserBridge: assistant + browser) so the side-panel ring
//     meter can follow whichever agent the chat picker has selected.
//   - Compact hint (below) + the 95 % safety net: assistant-only. The
//     workflow-helper AgentTypeBrowser has compact_conversation denied
//     in its tool list, so the hint would be useless; other agent
//     types don't have side-panel chat at all.
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
	// The display frame is published for any chat-capable agent
	// (assistant + browser) so the side-panel meter can follow the
	// selected agent. The compact hint + safety net below stay
	// assistant-only (browser agents have compact_conversation denied).
	if !usesBrowserBridge(agent.Type) {
		return
	}
	if d.contextCap == nil {
		return
	}
	// Three denominators (Phase 1 item 2 + Phase 3 item 7):
	//   - displayLimit: the ring meter's real-window approximation so
	//     "full means full" and a profile-input-budget model (Qwen 96K)
	//     doesn't peg at a false 100% ~35K early.
	//   - budget: the internal 75%/95% tiers measure against
	//     window-minus-output so compaction fires with room for the reply.
	//   - limit: the raw effectiveContextLimit, used only for the hint's
	//     human-readable "window" figure below.
	limit, _ := d.effectiveContextLimit(agent.Model, repoName, agentName)
	displayLimit := d.effectiveDisplayLimit(agent.Model, repoName, agentName)
	budget, _ := d.effectiveContextBudget(agent.Model, repoName, agentName)
	used, known := agentContextOccupancy(agent)
	displayPct := computeCapacityPct(used, displayLimit)
	tierPct := computeCapacityPct(used, budget)

	// Emit a capacity frame on EVERY token event (i.e. every turn) so
	// the side-panel ring meter is genuinely live, not a step function
	// that only moves on tier boundaries. The frame still carries the
	// tier name so the extension can drive the amber/red colour shift +
	// nudge copy; the per-turn cadence is low (one per reply) so the
	// broadcaster's small buffer is never stressed. When occupancy is
	// unknown the frame carries tier "unknown" (neutral/hidden meter).
	// The ring uses the real-window denominator (displayPct).
	if repo, ok := d.state.GetRepo(repoName); ok {
		d.publishCapacityFrame(repoName, agentName, repo.SessionName, displayPct, used, displayLimit, known)
	}

	// Hint + safety net are assistant-only and require a real window
	// reading; never nudge a browser agent or fire off an unknown
	// (would otherwise compact based on a number we don't have).
	if agent.Type != state.AgentTypeAssistant {
		return
	}
	if !known {
		return
	}

	if tierPct < contextTierHint {
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
		"[OAT-system] You are at %.0f%% of your effective context window (%d / %d tokens, reserving %d for output). Call compact_conversation now to free working memory before your next reply.",
		tierPct*100, used, budget, limit-budget,
	)
	if err := d.backend.SendMessage(d.ctx, repo.SessionName, agent.WindowName, directive); err != nil {
		d.logger.Warn(
			"context capacity hint failed for %s/%s at %.0f%%: %v",
			repoName, agentName, tierPct*100, err,
		)
		return
	}
	d.logger.Info(
		"context capacity hint sent to %s/%s: %.0f%% (%d / %d input budget, window %d)",
		repoName, agentName, tierPct*100, used, budget, limit,
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
	// Tier decision uses the output-reserved input budget (Phase 1 item 2),
	// not the full window, so the inject fires with room left for the reply.
	budget, _ := d.effectiveContextBudget(agent.Model, repoName, agentName)
	used, known := agentContextOccupancy(agent)
	if !known {
		// No live window reading yet — don't compact off a number we
		// don't have (previously this fell back to inflated cumulative
		// spend and could mis-fire right after a restart/wake).
		return "", false
	}
	pct := computeCapacityPct(used, budget)
	if pct < contextTierSafetyNet {
		return "", false
	}
	// Wording deliberately matches the `oat assistant compact`
	// directive in internal/cli/assistant.go so an assistant
	// receiving either path sees the same instruction shape and
	// can ignore-as-duplicate.
	return fmt.Sprintf(
		"[OAT-system] You are at %.0f%% of effective context capacity (%d / %d). Call compact_conversation now before responding to anything else.",
		pct*100, used, budget,
	), true
}
