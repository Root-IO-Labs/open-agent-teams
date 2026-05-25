// Package daemon — assistant_env.go owns the Part 5f env-var
// preparation block. **Memory itself is not implemented in this
// plan** (per plan body 5f: "A separate, unrelated OAT memory/RAG
// effort is in early design with unknown timeline and unknown
// architecture."). All we do here is emit a small generic env-var
// block at AgentTypeAssistant spawn so any future memory
// middleware that wants to opt into the assistant agent has a
// clean signal to consume.
//
// Three variables, all generic, all harmless if no consumer:
//
//   - OAT_AGENT_TYPE=assistant       which type spawned this process
//   - OAT_REPO=<virtual-repo-name>   the assistant's container
//   - OAT_MEMORY_ENABLED=1           "memory subsystems may activate"
//
// If the future memory system uses a totally different design
// (different env vars / file contracts / scope model), these
// emissions become dead weight -- drop them at that point. No
// coupling assumed today.
//
// Single source of truth for the assistant spawn env block --
// three call sites in daemon.go (startRegisteredAgent,
// startAgentWithConfig, the inline restart path) all use this
// helper so adding a fourth variable later means editing one
// place instead of three.

package daemon

import (
	"fmt"

	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

// assistantSpawnEnvVars returns the additional env vars that the
// daemon prepends to AgentTypeAssistant agent processes at spawn.
// Returns nil (a no-op append) for any other agent type, which is
// the correct behavior at every call site -- non-assistant agents
// must NOT inherit OAT_AGENT_TYPE=assistant or the future memory
// middleware will key into the wrong scope.
//
// `repoName` is the virtual-repo state-key name (e.g.
// `_assistant-personal`); the consumer reads OAT_REPO to find its
// per-repo storage area without needing to re-derive anything.
//
// Why a function-not-a-method: the daemon receiver isn't needed
// (no logger, no state lookup, no IO), and keeping it
// function-shaped lets the unit tests construct the input
// directly without spinning up a full *Daemon.
func assistantSpawnEnvVars(agentType state.AgentType, repoName string) []string {
	if agentType != state.AgentTypeAssistant {
		return nil
	}
	return []string{
		fmt.Sprintf("OAT_AGENT_TYPE=%s", state.AgentTypeAssistant),
		fmt.Sprintf("OAT_REPO=%s", repoName),
		// "1" not "true" -- matches the daemon's convention for
		// other env-var gates (OAT_USE_SIDECAR=1,
		// OAT_TEST_MODE=1, OAT_CONTEXT_SAFETY_NET=1).
		"OAT_MEMORY_ENABLED=1",
	}
}
