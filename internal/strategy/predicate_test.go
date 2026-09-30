package strategy

import (
	"encoding/json"
	"strings"
	"testing"
)

func priceRule(ticker string) CustomNode {
	return CustomNode{Type: "comparison", Ticker: ticker, Metric: "price", Comparator: "gt", CompareMode: "value", Value: 0}
}

// Evaluate the emitted the backtest engine score, rather than the source expression, to
// catch reversed branches or faulty lowering of mixed ANY/ALL expressions.
func selectedAsset(t *testing.T, score node, prices map[string]float64) string {
	t.Helper()
	children, _ := score["children"].([]node)
	switch score["step"] {
	case "asset":
		return score["ticker"].(string)
	case "if":
		rule := children[0]
		if rule["lhs-fn"] != fnPrice || rule["comparator"] != "gt" || rule["rhs-val"] != "0" {
			t.Fatalf("Unexpected test condition: %+v", rule)
		}
		branch := children[1]
		if prices[rule["lhs-val"].(string)] > 0 {
			branch = rule
		}
		return selectedAsset(t, branch, prices)
	default:
		if len(children) != 1 {
			t.Fatalf("Expected one outcome: %+v", score)
		}
		return selectedAsset(t, children[0], prices)
	}
}

func TestCombinedConditionTruthTables(t *testing.T) {
	// (AAPL OR MSFT) AND (QQQ OR SPY), across all 16 signal combinations.
	expression := CustomNode{Type: "all", Conditions: []CustomNode{
		{Type: "any", Conditions: []CustomNode{priceRule("AAPL"), priceRule("MSFT")}},
		{Type: "any", Conditions: []CustomNode{priceRule("QQQ"), priceRule("SPY")}},
	}}
	input := CustomStrategy{Rebalance: "threshold", Corridor: 10, Benchmark: "SPY", Children: []CustomNode{{
		Kind: "condition", Predicate: &expression,
		ThenChildren: []CustomNode{{Kind: "asset", Ticker: "NVDA"}}, ElseChildren: []CustomNode{{Kind: "asset", Ticker: "BIL"}},
	}}}
	compiled, err := CompileCustom(input)
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Tree["rebalance"] != "none" || compiled.Tree["rebalance-corridor-width"] != 0.1 {
		t.Fatal("Lost threshold settings")
	}
	for mask := 0; mask < 16; mask++ {
		prices := map[string]float64{}
		for i, ticker := range []string{"AAPL", "MSFT", "QQQ", "SPY"} {
			if mask&(1<<i) != 0 {
				prices[ticker] = 1
			}
		}
		want := "BIL"
		if mask&3 != 0 && mask&12 != 0 {
			want = "NVDA"
		}
		if got := selectedAsset(t, compiled.Tree, prices); got != want {
			t.Fatalf("Signals %04b: got %s, want %s", mask, got, want)
		}
	}
	// The opposite nesting must preserve OR semantics too.
	expression.Type = "any"
	expression.Conditions[0].Type = "all"
	expression.Conditions[1].Type = "all"
	compiled, err = CompileCustom(input)
	if err != nil {
		t.Fatal(err)
	}
	for mask := 0; mask < 16; mask++ {
		prices := map[string]float64{}
		for i, ticker := range []string{"AAPL", "MSFT", "QQQ", "SPY"} {
			if mask&(1<<i) != 0 {
				prices[ticker] = 1
			}
		}
		want := "BIL"
		if mask&3 == 3 || mask&12 == 12 {
			want = "NVDA"
		}
		if got := selectedAsset(t, compiled.Tree, prices); got != want {
			t.Fatalf("Signals %04b: got %s, want %s", mask, got, want)
		}
	}
}

func TestCombinedConditionValidationAndIdentity(t *testing.T) {
	compile := func(expression CustomNode) (*CompiledCustom, error) {
		return CompileCustom(CustomStrategy{Rebalance: "monthly", Benchmark: "SPY", Children: []CustomNode{{Kind: "condition", Predicate: &expression,
			ThenChildren: []CustomNode{{Kind: "asset", Ticker: "NVDA"}}, ElseChildren: []CustomNode{{Kind: "asset", Ticker: "BIL"}},
		}}})
	}
	a, err := compile(CustomNode{Type: "any", Conditions: []CustomNode{priceRule("AAPL")}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := compile(CustomNode{Type: "any", Conditions: []CustomNode{priceRule("AAPLon")}})
	if err != nil || a.Key != b.Key {
		t.Fatal("Token aliases must retain the same cache key", err)
	}
	c, err := compile(CustomNode{Type: "all", Conditions: []CustomNode{priceRule("AAPL"), priceRule("MSFT")}})
	if err != nil || a.Key == c.Key {
		t.Fatal("Changed condition rules must change the cache key", err)
	}
	badTicker := priceRule("FAKE")
	excessive := make([]CustomNode, 33)
	for i := range excessive {
		excessive[i] = priceRule("AAPL")
	}
	for _, expression := range []CustomNode{{Type: "all"}, {Type: "unknown", Conditions: []CustomNode{priceRule("AAPL")}}, badTicker, {Type: "all", Conditions: excessive}} {
		if _, err := compile(expression); err == nil {
			t.Fatalf("Accepted invalid expression: %+v", expression)
		}
	}
	// Keep expansion bounded even when many branches repeat large outcomes.
	outcomes := make([]CustomNode, 450)
	for i := range outcomes {
		outcomes[i] = CustomNode{Kind: "asset", Ticker: "SPY"}
	}
	rules := make([]CustomNode, 20)
	for i := range rules {
		rules[i] = priceRule("AAPL")
	}
	expression := CustomNode{Type: "all", Conditions: rules}
	_, err = CompileCustom(CustomStrategy{Rebalance: "daily", Benchmark: "SPY", Children: []CustomNode{{Kind: "condition", Predicate: &expression,
		ThenChildren: []CustomNode{{Kind: "asset", Ticker: "NVDA"}}, ElseChildren: outcomes,
	}}})
	if err == nil || !strings.Contains(err.Error(), "too many backtest blocks") {
		t.Fatalf("Unbounded expression expansion: %v", err)
	}
}

func TestStudioSerializedPredicate(t *testing.T) {
	// Match the browser's JSON document, including nested predicates and both
	// oscillator sides. The unused legacy comparison fields must not interfere.
	payload := `{"name":"Studio","rebalance":"threshold","corridor":5,"benchmark":"SPY","children":[{"kind":"condition","ticker":"FAKE","predicate":{"type":"all","conditions":[{"type":"comparison","ticker":"SPY","metric":"ppo","shortWindow":12,"longWindow":26,"comparator":"gte","compareMode":"indicator","rightTicker":"SPY","rightMetric":"ppo-signal","rightShortWindow":12,"rightLongWindow":26,"rightSmoothWindow":9}]},"thenChildren":[{"kind":"asset","ticker":"NVDA"}],"elseChildren":[{"kind":"asset","ticker":"BIL"}]}]}`
	var input CustomStrategy
	if err := json.Unmarshal([]byte(payload), &input); err != nil {
		t.Fatal(err)
	}
	compiled, err := CompileCustom(input)
	if err != nil {
		t.Fatal(err)
	}
	score := treeJSON(t, compiled)
	for _, expected := range []string{`"rebalance-corridor-width":0.05`, `"lhs-fn":"percentage-price-oscillator"`, `"rhs-fn":"percentage-price-oscillator-signal"`, `"comparator":"gte"`, `"smooth-window":9`} {
		if !strings.Contains(score, expected) {
			t.Fatalf("Missing %s: %s", expected, score)
		}
	}
}
