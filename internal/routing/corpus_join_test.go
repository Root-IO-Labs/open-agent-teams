package routing

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadCorpusJoined_NoMainFile(t *testing.T) {
	tmp := t.TempDir()
	recs, errs, err := LoadCorpusJoined(
		filepath.Join(tmp, "missing.jsonl"),
		filepath.Join(tmp, "missing.backfill.jsonl"),
	)
	if err != nil {
		t.Fatalf("missing main: want silent, got %v", err)
	}
	if len(recs) != 0 || len(errs) != 0 {
		t.Errorf("expected empty result; got %d recs, %d errs", len(recs), len(errs))
	}
}

func TestLoadCorpusJoined_MissingSidecarReturnsMainRecords(t *testing.T) {
	tmp := t.TempDir()
	mainPath := filepath.Join(tmp, "history.jsonl")
	rec := OutcomeRecord{
		SchemaVersion: 2,
		TS:            "2026-04-15T10:00:00Z",
		Repo:          "alpha",
		Worker:        "worker-1",
		Model:         "openai:gpt-5.4-mini",
		PRNumber:      42,
		Outcome:       "completed",
	}
	writeJSONL(t, mainPath, rec)

	recs, _, err := LoadCorpusJoined(mainPath, filepath.Join(tmp, "missing-sidecar.jsonl"))
	if err != nil {
		t.Fatalf("expected silent on missing sidecar: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d recs, want 1", len(recs))
	}
	if len(recs[0].PRStateHistory) != 0 {
		t.Errorf("PRStateHistory should be empty when sidecar missing; got %v", recs[0].PRStateHistory)
	}
}

func TestLoadCorpusJoined_FoldsSidecarObservations(t *testing.T) {
	tmp := t.TempDir()
	mainPath := filepath.Join(tmp, "history.jsonl")
	sidecarPath := filepath.Join(tmp, "history.backfill.jsonl")

	rec := OutcomeRecord{
		SchemaVersion: 2,
		TS:            "2026-04-15T10:00:00Z",
		Repo:          "alpha",
		Worker:        "worker-1",
		Model:         "openai:gpt-5.4-mini",
		PRNumber:      42,
		Outcome:       "removed", // initially failed
		RemovalReason: "failed",
	}
	writeJSONL(t, mainPath, rec)

	// Sidecar: 1h "open", 24h "merged"
	writeJSONL(t, sidecarPath,
		PRBackfillEntry{
			SchemaVersion: 1,
			SnapshotTS:    "2026-04-15T11:00:00Z",
			RecordKey:     OutcomeRecordKey{TS: rec.TS, Worker: rec.Worker, Repo: rec.Repo},
			Snapshot:      PRStateSnapshot{State: "open", LagBucket: "1h"},
		},
		PRBackfillEntry{
			SchemaVersion: 1,
			SnapshotTS:    "2026-04-16T10:00:00Z",
			RecordKey:     OutcomeRecordKey{TS: rec.TS, Worker: rec.Worker, Repo: rec.Repo},
			Snapshot:      PRStateSnapshot{State: "merged", LagBucket: "24h", MergedAt: "2026-04-16T09:30:00Z"},
		},
	)

	recs, _, err := LoadCorpusJoined(mainPath, sidecarPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d recs, want 1", len(recs))
	}
	if len(recs[0].PRStateHistory) != 2 {
		t.Errorf("expected 2 sidecar observations folded in; got %d", len(recs[0].PRStateHistory))
	}

	// CRITICAL: success_score must reflect the merged observation, not the
	// initial outcome=removed/failed. This is the bug we're fixing.
	score, basis, has := DeriveSuccessScore(recs[0])
	if !has {
		t.Fatal("hasScore = false after sidecar fold; expected scoreable")
	}
	if score != 1.0 || basis != BasisPRMerged {
		t.Errorf("post-fold score: got %v / %q, want 1.0 / pr_merged. "+
			"This is the regression that the corpus_join fix prevents.",
			score, basis)
	}
}

func TestLoadCorpusJoined_PreservesMultipleRecords(t *testing.T) {
	tmp := t.TempDir()
	mainPath := filepath.Join(tmp, "history.jsonl")
	sidecarPath := filepath.Join(tmp, "history.backfill.jsonl")

	rec1 := OutcomeRecord{TS: "2026-04-15T10:00:00Z", Repo: "alpha", Worker: "w1", PRNumber: 1, Outcome: "completed"}
	rec2 := OutcomeRecord{TS: "2026-04-15T11:00:00Z", Repo: "alpha", Worker: "w2", PRNumber: 2, Outcome: "removed", RemovalReason: "failed"}
	rec3 := OutcomeRecord{TS: "2026-04-15T12:00:00Z", Repo: "beta", Worker: "w3", PRNumber: 3, Outcome: "completed"}
	writeJSONL(t, mainPath, rec1, rec2, rec3)

	// Only rec2 has a backfill observation (it's the one that recovered)
	writeJSONL(t, sidecarPath, PRBackfillEntry{
		SchemaVersion: 1,
		RecordKey:     OutcomeRecordKey{TS: rec2.TS, Worker: rec2.Worker, Repo: rec2.Repo},
		Snapshot:      PRStateSnapshot{State: "merged", LagBucket: "24h"},
	})

	recs, _, err := LoadCorpusJoined(mainPath, sidecarPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 {
		t.Fatalf("got %d recs, want 3", len(recs))
	}
	// rec1 and rec3: no sidecar entry, so PRStateHistory empty
	if len(recs[0].PRStateHistory) != 0 {
		t.Errorf("rec1 should have no sidecar fold; got %v", recs[0].PRStateHistory)
	}
	if len(recs[2].PRStateHistory) != 0 {
		t.Errorf("rec3 should have no sidecar fold; got %v", recs[2].PRStateHistory)
	}
	// rec2: has the merged observation
	if len(recs[1].PRStateHistory) != 1 || recs[1].PRStateHistory[0].State != "merged" {
		t.Errorf("rec2 should have merged obs; got %v", recs[1].PRStateHistory)
	}
	// Order preserved
	if recs[0].Worker != "w1" || recs[1].Worker != "w2" || recs[2].Worker != "w3" {
		t.Errorf("record order changed: %s %s %s", recs[0].Worker, recs[1].Worker, recs[2].Worker)
	}
}

func TestLoadCorpusJoined_TolerantOfMalformedSidecar(t *testing.T) {
	tmp := t.TempDir()
	mainPath := filepath.Join(tmp, "history.jsonl")
	sidecarPath := filepath.Join(tmp, "history.backfill.jsonl")

	rec := OutcomeRecord{TS: "x", Worker: "w", Repo: "r", PRNumber: 1}
	writeJSONL(t, mainPath, rec)

	// Sidecar with one bad line and one good line.
	good := PRBackfillEntry{
		SchemaVersion: 1,
		RecordKey:     OutcomeRecordKey{TS: "x", Worker: "w", Repo: "r"},
		Snapshot:      PRStateSnapshot{State: "merged", LagBucket: "1h"},
	}
	goodBytes, _ := json.Marshal(good)
	if err := os.WriteFile(sidecarPath, []byte("not-json\n"+string(goodBytes)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	recs, _, err := LoadCorpusJoined(mainPath, sidecarPath)
	if err != nil {
		t.Fatalf("malformed sidecar should be silent; got %v", err)
	}
	if len(recs[0].PRStateHistory) != 1 {
		t.Errorf("good observation dropped; got %v", recs[0].PRStateHistory)
	}
}

// writeJSONL writes one or more values as JSON lines to path. Test helper.
func writeJSONL(t *testing.T, path string, vals ...any) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, v := range vals {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintln(f, string(b)); err != nil {
			t.Fatal(err)
		}
	}
}
