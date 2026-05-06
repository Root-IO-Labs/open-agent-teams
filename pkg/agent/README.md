# pkg/agent

A Go library for programmatically running and interacting with OAT Agent Runtime.

## Installation

```bash
go get github.com/Root-IO-Labs/open-agent-teams/pkg/agent
```

## Quick Start

```go
package main

import (
    "context"
    "log"

    "github.com/Root-IO-Labs/open-agent-teams/pkg/agent"
    "github.com/Root-IO-Labs/open-agent-teams/pkg/backend"
)

func main() {
    ctx := context.Background()

    // 1. Get a ProcessBackend (the direct PTY backend by default).
    //    Pass an empty preference and an empty data dir for an in-memory backend.
    b := backend.NewBackend("", "")

    // 2. Adapt the ProcessBackend to the TerminalRunner interface that
    //    agent.Runner expects.
    adapter := backend.NewTerminalAdapter(b)

    // 3. Build the runner.
    runner := agent.NewRunner(
        agent.WithTerminal(adapter),
        agent.WithBinaryPath("oat-agent"), // or an absolute path
    )

    // 4. Create a session that will hold one or more agents.
    if err := b.CreateSession(ctx, "my-session"); err != nil {
        log.Fatal(err)
    }
    defer b.DestroySession(context.Background(), "my-session")

    // 5. Start an agent within the session.
    result, err := runner.Start(ctx, "my-session", "agent", agent.Config{
        SystemPromptFile: "/path/to/prompt.md",
        WorkDir:          "/path/to/workspace",
    })
    if err != nil {
        log.Fatal(err)
    }
    log.Printf("agent started: session=%s, pid=%d", result.SessionID, result.PID)

    // 6. Send a message.
    if err := runner.SendMessage(ctx, "my-session", "agent", "Hello, agent!"); err != nil {
        log.Fatal(err)
    }
}
```

## Key Features

### Context Support

All I/O methods accept a `context.Context` as the first parameter, enabling:

- Request cancellation
- Timeouts
- Graceful shutdown

```go
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

result, err := runner.Start(ctx, "session", "window", agent.Config{})
if errors.Is(err, context.DeadlineExceeded) {
    log.Println("agent startup timed out")
}
```

### TerminalRunner Interface

The package uses the `TerminalRunner` interface to abstract terminal operations:

```go
type TerminalRunner interface {
    SendKeys(ctx context.Context, session, window, text string) error
    SendKeysLiteral(ctx context.Context, session, window, text string) error
    SendEnter(ctx context.Context, session, window string) error
    SendKeysLiteralWithEnter(ctx context.Context, session, window, text string) error
    GetPanePID(ctx context.Context, session, window string) (int, error)
    StartPipePane(ctx context.Context, session, window, outputFile string) error
    StopPipePane(ctx context.Context, session, window string) error
}
```

The `pkg/backend` package provides a ready-to-use implementation, but you can create custom implementations for other backends.

### Working Directory Support

Specify the working directory where agent should run:

```go
result, err := runner.Start(ctx, "session", "window", agent.Config{
    WorkDir: "/path/to/project",  // agent will cd here before starting
})
```

### Message of the Day (MOTD)

Display a custom message before agent starts (useful for showing restart instructions or context):

```go
result, err := runner.Start(ctx, "session", "window", agent.Config{
    MOTD: "Restarting agent session after crash...",
})
```

### Session ID Management

Each agent instance gets a unique UUID v4 session ID:

```go
// Generate a new session ID
sessionID, err := agent.GenerateSessionID()

// Or let Start() generate one
result, _ := runner.Start(ctx, "session", "window", agent.Config{})
fmt.Println(result.SessionID)

// Or provide your own
result, _ := runner.Start(ctx, "session", "window", agent.Config{
    SessionID: "my-custom-id",
})

// Resume an existing session
result, _ := runner.Start(ctx, "session", "window", agent.Config{
    SessionID: existingID,
    Resume:    true,  // Uses --resume instead of --session-id
})
```

### Output Capture

Capture agent's output to a file:

```go
result, err := runner.Start(ctx, "session", "window", agent.Config{
    OutputFile: "/tmp/agent-output.log",
})
```

### Multiline Messages

The `SendMessage` method uses atomic sends to properly handle multiline text:

```go
message := `Please review this code:

func hello() {
    fmt.Println("Hello, World!")
}

What improvements would you suggest?`

runner.SendMessage(ctx, "session", "window", message)
```

## Configuration Options

```go
runner := agent.NewRunner(
    // Path to agent binary (default: "agent")
    agent.WithBinaryPath("/usr/local/bin/agent"),

    // Terminal runner (required for Start/SendMessage)
    agent.WithTerminal(client),

    // Time to wait after starting before getting PID (default: 500ms)
    agent.WithStartupDelay(1 * time.Second),

    // Time to wait before sending initial message (default: 1s)
    agent.WithMessageDelay(2 * time.Second),

    // Whether to skip permission prompts (default: true)
    agent.WithPermissions(true),
)
```

## Config Fields

| Field | Description |
|-------|-------------|
| `SessionID` | Unique session identifier (auto-generated if empty) |
| `Resume` | If true, uses `--resume` instead of `--session-id` |
| `WorkDir` | Working directory to cd into before starting |
| `SystemPromptFile` | Path to system prompt file |
| `InitialMessage` | Optional message to send after startup |
| `OutputFile` | Path to capture output via pipe-pane |
| `MOTD` | Message to display before starting agent |

## CLI Flags

The runner constructs agent commands with these flags:

| Flag | Description |
|------|-------------|
| `--session-id <uuid>` | Unique session identifier |
| `--resume <uuid>` | Resume existing session |
| `--dangerously-skip-permissions` | Skip interactive permission prompts |
| `--append-system-prompt-file <path>` | Path to system prompt file |

## Prompt Building

For building complex prompts, see the `pkg/agent/prompt` subpackage:

```go
import "github.com/Root-IO-Labs/open-agent-teams/pkg/agent/prompt"

builder := prompt.NewBuilder()
builder.AddSection("Role", "You are a helpful coding assistant.")
builder.AddSection("Context", "Working on a Go project.")

promptText := builder.Build()
```

## Use Cases

- Running multiple agent instances in parallel
- Automated code review with agent
- CI/CD integration
- Interactive development assistants
- Pair programming automation

## Requirements

- OAT Agent Runtime installed and available
- Go 1.21 or later

## License

See the main project LICENSE file.
