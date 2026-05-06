package backend

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/pkg/sidecar"
)

// shortSockDir returns a /tmp-rooted temp dir short enough for macOS's
// 104-byte sun_path limit. The Go stdlib's t.TempDir() under /var/folders/
// typically blows past that.
func shortSockDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "scbr-")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// TestAppendOatTokensSentinel_WritesExpectedShape verifies the bridge
// writes the exact JSON shape that OutputWatcher already parses for the
// stdout [OAT_TOKENS] path — otherwise the daemon's handleTokenUsageEvent
// would reject it and token accounting silently stops.
func TestAppendOatTokensSentinel_WritesExpectedShape(t *testing.T) {
	dir := shortSockDir(t)
	logPath := filepath.Join(dir, "agent.log")

	data := sidecar.TokenUsageData{
		DeltaInput:       10,
		DeltaOutput:      5,
		CumulativeInput:  100,
		CumulativeOutput: 50,
		CacheRead:        30,
		CacheCreation:    7,
	}
	appendOatTokensSentinel(logPath, data)

	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	line := strings.TrimRight(string(content), "\n")
	if !strings.HasPrefix(line, "[OAT_TOKENS] ") {
		t.Fatalf("missing sentinel prefix: %q", line)
	}
	payload := strings.TrimPrefix(line, "[OAT_TOKENS] ")

	var parsed map[string]any
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		t.Fatalf("parse payload: %v\nline=%q", err, line)
	}
	// Keys must match what handleTokenUsageEvent unmarshals.
	for key, want := range map[string]float64{
		"delta_input":       10,
		"delta_output":      5,
		"cumulative_input":  100,
		"cumulative_output": 50,
		"cache_read":        30,
		"cache_creation":    7,
	} {
		got, ok := parsed[key]
		if !ok {
			t.Errorf("missing key %q", key)
			continue
		}
		if got.(float64) != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
}

// TestAppendOatTokensSentinel_OmitsCacheWhenZero matches Python's
// conditional-emit: cache_read/cache_creation only appear when non-zero.
// Daemon-side cache clamp is robust, but wire-byte parity lets the
// dedup test suite compare outputs byte-for-byte in future work.
func TestAppendOatTokensSentinel_OmitsCacheWhenZero(t *testing.T) {
	dir := shortSockDir(t)
	logPath := filepath.Join(dir, "agent.log")

	data := sidecar.TokenUsageData{
		DeltaInput: 1, DeltaOutput: 1,
		CumulativeInput: 10, CumulativeOutput: 5,
		// CacheRead and CacheCreation deliberately 0.
	}
	appendOatTokensSentinel(logPath, data)

	content, _ := os.ReadFile(logPath)
	s := string(content)
	if strings.Contains(s, "cache_read") || strings.Contains(s, "cache_creation") {
		t.Errorf("cache fields must be omitted when 0:\n%s", s)
	}
}

// TestAppendOatTokensSentinel_EmptyLogPathIsNoOp documents the behavior
// when no log file is configured — bridge should be silent, not panic.
func TestAppendOatTokensSentinel_EmptyLogPathIsNoOp(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panicked: %v", r)
		}
	}()
	appendOatTokensSentinel("", sidecar.TokenUsageData{})
}

// TestNewSidecarServerForAgent_ForwardsTokenUsage is the full bridge path
// exercised end-to-end at the bridge level (not yet through StartAgent):
// the server receives a token_usage event on a real socket, the OnEvent
// callback fires, the log file gets a [OAT_TOKENS] line. This is the
// tightest possible bridge test; integration into StartAgent is tested
// separately.
func TestNewSidecarServerForAgent_ForwardsTokenUsage(t *testing.T) {
	dir := shortSockDir(t)
	socketPath := filepath.Join(dir, "s")
	logPath := filepath.Join(dir, "agent.log")

	srv := newSidecarServerForAgent(socketPath, logPath, nil)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Close()

	// Act like the Python sidecar_client: connect and write one
	// newline-delimited token_usage envelope.
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	ev := sidecar.Event{
		V: 1, Seq: 1, TS: 1, Kind: sidecar.KindTokenUsage,
		TurnID: "t",
		Data: json.RawMessage(
			`{"delta_input":10,"delta_output":5,` +
				`"cumulative_input":100,"cumulative_output":50}`),
	}
	b, _ := json.Marshal(ev)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Wait for the callback to finish writing to the log.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(logPath); err == nil && info.Size() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	content, err := os.ReadFile(logPath)
	if err != nil || len(content) == 0 {
		t.Fatalf("log empty after event: err=%v bytes=%d", err, len(content))
	}
	if !strings.Contains(string(content), "[OAT_TOKENS]") {
		t.Errorf("log missing [OAT_TOKENS]:\n%s", content)
	}
	if !strings.Contains(string(content), `"cumulative_input":100`) {
		t.Errorf("log missing cumulative_input=100:\n%s", content)
	}
}

// TestNewSidecarServerForAgent_IgnoresNonTokenKinds ensures a Day 4-
// style assistant_message doesn't accidentally land in the log — Day 3b
// scope is tokens only, and leaking structured chat events into the log
// would confuse the existing OutputWatcher parsers.
func TestNewSidecarServerForAgent_IgnoresNonTokenKinds(t *testing.T) {
	dir := shortSockDir(t)
	socketPath := filepath.Join(dir, "s")
	logPath := filepath.Join(dir, "agent.log")

	srv := newSidecarServerForAgent(socketPath, logPath, nil)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Close()

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Send a non-token-usage event. Bridge should silently drop.
	ev := sidecar.Event{
		V: 1, Seq: 1, TS: 1, Kind: sidecar.KindAssistantMessage,
		Data: json.RawMessage(`{"content":"hello"}`),
	}
	b, _ := json.Marshal(ev)
	conn.Write(append(b, '\n'))

	// Give the server a moment to process.
	time.Sleep(200 * time.Millisecond)

	info, err := os.Stat(logPath)
	if err == nil && info.Size() > 0 {
		data, _ := os.ReadFile(logPath)
		t.Errorf("non-token event was written to log:\n%s", data)
	}
}
