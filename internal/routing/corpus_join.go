package routing

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
)

// LoadCorpusJoined reads the main routing-history.jsonl AND the backfill
// sidecar, then folds sidecar observations into each record's PRStateHistory
// before returning. This is the canonical reader for any caller that needs
// up-to-date success signals — the report, the share payload, future ML
// training pipelines.
//
// Why a separate joiner: the main file is strictly append-only and immutable
// (logger only appends; backfiller writes elsewhere). The sidecar
// architecture keeps the canonical record from being mutated, but readers
// have to do the join themselves. Failing to join means the backfilled
// observations are invisible — every "did the PR merge?" query returns
// false even when the merge was observed.
//
// Behavior:
//   - Reads main file: records returned in original order
//   - Reads sidecar (best-effort: missing file is silent, malformed lines skipped)
//   - For each record, looks up sidecar entries by (ts, worker, repo) and
//     appends them to rec.PRStateHistory in the order they were observed
//   - Returns parse errors from the main file separately so callers can
//     report them without polluting the records list
//   - Sidecar parse errors are silent (the sidecar is operator-debug; we don't
//     want backfill noise to clutter `oat routing report`)
func LoadCorpusJoined(historyPath, sidecarPath string) ([]OutcomeRecord, []ParseErrLine, error) {
	records, parseErrs, err := loadHistoryRecords(historyPath)
	if err != nil {
		return nil, nil, err
	}
	if len(records) == 0 {
		return records, parseErrs, nil
	}

	coverage, err := loadSidecarCoverage(sidecarPath)
	if err != nil {
		// Non-fatal: report main records even if sidecar is unreadable.
		// The user gets the (uninflated) immediate-signal scores rather
		// than zero data. Intentionally swallowed.
		return records, parseErrs, nil //nolint:nilerr // sidecar is best-effort
	}

	for i := range records {
		key := OutcomeRecordKey{
			TS:     records[i].TS,
			Worker: records[i].Worker,
			Repo:   records[i].Repo,
		}
		if snaps, ok := coverage[key]; ok && len(snaps) > 0 {
			// Append to whatever PRStateHistory the record already had.
			// Today main records always have empty PRStateHistory (logger
			// never populates it), but appending is the correct semantics
			// if a future writer ever does.
			records[i].PRStateHistory = append(records[i].PRStateHistory, snaps...)
		}
	}
	return records, parseErrs, nil
}

// ParseErrLine is exported so callers can render parse errors uniformly.
// One per malformed JSONL line in the main file.
type ParseErrLine struct {
	Line int
	Err  error
}

// loadHistoryRecords parses the main file. Mirrors the CLI's loadOutcomeRecords
// but lives in the routing package so it can be reused by share-payload and
// future report generators without circular imports.
func loadHistoryRecords(path string) ([]OutcomeRecord, []ParseErrLine, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	defer f.Close()

	var (
		recs    []OutcomeRecord
		errs    []ParseErrLine
		lineNum int
	)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		lineNum++
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var rec OutcomeRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			errs = append(errs, ParseErrLine{Line: lineNum, Err: err})
			continue
		}
		recs = append(recs, rec)
	}
	if err := sc.Err(); err != nil {
		return recs, errs, err
	}
	return recs, errs, nil
}

// loadSidecarCoverage parses the backfill sidecar and groups snapshots by
// record key. One record may have up to 3 snapshots (one per lag bucket).
//
// Returns an empty map when the sidecar doesn't exist (fresh installs) or
// is unreadable (silent — the main records are still useful).
func loadSidecarCoverage(path string) (map[OutcomeRecordKey][]PRStateSnapshot, error) {
	out := map[OutcomeRecordKey][]PRStateSnapshot{}

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return out, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1*1024*1024)
	for sc.Scan() {
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var entry PRBackfillEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			continue // sidecar parse errors are silent (operator-debug grade)
		}
		out[entry.RecordKey] = append(out[entry.RecordKey], entry.Snapshot)
	}
	return out, sc.Err()
}
