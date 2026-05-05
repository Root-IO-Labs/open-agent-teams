// Package preflight runs a set of environment checks that catch the most
// common first-run failures before a user hits a cryptic error inside an
// agent log. Each check is a small, self-contained function that returns a
// CheckResult describing what was tested, whether it passed, and what to
// do if it did not.
//
// ## Deliberately out of scope (consider later)
//
// Each bullet is a design decision the initial version chose NOT to take.
// Grouped here so future contributors see the contract at a glance.
//
//  1. `oat doctor` is NOT wired into `oat init` automatically.
//     Reason: running doctor inside init modifies a working path and risks
//     regression on the single most-used first-run flow. Users invoke
//     doctor explicitly when they want preflight. A later change can
//     gate init on doctor results behind a flag.
//
//  2. No `oat doctor --fix` auto-repair mode.
//     Reason: auto-install of Go / gh / API keys is a security and UX
//     risk (writing credentials, running installers, mutating shell rc).
//     Doctor is read-only diagnostics. A --fix mode could ship later
//     with a strict allowlist of safe operations.
//
//  3. No preflight on every CLI command.
//     Reason: would add startup latency to every `oat` invocation. Opt-in
//     via `oat doctor` is the right default.
//
//  4. `doctor` is NOT in the humanOnlyCLICommands trim list in cli.go.
//     Reason: it is safe to let agents see the doctor command in their
//     CLI reference; the cost is marginal and excluding it is an
//     optimisation, not a correctness fix.
//
//  5. Python check does not downgrade to WARN when an existing venv at
//     `agent-runtime/libs/cli/.venv` is detected.
//     Reason: a stale venv does not prove the system can still rebuild.
//     FAIL on below-minimum system Python is the honest signal. A future
//     version can add a venv-aware mode after reviewing the install flow.
//
//  6. No color or lipgloss styling in the text output.
//     Reason: plain ASCII OK/WARN/FAIL is universal, scriptable, and
//     does not break CI log parsers. Color can be added opt-in via an
//     env var later.
//
//  7. Checks run sequentially, not in parallel.
//     Reason: 7 checks complete in ~100 ms. Goroutine complexity costs
//     more than the ~70 ms it would save. If check count grows past ~20
//     or network checks get added, revisit.
package preflight

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/Root-IO-Labs/open-agent-teams/pkg/config"
)

// Status is the outcome of a single preflight check.
type Status string

const (
	// StatusOK means the check passed and OAT can rely on it.
	StatusOK Status = "OK"
	// StatusWarn means the check found something worth noting but OAT can
	// still start. Running without the daemon attached is an example.
	StatusWarn Status = "WARN"
	// StatusFail means OAT will almost certainly not work end-to-end
	// until the user fixes this.
	StatusFail Status = "FAIL"
)

// CheckResult is the outcome of one preflight check.
type CheckResult struct {
	Name    string `json:"name"`
	Status  Status `json:"status"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// Minimum supported tool versions. Match scripts/install.sh.
const (
	minGoMajor     = 1
	minGoMinor     = 24
	minGoPatch     = 2
	minPythonMajor = 3
	minPythonMinor = 11
)

// KeyProviders is the set of environment variable names that any of the
// supported LLM providers would read. If none is set and the global
// ~/.oat/.env does not supply one, LLM-backed agents will fail auth.
// Kept in the same order as docs/SUPPORTED_LLM_PROVIDERS.md for clarity.
var KeyProviders = []string{
	"ANTHROPIC_API_KEY",
	"OPENAI_API_KEY",
	"GOOGLE_API_KEY",
	"GOOGLE_CLOUD_PROJECT",
	"AZURE_OPENAI_API_KEY",
	"DEEPSEEK_API_KEY",
	"OPENROUTER_API_KEY",
	"GROQ_API_KEY",
	"MISTRAL_API_KEY",
	"FIREWORKS_API_KEY",
	"TOGETHER_API_KEY",
	"NVIDIA_API_KEY",
	"PPLX_API_KEY",
	"XAI_API_KEY",
	"COHERE_API_KEY",
	"HUGGINGFACEHUB_API_TOKEN",
	"WATSONX_APIKEY",
}

// Test hooks. Package-level vars so tests can replace the side-effecting
// calls with deterministic fixtures. Real code never calls these directly;
// the wrapper functions below do.
var (
	goVersionCmd   = func() ([]byte, error) { return exec.Command("go", "version").Output() }
	pythonVerCmd   = pythonVersionOutput
	ghVersionCmd   = func() ([]byte, error) { return exec.Command("gh", "--version").Output() }
	ghAuthCmd      = func() error { return exec.Command("gh", "auth", "status").Run() }
	oatAgentLookup = func() (string, error) { return exec.LookPath("oat-agent") }
	findProcess    = os.FindProcess
)

// Run executes every preflight check in a stable order. The slice order is
// the display order; tests assert on it.
//
// paths may be nil; checks that need a path fall back to user-home based
// defaults so `oat doctor` still gives useful output before `oat start`
// has ever been run.
func Run(paths *config.Paths) []CheckResult {
	return []CheckResult{
		checkGo(),
		checkPython(),
		checkGhCLI(),
		checkOatAgent(),
		checkLLMKey(paths),
		checkOatDir(paths),
		checkDaemon(paths),
	}
}

// Summarize counts how many checks landed at each status.
func Summarize(results []CheckResult) (ok, warn, fail int) {
	for _, r := range results {
		switch r.Status {
		case StatusOK:
			ok++
		case StatusWarn:
			warn++
		case StatusFail:
			fail++
		}
	}
	return
}

// ---------------------------------------------------------------------------
// Version parsing helpers. Pure, exhaustively testable.
// ---------------------------------------------------------------------------

var goVersionRE = regexp.MustCompile(`go(\d+)\.(\d+)(?:\.(\d+))?`)

// parseGoVersion extracts (major, minor, patch) from `go version` output.
// Returns ok=false if the output is not recognizable.
// Examples it must handle:
//
//	"go version go1.24.2 darwin/arm64"
//	"go version go1.26 linux/amd64"
//	"go version go1.24.2rc1 darwin/arm64"
func parseGoVersion(output string) (major, minor, patch int, ok bool) {
	m := goVersionRE.FindStringSubmatch(output)
	if m == nil {
		return 0, 0, 0, false
	}
	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	if len(m) >= 4 && m[3] != "" {
		patch, _ = strconv.Atoi(m[3])
	}
	return major, minor, patch, true
}

var pythonVersionRE = regexp.MustCompile(`(?i)python\s+(\d+)\.(\d+)`)

// parsePythonVersion extracts (major, minor) from `python --version` output.
// Examples: "Python 3.11.1", "Python 3.12".
func parsePythonVersion(output string) (major, minor int, ok bool) {
	m := pythonVersionRE.FindStringSubmatch(output)
	if m == nil {
		return 0, 0, false
	}
	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	return major, minor, true
}

// atLeast returns true when (have) is >= (need) in lexicographic order.
// Both must be the same length.
func atLeast(have, need []int) bool {
	for i := range need {
		if have[i] > need[i] {
			return true
		}
		if have[i] < need[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Individual checks.
// ---------------------------------------------------------------------------

func checkGo() CheckResult {
	out, err := goVersionCmd()
	if err != nil {
		return CheckResult{
			Name:    "Go toolchain",
			Status:  StatusFail,
			Message: "go command not found",
			Hint:    "Install Go " + minGoVersionString() + "+ from https://go.dev/dl/",
		}
	}
	major, minor, patch, ok := parseGoVersion(string(out))
	if !ok {
		return CheckResult{
			Name:    "Go toolchain",
			Status:  StatusWarn,
			Message: "could not parse version from: " + strings.TrimSpace(string(out)),
			Hint:    "Expected format 'go version goX.Y.Z ...'. Upgrade if unexpected.",
		}
	}
	if !atLeast([]int{major, minor, patch}, []int{minGoMajor, minGoMinor, minGoPatch}) {
		return CheckResult{
			Name:    "Go toolchain",
			Status:  StatusFail,
			Message: goVersionString(major, minor, patch) + " is below required " + minGoVersionString(),
			Hint:    "Upgrade Go from https://go.dev/dl/",
		}
	}
	return CheckResult{
		Name:    "Go toolchain",
		Status:  StatusOK,
		Message: goVersionString(major, minor, patch),
	}
}

func checkPython() CheckResult {
	output, err := pythonVerCmd()
	if err != nil {
		return CheckResult{
			Name:    "Python runtime",
			Status:  StatusFail,
			Message: "python3 and python both not found",
			Hint:    "Install Python " + minPythonVersionString() + "+ from https://www.python.org/downloads/",
		}
	}
	major, minor, ok := parsePythonVersion(output)
	if !ok {
		return CheckResult{
			Name:    "Python runtime",
			Status:  StatusWarn,
			Message: "could not parse version from: " + strings.TrimSpace(output),
		}
	}
	if !atLeast([]int{major, minor}, []int{minPythonMajor, minPythonMinor}) {
		return CheckResult{
			Name:    "Python runtime",
			Status:  StatusFail,
			Message: pythonVersionString(major, minor) + " is below required " + minPythonVersionString(),
			Hint:    "Install a newer Python from https://www.python.org/downloads/",
		}
	}
	return CheckResult{
		Name:    "Python runtime",
		Status:  StatusOK,
		Message: pythonVersionString(major, minor),
	}
}

func checkGhCLI() CheckResult {
	if _, err := ghVersionCmd(); err != nil {
		return CheckResult{
			Name:    "GitHub CLI",
			Status:  StatusFail,
			Message: "gh command not found",
			Hint:    "Install gh: brew install gh   OR   https://cli.github.com",
		}
	}
	if err := ghAuthCmd(); err != nil {
		return CheckResult{
			Name:    "GitHub CLI",
			Status:  StatusFail,
			Message: "gh is installed but not authenticated",
			Hint:    "Run: gh auth login",
		}
	}
	return CheckResult{
		Name:    "GitHub CLI",
		Status:  StatusOK,
		Message: "installed and authenticated",
	}
}

func checkOatAgent() CheckResult {
	path, err := oatAgentLookup()
	if err != nil {
		return CheckResult{
			Name:    "oat-agent binary",
			Status:  StatusFail,
			Message: "oat-agent not on PATH",
			Hint:    "Run ./scripts/install.sh from the repo, or make sure $(go env GOPATH)/bin is on PATH",
		}
	}
	return CheckResult{
		Name:    "oat-agent binary",
		Status:  StatusOK,
		Message: path,
	}
}

// checkLLMKey returns OK if at least one recognized provider key is set
// either in the process environment or in paths.Root/.env. If paths is
// nil the .env lookup is skipped.
func checkLLMKey(paths *config.Paths) CheckResult {
	if envKey := firstSetEnv(KeyProviders); envKey != "" {
		return CheckResult{
			Name:    "LLM API key",
			Status:  StatusOK,
			Message: envKey + " set in environment",
		}
	}
	if envPath := dotEnvPath(paths); envPath != "" {
		if envKey, found := firstKeyInEnvFile(envPath, KeyProviders); found {
			return CheckResult{
				Name:    "LLM API key",
				Status:  StatusOK,
				Message: envKey + " set in " + envPath,
			}
		}
	}
	return CheckResult{
		Name:    "LLM API key",
		Status:  StatusFail,
		Message: "no LLM provider key found in environment or ~/.oat/.env",
		Hint:    "mkdir -p ~/.oat && echo 'ANTHROPIC_API_KEY=sk-ant-...' >> ~/.oat/.env  (or set any of: OPENAI_API_KEY, GOOGLE_API_KEY, OPENROUTER_API_KEY, ...)",
	}
}

// checkOatDir returns OK if ~/.oat/ exists and is writable, or if we can
// create it. Fail if HOME is unknown or we cannot write a probe file.
func checkOatDir(paths *config.Paths) CheckResult {
	root := oatRoot(paths)
	if root == "" {
		return CheckResult{
			Name:    "~/.oat directory",
			Status:  StatusFail,
			Message: "cannot resolve user home directory",
			Hint:    "Set $HOME or pass --oat-root",
		}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return CheckResult{
			Name:    "~/.oat directory",
			Status:  StatusFail,
			Message: "cannot create " + root + ": " + err.Error(),
			Hint:    "Check filesystem permissions on your home directory",
		}
	}
	probe := filepath.Join(root, ".oat-doctor-probe")
	if err := os.WriteFile(probe, []byte("probe"), 0o600); err != nil {
		return CheckResult{
			Name:    "~/.oat directory",
			Status:  StatusFail,
			Message: root + " is not writable: " + err.Error(),
			Hint:    "chmod u+w " + root,
		}
	}
	_ = os.Remove(probe)
	return CheckResult{
		Name:    "~/.oat directory",
		Status:  StatusOK,
		Message: root + " is writable",
	}
}

// checkDaemon inspects the PID file and signals the process to confirm
// liveness. A missing daemon is a WARN (the user can start it) not a FAIL.
func checkDaemon(paths *config.Paths) CheckResult {
	pidFile := daemonPIDPath(paths)
	if pidFile == "" {
		return CheckResult{
			Name:    "Daemon",
			Status:  StatusWarn,
			Message: "daemon pid path unknown (no paths configured)",
			Hint:    "Run `oat start` after installing",
		}
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return CheckResult{
			Name:    "Daemon",
			Status:  StatusWarn,
			Message: "not running",
			Hint:    "Run: oat start",
		}
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return CheckResult{
			Name:    "Daemon",
			Status:  StatusWarn,
			Message: "pid file is malformed: " + strings.TrimSpace(string(data)),
			Hint:    "Run: oat daemon restart",
		}
	}
	proc, err := findProcess(pid)
	if err != nil || proc == nil {
		return CheckResult{
			Name:    "Daemon",
			Status:  StatusWarn,
			Message: "pid " + strconv.Itoa(pid) + " not found",
			Hint:    "Run: oat start",
		}
	}
	// Signal 0 is the standard Unix liveness probe. The nil signal
	// returned by os.Signal(nil) is rejected by Darwin's kill(2), so
	// pass syscall.Signal(0) directly.
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return CheckResult{
			Name:    "Daemon",
			Status:  StatusWarn,
			Message: "pid " + strconv.Itoa(pid) + " is not reachable: " + err.Error(),
			Hint:    "Run: oat start",
		}
	}
	return CheckResult{
		Name:    "Daemon",
		Status:  StatusOK,
		Message: "running (pid " + strconv.Itoa(pid) + ")",
	}
}

// ---------------------------------------------------------------------------
// Small utilities kept separate so tests can assert on them directly.
// ---------------------------------------------------------------------------

// pythonVersionOutput returns the version string from python3 (preferred)
// or python (fallback). Returns an error only if neither binary is present
// or both fail to report a version.
func pythonVersionOutput() (string, error) {
	for _, bin := range []string{"python3", "python"} {
		cmd := exec.CommandContext(context.TODO(), bin, "--version")
		// Some Python builds print to stderr; CombinedOutput covers both.
		out, err := cmd.CombinedOutput()
		if err == nil && len(out) > 0 {
			return string(out), nil
		}
	}
	return "", exec.ErrNotFound
}

// HasAnyLLMKey returns true if any supported provider API key is set
// (via env or `~/.oat/.env`). Exposed for callers like `oat init` that
// need a fast boolean check without running the full preflight. Matches
// the detection semantics of checkLLMKey so `oat doctor` and `oat init`
// never disagree on whether a key is configured.
func HasAnyLLMKey(paths *config.Paths) bool {
	if firstSetEnv(KeyProviders) != "" {
		return true
	}
	if envPath := dotEnvPath(paths); envPath != "" {
		if _, found := firstKeyInEnvFile(envPath, KeyProviders); found {
			return true
		}
	}
	return false
}

// firstSetEnv returns the first name in candidates whose env var is
// non-empty, or "" if none are set.
func firstSetEnv(candidates []string) string {
	for _, name := range candidates {
		if os.Getenv(name) != "" {
			return name
		}
	}
	return ""
}

// firstKeyInEnvFile looks for `KEY=...` lines matching any candidate.
// Returns the first key found (in file order) so users see the same name
// the file uses. Lines starting with '#' are skipped. Missing file is not
// an error; returns (_, false).
func firstKeyInEnvFile(path string, candidates []string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()

	candidateSet := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		candidateSet[c] = true
	}

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		if candidateSet[key] {
			value := strings.TrimSpace(line[eq+1:])
			if value != "" {
				return key, true
			}
		}
	}
	return "", false
}

// oatRoot returns the root OAT directory. Prefers paths.Root; falls back
// to ~/.oat so `oat doctor` works before any daemon state exists.
func oatRoot(paths *config.Paths) string {
	if paths != nil && paths.Root != "" {
		return paths.Root
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".oat")
}

func dotEnvPath(paths *config.Paths) string {
	root := oatRoot(paths)
	if root == "" {
		return ""
	}
	return filepath.Join(root, ".env")
}

func daemonPIDPath(paths *config.Paths) string {
	if paths != nil && paths.DaemonPID != "" {
		return paths.DaemonPID
	}
	root := oatRoot(paths)
	if root == "" {
		return ""
	}
	return filepath.Join(root, "daemon.pid")
}

func goVersionString(major, minor, patch int) string {
	return "go" + strconv.Itoa(major) + "." + strconv.Itoa(minor) + "." + strconv.Itoa(patch)
}

func minGoVersionString() string {
	return goVersionString(minGoMajor, minGoMinor, minGoPatch)
}

func pythonVersionString(major, minor int) string {
	return "Python " + strconv.Itoa(major) + "." + strconv.Itoa(minor)
}

func minPythonVersionString() string {
	return pythonVersionString(minPythonMajor, minPythonMinor)
}
