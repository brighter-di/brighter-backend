package strategy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Publication is immutable. Bindings always address this saved rule version.
type Publication struct {
	Strategy    CustomStrategy       `json:"strategy"`
	Description string               `json:"description"`
	Parameters  []PublishedParameter `json:"parameters"`
}
type PublishedParameter struct {
	ParamSpec
	Path          []any `json:"path"`
	RemainderPath []any `json:"remainderPath,omitempty"`
}
type parameterField struct {
	value, min, max float64
	integer         bool
	remainder       []any
}

func roundParameter(value float64) float64 {
	result, _ := strconv.ParseFloat(strconv.FormatFloat(value, 'g', 15, 64), 64)
	return result
}
func pathKey(path []any) string                { raw, _ := json.Marshal(path); return string(raw) }
func appendPath(path []any, keys ...any) []any { return append(slices.Clone(path), keys...) }

func activeParameterFields(c CustomStrategy) map[string]parameterField {
	fields := map[string]parameterField{}
	add := func(path []any, value, low, high float64, integer bool, remainder []any) {
		fields[pathKey(path)] = parameterField{value, low, high, integer, remainder}
	}
	var comparison func(CustomNode, []any)
	comparison = func(n CustomNode, path []any) {
		if n.Type == "any" || n.Type == "all" {
			for i, child := range n.Conditions {
				comparison(child, appendPath(path, "conditions", i))
			}
			return
		}
		side := func(metric, prefix string, lookback, short, long, smooth int) {
			measure := measures[metric]
			field := func(name string) string {
				if prefix == "" {
					return name
				}
				return prefix + strings.ToUpper(name[:1]) + name[1:]
			}
			window := func(name string, value int) {
				add(appendPath(path, field(name)), float64(value), customMinDays, customMaxDays, true, nil)
			}
			if measure.windowed {
				window("lookback", lookback)
			}
			if measure.oscillator {
				window("shortWindow", short)
				window("longWindow", long)
				if measure.smooth {
					window("smoothWindow", smooth)
				}
			}
		}
		side(n.Metric, "", n.Lookback, n.ShortWindow, n.LongWindow, n.SmoothWindow)
		if n.CompareMode == "indicator" {
			side(n.RightMetric, "right", n.RightLookback, n.RightShort, n.RightLong, n.RightSmooth)
		} else {
			add(appendPath(path, "value"), n.Value, -customMaxValue, customMaxValue, false, nil)
		}
	}
	var walk func([]CustomNode, []any)
	walk = func(nodes []CustomNode, path []any) {
		for i, n := range nodes {
			at := appendPath(path, i)
			if n.Kind == "condition" {
				if n.Predicate != nil {
					comparison(*n.Predicate, appendPath(at, "predicate"))
				} else {
					comparison(n, at)
				}
				walk(n.ThenChildren, appendPath(at, "thenChildren"))
				walk(n.ElseChildren, appendPath(at, "elseChildren"))
			}
			if n.Kind == "filter" {
				add(appendPath(at, "count"), float64(n.Count), 1, customMaxCount, true, nil)
				add(appendPath(at, "lookback"), float64(n.Lookback), customMinDays, customMaxDays, true, nil)
			}
			if n.Kind == "weight" && n.Method == "inverse-volatility" {
				add(appendPath(at, "lookback"), float64(n.Lookback), customMinDays, customMaxDays, true, nil)
			}
			if n.Kind == "weight" && n.Method == "specified" && len(n.Children) > 1 {
				last := len(n.Children) - 1
				for j, child := range n.Children[:last] {
					add(appendPath(at, "children", j, "percent"), float64(child.Percent), 1, float64(100-last), true, appendPath(at, "children", last, "percent"))
				}
			}
			walk(n.Children, appendPath(at, "children"))
		}
	}
	if c.Rebalance == "threshold" {
		add([]any{"corridor"}, float64(c.Corridor), customMinCorridor, customMaxCorridor, true, nil)
	}
	walk(c.Children, []any{"children"})
	return fields
}

var parameterKey = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,119}$`)

func (p Publication) Validate() error {
	if len(p.Strategy.Name) > customMaxName {
		return invalid("Strategy name must be at most 80 characters.")
	}
	if strings.TrimSpace(p.Strategy.Name) == "" {
		return invalid("Give the strategy a name.")
	}
	if len(p.Description) > 2000 {
		return invalid("Strategy description is too long.")
	}
	if _, err := CompileCustom(p.Strategy); err != nil {
		return err
	}
	fields := activeParameterFields(p.Strategy)
	if len(p.Parameters) > len(fields) {
		return invalid("Too many strategy parameters.")
	}
	keys, paths := map[string]bool{}, map[string]bool{}
	for _, s := range p.Parameters {
		field, ok := fields[pathKey(s.Path)]
		if !ok || !equalPaths(s.RemainderPath, field.remainder) {
			return invalid("%s does not bind to an adjustable rule.", s.Label)
		}
		if !parameterKey.MatchString(s.Key) || keys[s.Key] || paths[pathKey(s.Path)] {
			return invalid("Strategy parameters need unique keys and rule bindings.")
		}
		keys[s.Key], paths[pathKey(s.Path)] = true, true
		if strings.TrimSpace(s.Label) == "" || len(s.Label) > 120 || len(s.Unit) > 16 {
			return invalid("Give each parameter a short label and unit.")
		}
		grid := (s.Default - s.Min) / s.Step
		if math.IsNaN(s.Min) || math.IsNaN(s.Max) || math.IsNaN(s.Step) || math.IsInf(s.Min, 0) || math.IsInf(s.Max, 0) || math.IsInf(s.Step, 0) || s.Step <= 0 || s.Min > s.Max || s.Min < field.min || s.Max > field.max || s.Default != field.value || s.Default < s.Min || s.Default > s.Max || math.IsInf(grid, 0) || math.Abs(grid-math.Round(grid)) > 1e-7 {
			return invalid("%s has an invalid default, range or step.", s.Label)
		}
		if field.integer && (math.Trunc(s.Min) != s.Min || math.Trunc(s.Max) != s.Max || math.Trunc(s.Step) != s.Step) {
			return invalid("%s needs whole numbers.", s.Label)
		}
	}
	return nil
}

func setRuleValue(root map[string]any, path []any, value float64) error {
	var current any = root
	for i, key := range path {
		last := i == len(path)-1
		switch parent := current.(type) {
		case map[string]any:
			name, ok := key.(string)
			if !ok {
				return invalid("Invalid parameter binding.")
			}
			if last {
				parent[name] = value
				return nil
			}
			current = parent[name]
		case []any:
			var index int
			switch n := key.(type) {
			case int:
				index = n
			case float64:
				if n != math.Trunc(n) {
					return invalid("Invalid parameter binding.")
				}
				index = int(n)
			default:
				return invalid("Invalid parameter binding.")
			}
			if index < 0 || index >= len(parent) || last {
				return invalid("Invalid parameter binding.")
			}
			current = parent[index]
		default:
			return invalid("Invalid parameter binding.")
		}
	}
	return invalid("Invalid parameter binding.")
}

func (p Publication) Materialize(values Params) (CustomStrategy, error) {
	raw, _ := json.Marshal(p.Strategy)
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		return CustomStrategy{}, err
	}
	fields := activeParameterFields(p.Strategy)
	type remainder struct {
		path  []any
		value float64
	}
	remainders := map[string]*remainder{}
	for _, s := range p.Parameters {
		value, ok := values[s.Key]
		if !ok {
			value = s.Default
		}
		if !okFinite(value) || value < s.Min || value > s.Max {
			return CustomStrategy{}, invalid("%s is outside its published range.", s.Label)
		}
		if err := setRuleValue(document, s.Path, value); err != nil {
			return CustomStrategy{}, err
		}
		if len(s.RemainderPath) > 0 {
			key := pathKey(s.RemainderPath)
			if remainders[key] == nil {
				original := fields[pathKey(s.Path)]
				// The compiler has already checked the full percentage sleeve.
				var current any = document
				for _, key := range original.remainder {
					switch parent := current.(type) {
					case map[string]any:
						current = parent[key.(string)]
					case []any:
						index := 0
						switch n := key.(type) {
						case int:
							index = n
						case float64:
							index = int(n)
						}
						current = parent[index]
					}
				}
				remainders[key] = &remainder{s.RemainderPath, current.(float64)}
			}
			remainders[key].value -= value - s.Default
		}
	}
	for _, r := range remainders {
		if err := setRuleValue(document, r.path, r.value); err != nil {
			return CustomStrategy{}, err
		}
	}
	raw, err := json.Marshal(document)
	if err != nil {
		return CustomStrategy{}, err
	}
	var result CustomStrategy
	if err := json.Unmarshal(raw, &result); err != nil {
		return CustomStrategy{}, invalid("Rule windows, counts and allocations need whole numbers.")
	}
	if _, err := CompileCustom(result); err != nil {
		return CustomStrategy{}, err
	}
	return result, nil
}
func okFinite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func (p Publication) Template(id, author, publishedAt string) *Template {
	holdable := []string{}
	var walk func([]CustomNode)
	walk = func(nodes []CustomNode) {
		for _, n := range nodes {
			if n.Kind == "asset" {
				ticker, _ := ResearchTicker(n.Ticker)
				if !slices.Contains(holdable, ticker) {
					holdable = append(holdable, ticker)
				}
			}
			walk(n.Children)
			walk(n.ThenChildren)
			walk(n.ElseChildren)
		}
	}
	walk(p.Strategy.Children)
	specs := make([]ParamSpec, len(p.Parameters))
	for i, s := range p.Parameters {
		specs[i] = s.ParamSpec
	}
	description := strings.TrimSpace(p.Description)
	if description == "" {
		description = "Created in Brighter Studio."
	}
	return &Template{ID: id, Name: p.Strategy.Name, Thesis: description, Universe: holdable, Benchmark: p.Strategy.Benchmark, Cadence: p.Strategy.Rebalance, Params: specs, Author: author, PublishedAt: publishedAt, Rules: &p.Strategy, Bindings: p.Parameters, publication: &p}
}

// StudioRules are a published strategy's rules with the given values applied,
// for opening in Studio. Built-in templates have none: ok is false.
func (t *Template) StudioRules(values Params) (rules CustomStrategy, ok bool, err error) {
	if t.publication == nil {
		return CustomStrategy{}, false, nil
	}
	rules, err = t.publication.Materialize(values)
	return rules, true, err
}

func equalPaths(a, b []any) bool { return pathKey(a) == pathKey(b) }

// RulesFingerprint identifies executable rules, excluding the comparison benchmark, presentation and
// neutral group/single equal-weight wrappers. Ranking candidate order is kept:
// it decides ties, unlike the order of independently weighted sleeves.
func RulesFingerprint(tree map[string]any) string {
	var normalize func(any) any
	normalize = func(value any) any {
		switch v := value.(type) {
		case map[string]any:
			out := map[string]any{}
			for key, child := range v {
				if key != "id" && key != "name" && key != "description" && key != "collapsed?" {
					out[key] = normalize(child)
				}
			}
			step, _ := out["step"].(string)
			children, _ := out["children"].([]any)
			if step == "group" || (step == "wt-cash-equal" && len(children) == 1) {
				if len(children) == 1 {
					child := children[0].(map[string]any)
					if weight, ok := out["weight"]; ok {
						child["weight"] = weight
					}
					return child
				}
			}
			if step == "filter" {
				count, _ := strconv.Atoi(fmtString(out["select-n"]))
				if count >= len(children) {
					out = map[string]any{"step": "wt-cash-equal", "children": children}
					if weight, ok := v["weight"]; ok {
						out["weight"] = normalize(weight)
					}
					return normalize(out)
				}
			}
			if step == "wt-cash-equal" || step == "wt-inverse-vol" || step == "wt-cash-specified" {
				sort.SliceStable(children, func(i, j int) bool {
					a, _ := json.Marshal(children[i])
					b, _ := json.Marshal(children[j])
					return string(a) < string(b)
				})
				out["children"] = children
			}
			return out
		case []node:
			out := make([]any, len(v))
			for i, child := range v {
				out[i] = normalize(child)
			}
			return out
		case []any:
			out := make([]any, len(v))
			for i, child := range v {
				out[i] = normalize(child)
			}
			return out
		default:
			return value
		}
	}
	raw, _ := json.Marshal(normalize(tree))
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func fmtString(value any) string { v, _ := value.(string); return v }
func SameTreeRules(a, b map[string]any) bool {
	return RulesFingerprint(a) == RulesFingerprint(b)
}
