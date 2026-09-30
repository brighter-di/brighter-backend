package service

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/brighter-di/backend/internal/prices"
	"github.com/brighter-di/backend/internal/store"
	"github.com/brighter-di/backend/internal/strategy"
	"github.com/brighter-di/backend/internal/tokens"
)

type AssetChain struct {
	ChainID int64  `json:"chainId"`
	Address string `json:"address"`
}

type AssetStrategy struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// "universe": the rule can pick it. "fallback": it is where the rule
	// goes to be safe.
	Role string `json:"role"`
	// Weight at default settings right now; nil when not computed yet.
	HeldBps *int `json:"heldBps"`
}

type AssetDetail struct {
	AssetView
	About      string          `json:"about"`
	Chains     []AssetChain    `json:"chains"`
	Strategies []AssetStrategy `json:"strategies"`
}

func (s *Service) Assets() []AssetView {
	all := tokens.All()
	out := make([]AssetView, len(all))
	for i, a := range all {
		out[i] = assetView(a)
	}
	return out
}

func lookupAsset(ticker string) (tokens.Asset, error) {
	a, ok := tokens.Lookup(strings.ToUpper(ticker))
	if !ok {
		return tokens.Asset{}, errf(404, "UNKNOWN_ASSET", "Unknown asset %q.", ticker)
	}
	return a, nil
}

func (s *Service) Asset(ticker string) (*AssetDetail, error) {
	a, err := lookupAsset(ticker)
	if err != nil {
		return nil, err
	}
	d := &AssetDetail{AssetView: assetView(a), About: a.About, Chains: []AssetChain{}, Strategies: []AssetStrategy{}}
	for _, c := range s.enabledChains() {
		if addr, ok := a.Address(c.ID); ok {
			d.Chains = append(d.Chains, AssetChain{ChainID: c.ID, Address: addr})
		}
	}
	for _, t := range strategy.Templates {
		if t.Retired {
			continue
		}
		role := ""
		for _, u := range t.Universe {
			if u == a.Ticker {
				role = "universe"
			}
		}
		if role == "" && t.Fallback == a.Ticker {
			role = "fallback"
		}
		if role == "" {
			continue
		}
		st := AssetStrategy{ID: t.ID, Name: t.Name, Role: role}
		// Only what is already computed (the warmup keeps defaults fresh): an
		// asset page never waits on research calls.
		if v, ok := s.cache.get(targetCacheKey(t, t.Defaults())); ok {
			if tg := v.(*target); tg.gateErr == nil {
				b := tg.bps[a.Ticker]
				st.HeldBps = &b
			}
		}
		d.Strategies = append(d.Strategies, st)
	}
	// Largest current weight first, so the page opens with the strategies
	// actually holding the asset. Ones whose weight is not computed yet follow,
	// in template order.
	sort.SliceStable(d.Strategies, func(i, j int) bool { return heldBps(d.Strategies[i]) > heldBps(d.Strategies[j]) })
	return d, nil
}

// heldBps is a strategy's weight in the asset, or -1 when it is not computed.
func heldBps(s AssetStrategy) int {
	if s.HeldBps == nil {
		return -1
	}
	return *s.HeldBps
}

/* ── Daily closes ───────────────────────────────────────────────────── */

// ClosesView is one calendar year of an asset's daily closes, oldest first.
// The asset page asks for its range a year at a time and draws each year as
// it arrives.
type ClosesView struct {
	Ticker string    `json:"ticker"`
	Year   int       `json:"year"`
	Dates  []string  `json:"dates"`
	Closes []float64 `json:"closes"`
}

const (
	// A past year's closes never change; the current year's grow daily.
	closesTTLPast    = 24 * time.Hour
	closesTTLCurrent = 15 * time.Minute
	// How long an expired copy may still answer while a refresh runs.
	closesGrace = 7 * 24 * time.Hour
	// How many years back an asset page can reach. The price source reaches
	// ten years back from today, so nine keeps every year whole.
	maxCloseYears = 9
	// The warmup makes one price-source call per asset with this pause
	// between, and refreshes the current year well inside its TTL, so a
	// visitor finds it fresh.
	closesWarmPace  = 300 * time.Millisecond
	closesWarmEvery = 10 * time.Minute
)

func priceClass(a tokens.Asset) prices.Class {
	if strings.Contains(a.Kind, "ETF") {
		return prices.ETF
	}
	return prices.Stock
}

func closesKey(ticker string, year int) string { return "closes|" + ticker + "|" + strconv.Itoa(year) }

func closesTTL(year int, now time.Time) time.Duration {
	if year < now.Year() {
		return closesTTLPast
	}
	return closesTTLCurrent
}

// AssetCloses serves one year of an asset's closes without making a visitor
// wait on the price source whenever any copy exists:
//
//   - a fresh copy in memory answers;
//   - a finished year stored in full answers from the database;
//   - an expired copy (in memory, or stored for the current year) answers at
//     once while a background refresh fetches the new one.
//
// Only a year with no copy anywhere waits on the source. A refresh stores and
// caches exactly what a blocking fetch would, so the database and the cache
// end up the same either way.
func (s *Service) AssetCloses(ctx context.Context, ticker string, year int) (*ClosesView, error) {
	a, err := lookupAsset(ticker)
	if err != nil {
		return nil, err
	}
	if a.Kind == "Currency" {
		return nil, errf(422, "NO_PRICE_HISTORY", "%s is a currency token, not a stock or ETF price series.", a.Ticker)
	}
	now := s.now().UTC()
	if year > now.Year() || year < now.Year()-maxCloseYears {
		return nil, errf(400, "BAD_YEAR", "year must be between %d and %d", now.Year()-maxCloseYears, now.Year())
	}
	key := closesKey(a.Ticker, year)
	if v, ok := s.cache.get(key); ok {
		return v.(*ClosesView), nil
	}
	if year < now.Year() {
		if v := s.storedYear(ctx, a.Ticker, year, true); v != nil {
			s.cache.set(key, v, closesTTLPast, closesGrace)
			return v, nil
		}
	}
	if v, ok := s.cache.lastGood(key); ok {
		s.refreshCloses(a, year)
		return v.(*ClosesView), nil
	}
	if year == now.Year() {
		if stored := s.storedYear(ctx, a.Ticker, year, false); stored != nil {
			s.refreshCloses(a, year)
			return stored, nil
		}
	}
	v, err := s.cached(ctx, key, cachePolicy{ttl: closesTTL(year, now), grace: closesGrace}, func(ctx context.Context) (any, error) {
		return s.fetchFrom(ctx, a, year, now)
	})
	if err != nil {
		// The copy on disk outlives restarts.
		if stored := s.storedYear(ctx, a.Ticker, year, false); stored != nil {
			s.log.Warn("price source failed; serving stored closes", "ticker", a.Ticker, "year", year, "err", err)
			return stored, nil
		}
		return nil, err
	}
	return v.(*ClosesView), nil
}

// refreshCloses fetches one year in the background. Concurrent refreshes of
// the same year share one call to the source.
func (s *Service) refreshCloses(a tokens.Asset, year int) {
	go func() {
		now := s.now().UTC()
		key := closesKey(a.Ticker, year)
		if _, err := s.cached(context.Background(), key, cachePolicy{ttl: closesTTL(year, now), grace: closesGrace}, func(ctx context.Context) (any, error) {
			return s.fetchFrom(ctx, a, year, now)
		}); err != nil {
			s.log.Warn("closes refresh failed", "ticker", a.Ticker, "year", year, "err", err)
		}
	}()
}

// fetchFrom asks the price source for everything from the start of year to
// today and returns that year. The source only honours windows that end
// today — one ending in the past comes back cut to the last twelve months —
// so every year that arrives is stored and cached too: one call fills an
// asset's history from that year on.
func (s *Service) fetchFrom(ctx context.Context, a tokens.Asset, year int, now time.Time) (*ClosesView, error) {
	bars, err := s.prices.History(ctx, a.Ticker, priceClass(a), time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC), now)
	if err != nil {
		return nil, err
	}
	byYear := map[int]*ClosesView{}
	for _, b := range bars { // oldest first
		y, err := strconv.Atoi(b.Date[:4])
		if err != nil || y < year || y > now.Year() {
			continue
		}
		v := byYear[y]
		if v == nil {
			v = &ClosesView{Ticker: a.Ticker, Year: y}
			byYear[y] = v
		}
		v.Dates = append(v.Dates, b.Date)
		v.Closes = append(v.Closes, b.Close)
	}
	for y, v := range byYear {
		if y != year {
			s.cache.set(closesKey(a.Ticker, y), v, closesTTL(y, now), closesGrace)
		}
		// Indexed histories already have durable storage. Do not overwrite
		// untagged Nasdaq rows or reuse them after opting into another feed.
		if s.store == nil || s.cfg.HistoricalPricesURL != "" {
			continue
		}
		rows := make([]store.Close, len(v.Dates))
		for i := range v.Dates {
			rows[i] = store.Close{Day: v.Dates[i], Close: v.Closes[i]}
		}
		// Stored in full only once the year has ended and its first trading
		// days are there: a year cut short by the source's reach is not.
		complete := y < now.Year() && v.Dates[0] <= fmt.Sprintf("%d-01-07", y)
		if err := s.store.SaveCloses(ctx, a.Ticker, y, rows, complete); err != nil {
			s.log.Warn("closes not stored", "ticker", a.Ticker, "year", y, "err", err)
		}
	}
	if v := byYear[year]; v != nil {
		return v, nil
	}
	return &ClosesView{Ticker: a.Ticker, Year: year, Dates: []string{}, Closes: []float64{}}, nil
}

// storedYear loads a year from the database: only a year stored in full when
// complete is set, otherwise whatever is there. Nil when there is nothing
// usable, or no database.
func (s *Service) storedYear(ctx context.Context, ticker string, year int, complete bool) *ClosesView {
	if s.store == nil || s.cfg.HistoricalPricesURL != "" {
		return nil
	}
	rows, full, err := s.store.LoadCloses(ctx, ticker, year)
	if err != nil {
		s.log.Warn("stored closes unreadable", "ticker", ticker, "year", year, "err", err)
		return nil
	}
	if len(rows) == 0 || (complete && !full) {
		return nil
	}
	v := &ClosesView{Ticker: ticker, Year: year, Dates: make([]string, len(rows)), Closes: make([]float64, len(rows))}
	for i, r := range rows {
		v.Dates[i], v.Closes[i] = r.Day, r.Close
	}
	return v
}

// WarmCloses keeps every asset's price history ready so an asset page never
// waits on the price source. It asks for each asset's oldest year first,
// which brings back everything to today in one call, then refreshes the
// current year on an interval shorter than its TTL.
func (s *Service) WarmCloses(ctx context.Context) {
	s.warmCloses(ctx, tokens.All())
}

func (s *Service) warmCloses(ctx context.Context, assets []tokens.Asset) {
	pause := func() bool {
		t := time.NewTimer(closesWarmPace)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		}
	}
	warm := func(years func(current int) []int) {
		current := s.now().UTC().Year()
		for _, a := range assets {
			if a.Kind == "Currency" {
				continue
			}
			for _, year := range years(current) {
				if _, err := s.AssetCloses(ctx, a.Ticker, year); err != nil && ctx.Err() == nil {
					s.log.Warn("closes warmup failed", "ticker", a.Ticker, "year", year, "err", err)
				}
			}
			if !pause() {
				return
			}
		}
	}
	warm(func(current int) []int {
		years := make([]int, 0, maxCloseYears+1)
		for y := current - maxCloseYears; y <= current; y++ {
			years = append(years, y)
		}
		return years
	})
	s.log.Info("closes warmup complete")
	tick := time.NewTicker(closesWarmEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			// Expire the current year so the pass refreshes it; the expired
			// copy keeps answering visitors until the new one lands.
			current := s.now().UTC().Year()
			for _, a := range assets {
				if a.Kind == "Currency" {
					continue
				}
				s.cache.expire(closesKey(a.Ticker, current))
			}
			warm(func(current int) []int { return []int{current} })
		}
	}
}
