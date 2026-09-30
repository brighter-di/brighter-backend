package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/brighter-di/backend/internal/store"
	"github.com/brighter-di/backend/internal/strategy"
)

type testPublicationCatalog struct{ row store.PublishedStrategy }

func (c testPublicationCatalog) PublishedStrategy(_ context.Context, id string) (*store.PublishedStrategy, error) {
	if id != c.row.ID {
		return nil, store.ErrNotFound
	}
	row := c.row
	return &row, nil
}
func (c testPublicationCatalog) PublishedStrategies(context.Context) ([]store.PublishedStrategy, error) {
	return []store.PublishedStrategy{c.row}, nil
}
func publicationFixture(t *testing.T) (strategy.Publication, store.PublishedStrategy) {
	t.Helper()
	p := strategy.Publication{Strategy: strategy.CustomStrategy{Name: "Community selection", Rebalance: "monthly", Benchmark: "SPY", Children: []strategy.CustomNode{{Kind: "filter", Count: 1, Direction: "top", Metric: "cumulative-return", Lookback: 126, Children: []strategy.CustomNode{{Kind: "weight", Method: "equal", Children: []strategy.CustomNode{{Kind: "asset", Ticker: "AAPL"}, {Kind: "asset", Ticker: "SPY"}, {Kind: "asset", Ticker: "BIL"}}}}}}}, Parameters: []strategy.PublishedParameter{{ParamSpec: strategy.ParamSpec{Key: "count", Label: "Count", Min: 1, Max: 3, Step: 1, Default: 1}, Path: []any{"children", 0, "count"}}, {ParamSpec: strategy.ParamSpec{Key: "lookback", Label: "Lookback", Min: 2, Max: 1260, Step: 1, Default: 126}, Path: []any{"children", 0, "lookback"}}}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	compiled, _ := strategy.CompileCustom(p.Strategy)
	return p, store.PublishedStrategy{ID: "studio-fixture", Author: ownerLower, PublishedAt: testNow, Document: raw, RulesKey: strategy.RulesFingerprint(compiled.Tree)}
}
func TestPublishedCatalogBacktestAndTargetUseSavedBindings(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, false)
	p, row := publicationFixture(t)
	h.svc.catalog = testPublicationCatalog{row}
	views, err := h.svc.PublicTemplates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	view := views[len(views)-1]
	if view.ID != row.ID || view.Author != ownerLower || len(view.Params) != 2 || len(view.Assets) != 3 || view.Defaults["count"] != 1 {
		t.Fatalf("catalog %+v", view)
	}
	if views[0].Author != "Brighter" {
		t.Fatal("preset has no author")
	}
	tpl, err := h.svc.Template(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, count := range []float64{1, 3} {
		params := tpl.Coerce(func(k string) (float64, bool) {
			if k == "count" {
				return count, true
			}
			return 0, false
		})
		if _, err = h.svc.Backtest(ctx, tpl, params, "5y"); err != nil {
			t.Fatal(err)
		}
		var body struct {
			StrategyTree struct {
				RawValue map[string]any `json:"raw_value"`
			} `json:"strategyTree"`
		}
		requests := h.backtestEngine.Requests()
		if err = json.Unmarshal(requests[len(requests)-1], &body); err != nil {
			t.Fatal(err)
		}
		expected, _ := p.Materialize(params)
		compiled, _ := strategy.CompileCustom(expected)
		if !strategy.SameTreeRules(body.StrategyTree.RawValue, compiled.Tree) {
			t.Fatal("saved binding did not reach BacktestEngine", count)
		}
	}
	if h.backtestEngine.Calls() != 2 {
		t.Fatal("distinct rules used same cache entry")
	}
	it, err := h.svc.resolve(ctx, InvestRequest{TemplateID: row.ID, Params: map[string]float64{"count": 2, "lookback": 756}, Owner: owner, ChainID: 56})
	if err != nil || it.params["count"] != 2 || it.owner != ownerLower {
		t.Fatal("investment cannot resolve published rules", it, err)
	}
	h.backtestEngine.Set(map[string]float64{"AAPL": 1}, testNow)
	if _, err = h.svc.Target(ctx, tpl, it.params); err != nil {
		t.Fatal(err)
	}
	requests := h.backtestEngine.Requests()
	var body map[string]any
	json.Unmarshal(requests[len(requests)-1], &body)
	if body["start_date"] != testNow.AddDate(-4, 0, 0).Format(dateFmt) {
		t.Fatal("long published lookback not warmed up", body["start_date"])
	}
	if _, err = h.svc.Template(ctx, "studio-missing"); err == nil {
		t.Fatal("missing publication accepted")
	} else {
		wantCode(t, err, "UNKNOWN_TEMPLATE")
	}
}
func TestPublishedInvalidParameterCombinationStopsBeforeBacktestEngine(t *testing.T) {
	h := newHarness(t, false)
	p := strategy.Publication{Strategy: strategy.CustomStrategy{Name: "Percentages", Rebalance: "monthly", Benchmark: "SPY", Children: []strategy.CustomNode{{Kind: "weight", Method: "specified", Children: []strategy.CustomNode{{Kind: "asset", Ticker: "AAPL", Percent: 30}, {Kind: "asset", Ticker: "SPY", Percent: 20}, {Kind: "asset", Ticker: "BIL", Percent: 50}}}}}, Parameters: []strategy.PublishedParameter{{ParamSpec: strategy.ParamSpec{Key: "qcom", Label: "AAPL", Min: 1, Max: 98, Step: 1, Default: 30}, Path: []any{"children", 0, "children", 0, "percent"}, RemainderPath: []any{"children", 0, "children", 2, "percent"}}, {ParamSpec: strategy.ParamSpec{Key: "spy", Label: "SPY", Min: 1, Max: 98, Step: 1, Default: 20}, Path: []any{"children", 0, "children", 1, "percent"}, RemainderPath: []any{"children", 0, "children", 2, "percent"}}}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	tpl := p.Template("studio-percentages", ownerLower, "date")
	params := strategy.Params{"qcom": 60, "spy": 60}
	_, err := h.svc.Backtest(context.Background(), tpl, params, "5y")
	wantCode(t, err, "INVALID_STRATEGY")
	_, err = h.svc.Target(context.Background(), tpl, params)
	wantCode(t, err, "INVALID_STRATEGY")
	if h.backtestEngine.Calls() != 0 {
		t.Fatal("invalid rules reached upstream")
	}
}
func TestPublishedBNBAssetsAreExecutableAndChainSpecific(t *testing.T) {
	h := newHarness(t, false)
	p := strategy.Publication{Strategy: strategy.CustomStrategy{Name: "Research", Rebalance: "monthly", Benchmark: "SPY", Children: []strategy.CustomNode{{Kind: "asset", Ticker: "ABNB"}}}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	v := h.svc.templateView(p.Template("studio-research", ownerLower, "date"))
	if len(v.Assets) != 1 || v.Assets[0].Ticker != "ABNB" {
		t.Fatal("research holding silently omitted", v.Assets)
	}
	h.backtestEngine.Set(map[string]float64{"ABNB": 1}, testNow)
	target, err := h.svc.Target(context.Background(), v.Template, v.Defaults)
	if err != nil || len(target.Chains) != 1 || !target.Chains[0].Executable {
		t.Fatal("registered BNB token is not executable", target, err)
	}
}
func TestPublishedStrategyInvestmentAndReevaluation(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()
	_, row := publicationFixture(t)
	if err := h.store.PublishStrategy(ctx, &row); err != nil {
		t.Fatal(err)
	}
	h.backtestEngine.Set(map[string]float64{"AAPL": 1}, testNow.AddDate(0, 0, -1))
	pf := h.invest(t, InvestRequest{TemplateID: row.ID, Params: map[string]float64{"count": 2, "lookback": 126}, ChainID: 56, Owner: owner})
	if pf.TemplateName != "Community selection" || pf.Params["count"] != 2 {
		t.Fatalf("published investment %+v", pf)
	}
	combos, err := h.svc.Combos(ctx)
	if err != nil || len(combos) != 1 {
		t.Fatal(combos, err)
	}
	h.backtestEngine.Set(map[string]float64{"SPY": 0.5, "BIL": 0.5}, testNow)
	r := h.svc.Reevaluate(ctx, combos[0], "manual", 6)
	if r.Status != "published" {
		t.Fatalf("published strategy stopped rebalancing: %+v", r)
	}
	updated, err := h.svc.Portfolio(ctx, pf.PortfolioID)
	if err != nil || updated.TemplateName != "Community selection" || updated.Version != 2 {
		t.Fatal(updated, err)
	}
}

func TestBNBStudioStrategyInvestsUsingAllSelectedContracts(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()
	p := strategy.Publication{Strategy: strategy.CustomStrategy{Name: "BNB three stocks", Rebalance: "monthly", Benchmark: "SPY", Children: []strategy.CustomNode{{Kind: "filter", Count: 3, Direction: "top", Metric: "cumulative-return", Lookback: 126, Children: []strategy.CustomNode{{Kind: "weight", Method: "equal", Children: []strategy.CustomNode{{Kind: "asset", Ticker: "AMD"}, {Kind: "asset", Ticker: "PLTR"}, {Kind: "asset", Ticker: "SMCI"}}}}}}}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(p)
	compiled, _ := strategy.CompileCustom(p.Strategy)
	row := store.PublishedStrategy{ID: "studio-bnb-three", Author: ownerLower, Document: raw, RulesKey: strategy.RulesFingerprint(compiled.Tree)}
	if err := h.store.PublishStrategy(ctx, &row); err != nil {
		t.Fatal(err)
	}
	h.backtestEngine.Set(map[string]float64{"AMD": .3334, "PLTR": .3333, "SMCI": .3333}, testNow.AddDate(0, 0, -1))
	pf := h.invest(t, InvestRequest{TemplateID: row.ID, ChainID: 56, Owner: owner})
	if len(pf.Allocation) != 3 {
		t.Fatal("selected stock dropped", pf.Allocation)
	}
	requests := h.portfolioEngine.Requests("POST /strategies")
	if len(requests) != 1 {
		t.Fatal("portfolio did not create a PortfolioEngine strategy", len(requests))
	}
	got := weightsByAsset(requests[0].Body)
	for ticker, expected := range map[string]string{"AMD": "33.34", "PLTR": "33.33", "SMCI": "33.33"} {
		if got[assetID(t, ticker)] != expected {
			t.Fatal("wrong PortfolioEngine token or weight", ticker, got)
		}
	}
}
