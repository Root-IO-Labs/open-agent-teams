package routing

import "testing"

func TestBuildCorpusIndex_GroupsByCanonicalModelAndComplexity(t *testing.T) {
	records := []OutcomeRecord{
		// Two records same canonical model, same complexity — should aggregate
		{
			Model:          "anthropic:claude-3-5-sonnet-20241022",
			ModelCanonical: "claude-sonnet-3-5",
			TaskText:       "Fix the typo 'recieve'",
			Outcome:        "completed",
		},
		{
			Model:          "anthropic:claude-3-5-sonnet-20240620",
			ModelCanonical: "claude-sonnet-3-5", // different point release, same canonical
			TaskText:       "Fix the typo 'recieve'",
			Outcome:        "completed",
		},
		// Different model, same complexity → different bucket
		{
			Model:          "openai:gpt-5.4-mini",
			ModelCanonical: "gpt-5.4-mini",
			TaskText:       "Fix the typo 'recieve'",
			Outcome:        "completed",
		},
	}

	idx := BuildCorpusIndex(records)

	sonnet := idx.Lookup("claude-sonnet-3-5", ComplexityTrivial)
	if sonnet == nil {
		t.Fatal("sonnet+trivial bucket missing")
	}
	if sonnet.Total != 2 {
		t.Errorf("sonnet+trivial Total = %d, want 2 (point releases collapsed)", sonnet.Total)
	}

	mini := idx.Lookup("gpt-5.4-mini", ComplexityTrivial)
	if mini == nil || mini.Total != 1 {
		t.Errorf("gpt-mini bucket: %+v", mini)
	}
}

func TestBuildCorpusIndex_ExcludesUnscoreableFromSuccessRate(t *testing.T) {
	records := []OutcomeRecord{
		{Model: "openai:gpt-5.4-mini", ModelCanonical: "gpt-5.4-mini", TaskText: "fix typo", Outcome: "completed"},
		{Model: "openai:gpt-5.4-mini", ModelCanonical: "gpt-5.4-mini", TaskText: "fix typo", Outcome: "removed", RemovalReason: "manual"}, // unscoreable
		{Model: "openai:gpt-5.4-mini", ModelCanonical: "gpt-5.4-mini", TaskText: "fix typo", Outcome: "removed", RemovalReason: "superseded"},
	}
	idx := BuildCorpusIndex(records)
	bucket := idx.Lookup("gpt-5.4-mini", ComplexityTrivial)
	if bucket == nil {
		t.Fatal("bucket missing")
	}
	if bucket.Total != 3 {
		t.Errorf("Total = %d, want 3 (all records counted)", bucket.Total)
	}
	if bucket.Scored != 1 {
		t.Errorf("Scored = %d, want 1 (only the completed record is scoreable)", bucket.Scored)
	}
	if bucket.Successes != 1 {
		t.Errorf("Successes = %d, want 1", bucket.Successes)
	}
}

func TestHistoricalFactor_FallsThroughBelowThreshold(t *testing.T) {
	// 4 successes out of 4 — but N < MinHistoricalSamples, so no adjustment
	bucket := &CorpusStats{Scored: 4, Successes: 4, WilsonLowerBound: 0.4}
	if got := bucket.HistoricalFactor(); got != 1.0 {
		t.Errorf("below threshold: factor = %v, want 1.0 (no adjustment)", got)
	}
}

func TestHistoricalFactor_FloorsAt050(t *testing.T) {
	// 0/10 successes — Wilson lower bound is ~0; we don't want to fully banish the model
	bucket := &CorpusStats{Scored: 10, Successes: 0, WilsonLowerBound: 0.0}
	if got := bucket.HistoricalFactor(); got != 0.5 {
		t.Errorf("floored factor = %v, want 0.5", got)
	}
}

func TestHistoricalFactor_UsesWilsonAboveFloor(t *testing.T) {
	// 9/10 — Wilson should be around 0.6, above the 0.5 floor
	bucket := &CorpusStats{Scored: 10, Successes: 9, WilsonLowerBound: 0.62}
	if got := bucket.HistoricalFactor(); got != 0.62 {
		t.Errorf("factor = %v, want 0.62 (the wilson lower bound)", got)
	}
}

func TestCorpusIndex_NilSafe(t *testing.T) {
	var idx *CorpusIndex
	if !idx.IsEmpty() {
		t.Error("nil IsEmpty should be true")
	}
	if idx.Lookup("x", ComplexityComplex) != nil {
		t.Error("nil Lookup should return nil")
	}
}

func TestCorpusIndex_DeterministicGivenInputs(t *testing.T) {
	records := []OutcomeRecord{
		{Model: "openai:gpt-5.4-mini", ModelCanonical: "gpt-5.4-mini", TaskText: "fix typo", Outcome: "completed"},
		{Model: "anthropic:claude-haiku-4-5", ModelCanonical: "claude-haiku-4-5", TaskText: "Refactor the auth subsystem across many files", Outcome: "completed"},
	}
	a := BuildCorpusIndex(records)
	b := BuildCorpusIndex(records)

	for _, c := range []TaskComplexity{ComplexityTrivial, ComplexitySimple, ComplexityStandard, ComplexityComplex} {
		for _, m := range []string{"gpt-5.4-mini", "claude-haiku-4-5"} {
			ba := a.Lookup(m, c)
			bb := b.Lookup(m, c)
			if (ba == nil) != (bb == nil) {
				t.Errorf("(%s,%s): one nil, one not — non-deterministic", m, c)
			}
			if ba != nil && bb != nil {
				if ba.Total != bb.Total || ba.Successes != bb.Successes {
					t.Errorf("(%s,%s): stats differ between builds", m, c)
				}
			}
		}
	}
}
