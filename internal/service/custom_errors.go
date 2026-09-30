package service

import (
	"encoding/json"
	"strings"

	"github.com/brighter-di/backend/internal/backtestengine"
	"github.com/brighter-di/backend/internal/strategy"
)

func customBacktestRejection(upstream *backtestengine.Error) error {
	var issues []struct {
		Code string `json:"code"`
		Meta struct {
			Ticker       string `json:"ticker"`
			MinLookback  int    `json:"min_lookback"`
			SeriesLength int    `json:"series_length"`
		} `json:"meta"`
	}
	if json.Unmarshal([]byte(upstream.Detail), &issues) == nil {
		for _, issue := range issues {
			if issue.Code != "insufficient-ticker-data" {
				continue
			}
			ticker := strings.TrimPrefix(issue.Meta.Ticker, "EQUITIES::")
			ticker, _, _ = strings.Cut(ticker, "//")
			if canonical, ok := strategy.ResearchTicker(ticker); ok && issue.Meta.MinLookback > 0 && issue.Meta.SeriesLength >= 0 {
				return errf(422, "INSUFFICIENT_HISTORY", "%s has only %d available price observations; this rule needs %d. Reduce its lookback or remove %s from SELECT.", canonical, issue.Meta.SeriesLength, issue.Meta.MinLookback, canonical)
			}
		}
	}
	return errf(422, "STRATEGY_NOT_BACKTESTABLE", "This combination of blocks can't be backtested. Try simplifying the strategy.")
}
