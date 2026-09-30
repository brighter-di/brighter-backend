package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/brighter-di/backend/internal/store"
	"github.com/brighter-di/backend/internal/testkit"
)

const owner = "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func day(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

func insertStrategy(t *testing.T, st *store.Store, templateID, key string, chain int64) *store.Strategy {
	t.Helper()
	s := &store.Strategy{
		TemplateID: templateID, ParamsKey: key, Params: map[string]float64{"lookback": 126}, ChainID: chain,
		PortfolioEngineStrategyID: fmt.Sprintf("g-%s-%s-%d", templateID, key, chain),
		Allocation:                map[string]int{"BIL": 10000}, AllocationHash: "h1", Cutoff: day("2026-09-09"), Version: 1,
	}
	if err := st.InsertStrategy(context.Background(), s); err != nil {
		t.Fatalf("insert strategy: %v", err)
	}
	return s
}

func TestMigrateIsIdempotent(t *testing.T) {
	st := testkit.Store(t)
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestStrategies(t *testing.T) {
	ctx := context.Background()
	st := testkit.Store(t)
	s := insertStrategy(t, st, "mag7-rank", "count=3,lookback=126", 56)

	got, err := st.GetStrategy(ctx, "mag7-rank", "count=3,lookback=126", 56)
	if err != nil || got.ID != s.ID || got.Allocation["BIL"] != 10000 || got.Cutoff.Format("2006-01-02") != "2026-09-09" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if _, err := st.GetStrategyByPortfolioEngineID(ctx, s.PortfolioEngineStrategyID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetStrategy(ctx, "mag7-rank", "other", 56); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v", err)
	}

	dup := *s
	dup.PortfolioEngineStrategyID = "another"
	if err := st.InsertStrategy(ctx, &dup); err == nil {
		t.Fatal("a second strategy for the same template, parameters and chain was accepted")
	}

	if err := st.UpdateStrategyAllocation(ctx, s.ID, map[string]int{"AAPL": 10000}, "h2", day("2026-09-10"), 2); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetStrategy(ctx, "mag7-rank", "count=3,lookback=126", 56)
	if got.Version != 2 || got.AllocationHash != "h2" || got.Allocation["AAPL"] != 10000 {
		t.Fatalf("after update: %+v", got)
	}
	if err := st.UpdateStrategyAllocation(ctx, 999999, nil, "", day("2026-09-10"), 2); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestCombos(t *testing.T) {
	ctx := context.Background()
	st := testkit.Store(t)
	insertStrategy(t, st, "mag7-rank", "a", 56)
	insertStrategy(t, st, "mag7-rank", "a", 1)
	insertStrategy(t, st, "inv-vol", "b", 56)

	combos, err := st.ListCombos(ctx)
	if err != nil || len(combos) != 2 {
		t.Fatalf("combos %+v, %v", combos, err)
	}
	strategies, err := st.StrategiesForCombo(ctx, "mag7-rank", "a")
	if err != nil || len(strategies) != 2 || strategies[0].ChainID != 1 || strategies[1].ChainID != 56 {
		t.Fatalf("strategies %+v, %v", strategies, err)
	}
}

func TestPortfolios(t *testing.T) {
	ctx := context.Background()
	st := testkit.Store(t)
	s := insertStrategy(t, st, "mag7-rank", "a", 56)

	for range 2 { // a replayed confirm must not duplicate the row
		if err := st.InsertPortfolio(ctx, "pf-1", owner, s.ID, 56, testkit.DepositAddress); err != nil {
			t.Fatal(err)
		}
	}
	p, err := st.FindPortfolio(ctx, owner, s.ID)
	if err != nil || p.PortfolioID != "pf-1" || p.Strategy.PortfolioEngineStrategyID != s.PortfolioEngineStrategyID {
		t.Fatalf("got %+v, %v", p, err)
	}
	if list, _ := st.ListPortfolios(ctx, owner, false); len(list) != 1 {
		t.Fatalf("%d portfolios", len(list))
	}

	if err := st.MarkPortfolioDropped(ctx, "pf-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetPortfolio(ctx, "pf-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a dropped portfolio is still served: %v", err)
	}
	if list, _ := st.ListPortfolios(ctx, owner, false); len(list) != 0 {
		t.Fatalf("%d portfolios after drop", len(list))
	}
	// Once the old one is dropped the owner can enroll in the strategy again.
	if err := st.InsertPortfolio(ctx, "pf-2", owner, s.ID, 56, testkit.DepositAddress); err != nil {
		t.Fatal(err)
	}
	if p, err := st.FindPortfolio(ctx, owner, s.ID); err != nil || p.PortfolioID != "pf-2" {
		t.Fatalf("got %+v, %v", p, err)
	}
}

func TestEvaluationsAndRetention(t *testing.T) {
	ctx := context.Background()
	st := testkit.Store(t)
	insert := func(status, trigger, cutoff string) {
		e := &store.Evaluation{TemplateID: "mag7-rank", ParamsKey: "k", Trigger: trigger, Status: status, Reason: status}
		if cutoff != "" {
			d := day(cutoff)
			e.Cutoff = &d
		}
		if err := st.InsertEvaluation(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	insert("published", "create", "2026-09-01")
	insert("held", "schedule", "2026-09-02")
	insert("error", "schedule", "")
	insert("unchanged", "schedule", "2026-09-03")
	insert("held", "manual", "2026-09-04")

	last, err := st.LastAcceptedCutoff(ctx, "mag7-rank", "k")
	if err != nil || last == nil || last.Format("2006-01-02") != "2026-09-03" {
		t.Fatalf("last accepted %v, %v", last, err)
	}
	attempts, settled, err := st.EvaluationsSince(ctx, "mag7-rank", "k", time.Now().Add(-time.Hour))
	if err != nil || attempts != 3 || !settled {
		t.Fatalf("attempts %d settled %v err %v", attempts, settled, err)
	}

	// With every row past retention, published rows and the newest accepted
	// one survive; the rest go.
	n, err := st.PruneEvaluations(ctx, time.Now().Add(time.Hour))
	if err != nil || n != 3 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	left, _ := st.RecentEvaluations(ctx, 10)
	statuses := map[string]int{}
	for _, e := range left {
		statuses[e.Status]++
	}
	if len(left) != 2 || statuses["published"] != 1 || statuses["unchanged"] != 1 {
		t.Fatalf("left %v", statuses)
	}
	if last, _ := st.LastAcceptedCutoff(ctx, "mag7-rank", "k"); last.Format("2006-01-02") != "2026-09-03" {
		t.Fatalf("retention moved the last accepted cutoff to %v", last)
	}
	if n, _ := st.PruneEvaluations(ctx, time.Now().Add(time.Hour)); n != 0 {
		t.Fatalf("second prune deleted %d", n)
	}
}

func TestTryLockIsExclusive(t *testing.T) {
	ctx := context.Background()
	st := testkit.Store(t)
	err := st.WithLock(ctx, "test-lock", func(ctx context.Context) error {
		ran, err := st.TryLock(ctx, "test-lock", func(context.Context) error {
			t.Error("ran while another session held the lock")
			return nil
		})
		if err != nil || ran {
			t.Fatalf("ran %v, %v", ran, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if ran, err := st.TryLock(ctx, "test-lock", func(context.Context) error { return nil }); err != nil || !ran {
		t.Fatalf("lock not released: ran %v, %v", ran, err)
	}
}

func TestCloses(t *testing.T) {
	ctx := context.Background()
	st := testkit.Store(t)

	// The current year: stored, but not yet in full.
	if err := st.SaveCloses(ctx, "MSFT", 2026, []store.Close{{Day: "2026-01-02", Close: 100}, {Day: "2026-01-05", Close: 101}}, false); err != nil {
		t.Fatal(err)
	}
	got, complete, err := st.LoadCloses(ctx, "MSFT", 2026)
	if err != nil || complete || len(got) != 2 || got[1] != (store.Close{Day: "2026-01-05", Close: 101}) {
		t.Fatalf("got %v complete %v, %v", got, complete, err)
	}

	// A later fetch replaces a day's value and can mark the year complete.
	if err := st.SaveCloses(ctx, "MSFT", 2026, []store.Close{{Day: "2026-01-05", Close: 102}, {Day: "2026-01-06", Close: 103}}, true); err != nil {
		t.Fatal(err)
	}
	got, complete, _ = st.LoadCloses(ctx, "MSFT", 2026)
	if !complete || len(got) != 3 || got[1].Close != 102 {
		t.Fatalf("after update %v complete %v", got, complete)
	}

	// Years and tickers do not bleed into each other.
	if err := st.SaveCloses(ctx, "MSFT", 2025, []store.Close{{Day: "2025-12-31", Close: 99}}, true); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := st.LoadCloses(ctx, "MSFT", 2026); len(got) != 3 {
		t.Fatalf("2026 picked up another year: %v", got)
	}
	if got, complete, _ := st.LoadCloses(ctx, "AAPL", 2026); len(got) != 0 || complete {
		t.Fatalf("AAPL has nothing stored: %v %v", got, complete)
	}
	if err := st.SaveCloses(ctx, "MSFT", 2026, []store.Close{{Day: "not a day", Close: 1}}, false); err == nil {
		t.Fatal("a malformed day was stored")
	}
}
