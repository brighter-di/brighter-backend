// Package strategy holds the preset templates, their parameters, and the
// maths that turns a backtest engine allocation into something the portfolio engine can hold.
//
// The first five templates are ported one-for-one from the backtest-engine-prototype
// PoC, where every tree was validated against the live backtest engine API; the rest
// reuse the same node shapes and are validated the same way before they ship.
// Keep the node shapes byte-compatible with that file: the backtest engine's schema is
// EDN encoded as JSON, so keyword names keep their kebab-case and trailing
// `?`, and numeric fields are strings.
package strategy

import (
	"crypto/rand"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/brighter-di/backend/internal/tokens"
)

// ParamSpec is one tunable knob. Values live on a fixed grid (min..max by
// step) so the set of distinct strategies is finite and every value a user
// can pick has a meaning the template author intended. No free-form rules.
type ParamSpec struct {
	Key     string  `json:"key"`
	Label   string  `json:"label"`
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
	Step    float64 `json:"step"`
	Default float64 `json:"default"`
	Unit    string  `json:"unit"`
	Hint    string  `json:"hint"`
}

type Params map[string]float64

type Template struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Thesis   string   `json:"thesis"`
	Universe []string `json:"universe"`
	// Where the rule goes when it steps aside. Empty for templates that are
	// always fully invested in their universe.
	Fallback  string      `json:"fallback"`
	Benchmark string      `json:"benchmark"`
	Cadence   string      `json:"cadence"`
	Params    []ParamSpec `json:"params"`
	// A retired template takes no new investment and is not listed for new
	// investors; portfolios already on one keep running.
	Retired bool `json:"retired"`

	build       func(Params) node
	publication *Publication
	Author      string               `json:"author"`
	PublishedAt string               `json:"publishedAt,omitempty"`
	Rules       *CustomStrategy      `json:"rules,omitempty"`
	Bindings    []PublishedParameter `json:"bindings,omitempty"`
}

// The backtest engine indicator function names.
const (
	fnRSI       = "relative-strength-index"
	fnCumReturn = "cumulative-return"
	fnSMAPrice  = "moving-average-price"
	fnPrice     = "current-price"
	fnStdReturn = "standard-deviation-return"
	// Studio reads these too. The backtest engine's own market-cap weighting and MACD are
	// left out: it refuses the first and errors on the second.
	fnEMAPrice    = "exponential-moving-average-price"
	fnMAReturn    = "moving-average-return"
	fnMaxDrawdown = "max-drawdown"
	fnStdPrice    = "standard-deviation-price"
	fnPPO         = "percentage-price-oscillator"
	fnPPOSignal   = "percentage-price-oscillator-signal"
)

var (
	Mag7 = []string{"AAPL", "MSFT", "AMZN", "GOOGL", "META", "NVDA", "TSLA"}

	// Thirteen US blue chips: Mag7 plus six leaders outside tech.
	Leaders = append(slices.Clone(Mag7), "AVGO", "JPM", "LLY", "COST", "V", "WMT")

	// One ETF per major asset class, with T-bills competing as cash.
	AssetClasses = []string{"SPY", "IWM", "EFA", "EEM", "TLT", "GLD", "BIL"}

	// Risk-parity sleeves: stocks, long and intermediate Treasuries, gold,
	// commodities.
	AllWeather = []string{"SPY", "TLT", "IEF", "GLD", "DBC"}

	// The Treasury reference every step-aside template falls back to. On EVM
	// it maps directly to BILon — no proxy instrument, no stablecoin
	// substitution.
	Treasury  = "BIL"
	Benchmark = "SPY"
)

var Templates = []*Template{
	{
		ID:        "mag7-rank",
		Name:      "Mag7 Momentum Rank",
		Thesis:    "Hold the strongest Mag7 names by trailing total return, rebalanced monthly.",
		Universe:  Mag7,
		Fallback:  Treasury,
		Benchmark: Benchmark,
		Cadence:   "monthly",
		Params: []ParamSpec{
			{Key: "lookback", Label: "Lookback", Min: 21, Max: 252, Step: 7, Default: 126, Unit: "d", Hint: "Trading days of total return used to rank."},
			{Key: "count", Label: "Selection count", Min: 1, Max: 7, Step: 1, Default: 3, Hint: "How many top-ranked names to hold, equally weighted."},
		},
		build: func(p Params) node {
			return root("Mag7 Momentum Rank", "monthly",
				wtEqual(filter("top", int(p["count"]), fnCumReturn, int(p["lookback"]), assets(Mag7...)...)))
		},
	},
	{
		ID:        "rsi-riskoff",
		Name:      "RSI Risk-Off Switch",
		Thesis:    "Equal-weight Mag7 until the benchmark is overbought, then step entirely to Treasury.",
		Universe:  Mag7,
		Fallback:  Treasury,
		Benchmark: Benchmark,
		Cadence:   "daily",
		Params: []ParamSpec{
			{Key: "lookback", Label: "RSI lookback", Min: 2, Max: 30, Step: 1, Default: 14, Unit: "d", Hint: "RSI window on SPY."},
			{Key: "threshold", Label: "Risk-off threshold", Min: 50, Max: 90, Step: 1, Default: 70, Hint: "Above this RSI, move 100% to T-bills."},
		},
		build: func(p Params) node {
			return root("RSI Risk-Off Switch", "daily",
				ifElse(
					operand{fn: fnRSI, val: Benchmark, window: int(p["lookback"])}, "gt", fixed(int(p["threshold"])),
					[]node{wtEqual(asset(Treasury))},
					[]node{wtEqual(assets(Mag7...)...)},
				))
		},
	},
	{
		ID:        "dual-momentum",
		Name:      "Dual Momentum Core",
		Thesis:    "Pick the stronger of QQQ and SPY, but only if it beats the Treasury reference.",
		Universe:  []string{"QQQ", "SPY"},
		Fallback:  Treasury,
		Benchmark: Benchmark,
		Cadence:   "monthly",
		Retired:   true,
		Params: []ParamSpec{
			{Key: "lookback", Label: "Lookback", Min: 21, Max: 252, Step: 7, Default: 126, Unit: "d", Hint: "Window for both the relative and absolute momentum test."},
		},
		build: func(p Params) node {
			lb := int(p["lookback"])
			ret := func(t string) operand { return operand{fn: fnCumReturn, val: t, window: lb} }
			return root("Dual Momentum Core", "monthly",
				ifElse(ret("QQQ"), "gt", ret("SPY"),
					[]node{ifElse(ret("QQQ"), "gt", ret(Treasury),
						[]node{wtEqual(asset("QQQ"))}, []node{wtEqual(asset(Treasury))})},
					[]node{ifElse(ret("SPY"), "gt", ret(Treasury),
						[]node{wtEqual(asset("SPY"))}, []node{wtEqual(asset(Treasury))})},
				))
		},
	},
	{
		ID:        "inv-vol",
		Name:      "Inverse Volatility Mag7",
		Thesis:    "Hold all seven, weighted by the reciprocal of trailing realised volatility.",
		Universe:  Mag7,
		Fallback:  Treasury,
		Benchmark: Benchmark,
		Cadence:   "monthly",
		Params: []ParamSpec{
			{Key: "lookback", Label: "Volatility window", Min: 21, Max: 252, Step: 7, Default: 63, Unit: "d", Hint: "Annualised realised volatility window."},
		},
		build: func(p Params) node {
			return root("Inverse Volatility Mag7", "monthly", wtInverseVol(int(p["lookback"]), assets(Mag7...)...))
		},
	},
	{
		ID:        "trend-filter",
		Name:      "Trend Filter Mag7",
		Thesis:    "Each name holds its 1/7 sleeve only while above its moving average; the rest sits in Treasury.",
		Universe:  Mag7,
		Fallback:  Treasury,
		Benchmark: Benchmark,
		Cadence:   "weekly",
		Params: []ParamSpec{
			{Key: "lookback", Label: "Moving average", Min: 20, Max: 250, Step: 10, Default: 100, Unit: "d", Hint: "Simple moving average of close, per asset."},
		},
		build: func(p Params) node {
			sleeves := make([]node, 0, len(Mag7))
			for _, t := range Mag7 {
				sleeves = append(sleeves, group(t+" sleeve",
					ifElse(operand{fn: fnPrice, val: t}, "gt", operand{fn: fnSMAPrice, val: t, window: int(p["lookback"])},
						[]node{asset(t)}, []node{asset(Treasury)})))
			}
			return root("Trend Filter Mag7", "weekly", wtEqual(sleeves...))
		},
	},
	{
		ID:        "golden-cross",
		Name:      "Golden Cross Nasdaq",
		Thesis:    "Invests in the Nasdaq-100 when its 50-day moving average is above its 200-day moving average. When the 50-day average falls below the 200-day average, it shifts to short-term U.S. Treasuries. Brighter checks the strategy daily and updates its target allocation when the signals change.",
		Universe:  []string{"QQQ"},
		Fallback:  Treasury,
		Benchmark: Benchmark,
		Cadence:   "daily",
		Params: []ParamSpec{
			{Key: "fast", Label: "Fast average", Min: 10, Max: 100, Step: 5, Default: 50, Unit: "d", Hint: "Days in the fast moving average of QQQ."},
			{Key: "slow", Label: "Slow average", Min: 120, Max: 300, Step: 10, Default: 200, Unit: "d", Hint: "Days in the slow moving average of QQQ."},
		},
		build: func(p Params) node {
			return root("Golden Cross Nasdaq", "daily",
				ifElse(
					operand{fn: fnSMAPrice, val: "QQQ", window: int(p["fast"])}, "gt", operand{fn: fnSMAPrice, val: "QQQ", window: int(p["slow"])},
					[]node{wtEqual(asset("QQQ"))},
					[]node{wtEqual(asset(Treasury))},
				))
		},
	},
	{
		ID:        "global-rotation",
		Name:      "Global Rotation",
		Thesis:    "Hold the strongest asset classes by trailing return: US, small-cap, international and emerging stocks, Treasuries, gold or cash.",
		Universe:  AssetClasses,
		Fallback:  Treasury,
		Benchmark: Benchmark,
		Cadence:   "monthly",
		Retired:   true,
		Params: []ParamSpec{
			{Key: "lookback", Label: "Lookback", Min: 21, Max: 252, Step: 7, Default: 126, Unit: "d", Hint: "Trading days of total return used to rank."},
			{Key: "count", Label: "Selection count", Min: 1, Max: 3, Step: 1, Default: 2, Hint: "How many top-ranked asset classes to hold, equally weighted."},
		},
		build: func(p Params) node {
			return root("Global Rotation", "monthly",
				wtEqual(filter("top", int(p["count"]), fnCumReturn, int(p["lookback"]), assets(AssetClasses...)...)))
		},
	},
	{
		ID:        "all-weather",
		Name:      "All-Weather Risk Parity",
		Thesis:    "Spread risk evenly across stocks, Treasuries, gold and commodities by weighting each against its volatility.",
		Universe:  AllWeather,
		Benchmark: Benchmark,
		Cadence:   "monthly",
		Retired:   true,
		Params: []ParamSpec{
			{Key: "lookback", Label: "Volatility window", Min: 21, Max: 252, Step: 7, Default: 63, Unit: "d", Hint: "Trading days of volatility used to set each weight."},
		},
		build: func(p Params) node {
			return root("All-Weather Risk Parity", "monthly", wtInverseVol(int(p["lookback"]), assets(AllWeather...)...))
		},
	},
	{
		ID:        "low-vol",
		Name:      "Low Volatility Leaders",
		Thesis:    "Hold the calmest of thirteen US blue chips, measured by trailing volatility, rebalanced monthly.",
		Universe:  Leaders,
		Benchmark: Benchmark,
		Cadence:   "monthly",
		Params: []ParamSpec{
			{Key: "lookback", Label: "Volatility window", Min: 21, Max: 252, Step: 7, Default: 63, Unit: "d", Hint: "Trading days of daily-return volatility used to rank."},
			{Key: "count", Label: "Selection count", Min: 2, Max: 8, Step: 1, Default: 5, Hint: "How many of the calmest names to hold, equally weighted."},
		},
		build: func(p Params) node {
			return root("Low Volatility Leaders", "monthly",
				wtEqual(filter("bottom", int(p["count"]), fnStdReturn, int(p["lookback"]), assets(Leaders...)...)))
		},
	},
	{
		ID:        "oversold-rebound",
		Name:      "Mag 7 Mean Reversion",
		Thesis:    "Invests equally in the two Magnificent 7 stocks that have been the most oversold recently, aiming to capture a potential rebound. It does this while the broader market is in an uptrend and shifts to short-term U.S. Treasuries otherwise. Brighter checks the strategy daily and updates its target allocation when the signals change.",
		Universe:  Mag7,
		Fallback:  Treasury,
		Benchmark: Benchmark,
		Cadence:   "daily",
		Params: []ParamSpec{
			{Key: "lookback", Label: "RSI window", Min: 2, Max: 21, Step: 1, Default: 10, Unit: "d", Hint: "RSI window used to find the most oversold names."},
			{Key: "count", Label: "Selection count", Min: 1, Max: 4, Step: 1, Default: 2, Hint: "How many of the most oversold names to hold, equally weighted."},
		},
		build: func(p Params) node {
			return root("Mag 7 Mean Reversion", "daily",
				ifElse(
					operand{fn: fnPrice, val: Benchmark}, "gt", operand{fn: fnSMAPrice, val: Benchmark, window: 200},
					[]node{wtEqual(filter("bottom", int(p["count"]), fnRSI, int(p["lookback"]), assets(Mag7...)...))},
					[]node{wtEqual(asset(Treasury))},
				))
		},
	},

	// The next three are popular public strategies adapted to what Ondo lists:
	// they read signals from any ticker but hold only the funds below. The
	// defaults reproduce the originals' thresholds. All three are retired:
	// The backtest engine's backtest trades at the close that made the signal, Brighter
	// the next day, and for fast 3× switchers that lag roughly halves returns.
	{
		ID:        "semis-switch",
		Name:      "Semis Signal Switch",
		Thesis:    "Hold 3× semiconductors when bonds lead high-beta stocks, stepping to T-bills when chips or volatility look stretched.",
		Universe:  []string{"SOXL"},
		Fallback:  Treasury,
		Benchmark: Benchmark,
		Cadence:   "daily",
		Retired:   true,
		Params: []ParamSpec{
			{Key: "window", Label: "RSI window", Min: 5, Max: 20, Step: 1, Default: 10, Unit: "d", Hint: "RSI window for every signal."},
			{Key: "oversold", Label: "Oversold level", Min: 15, Max: 40, Step: 1, Default: 30, Hint: "Below this RSI, SOXL is bought on the dip."},
		},
		build: func(p Params) node {
			rsi := func(t string) operand { return operand{fn: fnRSI, val: t, window: int(p["window"])} }
			dip := func() node {
				return ifElse(rsi("SOXL"), "lt", fixed(int(p["oversold"])), []node{asset("SOXL")}, []node{asset(Treasury)})
			}
			return root("Semis Signal Switch", "daily",
				ifElse(rsi("BND"), "gt", rsi("SPHB"),
					[]node{cascade([]branch{
						{rsi("SOXX"), "gt", fixed(80), []node{asset(Treasury)}},
						{rsi("UVXY"), "gt", fixed(74), []node{ifElse(rsi("UVXY"), "gt", fixed(84), []node{asset("SOXL")}, []node{dip()})}},
					}, asset("SOXL"))},
					[]node{dip()},
				))
		},
	},
	{
		ID:        "tech-trend-switch",
		Name:      "Tech vs Trend Switcher",
		Thesis:    "Hedge when markets are overbought, buy sharp dips in 3× funds, and otherwise let tech's strength against a trend-following fund pick long or short.",
		Universe:  []string{"TQQQ", "SOXL", "QQQ", "SQQQ", "TLT"},
		Fallback:  Treasury,
		Benchmark: Benchmark,
		Cadence:   "daily",
		Retired:   true,
		Params: []ParamSpec{
			{Key: "overbought", Label: "Overbought level", Min: 70, Max: 90, Step: 1, Default: 79, Hint: "10-day RSI above which a market gauge triggers the SQQQ hedge."},
			{Key: "oversold", Label: "Oversold level", Min: 15, Max: 40, Step: 1, Default: 30, Hint: "10-day RSI below which a 3× fund is bought on the dip."},
		},
		build: func(p Params) node {
			rsi := func(t string) operand { return operand{fn: fnRSI, val: t, window: 10} }
			hi, lo := int(p["overbought"]), int(p["oversold"])
			var checks []branch
			// Each gauge keeps the original's offset from the base level.
			for _, g := range []struct {
				ticker string
				offset int
			}{{"QQQE", 0}, {"VTV", 0}, {"VOX", 0}, {"TECL", 0}, {"VOOG", 0}, {"VOOV", 0}, {"XLP", -4}, {"TQQQ", 0}, {"XLY", 1}, {"FAS", 1}, {"SPY", 1}} {
				checks = append(checks, branch{rsi(g.ticker), "gt", fixed(hi + g.offset), []node{asset("SQQQ")}})
			}
			checks = append(checks,
				branch{rsi("TQQQ"), "lt", fixed(lo), []node{asset("TQQQ")}},
				branch{rsi("SOXL"), "lt", fixed(lo), []node{asset("SOXL")}},
				branch{rsi("SPXL"), "lt", fixed(lo), []node{asset("TQQQ")}},
				branch{rsi("LABU"), "lt", fixed(lo - 5), []node{asset(Treasury)}},
			)
			return root("Tech vs Trend Switcher", "daily",
				cascade(checks, ifElse(rsi("XLK"), "gt", rsi("KMLM"),
					[]node{wtEqual(filter("bottom", 2, fnRSI, 10, assets("TQQQ", "SOXL", "QQQ")...))},
					[]node{wtEqual(filter("top", 1, fnRSI, 10, assets("SQQQ", "TLT")...))},
				)))
		},
	},
	{
		ID:        "frontrunner",
		Name:      "Leveraged Frontrunner",
		Thesis:    "T-bills when markets are overbought, 3× Nasdaq after sharp drops, 3× tech in bull markets and inverse funds as the hedge in bear markets.",
		Universe:  []string{"TQQQ", "SOXL", "QQQ", "SQQQ", "SOXS", "PSQ"},
		Fallback:  Treasury,
		Benchmark: Benchmark,
		Cadence:   "daily",
		Retired:   true,
		Params: []ParamSpec{
			{Key: "overbought", Label: "Overbought level", Min: 70, Max: 90, Step: 1, Default: 79, Hint: "10-day RSI above which the rule steps to T-bills."},
			{Key: "oversold", Label: "Oversold level", Min: 15, Max: 40, Step: 1, Default: 30, Hint: "10-day RSI below which TQQQ is bought on the dip."},
		},
		build: func(p Params) node {
			rsi := func(t string, w int) operand { return operand{fn: fnRSI, val: t, window: w} }
			price := func(t string) operand { return operand{fn: fnPrice, val: t} }
			sma := func(t string, w int) operand { return operand{fn: fnSMAPrice, val: t, window: w} }
			long := func() node { return wtEqual(assets("SOXL", "TQQQ")...) }
			// The Nasdaq hedge both bear-market halves fall back to.
			hedge := func() node {
				return ifElse(price("TQQQ"), "gt", sma("TQQQ", 20),
					[]node{ifElse(rsi("PSQ", 10), "lt", fixed(35), []node{asset("PSQ")},
						[]node{ifElse(rsi("AGG", 20), "gt", rsi("SH", 60), []node{asset("TQQQ")}, []node{asset("PSQ")})})},
					[]node{ifElse(rsi("IEF", 10), "gt", rsi("PSQ", 20), []node{asset("PSQ")}, []node{asset("SQQQ")})})
			}
			bull := ifElse(rsi("XLK", 10), "gt", rsi("KMLM", 10),
				[]node{long()},
				[]node{ifElse(price("KMLM"), "lt", sma("KMLM", 20), []node{long()}, []node{wtEqual(assets("SOXS", "SQQQ")...)})})
			bear := wtEqual(
				group("Bond signal", ifElse(rsi("TLT", 20), "gt", rsi("PSQ", 20), []node{asset("QQQ")}, []node{hedge()})),
				group("Crash guard", ifElse(operand{fn: fnCumReturn, val: "QQQ", window: 60}, "lt", fixed(-12),
					[]node{ifElse(rsi("BND", 10), "gt", rsi("QQQ", 10), []node{asset("QQQ")}, []node{asset(Treasury)})},
					[]node{hedge()})),
			)
			var checks []branch
			for _, t := range []string{"SPY", "IOO", "TQQQ", "VTV", "XLF"} {
				checks = append(checks, branch{rsi(t, 10), "gt", fixed(int(p["overbought"])), []node{asset(Treasury)}})
			}
			checks = append(checks,
				branch{rsi("TQQQ", 10), "lt", fixed(int(p["oversold"])), []node{asset("TQQQ")}},
				branch{rsi("SPY", 10), "lt", fixed(int(p["oversold"])), []node{asset("TQQQ")}},
				branch{price("SPY"), "gt", sma("SPY", 200), []node{bull}},
			)
			return root("Leveraged Frontrunner", "daily", cascade(checks, bear))
		},
	},
}

func ByID(id string) (*Template, bool) {
	for _, t := range Templates {
		if t.ID == id {
			return t, true
		}
	}
	return nil, false
}

// Holdable is every ticker the template can ever hold: its universe plus the
// fallback, if it has one. A backtest engine allocation naming anything else is
// rejected.
func (t *Template) Holdable() []string {
	out := slices.Clone(t.Universe)
	if t.Fallback == "" || slices.Contains(out, t.Fallback) {
		return out
	}
	return append(out, t.Fallback)
}

func (t *Template) Defaults() Params {
	p := Params{}
	for _, s := range t.Params {
		p[s.Key] = s.Default
	}
	return p
}

// Coerce snaps raw values onto the template's grid and clamps them to range.
// Missing or unparseable values take the default. Unknown keys are ignored.
func (t *Template) Coerce(get func(key string) (float64, bool)) Params {
	p := Params{}
	for _, s := range t.Params {
		v, ok := get(s.Key)
		if !ok || math.IsNaN(v) || math.IsInf(v, 0) {
			p[s.Key] = s.Default
			continue
		}
		snapped := math.Round(v/s.Step) * s.Step
		if t.publication != nil {
			snapped = s.Default + math.Round((v-s.Default)/s.Step)*s.Step
			snapped = roundParameter(snapped)
			if v == s.Default {
				snapped = s.Default
			}
		}
		p[s.Key] = min(s.Max, max(s.Min, snapped))
	}
	return p
}

// CoerceStrings is Coerce over string inputs (query parameters).
func (t *Template) CoerceStrings(get func(key string) string) Params {
	return t.Coerce(func(k string) (float64, bool) {
		s := strings.TrimSpace(get(k))
		if s == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil
	})
}

// Key is the canonical identity of a parameter set: "count=3,lookback=126"
// in spec order. Two requests with the same key are the same strategy.
func (t *Template) Key(p Params) string {
	parts := make([]string, 0, len(t.Params))
	for _, s := range t.Params {
		parts = append(parts, s.Key+"="+strconv.FormatFloat(p[s.Key], 'f', -1, 64))
	}
	return strings.Join(parts, ",")
}

// Label is a short human description of a parameter set: "126d · 3".
func (t *Template) Label(p Params) string {
	parts := make([]string, 0, len(t.Params))
	for _, s := range t.Params {
		parts = append(parts, strconv.FormatFloat(p[s.Key], 'f', -1, 64)+s.Unit)
	}
	return strings.Join(parts, " · ")
}

// Tree builds the backtest engine strategy tree for a (coerced) parameter set.
func (t *Template) Tree(p Params) map[string]any { tree, _ := t.TreeChecked(p); return tree }
func (t *Template) TreeChecked(p Params) (map[string]any, error) {
	if t.publication == nil {
		return t.build(p), nil
	}
	custom, err := t.publication.Materialize(p)
	if err != nil {
		return nil, err
	}
	compiled, err := CompileCustom(custom)
	if err != nil {
		return nil, err
	}
	return compiled.Tree, nil
}

/* ── Node builders ──────────────────────────────────────────────────── */

type node = map[string]any

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func asset(t string) node {
	return node{"id": newID(), "step": "asset", "ticker": t, "name": tokens.DisplayName(t)}
}

func assets(ts ...string) []node {
	out := make([]node, len(ts))
	for i, t := range ts {
		out[i] = asset(t)
	}
	return out
}

func wtEqual(children ...node) node {
	return node{"id": newID(), "step": "wt-cash-equal", "children": children}
}

func wtInverseVol(window int, children ...node) node {
	return node{"id": newID(), "step": "wt-inverse-vol", "window-days": strconv.Itoa(window), "children": children}
}

// wtSpecified splits the capital by a percentage per child. The backtest engine carries
// the share on the child itself, as a fraction of 100.
func wtSpecified(percents []int, children ...node) node {
	for i, c := range children {
		c["weight"] = node{"num": strconv.Itoa(percents[i]), "den": 100}
	}
	return node{"id": newID(), "step": "wt-cash-specified", "children": children}
}

// filter keeps the n children ranked highest ("top") or lowest ("bottom") by
// fn over the window.
func filter(selectFn string, n int, fn string, window int, children ...node) node {
	return node{
		"id": newID(), "step": "filter",
		"select?": true, "select-fn": selectFn, "select-n": strconv.Itoa(n),
		"sort-by-fn": fn, "sort-by-window-days": strconv.Itoa(window),
		"children": children,
	}
}

func group(name string, children ...node) node {
	return node{"id": newID(), "step": "group", "name": name, "collapsed?": false, "children": children}
}

// operand is one side of a condition: an indicator over a ticker, or a fixed
// number (fixedVal set).
type operand struct {
	fn     string
	val    string
	window int // 0 = the indicator takes no window
	// Named windows, for the oscillators that take more than one
	// (short-window, long-window, smooth-window).
	params   map[string]int
	fixedVal *string // The backtest engine's numeric fields are strings
}

func fixed(v int) operand {
	s := strconv.Itoa(v)
	return operand{fixedVal: &s}
}

// ifElse builds the nested if/if-child shape, which the PoC's gate confirmed
// is what the backtest engine accepts.
func ifElse(lhs operand, comparator string, rhs operand, then, otherwise []node) node {
	cond := node{
		"id": newID(), "step": "if-child", "is-else-condition?": false,
		"lhs-fn": lhs.fn, "lhs-val": lhs.val, "comparator": comparator,
		"children": then,
	}
	if lhs.window > 0 {
		cond["lhs-window-days"] = strconv.Itoa(lhs.window)
	}
	if len(lhs.params) > 0 {
		cond["lhs-fn-params"] = lhs.params
	}
	if rhs.fixedVal != nil {
		cond["rhs-fixed-value?"] = true
		cond["rhs-val"] = *rhs.fixedVal
	} else {
		cond["rhs-fixed-value?"] = false
		cond["rhs-fn"] = rhs.fn
		cond["rhs-val"] = rhs.val
		if rhs.window > 0 {
			cond["rhs-window-days"] = strconv.Itoa(rhs.window)
		}
		if len(rhs.params) > 0 {
			cond["rhs-fn-params"] = rhs.params
		}
	}
	return node{
		"id": newID(), "step": "if",
		"children": []node{
			cond,
			{"id": newID(), "step": "if-child", "is-else-condition?": true, "children": otherwise},
		},
	}
}

// branch is one test in a cascade: when lhs compares true against rhs, hold
// then.
type branch struct {
	lhs        operand
	comparator string
	rhs        operand
	then       []node
}

// cascade chains branches as if / else-if / … / else: the first test that
// holds decides, and otherwise is held when none does.
func cascade(branches []branch, otherwise node) node {
	out := otherwise
	for i := len(branches) - 1; i >= 0; i-- {
		b := branches[i]
		out = ifElse(b.lhs, b.comparator, b.rhs, b.then, []node{out})
	}
	return out
}

func root(name, rebalance string, children ...node) node {
	return node{
		"id": newID(), "step": "root", "name": name,
		"description": "Brighter strategy template. Generated payload — not a saved strategyTree.",
		"rebalance":   rebalance,
		"children":    children,
	}
}

// rootCorridor rebalances on drift instead of a calendar: the backtest engine reads no
// calendar plus a corridor width as "whenever a weight strays this far".
func rootCorridor(name string, corridor float64, children ...node) node {
	r := root(name, "none", children...)
	r["rebalance-corridor-width"] = corridor
	return r
}

// TargetWindowYears leaves a year of history beyond the longest active
// indicator window, with the existing two-year minimum for built-in rules.
func TargetWindowYears(tree map[string]any) int {
	longest := 0
	var walk func(any)
	walk = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			for key, child := range v {
				if strings.HasSuffix(key, "window-days") || key == "short-window" || key == "long-window" || key == "smooth-window" {
					days := 0
					switch n := child.(type) {
					case int:
						days = n
					case string:
						days, _ = strconv.Atoi(n)
					case float64:
						days = int(n)
					}
					longest = max(longest, days)
				}
				walk(child)
			}
		case map[string]int:
			for key, days := range v {
				if key == "short-window" || key == "long-window" || key == "smooth-window" {
					longest = max(longest, days)
				}
			}
		case []node:
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(tree)
	return max(2, (longest+251)/252+1)
}
