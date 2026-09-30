package strategy

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/brighter-di/backend/internal/tokens"
)

// starterCustom is Studio's "Momentum with a safety net".
func starterCustom() CustomStrategy {
	return CustomStrategy{
		Name: "Momentum with a safety net", Rebalance: "monthly", Benchmark: "SPY",
		Children: []CustomNode{{
			Kind: "condition", Ticker: "SPY", Metric: "price", Lookback: 200, Comparator: "gt",
			CompareMode: "indicator", RightMetric: "moving-average", RightTicker: "SPY", RightLookback: 200,
			ThenChildren: []CustomNode{{Kind: "weight", Method: "equal", Lookback: 63, Children: []CustomNode{{
				Kind: "filter", Direction: "top", Count: 3, Metric: "cumulative-return", Lookback: 90,
				Children: []CustomNode{{Kind: "asset", Ticker: "NVDAon"}, {Kind: "asset", Ticker: "MSFT"}, {Kind: "asset", Ticker: "AAPL"}},
			}}}},
			ElseChildren: []CustomNode{{Kind: "weight", Method: "equal", Children: []CustomNode{{Kind: "asset", Ticker: "BIL"}}}},
		}},
	}
}

func treeJSON(t *testing.T, c *CompiledCustom) string {
	t.Helper()
	b, err := json.Marshal(c.Tree)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCompileCustomBuildsBacktestEngineTree(t *testing.T) {
	c, err := CompileCustom(starterCustom())
	if err != nil {
		t.Fatal(err)
	}
	s := treeJSON(t, c)
	for _, want := range []string{
		`"step":"root"`, `"rebalance":"monthly"`, `"step":"if"`, `"is-else-condition?":true`,
		`"lhs-fn":"current-price"`, `"lhs-val":"SPY"`, `"comparator":"gt"`, `"rhs-fixed-value?":false`,
		`"rhs-fn":"moving-average-price"`, `"rhs-window-days":"200"`,
		`"step":"filter"`, `"select-fn":"top"`, `"select-n":"3"`, `"sort-by-fn":"cumulative-return"`, `"sort-by-window-days":"90"`,
		`"ticker":"NVDA"`, `"ticker":"BIL"`, `"step":"wt-cash-equal"`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("tree missing %s: %s", want, s)
		}
	}
	if strings.Contains(s, "NVDAon") {
		t.Fatalf("Ondo symbols must resolve to research tickers: %s", s)
	}
	if strings.Contains(s, `"lhs-window-days"`) {
		t.Fatalf("price takes no window: %s", s)
	}
	if c.Benchmark != "SPY" || len(c.Key) != 24 {
		t.Fatalf("benchmark %q key %q", c.Benchmark, c.Key)
	}
}

func TestCompileCustomKeyFollowsRulesOnly(t *testing.T) {
	base, _ := CompileCustom(starterCustom())

	renamed := starterCustom()
	renamed.Name = "Another name"
	renamed.Children[0].ThenChildren[0].Lookback = 999 // unused by an equal weight
	renamed.Children[0].ThenChildren[0].Children[0].Children[0].Ticker = "NVDA"
	if c, err := CompileCustom(renamed); err != nil || c.Key != base.Key {
		t.Fatalf("same rules must share a key: %v %s vs %s", err, c.Key, base.Key)
	}

	changed := starterCustom()
	changed.Children[0].ThenChildren[0].Children[0].Count = 2
	if c, err := CompileCustom(changed); err != nil || c.Key == base.Key {
		t.Fatalf("different rules must not share a key: %v", err)
	}
}

func TestCompileCustomSelectEqualWeightRanksCandidates(t *testing.T) {
	var keys []string
	for _, count := range []int{1, 3} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			selectNode := CustomNode{Kind: "filter", Count: count, Direction: "top", Metric: "cumulative-return", Lookback: 126,
				Children: []CustomNode{{Kind: "weight", Method: "equal", Children: []CustomNode{
					{Kind: "asset", Ticker: "QCOM"}, {Kind: "asset", Ticker: "SPY"}, {Kind: "asset", Ticker: "BIL"},
				}}}}
			input := CustomStrategy{Name: "Dual momentum core", Rebalance: "monthly", Benchmark: "SPY", Children: []CustomNode{selectNode}}
			compiled, err := CompileCustom(input)
			if err != nil {
				t.Fatal(err)
			}
			weight := compiled.Tree["children"].([]node)[0]
			if weight["step"] != "wt-cash-equal" {
				t.Fatalf("expected allocation after selection: %s", treeJSON(t, compiled))
			}
			filter := weight["children"].([]node)[0]
			candidates := filter["children"].([]node)
			if filter["step"] != "filter" || filter["select-n"] != strconv.Itoa(count) || len(candidates) != 3 {
				t.Fatalf("expected count %d ranking three candidates: %s", count, treeJSON(t, compiled))
			}
			for i, ticker := range []string{"QCOM", "SPY", "BIL"} {
				if candidates[i]["step"] != "asset" || candidates[i]["ticker"] != ticker {
					t.Fatalf("expected direct asset candidate %s: %s", ticker, treeJSON(t, compiled))
				}
			}
			keys = append(keys, compiled.Key)
			// The alternate DSL ordering has exactly the same meaning and key.
			selectNode.Children = selectNode.Children[0].Children
			input.Children = []CustomNode{{Kind: "weight", Method: "equal", Children: []CustomNode{selectNode}}}
			equivalent, err := CompileCustom(input)
			if err != nil || equivalent.Key != compiled.Key {
				t.Fatalf("equivalent ordering must share the new key: %v", err)
			}
		})
	}
	if keys[0] == keys[1] {
		t.Fatal("selection counts must not share a cache key")
	}
}

func TestCompileCustomSelectPreservesGroupCandidate(t *testing.T) {
	input := CustomStrategy{Rebalance: "monthly", Benchmark: "SPY", Children: []CustomNode{{
		Kind: "filter", Count: 1, Direction: "top", Metric: "cumulative-return", Lookback: 126,
		Children: []CustomNode{{Kind: "group", Name: "Basket", Children: []CustomNode{{
			Kind: "weight", Method: "equal", Children: []CustomNode{{Kind: "asset", Ticker: "QCOM"}, {Kind: "asset", Ticker: "SPY"}},
		}}}},
	}}}
	compiled, err := CompileCustom(input)
	if err != nil {
		t.Fatal(err)
	}
	weight := compiled.Tree["children"].([]node)[0]
	filter := weight["children"].([]node)[0]
	candidates := filter["children"].([]node)
	if len(candidates) != 1 || candidates[0]["step"] != "group" {
		t.Fatalf("an explicit group remains a basket candidate: %s", treeJSON(t, compiled))
	}
}

func TestCompileCustomFixedValue(t *testing.T) {
	c, err := CompileCustom(CustomStrategy{
		Rebalance: "daily", Benchmark: "SPY",
		Children: []CustomNode{{
			Kind: "condition", Ticker: "SPY", Metric: "rsi", Lookback: 14, Comparator: "lt", CompareMode: "value", Value: 70.5,
			ThenChildren: []CustomNode{{Kind: "asset", Ticker: "QQQ"}}, ElseChildren: []CustomNode{{Kind: "asset", Ticker: "BIL"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := treeJSON(t, c)
	for _, want := range []string{`"lhs-fn":"relative-strength-index"`, `"lhs-window-days":"14"`, `"comparator":"lt"`, `"rhs-fixed-value?":true`, `"rhs-val":"70.5"`, `"name":"Studio strategy"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("tree missing %s: %s", want, s)
		}
	}
}

func TestCompileCustomWrapsTopLevelAssets(t *testing.T) {
	c, err := CompileCustom(CustomStrategy{Rebalance: "weekly", Benchmark: "SPYon", Children: []CustomNode{{Kind: "asset", Ticker: "AAPL"}, {Kind: "asset", Ticker: "MSFTon"}}})
	if err != nil {
		t.Fatal(err)
	}
	children := c.Tree["children"].([]node)
	if len(children) != 1 || children[0]["step"] != "wt-cash-equal" || c.Benchmark != "SPY" {
		t.Fatalf("top-level blocks must share capital equally: %s", treeJSON(t, c))
	}
}

func TestCompileCustomFullOndoUniverse(t *testing.T) {
	children := make([]CustomNode, 0, len(tokens.All()))
	for _, a := range tokens.All() {
		if a.Kind == "Currency" {
			continue
		}
		symbol, ticker := a.OndoSymbol, a.Ticker
		for _, spelling := range []string{symbol, ticker, strings.ToLower(symbol)} {
			if got, ok := ResearchTicker(spelling); !ok || got != ticker {
				t.Fatalf("%s resolved to %q (%v), want %s", spelling, got, ok, ticker)
			}
		}
		children = append(children, CustomNode{Kind: "asset", Ticker: symbol})
	}
	if len(children) != 450 {
		t.Fatalf("want 450 Ondo securities, got %d", len(children))
	}
	c, err := CompileCustom(CustomStrategy{Rebalance: "weekly", Benchmark: "SPYon", Children: []CustomNode{{
		Kind: "weight", Method: "equal", Children: []CustomNode{{
			Kind: "filter", Direction: "top", Count: 5, Metric: "cumulative-return", Lookback: 90, Children: children,
		}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(treeJSON(t, c), `"step":"asset"`); got != 450 {
		t.Fatalf("want all 450 assets in the BacktestEngine tree, got %d", got)
	}
	for _, symbol := range []string{"FAKEon", "USDon", "USDY"} {
		if _, ok := ResearchTicker(symbol); ok {
			t.Fatalf("%s must not resolve to a stock price", symbol)
		}
	}
}

func TestCompileCustomTotalBlockLimit(t *testing.T) {
	assets := make([]CustomNode, customMaxList)
	for i := range assets {
		assets[i] = CustomNode{Kind: "asset", Ticker: "AAPL"}
	}
	_, err := CompileCustom(CustomStrategy{Rebalance: "monthly", Benchmark: "SPY", Children: []CustomNode{
		{Kind: "weight", Method: "equal", Children: assets},
		{Kind: "weight", Method: "equal", Children: assets},
	}})
	if err == nil || !strings.Contains(err.Error(), "2000 blocks") {
		t.Fatalf("want total block limit, got %v", err)
	}
}

// Everything Studio gained to match the backtest engine's own blocks, in one strategy:
// named groups, set percentages, the wider comparators, the indicators beyond
// return and RSI, and a rebalance on drift.
func TestCompileCustomRicherBlocks(t *testing.T) {
	c, err := CompileCustom(CustomStrategy{
		Name: "Everything", Rebalance: "threshold", Corridor: 10, Benchmark: "SPY",
		Children: []CustomNode{{
			Kind: "group", Name: "Core",
			Children: []CustomNode{{
				Kind: "condition", Ticker: "SPY", Metric: "exponential-moving-average", Lookback: 50, Comparator: "gte",
				CompareMode: "indicator", RightMetric: "max-drawdown", RightTicker: "QQQ", RightLookback: 60,
				ThenChildren: []CustomNode{{Kind: "weight", Method: "specified", Children: []CustomNode{
					{Kind: "asset", Ticker: "AAPL", Percent: 60},
					{Kind: "asset", Ticker: "MSFT", Percent: 40},
				}}},
				ElseChildren: []CustomNode{{Kind: "filter", Direction: "bottom", Count: 1, Metric: "max-drawdown", Lookback: 500,
					Children: []CustomNode{{Kind: "asset", Ticker: "BIL"}, {Kind: "asset", Ticker: "TLT"}}}},
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := treeJSON(t, c)
	for _, want := range []string{
		`"step":"group"`, `"name":"Core"`,
		`"step":"wt-cash-specified"`, `"weight":{"den":100,"num":"60"}`, `"weight":{"den":100,"num":"40"}`,
		`"comparator":"gte"`, `"lhs-fn":"exponential-moving-average-price"`, `"rhs-fn":"max-drawdown"`,
		`"sort-by-fn":"max-drawdown"`, `"sort-by-window-days":"500"`, `"select-fn":"bottom"`,
		// Drift rebalancing is no calendar plus a corridor.
		`"rebalance":"none"`, `"rebalance-corridor-width":0.1`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("tree missing %s: %s", want, s)
		}
	}

	// The corridor is part of the rules, so it changes the key.
	wider := CustomStrategy{
		Name: "Everything", Rebalance: "threshold", Corridor: 20, Benchmark: "SPY",
		Children: []CustomNode{{Kind: "asset", Ticker: "AAPL"}},
	}
	narrow := wider
	narrow.Corridor = 5
	a, err1 := CompileCustom(wider)
	b, err2 := CompileCustom(narrow)
	if err1 != nil || err2 != nil || a.Key == b.Key {
		t.Fatalf("a different corridor is a different strategy: %v %v", err1, err2)
	}
}

func TestCompileCustomOscillator(t *testing.T) {
	c, err := CompileCustom(CustomStrategy{
		Rebalance: "yearly", Benchmark: "SPY",
		Children: []CustomNode{{
			Kind: "condition", Ticker: "SPY", Metric: "ppo-signal", ShortWindow: 12, LongWindow: 26, SmoothWindow: 9,
			Comparator: "lte", CompareMode: "value", Value: 0,
			ThenChildren: []CustomNode{{Kind: "asset", Ticker: "BIL"}},
			ElseChildren: []CustomNode{{Kind: "asset", Ticker: "QQQ"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := treeJSON(t, c)
	for _, want := range []string{
		`"rebalance":"yearly"`, `"lhs-fn":"percentage-price-oscillator-signal"`,
		`"lhs-fn-params":{"long-window":26,"short-window":12,"smooth-window":9}`, `"comparator":"lte"`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("tree missing %s: %s", want, s)
		}
	}
	if strings.Contains(s, `"lhs-window-days"`) {
		t.Fatalf("an oscillator carries its own windows: %s", s)
	}
}

func TestCompileCustomRejects(t *testing.T) {
	nest := func(levels int) []CustomNode {
		n := []CustomNode{{Kind: "asset", Ticker: "AAPL"}}
		for range levels {
			n = []CustomNode{{Kind: "weight", Method: "equal", Children: n}}
		}
		return n
	}
	many := make([]CustomNode, customMaxList+1)
	for i := range many {
		many[i] = CustomNode{Kind: "asset", Ticker: "AAPL"}
	}
	edit := func(f func(*CustomStrategy)) CustomStrategy {
		s := starterCustom()
		f(&s)
		return s
	}
	shares := func(a, b int) CustomStrategy {
		return CustomStrategy{Rebalance: "daily", Benchmark: "SPY", Children: []CustomNode{{
			Kind: "weight", Method: "specified", Children: []CustomNode{
				{Kind: "asset", Ticker: "AAPL", Percent: a}, {Kind: "asset", Ticker: "MSFT", Percent: b},
			},
		}}}
	}
	cases := map[string]CustomStrategy{
		"unknown ticker":    edit(func(s *CustomStrategy) { s.Children[0].ElseChildren[0].Children[0].Ticker = "FAKE" }),
		"unknown benchmark": edit(func(s *CustomStrategy) { s.Benchmark = "NOPE" }),
		"bad rebalance":     edit(func(s *CustomStrategy) { s.Rebalance = "hourly" }),
		"no blocks":         edit(func(s *CustomStrategy) { s.Children = nil }),
		"empty branch":      edit(func(s *CustomStrategy) { s.Children[0].ElseChildren = nil }),
		"empty weight":      edit(func(s *CustomStrategy) { s.Children[0].ElseChildren[0].Children = nil }),
		"short lookback":    edit(func(s *CustomStrategy) { s.Children[0].RightLookback = 1 }),
		"long lookback":     edit(func(s *CustomStrategy) { s.Children[0].ThenChildren[0].Children[0].Lookback = customMaxDays + 1 }),
		"bad measure":       edit(func(s *CustomStrategy) { s.Children[0].Metric = "volume" }),
		"bad comparator":    edit(func(s *CustomStrategy) { s.Children[0].Comparator = "between" }),
		"zero count":        edit(func(s *CustomStrategy) { s.Children[0].ThenChildren[0].Children[0].Count = 0 }),
		"non-finite value": edit(func(s *CustomStrategy) {
			s.Children[0].CompareMode, s.Children[0].Value = "value", math.Inf(1)
		}),
		"unknown kind":      edit(func(s *CustomStrategy) { s.Children[0].Kind = "<script>" }),
		"too deep":          {Rebalance: "monthly", Benchmark: "SPY", Children: nest(customMaxDepth)},
		"too many in list":  {Rebalance: "monthly", Benchmark: "SPY", Children: many},
		"shares below 100":  shares(50, 40),
		"shares above 100":  shares(70, 40),
		"share of nothing":  shares(100, 0),
		"corridor too wide": {Rebalance: "threshold", Corridor: 90, Benchmark: "SPY", Children: []CustomNode{{Kind: "asset", Ticker: "AAPL"}}},
		"corridor missing":  {Rebalance: "threshold", Benchmark: "SPY", Children: []CustomNode{{Kind: "asset", Ticker: "AAPL"}}},
		"ppo without windows": {Rebalance: "daily", Benchmark: "SPY", Children: []CustomNode{{
			Kind: "condition", Ticker: "SPY", Metric: "ppo", Comparator: "gt", CompareMode: "value", Value: 0,
			ThenChildren: []CustomNode{{Kind: "asset", Ticker: "QQQ"}}, ElseChildren: []CustomNode{{Kind: "asset", Ticker: "BIL"}},
		}}},
		"ppo windows crossed": {Rebalance: "daily", Benchmark: "SPY", Children: []CustomNode{{
			Kind: "condition", Ticker: "SPY", Metric: "ppo", ShortWindow: 30, LongWindow: 10,
			Comparator: "gt", CompareMode: "value", Value: 0,
			ThenChildren: []CustomNode{{Kind: "asset", Ticker: "QQQ"}}, ElseChildren: []CustomNode{{Kind: "asset", Ticker: "BIL"}},
		}}},
		"select by oscillator": {Rebalance: "daily", Benchmark: "SPY", Children: []CustomNode{{
			Kind: "filter", Direction: "top", Count: 1, Metric: "ppo", Lookback: 30,
			Children: []CustomNode{{Kind: "asset", Ticker: "AAPL"}, {Kind: "asset", Ticker: "MSFT"}},
		}}},
	}
	for name, s := range cases {
		_, err := CompileCustom(s)
		var ce *CustomError
		if !errors.As(err, &ce) || ce.Reason == "" {
			t.Errorf("%s: want a CustomError, got %v", name, err)
		}
	}
}
