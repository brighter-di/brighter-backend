package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Request and Response define the versioned JSON boundary shared by CLI and Wasm.
type Request struct {
	Operation string    `json:"operation"`
	Pool      int       `json:"pool,omitempty"`
	Count     int       `json:"count,omitempty"`
	Node      *Node     `json:"node,omitempty"`
	Source    *string   `json:"source,omitempty"`
	Strategy  *Strategy `json:"strategy,omitempty"`
	Options   Options   `json:"options,omitempty"`
}
type Diagnostic struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Line        int    `json:"line,omitempty"`
	Column      int    `json:"column,omitempty"`
	Path        []any  `json:"path,omitempty"`
	Level       string `json:"level,omitempty"`
	SourceLevel string `json:"sourceLevel,omitempty"`
}
type Response struct {
	OK              bool               `json:"ok"`
	Capacity        *bool              `json:"capacity,omitempty"`
	Version         string             `json:"version"`
	LanguageVersion int                `json:"languageVersion"`
	Strategy        *Strategy          `json:"strategy,omitempty"`
	SourceMetadata  *SourceMetadata    `json:"sourceMetadata,omitempty"`
	Selection       *SelectionMetadata `json:"selection,omitempty"`
	Layout          *EditorLayout      `json:"layout,omitempty"`
	AssistantSchema json.RawMessage    `json:"assistantSchema,omitempty"`
	Code            string             `json:"code,omitempty"`
	Compiled        *Compiled          `json:"compiled,omitempty"`
	Specification   json.RawMessage    `json:"specification,omitempty"`
	Diagnostics     []Diagnostic       `json:"diagnostics,omitempty"`
}

// Execute evaluates one request without I/O or persistent state.
func Execute(request Request) Response {
	out := Response{Version: Version, LanguageVersion: LanguageVersion}
	fail := func(code string, err error) Response {
		d := Diagnostic{Code: code, Message: err.Error()}
		var syntax *SyntaxError
		if errors.As(err, &syntax) {
			d.Code = "SYNTAX_ERROR"
			d.Line = syntax.Line
			d.Column = syntax.Column
			d.Message = syntax.Message
		}
		out.Diagnostics = []Diagnostic{d}
		return out
	}
	if request.Operation == "selectionCapacity" {
		if request.Source != nil || request.Strategy != nil {
			return fail("BAD_REQUEST", fmt.Errorf("selectionCapacity accepts only pool and count"))
		}
		plan, err := planWeightedSelection(request.Pool, request.Count)
		capacity := err == nil && plan.size+2 <= customMaxExpandedBlocks
		out.OK = true
		out.Capacity = &capacity
		return out
	}

	if request.Operation == "assistantSchema" {
		out.OK = true
		out.AssistantSchema = AssistantSchema()
		return out
	}
	if request.Operation == "selectionMetadata" {
		if request.Node == nil {
			return fail("BAD_REQUEST", fmt.Errorf("provide a selection node"))
		}
		metadata := Selection(*request.Node)
		out.OK = true
		out.Selection = &metadata
		return out
	}
	if request.Operation == "editorLayout" {
		if request.Source == nil {
			return fail("BAD_REQUEST", fmt.Errorf("provide source text"))
		}
		layout, err := Layout(*request.Source)
		if err != nil {
			return fail("BAD_REQUEST", err)
		}
		out.OK = true
		out.Layout = layout
		return out
	}
	if request.Operation == "specification" {
		out.OK = true
		out.Specification = Specification()
		return out
	}
	switch request.Operation {
	case "parse", "parseDraft", "diagnose", "format", "validate", "compile":
	default:
		return fail("BAD_REQUEST", fmt.Errorf("operation must be one of: %s", strings.Join(Operations, ", ")))
	}
	if (request.Source == nil) == (request.Strategy == nil) {
		return fail("BAD_REQUEST", fmt.Errorf("provide exactly one of source or strategy"))
	}
	strategy := request.Strategy
	if request.Source != nil {
		var err error
		if request.Operation == "parseDraft" || request.Operation == "diagnose" {
			strategy, out.SourceMetadata, err = parseSourceMetadata(*request.Source, true)
		} else {
			strategy, err = Parse(*request.Source)
		}
		if err != nil {
			return fail("SYNTAX_ERROR", err)
		}
	}
	compiler := New(request.Options)
	switch request.Operation {
	case "parse", "parseDraft":
		out.Strategy = strategy
	case "diagnose":
		out.Diagnostics = compiler.Diagnostics(*strategy)
		out.OK = !hasDiagnosticErrors(out.Diagnostics)
		return out
	case "format":
		code, err := compiler.Format(*strategy)
		if err != nil {
			return fail("VALIDATION_ERROR", err)
		}
		out.Code = code
	case "validate":
		if err := compiler.Validate(*strategy); err != nil {
			return fail("VALIDATION_ERROR", err)
		}
	case "compile":
		compiled, err := compiler.Compile(*strategy)
		if err != nil {
			return fail("VALIDATION_ERROR", err)
		}
		out.Compiled = compiled
	}
	out.OK = true
	return out
}

// Operations lists every request ExecuteJSON answers.
var Operations = []string{
	"parse", "parseDraft", "diagnose", "format", "formatDraft", "formatMeasure", "formatPredicate", "validate", "compile",
	"specification", "importAssistant", "assistantSchema", "selectionMetadata", "selectionCapacity", "editorLayout",
}

// InternalErrorJSON is the response for a failure inside the core itself.
func InternalErrorJSON(message string) []byte {
	encoded, err := json.Marshal(Response{Version: Version, LanguageVersion: LanguageVersion, Diagnostics: []Diagnostic{{Code: "INTERNAL_ERROR", Message: message}}})
	if err != nil {
		return []byte(`{"ok":false,"diagnostics":[{"code":"INTERNAL_ERROR","message":"Internal error"}]}`)
	}
	return encoded
}

// OversizedRequestJSON rejects a request over the size limit without reading it.
func OversizedRequestJSON() []byte {
	encoded, _ := json.Marshal(Response{Version: Version, LanguageVersion: LanguageVersion, Diagnostics: []Diagnostic{{Code: "BAD_REQUEST", Message: "Invalid request JSON: JSON request exceeds 8 MiB"}}})
	return encoded
}

// recoverJSON turns a panic in fn into an INTERNAL_ERROR response, so one bad
// request can never take the caller (or a Wasm runtime) down.
func recoverJSON(fn func() []byte) (out []byte) {
	defer func() {
		if recovered := recover(); recovered != nil {
			out = InternalErrorJSON(fmt.Sprintf("The strategy engine failed on this request: %v", recovered))
		}
	}()
	return fn()
}

// ExecuteJSON is the stable Wasm/CLI ABI. It always returns valid JSON, even
// when the core panics.
func ExecuteJSON(raw []byte) []byte {
	return recoverJSON(func() []byte { return executeJSON(raw) })
}

func executeJSON(raw []byte) []byte {
	if len(raw) <= MaxSourceBytes*2 {
		// Decode only the operation before selecting a raw editor boundary. Strict
		// operations never decode the full strategy/options through those helpers.
		var header struct {
			Operation string `json:"operation"`
		}
		if json.Unmarshal(raw, &header) == nil {
			switch header.Operation {
			case "formatDraft", "formatMeasure", "formatPredicate":
				if out, handled := executeDraftJSON(raw); handled {
					return out
				}
			case "selectionMetadata":
				return executeSelectionJSON(raw)
			case "importAssistant":
				return executeAssistantJSON(raw)
			case "diagnose":
				if out, handled := executeDiagnosticsJSON(raw); handled {
					return out
				}
			}
		}
	}

	var request Request
	var err error
	if len(raw) > MaxSourceBytes*2 {
		err = fmt.Errorf("JSON request exceeds 8 MiB")
	} else {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		err = decoder.Decode(&request)
		if err == nil {
			var extra any
			if next := decoder.Decode(&extra); next != io.EOF {
				err = fmt.Errorf("expected exactly one JSON request")
			}
		}
	}
	var out Response
	if err != nil {
		out = Response{Version: Version, LanguageVersion: LanguageVersion, Diagnostics: []Diagnostic{{Code: "BAD_REQUEST", Message: "Invalid request JSON: " + err.Error()}}}
	} else {
		out = Execute(request)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		encoded = InternalErrorJSON("Could not encode response")
	}
	return encoded
}
