package service

import (
	"context"
	"testing"
)

func TestActivityShowsEachPublishedChangeAgainstTheOneBefore(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, true)
	pf := h.invest(t, mag7())

	// The allocation the portfolio started on is not activity.
	v, err := h.svc.Activity(ctx, pf.PortfolioID, 0)
	if err != nil || len(v.Changes) != 0 {
		t.Fatalf("before any change: %+v, %v", v, err)
	}

	combos, err := h.svc.Combos(ctx)
	if err != nil || len(combos) != 1 {
		t.Fatalf("combos %+v, %v", combos, err)
	}
	h.backtestEngine.Set(map[string]float64{"AAPL": 0.5, "BIL": 0.5}, testNow)
	if r := h.svc.Reevaluate(ctx, combos[0], "manual", 6); r.Status != "published" {
		t.Fatalf("reevaluate %+v", r)
	}

	v, err = h.svc.Activity(ctx, pf.PortfolioID, 0)
	if err != nil || len(v.Changes) != 1 {
		t.Fatalf("after a change: %+v, %v", v, err)
	}
	c := v.Changes[0]
	if c.Cutoff != "2026-09-10" || len(c.Lines) != 4 || c.Lines[0].Ticker != "BIL" || c.Lines[0].OndoSymbol != "BILon" || c.Lines[0].Icon == "" {
		t.Fatalf("change %+v", c)
	}
	want := map[string][2]int{"BIL": {0, 5000}, "NVDA": {3340, 0}, "MSFT": {3330, 0}, "AAPL": {3330, 5000}}
	for i, l := range c.Lines {
		if w := want[l.Ticker]; w != [2]int{l.FromBps, l.ToBps} {
			t.Errorf("line %d %s: %d → %d, want %d → %d", i, l.Ticker, l.FromBps, l.ToBps, w[0], w[1])
		}
	}

	// A second change, so "just the latest" is distinguishable from "all".
	h.backtestEngine.Set(map[string]float64{"AAPL": 1}, testNow)
	if r := h.svc.Reevaluate(ctx, combos[0], "manual", 6); r.Status != "published" {
		t.Fatalf("second reevaluate %+v", r)
	}

	// Weights drift daily, so the default is the allocation in force and
	// nothing else; the history is there for a caller that asks.
	v, err = h.svc.Activity(ctx, pf.PortfolioID, 0)
	if err != nil || len(v.Changes) != 1 {
		t.Fatalf("default: %+v, %v", v, err)
	}
	latest := v.Changes[0]

	all, err := h.svc.Activity(ctx, pf.PortfolioID, 10)
	if err != nil || len(all.Changes) != 2 {
		t.Fatalf("limit=10: %+v, %v", all, err)
	}
	// Newest first, so the default is the head of the full list.
	if !all.Changes[0].At.Equal(latest.At) {
		t.Fatalf("the default is not the newest: %v vs %v", all.Changes[0].At, latest.At)
	}
	if !all.Changes[0].At.After(all.Changes[1].At) {
		t.Fatalf("not newest first: %v then %v", all.Changes[0].At, all.Changes[1].At)
	}
	// A limit beyond the cap is the cap, not an error.
	if capped, err := h.svc.Activity(ctx, pf.PortfolioID, 1000); err != nil || len(capped.Changes) != 2 {
		t.Fatalf("limit=1000: %+v, %v", capped, err)
	}

	_, err = h.svc.Activity(ctx, "pf-missing", 0)
	wantCode(t, err, "PORTFOLIO_NOT_FOUND")
}
