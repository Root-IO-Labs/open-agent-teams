package diagnostics

import (
	"encoding/json"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
	"github.com/Root-IO-Labs/open-agent-teams/pkg/config"
)

// Report contains all diagnostic information in machine-readable format
type Report struct {
	// Version information
	Version      VersionInfo      `json:"version"`
	Environment  EnvironmentInfo  `json:"environment"`
	Capabilities CapabilitiesInfo `json:"capabilities"`
	Tools        ToolsInfo        `json:"tools"`
	Daemon       DaemonInfo       `json:"daemon"`
	Statistics   StatisticsInfo   `json:"statistics"`
}

// VersionInfo contains version details for oat and dependencies
type VersionInfo struct {
	OAT   string `json:"oat"`
	Go    string `json:"go"`
	IsDev bool   `json:"is_dev"`
}

// EnvironmentInfo contains environment variables and system information
type EnvironmentInfo struct {
	OS        string            `json:"os"`
	Arch      string            `json:"arch"`
	HomeDir   string            `json:"home_dir"`
	Paths     PathsInfo         `json:"paths"`
	Variables map[string]string `json:"variables"`
}

// PathsInfo contains oat directory paths
type PathsInfo struct {
	Root         string `json:"root"`
	StateFile    string `json:"state_file"`
	DaemonPID    string `json:"daemon_pid"`
	DaemonSock   string `json:"daemon_sock"`
	DaemonLog    string `json:"daemon_log"`
	ReposDir     string `json:"repos_dir"`
	WorktreesDir string `json:"worktrees_dir"`
	OutputDir    string `json:"output_dir"`
	MessagesDir  string `json:"messages_dir"`
}

// CapabilitiesInfo describes what features are available
type CapabilitiesInfo struct {
	TaskManagement bool `json:"task_management"`
	AgentInstalled bool `json:"oat_agent_installed"`
	GitInstalled   bool `json:"git_installed"`
}

// ToolsInfo contains version information for external tools
type ToolsInfo struct {
	Agent AgentInfo `json:"oat_agent"`
	Git   string    `json:"git"`
}

// AgentInfo contains detailed information about the Agent CLI
type AgentInfo struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version"`
	Path      string `json:"path"`
}

// DaemonInfo contains information about the daemon process
type DaemonInfo struct {
	Running bool `json:"running"`
	PID     int  `json:"pid"`
}

// StatisticsInfo contains agent and repository counts
type StatisticsInfo struct {
	Repositories  int `json:"repositories"`
	Workers       int `json:"workers"`
	Supervisors   int `json:"supervisors"`
	MergeQueues   int `json:"merge_queues"`
	Workspaces    int `json:"workspaces"`
	ReviewAgents  int `json:"review_agents"`
	AgentBuilders int `json:"agent_builders"`
}

// Collector gathers diagnostic information
type Collector struct {
	paths   *config.Paths
	version string
}

// NewCollector creates a new diagnostic collector
func NewCollector(paths *config.Paths, version string) *Collector {
	return &Collector{
		paths:   paths,
		version: version,
	}
}

// Collect gathers all diagnostic information
func (c *Collector) Collect() (*Report, error) {
	report := &Report{
		Version: VersionInfo{
			OAT:   c.version,
			Go:    runtime.Version(),
			IsDev: strings.Contains(c.version, "dev") || strings.Contains(c.version, "unknown"),
		},
		Environment: c.collectEnvironment(),
		Tools:       c.collectTools(),
		Daemon:      c.collectDaemon(),
		Statistics:  c.collectStatistics(),
	}

	// Determine capabilities based on tool versions
	report.Capabilities = c.determineCapabilities(report.Tools)

	return report, nil
}

// collectEnvironment gathers environment information
func (c *Collector) collectEnvironment() EnvironmentInfo {
	homeDir, _ := os.UserHomeDir()

	// Collect important environment variables
	envVars := make(map[string]string)
	importantVars := []string{
		"OAT_TEST_MODE",
		"PATH",
		"SHELL",
		"TERM",
		"OAT_BACKEND",
	}

	for _, varName := range importantVars {
		if value := os.Getenv(varName); value != "" {
			// Redact sensitive values
			if strings.Contains(strings.ToLower(varName), "token") ||
				strings.Contains(strings.ToLower(varName), "key") {
				envVars[varName] = "[REDACTED]"
			} else {
				envVars[varName] = value
			}
		}
	}

	return EnvironmentInfo{
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
		HomeDir: homeDir,
		Paths: PathsInfo{
			Root:         c.paths.Root,
			StateFile:    c.paths.StateFile,
			DaemonPID:    c.paths.DaemonPID,
			DaemonSock:   c.paths.DaemonSock,
			DaemonLog:    c.paths.DaemonLog,
			ReposDir:     c.paths.ReposDir,
			WorktreesDir: c.paths.WorktreesDir,
			OutputDir:    c.paths.OutputDir,
			MessagesDir:  c.paths.MessagesDir,
		},
		Variables: envVars,
	}
}

// collectTools gathers information about external tools
func (c *Collector) collectTools() ToolsInfo {
	return ToolsInfo{
		Agent: c.getAgentInfo(),
		Git:   c.getToolVersion("git", "--version"),
	}
}

// getAgentInfo returns detailed information about Agent CLI
func (c *Collector) getAgentInfo() AgentInfo {
	path, err := exec.LookPath("oat-agent")
	if err != nil {
		return AgentInfo{
			Installed: false,
		}
	}

	cmd := exec.Command("oat-agent", "--version")
	output, err := cmd.Output()
	if err != nil {
		return AgentInfo{
			Installed: true,
			Path:      path,
			Version:   "unknown",
		}
	}

	version := strings.TrimSpace(string(output))
	return AgentInfo{
		Installed: true,
		Path:      path,
		Version:   version,
	}
}

// getToolVersion returns the version string for a tool
func (c *Collector) getToolVersion(tool string, versionFlag string) string {
	cmd := exec.Command(tool, versionFlag)
	output, err := cmd.Output()
	if err != nil {
		return "not installed"
	}
	return strings.TrimSpace(string(output))
}

// determineCapabilities determines what features are available
func (c *Collector) determineCapabilities(tools ToolsInfo) CapabilitiesInfo {
	capabilities := CapabilitiesInfo{
		AgentInstalled: tools.Agent.Installed,
		GitInstalled:   tools.Git != "not installed",
	}

	// Task management is available in Agent Code 2.0+
	if tools.Agent.Installed && tools.Agent.Version != "unknown" {
		capabilities.TaskManagement = c.detectTaskManagementSupport(tools.Agent.Version)
	}

	return capabilities
}

// detectTaskManagementSupport checks if the Agent version supports task management
func (c *Collector) detectTaskManagementSupport(version string) bool {
	// Task management (TaskCreate/Update/List/Get) was introduced in Agent Code 2.0
	// Version format: "X.Y.Z (Agent Code)" or just "X.Y.Z"

	// Extract version number from string like "2.1.17 (Agent Code)"
	parts := strings.Fields(version)
	if len(parts) == 0 {
		return false
	}

	versionNum := parts[0]
	versionParts := strings.Split(versionNum, ".")
	if len(versionParts) < 2 {
		return false
	}

	major, err := strconv.Atoi(versionParts[0])
	if err != nil {
		return false
	}

	// Task management available in v2.0+
	return major >= 2
}

// collectDaemon gathers daemon status information
func (c *Collector) collectDaemon() DaemonInfo {
	pidData, err := os.ReadFile(c.paths.DaemonPID)
	if err != nil {
		return DaemonInfo{
			Running: false,
			PID:     0,
		}
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		return DaemonInfo{
			Running: false,
			PID:     0,
		}
	}

	// Check if process is running
	process, err := os.FindProcess(pid)
	if err != nil {
		return DaemonInfo{
			Running: false,
			PID:     pid,
		}
	}

	// On Unix, FindProcess always succeeds, so we send signal 0 to check
	err = process.Signal(os.Signal(nil))
	if err != nil {
		return DaemonInfo{
			Running: false,
			PID:     pid,
		}
	}

	return DaemonInfo{
		Running: true,
		PID:     pid,
	}
}

// collectStatistics gathers agent and repository statistics
func (c *Collector) collectStatistics() StatisticsInfo {
	st, err := state.Load(c.paths.StateFile)
	if err != nil {
		return StatisticsInfo{}
	}

	stats := StatisticsInfo{}
	repos := st.GetAllRepos()
	stats.Repositories = len(repos)

	for _, repo := range repos {
		for _, agent := range repo.Agents {
			switch agent.Type {
			case state.AgentTypeWorker:
				stats.Workers++
			case state.AgentTypeSupervisor:
				stats.Supervisors++
			case state.AgentTypeMergeQueue:
				stats.MergeQueues++
			case state.AgentTypeWorkspace:
				stats.Workspaces++
			case state.AgentTypeReview:
				stats.ReviewAgents++
			case state.AgentTypeAgentBuilder:
				stats.AgentBuilders++
			}
		}
	}

	return stats
}

// ToJSON converts the report to JSON format
func (r *Report) ToJSON(pretty bool) (string, error) {
	var data []byte
	var err error

	if pretty {
		data, err = json.MarshalIndent(r, "", "  ")
	} else {
		data, err = json.Marshal(r)
	}

	if err != nil {
		return "", err
	}

	return string(data), nil
}
