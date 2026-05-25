package templates

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestListAgentTemplates(t *testing.T) {
	templates, err := ListAgentTemplates()
	if err != nil {
		t.Fatalf("ListAgentTemplates failed: %v", err)
	}

	// Check that we have the expected templates. Part 5b added
	// `assistant.md` (the AgentTypeAssistant prompt) and
	// `_shared-browser-safety.md` (the safety fragment both
	// browser.md and assistant.md load via writePromptFileWithPrefix
	// in internal/daemon/daemon.go). The leading-underscore filename
	// is intentional: it cannot collide with any state.AgentType
	// string, and any future code that filters out underscore-
	// prefixed entries (e.g. an "agent type picker" UI) can rely on
	// the convention.
	expected := map[string]bool{
		"_shared-browser-safety.md": true,
		"agent-builder.md":          true,
		"assistant.md":              true,
		"browser.md":                true,
		"merge-queue.md":            true,
		"pr-shepherd.md":            true,
		"worker.md":                 true,
		"reviewer.md":               true,
		"verification.md":           true,
	}

	if len(templates) != len(expected) {
		t.Errorf("Expected %d templates, got %d: %v", len(expected), len(templates), templates)
	}

	for _, tmpl := range templates {
		if !expected[tmpl] {
			t.Errorf("Unexpected template: %s", tmpl)
		}
	}
}

func TestCopyAgentTemplates(t *testing.T) {
	// Create a temporary directory
	tmpDir, err := os.MkdirTemp("", "templates-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	destDir := filepath.Join(tmpDir, "agents")

	// Copy templates
	if err := CopyAgentTemplates(destDir); err != nil {
		t.Fatalf("CopyAgentTemplates failed: %v", err)
	}

	// Verify the destination directory was created
	if _, err := os.Stat(destDir); os.IsNotExist(err) {
		t.Error("Destination directory was not created")
	}

	// Verify all expected files exist and have content
	expectedFiles := []string{"agent-builder.md", "browser.md", "merge-queue.md", "pr-shepherd.md", "worker.md", "reviewer.md", "verification.md"}
	for _, filename := range expectedFiles {
		path := filepath.Join(destDir, filename)
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			t.Errorf("Expected file %s does not exist", filename)
			continue
		}
		if err != nil {
			t.Errorf("Error checking file %s: %v", filename, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("File %s is empty", filename)
		}
	}
}

func TestCopyAgentTemplatesIdempotent(t *testing.T) {
	// Create a temporary directory
	tmpDir, err := os.MkdirTemp("", "templates-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	destDir := filepath.Join(tmpDir, "agents")

	// Copy templates twice - should not error
	if err := CopyAgentTemplates(destDir); err != nil {
		t.Fatalf("First CopyAgentTemplates failed: %v", err)
	}
	if err := CopyAgentTemplates(destDir); err != nil {
		t.Fatalf("Second CopyAgentTemplates failed: %v", err)
	}
}

func TestCopyAgentTemplatesErrorHandling(t *testing.T) {
	t.Run("errors when destination is read-only", func(t *testing.T) {
		tmpDir, err := os.MkdirTemp("", "templates-test-*")
		if err != nil {
			t.Fatalf("Failed to create temp dir: %v", err)
		}
		defer os.RemoveAll(tmpDir)

		// Create a read-only directory
		destDir := filepath.Join(tmpDir, "readonly")
		if err := os.MkdirAll(destDir, 0755); err != nil {
			t.Fatalf("Failed to create readonly dir: %v", err)
		}

		// Make directory read-only
		if err := os.Chmod(destDir, 0444); err != nil {
			t.Fatalf("Failed to chmod: %v", err)
		}
		defer os.Chmod(destDir, 0755) // Restore permissions for cleanup

		// Attempt to copy should fail when trying to write files
		err = CopyAgentTemplates(destDir)
		if err == nil {
			t.Error("Expected error when writing to read-only directory")
		}
	})

	t.Run("handles nested directory creation", func(t *testing.T) {
		tmpDir, err := os.MkdirTemp("", "templates-test-*")
		if err != nil {
			t.Fatalf("Failed to create temp dir: %v", err)
		}
		defer os.RemoveAll(tmpDir)

		// Use a nested path that doesn't exist
		destDir := filepath.Join(tmpDir, "level1", "level2", "agents")

		// Should create all parent directories
		if err := CopyAgentTemplates(destDir); err != nil {
			t.Fatalf("CopyAgentTemplates failed with nested path: %v", err)
		}

		// Verify directory was created
		if _, err := os.Stat(destDir); os.IsNotExist(err) {
			t.Error("Nested destination directory was not created")
		}

		// Verify files were copied
		expectedFiles := []string{"merge-queue.md", "worker.md", "reviewer.md"}
		for _, filename := range expectedFiles {
			path := filepath.Join(destDir, filename)
			if _, err := os.Stat(path); os.IsNotExist(err) {
				t.Errorf("Expected file %s does not exist in nested directory", filename)
			}
		}
	})

	t.Run("handles empty destination path", func(t *testing.T) {
		// While empty string is technically valid (current directory),
		// the function should handle it gracefully
		tmpDir, err := os.MkdirTemp("", "templates-test-*")
		if err != nil {
			t.Fatalf("Failed to create temp dir: %v", err)
		}
		defer os.RemoveAll(tmpDir)

		// Change to temp directory
		oldDir, err := os.Getwd()
		if err != nil {
			t.Fatalf("Failed to get working directory: %v", err)
		}
		defer os.Chdir(oldDir)

		if err := os.Chdir(tmpDir); err != nil {
			t.Fatalf("Failed to change directory: %v", err)
		}

		// Use "." as destination
		if err := CopyAgentTemplates("."); err != nil {
			t.Fatalf("CopyAgentTemplates failed with '.' path: %v", err)
		}

		// Verify files were copied to current directory
		expectedFiles := []string{"merge-queue.md", "worker.md", "reviewer.md"}
		for _, filename := range expectedFiles {
			if _, err := os.Stat(filename); os.IsNotExist(err) {
				t.Errorf("Expected file %s does not exist", filename)
			}
		}
	})
}

func TestSyncAgentTemplates_RefreshesStaleFile(t *testing.T) {
	// Part 4.H regression: a per-repo agents/ dir that already exists
	// with stale content used to silently shadow embedded-template
	// edits. SyncAgentTemplates is the embedded-newer-wins refresh
	// that fixes it.
	tmpDir, err := os.MkdirTemp("", "templates-sync-stale-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	staleContent := []byte("STALE CONTENT — should be overwritten\n")
	stalePath := filepath.Join(tmpDir, "browser.md")
	if err := os.WriteFile(stalePath, staleContent, 0644); err != nil {
		t.Fatalf("Failed to seed stale file: %v", err)
	}

	refreshed, err := SyncAgentTemplates(tmpDir)
	if err != nil {
		t.Fatalf("SyncAgentTemplates failed: %v", err)
	}

	// Browser.md should be in the refreshed list (it drifted from embedded).
	foundBrowser := false
	for _, name := range refreshed {
		if name == "browser.md" {
			foundBrowser = true
			break
		}
	}
	if !foundBrowser {
		t.Errorf("Expected browser.md to be refreshed, got: %v", refreshed)
	}

	// Content on disk should now match the embedded template.
	embedded, err := ReadEmbeddedAgentTemplate("browser.md")
	if err != nil {
		t.Fatalf("ReadEmbeddedAgentTemplate failed: %v", err)
	}
	got, err := os.ReadFile(stalePath)
	if err != nil {
		t.Fatalf("Read back stale file failed: %v", err)
	}
	if string(got) != string(embedded) {
		t.Errorf("On-disk content did not match embedded after sync")
	}
}

func TestSyncAgentTemplates_NoOpWhenInSync(t *testing.T) {
	// Idempotency: a freshly-synced dir should not be re-written on
	// the next call (returns empty refreshed list, mtime preserved).
	tmpDir, err := os.MkdirTemp("", "templates-sync-noop-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	if _, err := SyncAgentTemplates(tmpDir); err != nil {
		t.Fatalf("First sync failed: %v", err)
	}

	// Capture mtime of browser.md after the first sync.
	browserPath := filepath.Join(tmpDir, "browser.md")
	info1, err := os.Stat(browserPath)
	if err != nil {
		t.Fatalf("Stat after first sync failed: %v", err)
	}

	// Sleep briefly so any rewrite would produce a distinguishable mtime.
	// (On filesystems with second-granularity mtime, even an unconditional
	// rewrite would change the timestamp on subsequent calls in this test.)
	time.Sleep(10 * time.Millisecond)

	refreshed, err := SyncAgentTemplates(tmpDir)
	if err != nil {
		t.Fatalf("Second sync failed: %v", err)
	}
	if len(refreshed) != 0 {
		t.Errorf("Expected no files refreshed on idempotent sync, got: %v", refreshed)
	}

	info2, err := os.Stat(browserPath)
	if err != nil {
		t.Fatalf("Stat after second sync failed: %v", err)
	}
	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Errorf("mtime changed on no-op sync: before=%v after=%v", info1.ModTime(), info2.ModTime())
	}
}

func TestSyncAgentTemplates_FreshDirCreatesAllFiles(t *testing.T) {
	// First-run behaviour: empty (or non-existent) destination should
	// get the full set of embedded files written, same as CopyAgentTemplates.
	tmpDir, err := os.MkdirTemp("", "templates-sync-fresh-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	destDir := filepath.Join(tmpDir, "agents")
	refreshed, err := SyncAgentTemplates(destDir)
	if err != nil {
		t.Fatalf("SyncAgentTemplates failed: %v", err)
	}
	embeddedList, err := ListAgentTemplates()
	if err != nil {
		t.Fatalf("ListAgentTemplates failed: %v", err)
	}
	if len(refreshed) != len(embeddedList) {
		t.Errorf("Fresh sync should refresh all %d embedded files, got %d: %v", len(embeddedList), len(refreshed), refreshed)
	}
}

func TestReadEmbeddedAgentTemplate(t *testing.T) {
	// Sanity: helper returns bytes for known templates, ErrNotExist
	// for unknown, and tolerates the `.md` suffix being absent.
	content, err := ReadEmbeddedAgentTemplate("browser.md")
	if err != nil {
		t.Fatalf("ReadEmbeddedAgentTemplate(browser.md) failed: %v", err)
	}
	if len(content) == 0 {
		t.Error("Expected non-empty browser.md content")
	}

	contentNoExt, err := ReadEmbeddedAgentTemplate("browser")
	if err != nil {
		t.Fatalf("ReadEmbeddedAgentTemplate(browser) (no .md) failed: %v", err)
	}
	if string(contentNoExt) != string(content) {
		t.Error("Helper should treat 'browser' and 'browser.md' identically")
	}

	if _, err := ReadEmbeddedAgentTemplate("no-such-template-doesnt-exist.md"); err != os.ErrNotExist {
		t.Errorf("Expected ErrNotExist for unknown template, got: %v", err)
	}
}

func TestListAgentTemplatesConsistency(t *testing.T) {
	// List templates
	templates, err := ListAgentTemplates()
	if err != nil {
		t.Fatalf("ListAgentTemplates failed: %v", err)
	}

	// Copy to a temp directory
	tmpDir, err := os.MkdirTemp("", "templates-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	if err := CopyAgentTemplates(tmpDir); err != nil {
		t.Fatalf("CopyAgentTemplates failed: %v", err)
	}

	// Read what was actually copied
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("Failed to read copied directory: %v", err)
	}

	var copiedFiles []string
	for _, entry := range entries {
		if !entry.IsDir() {
			copiedFiles = append(copiedFiles, entry.Name())
		}
	}

	// Lists should match
	if len(templates) != len(copiedFiles) {
		t.Errorf("ListAgentTemplates returned %d files but %d were copied", len(templates), len(copiedFiles))
	}

	templateMap := make(map[string]bool)
	for _, tmpl := range templates {
		templateMap[tmpl] = true
	}

	for _, copied := range copiedFiles {
		if !templateMap[copied] {
			t.Errorf("File %s was copied but not in ListAgentTemplates result", copied)
		}
	}
}
