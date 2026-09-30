package service

import (
	"context"
	"sync"
)

// SummaryView is one listed strategy's card: its backtest, downsampled, and
// when asked for its current allocation. Either is nil when it could not be
// had, so one slow or failing strategy never blanks the list.
type SummaryView struct {
	ID       string        `json:"id"`
	Backtest *BacktestView `json:"backtest"`
	Target   *TargetView   `json:"target,omitempty"`
}

// summaryWorkers bounds how many strategies are read at once. Answers come
// from the same caches the per-strategy routes use.
const summaryWorkers = 4

// Summaries are the cards for every listed strategy, in catalogue order, so a
// list page makes one request instead of two per strategy.
func (s *Service) Summaries(ctx context.Context, rng string, points int, withTargets bool) ([]SummaryView, error) {
	if _, ok := Ranges[rng]; !ok {
		return nil, errf(400, "BAD_RANGE", "range must be one of 1y, 3y, 5y, 10y")
	}
	templates, err := s.PublicTemplates(ctx)
	if err != nil {
		return nil, err
	}
	listed := make([]TemplateView, 0, len(templates))
	for _, t := range templates {
		if !t.Retired {
			listed = append(listed, t)
		}
	}
	out := make([]SummaryView, len(listed))
	sem := make(chan struct{}, summaryWorkers)
	var wg sync.WaitGroup
	for i, t := range listed {
		out[i].ID = t.ID
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			if bt, err := s.Backtest(ctx, t.Template, t.Defaults, rng); err == nil {
				v := bt.Downsample(points)
				out[i].Backtest = &v
			} else {
				s.log.Debug("summary backtest unavailable", "template", t.ID, "err", err)
			}
			if !withTargets {
				return
			}
			if v, err := s.Target(ctx, t.Template, t.Defaults); err == nil {
				out[i].Target = v
			} else {
				s.log.Debug("summary target unavailable", "template", t.ID, "err", err)
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
