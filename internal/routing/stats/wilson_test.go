package stats

import (
	"math"
	"testing"
)

// Reference values produced by:
//
//	from statsmodels.stats.proportion import proportion_confint
//	proportion_confint(count, nobs, alpha=2*one_sided_alpha, method='wilson')[0]
//
// (The two-sided alpha argument to proportion_confint is 2× the one-sided
// alpha we pass to WilsonLowerBound when we want matching tail probabilities.)
func TestWilsonLowerBound_ReferenceValues(t *testing.T) {
	cases := []struct {
		name      string
		successes int
		n         int
		alpha     float64
		want      float64
		// tolerance is generous; we're comparing against scipy's float64
		// and our quantile uses Beasley-Springer-Moro fallback for non-
		// hardcoded alpha. The hardcoded alphas should match to ~1e-6.
		tol float64
	}{
		// 97.5% one-sided lower bound (z ≈ 1.96; matches scipy
		// proportion_confint(alpha=0.05, method='wilson') two-sided default).
		{"2/3, z=1.96", 2, 3, 0.025, 0.20765, 1e-3},
		{"3/3, z=1.96", 3, 3, 0.025, 0.43850, 1e-3},
		{"0/3, z=1.96", 0, 3, 0.025, 0.0, 1e-9},
		{"19/20, z=1.96", 19, 20, 0.025, 0.76387, 1e-3}, // hand-derived: pHat=0.95,n=20,z=1.96
		{"15/20, z=1.96", 15, 20, 0.025, 0.53127, 1e-3},
		{"50/100, z=1.96", 50, 100, 0.025, 0.40383, 1e-3},
		{"95/100, z=1.96", 95, 100, 0.025, 0.88825, 1e-3}, // hand-derived: pHat=0.95,n=100,z=1.96
		// Edge cases
		{"n=0", 0, 0, 0.025, 0.0, 1e-9},
		{"n=1, success", 1, 1, 0.025, 0.20654, 1e-3},
		{"n=1, failure", 0, 1, 0.025, 0.0, 1e-9},
		// Cell big enough to converge near raw fraction
		{"800/1000, z=1.96", 800, 1000, 0.025, 0.77370, 1e-3},
		// 95% one-sided (z ≈ 1.6449) — sanity check that the hardcoded z
		// values in zScoreOneSided produce an internally-consistent answer.
		// Reference computed by hand with pHat=0.667, n=3, z=1.6449.
		{"2/3, z=1.6449", 2, 3, 0.05, 0.25353, 1e-3},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := WilsonLowerBound(tc.successes, tc.n, tc.alpha)
			if math.Abs(got-tc.want) > tc.tol {
				t.Errorf("WilsonLowerBound(%d, %d, %v) = %v; want %v ± %v",
					tc.successes, tc.n, tc.alpha, got, tc.want, tc.tol)
			}
		})
	}
}

func TestWilsonLowerBound_BoundedZeroToOne(t *testing.T) {
	// Sweep a grid; the bound must always be in [0, 1].
	for n := 1; n <= 50; n++ {
		for k := 0; k <= n; k++ {
			lower := WilsonLowerBound(k, n, 0.05)
			if lower < 0 || lower > 1 {
				t.Errorf("k=%d n=%d -> %v out of [0,1]", k, n, lower)
			}
		}
	}
}

func TestWilsonLowerBound_MonotonicInSuccesses(t *testing.T) {
	// More successes at fixed n should never decrease the bound.
	const n = 30
	prev := -1.0
	for k := 0; k <= n; k++ {
		got := WilsonLowerBound(k, n, 0.05)
		if got < prev-1e-12 {
			t.Errorf("not monotonic at k=%d (n=%d): %v < %v", k, n, got, prev)
		}
		prev = got
	}
}

func TestWilsonLowerBound_LooserAlphaTighterBound(t *testing.T) {
	// Smaller alpha = wider confidence interval = LOWER lower bound.
	tighter := WilsonLowerBound(80, 100, 0.10) // 90% one-sided
	wider := WilsonLowerBound(80, 100, 0.01)   // 99% one-sided
	if !(wider < tighter) {
		t.Errorf("wider CI should give lower lower bound: 99%%=%v vs 90%%=%v", wider, tighter)
	}
}

func TestWilsonLowerBound_DefensiveInputs(t *testing.T) {
	// Negative successes treated as 0; successes > n treated as n.
	// Should not panic.
	if got := WilsonLowerBound(-5, 10, 0.05); got != 0 {
		t.Errorf("negative successes: want 0, got %v", got)
	}
	if got := WilsonLowerBound(20, 10, 0.05); got <= 0.5 {
		t.Errorf("successes > n should clamp to n (10/10 high lower bound): got %v", got)
	}
}
