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
	"TAB_CLOSED":        true,
	"TAB_NOT_ATTACHED":  true,
	"NO_ACTIVE_TAB":     true,
	"CROSS_TAB_BLOCKED": true,
	"MAX_TABS_EXCEEDED": true,
	// Stale / missing element references — model can re-snapshot.
	"STALE_REF":         true,
	"REF_STALE":         true,
	"ELEMENT_NOT_FOUND": true,
	"NODE_NOT_FOUND":    true,
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
	case "TAB_CLOSED", "TAB_NOT_ATTACHED", "NO_ACTIVE_TAB", "CROSS_TAB_BLOCKED", "MAX_TABS_EXCEEDED":
		return "Re-check which browser tabs are open and attach to the correct one before retrying."
	case "STALE_REF", "REF_STALE", "ELEMENT_NOT_FOUND", "NODE_NOT_FOUND":
		return "Take a fresh snapshot of the page and locate the element again — the reference you used is no longer valid."
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

// assistantRecoveryController holds per-(repo, agent) recovery budget.
type assistantRecoveryController struct {
	mu     sync.Mutex
	budget map[string]*recoveryBudget
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
	return &assistantRecoveryController{budget: make(map[string]*recoveryBudget)}
}

// resetForUser clears the recovery budget for (session, agent). Called
// when a fresh USER message is delivered to the agent
// (armSidePanelAutoEmit) or when a turn produced a visible reply —
// either ends the current "stuck sequence", so the next genuine error
// gets a fresh budget. Keyed by (session, agent) to match the tailer map
// and armSidePanelAutoEmit.
func (c *assistantRecoveryController) resetForUser(sessionName, agent string) {
	if c == nil {
		return
	}
	key := turnKey(sessionName, agent)
	c.mu.Lock()
	delete(c.budget, key)
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
	// A turn that spoke to the user ends the stuck sequence — reset the
	// budget so a later, unrelated error gets a fresh allowance.
	if info.VisibleReply {
		d.assistantRecovery.resetForUser(sessionName, agentName)
		return false
	}
	if !info.HadError {
		return false
	}
	code := strings.TrimSpace(info.Code)
	// Fail-closed: only recover on the curated allowlist. Everything
	// else (incl. empty code, security/policy/user-recoverable codes)
	// is surfaced to the user by Layer 3.
	if code == "" || !recoverableErrorCodes[code] {
		return false
	}
	max := recoveryMax()
	if max <= 0 {
		return false
	}
	if !d.assistantRecovery.tryConsume(sessionName, agentName, code, max) {
		d.logger.Info(
			"assistant recovery exhausted/guarded for %s/%s (code=%s) — surfacing to user",
			repoName, agentName, code,
		)
		return false
	}

	repo, repoOK := d.state.GetRepo(repoName)
	agent, agentOK := d.state.GetAgent(repoName, agentName)
	if !repoOK || !agentOK {
		return false
	}
	// Belt-and-suspenders: the callback is only wired for assistants,
	// but re-verify so a future call-site change can't widen the scope.
	if agent.Type != state.AgentTypeAssistant {
		return false
	}

	msg := buildRecoveryReprompt(code)
	if err := d.backend.SendMessage(d.ctx, repo.SessionName, agent.WindowName, msg); err != nil {
		d.logger.Warn(
			"assistant recovery re-prompt send failed for %s/%s (code=%s): %v",
			repoName, agentName, code, err,
		)
		return false
	}
	d.logger.Info(
		"assistant_recovery_injected: repo=%s agent=%s code=%s",
		repoName, agentName, code,
	)
	return true
}
