# Makefile for oat - Local CI Guard Rails
# Run these targets to verify changes before pushing

.PHONY: help build test unit-tests e2e-tests verify-docs coverage check-all pre-commit clean \
	dev-install dev-clean-bin dev-restart dev-status dev-clean-sockets dev-full-reset \
	dev-verify dev-nuke sidecar-test \
	release-snapshot release-check stage-runtime

# Default target
help:
	@echo "OAT Local CI Guard Rails"
	@echo ""
	@echo "Targets that mirror CI checks:"
	@echo "  make build          - Build all packages (CI: Build job)"
	@echo "  make unit-tests     - Run unit tests (CI: Unit Tests job)"
	@echo "  make e2e-tests      - Run E2E tests (CI: E2E Tests job)"
	@echo "  make verify-docs    - Check generated docs are up to date (CI: Verify Generated Docs job)"
	@echo "  make coverage       - Run coverage check (CI: Coverage Check job)"
	@echo ""
	@echo "Comprehensive checks:"
	@echo "  make check-all      - Run all CI checks locally (recommended before push)"
	@echo "  make pre-commit     - Fast checks suitable for git pre-commit hook"
	@echo ""
	@echo "Setup:"
	@echo "  make install-hooks  - Install git pre-commit hook"
	@echo ""
	@echo "Other:"
	@echo "  make test           - Alias for unit-tests"
	@echo "  make clean          - Clean build artifacts"
	@echo ""
	@echo "Dev workflow (rebuild + test the current worktree against a running daemon):"
	@echo "  make dev-install       - Rebuild oat + oat-agent, install to \$$GOPATH/bin"
	@echo "  make dev-clean-bin     - Remove installed oat + oat-agent binaries"
	@echo "  make dev-restart       - Stop daemon, rebuild, restart with OAT_USE_SIDECAR=1"
	@echo "  make dev-verify        - One-shot smoke check: is the sidecar pipeline live?"
	@echo "  make dev-full-reset    - dev-clean-bin + dev-clean-sockets + dev-install (scorched-earth)"
	@echo "  make dev-status        - Show daemon state, running agents, live sidecar sockets"
	@echo "  make dev-clean-sockets - rm /tmp/oat-sdcr-*.sock (safe only when daemon is stopped)"
	@echo "  make dev-nuke          - EMERGENCY: kill every OAT process, clean all state"
	@echo "  make sidecar-test      - Run sidecar-related unit + integration tests"

# ---------------------------------------------------------------------------
# Release metadata injected at build time
#
# VERSION resolves to the nearest `v*` tag if available, otherwise the
# short commit SHA with a `-dev` suffix so local builds stay identifiable.
# COMMIT / DATE carry the usual git/UTC-timestamp shape. All three are
# propagated via -ldflags "-X internal/version.*" so the resulting binary
# can report a precise build identity without reading /proc or .git at runtime.
# goreleaser re-exports the same three variables on tag builds.
# ---------------------------------------------------------------------------
VERSION ?= $(shell git describe --tags --abbrev=0 2>/dev/null || echo "0.0.0-dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X github.com/Root-IO-Labs/open-agent-teams/internal/version.Version=$(VERSION) \
           -X github.com/Root-IO-Labs/open-agent-teams/internal/version.Commit=$(COMMIT) \
           -X github.com/Root-IO-Labs/open-agent-teams/internal/version.Date=$(DATE)

# Build - matches CI build job
build:
	@echo "==> Building all packages..."
	@go build -ldflags "$(LDFLAGS)" -v ./...
	@echo "✓ Build successful ($(VERSION) $(COMMIT))"

# Unit tests - matches CI unit-tests job
unit-tests:
	@echo "==> Running unit tests..."
	@go test -coverprofile=coverage.out -covermode=atomic ./internal/... ./pkg/...
	@go tool cover -func=coverage.out | tail -1
	@echo "✓ Unit tests passed"

# E2E tests - matches CI e2e-tests job
e2e-tests:
	@echo "==> Running E2E tests..."
	@git config user.email >/dev/null 2>&1 || git config --global user.email "ci@local.dev"
	@git config user.name >/dev/null 2>&1 || git config --global user.name "Local CI"
	@go test -v ./test/...
	@echo "✓ E2E tests passed"

# Verify generated docs - matches CI verify-generated-docs job
verify-docs:
	@echo "==> Verifying generated docs are up to date..."
	@go generate ./pkg/config/...
	@if ! git diff --quiet docs/DIRECTORY_STRUCTURE.md; then \
		echo "Error: docs/DIRECTORY_STRUCTURE.md is out of date!"; \
		echo "Run 'go generate ./pkg/config/...' or 'make generate' and commit the changes."; \
		echo ""; \
		echo "Diff:"; \
		git diff docs/DIRECTORY_STRUCTURE.md; \
		exit 1; \
	fi
	@echo "==> Verifying extension documentation consistency..."
	@go run ./cmd/verify-docs
	@echo "✓ Generated docs are up to date"

# Coverage check - matches CI coverage-check job
coverage:
	@echo "==> Checking coverage thresholds..."
	@go test -coverprofile=coverage.out -covermode=atomic ./internal/... ./pkg/...
	@echo ""
	@echo "Coverage summary:"
	@go tool cover -func=coverage.out | grep "total:" || true
	@echo ""
	@echo "Per-package coverage:"
	@go test -cover ./internal/... ./pkg/... 2>&1 | grep "coverage:" | sort
	@echo "✓ Coverage check complete"

# Helper to regenerate docs
generate:
	@echo "==> Regenerating documentation..."
	@go generate ./pkg/config/...
	@echo "✓ Documentation regenerated"

# Alias for unit-tests
test: unit-tests

# Pre-commit: Fast checks suitable for git hook
# Runs build + unit tests + verify docs (skips slower e2e tests)
pre-commit: build unit-tests verify-docs
	@echo ""
	@echo "✓ All pre-commit checks passed"

# Check all: Complete CI validation locally
# Runs all checks that CI will run
check-all: build unit-tests e2e-tests verify-docs coverage
	@echo ""
	@echo "=========================================="
	@echo "✓ All CI checks passed locally!"
	@echo "Your changes are ready to push."
	@echo "=========================================="

# Install git hooks
install-hooks:
	@echo "==> Installing git pre-commit hook..."
	@mkdir -p .git/hooks
	@if [ -f .git/hooks/pre-commit ]; then \
		echo "Warning: .git/hooks/pre-commit already exists"; \
		echo "Backing up to .git/hooks/pre-commit.backup"; \
		cp .git/hooks/pre-commit .git/hooks/pre-commit.backup; \
	fi
	@cp scripts/pre-commit.sh .git/hooks/pre-commit
	@chmod +x .git/hooks/pre-commit
	@echo "✓ Git pre-commit hook installed"
	@echo ""
	@echo "The hook will run 'make pre-commit' before each commit."
	@echo "To skip the hook temporarily, use: git commit --no-verify"

# Clean build artifacts
clean:
	@echo "==> Cleaning build artifacts..."
	@rm -f coverage.out
	@go clean -cache
	@echo "✓ Clean complete"

# ---------------------------------------------------------------------------
# Dev workflow — rebuild + test the current worktree against a running daemon
#
# The oat binary users run is always $(go env GOPATH)/bin/oat, regardless of
# which checkout they're working in. These targets make it trivial to swap
# that binary for the one built from the current worktree, so you're testing
# code changes immediately instead of accidentally running a stale install.
# ---------------------------------------------------------------------------

GOBIN := $(shell go env GOPATH)/bin
OAT_BIN := $(GOBIN)/oat
OAT_AGENT_BIN := $(GOBIN)/oat-agent

# Rebuild oat + oat-agent from the current worktree and install to $GOPATH/bin.
# Overwrites any previous install. Does NOT restart the daemon — run
# `oat stop && oat start` yourself, or use `make dev-restart`.
dev-install:
	@echo "==> Installing oat + oat-agent from $(shell pwd)..."
	@go install -ldflags "$(LDFLAGS)" ./cmd/...
	@if [ -x "$(OAT_BIN)" ]; then \
		echo "  ✓ oat        → $(OAT_BIN) ($$(stat -f %Sm $(OAT_BIN) 2>/dev/null || stat -c %y $(OAT_BIN)))"; \
	else \
		echo "  ✗ oat binary not found after install"; exit 1; \
	fi
	@if [ -x "$(OAT_AGENT_BIN)" ]; then \
		echo "  ✓ oat-agent  → $(OAT_AGENT_BIN) ($$(stat -f %Sm $(OAT_AGENT_BIN) 2>/dev/null || stat -c %y $(OAT_AGENT_BIN)))"; \
	fi

# Remove installed binaries so a stale build can't mask the worktree code.
dev-clean-bin:
	@rm -f $(OAT_BIN) $(OAT_AGENT_BIN)
	@echo "✓ Removed $(OAT_BIN) and $(OAT_AGENT_BIN)"

# Stop daemon, rebuild, restart with sidecar enabled. The full loop you want
# after changing Go code that touches the daemon or backend.
#
# Robust stop: `oat stop` returns as soon as SIGTERM is sent, but the
# daemon may still be shutting down (flushing state, closing sockets,
# removing the PID file). Starting a new daemon while the old one holds
# the PID file fails with "daemon already running". We poll up to 10s
# for the process to exit, then escalate to SIGKILL.
dev-restart: dev-install
	@echo "==> Stopping daemon (if running)..."
	@-$(OAT_BIN) stop 2>/dev/null || true
	@for i in 1 2 3 4 5 6 7 8 9 10; do \
		if ! pgrep -f "oat daemon _run" > /dev/null 2>&1; then break; fi; \
		sleep 1; \
	done
	@if pgrep -f "oat daemon _run" > /dev/null 2>&1; then \
		echo "  ! daemon still running after 10s — sending SIGKILL"; \
		pkill -9 -f "oat start" 2>/dev/null || true; \
		sleep 1; \
	fi
	@rm -f "$$HOME/.oat/daemon.pid" 2>/dev/null || true
	@echo "==> Starting daemon with OAT_USE_SIDECAR=1..."
	@OAT_USE_SIDECAR=1 $(OAT_BIN) start
	@echo ""
	@echo "Daemon running with sidecar enabled. Verify:"
	@echo "  make dev-verify    # one-shot smoke check with pass/fail per step"
	@echo "  make dev-status    # detailed state"

# One-shot smoke check: is the sidecar pipeline live, end-to-end?
# Runs against the currently-running daemon; prints a status line per check.
# Non-zero exit only if the daemon isn't running — other "failures" may
# just mean no agents have emitted yet.
dev-verify:
	@echo "=== sidecar pipeline smoke check ==="
	@echo ""
	@echo "[1/5] daemon running?"
	@if pgrep -f "oat daemon _run" > /dev/null 2>&1; then \
		echo "      ✓ PID $$(pgrep -f 'oat start' | head -1)"; \
	else \
		echo "      ✗ no daemon — run 'make dev-restart'"; exit 1; \
	fi
	@echo "[2/5] most recent startup cleaned stale sockets?"
	@line=$$(grep "Cleaned.*stale sidecar" "$$HOME/.oat/daemon.log" 2>/dev/null | tail -1); \
	if [ -n "$$line" ]; then \
		echo "      ✓ $$(echo "$$line" | head -c 110)"; \
	else \
		echo "      (no cleanup line — either nothing to clean or old daemon)"; \
	fi
	@echo "[3/5] live /tmp/oat-sdcr-*.sock bindings"
	@count=$$(ls /tmp/oat-sdcr-*.sock 2>/dev/null | wc -l | tr -d ' '); \
	echo "      sockets bound: $$count"
	@echo "[4/5] recent Go-format [OAT_TOKENS] (proof sidecar bridge is writing)"
	@go_fmt=$$(find "$$HOME/.oat/output" -name "*.log" -mmin -10 2>/dev/null | \
		xargs grep -l 'cache_creation":.*cache_read":' 2>/dev/null | wc -l | tr -d ' '); \
	echo "      log(s) with sidecar-bridge emits in last 10 min: $$go_fmt"; \
	if [ "$$go_fmt" -gt "0" ]; then \
		echo "      ✓ sidecar → log bridge is live"; \
	else \
		echo "      (no active workers emitted tokens via sidecar in last 10 min)"; \
	fi
	@echo "[5/5] TUI event-stream subscriptions recently"
	@events=$$(grep -c "Event stream\|stream_events\|event stream client" "$$HOME/.oat/daemon.log" 2>/dev/null); \
	echo "      daemon.log event-stream mentions: $$events"; \
	if [ "$$events" = "0" ]; then \
		echo "      (no TUI has subscribed — start 'oat ui --repo <repo>')"; \
	fi
	@echo ""
	@echo "To drive the pipeline: create a worker and open the TUI."
	@echo "  oat worker create \"smoke test\" --repo <your-repo>"
	@echo "  oat ui --repo <your-repo>"
	@echo "  # then re-run: make dev-verify"

# Show live sidecar state so you can eyeball what's actually running.
# Intentionally verbose — this is a debugging aid, not a monitoring dashboard.
dev-status:
	@echo "=== Binary ==="
	@if [ -x "$(OAT_BIN)" ]; then \
		echo "  oat       $(OAT_BIN)  ($$(stat -f %Sm $(OAT_BIN) 2>/dev/null || stat -c %y $(OAT_BIN)))"; \
	else \
		echo "  oat       (not installed — run 'make dev-install')"; \
	fi
	@if [ -x "$(OAT_AGENT_BIN)" ]; then \
		echo "  oat-agent $(OAT_AGENT_BIN)  ($$(stat -f %Sm $(OAT_AGENT_BIN) 2>/dev/null || stat -c %y $(OAT_AGENT_BIN)))"; \
	fi
	@echo ""
	@echo "=== Daemon ==="
	@if pgrep -f "oat daemon _run" > /dev/null; then \
		echo "  running  PID $$(pgrep -f 'oat start' | head -1)"; \
	else \
		echo "  stopped"; \
	fi
	@echo ""
	@echo "=== OAT_USE_SIDECAR in daemon env ==="
	@if pgrep -f "oat daemon _run" > /dev/null; then \
		daemon_pid=$$(pgrep -f 'oat start' | head -1); \
		grep -z 'OAT_USE_SIDECAR' /proc/$$daemon_pid/environ 2>/dev/null | tr '\0' '\n' | grep OAT_USE_SIDECAR || \
		ps -E -p $$daemon_pid 2>/dev/null | tr ' ' '\n' | grep OAT_USE_SIDECAR || \
		echo "  (unable to inspect env — check how you started the daemon)"; \
	else \
		echo "  (daemon not running)"; \
	fi
	@echo ""
	@echo "=== Sidecar sockets in /tmp ==="
	@count=$$(ls /tmp/oat-sdcr-*.sock 2>/dev/null | wc -l | tr -d ' '); \
	if [ "$$count" = "0" ]; then \
		echo "  (none)"; \
	else \
		echo "  $$count socket file(s):"; \
		ls -la /tmp/oat-sdcr-*.sock 2>/dev/null | awk '{print "    " $$9 "  " $$6 " " $$7 " " $$8}'; \
	fi

# Remove ALL /tmp/oat-sdcr-*.sock files. Use when the daemon is stopped —
# otherwise the daemon's currently-bound sockets get unlinked too, and live
# agents can't accept new sidecar connections. The daemon rebinds on next
# emit anyway, so mistakes are recoverable, but still: stop first.
dev-clean-sockets:
	@count=$$(ls /tmp/oat-sdcr-*.sock 2>/dev/null | wc -l | tr -d ' '); \
	if [ "$$count" = "0" ]; then \
		echo "✓ No sidecar sockets to clean."; \
	else \
		echo "==> Removing $$count sidecar socket file(s)..."; \
		rm -f /tmp/oat-sdcr-*.sock; \
		echo "✓ Done."; \
	fi

# Scorched earth: stop daemon, clean binaries, clean sockets, rebuild + install.
# Doesn't restart the daemon — run `make dev-restart` or start it manually
# (often you want to set other env vars for a specific test).
dev-full-reset:
	@echo "==> Stopping daemon..."
	@-$(OAT_BIN) stop 2>/dev/null || true
	@sleep 1
	@$(MAKE) dev-clean-bin
	@$(MAKE) dev-clean-sockets
	@$(MAKE) dev-install
	@echo ""
	@echo "✓ Full reset complete. Start the daemon when ready:"
	@echo "  OAT_USE_SIDECAR=1 oat start     # sidecar enabled"
	@echo "  oat start                        # sidecar disabled (default today)"

# Run just the sidecar-related tests so you don't wait on the full suite
# when iterating on pkg/sidecar or pkg/backend sidecar code.
sidecar-test:
	@echo "==> Running sidecar-related Go tests..."
	@go test ./pkg/sidecar/... -v -timeout=60s
	@go test ./pkg/backend/... -run "Sidecar|Bridge|EventBroadcast|SubscribeEvents|OatTokens" -v -timeout=60s
	@go test ./internal/daemon/... -run "Sidecar" -v -timeout=30s
	@echo ""
	@echo "==> Running sidecar-related Python tests..."
	@cd agent-runtime/libs/cli && \
		PYTHONPATH=. .venv/bin/python -m pytest \
			tests/unit_tests/test_sidecar_events.py \
			tests/unit_tests/test_sidecar_client.py \
			tests/unit_tests/test_sidecar_emitter.py \
			--timeout=30

# EMERGENCY: kill every OAT process and clean all state. Use when the
# dev loop has accumulated orphan agent processes (common after
# interrupted `oat stop` or crashed daemon restarts during iteration).
#
# What it kills:
#   - oat daemon _run          (the daemon itself)
#   - oat-agent                (Go bridge for each agent)
#   - oat_cli                  (Python LangGraph runtime per agent)
#
# What it cleans:
#   - /tmp/oat-sdcr-*.sock     (sidecar sockets)
#   - ~/.oat/daemon.pid        (stale PID file)
#
# Agents' log files and state are NOT touched — you can re-adopt them
# after next daemon start. If you want a fully clean slate, also run
# `oat repair` or manually clear ~/.oat/state.json.
dev-nuke:
	@echo "==> Counting runaway processes..."
	@echo "    oat_cli Python: $$(pgrep -f 'oat_cli' | wc -l | tr -d ' ')"
	@echo "    oat-agent:         $$(pgrep -f 'oat-agent' | wc -l | tr -d ' ')"
	@echo "    oat daemon:        $$(pgrep -f 'oat daemon _run' | wc -l | tr -d ' ')"
	@echo "    sidecar sockets:   $$(ls /tmp/oat-sdcr-*.sock 2>/dev/null | wc -l | tr -d ' ')"
	@echo ""
	@echo "==> Stopping daemon gracefully..."
	@-$(OAT_BIN) stop 2>/dev/null || true
	@sleep 2
	@echo "==> Sending SIGTERM to every OAT process..."
	@-pkill -TERM -f "oat_cli" 2>/dev/null || true
	@-pkill -TERM -f "oat-agent" 2>/dev/null || true
	@-pkill -TERM -f "oat daemon _run" 2>/dev/null || true
	@sleep 3
	@echo "==> Escalating to SIGKILL for stragglers..."
	@-pkill -KILL -f "oat_cli" 2>/dev/null || true
	@-pkill -KILL -f "oat-agent" 2>/dev/null || true
	@-pkill -KILL -f "oat daemon _run" 2>/dev/null || true
	@sleep 1
	@echo "==> Cleaning sidecar sockets + PID file..."
	@rm -f /tmp/oat-sdcr-*.sock 2>/dev/null || true
	@rm -f "$$HOME/.oat/daemon.pid" 2>/dev/null || true
	@echo ""
	@echo "==> Final state:"
	@echo "    oat_cli Python: $$(pgrep -f 'oat_cli' | wc -l | tr -d ' ')"
	@echo "    oat-agent:         $$(pgrep -f 'oat-agent' | wc -l | tr -d ' ')"
	@echo "    oat daemon:        $$(pgrep -f 'oat daemon _run' | wc -l | tr -d ' ')"
	@echo "    sidecar sockets:   $$(ls /tmp/oat-sdcr-*.sock 2>/dev/null | wc -l | tr -d ' ')"
	@echo ""
	@echo "✓ OAT fully nuked. Start fresh with: make dev-restart"

# Release: stage agent-runtime/ for packaging (excludes venvs and caches)
stage-runtime:
	@bash scripts/stage-agent-runtime.sh

# Release: validate .goreleaser.yml syntax
release-check:
	@command -v goreleaser >/dev/null 2>&1 || { echo "goreleaser not found. Install with: brew install goreleaser"; exit 1; }
	@goreleaser check

# Release: build a local snapshot (no publish, no tag required) for dry-run testing
release-snapshot:
	@command -v goreleaser >/dev/null 2>&1 || { echo "goreleaser not found. Install with: brew install goreleaser"; exit 1; }
	@goreleaser release --snapshot --clean --skip=publish
	@echo ""
	@echo "✓ Snapshot artifacts in dist/"
