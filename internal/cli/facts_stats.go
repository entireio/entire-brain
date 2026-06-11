package cli

import (
	"math"
	"sort"
)

// Statistical helpers for honest A/B comparison of eval runs. The eval A/B
// (e.g. recall with vs without query expansion) runs the SAME tasks through two
// configs, so the data is paired — a paired t-test is the correct, more
// powerful test, and the per-metric family is corrected with Holm-Bonferroni.
// The two-sided Student-t p-value is computed from the regularized incomplete
// beta function rather than a normal approximation (meaningless at small n).

// betacf is the continued-fraction expansion for the incomplete beta function
// (Numerical Recipes), used by incompleteBeta.
func betacf(a, b, x float64) float64 {
	const maxIterations = 200
	const eps = 3e-12
	const fpMin = 1e-300
	qab, qap, qam := a+b, a+1, a-1
	c := 1.0
	d := 1 - qab*x/qap
	if math.Abs(d) < fpMin {
		d = fpMin
	}
	d = 1 / d
	h := d
	for m := 1; m <= maxIterations; m++ {
		mf := float64(m)
		m2 := 2 * mf
		aa := mf * (b - mf) * x / ((qam + m2) * (a + m2))
		d = 1 + aa*d
		if math.Abs(d) < fpMin {
			d = fpMin
		}
		c = 1 + aa/c
		if math.Abs(c) < fpMin {
			c = fpMin
		}
		d = 1 / d
		h *= d * c
		aa = -(a + mf) * (qab + mf) * x / ((a + m2) * (qap + m2))
		d = 1 + aa*d
		if math.Abs(d) < fpMin {
			d = fpMin
		}
		c = 1 + aa/c
		if math.Abs(c) < fpMin {
			c = fpMin
		}
		d = 1 / d
		del := d * c
		h *= del
		if math.Abs(del-1) < eps {
			break
		}
	}
	return h
}

// incompleteBeta returns the regularized incomplete beta function I_x(a, b).
func incompleteBeta(a, b, x float64) float64 {
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return 1
	}
	lga, _ := math.Lgamma(a)
	lgb, _ := math.Lgamma(b)
	lgab, _ := math.Lgamma(a + b)
	bt := math.Exp(lgab - lga - lgb + a*math.Log(x) + b*math.Log(1-x))
	if x < (a+1)/(a+b+2) {
		return bt * betacf(a, b, x) / a
	}
	return 1 - bt*betacf(b, a, 1-x)/b
}

// studentTTwoSidedP returns the two-sided p-value for a Student-t statistic t
// with df degrees of freedom.
func studentTTwoSidedP(t, df float64) float64 {
	if df <= 0 {
		return 1
	}
	x := df / (df + t*t)
	return incompleteBeta(df/2, 0.5, x)
}

// pairedStats is the result of a paired comparison of two metric series.
type pairedStats struct {
	N      int
	MeanA  float64
	MeanB  float64
	Delta  float64 // mean(B - A)
	T      float64
	P      float64
	CohenD float64 // paired effect size: mean(diff) / sd(diff)
}

// pairedTTest computes paired statistics for equal-length series a, b. With
// fewer than two pairs, or zero-variance differences, it degrades gracefully:
// a consistent non-zero shift yields p=0 (perfectly reproducible), an identical
// pair yields p=1.
func pairedTTest(a, b []float64) pairedStats {
	n := len(a)
	s := pairedStats{N: n, P: 1}
	if n == 0 || len(b) != n {
		return s
	}
	var sumA, sumB, sumD float64
	diffs := make([]float64, n)
	for i := range a {
		sumA += a[i]
		sumB += b[i]
		diffs[i] = b[i] - a[i]
		sumD += diffs[i]
	}
	s.MeanA = sumA / float64(n)
	s.MeanB = sumB / float64(n)
	s.Delta = sumD / float64(n)
	if n < 2 {
		return s
	}
	var varD float64
	for _, d := range diffs {
		varD += (d - s.Delta) * (d - s.Delta)
	}
	varD /= float64(n - 1)
	sd := math.Sqrt(varD)
	if sd == 0 {
		if s.Delta == 0 {
			return s // identical: no effect, p=1
		}
		s.P = 0 // a perfectly consistent shift
		s.CohenD = math.Copysign(math.Inf(1), s.Delta)
		s.T = s.CohenD
		return s
	}
	s.T = s.Delta / (sd / math.Sqrt(float64(n)))
	s.P = studentTTwoSidedP(math.Abs(s.T), float64(n-1))
	s.CohenD = s.Delta / sd
	return s
}

// holmReject applies the Holm-Bonferroni step-down correction at family-wise
// level alpha, returning per-input rejection flags (true = significant after
// correction).
func holmReject(pvals []float64, alpha float64) []bool {
	reject, _ := holmRejectWithThresholds(pvals, alpha)
	return reject
}

func holmRejectWithThresholds(pvals []float64, alpha float64) ([]bool, []float64) {
	n := len(pvals)
	reject := make([]bool, n)
	thresholds := make([]float64, n)
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool { return pvals[order[i]] < pvals[order[j]] })
	for rank, idx := range order {
		thresholds[idx] = alpha / float64(n-rank)
	}
	for _, idx := range order {
		if pvals[idx] <= thresholds[idx] {
			reject[idx] = true
		} else {
			break // step-down: once one fails, all larger fail
		}
	}
	return reject, thresholds
}
