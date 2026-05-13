package backend

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"

	"github.com/Root-IO-Labs/open-agent-teams/pkg/sidecar"
)

// ErrAgentAdopted is returned when trying to send input to a re-adopted process
// whose PTY fd was lost during a daemon restart. The caller should restart the agent.
var ErrAgentAdopted = fmt.Errorf("agent was re-adopted without PTY; restart required for input")

// managedProcess tracks a single agent process launched via PTY.
type managedProcess struct {
	cmd              *exec.Cmd
	ptmx             *os.File // PTY master (nil for re-adopted processes)
	pid              int
	logFile          *os.File
	logPath          string
	done             chan struct{}     // closed when process exits
	doneOnce         sync.Once         // ensures done is closed exactly once
	stopMon          chan struct{}     // closed to stop the adopted-process monitor goroutine
	mu               sync.Mutex        // serializes writes to ptmx
	stopping         atomic.Bool       // set when StopAgent begins; prevents new writes
	adopted          bool              // true if re-adopted after daemon restart (no PTY)
	startedAt        time.Time         // when the process was originally started (persisted for PID reuse guard)
	broadcaster      *rawBroadcaster   // live ANSI-stripped output stream (nil for adopted processes)
	sidecarServer    *sidecar.Server   // optional structured-event socket; nil when OAT_USE_SIDECAR is off
	eventBroadcaster *eventBroadcaster // per-agent sidecar.Event pub/sub — always non-nil, fires only when sidecar is on
}

// closeDone safely closes the done channel exactly once.
func (p *managedProcess) closeDone() {
	p.doneOnce.Do(func() { close(p.done) })
}

// persistedAgent holds the on-disk metadata for one agent process.
type persistedAgent struct {
	PID       int    `json:"pid"`
	LogPath   string `json:"log_path"`
	StartedAt string `json:"started_at"`
}

// persistedState is the on-disk format of backend-sessions.json.
type persistedState struct {
	Sessions map[string]map[string]*persistedAgent `json:"sessions"`
}

// DirectBackend implements ProcessBackend using creack/pty.
// Sessions are logical groupings tracked in memory. Agents run as child processes
// with a real PTY so interactive CLI tools (like oat-agent) work correctly.
//
// Session metadata is persisted to dataDir/backend-sessions.json so that
// still-running agent processes can be re-adopted after a daemon restart.
type DirectBackend struct {
	mu       sync.RWMutex
	sessions map[string]map[string]*managedProcess // session → agent → process
	dataDir  string                                // directory for backend-sessions.json; empty disables persistence
}

// NewDirectBackend creates a new DirectBackend without persistence.
func NewDirectBackend() *DirectBackend {
	return &DirectBackend{
		sessions: make(map[string]map[string]*managedProcess),
	}
}

// NewDirectBackendWithDataDir creates a DirectBackend that persists session
// metadata to dataDir/backend-sessions.json. On construction it re-adopts
// any agent processes that are still alive from a previous daemon instance.
func NewDirectBackendWithDataDir(dataDir string) *DirectBackend {
	b := &DirectBackend{
		sessions: make(map[string]map[string]*managedProcess),
		dataDir:  dataDir,
	}
	b.loadPersistedSessions()
	return b
}

// Name returns "direct".
func (b *DirectBackend) Name() string { return "direct" }

// Available returns true — direct PTY is always available on Unix.
func (b *DirectBackend) Available() bool { return true }

// CreateSession creates a logical session (just a map entry).
func (b *DirectBackend) CreateSession(ctx context.Context, name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.sessions[name]; exists {
		return nil // idempotent
	}
	b.sessions[name] = make(map[string]*managedProcess)
	return nil
}

// DestroySession stops all agents in the session and removes it.
func (b *DirectBackend) DestroySession(ctx context.Context, name string) error {
	b.mu.Lock()
	agents, exists := b.sessions[name]
	if !exists {
		b.mu.Unlock()
		return nil
	}
	// Copy agent names to avoid holding lock during stop
	names := make([]string, 0, len(agents))
	for agentName := range agents {
		names = append(names, agentName)
	}
	b.mu.Unlock()

	for _, agentName := range names {
		_ = b.StopAgent(ctx, name, agentName)
	}

	b.mu.Lock()
	delete(b.sessions, name)
	if err := b.persistSessions(); err != nil {
		fmt.Fprintf(os.Stderr, "backend: %v\n", err)
	}
	b.mu.Unlock()
	return nil
}

// HasSession checks if a session exists.
func (b *DirectBackend) HasSession(ctx context.Context, name string) (bool, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, exists := b.sessions[name]
	return exists, nil
}

// ListSessions returns all session names.
func (b *DirectBackend) ListSessions(ctx context.Context) ([]string, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	sessions := make([]string, 0, len(b.sessions))
	for name := range b.sessions {
		sessions = append(sessions, name)
	}
	return sessions, nil
}

// ensureOatGitignore writes a .gitignore inside the .oat/ directory to prevent
// OAT runtime files (AGENTS.md, settings.json) from being committed by workers.
// Appends missing entries if the file already exists with custom content.
func ensureOatGitignore(oatDir string) {
	requiredEntries := []string{"AGENTS.md", "settings.json", "mcp.json"}
	gitignorePath := filepath.Join(oatDir, ".gitignore")

	existing, _ := os.ReadFile(gitignorePath)
	content := string(existing)

	var missing []string
	for _, entry := range requiredEntries {
		if !strings.Contains(content, entry) {
			missing = append(missing, entry)
		}
	}
	if len(missing) == 0 {
		return
	}

	var buf strings.Builder
	if len(existing) > 0 {
		buf.WriteString(content)
		if !strings.HasSuffix(content, "\n") {
			buf.WriteByte('\n')
		}
	} else {
		buf.WriteString("# Auto-generated by OAT -- runtime files, not project source\n")
	}
	for _, entry := range missing {
		buf.WriteString(entry)
		buf.WriteByte('\n')
	}
	_ = os.WriteFile(gitignorePath, []byte(buf.String()), 0o644)
}

// StartAgent launches an agent process with a real PTY.
func (b *DirectBackend) StartAgent(ctx context.Context, cfg AgentConfig) (*AgentHandle, error) {
	b.mu.Lock()
	if _, exists := b.sessions[cfg.SessionName]; !exists {
		b.sessions[cfg.SessionName] = make(map[string]*managedProcess)
	}
	b.mu.Unlock()

	// Write AGENTS.md if InitialPrompt is set — this is the agent's system
	// prompt, so failure means the agent launches without instructions.
	if cfg.InitialPrompt != "" {
		agentsDir := filepath.Join(cfg.WorkDir, ".oat")
		if err := os.MkdirAll(agentsDir, 0o755); err != nil {
			return nil, fmt.Errorf("failed to create .oat dir for agent prompt: %w", err)
		}
		if err := os.WriteFile(filepath.Join(agentsDir, "AGENTS.md"), []byte(cfg.InitialPrompt), 0o644); err != nil {
			return nil, fmt.Errorf("failed to write agent prompt (AGENTS.md): %w", err)
		}
		ensureOatGitignore(agentsDir)
	}

	// Write .oat/mcp.json if MCPConfig is set. Mirrors the AGENTS.md path
	// above: the file is the daemon's contract with the agent-runtime;
	// failure means the agent launches without its declared MCP tools,
	// so we surface the write error rather than continuing silently.
	if cfg.MCPConfig != "" {
		mcpDir := filepath.Join(cfg.WorkDir, ".oat")
		if err := os.MkdirAll(mcpDir, 0o755); err != nil {
			return nil, fmt.Errorf("failed to create .oat dir for MCP config: %w", err)
		}
		if err := os.WriteFile(filepath.Join(mcpDir, "mcp.json"), []byte(cfg.MCPConfig), 0o644); err != nil {
			return nil, fmt.Errorf("failed to write MCP config (.oat/mcp.json): %w", err)
		}
		ensureOatGitignore(mcpDir)
	}

	// Build the command string for bash -l -c
	shellCmd := ""
	if cfg.EnvPrefix != "" {
		shellCmd += cfg.EnvPrefix
	}
	if cfg.WorkDir != "" {
		shellCmd += fmt.Sprintf("cd %q && ", cfg.WorkDir)
	}
	shellCmd += shellQuote(cfg.BinaryPath)
	for _, arg := range cfg.Args {
		shellCmd += " " + shellQuote(arg)
	}

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "bash"
	}
	cmd := exec.CommandContext(ctx, shell, "-l", "-c", shellCmd)
	cmd.Dir = cfg.WorkDir

	// Set up environment
	cmd.Env = append(os.Environ(), cfg.Env...)

	// Allocate the event broadcaster unconditionally — it's cheap (a
	// struct + ~256-slot ring) and having it always present simplifies
	// the SubscribeEvents API (no "broadcaster might be nil" paths for
	// TUI consumers). When the sidecar is off, Publish is never called.
	eventBc := newEventBroadcaster()

	// Start the sidecar server BEFORE spawning the agent process so the
	// socket exists when Python's sidecar_emitter tries to connect on
	// first emit. The SidecarClient has its own connect-retry backoff
	// (~3s total), so small race windows don't cause drops — but binding
	// first keeps first-emit latency minimal.
	//
	// When SidecarPath is empty (feature flag off / default), nothing is
	// started and the env var is not injected; the agent runs exactly as
	// it does today. Failure to bind is logged but non-fatal — the agent
	// still runs with the stdout [OAT_TOKENS] path as the sole source
	// for token accounting.
	var sidecarSrv *sidecar.Server
	if cfg.SidecarPath != "" {
		sidecarSrv = newSidecarServerForAgent(cfg.SidecarPath, cfg.LogFile, eventBc)
		if err := sidecarSrv.Start(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: sidecar start failed for %s/%s: %v (continuing without sidecar)\n",
				cfg.SessionName, cfg.AgentName, err)
			sidecarSrv = nil
		} else {
			cmd.Env = append(cmd.Env, fmt.Sprintf("OAT_SIDECAR_SOCKET=%s", cfg.SidecarPath))
		}
	}

	// Start with PTY (50 rows x 200 cols to match typical terminal)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 50, Cols: 200})
	if err != nil {
		// Clean up the sidecar socket we just bound — otherwise we leak
		// a /tmp/*.sock file that blocks a future startup on the same path.
		if sidecarSrv != nil {
			_ = sidecarSrv.Close()
		}
		return nil, fmt.Errorf("failed to start agent with PTY: %w", err)
	}
	// Ensure PTY is cleaned up if anything below fails before we store proc
	ptmxClosed := false
	defer func() {
		if !ptmxClosed {
			ptmx.Close()
		}
	}()

	proc := &managedProcess{
		cmd:              cmd,
		ptmx:             ptmx,
		pid:              cmd.Process.Pid,
		logPath:          cfg.LogFile,
		done:             make(chan struct{}),
		startedAt:        time.Now().UTC(),
		broadcaster:      newRawBroadcaster(),
		sidecarServer:    sidecarSrv,
		eventBroadcaster: eventBc,
	}

	// Pre-seed the broadcaster's dedup window with the system prompt content.
	// The agent CLI renders AGENTS.md on its Textual TUI at startup, which the
	// PTY captures. By seeding the prompt text, those lines are immediately
	// recognized as "already seen" and suppressed on the first render.
	if cfg.InitialPrompt != "" {
		proc.broadcaster.SeedPromptContent(cfg.InitialPrompt)
	}

	// Skip cleanLogWriter file capture when OAT_TOOL_LOG is set -- the oat_cli
	// writes a full-content conversation log directly to the same path.
	hasToolLog := false
	for _, env := range cfg.Env {
		if strings.HasPrefix(env, "OAT_TOOL_LOG=") {
			hasToolLog = true
			break
		}
	}

	// Open log file for output capture (PTY → cleanLogWriter → .log)
	if cfg.LogFile != "" && !hasToolLog {
		logDir := filepath.Dir(cfg.LogFile)
		_ = os.MkdirAll(logDir, 0755)
		logFile, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			// Non-fatal: agent runs but output isn't captured
			fmt.Fprintf(os.Stderr, "Warning: failed to open log file %s: %v\n", cfg.LogFile, err)
		} else {
			proc.logFile = logFile
		}
	}

	// Goroutine to tee PTY output to log file AND live broadcaster.
	// cleanLogWriter: ANSI strip + dedup → .log file for CLI (oat attach)
	// rawBroadcaster: ANSI strip only → ring buffer → TUI socket streams
	go func() {
		defer proc.closeDone()
		var writer *cleanLogWriter
		if proc.logFile != nil {
			writer = newCleanLogWriter(proc.logFile)
			// Seed the log writer with prompt content so the initial
			// Textual TUI render of AGENTS.md is suppressed in logs too.
			if cfg.InitialPrompt != "" {
				writer.SeedPromptContent(cfg.InitialPrompt)
			}
		}
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				if writer != nil {
					_, _ = writer.Write(chunk) // log tee; missed writes acceptable
				}
				if proc.broadcaster != nil {
					_, _ = proc.broadcaster.Write(chunk) // fanout; subscribers handle drops
				}
			}
			if err != nil {
				break // EIO on child exit, or ptmx closed
			}
		}
		// Stop periodic flusher and write any remaining partial line
		if writer != nil {
			writer.Close()
		}
		if proc.broadcaster != nil {
			proc.broadcaster.Close()
		}
	}()

	// Wait for process exit in background to clean up
	go func() {
		_ = cmd.Wait()
	}()

	// Store process and persist to disk
	b.mu.Lock()
	b.sessions[cfg.SessionName][cfg.AgentName] = proc
	if err := b.persistSessions(); err != nil {
		fmt.Fprintf(os.Stderr, "backend: %v\n", err)
	}
	b.mu.Unlock()
	ptmxClosed = true // proc now owns the PTY — don't close in defer

	handle := &AgentHandle{
		PID:     proc.pid,
		LogFile: cfg.LogFile,
	}

	return handle, nil
}

// StopAgent terminates a running agent: SIGTERM → wait 5s → SIGKILL.
func (b *DirectBackend) StopAgent(ctx context.Context, session, agentName string) error {
	b.mu.Lock()
	agents, ok := b.sessions[session]
	if !ok {
		b.mu.Unlock()
		return fmt.Errorf("session %q not found", session)
	}
	proc, ok := agents[agentName]
	if !ok {
		b.mu.Unlock()
		return fmt.Errorf("agent %q not found in session %q", agentName, session)
	}
	// Mark as stopping before removing from map so concurrent callers
	// that already hold a proc pointer see the flag and bail out.
	proc.stopping.Store(true)
	delete(agents, agentName)
	if err := b.persistSessions(); err != nil {
		fmt.Fprintf(os.Stderr, "backend: %v\n", err)
	}
	b.mu.Unlock()

	return b.killProcess(proc)
}

// killProcess handles the SIGTERM → wait → SIGKILL sequence.
func (b *DirectBackend) killProcess(proc *managedProcess) error {
	if proc.adopted {
		// Adopted process: no cmd or PTY, use raw signals
		_ = syscall.Kill(proc.pid, syscall.SIGTERM)
		select {
		case <-proc.done:
		case <-time.After(5 * time.Second):
			_ = syscall.Kill(proc.pid, syscall.SIGKILL)
			// Wait briefly for the monitor goroutine to notice
			select {
			case <-proc.done:
			case <-time.After(2 * time.Second):
			}
		}
		// Stop the monitor goroutine to prevent leaks
		if proc.stopMon != nil {
			close(proc.stopMon)
		}
		return nil
	}

	if proc.cmd == nil || proc.cmd.Process == nil {
		return nil
	}

	// Send SIGTERM
	_ = proc.cmd.Process.Signal(syscall.SIGTERM)

	// Wait up to 5 seconds for graceful exit
	select {
	case <-proc.done:
		// Process exited gracefully
	case <-time.After(5 * time.Second):
		// Force kill
		_ = proc.cmd.Process.Signal(syscall.SIGKILL)
		<-proc.done
	}

	// Clean up — acquire proc.mu so concurrent SendMessage/SendInterrupt
	// callers see the nil ptmx and bail out instead of writing to a closed fd.
	proc.mu.Lock()
	if proc.ptmx != nil {
		proc.ptmx.Close()
		proc.ptmx = nil
	}
	proc.mu.Unlock()

	if proc.logFile != nil {
		_ = proc.logFile.Sync() // fsync best-effort; Close below still flushes buffered data
		proc.logFile.Close()
	}

	// Sidecar server runs independently of the PTY; close it after the
	// process has exited so any final events in-flight are delivered.
	// Close is idempotent and removes the socket file from /tmp.
	if proc.sidecarServer != nil {
		_ = proc.sidecarServer.Close()
		proc.sidecarServer = nil
	}

	// Close the event broadcaster last so any final events delivered by
	// the sidecar server land before subscribers see the channel close.
	if proc.eventBroadcaster != nil {
		proc.eventBroadcaster.Close()
	}

	return nil
}

// SubscribeEvents returns a live channel of sidecar events for the given
// agent, along with any ring-buffered events the new subscriber missed.
// Implements the optional SidecarSubscriber interface so the daemon's
// stream_events command can deliver structured events to the TUI.
//
// When the agent has no sidecar (OAT_USE_SIDECAR unset), the broadcaster
// still exists but Publish is never called, so the returned channel
// simply never fires and catchup is nil — the protocol is stable in
// both states.
func (b *DirectBackend) SubscribeEvents(
	ctx context.Context, session, agentName string,
) (uint64, <-chan sidecar.Event, []sidecar.Event, func(), error) {
	b.mu.RLock()
	agents, ok := b.sessions[session]
	if !ok {
		b.mu.RUnlock()
		return 0, nil, nil, nil, fmt.Errorf("session %q not found", session)
	}
	proc, ok := agents[agentName]
	if !ok {
		b.mu.RUnlock()
		return 0, nil, nil, nil, fmt.Errorf("agent %q not found in session %q", agentName, session)
	}
	bc := proc.eventBroadcaster
	b.mu.RUnlock()

	if bc == nil {
		// Defensive: re-adopted processes skip broadcaster construction.
		// Return a harmless closed channel so the caller's range loop
		// exits immediately rather than hanging.
		ch := make(chan sidecar.Event)
		close(ch)
		return 0, ch, nil, func() {}, nil
	}
	id, ch, catchup, cancel := bc.Subscribe()
	return id, ch, catchup, cancel, nil
}

// IsAgentAlive checks if the agent process is still running.
func (b *DirectBackend) IsAgentAlive(ctx context.Context, session, agentName string) (bool, error) {
	b.mu.RLock()
	agents, ok := b.sessions[session]
	if !ok {
		b.mu.RUnlock()
		return false, nil
	}
	proc, ok := agents[agentName]
	b.mu.RUnlock()
	if !ok {
		return false, nil
	}

	// Check if process is still alive via kill(pid, 0)
	err := syscall.Kill(proc.pid, 0)
	return err == nil, nil
}

// SendMessage sends text + Enter to the agent's PTY.
// Acquires per-agent lock to prevent concurrent writes from interleaving
// and to prevent writing to a PTY that is being closed by StopAgent.
// Returns ErrAgentAdopted if the agent was re-adopted without a PTY.
func (b *DirectBackend) SendMessage(ctx context.Context, session, agentName, message string) error {
	proc, err := b.getProcess(session, agentName)
	if err != nil {
		return err
	}

	if proc.adopted {
		return ErrAgentAdopted
	}

	// Early exit if the process already exited — avoids writing to a dead PTY
	// (which would succeed at the fd level but return EIO).
	select {
	case <-proc.done:
		return fmt.Errorf("agent %s/%s has already exited", session, agentName)
	default:
	}

	// Acquire per-agent lock BEFORE checking stopping flag and writing.
	// This ensures killProcess cannot close the PTY between our check and write.
	proc.mu.Lock()
	defer proc.mu.Unlock()

	if proc.stopping.Load() {
		return fmt.Errorf("agent %s/%s is stopping", session, agentName)
	}

	if proc.ptmx == nil {
		return fmt.Errorf("agent %s/%s has no PTY", session, agentName)
	}

	// Write all bytes, retrying on short writes (PTY buffer may be full).
	// Use carriage return for Enter so terminal UIs treat it as submit.
	data := []byte(message + "\r")
	for len(data) > 0 {
		n, err := syscall.Write(int(proc.ptmx.Fd()), data)
		if err != nil {
			return fmt.Errorf("failed to write to agent PTY: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("pty write returned 0 bytes")
		}
		data = data[n:]
	}
	return nil
}

// SendInterrupt sends Ctrl-C (0x03) to the agent's PTY.
// For adopted processes without a PTY, falls back to sending SIGINT.
func (b *DirectBackend) SendInterrupt(ctx context.Context, session, agentName string) error {
	proc, err := b.getProcess(session, agentName)
	if err != nil {
		return err
	}

	if proc.adopted {
		if proc.stopping.Load() {
			return fmt.Errorf("agent %s/%s is stopping", session, agentName)
		}
		// No PTY, but we can still send SIGINT via kill()
		return syscall.Kill(proc.pid, syscall.SIGINT)
	}

	// Early exit if the process already exited.
	select {
	case <-proc.done:
		return fmt.Errorf("agent %s/%s has already exited", session, agentName)
	default:
	}

	// Acquire per-agent lock BEFORE checking stopping flag and writing.
	// This ensures killProcess cannot close the PTY between our check and write.
	proc.mu.Lock()
	defer proc.mu.Unlock()

	if proc.stopping.Load() {
		return fmt.Errorf("agent %s/%s is stopping", session, agentName)
	}

	if proc.ptmx == nil {
		return fmt.Errorf("agent %s/%s has no PTY", session, agentName)
	}

	_, err = syscall.Write(int(proc.ptmx.Fd()), []byte{0x03})
	if err != nil {
		return fmt.Errorf("failed to send interrupt to agent PTY: %w", err)
	}
	return nil
}

// SendEscape sends Escape (0x1b) to the agent's PTY.
// Cancels "Thinking..." state without killing the process.
func (b *DirectBackend) SendEscape(ctx context.Context, session, agentName string) error {
	proc, err := b.getProcess(session, agentName)
	if err != nil {
		return err
	}

	if proc.stopping.Load() {
		return fmt.Errorf("agent %s/%s is stopping", session, agentName)
	}

	proc.mu.Lock()
	defer proc.mu.Unlock()

	if proc.stopping.Load() {
		return fmt.Errorf("agent %s/%s is stopping", session, agentName)
	}

	_, err = syscall.Write(int(proc.ptmx.Fd()), []byte{0x1b})
	if err != nil {
		return fmt.Errorf("failed to send escape to agent PTY: %w", err)
	}
	return nil
}

// GetOutputReader opens the agent's log file for reading.
func (b *DirectBackend) GetOutputReader(ctx context.Context, session, agentName string) (io.ReadCloser, error) {
	proc, err := b.getProcess(session, agentName)
	if err != nil {
		return nil, err
	}

	if proc.logPath == "" {
		return nil, fmt.Errorf("no log file for agent %s/%s", session, agentName)
	}

	return os.Open(proc.logPath)
}

// Attach connects the current terminal to the agent.
// If stdin is a terminal, provides interactive PTY proxy.
// Otherwise, tails the log file.
func (b *DirectBackend) Attach(ctx context.Context, session, agentName string, readOnly bool) error {
	proc, err := b.getProcess(session, agentName)
	if err != nil {
		// Agent not running — show snapshot of log
		return b.tailLogFile(ctx, session, agentName, false)
	}

	// Check if we're alive
	alive := syscall.Kill(proc.pid, 0) == nil

	if !alive {
		return b.tailLogFile(ctx, session, agentName, false)
	}

	// Adopted processes have no PTY — can only tail the log
	if proc.adopted {
		return b.tailLogFile(ctx, session, agentName, true)
	}

	if readOnly || !term.IsTerminal(int(os.Stdin.Fd())) {
		// Live follow mode — streams new output until agent exits or Ctrl-C
		return b.tailLogFile(ctx, session, agentName, true)
	}

	// Interactive mode: set raw, proxy stdin↔ptmx
	fmt.Printf("Attaching to agent %s (Ctrl-C to detach)...\n", agentName)

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("failed to set terminal raw mode: %w", err)
	}
	defer func() { _ = term.Restore(int(os.Stdin.Fd()), oldState) }() // cleanup on detach; failure here is unavoidable

	// Use a pipe to proxy stdin so we can close it on detach,
	// preventing goroutine leaks.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("failed to create stdin pipe: %w", err)
	}
	defer stdinR.Close()
	defer stdinW.Close()

	// Copy real stdin → pipe writer (stops when stdinW is closed)
	go func() {
		_, _ = io.Copy(stdinW, os.Stdin) // terminates on stdinW close / stdin EOF
	}()

	// Copy ptmx → stdout
	outputDone := make(chan struct{})
	go func() {
		defer close(outputDone)
		_, _ = io.Copy(os.Stdout, proc.ptmx) // terminates on ptmx close
	}()

	// Copy pipe reader → ptmx (stops when stdinR is closed)
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		_, _ = io.Copy(proc.ptmx, stdinR) // terminates on stdinR close
	}()

	// Wait for process to exit, ptmx to close, or context cancellation.
	// Closing stdinW/stdinR in defers ensures the input goroutine exits.
	select {
	case <-proc.done:
		fmt.Println("\nAgent process exited.")
	case <-outputDone:
	case <-ctx.Done():
	}

	return nil
}

// tailLogFile shows the agent's log file output.
// If follow is true, keeps reading new data (like tail -f) until the
// context is canceled or the agent process exits.  If follow is false,
// prints the last 8KB and exits.
func (b *DirectBackend) tailLogFile(ctx context.Context, session, agentName string, follow bool) error {
	// Extract only the values we need under the lock.  The done channel is
	// safe to retain — it's created once at process start and never replaced.
	// logPath is a string copy so it's inherently safe after unlock.
	b.mu.RLock()
	var logPath string
	var doneCh <-chan struct{}
	if agents, ok := b.sessions[session]; ok {
		if p, ok := agents[agentName]; ok {
			logPath = p.logPath
			doneCh = p.done
		}
	}
	b.mu.RUnlock()

	if logPath == "" {
		return fmt.Errorf("no log file found for agent %s/%s", session, agentName)
	}

	f, err := os.Open(logPath)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}
	defer f.Close()

	// Show recent context — seek near the end
	if info, statErr := f.Stat(); statErr == nil && info.Size() > 8192 {
		_, _ = f.Seek(-8192, io.SeekEnd) // best-effort; fall back to full read
		// Skip the first (likely partial) line
		scanner := bufio.NewScanner(f)
		scanner.Scan()
	}

	if !follow {
		// Snapshot mode: read what's there and exit
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			fmt.Println(scanner.Text())
		}
		fmt.Println("\n--- End of log (agent not running) ---")
		return nil
	}

	// Follow mode: read existing content, then poll for new data
	buf := make([]byte, 4096)
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			os.Stdout.Write(buf[:n])
		}
		if readErr != nil {
			// At EOF — check if we should keep following
			select {
			case <-ctx.Done():
				return nil
			default:
			}
			// If the process exited, drain remaining bytes and stop
			if doneCh != nil {
				select {
				case <-doneCh:
					// Drain anything written between last read and exit
					for {
						n2, _ := f.Read(buf)
						if n2 > 0 {
							os.Stdout.Write(buf[:n2])
						} else {
							break
						}
					}
					fmt.Println("\n--- Agent process exited ---")
					return nil
				default:
				}
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
}

// getProcess looks up a managed process.
func (b *DirectBackend) getProcess(session, agentName string) (*managedProcess, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	agents, ok := b.sessions[session]
	if !ok {
		return nil, fmt.Errorf("session %q not found", session)
	}
	proc, ok := agents[agentName]
	if !ok {
		return nil, fmt.Errorf("agent %q not found in session %q", agentName, session)
	}
	return proc, nil
}

// ---------------------------------------------------------------------------
// cleanLogWriter — strips terminal escape sequences from raw PTY output
// and deduplicates lines across a sliding window so TUI redraws don't
// flood the on-disk log file.  Result is human-readable plain text.
//
// Progressive dedup:  LLM streaming causes the TUI to redraw the current
// line with each new token, producing output like:
//
//	"It"
//	"It looks"
//	"It looks like your message..."
//
// The writer buffers non-blank lines as "pending" and only emits them when
// the next line is NOT a progressive extension.  This collapses an entire
// streaming sequence into a single final line.
// ---------------------------------------------------------------------------

const cleanLogRecentBufSize = 200 // ~4 full-screen redraws of context
// cleanLogPrefixMin is intentionally low (1) because the cleanLogWriter only
// compares CONSECUTIVE lines (pending vs the very next line). The risk of
// false positives is minimal — two adjacent lines where one is a single-char
// prefix of the other is almost always streaming. No word boundary check is
// used here either, because character-by-character streaming within a word
// (e.g. "I" → "I'" → "I'm") would fail boundary checks at every transition.
// The TUI-side dedup has stricter rules (min=8 + word boundary) since it
// scans a 30-line lookback window where false positives are more likely.
const cleanLogPrefixMin = 1

// cleanLogWriter implements io.Writer.  It uses ansiStripper (shared state
// machine) to strip ANSI/xterm escape sequences.  Cursor-positioning commands
// (CUP/HVP) are treated as implicit newlines because each one means the TUI
// is moving to a new screen row.  A sliding window of recently-seen lines
// prevents TUI redraws from producing duplicate output.
type cleanLogWriter struct {
	mu         sync.Mutex // serializes Write/Flush/periodicFlush
	w          io.Writer
	stripper   *ansiStripper
	recentSet  map[string]struct{}           // O(1) duplicate lookup
	recentRing [cleanLogRecentBufSize]string // ring buffer for eviction
	recentPos  int
	blankRun   int // consecutive blank lines seen

	// Progressive dedup state — buffer one line so we can detect streaming
	// extensions before committing to the output.
	pendingLine    string // formatted line (original spacing)
	pendingTrimmed string // trimmed version for comparison
	hasPending     bool
	deferredBlanks int // blanks seen while a line is pending

	// Periodic flush: a background goroutine commits pending lines on an interval
	// so they're visible to the TUI promptly and survive crashes.
	// Default: 5s (long enough to avoid splitting mid-stream lines).
	flushInterval time.Duration
	done          chan struct{}

	// Startup suppression (only active after SeedPromptContent is called)
	startupSuppress bool
	createdAt       time.Time
	userInputSeen   bool
}

func newCleanLogWriter(w io.Writer) *cleanLogWriter {
	return newCleanLogWriterWithInterval(w, 5*time.Second)
}

// newCleanLogWriterWithInterval creates a cleanLogWriter with a custom flush interval.
// Tests use a short interval to deterministically trigger periodic flushes.
func newCleanLogWriterWithInterval(w io.Writer, flushInterval time.Duration) *cleanLogWriter {
	c := &cleanLogWriter{
		w:             w,
		recentSet:     make(map[string]struct{}, cleanLogRecentBufSize),
		flushInterval: flushInterval,
		done:          make(chan struct{}),
		createdAt:     time.Now(),
	}
	c.stripper = newAnsiStripper(c.handleStrippedLine)
	go c.periodicFlush()
	return c
}

func (c *cleanLogWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stripper.Write(p)
	return len(p), nil
}

// SeedPromptContent pre-loads the system prompt into the recent-seen set
// so that the initial Textual TUI render of AGENTS.md is suppressed.
func (c *cleanLogWriter) SeedPromptContent(promptText string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.startupSuppress = true
	for _, line := range strings.Split(promptText, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			c.recordRecent(trimmed)
		}
	}
}

// handleStrippedLine is the callback from ansiStripper — delivers one clean
// line at a time into the dedup pipeline.  Called under c.mu.
func (c *cleanLogWriter) handleStrippedLine(line string) {
	c.flushLine(line)
}

// Flush writes any remaining buffered content to the underlying writer.
func (c *cleanLogWriter) Flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stripper.Flush()
	c.commitPending()
}

// Close stops the periodic flush goroutine and flushes remaining content.
func (c *cleanLogWriter) Close() {
	close(c.done)
	c.Flush()
}

// periodicFlush commits pending lines on c.flushInterval so that:
// (1) lines are visible to the TUI during long pauses (tool execution, thinking),
// (2) pending data survives daemon crashes (at most flushInterval of data lost).
//
// The default 5s interval is long enough that the timer almost never fires mid-stream
// (LLM streaming typically completes a line in 2-4 seconds). This prevents
// progressive streaming fragments from being committed to the log file.
func (c *cleanLogWriter) periodicFlush() {
	ticker := time.NewTicker(c.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.mu.Lock()
			c.commitPending()
			c.mu.Unlock()
		case <-c.done:
			return
		}
	}
}

func (c *cleanLogWriter) flushLine(line string) {
	trimmed := strings.TrimSpace(line)

	// Filter agent CLI chrome (sidebar panels, prompt headers).
	if trimmed != "" && isAgentChrome(trimmed) {
		return
	}

	// Startup suppression (only when SeedPromptContent was called):
	// during the first 5 seconds, suppress lines that match the seeded
	// prompt content. New lines (agent output) pass through.
	if trimmed != "" && c.startupSuppress && !c.userInputSeen {
		if strings.HasPrefix(trimmed, "Thinking") || strings.HasPrefix(trimmed, "(*) ") ||
			strings.HasPrefix(trimmed, "⏺ ") || strings.HasPrefix(trimmed, "● ") {
			c.userInputSeen = true
		} else if time.Since(c.createdAt) < 5*time.Second {
			if c.isRecentlySeen(trimmed) {
				return // seeded prompt content — suppress
			}
			// Not seeded — new content, let it through
		} else {
			c.userInputSeen = true
		}
	}

	// Skip exact duplicates from the recent window.
	if trimmed != "" && c.isRecentlySeen(trimmed) {
		return
	}

	// ── Blank line handling ──────────────────────────────────────────────
	if trimmed == "" {
		c.blankRun++
		if c.hasPending {
			// Don't flush the pending line yet — blank lines between
			// progressive streaming chunks are TUI artifacts.
			c.deferredBlanks++
			return
		}
		if c.blankRun > 1 {
			return // collapse consecutive blanks
		}
		fmt.Fprintln(c.w, line)
		return
	}

	// ── Non-blank line ───────────────────────────────────────────────────

	// Progressive dedup: if the new line extends the pending line
	// (LLM streaming), just update pending without writing the old one.
	// No word boundary check here — character-level streaming within words
	// (e.g. "I" → "I'" → "I'm" → "I'm ") is common and valid.
	if c.hasPending && c.pendingTrimmed != "" && len(c.pendingTrimmed) >= cleanLogPrefixMin {
		// Check both raw and markdown-stripped to handle backtick shifting
		// during LLM streaming (e.g., "start `" → "start BotServer").
		if strings.HasPrefix(trimmed, c.pendingTrimmed) || stripInlineMarkdownPrefix(trimmed, c.pendingTrimmed) {
			// New line is a progressive extension → update pending
			c.pendingLine = line
			c.pendingTrimmed = trimmed
			c.deferredBlanks = 0
			return
		}
		if strings.HasPrefix(c.pendingTrimmed, trimmed) || stripInlineMarkdownPrefix(c.pendingTrimmed, trimmed) {
			if len(trimmed) >= cleanLogPrefixMin {
				// New line is a shorter version of pending (cursor re-draw) → skip
				return
			}
		}
	}

	// The new line is unrelated to pending — commit pending first.
	c.commitPending()

	// Buffer the new line as pending (it might be the start of a
	// new progressive streaming sequence).
	c.pendingLine = line
	c.pendingTrimmed = trimmed
	c.hasPending = true
	c.deferredBlanks = 0
	c.blankRun = 0
}

// commitPending writes the buffered pending line (if any) to output.
func (c *cleanLogWriter) commitPending() {
	if !c.hasPending {
		return
	}
	c.recordRecent(c.pendingTrimmed)
	fmt.Fprintln(c.w, c.pendingLine)
	c.hasPending = false
	c.pendingLine = ""
	c.pendingTrimmed = ""
	c.blankRun = 0
	c.deferredBlanks = 0
}

// isRecentlySeen checks the map for O(1) duplicate detection using trimmed content
// so that TUI redraws with slightly different padding are still detected as dupes.
func (c *cleanLogWriter) isRecentlySeen(trimmed string) bool {
	_, ok := c.recentSet[trimmed]
	return ok
}

func (c *cleanLogWriter) recordRecent(trimmed string) {
	// Evict the old entry at this ring position
	idx := c.recentPos % cleanLogRecentBufSize
	if old := c.recentRing[idx]; old != "" {
		delete(c.recentSet, old)
	}
	// Add new entry
	c.recentRing[idx] = trimmed
	c.recentSet[trimmed] = struct{}{}
	c.recentPos++
}

// shellQuote returns a shell-safe representation of s.
// If s contains no special characters, it's returned as-is.
// Otherwise, it's wrapped in single quotes with internal single quotes escaped.
func shellQuote(s string) string {
	// If it looks safe, return as-is
	safe := true
	for _, c := range s {
		isAlnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		isSpecialSafe := c == '-' || c == '_' || c == '.' || c == '/' || c == ':' || c == '=' || c == ','
		if !isAlnum && !isSpecialSafe {
			safe = false
			break
		}
	}
	if safe && len(s) > 0 {
		return s
	}
	// Wrap in single quotes, escaping any internal single quotes
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// ---------------------------------------------------------------------------
// Session persistence — survives daemon restarts
// ---------------------------------------------------------------------------

// sessionsFile returns the path to the persistence file, or "" if disabled.
func (b *DirectBackend) sessionsFile() string {
	if b.dataDir == "" {
		return ""
	}
	return filepath.Join(b.dataDir, "backend-sessions.json")
}

// persistSessions writes the current in-memory session map to disk.
// Caller must hold b.mu (at least RLock).
// Returns an error if persistence fails so callers can log it properly.
func (b *DirectBackend) persistSessions() error {
	path := b.sessionsFile()
	if path == "" {
		return nil
	}

	ps := persistedState{
		Sessions: make(map[string]map[string]*persistedAgent),
	}
	for sessName, agents := range b.sessions {
		ps.Sessions[sessName] = make(map[string]*persistedAgent)
		for agentName, proc := range agents {
			ps.Sessions[sessName][agentName] = &persistedAgent{
				PID:       proc.pid,
				LogPath:   proc.logPath,
				StartedAt: proc.startedAt.Format(time.RFC3339),
			}
		}
	}

	data, err := json.MarshalIndent(ps, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal sessions: %w", err)
	}

	// Atomic write via temp file + rename
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		os.Remove(tmp) // clean up partial temp file
		return fmt.Errorf("write sessions file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename sessions file: %w", err)
	}
	return nil
}

// loadPersistedSessions reads backend-sessions.json and re-adopts any
// agent processes that are still alive. Adopted processes have no PTY fd,
// so SendMessage/SendInterrupt will return ErrAgentAdopted — the daemon
// should restart them if it needs to communicate.
func (b *DirectBackend) loadPersistedSessions() {
	path := b.sessionsFile()
	if path == "" {
		return
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "backend: failed to read sessions file %s: %v\n", path, err)
		}
		return // file doesn't exist or unreadable — nothing to adopt
	}

	var ps persistedState
	if err := json.Unmarshal(data, &ps); err != nil {
		fmt.Fprintf(os.Stderr, "backend: corrupted sessions file %s: %v (agents will not be re-adopted)\n", path, err)
		return
	}

	// Hold the lock for the entire adoption pass to prevent concurrent
	// map access from CreateSession/StartAgent/StopAgent.
	b.mu.Lock()
	defer b.mu.Unlock()

	for sessName, agents := range ps.Sessions {
		for agentName, pa := range agents {
			// Check if process is still alive
			if pa.PID <= 0 || syscall.Kill(pa.PID, 0) != nil {
				continue // dead — skip
			}

			// Guard against PID reuse: if the persisted entry is very old,
			// the PID likely belongs to an unrelated process now.
			if pa.StartedAt != "" {
				if startedAt, err := time.Parse(time.RFC3339, pa.StartedAt); err == nil {
					if time.Since(startedAt) > 7*24*time.Hour {
						continue // too old — PID reuse risk
					}
				}
			}

			// Recover the original start time for PID reuse guard
			var startedAt time.Time
			if pa.StartedAt != "" {
				if t, err := time.Parse(time.RFC3339, pa.StartedAt); err == nil {
					startedAt = t
				}
			}

			// Re-adopt: create managedProcess without PTY
			proc := &managedProcess{
				pid:       pa.PID,
				logPath:   pa.LogPath,
				done:      make(chan struct{}),
				stopMon:   make(chan struct{}),
				adopted:   true,
				startedAt: startedAt,
			}

			// Monitor the adopted process in background so proc.done
			// gets closed when it exits (enables health checks).
			// Exits when the process dies OR stopMon is closed (by killProcess).
			go func(p *managedProcess) {
				ticker := time.NewTicker(2 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-p.stopMon:
						// killProcess requested shutdown — signal done so waiters unblock.
						p.closeDone()
						return
					case <-ticker.C:
						if syscall.Kill(p.pid, 0) != nil {
							p.closeDone()
							return
						}
					}
				}
			}(proc)

			if b.sessions[sessName] == nil {
				b.sessions[sessName] = make(map[string]*managedProcess)
			}
			b.sessions[sessName][agentName] = proc
		}
	}

	// Re-persist to drop any dead processes we skipped
	if err := b.persistSessions(); err != nil {
		fmt.Fprintf(os.Stderr, "backend: %v\n", err)
	}
}

// IsAdopted returns true if the given agent was re-adopted after a daemon
// restart and has no PTY — meaning it can be monitored but not sent input.
func (b *DirectBackend) IsAdopted(ctx context.Context, session, agentName string) (bool, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	agents, ok := b.sessions[session]
	if !ok {
		return false, nil
	}
	proc, ok := agents[agentName]
	if !ok {
		return false, nil
	}
	return proc.adopted, nil
}

// SubscribeOutput subscribes to live ANSI-stripped output from the given agent.
// Returns the subscription ID, a channel of lines, and a cancel function.
// The channel includes catch-up lines from the ring buffer.
// Returns an error if the agent is not found or has no broadcaster (adopted).
func (b *DirectBackend) SubscribeOutput(session, agentName string) (uint64, <-chan string, func(), error) {
	return b.subscribeOutput(session, agentName, false)
}

// SubscribeOutputLive is like SubscribeOutput but skips ring buffer catch-up.
// Use this when the caller already has prior content and only wants new lines.
func (b *DirectBackend) SubscribeOutputLive(session, agentName string) (uint64, <-chan string, func(), error) {
	return b.subscribeOutput(session, agentName, true)
}

func (b *DirectBackend) subscribeOutput(session, agentName string, liveOnly bool) (uint64, <-chan string, func(), error) {
	b.mu.RLock()
	agents, ok := b.sessions[session]
	if !ok {
		b.mu.RUnlock()
		return 0, nil, nil, fmt.Errorf("session %q not found", session)
	}
	proc, ok := agents[agentName]
	b.mu.RUnlock()
	if !ok {
		return 0, nil, nil, fmt.Errorf("agent %q not found in session %q", agentName, session)
	}
	if proc.broadcaster == nil {
		return 0, nil, nil, fmt.Errorf("agent %q has no live output stream (adopted or dead)", agentName)
	}
	if liveOnly {
		id, ch, cancel := proc.broadcaster.SubscribeLive()
		return id, ch, cancel, nil
	}
	id, ch, cancel := proc.broadcaster.Subscribe()
	return id, ch, cancel, nil
}
