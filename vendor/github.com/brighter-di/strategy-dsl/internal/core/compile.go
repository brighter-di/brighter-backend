package core

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Strategies compile to the backtest engine compatible score trees without upstream calls.
// Conditional lowering, candidate ranking and weighted selection preserve the
// backend's initial semantics; asset availability is an injected caller policy.

// ValidationError is a strategy the caller has to fix. Reason is safe to show.
type ValidationError struct{ Reason string }

func (e *ValidationError) Error() string { return e.Reason }

func invalid(format string, a ...any) error {
	return &ValidationError{Reason: fmt.Sprintf(format, a...)}
}

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

// Compiled is a validated custom strategy, ready to backtest.
type Compiled struct {
	Tree      map[string]any `json:"tree"`
	Benchmark string         `json:"benchmark"`
	// Key identifies the rules: equal rules share a key whatever their block
	// ids, display names or token spelling (NVDA vs NVDAon).
	Key string `json:"key"`
}

func (compiler *Compiler) Compile(c Strategy) (*Compiled, error) {
	if compiler.requireName && strings.TrimSpace(c.Name) == "" {
		return nil, invalid("Give the strategy a name.")
	}
	if !slices.Contains(customRebalances, c.Rebalance) {
		return nil, invalid("Rebalance must be daily, weekly, monthly, quarterly, yearly or threshold.")
	}
	if c.Rebalance == "threshold" && (c.Corridor < customMinCorridor || c.Corridor > customMaxCorridor) {
		return nil, invalid("A threshold rebalance drifts between %d%% and %d%%.", customMinCorridor, customMaxCorridor)
	}
	benchmark, ok := compiler.resolveTicker(c.Benchmark)
	if !ok {
		return nil, invalid("Benchmark %s is not an allowed research ticker.", quoteTicker(c.Benchmark))
	}
	b := &customBuilder{compiler: compiler}
	children, canon, err := b.list(c.Children, 0, "The strategy")
	if err != nil {
		return nil, err
	}
	top := children
	if len(canon) != 1 || (canon[0].Kind != "weight" && canon[0].Kind != "condition") {
		// Several top-level blocks share the capital equally.
		top = []node{wtEqual(children...)}
	}

	corridor := 0
	if c.Rebalance == "threshold" {
		corridor = c.Corridor
	}
	identity, err := identityJSON(struct {
		Rebalance string `json:"rebalance"`
		Corridor  int    `json:"corridor"`
		Benchmark string `json:"benchmark"`
		Blocks    []Node `json:"blocks"`
	}{c.Rebalance, corridor, benchmark, canon})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(identity)

	// The backtest engine knows no "threshold" calendar: it rebalances on drift when the
	// calendar is "none" and a corridor is set.
	tree := root(customName(c.Name), c.Rebalance, top...)
	if c.Rebalance == "threshold" {
		tree = rootCorridor(customName(c.Name), float64(c.Corridor)/100, top...)
	}
	// Boolean expressions can repeat a branch in the serialized score. Bound
	// the expanded score as well as the user's source blocks.
	if !withinExpandedLimit(tree, customMaxExpandedBlocks) {
		return nil, invalid("Combined conditions produce too many backtest blocks. Split the strategy into simpler rules.")
	}
	return &Compiled{
		Tree:      tree,
		Benchmark: benchmark,
		Key:       hex.EncodeToString(sum[:12]),
	}, nil
}

type customBuilder struct {
	blocks   int
	compiler *Compiler
}

// list compiles a block list and returns the backtest engine nodes with a canonical
// copy that carries only what each block means.
func (b *customBuilder) list(in []Node, depth int, owner string) ([]node, []Node, error) {
	if len(in) == 0 {
		return nil, nil, invalid("%s needs at least one block.", owner)
	}
	if len(in) > customMaxList {
		return nil, nil, invalid("%s has more than %d blocks in one list.", owner, customMaxList)
	}
	out := make([]node, 0, len(in))
	canon := make([]Node, 0, len(in))
	for _, n := range in {
		tree, c, err := b.node(n, depth)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, tree)
		canon = append(canon, c)
	}
	return out, canon, nil
}

func (b *customBuilder) node(n Node, depth int) (node, Node, error) {
	fail := func(err error) (node, Node, error) { return nil, Node{}, err }
	if depth >= customMaxDepth {
		return fail(invalid("Blocks are nested more than %d levels deep.", customMaxDepth))
	}
	if b.blocks++; b.blocks > customMaxBlocks {
		return fail(invalid("The strategy has more than %d blocks.", customMaxBlocks))
	}

	switch n.Kind {
	case "asset":
		t, ok := b.compiler.resolveTicker(n.Ticker)
		if !ok {
			return fail(invalid("%s is not an allowed research ticker.", quoteTicker(n.Ticker)))
		}
		return asset(t), Node{Kind: "asset", Ticker: t}, nil

	case "group":
		// A name to read by; the rules inside are what counts, so the name
		// stays out of the canonical copy.
		children, kids, err := b.list(n.Children, depth+1, "GROUP")
		if err != nil {
			return fail(err)
		}
		inner := children
		if len(kids) != 1 || (kids[0].Kind != "weight" && kids[0].Kind != "condition") {
			inner = []node{wtEqual(children...)}
		}
		return group(customName(n.Name), inner...), Node{Kind: "group", Children: kids}, nil

	case "weight":
		canon := Node{Kind: "weight", Method: n.Method}
		switch n.Method {
		case "equal":
		case "inverse-volatility":
			if err := checkDays(n.Lookback, "WEIGHT"); err != nil {
				return fail(err)
			}
			canon.Lookback = n.Lookback
		case "specified":
		default:
			return fail(invalid("WEIGHT must be equal, by inverse volatility, or by set percentages."))
		}
		children, kids, err := b.list(n.Children, depth+1, "WEIGHT")
		if err != nil {
			return fail(err)
		}
		canon.Children = kids
		if n.Method == "inverse-volatility" && len(n.Children) == 1 && n.Children[0].Kind == "filter" {
			if !assetSelection(n.Children[0]) {
				return fail(invalid("Inverse-volatility weighting of a selection needs asset candidates directly under SELECT."))
			}
			return selectedWeight(kids[0], canon, children[0]["children"].([]node))
		}
		switch n.Method {
		case "equal":
			return wtEqual(children...), canon, nil
		case "inverse-volatility":
			return wtInverseVol(n.Lookback, children...), canon, nil
		}
		percents := make([]int, len(n.Children))
		total := 0
		for i, kid := range n.Children {
			if kid.Percent < customMinPercent || kid.Percent > customMaxPercent {
				return fail(invalid("Every share of a set-percentage WEIGHT is between 1%% and 100%%."))
			}
			percents[i] = kid.Percent
			total += kid.Percent
			canon.Children[i].Percent = kid.Percent
		}
		if total != customMaxPercent {
			return fail(invalid("The shares of a set-percentage WEIGHT add up to %d%%, not 100%%.", total))
		}
		return wtSpecified(percents, children...), canon, nil

	case "filter":
		spec, ok := measures[n.Metric]
		if !ok || !spec.windowed {
			return fail(invalid("SELECT ranks by an indicator that reads one lookback: return, RSI, drawdown, volatility or a moving average."))
		}
		if n.Direction != "top" && n.Direction != "bottom" {
			return fail(invalid("SELECT keeps the top or bottom blocks."))
		}
		if n.Count < 1 || n.Count > customMaxCount {
			return fail(invalid("SELECT must keep between 1 and %d blocks.", customMaxCount))
		}
		if err := checkDays(n.Lookback, "SELECT"); err != nil {
			return fail(err)
		}
		children, kids, err := b.list(n.Children, depth+1, "SELECT")
		if err != nil {
			return fail(err)
		}
		canon := Node{Kind: "filter", Direction: n.Direction, Count: n.Count, Metric: n.Metric, Lookback: n.Lookback, Children: kids}
		// A sole weight describes allocation after selection, rather than a
		// basket to rank. Equal weighting uses a native filter; other methods
		// use explicit ranked choices. Canonicalize the allocation too, so
		// old basket backtests cannot be served from cache.
		if len(n.Children) == 1 && n.Children[0].Kind == "weight" {
			canon.Children = kids[0].Children
			candidates := children[0]["children"].([]node)
			if kids[0].Method != "equal" {
				return selectedWeight(canon, kids[0], candidates)
			}
			return wtEqual(filter(n.Direction, n.Count, spec.fn, n.Lookback, candidates...)),
				Node{Kind: "weight", Method: "equal", Children: []Node{canon}}, nil
		}
		return filter(n.Direction, n.Count, spec.fn, n.Lookback, children...), canon, nil

	case "condition":
		var build predicateBuilder
		var canon Node
		var err error
		if n.Predicate != nil {
			parts := 0
			var predicate Node
			build, predicate, err = b.compiler.compilePredicate(*n.Predicate, &parts)
			canon = Node{Kind: "condition", Predicate: &predicate}
		} else {
			build, canon, err = b.compiler.compileComparison(n)
			canon.Kind = "condition"
		}
		if err != nil {
			return fail(err)
		}
		then, thenCanon, err := b.list(n.ThenChildren, depth+1, "The IF branch")
		if err != nil {
			return fail(err)
		}
		otherwise, elseCanon, err := b.list(n.ElseChildren, depth+1, "The ELSE branch")
		if err != nil {
			return fail(err)
		}
		canon.ThenChildren, canon.ElseChildren = thenCanon, elseCanon
		return build(then, otherwise), canon, nil
	}
	return fail(invalid("Unknown block type %s.", quoteTicker(n.Kind)))
}

type predicateBuilder func(then, otherwise []node) node

func (compiler *Compiler) compileComparison(n Node) (predicateBuilder, Node, error) {
	if !slices.Contains(customComparators, n.Comparator) {
		return nil, Node{}, invalid("IF compares with >, >=, =, <= or <.")
	}
	lhs, left, err := compiler.measure(n.Metric, n.Ticker, n.Lookback, n.ShortWindow, n.LongWindow, n.SmoothWindow)
	if err != nil {
		return nil, Node{}, err
	}
	canon := Node{
		Metric: left.Metric, Ticker: left.Ticker, Lookback: left.Lookback,
		ShortWindow: left.ShortWindow, LongWindow: left.LongWindow, SmoothWindow: left.SmoothWindow,
		Comparator: n.Comparator, CompareMode: n.CompareMode,
	}
	var rhs operand
	switch n.CompareMode {
	case "value":
		if math.IsNaN(n.Value) || math.IsInf(n.Value, 0) || math.Abs(n.Value) > customMaxValue {
			return nil, Node{}, invalid("IF compares with a number between -1,000,000 and 1,000,000.")
		}
		v := strconv.FormatFloat(n.Value, 'f', -1, 64)
		rhs = operand{fixedVal: &v}
		canon.Value = n.Value
	case "indicator":
		r, right, err := compiler.measure(n.RightMetric, n.RightTicker, n.RightLookback, n.RightShort, n.RightLong, n.RightSmooth)
		if err != nil {
			return nil, Node{}, err
		}
		rhs = r
		canon.RightMetric, canon.RightTicker, canon.RightLookback = right.Metric, right.Ticker, right.Lookback
		canon.RightShort, canon.RightLong, canon.RightSmooth = right.ShortWindow, right.LongWindow, right.SmoothWindow
	default:
		return nil, Node{}, invalid("IF compares with a number or another measure.")
	}
	return func(then, otherwise []node) node { return ifElse(lhs, n.Comparator, rhs, then, otherwise) }, canon, nil
}

func (compiler *Compiler) compilePredicate(n Node, parts *int) (predicateBuilder, Node, error) {
	*parts++
	if *parts > customMaxConditionParts {
		return nil, Node{}, invalid("A combined condition supports at most %d parts.", customMaxConditionParts)
	}
	if n.Type == "comparison" {
		build, canon, err := compiler.compileComparison(n)
		canon.Type = "comparison"
		return build, canon, err
	}
	if (n.Type != "any" && n.Type != "all") || len(n.Conditions) == 0 {
		return nil, Node{}, invalid("ANY/ALL needs at least one valid condition.")
	}
	canon := Node{Type: n.Type}
	builders := make([]predicateBuilder, 0, len(n.Conditions))
	for _, child := range n.Conditions {
		build, canonical, err := compiler.compilePredicate(child, parts)
		if err != nil {
			return nil, Node{}, err
		}
		builders = append(builders, build)
		canon.Conditions = append(canon.Conditions, canonical)
	}
	return func(then, otherwise []node) node {
		out := otherwise
		if n.Type == "all" {
			out = then
		}
		for i := len(builders) - 1; i >= 0; i-- {
			if n.Type == "all" {
				out = []node{builders[i](out, otherwise)}
			} else {
				out = []node{builders[i](then, out)}
			}
		}
		return out[0]
	}, canon, nil
}

func withinExpandedLimit(tree node, remaining int) bool {
	var visit func(node) bool
	visit = func(n node) bool {
		remaining--
		if remaining < 0 {
			return false
		}
		if children, ok := n["children"].([]node); ok {
			for _, child := range children {
				if !visit(child) {
					return false
				}
			}
		}
		return true
	}
	return visit(tree)
}

// measure is one side of an IF: an indicator over a ticker. The canonical copy
// keeps only the windows that measure actually uses.
func (compiler *Compiler) measure(metric, ticker string, lookback, short, long, smooth int) (operand, Node, error) {
	t, ok := compiler.resolveTicker(ticker)
	if !ok {
		return operand{}, Node{}, invalid("%s is not an allowed research ticker.", quoteTicker(ticker))
	}
	spec, ok := measures[metric]
	if !ok {
		return operand{}, Node{}, invalid("IF uses an unknown measure.")
	}
	switch {
	case spec.oscillator:
		if err := checkDays(short, "IF"); err != nil {
			return operand{}, Node{}, err
		}
		if err := checkDays(long, "IF"); err != nil {
			return operand{}, Node{}, err
		}
		if short >= long {
			return operand{}, Node{}, invalid("An oscillator's short window must be shorter than its long one.")
		}
		params := map[string]int{"short-window": short, "long-window": long}
		canon := Node{Metric: metric, Ticker: t, ShortWindow: short, LongWindow: long}
		if spec.smooth {
			if err := checkDays(smooth, "IF"); err != nil {
				return operand{}, Node{}, err
			}
			params["smooth-window"] = smooth
			canon.SmoothWindow = smooth
		}
		return operand{fn: spec.fn, val: t, params: params}, canon, nil
	case spec.windowed:
		if err := checkDays(lookback, "IF"); err != nil {
			return operand{}, Node{}, err
		}
		return operand{fn: spec.fn, val: t, window: lookback}, Node{Metric: metric, Ticker: t, Lookback: lookback}, nil
	}
	return operand{fn: spec.fn, val: t}, Node{Metric: metric, Ticker: t}, nil
}

func checkDays(days int, owner string) error {
	if days < customMinDays || days > customMaxDays {
		return invalid("%s lookback must be between %d and %d days.", owner, customMinDays, customMaxDays)
	}
	return nil
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
