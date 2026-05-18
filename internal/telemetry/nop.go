package telemetry

import (
	"context"
	"time"
)

// Nop is a Tracer that discards every call. It is the default when telemetry
// is disabled, when credentials are missing, or when the user is offline.
//
// All methods are zero-allocation in the steady state — calling code can
// instrument freely without runtime cost when telemetry is off.
type Nop struct{}

func (Nop) NewTrace(ctx context.Context, _ string, _ map[string]any) (context.Context, string) {
	return ctx, ""
}

func (Nop) Router(_ context.Context, _ RouterEvent) {}

func (Nop) AgentStart(_ context.Context, _ AgentEvent) {}

func (Nop) AgentEnd(_ context.Context, _ AgentExit) {}

func (Nop) Flush(_ time.Duration) error { return nil }

func (Nop) Close() error { return nil }
