package daemon

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// PIDFile manages the daemon PID file
type PIDFile struct {
	path string
}

// NewPIDFile creates a new PIDFile manager
func NewPIDFile(path string) *PIDFile {
	return &PIDFile{path: path}
}

// Write writes the current process PID to the file
func (p *PIDFile) Write() error {
	pid := os.Getpid()
	return os.WriteFile(p.path, []byte(fmt.Sprintf("%d\n", pid)), 0644)
}

// Read reads the PID from the file
func (p *PIDFile) Read() (int, error) {
	data, err := os.ReadFile(p.path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	pidStr := strings.TrimSpace(string(data))
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return 0, fmt.Errorf("invalid PID in file: %w", err)
	}

	return pid, nil
}

// Remove removes the PID file
func (p *PIDFile) Remove() error {
	if err := os.Remove(p.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// IsRunning checks if the daemon is running by checking the PID file
// and verifying the process is alive
func (p *PIDFile) IsRunning() (bool, int, error) {
	pid, err := p.Read()
	if err != nil {
		return false, 0, err
	}

	if pid == 0 {
		return false, 0, nil
	}

	// Check if process exists by sending signal 0
	process, err := os.FindProcess(pid)
	if err != nil {
		return false, 0, nil
	}

	err = process.Signal(syscall.Signal(0))
	if err != nil {
		// Process doesn't exist or we don't have permission
		return false, 0, nil
	}

	return true, pid, nil
}

// CheckAndClaim checks if another daemon is running and claims the PID file
// Returns error if another daemon is already running
func (p *PIDFile) CheckAndClaim() error {
	running, pid, err := p.IsRunning()
	if err != nil {
		return fmt.Errorf("failed to check daemon status: %w", err)
	}

	if running {
		return fmt.Errorf("daemon already running (PID: %d)", pid)
	}

	// Remove stale PID file if exists
	if err := p.Remove(); err != nil {
		return fmt.Errorf("failed to remove stale PID file: %w", err)
	}

	// Write our PID
	if err := p.Write(); err != nil {
		return fmt.Errorf("failed to write PID file: %w", err)
	}

	return nil
}
