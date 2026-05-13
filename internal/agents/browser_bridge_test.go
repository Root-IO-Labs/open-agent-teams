package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveBrowserBridge_EnvJSScript verifies that a *.js path in
// OAT_BROWSER_AGENT_BRIDGE_PATH is invoked through `node`. This is the
// path a developer uses against a local oat-browser-agent checkout.
func TestResolveBrowserBridge_EnvJSScript(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "bridge.js")
	if err := os.WriteFile(scriptPath, []byte("// stub"), 0644); err != nil {
		t.Fatalf("write stub script: %v", err)
	}
	t.Setenv("OAT_BROWSER_AGENT_BRIDGE_PATH", scriptPath)

	bridge, err := ResolveBrowserBridge()
	if err != nil {
		t.Fatalf("expected resolution to succeed, got: %v", err)
	}
	if bridge.Command != "node" {
		t.Fatalf("command = %q, want 'node' for .js extension", bridge.Command)
	}
	if len(bridge.Args) != 1 || bridge.Args[0] != scriptPath {
		t.Fatalf("args = %v, want [%q]", bridge.Args, scriptPath)
	}
	if !strings.Contains(bridge.Source, "OAT_BROWSER_AGENT_BRIDGE_PATH") {
		t.Fatalf("source = %q, must mention env var", bridge.Source)
	}
}

// TestResolveBrowserBridge_EnvExecutable verifies that a non-.js path
// in OAT_BROWSER_AGENT_BRIDGE_PATH is treated as a direct executable
// (the npm-installed `oat-browser-agent` shim case when set by hand).
func TestResolveBrowserBridge_EnvExecutable(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "oat-browser-agent")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("write stub binary: %v", err)
	}
	t.Setenv("OAT_BROWSER_AGENT_BRIDGE_PATH", binPath)

	bridge, err := ResolveBrowserBridge()
	if err != nil {
		t.Fatalf("expected resolution to succeed, got: %v", err)
	}
	if bridge.Command != binPath {
		t.Fatalf("command = %q, want %q (executable path)", bridge.Command, binPath)
	}
	if len(bridge.Args) != 0 {
		t.Fatalf("args = %v, want empty for executable path", bridge.Args)
	}
}

// TestResolveBrowserBridge_EnvMissing verifies that a non-existent path
// in OAT_BROWSER_AGENT_BRIDGE_PATH returns a structured error mentioning
// the bad path (so users know exactly what they typed wrong).
func TestResolveBrowserBridge_EnvMissing(t *testing.T) {
	t.Setenv("OAT_BROWSER_AGENT_BRIDGE_PATH", "/no/such/bridge.js")

	_, err := ResolveBrowserBridge()
	if err == nil {
		t.Fatal("expected error for non-existent env path, got nil")
	}
	if !strings.Contains(err.Error(), "/no/such/bridge.js") {
		t.Fatalf("error must mention the bad path, got: %v", err)
	}
}

// TestResolveBrowserBridge_BundledFallback covers the
// ~/.oat/oat-browser-agent/dist/bridge/index.js installation path. We
// can't easily test the PATH-lookup case without a real
// `oat-browser-agent` binary on $PATH, but the bundled path is a
// deterministic file probe we can simulate by overriding $HOME.
func TestResolveBrowserBridge_BundledFallback(t *testing.T) {
	t.Setenv("OAT_BROWSER_AGENT_BRIDGE_PATH", "")
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	// Also clear PATH-found shim so the second probe step misses and
	// we fall through to the bundle path. Setting PATH to an empty dir
	// is sufficient on all our supported platforms.
	t.Setenv("PATH", t.TempDir())

	bundled := filepath.Join(tmpHome, ".oat", "oat-browser-agent", "dist", "bridge", "index.js")
	if err := os.MkdirAll(filepath.Dir(bundled), 0755); err != nil {
		t.Fatalf("mkdir bundled dir: %v", err)
	}
	if err := os.WriteFile(bundled, []byte("// stub"), 0644); err != nil {
		t.Fatalf("write bundled stub: %v", err)
	}

	bridge, err := ResolveBrowserBridge()
	if err != nil {
		t.Fatalf("expected bundled-fallback to succeed, got: %v", err)
	}
	if bridge.Command != "node" || len(bridge.Args) != 1 || bridge.Args[0] != bundled {
		t.Fatalf("expected `node %s`, got command=%q args=%v", bundled, bridge.Command, bridge.Args)
	}
}

// TestResolveBrowserBridge_NotFoundErrorIsActionable verifies that when
// none of the probes hit, the error message lists every install option
// so the user knows what to do. This is the most important UX surface
// of this whole flow -- the failure mode users will see first.
func TestResolveBrowserBridge_NotFoundErrorIsActionable(t *testing.T) {
	t.Setenv("OAT_BROWSER_AGENT_BRIDGE_PATH", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())

	_, err := ResolveBrowserBridge()
	if err == nil {
		t.Fatal("expected not-found error, got nil")
	}
	msg := err.Error()
	for _, hint := range []string{"npm install", "OAT_BROWSER_AGENT_BRIDGE_PATH", "~/.oat/oat-browser-agent"} {
		if !strings.Contains(msg, hint) {
			t.Errorf("not-found error must mention %q for users to act on; full message:\n%s", hint, msg)
		}
	}
}
