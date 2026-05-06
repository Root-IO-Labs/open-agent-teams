# Active Known Issues

This document lists current known issues with OAT.

## Critical Issues

None currently tracked.

## High Priority Issues  

### Token Usage Optimization
- Workers receive entire agent prompt tree instead of role-specific prompts
- Default max_tokens=32000 is oversized for cheaper models
- Supervisor sends full agent definitions on initialization

**Workaround**: Use model-specific routing profiles to control token limits.

### Large File Organization
- `internal/cli/cli.go` (8,371 lines) - needs splitting by command group
- `internal/daemon/daemon.go` (5,380 lines) - needs modularization

**Impact**: Makes contributing more difficult for new developers.

## Medium Priority Issues

### Python Package Naming
- Internal package still uses `oat_sdk` namespace from upstream dependency
- Would require significant refactoring of the oat_sdk SDK dependency

**Impact**: Brand confusion, but functional.

### Benchmark Integration
- Fast benchmark mode not fully integrated
- No cost estimates in documentation

**Workaround**: Use standard benchmark mode with cost awareness.

## Low Priority Issues

### Platform Support
- No official Windows support (Unix/macOS only)
- No Windows CI testing

**Workaround**: Use WSL2 on Windows.

---

For bug reports and feature requests, please use [GitHub Issues](https://github.com/Root-IO-Labs/open-agent-teams/issues).