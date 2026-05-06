package routing

import _ "embed"

//go:embed pricing.yaml
var embeddedPricing string

// LoadEmbeddedPricing returns the pricing registry shipped with the binary.
// Update internal/routing/pricing.yaml and rebuild to change prices.
func LoadEmbeddedPricing() *PricingRegistry {
	r := &PricingRegistry{entries: map[string]*ModelPricing{}}
	parsePricingContent(r, embeddedPricing)
	return r
}
