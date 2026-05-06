// Package backend defines the ProcessBackend interface for managing agent
// processes. It abstracts away the underlying process management mechanism
// so the daemon and CLI can work with any backend.
//
// Interface methods:
//
//   - CreateSession/DestroySession manage logical session groupings
//   - StartAgent/StopAgent launch and terminate agent processes
//   - SendMessage/SendInterrupt deliver input to agents
//   - IsAgentAlive checks agent process health
//   - Attach connects a terminal to an agent
//   - GetOutputReader streams agent output
package backend

import (
	"context"
	"io"
	"os"

	"github.com/Root-IO-Labs/open-agent-teams/pkg/sidecar"
)

// ProcessBackend abstracts agent process lifecycle management.
// The DirectBackend implementation uses creack/pty for process management.
type ProcessBackend interface {
	// CreateSession creates a new logical session (a grouping of agents).
	CreateSession(ctx context.Context, name string) error

	// DestroySession tears down a session and all agents within it.
	DestroySession(ctx context.Context, name string) error

	// HasSession checks whether a session exists.
	HasSession(ctx context.Context, name string) (bool, error)

	// StartAgent launches an agent process within a session.
	// Returns a handle to the running agent.
	StartAgent(ctx context.Context, cfg AgentConfig) (*AgentHandle, error)

	// StopAgent terminates a running agent. Sends SIGTERM, waits, then SIGKILL.
	StopAgent(ctx context.Context, session, agent string) error

	// IsAgentAlive checks whether an agent process is still running.
	IsAgentAlive(ctx context.Context, session, agent string) (bool, error)

	// SendMessage sends text to an agent's stdin followed by Enter.
	// Must be safe for concurrent use (implementations serialize per-agent).
	SendMessage(ctx context.Context, session, agent, message string) error

	// SendInterrupt sends Ctrl-C to an agent.
	SendInterrupt(ctx context.Context, session, agent string) error

	// SendEscape sends the Escape key (0x1b) to an agent.
	// Used to cancel a "Thinking..." state without killing the process.
	SendEscape(ctx context.Context, session, agent string) error

	// GetOutputReader returns a reader for an agent's output stream.
	GetOutputReader(ctx context.Context, session, agent string) (io.ReadCloser, error)

	// Attach connects the current terminal to an agent for interactive use.
	// If stdin is a terminal, provides interactive PTY proxy; otherwise tails log.
	// If readOnly is true, attaches in read-only mode.
	Attach(ctx context.Context, session, agent string, readOnly bool) error

	// ListSessions returns all session names managed by this backend.
	// Used by cleanup/repair to find orphaned sessions.
	ListSessions(ctx context.Context) ([]string, error)
}

// BackendInfo provides metadata about a backend implementation.
// This is separate from ProcessBackend to allow checking availability
// without instantiating a full backend.
type BackendInfo interface {
	// Name returns the backend identifier (e.g., "direct").
	Name() string

	// Available returns true if this backend can be used in the current
	// environment (e.g., PTY support exists).
	Available() bool
}

// SidecarSubscriber is an optional interface for backends that expose
// a structured-event sidecar stream per agent. Consumers (e.g., the
// daemon's stream_events command) type-assert on this rather than
// adding the method to ProcessBackend, so non-sidecar backends (tmux,
// container) don't have to stub an unsupported method.
//
// The subscription succeeds even when the sidecar is off for that
// agent — the channel just never fires. That keeps the protocol
// stable regardless of feature-flag state.
type SidecarSubscriber interface {
	// SubscribeEvents returns a live channel of sidecar events for
	// (session, agent). catchup holds the most-recent buffered events
	// at subscription time in chronological order — deliver those to
	// the consumer before draining the channel so a mid-session TUI
	// sees prior context immediately. cancel is idempotent and must
	// be called when the consumer is done to free the subscriber slot.
	SubscribeEvents(ctx context.Context, session, agent string) (
		subID uint64,
		events <-chan sidecar.Event,
		catchup []sidecar.Event,
		cancel func(),
		err error,
	)
}

// AgentConfig contains all parameters needed to start an agent.
type AgentConfig struct {
	// SessionName is the session this agent belongs to.
	SessionName string

	// AgentName is the unique name for this agent within the session
	// (e.g., "supervisor", "worker-0").
	AgentName string

	// WorkDir is the working directory for the agent process.
	WorkDir string

	// BinaryPath is the path to the agent binary (e.g., "oat-agent").
	BinaryPath string

	// Args are command-line arguments for the agent binary.
	Args []string

	// Env is additional environment variables for the agent process.
	// Each entry is "KEY=VALUE".
	Env []string

	// EnvPrefix is a shell prefix run before the agent binary
	// (e.g., "source ~/.zshrc; export GH_TOKEN=...;").
	EnvPrefix string

	// InitialPrompt is the system prompt file path to write before launch.
	// If non-empty, written to .oat/AGENTS.md in the WorkDir.
	InitialPrompt string

	// LogFile is the path to capture agent output. If empty, a default
	// path is derived from the session/agent name.
	LogFile string

	// MOTD is an optional message of the day to display before the agent starts.
	MOTD string

	// SidecarPath, when non-empty, tells the backend to bind a Unix-socket
	// sidecar.Server at that path before the agent process is spawned. The
	// path is also threaded into the agent's env as OAT_SIDECAR_SOCKET so
	// the Python sidecar_emitter can connect. Empty = sidecar disabled
	// (current production default; behavior is unchanged).
	SidecarPath string
}

// AgentHandle represents a running agent process.
type AgentHandle struct {
	// PID is the OS process ID of the agent.
	PID int

	// Stdin provides write access to the agent's input.
	// May be nil if the backend manages input internally.
	Stdin io.WriteCloser

	// Stdout provides read access to the agent's output.
	// May be nil if the backend manages output internally.
	Stdout io.ReadCloser

	// LogFile is the path where agent output is being captured.
	LogFile string
}

// NewBackend creates a ProcessBackend based on the preference string.
// Supported values:
//   - "direct": use DirectBackend (uses creack/pty)
//   - "" (empty): defaults to DirectBackend
//
// dataDir is the directory for persisting session metadata (e.g., ~/.oat/).
// If empty, session persistence is disabled.
func NewBackend(preference string, dataDir string) ProcessBackend {
	if dataDir != "" {
		return NewDirectBackendWithDataDir(dataDir)
	}
	return NewDirectBackend()
}

// BackendFromEnv creates a ProcessBackend using the OAT_BACKEND environment variable.
// This is the standard entry point for CLI initialization (no persistence).
func BackendFromEnv() ProcessBackend {
	return NewBackend(os.Getenv("OAT_BACKEND"), "")
}
