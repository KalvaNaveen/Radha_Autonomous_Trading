// Package indicators implements the daily-bar maths used by the swing
// strategy: EMA, Wilder ATR, simple moving average, rolling highs, and
// helpers for price rounding. All functions are pure and allocation-light.
package indicators

import "math"

// EMA returns the exponential moving average series with alpha = 2/(N+1),
// seeded with the SMA of the first N values. Entries before index N-1 are 0.
func EMA(v []float64, n int) []float64 {
	out := make([]float64, len(v))
	if n < 1 || len(v) < n {
		return out
	}
	alpha := 2.0 / float64(n+1)
	var sum float64
	for i := 0; i < n; i++ {
		sum += v[i]
	}
	out[n-1] = sum / float64(n)
	for i := n; i < len(v); i++ {
		out[i] = alpha*v[i] + (1-alpha)*out[i-1]
	}
	return out
}

// SMA returns the simple moving average; entries before index N-1 are 0.
func SMA(v []float64, n int) []float64 {
	out := make([]float64, len(v))
	if n < 1 {
		return out
	}
	var sum float64
	for i := range v {
		sum += v[i]
		if i >= n {
			sum -= v[i-n]
		}
		if i >= n-1 {
			out[i] = sum / float64(n)
		}
	}
	return out
}

// ATR returns Wilder's average true range; entries before index N are 0.
// TR_i = max(H−L, |H−C_{i−1}|, |L−C_{i−1}|); ATR seeded with the mean of the
// first N true ranges, then ATR_i = (ATR_{i−1}·(N−1) + TR_i) / N.
func ATR(high, low, close []float64, n int) []float64 {
	out := make([]float64, len(close))
	if n < 1 || len(close) <= n {
		return out
	}
	tr := make([]float64, len(close))
	for i := 1; i < len(close); i++ {
		tr[i] = math.Max(high[i]-low[i], math.Max(math.Abs(high[i]-close[i-1]), math.Abs(low[i]-close[i-1])))
	}
	var sum float64
	for i := 1; i <= n; i++ {
		sum += tr[i]
	}
	out[n] = sum / float64(n)
	for i := n + 1; i < len(close); i++ {
		out[i] = (out[i-1]*float64(n-1) + tr[i]) / float64(n)
	}
	return out
}

// PriorHighest returns, for each i, the highest value in v[i-n .. i-1]
// (excluding today). Entries with fewer than n prior values are 0.
func PriorHighest(v []float64, n int) []float64 {
	out := make([]float64, len(v))
	for i := n; i < len(v); i++ {
		m := v[i-n]
		for j := i - n + 1; j < i; j++ {
			if v[j] > m {
				m = v[j]
			}
		}
		out[i] = m
	}
	return out
}

// RoundToTick rounds p to the instrument tick. dir > 0 rounds up, dir < 0 down, 0 nearest.
func RoundToTick(p, tick float64, dir int) float64 {
	if tick <= 0 {
		tick = 0.05
	}
	q := p / tick
	switch {
	case dir > 0:
		q = math.Ceil(q - 1e-9)
	case dir < 0:
		q = math.Floor(q + 1e-9)
	default:
		q = math.Round(q)
	}
	return math.Round(q*tick*100) / 100
}

// Pct converts a percentage (2.5) to a fraction (0.025).
func Pct(p float64) float64 { return p / 100 }
