// Package dsl is the stable public API for the Brighter strategy language.
// Its implementation lives under internal/core so command, build and fixture
// files remain separate from the library surface.
package dsl

import core "github.com/brighter-di/strategy-dsl/internal/core"

//go:generate go run ./internal/generate

const (
	Version         = core.Version
	LanguageVersion = core.LanguageVersion
	IndentWidth     = core.IndentWidth
	MaxSourceBytes  = core.MaxSourceBytes
)

type (
	Options           = core.Options
	Compiler          = core.Compiler
	Node              = core.Node
	Strategy          = core.Strategy
	Compiled          = core.Compiled
	Diagnostic        = core.Diagnostic
	Request           = core.Request
	Response          = core.Response
	SyntaxError       = core.SyntaxError
	ValidationError   = core.ValidationError
	SourceLocation    = core.SourceLocation
	SourceMetadata    = core.SourceMetadata
	SelectionMetadata = core.SelectionMetadata
	EditorLayout      = core.EditorLayout
)

func New(options Options) *Compiler                  { return core.New(options) }
func Parse(source string) (*Strategy, error)         { return core.Parse(source) }
func ParseDraft(source string) (*Strategy, error)    { return core.ParseDraft(source) }
func Validate(strategy Strategy) error               { return core.Validate(strategy) }
func Compile(strategy Strategy) (*Compiled, error)   { return core.Compile(strategy) }
func CompileSource(source string) (*Compiled, error) { return core.CompileSource(source) }
func Format(strategy Strategy) (string, error)       { return core.Format(strategy) }
func FormatSource(source string) (string, error)     { return core.FormatSource(source) }
func Specification() []byte                          { return core.Specification() }
func AssistantSchema() []byte                        { return core.AssistantSchema() }
func Selection(node Node) SelectionMetadata          { return core.Selection(node) }
func Layout(source string) (*EditorLayout, error)    { return core.Layout(source) }
func Execute(request Request) Response               { return core.Execute(request) }
func ExecuteJSON(request []byte) []byte              { return core.ExecuteJSON(request) }

// Operations lists every request ExecuteJSON answers.
var Operations = core.Operations

// InternalErrorJSON and OversizedRequestJSON let a host (the Wasm bridge) answer
// a panic or an oversized request with the same response shape.
func InternalErrorJSON(message string) []byte { return core.InternalErrorJSON(message) }
func OversizedRequestJSON() []byte            { return core.OversizedRequestJSON() }
