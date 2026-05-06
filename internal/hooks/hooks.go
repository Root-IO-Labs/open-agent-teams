// Package hooks provides utilities for managing hooks configuration for OAT Agent Runtime.
package hooks

import (
	"fmt"
	"os"
	"path/filepath"
)

// CopyConfig copies hooks configuration from repo to workdir if it exists.
// The hooks.json file in .oat directory is copied to .oat/settings.json
// in the target directory, allowing OAT Agent Runtime to use custom hooks in worktrees.
func CopyConfig(repoPath, workDir string) error {
	hooksPath := filepath.Join(repoPath, ".oat", "hooks.json")

	// Check if hooks.json exists
	if _, err := os.Stat(hooksPath); os.IsNotExist(err) {
		return nil // No hooks config, that's fine
	} else if err != nil {
		return fmt.Errorf("failed to check hooks config: %w", err)
	}

	// Create .oat directory in workdir
	oatDir := filepath.Join(workDir, ".oat")
	if err := os.MkdirAll(oatDir, 0755); err != nil {
		return fmt.Errorf("failed to create .oat directory: %w", err)
	}

	// Copy hooks.json to .oat/settings.json
	hooksData, err := os.ReadFile(hooksPath)
	if err != nil {
		return fmt.Errorf("failed to read hooks config: %w", err)
	}

	settingsPath := filepath.Join(oatDir, "settings.json")
	if err := os.WriteFile(settingsPath, hooksData, 0644); err != nil {
		return fmt.Errorf("failed to write settings.json: %w", err)
	}

	return nil
}
