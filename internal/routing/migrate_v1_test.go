package routing

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrateV1ToV2_NoFileIsNoop(t *testing.T) {
	tmp := t.TempDir()
	stats, err := MigrateV1ToV2(filepath.Join(tmp, "missing.jsonl"))
	if err != nil {
		t.Fatalf("missing file should be silent: %v", err)
	}
	if stats.TotalLines != 0 || stats.V1Migrated != 0 || stats.BackupCreated {
		t.Errorf("missing file produced non-zero stats: %+v", stats)
	}
}

func TestMigrateV1ToV2_EnrichesV1Records(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "history.jsonl")

	// Two v1 records (no schema_version field, or =1).
	v1A := `{"schema_version":1,"ts":"2026-04-15T10:00:00Z","repo":"alpha","worker":"swift-otter","agent_type":"worker","task_text":"Fix the typo 'recieve' in cli.py","model":"anthropic:claude-3-5-sonnet-20241022","outcome":"completed"}`
	v1B := `{"ts":"2026-04-15T11:00:00Z","repo":"beta","worker":"prime-falcon","agent_type":"worker","task_text":"","model":"openai:gpt-5.4-mini","outcome":"removed"}`
	if err := os.WriteFile(path, []byte(v1A+"\n"+v1B+"\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	stats, err := MigrateV1ToV2(path)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if stats.V1Migrated != 2 {
		t.Errorf("V1Migrated = %d, want 2", stats.V1Migrated)
	}
	if !stats.BackupCreated {
		t.Error("expected backup created on first migration")
	}

	// Backup must exist and contain the original content.
	backupBytes, err := os.ReadFile(stats.BackupPath)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if !strings.Contains(string(backupBytes), "schema_version\":1") {
		t.Error("backup should contain v1 records verbatim")
	}

	// Migrated file: every record at v2, with derivable fields populated.
	out, err := os.Open(path)
	if err != nil {
		t.Fatalf("open migrated: %v", err)
	}
	defer out.Close()
	sc := bufio.NewScanner(out)
	count := 0
	for sc.Scan() {
		count++
		var rec OutcomeRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("parse migrated line: %v", err)
		}
		if rec.SchemaVersion != 2 {
			t.Errorf("schema_version = %d, want 2", rec.SchemaVersion)
		}
		if rec.RecordID == "" {
			t.Error("record_id empty after migration")
		}
		if rec.OATVersion != "v1-migrated" {
			t.Errorf("oat_version = %q, want %q", rec.OATVersion, "v1-migrated")
		}
		if rec.Provider == "" {
			t.Error("provider should be derived from model")
		}
		if rec.ModelCanonical == "" {
			t.Error("model_canonical should be derived from model")
		}
	}
	if count != 2 {
		t.Errorf("migrated line count = %d, want 2", count)
	}
}

func TestMigrateV1ToV2_Idempotent(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "history.jsonl")
	v1 := `{"schema_version":1,"ts":"2026-04-15T10:00:00Z","repo":"alpha","worker":"x","model":"openai:gpt-5.4-mini","outcome":"completed","task_text":"Fix it"}`
	os.WriteFile(path, []byte(v1+"\n"), 0o644) //nolint:errcheck

	first, err := MigrateV1ToV2(path)
	if err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	contentAfterFirst, _ := os.ReadFile(path)

	second, err := MigrateV1ToV2(path)
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	contentAfterSecond, _ := os.ReadFile(path)

	if string(contentAfterFirst) != string(contentAfterSecond) {
		t.Errorf("not idempotent — content differs between runs")
	}
	if first.V1Migrated != 1 {
		t.Errorf("first run: V1Migrated = %d, want 1", first.V1Migrated)
	}
	if second.V1Migrated != 0 || second.V2Passthrough != 1 {
		t.Errorf("second run: V1Migrated=%d V2Passthrough=%d, want 0/1", second.V1Migrated, second.V2Passthrough)
	}
	if !first.BackupCreated {
		t.Error("first run should have created backup")
	}
	if second.BackupCreated {
		t.Error("second run should NOT re-create backup")
	}
}

func TestMigrateV1ToV2_RecordIDStableAcrossRuns(t *testing.T) {
	// Run migration on the same v1 record from two different temp files.
	// The derived record_id must match, because we use UUIDv5.
	v1 := `{"schema_version":1,"ts":"2026-04-15T10:00:00Z","repo":"alpha","worker":"swift-otter","agent_type":"worker","model":"openai:gpt-5.4-mini","outcome":"completed","task_text":"x"}`

	dirA := t.TempDir()
	pathA := filepath.Join(dirA, "history.jsonl")
	os.WriteFile(pathA, []byte(v1+"\n"), 0o644) //nolint:errcheck
	if _, err := MigrateV1ToV2(pathA); err != nil {
		t.Fatalf("A: %v", err)
	}

	dirB := t.TempDir()
	pathB := filepath.Join(dirB, "history.jsonl")
	os.WriteFile(pathB, []byte(v1+"\n"), 0o644) //nolint:errcheck
	if _, err := MigrateV1ToV2(pathB); err != nil {
		t.Fatalf("B: %v", err)
	}

	bytesA, _ := os.ReadFile(pathA)
	bytesB, _ := os.ReadFile(pathB)

	var recA, recB OutcomeRecord
	if err := json.Unmarshal(bytesA[:len(bytesA)-1], &recA); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(bytesB[:len(bytesB)-1], &recB); err != nil {
		t.Fatal(err)
	}
	if recA.RecordID != recB.RecordID {
		t.Errorf("record_id not stable across migrations: %s vs %s", recA.RecordID, recB.RecordID)
	}
}

func TestMigrateV1ToV2_PreservesUnparseableLines(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "history.jsonl")
	v1 := `{"schema_version":1,"ts":"2026-04-15T10:00:00Z","repo":"alpha","worker":"x","model":"openai:gpt-5.4-mini","outcome":"completed","task_text":"x"}`
	junk := `not-a-json-line-at-all`
	if err := os.WriteFile(path, []byte(v1+"\n"+junk+"\n"+v1+"\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	stats, err := MigrateV1ToV2(path)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if stats.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", stats.Skipped)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read out: %v", err)
	}
	if !strings.Contains(string(out), junk) {
		t.Error("unparseable line was dropped — must be preserved verbatim")
	}
}

func TestMigrateV1ToV2_AlreadyV2Untouched(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "history.jsonl")
	v2 := `{"schema_version":2,"record_id":"00000000-0000-7000-8000-000000000000","ts":"2026-04-15T10:00:00Z","repo":"alpha","worker":"x","model":"openai:gpt-5.4-mini","outcome":"completed","task_text":"x","oat_version":"v0.1.0 (abcdef0)"}`
	original := []byte(v2 + "\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	stats, err := MigrateV1ToV2(path)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if stats.V1Migrated != 0 {
		t.Errorf("V1Migrated = %d, want 0", stats.V1Migrated)
	}
	if stats.BackupCreated {
		t.Error("v2-only file should not create backup")
	}

	after, _ := os.ReadFile(path)
	if string(after) != string(original) {
		t.Errorf("v2-only file was rewritten\nbefore: %s\nafter:  %s", original, after)
	}
}

func TestMigrateV1ToV2_TempFileCleanedOnError(t *testing.T) {
	// Create a file we can read but the parent dir blocks .tmp creation by
	// being read-only. We test that no .v2.tmp file lingers after a failure.
	// On platforms where chmod 0o555 doesn't block creation in the dir,
	// this test is informational — skip cleanly.
	tmp := t.TempDir()
	path := filepath.Join(tmp, "history.jsonl")
	v1 := `{"schema_version":1,"ts":"x","repo":"r","worker":"w","model":"m","outcome":"o","task_text":"t"}`
	os.WriteFile(path, []byte(v1+"\n"), 0o644) //nolint:errcheck

	if err := os.Chmod(tmp, 0o555); err != nil {
		t.Skip("cannot chmod parent dir read-only")
	}
	defer os.Chmod(tmp, 0o755) //nolint:errcheck

	if _, err := MigrateV1ToV2(path); err == nil {
		// Some filesystems allow tmp creation despite 0o555 — that's fine,
		// this test just checks the cleanup branch when error occurs.
		t.Skip("filesystem allowed tmp creation; cleanup branch not exercised")
	}
	// Confirm no orphaned .tmp file
	if _, err := os.Stat(path + ".v2.tmp"); err == nil {
		t.Error(".v2.tmp file lingered after migration failure")
	}
}
