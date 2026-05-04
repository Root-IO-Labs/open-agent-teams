package backend

import (
	"context"
	"fmt"
)

// LiteralSender is an optional interface that backends can implement
// to support sending text without a trailing Enter key.
type LiteralSender interface {
	// SendKeysLiteral sends text to the agent without appending Enter.
	SendKeysLiteral(ctx context.Context, session, agent, text string) error

	// SendEnter sends just the Enter key.
	SendEnter(ctx context.Context, session, agent string) error
}

// PaneInspector is an optional interface for backends that support
// inspecting the pane PID.
type PaneInspector interface {
	GetPanePID(ctx context.Context, session, agent string) (int, error)
}

// PipePaneManager is an optional interface for backends that support
// output capture via pipe-pane.
type PipePaneManager interface {
	StartPipePane(ctx context.Context, session, agent, outputFile string) error
	StopPipePane(ctx context.Context, session, agent string) error
}

// TerminalAdapter wraps a ProcessBackend to implement the agent.TerminalRunner
// interface. This bridges the old terminal-centric API with the new backend
// abstraction, allowing the agent.Runner to work with any ProcessBackend.
//
// In the TerminalRunner interface, "session" and "window" map to the backend's
// "session" and "agent" parameters respectively.
//
// Optional capabilities (LiteralSender, PaneInspector, PipePaneManager) are
// detected via interface assertion — no backend-specific type assertions needed.
type TerminalAdapter struct {
	Backend ProcessBackend
}

// NewTerminalAdapter creates a TerminalAdapter for the given backend.
func NewTerminalAdapter(b ProcessBackend) *TerminalAdapter {
	return &TerminalAdapter{Backend: b}
}

// SendKeys sends text followed by Enter. Maps to Backend.SendMessage.
func (a *TerminalAdapter) SendKeys(ctx context.Context, session, window, text string) error {
	return a.Backend.SendMessage(ctx, session, window, text)
}

// SendKeysLiteral sends text without pressing Enter.
// Uses the LiteralSender interface if the backend supports it,
// otherwise falls back to SendMessage (which includes Enter).
func (a *TerminalAdapter) SendKeysLiteral(ctx context.Context, session, window, text string) error {
	if ls, ok := a.Backend.(LiteralSender); ok {
		return ls.SendKeysLiteral(ctx, session, window, text)
	}
	// Fallback: SendMessage includes Enter, which is acceptable for
	// agent runner use cases (MOTD, commands) that always need Enter.
	return a.Backend.SendMessage(ctx, session, window, text)
}

// SendEnter sends just the Enter key.
func (a *TerminalAdapter) SendEnter(ctx context.Context, session, window string) error {
	if ls, ok := a.Backend.(LiteralSender); ok {
		return ls.SendEnter(ctx, session, window)
	}
	return a.Backend.SendMessage(ctx, session, window, "")
}

// SendKeysLiteralWithEnter sends text + Enter atomically. Maps to Backend.SendMessage.
func (a *TerminalAdapter) SendKeysLiteralWithEnter(ctx context.Context, session, window, text string) error {
	return a.Backend.SendMessage(ctx, session, window, text)
}

// GetPanePID gets the process ID running in a pane.
// Uses the PaneInspector interface if available. For backends that don't
// support this (DirectBackend), callers should use AgentHandle.PID instead.
func (a *TerminalAdapter) GetPanePID(ctx context.Context, session, window string) (int, error) {
	if pi, ok := a.Backend.(PaneInspector); ok {
		return pi.GetPanePID(ctx, session, window)
	}
	return 0, fmt.Errorf("GetPanePID not supported for this backend; use AgentHandle.PID")
}

// StartPipePane starts capturing pane output to a file.
// Uses PipePaneManager if available. Other backends handle output capture
// internally (e.g., DirectBackend tees PTY output to log file).
func (a *TerminalAdapter) StartPipePane(ctx context.Context, session, window, outputFile string) error {
	if pm, ok := a.Backend.(PipePaneManager); ok {
		return pm.StartPipePane(ctx, session, window, outputFile)
	}
	// Other backends handle output capture internally
	return nil
}

// StopPipePane stops capturing pane output.
func (a *TerminalAdapter) StopPipePane(ctx context.Context, session, window string) error {
	if pm, ok := a.Backend.(PipePaneManager); ok {
		return pm.StopPipePane(ctx, session, window)
	}
	return nil
}
