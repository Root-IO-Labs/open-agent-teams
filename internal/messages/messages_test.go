package messages

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewManager(t *testing.T) {
	tmpDir := t.TempDir()
	m := NewManager(tmpDir)

	if m == nil {
		t.Fatal("NewManager() returned nil")
	}

	if m.messagesRoot != tmpDir {
		t.Errorf("messagesRoot = %q, want %q", m.messagesRoot, tmpDir)
	}
}

func TestSendMessage(t *testing.T) {
	tmpDir := t.TempDir()
	m := NewManager(tmpDir)

	repoName := "test-repo"
	from := "supervisor"
	to := "worker1"
	body := "How's it going?"

	msg, err := m.Send(repoName, from, to, body)
	if err != nil {
		t.Fatalf("Send() failed: %v", err)
	}

	if msg.From != from {
		t.Errorf("From = %q, want %q", msg.From, from)
	}

	if msg.To != to {
		t.Errorf("To = %q, want %q", msg.To, to)
	}

	if msg.Body != body {
		t.Errorf("Body = %q, want %q", msg.Body, body)
	}

	if msg.Status != StatusPending {
		t.Errorf("Status = %q, want %q", msg.Status, StatusPending)
	}

	if msg.ID == "" {
		t.Error("Message ID is empty")
	}

	// Verify file was created
	msgPath := filepath.Join(tmpDir, repoName, to, msg.ID+".json")
	if _, err := os.Stat(msgPath); os.IsNotExist(err) {
		t.Error("Message file not created")
	}
}

func TestListMessages(t *testing.T) {
	tmpDir := t.TempDir()
	m := NewManager(tmpDir)

	repoName := "test-repo"
	agentName := "worker1"

	// Empty list
	messages, err := m.List(repoName, agentName)
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}

	if len(messages) != 0 {
		t.Errorf("List() length = %d, want 0", len(messages))
	}

	// Send some messages
	for i := 0; i < 3; i++ {
		if _, err := m.Send(repoName, "supervisor", agentName, "Message"); err != nil {
			t.Fatalf("Send(%d) failed: %v", i, err)
		}
	}

	messages, err = m.List(repoName, agentName)
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}

	if len(messages) != 3 {
		t.Errorf("List() length = %d, want 3", len(messages))
	}
}

func TestGetMessage(t *testing.T) {
	tmpDir := t.TempDir()
	m := NewManager(tmpDir)

	repoName := "test-repo"
	agentName := "worker1"
	body := "Test message"

	msg, err := m.Send(repoName, "supervisor", agentName, body)
	if err != nil {
		t.Fatalf("Send() failed: %v", err)
	}

	// Get the message
	retrieved, err := m.Get(repoName, agentName, msg.ID)
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}

	if retrieved.ID != msg.ID {
		t.Errorf("ID = %q, want %q", retrieved.ID, msg.ID)
	}

	if retrieved.Body != body {
		t.Errorf("Body = %q, want %q", retrieved.Body, body)
	}
}

func TestUpdateStatus(t *testing.T) {
	tmpDir := t.TempDir()
	m := NewManager(tmpDir)

	repoName := "test-repo"
	agentName := "worker1"

	msg, err := m.Send(repoName, "supervisor", agentName, "Test")
	if err != nil {
		t.Fatalf("Send() failed: %v", err)
	}

	// Update to delivered
	if err := m.UpdateStatus(repoName, agentName, msg.ID, StatusDelivered); err != nil {
		t.Fatalf("UpdateStatus() failed: %v", err)
	}

	// Verify update
	updated, err := m.Get(repoName, agentName, msg.ID)
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}

	if updated.Status != StatusDelivered {
		t.Errorf("Status = %q, want %q", updated.Status, StatusDelivered)
	}

	// Update to read
	if err := m.UpdateStatus(repoName, agentName, msg.ID, StatusRead); err != nil {
		t.Fatalf("UpdateStatus() failed: %v", err)
	}

	updated, err = m.Get(repoName, agentName, msg.ID)
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}

	if updated.Status != StatusRead {
		t.Errorf("Status = %q, want %q", updated.Status, StatusRead)
	}
}

func TestAckMessage(t *testing.T) {
	tmpDir := t.TempDir()
	m := NewManager(tmpDir)

	repoName := "test-repo"
	agentName := "worker1"

	msg, err := m.Send(repoName, "supervisor", agentName, "Test")
	if err != nil {
		t.Fatalf("Send() failed: %v", err)
	}

	// Ack the message
	if err := m.Ack(repoName, agentName, msg.ID); err != nil {
		t.Fatalf("Ack() failed: %v", err)
	}

	// Verify status and acked time
	acked, err := m.Get(repoName, agentName, msg.ID)
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}

	if acked.Status != StatusAcked {
		t.Errorf("Status = %q, want %q", acked.Status, StatusAcked)
	}

	if acked.AckedAt == nil {
		t.Error("AckedAt is nil")
	} else {
		// Check that AckedAt is recent
		if time.Since(*acked.AckedAt) > time.Minute {
			t.Error("AckedAt is too old")
		}
	}
}

func TestDeleteMessage(t *testing.T) {
	tmpDir := t.TempDir()
	m := NewManager(tmpDir)

	repoName := "test-repo"
	agentName := "worker1"

	msg, err := m.Send(repoName, "supervisor", agentName, "Test")
	if err != nil {
		t.Fatalf("Send() failed: %v", err)
	}

	// Delete the message
	if err := m.Delete(repoName, agentName, msg.ID); err != nil {
		t.Fatalf("Delete() failed: %v", err)
	}

	// Verify it's gone
	if _, err := m.Get(repoName, agentName, msg.ID); err == nil {
		t.Error("Get() succeeded after delete")
	}

	// Deleting again should not error
	if err := m.Delete(repoName, agentName, msg.ID); err != nil {
		t.Errorf("Delete() second call failed: %v", err)
	}
}

func TestDeleteAcked(t *testing.T) {
	tmpDir := t.TempDir()
	m := NewManager(tmpDir)

	repoName := "test-repo"
	agentName := "worker1"

	// Send some messages
	var msgIDs []string
	for i := 0; i < 5; i++ {
		msg, err := m.Send(repoName, "supervisor", agentName, "Message")
		if err != nil {
			t.Fatalf("Send(%d) failed: %v", i, err)
		}
		msgIDs = append(msgIDs, msg.ID)
	}

	// Ack some of them
	if err := m.Ack(repoName, agentName, msgIDs[0]); err != nil {
		t.Fatalf("Ack() failed: %v", err)
	}
	if err := m.Ack(repoName, agentName, msgIDs[2]); err != nil {
		t.Fatalf("Ack() failed: %v", err)
	}

	// Delete acked
	count, err := m.DeleteAcked(repoName, agentName)
	if err != nil {
		t.Fatalf("DeleteAcked() failed: %v", err)
	}

	if count != 2 {
		t.Errorf("DeleteAcked() count = %d, want 2", count)
	}

	// Verify only unacked remain
	messages, err := m.List(repoName, agentName)
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}

	if len(messages) != 3 {
		t.Errorf("List() length = %d, want 3", len(messages))
	}

	// Verify the right ones remain
	for _, msg := range messages {
		if msg.Status == StatusAcked {
			t.Errorf("Found acked message after DeleteAcked: %s", msg.ID)
		}
	}
}

func TestListUnread(t *testing.T) {
	tmpDir := t.TempDir()
	m := NewManager(tmpDir)

	repoName := "test-repo"
	agentName := "worker1"

	// Send messages
	var msgIDs []string
	for i := 0; i < 5; i++ {
		msg, err := m.Send(repoName, "supervisor", agentName, "Message")
		if err != nil {
			t.Fatalf("Send(%d) failed: %v", i, err)
		}
		msgIDs = append(msgIDs, msg.ID)
	}

	// Mark some as delivered
	if err := m.UpdateStatus(repoName, agentName, msgIDs[0], StatusDelivered); err != nil {
		t.Fatalf("UpdateStatus() failed: %v", err)
	}

	// Mark some as read
	if err := m.UpdateStatus(repoName, agentName, msgIDs[1], StatusRead); err != nil {
		t.Fatalf("UpdateStatus() failed: %v", err)
	}

	// Mark some as acked
	if err := m.Ack(repoName, agentName, msgIDs[2]); err != nil {
		t.Fatalf("Ack() failed: %v", err)
	}

	// Get unread (pending + delivered)
	unread, err := m.ListUnread(repoName, agentName)
	if err != nil {
		t.Fatalf("ListUnread() failed: %v", err)
	}

	// Should have pending (3 and 4) and delivered (0) = 3 total
	if len(unread) != 3 {
		t.Errorf("ListUnread() length = %d, want 3", len(unread))
	}

	for _, msg := range unread {
		if msg.Status != StatusPending && msg.Status != StatusDelivered {
			t.Errorf("Found non-unread message: %s (status: %s)", msg.ID, msg.Status)
		}
	}
}

func TestCleanupOrphaned(t *testing.T) {
	tmpDir := t.TempDir()
	m := NewManager(tmpDir)

	repoName := "test-repo"

	// Create messages for several agents
	agents := []string{"agent1", "agent2", "agent3"}
	for _, agent := range agents {
		if _, err := m.Send(repoName, "supervisor", agent, "Test"); err != nil {
			t.Fatalf("Send() failed: %v", err)
		}
	}

	// Only agent1 and agent3 are valid now
	validAgents := []string{"agent1", "agent3"}

	count, err := m.CleanupOrphaned(repoName, validAgents)
	if err != nil {
		t.Fatalf("CleanupOrphaned() failed: %v", err)
	}

	if count != 1 {
		t.Errorf("CleanupOrphaned() count = %d, want 1", count)
	}

	// Verify agent2 directory is gone
	agent2Dir := filepath.Join(tmpDir, repoName, "agent2")
	if _, err := os.Stat(agent2Dir); !os.IsNotExist(err) {
		t.Error("Orphaned agent directory still exists")
	}

	// Verify other directories remain
	for _, agent := range validAgents {
		agentDir := filepath.Join(tmpDir, repoName, agent)
		if _, err := os.Stat(agentDir); os.IsNotExist(err) {
			t.Errorf("Valid agent directory removed: %s", agent)
		}
	}
}

func TestErrorHandling(t *testing.T) {
	t.Run("Send fails with invalid permissions", func(t *testing.T) {
		tmpDir := t.TempDir()
		m := NewManager(tmpDir)

		repoName := "test-repo"
		agentName := "worker1"

		// Create agent directory first
		agentDir := filepath.Join(tmpDir, repoName, agentName)
		if err := os.MkdirAll(agentDir, 0755); err != nil {
			t.Fatalf("Failed to create agent dir: %v", err)
		}

		// Make it read-only
		if err := os.Chmod(agentDir, 0444); err != nil {
			t.Fatalf("Failed to chmod: %v", err)
		}
		defer os.Chmod(agentDir, 0755) // Restore for cleanup

		// Send should fail
		_, err := m.Send(repoName, "supervisor", agentName, "Test")
		if err == nil {
			t.Error("Expected Send to fail with read-only directory")
		}
	})

	t.Run("Get fails for non-existent message", func(t *testing.T) {
		tmpDir := t.TempDir()
		m := NewManager(tmpDir)

		_, err := m.Get("repo", "agent", "nonexistent-id")
		if err == nil {
			t.Error("Expected Get to fail for non-existent message")
		}
	})

	t.Run("UpdateStatus fails for non-existent message", func(t *testing.T) {
		tmpDir := t.TempDir()
		m := NewManager(tmpDir)

		err := m.UpdateStatus("repo", "agent", "nonexistent-id", StatusRead)
		if err == nil {
			t.Error("Expected UpdateStatus to fail for non-existent message")
		}
	})

	t.Run("List handles non-existent directory", func(t *testing.T) {
		tmpDir := t.TempDir()
		m := NewManager(tmpDir)

		messages, err := m.List("nonexistent-repo", "nonexistent-agent")
		if err != nil {
			t.Fatalf("List should not error for non-existent directory: %v", err)
		}
		if len(messages) != 0 {
			t.Errorf("Expected empty list, got %d messages", len(messages))
		}
	})

	t.Run("ListUnread handles non-existent directory", func(t *testing.T) {
		tmpDir := t.TempDir()
		m := NewManager(tmpDir)

		messages, err := m.ListUnread("nonexistent-repo", "nonexistent-agent")
		if err != nil {
			t.Fatalf("ListUnread should not error for non-existent directory: %v", err)
		}
		if len(messages) != 0 {
			t.Errorf("Expected empty list, got %d messages", len(messages))
		}
	})

	t.Run("CleanupOrphaned handles non-existent repo", func(t *testing.T) {
		tmpDir := t.TempDir()
		m := NewManager(tmpDir)

		count, err := m.CleanupOrphaned("nonexistent-repo", []string{"agent1"})
		if err != nil {
			t.Fatalf("CleanupOrphaned should not error for non-existent repo: %v", err)
		}
		if count != 0 {
			t.Errorf("Expected 0 cleaned up, got %d", count)
		}
	})

	t.Run("read handles corrupted JSON", func(t *testing.T) {
		tmpDir := t.TempDir()
		m := NewManager(tmpDir)

		repoName := "test-repo"
		agentName := "worker1"

		// Create agent directory
		agentDir := filepath.Join(tmpDir, repoName, agentName)
		if err := os.MkdirAll(agentDir, 0755); err != nil {
			t.Fatalf("Failed to create agent dir: %v", err)
		}

		// Write invalid JSON
		badJSON := filepath.Join(agentDir, "bad.json")
		if err := os.WriteFile(badJSON, []byte("{invalid json"), 0644); err != nil {
			t.Fatalf("Failed to write bad JSON: %v", err)
		}

		// List should skip the corrupted file
		messages, err := m.List(repoName, agentName)
		if err != nil {
			t.Fatalf("List should handle corrupted JSON gracefully: %v", err)
		}
		// Should not include the corrupted message
		if len(messages) != 0 {
			t.Errorf("Expected 0 valid messages, got %d", len(messages))
		}
	})

	t.Run("Delete is idempotent", func(t *testing.T) {
		tmpDir := t.TempDir()
		m := NewManager(tmpDir)

		repoName := "test-repo"
		agentName := "worker1"

		msg, err := m.Send(repoName, "supervisor", agentName, "Test")
		if err != nil {
			t.Fatalf("Send failed: %v", err)
		}

		// Delete once
		if err := m.Delete(repoName, agentName, msg.ID); err != nil {
			t.Fatalf("First delete failed: %v", err)
		}

		// Delete again - should not error
		if err := m.Delete(repoName, agentName, msg.ID); err != nil {
			t.Errorf("Second delete should not error: %v", err)
		}
	})

	t.Run("CleanupOrphaned ignores files", func(t *testing.T) {
		tmpDir := t.TempDir()
		m := NewManager(tmpDir)

		repoName := "test-repo"

		// Create a file in the repo directory (not a directory)
		repoDir := filepath.Join(tmpDir, repoName)
		if err := os.MkdirAll(repoDir, 0755); err != nil {
			t.Fatalf("Failed to create repo dir: %v", err)
		}

		filePath := filepath.Join(repoDir, "somefile.txt")
		if err := os.WriteFile(filePath, []byte("content"), 0644); err != nil {
			t.Fatalf("Failed to create file: %v", err)
		}

		// CleanupOrphaned should not try to remove the file
		count, err := m.CleanupOrphaned(repoName, []string{})
		if err != nil {
			t.Fatalf("CleanupOrphaned failed: %v", err)
		}

		// File should still exist
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			t.Error("File was removed by CleanupOrphaned")
		}

		if count != 0 {
			t.Errorf("Expected 0 cleaned up, got %d", count)
		}
	})
}
