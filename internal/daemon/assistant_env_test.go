// Tests for the Part 5f env-var preparation helper. Tiny surface,
// pinned with three properties:
//
//  1. Non-assistant types return nil. A regression here would
//     leak OAT_AGENT_TYPE=assistant into worker/supervisor/etc.
//     processes, and any future memory middleware would key into
//     the wrong scope.
//  2. Assistant emits exactly the three documented vars in the
//     documented order. Order matters because the consumer side
//     hasn't been built -- when it is, it may iterate the env
//     and expect a stable shape.
//  3. OAT_REPO carries the canonical `_assistant-<name>` prefix
//     through unchanged. The virtual-repo key is what links the
//     env block to Part 5c's virtual repo on disk.

package daemon

import (
	"strings"
	"testing"

	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

func TestAssistantSpawnEnvVars_NonAssistantReturnsNil_Part5f(t *testing.T) {
	cases := []state.AgentType{
		state.AgentTypeWorker,
		state.AgentTypeSupervisor,
		state.AgentTypeMergeQueue,
		state.AgentTypePRShepherd,
		state.AgentTypeWorkspace,
		state.AgentTypeBrowser,
		state.AgentTypeReview,
		state.AgentTypeVerification,
	}
	for _, typ := range cases {
		t.Run(string(typ), func(t *testing.T) {
			got := assistantSpawnEnvVars(typ, "_assistant-personal")
			if got != nil {
				t.Errorf("type %q must return nil; got %v", typ, got)
			}
		})
	}
}

func TestAssistantSpawnEnvVars_Assistant_Part5f(t *testing.T) {
	got := assistantSpawnEnvVars(state.AgentTypeAssistant, "_assistant-personal")
	want := []string{
		"OAT_AGENT_TYPE=assistant",
		"OAT_REPO=_assistant-personal",
		"OAT_MEMORY_ENABLED=1",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d vars, want %d: %v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("var[%d] = %q, want %q", i, got[i], w)
		}
	}
}

func TestAssistantSpawnEnvVars_RepoNameAlongForRide_Part5f(t *testing.T) {
	// Different repo names should produce different OAT_REPO
	// values. Pins the "passthrough" property -- if a future
	// refactor accidentally hardcodes the repo to "personal" or
	// strips the prefix, this fails.
	cases := []struct {
		repoName string
		wantInOAT string
	}{
		{"_assistant-personal", "OAT_REPO=_assistant-personal"},
		{"_assistant-work", "OAT_REPO=_assistant-work"},
		{"_assistant-my-bot", "OAT_REPO=_assistant-my-bot"},
		// Non-virtual-prefixed name is still passed through
		// unchanged (the helper doesn't validate; that's a Part
		// 5c concern). Pin to catch any over-eager sanitization.
		{"some-other-name", "OAT_REPO=some-other-name"},
		{"", "OAT_REPO="},
	}
	for _, tc := range cases {
		t.Run(tc.repoName, func(t *testing.T) {
			got := assistantSpawnEnvVars(state.AgentTypeAssistant, tc.repoName)
			found := false
			for _, v := range got {
				if v == tc.wantInOAT {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("repo %q: missing %q in %v", tc.repoName, tc.wantInOAT, got)
			}
		})
	}
}

// Cross-check: the env vars use exactly the prefix the plan body
// documents. If any one of these strings drifts, the contract
// with the (future) memory middleware breaks silently. Strict
// prefix match (not substring) so a typo like
// "OAT_AGENT_TYPES=assistant" (extra S) doesn't pass.
func TestAssistantSpawnEnvVars_VariableNames_Part5f(t *testing.T) {
	got := assistantSpawnEnvVars(state.AgentTypeAssistant, "_assistant-personal")
	want := []string{
		"OAT_AGENT_TYPE=",
		"OAT_REPO=",
		"OAT_MEMORY_ENABLED=",
	}
	for i, prefix := range want {
		if !strings.HasPrefix(got[i], prefix) {
			t.Errorf("var[%d] = %q does not start with documented prefix %q", i, got[i], prefix)
		}
	}
}
