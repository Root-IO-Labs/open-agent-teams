package routing

import (
	"fmt"
	"os"
)

// ContextRegistryEntry holds hand-curated metadata for a model when probe-time
// fallbacks couldn't resolve the context window. See
// model-routing/context-registry.yaml for the authoritative source.
type ContextRegistryEntry struct {
	ModelID         string
	MaxInputTokens  int64
	MaxOutputTokens int64
	Source          string
	LastVerified    string
	Notes           string
}

// ContextRegistry is a lookup of model_id → ContextRegistryEntry.
// Nil-safe: all methods handle a nil receiver as "empty registry."
type ContextRegistry struct {
	entries map[string]*ContextRegistryEntry
}

// LoadContextRegistry reads a context-registry YAML into memory. The file
// format is nested (`models: { "<id>": { max_input_tokens: N, ... } }`) but
// we parse it with a minimal indent-aware state machine to avoid taking a
// yaml.v3 dependency just for this. See context_registry_embed.go for the
// shared parser (parseRegistryContent).
//
// Returns (nil, nil) if the file doesn't exist — callers should treat missing
// registry as "no overrides available" rather than an error.
func LoadContextRegistry(path string) (*ContextRegistry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read registry %s: %w", path, err)
	}
	r := &ContextRegistry{entries: map[string]*ContextRegistryEntry{}}
	parseRegistryContent(r, string(data))
	return r, nil
}

// Lookup returns the registry entry for a given model ID, or nil if absent.
// Nil-safe.
func (r *ContextRegistry) Lookup(modelID string) *ContextRegistryEntry {
	if r == nil {
		return nil
	}
	return r.entries[modelID]
}

// MaxInputTokens returns the registered max_input_tokens for a model, or 0 if
// the model isn't in the registry.
func (r *ContextRegistry) MaxInputTokens(modelID string) int64 {
	if r == nil {
		return 0
	}
	if e, ok := r.entries[modelID]; ok {
		return e.MaxInputTokens
	}
	return 0
}

// Count returns the number of registered models. Useful for diagnostics.
func (r *ContextRegistry) Count() int {
	if r == nil {
		return 0
	}
	return len(r.entries)
}

// ApplyToProfile fills in MaxInputTokens and EffectiveContextClass on a
// profile IF they are missing / unknown. Returns true if the profile was
// modified. Safe to call on every loaded profile — profiles with populated
// windows are untouched.
func (r *ContextRegistry) ApplyToProfile(p *ModelProfile) bool {
	if r == nil || p == nil {
		return false
	}
	e := r.Lookup(p.ModelID)
	if e == nil || e.MaxInputTokens <= 0 {
		return false
	}

	modified := false
	if p.MaxInputTokens <= 0 {
		p.MaxInputTokens = e.MaxInputTokens
		modified = true
	}
	if p.EffectiveContextClass == "" || p.EffectiveContextClass == "unknown" {
		switch {
		case e.MaxInputTokens >= 500_000:
			p.EffectiveContextClass = "large"
		case e.MaxInputTokens >= 100_000:
			p.EffectiveContextClass = "medium"
		case e.MaxInputTokens > 0:
			p.EffectiveContextClass = "small"
		}
		modified = true
	}
	return modified
}
