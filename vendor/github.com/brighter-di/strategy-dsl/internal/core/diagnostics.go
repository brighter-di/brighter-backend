package core

import (
	"fmt"
	"math"
	"slices"
	"strings"
)

// Diagnostics checks readiness without constructing score trees or generating
// UUIDs. Paths identify transport fields; SourceLevel preserves the distinction
// between source errors and incomplete, editable allocation drafts.
func (c *Compiler) Diagnostics(s Strategy) []Diagnostic {
	v := diagnosticValidator{compiler: c}
	if c.requireName && strings.TrimSpace(s.Name) == "" {
		v.add("EMPTY_NAME", "Give the strategy a name.", "name", "error")
	}
	if !slices.Contains(customRebalances, s.Rebalance) {
		v.add("INVALID_REBALANCE", "Rebalance must be daily, weekly, monthly, quarterly, yearly or threshold.", "rebalance", "error")
	}
	if s.Rebalance == "threshold" && (s.Corridor < customMinCorridor || s.Corridor > customMaxCorridor) {
		v.add("INVALID_CORRIDOR", fmt.Sprintf("A threshold rebalance drifts between %d%% and %d%%.", customMinCorridor, customMaxCorridor), "corridor", "error")
	}
	v.ticker(s.Benchmark, []any{"benchmark"})
	size, kinds := v.list(s.Children, 0, []any{"children"}, "The strategy")
	size = boundedSize(1, size)
	if len(kinds) != 1 || (kinds[0] != "weight" && kinds[0] != "condition") {
		size = boundedSize(size, 1)
	}
	if !hasDiagnosticErrors(v.diagnostics) && size > customMaxExpandedBlocks {
		v.issue("EXPANDED_LIMIT", "Combined conditions produce too many backtest blocks. Split the strategy into simpler rules.", []any{"children"}, "error", "")
	}
	return v.diagnostics
}

type diagnosticValidator struct {
	compiler    *Compiler
	diagnostics []Diagnostic
	blocks      int
}

func appendPath(p []any, fields ...any) []any { return append(append([]any(nil), p...), fields...) }
func (v *diagnosticValidator) issue(code, message string, path []any, level, sourceLevel string) {
	if len(v.diagnostics) >= customMaxBlocks {
		return
	}
	v.diagnostics = append(v.diagnostics, Diagnostic{Code: code, Message: message, Path: path, Level: level, SourceLevel: sourceLevel})
}
func (v *diagnosticValidator) add(code, message, field, sourceLevel string) {
	v.issue(code, message, []any{field}, "error", sourceLevel)
}
func hasDiagnosticErrors(ds []Diagnostic) bool {
	for _, d := range ds {
		if d.Level != "warning" {
			return true
		}
	}
	return false
}
func boundedSize(values ...int) int {
	total := 0
	for _, n := range values {
		total += n
		if total > customMaxExpandedBlocks {
			return customMaxExpandedBlocks + 1
		}
	}
	return total
}
func (v *diagnosticValidator) ticker(t string, p []any) {
	if _, ok := v.compiler.resolveTicker(t); !ok {
		v.issue("UNKNOWN_TICKER", fmt.Sprintf("%s is not an allowed research ticker.", quoteTicker(t)), p, "error", "error")
	}
}
func (v *diagnosticValidator) days(n int, p []any) {
	if n < customMinDays || n > customMaxDays {
		v.issue("INVALID_WINDOW", fmt.Sprintf("Lookback must be between %d and %d whole days.", customMinDays, customMaxDays), p, "error", "error")
	}
}
func (v *diagnosticValidator) measure(n Node, right bool, p []any) {
	metric, ticker, lookback, short, long, smooth := n.Metric, n.Ticker, n.Lookback, n.ShortWindow, n.LongWindow, n.SmoothWindow
	metricField, tickerField, lookbackField, shortField, longField, smoothField := "metric", "ticker", "lookback", "shortWindow", "longWindow", "smoothWindow"
	if right {
		metric, ticker, lookback, short, long, smooth = n.RightMetric, n.RightTicker, n.RightLookback, n.RightShort, n.RightLong, n.RightSmooth
		metricField, tickerField, lookbackField, shortField, longField, smoothField = "rightMetric", "rightTicker", "rightLookback", "rightShortWindow", "rightLongWindow", "rightSmoothWindow"
	}
	v.ticker(ticker, appendPath(p, tickerField))
	spec, ok := measures[metric]
	if !ok {
		v.issue("UNKNOWN_METRIC", "IF uses an unknown measure.", appendPath(p, metricField), "error", "error")
		return
	}
	if spec.windowed {
		v.days(lookback, appendPath(p, lookbackField))
	}
	if spec.oscillator {
		v.days(short, appendPath(p, shortField))
		v.days(long, appendPath(p, longField))
		if short >= long {
			v.issue("WINDOW_ORDER", "An oscillator's short window must be shorter than its long one.", appendPath(p, shortField), "error", "error")
		}
		if spec.smooth {
			v.days(smooth, appendPath(p, smoothField))
		}
	}
}
func (v *diagnosticValidator) predicate(n Node, p []any, parts *int) {
	*parts++
	if *parts > customMaxConditionParts {
		if *parts == customMaxConditionParts+1 {
			v.issue("PREDICATE_LIMIT", fmt.Sprintf("A combined condition supports at most %d parts.", customMaxConditionParts), p, "error", "error")
		}
		return
	}
	if n.Type == "any" || n.Type == "all" {
		if len(n.Conditions) == 0 {
			v.issue("EMPTY_PREDICATE", "ANY/ALL needs at least one valid condition.", appendPath(p, "conditions"), "error", "")
		}
		for i, kid := range n.Conditions {
			v.predicate(kid, appendPath(p, "conditions", i), parts)
		}
		return
	}
	if n.Type != "comparison" {
		v.issue("INVALID_PREDICATE", "ANY/ALL needs at least one valid condition.", appendPath(p, "type"), "error", "error")
		return
	}
	if !slices.Contains(customComparators, n.Comparator) {
		v.issue("INVALID_COMPARATOR", "IF compares with >, >=, =, <= or <.", appendPath(p, "comparator"), "error", "error")
	}
	v.measure(n, false, p)
	switch n.CompareMode {
	case "indicator":
		v.measure(n, true, p)
	case "value":
		if math.IsNaN(n.Value) || math.IsInf(n.Value, 0) || math.Abs(n.Value) > customMaxValue {
			v.issue("INVALID_VALUE", "A condition value must be between -1,000,000 and 1,000,000.", appendPath(p, "value"), "error", "")
		}
	default:
		v.issue("INVALID_COMPARISON", "IF compares with a number or another measure.", appendPath(p, "compareMode"), "error", "error")
	}
}
func predicateSize(n Node, then, otherwise int) int {
	if n.Type != "any" && n.Type != "all" {
		return boundedSize(3, then, otherwise)
	}
	out := otherwise
	if n.Type == "all" {
		out = then
	}
	for i := len(n.Conditions) - 1; i >= 0; i-- {
		if n.Type == "all" {
			out = predicateSize(n.Conditions[i], out, otherwise)
		} else {
			out = predicateSize(n.Conditions[i], then, out)
		}
	}
	return out
}
func (v *diagnosticValidator) list(nodes []Node, depth int, p []any, owner string) (int, []string) {
	if len(nodes) == 0 {
		sourceLevel := "warning"
		if depth == 0 {
			sourceLevel = ""
		}
		v.issue("EMPTY_LIST", owner+" needs at least one block.", p, "error", sourceLevel)
	}
	if len(nodes) > customMaxList {
		v.issue("LIST_LIMIT", fmt.Sprintf("A list has more than %d blocks.", customMaxList), p, "error", "")
	}
	total := 0
	kinds := make([]string, 0, len(nodes))
	for i, n := range nodes {
		size, kind := v.node(n, depth, appendPath(p, i))
		total = boundedSize(total, size)
		kinds = append(kinds, kind)
		if v.blocks > customMaxBlocks {
			break
		}
	}
	return total, kinds
}
func (v *diagnosticValidator) selection(selection Node, p []any) int {
	for _, kid := range selection.Children {
		if kid.Kind != "asset" {
			v.issue("SELECTION_CANDIDATES", "Selection with inverse-volatility or percentage weights needs asset candidates. Use equal weighting to select whole groups.", p, "error", "")
			return 0
		}
	}
	if len(selection.Children) == 0 || selection.Count < 1 || selection.Count > customMaxCount {
		return 0
	}
	plan, err := planWeightedSelection(len(selection.Children), selection.Count)
	if err != nil {
		v.issue("SELECTION_LIMIT", err.Error(), p, "error", "")
		return customMaxExpandedBlocks + 1
	}
	return plan.size
}
func (v *diagnosticValidator) node(n Node, depth int, p []any) (int, string) {

	v.blocks++
	if v.blocks > customMaxBlocks {
		if v.blocks == customMaxBlocks+1 {
			v.issue("BLOCK_LIMIT", fmt.Sprintf("The strategy has more than %d blocks.", customMaxBlocks), p, "error", "")
		}
		return 0, n.Kind
	}
	if depth >= customMaxDepth {
		v.issue("DEPTH_LIMIT", fmt.Sprintf("Blocks are nested more than %d levels deep.", customMaxDepth), p, "error", "")
		return 0, n.Kind
	}

	switch n.Kind {
	case "asset":
		v.ticker(n.Ticker, appendPath(p, "ticker"))
		return 1, "asset"
	case "condition":
		predicate := n
		predicate.Type = "comparison"
		pp := p
		if n.Predicate != nil {
			predicate = *n.Predicate
			pp = appendPath(p, "predicate")
		}
		parts := 0
		v.predicate(predicate, pp, &parts)
		then, _ := v.list(n.ThenChildren, depth+1, appendPath(p, "thenChildren"), "The IF branch")
		otherwise, _ := v.list(n.ElseChildren, depth+1, appendPath(p, "elseChildren"), "The ELSE branch")
		return predicateSize(predicate, then, otherwise), "condition"
	case "group", "weight", "filter":
		if n.Kind == "weight" {
			switch n.Method {
			case "equal", "specified":
			case "inverse-volatility":
				v.days(n.Lookback, appendPath(p, "lookback"))
			default:
				v.issue("INVALID_WEIGHT", "WEIGHT must be equal, by inverse volatility, or by set percentages.", appendPath(p, "method"), "error", "error")
			}
			if n.Method == "specified" && len(n.Children) > 0 {
				total := 0
				for i, kid := range n.Children {
					if kid.Percent < customMinPercent || kid.Percent > customMaxPercent {
						v.issue("INVALID_SHARE", "Every block under a percentage weight needs a whole share between 1% and 100%.", appendPath(p, "children", i, "percent"), "error", "error")
					}
					total += kid.Percent
				}
				if total != customMaxPercent {
					v.issue("SHARE_TOTAL", fmt.Sprintf("Shares add up to %d%%, not 100%%.", total), appendPath(p, "children"), "error", "error")
				}
			}
		}
		if n.Kind == "filter" {
			spec, ok := measures[n.Metric]
			if !ok || !spec.windowed {
				v.issue("INVALID_RANK_METRIC", "SELECT ranks by a measure with one lookback.", appendPath(p, "metric"), "error", "")
			}
			if n.Direction != "top" && n.Direction != "bottom" {
				v.issue("INVALID_DIRECTION", "SELECT keeps the top or bottom blocks.", appendPath(p, "direction"), "error", "error")
			}
			if n.Count < 1 || n.Count > customMaxCount {
				v.issue("INVALID_COUNT", fmt.Sprintf("SELECT must keep between 1 and %d blocks.", customMaxCount), appendPath(p, "count"), "error", "error")
			}
			v.days(n.Lookback, appendPath(p, "lookback"))
			pool, _ := selectionCandidates(n)
			if len(pool) == 1 {
				v.issue("REDUNDANT_SELECTION", "Select has only one candidate, so changing the count keeps the same choice. Add more candidates under Select or its equal weight.", p, "warning", "")
			} else if len(pool) > 1 && n.Count >= len(pool) {
				v.issue("REDUNDANT_SELECTION", fmt.Sprintf("Select has %d candidates and keeps up to %d, so none are dropped.", len(pool), n.Count), p, "warning", "")
			}

		}
		size, kinds := v.list(n.Children, depth+1, appendPath(p, "children"), strings.ToUpper(map[string]string{"group": "GROUP", "weight": "WEIGHT", "filter": "SELECT"}[n.Kind]))
		if n.Kind == "group" {
			if len(kinds) != 1 || (kinds[0] != "weight" && kinds[0] != "condition") {
				size = boundedSize(size, 1)
			}
			return boundedSize(1, size), "group"
		}
		if n.Kind == "weight" && n.Method == "inverse-volatility" && len(n.Children) == 1 && n.Children[0].Kind == "filter" {
			return v.selection(n.Children[0], p), "selected-weight"
		}
		if n.Kind == "filter" {
			pool := n.Children
			if len(pool) == 1 && pool[0].Kind == "weight" {
				allocation := pool[0]
				pool = allocation.Children
				if allocation.Method != "equal" {
					selection := n
					selection.Children = pool
					return v.selection(selection, p), "selected-weight"
				}
			}
			kind := "filter"
			if len(n.Children) == 1 && n.Children[0].Kind == "weight" {
				kind = "weight"
			}
			return boundedSize(1, size), kind
		}
		return boundedSize(1, size), "weight"
	default:
		v.issue("UNKNOWN_BLOCK", fmt.Sprintf("Unknown block type %s.", quoteTicker(n.Kind)), appendPath(p, "kind"), "error", "error")
		return 0, n.Kind
	}
}
