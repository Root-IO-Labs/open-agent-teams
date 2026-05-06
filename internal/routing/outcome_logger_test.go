package routing

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestOutcomeLogger_AppendsRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "routing-history.jsonl")
	l := NewOutcomeLogger(path, nil)

	l.Log(OutcomeRecord{
		Repo:      "r1",
		Worker:    "azure-badger",
		AgentType: "worker",
		TaskText:  "Fix typo",
		Model:     "anthropic:claude-haiku-4-5",
		TokensIn:  1234,
		TokensOut: 56,
		Outcome:   "completed",
	})

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}

	var got OutcomeRecord
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.SchemaVersion != outcomeLoggerSchemaVersion {
		t.Errorf("schema_version want %d, got %d", outcomeLoggerSchemaVersion, got.SchemaVersion)
	}
	if got.Worker != "azure-badger" {
		t.Errorf("worker mismatch: %q", got.Worker)
	}
	if got.TS == "" {
		t.Error("ts should be auto-filled")
	}
	if got.Outcome != "completed" {
		t.Errorf("outcome mismatch: %q", got.Outcome)
	}
}

func TestOutcomeLogger_NoPathIsNoop(t *testing.T) {
	l := NewOutcomeLogger("", nil)
	// Must not panic, must not write anywhere.
	l.Log(OutcomeRecord{Worker: "x"})
}

// Schema v1 record shape, frozen in this test as a fixture. New schema versions
// must continue reading these without dropping any v1 field.
const fixtureV1RecordJSON = `{"schema_version":1,"ts":"2026-04-20T10:00:00Z","repo":"my-repo","worker":"azure-badger","agent_type":"worker","task_text":"Fix typo","model":"anthropic:claude-haiku-4-5","routing_source":"router","started_at":"2026-04-20T09:55:00Z","completed_at":"2026-04-20T10:00:00Z","wall_ms":300000,"tokens_in":1234,"tokens_out":56,"outcome":"completed","pr_number":42,"pr_url":"https://github.com/owner/repo/pull/42","summary":"Fixed it","operator_override":true}`

func TestOutcomeLogger_V1RecordReadableByV2(t *testing.T) {
	// Forward compat: a v1 record on disk decodes into the current
	// OutcomeRecord (v2 reader) preserving every v1 field. New v2 fields
	// stay at their zero values (nil pointers / empty slices / "") so we
	// can distinguish "field not set" from "field set to false".
	var rec OutcomeRecord
	if err := json.Unmarshal([]byte(fixtureV1RecordJSON), &rec); err != nil {
		t.Fatalf("unmarshal v1 fixture: %v", err)
	}

	// Every v1 field present and identical.
	if rec.SchemaVersion != 1 {
		t.Errorf("schema_version: got %d want 1", rec.SchemaVersion)
	}
	if rec.Repo != "my-repo" || rec.Worker != "azure-badger" || rec.AgentType != "worker" {
		t.Errorf("v1 string fields not preserved: %+v", rec)
	}
	if rec.Model != "anthropic:claude-haiku-4-5" || rec.RoutingSource != "router" {
		t.Errorf("v1 routing fields not preserved: %+v", rec)
	}
	if rec.WallMs != 300000 || rec.TokensIn != 1234 || rec.TokensOut != 56 {
		t.Errorf("v1 numeric fields not preserved: %+v", rec)
	}
	if rec.Outcome != "completed" || rec.PRNumber != 42 || rec.Summary != "Fixed it" {
		t.Errorf("v1 outcome fields not preserved: %+v", rec)
	}
	if !rec.OperatorOverride {
		t.Error("v1 OperatorOverride flag not preserved")
	}

	// New v2 fields are at their zero values for v1 input.
	if rec.VerifyPassed != nil {
		t.Errorf("VerifyPassed should be nil for v1 record, got %v", *rec.VerifyPassed)
	}
	if rec.EscalationCount != 0 {
		t.Errorf("EscalationCount should be 0 for v1 record, got %d", rec.EscalationCount)
	}
	if rec.Provider != "" || rec.ModelCanonical != "" {
		t.Errorf("provider/canonical should be empty for v1 record, got (%q, %q)", rec.Provider, rec.ModelCanonical)
	}
	if rec.TaskFeatures != nil {
		t.Errorf("TaskFeatures should be nil for v1 record, got %+v", rec.TaskFeatures)
	}
	if rec.PRStateHistory != nil {
		t.Errorf("PRStateHistory should be nil for v1 record, got %+v", rec.PRStateHistory)
	}
	if rec.PostMergeRevertWithin7d != nil || rec.PostMergeFollowupWithin24h != nil {
		t.Error("post-merge tristate fields should be nil for v1 record")
	}
}

func TestOutcomeLogger_V2RecordIgnoredFieldsByOldReader(t *testing.T) {
	// Backward compat: simulate an "old reader" by decoding v2-shaped JSON
	// into a struct that only has v1 fields. encoding/json silently ignores
	// unknown fields, so old binaries can read new records without error.
	type v1Only struct {
		SchemaVersion int    `json:"schema_version"`
		Repo          string `json:"repo"`
		Worker        string `json:"worker"`
		Model         string `json:"model"`
		Outcome       string `json:"outcome"`
		PRNumber      int    `json:"pr_number,omitempty"`
	}

	verifyPassed := true
	v2 := OutcomeRecord{
		SchemaVersion:   outcomeLoggerSchemaVersion,
		Repo:            "r",
		Worker:          "w",
		Model:           "claude-sonnet-4-6",
		Provider:        "anthropic",
		ModelCanonical:  "claude-sonnet-4-6",
		Outcome:         "completed",
		PRNumber:        99,
		VerifyPassed:    &verifyPassed,
		EscalationCount: 1,
		TaskFeatures:    &LoggedTaskFeatures{CharCount: 50, LineCount: 2},
	}
	data, err := json.Marshal(v2)
	if err != nil {
		t.Fatalf("marshal v2: %v", err)
	}

	var old v1Only
	if err := json.Unmarshal(data, &old); err != nil {
		t.Fatalf("v1 reader rejected v2 record: %v", err)
	}
	if old.SchemaVersion != outcomeLoggerSchemaVersion {
		t.Errorf("schema_version: got %d want %d", old.SchemaVersion, outcomeLoggerSchemaVersion)
	}
	if old.Repo != "r" || old.Worker != "w" || old.Model != "claude-sonnet-4-6" {
		t.Errorf("v1 fields lost in v2->v1 read: %+v", old)
	}
	if old.PRNumber != 99 {
		t.Errorf("pr_number: got %d want 99", old.PRNumber)
	}
}

func TestOutcomeLogger_V2FieldsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "routing-history.jsonl")
	l := NewOutcomeLogger(path, nil)

	verifyTrue := true
	revertFalse := false
	rec := OutcomeRecord{
		Repo:            "r",
		Worker:          "w",
		AgentType:       "worker",
		TaskText:        "Fix the bug in foo.go\n```go\npanic\n```",
		Model:           "anthropic:claude-3-5-sonnet-20241022",
		Provider:        "anthropic",
		ModelCanonical:  "claude-sonnet-3-5",
		Outcome:         "completed",
		VerifyPassed:    &verifyTrue,
		EscalationCount: 2,
		TaskFeatures: &LoggedTaskFeatures{
			CharCount:        40,
			LineCount:        3,
			HasStackTrace:    false,
			CodeBlockCount:   1,
			FilePathMentions: 1,
			ImperativeVerb:   "fix",
		},
		PRStateHistory: []PRStateSnapshot{
			{TS: "2026-04-20T11:00:00Z", State: "open", LagBucket: "1h"},
			{TS: "2026-04-21T10:00:00Z", State: "merged", MergedAt: "2026-04-21T09:30:00Z", LagBucket: "24h"},
		},
		PostMergeRevertWithin7d: &revertFalse,
	}
	l.Log(rec)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got OutcomeRecord
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.SchemaVersion != outcomeLoggerSchemaVersion {
		t.Errorf("schema_version: got %d want %d", got.SchemaVersion, outcomeLoggerSchemaVersion)
	}
	if got.Provider != "anthropic" || got.ModelCanonical != "claude-sonnet-3-5" {
		t.Errorf("canonical fields lost in round trip: %+v", got)
	}
	if got.VerifyPassed == nil || *got.VerifyPassed != true {
		t.Errorf("VerifyPassed lost: %+v", got.VerifyPassed)
	}
	if got.EscalationCount != 2 {
		t.Errorf("EscalationCount: got %d want 2", got.EscalationCount)
	}
	if got.TaskFeatures == nil {
		t.Fatal("TaskFeatures lost")
	}
	if got.TaskFeatures.ImperativeVerb != "fix" || got.TaskFeatures.CodeBlockCount != 1 {
		t.Errorf("TaskFeatures content lost: %+v", got.TaskFeatures)
	}
	if len(got.PRStateHistory) != 2 {
		t.Fatalf("PRStateHistory length: got %d want 2", len(got.PRStateHistory))
	}
	if got.PRStateHistory[1].State != "merged" || got.PRStateHistory[1].LagBucket != "24h" {
		t.Errorf("PRStateHistory entry mangled: %+v", got.PRStateHistory[1])
	}
	if got.PostMergeRevertWithin7d == nil || *got.PostMergeRevertWithin7d != false {
		t.Errorf("PostMergeRevertWithin7d lost: %+v", got.PostMergeRevertWithin7d)
	}
	// Tri-state: not-set field must remain nil through round trip.
	if got.PostMergeFollowupWithin24h != nil {
		t.Errorf("unset tristate should remain nil, got %+v", got.PostMergeFollowupWithin24h)
	}
}

// TestOutcomeLogger_DaemonCompositionEndToEnd mirrors the wiring inside
// daemon.logOutcome (internal/daemon/daemon.go) without needing the full
// daemon test harness: given a "fake agent" representing what the daemon
// would have at completion time, exercise Canonicalize +
// ExtractLoggedTaskFeatures + verify-status mapping, write the record, then
// read it back and assert every v2 field is populated correctly.
//
// If this test breaks after a daemon refactor, the daemon's logOutcome
// composition diverged from the schema's expectations — regenerate the
// composition or update this test in lockstep.
func TestOutcomeLogger_DaemonCompositionEndToEnd(t *testing.T) {
	dir := t.TempDir()

	// Stand up a fake worktree so ExtractLoggedTaskFeatures has something
	// to introspect.
	worktreeDir := filepath.Join(dir, "fakerepo")
	if err := os.MkdirAll(filepath.Join(worktreeDir, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktreeDir, "internal", "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktreeDir, "go.sum"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	// Fake agent fields the daemon would supply.
	const (
		modelID            = "anthropic:claude-3-5-sonnet-20241022"
		taskText           = "Refactor the auth middleware in internal/auth/middleware.go to extract the token-parsing helper. Add tests."
		verificationStatus = "approved"
	)

	// Mirror the daemon's composition.
	canonical, provider := Canonicalize(modelID)
	taskFeatures := ExtractLoggedTaskFeatures(taskText, worktreeDir)
	var verifyPassed *bool
	switch verificationStatus {
	case "approved":
		v := true
		verifyPassed = &v
	case "rejected":
		v := false
		verifyPassed = &v
	}

	historyPath := filepath.Join(dir, "routing-history.jsonl")
	l := NewOutcomeLogger(historyPath, nil)
	rec := OutcomeRecord{
		Repo:           "myrepo",
		Worker:         "azure-badger",
		AgentType:      "worker",
		TaskText:       taskText,
		Model:          modelID,
		Provider:       provider,
		ModelCanonical: canonical,
		RoutingSource:  "router",
		Outcome:        "completed",
		VerifyPassed:   verifyPassed,
		TaskFeatures:   taskFeatures,
	}
	l.Log(rec)

	// Read it back and assert every v2 field is populated.
	data, err := os.ReadFile(historyPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got OutcomeRecord
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.SchemaVersion != outcomeLoggerSchemaVersion {
		t.Errorf("schema_version: got %d want %d", got.SchemaVersion, outcomeLoggerSchemaVersion)
	}
	if got.Provider != "anthropic" {
		t.Errorf("Provider: got %q want anthropic", got.Provider)
	}
	if got.ModelCanonical != "claude-sonnet-3-5" {
		t.Errorf("ModelCanonical: got %q want claude-sonnet-3-5", got.ModelCanonical)
	}
	if got.VerifyPassed == nil || *got.VerifyPassed != true {
		t.Errorf("VerifyPassed: got %v want true", got.VerifyPassed)
	}
	if got.TaskFeatures == nil {
		t.Fatal("TaskFeatures: nil after round-trip")
	}
	if got.TaskFeatures.ImperativeVerb != "refactor" {
		t.Errorf("TaskFeatures.ImperativeVerb: got %q want refactor", got.TaskFeatures.ImperativeVerb)
	}
	if got.TaskFeatures.FilePathMentions == 0 {
		t.Error("TaskFeatures.FilePathMentions: should be > 0 (text mentions middleware.go)")
	}
	if !got.TaskFeatures.HasTestInfra {
		t.Error("TaskFeatures.HasTestInfra: should be true (worktree contains go.sum)")
	}
	if got.TaskFeatures.LangDistribution["go"] == 0 {
		t.Error("TaskFeatures.LangDistribution[go]: should be > 0")
	}

	// Tristate verification: the rejected path produces *bool false.
	verificationStatus2 := "rejected"
	var verifyFailed *bool
	switch verificationStatus2 {
	case "approved":
		v := true
		verifyFailed = &v
	case "rejected":
		v := false
		verifyFailed = &v
	}
	if verifyFailed == nil || *verifyFailed != false {
		t.Errorf("rejected mapping: got %v want pointer-to-false", verifyFailed)
	}

	// Tristate verification: the absent / "pending" path leaves it nil.
	verificationStatus3 := ""
	var verifyAbsent *bool
	switch verificationStatus3 {
	case "approved":
		v := true
		verifyAbsent = &v
	case "rejected":
		v := false
		verifyAbsent = &v
	}
	if verifyAbsent != nil {
		t.Errorf("empty verification status: got %v want nil", verifyAbsent)
	}
}

func TestOutcomeLogger_Concurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "routing-history.jsonl")
	l := NewOutcomeLogger(path, nil)

	var wg sync.WaitGroup
	n := 50
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l.Log(OutcomeRecord{
				Repo:    "r",
				Worker:  "w",
				Model:   "anthropic:claude-haiku-4-5",
				Outcome: "completed",
			})
		}(i)
	}
	wg.Wait()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	count := 0
	for sc.Scan() {
		count++
		var rec OutcomeRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("bad line: %v (line=%q)", err, sc.Text())
		}
	}
	if count != n {
		t.Errorf("want %d lines, got %d", n, count)
	}
}
