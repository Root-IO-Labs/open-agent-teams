package cli

import (
	"strings"
	"testing"

	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

// TestDocsFilterTrimsForAgents guards the agent-type-filtered CLI docs:
// agents must not receive the full CLI reference. Each agent type should
// drop the operator-only commands, and the drop must be substantial
// (anything under a few KB suggests the filter was silently disabled).
func TestDocsFilterTrimsForAgents(t *testing.T) {
	c, err := New()
	if err != nil {
		t.Fatalf("cli.New: %v", err)
	}
	full := c.GenerateDocumentation()
	if len(full) == 0 {
		t.Fatal("full documentation empty")
	}

	// Commands that should never appear in any agent's injected reference.
	// Keep this list in sync with humanOnlyCLICommands.
	humanOnly := []string{
		"## daemon\n", "## stop-all\n", "## repo\n", "## model\n",
		"## bug\n", "## diagnostics\n", "## docs\n", "## ui\n",
		"## repair\n", "## version\n",
	}

	agentTypes := []state.AgentType{
		state.AgentTypeSupervisor,
		state.AgentTypeMergeQueue,
		state.AgentTypeWorker,
		state.AgentTypePRShepherd,
		state.AgentTypeWorkspace,
		state.AgentTypeReview,
		state.AgentTypeVerification,
	}

	const minSavedBytes = 4000 // guard against the filter being disabled

	for _, at := range agentTypes {
		filtered := c.GenerateDocumentationForAgent(at)
		saved := len(full) - len(filtered)
		t.Logf("%-12s %d bytes (%d saved)", at, len(filtered), saved)
		if saved < minSavedBytes {
			t.Errorf("%s: only %d bytes saved, expected >= %d", at, saved, minSavedBytes)
		}
		for _, marker := range humanOnly {
			if strings.Contains(filtered, marker) {
				t.Errorf("%s: filtered docs still contain human-only section %q", at, strings.TrimSpace(marker))
			}
		}
	}

	// review is supervisor-only; workers must not see it.
	workerDocs := c.GenerateDocumentationForAgent(state.AgentTypeWorker)
	if strings.Contains(workerDocs, "## review\n") {
		t.Error("worker docs should not contain the review command")
	}
	supervisorDocs := c.GenerateDocumentationForAgent(state.AgentTypeSupervisor)
	if !strings.Contains(supervisorDocs, "## review\n") {
		t.Error("supervisor docs should contain the review command")
	}
}

// TestDocsFilterCachesPerAgent verifies docsFor memoizes per agent type.
func TestDocsFilterCachesPerAgent(t *testing.T) {
	c, err := New()
	if err != nil {
		t.Fatalf("cli.New: %v", err)
	}
	first := c.docsFor(state.AgentTypeWorker)
	second := c.docsFor(state.AgentTypeWorker)
	if first != second {
		t.Error("docsFor returned different strings for the same agent type")
	}
	if _, ok := c.docsByAgent[state.AgentTypeWorker]; !ok {
		t.Error("docsFor did not populate the cache")
	}
}
