package core

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MaxSourceBytes bounds source parsing independently of semantic tree limits.
const MaxSourceBytes = 4 << 20

// SyntaxError uses one-based line and column positions.
type SyntaxError struct {
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Message string `json:"message"`
}

func (e *SyntaxError) Error() string {
	return fmt.Sprintf("line %d:%d: %s", e.Line, e.Column, e.Message)
}

type sourceLine struct {
	text           string
	indent, number int
}
type parser struct {
	lines       []sourceLine
	pos, blocks int
	locations   []sourceLine
}

func syntax(l sourceLine, message string) error {
	return &SyntaxError{Line: l.number, Column: l.indent + 1, Message: message}
}

var sharePattern = regexp.MustCompile(`\s+\((\d+)%\)$`)
var selectPattern = regexp.MustCompile(`(?i)^SELECT\s+(top|bottom)\s+(\d+)\s+BY\s+([a-z_]+)\s*\((\d+)\s*days?\)$`)
var inversePattern = regexp.MustCompile(`(?i)^WEIGHT\s+by\s+inverse\s+volatility\s*\((\d+)\s*days?\)$`)
var thresholdPattern = regexp.MustCompile(`(?i)^threshold\s*\((\d+)%\)$`)
var dayPattern = regexp.MustCompile(`(?i)^(\d+)\s*days?$`)
var numberPattern = regexp.MustCompile(`^[+-]?(?:\d+(?:\.\d*)?|\.\d+)$`)

// Parse reads complete documents or rule fragments. Omitted headers default to
// "Studio strategy", monthly rebalancing and SPY. Headers may appear in any
// order before the first rule. Semantic validation is a separate operation.
func Parse(source string) (*Strategy, error) { return parseSource(source, false) }

// ParseDraft accepts blank and header-only editor drafts with the same grammar.
func ParseDraft(source string) (*Strategy, error) { return parseSource(source, true) }

func parseSource(source string, allowEmpty bool) (*Strategy, error) {
	strategy, _, err := parseSourceMetadata(source, allowEmpty)
	return strategy, err
}

func parseSourceMetadata(source string, allowEmpty bool) (*Strategy, *SourceMetadata, error) {
	if len(source) > MaxSourceBytes || !utf8.ValidString(source) {
		return nil, nil, &SyntaxError{1, 1, "source must be valid UTF-8 and at most 4 MiB"}
	}
	p := &parser{}
	for i, raw := range strings.Split(source, "\n") {
		raw = strings.TrimRight(raw, "\r \t")
		if strings.TrimSpace(raw) == "" {
			continue
		}
		leading := len(raw) - len(strings.TrimLeft(raw, " \t"))
		prefix := strings.ReplaceAll(raw[:leading], "\t", strings.Repeat(" ", IndentWidth))
		p.lines = append(p.lines, sourceLine{strings.TrimSpace(raw), len(prefix), i + 1})
	}
	c := Strategy{Name: "Studio strategy", Rebalance: "monthly", Benchmark: "SPY"}
	seen := map[string]bool{}
	metadata := &SourceMetadata{Headers: map[string]SourceLocation{}}
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		key, value := command(l.text)
		if key != "STRATEGY" && key != "REBALANCE" && key != "BENCHMARK" {
			break
		}
		if l.indent != 0 || seen[key] {
			return nil, nil, syntax(l, "headers belong at the top level and appear at most once")
		}
		seen[key] = true
		metadata.Headers[key] = SourceLocation{Line: l.number, Column: l.indent + 1}
		p.pos++
		switch key {
		case "STRATEGY":
			if err := json.Unmarshal([]byte(value), &c.Name); err != nil || !strings.HasPrefix(value, "\"") || strings.TrimSpace(c.Name) == "" {
				return nil, nil, syntax(l, "expected STRATEGY followed by a nonempty JSON-quoted name")
			}
		case "REBALANCE":
			if m := thresholdPattern.FindStringSubmatch(value); m != nil {
				c.Rebalance = "threshold"
				c.Corridor = integer(m[1])
			} else {
				if len(strings.Fields(value)) != 1 {
					return nil, nil, syntax(l, "expected a calendar rebalance or threshold (N%)")
				}
				c.Rebalance = strings.ToLower(value)
			}
		case "BENCHMARK":
			if !tickerPattern.MatchString(value) {
				return nil, nil, syntax(l, "expected BENCHMARK TICKER")
			}
			c.Benchmark = value
		}
	}
	var err error
	c.Children, err = p.list(0, 0, false)
	if err != nil {
		return nil, nil, err
	}
	if p.pos != len(p.lines) {
		return nil, nil, syntax(p.lines[p.pos], "unexpected ELSE or END")
	}
	if len(c.Children) == 0 && !allowEmpty {
		return nil, nil, &SyntaxError{1, 1, "strategy needs at least one rule"}
	}
	if allowEmpty && c.Children == nil {
		c.Children = []Node{}
	}
	metadata.Locations = sourceNodeLocations(c.Children, p.locations)
	return &c, metadata, nil
}

func command(text string) (string, string) {
	at := strings.IndexAny(text, " \t")
	if at < 0 {
		return strings.ToUpper(text), ""
	}
	return strings.ToUpper(text[:at]), strings.TrimSpace(text[at:])
}
func integer(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return -1
	}
	return n
}

func (p *parser) list(indent, depth int, shares bool) ([]Node, error) {
	if depth >= customMaxDepth && p.pos < len(p.lines) && p.lines[p.pos].indent >= indent {
		return nil, syntax(p.lines[p.pos], "block nesting limit exceeded")
	}
	var out []Node
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if l.indent < indent {
			break
		}
		if l.indent > indent {
			return nil, syntax(l, fmt.Sprintf("expected %d spaces of indentation", indent))
		}
		key, value := command(l.text)
		if key == "ELSE" || key == "END" {
			if value != "" {
				return nil, syntax(l, key+" takes no arguments")
			}
			break
		}
		p.pos++
		p.blocks++
		p.locations = append(p.locations, l)
		if p.blocks > customMaxBlocks || len(out) >= customMaxList {
			return nil, syntax(l, "source block limit exceeded")
		}
		n := Node{}
		text := l.text
		if m := sharePattern.FindStringSubmatch(text); m != nil {
			if !shares {
				return nil, syntax(l, "percentage shares apply only directly inside WEIGHT by percentage")
			}
			n.Percent = integer(m[1])
			text = sharePattern.ReplaceAllString(text, "")
			key, value = command(text)
		}
		switch key {
		case "BUY":
			if !tickerPattern.MatchString(value) {
				return nil, syntax(l, "expected BUY TICKER")
			}
			n.Kind = "asset"
			n.Ticker = value
		case "GROUP":
			n.Kind = "group"
			if err := json.Unmarshal([]byte(value), &n.Name); err != nil || !strings.HasPrefix(value, "\"") {
				return nil, syntax(l, "expected GROUP followed by a JSON-quoted name")
			}
		case "WEIGHT":
			n.Kind = "weight"
			switch strings.ToLower(strings.Join(strings.Fields(value), " ")) {
			case "equally":
				n.Method = "equal"
			case "by percentage":
				n.Method = "specified"
			default:
				m := inversePattern.FindStringSubmatch(text)
				if m == nil {
					return nil, syntax(l, "expected equal, percentage or inverse-volatility weighting")
				}
				n.Method = "inverse-volatility"
				n.Lookback = integer(m[1])
			}
		case "SELECT":
			m := selectPattern.FindStringSubmatch(text)
			if m == nil {
				return nil, syntax(l, "expected SELECT top|bottom N BY metric (L days)")
			}
			metric, ok := metricsByCode[strings.ToLower(m[3])]
			if !ok {
				return nil, syntax(l, "unknown SELECT metric")
			}
			n.Kind = "filter"
			n.Direction = strings.ToLower(m[1])
			n.Count = integer(m[2])
			n.Metric = metric
			n.Lookback = integer(m[4])
		case "IF":
			parts := 0
			predicate, err := parsePredicate(value, &parts)
			if err != nil {
				return nil, syntax(l, err.Error())
			}
			if predicate.Type == "comparison" {
				percent := n.Percent
				n = predicate
				n.Type = ""
				n.Percent = percent
			} else {
				n.Predicate = &predicate
			}
			n.Kind = "condition"
			n.ThenChildren, err = p.list(indent+IndentWidth, depth+1, false)
			if err != nil {
				return nil, err
			}
			if p.pos >= len(p.lines) || p.lines[p.pos].indent != indent || strings.ToUpper(p.lines[p.pos].text) != "ELSE" {
				return nil, syntax(l, "IF requires an aligned ELSE")
			}
			p.pos++
			n.ElseChildren, err = p.list(indent+IndentWidth, depth+1, false)
			if err != nil {
				return nil, err
			}
			if p.pos >= len(p.lines) || p.lines[p.pos].indent != indent || strings.ToUpper(p.lines[p.pos].text) != "END" {
				return nil, syntax(l, "IF requires an aligned END")
			}
			p.pos++
		default:
			return nil, syntax(l, "unrecognized command "+strconv.Quote(key))
		}
		if n.Kind == "group" || n.Kind == "weight" || n.Kind == "filter" {
			var err error
			n.Children, err = p.list(indent+IndentWidth, depth+1, n.Kind == "weight" && n.Method == "specified")
			if err != nil {
				return nil, err
			}
		}
		out = append(out, n)
	}
	return out, nil
}

func splitArguments(s string) ([]string, error) {
	depth, start := 0, 0
	var out []string
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
		if depth < 0 {
			return nil, fmt.Errorf("unbalanced parentheses")
		}
	}
	if depth != 0 {
		return nil, fmt.Errorf("unbalanced parentheses")
	}
	out = append(out, strings.TrimSpace(s[start:]))
	for _, arg := range out {
		if arg == "" {
			return nil, fmt.Errorf("empty expression argument")
		}
	}
	return out, nil
}

func parseOperand(s string) (Node, error) {
	at := strings.IndexByte(s, '(')
	if at < 1 || !strings.HasSuffix(s, ")") {
		return Node{}, fmt.Errorf("expected an indicator")
	}
	metric, ok := metricsByCode[strings.ToLower(strings.TrimSpace(s[:at]))]
	if !ok {
		return Node{}, fmt.Errorf("unknown indicator")
	}
	args, err := splitArguments(s[at+1 : len(s)-1])
	if err != nil {
		return Node{}, err
	}
	if !tickerPattern.MatchString(args[0]) {
		return Node{}, fmt.Errorf("invalid indicator ticker")
	}
	n := Node{Metric: metric, Ticker: args[0]}
	spec := measures[metric]
	want := 1
	if spec.windowed {
		want = 2
	}
	if spec.oscillator {
		want = 3
	}
	if spec.smooth {
		want = 4
	}
	if len(args) != want {
		return n, fmt.Errorf("wrong number of indicator arguments")
	}
	if spec.windowed {
		m := dayPattern.FindStringSubmatch(args[1])
		if m == nil {
			return n, fmt.Errorf("expected a whole trading-day window")
		}
		n.Lookback = integer(m[1])
	}
	if spec.oscillator {
		for _, a := range args[1:] {
			if !regexpDigits(a) {
				return n, fmt.Errorf("oscillator windows must be whole days")
			}
		}
		n.ShortWindow = integer(args[1])
		n.LongWindow = integer(args[2])
		if spec.smooth {
			n.SmoothWindow = integer(args[3])
		}
	}
	return n, nil
}
func regexpDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func parsePredicate(s string, parts *int) (Node, error) {
	*parts++
	if *parts > customMaxConditionParts {
		return Node{}, fmt.Errorf("combined condition part limit exceeded")
	}
	at := strings.IndexByte(s, '(')
	if at > 0 {
		key := strings.ToUpper(strings.TrimSpace(s[:at]))
		if key == "ANY" || key == "ALL" {
			if !strings.HasSuffix(s, ")") {
				return Node{}, fmt.Errorf("unbalanced combined condition")
			}
			args, err := splitArguments(s[at+1 : len(s)-1])
			if err != nil {
				return Node{}, err
			}
			n := Node{Type: strings.ToLower(key)}
			for _, a := range args {
				term, err := parsePredicate(a, parts)
				if err != nil {
					return Node{}, err
				}
				n.Conditions = append(n.Conditions, term)
			}
			return n, nil
		}
	}
	depth := 0
	for i, r := range s {
		if r == '(' {
			depth++
		}
		if r == ')' {
			depth--
		}
		if depth < 0 {
			return Node{}, fmt.Errorf("unbalanced comparison")
		}
		if depth == 0 && (r == '>' || r == '<' || r == '=') {
			end := i + 1
			if end < len(s) && s[end] == '=' {
				end++
			}
			op := s[i:end]
			comparator := ""
			for name, code := range comparatorCodes {
				if code == op {
					comparator = name
				}
			}
			if comparator == "" {
				return Node{}, fmt.Errorf("invalid comparator")
			}
			left, err := parseOperand(strings.TrimSpace(s[:i]))
			if err != nil {
				return Node{}, err
			}
			left.Type = "comparison"
			left.Comparator = comparator
			rhs := strings.TrimSpace(s[end:])
			if numberPattern.MatchString(rhs) {
				left.CompareMode = "value"
				left.Value, err = strconv.ParseFloat(rhs, 64)
				if err != nil {
					return Node{}, fmt.Errorf("invalid comparison number")
				}
				return left, nil
			}
			right, err := parseOperand(rhs)
			if err != nil {
				return Node{}, err
			}
			left.CompareMode = "indicator"
			left.RightMetric = right.Metric
			left.RightTicker = right.Ticker
			left.RightLookback = right.Lookback
			left.RightShort = right.ShortWindow
			left.RightLong = right.LongWindow
			left.RightSmooth = right.SmoothWindow
			return left, nil
		}
	}
	return Node{}, fmt.Errorf("expected a comparison or ANY/ALL expression")
}
