package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/brighter-di/backend/internal/prices"
	"github.com/brighter-di/backend/internal/strategy"
)

func mag7Template(t *testing.T) *strategy.Template {
	t.Helper()
	tpl, ok := strategy.ByID("mag7-rank")
	if !ok {
		t.Fatal("mag7-rank template missing")
	}
	return tpl
}

func TestBacktest(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, false)
	tpl := mag7Template(t)

	v, err := h.svc.Backtest(ctx, tpl, tpl.Defaults(), "1y")
	if err != nil {
		t.Fatal(err)
	}
	if v.Source != "backtest" || v.ParamsKey != "lookback=126,count=3" || len(v.Series.Dates) != 30 ||
		v.Series.Strategy[0] != 100 || v.Benchmark.Metrics == nil || v.EndDate != "2026-09-09" {
		t.Fatalf("backtest %+v", v)
	}
	down := v.Downsample(10)
	if n := len(down.Series.Dates); n > 11 || down.Series.Dates[n-1] != v.EndDate {
		t.Fatalf("downsampled to %d points ending %s", n, down.Series.Dates[n-1])
	}
	_, err = h.svc.Backtest(ctx, tpl, tpl.Defaults(), "2y")
	wantCode(t, err, "BAD_RANGE")
}

func TestTargetReportsReadinessPerChain(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, false)
	tpl := mag7Template(t)

	v, err := h.svc.Target(ctx, tpl, tpl.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, l := range v.Lines {
		total += l.Bps
	}
	if total != 10000 || len(v.Lines) != 3 || v.Lines[0].Ticker != "NVDA" || !v.Fresh || v.Cutoff != "2026-09-09" {
		t.Fatalf("target %+v", v)
	}
	if len(v.Chains) != 1 || v.Chains[0].ChainID != 56 || !v.Chains[0].Executable {
		t.Fatalf("chains %+v", v.Chains)
	}

	// Stale data is reported, not hidden.
	h.backtestEngine.Set(map[string]float64{"AAPL": 1}, testNow.AddDate(0, 0, -10))
	v, err = h.svc.Target(ctx, tpl, strategy.Params{"count": 1, "lookback": 126})
	if err != nil {
		t.Fatal(err)
	}
	if v.Fresh || !strings.Contains(v.Problem, "stale") || v.Chains[0].Executable {
		t.Fatalf("stale target %+v", v)
	}
}

func TestAssetDetail(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, false)

	roles := func(ticker string) map[string]AssetStrategy {
		a, err := h.svc.Asset(ticker)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]AssetStrategy{}
		for _, s := range a.Strategies {
			out[s.ID] = s
		}
		return out
	}

	a, err := h.svc.Asset("msft")
	if err != nil || a.Ticker != "MSFT" || len(a.Chains) != 1 || a.Chains[0].ChainID != 56 {
		t.Fatalf("asset %+v, %v", a, err)
	}
	msft := roles("MSFT")
	if msft["mag7-rank"].Role != "universe" || msft["mag7-rank"].HeldBps != nil {
		t.Fatalf("MSFT strategies %+v", msft)
	}
	if _, ok := msft["dual-momentum"]; ok {
		t.Fatal("dual momentum cannot hold MSFT")
	}
	if roles("BIL")["mag7-rank"].Role != "fallback" {
		t.Fatal("BIL is where mag7-rank steps aside to")
	}

	// Once today's default target is computed, the page shows its weight.
	tpl := mag7Template(t)
	if _, err := h.svc.Target(ctx, tpl, tpl.Defaults()); err != nil {
		t.Fatal(err)
	}
	if held := roles("MSFT")["mag7-rank"].HeldBps; held == nil || *held != 3330 {
		t.Fatalf("held %v", held)
	}
	// The strategy holding it leads; the rest, with no weight computed, follow.
	a, err = h.svc.Asset("MSFT")
	if err != nil || len(a.Strategies) < 2 || a.Strategies[0].ID != "mag7-rank" || a.Strategies[1].HeldBps != nil {
		t.Fatalf("strategies are not sorted by weight: %+v, %v", a.Strategies, err)
	}
	_, err = h.svc.Asset("FAKE")
	wantCode(t, err, "UNKNOWN_ASSET")
}

func TestAssetClosesServeOneCalendarYear(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, false)
	// Weekdays Dec 29 to Jan 5: the three closes from 2025 come back too, and
	// only 2026 is kept.
	h.nasdaq.Set("MSFT", time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC), 90, 95, 100, 110, 99, 120)

	v, err := h.svc.AssetCloses(ctx, "msft", 2026)
	if err != nil {
		t.Fatal(err)
	}
	if v.Ticker != "MSFT" || v.Year != 2026 || len(v.Dates) != 3 || v.Dates[0] != "2026-01-01" || v.Dates[2] != "2026-01-05" || v.Closes[2] != 120 {
		t.Fatalf("closes %+v", v)
	}
	for _, year := range []int{2027, 2015} {
		_, err := h.svc.AssetCloses(ctx, "MSFT", year)
		wantCode(t, err, "BAD_YEAR")
	}
	_, err = h.svc.AssetCloses(ctx, "FAKE", 2026)
	wantCode(t, err, "UNKNOWN_ASSET")
}

func TestAssetClosesServeTheLastGoodCopyWhenTheSourceFails(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, false)
	h.nasdaq.Set("MSFT", time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC), 100, 120)
	if _, err := h.svc.AssetCloses(ctx, "MSFT", 2026); err != nil {
		t.Fatal(err)
	}

	h.svc.cache.expire("closes|MSFT|2026")
	h.nasdaq.FailNext(100)
	before := h.nasdaq.Calls()
	v, err := h.svc.AssetCloses(ctx, "MSFT", 2026)
	if err != nil || len(v.Closes) != 2 || v.Closes[1] != 120 {
		t.Fatalf("got %+v, %v", v, err)
	}
	// The refresh runs in the background, after the stale copy has answered.
	eventually(t, "an expired entry still tries to refresh", func() bool { return h.nasdaq.Calls() > before })

	// Without a last good copy the failure surfaces.
	_, err = h.svc.AssetCloses(ctx, "AAPL", 2026)
	var pe *prices.Error
	if !errors.As(err, &pe) {
		t.Fatalf("got %v", err)
	}
}

func TestAdmissionIsAskedOnlyForCacheMisses(t *testing.T) {
	h := newHarness(t, false)
	tpl := mag7Template(t)
	denied := errors.New("denied")
	admitted, deny := 0, false
	ctx := WithAdmit(context.Background(), func() error {
		admitted++
		if deny {
			return denied
		}
		return nil
	})

	for range 3 {
		if _, err := h.svc.Backtest(ctx, tpl, tpl.Defaults(), "1y"); err != nil {
			t.Fatal(err)
		}
	}
	if admitted != 1 || h.backtestEngine.Calls() != 1 {
		t.Fatalf("admitted %d, upstream %d; want 1, 1", admitted, h.backtestEngine.Calls())
	}

	deny = true
	if _, err := h.svc.Backtest(ctx, tpl, tpl.Defaults(), "3y"); !errors.Is(err, denied) {
		t.Fatalf("got %v", err)
	}
	if h.backtestEngine.Calls() != 1 {
		t.Fatal("a refused request reached the upstream")
	}
	// Work with no admission check (warmup, the scheduler) is never refused.
	if _, err := h.svc.Backtest(context.Background(), tpl, tpl.Defaults(), "3y"); err != nil {
		t.Fatal(err)
	}
}
