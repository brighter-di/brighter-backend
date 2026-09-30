// Package dsl parses, formats, validates and compiles Brighter strategy rules.
// It uses only the Go standard library and runs natively and in WebAssembly.
package core

import (
	contract "github.com/brighter-di/strategy-dsl/internal/spec"
	"regexp"
	"strings"
)

// Version (the package API, from package.json) and LanguageVersion (from the
// language specification) are generated into language_generated.go.

var languageSpecification = contract.Language()

// Specification returns an independent copy of the language specification.
func Specification() []byte { return append([]byte(nil), languageSpecification...) }

// Options keeps asset policy outside the language. Nil Tickers accepts any
// syntactically valid ticker. A non-nil map is an allowlist from aliases to
// canonical research symbols; an empty map accepts no assets.
type Options struct {
	Tickers     map[string]string `json:"tickers,omitempty"`
	RequireName bool              `json:"requireName,omitempty"`
}

// Compiler is immutable after construction and safe for concurrent use.
type Compiler struct {
	tickers     map[string]string
	requireName bool
}

var tickerPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._]*$`)

func New(options Options) *Compiler {
	c := &Compiler{requireName: options.RequireName}
	if options.Tickers != nil {
		c.tickers = make(map[string]string, len(options.Tickers))
		for alias, canonical := range options.Tickers {
			c.tickers[strings.ToUpper(strings.TrimSpace(alias))] = strings.ToUpper(strings.TrimSpace(canonical))
		}
	}
	return c
}

func (c *Compiler) resolveTicker(written string) (string, bool) {
	t := strings.ToUpper(strings.TrimSpace(written))
	if c.tickers != nil {
		var ok bool
		t, ok = c.tickers[t]
		if !ok {
			return "", false
		}
	}
	return t, tickerPattern.MatchString(t)
}

// Compile validates a strategy and emits a backtest engine-compatible score and rule identity.
func Compile(strategy Strategy) (*Compiled, error) { return New(Options{}).Compile(strategy) }

// Validate checks semantic readiness with the same rules used by Compile.
func Validate(strategy Strategy) error { return New(Options{}).Validate(strategy) }
func (c *Compiler) Validate(strategy Strategy) error {
	for _, d := range c.Diagnostics(strategy) {
		if d.Level != "warning" {
			return &ValidationError{Reason: d.Message}
		}
	}
	return nil
}

// CompileSource reads DSL and compiles it with this compiler's asset policy.
func (c *Compiler) CompileSource(source string) (*Compiled, error) {
	strategy, err := Parse(source)
	if err != nil {
		return nil, err
	}
	return c.Compile(*strategy)
}
func CompileSource(source string) (*Compiled, error) { return New(Options{}).CompileSource(source) }
