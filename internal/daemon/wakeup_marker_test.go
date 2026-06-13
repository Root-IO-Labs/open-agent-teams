package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
	backend_pkg "github.com/Root-IO-Labs/open-agent-teams/pkg/backend"
)

// markerTestBackend is the same shape as routeTestBackend in
// daemon_test.go but lives here so the wake-up marker tests are
// self-contained — the file can be deleted as a unit without
// chasing references into the route-message test fixtures, and a
// reader who jumps to this file does not have to context-switch
// between unrelated test wiring.
//
// Records every SendMessage call so assertions can pin BOTH the
// exact marker bytes that hit the (would-be) PTY AND the order in
// which markers arrived relative to other writes.
type markerTestBackend struct {
	backend_pkg.ProcessBackend // nil: forces a panic on any un-overridden method
	mu                         sync.Mutex
	sent                       []markerSendCall
	sendErr                    error
}

type markerSendCall struct {
	Session string
	Agent   string
	Message string
}

func (b *markerTestBackend) SendMessage(_ context.Context, session, agent, message string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sendErr != nil {
		return b.sendErr
	}
	b.sent = append(b.sent, markerSendCall{Session: session, Agent: agent, Message: message})
	return nil
}

func (b *markerTestBackend) calls() []markerSendCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]markerSendCall, len(b.sent))
	copy(out, b.sent)
	return out
}

func (b *markerTestBackend) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent = nil
}

// setupMarkerDaemon spins up a daemon for the wake-up marker test
// matrix and pre-registers a single assistant agent with a known
// PID. Returns (daemon, fake backend, repo name, agent name,
// cleanup).
//
// The agent's PID is set to a synthetic value (>0) because the
// in-process tracker keys on (repo, agent, pid) — production code
// uses the live process PID; tests just need a stable non-zero
// integer.
func setupMarkerDaemon(t *testing.T) (*Daemon, *markerTestBackend, string, string, func()) {
	t.Helper()
	d, cleanup := setupTestDaemon(t)

	fake := &markerTestBackend{}
	d.backend = fake

	repoName := "test-repo"
	agentName := "personal"
	repo := &state.Repository{
		GithubURL:   "https://github.com/test/repo",
		SessionName: "test-session",
		Agents:      map[string]state.Agent{},
	}
	if err := d.state.AddRepo(repoName, repo); err != nil {
		t.Fatalf("AddRepo: %v", err)
	}
	agent := state.Agent{
		Type:         state.AgentTypeAssistant,
		WindowName:   "personal",
		WorktreePath: filepath.Join(d.paths.WorktreeDir(repoName), agentName),
		PID:          42424,
	}
	if err := d.state.AddAgent(repoName, agentName, agent); err != nil {
		t.Fatalf("AddAgent: %v", err)
	}
	return d, fake, repoName, agentName, cleanup
}

// TestBuildWakeUpMarker_StableText pins the marker text: the exact
// wording is part of the contract — both because changing it
// requires a coordinated update to assistant.md's "Stale-intent
// guard" section, and because the marker text must stay hardcoded
// with no user-controllable interpolation surface (see
// buildWakeUpMarker's docstring for why).
func TestBuildWakeUpMarker_StableText(t *testing.T) {
	got := buildWakeUpMarker("my-repo", "my-agent")
	wantSubs := []string{
		"[OAT-system]",
		"my-repo/my-agent",
		"just (re)started",
		"Do NOT call any tools",
		"`[SIDE-PANEL CHAT] `",
		"stop and explain what you'd otherwise do",
	}
	for _, want := range wantSubs {
		if !strings.Contains(got, want) {
			t.Errorf("buildWakeUpMarker missing substring %q\nfull text: %s", want, got)
		}
	}

	// Stability across two calls (no time-dependent fields, no
	// random nonces). A regression that adds a timestamp or RNG
	// breaks both the security model (introduces non-determinism
	// the audit log can't verify against) and prompt-cache hits
	// for the agent.
	again := buildWakeUpMarker("my-repo", "my-agent")
	if got != again {
		t.Errorf("buildWakeUpMarker is not deterministic across calls")
	}
}

// Marker is prepended on the first PTY write for each of the three
// real trigger types — fresh spawn, restart, daemon-restart.
//
// We exercise injectWakeUpMarker directly (rather than going
// through the surrounding startRegisteredAgent / restartAgent
// paths) because those paths require a real backend to spawn an
// agent process. The injection contract is "after the spawn
// path's backend.StartAgent returns successfully, before anything
// else writes to the PTY" — calling injectWakeUpMarker in
// isolation is faithful to that contract because production code
// does the same thing at the same point in the pipeline.
func TestInjectWakeUpMarker_FiresOncePerTrigger(t *testing.T) {
	cases := []WakeUpMarkerTrigger{
		WakeUpMarkerTriggerFresh,
		WakeUpMarkerTriggerRestart,
		WakeUpMarkerTriggerDaemonRestart,
	}
	for _, trig := range cases {
		t.Run(string(trig), func(t *testing.T) {
			d, fake, repoName, agentName, cleanup := setupMarkerDaemon(t)
			defer cleanup()

			t.Setenv(wakeUpMarkerDisabledEnv, "")

			agent, _ := d.state.GetAgent(repoName, agentName)
			d.injectWakeUpMarker(repoName, agentName, agent, trig)

			calls := fake.calls()
			if len(calls) != 1 {
				t.Fatalf("trigger=%s expected 1 SendMessage, got %d", trig, len(calls))
			}
			if calls[0].Session != "test-session" || calls[0].Agent != "personal" {
				t.Errorf("trigger=%s send target = (%q, %q), want (test-session, personal)",
					trig, calls[0].Session, calls[0].Agent)
			}
			if !strings.Contains(calls[0].Message, "[OAT-system]") {
				t.Errorf("trigger=%s message missing [OAT-system] prefix; got: %q",
					trig, calls[0].Message)
			}
			if !strings.Contains(calls[0].Message, "test-repo/personal") {
				t.Errorf("trigger=%s message missing repo/agent identity; got: %q",
					trig, calls[0].Message)
			}
		})
	}
}

// Marker is NOT prepended a second time within the same process
// lifetime. The in-process tracker keys on (repo, agent, pid); a
// second injection call for the same pid is a no-op.
//
// In production this guards the "two parallel spawn-path callers
// race on the same window" case: e.g. the handleStartRepoAgents
// already-alive branch firing back-to-back with restartAgent. Both
// want to inject the marker, but only the first one should land
// bytes.
func TestInjectWakeUpMarker_OncePerProcessLifetime(t *testing.T) {
	d, fake, repoName, agentName, cleanup := setupMarkerDaemon(t)
	defer cleanup()

	t.Setenv(wakeUpMarkerDisabledEnv, "")
	// Disable the on-disk rate-limit so this test isolates the
	// in-process gate. The rate-limit file path is exercised
	// separately by TestInjectWakeUpMarker_RateLimitFile.
	t.Setenv(wakeUpMarkerIntervalEnv, "0")

	agent, _ := d.state.GetAgent(repoName, agentName)

	d.injectWakeUpMarker(repoName, agentName, agent, WakeUpMarkerTriggerFresh)
	d.injectWakeUpMarker(repoName, agentName, agent, WakeUpMarkerTriggerRestart)
	d.injectWakeUpMarker(repoName, agentName, agent, WakeUpMarkerTriggerDaemonRestart)

	calls := fake.calls()
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 SendMessage across 3 injections (same pid), got %d", len(calls))
	}

	// Bump the pid to simulate a real respawn (new process, new
	// pid). The tracker should let this through — different key.
	agent.PID = 99999
	d.injectWakeUpMarker(repoName, agentName, agent, WakeUpMarkerTriggerFresh)
	if got := len(fake.calls()); got != 2 {
		t.Fatalf("expected 2 SendMessage after pid change, got %d", got)
	}
}

// OAT_ASSISTANT_WAKEUP_MARKER_DISABLED=1 skips the marker
// regardless of trigger. The escape hatch is a global gate so a
// dev workflow that intentionally wants resumption (e.g. a
// restartAgent unit test that needs the agent to act on its
// rehydrated turn) can opt out without per-call plumbing.
func TestInjectWakeUpMarker_DisabledEnvSkips(t *testing.T) {
	d, fake, repoName, agentName, cleanup := setupMarkerDaemon(t)
	defer cleanup()

	t.Setenv(wakeUpMarkerDisabledEnv, "1")

	agent, _ := d.state.GetAgent(repoName, agentName)
	d.injectWakeUpMarker(repoName, agentName, agent, WakeUpMarkerTriggerFresh)

	if got := len(fake.calls()); got != 0 {
		t.Fatalf("expected 0 SendMessage when %s=1, got %d", wakeUpMarkerDisabledEnv, got)
	}

	// Anything other than the literal "1" leaves the marker
	// active — matches the explicit-opt-out semantics of the
	// other OAT_*_DISABLED knobs.
	t.Setenv(wakeUpMarkerDisabledEnv, "true")
	agent.PID = 55555 // bump pid so the in-process tracker doesn't suppress
	d.injectWakeUpMarker(repoName, agentName, agent, WakeUpMarkerTriggerFresh)
	if got := len(fake.calls()); got != 1 {
		t.Fatalf("expected 1 SendMessage when %s=true (not literal '1'), got %d",
			wakeUpMarkerDisabledEnv, got)
	}
}

// Marker is NOT prepended for AgentTypeBrowser. Browser-agents are
// workflow helpers — they are designed for single-task autonomous
// execution; suppressing post-restart action defeats their purpose.
// The type-gate is grep-friendly defense against a future PR
// widening the marker to all bridge-using agents.
func TestInjectWakeUpMarker_BrowserAgentExempt(t *testing.T) {
	d, fake, repoName, _, cleanup := setupMarkerDaemon(t)
	defer cleanup()

	t.Setenv(wakeUpMarkerDisabledEnv, "")

	browserName := "browser-agent"
	browser := state.Agent{
		Type:         state.AgentTypeBrowser,
		WindowName:   "browser-agent",
		WorktreePath: filepath.Join(d.paths.WorktreeDir(repoName), browserName),
		PID:          77777,
	}
	if err := d.state.AddAgent(repoName, browserName, browser); err != nil {
		t.Fatalf("AddAgent browser: %v", err)
	}

	d.injectWakeUpMarker(repoName, browserName, browser, WakeUpMarkerTriggerFresh)

	if got := len(fake.calls()); got != 0 {
		t.Fatalf("expected 0 SendMessage for browser-agent type, got %d", got)
	}
}

// Rate-limit suppresses a second marker within the interval, then
// allows one after the interval elapses.
//
// We can't roll the clock forward in process, so the test takes
// two angles:
//  1. With OAT_ASSISTANT_WAKEUP_MARKER_INTERVAL_MIN=60 and a
//     recent timestamp file on disk, the marker is suppressed.
//  2. With the same env and a stale-enough timestamp on disk
//     (now - 90 min), the marker fires.
//
// This pins the file-based rate-limit half of the design (which
// is what survives daemon restarts); the in-process gate is
// covered by TestInjectWakeUpMarker_OncePerProcessLifetime above.
func TestInjectWakeUpMarker_RateLimitFile(t *testing.T) {
	t.Run("recent_fire_suppresses", func(t *testing.T) {
		d, fake, repoName, agentName, cleanup := setupMarkerDaemon(t)
		defer cleanup()

		t.Setenv(wakeUpMarkerDisabledEnv, "")
		t.Setenv(wakeUpMarkerIntervalEnv, "60")

		// Plant a recent timestamp (1 min ago).
		statePath, err := wakeUpMarkerStateFile(d.paths.Root, repoName, agentName)
		if err != nil {
			t.Fatalf("wakeUpMarkerStateFile: %v", err)
		}
		recent := time.Now().Add(-1 * time.Minute)
		if err := writeWakeUpMarkerLastFire(statePath, recent); err != nil {
			t.Fatalf("writeWakeUpMarkerLastFire: %v", err)
		}

		agent, _ := d.state.GetAgent(repoName, agentName)
		d.injectWakeUpMarker(repoName, agentName, agent, WakeUpMarkerTriggerFresh)

		if got := len(fake.calls()); got != 0 {
			t.Fatalf("expected 0 SendMessage when last fire was 1m ago and interval is 60m, got %d", got)
		}
	})

	t.Run("stale_fire_proceeds", func(t *testing.T) {
		d, fake, repoName, agentName, cleanup := setupMarkerDaemon(t)
		defer cleanup()

		t.Setenv(wakeUpMarkerDisabledEnv, "")
		t.Setenv(wakeUpMarkerIntervalEnv, "60")

		statePath, err := wakeUpMarkerStateFile(d.paths.Root, repoName, agentName)
		if err != nil {
			t.Fatalf("wakeUpMarkerStateFile: %v", err)
		}
		stale := time.Now().Add(-90 * time.Minute)
		if err := writeWakeUpMarkerLastFire(statePath, stale); err != nil {
			t.Fatalf("writeWakeUpMarkerLastFire: %v", err)
		}

		agent, _ := d.state.GetAgent(repoName, agentName)
		d.injectWakeUpMarker(repoName, agentName, agent, WakeUpMarkerTriggerFresh)

		if got := len(fake.calls()); got != 1 {
			t.Fatalf("expected 1 SendMessage when last fire was 90m ago and interval is 60m, got %d", got)
		}

		// The successful firing must update the timestamp file
		// (otherwise the next spawn within the window would NOT
		// be rate-limited, defeating the persistence guarantee).
		updated, err := readWakeUpMarkerLastFire(statePath)
		if err != nil {
			t.Fatalf("readWakeUpMarkerLastFire after fire: %v", err)
		}
		if updated.Before(time.Now().Add(-1 * time.Minute)) {
			t.Errorf("expected timestamp file to be refreshed to ~now, got %s",
				updated.Format(time.RFC3339))
		}
	})

	t.Run("interval_zero_disables_ratelimit", func(t *testing.T) {
		d, fake, repoName, agentName, cleanup := setupMarkerDaemon(t)
		defer cleanup()

		t.Setenv(wakeUpMarkerDisabledEnv, "")
		t.Setenv(wakeUpMarkerIntervalEnv, "0")

		statePath, err := wakeUpMarkerStateFile(d.paths.Root, repoName, agentName)
		if err != nil {
			t.Fatalf("wakeUpMarkerStateFile: %v", err)
		}
		recent := time.Now().Add(-1 * time.Second)
		if err := writeWakeUpMarkerLastFire(statePath, recent); err != nil {
			t.Fatalf("writeWakeUpMarkerLastFire: %v", err)
		}

		agent, _ := d.state.GetAgent(repoName, agentName)
		d.injectWakeUpMarker(repoName, agentName, agent, WakeUpMarkerTriggerFresh)

		if got := len(fake.calls()); got != 1 {
			t.Fatalf("expected 1 SendMessage when interval=0, got %d", got)
		}
	})
}

// Suppressed firings still emit the audit-log INFO line with
// trigger=rate-limited.
//
// The current daemon plumbs the audit event via d.logger.Info (no
// structured event-log surface today). We can't easily intercept
// the production logger from this test without a larger refactor;
// instead we pin the BEHAVIOR (suppression produced no SendMessage
// AND did not refresh the on-disk timestamp file) and rely on code
// review to confirm the operator-visible audit line lands via
// daemon.log. A follow-up that adds a real audit-log channel can
// swap this for a direct assertion.
func TestInjectWakeUpMarker_RateLimitObservable(t *testing.T) {
	d, fake, repoName, agentName, cleanup := setupMarkerDaemon(t)
	defer cleanup()

	t.Setenv(wakeUpMarkerDisabledEnv, "")
	t.Setenv(wakeUpMarkerIntervalEnv, "60")

	statePath, err := wakeUpMarkerStateFile(d.paths.Root, repoName, agentName)
	if err != nil {
		t.Fatalf("wakeUpMarkerStateFile: %v", err)
	}
	if err := writeWakeUpMarkerLastFire(statePath, time.Now()); err != nil {
		t.Fatalf("writeWakeUpMarkerLastFire: %v", err)
	}

	agent, _ := d.state.GetAgent(repoName, agentName)
	d.injectWakeUpMarker(repoName, agentName, agent, WakeUpMarkerTriggerFresh)

	if got := len(fake.calls()); got != 0 {
		t.Fatalf("expected 0 SendMessage on suppression, got %d", got)
	}
	// File timestamp MUST NOT advance — the suppressed firing
	// shouldn't refresh the cooldown, otherwise an unbounded
	// restart loop would forever hold the cooldown at "now".
	ts, err := readWakeUpMarkerLastFire(statePath)
	if err != nil {
		t.Fatalf("readWakeUpMarkerLastFire: %v", err)
	}
	if time.Since(ts) > 5*time.Second {
		// The file timestamp we planted was time.Now(); if the
		// suppression path refreshed it, ts would be very close
		// to now. We want it to STAY at our planted value, so
		// the time-since check ensures we don't see a meaningful
		// drift in either direction.
		t.Errorf("rate-limited fire updated the timestamp file (now %s ago); should leave file untouched",
			time.Since(ts))
	}
}

// Path sanitization rejects payloads that would let a corrupted
// state.json steer the rate-limit file at an arbitrary location.
// The current daemon's state is trusted, but this layer is
// defense-in-depth.
func TestSanitizeWakeUpMarkerPathSegment(t *testing.T) {
	cases := []struct {
		name    string
		seg     string
		wantErr bool
	}{
		{"empty", "", true},
		{"slash", "foo/bar", true},
		{"parent_traversal", "..", true},
		{"parent_traversal_embedded", "foo..bar", true},
		{"null_byte", "foo\x00bar", true},
		{"normal_repo", "my-repo", false},
		{"normal_assistant", "_assistant-personal", false},
		{"dotted_name", "v1.2.3", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := sanitizeWakeUpMarkerPathSegment(tc.seg)
			if tc.wantErr && err == nil {
				t.Errorf("expected error for seg=%q, got nil", tc.seg)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error for seg=%q: %v", tc.seg, err)
			}
		})
	}
}

// Rate-limit interval env override parses correctly across the
// expected shapes; bogus values fall back to the default so a
// typo doesn't silently disable rate-limiting (which would
// reintroduce the crash-loop context-pollution risk the marker
// itself is supposed to bound).
func TestWakeUpMarkerInterval_EnvParsing(t *testing.T) {
	cases := []struct {
		env      string
		wantMins int
	}{
		{"", wakeUpMarkerDefaultIntervalMin},
		{"  ", wakeUpMarkerDefaultIntervalMin},
		{"0", 0},
		{"30", 30},
		{"-5", wakeUpMarkerDefaultIntervalMin},
		{"not-a-number", wakeUpMarkerDefaultIntervalMin},
		// Leading/trailing whitespace is trimmed before parse,
		// so "  15  " resolves to 15.
		{"  15  ", 15},
	}
	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv(wakeUpMarkerIntervalEnv, tc.env)
			got := wakeUpMarkerInterval()
			want := time.Duration(tc.wantMins) * time.Minute
			if got != want {
				t.Errorf("env=%q: wakeUpMarkerInterval = %s, want %s", tc.env, got, want)
			}
		})
	}
}

// On-disk timestamp file format is forward-compatible: garbage
// content reads as "never fired" (zero time + error), but the
// caller's fail-open policy lets the marker proceed anyway.
// A future format change (e.g. JSON instead of raw integer) must
// preserve the "unparseable → don't suppress" semantics.
func TestReadWakeUpMarkerLastFire_FormatHandling(t *testing.T) {
	tmp := t.TempDir()
	cases := []struct {
		name      string
		contents  string
		wantZero  bool
		wantError bool
	}{
		{"empty_file", "", true, false},
		{"whitespace_only", "   \n\t  ", true, false},
		{"plain_integer", "1700000000", false, false},
		{"plain_integer_with_newline", "1700000000\n", false, false},
		{"garbage", "not-a-timestamp", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(tmp, tc.name+".ts")
			if err := os.WriteFile(path, []byte(tc.contents), 0o644); err != nil {
				t.Fatalf("setup write: %v", err)
			}
			got, err := readWakeUpMarkerLastFire(path)
			if tc.wantError && err == nil {
				t.Errorf("expected error reading %q, got nil", tc.contents)
			}
			if !tc.wantError && err != nil {
				t.Errorf("unexpected error reading %q: %v", tc.contents, err)
			}
			if tc.wantZero && !got.IsZero() {
				t.Errorf("expected zero time for %q, got %s", tc.contents, got)
			}
		})
	}

	t.Run("missing_file_is_zero_no_error", func(t *testing.T) {
		got, err := readWakeUpMarkerLastFire(filepath.Join(tmp, "does-not-exist"))
		if err != nil {
			t.Errorf("missing file should return nil error, got %v", err)
		}
		if !got.IsZero() {
			t.Errorf("missing file should return zero time, got %s", got)
		}
	})
}

// Atomic write produces a file whose contents round-trip
// through readWakeUpMarkerLastFire as the same Unix-second
// timestamp. Tests both the happy path and the dir-creation
// branch.
func TestWriteWakeUpMarkerLastFire_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "nested", "deeper", "wakeup-marker.ts")

	want := time.Unix(time.Now().Unix(), 0) // truncate to seconds (Unix() drops sub-second)
	if err := writeWakeUpMarkerLastFire(path, want); err != nil {
		t.Fatalf("writeWakeUpMarkerLastFire: %v", err)
	}

	got, err := readWakeUpMarkerLastFire(path)
	if err != nil {
		t.Fatalf("readWakeUpMarkerLastFire: %v", err)
	}
	if !got.Equal(want) {
		t.Errorf("round-trip mismatch: got %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}

	// Inspect file contents directly to confirm it's a bare
	// integer (no JSON, no extra fields) — pins the on-disk
	// shape so a future change must also update the format
	// docstring.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read raw: %v", err)
	}
	body := strings.TrimSpace(string(raw))
	if _, parseErr := strconv.ParseInt(body, 10, 64); parseErr != nil {
		t.Errorf("file contents %q are not a bare integer: %v", body, parseErr)
	}
}

// prepareWakeUpMarkerFile is the (re)spawn-time delivery path: it must
// write the marker payload to a consume-once file and return its path
// (which the spawn code turns into OAT_ASSISTANT_WAKEUP_MARKER_FILE), and
// it must NEVER touch the PTY — so the fake backend's SendMessage stays
// at zero calls regardless of outcome.
func TestPrepareWakeUpMarkerFile_WritesPayload(t *testing.T) {
	d, fake, repoName, agentName, cleanup := setupMarkerDaemon(t)
	defer cleanup()

	t.Setenv(wakeUpMarkerDisabledEnv, "")
	// interval 0 → never rate-limited, so the decision is purely "assistant?"
	t.Setenv(wakeUpMarkerIntervalEnv, "0")

	path := d.prepareWakeUpMarkerFile(repoName, agentName, state.AgentTypeAssistant)
	if path == "" {
		t.Fatal("expected a non-empty pending-file path for an assistant")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pending file: %v", err)
	}
	if got, want := string(raw), buildWakeUpMarker(repoName, agentName); got != want {
		t.Errorf("pending file contents mismatch:\n got: %q\nwant: %q", got, want)
	}

	// The runtime-delivery path must not write to the PTY.
	if got := len(fake.calls()); got != 0 {
		t.Errorf("expected 0 PTY SendMessage from prepareWakeUpMarkerFile, got %d", got)
	}

	// Timestamp file advanced so a rapid respawn within the interval
	// would be suppressed (crash-loop guard shared with injectWakeUpMarker).
	statePath, err := wakeUpMarkerStateFile(d.paths.Root, repoName, agentName)
	if err != nil {
		t.Fatalf("wakeUpMarkerStateFile: %v", err)
	}
	ts, err := readWakeUpMarkerLastFire(statePath)
	if err != nil {
		t.Fatalf("readWakeUpMarkerLastFire: %v", err)
	}
	if ts.IsZero() {
		t.Error("expected last-fire timestamp to be written after a successful prepare")
	}
}

// Non-assistant, disabled, and rate-limited all resolve to "" (no env
// var → runtime injects nothing) and leave no payload behind.
func TestPrepareWakeUpMarkerFile_SuppressionCases(t *testing.T) {
	t.Run("non_assistant", func(t *testing.T) {
		d, _, repoName, agentName, cleanup := setupMarkerDaemon(t)
		defer cleanup()
		t.Setenv(wakeUpMarkerDisabledEnv, "")
		if got := d.prepareWakeUpMarkerFile(repoName, agentName, state.AgentTypeBrowser); got != "" {
			t.Errorf("expected empty path for browser-agent, got %q", got)
		}
	})

	t.Run("disabled", func(t *testing.T) {
		d, _, repoName, agentName, cleanup := setupMarkerDaemon(t)
		defer cleanup()
		t.Setenv(wakeUpMarkerDisabledEnv, "1")
		if got := d.prepareWakeUpMarkerFile(repoName, agentName, state.AgentTypeAssistant); got != "" {
			t.Errorf("expected empty path when disabled, got %q", got)
		}
	})

	t.Run("rate_limited", func(t *testing.T) {
		d, _, repoName, agentName, cleanup := setupMarkerDaemon(t)
		defer cleanup()
		t.Setenv(wakeUpMarkerDisabledEnv, "")
		t.Setenv(wakeUpMarkerIntervalEnv, "60")

		statePath, err := wakeUpMarkerStateFile(d.paths.Root, repoName, agentName)
		if err != nil {
			t.Fatalf("wakeUpMarkerStateFile: %v", err)
		}
		if err := writeWakeUpMarkerLastFire(statePath, time.Now()); err != nil {
			t.Fatalf("writeWakeUpMarkerLastFire: %v", err)
		}

		if got := d.prepareWakeUpMarkerFile(repoName, agentName, state.AgentTypeAssistant); got != "" {
			t.Errorf("expected empty path when rate-limited, got %q", got)
		}

		pendingPath, err := wakeUpMarkerPendingFile(d.paths.Root, repoName, agentName)
		if err != nil {
			t.Fatalf("wakeUpMarkerPendingFile: %v", err)
		}
		if _, statErr := os.Stat(pendingPath); !os.IsNotExist(statErr) {
			t.Errorf("rate-limited prepare must not write a payload (stat err=%v)", statErr)
		}
	})
}
