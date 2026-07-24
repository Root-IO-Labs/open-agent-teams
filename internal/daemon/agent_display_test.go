package daemon

import (
	"testing"

	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

func TestAgentRunningModel(t *testing.T) {
	t.Parallel()
	if got := agentRunningModel(state.Agent{ResolvedModel: "google_genai:gemini-2.5-flash"}); got != "google_genai:gemini-2.5-flash" {
		t.Fatalf("resolved wins: got %q", got)
	}
	if got := agentRunningModel(state.Agent{Model: "anthropic:claude-sonnet-5"}); got != "anthropic:claude-sonnet-5" {
		t.Fatalf("model fallback: got %q", got)
	}
	if got := agentRunningModel(state.Agent{
		Model:         "anthropic:claude-sonnet-5",
		ResolvedModel: "google_genai:gemini-2.5-flash",
	}); got != "google_genai:gemini-2.5-flash" {
		t.Fatalf("running prefers resolved over configured: got %q", got)
	}
}

func TestModelNeedsDeferredRestart(t *testing.T) {
	t.Parallel()
	if modelNeedsDeferredRestart(state.Agent{Model: "", ResolvedModel: "x"}) {
		t.Fatal("empty configured should not restart")
	}
	if modelNeedsDeferredRestart(state.Agent{Model: "a", ResolvedModel: "a"}) {
		t.Fatal("aligned should not restart")
	}
	if !modelNeedsDeferredRestart(state.Agent{Model: "a", ResolvedModel: "b"}) {
		t.Fatal("mismatch should restart")
	}
}

func TestValidateDisplayNameInput(t *testing.T) {
	t.Parallel()
	stored, err := validateDisplayNameInput("  Work Sonnet  ", "sonnettest")
	if err != nil || stored != "Work Sonnet" {
		t.Fatalf("trim: stored=%q err=%v", stored, err)
	}
	stored, err = validateDisplayNameInput("sonnettest", "sonnettest")
	if err != nil || stored != "" {
		t.Fatalf("own slug clears: stored=%q err=%v", stored, err)
	}
	if _, err := validateDisplayNameInput("bad\u200bname", "a"); err == nil {
		t.Fatal("zero-width should reject")
	}
	if _, err := validateDisplayNameInput(string(make([]rune, 65)), "a"); err == nil {
		t.Fatal("overlong should reject")
	}
}

func TestDisplayNameCollision(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()
	_ = d.state.AddRepo("_assistant-personal", &state.Repository{IsVirtual: true, Agents: map[string]state.Agent{}})
	_ = d.state.AddAgent("_assistant-personal", "personal", state.Agent{Type: state.AgentTypeAssistant})
	_ = d.state.AddRepo("_assistant-sonnettest", &state.Repository{IsVirtual: true, Agents: map[string]state.Agent{}})
	_ = d.state.AddAgent("_assistant-sonnettest", "sonnettest", state.Agent{
		Type:        state.AgentTypeAssistant,
		DisplayName: "Work Sonnet",
	})

	norm := normalizeDisplayNameForCompare("personal")
	if _, _, hit := displayNameCollision(d.state, "_assistant-sonnettest", "sonnettest", norm); !hit {
		t.Fatal("alias matching another agent name should collide")
	}
	norm = normalizeDisplayNameForCompare("work  sonnet")
	if _, _, hit := displayNameCollision(d.state, "_assistant-personal", "personal", norm); !hit {
		t.Fatal("alias matching another display_name should collide")
	}
	if _, _, hit := displayNameCollision(d.state, "_assistant-sonnettest", "sonnettest", normalizeDisplayNameForCompare("Work Sonnet")); hit {
		t.Fatal("self should not collide")
	}
}

func TestNormalizeDisplayNameForCompare(t *testing.T) {
	t.Parallel()
	if a, b := normalizeDisplayNameForCompare("  Work   Sonnet "), normalizeDisplayNameForCompare("work sonnet"); a != b {
		t.Fatalf("expected equal fold+space collapse: %q vs %q", a, b)
	}
}
