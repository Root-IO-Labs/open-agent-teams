package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultLiteLLMURL is the canonical LiteLLM pricing manifest. Updated
// continuously by the LiteLLM project, no auth required.
const DefaultLiteLLMURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

// liteLLMEntry mirrors the subset of fields we consume from each LiteLLM
// model entry. Costs are USD per single token (not per 1M); we multiply by
// 1e6 when surfacing them as ModelPricing.
type liteLLMEntry struct {
	InputCostPerToken           float64 `json:"input_cost_per_token"`
	OutputCostPerToken          float64 `json:"output_cost_per_token"`
	CacheReadInputTokenCost     float64 `json:"cache_read_input_token_cost"`
	CacheCreationInputTokenCost float64 `json:"cache_creation_input_token_cost"`
	LiteLLMProvider             string  `json:"litellm_provider"`
	Mode                        string  `json:"mode"`
}

// fetchLiteLLM GETs the manifest URL and returns the decoded model→entry
// map. Bounded by the request context. The "sample_spec" key is filtered
// out — LiteLLM uses it as a schema marker, not real model data.
func fetchLiteLLM(ctx context.Context, client *http.Client, url string) (map[string]liteLLMEntry, error) {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Drain a small head for diagnostics; ignore errors past the cap.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GET %s: HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	// Decode into json.RawMessage first so a single malformed entry can't
	// kill the whole map (LiteLLM occasionally adds non-numeric "max_*"
	// fields that would explode strict typing for unrelated entries).
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode JSON: %w", err)
	}
	out := make(map[string]liteLLMEntry, len(raw))
	for k, v := range raw {
		if k == "sample_spec" {
			continue
		}
		var e liteLLMEntry
		if err := json.Unmarshal(v, &e); err != nil {
			continue
		}
		out[k] = e
	}
	return out, nil
}

// litellmCandidate is one (key, expected-provider) shot at finding our model
// in the LiteLLM manifest. We try them in order and keep the first match
// whose litellm_provider is in the whitelist for that namespace.
type litellmCandidate struct {
	key               string
	providerWhitelist map[string]struct{}
}

// candidateLiteLLMKeys returns ordered candidates to look up `ourKey` in the
// LiteLLM manifest. Returns nil for namespaces LiteLLM doesn't track
// (ollama:, spark:, local:).
//
// Mapping rules — derived from inspection of model_prices_and_context_window.json:
//
//	anthropic:<m>          → "<m>"            with provider == "anthropic"
//	                         "anthropic/<m>"  (newer LiteLLM convention)
//	openai:<m>             → "<m>"            with provider == "openai"
//	                         "openai/<m>"
//	google_genai:<m>       → "gemini/<m>"     with provider in {gemini, vertex_ai-language-models}
//	                         "<m>"
//	openrouter:<v>/<m>[:t] → "openrouter/<v>/<m>"  with provider == "openrouter"
//	                         (":t" variant suffix stripped — openrouter prices
//	                         don't differ by route variant in the manifest)
func candidateLiteLLMKeys(ourKey string) []litellmCandidate {
	parts := strings.SplitN(ourKey, ":", 2)
	if len(parts) != 2 {
		return nil
	}
	provider, model := parts[0], parts[1]

	mk := func(s ...string) map[string]struct{} {
		m := make(map[string]struct{}, len(s))
		for _, p := range s {
			m[p] = struct{}{}
		}
		return m
	}

	switch provider {
	case "anthropic":
		return []litellmCandidate{
			{key: model, providerWhitelist: mk("anthropic")},
			{key: "anthropic/" + model, providerWhitelist: mk("anthropic")},
		}
	case "openai":
		return []litellmCandidate{
			{key: model, providerWhitelist: mk("openai")},
			{key: "openai/" + model, providerWhitelist: mk("openai")},
		}
	case "google_genai":
		return []litellmCandidate{
			{key: "gemini/" + model, providerWhitelist: mk("gemini", "vertex_ai-language-models", "google")},
			{key: model, providerWhitelist: mk("gemini", "vertex_ai-language-models", "google")},
		}
	case "openrouter":
		// Strip ":variant" suffix (e.g., ":nitro", ":free"). LiteLLM's
		// openrouter entries are keyed by base vendor/model only.
		bare := model
		if idx := strings.LastIndex(bare, ":"); idx >= 0 {
			bare = bare[:idx]
		}
		return []litellmCandidate{
			{key: "openrouter/" + bare, providerWhitelist: mk("openrouter")},
		}
	default:
		// ollama, spark, local — not in LiteLLM's universe.
		return nil
	}
}

// resolveLiteLLM finds the best LiteLLM entry for `ourKey` in `manifest`,
// or returns ("", nil) if no candidate matches with the right provider tag.
func resolveLiteLLM(ourKey string, manifest map[string]liteLLMEntry) (string, *liteLLMEntry) {
	for _, c := range candidateLiteLLMKeys(ourKey) {
		e, ok := manifest[c.key]
		if !ok {
			continue
		}
		if _, ok := c.providerWhitelist[e.LiteLLMProvider]; !ok {
			continue
		}
		// Skip embeddings/audio entries that share names with chat models.
		if e.Mode != "" && e.Mode != "chat" && e.Mode != "completion" {
			continue
		}
		return c.key, &e
	}
	return "", nil
}

// resolvedPrice is the per-Mtok view of a LiteLLM entry, ready to overlay
// onto an existing ModelPricing.
type resolvedPrice struct {
	OurKey             string
	LiteLLMKey         string
	InputPerMtok       float64
	OutputPerMtok      float64
	CacheReadPerMtok   float64
	CacheCreatePerMtok float64
}

// resolveAll iterates every key in the embedded registry and returns the
// LiteLLM-resolved price for those that match. Keys with no LiteLLM entry
// (ollama:*, spark:*, future models LiteLLM hasn't cataloged yet) are
// silently skipped — the caller falls back to the embedded price.
func resolveAll(embedded *PricingRegistry, manifest map[string]liteLLMEntry) []resolvedPrice {
	if embedded == nil {
		return nil
	}
	out := make([]resolvedPrice, 0, len(embedded.entries))
	for ourKey := range embedded.entries {
		llKey, e := resolveLiteLLM(ourKey, manifest)
		if e == nil {
			continue
		}
		out = append(out, resolvedPrice{
			OurKey:             ourKey,
			LiteLLMKey:         llKey,
			InputPerMtok:       e.InputCostPerToken * 1e6,
			OutputPerMtok:      e.OutputCostPerToken * 1e6,
			CacheReadPerMtok:   e.CacheReadInputTokenCost * 1e6,
			CacheCreatePerMtok: e.CacheCreationInputTokenCost * 1e6,
		})
	}
	return out
}
