package service

import (
	"context"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/brighter-di/backend/internal/tokens"
)

// The strategy list, filtered and paged. The catalogue is open-ended — anyone
// can publish from Studio — so the list is narrowed and cut here rather than
// in the browser, and the rules that decide what "leveraged" or "beats its
// benchmark" means live on this side where they cannot drift from the API.

// CataloguePeriods are the ranges the list may ask for: the ones the warmer
// keeps hot (see Warm). A cold range here would fan a backtest engine call out across
// the whole catalogue on one page view, so the two lists move together.
var CataloguePeriods = map[string]bool{"1y": true, "3y": true, "5y": true}

var catalogueCadences = map[string]bool{"daily": true, "weekly": true, "monthly": true}

const (
	SortReturn = "return"
	SortNewest = "newest"
)

// CatalogueQuery is one reading of the list: which strategies, over which
// period, in what order, and which slice of them.
type CatalogueQuery struct {
	Period         string
	Assets         []string // holds any of these; empty means any
	Cadences       []string // empty means any
	BeatsBenchmark bool
	Leveraged      bool // include strategies that can hold leveraged or inverse funds
	Sort           string
	Offset, Limit  int
	Points         int
}

// CardAsset is a logo on a card. The list draws a row of them and nothing
// else, so it carries none of the contract addresses, ISIN or full name that
// only a strategy's own page needs — those are most of an AssetView's bytes.
type CardAsset struct {
	Ticker     string `json:"ticker"`
	OndoSymbol string `json:"ondoSymbol"`
	Icon       string `json:"icon"`
}

// CardBacktest is the figure and the sparkline, with the benchmark comparison
// already made so the card never has to make it.
type CardBacktest struct {
	Range           string     `json:"range"`
	StartDate       string     `json:"startDate"`
	EndDate         string     `json:"endDate"`
	TotalReturn     float64    `json:"totalReturn"`
	BenchmarkTicker string     `json:"benchmarkTicker"`
	BenchmarkReturn float64    `json:"benchmarkReturn"`
	BeatsBenchmark  bool       `json:"beatsBenchmark"`
	Series          SeriesView `json:"series"`
}

// StrategyCardView is one card. Backtest is nil when it could not be had, so
// one slow or failing strategy never blanks the list.
type StrategyCardView struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Thesis      string        `json:"thesis"`
	Author      string        `json:"author"`
	PublishedAt string        `json:"publishedAt,omitempty"`
	Cadence     string        `json:"cadence"`
	Assets      []CardAsset   `json:"assets"`
	Leveraged   bool          `json:"leveraged"`
	Backtest    *CardBacktest `json:"backtest"`
	Target      *TargetView   `json:"target,omitempty"`
}

// CataloguePage is one page of the list. Total counts everything that passed
// the filters, not what fits on the page, so the reader is told how much there
// is to scroll through.
type CataloguePage struct {
	Total      int                `json:"total"`
	Offset     int                `json:"offset"`
	Limit      int                `json:"limit"`
	Strategies []StrategyCardView `json:"strategies"`
}

// Catalogue answers one page of the strategy list.
//
// Order of work matters. The metadata filters — assets, cadence, leverage —
// cost nothing, so they run first and cut the set before any backtest is read.
// Sorting by return and filtering on the benchmark both read the backtest, so
// when either is asked for, every strategy that survived the metadata filters
// has to be read before the list can be ordered or cut. Sorting by newest
// needs none of that, and reads only the page it returns.
func (s *Service) Catalogue(ctx context.Context, q CatalogueQuery) (*CataloguePage, error) {
	if !CataloguePeriods[q.Period] {
		return nil, errf(400, "BAD_PERIOD", "period must be one of 1y, 3y, 5y")
	}
	if q.Sort != SortReturn && q.Sort != SortNewest {
		return nil, errf(400, "BAD_SORT", "sort must be one of return, newest")
	}
	for _, c := range q.Cadences {
		if !catalogueCadences[c] {
			return nil, errf(400, "BAD_CADENCE", "cadence must be one of daily, weekly, monthly")
		}
	}
	for _, a := range q.Assets {
		if _, ok := tokens.Lookup(a); !ok {
			return nil, errf(400, "BAD_ASSET", "unknown asset %q", a)
		}
	}

	templates, err := s.PublicTemplates(ctx)
	if err != nil {
		return nil, err
	}
	matched := make([]TemplateView, 0, len(templates))
	for _, t := range templates {
		if !t.Retired && q.matches(t) {
			matched = append(matched, t)
		}
	}

	// Newest needs no backtest to order by, so the page is cut first and only
	// its own strategies are read.
	if q.Sort == SortNewest && !q.BeatsBenchmark {
		newestFirst(matched)
		return &CataloguePage{
			Total:      len(matched),
			Offset:     q.Offset,
			Limit:      q.Limit,
			Strategies: s.cards(ctx, pageOf(matched, q.Offset, q.Limit), q),
		}, nil
	}

	cards := s.cards(ctx, matched, q)
	if q.BeatsBenchmark {
		kept := make([]StrategyCardView, 0, len(cards))
		for _, c := range cards {
			if c.Backtest != nil && c.Backtest.BeatsBenchmark {
				kept = append(kept, c)
			}
		}
		cards = kept
	}
	if q.Sort == SortReturn {
		// A strategy whose backtest did not arrive sorts last rather than
		// jumping the queue, as the list has always done.
		sort.SliceStable(cards, func(i, j int) bool {
			return cardReturn(cards[i]) > cardReturn(cards[j])
		})
	}
	return &CataloguePage{
		Total:      len(cards),
		Offset:     q.Offset,
		Limit:      q.Limit,
		Strategies: pageOf(cards, q.Offset, q.Limit),
	}, nil
}

// matches applies only what can be decided from the strategy itself.
func (q CatalogueQuery) matches(t TemplateView) bool {
	if len(q.Cadences) > 0 && !slices.Contains(q.Cadences, t.Cadence) {
		return false
	}
	if !q.Leveraged && holdsLeveraged(t) {
		return false
	}
	if len(q.Assets) == 0 {
		return true
	}
	holdable := t.Holdable()
	return slices.ContainsFunc(q.Assets, func(want string) bool {
		return slices.ContainsFunc(holdable, func(held string) bool { return strings.EqualFold(held, want) })
	})
}

func holdsLeveraged(t TemplateView) bool {
	return slices.ContainsFunc(t.Assets, func(a AssetView) bool { return tokens.LeveragedKind(a.Kind) })
}

// newestFirst puts published strategies before the presets, newest first.
// Presets carry no publishedAt, so they keep the order the catalogue lists
// them in, reversed — the order the list has shown since it had one.
func newestFirst(views []TemplateView) {
	slices.Reverse(views)
	sort.SliceStable(views, func(i, j int) bool {
		return views[i].PublishedAt != "" && views[j].PublishedAt == ""
	})
}

func cardReturn(c StrategyCardView) float64 {
	if c.Backtest == nil {
		return -1e308
	}
	return c.Backtest.TotalReturn
}

func pageOf[T any](items []T, offset, limit int) []T {
	if offset >= len(items) {
		return []T{}
	}
	return items[offset:min(offset+limit, len(items))]
}

// cards reads each strategy's backtest and current allocation, from the same
// caches the per-strategy routes use.
func (s *Service) cards(ctx context.Context, views []TemplateView, q CatalogueQuery) []StrategyCardView {
	out := make([]StrategyCardView, len(views))
	sem := make(chan struct{}, summaryWorkers)
	var wg sync.WaitGroup
	for i, t := range views {
		out[i] = cardOf(t)
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			if bt, err := s.Backtest(ctx, t.Template, t.Defaults, q.Period); err == nil {
				out[i].Backtest = cardBacktest(bt.Downsample(q.Points))
			} else {
				s.log.Debug("catalogue backtest unavailable", "template", t.ID, "err", err)
			}
			if tv, err := s.Target(ctx, t.Template, t.Defaults); err == nil {
				out[i].Target = tv
			} else {
				s.log.Debug("catalogue target unavailable", "template", t.ID, "err", err)
			}
		}()
	}
	wg.Wait()
	return out
}

func cardOf(t TemplateView) StrategyCardView {
	c := StrategyCardView{
		ID: t.ID, Name: t.Name, Thesis: t.Thesis, Author: t.Author,
		PublishedAt: t.PublishedAt, Cadence: t.Cadence,
		Assets:    make([]CardAsset, 0, len(t.Assets)),
		Leveraged: holdsLeveraged(t),
	}
	for _, a := range t.Assets {
		c.Assets = append(c.Assets, CardAsset{Ticker: a.Ticker, OndoSymbol: a.OndoSymbol, Icon: a.Icon})
	}
	return c
}

func cardBacktest(b BacktestView) *CardBacktest {
	c := &CardBacktest{
		Range: b.Range, StartDate: b.StartDate, EndDate: b.EndDate,
		TotalReturn:     b.Metrics.TotalReturn,
		BenchmarkTicker: b.Benchmark.Ticker,
		Series:          b.Series,
	}
	if b.Benchmark.Metrics != nil {
		c.BenchmarkReturn = b.Benchmark.Metrics.TotalReturn
		c.BeatsBenchmark = b.Metrics.TotalReturn > b.Benchmark.Metrics.TotalReturn
	}
	return c
}

/* ── Facets ─────────────────────────────────────────────────────────── */

// The options the filter row offers. The list page no longer reads
// /v1/templates, so this is where the asset picker gets its tickers — grouped
// by the kind the registry already carries, and counted so a reader can see
// what is worth narrowing by. No backtests: this is metadata only.

type FacetAsset struct {
	Ticker     string `json:"ticker"`
	Name       string `json:"name"`
	OndoSymbol string `json:"ondoSymbol"`
	Kind       string `json:"kind"`
	Icon       string `json:"icon"`
	Leveraged  bool   `json:"leveraged"`
	Count      int    `json:"count"` // listed strategies that can hold it
}

type FacetCount struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

type CatalogueFacets struct {
	Assets    []FacetAsset `json:"assets"`
	Cadences  []FacetCount `json:"cadences"`
	Total     int          `json:"total"`     // listed strategies, leveraged ones included
	Leveraged int          `json:"leveraged"` // how many of those the toggle reveals
}

// kindOrder groups the picker the way a reader scans it: what a strategy
// usually holds first, the geared funds last, where the toggle leaves them.
var kindOrder = []string{
	"Common Stock", "ADR", "Preferred Stock",
	"Equity ETF", "Bond ETF", "Commodity ETF", "Crypto ETF", "Closed-End Fund",
	"Currency", "Leveraged ETF", "Inverse ETF",
}

func (s *Service) CatalogueFacets(ctx context.Context) (*CatalogueFacets, error) {
	templates, err := s.PublicTemplates(ctx)
	if err != nil {
		return nil, err
	}
	out := &CatalogueFacets{Assets: []FacetAsset{}, Cadences: []FacetCount{}}
	assets := map[string]*FacetAsset{}
	cadences := map[string]int{}
	for _, t := range templates {
		if t.Retired {
			continue
		}
		out.Total++
		if holdsLeveraged(t) {
			out.Leveraged++
		}
		cadences[t.Cadence]++
		for _, a := range t.Assets {
			f, ok := assets[a.Ticker]
			if !ok {
				f = &FacetAsset{
					Ticker: a.Ticker, Name: a.Name, OndoSymbol: a.OndoSymbol,
					Kind: a.Kind, Icon: a.Icon, Leveraged: tokens.LeveragedKind(a.Kind),
				}
				assets[a.Ticker] = f
			}
			f.Count++
		}
	}
	for _, f := range assets {
		out.Assets = append(out.Assets, *f)
	}
	rank := func(kind string) int {
		if i := slices.Index(kindOrder, kind); i >= 0 {
			return i
		}
		return len(kindOrder)
	}
	sort.Slice(out.Assets, func(i, j int) bool {
		a, b := out.Assets[i], out.Assets[j]
		if ra, rb := rank(a.Kind), rank(b.Kind); ra != rb {
			return ra < rb
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Ticker < b.Ticker
	})
	for _, cadence := range []string{"daily", "weekly", "monthly"} {
		if n := cadences[cadence]; n > 0 {
			out.Cadences = append(out.Cadences, FacetCount{Value: cadence, Count: n})
		}
	}
	return out, nil
}
