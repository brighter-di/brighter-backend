package service

import (
	"context"
	"strings"

	"github.com/brighter-di/backend/internal/tokens"
)

// Where the market has got to, for anything that reasons about a rule rather
// than draws a chart.
//
// This is closes, not ticks. Brighter has no intraday price anywhere: the
// price client returns daily bars, and an asset page's "price" is the latest
// close, as its own tooltip says. So every figure here is as of a close, and
// AsOf says which one — a caller that reads this as "right now" would be
// reading it wrong, and the field is named to stop that.
//
// It costs nothing upstream: the closes it reads are the ones WarmCloses keeps
// ready, cached 15 minutes for the running year.

// Sessions behind the latest close that each change looks back. Trading days,
// not calendar days, which is what a close series counts in.
const (
	sessionsWeek  = 5
	sessionsMonth = 21
)

// maxSnapshotTickers bounds one request. Each is a cached lookup, but a caller
// asking for the whole registry would still be asking for 452 of them.
const maxSnapshotTickers = 20

// QuoteView is one asset as of the last close. A change is nil when there is
// not enough history behind it to measure, which is never the same as zero.
type QuoteView struct {
	Ticker string  `json:"ticker"`
	Name   string  `json:"name"`
	AsOf   string  `json:"asOf"`
	Close  float64 `json:"close"`
	// Fractional changes: 0.0082 is +0.82%.
	Day   *float64 `json:"day"`
	Week  *float64 `json:"week"`
	Month *float64 `json:"month"`
	YTD   *float64 `json:"ytd"`
}

type MarketView struct {
	// What every figure is as of, repeated once for the whole reading so a
	// caller need not infer it from the rows.
	AsOf   string      `json:"asOf"`
	Quotes []QuoteView `json:"quotes"`
	// Tickers that were asked for and have no price series.
	Unavailable []string `json:"unavailable"`
}

// Market reads where the named assets closed, and how far they have moved.
func (s *Service) Market(ctx context.Context, tickers []string) (*MarketView, error) {
	if len(tickers) == 0 {
		return nil, errf(400, "NO_TICKERS", "name at least one ticker")
	}
	if len(tickers) > maxSnapshotTickers {
		return nil, errf(400, "TOO_MANY_TICKERS", "at most %d tickers at a time", maxSnapshotTickers)
	}
	out := &MarketView{Quotes: []QuoteView{}, Unavailable: []string{}}
	seen := map[string]bool{}
	for _, raw := range tickers {
		ticker := strings.ToUpper(strings.TrimSpace(raw))
		if ticker == "" || seen[ticker] {
			continue
		}
		seen[ticker] = true
		quote, err := s.quote(ctx, ticker)
		if err != nil {
			// An unknown ticker, a currency, or a series that would not load:
			// name it rather than failing the whole reading.
			out.Unavailable = append(out.Unavailable, ticker)
			continue
		}
		out.Quotes = append(out.Quotes, *quote)
		if quote.AsOf > out.AsOf {
			out.AsOf = quote.AsOf
		}
	}
	return out, nil
}

func (s *Service) quote(ctx context.Context, ticker string) (*QuoteView, error) {
	year := s.now().UTC().Year()
	current, err := s.AssetCloses(ctx, ticker, year)
	if err != nil {
		return nil, err
	}
	dates, closes := current.Dates, current.Closes
	// In January a year holds too few sessions to look back a month, so the
	// year before it is read as well. Past years are cached for a day.
	if len(closes) <= sessionsMonth {
		if before, err := s.AssetCloses(ctx, ticker, year-1); err == nil {
			dates = append(append([]string{}, before.Dates...), dates...)
			closes = append(append([]float64{}, before.Closes...), closes...)
		}
	}
	if len(closes) == 0 {
		return nil, errf(422, "NO_PRICE_HISTORY", "%s has no closes yet.", ticker)
	}
	last := len(closes) - 1
	q := &QuoteView{Ticker: current.Ticker, AsOf: dates[last], Close: closes[last]}
	if a, ok := tokens.Lookup(current.Ticker); ok {
		q.Name = a.Name
	}
	q.Day = changeOver(closes, 1)
	q.Week = changeOver(closes, sessionsWeek)
	q.Month = changeOver(closes, sessionsMonth)

	// Year to date is measured from the first close of the running year, which
	// is where the current series starts however much history was prepended.
	if len(current.Closes) > 0 {
		first := current.Closes[0]
		if first > 0 {
			ytd := closes[last]/first - 1
			q.YTD = &ytd
		}
	}
	return q, nil
}

// changeOver is the move from n sessions back to the latest close, or nil when
// the series does not reach that far.
func changeOver(closes []float64, sessions int) *float64 {
	last := len(closes) - 1
	from := last - sessions
	if from < 0 || closes[from] <= 0 {
		return nil
	}
	change := closes[last]/closes[from] - 1
	return &change
}
