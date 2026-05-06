package routing

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestE2E_LoaderFeedsAllConsumers exercises the full chain that the
// daemon uses today, in the same order it uses it:
//
//  1. LoadPricingWithRemote (replaces LoadEmbeddedPricing in daemon.go)
//  2. The returned *PricingRegistry is passed to RouteForTask, which
//     uses InputPrice() and Lookup() under the hood.
//  3. The same registry's Lookup() result is passed to budget.ComputeCost
//     (verified here by calling Lookup and exercising the same shape).
//  4. The same Lookup() result feeds budget.TrustLabel.
//
// If any of these consumers can't tolerate the new merged registry, this
// test fails. We deliberately do NOT import internal/budget here (would
// create an import cycle); instead we mirror the exact field accesses
// budget.ComputeCost / budget.TrustLabel make on *ModelPricing, so any
// regression in the registry shape surfaces here too.
func TestE2E_LoaderFeedsAllConsumers(t *testing.T) {
	// LiteLLM-shaped fixture covering models from each namespace we ship.
	fixture := `{
		"claude-sonnet-4-6": {
			"input_cost_per_token": 2.5e-06,
			"output_cost_per_token": 12e-06,
			"cache_read_input_token_cost": 2.5e-07,
			"litellm_provider": "anthropic",
			"mode": "chat"
		},
		"claude-haiku-4-5": {
			"input_cost_per_token": 0.8e-06,
			"output_cost_per_token": 4e-06,
			"cache_read_input_token_cost": 0.08e-06,
			"litellm_provider": "anthropic",
			"mode": "chat"
		},
		"gpt-5.4-mini": {
			"input_cost_per_token": 0.35e-06,
			"output_cost_per_token": 1.4e-06,
			"litellm_provider": "openai",
			"mode": "chat"
		},
		"gemini/gemini-2.5-flash": {
			"input_cost_per_token": 0.12e-06,
			"output_cost_per_token": 0.50e-06,
			"litellm_provider": "gemini",
			"mode": "chat"
		},
		"openrouter/deepseek/deepseek-v3.2": {
			"input_cost_per_token": 0.25e-06,
			"output_cost_per_token": 1.05e-06,
			"litellm_provider": "openrouter",
			"mode": "chat"
		}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fixture))
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)

	// Step 1: load via the daemon's loader path, populate cache.
	opts := PricingLoaderOptions{
		URL:        srv.URL,
		CachePath:  filepath.Join(cacheDir, "pricing-cache.json"),
		TTL:        24 * time.Hour,
		Now:        func() time.Time { return now },
		HTTPClient: srv.Client(),
		Sync:       true,
	}
	_ = LoadPricingWithRemote(opts) // priming run
	// Subsequent run reads cache and produces the registry actually used.
	opts.Now = func() time.Time { return now.Add(1 * time.Hour) }
	pricing := LoadPricingWithRemote(opts)
	if pricing == nil || pricing.Count() == 0 {
		t.Fatal("loader returned empty registry")
	}

	// Consumer 2: RouteForTask reads the registry. We use the same
	// minimalStoreForTests fixture the existing routing tests use, so
	// any field-shape mismatch would surface immediately.
	ps := minimalStoreForTests(t)
	d, err := ps.RouteForTask(RouteContext{
		TaskText: "Fix typo on line 42",
		Role:     RoleWorker,
	}, pricing)
	if err != nil {
		t.Fatalf("RouteForTask rejected the merged registry: %v", err)
	}
	if d.ChosenModel == "" {
		t.Error("RouteForTask returned no decision against the merged registry")
	}

	// Consumer 3: ComputeCost shape. Mirror the field reads exactly.
	p := pricing.Lookup("anthropic:claude-sonnet-4-6")
	if p == nil {
		t.Fatal("anthropic:claude-sonnet-4-6 missing from merged registry")
	}
	if !p.HasData {
		t.Error("HasData=false for a model that came through LiteLLM overlay")
	}
	if p.InputPerMtok != 2.5 {
		t.Errorf("InputPerMtok = %v, want 2.5 (LiteLLM overlay)", p.InputPerMtok)
	}
	if p.OutputPerMtok != 12 {
		t.Errorf("OutputPerMtok = %v, want 12", p.OutputPerMtok)
	}
	// Cost math identical to what budget.ComputeCost does:
	// 100k input - 60k cache_read = 40k non-cached
	// = 40k * 2.5/1e6 + 50k * 12/1e6 + 60k * 0.25/1e6 = 0.1 + 0.6 + 0.015 = 0.715
	gotCost := float64(40_000)*p.InputPerMtok/1e6 +
		float64(50_000)*p.OutputPerMtok/1e6 +
		float64(60_000)*p.CacheReadPerMtok/1e6
	wantCost := 0.715
	if abs(gotCost-wantCost) > 1e-9 {
		t.Errorf("ComputeCost-equivalent math = %v, want %v", gotCost, wantCost)
	}

	// Consumer 4: TrustLabel. With LiteLLM overlay, LastVerified is "today"
	// and Notes is empty (LiteLLM doesn't carry notes). Trust must be
	// "verified" — not "stale" or "est".
	if p.LastVerified != "2026-04-28" {
		t.Errorf("LastVerified after overlay = %q, want 2026-04-28", p.LastVerified)
	}
	if !strings.HasPrefix(p.Source, "litellm:") {
		t.Errorf("Source after overlay = %q, want litellm: prefix", p.Source)
	}
	if strings.Contains(strings.ToLower(p.Notes), "estimate") {
		t.Errorf("Notes after LiteLLM overlay should be empty/clean, got %q", p.Notes)
	}

	// Consumer sanity: ollama:* models still in the registry, and
	// recognized as local (input == 0 && output == 0) by budget.IsLocal logic.
	ollama := pricing.Lookup("ollama:qwen2.5:3b")
	if ollama == nil {
		t.Fatal("ollama:qwen2.5:3b dropped during overlay")
	}
	if ollama.InputPerMtok != 0 || ollama.OutputPerMtok != 0 {
		t.Errorf("ollama prices changed during overlay: input=%v output=%v", ollama.InputPerMtok, ollama.OutputPerMtok)
	}
}

// TestE2E_DaemonStartupNoNetwork simulates the worst case: GitHub is
// unreachable, no cache exists. The daemon must still boot with a usable
// pricing registry — embedded values exactly.
func TestE2E_DaemonStartupNoNetwork(t *testing.T) {
	// "Network down" — point at an unreachable URL with a tiny timeout.
	cacheDir := t.TempDir()
	opts := PricingLoaderOptions{
		URL:        "http://127.0.0.1:1/will-fail-immediately",
		CachePath:  filepath.Join(cacheDir, "pricing-cache.json"),
		TTL:        24 * time.Hour,
		Now:        time.Now,
		HTTPClient: &http.Client{Timeout: 50 * time.Millisecond},
		Sync:       true,
	}
	pricing := LoadPricingWithRemote(opts)

	// Must match embedded exactly when no cache + no network.
	embedded := LoadEmbeddedPricing()
	if pricing.Count() != embedded.Count() {
		t.Errorf("network-down loader returned %d entries, embedded has %d", pricing.Count(), embedded.Count())
	}
	if got := pricing.InputPrice("anthropic:claude-sonnet-4-6"); got != embedded.InputPrice("anthropic:claude-sonnet-4-6") {
		t.Errorf("network-down should equal embedded, got %v want %v", got, embedded.InputPrice("anthropic:claude-sonnet-4-6"))
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
