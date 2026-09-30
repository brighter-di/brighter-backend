package strategy

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/brighter-di/backend/internal/tokens"
)

func sum(m map[string]int) int {
	t := 0
	for _, v := range m {
		t += v
	}
	return t
}

func TestToBasisPointsSumsExactly(t *testing.T) {
	cases := []map[string]float64{
		{"AAPL": 1.0 / 3, "MSFT": 1.0 / 3, "NVDA": 1.0 / 3},
		// The backtest engine reports three decimals: a seven-way split arrives as 100.1%.
		{"AAPL": .143, "MSFT": .143, "AMZN": .143, "GOOGL": .143, "META": .143, "NVDA": .143, "TSLA": .143},
		// ...and a three-way split as 99.9%.
		{"AAPL": .333, "MSFT": .333, "NVDA": .333},
		{"BIL": 1},
	}
	for _, w := range cases {
		bps, err := ToBasisPoints(w)
		if err != nil {
			t.Fatalf("%v: %v", w, err)
		}
		if sum(bps) != 10000 {
			t.Fatalf("%v: sums to %d", w, sum(bps))
		}
	}
}

func TestToBasisPointsDeterministicTieBreak(t *testing.T) {
	bps, _ := ToBasisPoints(map[string]float64{"C": 1.0 / 3, "A": 1.0 / 3, "B": 1.0 / 3})
	if bps["A"] != 3334 || bps["B"] != 3333 || bps["C"] != 3333 {
		t.Fatalf("remainder should go to the alphabetically first ticker: %v", bps)
	}
}

func TestToBasisPointsRejectsIncomplete(t *testing.T) {
	var gate *GateError
	if _, err := ToBasisPoints(map[string]float64{"AAPL": .5, "MSFT": .4}); !errors.As(err, &gate) {
		t.Fatalf("90%% allocation must be rejected, got %v", err)
	}
	if _, err := ToBasisPoints(map[string]float64{"AAPL": 1.2}); !errors.As(err, &gate) {
		t.Fatalf("120%% allocation must be rejected, got %v", err)
	}
	if _, err := ToBasisPoints(map[string]float64{"AAPL": 1.1, "MSFT": -0.1}); !errors.As(err, &gate) {
		t.Fatalf("negative weight must be rejected, got %v", err)
	}
}

func TestChangeHashIgnoresOrderAndIsSensitiveToWeights(t *testing.T) {
	a := ChangeHash(map[string]int{"AAPL": 5000, "MSFT": 5000})
	b := ChangeHash(map[string]int{"MSFT": 5000, "AAPL": 5000})
	c := ChangeHash(map[string]int{"AAPL": 5001, "MSFT": 4999})
	if a != b {
		t.Fatal("hash must not depend on map order")
	}
	if a == c {
		t.Fatal("hash must change when weights change")
	}
}

func TestWeightFormatting(t *testing.T) {
	for bps, want := range map[int]string{3333: "33.33", 5000: "50", 1250: "12.5", 10000: "100", 1: "0.01"} {
		if got := (AllocationLine{Bps: bps}).Weight(); got != want {
			t.Errorf("%d bps: got %q want %q", bps, got, want)
		}
	}
}

func TestTradingDaysBetween(t *testing.T) {
	d := func(s string) time.Time { v, _ := time.Parse("2006-01-02", s); return v }
	cases := []struct {
		from, to string
		want     int
	}{
		{"2026-09-04", "2026-09-04", 0}, // same day
		{"2026-09-04", "2026-09-07", 1}, // Fri → Mon
		{"2026-09-02", "2026-09-07", 3}, // Wed → Mon: Thu, Fri, Mon
		{"2026-09-05", "2026-09-06", 0}, // Sat → Sun
	}
	for _, c := range cases {
		if got := TradingDaysBetween(d(c.from), d(c.to)); got != c.want {
			t.Errorf("%s → %s: got %d want %d", c.from, c.to, got, c.want)
		}
	}
}

func TestCheckFreshness(t *testing.T) {
	now, _ := time.Parse("2006-01-02", "2026-09-10") // Thursday
	if f := CheckFreshness("2026-09-09", 1, now, 3, 4); !f.Fresh {
		t.Fatalf("yesterday's close must be fresh: %+v", f)
	}
	if f := CheckFreshness("2026-09-03", 0, now, 3, 4); f.Fresh {
		t.Fatalf("five trading days old must be stale: %+v", f)
	}
	if f := CheckFreshness("2026-09-09", 6, now, 3, 4); f.Fresh {
		t.Fatalf("over-limit data lag must be stale: %+v", f)
	}
}

func TestCoerceSnapsAndClamps(t *testing.T) {
	tpl, _ := ByID("mag7-rank")
	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	p := tpl.CoerceStrings(get(map[string]string{"lookback": "130", "count": "99"}))
	if p["lookback"] != 133 || p["count"] != 7 {
		t.Fatalf("got %v", p)
	}
	p = tpl.CoerceStrings(get(map[string]string{"lookback": "junk"}))
	if p["lookback"] != 126 || p["count"] != 3 {
		t.Fatalf("defaults expected, got %v", p)
	}
	if k := tpl.Key(p); k != "lookback=126,count=3" {
		t.Fatalf("key %q", k)
	}
}

func TestAllocationForEveryHoldableOnEveryChain(t *testing.T) {
	for _, tpl := range Templates {
		bps := map[string]int{}
		holdable := tpl.Holdable()
		share := 10000 / len(holdable)
		for i, h := range holdable {
			bps[h] = share
			if i == 0 {
				bps[h] += 10000 - share*len(holdable)
			}
		}
		for _, chainID := range []int64{1, 56} {
			lines, err := AllocationFor(bps, chainID)
			if err != nil {
				t.Fatalf("%s on %d: %v", tpl.ID, chainID, err)
			}
			for _, l := range lines {
				if !strings.HasPrefix(l.AssetID, "eip155:") || strings.ToLower(l.AssetID) != l.AssetID {
					t.Fatalf("asset id must be lowercase CAIP-19: %s", l.AssetID)
				}
			}
		}
	}
}

func TestAllocationForRejectsUnmapped(t *testing.T) {
	var gate *GateError
	if _, err := AllocationFor(map[string]int{"XOM": 10000}, 1); !errors.As(err, &gate) {
		t.Fatalf("unmapped asset must be rejected, got %v", err)
	}
}

func TestCheckHoldable(t *testing.T) {
	tpl, _ := ByID("dual-momentum")
	if err := tpl.CheckHoldable(map[string]int{"QQQ": 10000}); err != nil {
		t.Fatal(err)
	}
	if err := tpl.CheckHoldable(map[string]int{"AAPL": 10000}); err == nil {
		t.Fatal("AAPL is outside dual-momentum's universe")
	}
}

func TestTreesAreBacktestEngineShaped(t *testing.T) {
	for _, tpl := range Templates {
		tree := tpl.Tree(tpl.Defaults())
		b, err := json.Marshal(tree)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if tree["step"] != "root" || !strings.Contains(s, `"step":"asset"`) {
			t.Fatalf("%s: not a root tree: %s", tpl.ID, s)
		}
		// Numeric fields are strings in the backtest engine's schema.
		if strings.Contains(s, `"select-n":3`) || strings.Contains(s, `"window-days":63`) {
			t.Fatalf("%s: numeric fields must be strings: %s", tpl.ID, s)
		}
	}
	rsi, _ := ByID("rsi-riskoff")
	s, _ := json.Marshal(rsi.Tree(rsi.Defaults()))
	for _, want := range []string{`"rhs-fixed-value?":true`, `"rhs-val":"70"`, `"lhs-window-days":"14"`, `"is-else-condition?":true`} {
		if !strings.Contains(string(s), want) {
			t.Fatalf("rsi tree missing %s: %s", want, s)
		}
	}
}

func TestAllocationForAllBNBAssetsUsesTheirOwnContracts(t *testing.T) {
	for _, a := range tokens.All() {
		lines, err := AllocationFor(map[string]int{a.Ticker: 10000}, 56)
		if err != nil || len(lines) != 1 {
			t.Fatalf("%s cannot execute on BNB: %v", a.Ticker, err)
		}
		want, _ := tokens.AssetID(a.Ticker, 56)
		if lines[0].AssetID != want || lines[0].Bps != 10000 {
			t.Fatal("wrong executable contract", a.Ticker, lines)
		}
	}
	lines, err := AllocationFor(map[string]int{"AMD": 3334, "PLTR": 3333, "SMCI": 3333}, 56)
	if err != nil || len(lines) != 3 {
		t.Fatal("reported strategy still cannot execute", lines, err)
	}
	if _, err := AllocationFor(map[string]int{"AMD": 10000}, 1); err == nil {
		t.Fatal("BNB-only registry asset approved on Ethereum")
	}
}
