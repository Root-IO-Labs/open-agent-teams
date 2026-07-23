package daemon

import (
	"os"
	"sync"

	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

// Plan-stale nudge: when the assistant makes real progress (screenshot /
// write / navigate / …) but does not call write_todos to update an open
// Plan card, inject one bounded [OAT-system] reminder. Separate from
// OAT_ASSISTANT_RECOVERY_MAX — inventing checkbox states in the daemon is
// forbidden; only the model may update the plan via write_todos.

// planProgressTools is the curated allowlist of tools that count as
// "progress" toward open plan items. Keep this small and code-owned.
var planProgressTools = map[string]bool{
	"browser_save_screenshot": true,
	"write_file":              true,
	"edit_file":               true,
	"browser_navigate":        true,
	"browser_click":           true,
}

func isPlanProgressTool(name string) bool {
	return planProgressTools[name]
}

func buildPlanStaleReprompt() string {
	return "[OAT-system] You made progress on the task but did not update your plan. " +
		"Call write_todos now and mark completed/in_progress items to match what you just did, " +
		"then continue. Do not invent work you have not done; do not restart from scratch."
}

// planStaleController caps the plan-update nudge once per stuck sequence
// until a fresh EventTodos arrives or the user starts a new sequence.
type planStaleController struct {
	mu     sync.Mutex
	nudged map[string]bool
}

func newPlanStaleController() *planStaleController {
	return &planStaleController{nudged: make(map[string]bool)}
}

func (c *planStaleController) resetForUser(sessionName, agent string) {
	if c == nil {
		return
	}
	key := turnKey(sessionName, agent)
	c.mu.Lock()
	delete(c.nudged, key)
	c.mu.Unlock()
}

// noteTodosUpdated clears the nudge latch after a fresh write_todos so a
// later progress gap can nudge again.
func (c *planStaleController) noteTodosUpdated(sessionName, agent string) {
	c.resetForUser(sessionName, agent)
}

// tryNudge authorizes one plan-stale inject for (session, agent). Returns
// false if already nudged this sequence.
func (c *planStaleController) tryNudge(sessionName, agent string) bool {
	if c == nil {
		return false
	}
	key := turnKey(sessionName, agent)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.nudged[key] {
		return false
	}
	c.nudged[key] = true
	return true
}

// maybePlanStaleNudge injects a one-shot plan-update reminder when the
// turn had progress tools, unfinished todos (from this or a prior turn),
// and no EventTodos this turn. Runs on silent or chatty turn_end. Does
// not consume OAT_ASSISTANT_RECOVERY_MAX. Returns true when a nudge was
// successfully injected (caller may skip incomplete-silent recovery for
// the same turn to avoid double [OAT-system] spam).
func (d *Daemon) maybePlanStaleNudge(repoName, sessionName, agentName string, info turnEndInfo) bool {
	if os.Getenv("OAT_TEST_MODE") == "1" {
		return false
	}
	if d.planStale == nil || d.assistantRecovery == nil {
		return false
	}
	if d.assistantRecovery.isInterrupted(sessionName, agentName) {
		return false
	}
	if info.SawTodosThisTurn {
		d.planStale.noteTodosUpdated(sessionName, agentName)
		return false
	}
	if !info.HasUnfinishedTodos || !info.ProgressToolsOK {
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

	if !d.planStale.tryNudge(sessionName, agentName) {
		return false
	}

	msg := buildPlanStaleReprompt()
	if err := d.backend.SendMessage(d.ctx, repo.SessionName, agent.WindowName, msg); err != nil {
		d.logger.Warn(
			"assistant plan-stale nudge send failed for %s/%s: %v",
			repoName, agentName, err,
		)
		// Refund so a later turn can retry the nudge.
		d.planStale.resetForUser(sessionName, agentName)
		return false
	}
	d.logger.Info(
		"assistant_plan_stale_nudge: repo=%s agent=%s",
		repoName, agentName,
	)
	return true
}
