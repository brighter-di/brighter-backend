package prices

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"sort"
	"time"
)

// indexedHistory uses provider closes without dividend adjustment for asset charts.
// Yahoo's provider closes include split adjustments; Tiingo's are as-traded.
// strategy engine consumes the separate adjusted dataset from the same indexer.
func (c *Client) indexedHistory(ctx context.Context, ticker string, from, to time.Time) ([]Bar, error) {
	query := url.Values{"start": {from.Format(time.DateOnly)}, "adjusted": {"false"}}
	var response struct {
		Symbol   string `json:"symbol"`
		Adjusted bool   `json:"adjusted"`
		Prices   []struct {
			Date  string  `json:"date"`
			Close float64 `json:"close"`
		} `json:"prices"`
	}
	if err := c.get(ctx, "/v1/assets/"+url.PathEscape(ticker)+"/prices?"+query.Encode(), &response); err != nil {
		return nil, err
	}
	if response.Symbol != ticker || response.Adjusted {
		return nil, fmt.Errorf("indexer returned wrong symbol or price convention")
	}
	bars := []Bar{}
	seen := map[string]bool{}
	for _, bar := range response.Prices {
		day, err := time.Parse(time.DateOnly, bar.Date)
		if err != nil || seen[bar.Date] || bar.Close <= 0 || math.IsNaN(bar.Close) || math.IsInf(bar.Close, 0) {
			return nil, fmt.Errorf("indexer returned invalid historical observation")
		}
		seen[bar.Date] = true
		if bar.Date < from.Format(time.DateOnly) || bar.Date > to.Format(time.DateOnly) {
			continue
		}
		bars = append(bars, Bar{Date: day.Format(time.DateOnly), Close: bar.Close})
	}
	sort.Slice(bars, func(i, j int) bool { return bars[i].Date < bars[j].Date })
	return bars, nil
}
