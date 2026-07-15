package daemon

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

// Layer 2 of the self-healing turn ladder. When an assistant turn ends
// with the LAST tool call in
// error and NO visible reply to the user (the "agent went silent after a
// tool error" symptom John reported), the daemon injects ONE bounded
// `[OAT-system]` recovery re-prompt so the model re-plans and continues
// on its own — the user never has to prod it.
//
// Design constraints (all deliberate):
//
//   - ASSISTANT ONLY. The controller is wired only for
//     AgentTypeAssistant (the side-panel chat agent). Browser agents
//     receive work via inter-agent messaging + a supervisor-driven
//     flow; auto-reprompting them could fight the supervisor.
//   - CURATED CODE ALLOWLIST, FAIL-CLOSED. We re-prompt only on a small
//     set of genuinely model-fixable bridge error codes (stale element
//     refs, wrong/closed tab, bad args, transient screenshot failure).
//     ANY code not on the allowlist — including security/policy blocks,
//     user-recoverable errors like EXTENSION_NOT_CONNECTED, and generic
//     EXCEPTION/TOOL_EXECUTION_ERROR whose messages may carry
//     page-derived bytes — is surfaced to the user, never auto-recovered.
//   - CODE-ONLY RE-PROMPT (prompt-injection hardening). The re-prompt is
//     built from a `code -> generic-instruction` map. We NEVER echo the
//     raw tool error message, so no page-derived / attacker-controlled
//     bytes reach the tool-capable planning model. The code itself is a
//     controlled enum from the bridge.
//   - BUDGET = 1 PER STUCK SEQUENCE (OAT_ASSISTANT_RECOVERY_MAX,
//     default 1). Tracked per-(repo, agent). Reset on a fresh user
//     message (resetForUser, from armSidePanelAutoEmit) or when a turn
//     produces a visible reply. A recovery turn that errors again — esp.
//     with the same code — is NOT re-prompted; the panel surfaces it.
//     This guards against the "loop burning tokens" failure mode.
//
// The daemon's PTY input sanitizer remains the authoritative trust
// boundary and action-gating still pauses consequential actions during
// the recovery turn; this controller adds no new tool surface.

// recoveryMaxEnv overrides the per-stuck-sequence recovery budget.
// Non-negative integer; invalid/empty falls back to the default. 0
// disables auto-recovery entirely (every silent errored turn surfaces).
const recoveryMaxEnv = "OAT_ASSISTANT_RECOVERY_MAX"

// recoveryDefaultMax is the shipped budget: exactly one auto-recovery
// re-prompt per stuck sequence.
const recoveryDefaultMax = 1

// recoverableErrorCodes is the curated, fail-closed allowlist of bridge
// error codes that are genuinely model-fixable by re-planning and are
// NOT page-derived, security, policy, or user-recoverable. Anything not
// here is surfaced to the user. See the bridge's error-code catalog
// (bridge/src/{index,mcp/server}.ts, extension/src/messaging.ts).
var recoverableErrorCodes = map[string]bool{
	// Tab addressing — model can re-observe tabs / (re)attach.
	// DEBUGGER_ATTACH_FAILED is the weak-model tabId-hallucination
	// symptom (Qwen `tabId:1`): re-checking tabs + attaching the right
	// one is exactly the model-fixable recovery.
	"TAB_CLOSED":             true,
	"TAB_NOT_ATTACHED":       true,
	"NO_ACTIVE_TAB":          true,
	"CROSS_TAB_BLOCKED":      true,
	"MAX_TABS_EXCEEDED":      true,
	"DEBUGGER_ATTACH_FAILED": true,
	// Stale / missing element references — model can re-snapshot.
	"STALE_REF":         true,
	"REF_STALE":         true,
	"ELEMENT_NOT_FOUND": true,
	"NODE_NOT_FOUND":    true,
	// Element-interaction failures — model can re-snapshot the page and
	// retry against a fresh reference (the target moved / re-rendered).
	"CLICK_FAILED":     true,
	"TYPE_FAILED":      true,
	"FILL_FAILED":      true,
	"SELECT_FAILED":    true,
	"CHECK_FAILED":     true,
	"HOVER_FAILED":     true,
	"DRAG_FAILED":      true,
	"SCROLL_FAILED":    true,
	"SCROLL_TO_FAILED": true,
	"KEY_PRESS_FAILED": true,
	// Navigation / wait — model can re-check the URL/tab or wait on a
	// different condition and continue.
	"NAVIGATION_FAILED": true,
	"WAIT_TIMEOUT":      true,
	// Bad tool arguments — model can correct and retry.
	"UNKNOWN_ARG":    true,
	"INVALID_PARAMS": true,
	"INVALID_RANGE":  true,
	// Transient capture failures — model can wait + retry.
	"SCREENSHOT_EMPTY":  true,
	"SCREENSHOT_FAILED": true,
}

// recoveryMax reads the budget from the env override, clamped to a
// non-negative integer. Invalid values fall back to the default so a
// typo doesn't silently disable the guard.
func recoveryMax() int {
	raw := strings.TrimSpace(os.Getenv(recoveryMaxEnv))
	if raw == "" {
		return recoveryDefaultMax
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return recoveryDefaultMax
	}
	return n
}

// recoveryInstructionForCode maps an error code to a GENERIC re-plan
// instruction. Code-only by construction: no error-message text is ever
// interpolated. Codes are grouped by remediation shape.
func recoveryInstructionForCode(code string) string {
	switch code {
	case "TAB_CLOSED", "TAB_NOT_ATTACHED", "NO_ACTIVE_TAB", "CROSS_TAB_BLOCKED", "MAX_TABS_EXCEEDED", "DEBUGGER_ATTACH_FAILED":
		return "Re-check which browser tabs are open and attach to the correct one before retrying."
	case "STALE_REF", "REF_STALE", "ELEMENT_NOT_FOUND", "NODE_NOT_FOUND",
		"CLICK_FAILED", "TYPE_FAILED", "FILL_FAILED", "SELECT_FAILED", "CHECK_FAILED",
		"HOVER_FAILED", "DRAG_FAILED", "SCROLL_FAILED", "SCROLL_TO_FAILED", "KEY_PRESS_FAILED":
		return "Take a fresh snapshot of the page and locate the element again — the reference you used is no longer valid."
	case "NAVIGATION_FAILED":
		return "Re-check the URL and the target tab, then try the navigation again."
	case "WAIT_TIMEOUT":
		return "The thing you waited for did not appear in time. Re-check the page state and either wait for a different condition or continue with what is available."
	case "UNKNOWN_ARG", "INVALID_PARAMS", "INVALID_RANGE":
		return "Re-check the arguments you passed to the tool and correct them."
	case "SCREENSHOT_EMPTY", "SCREENSHOT_FAILED":
		return "Wait briefly for the page to finish rendering, then try the capture again."
	default:
		return "Re-observe the current state of the page and try a different approach."
	}
}

// buildRecoveryReprompt returns the `[OAT-system]` re-prompt text for a
// recoverable code. CODE-ONLY — never echoes the raw tool error message.
// The `[OAT-system]` prefix is the trust signal the assistant is already
// conditioned to honor (capacity hints, wake-up markers, panic notices).
func buildRecoveryReprompt(code string) string {
	return fmt.Sprintf(
		"[OAT-system] Your last browser action failed with code %s and you stopped without telling the user. %s Do NOT repeat the exact same call. If you still cannot proceed after re-checking, tell the user plainly what went wrong and how they can fix it.",
		code,
		recoveryInstructionForCode(code),
	)
}

// incompleteSilentCode is the synthetic recovery "code" used for the
// same-code loop guard when injecting a silent-after-tools nudge.
const incompleteSilentCode = "INCOMPLETE_SILENT"

// todosHaveUnfinished reports whether any checklist item is not completed.
func todosHaveUnfinished(items []TodoItem) bool {
	for _, it := range items {
		st := strings.ToLower(strings.TrimSpace(it.Status))
		if st != "completed" && st != "cancelled" {
			return true
		}
	}
	return false
}

// buildIncompleteSilentReprompt is the code-only nudge for a turn that
// ended after tools with no chat bubble (hybrid 2B).
func buildIncompleteSilentReprompt(hasUnfinishedTodos bool) string {
	if hasUnfinishedTodos {
		return "[OAT-system] You ended your turn after tool calls with no message to the user, " +
			"and your plan still has unfinished items. Either finish the next open plan item now, " +
			"or tell the user in 1-2 lines what blocked you. Do not restart a giant browse loop; " +
			"do not apologize-only."
	}
	return "[OAT-system] You ended your turn after tool calls with no message to the user. " +
		"Tell the user in 1-2 lines what you just did and what blocked you or what you will do next, " +
		"OR continue with one concrete next step. Do not apologize-only; do not go silent again."
}

// assistantRecoveryController holds per-(repo, agent) recovery budget
// and interrupt latches.
type assistantRecoveryController struct {
	mu          sync.Mutex
	budget      map[string]*recoveryBudget
	interrupted map[string]bool
}

type recoveryBudget struct {
	// attempts is how many recovery re-prompts have been injected since
	// the last reset (new user message / visible reply).
	attempts int
	// lastCode is the error code of the most recent injected recovery,
	// for the same-code loop guard.
	lastCode string
}

func newAssistantRecoveryController() *assistantRecoveryController {
	return &assistantRecoveryController{
		budget:      make(map[string]*recoveryBudget),
		interrupted: make(map[string]bool),
	}
}

// markInterrupted records that the current side-panel turn was stopped
// by the user (Ctrl-C). Cleared when consumed at turn_end or on reset.
func (c *assistantRecoveryController) markInterrupted(sessionName, agent string) {
	if c == nil {
		return
	}
	key := turnKey(sessionName, agent)
	c.mu.Lock()
	c.interrupted[key] = true
	c.mu.Unlock()
}

// consumeInterrupted returns true once if an interrupt was marked since
// the last consume/reset.
func (c *assistantRecoveryController) consumeInterrupted(sessionName, agent string) bool {
	if c == nil {
		return false
	}
	key := turnKey(sessionName, agent)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.interrupted[key] {
		return false
	}
	delete(c.interrupted, key)
	return true
}

// resetForUser clears the recovery budget for (session, agent). Called
// when a fresh USER message is delivered to the agent
// (armSidePanelAutoEmit) or when a turn produced a visible reply —
// either ends the current "stuck sequence", so the next genuine error
// gets a fresh budget. Keyed by (session, agent) to match the tailer map
// and armSidePanelAutoEmit. Also clears any pending interrupt latch.
func (c *assistantRecoveryController) resetForUser(sessionName, agent string) {
	if c == nil {
		return
	}
	key := turnKey(sessionName, agent)
	c.mu.Lock()
	delete(c.budget, key)
	delete(c.interrupted, key)
	c.mu.Unlock()
}

// tryConsume atomically checks + consumes one unit of recovery budget
// for (session, agent) against the given code. Returns true if a
// recovery re-prompt is authorized (caller should inject). Enforces the
// budget cap and the same-code loop guard.
func (c *assistantRecoveryController) tryConsume(sessionName, agent, code string, max int) bool {
	key := turnKey(sessionName, agent)
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.budget[key]
	if b == nil {
		b = &recoveryBudget{}
		c.budget[key] = b
	}
	if b.attempts >= max {
		return false
	}
	// Same-code guard: never re-prompt twice for the identical failure
	// (defends against a loop if max is ever raised above 1).
	if b.lastCode == code && b.attempts >= 1 {
		return false
	}
	b.attempts++
	b.lastCode = code
	return true
}

// maybeRecoverAssistantTurn is the tailer's onTurnEnd callback body for
// assistant agents. Returns whether a recovery re-prompt was injected
// (the tailer stamps this onto the turn_end frame's `recovering` field,
// so the panel shows a quiet "trying another way" note instead of a
// prominent error). Assistant-scoped, fail-closed, code-only.
//
// The recovery budget is keyed by (sessionName, agentName) to match the
// tailer map + armSidePanelAutoEmit; state lookups + message delivery
// use repoName.
func (d *Daemon) maybeRecoverAssistantTurn(repoName, sessionName, agentName string, info turnEndInfo) bool {
	// No live process to re-prompt under test mode.
	if os.Getenv("OAT_TEST_MODE") == "1" {
		return false
	}
	// User Stop: do not inject recovery after an interrupted turn.
	if d.assistantRecovery.consumeInterrupted(sessionName, agentName) {
		d.logger.Info(
			"assistant recovery skipped for %s/%s — turn was interrupted by user",
			repoName, agentName,
		)
		return false
	}
	// A turn that spoke to the user ends the stuck sequence — reset the
	// budget so a later, unrelated error gets a fresh allowance.
	if info.VisibleReply {
		d.assistantRecovery.resetForUser(sessionName, agentName)
		return false
	}

	max := recoveryMax()
	if max <= 0 {
		return false
	}

	repo, repoOK := d.state.GetRepo(repoName)
	agent, agentOK := d.state.GetAgent(repoName, agentName)
	if !repoOK || !agentOK {
		return false
	}
	if agent.Type != state.AgentTypeAssistant {
		return false
	}

	var msg string
	var consumeCode string

	if info.HadError {
		code := strings.TrimSpace(info.Code)
		if code == "" || !recoverableErrorCodes[code] {
			return false
		}
		consumeCode = code
		msg = buildRecoveryReprompt(code)
	} else if info.HadTools {
		// Hybrid 2B: silent after successful tools — nudge once.
		consumeCode = incompleteSilentCode
		msg = buildIncompleteSilentReprompt(info.HasUnfinishedTodos)
	} else {
		return false
	}

	if !d.assistantRecovery.tryConsume(sessionName, agentName, consumeCode, max) {
		d.logger.Info(
			"assistant recovery exhausted/guarded for %s/%s (code=%s) — surfacing to user",
			repoName, agentName, consumeCode,
		)
		return false
	}

	if err := d.backend.SendMessage(d.ctx, repo.SessionName, agent.WindowName, msg); err != nil {
		d.logger.Warn(
			"assistant recovery re-prompt send failed for %s/%s (code=%s): %v",
			repoName, agentName, consumeCode, err,
		)
		return false
	}
	d.logger.Info(
		"assistant_recovery_injected: repo=%s agent=%s code=%s",
		repoName, agentName, consumeCode,
	)
	return true
}
