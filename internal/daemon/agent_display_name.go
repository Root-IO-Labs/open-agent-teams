package daemon

import (
	"github.com/Root-IO-Labs/open-agent-teams/internal/socket"
	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

// handleSetAgentDisplayName sets a cosmetic Manage-tab alias on an
// Assistant/Browser agent. Empty clears. Paths/slugs are unchanged.
func (d *Daemon) handleSetAgentDisplayName(req socket.Request) socket.Response {
	repoName, errResp, ok := getRequiredStringArg(req.Args, "repo", "repository name is required")
	if !ok {
		return errResp
	}
	agentName, errResp, ok := getRequiredStringArg(req.Args, "agent", "agent name is required")
	if !ok {
		return errResp
	}
	// display_name may be empty (clear). Accept missing as empty.
	raw := getOptionalStringArg(req.Args, "display_name", "")

	agent, exists := d.state.GetAgent(repoName, agentName)
	if !exists {
		return socket.ErrorResponse("agent %q not found in repository %q", agentName, repoName)
	}
	if !agent.Type.IsPausable() {
		return socket.ErrorResponse(
			"display name is only supported for assistants and browser-agents (got type %q)",
			agent.Type,
		)
	}

	stored, vErr := validateDisplayNameInput(raw, agentName)
	if vErr != nil {
		return socket.ErrorResponse("%s", vErr.Error())
	}
	if stored != "" {
		norm := normalizeDisplayNameForCompare(stored)
		if conflictRepo, conflictAgent, hit := displayNameCollision(d.state, repoName, agentName, norm); hit {
			return socket.ErrorResponse(
				"display name %q conflicts with agent %q in repo %q (names and aliases must be unique across assistants and browser-agents)",
				stored, conflictAgent, conflictRepo,
			)
		}
	}

	prior := agent.DisplayName
	if err := d.state.ModifyAgent(repoName, agentName, func(a *state.Agent) {
		a.DisplayName = stored
	}); err != nil {
		return socket.ErrorResponse("failed to update display name: %s", err.Error())
	}

	updated, _ := d.state.GetAgent(repoName, agentName)
	d.publishAgentLifecycle(lifecycleKindAgentUpdated, repoName, agentName, updated)
	d.logger.Info("Set agent %s/%s display_name: %q -> %q", repoName, agentName, prior, stored)

	return socket.SuccessResponse(map[string]interface{}{
		"repo":          repoName,
		"agent":         agentName,
		"prior":         prior,
		"display_name":  stored,
		"display_title": firstNonEmpty(stored, agentName),
	})
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
