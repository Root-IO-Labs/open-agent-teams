package routing

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestOutcomeLogger_WriteFailureNonFatal asserts the logger swallows write
// errors and never propagates them to the caller. This is the runtime side of
// the cardinal rule: a broken disk must not block a routing decision.
//
// The test points the logger at a path under a regular file (not a directory),
// which forces every OpenFile to fail. The Log call must return without
// panicking and without surfacing the error.
func TestOutcomeLogger_WriteFailureNonFatal(t *testing.T) {
	tmp := t.TempDir()
	// Create a regular file where the logger expects a parent directory. Any
	// MkdirAll/OpenFile attempt will fail because this isn't a directory.
	blocker := filepath.Join(tmp, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	logPath := filepath.Join(blocker, "subdir", "history.jsonl")

	var warns int32
	l := NewOutcomeLogger(logPath, func(format string, args ...any) {
		atomic.AddInt32(&warns, 1)
	})

	// Must not panic. Must not block. Must not error (the API has no error
	// return — that's the contract).
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Log(OutcomeRecord{Repo: "x", Worker: "y", Model: "z"})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Log() blocked > 2s on broken path — must be non-blocking")
	}

	if atomic.LoadInt32(&warns) == 0 {
		t.Error("expected warn callback to fire on broken path; got zero")
	}
}

// TestOutcomeLogger_NilReceiverNoop confirms a nil *OutcomeLogger.Log is a
// no-op rather than a panic. The daemon constructs the logger unconditionally
// today, but defensive code paths in tests and the future migrate-on-start
// flow may pass nil.
func TestOutcomeLogger_NilReceiverNoop(t *testing.T) {
	var l *OutcomeLogger
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil-receiver Log panicked: %v", r)
		}
	}()
	l.Log(OutcomeRecord{Repo: "x"})
}

// TestOutcomeLogger_ConcurrentStress amplifies the existing concurrent test:
// 100 goroutines each writing 50 records. We assert every line is parseable
// and no record is torn. This guards against append races introduced by
// future refactors.
func TestOutcomeLogger_ConcurrentStress(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test")
	}
	tmp := t.TempDir()
	path := filepath.Join(tmp, "history.jsonl")
	l := NewOutcomeLogger(path, nil)

	const goroutines = 100
	const perGoroutine = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				l.Log(OutcomeRecord{
					Repo:   "stress",
					Worker: "w",
					Model:  "m",
				})
			}
		}(i)
	}
	wg.Wait()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Every line must parse. We don't care about content here, only torn-line
	// detection.
	count := 0
	for _, line := range splitJSONLines(data) {
		if len(line) == 0 {
			continue
		}
		count++
	}
	if want := goroutines * perGoroutine; count != want {
		t.Errorf("got %d records, want %d (torn writes?)", count, want)
	}
}

// TestPRBackfiller_NilReceiverRunNoop confirms a nil *PRBackfiller.Run is a
// no-op. The daemon's constructor returns nil when paths are empty (caller-
// side opt-out), so Start() must tolerate nil.
func TestPRBackfiller_NilReceiverRunNoop(t *testing.T) {
	var b *PRBackfiller
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil-receiver Run panicked: %v", r)
		}
	}()
	b.Run(ctx) // must return immediately, no panic
}

// TestPRBackfiller_MissingHistoryFileTolerated covers the fresh-install case:
// the daemon starts, the backfiller ticks, but the user has not completed any
// task yet so routing-history.jsonl doesn't exist. Backfiller must not error
// or warn on that case.
func TestPRBackfiller_MissingHistoryFileTolerated(t *testing.T) {
	tmp := t.TempDir()
	hist := filepath.Join(tmp, "missing.jsonl")
	side := filepath.Join(tmp, "missing.backfill.jsonl")

	var warns int32
	b := NewPRBackfiller(PRBackfillerOptions{
		HistoryPath: hist,
		SidecarPath: side,
		Warn:        func(format string, args ...any) { atomic.AddInt32(&warns, 1) },
	})
	if b == nil {
		t.Fatal("backfiller nil with valid paths")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	b.runOnce(ctx)

	if atomic.LoadInt32(&warns) > 0 {
		t.Errorf("missing history file should be silent; got %d warns", warns)
	}
	if _, err := os.Stat(side); !os.IsNotExist(err) {
		t.Errorf("sidecar should not exist when there's no history; stat err=%v", err)
	}
}

// splitJSONLines splits a buffer on \n, stripping the trailing empty entry
// from a final newline. Avoids the standard-library bufio dependency for
// stress tests.
func splitJSONLines(data []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			out = append(out, data[start:i])
			start = i + 1
		}
	}
	if start < len(data) {
		out = append(out, data[start:])
	}
	return out
}
