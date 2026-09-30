// Package mcp serves Brighter's public research and the Studio authoring loop
// over the Model Context Protocol, so a model in any MCP client can browse the
// catalogue, write rules in Brighter's language, check them and run a real
// backtest.
//
// Everything here is already public and read-only: the same catalogue, assets
// and inline backtests the website serves to anyone. Nothing touches a
// portfolio, a wallet or a key, and there is nothing to authenticate. That is
// deliberate — a portfolio is identified by an unguessable id rather than by
// proving ownership, so an id repeated into a chat transcript would become a
// capability. Portfolios stay out until they authenticate a reader.
//
// The transport is stateless Streamable HTTP: one POST, a JSON-RPC 2.0 request
// in, a response out. No sessions and no server-sent events — every tool here
// answers in one round trip, and streaming would only cost an nginx buffering
// exception.
package mcp

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// The protocol moved: from 2026-07-28 a client declares its revision on every
// request and may call server/discover, which servers must implement, instead
// of shaking hands. Revisions up to 2025-11-25 still open with initialize, and
// most shipped clients still do, so both are served from one endpoint.
const protocolVersion = "2026-07-28"

// supportedVersions is what server/discover advertises, newest first.
var supportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26"}

func known(version string) bool {
	for _, v := range supportedVersions {
		if v == version {
			return true
		}
	}
	return false
}

// JSON-RPC error codes. The first four are the transport's; -32000 upwards are
// ours to define, and one is enough: the caller asked for more than we serve.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
	codeBusy           = -32000
)

// maxBody is generous for a rule but far short of a payload worth worrying
// about; the largest thing a caller sends is DSL text.
const maxBody = 256 << 10

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Admit is asked before every tool call, and told whether that call can reach
// an upstream. It is how the MCP endpoint carries its own budget: the website's
// limiters are never charged for a model's curiosity, and a model's loop can
// never starve the website of BacktestEngine. Reading the language costs nothing and
// must not be rationed like a backtest.
type Admit func(r *http.Request, upstream bool) error

// Server answers MCP over HTTP. It holds no state between requests.
type Server struct {
	tools     []Tool
	resources []Resource
	admit     Admit
	log       *slog.Logger
	name      string
}

// New builds the endpoint over a catalogue. admit may be nil, which serves
// every request.
func New(cat Catalogue, admit Admit, log *slog.Logger) *Server {
	s := &Server{admit: admit, log: log.With("component", "mcp"), name: "brighter"}
	s.tools = catalogueTools(cat)
	s.tools = append(s.tools, authoringTools(cat)...)
	s.resources = languageResources()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// A GET here is usually a browser or a health probe, not a client:
		// say what this is rather than 405 with an empty body.
		w.Header().Set("allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
			"name":     s.name,
			"protocol": protocolVersion,
			"transport": "Send JSON-RPC 2.0 over POST. " +
				"See https://modelcontextprotocol.io for clients.",
		})
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		s.fail(w, nil, codeInvalidRequest, "That request was too large to read.", nil)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.fail(w, nil, codeParse, "That is not JSON.", nil)
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		s.fail(w, req.ID, codeInvalidRequest, `Every call needs "jsonrpc":"2.0" and a method.`, nil)
		return
	}

	// A notification has no id and takes no answer. "initialized" is the only
	// one a client sends us, and it wants nothing back.
	notification := len(req.ID) == 0
	if strings.HasPrefix(req.Method, "notifications/") {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	result, rerr := s.dispatch(r, req)
	if notification {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if rerr != nil {
		writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: rerr})
		return
	}
	writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result})
}

func (s *Server) dispatch(r *http.Request, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "server/discover":
		return s.discover(), nil
	case "initialize":
		// The handshake of 2025-11-25 and earlier. Kept because that is what
		// most clients in the field still send.
		return s.initialize(req.Params), nil
	case "ping":
		// The spec's keepalive: an empty result is the whole answer.
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": s.tools}, nil
	case "tools/call":
		return s.callTool(r, req.Params)
	case "resources/list":
		return map[string]any{"resources": s.resources}, nil
	case "resources/read":
		return s.readResource(req.Params)
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "Unknown method " + req.Method + "."}
	}
}

// discover answers the modern negotiation: what we speak, what we offer, and
// who we are, in one request and before anything else is sent.
func (s *Server) discover() any {
	return map[string]any{
		"resultType":        "complete",
		"supportedVersions": supportedVersions,
		"capabilities": map[string]any{
			"tools":     map[string]any{},
			"resources": map[string]any{},
		},
		"instructions": instructions,
		"_meta": map[string]any{
			"io.modelcontextprotocol/serverInfo": map[string]any{"name": s.name, "version": Version},
		},
	}
}

func (s *Server) initialize(params json.RawMessage) any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	// A handshake-era client is answered in its own revision when we know it;
	// declaring the newest to a client that asked for an older one only
	// invites it to send shapes we would then have to reject.
	version := "2025-06-18"
	if known(p.ProtocolVersion) {
		version = p.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		// Neither list ever changes while the process runs, so no listChanged.
		"capabilities": map[string]any{
			"tools":     map[string]any{},
			"resources": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    s.name,
			"title":   "Brighter",
			"version": Version,
		},
		"instructions": instructions,
	}
}

// Version identifies this server to a client. It is the language the tools
// speak, which is what actually matters to a caller.
const Version = "0.1.0"

const instructions = "Brighter runs rule-based portfolios of tokenized US stocks. " +
	"Use list_strategies and get_strategy to read the published catalogue, and " +
	"backtest_strategy to test one with your own parameters. To write a new rule, " +
	"read dsl_reference first, then check_strategy on your code, then backtest_code " +
	"to run it against real market history. Every backtest is a real one — there is " +
	"no simulation here — so prefer few, deliberate runs."

func (s *Server) fail(w http.ResponseWriter, id json.RawMessage, code int, message string, data any) {
	writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message, Data: data}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("content-type", "application/json")
	w.Header().Set("cache-control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
