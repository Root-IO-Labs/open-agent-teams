package daemon

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSidecarSocketPath_EnabledByDefault(t *testing.T) {
	// Opt-out semantics: fresh installs (env var unset) get the
	// sidecar. If this test fails, we accidentally reverted to the
	// opt-in rollout behavior and new users won't see the fix.
	_ = os.Unsetenv("OAT_USE_SIDECAR")
	if got := sidecarSocketPath("r", "a"); got == "" {
		t.Errorf("expected non-empty path when flag unset (default-on), got empty")
	}
}

func TestSidecarSocketPath_DisabledOnlyForExplicitZero(t *testing.T) {
	// The single opt-out value is literal "0". Strict match avoids
	// "false"/"no"/"off" ambiguity that causes real ops incidents.
	t.Setenv("OAT_USE_SIDECAR", "0")
	if got := sidecarSocketPath("r", "a"); got != "" {
		t.Errorf("expected empty path when flag = 0, got %q", got)
	}
}

func TestSidecarSocketPath_EnabledForArbitraryNonZero(t *testing.T) {
	// Any value other than literal "0" enables the sidecar. An
	// operator who types "true" or "yes" gets the sidecar (intent
	// preserved), not silent failure.
	for _, v := range []string{"1", "true", "yes", "on", "anything"} {
		t.Setenv("OAT_USE_SIDECAR", v)
		if got := sidecarSocketPath("r", "a"); got == "" {
			t.Errorf("expected non-empty path for %q, got empty", v)
		}
	}
}

func TestSidecarSocketPath_EnabledShape(t *testing.T) {
	t.Setenv("OAT_USE_SIDECAR", "1")
	path := sidecarSocketPath("myrepo", "worker-1")
	if !strings.HasPrefix(path, "/tmp/oat-sdcr-") {
		t.Errorf("unexpected prefix: %q", path)
	}
	if !strings.HasSuffix(path, ".sock") {
		t.Errorf("unexpected suffix: %q", path)
	}
	// macOS sun_path is 104 bytes; leave ample headroom for test temp
	// dirs that might use longer paths in CI.
	if len(path) >= 104 {
		t.Errorf("path too long: %d bytes", len(path))
	}
}

func TestSidecarSocketPath_StableForSameInputs(t *testing.T) {
	// Determinism matters: the daemon must compute the same path before
	// StartAgent that StopAgent later uses to clean up. If the hash was
	// non-deterministic we'd leak socket files.
	t.Setenv("OAT_USE_SIDECAR", "1")
	p1 := sidecarSocketPath("r", "a")
	p2 := sidecarSocketPath("r", "a")
	if p1 != p2 {
		t.Errorf("non-deterministic: %q != %q", p1, p2)
	}
}

func TestSidecarSocketPath_DistinctForDifferentAgents(t *testing.T) {
	t.Setenv("OAT_USE_SIDECAR", "1")
	paths := map[string]string{
		"r1/a1": sidecarSocketPath("r1", "a1"),
		"r1/a2": sidecarSocketPath("r1", "a2"),
		"r2/a1": sidecarSocketPath("r2", "a1"),
	}
	seen := map[string]string{}
	for id, path := range paths {
		if other, exists := seen[path]; exists {
			t.Errorf("collision: %s and %s both → %q", id, other, path)
		}
		seen[path] = id
	}
}

func TestSidecarSocketPath_PickedUpAfterEnvChange(t *testing.T) {
	// A daemon may set OAT_USE_SIDECAR at startup; we read the env var
	// lazily rather than caching. Verify flipping between enabled
	// (unset / "1") and disabled ("0") changes the return without
	// restarting the process.
	t.Setenv("OAT_USE_SIDECAR", "0")
	if sidecarSocketPath("r", "a") != "" {
		t.Fatal("expected empty when flag = 0")
	}
	_ = os.Unsetenv("OAT_USE_SIDECAR")
	if sidecarSocketPath("r", "a") == "" {
		t.Fatal("expected non-empty after unset (default-on)")
	}
}

// TestIsLiveSocket_DetectsLiveAndDead verifies the distinction between a
// socket with a listener accepting and a stale socket file with no
// listener. Correctness here is the basis for the daemon-startup cleanup
// — if isLiveSocket returned false positives, we'd remove sockets
// belonging to a running daemon.
func TestIsLiveSocket_DetectsLiveAndDead(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "live-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	// Dead socket: create the file but don't bind a listener.
	dead := filepath.Join(dir, "dead")
	f, err := os.Create(dead)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if isLiveSocket(dead) {
		t.Error("plain file reported as live socket")
	}

	// Live socket: bind a real listener.
	live := filepath.Join(dir, "live")
	ln, err := net.Listen("unix", live)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if !isLiveSocket(live) {
		t.Error("bound listener reported as dead")
	}

	// Path doesn't exist at all → dead.
	if isLiveSocket(filepath.Join(dir, "nope")) {
		t.Error("non-existent path reported as live")
	}
}

// TestCleanStaleSidecarSockets_RemovesOnlyDead verifies the cleanup
// respects live listeners — it must not rm a socket belonging to a
// concurrently-running daemon (unlikely given the PID lock, but worth
// asserting).
func TestCleanStaleSidecarSockets_RemovesOnlyDead(t *testing.T) {
	// Plant a few stale socket files with the expected prefix.
	stale := []string{
		"/tmp/oat-sdcr-test1.sock",
		"/tmp/oat-sdcr-test2.sock",
	}
	for _, p := range stale {
		f, _ := os.Create(p)
		if f != nil {
			f.Close()
		}
		defer os.Remove(p)
	}

	// Plant one LIVE listener with the expected prefix.
	liveSock := "/tmp/oat-sdcr-testlive.sock"
	_ = os.Remove(liveSock)
	ln, err := net.Listen("unix", liveSock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	defer os.Remove(liveSock)

	removed := cleanStaleSidecarSockets()
	if removed < 2 {
		t.Errorf("removed %d stale sockets, want at least 2", removed)
	}

	// Live socket must still exist.
	if _, err := os.Stat(liveSock); err != nil {
		t.Errorf("live socket was wrongly removed: %v", err)
	}
	// Stale sockets must be gone.
	for _, p := range stale {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("stale socket %s not removed", p)
		}
	}
}
