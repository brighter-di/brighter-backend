package strategy

import (
	"encoding/json"
	"testing"
)

func publishedExample() Publication {
	return Publication{Strategy: CustomStrategy{Name: "Studio selection", Rebalance: "monthly", Benchmark: "SPY", Children: []CustomNode{{Kind: "filter", Count: 1, Direction: "top", Metric: "cumulative-return", Lookback: 126, Children: []CustomNode{{Kind: "weight", Method: "equal", Children: []CustomNode{{Kind: "asset", Ticker: "QCOM"}, {Kind: "asset", Ticker: "SPY"}, {Kind: "asset", Ticker: "BIL"}}}}}}}, Parameters: []PublishedParameter{{ParamSpec: ParamSpec{Key: "1-count", Label: "Count", Min: 1, Max: 3, Step: 1, Default: 1}, Path: []any{"children", 0, "count"}}}}
}
func TestPublishedSelectionUsesBoundCount(t *testing.T) {
	p := publishedExample()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	source, err := p.Materialize(Params{"1-count": 3})
	if err != nil {
		t.Fatal(err)
	}
	if source.Children[0].Count != 3 || p.Strategy.Children[0].Count != 1 {
		t.Fatal("incorrect or mutable rules")
	}
	a, _ := CompileCustom(p.Strategy)
	b, _ := CompileCustom(source)
	if a.Key == b.Key {
		t.Fatal("selection count ignored")
	}
	tpl := p.Template("studio-test", "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "2026-09-13T00:00:00Z")
	if tpl.Author == "" || len(tpl.Holdable()) != 3 {
		t.Fatal("missing author or holdings")
	}
	if _, err := tpl.TreeChecked(tpl.CoerceStrings(func(string) string { return "3" })); err != nil {
		t.Fatal(err)
	}
}
func TestStudioRulesApplyValuesToPublishedStrategiesOnly(t *testing.T) {
	tpl := publishedExample().Template("studio-test", "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "2026-09-13T00:00:00Z")
	rules, ok, err := tpl.StudioRules(Params{"1-count": 2})
	if err != nil || !ok || rules.Children[0].Count != 2 || tpl.Rules.Children[0].Count != 1 {
		t.Fatalf("rules %+v ok %v err %v", rules, ok, err)
	}
	if _, _, err := tpl.StudioRules(Params{"1-count": 9}); err == nil {
		t.Fatal("accepted a value outside the published range")
	}
	builtin, _ := ByID("mag7-rank")
	if _, ok, err := builtin.StudioRules(builtin.Defaults()); ok || err != nil {
		t.Fatalf("built-in template offered Studio rules: %v %v", ok, err)
	}
}

func TestPublishedBindingsRejectInactiveOrForgedFields(t *testing.T) {
	for _, path := range [][]any{{"benchmark"}, {"children", 0, "value"}, {"children", 0, "children", 0, "lookback"}, {"children", 0.5, "count"}} {
		p := publishedExample()
		p.Parameters[0].Path = path
		if p.Validate() == nil {
			t.Fatal("accepted invalid binding", path)
		}
	}
	p := publishedExample()
	p.Parameters = append(p.Parameters, p.Parameters[0])
	if p.Validate() == nil {
		t.Fatal("accepted duplicate key")
	}
}
func TestPublishedPercentageRemainders(t *testing.T) {
	p := Publication{Strategy: CustomStrategy{Name: "Oil sleeve", Rebalance: "monthly", Benchmark: "SPY", Children: []CustomNode{{Kind: "weight", Method: "specified", Children: []CustomNode{{Kind: "asset", Ticker: "XOM", Percent: 30}, {Kind: "asset", Ticker: "CVX", Percent: 20}, {Kind: "asset", Ticker: "BIL", Percent: 50}}}}}, Parameters: []PublishedParameter{{ParamSpec: ParamSpec{Key: "xom", Label: "XOM", Min: 1, Max: 98, Step: 1, Default: 30}, Path: []any{"children", 0, "children", 0, "percent"}, RemainderPath: []any{"children", 0, "children", 2, "percent"}}, {ParamSpec: ParamSpec{Key: "cvx", Label: "CVX", Min: 1, Max: 98, Step: 1, Default: 20}, Path: []any{"children", 0, "children", 1, "percent"}, RemainderPath: []any{"children", 0, "children", 2, "percent"}}}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	out, err := p.Materialize(Params{"xom": 40, "cvx": 25})
	if err != nil || out.Children[0].Children[2].Percent != 35 {
		t.Fatal("invalid remainder", out, err)
	}
	if _, err = p.Materialize(Params{"xom": 60, "cvx": 60}); err == nil {
		t.Fatal("accepted negative remainder")
	}
	p.Parameters[0].RemainderPath = nil
	if p.Validate() == nil {
		t.Fatal("accepted unbalanced binding")
	}
}
func TestPublishedNestedAndDecimalParameters(t *testing.T) {
	var p Publication
	err := json.Unmarshal([]byte(`{"strategy":{"name":"Nested","rebalance":"monthly","benchmark":"SPY","children":[{"kind":"condition","predicate":{"type":"all","conditions":[{"type":"comparison","metric":"cumulative-return","ticker":"SPY","lookback":30,"comparator":"gt","compareMode":"value","value":0.1},{"type":"comparison","metric":"price","ticker":"SPY","comparator":"gt","compareMode":"indicator","rightMetric":"moving-average","rightTicker":"SPY","rightLookback":200}]},"thenChildren":[{"kind":"asset","ticker":"QCOM"}],"elseChildren":[{"kind":"asset","ticker":"BIL"}]}]},"parameters":[{"key":"threshold","label":"Return threshold","min":-1,"max":1,"step":0.1,"default":0.1,"path":["children",0,"predicate","conditions",0,"value"]},{"key":"lookback","label":"Moving average","min":2,"max":252,"step":1,"default":200,"path":["children",0,"predicate","conditions",1,"rightLookback"]}]}`), &p)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Validate(); err != nil {
		t.Fatal(err)
	}
	tpl := p.Template("studio-nested", "author", "date")
	values := tpl.CoerceStrings(func(key string) string {
		if key == "threshold" {
			return "0.3"
		}
		return "100"
	})
	if values["threshold"] != 0.3 {
		t.Fatal("decimal coercion", values)
	}
	out, err := p.Materialize(values)
	if err != nil {
		t.Fatal(err)
	}
	if out.Children[0].Predicate.Conditions[0].Value != 0.3 || out.Children[0].Predicate.Conditions[1].RightLookback != 100 {
		t.Fatal("ignored nested binding")
	}
}
func TestUniqueRulesExcludeNamesIDsAndNeutralWrappers(t *testing.T) {
	p := publishedExample()
	a, _ := CompileCustom(p.Strategy)
	p.Strategy.Name = "New name"
	p.Strategy.Children[0].Children[0].Children[0].Ticker = "QCOMon"
	b, _ := CompileCustom(p.Strategy)
	if !SameTreeRules(a.Tree, b.Tree) {
		t.Fatal("renaming bypasses uniqueness")
	}
	p.Strategy.Children = []CustomNode{{Kind: "group", Name: "New group", Children: p.Strategy.Children}}
	c, _ := CompileCustom(p.Strategy)
	if !SameTreeRules(a.Tree, c.Tree) {
		t.Fatal("group wrapper bypasses uniqueness")
	}
	p = publishedExample()
	p.Strategy.Children[0].Count = 3
	d, _ := CompileCustom(p.Strategy)
	if SameTreeRules(a.Tree, d.Tree) {
		t.Fatal("different selection marked duplicate")
	}
	p.Strategy.Children[0].Count = 50
	e, _ := CompileCustom(p.Strategy)
	if !SameTreeRules(d.Tree, e.Tree) {
		t.Fatal("keeping all candidates bypasses uniqueness")
	}
}
func TestRankingCandidateOrderRemainsSignificant(t *testing.T) {
	p := publishedExample()
	a, _ := CompileCustom(p.Strategy)
	children := p.Strategy.Children[0].Children[0].Children
	children[0], children[1] = children[1], children[0]
	b, _ := CompileCustom(p.Strategy)
	if SameTreeRules(a.Tree, b.Tree) {
		t.Fatal("lost stable ranking tie order")
	}
}

func TestBenchmarkDoesNotBypassPublicationUniqueness(t *testing.T) {
	p := publishedExample()
	a, _ := CompileCustom(p.Strategy)
	p.Strategy.Benchmark = "QQQ"
	b, _ := CompileCustom(p.Strategy)
	if !SameTreeRules(a.Tree, b.Tree) {
		t.Fatal("comparison benchmark bypasses uniqueness")
	}
}
func TestTargetHistoryIncludesActiveNestedAndOscillatorWindows(t *testing.T) {
	tree := map[string]any{"children": []node{{"lhs-window-days": "756", "rhs-fn-params": map[string]int{"short-window": 12, "long-window": 1260}}}}
	if years := TargetWindowYears(tree); years != 6 {
		t.Fatal("long nested window omitted", years)
	}
	if years := TargetWindowYears(map[string]any{"children": []node{{"window-days": "252", "rhs-val": "1000000"}}}); years != 2 {
		t.Fatal("fixed threshold counted as history", years)
	}
}

func TestPublishedCoercionRoundsNegativeHalfStepsLikePositiveOnes(t *testing.T) {
	p := Publication{Parameters: []PublishedParameter{{ParamSpec: ParamSpec{Key: "threshold", Min: -2, Max: 2, Step: 0.1, Default: 0}}}}
	tpl := p.Template("studio-rounding", "author", "date")
	for _, tc := range []struct {
		input    string
		expected float64
	}{{"-.05", -.1}, {".05", .1}, {"-.15", -.1}, {".3", .3}, {"", 0}} {
		got := tpl.CoerceStrings(func(string) string { return tc.input })
		if got["threshold"] != tc.expected {
			t.Fatal(tc.input, got, tc.expected)
		}
	}
}
