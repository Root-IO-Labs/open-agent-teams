package sidecar

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// findRepoRoot walks up from this test file until it finds go.mod. Using
// runtime.Caller avoids depending on process CWD, which matters because
// `go test` sets CWD to the package directory, not the repo root.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root (no go.mod in ancestors)")
		}
		dir = parent
	}
}

// findPython locates a Python interpreter capable of running the sidecar
// client. Checks (in order): OAT_SIDECAR_TEST_PYTHON env var, the worktree's
// uv venv, python3 on PATH. The sidecar client is stdlib-only so any
// modern Python 3.11+ works — no venv required. Returns "" if none is
// available; the test skips rather than failing so CI without Python
// doesn't break the build.
func findPython(t *testing.T, repoRoot string) string {
	t.Helper()
	if p := os.Getenv("OAT_SIDECAR_TEST_PYTHON"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	venv := filepath.Join(repoRoot, "agent-runtime", "libs", "cli", ".venv", "bin", "python")
	if _, err := os.Stat(venv); err == nil {
		return venv
	}
	if p, err := exec.LookPath("python3"); err == nil {
		return p
	}
	if p, err := exec.LookPath("python"); err == nil {
		return p
	}
	return ""
}

// runPythonClient spawns the sidecar client module against the given
// socket path, asking it to emit `count` events in `mode`. `queueSize`
// overrides the client's default bounded queue — bump it for stress tests
// that burst events faster than LLM cadence. Returns stderr for
// diagnostics (the client prints a stats line there).
func runPythonClient(
	t *testing.T, python, repoRoot, socketPath string,
	count int, mode string, queueSize int,
) (stderr string, err error) {
	t.Helper()
	cliDir := filepath.Join(repoRoot, "agent-runtime", "libs", "cli")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	args := []string{
		"-m", "deepagents_cli.sidecar_client",
		socketPath, fmtInt(count), mode,
	}
	if queueSize > 0 {
		args = append(args, fmtInt(queueSize))
	}
	cmd := exec.CommandContext(ctx, python, args...)
	cmd.Dir = cliDir
	cmd.Env = append(os.Environ(), "PYTHONPATH="+cliDir)

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	err = cmd.Run()
	return stderrBuf.String(), err
}

func fmtInt(n int) string {
	// Tiny local formatter to avoid importing strconv just for test code
	// style consistency.
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// TestIntegration_PythonEmitsGoReceives_Mixed spawns a real Python process
// that uses SidecarClient to emit events of every kind; the Go server
// must parse them all with no gaps, no parse errors, and in order.
//
// This is the strongest cross-language guarantee we can provide for
// Day 2: the schema, the socket framing, the connect logic, the drain
// behavior, and the Go parser all work together on a real wire.
func TestIntegration_PythonEmitsGoReceives_Mixed(t *testing.T) {
	repoRoot := findRepoRoot(t)
	python := findPython(t, repoRoot)
	if python == "" {
		t.Skip("no Python interpreter available (set OAT_SIDECAR_TEST_PYTHON or run `uv sync` in agent-runtime/libs/cli)")
	}

	const count = 80 // 10x each of 8 kinds

	var (
		mu          sync.Mutex
		received    []Event
		parseErrors []string
		gaps        int
	)

	s := NewServer(sockPath(t))
	s.OnEvent = func(ev Event) {
		mu.Lock()
		received = append(received, ev)
		mu.Unlock()
	}
	s.OnGap = func(expected, got uint64) {
		mu.Lock()
		gaps++
		mu.Unlock()
	}
	s.OnParseError = func(line []byte, err error) {
		mu.Lock()
		parseErrors = append(parseErrors, err.Error())
		mu.Unlock()
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("server start: %v", err)
	}
	defer s.Close()

	stderr, err := runPythonClient(t, python, repoRoot, s.Path(), count, "mixed", 0)
	if err != nil {
		t.Logf("python stderr: %s", stderr)
		t.Fatalf("python client: %v", err)
	}

	// Give the server a final moment to drain any in-flight bytes that
	// arrived just as the client closed the socket. Without this, races
	// on macOS occasionally leave the last event in-flight.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := len(received) == count
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(received) != count {
		t.Errorf("received %d events, want %d\npython stderr: %s",
			len(received), count, stderr)
	}
	if gaps != 0 {
		t.Errorf("gaps=%d, want 0 (python emits monotonic seq)", gaps)
	}
	if len(parseErrors) != 0 {
		t.Errorf("parse errors: %v", parseErrors)
	}

	// Every kind should appear at least once.
	seen := map[string]int{}
	for _, ev := range received {
		seen[ev.Kind]++
	}
	wantKinds := []string{
		KindTurnStart, KindTurnEnd, KindAssistantDelta,
		KindAssistantMessage, KindToolCall, KindToolResult, KindInterrupt,
		KindTokenUsage,
	}
	for _, k := range wantKinds {
		if seen[k] == 0 {
			t.Errorf("kind %q not seen (counts: %+v)", k, seen)
		}
	}

	// Spot-check typed parsing on kinds that carry structure. The Python
	// client emits assistant_message with Usage and token_usage with the
	// sentinel-compatible payload — verify Go sees both cleanly.
	var sawMsgUsage, sawTokenUsage bool
	for _, ev := range received {
		switch ev.Kind {
		case KindAssistantMessage:
			if sawMsgUsage {
				continue
			}
			d, err := ev.AsAssistantMessage()
			if err != nil {
				t.Errorf("AsAssistantMessage: %v", err)
				continue
			}
			if d.Usage == nil || d.Usage.InputTokens == 0 {
				t.Errorf("AssistantMessage Usage not round-tripped: %+v", d.Usage)
			}
			sawMsgUsage = true
		case KindTokenUsage:
			if sawTokenUsage {
				continue
			}
			d, err := ev.AsTokenUsage()
			if err != nil {
				t.Errorf("AsTokenUsage: %v", err)
				continue
			}
			// Sentinel parity: the fields the daemon's existing
			// handleTokenUsageEvent would consume.
			if d.CumulativeInput == 0 && d.DeltaInput == 0 {
				t.Errorf("token_usage payload empty: %+v", d)
			}
			sawTokenUsage = true
		}
	}
	if !sawMsgUsage {
		t.Error("no AssistantMessage spot-check succeeded")
	}
	if !sawTokenUsage {
		t.Error("no TokenUsage spot-check succeeded")
	}
}

// TestIntegration_PythonEmitsGoReceives_HighThroughput hammers the socket
// with a stream of assistant_delta events to verify no events are lost,
// reordered, or corrupted under sustained load. This is the closest
// synthetic proxy for "a long streaming assistant response".
func TestIntegration_PythonEmitsGoReceives_HighThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping high-throughput integration test in -short mode")
	}
	repoRoot := findRepoRoot(t)
	python := findPython(t, repoRoot)
	if python == "" {
		t.Skip("no Python interpreter available")
	}

	const count = 5000

	var (
		mu       sync.Mutex
		lastSeq  uint64
		received int
		gaps     int
		parseErr int
	)

	s := NewServer(sockPath(t))
	s.OnEvent = func(ev Event) {
		mu.Lock()
		received++
		lastSeq = ev.Seq
		mu.Unlock()
	}
	s.OnGap = func(uint64, uint64) {
		mu.Lock()
		gaps++
		mu.Unlock()
	}
	s.OnParseError = func([]byte, error) {
		mu.Lock()
		parseErr++
		mu.Unlock()
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("server start: %v", err)
	}
	defer s.Close()

	// Use a queue larger than the event count — the throughput test
	// synthesizes a burst faster than the socket can drain (real LLM
	// cadence is much slower, so production default of 1000 is fine).
	// A larger queue makes this test about wire + parse correctness
	// under sustained load, not about the queue drop policy (which is
	// covered by unit tests).
	stderr, err := runPythonClient(t, python, repoRoot, s.Path(), count, "deltas", count*2)
	if err != nil {
		t.Logf("python stderr: %s", stderr)
		t.Fatalf("python client: %v", err)
	}

	// Wait for all events to land.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := received == count
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()

	if received != count {
		t.Errorf("received %d, want %d (last seq %d)\nstderr: %s",
			received, count, lastSeq, stderr)
	}
	if gaps != 0 {
		t.Errorf("gaps under load: %d", gaps)
	}
	if parseErr != 0 {
		t.Errorf("parse errors under load: %d", parseErr)
	}
}
