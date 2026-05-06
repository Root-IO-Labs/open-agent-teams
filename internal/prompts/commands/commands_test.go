package commands

import (
	"testing"

	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

// TestSlashCommandAgentTypesValid ensures ForAgentTypes entries match real agent type strings.
func TestSlashCommandAgentTypesValid(t *testing.T) {
	known := map[string]struct{}{
		string(state.AgentTypeSupervisor):        {},
		string(state.AgentTypeWorker):            {},
		string(state.AgentTypeMergeQueue):        {},
		string(state.AgentTypePRShepherd):        {},
		string(state.AgentTypeWorkspace):         {},
		string(state.AgentTypeReview):            {},
		string(state.AgentTypeVerification):      {},
		string(state.AgentTypeGenericPersistent): {},
	}
	for _, cmd := range AvailableCommands {
		for _, at := range cmd.ForAgentTypes {
			if _, ok := known[at]; !ok {
				t.Errorf("command %q lists unknown agent type %q in ForAgentTypes", cmd.Name, at)
			}
		}
	}
}

func TestCommandAppliesToAgentType(t *testing.T) {
	refresh := AvailableCommands[0]
	if refresh.Name != "refresh" {
		t.Fatal("test assumes first command is refresh")
	}
	if !CommandAppliesToAgentType(refresh, "") {
		t.Error("empty agentType should include all commands")
	}
	if !CommandAppliesToAgentType(refresh, "worker") {
		t.Error("refresh should apply to worker")
	}
	if CommandAppliesToAgentType(refresh, "supervisor") {
		t.Error("refresh should not apply to supervisor")
	}
	status := AvailableCommands[1]
	if status.Name != "status" {
		t.Fatal("test assumes second command is status")
	}
	if !CommandAppliesToAgentType(status, "supervisor") {
		t.Error("status should apply to all agent types")
	}
}
