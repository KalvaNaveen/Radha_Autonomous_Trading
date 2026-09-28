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

// HeikinAshi returns smoothed candles computed from real OHLC:
//
//	HAClose = (O+H+L+C)/4
//	HAOpen  = (prev HAOpen + prev HAClose)/2   (first bar: (O+C)/2)
//	HAHigh  = max(H, HAOpen, HAClose),  HALow = min(L, HAOpen, HAClose)
//
// The prices are synthetic — use them for signals only, never for orders.
func HeikinAshi(o, h, l, c []float64) (hO, hH, hL, hC []float64) {
	n := len(c)
	hO, hH, hL, hC = make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n)
	for i := 0; i < n; i++ {
		hC[i] = (o[i] + h[i] + l[i] + c[i]) / 4
		if i == 0 {
			hO[i] = (o[i] + c[i]) / 2
		} else {
			hO[i] = (hO[i-1] + hC[i-1]) / 2
		}
		hH[i] = math.Max(h[i], math.Max(hO[i], hC[i]))
		hL[i] = math.Min(l[i], math.Min(hO[i], hC[i]))
	}
	return
}

// Supertrend returns the trend direction (+1 green, −1 red, 0 = not ready)
// and the Supertrend line, using Wilder ATR(period) and a band multiplier —
// the same construction as TradingView's built-in Supertrend.
func Supertrend(h, l, c []float64, period int, mult float64) ([]int8, []float64) {
	n := len(c)
	dir, line := make([]int8, n), make([]float64, n)
	atr := ATR(h, l, c, period)
	var up, dn float64
	for i := 0; i < n; i++ {
		if atr[i] <= 0 {
			continue
		}
		mid := (h[i] + l[i]) / 2
		bu, bd := mid-mult*atr[i], mid+mult*atr[i]
		if i == 0 || dir[i-1] == 0 {
			up, dn = bu, bd
			if c[i] >= mid {
				dir[i] = 1
			} else {
				dir[i] = -1
			}
		} else {
			pu, pd := up, dn
			if c[i-1] > pu {
				up = math.Max(bu, pu)
			} else {
				up = bu
			}
			if c[i-1] < pd {
				dn = math.Min(bd, pd)
			} else {
				dn = bd
			}
			switch {
			case dir[i-1] == -1 && c[i] > dn:
				dir[i] = 1
			case dir[i-1] == 1 && c[i] < up:
				dir[i] = -1
			default:
				dir[i] = dir[i-1]
			}
		}
		if dir[i] == 1 {
			line[i] = up
		} else {
			line[i] = dn
		}
	}
	return dir, line
}
