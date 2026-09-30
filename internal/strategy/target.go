package strategy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/brighter-di/backend/internal/tokens"
)

// GateError is a target that must not reach the portfolio engine: incomplete, stale, or
// naming an asset we cannot trade. The reason is shown to the user verbatim.
type GateError struct {
	Reason string
}

func (e *GateError) Error() string { return e.Reason }

func gate(format string, a ...any) error { return &GateError{Reason: fmt.Sprintf(format, a...)} }

// completenessTolerance bounds how far the backtest engine's weights may sum from 100%
// before we refuse to normalise them. The backtest engine reports weights to three
// decimals, so an equal three-way split arrives as 99.9% and a seven-way as
// 100.1%; a larger gap is a real problem that must not be scaled away.
const completenessTolerance = 0.02

// ToBasisPoints converts fractional weights into basis points that sum to
// exactly 10000 using largest-remainder rounding, ties broken by ticker so the
// result is deterministic. Zero lines are dropped.
func ToBasisPoints(weights map[string]float64) (map[string]int, error) {
	var sum float64
	for t, w := range weights {
		if w < 0 || math.IsNaN(w) || math.IsInf(w, 0) {
			return nil, gate("The strategy returned an invalid weight for %s (%v).", t, w)
		}
		sum += w
	}
	if math.Abs(sum-1) > completenessTolerance {
		return nil, gate("Target is incomplete: weights sum to %.2f%%, not 100%%.", sum*100)
	}

	type line struct {
		ticker string
		floor  int
		frac   float64
	}
	lines := make([]line, 0, len(weights))
	total := 0
	for t, w := range weights {
		scaled := w / sum * 10000
		f := int(math.Floor(scaled))
		lines = append(lines, line{t, f, scaled - float64(f)})
		total += f
	}
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].frac != lines[j].frac {
			return lines[i].frac > lines[j].frac
		}
		return lines[i].ticker < lines[j].ticker
	})
	for i := 0; total < 10000; i, total = i+1, total+1 {
		lines[i%len(lines)].floor++
	}
	out := map[string]int{}
	for _, l := range lines {
		if l.floor > 0 {
			out[l.ticker] = l.floor
		}
	}
	return out, nil
}

// ChangeHash identifies an allocation by its weights alone. The data cutoff is
// excluded on purpose: the same weights on a new date are the same target, and
// including the date would republish a monthly strategy every day.
func ChangeHash(bps map[string]int) string {
	ts := sortedTickers(bps)
	var b strings.Builder
	for _, t := range ts {
		fmt.Fprintf(&b, "%s:%d;", t, bps[t])
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}

// CheckHoldable rejects an allocation naming any ticker the template cannot
// hold. The backtest engine returning an asset nobody approved is a failure, not a gap
// to fill.
func (t *Template) CheckHoldable(bps map[string]int) error {
	allowed := map[string]bool{}
	for _, h := range t.Holdable() {
		allowed[h] = true
	}
	var bad []string
	for ticker := range bps {
		if !allowed[ticker] {
			bad = append(bad, ticker)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return gate("The strategy allocated to %s, which is outside its universe. Rejected — not dropped or substituted.", strings.Join(bad, ", "))
	}
	return nil
}

// AllocationLine is one asset of a portfolio engine allocation.
type AllocationLine struct {
	Ticker  string
	AssetID string // CAIP-19
	Bps     int
}

// Weight renders basis points as the portfolio engine's percent string: 3333 → "33.33",
// 5000 → "50".
func (l AllocationLine) Weight() string {
	s := fmt.Sprintf("%d.%02d", l.Bps/100, l.Bps%100)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// AllocationFor maps a basis-point target onto one chain's Ondo tokens. Every
// line must have a token on that chain — an unmapped asset is never dropped
// or swapped for something else; the whole allocation is rejected.
func AllocationFor(bps map[string]int, chainID int64) ([]AllocationLine, error) {
	chain, ok := tokens.ChainByID(chainID)
	if !ok {
		return nil, gate("Chain %d is not supported.", chainID)
	}
	lines := make([]AllocationLine, 0, len(bps))
	var missing []string
	total := 0
	for _, t := range sortedTickers(bps) {
		id, ok := tokens.AssetID(t, chainID)
		if !ok {
			missing = append(missing, t)
			continue
		}
		lines = append(lines, AllocationLine{Ticker: t, AssetID: id, Bps: bps[t]})
		total += bps[t]
	}
	if len(missing) > 0 {
		return nil, gate("No Ondo token on %s for %s. This target cannot be executed there.", chain.Name, strings.Join(missing, ", "))
	}
	if total != 10000 {
		return nil, gate("Allocation sums to %d bps, not 10000.", total)
	}
	return lines, nil
}

/* ── Freshness ──────────────────────────────────────────────────────── */

// TradingDaysBetween counts weekdays in (from, to]. There is deliberately no
// holiday table — a hardcoded one rots. On a holiday the backtest engine's cutoff simply
// does not advance, and the scheduler's "no new close" gate handles it.
func TradingDaysBetween(from, to time.Time) int {
	from = dateOnly(from)
	to = dateOnly(to)
	n := 0
	for d := from.AddDate(0, 0, 1); !d.After(to); d = d.AddDate(0, 0, 1) {
		if wd := d.Weekday(); wd != time.Saturday && wd != time.Sunday {
			n++
		}
	}
	return n
}

// Freshness is how old a target's data is, and whether it may be traded on.
type Freshness struct {
	AgeTradingDays int
	Fresh          bool
	Reason         string
}

func CheckFreshness(cutoff string, dataLagDays int, now time.Time, maxAge, maxLag int) Freshness {
	c, err := time.Parse("2006-01-02", cutoff)
	if err != nil {
		return Freshness{Reason: "Target has no valid data cutoff."}
	}
	f := Freshness{AgeTradingDays: TradingDaysBetween(c, now)}
	switch {
	case f.AgeTradingDays > maxAge:
		f.Reason = fmt.Sprintf("Market data is stale: cutoff %s is %d trading days old (limit %d).", cutoff, f.AgeTradingDays, maxAge)
	case dataLagDays > maxLag:
		f.Reason = fmt.Sprintf("The latest allocation is %d days behind the last market day (limit %d).", dataLagDays, maxLag)
	default:
		f.Fresh = true
	}
	return f
}

func dateOnly(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func sortedTickers(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for t := range m {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
