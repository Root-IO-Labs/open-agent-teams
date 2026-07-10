package cli

import (
	"strings"
	"testing"
)

// TestBuildWrapperScript pins the launchd env-inheritance fix (Phase 7 edge
// case): the wrapper MUST re-enter a login shell (`-lc`) so the daemon gets the
// user's PATH / API keys / node/git/gh, and it must exec `oat daemon _run`.
func TestBuildWrapperScript(t *testing.T) {
	got := buildWrapperScript("/bin/zsh", "/usr/local/bin/oat")

	if !strings.HasPrefix(got, "#!/bin/sh") {
		t.Errorf("wrapper must start with a shebang; got:\n%s", got)
	}
	if !strings.Contains(got, "-lc") {
		t.Errorf("wrapper must use a login shell (-lc) to inherit env; got:\n%s", got)
	}
	if !strings.Contains(got, "'/bin/zsh'") {
		t.Errorf("wrapper must invoke the user's shell quoted; got:\n%s", got)
	}
	if !strings.Contains(got, `"/usr/local/bin/oat" daemon _run`) {
		t.Errorf("wrapper must exec `oat daemon _run`; got:\n%s", got)
	}
}

// TestBuildWrapperScript_ShellQuoting guards against command injection via a
// pathological SHELL value with a single quote.
func TestBuildWrapperScript_ShellQuoting(t *testing.T) {
	got := buildWrapperScript("/weird/sh'; rm -rf ~ #", "/usr/local/bin/oat")
	if strings.Contains(got, "rm -rf ~ #\n") && !strings.Contains(got, `'\''`) {
		t.Errorf("shell path with a quote was not safely escaped; got:\n%s", got)
	}
}

// TestBuildLaunchdPlist pins the hardening knobs the plan requires: crash-only
// KeepAlive (SuccessfulExit:false), a 30s throttle, the reverse-DNS label, and
// that NO secrets/EnvironmentVariables are baked into the plist.
func TestBuildLaunchdPlist(t *testing.T) {
	got := buildLaunchdPlist("/Users/j/.oat/oat-daemon-service.sh", "/Users/j/.oat/daemon.log", "/Users/j/.oat")

	mustContain := []string{
		"<string>io.oat.daemon</string>",
		"<key>KeepAlive</key>",
		"<key>SuccessfulExit</key>",
		"<false/>",
		"<key>ThrottleInterval</key>",
		"<integer>30</integer>",
		"/Users/j/.oat/oat-daemon-service.sh",
	}
	for _, s := range mustContain {
		if !strings.Contains(got, s) {
			t.Errorf("plist missing %q:\n%s", s, got)
		}
	}
	// The plist must NOT hardcode env vars — those are sourced by the wrapper.
	if strings.Contains(got, "EnvironmentVariables") {
		t.Errorf("plist must not hardcode EnvironmentVariables (secrets/PATH belong in the wrapper):\n%s", got)
	}
}
