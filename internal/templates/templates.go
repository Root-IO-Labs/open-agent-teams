// Package templates provides embedded agent templates that are copied to
// per-repository agent directories during initialization.
package templates

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Embed the agent-templates directory from the repository root
//
//go:embed all:agent-templates
var agentTemplates embed.FS

// CopyAgentTemplates copies all agent template files from the embedded
// agent-templates directory to the specified destination directory.
// The destination directory will be created if it doesn't exist.
func CopyAgentTemplates(destDir string) error {
	// Create the destination directory
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("failed to create agents directory: %w", err)
	}

	// Walk the embedded filesystem and copy all .md files
	err := fs.WalkDir(agentTemplates, "agent-templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Skip the root directory
		if path == "agent-templates" {
			return nil
		}

		// Only copy .md files
		if d.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}

		// Read the embedded file
		content, err := agentTemplates.ReadFile(path)
		if err != nil {
			return fmt.Errorf("failed to read template %s: %w", path, err)
		}

		// Get the filename (strip the "agent-templates/" prefix)
		filename := filepath.Base(path)
		destPath := filepath.Join(destDir, filename)

		// Write to destination
		if err := os.WriteFile(destPath, content, 0644); err != nil {
			return fmt.Errorf("failed to write template %s: %w", destPath, err)
		}

		return nil
	})

	if err != nil {
		return fmt.Errorf("failed to copy agent templates: %w", err)
	}

	return nil
}

// ReadEmbeddedAgentTemplate returns the byte content of the embedded
// `agent-templates/<name>.md` template, or `(nil, os.ErrNotExist)` if
// no such template is embedded. The `.md` suffix is added if absent.
// Used by the daemon's prompt-refresh logic (Part 4.H) so a per-repo
// agents/ dir can be reconciled with the embedded template content
// without re-walking the entire embedded filesystem.
func ReadEmbeddedAgentTemplate(name string) ([]byte, error) {
	if filepath.Ext(name) != ".md" {
		name += ".md"
	}
	content, err := agentTemplates.ReadFile(filepath.Join("agent-templates", name))
	if err != nil {
		return nil, os.ErrNotExist
	}
	return content, nil
}

// SyncAgentTemplates ensures every .md file in destDir matches its
// embedded counterpart byte-for-byte; any drift is overwritten with
// the embedded content. Returns the list of basenames that were
// refreshed (empty when nothing changed) plus any walk error.
//
// Part 4.H: this is the embedded-newer-wins refresh that
// CopyAgentTemplates used to only fire on first-time creation (when
// destDir didn't exist). Now an idempotent diff-and-write per call,
// safe to invoke on every agent start AND from the `oat agent
// refresh-prompts` CLI verb.
//
// Non-destructive: only touches files that have an embedded
// counterpart, leaves user-authored files alone (e.g. anything
// not named like one of the embedded templates is ignored).
// Repository-specific customization is handled separately via
// prompts.LoadCustomPrompt, which the daemon appends under a
// dedicated heading — it does NOT live in this dir.
func SyncAgentTemplates(destDir string) ([]string, error) {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create agents directory: %w", err)
	}
	var refreshed []string
	walkErr := fs.WalkDir(agentTemplates, "agent-templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == "agent-templates" || d.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}
		embedded, err := agentTemplates.ReadFile(path)
		if err != nil {
			return fmt.Errorf("failed to read embedded template %s: %w", path, err)
		}
		filename := filepath.Base(path)
		destPath := filepath.Join(destDir, filename)
		onDisk, readErr := os.ReadFile(destPath)
		if readErr != nil {
			if !os.IsNotExist(readErr) {
				return fmt.Errorf("failed to read on-disk template %s: %w", destPath, readErr)
			}
			// Treat "does not exist" as "drift" so the first-run
			// fresh-clone path lands in the same branch as a stale-file
			// refresh — one code path instead of two.
		} else if string(onDisk) == string(embedded) {
			return nil
		}
		if err := os.WriteFile(destPath, embedded, 0644); err != nil {
			return fmt.Errorf("failed to write template %s: %w", destPath, err)
		}
		refreshed = append(refreshed, filename)
		return nil
	})
	if walkErr != nil {
		return refreshed, fmt.Errorf("failed to sync agent templates: %w", walkErr)
	}
	return refreshed, nil
}

// ListAgentTemplates returns the names of all available agent templates.
func ListAgentTemplates() ([]string, error) {
	var templates []string

	entries, err := agentTemplates.ReadDir("agent-templates")
	if err != nil {
		return nil, fmt.Errorf("failed to read agent templates: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".md" {
			templates = append(templates, entry.Name())
		}
	}

	return templates, nil
}
