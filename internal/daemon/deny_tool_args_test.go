package daemon

import (
	"testing"

	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

// TestDenyToolArgs_BrowserAgentGetsFullList locks in that browser agents are
// always spawned with the four-flag deny list. Drift here would silently let
// the LLM call back the very tools we filter (task / http_request / fetch_url
// / compact_conversation), which has historically produced the "iana mystery"
// and "agent stuck processing" bugs documented in CHANGELOG.
func TestDenyToolArgs_BrowserAgentGetsFullList(t *testing.T) {
	got := denyToolArgs(state.AgentTypeBrowser)

	want := []string{
		"--deny-tool", "task",
		"--deny-tool", "http_request",
		"--deny-tool", "fetch_url",
		"--deny-tool", "compact_conversation",
	}
	if len(got) != len(want) {
		t.Fatalf("denyToolArgs(Browser) length = %d, want %d (got=%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("denyToolArgs(Browser)[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestDenyToolArgs_NonBrowserAgentsGetEmpty makes sure we are not accidentally
// stripping tools from worker / supervisor / merge-queue / review /
// verification / pr-shepherd agents. They need the full catalog (especially
// `task` for SubAgent delegation in coding workflows) and any leakage here
// would be a silent behaviour change.
func TestDenyToolArgs_NonBrowserAgentsGetEmpty(t *testing.T) {
	others := []state.AgentType{
		state.AgentTypeWorker,
		state.AgentTypeSupervisor,
		state.AgentTypeMergeQueue,
		state.AgentTypeReview,
		state.AgentTypeVerification,
		state.AgentTypePRShepherd,
		state.AgentTypeWorkspace,
	}
	for _, agentType := range others {
		got := denyToolArgs(agentType)
		if len(got) != 0 {
			t.Errorf("denyToolArgs(%v) = %v, want nil/empty", agentType, got)
		}
	}
}

// TestDenyToolArgs_FlagShapeIsPairs guards the invariant that callers can
// `append(args, denyToolArgs(t)...)` and produce a well-formed argv: each
// `--deny-tool` flag must be immediately followed by exactly one name. If we
// ever reshape this to use `--deny-tool=NAME` we need to update callers too,
// and this test will surface the change.
func TestDenyToolArgs_FlagShapeIsPairs(t *testing.T) {
	got := denyToolArgs(state.AgentTypeBrowser)
	if len(got)%2 != 0 {
		t.Fatalf("denyToolArgs(Browser) length = %d, want even (flag/value pairs); got=%v", len(got), got)
	}
	for i := 0; i < len(got); i += 2 {
		if got[i] != "--deny-tool" {
			t.Errorf("expected --deny-tool at index %d, got %q (full=%v)", i, got[i], got)
		}
		if got[i+1] == "" {
			t.Errorf("expected non-empty tool name at index %d, got empty (full=%v)", i+1, got)
		}
	}
}
