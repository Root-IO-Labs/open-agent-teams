package routing

import "testing"

// TestBrowsingFitWarnings covers the Phase 10 item 2 model-suitability guard:
// it must flag a slow / low-shell_recovery model (the DGX-Spark Qwen class John
// hit) while staying quiet for a good fit, an un-probed metric, or a nil profile.
func TestBrowsingFitWarnings(t *testing.T) {
	t.Run("flags low shell_recovery and slow inference (Qwen-like)", func(t *testing.T) {
		p := &ModelProfile{
			ShellRecovery:           0.44,
			LatencyBasicInferenceMs: 40000,
		}
		w := p.BrowsingFitWarnings()
		if len(w) != 2 {
			t.Fatalf("expected 2 warnings (shell_recovery + slow inference), got %d: %v", len(w), w)
		}
	})

	t.Run("quiet for a good fit", func(t *testing.T) {
		p := &ModelProfile{
			ShellRecovery:           0.9,
			LatencyBasicInferenceMs: 3000,
		}
		if w := p.BrowsingFitWarnings(); len(w) != 0 {
			t.Errorf("expected no warnings for a good model, got: %v", w)
		}
	})

	t.Run("does not flag un-probed shell_recovery (0 is not a failure)", func(t *testing.T) {
		p := &ModelProfile{
			ShellRecovery:           0.0,
			UnprobedCapabilities:    map[string]bool{"shell_recovery": true},
			LatencyBasicInferenceMs: 3000,
		}
		if w := p.BrowsingFitWarnings(); len(w) != 0 {
			t.Errorf("un-probed shell_recovery must not be flagged, got: %v", w)
		}
	})

	t.Run("does not flag when latency unmeasured", func(t *testing.T) {
		p := &ModelProfile{
			ShellRecovery:           0.9,
			LatencyBasicInferenceMs: 0,
		}
		if w := p.BrowsingFitWarnings(); len(w) != 0 {
			t.Errorf("unmeasured latency must not be flagged, got: %v", w)
		}
	})

	t.Run("nil profile is safe", func(t *testing.T) {
		var p *ModelProfile
		if w := p.BrowsingFitWarnings(); w != nil {
			t.Errorf("nil profile should return nil, got: %v", w)
		}
	})

	t.Run("env overrides tune the thresholds", func(t *testing.T) {
		// Raise the shell-recovery floor so an otherwise-fine 0.6 trips, and
		// lower the latency ceiling so a 5s model trips.
		t.Setenv("OAT_BROWSER_MODEL_MIN_SHELL_RECOVERY", "0.8")
		t.Setenv("OAT_BROWSER_MODEL_MAX_INFERENCE_MS", "4000")
		p := &ModelProfile{
			ShellRecovery:           0.6,
			LatencyBasicInferenceMs: 5000,
		}
		if w := p.BrowsingFitWarnings(); len(w) != 2 {
			t.Errorf("expected both checks to trip under tightened env thresholds, got: %v", w)
		}
	})
}
