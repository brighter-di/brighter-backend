// Package metrics computes performance statistics from an equity curve. It
// serves both strategy backtests and plain market price history, so it
// depends on neither.
package metrics

import "math"

type Metrics struct {
	TotalReturn float64 `json:"totalReturn"`
	CAGR        float64 `json:"cagr"`
	MaxDrawdown float64 `json:"maxDrawdown"`
	Volatility  float64 `json:"volatility"`
	Sharpe      float64 `json:"sharpe"`
	Sortino     float64 `json:"sortino"`
}

// Compute derives metrics from a curve of values on epoch days. Metrics are
// computed from the curve itself rather than read from an upstream stats
// block, so every source is measured the same way. Non-positive and NaN points
// are skipped; fewer than two usable points is not a series.
func Compute(days []int, values []float64) (Metrics, bool) {
	var d []int
	var v []float64
	for i := range values {
		if values[i] > 0 && !math.IsNaN(values[i]) {
			d = append(d, days[i])
			v = append(v, values[i])
		}
	}
	if len(v) < 2 {
		return Metrics{}, false
	}
	m := Metrics{TotalReturn: v[len(v)-1]/v[0] - 1}
	if years := float64(d[len(d)-1]-d[0]) / 365.25; years > 0 {
		m.CAGR = math.Pow(v[len(v)-1]/v[0], 1/years) - 1
	}
	peak := v[0]
	for _, x := range v {
		peak = math.Max(peak, x)
		m.MaxDrawdown = math.Min(m.MaxDrawdown, x/peak-1)
	}

	rets := make([]float64, 0, len(v)-1)
	for i := 1; i < len(v); i++ {
		rets = append(rets, v[i]/v[i-1]-1)
	}
	mean, sd := meanStd(rets)
	var downSq float64
	for _, r := range rets {
		if r < 0 {
			downSq += r * r
		}
	}
	downDev := math.Sqrt(downSq / float64(len(rets)))
	const annual = 252
	m.Volatility = sd * math.Sqrt(annual)
	if sd > 0 {
		m.Sharpe = mean / sd * math.Sqrt(annual)
	}
	if downDev > 0 {
		m.Sortino = mean / downDev * math.Sqrt(annual)
	}
	return m, true
}

func meanStd(xs []float64) (float64, float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean := sum / float64(len(xs))
	if len(xs) < 2 {
		return mean, 0
	}
	var ss float64
	for _, x := range xs {
		ss += (x - mean) * (x - mean)
	}
	return mean, math.Sqrt(ss / float64(len(xs)-1))
}
