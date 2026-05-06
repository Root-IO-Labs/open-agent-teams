// Package agent provides utilities for programmatically running OAT Agent CLI.
//
// This package abstracts the details of launching and interacting with OAT Agent
// instances running in PTY-based terminal sessions. It handles:
//
//   - CLI flag construction (--resume, --auto-approve, -M)
//   - Session ID generation (UUID v4)
//   - Startup timing quirks
//   - Terminal integration via the [TerminalRunner] interface
//
// # Installation
//
//	go get github.com/Root-IO-Labs/open-agent-teams/pkg/agent
//
// # Requirements
//
// This package requires the OAT Agent CLI to be available. The binary is typically
// named "oat-agent" and should be available in current directory or PATH. Use [ResolveBinaryPath] to find it,
// and [Runner.IsBinaryAvailable] to verify it's available before use.
//
// # Example Usage
//
//	package main
//
//	import (
//	    "log"
//	    "github.com/Root-IO-Labs/open-agent-teams/pkg/agent"
//	    "github.com/Root-IO-Labs/open-agent-teams/pkg/backend"
//	)
//
//	func main() {
//	    // Create a backend client for terminal session management
//	    client := backend.NewClient()
//
//	    // Create agent runner with the backend as the terminal
//	    runner := agent.NewRunner(
//	        agent.WithTerminal(client),
//	        agent.WithBinaryPath(agent.ResolveBinaryPath()),
//	    )
//
//	    // Verify OAT Agent CLI is available
//	    if !runner.IsBinaryAvailable() {
//	        log.Fatal("OAT Agent CLI is not available")
//	    }
//
//	    // Prepare a session
//	    client.CreateSession("demo", true)
//	    client.CreateWindow("demo", "agent")
//
//	    // Start an agent
//	    result, err := runner.Start("demo", "agent", agent.Config{
//	        OutputFile: "/tmp/agent-output.log",
//	    })
//	    if err != nil {
//	        log.Fatal(err)
//	    }
//
//	    log.Printf("Agent started with session ID: %s, PID: %d", result.SessionID, result.PID)
//
//	    // Send a message
//	    if err := runner.SendMessage("demo", "agent", "Hello!"); err != nil {
//	        log.Fatal(err)
//	    }
//	}
//
// # The TerminalRunner Interface
//
// The [TerminalRunner] interface abstracts terminal operations, allowing this package
// to work with any backend that manages PTY sessions.
//
// # Session Management
//
// Each agent instance is identified by a session ID (UUID v4). This allows:
//
//   - Resuming sessions across process restarts
//   - Tracking multiple concurrent agent instances
//   - Correlating logs with specific sessions
//
// Use [GenerateSessionID] to create new session IDs, or provide your own via [Config.SessionID].
//
// # Timing Considerations
//
// Starting an agent and sending messages requires careful timing:
//
//   - [Runner.StartupDelay] (default 2s): Wait after launching before getting PID
//   - [Runner.MessageDelay] (default 2s): Wait before sending initial message
//
// These can be adjusted via [WithStartupDelay] and [WithMessageDelay] options.
package agent
