package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Root-IO-Labs/open-agent-teams/internal/socket"
	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

// Test coverage for handleSetAgentModel (Part 4 follow-up — new
// `oat agent set-model` CLI verb, replacing the hand-edit-
// state.json workflow that the 2026-05-22 Opus 4.7 onboarding
// session had to walk an agent through).
//
// The handler is responsible for:
//   - Argument validation (repo, agent, model all required).
//   - Existence checks (the repo + the agent must already exist;
//     this is a set-existing, not a create-or-update).
//   - Model validation against loaded profiles when present
//     (typo here, not at next agent restart).
//   - Canonicalization (so `claude-opus-4-7` and
//     `anthropic:claude-opus-4-7` both persist as the prefixed
//     form, matching `oat model onboard` semantics).
//   - Allow-list enforcement for worker agents when
//     AllowedWorkerModels is set on the repo.
//   - Atomic state update via ModifyAgent (preserves the rest of
//     the agent's fields — PID, session, worktree, etc.).
//   - A no-op success path when the model is already set to the
//     requested value (so a chained --restart can still proceed).
//   - Response data with prior_model + new_model + changed +
//     requires_restart so the CLI can render the right wording
//     and decide whether to nudge the user.

func setupSetModelTestState(t *testing.T) *Daemon {
	t.Helper()
	d, cleanup := setupTestDaemonWithState(t, nil)
	t.Cleanup(cleanup)

	// Onboard one worker-eligible and one orchestrator-eligible model
	// so the validation surface gets exercised end-to-end.
	profileYAML := `model_id: "anthropic:claude-sonnet-4-6"
status: known
provider:
  name: anthropic
routing:
  autonomy_tier: full
  overall_score: 99
contract:
  onboarding_passed: true
  worker_eligible: true
  orchestrator_eligible: true
`
	if err := os.WriteFile(filepath.Join(d.paths.ModelProfilesDir, "p1.yaml"), []byte(profileYAML), 0644); err != nil {
		t.Fatal(err)
	}
	profileYAML2 := `model_id: "anthropic:claude-opus-4-7"
status: known
provider:
  name: anthropic
routing:
  autonomy_tier: full
  overall_score: 100
contract:
  onboarding_passed: true
  worker_eligible: true
  orchestrator_eligible: true
`
	if err := os.WriteFile(filepath.Join(d.paths.ModelProfilesDir, "p2.yaml"), []byte(profileYAML2), 0644); err != nil {
		t.Fatal(err)
	}
	if err := d.modelProfiles.Reload(); err != nil {
		t.Fatal(err)
	}

	if err := d.state.AddRepo("test-repo", &state.Repository{
		GithubURL:   "https://github.com/test/repo",
		SessionName: "test-session",
		Agents:      make(map[string]state.Agent),
	}); err != nil {
		t.Fatal(err)
	}

	// Seed a browser agent on sonnet-4-6.
	if err := d.state.AddAgent("test-repo", "browser-agent", state.Agent{
		Type:         state.AgentTypeBrowser,
		WorktreePath: "/tmp/ba",
		WindowName:   "browser-agent",
		SessionID:    "test-session",
		PID:          0,
		Model:        "anthropic:claude-sonnet-4-6",
	}); err != nil {
		t.Fatal(err)
	}

	return d
}

func TestHandleSetAgentModel_ArgValidation(t *testing.T) {
	d := setupSetModelTestState(t)

	tests := []struct {
		name    string
		args    map[string]interface{}
		wantErr string
	}{
		{
			name:    "missing repo",
			args:    map[string]interface{}{"agent": "browser-agent", "model": "anthropic:claude-opus-4-7"},
			wantErr: "missing 'repo'",
		},
		{
			name:    "missing agent",
			args:    map[string]interface{}{"repo": "test-repo", "model": "anthropic:claude-opus-4-7"},
			wantErr: "missing 'agent'",
		},
		{
			name:    "missing model",
			args:    map[string]interface{}{"repo": "test-repo", "agent": "browser-agent"},
			wantErr: "missing 'model'",
		},
		{
			name:    "unknown repo",
			args:    map[string]interface{}{"repo": "nope", "agent": "browser-agent", "model": "anthropic:claude-opus-4-7"},
			wantErr: "repository \"nope\" not found",
		},
		{
			name:    "unknown agent",
			args:    map[string]interface{}{"repo": "test-repo", "agent": "nope", "model": "anthropic:claude-opus-4-7"},
			wantErr: "agent \"nope\" not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := d.handleSetAgentModel(socket.Request{
				Command: "set_agent_model",
				Args:    tt.args,
			})
			if resp.Success {
				t.Fatalf("expected failure, got success")
			}
			if !strings.Contains(resp.Error, tt.wantErr) {
				t.Errorf("error %q does not contain %q", resp.Error, tt.wantErr)
			}
		})
	}
}

func TestHandleSetAgentModel_HappyPath(t *testing.T) {
	d := setupSetModelTestState(t)

	resp := d.handleSetAgentModel(socket.Request{
		Command: "set_agent_model",
		Args: map[string]interface{}{
			"repo":  "test-repo",
			"agent": "browser-agent",
			"model": "anthropic:claude-opus-4-7",
		},
	})
	if !resp.Success {
		t.Fatalf("expected success, got error: %s", resp.Error)
	}

	agent, ok := d.state.GetAgent("test-repo", "browser-agent")
	if !ok {
		t.Fatal("agent disappeared from state")
	}
	if agent.Model != "anthropic:claude-opus-4-7" {
		t.Errorf("agent.Model = %q, want anthropic:claude-opus-4-7", agent.Model)
	}
	// Other fields must be left alone — this is what ModifyAgent buys us.
	if agent.Type != state.AgentTypeBrowser {
		t.Errorf("agent.Type changed to %q", agent.Type)
	}
	if agent.WorktreePath != "/tmp/ba" {
		t.Errorf("agent.WorktreePath changed to %q", agent.WorktreePath)
	}
	if agent.SessionID != "test-session" {
		t.Errorf("agent.SessionID changed to %q", agent.SessionID)
	}

	// Response metadata so the CLI can render "X -> Y" and decide
	// whether to nudge about --restart.
	data, ok := resp.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("response Data is not a map: %T", resp.Data)
	}
	if data["prior_model"] != "anthropic:claude-sonnet-4-6" {
		t.Errorf("prior_model = %v", data["prior_model"])
	}
	if data["new_model"] != "anthropic:claude-opus-4-7" {
		t.Errorf("new_model = %v", data["new_model"])
	}
	if data["changed"] != true {
		t.Errorf("changed = %v, want true", data["changed"])
	}
	if data["requires_restart"] != true {
		t.Errorf("requires_restart = %v, want true", data["requires_restart"])
	}
}

func TestHandleSetAgentModel_Canonicalization(t *testing.T) {
	// Operator typing the short form should land on the canonical
	// (always-prefixed) form in state.json — matches the
	// `oat model onboard` shape so state stays consistent.
	d := setupSetModelTestState(t)

	resp := d.handleSetAgentModel(socket.Request{
		Command: "set_agent_model",
		Args: map[string]interface{}{
			"repo":  "test-repo",
			"agent": "browser-agent",
			"model": "claude-opus-4-7",
		},
	})
	if !resp.Success {
		t.Fatalf("expected success, got error: %s", resp.Error)
	}
	agent, _ := d.state.GetAgent("test-repo", "browser-agent")
	if agent.Model != "anthropic:claude-opus-4-7" {
		t.Errorf("agent.Model = %q, want canonical form anthropic:claude-opus-4-7", agent.Model)
	}
	data := resp.Data.(map[string]interface{})
	if data["new_model"] != "anthropic:claude-opus-4-7" {
		t.Errorf("new_model = %v, want canonical form", data["new_model"])
	}
}

func TestHandleSetAgentModel_NoOpWhenAlreadySet(t *testing.T) {
	// Agent already on the requested model — return success with
	// changed=false + requires_restart=false so a chained --restart
	// in the CLI doesn't fire unnecessarily.
	d := setupSetModelTestState(t)

	resp := d.handleSetAgentModel(socket.Request{
		Command: "set_agent_model",
		Args: map[string]interface{}{
			"repo":  "test-repo",
			"agent": "browser-agent",
			"model": "anthropic:claude-sonnet-4-6",
		},
	})
	if !resp.Success {
		t.Fatalf("expected success (no-op), got error: %s", resp.Error)
	}
	data := resp.Data.(map[string]interface{})
	if data["changed"] != false {
		t.Errorf("changed = %v, want false on no-op", data["changed"])
	}
	if data["requires_restart"] != false {
		t.Errorf("requires_restart = %v, want false on no-op", data["requires_restart"])
	}
}

func TestHandleSetAgentModel_RejectsUnknownModel(t *testing.T) {
	d := setupSetModelTestState(t)

	resp := d.handleSetAgentModel(socket.Request{
		Command: "set_agent_model",
		Args: map[string]interface{}{
			"repo":  "test-repo",
			"agent": "browser-agent",
			"model": "anthropic:claude-fake-99",
		},
	})
	if resp.Success {
		t.Fatal("expected failure for unknown model, got success")
	}
	if !strings.Contains(resp.Error, "rejected") {
		t.Errorf("error %q does not mention 'rejected'", resp.Error)
	}
	if !strings.Contains(resp.Error, "oat model onboard") {
		t.Errorf("error %q should suggest `oat model onboard`", resp.Error)
	}
	// State must not have been mutated.
	agent, _ := d.state.GetAgent("test-repo", "browser-agent")
	if agent.Model != "anthropic:claude-sonnet-4-6" {
		t.Errorf("agent.Model mutated to %q on rejection", agent.Model)
	}
}

func TestHandleSetAgentModel_WorkerAllowList(t *testing.T) {
	// Workers with an AllowedWorkerModels list set on the repo
	// must have their requested model intersect that list. This
	// mirrors the same constraint in handleAddAgent (so the
	// set-model surface stays consistent with the add-agent surface).
	d := setupSetModelTestState(t)
	if err := d.state.ModifyRepo("test-repo", func(r *state.Repository) {
		r.AllowedWorkerModels = []string{"anthropic:claude-sonnet-4-6"}
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.state.AddAgent("test-repo", "worker-eagle", state.Agent{
		Type:         state.AgentTypeWorker,
		WorktreePath: "/tmp/eagle",
		WindowName:   "worker-eagle",
		SessionID:    "test-session",
		Model:        "anthropic:claude-sonnet-4-6",
	}); err != nil {
		t.Fatal(err)
	}

	// Switching to a model NOT in the allow list must fail.
	resp := d.handleSetAgentModel(socket.Request{
		Command: "set_agent_model",
		Args: map[string]interface{}{
			"repo":  "test-repo",
			"agent": "worker-eagle",
			"model": "anthropic:claude-opus-4-7",
		},
	})
	if resp.Success {
		t.Fatal("expected failure when switching worker to a model outside the allow list")
	}
	if !strings.Contains(resp.Error, "not in the allowed worker models") {
		t.Errorf("error %q should mention the allow-list constraint", resp.Error)
	}
}
