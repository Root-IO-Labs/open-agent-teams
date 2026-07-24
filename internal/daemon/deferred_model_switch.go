package daemon

import (
	"fmt"

	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

// ensureDesiredModelBeforeSend session-preserving-restarts an
// Assistant/Browser agent when the operator's configured Model differs
// from the running ResolvedModel. Called from side-panel chat delivery
// paths so a model picker change applies on the next Send (flip-flop
// without Send never restarts).
//
// Returns the (possibly refreshed) agent record to use for delivery.
// On failure, returns the original agent and an error — callers must
// not silently drop the user message.
func (d *Daemon) ensureDesiredModelBeforeSend(repoName, agentName string, agent state.Agent, repo *state.Repository) (state.Agent, error) {
	if !usesBrowserBridge(agent.Type) {
		return agent, nil
	}
	if !modelNeedsDeferredRestart(agent) {
		return agent, nil
	}

	mu := d.agentLifecycleMutex(repoName, agentName)
	mu.Lock()
	defer mu.Unlock()

	// Re-read under lock — preference may have flipped back (Sonnet→GPT→Sonnet).
	fresh, ok := d.state.GetAgent(repoName, agentName)
	if !ok {
		return agent, fmt.Errorf("agent %q not found in repository %q", agentName, repoName)
	}
	if !modelNeedsDeferredRestart(fresh) {
		return fresh, nil
	}

	d.logger.Info(
		"deferred model switch for %s/%s: running=%q configured=%q — session-preserving restart before send",
		repoName, agentName, fresh.ResolvedModel, fresh.Model,
	)

	if fresh.PID > 0 {
		window := fresh.WindowName
		if window == "" {
			window = agentName
		}
		if err := d.backend.StopAgent(d.ctx, repo.SessionName, window); err != nil {
			d.logger.Warn("deferred model switch: stop prior %s/%s: %v", repoName, agentName, err)
		}
	}

	if err := d.restartAgent(repoName, agentName, fresh, repo); err != nil {
		return fresh, fmt.Errorf("restart for model switch failed: %w", err)
	}

	if usesBrowserBridge(fresh.Type) {
		d.clearBridgeUnreachable(fmt.Sprintf("%s/%s", repoName, agentName))
	}

	updated, ok := d.state.GetAgent(repoName, agentName)
	if !ok {
		return fresh, fmt.Errorf("agent %q missing after model-switch restart", agentName)
	}
	d.publishAgentLifecycle(lifecycleKindAgentStarted, repoName, agentName, updated)
	return updated, nil
}
