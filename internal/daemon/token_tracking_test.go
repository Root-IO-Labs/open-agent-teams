package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

func TestHandleTokenUsageEvent(t *testing.T) {
	t.Run("normal cumulative update", func(t *testing.T) {
		d, cleanup := setupTestDaemon(t)
		defer cleanup()

		repo := "test-repo"
		agent := "test-agent"
		d.state.AddRepo(repo, &state.Repository{GithubURL: "https://github.com/test/test"})
		d.state.AddAgent(repo, agent, state.Agent{Type: state.AgentTypeWorker})

		payload, _ := json.Marshal(map[string]int64{
			"delta_input":       800,
			"delta_output":      200,
			"cumulative_input":  800,
			"cumulative_output": 200,
		})
		d.handleTokenUsageEvent(repo, agent, string(payload))

		a, _ := d.state.GetAgent(repo, agent)
		if a.InputTokens != 800 {
			t.Errorf("InputTokens = %d, want 800", a.InputTokens)
		}
		if a.OutputTokens != 200 {
			t.Errorf("OutputTokens = %d, want 200", a.OutputTokens)
		}
		if a.TotalTokens != 1000 {
			t.Errorf("TotalTokens = %d, want 1000", a.TotalTokens)
		}
		if a.LastTokenUpdate.IsZero() {
			t.Error("LastTokenUpdate should be set")
		}
	})

	t.Run("monotonicity guard rejects stale event", func(t *testing.T) {
		d, cleanup := setupTestDaemon(t)
		defer cleanup()

		repo := "test-repo"
		agent := "test-agent"
		d.state.AddRepo(repo, &state.Repository{GithubURL: "https://github.com/test/test"})
		d.state.AddAgent(repo, agent, state.Agent{
			Type:         state.AgentTypeWorker,
			InputTokens:  5000,
			OutputTokens: 1000,
			TotalTokens:  6000,
		})

		// Stale event with lower cumulative (e.g. from restarted process)
		payload, _ := json.Marshal(map[string]int64{
			"delta_input":       100,
			"delta_output":      50,
			"cumulative_input":  100,
			"cumulative_output": 50,
		})
		d.handleTokenUsageEvent(repo, agent, string(payload))

		a, _ := d.state.GetAgent(repo, agent)
		// Should be unchanged — stale event ignored
		if a.InputTokens != 5000 {
			t.Errorf("InputTokens = %d, want 5000 (stale event should be ignored)", a.InputTokens)
		}
		if a.OutputTokens != 1000 {
			t.Errorf("OutputTokens = %d, want 1000 (stale event should be ignored)", a.OutputTokens)
		}
	})

	t.Run("replayed same payload is idempotent", func(t *testing.T) {
		d, cleanup := setupTestDaemon(t)
		defer cleanup()

		repo := "test-repo"
		agent := "test-agent"
		d.state.AddRepo(repo, &state.Repository{GithubURL: "https://github.com/test/test"})
		d.state.AddAgent(repo, agent, state.Agent{Type: state.AgentTypeWorker})

		payload, _ := json.Marshal(map[string]int64{
			"delta_input":       500,
			"delta_output":      200,
			"cumulative_input":  500,
			"cumulative_output": 200,
		})

		// Send same payload twice
		d.handleTokenUsageEvent(repo, agent, string(payload))
		d.handleTokenUsageEvent(repo, agent, string(payload))

		a, _ := d.state.GetAgent(repo, agent)
		if a.TotalTokens != 700 {
			t.Errorf("TotalTokens = %d, want 700 (replayed payload should be idempotent)", a.TotalTokens)
		}
	})

	t.Run("increasing cumulative accepted", func(t *testing.T) {
		d, cleanup := setupTestDaemon(t)
		defer cleanup()

		repo := "test-repo"
		agent := "test-agent"
		d.state.AddRepo(repo, &state.Repository{GithubURL: "https://github.com/test/test"})
		d.state.AddAgent(repo, agent, state.Agent{Type: state.AgentTypeWorker})

		// First event
		payload1, _ := json.Marshal(map[string]int64{
			"delta_input":       500,
			"delta_output":      200,
			"cumulative_input":  500,
			"cumulative_output": 200,
		})
		d.handleTokenUsageEvent(repo, agent, string(payload1))

		// Second event with higher cumulative
		payload2, _ := json.Marshal(map[string]int64{
			"delta_input":       300,
			"delta_output":      100,
			"cumulative_input":  800,
			"cumulative_output": 300,
		})
		d.handleTokenUsageEvent(repo, agent, string(payload2))

		a, _ := d.state.GetAgent(repo, agent)
		if a.InputTokens != 800 {
			t.Errorf("InputTokens = %d, want 800", a.InputTokens)
		}
		if a.OutputTokens != 300 {
			t.Errorf("OutputTokens = %d, want 300", a.OutputTokens)
		}
		if a.TotalTokens != 1100 {
			t.Errorf("TotalTokens = %d, want 1100", a.TotalTokens)
		}
	})

	t.Run("true input/output separation", func(t *testing.T) {
		d, cleanup := setupTestDaemon(t)
		defer cleanup()

		repo := "test-repo"
		agent := "test-agent"
		d.state.AddRepo(repo, &state.Repository{GithubURL: "https://github.com/test/test"})
		d.state.AddAgent(repo, agent, state.Agent{Type: state.AgentTypeWorker})

		payload, _ := json.Marshal(map[string]int64{
			"delta_input":       1000,
			"delta_output":      500,
			"cumulative_input":  1000,
			"cumulative_output": 500,
		})
		d.handleTokenUsageEvent(repo, agent, string(payload))

		a, _ := d.state.GetAgent(repo, agent)
		// Must NOT collapse into InputTokens=total, OutputTokens=0
		if a.InputTokens != 1000 {
			t.Errorf("InputTokens = %d, want 1000", a.InputTokens)
		}
		if a.OutputTokens != 500 {
			t.Errorf("OutputTokens = %d, want 500 (should be separate, not 0)", a.OutputTokens)
		}
	})

	t.Run("nonexistent agent ignored", func(t *testing.T) {
		d, cleanup := setupTestDaemon(t)
		defer cleanup()

		repo := "test-repo"
		d.state.AddRepo(repo, &state.Repository{GithubURL: "https://github.com/test/test"})

		payload, _ := json.Marshal(map[string]int64{
			"cumulative_input":  100,
			"cumulative_output": 50,
		})
		// Should not panic
		d.handleTokenUsageEvent(repo, "nonexistent", string(payload))
	})

	t.Run("invalid JSON ignored", func(t *testing.T) {
		d, cleanup := setupTestDaemon(t)
		defer cleanup()

		repo := "test-repo"
		agent := "test-agent"
		d.state.AddRepo(repo, &state.Repository{GithubURL: "https://github.com/test/test"})
		d.state.AddAgent(repo, agent, state.Agent{Type: state.AgentTypeWorker})

		// Should not panic, just log warning
		d.handleTokenUsageEvent(repo, agent, "not json")

		a, _ := d.state.GetAgent(repo, agent)
		if a.TotalTokens != 0 {
			t.Errorf("TotalTokens = %d, want 0 (invalid JSON should be ignored)", a.TotalTokens)
		}
	})
}

func TestTokenBudgetEnforcement(t *testing.T) {
	t.Run("under budget is not killed", func(t *testing.T) {
		d, cleanup := setupTestDaemon(t)
		defer cleanup()

		repo := "test-repo"
		agent := "test-agent"
		d.state.AddRepo(repo, &state.Repository{GithubURL: "https://github.com/test/test"})
		d.state.AddAgent(repo, agent, state.Agent{
			Type:      state.AgentTypeWorker,
			MaxTokens: 100000,
		})

		payload, _ := json.Marshal(map[string]int64{
			"delta_input":       50000,
			"delta_output":      5000,
			"cumulative_input":  50000,
			"cumulative_output": 5000,
		})
		d.handleTokenUsageEvent(repo, agent, string(payload))

		a, exists := d.state.GetAgent(repo, agent)
		if !exists {
			t.Fatal("agent should still exist")
		}
		if a.TotalTokens != 55000 {
			t.Errorf("TotalTokens = %d, want 55000", a.TotalTokens)
		}
		if a.FailureReason != "" {
			t.Errorf("FailureReason should be empty, got %q", a.FailureReason)
		}
	})

	t.Run("over budget marks failure reason", func(t *testing.T) {
		d, cleanup := setupTestDaemon(t)
		defer cleanup()

		repo := "test-repo"
		agent := "test-agent"
		d.state.AddRepo(repo, &state.Repository{GithubURL: "https://github.com/test/test"})
		d.state.AddAgent(repo, agent, state.Agent{
			Type:      state.AgentTypeWorker,
			MaxTokens: 50000,
			Task:      "test task",
		})

		payload, _ := json.Marshal(map[string]int64{
			"delta_input":       60000,
			"delta_output":      5000,
			"cumulative_input":  60000,
			"cumulative_output": 5000,
		})
		d.handleTokenUsageEvent(repo, agent, string(payload))

		// Give the goroutine a moment to run
		time.Sleep(100 * time.Millisecond)

		a, _ := d.state.GetAgent(repo, agent)
		if a.TotalTokens != 65000 {
			t.Errorf("TotalTokens = %d, want 65000", a.TotalTokens)
		}
		if a.FailureReason == "" {
			t.Error("FailureReason should be set for over-budget agent")
		}
		if !a.ReadyForCleanup {
			t.Error("ReadyForCleanup should be true for over-budget agent")
		}
	})

	t.Run("no budget means no limit", func(t *testing.T) {
		d, cleanup := setupTestDaemon(t)
		defer cleanup()

		repo := "test-repo"
		agent := "test-agent"
		d.state.AddRepo(repo, &state.Repository{GithubURL: "https://github.com/test/test"})
		d.state.AddAgent(repo, agent, state.Agent{
			Type:      state.AgentTypeWorker,
			MaxTokens: 0, // no limit
		})

		payload, _ := json.Marshal(map[string]int64{
			"delta_input":       9999999,
			"delta_output":      9999999,
			"cumulative_input":  9999999,
			"cumulative_output": 9999999,
		})
		d.handleTokenUsageEvent(repo, agent, string(payload))

		a, _ := d.state.GetAgent(repo, agent)
		if a.FailureReason != "" {
			t.Errorf("FailureReason should be empty with no budget, got %q", a.FailureReason)
		}
	})
}

func TestTaskHistoryTokenSnapshot(t *testing.T) {
	t.Run("completion snapshots tokens", func(t *testing.T) {
		entry := state.TaskHistoryEntry{
			Name:         "worker-1",
			Task:         "Fix bug",
			InputTokens:  5000,
			OutputTokens: 2000,
			TotalTokens:  7000,
		}

		if entry.InputTokens != 5000 {
			t.Errorf("InputTokens = %d, want 5000", entry.InputTokens)
		}
		if entry.OutputTokens != 2000 {
			t.Errorf("OutputTokens = %d, want 2000", entry.OutputTokens)
		}
		if entry.TotalTokens != 7000 {
			t.Errorf("TotalTokens = %d, want 7000", entry.TotalTokens)
		}
	})

	t.Run("zero tokens omitted in JSON", func(t *testing.T) {
		entry := state.TaskHistoryEntry{
			Name: "worker-1",
			Task: "Fix bug",
		}

		data, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("Marshal failed: %v", err)
		}

		var m map[string]interface{}
		json.Unmarshal(data, &m)

		// Zero-valued token fields should be omitted (omitempty)
		if _, exists := m["input_tokens"]; exists {
			t.Error("input_tokens should be omitted when zero")
		}
		if _, exists := m["output_tokens"]; exists {
			t.Error("output_tokens should be omitted when zero")
		}
	})
}

func TestCacheMetricsTracking(t *testing.T) {
	t.Run("cache fields parsed and stored", func(t *testing.T) {
		d, cleanup := setupTestDaemon(t)
		defer cleanup()

		repo := "test-repo"
		agent := "test-agent"
		d.state.AddRepo(repo, &state.Repository{GithubURL: "https://github.com/test/test"})
		d.state.AddAgent(repo, agent, state.Agent{Type: state.AgentTypeWorker})

		payload, _ := json.Marshal(map[string]int64{
			"cumulative_input":  10000,
			"cumulative_output": 500,
			"cache_read":        7000,
			"cache_creation":    2000,
		})
		d.handleTokenUsageEvent(repo, agent, string(payload))

		a, _ := d.state.GetAgent(repo, agent)
		if a.InputTokens != 10000 {
			t.Errorf("InputTokens = %d, want 10000", a.InputTokens)
		}
		if a.CacheReadTokens != 7000 {
			t.Errorf("CacheReadTokens = %d, want 7000", a.CacheReadTokens)
		}
		if a.CacheCreationTokens != 2000 {
			t.Errorf("CacheCreationTokens = %d, want 2000", a.CacheCreationTokens)
		}
	})

	t.Run("cache fields omitted when zero", func(t *testing.T) {
		d, cleanup := setupTestDaemon(t)
		defer cleanup()

		repo := "test-repo"
		agent := "test-agent"
		d.state.AddRepo(repo, &state.Repository{GithubURL: "https://github.com/test/test"})
		d.state.AddAgent(repo, agent, state.Agent{Type: state.AgentTypeWorker})

		// No cache fields in payload (non-Anthropic model)
		payload, _ := json.Marshal(map[string]int64{
			"cumulative_input":  5000,
			"cumulative_output": 200,
		})
		d.handleTokenUsageEvent(repo, agent, string(payload))

		a, _ := d.state.GetAgent(repo, agent)
		if a.CacheReadTokens != 0 {
			t.Errorf("CacheReadTokens = %d, want 0 for non-caching provider", a.CacheReadTokens)
		}
		if a.CacheCreationTokens != 0 {
			t.Errorf("CacheCreationTokens = %d, want 0 for non-caching provider", a.CacheCreationTokens)
		}

		// Verify they're omitted in JSON serialization
		data, _ := json.Marshal(a)
		var m map[string]interface{}
		json.Unmarshal(data, &m)
		if _, exists := m["cache_read_tokens"]; exists {
			t.Error("cache_read_tokens should be omitted when zero")
		}
	})

	t.Run("cache hit rate calculable from stored values", func(t *testing.T) {
		a := state.Agent{
			InputTokens:         100000,
			CacheReadTokens:     70000,
			CacheCreationTokens: 20000,
		}
		// Cache hit rate: cache_read / input_tokens
		hitRate := float64(a.CacheReadTokens) / float64(a.InputTokens)
		if hitRate < 0.69 || hitRate > 0.71 {
			t.Errorf("cache hit rate = %.2f, want ~0.70", hitRate)
		}
	})
}

// TestTokenEmissionEndToEnd proves the full pipeline:
//
//	file write → OutputWatcher → EventTokenUsage → handleTokenUsageEvent → state.json
//
// This is the pipeline a real agent uses: Python's _emit_oat_tokens appends
// [OAT_TOKENS] lines to OAT_TOOL_LOG, and the daemon's OutputWatcher
// goroutine tails that file. If any link is wrong — regex, JSON parse,
// event routing, state update — this test catches it.
func TestTokenEmissionEndToEnd(t *testing.T) {
	t.Run("three-turn agent tool chain with cache metrics", func(t *testing.T) {
		d, cleanup := setupTestDaemon(t)
		defer cleanup()

		repo := "test-repo"
		agent := "test-agent"
		d.state.AddRepo(repo, &state.Repository{GithubURL: "https://github.com/test/test"})
		d.state.AddAgent(repo, agent, state.Agent{Type: state.AgentTypeWorker})

		// Create the log file in a daemon-managed temp location.
		logFile := filepath.Join(d.paths.OutputDir, "e2e-test.log")
		if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		f, err := os.Create(logFile)
		if err != nil {
			t.Fatalf("create log: %v", err)
		}
		f.Close()

		// Start the real OutputWatcher on the file.
		d.startOutputWatcher(repo, agent, logFile)

		// Simulate three turns exactly as _emit_oat_tokens would write them.
		// Cumulative values are what the daemon treats as authoritative;
		// delta values are the per-turn spend.
		lines := []string{
			`[OAT_TOKENS] {"delta_input": 2000, "delta_output": 80, "cumulative_input": 2000, "cumulative_output": 80, "cache_read": 0, "cache_creation": 1500}` + "\n",
			`[OAT_TOKENS] {"delta_input": 1200, "delta_output": 150, "cumulative_input": 3200, "cumulative_output": 230, "cache_read": 1000, "cache_creation": 1500}` + "\n",
			`[OAT_TOKENS] {"delta_input": 1500, "delta_output": 200, "cumulative_input": 4700, "cumulative_output": 430, "cache_read": 2400, "cache_creation": 1500}` + "\n",
		}
		appendFile, err := os.OpenFile(logFile, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatalf("open log for append: %v", err)
		}
		for _, line := range lines {
			if _, err := appendFile.WriteString(line); err != nil {
				t.Fatalf("write line: %v", err)
			}
			// Small gap so the watcher processes each line independently
			// rather than as one jumbo chunk.
			time.Sleep(20 * time.Millisecond)
		}
		appendFile.Close()

		// Poll state until the final cumulative lands, up to 2 s.
		deadline := time.Now().Add(2 * time.Second)
		var finalAgent state.Agent
		for time.Now().Before(deadline) {
			a, ok := d.state.GetAgent(repo, agent)
			if ok && a.InputTokens == 4700 {
				finalAgent = a
				break
			}
			time.Sleep(20 * time.Millisecond)
		}

		if finalAgent.InputTokens != 4700 {
			t.Fatalf("InputTokens = %d, want 4700 (watcher did not process file)", finalAgent.InputTokens)
		}
		if finalAgent.OutputTokens != 430 {
			t.Errorf("OutputTokens = %d, want 430", finalAgent.OutputTokens)
		}
		if finalAgent.TotalTokens != 5130 {
			t.Errorf("TotalTokens = %d, want 5130", finalAgent.TotalTokens)
		}
		if finalAgent.CacheReadTokens != 2400 {
			t.Errorf("CacheReadTokens = %d, want 2400", finalAgent.CacheReadTokens)
		}
		if finalAgent.CacheCreationTokens != 1500 {
			t.Errorf("CacheCreationTokens = %d, want 1500", finalAgent.CacheCreationTokens)
		}
	})

	t.Run("non-caching provider: compact payload also lands", func(t *testing.T) {
		d, cleanup := setupTestDaemon(t)
		defer cleanup()

		repo := "test-repo"
		agent := "test-agent"
		d.state.AddRepo(repo, &state.Repository{GithubURL: "https://github.com/test/test"})
		d.state.AddAgent(repo, agent, state.Agent{Type: state.AgentTypeWorker})

		logFile := filepath.Join(d.paths.OutputDir, "e2e-nocache.log")
		_ = os.MkdirAll(filepath.Dir(logFile), 0o755)
		f, _ := os.Create(logFile)
		f.Close()

		d.startOutputWatcher(repo, agent, logFile)

		// Payload with no cache fields — simulates OpenAI/local models.
		line := `[OAT_TOKENS] {"delta_input": 5000, "delta_output": 200, "cumulative_input": 5000, "cumulative_output": 200}` + "\n"
		appendFile, _ := os.OpenFile(logFile, os.O_APPEND|os.O_WRONLY, 0o644)
		appendFile.WriteString(line)
		appendFile.Close()

		deadline := time.Now().Add(2 * time.Second)
		var finalAgent state.Agent
		for time.Now().Before(deadline) {
			a, ok := d.state.GetAgent(repo, agent)
			if ok && a.InputTokens == 5000 {
				finalAgent = a
				break
			}
			time.Sleep(20 * time.Millisecond)
		}

		if finalAgent.InputTokens != 5000 {
			t.Fatalf("InputTokens = %d, want 5000", finalAgent.InputTokens)
		}
		if finalAgent.CacheReadTokens != 0 {
			t.Errorf("CacheReadTokens = %d, want 0 for non-caching provider", finalAgent.CacheReadTokens)
		}
	})

	// Workers live at <repo>/workers/<name>.log rather than
	// <repo>/<name>.log. That subdirectory convention has its own
	// mkdir / create / watcher-attach path through d.paths.AgentLogFile.
	// This subtest forces the full worker path — the same path `oat work
	// create` will use when the benchmark spawns workers — so a regression
	// in worker log resolution would be caught here instead of during an
	// expensive full benchmark run.
	t.Run("worker at workers/ subdir emits and state updates", func(t *testing.T) {
		d, cleanup := setupTestDaemon(t)
		defer cleanup()

		repo := "test-repo"
		workerName := "happy-eagle"
		d.state.AddRepo(repo, &state.Repository{GithubURL: "https://github.com/test/test"})
		d.state.AddAgent(repo, workerName, state.Agent{Type: state.AgentTypeWorker})

		// Use the exact path resolver the daemon uses so any future change
		// to the worker log convention is caught by this test.
		logFile := d.paths.AgentLogFile(repo, workerName, true)
		if !strings.Contains(logFile, "workers") {
			t.Fatalf("worker log path should live under workers/: got %q", logFile)
		}
		if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(logFile), err)
		}
		f, err := os.Create(logFile)
		if err != nil {
			t.Fatalf("create worker log: %v", err)
		}
		f.Close()

		d.startOutputWatcher(repo, workerName, logFile)

		// Two-turn worker chain: first turn creates cache, second hits it.
		// Byte-identical to what real Python emits at the end of a task.
		lines := []string{
			`[OAT_TOKENS] {"delta_input": 8000, "delta_output": 250, "cumulative_input": 8000, "cumulative_output": 250, "cache_read": 0, "cache_creation": 6000}` + "\n",
			`[OAT_TOKENS] {"delta_input": 3500, "delta_output": 180, "cumulative_input": 11500, "cumulative_output": 430, "cache_read": 5700, "cache_creation": 6000}` + "\n",
		}
		appendFile, err := os.OpenFile(logFile, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatalf("open for append: %v", err)
		}
		for _, line := range lines {
			if _, err := appendFile.WriteString(line); err != nil {
				t.Fatalf("write: %v", err)
			}
			time.Sleep(20 * time.Millisecond)
		}
		appendFile.Close()

		deadline := time.Now().Add(2 * time.Second)
		var final state.Agent
		for time.Now().Before(deadline) {
			a, ok := d.state.GetAgent(repo, workerName)
			if ok && a.InputTokens == 11500 {
				final = a
				break
			}
			time.Sleep(20 * time.Millisecond)
		}

		if final.InputTokens != 11500 {
			t.Fatalf("worker InputTokens = %d, want 11500 (watcher did not see worker log)", final.InputTokens)
		}
		if final.OutputTokens != 430 {
			t.Errorf("worker OutputTokens = %d, want 430", final.OutputTokens)
		}
		if final.CacheReadTokens != 5700 {
			t.Errorf("worker CacheReadTokens = %d, want 5700", final.CacheReadTokens)
		}
		if final.CacheCreationTokens != 6000 {
			t.Errorf("worker CacheCreationTokens = %d, want 6000", final.CacheCreationTokens)
		}
		if final.Type != state.AgentTypeWorker {
			t.Errorf("worker agent Type = %q, want worker", final.Type)
		}
	})
}

// TestHandleTokenUsageEvent_CacheReadMonotonicity verifies that a payload
// reporting cache_read LOWER than the stored value does not clobber state.
// Scenario: agent accumulates CacheReadTokens=5000 over its lifetime, then
// its process restarts with a cold cache and emits cache_read=0. We must
// keep the stored 5000, while still accepting the incoming input/output
// cumulative totals (they pass the combined-total guard).
func TestHandleTokenUsageEvent_CacheReadMonotonicity(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()

	repo := "test-repo"
	agent := "test-agent"
	d.state.AddRepo(repo, &state.Repository{GithubURL: "https://github.com/test/test"})
	d.state.AddAgent(repo, agent, state.Agent{
		Type:                state.AgentTypeWorker,
		InputTokens:         10000,
		OutputTokens:        500,
		TotalTokens:         10500,
		CacheReadTokens:     5000,
		CacheCreationTokens: 2000,
	})

	// Higher cumulative input/output (advances past the total guard),
	// but cache_read regressed to 0. Must clamp cache_read only.
	payload, _ := json.Marshal(map[string]int64{
		"cumulative_input":  12000,
		"cumulative_output": 600,
		"cache_read":        0,
		"cache_creation":    2000,
	})
	d.handleTokenUsageEvent(repo, agent, string(payload))

	a, _ := d.state.GetAgent(repo, agent)
	if a.InputTokens != 12000 {
		t.Errorf("InputTokens = %d, want 12000 (should still advance)", a.InputTokens)
	}
	if a.OutputTokens != 600 {
		t.Errorf("OutputTokens = %d, want 600 (should still advance)", a.OutputTokens)
	}
	if a.CacheReadTokens != 5000 {
		t.Errorf("CacheReadTokens = %d, want 5000 (regression must be clamped)", a.CacheReadTokens)
	}
	if a.CacheCreationTokens != 2000 {
		t.Errorf("CacheCreationTokens = %d, want 2000 (unchanged)", a.CacheCreationTokens)
	}
}

// TestHandleTokenUsageEvent_CacheCreationMonotonicity is the symmetric test
// for cache_creation. Same structure, different field.
func TestHandleTokenUsageEvent_CacheCreationMonotonicity(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()

	repo := "test-repo"
	agent := "test-agent"
	d.state.AddRepo(repo, &state.Repository{GithubURL: "https://github.com/test/test"})
	d.state.AddAgent(repo, agent, state.Agent{
		Type:                state.AgentTypeWorker,
		InputTokens:         10000,
		OutputTokens:        500,
		TotalTokens:         10500,
		CacheReadTokens:     3000,
		CacheCreationTokens: 4000,
	})

	// Higher cumulative totals, but cache_creation regressed.
	payload, _ := json.Marshal(map[string]int64{
		"cumulative_input":  12000,
		"cumulative_output": 600,
		"cache_read":        3000,
		"cache_creation":    0,
	})
	d.handleTokenUsageEvent(repo, agent, string(payload))

	a, _ := d.state.GetAgent(repo, agent)
	if a.CacheCreationTokens != 4000 {
		t.Errorf("CacheCreationTokens = %d, want 4000 (regression must be clamped)", a.CacheCreationTokens)
	}
	if a.CacheReadTokens != 3000 {
		t.Errorf("CacheReadTokens = %d, want 3000 (unchanged)", a.CacheReadTokens)
	}
	if a.InputTokens != 12000 {
		t.Errorf("InputTokens = %d, want 12000", a.InputTokens)
	}
}

// TestHandleTokenUsageEvent_CacheFieldsAdvanceNormally guards against a
// false-positive clamp: when cache counts legitimately grow, they must be
// written through, not held.
func TestHandleTokenUsageEvent_CacheFieldsAdvanceNormally(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()

	repo := "test-repo"
	agent := "test-agent"
	d.state.AddRepo(repo, &state.Repository{GithubURL: "https://github.com/test/test"})
	d.state.AddAgent(repo, agent, state.Agent{
		Type:                state.AgentTypeWorker,
		InputTokens:         10000,
		OutputTokens:        500,
		TotalTokens:         10500,
		CacheReadTokens:     5000,
		CacheCreationTokens: 2000,
	})

	payload, _ := json.Marshal(map[string]int64{
		"cumulative_input":  15000,
		"cumulative_output": 700,
		"cache_read":        7000, // grew from 5000
		"cache_creation":    3500, // grew from 2000
	})
	d.handleTokenUsageEvent(repo, agent, string(payload))

	a, _ := d.state.GetAgent(repo, agent)
	if a.CacheReadTokens != 7000 {
		t.Errorf("CacheReadTokens = %d, want 7000 (should advance normally)", a.CacheReadTokens)
	}
	if a.CacheCreationTokens != 3500 {
		t.Errorf("CacheCreationTokens = %d, want 3500 (should advance normally)", a.CacheCreationTokens)
	}
}

// TestStartOutputWatcher_DrainsOnShutdown verifies that buffered token
// events still in the watcher's channel at daemon-shutdown time are
// processed rather than silently dropped. We write several [OAT_TOKENS]
// lines quickly, then cancel the daemon context before the outer select
// can drain them, and assert the final cumulative total lands in state.
func TestStartOutputWatcher_DrainsOnShutdown(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()

	repo := "test-repo"
	agent := "test-agent"
	d.state.AddRepo(repo, &state.Repository{GithubURL: "https://github.com/test/test"})
	d.state.AddAgent(repo, agent, state.Agent{Type: state.AgentTypeWorker})

	logFile := filepath.Join(d.paths.OutputDir, "drain-test.log")
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f, err := os.Create(logFile)
	if err != nil {
		t.Fatalf("create log: %v", err)
	}
	f.Close()

	d.startOutputWatcher(repo, agent, logFile)

	// Write a sequence of cumulative-advancing payloads and then cancel the
	// daemon context. The drain loop must flush any buffered events through
	// handleTokenUsageEvent before the goroutine returns.
	appendFile, err := os.OpenFile(logFile, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	lines := []string{
		`[OAT_TOKENS] {"delta_input": 1000, "delta_output": 50, "cumulative_input": 1000, "cumulative_output": 50}` + "\n",
		`[OAT_TOKENS] {"delta_input": 1000, "delta_output": 50, "cumulative_input": 2000, "cumulative_output": 100}` + "\n",
		`[OAT_TOKENS] {"delta_input": 1000, "delta_output": 50, "cumulative_input": 3000, "cumulative_output": 150}` + "\n",
	}
	for _, line := range lines {
		if _, err := appendFile.WriteString(line); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	appendFile.Close()

	// Give the tail reader a brief moment to pick up the appended data and
	// emit events into the watcher's buffered channel.
	time.Sleep(300 * time.Millisecond)

	// Now cancel — drain path must process any still-buffered events.
	d.cancel()

	// Bounded wait for the watcher goroutine to exit.
	waitDone := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(3 * time.Second):
		t.Fatal("watcher goroutine did not exit within 3s of cancel")
	}

	a, _ := d.state.GetAgent(repo, agent)
	if a.InputTokens != 3000 {
		t.Errorf("InputTokens = %d, want 3000 (drain should have processed final event)", a.InputTokens)
	}
	if a.OutputTokens != 150 {
		t.Errorf("OutputTokens = %d, want 150", a.OutputTokens)
	}
}

// TestStartOutputWatcher_DrainDeadline verifies the drain loop is bounded
// by shutdownDrainTimeout. If the watcher channel never closes (e.g. a
// stuck producer), the goroutine must still exit within ~1.1s of cancel.
func TestStartOutputWatcher_DrainDeadline(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()

	repo := "test-repo"
	agent := "test-agent"
	d.state.AddRepo(repo, &state.Repository{GithubURL: "https://github.com/test/test"})
	d.state.AddAgent(repo, agent, state.Agent{Type: state.AgentTypeWorker})

	logFile := filepath.Join(d.paths.OutputDir, "drain-deadline.log")
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f, _ := os.Create(logFile)
	f.Close()

	d.startOutputWatcher(repo, agent, logFile)

	// Let the watcher fully attach, then cancel WITHOUT writing any events.
	// The drain loop will have nothing to receive — deadline must fire and
	// return within shutdownDrainTimeout.
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	d.cancel()

	waitDone := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
		elapsed := time.Since(start)
		// Deadline is 1s; tail reader pollInterval is 250ms. Allow generous
		// headroom while still catching a runaway drain.
		if elapsed > 2500*time.Millisecond {
			t.Errorf("drain took %v, want <= 2.5s (deadline is 1s)", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watcher goroutine did not exit within 5s of cancel")
	}
}

// TestStartOutputWatcher_DrainLogMessage is a smoke test: when there are
// buffered events to drain on shutdown, the goroutine must complete
// cleanly (no panic, no leak). We don't assert log content — that matches
// the existing token-tracking test style (see DECISIONS.md S2).
func TestStartOutputWatcher_DrainLogMessage(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()

	repo := "test-repo"
	agent := "test-agent"
	d.state.AddRepo(repo, &state.Repository{GithubURL: "https://github.com/test/test"})
	d.state.AddAgent(repo, agent, state.Agent{Type: state.AgentTypeWorker})

	logFile := filepath.Join(d.paths.OutputDir, "drain-log.log")
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f, _ := os.Create(logFile)
	f.Close()

	d.startOutputWatcher(repo, agent, logFile)

	appendFile, _ := os.OpenFile(logFile, os.O_APPEND|os.O_WRONLY, 0o644)
	line := `[OAT_TOKENS] {"delta_input": 500, "delta_output": 25, "cumulative_input": 500, "cumulative_output": 25}` + "\n"
	appendFile.WriteString(line)
	appendFile.Close()

	time.Sleep(300 * time.Millisecond)
	d.cancel()

	waitDone := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(3 * time.Second):
		t.Fatal("watcher did not exit cleanly")
	}

	// State must reflect the emitted event (either by normal path or drain).
	a, _ := d.state.GetAgent(repo, agent)
	if a.InputTokens != 500 {
		t.Errorf("InputTokens = %d, want 500", a.InputTokens)
	}
}
