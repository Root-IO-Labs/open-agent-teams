package routing

import "testing"

func TestPricingRegistry_SnapshotID_StableForSameContent(t *testing.T) {
	a := &PricingRegistry{entries: map[string]*ModelPricing{
		"openai:gpt-5.4-mini": {InputPerMtok: 0.40, OutputPerMtok: 1.60},
		"anthropic:claude-haiku-4-5": {
			InputPerMtok: 1.00, OutputPerMtok: 5.00,
			CacheCreationPerMtok: 1.25, HasCacheCreation: true,
		},
	}}
	b := &PricingRegistry{entries: map[string]*ModelPricing{
		// Same data, different insertion order. Map iteration is randomized
		// in Go; SnapshotID must canonicalize.
		"anthropic:claude-haiku-4-5": {
			InputPerMtok: 1.00, OutputPerMtok: 5.00,
			CacheCreationPerMtok: 1.25, HasCacheCreation: true,
		},
		"openai:gpt-5.4-mini": {InputPerMtok: 0.40, OutputPerMtok: 1.60},
	}}

	if a.SnapshotID() != b.SnapshotID() {
		t.Fatalf("same content → different hashes: %s vs %s", a.SnapshotID(), b.SnapshotID())
	}
	if len(a.SnapshotID()) != 16 {
		t.Errorf("hash length %d, want 16", len(a.SnapshotID()))
	}
}

func TestPricingRegistry_SnapshotID_ChangesOnPriceMutation(t *testing.T) {
	base := &PricingRegistry{entries: map[string]*ModelPricing{
		"openai:gpt-5.4-mini": {InputPerMtok: 0.40, OutputPerMtok: 1.60},
	}}
	mutated := &PricingRegistry{entries: map[string]*ModelPricing{
		"openai:gpt-5.4-mini": {InputPerMtok: 0.41, OutputPerMtok: 1.60}, // 1 cent change
	}}
	if base.SnapshotID() == mutated.SnapshotID() {
		t.Fatal("price mutation produced identical hash")
	}
}

func TestPricingRegistry_SnapshotID_ChangesOnAddedModel(t *testing.T) {
	base := &PricingRegistry{entries: map[string]*ModelPricing{
		"openai:gpt-5.4-mini": {InputPerMtok: 0.40, OutputPerMtok: 1.60},
	}}
	added := &PricingRegistry{entries: map[string]*ModelPricing{
		"openai:gpt-5.4-mini":        {InputPerMtok: 0.40, OutputPerMtok: 1.60},
		"anthropic:claude-haiku-4-5": {InputPerMtok: 1.00, OutputPerMtok: 5.00},
	}}
	if base.SnapshotID() == added.SnapshotID() {
		t.Fatal("adding a model produced identical hash")
	}
}

func TestPricingRegistry_SnapshotID_NilOrEmptyReturnsEmpty(t *testing.T) {
	var nilReg *PricingRegistry
	if nilReg.SnapshotID() != "" {
		t.Errorf("nil registry: want empty, got %q", nilReg.SnapshotID())
	}
	empty := &PricingRegistry{entries: map[string]*ModelPricing{}}
	if empty.SnapshotID() != "" {
		t.Errorf("empty registry: want empty, got %q", empty.SnapshotID())
	}
}

func TestPricingRegistry_SnapshotID_DistinguishesHasCacheCreation(t *testing.T) {
	// Same numeric prices but different HasCacheCreation flag. The flag
	// affects the "use fallback vs explicit" lookup, so it must be part of
	// the hash.
	implicit := &PricingRegistry{entries: map[string]*ModelPricing{
		"anthropic:claude-haiku-4-5": {
			InputPerMtok: 1.00, CacheCreationPerMtok: 0, HasCacheCreation: false,
		},
	}}
	explicit := &PricingRegistry{entries: map[string]*ModelPricing{
		"anthropic:claude-haiku-4-5": {
			InputPerMtok: 1.00, CacheCreationPerMtok: 0, HasCacheCreation: true,
		},
	}}
	if implicit.SnapshotID() == explicit.SnapshotID() {
		t.Fatal("HasCacheCreation flag not reflected in hash")
	}
}
