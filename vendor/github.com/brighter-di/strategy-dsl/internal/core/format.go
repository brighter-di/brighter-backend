package core

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Format prints canonical DSL for a semantically valid AST. Editor IDs,
// asset display names and inactive indicator fields are intentionally omitted.
func Format(strategy Strategy) (string, error) { return New(Options{}).Format(strategy) }
func (c *Compiler) Format(strategy Strategy) (string, error) {
	if err := c.Validate(strategy); err != nil {
		return "", err
	}
	var lines []string
	rebalance := strategy.Rebalance
	if rebalance == "threshold" {
		rebalance += fmt.Sprintf(" (%d%%)", strategy.Corridor)
	}
	lines = append(lines, "STRATEGY "+quoted(customName(strategy.Name)), "REBALANCE "+rebalance, "BENCHMARK "+c.ticker(strategy.Benchmark), "")
	var blocks func([]Node, int, bool)
	blocks = func(nodes []Node, depth int, shares bool) {
		prefix := strings.Repeat(" ", depth*IndentWidth)
		for _, n := range nodes {
			text := ""
			switch n.Kind {
			case "asset":
				text = "BUY " + c.ticker(n.Ticker)
			case "group":
				text = "GROUP " + quoted(customName(n.Name))
			case "weight":
				switch n.Method {
				case "equal":
					text = "WEIGHT equally"
				case "specified":
					text = "WEIGHT by percentage"
				case "inverse-volatility":
					text = fmt.Sprintf("WEIGHT by inverse volatility (%d days)", n.Lookback)
				}
			case "filter":
				text = fmt.Sprintf("SELECT %s %d BY %s (%d days)", n.Direction, n.Count, metricCodes[n.Metric], n.Lookback)
			case "condition":
				if n.Predicate != nil {
					text = "IF " + c.predicate(*n.Predicate)
				} else {
					text = "IF " + c.predicate(n)
				}
			}
			if shares {
				text += fmt.Sprintf(" (%d%%)", n.Percent)
			}
			lines = append(lines, prefix+text)
			if n.Kind == "condition" {
				blocks(n.ThenChildren, depth+1, false)
				lines = append(lines, prefix+"ELSE")
				blocks(n.ElseChildren, depth+1, false)
				lines = append(lines, prefix+"END")
			} else if n.Kind != "asset" {
				blocks(n.Children, depth+1, n.Kind == "weight" && n.Method == "specified")
			}
		}
	}
	blocks(strategy.Children, 0, false)
	return strings.Join(lines, "\n"), nil
}
func quoted(s string) string               { b, _ := json.Marshal(s); return string(b) }
func (c *Compiler) ticker(s string) string { t, _ := c.resolveTicker(s); return t }
func (c *Compiler) operand(n Node, right bool) string {
	metric, ticker, lookback, short, long, smooth := n.Metric, n.Ticker, n.Lookback, n.ShortWindow, n.LongWindow, n.SmoothWindow
	if right {
		metric, ticker, lookback, short, long, smooth = n.RightMetric, n.RightTicker, n.RightLookback, n.RightShort, n.RightLong, n.RightSmooth
	}
	spec := measures[metric]
	args := c.ticker(ticker)
	if spec.windowed {
		args += fmt.Sprintf(", %d days", lookback)
	}
	if spec.oscillator {
		args += fmt.Sprintf(", %d, %d", short, long)
	}
	if spec.smooth {
		args += fmt.Sprintf(", %d", smooth)
	}
	return metricCodes[metric] + "(" + args + ")"
}
func (c *Compiler) predicate(n Node) string {
	if n.Type == "any" || n.Type == "all" {
		var terms []string
		for _, term := range n.Conditions {
			terms = append(terms, c.predicate(term))
		}
		return strings.ToUpper(n.Type) + "(" + strings.Join(terms, ", ") + ")"
	}
	rhs := strconv.FormatFloat(n.Value, 'f', -1, 64)
	if n.CompareMode == "indicator" {
		rhs = c.operand(n, true)
	}
	return c.operand(n, false) + " " + comparatorCodes[n.Comparator] + " " + rhs
}

// FormatSource parses and canonicalizes a complete document or fragment.
// Invalid code returns an error; it is never repaired into different rules.
func FormatSource(source string) (string, error) { return New(Options{}).FormatSource(source) }
func (c *Compiler) FormatSource(source string) (string, error) {
	strategy, err := Parse(source)
	if err != nil {
		return "", err
	}
	return c.Format(*strategy)
}
