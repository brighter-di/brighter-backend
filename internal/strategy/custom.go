package strategy

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/brighter-di/backend/internal/tokens"
	dsl "github.com/brighter-di/strategy-dsl"
)

// Custom strategies are rule trees composed in Brighter Studio. They are
// compiled by strategy-dsl — the same compiler Studio and the strategy engine
// use — into the backtest engine score this backend sends as an inline backtest.
// Nothing is saved to BacktestEngine. Authenticated publications become immutable
// templates that use the same guarded investment and re-evaluation paths as
// presets.
//
// The backend owns two things the language does not: which symbols are
// tradable (the token registry, injected as the compiler's ticker policy) and
// how a score presents itself to the backtest engine (security names and the root
// description, restored below so a Studio strategy and a preset send the same
// shape).

// CustomError is a strategy the caller has to fix. Reason is safe to show.
type CustomError struct{ Reason string }

func (e *CustomError) Error() string { return e.Reason }

func invalid(format string, a ...any) error { return &CustomError{Reason: fmt.Sprintf(format, a...)} }

// measure is one indicator a rule can read.
type measureSpec struct {
	fn string
	// windowed reads one lookback; oscillator reads short and long windows,
	// and smooth as well when it has a signal line. A measure with neither
	// takes no parameters at all.
	windowed   bool
	oscillator bool
	smooth     bool
}

// CompiledCustom is a validated custom strategy, ready to backtest.
type CompiledCustom struct {
	Tree      map[string]any
	Benchmark string
	// Key identifies the rules: equal rules share a key whatever their block
	// ids, display names or token spelling (NVDA vs NVDAon).
	Key string
}

// Research and portfolio trading resolve symbols from the same contract
// registry. Currency tokens have no stock-price series in BacktestEngine.
func ResearchTicker(t string) (string, bool) {
	if a, ok := tokens.Lookup(t); ok && a.Kind != "Currency" {
		return a.Ticker, true
	}
	return "", false
}

// researchTickers is the ticker policy handed to the compiler: every symbol
// Brighter can price, under both its research spelling and its Ondo token
// spelling, resolving to the research ticker.
func researchTickers() map[string]string {
	out := make(map[string]string, 2*len(tokens.All()))
	for _, a := range tokens.All() {
		if a.Kind == "Currency" {
			continue
		}
		out[a.Ticker] = a.Ticker
		out[a.OndoSymbol] = a.Ticker
	}
	return out
}

// customDescription is what a generated score says about itself. The backtest engine
// never stores these trees; the line exists so a captured payload is
// recognisable.
const customDescription = "Brighter strategy template. Generated payload — not a saved strategyTree."

func CompileCustom(c CustomStrategy) (*CompiledCustom, error) {
	// The transport is generated from one language specification, so the
	// backend's structs and the compiler's carry identical JSON tags.
	raw, err := json.Marshal(c)
	if err != nil {
		// The only values this transport cannot carry are non-finite numbers,
		// which reach it as a comparison value. Keep that a strategy the
		// caller can fix rather than a server error.
		return nil, invalid("IF compares with a number between -1,000,000 and 1,000,000.")
	}
	var strategy dsl.Strategy
	if err := json.Unmarshal(raw, &strategy); err != nil {
		return nil, err
	}
	compiled, err := dsl.New(dsl.Options{Tickers: researchTickers()}).Compile(strategy)
	if err != nil {
		var reason *dsl.ValidationError
		if errors.As(err, &reason) {
			return nil, &CustomError{Reason: reason.Reason}
		}
		var syntax *dsl.SyntaxError
		if errors.As(err, &syntax) {
			return nil, &CustomError{Reason: syntax.Message}
		}
		return nil, err
	}
	present(compiled.Tree, c.Name)
	return &CompiledCustom{Tree: compiled.Tree, Benchmark: compiled.Benchmark, Key: compiled.Key}, nil
}

// present restores what the backtest engine reads but the rules do not decide: each
// holding's security name, and the root's name and description. Rule identity
// excludes all three, so this changes how a score reads, never what it does.
func present(tree map[string]any, name string) {
	tree["name"] = customName(name)
	tree["description"] = customDescription
	var walk func(any)
	walk = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			if ticker, ok := v["ticker"].(string); ok {
				v["name"] = tokens.DisplayName(ticker)
			}
			walk(v["children"])
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
}

func customName(name string) string {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return "Studio strategy"
	}
	if utf8.RuneCountInString(name) > customMaxName {
		return string([]rune(name)[:customMaxName])
	}
	return name
}

// quoteTicker quotes user input for an error message, shortened.
func quoteTicker(s string) string {
	if utf8.RuneCountInString(s) > 16 {
		s = string([]rune(s)[:16]) + "…"
	}
	return strconv.Quote(s)
}
