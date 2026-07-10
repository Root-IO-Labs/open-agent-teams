package daemon

import (
	"fmt"
	"os"
	"os/exec"
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
		return false, 0, nil //nolint:nilerr // FindProcess failure -> treat as "not running"
	}

	err = process.Signal(syscall.Signal(0))
	if err != nil {
		// Process doesn't exist or we don't have permission
		return false, 0, nil //nolint:nilerr // Signal failure -> treat as "not running"
	}

	// Signal 0 succeeded, but a bare-PID check can false-positive after PID
	// reuse: the OS may have handed our old daemon's PID to an unrelated
	// process, which would make CheckAndClaim wrongly refuse a fresh `oat
	// daemon start` (the "won't restart after a kill" churn John hit). Only
	// downgrade to "stale" when we can POSITIVELY confirm the live PID is NOT
	// an oat daemon; if we can't tell (ps unavailable, ambiguous), keep the
	// conservative "running" answer so we never race two real daemons.
	if pidReused := processIsDefinitelyNotDaemon(pid); pidReused {
		return false, 0, nil
	}

	return true, pid, nil
}

// processIsDefinitelyNotDaemon returns true only when it can positively confirm
// that pid belongs to a process that is NOT an oat daemon (PID reuse). It is
// deliberately conservative: any uncertainty (ps missing/errors, empty output)
// returns false so the caller keeps treating the PID as a live daemon rather
// than risk spawning a duplicate. Matches on the daemon's own command shape
// (`oat daemon _run` / `oat daemon start`).
func processIsDefinitelyNotDaemon(pid int) bool {
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false // can't tell -> assume it IS the daemon (safe)
	}
	cmdline := strings.ToLower(strings.TrimSpace(string(out)))
	if cmdline == "" {
		return false // no info -> assume it IS the daemon (safe)
	}
	// A real oat daemon's argv contains "oat" and "daemon" (`oat daemon _run`).
	// Match loosely (either token) rather than both: this is a POSITIVE-exclude
	// check, so we only declare "stale" when NEITHER token appears — i.e. the
	// reused PID clearly belongs to some unrelated process (bash, node, Chrome,
	// postgres, …). A reused PID whose argv coincidentally contains one of these
	// tokens stays a (harmless) false-"running", never a duplicate-daemon race.
	//
	// Go test binaries are also treated as "could be the daemon" (return false):
	// tests across packages plant `os.Getpid()` as a fake live-daemon PID to
	// exercise the "daemon already up" path (e.g. internal/cli assistant Stop/
	// Remove). Those binaries are named per-package — `daemon.test`, `cli.test`,
	// … — so matching on the daemon tokens alone only rescued `daemon.test` by
	// luck and left `cli.test` misclassified as stale (it spuriously tried to
	// spawn a real daemon). Recognising the `.test` suffix generalises the
	// accommodation to every package's test binary. Production is unaffected: the
	// real daemon is never a `.test` binary, and the failure direction here is the
	// conservative "assume running", so at worst a genuinely-reused `.test` PID
	// stays a harmless false-"running".
	looksLikeDaemon := strings.Contains(cmdline, "oat") ||
		strings.Contains(cmdline, "daemon") ||
		strings.Contains(cmdline, ".test")
	return !looksLikeDaemon
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
