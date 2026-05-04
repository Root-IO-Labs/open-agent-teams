// Core agent (merge-queue, supervisor) stuck detection.
//
// "Stuck" means the agent has been in a prolonged "Thinking..." state — the LLM
// is unresponsive or hanging on a request. It does NOT mean the agent is idle or
// between tasks. The signal is the agent's output log file
// (~/.oat/output/<repo>/<agent>.log) not being updated (no new PTY output).
//
// How detection works: the daemon periodically checks the output log's modification
// time. If the log hasn't been updated within the soft timeout, the daemon sends an
// ESC key (to cancel extended thinking) followed by a nudge message containing any
// missed messages and fresh state. If the log still hasn't been updated after the
// hard timeout, the daemon restarts the agent.
//
// Core agent stuck detection is ALWAYS ON (skips idle repos) because the merge-queue
// and supervisor blocking directly impacts PR throughput for all users. Unlike the
// workspace (which is user-driven and naturally goes quiet), core agents should
// always be responsive when there is active work.
package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

// Core agent (merge-queue, supervisor) stuck detection thresholds.
// These are shorter than workspace timeouts because core agents perform
// quick operations (gh pr merge, status checks) and should rarely think
// for more than a few minutes.
var (
	coreAgentSoftTimeout = time.Duration(getEnvInt("OAT_CORE_AGENT_SOFT_TIMEOUT", 5)) * time.Minute
	coreAgentHardTimeout = time.Duration(getEnvInt("OAT_CORE_AGENT_HARD_TIMEOUT", 15)) * time.Minute
)

const maxMissedMessages = 5

// coreAgentActivity tracks output log modification time for stuck detection.
type coreAgentActivity struct {
	lastModTime time.Time
	nudgedAt    time.Time // zero value means no nudge sent
}

// recoverMissedMessages reads messages from the agent's message directory
// that were delivered after the output log went stale. These are messages
// that were written to the PTY while the agent was thinking and lost when
// ESC cleared the pending message queue.
func (d *Daemon) recoverMissedMessages(repoName, agentName string, stalePoint time.Time) string {
	msgDir := d.paths.AgentMessagesDir(repoName, agentName)

	entries, err := os.ReadDir(msgDir)
	if err != nil {
		return ""
	}

	type missedMsg struct {
		From      string    `json:"from"`
		Body      string    `json:"body"`
		Timestamp time.Time `json:"timestamp"`
		Status    string    `json:"status"`
	}

	var missed []missedMsg
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(msgDir, entry.Name()))
		if err != nil {
			continue
		}

		var msg missedMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		// Only include messages delivered after the stale point
		if msg.Timestamp.After(stalePoint) && (msg.Status == "delivered" || msg.Status == "read") {
			missed = append(missed, msg)
		}
	}

	if len(missed) == 0 {
		return ""
	}

	sort.Slice(missed, func(i, j int) bool {
		return missed[i].Timestamp.Before(missed[j].Timestamp)
	})

	var omitted int
	if len(missed) > maxMissedMessages {
		omitted = len(missed) - maxMissedMessages
		missed = missed[omitted:] // keep the most recent
	}

	var sb strings.Builder
	sb.WriteString("\nMissed messages while you were thinking:\n")
	if omitted > 0 {
		sb.WriteString(fmt.Sprintf("  (%d earlier messages omitted)\n", omitted))
	}
	for _, m := range missed {
		sb.WriteString(fmt.Sprintf("  [%s] %s\n", m.From, m.Body))
	}
	return sb.String()
}

// buildCoreAgentStateSummary builds a fresh state summary appropriate for
// the agent type, so the nudge message includes actionable current state.
func (d *Daemon) buildCoreAgentStateSummary(repoName string, agentType state.AgentType) string {
	switch agentType {
	case state.AgentTypeMergeQueue:
		repoPath := d.paths.RepoDir(repoName)
		ciSummary := d.buildMergeQueueCISummary(repoPath)
		return "\nCurrent status:\n" + ciSummary + "\nMerge any PRs that pass CI and have no merge conflicts."

	case state.AgentTypeSupervisor:
		repos := d.state.GetAllRepos()
		repo, exists := repos[repoName]
		if !exists {
			return ""
		}
		var workers []string
		for name, agent := range repo.Agents {
			if agent.Type == state.AgentTypeWorker {
				status := "active"
				if agent.WaitingForPR {
					status = "dormant"
				}
				if agent.ReadyForCleanup {
					status = "cleaning up"
				}
				workers = append(workers, fmt.Sprintf("  %s: %s", name, status))
			}
		}
		if len(workers) == 0 {
			return "\nCurrent status: No active workers."
		}
		sort.Strings(workers)
		return "\nCurrent status:\nWorkers:\n" + strings.Join(workers, "\n")

	default:
		return ""
	}
}

// checkCoreAgentHealth checks if merge-queue and supervisor agents are stuck
// by monitoring their output log file modification time. Uses SendEscape to
// cancel extended thinking, then delivers a nudge with missed messages and
// fresh state. Restarts the agent on hard timeout.
// Uses the backend-agnostic output log at ~/.oat/output/<repo>/<agent>.log
// instead of Claude-specific session files, so it works with any model backend.
func (d *Daemon) checkCoreAgentHealth() {
	d.coreAgentActivityMu.Lock()
	defer d.coreAgentActivityMu.Unlock()

	repos := d.state.GetAllRepos()
	for repoName, repo := range repos {
		if repo.IdleMode {
			continue
		}

		for agentName, agent := range repo.Agents {
			if agent.Type != state.AgentTypeMergeQueue && agent.Type != state.AgentTypeSupervisor {
				continue
			}
			if agent.PID <= 0 || !isProcessAlive(agent.PID) {
				continue
			}
			if agent.ReadyForCleanup {
				continue
			}

			logFile := d.paths.AgentLogFile(repoName, agentName, false)
			info, err := os.Stat(logFile)
			if err != nil {
				d.logger.Debug("Could not stat output log for %s %s/%s: %v", agent.Type, repoName, agentName, err)
				continue
			}

			modTime := info.ModTime()
			staleDuration := time.Since(modTime)

			key := repoName + "/" + agentName
			activity, exists := d.coreAgentActivity[key]
			if !exists {
				d.coreAgentActivity[key] = &coreAgentActivity{
					lastModTime: modTime,
				}
				continue
			}

			// Activity detected — but ignore log updates within the grace period
			// after a nudge, since those are likely the nudge message itself being
			// written to the PTY, not genuine agent activity.
			if modTime.After(activity.lastModTime) {
				if !activity.nudgedAt.IsZero() && time.Since(activity.nudgedAt) < nudgeGracePeriod {
					activity.lastModTime = modTime
					continue
				}
				activity.lastModTime = modTime
				activity.nudgedAt = time.Time{}
				continue
			}

			// Hard timeout: restart the agent (requires a prior soft nudge)
			if staleDuration >= coreAgentHardTimeout && !activity.nudgedAt.IsZero() {
				d.logger.Warn("%s %s/%s has been unresponsive for %v (hard timeout), restarting",
					agent.Type, repoName, agentName, staleDuration.Round(time.Second))
				if err := d.restartAgent(repoName, agentName, agent, repo); err != nil {
					d.logger.Error("Failed to restart stuck %s %s/%s: %v", agent.Type, repoName, agentName, err)
				} else {
					d.logger.Info("Successfully restarted stuck %s %s/%s", agent.Type, repoName, agentName)
				}
				activity.nudgedAt = time.Time{}
				activity.lastModTime = time.Now()
				continue
			}

			// Soft timeout: ESC + nudge with missed messages
			if staleDuration >= coreAgentSoftTimeout && activity.nudgedAt.IsZero() {
				d.logger.Info("%s %s/%s has been unresponsive for %v (soft timeout), sending ESC + nudge",
					agent.Type, repoName, agentName, staleDuration.Round(time.Second))

				if err := d.backend.SendEscape(d.ctx, repo.SessionName, agent.WindowName); err != nil {
					d.logger.Error("Failed to send ESC to %s %s/%s: %v", agent.Type, repoName, agentName, err)
					continue
				}

				select {
				case <-time.After(2 * time.Second):
				case <-d.ctx.Done():
					return
				}

				staleMinutes := int(staleDuration.Minutes())
				nudgeMsg := fmt.Sprintf("[daemon] You were stuck thinking for %d minutes and were automatically interrupted.", staleMinutes)

				missedMsgs := d.recoverMissedMessages(repoName, agentName, activity.lastModTime)
				if missedMsgs != "" {
					nudgeMsg += missedMsgs
				}

				stateSummary := d.buildCoreAgentStateSummary(repoName, agent.Type)
				if stateSummary != "" {
					nudgeMsg += stateSummary
				}

				if err := d.backend.SendMessage(d.ctx, repo.SessionName, agent.WindowName, nudgeMsg); err != nil {
					d.logger.Error("Failed to send nudge to %s %s/%s: %v", agent.Type, repoName, agentName, err)
					continue
				}

				activity.nudgedAt = time.Now()
			}
		}
	}
}
