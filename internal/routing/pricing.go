package routing

import (
	"fmt"
	"os"
	"strings"
)

// ModelPricing holds per-model cost-per-million-tokens. Used by RouteForTask
// and cost-counterfactual analyses. Loaded from internal/routing/pricing.yaml.
//
// All fields are USD per million tokens. Unknown prices read as 0; use
// HasData to distinguish "not priced" from "priced at zero" (e.g. local
// Ollama models have HasData=true with 0/0/0).
type ModelPricing struct {
	ModelID              string
	InputPerMtok         float64
	OutputPerMtok        float64
	CacheReadPerMtok     float64
	CacheCreationPerMtok float64 // 0 when YAML omits it; callers use CacheCreationPriceFor() for per-provider fallback
	HasCacheCreation     bool    // true if YAML had an explicit cache_creation_per_mtok field (distinguishes "$0" from "use fallback")
	Source               string
	LastVerified         string
	Notes                string // free-text — "estimate" downgrades trust to "est"
	HasData              bool   // true when pricing was loaded for this model
}

// CacheCreationPriceFor returns USD per Mtok for cache-creation
// (cache-write) tokens. Order of resolution:
//
//  1. Explicit cache_creation_per_mtok in pricing.yaml (HasCacheCreation=true).
//  2. Per-provider fallback derived from the input price:
//     - anthropic:* → 1.25 × input (5-minute TTL default; we don't use 1h TTL).
//     - openai:*    → 0 (OpenAI doesn't bill cache writes; cached reads are
//     discounted via cache_read_per_mtok already).
//     - google* / google_genai:* → 1.0 × input (explicit cache writes billed at
//     standard input rate; storage fee not modeled).
//     - all others (deepseek, openrouter, ollama, spark, ...) → 1.0 × input
//     (conservative default; Ollama/spark entries have input=0 anyway).
//
// `modelID` may be a normalized or raw provider-prefixed ID; matching is by
// "<provider>:" prefix.
func CacheCreationPriceFor(modelID string, p *ModelPricing) float64 {
	if p != nil && p.HasCacheCreation {
		return p.CacheCreationPerMtok
	}
	inputPrice := 0.0
	if p != nil {
		inputPrice = p.InputPerMtok
	}
	id := NormalizeModelID(modelID)
	switch {
	case strings.HasPrefix(id, "anthropic:"):
		return inputPrice * 1.25
	case strings.HasPrefix(id, "openai:"):
		return 0
	case strings.HasPrefix(id, "google:"), strings.HasPrefix(id, "google_genai:"):
		return inputPrice * 1.0
	default:
		return inputPrice * 1.0
	}
}

// NormalizeModelID returns a canonical model ID for pricing lookups.
// Trims surrounding whitespace and quote characters, lowercases the
// provider prefix, and collapses redundant whitespace. Keeps the
// "<provider>:<model-id>" shape used throughout the codebase.
//
// This is intentionally conservative: we do NOT strip the provider
// prefix (pricing.yaml keys include it) -- but we do tolerate input
// like ` Anthropic:claude-sonnet-4-6 ` from log payloads.
func NormalizeModelID(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "\"' ")
	if s == "" {
		return s
	}
	if i := strings.Index(s, ":"); i > 0 {
		return strings.ToLower(s[:i]) + s[i:]
	}
	return s
}

// PricingRegistry is a keyed map of ModelPricing. Nil-safe lookups.
type PricingRegistry struct {
	entries map[string]*ModelPricing
}

// LoadPricing reads a pricing YAML from disk. Returns (nil, nil) if the
// file doesn't exist. Uses the shared indent-aware parser from
// context_registry_embed.go since the YAML shape is structurally similar.
func LoadPricing(path string) (*PricingRegistry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read pricing %s: %w", path, err)
	}
	r := &PricingRegistry{entries: map[string]*ModelPricing{}}
	parsePricingContent(r, string(data))
	return r, nil
}

// parsePricingContent runs a trimmed version of the registry parser that
// understands the pricing.yaml schema (same nested models: { id: { fields }}
// shape as context-registry).
func parsePricingContent(r *PricingRegistry, content string) {
	var (
		inModelsBlock bool
		currentModel  string
		cur           *ModelPricing
	)
	flush := func() {
		if cur != nil && currentModel != "" {
			cur.ModelID = currentModel
			cur.HasData = true
			r.entries[currentModel] = cur
		}
		currentModel = ""
		cur = nil
	}

	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		if !inModelsBlock {
			if strings.HasPrefix(trimmed, "models:") {
				inModelsBlock = true
			}
			continue
		}

		// 2-space-indented: new model entry
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "    ") {
			flush()
			key := strings.TrimSuffix(trimmed, ":")
			key = strings.Trim(key, "\"")
			currentModel = key
			cur = &ModelPricing{}
			continue
		}

		// 4+-space-indented: field
		if strings.HasPrefix(line, "    ") && cur != nil {
			parts := strings.SplitN(trimmed, ":", 2)
			if len(parts) != 2 {
				continue
			}
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			val = strings.Trim(val, "\"")
			switch key {
			case "input_per_mtok":
				if v, err := parseFloatSafe(val); err == nil {
					cur.InputPerMtok = v
				}
			case "output_per_mtok":
				if v, err := parseFloatSafe(val); err == nil {
					cur.OutputPerMtok = v
				}
			case "cache_read_per_mtok":
				if v, err := parseFloatSafe(val); err == nil {
					cur.CacheReadPerMtok = v
				}
			case "cache_creation_per_mtok":
				if v, err := parseFloatSafe(val); err == nil {
					cur.CacheCreationPerMtok = v
					cur.HasCacheCreation = true
				}
			case "source":
				cur.Source = val
			case "last_verified":
				cur.LastVerified = val
			case "notes":
				cur.Notes = val
			}
			continue
		}

		// Leaving models: block
		if !strings.HasPrefix(line, " ") {
			inModelsBlock = false
			flush()
		}
	}
	flush()
}

// Lookup returns the pricing entry for `modelID`, or nil if absent.
//
// Looks up the raw key first, then retries with NormalizeModelID(modelID)
// to tolerate provider-prefix capitalization or surrounding whitespace
// in caller-supplied IDs (e.g. log payloads).
func (r *PricingRegistry) Lookup(modelID string) *ModelPricing {
	if r == nil {
		return nil
	}
	if e, ok := r.entries[modelID]; ok {
		return e
	}
	if norm := NormalizeModelID(modelID); norm != modelID {
		if e, ok := r.entries[norm]; ok {
			return e
		}
	}
	return nil
}

// InputPrice returns USD per million input tokens for a model, or 0 if unknown.
// Unknown prices are 0 (not a sentinel) — use Lookup(...) if you need to
// distinguish "free" from "unknown."
func (r *PricingRegistry) InputPrice(modelID string) float64 {
	if e := r.Lookup(modelID); e != nil {
		return e.InputPerMtok
	}
	return 0
}

// Count is the number of priced models in the registry.
func (r *PricingRegistry) Count() int {
	if r == nil {
		return 0
	}
	return len(r.entries)
}

// parseFloatSafe: small tolerant float parser (handles empty string, trims,
// accepts digits + at most one '.'). Sufficient for price values which are
// always plain decimals in our YAML.
func parseFloatSafe(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "null" {
		return 0, errEmpty
	}
	var (
		result    float64
		frac      float64
		inFrac    bool
		fracScale = 1.0
		sign      = 1.0
	)
	for i, c := range s {
		if i == 0 && c == '-' {
			sign = -1
			continue
		}
		if c == '.' && !inFrac {
			inFrac = true
			continue
		}
		if c < '0' || c > '9' {
			return 0, errNotFloat
		}
		if inFrac {
			fracScale *= 10
			frac = frac*10 + float64(c-'0')
		} else {
			result = result*10 + float64(c-'0')
		}
	}
	return sign * (result + frac/fracScale), nil
}

var errNotFloat = stringError("not a float")
