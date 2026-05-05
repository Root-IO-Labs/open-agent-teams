package bugreport

import (
	"fmt"
	"strings"
)

// FormatMarkdown formats the report as a Markdown document
func FormatMarkdown(report *Report) string {
	var sb strings.Builder

	// Title
	sb.WriteString("# OAT Bug Report\n\n")

	// Description (if provided)
	if report.Description != "" {
		sb.WriteString("## Description\n\n")
		sb.WriteString(report.Description)
		sb.WriteString("\n\n")
	}

	// Environment section
	sb.WriteString("## Environment\n\n")
	sb.WriteString("| Property | Value |\n")
	sb.WriteString("|----------|-------|\n")
	fmt.Fprintf(&sb, "| oat version | %s |\n", report.Version)
	fmt.Fprintf(&sb, "| Go version | %s |\n", report.GoVersion)
	fmt.Fprintf(&sb, "| OS | %s |\n", report.OS)
	fmt.Fprintf(&sb, "| Architecture | %s |\n", report.Arch)
	sb.WriteString("\n")

	// Tool versions section
	sb.WriteString("## Tool Versions\n\n")
	sb.WriteString("| Tool | Status |\n")
	sb.WriteString("|------|--------|\n")
	fmt.Fprintf(&sb, "| git | %s |\n", report.GitVersion)
	agentStatus := "not found"
	if report.AgentExists {
		agentStatus = "installed"
	}
	fmt.Fprintf(&sb, "| oat-agent CLI | %s |\n", agentStatus)
	sb.WriteString("\n")

	// Daemon status section
	sb.WriteString("## Daemon Status\n\n")
	if report.DaemonRunning {
		fmt.Fprintf(&sb, "- **Status**: Running (PID: %d)\n", report.DaemonPID)
	} else if report.DaemonPID > 0 {
		fmt.Fprintf(&sb, "- **Status**: Not running (stale PID: %d)\n", report.DaemonPID)
	} else {
		sb.WriteString("- **Status**: Not running\n")
	}
	sb.WriteString("\n")

	// Statistics section
	sb.WriteString("## Statistics\n\n")
	sb.WriteString("| Metric | Count |\n")
	sb.WriteString("|--------|-------|\n")
	fmt.Fprintf(&sb, "| Repositories | %d |\n", report.RepoCount)
	fmt.Fprintf(&sb, "| Workers | %d |\n", report.WorkerCount)
	fmt.Fprintf(&sb, "| Supervisors | %d |\n", report.SupervisorCount)
	fmt.Fprintf(&sb, "| Merge Queues | %d |\n", report.MergeQueueCount)
	fmt.Fprintf(&sb, "| Workspaces | %d |\n", report.WorkspaceCount)
	fmt.Fprintf(&sb, "| Review Agents | %d |\n", report.ReviewAgentCount)
	sb.WriteString("\n")

	// Verbose per-repo breakdown
	if report.Verbose && len(report.RepoStats) > 0 {
		sb.WriteString("### Per-Repository Breakdown\n\n")
		sb.WriteString("| Repository | Workers | Supervisor | Merge Queue | Workspaces |\n")
		sb.WriteString("|------------|---------|------------|-------------|------------|\n")
		for _, repo := range report.RepoStats {
			supervisor := "no"
			if repo.HasSupervisor {
				supervisor = "yes"
			}
			mergeQueue := "no"
			if repo.HasMergeQueue {
				mergeQueue = "yes"
			}
			fmt.Fprintf(&sb, "| %s | %d | %s | %s | %d |\n",
				repo.Name, repo.WorkerCount, supervisor, mergeQueue, repo.WorkspaceCount)
		}
		sb.WriteString("\n")
	}

	// Daemon log section
	sb.WriteString("## Daemon Log (last 50 lines, redacted)\n\n")
	sb.WriteString("```\n")
	sb.WriteString(report.DaemonLogTail)
	if !strings.HasSuffix(report.DaemonLogTail, "\n") {
		sb.WriteString("\n")
	}
	sb.WriteString("```\n")

	return sb.String()
}
