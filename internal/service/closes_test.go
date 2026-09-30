package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/brighter-di/backend/internal/tokens"
)

func date(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

// weekdays counts Monday to Friday in [from, to].
func weekdays(from, to time.Time) int {
	n := 0
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		if wd := d.Weekday(); wd != time.Saturday && wd != time.Sunday {
			n++
		}
	}
	return n
}

// rising is n closes climbing from 100.
func rising(n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = 100 + float64(i)
	}
	return out
}

// eventually polls cond for up to two seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting: %s", what)
}

func TestOneFetchStoresEveryYearToToday(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, true)
	last := date(2026, 9, 10)
	h.nasdaq.Set("MSFT", last, rising(weekdays(date(2024, 1, 1), last))...)

	v, err := h.svc.AssetCloses(ctx, "MSFT", 2024)
	if err != nil || len(v.Dates) != weekdays(date(2024, 1, 1), date(2024, 12, 31)) || v.Dates[0] != "2024-01-01" {
		t.Fatalf("2024: %d days from %v, %v", len(v.Dates), v.Dates[:1], err)
	}
	if h.nasdaq.Calls() != 1 {
		t.Fatalf("%d calls for one year", h.nasdaq.Calls())
	}

	// After a restart, with the source down and the visitor over the rate
	// limit, every finished year still comes from the database.
	h.svc.cache = newTTLCache()
	h.nasdaq.FailNext(100)
	limited := WithAdmit(ctx, func() error { return errors.New("rate limited") })
	for _, year := range []int{2024, 2025} {
		v, err := h.svc.AssetCloses(limited, "MSFT", year)
		if err != nil || len(v.Dates) != weekdays(date(year, 1, 1), date(year, 12, 31)) {
			t.Fatalf("%d from the database: %v", year, err)
		}
	}
	if h.nasdaq.Calls() != 1 {
		t.Fatal("a finished year stored in full went back to the price source")
	}
}

func TestAYearCutShortIsFetchedAgain(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, true)
	last := date(2026, 9, 10)
	// The source's reach ends in March 2023: that year is incomplete.
	h.nasdaq.Set("MSFT", last, rising(weekdays(date(2023, 3, 1), last))...)
	if _, err := h.svc.AssetCloses(ctx, "MSFT", 2023); err != nil {
		t.Fatal(err)
	}

	h.svc.cache = newTTLCache()
	calls := h.nasdaq.Calls()
	if _, err := h.svc.AssetCloses(ctx, "MSFT", 2024); err != nil || h.nasdaq.Calls() != calls {
		t.Fatalf("2024 is whole and should come from the database: %v", err)
	}
	if _, err := h.svc.AssetCloses(ctx, "MSFT", 2023); err != nil || h.nasdaq.Calls() == calls {
		t.Fatalf("2023 is missing its start and must be fetched again: %v", err)
	}
}

func TestExpiredCurrentYearAnswersAtOnceAndRefreshes(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, false)
	h.nasdaq.Set("MSFT", date(2026, 9, 9), 100, 120)
	if _, err := h.svc.AssetCloses(ctx, "MSFT", 2026); err != nil {
		t.Fatal(err)
	}

	// A new close arrives at the source and the cached copy expires. The
	// visitor gets the old copy immediately; the refresh brings the new one.
	h.nasdaq.Set("MSFT", date(2026, 9, 10), 100, 120, 130)
	h.svc.cache.expire("closes|MSFT|2026")
	v, err := h.svc.AssetCloses(ctx, "MSFT", 2026)
	if err != nil || len(v.Closes) != 2 {
		t.Fatalf("the stale copy should answer at once: %+v, %v", v, err)
	}
	eventually(t, "the refreshed year in the cache", func() bool {
		fresh, ok := h.svc.cache.get("closes|MSFT|2026")
		return ok && len(fresh.(*ClosesView).Closes) == 3
	})
	if v, _ := h.svc.AssetCloses(ctx, "MSFT", 2026); len(v.Closes) != 3 {
		t.Fatalf("after the refresh: %+v", v)
	}
}

func TestStoredCurrentYearAnswersAfterARestart(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, true)
	h.nasdaq.Set("MSFT", date(2026, 9, 10), 100, 120)
	if _, err := h.svc.AssetCloses(ctx, "MSFT", 2026); err != nil {
		t.Fatal(err)
	}

	// After a restart the stored copy answers at once, even with the source
	// down, and a refresh still goes to the source.
	h.svc.cache = newTTLCache()
	h.nasdaq.FailNext(100)
	calls := h.nasdaq.Calls()
	v, err := h.svc.AssetCloses(ctx, "MSFT", 2026)
	if err != nil || len(v.Closes) != 2 || v.Closes[1] != 120 {
		t.Fatalf("stored copy %+v, %v", v, err)
	}
	eventually(t, "a background refresh", func() bool { return h.nasdaq.Calls() > calls })
}

func TestWarmClosesFillsEveryYearInOneCallPerAsset(t *testing.T) {
	h := newHarness(t, false)
	last := date(2026, 9, 10)
	for _, ticker := range []string{"MSFT", "SPY"} {
		h.nasdaq.Set(ticker, last, rising(weekdays(date(2017, 1, 2), last))...)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.svc.warmCloses(ctx, []tokens.Asset{mustAsset(t, "MSFT"), mustAsset(t, "SPY")})
	}()
	warmed := func() bool {
		for _, ticker := range []string{"MSFT", "SPY"} {
			for year := 2017; year <= 2026; year++ {
				if _, ok := h.svc.cache.get(closesKey(ticker, year)); !ok {
					return false
				}
			}
		}
		return true
	}
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline) && !warmed(); {
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done
	if !warmed() {
		t.Fatal("not every year was warmed")
	}
}

func mustAsset(t *testing.T, ticker string) tokens.Asset {
	t.Helper()
	a, ok := tokens.Lookup(ticker)
	if !ok {
		t.Fatal("missing registry asset", ticker)
	}
	return a
}
