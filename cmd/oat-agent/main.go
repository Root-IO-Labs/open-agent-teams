package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Root-IO-Labs/open-agent-teams/internal/version"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "-v", "version":
			info := version.Current()
			fmt.Printf("oat-agent %s\n  commit:  %s\n  built:   %s\n", info.Version, info.Commit, info.Date)
			return
		}
	}
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Find the agent-runtime directory
	agentRuntimeDir, err := findAgentRuntimeDir()
	if err != nil {
		return fmt.Errorf("failed to find agent-runtime directory: %w", err)
	}

	// Find Python executable — prefer the uv venv in agent-runtime which
	// has all deps installed, then fall back to system Python.
	pythonExe, err := findPythonExecutable(agentRuntimeDir)
	if err != nil {
		return fmt.Errorf("failed to find Python executable: %w", err)
	}

	// Build the command to execute the Python CLI
	cmd := buildPythonCommand(pythonExe, agentRuntimeDir, os.Args[1:])

	// Set up the command I/O (env is already configured by buildPythonCommand)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Debug: print the command being executed
	if os.Getenv("OAT_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "Debug: Running command: %v %v\n", cmd.Path, cmd.Args)
		fmt.Fprintf(os.Stderr, "Debug: Working directory: %s\n", cmd.Dir)
		for _, env := range cmd.Env {
			if strings.HasPrefix(env, "PYTHONPATH=") {
				fmt.Fprintf(os.Stderr, "Debug: %s\n", env)
				break
			}
		}
	}

	// Execute the command
	return cmd.Run()
}

// findAgentRuntimeDir locates the agent-runtime directory relative to this binary
func findAgentRuntimeDir() (string, error) {
	// Priority: explicit env var override (useful when binary is in $GOPATH/bin
	// with no agent-runtime nearby)
	if envDir := os.Getenv("OAT_AGENT_RUNTIME_DIR"); envDir != "" {
		if absPath, err := filepath.Abs(envDir); err == nil {
			if isAgentRuntimeDir(absPath) {
				return absPath, nil
			}
		}
		return "", fmt.Errorf("OAT_AGENT_RUNTIME_DIR=%q is not a valid agent-runtime directory", envDir)
	}

	// Get the current working directory
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}

	// Get the directory containing this executable
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}

	exeDir := filepath.Dir(exe)
	// Resolve symlinks so we look next to the real binary (e.g. when oat-agent is in PATH)
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exeDir = filepath.Dir(resolved)
	}

	// Look for agent-runtime: prefer next to the binary so OAT works when cwd is a worktree.
	searchPaths := []string{
		// Next to the binary (when OAT runs oat-agent with cwd=worktree, we still find runtime)
		filepath.Join(exeDir, "agent-runtime"),
		// Current working directory (for development)
		filepath.Join(cwd, "agent-runtime"),
		// If running from project root
		filepath.Join(cwd, "..", "agent-runtime"),
		// If binary is in cmd/oat-agent or bin/
		filepath.Join(exeDir, "..", "agent-runtime"),
		filepath.Join(exeDir, "..", "..", "agent-runtime"),
		filepath.Join(filepath.Dir(exeDir), "agent-runtime"),
	}

	for _, path := range searchPaths {
		if absPath, err := filepath.Abs(path); err == nil {
			if info, err := os.Stat(absPath); err == nil && info.IsDir() {
				// Check if this looks like the agent-runtime directory by looking for key files
				if isAgentRuntimeDir(absPath) {
					return absPath, nil
				}
			}
		}
	}

	return "", fmt.Errorf("agent-runtime directory not found in expected locations")
}

// isAgentRuntimeDir checks if a directory contains the expected agent-runtime structure
func isAgentRuntimeDir(dir string) bool {
	// Check for key files/directories that should exist in agent-runtime
	keyPaths := []string{
		"libs/cli/oat_cli",
		"libs/oat_sdk",
	}

	for _, keyPath := range keyPaths {
		fullPath := filepath.Join(dir, keyPath)
		if _, err := os.Stat(fullPath); err != nil {
			return false
		}
	}
	return true
}

// findPythonExecutable finds a suitable Python executable.
// It prefers the uv venv in agent-runtime/libs/cli/.venv which has all
// dependencies pre-installed via `uv sync`, then falls back to system Python.
func findPythonExecutable(agentRuntimeDir string) (string, error) {
	// Build candidate list: venv first (has all deps), then system Python
	candidates := []string{}

	// Highest priority: uv venv created by `uv sync` in libs/cli
	if agentRuntimeDir != "" {
		candidates = append(candidates,
			filepath.Join(agentRuntimeDir, "libs", "cli", ".venv", "bin", "python"),
			filepath.Join(agentRuntimeDir, "libs", "cli", ".venv", "bin", "python3"),
		)
	}

	// Fall back to system Python
	candidates = append(candidates,
		"/opt/anaconda3/bin/python", // Common anaconda location
		"/opt/anaconda3/bin/python3",
		"python",  // Usually points to conda python if activated
		"python3", // System python3
	)

	for _, candidate := range candidates {
		var path string
		var err error

		// If it's an absolute path, check directly
		if filepath.IsAbs(candidate) {
			if _, statErr := os.Stat(candidate); statErr == nil {
				path = candidate
			} else {
				continue
			}
		} else {
			// Use LookPath for relative names
			path, err = exec.LookPath(candidate)
			if err != nil {
				continue
			}
		}

		// Verify it's Python 3.11+ and has required packages
		if isPythonVersionSupported(path) && hasRequiredPackages(path) {
			return path, nil
		}
	}

	return "", fmt.Errorf("no suitable Python executable found (requires Python 3.11+ with agent-runtime CLI dependencies)")
}

// isPythonVersionSupported checks if the Python version is 3.11 or higher
func isPythonVersionSupported(pythonPath string) bool {
	cmd := exec.Command(pythonPath, "-c", "import sys; print(f'{sys.version_info.major}.{sys.version_info.minor}')")
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	version := strings.TrimSpace(string(output))
	// Simple version check - we need at least 3.11
	supportedVersions := []string{"3.11", "3.12", "3.13", "3.14"}
	for _, supported := range supportedVersions {
		if strings.HasPrefix(version, supported) {
			return true
		}
	}
	return false
}

// hasRequiredPackages checks if the required CLI dependencies are available
func hasRequiredPackages(pythonPath string) bool {
	// Check for the required packages that the CLI checks for
	packages := []string{"requests", "dotenv", "tavily", "textual"}

	for _, pkg := range packages {
		checkCode := fmt.Sprintf("import importlib.util; import sys; sys.exit(0 if importlib.util.find_spec('%s') else 1)", pkg)
		cmd := exec.Command(pythonPath, "-c", checkCode)
		if err := cmd.Run(); err != nil {
			return false
		}
	}
	return true
}

// buildPythonCommand constructs the command to run the Python CLI.
//
// Uses `python -m oat_cli` (invoking the package via its `__main__.py`)
// rather than `python -m oat_cli.main`. Running the submodule directly
// causes CPython to emit a `RuntimeWarning: 'oat_cli.main' found in
// sys.modules after import of package 'oat_cli', but prior to execution
// of 'oat_cli.main'; this may result in unpredictable behavior` on
// every invocation — including trivial ones like `oat-agent --help` — because
// the package's `__init__.py` imports `oat_cli.main` eagerly. Invoking
// the package instead lets the runtime handle double-registration cleanly.
func buildPythonCommand(pythonExe, agentRuntimeDir string, args []string) *exec.Cmd {
	validPath := regexp.MustCompile(`^[a-zA-Z0-9_\-\./\\]+$`)
	if !validPath.MatchString(pythonExe) {
		return nil
	}
	// The command will be: python -m oat_cli [args...]
	cmdArgs := []string{"-m", "oat_cli"}
	cmdArgs = append(cmdArgs, args...)

	cmd := exec.Command(pythonExe, cmdArgs...)

	// Inherit the caller's working directory so agents start in their worktree.
	// Module resolution is handled by PYTHONPATH below.

	// Add the agent-runtime directory to PYTHONPATH
	pythonPath := os.Getenv("PYTHONPATH")
	if pythonPath != "" {
		pythonPath = agentRuntimeDir + string(os.PathListSeparator) + pythonPath
	} else {
		pythonPath = agentRuntimeDir
	}

	// Add libs directories to PYTHONPATH for proper module resolution
	libsDirs := []string{
		filepath.Join(agentRuntimeDir, "libs", "cli"),
		filepath.Join(agentRuntimeDir, "libs", "oat_sdk"),
	}

	for _, libDir := range libsDirs {
		pythonPath = libDir + string(os.PathListSeparator) + pythonPath
	}

	// Set the environment
	env := os.Environ()
	pythonPathSet := false
	for i, envVar := range env {
		if strings.HasPrefix(envVar, "PYTHONPATH=") {
			env[i] = "PYTHONPATH=" + pythonPath
			pythonPathSet = true
			break
		}
	}
	if !pythonPathSet {
		env = append(env, "PYTHONPATH="+pythonPath)
	}

	cmd.Env = env

	return cmd
}
