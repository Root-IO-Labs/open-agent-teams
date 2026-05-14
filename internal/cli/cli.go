package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Root-IO-Labs/open-agent-teams/internal/agents"
	"github.com/Root-IO-Labs/open-agent-teams/internal/bugreport"
	"github.com/Root-IO-Labs/open-agent-teams/internal/daemon"
	"github.com/Root-IO-Labs/open-agent-teams/internal/diagnostics"
	"github.com/Root-IO-Labs/open-agent-teams/internal/errors"
	"github.com/Root-IO-Labs/open-agent-teams/internal/fork"
	"github.com/Root-IO-Labs/open-agent-teams/internal/format"
	"github.com/Root-IO-Labs/open-agent-teams/internal/hooks"
	"github.com/Root-IO-Labs/open-agent-teams/internal/messages"
	"github.com/Root-IO-Labs/open-agent-teams/internal/names"
	"github.com/Root-IO-Labs/open-agent-teams/internal/preflight"
	"github.com/Root-IO-Labs/open-agent-teams/internal/prompts"
	"github.com/Root-IO-Labs/open-agent-teams/internal/routing"
	"github.com/Root-IO-Labs/open-agent-teams/internal/socket"
	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
	"github.com/Root-IO-Labs/open-agent-teams/internal/templates"
	"github.com/Root-IO-Labs/open-agent-teams/internal/tui"
	"github.com/Root-IO-Labs/open-agent-teams/internal/version"
	"github.com/Root-IO-Labs/open-agent-teams/internal/worktree"
	agent_pkg "github.com/Root-IO-Labs/open-agent-teams/pkg/agent"
	backend_pkg "github.com/Root-IO-Labs/open-agent-teams/pkg/backend"
	"github.com/Root-IO-Labs/open-agent-teams/pkg/config"
)

// ─── Function index (approximate line numbers) ──────────────────
//
// Types + constructors:           ~84   (Command, CLI, New, NewWithPaths)
// registerCommands:               ~389  (all subcommand wiring)
// Daemon lifecycle:               ~1063 (startDaemon, stopDaemon, nukeDaemon)
// Repo commands:                  ~1875 (initRepo, listRepos, removeRepo)
// Worker commands:                ~2939 (createWorker, listWorkers, removeWorker)
// Agent definitions:              ~3396 (listAgentDefinitions, resetAgentDefinitions)
// Message commands:               ~5322 (sendMessage, listMessages, readMessage)
// Agent state:                    ~5853 (completeWorker, waitingForPR)
// Attach / logs:                  ~6925 (listLogsForRepo, attachAgent)
// Model commands:                 ~8602 (modelOnboard, modelList, modelSet, modelShow)
// Doctor:                         ~8520 (doctor preflight)
// Token reporting:                see tokens_report.go
// Helpers:                        ~8153 (ParseFlags, collectCLIEnvVars)
//
// ─────────────────────────────────────────────────────────────────

// Version is the current version of oat. Kept as a package-level alias for
// backwards compatibility with callers that set it directly via older
// `-ldflags "-X .../internal/cli.Version=..."` invocations. New build
// tooling should set internal/version.Version instead; the canonical
// getter (GetVersion) already consults that package.
var Version = "dev"

// GetVersion returns the semver-formatted version string for the running
// binary. Release builds inject the real version via ldflags into
// internal/version; development builds fall back to Go's VCS stamps so
// `go install …@sha` still produces a useful "0.0.0+<sha>-dev" label.
func GetVersion() string {
	// Honor the legacy override first so existing CI pipelines that stamp
	// internal/cli.Version keep working during the release-infra rollout.
	if Version != "dev" && Version != "" {
		return Version
	}

	info := version.Current()
	if !info.IsDev {
		return info.Version
	}
	if info.Commit != "" && info.Commit != "none" {
		return fmt.Sprintf("0.0.0+%s-dev", info.Commit)
	}
	return "0.0.0-dev"
}

// IsDevVersion reports whether the binary was built without a release
// version stamp (either via the new internal/version package or the
// legacy internal/cli.Version override).
func IsDevVersion() bool {
	if Version != "dev" && Version != "" {
		return false
	}
	return version.Current().IsDev
}

// Command represents a CLI command
type Command struct {
	Name        string
	Description string
	Usage       string
	Run         func(args []string) error
	Subcommands map[string]*Command
}

// CLI manages the command-line interface.
//
// Every exec.Command and worktree.Manager constructed by a CLI method uses
// ctx so that Ctrl-C (SIGINT) cancels in-flight git / gh / tmux calls
// instead of orphaning them. The ctx is populated by Execute; when using
// the CLI outside of a signal-aware entry point (tests), ctx defaults to
// context.Background().
type CLI struct {
	rootCmd       *Command
	paths         *config.Paths
	backend       backend_pkg.ProcessBackend
	documentation string                     // Full CLI reference (`oat docs`, tests)
	docsByAgent   map[state.AgentType]string // Per-agent-type filtered reference, populated on demand
	ctx           context.Context
}

// New creates a new CLI
func New() (*CLI, error) {
	paths, err := config.DefaultPaths()
	if err != nil {
		return nil, err
	}

	cli := &CLI{
		paths:   paths,
		backend: backend_pkg.BackendFromEnv(),
		ctx:     context.Background(),
		rootCmd: &Command{
			Name:        "OAT",
			Description: "Open Agent Teams",
			Subcommands: make(map[string]*Command),
		},
	}

	cli.registerCommands()

	// Generate documentation after commands are registered
	cli.documentation = cli.GenerateDocumentation()

	return cli, nil
}

// NewWithPaths creates a CLI with custom paths (for testing)
func NewWithPaths(paths *config.Paths) *CLI {
	cli := &CLI{
		paths:   paths,
		backend: backend_pkg.BackendFromEnv(),
		ctx:     context.Background(),
		rootCmd: &Command{
			Name:        "OAT",
			Description: "Open Agent Teams",
			Subcommands: make(map[string]*Command),
		},
	}

	cli.registerCommands()

	// Generate documentation after commands are registered
	cli.documentation = cli.GenerateDocumentation()

	return cli
}

// SetContext wires a cancellable context into the CLI. Must be called by
// the entry point before Execute so that SIGINT / SIGTERM propagate into
// exec.Command calls spawned by CLI commands.
func (c *CLI) SetContext(ctx context.Context) {
	c.ctx = ctx
}

// cmdCtx returns the CLI's context, falling back to context.Background()
// when tests construct a bare &CLI{} without going through New/NewWithPaths.
// Every exec.CommandContext / NewManagerWithContext call in this package
// routes through cmdCtx so the struct is nil-safe.
func (c *CLI) cmdCtx() context.Context {
	if c.ctx == nil {
		return context.Background()
	}
	return c.ctx
}

// getAgentBinary resolves the oat-agent binary path.
// Looks next to this binary first (co-located install), then falls back to PATH.
func (c *CLI) getAgentBinary() (string, error) {
	if p, err := findColocatedBinary("oat-agent"); err == nil {
		return p, nil
	}
	path, err := exec.LookPath("oat-agent")
	if err != nil {
		return "", errors.AgentBinaryNotFound(fmt.Errorf("oat-agent not found next to oat binary or in PATH: %w", err))
	}
	return path, nil
}

// findColocatedBinary returns the absolute path to name if it exists next to
// the currently running executable (resolving symlinks).
func findColocatedBinary(name string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	p := filepath.Join(filepath.Dir(exe), name)
	if _, err := os.Stat(p); err != nil {
		return "", err
	}
	return p, nil
}

// loadState loads the state file, wrapping errors with context
func (c *CLI) loadState() (*state.State, error) {
	st, err := state.Load(c.paths.StateFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load state: %w", err)
	}
	return st, nil
}

// sendDaemonRequest sends a request to the daemon and handles common error cases.
// It returns the response if successful, or an error if communication fails or the daemon returns an error.
func (c *CLI) sendDaemonRequest(command string, args map[string]interface{}) (*socket.Response, error) {
	client := socket.NewClient(c.paths.DaemonSock)
	resp, err := client.Send(socket.Request{
		Command: command,
		Args:    args,
	})
	if err != nil {
		return nil, errors.DaemonCommunicationFailed(command, err)
	}
	if !resp.Success {
		return nil, fmt.Errorf("%s failed: %s", command, resp.Error)
	}
	return resp, nil
}

// removeDirectoryIfExists removes a directory and prints status messages.
// It prints a warning if removal fails, or a success message if it succeeds.
// If the directory doesn't exist, it does nothing.
func removeDirectoryIfExists(path, description string) {
	if _, err := os.Stat(path); err == nil {
		if err := os.RemoveAll(path); err != nil {
			fmt.Printf("  Warning: failed to remove %s: %v\n", description, err)
		} else {
			fmt.Printf("  Removed %s\n", path)
		}
	}
}

// sessionSanitizer replaces problematic characters with hyphens for session names.
// Dots, colons, spaces, and forward slashes are not safe in session names.
var sessionSanitizer = strings.NewReplacer(
	".", "-",
	":", "-",
	" ", "-",
	"/", "-",
)

// sanitizeSessionName creates a safe session name from a repo name.
// Certain characters like dots are replaced to avoid issues with backend session names.
func sanitizeSessionName(repoName string) string {
	// Strip control characters (ASCII 0-31) for safety
	sanitized := strings.Map(func(r rune) rune {
		if r < 32 {
			return -1 // drop the character
		}
		return r
	}, repoName)
	return fmt.Sprintf("oat-%s", sessionSanitizer.Replace(sanitized))
}

// Execute executes the CLI with the given arguments
func (c *CLI) Execute(args []string) error {
	if len(args) == 0 {
		return c.showHelp()
	}

	// Check for --version or -v flag at top level
	if args[0] == "--version" || args[0] == "-v" {
		return c.showVersion()
	}

	return c.executeCommand(c.rootCmd, args)
}

// showVersion displays the version information
func (c *CLI) showVersion() error {
	fmt.Println(version.Current().String())
	return nil
}

// versionCommand displays version information with optional JSON output.
// Release builds (with ldflags injection into internal/version) emit the
// tagged version, the short commit SHA, and the build date. Development
// builds fall back to "0.0.0+<sha>-dev" via GetVersion.
func (c *CLI) versionCommand(args []string) error {
	flags, _ := ParseFlags(args)
	outputJSON := flags["json"] == "true"

	info := version.Current()
	displayVersion := GetVersion()

	if outputJSON {
		output := map[string]interface{}{
			"version":    displayVersion,
			"commit":     info.Commit,
			"date":       info.Date,
			"isDev":      IsDevVersion(),
			"rawVersion": Version,
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(output)
	}

	fmt.Printf("oat %s\n", displayVersion)
	if info.Commit != "" && info.Commit != "none" {
		fmt.Printf("  commit:  %s\n", info.Commit)
	}
	if info.Date != "" && info.Date != "unknown" {
		fmt.Printf("  built:   %s\n", info.Date)
	}
	return nil
}

// executeCommand recursively executes commands and subcommands
func (c *CLI) executeCommand(cmd *Command, args []string) error {
	if len(args) == 0 {
		if cmd.Run != nil {
			return cmd.Run([]string{})
		}
		return c.showCommandHelp(cmd)
	}

	// Check for --help or -h flag
	if args[0] == "--help" || args[0] == "-h" {
		return c.showCommandHelp(cmd)
	}

	// Check for subcommands
	if subcmd, exists := cmd.Subcommands[args[0]]; exists {
		return c.executeCommand(subcmd, args[1:])
	}

	// No subcommand found, run this command with args
	if cmd.Run != nil {
		return cmd.Run(args)
	}

	return errors.UnknownCommand(args[0])
}

// showHelp shows the main help message
func (c *CLI) showHelp() error {
	fmt.Println("OAT - Open Agent Teams")
	fmt.Println()
	fmt.Println("Quick Start:")
	fmt.Println("  oat init https://github.com/your/repo")
	fmt.Println("  oat worker create \"Add unit tests\"")
	fmt.Println("  oat attach worker-name")
	fmt.Println()
	fmt.Println("Commands:")

	for name, cmd := range c.rootCmd.Subcommands {
		fmt.Printf("  %-15s %s\n", name, cmd.Description)
	}

	fmt.Println()
	fmt.Println("Use 'oat <command> --help' for more information about a command.")
	return nil
}

// showCommandHelp shows help for a specific command
func (c *CLI) showCommandHelp(cmd *Command) error {
	fmt.Printf("%s - %s\n", cmd.Name, cmd.Description)
	fmt.Println()
	if cmd.Usage != "" {
		fmt.Printf("Usage: %s\n", cmd.Usage)
		fmt.Println()
	}

	if len(cmd.Subcommands) > 0 {
		fmt.Println("Subcommands:")
		for name, subcmd := range cmd.Subcommands {
			// Skip internal commands (prefixed with _)
			if strings.HasPrefix(name, "_") {
				continue
			}
			fmt.Printf("  %-15s %s\n", name, subcmd.Description)
		}
		fmt.Println()
	}

	return nil
}

// registerCommands registers all CLI commands
func (c *CLI) registerCommands() {
	// Daemon commands
	// Root-level 'start' is kept as alias for backward compatibility
	c.rootCmd.Subcommands["start"] = &Command{
		Name:        "start",
		Description: "Start the daemon (alias for 'daemon start')",
		Usage:       "oat start",
		Run:         c.startDaemon,
	}

	c.rootCmd.Subcommands["stop"] = &Command{
		Name:        "stop",
		Description: "Stop the daemon (alias for 'daemon stop')",
		Usage:       "oat stop",
		Run:         c.stopDaemon,
	}

	c.rootCmd.Subcommands["restart"] = &Command{
		Name:        "restart",
		Description: "Restart the daemon (alias for 'daemon restart')",
		Usage:       "oat restart",
		Run:         c.restartDaemon,
	}

	// Root-level status command - comprehensive system overview
	c.rootCmd.Subcommands["status"] = &Command{
		Name:        "status",
		Description: "Show system status and agent stats. Use --tokens for per-agent token and cache-hit breakdown.",
		Usage:       "oat status [--tokens]",
		Run:         c.systemStatus,
	}

	daemonCmd := &Command{
		Name:        "daemon",
		Description: "Manage the oat daemon",
		Subcommands: make(map[string]*Command),
	}

	daemonCmd.Subcommands["start"] = &Command{
		Name:        "start",
		Description: "Start the daemon",
		Usage:       "oat daemon start",
		Run:         c.startDaemon,
	}

	daemonCmd.Subcommands["stop"] = &Command{
		Name:        "stop",
		Description: "Stop the daemon",
		Usage:       "oat daemon stop",
		Run:         c.stopDaemon,
	}

	daemonCmd.Subcommands["status"] = &Command{
		Name:        "status",
		Description: "Show daemon status",
		Usage:       "oat daemon status",
		Run:         c.daemonStatus,
	}

	daemonCmd.Subcommands["logs"] = &Command{
		Name:        "logs",
		Description: "View daemon logs",
		Usage:       "oat daemon logs [-f|--follow] [-n <lines>]",
		Run:         c.daemonLogs,
	}

	daemonCmd.Subcommands["restart"] = &Command{
		Name:        "restart",
		Description: "Restart the daemon (stop + start)",
		Usage:       "oat daemon restart",
		Run:         c.restartDaemon,
	}

	daemonCmd.Subcommands["nuke"] = &Command{
		Name:        "nuke",
		Description: "Force-cleanup when the daemon is wedged (bypasses socket)",
		Usage:       "oat daemon nuke",
		Run:         c.nukeDaemon,
	}

	daemonCmd.Subcommands["_run"] = &Command{
		Name:        "_run",
		Description: "Internal: run daemon in foreground (used by daemon start)",
		Run:         c.runDaemon,
	}

	c.rootCmd.Subcommands["daemon"] = daemonCmd

	// Stop-all command (convenience for stopping everything)
	c.rootCmd.Subcommands["stop-all"] = &Command{
		Name:        "stop-all",
		Description: "Stop daemon and kill all oat sessions",
		Usage:       "oat stop-all [--clean] [--yes]",
		Run:         c.stopAll,
	}

	// Repository commands (repo subcommand)
	repoCmd := &Command{
		Name:        "repo",
		Description: "Manage repositories",
		Subcommands: make(map[string]*Command),
	}

	repoCmd.Subcommands["init"] = &Command{
		Name:        "init",
		Description: "Initialize a repository for OAT agents",
		Usage:       "oat repo init <github-url> [name] [--model=<model>]\n\nExample:\n  oat init https://github.com/myorg/myproject\n  oat init https://github.com/myorg/myproject --model claude-sonnet-4-6",
		Run:         c.initRepo,
	}

	repoCmd.Subcommands["list"] = &Command{
		Name:        "list",
		Description: "List tracked repositories",
		Usage:       "oat repo list",
		Run:         c.listRepos,
	}

	repoCmd.Subcommands["rm"] = &Command{
		Name:        "rm",
		Description: "Remove a tracked repository",
		Usage:       "oat repo rm <name>",
		Run:         c.removeRepo,
	}

	repoCmd.Subcommands["use"] = &Command{
		Name:        "use",
		Description: "Set the default repository",
		Usage:       "oat repo use <name>",
		Run:         c.setCurrentRepo,
	}

	repoCmd.Subcommands["current"] = &Command{
		Name:        "current",
		Description: "Show the default repository",
		Usage:       "oat repo current",
		Run:         c.getCurrentRepo,
	}

	repoCmd.Subcommands["unset"] = &Command{
		Name:        "unset",
		Description: "Clear the default repository",
		Usage:       "oat repo unset",
		Run:         c.clearCurrentRepo,
	}

	repoCmd.Subcommands["history"] = &Command{
		Name:        "history",
		Description: "Show task history for a repository",
		Usage:       "oat repo history [--repo <repo>] [-n <count>] [--status <status>] [--search <query>] [--full]",
		Run:         c.showHistory,
	}

	repoCmd.Subcommands["hibernate"] = &Command{
		Name:        "hibernate",
		Description: "Hibernate a repository, archiving uncommitted changes",
		Usage:       "oat repo hibernate [--repo <repo>] [--all] [--yes]",
		Run:         c.hibernateRepo,
	}

	c.rootCmd.Subcommands["repo"] = repoCmd

	// Backward compatibility aliases for root-level repo commands
	c.rootCmd.Subcommands["init"] = repoCmd.Subcommands["init"]
	c.rootCmd.Subcommands["list"] = repoCmd.Subcommands["list"]
	c.rootCmd.Subcommands["history"] = repoCmd.Subcommands["history"]

	// Worker commands
	workerCmd := &Command{
		Name:        "worker",
		Description: "Manage worker agents",
		Usage:       "oat worker [<task>] [--repo <repo>] [--branch <branch>] [--push-to <branch>] [--issue <number>] [--issue-url <url>]",
		Subcommands: make(map[string]*Command),
	}

	workerCmd.Run = c.createWorker // Default action for 'worker' command (same as 'worker create')

	workerCmd.Subcommands["create"] = &Command{
		Name:        "create",
		Description: "Create a worker agent to handle a coding task",
		Usage:       "oat worker create <task description>\n\nExamples:\n  oat worker create \"Add unit tests for auth module\"\n  oat worker create \"Fix login bug\" --issue 42\n  oat worker create \"Refactor database layer\" --model claude-opus-4-6",
		Run:         c.createWorker,
	}

	workerCmd.Subcommands["spawn"] = workerCmd.Subcommands["create"]

	workerCmd.Subcommands["list"] = &Command{
		Name:        "list",
		Description: "List active workers",
		Usage:       "oat worker list [--repo <repo>]",
		Run:         c.listWorkers,
	}

	workerCmd.Subcommands["rm"] = &Command{
		Name:        "rm",
		Description: "Remove a worker (or all workers with --all --force). Use --force to skip confirmations.",
		Usage:       "oat worker rm <worker-name> [--force] | oat worker rm --all --force [--repo <repo>]",
		Run:         c.removeWorker,
	}

	workerCmd.Subcommands["remove"] = &Command{
		Name:        "remove",
		Description: "Remove a worker (alias for rm). Use --all --force to remove all workers.",
		Usage:       "oat worker remove <worker-name> [--force] | oat worker remove --all --force [--repo <repo>]",
		Run:         c.removeWorker,
	}

	workerCmd.Subcommands["reset-nudge"] = &Command{
		Name:        "reset-nudge",
		Description: "Reset a worker's nudge count (one-time use per worker, for supervisor use)",
		Usage:       "oat worker reset-nudge <worker-name>",
		Run:         c.resetWorkerNudge,
	}

	workerCmd.Subcommands["verify"] = &Command{
		Name:        "verify",
		Description: "Verify worker implementation quality before creating PR (diagnostic, not required)",
		Usage:       "oat worker verify [--fix] [--skip-tests] [--verbose] [--json]",
		Run:         c.verifyWorker,
	}

	workerCmd.Subcommands["request-review"] = &Command{
		Name:        "request-review",
		Description: "Spawn a verification agent to independently review your work before PR creation",
		Usage:       "oat worker request-review",
		Run:         c.requestVerification,
	}

	workerCmd.Subcommands["set-verdict"] = &Command{
		Name:        "set-verdict",
		Description: "Set verification verdict for a worker (used by verification agents)",
		Usage:       "oat worker set-verdict <worker-name> approved|rejected --sha <sha> --reason \"...\"",
		Run:         c.setVerificationVerdict,
	}

	c.rootCmd.Subcommands["worker"] = workerCmd

	// 'work' is an alias for 'worker' (backward compatibility)
	c.rootCmd.Subcommands["work"] = workerCmd

	// Model commands
	modelCmd := &Command{
		Name:        "model",
		Description: "Manage model profiles and onboarding",
		Usage:       "oat model <subcommand>",
		Subcommands: make(map[string]*Command),
	}

	modelCmd.Subcommands["onboard"] = &Command{
		Name:        "onboard",
		Description: "Probe a model's capabilities and generate a profile",
		Usage:       "oat model onboard <provider:model> [--probe-set minimum|default] [--verbose]",
		Run:         c.modelOnboard,
	}

	modelCmd.Subcommands["list"] = &Command{
		Name:        "list",
		Description: "List checked-in model profiles",
		Usage:       "oat model list",
		Run:         c.modelList,
	}

	modelCmd.Subcommands["show"] = &Command{
		Name:        "show",
		Description: "Show a model's capability profile",
		Usage:       "oat model show <provider:model>",
		Run:         c.modelShow,
	}

	modelCmd.Subcommands["restore"] = &Command{
		Name:        "restore",
		Description: "Restore a model profile from backup (undo a bad onboard)",
		Usage:       "oat model restore <provider:model>",
		Run:         c.modelRestore,
	}

	modelCmd.Subcommands["set"] = &Command{
		Name:        "set",
		Description: "Update per-model runtime parameters (max_tokens, nudge_interval_seconds)",
		Usage:       "oat model set <provider:model> [--max-tokens N] [--nudge-interval SECONDS]",
		Run:         c.modelSet,
	}

	c.rootCmd.Subcommands["model"] = modelCmd

	// Routing-history reporting + privacy. Read-only commands that surface
	// the corpus collected at ~/.oat/routing-history.jsonl.
	routingCmd := &Command{
		Name:        "routing",
		Description: "Inspect and manage the routing-history corpus",
		Usage:       "oat routing <subcommand>",
		Subcommands: make(map[string]*Command),
	}
	routingCmd.Subcommands["report"] = &Command{
		Name:        "report",
		Description: "Summarize routing history by model: success rate, p50/p95 wall, Wilson 95% lower bound",
		Usage:       "oat routing report",
		Run:         c.routingReport,
	}
	routingCmd.Subcommands["privacy"] = &Command{
		Name:        "privacy",
		Description: "Show current privacy mode + describe each level",
		Usage:       "oat routing privacy",
		Run:         c.routingPrivacy,
	}
	routingCmd.Subcommands["migrate-v1"] = &Command{
		Name:        "migrate-v1",
		Description: "Force a v1→v2 corpus migration (daemon also auto-migrates on start)",
		Usage:       "oat routing migrate-v1",
		Run:         c.routingMigrate,
	}
	routingCmd.Subcommands["share"] = &Command{
		Name:        "share",
		Description: "Build the opt-in upload payload (--dry-run only in v0; endpoint not yet live)",
		Usage:       "oat routing share --dry-run",
		Run:         c.routingShare,
	}
	routingCmd.Subcommands["route"] = &Command{
		Name:        "route",
		Description: "Preview the router's pick for a task (no worker spawn, no LLM calls)",
		Usage:       "oat routing route --task \"<text>\" [--allow csv] [--v2 | --all]",
		Run:         c.routingRoute,
	}
	c.rootCmd.Subcommands["routing"] = routingCmd

	// Workspace commands
	workspaceCmd := &Command{
		Name:        "workspace",
		Description: "Manage workspaces",
		Usage:       "oat workspace [<name>]",
		Subcommands: make(map[string]*Command),
	}

	workspaceCmd.Run = c.workspaceDefault // Default action: list or connect

	workspaceCmd.Subcommands["add"] = &Command{
		Name:        "add",
		Description: "Add a new workspace",
		Usage:       "oat workspace add <name> [--branch <branch>]",
		Run:         c.addWorkspace,
	}

	workspaceCmd.Subcommands["rm"] = &Command{
		Name:        "rm",
		Description: "Remove a workspace",
		Usage:       "oat workspace rm <name>",
		Run:         c.removeWorkspace,
	}

	workspaceCmd.Subcommands["list"] = &Command{
		Name:        "list",
		Description: "List workspaces",
		Usage:       "oat workspace list",
		Run:         c.listWorkspaces,
	}

	workspaceCmd.Subcommands["connect"] = &Command{
		Name:        "connect",
		Description: "Connect to a workspace",
		Usage:       "oat workspace connect <name>",
		Run:         c.connectWorkspace,
	}

	c.rootCmd.Subcommands["workspace"] = workspaceCmd

	// Agent commands (run from within agent)
	agentCmd := &Command{
		Name:        "agent",
		Description: "Agent communication commands",
		Subcommands: make(map[string]*Command),
	}

	// Legacy message commands (aliases for backward compatibility)
	// Prefer: oat message send/list/read/ack
	agentCmd.Subcommands["send-message"] = &Command{
		Name:        "send-message",
		Description: "Send a message to another agent (alias for 'message send')",
		Usage:       "oat agent send-message <recipient> <message>",
		Run:         c.sendMessage,
	}

	agentCmd.Subcommands["list-messages"] = &Command{
		Name:        "list-messages",
		Description: "List pending messages (alias for 'message list')",
		Usage:       "oat agent list-messages",
		Run:         c.listMessages,
	}

	agentCmd.Subcommands["read-message"] = &Command{
		Name:        "read-message",
		Description: "Read a specific message (alias for 'message read')",
		Usage:       "oat agent read-message <message-id>",
		Run:         c.readMessage,
	}

	agentCmd.Subcommands["ack-message"] = &Command{
		Name:        "ack-message",
		Description: "Acknowledge a message (alias for 'message ack')",
		Usage:       "oat agent ack-message <message-id>",
		Run:         c.ackMessage,
	}

	agentCmd.Subcommands["complete"] = &Command{
		Name:        "complete",
		Description: "Signal worker completion",
		Usage:       "oat agent complete [--summary <text>] [--failure <reason>] [--worker <name>] [--force]",
		Run:         c.completeWorker,
	}

	agentCmd.Subcommands["waiting"] = &Command{
		Name:        "waiting",
		Description: "Signal that worker is waiting for PR resolution (dormant, zero token burn)",
		Usage:       "oat agent waiting",
		Run:         c.waitingForPR,
	}

	agentCmd.Subcommands["restart"] = &Command{
		Name:        "restart",
		Description: "Restart a crashed or exited agent",
		Usage:       "oat agent restart <name> [--repo <repo>] [--force]",
		Run:         c.restartAgentCmd,
	}

	agentCmd.Subcommands["attach"] = &Command{
		Name:        "attach",
		Description: "Watch an agent work in real-time",
		Usage:       "oat agent attach <agent-name> [--read-only]\n\nExamples:\n  oat attach worker-swift-eagle\n  oat attach supervisor --read-only",
		Run:         c.attachAgent,
	}

	agentCmd.Subcommands["tell"] = &Command{
		Name:        "tell",
		Description: "Send direct input to an agent (works with direct backend)",
		Usage:       "oat agent tell <agent-name> <message> [--repo <repo>]",
		Run:         c.tellAgent,
	}

	agentCmd.Subcommands["interrupt"] = &Command{
		Name:        "interrupt",
		Description: "Send Ctrl-C to a running agent",
		Usage:       "oat agent interrupt <agent-name> [--repo <repo>]",
		Run:         c.interruptAgent,
	}

	// Default Run for "oat agent" with no subcommand: resume agent session
	agentCmd.Run = c.restartAgentInContext

	c.rootCmd.Subcommands["agent"] = agentCmd

	// Message commands (new noun group for message operations)
	// These are the preferred commands; agent *-message commands are kept as aliases
	messageCmd := &Command{
		Name:        "message",
		Description: "Manage inter-agent messages",
		Subcommands: make(map[string]*Command),
	}

	messageCmd.Subcommands["send"] = &Command{
		Name:        "send",
		Description: "Send a message to another agent",
		Usage:       "oat message send <recipient> <message>",
		Run:         c.sendMessage,
	}

	messageCmd.Subcommands["list"] = &Command{
		Name:        "list",
		Description: "List pending messages",
		Usage:       "oat message list",
		Run:         c.listMessages,
	}

	messageCmd.Subcommands["read"] = &Command{
		Name:        "read",
		Description: "Read a specific message",
		Usage:       "oat message read <message-id>",
		Run:         c.readMessage,
	}

	messageCmd.Subcommands["ack"] = &Command{
		Name:        "ack",
		Description: "Acknowledge a message",
		Usage:       "oat message ack <message-id>",
		Run:         c.ackMessage,
	}

	c.rootCmd.Subcommands["message"] = messageCmd

	// PR commands
	prCmd := &Command{
		Name:        "pr",
		Description: "Pull request management",
		Subcommands: make(map[string]*Command),
	}

	prCmd.Subcommands["create"] = &Command{
		Name:        "create",
		Description: "Create a PR with proper formatting and auto-dormant",
		Usage:       "oat pr create --title <title> --body <body> [--closes <issue>] [--draft]",
		Run:         c.prCreate,
	}

	c.rootCmd.Subcommands["pr"] = prCmd

	// Issue commands
	issueCmd := &Command{
		Name:        "issue",
		Description: "Issue management",
		Subcommands: make(map[string]*Command),
	}

	issueCmd.Subcommands["create"] = &Command{
		Name:        "create",
		Description: "Create a structured issue with proper labeling and optional workspace notification",
		Usage:       "oat issue create --title <title> --body <body> [--label <label>]... [--wave <N>] [--blocker] [--file <path>]... [--expected <text>] [--actual <text>] [--spec-ref <text>]",
		Run:         c.issueCreate,
	}

	c.rootCmd.Subcommands["issue"] = issueCmd

	// 'attach' is an alias for 'agent attach' (backward compatibility)
	c.rootCmd.Subcommands["attach"] = agentCmd.Subcommands["attach"]
	// 'tell' is an alias for 'agent tell' for quick operator nudges.
	c.rootCmd.Subcommands["tell"] = agentCmd.Subcommands["tell"]
	c.rootCmd.Subcommands["interrupt"] = agentCmd.Subcommands["interrupt"]

	// Maintenance commands
	c.rootCmd.Subcommands["cleanup"] = &Command{
		Name:        "cleanup",
		Description: "Clean up orphaned resources",
		Usage:       "oat cleanup [--dry-run] [--verbose] [--merged]",
		Run:         c.cleanup,
	}

	c.rootCmd.Subcommands["repair"] = &Command{
		Name:        "repair",
		Description: "Repair state after crash",
		Usage:       "oat repair [--verbose]",
		Run:         c.repair,
	}

	c.rootCmd.Subcommands["sync"] = &Command{
		Name:        "sync",
		Description: "Pull latest changes from remote and sync agent worktrees",
		Usage:       "oat sync [--branch <branch>] [--repo <repo>]",
		Run:         c.syncWorktrees,
	}

	c.rootCmd.Subcommands["refresh"] = &Command{
		Name:        "refresh",
		Description: "Sync agent worktrees (alias for 'oat sync')",
		Usage:       "oat refresh",
		Run:         c.syncWorktrees,
	}

	// Debug command
	c.rootCmd.Subcommands["docs"] = &Command{
		Name:        "docs",
		Description: "Show generated CLI documentation",
		Usage:       "oat docs",
		Run:         c.showDocs,
	}

	// Review command
	c.rootCmd.Subcommands["review"] = &Command{
		Name:        "review",
		Description: "Spawn a review agent for a PR",
		Usage:       "oat review <pr-url>",
		Run:         c.reviewPR,
	}

	// Logs commands
	logsCmd := &Command{
		Name:        "logs",
		Description: "View and manage agent output logs",
		Usage:       "oat logs [<agent-name>] [-f|--follow]",
		Subcommands: make(map[string]*Command),
	}

	logsCmd.Run = c.viewLogs // Default action: view logs for an agent

	logsCmd.Subcommands["list"] = &Command{
		Name:        "list",
		Description: "List log files",
		Usage:       "oat logs list [--repo <repo>]",
		Run:         c.listLogs,
	}

	logsCmd.Subcommands["search"] = &Command{
		Name:        "search",
		Description: "Search across logs",
		Usage:       "oat logs search <pattern> [--repo <repo>]",
		Run:         c.searchLogs,
	}

	logsCmd.Subcommands["clean"] = &Command{
		Name:        "clean",
		Description: "Remove old logs",
		Usage:       "oat logs clean --older-than <duration>",
		Run:         c.cleanLogs,
	}

	c.rootCmd.Subcommands["logs"] = logsCmd

	// Config command
	c.rootCmd.Subcommands["config"] = &Command{
		Name:        "config",
		Description: "View or modify repository configuration",
		Usage: `oat config [repo] [flags]
  oat config [repo] worker-models set <csv>       Set the full allowed worker models list
  oat config [repo] worker-models add <csv>       Add model(s) to the allowed list
  oat config [repo] worker-models remove <csv>    Remove model(s) from the allowed list
  oat config [repo] worker-models list            Show current allowed worker models
  oat config [repo] worker-models clear           Clear the list (no restriction)

Flags:
  --mq-enabled=true|false              Enable/disable merge queue
  --mq-track=all|author|assigned       Set merge queue tracking mode
  --ps-enabled=true|false              Enable/disable PR shepherd
  --ps-track=all|author|assigned       Set PR shepherd tracking mode
  --workspace-stuck-detection=true|false  Enable/disable workspace stuck detection
  --allowed-worker-models=<csv>        Set allowed worker models (shorthand for worker-models set)

Aliases for --allowed-worker-models: --available-worker-models, --allowed-models`,
		Run: c.configRepo,
	}

	// Bug report command
	c.rootCmd.Subcommands["bug"] = &Command{
		Name:        "bug",
		Description: "Generate a diagnostic bug report",
		Usage:       "oat bug [--output <file>] [--verbose] [description]",
		Run:         c.bugReport,
	}

	// Diagnostics command
	c.rootCmd.Subcommands["diagnostics"] = &Command{
		Name:        "diagnostics",
		Description: "Show system diagnostics in machine-readable format",
		Usage:       "oat diagnostics [--json] [--output <file>]",
		Run:         c.diagnostics,
	}

	// Doctor command — preflight checks for first-run and support triage.
	c.rootCmd.Subcommands["doctor"] = &Command{
		Name:        "doctor",
		Description: "Run preflight checks for required tools, API keys, and daemon",
		Usage:       "oat doctor [--json]",
		Run:         c.doctor,
	}

	// Version command
	c.rootCmd.Subcommands["version"] = &Command{
		Name:        "version",
		Description: "Show version information",
		Usage:       "oat version [--json]",
		Run:         c.versionCommand,
	}

	tokensCmd := &Command{
		Name:        "tokens",
		Description: "Token usage reports (historical, parsed from agent logs)",
		Subcommands: make(map[string]*Command),
	}
	tokensCmd.Subcommands["report"] = &Command{
		Name: "report",
		Description: "Historical per-agent token usage parsed from [OAT_TOKENS] log lines. " +
			"Distinct from `oat status --tokens`, which reads live daemon state.",
		Usage: "oat tokens report --repo <name> [--since <ts>] [--until <ts>] " +
			"[--wave N] [--waves-file <path>] [--format table|json]",
		Run: c.tokensReport,
	}
	c.rootCmd.Subcommands["tokens"] = tokensCmd

	// Agents command - for managing agent definitions
	agentsCmd := &Command{
		Name:        "agents",
		Description: "Manage agent definitions",
		Subcommands: make(map[string]*Command),
	}

	agentsCmd.Subcommands["list"] = &Command{
		Name:        "list",
		Description: "List available agent definitions for a repository",
		Usage:       "oat agents list [--repo <repo>]",
		Run:         c.listAgentDefinitions,
	}

	agentsCmd.Subcommands["spawn"] = &Command{
		Name:        "spawn",
		Description: "Spawn an agent from a prompt file",
		Usage:       "oat agents spawn --name <name> --class <class> --prompt-file <file> [--repo <repo>] [--task <task>]",
		Run:         c.spawnAgentFromFile,
	}

	agentsCmd.Subcommands["reset"] = &Command{
		Name:        "reset",
		Description: "Reset agent definitions to defaults (re-copy from templates)",
		Usage:       "oat agents reset [--repo <repo>]",
		Run:         c.resetAgentDefinitions,
	}

	c.rootCmd.Subcommands["agents"] = agentsCmd

	// TUI command
	c.rootCmd.Subcommands["ui"] = &Command{
		Name:        "ui",
		Description: "Launch the interactive terminal UI",
		Usage:       "oat ui [--repo <repo>]",
		Run:         c.runUI,
	}
}

// Daemon command implementations

func (c *CLI) startDaemon(args []string) error {
	return daemon.RunDetached()
}

func (c *CLI) runDaemon(args []string) error {
	return daemon.Run()
}

func (c *CLI) stopDaemon(args []string) error {
	stopped, err := c.killDaemon()
	if err != nil {
		return err
	}
	if !stopped {
		fmt.Println("Daemon is not running")
		return nil
	}
	fmt.Println("Daemon stopped successfully")
	return nil
}

func (c *CLI) restartDaemon(args []string) error {
	if _, err := c.killDaemon(); err != nil {
		return fmt.Errorf("failed to stop daemon: %w", err)
	}
	fmt.Println("Daemon stopped, restarting...")
	return daemon.RunDetached()
}

// killDaemon stops the daemon, escalating from a graceful socket shutdown to
// SIGTERM and finally SIGKILL if the daemon is unresponsive. It also cleans
// up a stale PID file. Returns (stopped, err): stopped=false if no daemon was
// running (nothing to do); stopped=true if we actually terminated something.
func (c *CLI) killDaemon() (bool, error) {
	pidFile := daemon.NewPIDFile(c.paths.DaemonPID)
	running, pid, _ := pidFile.IsRunning()
	if !running {
		// Clean up a stale PID file if one exists so the next start isn't blocked.
		_ = pidFile.Remove()
		return false, nil
	}

	// 1) Try graceful shutdown via the socket. Short window because an
	//    unresponsive daemon is exactly why we'd be calling this.
	_, _ = c.sendDaemonRequest("stop", nil)
	if waitForPIDExit(pid, 3*time.Second) {
		_ = pidFile.Remove()
		return true, nil
	}

	// 2) SIGTERM
	if proc, err := os.FindProcess(pid); err == nil {
		_ = proc.Signal(syscall.SIGTERM)
	}
	if waitForPIDExit(pid, 3*time.Second) {
		_ = pidFile.Remove()
		return true, nil
	}

	// 3) SIGKILL — last resort.
	if proc, err := os.FindProcess(pid); err == nil {
		_ = proc.Signal(syscall.SIGKILL)
	}
	if waitForPIDExit(pid, 2*time.Second) {
		_ = pidFile.Remove()
		return true, nil
	}

	return false, fmt.Errorf("daemon (PID %d) still alive after SIGKILL", pid)
}

// nukeDaemon is the "break glass" escape hatch for when the daemon is
// wedged (socket unresponsive, processes in macOS UE state, etc.).
//
// Unlike stopDaemon/restartDaemon, nuke:
//   - never talks to the daemon socket (which may be hung)
//   - SIGKILLs every oat daemon + agent sidecar process
//   - removes socket + pid files so a fresh daemon can start on a new inode
//   - ignores UE/zombie processes it can't kill — they're fossils from the
//     kernel's perspective and don't block a new daemon from binding a new
//     socket inode at the same path
//
// Use this when `oat daemon status` hangs or `oat daemon stop` won't return.
func (c *CLI) nukeDaemon(args []string) error {
	fmt.Println("==> Force-cleaning OAT daemon state (bypasses socket)")

	// 1) Record what's alive now so we can show the user what got cleaned up
	//    vs. what is still lingering as a kernel fossil.
	before := snapshotOatProcesses()

	// 2) SIGKILL all daemon + agent processes. pkill returns non-zero if no
	//    matches; ignore errors. Order: daemons first, then agents so the
	//    daemon can't respawn supervisor/merge-queue before we kill them.
	for _, pattern := range []string{
		"oat daemon _run",
		"oat ui",
		"oat_cli.main",
	} {
		_ = exec.CommandContext(c.ctx, "pkill", "-9", "-f", pattern).Run()
	}

	// 3) Remove the pid + socket files. If a fossil process still holds the
	//    old socket inode in kernel space, it's fine — removing the path
	//    lets `net.Listen("unix", path)` create a fresh inode at the same
	//    filesystem path, and the fossil's open FD is unrelated to new traffic.
	removed := []string{}
	for _, f := range []string{c.paths.DaemonPID, c.paths.DaemonSock} {
		if err := os.Remove(f); err == nil {
			removed = append(removed, f)
		}
	}

	// Give SIGKILL a moment to propagate before we inspect what's left.
	time.Sleep(500 * time.Millisecond)

	after := snapshotOatProcesses()

	// 4) Report. "Killed" = gone after our pkill. "Fossils" = still present,
	//    almost certainly in UE state and unkillable until the kernel
	//    releases them. The user can safely ignore fossils and start a new
	//    daemon.
	killed := diffProcesses(before, after)
	fossils := after

	if len(killed) > 0 {
		fmt.Printf("  Killed %d process(es):\n", len(killed))
		for _, p := range killed {
			fmt.Printf("    %d  %s\n", p.pid, p.cmd)
		}
	}
	for _, f := range removed {
		fmt.Printf("  Removed: %s\n", f)
	}
	if len(fossils) > 0 {
		fmt.Printf("  WARNING: %d process(es) still present (likely UE/zombie — kernel wedge):\n", len(fossils))
		for _, p := range fossils {
			fmt.Printf("    %d  %s\n", p.pid, p.cmd)
		}
		fmt.Println("  These are kernel fossils. They cannot be killed until the kernel")
		fmt.Println("  releases them (usually a reboot). They do NOT block a new daemon —")
		fmt.Println("  the new daemon binds a fresh socket inode at the same path.")
	}

	fmt.Println()
	fmt.Println("==> Nuke complete. Next: oat daemon start")
	return nil
}

// oatProcess is a minimal (pid, command) pair used by nukeDaemon for
// before/after diffs. Not exposed.
type oatProcess struct {
	pid int
	cmd string
}

// snapshotOatProcesses lists all live oat daemon / agent processes. Used by
// nukeDaemon to report what it killed vs. what is still fossilized.
func snapshotOatProcesses() []oatProcess {
	out, _ := exec.CommandContext(context.Background(), "pgrep", "-fl",
		"oat daemon _run|oat ui|oat_cli.main",
	).Output()
	var procs []oatProcess
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		// pgrep -fl output: "<pid> <command>"
		sp := strings.SplitN(line, " ", 2)
		if len(sp) != 2 {
			continue
		}
		pid, err := strconv.Atoi(sp[0])
		if err != nil {
			continue
		}
		cmd := sp[1]
		if len(cmd) > 80 {
			cmd = cmd[:77] + "..."
		}
		procs = append(procs, oatProcess{pid: pid, cmd: cmd})
	}
	return procs
}

// diffProcesses returns processes present in `before` but not in `after` — i.e.
// the ones our SIGKILL successfully terminated.
func diffProcesses(before, after []oatProcess) []oatProcess {
	afterPIDs := make(map[int]struct{}, len(after))
	for _, p := range after {
		afterPIDs[p.pid] = struct{}{}
	}
	var killed []oatProcess
	for _, p := range before {
		if _, stillAlive := afterPIDs[p.pid]; !stillAlive {
			killed = append(killed, p)
		}
	}
	return killed
}

// waitForPIDExit polls until the process is gone or the timeout elapses.
func waitForPIDExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		proc, err := os.FindProcess(pid)
		if err != nil {
			return true
		}
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			return true // ESRCH or similar — process is gone
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func (c *CLI) daemonStatus(args []string) error {
	// Check PID file first
	pidFile := daemon.NewPIDFile(c.paths.DaemonPID)
	running, pid, err := pidFile.IsRunning()
	if err != nil {
		return fmt.Errorf("failed to check daemon status: %w", err)
	}

	if !running {
		fmt.Println("Daemon is not running")
		return nil
	}

	// Try to connect to daemon
	client := socket.NewClient(c.paths.DaemonSock)
	resp, err := client.Send(socket.Request{
		Command: "status",
	})
	if err != nil {
		// Daemon-unreachable is reportable status, not a CLI error.
		fmt.Printf("Daemon PID file exists (PID: %d) but daemon is not responding\n", pid)
		return nil //nolint:nilerr // graceful status reporting
	}

	if !resp.Success {
		return fmt.Errorf("status check failed: %s", resp.Error)
	}

	// Pretty print status
	fmt.Println("Daemon Status:")
	if statusMap, ok := resp.Data.(map[string]interface{}); ok {
		fmt.Printf("  Running: %v\n", statusMap["running"])
		fmt.Printf("  PID: %v\n", statusMap["pid"])
		fmt.Printf("  Repos: %v\n", statusMap["repos"])
		fmt.Printf("  Agents: %v\n", statusMap["agents"])
		fmt.Printf("  Socket: %v\n", statusMap["socket_path"])
		if idleRepos, ok := statusMap["idle_repos"].([]interface{}); ok && len(idleRepos) > 0 {
			names := make([]string, 0, len(idleRepos))
			for _, r := range idleRepos {
				if s, _ := r.(string); s != "" {
					names = append(names, s)
				}
			}
			fmt.Printf("  Idle:   %s\n", strings.Join(names, ", "))
		}
		if activeRepos, ok := statusMap["active_repos"].([]interface{}); ok && len(activeRepos) > 0 {
			names := make([]string, 0, len(activeRepos))
			for _, r := range activeRepos {
				if s, _ := r.(string); s != "" {
					names = append(names, s)
				}
			}
			fmt.Printf("  Active: %s\n", strings.Join(names, ", "))
		}
	} else {
		// Fallback: print as JSON
		jsonData, _ := json.MarshalIndent(resp.Data, "  ", "  ")
		fmt.Println(string(jsonData))
	}

	return nil
}

// runUI launches the interactive terminal UI.
func (c *CLI) runUI(args []string) error {
	flags, _ := ParseFlags(args)

	// Resolve the current repository
	repoName, err := c.resolveRepo(flags)
	if err != nil {
		return fmt.Errorf("cannot determine repository: %w\nUse --repo flag or run 'oat repo init' first", err)
	}

	// Auto-start daemon if not running
	pidFile := daemon.NewPIDFile(c.paths.DaemonPID)
	running, _, _ := pidFile.IsRunning()
	if !running {
		fmt.Println("Starting daemon...")
		if err := daemon.RunDetached(); err != nil {
			return fmt.Errorf("failed to start daemon: %w", err)
		}
		// Brief wait for socket to become available
		time.Sleep(500 * time.Millisecond)
	}

	// Honor OAT_THEME=dark|light to override lipgloss's background detection,
	// which is unreliable across terminal emulators (tmux, some SSH clients,
	// non-xterm-compatible terminals).
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OAT_THEME"))) {
	case "dark":
		lipgloss.SetHasDarkBackground(true)
	case "light":
		lipgloss.SetHasDarkBackground(false)
	}

	app := tui.NewApp(c.paths.DaemonSock, repoName, c.paths)
	// No mouse capture — allows standard terminal text selection and copy.
	// Mouse scrolling is handled by the terminal emulator natively.
	p := tea.NewProgram(app, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("TUI error: %w", err)
	}
	return nil
}

// systemStatus shows a comprehensive system overview that gracefully handles
// the daemon not running (unlike list commands which error).
func (c *CLI) systemStatus(args []string) error {
	flags, _ := ParseFlags(args)
	if flags["tokens"] == "true" {
		return c.systemStatusTokens()
	}

	// Check PID file first
	pidFile := daemon.NewPIDFile(c.paths.DaemonPID)
	running, pid, err := pidFile.IsRunning()
	if err != nil {
		return fmt.Errorf("failed to check daemon status: %w", err)
	}

	if !running {
		format.Header("OAT Status")
		fmt.Println()
		fmt.Printf("  Daemon: %s\n", format.Red.Sprint("not running"))
		fmt.Println()
		format.Dimmed("Start with: oat start")
		return nil
	}

	// Try to connect to daemon and get rich status
	client := socket.NewClient(c.paths.DaemonSock)
	resp, err := client.Send(socket.Request{
		Command: "list_repos",
		Args:    map[string]interface{}{"rich": true},
	})

	if err != nil {
		format.Header("OAT Status")
		fmt.Println()
		fmt.Printf("  Daemon: %s (PID: %d, not responding)\n", format.Yellow.Sprint("unhealthy"), pid)
		fmt.Println()
		format.Dimmed("Try: oat restart")
		return nil //nolint:nilerr // graceful status reporting
	}

	if !resp.Success {
		format.Header("OAT Status")
		fmt.Println()
		fmt.Printf("  Daemon: %s (PID: %d)\n", format.Yellow.Sprint("error"), pid)
		fmt.Printf("  Error: %s\n", resp.Error)
		return nil
	}

	// Print status header
	format.Header("OAT Status")
	fmt.Println()
	fmt.Printf("  Daemon: %s (PID: %d)\n", format.Green.Sprint("running"), pid)

	repos, ok := resp.Data.([]interface{})
	if !ok || len(repos) == 0 {
		fmt.Printf("  Repos:  %s\n", format.Dim.Sprint("none"))
		fmt.Println()
		format.Dimmed("Initialize a repo with: oat init <github-url>")
		return nil
	}

	fmt.Printf("  Repos:  %d\n", len(repos))
	fmt.Println()

	// Show each repo with agents
	for _, repo := range repos {
		repoMap, ok := repo.(map[string]interface{})
		if !ok {
			continue
		}

		name, _ := repoMap["name"].(string)
		totalAgents := 0
		if v, ok := repoMap["total_agents"].(float64); ok {
			totalAgents = int(v)
		}
		workerCount := 0
		if v, ok := repoMap["worker_count"].(float64); ok {
			workerCount = int(v)
		}
		sessionHealthy, _ := repoMap["session_healthy"].(bool)
		idleMode, _ := repoMap["idle_mode"].(bool)

		// Repo line
		repoStatus := format.Green.Sprint("●")
		if !sessionHealthy {
			repoStatus = format.Yellow.Sprint("○")
		}
		idleLabel := ""
		if idleMode {
			idleLabel = " " + format.Dim.Sprint("(idle)")
		} else {
			idleLabel = " " + format.Dim.Sprint("(active)")
		}
		fmt.Printf("  %s %s%s\n", repoStatus, format.Bold.Sprint(name), idleLabel)

		// Agent summary
		coreAgents := totalAgents - workerCount
		if coreAgents < 0 {
			coreAgents = 0
		}
		fmt.Printf("      Agents: %d core, %d workers\n", coreAgents, workerCount)

		// Show fork info if applicable
		if isFork, _ := repoMap["is_fork"].(bool); isFork {
			upstreamOwner, _ := repoMap["upstream_owner"].(string)
			upstreamRepo, _ := repoMap["upstream_repo"].(string)
			if upstreamOwner != "" && upstreamRepo != "" {
				fmt.Printf("      Fork of: %s/%s\n", upstreamOwner, upstreamRepo)
			}
		}
	}

	fmt.Println()
	format.Dimmed("Details: oat repo list | oat worker list")
	return nil
}

// staleTokenAge is the threshold past which a non-zero LastTokenUpdate is
// treated as stale (daemon-down, hung watcher, or agent crashed without
// cleanup). Surfaced in `oat status --tokens` via a ⚠ suffix on the age cell.
const staleTokenAge = 5 * time.Minute

// formatAge renders time.Since(t) as a compact human string:
//   - zero timestamp  -> "—"
//   - less than 1 min -> "45s"
//   - less than 1 h   -> "2m34s"
//   - 1 h or more     -> "1h5m"
//
// Negative deltas (clock skew) clamp to zero.
func formatAge(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		m := int(d.Minutes())
		s := int(d.Seconds()) - m*60
		return fmt.Sprintf("%dm%ds", m, s)
	default:
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		return fmt.Sprintf("%dh%dm", h, m)
	}
}

// systemStatusTokens prints a per-agent token usage table read directly
// from state.json (no daemon required). Shows input/output/cache, the
// cache hit rate, and the age of each agent's last token update for
// quick at-a-glance health. A ⚠ suffix on LAST UPDATE flags agents that
// have not reported token usage within staleTokenAge — common causes are
// a stopped daemon or a hung output watcher.
func (c *CLI) systemStatusTokens() error {
	s, err := state.Load(c.paths.StateFile)
	if err != nil {
		return fmt.Errorf("failed to load state: %w", err)
	}

	format.Header("OAT Token Usage")
	fmt.Println()

	repos := s.GetAllRepos()
	if len(repos) == 0 {
		format.Dimmed("  No repos tracked.")
		fmt.Println()
		return nil
	}

	// Collect rows, sort for stable output.
	type row struct {
		repo, agent, model string
		in, out, cr, cc    int64
		cost               *float64
		lastUpd            time.Time
	}
	var rows []row
	var grandIn, grandOut, grandCR, grandCC int64
	var grandCost float64
	var grandCostAny bool

	pricing := routing.LoadEmbeddedPricing()

	repoNames := make([]string, 0, len(repos))
	for name := range repos {
		repoNames = append(repoNames, name)
	}
	sort.Strings(repoNames)

	for _, repoName := range repoNames {
		repo := repos[repoName]
		if repo == nil || len(repo.Agents) == 0 {
			continue
		}
		agentNames := make([]string, 0, len(repo.Agents))
		for name := range repo.Agents {
			agentNames = append(agentNames, name)
		}
		sort.Strings(agentNames)
		for _, agentName := range agentNames {
			a := repo.Agents[agentName]
			cost := computeAgentCost(agentReport{
				Agent:         agentName,
				Model:         a.Model,
				InputTokens:   a.InputTokens,
				OutputTokens:  a.OutputTokens,
				CacheRead:     a.CacheReadTokens,
				CacheCreation: a.CacheCreationTokens,
			}, pricing)
			rows = append(rows, row{
				repo:    repoName,
				agent:   agentName,
				model:   a.Model,
				in:      a.InputTokens,
				out:     a.OutputTokens,
				cr:      a.CacheReadTokens,
				cc:      a.CacheCreationTokens,
				cost:    cost,
				lastUpd: a.LastTokenUpdate,
			})
			grandIn += a.InputTokens
			grandOut += a.OutputTokens
			grandCR += a.CacheReadTokens
			grandCC += a.CacheCreationTokens
			if cost != nil {
				grandCost += *cost
				grandCostAny = true
			}
		}
	}

	if len(rows) == 0 {
		format.Dimmed("  No agents registered.")
		fmt.Println()
		return nil
	}

	fmt.Printf("  %-55s %10s %8s %12s %12s %6s %10s %12s\n",
		"AGENT", "INPUT", "OUTPUT", "CACHE_READ", "CACHE_CREATE", "HIT%", "COST_USD", "LAST UPDATE")
	fmt.Println("  " + strings.Repeat("-", 137))

	var unpriced int
	for _, r := range rows {
		hitPct := ""
		if r.in > 0 {
			hitPct = fmt.Sprintf("%.1f%%", float64(r.cr)/float64(r.in)*100)
		} else {
			hitPct = "—"
		}
		label := r.repo + "/" + r.agent
		if len(label) > 55 {
			label = label[:52] + "..."
		}
		age := formatAge(r.lastUpd)
		// Staleness indicator: non-zero timestamp aged beyond threshold.
		// No color — matches existing near-budget pattern (cli.go ⚠ suffix).
		if !r.lastUpd.IsZero() && time.Since(r.lastUpd) > staleTokenAge {
			age += " ⚠"
		}
		if r.cost == nil {
			unpriced++
		}
		fmt.Printf("  %-55s %10d %8d %12d %12d %6s %10s %12s\n",
			label, r.in, r.out, r.cr, r.cc, hitPct, fmtCost(r.cost), age)
	}

	fmt.Println("  " + strings.Repeat("-", 137))
	hitPct := "—"
	if grandIn > 0 {
		hitPct = fmt.Sprintf("%.1f%%", float64(grandCR)/float64(grandIn)*100)
	}
	var grandCostPtr *float64
	if grandCostAny {
		grandCostPtr = &grandCost
	}
	// Grand-total row: aggregating ages across agents is misleading, leave "—".
	fmt.Printf("  %-55s %10d %8d %12d %12d %6s %10s %12s\n",
		"TOTAL", grandIn, grandOut, grandCR, grandCC, hitPct, fmtCost(grandCostPtr), "—")
	fmt.Println()
	if unpriced > 0 {
		format.Dimmed(fmt.Sprintf("Note: %d agent(s) show — for COST_USD — model not in internal/routing/pricing.yaml.", unpriced))
	}
	format.Dimmed("Hint: cache hit below 50 percent on a long-running agent is a red flag.")
	format.Dimmed("Hint: ⚠ on LAST UPDATE means no token events for 5+ minutes (daemon down or hung watcher).")
	return nil
}

func (c *CLI) daemonLogs(args []string) error {
	flags, _ := ParseFlags(args)

	// Check if we should follow logs
	follow := flags["follow"] == "true" || flags["f"] == "true"

	if follow {
		// Use tail -f to follow logs
		cmd := exec.CommandContext(c.cmdCtx(), "tail", "-f", c.paths.DaemonLog)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	// Show last 50 lines
	lines := "50"
	if n, ok := flags["n"]; ok {
		lines = n
	}

	cmd := exec.CommandContext(c.cmdCtx(), "tail", "-n", lines, c.paths.DaemonLog)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (c *CLI) stopAll(args []string) error {
	flags, _ := ParseFlags(args)
	clean := flags["clean"] == "true"
	skipConfirm := flags["yes"] == "true"

	// Get list of repos (try daemon first, then state file)
	var repos []string
	client := socket.NewClient(c.paths.DaemonSock)
	resp, err := client.Send(socket.Request{Command: "list_repos"})
	if err == nil && resp.Success {
		// Daemon is running, get repos from it
		if repoList, ok := resp.Data.([]interface{}); ok {
			for _, repo := range repoList {
				if repoStr, ok := repo.(string); ok {
					repos = append(repos, repoStr)
				}
			}
		}
	} else {
		// Daemon not running, try to load from state file
		st, err := state.Load(c.paths.StateFile)
		if err == nil {
			repos = st.ListRepos()
		}
	}

	// If --clean is specified, require confirmation
	if clean {
		fmt.Println("WARNING: This will permanently delete:")
		fmt.Println("  - All worktrees (~/.oat/wts/)")
		fmt.Println("  - All agent state (state.json agents section)")
		fmt.Println("  - All message queues (~/.oat/messages/)")
		fmt.Println("  - All output logs (~/.oat/output/)")
		fmt.Println("  - All agent configs (~/.oat/agent-config/)")
		fmt.Println("  - All prompts (~/.oat/prompts/)")
		fmt.Println("  - Local branches (work/*, oat/*)")
		fmt.Println()
		fmt.Println("The following will be PRESERVED:")
		fmt.Println("  - Cloned repositories (~/.oat/repos/)")
		fmt.Println("  - Git credentials")
		fmt.Println()

		if !skipConfirm {
			fmt.Print("Type 'NUKE' to confirm: ")
			reader := bufio.NewReader(os.Stdin)
			input, err := reader.ReadString('\n')
			if err != nil {
				return fmt.Errorf("failed to read input: %w", err)
			}
			input = strings.TrimSpace(input)
			if input != "NUKE" {
				fmt.Println("Aborted.")
				return nil
			}
			fmt.Println()
		}
	}

	fmt.Println("Stopping all oat sessions...")

	// Destroy all known sessions via backend
	for _, repo := range repos {
		sessionName := fmt.Sprintf("oat-%s", repo)
		exists, err := c.backend.HasSession(context.Background(), sessionName)
		if err == nil && exists {
			fmt.Printf("Destroying session: %s\n", sessionName)
			if err := c.backend.DestroySession(context.Background(), sessionName); err != nil {
				fmt.Printf("Warning: failed to destroy session %s: %v\n", sessionName, err)
			}
		}
	}

	// Also check for any orphaned oat-* sessions
	sessions, err := c.backend.ListSessions(context.Background())
	if err == nil {
		knownSessions := make(map[string]bool)
		for _, repo := range repos {
			knownSessions[fmt.Sprintf("oat-%s", repo)] = true
		}
		for _, session := range sessions {
			if strings.HasPrefix(session, "oat-") && !knownSessions[session] {
				fmt.Printf("Destroying orphaned session: %s\n", session)
				if err := c.backend.DestroySession(context.Background(), session); err != nil {
					fmt.Printf("Warning: failed to destroy session %s: %v\n", session, err)
				}
			}
		}
	}

	// Stop the daemon
	fmt.Println("Stopping daemon...")
	resp, err = client.Send(socket.Request{Command: "stop"})
	if err != nil {
		fmt.Printf("Daemon already stopped or not responding\n")
	} else if resp.Success {
		fmt.Println("Daemon stopped")
	}

	// Full cleanup if --clean is specified
	if clean {
		// Remove worktrees directory
		fmt.Println("\nRemoving worktrees...")
		removeDirectoryIfExists(c.paths.WorktreesDir, "worktrees")

		// Remove messages directory
		fmt.Println("Removing messages...")
		removeDirectoryIfExists(c.paths.MessagesDir, "messages")

		// Remove output logs
		fmt.Println("Removing output logs...")
		removeDirectoryIfExists(c.paths.OutputDir, "output logs")

		// Remove agent config (per-agent settings)
		fmt.Println("Removing agent configs...")
		removeDirectoryIfExists(c.paths.ArchiveDir, "agent configs")

		// Remove prompts directory
		fmt.Println("Removing prompts...")
		promptsDir := filepath.Join(c.paths.Root, "prompts")
		removeDirectoryIfExists(promptsDir, "prompts")

		// Clean up local branches in each repository
		fmt.Println("\nCleaning up local branches...")
		for _, repoName := range repos {
			repoPath := c.paths.RepoDir(repoName)
			if _, err := os.Stat(repoPath); os.IsNotExist(err) {
				continue
			}

			fmt.Printf("  Repository: %s\n", repoName)

			// Delete work/* and oat/* branches
			wt := worktree.NewManagerWithContext(c.cmdCtx(), repoPath)
			for _, prefix := range []string{"work/", "oat/"} {
				branches, err := c.listBranchesWithPrefix(repoPath, prefix)
				if err != nil {
					fmt.Printf("    Warning: failed to list %s branches: %v\n", prefix, err)
					continue
				}
				for _, branch := range branches {
					// First remove any worktree associated with this branch.
					// Ignore errors: worktree may not exist.
					_ = wt.Remove(branch, true)
					// Delete the branch
					if err := c.deleteBranch(repoPath, branch); err != nil {
						fmt.Printf("    Warning: failed to delete branch %s: %v\n", branch, err)
					} else {
						fmt.Printf("    Deleted branch: %s\n", branch)
					}
				}
			}

			// Prune worktrees
			if err := wt.Prune(); err != nil {
				fmt.Printf("    Warning: failed to prune worktrees: %v\n", err)
			}
		}

		// Clear agent state but preserve repository entries
		fmt.Println("\nClearing agent state...")
		st, err := state.Load(c.paths.StateFile)
		if err == nil {
			if err := st.ClearAllAgents(); err != nil {
				fmt.Printf("  Warning: failed to clear agents: %v\n", err)
			} else if err := st.Save(); err != nil {
				fmt.Printf("  Warning: failed to save state: %v\n", err)
			} else {
				fmt.Println("  Cleared all agents from state")
			}
		}

		// Remove daemon files (they'll be recreated on next start)
		fmt.Println("Cleaning up daemon files...")

		// Helper function to remove file with proper error handling
		removeFileWithFeedback := func(path, description string) {
			if err := os.Remove(path); err != nil {
				if os.IsNotExist(err) {
					// File doesn't exist, which is fine - no need to report
					return
				}
				// Report actual errors (permissions, disk issues, etc.)
				fmt.Printf("  Warning: failed to remove %s: %v\n", description, err)
			} else {
				fmt.Printf("  Removed %s\n", description)
			}
		}

		removeFileWithFeedback(c.paths.DaemonPID, "daemon PID file")
		removeFileWithFeedback(c.paths.DaemonSock, "daemon socket")
		removeFileWithFeedback(c.paths.DaemonLog, "daemon log file")

		fmt.Println("\n✓ Full cleanup complete! OAT has been reset to a clean state.")
		fmt.Println("Your repositories are preserved at:", c.paths.ReposDir)
		fmt.Println("\nRun 'oat start' to begin fresh.")
	} else {
		fmt.Println("\n✓ All oat sessions stopped")
	}

	return nil
}

func (c *CLI) initRepo(args []string) error {
	flags, posArgs := ParseFlags(args)

	if len(posArgs) < 1 {
		return errors.InvalidUsage("usage: oat init <github-url> [name] [--model=<model>] [--no-merge-queue] [--mq-track=all|author|assigned]")
	}

	githubURL := strings.TrimRight(posArgs[0], "/")

	// Parse repository name from URL if not provided
	var repoName string
	if len(posArgs) >= 2 {
		repoName = posArgs[1]
	} else {
		// Extract repo name from URL (e.g., github.com/user/repo -> repo)
		// A valid GitHub URL has format: https://github.com/owner/repo
		// When split by "/": ["https:", "", "github.com", "owner", "repo"] - 5+ parts
		parts := strings.Split(githubURL, "/")
		if len(parts) < 5 {
			return errors.InvalidUsage("could not determine repository name from URL; please provide a name: oat init <url> <name>")
		}
		repoName = strings.TrimSuffix(parts[len(parts)-1], ".git")
	}

	// Validate repository name before any operations
	if repoName == "" {
		return errors.InvalidUsage("could not determine repository name from URL; please provide a name: oat init <url> <name>")
	}

	// Preflight: bail out if no provider API key is set. Without one,
	// `start_repo_agents` succeeds at the daemon layer but every agent
	// immediately fails its first LLM call — a terrible first-run UX.
	// Uses the same detection as `oat doctor` (env + ~/.oat/.env).
	// Skipped in test mode where no real agents are started.
	if os.Getenv("OAT_TEST_MODE") != "1" && !preflight.HasAnyLLMKey(c.paths) {
		return errors.MissingAPIKey()
	}

	// Parse merge queue configuration flags
	mqEnabled := flags["no-merge-queue"] != "true"
	mqTrackMode := state.TrackModeAll
	if trackMode, ok := flags["mq-track"]; ok {
		switch trackMode {
		case "all":
			mqTrackMode = state.TrackModeAll
		case "author":
			mqTrackMode = state.TrackModeAuthor
		case "assigned":
			mqTrackMode = state.TrackModeAssigned
		default:
			return fmt.Errorf("invalid --mq-track value: %s (must be 'all', 'author', or 'assigned')", trackMode)
		}
	}

	mqConfig := state.MergeQueueConfig{
		Enabled:   mqEnabled,
		TrackMode: mqTrackMode,
	}

	// Parse --model flag for LLM model selection
	model := flags["model"]

	fmt.Printf("Initializing repository: %s\n", repoName)
	fmt.Printf("GitHub URL: %s\n", githubURL)
	if mqEnabled {
		fmt.Printf("Merge queue: enabled (tracking: %s)\n", mqTrackMode)
	} else {
		fmt.Printf("Merge queue: disabled\n")
	}
	if model != "" {
		fmt.Printf("Model: %s\n", model)
	} else {
		fmt.Printf("Model: auto-detect (based on available API keys)\n")
	}

	// Check if daemon is running
	client := socket.NewClient(c.paths.DaemonSock)
	if _, err := client.Send(socket.Request{Command: "ping"}); err != nil {
		return errors.DaemonNotRunning()
	}

	// Check if repository is already initialized
	st, err := state.Load(c.paths.StateFile)
	if err != nil {
		return fmt.Errorf("failed to load state: %w", err)
	}
	if _, exists := st.GetRepo(repoName); exists {
		return fmt.Errorf("repository '%s' is already initialized\nUse 'oat repo rm %s' to remove it first, or choose a different name", repoName, repoName)
	}

	// Check if session already exists (stale session from previous incomplete init)
	sessionName := sanitizeSessionName(repoName)
	if sessionName == "oat-" {
		return fmt.Errorf("invalid session name: repository name cannot be empty")
	}
	if exists, err := c.backend.HasSession(context.Background(), sessionName); err == nil && exists {
		fmt.Printf("Warning: Session '%s' already exists\n", sessionName)
		fmt.Printf("This may be from a previous incomplete initialization.\n")
		fmt.Printf("Auto-repairing: destroying existing session...\n")
		if err := c.backend.DestroySession(context.Background(), sessionName); err != nil {
			return fmt.Errorf("failed to clean up existing session: %w", err)
		}
		fmt.Println("✓ Cleaned up stale session")
	}

	// Check if repository directory already exists
	repoPath := c.paths.RepoDir(repoName)
	if _, err := os.Stat(repoPath); err == nil {
		return fmt.Errorf("directory already exists: %s\nRemove it manually or choose a different name", repoPath)
	}

	// Clone repository
	fmt.Printf("Cloning to: %s\n", repoPath)

	cmd := exec.CommandContext(c.cmdCtx(), "git", "clone", githubURL, repoPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return errors.GitOperationFailed("clone", err)
	}

	// Ensure the "oat" label exists on the GitHub repo (workers tag PRs with it)
	fmt.Println("Ensuring 'oat' label exists on GitHub repository...")
	labelCmd := exec.CommandContext(c.cmdCtx(), "gh", "label", "create", "oat",
		"--description", "Created by oat orchestrator",
		"--color", "1d76db",
		"--force",
	)
	labelCmd.Dir = repoPath
	if labelOut, labelErr := labelCmd.CombinedOutput(); labelErr != nil {
		fmt.Printf("Warning: Failed to create 'oat' label: %s\n", strings.TrimSpace(string(labelOut)))
		fmt.Println("Workers may fail to create PRs. Create it manually: gh label create oat")
	}

	// Detect if this is a fork
	forkInfo, err := fork.DetectFork(repoPath)
	if err != nil {
		fmt.Printf("Warning: Failed to detect fork status: %v\n", err)
		forkInfo = &fork.ForkInfo{IsFork: false}
	}

	// Store fork config
	var forkConfig state.ForkConfig
	if forkInfo.IsFork {
		fmt.Printf("Detected fork of %s/%s\n", forkInfo.UpstreamOwner, forkInfo.UpstreamRepo)
		forkConfig = state.ForkConfig{
			IsFork:        true,
			UpstreamURL:   forkInfo.UpstreamURL,
			UpstreamOwner: forkInfo.UpstreamOwner,
			UpstreamRepo:  forkInfo.UpstreamRepo,
		}

		// Add upstream remote if not already present
		if !fork.HasUpstreamRemote(repoPath) {
			fmt.Printf("Adding upstream remote: %s\n", forkInfo.UpstreamURL)
			if err := fork.AddUpstreamRemote(repoPath, forkInfo.UpstreamURL); err != nil {
				fmt.Printf("Warning: Failed to add upstream remote: %v\n", err)
			}
		}

		// In fork mode, disable merge-queue and enable pr-shepherd by default
		mqConfig.Enabled = false
		mqEnabled = false
	}

	// PR Shepherd config (used in fork mode)
	psConfig := state.DefaultPRShepherdConfig()
	psEnabled := forkInfo.IsFork && psConfig.Enabled

	// Copy agent templates to per-repo agents directory
	agentsDir := c.paths.RepoAgentsDir(repoName)
	fmt.Printf("Copying agent templates to: %s\n", agentsDir)
	if err := templates.CopyAgentTemplates(agentsDir); err != nil {
		return fmt.Errorf("failed to copy agent templates: %w", err)
	}

	// Create shared scratchpad directory for cross-worker knowledge sharing
	scratchpadDir := c.paths.ScratchpadDir(repoName)
	if err := os.MkdirAll(scratchpadDir, 0755); err != nil {
		fmt.Printf("Warning: failed to create scratchpad directory: %v\n", err)
	}

	// Session and agent processes are started by the daemon (which owns the
	// backend and keeps agents alive after the CLI exits). We register state
	// here and send start_repo_agents at the end.

	// Create isolated worktrees for persistent agents so each gets its own .oat/AGENTS.md
	wtMgr := worktree.NewManagerWithContext(c.cmdCtx(), repoPath)
	supervisorWtPath := c.paths.AgentWorktree(repoName, "supervisor")
	fmt.Printf("Creating supervisor worktree at: %s\n", supervisorWtPath)
	if err := wtMgr.CreateDetached(supervisorWtPath, "HEAD"); err != nil {
		return fmt.Errorf("failed to create supervisor worktree: %w", err)
	}
	if err := wtMgr.CheckoutBranch(supervisorWtPath, "main"); err != nil {
		fmt.Printf("Warning: failed to checkout main in supervisor worktree: %v\n", err)
	}

	// Copy hooks configuration to supervisor worktree
	if err := hooks.CopyConfig(repoPath, supervisorWtPath); err != nil {
		fmt.Printf("Warning: failed to copy hooks config to supervisor: %v\n", err)
	}

	var mqWtPath, psWtPath string
	if mqEnabled {
		mqWtPath = c.paths.AgentWorktree(repoName, "merge-queue")
		fmt.Printf("Creating merge-queue worktree at: %s\n", mqWtPath)
		if err := wtMgr.CreateDetached(mqWtPath, "HEAD"); err != nil {
			return fmt.Errorf("failed to create merge-queue worktree: %w", err)
		}
		if err := wtMgr.CheckoutBranch(mqWtPath, "main"); err != nil {
			fmt.Printf("Warning: failed to checkout main in merge-queue worktree: %v\n", err)
		}
		if err := hooks.CopyConfig(repoPath, mqWtPath); err != nil {
			fmt.Printf("Warning: failed to copy hooks config to merge-queue: %v\n", err)
		}
	} else if psEnabled {
		psWtPath = c.paths.AgentWorktree(repoName, "pr-shepherd")
		fmt.Printf("Creating pr-shepherd worktree at: %s\n", psWtPath)
		if err := wtMgr.CreateDetached(psWtPath, "HEAD"); err != nil {
			return fmt.Errorf("failed to create pr-shepherd worktree: %w", err)
		}
		if err := wtMgr.CheckoutBranch(psWtPath, "main"); err != nil {
			fmt.Printf("Warning: failed to checkout main in pr-shepherd worktree: %v\n", err)
		}
		if err := hooks.CopyConfig(repoPath, psWtPath); err != nil {
			fmt.Printf("Warning: failed to copy hooks config to pr-shepherd: %v\n", err)
		}
	}

	// Add repository to daemon state (with merge queue and fork config)
	addRepoArgs := map[string]interface{}{
		"name":          repoName,
		"github_url":    githubURL,
		"session_name":  sessionName,
		"mq_enabled":    mqConfig.Enabled,
		"mq_track_mode": string(mqConfig.TrackMode),
		"ps_enabled":    psConfig.Enabled,
		"ps_track_mode": string(psConfig.TrackMode),
		"is_fork":       forkConfig.IsFork,
	}
	if model != "" {
		addRepoArgs["model"] = model
	}
	if forkConfig.IsFork {
		addRepoArgs["upstream_url"] = forkConfig.UpstreamURL
		addRepoArgs["upstream_owner"] = forkConfig.UpstreamOwner
		addRepoArgs["upstream_repo"] = forkConfig.UpstreamRepo
	}
	resp, err := client.Send(socket.Request{
		Command: "add_repo",
		Args:    addRepoArgs,
	})
	if err != nil {
		return fmt.Errorf("failed to register repository with daemon: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("failed to register repository: %s", resp.Error)
	}

	// Add supervisor agent
	resp, err = client.Send(socket.Request{
		Command: "add_agent",
		Args: map[string]interface{}{
			"repo":          repoName,
			"agent":         "supervisor",
			"type":          "supervisor",
			"worktree_path": supervisorWtPath,
			"window_name":   "supervisor",
		},
	})
	if err != nil {
		return fmt.Errorf("failed to register supervisor: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("failed to register supervisor: %s", resp.Error)
	}

	// Add merge-queue agent only if enabled (non-fork mode)
	if mqEnabled {
		resp, err = client.Send(socket.Request{
			Command: "add_agent",
			Args: map[string]interface{}{
				"repo":          repoName,
				"agent":         "merge-queue",
				"type":          "merge-queue",
				"worktree_path": mqWtPath,
				"window_name":   "merge-queue",
			},
		})
		if err != nil {
			return fmt.Errorf("failed to register merge-queue: %w", err)
		}
		if !resp.Success {
			return fmt.Errorf("failed to register merge-queue: %s", resp.Error)
		}
	}

	// Add pr-shepherd agent only if enabled (fork mode)
	if psEnabled {
		resp, err = client.Send(socket.Request{
			Command: "add_agent",
			Args: map[string]interface{}{
				"repo":          repoName,
				"agent":         "pr-shepherd",
				"type":          "pr-shepherd",
				"worktree_path": psWtPath,
				"window_name":   "pr-shepherd",
			},
		})
		if err != nil {
			return fmt.Errorf("failed to register pr-shepherd: %w", err)
		}
		if !resp.Success {
			return fmt.Errorf("failed to register pr-shepherd: %s", resp.Error)
		}
	}

	// Create default workspace worktree
	wt := worktree.NewManagerWithContext(c.cmdCtx(), repoPath)
	workspacePath := c.paths.AgentWorktree(repoName, "default")

	// Check for and migrate legacy "workspace" branch to "workspace/default"
	// This allows the new workspace/<name> naming convention to work
	migrated, err := wt.MigrateLegacyWorkspaceBranch()
	if err != nil {
		// Check if it's a conflict state that requires manual resolution
		hasConflict, suggestion, checkErr := wt.CheckWorkspaceBranchConflict()
		if checkErr == nil && hasConflict {
			return fmt.Errorf("workspace branch conflict detected:\n%s", suggestion)
		}
		return fmt.Errorf("failed to check workspace branch state: %w", err)
	}
	if migrated {
		fmt.Println("Migrated legacy 'workspace' branch to 'workspace/default'")
	}
	workspaceBranch := "workspace/default"

	fmt.Printf("Creating default workspace worktree at: %s\n", workspacePath)
	if err := wt.CreateNewBranch(workspacePath, workspaceBranch, "HEAD"); err != nil {
		return fmt.Errorf("failed to create default workspace worktree: %w", err)
	}

	// Add default workspace agent
	resp, err = client.Send(socket.Request{
		Command: "add_agent",
		Args: map[string]interface{}{
			"repo":          repoName,
			"agent":         "default",
			"type":          "workspace",
			"worktree_path": workspacePath,
			"window_name":   "default",
		},
	})
	if err != nil {
		return fmt.Errorf("failed to register default workspace: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("failed to register default workspace: %s", resp.Error)
	}

	// Create planner worktree
	plannerWtPath := c.paths.AgentWorktree(repoName, "planner")
	fmt.Printf("Creating planner worktree at: %s\n", plannerWtPath)
	if err := wtMgr.CreateDetached(plannerWtPath, "HEAD"); err != nil {
		return fmt.Errorf("failed to create planner worktree: %w", err)
	}
	// Match supervisor/merge-queue/pr-shepherd: a missing "main" branch is a
	// warning, not fatal. Repos using "master" (or other default branch names)
	// still get a working planner worktree at the detached HEAD.
	if err := wtMgr.CheckoutBranch(plannerWtPath, "main"); err != nil {
		fmt.Printf("Warning: failed to checkout main in planner worktree: %v\n", err)
	}

	// Select the best available model for the planner (prefers Anthropic for
	// reasoning quality). Falls back silently so init never fails on this.
	plannerModel, _ := routing.GetModelForTask("planner")

	// Add planner agent
	addPlannerArgs := map[string]interface{}{
		"repo":          repoName,
		"agent":         "planner",
		"type":          "planner",
		"worktree_path": plannerWtPath,
		"window_name":   "planner",
	}
	if plannerModel != "" {
		addPlannerArgs["model"] = plannerModel
	}
	resp, err = client.Send(socket.Request{
		Command: "add_agent",
		Args:    addPlannerArgs,
	})
	if err != nil {
		return fmt.Errorf("failed to register planner: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("failed to register planner: %s", resp.Error)
	}

	// Ask the daemon to create the backend session and start all agents.
	// The daemon owns the backend, so agent processes survive CLI exit.
	// Forward relevant env vars so agents inherit tokens even when the
	// daemon's own environment lacks them (common with direct backend).
	fmt.Println("Starting agents via daemon...")
	cliEnv := collectCLIEnvVars()
	startArgs := map[string]interface{}{
		"repo": repoName,
	}
	if len(cliEnv) > 0 {
		startArgs["cli_env"] = cliEnv
	}
	resp, err = client.Send(socket.Request{
		Command: "start_repo_agents",
		Args:    startArgs,
	})
	if err != nil {
		return fmt.Errorf("failed to start agents: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("failed to start agents: %s", resp.Error)
	}

	// Report agent PIDs from daemon response
	if results, ok := resp.Data.([]interface{}); ok {
		for _, r := range results {
			if m, ok := r.(map[string]interface{}); ok {
				name, _ := m["name"].(string)
				pid, _ := m["pid"].(float64)
				if name != "" && pid > 0 {
					fmt.Printf("  Agent %s started (PID %d)\n", name, int(pid))
				}
			}
		}
	}

	fmt.Println()
	fmt.Println("✓ Repository initialized successfully!")
	fmt.Printf("  Session: %s\n", sessionName)
	if mqEnabled {
		fmt.Printf("  Agents: supervisor, merge-queue, planner, default (workspace)\n")
	} else {
		fmt.Printf("  Agents: supervisor, planner, default (workspace)\n")
	}
	fmt.Printf("\nMonitor agents: oat ui --repo %s\n", repoName)
	fmt.Printf("Or tail one agent: oat attach <agent-name> --repo %s\n", repoName)

	return nil
}

func (c *CLI) listRepos(args []string) error {
	resp, err := c.sendDaemonRequest("list_repos", map[string]interface{}{
		"rich": true,
	})
	if err != nil {
		return err
	}

	repos, ok := resp.Data.([]interface{})
	if !ok {
		return errors.New(errors.CategoryRuntime, "unexpected response format from daemon")
	}

	if len(repos) == 0 {
		fmt.Println("No repositories tracked")
		format.Dimmed("\nInitialize a repository with: oat init <github-url>")
		return nil
	}

	format.Header("Tracked repositories (%d):", len(repos))
	fmt.Println()

	table := format.NewColoredTable("REPO", "MODE", "AGENTS", "STATUS", "SESSION")
	for _, repo := range repos {
		if repoMap, ok := repo.(map[string]interface{}); ok {
			name, _ := repoMap["name"].(string)
			totalAgents := 0
			if v, ok := repoMap["total_agents"].(float64); ok {
				totalAgents = int(v)
			}
			workerCount := 0
			if v, ok := repoMap["worker_count"].(float64); ok {
				workerCount = int(v)
			}
			sessionHealthy, _ := repoMap["session_healthy"].(bool)
			sessionName, _ := repoMap["session_name"].(string)

			// Get fork info
			isFork, _ := repoMap["is_fork"].(bool)
			upstreamOwner, _ := repoMap["upstream_owner"].(string)
			upstreamRepo, _ := repoMap["upstream_repo"].(string)

			// Format mode string
			var modeStr string
			if isFork {
				modeStr = fmt.Sprintf("fork of %s/%s", upstreamOwner, upstreamRepo)
			} else {
				modeStr = "upstream"
			}

			// Format agent count
			agentStr := fmt.Sprintf("%d total", totalAgents)
			if workerCount > 0 {
				agentStr = fmt.Sprintf("%d (%d workers)", totalAgents, workerCount)
			}

			// Format status
			var statusCell format.ColoredCell
			if sessionHealthy {
				statusCell = format.ColorCell(format.ColoredStatus(format.StatusHealthy), nil)
			} else {
				statusCell = format.ColorCell(format.ColoredStatus(format.StatusError), nil)
			}

			table.AddRow(
				format.Cell(name),
				format.ColorCell(modeStr, format.Dim),
				format.Cell(agentStr),
				statusCell,
				format.ColorCell(sessionName, format.Dim),
			)
		}
	}
	table.Print()

	return nil
}

func (c *CLI) removeRepo(args []string) error {
	var repoName string
	if len(args) > 0 {
		repoName = args[0]
	} else {
		// Interactive selection - list repos
		client := socket.NewClient(c.paths.DaemonSock)
		resp, err := client.Send(socket.Request{
			Command: "list_repos",
			Args: map[string]interface{}{
				"rich": true,
			},
		})
		if err != nil {
			return errors.DaemonCommunicationFailed("listing repositories", err)
		}
		if !resp.Success {
			return errors.Wrap(errors.CategoryRuntime, "failed to list repos", fmt.Errorf("%s", resp.Error))
		}

		repos, ok := resp.Data.([]interface{})
		if !ok {
			return errors.Wrap(errors.CategoryRuntime, "failed to list repos", fmt.Errorf("daemon returned unexpected data type, expected []interface{} but got %T", resp.Data))
		}
		items := reposToSelectableItems(repos)
		if len(items) == 0 {
			return errors.NoRepositoriesFound()
		}
		selected, err := SelectFromList("Select repository to remove:", items)
		if err != nil {
			return err
		}
		if selected == "" {
			fmt.Println("Canceled")
			return nil
		}
		repoName = selected
	}

	fmt.Printf("Removing repository '%s'...\n", repoName)

	// Get repo info from daemon
	client := socket.NewClient(c.paths.DaemonSock)
	resp, err := client.Send(socket.Request{
		Command: "list_agents",
		Args: map[string]interface{}{
			"repo": repoName,
		},
	})
	if err != nil {
		return errors.DaemonCommunicationFailed("getting repo info", err)
	}
	if !resp.Success {
		return errors.Wrap(errors.CategoryRuntime, "failed to get repo info", fmt.Errorf("%s", resp.Error))
	}

	// Get list of agents
	agents, ok := resp.Data.([]interface{})
	if !ok {
		return errors.Wrap(errors.CategoryRuntime, "failed to get repo info", fmt.Errorf("daemon returned unexpected data type, expected []interface{} but got %T", resp.Data))
	}

	// Check for any workers with uncommitted changes
	for _, agent := range agents {
		if agentMap, ok := agent.(map[string]interface{}); ok {
			agentType, ok := agentMap["type"].(string)
			if !ok {
				continue // Skip agents with invalid type field
			}
			if agentType == "worker" || agentType == "review" || agentType == "verification" {
				wtPath, ok := agentMap["worktree_path"].(string)
				if !ok {
					continue // Skip agents with invalid worktree_path field
				}
				if wtPath != "" {
					hasUncommitted, err := worktree.HasUncommittedChanges(c.cmdCtx(), wtPath)
					if err == nil && hasUncommitted {
						agentName, ok := agentMap["name"].(string)
						if !ok {
							agentName = "<unknown>" // Default name if type assertion fails
						}
						fmt.Printf("\nWarning: Agent '%s' has uncommitted changes!\n", agentName)
						fmt.Println("Files may be lost if you continue.")
						fmt.Print("Continue with removal? [y/N]: ")

						var response string
						_, _ = fmt.Scanln(&response) // EOF/empty -> treated as "N" below
						if response != "y" && response != "Y" {
							fmt.Println("Removal canceled")
							return nil
						}
						break // Only ask once
					}
				}
			}
		}
	}

	// Destroy session via backend
	sessionName := sanitizeSessionName(repoName)
	if exists, err := c.backend.HasSession(context.Background(), sessionName); err == nil && exists {
		fmt.Printf("Destroying session: %s\n", sessionName)
		if err := c.backend.DestroySession(context.Background(), sessionName); err != nil {
			fmt.Printf("Warning: failed to destroy session: %v\n", err)
		}
	}

	// Remove worktrees for all agents
	repoPath := c.paths.RepoDir(repoName)
	wt := worktree.NewManagerWithContext(c.cmdCtx(), repoPath)
	for _, agent := range agents {
		if agentMap, ok := agent.(map[string]interface{}); ok {
			wtPath, ok := agentMap["worktree_path"].(string)
			if !ok {
				continue // Skip agents with invalid worktree_path field
			}
			agentName, ok := agentMap["name"].(string)
			if !ok {
				agentName = "<unknown>" // Default name if type assertion fails
			}
			if wtPath != "" && wtPath != repoPath {
				fmt.Printf("Removing worktree for '%s': %s\n", agentName, wtPath)
				if err := wt.Remove(wtPath, true); err != nil {
					fmt.Printf("Warning: failed to remove worktree: %v\n", err)
				}
			}
		}
	}

	// Remove the worktrees directory for this repo
	wtDir := c.paths.WorktreeDir(repoName)
	if _, err := os.Stat(wtDir); err == nil {
		fmt.Printf("Removing worktrees directory: %s\n", wtDir)
		if err := os.RemoveAll(wtDir); err != nil {
			fmt.Printf("Warning: failed to remove worktrees directory: %v\n", err)
		}
	}

	// Clean up messages directory for this repo
	msgDir := filepath.Join(c.paths.MessagesDir, repoName)
	if _, err := os.Stat(msgDir); err == nil {
		fmt.Printf("Removing messages directory: %s\n", msgDir)
		if err := os.RemoveAll(msgDir); err != nil {
			fmt.Printf("Warning: failed to remove messages directory: %v\n", err)
		}
	}

	// Clean up output logs for this repo
	outputDir := filepath.Join(c.paths.OutputDir, repoName)
	if _, err := os.Stat(outputDir); err == nil {
		fmt.Printf("Removing output logs: %s\n", outputDir)
		if err := os.RemoveAll(outputDir); err != nil {
			fmt.Printf("Warning: failed to remove output logs: %v\n", err)
		}
	}

	// Unregister from daemon
	resp, err = client.Send(socket.Request{
		Command: "remove_repo",
		Args: map[string]interface{}{
			"name": repoName,
		},
	})
	if err != nil {
		return errors.DaemonCommunicationFailed("removing repo", err)
	}
	if !resp.Success {
		return errors.Wrap(errors.CategoryRuntime, "failed to remove repo from state", fmt.Errorf("%s", resp.Error))
	}

	fmt.Println("✓ Repository removed successfully")
	fmt.Printf("\nNote: The cloned repository at '%s' was NOT deleted.\n", repoPath)
	fmt.Println("Delete it manually if you no longer need it.")
	return nil
}

func (c *CLI) setCurrentRepo(args []string) error {
	if len(args) < 1 {
		return errors.InvalidUsage("usage: oat repo use <name>")
	}

	repoName := args[0]

	_, err := c.sendDaemonRequest("set_current_repo", map[string]interface{}{
		"name": repoName,
	})
	if err != nil {
		return err
	}

	fmt.Printf("Current repository set to: %s\n", repoName)
	return nil
}

func (c *CLI) getCurrentRepo(args []string) error {
	resp, err := c.sendDaemonRequest("get_current_repo", nil)
	if err != nil {
		return err
	}

	currentRepo, _ := resp.Data.(string)
	if currentRepo == "" {
		fmt.Println("No current repository set")
		fmt.Println("\nUse 'oat repo use <name>' to set one")
	} else {
		fmt.Printf("Current repository: %s\n", currentRepo)
	}
	return nil
}

func (c *CLI) clearCurrentRepo(args []string) error {
	_, err := c.sendDaemonRequest("clear_current_repo", nil)
	if err != nil {
		return err
	}

	fmt.Println("Current repository cleared")
	return nil
}

func (c *CLI) configRepo(args []string) error {
	flags, posArgs := ParseFlags(args)

	// Determine repository
	var repoName string
	if len(posArgs) >= 1 {
		repoName = posArgs[0]
	} else {
		// Try to infer from current directory
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get current directory: %w", err)
		}

		// Check if we're in a tracked repo
		repos := c.getReposList()
		for _, repo := range repos {
			repoPath := c.paths.RepoDir(repo)
			if strings.HasPrefix(cwd, repoPath) {
				repoName = repo
				break
			}
		}

		if repoName == "" {
			// If only one repo exists, use it
			if len(repos) == 1 {
				repoName = repos[0]
			} else {
				return fmt.Errorf("please specify a repository name or run from within a tracked repository")
			}
		}
	}

	// Check for "worker-models" subcommand (e.g., oat config my-repo worker-models set "a,b")
	if len(posArgs) >= 2 && posArgs[1] == "worker-models" {
		return c.configWorkerModels(repoName, posArgs[2:])
	}

	// Handle --allowed-worker-models / --available-worker-models / --allowed-models flag (shorthand for worker-models set)
	allowedModels := ""
	hasAllowedModels := false
	for _, key := range []string{"allowed-worker-models", "available-worker-models", "allowed-models"} {
		if v, ok := flags[key]; ok {
			allowedModels = v
			hasAllowedModels = true
			break
		}
	}

	// Check if any config flags are provided
	hasMqEnabled := flags["mq-enabled"] != ""
	hasMqTrack := flags["mq-track"] != ""
	hasPsEnabled := flags["ps-enabled"] != ""
	hasPsTrack := flags["ps-track"] != ""
	hasWSD := flags["workspace-stuck-detection"] != ""

	if !hasMqEnabled && !hasMqTrack && !hasPsEnabled && !hasPsTrack && !hasWSD && !hasAllowedModels {
		// No flags - just show current config
		return c.showRepoConfig(repoName)
	}

	// Apply config changes
	if err := c.updateRepoConfig(repoName, flags); err != nil {
		return err
	}

	// Handle allowed worker models flag separately (shorthand for worker-models set/clear)
	if hasAllowedModels {
		if allowedModels == "" {
			return c.configWorkerModels(repoName, []string{"clear"})
		}
		return c.configWorkerModels(repoName, []string{"set", allowedModels})
	}

	return nil
}

func (c *CLI) showRepoConfig(repoName string) error {
	client := socket.NewClient(c.paths.DaemonSock)
	resp, err := client.Send(socket.Request{
		Command: "get_repo_config",
		Args: map[string]interface{}{
			"name": repoName,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to get repo config: %w (is daemon running?)", err)
	}

	if !resp.Success {
		return fmt.Errorf("failed to get repo config: %s", resp.Error)
	}

	// Parse response
	configMap, ok := resp.Data.(map[string]interface{})
	if !ok {
		return fmt.Errorf("unexpected response format")
	}

	fmt.Printf("Configuration for repository: %s\n\n", repoName)

	// Show fork info if this is a fork
	isFork, _ := configMap["is_fork"].(bool)
	if isFork {
		upstreamOwner, _ := configMap["upstream_owner"].(string)
		upstreamRepo, _ := configMap["upstream_repo"].(string)
		fmt.Printf("Fork Mode: Yes (fork of %s/%s)\n\n", upstreamOwner, upstreamRepo)
	} else {
		fmt.Println("Fork Mode: No (upstream/direct repository)")
		fmt.Println()
	}

	// Show merge queue config
	fmt.Println("Merge Queue:")
	mqEnabled := true
	if enabled, ok := configMap["mq_enabled"].(bool); ok {
		mqEnabled = enabled
	}
	mqTrackMode := "all"
	if trackMode, ok := configMap["mq_track_mode"].(string); ok {
		mqTrackMode = trackMode
	}
	if mqEnabled {
		fmt.Printf("  Enabled: true\n")
		fmt.Printf("  Track mode: %s\n", mqTrackMode)
	} else {
		fmt.Printf("  Enabled: false\n")
	}

	// Show PR shepherd config
	fmt.Println("\nPR Shepherd:")
	psEnabled := true
	if enabled, ok := configMap["ps_enabled"].(bool); ok {
		psEnabled = enabled
	}
	psTrackMode := "author"
	if trackMode, ok := configMap["ps_track_mode"].(string); ok {
		psTrackMode = trackMode
	}
	if psEnabled {
		fmt.Printf("  Enabled: true\n")
		fmt.Printf("  Track mode: %s\n", psTrackMode)
	} else {
		fmt.Printf("  Enabled: false\n")
	}

	// Show workspace stuck detection
	fmt.Println("\nWorkspace Stuck Detection:")
	wsdEnabled := false
	if enabled, ok := configMap["workspace_stuck_detection"].(bool); ok {
		wsdEnabled = enabled
	}
	if wsdEnabled {
		fmt.Printf("  Enabled: true\n")
	} else {
		fmt.Printf("  Enabled: false (default)\n")
	}

	// Show model
	if model, ok := configMap["model"].(string); ok && model != "" {
		fmt.Printf("\nDefault Model: %s\n", model)
	}

	// Show allowed worker models
	fmt.Println("\nAllowed Worker Models:")
	allowedModels, _ := configMap["allowed_worker_models"].([]interface{})
	if len(allowedModels) == 0 {
		fmt.Println("  (none — all eligible onboarded models are available)")
	} else {
		for _, m := range allowedModels {
			fmt.Printf("  - %s\n", m)
		}
	}

	fmt.Println("\nTo modify:")
	fmt.Printf("  oat config %s --mq-enabled=true|false\n", repoName)
	fmt.Printf("  oat config %s --mq-track=all|author|assigned\n", repoName)
	fmt.Printf("  oat config %s --ps-enabled=true|false\n", repoName)
	fmt.Printf("  oat config %s --ps-track=all|author|assigned\n", repoName)
	fmt.Printf("  oat config %s --workspace-stuck-detection=true|false\n", repoName)
	fmt.Printf("  oat config %s worker-models set|add|remove|list|clear\n", repoName)

	return nil
}

func (c *CLI) updateRepoConfig(repoName string, flags map[string]string) error {
	// Build update args
	updateArgs := map[string]interface{}{
		"name": repoName,
	}

	// Parse and validate flags
	if mqEnabled, ok := flags["mq-enabled"]; ok {
		switch mqEnabled {
		case "true":
			updateArgs["mq_enabled"] = true
		case "false":
			updateArgs["mq_enabled"] = false
		default:
			return fmt.Errorf("invalid --mq-enabled value: %s (must be 'true' or 'false')", mqEnabled)
		}
	}

	if mqTrack, ok := flags["mq-track"]; ok {
		switch mqTrack {
		case "all", "author", "assigned":
			updateArgs["mq_track_mode"] = mqTrack
		default:
			return fmt.Errorf("invalid --mq-track value: %s (must be 'all', 'author', or 'assigned')", mqTrack)
		}
	}

	// Parse workspace stuck detection flag
	if wsd, ok := flags["workspace-stuck-detection"]; ok {
		switch wsd {
		case "true":
			updateArgs["workspace_stuck_detection"] = true
		case "false":
			updateArgs["workspace_stuck_detection"] = false
		default:
			return fmt.Errorf("invalid --workspace-stuck-detection value: %s (must be 'true' or 'false')", wsd)
		}
	}

	// Parse PR shepherd flags
	if psEnabled, ok := flags["ps-enabled"]; ok {
		switch psEnabled {
		case "true":
			updateArgs["ps_enabled"] = true
		case "false":
			updateArgs["ps_enabled"] = false
		default:
			return fmt.Errorf("invalid --ps-enabled value: %s (must be 'true' or 'false')", psEnabled)
		}
	}

	if psTrack, ok := flags["ps-track"]; ok {
		switch psTrack {
		case "all", "author", "assigned":
			updateArgs["ps_track_mode"] = psTrack
		default:
			return fmt.Errorf("invalid --ps-track value: %s (must be 'all', 'author', or 'assigned')", psTrack)
		}
	}

	client := socket.NewClient(c.paths.DaemonSock)
	resp, err := client.Send(socket.Request{
		Command: "update_repo_config",
		Args:    updateArgs,
	})
	if err != nil {
		return fmt.Errorf("failed to update repo config: %w (is daemon running?)", err)
	}

	if !resp.Success {
		return fmt.Errorf("failed to update repo config: %s", resp.Error)
	}

	fmt.Printf("Configuration updated for repository: %s\n", repoName)
	return nil
}

func (c *CLI) configWorkerModels(repoName string, args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}

	action := args[0]
	switch action {
	case "set", "add", "remove":
		if len(args) < 2 {
			return fmt.Errorf("usage: oat config %s worker-models %s <model1,model2,...>", repoName, action)
		}
		value := strings.Join(args[1:], ",")

		client := socket.NewClient(c.paths.DaemonSock)
		resp, err := client.Send(socket.Request{
			Command: "update_repo_config",
			Args: map[string]interface{}{
				"name":                 repoName,
				"worker_models_action": action,
				"worker_models_value":  value,
			},
		})
		if err != nil {
			return fmt.Errorf("failed to update worker models: %w (is daemon running?)", err)
		}
		if !resp.Success {
			return fmt.Errorf("failed to update worker models: %s", resp.Error)
		}

		// Print warnings if any
		if respMap, ok := resp.Data.(map[string]interface{}); ok {
			if warnings, ok := respMap["warnings"].([]interface{}); ok {
				for _, w := range warnings {
					fmt.Printf("  Warning: %s\n", w)
				}
			}
		}

		fmt.Printf("Allowed worker models updated for repository: %s\n", repoName)
		return c.configWorkerModels(repoName, []string{"list"})

	case "list":
		client := socket.NewClient(c.paths.DaemonSock)
		resp, err := client.Send(socket.Request{
			Command: "get_repo_config",
			Args:    map[string]interface{}{"name": repoName},
		})
		if err != nil {
			return fmt.Errorf("failed to get repo config: %w (is daemon running?)", err)
		}
		if !resp.Success {
			return fmt.Errorf("failed to get repo config: %s", resp.Error)
		}

		configMap, ok := resp.Data.(map[string]interface{})
		if !ok {
			return fmt.Errorf("unexpected response format")
		}

		fmt.Printf("Allowed worker models for %s:\n", repoName)
		models, _ := configMap["allowed_worker_models"].([]interface{})
		if len(models) == 0 {
			fmt.Println("  (none — all eligible onboarded models are available)")
		} else {
			for _, m := range models {
				fmt.Printf("  - %s\n", m)
			}
		}
		return nil

	case "clear":
		client := socket.NewClient(c.paths.DaemonSock)
		resp, err := client.Send(socket.Request{
			Command: "update_repo_config",
			Args: map[string]interface{}{
				"name":                 repoName,
				"worker_models_action": "clear",
			},
		})
		if err != nil {
			return fmt.Errorf("failed to clear worker models: %w (is daemon running?)", err)
		}
		if !resp.Success {
			return fmt.Errorf("failed to clear worker models: %s", resp.Error)
		}
		fmt.Printf("Allowed worker models cleared for repository: %s (no restriction)\n", repoName)
		return nil

	default:
		return fmt.Errorf("unknown worker-models action %q (use: set, add, remove, list, clear)", action)
	}
}

func (c *CLI) createWorker(args []string) error {
	flags, posArgs := ParseFlags(args)

	// Get task description
	task := strings.Join(posArgs, " ")
	if task == "" {
		return errors.InvalidUsage("usage: oat worker create <task description>")
	}

	// Determine repository
	repoName, err := c.resolveRepo(flags)
	if err != nil {
		return errors.NotInRepo()
	}

	// Generate worker name (Docker-style)
	workerName := names.Generate()
	if name, ok := flags["name"]; ok {
		workerName = name
	}

	// Check for --push-to flag (for iterating on existing PRs)
	pushTo, hasPushTo := flags["push-to"]
	if hasPushTo {
		// --push-to requires --branch to specify the remote branch to start from
		if _, hasBranch := flags["branch"]; !hasBranch {
			return errors.InvalidUsage("--push-to requires --branch to specify the remote branch (e.g., --branch origin/work/jolly-hawk --push-to work/jolly-hawk)")
		}
	}

	// Optional --issue and --issue-url for issue-tied tasks
	// Parse --model flag (overrides repo default)
	model := flags["model"]

	// Parse --max-tokens flag (token budget, 0 = unlimited)
	var maxTokens int64
	if v, ok := flags["max-tokens"]; ok && v != "" {
		parsed, parseErr := strconv.ParseInt(v, 10, 64)
		if parseErr != nil || parsed < 0 {
			return errors.InvalidUsage("--max-tokens must be a positive integer")
		}
		maxTokens = parsed
	}

	var issueNumber, issueURL string
	if v, ok := flags["issue"]; ok && v != "" {
		issueNumber = strings.TrimSpace(v)
	}
	if v, ok := flags["issue-url"]; ok && v != "" {
		issueURL = strings.TrimSpace(v)
	}

	// Get repository path
	repoPath := c.paths.RepoDir(repoName)

	// Fetch latest from origin before creating worktree
	// This ensures workers start from the latest code, not stale local refs
	// Note: We use "git fetch origin main" (not "main:main") because the latter
	// fails when main is checked out in the bare repo with:
	// "fatal: refusing to fetch into branch 'refs/heads/main' checked out at ..."
	fmt.Println("Fetching latest from origin...")
	fetchCmd := exec.CommandContext(c.cmdCtx(), "git", "fetch", "origin")
	fetchCmd.Dir = repoPath
	if err := fetchCmd.Run(); err != nil {
		// Best effort - don't fail if offline or fetch fails
		fmt.Printf("Warning: failed to fetch from origin: %v (continuing with local refs)\n", err)
	}

	// Determine branch to start from
	// Prefer origin/main if it exists (updated by fetch), otherwise fall back to HEAD
	// This handles both normal repos and test repos without remotes
	startBranch := "HEAD"
	checkOriginCmd := exec.CommandContext(c.cmdCtx(), "git", "rev-parse", "--verify", "origin/main")
	checkOriginCmd.Dir = repoPath
	if err := checkOriginCmd.Run(); err == nil {
		startBranch = "origin/main"
	}
	if branch, ok := flags["branch"]; ok {
		startBranch = branch
		if hasPushTo {
			fmt.Printf("Creating worker '%s' in repo '%s' to iterate on branch '%s'\n", workerName, repoName, pushTo)
		} else {
			fmt.Printf("Creating worker '%s' in repo '%s' from branch '%s'\n", workerName, repoName, branch)
		}
	} else {
		fmt.Printf("Creating worker '%s' in repo '%s'\n", workerName, repoName)
	}
	fmt.Printf("Task: %s\n", task)

	// Create worktree
	wt := worktree.NewManagerWithContext(c.cmdCtx(), repoPath)
	wtPath := c.paths.AgentWorktree(repoName, workerName)

	var branchName string
	if hasPushTo {
		// When --push-to is specified, we're iterating on an existing PR branch
		// Create a worktree that checks out the remote branch into a local branch
		branchName = pushTo
		fmt.Printf("Creating worktree at: %s (checking out %s)\n", wtPath, startBranch)

		// Check if the local branch already exists
		branchExists, err := wt.BranchExists(branchName)
		if err != nil {
			return errors.WorktreeCreationFailed(err)
		}

		if branchExists {
			// Branch exists locally, check it out
			if err := wt.Create(wtPath, branchName); err != nil {
				return errors.WorktreeCreationFailed(err)
			}
		} else {
			// Branch doesn't exist, create it from the start point
			if err := wt.CreateNewBranch(wtPath, branchName, startBranch); err != nil {
				return errors.WorktreeCreationFailed(err)
			}
		}
	} else {
		// Normal case: pick unique name (with retry on collision) then create worktree (Bug 4 Option 2)
		const maxRetries = 20
		branchName = fmt.Sprintf("work/%s", workerName)
		for attempt := 0; attempt < maxRetries; attempt++ {
			exists, err := wt.BranchExists(branchName)
			if err != nil {
				return errors.WorktreeCreationFailed(err)
			}
			if !exists {
				checkRemoteCmd := exec.CommandContext(c.cmdCtx(), "git", "ls-remote", "--heads", "origin", branchName)
				checkRemoteCmd.Dir = repoPath
				output, _ := checkRemoteCmd.Output()
				if len(output) == 0 {
					break
				}
			}
			if _, ok := flags["name"]; ok {
				return errors.WorktreeCreationFailed(fmt.Errorf("branch %s already exists", branchName))
			}
			if attempt == maxRetries-1 {
				return fmt.Errorf("failed to generate unique worker name after %d attempts", maxRetries)
			}
			workerName = names.Generate()
			branchName = fmt.Sprintf("work/%s", workerName)
			wtPath = c.paths.AgentWorktree(repoName, workerName)
			time.Sleep(100 * time.Millisecond)
		}
		fmt.Printf("Creating worktree at: %s\n", wtPath)
		if err := wt.CreateNewBranch(wtPath, branchName, startBranch); err != nil {
			return errors.WorktreeCreationFailed(err)
		}
	}

	// Get repository info to determine session
	client := socket.NewClient(c.paths.DaemonSock)
	resp, err := client.Send(socket.Request{
		Command: "list_agents",
		Args: map[string]interface{}{
			"repo": repoName,
		},
	})
	if err != nil {
		return errors.DaemonCommunicationFailed("getting repo info", err)
	}
	if !resp.Success {
		return errors.Wrap(errors.CategoryRuntime, "failed to get repo info", fmt.Errorf("%s", resp.Error))
	}

	// Generate session ID for worker
	workerSessionID, err := agent_pkg.GenerateSessionID()
	if err != nil {
		return fmt.Errorf("failed to generate worker session ID: %w", err)
	}

	// Get fork config from daemon to include in worker prompt
	var forkConfig state.ForkConfig
	configResp, err := client.Send(socket.Request{
		Command: "get_repo_config",
		Args: map[string]interface{}{
			"name": repoName,
		},
	})
	if err == nil && configResp.Success {
		if configMap, ok := configResp.Data.(map[string]interface{}); ok {
			if isFork, ok := configMap["is_fork"].(bool); ok && isFork {
				forkConfig.IsFork = true
				forkConfig.UpstreamURL, _ = configMap["upstream_url"].(string)
				forkConfig.UpstreamOwner, _ = configMap["upstream_owner"].(string)
				forkConfig.UpstreamRepo, _ = configMap["upstream_repo"].(string)
			}
			// Fall back to repo-level model if no per-worker override
			if model == "" {
				if repoModel, ok := configMap["model"].(string); ok && repoModel != "" {
					model = repoModel
				}
			}
		}
	}

	// Write prompt file for worker (with push-to config, fork config, and optional issue if applicable)
	workerConfig := WorkerConfig{
		ForkConfig: forkConfig,
	}
	if hasPushTo {
		workerConfig.PushToBranch = pushTo
	}
	if issueNumber != "" {
		workerConfig.IssueNumber = issueNumber
		workerConfig.IssueURL = issueURL
	}
	workerPromptFile, err := c.writeWorkerPromptFile(repoPath, workerName, workerConfig)
	if err != nil {
		return fmt.Errorf("failed to write worker prompt: %w", err)
	}

	// Copy hooks configuration if it exists
	if err := hooks.CopyConfig(repoPath, wtPath); err != nil {
		fmt.Printf("Warning: failed to copy hooks config: %v\n", err)
	}

	// Start and register worker via daemon so one process owns lifecycle.
	// In test mode, keep legacy add_agent-only behavior to avoid launching real agents.
	if os.Getenv("OAT_TEST_MODE") != "1" {
		fmt.Println("Starting OAT agent in worker window...")
		startArgs := map[string]interface{}{
			"repo":          repoName,
			"agent":         workerName,
			"worktree_path": wtPath,
			"prompt_file":   workerPromptFile,
			"task":          task,
			"session_id":    workerSessionID,
		}
		if issueNumber != "" {
			startArgs["issue_number"] = issueNumber
			if issueURL != "" {
				startArgs["issue_url"] = issueURL
			}
		}
		if model != "" {
			startArgs["model"] = model
		}
		if maxTokens > 0 {
			startArgs["max_tokens_budget"] = maxTokens
		}

		resp, err = client.Send(socket.Request{
			Command: "start_worker",
			Args:    startArgs,
		})
		if err != nil {
			// Clean up worktree on failure (e.g. model routing rejection)
			_ = wt.Remove(wtPath, true)
			return fmt.Errorf("failed to start/register worker: %w", err)
		}
		if !resp.Success {
			_ = wt.Remove(wtPath, true)
			return fmt.Errorf("failed to start/register worker: %s", resp.Error)
		}
	} else {
		addAgentArgs := map[string]interface{}{
			"repo":          repoName,
			"agent":         workerName,
			"type":          "worker",
			"worktree_path": wtPath,
			"window_name":   workerName,
			"task":          task,
			"session_id":    workerSessionID,
			"pid":           0,
		}
		if issueNumber != "" {
			addAgentArgs["issue_number"] = issueNumber
			if issueURL != "" {
				addAgentArgs["issue_url"] = issueURL
			}
		}
		if model != "" {
			addAgentArgs["model"] = model
		}
		resp, err = client.Send(socket.Request{
			Command: "add_agent",
			Args:    addAgentArgs,
		})
		if err != nil {
			return fmt.Errorf("failed to register worker: %w", err)
		}
		if !resp.Success {
			return fmt.Errorf("failed to register worker: %s", resp.Error)
		}
	}

	fmt.Println()
	fmt.Println("✓ Worker created successfully!")
	fmt.Printf("  Name: %s\n", workerName)
	fmt.Printf("  Branch: %s\n", branchName)
	fmt.Printf("  Worktree: %s\n", wtPath)
	if model != "" {
		fmt.Printf("  Model: %s\n", model)
	}
	if hasPushTo {
		fmt.Printf("  Mode: Push to existing PR branch (%s)\n", pushTo)
	}
	fmt.Printf("\nMonitor agents: oat ui\n")
	fmt.Printf("Or tail this worker: oat attach %s\n", workerName)

	return nil
}

func (c *CLI) listWorkers(args []string) error {
	flags, _ := ParseFlags(args)

	// Determine repository
	repoName, err := c.resolveRepo(flags)
	if err != nil {
		return errors.NotInRepo()
	}

	resp, err := c.sendDaemonRequest("list_agents", map[string]interface{}{
		"repo": repoName,
		"rich": true,
	})
	if err != nil {
		return err
	}

	agents, ok := resp.Data.([]interface{})
	if !ok {
		return errors.New(errors.CategoryRuntime, "unexpected response format from daemon")
	}

	// Filter for workers and workspace
	workers := []map[string]interface{}{}
	var workspace map[string]interface{}
	for _, agent := range agents {
		if agentMap, ok := agent.(map[string]interface{}); ok {
			agentType, _ := agentMap["type"].(string)
			switch agentType {
			case "worker":
				workers = append(workers, agentMap)
			case "workspace":
				workspace = agentMap
			}
		}
	}

	// Show workspace first if it exists
	if workspace != nil {
		format.Header("Workspace in '%s':", repoName)
		status, _ := workspace["status"].(string)
		statusCell := formatAgentStatusCell(status)
		fmt.Printf("  workspace ")
		fmt.Print(statusCell.Text)
		fmt.Println()
		fmt.Println()
	}

	if len(workers) == 0 {
		fmt.Printf("No workers in repository '%s'\n", repoName)
		format.Dimmed("\nCreate a worker with: oat worker create <task>")
		return nil
	}

	format.Header("Workers in '%s' (%d):", repoName, len(workers))
	fmt.Println()

	table := format.NewColoredTable("NAME", "MODEL", "STATUS", "TOKENS", "BRANCH", "TASK")
	for _, worker := range workers {
		name, _ := worker["name"].(string)
		task, _ := worker["task"].(string)
		status, _ := worker["status"].(string)
		branch, _ := worker["branch"].(string)
		model, _ := worker["model"].(string)
		msgsTotal := 0
		if v, ok := worker["messages_total"].(float64); ok {
			msgsTotal = int(v)
		}
		msgsPending := 0
		if v, ok := worker["messages_pending"].(float64); ok {
			msgsPending = int(v)
		}
		_ = msgsTotal // used below for status display
		_ = msgsPending

		// Format token usage with budget
		var tokenStr string
		totalTokens := int64(0)
		if v, ok := worker["total_tokens"].(float64); ok {
			totalTokens = int64(v)
		}
		maxTokens := int64(0)
		if v, ok := worker["max_tokens"].(float64); ok {
			maxTokens = int64(v)
		}
		if totalTokens > 0 {
			tokenStr = formatTokenCountCLI(totalTokens)
			if maxTokens > 0 {
				pct := float64(totalTokens) / float64(maxTokens) * 100
				tokenStr = fmt.Sprintf("%s/%s", formatTokenCountCLI(totalTokens), formatTokenCountCLI(maxTokens))
				if pct >= 90 {
					tokenStr += " ⚠"
				}
			}
		} else if maxTokens > 0 {
			tokenStr = fmt.Sprintf("-/%s", formatTokenCountCLI(maxTokens))
		} else {
			tokenStr = "-"
		}

		// Format status with color
		statusCell := formatAgentStatusCell(status)

		// Format branch
		branchCell := format.ColorCell(branch, format.Cyan)
		if branch == "" {
			branchCell = format.ColorCell("-", format.Dim)
		}

		// Truncate task
		truncTask := format.Truncate(task, 40)

		modelCell := format.ColorCell(shortenModelID(model), format.Dim)

		table.AddRow(
			format.Cell(name),
			modelCell,
			statusCell,
			format.Cell(tokenStr),
			branchCell,
			format.Cell(truncTask),
		)
	}
	table.Print()

	return nil
}

// shortenModelID strips the provider prefix from a model ID for compact display.
func shortenModelID(model string) string {
	if model == "" {
		return "-"
	}
	if i := strings.Index(model, ":"); i >= 0 {
		return model[i+1:]
	}
	return model
}

// formatTokenCountCLI formats a token count for CLI table display.
func formatTokenCountCLI(count int64) string {
	switch {
	case count >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(count)/1_000_000)
	case count >= 1_000:
		return fmt.Sprintf("%.1fK", float64(count)/1_000)
	default:
		return fmt.Sprintf("%d", count)
	}
}

// listAgentDefinitions lists available agent definitions for a repository
func (c *CLI) listAgentDefinitions(args []string) error {
	flags, _ := ParseFlags(args)

	// Determine repository
	repoName, err := c.resolveRepo(flags)
	if err != nil {
		return errors.NotInRepo()
	}

	// Get paths to agent definition directories
	localAgentsDir := c.paths.RepoAgentsDir(repoName)
	repoPath := c.paths.RepoDir(repoName)

	// Read and merge agent definitions
	reader := agents.NewReader(localAgentsDir, repoPath)
	defs, err := reader.ReadAllDefinitions()
	if err != nil {
		return errors.Wrap(errors.CategoryRuntime, "failed to read agent definitions", err)
	}

	if len(defs) == 0 {
		fmt.Println("No agent definitions found.")
		fmt.Printf("\nAgent definitions are stored in:\n")
		fmt.Printf("  Local: %s\n", localAgentsDir)
		fmt.Printf("  Repo:  %s/.oat/agents/\n", repoPath)
		return nil
	}

	fmt.Printf("Agent definitions for %s:\n\n", repoName)

	// Create colored table
	table := format.NewColoredTable("Name", "Source", "Title", "Description")

	for _, def := range defs {
		source := string(def.Source)
		title := def.ParseTitle()
		desc := def.ParseDescription()

		// Truncate description if too long
		desc = format.Truncate(desc, 50)

		// Color the source based on type
		sourceCell := format.Cell(source)
		if def.Source == agents.SourceRepo {
			sourceCell = format.ColorCell(source, format.Green)
		}

		table.AddRow(
			format.Cell(def.Name),
			sourceCell,
			format.Cell(title),
			format.Cell(desc),
		)
	}

	table.Print()

	return nil
}

// spawnAgentFromFile spawns an agent using a prompt file and the daemon's spawn_agent handler.
// This is the CLI command that connects supervisor orchestration with daemon agent spawning.
func (c *CLI) spawnAgentFromFile(args []string) error {
	flags, _ := ParseFlags(args)

	// Get required parameters
	agentName, ok := flags["name"]
	if !ok || agentName == "" {
		return errors.InvalidUsage("--name is required")
	}

	agentClass, ok := flags["class"]
	if !ok || agentClass == "" {
		return errors.InvalidUsage("--class is required (persistent or ephemeral)")
	}
	if agentClass != "persistent" && agentClass != "ephemeral" {
		return errors.InvalidUsage("--class must be 'persistent' or 'ephemeral'")
	}

	promptFile, ok := flags["prompt-file"]
	if !ok || promptFile == "" {
		return errors.InvalidUsage("--prompt-file is required")
	}

	// Determine repository
	repoName, err := c.resolveRepo(flags)
	if err != nil {
		return errors.NotInRepo()
	}

	// Read prompt from file
	promptContent, err := os.ReadFile(promptFile)
	if err != nil {
		return errors.Wrap(errors.CategoryRuntime, "failed to read prompt file", err)
	}

	// Get optional task parameter
	task := flags["task"]

	// Send spawn_agent request to daemon
	client := socket.NewClient(c.paths.DaemonSock)
	reqArgs := map[string]interface{}{
		"repo":   repoName,
		"name":   agentName,
		"class":  agentClass,
		"prompt": string(promptContent),
	}
	if task != "" {
		reqArgs["task"] = task
	}

	resp, err := client.Send(socket.Request{
		Command: "spawn_agent",
		Args:    reqArgs,
	})
	if err != nil {
		return errors.DaemonCommunicationFailed("spawning agent", err)
	}
	if !resp.Success {
		return errors.Wrap(errors.CategoryRuntime, "failed to spawn agent", fmt.Errorf("%s", resp.Error))
	}

	fmt.Printf("Agent '%s' spawned successfully (class: %s)\n", agentName, agentClass)
	return nil
}

// resetAgentDefinitions deletes the local agent definitions and re-copies from templates.
func (c *CLI) resetAgentDefinitions(args []string) error {
	flags, _ := ParseFlags(args)

	// Determine repository
	repoName, err := c.resolveRepo(flags)
	if err != nil {
		return errors.NotInRepo()
	}

	// Get agents directory path
	agentsDir := c.paths.RepoAgentsDir(repoName)

	// Check if directory exists
	if _, err := os.Stat(agentsDir); os.IsNotExist(err) {
		fmt.Printf("No agent definitions found at %s\n", agentsDir)
		fmt.Println("Creating new definitions from templates...")
	} else {
		// Remove existing directory
		fmt.Printf("Removing existing agent definitions at %s...\n", agentsDir)
		if err := os.RemoveAll(agentsDir); err != nil {
			return errors.Wrap(errors.CategoryRuntime, "failed to remove agent definitions", err)
		}
	}

	// Copy templates
	if err := templates.CopyAgentTemplates(agentsDir); err != nil {
		return errors.Wrap(errors.CategoryRuntime, "failed to copy agent templates", err)
	}

	// List what was copied
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		return errors.Wrap(errors.CategoryRuntime, "failed to list agent definitions", err)
	}

	fmt.Printf("Reset complete. Agent definitions in %s:\n", agentsDir)
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".md" {
			fmt.Printf("  - %s\n", entry.Name())
		}
	}

	return nil
}

func (c *CLI) showHistory(args []string) error {
	flags, _ := ParseFlags(args)

	// Determine repository
	repoName, err := c.resolveRepo(flags)
	if err != nil {
		return errors.NotInRepo()
	}

	// Get limit from flags (default 10)
	limit := 10
	if n, ok := flags["n"]; ok {
		if v, err := strconv.Atoi(n); err == nil && v > 0 {
			limit = v
		}
	}

	// Get filter options
	statusFilter := flags["status"] // Filter by status (merged, open, closed, failed, no-pr)
	searchQuery := flags["search"]  // Search in task descriptions
	showFull := flags["full"] == "true"

	// Validate status filter if provided
	validStatuses := map[string]bool{
		"merged": true, "open": true, "closed": true, "failed": true, "no-pr": true,
	}
	if statusFilter != "" && !validStatuses[statusFilter] {
		return errors.InvalidUsage(fmt.Sprintf("invalid status filter: %s (valid values: merged, open, closed, failed, no-pr)", statusFilter))
	}

	// When filtering, fetch more history to ensure we get enough results
	fetchLimit := limit
	if statusFilter != "" || searchQuery != "" {
		fetchLimit = limit * 10 // Fetch more to allow for filtering
		if fetchLimit > 100 {
			fetchLimit = 100
		}
	}

	// Get task history from daemon
	client := socket.NewClient(c.paths.DaemonSock)
	resp, err := client.Send(socket.Request{
		Command: "task_history",
		Args: map[string]interface{}{
			"repo":  repoName,
			"limit": fetchLimit,
		},
	})
	if err != nil {
		return errors.DaemonCommunicationFailed("getting task history", err)
	}
	if !resp.Success {
		return errors.Wrap(errors.CategoryRuntime, "failed to get task history", fmt.Errorf("%s", resp.Error))
	}

	history, ok := resp.Data.([]interface{})
	if !ok || len(history) == 0 {
		fmt.Printf("No task history for repository '%s'\n", repoName)
		format.Dimmed("\nCreate workers with: oat worker create <task>")
		return nil
	}

	// Query GitHub for PR status for each task with a branch
	repoPath := c.paths.RepoDir(repoName)

	// Build filtered header
	headerParts := []string{fmt.Sprintf("Task History for '%s'", repoName)}
	if statusFilter != "" {
		headerParts = append(headerParts, fmt.Sprintf("status=%s", statusFilter))
	}
	if searchQuery != "" {
		headerParts = append(headerParts, fmt.Sprintf("search=%q", searchQuery))
	}
	format.Header("%s:", strings.Join(headerParts, ", "))
	fmt.Println()

	// First pass: collect entries with details to show after table
	type entryDetails struct {
		name          string
		summary       string
		failureReason string
	}
	var detailsToShow []entryDetails

	table := format.NewColoredTable("NAME", "STATUS", "MODEL", "PR", "COMPLETED", "TASK")
	displayedCount := 0
	for _, item := range history {
		// Stop once we've displayed enough
		if displayedCount >= limit {
			break
		}

		entry, ok := item.(map[string]interface{})
		if !ok {
			continue
		}

		name, _ := entry["name"].(string)
		task, _ := entry["task"].(string)
		branch, _ := entry["branch"].(string)
		prURL, _ := entry["pr_url"].(string)
		completedAt, _ := entry["completed_at"].(string)
		summary, _ := entry["summary"].(string)
		failureReason, _ := entry["failure_reason"].(string)
		storedStatus, _ := entry["status"].(string)
		model, _ := entry["model"].(string)

		// Try to get PR status from GitHub if we have a branch
		prStatus, prLink := c.getPRStatusForBranch(repoPath, branch, prURL)

		// Use stored status if it indicates failure
		if storedStatus == "failed" {
			prStatus = "failed"
		}

		// Apply status filter
		if statusFilter != "" {
			effectiveStatus := prStatus
			if effectiveStatus == "" {
				effectiveStatus = "no-pr"
			}
			if effectiveStatus != statusFilter {
				continue
			}
		}

		// Apply search filter (case-insensitive)
		if searchQuery != "" {
			lowerQuery := strings.ToLower(searchQuery)
			lowerTask := strings.ToLower(task)
			lowerName := strings.ToLower(name)
			if !strings.Contains(lowerTask, lowerQuery) && !strings.Contains(lowerName, lowerQuery) {
				continue
			}
		}

		displayedCount++

		// Collect entries with summary or failure for detailed display
		if summary != "" || failureReason != "" {
			detailsToShow = append(detailsToShow, entryDetails{
				name:          name,
				summary:       summary,
				failureReason: failureReason,
			})
		}

		// Format status with color
		var statusCell format.ColoredCell
		switch prStatus {
		case "merged":
			statusCell = format.ColorCell("merged", format.Green)
		case "open":
			statusCell = format.ColorCell("open", format.Yellow)
		case "closed":
			statusCell = format.ColorCell("closed", format.Red)
		case "failed":
			statusCell = format.ColorCell("failed", format.Red)
		default:
			statusCell = format.ColorCell("no-pr", format.Dim)
		}

		// Format PR link
		prCell := format.ColorCell("-", format.Dim)
		if prLink != "" {
			// Extract just the PR number for display
			prCell = format.ColorCell(prLink, format.Cyan)
		}

		// Format completed time
		completedCell := format.ColorCell("-", format.Dim)
		if completedAt != "" {
			if t, err := time.Parse(time.RFC3339, completedAt); err == nil {
				completedCell = format.Cell(format.TimeAgo(t))
			}
		}

		// Format task - show full or truncate
		displayTask := task
		if !showFull {
			displayTask = format.Truncate(task, 50)
		}

		modelCell := format.ColorCell(shortenModelID(model), format.Dim)

		table.AddRow(
			format.Cell(name),
			statusCell,
			modelCell,
			prCell,
			completedCell,
			format.Cell(displayTask),
		)
	}

	// Show message if no results after filtering
	if displayedCount == 0 {
		if statusFilter != "" || searchQuery != "" {
			fmt.Printf("No tasks match the filter criteria\n")
		}
		return nil
	}

	table.Print()

	// Print detailed summary/failure section if any entries have them
	if len(detailsToShow) > 0 {
		fmt.Println()
		format.Header("Details:")
		for _, d := range detailsToShow {
			format.Bold.Printf("\n%s:\n", d.name)
			if d.summary != "" {
				format.Dimmed("  Summary: %s", d.summary)
			}
			if d.failureReason != "" {
				format.Red.Printf("  Failure: %s\n", d.failureReason)
			}
		}
	}

	return nil
}

// getPRStatusForBranch queries GitHub for the PR status of a branch
func (c *CLI) getPRStatusForBranch(repoPath, branch, existingPRURL string) (status, prLink string) {
	// If we already have a PR URL, just return it formatted
	if existingPRURL != "" {
		// Extract PR number from URL for shorter display
		parts := strings.Split(existingPRURL, "/")
		if len(parts) > 0 {
			prNum := parts[len(parts)-1]
			return "unknown", "#" + prNum
		}
		return "unknown", existingPRURL
	}

	// If no branch, nothing to query
	if branch == "" {
		return "no-pr", ""
	}

	// Query GitHub for PR associated with this branch using gh CLI
	cmd := exec.CommandContext(c.cmdCtx(), "gh", "pr", "list", "--head", branch, "--state", "all", "--json", "number,state,url", "--limit", "1")
	cmd.Dir = repoPath
	output, err := cmd.Output()
	if err != nil {
		return "no-pr", ""
	}

	// Parse JSON output
	var prs []struct {
		Number int    `json:"number"`
		State  string `json:"state"`
		URL    string `json:"url"`
	}
	if err := json.Unmarshal(output, &prs); err != nil || len(prs) == 0 {
		return "no-pr", ""
	}

	pr := prs[0]
	prLink = fmt.Sprintf("#%d", pr.Number)

	switch strings.ToLower(pr.State) {
	case "merged":
		return "merged", prLink
	case "open":
		return "open", prLink
	case "closed":
		return "closed", prLink
	default:
		return "unknown", prLink
	}
}

func (c *CLI) removeWorker(args []string) error {
	flags, remainingArgs := ParseFlags(args)
	force := flags["force"] == "true"
	removeAll := flags["all"] == "true"

	// --all requires --force for safety
	if removeAll && !force {
		return fmt.Errorf("--all requires --force (removing all workers is destructive)\nUsage: oat worker rm --all --force [--repo <repo>]")
	}

	// Determine repository
	repoName, err := c.resolveRepo(flags)
	if err != nil {
		return errors.NotInRepo()
	}

	// Get worker info
	client := socket.NewClient(c.paths.DaemonSock)
	resp, err := client.Send(socket.Request{
		Command: "list_agents",
		Args: map[string]interface{}{
			"repo": repoName,
		},
	})
	if err != nil {
		return errors.DaemonCommunicationFailed("getting worker info", err)
	}
	if !resp.Success {
		return errors.Wrap(errors.CategoryRuntime, "failed to get worker info", fmt.Errorf("%s", resp.Error))
	}

	agents, _ := resp.Data.([]interface{})

	// Handle --all: remove every worker in the repo
	if removeAll {
		workers := agentsToSelectableItems(agents, []string{"worker"})
		if len(workers) == 0 {
			fmt.Printf("No workers found for repo '%s'\n", repoName)
			return nil
		}
		fmt.Printf("Removing all %d worker(s) from repo '%s'\n", len(workers), repoName)
		var lastErr error
		for _, w := range workers {
			if err := c.removeSingleWorker(client, repoName, w.Name, agents, true); err != nil {
				fmt.Printf("Error removing worker '%s': %v\n", w.Name, err)
				lastErr = err
			}
		}
		return lastErr
	}

	// Determine worker name - from args or interactive selection
	var workerName string
	if len(remainingArgs) > 0 {
		workerName = remainingArgs[0]
	} else {
		// Interactive selection
		items := agentsToSelectableItems(agents, []string{"worker"})
		if len(items) == 0 {
			return errors.NoWorkersFound(repoName)
		}
		selected, err := SelectFromList("Select worker to remove:", items)
		if err != nil {
			return err
		}
		if selected == "" {
			fmt.Println("Canceled")
			return nil
		}
		workerName = selected
	}

	return c.removeSingleWorker(client, repoName, workerName, agents, force)
}

func (c *CLI) removeSingleWorker(client *socket.Client, repoName, workerName string, agents []interface{}, force bool) error {
	fmt.Printf("Removing worker '%s' from repo '%s'\n", workerName, repoName)

	// Find worker
	var workerInfo map[string]interface{}
	for _, agent := range agents {
		if agentMap, ok := agent.(map[string]interface{}); ok {
			if name, _ := agentMap["name"].(string); name == workerName {
				workerInfo = agentMap
				break
			}
		}
	}

	if workerInfo == nil {
		return errors.AgentNotFound("worker", workerName, repoName)
	}

	// Get worktree path
	wtPath := workerInfo["worktree_path"].(string)

	// Check for uncommitted changes (skip prompts when --force)
	if !force {
		hasUncommitted, err := worktree.HasUncommittedChanges(c.cmdCtx(), wtPath)
		if err != nil {
			fmt.Printf("Warning: failed to check for uncommitted changes: %v\n", err)
		} else if hasUncommitted {
			fmt.Println("\nWarning: Worker has uncommitted changes!")
			fmt.Println("Files may be lost if you continue with cleanup.")
			fmt.Print("Continue with cleanup? [y/N]: ")

			var response string
			_, _ = fmt.Scanln(&response) // EOF/empty -> treated as "N" below
			if response != "y" && response != "Y" {
				fmt.Println("Cleanup canceled")
				return nil
			}
		}

		// Check for unpushed commits; user declined cleanup -> exit early but
		// not as an error.
		if err := c.checkUnpushedCommits(wtPath, "Worker", "cleanup"); err != nil {
			return nil //nolint:nilerr // user declined, not a CLI error
		}
	}

	fmt.Printf("Stopping agent: %s\n", workerName)

	// Remove worktree
	repoPath := c.paths.RepoDir(repoName)
	wt := worktree.NewManagerWithContext(c.cmdCtx(), repoPath)

	fmt.Printf("Removing worktree: %s\n", wtPath)
	if err := wt.Remove(wtPath, force); err != nil {
		fmt.Printf("Warning: failed to remove worktree: %v\n", err)
	}

	// Unregister from daemon
	resp, err := client.Send(socket.Request{
		Command: "remove_agent",
		Args: map[string]interface{}{
			"repo":  repoName,
			"agent": workerName,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to unregister worker: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("failed to unregister worker: %s", resp.Error)
	}

	fmt.Println("✓ Worker removed successfully")
	return nil
}

func (c *CLI) resetWorkerNudge(args []string) error {
	flags, remainingArgs := ParseFlags(args)

	repoName, err := c.resolveRepo(flags)
	if err != nil {
		return errors.NotInRepo()
	}

	if len(remainingArgs) == 0 {
		return fmt.Errorf("worker name is required\nUsage: oat worker reset-nudge <worker-name>")
	}
	workerName := remainingArgs[0]

	client := socket.NewClient(c.paths.DaemonSock)
	resp, err := client.Send(socket.Request{
		Command: "reset_nudge",
		Args: map[string]interface{}{
			"repo":  repoName,
			"agent": workerName,
		},
	})
	if err != nil {
		return errors.DaemonCommunicationFailed("resetting nudge count", err)
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Error)
	}

	fmt.Printf("✓ Nudge count reset for worker '%s'\n", workerName)
	return nil
}

// verifyWorker performs comprehensive verification of worker implementation quality
func (c *CLI) verifyWorker(args []string) error {
	// Only swallow panics in production — let them surface during testing
	if os.Getenv("OAT_TEST_MODE") != "1" {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("⚠️  Verification system temporarily unavailable: %v\n", r)
				fmt.Println("✅ Proceeding with normal workflow")
			}
		}()
	}

	flags, _ := ParseFlags(args)
	autoFix := flags["fix"] == "true"
	skipTests := flags["skip-tests"] == "true"
	verbose := flags["verbose"] == "true"
	jsonOutput := flags["json"] == "true"

	// Infer current context
	repoName, err := c.inferRepoFromCwd()
	if err != nil {
		return fmt.Errorf("could not determine repository context: %w", err)
	}

	agentName := c.inferAgentNameFromCwd()
	if agentName == "" {
		return fmt.Errorf("could not determine agent context - run this command from within a worker worktree")
	}

	// Block self-verification while a verification agent is still reviewing (< 5 min).
	if st, stErr := c.loadState(); stErr == nil {
		if agent, found := st.GetAgent(repoName, agentName); found {
			if agent.VerificationStatus == "pending" {
				verifierAlive := false
				if agent.VerificationAgent != "" {
					if verifier, vFound := st.GetAgent(repoName, agent.VerificationAgent); vFound {
						verifierAlive = true
						elapsed := time.Since(verifier.CreatedAt)
						if elapsed < 5*time.Minute {
							fmt.Printf("Verification agent '%s' is still reviewing (started %s ago).\n",
								agent.VerificationAgent, elapsed.Round(time.Second))
							fmt.Println("Going dormant -- the daemon will wake you with the verdict.")
							waitClient := socket.NewClient(c.paths.DaemonSock)
							waitResp, waitErr := waitClient.Send(socket.Request{
								Command: "agent_waiting",
								Args: map[string]interface{}{
									"repo":  repoName,
									"agent": agentName,
								},
							})
							if waitErr == nil && waitResp.Success {
								fmt.Println("STOP. Do not run any commands until the daemon delivers the [APPROVED] or [REJECTED] message.")
							}
							return fmt.Errorf("verification agent still in progress -- went dormant to wait for verdict")
						}
						fmt.Printf("Verification agent has been pending for %s (> 5 min) — proceeding with self-verify as fallback.\n",
							elapsed.Round(time.Second))
					}
				}
				if !verifierAlive {
					fmt.Println("Verification agent no longer running — proceeding with self-verify as fallback.")
				}
			}
		}
	}

	if !jsonOutput {
		fmt.Printf("🔍 Running verification for worker '%s' in repo '%s'\n", agentName, repoName)
		if autoFix {
			fmt.Println("🔧 Auto-fix mode enabled")
		}
	}

	// Run comprehensive verification with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	result, err := c.runComprehensiveVerification(ctx, repoName, agentName, autoFix, skipTests, jsonOutput)
	if err != nil {
		if jsonOutput {
			fmt.Printf(`{"error": %q, "overall_passed": false}`, err.Error())
		} else {
			fmt.Printf("⚠️  Verification error: %v\n", err)
			fmt.Println("✅ Proceeding with caution - consider manual review")
		}
		return nil
	}

	// Present results (JSON or human-readable)
	if jsonOutput {
		return c.presentVerificationResultsJSON(result)
	}
	if err := c.presentVerificationResults(result, verbose); err != nil {
		return err
	}

	// Return non-zero exit code when verification fails so agents can detect it programmatically
	if !result.OverallPassed {
		return fmt.Errorf("verification failed: score %.0f/100 (threshold 70)", result.OverallScore)
	}
	return nil
}

// requestVerification spawns an ephemeral verification agent for the current worker.
func (c *CLI) requestVerification(args []string) error {
	repoName, workerName, err := c.inferAgentContext()
	if err != nil {
		return fmt.Errorf("failed to determine agent context (run from within a worker worktree): %w", err)
	}

	// Auto-commit and push if the worktree is dirty
	statusCmd := exec.CommandContext(c.cmdCtx(), "git", "status", "--porcelain")
	if statusOut, err := statusCmd.Output(); err == nil && len(statusOut) > 0 {
		fmt.Println("Uncommitted changes detected — auto-committing before verification...")
		fmt.Printf("  Dirty files:\n%s\n", strings.TrimSpace(string(statusOut)))

		addCmd := exec.CommandContext(c.cmdCtx(), "git", "add", "-A")
		if addOut, err := addCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("auto-commit failed (git add): %w\n%s", err, string(addOut))
		}

		suspectPatterns := []string{".env", ".pem", ".key", ".secret", "credentials", ".p12", ".pfx", ".jks"}
		var skippedFiles []string
		cachedCmd := exec.CommandContext(c.cmdCtx(), "git", "diff", "--cached", "--name-only")
		if cachedOut, err := cachedCmd.Output(); err == nil {
			for _, file := range strings.Split(strings.TrimSpace(string(cachedOut)), "\n") {
				if file == "" {
					continue
				}
				base := strings.ToLower(filepath.Base(file))
				for _, pattern := range suspectPatterns {
					if strings.Contains(base, pattern) {
						// Best-effort unstage; failure leaves the suspect file staged
						// and the skip logged below.
						_ = exec.CommandContext(c.cmdCtx(), "git", "reset", "HEAD", "--", file).Run()
						skippedFiles = append(skippedFiles, file)
						break
					}
				}
			}
		}

		remainingCmd := exec.CommandContext(c.cmdCtx(), "git", "diff", "--cached", "--quiet")
		if remainingCmd.Run() == nil {
			fmt.Println("  No files to commit after filtering suspect files.")
			if len(skippedFiles) > 0 {
				fmt.Printf("  %d file(s) were not auto-committed due to potential secrets:\n", len(skippedFiles))
				for _, f := range skippedFiles {
					fmt.Printf("    - %s\n", f)
				}
				fmt.Println("  If they don't contain secrets, run: git add <file> && git commit && git push")
				fmt.Println("  If they do contain secrets, add them to .gitignore.")
			}
		} else {
			commitCmd := exec.CommandContext(c.cmdCtx(), "git", "commit", "-m", "pre-review commit")
			if commitOut, err := commitCmd.CombinedOutput(); err != nil {
				return fmt.Errorf("auto-commit failed (git commit): %w\n%s", err, string(commitOut))
			}
			pushCmd := exec.CommandContext(c.cmdCtx(), "git", "push", "-u", "origin", "HEAD")
			if pushOut, err := pushCmd.CombinedOutput(); err != nil {
				return fmt.Errorf("auto-commit failed (git push): %w\n%s", err, string(pushOut))
			}
			fmt.Println("  Auto-committed and pushed successfully.")
			if len(skippedFiles) > 0 {
				fmt.Printf("  Note: %d file(s) were NOT auto-committed due to potential secrets:\n", len(skippedFiles))
				for _, f := range skippedFiles {
					fmt.Printf("    - %s\n", f)
				}
				fmt.Println("  If they don't contain secrets, run: git add <file> && git commit && git push")
				fmt.Println("  If they do contain secrets, add them to .gitignore.")
			}
		}
	}

	// Handle committed-but-not-pushed
	unpushedCmd := exec.CommandContext(c.cmdCtx(), "git", "log", "--oneline", "@{u}..", "--")
	if unpushedOut, err := unpushedCmd.Output(); err == nil && len(strings.TrimSpace(string(unpushedOut))) > 0 {
		fmt.Println("Unpushed commits detected — pushing...")
		pushCmd := exec.CommandContext(c.cmdCtx(), "git", "push", "-u", "origin", "HEAD")
		if pushOut, err := pushCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("failed to push unpushed commits: %w\n%s", err, string(pushOut))
		}
		fmt.Println("  Pushed successfully.")
	}

	// Get current commit SHA (fresh, from worktree)
	shaCmd := exec.CommandContext(c.cmdCtx(), "git", "rev-parse", "HEAD")
	shaOut, err := shaCmd.Output()
	if err != nil {
		return fmt.Errorf("failed to get current commit SHA: %w", err)
	}
	commitSHA := strings.TrimSpace(string(shaOut))

	// Get branch name
	branchCmd := exec.CommandContext(c.cmdCtx(), "git", "rev-parse", "--abbrev-ref", "HEAD")
	branchOut, err := branchCmd.Output()
	if err != nil {
		return fmt.Errorf("failed to get branch name: %w", err)
	}
	workerBranch := strings.TrimSpace(string(branchOut))

	verifierName := fmt.Sprintf("verify-%s", workerName)

	// Snap the base branch SHA from the worker's worktree (fresher than daemon clone)
	var baseSHA string
	if baseRef := c.getBaseBranchRef(); baseRef != "" {
		baseCmd := exec.CommandContext(c.cmdCtx(), "git", "rev-parse", baseRef)
		if baseOut, err := baseCmd.Output(); err == nil {
			baseSHA = strings.TrimSpace(string(baseOut))
		}
	}

	fmt.Printf("Requesting verification for worker '%s' (SHA: %s)\n", workerName, commitSHA[:min(len(commitSHA), 8)])

	// Set pending state atomically via daemon
	client := socket.NewClient(c.paths.DaemonSock)
	startVerArgs := map[string]interface{}{
		"repo":          repoName,
		"worker":        workerName,
		"verifier_name": verifierName,
		"commit_sha":    commitSHA,
	}
	if baseSHA != "" {
		startVerArgs["base_sha"] = baseSHA
	}
	resp, err := client.Send(socket.Request{
		Command: "start_verification",
		Args:    startVerArgs,
	})
	if err != nil {
		return fmt.Errorf("failed to contact daemon: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("failed to set verification state: %s", resp.Error)
	}

	// Get worker task from state for prompt injection
	workerTask := ""
	if st, stErr := c.loadState(); stErr == nil {
		if agent, found := st.GetAgent(repoName, workerName); found {
			workerTask = agent.Task
		}
	}

	// Create worktree for verification agent (detached at worker's commit)
	wt := worktree.NewManager(c.paths.RepoDir(repoName))
	wtPath := c.paths.AgentWorktree(repoName, verifierName)
	verifyBranch := fmt.Sprintf("verify/%s", verifierName)

	// Auto-repair stale verification worktree from a previous run
	if _, statErr := os.Stat(wtPath); statErr == nil {
		fmt.Printf("Cleaning up stale verification worktree: %s\n", wtPath)
		_ = wt.Remove(wtPath, true)
		fmt.Println("Cleaned up stale verification worktree")
	}
	_ = wt.DeleteBranch(verifyBranch)

	fmt.Printf("Creating verification worktree at: %s\n", wtPath)
	if err := wt.CreateNewBranch(wtPath, verifyBranch, commitSHA); err != nil {
		return fmt.Errorf("failed to create worktree: %w", err)
	}

	// Gather project context for prompt injection. Pass the workerName so
	// gatherVerificationContext can look up the worker's pinned BaseSHA
	// (set by the daemon during `start_verification` above) and diff
	// against it instead of live origin/main.
	verifyCtx := c.gatherVerificationContext(repoName, workerName)

	// Write prompt file with template variable substitution
	promptFile, err := c.writeVerificationPromptFile(c.paths.RepoDir(repoName), verifierName, workerName, workerBranch, workerTask, commitSHA, verifyCtx)
	if err != nil {
		return fmt.Errorf("failed to write verification prompt: %w", err)
	}

	// Copy hooks config
	if err := hooks.CopyConfig(c.paths.RepoDir(repoName), wtPath); err != nil {
		fmt.Printf("Warning: failed to copy hooks config: %v\n", err)
	}

	// Start verification agent via daemon backend (not CLI backend) so the
	// daemon owns the PTY lifecycle and the agent survives CLI exit.
	initialMessage := fmt.Sprintf("Verify worker %s: review the diff on branch %s, run tests, and deliver your verdict.", workerName, workerBranch)
	resp, err = client.Send(socket.Request{
		Command: "start_verification_agent",
		Args: map[string]interface{}{
			"repo":            repoName,
			"agent":           verifierName,
			"worktree_path":   wtPath,
			"prompt_file":     promptFile,
			"task":            fmt.Sprintf("Verify worker %s commit %s", workerName, commitSHA[:min(len(commitSHA), 8)]),
			"initial_message": initialMessage,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to start verification agent: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("failed to start verification agent: %s", resp.Error)
	}

	fmt.Println()
	fmt.Println("Verification agent spawned successfully!")
	fmt.Printf("  Name: %s\n", verifierName)
	fmt.Printf("  Reviewing: %s (SHA: %s)\n", workerBranch, commitSHA[:min(len(commitSHA), 8)])
	fmt.Println()
	fmt.Println("Going dormant -- the daemon will wake you with the verdict.")
	fmt.Println("Do NOT poll, sleep, or run any other commands.")

	waitClient := socket.NewClient(c.paths.DaemonSock)
	waitResp, waitErr := waitClient.Send(socket.Request{
		Command: "agent_waiting",
		Args: map[string]interface{}{
			"repo":  repoName,
			"agent": workerName,
		},
	})
	if waitErr == nil && waitResp.Success {
		if data, ok := waitResp.Data.(map[string]any); ok {
			if status, _ := data["status"].(string); status == "dormant_verification" {
				fmt.Println("\n══════════════════════════════════════════════════════════════")
				fmt.Println("DORMANT: Waiting for verification verdict.")
				fmt.Println("STOP. Do NOT generate any text, run any commands, or take")
				fmt.Println("any action until you see a new USER message from the daemon.")
				fmt.Println("Any \"[daemon] Status check\" messages below are STALE -- they")
				fmt.Println("were queued BEFORE you went dormant. Ignore them completely.")
				fmt.Println("Do NOT respond to them. Do NOT poll. Do NOT check status.")
				fmt.Println("STOP HERE. WAIT FOR THE DAEMON.")
				fmt.Println("══════════════════════════════════════════════════════════════")
			}
		}
	} else {
		fmt.Println("WARNING: Could not auto-enter dormancy. Run `oat agent waiting` manually, then STOP.")
	}

	return nil
}

// verificationContext holds project-specific context injected into the verification prompt.
type verificationContext struct {
	ChangedFiles   string
	DiffStat       string
	ProjectType    string
	TestCommand    string
	BaseRef        string // diff base used for ChangedFiles/DiffStat (BaseSHA when pinned, "origin/main" fallback)
	FileCount      int
	TimeoutMinutes int
}

// gatherVerificationContext collects project info for the verification
// agent prompt. The diff base is the worker's pinned BaseSHA when set
// (snapshotted by the daemon at request-review time), falling back to
// origin/main when empty (back-compat for in-flight verifications during
// upgrade).
func (c *CLI) gatherVerificationContext(repoName, workerName string) verificationContext {
	ctx := verificationContext{TimeoutMinutes: 15}

	if envTimeout := os.Getenv("OAT_VERIFICATION_TIMEOUT_MINUTES"); envTimeout != "" {
		if v, err := strconv.Atoi(envTimeout); err == nil && v > 0 {
			ctx.TimeoutMinutes = v
		}
	}

	// Resolve diff base: prefer worker's pinned BaseSHA, fall back to
	// live origin/main. Empty BaseSHA means either an upgrade with
	// in-flight verification or a daemon-side snapshot failure.
	baseRef := "origin/main"
	if repoName != "" && workerName != "" {
		if st, err := c.loadState(); err == nil {
			if w, ok := st.GetAgent(repoName, workerName); ok && w.BaseSHA != "" {
				baseRef = w.BaseSHA
			}
		}
	}
	ctx.BaseRef = baseRef
	diffRange := fmt.Sprintf("%s..HEAD", baseRef)

	if out, err := exec.CommandContext(c.cmdCtx(), "git", "diff", "--name-only", diffRange).Output(); err == nil {
		ctx.ChangedFiles = strings.TrimSpace(string(out))
		ctx.FileCount = len(strings.Split(ctx.ChangedFiles, "\n"))
		if ctx.ChangedFiles == "" {
			ctx.FileCount = 0
		}
	}

	if out, err := exec.CommandContext(c.cmdCtx(), "git", "diff", "--stat", diffRange).Output(); err == nil {
		ctx.DiffStat = strings.TrimSpace(string(out))
	}

	ctx.ProjectType, ctx.TestCommand = detectProjectTypeAndTestCmd()

	return ctx
}

// detectProjectTypeAndTestCmd detects the project language and test command from cwd.
func detectProjectTypeAndTestCmd() (projectType, testCmd string) {
	if _, err := os.Stat("go.mod"); err == nil {
		return "Go", "go test ./..."
	}
	if _, err := os.Stat("package.json"); err == nil {
		return "JavaScript/TypeScript", "npm test"
	}
	if _, err := os.Stat("pyproject.toml"); err == nil {
		return "Python", "pytest"
	}
	if _, err := os.Stat("requirements.txt"); err == nil {
		return "Python", "pytest"
	}
	if _, err := os.Stat("Makefile"); err == nil {
		return "Unknown (has Makefile)", "make test"
	}
	return "Unknown", ""
}

// writeVerificationPromptFile loads the static verification template and prepends
// a structured context block with dynamic info (matching the merge-queue pattern).
func (c *CLI) writeVerificationPromptFile(repoPath, verifierName, workerName, workerBranch, workerTask, commitSHA string, vCtx verificationContext) (string, error) {
	repoName := filepath.Base(repoPath)

	// Load static template from agent definitions (same path as merge-queue, worker, etc.)
	promptText, err := c.getAgentDefinition(repoName, repoPath, "verification")
	if err != nil {
		return "", err
	}

	// Append CLI docs and slash commands
	promptText = c.appendDocsAndSlashCommands(promptText, string(state.AgentTypeVerification))

	// Substitute the ${BASE_SHA} template variable with the resolved
	// diff base. When the daemon successfully snapshotted origin/main at
	// request-review time, BaseRef is that pinned SHA; otherwise it falls
	// back to the literal "origin/main" so the diff command still works.
	baseRef := vCtx.BaseRef
	if baseRef == "" {
		baseRef = "origin/main"
	}
	promptText = strings.ReplaceAll(promptText, "${BASE_SHA}", baseRef)

	// Build structured context block and prepend it
	var ctx strings.Builder
	ctx.WriteString("## Verification Context\n\n")
	fmt.Fprintf(&ctx, "- **Worker:** `%s`\n", workerName)
	fmt.Fprintf(&ctx, "- **Branch:** `%s`\n", workerBranch)
	fmt.Fprintf(&ctx, "- **Commit under review:** `%s`\n", commitSHA)
	fmt.Fprintf(&ctx, "- **Diff base (pinned):** `%s` -- snapshotted at request-review time, not live origin/main\n", baseRef)
	fmt.Fprintf(&ctx, "- **Time budget:** %d minutes\n", vCtx.TimeoutMinutes)
	fmt.Fprintf(&ctx, "- **Project type:** %s\n", vCtx.ProjectType)
	if vCtx.TestCommand != "" {
		fmt.Fprintf(&ctx, "- **Test command:** `%s`\n", vCtx.TestCommand)
	}
	fmt.Fprintf(&ctx, "\n**Original task:**\n%s\n", workerTask)
	fmt.Fprintf(&ctx, "\n**Files changed (%d):**\n```\n%s\n```\n", vCtx.FileCount, vCtx.ChangedFiles)
	fmt.Fprintf(&ctx, "\n**Diff summary:**\n```\n%s\n```\n", vCtx.DiffStat)

	// Verdict commands (so the agent doesn't have to figure out the exact syntax)
	ctx.WriteString("\n**To approve:**\n```bash\n")
	fmt.Fprintf(&ctx, "oat worker set-verdict %s approved --sha %s --reason \"All checks passed: [summary]\"\n", workerName, commitSHA)
	ctx.WriteString("```\n")
	ctx.WriteString("\n**To reject:**\n```bash\n")
	fmt.Fprintf(&ctx, "oat worker set-verdict %s rejected --sha %s --reason \"FAILED: [details]\"\n", workerName, commitSHA)
	ctx.WriteString("```\n")

	promptText = ctx.String() + "\n---\n\n" + promptText

	return c.savePromptToFile(verifierName, promptText)
}

// setVerificationVerdict sends a verdict to the daemon for a worker.
// Used by verification agents to approve or reject worker output.
func (c *CLI) setVerificationVerdict(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: oat worker set-verdict <worker-name> approved|rejected --sha <sha> --reason \"...\"")
	}

	workerName := args[0]
	verdict := args[1]

	if verdict != "approved" && verdict != "rejected" {
		return fmt.Errorf("verdict must be 'approved' or 'rejected', got %q", verdict)
	}

	flags, _ := ParseFlags(args[2:])
	sha := flags["sha"]
	if sha == "" {
		return fmt.Errorf("--sha is required")
	}
	reason := flags["reason"]

	// Infer caller (verifier) context from cwd
	repoName, verifierName, err := c.inferAgentContext()
	if err != nil {
		return fmt.Errorf("failed to determine agent context: %w", err)
	}

	client := socket.NewClient(c.paths.DaemonSock)
	resp, err := client.Send(socket.Request{
		Command: "verification_verdict",
		Args: map[string]interface{}{
			"repo":     repoName,
			"worker":   workerName,
			"verifier": verifierName,
			"verdict":  verdict,
			"sha":      sha,
			"reason":   reason,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to contact daemon: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("verdict rejected: %s", resp.Error)
	}

	fmt.Printf("Verdict '%s' recorded for worker '%s'\n", verdict, workerName)
	return nil
}

// hibernateRepo stops all work in a repository and archives uncommitted changes
func (c *CLI) hibernateRepo(args []string) error {
	flags, _ := ParseFlags(args)
	skipConfirm := flags["yes"] == "true"
	hibernateAll := flags["all"] == "true" // Also hibernate persistent agents (supervisor, workspace)

	// Determine repository
	repoName, err := c.resolveRepo(flags)
	if err != nil {
		return errors.NotInRepo()
	}

	// Get agent list from daemon
	client := socket.NewClient(c.paths.DaemonSock)
	resp, err := client.Send(socket.Request{
		Command: "list_agents",
		Args: map[string]interface{}{
			"repo": repoName,
		},
	})
	if err != nil {
		return errors.DaemonCommunicationFailed("getting agent info", err)
	}
	if !resp.Success {
		return errors.Wrap(errors.CategoryRuntime, "failed to get agent info", fmt.Errorf("%s", resp.Error))
	}

	agents, _ := resp.Data.([]interface{})
	if len(agents) == 0 {
		fmt.Printf("No agents running in repository '%s'\n", repoName)
		return nil
	}

	// Filter agents to hibernate (workers, review agents; optionally all)
	var agentsToHibernate []map[string]interface{}
	var agentsWithChanges []map[string]interface{}

	for _, agent := range agents {
		agentMap, ok := agent.(map[string]interface{})
		if !ok {
			continue
		}

		agentType, _ := agentMap["type"].(string)
		wtPath, _ := agentMap["worktree_path"].(string)

		// Determine if this agent should be hibernated
		shouldHibernate := false
		switch agentType {
		case "worker", "review", "verification":
			shouldHibernate = true
		case "supervisor", "merge-queue", "pr-shepherd", "workspace", "generic-persistent":
			shouldHibernate = hibernateAll
		}

		if !shouldHibernate {
			continue
		}

		agentsToHibernate = append(agentsToHibernate, agentMap)

		// Check for uncommitted changes
		if wtPath != "" {
			hasUncommitted, err := worktree.HasUncommittedChanges(c.cmdCtx(), wtPath)
			if err == nil && hasUncommitted {
				agentsWithChanges = append(agentsWithChanges, agentMap)
			}
		}
	}

	if len(agentsToHibernate) == 0 {
		fmt.Printf("No agents to hibernate in repository '%s'\n", repoName)
		if !hibernateAll {
			fmt.Println("Use --all to also hibernate persistent agents (supervisor, workspace, etc.)")
		}
		return nil
	}

	// Show summary and confirm
	fmt.Printf("Hibernating %d agent(s) in repository '%s':\n", len(agentsToHibernate), repoName)
	for _, agent := range agentsToHibernate {
		name, _ := agent["name"].(string)
		agentType, _ := agent["type"].(string)
		hasChanges := false
		for _, changed := range agentsWithChanges {
			if changed["name"] == name {
				hasChanges = true
				break
			}
		}
		changeMarker := ""
		if hasChanges {
			changeMarker = " [has uncommitted changes]"
		}
		fmt.Printf("  - %s (%s)%s\n", name, agentType, changeMarker)
	}

	if len(agentsWithChanges) > 0 {
		fmt.Printf("\n%d agent(s) have uncommitted changes that will be archived.\n", len(agentsWithChanges))
	}

	if !skipConfirm {
		fmt.Print("\nContinue? [y/N]: ")
		var response string
		_, _ = fmt.Scanln(&response) // EOF/empty -> treated as "N" below
		if response != "y" && response != "Y" {
			fmt.Println("Canceled")
			return nil
		}
	}

	// Create archive directory with timestamp
	timestamp := time.Now().Format("2006-01-02_15-04-05")
	archiveDir := filepath.Join(c.paths.RepoArchiveDir(repoName), timestamp)
	if len(agentsWithChanges) > 0 {
		if err := os.MkdirAll(archiveDir, 0755); err != nil {
			return fmt.Errorf("failed to create archive directory: %w", err)
		}
		fmt.Printf("\nArchiving to: %s\n", archiveDir)
	}

	// Archive uncommitted changes
	var archivedAgents []string
	for _, agent := range agentsWithChanges {
		name, _ := agent["name"].(string)
		wtPath, _ := agent["worktree_path"].(string)
		branch, _ := agent["branch"].(string)
		task, _ := agent["task"].(string)

		fmt.Printf("Archiving changes from %s...\n", name)

		// Create patch file with git diff
		patchPath := filepath.Join(archiveDir, name+".patch")
		cmd := exec.CommandContext(c.cmdCtx(), "git", "diff", "HEAD", "--", ".", ":!.oat")
		cmd.Dir = wtPath
		output, err := cmd.Output()
		if err != nil {
			fmt.Printf("Warning: failed to create patch for %s: %v\n", name, err)
			continue
		}

		// Include untracked files in the patch (excluding OAT runtime dir)
		untrackedCmd := exec.CommandContext(c.cmdCtx(), "git", "ls-files", "--others", "--exclude-standard", "--", ".", ":!.oat")
		untrackedCmd.Dir = wtPath
		untrackedOutput, _ := untrackedCmd.Output()

		// Write patch file
		if err := os.WriteFile(patchPath, output, 0644); err != nil {
			fmt.Printf("Warning: failed to write patch for %s: %v\n", name, err)
			continue
		}

		// Write untracked files list if any
		if len(untrackedOutput) > 0 {
			untrackedPath := filepath.Join(archiveDir, name+".untracked")
			if err := os.WriteFile(untrackedPath, untrackedOutput, 0644); err != nil {
				fmt.Printf("Warning: failed to write untracked list for %s: %v\n", name, err)
			}
		}

		// Write metadata for this agent
		metaPath := filepath.Join(archiveDir, name+".json")
		meta := map[string]interface{}{
			"name":          name,
			"type":          agent["type"],
			"branch":        branch,
			"task":          task,
			"worktree_path": wtPath,
			"archived_at":   time.Now().Format(time.RFC3339),
		}
		metaData, _ := json.MarshalIndent(meta, "", "  ")
		if err := os.WriteFile(metaPath, metaData, 0644); err != nil {
			fmt.Printf("Warning: failed to write metadata for %s: %v\n", name, err)
		}

		archivedAgents = append(archivedAgents, name)
	}

	// Write summary metadata
	if len(agentsWithChanges) > 0 {
		summaryPath := filepath.Join(archiveDir, "hibernate-summary.json")
		summary := map[string]interface{}{
			"repo":              repoName,
			"hibernated_at":     time.Now().Format(time.RFC3339),
			"agents_hibernated": len(agentsToHibernate),
			"agents_archived":   archivedAgents,
		}
		summaryData, _ := json.MarshalIndent(summary, "", "  ")
		if err := os.WriteFile(summaryPath, summaryData, 0644); err != nil {
			fmt.Printf("Warning: failed to write hibernate summary: %v\n", err)
		}
	}

	// Stop agents
	sessionName := sanitizeSessionName(repoName)
	repoPath := c.paths.RepoDir(repoName)
	wt := worktree.NewManagerWithContext(c.cmdCtx(), repoPath)

	fmt.Println()
	for _, agent := range agentsToHibernate {
		name, _ := agent["name"].(string)
		wtPath, _ := agent["worktree_path"].(string)
		windowName, _ := agent["window_name"].(string)

		fmt.Printf("Stopping %s...\n", name)

		// Stop agent via backend
		if windowName != "" {
			_ = c.backend.StopAgent(context.Background(), sessionName, windowName)
		}

		// Remove worktree (force since we archived changes)
		if wtPath != "" {
			if err := wt.Remove(wtPath, true); err != nil {
				// Try harder with force; last-resort cleanup so errors are non-fatal.
				cmd := exec.CommandContext(c.cmdCtx(), "git", "worktree", "remove", "--force", wtPath)
				cmd.Dir = repoPath
				_ = cmd.Run()
			}
		}

		// Unregister from daemon (ignore errors during cleanup)
		_, _ = client.Send(socket.Request{
			Command: "remove_agent",
			Args: map[string]interface{}{
				"repo":  repoName,
				"agent": name,
			},
		})
	}

	fmt.Println()
	fmt.Printf("✓ Hibernated %d agent(s) in '%s'\n", len(agentsToHibernate), repoName)
	if len(archivedAgents) > 0 {
		fmt.Printf("✓ Archived %d agent(s) with uncommitted changes to:\n", len(archivedAgents))
		fmt.Printf("  %s\n", archiveDir)
		fmt.Println("\nTo restore archived patches:")
		fmt.Println("  cd <worktree>")
		fmt.Printf("  git apply %s/<agent>.patch\n", archiveDir)
	}

	return nil
}

// Workspace command implementations

// workspaceDefault handles `oat workspace` with no subcommand or `oat workspace <name>`
func (c *CLI) workspaceDefault(args []string) error {
	// If no args, list workspaces
	if len(args) == 0 {
		return c.listWorkspaces(args)
	}

	// If first arg looks like a workspace name (not a flag), treat as connect
	if !strings.HasPrefix(args[0], "-") {
		return c.connectWorkspace(args)
	}

	// Otherwise list with flags
	return c.listWorkspaces(args)
}

// addWorkspace creates a new workspace
func (c *CLI) addWorkspace(args []string) error {
	flags, posArgs := ParseFlags(args)

	if len(posArgs) < 1 {
		return errors.InvalidUsage("usage: oat workspace add <name> [--branch <branch>]")
	}

	workspaceName := posArgs[0]

	// Validate workspace name (same restrictions as branch names)
	if err := validateWorkspaceName(workspaceName); err != nil {
		return err
	}

	// Determine repository using standard resolution chain
	repoName, err := c.resolveRepo(flags)
	if err != nil {
		return err
	}

	// Determine branch to start from
	startBranch := "HEAD" // Default to current branch/HEAD
	if branch, ok := flags["branch"]; ok {
		startBranch = branch
		fmt.Printf("Creating workspace '%s' in repo '%s' from branch '%s'\n", workspaceName, repoName, branch)
	} else {
		fmt.Printf("Creating workspace '%s' in repo '%s'\n", workspaceName, repoName)
	}

	// Check if workspace already exists
	client := socket.NewClient(c.paths.DaemonSock)
	resp, err := client.Send(socket.Request{
		Command: "list_agents",
		Args: map[string]interface{}{
			"repo": repoName,
		},
	})
	if err != nil {
		return errors.DaemonCommunicationFailed("checking existing workspaces", err)
	}
	if !resp.Success {
		return errors.Wrap(errors.CategoryRuntime, "failed to check existing workspaces", fmt.Errorf("%s", resp.Error))
	}

	agents, _ := resp.Data.([]interface{})
	for _, agent := range agents {
		if agentMap, ok := agent.(map[string]interface{}); ok {
			agentType, _ := agentMap["type"].(string)
			name, _ := agentMap["name"].(string)
			if agentType == "workspace" && name == workspaceName {
				return fmt.Errorf("workspace '%s' already exists in repo '%s'", workspaceName, repoName)
			}
		}
	}

	// Get repository path
	repoPath := c.paths.RepoDir(repoName)

	// Create worktree
	wt := worktree.NewManagerWithContext(c.cmdCtx(), repoPath)
	wtPath := c.paths.AgentWorktree(repoName, workspaceName)
	branchName := fmt.Sprintf("workspace/%s", workspaceName)

	// Check if worktree path already exists (from previous incomplete workspace add)
	if _, err := os.Stat(wtPath); err == nil {
		fmt.Printf("Warning: Worktree path '%s' already exists\n", wtPath)
		fmt.Printf("This may be from a previous incomplete workspace creation.\n")
		fmt.Printf("Auto-repairing: removing existing worktree...\n")
		if err := wt.Remove(wtPath, true); err != nil {
			return fmt.Errorf("failed to clean up existing worktree: %w\nPlease manually remove it with: git worktree remove %s", err, wtPath)
		}
		fmt.Println("✓ Cleaned up stale worktree")
	}

	fmt.Printf("Creating worktree at: %s\n", wtPath)
	if err := wt.CreateNewBranch(wtPath, branchName, startBranch); err != nil {
		return errors.WorktreeCreationFailed(err)
	}

	// Get session name
	sessionName := sanitizeSessionName(repoName)

	// Check if agent already exists (stale from previous incomplete workspace add)
	if alive, err := c.backend.IsAgentAlive(context.Background(), sessionName, workspaceName); err == nil && alive {
		fmt.Printf("Warning: Agent '%s' already exists in session '%s'\n", workspaceName, sessionName)
		fmt.Printf("This may be from a previous incomplete workspace creation.\n")
		fmt.Printf("Auto-repairing: stopping existing agent...\n")
		if err := c.backend.StopAgent(context.Background(), sessionName, workspaceName); err != nil {
			return fmt.Errorf("failed to clean up existing agent: %w", err)
		}
		fmt.Println("✓ Cleaned up stale agent")
	}

	// Generate session ID for workspace
	workspaceSessionID, err := agent_pkg.GenerateSessionID()
	if err != nil {
		return fmt.Errorf("failed to generate workspace session ID: %w", err)
	}

	// Write prompt file for workspace
	workspacePromptFile, err := c.writePromptFile(repoPath, state.AgentTypeWorkspace, workspaceName)
	if err != nil {
		return fmt.Errorf("failed to write workspace prompt: %w", err)
	}

	// Copy hooks configuration if it exists
	if err := hooks.CopyConfig(repoPath, wtPath); err != nil {
		fmt.Printf("Warning: failed to copy hooks config: %v\n", err)
	}

	// Start agent in workspace window (skip in test mode)
	var workspacePID int
	if os.Getenv("OAT_TEST_MODE") != "1" {
		// Resolve agent binary
		agentBinary, err := c.getAgentBinary()
		if err != nil {
			return fmt.Errorf("failed to resolve agent binary: %w", err)
		}

		fmt.Println("Starting OAT agent in workspace window...")
		pid, err := c.startAgentViaBackend(agentBinary, sessionName, workspaceName, wtPath, workspaceSessionID, workspacePromptFile, repoName, "", "")
		if err != nil {
			return fmt.Errorf("failed to start workspace agent: %w", err)
		}
		workspacePID = pid
	}

	// Register workspace with daemon
	resp, err = client.Send(socket.Request{
		Command: "add_agent",
		Args: map[string]interface{}{
			"repo":          repoName,
			"agent":         workspaceName,
			"type":          "workspace",
			"worktree_path": wtPath,
			"window_name":   workspaceName,
			"session_id":    workspaceSessionID,
			"pid":           workspacePID,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to register workspace: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("failed to register workspace: %s", resp.Error)
	}

	fmt.Println()
	fmt.Println("✓ Workspace created successfully!")
	fmt.Printf("  Name: %s\n", workspaceName)
	fmt.Printf("  Branch: %s\n", branchName)
	fmt.Printf("  Worktree: %s\n", wtPath)
	fmt.Printf("\nMonitor agents: oat ui\n")
	fmt.Printf("Or tail this workspace: oat attach %s\n", workspaceName)

	return nil
}

// removeWorkspace removes a workspace
func (c *CLI) removeWorkspace(args []string) error {
	flags, remainingArgs := ParseFlags(args)

	// Determine repository
	repoName, err := c.resolveRepo(flags)
	if err != nil {
		return errors.NotInRepo()
	}

	// Get workspace info
	client := socket.NewClient(c.paths.DaemonSock)
	resp, err := client.Send(socket.Request{
		Command: "list_agents",
		Args: map[string]interface{}{
			"repo": repoName,
		},
	})
	if err != nil {
		return errors.DaemonCommunicationFailed("getting workspace info", err)
	}
	if !resp.Success {
		return errors.Wrap(errors.CategoryRuntime, "failed to get workspace info", fmt.Errorf("%s", resp.Error))
	}

	agents, _ := resp.Data.([]interface{})

	// Determine workspace name - from args or interactive selection
	var workspaceName string
	if len(remainingArgs) > 0 {
		workspaceName = remainingArgs[0]
	} else {
		// Interactive selection
		items := agentsToSelectableItems(agents, []string{"workspace"})
		if len(items) == 0 {
			return errors.NoWorkspacesFound(repoName)
		}
		selected, err := SelectFromList("Select workspace to remove:", items)
		if err != nil {
			return err
		}
		if selected == "" {
			fmt.Println("Canceled")
			return nil
		}
		workspaceName = selected
	}

	fmt.Printf("Removing workspace '%s' from repo '%s'\n", workspaceName, repoName)

	// Find workspace
	var workspaceInfo map[string]interface{}
	for _, agent := range agents {
		if agentMap, ok := agent.(map[string]interface{}); ok {
			agentType, _ := agentMap["type"].(string)
			name, _ := agentMap["name"].(string)
			if agentType == "workspace" && name == workspaceName {
				workspaceInfo = agentMap
				break
			}
		}
	}

	if workspaceInfo == nil {
		return errors.AgentNotFound("workspace", workspaceName, repoName)
	}

	// Get worktree path
	wtPath := workspaceInfo["worktree_path"].(string)

	// Check for uncommitted changes
	hasUncommitted, err := worktree.HasUncommittedChanges(c.cmdCtx(), wtPath)
	if err != nil {
		fmt.Printf("Warning: failed to check for uncommitted changes: %v\n", err)
	} else if hasUncommitted {
		fmt.Println("\nWarning: Workspace has uncommitted changes!")
		fmt.Println("Files may be lost if you continue with removal.")
		fmt.Print("Continue with removal? [y/N]: ")

		var response string
		_, _ = fmt.Scanln(&response) // EOF/empty -> treated as "N" below
		if response != "y" && response != "Y" {
			fmt.Println("Removal canceled")
			return nil
		}
	}

	// Check for unpushed commits; user declined cleanup -> exit early but not
	// as an error.
	if err := c.checkUnpushedCommits(wtPath, "Workspace", "removal"); err != nil {
		return nil //nolint:nilerr // user declined, not a CLI error
	}

	// Stop agent via backend
	sessionName := sanitizeSessionName(repoName)
	windowName, _ := workspaceInfo["window_name"].(string)
	if windowName == "" {
		windowName = workspaceName
	}
	fmt.Printf("Stopping agent: %s\n", windowName)
	if err := c.backend.StopAgent(context.Background(), sessionName, windowName); err != nil {
		fmt.Printf("Warning: failed to stop agent: %v\n", err)
	}

	// Remove worktree
	repoPath := c.paths.RepoDir(repoName)
	wt := worktree.NewManagerWithContext(c.cmdCtx(), repoPath)

	fmt.Printf("Removing worktree: %s\n", wtPath)
	if err := wt.Remove(wtPath, false); err != nil {
		fmt.Printf("Warning: failed to remove worktree: %v\n", err)
	}

	// Unregister from daemon
	resp, err = client.Send(socket.Request{
		Command: "remove_agent",
		Args: map[string]interface{}{
			"repo":  repoName,
			"agent": workspaceName,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to unregister workspace: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("failed to unregister workspace: %s", resp.Error)
	}

	fmt.Println("✓ Workspace removed successfully")
	return nil
}

// listWorkspaces lists all workspaces in a repository
func (c *CLI) listWorkspaces(args []string) error {
	flags, _ := ParseFlags(args)

	// Determine repository
	repoName, err := c.resolveRepo(flags)
	if err != nil {
		return errors.NotInRepo()
	}

	client := socket.NewClient(c.paths.DaemonSock)
	resp, err := client.Send(socket.Request{
		Command: "list_agents",
		Args: map[string]interface{}{
			"repo": repoName,
			"rich": true,
		},
	})
	if err != nil {
		return errors.DaemonCommunicationFailed("listing workspaces", err)
	}

	if !resp.Success {
		return errors.Wrap(errors.CategoryRuntime, "failed to list workspaces", fmt.Errorf("%s", resp.Error))
	}

	agents, ok := resp.Data.([]interface{})
	if !ok {
		return errors.New(errors.CategoryRuntime, "unexpected response format from daemon")
	}

	// Filter for workspaces
	workspaces := []map[string]interface{}{}
	for _, agent := range agents {
		if agentMap, ok := agent.(map[string]interface{}); ok {
			agentType, _ := agentMap["type"].(string)
			if agentType == "workspace" {
				workspaces = append(workspaces, agentMap)
			}
		}
	}

	if len(workspaces) == 0 {
		fmt.Printf("No workspaces in repository '%s'\n", repoName)
		format.Dimmed("\nCreate a workspace with: oat workspace add <name>")
		return nil
	}

	format.Header("Workspaces in '%s' (%d):", repoName, len(workspaces))
	fmt.Println()

	table := format.NewColoredTable("NAME", "BRANCH", "STATUS")
	for _, ws := range workspaces {
		name, _ := ws["name"].(string)
		status, _ := ws["status"].(string)
		branch, _ := ws["branch"].(string)

		// Format status with color
		statusCell := formatAgentStatusCell(status)

		// Format branch
		branchCell := format.ColorCell(branch, format.Cyan)
		if branch == "" {
			branchCell = format.ColorCell("-", format.Dim)
		}

		table.AddRow(
			format.Cell(name),
			branchCell,
			statusCell,
		)
	}
	table.Print()

	return nil
}

// connectWorkspace attaches to a workspace
func (c *CLI) connectWorkspace(args []string) error {
	flags, remainingArgs := ParseFlags(args)

	// Determine repository
	repoName, err := c.resolveRepo(flags)
	if err != nil {
		return errors.NotInRepo()
	}

	// Get workspace info
	client := socket.NewClient(c.paths.DaemonSock)
	resp, err := client.Send(socket.Request{
		Command: "list_agents",
		Args: map[string]interface{}{
			"repo": repoName,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to get workspace info: %w (is daemon running?)", err)
	}
	if !resp.Success {
		return fmt.Errorf("failed to get workspace info: %s", resp.Error)
	}

	agents, _ := resp.Data.([]interface{})

	// Determine workspace name - from args or interactive selection
	var workspaceName string
	if len(remainingArgs) > 0 {
		workspaceName = remainingArgs[0]
	} else {
		// Interactive selection
		items := agentsToSelectableItems(agents, []string{"workspace"})
		if len(items) == 0 {
			return errors.NoWorkspacesFound(repoName)
		}
		selected, err := SelectFromList("Select workspace to connect:", items)
		if err != nil {
			return err
		}
		if selected == "" {
			fmt.Println("Canceled")
			return nil
		}
		workspaceName = selected
	}

	// Find workspace
	var workspaceInfo map[string]interface{}
	for _, agent := range agents {
		if agentMap, ok := agent.(map[string]interface{}); ok {
			agentType, _ := agentMap["type"].(string)
			name, _ := agentMap["name"].(string)
			if agentType == "workspace" && name == workspaceName {
				workspaceInfo = agentMap
				break
			}
		}
	}

	if workspaceInfo == nil {
		return errors.WorkspaceNotFound(workspaceName, repoName)
	}

	// Attach via backend
	sessionName := sanitizeSessionName(repoName)
	windowName, _ := workspaceInfo["window_name"].(string)
	if windowName == "" {
		windowName = workspaceName
	}

	readOnly := flags["read-only"] == "true" || flags["r"] == "true"
	return c.backend.Attach(context.Background(), sessionName, windowName, readOnly)
}

// validateWorkspaceName validates that a workspace name follows branch name restrictions
func validateWorkspaceName(name string) error {
	if name == "" {
		return fmt.Errorf("workspace name cannot be empty")
	}

	// Git branch name restrictions
	// - Cannot start with . or -
	// - Cannot contain consecutive dots ..
	// - Cannot contain \ or any of these characters: ~ ^ : ? * [ @ { } space
	// - Cannot end with . or /
	// - Cannot be "." or ".."

	if name == "." || name == ".." {
		return fmt.Errorf("workspace name cannot be '.' or '..'")
	}

	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "-") {
		return fmt.Errorf("workspace name cannot start with '.' or '-'")
	}

	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, "/") {
		return fmt.Errorf("workspace name cannot end with '.' or '/'")
	}

	if strings.Contains(name, "..") {
		return fmt.Errorf("workspace name cannot contain '..'")
	}

	invalidChars := []string{"\\", "~", "^", ":", "?", "*", "[", "@", "{", "}", " ", "\t", "\n"}
	for _, char := range invalidChars {
		if strings.Contains(name, char) {
			return fmt.Errorf("workspace name cannot contain '%s'", char)
		}
	}

	return nil
}

// getReposList is a helper to get the list of repos
func (c *CLI) getReposList() []string {
	client := socket.NewClient(c.paths.DaemonSock)
	resp, err := client.Send(socket.Request{Command: "list_repos"})
	if err != nil {
		return []string{}
	}

	if !resp.Success {
		return []string{}
	}

	repos, ok := resp.Data.([]interface{})
	if !ok {
		return []string{}
	}

	result := make([]string, 0, len(repos))
	for _, repo := range repos {
		if repoStr, ok := repo.(string); ok {
			result = append(result, repoStr)
		}
	}

	return result
}

func (c *CLI) tellAgent(args []string) error {
	flags, posArgs := ParseFlags(args)
	if len(posArgs) < 2 {
		return errors.InvalidUsage("usage: oat agent tell <agent-name> <message> [--repo <repo>]")
	}

	agentName := posArgs[0]
	message := strings.Join(posArgs[1:], " ")

	repoName, err := c.resolveRepo(flags)
	if err != nil {
		return errors.NotInRepo()
	}

	_, err = c.sendDaemonRequest("send_agent_input", map[string]interface{}{
		"repo":    repoName,
		"agent":   agentName,
		"message": message,
	})
	if err != nil {
		return err
	}

	fmt.Printf("Sent input to %s in %s\n", agentName, repoName)
	return nil
}

func (c *CLI) interruptAgent(args []string) error {
	flags, posArgs := ParseFlags(args)
	if len(posArgs) < 1 {
		return errors.InvalidUsage("usage: oat agent interrupt <agent-name> [--repo <repo>]")
	}

	agentName := posArgs[0]
	repoName, err := c.resolveRepo(flags)
	if err != nil {
		return errors.NotInRepo()
	}

	_, err = c.sendDaemonRequest("interrupt_agent", map[string]interface{}{
		"repo":  repoName,
		"agent": agentName,
	})
	if err != nil {
		return err
	}

	fmt.Printf("Sent interrupt to %s in %s\n", agentName, repoName)
	return nil
}

func (c *CLI) sendMessage(args []string) error {
	if len(args) < 2 {
		return errors.InvalidUsage("usage: oat agent send-message <to> <message>")
	}

	to := args[0]
	body := strings.Join(args[1:], " ")

	// Determine current agent and repo
	repoName, agentName, err := c.inferAgentContext()
	if err != nil {
		return err
	}

	// Create message manager
	msgMgr := messages.NewManager(c.paths.MessagesDir)

	// Send message
	msg, err := msgMgr.Send(repoName, agentName, to, body)
	if err != nil {
		return fmt.Errorf("failed to send message: %w", err)
	}

	// Trigger immediate routing (best-effort, polling is fallback)
	client := socket.NewClient(c.paths.DaemonSock)
	_, _ = client.Send(socket.Request{Command: "route_messages"})
	// Ignore errors - 2-minute polling fallback will catch it

	fmt.Printf("Message sent to %s (ID: %s)\n", to, msg.ID)
	return nil
}

func (c *CLI) listMessages(args []string) error {
	// Determine current agent and repo
	repoName, agentName, err := c.inferAgentContext()
	if err != nil {
		return err
	}

	msgMgr := messages.NewManager(c.paths.MessagesDir)

	// List messages
	msgs, err := msgMgr.List(repoName, agentName)
	if err != nil {
		return fmt.Errorf("failed to list messages: %w", err)
	}

	if len(msgs) == 0 {
		fmt.Println("No messages")
		return nil
	}

	fmt.Printf("Messages for %s (%d):\n", agentName, len(msgs))
	for _, msg := range msgs {
		status := msg.Status
		if msg.Status == messages.StatusAcked && msg.AckedAt != nil {
			status = messages.Status(fmt.Sprintf("acked (%s)", formatTime(*msg.AckedAt)))
		}
		fmt.Printf("  [%s] %s - From: %s - %s - %s\n",
			msg.ID,
			formatTime(msg.Timestamp),
			msg.From,
			status,
			truncateString(msg.Body, 60))
	}

	return nil
}

func (c *CLI) readMessage(args []string) error {
	if len(args) < 1 {
		return errors.InvalidUsage("usage: oat agent read-message <message-id>")
	}

	messageID := args[0]

	// Determine current agent and repo
	repoName, agentName, err := c.inferAgentContext()
	if err != nil {
		return err
	}

	msgMgr := messages.NewManager(c.paths.MessagesDir)

	// Get message
	msg, err := msgMgr.Get(repoName, agentName, messageID)
	if err != nil {
		return fmt.Errorf("failed to read message: %w", err)
	}

	// Update status to read
	if msg.Status == messages.StatusPending || msg.Status == messages.StatusDelivered {
		if err := msgMgr.UpdateStatus(repoName, agentName, messageID, messages.StatusRead); err != nil {
			fmt.Printf("Warning: failed to update message status: %v\n", err)
		}
	}

	// Display message
	fmt.Printf("Message: %s\n", msg.ID)
	fmt.Printf("From: %s\n", msg.From)
	fmt.Printf("To: %s\n", msg.To)
	fmt.Printf("Time: %s\n", msg.Timestamp.Format(time.RFC3339))
	fmt.Printf("Status: %s\n", msg.Status)
	if msg.AckedAt != nil {
		fmt.Printf("Acked: %s\n", msg.AckedAt.Format(time.RFC3339))
	}
	fmt.Println()
	fmt.Println(msg.Body)

	return nil
}

func (c *CLI) ackMessage(args []string) error {
	if len(args) < 1 {
		return errors.InvalidUsage("usage: oat agent ack-message <message-id>")
	}

	messageID := args[0]

	// Determine current agent and repo
	repoName, agentName, err := c.inferAgentContext()
	if err != nil {
		return err
	}

	msgMgr := messages.NewManager(c.paths.MessagesDir)

	// Ack message
	if err := msgMgr.Ack(repoName, agentName, messageID); err != nil {
		return fmt.Errorf("failed to acknowledge message: %w", err)
	}

	fmt.Printf("Message %s acknowledged\n", messageID)
	return nil
}

// collectCLIEnvVars returns a map of environment variables from the CLI process
// that should be forwarded to the daemon for agent startup. This ensures agents
// inherit tokens and API keys even when the daemon was started in a different
// environment (common with the direct backend). Uses the canonical provider
// list from internal/preflight so `oat init`, `oat doctor`, and the daemon
// agree on which keys count.
func collectCLIEnvVars() map[string]string {
	envKeys := []string{
		"GH_TOKEN", "GITHUB_TOKEN", "GH_TOKEN_ORG", "GH_TOKEN_CLASSIC",
	}
	envKeys = append(envKeys, preflight.KeyProviders...)
	result := make(map[string]string)
	for _, key := range envKeys {
		if val := os.Getenv(key); val != "" {
			result[key] = val
		}
	}
	return result
}

// inferRepoFromCwd infers just the repository name from the current working directory.
// Unlike inferAgentContext, it doesn't require determining the specific agent.
func (c *CLI) inferRepoFromCwd() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get current directory: %w", err)
	}

	// Resolve symlinks in cwd for proper path comparison
	// This is especially important on macOS where /tmp -> /private/tmp
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}

	// Check if we're in a worktree path
	// Path format: ~/.oat/wts/<repo>/<agent>
	if hasPathPrefix(cwd, c.paths.WorktreesDir) {
		rel, err := filepath.Rel(c.paths.WorktreesDir, cwd)
		if err == nil {
			parts := strings.SplitN(rel, string(filepath.Separator), 2)
			if len(parts) >= 1 && parts[0] != "" && parts[0] != "." {
				return parts[0], nil
			}
		}
	}

	// Check if we're in a main repo path
	// Path format: ~/.oat/repos/<repo>
	if hasPathPrefix(cwd, c.paths.ReposDir) {
		rel, err := filepath.Rel(c.paths.ReposDir, cwd)
		if err == nil {
			parts := strings.SplitN(rel, string(filepath.Separator), 2)
			if len(parts) >= 1 && parts[0] != "" && parts[0] != "." {
				return parts[0], nil
			}
		}
	}

	return "", fmt.Errorf("not in a oat directory")
}

// inferAgentNameFromCwd attempts to determine the agent name from the current working directory.
// Returns empty string if not in an agent worktree.
func (c *CLI) inferAgentNameFromCwd() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}

	// Resolve symlinks in cwd for proper path comparison
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}

	// Check if we're in a worktree path
	// Path format: ~/.oat/wts/<repo>/<agent>
	if hasPathPrefix(cwd, c.paths.WorktreesDir) {
		rel, err := filepath.Rel(c.paths.WorktreesDir, cwd)
		if err == nil {
			parts := strings.SplitN(rel, string(filepath.Separator), 3)
			if len(parts) >= 2 && parts[1] != "" && parts[1] != "." {
				return parts[1] // Return agent name (second part)
			}
		}
	}

	return ""
}

// normalizeGitHubURL normalizes GitHub URLs to a common format for comparison.
// It handles both SSH (git@github.com:user/repo.git) and HTTPS (https://github.com/user/repo) formats.
// Returns lowercase "github.com/user/repo" format for comparison.
func normalizeGitHubURL(url string) string {
	url = strings.TrimSpace(url)
	lowerURL := strings.ToLower(url)

	// Handle SSH format: git@github.com:user/repo.git
	if strings.HasPrefix(lowerURL, "git@github.com:") {
		path := url[len("git@github.com:"):]
		path = strings.TrimSuffix(path, ".git")
		return strings.ToLower("github.com/" + path)
	}

	// Handle HTTPS format: https://github.com/user/repo or https://github.com/user/repo.git
	if strings.HasPrefix(lowerURL, "https://github.com/") {
		path := url[len("https://"):]
		path = strings.TrimSuffix(path, ".git")
		return strings.ToLower(path)
	}

	// Handle HTTP format: http://github.com/user/repo
	if strings.HasPrefix(lowerURL, "http://github.com/") {
		path := url[len("http://"):]
		path = strings.TrimSuffix(path, ".git")
		return strings.ToLower(path)
	}

	// Handle git:// protocol: git://github.com/user/repo.git
	if strings.HasPrefix(lowerURL, "git://github.com/") {
		path := url[len("git://"):]
		path = strings.TrimSuffix(path, ".git")
		return strings.ToLower(path)
	}

	// Return empty string for non-GitHub URLs
	return ""
}

// issueNumberFromTaskRegex extracts an issue number from a task description like "Work issue #5: ..."
var issueNumberFromTaskRegex = regexp.MustCompile(`(?i)issue\s*#(\d+)`)

// extractOwnerRepo extracts "owner/repo" from a GitHub URL.
// Handles HTTPS, HTTP, SSH, and git:// formats.
func extractOwnerRepo(githubURL string) string {
	githubURL = strings.TrimSpace(githubURL)
	lowerURL := strings.ToLower(githubURL)

	prefixes := []struct {
		lower  string
		actual string
	}{
		{"https://github.com/", "https://github.com/"},
		{"http://github.com/", "http://github.com/"},
		{"git@github.com:", "git@github.com:"},
		{"git://github.com/", "git://github.com/"},
	}

	for _, p := range prefixes {
		if strings.HasPrefix(lowerURL, p.lower) {
			path := githubURL[len(p.actual):]
			return strings.TrimSuffix(path, ".git")
		}
	}
	return githubURL
}

// checkPRCIStatus checks the latest CI run on the worker's branch and returns
// the conclusion ("success", "failure", "pending", etc). Returns "" on error
// or if no runs are found, so callers can treat unknown as non-blocking.
func checkPRCIStatus(ghRepo, agentName string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "run", "list",
		"--repo", ghRepo,
		"--branch", "work/"+agentName,
		"--limit", "1",
		"--json", "conclusion,status",
		"--jq", ".[0] | if .conclusion != \"\" then .conclusion else .status end",
	)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// findRepoFromGitRemote looks for a git remote in the current directory
// and tries to match it against known repositories in state.
func (c *CLI) findRepoFromGitRemote() (string, error) {
	// Run git remote get-url origin
	cmd := exec.CommandContext(c.cmdCtx(), "git", "remote", "get-url", "origin")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get git remote: %w", err)
	}

	remoteURL := strings.TrimSpace(string(output))
	if remoteURL == "" {
		return "", fmt.Errorf("git remote URL is empty")
	}

	normalizedRemote := normalizeGitHubURL(remoteURL)
	if normalizedRemote == "" {
		return "", fmt.Errorf("not a GitHub URL: %s", remoteURL)
	}

	// Load state to check against known repositories
	st, err := c.loadState()
	if err != nil {
		return "", err
	}

	// Iterate through repos and find a match
	for _, repoName := range st.ListRepos() {
		repo, exists := st.GetRepo(repoName)
		if !exists {
			continue
		}

		normalizedStateURL := normalizeGitHubURL(repo.GithubURL)
		if normalizedStateURL != "" && normalizedStateURL == normalizedRemote {
			return repoName, nil
		}
	}

	return "", fmt.Errorf("no matching repository found for remote: %s", remoteURL)
}

// resolveRepo determines the repository to use based on:
// 1. Explicit --repo flag (highest priority)
// 2. Git remote URL matching (if in a git repo with origin pointing to a tracked repo)
// 3. Current working directory (if in a oat directory)
// 4. Current repo set via 'oat repo use' (lowest priority)
func (c *CLI) resolveRepo(flags map[string]string) (string, error) {
	// 1. Check explicit --repo flag
	if r, ok := flags["repo"]; ok {
		// Normalize "org/repo" to just "repo" — LLM agents sometimes pass
		// the full GitHub owner/repo format (e.g., "Root-IO-Labs/my-repo")
		// but OAT state keys are bare repo names.
		r = strings.TrimRight(r, "/")
		if idx := strings.LastIndex(r, "/"); idx >= 0 {
			r = r[idx+1:]
		}
		return r, nil
	}

	// 2. Try to infer from git remote URL
	if repoName, err := c.findRepoFromGitRemote(); err == nil {
		return repoName, nil
	}

	// 3. Try to infer from current working directory
	if inferred, err := c.inferRepoFromCwd(); err == nil {
		return inferred, nil
	}

	// 4. Check current repo from daemon
	client := socket.NewClient(c.paths.DaemonSock)
	resp, err := client.Send(socket.Request{
		Command: "get_current_repo",
	})
	if err == nil && resp.Success {
		if currentRepo, ok := resp.Data.(string); ok && currentRepo != "" {
			return currentRepo, nil
		}
	}

	// If multiple repos exist, give a specific suggestion
	repoResp, repoErr := client.Send(socket.Request{Command: "list_repos"})
	if repoErr == nil && repoResp.Success {
		if repoList, ok := repoResp.Data.([]interface{}); ok && len(repoList) > 1 {
			return "", errors.MultipleRepos()
		}
	}
	return "", fmt.Errorf("could not determine repository; use --repo flag or run 'oat repo use <name>'")
}

// inferAgentContext infers the current agent and repo from working directory
func (c *CLI) inferAgentContext() (repoName, agentName string, err error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", "", fmt.Errorf("failed to get current directory: %w", err)
	}

	// Resolve symlinks in cwd for proper path comparison
	// This is especially important on macOS where /tmp -> /private/tmp
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}

	// Check if we're in a worktree path
	// Path format: ~/.oat/wts/<repo>/<agent>
	if hasPathPrefix(cwd, c.paths.WorktreesDir) {
		// Extract repo and agent from path
		rel, err := filepath.Rel(c.paths.WorktreesDir, cwd)
		if err == nil {
			parts := strings.SplitN(rel, string(filepath.Separator), 2)
			if len(parts) >= 2 {
				// Prefer OAT_AGENT_NAME over path-derived name.
				// A fix worker may operate in another worker's worktree;
				// the env var reflects the actual agent identity.
				if envAgent := os.Getenv("OAT_AGENT_NAME"); envAgent != "" {
					return parts[0], envAgent, nil
				}
				return parts[0], parts[1], nil
			}
			if len(parts) == 1 {
				// We're in the repo worktree dir itself
				return parts[0], "", fmt.Errorf("cannot determine agent - in repo worktree directory")
			}
		}
	}

	// Check if we're in a main repo path
	// Path format: ~/.oat/repos/<repo>
	if hasPathPrefix(cwd, c.paths.ReposDir) {
		rel, err := filepath.Rel(c.paths.ReposDir, cwd)
		if err == nil {
			parts := strings.SplitN(rel, string(filepath.Separator), 2)
			if len(parts) >= 1 {
				// In main repo - could be supervisor or merge-queue
				// Check OAT_AGENT_NAME env var first (set by backend for all agents)
				if agentName := os.Getenv("OAT_AGENT_NAME"); agentName != "" {
					return parts[0], agentName, nil
				}

				// Fallback: assume supervisor
				return parts[0], "supervisor", nil
			}
		}
	}

	return "", "", errors.NotInAgentContext()
}

// Helper functions

// hasPathPrefix checks if path starts with prefix using proper path semantics.
// Unlike strings.Contains or strings.HasPrefix, this ensures we're comparing
// complete path components (e.g., "/foo/bar" is under "/foo" but not under "/fo").
func hasPathPrefix(path, prefix string) bool {
	// Clean both paths to normalize them
	path = filepath.Clean(path)
	prefix = filepath.Clean(prefix)

	// Check if path equals or starts with prefix followed by separator
	if path == prefix {
		return true
	}
	// Ensure prefix ends with separator for proper prefix matching
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(path, prefix)
}

func formatTime(t time.Time) string {
	if time.Since(t) < 24*time.Hour {
		return t.Format("15:04:05")
	}
	return t.Format("Jan 02 15:04")
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// checkUnpushedCommits checks if a worktree has unpushed commits and prompts the user for confirmation.
// Returns nil if the user wants to continue, or an error to cancel the operation.
// The entityType parameter should be "Worker" or "Workspace" for appropriate messaging.
// The action parameter should be "cleanup" or "removal" for appropriate messaging.
func (c *CLI) checkUnpushedCommits(wtPath, entityType, action string) error {
	hasUnpushed, err := worktree.HasUnpushedCommits(c.cmdCtx(), wtPath)
	if err != nil {
		// This is ok - might not have a tracking branch
		fmt.Printf("Note: Could not check for unpushed commits (no tracking branch?)\n")
		return nil //nolint:nilerr // inability to check isn't a fatal CLI error
	}

	if !hasUnpushed {
		return nil
	}

	fmt.Printf("\nWarning: %s has unpushed commits!\n", entityType)
	branch, err := worktree.GetCurrentBranch(c.cmdCtx(), wtPath)
	if err == nil {
		fmt.Printf("Branch '%s' has commits not pushed to remote.\n", branch)
	}
	fmt.Printf("These commits may be lost if you continue with %s.\n", action)
	fmt.Printf("Continue with %s? [y/N]: ", action)

	var response string
	_, _ = fmt.Scanln(&response) // EOF/empty -> treated as "N" below
	if response != "y" && response != "Y" {
		// Capitalize first letter of action for the message
		actionCapitalized := strings.ToUpper(action[:1]) + action[1:]
		fmt.Printf("%s canceled\n", actionCapitalized)
		return fmt.Errorf("canceled by user")
	}
	return nil
}

func (c *CLI) completeWorker(args []string) error {
	flags, _ := ParseFlags(args)

	repoName, agentName, err := c.inferAgentContext()
	if err != nil {
		return fmt.Errorf("failed to determine agent context: %w", err)
	}

	reqArgs := map[string]interface{}{
		"repo":  repoName,
		"agent": agentName,
	}

	// --worker lets the supervisor complete a worker on its behalf
	if targetWorker, ok := flags["worker"]; ok && targetWorker != "" {
		reqArgs["target_agent"] = targetWorker
		fmt.Printf("Completing worker '%s' on behalf of '%s'...\n", targetWorker, agentName)
	} else {
		fmt.Printf("Marking agent '%s' as complete...\n", agentName)
	}

	// Add optional summary
	if summary, ok := flags["summary"]; ok && summary != "" {
		reqArgs["summary"] = summary
		fmt.Printf("Summary: %s\n", summary)
	}

	// Add optional failure reason
	if failureReason, ok := flags["failure"]; ok && failureReason != "" {
		reqArgs["failure_reason"] = failureReason
		fmt.Printf("Failure reason: %s\n", failureReason)
	}

	force := flags["force"] == "true"
	_, isFailure := flags["failure"]
	_, isSupervisorComplete := flags["worker"]

	// CI guardrail: warn if the worker has an open PR with pending/failing CI.
	// Skip when: --force, --failure, --worker (supervisor completing on behalf), or no PR.
	if !force && !isFailure && !isSupervisorComplete {
		if st, stErr := c.loadState(); stErr == nil {
			if agent, found := st.GetAgent(repoName, agentName); found && agent.PRNumber > 0 {
				if repo, repoFound := st.GetRepo(repoName); repoFound {
					ghRepo := extractOwnerRepo(repo.GithubURL)
					ciStatus := checkPRCIStatus(ghRepo, agentName)
					if ciStatus != "" && ciStatus != "success" {
						return fmt.Errorf(
							"your PR #%d has CI status '%s' — completing now will abandon it\n"+
								"  - If CI is still running, wait for it: run `oat agent waiting` instead.\n"+
								"  - If you're intentionally abandoning this PR, re-run with: oat agent complete --force",
							agent.PRNumber, ciStatus)
					}
				}
			}
		}
	}

	client := socket.NewClient(c.paths.DaemonSock)
	resp, err := client.Send(socket.Request{
		Command: "complete_agent",
		Args:    reqArgs,
	})
	if err != nil {
		return errors.DaemonCommunicationFailed("marking agent complete", err)
	}
	if !resp.Success {
		return errors.Wrap(errors.CategoryRuntime, "failed to mark agent complete", fmt.Errorf("%s", resp.Error))
	}

	fmt.Println("✓ Agent marked as complete")
	fmt.Println("The daemon will clean up this agent's resources shortly.")

	// Auto-close issue if: successful completion (no --failure), has issue, no PR created
	if !isFailure {
		st, stErr := c.loadState()
		if stErr == nil {
			agent, found := st.GetAgent(repoName, agentName)
			// Fallback: parse issue number from task description if IssueNumber not set
			if found && agent.IssueNumber == "" && agent.Task != "" {
				if match := issueNumberFromTaskRegex.FindStringSubmatch(agent.Task); len(match) > 1 {
					agent.IssueNumber = match[1]
				}
			}
			if found && agent.IssueNumber != "" && agent.PRNumber == 0 {
				repo, repoFound := st.GetRepo(repoName)
				if repoFound {
					ghRepo := extractOwnerRepo(repo.GithubURL)

					// Check if the worker has an open PR on its branch before closing the issue.
					// PRNumber==0 only means the worker never went dormant, not that no PR exists.
					hasOpenPR := false
					checkCtx, checkCancel := context.WithTimeout(context.Background(), 15*time.Second)
					checkCmd := exec.CommandContext(checkCtx, "gh", "pr", "list",
						"--repo", ghRepo,
						"--head", "work/"+agentName,
						"--state", "open",
						"--json", "number",
						"--jq", "length")
					if checkOut, checkErr := checkCmd.Output(); checkErr == nil {
						if count := strings.TrimSpace(string(checkOut)); count != "" && count != "0" {
							hasOpenPR = true
						}
					}
					checkCancel()

					if hasOpenPR {
						fmt.Printf("Skipping issue close: open PR exists on branch work/%s. The issue will close when the PR is merged via 'Closes #%s'.\n", agentName, agent.IssueNumber)
					} else {
						closeCtx, closeCancel := context.WithTimeout(context.Background(), 15*time.Second)
						defer closeCancel()
						closeCmd := exec.CommandContext(closeCtx, "gh", "issue", "close", agent.IssueNumber,
							"--repo", ghRepo,
							"--comment", fmt.Sprintf("Closed by agent %s (no PR needed)", agentName))
						if err := closeCmd.Run(); err == nil {
							fmt.Printf("✓ Closed issue #%s (no PR created)\n", agent.IssueNumber)
						}
					}
				}
			} else if found && agent.IssueNumber != "" && agent.PRNumber > 0 {
				repo, repoFound := st.GetRepo(repoName)
				if repoFound {
					ghRepo := extractOwnerRepo(repo.GithubURL)
					prNum := strconv.Itoa(agent.PRNumber)

					viewCtx, viewCancel := context.WithTimeout(context.Background(), 15*time.Second)
					viewCmd := exec.CommandContext(viewCtx, "gh", "pr", "view", prNum,
						"--repo", ghRepo, "--json", "state", "--jq", ".state")
					viewOut, viewErr := viewCmd.Output()
					viewCancel()

					if viewErr == nil {
						prState := strings.TrimSpace(string(viewOut))
						if prState == "CLOSED" {
							closeCtx, closeCancel := context.WithTimeout(context.Background(), 15*time.Second)
							defer closeCancel()
							closeCmd := exec.CommandContext(closeCtx, "gh", "issue", "close", agent.IssueNumber,
								"--repo", ghRepo,
								"--comment", fmt.Sprintf("Closed by agent %s: PR #%s was closed without merging", agentName, prNum))
							if err := closeCmd.Run(); err == nil {
								fmt.Printf("✓ Closed issue #%s (PR #%s was closed without merging)\n", agent.IssueNumber, prNum)
							}
						}
					}
				}
			}
		}
	}

	return nil
}

func (c *CLI) waitingForPR(args []string) error {
	repoName, agentName, err := c.inferAgentContext()
	if err != nil {
		return fmt.Errorf("failed to determine agent context: %w", err)
	}

	fmt.Printf("Marking agent '%s' as waiting for PR resolution...\n", agentName)

	client := socket.NewClient(c.paths.DaemonSock)
	resp, err := client.Send(socket.Request{
		Command: "agent_waiting",
		Args: map[string]interface{}{
			"repo":  repoName,
			"agent": agentName,
		},
	})
	if err != nil {
		return errors.DaemonCommunicationFailed("marking agent waiting", err)
	}
	if !resp.Success {
		return errors.Wrap(errors.CategoryRuntime, "failed to mark agent waiting", fmt.Errorf("%s", resp.Error))
	}

	if data, ok := resp.Data.(map[string]interface{}); ok {
		switch data["status"] {
		case "auto_completed":
			fmt.Println("No PR found — worker has been auto-completed.")
			if msg, ok := data["message"].(string); ok {
				fmt.Println(msg)
			}
			return nil
		case "dormant_verification":
			fmt.Println("✓ Agent is now dormant (waiting for verification verdict)")
			fmt.Println("The daemon will wake you when the verdict arrives.")
			fmt.Println("\n══════════════════════════════════════════════════════════════")
			if verifier, ok := data["verifier"].(string); ok && verifier != "" {
				fmt.Printf("DORMANT: Waiting for verification verdict from %s\n", verifier)
			} else {
				fmt.Println("DORMANT: Waiting for verification verdict")
			}
			fmt.Println("STOP. Do NOT generate any text, run any commands, or take")
			fmt.Println("any action until you see a new USER message from the daemon.")
			fmt.Println("Any \"[daemon] Status check\" messages below are STALE -- they")
			fmt.Println("were queued BEFORE you went dormant. Ignore them completely.")
			fmt.Println("Do NOT respond to them. Do NOT poll. Do NOT check status.")
			fmt.Println("STOP HERE. WAIT FOR THE DAEMON.")
			fmt.Println("══════════════════════════════════════════════════════════════")
			return nil
		case "already_dormant":
			if msg, ok := data["message"].(string); ok {
				fmt.Println(msg)
			}
			return nil
		}
	}

	fmt.Println("✓ Agent is now dormant (waiting for PR)")
	fmt.Println("The daemon will notify you when your PR needs attention (CI failure, merge conflict, merge, or comments).")
	fmt.Println("\n══════════════════════════════════════════════════════════════")
	fmt.Println("DORMANT: Waiting for PR resolution.")
	fmt.Println("STOP. Do NOT generate any text, run any commands, or take")
	fmt.Println("any action until you see a new USER message from the daemon.")
	fmt.Println("Any \"[daemon] Status check\" messages below are STALE -- they")
	fmt.Println("were queued BEFORE you went dormant. Ignore them completely.")
	fmt.Println("Do NOT respond to them. Do NOT poll. Do NOT check status.")
	fmt.Println("STOP HERE. WAIT FOR THE DAEMON.")
	fmt.Println("══════════════════════════════════════════════════════════════")
	return nil
}

func (c *CLI) prCreate(args []string) error {
	flags, _ := ParseFlags(args)

	title := flags["title"]
	if title == "" {
		return errors.InvalidUsage("--title is required: oat pr create --title <title> --body <body> [--closes <issue>] [--draft] [--force]")
	}
	body := flags["body"]
	closesIssue := flags["closes"]
	_, isDraft := flags["draft"]
	_, forceSkipVerify := flags["force"]

	repoName, agentName, err := c.inferAgentContext()
	if err != nil {
		return fmt.Errorf("failed to determine agent context: %w", err)
	}

	// Verification gate (dual-mode):
	// 1. Verification agent approved the current commit (preferred), OR
	// 2. `oat worker verify` passed for the current commit (fallback, with timeout when pending)
	// Use --force to bypass entirely.
	const verificationPendingTimeout = 5 * time.Minute
	if !forceSkipVerify {
		commitSHA := c.getCurrentCommitSHA()
		if commitSHA != "" {
			gatePass := false
			pendingBlocked := false

			// Check 1: verification agent approval (commit-bound)
			st, stErr := c.loadState()
			if stErr == nil {
				if agent, found := st.GetAgent(repoName, agentName); found {
					if agent.VerificationStatus == "approved" && agent.VerifiedCommitSHA == commitSHA {
						fmt.Println("Verification agent approved this commit")
						gatePass = true
					} else if agent.VerificationStatus == "pending" {
						// Check if the verifier is still alive and how long it's been running
						verifierAlive := false
						if agent.VerificationAgent != "" {
							if verifier, vFound := st.GetAgent(repoName, agent.VerificationAgent); vFound {
								verifierAlive = true
								elapsed := time.Since(verifier.CreatedAt)
								if elapsed < verificationPendingTimeout {
									pendingBlocked = true
									fmt.Printf("Verification agent is still reviewing your work (started %s ago).\n", elapsed.Round(time.Second))
									fmt.Println("Wait for the [APPROVED] or [REJECTED] message before creating a PR.")
									fmt.Println("If you've already called `oat worker request-review`, run `oat agent waiting` to go dormant — the daemon will wake you when verification finishes.")
									fmt.Println("If you need to skip verification: oat pr create --force")
								} else {
									fmt.Printf("Verification agent has been pending for %s (> %s timeout) — checking fallback...\n", elapsed.Round(time.Second), verificationPendingTimeout)
								}
							}
						}
						if !verifierAlive && !pendingBlocked {
							fmt.Println("Verification agent no longer running — checking fallback...")
						}
					} else if agent.VerificationStatus == "rejected" {
						if agent.VerifiedCommitSHA == commitSHA {
							fmt.Printf("Verification agent rejected this commit: %s\n", agent.VerificationReason)
							fmt.Println("Fix the issues, push a new commit, and run 'oat worker request-review' again.")
							fmt.Println("Or use 'oat pr create --force' to bypass verification.")
							return fmt.Errorf("verification agent rejected this commit — fix and re-request review")
						}
						fmt.Printf("Verification agent rejected a prior commit (%s): %s — checking fallback for current commit...\n",
							agent.VerifiedCommitSHA[:min(len(agent.VerifiedCommitSHA), 8)], agent.VerificationReason)
					}
				}
			}

			if pendingBlocked {
				return fmt.Errorf("verification agent still in progress — wait for result or use --force")
			}

			// Check 2: fallback to oat worker verify log
			if !gatePass {
				passed, found := c.getLastVerificationForCommit(commitSHA)
				if found && passed {
					fmt.Println("Self-verification passed for current commit (fallback)")
					gatePass = true
				}
			}

			if !gatePass {
				fmt.Println("No verification passed for current commit.")
				fmt.Println("   Option 1: oat worker request-review  (spawns verification agent)")
				fmt.Println("   Option 2: oat worker verify           (self-verify fallback)")
				fmt.Println("   Option 3: oat pr create --force       (skip verification)")
				return fmt.Errorf("verification required before PR creation")
			}
		}
	} else {
		fmt.Println("Skipping verification gate (--force)")
	}

	// Load state for --repo and auto-close detection (graceful fallback if unavailable)
	var ghRepo, branch string
	st, stErr := c.loadState()
	if stErr == nil {
		if repo, exists := st.GetRepo(repoName); exists {
			ghRepo = extractOwnerRepo(repo.GithubURL)
		}
		// Auto-detect issue number from agent state when --closes not provided
		if closesIssue == "" {
			if agent, found := st.GetAgent(repoName, agentName); found {
				if agent.IssueNumber != "" {
					closesIssue = agent.IssueNumber
				} else if agent.Task != "" {
					if match := issueNumberFromTaskRegex.FindStringSubmatch(agent.Task); len(match) > 1 {
						closesIssue = match[1]
					}
				}
			}
		}
	}

	// Detect current branch from worktree (handles fixup workers on different branches)
	branchCmd := exec.CommandContext(c.cmdCtx(), "git", "rev-parse", "--abbrev-ref", "HEAD")
	if branchOut, branchErr := branchCmd.Output(); branchErr == nil {
		branch = strings.TrimSpace(string(branchOut))
	}

	// Duplicate PR guard: check for existing/recently-merged PRs before creating
	if !forceSkipVerify && branch != "" {
		// Check 1: open PR on same branch
		checkArgs := []string{"pr", "list", "--head", branch, "--state", "open", "--json", "number,url"}
		if ghRepo != "" {
			checkArgs = append(checkArgs, "--repo", ghRepo)
		}
		checkCtx, checkCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer checkCancel()
		checkOut, checkErr := exec.CommandContext(checkCtx, "gh", checkArgs...).Output()
		if checkErr == nil {
			var existingPRs []struct {
				Number int    `json:"number"`
				URL    string `json:"url"`
			}
			if json.Unmarshal(checkOut, &existingPRs) == nil && len(existingPRs) > 0 {
				pr := existingPRs[0]
				fmt.Printf("Open PR already exists for branch '%s': #%d (%s)\n", branch, pr.Number, pr.URL)
				fmt.Println("Skipping PR creation — going dormant with existing PR.")
				client := socket.NewClient(c.paths.DaemonSock)
				resp, sockErr := client.Send(socket.Request{
					Command: "agent_waiting",
					Args: map[string]interface{}{
						"repo":      repoName,
						"agent":     agentName,
						"pr_number": pr.Number,
					},
				})
				if sockErr != nil {
					fmt.Printf("Warning: failed to contact daemon: %v\n", sockErr)
				} else if resp.Success {
					fmt.Printf("✓ Agent is now dormant (monitoring PR #%d)\n", pr.Number)
				}
				return nil
			}
		}

		// Check 2: agent already has a known PR that may be merged
		if stErr == nil {
			if agent, found := st.GetAgent(repoName, agentName); found && agent.PRNumber > 0 {
				prViewArgs := []string{"pr", "view", strconv.Itoa(agent.PRNumber), "--json", "state", "--jq", ".state"}
				if ghRepo != "" {
					prViewArgs = append(prViewArgs, "--repo", ghRepo)
				}
				prViewCtx, prViewCancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer prViewCancel()
				prViewOut, prViewErr := exec.CommandContext(prViewCtx, "gh", prViewArgs...).Output()
				if prViewErr == nil {
					prState := strings.TrimSpace(string(prViewOut))
					if prState == "MERGED" {
						fmt.Printf("PR #%d already merged — your work is complete!\n", agent.PRNumber)
						client := socket.NewClient(c.paths.DaemonSock)
						resp, sockErr := client.Send(socket.Request{
							Command: "agent_waiting",
							Args: map[string]interface{}{
								"repo":      repoName,
								"agent":     agentName,
								"pr_number": agent.PRNumber,
							},
						})
						if sockErr != nil {
							fmt.Printf("Warning: failed to contact daemon: %v\n", sockErr)
						} else if resp.Success {
							if data, ok := resp.Data.(map[string]interface{}); ok {
								if data["status"] == "auto_completed" {
									fmt.Println("✓ Worker auto-completed (PR already merged)")
								}
							}
						}
						return nil
					}
				}
			}
		}
	}

	// Build PR body with actual newlines: interpret literal \n sequences
	bodyText := strings.ReplaceAll(body, `\n`, "\n")

	if closesIssue != "" {
		if bodyText != "" {
			bodyText += "\n\n"
		}
		bodyText += fmt.Sprintf("Closes #%s", closesIssue)
	}

	// Append agent attribution
	bodyText += fmt.Sprintf("\n\nAgent: %s", agentName)

	// Write body to a temp file so gh pr create reads it correctly
	tmpFile, err := os.CreateTemp("", "oat-pr-body-*.md")
	if err != nil {
		return fmt.Errorf("failed to create temp file for PR body: %w", err)
	}
	defer os.Remove(tmpFile.Name())
	if _, err := tmpFile.WriteString(bodyText); err != nil {
		tmpFile.Close()
		return fmt.Errorf("failed to write PR body: %w", err)
	}
	tmpFile.Close()

	// Build gh pr create command with explicit --repo and --head when available
	ghArgs := []string{"pr", "create", "--title", title, "--body-file", tmpFile.Name(), "--label", "oat"}
	if ghRepo != "" && closesIssue != "" {
		if wave := detectWaveFromIssue(ghRepo, closesIssue); wave != "" {
			ghArgs = append(ghArgs, "--label", "wave:"+wave)
		}
	}
	if ghRepo != "" {
		ghArgs = append(ghArgs, "--repo", ghRepo)
	}
	if branch != "" {
		ghArgs = append(ghArgs, "--head", branch)
	}
	if isDraft {
		ghArgs = append(ghArgs, "--draft")
	}

	fmt.Println("Creating PR...")
	prCtx, prCancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer prCancel()
	cmd := exec.CommandContext(prCtx, "gh", ghArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh pr create failed: %w", err)
	}

	// Extract PR number from the created PR
	var prNumber int
	viewCtx, viewCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer viewCancel()
	prNumCmd := exec.CommandContext(viewCtx, "gh", "pr", "view", "--json", "number", "--jq", ".number")
	prNumOut, err := prNumCmd.Output()
	if err == nil {
		prNumStr := strings.TrimSpace(string(prNumOut))
		fmt.Printf("✓ PR #%s created\n", prNumStr)
		if n, parseErr := strconv.Atoi(prNumStr); parseErr == nil {
			prNumber = n
		}
	}

	// Auto-dormant: mark agent as waiting for PR resolution
	fmt.Printf("Marking agent '%s' as waiting for PR resolution...\n", agentName)
	waitingArgs := map[string]interface{}{
		"repo":  repoName,
		"agent": agentName,
	}
	if prNumber > 0 {
		waitingArgs["pr_number"] = prNumber
	}
	client := socket.NewClient(c.paths.DaemonSock)
	resp, err := client.Send(socket.Request{
		Command: "agent_waiting",
		Args:    waitingArgs,
	})
	if err != nil {
		fmt.Printf("Warning: failed to contact daemon for auto-dormant: %v\n", err)
	} else if !resp.Success {
		fmt.Printf("Warning: failed to set dormant: %s\n", resp.Error)
	} else {
		if data, ok := resp.Data.(map[string]interface{}); ok {
			switch data["status"] {
			case "auto_completed":
				fmt.Println("No PR found — worker has been auto-completed.")
				if msg, ok := data["message"].(string); ok {
					fmt.Println(msg)
				}
			case "dormant_verification":
				fmt.Println("✓ Agent is now dormant (waiting for verification verdict)")
				fmt.Println("\n══════════════════════════════════════════════════════════════")
				fmt.Println("DORMANT: Waiting for verification verdict.")
				fmt.Println("STOP. Do NOT generate any text, run any commands, or take")
				fmt.Println("any action until you see a new USER message from the daemon.")
				fmt.Println("Any \"[daemon] Status check\" messages below are STALE -- they")
				fmt.Println("were queued BEFORE you went dormant. Ignore them completely.")
				fmt.Println("Do NOT respond to them. Do NOT poll. Do NOT check status.")
				fmt.Println("STOP HERE. WAIT FOR THE DAEMON.")
				fmt.Println("══════════════════════════════════════════════════════════════")
			default:
				fmt.Println("✓ Agent is now dormant (waiting for PR)")
				fmt.Println("\n══════════════════════════════════════════════════════════════")
				fmt.Println("DORMANT: Waiting for PR resolution.")
				fmt.Println("STOP. Do NOT generate any text, run any commands, or take")
				fmt.Println("any action until you see a new USER message from the daemon.")
				fmt.Println("Any \"[daemon] Status check\" messages below are STALE -- they")
				fmt.Println("were queued BEFORE you went dormant. Ignore them completely.")
				fmt.Println("Do NOT respond to them. Do NOT poll. Do NOT check status.")
				fmt.Println("STOP HERE. WAIT FOR THE DAEMON.")
				fmt.Println("══════════════════════════════════════════════════════════════")
			}
		} else {
			fmt.Println("✓ Agent is now dormant (waiting for PR)")
			fmt.Println("\n══════════════════════════════════════════════════════════════")
			fmt.Println("DORMANT: Waiting for PR resolution.")
			fmt.Println("STOP. Do NOT generate any text, run any commands, or take")
			fmt.Println("any action until you see a new USER message from the daemon.")
			fmt.Println("Any \"[daemon] Status check\" messages below are STALE -- they")
			fmt.Println("were queued BEFORE you went dormant. Ignore them completely.")
			fmt.Println("Do NOT respond to them. Do NOT poll. Do NOT check status.")
			fmt.Println("STOP HERE. WAIT FOR THE DAEMON.")
			fmt.Println("══════════════════════════════════════════════════════════════")
		}
	}

	return nil
}

func (c *CLI) issueCreate(args []string) error {
	flags, _ := ParseFlags(args)

	title := flags["title"]
	if title == "" {
		return errors.InvalidUsage("--title is required: oat issue create --title <title> --body <body> [--label <label>]... [--wave <N>] [--blocker] [--file <path>]... [--expected <text>] [--actual <text>] [--spec-ref <text>]")
	}
	body := flags["body"]
	wave := flags["wave"]
	expectedBehavior := flags["expected"]
	actualBehavior := flags["actual"]
	specRef := flags["spec-ref"]
	_, isBlocker := flags["blocker"]

	// Collect repeatable flags (--label and --file) from raw args since ParseFlags overwrites
	var labels []string
	var files []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--label" && i+1 < len(args) {
			labels = append(labels, args[i+1])
			i++
		} else if args[i] == "--file" && i+1 < len(args) {
			files = append(files, args[i+1])
			i++
		}
	}

	repoName, agentName, err := c.inferAgentContext()
	if err != nil {
		return fmt.Errorf("failed to determine agent context: %w", err)
	}

	// Look up the GitHub repo URL from state
	var ghRepo string
	st, stErr := c.loadState()
	if stErr == nil {
		if repo, exists := st.GetRepo(repoName); exists {
			ghRepo = extractOwnerRepo(repo.GithubURL)
		}
	}

	// Wave auto-detection: if --wave not passed, infer from the worker's assigned issue
	if wave == "" && stErr == nil {
		if agent, exists := st.GetAgent(repoName, agentName); exists && agent.IssueNumber != "" {
			detectedWave := detectWaveFromIssue(ghRepo, agent.IssueNumber)
			if detectedWave != "" {
				wave = detectedWave
				fmt.Printf("Auto-detected wave label: wave:%s\n", wave)
			}
		}
	}

	// Determine wave label
	waveLabel := ""
	if wave != "" {
		waveLabel = "wave:" + wave
		labels = append(labels, waveLabel)
	}
	if isBlocker {
		labels = append(labels, "blocker")
	}
	labels = append(labels, "oat")

	// Build structured body
	var bodyBuilder strings.Builder
	if isBlocker {
		fmt.Fprintf(&bodyBuilder, "## Blocker: %s\n\n", title)
		bodyBuilder.WriteString("**Type**: Blocker\n")
		if wave != "" {
			fmt.Fprintf(&bodyBuilder, "**Wave**: %s\n", wave)
		}
	} else {
		fmt.Fprintf(&bodyBuilder, "## Fix: %s\n\n", title)
		bodyBuilder.WriteString("**Type**: Fix\n")
		if wave != "" {
			fmt.Fprintf(&bodyBuilder, "**Wave**: %s\n", wave)
		}
	}

	if len(files) > 0 {
		fmt.Fprintf(&bodyBuilder, "**File(s)**: %s\n", strings.Join(files, ", "))
	}
	bodyBuilder.WriteString("\n")

	// Problem section
	bodyBuilder.WriteString("### Problem\n")
	if body != "" {
		bodyBuilder.WriteString(strings.ReplaceAll(body, `\n`, "\n"))
	} else {
		bodyBuilder.WriteString("_No description provided_")
	}
	bodyBuilder.WriteString("\n\n")

	if isBlocker {
		bodyBuilder.WriteString("### Root Cause\n_To be determined_\n\n")
		bodyBuilder.WriteString("### Suggested Fix\n_To be determined_\n\n")
	} else {
		if expectedBehavior != "" {
			bodyBuilder.WriteString("### Expected Behavior\n")
			bodyBuilder.WriteString(strings.ReplaceAll(expectedBehavior, `\n`, "\n"))
			bodyBuilder.WriteString("\n\n")
		}
		if actualBehavior != "" {
			bodyBuilder.WriteString("### Actual Behavior\n")
			bodyBuilder.WriteString(strings.ReplaceAll(actualBehavior, `\n`, "\n"))
			bodyBuilder.WriteString("\n\n")
		}
	}

	if specRef != "" {
		bodyBuilder.WriteString("### Spec Reference\n")
		bodyBuilder.WriteString(strings.ReplaceAll(specRef, `\n`, "\n"))
		bodyBuilder.WriteString("\n\n")
	}

	// Guidance section
	bodyBuilder.WriteString("### Guidance\n")
	if isBlocker {
		bodyBuilder.WriteString("- DO: Check the operational spec to confirm expected behavior\n")
		bodyBuilder.WriteString("- DO: Verify the test setup matches the scenario being tested\n")
		bodyBuilder.WriteString("- DON'T: Weaken or remove tests to make them pass -- fix the code or the test setup\n")
	} else {
		bodyBuilder.WriteString("- DO: Reference the operational spec before changing behavior\n")
		bodyBuilder.WriteString("- DO: Run the blackbox test locally after fixing\n")
		bodyBuilder.WriteString("- DON'T: Weaken or remove tests to make them pass -- fix the code\n")
		bodyBuilder.WriteString("- DON'T: Modify the reference acceptance test\n")
	}
	bodyBuilder.WriteString("\n")

	if len(files) > 0 {
		bodyBuilder.WriteString("### Files to Touch\n")
		for _, f := range files {
			fmt.Fprintf(&bodyBuilder, "- `%s`\n", f)
		}
		bodyBuilder.WriteString("\n")
	}

	fmt.Fprintf(&bodyBuilder, "\n---\n_Created by agent: %s_\n", agentName)

	bodyText := bodyBuilder.String()

	// Write body to a temp file for gh issue create
	tmpFile, err := os.CreateTemp("", "oat-issue-body-*.md")
	if err != nil {
		return fmt.Errorf("failed to create temp file for issue body: %w", err)
	}
	defer os.Remove(tmpFile.Name())
	if _, err := tmpFile.WriteString(bodyText); err != nil {
		tmpFile.Close()
		return fmt.Errorf("failed to write issue body: %w", err)
	}
	tmpFile.Close()

	// Auto-create all labels on the GitHub repo (idempotent via --force)
	if ghRepo != "" {
		for _, l := range labels {
			labelArgs := []string{"label", "create", l, "--repo", ghRepo, "--color", "ededed", "--force"}
			labelCtx, labelCancel := context.WithTimeout(context.Background(), 15*time.Second)
			labelCmd := exec.CommandContext(labelCtx, "gh", labelArgs...)
			_, _ = labelCmd.CombinedOutput() // best-effort; --force makes this idempotent
			labelCancel()
		}
	}

	// Build gh issue create command
	ghArgs := []string{"issue", "create", "--title", title, "--body-file", tmpFile.Name()}
	if ghRepo != "" {
		ghArgs = append(ghArgs, "--repo", ghRepo)
	}
	for _, l := range labels {
		ghArgs = append(ghArgs, "--label", l)
	}

	fmt.Println("Creating issue...")
	issueCtx, issueCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer issueCancel()
	cmd := exec.CommandContext(issueCtx, "gh", ghArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", string(out))
		return fmt.Errorf("gh issue create failed: %w", err)
	}

	// Extract issue number from the output URL (gh prints URL like https://github.com/owner/repo/issues/42)
	issueURL := strings.TrimSpace(string(out))
	fmt.Printf("✓ Issue created: %s\n", issueURL)

	var issueNumber string
	if parts := strings.Split(issueURL, "/"); len(parts) > 0 {
		issueNumber = parts[len(parts)-1]
	}

	// If --blocker, notify workspace agent to spawn a worker
	if isBlocker && issueNumber != "" {
		msgBody := fmt.Sprintf("Blocker issue #%s created: %s. Please spawn a worker for it.", issueNumber, title)
		msgMgr := messages.NewManager(c.paths.MessagesDir)
		_, msgErr := msgMgr.Send(repoName, agentName, "workspace", msgBody)
		if msgErr != nil {
			fmt.Printf("Warning: failed to notify workspace about blocker: %v\n", msgErr)
		} else {
			fmt.Printf("✓ Workspace notified about blocker #%s\n", issueNumber)
			// Trigger immediate message routing
			client := socket.NewClient(c.paths.DaemonSock)
			_, _ = client.Send(socket.Request{Command: "route_messages"})
		}
	}

	// Print the issue number for scripting
	if issueNumber != "" {
		fmt.Printf("Issue number: %s\n", issueNumber)
	}

	return nil
}

// detectWaveFromIssue queries a GitHub issue's labels and returns the wave
// identifier (e.g. "0", "fix-1") if a wave:* label is found, or "" if none.
func detectWaveFromIssue(ghRepo, issueNumber string) string {
	if ghRepo == "" || issueNumber == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "issue", "view", issueNumber,
		"--repo", ghRepo, "--json", "labels", "--jq", ".labels[].name")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.HasPrefix(line, "wave:") {
			return strings.TrimPrefix(line, "wave:")
		}
	}
	return ""
}

func (c *CLI) restartAgentCmd(args []string) error {
	// Parse flags
	flags, remaining := ParseFlags(args)

	// Get agent name from args
	if len(remaining) < 1 {
		return errors.InvalidUsage("usage: oat agent restart <name> [--repo <repo>] [--force]")
	}
	agentName := remaining[0]

	// Get repo from flag or infer from cwd
	repoName := flags["repo"]
	if repoName == "" {
		// Try to infer from cwd
		inferred, err := c.inferRepoFromCwd()
		if err != nil {
			return errors.InvalidUsage("could not determine repository - use --repo flag or run from within a oat worktree")
		}
		repoName = inferred
	}

	force := flags["force"] == "true"

	fmt.Printf("Restarting agent '%s' in repository '%s'...\n", agentName, repoName)

	client := socket.NewClient(c.paths.DaemonSock)
	resp, err := client.Send(socket.Request{
		Command: "restart_agent",
		Args: map[string]interface{}{
			"repo":  repoName,
			"agent": agentName,
			"force": force,
		},
	})
	if err != nil {
		return errors.DaemonCommunicationFailed("restarting agent", err)
	}
	if !resp.Success {
		return errors.Wrap(errors.CategoryRuntime, "failed to restart agent", fmt.Errorf("%s", resp.Error))
	}

	// Extract PID from response
	if data, ok := resp.Data.(map[string]interface{}); ok {
		if pid, ok := data["pid"].(float64); ok {
			fmt.Printf("✓ Agent '%s' restarted successfully (PID: %d)\n", agentName, int(pid))
		} else {
			fmt.Printf("✓ Agent '%s' restarted successfully\n", agentName)
		}
	} else {
		fmt.Printf("✓ Agent '%s' restarted successfully\n", agentName)
	}

	return nil
}

func (c *CLI) reviewPR(args []string) error {
	if len(args) < 1 {
		return errors.InvalidUsage("usage: oat review <pr-url>")
	}

	prURL := args[0]

	// Parse PR URL to extract owner, repo, and PR number
	// Expected formats:
	// - https://github.com/owner/repo/pull/123
	// - github.com/owner/repo/pull/123
	prURL = strings.TrimPrefix(prURL, "https://")
	prURL = strings.TrimPrefix(prURL, "http://")
	parts := strings.Split(prURL, "/")

	if len(parts) < 5 || parts[3] != "pull" {
		return errors.InvalidPRURL()
	}

	prNumber := parts[4]
	fmt.Printf("Reviewing PR #%s\n", prNumber)

	// Determine repository from flag or current directory
	flags, _ := ParseFlags(args[1:])
	var repoName string
	if r, ok := flags["repo"]; ok {
		repoName = r
	} else {
		// Try to infer from current directory
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get current directory: %w", err)
		}

		// Check if we're in a tracked repo
		repos := c.getReposList()
		for _, repo := range repos {
			repoPath := c.paths.RepoDir(repo)
			if strings.HasPrefix(cwd, repoPath) {
				repoName = repo
				break
			}
		}

		if repoName == "" {
			return errors.NotInRepo()
		}
	}

	// Generate review agent name
	reviewerName := fmt.Sprintf("review-%s", prNumber)

	fmt.Printf("Creating review agent '%s' in repo '%s'\n", reviewerName, repoName)

	// Get repository path
	repoPath := c.paths.RepoDir(repoName)

	// Fetch the PR using GitHub's PR refs - this works for both same-repo and fork PRs
	// The refs/pull/<number>/head ref always exists and points to the PR's head commit
	fmt.Printf("Fetching PR #%s...\n", prNumber)
	prRef := fmt.Sprintf("refs/pull/%s/head", prNumber)
	localRef := fmt.Sprintf("refs/oat/pr-%s", prNumber)
	cmd := exec.CommandContext(c.cmdCtx(), "git", "fetch", "origin", fmt.Sprintf("%s:%s", prRef, localRef))
	cmd.Dir = repoPath
	if output, err := cmd.CombinedOutput(); err != nil {
		return errors.Wrap(errors.CategoryRuntime, fmt.Sprintf("failed to fetch PR #%s: %s", prNumber, strings.TrimSpace(string(output))), err).
			WithSuggestion("ensure the PR exists and you have access to the repository")
	}

	// Create worktree for review
	wt := worktree.NewManagerWithContext(c.cmdCtx(), repoPath)
	wtPath := c.paths.AgentWorktree(repoName, reviewerName)
	reviewBranch := fmt.Sprintf("review/%s", reviewerName)

	fmt.Printf("Creating worktree at: %s\n", wtPath)
	if err := wt.CreateNewBranch(wtPath, reviewBranch, localRef); err != nil {
		return fmt.Errorf("failed to create worktree: %w", err)
	}

	// Get session name
	sessionName := sanitizeSessionName(repoName)

	// Generate session ID for reviewer
	reviewerSessionID, err := agent_pkg.GenerateSessionID()
	if err != nil {
		return fmt.Errorf("failed to generate reviewer session ID: %w", err)
	}

	// Write prompt file for reviewer
	reviewerPromptFile, err := c.writePromptFile(repoPath, state.AgentTypeReview, reviewerName)
	if err != nil {
		return fmt.Errorf("failed to write reviewer prompt: %w", err)
	}

	// Copy hooks configuration if it exists
	if err := hooks.CopyConfig(repoPath, wtPath); err != nil {
		fmt.Printf("Warning: failed to copy hooks config: %v\n", err)
	}

	// Start agent in reviewer window with initial task (skip in test mode)
	var reviewerPID int
	if os.Getenv("OAT_TEST_MODE") != "1" {
		// Resolve agent binary
		agentBinary, err := c.getAgentBinary()
		if err != nil {
			return fmt.Errorf("failed to resolve agent binary: %w", err)
		}

		fmt.Println("Starting OAT - Open Agent Teams in reviewer window...")
		initialMessage := fmt.Sprintf("Review PR #%s: https://github.com/%s/%s/pull/%s", prNumber, parts[1], parts[2], prNumber)
		pid, err := c.startAgentViaBackend(agentBinary, sessionName, reviewerName, wtPath, reviewerSessionID, reviewerPromptFile, repoName, initialMessage, "")
		if err != nil {
			return fmt.Errorf("failed to start reviewer agent: %w", err)
		}
		reviewerPID = pid
	}

	// Register reviewer with daemon
	client := socket.NewClient(c.paths.DaemonSock)
	resp, err := client.Send(socket.Request{
		Command: "add_agent",
		Args: map[string]interface{}{
			"repo":          repoName,
			"agent":         reviewerName,
			"type":          "review",
			"worktree_path": wtPath,
			"window_name":   reviewerName,
			"task":          fmt.Sprintf("Review PR #%s", prNumber),
			"session_id":    reviewerSessionID,
			"pid":           reviewerPID,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to register reviewer: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("failed to register reviewer: %s", resp.Error)
	}

	fmt.Println()
	fmt.Println("✓ Review agent created successfully!")
	fmt.Printf("  Name: %s\n", reviewerName)
	fmt.Printf("  Branch: %s\n", reviewBranch)
	fmt.Printf("  Worktree: %s\n", wtPath)
	fmt.Printf("\nMonitor agents: oat ui\n")
	fmt.Printf("Or tail this reviewer: oat attach %s\n", reviewerName)

	return nil
}

// Logs command implementations

func (c *CLI) viewLogs(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: oat logs <agent> [--lines N] [--follow]")
	}

	agentName := args[0]
	flags, _ := ParseFlags(args[1:])

	// Determine repository
	repoName, err := c.resolveRepo(flags)
	if err != nil {
		return fmt.Errorf("could not determine repository: %w", err)
	}

	// Determine if it's a worker or system agent by checking if it exists in workers dir
	workerLogFile := c.paths.AgentLogFile(repoName, agentName, true)
	systemLogFile := c.paths.AgentLogFile(repoName, agentName, false)

	var logFile string
	if _, err := os.Stat(workerLogFile); err == nil {
		logFile = workerLogFile
	} else if _, err := os.Stat(systemLogFile); err == nil {
		logFile = systemLogFile
	} else {
		return fmt.Errorf("no log file found for agent %s in repo %s", agentName, repoName)
	}

	// Check for --follow flag
	if _, ok := flags["follow"]; ok {
		// Use tail -f
		cmd := exec.CommandContext(c.cmdCtx(), "tail", "-f", logFile)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	// Determine number of lines
	lines := "100"
	if l, ok := flags["lines"]; ok {
		lines = l
	}

	// Use tail to get recent lines
	cmd := exec.CommandContext(c.cmdCtx(), "tail", "-n", lines, logFile)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (c *CLI) listLogs(args []string) error {
	flags, _ := ParseFlags(args)

	// Determine repository
	var repoName string
	if r, ok := flags["repo"]; ok {
		repoName = r
	}

	if repoName != "" {
		// List logs for specific repo
		return c.listLogsForRepo(repoName)
	}

	// List logs for all repos
	repos := c.getReposList()
	if len(repos) == 0 {
		fmt.Println("No repositories tracked")
		return nil
	}

	for _, repo := range repos {
		if err := c.listLogsForRepo(repo); err != nil {
			fmt.Printf("Warning: failed to list logs for %s: %v\n", repo, err)
		}
	}
	return nil
}

func (c *CLI) listLogsForRepo(repoName string) error {
	repoOutputDir := c.paths.RepoOutputDir(repoName)

	// Check if directory exists
	if _, err := os.Stat(repoOutputDir); os.IsNotExist(err) {
		fmt.Printf("No logs for %s\n", repoName)
		return nil
	}

	fmt.Printf("\n%s:\n", repoName)

	// List system agent logs
	entries, err := os.ReadDir(repoOutputDir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() && entry.Name() == "workers" {
			continue
		}
		if strings.HasSuffix(entry.Name(), ".log") {
			info, _ := entry.Info()
			agentName := strings.TrimSuffix(entry.Name(), ".log")
			if info != nil {
				fmt.Printf("  %s (%d bytes)\n", agentName, info.Size())
			} else {
				fmt.Printf("  %s\n", agentName)
			}
		}
	}

	// List worker logs
	workersDir := c.paths.WorkersOutputDir(repoName)
	if _, err := os.Stat(workersDir); err == nil {
		workerEntries, err := os.ReadDir(workersDir)
		if err == nil && len(workerEntries) > 0 {
			fmt.Println("  workers/")
			for _, entry := range workerEntries {
				if strings.HasSuffix(entry.Name(), ".log") {
					info, _ := entry.Info()
					workerName := strings.TrimSuffix(entry.Name(), ".log")
					if info != nil {
						fmt.Printf("    %s (%d bytes)\n", workerName, info.Size())
					} else {
						fmt.Printf("    %s\n", workerName)
					}
				}
			}
		}
	}

	return nil
}

func (c *CLI) searchLogs(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: oat logs search <pattern> [--repo <repo>]")
	}

	pattern := args[0]
	flags, _ := ParseFlags(args[1:])

	// Determine repository
	var repoName string
	if r, ok := flags["repo"]; ok {
		repoName = r
	}

	// Get search directories
	var searchPaths []string
	if repoName != "" {
		repoOutputDir := c.paths.RepoOutputDir(repoName)
		if _, err := os.Stat(repoOutputDir); err == nil {
			searchPaths = append(searchPaths, repoOutputDir)
		}
	} else {
		// Search all repos
		repos := c.getReposList()
		for _, repo := range repos {
			repoOutputDir := c.paths.RepoOutputDir(repo)
			if _, err := os.Stat(repoOutputDir); err == nil {
				searchPaths = append(searchPaths, repoOutputDir)
			}
		}
	}

	if len(searchPaths) == 0 {
		fmt.Println("No log directories found")
		return nil
	}

	// Use grep to search recursively
	grepArgs := []string{"-r", "-n", "--include=*.log", pattern}
	grepArgs = append(grepArgs, searchPaths...)

	cmd := exec.CommandContext(c.cmdCtx(), "grep", grepArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Run grep (exit code 1 means no matches, which is fine)
	err := cmd.Run()
	// Direct type assertion is intentional: errors.As would unwrap past the
	// ExitError and silently return the underlying cause, whose ExitCode()
	// semantics differ. We specifically want grep's own exit code here.
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 { //nolint:errorlint // see comment above
		fmt.Println("No matches found")
		return nil
	}
	return err
}

func (c *CLI) cleanLogs(args []string) error {
	flags, _ := ParseFlags(args)

	olderThan, ok := flags["older-than"]
	if !ok {
		return fmt.Errorf("usage: oat logs clean --older-than <duration> (e.g., 7d, 24h)")
	}

	// Parse duration
	duration, err := parseDuration(olderThan)
	if err != nil {
		return fmt.Errorf("invalid duration: %w", err)
	}

	cutoff := time.Now().Add(-duration)
	fmt.Printf("Cleaning logs older than %s...\n", cutoff.Format(time.RFC3339))

	var deletedCount, deletedBytes int64

	// Walk output directory
	err = filepath.Walk(c.paths.OutputDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil //nolint:nilerr // per-entry walk errors are non-fatal
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".log") {
			return nil
		}
		if info.ModTime().Before(cutoff) {
			deletedBytes += info.Size()
			// TOCTOU (G122): this path came from a walk of the user's own ~/.oat
			// output directory, the cleanup is user-invoked, and the attacker
			// model requires the same user planting a symlink in their own home
			// dir. Not a reachable exploit surface.
			if err := os.Remove(path); err != nil { //nolint:gosec // G122 not reachable here
				fmt.Printf("Warning: failed to remove %s: %v\n", path, err)
			} else {
				deletedCount++
			}
		}
		return nil
	})

	if err != nil {
		return fmt.Errorf("failed to walk output directory: %w", err)
	}

	fmt.Printf("Deleted %d files (%.2f MB)\n", deletedCount, float64(deletedBytes)/(1024*1024))
	return nil
}

// parseDuration parses a duration string like "7d", "24h", "30m"
func parseDuration(s string) (time.Duration, error) {
	if len(s) < 2 {
		return 0, fmt.Errorf("duration too short")
	}

	unit := s[len(s)-1]
	valueStr := s[:len(s)-1]

	var value int
	if _, err := fmt.Sscanf(valueStr, "%d", &value); err != nil {
		return 0, err
	}

	switch unit {
	case 'd':
		return time.Duration(value) * 24 * time.Hour, nil
	case 'h':
		return time.Duration(value) * time.Hour, nil
	case 'm':
		return time.Duration(value) * time.Minute, nil
	default:
		return 0, fmt.Errorf("unknown unit: %c (use d, h, or m)", unit)
	}
}

func (c *CLI) attachAgent(args []string) error {
	flags, remainingArgs := ParseFlags(args)
	readOnly := flags["read-only"] == "true" || flags["r"] == "true"

	// Determine repository
	repoName, err := c.resolveRepo(flags)
	if err != nil {
		return errors.NotInRepo()
	}

	// Get agent info to find session and window
	client := socket.NewClient(c.paths.DaemonSock)
	resp, err := client.Send(socket.Request{
		Command: "list_agents",
		Args: map[string]interface{}{
			"repo": repoName,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to get agent info: %w (is daemon running?)", err)
	}
	if !resp.Success {
		return fmt.Errorf("failed to get agent info: %s", resp.Error)
	}

	agents, _ := resp.Data.([]interface{})

	// Determine agent name - from args or interactive selection
	var agentName string
	if len(remainingArgs) > 0 {
		agentName = remainingArgs[0]
	} else {
		// Interactive selection - all agent types
		items := agentsToSelectableItems(agents, nil)
		if len(items) == 0 {
			return errors.NoAgentsFound(repoName)
		}
		selected, err := SelectFromList("Select agent to attach:", items)
		if err != nil {
			return err
		}
		if selected == "" {
			fmt.Println("Canceled")
			return nil
		}
		agentName = selected
	}

	// Find agent
	var agentInfo map[string]interface{}
	for _, agent := range agents {
		if agentMap, ok := agent.(map[string]interface{}); ok {
			if name, _ := agentMap["name"].(string); name == agentName {
				agentInfo = agentMap
				break
			}
		}
	}

	if agentInfo == nil {
		return errors.AgentNotFound("agent", agentName, repoName)
	}

	// Attach via backend. Suppress stderr during the attempt so that the
	// backend's "can't find session" message doesn't leak when the session
	// lives in the daemon's memory rather than the CLI's.
	sessionName := sanitizeSessionName(repoName)
	windowName, _ := agentInfo["window_name"].(string)
	if windowName == "" {
		windowName = agentName
	}

	origStderr := os.Stderr
	if devNull, nullErr := os.Open(os.DevNull); nullErr == nil {
		os.Stderr = devNull
		defer func() {
			os.Stderr = origStderr
			devNull.Close()
		}()
	}

	err = c.backend.Attach(context.Background(), sessionName, windowName, readOnly)
	os.Stderr = origStderr
	if err == nil {
		return nil
	}

	// Fallback: for direct backend, the CLI process doesn't own the agent PTY
	// (the daemon does), so we tail the log file directly.
	agentType, _ := agentInfo["type"].(string)
	isWorker := agentType == "worker" || agentType == "review" || agentType == "verification"
	logFile := c.paths.AgentLogFile(repoName, agentName, isWorker)
	if _, statErr := os.Stat(logFile); statErr == nil {
		return c.tailFile(logFile)
	}
	logFile = c.paths.AgentLogFile(repoName, agentName, !isWorker)
	if _, statErr := os.Stat(logFile); statErr == nil {
		return c.tailFile(logFile)
	}

	return err
}

// tailFile follows a log file, displaying output in real time (like tail -f).
// Since the DirectBackend's log writer already strips ANSI escape codes and
// deduplicates TUI redraws, this simply streams the clean log to stdout.
// Press Ctrl-C to stop.
func (c *CLI) tailFile(path string) error {
	fmt.Printf("\033[1mFollowing agent output\033[0m  %s\n(Press Ctrl-C to stop)\n\n", path)

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}
	defer f.Close()

	// Show recent context — seek near the end so the user sees
	// what happened recently rather than the entire history.
	if info, statErr := f.Stat(); statErr == nil && info.Size() > 8192 {
		_, _ = f.Seek(info.Size()-8192, 0) // 0 = SeekStart; best-effort, fall back to full read
		// Skip the first (likely partial) line
		scanner := bufio.NewScanner(f)
		scanner.Scan()
	}

	buf := make([]byte, 4096)
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			os.Stdout.Write(buf[:n])
		}
		if readErr != nil {
			// At EOF — wait for more data (tail -f behavior)
			time.Sleep(250 * time.Millisecond)
		}
	}
}

func (c *CLI) cleanup(args []string) error {
	flags, _ := ParseFlags(args)
	dryRun := flags["dry-run"] == "true"
	verbose := flags["verbose"] == "true" || flags["v"] == "true"
	cleanMerged := flags["merged"] == "true"

	if dryRun {
		fmt.Println("Running cleanup in dry-run mode (no changes will be made)...")
	} else {
		fmt.Println("Running cleanup...")
	}

	// If --merged flag is set, run merged branch cleanup
	if cleanMerged {
		return c.cleanupMergedBranches(dryRun, verbose)
	}

	client := socket.NewClient(c.paths.DaemonSock)

	// Check if daemon is running
	_, err := client.Send(socket.Request{Command: "ping"})
	if err != nil {
		fmt.Println("Daemon is not running. Running local cleanup...")
		return c.localCleanup(dryRun, verbose)
	}

	// Trigger daemon cleanup
	resp, err := client.Send(socket.Request{
		Command: "trigger_cleanup",
		Args: map[string]interface{}{
			"dry_run": dryRun,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to trigger cleanup: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("cleanup failed: %s", resp.Error)
	}

	fmt.Println("Cleanup completed")
	return nil
}

// cleanupMergedBranches cleans up branches that have been merged upstream
func (c *CLI) cleanupMergedBranches(dryRun bool, verbose bool) error {
	fmt.Println("\nChecking for branches merged upstream...")

	// Load state to get repository list
	st, err := c.loadState()
	if err != nil {
		return err
	}

	totalDeleted := 0
	totalFound := 0

	// Process each repository
	repos := st.ListRepos()
	if len(repos) == 0 {
		fmt.Println("No repositories tracked. Nothing to clean up.")
		return nil
	}

	for _, repoName := range repos {
		repoPath := c.paths.RepoDir(repoName)

		// Check if repo exists
		if _, err := os.Stat(repoPath); os.IsNotExist(err) {
			if verbose {
				fmt.Printf("\nRepository %s: path does not exist, skipping\n", repoName)
			}
			continue
		}

		if verbose {
			fmt.Printf("\nRepository: %s\n", repoName)
		}

		wt := worktree.NewManagerWithContext(c.cmdCtx(), repoPath)

		// Check for merged branches with common prefixes
		for _, prefix := range []string{"oat/", "work/"} {
			mergedBranches, err := wt.FindMergedUpstreamBranches(prefix)
			if err != nil {
				if verbose {
					fmt.Printf("  Warning: failed to find merged branches with prefix %s: %v\n", prefix, err)
				}
				continue
			}

			if len(mergedBranches) == 0 {
				if verbose {
					fmt.Printf("  No merged branches with prefix %s\n", prefix)
				}
				continue
			}

			// Get worktrees to skip branches that are still checked out
			worktrees, err := wt.List()
			if err != nil {
				if verbose {
					fmt.Printf("  Warning: failed to list worktrees: %v\n", err)
				}
				continue
			}

			activeBranches := make(map[string]bool)
			for _, wtInfo := range worktrees {
				if wtInfo.Branch != "" {
					activeBranches[wtInfo.Branch] = true
				}
			}

			fmt.Printf("\nMerged branches with prefix %s for %s:\n", prefix, repoName)
			for _, branch := range mergedBranches {
				if activeBranches[branch] {
					if verbose {
						fmt.Printf("  Skipping %s (still checked out)\n", branch)
					}
					continue
				}

				totalFound++
				if dryRun {
					fmt.Printf("  Would delete: %s\n", branch)
				} else {
					// Delete local branch
					if err := wt.DeleteBranch(branch); err != nil {
						fmt.Printf("  Failed to delete %s: %v\n", branch, err)
						continue
					}
					fmt.Printf("  Deleted: %s\n", branch)
					totalDeleted++

					// Try to delete remote branch from origin (the fork)
					if err := wt.DeleteRemoteBranch("origin", branch); err != nil {
						if verbose {
							fmt.Printf("    (remote branch deletion failed: %v)\n", err)
						}
					} else if verbose {
						fmt.Printf("    (also deleted from origin)\n")
					}
				}
			}
		}
	}

	if dryRun {
		if totalFound > 0 {
			fmt.Printf("\nFound %d merged branch(es) that would be deleted\n", totalFound)
		} else {
			fmt.Println("\nNo merged branches found to clean up")
		}
	} else {
		if totalDeleted > 0 {
			fmt.Printf("\nDeleted %d merged branch(es)\n", totalDeleted)
		} else {
			fmt.Println("\nNo merged branches found to clean up")
		}
	}

	return nil
}

// cleanupOrphanedBranchesWithPrefix removes orphaned branches matching the given prefix
func (c *CLI) cleanupOrphanedBranchesWithPrefix(wt *worktree.Manager, branchPrefix, repoName string, dryRun, verbose bool) (removed int, issues int) {
	orphanedBranches, err := wt.FindOrphanedBranches(branchPrefix)
	if err != nil && verbose {
		fmt.Printf("  Warning: failed to find orphaned %s branches: %v\n", branchPrefix, err)
		return 0, 0
	}

	if len(orphanedBranches) == 0 {
		if verbose {
			branchType := "work"
			if branchPrefix == "workspace/" {
				branchType = "workspace"
			}
			fmt.Printf("  No orphaned %s branches\n", branchType)
		}
		return 0, 0
	}

	branchType := "work"
	if branchPrefix == "workspace/" {
		branchType = "workspace"
	}
	fmt.Printf("\nOrphaned %s branches (%d) for %s:\n", branchType, len(orphanedBranches), repoName)

	for _, branch := range orphanedBranches {
		if dryRun {
			fmt.Printf("  Would delete branch: %s\n", branch)
			issues++
		} else {
			if err := wt.DeleteBranch(branch); err != nil {
				fmt.Printf("  Failed to delete %s: %v\n", branch, err)
			} else {
				fmt.Printf("  Deleted branch: %s\n", branch)
				removed++
			}
		}
	}

	return removed, issues
}

func (c *CLI) localCleanup(dryRun bool, verbose bool) error {
	// Clean up orphaned worktrees, sessions, and other resources
	fmt.Println("\nChecking for orphaned resources...")

	totalRemoved := 0
	totalIssues := 0

	// Load state for reference
	st, err := state.Load(c.paths.StateFile)
	if err != nil {
		fmt.Printf("Warning: could not load state file: %v\n", err)
		st = state.New(c.paths.StateFile)
	}

	// Check for orphaned sessions (oat-* sessions not in state)
	sessions, err := c.backend.ListSessions(context.Background())
	if err == nil {
		repos := st.ListRepos()
		validSessions := make(map[string]bool)
		for _, repo := range repos {
			validSessions[fmt.Sprintf("oat-%s", repo)] = true
		}

		orphanedSessions := []string{}
		for _, session := range sessions {
			if strings.HasPrefix(session, "oat-") && !validSessions[session] {
				orphanedSessions = append(orphanedSessions, session)
			}
		}

		if len(orphanedSessions) > 0 {
			fmt.Printf("\nOrphaned sessions (%d):\n", len(orphanedSessions))
			for _, session := range orphanedSessions {
				if dryRun {
					fmt.Printf("  Would destroy: %s\n", session)
				} else {
					if err := c.backend.DestroySession(context.Background(), session); err != nil {
						fmt.Printf("  Failed to destroy %s: %v\n", session, err)
					} else {
						fmt.Printf("  Destroyed: %s\n", session)
						totalRemoved++
					}
				}
			}
		} else if verbose {
			fmt.Println("\nNo orphaned sessions found")
		}
	}

	// Check for orphaned worktree directories (in wts/ but not in any repo's git worktrees)
	entries, err := os.ReadDir(c.paths.WorktreesDir)
	if err != nil && !os.IsNotExist(err) {
		fmt.Printf("Warning: failed to read worktrees directory: %v\n", err)
	} else if err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}

			repoName := entry.Name()
			repoPath := c.paths.RepoDir(repoName)
			wtRootDir := c.paths.WorktreeDir(repoName)

			// Check if the repo still exists
			if _, err := os.Stat(repoPath); os.IsNotExist(err) {
				fmt.Printf("\nOrphaned worktree directory (repo missing): %s\n", wtRootDir)
				if !dryRun {
					if err := os.RemoveAll(wtRootDir); err != nil {
						fmt.Printf("  Failed to remove: %v\n", err)
					} else {
						fmt.Printf("  Removed\n")
						totalRemoved++
					}
				}
				continue
			}

			if verbose {
				fmt.Printf("\nRepository: %s\n", repoName)
			}

			wt := worktree.NewManagerWithContext(c.cmdCtx(), repoPath)

			// Cleanup orphaned worktree directories
			if !dryRun {
				removed, err := worktree.CleanupOrphaned(wtRootDir, wt)
				if err != nil {
					fmt.Printf("  Warning: failed to cleanup worktrees: %v\n", err)
				} else if len(removed) > 0 {
					for _, path := range removed {
						fmt.Printf("  Removed: %s\n", path)
					}
					totalRemoved += len(removed)
				} else if verbose {
					fmt.Println("  No orphaned worktrees")
				}
			} else {
				// Dry run: just check what would be removed
				gitWorktrees, _ := wt.List()
				gitPaths := make(map[string]bool)
				for _, gwt := range gitWorktrees {
					absPath, _ := filepath.Abs(gwt.Path)
					evalPath, err := filepath.EvalSymlinks(absPath)
					if err != nil {
						evalPath = absPath
					}
					gitPaths[evalPath] = true
				}

				dirEntries, _ := os.ReadDir(wtRootDir)
				for _, de := range dirEntries {
					if !de.IsDir() {
						continue
					}
					path := filepath.Join(wtRootDir, de.Name())
					absPath, _ := filepath.Abs(path)
					evalPath, err := filepath.EvalSymlinks(absPath)
					if err != nil {
						evalPath = absPath
					}
					if !gitPaths[evalPath] {
						fmt.Printf("  Would remove: %s\n", path)
						totalIssues++
					}
				}
			}

			// Prune git worktree references
			if !dryRun {
				if err := wt.Prune(); err != nil && verbose {
					fmt.Printf("  Warning: failed to prune worktrees: %v\n", err)
				}
			}

			// Clean up orphaned work/* and workspace/* branches
			removed, issues := c.cleanupOrphanedBranchesWithPrefix(wt, "work/", repoName, dryRun, verbose)
			totalRemoved += removed
			totalIssues += issues

			removed, issues = c.cleanupOrphanedBranchesWithPrefix(wt, "workspace/", repoName, dryRun, verbose)
			totalRemoved += removed
			totalIssues += issues
		}
	}

	// Check for orphaned message directories
	msgEntries, err := os.ReadDir(c.paths.MessagesDir)
	if err != nil && !os.IsNotExist(err) {
		fmt.Printf("Warning: failed to read messages directory: %v\n", err)
	} else if err == nil {
		for _, entry := range msgEntries {
			if !entry.IsDir() {
				continue
			}

			repoName := entry.Name()
			validAgents, _ := st.ListAgents(repoName)

			msgMgr := messages.NewManager(c.paths.MessagesDir)

			if !dryRun {
				count, err := msgMgr.CleanupOrphaned(repoName, validAgents)
				if err != nil && verbose {
					fmt.Printf("Warning: failed to cleanup messages for %s: %v\n", repoName, err)
				} else if count > 0 {
					fmt.Printf("Cleaned up %d orphaned message dir(s) for %s\n", count, repoName)
					totalRemoved += count
				}
			} else {
				// Dry run check
				repoDir := filepath.Join(c.paths.MessagesDir, repoName)
				agentEntries, _ := os.ReadDir(repoDir)
				validAgentMap := make(map[string]bool)
				for _, a := range validAgents {
					validAgentMap[a] = true
				}
				for _, ae := range agentEntries {
					if ae.IsDir() && !validAgentMap[ae.Name()] {
						fmt.Printf("Would remove orphaned message dir: %s/%s\n", repoName, ae.Name())
						totalIssues++
					}
				}
			}
		}
	}

	// Check for stale socket and PID files (when daemon not running)
	pidFile := daemon.NewPIDFile(c.paths.DaemonPID)
	if running, _, _ := pidFile.IsRunning(); !running {
		// Daemon not running, check for stale files
		if _, err := os.Stat(c.paths.DaemonPID); err == nil {
			if dryRun {
				fmt.Printf("\nWould remove stale PID file: %s\n", c.paths.DaemonPID)
				totalIssues++
			} else {
				if err := os.Remove(c.paths.DaemonPID); err == nil {
					fmt.Printf("Removed stale PID file: %s\n", c.paths.DaemonPID)
					totalRemoved++
				}
			}
		}
		if _, err := os.Stat(c.paths.DaemonSock); err == nil {
			if dryRun {
				fmt.Printf("Would remove stale socket file: %s\n", c.paths.DaemonSock)
				totalIssues++
			} else {
				if err := os.Remove(c.paths.DaemonSock); err == nil {
					fmt.Printf("Removed stale socket file: %s\n", c.paths.DaemonSock)
					totalRemoved++
				}
			}
		}
	}

	fmt.Println()
	if dryRun {
		if totalIssues > 0 {
			fmt.Printf("✓ Dry run completed: would fix %d issue(s)\n", totalIssues)
		} else {
			fmt.Println("✓ Dry run completed: no issues found")
		}
	} else {
		if totalRemoved > 0 {
			fmt.Printf("✓ Cleanup completed: removed %d item(s)\n", totalRemoved)
		} else {
			fmt.Println("✓ Cleanup completed: no orphaned resources found")
		}
	}

	return nil
}

func (c *CLI) repair(args []string) error {
	flags, _ := ParseFlags(args)
	verbose := flags["verbose"] == "true" || flags["v"] == "true"

	fmt.Println("Repairing state...")

	// Check if daemon is running
	client := socket.NewClient(c.paths.DaemonSock)
	_, err := client.Send(socket.Request{Command: "ping"})
	if err != nil {
		// Daemon not running - do local repair
		fmt.Println("Daemon is not running. Performing local repair...")
		return c.localRepair(verbose)
	}

	// Trigger state repair via daemon
	resp, err := client.Send(socket.Request{
		Command: "repair_state",
	})
	if err != nil {
		return fmt.Errorf("failed to trigger repair: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("repair failed: %s", resp.Error)
	}

	fmt.Println("✓ State repaired successfully")
	if data, ok := resp.Data.(map[string]interface{}); ok {
		if removed, ok := data["agents_removed"].(float64); ok && removed > 0 {
			fmt.Printf("  Removed %d dead agent(s)\n", int(removed))
		}
		if fixed, ok := data["issues_fixed"].(float64); ok && fixed > 0 {
			fmt.Printf("  Fixed %d issue(s)\n", int(fixed))
		}
	}

	return nil
}

// syncWorktrees triggers an immediate worktree sync for agents.
// Supports --branch to target a specific branch and --repo to target a specific repo.
func (c *CLI) syncWorktrees(args []string) error {
	flags, _ := ParseFlags(args)

	client := socket.NewClient(c.paths.DaemonSock)
	_, err := client.Send(socket.Request{Command: "ping"})
	if err != nil {
		return errors.DaemonNotRunning()
	}

	reqArgs := map[string]interface{}{}

	if branch, ok := flags["branch"]; ok && branch != "" {
		reqArgs["branch"] = branch
	}

	if repo, ok := flags["repo"]; ok && repo != "" {
		reqArgs["repo"] = repo
	}

	branchMsg := ""
	if b, ok := reqArgs["branch"]; ok {
		branchMsg = fmt.Sprintf(" (branch: %s)", b)
	}
	fmt.Printf("Syncing agent worktrees%s...\n", branchMsg)

	resp, err := client.Send(socket.Request{
		Command: "trigger_sync",
		Args:    reqArgs,
	})
	if err != nil {
		return fmt.Errorf("failed to trigger sync: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("sync failed: %s", resp.Error)
	}

	fmt.Println("✓ Sync triggered")
	fmt.Println("  Agent worktrees will be synced in the background.")

	return nil
}

// localRepair performs state repair without the daemon running
func (c *CLI) localRepair(verbose bool) error {
	// Load state from disk
	st, err := c.loadState()
	if err != nil {
		return err
	}

	agentsRemoved := 0
	issuesFixed := 0

	// Track orphaned sessions
	orphanedSessions := []string{}

	// Get all sessions and find orphaned ones
	sessions, err := c.backend.ListSessions(context.Background())
	if err == nil {
		repos := st.ListRepos()
		validSessions := make(map[string]bool)
		for _, repo := range repos {
			validSessions[fmt.Sprintf("oat-%s", repo)] = true
		}
		for _, session := range sessions {
			if strings.HasPrefix(session, "oat-") && !validSessions[session] {
				orphanedSessions = append(orphanedSessions, session)
			}
		}
	}

	// Check each repo and its agents
	repos := st.GetAllRepos()
	for repoName, repo := range repos {
		if verbose {
			fmt.Printf("\nChecking repository: %s\n", repoName)
		}

		// Check if session exists
		hasSession, err := c.backend.HasSession(context.Background(), repo.SessionName)
		if err != nil && verbose {
			fmt.Printf("  Warning: failed to check session %s: %v\n", repo.SessionName, err)
			continue
		}

		if !hasSession {
			if verbose {
				fmt.Printf("  Session %s not found\n", repo.SessionName)
			}
			// Remove all agents for this repo
			for agentName := range repo.Agents {
				if verbose {
					fmt.Printf("  Removing agent %s (session gone)\n", agentName)
				}
				if err := st.RemoveAgent(repoName, agentName); err == nil {
					agentsRemoved++
				}
			}
			issuesFixed++
			continue
		}

		// Check each agent
		for agentName, agent := range repo.Agents {
			// Check if agent is alive
			alive, _ := c.backend.IsAgentAlive(context.Background(), repo.SessionName, agent.WindowName)
			if !alive {
				if verbose {
					fmt.Printf("  Removing agent %s (agent %s not alive)\n", agentName, agent.WindowName)
				}
				if err := st.RemoveAgent(repoName, agentName); err == nil {
					agentsRemoved++
					issuesFixed++
				}
				continue
			}

			// Check if worktree exists (for workers)
			if agent.Type == state.AgentTypeWorker && agent.WorktreePath != "" {
				if _, err := os.Stat(agent.WorktreePath); os.IsNotExist(err) {
					if verbose {
						fmt.Printf("  Warning: worktree missing for %s: %s\n", agentName, agent.WorktreePath)
					}
				}
			}

			if verbose {
				fmt.Printf("  Agent %s: OK\n", agentName)
			}
		}
	}

	// Clean up orphaned worktrees
	for _, repoName := range st.ListRepos() {
		repoPath := c.paths.RepoDir(repoName)
		wtRootDir := c.paths.WorktreeDir(repoName)

		if _, err := os.Stat(wtRootDir); os.IsNotExist(err) {
			continue
		}

		wt := worktree.NewManagerWithContext(c.cmdCtx(), repoPath)
		removed, err := worktree.CleanupOrphaned(wtRootDir, wt)
		if err != nil {
			if verbose {
				fmt.Printf("  Warning: failed to cleanup worktrees for %s: %v\n", repoName, err)
			}
			continue
		}

		if len(removed) > 0 {
			if verbose {
				fmt.Printf("  Cleaned up %d orphaned worktree(s) for %s\n", len(removed), repoName)
			}
			issuesFixed += len(removed)
		}

		// Prune git worktree references
		if err := wt.Prune(); err != nil && verbose {
			fmt.Printf("  Warning: failed to prune worktrees for %s: %v\n", repoName, err)
		}
	}

	// Clean up orphaned message directories
	msgMgr := messages.NewManager(c.paths.MessagesDir)
	for _, repoName := range st.ListRepos() {
		validAgents, _ := st.ListAgents(repoName)
		if count, err := msgMgr.CleanupOrphaned(repoName, validAgents); err == nil && count > 0 {
			if verbose {
				fmt.Printf("  Cleaned up %d orphaned message dir(s) for %s\n", count, repoName)
			}
			issuesFixed += count
		}
	}

	// Report orphaned sessions
	if len(orphanedSessions) > 0 {
		fmt.Printf("\nFound %d orphaned session(s) not in state:\n", len(orphanedSessions))
		for _, session := range orphanedSessions {
			fmt.Printf("  - %s\n", session)
		}
		fmt.Println("To remove these, run: oat stop-all")
	}

	// Save updated state
	if err := st.Save(); err != nil {
		return fmt.Errorf("failed to save repaired state: %w", err)
	}

	fmt.Println("\n✓ Local repair completed")
	if agentsRemoved > 0 {
		fmt.Printf("  Removed %d dead agent(s)\n", agentsRemoved)
	}
	if issuesFixed > 0 {
		fmt.Printf("  Fixed %d issue(s)\n", issuesFixed)
	}
	if agentsRemoved == 0 && issuesFixed == 0 {
		fmt.Println("  No issues found")
	}

	return nil
}

// restartAgentInContext restarts the agent in the current context.
// Uses --resume to continue an existing session via OAT Agent Runtime's SQLite checkpointer.
func (c *CLI) restartAgentInContext(args []string) error {
	repoName, agentName, err := c.inferAgentContext()
	if err != nil {
		return fmt.Errorf("cannot determine agent context: %w\n\nRun this command from within an oat agent session window", err)
	}

	st, err := state.Load(c.paths.StateFile)
	if err != nil {
		return fmt.Errorf("failed to load state: %w", err)
	}

	agent, exists := st.GetAgent(repoName, agentName)
	if !exists {
		return fmt.Errorf("agent '%s' not found in state for repo '%s'", agentName, repoName)
	}

	if agent.SessionID == "" {
		return fmt.Errorf("agent has no session ID - try removing and recreating the agent")
	}

	// Write prompt to .oat/AGENTS.md in the worktree
	promptFile := filepath.Join(c.paths.Root, "prompts", agentName+".md")
	if _, err := os.Stat(promptFile); err == nil {
		promptContent, err := os.ReadFile(promptFile)
		if err == nil {
			agentsDir := filepath.Join(agent.WorktreePath, ".oat")
			if err := os.MkdirAll(agentsDir, 0o755); err != nil {
				fmt.Printf("Warning: failed to create %s: %v\n", agentsDir, err)
			} else if err := os.WriteFile(filepath.Join(agentsDir, "AGENTS.md"), promptContent, 0o644); err != nil {
				fmt.Printf("Warning: failed to write AGENTS.md: %v\n", err)
			}
		}
	}

	cmdArgs := []string{"--resume", agent.SessionID, "--auto-approve"}
	fmt.Printf("Resuming agent session %s...\n", agent.SessionID)

	agentPath, err := c.getAgentBinary()
	if err != nil {
		return fmt.Errorf("failed to find oat-agent binary: %w", err)
	}
	fmt.Printf("Running: %s %s\n\n", agentPath, strings.Join(cmdArgs, " "))

	cmd := exec.CommandContext(c.cmdCtx(), agentPath, cmdArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Dir = agent.WorktreePath

	return cmd.Run()
}

func (c *CLI) showDocs(args []string) error {
	fmt.Println(c.documentation)
	return nil
}

// humanOnlyCLICommands are top-level commands that agents should never see in
// their injected CLI reference. They are operator/developer commands: daemon
// lifecycle, kill-switches, repo setup, model onboarding, diagnostics, the
// docs self-dump, and the TUI. Trimming them cuts the reference roughly in
// half (~2k tokens) without touching any command an agent would invoke.
var humanOnlyCLICommands = map[string]bool{
	"start":       true,
	"stop":        true,
	"restart":     true,
	"status":      true, // daemon-status alias; slash-command "/status" is separate
	"daemon":      true,
	"stop-all":    true,
	"repo":        true,
	"init":        true, // alias of `repo init`
	"list":        true, // alias of `repo list`
	"model":       true,
	"bug":         true,
	"diagnostics": true,
	"version":     true,
	"docs":        true,
	"logs":        true,
	"ui":          true,
	"repair":      true,
	"agents":      true, // agent-definition CRUD, user-facing
	"cleanup":     true,
}

// agentRestrictedCLICommands lists top-level commands that are only relevant
// to specific agent types. Commands not in this map are shown to every agent
// type that passes the humanOnlyCLICommands filter.
//
// Example: only the supervisor spawns review agents (`oat review <url>`), so
// workers and merge-queue never need to see that reference.
var agentRestrictedCLICommands = map[string][]state.AgentType{
	"review": {state.AgentTypeSupervisor},
	"config": {state.AgentTypeSupervisor, state.AgentTypeMergeQueue},
	// "history" is supervisor-only in practice but cheap to leave in for
	// workspace too; keep unrestricted for now.
}

// commandVisibleForAgent reports whether a top-level CLI command named `name`
// should appear in the reference injected into an agent's system prompt. An
// empty agentType returns true for every non-human-only command (used by
// `oat docs` and tests that want the full filtered-for-agent-use reference).
func commandVisibleForAgent(name string, agentType state.AgentType) bool {
	if humanOnlyCLICommands[name] {
		return false
	}
	if agentType == "" {
		return true
	}
	allowed, restricted := agentRestrictedCLICommands[name]
	if !restricted {
		return true
	}
	for _, t := range allowed {
		if t == agentType {
			return true
		}
	}
	return false
}

// GenerateDocumentation generates the full markdown reference for every
// registered CLI command. Used by `oat docs` and tests; agent prompts should
// call GenerateDocumentationForAgent instead so human-only and
// agent-irrelevant commands get trimmed.
func (c *CLI) GenerateDocumentation() string {
	var sb strings.Builder

	sb.WriteString("# OAT CLI Reference\n\n")
	sb.WriteString("This is an automatically generated reference for all oat commands.\n\n")

	// Generate docs for each top-level command
	for name, cmd := range c.rootCmd.Subcommands {
		c.generateCommandDocs(&sb, name, cmd, 0)
	}

	return sb.String()
}

// GenerateDocumentationForAgent returns a CLI reference trimmed to the
// commands relevant to agentType. Pass an empty agentType to drop only
// human-only commands while keeping everything else.
func (c *CLI) GenerateDocumentationForAgent(agentType state.AgentType) string {
	var sb strings.Builder

	sb.WriteString("# OAT CLI Reference\n\n")
	sb.WriteString("This is an automatically generated reference for all oat commands.\n\n")

	for name, cmd := range c.rootCmd.Subcommands {
		if !commandVisibleForAgent(name, agentType) {
			continue
		}
		c.generateCommandDocs(&sb, name, cmd, 0)
	}

	return sb.String()
}

// docsFor returns the cached CLI reference for the given agent type, filling
// the cache on first access. Falls back to the pre-computed full
// documentation when the agent type is empty.
func (c *CLI) docsFor(agentType state.AgentType) string {
	if agentType == "" {
		return c.documentation
	}
	if cached, ok := c.docsByAgent[agentType]; ok {
		return cached
	}
	built := c.GenerateDocumentationForAgent(agentType)
	if c.docsByAgent == nil {
		c.docsByAgent = make(map[state.AgentType]string)
	}
	c.docsByAgent[agentType] = built
	return built
}

// generateCommandDocs recursively generates documentation for a command and its subcommands
func (c *CLI) generateCommandDocs(sb *strings.Builder, name string, cmd *Command, level int) {
	indent := strings.Repeat("#", level+2)

	// Command header
	fmt.Fprintf(sb, "%s %s\n\n", indent, name)

	// Description
	if cmd.Description != "" {
		fmt.Fprintf(sb, "%s\n\n", cmd.Description)
	}

	// Usage
	if cmd.Usage != "" {
		fmt.Fprintf(sb, "**Usage:** `%s`\n\n", cmd.Usage)
	}

	// Subcommands
	if len(cmd.Subcommands) > 0 {
		sb.WriteString("**Subcommands:**\n\n")
		for subName, subCmd := range cmd.Subcommands {
			// Skip internal commands
			if strings.HasPrefix(subName, "_") {
				continue
			}
			fmt.Fprintf(sb, "- `%s` - %s\n", subName, subCmd.Description)
		}
		sb.WriteString("\n")

		// Recursively document subcommands
		for subName, subCmd := range cmd.Subcommands {
			if !strings.HasPrefix(subName, "_") {
				c.generateCommandDocs(sb, subName, subCmd, level+1)
			}
		}
	}
}

// ParseFlags is a simple flag parser
func ParseFlags(args []string) (map[string]string, []string) {
	flags := make(map[string]string)
	var positional []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "--") {
			// Long flag
			flag := strings.TrimPrefix(arg, "--")
			// Handle --flag=value format
			if idx := strings.Index(flag, "="); idx != -1 {
				flags[flag[:idx]] = flag[idx+1:]
			} else if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				flags[flag] = args[i+1]
				i++
			} else {
				flags[flag] = "true"
			}
		} else if strings.HasPrefix(arg, "-") {
			// Short flag
			flag := strings.TrimPrefix(arg, "-")
			// Handle -f=value format
			if idx := strings.Index(flag, "="); idx != -1 {
				flags[flag[:idx]] = flag[idx+1:]
			} else if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				flags[flag] = args[i+1]
				i++
			} else {
				flags[flag] = "true"
			}
		} else {
			positional = append(positional, arg)
		}
	}

	return flags, positional
}

// savePromptToFile writes prompt text to the prompts directory and returns the path.
// This is a common helper used by various prompt-writing functions.
func (c *CLI) savePromptToFile(agentName, promptText string) (string, error) {
	promptDir := filepath.Join(c.paths.Root, "prompts")
	if err := os.MkdirAll(promptDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create prompt directory: %w", err)
	}

	promptPath := filepath.Join(promptDir, fmt.Sprintf("%s.md", agentName))
	if err := os.WriteFile(promptPath, []byte(promptText), 0644); err != nil {
		return "", fmt.Errorf("failed to write prompt file: %w", err)
	}

	return promptPath, nil
}

// getAgentDefinition finds an agent definition by name, copying templates if needed.
// Returns the prompt content or an error if not found.
func (c *CLI) getAgentDefinition(repoName, repoPath, agentDefName string) (string, error) {
	localAgentsDir := c.paths.RepoAgentsDir(repoName)
	reader := agents.NewReader(localAgentsDir, repoPath)
	definitions, err := reader.ReadAllDefinitions()
	if err != nil {
		return "", fmt.Errorf("failed to read agent definitions: %w", err)
	}

	// Find the definition
	for _, def := range definitions {
		if def.Name == agentDefName {
			return def.Content, nil
		}
	}

	// If not found, try to copy from templates and retry
	if _, err := os.Stat(localAgentsDir); os.IsNotExist(err) {
		if err := templates.CopyAgentTemplates(localAgentsDir); err != nil {
			return "", fmt.Errorf("failed to copy agent templates: %w", err)
		}
		// Re-read definitions
		definitions, err = reader.ReadAllDefinitions()
		if err != nil {
			return "", fmt.Errorf("failed to read agent definitions after template copy: %w", err)
		}
		for _, def := range definitions {
			if def.Name == agentDefName {
				return def.Content, nil
			}
		}
	}

	return "", fmt.Errorf("no %s agent definition found", agentDefName)
}

// appendDocsAndSlashCommands adds CLI documentation and slash commands to prompt text.
// agentType is the string form of state.AgentType (e.g. "worker", "merge-queue").
func (c *CLI) appendDocsAndSlashCommands(promptText string, agentType string) string {
	docs := c.docsFor(state.AgentType(agentType))
	if docs != "" {
		promptText += fmt.Sprintf("\n\n---\n\n%s", docs)
	}

	slashCommands := prompts.GetSlashCommandsPrompt(agentType)
	if slashCommands != "" {
		promptText += fmt.Sprintf("\n\n---\n\n%s", slashCommands)
	}

	return promptText
}

// writePromptFile writes the agent prompt to a temporary file and returns the path
func (c *CLI) writePromptFile(repoPath string, agentType state.AgentType, agentName string) (string, error) {
	// Get the complete prompt (default + custom + CLI docs). Pass the
	// agent-type-filtered doc view so a workspace or review agent doesn't
	// carry the full command tree in its system prompt.
	promptText, err := prompts.GetPrompt(repoPath, agentType, c.docsFor(agentType))
	if err != nil {
		return "", fmt.Errorf("failed to get prompt: %w", err)
	}

	return c.savePromptToFile(agentName, promptText)
}

// WorkerConfig holds configuration for creating worker prompts
type WorkerConfig struct {
	PushToBranch string           // Branch to push to instead of creating a new PR (for iterating on existing PRs)
	ForkConfig   state.ForkConfig // Fork configuration (if working in a fork)
	IssueNumber  string           // GitHub issue number for this task (optional, for issue-tied comments)
	IssueURL     string           // Optional issue URL
}

// writeWorkerPromptFile writes a worker prompt file with optional configuration.
// It reads the worker prompt from agent definitions (configurable agent system).
func (c *CLI) writeWorkerPromptFile(repoPath string, agentName string, config WorkerConfig) (string, error) {
	repoName := filepath.Base(repoPath)

	promptText, err := c.getAgentDefinition(repoName, repoPath, "worker")
	if err != nil {
		return "", err
	}

	// Add CLI documentation and slash commands
	promptText = c.appendDocsAndSlashCommands(promptText, string(state.AgentTypeWorker))

	// Prepend scratchpad path so workers know the concrete directory
	scratchpadDir := c.paths.ScratchpadDir(repoName)
	promptText = fmt.Sprintf("Shared scratchpad path: %s\n\n", scratchpadDir) + promptText

	// Prepend GitHub issue line first (before fork/push-to) when --issue was provided
	if config.IssueNumber != "" {
		issueLine := fmt.Sprintf("GitHub issue for this task: #%s (comment on it when you start and when you complete).\n", config.IssueNumber)
		if config.IssueURL != "" {
			issueLine += fmt.Sprintf("Issue URL: %s\n\n", config.IssueURL)
		} else {
			issueLine += "\n"
		}
		promptText = issueLine + promptText
	}

	// Add fork workflow context if working in a fork
	if config.ForkConfig.IsFork {
		// Get the fork owner from the GitHub URL
		forkOwner := c.extractOwnerFromGitHubURL(repoPath)
		forkWorkflow := prompts.GenerateForkWorkflowPrompt(
			config.ForkConfig.UpstreamOwner,
			config.ForkConfig.UpstreamRepo,
			forkOwner,
		)
		promptText = forkWorkflow + "\n---\n\n" + promptText
	}

	// Add push-to configuration if specified
	if config.PushToBranch != "" {
		pushToConfig := fmt.Sprintf(`## PR Iteration Mode

**IMPORTANT: You are iterating on an existing PR, not creating a new one.**

Instead of creating a new PR, push your changes to the existing branch: %s

When your work is ready:
1. Commit your changes
2. Push to origin: git push origin %s
3. Signal completion with: oat agent complete

Do NOT create a new PR. The existing PR will be updated automatically when you push.

---

`, config.PushToBranch, config.PushToBranch)
		promptText = pushToConfig + promptText
	}

	return c.savePromptToFile(agentName, promptText)
}

// startAgentViaBackend starts an agent using the ProcessBackend abstraction.
// Returns the PID of the agent process.
func (c *CLI) startAgentViaBackend(binaryPath, session, agentName, workDir, sessionID, promptFile, repoName string, initialMessage string, model string) (int, error) {
	// Determine log file path
	isWorker := true // default to worker-style log path
	logFile := c.paths.AgentLogFile(repoName, agentName, isWorker)

	// Ensure log directory exists
	logDir := filepath.Dir(logFile)
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return 0, fmt.Errorf("failed to create output directory: %w", err)
	}

	// Read prompt content for InitialPrompt
	var promptContent string
	if promptFile != "" {
		if data, err := os.ReadFile(promptFile); err == nil {
			promptContent = string(data)
		}
	}

	// Build TERM_PROGRAM env prefix
	envPrefix := "export TERM_PROGRAM=terminal; "
	if v := os.Getenv("TERM_PROGRAM"); v != "" {
		envPrefix = fmt.Sprintf("export TERM_PROGRAM=%s; ", v)
	}
	// Prevent git from opening an interactive editor (e.g. vi) during rebase,
	// which would permanently block the agent's terminal pane.
	envPrefix += "export GIT_EDITOR=true; export GIT_SEQUENCE_EDITOR=true; "
	// Ensure ~/.oat/bin/ has symlinks to the real binaries, then prepend it
	// to PATH (with guard to avoid accumulation on restarts).
	c.ensureBinSymlinks()
	envPrefix += pathPrependGuard(c.paths.BinDir)

	// Build args for oat-agent CLI
	args := []string{"--auto-approve"}
	if initialMessage != "" {
		args = append(args, "-m", initialMessage)
	}
	if model != "" {
		args = append(args, "-M", model)
	}
	args = append(args, "--model-params", `{"max_tokens":32000}`)

	cfg := backend_pkg.AgentConfig{
		SessionName:   session,
		AgentName:     agentName,
		WorkDir:       workDir,
		BinaryPath:    binaryPath,
		Args:          args,
		Env:           []string{fmt.Sprintf("OAT_AGENT_NAME=%s", agentName)},
		EnvPrefix:     envPrefix,
		InitialPrompt: promptContent,
		LogFile:       logFile,
	}

	handle, err := c.backend.StartAgent(context.Background(), cfg)
	if err != nil {
		return 0, fmt.Errorf("failed to start agent: %w", err)
	}

	pid := 0
	if handle != nil {
		pid = handle.PID
	}

	// Initial message is passed via -m flag to oat-agent, which auto-submits
	// it on startup. No need for SendMessage — the TUI handles it natively.

	return pid, nil
}

// quoteForShell returns s quoted for safe use in a shell command (handles spaces and single quotes).
func quoteForShell(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// pathPrependGuard returns a shell snippet that prepends dir to PATH only if
// it's not already present. Prevents PATH from growing on agent restarts.
func pathPrependGuard(dir string) string {
	q := quoteForShell(dir)
	return fmt.Sprintf(`case ":$PATH:" in *":%s:"*) ;; *) export PATH=%s:"$PATH" ;; esac; `, dir, q)
}

// ensureBinSymlinks creates/updates symlinks in ~/.oat/bin/ pointing to the
// actual oat and oat-agent binaries so agents get a neutral PATH entry.
func (c *CLI) ensureBinSymlinks() {
	bins := make(map[string]string)
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		bins["oat"] = exe
		agentBin := filepath.Join(filepath.Dir(exe), "oat-agent")
		if _, err := os.Stat(agentBin); err == nil {
			bins["oat-agent"] = agentBin
		}
	}
	if len(bins) > 0 {
		_ = c.paths.EnsureBinSymlinks(bins)
	}
}

// bugReport generates a diagnostic bug report with redacted sensitive information
func (c *CLI) bugReport(args []string) error {
	flags, positionalArgs := ParseFlags(args)

	// Check for verbose flag
	verbose := flags["verbose"] == "true" || flags["v"] == "true"

	// Get optional description from positional args
	description := ""
	if len(positionalArgs) > 0 {
		description = strings.Join(positionalArgs, " ")
	}

	// Create collector and generate report
	collector := bugreport.NewCollector(c.paths, Version)
	report, err := collector.Collect(description, verbose)
	if err != nil {
		return fmt.Errorf("failed to collect diagnostic information: %w", err)
	}

	// Format as Markdown
	markdown := bugreport.FormatMarkdown(report)

	// Check if output file specified
	if outputFile, ok := flags["output"]; ok {
		if err := os.WriteFile(outputFile, []byte(markdown), 0644); err != nil {
			return fmt.Errorf("failed to write report to %s: %w", outputFile, err)
		}
		fmt.Printf("Bug report written to: %s\n", outputFile)
		return nil
	}

	// Print to stdout
	fmt.Print(markdown)
	return nil
}

// diagnostics generates system diagnostics in machine-readable format
func (c *CLI) diagnostics(args []string) error {
	flags, _ := ParseFlags(args)

	// Create collector and generate report
	collector := diagnostics.NewCollector(c.paths, Version)
	report, err := collector.Collect()
	if err != nil {
		return fmt.Errorf("failed to collect diagnostics: %w", err)
	}

	// Always output as pretty JSON by default (unless --json=false for compact)
	prettyJSON := flags["json"] != "false"
	jsonOutput, err := report.ToJSON(prettyJSON)
	if err != nil {
		return fmt.Errorf("failed to format diagnostics as JSON: %w", err)
	}

	// Check if output file specified
	if outputFile, ok := flags["output"]; ok {
		if err := os.WriteFile(outputFile, []byte(jsonOutput), 0644); err != nil {
			return fmt.Errorf("failed to write diagnostics to %s: %w", outputFile, err)
		}
		fmt.Printf("Diagnostics written to: %s\n", outputFile)
		return nil
	}

	// Print to stdout
	fmt.Println(jsonOutput)
	return nil
}

// doctor runs preflight checks and prints either a human-readable summary
// or a JSON report. Exits with a non-zero status (via returned error) when
// any check fails, so CI and scripts can gate on `oat doctor`.
func (c *CLI) doctor(args []string) error {
	flags, _ := ParseFlags(args)
	results := preflight.Run(c.paths)

	// ParseFlags sets flags["json"] = "true" for bare --json as well as
	// --json=true. Anything else (including "false") renders the text form.
	if v, ok := flags["json"]; ok && v != "false" {
		data, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to format preflight results as JSON: %w", err)
		}
		fmt.Println(string(data))
	} else {
		printDoctorResults(os.Stdout, results)
	}

	_, _, fail := preflight.Summarize(results)
	if fail > 0 {
		return fmt.Errorf("%d preflight check(s) failed", fail)
	}
	return nil
}

// printDoctorResults renders a padded status table to w. Kept separate so
// tests can render into a bytes.Buffer and assert on the output.
func printDoctorResults(w io.Writer, results []preflight.CheckResult) {
	maxName := 0
	for _, r := range results {
		if len(r.Name) > maxName {
			maxName = len(r.Name)
		}
	}
	for _, r := range results {
		marker := "?"
		switch r.Status {
		case preflight.StatusOK:
			marker = "OK  "
		case preflight.StatusWarn:
			marker = "WARN"
		case preflight.StatusFail:
			marker = "FAIL"
		}
		fmt.Fprintf(w, "  %s  %-*s  %s\n", marker, maxName, r.Name, r.Message)
		if r.Hint != "" && r.Status != preflight.StatusOK {
			fmt.Fprintf(w, "        %s  hint: %s\n", strings.Repeat(" ", maxName), r.Hint)
		}
	}
	ok, warn, fail := preflight.Summarize(results)
	fmt.Fprintf(w, "\n%d OK, %d WARN, %d FAIL\n", ok, warn, fail)
}

// listBranchesWithPrefix returns all local branches with the given prefix
func (c *CLI) listBranchesWithPrefix(repoPath, prefix string) ([]string, error) {
	cmd := exec.CommandContext(c.cmdCtx(), "git", "branch", "--list", prefix+"*")
	cmd.Dir = repoPath
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var branches []string
	for _, line := range strings.Split(string(output), "\n") {
		branch := strings.TrimSpace(line)
		branch = strings.TrimPrefix(branch, "* ") // Remove current branch marker
		if branch != "" {
			branches = append(branches, branch)
		}
	}
	return branches, nil
}

// deleteBranch deletes a local git branch
func (c *CLI) deleteBranch(repoPath, branch string) error {
	cmd := exec.CommandContext(c.cmdCtx(), "git", "branch", "-D", branch)
	cmd.Dir = repoPath
	return cmd.Run()
}

// ---------------------------------------------------------------------------
// Model profile commands
// ---------------------------------------------------------------------------

func (c *CLI) modelOnboard(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: oat model onboard <provider:model> [--probe-set minimum|default] [--verbose]")
	}
	modelStr := args[0]
	flags, _ := ParseFlags(args[1:])

	probeSet := "default"
	if ps, ok := flags["probe-set"]; ok {
		probeSet = ps
	}
	_, verbose := flags["verbose"]

	// Find probe script
	probeScript := filepath.Join(c.findRepoRoot(), "benchmarks", "probe-model.py")
	if _, err := os.Stat(probeScript); err != nil {
		return fmt.Errorf("probe script not found at %s", probeScript)
	}

	// Always save — profiles go to ~/.oat/model-profiles/ for daemon routing
	// and also to model-routing/profiles/ in the source tree for version control.
	cmdArgs := []string{probeScript, modelStr, "--probe-set", probeSet, "--save"}

	cmd := exec.CommandContext(c.cmdCtx(), "python3", cmdArgs...)

	var stderrBuf strings.Builder
	if verbose {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	} else {
		cmd.Stdout = io.Discard
		cmd.Stderr = &onboardStderrFilter{buf: &stderrBuf}
	}

	if err := cmd.Run(); err != nil {
		if !verbose {
			// On failure, dump captured stderr so operators can diagnose
			os.Stderr.WriteString(stderrBuf.String())
		}
		return err
	}

	if !verbose {
		c.printOnboardSummary(modelStr, probeSet, stderrBuf.String())
	}

	// Copy the generated profile to ~/.oat/model-profiles/ for daemon access.
	// Backup existing profile if it would be overwritten.
	filename := strings.ReplaceAll(modelStr, ":", "__")
	filename = strings.ReplaceAll(filename, "/", "__") + ".yaml"
	srcProfile := filepath.Join(c.findRepoRoot(), "model-routing", "profiles", filename)
	if data, err := os.ReadFile(srcProfile); err == nil {
		home, _ := os.UserHomeDir()
		dstDir := filepath.Join(home, ".oat", "model-profiles")
		_ = os.MkdirAll(dstDir, 0755)
		dstProfile := filepath.Join(dstDir, filename)

		// Backup existing profile before overwrite
		if existing, readErr := os.ReadFile(dstProfile); readErr == nil {
			backupPath := dstProfile + ".bak"
			_ = os.WriteFile(backupPath, existing, 0644)
		}

		if err := os.WriteFile(dstProfile, data, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not copy profile to %s: %v\n", dstDir, err)
		} else if verbose {
			fmt.Printf("Profile saved to %s\n", dstProfile)
		}
	}

	// Tell daemon to reload profiles. If the daemon is offline this is a
	// hard error (PR #2 P1-B) so operators notice — previously it was a
	// silent warning that exited 0. The YAML files remain on disk and will
	// be picked up on the next daemon start.
	//
	// Exit-code mechanism: the CLI has no typed exit-code error today (Q12),
	// so we os.Exit(2) here with an explicit ERROR message. Main() maps all
	// returned errors to exit 1, which would lose the signal. No automated
	// test covers this branch (Q13/S3) — manual smoke only.
	//
	// TODO(oat): once a typed cliExitErr{code:int, err:error} exists, replace
	// os.Exit(2) with a return so the error formatter can decorate output.
	if _, err := c.sendDaemonRequest("reload_model_profiles", nil); err != nil {
		fmt.Fprintf(os.Stderr,
			"ERROR: profile written but daemon is not running.\n"+
				"       Start it with 'oat daemon start' for the profile to take effect.\n"+
				"       (underlying error: %v)\n", err)
		os.Exit(2)
	}
	if verbose {
		fmt.Println("Daemon model profiles reloaded")
	}

	return nil
}

// onboardStderrFilter captures all stderr output and shows progress lines
// (probe execution status) live. The full report section is suppressed in
// compact mode but available in the buffer for parsing.
type onboardStderrFilter struct {
	buf         *strings.Builder
	lineBuf     []byte
	showingLine bool
}

func (f *onboardStderrFilter) Write(p []byte) (n int, err error) {
	f.buf.Write(p)
	f.lineBuf = append(f.lineBuf, p...)

	for {
		idx := -1
		for i, b := range f.lineBuf {
			if b == '\n' {
				idx = i
				break
			}
		}
		if idx == -1 {
			// No newline — check for partial progress line
			line := string(f.lineBuf)
			if !f.showingLine && (strings.Contains(line, "Running ") || strings.HasPrefix(strings.TrimSpace(line), "Resolving model:") || strings.HasPrefix(strings.TrimSpace(line), "Retrying ")) {
				os.Stderr.WriteString(line)
				f.lineBuf = f.lineBuf[:0]
				f.showingLine = true
			}
			break
		}

		line := string(f.lineBuf[:idx+1])
		f.lineBuf = f.lineBuf[idx+1:]

		if f.showingLine {
			// Continuation of a progress line — print the result portion
			os.Stderr.WriteString(line)
			f.showingLine = false
		} else if strings.Contains(line, "Running ") ||
			strings.HasPrefix(strings.TrimSpace(line), "Resolving model:") ||
			strings.Contains(line, "Skipping remaining") ||
			strings.HasPrefix(strings.TrimSpace(line), "Retrying ") {
			os.Stderr.WriteString(line)
		}
	}
	return len(p), nil
}

func (c *CLI) printOnboardSummary(modelStr, probeSet, stderr string) {
	// Parse key fields from captured stderr. The Python script emits the YAML
	// profile to stderr after "Profile contents:". We extract fields with
	// simple string matching to avoid a YAML dependency.
	getField := func(key string) string {
		for _, line := range strings.Split(stderr, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, key+":") {
				return strings.TrimSpace(strings.TrimPrefix(trimmed, key+":"))
			}
		}
		return ""
	}

	score := getField("overall_score")
	workerEligible := getField("worker_eligible")
	orchestratorEligible := getField("orchestrator_eligible")
	probesRun := getField("probes_run")
	probesPassed := getField("probes_passed")

	if score == "" || probesRun == "" {
		// Parsing failed — fall back to full stderr output
		os.Stderr.WriteString(stderr)
		return
	}

	yesNo := func(val string) string {
		if val == "true" {
			return "yes"
		}
		return "no"
	}

	fmt.Printf("  ✓ %s/%s probes passed\n", probesPassed, probesRun)
	fmt.Printf("  Score: %s/100 | Worker: %s | Orchestrator: %s\n",
		score, yesNo(workerEligible), yesNo(orchestratorEligible))

	home, _ := os.UserHomeDir()
	filename := strings.ReplaceAll(modelStr, ":", "__")
	filename = strings.ReplaceAll(filename, "/", "__") + ".yaml"
	fmt.Printf("  Profile saved to %s\n", filepath.Join(home, ".oat", "model-profiles", filename))
	fmt.Printf("  (also written to model-routing/profiles/ in source tree)\n")

	// Print any error/warning lines that would otherwise be hidden
	for _, line := range strings.Split(stderr, "\n") {
		upper := strings.ToUpper(line)
		if strings.Contains(upper, "ERROR:") || strings.Contains(upper, "WARNING:") {
			fmt.Fprintln(os.Stderr, line)
		}
	}
}

func (c *CLI) modelList(args []string) error {
	profileDirs := c.modelProfileDirs()

	// Use directory origin as the availability signal.
	//
	// The daemon only loads profiles from ~/.oat/model-profiles/ — models
	// there have been explicitly onboarded on THIS machine (oat model onboard
	// actually tested them, so they definitely work). Profiles from the
	// bundled model-routing/profiles/ directory are reference templates for
	// other setups and have NOT been validated here.
	//
	// This avoids brittle env-var name guessing that breaks when users store
	// credentials differently (e.g. ANTHROPIC_API_KEY vs CLAUDE_API_KEY vs
	// set via a credentials helper).
	home, _ := os.UserHomeDir()
	userProfileDir := ""
	if home != "" {
		userProfileDir = filepath.Join(home, ".oat", "model-profiles")
	}

	// First pass: collect which models are in the user's onboarded dir.
	onboarded := make(map[string]bool)
	if userProfileDir != "" {
		if entries, err := os.ReadDir(userProfileDir); err == nil {
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".yaml") {
					data, err := os.ReadFile(filepath.Join(userProfileDir, e.Name()))
					if err == nil {
						fields := parseYAMLFlat(string(data))
						if id := fields["model_id"]; id != "" {
							onboarded[id] = true
						}
					}
				}
			}
		}
	}

	seen := make(map[string]bool)
	fmt.Printf("%-40s %-12s %-8s %-10s %-12s %-12s\n",
		"MODEL", "STATUS", "SCORE", "WORKER", "ORCHESTRATOR", "AVAILABLE")
	fmt.Println(strings.Repeat("─", 99))

	for _, profileDir := range profileDirs {
		entries, err := os.ReadDir(profileDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(profileDir, e.Name()))
			if err != nil {
				continue
			}
			fields := parseYAMLFlat(string(data))
			modelID := fields["model_id"]
			if seen[modelID] {
				continue
			}
			seen[modelID] = true
			orchEligible := fields["orchestrator_eligible"]
			if orchEligible == "" {
				orchEligible = fields["supervisor_eligible"]
			}

			// A model is available if it has been onboarded on this machine.
			// Onboarding runs actual capability probes, so if it succeeded the
			// credentials and endpoint are confirmed working.
			avail := "✓ onboarded"
			if !onboarded[modelID] {
				avail = "— not onboarded"
			}

			fmt.Printf("%-40s %-12s %-8s %-10s %-12s %-12s\n",
				modelID,
				fields["status"],
				fields["overall_score"],
				fields["worker_eligible"],
				orchEligible,
				avail,
			)
		}
	}

	if len(seen) == 0 {
		fmt.Println("No model profiles found.")
	}

	if len(onboarded) == 0 {
		fmt.Println("\nNo models onboarded yet. Run: oat model onboard <provider:model>")
		fmt.Println("Example: oat model onboard anthropic:claude-haiku-4-5")
	} else {
		fmt.Printf("\n%d model(s) onboarded on this machine (✓). Others require onboarding before use.\n", len(onboarded))
	}
	return nil
}

// modelProfileDirs returns profile directories to search (daemon dir first, then source tree).
func (c *CLI) modelProfileDirs() []string {
	var dirs []string
	home, _ := os.UserHomeDir()
	if home != "" {
		dirs = append(dirs, filepath.Join(home, ".oat", "model-profiles"))
	}
	dirs = append(dirs, filepath.Join(c.findRepoRoot(), "model-routing", "profiles"))
	return dirs
}

func (c *CLI) modelShow(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: oat model show <provider:model>")
	}
	modelStr := args[0]

	// Convert model string to filename: "anthropic:claude-sonnet-4-6" → "anthropic__claude-sonnet-4-6.yaml"
	filename := strings.ReplaceAll(modelStr, ":", "__")
	filename = strings.ReplaceAll(filename, "/", "__") + ".yaml"

	for _, dir := range c.modelProfileDirs() {
		profilePath := filepath.Join(dir, filename)
		data, err := os.ReadFile(profilePath)
		if err == nil {
			fmt.Println(string(data))
			return nil
		}
	}

	return fmt.Errorf("profile not found for %s\n  Run: oat model onboard %s", modelStr, modelStr)
}

func (c *CLI) modelRestore(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: oat model restore <provider:model>")
	}
	modelStr := args[0]

	filename := strings.ReplaceAll(modelStr, ":", "__")
	filename = strings.ReplaceAll(filename, "/", "__") + ".yaml"

	restored := false
	for _, dir := range c.modelProfileDirs() {
		backupPath := filepath.Join(dir, filename+".bak")
		profilePath := filepath.Join(dir, filename)
		if data, err := os.ReadFile(backupPath); err == nil {
			if err := os.WriteFile(profilePath, data, 0644); err != nil {
				fmt.Fprintf(os.Stderr, "Error restoring %s: %v\n", profilePath, err)
				continue
			}
			_ = os.Remove(backupPath)
			fmt.Printf("Restored %s from backup in %s\n", modelStr, dir)
			restored = true
		}
	}

	if !restored {
		return fmt.Errorf("no backup found for %s\n  Backups are created automatically when oat model onboard overwrites a profile", modelStr)
	}

	// Reload daemon profiles
	if _, err := c.sendDaemonRequest("reload_model_profiles", nil); err != nil {
		fmt.Fprintf(os.Stderr, "Note: daemon not running — profiles will be loaded on next start\n")
	} else {
		fmt.Println("Daemon model profiles reloaded")
	}

	return nil
}

// modelSet updates per-model runtime parameters in the profile YAML
// (and the daemon's working copy under ~/.oat/model-profiles), then asks
// the daemon to reload profiles. Intended as the operator-friendly
// alternative to hand-editing the YAML. The same fields can be edited
// directly in model-routing/profiles/<provider>__<model>.yaml — see
// CONTRIBUTING.md § "Tuning model runtime parameters".
func (c *CLI) modelSet(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: oat model set <provider:model> [--max-tokens N] [--nudge-interval SECONDS]")
	}
	modelStr := args[0]
	flags, _ := ParseFlags(args[1:])

	maxTokensStr, hasMaxTokens := flags["max-tokens"]
	nudgeStr, hasNudge := flags["nudge-interval"]
	if !hasMaxTokens && !hasNudge {
		return fmt.Errorf("oat model set: at least one of --max-tokens or --nudge-interval is required")
	}

	var maxTokens, nudgeSecs int
	if hasMaxTokens {
		n, err := strconv.Atoi(maxTokensStr)
		if err != nil || n <= 0 {
			return fmt.Errorf("--max-tokens must be a positive integer (got %q)", maxTokensStr)
		}
		maxTokens = n
	}
	if hasNudge {
		n, err := strconv.Atoi(nudgeStr)
		if err != nil || n <= 0 {
			return fmt.Errorf("--nudge-interval must be a positive integer seconds value (got %q)", nudgeStr)
		}
		nudgeSecs = n
	}

	filename := strings.ReplaceAll(modelStr, ":", "__")
	filename = strings.ReplaceAll(filename, "/", "__") + ".yaml"

	// Look up the profile — prefer the in-repo source of truth so edits stay
	// version-controlled, then fall back to ~/.oat/ for operators running
	// against a pre-built binary outside the repo.
	var profilePath string
	for _, dir := range c.modelProfileDirs() {
		candidate := filepath.Join(dir, filename)
		if _, err := os.Stat(candidate); err == nil {
			profilePath = candidate
			break
		}
	}
	if profilePath == "" {
		return fmt.Errorf("profile not found for %s\n  Run: oat model onboard %s", modelStr, modelStr)
	}

	data, err := os.ReadFile(profilePath)
	if err != nil {
		return fmt.Errorf("read %s: %w", profilePath, err)
	}

	updates := map[string]int{}
	if hasMaxTokens {
		updates["max_tokens"] = maxTokens
	}
	if hasNudge {
		updates["nudge_interval_seconds"] = nudgeSecs
	}
	updated := updateRuntimeBlock(string(data), updates)

	if err := os.WriteFile(profilePath, []byte(updated), 0644); err != nil {
		return fmt.Errorf("write %s: %w", profilePath, err)
	}
	fmt.Printf("Updated runtime parameters in %s\n", profilePath)

	// Mirror to ~/.oat/model-profiles/ so the running daemon sees the change
	// without needing the repo on its PATH.
	if home, _ := os.UserHomeDir(); home != "" {
		dst := filepath.Join(home, ".oat", "model-profiles", filename)
		if dst != profilePath {
			_ = os.MkdirAll(filepath.Dir(dst), 0755)
			if err := os.WriteFile(dst, []byte(updated), 0644); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not mirror profile to %s: %v\n", dst, err)
			}
		}
	}

	if _, err := c.sendDaemonRequest("reload_model_profiles", nil); err != nil {
		fmt.Fprintf(os.Stderr, "Note: daemon not running — new values take effect on next daemon start\n")
	} else {
		fmt.Println("Daemon model profiles reloaded")
	}
	return nil
}

// updateRuntimeBlock inserts or replaces entries under the top-level
// `runtime:` mapping in a flat profile YAML. It preserves surrounding
// content (comments, blank lines, other blocks) and only rewrites the
// specific keys supplied in updates.
//
// The format we emit matches the schema in model-routing/profiles/README.md:
//
//	runtime:
//	  max_tokens: 16000
//	  nudge_interval_seconds: 120
//
// If the `runtime:` block is missing, we append it (with a preceding blank
// line) so the resulting file reads cleanly next to hand-written profiles.
func updateRuntimeBlock(content string, updates map[string]int) string {
	if len(updates) == 0 {
		return content
	}
	lines := strings.Split(content, "\n")

	blockStart := -1
	blockEnd := -1 // exclusive; first line that leaves the block
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if blockStart == -1 {
			if trimmed == "runtime:" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
				blockStart = i
				blockEnd = len(lines)
				continue
			}
		} else {
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if !strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "\t") {
				blockEnd = i
				break
			}
		}
	}

	if blockStart == -1 {
		// Append a new runtime block. Strip trailing blank lines so we
		// don't stack them up on repeated edits.
		for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, "", "runtime:")
		for _, key := range runtimeBlockOrder(updates) {
			lines = append(lines, fmt.Sprintf("  %s: %d", key, updates[key]))
		}
		lines = append(lines, "")
		return strings.Join(lines, "\n")
	}

	// Replace or insert inside the existing block.
	remaining := make(map[string]int, len(updates))
	for k, v := range updates {
		remaining[k] = v
	}
	for i := blockStart + 1; i < blockEnd; i++ {
		trimmed := strings.TrimSpace(lines[i])
		parts := strings.SplitN(trimmed, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		if v, ok := remaining[key]; ok {
			lines[i] = fmt.Sprintf("  %s: %d", key, v)
			delete(remaining, key)
		}
	}
	if len(remaining) > 0 {
		insertion := make([]string, 0, len(remaining))
		for _, key := range runtimeBlockOrder(remaining) {
			insertion = append(insertion, fmt.Sprintf("  %s: %d", key, remaining[key]))
		}
		tail := append([]string{}, lines[blockEnd:]...)
		lines = append(lines[:blockEnd], insertion...)
		lines = append(lines, tail...)
	}
	return strings.Join(lines, "\n")
}

// runtimeBlockOrder returns updates' keys in a stable, human-friendly
// order (max_tokens before nudge_interval_seconds, then anything else
// alphabetically) so successive edits produce minimal diffs.
func runtimeBlockOrder(updates map[string]int) []string {
	preferred := []string{"max_tokens", "nudge_interval_seconds"}
	seen := make(map[string]bool, len(updates))
	order := make([]string, 0, len(updates))
	for _, k := range preferred {
		if _, ok := updates[k]; ok {
			order = append(order, k)
			seen[k] = true
		}
	}
	var extras []string
	for k := range updates {
		if !seen[k] {
			extras = append(extras, k)
		}
	}
	sort.Strings(extras)
	order = append(order, extras...)
	return order
}

// parseYAMLFlat does a simple key extraction from flat YAML (no nesting awareness).
// Sufficient for reading profile summary fields.
func parseYAMLFlat(content string) map[string]string {
	m := make(map[string]string)
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			val = strings.Trim(val, "\"")
			m[key] = val
		}
	}
	return m
}

// findRepoRoot returns the repo root by walking up from the executable or cwd.
func (c *CLI) findRepoRoot() string {
	// Try cwd first
	dir, _ := os.Getwd()
	for dir != "/" {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	return "."
}

// extractOwnerFromGitHubURL extracts the owner from a repository's origin URL.
// It first tries to get the origin URL from git remote, then parses it.
func (c *CLI) extractOwnerFromGitHubURL(repoPath string) string {
	cmd := exec.CommandContext(c.cmdCtx(), "git", "remote", "get-url", "origin")
	cmd.Dir = repoPath
	output, err := cmd.Output()
	if err != nil {
		return ""
	}

	originURL := strings.TrimSpace(string(output))
	owner, _, err := fork.ParseGitHubURL(originURL)
	if err != nil {
		return ""
	}
	return owner
}
