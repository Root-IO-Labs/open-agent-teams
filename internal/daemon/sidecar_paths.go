package daemon

import (
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// sidecarSocketPath returns the per-agent Unix-domain socket path used
// by the astream sidecar, or "" when the sidecar is explicitly disabled.
//
// The path must fit macOS's 104-byte sun_path limit. We use a short
// 8-hex-char hash of "repo/agent" under /tmp rather than the agent's
// log directory — /var/folders/... paths on macOS are already ~50
// bytes before we append anything, leaving almost no headroom.
//
// Layout: /tmp/oat-sdcr-<hash8>.sock
//
//	14 + 8 + 5 = 27 bytes. Plenty of margin under the 104-byte limit.
//
// Feature gating is OPT-OUT: sidecar is on by default for fresh
// installs and any deployment that hasn't explicitly set the env var.
// Operators who want the legacy PTY-scraping chat path can set
// OAT_USE_SIDECAR=0 before starting the daemon. Any other value (unset,
// "1", "true", etc.) enables the sidecar.
//
// Rationale for default-on: the sidecar fixes the ghosting bug by
// construction, provides lossless token accounting, and is provider-
// agnostic. Opt-in by env var was the right move during rollout; now
// that the pipeline is verified live, fresh installs should get the
// correctness benefits without having to know the flag exists.
func sidecarSocketPath(repoName, agentName string) string {
	if os.Getenv("OAT_USE_SIDECAR") == "0" {
		return ""
	}
	sum := sha256.Sum256([]byte(repoName + "/" + agentName))
	return filepath.Join("/tmp", fmt.Sprintf("oat-sdcr-%x.sock", sum[:4]))
}

// cleanStaleSidecarSockets removes /tmp/oat-sdcr-*.sock files that no
// longer have a live listener. Called at daemon startup so each run
// begins with a clean slate.
//
// Why this is safe: the daemon is a per-user singleton (guaranteed by
// the PID file claim at Start). If another daemon was alive and bound
// to one of these sockets, we wouldn't be here — PID claim would have
// failed and we'd have exited. By the time we reach this point, any
// listener bound to these files belonged to a dead process.
//
// "No live listener" detection: try a short-timeout Dial on each
// socket. A live listener immediately accepts (we close the conn
// without sending). A dead listener gives ECONNREFUSED or a timeout —
// in either case we remove the file.
//
// Returns the number of stale sockets removed. Errors are swallowed
// (logged by the caller via the return count); this path must not
// prevent daemon startup.
func cleanStaleSidecarSockets() int {
	matches, err := filepath.Glob("/tmp/oat-sdcr-*.sock")
	if err != nil {
		return 0
	}
	removed := 0
	for _, path := range matches {
		if isLiveSocket(path) {
			continue
		}
		if err := os.Remove(path); err == nil {
			removed++
		}
	}
	return removed
}

// isLiveSocket returns true if a process is currently accepting on the
// given Unix socket path. 200ms timeout — long enough to tolerate a
// momentarily-loaded daemon, short enough that a batch cleanup of
// dozens of stale sockets finishes in under a second.
func isLiveSocket(path string) bool {
	conn, err := net.DialTimeout("unix", path, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
