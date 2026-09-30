package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// rpc sends one JSON-RPC call to the MCP endpoint and returns the envelope.
func rpc(t *testing.T, h http.Handler, method string, params any) map[string]any {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(raw)))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", method, rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: not JSON: %s", method, rec.Body)
	}
	return out
}

// call runs a tool and returns the text of its single content block, plus
// whether the tool itself refused.
func call(t *testing.T, h http.Handler, name string, arguments map[string]any) (map[string]any, bool) {
	t.Helper()
	env := rpc(t, h, "tools/call", map[string]any{"name": name, "arguments": arguments})
	if e, ok := env["error"]; ok {
		t.Fatalf("%s: transport error %v", name, e)
	}
	result, _ := env["result"].(map[string]any)
	failed, _ := result["isError"].(bool)
	content, _ := result["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("%s: %d content blocks", name, len(content))
	}
	block, _ := content[0].(map[string]any)
	text, _ := block["text"].(string)
	if failed {
		return map[string]any{"message": text}, true
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("%s: tool text is not JSON: %s", name, text)
	}
	return payload, false
}

func mcpServer(t *testing.T) http.Handler {
	t.Helper()
	s, _ := newTestServer(t)
	// A model's budget is its own; widen it so a test is not rationed.
	s.mcpIP = newIPLimiter(1000, 1000)
	s.mcpGlobal = newGlobalLimiter(1e6, 1000)
	return s.handler()
}

func TestMCPHandshakeAndCatalogue(t *testing.T) {
	h := mcpServer(t)

	result, _ := rpc(t, h, "initialize", map[string]any{"protocolVersion": "2025-06-18"})["result"].(map[string]any)
	if result["protocolVersion"] != "2025-06-18" {
		t.Fatalf("protocol %v", result["protocolVersion"])
	}
	if info, _ := result["serverInfo"].(map[string]any); info["name"] != "brighter" {
		t.Fatalf("serverInfo %v", result["serverInfo"])
	}
	if caps, _ := result["capabilities"].(map[string]any); caps["tools"] == nil || caps["resources"] == nil {
		t.Fatalf("capabilities %v", result["capabilities"])
	}

	// A client that asks for a revision we do not know is still answered.
	if r, _ := rpc(t, h, "initialize", map[string]any{"protocolVersion": "1999-01-01"})["result"].(map[string]any); r["protocolVersion"] == "1999-01-01" {
		t.Fatal("an unknown protocol revision was echoed back")
	}

	listed, _ := rpc(t, h, "tools/list", nil)["result"].(map[string]any)
	tools, _ := listed["tools"].([]any)
	names := map[string]bool{}
	for _, item := range tools {
		tool, _ := item.(map[string]any)
		name, _ := tool["name"].(string)
		if name == "" || tool["description"] == "" {
			t.Fatalf("a tool without a name or description: %v", tool)
		}
		schema, _ := tool["inputSchema"].(map[string]any)
		if schema["type"] != "object" {
			t.Fatalf("%s: inputSchema is not an object schema: %v", name, schema)
		}
		// A tool must never leak its handler onto the wire.
		if _, leaked := tool["run"]; leaked {
			t.Fatalf("%s: the handler reached the wire", name)
		}
		names[name] = true
	}
	for _, want := range []string{
		"list_strategies", "get_strategy", "backtest_strategy", "get_allocation", "find_assets", "asset_history",
		"dsl_reference", "strategy_schema", "check_strategy", "format_strategy", "backtest_code",
	} {
		if !names[want] {
			t.Fatalf("tools/list is missing %s", want)
		}
	}
	// Nothing that touches a portfolio, a wallet or a key may be listed.
	for name := range names {
		for _, forbidden := range []string{"portfolio", "invest", "withdraw", "deploy", "wallet", "publish", "deposit"} {
			if strings.Contains(name, forbidden) {
				t.Fatalf("%s exposes %s", name, forbidden)
			}
		}
	}

	list, failed := call(t, h, "list_strategies", map[string]any{"period": "1y", "limit": 3})
	if failed {
		t.Fatalf("list_strategies: %v", list)
	}
	rows, _ := list["strategies"].([]any)
	if len(rows) == 0 || list["total"] == nil {
		t.Fatalf("list_strategies returned %v", list)
	}
	first, _ := rows[0].(map[string]any)
	id, _ := first["id"].(string)
	if id == "" || first["name"] == nil || first["canHold"] == nil {
		t.Fatalf("a thin row: %v", first)
	}

	one, failed := call(t, h, "get_strategy", map[string]any{"id": id})
	if failed || one["parameters"] == nil || one["defaults"] == nil {
		t.Fatalf("get_strategy: %v", one)
	}

	bt, failed := call(t, h, "backtest_strategy", map[string]any{"id": id, "range": "1y"})
	if failed {
		t.Fatalf("backtest_strategy: %v", bt)
	}
	assertMetricsOnly(t, bt["metrics"])
}

func TestMCPWritesAndChecksAStrategy(t *testing.T) {
	h := mcpServer(t)

	reference, failed := call(t, h, "dsl_reference", map[string]any{"limits": true})
	if failed {
		t.Fatalf("dsl_reference: %v", reference)
	}
	text, _ := reference["reference"].(string)
	if len(text) < 200 || strings.Contains(text, "{{") {
		t.Fatalf("the reference did not render: %.120s", text)
	}
	if reference["limits"] == nil {
		t.Fatal("limits were asked for and not given")
	}

	const good = "WEIGHT equally\n  BUY SPY\n  BUY QQQ"

	checked, failed := call(t, h, "check_strategy", map[string]any{"code": good})
	if failed || checked["valid"] != true {
		t.Fatalf("a valid strategy was refused: %v", checked)
	}

	// A syntax error must come back with somewhere to look.
	broken, _ := call(t, h, "check_strategy", map[string]any{"code": "WEIGHT sideways\n  BUY SPY"})
	if broken["valid"] != false {
		t.Fatalf("a broken strategy passed: %v", broken)
	}
	problems, _ := broken["problems"].([]any)
	if len(problems) == 0 {
		t.Fatalf("no problem was reported: %v", broken)
	}

	// A ticker Brighter cannot trade is a fault, not a backtest.
	untradable, _ := call(t, h, "check_strategy", map[string]any{"code": "WEIGHT equally\n  BUY NOTATICKER"})
	if untradable["valid"] != false {
		t.Fatalf("an untradable ticker passed: %v", untradable)
	}

	formatted, failed := call(t, h, "format_strategy", map[string]any{"code": good})
	if failed || formatted["code"] == "" {
		t.Fatalf("format_strategy: %v", formatted)
	}

	ran, failed := call(t, h, "backtest_code", map[string]any{"code": good, "range": "1y"})
	if failed {
		t.Fatalf("backtest_code: %v", ran)
	}
	assertMetricsOnly(t, ran["metrics"])
}

// assertMetricsOnly is the promise the tools make to a model: figures, never a
// curve. A hundred dated points would cost it most of its context.
func assertMetricsOnly(t *testing.T, value any) {
	t.Helper()
	m, _ := value.(map[string]any)
	if m == nil || m["totalReturn"] == nil || m["maxDrawdown"] == nil {
		t.Fatalf("no metrics: %v", value)
	}
	for _, forbidden := range []string{"series", "dates", "points", "strategy", "benchmarkSeries"} {
		if _, present := m[forbidden]; present {
			t.Fatalf("a backtest carried %q back to the model: %v", forbidden, m)
		}
	}
}

func TestMCPRejectsWhatItCannotAnswer(t *testing.T) {
	h := mcpServer(t)

	send := func(body string) map[string]any {
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	code := func(env map[string]any) float64 {
		e, _ := env["error"].(map[string]any)
		c, _ := e["code"].(float64)
		return c
	}

	if got := code(send("not json at all")); got != -32700 {
		t.Fatalf("malformed JSON: %v", got)
	}
	if got := code(send(`{"id":1,"method":"tools/list"}`)); got != -32600 {
		t.Fatalf("a missing jsonrpc version: %v", got)
	}
	if got := code(rpc(t, h, "nope/nothing", nil)); got != -32601 {
		t.Fatalf("unknown method: %v", got)
	}
	if got := code(rpc(t, h, "tools/call", map[string]any{"name": "no_such_tool"})); got != -32602 {
		t.Fatalf("unknown tool: %v", got)
	}

	// An argument the schema does not allow is the model's mistake to fix, so
	// it comes back as a tool result rather than a transport error.
	out, failed := call(t, h, "get_strategy", map[string]any{"id": "mag7-rank", "nonsense": 1})
	if !failed || !strings.Contains(fmt.Sprint(out["message"]), "schema") {
		t.Fatalf("an unknown argument was accepted: %v", out)
	}

	// A GET says what this is rather than failing blankly.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if rec.Code != http.StatusMethodNotAllowed || !strings.Contains(rec.Body.String(), "JSON-RPC") {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body)
	}
}

func TestMCPHasItsOwnBudget(t *testing.T) {
	s, _ := newTestServer(t)
	// One call, then nothing: the model's own ceiling, not the website's.
	s.mcpIP = newIPLimiter(0, 1)
	s.research = newIPLimiter(1000, 1000)
	h := s.handler()

	if _, failed := call(t, h, "dsl_reference", nil); failed {
		t.Fatal("the first call was refused")
	}
	// Past the budget the model is told to wait, in a result it can read,
	// rather than handed a transport error it cannot interpret.
	out, failed := call(t, h, "dsl_reference", nil)
	if !failed || !strings.Contains(fmt.Sprint(out["message"]), "Rate limited") {
		t.Fatalf("a call past the budget was served: %v", out)
	}

	// The website's own budget must be untouched by any of that.
	rec := send(h, http.MethodGet, "/v1/templates", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("the website was charged for the model: %d", rec.Code)
	}
}

// From 2026-07-28 a client declares its revision per request and may call
// server/discover instead of shaking hands. Both eras are served here, because
// most clients in the field still open with initialize.
func TestMCPDiscoverAnswersTheCurrentProtocol(t *testing.T) {
	h := mcpServer(t)

	result, _ := rpc(t, h, "server/discover", map[string]any{
		"_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		},
	})["result"].(map[string]any)

	if result["resultType"] != "complete" {
		t.Fatalf("resultType %v", result["resultType"])
	}
	versions, _ := result["supportedVersions"].([]any)
	if len(versions) == 0 || versions[0] != "2026-07-28" {
		t.Fatalf("supportedVersions %v", versions)
	}
	// The handshake era must still be offered, or a shipped client that opens
	// with initialize has no version in common with us.
	legacy := false
	for _, v := range versions {
		if v == "2025-06-18" {
			legacy = true
		}
	}
	if !legacy {
		t.Fatalf("no handshake-era revision offered: %v", versions)
	}
	if caps, _ := result["capabilities"].(map[string]any); caps["tools"] == nil || caps["resources"] == nil {
		t.Fatalf("capabilities %v", result["capabilities"])
	}
	meta, _ := result["_meta"].(map[string]any)
	info, _ := meta["io.modelcontextprotocol/serverInfo"].(map[string]any)
	if info["name"] != "brighter" || info["version"] == nil {
		t.Fatalf("serverInfo %v", meta)
	}
	if text, _ := result["instructions"].(string); len(text) < 50 {
		t.Fatalf("instructions %q", text)
	}
}

func TestMCPServesItsLanguageAsResources(t *testing.T) {
	h := mcpServer(t)

	listed, _ := rpc(t, h, "resources/list", nil)["result"].(map[string]any)
	resources, _ := listed["resources"].([]any)
	if len(resources) != 2 {
		t.Fatalf("%d resources", len(resources))
	}
	for _, item := range resources {
		r, _ := item.(map[string]any)
		uri, _ := r["uri"].(string)
		read, _ := rpc(t, h, "resources/read", map[string]any{"uri": uri})["result"].(map[string]any)
		contents, _ := read["contents"].([]any)
		if len(contents) != 1 {
			t.Fatalf("%s: %d contents", uri, len(contents))
		}
		body, _ := contents[0].(map[string]any)
		text, _ := body["text"].(string)
		var parsed any
		if err := json.Unmarshal([]byte(text), &parsed); err != nil {
			t.Fatalf("%s is not JSON: %v", uri, err)
		}
	}
	env := rpc(t, h, "resources/read", map[string]any{"uri": "brighter://nope"})
	e, _ := env["error"].(map[string]any)
	if e == nil || e["code"].(float64) != -32602 {
		t.Fatalf("an unknown resource was served: %v", env)
	}
}
