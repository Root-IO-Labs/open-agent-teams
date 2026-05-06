package daemon

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/routing"
	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
	"github.com/Root-IO-Labs/open-agent-teams/pkg/config"
)

// TestSchemaV2_E2E_HappyPath simulates the full lifecycle of a worker that
// runs and self-completes successfully, then asserts every schema v2 field
// that should be populated, is. This is the closest thing we have to a real
// user workflow without spinning up an actual oat-agent process.
//
// What it covers:
//   - Daemon construction (oatVersion + pricingSnapshotID snapshotted)
//   - Outcome logger creation (privacy mode resolved)
//   - logOutcome writes a complete v2 record
//   - All v2 fields populated correctly (record_id, oat_version, provider,
//     model_canonical, task_features, prompt metadata, privacy metadata)
//   - DeriveSuccessScore on the on-disk record matches the in-memory result
func TestSchemaV2_E2E_HappyPath(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL:   "https://github.com/test/repo",
			SessionName: "test-session",
			Agents:      make(map[string]state.Agent),
		})
	})
	defer cleanup()

	// The daemon constructor stamped these. They must be non-empty for
	// records to carry release identity.
	if d.oatVersion == "" {
		t.Error("oatVersion not stamped at daemon New()")
	}
	// pricingSnapshotID may be "" if no pricing yaml is loaded; that's allowed.

	// Synthesize an agent that ran a real-looking task. Populate the fields
	// the daemon would have set at spawn (RoutingDecisionReason, candidates,
	// allowlist, prompt hashes).
	taskText := "Refactor the worker pool to use context.WithCancel instead of explicit done channels. Touch internal/daemon/worker.go and internal/daemon/pool.go."
	agent := state.Agent{
		Type:                  state.AgentTypeWorker,
		WindowName:            "swift-otter",
		PID:                   12345,
		CreatedAt:             time.Now().Add(-2 * time.Minute),
		Model:                 "anthropic:claude-3-5-sonnet-20241022",
		RoutingSource:         RoutingSourceRouterAuto,
		RoutingDecisionReason: "complexity=complex floor=9 chose claude-sonnet-3-5",
		RoutingCandidates:     []string{"anthropic:claude-3-5-sonnet-20241022", "anthropic:claude-haiku-4-5"},
		RoutingAllowlist:      []string{"anthropic:claude-3-5-sonnet-20241022", "anthropic:claude-haiku-4-5"},
		Task:                  taskText,
		IssueNumber:           "42",
		PromptSystemHash:      routing.HashPromptText("you are a software engineer"),
		PromptSystemTokens:    routing.EstimateTokens("you are a software engineer"),
		PromptUserHash:        routing.HashPromptText(taskText),
		PromptUserTokens:      routing.EstimateTokens(taskText),
		InputTokens:           45000,
		OutputTokens:          1200,
		CacheReadTokens:       38000,
		CacheCreationTokens:   7000,
		Summary:               "Refactored pool.go and worker.go to use context.WithCancel. Tests pass.",
		PRNumber:              123,
		VerificationStatus:    "approved",
	}
	if err := d.state.AddAgent("test-repo", "swift-otter", agent); err != nil {
		t.Fatalf("AddAgent: %v", err)
	}

	// Drive the completion path the same way handleCompleteAgent would.
	d.logOutcome("test-repo", "swift-otter", agent, "completed", "")

	// Read the on-disk record back.
	rec := readSingleHistoryRecord(t, d.paths.Root)

	// ─── Identity ───────────────────────────────────────────────────────
	if rec.SchemaVersion != 2 {
		t.Errorf("schema_version = %d, want 2", rec.SchemaVersion)
	}
	if rec.RecordID == "" {
		t.Error("record_id not populated")
	}
	if rec.OATVersion == "" {
		t.Error("oat_version not stamped on record")
	}

	// ─── Routing decision context ───────────────────────────────────────
	if rec.DecisionReason != "complexity=complex floor=9 chose claude-sonnet-3-5" {
		t.Errorf("decision_reason = %q, want a populated reason", rec.DecisionReason)
	}
	if len(rec.CandidatesConsidered) != 2 || rec.CandidatesConsidered[0] != "anthropic:claude-3-5-sonnet-20241022" {
		t.Errorf("candidates_considered = %v, want 2-element list with sonnet first", rec.CandidatesConsidered)
	}
	if len(rec.Allowlist) != 2 {
		t.Errorf("allowlist = %v, want 2-element snapshot", rec.Allowlist)
	}

	// ─── Model identity (canonicalization happened) ─────────────────────
	if rec.Provider != "anthropic" {
		t.Errorf("provider = %q, want anthropic", rec.Provider)
	}
	if !strings.Contains(rec.ModelCanonical, "claude-sonnet") {
		t.Errorf("model_canonical = %q, want claude-sonnet-3-5", rec.ModelCanonical)
	}

	// ─── Outcome ────────────────────────────────────────────────────────
	if rec.Outcome != "completed" {
		t.Errorf("outcome = %q, want completed", rec.Outcome)
	}
	if rec.PRNumber != 123 {
		t.Errorf("pr_number = %d, want 123", rec.PRNumber)
	}
	if rec.IssueNum != "42" {
		t.Errorf("issue_number = %q, want 42", rec.IssueNum)
	}
	if rec.VerifyPassed == nil || !*rec.VerifyPassed {
		t.Errorf("verify_passed = %v, want *true (approved)", rec.VerifyPassed)
	}

	// ─── Task features (text-side; worktree is empty here) ──────────────
	if rec.TaskFeatures == nil {
		t.Fatal("task_features not extracted")
	}
	if rec.TaskFeatures.CharCount == 0 {
		t.Error("task_features.char_count not populated")
	}
	if rec.TaskFeatures.ImperativeVerb != "refactor" {
		t.Errorf("imperative_verb = %q, want refactor", rec.TaskFeatures.ImperativeVerb)
	}
	if rec.TaskFeatures.FilePathMentions < 2 {
		t.Errorf("file_path_mentions = %d, want >= 2 (worker.go + pool.go)", rec.TaskFeatures.FilePathMentions)
	}

	// ─── Prompt metadata ────────────────────────────────────────────────
	if rec.Prompt == nil {
		t.Fatal("prompt metadata not populated")
	}
	if len(rec.Prompt.UserMessageHash) != 64 {
		t.Errorf("prompt.user_message_hash length = %d, want 64 (sha256 hex)", len(rec.Prompt.UserMessageHash))
	}
	if rec.Prompt.UserMessageTokens == 0 {
		t.Error("prompt.user_message_tokens not populated")
	}

	// ─── Privacy metadata (default mode = local) ────────────────────────
	if rec.Privacy == nil {
		t.Fatal("privacy metadata not populated")
	}
	if rec.Privacy.UploadConsent != "local" {
		t.Errorf("upload_consent = %q, want local (default)", rec.Privacy.UploadConsent)
	}
	if !rec.Privacy.TaskTextPresent {
		t.Error("task_text_present = false but task_text was non-empty in local mode")
	}
	// task_text on disk must match what we provided (local mode = full text)
	if rec.TaskText != taskText {
		t.Error("task_text on disk does not match input (local mode should preserve)")
	}

	// ─── Success score derivation ───────────────────────────────────────
	score, basis, has := routing.DeriveSuccessScore(rec)
	if !has {
		t.Error("hasScore = false; verifier-approved should be scoreable")
	}
	if score != 0.7 {
		t.Errorf("success_score = %v, want 0.7 (verifier_approved_no_pr)", score)
	}
	if basis != routing.BasisVerifierApprovedNoPR {
		t.Errorf("basis = %q, want verifier_approved_no_pr", basis)
	}
}

// TestSchemaV2_E2E_RemovedWithReason simulates a worker that fails and gets
// removed by the supervisor. Asserts the removal_reason propagates and
// success_score reflects the failure.
func TestSchemaV2_E2E_RemovedWithReason(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL: "https://github.com/test/repo", SessionName: "s",
			Agents: make(map[string]state.Agent),
		})
	})
	defer cleanup()

	agent := state.Agent{
		Type:          state.AgentTypeWorker,
		CreatedAt:     time.Now().Add(-30 * time.Second),
		Model:         "openai:gpt-5.4-mini",
		RoutingSource: RoutingSourceRouterAuto,
		Task:          "Add a --category filter to the list command.",
		FailureReason: "Worker stuck in tool loop after 50 iterations",
	}
	d.state.AddAgent("test-repo", "stuck-falcon", agent) //nolint:errcheck

	d.logOutcome("test-repo", "stuck-falcon", agent, "removed", RemovalReasonFailed)

	rec := readSingleHistoryRecord(t, d.paths.Root)

	if rec.Outcome != "removed" {
		t.Errorf("outcome = %q, want removed", rec.Outcome)
	}
	if rec.RemovalReason != "failed" {
		t.Errorf("removal_reason = %q, want failed", rec.RemovalReason)
	}
	if rec.FailureReason == "" {
		t.Error("failure_reason should propagate from agent.FailureReason")
	}

	score, basis, has := routing.DeriveSuccessScore(rec)
	if !has || score != 0.0 || basis != routing.BasisRemovedFailed {
		t.Errorf("removed-failed scoring: score=%v basis=%q has=%v, want 0.0 / removed_failed / true",
			score, basis, has)
	}
}

// TestSchemaV2_E2E_PrivacyStrictRedactsOnDisk verifies that strict mode
// actually strips raw text from the JSONL written to disk. Critical for
// users who'd be embarrassed to have ~/.oat/routing-history.jsonl
// readable by anyone with shell access.
func TestSchemaV2_E2E_PrivacyStrictRedactsOnDisk(t *testing.T) {
	t.Setenv("OAT_LOG_PRIVACY", "strict")

	d, cleanup := setupTestDaemonWithState(t, func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL: "https://github.com/test/repo", SessionName: "s",
			Agents: make(map[string]state.Agent),
		})
	})
	defer cleanup()

	const secret = "PROPRIETARY-BUSINESS-LOGIC-DETAIL"
	agent := state.Agent{
		Type:      state.AgentTypeWorker,
		CreatedAt: time.Now(),
		Model:     "openai:gpt-5.4-mini",
		Task:      "Implement " + secret + " in the order processor",
		Summary:   "Did the " + secret + " thing",
	}
	d.state.AddAgent("test-repo", "secret-worker", agent) //nolint:errcheck
	d.logOutcome("test-repo", "secret-worker", agent, "completed", "")

	historyPath := filepath.Join(d.paths.Root, "routing-history.jsonl")
	raw, err := os.ReadFile(historyPath)
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Errorf("strict mode wrote secret to disk!\nraw file contents:\n%s", raw)
	}

	rec := readSingleHistoryRecord(t, d.paths.Root)
	if rec.TaskText != "" {
		t.Error("strict mode left task_text on record")
	}
	if rec.Summary != "" {
		t.Error("strict mode left summary on record")
	}
	if rec.Privacy == nil || rec.Privacy.UploadConsent != "strict" {
		t.Errorf("privacy.upload_consent = %v, want strict", rec.Privacy)
	}
	// Hashes should be preserved across all modes (privacy-safe by construction).
	// Note: PromptHashes are computed at SPAWN time (in startAgentConfig) and
	// stored on state.Agent before logOutcome runs. The test sets agent fields
	// directly without hashes, so they're empty here — that's fine, the redaction
	// is what matters.
}

// TestSchemaV2_E2E_KillSwitchSilences verifies OAT_OUTCOME_LOG=off skips
// every write completely. No file should be created.
func TestSchemaV2_E2E_KillSwitchSilences(t *testing.T) {
	t.Setenv("OAT_OUTCOME_LOG", "off")

	d, cleanup := setupTestDaemonWithState(t, func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL: "https://github.com/test/repo", SessionName: "s",
			Agents: make(map[string]state.Agent),
		})
	})
	defer cleanup()

	if d.outcomeLogger != nil {
		t.Error("OAT_OUTCOME_LOG=off should yield nil outcomeLogger")
	}

	agent := state.Agent{Type: state.AgentTypeWorker, Model: "x", Task: "y"}
	d.state.AddAgent("test-repo", "ghost", agent)              //nolint:errcheck
	d.logOutcome("test-repo", "ghost", agent, "completed", "") // should be a no-op

	historyPath := filepath.Join(d.paths.Root, "routing-history.jsonl")
	if _, err := os.Stat(historyPath); err == nil {
		t.Error("kill switch active but history file was created")
	}
}

// TestSchemaV2_E2E_MigrationOnDaemonStart writes a v1 corpus before
// daemon.New() and asserts the daemon migrates it on construction.
func TestSchemaV2_E2E_MigrationOnDaemonStart(t *testing.T) {
	tmp := t.TempDir()
	historyPath := filepath.Join(tmp, "routing-history.jsonl")
	v1 := `{"schema_version":1,"ts":"2026-04-15T10:00:00Z","repo":"alpha","worker":"old-worker","agent_type":"worker","task_text":"Fix the typo","model":"anthropic:claude-3-5-sonnet-20241022","outcome":"completed"}`
	if err := os.WriteFile(historyPath, []byte(v1+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Construct the daemon with paths pointing at our seeded tmp.
	paths := pathsAt(tmp)
	if err := paths.EnsureDirectories(); err != nil {
		t.Fatal(err)
	}
	d, err := New(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Stop() //nolint:errcheck

	// Verify the on-disk file is now v2.
	rec := readSingleHistoryRecord(t, tmp)
	if rec.SchemaVersion != 2 {
		t.Errorf("post-migration schema_version = %d, want 2", rec.SchemaVersion)
	}
	if rec.RecordID == "" {
		t.Error("record_id not populated by migration")
	}
	if rec.Provider != "anthropic" {
		t.Errorf("provider = %q after migration, want anthropic", rec.Provider)
	}
	if rec.OATVersion != "v1-migrated" {
		t.Errorf("oat_version = %q, want v1-migrated sentinel", rec.OATVersion)
	}

	// Backup must exist.
	backupPath := historyPath + ".v1.bak.jsonl"
	if _, err := os.Stat(backupPath); err != nil {
		t.Errorf("backup not created: %v", err)
	}
}

// TestSchemaV2_E2E_BackfillSidecarJoinFlipsScoreToMerged verifies the
// recovered-worker case end-to-end: a worker is logged as outcome=removed
// initially (score=0.0), then the backfill sidecar observes the PR as merged,
// and the joined reader produces score=1.0.
//
// This test catches the most subtle ship-blocker: the backfill data exists
// but was invisible to the report. Without LoadCorpusJoined, this scoring
// flip never happens — the corpus says "0.0 forever" even when the work
// actually succeeded.
func TestSchemaV2_E2E_BackfillSidecarJoinFlipsScoreToMerged(t *testing.T) {
	d, cleanup := setupTestDaemonWithState(t, func(s *state.State) {
		s.AddRepo("test-repo", &state.Repository{
			GithubURL: "https://github.com/test/repo", SessionName: "s",
			Agents: make(map[string]state.Agent),
		})
	})
	defer cleanup()

	// 1. Worker fails and is logged as outcome=removed/failed
	agent := state.Agent{
		Type:      state.AgentTypeWorker,
		CreatedAt: time.Now().Add(-1 * time.Hour),
		Model:     "anthropic:claude-haiku-4-5",
		Task:      "Fix the auth bug",
		PRNumber:  77,
	}
	d.state.AddAgent("test-repo", "tired-bee", agent) //nolint:errcheck
	d.logOutcome("test-repo", "tired-bee", agent, "removed", RemovalReasonFailed)

	// 2. Sidecar observation: PR eventually merged at 24h lag.
	//    We synthesize the entry directly because the live backfiller would
	//    require gh + waiting 24h. The schema is what we're testing here.
	historyPath := filepath.Join(d.paths.Root, "routing-history.jsonl")
	sidecarPath := filepath.Join(d.paths.Root, "routing-history.backfill.jsonl")

	main := readSingleHistoryRecord(t, d.paths.Root)
	sidecarLine := routing.PRBackfillEntry{
		SchemaVersion: 1,
		SnapshotTS:    time.Now().UTC().Format(time.RFC3339),
		RecordKey: routing.OutcomeRecordKey{
			TS:     main.TS,
			Worker: main.Worker,
			Repo:   main.Repo,
		},
		Snapshot: routing.PRStateSnapshot{
			State:     "merged",
			LagBucket: "24h",
			MergedAt:  time.Now().UTC().Format(time.RFC3339),
		},
	}
	sidecarBytes, err := json.Marshal(sidecarLine)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sidecarPath, append(sidecarBytes, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	// 3. Read via the joined reader.
	records, _, err := routing.LoadCorpusJoined(historyPath, sidecarPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	rec := records[0]

	// 4. The joined record should have the merge observation folded in.
	if len(rec.PRStateHistory) != 1 {
		t.Errorf("PRStateHistory after join = %d, want 1", len(rec.PRStateHistory))
	}

	// 5. CRITICAL: success_score must flip from 0.0 (removed/failed) to
	//    1.0 (pr_merged). This is the bug the join fix catches.
	score, basis, has := routing.DeriveSuccessScore(rec)
	if !has {
		t.Fatal("score not derivable after join")
	}
	if score != 1.0 || basis != routing.BasisPRMerged {
		t.Errorf("post-join score = %v / %q, want 1.0 / pr_merged. "+
			"This means the corpus_join didn't flip the recovered-worker case correctly.",
			score, basis)
	}

	// 6. Without the join (reading main file alone), score is the original
	//    failure value. Belt-and-braces check that the join is what's
	//    making the difference — not some accidental field on the record.
	plainRec := readSingleHistoryRecord(t, d.paths.Root)
	plainScore, plainBasis, _ := routing.DeriveSuccessScore(plainRec)
	if plainScore != 0.0 || plainBasis != routing.BasisRemovedFailed {
		t.Errorf("pre-join score = %v / %q, want 0.0 / removed_failed (the bug-of-omission case)",
			plainScore, plainBasis)
	}
}

// readSingleHistoryRecord opens routing-history.jsonl in the given oat root
// and returns the first parsed record. Fails the test if there are zero
// records or if any line is malformed.
func readSingleHistoryRecord(t *testing.T, oatRoot string) routing.OutcomeRecord {
	t.Helper()
	historyPath := filepath.Join(oatRoot, "routing-history.jsonl")
	f, err := os.Open(historyPath)
	if err != nil {
		t.Fatalf("open %s: %v", historyPath, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		t.Fatalf("history file empty: %s", historyPath)
	}
	var rec routing.OutcomeRecord
	if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
		t.Fatalf("parse line: %v\nline: %s", err, sc.Bytes())
	}
	return rec
}

// pathsAt returns a config.Paths rooted at dir, suitable for tests that
// need to seed files BEFORE constructing the daemon (which is when the
// migration runs). Mirrors the inline shape used by setupTestDaemonWithState.
func pathsAt(dir string) *config.Paths {
	return &config.Paths{
		Root:         dir,
		BinDir:       filepath.Join(dir, "bin"),
		DaemonPID:    filepath.Join(dir, "daemon.pid"),
		DaemonSock:   filepath.Join(dir, "daemon.sock"),
		DaemonLog:    filepath.Join(dir, "daemon.log"),
		StateFile:    filepath.Join(dir, "state.json"),
		ReposDir:     filepath.Join(dir, "repos"),
		WorktreesDir: filepath.Join(dir, "wts"),
		MessagesDir:  filepath.Join(dir, "messages"),
		OutputDir:    filepath.Join(dir, "output"),
		ArchiveDir:   filepath.Join(dir, "archive"),
	}
}
