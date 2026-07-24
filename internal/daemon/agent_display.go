package daemon

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
	"golang.org/x/text/unicode/norm"
)

const (
	displayNameMaxLen = 64
)

// agentRunningModel is what the side-panel should show as the live model:
// ResolvedModel (spawn-time -M) when set, else configured Model. Fixes the
// Manage-tab "default" bug when Model is empty but ResolvedModel is set.
func agentRunningModel(a state.Agent) string {
	if a.ResolvedModel != "" {
		return a.ResolvedModel
	}
	return a.Model
}

// agentConfiguredModel is the operator preference (Agent.Model). Empty means
// "use repo/daemon default at next spawn" — not the same as running.
func agentConfiguredModel(a state.Agent) string {
	return a.Model
}

// modelNeedsDeferredRestart reports whether the next user Send should
// session-preserving-restart before delivery so the process picks up Model.
func modelNeedsDeferredRestart(a state.Agent) bool {
	if a.Model == "" {
		return false
	}
	if a.ResolvedModel == "" {
		// Never spawned / unknown running — restart path will set ResolvedModel.
		return a.PID > 0
	}
	return a.Model != a.ResolvedModel
}

// normalizeDisplayNameForCompare collapses an alias for uniqueness checks:
// NFKC, trim, collapse internal whitespace, case-fold via EqualFold-ready lower.
// Returns empty if the raw value clears to nothing.
func normalizeDisplayNameForCompare(raw string) string {
	s := norm.NFKC.String(raw)
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		prevSpace = false
		b.WriteRune(unicode.ToLower(r))
	}
	return strings.TrimSpace(b.String())
}

// validateDisplayNameInput checks raw user input before persist.
// Empty/whitespace → cleared (ok, stored empty). Own slug match → clear.
// Rejects control/zero-width chars and over-long values.
func validateDisplayNameInput(raw, agentName string) (stored string, err error) {
	s := strings.TrimSpace(norm.NFKC.String(raw))
	if s == "" {
		return "", nil
	}
	if utf8.RuneCountInString(s) > displayNameMaxLen {
		return "", fmt.Errorf("display name must be at most %d characters", displayNameMaxLen)
	}
	for _, r := range s {
		if r == '\u200b' || r == '\u200c' || r == '\u200d' || r == '\ufeff' {
			return "", fmt.Errorf("display name must not contain zero-width characters")
		}
		if unicode.IsControl(r) {
			return "", fmt.Errorf("display name must not contain control characters")
		}
	}
	if strings.EqualFold(strings.TrimSpace(s), strings.TrimSpace(agentName)) {
		return "", nil // own slug → clear
	}
	return s, nil
}

// displayNameCollision reports whether normalized alias collides with another
// Assistant/Browser agent's name or display_name (global scope).
func displayNameCollision(st *state.State, selfRepo, selfAgent, normalized string) (conflictRepo, conflictAgent string, ok bool) {
	if normalized == "" || st == nil {
		return "", "", false
	}
	for repoName, repo := range st.GetAllRepos() {
		for agentName, agent := range repo.Agents {
			if !agent.Type.IsPausable() {
				continue
			}
			if repoName == selfRepo && agentName == selfAgent {
				continue
			}
			if normalizeDisplayNameForCompare(agentName) == normalized {
				return repoName, agentName, true
			}
			if other := normalizeDisplayNameForCompare(agent.DisplayName); other != "" && other == normalized {
				return repoName, agentName, true
			}
		}
	}
	return "", "", false
}
