// Browser-agent bridge resolution.
//
// The browser-agent uses the oat-browser-agent npm package's stdio MCP
// bridge to drive Chrome. Both the daemon (when spawning the agent) and
// the CLI (when running `oat agent add browser-agent` preflight) need
// to resolve the bridge to the same command, so the resolution lives
// here as a single function.
//
// See:
//   - https://github.com/Root-IO-Labs/oat-browser-agent
//   - internal/templates/agent-templates/browser.md (system prompt)

package agents

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// BridgeCommand is the resolved invocation for the oat-browser-agent
// stdio bridge -- argv[0] (Command) plus any leading args (Args). The
// Source field is human-readable text describing where the bridge was
// found ("$OAT_BROWSER_AGENT_BRIDGE_PATH", "$PATH", "~/.oat bundle"),
// surfaced to the user in the `oat agent add` preflight output.
type BridgeCommand struct {
	Command string
	Args    []string
	Source  string
}

// ResolveBrowserBridge probes for the oat-browser-agent bridge in this
// order:
//
//  1. $OAT_BROWSER_AGENT_BRIDGE_PATH (literal path; treated as a Node
//     script when it ends in .js or .mjs, otherwise as an executable).
//  2. `oat-browser-agent` discovered on PATH (npm-installed shim).
//  3. ~/.oat/oat-browser-agent/dist/bridge/index.js bundled by an
//     install script, run through `node`.
//
// Returns a structured error describing what to do next if none match.
func ResolveBrowserBridge() (*BridgeCommand, error) {
	if envPath := strings.TrimSpace(os.Getenv("OAT_BROWSER_AGENT_BRIDGE_PATH")); envPath != "" {
		if _, statErr := os.Stat(envPath); statErr != nil {
			return nil, fmt.Errorf(
				"OAT_BROWSER_AGENT_BRIDGE_PATH=%q is set but does not exist: %w",
				envPath, statErr,
			)
		}
		if strings.HasSuffix(envPath, ".js") || strings.HasSuffix(envPath, ".mjs") {
			return &BridgeCommand{
				Command: "node",
				Args:    []string{envPath},
				Source:  "$OAT_BROWSER_AGENT_BRIDGE_PATH (via node)",
			}, nil
		}
		return &BridgeCommand{
			Command: envPath,
			Source:  "$OAT_BROWSER_AGENT_BRIDGE_PATH",
		}, nil
	}
	if p, lookErr := exec.LookPath("oat-browser-agent"); lookErr == nil {
		return &BridgeCommand{Command: p, Source: "$PATH"}, nil
	}
	home, homeErr := os.UserHomeDir()
	if homeErr == nil {
		bundled := filepath.Join(home, ".oat", "oat-browser-agent", "dist", "bridge", "index.js")
		if _, statErr := os.Stat(bundled); statErr == nil {
			return &BridgeCommand{
				Command: "node",
				Args:    []string{bundled},
				Source:  "~/.oat/oat-browser-agent/ bundle (via node)",
			}, nil
		}
	}
	return nil, fmt.Errorf(
		"oat-browser-agent bridge not found. Install it one of these ways:\n" +
			"  - npm install -g oat-browser-agent (puts `oat-browser-agent` on $PATH)\n" +
			"  - unpack a release tarball into ~/.oat/oat-browser-agent/\n" +
			"  - point $OAT_BROWSER_AGENT_BRIDGE_PATH at a local checkout's\n" +
			"    dist/bridge/index.js (for development)\n" +
			"See https://github.com/Root-IO-Labs/oat-browser-agent for full setup",
	)
}
