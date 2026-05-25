package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/agents"
	"github.com/Root-IO-Labs/open-agent-teams/internal/diagnostics"
	"github.com/Root-IO-Labs/open-agent-teams/internal/hooks"
	"github.com/Root-IO-Labs/open-agent-teams/internal/logging"
	"github.com/Root-IO-Labs/open-agent-teams/internal/messages"
	"github.com/Root-IO-Labs/open-agent-teams/internal/prompts"
	"github.com/Root-IO-Labs/open-agent-teams/internal/routing"
	"github.com/Root-IO-Labs/open-agent-teams/internal/socket"
	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
	"github.com/Root-IO-Labs/open-agent-teams/internal/templates"
	"github.com/Root-IO-Labs/open-agent-teams/internal/version"
	"github.com/Root-IO-Labs/open-agent-teams/internal/worktree"
	agent_pkg "github.com/Root-IO-Labs/open-agent-teams/pkg/agent"
	backend_pkg "github.com/Root-IO-Labs/open-agent-teams/pkg/backend"
	"github.com/Root-IO-Labs/open-agent-teams/pkg/config"
)

// ─── Function index (approximate line numbers) ──────────────────
//
// Daemon struct + constructor:    ~72   (Daemon, New)
// Start / Stop / Run:             ~246  (Start, Stop, periodicLoop)
// PR monitoring:                  ~513  (prMonitorLoop, checkSingleWorkerPR)
// Health checks:                  ~664  (checkAgentHealth, pruneOrphans)
// Message routing:                ~816  (messageRouterLoop, routeMessages)
// Wake / nudge:                   ~997  (wakeLoop, wakeAgents, nudgeIntervalFor)
// Idle mode:                      ~1057 (idle transitions, final nudges)
// Request handling:               ~1604 (handleRequest switch — all socket commands)
// Worker lifecycle:               ~1970 (handleStartWorker, handleSendAgentInput)
// Repo agent startup:             ~2875 (handleStartRepoAgents, startRegisteredAgent)
// Agent restart / remove:         ~3313 (handleRestartAgent, handleRemoveAgent)
// Agent definitions:              ~4558 (supervisorDefinitionBodyForMessage, sendAgentDefinitionsToSupervisor)
// Binary resolution:              ~4702 (getAgentBinaryPath, ensureBinSymlinks)
// Prompt assembly:                ~5173 (writePromptFileWithPrefix, writePromptFile)
// Helpers:                        ~5500 (env vars, session names, state snapshot)
//
// ─────────────────────────────────────────────────────────────────

// workerPostCompletionDelay is the grace period after a worker signals completion
// before its window is cleaned up. Kept short (3s) to prevent completed agents
// from burning tokens running commands after they're done.
const workerPostCompletionDelay = 3 * time.Second

// shutdownDrainTimeout bounds how long an output-watcher consumer goroutine
// will wait to drain buffered token events after the daemon context is
// canceled. Prevents up-to-32-events-per-agent token spend loss on shutdown
// while keeping daemon stop snappy.
const shutdownDrainTimeout = 1 * time.Second

// fetchFailureThreshold is the number of consecutive fetch failures before
// the daemon stops trying to fetch for a repo (until daemon restart).
const fetchFailureThreshold = 3

// defaultMaxTokens is the fallback output-token limit passed via --model-params
// to every agent. It prevents large tool calls (e.g. write_file with big content)
// from being silently truncated when the model's output token limit is too low.
// Per-model overrides come from ModelProfile.Runtime.MaxTokens, and when that
// is zero this default is used. Profiles can tune this value independently.
const defaultMaxTokens = 32000

// modelParamsJSON returns the raw JSON string passed via --model-params for a
// given model, honoring the per-profile MaxTokens override when non-zero.
// Keep the JSON simple; backends handle shell quoting when constructing commands.
func (d *Daemon) modelParamsJSON(modelID string) string {
	maxTokens := defaultMaxTokens
	if d.modelProfiles != nil && modelID != "" {
		if p := d.modelProfiles.Get(modelID); p != nil && p.Runtime.MaxTokens > 0 {
			maxTokens = p.Runtime.MaxTokens
		}
	}
	return fmt.Sprintf(`{"max_tokens":%d}`, maxTokens)
}

// denyToolArgs returns the `--deny-tool NAME` argv pairs that must be appended
// to the oat-agent CLI invocation for the given agent type.
//
// The browser-agent gets a hardcoded deny list:
//   - task: spawns long-running subagents that hit the CDP timeout and leave
//     the parent stuck "processing" with no recovery path (the "iana mystery"
//     bug). Browser agents have no need for subagents — they delegate to the
//     extension, not to other LLMs.
//   - http_request / fetch_url: bypass the MCP bridge entirely and grab HTML
//     directly via the Python process. That defeats the whole point of a
//     browser agent (cookies, JS execution, login state) and routes around
//     the side-panel activity log.
//   - compact_conversation: manual compaction tool is exposed via
//     SummarizationToolMiddleware; gating it here keeps the browser-agent's
//     short-session model from offering a destructive command users can't
//     undo from inside the side panel.
//
// Other agent types (worker, supervisor, merge-queue, review, verification,
// pr-shepherd) keep the full tool catalog. Returns nil for those so callers
// can `append(args, denyToolArgs(t)...)` unconditionally.
// usesBrowserBridge returns true for agent types that spawn (or coexist
// with) an oat-browser-agent bridge process. Today both AgentTypeBrowser
// (workflow helpers) and AgentTypeAssistant (Part 5a personal assistants)
// share the bridge — same MCP wiring via buildBrowserAgentMCPConfig, same
// bridge-unreachable back-off semantics, same assistant-turn tailer for
// side-panel chat. Callers should branch on this helper, NOT a raw
// `agent.Type == state.AgentTypeBrowser` comparison, when their concern
// is "is there a bridge involved?" rather than "is this specifically the
// goal-driven workflow helper?". See Part 5a of the
// side-panel-chat-and-status plan for the call-site audit.
func usesBrowserBridge(t state.AgentType) bool {
	return t == state.AgentTypeBrowser || t == state.AgentTypeAssistant
}

func denyToolArgs(agentType state.AgentType) []string {
	switch agentType {
	case state.AgentTypeBrowser:
		return []string{
			"--deny-tool", "task",
			"--deny-tool", "http_request",
			"--deny-tool", "fetch_url",
			"--deny-tool", "compact_conversation",
		}
	case state.AgentTypeAssistant:
		// Part 5a + 5e: the assistant shares browser's "no task /
		// no raw http" posture (its tool surface is bridge-mediated
		// browsing, not orchestration or raw fetch), BUT it MUST
		// retain `compact_conversation` because the entire 75/85/90/95
		// capacity-tier mechanism in 5e relies on the agent (or the
		// daemon, at 95% under OAT_CONTEXT_SAFETY_NET) being able to
		// invoke it. Stripping it would silently disarm the
		// safety net.
		return []string{
			"--deny-tool", "task",
			"--deny-tool", "http_request",
			"--deny-tool", "fetch_url",
		}
	default:
		return nil
	}
}

// Daemon represents the main daemon process
type Daemon struct {
	paths   *config.Paths
	state   *state.State
	backend backend_pkg.ProcessBackend
	logger  *logging.Logger
	server  *socket.Server
	pidFile *PIDFile
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	fetchFailures   map[string]int // repo name -> consecutive fetch failure count
	fetchFailuresMu sync.Mutex
	// workspaceActivity is ONLY accessed from healthCheckLoop. It has no mutex
	// on purpose — the loop is single-goroutine and serializes iterations. If
	// a second goroutine ever needs to touch it, add a mutex AND restructure
	// checkWorkspaceHealth to copy-under-lock / mutate / swap-under-lock so
	// the struct fields stay race-free alongside the map.
	workspaceActivity map[string]*workspaceActivity // repo name -> workspace activity tracking

	coreAgentActivity   map[string]*coreAgentActivity // "repo/agent" -> core agent activity tracking
	coreAgentActivityMu sync.Mutex

	prGreenNotified   map[string]bool // "repo/prNum" -> already notified merge-queue
	prGreenNotifiedMu sync.Mutex

	conflictNotified   map[string]bool // "repo/prNum" -> already notified about merge conflict
	conflictNotifiedMu sync.Mutex

	ciFailureNotified   map[string]bool // "repo/prNum" -> already notified active worker about CI failure
	ciFailureNotifiedMu sync.Mutex

	completionNotified   map[string]bool // "repo/agent" -> already sent completion msg to merge-queue
	completionNotifiedMu sync.Mutex

	routeMessagesMu      sync.Mutex
	routeMessagesTrigger chan struct{} // buffered(1) channel to coalesce routeMessages requests
	prMonitorTrigger     chan struct{} // buffered(1) — fires when a worker goes dormant or needs PR check
	wakeTrigger          chan struct{} // buffered(1) — fires when a worker is created or woken

	workspaceActivityMu sync.Mutex // protects workspaceActivity map

	prWakeCount     map[string]int  // "repo/agent" -> conflict/CI wake count for circuit breaker
	prWakeEscalated map[string]bool // "repo/agent" -> supervisor already notified
	prWakeCountMu   sync.Mutex

	verificationTimeoutNotified   map[string]time.Time // "repo/agent" -> when soft-timeout notification was sent
	verificationTimeoutNotifiedMu sync.Mutex

	mainCIAlertTime   map[string]time.Time // repo -> last main CI red alert time (dedup)
	mainCIAlertTimeMu sync.Mutex

	restartCooldown   map[string]time.Time // "repo/agent" -> last restart attempt time
	restartCooldownMu sync.Mutex

	// bridgeUnreachable tracks consecutive health-check failures for
	// browser-agent so the daemon can stop respawning a doomed bridge
	// subprocess every 2-min cycle when Chrome is closed or the
	// extension is uninstalled. Each entry is a sliding window of
	// recent failure timestamps for "<repo>/<agent>"; once the window
	// holds >= bridgeUnreachableThreshold entries within
	// bridgeUnreachableWindow, the daemon disables auto-restart for
	// that agent until the user explicitly re-engages via
	// `oat agent restart`. See Part 2 of
	// mcp-and-opt-in-browser-agent_a10544be.plan.md.
	bridgeUnreachable   map[string][]time.Time
	bridgeUnreachableMu sync.Mutex

	// assistantTurnTailers maps "<session>/<agent>" → per-agent log
	// tailer that watches OAT_TOOL_LOG, parses ASSISTANT blocks, and
	// fans them out to stream_assistant_turns subscribers. Created
	// when a browser-agent starts (see startRegisteredAgent's
	// AgentTypeBrowser branch), torn down when the agent exits.
	// Implements the Option-E auto-emit path from Part 2g so side-panel
	// chat replies don't depend on the model calling
	// browser_emit_to_user.
	assistantTurnTailers   map[string]*assistantTurnTailer
	assistantTurnTailersMu sync.Mutex

	modelProfiles     *routing.ProfileStore    // loaded model capability profiles for routing
	outcomeLogger     *routing.OutcomeLogger   // appends per-completion records to routing-history.jsonl
	prBackfiller      *routing.PRBackfiller    // observes PR state at lag buckets after completion (sidecar writer)
	oatVersion        string                   // build-time release identity, snapshotted on New() so logOutcome doesn't recompute
	pricingSnapshotID string                   // sha-prefix of pricing registry at boot, snapshotted after pricing load
	pricing           *routing.PricingRegistry // embedded pricing YAML for cost-aware routing (Router V1)

	// V2 router corpus snapshot. Built at startup from the joined corpus
	// (main + sidecar), refreshed every corpusIndexRefreshInterval. Atomic
	// pointer so reads (per-spawn routing decisions) don't lock against
	// the periodic refresher.
	corpusIndex   *routing.CorpusIndex
	corpusIndexMu sync.RWMutex
}

// corpusIndexRefreshInterval — how often the daemon rebuilds the V2
// router's corpus snapshot. Routing decisions read the cached snapshot,
// so the freshness/cost trade-off is bounded: 10 min lag at most before a
// new merged-PR observation feeds into routing.
const corpusIndexRefreshInterval = 10 * time.Minute

func backendModeName(backend backend_pkg.ProcessBackend) string {
	if info, ok := backend.(backend_pkg.BackendInfo); ok {
		return info.Name()
	}
	if v := strings.TrimSpace(os.Getenv("OAT_BACKEND")); v != "" {
		return v
	}
	return ""
}

func (d *Daemon) isDirectBackend() bool {
	return backendModeName(d.backend) == "direct"
}

// New creates a new daemon instance
func New(paths *config.Paths) (*Daemon, error) {
	// Ensure directories exist
	if err := paths.EnsureDirectories(); err != nil {
		return nil, fmt.Errorf("failed to create directories: %w", err)
	}

	// Initialize logger
	logger, err := logging.NewFile(paths.DaemonLog)
	if err != nil {
		return nil, fmt.Errorf("failed to create logger: %w", err)
	}

	// Load or create state
	st, err := state.Load(paths.StateFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load state: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	be := backend_pkg.NewBackend(os.Getenv("OAT_BACKEND"), paths.Root)
	d := &Daemon{
		paths:                       paths,
		state:                       st,
		backend:                     be,
		logger:                      logger,
		pidFile:                     NewPIDFile(paths.DaemonPID),
		ctx:                         ctx,
		cancel:                      cancel,
		fetchFailures:               make(map[string]int),
		workspaceActivity:           make(map[string]*workspaceActivity),
		coreAgentActivity:           make(map[string]*coreAgentActivity),
		prGreenNotified:             make(map[string]bool),
		conflictNotified:            make(map[string]bool),
		ciFailureNotified:           make(map[string]bool),
		completionNotified:          make(map[string]bool),
		routeMessagesTrigger:        make(chan struct{}, 1),
		prMonitorTrigger:            make(chan struct{}, 1),
		wakeTrigger:                 make(chan struct{}, 1),
		prWakeCount:                 make(map[string]int),
		prWakeEscalated:             make(map[string]bool),
		verificationTimeoutNotified: make(map[string]time.Time),
		mainCIAlertTime:             make(map[string]time.Time),
		restartCooldown:             make(map[string]time.Time),
		bridgeUnreachable:           make(map[string][]time.Time),
		assistantTurnTailers:        make(map[string]*assistantTurnTailer),
	}

	// Load model profiles for routing (non-fatal if missing).
	// Pass the daemon logger in via an adapter so per-file diagnostics (skipped
	// YAML, missing-field rejections, load-count summary) show up in
	// daemon.log instead of being silently dropped. The routing package emits
	// the load-count INFO itself; daemon.go only needs to surface the
	// operator-actionable "empty store" case as a WARN.
	profiles, err := routing.NewProfileStoreWithLogger(paths.ModelProfilesDir, newRoutingLogger(logger))
	if err != nil {
		logger.Warn("Failed to load model profiles: %v", err)
	} else {
		d.modelProfiles = profiles
		// Attach the embedded context-registry BEFORE the first Reload so the
		// initial load picks up overrides. Un-probed max_input_tokens values
		// get filled from the registry for providers whose probe fallback
		// doesn't work (openai/google_genai/ollama — see Phase 5 audit).
		reg := routing.LoadEmbeddedContextRegistry()
		profiles.SetContextRegistry(reg)
		if reg != nil && reg.Count() > 0 {
			logger.Info("Context-registry loaded (%d entries) — will fill missing max_input_tokens on profile reload", reg.Count())
		}
		// Re-run load so the registry overrides take effect on startup.
		// Failures here are non-fatal; the already-loaded profiles remain.
		if reloadErr := profiles.Reload(); reloadErr != nil {
			logger.Warn("Profile reload after context-registry attach failed: %v", reloadErr)
		}
		if profiles.IsEmpty() {
			logger.Warn("No model profiles loaded from %s; agents will use explicit --model or passthrough routing (run: oat model onboard <provider:model>)", paths.ModelProfilesDir)
		}
	}

	// Routing outcome logger — appends one JSONL line per worker/review completion
	// so the replay harness in benchmarks/routing-replay can compute counterfactual
	// $/success. Failures in this path are logged but never block completion.
	historyPath := routing.DefaultOutcomeHistoryPath(paths.Root)

	// One-shot v1→v2 migration: lifts schema_version=1 records (or older
	// records with no schema_version field) to v2, enriching with provider,
	// model_canonical, task_features, and a stable record_id derived via
	// UUIDv5 from the record's natural key. Idempotent — re-running on an
	// already-migrated file is a no-op. Original is backed up exactly once
	// to <path>.v1.bak.jsonl. Failures here log a warning but do not block
	// daemon startup; corpus quality is best-effort.
	if migStats, migErr := routing.MigrateV1ToV2(historyPath); migErr != nil {
		logger.Warn("Routing-history v1→v2 migration failed: %v — corpus may have mixed schemas", migErr)
	} else if migStats.V1Migrated > 0 {
		logger.Info("Routing-history migrated %d v1 record(s) to v2 (backup: %s)", migStats.V1Migrated, migStats.BackupPath)
	}

	d.outcomeLogger = routing.NewOutcomeLogger(
		historyPath,
		func(format string, args ...any) { logger.Warn(format, args...) },
	)

	// PR-state backfiller (Phase 1, schema v2) — at fixed lag buckets after a
	// worker completes (1h, 24h, 7d), observes the PR's merge state via gh
	// and writes one append-only sidecar entry. Sidecar lives next to the
	// main routing-history.jsonl; the main file stays strictly append-only
	// and immutable. The Phase 2 indexer will join the two on (ts,worker,repo).
	d.prBackfiller = routing.NewPRBackfiller(routing.PRBackfillerOptions{
		HistoryPath: routing.DefaultOutcomeHistoryPath(paths.Root),
		SidecarPath: routing.DefaultBackfillSidecarPath(paths.Root),
		Warn:        func(format string, args ...any) { logger.Warn(format, args...) },
	})

	// Pricing registry — used by RouteForTask when OAT_ROUTING_V1=1, and by
	// budget cost computation. The loader prefers a fresh LiteLLM-derived
	// cache at $OAT_HOME/pricing-cache.json, falling back to the embedded
	// YAML shipped in the binary. A background goroutine refreshes the
	// cache every 24h so prices stay current without requiring a release.
	// Disable by setting OAT_PRICING_REMOTE=0 (uses embedded YAML only).
	pricingOpts := routing.NewPricingLoaderOptions(paths.Root)
	pricingOpts.Log = func(format string, args ...any) { logger.Info(format, args...) }
	if v := strings.TrimSpace(os.Getenv("OAT_PRICING_REMOTE")); v == "0" || strings.EqualFold(v, "false") {
		pricingOpts.Disabled = true
		logger.Info("Pricing remote refresh disabled via OAT_PRICING_REMOTE=0; using embedded YAML only")
	}
	d.pricing = routing.LoadPricingWithRemote(pricingOpts)
	if d.pricing.Count() > 0 {
		logger.Info("Pricing registry loaded (%d models) — cost-aware routing available via OAT_ROUTING_V1=1", d.pricing.Count())
	}

	// Schema v2 identity snapshot — captured once so logOutcome doesn't pay
	// for recomputation on every completion. Both fields decorate every
	// OutcomeRecord written by this daemon process. If pricing reloads
	// mid-run (LiteLLM background refresh), the snapshot deliberately stays
	// pinned to the boot value — that's the correct behavior since the
	// records' cost basis was set at decision time, not at log time.
	d.oatVersion = formatOATVersion(version.Current())
	d.pricingSnapshotID = d.pricing.SnapshotID()

	// V2 router corpus snapshot — built once at startup so the first
	// routing decision after daemon start doesn't pay the full corpus
	// scan. Refreshed periodically by Start(). Failure here is silent;
	// V2 just falls through to no-corpus behavior, which is V1-quality.
	d.refreshCorpusIndex()

	// Create socket server with streaming support
	sh := &streamHandler{d: d}
	d.server = socket.NewServer(paths.DaemonSock, socket.HandlerFunc(d.handleRequest), socket.WithStreamHandler(sh))

	return d, nil
}

// formatOATVersion renders version.Info into the compact "<semver> (<sha>)"
// shape stored on every OutcomeRecord. Strips the "(date)" segment because
// the date isn't useful for grouping and the sha already pins the build.
func formatOATVersion(v version.Info) string {
	if v.IsDev || v.Version == "" {
		if v.Commit != "" && v.Commit != "none" {
			return "dev (" + v.Commit + ")"
		}
		return "dev"
	}
	if v.Commit != "" && v.Commit != "none" {
		return v.Version + " (" + v.Commit + ")"
	}
	return v.Version
}

// Start starts the daemon
func (d *Daemon) Start() error {
	d.logger.Info("Starting daemon")

	// Check and claim PID file
	if err := d.pidFile.CheckAndClaim(); err != nil {
		return err
	}

	// Clean up stale sidecar sockets from previous daemon runs. The PID
	// claim above guarantees no other daemon is alive for this user, so
	// any existing /tmp/oat-sdcr-*.sock files belong to a dead daemon.
	// Without this, crashed-daemon artifacts accumulate in /tmp and
	// eventually trigger filesystem warnings.
	if removed := cleanStaleSidecarSockets(); removed > 0 {
		d.logger.Info("Cleaned %d stale sidecar socket(s) from previous run", removed)
	}

	// Start socket server
	if err := d.server.Start(); err != nil {
		return fmt.Errorf("failed to start socket server: %w", err)
	}

	d.logger.Info("Socket server started at %s", d.paths.DaemonSock)

	d.logger.Info("Daemon started successfully")

	// Ensure ~/.oat/bin/ has symlinks to the real binaries so agents
	// get a neutral PATH entry that won't be mistaken for a project dir.
	d.ensureBinSymlinks()

	// Log system diagnostics for monitoring and debugging
	d.logDiagnostics()

	// Start server loop first so CLI commands (e.g. oat repo use) are accepted during restore.
	// Otherwise restoreTrackedRepos() can take many seconds and clients hit read timeout.
	d.wg.Add(1)
	go d.serverLoop()

	// Restore agents for tracked repos BEFORE starting health checks
	// This prevents race conditions where health check cleans up agents being restored
	d.restoreTrackedRepos()

	// Start remaining core loops after restore completes
	d.wg.Add(4)
	go d.healthCheckLoop()
	go d.messageRouterLoop()
	go d.wakeLoop()
	go d.prMonitorLoop()

	// Routing-history PR backfiller (schema v2). Runs on its own ticker;
	// d.cancel() in Stop() unwinds the loop and d.wg.Wait() blocks until it
	// returns. No-op if NewPRBackfiller returned nil (which it won't here).
	if d.prBackfiller != nil {
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			d.prBackfiller.Run(d.ctx)
		}()
	}

	// V2 router corpus refresher — every corpusIndexRefreshInterval, rebuild
	// the in-memory snapshot from disk so newly-completed records and fresh
	// PR-merge observations from the backfiller's sidecar feed into routing.
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		t := time.NewTicker(corpusIndexRefreshInterval)
		defer t.Stop()
		for {
			select {
			case <-d.ctx.Done():
				return
			case <-t.C:
				d.refreshCorpusIndex()
			}
		}
	}()

	return nil
}

// refreshCorpusIndex rebuilds the V2 router's corpus snapshot. Called once
// at New() and periodically by the goroutine in Start(). Reads the joined
// corpus (main + sidecar) so historical success rates reflect the latest
// PR-merge observations.
//
// Failures are logged at debug — V2 keeps working with the previous snapshot
// (or with nil, which falls through to no-historical-data behavior).
func (d *Daemon) refreshCorpusIndex() {
	historyPath := routing.DefaultOutcomeHistoryPath(d.paths.Root)
	sidecarPath := routing.DefaultBackfillSidecarPath(d.paths.Root)

	records, _, err := routing.LoadCorpusJoined(historyPath, sidecarPath)
	if err != nil {
		if d.logger != nil {
			d.logger.Debug("V2 corpus refresh failed: %v (using previous snapshot)", err)
		}
		return
	}
	idx := routing.BuildCorpusIndex(records)

	d.corpusIndexMu.Lock()
	d.corpusIndex = idx
	d.corpusIndexMu.Unlock()
}

// getCorpusIndex returns the current V2 router corpus snapshot. Lock-light
// for the per-spawn read path. May return nil during the brief window
// between New() and the first refresh — V2 routing handles nil gracefully.
func (d *Daemon) getCorpusIndex() *routing.CorpusIndex {
	d.corpusIndexMu.RLock()
	defer d.corpusIndexMu.RUnlock()
	return d.corpusIndex
}

// Wait waits for the daemon to shut down
func (d *Daemon) Wait() {
	d.wg.Wait()
}

// GetState returns the daemon's state (for testing)
func (d *Daemon) GetState() *state.State {
	return d.state
}

// GetPaths returns the daemon's paths (for testing)
func (d *Daemon) GetPaths() *config.Paths {
	return d.paths
}

// GetBackend returns the daemon's process backend (for testing)
func (d *Daemon) GetBackend() backend_pkg.ProcessBackend {
	return d.backend
}

// TriggerHealthCheck triggers an immediate health check (for testing)
func (d *Daemon) TriggerHealthCheck() {
	d.checkAgentHealth()
}

// TriggerMessageRouting triggers an immediate message routing (for testing)
func (d *Daemon) TriggerMessageRouting() {
	d.routeMessages()
}

// TriggerWake triggers an immediate wake cycle (for testing)
func (d *Daemon) TriggerWake() {
	d.wakeAgents()
}

// logDiagnostics logs system diagnostics in machine-readable JSON format
func (d *Daemon) logDiagnostics() {
	// Get version from CLI package (same as used by CLI)
	version := "dev"

	collector := diagnostics.NewCollector(d.paths, version)
	report, err := collector.Collect()
	if err != nil {
		d.logger.Error("Failed to collect diagnostics: %v", err)
		return
	}

	jsonOutput, err := report.ToJSON(false) // Compact JSON for logs
	if err != nil {
		d.logger.Error("Failed to format diagnostics: %v", err)
		return
	}

	d.logger.Info("System diagnostics: %s", jsonOutput)
}

// Stop stops the daemon
func (d *Daemon) Stop() error {
	d.logger.Info("Stopping daemon")

	// Part 2g: tear down assistant-turn tailers before cancelling the
	// daemon context. Each tailer closes its broadcaster, which lets any
	// connected stream_assistant_turns subscriber observe a clean Done
	// frame rather than a socket EOF.
	d.stopAllAssistantTurnTailers()

	// Cancel context to stop all loops
	d.cancel()

	// Wait for all goroutines to finish
	d.wg.Wait()

	// Stop socket server
	if err := d.server.Stop(); err != nil {
		d.logger.Error("Failed to stop socket server: %v", err)
	}

	// Save state
	if err := d.state.Save(); err != nil {
		d.logger.Error("Failed to save state: %v", err)
	}

	// Remove PID file
	if err := d.pidFile.Remove(); err != nil {
		d.logger.Error("Failed to remove PID file: %v", err)
	}

	d.logger.Info("Daemon stopped")
	return nil
}

// getRequiredStringArg extracts a required string argument from request Args.
// Returns the value and true if present, or an error response and false if missing.
func getRequiredStringArg(args map[string]interface{}, key, description string) (string, socket.Response, bool) {
	val, ok := args[key].(string)
	if !ok || val == "" {
		return "", socket.ErrorResponse("missing '%s': %s", key, description), false
	}
	return val, socket.Response{}, true
}

// getOptionalStringArg extracts an optional string argument from request Args.
// Returns the value if present, or the default value if missing.
func getOptionalStringArg(args map[string]interface{}, key, defaultVal string) string {
	if val, ok := args[key].(string); ok {
		return val
	}
	return defaultVal
}

// getOptionalIntArg extracts an optional int argument from request Args.
// Handles both float64 (JSON unmarshaling) and int values.
func getOptionalIntArg(args map[string]interface{}, key string, defaultVal int) int {
	if val, ok := args[key].(float64); ok {
		return int(val)
	}
	if val, ok := args[key].(int); ok {
		return val
	}
	return defaultVal
}

// getOptionalInt64Arg extracts an optional int64 argument from request Args.
// Handles both float64 (JSON unmarshaling) and int/int64 values.
// Returns 0 if the key is missing.
func getOptionalInt64Arg(args map[string]interface{}, key string) int64 {
	if val, ok := args[key].(float64); ok {
		return int64(val)
	}
	if val, ok := args[key].(int64); ok {
		return val
	}
	if val, ok := args[key].(int); ok {
		return int64(val)
	}
	return 0
}

// getOptionalBoolArg extracts an optional bool argument from request Args.
// Returns the value if present, or the default value if missing.
func getOptionalBoolArg(args map[string]interface{}, key string, defaultVal bool) bool {
	if val, ok := args[key].(bool); ok {
		return val
	}
	return defaultVal
}

// periodicLoop runs a function periodically at the specified interval.
// If onStartup is provided, it's called immediately before entering the loop.
// The onTick function is called on each timer tick.
// safeGo runs fn in a goroutine with panic recovery. If the goroutine
// panics, the error is logged and the daemon continues running.
func (d *Daemon) safeGo(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				d.logger.Error("Panic in goroutine %q: %v", name, r)
			}
		}()
		fn()
	}()
}

func (d *Daemon) periodicLoop(name string, interval time.Duration, onStartup, onTick func()) {
	defer d.wg.Done()
	d.logger.Info("Starting %s loop", name)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Run startup tasks if provided
	if onStartup != nil {
		onStartup()
	}

	for {
		select {
		case <-ticker.C:
			func() {
				defer func() {
					if r := recover(); r != nil {
						d.logger.Error("Panic in %s loop tick: %v", name, r)
					}
				}()
				onTick()
			}()
		case <-d.ctx.Done():
			d.logger.Info("%s loop stopped", name)
			return
		}
	}
}

// serverLoop handles socket connections
func (d *Daemon) serverLoop() {
	defer d.wg.Done()
	d.logger.Info("Starting server loop")

	// Run server in a goroutine so we can handle cancellation
	errCh := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				errCh <- fmt.Errorf("server panic: %v", r)
			}
		}()
		errCh <- d.server.Serve()
	}()

	select {
	case err := <-errCh:
		if err != nil {
			d.logger.Error("Server error: %v", err)
		}
	case <-d.ctx.Done():
		d.logger.Info("Server loop stopped")
	}
}

// prMonitorLoop periodically checks PR status for dormant workers.
// Also triggers immediately when a worker goes dormant (via prMonitorTrigger).
func (d *Daemon) prMonitorLoop() {
	defer d.wg.Done()
	interval := time.Duration(getEnvInt("OAT_PR_MONITOR_INTERVAL_SECONDS", 60)) * time.Second
	d.logger.Info("Starting PR monitor loop (interval: %s)", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	safeTick := func() {
		defer func() {
			if r := recover(); r != nil {
				d.logger.Error("Panic in PR monitor loop tick: %v", r)
			}
		}()
		d.checkWorkerPRs()
	}

	for {
		select {
		case <-d.ctx.Done():
			d.logger.Info("PR monitor loop stopped")
			return
		case <-ticker.C:
			safeTick()
		case <-d.prMonitorTrigger:
			safeTick()
		}
	}
}

// healthCheckLoop periodically checks agent health
func (d *Daemon) healthCheckLoop() {
	startup := func() {
		d.checkAgentHealth()
		d.rotateLogsIfNeeded()
		d.cleanupMergedBranches()
		d.checkWorkspaceHealth()
		d.checkCoreAgentHealth()
		d.checkVerificationTimeouts()
	}
	d.periodicLoop("health check", 2*time.Minute, startup, startup)
}

const verificationTimeout = 5 * time.Minute

// checkVerificationTimeouts scans dormant workers waiting for verification
// verdicts and wakes them if the verification has been pending too long or if
// the verifier is gone without delivering a verdict (safety net).
func (d *Daemon) checkVerificationTimeouts() {
	repos := d.state.GetAllRepos()
	hardTimeout := 2 * verificationTimeout // 10 minutes
	for repoName, repo := range repos {
		for agentName, agent := range repo.Agents {
			if !agent.WaitingForVerification {
				continue
			}

			notifyKey := fmt.Sprintf("%s/%s", repoName, agentName)
			d.verificationTimeoutNotifiedMu.Lock()
			_, softNotified := d.verificationTimeoutNotified[notifyKey]
			d.verificationTimeoutNotifiedMu.Unlock()

			// Hard timeout: if soft notification was sent and verify agent has
			// been running for 10+ minutes, kill the verify agent directly.
			if softNotified && !agent.WaitingForVerificationSince.IsZero() && time.Since(agent.WaitingForVerificationSince) > hardTimeout {
				verifierName := agent.VerificationAgent
				if verifierName != "" {
					verifier, verifierExists := repo.Agents[verifierName]
					if verifierExists && !verifier.ReadyForCleanup {
						d.logger.Warn("Hard timeout (%v) for verify agent %s/%s — killing", hardTimeout, repoName, verifierName)
						d.cleanupVerifyAgent(repoName, agentName)
					}
				}

				// Reset the worker's verification fields so it can proceed
				if err := d.state.ModifyAgent(repoName, agentName, func(a *state.Agent) {
					a.WaitingForVerification = false
					a.WaitingForVerificationSince = time.Time{}
					a.VerificationAgent = ""
					a.VerificationStatus = ""
				}); err != nil {
					d.logger.Error("Failed to reset verification fields for %s/%s: %v", repoName, agentName, err)
				}

				d.verificationTimeoutNotifiedMu.Lock()
				delete(d.verificationTimeoutNotified, notifyKey)
				d.verificationTimeoutNotifiedMu.Unlock()

				elapsed := int(time.Since(agent.WaitingForVerificationSince).Minutes())
				msg := fmt.Sprintf("[daemon] Verification hard timeout (%d min). The verify agent was killed. "+
					"Self-verify and create your PR: run `oat worker verify` then `oat pr create`.", elapsed)
				d.logger.Info("Verification hard timeout for %s/%s — waking worker", repoName, agentName)
				d.wakeWorker(repoName, agentName, agent, msg)
				continue
			}

			if softNotified {
				continue
			}

			// Soft timeout: verification still pending after 5 minutes
			if !agent.WaitingForVerificationSince.IsZero() && time.Since(agent.WaitingForVerificationSince) > verificationTimeout {
				var msg string
				verifierName := agent.VerificationAgent
				if verifierName != "" {
					_, verifierExists := repo.Agents[verifierName]
					elapsed := int(time.Since(agent.WaitingForVerificationSince).Minutes())
					if verifierExists {
						msg = fmt.Sprintf("[daemon] Your verification agent '%s' has been running for %d minutes (longer than the usual ~2 min). "+
							"Check its progress: `cat ~/.oat/output/%s/workers/%s.log | tail -20`. "+
							"If it appears stuck or not making progress, self-verify and create your PR: run `oat worker verify` then `oat pr create`. "+
							"If you receive an [APPROVED] or [REJECTED] verdict before you start, follow the verdict instead.",
							verifierName, elapsed, repoName, verifierName)
					} else {
						msg = fmt.Sprintf("[daemon] Your verification agent '%s' is no longer running and did not deliver a verdict. "+
							"Self-verify and create your PR: run `oat worker verify` then `oat pr create`.",
							verifierName)
					}
				} else {
					msg = "[daemon] Your verification has been pending for over 5 minutes with no verification agent assigned. " +
						"Self-verify and create your PR: run `oat worker verify` then `oat pr create`."
				}

				d.verificationTimeoutNotifiedMu.Lock()
				d.verificationTimeoutNotified[notifyKey] = time.Now()
				d.verificationTimeoutNotifiedMu.Unlock()

				d.logger.Info("Verification soft timeout for %s/%s — waking worker", repoName, agentName)
				d.wakeWorker(repoName, agentName, agent, msg)
				continue
			}

			// Safety net: worker is dormant with no PR, no verification status,
			// and no verification agent (verifier was cleaned up and reset the
			// fields, but the wake message may have failed).
			if agent.VerificationStatus == "" && agent.VerificationAgent == "" {
				msg := "[daemon] Your verification agent was cleaned up without delivering a verdict. " +
					"Self-verify and create your PR: run `oat worker verify` then `oat pr create`."

				d.verificationTimeoutNotifiedMu.Lock()
				d.verificationTimeoutNotified[notifyKey] = time.Now()
				d.verificationTimeoutNotifiedMu.Unlock()

				d.logger.Info("Orphaned dormant worker %s/%s (no verification, no PR) — waking as safety net", repoName, agentName)
				d.wakeWorker(repoName, agentName, agent, msg)
				continue
			}
		}
	}
}

// checkAgentHealth checks if agents are still alive
func (d *Daemon) checkAgentHealth() {
	d.logger.Debug("Checking agent health")

	deadAgents := make(map[string][]string) // repo -> []agent names

	// Get a snapshot of repos to avoid concurrent map access
	repos := d.state.GetAllRepos()
	for repoName, repo := range repos {
		// Check if backend session exists
		hasSession, err := d.backend.HasSession(d.ctx, repo.SessionName)
		if err != nil {
			d.logger.Error("Failed to check session %s: %v", repo.SessionName, err)
			continue
		}

		if !hasSession {
			d.logger.Warn("Session %s not found for repo %s, attempting restoration", repo.SessionName, repoName)
			if err := d.restoreRepoAgents(repoName, repo); err != nil {
				// Track consecutive restore failures — only wipe agents after
				// multiple failures to avoid data loss on transient errors.
				d.fetchFailuresMu.Lock()
				key := "restore:" + repoName
				d.fetchFailures[key]++
				failures := d.fetchFailures[key]
				d.fetchFailuresMu.Unlock()

				if failures >= fetchFailureThreshold {
					d.logger.Error("Failed to restore repo %s %d times, marking all agents for cleanup: %v", repoName, failures, err)
					for agentName := range repo.Agents {
						appendToSliceMap(deadAgents, repoName, agentName)
					}
				} else {
					d.logger.Warn("Failed to restore repo %s (attempt %d/%d): %v", repoName, failures, fetchFailureThreshold, err)
				}
			} else {
				d.logger.Info("Successfully restored session and agents for repo %s", repoName)
				// Reset failure counter on success
				d.fetchFailuresMu.Lock()
				delete(d.fetchFailures, "restore:"+repoName)
				d.fetchFailuresMu.Unlock()
			}
			continue
		}

		// Check each agent
		for agentName, agent := range repo.Agents {
			// Check if agent is marked as ready for cleanup
			if agent.ReadyForCleanup {
				// Workers and review agents get a delay so they can see merge-queue feedback
				if agent.Type == state.AgentTypeWorker || agent.Type == state.AgentTypeReview || agent.Type == state.AgentTypeVerification {
					if agent.ReadyForCleanupAt.IsZero() || time.Since(agent.ReadyForCleanupAt) >= workerPostCompletionDelay {
						d.logger.Info("Agent %s is ready for cleanup", agentName)
						appendToSliceMap(deadAgents, repoName, agentName)
					}
					// else: not yet past delay, skip
				} else {
					d.logger.Info("Agent %s is ready for cleanup", agentName)
					appendToSliceMap(deadAgents, repoName, agentName)
				}
				continue
			}

			// Check if agent was re-adopted (alive but no PTY). Persistent agents
			// should be stopped and restarted to regain full control; transient
			// agents are left alone since they'll finish on their own.
			if db, ok := d.backend.(*backend_pkg.DirectBackend); ok {
				if adopted, _ := db.IsAdopted(d.ctx, repo.SessionName, agent.WindowName); adopted {
					if agent.Type.IsPersistent() {
						d.logger.Info("Agent %s was re-adopted without PTY, stopping then restarting", agentName)
						// Must kill the adopted process first — restartAgent calls
						// StartAgent which would overwrite the map entry, orphaning
						// the old process forever.
						if err := d.backend.StopAgent(d.ctx, repo.SessionName, agent.WindowName); err != nil {
							d.logger.Warn("Failed to stop adopted agent %s: %v", agentName, err)
						}
						if err := d.restartAgent(repoName, agentName, agent, repo); err != nil {
							d.logger.Error("Failed to restart adopted agent %s: %v", agentName, err)
						} else {
							d.logger.Info("Successfully restarted adopted agent %s", agentName)
						}
					} else {
						d.logger.Debug("Adopted transient agent %s left running (will finish on its own)", agentName)
					}
					continue
				}
			}

			// Check if agent is alive — try backend first, fall back to PID check.
			// For DirectBackend, IsAgentAlive checks the in-memory map (only works
			// for agents started by this daemon instance). PID check is the universal fallback.
			alive := false
			if hasWindow, err := d.backend.IsAgentAlive(d.ctx, repo.SessionName, agent.WindowName); err == nil && hasWindow {
				alive = true
			}
			if !alive && agent.PID > 0 {
				alive = isProcessAlive(agent.PID)
			}

			if !alive {
				// Grace period: don't mark agents as dead if they were created
				// within the last 5 minutes — covers slow startup, agent runtime
				// initialization, and daemon restart PID staleness.
				if time.Since(agent.CreatedAt) < 5*time.Minute {
					d.logger.Debug("Agent %s not alive but within startup grace period (created %s ago), skipping", agentName, time.Since(agent.CreatedAt).Round(time.Second))
					continue
				}

				d.logger.Warn("Agent %s not alive (pid=%d)", agentName, agent.PID)

				// For persistent agents, attempt auto-restart with cooldown to
				// prevent restart storms when startup is unstable.
				if agent.Type.IsPersistent() {
					cooldownKey := fmt.Sprintf("%s/%s", repoName, agentName)

					// Browser-agent back-off: if the bridge has been
					// found dead repeatedly within a 10-min window
					// (typically: Chrome closed, extension
					// uninstalled, NM host missing), stop
					// auto-restarting. The user re-engages via
					// `oat agent restart browser-agent`, which both
					// restarts the agent and clears this counter.
					// Without this guard, the 2-min health-check
					// loop spawns a doomed bridge subprocess every
					// cycle and burns tokens on its startup banner.
					if usesBrowserBridge(agent.Type) {
						failures := d.recordBridgeUnreachable(cooldownKey, time.Now())
						if failures >= bridgeUnreachableThreshold {
							d.logger.Warn(
								"Browser-agent %s/%s unreachable %d times in last %s; auto-restart disabled. "+
									"Run `oat agent restart browser-agent --repo %s` after the bridge is reachable.",
								repoName, agentName, failures, bridgeUnreachableWindow, repoName,
							)
							appendToSliceMap(deadAgents, repoName, agentName)
							continue
						}
					}

					d.restartCooldownMu.Lock()
					lastRestart := d.restartCooldown[cooldownKey]
					d.restartCooldownMu.Unlock()
					if time.Since(lastRestart) < 30*time.Second {
						d.logger.Debug("Agent %s restart skipped (cooldown, last restart %s ago)", agentName, time.Since(lastRestart).Round(time.Second))
						continue
					}

					d.logger.Info("Attempting to auto-restart agent %s", agentName)
					d.restartCooldownMu.Lock()
					d.restartCooldown[cooldownKey] = time.Now()
					d.restartCooldownMu.Unlock()
					if err := d.restartAgent(repoName, agentName, agent, repo); err != nil {
						d.logger.Error("Failed to restart agent %s: %v", agentName, err)
						appendToSliceMap(deadAgents, repoName, agentName)
					} else {
						d.logger.Info("Successfully restarted agent %s", agentName)
						// Browser-agent: a successful restart clears
						// the failure window. The next dead-check
						// starts the counter over.
						if usesBrowserBridge(agent.Type) {
							d.clearBridgeUnreachable(cooldownKey)
						}
					}
				} else {
					// For transient agents (workers, review), mark for cleanup
					d.logger.Info("Transient agent %s dead, marking for cleanup", agentName)
					appendToSliceMap(deadAgents, repoName, agentName)
				}
			}
		}
	}

	// Clean up dead agents, collecting their worktree paths so orphan cleanup
	// doesn't race and delete them before git worktree remove finishes.
	var recentlyRemovedPaths []string
	if len(deadAgents) > 0 {
		recentlyRemovedPaths = d.cleanupDeadAgents(deadAgents)
	}

	// Clean up orphaned worktrees — pass recently removed paths as protected
	d.cleanupOrphanedWorktrees(recentlyRemovedPaths)
}

// messageRouterLoop watches for new messages and delivers them
func (d *Daemon) messageRouterLoop() {
	defer d.wg.Done()
	ticker := time.NewTicker(time.Duration(getEnvInt("OAT_MESSAGE_ROUTER_INTERVAL_SECONDS", 60)) * time.Second)
	defer ticker.Stop()

	safeRoute := func() {
		defer func() {
			if r := recover(); r != nil {
				d.logger.Error("Panic in message router: %v", r)
			}
		}()
		d.routeMessages()
	}

	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			safeRoute()
		case <-d.routeMessagesTrigger:
			safeRoute()
		}
	}
}

// triggerRouteMessages requests an immediate message routing pass.
// Non-blocking: if a trigger is already pending, the request is coalesced.
func (d *Daemon) triggerRouteMessages() {
	select {
	case d.routeMessagesTrigger <- struct{}{}:
	default:
		// Already triggered — the pending run will pick up new messages
	}
}

// triggerPRMonitor requests an immediate PR monitoring pass.
// Non-blocking: if a trigger is already pending, the request is coalesced.
func (d *Daemon) triggerPRMonitor() {
	select {
	case d.prMonitorTrigger <- struct{}{}:
	default:
	}
}

// triggerWake requests an immediate wake/nudge pass.
// Non-blocking: if a trigger is already pending, the request is coalesced.
func (d *Daemon) triggerWake() {
	select {
	case d.wakeTrigger <- struct{}{}:
	default:
	}
}

// pendingDelivery holds a message ready for delivery, collected under the lock.
type pendingDelivery struct {
	repoName    string
	agentName   string
	sessionName string
	windowName  string
	msg         messages.Message
	mailbox     string // mailbox the message lives in (may differ from agentName for workspace aliases)
}

// routeMessages checks for pending messages and delivers them.
// Collection is serialized via routeMessagesMu; delivery happens outside
// the lock so slow backend.SendMessage calls don't block other daemon operations.
func (d *Daemon) routeMessages() {
	// Phase 1: Collect pending deliveries under the lock.
	d.routeMessagesMu.Lock()
	d.logger.Debug("Routing messages")

	msgMgr := d.getMessageManager()
	repos := d.state.GetAllRepos()

	var deliveries []pendingDelivery
	for repoName, repo := range repos {
		for agentName, agent := range repo.Agents {
			if agent.ReadyForCleanup {
				continue
			}

			// Skip agents whose process is dead — delivering to them is
			// pointless and would retry every cycle forever.
			if agent.PID > 0 && !isProcessAlive(agent.PID) {
				continue
			}

			unreadMsgs, err := msgMgr.ListUnread(repoName, agentName)
			if err != nil {
				d.logger.Error("Failed to list messages for %s/%s: %v", repoName, agentName, err)
				continue
			}

			// Workspace agents can be named "default" or "workspace" in state,
			// but senders may address messages to either name. Check both
			// mailboxes so messages aren't silently dropped.
			if agent.Type == state.AgentTypeWorkspace {
				alias := "workspace"
				if agentName == "workspace" {
					alias = "default"
				}
				aliasMessages, err := msgMgr.ListUnread(repoName, alias)
				if err == nil {
					unreadMsgs = append(unreadMsgs, aliasMessages...)
				}
			}

			for _, msg := range unreadMsgs {
				if msg.Status != messages.StatusPending {
					continue
				}
				mailbox := agentName
				if msg.To != agentName {
					mailbox = msg.To
				}
				deliveries = append(deliveries, pendingDelivery{
					repoName:    repoName,
					agentName:   agentName,
					sessionName: repo.SessionName,
					windowName:  agent.WindowName,
					msg:         *msg,
					mailbox:     mailbox,
				})
			}
		}
	}
	d.routeMessagesMu.Unlock()

	// Phase 2: Deliver outside the lock so slow sends don't block other operations.
	delivered := 0
	for _, del := range deliveries {
		// Space out deliveries so the TUI can process each message
		if delivered > 0 {
			time.Sleep(200 * time.Millisecond)
		}

		messageText := fmt.Sprintf("📨 Message from %s: %s", del.msg.From, del.msg.Body)

		if err := d.backend.SendMessage(d.ctx, del.sessionName, del.windowName, messageText); err != nil {
			d.logger.Error("Failed to deliver message %s to %s/%s: %v", del.msg.ID, del.repoName, del.agentName, err)

			// Determine if this is a permanent failure (mark as failed) or transient (leave pending for retry).
			// Permanent: agent adopted without PTY, or agent process is confirmed dead.
			// Transient: temporary backend hiccup — message stays pending and retries next cycle.
			permanentFailure := errors.Is(err, backend_pkg.ErrAgentAdopted)
			if !permanentFailure {
				// Check if the target agent's process is actually dead
				agent, exists := d.state.GetAgent(del.repoName, del.agentName)
				if !exists || agent.ReadyForCleanup || (agent.PID > 0 && !isProcessAlive(agent.PID)) {
					permanentFailure = true
				}
			}

			if permanentFailure {
				if markErr := msgMgr.UpdateStatus(del.repoName, del.mailbox, del.msg.ID, messages.StatusFailed); markErr != nil {
					d.logger.Error("Failed to mark message %s as failed: %v", del.msg.ID, markErr)
				} else {
					d.logger.Info("Marked message %s as permanently failed (agent %s/%s unreachable)", del.msg.ID, del.repoName, del.agentName)
				}
			}
			continue
		}

		if err := msgMgr.UpdateStatus(del.repoName, del.mailbox, del.msg.ID, messages.StatusDelivered); err != nil {
			d.logger.Error("Failed to update message %s status: %v", del.msg.ID, err)
			continue
		}

		d.logger.Info("Delivered message %s from %s to %s/%s", del.msg.ID, del.msg.From, del.repoName, del.agentName)
		delivered++
	}
}

// getMessageManager returns a message manager instance
func (d *Daemon) getMessageManager() *messages.Manager {
	return messages.NewManager(d.paths.MessagesDir)
}

// wakeLoop periodically wakes agents with status checks.
// Also triggers immediately when a worker is created or woken (via wakeTrigger).
func (d *Daemon) wakeLoop() {
	defer d.wg.Done()
	interval := time.Duration(getEnvInt("OAT_WAKE_INTERVAL_SECONDS", 60)) * time.Second
	d.logger.Info("Starting wake loop (interval: %s)", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	safeTick := func() {
		defer func() {
			if r := recover(); r != nil {
				d.logger.Error("Panic in wake loop tick: %v", r)
			}
		}()
		d.wakeAgents()
	}

	for {
		select {
		case <-d.ctx.Done():
			d.logger.Info("Wake loop stopped")
			return
		case <-ticker.C:
			safeTick()
		case <-d.wakeTrigger:
			safeTick()
		}
	}
}

// repoHasActiveWorkers returns true if the repo has at least one worker or review agent
// that is not ready for cleanup and not dormant (waiting for PR resolution).
func repoHasActiveWorkers(repo *state.Repository) bool {
	for _, agent := range repo.Agents {
		if (agent.Type == state.AgentTypeWorker || agent.Type == state.AgentTypeReview || agent.Type == state.AgentTypeVerification) &&
			!agent.ReadyForCleanup && !agent.IsDormant() {
			return true
		}
	}
	return false
}

// Final nudge messages sent when transitioning a repo to idle mode (one per
// agent type). These used to carry meta-commentary ("before we pause nudges",
// "after this we'll stop status checks until new workers are created") that
// didn't drive agent behavior; compacted in the P1 release-prep pass while
// preserving the actionable instructions. `sleep 30` is kept literal inside
// the merge-queue template so agents don't reinterpret it as a busy-loop hint.
const (
	finalNudgeSupervisor = "[daemon] Final status check. Any pending messages or follow-up?"

	finalNudgeMergeQueue = "[daemon] Final status check. Merge any open PRs with green CI. " +
		"For in-progress CI: poll `gh run list --branch <branch> --limit 1`, `sleep 30`, " +
		"repeat, for up to 5 min. Don't stop until CI completes (pass/fail) and all " +
		"actionable PRs are merged or reported."

	finalNudgePRShepherd = "[daemon] Final check: review upstream PRs, check CI, rebase if needed."
)

// wakeAgents sends periodic nudges to agents. When a repo has no workers (idle mode), the daemon
// sends one final nudge to supervisor/merge-queue/PR shepherd then stops nudging until workers appear.
func (d *Daemon) wakeAgents() {
	d.logger.Debug("Waking agents")

	now := time.Now()
	repos := d.state.GetAllRepos()

	for repoName, repo := range repos {
		hasActive := repoHasActiveWorkers(repo)
		idle := repo.IdleMode

		// Return to active mode when workers appear.
		// Uses ModifyRepo to atomically re-check live state, preventing
		// a race where the worker disappears between snapshot and write.
		if idle && hasActive {
			if err := d.state.ModifyRepo(repoName, func(r *state.Repository) {
				if repoHasActiveWorkers(r) {
					r.IdleMode = false
					d.logger.Info("Repo %s resuming nudges (workers present)", repoName)
				}
			}); err != nil {
				d.logger.Error("Failed to clear idle mode for repo %s: %v", repoName, err)
			}
			idle = false
		}

		// Transition to idle: send final nudge to top-level agents only, then stop nudging this repo.
		// Also message the workspace agent so it checks for pending work (e.g., next wave of issues).
		// Without this, the workspace stalls after all workers complete and never spawns new ones.
		// Uses ModifyRepo to atomically re-check that no workers appeared since the snapshot.
		if !idle && !hasActive {
			d.sendFinalNudgeToRepo(repoName, repo, now)
			// Historical note: a notifyWorkspaceIdleTransition helper used to fire
			// here so the workspace would spawn next-wave workers. It was removed
			// because it raced with benchmark wave control — fresh repos that
			// never had workers triggered a spawn before the benchmark gate
			// finished. See https://github.com/Root-IO-Labs/open-agent-teams/pull/58
			// for the redesign discussion.
			if err := d.state.ModifyRepo(repoName, func(r *state.Repository) {
				if !repoHasActiveWorkers(r) {
					r.IdleMode = true
					d.logger.Info("Repo %s entering idle mode (no workers)", repoName)
				} else {
					d.logger.Info("Repo %s: worker appeared during idle transition, staying active", repoName)
				}
			}); err != nil {
				d.logger.Error("Failed to set idle mode for repo %s: %v", repoName, err)
			}
			d.scheduleDelayedMergeQueueNudge(repoName, repo)
			continue
		}

		// Already idle and no workers: skip this repo
		if idle && !hasActive {
			continue
		}

		// Active mode: nudge all non-workspace agents as before
		d.nudgeAgentsInRepo(repoName, repo, now)
	}
}

// sendFinalNudgeToRepo sends the final status-check message to supervisor, merge-queue, and PR shepherd only.
// Uses shouldSendFinalNudge instead of shouldNudgeAgent to bypass the 2-minute cooldown,
// since the final nudge is critical and must not be silently skipped.
func (d *Daemon) sendFinalNudgeToRepo(repoName string, repo *state.Repository, now time.Time) {
	repoPath := d.paths.RepoDir(repoName)

	for agentName, agent := range repo.Agents {
		var message string
		switch agent.Type {
		case state.AgentTypeSupervisor:
			message = finalNudgeSupervisor
		case state.AgentTypeMergeQueue:
			ciSummary := d.buildMergeQueueCISummary(repoPath)
			message = finalNudgeMergeQueue + "\n" + ciSummary
		case state.AgentTypePRShepherd:
			message = finalNudgePRShepherd
		default:
			continue
		}
		if !d.shouldSendFinalNudge(agent) {
			continue
		}
		// Attach daemon-state snapshot for supervisor/merge-queue so the agent
		// doesn't respond to this nudge with a status-check shell loop.
		// Non-target agent types pass through unchanged.
		message = d.withRepoSnapshot(repoName, agent.Type, message)
		if err := d.backend.SendMessage(d.ctx, repo.SessionName, agent.WindowName, message); err != nil {
			d.logger.Error("Failed to send final wake message to agent %s: %v", agentName, err)
			continue
		}
		if err := d.state.ModifyAgent(repoName, agentName, func(a *state.Agent) {
			a.LastNudge = now
		}); err != nil {
			d.logger.Error("Failed to update agent %s last nudge: %v", agentName, err)
		}
		d.logger.Debug("Sent final nudge to agent %s in repo %s", agentName, repoName)
	}
}

// scheduleDelayedMergeQueueNudge schedules a follow-up nudge to the merge-queue
// ~3 minutes after the repo enters idle mode. This catches the case where CI was
// still in-progress during the final nudge and the merge-queue's LLM polling loop
// was interrupted or didn't fire.
func (d *Daemon) scheduleDelayedMergeQueueNudge(repoName string, repo *state.Repository) {
	var mqAgentName string
	var mqWindowName string
	var sessionName string
	for name, agent := range repo.Agents {
		if agent.Type == state.AgentTypeMergeQueue && !agent.ReadyForCleanup {
			mqAgentName = name
			mqWindowName = agent.WindowName
			sessionName = repo.SessionName
			break
		}
	}
	if mqAgentName == "" {
		return
	}

	repoPath := d.paths.RepoDir(repoName)
	go func() {
		select {
		case <-time.After(3 * time.Minute):
			// Re-check: only send if repo is still idle
			r, exists := d.state.GetRepo(repoName)
			if !exists || !r.IdleMode {
				return
			}
			ciSummary := d.buildMergeQueueCISummary(repoPath)
			message := "[daemon] Follow-up status check (3 min after idle). Check for any open PRs that may now have CI results.\n" + ciSummary
			message = d.withRepoSnapshot(repoName, state.AgentTypeMergeQueue, message)
			if err := d.backend.SendMessage(d.ctx, sessionName, mqWindowName, message); err != nil {
				d.logger.Error("Failed to send delayed merge-queue nudge for repo %s: %v", repoName, err)
			} else {
				d.logger.Info("Sent delayed follow-up nudge to merge-queue in repo %s", repoName)
			}
		case <-d.ctx.Done():
			return
		}
	}()
}

// shouldSendFinalNudge checks whether a final nudge should be sent to an agent
// before entering idle mode. Unlike shouldNudgeAgent, this deliberately skips
// the 2-minute time-based cooldown because the final nudge is critical.
func (d *Daemon) shouldSendFinalNudge(agent state.Agent) bool {
	if agent.Type == state.AgentTypeWorkspace {
		return false
	}
	if agent.ReadyForCleanup {
		return false
	}
	if agent.IsDormant() {
		return false
	}
	return true
}

// nudgeIntervalFor resolves the minimum nudge interval for an agent using a
// three-step fallback ladder:
//
//  1. If the agent's resolved model has a non-zero Runtime.NudgeIntervalSeconds
//     in its ModelProfile, use it.
//  2. Otherwise fall back to the OAT_NUDGE_INTERVAL_SECONDS env var (default 60s).
//  3. For the direct backend supervisor we clamp to at least 10 minutes to
//     avoid chat-spam on operator-driven workflows.
//
// Returning 0 is never allowed — callers rely on a positive interval to debounce.
func (d *Daemon) nudgeIntervalFor(repo *state.Repository, agent state.Agent) time.Duration {
	// Env-based default (shared across all models).
	def := time.Duration(getEnvInt("OAT_NUDGE_INTERVAL_SECONDS", 60)) * time.Second

	// Per-model override: walk the resolution we use everywhere else
	// (agent.Model → repo.Model) so the value sticks for the agent's lifetime.
	modelID := agent.Model
	if modelID == "" && repo != nil {
		modelID = repo.Model
	}
	if d.modelProfiles != nil && modelID != "" {
		if p := d.modelProfiles.Get(modelID); p != nil && p.Runtime.NudgeIntervalSeconds > 0 {
			def = time.Duration(p.Runtime.NudgeIntervalSeconds) * time.Second
		}
	}

	// Direct backend operator mode is chat-driven; reduce supervisor churn.
	if d.isDirectBackend() && agent.Type == state.AgentTypeSupervisor {
		if def < 10*time.Minute {
			def = 10 * time.Minute
		}
	}
	return def
}

// shouldNudgeAgent returns false if the agent should be skipped (workspace, dormant, recently nudged, or process not alive).
func (d *Daemon) shouldNudgeAgent(repo *state.Repository, agentName string, agent state.Agent, now time.Time) bool {
	if agent.Type == state.AgentTypeWorkspace {
		return false
	}
	if agent.IsDormant() {
		return false
	}
	if agent.ReadyForCleanup {
		return false
	}
	minNudgeInterval := d.nudgeIntervalFor(repo, agent)
	if !agent.LastNudge.IsZero() && now.Sub(agent.LastNudge) < minNudgeInterval {
		return false
	}
	// Skip nudging agents that haven't started yet (PID 0) or whose
	// process is no longer alive — nudging them wastes resources and
	// increments the nudge counter incorrectly.
	if agent.PID <= 0 {
		d.logger.Debug("Agent %s has no PID yet, skipping wake", agentName)
		return false
	}
	if !isProcessAlive(agent.PID) {
		d.logger.Debug("Agent %s PID %d not alive, skipping wake", agentName, agent.PID)
		return false
	}
	if !isAgentProcess(agent.PID) {
		d.logger.Debug("Agent %s PID %d is not agent process, skipping wake", agentName, agent.PID)
		return false
	}
	return true
}

// nudgeAgentsInRepo sends status-check nudges to all non-workspace agents.
// For workers, it delegates to the escalating nudge ladder.
func (d *Daemon) nudgeAgentsInRepo(repoName string, repo *state.Repository, now time.Time) {
	repoPath := d.paths.RepoDir(repoName)

	for agentName, agent := range repo.Agents {
		if !d.shouldNudgeAgent(repo, agentName, agent, now) {
			continue
		}

		if agent.Type == state.AgentTypeWorker {
			d.nudgeWorkerEscalating(repoName, repoPath, agentName, agent, now)
			continue
		}

		var message string
		switch agent.Type {
		case state.AgentTypeSupervisor:
			message = "[daemon] Status check: Review worker progress and check merge queue."
		case state.AgentTypeMergeQueue:
			ciSummary := d.buildMergeQueueCISummary(repoPath)
			message = "[daemon] Status check: Review open PRs and check CI status. Merge any PRs that pass CI and have no merge conflicts.\n" + ciSummary
		case state.AgentTypePRShepherd:
			message = "[daemon] Status check: Review PRs on upstream, check CI status, and rebase branches if needed."
		case state.AgentTypeReview:
			message = "[daemon] Status check: Update on your review progress?"
		case state.AgentTypeVerification:
			message = "[daemon] Status check: Update on your verification progress? Deliver your verdict soon."
		// AgentTypeBrowser and AgentTypeAssistant are intentionally
		// NOT nudged. Per Part 2 of mcp-and-opt-in-browser-agent:
		// browser-agent is a tool, not a worker; it receives tasks via
		// inter-agent messaging and sits silent between tasks. Per
		// Part 5a of side-panel-chat-and-status: AgentTypeAssistant
		// receives its tasks from side-panel chat (user input) and
		// sits silent between user messages. In both cases a "status
		// check" nudge would waste an LLM turn to answer "nothing
		// happening" -- the wrong UX for the assistant especially,
		// which would otherwise interrupt the user with unsolicited
		// status messages in the side panel.
		case state.AgentTypeGenericPersistent:
			message = "[daemon] Status check: Update on your progress?"
		default:
			continue
		}
		// Supervisor/merge-queue get a daemon-state snapshot appended so they
		// don't respond to this nudge with a `oat worker list` / `gh pr list`
		// loop. Other agent types pass through unchanged.
		message = d.withRepoSnapshot(repoName, agent.Type, message)

		// No-change skip: if the fully-composed message (including injected
		// snapshot) is byte-identical to the last nudge sent to this agent,
		// the LLM would spin up a turn just to read the same state. Skip
		// the PTY send and record the skip. After maxNudgeSkips consecutive
		// skips we always send a heartbeat so the agent proves liveness
		// and picks up any out-of-band change we missed.
		if d.shouldSkipNudgeForAgent(agent.Type) {
			hash := hashNudgeContent(message)
			maxSkips := nudgeSkipMax()
			if hash == agent.LastNudgeHash && agent.NudgeSkipCount < maxSkips {
				d.logger.Debug("Skipped no-change nudge to %s/%s (skip %d/%d)",
					repoName, agentName, agent.NudgeSkipCount+1, maxSkips)
				if err := d.state.ModifyAgent(repoName, agentName, func(a *state.Agent) {
					a.NudgeSkipCount++
				}); err != nil {
					d.logger.Error("Failed to update skip count for agent %s: %v", agentName, err)
				}
				continue
			}
			if err := d.backend.SendMessage(d.ctx, repo.SessionName, agent.WindowName, message); err != nil {
				d.logger.Error("Failed to send wake message to agent %s: %v", agentName, err)
				continue
			}
			if err := d.state.ModifyAgent(repoName, agentName, func(a *state.Agent) {
				a.LastNudge = now
				a.LastNudgeHash = hash
				a.NudgeSkipCount = 0
			}); err != nil {
				d.logger.Error("Failed to update agent %s last nudge: %v", agentName, err)
			}
			d.logger.Debug("Woke agent %s in repo %s", agentName, repoName)
			continue
		}

		if err := d.backend.SendMessage(d.ctx, repo.SessionName, agent.WindowName, message); err != nil {
			d.logger.Error("Failed to send wake message to agent %s: %v", agentName, err)
			continue
		}
		if err := d.state.ModifyAgent(repoName, agentName, func(a *state.Agent) {
			a.LastNudge = now
		}); err != nil {
			d.logger.Error("Failed to update agent %s last nudge: %v", agentName, err)
		}
		d.logger.Debug("Woke agent %s in repo %s", agentName, repoName)
	}
}

// shouldSkipNudgeForAgent reports whether the no-change nudge-skip logic
// applies to this agent type. Only supervisor and merge-queue receive
// daemon-injected state snapshots, so only they are vulnerable to the
// "identical snapshot, duplicate turn" failure mode. Other persistent
// agents still get the pre-existing unconditional nudge.
func (d *Daemon) shouldSkipNudgeForAgent(t state.AgentType) bool {
	return t == state.AgentTypeSupervisor || t == state.AgentTypeMergeQueue
}

// hashNudgeContent returns a short content hash of the fully-composed
// nudge message. sha256 + hex → deterministic and collision-safe for our
// volume. We store the hex string in state so a snapshot can be diffed
// across daemon restarts.
func hashNudgeContent(msg string) string {
	sum := sha256.Sum256([]byte(msg))
	return hex.EncodeToString(sum[:])
}

// nudgeSkipMax returns the configured ceiling on consecutive skipped
// nudges to a single agent. At the default 60s wake cadence, a max of 5
// means we force a liveness nudge every ~5 minutes even when state is
// static. Override via OAT_NUDGE_SKIP_MAX; a value of 0 disables the
// skip path entirely.
func nudgeSkipMax() int {
	n := getEnvInt("OAT_NUDGE_SKIP_MAX", 5)
	if n < 0 {
		return 0
	}
	return n
}

// refreshWorktrees syncs worker worktrees that are behind main
func (d *Daemon) refreshWorktrees() {
	d.logger.Debug("Checking worker worktrees for refresh")

	repos := d.state.GetAllRepos()
	for repoName, repo := range repos {
		if d.isRepoFetchDisabled(repoName) {
			continue
		}

		repoPath := d.paths.RepoDir(repoName)

		// Check if repo path exists
		if _, err := os.Stat(repoPath); os.IsNotExist(err) {
			continue
		}

		wt := worktree.NewManagerWithContext(d.ctx, repoPath)

		// Get the upstream remote and default branch
		remote, err := wt.GetUpstreamRemote()
		if err != nil {
			d.logger.Debug("Could not get remote for %s: %v", repoName, err)
			continue
		}

		mainBranch, err := wt.GetDefaultBranch(remote)
		if err != nil {
			d.logger.Debug("Could not get default branch for %s: %v", repoName, err)
			continue
		}

		// Fetch from remote to have latest state
		if err := wt.FetchRemote(remote); err != nil {
			d.recordFetchFailure(repoName)
			d.logger.Debug("Could not fetch from remote for %s: %v", repoName, err)
			continue
		}
		d.resetFetchFailures(repoName)

		// Check each worker agent's worktree
		for agentName, agent := range repo.Agents {
			// Only refresh worker worktrees
			if agent.Type != state.AgentTypeWorker {
				continue
			}

			// Skip if worktree path is empty
			if agent.WorktreePath == "" {
				continue
			}

			// Check if worktree exists
			if _, err := os.Stat(agent.WorktreePath); os.IsNotExist(err) {
				continue
			}

			// Check worktree state
			wtState, err := worktree.GetWorktreeState(d.ctx, agent.WorktreePath, remote, mainBranch)
			if err != nil {
				d.logger.Debug("Could not get worktree state for %s/%s: %v", repoName, agentName, err)
				continue
			}

			// Skip if can't refresh (detached HEAD, mid-rebase, mid-merge, on main, or up to date)
			if !wtState.CanRefresh {
				d.logger.Debug("Skipping refresh for %s/%s: %s", repoName, agentName, wtState.RefreshReason)
				continue
			}

			// Refresh the worktree
			d.logger.Info("Refreshing worktree for %s/%s (%d commits behind)", repoName, agentName, wtState.CommitsBehind)
			result := worktree.RefreshWorktree(d.ctx, agent.WorktreePath, remote, mainBranch)

			if result.Error != nil {
				if result.HasConflicts {
					d.logger.Warn("Worktree refresh for %s/%s has conflicts in: %v", repoName, agentName, result.ConflictFiles)
				} else {
					d.logger.Error("Failed to refresh worktree for %s/%s: %v", repoName, agentName, result.Error)
				}
			} else if result.Skipped {
				d.logger.Debug("Worktree refresh for %s/%s skipped: %s", repoName, agentName, result.SkipReason)
			} else {
				d.logger.Info("Refreshed worktree for %s/%s: rebased %d commits", repoName, agentName, result.CommitsRebased)
			}
		}
	}
}

// refreshWorktreesWithOptions syncs worktrees with optional repo and branch targeting.
// If targetRepo is empty, all repos are synced. If branch is empty, each repo's default branch is used.
func (d *Daemon) refreshWorktreesWithOptions(targetRepo, branch string) {
	d.logger.Debug("Checking worker worktrees for sync (repo=%q, branch=%q)", targetRepo, branch)

	repos := d.state.GetAllRepos()
	for repoName, repo := range repos {
		if targetRepo != "" && repoName != targetRepo {
			continue
		}
		if d.isRepoFetchDisabled(repoName) {
			continue
		}

		repoPath := d.paths.RepoDir(repoName)
		if _, err := os.Stat(repoPath); os.IsNotExist(err) {
			continue
		}

		wt := worktree.NewManager(repoPath)

		remote, err := wt.GetUpstreamRemote()
		if err != nil {
			d.logger.Debug("Could not get remote for %s: %v", repoName, err)
			continue
		}

		syncBranch := branch
		if syncBranch == "" {
			syncBranch, err = wt.GetDefaultBranch(remote)
			if err != nil {
				d.logger.Debug("Could not get default branch for %s: %v", repoName, err)
				continue
			}
		}

		if err := wt.FetchRemote(remote); err != nil {
			d.recordFetchFailure(repoName)
			d.logger.Debug("Could not fetch from remote for %s: %v", repoName, err)
			continue
		}
		d.resetFetchFailures(repoName)

		for agentName, agent := range repo.Agents {
			if agent.Type != state.AgentTypeWorker {
				continue
			}
			if agent.WorktreePath == "" {
				continue
			}
			if _, err := os.Stat(agent.WorktreePath); os.IsNotExist(err) {
				continue
			}

			wtState, err := worktree.GetWorktreeState(d.ctx, agent.WorktreePath, remote, syncBranch)
			if err != nil {
				d.logger.Debug("Could not get worktree state for %s/%s: %v", repoName, agentName, err)
				continue
			}

			if !wtState.CanRefresh {
				d.logger.Debug("Skipping sync for %s/%s: %s", repoName, agentName, wtState.RefreshReason)
				continue
			}

			d.logger.Info("Syncing worktree for %s/%s (%d commits behind %s)", repoName, agentName, wtState.CommitsBehind, syncBranch)
			result := worktree.RefreshWorktree(d.ctx, agent.WorktreePath, remote, syncBranch)

			if result.Error != nil {
				if result.HasConflicts {
					d.logger.Warn("Worktree sync for %s/%s has conflicts in: %v", repoName, agentName, result.ConflictFiles)
				} else {
					d.logger.Error("Failed to sync worktree for %s/%s: %v", repoName, agentName, result.Error)
				}
			} else if result.Skipped {
				d.logger.Debug("Worktree sync for %s/%s skipped: %s", repoName, agentName, result.SkipReason)
			} else {
				d.logger.Info("Synced worktree for %s/%s: rebased %d commits", repoName, agentName, result.CommitsRebased)
			}
		}
	}
}

// TriggerWorktreeRefresh triggers an immediate worktree refresh (for testing)
func (d *Daemon) TriggerWorktreeRefresh() {
	d.refreshWorktrees()
}

// recordFetchFailure increments the consecutive fetch failure counter for a repo.
// After reaching fetchFailureThreshold, the repo is skipped for future fetches.
func (d *Daemon) recordFetchFailure(repoName string) {
	d.fetchFailuresMu.Lock()
	defer d.fetchFailuresMu.Unlock()
	d.fetchFailures[repoName]++
	count := d.fetchFailures[repoName]
	if count == fetchFailureThreshold {
		d.logger.Warn("Repo %s has %d consecutive fetch failures; skipping future fetches until daemon restart. Run 'oat repo rm %s' if the remote no longer exists.", repoName, count, repoName)
	}
}

// resetFetchFailures clears the failure counter for a repo after a successful fetch.
func (d *Daemon) resetFetchFailures(repoName string) {
	d.fetchFailuresMu.Lock()
	defer d.fetchFailuresMu.Unlock()
	delete(d.fetchFailures, repoName)
}

// isRepoFetchDisabled returns true if a repo has exceeded the fetch failure threshold.
func (d *Daemon) isRepoFetchDisabled(repoName string) bool {
	d.fetchFailuresMu.Lock()
	defer d.fetchFailuresMu.Unlock()
	return d.fetchFailures[repoName] >= fetchFailureThreshold
}

// handleRequest handles incoming socket requests
func (d *Daemon) handleRequest(req socket.Request) socket.Response {
	d.logger.Debug("Handling request: %s", req.Command)

	switch req.Command {
	case "ping":
		return socket.SuccessResponse("pong")

	case "status":
		return d.handleStatus(req)

	case "stop":
		d.safeGo("daemon-stop", func() {
			time.Sleep(100 * time.Millisecond)
			if err := d.Stop(); err != nil {
				d.logger.Error("Daemon stop returned error: %v", err)
			}
		})
		return socket.SuccessResponse("Daemon stopping")

	case "list_repos":
		return d.handleListRepos(req)

	case "add_repo":
		return d.handleAddRepo(req)

	case "remove_repo":
		return d.handleRemoveRepo(req)

	case "add_agent":
		return d.handleAddAgent(req)

	case "start_worker":
		return d.handleStartWorker(req)

	case "send_agent_input":
		return d.handleSendAgentInput(req)

	case "agent_input":
		return d.handleAgentInput(req)

	case "interrupt_agent":
		return d.handleInterruptAgent(req)

	case "escape_agent":
		return d.handleEscapeAgent(req)

	case "remove_agent":
		return d.handleRemoveAgent(req)

	case "list_agents":
		return d.handleListAgents(req)

	case "complete_agent":
		return d.handleCompleteAgent(req)

	case "agent_waiting":
		return d.handleAgentWaiting(req)

	case "restart_agent":
		return d.handleRestartAgent(req)
	case "restart_browser_agent":
		return d.handleRestartBrowserAgent(req)
	case "set_agent_model":
		return d.handleSetAgentModel(req)

	case "trigger_cleanup":
		return d.handleTriggerCleanup(req)

	case "repair_state":
		return d.handleRepairState(req)

	case "get_repo_config":
		return d.handleGetRepoConfig(req)

	case "update_repo_config":
		return d.handleUpdateRepoConfig(req)

	case "set_current_repo":
		return d.handleSetCurrentRepo(req)

	case "get_current_repo":
		return d.handleGetCurrentRepo(req)

	case "clear_current_repo":
		return d.handleClearCurrentRepo(req)

	case "route_messages":
		d.triggerRouteMessages()
		return socket.SuccessResponse("Message routing triggered")

	case "task_history":
		return d.handleTaskHistory(req)

	case "spawn_agent":
		return d.handleSpawnAgent(req)

	case "trigger_refresh":
		return d.handleTriggerRefresh(req)

	case "trigger_sync":
		return d.handleTriggerSync(req)

	case "reset_nudge":
		return d.handleResetNudge(req)

	case "start_repo_agents":
		return d.handleStartRepoAgents(req)

	case "start_verification":
		return d.handleStartVerification(req)

	case "start_verification_agent":
		return d.handleStartVerificationAgent(req)

	case "verification_verdict":
		return d.handleVerificationVerdict(req)

	case "reload_model_profiles":
		return d.handleReloadModelProfiles(req)

	default:
		return socket.ErrorResponse("unknown command: %q. Run 'oat --help' for available commands", req.Command)
	}
}

// handleStatus returns daemon status
func (d *Daemon) handleStatus(req socket.Request) socket.Response {
	repos := d.state.GetAllRepos()
	agentCount := 0
	idleRepos := make([]string, 0)
	activeRepos := make([]string, 0)
	for name, repo := range repos {
		agentCount += len(repo.Agents)
		if repo.IdleMode {
			idleRepos = append(idleRepos, name)
		} else {
			activeRepos = append(activeRepos, name)
		}
	}

	return socket.SuccessResponse(map[string]interface{}{
		"running":      true,
		"pid":          os.Getpid(),
		"repos":        len(repos),
		"agents":       agentCount,
		"socket_path":  d.paths.DaemonSock,
		"idle_repos":   idleRepos,
		"active_repos": activeRepos,
	})
}

// handleListRepos lists all repositories with detailed status
func (d *Daemon) handleListRepos(req socket.Request) socket.Response {
	repos := d.state.GetAllRepos()

	// Part 5c: virtual repos (IsVirtual == true; today only
	// `_assistant-<name>` repos) are hidden from the default
	// `oat repo list` output. The user shouldn't have to scroll
	// past their personal assistant's bookkeeping when listing
	// real source-code repos. `include_virtual` (default false) is
	// the opt-in toggle the CLI flips when the user passes
	// `oat repo list --all`. The simple (non-rich) backward-compat
	// branch honors the same filter so CLI verbs that just want
	// the name set don't accidentally enumerate assistants.
	includeVirtual := getOptionalBoolArg(req.Args, "include_virtual", false)

	// Check if rich format is requested
	rich := getOptionalBoolArg(req.Args, "rich", false)
	if !rich {
		// Return simple list for backward compatibility
		repoNames := make([]string, 0, len(repos))
		for name, repo := range repos {
			if !includeVirtual && repo.IsVirtual {
				continue
			}
			repoNames = append(repoNames, name)
		}
		return socket.SuccessResponse(repoNames)
	}

	// Return detailed repo info
	repoDetails := make([]map[string]interface{}, 0, len(repos))
	for repoName, repo := range repos {
		if !includeVirtual && repo.IsVirtual {
			continue
		}
		// Count agents by type
		workerCount := 0
		swappedModelCount := 0
		totalAgents := len(repo.Agents)
		for _, agent := range repo.Agents {
			if agent.Type == state.AgentTypeWorker {
				workerCount++
			}
			if agent.ModelSwappedOnRestart {
				swappedModelCount++
			}
		}

		// Check session health
		sessionHealthy := false
		if hasSession, err := d.backend.HasSession(d.ctx, repo.SessionName); err == nil {
			sessionHealthy = hasSession
		}

		// Determine PR management mode
		prManagementMode := "merge-queue"
		if repo.ForkConfig.IsFork {
			prManagementMode = "pr-shepherd"
		}

		repoDetails = append(repoDetails, map[string]interface{}{
			"name":               repoName,
			"github_url":         repo.GithubURL,
			"session_name":       repo.SessionName,
			"total_agents":       totalAgents,
			"worker_count":       workerCount,
			"session_healthy":    sessionHealthy,
			"is_fork":            repo.ForkConfig.IsFork,
			"upstream_owner":     repo.ForkConfig.UpstreamOwner,
			"upstream_repo":      repo.ForkConfig.UpstreamRepo,
			"pr_management_mode":  prManagementMode,
			"idle_mode":           repo.IdleMode,
			"swapped_model_count": swappedModelCount,
			"is_virtual":          repo.IsVirtual,
		})
	}

	return socket.SuccessResponse(repoDetails)
}

// handleAddRepo adds a new repository
func (d *Daemon) handleAddRepo(req socket.Request) socket.Response {
	name, errResp, ok := getRequiredStringArg(req.Args, "name", "repository name is required (e.g., 'my-project')")
	if !ok {
		return errResp
	}

	// Part 5c: virtual repos have no GitHub remote. The CLI's
	// virtual-repo helper passes `is_virtual=true` along with an
	// empty `github_url`, so we must read `is_virtual` BEFORE the
	// github_url requirement check and bypass it for virtual repos.
	// Validated separately below; the empty string is the only
	// permitted github_url value when is_virtual=true.
	isVirtualArg := getOptionalBoolArg(req.Args, "is_virtual", false)
	var githubURL string
	if isVirtualArg {
		githubURL = getOptionalStringArg(req.Args, "github_url", "")
		if githubURL != "" {
			return socket.ErrorResponse("virtual repositories must not have a github_url (got %q); they exist purely as containers for assistant-style agents", githubURL)
		}
	} else {
		var resp socket.Response
		githubURL, resp, ok = getRequiredStringArg(req.Args, "github_url", "GitHub repository URL is required (e.g., 'https://github.com/owner/repo')")
		if !ok {
			return resp
		}
	}

	sessionName, errResp, ok := getRequiredStringArg(req.Args, "session_name", "session name is required")
	if !ok {
		return errResp
	}

	// Parse merge queue configuration (optional, defaults to enabled with "all" tracking)
	mqConfig := state.DefaultMergeQueueConfig()
	if mqEnabled, hasMqEnabled := req.Args["mq_enabled"].(bool); hasMqEnabled {
		mqConfig.Enabled = mqEnabled
	}
	if mqTrackMode := getOptionalStringArg(req.Args, "mq_track_mode", ""); mqTrackMode != "" {
		mode, err := state.ParseTrackMode(mqTrackMode)
		if err != nil {
			return socket.ErrorResponse("%s", err.Error())
		}
		mqConfig.TrackMode = mode
	}

	// Parse fork configuration (optional)
	forkConfig := state.ForkConfig{
		IsFork:        getOptionalBoolArg(req.Args, "is_fork", false),
		UpstreamURL:   getOptionalStringArg(req.Args, "upstream_url", ""),
		UpstreamOwner: getOptionalStringArg(req.Args, "upstream_owner", ""),
		UpstreamRepo:  getOptionalStringArg(req.Args, "upstream_repo", ""),
	}

	// Parse PR shepherd configuration (optional, defaults for fork mode)
	psConfig := state.DefaultPRShepherdConfig()
	if psEnabled, hasPsEnabled := req.Args["ps_enabled"].(bool); hasPsEnabled {
		psConfig.Enabled = psEnabled
	}
	if psTrackMode := getOptionalStringArg(req.Args, "ps_track_mode", ""); psTrackMode != "" {
		mode, err := state.ParseTrackMode(psTrackMode)
		if err != nil {
			return socket.ErrorResponse("%s", err.Error())
		}
		psConfig.TrackMode = mode
	}

	// If in fork mode, disable merge-queue and enable pr-shepherd by default
	if forkConfig.IsFork {
		mqConfig.Enabled = false
		psConfig.Enabled = true
	}

	// Part 5c: `is_virtual` was already parsed at the top of the
	// handler (so it can gate the `github_url` requirement check);
	// reuse the value here. When true the daemon stores the repo
	// as-is but the CLI's preflight has skipped `git clone`,
	// `gh label create`, fork detection, merge-queue / pr-shepherd
	// registration, etc. -- everything that assumes a real git
	// remote. mqConfig / psConfig fields are still persisted
	// (Enabled defaults to true) so that if a virtual repo is ever
	// flipped to non-virtual the merge-queue / pr-shepherd configs
	// are sensible; but the daemon's spawn paths gate on IsVirtual
	// before reading them.
	isVirtual := isVirtualArg

	repo := &state.Repository{
		GithubURL:        githubURL,
		SessionName:      sessionName,
		Agents:           make(map[string]state.Agent),
		MergeQueueConfig: mqConfig,
		PRShepherdConfig: psConfig,
		ForkConfig:       forkConfig,
		Model:            getOptionalStringArg(req.Args, "model", ""),
		IsVirtual:        isVirtual,
	}

	if err := d.state.AddRepo(name, repo); err != nil {
		return socket.ErrorResponse("%s", err.Error())
	}

	if isVirtual {
		d.logger.Info("Added virtual repository: %s (no git remote; for assistant-style agents)", name)
	} else if forkConfig.IsFork {
		d.logger.Info("Added repository: %s (fork of %s/%s, pr-shepherd: enabled=%v)", name, forkConfig.UpstreamOwner, forkConfig.UpstreamRepo, psConfig.Enabled)
	} else {
		d.logger.Info("Added repository: %s (merge queue: enabled=%v, track=%s)", name, mqConfig.Enabled, mqConfig.TrackMode)
	}
	return socket.SuccessResponse(nil)
}

// handleRemoveRepo removes a repository from state
func (d *Daemon) handleRemoveRepo(req socket.Request) socket.Response {
	name, errResp, ok := getRequiredStringArg(req.Args, "name", "repository name is required")
	if !ok {
		return errResp
	}

	if err := d.state.RemoveRepo(name); err != nil {
		return socket.ErrorResponse("%s", err.Error())
	}

	d.logger.Info("Removed repository: %s", name)
	return socket.SuccessResponse(nil)
}

// handleAddAgent adds a new agent
func (d *Daemon) handleAddAgent(req socket.Request) socket.Response {
	repoName, errResp, ok := getRequiredStringArg(req.Args, "repo", "repository name is required")
	if !ok {
		return errResp
	}

	agentName, errResp, ok := getRequiredStringArg(req.Args, "agent", "agent name is required")
	if !ok {
		return errResp
	}

	agentTypeStr, errResp, ok := getRequiredStringArg(req.Args, "type", "agent type is required (supervisor, worker, merge-queue, or reviewer)")
	if !ok {
		return errResp
	}

	worktreePath, errResp, ok := getRequiredStringArg(req.Args, "worktree_path", "path to the agent's git worktree is required")
	if !ok {
		return errResp
	}

	windowName, errResp, ok := getRequiredStringArg(req.Args, "window_name", "window name is required")
	if !ok {
		return errResp
	}

	// Get session ID from args or generate one
	sessionID, _, ok := getRequiredStringArg(req.Args, "session_id", "")
	if !ok {
		sessionID = fmt.Sprintf("agent-%d", time.Now().UnixNano())
	}

	// Get PID from args (optional)
	var pid int
	if pidFloat, ok := req.Args["pid"].(float64); ok {
		pid = int(pidFloat)
	} else if pidInt, ok := req.Args["pid"].(int); ok {
		pid = pidInt
	}

	agent := state.Agent{
		Type:         state.AgentType(agentTypeStr),
		WorktreePath: worktreePath,
		WindowName:   windowName,
		SessionID:    sessionID,
		PID:          pid,
		CreatedAt:    time.Now(),
	}

	// Optional task field for workers
	agent.Task = getOptionalStringArg(req.Args, "task", "")

	// Optional issue number and URL for issue-tied workers
	agent.IssueNumber = getOptionalStringArg(req.Args, "issue_number", "")
	agent.IssueURL = getOptionalStringArg(req.Args, "issue_url", "")

	// Optional model override for this specific agent. Validated against
	// loaded profiles before persisting so a typo here doesn't spawn a
	// misconfigured agent that the next restart silently auto-swaps.
	// The canonical (always-prefixed) form is what gets persisted so state
	// converges on the same shape as `oat model onboard` registrations.
	if rawModel := getOptionalStringArg(req.Args, "model", ""); rawModel != "" {
		repo, _ := d.state.GetRepo(repoName)
		role := routing.RoleWorker
		switch agent.Type {
		case state.AgentTypeSupervisor, state.AgentTypeWorkspace, state.AgentTypeMergeQueue, state.AgentTypePRShepherd:
			role = routing.RoleOrchestrator
		}
		if d.modelProfiles != nil && d.modelProfiles.Count() > 0 {
			canonical, vErr := d.modelProfiles.ValidateAndCanonicalize(rawModel, role)
			if vErr != nil {
				return socket.ErrorResponse("model %q rejected: %s", rawModel, vErr.Error())
			}
			if role == routing.RoleWorker && len(repo.AllowedWorkerModels) > 0 {
				found := false
				for _, m := range repo.AllowedWorkerModels {
					if m == rawModel || m == canonical {
						found = true
						break
					}
				}
				if !found {
					return socket.ErrorResponse("model %q is not in the allowed worker models for repo %q — update with: oat config %s worker-models add %s", canonical, repoName, repoName, canonical)
				}
			}
			agent.Model = canonical
		} else {
			// No profiles loaded — passthrough (matches existing behavior
			// elsewhere in the daemon). Operator can still onboard later
			// and the next restart will canonicalize.
			agent.Model = rawModel
		}
	}

	if err := d.state.AddAgent(repoName, agentName, agent); err != nil {
		return socket.ErrorResponse("%s", err.Error())
	}

	d.logger.Info("Added agent %s to repo %s", agentName, repoName)
	return socket.SuccessResponse(nil)
}

// handleSetAgentModel updates the model an agent uses. Validates the
// new model against loaded profiles (so a typo fails here rather than
// silently triggering a swap on the next restart), then writes the
// canonical form to state.json via ModifyAgent so the change is
// atomic and survives concurrent updates. The agent process is NOT
// restarted by this handler — the agent only re-reads its model
// configuration on (re)spawn. The CLI's --restart flag chains a
// follow-up restart_agent call after this returns.
//
// Why a dedicated handler vs. extending add_agent: add_agent is a
// create-only handler that fails on duplicates. The set-model flow
// needs the opposite semantics — the agent must already exist, and
// the rest of its config (worktree, session, PID, etc.) must be left
// alone. ModifyAgent gives the atomic-update primitive without
// risking a stale-copy clobber of unrelated fields.
func (d *Daemon) handleSetAgentModel(req socket.Request) socket.Response {
	repoName, errResp, ok := getRequiredStringArg(req.Args, "repo", "repository name is required")
	if !ok {
		return errResp
	}

	agentName, errResp, ok := getRequiredStringArg(req.Args, "agent", "agent name is required")
	if !ok {
		return errResp
	}

	rawModel, errResp, ok := getRequiredStringArg(req.Args, "model", "model id is required (e.g. anthropic:claude-opus-4-7)")
	if !ok {
		return errResp
	}

	// Look up the agent first — fail fast with a clear error if the
	// repo or agent doesn't exist (the CLI's preflight should have
	// caught this, but the handler must be safe to call directly).
	repo, exists := d.state.GetRepo(repoName)
	if !exists {
		return socket.ErrorResponse("repository %q not found", repoName)
	}
	agent, agentExists := repo.Agents[agentName]
	if !agentExists {
		return socket.ErrorResponse("agent %q not found in repository %q", agentName, repoName)
	}

	// Validate the new model against loaded profiles. Reject typos
	// here rather than silently letting a misconfigured agent
	// auto-swap on the next restart (same logic as handleAddAgent's
	// model-override branch). The canonical (always-prefixed) form
	// is what gets persisted so state converges on the same shape
	// as `oat model onboard` registrations.
	role := routing.RoleWorker
	switch agent.Type {
	case state.AgentTypeSupervisor, state.AgentTypeWorkspace, state.AgentTypeMergeQueue, state.AgentTypePRShepherd:
		role = routing.RoleOrchestrator
	}
	canonical := rawModel
	if d.modelProfiles != nil && d.modelProfiles.Count() > 0 {
		c, vErr := d.modelProfiles.ValidateAndCanonicalize(rawModel, role)
		if vErr != nil {
			return socket.ErrorResponse(
				"model %q rejected: %s — run `oat model onboard %s` first if this is a new model",
				rawModel, vErr.Error(), rawModel,
			)
		}
		if role == routing.RoleWorker && len(repo.AllowedWorkerModels) > 0 {
			found := false
			for _, m := range repo.AllowedWorkerModels {
				if m == rawModel || m == c {
					found = true
					break
				}
			}
			if !found {
				return socket.ErrorResponse(
					"model %q is not in the allowed worker models for repo %q — update with: oat config %s worker-models add %s",
					c, repoName, repoName, c,
				)
			}
		}
		canonical = c
	}
	// Always run through ModifyAgent so two things happen
	// atomically when both are needed: (a) the model swap, and
	// (b) the clearing of any auto-swap markers from a previous
	// restart-on-missing-model recovery. The plan body (cli-
	// agent-set-model) is explicit that "the operator's explicit
	// choice supersedes any prior auto-swap" — once the operator
	// has actively picked a model, the marker is misleading even
	// when the picked model equals the auto-swap fallback (in
	// which case the operator has implicitly endorsed the
	// fallback as their choice). The no-op "model didn't change"
	// case still flips the markers off if they were set; the
	// response surfaces both signals so the CLI can render the
	// right wording.
	priorModel := agent.Model
	// Whether the agent is currently running. The CLI uses this to
	// make the "you need to restart" nudge explicit ("agent is still
	// running on the old model" vs the generic "restart for this to
	// take effect"). PID > 0 in state.json is the same liveness
	// signal the rest of the daemon's lifecycle code reads -- it
	// can lag a recently-died process briefly, but the worst case
	// here is a stale "still running" hint, which is cheap.
	wasRunning := agent.PID > 0
	var (
		changedModel   bool
		clearedMarkers bool
	)
	if err := d.state.ModifyAgent(repoName, agentName, func(a *state.Agent) {
		if a.Model != canonical {
			a.Model = canonical
			changedModel = true
		}
		if a.ModelSwappedOnRestart || a.ModelSwapReason != "" || a.ModelSwapPrevious != "" {
			a.ModelSwappedOnRestart = false
			a.ModelSwapReason = ""
			a.ModelSwapPrevious = ""
			clearedMarkers = true
		}
	}); err != nil {
		return socket.ErrorResponse("failed to update agent model: %s", err.Error())
	}

	if changedModel {
		d.logger.Info("Set agent %s/%s model: %q -> %q (cleared_swap_markers=%t)", repoName, agentName, priorModel, canonical, clearedMarkers)
	} else if clearedMarkers {
		d.logger.Info("Set agent %s/%s model: no model change (already %q); cleared auto-swap markers", repoName, agentName, canonical)
	}
	return socket.SuccessResponse(map[string]interface{}{
		"prior_model": priorModel,
		"new_model":   canonical,
		"changed":     changedModel,
		// Agent only picks up model changes on (re)spawn. CLI
		// uses this to decide whether to nudge the user about
		// the --restart flag. No-op-model-but-marker-cleared
		// case does NOT require a restart (state already
		// reflects the running agent's model).
		"requires_restart":     changedModel,
		"cleared_swap_markers": clearedMarkers,
		// Agent liveness at the moment the change was persisted.
		// CLI uses this to make the post-set-model nudge explicit
		// ("agent is still running on the old model" instead of
		// the generic "restart for this to take effect"). Read
		// from state.Agent.PID -- same signal the rest of the
		// daemon's lifecycle code uses.
		"was_running": wasRunning,
	})
}

// handleStartWorker starts a worker process via the daemon backend and registers
// it in state atomically. This is required for direct backend mode where the
// daemon must own the PTY/process lifecycle.
func (d *Daemon) handleStartWorker(req socket.Request) socket.Response {
	repoName, errResp, ok := getRequiredStringArg(req.Args, "repo", "repository name is required")
	if !ok {
		return errResp
	}

	agentName, errResp, ok := getRequiredStringArg(req.Args, "agent", "agent name is required")
	if !ok {
		return errResp
	}

	worktreePath, errResp, ok := getRequiredStringArg(req.Args, "worktree_path", "path to the agent's git worktree is required")
	if !ok {
		return errResp
	}

	promptFile, errResp, ok := getRequiredStringArg(req.Args, "prompt_file", "prompt file path is required")
	if !ok {
		return errResp
	}

	task := getOptionalStringArg(req.Args, "task", "")
	model := getOptionalStringArg(req.Args, "model", "")
	sessionID := getOptionalStringArg(req.Args, "session_id", "")
	issueNumber := getOptionalStringArg(req.Args, "issue_number", "")
	issueURL := getOptionalStringArg(req.Args, "issue_url", "")

	repo, exists := d.state.GetRepo(repoName)
	if !exists {
		return socket.ErrorResponse("repository '%s' not found", repoName)
	}

	if _, exists := d.state.GetAgent(repoName, agentName); exists {
		return socket.ErrorResponse("agent '%s' already exists in repository '%s'", agentName, repoName)
	}

	// Ensure session exists. Direct backend auto-creates on StartAgent,
	// but explicit create keeps behavior consistent across backends.
	hasSession, err := d.backend.HasSession(d.ctx, repo.SessionName)
	if err != nil {
		return socket.ErrorResponse("failed to check session '%s': %v", repo.SessionName, err)
	}
	if !hasSession {
		if err := d.backend.CreateSession(d.ctx, repo.SessionName); err != nil {
			return socket.ErrorResponse("failed to create session '%s': %v", repo.SessionName, err)
		}
	}

	initialMessage := ""
	if task != "" {
		initialMessage = "Task: " + task
	}

	if err := d.startAgentWithConfig(repoName, repo, agentStartConfig{
		agentName:      agentName,
		agentType:      state.AgentTypeWorker,
		promptFile:     promptFile,
		workDir:        worktreePath,
		model:          model,
		initialMessage: initialMessage,
	}); err != nil {
		return socket.ErrorResponse("failed to start worker: %v", err)
	}

	// Update worker metadata not covered by startAgentWithConfig.
	agent, exists := d.state.GetAgent(repoName, agentName)
	if !exists {
		return socket.ErrorResponse("worker '%s' was started but not found in state", agentName)
	}
	agent.Task = task
	agent.IssueNumber = issueNumber
	agent.IssueURL = issueURL
	if model != "" {
		agent.Model = model
	}
	if sessionID != "" {
		agent.SessionID = sessionID
	}
	if v := getOptionalInt64Arg(req.Args, "max_tokens_budget"); v > 0 {
		agent.MaxTokens = v
	}
	if err := d.state.UpdateAgent(repoName, agentName, agent); err != nil {
		return socket.ErrorResponse("failed to update worker metadata: %v", err)
	}

	// Trigger immediate wake pass so the new worker gets nudged right away
	// instead of waiting for the next polling tick.
	d.triggerWake()

	return socket.SuccessResponse(map[string]interface{}{
		"agent": agentName,
		"repo":  repoName,
		"pid":   agent.PID,
	})
}

// handleSendAgentInput sends a one-shot line of input to a running agent.
// This enables operator control from a regular shell (without attaching to the session).
func (d *Daemon) handleSendAgentInput(req socket.Request) socket.Response {
	repoName, errResp, ok := getRequiredStringArg(req.Args, "repo", "repository name is required")
	if !ok {
		return errResp
	}
	agentName, errResp, ok := getRequiredStringArg(req.Args, "agent", "agent name is required")
	if !ok {
		return errResp
	}
	message, errResp, ok := getRequiredStringArg(req.Args, "message", "message is required")
	if !ok {
		return errResp
	}

	repo, exists := d.state.GetRepo(repoName)
	if !exists {
		return socket.ErrorResponse("repository '%s' not found", repoName)
	}
	agent, exists := d.state.GetAgent(repoName, agentName)
	if !exists {
		return socket.ErrorResponse("agent '%s' not found in repository '%s'", agentName, repoName)
	}

	if err := d.backend.SendMessage(d.ctx, repo.SessionName, agent.WindowName, message); err != nil {
		if errors.Is(err, backend_pkg.ErrAgentAdopted) {
			return socket.ErrorResponse("agent '%s' was re-adopted after daemon restart and needs to be restarted before it can accept input", agentName)
		}
		return socket.ErrorResponse("failed to send input to agent '%s': %v", agentName, err)
	}

	d.logger.Debug("Sent direct input to %s/%s", repoName, agentName)
	return socket.SuccessResponse(nil)
}

// handleAgentInput is the side-panel chat path's PTY-injection verb.
// Distinct from handleSendAgentInput because the caller (the
// oat-browser-agent bridge) addresses agents by (session, agent_name)
// instead of (repo, agent) — the bridge only knows the env vars set
// by buildBrowserAgentMCPConfig (Part 2a) and can't trivially derive
// the repo name. The verb is restricted to AgentTypeBrowser so a
// malicious bridge can't use it to inject text into the supervisor's
// or a worker's PTY.
//
// Input sanitization runs at this entry point so EVERY caller gets
// the same filter applied (including the synthetic 95 % compaction
// inject in Part 5e, which will route through here too). See
// internal/socket/sanitize.go for the rule set.
func (d *Daemon) handleAgentInput(req socket.Request) socket.Response {
	sessionName, errResp, ok := getRequiredStringArg(req.Args, "session", "session name is required")
	if !ok {
		return errResp
	}
	agentName, errResp, ok := getRequiredStringArg(req.Args, "agent", "agent name is required")
	if !ok {
		return errResp
	}
	// `text` is required when interrupt is false, AND when interrupt is
	// true (the only valid value is "\x03"). Either way the field must
	// be present; getRequiredStringArg already returns a structured
	// error if it's missing.
	text, errResp, ok := getRequiredStringArg(req.Args, "text", "text is required")
	if !ok {
		return errResp
	}

	// `interrupt` is optional and bool. Default false. We accept either
	// JSON bool or "true"/"false" string (some socket clients stringify
	// args; the existing dispatch table is forgiving about this).
	interrupt := false
	if raw, present := req.Args["interrupt"]; present {
		switch v := raw.(type) {
		case bool:
			interrupt = v
		case string:
			interrupt = strings.EqualFold(v, "true")
		}
	}

	// Look up the repo whose SessionName matches. Linear scan because
	// repos are typically O(1–3) per daemon and adding an index would
	// add a sync invariant we don't need yet. If repo-count grows we
	// can promote this to a map in a follow-up.
	repoName, repo, found := d.findRepoBySession(sessionName)
	if !found {
		return socket.ErrorResponse("no repository is bound to session %q", sessionName)
	}
	agent, exists := d.state.GetAgent(repoName, agentName)
	if !exists {
		return socket.ErrorResponse("agent '%s' not found in session %q", agentName, sessionName)
	}
	// Security boundary: agent_input is the side-panel chat path and
	// is allowed to address browser agents ONLY. Restricting at the
	// daemon edge means a misconfigured bridge or a malicious WS
	// peer can't escalate to "inject prompt text into the supervisor."
	if !usesBrowserBridge(agent.Type) {
		return socket.ErrorResponse("agent_input is restricted to browser-bridge agent types (browser, assistant); %s/%s is %s", repoName, agentName, agent.Type)
	}

	sanitized, sErr := socket.SanitizePTYInput(text, socket.SanitizeOpts{AllowInterrupt: interrupt})
	if sErr != nil {
		// Log the unsafe input length so the operator can correlate
		// rejections with bridge-side issues, but don't echo the
		// content (we just declared it suspect).
		d.logger.Warn("agent_input rejected for %s/%s (len=%d, interrupt=%v): %v", repoName, agentName, len(text), interrupt, sErr)
		return socket.ErrorResponse("input rejected by sanitizer: %v", sErr)
	}
	if sanitized == "" && !interrupt {
		// All bytes stripped (e.g. input was a lone ANSI escape with
		// no payload). Surface to the caller so the side panel can
		// keep the user's draft visible instead of optimistically
		// rendering an empty bubble.
		return socket.ErrorResponse("input was empty after sanitization")
	}

	// Part 2g: prefix non-interrupt user input from the side panel with
	// a sentinel so the agent's prompt can disambiguate "this is the
	// side-panel user chatting" from "this is an inter-agent message
	// via `oat message send`". The sentinel is plain ASCII; the
	// sanitizer above only ran on the user-supplied bytes so the
	// prefix is guaranteed not to be re-stripped. Interrupts (\x03)
	// are passed through unchanged — that path doesn't carry text.
	//
	// Part 4.K: when the side panel attaches an active_tab_id, insert
	// `[active-tab-id: <N>] ` between the sentinel and the user's
	// text. The agent prompt (browser.md) is taught to read this as
	// "the user's last-focused tab id when they sent this message,"
	// removing the chrome.tabs.query({}) ambiguity that caused the
	// agent to act on the wrong tab when multiple windows are open.
	if !interrupt {
		// Part 4.K diagnostic (added 2026-05-21): log the inbound
		// active_tab_id value at INFO so we can correlate "side
		// panel said X" with "daemon saw X" without tailing
		// per-process console.* in the extension/bridge. User
		// reported every [SIDE-PANEL CHAT] line reaching the
		// agent prompt without an [active-tab-id: N] prefix
		// despite the upstream wire chain looking correct — the
		// next retest's daemon.log line tells us whether the
		// daemon is dropping the field, never receiving it, or
		// receiving and silently passing it through. Demote to
		// d.logger.Debug once the active-tab-id flow is
		// confirmed end-to-end.
		rawTabID := req.Args["active_tab_id"]
		prefix := buildActiveTabPrefix(rawTabID)
		d.logger.Info(
			"agent_input active_tab_id for %s/%s: raw=%v type=%T → prefix=%q",
			repoName, agentName, rawTabID, rawTabID, prefix,
		)
		sanitized = sidePanelInputSentinel + prefix + sanitized
	}

	if err := d.backend.SendMessage(d.ctx, repo.SessionName, agent.WindowName, sanitized); err != nil {
		if errors.Is(err, backend_pkg.ErrAgentAdopted) {
			return socket.ErrorResponse("agent '%s' was re-adopted after daemon restart and needs to be restarted before it can accept input", agentName)
		}
		return socket.ErrorResponse("failed to send input to agent '%s': %v", agentName, err)
	}
	d.logger.Debug("agent_input delivered to %s/%s (interrupt=%v, len=%d)", repoName, agentName, interrupt, len(sanitized))
	return socket.SuccessResponse(nil)
}

// findRepoBySession returns (repoName, *repository, true) for the
// repo whose SessionName matches; (_, nil, false) otherwise. Linear
// scan over d.state.Repos under the state mutex.
func (d *Daemon) findRepoBySession(sessionName string) (string, *state.Repository, bool) {
	for _, name := range d.state.ListRepos() {
		r, ok := d.state.GetRepo(name)
		if !ok {
			continue
		}
		if r.SessionName == sessionName {
			return name, r, true
		}
	}
	return "", nil, false
}

func (d *Daemon) handleInterruptAgent(req socket.Request) socket.Response {
	repoName, errResp, ok := getRequiredStringArg(req.Args, "repo", "repository name is required")
	if !ok {
		return errResp
	}
	agentName, errResp, ok := getRequiredStringArg(req.Args, "agent", "agent name is required")
	if !ok {
		return errResp
	}

	repo, exists := d.state.GetRepo(repoName)
	if !exists {
		return socket.ErrorResponse("repository '%s' not found", repoName)
	}
	agent, exists := d.state.GetAgent(repoName, agentName)
	if !exists {
		return socket.ErrorResponse("agent '%s' not found in repository '%s'", agentName, repoName)
	}

	if err := d.backend.SendInterrupt(d.ctx, repo.SessionName, agent.WindowName); err != nil {
		return socket.ErrorResponse("failed to interrupt agent '%s': %v", agentName, err)
	}

	d.logger.Debug("Sent interrupt to %s/%s", repoName, agentName)
	return socket.SuccessResponse(nil)
}

// handleEscapeAgent sends the Escape key to an agent to cancel "Thinking..." state.
func (d *Daemon) handleEscapeAgent(req socket.Request) socket.Response {
	repoName, errResp, ok := getRequiredStringArg(req.Args, "repo", "repository name is required")
	if !ok {
		return errResp
	}
	agentName, errResp, ok := getRequiredStringArg(req.Args, "agent", "agent name is required")
	if !ok {
		return errResp
	}

	repo, exists := d.state.GetRepo(repoName)
	if !exists {
		return socket.ErrorResponse("repository '%s' not found", repoName)
	}
	agent, exists := d.state.GetAgent(repoName, agentName)
	if !exists {
		return socket.ErrorResponse("agent '%s' not found in repository '%s'", agentName, repoName)
	}

	if err := d.backend.SendEscape(d.ctx, repo.SessionName, agent.WindowName); err != nil {
		return socket.ErrorResponse("failed to send escape to agent '%s': %v", agentName, err)
	}

	d.logger.Debug("Sent escape to %s/%s", repoName, agentName)
	return socket.SuccessResponse(nil)
}

// handleRemoveAgent kills an agent process and removes it from state.
func (d *Daemon) handleRemoveAgent(req socket.Request) socket.Response {
	repoName, errResp, ok := getRequiredStringArg(req.Args, "repo", "repository name is required")
	if !ok {
		return errResp
	}

	agentName, errResp, ok := getRequiredStringArg(req.Args, "agent", "agent name is required")
	if !ok {
		return errResp
	}

	// Kill the agent process via the daemon's backend before removing state.
	// The daemon backend owns the PTY, so it can reliably terminate the process.
	agent, agentExists := d.state.GetAgent(repoName, agentName)
	if agentExists {
		repo, repoExists := d.state.GetRepo(repoName)
		if repoExists {
			windowName := agent.WindowName
			if windowName == "" {
				windowName = agentName
			}
			if err := d.backend.StopAgent(d.ctx, repo.SessionName, windowName); err != nil {
				d.logger.Warn("Failed to stop agent process %s/%s: %v", repoName, agentName, err)
			} else {
				d.logger.Info("Stopped agent process %s/%s", repoName, agentName)
			}
			// Part 2g: stop the assistant-turn tailer for browser-agents
			// so the next add+start spins up a fresh tailer rather than
			// leaking a goroutine and a stale broadcaster.
			if usesBrowserBridge(agent.Type) {
				d.stopAssistantTurnTailer(repo.SessionName, agentName)
			}
		}
	}

	// Log the outcome BEFORE removing from state (the logger reads agent fields).
	// Workers killed without having reached ReadyForCleanup are recorded as "removed"
	// so the replay harness can distinguish crash/timeout from graceful completion.
	if agentExists && (agent.Type == state.AgentTypeWorker || agent.Type == state.AgentTypeReview || agent.Type == state.AgentTypeVerification) {
		outcome := "removed"
		if agent.ReadyForCleanup {
			// Already logged at handleCompleteAgent — don't double-log.
			outcome = ""
		}
		if outcome != "" {
			// Reason source priority: explicit `reason` arg (supervisor /
			// budget-cap / re-route flows pass it) → fall back to "manual"
			// for the bare CLI `oat agent remove` path.
			reason := getOptionalStringArg(req.Args, "reason", RemovalReasonManual)
			d.logOutcome(repoName, agentName, agent, outcome, reason)
		}
	}

	if err := d.state.RemoveAgent(repoName, agentName); err != nil {
		return socket.ErrorResponse("%s", err.Error())
	}

	d.logger.Info("Removed agent %s from repo %s", agentName, repoName)

	// If a worker with an unfinished task was removed (not through normal
	// completion), notify workspace so it can spawn a replacement.
	if agentExists && agent.Type == state.AgentTypeWorker && !agent.ReadyForCleanup && agent.Task != "" {
		msgMgr := d.getMessageManager()
		msg := fmt.Sprintf("[daemon] Worker '%s' was removed before completing task: '%s'.", agentName, agent.Task)
		if agent.IssueNumber != "" {
			msg += fmt.Sprintf(" (Issue #%s)", agent.IssueNumber)
		}
		if agent.PRNumber > 0 {
			msg += fmt.Sprintf(" NOTE: Worker had PR #%d. Check its status with `gh pr view %d --json state,mergeStateStatus` before spawning a replacement -- if the PR is still open or was already merged, a replacement is unnecessary.", agent.PRNumber, agent.PRNumber)
		} else {
			msg += " No PR was created. Consider spawning a replacement worker for this task."
		}
		if _, err := msgMgr.Send(repoName, "daemon", "default", msg); err != nil {
			d.logger.Warn("Failed to notify workspace about removed worker %s: %v", agentName, err)
		}
	}

	return socket.SuccessResponse(nil)
}

// handleListAgents lists agents for a repository
func (d *Daemon) handleListAgents(req socket.Request) socket.Response {
	repoName, errResp, ok := getRequiredStringArg(req.Args, "repo", "repository name is required")
	if !ok {
		return errResp
	}

	agents, err := d.state.ListAgents(repoName)
	if err != nil {
		return socket.ErrorResponse("%s", err.Error())
	}

	// Check if rich format is requested
	rich := getOptionalBoolArg(req.Args, "rich", false)

	// Get repository to check session
	repo, repoExists := d.state.GetRepo(repoName)

	// Get full agent details
	agentDetails := make([]map[string]interface{}, 0, len(agents))
	for _, agentName := range agents {
		agent, exists := d.state.GetAgent(repoName, agentName)
		if !exists {
			continue
		}

		detail := map[string]interface{}{
			"name":          agentName,
			"type":          agent.Type,
			"worktree_path": agent.WorktreePath,
			"window_name":   agent.WindowName,
			"task":          agent.Task,
			"summary":       agent.Summary,
			"model":         agent.Model,
			"created_at":    agent.CreatedAt,
			"pid":           agent.PID,
		}

		// Add rich status information if requested
		if rich {
			// Determine agent status
			status := "unknown"
			if agent.ReadyForCleanup {
				status = "completed"
			} else if agent.WaitingForVerification {
				status = "waiting for verification"
			} else if agent.WaitingForPR {
				status = "waiting for PR"
			} else if repoExists {
				// Check if process is alive (means agent is running)
				hasWindow, err := d.backend.IsAgentAlive(d.ctx, repo.SessionName, agent.WindowName)
				if err == nil && hasWindow {
					status = "running"
				} else {
					status = "stopped"
				}
			}
			detail["status"] = status

			// Get current branch from worktree
			branch := ""
			if agent.WorktreePath != "" {
				if b, err := worktree.GetCurrentBranch(d.ctx, agent.WorktreePath); err == nil {
					branch = b
				}
			}
			detail["branch"] = branch

			// Get message counts
			msgManager := messages.NewManager(d.paths.MessagesDir)
			allMsgs, _ := msgManager.List(repoName, agentName)
			pendingCount := 0
			for _, msg := range allMsgs {
				if msg.Status == messages.StatusPending || msg.Status == messages.StatusDelivered {
					pendingCount++
				}
			}
			detail["messages_total"] = len(allMsgs)
			detail["messages_pending"] = pendingCount

			// Token usage
			detail["input_tokens"] = agent.InputTokens
			detail["output_tokens"] = agent.OutputTokens
			detail["total_tokens"] = agent.TotalTokens
			if !agent.LastTokenUpdate.IsZero() {
				detail["last_token_update"] = agent.LastTokenUpdate.Format(time.RFC3339)
			}
			if agent.MaxTokens > 0 {
				detail["max_tokens"] = agent.MaxTokens
			}

			// Dormancy status
			detail["waiting_for_pr"] = agent.WaitingForPR
			detail["waiting_for_verification"] = agent.WaitingForVerification

			// Model-swap marker (set when the daemon auto-selected a
			// replacement model on restart because the configured one
			// failed validation). Surfaced in CLI tables so the operator
			// notices and can either re-onboard the original or pick a
			// permanent replacement with `oat agent set-model`.
			detail["model_swapped_on_restart"] = agent.ModelSwappedOnRestart
			if agent.ModelSwappedOnRestart {
				detail["model_swap_reason"] = agent.ModelSwapReason
				detail["model_swap_previous"] = agent.ModelSwapPrevious
			}

			// Log file path — so TUI doesn't have to compute it client-side
			isWorker := agent.Type == state.AgentTypeWorker || agent.Type == state.AgentTypeReview || agent.Type == state.AgentTypeVerification
			detail["log_path"] = d.paths.AgentLogFile(repoName, agentName, isWorker)
		}

		agentDetails = append(agentDetails, detail)
	}

	return socket.SuccessResponse(agentDetails)
}

// handleCompleteAgent marks an agent as ready for cleanup
func (d *Daemon) handleCompleteAgent(req socket.Request) socket.Response {
	repoName, errResp, ok := getRequiredStringArg(req.Args, "repo", "repository name is required")
	if !ok {
		return errResp
	}

	callerName, errResp, ok := getRequiredStringArg(req.Args, "agent", "agent name is required")
	if !ok {
		return errResp
	}

	// --worker flag: supervisor completing a worker on its behalf
	agentName := callerName
	if targetAgent := getOptionalStringArg(req.Args, "target_agent", ""); targetAgent != "" {
		agentName = targetAgent
		d.logger.Info("Agent %s/%s requesting completion of %s via --worker flag", repoName, callerName, targetAgent)
	}

	agent, exists := d.state.GetAgent(repoName, agentName)
	if !exists {
		return socket.ErrorResponse("agent '%s' not found in repository '%s' - check available agents with: oat worker list --repo %s", agentName, repoName, repoName)
	}

	// Part 5a: AgentTypeAssistant defense-in-depth. The assistant
	// prompt tells the model not to call `oat agent complete`, but if
	// it improvises and calls anyway we don't want an error response
	// that confuses the model into looping or escalating. Instead we
	// log a WARN (so operators can see drift from the prompt) and
	// return a no-op success so the agent moves on. Returning an
	// error here would also propagate to the CLI and surface as a
	// hard failure in the side panel, which the user can't easily
	// recover from. Handled before the permanentTypes map so the
	// no-op path takes precedence over the rejected-with-error path.
	if agent.Type == state.AgentTypeAssistant {
		d.logger.Warn("Assistant %s/%s called `oat agent complete` -- ignoring (assistants are persistent; no-op ACK)", repoName, agentName)
		return socket.SuccessResponse(map[string]any{
			"status":  "no_op",
			"message": "Assistants are persistent and do not complete. The daemon ignored this call. Continue the conversation; the user is still here.",
		})
	}

	// Guard: permanent agents (supervisor, workspace, merge-queue) cannot be completed.
	// This prevents accidents like a supervisor running "oat agent complete" without --worker.
	permanentTypes := map[state.AgentType]bool{
		state.AgentTypeSupervisor:        true,
		state.AgentTypeWorkspace:         true,
		state.AgentTypeMergeQueue:        true,
		state.AgentTypePRShepherd:        true,
		state.AgentTypeBrowser:           true,
		state.AgentTypeGenericPersistent: true,
	}
	if permanentTypes[agent.Type] {
		d.logger.Warn("Rejected oat agent complete for %s agent %s/%s", agent.Type, repoName, agentName)
		return socket.ErrorResponse(
			"Agent '%s' is a %s and cannot be completed. Only workers and review agents can use 'oat agent complete'. "+
				"If you need to complete a worker, use: oat agent complete --worker <worker-name>",
			agentName, agent.Type)
	}

	if agent.ReadyForCleanup {
		// Daemon already marked this agent complete (e.g. auto-complete on
		// PR merge, or verdict delivery). Return success so the agent stops
		// gracefully instead of panicking and running more commands.
		if summary := getOptionalStringArg(req.Args, "summary", ""); summary != "" && agent.Summary == "" {
			agent.Summary = summary
			_ = d.state.UpdateAgent(repoName, agentName, agent)
		}
		d.logger.Debug("Agent '%s' called complete but already marked ReadyForCleanup -- returning success", agentName)
		return socket.SuccessResponse(map[string]any{
			"status":  "already_complete",
			"message": "Agent already completed",
		})
	}

	// Reject if worker is self-completing while waiting for a verification verdict.
	// The worker should wait for the daemon to deliver the verdict before acting.
	if agent.WaitingForVerification && callerName == agentName {
		return socket.ErrorResponse(
			"Cannot complete: you are waiting for a verification verdict. STOP. Do NOT run oat agent complete. " +
				"The daemon will wake you with the [APPROVED] or [REJECTED] result. Wait for the verdict before taking any action.")
	}

	// Reject completion if the worker has an open PR with merge conflicts.
	if agent.Type == state.AgentTypeWorker {
		repoPath := d.paths.RepoDir(repoName)
		prNum := agent.PRNumber

		// If PRNumber not tracked (worker never went dormant), check GitHub directly
		if prNum == 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			cmd := exec.CommandContext(ctx, "gh", "pr", "list",
				"--head", "work/"+agentName,
				"--state", "open",
				"--json", "number,mergeable",
				"--limit", "1")
			cmd.Dir = repoPath
			if out, err := cmd.Output(); err == nil {
				var prs []struct {
					Number    int    `json:"number"`
					Mergeable string `json:"mergeable"`
				}
				if json.Unmarshal(out, &prs) == nil && len(prs) > 0 {
					if prs[0].Mergeable == "CONFLICTING" {
						cancel()
						return socket.ErrorResponse(
							"Cannot complete: you have an open PR (#%d) with merge conflicts. "+
								"Resolve the conflicts and push, or if you cannot, create a blocker issue:\n"+
								"  oat issue create --blocker --title \"Blocker: Merge conflict on PR #%d needs help\" --body \"...\"\n"+
								"Do NOT run oat agent complete until the conflicts are resolved.",
							prs[0].Number, prs[0].Number)
					}
				}
			}
			cancel()
		} else {
			prNumStr := strconv.Itoa(prNum)
			if result, ok := d.queryPRStatus(repoPath, prNumStr, repoName, agentName); ok {
				if result.State == "OPEN" && result.Mergeable == "CONFLICTING" {
					return socket.ErrorResponse(
						"Cannot complete: you have an open PR (#%d) with merge conflicts. "+
							"Resolve the conflicts and push, or if you cannot, create a blocker issue:\n"+
							"  oat issue create --blocker --title \"Blocker: Merge conflict on PR #%d needs help\" --body \"...\"\n"+
							"Do NOT run oat agent complete until the conflicts are resolved.",
						prNum, prNum)
				}
			}
		}
	}

	// Clean up any associated verify agent before completing the worker.
	if agent.Type == state.AgentTypeWorker {
		d.cleanupVerifyAgent(repoName, agentName)
	}

	// Mark as ready for cleanup and clear dormancy flags so the PR monitor
	// and message router stop interacting with this agent.
	agent.ReadyForCleanup = true
	agent.ReadyForCleanupAt = time.Now()
	agent.ClearDormancy()

	// Optional: capture summary and failure reason for task history
	if summary := getOptionalStringArg(req.Args, "summary", ""); summary != "" {
		agent.Summary = summary
	}
	if failureReason := getOptionalStringArg(req.Args, "failure_reason", ""); failureReason != "" {
		agent.FailureReason = failureReason
	}

	if err := d.state.UpdateAgent(repoName, agentName, agent); err != nil {
		return socket.ErrorResponse("%s", err.Error())
	}

	d.logger.Info("Agent %s/%s marked as ready for cleanup", repoName, agentName)

	// Append to routing-history.jsonl for offline replay analysis. Outcome is
	// "completed" here; the PR merge-status backfill happens separately.
	d.logOutcome(repoName, agentName, agent, "completed", "")

	// Close associated issue when a worker self-completes without a PR.
	// This covers the "impossible task" scenario where the worker determines
	// the work cannot be done and calls oat agent complete directly.
	// Skip for supervisor force-completes (callerName != agentName) which have
	// their own replacement-worker process.
	if agent.PRNumber == 0 && agent.IssueNumber != "" && callerName == agentName {
		d.closeAssociatedIssue(repoName, agentName, agent, "worker completed without creating a PR")
	}

	// Purge any pending (undelivered) messages to prevent stale delivery
	msgMgr := d.getMessageManager()
	if purged, err := msgMgr.PurgePending(repoName, agentName); err != nil {
		d.logger.Warn("Failed to purge pending messages for %s/%s: %v", repoName, agentName, err)
	} else if purged > 0 {
		d.logger.Info("Purged %d pending messages for completed agent %s/%s", purged, repoName, agentName)
	}

	// Notify supervisor and merge-queue that worker or review agent completed.
	// Deduplicate: only send each notification once per agent.
	notifyKey := fmt.Sprintf("%s/%s", repoName, agentName)
	d.completionNotifiedMu.Lock()
	alreadyNotified := d.completionNotified[notifyKey]
	if !alreadyNotified {
		d.completionNotified[notifyKey] = true
	}
	d.completionNotifiedMu.Unlock()

	if !alreadyNotified && (agent.Type == state.AgentTypeWorker || agent.Type == state.AgentTypeReview || agent.Type == state.AgentTypeVerification) {
		msgMgr := d.getMessageManager()

		switch agent.Type {
		case state.AgentTypeWorker:
			// Attach a pre-computed state snapshot so supervisor / merge-queue
			// don't run `oat worker list` / `gh pr list` themselves. See
			// daemon_messaging.go for the chokepoint that makes this decision.
			supervisorMessage := d.withRepoSnapshot(repoName, state.AgentTypeSupervisor,
				fmt.Sprintf("[daemon] Worker '%s' has completed and may have a PR to merge. Merge-queue has also been notified.", agentName))
			if _, err := msgMgr.Send(repoName, "daemon", "supervisor", supervisorMessage); err != nil {
				d.logger.Error("Failed to send completion message to supervisor: %v", err)
			} else {
				d.logger.Info("Sent completion notification to supervisor for worker %s", agentName)
			}

			mergeQueueMessage := d.withRepoSnapshot(repoName, state.AgentTypeMergeQueue,
				fmt.Sprintf("[daemon] Worker '%s' has completed and may have a PR. Check for new PRs to process.", agentName))
			if _, err := msgMgr.Send(repoName, "daemon", "merge-queue", mergeQueueMessage); err != nil {
				d.logger.Error("Failed to send completion message to merge-queue: %v", err)
			} else {
				d.logger.Info("Sent completion notification to merge-queue for worker %s", agentName)
			}
		case state.AgentTypeReview:
			mergeQueueMessage := d.withRepoSnapshot(repoName, state.AgentTypeMergeQueue,
				fmt.Sprintf("[daemon] Review agent '%s' has completed its review. Check the review summary and decide on next steps.", agentName))
			if _, err := msgMgr.Send(repoName, "daemon", "merge-queue", mergeQueueMessage); err != nil {
				d.logger.Error("Failed to send completion message to merge-queue: %v", err)
			} else {
				d.logger.Info("Sent completion notification to merge-queue for review agent %s", agentName)
			}
		case state.AgentTypeVerification:
			// Notify supervisor that verification completed
			supervisorMessage := d.withRepoSnapshot(repoName, state.AgentTypeSupervisor,
				fmt.Sprintf("[daemon] Verification agent '%s' has completed. The linked worker should have received the verdict.", agentName))
			if _, err := msgMgr.Send(repoName, "daemon", "supervisor", supervisorMessage); err != nil {
				d.logger.Error("Failed to send completion message to supervisor: %v", err)
			} else {
				d.logger.Info("Sent completion notification to supervisor for verification agent %s", agentName)
			}
		}

		// Trigger immediate message delivery
		d.triggerRouteMessages()
	}

	// Kill agent process after grace period to prevent post-completion token waste.
	// Send a shutdown message first so the agent knows to stop immediately.
	windowName := agent.WindowName // capture for goroutine
	sessionName := ""
	if repo, exists := d.state.GetRepo(repoName); exists && repo != nil {
		sessionName = repo.SessionName
		_ = d.backend.SendMessage(d.ctx, sessionName, windowName, "[daemon] Agent complete. Process will be terminated. Do not run any more commands.")
	}
	d.safeGo("kill-completed-"+agentName, func() {
		time.Sleep(workerPostCompletionDelay)
		if sessionName == "" {
			return
		}
		if err := d.backend.StopAgent(d.ctx, sessionName, windowName); err != nil {
			d.logger.Warn("Failed to kill completed agent %s: %v", agentName, err)
		} else {
			d.logger.Info("Killed completed agent %s after grace period", agentName)
		}
	})

	// Trigger immediate cleanup check
	d.safeGo("health-check-after-complete", d.checkAgentHealth)

	return socket.SuccessResponse(nil)
}

// handleResetNudge resets a worker's nudge count (one-time use per worker).
// This allows the supervisor to buy more time for workers that are actively
// working but not producing git activity (e.g., running long test suites).
func (d *Daemon) handleResetNudge(req socket.Request) socket.Response {
	repoName, errResp, ok := getRequiredStringArg(req.Args, "repo", "repository name is required")
	if !ok {
		return errResp
	}

	agentName, errResp, ok := getRequiredStringArg(req.Args, "agent", "agent name is required")
	if !ok {
		return errResp
	}

	agent, exists := d.state.GetAgent(repoName, agentName)
	if !exists {
		return socket.ErrorResponse("agent '%s' not found in repository '%s'", agentName, repoName)
	}

	if agent.Type != "worker" {
		return socket.ErrorResponse("reset-nudge only works on workers, '%s' is a %s", agentName, agent.Type)
	}

	if agent.ReadyForCleanup {
		return socket.ErrorResponse("worker '%s' has already completed and cannot be reset", agentName)
	}

	if agent.NudgeResetUsed {
		return socket.ErrorResponse("Nudge reset already used for worker '%s'. Each worker can only be reset once.", agentName)
	}

	agent.NudgeCount = 0
	agent.NudgeResetUsed = true
	if err := d.state.UpdateAgent(repoName, agentName, agent); err != nil {
		d.logger.Error("Failed to persist nudge reset for worker %s/%s: %v", repoName, agentName, err)
	}
	d.logger.Info("Supervisor reset nudge count for worker %s/%s", repoName, agentName)

	return socket.SuccessResponse(fmt.Sprintf("Nudge count reset for worker '%s'. The daemon will start a fresh escalation cycle.", agentName))
}

// handleStartVerification atomically sets a worker's verification state to "pending".
// Called by the CLI when a worker runs `oat worker request-review`.
func (d *Daemon) handleStartVerification(req socket.Request) socket.Response {
	repoName, errResp, ok := getRequiredStringArg(req.Args, "repo", "repository name is required")
	if !ok {
		return errResp
	}

	workerName, errResp, ok := getRequiredStringArg(req.Args, "worker", "worker name is required")
	if !ok {
		return errResp
	}

	verifierName, errResp, ok := getRequiredStringArg(req.Args, "verifier_name", "verifier name is required")
	if !ok {
		return errResp
	}

	commitSHA, errResp, ok := getRequiredStringArg(req.Args, "commit_sha", "commit SHA is required")
	if !ok {
		return errResp
	}

	// Use CLI-provided base SHA (snapped from the worker's worktree, which is
	// fresher than the daemon's repo clone). Fall back to daemon-side snapshot
	// for backward compatibility with older CLIs that don't send base_sha.
	baseSHA := getOptionalStringArg(req.Args, "base_sha", "")
	if baseSHA == "" {
		repoPath := d.paths.RepoDir(repoName)
		baseSHA = d.snapshotRemoteBaseSHA(repoPath)
	}

	// Atomically set all verification fields on the worker
	err := d.state.ModifyAgent(repoName, workerName, func(a *state.Agent) {
		a.VerificationAgent = verifierName
		a.VerificationStatus = "pending"
		a.VerifiedCommitSHA = ""
		a.VerificationReason = ""
		a.VerificationAt = time.Time{}
		a.LastBranchSHA = commitSHA
		a.BaseSHA = baseSHA
	})
	if err != nil {
		return socket.ErrorResponse("failed to set verification state: %v", err)
	}

	if baseSHA != "" {
		d.logger.Info("Verification started for worker %s/%s by %s (SHA: %s, base: %s)", repoName, workerName, verifierName, commitSHA[:min(len(commitSHA), 8)], baseSHA[:min(len(baseSHA), 8)])
	} else {
		d.logger.Info("Verification started for worker %s/%s by %s (SHA: %s, base: <unpinned>)", repoName, workerName, verifierName, commitSHA[:min(len(commitSHA), 8)])
	}

	// Trigger immediate wake so the verification agent gets nudged right away
	// instead of waiting for the next polling tick (up to 60s).
	d.triggerWake()

	return socket.SuccessResponse(fmt.Sprintf("verification pending for worker '%s'", workerName))
}

// snapshotRemoteBaseSHA fetches the remote default branch and returns its
// commit SHA. The result is meant to be persisted on the worker's state so
// the verifier can diff against this pinned base instead of live
// origin/main. Returns "" on any failure (caller falls back to live ref).
//
// Steps:
//  1. Resolve the default branch ref (origin/HEAD → origin/<default>;
//     fallback probe origin/main, origin/master).
//  2. git fetch origin <branch> (best-effort; offline daemon still gets
//     the last-fetched SHA below).
//  3. git rev-parse <ref>.
//
// All sub-commands are scoped to d.ctx via exec.CommandContext (PR #85
// noctx lint).
func (d *Daemon) snapshotRemoteBaseSHA(repoPath string) string {
	if repoPath == "" {
		return ""
	}

	branchRef := d.resolveDefaultBranchRef(repoPath)
	if branchRef == "" {
		return ""
	}

	// branchRef is e.g. "origin/main"; the bare branch name (after the
	// "origin/" prefix) is what `git fetch origin <branch>` expects.
	branchName := strings.TrimPrefix(branchRef, "origin/")
	if branchName != "" {
		fetchCmd := exec.CommandContext(d.ctx, "git", "fetch", "origin", branchName)
		fetchCmd.Dir = repoPath
		if out, err := fetchCmd.CombinedOutput(); err != nil {
			d.logger.Warn("BaseSHA snapshot: git fetch origin %s failed in %s: %v (%s); using last-fetched SHA", branchName, repoPath, err, strings.TrimSpace(string(out)))
		}
	}

	revParseCmd := exec.CommandContext(d.ctx, "git", "rev-parse", branchRef)
	revParseCmd.Dir = repoPath
	out, err := revParseCmd.Output()
	if err != nil {
		d.logger.Warn("BaseSHA snapshot: git rev-parse %s failed in %s: %v; verifier will fall back to live origin/main", branchRef, repoPath, err)
		return ""
	}
	return strings.TrimSpace(string(out))
}

// resolveDefaultBranchRef returns the remote default branch as
// "origin/<name>". Mirrors verify_helpers.go getBaseBranchRef().
func (d *Daemon) resolveDefaultBranchRef(repoPath string) string {
	symCmd := exec.CommandContext(d.ctx, "git", "symbolic-ref", "refs/remotes/origin/HEAD")
	symCmd.Dir = repoPath
	if out, err := symCmd.Output(); err == nil {
		refPath := strings.TrimSpace(string(out))
		if parts := strings.SplitN(refPath, "refs/remotes/", 2); len(parts) == 2 {
			return parts[1]
		}
	}

	for _, branch := range []string{"origin/main", "origin/master"} {
		probe := exec.CommandContext(d.ctx, "git", "rev-parse", "--verify", branch)
		probe.Dir = repoPath
		if err := probe.Run(); err == nil {
			return branch
		}
	}

	return ""
}

// handleStartVerificationAgent starts a verification agent process via the daemon
// backend so the PTY lifecycle is owned by the long-running daemon (not the
// short-lived CLI process). Mirrors handleStartWorker but uses AgentTypeVerification.
func (d *Daemon) handleStartVerificationAgent(req socket.Request) socket.Response {
	repoName, errResp, ok := getRequiredStringArg(req.Args, "repo", "repository name is required")
	if !ok {
		return errResp
	}

	agentName, errResp, ok := getRequiredStringArg(req.Args, "agent", "agent name is required")
	if !ok {
		return errResp
	}

	worktreePath, errResp, ok := getRequiredStringArg(req.Args, "worktree_path", "path to the agent's git worktree is required")
	if !ok {
		return errResp
	}

	promptFile, errResp, ok := getRequiredStringArg(req.Args, "prompt_file", "prompt file path is required")
	if !ok {
		return errResp
	}

	task := getOptionalStringArg(req.Args, "task", "")
	model := getOptionalStringArg(req.Args, "model", "")
	initialMessage := getOptionalStringArg(req.Args, "initial_message", "")

	repo, exists := d.state.GetRepo(repoName)
	if !exists {
		return socket.ErrorResponse("repository '%s' not found", repoName)
	}

	// Auto-retire stale or completed verifier from a previous run instead of failing.
	// This handles the case where a worker re-requests review before the
	// health check cleans up the old verifier agent from state.
	if existing, exists := d.state.GetAgent(repoName, agentName); exists {
		if existing.ReadyForCleanup {
			// Verifier already completed -- force-retire it for re-request
			// regardless of whether its process is still alive (it may be
			// lingering during the post-completion cleanup delay).
			d.logger.Info("Auto-retiring completed verifier %s/%s for re-request", repoName, agentName)
			_ = d.backend.StopAgent(d.ctx, repo.SessionName, existing.WindowName)
			if err := d.state.RemoveAgent(repoName, agentName); err != nil {
				return socket.ErrorResponse("failed to clean up completed verifier '%s': %v", agentName, err)
			}
		} else {
			alive := false
			if hasWindow, err := d.backend.IsAgentAlive(d.ctx, repo.SessionName, existing.WindowName); err == nil && hasWindow {
				alive = true
			}
			if !alive && existing.PID > 0 {
				alive = isProcessAlive(existing.PID)
			}
			if alive {
				return socket.ErrorResponse("agent '%s' is still running in repository '%s'", agentName, repoName)
			}
			d.logger.Info("Auto-retiring stale verifier %s/%s (pid=%d, not alive) for re-request", repoName, agentName, existing.PID)
			_ = d.backend.StopAgent(d.ctx, repo.SessionName, existing.WindowName)
			if err := d.state.RemoveAgent(repoName, agentName); err != nil {
				return socket.ErrorResponse("failed to clean up stale verifier '%s': %v", agentName, err)
			}
		}
	}

	hasSession, err := d.backend.HasSession(d.ctx, repo.SessionName)
	if err != nil {
		return socket.ErrorResponse("failed to check session '%s': %v", repo.SessionName, err)
	}
	if !hasSession {
		if err := d.backend.CreateSession(d.ctx, repo.SessionName); err != nil {
			return socket.ErrorResponse("failed to create session '%s': %v", repo.SessionName, err)
		}
	}

	if err := d.startAgentWithConfig(repoName, repo, agentStartConfig{
		agentName:      agentName,
		agentType:      state.AgentTypeVerification,
		promptFile:     promptFile,
		workDir:        worktreePath,
		model:          model,
		initialMessage: initialMessage,
	}); err != nil {
		return socket.ErrorResponse("failed to start verification agent: %v", err)
	}

	agent, exists := d.state.GetAgent(repoName, agentName)
	if !exists {
		return socket.ErrorResponse("verification agent '%s' was started but not found in state", agentName)
	}
	agent.Task = task
	if err := d.state.UpdateAgent(repoName, agentName, agent); err != nil {
		return socket.ErrorResponse("failed to update verification agent metadata: %v", err)
	}

	d.logger.Info("Started verification agent %s in repo %s (PID: %d)", agentName, repoName, agent.PID)

	return socket.SuccessResponse(map[string]interface{}{
		"agent": agentName,
		"repo":  repoName,
		"pid":   agent.PID,
	})
}

// handleVerificationVerdict processes a verdict from a verification agent.
// Validates: caller matches linked verifier, SHA matches, worker is pending.
func (d *Daemon) handleVerificationVerdict(req socket.Request) socket.Response {
	repoName, errResp, ok := getRequiredStringArg(req.Args, "repo", "repository name is required")
	if !ok {
		return errResp
	}

	workerName, errResp, ok := getRequiredStringArg(req.Args, "worker", "worker name is required")
	if !ok {
		return errResp
	}

	verdict, errResp, ok := getRequiredStringArg(req.Args, "verdict", "verdict is required (approved or rejected)")
	if !ok {
		return errResp
	}

	sha, errResp, ok := getRequiredStringArg(req.Args, "sha", "commit SHA is required")
	if !ok {
		return errResp
	}

	reason := getOptionalStringArg(req.Args, "reason", "")
	verifier := getOptionalStringArg(req.Args, "verifier", "")

	// Validate verdict value
	if verdict != "approved" && verdict != "rejected" {
		return socket.ErrorResponse("invalid verdict %q: must be 'approved' or 'rejected'", verdict)
	}

	// Get current worker state for validation
	worker, exists := d.state.GetAgent(repoName, workerName)
	if !exists {
		d.logger.Info("Verdict for %s/%s (%s) but worker no longer exists; verdict noted but not needed", repoName, workerName, verdict)
		return socket.SuccessResponse("worker already completed; verdict noted but not needed")
	}

	// Tolerate verdict when worker is already completing
	if worker.ReadyForCleanup {
		d.logger.Info("Verdict for %s/%s (%s) but worker is ready for cleanup; verdict noted", repoName, workerName, verdict)
		return socket.SuccessResponse("worker already completing; verdict noted but not needed")
	}

	// Validate worker is pending
	if worker.VerificationStatus != "pending" {
		return socket.ErrorResponse("worker '%s' is not pending verification (status: %s)", workerName, worker.VerificationStatus)
	}

	// Validate caller is the linked verifier
	if verifier != "" && verifier != worker.VerificationAgent {
		return socket.ErrorResponse("verifier mismatch: verdict from '%s' but worker expects '%s'", verifier, worker.VerificationAgent)
	}

	// Validate SHA matches (relaxed when worker is no longer actively working)
	if sha != worker.LastBranchSHA {
		d.logger.Warn("Verdict SHA mismatch for %s/%s: verdict=%s, branch=%s; accepting late verdict", repoName, workerName, sha[:min(len(sha), 8)], worker.LastBranchSHA[:min(len(worker.LastBranchSHA), 8)])
		return socket.SuccessResponse(fmt.Sprintf("SHA mismatch (verdict=%s, branch=%s); worker has moved on — verdict noted", sha[:min(len(sha), 8)], worker.LastBranchSHA[:min(len(worker.LastBranchSHA), 8)]))
	}

	// Apply verdict atomically (increment RejectionCount on rejection)
	var newRejectionCount int
	err := d.state.ModifyAgent(repoName, workerName, func(a *state.Agent) {
		a.VerificationStatus = verdict
		a.VerifiedCommitSHA = sha
		a.VerificationReason = reason
		a.VerificationAt = time.Now()
		if verdict == "rejected" {
			a.RejectionCount++
			newRejectionCount = a.RejectionCount
		}
	})
	if err != nil {
		return socket.ErrorResponse("failed to set verdict: %v", err)
	}

	// Mark the verifier ready for cleanup so the health check picks it up
	// promptly instead of waiting for the 5-minute startup grace to expire.
	if worker.VerificationAgent != "" {
		if _, vExists := d.state.GetAgent(repoName, worker.VerificationAgent); vExists {
			_ = d.state.ModifyAgent(repoName, worker.VerificationAgent, func(a *state.Agent) {
				a.ReadyForCleanup = true
				a.ReadyForCleanupAt = time.Now()
			})
			d.logger.Info("Marked verifier %s ready for cleanup after delivering %s verdict", worker.VerificationAgent, verdict)
		}
	}

	// On rejection: check if the worker has hit the rejection cap
	if verdict == "rejected" && maxRejections > 0 && newRejectionCount >= maxRejections {
		d.logger.Warn("Worker %s/%s reached rejection cap (%d/%d), escalating", repoName, workerName, newRejectionCount, maxRejections)
		d.rejectionCapReached(repoName, workerName, worker, newRejectionCount, reason)
		return socket.SuccessResponse(fmt.Sprintf("verdict 'rejected' recorded for worker '%s' (rejection cap reached — worker auto-completed)", workerName))
	}

	// Send message to worker and wake them if dormant
	msgMgr := d.getMessageManager()
	var verdictMsg string
	if verdict == "approved" {
		verdictMsg = fmt.Sprintf("[daemon] [APPROVED] Verification agent approved commit %s. This approval applies ONLY to that specific commit. If you have made changes since (e.g., rebasing, conflict resolution), run `oat worker verify` on your current commit before running `oat pr create`.", sha[:min(len(sha), 8)])
		if reason != "" {
			verdictMsg += fmt.Sprintf(" Reason: %s", reason)
		}
	} else {
		verdictMsg = fmt.Sprintf("[daemon] [REJECTED] Verification agent rejected your work (SHA: %s, attempt %d/%d).", sha[:min(len(sha), 8)], newRejectionCount, maxRejections)
		if reason != "" {
			verdictMsg += fmt.Sprintf(" Reason: %s", reason)
		}
		verdictMsg += " Fix the issues and run `oat worker request-review` again."
	}

	if worker.IsDormant() {
		d.wakeWorker(repoName, workerName, worker, verdictMsg)
	} else if _, err := msgMgr.Send(repoName, "daemon", workerName, verdictMsg); err != nil {
		d.logger.Warn("Failed to deliver verdict message to %s/%s: %v", repoName, workerName, err)
	}

	d.logger.Info("Verification verdict for %s/%s: %s (SHA: %s)", repoName, workerName, verdict, sha[:min(len(sha), 8)])
	return socket.SuccessResponse(fmt.Sprintf("verdict '%s' recorded for worker '%s'", verdict, workerName))
}

// handleStartRepoAgents creates the backend session and starts all agents
// registered in state for a given repo. This is called by the CLI after
// registering agents via add_agent, so the daemon (which owns the backend)
// is the process that creates and owns the agent child processes.
func (d *Daemon) handleStartRepoAgents(req socket.Request) socket.Response {
	repoName, errResp, ok := getRequiredStringArg(req.Args, "repo", "repository name is required")
	if !ok {
		return errResp
	}

	repo, exists := d.state.GetRepo(repoName)
	if !exists {
		return socket.ErrorResponse("repository '%s' not found", repoName)
	}

	// Extract CLI-forwarded env vars (tokens, API keys) if provided.
	// These override .env file values for this startup batch.
	var cliEnvVars []string
	if cliEnvRaw, ok := req.Args["cli_env"]; ok {
		if envMap, ok := cliEnvRaw.(map[string]interface{}); ok {
			for k, v := range envMap {
				if s, ok := v.(string); ok {
					cliEnvVars = append(cliEnvVars, fmt.Sprintf("%s=%s", k, s))
				}
			}
		}
	}

	// Create backend session
	if err := d.backend.CreateSession(d.ctx, repo.SessionName); err != nil {
		return socket.ErrorResponse("failed to create session for '%s': %v", repoName, err)
	}

	d.ensureBinSymlinks()

	type agentResult struct {
		Name string `json:"name"`
		PID  int    `json:"pid"`
	}
	results := make([]agentResult, 0)

	for agentName, agent := range repo.Agents {
		// Idempotency: skip agents that are already running. This makes
		// start_repo_agents safe to re-invoke after an incremental
		// add_agent (e.g. `oat agent add browser-agent` adds a single
		// agent to an already-running repo, then calls start_repo_agents
		// to spawn just it -- without this guard we'd double-spawn every
		// existing supervisor / merge-queue / worker).
		if agent.PID > 0 && isProcessAlive(agent.PID) {
			results = append(results, agentResult{Name: agentName, PID: agent.PID})
			continue
		}
		pid, err := d.startRegisteredAgent(repoName, repo, agentName, agent, cliEnvVars)
		if err != nil {
			d.logger.Error("Failed to start agent %s/%s: %v", repoName, agentName, err)
			continue
		}
		results = append(results, agentResult{Name: agentName, PID: pid})
	}

	// Send agent definitions to supervisor so it knows about available agent types
	repoPath := d.paths.RepoDir(repoName)
	mqConfig := repo.MergeQueueConfig
	if mqConfig.TrackMode == "" {
		mqConfig = state.DefaultMergeQueueConfig()
	}
	if err := d.sendAgentDefinitionsToSupervisor(repoName, repoPath, mqConfig); err != nil {
		d.logger.Warn("Failed to send agent definitions to supervisor for %s: %v", repoName, err)
	}

	return socket.SuccessResponse(results)
}

// startRegisteredAgent starts an agent that is already registered in state.
// It writes prompts, starts the process via the backend, and updates the
// agent's PID and SessionID in state. Unlike startAgentWithConfig, this does
// not call AddAgent (the agent already exists in state).
func (d *Daemon) startRegisteredAgent(repoName string, repo *state.Repository, agentName string, agent state.Agent, extraEnv []string) (int, error) {
	repoPath := d.paths.RepoDir(repoName)

	// Write prompt file
	promptFile, err := d.writePromptFile(repoName, agent.Type, agentName)
	if err != nil {
		return 0, fmt.Errorf("failed to write prompt file: %w", err)
	}

	// Generate new session ID
	sessionID, err := agent_pkg.GenerateSessionID()
	if err != nil {
		return 0, fmt.Errorf("failed to generate session ID: %w", err)
	}

	// Copy hooks config
	if err := hooks.CopyConfig(repoPath, agent.WorktreePath); err != nil {
		d.logger.Warn("Failed to copy hooks config for %s/%s: %v", repoName, agentName, err)
	}

	var pid int

	if os.Getenv("OAT_TEST_MODE") != "1" {
		binaryPath, err := d.getAgentBinaryPath()
		if err != nil {
			return 0, fmt.Errorf("failed to resolve agent binary: %w", err)
		}

		envPrefix := buildAgentEnvPrefix(d.paths, repoName)

		args := []string{"--auto-approve"}

		// Re-validate the model against the current profile store and repo
		// allowlist. Without this, startRegisteredAgent restored agents with
		// models that may since have been un-onboarded or disallowed — the
		// "silent bypass" identified in the routing live-test audit.
		// Validation errors are non-fatal here: the agent still starts with
		// its historical model (operator visibility wins over refusal for
		// restore paths), but the WARN gives the operator a trail.
		resolvedModel := agent.Model
		if resolvedModel == "" {
			resolvedModel = repo.Model
		}
		if resolvedModel != "" {
			if vErr := d.validateModelForAgentType(resolvedModel, agent.Type, repo.AllowedWorkerModels); vErr != nil {
				d.logger.Warn("Model routing: %s/%s restoring with model %q despite validation error: %v — consider re-onboarding or updating allowlist", repoName, agentName, resolvedModel, vErr)
			}
			args = append(args, "-M", resolvedModel)
			// Persist resolved model so TUI/CLI can display it
			if agent.Model == "" {
				agent.Model = resolvedModel
			}
		}
		args = append(args, "--model-params", d.modelParamsJSON(resolvedModel))
		// Browser-agent tool catalog filter. See denyToolArgs() for rationale.
		args = append(args, denyToolArgs(agent.Type)...)

		isWorker := agent.Type == state.AgentTypeWorker || agent.Type == state.AgentTypeReview || agent.Type == state.AgentTypeVerification
		logFile := d.paths.AgentLogFile(repoName, agentName, isWorker)
		logDir := filepath.Dir(logFile)
		_ = os.MkdirAll(logDir, 0755)

		var promptContent string
		if promptFile != "" {
			if data, readErr := os.ReadFile(promptFile); readErr == nil {
				promptContent = string(data)
			}
		}

		envVars := []string{fmt.Sprintf("OAT_AGENT_NAME=%s", agentName)}
		if beName := backendModeName(d.backend); beName != "" {
			envVars = append(envVars, fmt.Sprintf("OAT_BACKEND=%s", beName))
		}
		envVars = append(envVars, fmt.Sprintf("OAT_TOOL_LOG=%s", logFile))
		// Inject CLI-forwarded env vars (tokens, API keys) so agents inherit
		// the caller's environment even when the daemon lacks those vars.
		envVars = append(envVars, extraEnv...)

		// Opt-in sidecar. When OAT_USE_SIDECAR=1 is set on the daemon,
		// compute a per-agent socket path; the backend binds to it before
		// spawning Python and passes OAT_SIDECAR_SOCKET=<path> to the
		// agent env so sidecar_emitter.py connects. When unset (default),
		// SidecarPath stays empty and the agent runs unchanged.
		sidecarPath := sidecarSocketPath(repoName, agentName)

		// MCP config: only browser-agent currently declares an MCP
		// server (the oat-browser-agent stdio bridge). Resolution
		// failure is logged but non-fatal -- the agent starts with
		// just its built-in tools (http_request, fetch_url) so the
		// operator can still attach and see what went wrong. The
		// CLI `oat agent add browser-agent` runs the same probe at
		// add-time so users get the actionable error earlier.
		var mcpConfig string
		if usesBrowserBridge(agent.Type) {
			cfg, mcpErr := d.buildBrowserAgentMCPConfig(repoName, repo.SessionName, agentName)
			if mcpErr != nil {
				d.logger.Warn("Bridge-using agent %s/%s (%s) starting without MCP tools: %v", repoName, agentName, agent.Type, mcpErr)
			} else {
				mcpConfig = cfg
			}
		}

		// Start the assistant-turn tailer BEFORE the agent process so
		// the broadcaster is registered before the bridge subprocess
		// connects and calls stream_assistant_turns. See the matching
		// comment in restartAgent for the race-window post-mortem.
		// Polling inside tailer.run() handles the "log file does not
		// exist yet" case until the agent's first write.
		if usesBrowserBridge(agent.Type) {
			d.startAssistantTurnTailer(repo.SessionName, agentName, logFile)
		}

		handle, err := d.backend.StartAgent(d.ctx, backend_pkg.AgentConfig{
			SessionName:   repo.SessionName,
			AgentName:     agentName,
			WorkDir:       agent.WorktreePath,
			BinaryPath:    binaryPath,
			Args:          args,
			Env:           envVars,
			EnvPrefix:     envPrefix,
			InitialPrompt: promptContent,
			MCPConfig:     mcpConfig,
			LogFile:       logFile,
			SidecarPath:   sidecarPath,
		})
		if err != nil {
			return 0, fmt.Errorf("failed to start agent: %w", err)
		}
		if handle != nil {
			pid = handle.PID
		}

		d.startOutputWatcher(repoName, agentName, logFile)
	}

	// Update agent in state with new PID and SessionID
	agent.PID = pid
	agent.SessionID = sessionID
	if err := d.state.UpdateAgent(repoName, agentName, agent); err != nil {
		return 0, fmt.Errorf("failed to update agent state: %w", err)
	}

	d.logger.Info("Started registered agent %s/%s (PID=%d)", repoName, agentName, pid)

	// Part 5g.5 Slice A (2026-05-22): operator-visible coexistence
	// log for browser-agents. When a second (or third, ...)
	// browser-agent across any repo comes up while another is
	// already alive, emit a one-line INFO event so the operator
	// can correlate "I started a workflow-helper in repo B and
	// my agent in repo A kept working" with the design intent.
	//
	// The non-blocking invariant itself is implicit in the
	// surrounding code: there's no `if otherBrowserAgentAlive {
	// block / wait }` gate before the spawn -- each browser-agent
	// goes through the same per-repo path independently. Each
	// gets its own `OAT_BROWSER_AGENT_ID` (Part 5g.1) and the
	// extension's 5g.2 broker selection picks among them. The
	// load-bearing assertion that the spawn env is consistent
	// (so chat_capable: true is deterministic) lives in
	// daemon_test.go:TestBuildBrowserAgentMCPConfig_IdentityVarsAreFaithfullyPlumbed.
	//
	// No-op for non-browser agents (log gates on AgentTypeBrowser).
	// No throttle: coexistence transitions are rare in practice and
	// the log line is cheap; deferring to a future debouncer would
	// risk silently dropping the very signal operators need to see.
	if usesBrowserBridge(agent.Type) {
		others := countLiveBrowserAgentsExcept(d.state, repoName, agentName)
		if others > 0 {
			d.logger.Info(
				"browser-agent coexistence: %s/%s (id=%s:%s, PID=%d) started; %d other live browser-agent(s) across all repos",
				repoName, agentName, repoName, agentName, pid, others,
			)
		}
	}

	return pid, nil
}

// countLiveBrowserAgentsExcept counts bridge-using agents
// (AgentTypeBrowser + AgentTypeAssistant — anything for which
// usesBrowserBridge returns true) across every repo that have a
// non-zero PID, EXCLUDING the (repoName, agentName) just spawned
// (the caller has already written its PID to state when this is
// called).
//
// Part 5a extended this counter from "browser only" to "any
// bridge-using type" so the 5g.5 coexistence log fires for the new
// browser↔assistant and assistant↔assistant scenarios too, not just
// the original browser↔browser one. The name is retained (rather
// than renamed to e.g. countLiveBridgeAgentsExcept) to avoid churning
// the test surface that already pins the behavior; the docstring is
// the source of truth for what it actually counts.
//
// "Live" here is the state-file truth (PID != 0). Health-check
// reconciliation that clears stale PIDs runs every ~2 minutes; in
// the rare window where a dead bridge's PID hasn't been cleared
// yet, the count is an over-estimate, which is the right error
// direction for an operator-visible log (worst case: a spurious
// coexistence line when one of the "other" agents is already
// gone -- preferable to silently undercounting an actually-live
// coexistence event).
//
// Cheap: a state snapshot iteration is O(repos * agents) and only
// runs on bridge-agent spawn. Not in any hot path.
func countLiveBrowserAgentsExcept(s *state.State, excludeRepo, excludeAgent string) int {
	count := 0
	for repoName, repo := range s.GetAllRepos() {
		for agentName, agent := range repo.Agents {
			if repoName == excludeRepo && agentName == excludeAgent {
				continue
			}
			if !usesBrowserBridge(agent.Type) {
				continue
			}
			if agent.PID == 0 {
				continue
			}
			count++
		}
	}
	return count
}

// handleAgentWaiting marks a worker as dormant (waiting for PR resolution).
// The daemon will monitor the PR and notify the worker when action is needed.
func (d *Daemon) handleAgentWaiting(req socket.Request) socket.Response {
	repoName, errResp, ok := getRequiredStringArg(req.Args, "repo", "repository name is required")
	if !ok {
		return errResp
	}

	agentName, errResp, ok := getRequiredStringArg(req.Args, "agent", "agent name is required")
	if !ok {
		return errResp
	}

	agent, exists := d.state.GetAgent(repoName, agentName)
	if !exists {
		return socket.ErrorResponse("agent '%s' not found in repository '%s'", agentName, repoName)
	}

	// Agent already completed — return success so it stops gracefully
	if agent.ReadyForCleanup {
		d.logger.Debug("Agent '%s' called waiting but already marked ReadyForCleanup -- returning success", agentName)
		return socket.SuccessResponse(map[string]any{
			"status":  "already_complete",
			"message": "Agent already completed",
		})
	}

	// Always accept pr_number updates, even when already dormant.
	// oat pr create auto-calls agent_waiting with pr_number after the PR is
	// created; if the worker is already dormant (from request-review), we must
	// still persist the PR number so later flows (complete, timeout) see it.
	if prNum := getOptionalIntArg(req.Args, "pr_number", 0); prNum > 0 && agent.PRNumber == 0 {
		agent.PRNumber = prNum
		if err := d.state.UpdateAgent(repoName, agentName, agent); err != nil {
			return socket.ErrorResponse("failed to update agent PR number: %v", err)
		}
		d.logger.Info("Updated PR number for %s/%s to #%d (while already dormant)", repoName, agentName, prNum)
	}

	if agent.IsDormant() {
		if agent.WaitingForVerification && agent.PRNumber > 0 {
			// PR was created while dormant for verification — transition to PR monitoring
			agent.WaitingForVerification = false
			agent.WaitingForVerificationSince = time.Time{}
			agent.WaitingForPR = true
			agent.WaitingForPRSince = time.Now()
			if err := d.state.UpdateAgent(repoName, agentName, agent); err != nil {
				return socket.ErrorResponse("failed to transition dormancy state: %v", err)
			}
			return socket.SuccessResponse(map[string]any{
				"status":  "already_dormant",
				"message": fmt.Sprintf("Transitioned to PR monitoring (PR #%d).", agent.PRNumber),
			})
		}
		if agent.WaitingForVerification {
			return socket.SuccessResponse(map[string]any{
				"status":   "dormant_verification",
				"message":  "Already dormant -- waiting for verification verdict. STOP. Do NOT poll, sleep, or run any commands. The daemon will wake you when the verdict arrives.",
				"verifier": agent.VerificationAgent,
			})
		}
		return socket.SuccessResponse(map[string]any{
			"status":  "already_dormant",
			"message": fmt.Sprintf("You are already dormant (waiting for PR #%d). The daemon will wake you when action is needed.", agent.PRNumber),
		})
	}

	// Accept optional pr_number from caller (e.g., oat pr create passes it)
	repoPath := d.paths.RepoDir(repoName)

	// Discover PR number if not already known, with one retry
	if agent.PRNumber == 0 {
		branchName := "work/" + agentName
		pr := d.getWorkerPR(repoPath, branchName)
		if pr != nil {
			agent.PRNumber = pr.Number
		} else {
			time.Sleep(3 * time.Second)
			pr = d.getWorkerPR(repoPath, branchName)
			if pr != nil {
				agent.PRNumber = pr.Number
			}
		}
	}

	// Worker is waiting for verification verdict before creating PR — allow dormancy.
	if agent.PRNumber == 0 && agent.VerificationStatus == "pending" {
		wasAlreadyWaiting := agent.IsDormant()
		agent.WaitingForVerification = true
		agent.WaitingForVerificationSince = time.Now()
		agent.NudgeCount = 0
		agent.NudgeResetUsed = false
		if err := d.state.UpdateAgent(repoName, agentName, agent); err != nil {
			return socket.ErrorResponse("failed to update agent state: %v", err)
		}
		d.logger.Info("Worker %s/%s going dormant while waiting for verification (no PR yet)", repoName, agentName)
		if !wasAlreadyWaiting {
			d.triggerRouteMessages()
		}
		return socket.SuccessResponse(map[string]any{
			"status":   "dormant_verification",
			"message":  "Waiting for verification verdict. STOP. Do NOT poll, sleep, or run any commands. The daemon will deliver the result and wake you.",
			"verifier": agent.VerificationAgent,
		})
	}

	// No PR to monitor -- dormancy would be a dead-end. Auto-complete instead.
	if agent.PRNumber == 0 {
		agent.ReadyForCleanup = true
		agent.ReadyForCleanupAt = time.Now()
		agent.ClearDormancy()
		if err := d.state.UpdateAgent(repoName, agentName, agent); err != nil {
			return socket.ErrorResponse("failed to update agent state: %v", err)
		}
		d.logger.Info("Auto-completed worker %s/%s (no PR found, dormancy would be a dead-end)", repoName, agentName)
		d.closeAssociatedIssue(repoName, agentName, agent, "worker completed without a PR")

		notifyKey := fmt.Sprintf("%s/%s", repoName, agentName)
		d.completionNotifiedMu.Lock()
		alreadyNotified := d.completionNotified[notifyKey]
		if !alreadyNotified {
			d.completionNotified[notifyKey] = true
		}
		d.completionNotifiedMu.Unlock()

		if !alreadyNotified {
			msgMgr := d.getMessageManager()
			supervisorMsg := d.withRepoSnapshot(repoName, state.AgentTypeSupervisor,
				fmt.Sprintf("[daemon] Worker '%s' auto-completed (no PR found). Merge-queue has also been notified.", agentName))
			if _, err := msgMgr.Send(repoName, "daemon", "supervisor", supervisorMsg); err != nil {
				d.logger.Error("Failed to send auto-complete notification to supervisor: %v", err)
			}
			mergeQueueMsg := d.withRepoSnapshot(repoName, state.AgentTypeMergeQueue,
				fmt.Sprintf("[daemon] Worker '%s' auto-completed (no PR found). Check for new PRs to process.", agentName))
			if _, err := msgMgr.Send(repoName, "daemon", "merge-queue", mergeQueueMsg); err != nil {
				d.logger.Error("Failed to send auto-complete notification to merge-queue: %v", err)
			}
			d.triggerRouteMessages()
		}
		d.safeGo("health-check-auto-complete", d.checkAgentHealth)

		return socket.SuccessResponse(map[string]any{
			"status":  "auto_completed",
			"message": "No PR found on branch work/" + agentName + ". Worker has been auto-completed instead of going dormant.",
		})
	}

	// Check PR status before allowing dormancy.
	if agent.PRNumber > 0 {
		prNum := strconv.Itoa(agent.PRNumber)
		if result, ok := d.queryPRStatus(repoPath, prNum, repoName, agentName); ok {
			// PR already merged or closed — auto-complete instead of going dormant
			if result.State == "MERGED" || result.State == "CLOSED" {
				agent.ReadyForCleanup = true
				agent.ReadyForCleanupAt = time.Now()
				agent.ClearDormancy()
				if err := d.state.UpdateAgent(repoName, agentName, agent); err != nil {
					return socket.ErrorResponse("failed to update agent state: %v", err)
				}
				d.logger.Info("Auto-completed worker %s/%s (PR #%d already %s)", repoName, agentName, agent.PRNumber, result.State)
				if result.State == "CLOSED" {
					d.closeAssociatedIssue(repoName, agentName, agent,
						fmt.Sprintf("PR #%d was closed without merging", agent.PRNumber))
				}

				notifyKey := fmt.Sprintf("%s/%s", repoName, agentName)
				d.completionNotifiedMu.Lock()
				alreadyNotified := d.completionNotified[notifyKey]
				if !alreadyNotified {
					d.completionNotified[notifyKey] = true
				}
				d.completionNotifiedMu.Unlock()

				if !alreadyNotified {
					msgMgr := d.getMessageManager()
					supervisorMsg := d.withRepoSnapshot(repoName, state.AgentTypeSupervisor,
						fmt.Sprintf("[daemon] Worker '%s' auto-completed: PR #%d already %s.", agentName, agent.PRNumber, strings.ToLower(result.State)))
					if _, err := msgMgr.Send(repoName, "daemon", "supervisor", supervisorMsg); err != nil {
						d.logger.Error("Failed to send auto-complete notification to supervisor: %v", err)
					}
					d.triggerRouteMessages()
				}
				d.safeGo("health-check-auto-complete", d.checkAgentHealth)

				stateLabel := "merged"
				if result.State == "CLOSED" {
					stateLabel = "closed"
				}
				return socket.SuccessResponse(map[string]any{
					"status":  "auto_completed",
					"message": fmt.Sprintf("PR #%d is already %s. Worker has been auto-completed.", agent.PRNumber, stateLabel),
				})
			}

			// Hard-reject dormancy if the PR still has a merge conflict.
			if result.Mergeable == "CONFLICTING" {
				return socket.ErrorResponse("PR #%d still has a merge conflict. Resolve the conflict and verify with `gh pr view %d --json mergeable` before going dormant.", agent.PRNumber, agent.PRNumber)
			}

			// Hard-reject dormancy if CI has already failed on this PR.
			// This breaks the wake-dormant loop where workers go dormant
			// immediately after being woken for CI failure without fixing it.
			if hasFailedChecks(result.StatusCheckRollup) {
				return socket.ErrorResponse(
					"PR #%d has failing CI. You MUST fix CI before going dormant. "+
						"Run `gh run list --branch work/%s --limit 1` then `gh run view <run-id> --log-failed` to see failures. "+
						"Fix and push, then run `oat agent waiting` again.",
					agent.PRNumber, agentName)
			}
		}
	}

	wasAlreadyWaiting := agent.IsDormant()
	agent.WaitingForPR = true
	agent.WaitingForPRSince = time.Now()
	agent.NudgeCount = 0
	agent.NudgeResetUsed = false

	if err := d.state.UpdateAgent(repoName, agentName, agent); err != nil {
		return socket.ErrorResponse("failed to update agent state: %v", err)
	}

	// Reset dedup maps so the monitor will re-evaluate this PR fresh
	if agent.PRNumber > 0 {
		key := fmt.Sprintf("%s/%d", repoName, agent.PRNumber)
		d.prGreenNotifiedMu.Lock()
		delete(d.prGreenNotified, key)
		d.prGreenNotifiedMu.Unlock()
		d.conflictNotifiedMu.Lock()
		delete(d.conflictNotified, key)
		d.conflictNotifiedMu.Unlock()
		d.ciFailureNotifiedMu.Lock()
		delete(d.ciFailureNotified, key)
		d.ciFailureNotifiedMu.Unlock()
	}

	d.logger.Info("Agent %s/%s is now dormant (waiting for PR #%d)", repoName, agentName, agent.PRNumber)

	// Trigger immediate PR monitoring so the daemon checks this worker's PR ASAP
	// instead of waiting for the next polling tick.
	d.triggerPRMonitor()

	// Notify merge-queue the first time a worker goes dormant for a given PR.
	// Skip if re-entering dormancy (worker was woken for CI fix / conflict and
	// is going back to sleep for the same PR — merge-queue already knows).
	if agent.PRNumber > 0 && !wasAlreadyWaiting {
		msgMgr := d.getMessageManager()
		mqMsg := d.withRepoSnapshot(repoName, state.AgentTypeMergeQueue,
			fmt.Sprintf("[daemon] Worker '%s' has submitted PR #%d. Please check and merge when CI is green.", agentName, agent.PRNumber))
		if _, err := msgMgr.Send(repoName, "daemon", "merge-queue", mqMsg); err != nil {
			d.logger.Error("Failed to notify merge-queue about dormant worker %s/%s: %v", repoName, agentName, err)
		}
		d.triggerRouteMessages()
	}

	return socket.SuccessResponse(nil)
}

// handleRestartBrowserAgent is the side-panel "Restart agent" path:
// the bridge knows its own (session, agent) identity from
// OAT_BROWSER_AGENT_SESSION / _NAME but does NOT know the repo name
// — repo lives only on the daemon side. This handler accepts the
// bridge's identity, resolves the repo via findRepoBySession, then
// delegates to the same code path as `restart_agent --force`.
//
// Security model:
//
//   - Restricted to `state.AgentTypeBrowser` agents. Mirrors the
//     defense-in-depth used by `agent_input` (line 2472): a
//     misconfigured or malicious bridge MUST NOT be able to kick
//     the supervisor or merge-queue.
//   - Always forces. A user clicking "Restart agent" in the side
//     panel has unambiguously asked for a fresh start; gating on
//     `force=false` here would just produce a confusing "already
//     running, use --force" error.
//
// Wire format on success: { restarted: true, agent: "<name>",
// repo: "<repo>" } so the bridge can echo the repo back to the
// side panel ("Restarted agent in repo X") if it wants.
func (d *Daemon) handleRestartBrowserAgent(req socket.Request) socket.Response {
	sessionName, errResp, ok := getRequiredStringArg(req.Args, "session", "session name is required")
	if !ok {
		return errResp
	}
	agentName, errResp, ok := getRequiredStringArg(req.Args, "agent", "agent name is required")
	if !ok {
		return errResp
	}

	repoName, repo, found := d.findRepoBySession(sessionName)
	if !found {
		return socket.ErrorResponse("no repository is bound to session %q", sessionName)
	}
	agent, exists := d.state.GetAgent(repoName, agentName)
	if !exists {
		return socket.ErrorResponse("agent '%s' not found in session %q", agentName, sessionName)
	}
	if !usesBrowserBridge(agent.Type) {
		return socket.ErrorResponse("restart_browser_agent is restricted to browser-bridge agent types (browser, assistant); %s/%s is %s", repoName, agentName, agent.Type)
	}
	if agent.ReadyForCleanup {
		return socket.ErrorResponse("agent '%s' is marked as complete and pending cleanup", agentName)
	}

	hasWindow, err := d.backend.IsAgentAlive(d.ctx, repo.SessionName, agentName)
	if err != nil {
		return socket.ErrorResponse("failed to check agent window: %v", err)
	}
	if !hasWindow {
		return socket.ErrorResponse("agent window '%s' does not exist - the agent may need to be recreated", agentName)
	}

	// Always force from this path: see comment above.
	if agent.PID > 0 && isProcessAlive(agent.PID) {
		d.logger.Info("Side-panel-driven restart for %s/%s (PID %d was still running)", repoName, agentName, agent.PID)
		if err := d.backend.StopAgent(d.ctx, repo.SessionName, agent.WindowName); err != nil {
			d.logger.Warn("Failed to stop prior agent %s/%s before side-panel restart: %v", repoName, agentName, err)
		}
	}

	if err := d.restartAgent(repoName, agentName, agent, repo); err != nil {
		return socket.ErrorResponse("failed to restart agent: %v", err)
	}
	if usesBrowserBridge(agent.Type) {
		d.clearBridgeUnreachable(fmt.Sprintf("%s/%s", repoName, agentName))
	}
	d.logger.Info("Successfully restarted %s/%s from side-panel request", repoName, agentName)
	return socket.SuccessResponse(map[string]interface{}{
		"restarted": true,
		"agent":     agentName,
		"repo":      repoName,
	})
}

// handleRestartAgent restarts an agent that has crashed or exited
func (d *Daemon) handleRestartAgent(req socket.Request) socket.Response {
	repoName, errResp, ok := getRequiredStringArg(req.Args, "repo", "repository name is required")
	if !ok {
		return errResp
	}

	agentName, errResp, ok := getRequiredStringArg(req.Args, "agent", "agent name is required")
	if !ok {
		return errResp
	}

	force := getOptionalBoolArg(req.Args, "force", false)

	agent, exists := d.state.GetAgent(repoName, agentName)
	if !exists {
		return socket.ErrorResponse("agent '%s' not found in repository '%s' - check available agents with: oat worker list --repo %s", agentName, repoName, repoName)
	}

	// Check if agent is marked for cleanup (completed)
	if agent.ReadyForCleanup {
		return socket.ErrorResponse("agent '%s' is marked as complete and pending cleanup - cannot restart a completed agent", agentName)
	}

	// Check if agent window exists
	repo, exists := d.state.GetRepo(repoName)
	if !exists {
		return socket.ErrorResponse("repository '%s' not found in state", repoName)
	}

	hasWindow, err := d.backend.IsAgentAlive(d.ctx, repo.SessionName, agentName)
	if err != nil {
		return socket.ErrorResponse("failed to check agent window: %v", err)
	}
	if !hasWindow {
		return socket.ErrorResponse("agent window '%s' does not exist - the agent may need to be recreated", agentName)
	}

	// Check if agent is already running
	if agent.PID > 0 && isProcessAlive(agent.PID) {
		if !force {
			return socket.ErrorResponse("agent '%s' is already running with PID %d - use --force to restart anyway", agentName, agent.PID)
		}
		d.logger.Info("Force restarting agent %s (PID %d was still running)", agentName, agent.PID)
		// Must kill the prior process tree before calling restartAgent
		// — otherwise StartAgent overwrites the backend's map entry,
		// orphaning the old oat-agent + its python child + its bridge
		// child. The adopted-restart path (line ~967) has done this for
		// years; the force-restart path was missing the same step and
		// produced the zombie bridge processes the side-panel chat
		// smoke test exposed.
		if err := d.backend.StopAgent(d.ctx, repo.SessionName, agent.WindowName); err != nil {
			d.logger.Warn("Failed to stop prior agent %s/%s before force-restart: %v", repoName, agentName, err)
		}
	}

	// Restart the agent
	if err := d.restartAgent(repoName, agentName, agent, repo); err != nil {
		return socket.ErrorResponse("failed to restart agent: %v", err)
	}

	// Browser-bridge agents (browser, assistant): a user-initiated
	// restart clears the bridge-unreachable back-off window so the
	// next failure starts the counter over rather than instantly
	// tripping the threshold. Per Part 2 of
	// mcp-and-opt-in-browser-agent_a10544be.plan.md (extended to
	// AgentTypeAssistant in Part 5a of side-panel-chat-and-status).
	if usesBrowserBridge(agent.Type) {
		d.clearBridgeUnreachable(fmt.Sprintf("%s/%s", repoName, agentName))
	}

	// Get updated PID from state
	updatedAgent, _ := d.state.GetAgent(repoName, agentName)
	return socket.SuccessResponse(map[string]interface{}{
		"agent":   agentName,
		"repo":    repoName,
		"pid":     updatedAgent.PID,
		"message": fmt.Sprintf("Agent '%s' restarted successfully", agentName),
	})
}

// handleTriggerCleanup manually triggers cleanup operations
func (d *Daemon) handleTriggerCleanup(req socket.Request) socket.Response {
	d.logger.Info("Manual cleanup triggered")

	// Run health check to find dead agents
	d.checkAgentHealth()

	return socket.SuccessResponse("Cleanup triggered")
}

// handleTriggerRefresh manually triggers worktree refresh for all agents
func (d *Daemon) handleTriggerRefresh(req socket.Request) socket.Response {
	d.logger.Info("Manual worktree refresh triggered")

	// Run refresh in background so we can return immediately
	d.safeGo("manual-worktree-refresh", d.refreshWorktrees)

	return socket.SuccessResponse("Worktree refresh triggered")
}

// handleTriggerSync triggers worktree sync with optional branch/repo targeting
func (d *Daemon) handleTriggerSync(req socket.Request) socket.Response {
	branch, _ := req.Args["branch"].(string)
	targetRepo, _ := req.Args["repo"].(string)

	if branch != "" {
		d.logger.Info("Manual sync triggered (branch: %s, repo: %s)", branch, targetRepo)
	} else {
		d.logger.Info("Manual sync triggered (repo: %s)", targetRepo)
	}

	d.safeGo("manual-worktree-sync", func() {
		d.refreshWorktreesWithOptions(targetRepo, branch)
	})

	return socket.SuccessResponse("Sync triggered")
}

// handleRepairState repairs state inconsistencies
func (d *Daemon) handleRepairState(req socket.Request) socket.Response {
	d.logger.Info("State repair triggered")

	agentsRemoved := 0
	issuesFixed := 0

	// Get a snapshot of repos to avoid concurrent map access
	repos := d.state.GetAllRepos()

	// Check all agents and verify resources exist
	for repoName, repo := range repos {
		// Check backend session
		hasSession, err := d.backend.HasSession(d.ctx, repo.SessionName)
		if err != nil {
			d.logger.Error("Failed to check session %s: %v", repo.SessionName, err)
			continue
		}

		if !hasSession {
			d.logger.Warn("Session %s not found, removing all agents for repo %s", repo.SessionName, repoName)
			// Remove all agents for this repo
			for agentName := range repo.Agents {
				if err := d.state.RemoveAgent(repoName, agentName); err == nil {
					agentsRemoved++
				}
			}
			issuesFixed++
			continue
		}

		// Check each agent's resources
		for agentName, agent := range repo.Agents {
			hasWindow, _ := d.backend.IsAgentAlive(d.ctx, repo.SessionName, agent.WindowName)
			if !hasWindow {
				d.logger.Info("Removing agent %s (window not found)", agentName)
				if err := d.state.RemoveAgent(repoName, agentName); err == nil {
					agentsRemoved++
					issuesFixed++
				}
				continue
			}

			// Check if worktree exists (for workers and review agents)
			if (agent.Type == state.AgentTypeWorker || agent.Type == state.AgentTypeReview || agent.Type == state.AgentTypeVerification) && agent.WorktreePath != "" {
				if _, err := os.Stat(agent.WorktreePath); os.IsNotExist(err) {
					d.logger.Warn("Worktree missing for agent %s, but window exists - keeping agent", agentName)
					// Don't remove - user might have manually deleted worktree
				}
			}
		}
	}

	// Clean up orphaned worktrees (no recently-removed paths in this context)
	d.cleanupOrphanedWorktrees(nil)

	// Clean up orphaned message directories
	msgMgr := d.getMessageManager()
	repoNames := d.state.ListRepos()
	for _, repoName := range repoNames {
		validAgents, _ := d.state.ListAgents(repoName)
		if count, err := msgMgr.CleanupOrphaned(repoName, validAgents); err == nil && count > 0 {
			issuesFixed += count
		}
	}

	d.logger.Info("State repair completed: %d agents removed, %d issues fixed", agentsRemoved, issuesFixed)

	return socket.SuccessResponse(map[string]interface{}{
		"agents_removed": agentsRemoved,
		"issues_fixed":   issuesFixed,
	})
}

// handleGetRepoConfig returns the configuration for a repository
func (d *Daemon) handleGetRepoConfig(req socket.Request) socket.Response {
	name, errResp, ok := getRequiredStringArg(req.Args, "name", "repository name is required")
	if !ok {
		return errResp
	}

	repo, exists := d.state.GetRepo(name)
	if !exists {
		return socket.ErrorResponse("repository %q not found", name)
	}

	// Get merge queue config (use default if not set for backward compatibility)
	mqConfig := repo.MergeQueueConfig
	if mqConfig.TrackMode == "" {
		mqConfig = state.DefaultMergeQueueConfig()
	}

	// Get PR shepherd config (use default if not set)
	psConfig := repo.PRShepherdConfig
	if psConfig.TrackMode == "" {
		psConfig = state.DefaultPRShepherdConfig()
	}

	// Get fork config
	forkConfig := repo.ForkConfig

	return socket.SuccessResponse(map[string]interface{}{
		"mq_enabled":                mqConfig.Enabled,
		"mq_track_mode":             string(mqConfig.TrackMode),
		"ps_enabled":                psConfig.Enabled,
		"ps_track_mode":             string(psConfig.TrackMode),
		"is_fork":                   forkConfig.IsFork,
		"upstream_url":              forkConfig.UpstreamURL,
		"upstream_owner":            forkConfig.UpstreamOwner,
		"upstream_repo":             forkConfig.UpstreamRepo,
		"force_fork_mode":           forkConfig.ForceForkMode,
		"model":                     repo.Model,
		"allowed_worker_models":     repo.AllowedWorkerModels,
		"workspace_stuck_detection": repo.WorkspaceStuckDetection,
	})
}

// handleUpdateRepoConfig updates the configuration for a repository
func (d *Daemon) handleUpdateRepoConfig(req socket.Request) socket.Response {
	name, errResp, ok := getRequiredStringArg(req.Args, "name", "repository name is required")
	if !ok {
		return errResp
	}

	// Get current merge queue config
	currentMQConfig, err := d.state.GetMergeQueueConfig(name)
	if err != nil {
		return socket.ErrorResponse("%s", err.Error())
	}

	// Update merge queue config with provided values
	mqUpdated := false
	if mqEnabled, hasMqEnabled := req.Args["mq_enabled"].(bool); hasMqEnabled {
		currentMQConfig.Enabled = mqEnabled
		mqUpdated = true
	}
	if mqTrackMode := getOptionalStringArg(req.Args, "mq_track_mode", ""); mqTrackMode != "" {
		mode, err := state.ParseTrackMode(mqTrackMode)
		if err != nil {
			return socket.ErrorResponse("%s", err.Error())
		}
		currentMQConfig.TrackMode = mode
		mqUpdated = true
	}

	if mqUpdated {
		if err := d.state.UpdateMergeQueueConfig(name, currentMQConfig); err != nil {
			return socket.ErrorResponse("%s", err.Error())
		}
		d.logger.Info("Updated merge queue config for repo %s: enabled=%v, track=%s", name, currentMQConfig.Enabled, currentMQConfig.TrackMode)
	}

	// Get current PR shepherd config
	currentPSConfig, err := d.state.GetPRShepherdConfig(name)
	if err != nil {
		return socket.ErrorResponse("%s", err.Error())
	}

	// Update PR shepherd config with provided values
	psUpdated := false
	if psEnabled, hasPsEnabled := req.Args["ps_enabled"].(bool); hasPsEnabled {
		currentPSConfig.Enabled = psEnabled
		psUpdated = true
	}
	if psTrackMode := getOptionalStringArg(req.Args, "ps_track_mode", ""); psTrackMode != "" {
		mode, err := state.ParseTrackMode(psTrackMode)
		if err != nil {
			return socket.ErrorResponse("%s", err.Error())
		}
		currentPSConfig.TrackMode = mode
		psUpdated = true
	}

	if psUpdated {
		if err := d.state.UpdatePRShepherdConfig(name, currentPSConfig); err != nil {
			return socket.ErrorResponse("%s", err.Error())
		}
		d.logger.Info("Updated PR shepherd config for repo %s: enabled=%v, track=%s", name, currentPSConfig.Enabled, currentPSConfig.TrackMode)
	}

	// Update workspace stuck detection if provided
	if wsd, hasWSD := req.Args["workspace_stuck_detection"].(bool); hasWSD {
		if err := d.state.ModifyRepo(name, func(repo *state.Repository) {
			repo.WorkspaceStuckDetection = wsd
		}); err != nil {
			return socket.ErrorResponse("failed to update workspace stuck detection: %s", err.Error())
		}
		d.logger.Info("Updated workspace stuck detection for repo %s: enabled=%v", name, wsd)
	}

	// Handle allowed worker models operations
	var workerModelWarnings []string
	workerModelsAction := getOptionalStringArg(req.Args, "worker_models_action", "")
	workerModelsValue := getOptionalStringArg(req.Args, "worker_models_value", "")

	if workerModelsAction != "" {
		var newModels []string
		if workerModelsValue != "" {
			for _, m := range strings.Split(workerModelsValue, ",") {
				m = strings.TrimSpace(m)
				if m != "" {
					newModels = append(newModels, m)
				}
			}
		}

		// Snapshot the pre-mutation allow-list and default repo model so we
		// can compute which models transitioned allowed → disallowed (drift)
		// after the mutation commits. GetRepo returns a pointer to live state
		// — copy the slice explicitly because ModifyRepo will mutate it.
		var previousAllowed []string
		var previousDefaultModel string
		if preRepo, exists := d.state.GetRepo(name); exists && preRepo != nil {
			previousAllowed = append(previousAllowed, preRepo.AllowedWorkerModels...)
			previousDefaultModel = preRepo.Model
		}

		if err := d.state.ModifyRepo(name, func(repo *state.Repository) {
			switch workerModelsAction {
			case "set":
				repo.AllowedWorkerModels = newModels
			case "add":
				existing := make(map[string]bool, len(repo.AllowedWorkerModels))
				for _, m := range repo.AllowedWorkerModels {
					existing[m] = true
				}
				for _, m := range newModels {
					if !existing[m] {
						repo.AllowedWorkerModels = append(repo.AllowedWorkerModels, m)
						existing[m] = true
					}
				}
			case "remove":
				removeSet := make(map[string]bool, len(newModels))
				for _, m := range newModels {
					removeSet[m] = true
				}
				filtered := repo.AllowedWorkerModels[:0]
				for _, m := range repo.AllowedWorkerModels {
					if !removeSet[m] {
						filtered = append(filtered, m)
					}
				}
				repo.AllowedWorkerModels = filtered
			case "clear":
				repo.AllowedWorkerModels = nil
			}
		}); err != nil {
			return socket.ErrorResponse("failed to update allowed worker models: %s", err.Error())
		}

		// Validate models against profiles and collect warnings
		if workerModelsAction == "set" || workerModelsAction == "add" {
			for _, m := range newModels {
				if d.modelProfiles != nil && d.modelProfiles.Get(m) == nil {
					workerModelWarnings = append(workerModelWarnings,
						fmt.Sprintf("model %q is not onboarded — it won't appear in the worker roster until onboarded (run: oat model onboard %s)", m, m))
				}
			}
		}

		repo, _ := d.state.GetRepo(name)
		if repo == nil {
			return socket.ErrorResponse("repo %s disappeared after worker model update", name)
		}
		d.logger.Info("Updated allowed worker models for repo %s: action=%s, models=%v", name, workerModelsAction, repo.AllowedWorkerModels)

		// Drift detection: when set/remove/clear causes a model to transition
		// from allowed → disallowed, any worker currently running on it will
		// be rerouted on the next restart. Warn per affected worker so
		// operators aren't surprised. add cannot narrow the allow-list.
		if workerModelsAction == "remove" || workerModelsAction == "set" || workerModelsAction == "clear" {
			removedSet := computeRemovedWorkerModels(previousAllowed, repo.AllowedWorkerModels)
			if len(removedSet) > 0 {
				for agentName, agent := range repo.Agents {
					isWorker := agent.Type == state.AgentTypeWorker || agent.Type == state.AgentTypeReview || agent.Type == state.AgentTypeVerification
					if !isWorker {
						continue
					}
					// Resolve inherited model manually because resolveAgentModel
					// needs the repo default, which may itself have changed. The
					// comparison uses the pre-mutation repo default so we only
					// warn about workers that were, in fact, running on a
					// now-disallowed model prior to this mutation.
					effective := agent.Model
					if effective == "" {
						effective = previousDefaultModel
					}
					if effective == "" {
						continue
					}
					if removedSet[effective] {
						d.logger.Warn("Worker %s/%s is running on disallowed model %s; will be rerouted on next restart",
							name, agentName, effective)
					}
				}
			}
		}
	}

	resp := map[string]interface{}{}
	if len(workerModelWarnings) > 0 {
		resp["warnings"] = workerModelWarnings
	}
	return socket.SuccessResponse(resp)
}

// handleSetCurrentRepo sets the current/default repository
func (d *Daemon) handleSetCurrentRepo(req socket.Request) socket.Response {
	name, errResp, ok := getRequiredStringArg(req.Args, "name", "repository name is required")
	if !ok {
		return errResp
	}

	if err := d.state.SetCurrentRepo(name); err != nil {
		return socket.ErrorResponse("%s", err.Error())
	}

	d.logger.Info("Set current repository to: %s", name)
	return socket.SuccessResponse(name)
}

// handleGetCurrentRepo returns the current/default repository
func (d *Daemon) handleGetCurrentRepo(req socket.Request) socket.Response {
	currentRepo := d.state.GetCurrentRepo()
	if currentRepo == "" {
		return socket.ErrorResponse("no current repository set")
	}
	return socket.SuccessResponse(currentRepo)
}

// handleClearCurrentRepo clears the current/default repository
func (d *Daemon) handleClearCurrentRepo(req socket.Request) socket.Response {
	if err := d.state.ClearCurrentRepo(); err != nil {
		return socket.ErrorResponse("%s", err.Error())
	}

	d.logger.Info("Cleared current repository")
	return socket.SuccessResponse(nil)
}

// cleanupDeadAgents removes dead agents from state and returns the worktree
// paths that were removed, so callers can protect them from orphan cleanup.
func (d *Daemon) cleanupDeadAgents(deadAgents map[string][]string) []string {
	var removedPaths []string
	for repoName, agentNames := range deadAgents {
		for _, agentName := range agentNames {
			d.logger.Info("Cleaning up dead agent %s/%s", repoName, agentName)

			agent, exists := d.state.GetAgent(repoName, agentName)
			if !exists {
				continue
			}

			// Get repo info for session cleanup
			repo, exists := d.state.GetRepo(repoName)
			if !exists {
				d.logger.Error("Failed to get repo %s for cleanup", repoName)
				continue
			}

			// Record task history for workers before cleanup
			if agent.Type == state.AgentTypeWorker {
				d.recordTaskHistory(repoName, agentName, agent)
			}

			// If this is a verification agent, reset the linked worker's pending status
			// and wake the worker if dormant (otherwise it gets stuck forever).
			if agent.Type == state.AgentTypeVerification {
				if strings.HasPrefix(agentName, "verify-") {
					linkedWorker := strings.TrimPrefix(agentName, "verify-")
					if worker, wExists := d.state.GetAgent(repoName, linkedWorker); wExists && worker.VerificationStatus == "pending" && worker.VerificationAgent == agentName {
						// Guard: if the verifier was already marked
						// ReadyForCleanup, it cleanly delivered a verdict
						// (handleSetVerdict sets ReadyForCleanup=true). The
						// worker's status being "pending" at this moment
						// means a concurrent request-review reset it, NOT
						// that the verifier crashed. Skip the bogus crash
						// wake-message but still clear the orphaned
						// verifier-name pointer so the worker's state stays
						// internally consistent.
						verdictDelivered := agent.ReadyForCleanup
						_ = d.state.ModifyAgent(repoName, linkedWorker, func(a *state.Agent) {
							a.VerificationStatus = ""
							a.VerificationAgent = ""
						})
						if verdictDelivered {
							d.logger.Info("Verifier %s already delivered verdict; clearing stale worker pointer without crash wake-message", agentName)
						} else {
							d.logger.Info("Reset pending verification status for worker %s (verifier %s cleaned up)", linkedWorker, agentName)

							// Re-read the worker after modifying to get updated state
							if updatedWorker, ok := d.state.GetAgent(repoName, linkedWorker); ok && updatedWorker.IsDormant() {
								wakeMsg := fmt.Sprintf("[daemon] Your verification agent '%s' crashed before delivering a verdict. "+
									"Self-verify and create your PR: run `oat worker verify` then `oat pr create`.", agentName)
								d.wakeWorker(repoName, linkedWorker, updatedWorker, wakeMsg)
								d.logger.Info("Woke dormant worker %s (verifier %s crashed)", linkedWorker, agentName)
							}
						}
					}
				}
			}

			// Kill agent window
			stopFailed := false
			if err := d.backend.StopAgent(d.ctx, repo.SessionName, agent.WindowName); err != nil {
				d.logger.Warn("Failed to kill agent window %s: %v", agent.WindowName, err)
				stopFailed = true
			} else {
				d.logger.Info("Killed agent window for agent %s: %s", agentName, agent.WindowName)
			}

			// Safety: if we failed to stop the window AND the process is still
			// alive, don't remove from state — the agent may still be running
			// and removing it would cause orphan cleanup to delete its worktree.
			if stopFailed && agent.PID > 0 && isProcessAlive(agent.PID) {
				d.logger.Warn("Agent %s/%s process still alive after StopAgent failure, keeping in state to protect worktree", repoName, agentName)
				continue
			}

			// Track worktree path BEFORE removing from state, so orphan
			// cleanup won't race and delete it.
			if agent.WorktreePath != "" {
				removedPaths = append(removedPaths, agent.WorktreePath)
			}

			// Remove from state
			if err := d.state.RemoveAgent(repoName, agentName); err != nil {
				d.logger.Error("Failed to remove agent %s/%s from state: %v", repoName, agentName, err)
			}

			// Clean up worktree if it exists and differs from the repo dir
			repoPath := d.paths.RepoDir(repoName)
			if agent.WorktreePath != "" && agent.WorktreePath != repoPath {
				wt := worktree.NewManagerWithContext(d.ctx, repoPath)
				if err := wt.Remove(agent.WorktreePath, true); err != nil {
					d.logger.Warn("Failed to remove worktree %s: %v", agent.WorktreePath, err)
				} else {
					d.logger.Info("Removed worktree for dead agent: %s", agent.WorktreePath)
				}
			}

			// Clean up message directory
			msgMgr := d.getMessageManager()
			validAgents, _ := d.state.ListAgents(repoName)
			if _, err := msgMgr.CleanupOrphaned(repoName, validAgents); err != nil {
				d.logger.Warn("Failed to cleanup orphaned messages for %s: %v", repoName, err)
			}
		}
	}
	return removedPaths
}

// recordTaskHistory saves a worker's task to the history before cleanup
func (d *Daemon) recordTaskHistory(repoName, agentName string, agent state.Agent) {
	// Get the branch name from the worktree if it exists
	branch := ""
	if agent.WorktreePath != "" {
		if b, err := worktree.GetCurrentBranch(d.ctx, agent.WorktreePath); err == nil {
			branch = b
		} else {
			// Fallback: construct expected branch name
			branch = "work/" + agentName
		}
	}

	// Determine initial status
	status := state.TaskStatusUnknown
	if agent.FailureReason != "" {
		status = state.TaskStatusFailed
	}

	entry := state.TaskHistoryEntry{
		Name:          agentName,
		Task:          agent.Task,
		Branch:        branch,
		PRNumber:      agent.PRNumber,
		Status:        status, // Will be updated when displaying if a PR exists
		Summary:       agent.Summary,
		FailureReason: agent.FailureReason,
		Model:         agent.Model,
		CreatedAt:     agent.CreatedAt,
		CompletedAt:   time.Now(),
		InputTokens:   agent.InputTokens,
		OutputTokens:  agent.OutputTokens,
		TotalTokens:   agent.TotalTokens,
	}

	if err := d.state.AddTaskHistory(repoName, entry); err != nil {
		d.logger.Warn("Failed to record task history for %s: %v", agentName, err)
	} else {
		d.logger.Info("Recorded task history for %s (branch: %s, summary: %q)", agentName, branch, agent.Summary)
	}
}

// handleTaskHistory returns the task history for a repository
func (d *Daemon) handleTaskHistory(req socket.Request) socket.Response {
	repoName, errResp, ok := getRequiredStringArg(req.Args, "repo", "repository name is required")
	if !ok {
		return errResp
	}

	// Get optional limit
	limit := 10 // default
	if l, ok := req.Args["limit"].(float64); ok {
		limit = int(l)
	}

	history, err := d.state.GetTaskHistory(repoName, limit)
	if err != nil {
		return socket.ErrorResponse("%s", err.Error())
	}

	// Convert to interface slice for JSON serialization
	result := make([]map[string]interface{}, len(history))
	for i, entry := range history {
		result[i] = map[string]interface{}{
			"name":           entry.Name,
			"task":           entry.Task,
			"branch":         entry.Branch,
			"pr_url":         entry.PRURL,
			"pr_number":      entry.PRNumber,
			"status":         string(entry.Status),
			"model":          entry.Model,
			"summary":        entry.Summary,
			"failure_reason": entry.FailureReason,
			"created_at":     entry.CreatedAt,
			"completed_at":   entry.CompletedAt,
		}
	}

	return socket.SuccessResponse(result)
}

// handleSpawnAgent spawns a new agent with an inline prompt (no hardcoded type).
// This is used by the supervisor to spawn agents based on markdown definitions.
// Args:
//   - repo: repository name
//   - name: agent name (used for backend window and worktree)
//   - class: "persistent" or "ephemeral"
//   - prompt: full prompt text to use as system prompt
//   - task: optional task description (for ephemeral/worker agents)
func (d *Daemon) handleSpawnAgent(req socket.Request) socket.Response {
	repoName, errResp, ok := getRequiredStringArg(req.Args, "repo", "repository name is required")
	if !ok {
		return errResp
	}

	agentName, errResp, ok := getRequiredStringArg(req.Args, "name", "agent name is required")
	if !ok {
		return errResp
	}

	agentClass, errResp, ok := getRequiredStringArg(req.Args, "class", "agent class is required (persistent or ephemeral)")
	if !ok {
		return errResp
	}

	promptText, errResp, ok := getRequiredStringArg(req.Args, "prompt", "prompt text is required")
	if !ok {
		return errResp
	}

	// Validate class
	if agentClass != "persistent" && agentClass != "ephemeral" {
		return socket.ErrorResponse("invalid agent class %q: must be 'persistent' or 'ephemeral'", agentClass)
	}

	// Get optional task
	task := getOptionalStringArg(req.Args, "task", "")

	// Get repository
	repo, exists := d.state.GetRepo(repoName)
	if !exists {
		return socket.ErrorResponse("repository %q not found", repoName)
	}

	// Check if agent already exists
	if _, exists := d.state.GetAgent(repoName, agentName); exists {
		return socket.ErrorResponse("agent %q already exists in repository %q", agentName, repoName)
	}

	// Determine agent type based on class
	var agentType state.AgentType
	if agentClass == "persistent" {
		// For persistent agents, use specific type if known or generic persistent
		switch agentName {
		case "merge-queue":
			agentType = state.AgentTypeMergeQueue
		case "pr-shepherd":
			agentType = state.AgentTypePRShepherd
		default:
			agentType = state.AgentTypeGenericPersistent
		}
	} else {
		// Ephemeral agents are workers, reviewers, or verification agents
		if strings.Contains(strings.ToLower(agentName), "verif") {
			agentType = state.AgentTypeVerification
		} else if strings.Contains(strings.ToLower(agentName), "review") {
			agentType = state.AgentTypeReview
		} else {
			agentType = state.AgentTypeWorker
		}
	}

	// Create worktree for the agent
	repoPath := d.paths.RepoDir(repoName)
	worktreePath := d.paths.AgentWorktree(repoName, agentName)

	wt := worktree.NewManagerWithContext(d.ctx, repoPath)

	if agentClass == "persistent" {
		// Persistent agents get their own worktree in detached HEAD mode
		// to avoid AGENTS.md prompt overwrites between agents
		if err := wt.CreateDetached(worktreePath, "HEAD"); err != nil {
			return socket.ErrorResponse("failed to create persistent agent worktree: %v", err)
		}
	} else {
		// Ephemeral agents get their own worktree with a new branch
		branchName := fmt.Sprintf("work/%s", agentName)
		if err := wt.CreateNewBranch(worktreePath, branchName, "HEAD"); err != nil {
			return socket.ErrorResponse("failed to create worktree: %v", err)
		}
	}

	// Window creation is handled by backend.StartAgent() in startAgentWithConfig

	// Write prompt file. For generic-persistent agents, use the supervisor's
	// prompt text directly since there's no embedded template. For other types,
	// use writePromptFile which loads from agent definitions or embedded prompts.
	var promptPath string
	var promptErr error
	if agentType == state.AgentTypeGenericPersistent {
		promptPath, promptErr = d.writePromptFileWithPrefix(repoName, agentType, agentName, promptText)
	} else {
		promptPath, promptErr = d.writePromptFile(repoName, agentType, agentName)
	}
	if promptErr != nil {
		return socket.ErrorResponse("failed to write prompt file: %v", promptErr)
	}

	// Copy hooks config
	if err := hooks.CopyConfig(repoPath, worktreePath); err != nil {
		d.logger.Warn("Failed to copy hooks config: %v", err)
	}

	// Start the agent in a backend window
	cfg := agentStartConfig{
		agentName:  agentName,
		agentType:  agentType,
		promptFile: promptPath,
		workDir:    worktreePath,
	}

	if err := d.startAgentWithConfig(repoName, repo, cfg); err != nil {
		if stopErr := d.backend.StopAgent(d.ctx, repo.SessionName, agentName); stopErr != nil {
			d.logger.Warn("cleanup: StopAgent after failed start for %s/%s: %v", repoName, agentName, stopErr)
		}
		if rmErr := wt.Remove(worktreePath, true); rmErr != nil {
			d.logger.Warn("cleanup: worktree remove after failed start for %s/%s: %v", repoName, agentName, rmErr)
		}
		return socket.ErrorResponse("failed to start agent: %v", err)
	}

	// Update task if provided
	if task != "" {
		agent, _ := d.state.GetAgent(repoName, agentName)
		agent.Task = task
		if err := d.state.UpdateAgent(repoName, agentName, agent); err != nil {
			d.logger.Error("Failed to persist task for spawned agent %s/%s: %v", repoName, agentName, err)
		}
	}

	d.logger.Info("Spawned agent %s/%s (class=%s, type=%s)", repoName, agentName, agentClass, agentType)

	// Trigger immediate message routing so any queued messages reach the new agent
	// without waiting for the next 60s routing cycle.
	d.triggerRouteMessages()

	return socket.SuccessResponse(map[string]interface{}{
		"name":          agentName,
		"class":         agentClass,
		"type":          string(agentType),
		"worktree_path": worktreePath,
	})
}

// cleanupOrphanedWorktrees removes worktree directories without git tracking.
// Active agent worktrees are protected from removal even if git tracking is
// out of sync (e.g. due to concurrent git operations by agent processes).
// recentlyRemovedPaths are worktrees from agents just cleaned up — they're
// protected to close the race window between state removal and git cleanup.
func (d *Daemon) cleanupOrphanedWorktrees(recentlyRemovedPaths []string) {
	repoNames := d.state.ListRepos()
	for _, repoName := range repoNames {
		repoPath := d.paths.RepoDir(repoName)
		wtRootDir := d.paths.WorktreeDir(repoName)

		if _, err := os.Stat(wtRootDir); os.IsNotExist(err) {
			continue
		}

		wt := worktree.NewManagerWithContext(d.ctx, repoPath)

		// Prune stale git worktree references BEFORE checking for orphans
		// so that git worktree list returns the most accurate state possible.
		if err := wt.Prune(); err != nil {
			d.logger.Warn("Failed to prune worktrees for %s: %v", repoName, err)
		}

		// Build protected set from active (non-completed) agents so that
		// concurrent git operations that corrupt worktree tracking cannot
		// cause active worktree directories to be deleted as orphans.
		protectedPaths := make(map[string]bool)
		agents, _ := d.state.ListAgents(repoName)
		for _, agentName := range agents {
			agent, exists := d.state.GetAgent(repoName, agentName)
			if exists && agent.WorktreePath != "" && !agent.ReadyForCleanup {
				protectedPaths[agent.WorktreePath] = true
			}
		}

		// Also protect worktrees from agents just removed in this health
		// check cycle — closes the race between state removal and git cleanup.
		for _, p := range recentlyRemovedPaths {
			protectedPaths[p] = true
		}

		result, err := worktree.CleanupOrphanedWithDetails(wtRootDir, wt, protectedPaths)
		if err != nil {
			d.logger.Error("Failed to cleanup orphaned worktrees for %s: %v", repoName, err)
			continue
		}

		if len(result.Removed) > 0 {
			d.logger.Info("Cleaned up %d orphaned worktree(s) for %s", len(result.Removed), repoName)
			for _, path := range result.Removed {
				d.logger.Debug("Removed orphaned worktree: %s", path)
			}
		}

		for path, reason := range result.Errors {
			if reason == "skipped: directory belongs to active agent (git tracking out of sync)" {
				d.logger.Warn("Protected active worktree from orphan cleanup: %s (git tracking out of sync)", path)
			}
		}
	}
}

// cleanupMergedBranches cleans up branches that have been merged upstream
func (d *Daemon) cleanupMergedBranches() {
	d.logger.Debug("Checking for merged branches to cleanup")

	repoNames := d.state.ListRepos()
	for _, repoName := range repoNames {
		if d.isRepoFetchDisabled(repoName) {
			continue
		}

		repoPath := d.paths.RepoDir(repoName)

		// Check if repo path exists
		if _, err := os.Stat(repoPath); os.IsNotExist(err) {
			continue
		}

		wt := worktree.NewManagerWithContext(d.ctx, repoPath)

		// Clean up merged branches with common oat prefixes.
		// Delete local branches first, then selectively delete remote branches
		// only if they have no open PRs (prevents auto-closing PRs on GitHub).
		for _, prefix := range []string{"work/"} {
			deleted, err := wt.CleanupMergedBranches(prefix, false)
			if err != nil {
				d.logger.Debug("Failed to cleanup merged branches with prefix %s for %s: %v", prefix, repoName, err)
				continue
			}

			if len(deleted) == 0 {
				continue
			}

			d.logger.Info("Cleaned up %d merged local branch(es) for %s", len(deleted), repoName)

			// Fetch all open PR head branches in a single API call
			openPRBranches := d.getOpenPRBranches(repoPath)

			for _, branch := range deleted {
				if openPRBranches[branch] {
					d.logger.Warn("Skipping remote deletion of %s: open PR exists", branch)
					continue
				}
				if err := wt.DeleteRemoteBranch("origin", branch); err != nil {
					d.logger.Debug("Failed to delete remote branch %s: %v", branch, err)
				} else {
					d.logger.Info("Deleted merged remote branch: %s", branch)
				}
			}
		}
	}
}

// getOpenPRBranches returns a set of branch names that have open PRs.
// Uses a single batched API call regardless of how many branches exist.
func (d *Daemon) getOpenPRBranches(repoPath string) map[string]bool {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "pr", "list", "--state", "open", "--json", "headRefName", "--limit", "200")
	cmd.Dir = repoPath
	output, err := cmd.Output()
	if err != nil {
		d.logger.Debug("Failed to fetch open PR branches: %v", err)
		return nil
	}
	var prs []struct {
		HeadRefName string `json:"headRefName"`
	}
	if err := json.Unmarshal(output, &prs); err != nil {
		d.logger.Debug("Failed to parse open PR branches: %v", err)
		return nil
	}
	branches := make(map[string]bool, len(prs))
	for _, pr := range prs {
		branches[pr.HeadRefName] = true
	}
	return branches
}

// restoreTrackedRepos restores agents for tracked repos that are missing their backend sessions
// or have dead agent processes
func (d *Daemon) restoreTrackedRepos() {
	d.logger.Info("Checking tracked repos for restoration")

	repos := d.state.GetAllRepos()
	for repoName, repo := range repos {
		// Check if backend session exists
		hasSession, err := d.backend.HasSession(d.ctx, repo.SessionName)
		if err != nil {
			d.logger.Error("Failed to check session %s: %v", repo.SessionName, err)
			continue
		}

		if hasSession {
			d.logger.Debug("Session %s exists for repo %s", repo.SessionName, repoName)
			// Session exists but agents might have dead processes - check and restart them
			d.restoreDeadAgents(repoName, repo)
			// If state has no workspace agent but a workspace window exists (e.g. after daemon restart with lost state), register it so "oat workspace connect" works
			d.discoverMissingWorkspaces(repoName, repo)
			continue
		}

		// Session doesn't exist - restore it
		d.logger.Info("Restoring agents for repo %s (session %s was missing)", repoName, repo.SessionName)
		if err := d.restoreRepoAgents(repoName, repo); err != nil {
			d.logger.Error("Failed to restore agents for repo %s: %v", repoName, err)
		}
	}
}

// restoreDeadAgents restarts agents that have dead agent processes or were
// re-adopted without a PTY (after daemon restart with session persistence).
// This is called on daemon startup when the session exists but agent processes
// may have died or lost their PTY connection.
func (d *Daemon) restoreDeadAgents(repoName string, repo *state.Repository) {
	d.logger.Debug("Checking for dead agents in repo %s", repoName)

	for agentName, agent := range repo.Agents {
		// Skip agents without a PID (shouldn't happen, but be safe)
		if agent.PID <= 0 {
			d.logger.Debug("Agent %s has no PID, skipping", agentName)
			continue
		}

		// Check if agent was re-adopted (alive but no PTY). Persistent agents
		// must be stop+restarted immediately to regain control; don't wait for
		// the 2-minute health check cycle.
		if db, ok := d.backend.(*backend_pkg.DirectBackend); ok {
			if adopted, _ := db.IsAdopted(d.ctx, repo.SessionName, agent.WindowName); adopted {
				if agent.Type.IsPersistent() {
					d.logger.Info("Agent %s was re-adopted without PTY at startup, stopping then restarting", agentName)
					if err := d.backend.StopAgent(d.ctx, repo.SessionName, agent.WindowName); err != nil {
						d.logger.Warn("Failed to stop adopted agent %s: %v", agentName, err)
					}
					if err := d.restartAgent(repoName, agentName, agent, repo); err != nil {
						d.logger.Error("Failed to restart adopted agent %s: %v", agentName, err)
					} else {
						d.logger.Info("Successfully restarted adopted agent %s with --resume", agentName)
					}
				} else {
					d.logger.Debug("Adopted transient agent %s left running at startup", agentName)
				}
				continue
			}
		}

		// Check if the agent window still exists
		hasWindow, err := d.backend.IsAgentAlive(d.ctx, repo.SessionName, agent.WindowName)
		if err != nil {
			d.logger.Error("Failed to check window for agent %s: %v", agentName, err)
			continue
		}

		if !hasWindow {
			d.logger.Debug("Agent %s window not found, will be handled by health check", agentName)
			continue
		}

		// Check if the process is still alive
		if isProcessAlive(agent.PID) {
			d.logger.Debug("Agent %s process (PID %d) is alive", agentName, agent.PID)
			continue
		}

		// Process is dead but window exists - restart persistent agents with --resume
		d.logger.Info("Agent %s process (PID %d) is dead, attempting restart", agentName, agent.PID)

		// For persistent agents, auto-restart with cooldown to prevent storms.
		if agent.Type.IsPersistent() {
			cooldownKey := fmt.Sprintf("%s/%s", repoName, agentName)
			d.restartCooldownMu.Lock()
			lastRestart := d.restartCooldown[cooldownKey]
			d.restartCooldownMu.Unlock()
			if time.Since(lastRestart) < 30*time.Second {
				d.logger.Debug("Agent %s restart skipped at startup (cooldown, last restart %s ago)", agentName, time.Since(lastRestart).Round(time.Second))
				continue
			}
			d.restartCooldownMu.Lock()
			d.restartCooldown[cooldownKey] = time.Now()
			d.restartCooldownMu.Unlock()
			if err := d.restartAgent(repoName, agentName, agent, repo); err != nil {
				d.logger.Error("Failed to restart agent %s: %v", agentName, err)
			} else {
				d.logger.Info("Successfully restarted agent %s with --resume", agentName)
			}
		} else {
			d.logger.Debug("Skipping transient agent %s (type %s) - will be cleaned up", agentName, agent.Type)
		}
	}
}

// discoverMissingWorkspaces ensures at least one workspace is in state and running so "oat workspace connect"
// shows the agent UI (not just a shell). Handles: (1) workspace in state but PID 0 or dead — start agent in that window;
// (2) no workspace in state but worktree exists — create window if needed, then start agent.
func (d *Daemon) discoverMissingWorkspaces(repoName string, repo *state.Repository) {
	// If we have a workspace agent that was never started (PID 0) or is dead, start it in its existing window.
	for agentName, agent := range repo.Agents {
		if agent.Type != state.AgentTypeWorkspace {
			continue
		}
		if agent.PID > 0 && isProcessAlive(agent.PID) {
			return // already running
		}
		wtPath := agent.WorktreePath
		if wtPath == "" {
			wtPath = d.paths.AgentWorktree(repoName, agentName)
		}
		d.logger.Info("Workspace %s has no running process (PID %d), starting agent in window %s", agentName, agent.PID, agentName)
		if err := d.state.RemoveAgent(repoName, agentName); err != nil {
			d.logger.Warn("Failed to remove stale workspace %s for restart: %v", agentName, err)
			continue
		}
		if err := d.startAgent(repoName, repo, agentName, state.AgentTypeWorkspace, wtPath); err != nil {
			d.logger.Warn("Failed to start workspace agent %s: %v", agentName, err)
			// Re-add workspace with PID 0 so "oat workspace connect" still works; user can run the agent manually in the pane
			sessionID, _ := agent_pkg.GenerateSessionID()
			if sessionID != "" {
				_ = d.state.AddAgent(repoName, agentName, state.Agent{
					Type:         state.AgentTypeWorkspace,
					WorktreePath: wtPath,
					WindowName:   agentName,
					SessionID:    sessionID,
					PID:          0,
					CreatedAt:    time.Now(),
				})
			}
			continue
		}
		d.logger.Info("Started workspace %s for repo %s", agentName, repoName)
		return
	}
	// No workspace in state. Check for worktrees and start agent via backend.
	for _, windowName := range []string{"default", "workspace"} {
		wtPath := d.paths.AgentWorktree(repoName, windowName)
		if _, err := os.Stat(wtPath); os.IsNotExist(err) {
			continue
		}
		// Check if agent is already alive via backend
		alive, err := d.backend.IsAgentAlive(d.ctx, repo.SessionName, windowName)
		if err != nil {
			d.logger.Debug("Failed to check if workspace %s is alive: %v", windowName, err)
		}
		if alive {
			d.logger.Info("Workspace %s already alive for repo %s", windowName, repoName)
			return
		}
		// Start the workspace agent so "oat workspace connect" shows the OAT agent UI.
		if err := d.startAgent(repoName, repo, windowName, state.AgentTypeWorkspace, wtPath); err != nil {
			d.logger.Warn("Failed to start workspace agent %s for repo %s: %v", windowName, repoName, err)
			continue
		}
		d.logger.Info("Started workspace %s for repo %s", windowName, repoName)
		return
	}
}

// restoreRepoAgents restores the backend session and agents for a tracked repo
func (d *Daemon) restoreRepoAgents(repoName string, repo *state.Repository) error {
	repoPath := d.paths.RepoDir(repoName)

	// Verify the repo still exists on disk
	if _, err := os.Stat(repoPath); os.IsNotExist(err) {
		return fmt.Errorf("repository path does not exist: %s", repoPath)
	}

	// Create isolated worktree for supervisor so it gets its own .oat/AGENTS.md
	supervisorWtPath := d.paths.AgentWorktree(repoName, "supervisor")
	if _, err := os.Stat(supervisorWtPath); os.IsNotExist(err) {
		d.logger.Info("Creating supervisor worktree for %s", repoName)
		wt := worktree.NewManagerWithContext(d.ctx, repoPath)
		if err := wt.Prune(); err != nil {
			d.logger.Warn("Failed to prune worktrees for %s: %v", repoName, err)
		}
		if err := wt.CreateDetached(supervisorWtPath, "HEAD"); err != nil {
			d.logger.Error("Failed to create supervisor worktree for %s: %v", repoName, err)
			supervisorWtPath = repoPath // fallback to repo dir
		}
	}

	// Create session for the repo
	d.logger.Info("Creating session %s for repo %s", repo.SessionName, repoName)
	if err := d.backend.CreateSession(d.ctx, repo.SessionName); err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}

	// Get merge queue config (use default if not set for backward compatibility)
	mqConfig := repo.MergeQueueConfig
	if mqConfig.TrackMode == "" {
		mqConfig = state.DefaultMergeQueueConfig()
	}

	// Start supervisor agent — do this BEFORE clearing stale agents so we
	// don't leave the repo with zero agents if supervisor startup fails.
	if err := d.startAgent(repoName, repo, "supervisor", state.AgentTypeSupervisor, supervisorWtPath); err != nil {
		d.logger.Error("Failed to start supervisor for %s: %v (keeping stale agents in state for retry)", repoName, err)
		return fmt.Errorf("failed to start supervisor: %w", err)
	}

	// Supervisor started successfully — now safe to clear stale agents
	// (the new supervisor was already added to state by startAgent).
	for agentName := range repo.Agents {
		if agentName == "supervisor" {
			continue // just started this one
		}
		d.logger.Debug("Removing stale agent %s/%s from state", repoName, agentName)
		if err := d.state.RemoveAgent(repoName, agentName); err != nil {
			d.logger.Warn("Failed to remove stale agent %s/%s: %v", repoName, agentName, err)
		}
	}

	// Send agent definitions to supervisor as informational context
	if err := d.sendAgentDefinitionsToSupervisor(repoName, repoPath, mqConfig); err != nil {
		d.logger.Warn("Failed to send agent definitions to supervisor: %v", err)
	}

	// Start merge-queue or pr-shepherd depending on fork mode
	isForkMode := repo.ForkConfig.IsFork || repo.ForkConfig.ForceForkMode
	psConfig := repo.PRShepherdConfig

	if mqConfig.Enabled && !isForkMode {
		mqName := "merge-queue"
		mqWtPath := d.paths.AgentWorktree(repoName, mqName)
		if _, err := os.Stat(mqWtPath); os.IsNotExist(err) {
			d.logger.Info("Creating merge-queue worktree for %s", repoName)
			wt := worktree.NewManager(repoPath)
			if err := wt.CreateDetached(mqWtPath, "HEAD"); err != nil {
				d.logger.Error("Failed to create merge-queue worktree for %s: %v", repoName, err)
			} else if err := wt.CheckoutBranch(mqWtPath, "main"); err != nil {
				d.logger.Warn("Failed to checkout main in merge-queue worktree for %s: %v", repoName, err)
			}
		}
		if _, err := os.Stat(mqWtPath); err == nil {
			if err := d.startAgent(repoName, repo, mqName, state.AgentTypeMergeQueue, mqWtPath); err != nil {
				d.logger.Error("Failed to start merge-queue for %s: %v", repoName, err)
			}
		}
	}

	if isForkMode && psConfig.Enabled {
		psName := "pr-shepherd"
		psWtPath := d.paths.AgentWorktree(repoName, psName)
		if _, err := os.Stat(psWtPath); os.IsNotExist(err) {
			d.logger.Info("Creating pr-shepherd worktree for %s", repoName)
			wt := worktree.NewManager(repoPath)
			if err := wt.CreateDetached(psWtPath, "HEAD"); err != nil {
				d.logger.Error("Failed to create pr-shepherd worktree for %s: %v", repoName, err)
			} else if err := wt.CheckoutBranch(psWtPath, "main"); err != nil {
				d.logger.Warn("Failed to checkout main in pr-shepherd worktree for %s: %v", repoName, err)
			}
		}
		if _, err := os.Stat(psWtPath); err == nil {
			if err := d.startAgent(repoName, repo, psName, state.AgentTypePRShepherd, psWtPath); err != nil {
				d.logger.Error("Failed to start pr-shepherd for %s: %v", repoName, err)
			}
		}
	}

	// Create and restore workspace. The workspace worktree may live at either
	// "default" (created by CLI init) or "workspace" (legacy). Check both.
	var workspacePath, workspaceName string
	for _, name := range []string{"default", "workspace"} {
		p := d.paths.AgentWorktree(repoName, name)
		if _, err := os.Stat(p); err == nil {
			workspacePath = p
			workspaceName = name
			break
		}
	}

	if workspacePath == "" {
		// Neither path exists; create one at "default" (matches CLI init convention)
		workspaceName = "default"
		workspacePath = d.paths.AgentWorktree(repoName, workspaceName)
		d.logger.Info("Creating workspace worktree for %s", repoName)
		wt := worktree.NewManagerWithContext(d.ctx, repoPath)

		if err := wt.Prune(); err != nil {
			d.logger.Warn("Failed to prune worktrees for %s: %v", repoName, err)
		}

		migrated, migrateErr := wt.MigrateLegacyWorkspaceBranch()
		if migrateErr != nil {
			d.logger.Warn("Failed to migrate legacy workspace branch for %s: %v", repoName, migrateErr)
		} else if migrated {
			d.logger.Info("Migrated legacy 'workspace' branch to 'workspace/default' for %s", repoName)
		}

		branchExists, err := wt.BranchExists("workspace/default")
		if err != nil {
			d.logger.Warn("Failed to check if workspace/default branch exists for %s: %v", repoName, err)
		}

		if branchExists {
			if err := wt.Create(workspacePath, "workspace/default"); err != nil {
				d.logger.Error("Failed to create workspace worktree with existing branch for %s: %v", repoName, err)
			}
		} else {
			if err := wt.CreateNewBranch(workspacePath, "workspace/default", "HEAD"); err != nil {
				d.logger.Error("Failed to create workspace worktree with new branch for %s: %v", repoName, err)
			}
		}
	}

	// Start the workspace agent if worktree exists
	if _, err := os.Stat(workspacePath); err == nil {
		if err := d.startAgent(repoName, repo, workspaceName, state.AgentTypeWorkspace, workspacePath); err != nil {
			d.logger.Error("Failed to start workspace for %s: %v", repoName, err)
		}
	}

	// Restore opt-in browser-agent. The worktree at ~/.oat/wts/<repo>/
	// browser-agent/ acts as the "user opted in" persistence marker --
	// `oat agent add browser-agent` creates it, `oat agent remove`
	// deletes it (when it lands). If the path exists, we must respawn
	// the agent so a daemon crash + restart doesn't silently drop an
	// opted-in extension. The MCP config is rewritten at spawn time by
	// startRegisteredAgent's AgentTypeBrowser branch, so a fresh bridge
	// resolution + audit-log dir is always written.
	browserName := "browser-agent"
	browserWtPath := d.paths.AgentWorktree(repoName, browserName)
	if _, err := os.Stat(browserWtPath); err == nil {
		if err := d.startAgent(repoName, repo, browserName, state.AgentTypeBrowser, browserWtPath); err != nil {
			d.logger.Error("Failed to restore browser-agent for %s: %v", repoName, err)
		} else {
			d.logger.Info("Restored opt-in browser-agent for %s", repoName)
		}
	}

	return nil
}

// supervisorDefinitionBodyForMessage returns markdown for the supervisor informational message.
// Repo-sourced and merged definitions keep full content; local-only templates are replaced with
// a short capability summary to reduce token cost.
func supervisorDefinitionBodyForMessage(def agents.Definition) string {
	switch def.Source {
	case agents.SourceRepo, agents.SourceMerged:
		return def.Content
	default:
		return agentCapabilitySummaryLine(def.Name)
	}
}

// agentCapabilitySummaryLine is a short description of a built-in agent role for the supervisor.
func agentCapabilitySummaryLine(name string) string {
	switch name {
	case "worker":
		return "Executes assigned GitHub issues: implements changes, opens and updates pull requests."
	case "merge-queue":
		return "Monitors oat-labeled pull requests, runs checks, and merges when ready."
	case "verification":
		return "Reviews worker output against requirements; approves or rejects with a verdict."
	case "reviewer", "review":
		return "Performs structured code review on pull requests."
	case "pr-shepherd":
		return "In fork mode: prepares and tracks pull requests against the upstream repository."
	default:
		return fmt.Sprintf("Configurable agent role `%s` (see repository `.oat/agents/` for full instructions).", name)
	}
}

// sendAgentDefinitionsToSupervisor reads agent definitions and sends them to the supervisor
// as informational context. The supervisor does not spawn persistent agents (that's handled
// by oat init and restoreRepoAgents); this message just gives it awareness of agent types.
func (d *Daemon) sendAgentDefinitionsToSupervisor(repoName, repoPath string, mqConfig state.MergeQueueConfig) error {
	// Get repo to check fork config
	repo, exists := d.state.GetRepo(repoName)
	var forkConfig state.ForkConfig
	var psConfig state.PRShepherdConfig
	if exists {
		forkConfig = repo.ForkConfig
		psConfig = repo.PRShepherdConfig
	}

	// Create agent reader
	localAgentsDir := d.paths.RepoAgentsDir(repoName)
	reader := agents.NewReader(localAgentsDir, repoPath)

	// Read all definitions
	definitions, err := reader.ReadAllDefinitions()
	if err != nil {
		return fmt.Errorf("failed to read agent definitions: %w", err)
	}

	if len(definitions) == 0 {
		d.logger.Info("No agent definitions found for repo %s", repoName)
		return nil
	}

	// Build message with all definitions for supervisor to interpret
	var sb strings.Builder
	sb.WriteString("Agent definitions available for this repository:\n\n")

	// Include fork mode information if applicable
	isForkMode := forkConfig.IsFork || forkConfig.ForceForkMode
	if isForkMode {
		sb.WriteString("## Fork Mode (ACTIVE)\n")
		fmt.Fprintf(&sb, "This repository is a fork of **%s/%s**.\n\n", forkConfig.UpstreamOwner, forkConfig.UpstreamRepo)
		sb.WriteString("**Key differences in fork mode:**\n")
		sb.WriteString("- Use `pr-shepherd` instead of `merge-queue`\n")
		sb.WriteString("- PRs target the upstream repository\n")
		sb.WriteString("- You cannot merge PRs - only prepare them for review\n\n")

		sb.WriteString("## PR Shepherd Configuration\n")
		if psConfig.Enabled {
			sb.WriteString("- Enabled: yes\n")
			fmt.Fprintf(&sb, "- Track Mode: %s\n\n", psConfig.TrackMode)
		} else {
			sb.WriteString("- Enabled: no (do NOT spawn pr-shepherd agent)\n\n")
		}
	} else {
		// Include merge-queue configuration for non-fork mode
		sb.WriteString("## Merge Queue Configuration\n")
		if mqConfig.Enabled {
			sb.WriteString("- Enabled: yes\n")
			fmt.Fprintf(&sb, "- Track Mode: %s\n\n", mqConfig.TrackMode)
		} else {
			sb.WriteString("- Enabled: no (do NOT spawn merge-queue agent)\n\n")
		}
	}

	for i, def := range definitions {
		// Skip merge-queue definition in fork mode
		if isForkMode && def.Name == "merge-queue" {
			continue
		}
		// Skip pr-shepherd definition in non-fork mode
		if !isForkMode && def.Name == "pr-shepherd" {
			continue
		}

		fmt.Fprintf(&sb, "--- Agent Definition %d: %s (source: %s) ---\n", i+1, def.Name, def.Source)

		// For merge-queue, prepend the tracking mode configuration if enabled
		if def.Name == "merge-queue" && mqConfig.Enabled {
			trackModePrompt := prompts.GenerateTrackingModePrompt(string(mqConfig.TrackMode))
			sb.WriteString(trackModePrompt)
			sb.WriteString("\n\n")
		}

		// For pr-shepherd, prepend the tracking mode configuration if enabled
		if def.Name == "pr-shepherd" && psConfig.Enabled {
			trackModePrompt := prompts.GenerateTrackingModePrompt(string(psConfig.TrackMode))
			sb.WriteString(trackModePrompt)
			sb.WriteString("\n\n")
			// Also add fork workflow context
			forkPrompt := prompts.GenerateForkWorkflowPrompt(forkConfig.UpstreamOwner, forkConfig.UpstreamRepo, forkConfig.UpstreamOwner)
			sb.WriteString(forkPrompt)
			sb.WriteString("\n\n")
		}

		sb.WriteString(supervisorDefinitionBodyForMessage(def))
		sb.WriteString("\n--- End of Definition ---\n\n")
	}

	sb.WriteString("These are the agent types available in this repository.\n")
	sb.WriteString("All persistent agents (merge-queue, workspace) are already running -- they were started by `oat init` and are restored automatically by the daemon if they crash. Do not spawn them yourself.\n\n")
	sb.WriteString("Workers are created by the workspace agent or by users. You do not create workers proactively.\n")
	sb.WriteString("Your role: monitor agents, nudge stuck ones, and handle escalations.\n")

	// Send message to supervisor — attach an initial (likely empty) state
	// snapshot so the supervisor's "trust the snapshot" policy has a concrete
	// anchor from turn 1, not just in later nudges.
	msgMgr := d.getMessageManager()
	supMsg := d.withRepoSnapshot(repoName, state.AgentTypeSupervisor, sb.String())
	if _, err := msgMgr.Send(repoName, "daemon", "supervisor", supMsg); err != nil {
		return fmt.Errorf("failed to send message to supervisor: %w", err)
	}

	d.logger.Info("Sent %d agent definition(s) to supervisor for repo %s", len(definitions), repoName)
	return nil
}

// getAgentBinaryPath resolves the oat-agent binary path.
// Looks next to this binary first (co-located install), then falls back to PATH.
func (d *Daemon) getAgentBinaryPath() (string, error) {
	if p, err := findColocatedBinary("oat-agent"); err == nil {
		return p, nil
	}
	path, err := exec.LookPath("oat-agent")
	if err != nil {
		return "", fmt.Errorf("oat-agent not found next to oat binary or in PATH: %w", err)
	}
	return path, nil
}

// buildBrowserAgentMCPConfig produces the JSON written to .oat/mcp.json
// for a browser-agent worktree. The Python agent-runtime reads this at
// startup (see oat_sdk.mcp_client.load_mcp_config) and exposes the
// bridge's tools as LangChain tools. Returns "" if the bridge cannot be
// resolved -- the caller logs and continues without MCP rather than
// failing the agent spawn.
//
// `sessionName` and `agentName` are surfaced to the bridge process via
// `OAT_BROWSER_AGENT_SESSION` / `OAT_BROWSER_AGENT_NAME` so it can ask
// the daemon to address PTY input/output to the correct agent (Part 2b/2c
// — `agent_input` and `agent_output_subscribe` socket verbs). When the
// bridge runs outside of OAT (e.g. directly under Cursor or Claude
// Code), these vars are absent and the side-panel chat path stays
// disabled per Part 4.
func (d *Daemon) buildBrowserAgentMCPConfig(repoName, sessionName, agentName string) (string, error) {
	bridge, err := agents.ResolveBrowserBridge()
	if err != nil {
		return "", err
	}
	auditLogDir := d.paths.RepoOutputDir(repoName)
	// Bridge env is intentionally minimal. We pass only the
	// per-repo audit-log directory and let the bridge use its own
	// defaults for everything else:
	//
	//   - WS sidecar port: OS-assigned (port 0). The bridge writes
	//     the assigned port + per-launch session token to
	//     ~/.oat/output/<repo>/bridge-runtime.json, and the
	//     extension's NM broker (extension/src/nm-port.ts +
	//     bridge/src/nm-broker.ts) pushes them into
	//     chrome.storage.local so the SW reconnects to the right
	//     port with the right token.
	//   - Token handshake: required (the bridge's default since
	//     Part 9a). The NM broker is what makes this work for
	//     OAT-spawned bridges -- no need for the legacy
	//     trust-localhost escape hatch anymore.
	//
	// Side-effect of OS-assigned ports: two simultaneous bridges
	// (e.g. Cursor + OAT) no longer collide on bind(). They do
	// still contend for the single Chrome extension though -- the
	// last bridge to push its (port, token) to chrome.storage wins,
	// and the previous bridge loses its WS client until the next
	// NM handshake. That's the documented v1 limitation from plan
	// 8a; true concurrency is a Chrome-multi-profile or
	// extension-multi-tenant follow-up.
	server := map[string]any{
		"name":      "browser_bridge",
		"command":   bridge.Command,
		"args":      bridge.Args,
		"transport": "stdio",
		"env": map[string]string{
			"OAT_BROWSER_AGENT_AUDIT_LOG_DIR": auditLogDir,
			// Identity plumbing (Part 2a). The bridge uses these to
			// scope `agent_input` / `agent_output_subscribe` socket
			// calls to the right PTY. Empty values are a deliberate
			// signal that the bridge is not OAT-spawned (e.g. ran
			// directly under Cursor) -- the bridge treats them as
			// "chat path disabled".
			"OAT_BROWSER_AGENT_SESSION": sessionName,
			"OAT_BROWSER_AGENT_NAME":    agentName,
			// Stable bridge identity (Part 5g.1). The bridge writes
			// this into bridge-runtime.json so the NM broker (Part
			// 5g.2) can rank concurrent bridges without a WS round-
			// trip per candidate.
			//
			// Format `<repo>:<agent>` for browser-agents; the
			// `_assistant-<name>` shape lands when the assistant
			// agent type ships (Part 5). The bridge falls back to
			// the audit-log-dir's parent basename when this var is
			// missing -- backwards-compat for older daemons spawning
			// new bridges, or new daemons spawning older bridges
			// (the new bridge tolerates absence; the old bridge
			// ignores the env var entirely; either way the
			// downstream broker code does the right thing).
			//
			// NOTE: chat_capable is deliberately NOT passed via env.
			// The bridge derives it from
			// OAT_BROWSER_AGENT_SESSION/_NAME (the canonical Part
			// 4.A check) so the persisted value can never disagree
			// with the WS bridge_capabilities frame. Closes 5g.6 T1.
			"OAT_BROWSER_AGENT_ID": repoName + ":" + agentName,
		},
	}
	cfg := map[string]any{
		"servers": []map[string]any{server},
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal MCP config: %w", err)
	}
	return string(data), nil
}

// findColocatedBinary returns the absolute path to name if it exists next to
// the currently running executable (resolving symlinks).
func findColocatedBinary(name string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	p := filepath.Join(filepath.Dir(exe), name)
	if _, err := os.Stat(p); err != nil {
		return "", err
	}
	return p, nil
}

// ensureBinSymlinks creates/updates symlinks in ~/.oat/bin/ pointing to the
// actual oat and oat-agent binaries. This gives agents a neutral PATH entry
// that won't be confused with a project directory.
func (d *Daemon) ensureBinSymlinks() {
	bins := make(map[string]string)
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		bins["oat"] = exe
	}
	if agentBin, err := d.getAgentBinaryPath(); err == nil {
		if resolved, err := filepath.EvalSymlinks(agentBin); err == nil {
			agentBin = resolved
		}
		bins["oat-agent"] = agentBin
	}
	if len(bins) > 0 {
		if err := d.paths.EnsureBinSymlinks(bins); err != nil {
			d.logger.Error("Failed to create bin symlinks: %v", err)
		}
	}
}

// agentStartConfig holds configuration for starting an agent
type agentStartConfig struct {
	agentName  string
	agentType  state.AgentType
	promptFile string
	workDir    string
	model      string // LLM model override; falls back to repo.Model if empty
	// Optional initial user message sent via -m. If set, this is used instead
	// of sending the prompt text as the startup message.
	initialMessage string
}

// startAgentWithConfig is the unified agent start function that handles all common logic
func (d *Daemon) startAgentWithConfig(repoName string, repo *state.Repository, cfg agentStartConfig) error {
	// Generate session ID
	sessionID, err := agent_pkg.GenerateSessionID()
	if err != nil {
		return fmt.Errorf("failed to generate session ID: %w", err)
	}

	// Copy hooks config if needed
	repoPath := d.paths.RepoDir(repoName)
	if err := hooks.CopyConfig(repoPath, cfg.workDir); err != nil {
		d.logger.Warn("Failed to copy hooks config: %v", err)
	}

	// Resolve and validate model. Three code paths, gated by env flags:
	//
	// 1. Router V2 (OAT_ROUTER_VERSION=v2): lookup-aware routing via
	//    RouteForTaskV2. Same eligibility/floor as V1, then re-ranks
	//    candidates by static_score × historical_factor (Wilson lower bound
	//    on per-(model,complexity) success rate, with N>=5 threshold).
	//    Falls through to V1 behavior on empty/sparse corpus. Beats V1 when
	//    the corpus has signal.
	//
	// 2. Router V1 (OAT_ROUTING_V1=1, V2 not set): cost-aware routing via
	//    RouteForTask. Picks cheapest model meeting the tier floor.
	//
	// 3. Default: resolveAndValidateModelWithSource (argmax score within
	//    allowlist). Pre-rewrite behavior.
	//
	// The flags exist so A/B comparison is possible without a branch switch.
	var resolvedModel, routingSource string
	var routingDecisionReason string
	var routingCandidates []string
	var modelErr error

	v2Enabled := os.Getenv("OAT_ROUTER_VERSION") == "v2"
	v1Enabled := os.Getenv("OAT_ROUTING_V1") == "1" || v2Enabled // V2 requires the V1 stack
	routerEligible := v1Enabled &&
		cfg.agentType == state.AgentTypeWorker &&
		cfg.model == "" &&
		d.pricing != nil && d.pricing.Count() > 0 &&
		d.modelProfiles != nil && d.modelProfiles.Count() > 0

	if routerEligible {
		// Strip the "Task: " prefix startAgentConfig adds, so the classifier
		// sees the user's actual task description.
		taskText := strings.TrimPrefix(cfg.initialMessage, "Task: ")
		routeCtx := routing.RouteContext{
			TaskText:      taskText,
			Role:          routing.RoleWorker,
			AllowedModels: repo.AllowedWorkerModels,
		}
		var decision *routing.RouteDecision
		var routeErr error
		if v2Enabled {
			decision, routeErr = d.modelProfiles.RouteForTaskV2(routeCtx, d.pricing, d.getCorpusIndex())
		} else {
			decision, routeErr = d.modelProfiles.RouteForTask(routeCtx, d.pricing)
		}
		routerName := "RouterV1"
		if v2Enabled {
			routerName = "RouterV2"
		}
		if routeErr != nil {
			d.logger.Warn("%s: %v — falling back to default router", routerName, routeErr)
		} else {
			d.logger.Info("%s: %s  complexity=%s  candidates=%v", routerName, decision.Reason, decision.Complexity, decision.Candidates)
			// Validate the pick (handles status=restricted + allowlist recheck).
			if vErr := d.validateModelForAgentType(decision.ChosenModel, cfg.agentType, repo.AllowedWorkerModels); vErr != nil {
				d.logger.Warn("%s picked %s but validation failed: %v — falling back", routerName, decision.ChosenModel, vErr)
			} else {
				resolvedModel = decision.ChosenModel
				routingSource = decision.RoutingSource
				routingDecisionReason = decision.Reason
				routingCandidates = decision.Candidates
			}
		}
	}

	// Fall through to default router if V1 was not eligible or failed.
	if resolvedModel == "" {
		resolvedModel, routingSource, modelErr = d.resolveAndValidateModelWithSource(cfg.model, repo.Model, cfg.agentType, repo.AllowedWorkerModels)
		if modelErr != nil {
			return fmt.Errorf("model routing: %w", modelErr)
		}
	}

	// Capture prompt metadata BEFORE the backend-spawn block so test-mode
	// agents (which skip backend) still get hashes. Reading the prompt file
	// here is a duplicate of the read inside StartAgent below; it's cheap
	// (small files) and the duplication is preferable to threading the
	// content through extra plumbing. Failures are silent: prompt metadata
	// is best-effort, never blocks spawn.
	var spawnSystemPromptContent string
	if cfg.promptFile != "" {
		if data, readErr := os.ReadFile(cfg.promptFile); readErr == nil {
			spawnSystemPromptContent = string(data)
		}
	}
	taskTextForHash := strings.TrimPrefix(cfg.initialMessage, "Task: ")

	var pid int

	// Skip actual agent startup in test mode
	if os.Getenv("OAT_TEST_MODE") != "1" {
		// Resolve oat-agent binary path
		binaryPath, err := d.getAgentBinaryPath()
		if err != nil {
			return fmt.Errorf("failed to resolve agent binary: %w", err)
		}

		// Build env prefix so core agents get user secrets (shell profile + .env)
		envPrefix := buildAgentEnvPrefix(d.paths, repoName)

		// Build args for oat-agent CLI
		args := []string{"--auto-approve"}

		// Pass startup message via -m only when explicitly requested
		// (e.g. worker task). System prompts are provided via AGENTS.md.
		if cfg.initialMessage != "" {
			args = append(args, "-m", cfg.initialMessage)
		}

		if resolvedModel != "" {
			args = append(args, "-M", resolvedModel)
		}
		args = append(args, "--model-params", d.modelParamsJSON(resolvedModel))
		// Browser-agent tool catalog filter. See denyToolArgs() for rationale.
		args = append(args, denyToolArgs(cfg.agentType)...)

		// Determine log file path
		isWorker := cfg.agentType == state.AgentTypeWorker || cfg.agentType == state.AgentTypeReview || cfg.agentType == state.AgentTypeVerification
		logFile := d.paths.AgentLogFile(repoName, cfg.agentName, isWorker)
		logDir := filepath.Dir(logFile)
		_ = os.MkdirAll(logDir, 0755)

		// Read prompt content for InitialPrompt (written to AGENTS.md by backend)
		var promptContent string
		if cfg.promptFile != "" {
			if data, readErr := os.ReadFile(cfg.promptFile); readErr == nil {
				promptContent = string(data)
			}
		}

		// Start agent via backend (handles window creation, command, PID, output capture)
		envVars := []string{fmt.Sprintf("OAT_AGENT_NAME=%s", cfg.agentName)}
		if backend := backendModeName(d.backend); backend != "" {
			envVars = append(envVars, fmt.Sprintf("OAT_BACKEND=%s", backend))
		}
		envVars = append(envVars, fmt.Sprintf("OAT_TOOL_LOG=%s", logFile))
		// Sidecar socket path (empty when OAT_USE_SIDECAR != 1). Must be
		// wired at every StartAgent call site or the feature flag is
		// partial — agents created through this path would silently skip
		// sidecar emission even when the operator enabled it.
		sidecarPath := sidecarSocketPath(repoName, cfg.agentName)

		// MCP config for browser-agent. This path is hit by
		// startAgent() -> startAgentWithConfig() which is what
		// restoreRepoAgents (the recovery path) calls. Without this
		// branch, a daemon-restart-restored browser-agent would
		// launch with no .oat/mcp.json and silently lose all its
		// browser tools. Resolution failure logs a WARN; the agent
		// still spawns so the operator can attach and diagnose.
		var mcpConfig string
		if usesBrowserBridge(cfg.agentType) {
			mc, mcpErr := d.buildBrowserAgentMCPConfig(repoName, repo.SessionName, cfg.agentName)
			if mcpErr != nil {
				d.logger.Warn("Bridge-using agent %s/%s (%s) starting without MCP tools: %v", repoName, cfg.agentName, cfg.agentType, mcpErr)
			} else {
				mcpConfig = mc
			}
		}

		handle, err := d.backend.StartAgent(d.ctx, backend_pkg.AgentConfig{
			SessionName:   repo.SessionName,
			AgentName:     cfg.agentName,
			WorkDir:       cfg.workDir,
			BinaryPath:    binaryPath,
			Args:          args,
			Env:           envVars,
			EnvPrefix:     envPrefix,
			InitialPrompt: promptContent,
			MCPConfig:     mcpConfig,
			LogFile:       logFile,
			SidecarPath:   sidecarPath,
		})
		if err != nil {
			return fmt.Errorf("failed to start agent: %w", err)
		}
		if handle != nil {
			pid = handle.PID
		}
	}

	// Register agent with state
	agent := state.Agent{
		Type:                  cfg.agentType,
		WorktreePath:          cfg.workDir,
		WindowName:            cfg.agentName,
		SessionID:             sessionID,
		PID:                   pid,
		CreatedAt:             time.Now(),
		Model:                 resolvedModel,
		RoutingSource:         routingSource,
		RoutingDecisionReason: routingDecisionReason,
		RoutingCandidates:     routingCandidates,
		RoutingAllowlist:      append([]string(nil), repo.AllowedWorkerModels...),
		PromptSystemHash:      routing.HashPromptText(spawnSystemPromptContent),
		PromptSystemTokens:    routing.EstimateTokens(spawnSystemPromptContent),
		PromptUserHash:        routing.HashPromptText(taskTextForHash),
		PromptUserTokens:      routing.EstimateTokens(taskTextForHash),
	}

	if err := d.state.AddAgent(repoName, cfg.agentName, agent); err != nil {
		return fmt.Errorf("failed to register agent: %w", err)
	}

	// Start output watcher for agent log file (non-blocking)
	if os.Getenv("OAT_TEST_MODE") != "1" {
		isWorker := cfg.agentType == state.AgentTypeWorker || cfg.agentType == state.AgentTypeReview || cfg.agentType == state.AgentTypeVerification
		logFile := d.paths.AgentLogFile(repoName, cfg.agentName, isWorker)
		d.startOutputWatcher(repoName, cfg.agentName, logFile)
	}

	modelInfo := resolvedModel
	if modelInfo == "" {
		modelInfo = "(no model)"
	}
	d.logger.Info("Started and registered agent %s/%s model=%s", repoName, cfg.agentName, modelInfo)
	return nil
}

// startAgent starts an agent in a backend window and registers it with state
func (d *Daemon) startAgent(repoName string, repo *state.Repository, agentName string, agentType state.AgentType, workDir string) error {
	promptFile, err := d.writePromptFile(repoName, agentType, agentName)
	if err != nil {
		return fmt.Errorf("failed to write prompt file: %w", err)
	}

	return d.startAgentWithConfig(repoName, repo, agentStartConfig{
		agentName:  agentName,
		agentType:  agentType,
		promptFile: promptFile,
		workDir:    workDir,
	})
}

// startOutputWatcher launches an OutputWatcher goroutine that tails the agent's
// log file and logs detected events (PR creation, errors, stuck loops).
func (d *Daemon) startOutputWatcher(repoName, agentName, logFile string) {
	// Ensure the log directory and file exist before the watcher attaches.
	// Workers (workers/<name>.log) are created after this function runs —
	// Python doesn't write the file until the first stream completes, so
	// opening read-only here would fail and the watcher would silently
	// never attach. Creating the file up front (O_CREATE) guarantees the
	// tailer sees every byte the agent emits.
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		d.logger.Warn("Failed to create log dir for %s/%s: %v", repoName, agentName, err)
		return
	}
	// Touch the file so the tailer has something to open.
	if touchFile, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		touchFile.Close()
	}

	f, err := os.Open(logFile)
	if err != nil {
		d.logger.Warn("Could not open log file for output watcher %s/%s: %v", repoName, agentName, err)
		return
	}

	// Seek to end — we only want new output; best-effort (fall back to reading from 0 on failure).
	_, _ = f.Seek(0, 2)

	watcher := agent_pkg.NewOutputWatcher(f)

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer f.Close()
		defer watcher.Stop()
		defer func() {
			if r := recover(); r != nil {
				d.logger.Error("Panic in output watcher for %s/%s: %v", repoName, agentName, r)
			}
		}()

		for {
			select {
			case <-d.ctx.Done():
				// Drain buffered token events before exiting so we don't lose
				// up-to-32 events/agent worth of spend data on shutdown. The
				// deferred watcher.Stop() will close w.events (via watch()'s
				// defer close(w.events) at output_parser.go:146) which is our
				// primary exit signal; the deadline is belt-and-braces so we
				// never hold up daemon stop.
				drained := 0
				deadline := time.After(shutdownDrainTimeout)
				for {
					select {
					case event, ok := <-watcher.Events():
						if !ok {
							if drained > 0 {
								d.logger.Info("Drained %d pending token events for %s/%s on shutdown", drained, repoName, agentName)
							}
							return
						}
						if event.Type == agent_pkg.EventTokenUsage {
							d.handleTokenUsageEvent(repoName, agentName, event.Message)
							drained++
						}
					case <-deadline:
						if drained > 0 {
							d.logger.Warn("Drained %d pending token events for %s/%s on shutdown (deadline hit)", drained, repoName, agentName)
						}
						return
					}
				}
			case event, ok := <-watcher.Events():
				if !ok {
					return
				}
				switch event.Type {
				case agent_pkg.EventPRCreated:
					d.logger.Info("Agent %s/%s created PR: %s", repoName, agentName, event.Message)
				case agent_pkg.EventError:
					d.logger.Warn("Agent %s/%s error: %s", repoName, agentName, event.Message)
				case agent_pkg.EventTaskComplete:
					d.logger.Info("Agent %s/%s task complete: %s", repoName, agentName, event.Message)
				case agent_pkg.EventStuck:
					d.logger.Warn("Agent %s/%s may be stuck: %s", repoName, agentName, event.Message)
				case agent_pkg.EventIdle:
					d.logger.Info("Agent %s/%s idle: %s", repoName, agentName, event.Message)
				case agent_pkg.EventTokenUsage:
					d.handleTokenUsageEvent(repoName, agentName, event.Message)
				}
			}
		}
	}()
}

// handleTokenUsageEvent parses an [OAT_TOKENS] JSON payload and updates the
// agent's token counters in state.
//
// The runtime emits honest field names:
//   - delta_input / delta_output: tokens spent this request
//   - cumulative_input / cumulative_output: monotonic lifetime totals
//
// Cumulative values are authoritative.  A combined-total monotonicity guard
// rejects stale/replayed events (e.g. after process restart) — the guard
// compares the incoming combined total against the stored combined total and
// silently drops payloads where the new total is lower.  This is a pragmatic
// tradeoff: a payload where input drops and output rises more could still be
// accepted, resulting in slightly shifted attribution while total remains
// correct.  Acceptable because the TUI displays spend totals, not per-axis
// fidelity.
//
// Mixed-version operation is unsupported for v1.  Python runtime and Go
// daemon/TUI ship atomically in the same release.
func (d *Daemon) handleTokenUsageEvent(repoName, agentName, jsonPayload string) {
	var payload struct {
		DeltaInput       int64 `json:"delta_input"`
		DeltaOutput      int64 `json:"delta_output"`
		CumulativeInput  int64 `json:"cumulative_input"`
		CumulativeOutput int64 `json:"cumulative_output"`
		CacheRead        int64 `json:"cache_read,omitempty"`
		CacheCreation    int64 `json:"cache_creation,omitempty"`
	}
	if err := json.Unmarshal([]byte(jsonPayload), &payload); err != nil {
		d.logger.Warn("Failed to parse token usage from %s/%s: %v", repoName, agentName, err)
		return
	}

	agent, exists := d.state.GetAgent(repoName, agentName)
	if !exists {
		return
	}

	// Monotonicity guard: cumulative must be >= existing combined total.
	// Lower cumulative = stale/replayed event from restarted process → ignore.
	// No delta fallback — delta replay is not safe.
	newTotal := payload.CumulativeInput + payload.CumulativeOutput
	oldTotal := agent.InputTokens + agent.OutputTokens
	if newTotal < oldTotal {
		d.logger.Warn(
			"Dropped stale token usage for %s/%s: incoming total %d < stored %d (agent or daemon restart replay?)",
			repoName, agentName, newTotal, oldTotal,
		)
		return
	}

	// Per-field cache monotonicity clamp: cache counters are cumulative lifetime
	// totals from the runtime. A payload reporting a lower value than what we
	// already have (e.g. a post-restart replay where the new process starts
	// with a cold cache and emits cache_read=0) must not clobber the stored
	// warm total. Clamp each field independently — input/output are already
	// guarded above by the combined-total check.
	newCacheRead := payload.CacheRead
	newCacheCreation := payload.CacheCreation
	if newCacheRead < agent.CacheReadTokens {
		d.logger.Warn(
			"Cache read regression for %s/%s: incoming %d < stored %d; keeping stored",
			repoName, agentName, newCacheRead, agent.CacheReadTokens,
		)
		newCacheRead = agent.CacheReadTokens
	}
	if newCacheCreation < agent.CacheCreationTokens {
		d.logger.Warn(
			"Cache creation regression for %s/%s: incoming %d < stored %d; keeping stored",
			repoName, agentName, newCacheCreation, agent.CacheCreationTokens,
		)
		newCacheCreation = agent.CacheCreationTokens
	}

	agent.InputTokens = payload.CumulativeInput
	agent.OutputTokens = payload.CumulativeOutput
	agent.TotalTokens = agent.InputTokens + agent.OutputTokens
	agent.CacheReadTokens = newCacheRead
	agent.CacheCreationTokens = newCacheCreation
	agent.LastTokenUpdate = time.Now()

	if err := d.state.UpdateAgent(repoName, agentName, agent); err != nil {
		d.logger.Warn("Failed to persist token usage for %s/%s: %v", repoName, agentName, err)
	}

	// Token budget enforcement: kill worker if it exceeds its budget.
	if agent.MaxTokens > 0 && agent.TotalTokens > agent.MaxTokens {
		d.logger.Warn("Token budget exceeded for %s/%s: %d > %d (budget). Stopping agent.",
			repoName, agentName, agent.TotalTokens, agent.MaxTokens)
		go d.stopAgentOverBudget(repoName, agentName, agent)
	}
}

// stopAgentOverBudget kills a worker that exceeded its token budget and notifies
// the supervisor/workspace. Runs in a goroutine from handleTokenUsageEvent.
func (d *Daemon) stopAgentOverBudget(repoName, agentName string, agent state.Agent) {
	repo, exists := d.state.GetRepo(repoName)
	if !exists {
		return
	}

	// Kill the agent process
	windowName := agent.WindowName
	if windowName == "" {
		windowName = agentName
	}
	if err := d.backend.StopAgent(d.ctx, repo.SessionName, windowName); err != nil {
		d.logger.Warn("Failed to stop over-budget agent %s/%s: %v", repoName, agentName, err)
	}

	// Mark as failed with budget reason
	agent.FailureReason = fmt.Sprintf("token budget exceeded: %d / %d tokens", agent.TotalTokens, agent.MaxTokens)
	agent.ReadyForCleanup = true
	agent.ReadyForCleanupAt = time.Now()
	if err := d.state.UpdateAgent(repoName, agentName, agent); err != nil {
		d.logger.Warn("Failed to update over-budget agent state %s/%s: %v", repoName, agentName, err)
	}

	// Notify workspace about the budget kill
	msgMgr := d.getMessageManager()
	msg := fmt.Sprintf("[daemon] Worker '%s' was stopped: token budget exceeded (%d / %d tokens). Task: '%s'",
		agentName, agent.TotalTokens, agent.MaxTokens, agent.Task)
	if _, err := msgMgr.Send(repoName, "daemon", "default", msg); err != nil {
		d.logger.Warn("Failed to notify workspace about budget kill %s: %v", agentName, err)
	}

	d.logger.Info("Stopped over-budget agent %s/%s (%d / %d tokens)", repoName, agentName, agent.TotalTokens, agent.MaxTokens)
}

// writePromptFileWithPrefix writes a prompt file with an optional prefix prepended to the content
func (d *Daemon) writePromptFileWithPrefix(repoName string, agentType state.AgentType, agentName, prefix string) (string, error) {
	repoPath := d.paths.RepoDir(repoName)

	var promptText string

	switch agentType {
	case state.AgentTypeMergeQueue, state.AgentTypeWorker, state.AgentTypeReview, state.AgentTypeVerification, state.AgentTypeBrowser, state.AgentTypeAssistant:
		localAgentsDir := d.paths.RepoAgentsDir(repoName)
		// Part 4.H: keep the per-repo agents/ dir in sync with the
		// embedded templates on EVERY prompt-write call, not just on
		// first-time clone setup. Without this, edits to
		// internal/templates/agent-templates/*.md only land on fresh
		// repos and silently stale on every existing one. Idempotent:
		// only files that drift from the embedded content are
		// rewritten. User customization lives under a separate
		// `Repository-specific instructions:` heading appended
		// later via prompts.LoadCustomPrompt — overwriting
		// agents/*.md here is non-destructive for that flow.
		if refreshed, syncErr := templates.SyncAgentTemplates(localAgentsDir); syncErr != nil {
			d.logger.Warn("Failed to sync agent templates for %s: %v", agentType, syncErr)
		} else if len(refreshed) > 0 {
			d.logger.Info("Refreshed %d agent template(s) in %s: %v", len(refreshed), localAgentsDir, refreshed)
		}

		reader := agents.NewReader(localAgentsDir, repoPath)
		definitions, err := reader.ReadAllDefinitions()
		if err != nil {
			d.logger.Warn("Failed to read agent definitions for %s: %v", agentType, err)
		}

		defName := string(agentType)
		for _, def := range definitions {
			if def.Name == defName {
				promptText = def.Content
				break
			}
		}

		// Part 5b: for bridge-using agent types (browser + assistant),
		// concatenate the shared `_shared-browser-safety.md` fragment
		// after the per-type prompt. The fragment carries the
		// safety-critical bits (Safety Rules / Prompt Injection
		// Defense / Cross-Tab Discipline / Dedicated Agent Window)
		// that both agents must obey -- sharing prevents drift
		// between the two prompts' safety sections (the most
		// security-sensitive part of the prompt; a fix in one would
		// silently miss the other). Loaded via the same
		// agents.NewReader path that already read the per-type
		// definition, so the lookup is O(N) over a tiny N and adds
		// no IO. The fragment's filename starts with "_" so it
		// never matches an AgentType string and can't be picked up
		// as a primary prompt by accident.
		if usesBrowserBridge(agentType) {
			const sharedFragmentName = "_shared-browser-safety"
			for _, def := range definitions {
				if def.Name == sharedFragmentName {
					if promptText != "" {
						promptText += "\n\n---\n\n"
					}
					promptText += def.Content
					break
				}
			}
		}

		if slashCmds := prompts.GetSlashCommandsPrompt(string(agentType)); slashCmds != "" {
			promptText += "\n\n---\n\n" + slashCmds
		}

		customPrompt, _ := prompts.LoadCustomPrompt(repoPath, agentType)
		if customPrompt != "" {
			promptText += fmt.Sprintf("\n\n---\n\nRepository-specific instructions:\n\n%s", customPrompt)
		}

	default:
		// Supervisor, workspace, and others: use embedded default prompts
		text, err := prompts.GetPrompt(repoPath, agentType, "")
		if err != nil {
			return "", fmt.Errorf("failed to get prompt: %w", err)
		}
		promptText = text
	}

	var prefixes []string

	// Inject model roster into supervisor/workspace prompts so they can make routing decisions
	if (agentType == state.AgentTypeSupervisor || agentType == state.AgentTypeWorkspace) && d.modelProfiles != nil && d.modelProfiles.Count() > 0 {
		var allowedModels []string
		if repo, ok := d.state.GetRepo(repoName); ok && repo != nil {
			allowedModels = repo.AllowedWorkerModels
		}
		roster := routing.GenerateModelRoster(d.modelProfiles, allowedModels)
		if roster != "" {
			prefixes = append(prefixes, roster)
		}
	}

	if d.isDirectBackend() {
		prefixes = append(prefixes, "## Runtime Backend (IMPORTANT)\nYou are running under the OAT backend.\n- Use `oat` commands for orchestration/status.\n- For normal task dispatch, create workers with `oat worker create \"<task>\" --repo <repo>`.\n- Do not use `oat agents spawn` for routine worker tasks.\n- If asked repeatedly for status and nothing changed, reply briefly with \"no change\" instead of repeating long output.")
	}
	// Inject concrete scratchpad path for worker/review/verification agents
	// so they can read/write shared knowledge without guessing the repo name.
	if agentType == state.AgentTypeWorker || agentType == state.AgentTypeReview || agentType == state.AgentTypeVerification {
		scratchpadDir := d.paths.ScratchpadDir(repoName)
		prefixes = append(prefixes, fmt.Sprintf("Shared scratchpad path: %s", scratchpadDir))
	}
	if prefix != "" {
		prefixes = append(prefixes, prefix)
	}
	if len(prefixes) > 0 {
		promptText = strings.Join(prefixes, "\n\n") + "\n\n" + promptText
	}

	// Create prompt file in prompts directory
	promptDir := filepath.Join(d.paths.Root, "prompts")
	if err := os.MkdirAll(promptDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create prompt directory: %w", err)
	}

	promptPath := filepath.Join(promptDir, fmt.Sprintf("%s.md", agentName))
	if err := os.WriteFile(promptPath, []byte(promptText), 0644); err != nil {
		return "", fmt.Errorf("failed to write prompt file: %w", err)
	}

	return promptPath, nil
}

// bridgeUnreachableThreshold and bridgeUnreachableWindow define the
// back-off policy for browser-agent auto-restart. Per Part 2 of
// mcp-and-opt-in-browser-agent_a10544be.plan.md: if the health check
// finds the browser-agent dead this many times within the window, stop
// respawning and require the user to manually `oat agent restart
// browser-agent`. Prevents the 2-min health-check loop from spinning a
// doomed bridge subprocess every cycle when Chrome is closed or the
// extension is uninstalled.
const (
	bridgeUnreachableThreshold = 3
	bridgeUnreachableWindow    = 10 * time.Minute
)

// recordBridgeUnreachable appends now to the sliding window for key
// ("<repo>/<agent>"), prunes entries older than bridgeUnreachableWindow,
// and returns the resulting window length. Callers use the return value
// to decide whether to give up on auto-restarting the agent.
func (d *Daemon) recordBridgeUnreachable(key string, now time.Time) int {
	d.bridgeUnreachableMu.Lock()
	defer d.bridgeUnreachableMu.Unlock()
	cutoff := now.Add(-bridgeUnreachableWindow)
	timestamps := d.bridgeUnreachable[key]
	pruned := timestamps[:0]
	for _, t := range timestamps {
		if t.After(cutoff) {
			pruned = append(pruned, t)
		}
	}
	pruned = append(pruned, now)
	d.bridgeUnreachable[key] = pruned
	return len(pruned)
}

// clearBridgeUnreachable drops the failure window for key. Called when
// a browser-agent restart succeeds OR when the user explicitly issues
// `oat agent restart` (the latter so the next failure restarts the
// counter rather than instantly hitting the threshold again).
func (d *Daemon) clearBridgeUnreachable(key string) {
	d.bridgeUnreachableMu.Lock()
	defer d.bridgeUnreachableMu.Unlock()
	delete(d.bridgeUnreachable, key)
}

// restartAgent restarts an agent that has exited.
// It uses --resume to continue the existing session if history exists.
// This works for all agent types: supervisor, merge-queue, workspace, workers, and review agents.
func (d *Daemon) restartAgent(repoName, agentName string, agent state.Agent, repo *state.Repository) error {
	// Check if the session has history
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	// Remove stale session lock so "Session ID already in use" does not block restart (Bug 1 Option D)
	sessionLockDir := filepath.Join(home, ".claude", "session-env", agent.SessionID)
	if err := os.RemoveAll(sessionLockDir); err != nil {
		d.logger.Warn("Failed to remove session lock %s: %v", sessionLockDir, err)
	}

	// NOTE: prior code sent the literal string "clear" to the agent
	// here, claiming to clear the tmux pane buffer ("Bug 1 Option C").
	// On the tmux backend that ran the `clear` shell builtin in the
	// agent's pane; on the PTY backend stdin goes directly to the
	// MODEL, which then dutifully invokes the `clear` *tool* on every
	// restart — burning tokens and producing the "(Screen cleared —
	// ready for your next task.)" ASSISTANT line operators kept seeing
	// after `oat agent restart --force`. The PTY backend allocates a
	// fresh terminal for the new process anyway, so there is no buffer
	// to inherit. Leaving this as a comment so the next person who
	// thinks "I should re-add a clear-on-restart" reads the
	// archaeology before re-introducing the bug.

	agentProjectsDir := filepath.Join(home, ".claude", "projects")
	encodedPath := strings.ReplaceAll(agent.WorktreePath, "/", "-")
	sessionFile := filepath.Join(agentProjectsDir, encodedPath, agent.SessionID+".jsonl")

	hasHistory := false
	if info, err := os.Stat(sessionFile); err == nil && info.Size() > 0 {
		hasHistory = true
	}

	// Always regenerate prompt file on restart so code changes take effect
	promptFile, err := d.writePromptFile(repoName, agent.Type, agentName)
	if err != nil {
		return fmt.Errorf("failed to regenerate prompt file: %w", err)
	}

	// Resolve agent binary path
	binaryPath, err := d.getAgentBinaryPath()
	if err != nil {
		return fmt.Errorf("failed to resolve agent binary: %w", err)
	}

	// Build args for oat-agent CLI
	args := []string{"--auto-approve"}
	if hasHistory {
		args = append(args, "--resume", agent.SessionID)
	} else {
		// Send system prompt as initial message for fresh starts
		if data, readErr := os.ReadFile(promptFile); readErr == nil && len(data) > 0 {
			args = append(args, "-m", string(data))
		}
	}

	// Validate model on restart (prevents switching to an ineligible model).
	//
	// On validation failure we try harder than the previous "fallback to
	// previous model" path, which silently kept the un-validatable model alive
	// — the supervisor-on-flash bug from the Phase 5 audit. Strategy:
	//
	//   1. Try the agent's stored model (respects operator-set overrides).
	//   2. If rejected, auto-select from the pool (explicitModel="" triggers
	//      BestEligible inside resolveAndValidateModel).
	//   3. If THAT also fails (no eligible models at all), log an error and
	//      fall back to agent.Model just to keep the agent running — operator
	//      has visibility via WARN and can fix the team.
	resolvedModel, modelErr := d.resolveAndValidateModel(agent.Model, repo.Model, agent.Type, repo.AllowedWorkerModels)
	if modelErr != nil {
		d.logger.Warn("Model routing failed on restart for %s: %v — attempting auto-select from pool", agentName, modelErr)
		autoResolved, autoErr := d.resolveAndValidateModel("", repo.Model, agent.Type, repo.AllowedWorkerModels)
		if autoErr == nil && autoResolved != "" && autoResolved != agent.Model {
			// Persist so next restart starts from the validated model AND
			// the swap is loudly visible. Operators were missing this
			// before — the INFO log line is easy to scroll past and the
			// agent record didn't carry the marker.
			d.logger.Warn("Model routing: %s SWAPPED from %q to %q on restart (auto-selected; original reason: %v)", agentName, agent.Model, autoResolved, modelErr)
			previous := agent.Model
			resolvedModel = autoResolved
			if _, ok := d.state.GetAgent(repoName, agentName); ok {
				agent.ModelSwapPrevious = previous
				agent.Model = autoResolved
				agent.ModelSwappedOnRestart = true
				agent.ModelSwapReason = modelErr.Error()
				_ = d.state.UpdateAgent(repoName, agentName, agent)
			}
		} else {
			d.logger.Error("Model routing: no valid model for %s/%s after auto-select (primary err=%v, auto err=%v) — restarting with previous model %q anyway", repoName, agentName, modelErr, autoErr, agent.Model)
			resolvedModel = resolveAgentModel(agent, repo)
		}
	} else if agent.ModelSwappedOnRestart {
		// The agent's stored model validated cleanly this time around —
		// the underlying registry issue must have been fixed (operator
		// onboarded the model, fixed the prefix in state.json, etc.).
		// Clear the swap marker so it doesn't keep haunting `oat agent ls`.
		if _, ok := d.state.GetAgent(repoName, agentName); ok {
			d.logger.Info("Model routing: %s previous swap cleared (model %q now validates)", agentName, agent.Model)
			agent.ModelSwappedOnRestart = false
			agent.ModelSwapReason = ""
			agent.ModelSwapPrevious = ""
			_ = d.state.UpdateAgent(repoName, agentName, agent)
		}
	}
	if resolvedModel != "" {
		args = append(args, "-M", resolvedModel)
	}
	args = append(args, "--model-params", d.modelParamsJSON(resolvedModel))
	// Browser-agent tool catalog filter. See denyToolArgs() for rationale.
	args = append(args, denyToolArgs(agent.Type)...)

	envPrefix := buildAgentEnvPrefix(d.paths, repoName)
	isWorker := agent.Type == state.AgentTypeWorker || agent.Type == state.AgentTypeReview || agent.Type == state.AgentTypeVerification
	logFile := d.paths.AgentLogFile(repoName, agentName, isWorker)
	logDir := filepath.Dir(logFile)
	_ = os.MkdirAll(logDir, 0755)

	// Read prompt content for InitialPrompt
	var promptContent string
	if data, readErr := os.ReadFile(promptFile); readErr == nil {
		promptContent = string(data)
	}

	// Restart via backend
	envVars := []string{fmt.Sprintf("OAT_AGENT_NAME=%s", agentName)}
	if backend := backendModeName(d.backend); backend != "" {
		envVars = append(envVars, fmt.Sprintf("OAT_BACKEND=%s", backend))
	}
	envVars = append(envVars, fmt.Sprintf("OAT_TOOL_LOG=%s", logFile))
	// Sidecar path also wired on restart so an agent that was started
	// with sidecar on keeps sidecar on across a manual restart.
	sidecarPath := sidecarSocketPath(repoName, agentName)

	// Re-resolve MCP config on restart -- the bridge binary may have
	// been upgraded between the first spawn and the restart, and we
	// always want to write the freshest .oat/mcp.json.
	var mcpConfig string
	if usesBrowserBridge(agent.Type) {
		cfg, mcpErr := d.buildBrowserAgentMCPConfig(repoName, repo.SessionName, agentName)
		if mcpErr != nil {
			d.logger.Warn("Bridge-using agent %s/%s (%s) restarting without MCP tools: %v", repoName, agentName, agent.Type, mcpErr)
		} else {
			mcpConfig = cfg
		}
	}

	// IMPORTANT: start the assistant-turn tailer BEFORE spawning the
	// agent process. The agent's bridge subprocess connects to the
	// daemon socket and calls `stream_assistant_turns` almost
	// immediately after the agent's MCP plumbing is up; if the
	// tailer registration races behind that subscribe, the bridge's
	// handler returns "no assistant-turn tailer active" and the
	// bridge falls into its 1s/2s/... reconnect backoff. The agent's
	// first ASSISTANT turn often lands inside that backoff window,
	// and the broadcaster does not replay — turn is lost forever
	// (smoke-test report: 2026-05-18, post-restart replies invisible
	// in the side panel while the oat-ui terminal showed them
	// correctly). The tailer.run() goroutine itself polls until the
	// log file exists, so registering early when the file may not
	// yet be created is safe.
	if os.Getenv("OAT_TEST_MODE") != "1" && usesBrowserBridge(agent.Type) {
		d.startAssistantTurnTailer(repo.SessionName, agentName, logFile)
	}

	handle, err := d.backend.StartAgent(d.ctx, backend_pkg.AgentConfig{
		SessionName:   repo.SessionName,
		AgentName:     agentName,
		WorkDir:       agent.WorktreePath,
		BinaryPath:    binaryPath,
		Args:          args,
		Env:           envVars,
		EnvPrefix:     envPrefix,
		InitialPrompt: promptContent,
		MCPConfig:     mcpConfig,
		LogFile:       logFile,
		SidecarPath:   sidecarPath,
	})
	if err != nil {
		return fmt.Errorf("failed to restart agent: %w", err)
	}

	pid := 0
	if handle != nil {
		pid = handle.PID
	}

	// Update the agent's PID in state
	if err := d.state.UpdateAgentPID(repoName, agentName, pid); err != nil {
		d.logger.Warn("Failed to update agent PID: %v", err)
	}

	// Re-attach OutputWatcher so token / PR / error events flow again after
	// a restart. Without this, the agent emits [OAT_TOKENS] but nobody is
	// reading the log file and state.json stays frozen.
	if os.Getenv("OAT_TEST_MODE") != "1" {
		d.startOutputWatcher(repoName, agentName, logFile)
	}

	d.logger.Info("Restarted agent %s with PID %d (resumed=%v)", agentName, pid, hasHistory)
	return nil
}

// writePromptFile writes the agent prompt to a file and returns the path
func (d *Daemon) writePromptFile(repoName string, agentType state.AgentType, agentName string) (string, error) {
	return d.writePromptFileWithPrefix(repoName, agentType, agentName, "")
}

// isProcessAlive checks if a process is running

// resolveAgentModel returns the agent-level model if set, otherwise the repo default.
func resolveAgentModel(agent state.Agent, repo *state.Repository) string {
	if agent.Model != "" {
		return agent.Model
	}
	return repo.Model
}

// validateModelForAgentType applies the same checks resolveAndValidateModel
// does (profile eligibility + allowlist for workers), without the
// auto-selection fallback. Returns nil if the model is safe to use; returns
// an error the caller can log or surface.
//
// This exists for code paths that already have a resolved model (e.g. agent
// restart / restore) and just need the "is this still allowed?" check. It is
// the chokepoint closing the startRegisteredAgent bypass identified in the
// Phase 5 audit.
func (d *Daemon) validateModelForAgentType(model string, agentType state.AgentType, allowedModels []string) error {
	if model == "" {
		return nil // caller handles empty-model defaulting
	}
	if d.modelProfiles == nil || d.modelProfiles.Count() == 0 {
		return nil // no profiles — can't validate, pass through
	}

	role := routing.RoleWorker
	if agentType == state.AgentTypeSupervisor || agentType == state.AgentTypeWorkspace || agentType == state.AgentTypeMergeQueue || agentType == state.AgentTypePRShepherd {
		role = routing.RoleOrchestrator
	}

	canonical, err := d.modelProfiles.ValidateAndCanonicalize(model, role)
	if err != nil {
		return err
	}

	// Allowlist check only applies to workers (matches resolveAndValidateModel semantics).
	// The allowlist is checked against BOTH the original input and the canonical form, so
	// operators who configured the repo with an unprefixed entry still match. Operators are
	// encouraged to canonicalize their allowed-models entries when they next edit them.
	if role == routing.RoleWorker && len(allowedModels) > 0 {
		found := false
		for _, m := range allowedModels {
			if m == model || m == canonical {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("model %q is not in the allowed worker models for this repo — update with: oat config <repo> worker-models add %s", canonical, canonical)
		}
	}
	return nil
}

// Routing-source labels written into state.Agent.RoutingSource and surfaced
// in the outcome log. Labels are the single source of truth for "how did this
// model get picked?" — the replay analysis filters on them.
const (
	RoutingSourceOperatorExplicit = "operator-explicit"
	RoutingSourceRepoDefault      = "repo-default"
	RoutingSourceRouterAuto       = "router-auto"
	RoutingSourcePassthrough      = "passthrough" // no profiles loaded, took whatever was asked
	RoutingSourceUnknown          = "unknown"
)

// resolveAndValidateModel resolves the model for an agent and validates it against
// loaded profiles. Thin wrapper around resolveAndValidateModelWithSource that
// discards the source label — kept for existing call sites that don't track it.
func (d *Daemon) resolveAndValidateModel(explicitModel string, repoModel string, agentType state.AgentType, allowedModels []string) (string, error) {
	model, _, err := d.resolveAndValidateModelWithSource(explicitModel, repoModel, agentType, allowedModels)
	return model, err
}

// resolveAndValidateModelWithSource does the same job as resolveAndValidateModel
// and additionally returns a RoutingSource* label describing the decision path.
// Callers that persist routing_source (agent-create, supervisor-spawn) should
// use this variant; the replay analysis keys on the source label to separate
// operator decisions from router auto-selects.
func (d *Daemon) resolveAndValidateModelWithSource(explicitModel string, repoModel string, agentType state.AgentType, allowedModels []string) (string, string, error) {
	// If no profiles loaded, fall back to old behavior
	if d.modelProfiles == nil || d.modelProfiles.Count() == 0 {
		if explicitModel != "" {
			d.logger.Info("Model routing: no profiles loaded, passthrough explicit=%s", explicitModel)
			return explicitModel, RoutingSourcePassthrough, nil
		}
		if repoModel != "" {
			d.logger.Info("Model routing: no profiles loaded, fallback to repo=%s", repoModel)
		}
		return repoModel, RoutingSourcePassthrough, nil
	}

	role := routing.RoleWorker
	if agentType == state.AgentTypeSupervisor || agentType == state.AgentTypeWorkspace ||
		agentType == state.AgentTypeMergeQueue || agentType == state.AgentTypePRShepherd {
		role = routing.RoleOrchestrator
	}

	// Build allowed set for workers (only enforced for worker role)
	isWorker := role == routing.RoleWorker
	var allowSet map[string]bool
	if isWorker && len(allowedModels) > 0 {
		allowSet = make(map[string]bool, len(allowedModels))
		for _, m := range allowedModels {
			allowSet[m] = true
		}
	}

	// If a model was explicitly specified, validate it (accepting prefixed or
	// unprefixed). The canonical form is returned so downstream code and
	// persisted state converge on the prefixed shape.
	if explicitModel != "" {
		canonical, err := d.modelProfiles.ValidateAndCanonicalize(explicitModel, role)
		if err != nil {
			d.logger.Warn("Model routing: rejected %s for %s: %v", explicitModel, role, err)
			return "", "", err
		}
		if allowSet != nil && !allowSet[explicitModel] && !allowSet[canonical] {
			d.logger.Warn("Model routing: %s not in allowed worker models for this repo", canonical)
			return "", "", fmt.Errorf("model %q is not in the allowed worker models for this repo — update with: oat config <repo> worker-models add %s", canonical, canonical)
		}
		if canonical != explicitModel {
			d.logger.Info("Model routing: normalized %q -> %q for %s", explicitModel, canonical, role)
		} else {
			d.logger.Info("Model routing: validated %s for %s", canonical, role)
		}
		return canonical, RoutingSourceOperatorExplicit, nil
	}

	// No explicit model — try auto-select from allowed subset (or all eligible)
	if isWorker && len(allowedModels) > 0 {
		eligible := d.modelProfiles.EligibleFiltered(role, allowedModels)
		if len(eligible) == 0 {
			// Fallback to repo default with warning
			if repoModel != "" {
				d.logger.Warn("Model routing: no allowed worker models are eligible — falling back to repo default %s", repoModel)
				return repoModel, RoutingSourceRepoDefault, nil
			}
			return "", RoutingSourceUnknown, nil
		}
		// Pick best from allowed subset, preferring repoModel if it's in the set
		var best *routing.ModelProfile
		if repoModel != "" && allowSet[repoModel] {
			if p := d.modelProfiles.Get(repoModel); p != nil && p.IsEligible(role) {
				return repoModel, RoutingSourceRepoDefault, nil
			}
		}
		for _, p := range eligible {
			if best == nil || p.OverallScore > best.OverallScore {
				best = p
			}
		}
		d.logger.Info("Model routing: auto-selected %s for %s from allowed set (preferred=%s)", best.ModelID, role, repoModel)
		return best.ModelID, RoutingSourceRouterAuto, nil
	}

	// No allowed-list restriction — use global pool
	best, err := d.modelProfiles.BestEligible(role, repoModel)
	if err != nil {
		if repoModel != "" {
			d.logger.Info("Model routing: no eligible profiles, fallback to repo=%s", repoModel)
			return repoModel, RoutingSourceRepoDefault, nil
		}
		return "", RoutingSourceUnknown, nil
	}
	d.logger.Info("Model routing: auto-selected %s for %s (preferred=%s)", best, role, repoModel)
	return best, RoutingSourceRouterAuto, nil
}

// handleReloadModelProfiles reloads model profiles from disk.
// Called after oat model onboard to pick up new profiles.
func (d *Daemon) handleReloadModelProfiles(req socket.Request) socket.Response {
	if d.modelProfiles == nil {
		profiles, err := routing.NewProfileStore(d.paths.ModelProfilesDir)
		if err != nil {
			return socket.ErrorResponse("failed to load model profiles: %v", err)
		}
		d.modelProfiles = profiles
	} else {
		if err := d.modelProfiles.Reload(); err != nil {
			return socket.ErrorResponse("failed to reload model profiles: %v", err)
		}
	}
	count := d.modelProfiles.Count()
	d.logger.Info("Reloaded model profiles: %d profiles available", count)
	return socket.SuccessResponse(map[string]interface{}{
		"count": count,
	})
}

func isProcessAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	// Send signal 0 to check if process exists (doesn't actually signal, just checks)
	err = process.Signal(syscall.Signal(0))
	return err == nil
}

// isAgentProcess checks whether the given PID corresponds to the agent process (not the shell).
// Uses a background context because this is a fast (<10ms) ps check with no
// cancellation benefit; adding a ctx parameter would break every caller.
// Uses the full command line (args) so we don't rely on comm, which is truncated to 16 chars on macOS.
func isAgentProcess(pid int) bool {
	if pid <= 0 {
		return false
	}
	// Use args= (full command line) so we match "oat-agent" even when binary path is long or comm is truncated.
	// Background ctx: fast ps check, no cancellation benefit.
	cmd := exec.CommandContext(context.Background(), "ps", "-p", strconv.Itoa(pid), "-o", "args=") //nolint:contextcheck // see comment above
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	cmdLine := strings.ToLower(strings.TrimSpace(string(output)))
	return strings.Contains(cmdLine, "oat-agent")
}

// appendToSliceMap appends a value to a slice in a map, initializing the slice if needed.
func appendToSliceMap(m map[string][]string, key, value string) {
	if m[key] == nil {
		m[key] = []string{}
	}
	m[key] = append(m[key], value)
}

// buildAgentEnvPrefix returns a shell prefix so core agents inherit user environment (Bug 2).
// Sources ~/.zshrc or ~/.bashrc, then exports variables from ~/.oat/.env and
// ~/.oat/repos/<repo>/.env (repo overrides global). Never logs secret values.
// Sets TERM_PROGRAM so the agent accepts input inside the backend session.
func buildAgentEnvPrefix(paths *config.Paths, repoName string) string {
	// Unset CLAUDECODE so the agent CLI doesn't refuse to start as "nested session"
	prefix := "unset CLAUDECODE 2>/dev/null; "
	prefix += "source ~/.zshrc 2>/dev/null || source ~/.bashrc 2>/dev/null; "
	// Prevent git from opening an interactive editor (e.g. vi) during rebase,
	// which would permanently block the agent process.
	prefix += "export GIT_EDITOR=true; export GIT_SEQUENCE_EDITOR=true; "
	// Prepend ~/.oat/bin/ to PATH (with guard to avoid accumulation on restarts).
	prefix += pathPrependGuard(paths.BinDir)
	// Ensure the currently running oat binary is available to agent subprocesses.
	// This keeps supervisor/agents able to run "oat ..." even if user PATH doesn't
	// include the repo-local build directory.
	if exePath, err := os.Executable(); err == nil && exePath != "" {
		exeDir := filepath.Dir(exePath)
		prefix += pathPrependGuard(exeDir)
	}
	// Load optional .env files (global then per-repo); repo overrides
	envVars := loadEnvFiles(paths.Root, paths.RepoDir(repoName))
	if len(envVars) > 0 {
		for k, v := range envVars {
			prefix += "export " + k + "=" + quoteShellValue(v) + "; "
		}
	}
	// Agent may not process input correctly depending on TERM_PROGRAM; override to ensure normal terminal behavior (user can set in .env)
	if envVars["TERM_PROGRAM"] == "" {
		prefix += "export TERM_PROGRAM=terminal; "
	}
	return prefix
}

// loadEnvFiles reads KEY=value from globalEnvPath and repoEnvPath (.env files).
// Repo keys override global. Returns a map; keys are not logged. Empty/invalid lines and comments are skipped.
func loadEnvFiles(globalRoot, repoDir string) map[string]string {
	out := make(map[string]string)
	for _, path := range []string{filepath.Join(globalRoot, ".env"), filepath.Join(repoDir, ".env")} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			idx := strings.Index(line, "=")
			if idx <= 0 {
				continue
			}
			key := strings.TrimSpace(line[:idx])
			// Strip optional "export " prefix so both "KEY=val" and "export KEY=val" work
			key = strings.TrimPrefix(key, "export ")
			value := strings.TrimSpace(line[idx+1:])
			// Remove surrounding quotes if present
			if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
				value = value[1 : len(value)-1]
			}
			out[key] = value
		}
	}
	return out
}

// quoteShellValue returns a single-quoted shell-safe representation of v (no logging of v).
func quoteShellValue(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "'\\''") + "'"
}

// pathPrependGuard returns a shell snippet that prepends dir to PATH only if
// it's not already present. Prevents PATH from growing on agent restarts.
func pathPrependGuard(dir string) string {
	q := quoteShellValue(dir)
	return fmt.Sprintf(`case ":$PATH:" in *":%s:"*) ;; *) export PATH=%s:"$PATH" ;; esac; `, dir, q)
}

// Run runs the daemon in the foreground
func Run() error {
	paths, err := config.DefaultPaths()
	if err != nil {
		return fmt.Errorf("failed to get paths: %w", err)
	}

	d, err := New(paths)
	if err != nil {
		return fmt.Errorf("failed to create daemon: %w", err)
	}

	if err := d.Start(); err != nil {
		return fmt.Errorf("failed to start daemon: %w", err)
	}

	// Wait for shutdown
	d.Wait()

	return nil
}

// RunDetached starts the daemon in detached mode
func RunDetached() error {
	paths, err := config.DefaultPaths()
	if err != nil {
		return fmt.Errorf("failed to get paths: %w", err)
	}

	// Check if already running
	pidFile := NewPIDFile(paths.DaemonPID)
	if running, pid, _ := pidFile.IsRunning(); running {
		return fmt.Errorf("daemon already running (PID: %d)", pid)
	}

	// Ensure config directory exists
	if err := os.MkdirAll(paths.Root, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Create log file for output
	logFile, err := os.OpenFile(paths.DaemonLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}

	// Prepare daemon command
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}

	// Fork and daemonize
	attr := &os.ProcAttr{
		Dir: filepath.Dir(paths.Root),
		Env: os.Environ(),
		Files: []*os.File{
			nil,     // stdin
			logFile, // stdout
			logFile, // stderr
		},
		Sys: nil,
	}

	// Start daemon process
	process, err := os.StartProcess(executable, []string{executable, "daemon", "_run"}, attr)
	if err != nil {
		return fmt.Errorf("failed to start daemon process: %w", err)
	}

	// Detach from parent
	if err := process.Release(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to release process: %v\n", err)
	}

	// Poll to verify the daemon actually came up. The child writes a PID file
	// and opens the socket within ~1s on success; if it panics during startup
	// (e.g. state restoration), the PID file disappears and the socket never
	// opens. Without this check the user sees "Daemon started" even if the
	// child crashed immediately.
	sockPath := paths.DaemonSock
	for i := 0; i < 25; i++ {
		time.Sleep(200 * time.Millisecond)

		pidFile := NewPIDFile(paths.DaemonPID)
		running, _, _ := pidFile.IsRunning()
		if !running {
			return fmt.Errorf("daemon crashed during startup — check %s for details", paths.DaemonLog)
		}

		if _, err := os.Stat(sockPath); err == nil {
			fmt.Println("Daemon started successfully")
			return nil
		}
	}

	// Timed out waiting for socket but process is still alive
	fmt.Printf("Daemon started but slow to respond — if issues persist, check %s\n", paths.DaemonLog)
	return nil
}

// MaxLogFileSize is the threshold for log rotation (10MB)
const MaxLogFileSize = 10 * 1024 * 1024

// rotateLogsIfNeeded checks log files and rotates any that exceed MaxLogFileSize
func (d *Daemon) rotateLogsIfNeeded() {
	d.logger.Debug("Checking for log rotation")

	err := filepath.Walk(d.paths.OutputDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil //nolint:nilerr // per-entry walk errors are non-fatal
		}
		if info.IsDir() {
			return nil
		}
		if !isLogFile(path) {
			return nil
		}

		if info.Size() > MaxLogFileSize {
			if err := d.rotateLog(path); err != nil {
				d.logger.Error("Failed to rotate log %s: %v", path, err)
			} else {
				d.logger.Info("Rotated log %s (was %d bytes)", path, info.Size())
			}
		}
		return nil
	})

	if err != nil {
		d.logger.Error("Failed to walk output directory for log rotation: %v", err)
	}
}

// rotateLog rotates a single log file by renaming it with a timestamp suffix
func (d *Daemon) rotateLog(logPath string) error {
	timestamp := time.Now().Format("20060102-150405")
	rotatedPath := logPath + "." + timestamp

	// Copy-then-truncate: the backend may hold the fd open, so rename would
	// leave the "rotated" file as the live file (same inode). Instead, copy
	// the content to an archive and truncate the original in place.
	src, err := os.Open(logPath)
	if err != nil {
		return fmt.Errorf("failed to open log for rotation: %w", err)
	}
	dst, err := os.Create(rotatedPath)
	if err != nil {
		src.Close()
		return fmt.Errorf("failed to create rotated log: %w", err)
	}
	_, copyErr := io.Copy(dst, src)
	src.Close()
	closeErr := dst.Close()
	if copyErr != nil {
		return fmt.Errorf("failed to copy log data: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("failed to close rotated log: %w", closeErr)
	}

	// Truncate the original file so the backend keeps writing to it at offset 0
	return os.Truncate(logPath, 0)
}

// isLogFile checks if a file is a log file
func isLogFile(path string) bool {
	base := filepath.Base(path)
	// Only match .log files, not already-rotated files (which have timestamps)
	return len(base) > 4 && base[len(base)-4:] == ".log"
}

// RemovalReason* values for OutcomeRecord.RemovalReason. Used only when
// outcome="removed" to disambiguate the cause. Empty string is the default
// for the "completed" outcome.
const (
	RemovalReasonFailed         = "failed"          // worker self-reported failure or supervisor judged it failed
	RemovalReasonSuperseded     = "superseded"      // re-route replaced this worker before it finished
	RemovalReasonManual         = "manual"          // operator ran `oat agent remove` directly
	RemovalReasonTimeout        = "timeout"         // worker exceeded a wall-clock budget
	RemovalReasonBudgetExceeded = "budget_exceeded" // spend cap tripped (--max-spend / --max-tokens)
	RemovalReasonDaemonRestart  = "daemon_restart"  // daemon shutdown / cleanup, not a per-agent decision
)

// logOutcome appends a routing-history record for the given agent. Called at
// completion (outcome="completed") and at kill/remove (outcome="removed"). Never
// blocks the caller — all I/O errors route through the logger's warn callback.
//
// removalReason is empty for outcome="completed". For outcome="removed" it
// disambiguates the cause; pass one of the RemovalReason* constants. An
// empty string with outcome="removed" is logged as-is; downstream consumers
// treat that as "removed for unrecorded reason" and weight accordingly.
func (d *Daemon) logOutcome(repoName, agentName string, agent state.Agent, outcome, removalReason string) {
	if d.outcomeLogger == nil {
		return
	}

	completed := time.Now().UTC()
	started := agent.CreatedAt
	if started.IsZero() {
		started = completed
	}
	wallMs := completed.Sub(started).Milliseconds()
	if wallMs < 0 {
		wallMs = 0
	}

	routingSource := agent.RoutingSource
	if routingSource == "" {
		routingSource = RoutingSourceUnknown // legacy agents created before the field was added
	}

	// Schema v2 enrichments — all additive, all best-effort. None of these
	// mutate routing decisions; they only enrich the log line so Phase 2/3/4
	// have richer signal. Failures here fall back to zero values, never
	// blocking the completion path.
	canonical, provider := routing.Canonicalize(agent.Model)
	taskFeatures := routing.ExtractLoggedTaskFeatures(agent.Task, agent.WorktreePath)

	// VerifyPassed is the daemon's own verdict — strongest immediate success
	// signal. Tristate: nil if verification didn't run for this agent, else
	// pointer-to-bool reflecting the verdict. Decoupled from outcome (which
	// is "completed" even when verification failed downstream).
	var verifyPassed *bool
	switch agent.VerificationStatus {
	case "approved":
		v := true
		verifyPassed = &v
	case "rejected":
		v := false
		verifyPassed = &v
	}

	// Prompt metadata. Built here from agent fields (captured at spawn) so
	// the daemon can reconstruct the prompt surface without re-reading the
	// prompt file. Tool-defs / skills come from the sidecar protocol later;
	// they're zero here today.
	var promptMeta *routing.PromptMetadata
	if agent.PromptSystemHash != "" || agent.PromptUserHash != "" {
		promptMeta = &routing.PromptMetadata{
			SystemPromptHash:   agent.PromptSystemHash,
			SystemPromptTokens: agent.PromptSystemTokens,
			UserMessageHash:    agent.PromptUserHash,
			UserMessageTokens:  agent.PromptUserTokens,
		}
	}

	rec := routing.OutcomeRecord{
		RecordID:             routing.NewRecordID(),
		OATVersion:           d.oatVersion,
		PricingSnapshotID:    d.pricingSnapshotID,
		TS:                   completed.Format(time.RFC3339),
		Repo:                 repoName,
		Worker:               agentName,
		AgentType:            string(agent.Type),
		TaskText:             agent.Task,
		IssueNum:             agent.IssueNumber,
		Model:                agent.Model,
		Provider:             provider,
		ModelCanonical:       canonical,
		RoutingSource:        routingSource,
		DecisionReason:       agent.RoutingDecisionReason,
		CandidatesConsidered: agent.RoutingCandidates,
		Allowlist:            agent.RoutingAllowlist,
		StartedAt:            started.UTC().Format(time.RFC3339),
		CompletedAt:          completed.Format(time.RFC3339),
		WallMs:               wallMs,
		TokensIn:             agent.InputTokens,
		TokensOut:            agent.OutputTokens,
		CacheRead:            agent.CacheReadTokens,
		CacheWrite:           agent.CacheCreationTokens,
		Outcome:              outcome,
		RemovalReason:        removalReason,
		PRNumber:             agent.PRNumber,
		Summary:              agent.Summary,
		FailureReason:        agent.FailureReason,
		VerifyPassed:         verifyPassed,
		TaskFeatures:         taskFeatures,
		Prompt:               promptMeta,
		// EscalationCount stays 0 here; cascade escalation in Phase 2.5 will
		// set it based on agent.EscalatedFrom chain depth.
	}

	d.outcomeLogger.Log(rec)
}
