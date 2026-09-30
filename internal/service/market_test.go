package service

import "testing"

func TestChangeOverMeasuresBackFromTheLastClose(t *testing.T) {
	// 10 sessions, each 1% above the one before.
	closes := []float64{100}
	for len(closes) < 10 {
		closes = append(closes, closes[len(closes)-1]*1.01)
	}

	if day := changeOver(closes, 1); day == nil || !near(*day, 0.01) {
		t.Fatalf("one session back: %v", day)
	}
	if week := changeOver(closes, 5); week == nil || !near(*week, 1.01*1.01*1.01*1.01*1.01-1) {
		t.Fatalf("five sessions back: %v", week)
	}
	// Reaching further back than the series goes is unknown, never zero: a
	// caller must be able to tell "flat" from "we cannot say".
	if beyond := changeOver(closes, 10); beyond != nil {
		t.Fatalf("past the start of the series: %v", *beyond)
	}
	if exact := changeOver(closes, 9); exact == nil {
		t.Fatal("the whole series is a measurable span")
	}
	// A zero or negative close cannot be a base.
	if bad := changeOver([]float64{0, 5}, 1); bad != nil {
		t.Fatalf("divided by a zero close: %v", *bad)
	}
	if empty := changeOver(nil, 1); empty != nil {
		t.Fatal("an empty series measured something")
	}
}

func TestMarketRefusesMoreThanItWillRead(t *testing.T) {
	s := &Service{}
	if _, err := s.Market(t.Context(), nil); err == nil {
		t.Fatal("no tickers was accepted")
	}
	many := make([]string, maxSnapshotTickers+1)
	for i := range many {
		many[i] = "SPY"
	}
	if _, err := s.Market(t.Context(), many); err == nil {
		t.Fatalf("more than %d tickers was accepted", maxSnapshotTickers)
	}
}

func near(got, want float64) bool {
	d := got - want
	return d < 1e-9 && d > -1e-9
}
