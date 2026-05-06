package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPIDFileWriteRead(t *testing.T) {
	tmpDir := t.TempDir()
	pidPath := filepath.Join(tmpDir, "test.pid")

	pf := NewPIDFile(pidPath)

	// Write PID
	if err := pf.Write(); err != nil {
		t.Fatalf("Write() failed: %v", err)
	}

	// Read PID
	pid, err := pf.Read()
	if err != nil {
		t.Fatalf("Read() failed: %v", err)
	}

	if pid != os.Getpid() {
		t.Errorf("Read() = %d, want %d", pid, os.Getpid())
	}
}

func TestPIDFileReadNonExistent(t *testing.T) {
	tmpDir := t.TempDir()
	pidPath := filepath.Join(tmpDir, "nonexistent.pid")

	pf := NewPIDFile(pidPath)

	pid, err := pf.Read()
	if err != nil {
		t.Fatalf("Read() failed: %v", err)
	}

	if pid != 0 {
		t.Errorf("Read() = %d, want 0 for nonexistent file", pid)
	}
}

func TestPIDFileRemove(t *testing.T) {
	tmpDir := t.TempDir()
	pidPath := filepath.Join(tmpDir, "test.pid")

	pf := NewPIDFile(pidPath)

	// Write PID
	if err := pf.Write(); err != nil {
		t.Fatalf("Write() failed: %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(pidPath); os.IsNotExist(err) {
		t.Fatal("PID file not created")
	}

	// Remove PID file
	if err := pf.Remove(); err != nil {
		t.Fatalf("Remove() failed: %v", err)
	}

	// Verify file is gone
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("PID file still exists after Remove()")
	}

	// Removing again should not error
	if err := pf.Remove(); err != nil {
		t.Errorf("Remove() second call failed: %v", err)
	}
}

func TestPIDFileIsRunning(t *testing.T) {
	tmpDir := t.TempDir()
	pidPath := filepath.Join(tmpDir, "test.pid")

	pf := NewPIDFile(pidPath)

	// No PID file
	running, pid, err := pf.IsRunning()
	if err != nil {
		t.Fatalf("IsRunning() failed: %v", err)
	}
	if running {
		t.Error("IsRunning() = true for nonexistent PID file")
	}
	if pid != 0 {
		t.Errorf("PID = %d, want 0", pid)
	}

	// Write current process PID
	if err := pf.Write(); err != nil {
		t.Fatalf("Write() failed: %v", err)
	}

	// Should detect current process as running
	running, pid, err = pf.IsRunning()
	if err != nil {
		t.Fatalf("IsRunning() failed: %v", err)
	}
	if !running {
		t.Error("IsRunning() = false for current process")
	}
	if pid != os.Getpid() {
		t.Errorf("PID = %d, want %d", pid, os.Getpid())
	}

	// Write a PID that doesn't exist (use very high number)
	if err := os.WriteFile(pidPath, []byte("999999\n"), 0644); err != nil {
		t.Fatalf("WriteFile() failed: %v", err)
	}

	running, _, err = pf.IsRunning()
	if err != nil {
		t.Fatalf("IsRunning() failed: %v", err)
	}
	if running {
		t.Error("IsRunning() = true for non-existent process")
	}
}

func TestPIDFileCheckAndClaim(t *testing.T) {
	tmpDir := t.TempDir()
	pidPath := filepath.Join(tmpDir, "test.pid")

	pf := NewPIDFile(pidPath)

	// Should succeed when no PID file exists
	if err := pf.CheckAndClaim(); err != nil {
		t.Fatalf("CheckAndClaim() failed: %v", err)
	}

	// Verify PID was written
	pid, err := pf.Read()
	if err != nil {
		t.Fatalf("Read() failed: %v", err)
	}
	if pid != os.Getpid() {
		t.Errorf("PID = %d, want %d", pid, os.Getpid())
	}

	// Should fail when process is running
	if err := pf.CheckAndClaim(); err == nil {
		t.Error("CheckAndClaim() succeeded when process already running")
	}

	// Write stale PID and verify we can claim it
	if err := os.WriteFile(pidPath, []byte("999999\n"), 0644); err != nil {
		t.Fatalf("WriteFile() failed: %v", err)
	}

	if err := pf.CheckAndClaim(); err != nil {
		t.Errorf("CheckAndClaim() failed for stale PID: %v", err)
	}

	// Verify our PID was written
	pid, err = pf.Read()
	if err != nil {
		t.Fatalf("Read() failed: %v", err)
	}
	if pid != os.Getpid() {
		t.Errorf("PID = %d, want %d after claiming stale", pid, os.Getpid())
	}
}
