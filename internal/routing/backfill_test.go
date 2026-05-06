package routing

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fixedClock returns a deterministic time function for tests.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// recordingCmdRunner captures all `gh` invocations and returns scripted replies.
type recordingCmdRunner struct {
	calls    int32
	response []byte
	err      error
	last     []string // last set of args
}

func (r *recordingCmdRunner) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	atomic.AddInt32(&r.calls, 1)
	r.last = args
	return r.response, r.err
}

func writeMainHistory(t *testing.T, path string, recs []OutcomeRecord) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, r := range recs {
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(f, string(data))
	}
}

func readSidecar(t *testing.T, path string) []PRBackfillEntry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	defer f.Close()
	var entries []PRBackfillEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1*1024*1024)
	for sc.Scan() {
		var e PRBackfillEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("bad sidecar line: %v", err)
		}
		entries = append(entries, e)
	}
	return entries
}

func TestBackfill_NewPRBackfiller_RequiresPaths(t *testing.T) {
	if NewPRBackfiller(PRBackfillerOptions{}) != nil {
		t.Error("empty paths should return nil")
	}
	if NewPRBackfiller(PRBackfillerOptions{HistoryPath: "/x"}) != nil {
		t.Error("missing sidecar path should return nil")
	}
	if NewPRBackfiller(PRBackfillerOptions{SidecarPath: "/y"}) != nil {
		t.Error("missing history path should return nil")
	}
}

func TestBackfill_RunOnce_FetchesAndAppendsForDueBuckets(t *testing.T) {
	dir := t.TempDir()
	historyPath := filepath.Join(dir, "history.jsonl")
	sidecarPath := filepath.Join(dir, "history.backfill.jsonl")

	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	// Record completed 2 hours ago — 1h bucket due, 24h/7d not yet.
	recordTS := now.Add(-2 * time.Hour).Format(time.RFC3339)

	writeMainHistory(t, historyPath, []OutcomeRecord{{
		SchemaVersion: 2,
		TS:            recordTS,
		Repo:          "myrepo",
		Worker:        "azure-badger",
		PRNumber:      42,
		Outcome:       "completed",
	}})

	runner := &recordingCmdRunner{
		response: []byte(`{"state":"OPEN","mergedAt":"","closedAt":""}`),
	}
	bf := NewPRBackfiller(PRBackfillerOptions{
		HistoryPath: historyPath,
		SidecarPath: sidecarPath,
		Now:         fixedClock(now),
		RunCmd:      runner.run,
	})
	if bf == nil {
		t.Fatal("backfiller must be non-nil")
	}

	bf.runOnce(context.Background())

	if got := atomic.LoadInt32(&runner.calls); got != 1 {
		t.Errorf("gh calls: got %d want 1", got)
	}
	entries := readSidecar(t, sidecarPath)
	if len(entries) != 1 {
		t.Fatalf("sidecar entries: got %d want 1", len(entries))
	}
	if entries[0].Snapshot.LagBucket != "1h" {
		t.Errorf("lag_bucket: got %q want 1h", entries[0].Snapshot.LagBucket)
	}
	if entries[0].Snapshot.State != "open" {
		t.Errorf("state: got %q want open (normalized)", entries[0].Snapshot.State)
	}
	if entries[0].RecordKey.Worker != "azure-badger" {
		t.Errorf("record_key.worker: got %q", entries[0].RecordKey.Worker)
	}
}

func TestBackfill_DoesNotRefetchCoveredBuckets(t *testing.T) {
	dir := t.TempDir()
	historyPath := filepath.Join(dir, "history.jsonl")
	sidecarPath := filepath.Join(dir, "history.backfill.jsonl")

	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	recordTS := now.Add(-2 * time.Hour).Format(time.RFC3339)

	writeMainHistory(t, historyPath, []OutcomeRecord{{
		SchemaVersion: 2,
		TS:            recordTS,
		Repo:          "myrepo",
		Worker:        "azure-badger",
		PRNumber:      42,
		Outcome:       "completed",
	}})

	runner := &recordingCmdRunner{
		response: []byte(`{"state":"OPEN","mergedAt":"","closedAt":""}`),
	}
	bf := NewPRBackfiller(PRBackfillerOptions{
		HistoryPath: historyPath,
		SidecarPath: sidecarPath,
		Now:         fixedClock(now),
		RunCmd:      runner.run,
	})

	bf.runOnce(context.Background())
	bf.runOnce(context.Background()) // second tick

	// 1h bucket is now covered. No new gh call.
	if got := atomic.LoadInt32(&runner.calls); got != 1 {
		t.Errorf("gh calls after 2 ticks: got %d want 1", got)
	}
	entries := readSidecar(t, sidecarPath)
	if len(entries) != 1 {
		t.Fatalf("sidecar entries: got %d want 1", len(entries))
	}
}

func TestBackfill_AdvancesThroughBucketsAsTimePasses(t *testing.T) {
	dir := t.TempDir()
	historyPath := filepath.Join(dir, "history.jsonl")
	sidecarPath := filepath.Join(dir, "history.backfill.jsonl")

	t0 := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	recordTS := t0.Format(time.RFC3339)

	writeMainHistory(t, historyPath, []OutcomeRecord{{
		SchemaVersion: 2,
		TS:            recordTS,
		Repo:          "myrepo",
		Worker:        "azure-badger",
		PRNumber:      42,
		Outcome:       "completed",
	}})

	runner := &recordingCmdRunner{
		response: []byte(`{"state":"OPEN","mergedAt":"","closedAt":""}`),
	}
	clock := t0
	bf := NewPRBackfiller(PRBackfillerOptions{
		HistoryPath: historyPath,
		SidecarPath: sidecarPath,
		Now:         func() time.Time { return clock },
		RunCmd:      runner.run,
	})

	// Tick 1 — record is 0h old, no bucket due.
	bf.runOnce(context.Background())

	// Tick 2 — 1h+ later: 1h due.
	clock = t0.Add(2 * time.Hour)
	bf.runOnce(context.Background())

	// Tick 3 — 25h+ later: 24h due.
	clock = t0.Add(25 * time.Hour)
	bf.runOnce(context.Background())

	// Tick 4 — 8d later: 7d due.
	clock = t0.Add(8 * 24 * time.Hour)
	bf.runOnce(context.Background())

	entries := readSidecar(t, sidecarPath)
	if len(entries) != 3 {
		t.Fatalf("sidecar entries: got %d want 3", len(entries))
	}
	wantBuckets := []string{"1h", "24h", "7d"}
	for i, e := range entries {
		if e.Snapshot.LagBucket != wantBuckets[i] {
			t.Errorf("entry[%d] bucket: got %q want %q", i, e.Snapshot.LagBucket, wantBuckets[i])
		}
	}

	// Tick 5 — 9d (still in window). All buckets covered. No new gh call.
	prevCalls := atomic.LoadInt32(&runner.calls)
	clock = t0.Add(9 * 24 * time.Hour)
	bf.runOnce(context.Background())
	if got := atomic.LoadInt32(&runner.calls); got != prevCalls {
		t.Errorf("expected no more gh calls; got %d (was %d)", got, prevCalls)
	}
}

func TestBackfill_SkipsRecordsBeyondMaxAge(t *testing.T) {
	dir := t.TempDir()
	historyPath := filepath.Join(dir, "history.jsonl")
	sidecarPath := filepath.Join(dir, "history.backfill.jsonl")

	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	// 30 days ago — well past 14d cutoff.
	oldTS := now.Add(-30 * 24 * time.Hour).Format(time.RFC3339)

	writeMainHistory(t, historyPath, []OutcomeRecord{{
		SchemaVersion: 2,
		TS:            oldTS,
		Repo:          "myrepo",
		Worker:        "old-worker",
		PRNumber:      1,
		Outcome:       "completed",
	}})

	runner := &recordingCmdRunner{response: []byte(`{"state":"OPEN"}`)}
	bf := NewPRBackfiller(PRBackfillerOptions{
		HistoryPath: historyPath,
		SidecarPath: sidecarPath,
		Now:         fixedClock(now),
		RunCmd:      runner.run,
	})

	bf.runOnce(context.Background())

	if got := atomic.LoadInt32(&runner.calls); got != 0 {
		t.Errorf("expected zero gh calls for stale record, got %d", got)
	}
	if entries := readSidecar(t, sidecarPath); len(entries) != 0 {
		t.Errorf("expected zero sidecar entries, got %d", len(entries))
	}
}

func TestBackfill_SkipsRecordsWithoutPR(t *testing.T) {
	dir := t.TempDir()
	historyPath := filepath.Join(dir, "history.jsonl")
	sidecarPath := filepath.Join(dir, "history.backfill.jsonl")

	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	recordTS := now.Add(-2 * time.Hour).Format(time.RFC3339)

	writeMainHistory(t, historyPath, []OutcomeRecord{{
		SchemaVersion: 2,
		TS:            recordTS,
		Repo:          "myrepo",
		Worker:        "no-pr-worker",
		Outcome:       "removed",
		// No PRNumber, no PRURL — nothing to backfill.
	}})

	runner := &recordingCmdRunner{response: []byte(`{"state":"OPEN"}`)}
	bf := NewPRBackfiller(PRBackfillerOptions{
		HistoryPath: historyPath,
		SidecarPath: sidecarPath,
		Now:         fixedClock(now),
		RunCmd:      runner.run,
	})

	bf.runOnce(context.Background())
	if got := atomic.LoadInt32(&runner.calls); got != 0 {
		t.Errorf("no PR -> no gh: got %d calls", got)
	}
}

func TestBackfill_GhFailureWarnsAndRetriesNextTick(t *testing.T) {
	dir := t.TempDir()
	historyPath := filepath.Join(dir, "history.jsonl")
	sidecarPath := filepath.Join(dir, "history.backfill.jsonl")

	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	recordTS := now.Add(-2 * time.Hour).Format(time.RFC3339)

	writeMainHistory(t, historyPath, []OutcomeRecord{{
		SchemaVersion: 2,
		TS:            recordTS,
		Repo:          "myrepo",
		Worker:        "azure-badger",
		PRNumber:      42,
	}})

	var failingRunner = &recordingCmdRunner{err: fmt.Errorf("network down")}
	var warnings []string
	bf := NewPRBackfiller(PRBackfillerOptions{
		HistoryPath: historyPath,
		SidecarPath: sidecarPath,
		Now:         fixedClock(now),
		RunCmd:      failingRunner.run,
		Warn:        func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) },
	})

	bf.runOnce(context.Background())
	if entries := readSidecar(t, sidecarPath); len(entries) != 0 {
		t.Errorf("expected no sidecar entries on gh failure, got %d", len(entries))
	}
	if len(warnings) == 0 {
		t.Error("expected warn call on gh failure")
	}

	// Recover: next tick succeeds, writes the entry.
	failingRunner.err = nil
	failingRunner.response = []byte(`{"state":"MERGED","mergedAt":"2026-04-28T11:00:00Z"}`)
	bf.runOnce(context.Background())
	entries := readSidecar(t, sidecarPath)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry after recovery, got %d", len(entries))
	}
	if entries[0].Snapshot.State != "merged" {
		t.Errorf("state after recovery: got %q want merged", entries[0].Snapshot.State)
	}
}

func TestBackfill_Run_RespondsToContextCancel(t *testing.T) {
	dir := t.TempDir()
	historyPath := filepath.Join(dir, "history.jsonl")
	sidecarPath := filepath.Join(dir, "history.backfill.jsonl")
	writeMainHistory(t, historyPath, nil)

	bf := NewPRBackfiller(PRBackfillerOptions{
		HistoryPath: historyPath,
		SidecarPath: sidecarPath,
		Interval:    50 * time.Millisecond,
		RunCmd: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return []byte(`{"state":"OPEN"}`), nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		bf.Run(ctx)
		close(done)
	}()

	// Let it tick a few times, then cancel.
	time.Sleep(120 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of cancel")
	}
}

func TestBackfill_NormalizeState(t *testing.T) {
	cases := map[string]string{
		"OPEN":   "open",
		"open":   "open",
		"MERGED": "merged",
		"merged": "merged",
		"CLOSED": "closed",
		"":       "",
		"WEIRD":  "WEIRD", // unknown values pass through verbatim
	}
	for in, want := range cases {
		if got := normalizeState(in); got != want {
			t.Errorf("normalizeState(%q) = %q want %q", in, got, want)
		}
	}
}

func TestBackfill_TolaratesMalformedHistoryLines(t *testing.T) {
	dir := t.TempDir()
	historyPath := filepath.Join(dir, "history.jsonl")
	sidecarPath := filepath.Join(dir, "history.backfill.jsonl")

	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	recordTS := now.Add(-2 * time.Hour).Format(time.RFC3339)

	good, _ := json.Marshal(OutcomeRecord{
		SchemaVersion: 2, TS: recordTS, Repo: "r", Worker: "w", PRNumber: 1,
	})
	body := strings.Join([]string{
		"this is not json",
		string(good),
		"{partial: {",
	}, "\n") + "\n"
	if err := os.WriteFile(historyPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := &recordingCmdRunner{response: []byte(`{"state":"OPEN"}`)}
	bf := NewPRBackfiller(PRBackfillerOptions{
		HistoryPath: historyPath,
		SidecarPath: sidecarPath,
		Now:         fixedClock(now),
		RunCmd:      runner.run,
	})
	bf.runOnce(context.Background())
	// Only the good line should produce a sidecar entry.
	if entries := readSidecar(t, sidecarPath); len(entries) != 1 {
		t.Fatalf("expected 1 entry from 1 good line, got %d", len(entries))
	}
}
