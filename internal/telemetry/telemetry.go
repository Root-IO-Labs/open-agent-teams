// Package telemetry emits structured traces to Langfuse for router decisions,
// agent lifecycle, and (via the Python runtime) LLM calls and tool calls.
//
// The Tracer interface has two production implementations:
//
//   - Nop: zero-allocation no-op, used when telemetry is disabled. This is the
//     default; consumers should always be safe to call Tracer methods without
//     checking whether telemetry is on.
//   - Langfuse: HTTP client that queues spans on a background goroutine and
//     flushes in batches. Failures degrade silently after a single warning per
//     session so telemetry never blocks the hot path.
//
// Spans are correlated across the Go daemon and the Python agent runtime by a
// shared TraceID. The daemon assigns a TraceID per agent spawn and passes it to
// the Python process via the OAT_TRACE_ID environment variable; the Python
// LangfuseMiddleware reads it and nests its generations under the same trace.
package telemetry

import (
	"context"
	"time"
)

// ctxKey is unexported so callers can only thread TraceIDs via the helpers
// below — preventing accidental collisions with other context values.
type ctxKey struct{}

// WithTraceID stores a trace id on ctx for downstream spans to inherit.
func WithTraceID(ctx context.Context, traceID string) context.Context {
	if traceID == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, traceID)
}

// TraceIDFromContext returns the trace id stored on ctx, or "" if none.
func TraceIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(ctxKey{}).(string)
	return v
}

// Config controls Tracer construction. Mirrors the on-disk TelemetryConfig in
// internal/state; kept separate so the telemetry package has no dependency on
// the state package (which would create an import cycle for tests).
type Config struct {
	Enabled    bool
	Host       string  // e.g. "https://cloud.langfuse.com"
	PublicKey  string  // pk-lf-...
	SecretKey  string  // sk-lf-...
	RedactArgs bool    // redact tool args / prompt bodies (default true)
	SampleRate float64 // 0.0 – 1.0; 1.0 = all spans
	Release    string  // OAT version, for grouping in Langfuse UI
}

// RouterEvent records one model-routing decision. Captured at the moment the
// router returns its pick.
type RouterEvent struct {
	TaskTextHash string   // sha256 of input; never the input itself
	TaskTextLen  int      // length in bytes — gives shape without leaking content
	Complexity   string   // "trivial" / "simple" / "standard" / "complex" / "unknown"
	Candidates   []string // ordered, first = chosen
	ChosenModel  string
	Reason       string
	FloorMet     bool
	InputPriceUS float64 // USD per million input tokens
}

// AgentEvent records an agent lifecycle. Returned Span is ended on exit.
type AgentEvent struct {
	AgentID       string // e.g. worker name
	AgentType     string // "worker" / "supervisor" / ...
	RepoName      string
	Model         string
	RoutingSource string
	ParentTraceID string // optional — for nested agents
}

// AgentExit is the terminal state of an agent. Passed to Span.End via attrs.
type AgentExit struct {
	Reason       string // "success" / "crashed" / "timeout" / "cancelled" / "killed"
	ExitCode     int
	Signal       string
	InputTokens  int64
	OutputTokens int64
}

// Span is a started, not-yet-ended operation. Callers must call End exactly
// once. End is safe against double-call; subsequent calls are no-ops.
type Span interface {
	// TraceID returns the underlying trace ID. The daemon uses this to wire
	// OAT_TRACE_ID into spawned agent processes so Python spans nest into the
	// same trace.
	TraceID() string

	// End finalizes the span. err != nil marks the span as errored. attrs are
	// merged into the span's payload. Must be safe to call once.
	End(err error, attrs map[string]any)
}

// Tracer is the telemetry sink.
//
// All methods must be safe to call on a nil-or-disabled Tracer (the Nop
// implementation handles that). The router decision span is fire-and-forget;
// the agent span returns a handle the caller closes when the agent terminates.
type Tracer interface {
	// NewTrace allocates a new trace ID and returns ctx + the id. Use this at
	// the top of a logical session (e.g. one agent task).
	NewTrace(ctx context.Context, name string) (context.Context, string)

	// Router records a routing decision. Fire-and-forget.
	Router(ctx context.Context, ev RouterEvent)

	// AgentStart records an agent spawn. Caller invokes Span.End on exit.
	AgentStart(ctx context.Context, ev AgentEvent) Span

	// Flush drains the in-memory queue. Bounded by timeout. Used at daemon
	// shutdown to avoid dropping the last batch.
	Flush(timeout time.Duration) error

	// Close stops the background worker and releases resources. Idempotent.
	Close() error
}

// New constructs a Tracer from cfg. If cfg.Enabled is false or required fields
// are missing, returns a Nop tracer — callers don't need to branch.
func New(cfg Config) Tracer {
	if !cfg.Enabled || cfg.PublicKey == "" || cfg.SecretKey == "" {
		return Nop{}
	}
	host := cfg.Host
	if host == "" {
		host = "https://cloud.langfuse.com"
	}
	rate := cfg.SampleRate
	if rate <= 0 {
		rate = 1.0
	}
	return newLangfuseTracer(cfg, host, rate)
}
