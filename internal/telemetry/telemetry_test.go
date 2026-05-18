package telemetry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNew_DisabledReturnsNop(t *testing.T) {
	tr := New(Config{Enabled: false, PublicKey: "pk", SecretKey: "sk"})
	if _, ok := tr.(Nop); !ok {
		t.Fatalf("disabled config should return Nop, got %T", tr)
	}
}

func TestNew_MissingKeysReturnsNop(t *testing.T) {
	tr := New(Config{Enabled: true, PublicKey: "pk"}) // no secret
	if _, ok := tr.(Nop); !ok {
		t.Fatalf("missing keys should return Nop, got %T", tr)
	}
}

func TestNop_IsSafeAndZeroEffect(t *testing.T) {
	tr := Nop{}
	ctx, id := tr.NewTrace(context.Background(), "session")
	if id != "" {
		t.Errorf("Nop NewTrace should return empty id, got %q", id)
	}
	tr.Router(ctx, RouterEvent{ChosenModel: "x"})
	sp := tr.AgentStart(ctx, AgentEvent{AgentID: "w"})
	sp.End(nil, map[string]any{"k": "v"})
	if err := tr.Flush(time.Second); err != nil {
		t.Errorf("Nop Flush returned %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Errorf("Nop Close returned %v", err)
	}
}

func TestWithTraceID_RoundTrip(t *testing.T) {
	ctx := WithTraceID(context.Background(), "abc")
	if got := TraceIDFromContext(ctx); got != "abc" {
		t.Errorf("expected abc, got %q", got)
	}
	if got := TraceIDFromContext(WithTraceID(context.Background(), "")); got != "" {
		t.Errorf("empty trace id should round-trip empty, got %q", got)
	}
}

func TestRedact_ScrubsKnownSecretShapes(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"anthropic key", "leak sk-ant-abcdef12345678901234567890 here"},
		{"langfuse secret", "leak sk-lf-abcdef1234567890abcdef end"},
		{"github pat", "leak ghp_abcdef1234567890abcdef1234567890ABCD trailing"},
		{"jwt", "auth eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjMifQ.abc-xyz_def"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := Scrub(c.in)
			if strings.Contains(out, "sk-ant-") || strings.Contains(out, "sk-lf-") ||
				strings.Contains(out, "ghp_") || strings.Contains(out, "eyJhbG") {
				t.Errorf("Scrub leaked secret content: %q", out)
			}
			if !strings.Contains(out, "<secret:") {
				t.Errorf("Scrub should mark redaction with <secret:..>: %q", out)
			}
		})
	}
}

func TestRedact_Idempotent(t *testing.T) {
	in := "leak sk-ant-abcdef12345678901234567890 end"
	once := Scrub(in)
	twice := Scrub(once)
	if once != twice {
		t.Errorf("Scrub not idempotent: once=%q twice=%q", once, twice)
	}
}

func TestRedacted_HasShape(t *testing.T) {
	if got := Redacted(""); got != "<empty>" {
		t.Errorf("expected <empty>, got %q", got)
	}
	got := Redacted("hello")
	if !strings.HasPrefix(got, "<redacted:len=5") {
		t.Errorf("expected len=5 prefix, got %q", got)
	}
}

// TestLangfuse_RouterAndAgentRoundTrip stands up a stub HTTP server, runs a
// representative trace, and asserts the wire payload looks right. This is the
// integration safety net — if the Langfuse API contract changes we'll see it.
func TestLangfuse_RouterAndAgentRoundTrip(t *testing.T) {
	var received int32
	var bodies [][]byte
	var bodiesMu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/public/ingestion" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if u, p, ok := r.BasicAuth(); !ok || u != "pk-test" || p != "sk-test" {
			t.Errorf("bad auth: ok=%v u=%q p=%q", ok, u, p)
		}
		b, _ := io.ReadAll(r.Body)
		bodiesMu.Lock()
		bodies = append(bodies, b)
		bodiesMu.Unlock()
		atomic.AddInt32(&received, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tr := New(Config{
		Enabled:    true,
		Host:       srv.URL,
		PublicKey:  "pk-test",
		SecretKey:  "sk-test",
		SampleRate: 1.0,
		Release:    "test",
	})
	defer tr.Close()

	ctx, traceID := tr.NewTrace(context.Background(), "test-session")
	if traceID == "" {
		t.Fatal("expected non-empty trace id")
	}

	tr.Router(ctx, RouterEvent{
		TaskTextHash: "abcd",
		TaskTextLen:  42,
		Complexity:   "standard",
		Candidates:   []string{"anthropic:claude-haiku-4-5", "openai:gpt-5.4-mini"},
		ChosenModel:  "anthropic:claude-haiku-4-5",
		Reason:       "complexity=standard chose haiku",
		FloorMet:     true,
		InputPriceUS: 0.80,
	})

	sp := tr.AgentStart(ctx, AgentEvent{
		AgentID:   "worker-foo",
		AgentType: "worker",
		RepoName:  "demo",
		Model:     "anthropic:claude-haiku-4-5",
	})
	sp.End(nil, map[string]any{"exit_reason": "success"})

	// Allow background worker to flush.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&received) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := tr.Flush(2 * time.Second); err != nil {
		t.Errorf("flush: %v", err)
	}
	tr.Close()

	if atomic.LoadInt32(&received) == 0 {
		t.Fatal("Langfuse server received no requests")
	}

	// Verify we sent the right event types.
	bodiesMu.Lock()
	defer bodiesMu.Unlock()
	var seenTypes = map[string]int{}
	for _, b := range bodies {
		var env struct {
			Batch []struct {
				Type string `json:"type"`
			} `json:"batch"`
		}
		if err := json.Unmarshal(b, &env); err != nil {
			t.Errorf("bad payload: %v", err)
			continue
		}
		for _, ev := range env.Batch {
			seenTypes[ev.Type]++
		}
	}
	for _, want := range []string{"trace-create", "event-create", "span-create", "span-update"} {
		if seenTypes[want] == 0 {
			t.Errorf("expected at least one %q event, got %+v", want, seenTypes)
		}
	}
}

func TestLangfuse_NetworkFailureIsSilent(t *testing.T) {
	// Point at an obviously-dead host; the tracer must not panic and must
	// drop events without breaking the caller.
	tr := New(Config{
		Enabled:    true,
		Host:       "http://127.0.0.1:1", // refused
		PublicKey:  "pk",
		SecretKey:  "sk",
		SampleRate: 1.0,
	})
	defer tr.Close()

	ctx, _ := tr.NewTrace(context.Background(), "doomed")
	for i := 0; i < 100; i++ {
		tr.Router(ctx, RouterEvent{ChosenModel: "x"})
	}
	// If we got here without panicking the contract holds.
	_ = tr.Flush(500 * time.Millisecond)
}
