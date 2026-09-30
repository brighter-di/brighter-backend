package core

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
)

// The diagnostic transport shadows numeric fields and child lists in the
// execution transport. One decode preserves invalid numbers and presence;
// execution keeps using the original strict Go types and canonical identities.
type diagnosticNumber struct {
	value          float64
	present, valid bool
}

func (n *diagnosticNumber) UnmarshalJSON(raw []byte) error {
	n.present = true
	value, err := strconv.ParseFloat(string(raw), 64)
	n.value = value
	n.valid = err == nil && !math.IsNaN(value) && !math.IsInf(value, 0)
	return nil
}

type diagnosticNode struct {
	Node
	Percent       diagnosticNumber `json:"percent"`
	Count         diagnosticNumber `json:"count"`
	Lookback      diagnosticNumber `json:"lookback"`
	ShortWindow   diagnosticNumber `json:"shortWindow"`
	LongWindow    diagnosticNumber `json:"longWindow"`
	SmoothWindow  diagnosticNumber `json:"smoothWindow"`
	RightLookback diagnosticNumber `json:"rightLookback"`
	RightShort    diagnosticNumber `json:"rightShortWindow"`
	RightLong     diagnosticNumber `json:"rightLongWindow"`
	RightSmooth   diagnosticNumber `json:"rightSmoothWindow"`
	Value         diagnosticNumber `json:"value"`
	Children      []diagnosticNode `json:"children"`
	ThenChildren  []diagnosticNode `json:"thenChildren"`
	ElseChildren  []diagnosticNode `json:"elseChildren"`
	Conditions    []diagnosticNode `json:"conditions"`
	Predicate     *diagnosticNode  `json:"predicate"`
}
type diagnosticStrategy struct {
	Strategy
	Corridor diagnosticNumber `json:"corridor"`
	Children []diagnosticNode `json:"children"`
}

func executeDiagnosticsJSON(raw []byte) ([]byte, bool) {
	var request struct {
		Operation string              `json:"operation"`
		Strategy  *diagnosticStrategy `json:"strategy"`
		Source    *string             `json:"source"`
		Options   Options             `json:"options"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		out := Response{Version: Version, LanguageVersion: LanguageVersion, Diagnostics: []Diagnostic{{Code: "BAD_REQUEST", Message: fmt.Sprintf("Invalid strategy transport: %v", err), Level: "error"}}}
		data, _ := json.Marshal(out)
		return data, true
	}
	if request.Strategy == nil || request.Source != nil {
		return nil, false
	}
	invalidNumbers := []Diagnostic{}
	numeric := func(number diagnosticNumber, path []any, field string, active, integer bool) float64 {
		valid := number.present && number.valid
		if integer {
			valid = valid && math.Trunc(number.value) == number.value && number.value >= float64(math.MinInt) && number.value < float64(math.MaxInt)
		}
		if !valid {
			// Missing integer fields retain the transport's zero default; readiness
			// supplies their range/share error. Missing active values are editor errors.
			if active && (number.present || !integer) && len(invalidNumbers) < customMaxBlocks {
				sourceLevel := "error"
				message := "This field must be a whole number."
				code := "INVALID_NUMBER"
				if !integer {
					sourceLevel = ""
					message = "A condition value must be a finite number."
					code = "INVALID_VALUE"
				}
				invalidNumbers = append(invalidNumbers, Diagnostic{Code: code, Message: message, Path: appendPath(path, field), Level: "error", SourceLevel: sourceLevel})
			}
			return 0
		}
		return number.value
	}
	blocks := 0
	var block func(diagnosticNode, []any, int, bool) Node
	var list func([]diagnosticNode, []any, int, bool) []Node
	list = func(in []diagnosticNode, path []any, depth int, shares bool) []Node {
		if len(in) == 0 {
			return nil
		}
		out := make([]Node, 0, min(len(in), maxDraftBlocks+1))
		for i, n := range in {
			out = append(out, block(n, appendPath(path, i), depth, shares))
			if blocks > maxDraftBlocks {
				break
			}
		}
		return out
	}
	block = func(raw diagnosticNode, path []any, depth int, shares bool) Node {
		n := raw.Node
		blocks++
		if depth >= maxDraftDepth || blocks > maxDraftBlocks {
			return n
		}
		metric := measures[n.Metric]
		right := measures[n.RightMetric]
		comparison := n.Kind == "condition" && raw.Predicate == nil || n.Type == "comparison"
		indicator := comparison && n.CompareMode == "indicator"
		n.Percent = int(numeric(raw.Percent, path, "percent", shares, true))
		n.Count = int(numeric(raw.Count, path, "count", n.Kind == "filter", true))
		n.Lookback = int(numeric(raw.Lookback, path, "lookback", comparison && metric.windowed || n.Kind == "filter" || n.Kind == "weight" && n.Method == "inverse-volatility", true))
		n.ShortWindow = int(numeric(raw.ShortWindow, path, "shortWindow", comparison && metric.oscillator, true))
		n.LongWindow = int(numeric(raw.LongWindow, path, "longWindow", comparison && metric.oscillator, true))
		n.SmoothWindow = int(numeric(raw.SmoothWindow, path, "smoothWindow", comparison && metric.smooth, true))
		n.RightLookback = int(numeric(raw.RightLookback, path, "rightLookback", indicator && right.windowed, true))
		n.RightShort = int(numeric(raw.RightShort, path, "rightShortWindow", indicator && right.oscillator, true))
		n.RightLong = int(numeric(raw.RightLong, path, "rightLongWindow", indicator && right.oscillator, true))
		n.RightSmooth = int(numeric(raw.RightSmooth, path, "rightSmoothWindow", indicator && right.smooth, true))
		n.Value = numeric(raw.Value, path, "value", comparison && n.CompareMode == "value", false)
		if raw.Predicate != nil {
			predicate := block(*raw.Predicate, appendPath(path, "predicate"), depth+1, false)
			n.Predicate = &predicate
		}
		if len(raw.Conditions) > 0 {
			n.Conditions = list(raw.Conditions, appendPath(path, "conditions"), depth+1, false)
		}
		if len(raw.Children) > 0 {
			n.Children = list(raw.Children, appendPath(path, "children"), depth+1, n.Kind == "weight" && n.Method == "specified")
		}
		if len(raw.ThenChildren) > 0 {
			n.ThenChildren = list(raw.ThenChildren, appendPath(path, "thenChildren"), depth+1, false)
		}
		if len(raw.ElseChildren) > 0 {
			n.ElseChildren = list(raw.ElseChildren, appendPath(path, "elseChildren"), depth+1, false)
		}
		return n
	}
	strategy := request.Strategy.Strategy
	strategy.Corridor = int(numeric(request.Strategy.Corridor, nil, "corridor", strategy.Rebalance == "threshold", true))
	strategy.Children = list(request.Strategy.Children, []any{"children"}, 0, false)
	diagnostics := invalidNumbers
	seen := map[string]bool{}
	for _, d := range diagnostics {
		path, _ := json.Marshal(d.Path)
		seen[string(path)] = true
	}
	for _, d := range New(request.Options).Diagnostics(strategy) {
		path, _ := json.Marshal(d.Path)
		if !seen[string(path)] {
			diagnostics = append(diagnostics, d)
		}
	}
	out := Response{Version: Version, LanguageVersion: LanguageVersion, OK: !hasDiagnosticErrors(diagnostics), Diagnostics: diagnostics}
	data, _ := json.Marshal(out)
	return data, true
}
