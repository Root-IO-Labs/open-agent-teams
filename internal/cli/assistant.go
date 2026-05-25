// Package cli — assistant.go owns the Part 5d `oat assistant` verb
// tree introduced by the side-panel-chat-and-status plan. An
// AgentTypeAssistant (5a) is a persistent personal-assistant agent
// that lives in a virtual repo (5c) and chats with the user through
// the side panel.
//
// Verb table (one entry per Subcommand):
//
//	start [name] [--model id] [--open-panel]   Create + spawn
//	stop [name]                                Gracefully stop
//	restart [name] [--fresh]                   Bounce; --fresh wipes JSONL
//	status [name]                              Bridge / PID / model / capacity
//	attach [name]                              Alias for `oat ui --repo`
//	set-model <id> [name]                      Update model preference
//	reset [name] [--full]                      Wipe session JSONL
//	compact [name]                             Synthetic compact_conversation
//	logs [name] [--follow]                     Tail agent log
//	list                                       Filter view of `oat agent list`
//
// Lifecycle model:
//   - The virtual repo (`_assistant-<name>`) persists across stop/start
//     cycles. The state.Agent record does NOT — `stop` removes it
//     (mirrors the existing `remove_agent` semantics). To preserve
//     model preference across stop+start, the assistant's chosen
//     model is mirrored to state.Repository.Model on `set-model`, so
//     the next `start` re-reads it.
//   - Auto-restart: AgentTypeAssistant is in IsPersistent() (Part 5a),
//     so a CRASHED assistant gets auto-respawned by the daemon's
//     health-check loop. A MANUALLY stopped one (no state.Agent
//     record) does not, because there's nothing for the loop to
//     iterate over. That's the right user-visible semantic: crashes
//     should self-heal, but `oat assistant stop` should mean stop.

package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/daemon"
	"github.com/Root-IO-Labs/open-agent-teams/internal/errors"
	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

// defaultAssistantName is the name selected when the user runs an
// `oat assistant` verb with no positional name. Single value
// (instead of inferring from cwd like worker verbs do) because the
// assistant is a user-scoped artifact, not a repo-scoped one.
const defaultAssistantName = "personal"

// registerAssistantCommands wires the entire `oat assistant` verb
// tree under the root command. Called from registerCommands so the
// dispatch is identical to the other top-level verbs.
func (c *CLI) registerAssistantCommands() {
	assistantCmd := &Command{
		Name:        "assistant",
		Description: "Manage personal AI assistants (persistent agents in virtual repos)",
		Subcommands: make(map[string]*Command),
		Usage: "oat assistant <subcommand> [name] [flags]\n\n" +
			"Personal AI assistants are persistent agents (AgentTypeAssistant)\n" +
			"that live in virtual repos (`_assistant-<name>`). They chat with\n" +
			"you through the side panel of the oat-browser-agent Chrome\n" +
			"extension. Unlike browser-agents, they do not complete after a\n" +
			"single task; they stay alive until you `oat assistant stop` them.\n\n" +
			"Subcommands: start | stop | restart | status | attach | set-model |\n" +
			"             reset | compact | logs | list\n\n" +
			"The default assistant name is `" + defaultAssistantName + "`. Provide a\n" +
			"different name to run multiple assistants in parallel (e.g.\n" +
			"`oat assistant start work` alongside `oat assistant start personal`).",
	}

	assistantCmd.Subcommands["start"] = &Command{
		Name:        "start",
		Description: "Create + spawn an assistant (idempotent on a healthy one)",
		Usage:       "oat assistant start [name] [--model <id>] [--open-panel]",
		Run:         c.assistantStart,
	}
	assistantCmd.Subcommands["stop"] = &Command{
		Name:        "stop",
		Description: "Gracefully stop an assistant; preserves session JSONL + model preference",
		Usage:       "oat assistant stop [name]",
		Run:         c.assistantStop,
	}
	assistantCmd.Subcommands["restart"] = &Command{
		Name:        "restart",
		Description: "Bounce an assistant; --fresh wipes the session JSONL first",
		Usage:       "oat assistant restart [name] [--fresh]",
		Run:         c.assistantRestart,
	}
	assistantCmd.Subcommands["status"] = &Command{
		Name:        "status",
		Description: "Show assistant state: model, PID, last activity",
		Usage:       "oat assistant status [name] [--json]",
		Run:         c.assistantStatus,
	}
	assistantCmd.Subcommands["attach"] = &Command{
		Name:        "attach",
		Description: "Attach to the assistant's PTY (read-only diagnostics)",
		Usage:       "oat assistant attach [name]",
		Run:         c.assistantAttach,
	}
	assistantCmd.Subcommands["set-model"] = &Command{
		Name:        "set-model",
		Description: "Update the model an assistant will use on next (re)start",
		Usage:       "oat assistant set-model <model-id> [name]",
		Run:         c.assistantSetModel,
	}
	assistantCmd.Subcommands["reset"] = &Command{
		Name:        "reset",
		Description: "Delete the assistant's session JSONL; --full also clears scratchpad",
		Usage:       "oat assistant reset [name] [--full]",
		Run:         c.assistantReset,
	}
	assistantCmd.Subcommands["compact"] = &Command{
		Name:        "compact",
		Description: "Synthetically inject compact_conversation into the assistant's PTY",
		Usage:       "oat assistant compact [name]",
		Run:         c.assistantCompact,
	}
	assistantCmd.Subcommands["logs"] = &Command{
		Name:        "logs",
		Description: "Tail the assistant's output log",
		Usage:       "oat assistant logs [name] [--follow]",
		Run:         c.assistantLogs,
	}
	assistantCmd.Subcommands["list"] = &Command{
		Name:        "list",
		Description: "List all assistants and their current state",
		Usage:       "oat assistant list [--json]",
		Run:         c.assistantList,
	}

	c.rootCmd.Subcommands["assistant"] = assistantCmd
}

// ensureDaemonRunning is the auto-start shim used by every assistant
// verb. The personal-assistant use case is the first OAT entry point
// where the user has not run any `oat repo init` beforehand, so the
// daemon may genuinely not be running yet. Verbs that need the
// daemon call this; verbs that don't (e.g. validation-only helpers)
// don't pay the latency. Idempotent — no-op if the PID file says
// the daemon is alive.
func (c *CLI) ensureDaemonRunning() error {
	pidFile := daemon.NewPIDFile(c.paths.DaemonPID)
	running, _, _ := pidFile.IsRunning()
	if running {
		return nil
	}
	fmt.Println("Starting daemon...")
	if err := daemon.RunDetached(); err != nil {
		return errors.Wrap(errors.CategoryRuntime, "failed to start daemon", err)
	}
	// Same brief wait the TUI uses (cli.go ~line 1492) so the socket
	// has time to come up before the next request. Empirically 500 ms
	// is enough on macOS+linux dev boxes; the verb's first socket
	// call will retry once if the wait was short.
	time.Sleep(500 * time.Millisecond)
	return nil
}

// resolveAssistantName resolves the optional [name] positional. If
// not supplied, returns defaultAssistantName. Centralized so every
// verb's "no name → personal" fallback is byte-identical.
//
// Future work: when there are multiple assistants alive and the
// user runs a verb with no name, we should error and list them
// (per plan body's "errors and asks which"). Today the default-to-
// `personal` covers the single-assistant common case; the multi-
// assistant disambiguation is layered on once the verb tree has
// shipped and the corner is observable.
func resolveAssistantName(args []string) string {
	if len(args) == 0 {
		return defaultAssistantName
	}
	first := strings.TrimSpace(args[0])
	if first == "" || strings.HasPrefix(first, "-") {
		return defaultAssistantName
	}
	return first
}

// agentSlug is the name an assistant agent is registered under
// within its virtual repo. We use the same string as the
// assistant's user-facing name so `oat assistant start personal`
// produces an agent record at (_assistant-personal, personal). This
// keeps the (repo, agent) tuple visible in `oat agent list --all`
// readable without re-deriving anything.
func agentSlug(name string) string {
	return name
}

// assistantStart implements `oat assistant start [name]
// [--model <id>] [--open-panel]`. Idempotent on a healthy
// assistant; restarts a dead one; otherwise creates the virtual
// repo + spawns a fresh assistant agent.
func (c *CLI) assistantStart(args []string) error {
	flags, remaining := ParseFlags(args)
	name := resolveAssistantName(remaining)
	if err := validateVirtualRepoName(name); err != nil {
		return err
	}
	if err := c.ensureDaemonRunning(); err != nil {
		return err
	}

	repoKey, err := c.ensureVirtualRepo(name)
	if err != nil {
		return err
	}
	agent := agentSlug(name)

	// Idempotency check: if the agent is already alive, return
	// success without doing anything (matches `oat assistant
	// start` being a casual "make sure it's running" idiom).
	if pid, alive := c.lookupAliveAssistant(repoKey, agent); alive {
		fmt.Printf("Assistant '%s' is already running (PID %d).\n", name, pid)
		if flags["open-panel"] == "true" {
			c.openSidePanel()
		}
		return nil
	}

	// If a dead state.Agent record exists, remove it before
	// re-adding -- handleAddAgent rejects duplicates. Best-effort:
	// "no such agent" is the happy path, anything else logs+falls
	// through so add_agent's own error message wins (it's more
	// specific than what remove_agent would say).
	_, _ = c.sendDaemonRequest("remove_agent", map[string]interface{}{
		"repo":  repoKey,
		"agent": agent,
	})

	// Worktree path: for a virtual repo this is a plain dir under
	// ~/.oat/wts/_assistant-<name>/<agent>/. Per plan body 5c the
	// agent doesn't actually read it; we create it so existing
	// daemon code paths that os.Stat the worktree don't error.
	wtPath := c.paths.AgentWorktree(repoKey, agent)
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		return errors.Wrap(errors.CategoryRuntime, fmt.Sprintf("failed to create virtual worktree %s", wtPath), err)
	}

	addArgs := map[string]interface{}{
		"repo":          repoKey,
		"agent":         agent,
		"type":          string(state.AgentTypeAssistant),
		"worktree_path": wtPath,
		"window_name":   agent,
	}
	if modelFlag := strings.TrimSpace(flags["model"]); modelFlag != "" {
		addArgs["model"] = modelFlag
	}
	if _, err := c.sendDaemonRequest("add_agent", addArgs); err != nil {
		return err
	}

	// Spawn via start_repo_agents (the same verb the daemon's own
	// boot path uses). For a virtual repo with one registered agent
	// this spawns just the assistant -- no supervisor / merge-queue
	// / workspace, because state.Repository.IsVirtual gates those
	// out (Part 5c).
	startArgs := map[string]interface{}{"repo": repoKey}
	if cliEnv := collectCLIEnvVars(); len(cliEnv) > 0 {
		startArgs["cli_env"] = cliEnv
	}
	if _, err := c.sendDaemonRequest("start_repo_agents", startArgs); err != nil {
		return err
	}

	fmt.Printf("✓ Assistant '%s' started.\n", name)
	fmt.Printf("  Virtual repo: %s\n", repoKey)
	if modelFlag := strings.TrimSpace(flags["model"]); modelFlag != "" {
		fmt.Printf("  Model: %s\n", modelFlag)
	}
	if flags["open-panel"] == "true" {
		c.openSidePanel()
	} else {
		fmt.Println("  Open the side panel of the oat-browser-agent Chrome extension to chat.")
	}
	return nil
}

// assistantStop removes the agent record (which kills the process
// via remove_agent's backend.StopAgent call). The virtual repo +
// session JSONL remain intact for next `oat assistant start`.
func (c *CLI) assistantStop(args []string) error {
	_, remaining := ParseFlags(args)
	name := resolveAssistantName(remaining)
	if err := validateVirtualRepoName(name); err != nil {
		return err
	}
	if err := c.ensureDaemonRunning(); err != nil {
		return err
	}
	repoKey := virtualRepoNameFor(name)
	agent := agentSlug(name)

	_, err := c.sendDaemonRequest("remove_agent", map[string]interface{}{
		"repo":  repoKey,
		"agent": agent,
	})
	if err != nil {
		// "Not found" / "no such agent" is acceptable -- the user's
		// mental model of `stop` is "make sure it's not running".
		// If it wasn't running, we did our job.
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "not found") || strings.Contains(msg, "no such agent") {
			fmt.Printf("Assistant '%s' is not running.\n", name)
			return nil
		}
		return err
	}
	fmt.Printf("✓ Assistant '%s' stopped.\n", name)
	return nil
}

// assistantRestart wraps restart_agent. --fresh wipes the session
// JSONL first, surfacing the "I want a clean conversation" semantic
// without needing two commands.
func (c *CLI) assistantRestart(args []string) error {
	flags, remaining := ParseFlags(args)
	name := resolveAssistantName(remaining)
	if err := validateVirtualRepoName(name); err != nil {
		return err
	}
	if err := c.ensureDaemonRunning(); err != nil {
		return err
	}
	repoKey := virtualRepoNameFor(name)
	agent := agentSlug(name)

	if flags["fresh"] == "true" {
		if err := c.wipeAssistantSession(repoKey, agent); err != nil {
			return err
		}
		fmt.Printf("✓ Session JSONL wiped for assistant '%s'.\n", name)
	}

	resp, err := c.sendDaemonRequest("restart_agent", map[string]interface{}{
		"repo":  repoKey,
		"agent": agent,
	})
	if err != nil {
		return err
	}
	if data, ok := resp.Data.(map[string]interface{}); ok {
		if pid, ok := data["pid"].(float64); ok {
			fmt.Printf("✓ Assistant '%s' restarted (PID %d).\n", name, int(pid))
			return nil
		}
	}
	fmt.Printf("✓ Assistant '%s' restarted.\n", name)
	return nil
}

// AssistantStatusJSON is the wire shape `oat assistant status
// --json` emits to stdout. Owned here on the Go side because Part 6a
// (NM broker RPC handlers in oat-browser-agent) just JSON-parses +
// forwards; the schema decision lives in one place. Field renames or
// type changes need a coordinated edit at both the
// `oat_assistant_status` NM handler and any side-panel renderer that
// consumes the parsed shape. Pinned by TestAssistantStatusJSON in
// `internal/cli/assistant_test.go`.
//
// State enum semantics (use exactly these strings; the side panel
// reads them):
//
//	"no_repo"                  — virtual repo doesn't exist at all
//	                             (assistant has never been started or
//	                             the repo was manually removed).
//	"stopped"                  — virtual repo exists, no Agent record
//	                             (operator ran `oat assistant stop`,
//	                             or the agent was never started).
//	"running"                  — Agent record exists, pid>0, pid is
//	                             alive on this host.
//	"registered_not_running"   — Agent record exists but no live PID
//	                             (auto-restart may be pending, or
//	                             agent was killed externally).
type AssistantStatusJSON struct {
	Name      string `json:"name"`
	RepoKey   string `json:"repo_key"`
	AgentName string `json:"agent_name"`
	State     string `json:"state"`
	// PID == 0 when no agent record OR when daemon hasn't observed a
	// PID yet. Side panel renders as "—" / hides the row in that case.
	PID int `json:"pid"`
	// Model is the LITERAL agent.Model value (empty string when unset).
	// We don't substitute "(default)" the way the human printer does
	// because callers shelling out + parsing want to round-trip a
	// missing model back into `oat assistant set-model`. Side panel
	// can render the empty string as "(default)" client-side.
	Model                 string `json:"model"`
	ModelSwappedOnRestart bool   `json:"model_swapped_on_restart"`
	ModelSwapReason       string `json:"model_swap_reason,omitempty"`
}

// assistantStatus prints a compact snapshot of the assistant's
// state. Reads from list_agents on the virtual repo (cheap; no
// extra socket verbs needed). If no agent record exists, reports
// "stopped" — that's the user's mental model when an assistant
// hasn't been started yet OR has been explicitly stopped.
//
// --json (Part 6 Slice 6.1, 2026-05-25): emit AssistantStatusJSON
// instead of the multi-line human format. The Part 6a NM RPC
// `oat_assistant_status` shells out to this and JSON-parses the
// result, so the wire shape lives here and not in the bridge.
func (c *CLI) assistantStatus(args []string) error {
	flags, remaining := ParseFlags(args)
	name := resolveAssistantName(remaining)
	if err := validateVirtualRepoName(name); err != nil {
		return err
	}
	if err := c.ensureDaemonRunning(); err != nil {
		return err
	}
	repoKey := virtualRepoNameFor(name)
	agent := agentSlug(name)
	outputJSON := flags["json"] == "true"

	status := AssistantStatusJSON{
		Name:      name,
		RepoKey:   repoKey,
		AgentName: agent,
		State:     "stopped",
	}

	resp, err := c.sendDaemonRequest("list_agents", map[string]interface{}{
		"repo": repoKey,
		"rich": true,
	})
	if err != nil {
		// list_agents on an unregistered repo yields an error;
		// treat that as "stopped (no_repo)" rather than spamming the
		// user with a low-level daemon message. JSON callers
		// distinguish via state="no_repo" so they can render
		// "Assistant has never been started" vs "Assistant was
		// stopped" appropriately.
		status.State = "no_repo"
		if outputJSON {
			return emitAssistantJSON(status)
		}
		fmt.Printf("Assistant '%s': stopped (no virtual repo registered)\n", name)
		return nil
	}
	rec, ok := findAssistantAgentMap(resp.Data, agent)
	if !ok {
		// Virtual repo exists, no Agent record. Keep state="stopped"
		// (vs "no_repo") so the side panel can show "Stopped — Start
		// to begin chatting".
		if outputJSON {
			return emitAssistantJSON(status)
		}
		fmt.Printf("Assistant '%s': stopped\n", name)
		fmt.Printf("  Virtual repo: %s (registered; no agent record)\n", repoKey)
		fmt.Printf("  Start with: oat assistant start %s\n", name)
		return nil
	}
	pid := 0
	if v, ok := rec["pid"].(float64); ok {
		pid = int(v)
	}
	status.PID = pid
	if m, _ := rec["model"].(string); m != "" {
		status.Model = m
	}
	status.ModelSwappedOnRestart, _ = rec["model_swapped_on_restart"].(bool)
	status.ModelSwapReason, _ = rec["model_swap_reason"].(string)
	if pid > 0 && isProcessAlive(pid) {
		status.State = "running"
	} else {
		status.State = "registered_not_running"
	}

	if outputJSON {
		return emitAssistantJSON(status)
	}

	model := status.Model
	if model == "" {
		model = "(default)"
	}
	fmt.Printf("Assistant '%s':\n", name)
	fmt.Printf("  Virtual repo: %s\n", repoKey)
	fmt.Printf("  Agent name:   %s\n", agent)
	fmt.Printf("  PID:          %d\n", pid)
	fmt.Printf("  Model:        %s\n", model)
	if status.ModelSwappedOnRestart {
		fmt.Printf("  WARNING:      Model auto-swapped on last restart (%s)\n", status.ModelSwapReason)
	}
	if status.State == "running" {
		fmt.Printf("  State:        running\n")
	} else {
		fmt.Printf("  State:        registered but not running (auto-restart may be pending)\n")
	}
	return nil
}

// emitAssistantJSON writes any value as 2-space-indented JSON to
// stdout with a trailing newline. Matches the convention the
// existing `oat version --json` path uses (cli.go line 309).
// Centralised so the status + list paths produce byte-identical
// formatting and the test suite can assert on it without per-call
// duplication.
func emitAssistantJSON(v interface{}) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// findAssistantAgentMap pulls the raw map for a named agent out of
// a list_agents `rich: true` response. Returns nil, false on any
// shape mismatch. Used by status/list to read the full field set
// (model, model_swapped_on_restart, etc.) that the shared
// lookupAgentInListResp helper deliberately strips out.
func findAssistantAgentMap(data interface{}, agentName string) (map[string]interface{}, bool) {
	arr, ok := data.([]interface{})
	if !ok {
		return nil, false
	}
	for _, item := range arr {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if n, _ := m["name"].(string); n == agentName {
			return m, true
		}
	}
	return nil, false
}

// assistantAttach delegates to `oat ui --repo <virtual-repo-key>`.
// That UI already speaks to the daemon and can attach to any agent
// in the named repo, so we get the assistant attach for free.
func (c *CLI) assistantAttach(args []string) error {
	_, remaining := ParseFlags(args)
	name := resolveAssistantName(remaining)
	if err := validateVirtualRepoName(name); err != nil {
		return err
	}
	repoKey := virtualRepoNameFor(name)
	return c.runUI([]string{"--repo", repoKey})
}

// assistantSetModel changes the model on a registered assistant
// via `set_agent_model`. Mirrors the behaviour of `oat agent set-
// model` for browser-agents — writes to state.Agent.Model, doesn't
// auto-restart, takes effect on the next restart.
//
// If the assistant isn't currently registered (e.g. has never been
// started, or was stopped), this returns an actionable error
// pointing the user at `oat assistant start --model <id>` instead.
// We don't auto-create here because doing so would silently spawn
// an agent the user didn't ask for.
func (c *CLI) assistantSetModel(args []string) error {
	_, remaining := ParseFlags(args)
	if len(remaining) < 1 {
		return errors.InvalidUsage("usage: oat assistant set-model <model-id> [name]")
	}
	modelID := strings.TrimSpace(remaining[0])
	if modelID == "" {
		return errors.InvalidUsage("model-id is required")
	}
	name := defaultAssistantName
	if len(remaining) >= 2 && strings.TrimSpace(remaining[1]) != "" {
		name = strings.TrimSpace(remaining[1])
	}
	if err := validateVirtualRepoName(name); err != nil {
		return err
	}
	if err := c.ensureDaemonRunning(); err != nil {
		return err
	}
	repoKey := virtualRepoNameFor(name)
	agent := agentSlug(name)

	_, err := c.sendDaemonRequest("set_agent_model", map[string]interface{}{
		"repo":  repoKey,
		"agent": agent,
		"model": modelID,
	})
	if err != nil {
		errLower := strings.ToLower(err.Error())
		if strings.Contains(errLower, "not found") || strings.Contains(errLower, "no such agent") {
			return errors.Wrap(
				errors.CategoryNotFound,
				fmt.Sprintf("assistant %q is not registered", name),
				fmt.Errorf("start it with: oat assistant start %s --model %s", name, modelID),
			)
		}
		return err
	}
	fmt.Printf("✓ Assistant '%s' model set to %q.\n", name, modelID)
	fmt.Printf("  Restart with `oat assistant restart %s` for the change to take effect.\n", name)
	return nil
}

// assistantReset wipes the assistant's session JSONL. With --full,
// also clears any scratchpad directory (today: empty for virtual
// repos, but the flag's plumbed through so future memory work has
// a stable verb to extend).
func (c *CLI) assistantReset(args []string) error {
	flags, remaining := ParseFlags(args)
	name := resolveAssistantName(remaining)
	if err := validateVirtualRepoName(name); err != nil {
		return err
	}
	repoKey := virtualRepoNameFor(name)
	agent := agentSlug(name)

	if err := c.wipeAssistantSession(repoKey, agent); err != nil {
		return err
	}
	fmt.Printf("✓ Session JSONL wiped for assistant '%s'.\n", name)
	if flags["full"] == "true" {
		// Scratchpad clearing — empty for virtual repos today but
		// the verb stays forward-compatible.
		scratch := c.paths.ScratchpadDir(repoKey)
		if err := os.RemoveAll(scratch); err != nil && !os.IsNotExist(err) {
			fmt.Printf("  (Warning: failed to clear scratchpad at %s: %v)\n", scratch, err)
		} else {
			fmt.Printf("  Scratchpad cleared.\n")
		}
	}
	return nil
}

// assistantCompact synthetically injects compact_conversation into
// the assistant's PTY via agent_input — the same code path the
// daemon's 95% capacity safety net (5e) uses. Done as a USER-shaped
// message so the agent processes it as conversational input from
// the human, not as a system directive: the agent then chooses to
// call compact_conversation in its next turn.
//
// We send via the existing send_agent_input verb (NOT agent_input,
// which is the security-gated side-panel-chat path). send_agent_input
// addresses by (repo, agent) -- exactly what we have.
func (c *CLI) assistantCompact(args []string) error {
	_, remaining := ParseFlags(args)
	name := resolveAssistantName(remaining)
	if err := validateVirtualRepoName(name); err != nil {
		return err
	}
	if err := c.ensureDaemonRunning(); err != nil {
		return err
	}
	repoKey := virtualRepoNameFor(name)
	agent := agentSlug(name)

	// Match the wording the daemon's 5e safety-net uses so the
	// assistant's prompt instructions about "don't fight the
	// compact" trigger consistently across both paths.
	const compactDirective = "[OAT-system] User requested manual context compaction. Call compact_conversation now before your next reply."
	if _, err := c.sendDaemonRequest("send_agent_input", map[string]interface{}{
		"repo":    repoKey,
		"agent":   agent,
		"message": compactDirective,
	}); err != nil {
		return err
	}
	fmt.Printf("✓ Compact directive injected into assistant '%s'.\n", name)
	return nil
}

// assistantLogs tails the assistant's output log. --follow runs
// `tail -f`-style; otherwise dumps the last 200 lines and returns.
func (c *CLI) assistantLogs(args []string) error {
	flags, remaining := ParseFlags(args)
	name := resolveAssistantName(remaining)
	if err := validateVirtualRepoName(name); err != nil {
		return err
	}
	repoKey := virtualRepoNameFor(name)
	agent := agentSlug(name)
	logPath := c.paths.AgentLogFile(repoKey, agent, false)

	if _, err := os.Stat(logPath); err != nil {
		if os.IsNotExist(err) {
			return errors.Wrap(errors.CategoryNotFound, fmt.Sprintf("no log file at %s", logPath), fmt.Errorf("is assistant '%s' running?", name))
		}
		return errors.Wrap(errors.CategoryRuntime, "failed to stat log file", err)
	}

	follow := flags["follow"] == "true" || flags["f"] == "true"
	if follow {
		// c.tailFile is "follow forever" (cli.go ~line 7901);
		// matches the default-on plan-body behaviour. Press Ctrl-C
		// to stop.
		return c.tailFile(logPath)
	}
	// --no-follow: dump the last ~8KB and return. Same window the
	// follow-mode tailFile pre-seeds.
	return printLogTail(logPath, 8192)
}

// printLogTail prints up to `tailBytes` of the end of a log file
// without following. Inlined here instead of bolting onto tailFile
// because tailFile's contract is "follow forever" and changing
// that would ripple into the other (browser-agent) caller.
func printLogTail(path string, tailBytes int64) error {
	f, err := os.Open(path)
	if err != nil {
		return errors.Wrap(errors.CategoryRuntime, "failed to open log file", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return errors.Wrap(errors.CategoryRuntime, "failed to stat log file", err)
	}
	if info.Size() > tailBytes {
		if _, err := f.Seek(info.Size()-tailBytes, 0); err != nil {
			return errors.Wrap(errors.CategoryRuntime, "failed to seek log file", err)
		}
		// Skip the (likely partial) first line so the output isn't
		// visually jarring.
		scanner := bufio.NewScanner(f)
		scanner.Scan()
	}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fmt.Println(scanner.Text())
	}
	return scanner.Err()
}

// AssistantListEntryJSON is one row of the `oat assistant list
// --json` output. Pinned by TestAssistantListJSON. State enum is a
// SUPERSET of AssistantStatusJSON.State because list also surfaces
// "dead" (pid>0 but process not alive on this host — a CRASHED
// agent that the daemon's health-check hasn't observed yet) and
// "registered" (Agent record exists, pid==0; never started since
// daemon boot OR daemon hasn't spawned it yet).
//
// Field naming intentionally matches AssistantStatusJSON where it
// overlaps so the side panel can reuse a single renderer. The list
// shape stays a flat array (NOT { entries: [...] }) so the NM RPC
// handler can `JSON.parse(stdout)` and treat the result as the
// final value with no unwrapping.
type AssistantListEntryJSON struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Model string `json:"model"`
	PID   int    `json:"pid"`
}

// assistantList enumerates every virtual repo + its assistant
// state. This is the "filter view" implementation chosen during
// the Part 5 design questions — same data as `oat agent list`,
// rendered with assistant-friendly columns and filtered to
// AgentTypeAssistant.
//
// --json (Part 6 Slice 6.1, 2026-05-25): emit a JSON array of
// AssistantListEntryJSON. Empty result is the literal `[]\n` (NOT
// the human-text "No assistants registered." message) so callers
// can `length === 0` check the parsed result without string parsing.
func (c *CLI) assistantList(args []string) error {
	flags, _ := ParseFlags(args)
	outputJSON := flags["json"] == "true"

	if err := c.ensureDaemonRunning(); err != nil {
		return err
	}
	virt, err := c.listVirtualRepos()
	if err != nil {
		return err
	}

	entries := make([]AssistantListEntryJSON, 0, len(virt))
	for repoKey := range virt {
		name := strings.TrimPrefix(repoKey, "_assistant-")
		entry := AssistantListEntryJSON{
			Name:  name,
			State: "stopped",
		}
		resp, err := c.sendDaemonRequest("list_agents", map[string]interface{}{
			"repo": repoKey,
			"rich": true,
		})
		if err == nil {
			if rec, ok := findAssistantAgentMap(resp.Data, agentSlug(name)); ok {
				pid := 0
				if v, ok := rec["pid"].(float64); ok {
					pid = int(v)
				}
				if m, _ := rec["model"].(string); m != "" {
					entry.Model = m
				}
				entry.PID = pid
				if pid > 0 {
					if isProcessAlive(pid) {
						entry.State = "running"
					} else {
						entry.State = "dead"
					}
				} else {
					entry.State = "registered"
				}
			}
		}
		entries = append(entries, entry)
	}

	if outputJSON {
		return emitAssistantJSON(entries)
	}

	if len(entries) == 0 {
		fmt.Println("No assistants registered.")
		fmt.Println("Start one with: oat assistant start")
		return nil
	}

	fmt.Printf("Assistants (%d):\n\n", len(entries))
	fmt.Printf("  %-20s  %-10s  %-30s  %-6s\n", "NAME", "STATE", "MODEL", "PID")
	fmt.Printf("  %s\n", strings.Repeat("-", 70))
	for _, e := range entries {
		model := e.Model
		if model == "" {
			model = "(default)"
		}
		pidStr := "-"
		if e.PID > 0 {
			pidStr = fmt.Sprintf("%d", e.PID)
		}
		fmt.Printf("  %-20s  %-10s  %-30s  %-6s\n", e.Name, e.State, model, pidStr)
	}
	return nil
}

// ----------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------

// lookupAliveAssistant returns (pid, true) iff the agent record
// exists, has a non-zero PID, and the PID is alive. Centralized so
// `start`'s idempotency check and `status`'s state computation use
// identical liveness criteria.
func (c *CLI) lookupAliveAssistant(repoKey, agent string) (int, bool) {
	resp, err := c.sendDaemonRequest("list_agents", map[string]interface{}{
		"repo": repoKey,
		"rich": true,
	})
	if err != nil {
		return 0, false
	}
	rec, ok := findAssistantAgentMap(resp.Data, agent)
	if !ok {
		return 0, false
	}
	pid := 0
	if v, ok := rec["pid"].(float64); ok {
		pid = int(v)
	}
	if pid > 0 && isProcessAlive(pid) {
		return pid, true
	}
	return 0, false
}

// wipeAssistantSession deletes the session-JSONL file the agent
// uses for `--resume` continuity. Logic lives in oat-agent runtime
// (the Python side); the on-disk path is `~/.oat/output/<repo>/
// <agent>.session.jsonl`. Best-effort: missing file is success.
func (c *CLI) wipeAssistantSession(repoKey, agent string) error {
	// Today the canonical session-JSONL path isn't a first-class
	// config.Paths method (it's a per-runtime convention). Derive
	// it from the existing output-dir layout; if the runtime
	// rename ever changes this, update here in one place.
	jsonlPath := c.paths.AgentLogFile(repoKey, agent, false)
	jsonlPath = strings.TrimSuffix(jsonlPath, ".log") + ".session.jsonl"
	if err := os.Remove(jsonlPath); err != nil && !os.IsNotExist(err) {
		return errors.Wrap(errors.CategoryRuntime, fmt.Sprintf("failed to wipe session JSONL %s", jsonlPath), err)
	}
	return nil
}

// openSidePanel best-effort opens the user's browser at a hint
// page so they know where to look for the side panel. macOS / linux
// only for now; Windows just prints the hint.
func (c *CLI) openSidePanel() {
	const hint = "Open the side panel of the oat-browser-agent Chrome extension."
	fmt.Println(hint)
	// Future work: chrome://extensions/?id=... deep-link. For v1 we
	// just print the hint; the user already has the extension
	// installed since they're using the assistant.
}

// Compile-time anchor for the `daemon` import — only used inside
// ensureDaemonRunning + the package-level functions; keeping the
// reference here means a future refactor that drops the auto-start
// path doesn't leave a dangling import to chase.
var _ = daemon.NewPIDFile
