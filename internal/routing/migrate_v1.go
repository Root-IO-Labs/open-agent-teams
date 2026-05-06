package routing

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// migrateRecordIDNamespace pins the UUIDv5 namespace used to derive stable
// record_ids for v1 records during migration. UUIDv5 is content-derived
// (namespace + name → deterministic UUID), so re-running migration on the
// same v1 record produces the same record_id every time.
//
// Pinned to a fresh, project-specific UUIDv4. Do NOT change this value —
// changing it re-keys every migrated record, breaking dedup and any
// downstream join that uses record_id.
var migrateRecordIDNamespace = uuid.MustParse("4d6f6967-7261-7465-2d76-3120746f2076")

// MigrateV1Stats summarizes the outcome of one migration pass. Returned
// from MigrateV1ToV2 so callers can log how many records moved.
type MigrateV1Stats struct {
	TotalLines    int    // total non-empty lines read
	V1Migrated    int    // records lifted from v1 to v2
	V2Passthrough int    // records already at v2; copied verbatim
	Skipped       int    // unparseable lines (preserved verbatim)
	BackupCreated bool   // true if backup was written this pass
	BackupPath    string // path to the .v1.bak.jsonl backup
	OutputPath    string // path to the resulting (v2-migrated) file
}

// MigrateV1ToV2 lifts a routing-history.jsonl file from schema v1 to v2
// in place. Idempotent: re-running on an already-migrated file is a no-op
// (every line is already v2; no backup re-created).
//
// Safety contract:
//  1. The input file is never deleted. The original is moved to
//     <path>.v1.bak.jsonl exactly once (the first time migration runs).
//     Subsequent runs leave the backup alone.
//  2. The new content is written to <path>.tmp first, then renamed atomically.
//     A crash mid-write leaves the original untouched.
//  3. Unparseable lines are preserved verbatim. We never silently drop data.
//  4. Migration enriches with derivable v2 fields only. Fields that need
//     backfill (pr_state_history, post_merge_*) stay empty until the
//     backfill goroutine fills them.
//
// Returns nil error and a stats struct describing what happened. If the
// input file doesn't exist, returns nil error and zero stats — fresh
// installs are a normal case.
func MigrateV1ToV2(historyPath string) (MigrateV1Stats, error) {
	stats := MigrateV1Stats{OutputPath: historyPath}

	if strings.Contains(historyPath, "..") {
		return stats, fmt.Errorf("invalid file path")
	}

	in, err := os.Open(historyPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return stats, nil // fresh install — nothing to migrate
		}
		return stats, fmt.Errorf("open history: %w", err)
	}

	// Read all lines first so we can decide whether to bother writing.
	// History files are bounded by the user's workflow volume; in practice
	// well under 10MB, so reading the whole file is fine.
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	type line struct {
		raw string
		rec *OutcomeRecord
	}
	var lines []line
	for scanner.Scan() {
		raw := scanner.Text()
		if raw == "" {
			continue
		}
		stats.TotalLines++
		var rec OutcomeRecord
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			lines = append(lines, line{raw: raw}) // preserve verbatim
			stats.Skipped++
			continue
		}
		lines = append(lines, line{raw: raw, rec: &rec})
	}
	if err := scanner.Err(); err != nil {
		in.Close()
		return stats, fmt.Errorf("scan history: %w", err)
	}
	in.Close()

	// Count migration candidates before deciding whether to write.
	candidates := 0
	for _, l := range lines {
		if l.rec != nil && l.rec.SchemaVersion < outcomeLoggerSchemaVersion {
			candidates++
		}
	}
	if candidates == 0 {
		// Already-migrated or parse-only file — nothing to do.
		stats.V2Passthrough = stats.TotalLines - stats.Skipped
		return stats, nil
	}

	// Backup first (idempotent — only writes if backup doesn't exist).
	backupPath := historyPath + ".v1.bak.jsonl"
	if _, err := os.Stat(backupPath); errors.Is(err, os.ErrNotExist) {
		if err := copyFile(historyPath, backupPath); err != nil {
			return stats, fmt.Errorf("backup: %w", err)
		}
		stats.BackupCreated = true
	}
	stats.BackupPath = backupPath

	// Write enriched content to a temp file, then atomic rename.
	tmpPath := historyPath + ".v2.tmp"
	tmp, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return stats, fmt.Errorf("create tmp: %w", err)
	}
	w := bufio.NewWriter(tmp)
	for _, l := range lines {
		if l.rec == nil {
			// Unparseable — preserve verbatim.
			if _, err := fmt.Fprintln(w, l.raw); err != nil {
				tmp.Close()
				os.Remove(tmpPath)
				return stats, fmt.Errorf("write tmp: %w", err)
			}
			continue
		}
		if l.rec.SchemaVersion < outcomeLoggerSchemaVersion {
			enrichV1ToV2(l.rec)
			stats.V1Migrated++
		} else {
			stats.V2Passthrough++
		}
		out, err := json.Marshal(l.rec)
		if err != nil {
			// Shouldn't happen — every input we accept is round-trippable.
			tmp.Close()
			os.Remove(tmpPath)
			return stats, fmt.Errorf("marshal record: %w", err)
		}
		if _, err := fmt.Fprintln(w, string(out)); err != nil {
			tmp.Close()
			os.Remove(tmpPath)
			return stats, fmt.Errorf("write record: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return stats, fmt.Errorf("flush tmp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return stats, fmt.Errorf("sync tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return stats, fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(tmpPath, historyPath); err != nil {
		os.Remove(tmpPath)
		return stats, fmt.Errorf("rename tmp: %w", err)
	}
	return stats, nil
}

// enrichV1ToV2 mutates a v1 record in place to v2 shape. Only adds derivable
// fields; backfill-only fields (pr_state_history, post_merge_*) stay empty
// for the backfill goroutine to populate later.
func enrichV1ToV2(rec *OutcomeRecord) {
	rec.SchemaVersion = outcomeLoggerSchemaVersion

	// Stable record_id derived from the record's natural key. Re-running
	// migration on the same record yields the same UUID.
	if rec.RecordID == "" {
		key := rec.TS + "|" + rec.Repo + "|" + rec.Worker + "|" + rec.AgentType
		rec.RecordID = uuid.NewSHA1(migrateRecordIDNamespace, []byte(key)).String()
	}

	// oat_version unknown for v1 — pin to a sentinel so analyses can filter
	// "v1-migrated" records explicitly. Real v2 records have a populated
	// oat_version from the writing daemon.
	if rec.OATVersion == "" {
		rec.OATVersion = "v1-migrated"
	}

	// Provider/canonical: derivable from the existing Model field.
	if rec.Provider == "" || rec.ModelCanonical == "" {
		canonical, provider := Canonicalize(rec.Model)
		if rec.Provider == "" {
			rec.Provider = provider
		}
		if rec.ModelCanonical == "" {
			rec.ModelCanonical = canonical
		}
	}

	// Task features: re-extract from text if absent. Worktree path is
	// unknown post-hoc, so worktree features stay empty (LangDistribution,
	// repo size, etc.). The text-side features (length, code blocks,
	// stack-trace detection, file mentions, imperative verb) populate.
	if rec.TaskFeatures == nil && rec.TaskText != "" {
		rec.TaskFeatures = ExtractLoggedTaskFeatures(rec.TaskText, "")
	}
}

// copyFile copies src to dst byte-for-byte. Used for the v1 backup —
// not a rename, because we want the original at historyPath to be
// readable by an in-flight backfill while migration runs.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}
