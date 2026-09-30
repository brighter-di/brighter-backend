package service

import (
	"context"
	"math"
	"testing"
)

func TestPerformanceTracksProfitAgainstDeposits(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, true)
	pf := h.invest(t, mag7())

	v, err := h.svc.Performance(ctx, pf.PortfolioID)
	if err != nil {
		t.Fatal(err)
	}
	if v.Method != "MWR" || len(v.Points) != 3 {
		t.Fatalf("view %+v", v)
	}
	want := []struct{ deposited, profit float64 }{{100, 0}, {100, 5}, {150, 5}}
	for i, w := range want {
		p := v.Points[i]
		if p.Deposited != w.deposited || math.Abs(p.Profit-w.profit) > 1e-9 {
			t.Fatalf("point %d: %+v, want deposited %v profit %v", i, p, w.deposited, w.profit)
		}
	}
	if r := v.Points[2].Return; r == nil || math.Abs(*r-0.048) > 1e-12 || !v.Points[2].Live {
		t.Fatalf("last point %+v", v.Points[2])
	}

	// Trimmed points keep the deposits accumulated over the whole history.
	if tail := v.Since("2026-09-09").Points; len(tail) != 2 || tail[0].Date != "2026-09-09" || tail[1].Deposited != 150 {
		t.Fatalf("since: %+v", tail)
	}
	if tail := v.Since("2026-09-11").Points; len(tail) != 0 || len(v.Points) != 3 {
		t.Fatalf("since after the last point: %+v", tail)
	}

	// A second read within the minute is served from cache.
	if _, err := h.svc.Performance(ctx, pf.PortfolioID); err != nil {
		t.Fatal(err)
	}
	if n := len(h.portfolioEngine.Requests("GET /portfolios/{id}/performance")); n != 1 {
		t.Fatalf("%d upstream reads, want 1", n)
	}
	_, err = h.svc.Performance(ctx, "pf-missing")
	wantCode(t, err, "PORTFOLIO_NOT_FOUND")
}
