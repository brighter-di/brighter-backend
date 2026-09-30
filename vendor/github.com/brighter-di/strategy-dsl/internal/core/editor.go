package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

// SourceMetadata comes from the parser, never from a second consumer-side scan.
type SourceLocation struct {
	Path   []any `json:"path,omitempty"`
	Line   int   `json:"line"`
	Column int   `json:"column"`
}
type SourceMetadata struct {
	Headers   map[string]SourceLocation `json:"headers"`
	Locations []SourceLocation          `json:"locations"`
}

func sourceNodeLocations(nodes []Node, lines []sourceLine) []SourceLocation {
	out := []SourceLocation{}
	index := 0
	var list func([]Node, []any)
	var predicate func(Node, []any, sourceLine)
	predicate = func(n Node, path []any, line sourceLine) {
		out = append(out, SourceLocation{path, line.number, line.indent + 1})
		for i, child := range n.Conditions {
			predicate(child, appendPath(appendPath(path, "conditions"), i), line)
		}
	}
	list = func(nodes []Node, path []any) {
		for i, n := range nodes {
			p := appendPath(path, i)
			if index >= len(lines) {
				return
			}
			line := lines[index]
			index++
			out = append(out, SourceLocation{p, line.number, line.indent + 1})
			if n.Predicate != nil {
				predicate(*n.Predicate, appendPath(p, "predicate"), line)
			}
			list(n.Children, appendPath(p, "children"))
			list(n.ThenChildren, appendPath(p, "thenChildren"))
			list(n.ElseChildren, appendPath(p, "elseChildren"))
		}
	}
	list(nodes, []any{"children"})
	return out
}

// SelectionMetadata is shared with validation and exposed for parameter controls.
type SelectionMetadata struct {
	CandidatePath []any `json:"candidatePath"`
	Count         int   `json:"count"`
}

func selectionCandidates(n Node) ([]Node, []any) {
	if len(n.Children) == 1 && n.Children[0].Kind == "weight" {
		return n.Children[0].Children, []any{"children", 0, "children"}
	}
	return n.Children, []any{"children"}
}
func Selection(n Node) SelectionMetadata {
	pool, path := selectionCandidates(n)
	return SelectionMetadata{path, len(pool)}
}

// EditorLayout supports incomplete text, using the language's command and
// indentation rules. It never changes strategy values or invents missing blocks.
type EditorLayout struct {
	Tidied       string `json:"tidied"`
	Repaired     string `json:"repaired"`
	OpenIfIndent *int   `json:"openIfIndent"`
}

func Layout(source string) (*EditorLayout, error) {
	if len(source) > MaxSourceBytes {
		return nil, fmt.Errorf("source exceeds 4 MiB")
	}
	keywords := map[string]bool{}
	var vocabulary struct{ Headers, BlockKeywords []string }
	if err := json.Unmarshal(languageSpecification, &vocabulary); err != nil {
		return nil, err
	}
	for _, key := range append(vocabulary.Headers, vocabulary.BlockKeywords...) {
		keywords[key] = true
	}
	lines := strings.Split(source, "\n")
	for i, raw := range lines {
		// Match editor whitespace cleanup, including tab expansion in the prefix.
		leading := len(raw) - len(strings.TrimLeftFunc(raw, unicode.IsSpace))
		prefix := strings.ReplaceAll(raw[:leading], "\t", strings.Repeat(" ", IndentWidth))
		line := strings.TrimRightFunc(prefix+raw[leading:], unicode.IsSpace)
		at := len(line) - len(strings.TrimLeftFunc(line, unicode.IsSpace))
		end := at
		for end < len(line) && (line[end] >= 'a' && line[end] <= 'z' || line[end] >= 'A' && line[end] <= 'Z' || line[end] == '_') {
			end++
		}
		key := strings.ToUpper(line[at:end])
		if keywords[key] {
			line = line[:at] + key + line[end:]
		}
		lines[i] = line
	}
	out := &EditorLayout{Tidied: strings.Join(lines, "\n")}
	type block struct {
		keyword string
		indent  int
	}
	stack := []block{}
	for i, raw := range lines {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		trimmed := strings.TrimLeftFunc(raw, unicode.IsSpace)
		keyword := editorKeyword(trimmed)
		if keyword == "ELSE" || keyword == "END" {
			index := len(stack) - 1
			for index >= 0 && stack[index].keyword != "IF" {
				index--
			}
			if index < 0 {
				continue
			}
			stack = stack[:index+1]
			lines[i] = strings.Repeat(" ", index*IndentWidth) + trimmed
			if keyword == "END" {
				stack = stack[:index]
			}
			continue
		}
		indent := len(raw) - len(trimmed)
		for len(stack) > 0 && stack[len(stack)-1].keyword != "IF" && indent <= stack[len(stack)-1].indent {
			stack = stack[:len(stack)-1]
		}
		lines[i] = strings.Repeat(" ", len(stack)*IndentWidth) + trimmed
		if keyword == "IF" || keyword == "GROUP" || keyword == "WEIGHT" || keyword == "SELECT" {
			stack = append(stack, block{keyword, indent})
		}
	}
	out.Repaired = out.Tidied
	for _, raw := range strings.Split(out.Tidied, "\n") {
		if editorKeyword(strings.TrimSpace(raw)) == "IF" {
			out.Repaired = strings.Join(lines, "\n")
			break
		}
	}
	original := strings.Split(source, "\n")
	depth := 0
	for i := len(original) - 1; i >= 0; i-- {
		raw := original[i]
		trimmed := strings.TrimLeftFunc(raw, unicode.IsSpace)
		keyword := editorKeyword(trimmed)
		if keyword == "END" {
			depth++
		} else if keyword == "IF" {
			if depth == 0 {
				indent := len(strings.ReplaceAll(raw[:len(raw)-len(trimmed)], "\t", strings.Repeat(" ", IndentWidth)))
				out.OpenIfIndent = &indent
				break
			}
			depth--
		}
	}
	return out, nil
}

// Incomplete editor words use the same lexical prefix as keyword highlighting.
func editorKeyword(text string) string {
	end := 0
	for end < len(text) && (text[end] >= 'a' && text[end] <= 'z' || text[end] >= 'A' && text[end] <= 'Z' || text[end] == '_') {
		end++
	}
	return strings.ToUpper(text[:end])
}

// Metadata deliberately ignores numeric draft fields. A partially edited count
// or window must not prevent the inspector from finding its candidate nodes.
func executeSelectionJSON(raw []byte) []byte {
	var request struct {
		Node *struct {
			Children []struct {
				Kind     string            `json:"kind"`
				Children []json.RawMessage `json:"children"`
			} `json:"children"`
		} `json:"node"`
	}
	out := Response{Version: Version, LanguageVersion: LanguageVersion}
	if err := json.Unmarshal(raw, &request); err != nil {
		out.Diagnostics = []Diagnostic{{Code: "BAD_REQUEST", Message: err.Error(), Level: "error"}}
	} else if request.Node == nil {
		out.Diagnostics = []Diagnostic{{Code: "BAD_REQUEST", Message: "provide a selection node", Level: "error"}}
	} else {
		n := Node{Kind: "filter", Children: make([]Node, len(request.Node.Children))}
		if len(n.Children) == 1 {
			child := request.Node.Children[0]
			n.Children[0] = Node{Kind: child.Kind, Children: make([]Node, len(child.Children))}
		}
		metadata := Selection(n)
		out.OK = true
		out.Selection = &metadata
	}
	encoded, _ := json.Marshal(out)
	return encoded
}
