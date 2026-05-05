package routing

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PRBackfillEntry is one row of the sidecar file ~/.oat/routing-history.backfill.jsonl.
// Each entry records ONE observation of a PR's state at one lag bucket.
// Multiple entries per OutcomeRecord (one per lag bucket) accumulate into the
// PRStateHistory series consumers see at index time.
//
// The sidecar is a separate file from routing-history.jsonl so the main file
// stays strictly append-only and immutable. Phase 2's index step joins the two
// on RecordKey; downstream readers MUST tolerate sidecar entries that don't
// match any record (e.g., the main file was rotated/truncated).
type PRBackfillEntry struct {
	SchemaVersion int              `json:"schema_version"`
	SnapshotTS    string           `json:"snapshot_ts"`
	RecordKey     OutcomeRecordKey `json:"record_key"`
	Snapshot      PRStateSnapshot  `json:"snapshot"`
}

// OutcomeRecordKey identifies one OutcomeRecord across the main JSONL and the
// sidecar without requiring an explicit ID column. (TS, Worker, Repo) is
// effectively unique because the same worker can't complete twice at the same
// timestamp in the same repo.
type OutcomeRecordKey struct {
	TS     string `json:"ts"`
	Worker string `json:"worker"`
	Repo   string `json:"repo"`
}

const prBackfillSchemaVersion = 1

// DefaultBackfillSidecarPath returns the standard sidecar location.
func DefaultBackfillSidecarPath(oatRoot string) string {
	return filepath.Join(oatRoot, "routing-history.backfill.jsonl")
}

// PRBackfiller is a long-running background worker that observes PR state at
// fixed lag buckets after task completion. Lifecycle: NewPRBackfiller, then
// Run(ctx) (blocks until ctx canceled). Designed to be wired into the
// daemon's startup/shutdown the same way OutcomeLogger is.
//
// Errors never propagate; everything routes through the warn callback. A
// failure to read the main file or run gh must not block routing or the
// daemon's main loop.
type PRBackfiller struct {
	historyPath string
	sidecarPath string
	interval    time.Duration

	// Injectable for tests. In prod, runCmd shells out to `gh`.
	runCmd func(ctx context.Context, name string, args ...string) ([]byte, error)
	now    func() time.Time
	warn   func(format string, args ...any)

	mu sync.Mutex // protects sidecar writes
}

// PRBackfillerOptions tunes the backfiller. Zero values get sensible defaults.
type PRBackfillerOptions struct {
	HistoryPath string // required; main routing-history.jsonl
	SidecarPath string // required; sidecar file
	Interval    time.Duration
	RunCmd      func(ctx context.Context, name string, args ...string) ([]byte, error)
	Now         func() time.Time
	Warn        func(format string, args ...any)
}

// NewPRBackfiller constructs a backfiller. Returns nil if HistoryPath or
// SidecarPath is empty (caller-side opt-out).
func NewPRBackfiller(opts PRBackfillerOptions) *PRBackfiller {
	if opts.HistoryPath == "" || opts.SidecarPath == "" {
		return nil
	}
	if opts.Interval <= 0 {
		opts.Interval = 5 * time.Minute
	}
	if opts.RunCmd == nil {
		opts.RunCmd = defaultRunCmd
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Warn == nil {
		opts.Warn = func(string, ...any) {}
	}
	return &PRBackfiller{
		historyPath: opts.HistoryPath,
		sidecarPath: opts.SidecarPath,
		interval:    opts.Interval,
		runCmd:      opts.RunCmd,
		now:         opts.Now,
		warn:        opts.Warn,
	}
}

func defaultRunCmd(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// Run blocks until ctx is canceled, ticking once at start and then on
// `interval`. Each tick scans the main file, looks at the sidecar to see what's
// already covered, and appends new observations as buckets become due.
func (b *PRBackfiller) Run(ctx context.Context) {
	if b == nil {
		return
	}
	// Tick once immediately — useful for tests and gives users data faster
	// after daemon restart.
	b.runOnce(ctx)

	t := time.NewTicker(b.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.runOnce(ctx)
		}
	}
}

// backfillLagThresholds defines the lag buckets we sample at. Order: shortest
// first. Once a record has all three covered it's never visited again.
var backfillLagThresholds = []struct {
	bucket string
	age    time.Duration
}{
	{"1h", 1 * time.Hour},
	{"24h", 24 * time.Hour},
	{"7d", 7 * 24 * time.Hour},
}

// backfillMaxAge bounds how far back we look. A bit of slack past 7d so
// snapshots aren't lost if the daemon was offline at the 7d mark.
const backfillMaxAge = 14 * 24 * time.Hour

// runOnce performs a single pass: read sidecar coverage, scan main file,
// fetch + append for due buckets.
func (b *PRBackfiller) runOnce(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}

	covered, err := b.readCoverage()
	if err != nil {
		b.warn("backfill: read coverage failed: %v", err)
		// Continue with empty covered map — worst case we write duplicate
		// snapshots, which the index step can dedupe.
		covered = map[OutcomeRecordKey]map[string]struct{}{}
	}

	f, err := os.Open(b.historyPath)
	if err != nil {
		// If the main file doesn't exist yet, there's nothing to backfill.
		// This is normal on a fresh install.
		if !os.IsNotExist(err) {
			b.warn("backfill: open history failed: %v", err)
		}
		return
	}
	defer f.Close()

	now := b.now()
	scanner := bufio.NewScanner(f)
	// Records can carry full task text — be generous with buffer.
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		if ctx.Err() != nil {
			return
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec OutcomeRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			// Malformed lines are tolerated (someone could have hand-edited).
			continue
		}
		if !shouldBackfill(rec, now) {
			continue
		}
		b.processRecord(ctx, rec, covered, now)
	}
	if err := scanner.Err(); err != nil {
		b.warn("backfill: scan history failed: %v", err)
	}
}

// shouldBackfill returns true if this record is a candidate for at least one
// state observation: it has a PR, isn't too old, and has a parsable TS.
func shouldBackfill(rec OutcomeRecord, now time.Time) bool {
	if rec.PRNumber <= 0 && rec.PRURL == "" {
		return false
	}
	ts, err := time.Parse(time.RFC3339, rec.TS)
	if err != nil {
		return false
	}
	age := now.Sub(ts)
	if age < 0 || age > backfillMaxAge {
		return false
	}
	return true
}

// processRecord checks each lag bucket and writes a snapshot for any that are
// due-and-not-covered.
func (b *PRBackfiller) processRecord(ctx context.Context, rec OutcomeRecord, covered map[OutcomeRecordKey]map[string]struct{}, now time.Time) {
	ts, err := time.Parse(time.RFC3339, rec.TS)
	if err != nil {
		return
	}
	age := now.Sub(ts)
	key := OutcomeRecordKey{TS: rec.TS, Worker: rec.Worker, Repo: rec.Repo}
	coveredBuckets := covered[key]

	for _, threshold := range backfillLagThresholds {
		if age < threshold.age {
			continue // not yet due
		}
		if _, already := coveredBuckets[threshold.bucket]; already {
			continue
		}

		snap, err := b.fetchPRState(ctx, rec)
		if err != nil {
			b.warn("backfill: fetch PR state for %s/%s failed: %v", rec.Repo, rec.Worker, err)
			// Don't write a partial entry. Try again next tick.
			continue
		}
		snap.LagBucket = threshold.bucket

		entry := PRBackfillEntry{
			SchemaVersion: prBackfillSchemaVersion,
			SnapshotTS:    now.UTC().Format(time.RFC3339),
			RecordKey:     key,
			Snapshot:      snap,
		}
		if err := b.appendSidecar(entry); err != nil {
			b.warn("backfill: append sidecar failed: %v", err)
			return
		}
		// Record locally so the same tick won't re-fetch other buckets we
		// also discover are due (rare race, but cheap to guard).
		if coveredBuckets == nil {
			coveredBuckets = map[string]struct{}{}
			covered[key] = coveredBuckets
		}
		coveredBuckets[threshold.bucket] = struct{}{}
	}
}

// fetchPRState invokes `gh pr view` for the record's PR and parses the JSON
// response into a PRStateSnapshot. The repo path is `rec.Repo` (the repo name
// the daemon uses, NOT necessarily an owner/repo slug — gh resolves from cwd
// or via --repo flag if needed in future).
//
// Captures CI status alongside merge state via statusCheckRollup. A single gh
// call gives us both, avoiding a second round-trip per record per bucket.
func (b *PRBackfiller) fetchPRState(ctx context.Context, rec OutcomeRecord) (PRStateSnapshot, error) {
	if rec.PRNumber <= 0 && rec.PRURL == "" {
		return PRStateSnapshot{}, fmt.Errorf("no PR identifier on record")
	}

	args := []string{"pr", "view", "--json", "state,mergedAt,closedAt,statusCheckRollup"}
	switch {
	case rec.PRURL != "":
		args = append(args, rec.PRURL)
	case rec.PRNumber > 0:
		args = append(args, strconv.Itoa(rec.PRNumber))
	}

	out, err := b.runCmd(ctx, "gh", args...)
	if err != nil {
		return PRStateSnapshot{}, err
	}

	var resp struct {
		State             string         `json:"state"`
		MergedAt          string         `json:"mergedAt"`
		ClosedAt          string         `json:"closedAt"`
		StatusCheckRollup []ghCheckEntry `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return PRStateSnapshot{}, fmt.Errorf("parse gh output: %w", err)
	}

	ciStatus, firstFailure := summarizeCheckRollup(resp.StatusCheckRollup)

	return PRStateSnapshot{
		TS:             b.now().UTC().Format(time.RFC3339),
		State:          normalizeState(resp.State),
		MergedAt:       resp.MergedAt,
		ClosedAt:       resp.ClosedAt,
		CIStatus:       ciStatus,
		CIFirstFailure: firstFailure,
	}, nil
}

// ghCheckEntry mirrors one entry of `gh pr view --json statusCheckRollup`.
// gh returns a heterogeneous union of "Check" and "StatusContext" objects;
// we read the small subset of fields shared by both.
type ghCheckEntry struct {
	Name       string `json:"name"`
	Status     string `json:"status"`     // QUEUED | IN_PROGRESS | COMPLETED (Check)
	Conclusion string `json:"conclusion"` // SUCCESS | FAILURE | CANCELED | NEUTRAL | TIMED_OUT | SKIPPED (Check)
	State      string `json:"state"`      // PENDING | EXPECTED | ERROR | FAILURE | SUCCESS (StatusContext)
	Context    string `json:"context"`    // (StatusContext display name)
}

// summarizeCheckRollup reduces the gh rollup into (status, firstFailure).
//
//	"passed"  — every entry is a clear success / skipped / neutral
//	"failed"  — at least one entry failed
//	"pending" — at least one entry is still running and none have failed yet
//	"none"    — the repo returned an empty rollup (no CI configured)
//	""        — rollup parsing produced no usable signal
//
// firstFailure is the name (or context) of the first failing entry, if any.
// "First" is by rollup order — gh sorts by check creation time, which is
// usually the order the user perceives.
func summarizeCheckRollup(entries []ghCheckEntry) (status, firstFailure string) {
	if len(entries) == 0 {
		return "none", ""
	}
	hasFailure := false
	hasPending := false
	for _, e := range entries {
		// Check object (preferred path)
		if e.Status != "" {
			switch e.Status {
			case "QUEUED", "IN_PROGRESS", "WAITING":
				hasPending = true
			case "COMPLETED":
				switch e.Conclusion {
				case "FAILURE", "TIMED_OUT", "STARTUP_FAILURE", "ACTION_REQUIRED":
					hasFailure = true
					if firstFailure == "" {
						firstFailure = displayName(e)
					}
				}
			}
			continue
		}
		// StatusContext fallback
		switch strings.ToUpper(e.State) {
		case "PENDING", "EXPECTED":
			hasPending = true
		case "ERROR", "FAILURE":
			hasFailure = true
			if firstFailure == "" {
				firstFailure = displayName(e)
			}
		}
	}
	switch {
	case hasFailure:
		return "failed", firstFailure
	case hasPending:
		return "pending", ""
	default:
		return "passed", ""
	}
}

func displayName(e ghCheckEntry) string {
	if e.Name != "" {
		return e.Name
	}
	return e.Context
}

func normalizeState(s string) string {
	switch s {
	case "MERGED", "merged":
		return "merged"
	case "CLOSED", "closed":
		return "closed"
	case "OPEN", "open":
		return "open"
	default:
		return s
	}
}

// readCoverage builds a lookup map (RecordKey -> set of covered LagBuckets)
// by parsing every sidecar entry. The sidecar can be missing (first run) —
// that's handled cleanly by returning an empty map.
func (b *PRBackfiller) readCoverage() (map[OutcomeRecordKey]map[string]struct{}, error) {
	covered := map[OutcomeRecordKey]map[string]struct{}{}
	f, err := os.Open(b.sidecarPath)
	if err != nil {
		if os.IsNotExist(err) {
			return covered, nil
		}
		return covered, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry PRBackfillEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		set, ok := covered[entry.RecordKey]
		if !ok {
			set = map[string]struct{}{}
			covered[entry.RecordKey] = set
		}
		set[entry.Snapshot.LagBucket] = struct{}{}
	}
	return covered, scanner.Err()
}

// appendSidecar appends one JSON line to the sidecar file. Holds an internal
// mutex so concurrent ticks (none today, but cheap insurance) don't interleave.
func (b *PRBackfiller) appendSidecar(entry PRBackfillEntry) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(b.sidecarPath), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(b.sidecarPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(f, string(data)); err != nil {
		return err
	}
	return nil
}
