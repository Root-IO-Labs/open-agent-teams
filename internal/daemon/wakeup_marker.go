package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

// Wake-up marker — daemon-enforced safeguard against assistant agents
// acting on stale rehydrated intent after a (re)spawn. Pairs with the
// "Stale-intent guard" rule in assistant.md as the load-bearing
// (daemon side) half of the defense; the prompt rule is defense-in-depth.
//
// Why daemon-side and not prompt-only: LLMs ignore prompt rules under
// context pressure. The 2026-05-28 smoke test showed an assistant
// rehydrating a "I was about to visit Wikipedia" intent on wake-up
// and acting on it autonomously, wedging itself at >100% effective
// context capacity. The daemon-injected PTY message is deterministic;
// the prompt rule is the safety net.
//
// Marker semantics:
//   - Prepended via PTY input on every (re)spawn of AgentTypeAssistant
//     agents (fresh spawn, auto-restart from health check, manual
//     restart from side panel, daemon-restart-driven adoption of an
//     alive process). Browser-agent (AgentTypeBrowser) is explicitly
//     scoped out — workflow helpers are designed for single-task
//     autonomous execution; suppressing post-restart action defeats
//     their purpose.
//   - Uses the [OAT-system] prefix the assistant is already conditioned
//     to trust (capacity hints, panic notices). Text is hardcoded; no
//     interpolation of user-supplied fields.
//   - Per-(repo, agent, pid) lifetime: in-memory sync.Map ensures the
//     marker fires at most once per process lifetime, even if multiple
//     spawn/restart paths race on the same window.
//   - Rate-limited per (repo, agent) via on-disk timestamp file at
//     ~/.oat/runtime/<repo>/<agent>/wakeup-marker.ts. Persists across
//     daemon restarts so a crash-loop doesn't spam markers (which
//     would themselves eat agent context).
//
// The on-disk format and rate-limit semantics deliberately mirror the
// oat-browser-agent bridge's bridge-restart-marker module so an
// operator familiar with one knows what to expect from the other.

// wakeUpMarkerDefaultIntervalMin is the default suppression window (in
// minutes) between consecutive marker firings for the same (repo,
// agent). Parallels the OAT_BRIDGE_RESTART_NOTICE_INTERVAL_MIN knob
// the bridge exposes — same operator surface, same rate-limit
// semantics.
const wakeUpMarkerDefaultIntervalMin = 10

// wakeUpMarkerIntervalEnv is the env var override for the rate-limit
// window. Accepts a non-negative integer (minutes); invalid or empty
// values fall back to wakeUpMarkerDefaultIntervalMin.
const wakeUpMarkerIntervalEnv = "OAT_ASSISTANT_WAKEUP_MARKER_INTERVAL_MIN"

// wakeUpMarkerDisabledEnv is the global escape hatch. When set to "1"
// the daemon skips the marker entirely on every spawn path. Documented
// as a dev/test-only override; the daemon emits a startup WARN when
// observed so a misconfigured production deployment is grep-friendly.
const wakeUpMarkerDisabledEnv = "OAT_ASSISTANT_WAKEUP_MARKER_DISABLED"

// wakeUpMarkerFileName is the per-(repo, agent) timestamp file name
// inside ~/.oat/runtime/<repo>/<agent>/. Suffix ".ts" stands for
// "timestamp" (parallel to bridge-restart-marker's ".marker"); body
// is a base-10 Unix-second integer.
const wakeUpMarkerFileName = "wakeup-marker.ts"

// wakeUpMarkerDirName is the top-level runtime directory under
// p.Root. Created on demand (parent dir does not exist in the standard
// EnsureDirectories set).
const wakeUpMarkerDirName = "runtime"

// wakeUpMarkerFiredKey is the in-process tracker that ensures a single
// (repo, agent, pid) sees the marker at most once per lifetime. Keyed
// on the canonical "repo/agent/pid" string. Values are unused
// (sync.Map present-only).
type wakeUpMarkerTracker struct {
	fired sync.Map
}

// newWakeUpMarkerTracker constructs a fresh tracker. Per-Daemon
// instance (not package-global) so daemon.go's New() can wire it
// alongside other daemon-scoped state, and tests can spin up
// independent daemons without leaking lifetime state across runs.
func newWakeUpMarkerTracker() *wakeUpMarkerTracker {
	return &wakeUpMarkerTracker{}
}

// markFired records that (repo, agent, pid) has had its marker
// injected this lifetime. Returns true if this is the first call for
// the key (caller should proceed with injection), false if the marker
// was already fired (caller should no-op). Atomic via LoadOrStore.
//
// Mechanism: the spawn paths (startRegisteredAgent, restartAgent,
// handleStartRepoAgents already-alive branch) all call into
// injectWakeUpMarker after the backend.StartAgent return; the
// markFired gate ensures a benign race between two spawn paths firing
// on the same window resolves to a single marker injection.
func (t *wakeUpMarkerTracker) markFired(repo, agent string, pid int) bool {
	key := wakeUpMarkerProcessKey(repo, agent, pid)
	_, loaded := t.fired.LoadOrStore(key, true)
	return !loaded
}

// forgetPID drops the in-memory "marker fired" record for a (repo,
// agent, pid) so a subsequent spawn with the SAME pid (extremely rare;
// would require OS pid recycling AND restart of the same window
// within the daemon's lifetime) starts clean. Called from the agent
// stop / cleanup paths in daemon.go if/when we wire it; for now, the
// tracker simply accumulates entries and resets on daemon restart
// (the rate-limit file is the persistent half).
func (t *wakeUpMarkerTracker) forgetPID(repo, agent string, pid int) {
	t.fired.Delete(wakeUpMarkerProcessKey(repo, agent, pid))
}

func wakeUpMarkerProcessKey(repo, agent string, pid int) string {
	return repo + "/" + agent + "/" + strconv.Itoa(pid)
}

// buildWakeUpMarker returns the literal PTY text the daemon prepends
// on (re)spawn. Hardcoded constant — NO interpolation of user-supplied
// fields beyond repo/agent (themselves trusted daemon state). The
// function MUST NOT be extended to interpolate any other field: the
// [OAT-system] prefix is the agent's "this comes from the daemon,
// trust it" signal, so injecting attacker-controlled text into the
// marker would invert the trust model.
//
// The repo/agent are included as a debugging affordance (operator
// tailing the log can see which agent received the marker). The agent
// itself is also told its own identity here so the rare cross-agent
// confusion case — operator looking at logs for agent A and seeing a
// marker that mentions agent B — is unambiguous.
func buildWakeUpMarker(repo, agent string) string {
	return fmt.Sprintf(
		"[OAT-system] You (%s/%s) just (re)started. Any text below in your conversation history is from a previous session. Do NOT call any tools or take any actions on the basis of that history. Wait for the next user message (which will arrive prefixed `[SIDE-PANEL CHAT] `) or the next inter-agent message before doing anything. If you find yourself about to act with no fresh trigger in the current process lifetime, stop and explain what you'd otherwise do — let the user confirm.",
		repo,
		agent,
	)
}

// wakeUpMarkerInterval returns the rate-limit window from the env
// override, falling back to wakeUpMarkerDefaultIntervalMin. Negative or
// non-numeric values are clamped to the default so a typo in the env
// var doesn't silently disable rate-limiting (which would degrade the
// crash-loop guarantee).
//
// Returning a time.Duration (not minutes) so the caller doesn't have
// to remember the unit. A value of 0 means "no rate limit" — every
// firing proceeds.
func wakeUpMarkerInterval() time.Duration {
	raw := strings.TrimSpace(os.Getenv(wakeUpMarkerIntervalEnv))
	if raw == "" {
		return time.Duration(wakeUpMarkerDefaultIntervalMin) * time.Minute
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return time.Duration(wakeUpMarkerDefaultIntervalMin) * time.Minute
	}
	return time.Duration(n) * time.Minute
}

// wakeUpMarkerDisabled reports whether the global escape hatch is set.
// Read fresh each call so a test can flip the env var without
// re-instantiating the daemon. Only the literal "1" disables —
// anything else (including "true", "yes", empty) leaves the marker
// active. Matches the explicit-opt-out semantics of the other
// OAT_* disable knobs.
func wakeUpMarkerDisabled() bool {
	return os.Getenv(wakeUpMarkerDisabledEnv) == "1"
}

// sanitizeWakeUpMarkerPathSegment rejects path components that could
// escape the per-(repo, agent) runtime directory or smuggle injection
// into the filesystem layer:
//
//   - empty string
//   - any '/' (would let an attacker steer the rate-limit file at an
//     arbitrary subdirectory)
//   - '..' anywhere in the segment (parent-traversal)
//   - NUL byte (would truncate the path at the C library boundary,
//     creating a confused-deputy primitive)
//
// All segments inside ~/.oat/runtime/<repo>/<agent>/ come from daemon-
// trusted state (repo name from oat init / oat clone, agent name from
// oat agent add or the assistant virtual-repo bootstrap). The
// sanitization is defense-in-depth — if state.json itself is
// corrupted, we still refuse to write outside the intended subtree.
//
// Returns the segment unchanged on success, or an error suitable for
// logging at WARN level when the marker is skipped.
func sanitizeWakeUpMarkerPathSegment(seg string) (string, error) {
	if seg == "" {
		return "", fmt.Errorf("wake-up marker path segment is empty")
	}
	if strings.ContainsRune(seg, '/') {
		return "", fmt.Errorf("wake-up marker path segment %q contains '/'", seg)
	}
	if strings.Contains(seg, "..") {
		return "", fmt.Errorf("wake-up marker path segment %q contains '..'", seg)
	}
	if strings.ContainsRune(seg, '\x00') {
		return "", fmt.Errorf("wake-up marker path segment %q contains NUL byte", seg)
	}
	return seg, nil
}

// wakeUpMarkerStateFile resolves the per-(repo, agent) timestamp file
// path under p.Root/runtime/<repo>/<agent>/wakeup-marker.ts. Returns
// an error if either segment fails sanitization; the marker should
// not fire (the rate-limit file is the source of truth and we'd
// rather refuse than silently fail open).
//
// Note: we deliberately do NOT MkdirAll here so callers can decide
// whether to create the directory (write path) or not (read path).
// Reads on a missing directory return "never fired" via
// readWakeUpMarkerLastFire's os.IsNotExist handling.
func wakeUpMarkerStateFile(root, repo, agent string) (string, error) {
	repoSeg, err := sanitizeWakeUpMarkerPathSegment(repo)
	if err != nil {
		return "", err
	}
	agentSeg, err := sanitizeWakeUpMarkerPathSegment(agent)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, wakeUpMarkerDirName, repoSeg, agentSeg, wakeUpMarkerFileName), nil
}

// readWakeUpMarkerLastFire returns the last-fire timestamp parsed
// from the rate-limit file, or the zero time if the file does not
// exist. Errors other than NotExist are logged-and-returned as zero
// (fail-open: refuse to suppress the marker if we can't read the
// state file — pollution from a single extra marker is strictly less
// harmful than a missing one).
//
// Note on threat model: an attacker with write access to
// ~/.oat/runtime/ already implies write access to spawn arbitrary
// processes (it's the daemon's own runtime dir). A malicious
// timestamp file could either (a) bypass the marker by writing a
// future-dated timestamp, or (b) DoS the agent's context by deleting
// the file to force the marker every spawn. Documented in
// THREAT_MODEL.md as a lost-cause threat — anyone who can write here
// owns the daemon.
func readWakeUpMarkerLastFire(path string) (time.Time, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return time.Time{}, nil
	}
	sec, parseErr := strconv.ParseInt(trimmed, 10, 64)
	if parseErr != nil {
		return time.Time{}, fmt.Errorf("parse wake-up marker timestamp %q: %w", trimmed, parseErr)
	}
	return time.Unix(sec, 0), nil
}

// writeWakeUpMarkerLastFire writes the timestamp atomically via .tmp
// + rename so a daemon crash mid-write cannot leave the file in an
// unparseable state.
func writeWakeUpMarkerLastFire(path string, t time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create wake-up marker dir: %w", err)
	}
	tmp := path + ".tmp"
	contents := strconv.FormatInt(t.Unix(), 10) + "\n"
	if err := os.WriteFile(tmp, []byte(contents), 0o644); err != nil {
		return fmt.Errorf("write wake-up marker tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename wake-up marker tmp: %w", err)
	}
	return nil
}

// WakeUpMarkerTrigger names the spawn path that asked for a marker.
// Surfaces in the audit-log line for postmortem analysis; "rate-
// limited" is the synthetic value used when the marker was suppressed.
type WakeUpMarkerTrigger string

const (
	// WakeUpMarkerTriggerFresh is a brand-new spawn — startRegisteredAgent
	// after a fresh `oat agent add` or after a daemon-restart that
	// discovered the agent's PID was 0 (process gone).
	WakeUpMarkerTriggerFresh WakeUpMarkerTrigger = "fresh"

	// WakeUpMarkerTriggerRestart covers BOTH operator-driven restarts
	// (handleRestartAgent / handleRestartBrowserAgent) AND health-
	// check auto-restarts (the daemon's stuck-agent loops feed into
	// the same restartAgent function). The auto vs manual distinction
	// is not load-bearing — both produce a fresh process with
	// rehydrated history, which is exactly what the marker guards
	// against. Operators distinguishing the two can correlate with
	// the surrounding daemon-log lines (the health-check loop's WARN
	// line precedes a restart in the auto case; the
	// handleRestart{Agent,BrowserAgent} INFO line precedes a restart
	// in the manual case).
	WakeUpMarkerTriggerRestart WakeUpMarkerTrigger = "restart"

	// WakeUpMarkerTriggerDaemonRestart is the daemon-startup path
	// covering the "agent process survived the daemon restart and was
	// re-adopted" case. Fires once per daemon-boot per such agent.
	WakeUpMarkerTriggerDaemonRestart WakeUpMarkerTrigger = "daemon-restart"

	// WakeUpMarkerTriggerRateLimited is the synthetic value written to
	// the audit-log line when a real trigger fires but the rate-limit
	// suppresses the actual PTY write. Lets observability tools detect
	// crash-loop patterns (frequent rate-limited firings imply
	// repeated respawns).
	WakeUpMarkerTriggerRateLimited WakeUpMarkerTrigger = "rate-limited"
)

// injectWakeUpMarker is the single entry point for the wake-up marker
// system. Callers pass the agent's current state.Agent record
// (already updated with the new PID, if any) and the trigger that
// caused the injection.
//
// Behavior contract:
//
//  1. No-op for any agent type other than AgentTypeAssistant. Browser-
//     agents (AgentTypeBrowser) are intentionally excluded — workflow-
//     helper semantics. Other types never reach this code in practice
//     (workers/supervisors/etc. don't use the bridge), but the
//     type-check makes the assistant scope grep-friendly and prevents
//     a future PR from accidentally widening the marker.
//  2. No-op if OAT_ASSISTANT_WAKEUP_MARKER_DISABLED=1 (with a one-
//     line INFO at the call site so the operator can see why the
//     marker didn't fire).
//  3. No-op if the in-process tracker already recorded a firing for
//     (repo, agent, pid) this lifetime. Prevents two racing spawn
//     paths from each firing the marker on the same window.
//  4. Rate-limit consult: if the last on-disk firing was within the
//     OAT_ASSISTANT_WAKEUP_MARKER_INTERVAL_MIN window, log the
//     suppression with trigger=rate-limited and return WITHOUT
//     sending the PTY message. The in-process tracker is still
//     marked-fired so a subsequent spawn within the same window
//     doesn't repeatedly hit the file.
//  5. Otherwise: send the marker via backend.SendMessage, update the
//     rate-limit file, log INFO with the real trigger.
//
// Returns no error — best-effort. The marker is defense-in-depth (the
// prompt rule is the agent-side half); a failed marker injection
// must not block agent startup. All failures are logged at WARN with
// enough context to debug.
func (d *Daemon) injectWakeUpMarker(repoName, agentName string, agent state.Agent, trigger WakeUpMarkerTrigger) {
	if agent.Type != state.AgentTypeAssistant {
		return
	}

	if wakeUpMarkerDisabled() {
		d.logger.Info(
			"wake-up marker skipped for %s/%s (trigger=%s): %s=1",
			repoName, agentName, trigger, wakeUpMarkerDisabledEnv,
		)
		return
	}

	pid := agent.PID
	if pid <= 0 {
		d.logger.Warn(
			"wake-up marker skipped for %s/%s (trigger=%s): agent PID is zero (likely test mode); marker only meaningful for live processes",
			repoName, agentName, trigger,
		)
		return
	}

	// In-process gate: only the first spawn-path caller for this pid
	// proceeds. Subsequent callers see the in-memory record and no-op.
	if !d.wakeUpMarkers.markFired(repoName, agentName, pid) {
		d.logger.Debug(
			"wake-up marker skipped for %s/%s pid=%d (trigger=%s): already fired this process lifetime",
			repoName, agentName, pid, trigger,
		)
		return
	}

	// Compute the rate-limit file path. Sanitization failure here is
	// a hard skip — better to drop the marker than write to an
	// attacker-controlled path. The in-process gate above already
	// recorded the firing, so a sanitization failure does not re-fire
	// on a subsequent spawn (which is correct: a path violation is a
	// repo/agent name corruption, not a transient).
	statePath, err := wakeUpMarkerStateFile(d.paths.Root, repoName, agentName)
	if err != nil {
		d.logger.Warn(
			"wake-up marker skipped for %s/%s pid=%d (trigger=%s): rate-limit path sanitization failed: %v",
			repoName, agentName, pid, trigger, err,
		)
		return
	}

	interval := wakeUpMarkerInterval()
	now := time.Now()

	lastFire, readErr := readWakeUpMarkerLastFire(statePath)
	if readErr != nil {
		// Fail-open: log but proceed with the marker. A corrupted /
		// unreadable timestamp file is strictly less harmful than a
		// missing marker — the marker itself costs ~one line of
		// context, the missing marker costs an autonomous Wikipedia
		// rabbit hole.
		d.logger.Warn(
			"wake-up marker for %s/%s pid=%d (trigger=%s): rate-limit read failed (%v); proceeding without suppression",
			repoName, agentName, pid, trigger, readErr,
		)
	}

	if !lastFire.IsZero() && interval > 0 && now.Sub(lastFire) < interval {
		d.logger.Info(
			"wake-up marker rate-limited for %s/%s pid=%d (real_trigger=%s, last_fire=%s, interval=%s)",
			repoName, agentName, pid, trigger, lastFire.Format(time.RFC3339), interval,
		)
		return
	}

	// Look up the window name from current state. The agent.WindowName
	// passed in may be stale across a restart (the new process may
	// have a different window); reading fresh is cheap and avoids the
	// "marker sent to a dead window" failure mode.
	fresh, exists := d.state.GetAgent(repoName, agentName)
	if !exists {
		d.logger.Warn(
			"wake-up marker skipped for %s/%s pid=%d (trigger=%s): agent vanished from state between spawn and marker injection",
			repoName, agentName, pid, trigger,
		)
		return
	}

	repo, repoExists := d.state.GetRepo(repoName)
	if !repoExists {
		d.logger.Warn(
			"wake-up marker skipped for %s/%s pid=%d (trigger=%s): repo vanished from state between spawn and marker injection",
			repoName, agentName, pid, trigger,
		)
		return
	}

	marker := buildWakeUpMarker(repoName, agentName)
	if err := d.backend.SendMessage(d.ctx, repo.SessionName, fresh.WindowName, marker); err != nil {
		d.logger.Warn(
			"wake-up marker send failed for %s/%s pid=%d (trigger=%s): %v",
			repoName, agentName, pid, trigger, err,
		)
		return
	}

	if err := writeWakeUpMarkerLastFire(statePath, now); err != nil {
		// Non-fatal: marker was already sent. Log so a recurring
		// pattern surfaces (e.g. permission issue on the runtime
		// dir).
		d.logger.Warn(
			"wake-up marker timestamp write failed for %s/%s pid=%d (trigger=%s): %v",
			repoName, agentName, pid, trigger, err,
		)
	}

	d.logger.Info(
		"wakeup_marker_injected: repo=%s agent=%s pid=%d trigger=%s",
		repoName, agentName, pid, trigger,
	)
}
