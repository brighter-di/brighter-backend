package strategy

import (
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

type scoreInputs struct {
	rank    map[string]float64
	vol     map[string]float64
	signals map[string]float64
}

func signalKey(fn, ticker string, window int, params map[string]int) string {
	return fmt.Sprintf("%s:%s:%d:%d:%d:%d", fn, ticker, window, params["short-window"], params["long-window"], params["smooth-window"])
}

func scoreSignal(t *testing.T, input scoreInputs, fn, ticker string, window int, params map[string]int) float64 {
	t.Helper()
	if input.signals == nil {
		return input.rank[ticker]
	}
	key := signalKey(fn, ticker, window, params)
	value, ok := input.signals[key]
	if !ok {
		t.Fatalf("compiled score reads an unexpected indicator: %s", key)
	}
	return value
}

// Evaluate the compiled the backtest engine branches and native allocations on fixed
// signals. Expectations below are independently specified ticker weights;
// a filter that still selects a whole weighted basket fails these checks.
func scoreAllocation(t *testing.T, n node, input scoreInputs) map[string]float64 {
	t.Helper()
	if n["step"] == "asset" {
		return map[string]float64{n["ticker"].(string): 1}
	}
	children := n["children"].([]node)
	if n["step"] == "root" || n["step"] == "group" || n["step"] == "if-child" {
		out := map[string]float64{}
		for _, child := range children {
			for ticker, weight := range scoreAllocation(t, child, input) {
				out[ticker] += weight / float64(len(children))
			}
		}
		return out
	}
	if n["step"] == "if" {
		condition := children[0]
		read := func(side string) float64 {
			window, _ := strconv.Atoi(fmt.Sprint(condition[side+"-window-days"]))
			params, _ := condition[side+"-fn-params"].(map[string]int)
			return scoreSignal(t, input, condition[side+"-fn"].(string), condition[side+"-val"].(string), window, params)
		}
		lhs := read("lhs")
		var rhs float64
		if condition["rhs-fixed-value?"] == true {
			var err error
			rhs, err = strconv.ParseFloat(condition["rhs-val"].(string), 64)
			if err != nil {
				t.Fatal(err)
			}
		} else {
			rhs = read("rhs")
		}
		var matched bool
		switch condition["comparator"] {
		case "gt":
			matched = lhs > rhs
		case "gte":
			matched = lhs >= rhs
		case "lt":
			matched = lhs < rhs
		case "lte":
			matched = lhs <= rhs
		case "eq":
			matched = lhs == rhs
		default:
			t.Fatalf("unsupported comparison %v", condition["comparator"])
		}
		if matched {
			return scoreAllocation(t, children[0], input)
		}
		return scoreAllocation(t, children[1], input)
	}
	if n["step"] != "wt-cash-equal" && n["step"] != "wt-inverse-vol" && n["step"] != "wt-cash-specified" && n["step"] != "filter" {
		t.Fatalf("unsupported score step: %v", n["step"])
	}
	allocations := make([]map[string]float64, len(children))
	scores := make([]float64, len(children))
	for i, child := range children {
		allocations[i] = scoreAllocation(t, child, input)
		scores[i] = 1
		if n["step"] == "wt-cash-specified" {
			fraction := child["weight"].(node)
			num, _ := strconv.ParseFloat(fraction["num"].(string), 64)
			scores[i] = num / float64(fraction["den"].(int))
		}
		if n["step"] == "wt-inverse-vol" {
			variance := 0.0
			window, _ := strconv.Atoi(n["window-days"].(string))
			for ticker, weight := range allocations[i] {
				vol := input.vol[ticker]
				if input.signals != nil {
					vol = scoreSignal(t, input, "standard-deviation-return", ticker, window, nil)
				}
				variance += math.Pow(weight*vol, 2)
			}
			scores[i] = 1 / math.Sqrt(variance)
		}
	}
	if n["step"] == "filter" {
		indices := make([]int, len(children))
		for i := range indices {
			indices[i] = i
		}
		rank := func(i int) float64 {
			value := 0.0
			window, _ := strconv.Atoi(n["sort-by-window-days"].(string))
			for ticker, weight := range allocations[i] {
				value += weight * scoreSignal(t, input, n["sort-by-fn"].(string), ticker, window, nil)
			}
			return value
		}
		slices.SortStableFunc(indices, func(a, b int) int {
			if rank(a) == rank(b) {
				return 0
			}
			less := rank(a) < rank(b)
			if n["select-fn"] == "top" {
				less = !less
			}
			if less {
				return -1
			}
			return 1
		})
		count, _ := strconv.Atoi(n["select-n"].(string))
		for _, index := range indices[min(count, len(indices)):] {
			scores[index] = 0
		}
	}
	total := 0.0
	for _, score := range scores {
		total += score
	}
	out := map[string]float64{}
	for i, allocation := range allocations {
		if scores[i] == 0 {
			continue
		}
		for ticker, weight := range allocation {
			out[ticker] += weight * scores[i] / total
		}
	}
	return out
}

func weightedSelectionInput(method, direction string, count int) CustomStrategy {
	return CustomStrategy{Rebalance: "monthly", Benchmark: "SPY", Children: []CustomNode{{
		Kind: "filter", Count: count, Direction: direction, Metric: "cumulative-return", Lookback: 126,
		Children: []CustomNode{{Kind: "weight", Method: method, Lookback: 63, Children: []CustomNode{
			{Kind: "asset", Ticker: "QCOM", Percent: 40}, {Kind: "asset", Ticker: "SPY", Percent: 35}, {Kind: "asset", Ticker: "BIL", Percent: 25},
		}}},
	}}}
}

func TestSelectedWeightAllocations(t *testing.T) {
	input := scoreInputs{rank: map[string]float64{"QCOM": 30, "SPY": 10, "BIL": 1}, vol: map[string]float64{"QCOM": .2, "SPY": .1, "BIL": .01}}
	cases := []struct {
		method, direction string
		count             int
		want              map[string]float64
	}{
		{"inverse-volatility", "top", 1, map[string]float64{"QCOM": 1}},
		{"inverse-volatility", "top", 2, map[string]float64{"QCOM": 1.0 / 3, "SPY": 2.0 / 3}},
		{"inverse-volatility", "top", 3, map[string]float64{"QCOM": 5.0 / 115, "SPY": 10.0 / 115, "BIL": 100.0 / 115}},
		{"inverse-volatility", "bottom", 2, map[string]float64{"SPY": 1.0 / 11, "BIL": 10.0 / 11}},
		{"specified", "top", 1, map[string]float64{"QCOM": 1}},
		{"specified", "top", 2, map[string]float64{"QCOM": 40.0 / 75, "SPY": 35.0 / 75}},
		{"specified", "top", 3, map[string]float64{"QCOM": .4, "SPY": .35, "BIL": .25}},
		{"specified", "bottom", 1, map[string]float64{"BIL": 1}},
		{"specified", "bottom", 2, map[string]float64{"SPY": 35.0 / 60, "BIL": 25.0 / 60}},
	}
	for _, c := range cases {
		t.Run(c.method+c.direction+strconv.Itoa(c.count), func(t *testing.T) {
			compiled, err := CompileCustom(weightedSelectionInput(c.method, c.direction, c.count))
			if err != nil {
				t.Fatal(err)
			}
			got := scoreAllocation(t, compiled.Tree, input)
			if len(got) != len(c.want) {
				t.Fatalf("selected tickers: got %v, want %v", got, c.want)
			}
			for ticker, weight := range c.want {
				if math.Abs(got[ticker]-weight) > 1e-10 {
					t.Fatalf("%s allocation: got %v, want %v", ticker, got, c.want)
				}
			}
		})
	}
}

func TestSelectedWeightOrderAndIdentity(t *testing.T) {
	input := weightedSelectionInput("inverse-volatility", "top", 2)
	wrapped, err := CompileCustom(input)
	if err != nil {
		t.Fatal(err)
	}
	selection := input.Children[0]
	allocation := selection.Children[0]
	selection.Children = allocation.Children
	allocation.Children = []CustomNode{selection}
	input.Children = []CustomNode{allocation}
	outside, err := CompileCustom(input)
	if err != nil {
		t.Fatal(err)
	}
	if wrapped.Key != outside.Key {
		t.Fatal("equivalent selection/weight ordering must share identity")
	}
	allocation.Lookback = 21
	input.Children = []CustomNode{allocation}
	changed, err := CompileCustom(input)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Key == outside.Key {
		t.Fatal("weight lookback must affect cache identity")
	}
}

func TestSelectedWeightTiesAndAllChoices(t *testing.T) {
	for _, method := range []string{"inverse-volatility", "specified"} {
		for _, direction := range []string{"top", "bottom"} {
			for _, order := range [][]float64{{30, 10, 1}, {30, 1, 10}, {10, 30, 1}, {1, 30, 10}, {10, 1, 30}, {1, 10, 30}, {10, 10, 1}, {10, 1, 10}, {1, 10, 10}, {10, 10, 10}} {
				for _, count := range []int{1, 2, 3} {
					compiled, err := CompileCustom(weightedSelectionInput(method, direction, count))
					if err != nil {
						t.Fatal(err)
					}
					tickers := []string{"QCOM", "SPY", "BIL"}
					input := scoreInputs{rank: map[string]float64{}, vol: map[string]float64{"QCOM": .2, "SPY": .1, "BIL": .01}}
					for i, ticker := range tickers {
						input.rank[ticker] = order[i]
					}
					slices.SortStableFunc(tickers, func(a, b string) int {
						if input.rank[a] == input.rank[b] {
							return 0
						}
						less := input.rank[a] < input.rank[b]
						if direction == "top" {
							less = !less
						}
						if less {
							return -1
						}
						return 1
					})
					got := scoreAllocation(t, compiled.Tree, input)
					for _, ticker := range tickers[:count] {
						if got[ticker] <= 0 {
							t.Fatalf("%s %s %d %v selected %v, want %v", method, direction, count, order, got, tickers[:count])
						}
					}
					if len(got) != count {
						t.Fatalf("wrong selected count: %v", got)
					}
				}
			}
		}
	}
}

func TestSelectedWeightRejectsUnrepresentablePools(t *testing.T) {
	input := weightedSelectionInput("inverse-volatility", "top", 1)
	input.Children[0].Children[0].Children[0] = CustomNode{Kind: "group", Name: "Basket", Children: []CustomNode{{Kind: "asset", Ticker: "QCOM"}}}
	if _, err := CompileCustom(input); err == nil || !strings.Contains(err.Error(), "asset candidates") {
		t.Fatalf("wanted clear candidate error, got %v", err)
	}
	input = weightedSelectionInput("inverse-volatility", "top", 3)
	input.Children[0].Children[0].Children = make([]CustomNode, 1000)
	for i := range input.Children[0].Children[0].Children {
		input.Children[0].Children[0].Children[i] = CustomNode{Kind: "asset", Ticker: "SPY"}
	}
	before := input.Children[0].Children[0].Children[0]
	if _, err := CompileCustom(input); err == nil || !strings.Contains(err.Error(), "compilation limit") {
		t.Fatalf("wanted bounded expansion error, got %v", err)
	}
	if !reflect.DeepEqual(before, input.Children[0].Children[0].Children[0]) {
		t.Fatal("compilation mutated source")
	}
}

func TestSelectedWeightRetainsParentPercentageAllocation(t *testing.T) {
	input := weightedSelectionInput("specified", "top", 1)
	selection := input.Children[0]
	selection.Percent = 60
	input.Children = []CustomNode{{Kind: "weight", Method: "specified", Children: []CustomNode{selection, {Kind: "asset", Ticker: "BIL", Percent: 40}}}}
	compiled, err := CompileCustom(input)
	if err != nil {
		t.Fatal(err)
	}
	got := scoreAllocation(t, compiled.Tree, scoreInputs{rank: map[string]float64{"QCOM": 30, "SPY": 10, "BIL": 1}})
	if math.Abs(got["QCOM"]-.6) > 1e-10 || math.Abs(got["BIL"]-.4) > 1e-10 || len(got) != 2 {
		t.Fatalf("parent allocation was lost: %v", got)
	}
}

func TestSelectedWeightUsesRankingFunctionAndWindows(t *testing.T) {
	for metric, spec := range measures {
		if !spec.windowed {
			continue
		}
		input := weightedSelectionInput("inverse-volatility", "top", 2)
		input.Children[0].Metric = metric
		compiled, err := CompileCustom(input)
		if err != nil {
			t.Fatal(err)
		}
		encoded := treeJSON(t, compiled)
		for _, value := range []string{`"lhs-fn":"` + spec.fn + `"`, `"rhs-fn":"` + spec.fn + `"`, `"lhs-window-days":"126"`, `"rhs-window-days":"126"`, `"window-days":"63"`} {
			if !strings.Contains(encoded, value) {
				t.Fatalf("%s lost %s", metric, value)
			}
		}
	}
}
