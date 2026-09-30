package strategy

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand"
	"os"
	"slices"
	"testing"
)

// This reference interpreter follows the Studio source rules, independently
// of the compiler's lowering. Signals are supplied, not historical prices:
// these tests verify allocation, indicator wiring and branch semantics, not
// The backtest engine's numerical implementation of market indicators.
type sourceOracle struct {
	t         *testing.T
	functions map[string]string
	input     scoreInputs
}

func (o sourceOracle) indicator(n CustomNode, right bool) float64 {
	metric, ticker, window, short, long, smooth := n.Metric, n.Ticker, n.Lookback, n.ShortWindow, n.LongWindow, n.SmoothWindow
	if right {
		metric, ticker, window, short, long, smooth = n.RightMetric, n.RightTicker, n.RightLookback, n.RightShort, n.RightLong, n.RightSmooth
	}
	params := map[string]int{}
	switch metric {
	case "price":
		window = 0
	case "ppo", "ppo-signal":
		window = 0
		params["short-window"], params["long-window"] = short, long
		if metric == "ppo-signal" {
			params["smooth-window"] = smooth
		}
	}
	ticker, _ = ResearchTicker(ticker)
	return scoreSignal(o.t, o.input, o.functions[metric], ticker, window, params)
}

func compareValues(comparator string, left, right float64) bool {
	switch comparator {
	case "gt":
		return left > right
	case "gte":
		return left >= right
	case "eq":
		return left == right
	case "lte":
		return left <= right
	case "lt":
		return left < right
	default:
		panic("invalid reference comparator")
	}
}

func (o sourceOracle) matches(n CustomNode) bool {
	if n.Type == "any" {
		for _, child := range n.Conditions {
			if o.matches(child) {
				return true
			}
		}
		return false
	}
	if n.Type == "all" {
		for _, child := range n.Conditions {
			if !o.matches(child) {
				return false
			}
		}
		return true
	}
	right := n.Value
	if n.CompareMode == "indicator" {
		right = o.indicator(n, true)
	}
	return compareValues(n.Comparator, o.indicator(n, false), right)
}

func (o sourceOracle) allocation(n CustomNode) map[string]float64 {
	switch n.Kind {
	case "asset":
		ticker, _ := ResearchTicker(n.Ticker)
		return map[string]float64{ticker: 1}
	case "group":
		return o.weighted(n.Children, "equal", 0)
	case "condition":
		predicate := n
		if n.Predicate != nil {
			predicate = *n.Predicate
		}
		if o.matches(predicate) {
			return o.weighted(n.ThenChildren, "equal", 0)
		}
		return o.weighted(n.ElseChildren, "equal", 0)
	case "weight":
		if n.Method == "inverse-volatility" && len(n.Children) == 1 && n.Children[0].Kind == "filter" {
			return o.selected(n.Children[0], n.Children[0].Children, n.Method, n.Lookback)
		}
		return o.weighted(n.Children, n.Method, n.Lookback)
	case "filter":
		if len(n.Children) == 1 && n.Children[0].Kind == "weight" {
			weight := n.Children[0]
			return o.selected(n, weight.Children, weight.Method, weight.Lookback)
		}
		return o.selected(n, n.Children, "equal", 0)
	default:
		o.t.Fatalf("unsupported source block: %s", n.Kind)
		return nil
	}
}

func (o sourceOracle) selected(selection CustomNode, candidates []CustomNode, method string, window int) map[string]float64 {
	pool := slices.Clone(candidates)
	rank := func(n CustomNode) float64 {
		value := 0.0
		for ticker, share := range o.allocation(n) {
			value += share * scoreSignal(o.t, o.input, o.functions[selection.Metric], ticker, selection.Lookback, nil)
		}
		return value
	}
	slices.SortStableFunc(pool, func(a, b CustomNode) int {
		left, right := rank(a), rank(b)
		if left == right {
			return 0
		}
		if (left > right) == (selection.Direction == "top") {
			return -1
		}
		return 1
	})
	return o.weighted(pool[:min(selection.Count, len(pool))], method, window)
}

func (o sourceOracle) weighted(children []CustomNode, method string, window int) map[string]float64 {
	allocations := make([]map[string]float64, len(children))
	shares := make([]float64, len(children))
	total := 0.0
	for i, child := range children {
		allocations[i] = o.allocation(child)
		shares[i] = 1
		if method == "specified" {
			shares[i] = float64(child.Percent)
		}
		if method == "inverse-volatility" {
			variance := 0.0
			for ticker, share := range allocations[i] {
				vol := scoreSignal(o.t, o.input, "standard-deviation-return", ticker, window, nil)
				variance += share * share * vol * vol
			}
			shares[i] = 1 / math.Sqrt(variance)
		}
		total += shares[i]
	}
	out := map[string]float64{}
	for i, allocation := range allocations {
		for ticker, share := range allocation {
			out[ticker] += share * shares[i] / total
		}
	}
	return out
}

func oracleFor(t *testing.T, c CustomStrategy, seed int) sourceOracle {
	t.Helper()
	data, err := os.ReadFile("testdata/studio-language.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Metrics []struct{ Name, BacktestEngine string }
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	o := sourceOracle{t: t, functions: map[string]string{}, input: scoreInputs{signals: map[string]float64{}}}
	for _, metric := range spec.Metrics {
		o.functions[metric.Name] = metric.BacktestEngine
	}
	tickers := map[string]bool{}
	var walk func([]CustomNode, func(CustomNode))
	walk = func(nodes []CustomNode, visit func(CustomNode)) {
		for _, n := range nodes {
			visit(n)
			walk(n.Children, visit)
			walk(n.ThenChildren, visit)
			walk(n.ElseChildren, visit)
		}
	}
	walk(c.Children, func(n CustomNode) {
		if n.Kind == "asset" {
			ticker, _ := ResearchTicker(n.Ticker)
			tickers[ticker] = true
		}
	})
	put := func(fn, ticker string, window int, params map[string]int) {
		key := signalKey(fn, ticker, window, params)
		hash := fnv.New32a()
		fmt.Fprintf(hash, "%d:%s", seed, key)
		value := float64(int(hash.Sum32()%201) - 100)
		if fn == "standard-deviation-return" {
			value = .01 + math.Abs(value)/100
		}
		o.input.signals[key] = value
	}
	var comparison func(CustomNode)
	comparison = func(n CustomNode) {
		if n.Type == "any" || n.Type == "all" {
			for _, child := range n.Conditions {
				comparison(child)
			}
			return
		}
		for _, right := range []bool{false, true} {
			if right && n.CompareMode != "indicator" {
				continue
			}
			metric, ticker, window, short, long, smooth := n.Metric, n.Ticker, n.Lookback, n.ShortWindow, n.LongWindow, n.SmoothWindow
			if right {
				metric, ticker, window, short, long, smooth = n.RightMetric, n.RightTicker, n.RightLookback, n.RightShort, n.RightLong, n.RightSmooth
			}
			params := map[string]int{}
			if metric == "price" {
				window = 0
			}
			if metric == "ppo" || metric == "ppo-signal" {
				window = 0
				params["short-window"], params["long-window"] = short, long
				if metric == "ppo-signal" {
					params["smooth-window"] = smooth
				}
			}
			ticker, _ = ResearchTicker(ticker)
			put(o.functions[metric], ticker, window, params)
		}
	}
	walk(c.Children, func(n CustomNode) {
		switch n.Kind {
		case "condition":
			if n.Predicate != nil {
				comparison(*n.Predicate)
			} else {
				comparison(n)
			}
		case "filter":
			for ticker := range tickers {
				put(o.functions[n.Metric], ticker, n.Lookback, nil)
			}
		case "weight":
			if n.Method == "inverse-volatility" {
				for ticker := range tickers {
					put("standard-deviation-return", ticker, n.Lookback, nil)
				}
			}
		}
	})
	return o
}

func assertAllocation(t *testing.T, got, want map[string]float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("selected assets: got %v, want %v", got, want)
	}
	total := 0.0
	for ticker, share := range want {
		if math.IsNaN(got[ticker]) || math.Abs(got[ticker]-share) > 1e-10 {
			t.Fatalf("%s: got %v, want %v", ticker, got, want)
		}
		total += got[ticker]
	}
	if math.Abs(total-1) > 1e-10 {
		t.Fatalf("allocation sums to %g", total)
	}
}

func TestStudioFixturesPreserveSourceSemantics(t *testing.T) {
	data, err := os.ReadFile("testdata/studio-contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Name     string
		Valid    bool
		Strategy CustomStrategy
	}
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		if !fixture.Valid {
			continue
		}
		t.Run(fixture.Name, func(t *testing.T) {
			compiled, err := CompileCustom(fixture.Strategy)
			if err != nil {
				t.Fatal(err)
			}
			for seed := 0; seed < 16; seed++ {
				o := oracleFor(t, fixture.Strategy, seed)
				want := o.weighted(fixture.Strategy.Children, "equal", 0)
				assertAllocation(t, scoreAllocation(t, compiled.Tree, o.input), want)
			}
		})
	}
}

func TestEveryComparatorPreservesBoundaries(t *testing.T) {
	truth := map[string][3]bool{
		"gt": {false, false, true}, "gte": {false, true, true}, "eq": {false, true, false},
		"lte": {true, true, false}, "lt": {true, false, false},
	}
	for comparator, outcomes := range truth {
		for _, mode := range []string{"value", "indicator"} {
			for _, boundary := range []float64{-10.5, 0, 10.5} {
				rule := CustomNode{Kind: "condition", Metric: "rsi", Ticker: "SPY", Lookback: 14, Comparator: comparator, CompareMode: mode, Value: boundary,
					RightMetric: "moving-average", RightTicker: "QQQ", RightLookback: 200,
					ThenChildren: []CustomNode{{Kind: "asset", Ticker: "NVDA"}}, ElseChildren: []CustomNode{{Kind: "asset", Ticker: "BIL"}}}
				input := CustomStrategy{Rebalance: "monthly", Benchmark: "SPY", Children: []CustomNode{rule}}
				compiled, err := CompileCustom(input)
				if err != nil {
					t.Fatal(err)
				}
				for i, delta := range []float64{-1, 0, 1} {
					o := oracleFor(t, input, 0)
					o.input.signals[signalKey("relative-strength-index", "SPY", 14, nil)] = boundary + delta
					o.input.signals[signalKey("moving-average-price", "QQQ", 200, nil)] = boundary
					want := "BIL"
					if outcomes[i] {
						want = "NVDA"
					}
					assertAllocation(t, scoreAllocation(t, compiled.Tree, o.input), map[string]float64{want: 1})
				}
			}
		}
	}
}

func TestNestedRuleCombinationsPreserveSemantics(t *testing.T) {
	rng := rand.New(rand.NewSource(20260913))
	tickers := []string{"QCOM", "SPY", "QQQ", "BIL"}
	metrics := []string{"price", "moving-average", "exponential-moving-average", "cumulative-return", "moving-average-return", "rsi", "max-drawdown", "standard-deviation-return", "standard-deviation-price", "ppo", "ppo-signal"}
	comparison := func() CustomNode {
		return CustomNode{Type: "comparison", Metric: metrics[rng.Intn(len(metrics))], Ticker: tickers[rng.Intn(len(tickers))], Lookback: 14 + rng.Intn(200), ShortWindow: 12, LongWindow: 26, SmoothWindow: 9,
			Comparator: []string{"gt", "gte", "eq", "lte", "lt"}[rng.Intn(5)], CompareMode: []string{"value", "indicator"}[rng.Intn(2)], Value: float64(rng.Intn(31) - 15),
			RightMetric: metrics[rng.Intn(len(metrics))], RightTicker: tickers[rng.Intn(len(tickers))], RightLookback: 14 + rng.Intn(200), RightShort: 10, RightLong: 30, RightSmooth: 7}
	}
	var block func(int) CustomNode
	block = func(depth int) CustomNode {
		if depth == 0 {
			return CustomNode{Kind: "asset", Ticker: tickers[rng.Intn(len(tickers))]}
		}
		switch rng.Intn(5) {
		case 0:
			return block(0)
		case 1:
			return CustomNode{Kind: "group", Name: "Sleeve", Children: []CustomNode{block(depth - 1), block(depth - 1)}}
		case 2:
			children := []CustomNode{block(depth - 1), block(depth - 1)}
			children[0].Percent, children[1].Percent = 60, 40
			return CustomNode{Kind: "weight", Method: []string{"equal", "specified", "inverse-volatility"}[rng.Intn(3)], Lookback: 63, Children: children}
		case 3:
			predicate := CustomNode{Type: []string{"any", "all"}[rng.Intn(2)], Conditions: []CustomNode{comparison(), comparison()}}
			return CustomNode{Kind: "condition", Predicate: &predicate, ThenChildren: []CustomNode{block(depth - 1), block(depth - 1)}, ElseChildren: []CustomNode{block(depth - 1)}}
		default:
			children := []CustomNode{block(depth - 1), block(depth - 1)}
			if rng.Intn(2) == 0 {
				children = []CustomNode{{Kind: "weight", Method: []string{"equal", "specified", "inverse-volatility"}[rng.Intn(3)], Lookback: 63, Children: []CustomNode{
					{Kind: "asset", Ticker: "QCOM", Percent: 40}, {Kind: "asset", Ticker: "SPY", Percent: 35}, {Kind: "asset", Ticker: "BIL", Percent: 25},
				}}}
			}
			return CustomNode{Kind: "filter", Metric: "cumulative-return", Lookback: 126, Direction: []string{"top", "bottom"}[rng.Intn(2)], Count: 1 + rng.Intn(2), Children: children}
		}
	}
	for i := 0; i < 256; i++ {
		input := CustomStrategy{Name: "Generated rules", Rebalance: "monthly", Benchmark: "SPY", Children: []CustomNode{block(3), block(2)}}
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			compiled, err := CompileCustom(input)
			if err != nil {
				t.Fatal(err)
			}
			for seed := 0; seed < 8; seed++ {
				o := oracleFor(t, input, seed)
				assertAllocation(t, scoreAllocation(t, compiled.Tree, o.input), o.weighted(input.Children, "equal", 0))
			}
		})
	}
}
