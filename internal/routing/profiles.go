package routing

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// ErrAmbiguousModel is returned by LookupNormalized when a bare (unprefixed)
// model name matches profiles registered under multiple providers. Callers
// must disambiguate by passing the prefixed form (e.g.
// "anthropic:claude-sonnet-4-6" instead of "claude-sonnet-4-6").
var ErrAmbiguousModel = errors.New("ambiguous model name matches multiple onboarded profiles")

// minimumProbeSetPenalty is the score deduction applied to profiles that were
// onboarded with --probe-set minimum. Untested probes in the minimum set default
// to optimistic 1.0 scores; the penalty makes BestEligible prefer fully-probed
// profiles when overall scores are close. Tuned to 5 per PR #2 brief (P0-C).
const minimumProbeSetPenalty = 5

// AgentRole represents what role the model will fill.
type AgentRole int

const (
	RoleWorker AgentRole = iota
	RoleOrchestrator
)

func (r AgentRole) String() string {
	switch r {
	case RoleWorker:
		return "worker"
	case RoleOrchestrator:
		return "orchestrator"
	default:
		return "unknown"
	}
}

// RuntimeConfig holds per-model runtime parameters that the daemon passes to
// agent backends. Zero values mean "fall back to the daemon default", so
// profiles can override specific knobs without enumerating every field.
type RuntimeConfig struct {
	MaxTokens            int // 0 = fall back to daemon default
	NudgeIntervalSeconds int // 0 = fall back to daemon default
}

// ModelProfile holds the parsed capability data from a YAML profile.
type ModelProfile struct {
	ModelID  string
	Status   string // "known" or "restricted"
	Provider string

	// Capabilities (0.0–1.0 scores)
	ToolReliability      float64
	ShellReliability     float64
	ShellRecovery        float64
	FileWriteReliability float64
	TokenReporting       float64
	Streaming            float64
	MultiTurn            float64
	LargeOutput          float64

	// UnprobedCapabilities records which capability fields were emitted as
	// `null` in the YAML (probe skipped, score unknown). Consumers that need
	// to distinguish "unprobed" from "probed and scored 0" read this map.
	// Keys are the YAML field names (e.g. "shell_recovery", "multi_turn").
	// The corresponding float64 fields stay at their zero value — check here
	// before treating 0 as "failed."
	//
	// Rationale (Phase-5 audit): pre-2026-04-23 the probe script emitted
	// `1.0` for un-probed capabilities, which silently promoted fast-gate
	// models to parity with fully-probed ones. The probe now emits `null`
	// and this map tracks it so the router can exclude-from-consideration
	// rather than penalize or reward.
	UnprobedCapabilities map[string]bool

	// Context
	EffectiveContextClass string // "large", "medium", "small", "unknown"
	MaxInputTokens        int64  // 0 if unknown

	// Reasoning
	ReasoningControls string // "none", "not_tested", or csv of controls

	// Latency (ms, from probe measurements)
	LatencyAvgMs            int // average across probes; 0 = not measured
	LatencyBasicInferenceMs int
	LatencyToolCallMs       int

	// Routing
	AutonomyTier string // "full", "standard", "limited", "restricted"
	OverallScore int

	// Contract
	OnboardingPassed     bool
	WorkerEligible       bool
	OrchestratorEligible bool

	// Evidence
	ProbeSet     string // "default" or "minimum"
	ProbesRun    int    // number of probes executed (0 if unknown / legacy profile)
	ProbesPassed int    // number of probes that passed

	// Warnings
	Warnings []string

	// Runtime holds per-model overrides for daemon defaults such as
	// max_tokens and nudge_interval_seconds. Zero values fall back to the
	// daemon default. See model-routing/profiles/README.md for the YAML schema.
	Runtime RuntimeConfig
}

// IsEligible checks whether this profile is eligible for the given role.
func (p *ModelProfile) IsEligible(role AgentRole) bool {
	if p.Status == "restricted" {
		return false
	}
	switch role {
	case RoleWorker:
		return p.WorkerEligible
	case RoleOrchestrator:
		return p.OrchestratorEligible
	default:
		return false
	}
}

// HasReasoningControls returns true if the model supports reasoning effort tuning.
func (p *ModelProfile) HasReasoningControls() bool {
	return p.ReasoningControls != "" && p.ReasoningControls != "none" && p.ReasoningControls != "not_tested"
}

// HasLatencyData returns true if probe latency measurements are available.
func (p *ModelProfile) HasLatencyData() bool {
	return p.LatencyAvgMs > 0
}

// IsFullyProbed returns true if the model was onboarded with the full probe set.
func (p *ModelProfile) IsFullyProbed() bool {
	return p.ProbeSet == "" || p.ProbeSet == "default"
}

// ProfileStore loads and caches model profiles from disk.
type ProfileStore struct {
	mu              sync.RWMutex
	profiles        map[string]*ModelProfile // keyed by model_id
	dir             string                   // path to profiles directory
	logger          Logger                   // diagnostic logger; never nil (defaults to noopLogger)
	contextRegistry *ContextRegistry         // optional hand-curated max_input_tokens overrides
}

// SetContextRegistry wires in a context-registry. When set, each profile
// loaded from disk gets its missing MaxInputTokens / EffectiveContextClass
// filled from the registry (the probe script can't reliably discover these
// for openai/google_genai/ollama). Safe to call multiple times; takes effect
// on the next Reload.
//
// Pass nil to disable registry overrides.
func (ps *ProfileStore) SetContextRegistry(reg *ContextRegistry) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.contextRegistry = reg
}

// NewProfileStore creates a store and loads profiles from the given directory.
// Diagnostics emitted during load (malformed YAML, missing required fields,
// load-count summary) are discarded. Callers that want visibility into these
// events should use NewProfileStoreWithLogger.
func NewProfileStore(profileDir string) (*ProfileStore, error) {
	return NewProfileStoreWithLogger(profileDir, nil)
}

// NewProfileStoreWithLogger creates a store and loads profiles from the given
// directory, emitting diagnostic messages through logger. A nil logger is
// replaced with a silent no-op implementation so callers can opt in without
// changing behavior. See the Logger interface for the methods implementations
// must provide.
func NewProfileStoreWithLogger(profileDir string, logger Logger) (*ProfileStore, error) {
	if logger == nil {
		logger = noopLogger{}
	}
	ps := &ProfileStore{
		profiles: make(map[string]*ModelProfile),
		dir:      profileDir,
		logger:   logger,
	}
	if err := ps.load(); err != nil {
		return nil, err
	}
	return ps, nil
}

// Reload re-reads profiles from disk. Call after oat model onboard.
func (ps *ProfileStore) Reload() error {
	return ps.load()
}

// Get returns a profile by model ID, or nil if not found.
func (ps *ProfileStore) Get(modelID string) *ModelProfile {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.profiles[modelID]
}

// All returns all loaded profiles.
func (ps *ProfileStore) All() []*ModelProfile {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	result := make([]*ModelProfile, 0, len(ps.profiles))
	for _, p := range ps.profiles {
		result = append(result, p)
	}
	return result
}

// Eligible returns all profiles eligible for the given role.
func (ps *ProfileStore) Eligible(role AgentRole) []*ModelProfile {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	var result []*ModelProfile
	for _, p := range ps.profiles {
		if p.IsEligible(role) {
			result = append(result, p)
		}
	}
	return result
}

// EligibleFiltered returns profiles eligible for the given role, intersected with
// the allowed list. If allowed is empty/nil, returns all eligible profiles (no filtering).
func (ps *ProfileStore) EligibleFiltered(role AgentRole, allowed []string) []*ModelProfile {
	if len(allowed) == 0 {
		return ps.Eligible(role)
	}
	allowSet := make(map[string]bool, len(allowed))
	for _, m := range allowed {
		allowSet[m] = true
	}
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	var result []*ModelProfile
	for _, p := range ps.profiles {
		if p.IsEligible(role) && allowSet[p.ModelID] {
			result = append(result, p)
		}
	}
	return result
}

// LookupNormalized resolves a model identifier to an onboarded profile,
// accepting both prefixed ("provider:model") and unprefixed ("model") inputs.
//
// Resolution order:
//  1. Exact match on the input.
//  2. If the input lacks a colon: walk profiles whose bare suffix
//     (strings.SplitN(ModelID, ":", 2)[1]) equals the input. Match if
//     exactly one profile is found; return ErrAmbiguousModel if multiple
//     profiles across different providers share the same bare name.
//  3. If the input contains a colon but missed step 1: try the bare suffix
//     against profiles registered without prefix (rare).
//
// Returns (profile, canonicalModelID, nil) on success, where canonicalModelID
// is the profile's stored ModelID (always the prefixed form when normalization
// promoted an unprefixed input).
// Returns (nil, "", nil) on a clean miss (no profile matches).
// Returns (nil, "", ErrAmbiguousModel) when a bare input is ambiguous.
func (ps *ProfileStore) LookupNormalized(modelID string) (*ModelProfile, string, error) {
	ps.mu.RLock()
	defer ps.mu.RUnlock()

	if p, ok := ps.profiles[modelID]; ok {
		return p, modelID, nil
	}

	if !strings.Contains(modelID, ":") {
		var matches []*ModelProfile
		for _, p := range ps.profiles {
			parts := strings.SplitN(p.ModelID, ":", 2)
			if len(parts) == 2 && parts[1] == modelID {
				matches = append(matches, p)
			} else if len(parts) == 1 && parts[0] == modelID {
				matches = append(matches, p)
			}
		}
		if len(matches) == 0 {
			return nil, "", nil
		}
		if len(matches) > 1 {
			return nil, "", ErrAmbiguousModel
		}
		return matches[0], matches[0].ModelID, nil
	}

	bare := strings.SplitN(modelID, ":", 2)[1]
	if p, ok := ps.profiles[bare]; ok {
		return p, p.ModelID, nil
	}
	return nil, "", nil
}

// Validate checks that a model is onboarded and eligible for the given role.
// Accepts both prefixed and unprefixed model IDs via LookupNormalized.
// Returns nil if valid, or a descriptive error.
func (ps *ProfileStore) Validate(modelID string, role AgentRole) error {
	_, err := ps.ValidateAndCanonicalize(modelID, role)
	return err
}

// ValidateAndCanonicalize is Validate plus the canonical (always-prefixed)
// ModelID. Callers that need to update persisted state with the normalized
// form (e.g. agent state after a daemon restart with an unprefixed model)
// should prefer this variant. Returns the canonical ID on success and a
// descriptive error otherwise.
func (ps *ProfileStore) ValidateAndCanonicalize(modelID string, role AgentRole) (string, error) {
	p, canonical, err := ps.LookupNormalized(modelID)
	if errors.Is(err, ErrAmbiguousModel) {
		return "", fmt.Errorf("model %q matches multiple onboarded profiles across providers — use the prefixed form (e.g. provider:%s)", modelID, modelID)
	}
	if err != nil {
		return "", err
	}
	if p == nil {
		return "", fmt.Errorf("model %q is not onboarded — run: oat model onboard %s", modelID, modelID)
	}
	if !p.IsEligible(role) {
		return "", fmt.Errorf("model %q is not eligible as %s (status=%s, tier=%s, worker=%v, orchestrator=%v)",
			canonical, role, p.Status, p.AutonomyTier, p.WorkerEligible, p.OrchestratorEligible)
	}
	return canonical, nil
}

// BestEligible returns the highest-scoring eligible model for the given role.
// If preferredModel is set and eligible, it is returned instead.
// Tiebreaker: lower latency wins. If latency is equal or unknown, lexicographic model ID for stability.
// Returns ("", error) if no eligible models exist.
func (ps *ProfileStore) BestEligible(role AgentRole, preferredModel string) (string, error) {
	ps.mu.RLock()
	defer ps.mu.RUnlock()

	// If the user/repo has a preferred model and it's eligible, use it.
	if preferredModel != "" {
		if p, ok := ps.profiles[preferredModel]; ok && p.IsEligible(role) {
			return preferredModel, nil
		}
	}

	// effectiveScore penalizes minimum-set profiles so BestEligible prefers
	// fully-probed ones when overall scores are close. See minimumProbeSetPenalty
	// comment for rationale.
	effectiveScore := func(p *ModelProfile) int {
		s := p.OverallScore
		if p.ProbeSet == "minimum" {
			s -= minimumProbeSetPenalty
		}
		return s
	}

	var best *ModelProfile
	for _, p := range ps.profiles {
		if !p.IsEligible(role) {
			continue
		}
		if best == nil {
			best = p
			continue
		}
		pScore := effectiveScore(p)
		bScore := effectiveScore(best)
		if pScore > bScore {
			best = p
		} else if pScore == bScore {
			// Tiebreaker 1: prefer lower latency (0 = unknown, sort last)
			pLat := p.LatencyAvgMs
			bLat := best.LatencyAvgMs
			if pLat > 0 && (bLat == 0 || pLat < bLat) {
				best = p
			} else if pLat == bLat && p.ModelID < best.ModelID {
				// Tiebreaker 2: lexicographic for determinism
				best = p
			}
		}
	}
	if best == nil {
		return "", fmt.Errorf("no eligible models onboarded for role %s — run: oat model onboard <provider:model>", role)
	}
	return best.ModelID, nil
}

// Count returns the number of loaded profiles.
func (ps *ProfileStore) Count() int {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return len(ps.profiles)
}

// IsEmpty reports whether the store has zero loaded profiles. It is a
// convenience for daemon startup code that wants to warn operators when the
// profile directory is missing, empty, or entirely malformed.
func (ps *ProfileStore) IsEmpty() bool {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return len(ps.profiles) == 0
}

// load reads all YAML files from the profile directory.
//
// Files are skipped (with a WARN or ERROR logged against ps.logger) when they
// cannot be read, lack a model_id, or are missing required fields. A missing
// profile directory is not an error — it is treated as an empty directory so
// daemon startup remains non-fatal. Callers can detect the degenerate case via
// IsEmpty after construction.
func (ps *ProfileStore) load() error {
	logger := ps.logger
	if logger == nil {
		logger = noopLogger{}
	}

	entries, err := os.ReadDir(ps.dir)
	if err != nil {
		if os.IsNotExist(err) {
			// No profiles directory yet — not an error, just empty. Emit an
			// INFO with zero count so operators can still see the path the
			// daemon checked in logs.
			logger.Infof("loaded 0 model profile(s) from %s (directory does not exist)", ps.dir)
			return nil
		}
		return fmt.Errorf("reading profile directory %s: %w", ps.dir, err)
	}

	loaded := make(map[string]*ModelProfile)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(ps.dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			logger.Warnf("skipping profile %s: read error: %v", e.Name(), err)
			continue
		}
		p := parseProfile(string(data))
		if p.ModelID == "" {
			logger.Warnf("skipping profile %s: missing or malformed model_id", e.Name())
			continue
		}
		if missing := validateRequiredFields(p); len(missing) > 0 {
			logger.Errorf("skipping profile %s (%s): missing required fields: %s",
				e.Name(), p.ModelID, strings.Join(missing, ", "))
			continue
		}
		loaded[p.ModelID] = p
	}

	// Apply context-registry overrides BEFORE swapping the store map. Profiles
	// with missing/unknown windows get filled from the hand-curated registry.
	// Operators see per-profile INFO lines so drift is visible.
	ps.mu.RLock()
	reg := ps.contextRegistry
	ps.mu.RUnlock()
	if reg != nil && reg.Count() > 0 {
		applied := 0
		for _, p := range loaded {
			if reg.ApplyToProfile(p) {
				applied++
				logger.Infof("context-registry override applied to %s: max_input_tokens=%d, class=%s",
					p.ModelID, p.MaxInputTokens, p.EffectiveContextClass)
			}
		}
		if applied > 0 {
			logger.Infof("context-registry applied %d override(s) during profile load", applied)
		}
	}

	ps.mu.Lock()
	ps.profiles = loaded
	ps.mu.Unlock()

	logger.Infof("loaded %d model profile(s) from %s", len(loaded), ps.dir)
	return nil
}

// validateRequiredFields returns the names of required fields missing from p.
// The rule is literal: Provider and AutonomyTier must be non-empty, and the
// profile must carry either a non-zero OverallScore or OnboardingPassed=true.
// The latter joint check mirrors how onboarding treats restricted profiles
// (score=50, passed=false) as still loadable; fully unscored AND not-passed
// profiles are rejected as "operator likely forgot to fill the file out".
//
// A P2 follow-up may tighten this to require both independently; until then
// the function exists specifically to surface the "empty skeleton" case that
// today loads silently with zero fields populated.
func validateRequiredFields(p *ModelProfile) []string {
	var missing []string
	if p.Provider == "" {
		missing = append(missing, "provider.name")
	}
	if p.AutonomyTier == "" {
		missing = append(missing, "routing.autonomy_tier")
	}
	if p.OverallScore == 0 && !p.OnboardingPassed {
		missing = append(missing, "routing.overall_score or contract.onboarding_passed")
	}
	return missing
}

// parseProfile extracts a ModelProfile from flat YAML content.
// This is a simple line-based parser that handles the known profile schema
// without requiring a YAML library dependency.
func parseProfile(content string) *ModelProfile {
	fields := make(map[string]string)
	var warnings []string
	inWarnings := false

	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)

		// Skip comments and empty lines
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		// Detect warnings list
		if trimmed == "warnings:" {
			inWarnings = true
			continue
		}
		if inWarnings {
			if strings.HasPrefix(trimmed, "- ") {
				w := strings.TrimPrefix(trimmed, "- ")
				w = strings.Trim(w, "\"")
				warnings = append(warnings, w)
				continue
			}
			// End of warnings section
			inWarnings = false
		}

		// Parse key: value
		parts := strings.SplitN(trimmed, ":", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			val = strings.Trim(val, "\"")
			if val != "" {
				fields[key] = val
			}
		}
	}

	// Track capabilities emitted as `null` in the YAML — the probe-script
	// signal for "not probed." Stored so router code can distinguish
	// "unprobed" from "probed and failed."
	unprobed := map[string]bool{}
	for _, cap := range []string{
		"tool_reliability", "shell_roundtrip", "shell_recovery",
		"file_write_reliability", "token_reporting", "streaming",
		"multi_turn", "large_output",
	} {
		if v, ok := fields[cap]; ok && strings.EqualFold(v, "null") {
			unprobed[cap] = true
		}
	}

	return &ModelProfile{
		ModelID:                 fields["model_id"],
		Status:                  fields["status"],
		Provider:                fields["name"], // under provider: block, "name" is the key
		ToolReliability:         parseFloat(fields["tool_reliability"]),
		ShellReliability:        parseFloatFallback(fields, "shell_roundtrip", "shell_reliability"),
		ShellRecovery:           parseFloat(fields["shell_recovery"]),
		FileWriteReliability:    parseFloat(fields["file_write_reliability"]),
		TokenReporting:          parseFloat(fields["token_reporting"]),
		Streaming:               parseFloat(fields["streaming"]),
		MultiTurn:               parseFloat(fields["multi_turn"]),
		LargeOutput:             parseFloat(fields["large_output"]),
		UnprobedCapabilities:    unprobed,
		EffectiveContextClass:   fields["effective_context_class"],
		MaxInputTokens:          parseInt64(fields["max_input_tokens"]),
		ReasoningControls:       fields["reasoning_controls"],
		LatencyAvgMs:            parseInt(fields["avg_ms"]),
		LatencyBasicInferenceMs: parseInt(fields["basic_inference_ms"]),
		LatencyToolCallMs:       parseInt(fields["tool_calling_ms"]),
		AutonomyTier:            fields["autonomy_tier"],
		OverallScore:            parseInt(fields["overall_score"]),
		OnboardingPassed:        fields["onboarding_passed"] == "true",
		WorkerEligible:          fields["worker_eligible"] == "true",
		OrchestratorEligible:    fields["orchestrator_eligible"] == "true" || fields["supervisor_eligible"] == "true",
		ProbeSet:                fields["probe_set"],
		ProbesRun:               parseInt(fields["probes_run"]),
		ProbesPassed:            parseInt(fields["probes_passed"]),
		Warnings:                warnings,
		Runtime: RuntimeConfig{
			MaxTokens:            parseInt(fields["max_tokens"]),
			NudgeIntervalSeconds: parseInt(fields["nudge_interval_seconds"]),
		},
	}
}

// IsProbed reports whether the given capability field was scored during
// onboarding. Un-probed capabilities have a 0 value in their float64 field;
// callers that need to distinguish "unknown" from "failed" must consult
// this helper. Returns true for unknown capability names (conservative:
// if we don't know about it, don't flag it as unprobed).
func (p *ModelProfile) IsProbed(capName string) bool {
	if p == nil || p.UnprobedCapabilities == nil {
		return true
	}
	return !p.UnprobedCapabilities[capName]
}

func parseFloat(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// parseFloatFallback tries the primary key, falls back to the secondary.
// Handles the shell_reliability → shell_roundtrip rename: new profiles write
// shell_roundtrip, old profiles write shell_reliability.
func parseFloatFallback(fields map[string]string, primary, fallback string) float64 {
	if v, ok := fields[primary]; ok {
		return parseFloat(v)
	}
	return parseFloat(fields[fallback])
}

func parseInt(s string) int {
	v, _ := strconv.Atoi(s)
	return v
}

func parseInt64(s string) int64 {
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}
