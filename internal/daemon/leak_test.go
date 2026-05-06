package daemon

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain wires goleak.VerifyTestMain for the daemon package so any
// goroutine that outlives a test fails CI. setupTestDaemon's cleanup()
// now calls d.Stop() which cancels d.ctx and waits on d.wg — every
// goroutine the daemon itself spawns (health check, message router,
// wake/nudge, socket server, PR monitor, OutputWatchers) observes the
// cancel and exits.
//
// The remaining leak sources are backend-owned — they fire only in
// tests that start a real agent subprocess through the backend and
// don't round-trip through StopAgent:
//
//	DirectBackend.StartAgent.func2 — PTY read loop
//	DirectBackend.StartAgent.func3 — cmd.Wait on the subprocess
//	newRawBroadcaster.func1         — tee ring buffer fanout
//
// All three live as long as the subprocess does. This is a backend-side
// test-cleanup gap; ignore at the daemon-package boundary here and track
// the real fix under a separate goleak-in-pkg/backend follow-up.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreAnyFunction("github.com/Root-IO-Labs/open-agent-teams/pkg/backend.(*DirectBackend).StartAgent.func2"),
		goleak.IgnoreAnyFunction("github.com/Root-IO-Labs/open-agent-teams/pkg/backend.(*DirectBackend).StartAgent.func3"),
		goleak.IgnoreAnyFunction("github.com/Root-IO-Labs/open-agent-teams/pkg/backend.newRawBroadcaster.func1"),
	)
}
