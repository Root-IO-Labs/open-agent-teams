package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestCrossRuntime_TraceCorrelation proves the architectural invariant of the
// whole telemetry feature: that the Go side and Python side share one Langfuse
// trace when the daemon hands the Python child an OAT_TRACE_ID env var.
//
// Without this test, the Go and Python pieces are each tested in isolation,
// but nothing verifies the actual handshake. Here we:
//
//  1. Stand up a stub Langfuse ingestion server.
//  2. Have the Go tracer emit a trace + router + agent_start.
//  3. Spawn a Python child (using the real LangfuseClient module from the
//     worktree) with LANGFUSE_*/OAT_TRACE_ID set; it emits a generation +
//     tool span.
//  4. Have the Go tracer emit agent_end.
//  5. Assert the stub saw FIVE events, ALL on the same traceId.
//
// Skipped automatically if python3 (≥3.11) isn't on PATH, so it doesn't break
// CI on barebones runners.
func TestCrossRuntime_TraceCorrelation(t *testing.T) {
	pyBin := findPython3(t)
	if pyBin == "" {
		t.Skip("python3 (≥3.11) not available on PATH; skipping cross-runtime test")
	}

	// 1. Stub server collecting every batch it receives.
	var mu sync.Mutex
	var batches [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/public/ingestion" {
			http.Error(w, "not found", 404)
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		batches = append(batches, b)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	cfg := Config{
		Enabled:    true,
		Host:       srv.URL,
		PublicKey:  "pk-cross",
		SecretKey:  "sk-cross",
		SampleRate: 1.0,
		Release:    "cross-runtime-test",
	}
	tr := New(cfg)
	defer tr.Close()

	// 2. Go-side events.
	ctx, traceID := tr.NewTrace(context.Background(), "agent:worker", map[string]any{
		"repo":       "demo-cross",
		"agent_id":   "worker-x",
		"agent_type": "worker",
	})
	if traceID == "" {
		t.Fatal("Go tracer returned empty trace id — telemetry.New fell back to Nop?")
	}
	tr.Router(ctx, RouterEvent{
		TaskTextHash: "ab12",
		TaskTextLen:  120,
		Complexity:   "standard",
		Candidates:   []string{"anthropic:claude-haiku-4-5", "openai:gpt-5.4-mini"},
		ChosenModel:  "anthropic:claude-haiku-4-5",
		Reason:       "complexity=standard chose haiku",
		FloorMet:     true,
		InputPriceUS: 0.80,
	})
	tr.AgentStart(ctx, AgentEvent{
		AgentID:   "worker-x",
		AgentType: "worker",
		RepoName:  "demo-cross",
		Model:     "anthropic:claude-haiku-4-5",
	})

	// 3. Spawn Python child with the trace id.
	worktreeRoot := findWorktreeRoot(t)
	scriptPath := filepath.Join(t.TempDir(), "child.py")
	if err := os.WriteFile(scriptPath, []byte(crossRuntimeChildScript), 0644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	cmd := exec.Command(pyBin, scriptPath, worktreeRoot)
	cmd.Env = append(os.Environ(),
		"LANGFUSE_HOST="+srv.URL,
		"LANGFUSE_PUBLIC_KEY=pk-cross",
		"LANGFUSE_SECRET_KEY=sk-cross",
		"OAT_TRACE_ID="+traceID,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("python child failed: %v\nstderr: %s", err, stderr.String())
	}

	// 4. Go-side agent_end.
	tr.AgentEnd(ctx, AgentExit{
		AgentID:      "worker-x",
		Reason:       "success",
		InputTokens:  1200,
		OutputTokens: 450,
	})
	if err := tr.Flush(3 * time.Second); err != nil {
		t.Fatalf("flush: %v", err)
	}
	tr.Close()
	// Give the in-flight HTTP send a final beat to land.
	time.Sleep(200 * time.Millisecond)

	// 5. Assert.
	mu.Lock()
	defer mu.Unlock()
	if len(batches) == 0 {
		t.Fatal("stub server received zero requests")
	}

	type seen struct {
		Type    string
		TraceID string
	}
	var allEvents []seen
	for _, raw := range batches {
		var env struct {
			Batch []struct {
				Type string         `json:"type"`
				Body map[string]any `json:"body"`
			} `json:"batch"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("bad payload: %v\n%s", err, raw)
		}
		for _, ev := range env.Batch {
			tid, _ := ev.Body["traceId"].(string)
			allEvents = append(allEvents, seen{Type: ev.Type, TraceID: tid})
		}
	}

	// Every required event type must appear at least once.
	counts := map[string]int{}
	traceIDs := map[string]int{}
	for _, e := range allEvents {
		counts[e.Type]++
		if e.TraceID != "" {
			traceIDs[e.TraceID]++
		}
	}

	for _, want := range []string{"trace-create", "event-create", "generation-create", "span-create"} {
		if counts[want] == 0 {
			t.Errorf("missing event type %q in cross-runtime trace; got types=%v", want, counts)
		}
	}

	// Exactly ONE trace id should appear across non-trace-create events.
	// trace-create's traceId is empty; the body.id is what matters there.
	if len(traceIDs) != 1 {
		t.Errorf("expected one correlated trace id across Go+Python events, saw %d distinct: %v", len(traceIDs), traceIDs)
	}
	if traceIDs[traceID] == 0 {
		t.Errorf("expected events on trace %q (Go-generated), but found only %v", traceID, traceIDs)
	}

	t.Logf("cross-runtime trace correlated: %d events on trace %s — counts=%v", len(allEvents), traceID, counts)
}

// findPython3 returns the path to a Python 3.11+ interpreter, or "" if none.
// We try the homebrew paths first (where most macOS dev machines have a
// recent Python), then fall back to plain `python3` on PATH.
func findPython3(t *testing.T) string {
	t.Helper()
	candidates := []string{
		"/opt/homebrew/bin/python3.13",
		"/opt/homebrew/bin/python3.12",
		"/opt/homebrew/bin/python3.11",
		"python3",
	}
	for _, c := range candidates {
		path, err := exec.LookPath(c)
		if err != nil {
			continue
		}
		// Quick version sniff — must be ≥ 3.11 because oat_sdk uses NotRequired.
		out, err := exec.Command(path, "-c", "import sys; print(sys.version_info[0], sys.version_info[1])").CombinedOutput()
		if err != nil {
			continue
		}
		var maj, min int
		if _, err := fmtScan(out, &maj, &min); err != nil {
			continue
		}
		if maj == 3 && min >= 11 {
			return path
		}
	}
	return ""
}

// fmtScan parses "<maj> <min>\n" from a bytes buffer. Tiny helper, keeps the
// fmt.Sscan call site readable.
func fmtScan(b []byte, maj, min *int) (int, error) {
	r := bytes.TrimSpace(b)
	parts := bytes.Fields(r)
	if len(parts) < 2 {
		return 0, io.ErrUnexpectedEOF
	}
	for _, p := range [...]struct {
		dst *int
		src []byte
	}{{maj, parts[0]}, {min, parts[1]}} {
		v := 0
		for _, c := range p.src {
			if c < '0' || c > '9' {
				return 0, io.ErrUnexpectedEOF
			}
			v = v*10 + int(c-'0')
		}
		*p.dst = v
	}
	return 2, nil
}

// findWorktreeRoot walks up from this test file to find the worktree root
// (the dir containing go.mod). Used so the Python child can find the
// sibling oat_sdk source tree without hardcoding paths.
func findWorktreeRoot(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := cwd
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not locate worktree root containing go.mod (started from %s)", cwd)
	return ""
}

const crossRuntimeChildScript = `"""Python half of the cross-runtime trace correlation test.
Loads the LangfuseClient module from the worktree (sys.argv[1] is the
worktree root), emits one generation + one tool span, and exits.
"""
import sys, importlib.util

worktree = sys.argv[1]
spec = importlib.util.spec_from_file_location(
    "lc",
    f"{worktree}/agent-runtime/libs/oat_sdk/oat_sdk/middleware/_langfuse_client.py",
)
lc = importlib.util.module_from_spec(spec)
spec.loader.exec_module(lc)

client = lc.get_client()
if client is None:
    print("ERROR: client is None (env vars missing?)", file=sys.stderr)
    sys.exit(2)

client.emit_generation(
    name="llm_call",
    model="anthropic:claude-haiku-4-5",
    input_tokens=800,
    output_tokens=200,
    start_time="2026-05-18T16:00:01.000Z",
    end_time="2026-05-18T16:00:02.500Z",
    metadata={"wall_ms": 1500},
)
client.emit_tool_span(
    name="edit_file",
    start_time="2026-05-18T16:00:02.500Z",
    end_time="2026-05-18T16:00:02.503Z",
    metadata={"tool_call_id": "tc-cross", "arg_count": 2},
)
client.close(timeout=3.0)
print("python child done", file=sys.stderr)
`
