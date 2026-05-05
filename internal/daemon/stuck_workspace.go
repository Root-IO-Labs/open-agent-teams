// Workspace stuck detection.
//
// "Stuck" means the agent has been in a prolonged "Thinking..." state — the LLM
// is unresponsive or hanging on a request. It does NOT mean the agent is idle,
// waiting for user input, or between tasks. The signal is the agent's output log
// file (~/.oat/output/<repo>/<agent>.log) not being updated (no new PTY output).
//
// How detection works: the daemon periodically checks the output log's modification
// time. If the log hasn't been updated within the soft timeout, the daemon sends a
// Ctrl+C interrupt followed by a nudge message. If the log still hasn't been updated
// after the hard timeout, the daemon restarts the workspace agent.
//
// Workspace stuck detection is OFF by default because the workspace is user-driven —
// it naturally goes quiet when the user steps away, and that's not "stuck," just
// waiting for human input. Restarting it would destroy conversation context for no
// reason. Enable per-repo with: oat config --workspace-stuck-detection=true
//
// Benchmark runs enable it automatically because benchmarks are unattended — no human
// is at the keyboard to notice or intervene if the workspace gets stuck thinking, so
// stuck detection is the only safety net and a stuck workspace wastes the fixed time
// budget. Any similar unattended/autonomous workflow should enable it.
package daemon

import (
	"fmt"
	"os"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

// Workspace stuck detection thresholds (configurable via env vars).
var (
	workspaceSoftTimeout = time.Duration(getEnvInt("OAT_WORKSPACE_SOFT_TIMEOUT", 15)) * time.Minute
	workspaceHardTimeout = time.Duration(getEnvInt("OAT_WORKSPACE_HARD_TIMEOUT", 30)) * time.Minute
)

// nudgeGracePeriod is how long after sending a nudge we ignore output log
// updates, so the nudge's own PTY output doesn't reset the tracker.
const nudgeGracePeriod = 10 * time.Second

// workspaceActivity tracks output log modification time for stuck detection.
type workspaceActivity struct {
	lastModTime time.Time
	nudgedAt    time.Time // zero value means no nudge sent
}

// checkWorkspaceHealth checks if workspace agents are stuck by monitoring
// their output log file modification time. Only runs for repos that have
// workspace stuck detection enabled (off by default, enable per-repo with
// oat config --workspace-stuck-detection=true).
// Uses the backend-agnostic output log at ~/.oat/output/<repo>/<agent>.log
// instead of Claude-specific session files, so it works with any model backend.
func (d *Daemon) checkWorkspaceHealth() {
	d.workspaceActivityMu.Lock()
	defer d.workspaceActivityMu.Unlock()

	repos := d.state.GetAllRepos()
	for repoName, repo := range repos {
		if !repo.WorkspaceStuckDetection {
			continue
		}

		for agentName, agent := range repo.Agents {
			if agent.Type != state.AgentTypeWorkspace {
				continue
			}
			if agent.PID <= 0 || !isProcessAlive(agent.PID) {
				continue
			}

			logFile := d.paths.AgentLogFile(repoName, agentName, false)
			info, err := os.Stat(logFile)
			if err != nil {
				d.logger.Debug("Could not stat output log for workspace %s/%s: %v", repoName, agentName, err)
				continue
			}

			modTime := info.ModTime()
			staleDuration := time.Since(modTime)

			// d.workspaceActivity is owned by this loop; no mutex required.
			// See the field comment on Daemon before adding another accessor.
			activity, exists := d.workspaceActivity[repoName]
			if !exists {
				d.workspaceActivity[repoName] = &workspaceActivity{
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
			if staleDuration >= workspaceHardTimeout && !activity.nudgedAt.IsZero() {
				d.logger.Warn("Workspace %s/%s has been unresponsive for %v (hard timeout), restarting", repoName, agentName, staleDuration.Round(time.Second))
				if err := d.restartAgent(repoName, agentName, agent, repo); err != nil {
					d.logger.Error("Failed to restart stuck workspace %s/%s: %v", repoName, agentName, err)
				} else {
					d.logger.Info("Successfully restarted stuck workspace %s/%s", repoName, agentName)
				}
				activity.nudgedAt = time.Time{}
				activity.lastModTime = time.Now()
				continue
			}

			// Soft timeout: interrupt + nudge
			if staleDuration >= workspaceSoftTimeout && activity.nudgedAt.IsZero() {
				d.logger.Info("Workspace %s/%s has been unresponsive for %v (soft timeout), sending interrupt + nudge", repoName, agentName, staleDuration.Round(time.Second))

				if err := d.backend.SendInterrupt(d.ctx, repo.SessionName, agent.WindowName); err != nil {
					d.logger.Error("Failed to send Ctrl+C to workspace %s/%s: %v", repoName, agentName, err)
					continue
				}

				select {
				case <-time.After(2 * time.Second):
				case <-d.ctx.Done():
					return
				}

				nudgeMsg := fmt.Sprintf("[daemon] You have not produced any output for %d minutes. "+
					"This may be a false alarm if you are legitimately working on a complex task. "+
					"If you are stuck or encountered an error, please check and resume. "+
					"If you are still working, carry on.", int(staleDuration.Minutes()))

				if err := d.backend.SendMessage(d.ctx, repo.SessionName, agent.WindowName, nudgeMsg); err != nil {
					d.logger.Error("Failed to send nudge to workspace %s/%s: %v", repoName, agentName, err)
					continue
				}

				activity.nudgedAt = time.Now()
			}
		}
	}
}
