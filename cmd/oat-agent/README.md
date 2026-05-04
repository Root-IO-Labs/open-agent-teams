# OAT Agent Wrapper

This Go wrapper integrates with the OAT-branded Python agent runtime located in `agent-runtime/`.

## What it does

The `oat-agent` command:

1. **Finds the Python agent runtime** - Locates the `agent-runtime/` directory relative to the binary
2. **Handles Python environment setup** - Sets up PYTHONPATH and working directory correctly  
3. **Provides the same CLI interface** - Passes through all CLI arguments to the underlying Python CLI
4. **Cross-platform execution** - Works on macOS, Linux, and Windows
5. **Smart execution strategy**:
   - First tries to use the installed `oat-agent` command (if available)
   - Falls back to running the Python module directly from source

## Building

```bash
go build -o oat-agent cmd/oat-agent/main.go
```

## Usage

The wrapper provides the exact same interface as the Python CLI:

```bash
# Show help
./oat-agent --help

# Start interactive mode (displays OAT ASCII art)  
./oat-agent

# Non-interactive mode
./oat-agent -n "Summarize this file"

# Use specific agent
./oat-agent -a coder

# Show version
./oat-agent --version
```

## OAT Branding

When run in interactive mode, the agent displays the OAT (Open Agent Teams) ASCII art banner:

```
 ██████╗  █████╗ ████████╗
██╔═══██╗██╔══██╗╚══██╔══╝    ▄▓▓▄
██║   ██║███████║   ██║      ▓•███▙
██║   ██║██╔══██║   ██║      ░▀▀████▙▖
╚██████╔╝██║  ██║   ██║         █▓████▙▖
 ╚═════╝ ╚═╝  ╚═╝   ╚═╝         ▝█▓█████▙
                                  ░▜█▓████▙
 ██████╗ ██████╗ ███████╗███╗   ██╗  ░█▀█▛▀▀▜▙▄
██╔═══██╗██╔══██╗██╔════╝████╗  ██║ ░▀░▀▒▛░░  ▝▀▘
██║   ██║██████╔╝█████╗  ██╔██╗ ██║
...
OAT - Open Agent Teams ready! What would you like to build?
```

## Requirements

- Go 1.24.2+ for building
- Python 3.11+ runtime
- The agent-runtime dependencies installed (automatically detected)

## Error Handling

The wrapper handles errors gracefully:

- Reports if agent-runtime directory is not found
- Checks for compatible Python version (3.11+)
- Falls back between execution strategies
- Provides clear error messages

## Directory Structure

```
cmd/oat-agent/
├── main.go          # Go wrapper implementation
└── README.md        # This file
```

The wrapper looks for `agent-runtime/` in several locations relative to the binary:
- Current working directory
- Two directories up (for development)
- Alongside the binary (for distribution)