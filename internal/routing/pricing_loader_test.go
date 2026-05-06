package routing

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// fixedNow returns a clock that always reports `t`, for deterministic
// freshness checks.
func fixedNow(t time.Time) func() time.Time { return func() time.Time { return t } }

// fixtureManifest is the LiteLLM JSON used across loader tests. Three
// entries: an Anthropic chat model that should overlay our embedded
// price, an OpenAI model with a different price tier, and an embedding
// model that must be rejected.
const fixtureManifest = `{
  "sample_spec": {"input_cost_per_token": 1.0},
  "claude-sonnet-4-6": {
    "input_cost_per_token": 2.5e-06,
    "output_cost_per_token": 12e-06,
    "cache_read_input_token_cost": 2.5e-07,
    "cache_creation_input_token_cost": 3.125e-06,
    "litellm_provider": "anthropic",
    "mode": "chat"
  },
  "gpt-5.4-mini": {
    "input_cost_per_token": 0.35e-06,
    "output_cost_per_token": 1.4e-06,
    "litellm_provider": "openai",
    "mode": "chat"
  },
  "text-embedding-3-large": {
    "input_cost_per_token": 0.13e-06,
    "litellm_provider": "openai",
    "mode": "embedding"
  }
}`

func newFixtureServer(t *testing.T, body string) (*httptest.Server, *int64) {
	t.Helper()
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestLoadPricingWithRemote_FirstRunUsesEmbeddedAndKicksFetch(t *testing.T) {
	srv, hits := newFixtureServer(t, fixtureManifest)
	cacheDir := t.TempDir()
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)

	opts := PricingLoaderOptions{
		URL:        srv.URL,
		CachePath:  filepath.Join(cacheDir, "pricing-cache.json"),
		TTL:        24 * time.Hour,
		Now:        fixedNow(now),
		HTTPClient: srv.Client(),
		Sync:       true, // deterministic for the test
	}
	reg := LoadPricingWithRemote(opts)

	// First run: no cache existed, so we got embedded values back.
	embedded := LoadEmbeddedPricing()
	if got := reg.InputPrice("anthropic:claude-sonnet-4-6"); got != embedded.InputPrice("anthropic:claude-sonnet-4-6") {
		t.Errorf("first run should return embedded price, got %v want %v", got, embedded.InputPrice("anthropic:claude-sonnet-4-6"))
	}
	// And the background refresh (Sync=true) should have written a cache.
	if atomic.LoadInt64(hits) != 1 {
		t.Errorf("expected 1 fetch hit, got %d", atomic.LoadInt64(hits))
	}
	cache, err := readCacheFile(opts.CachePath)
	if err != nil {
		t.Fatalf("cache should exist after sync refresh: %v", err)
	}
	if _, ok := cache.Entries["anthropic:claude-sonnet-4-6"]; !ok {
		t.Error("cache should contain the resolved sonnet price")
	}
}

func TestLoadPricingWithRemote_SecondRunUsesCache(t *testing.T) {
	srv, hits := newFixtureServer(t, fixtureManifest)
	cacheDir := t.TempDir()
	t0 := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)

	// Prime: first call writes cache.
	opts := PricingLoaderOptions{
		URL:        srv.URL,
		CachePath:  filepath.Join(cacheDir, "pricing-cache.json"),
		TTL:        24 * time.Hour,
		Now:        fixedNow(t0),
		HTTPClient: srv.Client(),
		Sync:       true,
	}
	_ = LoadPricingWithRemote(opts)
	if atomic.LoadInt64(hits) != 1 {
		t.Fatalf("priming fetch: expected 1 hit, got %d", atomic.LoadInt64(hits))
	}

	// 1 hour later, well within TTL. Should NOT refetch.
	opts.Now = fixedNow(t0.Add(1 * time.Hour))
	reg := LoadPricingWithRemote(opts)
	if atomic.LoadInt64(hits) != 1 {
		t.Errorf("fresh-cache run should not refetch; hits = %d", atomic.LoadInt64(hits))
	}

	// And the cache value (LiteLLM-derived $2.50) should override the
	// embedded value ($3.00) for sonnet.
	got := reg.InputPrice("anthropic:claude-sonnet-4-6")
	if got != 2.5 {
		t.Errorf("cache overlay failed: input price = %v, want 2.5 (from LiteLLM fixture)", got)
	}
}

func TestLoadPricingWithRemote_StaleCacheTriggersRefresh(t *testing.T) {
	srv, hits := newFixtureServer(t, fixtureManifest)
	cacheDir := t.TempDir()
	t0 := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)

	opts := PricingLoaderOptions{
		URL:        srv.URL,
		CachePath:  filepath.Join(cacheDir, "pricing-cache.json"),
		TTL:        24 * time.Hour,
		Now:        fixedNow(t0),
		HTTPClient: srv.Client(),
		Sync:       true,
	}
	_ = LoadPricingWithRemote(opts) // hits=1
	// 25 hours later — past TTL. Should still return cached value, but
	// also kick off a refresh.
	opts.Now = fixedNow(t0.Add(25 * time.Hour))
	reg := LoadPricingWithRemote(opts)
	if atomic.LoadInt64(hits) != 2 {
		t.Errorf("stale cache should trigger refresh; hits = %d, want 2", atomic.LoadInt64(hits))
	}
	if got := reg.InputPrice("anthropic:claude-sonnet-4-6"); got != 2.5 {
		t.Errorf("stale-cache run should still return cached value, got %v", got)
	}
}

func TestLoadPricingWithRemote_NetworkFailureFallsBack(t *testing.T) {
	// Server always 500s — fetch must fail. With no cache, embedded wins.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	opts := PricingLoaderOptions{
		URL:        srv.URL,
		CachePath:  filepath.Join(cacheDir, "pricing-cache.json"),
		TTL:        24 * time.Hour,
		Now:        fixedNow(time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)),
		HTTPClient: srv.Client(),
		Sync:       true,
	}
	reg := LoadPricingWithRemote(opts)
	embedded := LoadEmbeddedPricing()
	if got := reg.InputPrice("anthropic:claude-sonnet-4-6"); got != embedded.InputPrice("anthropic:claude-sonnet-4-6") {
		t.Errorf("network failure should fall back to embedded; got %v want %v", got, embedded.InputPrice("anthropic:claude-sonnet-4-6"))
	}
	// Make sure no garbage cache was written.
	if _, err := readCacheFile(opts.CachePath); err == nil {
		t.Error("failed fetch should not write a cache file")
	}
}

func TestLoadPricingWithRemote_DisabledShortCircuits(t *testing.T) {
	srv, hits := newFixtureServer(t, fixtureManifest)
	cacheDir := t.TempDir()

	opts := PricingLoaderOptions{
		URL:        srv.URL,
		CachePath:  filepath.Join(cacheDir, "pricing-cache.json"),
		HTTPClient: srv.Client(),
		Disabled:   true,
		Sync:       true,
	}
	_ = LoadPricingWithRemote(opts)
	if atomic.LoadInt64(hits) != 0 {
		t.Errorf("Disabled=true must not hit the network; hits = %d", atomic.LoadInt64(hits))
	}
}

func TestLoadPricingWithRemote_EmbeddedOnlyKeysSurvive(t *testing.T) {
	// The merged registry MUST still know about ollama:* and spark:* —
	// LiteLLM doesn't catalog them, and our overlay must not drop them.
	srv, _ := newFixtureServer(t, fixtureManifest)
	cacheDir := t.TempDir()
	t0 := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)

	opts := PricingLoaderOptions{
		URL:        srv.URL,
		CachePath:  filepath.Join(cacheDir, "pricing-cache.json"),
		TTL:        24 * time.Hour,
		Now:        fixedNow(t0),
		HTTPClient: srv.Client(),
		Sync:       true,
	}
	// Prime cache.
	_ = LoadPricingWithRemote(opts)
	// Re-load from cache (simulating subsequent daemon start).
	opts.Now = fixedNow(t0.Add(1 * time.Hour))
	reg := LoadPricingWithRemote(opts)

	for _, key := range []string{"ollama:qwen2.5:3b", "ollama:gemma3:1b", "spark:bg-digitalservices/Gemma-4-26B-A4B-it-NVFP4"} {
		if reg.Lookup(key) == nil {
			t.Errorf("embedded-only key %q dropped during overlay", key)
		}
	}
}

func TestLoadPricingWithRemote_OverlayPreservesEmbeddedWhenLiteLLMHasZero(t *testing.T) {
	// Defensive: if LiteLLM ever publishes a stub entry with zero rates,
	// we should keep the embedded value rather than mark the model free.
	zeroBody := `{
		"claude-sonnet-4-6": {
			"input_cost_per_token": 0,
			"output_cost_per_token": 0,
			"litellm_provider": "anthropic",
			"mode": "chat"
		}
	}`
	srv, _ := newFixtureServer(t, zeroBody)
	cacheDir := t.TempDir()
	t0 := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)

	opts := PricingLoaderOptions{
		URL:        srv.URL,
		CachePath:  filepath.Join(cacheDir, "pricing-cache.json"),
		TTL:        24 * time.Hour,
		Now:        fixedNow(t0),
		HTTPClient: srv.Client(),
		Sync:       true,
	}
	_ = LoadPricingWithRemote(opts)
	opts.Now = fixedNow(t0.Add(1 * time.Hour))
	reg := LoadPricingWithRemote(opts)

	embedded := LoadEmbeddedPricing()
	if got := reg.InputPrice("anthropic:claude-sonnet-4-6"); got != embedded.InputPrice("anthropic:claude-sonnet-4-6") {
		t.Errorf("zero-stub LiteLLM entry should be ignored; got %v want %v", got, embedded.InputPrice("anthropic:claude-sonnet-4-6"))
	}
}

func TestReadWriteCacheFile_RoundTrip(t *testing.T) {
	cacheDir := t.TempDir()
	path := filepath.Join(cacheDir, "nested", "pricing-cache.json")
	c := &pricingCacheFile{
		Version:   pricingCacheVersion,
		FetchedAt: time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC),
		Source:    "https://example/price.json",
		Entries: map[string]cachedPrice{
			"anthropic:claude-sonnet-4-6": {
				LiteLLMKey:    "claude-sonnet-4-6",
				InputPerMtok:  2.5,
				OutputPerMtok: 12,
			},
		},
	}
	if err := writeCacheFile(path, c); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := readCacheFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Entries["anthropic:claude-sonnet-4-6"].InputPerMtok != 2.5 {
		t.Errorf("roundtrip lost input price: %+v", got.Entries)
	}
}

func TestReadCacheFile_VersionMismatch(t *testing.T) {
	cacheDir := t.TempDir()
	path := filepath.Join(cacheDir, "pricing-cache.json")
	if err := writeCacheFile(path, &pricingCacheFile{
		Version:   pricingCacheVersion + 99, // future schema
		FetchedAt: time.Now(),
		Entries:   map[string]cachedPrice{},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := readCacheFile(path); err == nil {
		t.Error("expected version-mismatch error, got nil")
	}
}
