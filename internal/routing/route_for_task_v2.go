package routing

import (
	"fmt"
	"sort"
)

// RouteForTaskV2 is the lookup-aware extension of V1. Same eligibility,
// allowlist, and complexity-floor logic; then re-ranks the floor-passing
// candidates using historical success rate from the provided CorpusIndex.
//
// effective_score = profile.OverallScore × historical_factor(model, complexity)
//
// Where historical_factor is HistoricalFactor() per CorpusStats —
// 1.0 (no adjustment) when N < MinHistoricalSamples, max(0.5, wilson_lo)
// otherwise. The floor stops a bad streak from fully banishing a model;
// the threshold stops noisy small-N data from dominating a static prior.
//
// Activation: daemon checks OAT_ROUTER_VERSION=v2 and calls this entrypoint
// instead of RouteForTask. V0/V1 paths remain unchanged.
//
// Determinism contract: same (ctx, pricing, corpus) → same decision.
// The corpus is passed as an argument so the caller pins the snapshot;
// concurrent backfill writes can't make this function non-deterministic.
//
// Falls through to V1-equivalent ranking when corpus is nil or empty —
// fresh-install daemons keep working without historical data.
func (ps *ProfileStore) RouteForTaskV2(
	ctx RouteContext,
	pricing *PricingRegistry,
	corpus *CorpusIndex,
) (*RouteDecision, error) {
	features := ExtractFeatures(ctx.TaskText)
	floor, ok := complexityFloor[features.Complexity]
	if !ok {
		floor = complexityFloor[ComplexityUnknown]
	}

	if ctx.PreferredModel != "" {
		return &RouteDecision{
			ChosenModel:   ctx.PreferredModel,
			RoutingSource: "operator-explicit",
			Complexity:    features.Complexity,
			Features:      features,
			Reason:        fmt.Sprintf("operator --model=%s", ctx.PreferredModel),
			Candidates:    []string{ctx.PreferredModel},
		}, nil
	}

	var eligible []*ModelProfile
	if len(ctx.AllowedModels) > 0 && ctx.Role == RoleWorker {
		eligible = ps.EligibleFiltered(ctx.Role, ctx.AllowedModels)
	} else {
		eligible = ps.Eligible(ctx.Role)
	}
	if len(eligible) == 0 {
		return nil, fmt.Errorf("RouteForTaskV2: no eligible models for role=%s", ctx.Role)
	}

	if ctx.MinContextTokens > 0 {
		filtered := make([]*ModelProfile, 0, len(eligible))
		for _, p := range eligible {
			if p.MaxInputTokens >= ctx.MinContextTokens {
				filtered = append(filtered, p)
			}
		}
		if len(filtered) > 0 {
			eligible = filtered
		}
	}

	// Pre-compute (effective_score, historical_factor, n_samples) per
	// candidate so the comparator below is cheap and the reason string can
	// surface the historical data when relevant.
	type ranked struct {
		profile           *ModelProfile
		tier              int
		meetsFloor        bool
		price             float64
		histFactor        float64
		histN             int
		effectiveScore    float64
		historicalApplied bool // true when factor != 1.0
	}
	rs := make([]ranked, len(eligible))
	for i, p := range eligible {
		bucket := corpus.Lookup(canonicalKeyForModel(p.ModelID), features.Complexity)
		factor := 1.0
		histN := 0
		applied := false
		if bucket != nil {
			factor = bucket.HistoricalFactor()
			histN = bucket.Scored
			applied = factor != 1.0
		}
		ti := tierIndexOf(p.ModelID)
		rs[i] = ranked{
			profile:           p,
			tier:              ti,
			meetsFloor:        ti >= floor,
			price:             pricing.InputPrice(p.ModelID),
			histFactor:        factor,
			histN:             histN,
			effectiveScore:    float64(p.OverallScore) * factor,
			historicalApplied: applied,
		}
	}

	sort.SliceStable(rs, func(i, j int) bool {
		// 1. Floor-passing candidates first
		if rs[i].meetsFloor != rs[j].meetsFloor {
			return rs[i].meetsFloor
		}
		// 2. Higher effective score (static × historical) wins
		if rs[i].effectiveScore != rs[j].effectiveScore {
			return rs[i].effectiveScore > rs[j].effectiveScore
		}
		// 3. Cheaper input price as tie-breaker (zero treated as "unknown,
		//    push back" to match V1 semantics).
		pi, pj := rs[i].price, rs[j].price
		if pi != pj {
			if pi == 0 {
				return false
			}
			if pj == 0 {
				return true
			}
			return pi < pj
		}
		// 4. Lower tier index (cheaper-tier) as final tiebreaker
		return rs[i].tier < rs[j].tier
	})

	chosen := rs[0]

	source := "router-v2-auto"
	reason := fmt.Sprintf(
		"complexity=%s floor=%d chose %s (tier=%d, score=%d, hist_factor=%.2f, hist_n=%d, $%.2f/Mtok in)",
		features.Complexity, floor, chosen.profile.ModelID,
		chosen.tier, chosen.profile.OverallScore,
		chosen.histFactor, chosen.histN, chosen.price,
	)
	if !chosen.meetsFloor {
		reason = "floor_violated: " + reason
	}
	if corpus == nil || corpus.IsEmpty() {
		reason += " [no corpus — V1 equivalent]"
	}

	candidates := make([]string, 0, len(rs))
	for _, r := range rs {
		candidates = append(candidates, r.profile.ModelID)
	}

	return &RouteDecision{
		ChosenModel:   chosen.profile.ModelID,
		RoutingSource: source,
		Complexity:    features.Complexity,
		Features:      features,
		Reason:        reason,
		Candidates:    candidates,
	}, nil
}

// canonicalKeyForModel returns the same canonical name the index buckets on,
// computed from a raw model ID. Mirror of canonicalKey but for ModelProfile
// IDs (which are full provider:model strings).
func canonicalKeyForModel(modelID string) string {
	canon, _ := Canonicalize(modelID)
	if canon != "" {
		return canon
	}
	return modelID
}
