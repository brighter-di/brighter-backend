package core

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode/utf8"

	contract "github.com/brighter-di/strategy-dsl/internal/spec"
)

var assistantSchemaJSON = contract.Assistant()

var assistantSchema = func() map[string]any {
	var schema map[string]any
	if err := json.Unmarshal(assistantSchemaJSON, &schema); err != nil {
		panic(err)
	}
	return schema
}()

// AssistantSchema describes the legacy assistant JSON envelope. Its enums and
// bounds are generated from the core language specification.
func AssistantSchema() []byte { return append([]byte(nil), assistantSchemaJSON...) }

// validateAssistantSchema implements only the keywords in the generated schema.
// Variant discrimination keeps nested unions linear; a budget bounds hostile
// requests independently of the compiler's semantic depth and expansion limits.
func validateAssistantSchema(value any) *Diagnostic {
	budget := 500000
	var check func(any, map[string]any, []any, int) *Diagnostic
	issue := func(path []any, message string) *Diagnostic {
		return &Diagnostic{Code: "ASSISTANT_SCHEMA", Message: fmt.Sprintf("The draft at /%s %s", joinPath(path), message), Path: path, Level: "error"}
	}
	check = func(value any, schema map[string]any, path []any, depth int) *Diagnostic {
		budget--
		if budget < 0 || depth > 512 {
			return issue(path, "exceeds the schema traversal limit")
		}
		if ref, ok := schema["$ref"].(string); ok {
			return check(value, assistantSchema["$defs"].(map[string]any)[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any), path, depth+1)
		}
		if variants, ok := schema["oneOf"].([]any); ok {
			var best *Diagnostic
			for _, variant := range variants {
				branch := variant.(map[string]any)
				if object, ok := value.(map[string]any); ok {
					props := branch["properties"].(map[string]any)
					typ := props["type"].(map[string]any)
					if constant, has := typ["const"]; has && object["type"] != constant {
						continue
					}
					if allowed, has := typ["enum"].([]any); has && !schemaEnum(object["type"], allowed) {
						continue
					}
					// Condition variants differ by their comparison/predicate envelope.
					if object["type"] == "condition" {
						_, hasPredicate := object["predicate"]
						_, predicateBranch := props["predicate"]
						if hasPredicate != predicateBranch {
							continue
						}
					}
				}
				if d := check(value, branch, path, depth+1); d == nil {
					return nil
				} else if best == nil || len(d.Path) > len(best.Path) {
					best = d
				}
			}
			if best != nil {
				return best
			}
			return issue(path, "has an unknown variant")
		}
		if constant, ok := schema["const"]; ok && value != constant {
			return issue(path, "has an invalid type")
		}
		if allowed, ok := schema["enum"].([]any); ok && !schemaEnum(value, allowed) {
			return issue(path, "has an unknown value")
		}
		switch schema["type"] {
		case "object":
			object, ok := value.(map[string]any)
			if !ok {
				return issue(path, "must be an object")
			}
			props := schema["properties"].(map[string]any)
			for _, field := range schema["required"].([]any) {
				key := field.(string)
				if _, ok := object[key]; !ok {
					return issue(appendPath(path, key), "is required")
				}
			}
			keys := make([]string, 0, len(object))
			for key := range object {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				child, ok := props[key]
				if !ok {
					return issue(appendPath(path, key), "is not a supported field")
				}
				if d := check(object[key], child.(map[string]any), appendPath(path, key), depth+1); d != nil {
					return d
				}
			}
		case "array":
			array, ok := value.([]any)
			if !ok {
				return issue(path, "must be an array")
			}
			if len(array) < int(schema["minItems"].(float64)) || len(array) > int(schema["maxItems"].(float64)) {
				return issue(path, "has an invalid number of blocks")
			}
			for i, item := range array {
				if d := check(item, schema["items"].(map[string]any), appendPath(path, i), depth+1); d != nil {
					return d
				}
			}
		case "string":
			text, ok := value.(string)
			if !ok {
				return issue(path, "must be a string")
			}
			if limit, ok := schema["maxLength"].(float64); ok && utf8.RuneCountInString(text) > int(limit) {
				return issue(path, "is too long")
			}
		case "integer", "number":
			number, ok := value.(float64)
			if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
				return issue(path, "must be a finite number")
			}
			if schema["type"] == "integer" && math.Trunc(number) != number {
				return issue(path, "must be a whole number")
			}
			if number < schema["minimum"].(float64) || number > schema["maximum"].(float64) {
				return issue(path, "is outside the allowed range")
			}
		}
		return nil
	}
	return check(value, assistantSchema, nil, 0)
}
func schemaEnum(value any, allowed []any) bool {
	for _, item := range allowed {
		if text, ok := value.(string); ok && text == item {
			return true
		}
	}
	return false
}
func joinPath(path []any) string {
	parts := make([]string, len(path))
	for i, item := range path {
		parts[i] = fmt.Sprint(item)
	}
	return strings.Join(parts, "/")
}

// ImportAssistant converts and validates the assistant envelope without building
// execution score trees. Studio's presentation and asset availability remain
// caller policy; all strategy semantics are checked by the core.
func (c *Compiler) ImportAssistant(raw []byte) (*Strategy, []Diagnostic) {
	var value any
	if len(raw) > MaxSourceBytes*2 {
		return nil, []Diagnostic{{Code: "BAD_REQUEST", Message: "Assistant JSON exceeds 8 MiB", Level: "error"}}
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, []Diagnostic{{Code: "BAD_REQUEST", Message: "Invalid assistant JSON: " + err.Error(), Level: "error"}}
	}
	if d := validateAssistantSchema(value); d != nil {
		return nil, []Diagnostic{*d}
	}
	text := func(object map[string]any, key string) string { s, _ := object[key].(string); return s }
	number := func(object map[string]any, key string, fallback int) int {
		n, ok := object[key].(float64)
		if !ok {
			return fallback
		}
		return int(n)
	}
	var block func(map[string]any, bool) Node
	var list func(any) []Node
	list = func(value any) []Node {
		array, _ := value.([]any)
		nodes := make([]Node, 0, len(array))
		for _, value := range array {
			nodes = append(nodes, block(value.(map[string]any), false))
		}
		return nodes
	}
	block = func(raw map[string]any, predicate bool) Node {
		kind := text(raw, "type")
		n := Node{Kind: kind, Percent: number(raw, "percent", 0)}
		if predicate {
			n.Kind = ""
			n.Type = kind
		}
		if kind == "any" || kind == "all" {
			n.Conditions = list(raw["conditions"])
			for i := range n.Conditions {
				n.Conditions[i].Type = n.Conditions[i].Kind
				n.Conditions[i].Kind = ""
			}
			return n
		}
		switch kind {
		case "asset":
			n.Ticker = text(raw, "ticker")
		case "group":
			n.Name = text(raw, "name")
			n.Children = list(raw["children"])
		case "weight":
			n.Method = text(raw, "method")
			n.Lookback = number(raw, "lookback", 63)
			n.Children = list(raw["children"])
		case "filter":
			n.Direction = text(raw, "direction")
			n.Count = number(raw, "count", 3)
			n.Metric = text(raw, "metric")
			n.Lookback = number(raw, "lookback", 90)
			n.Children = list(raw["children"])
		case "condition", "comparison":
			// Preserve editor defaults for fields omitted by the assistant envelope.
			n.Ticker = text(raw, "ticker")
			n.Metric = text(raw, "metric")
			n.Lookback = number(raw, "lookback", 200)
			n.ShortWindow = number(raw, "shortWindow", 12)
			n.LongWindow = number(raw, "longWindow", 26)
			n.SmoothWindow = number(raw, "smoothWindow", 9)
			n.Comparator = text(raw, "comparator")
			n.RightMetric = "moving-average"
			n.RightTicker = "SPY"
			n.RightLookback = 200
			n.RightShort = 12
			n.RightLong = 26
			n.RightSmooth = 9
			if expression, ok := raw["predicate"].(map[string]any); ok {
				p := block(expression, true)
				n.Predicate = &p
			} else {
				compare := raw["compare"].(map[string]any)
				n.CompareMode = text(compare, "type")
				if n.CompareMode == "value" {
					n.Value = compare["value"].(float64)
				} else {
					n.RightMetric = text(compare, "metric")
					n.RightTicker = text(compare, "ticker")
					n.RightLookback = number(compare, "lookback", 200)
					n.RightShort = number(compare, "shortWindow", 12)
					n.RightLong = number(compare, "longWindow", 26)
					n.RightSmooth = number(compare, "smoothWindow", 9)
				}
			}
			if kind == "condition" {
				n.ThenChildren = list(raw["then"])
				n.ElseChildren = list(raw["else"])
			}
		}
		return n
	}
	root := value.(map[string]any)
	strategy := &Strategy{Name: text(root, "name"), Benchmark: text(root, "benchmark"), Rebalance: text(root, "rebalance"), Corridor: number(root, "corridor", 0), Children: list(root["root"])}
	if strategy.Rebalance != "threshold" {
		strategy.Corridor = 0
	}
	diagnostics := c.Diagnostics(*strategy)
	if hasDiagnosticErrors(diagnostics) {
		return nil, diagnostics
	}

	// Resolve aliases before returning the transport, just as source hydration does.
	var resolve func(*Node)
	resolve = func(n *Node) {
		if ticker, ok := c.resolveTicker(n.Ticker); ok {
			n.Ticker = ticker
		}
		if ticker, ok := c.resolveTicker(n.RightTicker); ok {
			n.RightTicker = ticker
		}
		if n.Predicate != nil {
			resolve(n.Predicate)
		}
		for _, nodes := range [][]Node{n.Children, n.ThenChildren, n.ElseChildren, n.Conditions} {
			for i := range nodes {
				resolve(&nodes[i])
			}
		}
	}
	for i := range strategy.Children {
		resolve(&strategy.Children[i])
	}
	if ticker, ok := c.resolveTicker(strategy.Benchmark); ok {
		strategy.Benchmark = ticker
	}
	return strategy, diagnostics
}

func executeAssistantJSON(raw []byte) []byte {
	var request struct {
		Operation string          `json:"operation"`
		Assistant json.RawMessage `json:"assistant"`
		Options   Options         `json:"options"`
	}
	out := Response{Version: Version, LanguageVersion: LanguageVersion}
	if err := json.Unmarshal(raw, &request); err != nil {
		out.Diagnostics = []Diagnostic{{Code: "BAD_REQUEST", Message: err.Error(), Level: "error"}}
	} else {
		out.Strategy, out.Diagnostics = New(request.Options).ImportAssistant(request.Assistant)
		out.OK = out.Strategy != nil
	}
	encoded, _ := json.Marshal(out)
	return encoded
}
