package routing

import (
	"fmt"
	"sort"
)

// RouteDecision is the full record of a RouteForTask call.
type RouteDecision struct {
	ChosenModel   string
	RoutingSource string // label; callers should use daemon.RoutingSource* constants
	Complexity    TaskComplexity
	Reason        string
	Features      TaskFeatures
	// Candidates considered (after hard-constraint filter), sorted by the
	// ranking function. First entry is the pick.
	Candidates []string
}

// RouteContext bundles inputs for a single routing decision. Callers
// populate the fields they have; zero values are OK.
type RouteContext struct {
	TaskText         string
	Role             AgentRole
	AllowedModels    []string // repo allowlist, empty = no restriction
	PreferredModel   string   // operator --model flag, empty = router picks
	MinContextTokens int64    // task hint; 0 = no constraint
}

// tierOrder is the ordered list of model IDs from cheapest-weakest to
// most-capable-expensive. Position in this slice = "tier index" that the
// complexity-floor map below keys against.
//
// This is a hand-maintained shipping default. Operators who want different
// ordering can override per-repo via future `oat config ... model-order` —
// not implemented yet.
var tierOrder = []string{
	"ollama:gemma3:1b",                           // 0 — restricted
	"ollama:gemma4",                              // 1
	"ollama:qwen2.5:3b",                          // 1
	"openai:gpt-5.4-nano",                        // 2
	"google_genai:gemini-3.1-flash-lite-preview", // 2
	"openrouter:meta-llama/llama-4-scout",        // 3
	"google_genai:gemini-2.5-flash",              // 3
	"openai:gpt-5.4-mini",                        // 3
	"openrouter:deepseek/deepseek-v3.2:nitro",    // 4
	"anthropic:claude-haiku-4-5",                 // 5
	"anthropic:claude-sonnet-4-6",                // 6
	"openai:o4-mini",                             // 7 — reasoning-heavy
}

// complexityFloor maps task complexity → minimum tier-index. Tier indices
// correspond to positions in `tierOrder` above. Picked so that:
//
//	trivial   → nano / flash-lite tier (cheap, one-shot fixes)
//	simple    → same cheap tier floor — "Add a flag" / "update README"
//	standard  → flash / gpt-mini tier — multi-step implementation work
//	complex   → haiku+ — multi-file refactors, anything needing planning
//
// The complex floor (9) deliberately excludes flash/mini because the
// baseline suite showed those tiers fail on multi-file refactors. Kept
// under active review — if a cheaper model proves competent via online
// data, the floor drops.
var complexityFloor = map[TaskComplexity]int{
	ComplexityTrivial:  3, // gpt-5.4-nano / flash-lite
	ComplexitySimple:   3, // same floor
	ComplexityStandard: 6, // gemini-2.5-flash+
	ComplexityComplex:  9, // haiku+ (no flash/mini for refactors)
	ComplexityUnknown:  9, // conservative fallback — unknown → assume complex
}

// tierIndexOf returns the position of a model in tierOrder, or -1 if absent.
func tierIndexOf(modelID string) int {
	for i, m := range tierOrder {
		if m == modelID {
			return i
		}
	}
	return -1
}

// RouteForTask is the cost-aware routing entry point. Feature-flagged in
// the daemon via OAT_ROUTING_V1 — the existing resolveAndValidateModel
// path stays the default until this is proven.
//
// Policy:
//  1. If operator specified PreferredModel, respect it (route_source=operator-explicit).
//  2. Classify the task. Compute the minimum acceptable tier.
//  3. Among eligible profiles (role + allowlist), find the cheapest whose
//     tier index >= floor.
//  4. Break ties by input-price ascending, then tier index ascending.
//  5. If no model meets the floor, fall back to the highest-tier eligible
//     model and flag it as `floor_violated` in the reason.
//
// Returns an error only when no eligible model exists at all. Callers should
// treat that the same way they treat resolveAndValidateModel's "no eligible
// profiles" case.
func (ps *ProfileStore) RouteForTask(ctx RouteContext, pricing *PricingRegistry) (*RouteDecision, error) {
	features := ExtractFeatures(ctx.TaskText)
	floor, ok := complexityFloor[features.Complexity]
	if !ok {
		floor = complexityFloor[ComplexityUnknown]
	}

	// Explicit model respected as-is (validation happens upstream).
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

	// Build eligible set: worker/orchestrator-eligible + in allowlist (if any).
	var eligible []*ModelProfile
	if len(ctx.AllowedModels) > 0 && ctx.Role == RoleWorker {
		eligible = ps.EligibleFiltered(ctx.Role, ctx.AllowedModels)
	} else {
		eligible = ps.Eligible(ctx.Role)
	}
	if len(eligible) == 0 {
		return nil, fmt.Errorf("RouteForTask: no eligible models for role=%s", ctx.Role)
	}

	// Apply context-window hard constraint if task has one.
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
		// If no model meets the context constraint we fall through with the
		// original eligible set — the router prefers trying with a too-small
		// window (router can fail loudly) over refusing entirely.
	}

	// Rank candidates. Primary key: meets floor (desc true-first).
	// Secondary: input price (ascending — cheaper first).
	// Tertiary: overall_score (descending — better-scored first).
	// Quaternary: tier index ascending (weakest acceptable first).
	ranked := append(make([]*ModelProfile, 0, len(eligible)), eligible...)
	sort.SliceStable(ranked, func(i, j int) bool {
		ti := tierIndexOf(ranked[i].ModelID)
		tj := tierIndexOf(ranked[j].ModelID)
		floorI := ti >= floor
		floorJ := tj >= floor

		if floorI != floorJ {
			return floorI // meets-floor first
		}

		priceI := pricing.InputPrice(ranked[i].ModelID)
		priceJ := pricing.InputPrice(ranked[j].ModelID)
		if priceI != priceJ {
			// Treat 0 (unknown-or-free) as MORE expensive than any known
			// price, so unpriced models don't win by default. Unknown
			// pricing means we can't rank by cost — push them to the
			// back of the floor-passing group.
			if priceI == 0 {
				return false
			}
			if priceJ == 0 {
				return true
			}
			return priceI < priceJ // cheaper first
		}

		// Tied on price — prefer higher overall_score.
		if ranked[i].OverallScore != ranked[j].OverallScore {
			return ranked[i].OverallScore > ranked[j].OverallScore
		}

		return ti < tj
	})

	chosen := ranked[0]
	meetsFloor := tierIndexOf(chosen.ModelID) >= floor

	source := "router-v1-auto"
	reason := fmt.Sprintf("complexity=%s floor=%d chose %s (tier=%d, $%.2f/Mtok in)",
		features.Complexity, floor, chosen.ModelID,
		tierIndexOf(chosen.ModelID), pricing.InputPrice(chosen.ModelID))
	if !meetsFloor {
		reason = "floor_violated: " + reason
	}

	candidates := make([]string, 0, len(ranked))
	for _, p := range ranked {
		candidates = append(candidates, p.ModelID)
	}

	return &RouteDecision{
		ChosenModel:   chosen.ModelID,
		RoutingSource: source,
		Complexity:    features.Complexity,
		Features:      features,
		Reason:        reason,
		Candidates:    candidates,
	}, nil
}
