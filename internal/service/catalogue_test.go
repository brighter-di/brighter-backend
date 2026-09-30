package service

import (
	"slices"
	"testing"

	"github.com/brighter-di/backend/internal/metrics"
	"github.com/brighter-di/backend/internal/strategy"
)

// view builds a listed strategy with the kinds its assets carry, so the
// metadata filters can be read without a backtest engine or a database. Every
// strategy that can hold a leveraged fund today is retired, so this is the
// only place the leverage filter can actually be exercised.
func view(id, cadence string, universe []string, kinds map[string]string) TemplateView {
	v := TemplateView{Template: &strategy.Template{ID: id, Cadence: cadence, Universe: universe}}
	for _, ticker := range universe {
		kind := kinds[ticker]
		if kind == "" {
			kind = "Common Stock"
		}
		v.Assets = append(v.Assets, AssetView{Ticker: ticker, Kind: kind})
	}
	return v
}

func TestCatalogueFiltersReadOnlyTheStrategyItself(t *testing.T) {
	plain := view("plain", "monthly", []string{"AAPL", "MSFT"}, nil)
	daily := view("daily", "daily", []string{"SPY", "BIL"}, nil)
	geared := view("geared", "daily", []string{"QQQ", "TQQQ"}, map[string]string{"TQQQ": "Leveraged ETF"})
	hedged := view("hedged", "weekly", []string{"QQQ", "SQQQ"}, map[string]string{"SQQQ": "Inverse ETF"})

	for _, c := range []struct {
		name string
		q    CatalogueQuery
		want map[string]bool
	}{
		{"nothing set keeps everything unleveraged", CatalogueQuery{}, map[string]bool{"plain": true, "daily": true}},
		{"leveraged=1 admits the geared ones", CatalogueQuery{Leveraged: true}, map[string]bool{"plain": true, "daily": true, "geared": true, "hedged": true}},
		{"inverse counts as leveraged", CatalogueQuery{}, map[string]bool{"plain": true, "daily": true}},
		{"one cadence", CatalogueQuery{Cadences: []string{"daily"}}, map[string]bool{"daily": true}},
		{"two cadences", CatalogueQuery{Cadences: []string{"daily", "monthly"}}, map[string]bool{"plain": true, "daily": true}},
		{"holds any of, not all of", CatalogueQuery{Assets: []string{"AAPL", "SPY"}}, map[string]bool{"plain": true, "daily": true}},
		{"a ticker nobody holds", CatalogueQuery{Assets: []string{"TLT"}}, map[string]bool{}},
		{"tickers are matched without case", CatalogueQuery{Assets: []string{"aapl"}}, map[string]bool{"plain": true}},
		{"asset and cadence both apply", CatalogueQuery{Assets: []string{"SPY", "AAPL"}, Cadences: []string{"daily"}}, map[string]bool{"daily": true}},
		{"a leveraged holding is excluded even when its ticker is asked for", CatalogueQuery{Assets: []string{"TQQQ"}}, map[string]bool{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := map[string]bool{}
			for _, v := range []TemplateView{plain, daily, geared, hedged} {
				if c.q.matches(v) {
					got[v.ID] = true
				}
			}
			if len(got) != len(c.want) {
				t.Fatalf("matched %v, want %v", got, c.want)
			}
			for id := range c.want {
				if !got[id] {
					t.Fatalf("matched %v, want %v", got, c.want)
				}
			}
		})
	}
}

func TestNewestPutsPublishedStrategiesFirst(t *testing.T) {
	// PublicTemplates emits the presets in catalogue order, then published
	// strategies oldest first.
	views := []TemplateView{
		{Template: &strategy.Template{ID: "preset-a"}},
		{Template: &strategy.Template{ID: "preset-b"}},
		{Template: &strategy.Template{ID: "studio-old", PublishedAt: "2026-01-01T00:00:00Z"}},
		{Template: &strategy.Template{ID: "studio-new", PublishedAt: "2026-06-01T00:00:00Z"}},
	}
	newestFirst(views)
	want := []string{"studio-new", "studio-old", "preset-b", "preset-a"}
	for i, id := range want {
		if views[i].ID != id {
			t.Fatalf("position %d is %s, want %s", i, views[i].ID, id)
		}
	}
}

func TestBeatsBenchmarkComparesTheSamePeriod(t *testing.T) {
	backtest := func(strategyReturn float64, benchmark *metrics.Metrics) BacktestView {
		return BacktestView{
			Range:     "1y",
			Metrics:   metrics.Metrics{TotalReturn: strategyReturn},
			Benchmark: BenchmarkView{Ticker: "SPY", Metrics: benchmark},
		}
	}
	spy := &metrics.Metrics{TotalReturn: 0.16}

	if c := cardBacktest(backtest(0.20, spy)); !c.BeatsBenchmark || c.BenchmarkReturn != 0.16 {
		t.Fatalf("ahead of the benchmark: %+v", c)
	}
	if c := cardBacktest(backtest(0.10, spy)); c.BeatsBenchmark {
		t.Fatalf("behind the benchmark: %+v", c)
	}
	// Matching it exactly is not beating it.
	if c := cardBacktest(backtest(0.16, spy)); c.BeatsBenchmark {
		t.Fatalf("level with the benchmark: %+v", c)
	}
	// A backtest whose benchmark leg is missing claims nothing.
	if c := cardBacktest(backtest(0.20, nil)); c.BeatsBenchmark || c.BenchmarkReturn != 0 {
		t.Fatalf("no benchmark: %+v", c)
	}
}

func TestAStrategyWithoutABacktestSortsLast(t *testing.T) {
	ahead := StrategyCardView{ID: "ahead", Backtest: &CardBacktest{TotalReturn: 0.2}}
	behind := StrategyCardView{ID: "behind", Backtest: &CardBacktest{TotalReturn: -0.5}}
	unknown := StrategyCardView{ID: "unknown"}
	if !(cardReturn(ahead) > cardReturn(behind) && cardReturn(behind) > cardReturn(unknown)) {
		t.Fatal("a missing backtest must not jump the queue")
	}
}

func TestPagesPartitionTheListWithoutRepeating(t *testing.T) {
	items := []string{"a", "b", "c", "d", "e"}
	if got := pageOf(items, 0, 2); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatal("first page", got)
	}
	if got := pageOf(items, 4, 2); len(got) != 1 || got[0] != "e" {
		t.Fatal("a short last page", got)
	}
	if got := pageOf(items, 5, 2); len(got) != 0 {
		t.Fatal("past the end", got)
	}
	if got := pageOf(items, 99, 2); len(got) != 0 {
		t.Fatal("far past the end", got)
	}
}

// The list may only offer periods the warmer keeps hot; a cold one would fan a
// backtest engine call out across the whole catalogue on one page view.
func TestEveryOfferedPeriodIsOneTheWarmerKeepsHot(t *testing.T) {
	for period := range CataloguePeriods {
		if !slices.Contains(warmRanges, period) {
			t.Fatalf("the list offers %s but the warmer does not keep it hot", period)
		}
	}
	if CataloguePeriods["10y"] {
		t.Fatal("10y is not warmed and must not be offered")
	}
}
