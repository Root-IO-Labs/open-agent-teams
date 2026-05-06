package routing

import (
	"sync"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/routing/stats"
)

// MinHistoricalSamples is the threshold below which the V2 router refuses
// to apply a historical adjustment. With fewer than this many records for
// a (model, complexity) bucket, we trust the static prior alone — small-N
// data is noise, not signal.
//
// Rationale: per the benchmark methodology rule on N=3 outliers, the
// Wilson lower bound at N=3 is wide enough that the historical factor
// would dominate every static-tier difference. Threshold of 5 keeps us
// honest while still letting the corpus take over once we have real volume.
const MinHistoricalSamples = 5

// CorpusIndex is an in-memory aggregation of routing-history records,
// bucketed by (canonical_model, complexity). Built from a snapshot of the
// joined corpus (main + sidecar). Read-many, write-rarely (the daemon
// rebuilds it every ~10 min in a background goroutine).
//
// Why pre-aggregate: the V2 router is called on every worker spawn. Walking
// the full JSONL corpus per spawn is fine performance-wise (sub-ms at
// 10k records) but the structure makes intent clear: routing is a function
// of (eligible models, pricing, corpus snapshot) → decision. The snapshot
// is cached so concurrent backfill writes can't cause non-determinism
// within a single decision.
//
// Determinism contract: BuildCorpusIndex is pure. Same records → same
// index → same Lookup results. Critical for the cardinal-rule guard.
type CorpusIndex struct {
	mu      sync.RWMutex
	builtAt time.Time
	buckets map[corpusKey]*CorpusStats
}

// corpusKey identifies one (canonical_model, complexity) bucket. Canonical
// model means we group claude-3-5-sonnet-20241022 with claude-3-5-sonnet-20240620
// (both → claude-sonnet-3-5) so historical signal survives point-release rotation.
type corpusKey struct {
	Model      string
	Complexity TaskComplexity
}

// CorpusStats summarizes one bucket. Pre-computed Wilson lower bound saves
// the V2 router from doing the math on every routing decision.
type CorpusStats struct {
	Total            int     // records in this bucket (any score, including unscoreable)
	Scored           int     // records with hasScore=true
	Successes        int     // count of scoreable records with score >= 0.5
	MeanScore        float64 // mean of all scoreable scores
	WilsonLowerBound float64 // 95% one-sided lower bound on success rate
}

// HistoricalFactor returns the multiplier the V2 router should apply to the
// static prior for this bucket. Returns 1.0 (no adjustment) when N is below
// MinHistoricalSamples — falls back to V1 behavior, which is the right
// thing at small N.
//
// When N is sufficient, returns max(0.5, wilson_lo). The 0.5 floor prevents
// a bad streak from completely banishing a model — we still want it in the
// candidate pool for re-trial.
func (s *CorpusStats) HistoricalFactor() float64 {
	if s == nil || s.Scored < MinHistoricalSamples {
		return 1.0
	}
	const minFactor = 0.5
	if s.WilsonLowerBound < minFactor {
		return minFactor
	}
	return s.WilsonLowerBound
}

// BuildCorpusIndex constructs an index from the provided records. Pure
// function — no I/O, no side effects, deterministic given the input slice.
//
// Records that don't have a derivable success score (in-flight, manual
// removed, superseded) are counted in Total but skipped from the success
// arithmetic. This matches the report's exclusion logic so the V2 router
// doesn't see a different success rate than `oat routing report` shows.
func BuildCorpusIndex(records []OutcomeRecord) *CorpusIndex {
	idx := &CorpusIndex{
		builtAt: time.Now().UTC(),
		buckets: map[corpusKey]*CorpusStats{},
	}
	for _, rec := range records {
		key := corpusKey{
			Model:      canonicalKey(rec),
			Complexity: ExtractFeatures(rec.TaskText).Complexity,
		}
		bucket, ok := idx.buckets[key]
		if !ok {
			bucket = &CorpusStats{}
			idx.buckets[key] = bucket
		}
		bucket.Total++

		score, _, has := DeriveSuccessScore(rec)
		if !has {
			continue
		}
		bucket.Scored++
		bucket.MeanScore += score
		if score >= 0.5 {
			bucket.Successes++
		}
	}
	for _, bucket := range idx.buckets {
		if bucket.Scored > 0 {
			bucket.MeanScore /= float64(bucket.Scored)
			bucket.WilsonLowerBound = stats.WilsonLowerBound(bucket.Successes, bucket.Scored, 0.05)
		}
	}
	return idx
}

// canonicalKey returns the model identifier the index buckets on. Prefers
// ModelCanonical (Provider+canonical join already done by the writer); falls
// back to the raw Model field for v1 records that didn't get migrated.
func canonicalKey(rec OutcomeRecord) string {
	if rec.ModelCanonical != "" {
		return rec.ModelCanonical
	}
	if rec.Model != "" {
		canon, _ := Canonicalize(rec.Model)
		return canon
	}
	return ""
}

// Lookup returns the stats for one bucket, or nil if no records exist for it.
// Lock-free for the common cached case — the index is built once and read
// many times.
func (c *CorpusIndex) Lookup(model string, complexity TaskComplexity) *CorpusStats {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.buckets[corpusKey{Model: model, Complexity: complexity}]
}

// IsEmpty returns true when no records were observed. The V2 router uses
// this to short-circuit to V1 behavior on a fresh-install / empty-corpus
// daemon — no point computing historical factors that are all 1.0.
func (c *CorpusIndex) IsEmpty() bool {
	if c == nil {
		return true
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.buckets) == 0
}

// BuiltAt is the timestamp the index was constructed. Useful for a `oat
// routing health` style command to surface freshness.
func (c *CorpusIndex) BuiltAt() time.Time {
	if c == nil {
		return time.Time{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.builtAt
}
