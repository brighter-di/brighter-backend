package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Draft formatting serializes editor ASTs without requiring semantic readiness.
// It preserves labels and invalid numeric values; it never repairs rules. The
// separate structural limits allow displaying the first invalid depth/list edge
// while bounding recursion and output even for untrusted editor payloads.
const maxDraftDepth = 64
const maxDraftBlocks = 6000

type draftPrinter struct {
	compiler *Compiler
	blocks   int
	lines    []string
}

func draftObject(value any) (map[string]any, error) {
	m, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("draft block must be an object")
	}
	return m, nil
}
func draftString(m map[string]any, key string) string { s, _ := m[key].(string); return s }
func draftNumber(m map[string]any, key string) string {
	value, ok := m[key]
	if !ok {
		return "undefined"
	}
	if value == nil {
		return "null"
	}
	switch n := value.(type) {
	case json.Number:
		return string(n)
	case float64:
		return strconv.FormatFloat(n, 'f', -1, 64)
	case string:
		return n
	default:
		return fmt.Sprint(n)
	}
}
func (p *draftPrinter) ticker(m map[string]any, key string) string {
	s := draftString(m, key)
	if t, ok := p.compiler.resolveTicker(s); ok {
		return t
	}
	return s
}
func (p *draftPrinter) measure(m map[string]any, right bool) string {
	metricField, tickerField, lookbackField, shortField, longField, smoothField := "metric", "ticker", "lookback", "shortWindow", "longWindow", "smoothWindow"
	if right {
		metricField, tickerField, lookbackField, shortField, longField, smoothField = "rightMetric", "rightTicker", "rightLookback", "rightShortWindow", "rightLongWindow", "rightSmoothWindow"
	}
	metric := draftString(m, metricField)
	spec := measures[metric]
	args := p.ticker(m, tickerField)
	if spec.windowed {
		args += ", " + draftNumber(m, lookbackField) + " days"
	}
	if spec.oscillator {
		args += ", " + draftNumber(m, shortField) + ", " + draftNumber(m, longField)
	}
	if spec.smooth {
		args += ", " + draftNumber(m, smoothField)
	}
	return metricCodes[metric] + "(" + args + ")"
}
func (p *draftPrinter) predicate(m map[string]any, depth int) (string, error) {
	if depth >= maxDraftDepth {
		return "", fmt.Errorf("draft predicate exceeds %d levels", maxDraftDepth)
	}
	kind := draftString(m, "type")
	if kind == "any" || kind == "all" {
		values, ok := m["conditions"].([]any)
		if !ok && m["conditions"] != nil {
			return "", fmt.Errorf("draft conditions must be a list")
		}
		if len(values) > customMaxConditionParts+1 {
			return "", fmt.Errorf("draft predicate has too many parts")
		}
		parts := make([]string, 0, len(values))
		for _, value := range values {
			child, err := draftObject(value)
			if err != nil {
				return "", err
			}
			text, err := p.predicate(child, depth+1)
			if err != nil {
				return "", err
			}
			parts = append(parts, text)
		}
		return strings.ToUpper(kind) + "(" + strings.Join(parts, ", ") + ")", nil
	}
	rhs := draftNumber(m, "value")
	if draftString(m, "compareMode") == "indicator" {
		rhs = p.measure(m, true)
	}
	return p.measure(m, false) + " " + comparatorCodes[draftString(m, "comparator")] + " " + rhs, nil
}
func (p *draftPrinter) list(value any, depth int, shares bool) error {
	if value == nil {
		return nil
	}
	nodes, ok := value.([]any)
	if !ok {
		return fmt.Errorf("draft children must be a list")
	}
	if depth >= maxDraftDepth || len(nodes) > maxDraftBlocks {
		return fmt.Errorf("draft exceeds structural formatting limits")
	}
	for _, value := range nodes {
		p.blocks++
		if p.blocks > maxDraftBlocks {
			return fmt.Errorf("draft has more than %d blocks", maxDraftBlocks)
		}
		m, err := draftObject(value)
		if err != nil {
			return err
		}
		kind := draftString(m, "kind")
		text := ""
		switch kind {
		case "asset":
			text = "BUY " + p.ticker(m, "ticker")
		case "group":
			text = "GROUP " + quoted(draftString(m, "name"))
		case "weight":
			switch draftString(m, "method") {
			case "equal":
				text = "WEIGHT equally"
			case "specified":
				text = "WEIGHT by percentage"
			case "inverse-volatility":
				text = "WEIGHT by inverse volatility (" + draftNumber(m, "lookback") + " days)"
			default:
				return fmt.Errorf("unknown draft weight method")
			}
		case "filter":
			text = "SELECT " + draftString(m, "direction") + " " + draftNumber(m, "count") + " BY " + metricCodes[draftString(m, "metric")] + " (" + draftNumber(m, "lookback") + " days)"
		case "condition":
			expression := m
			if m["predicate"] != nil {
				expression, err = draftObject(m["predicate"])
				if err != nil {
					return err
				}
			}
			pred, err := p.predicate(expression, 0)
			if err != nil {
				return err
			}
			text = "IF " + pred
		default:
			return fmt.Errorf("unknown draft block kind %q", kind)
		}
		if shares && m["percent"] != nil {
			text += " (" + draftNumber(m, "percent") + "%)"
		}
		prefix := strings.Repeat(" ", depth*IndentWidth)
		p.lines = append(p.lines, prefix+text)
		if kind == "condition" {
			if err := p.list(m["thenChildren"], depth+1, false); err != nil {
				return err
			}
			p.lines = append(p.lines, prefix+"ELSE")
			if err := p.list(m["elseChildren"], depth+1, false); err != nil {
				return err
			}
			p.lines = append(p.lines, prefix+"END")
		} else if kind != "asset" {
			if err := p.list(m["children"], depth+1, kind == "weight" && draftString(m, "method") == "specified"); err != nil {
				return err
			}
		}
	}
	return nil
}

// FormatDraft prints a native typed draft. The JSON ABI additionally preserves
// missing, fractional and nonfinite editor values represented in raw JSON.
func (c *Compiler) FormatDraft(s Strategy) (string, error) {
	var nodes func([]Node) []any
	var block func(Node) map[string]any
	nodes = func(in []Node) []any {
		out := make([]any, len(in))
		for i, n := range in {
			out[i] = block(n)
		}
		return out
	}
	block = func(n Node) map[string]any {
		m := map[string]any{"kind": n.Kind, "type": n.Type, "name": n.Name, "method": n.Method, "direction": n.Direction, "count": n.Count, "percent": n.Percent, "ticker": n.Ticker, "metric": n.Metric, "lookback": n.Lookback, "shortWindow": n.ShortWindow, "longWindow": n.LongWindow, "smoothWindow": n.SmoothWindow, "comparator": n.Comparator, "compareMode": n.CompareMode, "value": n.Value, "rightMetric": n.RightMetric, "rightTicker": n.RightTicker, "rightLookback": n.RightLookback, "rightShortWindow": n.RightShort, "rightLongWindow": n.RightLong, "rightSmoothWindow": n.RightSmooth, "children": nodes(n.Children), "thenChildren": nodes(n.ThenChildren), "elseChildren": nodes(n.ElseChildren), "conditions": nodes(n.Conditions)}
		if n.Predicate != nil {
			m["predicate"] = block(*n.Predicate)
		}
		return m
	}
	return c.formatDraft(map[string]any{"name": s.Name, "rebalance": s.Rebalance, "corridor": s.Corridor, "benchmark": s.Benchmark, "children": nodes(s.Children)})
}
func (c *Compiler) formatDraft(m map[string]any) (string, error) {
	p := draftPrinter{compiler: c}
	rebalance := draftString(m, "rebalance")
	if rebalance == "threshold" {
		corridor := draftNumber(m, "corridor")
		if m["corridor"] == nil {
			corridor = strconv.Itoa(customMinCorridor)
		}
		rebalance += " (" + corridor + "%)"
	}
	p.lines = []string{"STRATEGY " + quoted(draftString(m, "name")), "REBALANCE " + rebalance, "BENCHMARK " + p.ticker(m, "benchmark"), ""}
	if err := p.list(m["children"], 0, false); err != nil {
		return "", err
	}
	code := strings.Join(p.lines, "\n")
	if len(code) > MaxSourceBytes*2 {
		return "", fmt.Errorf("formatted draft exceeds 8 MiB")
	}
	return code, nil
}

// executeDraftJSON keeps editor serialization separate from the strict native
// transport used for execution. Compilation's numeric admission is unchanged.
func executeDraftJSON(raw []byte) ([]byte, bool) {
	var request struct {
		Operation string          `json:"operation"`
		Strategy  json.RawMessage `json:"strategy"`
		Node      json.RawMessage `json:"node"`
		Source    *string         `json:"source"`
		Options   Options         `json:"options"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, false
	}
	if request.Operation != "formatDraft" && request.Operation != "formatMeasure" && request.Operation != "formatPredicate" {
		return nil, false
	}
	out := Response{Version: Version, LanguageVersion: LanguageVersion}
	fail := func(err error) ([]byte, bool) {
		out.Diagnostics = []Diagnostic{{Code: "BAD_DRAFT", Message: err.Error(), Level: "error"}}
		data, _ := json.Marshal(out)
		return data, true
	}
	input := request.Strategy
	if request.Operation != "formatDraft" {
		input = request.Node
	}
	if len(input) == 0 || request.Source != nil || (request.Operation == "formatDraft" && len(request.Node) > 0) || (request.Operation != "formatDraft" && len(request.Strategy) > 0) {
		return fail(fmt.Errorf("provide exactly one draft strategy or expression for this operation"))
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	var m map[string]any
	if err := decoder.Decode(&m); err != nil || m == nil {
		return fail(fmt.Errorf("draft must be an object"))
	}
	compiler := New(request.Options)
	var code string
	var err error
	switch request.Operation {
	case "formatDraft":
		code, err = compiler.formatDraft(m)
	case "formatMeasure":
		p := draftPrinter{compiler: compiler}
		code = p.measure(m, false)
	case "formatPredicate":
		p := draftPrinter{compiler: compiler}
		code, err = p.predicate(m, 0)
	}
	if err != nil {
		return fail(err)
	}
	out.OK = true
	out.Code = code
	data, _ := json.Marshal(out)
	return data, true
}
