package cli

// Tests cover the narrow on-disk contract of `oat tokens report`: given an
// output dir laid out exactly the way the daemon writes it (top-level
// `<agent>.log` files plus a `workers/` subdir), we parse every
// [OAT_TOKENS] line we find and aggregate correctly.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/routing"
)

func writeLog(t *testing.T, path string, lines []string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func tokensLine(in, out, cr, cc int64) string {
	b, _ := json.Marshal(map[string]int64{
		"delta_input":       in,
		"delta_output":      out,
		"cumulative_input":  in,
		"cumulative_output": out,
		"cache_read":        cr,
		"cache_creation":    cc,
	})
	return "[OAT_TOKENS] " + string(b)
}

// TestScanTokenLogs_HappyPath exercises the primary flow: a worker log and
// a non-worker log sit side by side, each with multiple [OAT_TOKENS] lines.
// We expect the final cumulative snapshot per agent and correct worker
// classification based on directory.
func TestScanTokenLogs_HappyPath(t *testing.T) {
	tmp := t.TempDir()

	writeLog(t, filepath.Join(tmp, "supervisor.log"), []string{
		"irrelevant prefix line",
		tokensLine(100, 20, 0, 0),
		tokensLine(300, 60, 150, 80),
	})
	writeLog(t, filepath.Join(tmp, "workers", "warm-albatross.log"), []string{
		tokensLine(1000, 200, 700, 120),
		tokensLine(2500, 400, 1900, 120),
	})

	got, err := scanTokenLogs(tmp, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("scanTokenLogs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 reports, got %d: %+v", len(got), got)
	}
	bySlug := map[string]agentReport{}
	for _, r := range got {
		bySlug[r.Agent] = r
	}
	sup := bySlug["supervisor"]
	if sup.IsWorker {
		t.Errorf("supervisor classified as worker")
	}
	if sup.InputTokens != 300 || sup.CacheRead != 150 {
		t.Errorf("supervisor totals wrong: %+v", sup)
	}
	w := bySlug["warm-albatross"]
	if !w.IsWorker {
		t.Errorf("warm-albatross should be a worker")
	}
	if w.InputTokens != 2500 || w.OutputTokens != 400 ||
		w.CacheRead != 1900 || w.CacheCreation != 120 {
		t.Errorf("worker totals wrong: %+v", w)
	}
	if w.CacheHitPct != "76.0%" {
		t.Errorf("hit pct = %s, want 76.0%%", w.CacheHitPct)
	}
}

// TestScanTokenLogs_NoTokenLines: an agent whose log has no [OAT_TOKENS]
// lines (e.g. a non-Anthropic model with the bridge disabled, or a
// TUI-only render log) should be silently dropped rather than surfaced as
// a zero-token row.
func TestScanTokenLogs_NoTokenLines(t *testing.T) {
	tmp := t.TempDir()
	writeLog(t, filepath.Join(tmp, "quiet.log"), []string{
		"starting up",
		"fetching repo",
		"done",
	})
	got, err := scanTokenLogs(tmp, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("scanTokenLogs: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want empty, got %+v", got)
	}
}

// TestScanTokenLogs_SinceFiltersByMtime verifies --since filtering uses
// file mtime. A log older than `since` is skipped entirely so a scanner
// that used epoch-0 timestamps internally would fail this test.
func TestScanTokenLogs_SinceFiltersByMtime(t *testing.T) {
	tmp := t.TempDir()
	oldLog := filepath.Join(tmp, "old.log")
	newLog := filepath.Join(tmp, "new.log")
	writeLog(t, oldLog, []string{tokensLine(500, 50, 200, 20)})
	writeLog(t, newLog, []string{tokensLine(800, 80, 400, 30)})

	oldTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(oldLog, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	cutoff := time.Now().Add(-1 * time.Hour)
	got, err := scanTokenLogs(tmp, cutoff, time.Time{})
	if err != nil {
		t.Fatalf("scanTokenLogs: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 report after since filter, got %d: %+v", len(got), got)
	}
	if got[0].Agent != "new" {
		t.Errorf("expected 'new' to survive cutoff, got %q", got[0].Agent)
	}
}

// TestParseSinceUntil_Duration: the user-friendly "2h" form should
// resolve to "now minus 2h", not fail with a parse error. RFC3339 should
// round-trip cleanly.
func TestParseSinceUntil_Duration(t *testing.T) {
	t.Run("duration", func(t *testing.T) {
		got, err := parseSinceUntil("30m")
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if got.IsZero() {
			t.Errorf("zero time for duration")
		}
		if time.Since(got) < 25*time.Minute || time.Since(got) > 35*time.Minute {
			t.Errorf("duration resolved to an unexpected point: %v", got)
		}
	})
	t.Run("rfc3339", func(t *testing.T) {
		got, err := parseSinceUntil("2026-01-15T10:30:00Z")
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if got.Year() != 2026 || got.Month() != 1 || got.Day() != 15 {
			t.Errorf("rfc3339 roundtrip wrong: %v", got)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		if _, err := parseSinceUntil("not-a-time"); err == nil {
			t.Errorf("want err for bogus input")
		}
	})
	t.Run("empty", func(t *testing.T) {
		got, err := parseSinceUntil("")
		if err != nil {
			t.Errorf("unexpected err: %v", err)
		}
		if !got.IsZero() {
			t.Errorf("empty should yield zero time, got %v", got)
		}
	})
}

// TestFilterByWave_NormalizesPrefix: users type `--wave 2` more often than
// `--wave wave:2`. Both must do the same thing.
func TestFilterByWave_NormalizesPrefix(t *testing.T) {
	in := []agentReport{
		{Agent: "a", Wave: "wave:2", InputTokens: 100},
		{Agent: "b", Wave: "wave:3", InputTokens: 100},
		{Agent: "c", Wave: "", InputTokens: 100},
	}
	got := filterByWave(in, "2")
	if len(got) != 1 || got[0].Agent != "a" {
		t.Fatalf("want only agent a, got %+v", got)
	}
	got = filterByWave(in, "wave:3")
	if len(got) != 1 || got[0].Agent != "b" {
		t.Fatalf("want only agent b, got %+v", got)
	}
}

// TestComputeAgentCost_SonnetExample: validates the cost formula against
// pricing.yaml's anthropic:claude-sonnet-4-6 entry. 1M input + 200k
// output + 500k cache_read + 100k cache_creation at sonnet-4-6 prices:
//
//	1.0 * 3.00 + 0.2 * 15.00 + 0.5 * 0.30 + 0.1 * 3.75
//	= 3.00 + 3.00 + 0.15 + 0.375
//	= 6.525
//
// This catches both off-by-1M errors and missing cache_creation.
func TestComputeAgentCost_SonnetExample(t *testing.T) {
	pricing := routing.LoadEmbeddedPricing()
	a := agentReport{
		Agent:         "test-worker",
		Model:         "anthropic:claude-sonnet-4-6",
		InputTokens:   1_000_000,
		OutputTokens:  200_000,
		CacheRead:     500_000,
		CacheCreation: 100_000,
	}
	got := computeAgentCost(a, pricing)
	if got == nil {
		t.Fatalf("expected non-nil cost for known model")
	}
	want := 6.525
	if diff := *got - want; diff > 0.001 || diff < -0.001 {
		t.Errorf("computeAgentCost = %v, want %v (diff %v)", *got, want, diff)
	}
}

// TestComputeAgentCost_NilWhenModelMissing: agents with no model (already
// retired from state.json) and agents whose model isn't in pricing.yaml
// both return nil so the table footnote can call them out.
func TestComputeAgentCost_NilWhenModelMissing(t *testing.T) {
	pricing := routing.LoadEmbeddedPricing()
	noModel := agentReport{Agent: "x", InputTokens: 1000}
	if got := computeAgentCost(noModel, pricing); got != nil {
		t.Errorf("want nil cost when Model=\"\", got %v", *got)
	}
	unknownModel := agentReport{Agent: "x", Model: "fakeprovider:fake-model", InputTokens: 1000}
	if got := computeAgentCost(unknownModel, pricing); got != nil {
		t.Errorf("want nil cost for unpriced model, got %v", *got)
	}
}

// TestComputeAgentCost_OpenAIIgnoresCacheCreation: OpenAI doesn't bill
// cache writes (cache reads are already discounted via cache_read price).
// The per-provider fallback in routing.CacheCreationPriceFor must zero
// out the cache_creation term for openai:* even when CacheCreation > 0.
func TestComputeAgentCost_OpenAIIgnoresCacheCreation(t *testing.T) {
	pricing := routing.LoadEmbeddedPricing()
	a := agentReport{
		Agent:         "x",
		Model:         "openai:gpt-5.4-mini",
		InputTokens:   100_000,
		OutputTokens:  10_000,
		CacheRead:     0,
		CacheCreation: 999_999, // intentionally large
	}
	got := computeAgentCost(a, pricing)
	if got == nil {
		t.Fatalf("expected non-nil cost")
	}
	// 0.1M * 0.750 + 0.01M * 4.500 = 0.075 + 0.045 = 0.12
	want := 0.12
	if diff := *got - want; diff > 0.001 || diff < -0.001 {
		t.Errorf("computeAgentCost = %v, want %v (cache_creation should be free for openai)", *got, want)
	}
}

// TestFmtCost: nil → "—" (matches "no data" rendering elsewhere in the
// table); priced → "$X.XXXX" with 4 decimal places so $0.0001-scale
// runs are still visible.
func TestFmtCost(t *testing.T) {
	if got := fmtCost(nil); got != "—" {
		t.Errorf("fmtCost(nil) = %q, want \"—\"", got)
	}
	v := 1.234567
	if got := fmtCost(&v); got != "$1.2346" {
		t.Errorf("fmtCost(1.234567) = %q, want $1.2346", got)
	}
	zero := 0.0
	if got := fmtCost(&zero); got != "$0.0000" {
		t.Errorf("fmtCost(0.0) = %q, want $0.0000", got)
	}
}

// TestLoadWavesFile_ShapeTolerance: accept either wrapper-keyed or bare
// map JSON since benchmarks may produce either.
func TestLoadWavesFile_ShapeTolerance(t *testing.T) {
	tmp := t.TempDir()

	wrapped := filepath.Join(tmp, "wrapped.json")
	_ = os.WriteFile(wrapped, []byte(`{"waves":{"worker-a":"wave:1","worker-b":"wave:2"}}`), 0644)
	got, err := loadWavesFile(wrapped)
	if err != nil {
		t.Fatalf("wrapped: %v", err)
	}
	if got["worker-a"] != "wave:1" || got["worker-b"] != "wave:2" {
		t.Errorf("wrapped parse wrong: %+v", got)
	}

	bare := filepath.Join(tmp, "bare.json")
	_ = os.WriteFile(bare, []byte(`{"worker-c":"wave:3"}`), 0644)
	got2, err := loadWavesFile(bare)
	if err != nil {
		t.Fatalf("bare: %v", err)
	}
	if got2["worker-c"] != "wave:3" {
		t.Errorf("bare parse wrong: %+v", got2)
	}
}
