package routing

import (
	"crypto/sha256"
	"fmt"
	"sort"
)

// SnapshotID returns a short, stable hash of the registry's contents. Used as
// `pricing_snapshot_id` on OutcomeRecord so historical cost calculations stay
// reproducible after the embedded YAML or remote LiteLLM cache changes.
//
// Determinism contract: same set of priced models with the same prices →
// same hash, every run, every machine. Adding or removing a model, or changing
// any priced field on any model, yields a different hash.
//
// Returns "" when the registry is nil or empty so the field is omitted from
// serialized records (see omitempty on PricingSnapshotID).
//
// 16-char prefix is enough collision-resistance for a key space bounded by the
// number of pricing-yaml revisions a binary will ever see (low thousands at
// most). Use the full 64-char hash only for cross-run audit.
func (r *PricingRegistry) SnapshotID() string {
	if r == nil || len(r.entries) == 0 {
		return ""
	}
	keys := make([]string, 0, len(r.entries))
	for k := range r.entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	for _, k := range keys {
		e := r.entries[k]
		// Format pinned: changing the layout silently re-keys every existing
		// record. If you must change it, bump a separate version constant
		// in this file so old hashes stay distinguishable.
		_, _ = fmt.Fprintf(h, "%s|%g|%g|%g|%g|%t\n",
			k,
			e.InputPerMtok,
			e.OutputPerMtok,
			e.CacheReadPerMtok,
			e.CacheCreationPerMtok,
			e.HasCacheCreation,
		)
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}
