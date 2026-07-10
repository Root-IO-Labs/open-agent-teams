package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// launchd LaunchAgent integration (Phase 7 item 2). This is an OPT-IN OS
// supervisor: `oat daemon start`/`stop`/`nuke` keep working exactly as before
// (now with the always-on Setsid detach fix). `oat daemon install-service`
// hands liveness to launchd (auto-start at login + auto-restart on crash);
// `oat daemon uninstall-service` reverses it. Rationale + edge cases are in the
// plan's "Phase 7 — How install-service coexists" and "Edge cases (launchd)".
//
// Two edges this code is built around:
//   - launchd does NOT inherit the shell env (minimal PATH, no API keys, no
//     node/git/gh). So we do NOT hardcode EnvironmentVariables in the plist;
//     instead ProgramArguments points at a generated wrapper that sources the
//     user's login shell (`$SHELL -lc`) and then execs `oat daemon _run`.
//   - Duplicate-daemon safety is already handled by the PID-file guard
//     (daemon.Start -> pidFile.CheckAndClaim): a second `_run` (launchd's or a
//     stray manual start) fails to claim and exits, so the two can't coexist.

const (
	launchdLabel      = "io.oat.daemon"
	launchdHintMarker = ".launchd-hint-shown"
)

// launchdPlistPath returns ~/Library/LaunchAgents/io.oat.daemon.plist.
func launchdPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
}

// launchdWrapperPath returns the generated wrapper script path under ~/.oat.
func (c *CLI) launchdWrapperPath() string {
	return filepath.Join(c.paths.Root, "oat-daemon-service.sh")
}

// launchdServiceInstalled reports whether the LaunchAgent plist exists.
func launchdServiceInstalled() bool {
	p, err := launchdPlistPath()
	if err != nil {
		return false
	}
	_, statErr := os.Stat(p)
	return statErr == nil
}

// installDaemonService writes the wrapper + plist and bootstraps the
// LaunchAgent so launchd supervises the daemon.
func (c *CLI) installDaemonService(args []string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("oat daemon install-service is macOS-only (launchd); on Linux use a systemd user unit running `oat daemon _run`")
	}
	if os.Geteuid() == 0 {
		return fmt.Errorf("refusing to install the LaunchAgent as root: run as your normal user so the daemon inherits your login environment and API keys")
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to resolve oat binary path: %w", err)
	}
	if abs, absErr := filepath.Abs(exe); absErr == nil {
		exe = abs
	}

	if err := c.paths.EnsureDirectories(); err != nil {
		return fmt.Errorf("failed to ensure ~/.oat exists: %w", err)
	}

	// 1) Wrapper script: source the login shell env, then exec the daemon in
	//    the foreground for launchd to supervise. `-lc` makes it a login shell
	//    so PATH / version-manager shims / API keys / GH_TOKEN are present.
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/zsh"
	}
	wrapperPath := c.launchdWrapperPath()
	wrapper := buildWrapperScript(shell, exe)
	if err := os.WriteFile(wrapperPath, []byte(wrapper), 0o755); err != nil {
		return fmt.Errorf("failed to write wrapper script %s: %w", wrapperPath, err)
	}

	// 2) plist. KeepAlive{SuccessfulExit:false} → relaunch on crash but NOT on
	//    a clean `exit 0` stop. KeepAlive implies RunAtLoad. ThrottleInterval
	//    30s so an OOM crash-loop doesn't relaunch every 10s (the default).
	plistPath, err := launchdPlistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return fmt.Errorf("failed to create LaunchAgents dir: %w", err)
	}
	plist := buildLaunchdPlist(wrapperPath, c.paths.DaemonLog, c.paths.Root)
	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("failed to write plist %s: %w", plistPath, err)
	}

	// 3) (Re)bootstrap into the per-user GUI domain. bootout first so a repeat
	//    install is idempotent (bootstrap fails if already loaded).
	domain := "gui/" + strconv.Itoa(os.Getuid())
	_ = runLaunchctl("bootout", domain+"/"+launchdLabel) // ignore: not-loaded is fine
	if out, err := runLaunchctlOutput("bootstrap", domain, plistPath); err != nil {
		return fmt.Errorf("launchctl bootstrap failed: %w\n%s", err, out)
	}

	fmt.Printf("Installed and started the OAT daemon LaunchAgent (%s).\n", launchdLabel)
	fmt.Println("launchd now owns the daemon's liveness: it starts at login and restarts on crash.")
	fmt.Println()
	fmt.Println("Note: because launchd supervises it now, `oat daemon stop` will halt the")
	fmt.Println("process but launchd may immediately relaunch it. To stop it for good, run:")
	fmt.Println("  oat daemon uninstall-service")
	return nil
}

// uninstallDaemonService boots the LaunchAgent out and removes the files.
func (c *CLI) uninstallDaemonService(args []string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("oat daemon uninstall-service is macOS-only (launchd)")
	}
	plistPath, err := launchdPlistPath()
	if err != nil {
		return err
	}

	domain := "gui/" + strconv.Itoa(os.Getuid())
	_ = runLaunchctl("bootout", domain+"/"+launchdLabel) // ignore: may not be loaded

	var removedAny bool
	if err := os.Remove(plistPath); err == nil {
		removedAny = true
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove plist %s: %w", plistPath, err)
	}
	if err := os.Remove(c.launchdWrapperPath()); err == nil {
		removedAny = true
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove wrapper %s: %w", c.launchdWrapperPath(), err)
	}

	if !removedAny {
		fmt.Println("No OAT LaunchAgent was installed (nothing to remove).")
		return nil
	}
	fmt.Println("Uninstalled the OAT daemon LaunchAgent.")
	fmt.Println("The daemon (if running) keeps running until you `oat daemon stop`.")
	return nil
}

// maybePrintLaunchdHint prints a one-time recommendation to install the
// LaunchAgent supervisor when the daemon is started manually and unsupervised.
// It's a best-effort convenience: any error (e.g. marker write) is swallowed so
// it never affects `oat daemon start`.
func (c *CLI) maybePrintLaunchdHint() {
	if runtime.GOOS != "darwin" {
		return
	}
	if launchdServiceInstalled() {
		return
	}
	marker := filepath.Join(c.paths.Root, launchdHintMarker)
	if _, err := os.Stat(marker); err == nil {
		return // already shown once
	}
	fmt.Println()
	fmt.Println("Tip: for auto-restart on crash and start-at-login, install the launchd supervisor:")
	fmt.Println("  oat daemon install-service")
	fmt.Println("(optional; `oat daemon start` already survives the terminal closing.)")
	_ = os.WriteFile(marker, []byte("1\n"), 0o644)
}

// buildWrapperScript returns the /bin/sh wrapper that launchd's plist points
// at. It re-enters the user's login shell so PATH / API keys / node/git/gh /
// version-manager shims are present (launchd itself provides none of these),
// then execs the daemon in the foreground for launchd to supervise. Secrets
// are intentionally sourced here (not the plist) so they never sit
// world-readable in ~/Library/LaunchAgents.
func buildWrapperScript(shell, exe string) string {
	return fmt.Sprintf(`#!/bin/sh
# Generated by `+"`oat daemon install-service`"+`. Do not edit by hand.
# launchd runs with a minimal environment (no PATH, no API keys, no node/git/gh),
# so we re-enter your login shell to load them, then run the daemon in the
# foreground. Do NOT put secrets in the plist — they'd be world-readable in
# ~/Library/LaunchAgents; they come from your shell env here instead.
exec %s -lc 'exec "%s" daemon _run'
`, shellQuote(shell), exe)
}

// buildLaunchdPlist returns the LaunchAgent plist XML. KeepAlive
// {SuccessfulExit:false} relaunches on crash but not on a clean `exit 0` stop;
// ThrottleInterval 30s prevents an OOM crash-loop from relaunching every 10s.
func buildLaunchdPlist(wrapperPath, logPath, workDir string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
	</array>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>30</integer>
	<key>ProcessType</key>
	<string>Background</string>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
	<key>WorkingDirectory</key>
	<string>%s</string>
</dict>
</plist>
`, launchdLabel, wrapperPath, logPath, logPath, workDir)
}

// runLaunchctl runs `launchctl <args...>` discarding output.
func runLaunchctl(args ...string) error {
	return exec.Command("launchctl", args...).Run()
}

// runLaunchctlOutput runs `launchctl <args...>` returning combined output.
func runLaunchctlOutput(args ...string) (string, error) {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	return string(out), err
}

// shellQuote single-quotes a string for safe embedding in a /bin/sh command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
