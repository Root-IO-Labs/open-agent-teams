// Package cli — virtual_repo.go owns the small helpers for the
// Part 5c "virtual repository" concept introduced by the
// side-panel-chat-and-status plan. A virtual repo is a state.Repository
// whose IsVirtual field is true: it has NO `.git`, NO worktree, NO
// supervisor / merge-queue / workspace agents — it exists purely as a
// container for AgentTypeAssistant (and any future persistent
// agent type whose lifecycle is dictated by the user rather than by
// the git surface).
//
// Today the only producer of virtual repos is the `oat assistant`
// CLI verb tree (Part 5d). This file is kept small and isolated so
// future agent-type containers can reuse the same primitives without
// pulling in any of the assistant-specific orchestration.

package cli

import (
	"fmt"
	"os"
	"regexp"

	"github.com/Root-IO-Labs/open-agent-teams/internal/errors"
)

// virtualRepoNamePattern is the whitelist for the user-supplied
// `<name>` half of a virtual repo's identifier
// (`_assistant-<name>`). Constraints (per Part 5c plan body):
//
//   - 1-32 characters
//   - ASCII letters (case-sensitive), digits, hyphen, underscore
//   - rejects `..`, `/`, `\`, whitespace, dots, every shell-active char
//
// The bounds bound the length of the resulting filesystem path
// (`~/.oat/repos/_assistant-<name>/`) inside POSIX NAME_MAX even on
// case-folding filesystems and after the daemon appends per-agent
// subpaths. The character set keeps `<name>` safe to splice into
// shell commands, log lines, and JSON without escaping.
var virtualRepoNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,32}$`)

// validateVirtualRepoName returns nil iff `name` is a legal virtual-
// repo `<name>` component. Exported as a package-private helper so
// the future `oat assistant` verbs (Part 5d) and the unit tests can
// both use the same validator.
func validateVirtualRepoName(name string) error {
	if name == "" {
		return errors.New(errors.CategoryUsage, "assistant name is required (default: 'personal')")
	}
	if len(name) > 32 {
		return errors.New(errors.CategoryUsage, fmt.Sprintf("assistant name %q is too long (max 32 chars; got %d)", name, len(name)))
	}
	if !virtualRepoNamePattern.MatchString(name) {
		return errors.New(errors.CategoryUsage, fmt.Sprintf("assistant name %q is invalid (allowed: letters, digits, '-', '_'; 1-32 chars)", name))
	}
	return nil
}

// virtualRepoNameFor returns the canonical state-key name of the
// virtual repo for an assistant called `name`. Centralized here so
// the prefix is changed in exactly one place if we ever rename it.
// The leading underscore makes the canonical name sort BEFORE all
// real-repo entries and means an existing `oat repo` whitelist
// regex that allows only `[a-zA-Z0-9-]+` will reject it
// automatically — a virtual repo is never a valid `oat repo init`
// target.
func virtualRepoNameFor(name string) string {
	return "_assistant-" + name
}

// ensureVirtualRepo is the idempotent creator used by `oat
// assistant start [name]`. Walks the four pieces of state a
// virtual repo needs:
//
//  1. Validate `<name>` (see validateVirtualRepoName).
//  2. Create `~/.oat/repos/_assistant-<name>/` as a plain directory
//     (no `.git`). Idempotent — pre-existing dir is fine.
//  3. Register the repo with the daemon via `add_repo`
//     `is_virtual=true`. Idempotent — pre-existing registration
//     is treated as success (the daemon's AddRepo already errors
//     on duplicate, so we swallow that one specific error and
//     read-back to confirm).
//  4. NOT spawned here: the assistant agent itself. That's the
//     caller's job (Part 5d).
//
// Returns the canonical virtual-repo state-key name on success
// (e.g. "_assistant-personal") so the caller can use it in
// subsequent socket calls without re-deriving it. Returns a
// CLIError on any validation / IO / daemon failure.
func (c *CLI) ensureVirtualRepo(name string) (string, error) {
	if err := validateVirtualRepoName(name); err != nil {
		return "", err
	}

	repoKey := virtualRepoNameFor(name)

	// Step 2: filesystem dir. Plain mkdir -p. IsVirtual repos
	// don't have `.git`, so we deliberately skip `git init`.
	// internal/hooks/hooks.go silently no-ops when
	// <repoPath>/.oat/hooks.json doesn't exist (verified during
	// plan-mode research), so the dir-only setup is safe.
	repoPath := c.paths.RepoDir(repoKey)
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		return "", errors.Wrap(errors.CategoryRuntime, fmt.Sprintf("failed to create virtual repo dir %s", repoPath), err)
	}

	// Step 3: register with the daemon. The session-name is a
	// deterministic derivation of the repo key (same convention as
	// real repos via sanitizeSessionName — see cmdRepoInit).
	sessionName := sanitizeSessionName(repoKey)

	// Idempotency probe first: if the repo is already registered we
	// don't need to send add_repo at all. Cheap because list_repos
	// is in-memory and the daemon's already running by the time
	// `oat assistant start` reaches this code path.
	if existing, err := c.listVirtualRepos(); err == nil {
		if _, present := existing[repoKey]; present {
			return repoKey, nil
		}
	}

	resp, err := c.sendDaemonRequest("add_repo", map[string]interface{}{
		"name":         repoKey,
		"github_url":   "", // virtual repos have no remote
		"session_name": sessionName,
		"is_virtual":   true,
	})
	if err != nil {
		return "", err
	}
	if !resp.Success {
		// AddRepo's only failure mode is duplicate-name (state-
		// level). We've already probed that above; if we still hit
		// it, surface the daemon's error verbatim.
		return "", errors.Wrap(errors.CategoryRuntime, "failed to register virtual repo with daemon", fmt.Errorf("%s", resp.Error))
	}

	return repoKey, nil
}

// listVirtualRepos returns the set of currently-registered virtual
// repos keyed by their state-key name (e.g. "_assistant-personal").
// Used by ensureVirtualRepo for idempotency and by the future
// `oat assistant list` for its filter-view rendering. Passes
// `include_virtual: true` so the daemon doesn't strip them; the
// caller then filters to is_virtual==true entries. This intentionally
// piggybacks on `list_repos` rather than introducing a separate
// `list_virtual_repos` socket verb — one less surface to maintain.
func (c *CLI) listVirtualRepos() (map[string]map[string]interface{}, error) {
	resp, err := c.sendDaemonRequest("list_repos", map[string]interface{}{
		"rich":            true,
		"include_virtual": true,
	})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("list_repos: %s", resp.Error)
	}
	rows, ok := resp.Data.([]interface{})
	if !ok {
		return nil, fmt.Errorf("list_repos returned unexpected shape: %T", resp.Data)
	}
	out := make(map[string]map[string]interface{}, len(rows))
	for _, row := range rows {
		m, ok := row.(map[string]interface{})
		if !ok {
			continue
		}
		isVirt, _ := m["is_virtual"].(bool)
		if !isVirt {
			continue
		}
		name, _ := m["name"].(string)
		if name == "" {
			continue
		}
		out[name] = m
	}
	return out, nil
}

