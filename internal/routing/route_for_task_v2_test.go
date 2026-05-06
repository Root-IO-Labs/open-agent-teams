package routing

import (
	"strings"
	"testing"
)

// TestRouteForTaskV2_FallsBackOnEmptyCorpus: with no historical data,
// V2 must produce a sensible decision (V1-equivalent ranking) rather than
// erroring or picking pathologically.
func TestRouteForTaskV2_FallsBackOnEmptyCorpus(t *testing.T) {
	ps := minimalStoreForTests(t)
	pricing := LoadEmbeddedPricing()

	d, err := ps.RouteForTaskV2(RouteContext{
		TaskText: "Refactor the auth subsystem across many directories",
		Role:     RoleWorker,
	}, pricing, nil) // nil corpus
	if err != nil {
		t.Fatalf("V2 with nil corpus errored: %v", err)
	}
	if d.ChosenModel == "" {
		t.Error("V2 produced empty pick on empty corpus")
	}
	if !strings.Contains(d.Reason, "no corpus") {
		t.Errorf("Reason should signal empty corpus path; got %q", d.Reason)
	}
}

// TestRouteForTaskV2_HistoricalDowngrades: a model with very poor history
// in this complexity bucket should get re-ranked behind a model with no
// history (which gets factor=1.0, the higher effective score).
func TestRouteForTaskV2_HistoricalDowngrades(t *testing.T) {
	ps := minimalStoreForTests(t)
	pricing := LoadEmbeddedPricing()

	// Build a corpus where claude-haiku-4-5 has 10 records all failed at
	// Standard complexity. gpt-mini has zero records (factor=1.0 for it).
	records := make([]OutcomeRecord, 0, 10)
	for i := 0; i < 10; i++ {
		records = append(records, OutcomeRecord{
			Model:          "anthropic:claude-haiku-4-5",
			ModelCanonical: "claude-haiku-4-5",
			TaskText:       "Implement the new payment flow described in specs/payment.md so it returns 200 on charge",
			Outcome:        "removed",
			RemovalReason:  "failed",
		})
	}
	corpus := BuildCorpusIndex(records)

	d, err := ps.RouteForTaskV2(RouteContext{
		TaskText: "Implement the new payment flow described in specs/payment.md so it returns 200 on charge",
		Role:     RoleWorker,
	}, pricing, corpus)
	if err != nil {
		t.Fatalf("V2 errored: %v", err)
	}

	// haiku has historical_factor floored at 0.5; effective = 96 * 0.5 = 48
	// gpt-mini has factor=1.0; effective = 87 * 1.0 = 87
	// gemini-flash has factor=1.0; effective = 99
	// → flash should beat haiku via the historical downgrade
	if d.ChosenModel == "anthropic:claude-haiku-4-5" {
		t.Errorf("haiku has 10 failures in this bucket but was still chosen: %s", d.Reason)
	}
}

// TestRouteForTaskV2_DeterministicGivenInputs: cardinal-rule guard for V2.
// Same inputs (including the corpus snapshot) must produce byte-identical
// decisions.
func TestRouteForTaskV2_DeterministicGivenInputs(t *testing.T) {
	ps := minimalStoreForTests(t)
	pricing := LoadEmbeddedPricing()

	records := []OutcomeRecord{
		{Model: "openai:gpt-5.4-mini", ModelCanonical: "gpt-5.4-mini", TaskText: "Refactor a complex piece across many files", Outcome: "completed"},
	}
	corpus := BuildCorpusIndex(records)

	ctx := RouteContext{
		TaskText: "Refactor the auth subsystem across models/, controllers/, middleware/",
		Role:     RoleWorker,
	}

	first, err := ps.RouteForTaskV2(ctx, pricing, corpus)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	for i := 0; i < 100; i++ {
		got, err := ps.RouteForTaskV2(ctx, pricing, corpus)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if got.ChosenModel != first.ChosenModel || got.Reason != first.Reason {
			t.Fatalf("call %d non-deterministic: chose %q (was %q), reason %q (was %q)",
				i, got.ChosenModel, first.ChosenModel, got.Reason, first.Reason)
		}
	}
}

// TestRouteForTaskV2_OperatorOverrideRespected: explicit --model still wins.
func TestRouteForTaskV2_OperatorOverrideRespected(t *testing.T) {
	ps := minimalStoreForTests(t)
	pricing := LoadEmbeddedPricing()

	d, err := ps.RouteForTaskV2(RouteContext{
		TaskText:       "anything",
		Role:           RoleWorker,
		PreferredModel: "anthropic:claude-sonnet-4-6",
	}, pricing, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.ChosenModel != "anthropic:claude-sonnet-4-6" {
		t.Errorf("operator override not honored: chose %q", d.ChosenModel)
	}
	if d.RoutingSource != "operator-explicit" {
		t.Errorf("source = %q, want operator-explicit", d.RoutingSource)
	}
}

// TestRouteForTaskV2_BelowThresholdHistoricalIgnored: at N=3 (below
// MinHistoricalSamples=5), the historical factor must NOT pull a model
// down — V2's pick must be identical whether the bucket has 3 failed
// records or zero records. This is the small-N safety guarantee.
//
// Note: V2 != V1. V2 ranks by effective_score primarily (cost as tiebreaker),
// V1 ranks by cost primarily (score as tiebreaker). They're different
// policies by design. This test asserts V2's INTERNAL invariant: small-N
// data must not influence routing.
func TestRouteForTaskV2_BelowThresholdHistoricalIgnored(t *testing.T) {
	ps := minimalStoreForTests(t)
	pricing := LoadEmbeddedPricing()
	ctx := RouteContext{
		TaskText: "Fix typo 'recieve' on line 42",
		Role:     RoleWorker,
	}

	// Zero records — historical factor uniformly 1.0
	emptyDecision, err := ps.RouteForTaskV2(ctx, pricing, BuildCorpusIndex(nil))
	if err != nil {
		t.Fatal(err)
	}

	// 3 records all failed for haiku — below threshold, factor still 1.0
	smallNRecords := []OutcomeRecord{
		{Model: "anthropic:claude-haiku-4-5", ModelCanonical: "claude-haiku-4-5", TaskText: "fix typo", Outcome: "removed", RemovalReason: "failed"},
		{Model: "anthropic:claude-haiku-4-5", ModelCanonical: "claude-haiku-4-5", TaskText: "fix typo", Outcome: "removed", RemovalReason: "failed"},
		{Model: "anthropic:claude-haiku-4-5", ModelCanonical: "claude-haiku-4-5", TaskText: "fix typo", Outcome: "removed", RemovalReason: "failed"},
	}
	smallNDecision, err := ps.RouteForTaskV2(ctx, pricing, BuildCorpusIndex(smallNRecords))
	if err != nil {
		t.Fatal(err)
	}

	if emptyDecision.ChosenModel != smallNDecision.ChosenModel {
		t.Errorf("small-N corpus changed routing decision (must not, below threshold): "+
			"empty=%s smallN=%s. smallN.reason=%s",
			emptyDecision.ChosenModel, smallNDecision.ChosenModel, smallNDecision.Reason)
	}
}
