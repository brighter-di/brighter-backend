package backtestengine

import (
	"encoding/json"
	"testing"
)

func mustParse(t *testing.T, s string) *Backtest {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		t.Fatal(err)
	}
	bt, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return bt
}

func TestEpochDayToISO(t *testing.T) {
	if got := EpochDayToISO(20700); got != "2026-09-04" {
		t.Fatalf("got %s", got)
	}
}

func TestLatestAllocationAnchorsOnNewestDayAcrossTickers(t *testing.T) {
	bt := mustParse(t, `{
		"last_market_day": 20703,
		"dvm_capital": {"0b3c1d2e-aaaa-bbbb-cccc-000000000000": {"20699": 100, "20700": 101}, "SPY": {"20699": 50, "20700": 51}},
		"daily_weights": {
			"AAPL":  {"20699": 0.5, "20700": 0.5},
			"MSFT":  {"20700": 0.5},
			"GOOGL": {"20690": 0.5, "20699": 0.5}
		},
		"last_market_days_holdings": {"AAPL": 328.15}
	}`)
	a, ok := bt.LatestAllocation()
	if !ok {
		t.Fatal("no allocation")
	}
	if a.Date != "2026-09-04" || a.MarketDay != "2026-09-07" || a.LagDays != 3 {
		t.Fatalf("dates: %+v", a)
	}
	// GOOGL's own last day is earlier; on the anchor day it was not held.
	if len(a.Weights) != 2 || a.Weights["AAPL"] != 0.5 || a.Weights["MSFT"] != 0.5 {
		t.Fatalf("weights: %v", a.Weights)
	}
}

func TestCurveRebasesBothLegsAtFirstCommonDay(t *testing.T) {
	bt := mustParse(t, `{
		"dvm_capital": {
			"0b3c1d2e-aaaa-bbbb-cccc-000000000000": {"1": 200, "2": 220, "3": 210},
			"SPY": {"2": 50, "3": 55}
		}
	}`)
	c, err := bt.Curve()
	if err != nil {
		t.Fatal(err)
	}
	if bt.BenchmarkKey != "SPY" || len(c.Days) != 2 || c.Days[0] != 2 {
		t.Fatalf("curve should start at day 2: %+v", c)
	}
	if c.Strategy[0] != 100 || c.Strategy[1] != 100*210.0/220 || *c.Benchmark[0] != 100 || *c.Benchmark[1] != 110 {
		t.Fatalf("rebase wrong: %+v %v %v", c.Strategy, *c.Benchmark[0], *c.Benchmark[1])
	}
}

func TestParseRejectsMissingCapital(t *testing.T) {
	if _, err := Parse(map[string]any{"stats": map[string]any{}}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestParseRejectsBenchmarkWithoutStrategy(t *testing.T) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(`{"dvm_capital":{"SPY":{"1":100,"2":110}}}`), &raw); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(raw); err == nil {
		t.Fatal("benchmark-only response must not become a strategy backtest")
	}
}

func TestParseRejectsAmbiguousStrategies(t *testing.T) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(`{"dvm_capital":{"0b3c1d2e-aaaa-bbbb-cccc-000000000000":{"1":100,"2":110},"1b3c1d2e-aaaa-bbbb-cccc-000000000000":{"1":100,"2":120},"SPY":{"1":100,"2":130}}}`), &raw); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(raw); err == nil {
		t.Fatal("must not pick an arbitrary strategy from multiple capital series")
	}
}

func TestParseUsesRequestedBenchmark(t *testing.T) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(`{"dvm_capital":{"0b3c1d2e-aaaa-bbbb-cccc-000000000000":{"1":100,"2":110},"QQQ":{"1":100,"2":120},"SPY":{"1":100,"2":130}}}`), &raw); err != nil {
		t.Fatal(err)
	}
	bt, err := parse(raw, "SPY")
	if err != nil {
		t.Fatal(err)
	}
	curve, err := bt.Curve()
	if err != nil {
		t.Fatal(err)
	}
	if bt.BenchmarkKey != "SPY" || *curve.Benchmark[1] != 130 {
		t.Fatalf("wrong benchmark: %+v", bt)
	}
	if _, err := parse(raw, "BIL"); err == nil {
		t.Fatal("missing requested benchmark must not use another ticker")
	}
}
