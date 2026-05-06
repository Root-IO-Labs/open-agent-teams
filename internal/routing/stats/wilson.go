// Package stats provides small numerical helpers used by routing analysis.
// Kept separate from the main routing package so it can be unit-tested in
// isolation and consumed without importing the full routing dependency tree.
package stats

import "math"

// WilsonLowerBound returns the lower bound of the Wilson score interval for a
// proportion of `successes` in `n` trials, at confidence level 1-alpha.
// alpha=0.05 gives a 95% one-sided lower bound (z ≈ 1.6449); alpha=0.025
// gives a 97.5% one-sided / 95% two-sided lower bound (z ≈ 1.96).
//
// Why Wilson instead of raw fraction:
//
// The naive estimator successes/n is wildly optimistic at small n. With 2/3
// successes the raw rate is 0.667; the Wilson 95% lower bound is ≈0.208.
// The lookup-aware router (Phase 3) uses this lower bound as the historical-
// success-rate term so it refuses to deviate from the static prior on three
// coin flips.
//
// Edge cases:
//   - n <= 0 returns 0
//   - successes < 0 is treated as 0
//   - successes > n is treated as n
//   - Wilson bound is clamped to [0, 1]
//
// Reference values match scipy.stats.proportion_confint(method='wilson') for
// the corresponding (count, nobs, alpha) inputs (see wilson_test.go).
func WilsonLowerBound(successes, n int, alpha float64) float64 {
	if n <= 0 {
		return 0
	}
	if successes < 0 {
		successes = 0
	}
	if successes > n {
		successes = n
	}

	// One-sided z-score for the lower tail.
	z := zScoreOneSided(alpha)
	if z <= 0 {
		// Pathological alpha — fall back to the raw rate so callers don't
		// blow up on misuse.
		return float64(successes) / float64(n)
	}

	pHat := float64(successes) / float64(n)
	nf := float64(n)
	z2 := z * z

	denominator := 1 + z2/nf
	center := pHat + z2/(2*nf)
	margin := z * math.Sqrt(pHat*(1-pHat)/nf+z2/(4*nf*nf))

	lower := (center - margin) / denominator
	if lower < 0 {
		lower = 0
	}
	if lower > 1 {
		lower = 1
	}
	return lower
}

// zScoreOneSided returns the z value such that the upper tail of a standard
// normal has probability alpha. We hardcode the few values we actually use
// (95%, 97.5%, 99% confidence) plus a continuous approximation fallback.
//
// The fallback uses a rational approximation accurate to ~1e-4 in the tails
// we care about — good enough for routing decisions where signal noise
// dwarfs numerical error.
func zScoreOneSided(alpha float64) float64 {
	switch {
	case math.Abs(alpha-0.05) < 1e-9:
		return 1.6448536269514722 // 95% one-sided
	case math.Abs(alpha-0.025) < 1e-9:
		return 1.959963984540054 // 97.5% one-sided / 95% two-sided
	case math.Abs(alpha-0.01) < 1e-9:
		return 2.3263478740408408 // 99% one-sided
	case math.Abs(alpha-0.005) < 1e-9:
		return 2.5758293035489004 // 99.5% one-sided / 99% two-sided
	}
	// Fallback: invert the upper tail of the standard normal.
	// 1 - alpha is the desired CDF. probit via Beasley-Springer-Moro.
	return inverseNormalCDF(1 - alpha)
}

// inverseNormalCDF approximates the standard normal quantile function.
// Beasley-Springer-Moro algorithm; sufficient accuracy for confidence-bound
// uses. Returns 0 for inputs at or below 0.5 since we only call it for
// upper-tail z values (alpha < 0.5).
func inverseNormalCDF(p float64) float64 {
	if p <= 0.5 {
		return 0
	}
	if p >= 1 {
		return 8 // saturate; far beyond any alpha we care about
	}
	// Coefficients from Beasley-Springer-Moro.
	a := []float64{-3.969683028665376e+01, 2.209460984245205e+02, -2.759285104469687e+02, 1.383577518672690e+02, -3.066479806614716e+01, 2.506628277459239e+00}
	b := []float64{-5.447609879822406e+01, 1.615858368580409e+02, -1.556989798598866e+02, 6.680131188771972e+01, -1.328068155288572e+01}
	c := []float64{-7.784894002430293e-03, -3.223964580411365e-01, -2.400758277161838e+00, -2.549732539343734e+00, 4.374664141464968e+00, 2.938163982698783e+00}
	d := []float64{7.784695709041462e-03, 3.224671290700398e-01, 2.445134137142996e+00, 3.754408661907416e+00}

	plow := 0.02425
	phigh := 1 - plow

	var q, r float64
	switch {
	case p < plow:
		q = math.Sqrt(-2 * math.Log(p))
		return (((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) /
			((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	case p <= phigh:
		q = p - 0.5
		r = q * q
		return (((((a[0]*r+a[1])*r+a[2])*r+a[3])*r+a[4])*r + a[5]) * q /
			(((((b[0]*r+b[1])*r+b[2])*r+b[3])*r+b[4])*r + 1)
	default:
		q = math.Sqrt(-2 * math.Log(1-p))
		return -(((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) /
			((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	}
}
