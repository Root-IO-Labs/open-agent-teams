# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in OAT, please report it responsibly through **GitHub Security Advisories**:

1. Go to the [Security Advisories page](https://github.com/Root-IO-Labs/open-agent-teams/security/advisories/new)
2. Click **"New draft security advisory"**
3. Fill in the details of the vulnerability
4. Submit the advisory

This creates a private channel where we can discuss and address the issue before any public disclosure.

**Please do not file public GitHub issues for security vulnerabilities.**

## Scope

The following are in scope for security reports:

- The `oat` CLI and daemon (`cmd/oat`, `internal/`)
- The agent runtime (`agent-runtime/`)
- The model routing system (`model-routing/`, `internal/routing/`)
- The Unix socket API (`internal/socket/`)

## Trust Model

OAT orchestrates AI coding agents that execute shell commands and modify files within isolated git worktrees. By design:

- **Agents can execute arbitrary shell commands** within their assigned worktree
- **The daemon manages agent processes** via PTY sessions
- **Inter-agent communication** flows through filesystem JSON files, routed by the daemon
- **No telemetry is collected or transmitted** -- all data stays local (LangSmith integration is optional and user-configured)

Users should be aware that running OAT grants AI agents the ability to execute code on your machine within their worktree scope.

## Supported Versions

We support the latest release. Older versions do not receive security patches.
