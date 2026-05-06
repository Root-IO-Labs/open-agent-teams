package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// DefaultPricingTTL is how long a cache entry counts as "fresh" before we
// kick off a background refresh. Stale-but-present cache is still used —
// users always see the last-known-good prices, network or not.
const DefaultPricingTTL = 24 * time.Hour

// pricingCacheFile is the on-disk shape of the price cache. Stored under
// $OAT_HOME/pricing-cache.json. Versioned so we can tolerate schema drift.
type pricingCacheFile struct {
	Version   int                    `json:"version"`
	FetchedAt time.Time              `json:"fetched_at"`
	Source    string                 `json:"source"`
	Entries   map[string]cachedPrice `json:"entries"`
}

type cachedPrice struct {
	LiteLLMKey         string  `json:"litellm_key"`
	InputPerMtok       float64 `json:"input_per_mtok"`
	OutputPerMtok      float64 `json:"output_per_mtok"`
	CacheReadPerMtok   float64 `json:"cache_read_per_mtok"`
	CacheCreatePerMtok float64 `json:"cache_create_per_mtok"`
}

const pricingCacheVersion = 1

// PricingLoaderOptions configures the cache-aware, network-aware loader.
// The zero value is unsafe — use NewPricingLoaderOptions to get sane
// defaults. All fields are intentionally exported so tests can inject a
// fake clock, a fake URL (httptest server), and a non-default cache path.
type PricingLoaderOptions struct {
	// URL is the LiteLLM JSON manifest. Defaults to DefaultLiteLLMURL.
	URL string

	// CachePath is where the resolved pricing snapshot is persisted.
	// Defaults to $OAT_HOME/pricing-cache.json (caller supplies $OAT_HOME).
	CachePath string

	// TTL is how long a cache file is considered fresh. Defaults to 24h.
	TTL time.Duration

	// Now lets tests freeze the clock. Defaults to time.Now.
	Now func() time.Time

	// HTTPClient is used for the manifest fetch. Defaults to a 10s-timeout
	// client. Tests inject httptest.Server.Client().
	HTTPClient *http.Client

	// Log surfaces non-fatal events (cache miss, fetch failure, write
	// failure). Defaults to a no-op so callers don't need to wire one up.
	Log func(format string, args ...any)

	// Disabled forces "embedded only" mode. When true, no cache is read,
	// no network is touched. Used by OAT_PRICING_REMOTE=0.
	Disabled bool

	// Sync forces the refetch path to run synchronously rather than in a
	// background goroutine. Defaults to false. Tests set true for
	// determinism; production keeps it false to never block daemon start.
	Sync bool
}

// NewPricingLoaderOptions returns options with sane defaults for a daemon
// rooted at oatHome (typically `$HOME/.oat/`).
func NewPricingLoaderOptions(oatHome string) PricingLoaderOptions {
	return PricingLoaderOptions{
		URL:        DefaultLiteLLMURL,
		CachePath:  filepath.Join(oatHome, "pricing-cache.json"),
		TTL:        DefaultPricingTTL,
		Now:        time.Now,
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
		Log:        func(string, ...any) {},
	}
}

func (o *PricingLoaderOptions) normalize() {
	if o.URL == "" {
		o.URL = DefaultLiteLLMURL
	}
	if o.TTL <= 0 {
		o.TTL = DefaultPricingTTL
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
}

// LoadPricingWithRemote returns a PricingRegistry that prefers, in order:
//
//  1. Fresh cache (age < TTL) overlaid on embedded YAML.
//  2. Embedded YAML alone, with a background refresh kicked off.
//  3. Stale cache overlaid on embedded YAML — used only when remote fetch
//     fails and we have *some* prior data.
//
// The function never blocks on the network in normal operation. Daemon
// startup time stays in the single-digit ms range whether GitHub is
// reachable or not. The first run after install ships embedded values;
// the next start (24h later or after the background fetch completes)
// picks up LiteLLM-derived prices.
func LoadPricingWithRemote(opts PricingLoaderOptions) *PricingRegistry {
	opts.normalize()
	embedded := LoadEmbeddedPricing()

	if opts.Disabled || opts.CachePath == "" {
		return embedded
	}

	cache, cacheErr := readCacheFile(opts.CachePath)
	now := opts.Now()

	switch {
	case cacheErr == nil && now.Sub(cache.FetchedAt) < opts.TTL:
		// Fresh — use cache, no refresh needed.
		return overlayCache(embedded, cache)

	case cacheErr == nil:
		// Stale — use cache for now, refresh in background.
		opts.Log("pricing: cache stale (age %s, ttl %s), refreshing",
			now.Sub(cache.FetchedAt).Truncate(time.Minute), opts.TTL)
		opts.runRefresh(embedded)
		return overlayCache(embedded, cache)

	default:
		// No cache (first run, or unreadable). Use embedded now, fetch
		// async so the next daemon start has data.
		if !os.IsNotExist(cacheErr) {
			opts.Log("pricing: cache unreadable: %v (using embedded)", cacheErr)
		}
		opts.runRefresh(embedded)
		return embedded
	}
}

func (o *PricingLoaderOptions) runRefresh(embedded *PricingRegistry) {
	// Skip the remote fetch when running under `go test` against the
	// default URL. Daemon and routing tests construct many daemons, each
	// of which would otherwise spawn a goroutine hitting GitHub —
	// flaky in CI, wasteful even when it succeeds. Tests that exercise
	// the refresh path point URL at an httptest.Server, so they're
	// excluded from this guard and continue to run normally.
	if testing.Testing() && (o.URL == "" || o.URL == DefaultLiteLLMURL) {
		return
	}
	if o.Sync {
		o.refreshOnce(embedded)
		return
	}
	go o.refreshOnce(embedded)
}

func (o *PricingLoaderOptions) refreshOnce(embedded *PricingRegistry) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	manifest, err := fetchLiteLLM(ctx, o.HTTPClient, o.URL)
	if err != nil {
		o.Log("pricing: remote fetch failed: %v", err)
		return
	}

	cache := &pricingCacheFile{
		Version:   pricingCacheVersion,
		FetchedAt: o.Now(),
		Source:    o.URL,
		Entries:   make(map[string]cachedPrice),
	}
	for _, r := range resolveAll(embedded, manifest) {
		cache.Entries[r.OurKey] = cachedPrice{
			LiteLLMKey:         r.LiteLLMKey,
			InputPerMtok:       r.InputPerMtok,
			OutputPerMtok:      r.OutputPerMtok,
			CacheReadPerMtok:   r.CacheReadPerMtok,
			CacheCreatePerMtok: r.CacheCreatePerMtok,
		}
	}
	if err := writeCacheFile(o.CachePath, cache); err != nil {
		o.Log("pricing: cache write failed: %v", err)
		return
	}
	o.Log("pricing: refreshed %d entries from %s", len(cache.Entries), o.URL)
}

// overlayCache returns a new registry with embedded as the base and
// cached entries overlaid for any key both sides agree on. Embedded-only
// keys (ollama:*, spark:*, future models LiteLLM doesn't know yet) keep
// their YAML values. Cached entries with zero input/output rates are
// ignored — LiteLLM occasionally has stub entries with no pricing, and
// we'd rather show a stale embedded price than mark a model as free.
func overlayCache(embedded *PricingRegistry, cache *pricingCacheFile) *PricingRegistry {
	out := &PricingRegistry{entries: make(map[string]*ModelPricing, len(embedded.entries))}
	for k, v := range embedded.entries {
		cp := *v
		out.entries[k] = &cp
	}
	verified := cache.FetchedAt.UTC().Format("2006-01-02")
	for ourKey, ce := range cache.Entries {
		if ce.InputPerMtok == 0 && ce.OutputPerMtok == 0 {
			continue
		}
		existing, ok := out.entries[ourKey]
		if !ok {
			existing = &ModelPricing{ModelID: ourKey, HasData: true}
			out.entries[ourKey] = existing
		}
		existing.InputPerMtok = ce.InputPerMtok
		existing.OutputPerMtok = ce.OutputPerMtok
		// LiteLLM omits cache_read for some providers — only overlay when
		// they actually published a number. Otherwise cost.go falls back
		// to input rate (the documented conservative path).
		if ce.CacheReadPerMtok > 0 {
			existing.CacheReadPerMtok = ce.CacheReadPerMtok
		}
		existing.Source = "litellm:" + ce.LiteLLMKey
		existing.LastVerified = verified
		// Clear the embedded "estimate" note — this price came from
		// LiteLLM, so trust.go should label it "verified" not "est".
		existing.Notes = ""
		existing.HasData = true
	}
	return out
}

func readCacheFile(path string) (*pricingCacheFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c pricingCacheFile
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("decode cache: %w", err)
	}
	if c.Version != pricingCacheVersion {
		return nil, fmt.Errorf("cache schema mismatch: got v%d, want v%d", c.Version, pricingCacheVersion)
	}
	return &c, nil
}

func writeCacheFile(path string, c *pricingCacheFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir cache dir: %w", err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cache: %w", err)
	}
	// Atomic-ish: write to temp + rename so a crash mid-write doesn't
	// leave a half-written cache that future daemons fail to parse.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write cache temp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename cache: %w", err)
	}
	return nil
}
