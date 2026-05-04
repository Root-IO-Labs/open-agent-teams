// Package agent provides utilities for programmatically running OAT Agent CLI.
//
// This package abstracts the details of launching and interacting with OAT Agent
// instances running in PTY-based terminal sessions. It handles:
//
//   - CLI flag construction
//   - Session ID generation
//   - Startup timing quirks
//   - Terminal integration via the TerminalRunner interface
//   - Context support for cancellation and timeouts
//
// # Quick Start
//
//	import (
//	    "context"
//	    "github.com/Root-IO-Labs/open-agent-teams/pkg/agent"
//	    "github.com/Root-IO-Labs/open-agent-teams/pkg/backend"
//	)
//
//	// Create a ProcessBackend and adapt it to the TerminalRunner interface.
//	b := backend.NewBackend("", "")
//	adapter := backend.NewTerminalAdapter(b)
//	runner := agent.NewRunner(agent.WithTerminal(adapter))
//
//	// Create a session, then start an agent inside it.
//	ctx := context.Background()
//	if err := b.CreateSession(ctx, "my-session"); err != nil { /* ... */ }
//	result, err := runner.Start(ctx, "my-session", "agent-window", agent.Config{
//	    WorkDir: "/path/to/workspace",
//	})
//
// # Sending Messages
//
//	// Send a message to a running agent instance
//	err := runner.SendMessage(ctx, "my-session", "agent-window", "Hello!")
package agent

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// TerminalRunner abstracts terminal interaction for running OAT Agents.
// Any backend that manages PTY sessions can implement this interface.
type TerminalRunner interface {
	// SendKeys sends text followed by Enter to submit.
	SendKeys(ctx context.Context, session, window, text string) error

	// SendKeysLiteral sends text without pressing Enter (supports multiline via paste-buffer).
	SendKeysLiteral(ctx context.Context, session, window, text string) error

	// SendEnter sends just the Enter key.
	SendEnter(ctx context.Context, session, window string) error

	// SendKeysLiteralWithEnter sends text + Enter atomically.
	// This prevents race conditions where Enter might be lost between separate calls.
	SendKeysLiteralWithEnter(ctx context.Context, session, window, text string) error

	// GetPanePID gets the process ID running in a pane.
	GetPanePID(ctx context.Context, session, window string) (int, error)

	// StartPipePane starts capturing pane output to a file.
	StartPipePane(ctx context.Context, session, window, outputFile string) error

	// StopPipePane stops capturing pane output.
	StopPipePane(ctx context.Context, session, window string) error
}

// Runner manages OAT Agent instances.
type Runner struct {
	// BinaryPath is the path to the oat-agent binary.
	// Defaults to "oat-agent" (relies on PATH).
	BinaryPath string

	// Terminal is the terminal runner for sending commands.
	Terminal TerminalRunner

	// StartupDelay is how long to wait after starting the agent before
	// attempting to get the PID. Defaults to 2s.
	StartupDelay time.Duration

	// MessageDelay is how long to wait after startup before sending
	// the first message. Defaults to 2s.
	MessageDelay time.Duration

	// SkipPermissions controls whether to pass --auto-approve.
	// This is required for non-interactive use. Defaults to true.
	SkipPermissions bool
}

// RunnerOption is a functional option for configuring a Runner.
type RunnerOption func(*Runner)

// WithBinaryPath sets a custom path to the oat-agent binary.
func WithBinaryPath(path string) RunnerOption {
	return func(r *Runner) {
		r.BinaryPath = path
	}
}

// WithTerminal sets the terminal runner.
func WithTerminal(t TerminalRunner) RunnerOption {
	return func(r *Runner) {
		r.Terminal = t
	}
}

// WithStartupDelay sets the startup delay.
func WithStartupDelay(d time.Duration) RunnerOption {
	return func(r *Runner) {
		r.StartupDelay = d
	}
}

// WithMessageDelay sets the message delay.
func WithMessageDelay(d time.Duration) RunnerOption {
	return func(r *Runner) {
		r.MessageDelay = d
	}
}

// WithPermissions controls whether to skip permission checks.
// Set to false to require interactive permission prompts.
func WithPermissions(skip bool) RunnerOption {
	return func(r *Runner) {
		r.SkipPermissions = skip
	}
}

// NewRunner creates a new OAT Agent runner with the given options.
func NewRunner(opts ...RunnerOption) *Runner {
	r := &Runner{
		BinaryPath:      "oat-agent",
		StartupDelay:    2 * time.Second,
		MessageDelay:    2 * time.Second,
		SkipPermissions: true,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// ResolveBinaryPath attempts to find the oat-agent binary in PATH.
// Returns the full path if found, otherwise returns "oat-agent".
func ResolveBinaryPath() string {
	if path, err := exec.LookPath("oat-agent"); err == nil {
		return path
	}
	return "oat-agent"
}

// IsBinaryAvailable checks if the OAT Agent CLI is installed and available.
// This is useful for verifying prerequisites before attempting to use the Runner.
// Similar to checking backend availability before use.
func (r *Runner) IsBinaryAvailable() bool {
	cmd := exec.Command(r.BinaryPath, "--version")
	return cmd.Run() == nil
}

// Config contains configuration for starting an OAT Agent instance.
type Config struct {
	// SessionID is the unique identifier for this agent session.
	// If empty, a new UUID will be generated.
	//
	// Session IDs allow resuming conversations across process restarts.
	// They correlate logs with specific sessions and track concurrent instances.
	SessionID string

	// Resume indicates this is resuming an existing session rather than starting fresh.
	// When true, uses --resume instead of --thread-id.
	//
	// Use Resume=true when:
	//   - Restarting an agent after a crash
	//   - Continuing a conversation from a previous run
	//   - The session state was previously saved by the agent
	//
	// Use Resume=false (default) when:
	//   - Starting a new conversation
	//   - The session has never been started before
	Resume bool

	// WorkDir is the working directory for the agent.
	// If non-empty, the command will cd to this directory before launching the agent.
	WorkDir string

	// SystemPromptFile is the path to a file containing the system prompt.
	// The daemon reads this file and passes the content via -m to oat-agent.
	SystemPromptFile string

	// InitialMessage is an optional message to send to the agent after startup.
	// If non-empty, sent after MessageDelay.
	InitialMessage string

	// OutputFile is the path to capture the agent's output.
	// If non-empty, StartPipePane is called with this file.
	OutputFile string

	// MOTD is an optional message of the day to display before starting the agent.
	// This is useful for showing restart instructions or other information.
	// If empty, no MOTD is displayed.
	MOTD string

	// EnvPrefix is an optional shell prefix (e.g. "source ~/.zshrc; export FOO=bar; ") run before the oat-agent binary.
	// Used so core agents started by the daemon inherit user environment (GH_TOKEN, etc.). Never logged.
	EnvPrefix string

	// Model is the LLM model spec (e.g., "claude-sonnet-4-6"). Passed via --model to the agent CLI.
	// If empty, the agent CLI auto-detects from available API keys.
	Model string

	// ModelParams is a JSON string of extra kwargs passed to the model via --model-params.
	// e.g. `{"max_tokens": 32000}`. If empty, only agent CLI defaults apply.
	ModelParams string
}

// StartResult contains information about a started OAT Agent instance.
type StartResult struct {
	// SessionID is the session ID used for this agent instance.
	SessionID string

	// PID is the process ID of the agent process.
	PID int

	// Command is the full command that was executed.
	Command string
}

// Start launches an agent in the specified session/window.
func (r *Runner) Start(ctx context.Context, session, window string, cfg Config) (*StartResult, error) {
	if r.Terminal == nil {
		return nil, fmt.Errorf("terminal runner not configured")
	}

	// Generate session ID if not provided
	sessionID := cfg.SessionID
	if sessionID == "" {
		var err error
		sessionID, err = GenerateSessionID()
		if err != nil {
			return nil, fmt.Errorf("failed to generate session ID: %w", err)
		}
	}

	// Build the command
	cmd := r.buildCommand(sessionID, cfg)

	// Start output capture if configured
	if cfg.OutputFile != "" {
		if err := r.Terminal.StartPipePane(ctx, session, window, cfg.OutputFile); err != nil {
			return nil, fmt.Errorf("failed to start output capture: %w", err)
		}
	}

	// Print MOTD before starting the agent if configured
	if cfg.MOTD != "" {
		motd := fmt.Sprintf("echo %q", cfg.MOTD)
		if err := r.Terminal.SendKeys(ctx, session, window, motd); err != nil {
			// Non-fatal - just continue
		}
	}

	// Send the command to start the agent
	if err := r.Terminal.SendKeys(ctx, session, window, cmd); err != nil {
		return nil, fmt.Errorf("failed to send agent command: %w", err)
	}

	// Wait for the agent to start (respecting context)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(r.StartupDelay):
	}

	// Get the shell PID (pane's process is the shell, not the agent)
	shellPID, err := r.Terminal.GetPanePID(ctx, session, window)
	if err != nil {
		return nil, fmt.Errorf("failed to get pane PID: %w", err)
	}

	// Find agent child process (shell spawns agent); retry if agent starts slowly
	pid, err := FindAgentChildWithRetry(ctx, shellPID, 30, 500*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("failed to find agent process: %w", err)
	}

	// Send initial message if configured
	if cfg.InitialMessage != "" {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(r.MessageDelay):
		}
		if err := r.Terminal.SendKeysLiteralWithEnter(ctx, session, window, cfg.InitialMessage); err != nil {
			return nil, fmt.Errorf("failed to send initial message: %w", err)
		}
	}

	return &StartResult{
		SessionID: sessionID,
		PID:       pid,
		Command:   cmd,
	}, nil
}

// buildCommand constructs the OAT Agent CLI command string.
func (r *Runner) buildCommand(sessionID string, cfg Config) string {
	var cmd string

	if cfg.EnvPrefix != "" {
		cmd = cfg.EnvPrefix
	}

	if cfg.WorkDir != "" {
		cmd += fmt.Sprintf("cd %q && ", cfg.WorkDir)
	}

	cmd += r.BinaryPath

	// Session identity
	if cfg.Resume {
		cmd += fmt.Sprintf(" --resume %s", sessionID)
	}

	// Auto-approve for non-interactive use
	if r.SkipPermissions {
		cmd += " --auto-approve"
	}

	if cfg.Model != "" {
		cmd += fmt.Sprintf(" -M %s", cfg.Model)
	}

	if cfg.ModelParams != "" {
		cmd += fmt.Sprintf(" --model-params %s", cfg.ModelParams)
	}

	return cmd
}

// SendMessage sends a message to a running agent instance.
// This properly handles multiline messages using paste-buffer and sends
// text + Enter atomically to prevent race conditions.
func (r *Runner) SendMessage(ctx context.Context, session, window, message string) error {
	if r.Terminal == nil {
		return fmt.Errorf("terminal runner not configured")
	}

	// Use atomic send for reliability
	if err := r.Terminal.SendKeysLiteralWithEnter(ctx, session, window, message); err != nil {
		return fmt.Errorf("failed to send message: %w", err)
	}

	return nil
}

// GenerateSessionID generates a UUID v4 session ID.
func GenerateSessionID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("failed to generate session ID: %w", err)
	}

	// Set version (4) and variant bits for UUID v4
	bytes[6] = (bytes[6] & 0x0f) | 0x40 // Version 4
	bytes[8] = (bytes[8] & 0x3f) | 0x80 // Variant 10

	return fmt.Sprintf("%x-%x-%x-%x-%x",
		bytes[0:4],
		bytes[4:6],
		bytes[6:8],
		bytes[8:10],
		bytes[10:16],
	), nil
}

// FindAgentChildWithRetry finds the agent process that is a child of the given shell PID.
// It retries up to maxAttempts times, waiting interval between attempts, so that slow
// startup is handled. Returns the agent process PID or an error.
// When OAT_TEST_MODE=1, returns shellPID so tests that mock GetPanePID pass.
func FindAgentChildWithRetry(ctx context.Context, shellPID int, maxAttempts int, interval time.Duration) (int, error) {
	if os.Getenv("OAT_TEST_MODE") == "1" {
		return shellPID, nil
	}
	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		pid, err := findAgentChild(shellPID)
		if err == nil {
			return pid, nil
		}
		lastErr = err
		if i < maxAttempts-1 {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(interval):
			}
		}
	}
	return 0, fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
}

// findAgentChild returns the PID of an oat-agent process that is a direct child or grandchild of parentPID.
// The pane shell often runs "source ~/.zshrc; ...; <agent>" so the agent can be a child of a subshell (grandchild of pane).
func findAgentChild(parentPID int) (int, error) {
	cmd := exec.Command("ps", "-e", "-o", "pid=", "-o", "ppid=", "-o", "args=")
	output, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("ps failed: %w", err)
	}
	parentStr := strconv.Itoa(parentPID)
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")

	// Collect direct children of parentPID
	childPIDs := make(map[string]bool)
	childPIDs[parentStr] = true

	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pidStr, ppidStr := fields[0], fields[1]
		if ppidStr == parentStr {
			childPIDs[pidStr] = true
		}
	}

	// Look for agent process as direct child or grandchild
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pidStr, ppidStr := fields[0], fields[1]
		args := strings.ToLower(strings.Join(fields[2:], " "))
		if !strings.Contains(args, "oat-agent") && !strings.Contains(args, "oatagent") && !strings.Contains(args, "deepagents") {
			continue
		}
		if ppidStr != parentStr && !childPIDs[ppidStr] {
			continue
		}
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			continue
		}
		return pid, nil
	}
	return 0, fmt.Errorf("no oat-agent process found for parent PID %d", parentPID)
}
