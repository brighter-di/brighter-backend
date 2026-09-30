package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/brighter-di/backend/internal/strategy"
	dsl "github.com/brighter-di/strategy-dsl"
)

// Writing a strategy, without a browser.
//
// strategy-dsl is a plain Go package with no network of its own, so the whole
// authoring loop — parse, check, format, compile — runs in this process. The
// one thing the language does not decide is which symbols are tradable; that
// belongs to the token registry, and strategy.CompileCustom applies it, so a
// rule naming an unlisted ticker is refused here exactly as it is in Studio.

func authoringTools(cat Catalogue) []Tool {
	return []Tool{{
		Name: "dsl_reference",
		Description: "How to write a Brighter strategy: the language's blocks, indentation rules, indicators and limits. " +
			"Read this before writing any rule — the language is small and specific, and guessing its syntax wastes a turn.",
		InputSchema: object(map[string]any{
			"indicators": boolean("Include the full indicator table with every measure's syntax. Off by default; the reference already names them."),
			"limits":     boolean("Include the numeric limits a rule must stay inside."),
		}),
		run: func(_ context.Context, raw json.RawMessage) (any, error) {
			a, err := args[struct {
				Indicators bool `json:"indicators"`
				Limits     bool `json:"limits"`
			}](raw)
			if err != nil {
				return nil, err
			}
			spec, err := specification()
			if err != nil {
				return nil, err
			}
			out := map[string]any{"version": spec["version"], "reference": renderGrammar(spec)}
			if a.Indicators {
				out["indicators"] = spec["metrics"]
			}
			if a.Limits {
				out["limits"] = spec["limits"]
			}
			return out, nil
		},
	}, {
		Name: "strategy_schema",
		Description: "The JSON schema for a strategy as an object, for when you would rather build the rule tree " +
			"than write the text. check_strategy and backtest_code both accept text, which is usually easier.",
		InputSchema: object(map[string]any{}),
		run: func(_ context.Context, _ json.RawMessage) (any, error) {
			var schema any
			if err := json.Unmarshal(dsl.AssistantSchema(), &schema); err != nil {
				return nil, errors.New("the strategy schema could not be read")
			}
			return schema, nil
		},
	}, {
		Name: "check_strategy",
		Description: "Check a strategy written in Brighter's language: syntax, semantics, and whether every ticker it " +
			"names is one Brighter can actually trade. Returns the faults with their line and column. " +
			"Costs nothing and calls nothing upstream — check before you backtest.",
		InputSchema: object(map[string]any{"code": str("The strategy, in Brighter's language.")}, "code"),
		run: func(_ context.Context, raw json.RawMessage) (any, error) {
			a, err := args[struct {
				Code string `json:"code"`
			}](raw)
			if err != nil {
				return nil, err
			}
			custom, faults := parseToCustom(a.Code)
			if len(faults) > 0 {
				return map[string]any{"valid": false, "problems": faults}, nil
			}
			// Compiling is what applies the ticker policy and every semantic
			// rule, so a strategy that compiles is one that can be run.
			if _, err := strategy.CompileCustom(custom); err != nil {
				return map[string]any{"valid": false, "problems": []map[string]any{{"message": reason(err)}}}, nil
			}
			return map[string]any{"valid": true, "name": custom.Name, "rebalance": custom.Rebalance, "benchmark": custom.Benchmark}, nil
		},
	}, {
		Name: "format_strategy",
		Description: "Rewrite a strategy in the language's canonical form — the spelling Studio's editor shows. " +
			"Useful for checking that what you wrote means what you think it means.",
		InputSchema: object(map[string]any{"code": str("The strategy, in Brighter's language.")}, "code"),
		run: func(_ context.Context, raw json.RawMessage) (any, error) {
			a, err := args[struct {
				Code string `json:"code"`
			}](raw)
			if err != nil {
				return nil, err
			}
			formatted, err := dsl.FormatSource(a.Code)
			if err != nil {
				return nil, errors.New(reason(err))
			}
			return map[string]any{"code": formatted}, nil
		},
	}, {
		Name: "backtest_code", upstream: true,
		Description: "Run a strategy you have written against real market history and return its figures. " +
			"This is a real backtest against real prices — not a simulation and not cached per call — so run it " +
			"deliberately, after check_strategy passes. Returns metrics only, never the curve.",
		InputSchema: object(map[string]any{
			"code":  str("The strategy, in Brighter's language."),
			"range": enum("Window to test over.", "1y", "3y", "5y"),
		}, "code"),
		run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			a, err := args[struct {
				Code  string `json:"code"`
				Range string `json:"range"`
			}](raw)
			if err != nil {
				return nil, err
			}
			custom, faults := parseToCustom(a.Code)
			if len(faults) > 0 {
				return nil, fmt.Errorf("that strategy does not parse: %s", faults[0]["message"])
			}
			v, err := cat.BacktestCustom(ctx, custom, or(a.Range, "5y"))
			if err != nil {
				return nil, errors.New(reason(err))
			}
			return map[string]any{"name": custom.Name, "metrics": shapeBacktest(v)}, nil
		},
	}}
}

// parseToCustom turns the language's text into the transport the backend
// backtests. The round trip through JSON is the same one CompileCustom makes in
// reverse: both structs are generated from one language specification, so their
// tags match exactly.
func parseToCustom(code string) (strategy.CustomStrategy, []map[string]any) {
	var out strategy.CustomStrategy
	if strings.TrimSpace(code) == "" {
		return out, []map[string]any{{"message": "There is no strategy here."}}
	}
	parsed, err := dsl.Parse(code)
	if err != nil {
		var syntax *dsl.SyntaxError
		if errors.As(err, &syntax) {
			return out, []map[string]any{{"message": syntax.Message, "line": syntax.Line, "column": syntax.Column}}
		}
		return out, []map[string]any{{"message": reason(err)}}
	}
	raw, err := json.Marshal(parsed)
	if err != nil {
		return out, []map[string]any{{"message": "That strategy holds a value the language cannot carry."}}
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, []map[string]any{{"message": "That strategy could not be read."}}
	}
	return out, nil
}

// reason unwraps the language's and the backend's own errors into something a
// caller can act on, and never leaks an internal one.
func reason(err error) string {
	var syntax *dsl.SyntaxError
	if errors.As(err, &syntax) {
		return fmt.Sprintf("line %d:%d: %s", syntax.Line, syntax.Column, syntax.Message)
	}
	var invalid *dsl.ValidationError
	if errors.As(err, &invalid) {
		return invalid.Reason
	}
	var custom *strategy.CustomError
	if errors.As(err, &custom) {
		return custom.Reason
	}
	var svc interface{ Error() string }
	if errors.As(err, &svc) {
		return svc.Error()
	}
	return "That strategy could not be run."
}

func specification() (map[string]any, error) {
	var spec map[string]any
	if err := json.Unmarshal(dsl.Specification(), &spec); err != nil {
		return nil, errors.New("the language specification could not be read")
	}
	return spec, nil
}

// renderGrammar fills the specification's reference text, which carries
// placeholders for the parts that are generated — the cadences, the comparison
// operators, the indicator list — so the reference can never drift from the
// language it describes.
func renderGrammar(spec map[string]any) string {
	grammar, _ := spec["grammar"].(string)
	if grammar == "" {
		return ""
	}
	context := map[string]any{}
	for k, v := range spec {
		context[k] = v
	}
	context["calendarRebalances"] = strings.Join(without(strings2(spec["rebalances"]), "threshold"), "|")
	context["comparisonCodes"] = strings.Join(field(spec["comparators"], "code"), ", ")
	context["measureReference"] = strings.Join(measureLines(spec["metrics"]), "\n")
	context["metricMapping"] = strings.Join(metricMapping(spec["metrics"]), ", ")

	var out strings.Builder
	for {
		open := strings.Index(grammar, "{{")
		if open < 0 {
			out.WriteString(grammar)
			break
		}
		close := strings.Index(grammar[open:], "}}")
		if close < 0 {
			out.WriteString(grammar)
			break
		}
		out.WriteString(grammar[:open])
		out.WriteString(lookup(context, strings.TrimSpace(grammar[open+2:open+close])))
		grammar = grammar[open+close+2:]
	}
	return out.String()
}

func lookup(context map[string]any, path string) string {
	var value any = context
	for _, part := range strings.Split(path, ".") {
		m, ok := value.(map[string]any)
		if !ok {
			return ""
		}
		value = m[part]
	}
	switch v := value.(type) {
	case string:
		return v
	case float64:
		return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.6f", v), "0"), ".")
	case nil:
		return ""
	default:
		raw, _ := json.Marshal(v)
		return string(raw)
	}
}

func strings2(value any) []string {
	items, _ := value.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func without(values []string, drop string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v != drop {
			out = append(out, v)
		}
	}
	return out
}

func field(value any, key string) []string {
	items, _ := value.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			if s, ok := m[key].(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// measureLines renders each indicator the way the reference reads it: the call
// it makes, then what it measures.
func measureLines(value any) []string {
	items, _ := value.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		code, _ := m["code"].(string)
		description, _ := m["description"].(string)
		signature := code + "(T)"
		switch windows, _ := m["windows"].(string); windows {
		case "lookback":
			signature = code + "(T, N days)"
		case "oscillator":
			signature = code + "(T, short, long)"
		case "signal":
			signature = code + "(T, short, long, smooth)"
		}
		out = append(out, signature+": "+description)
	}
	return out
}

func metricMapping(value any) []string {
	items, _ := value.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			code, _ := m["code"].(string)
			name, _ := m["name"].(string)
			out = append(out, code+` -> "`+name+`"`)
		}
	}
	return out
}

/* ── Resources ──────────────────────────────────────────────────────── */

// Resource is a document a client can read without calling a tool. Both of
// these are static: they are the language itself.
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description"`
	MimeType    string `json:"mimeType"`
}

const (
	uriSpecification = "brighter://dsl/specification"
	uriSchema        = "brighter://dsl/schema"
)

func languageResources() []Resource {
	return []Resource{{
		URI: uriSpecification, Name: "dsl-specification", Title: "Brighter strategy language",
		Description: "The language specification: blocks, indicators, comparators, cadences and limits.",
		MimeType:    "application/json",
	}, {
		URI: uriSchema, Name: "strategy-schema", Title: "Strategy schema",
		Description: "JSON schema for a strategy expressed as an object rather than as text.",
		MimeType:    "application/json",
	}}
}

func (s *Server) readResource(params json.RawMessage) (any, *rpcError) {
	var p struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "resources/read needs a uri."}
	}
	var body []byte
	switch p.URI {
	case uriSpecification:
		body = dsl.Specification()
	case uriSchema:
		body = dsl.AssistantSchema()
	default:
		return nil, &rpcError{Code: codeInvalidParams, Message: "Unknown resource " + p.URI + "."}
	}
	return map[string]any{"contents": []any{map[string]any{
		"uri": p.URI, "mimeType": "application/json", "text": string(body),
	}}}, nil
}
