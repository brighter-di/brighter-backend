package backtestengine

import (
	"errors"
	"math"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// Backtest is the part of the backtest engine's response this service depends on. Three
// facts about that response are load-bearing (all verified against the live
// API in the backtest-engine-prototype PoC):
//
//   - Every series is keyed by EPOCH DAY as a string ("20700" = 2026-09-04),
//     not by ISO date.
//   - daily_weights carries the model's WEIGHTS (ticker → day → fraction).
//     last_market_days_holdings looks similar but holds SHARE QUANTITIES;
//     normalising those yields weights that are wrong and plausible-looking.
//     It is deliberately not read.
//   - last_market_day is the last market day of the requested range and can
//     be LATER than the last day with weights (holiday, close not processed).
type Backtest struct {
	LastMarketDay int // epoch day; 0 when absent

	StrategyKey  string
	BenchmarkKey string
	capital      map[string]map[int]float64
	weights      map[string]map[int]float64
}

var uuidLike = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-`)

// Parse extracts the capital and weights series from a raw backtest response.
func Parse(raw map[string]any) (*Backtest, error) {
	return parse(raw, "")
}

func parse(raw map[string]any, benchmark string) (*Backtest, error) {
	bt := &Backtest{
		capital: daySeriesMap(first(raw, "dvm_capital", "dvm-capital")),
		weights: daySeriesMap(first(raw, "daily_weights", "daily-weights")),
	}
	if v, ok := raw["last_market_day"].(float64); ok {
		bt.LastMarketDay = int(v)
	}
	if len(bt.capital) == 0 {
		return nil, &Error{Status: 502, Method: "POST", Path: "/v1/backtest", Detail: "response has no dvm_capital series; top-level keys: " + keys(raw)}
	}
	// The strategy leg is keyed by the strategy tree uuid, the benchmark by ticker.
	names := make([]string, 0, len(bt.capital))
	for k := range bt.capital {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if uuidLike.MatchString(k) {
			if bt.StrategyKey != "" {
				return nil, &Error{Status: 502, Method: "POST", Path: "/v1/backtest", Detail: "response contains multiple strategy capital series"}
			}
			bt.StrategyKey = k
		}
	}
	if bt.StrategyKey == "" {
		return nil, &Error{Status: 502, Method: "POST", Path: "/v1/backtest", Detail: "response has no strategy capital series"}
	}
	if benchmark != "" {
		if len(bt.capital[benchmark]) == 0 {
			return nil, &Error{Status: 502, Method: "POST", Path: "/v1/backtest", Detail: "response has no capital series for the requested benchmark"}
		}
		bt.BenchmarkKey = benchmark
	} else {
		for _, k := range names {
			if k != bt.StrategyKey {
				if bt.BenchmarkKey != "" {
					return nil, &Error{Status: 502, Method: "POST", Path: "/v1/backtest", Detail: "response contains multiple benchmarks without a requested benchmark"}
				}
				bt.BenchmarkKey = k
			}
		}
	}
	return bt, nil
}

// Curve is the strategy and benchmark equity curves rebased to 100 at the
// first day both have a value, so they share ONE y-scale.
type Curve struct {
	Days      []int
	Strategy  []float64
	Benchmark []*float64 // nil where the benchmark has no print
}

var ErrShortSeries = errors.New("backtestEngine returned fewer than two capital observations")

func (b *Backtest) Curve() (*Curve, error) {
	strat := b.capital[b.StrategyKey]
	bench := b.capital[b.BenchmarkKey]
	days := sortedDays(strat)

	start := -1
	for i, d := range days {
		if strat[d] <= 0 {
			continue
		}
		if bench == nil || bench[d] > 0 {
			start = i
			break
		}
	}
	if start < 0 || len(days)-start < 2 {
		return nil, ErrShortSeries
	}
	days = days[start:]
	sBase := strat[days[0]]
	var bBase float64
	if bench != nil {
		bBase = bench[days[0]]
	}

	c := &Curve{Days: days, Strategy: make([]float64, len(days)), Benchmark: make([]*float64, len(days))}
	for i, d := range days {
		c.Strategy[i] = 100 * strat[d] / sBase
		if bench != nil {
			if v, ok := bench[d]; ok && v > 0 && bBase > 0 {
				x := 100 * v / bBase
				c.Benchmark[i] = &x
			}
		}
	}
	return c, nil
}

// Allocation is the model's allocation on one day.
type Allocation struct {
	Day       int
	Date      string
	Weights   map[string]float64
	MarketDay string // The backtest engine's last market day for the range; may be later than Date
	LagDays   int    // MarketDay - Date, in calendar days
}

// LatestAllocation reads the model's weights on the newest day present across
// ALL tickers. A ticker only appears on days it was held, so each ticker's
// own last day is just the last day it was in the portfolio; reading every
// ticker at one anchor day is the only way to get a real allocation. A ticker
// absent on that day was not held — a real 0, not a missing value.
func (b *Backtest) LatestAllocation() (*Allocation, bool) {
	newest := -1
	for _, series := range b.weights {
		for d := range series {
			if d > newest {
				newest = d
			}
		}
	}
	if newest < 0 {
		return nil, false
	}
	row := map[string]float64{}
	for ticker, series := range b.weights {
		if v, ok := series[newest]; ok && v != 0 {
			row[ticker] = v
		}
	}
	if len(row) == 0 {
		return nil, false
	}
	a := &Allocation{Day: newest, Date: EpochDayToISO(newest), Weights: row}
	if b.LastMarketDay > 0 {
		a.MarketDay = EpochDayToISO(b.LastMarketDay)
		a.LagDays = b.LastMarketDay - newest
	}
	return a, true
}

// EpochDayToISO converts a backtest engine epoch day to YYYY-MM-DD (20700 → 2026-09-04).
func EpochDayToISO(day int) string {
	return time.Unix(int64(day)*86400, 0).UTC().Format("2006-01-02")
}

func first(m map[string]any, names ...string) map[string]any {
	for _, n := range names {
		if v, ok := m[n].(map[string]any); ok {
			return v
		}
	}
	return nil
}

// daySeriesMap turns {"KEY": {"20700": 1.23, ...}} into KEY → day → value,
// ignoring anything that is not an epoch-day key with a numeric value.
func daySeriesMap(m map[string]any) map[string]map[int]float64 {
	out := map[string]map[int]float64{}
	for name, v := range m {
		series, ok := v.(map[string]any)
		if !ok {
			continue
		}
		s := map[int]float64{}
		for k, x := range series {
			d, err := strconv.Atoi(k)
			if err != nil {
				continue
			}
			if f, ok := x.(float64); ok && !math.IsNaN(f) && !math.IsInf(f, 0) {
				s[d] = f
			}
		}
		if len(s) > 0 {
			out[name] = s
		}
	}
	return out
}

func sortedDays(s map[int]float64) []int {
	out := make([]int, 0, len(s))
	for d := range s {
		out = append(out, d)
	}
	sort.Ints(out)
	return out
}

func keys(m map[string]any) string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	if len(ks) > 15 {
		ks = ks[:15]
	}
	out := ""
	for i, k := range ks {
		if i > 0 {
			out += ", "
		}
		out += k
	}
	return out
}
